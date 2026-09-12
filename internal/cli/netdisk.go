package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
)

type remoteFile struct {
	Path        string `json:"path"`
	ID          int64  `json:"fs_id"`
	Size        int64  `json:"size"`
	IsDir       int    `json:"isdir"`
	ServerMtime int64  `json:"server_mtime"`
	Mtime       int64  `json:"mtime"`
	Dlink       string `json:"dlink"`
}

func (f remoteFile) modified() int64 {
	if f.ServerMtime != 0 {
		return f.ServerMtime
	}
	return f.Mtime
}

type netdiskError int

func (e netdiskError) Error() string { return fmt.Sprintf("Netdisk errno %d", int(e)) }

// decodeAPI deliberately excludes response bodies, which may contain credentials
// or temporary download URLs, from diagnostics.
func decodeAPI(body []byte, out any) error {
	var status struct {
		Errno *int `json:"errno"`
	}
	if err := json.Unmarshal(body, &status); err != nil || status.Errno == nil {
		return errors.New("invalid Netdisk response (missing errno)")
	}
	if *status.Errno != 0 {
		return netdiskError(*status.Errno)
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return errors.New("invalid Netdisk response")
		}
	}
	return nil
}

func (cfg *config) diskAPI(ctx context.Context, method, endpoint string, query, form url.Values, out any) error {
	token, err := cfg.resolveAccessToken()
	if err != nil {
		return withCode(253, err)
	}
	query = cloneValues(query)
	query.Set("access_token", token)
	body, err := cfg.request(ctx, method, endpoint, query, form, nil)
	if err != nil {
		return err
	}
	return decodeAPI(body, out)
}

func (cfg *config) listRemote(ctx context.Context, dir string, pageSize int) ([]remoteFile, error) {
	var all []remoteFile
	seen := map[string]bool{}
	for start := 0; ; {
		var result struct {
			List    []json.RawMessage `json:"list"`
			HasMore *int              `json:"has_more"`
		}
		err := cfg.diskAPI(ctx, http.MethodGet, panBase+"/rest/2.0/xpan/file",
			values("method", "list", "dir", dir, "order", "name", "desc", "0",
				"start", strconv.Itoa(start), "limit", strconv.Itoa(pageSize)), nil, &result)
		if err != nil {
			return nil, err
		}
		if result.List == nil {
			return nil, errors.New("incomplete directory response (missing list)")
		}
		for _, raw := range result.List {
			var fields map[string]json.RawMessage
			if json.Unmarshal(raw, &fields) != nil {
				return nil, errors.New("invalid directory entry")
			}
			// Missing isdir must never turn a directory into a deletable file;
			// missing size must not masquerade as a legitimate empty file.
			for _, key := range []string{"path", "fs_id", "size", "isdir"} {
				if len(fields[key]) == 0 || string(fields[key]) == "null" {
					return nil, errors.New("incomplete directory entry")
				}
			}
			var f remoteFile
			if json.Unmarshal(raw, &f) != nil {
				return nil, errors.New("invalid directory metadata")
			}
			if !validRemotePath(f.Path) || f.Path == dir || path.Dir(f.Path) != dir ||
				f.Size < 0 || f.ID <= 0 || (f.IsDir != 0 && f.IsDir != 1) || seen[f.Path] {
				return nil, errors.New("invalid or repeated directory entry")
			}
			seen[f.Path] = true
			all = append(all, f)
		}
		start += len(result.List)
		if result.HasMore != nil {
			if *result.HasMore == 0 {
				break
			}
			if *result.HasMore != 1 || len(result.List) == 0 {
				return nil, errors.New("incomplete directory pagination")
			}
		} else if len(result.List) < pageSize {
			break
		}
	}
	return all, nil
}

func (cfg *config) statRemote(ctx context.Context, name string, pageSize int) (remoteFile, error) {
	if name == "/" {
		return remoteFile{Path: "/", IsDir: 1}, nil
	}
	// ponytail: complete parent listings cost O(directory size) per stat; use a
	// verified direct-metadata API if large-directory latency warrants it.
	files, err := cfg.listRemote(ctx, path.Dir(name), pageSize)
	if errors.Is(err, netdiskError(-9)) {
		return remoteFile{}, os.ErrNotExist
	}
	if err != nil {
		return remoteFile{}, err
	}
	for _, f := range files {
		if f.Path == name {
			return f, nil
		}
	}
	return remoteFile{}, os.ErrNotExist
}

