package cli

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const downloadPartSize int64 = 8 << 20

var errRangeUnsupported = errors.New("server does not support range downloads")

func (cfg *config) openDownload(ctx context.Context, link string, size, start, end int64, etag string, concurrency int) (*http.Response, error) {
	requestSize := size
	if start >= 0 {
		requestSize = end - start + 1
	}
	ctx, client, touch, stop := cfg.startTransfer(ctx, requestSize, concurrency)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		stop()
		return nil, errors.New("invalid download request")
	}
	req.Header.Set("User-Agent", "pan.baidu.com")
	req.Header.Set("Accept-Encoding", "identity")
	if start >= 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	}
	if etag != "" {
		req.Header.Set("If-Match", etag)
	}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 || !trustedDownloadURL(req.URL) {
			return errors.New("untrusted download redirect")
		}
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		err = transferError(ctx, err)
		stop()
		return nil, err
	}
	touch(1)
	resp.Body = &transferBody{ReadCloser: resp.Body, touch: touch, stop: stop,
		failure: func(err error) error { return transferError(ctx, err) }}
	return resp, nil
}

// Validate the response before writing any bytes, especially before falling back
// from Range. JSON is inspected only when it cannot be the expected file body.
func checkDownloadResponse(resp *http.Response, size, start, end int64, etag string) (bool, error) {
	if resp.StatusCode == 429 || resp.StatusCode >= 500 {
		return true, fmt.Errorf("download HTTP status %d", resp.StatusCode)
	}
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return true, fmt.Errorf("download HTTP status %d", resp.StatusCode)
	}
	expected := size
	if start >= 0 {
		expected = end - start + 1
	}
	if strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "application/json") &&
		resp.ContentLength >= 0 && resp.ContentLength != size && (start < 0 || resp.StatusCode != http.StatusPartialContent) {
		body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if err != nil {
			return true, err
		}
		var failure struct {
			Errno int `json:"errno"`
		}
		if json.Unmarshal(body, &failure) == nil && failure.Errno != 0 {
			return failure.Errno == 31360, netdiskError(failure.Errno)
		}
		return false, errors.New("invalid download error response")
	}
	if start >= 0 && resp.StatusCode == http.StatusOK {
		return false, errRangeUnsupported
	}
	status := http.StatusOK
	if start >= 0 {
		status = http.StatusPartialContent
		if resp.Header.Get("Content-Range") != fmt.Sprintf("bytes %d-%d/%d", start, end, size) {
			return false, errors.New("download Content-Range does not match requested range")
		}
	} else if resp.Header.Get("Content-Range") != "" {
		return false, errors.New("unexpected range in full download")
	}
	if resp.StatusCode != status {
		return false, fmt.Errorf("download HTTP status %d", resp.StatusCode)
	}
	if encoding := resp.Header.Get("Content-Encoding"); encoding != "" && encoding != "identity" {
		return false, errors.New("unexpected download Content-Encoding")
	}
	if etag != "" && resp.Header.Get("ETag") != etag {
		return false, errors.New("download ETag changed or is missing")
	}
	if resp.ContentLength >= 0 && resp.ContentLength != expected {
		return false, errors.New("download length does not match metadata")
	}
	return false, nil
}

// The reader is bounded even for chunked bodies. Writer failures are never
// classified as network retries (notably disk-full and broken stdout).
func copyDownload(resp *http.Response, target io.Writer, size int64) (bool, error) {
	hash := md5.New()
	left := size
	buffer := make([]byte, 64<<10)
	for left > 0 {
		n, readErr := resp.Body.Read(buffer[:min(int64(len(buffer)), left)])
		if n > 0 {
			written, err := target.Write(buffer[:n])
			if err != nil {
				return false, err
			}
			if written != n {
				return false, io.ErrShortWrite
			}
			_, _ = hash.Write(buffer[:n])
			left -= int64(n)
		}
		if readErr != nil {
			if readErr == io.EOF && left == 0 {
				break
			}
			return true, fmt.Errorf("incomplete download: %w", readErr)
		}
	}
	var extra [1]byte
	n, err := io.ReadFull(resp.Body, extra[:])
	if n != 0 {
		return false, errors.New("overlong download")
	}
	if err != io.EOF {
		return true, fmt.Errorf("download final read: %w", err)
	}
	if digest := resp.Header.Get("Content-MD5"); digest != "" &&
		digest != base64.StdEncoding.EncodeToString(hash.Sum(nil)) {
		return false, errors.New("download Content-MD5 mismatch")
	}
	return false, nil
}

