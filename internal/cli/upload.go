package cli

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

const chunkSize = 4 << 20

func uploadFile(ctx context.Context, cfg *config, local, remote string, noOverwrite bool, partConcurrency int, progress func(int64)) error {
	f, err := os.Open(local)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("upload source must be a regular file")
	}
	partSize := int64(chunkSize)
	if info.Size() > chunkSize {
		var membership struct {
			VIPType *int `json:"vip_type"`
		}
		if err := cfg.diskAPI(ctx, http.MethodGet, panBase+"/rest/2.0/xpan/nas",
			values("method", "uinfo", "vip_version", "v2"), nil, &membership); err != nil {
			return fmt.Errorf("query upload membership: %w", err)
		}
		if membership.VIPType == nil {
			return errors.New("membership response contains no vip_type")
		}
		switch *membership.VIPType {
		case 1:
			partSize = 16 << 20
		case 2:
			partSize = 32 << 20
		}
	}
	hashes, contentMD5, sliceMD5, err := hashReader(contextReader{ctx, f}, partSize)
	if err != nil {
		return err
	}
	if err := unchangedFile(local, info); err != nil {
		return err
	}
	rtype := "3"
	if noOverwrite {
		rtype = "0"
	}
	blockJSON, _ := json.Marshal(hashes)
	form := values(
		"path", remote, "size", strconv.FormatInt(info.Size(), 10), "isdir", "0",
		"block_list", string(blockJSON), "autoinit", "1", "rtype", rtype,
		"content-md5", contentMD5, "slice-md5", sliceMD5,
		"local_mtime", strconv.FormatInt(info.ModTime().Unix(), 10),
	)
	var pre struct {
		ReturnType int        `json:"return_type"`
		UploadID   string     `json:"uploadid"`
		BlockList  []int      `json:"block_list"`
		Info       remoteFile `json:"info"`
	}
	if err := cfg.diskAPI(ctx, http.MethodPost, panBase+"/rest/2.0/xpan/file",
		values("method", "precreate"), form, &pre); err != nil {
		return err
	}
	if pre.ReturnType == 2 {
		if pre.Info.ID <= 0 {
			return errors.New("rapid upload completion was not confirmed")
		}
		return unchangedFile(local, info)
	}
	if pre.UploadID == "" {
		return errors.New("precreate response contains no upload ID")
	}
	needed := pre.BlockList
	if len(needed) == 0 {
		needed = make([]int, len(hashes))
		for i := range needed {
			needed[i] = i
		}
	}
	seen := make(map[int]bool, len(needed))
	for _, index := range needed {
		if index < 0 || index >= len(hashes) || seen[index] {
			return fmt.Errorf("server requested invalid or repeated part %d", index)
		}
		seen[index] = true
	}
	if err := cfg.uploadParts(ctx, f, remote, pre.UploadID, hashes, needed, partSize, info.Size(), partConcurrency, progress); err != nil {
		return err
	}
	if err := unchangedFile(local, info); err != nil {
		return err
	}
	createForm := values(
		"path", remote, "size", strconv.FormatInt(info.Size(), 10), "isdir", "0",
		"block_list", string(blockJSON), "uploadid", pre.UploadID, "rtype", rtype,
		"local_mtime", strconv.FormatInt(info.ModTime().Unix(), 10),
	)
	var created remoteFile
	if err := cfg.diskAPI(ctx, http.MethodPost, panBase+"/rest/2.0/xpan/file",
		values("method", "create"), createForm, &created); err != nil {
		return err
	}
	if created.Path != remote || created.Size != info.Size() || created.IsDir != 0 || created.ID <= 0 {
		return errors.New("upload completion was not confirmed at the requested path")
	}
	return unchangedFile(local, info)
}