func (cfg *config) ensureRemoteDir(ctx context.Context, dir string, pageSize int) error {
	f, err := cfg.statRemote(ctx, dir, pageSize)
	if err == nil {
		if f.IsDir == 0 {
			return fmt.Errorf("remote directory is a file: bd:/%s", dir)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := cfg.ensureRemoteDir(ctx, path.Dir(dir), pageSize); err != nil {
		return err
	}
	var made remoteFile
	err = cfg.diskAPI(ctx, http.MethodPost, panBase+"/rest/2.0/xpan/file",
		values("method", "create"), values("path", dir, "isdir", "1", "size", "0", "block_list", "[]", "rtype", "0"), &made)
	if errors.Is(err, netdiskError(-8)) {
		// A concurrent creator is safe only when it created the exact directory.
		f, statErr := cfg.statRemote(ctx, dir, pageSize)
		if statErr == nil && f.IsDir == 1 {
			return nil
		}
		return errors.New("concurrent directory creation was not confirmed")
	}
	if err != nil {
		return err
	}
	if made.Path != dir || made.IsDir != 1 {
		return errors.New("directory creation was not confirmed at the requested path")
	}
	return nil
}

func (cfg *config) manage(ctx context.Context, operation, source, dest string, noOverwrite bool) error {
	var files any = []string{source}
	form := values("async", "0")
	if operation != "delete" {
		files = []map[string]string{{"path": source, "dest": path.Dir(dest), "newname": path.Base(dest)}}
		form.Set("ondup", "overwrite")
		if noOverwrite {
			form.Set("ondup", "fail")
		}
	}
	encoded, _ := json.Marshal(files)
	form.Set("filelist", string(encoded))
	var result struct {
		TaskID int64 `json:"taskid"`
		Info   []struct {
			Errno *int   `json:"errno"`
			Path  string `json:"path"`
		} `json:"info"`
	}
	if err := cfg.diskAPI(ctx, http.MethodPost, panBase+"/rest/2.0/xpan/file",
		values("method", "filemanager", "opera", operation), form, &result); err != nil {
		return err
	}
	if result.TaskID != 0 || len(result.Info) != 1 || result.Info[0].Errno == nil {
		return errors.New("file operation completion was not confirmed")
	}
	if result.Info[0].Path != "" && result.Info[0].Path != source {
		return errors.New("file operation returned an unexpected source path")
	}
	if *result.Info[0].Errno != 0 {
		return netdiskError(*result.Info[0].Errno)
	}
	return nil
}

func trustedDownloadURL(u *url.URL) bool {
	host := strings.ToLower(u.Hostname())
	return u.Scheme == "https" && u.User == nil && (u.Port() == "" || u.Port() == "443") &&
		(strings.HasSuffix(host, ".baidu.com") || strings.HasSuffix(host, ".baidupcs.com"))
}

func (cfg *config) downloadLink(ctx context.Context, file remoteFile) (string, error) {
	if file.ID <= 0 || file.Size < 0 {
		return "", errors.New("invalid file download metadata")
	}
	var result struct {
		List []remoteFile `json:"list"`
	}
	err := cfg.diskAPI(ctx, http.MethodGet, panBase+"/rest/2.0/xpan/multimedia",
		values("method", "filemetas", "fsids", fmt.Sprintf("[%d]", file.ID), "dlink", "1"), nil, &result)
	if err != nil {
		return "", err
	}
	if len(result.List) != 1 || result.List[0].ID != file.ID ||
		result.List[0].Size != file.Size || result.List[0].IsDir != 0 ||
		(result.List[0].Path != "" && result.List[0].Path != file.Path) ||
		(result.List[0].modified() != 0 && result.List[0].modified() != file.modified()) {
		return "", errors.New("download metadata changed or is incomplete")
	}
	u, err := url.Parse(result.List[0].Dlink)
	if err != nil || !trustedDownloadURL(u) {
		return "", errors.New("invalid or untrusted download URL")
	}
	token, err := cfg.resolveAccessToken()
	if err != nil {
		return "", withCode(253, err)
	}
	q := u.Query()
	q.Set("access_token", token)
	u.RawQuery = q.Encode()
	return u.String(), nil
}