// stdout cannot roll back already emitted bytes; keep it a single stream.
func (cfg *config) download(ctx context.Context, file remoteFile, target io.Writer) error {
	link, err := cfg.downloadLink(ctx, file)
	if err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		resp, err := cfg.openDownload(ctx, link, file.Size, -1, 0, "", 1)
		retry := err != nil
		if err == nil {
			retry, err = checkDownloadResponse(resp, file.Size, -1, 0, "")
			if err == nil {
				_, err = copyDownload(resp, target, file.Size)
				resp.Body.Close()
				if err == nil {
					err = ctx.Err()
				}
				return err
			}
			resp.Body.Close()
		}
		if !retry || attempt == 2 || ctx.Err() != nil {
			return err
		}
		if err := pauseRetry(ctx, attempt); err != nil {
			return err
		}
		link, err = cfg.downloadLink(ctx, file)
		if err != nil {
			return err
		}
	}
}

func (cfg *config) downloadFile(ctx context.Context, file remoteFile, target *os.File, concurrency int, progress func(int64)) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	link, err := cfg.downloadLink(ctx, file)
	if err != nil {
		return err
	}
	// Share refreshed links; failures on an old link must not each refresh it.
	linkGate := make(chan struct{}, 1)
	linkGate <- struct{}{}
	expires := time.Now().Add(7 * time.Hour)
	getLink := func(failed string) (string, error) {
		select {
		case <-ctx.Done():
			return "", context.Cause(ctx)
		case <-linkGate:
		}
		defer func() { linkGate <- struct{}{} }()
		if time.Now().After(expires) || (failed != "" && failed == link) {
			fresh, err := cfg.downloadLink(ctx, file)
			if err != nil {
				return "", err
			}
			link, expires = fresh, time.Now().Add(7*time.Hour)
		}
		return link, nil
	}
	var progressMu sync.Mutex
	report := func(n int64) {
		progressMu.Lock()
		defer progressMu.Unlock()
		if progress != nil {
			progress(n)
		}
	}
	fetch := func(start, end int64, etag string) (string, error) {
		address, err := getLink("")
		if err != nil {
			return "", err
		}
		for attempt := 0; ; attempt++ {
			resp, err := cfg.openDownload(ctx, address, file.Size, start, end, etag, concurrency)
			retry := err != nil
			var received int64
			var validator string
			if err == nil {
				retry, err = checkDownloadResponse(resp, file.Size, start, end, etag)
				if err == nil {
					validator = resp.Header.Get("ETag")
					size, offset := file.Size, int64(0)
					if start >= 0 {
						size, offset = end-start+1, start
					}
					writer := &downloadWriter{writer: io.NewOffsetWriter(target, offset), progress: func(n int64) {
						received += n
						report(n)
					}}
					retry, err = copyDownload(resp, writer, size)
				}
				resp.Body.Close()
			}
			if err == nil {
				return validator, ctx.Err()
			}
			report(-received)
			if !retry || attempt == 2 || ctx.Err() != nil {
				return "", err
			}
			if err := pauseRetry(ctx, attempt); err != nil {
				return "", err
			}
			address, err = getLink(address)
			if err != nil {
				return "", err
			}
		}
	}
	if concurrency <= 1 || file.Size <= downloadPartSize {
		_, err := fetch(-1, 0, "")
		return err
	}
	// The first part doubles as a capability probe and establishes the validator.
	etag, err := fetch(0, downloadPartSize-1, "")
	if errors.Is(err, errRangeUnsupported) {
		_, err = fetch(-1, 0, "")
		return err
	}
	if err != nil {
		return err
	}
	if !strings.HasPrefix(etag, `"`) || !strings.HasSuffix(etag, `"`) {
		etag = "" // Weak/missing validators cannot be used in If-Match.
	}
	var nextMu sync.Mutex
	next := downloadPartSize
	work := func() {
		for {
			nextMu.Lock()
			start := next
			end := start + min(downloadPartSize, file.Size-start) - 1
			if ctx.Err() != nil || start >= file.Size {
				nextMu.Unlock()
				return
			}
			next = end + 1
			nextMu.Unlock()
			if _, err := fetch(start, end, etag); err != nil {
				cancel(fmt.Errorf("download bytes %d-%d: %w", start, end, err))
				return
			}
		}
	}
	var workers sync.WaitGroup
	for range int(min(int64(max(1, concurrency)), (file.Size-1)/downloadPartSize)) {
		workers.Go(work)
	}
	workers.Wait()
	if err := context.Cause(ctx); err != nil {
		return err
	}
	// Without a strong ETag this is the available server-side identity check.
	// Do not compare Netdisk's metadata MD5: it may be transformed by the API.
	_, err = cfg.downloadLink(ctx, file)
	return err
}

type downloadWriter struct {
	writer   io.Writer
	progress func(int64)
}

func (w *downloadWriter) Write(p []byte) (int, error) {
	n, err := w.writer.Write(p)
	w.progress(int64(n))
	return n, err
}