func (cfg *config) uploadParts(ctx context.Context, f *os.File, remote, uploadID string, hashes []string, needed []int, partSize, fileSize int64, concurrency int, progress func(int64)) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	token, err := cfg.resolveAccessToken()
	if err != nil {
		return withCode(253, err)
	}
	// Routing is scoped to this upload ID and refreshed on expiry or failure.
	var hostMu sync.Mutex
	var host string
	var expires time.Time
	getHost := func(refresh bool) (string, error) {
		hostMu.Lock()
		defer hostMu.Unlock()
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if refresh || host == "" || !time.Now().Before(expires) {
			var err error
			host, expires, err = cfg.locateUpload(ctx, token, remote, uploadID)
			if err != nil {
				return "", err
			}
		}
		return host, nil
	}
	var mu sync.Mutex
	next := 0
	work := func() {
		for {
			mu.Lock()
			if ctx.Err() != nil || next == len(needed) {
				mu.Unlock()
				return
			}
			index := needed[next]
			next++
			mu.Unlock()
			size := min(partSize, fileSize-int64(index)*partSize)
			for attempt := 0; ; attempt++ {
				host, err := getHost(attempt > 0)
				if err != nil {
					cancel(err)
					return
				}
				section := io.NewSectionReader(f, int64(index)*partSize, size)
				var sent int64
				report := func(n int64) {
					mu.Lock()
					defer mu.Unlock()
					sent += n
					if progress != nil {
						progress(n)
					}
				}
				retry, err := cfg.sendUploadPart(ctx, host, token, remote, uploadID, index, hashes[index], section, concurrency, report)
				if err == nil {
					break
				}
				// Failed attempts are not completed bytes. The UI separately
				// tracks positive traffic for its rolling speed measurement.
				report(-sent)
				if !retry || attempt == 2 || ctx.Err() != nil {
					cancel(fmt.Errorf("upload part %d: %w", index, err))
					return
				}
				// Retry only idempotent parts, not precreate/create. The reader
				// and multipart body are rebuilt for every attempt.
				if pauseRetry(ctx, attempt) != nil {
					return
				}
			}
		}
	}
	var group sync.WaitGroup
	for range max(1, min(concurrency, len(needed))) {
		group.Go(work)
	}
	group.Wait()
	return context.Cause(ctx)
}

func (cfg *config) locateUpload(ctx context.Context, token, remote, uploadID string) (string, time.Time, error) {
	// The routing request also carries a token; do not leak it via redirects.
	client := *cfg.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	requestConfig := *cfg
	requestConfig.client = &client
	body, err := requestConfig.request(ctx, http.MethodGet, "https://d.pcs.baidu.com/rest/2.0/pcs/file",
		values("method", "locateupload", "appid", "250528", "access_token", token,
			"path", remote, "uploadid", uploadID, "upload_version", "2.0"), nil, nil)
	if err != nil {
		return "", time.Time{}, err
	}
	var result struct {
		ErrorCode *int  `json:"error_code"`
		Expire    int64 `json:"expire"`
		Servers   []struct {
			Server string `json:"server"`
		} `json:"servers"`
	}
	if json.Unmarshal(body, &result) != nil || result.ErrorCode == nil {
		return "", time.Time{}, errors.New("invalid upload routing response")
	}
	if *result.ErrorCode != 0 {
		return "", time.Time{}, fmt.Errorf("locateupload error_code %d", *result.ErrorCode)
	}
	for _, server := range result.Servers {
		u, err := url.Parse(server.Server)
		if err == nil && trustedUploadURL(u) {
			// Treat expire conservatively as seconds, and never cache beyond
			// a minute. A missing/zero TTL causes a refresh for the next part.
			expires := time.Now().Add(time.Duration(max(0, min(result.Expire, 60))) * time.Second)
			return u.Scheme + "://" + u.Host, expires, nil
		}
	}
	return "", time.Time{}, errors.New("upload routing contains no trusted HTTPS server")
}

func trustedUploadURL(u *url.URL) bool {
	host := strings.ToLower(u.Hostname())
	return u.Scheme == "https" && u.User == nil && (u.Port() == "" || u.Port() == "443") &&
		(strings.HasSuffix(host, ".pcs.baidu.com") || strings.HasSuffix(host, ".baidupcs.com")) &&
		(u.Path == "" || u.Path == "/") && u.RawQuery == "" && u.Fragment == "" && !u.ForceQuery
}

