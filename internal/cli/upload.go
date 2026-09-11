package cli

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"github.com/spf13/cobra"
)

const (
	pcsBase   = "https://c3.pcs.baidu.com"
	chunkSize = 4 << 20
)

func newUploadPartCmd(cfg *config) *cobra.Command {
	var path, uploadID, file string
	var part int
	var yes bool
	cmd := &cobra.Command{
		Use:   "upload-part",
		Short: "Upload one file part",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !yes {
				return errors.New("this command changes the netdisk; pass --yes to continue")
			}
			if path == "" || uploadID == "" || file == "" {
				return errors.New("--path, --uploadid and --file are required")
			}
			if part < 0 {
				return errors.New("--partseq must be zero or greater")
			}
			token, err := cfg.resolveAccessToken()
			if err != nil {
				return err
			}
			f, err := os.Open(file)
			if err != nil {
				return err
			}
			defer f.Close()
			q := values(
				"method", "upload",
				"access_token", token,
				"type", "tmpfile",
				"path", path,
				"uploadid", uploadID,
				"partseq", strconv.Itoa(part),
			)
			return cfg.doJSON(cmd, http.MethodPost, pcsBase+"/rest/2.0/pcs/superfile2", q, nil, func(w *multipart.Writer) error {
				partWriter, err := w.CreateFormFile("file", filepath.Base(file))
				if err != nil {
					return err
				}
				_, err = io.Copy(partWriter, f)
				return err
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&path, "path", "", "Absolute destination path")
	f.StringVar(&uploadID, "uploadid", "", "Upload ID returned by precreate")
	f.IntVar(&part, "partseq", 0, "Part sequence number")
	f.StringVar(&file, "file", "", "Local part file")
	f.BoolVar(&yes, "yes", false, "Confirm writing to the netdisk")
	return cmd
}

func newUploadCmd(cfg *config) *cobra.Command {
	var local, remote string
	var rtype int
	var yes bool
	cmd := &cobra.Command{
		Use:   "upload",
		Short: "Precreate, upload parts, and create a file",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !yes {
				return errors.New("this command creates a netdisk file; pass --yes to continue")
			}
			if local == "" || remote == "" {
				return errors.New("--file and --path are required")
			}
			return uploadFile(cmd, cfg, local, remote, rtype)
		},
	}
	f := cmd.Flags()
	f.StringVar(&local, "file", "", "Local file")
	f.StringVar(&remote, "path", "", "Absolute destination path on the netdisk")
	f.IntVar(&rtype, "rtype", 1, "1 rename on conflict/2 rename if content differs/3 overwrite")
	f.BoolVar(&yes, "yes", false, "Confirm creating a netdisk file")
	return cmd
}

func uploadFile(cmd *cobra.Command, cfg *config, local, remote string, rtype int) error {
	if rtype < 1 || rtype > 3 {
		return errors.New("--rtype must be 1, 2 or 3")
	}
	tokenValue, err := cfg.resolveAccessToken()
	if err != nil {
		return err
	}
	info, err := os.Stat(local)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("--file must be a regular file")
	}
	hashes, contentMD5, sliceMD5, err := hashFile(local)
	if err != nil {
		return err
	}
	blockJSON, _ := json.Marshal(hashes)
	form := values(
		"path", remote,
		"size", strconv.FormatInt(info.Size(), 10),
		"isdir", "0",
		"block_list", string(blockJSON),
		"autoinit", "1",
		"rtype", strconv.Itoa(rtype),
		"content-md5", contentMD5,
		"slice-md5", sliceMD5,
	)
	body, err := cfg.request(cmd.Context(), http.MethodPost, panBase+"/rest/2.0/xpan/file", values("method", "precreate", "access_token", tokenValue), form, nil)
	if err != nil {
		return err
	}
	var pre struct {
		Errno      int    `json:"errno"`
		ReturnType int    `json:"return_type"`
		UploadID   string `json:"uploadid"`
		BlockList  []int  `json:"block_list"`
	}
	if err := json.Unmarshal(body, &pre); err != nil || pre.Errno != 0 {
		if err == nil {
			err = fmt.Errorf("precreate failed with errno %d", pre.Errno)
		}
		return printResponse(cmd, body, err)
	}
	if pre.ReturnType == 2 {
		return printResponse(cmd, body, nil)
	}
	if pre.UploadID == "" {
		return printResponse(cmd, body, errors.New("precreate response contains no upload ID"))
	}
	needed := pre.BlockList
	if len(needed) == 0 {
		needed = make([]int, len(hashes))
		for i := range needed {
			needed[i] = i
		}
	}
	f, err := os.Open(local)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, index := range needed {
		if index < 0 || index >= len(hashes) {
			return fmt.Errorf("server requested invalid part %d", index)
		}
		size := int64(chunkSize)
		if remain := info.Size() - int64(index)*chunkSize; remain < size {
			size = remain
		}
		section := io.NewSectionReader(f, int64(index)*chunkSize, size)
		q := values(
			"method", "upload",
			"access_token", tokenValue,
			"type", "tmpfile",
			"path", remote,
			"uploadid", pre.UploadID,
			"partseq", strconv.Itoa(index),
		)
		partBody, err := cfg.request(cmd.Context(), http.MethodPost, pcsBase+"/rest/2.0/pcs/superfile2", q, nil, func(w *multipart.Writer) error {
			part, err := w.CreateFormFile("file", filepath.Base(local))
			if err != nil {
				return err
			}
			_, err = io.Copy(part, section)
			return err
		})
		if err != nil {
			return err
		}
		if err := apiError(partBody); err != nil {
			return printResponse(cmd, partBody, fmt.Errorf("upload part %d: %w", index, err))
		}
	}
	createForm := values(
		"path", remote,
		"size", strconv.FormatInt(info.Size(), 10),
		"isdir", "0",
		"block_list", string(blockJSON),
		"uploadid", pre.UploadID,
		"rtype", strconv.Itoa(rtype),
	)
	return cfg.doJSON(cmd, http.MethodPost, panBase+"/rest/2.0/xpan/file", values("method", "create", "access_token", tokenValue), createForm, nil)
}

func hashFile(path string) ([]string, string, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, "", "", err
	}
	defer f.Close()
	full := md5.New()
	var blocks []string
	buf := make([]byte, chunkSize)
	var first []byte
	for {
		n, readErr := io.ReadFull(f, buf)
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
		first = []byte{}
	}
	slice := md5.Sum(first)
	return blocks, hex.EncodeToString(full.Sum(nil)), hex.EncodeToString(slice[:]), nil
}