func (cfg *config) uploadPart(ctx context.Context, host, token, remote, uploadID string, index int, hash string, section *io.SectionReader) (bool, error) {
	return cfg.sendUploadPart(ctx, host, token, remote, uploadID, index, hash, section, 1, nil)
}

func (cfg *config) sendUploadPart(ctx context.Context, host, token, remote, uploadID string, index int, hash string, section *io.SectionReader, concurrency int, progress func(int64)) (bool, error) {
	ctx, client, touch, stop := cfg.startTransfer(ctx, section.Size(), concurrency)
	defer stop()
	// Stream prefix + file section + suffix, with an exact Content-Length.
	// No full-part buffer and no pipe goroutine to strand on cancellation.
	var buffer bytes.Buffer
	w := multipart.NewWriter(&buffer)
	if _, err := w.CreateFormFile("file", path.Base(remote)); err != nil {
		return false, err
	}
	prefix := bytes.Clone(buffer.Bytes())
	buffer.Reset()
	if err := w.Close(); err != nil {
		return false, err
	}
	content := &activityReader{reader: contextReader{ctx, section}, read: func(n int) {
		touch(n)
		if progress != nil && n > 0 {
			progress(int64(n))
		}
	}}
	body := &uploadBody{reader: io.MultiReader(bytes.NewReader(prefix), content, &buffer)}
	defer body.Close()
	q := values("method", "upload", "access_token", token, "type", "tmpfile",
		"path", remote, "uploadid", uploadID, "partseq", strconv.Itoa(index))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, host+"/rest/2.0/pcs/superfile2?"+q.Encode(), body)
	if err != nil {
		return false, errors.New("invalid upload endpoint")
	}
	req.ContentLength = int64(len(prefix)+buffer.Len()) + section.Size()
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("User-Agent", "pan.baidu.com")
	// Upload URLs contain credentials: do not follow redirects or replay
	// file bytes to a host other than the validated routing result.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return true, transferError(ctx, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode == 429 || resp.StatusCode >= 500, fmt.Errorf("HTTP status %d", resp.StatusCode)
	}
	touch(1)
	data, err := io.ReadAll(io.LimitReader(&activityReader{reader: resp.Body, read: touch}, 1<<20))
	if err != nil {
		return true, transferError(ctx, err)
	}
	var uploaded struct {
		MD5       string `json:"md5"`
		Errno     int    `json:"errno"`
		ErrorCode int    `json:"error_code"`
	}
	// PCS success responses carry md5 and may omit errno.
	if json.Unmarshal(data, &uploaded) != nil {
		return false, errors.New("invalid upload part response")
	}
	if uploaded.Errno != 0 {
		return false, netdiskError(uploaded.Errno)
	}
	if uploaded.ErrorCode != 0 {
		return false, fmt.Errorf("upload error_code %d", uploaded.ErrorCode)
	}
	if uploaded.MD5 != hash {
		return false, errors.New("upload checksum was not verified")
	}
	return false, nil
}

func unchangedFile(name string, before os.FileInfo) error {
	after, err := os.Stat(name)
	if err != nil {
		return err
	}
	if !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return fmt.Errorf("source changed during operation: %s", name)
	}
	return nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func hashFile(name string) ([]string, string, string, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, "", "", err
	}
	defer f.Close()
	return hashReader(f, chunkSize)
}

func hashReader(r io.Reader, partSize int64) ([]string, string, string, error) {
	full := md5.New()
	var blocks []string
	buf := make([]byte, partSize)
	var first []byte
	for {
		n, readErr := io.ReadFull(r, buf)
		if n > 0 {
			chunk := buf[:n]
			if first == nil {
				first = append([]byte(nil), chunk[:min(n, 256<<10)]...)
			}
			full.Write(chunk)
			sum := md5.Sum(chunk)
			blocks = append(blocks, hex.EncodeToString(sum[:]))
		}
		if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
			break
		}
		if readErr != nil {
			return nil, "", "", readErr
		}
	}
	if len(blocks) == 0 {
		sum := md5.Sum(nil)
		blocks = []string{hex.EncodeToString(sum[:])}
	}
	slice := md5.Sum(first)
	return blocks, hex.EncodeToString(full.Sum(nil)), hex.EncodeToString(slice[:]), nil
}
