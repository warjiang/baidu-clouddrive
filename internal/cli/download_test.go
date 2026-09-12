package cli

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func TestRangedDownloads(t *testing.T) {
	for _, mode := range []string{
		"parallel", "retry", "truncated", "expired", "fallback",
		"bad-range", "bad-total", "bad-length", "overlong", "bad-md5",
		"changed-etag", "missing-etag", "changed-metadata", "exhausted", "cancel",
	} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				data := bytes.Repeat([]byte("01234567"), int(3*downloadPartSize/8)+3)
				d := newMockDisk()
				d.put("/file", data, false)
				target := localFile(t, "file", []byte("original"))
				var mu sync.Mutex
				attempts := map[int64]int{}
				active, peak, metadata, whole := 0, 0, 0, 0
				cfg := &config{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					if req.URL.Hostname() != "download.baidu.com" {
						if req.URL.Query().Get("method") == "filemetas" {
							mu.Lock()
							metadata++
							if mode == "changed-metadata" && metadata > 1 {
								d.mu.Lock()
								file := d.files["/file"]
								file.ServerMtime++
								d.files["/file"] = file
								d.mu.Unlock()
							}
							mu.Unlock()
						}
						return d.RoundTrip(req)
					}
					if req.Header.Get("Range") == "" {
						mu.Lock()
						whole++
						mu.Unlock()
						return response(req, data), nil
					}
					var start, end int64
					if _, err := fmt.Sscanf(req.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil ||
						start < 0 || end >= int64(len(data)) || end < start {
						t.Errorf("invalid range %q", req.Header.Get("Range"))
						return nil, errors.New("bad test range")
					}
					if start > 0 && req.Header.Get("If-Match") != `"file-v1"` {
						t.Error("missing strong validator on subsequent part")
					}
					mu.Lock()
					active++
					peak = max(peak, active)
					attempts[start]++
					attempt := attempts[start]
					mu.Unlock()
					defer func() {
						mu.Lock()
						active--
						mu.Unlock()
					}()
					if mode == "cancel" && start > 0 {
						<-req.Context().Done()
						return nil, req.Context().Err()
					}
					time.Sleep(2 * time.Second)
					r := response(req, data[start:end+1])
					r.StatusCode = 206
					r.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
					r.Header.Set("ETag", `"file-v1"`)
					digest := md5.Sum(data[start : end+1])
					r.Header.Set("Content-MD5", base64.StdEncoding.EncodeToString(digest[:]))
					if mode == "fallback" {
						return response(req, data), nil
					}
					if start == downloadPartSize {
						switch mode {
						case "retry":
							if attempt == 1 {
								r.StatusCode = 503
							}
						case "expired":
							if attempt == 1 {
								r = response(req, []byte(`{"errno":31360,"show_msg":"SECRET"}`))
								r.Header.Set("Content-Type", "application/json")
							}
						case "truncated":
							if attempt == 1 {
								r.Body = io.NopCloser(bytes.NewReader(data[start : start+123]))
							}
						case "bad-range":
							r.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start+1, end+1, len(data)))
						case "bad-total":
							r.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)+1))
						case "bad-length":
							r.ContentLength--
						case "overlong":
							r.ContentLength = -1
							r.Body = io.NopCloser(bytes.NewReader(data[start : end+2]))
						case "bad-md5":
							r.Header.Set("Content-MD5", "incorrect")
						case "changed-etag":
							r.Header.Set("ETag", `"file-v2"`)
						case "missing-etag":
							r.Header.Del("ETag")
						case "exhausted":
							r.StatusCode = 503
						}
					}
					return r, nil
				})}}
				root := newRoot("test", cfg)
				root.SetOut(io.Discard)
				root.SetErr(io.Discard)
				root.SetArgs([]string{"mv", "bd://file", target, "--part-concurrency", "4", "--access-token", "SECRET"})
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				done := make(chan error, 1)
				go func() { done <- root.ExecuteContext(ctx) }()
				if mode == "cancel" {
					time.Sleep(3 * time.Second)
					synctest.Wait()
					cancel()
				}
				err := <-done
				success := mode == "parallel" || mode == "retry" || mode == "truncated" || mode == "expired" || mode == "fallback"
				if (err == nil) != success {
					t.Fatalf("unexpected result: %v", err)
				}
				if err != nil && strings.Contains(err.Error(), "SECRET") {
					t.Fatal("credentials leaked")
				}
				if active != 0 || peak > 4 {
					t.Fatalf("workers not bounded/joined: active=%d peak=%d", active, peak)
				}
				if mode == "parallel" && peak != 3 {
					t.Fatalf("parts did not overlap: peak=%d", peak)
				}
				if mode == "retry" || mode == "truncated" || mode == "expired" {
					if attempts[downloadPartSize] != 2 || attempts[0] != 1 || metadata < 2 {
						t.Fatalf("retried the wrong parts: %v metadata=%d", attempts, metadata)
					}
				}
				if mode == "exhausted" && attempts[downloadPartSize] != 3 {
					t.Fatalf("retry limit: %v", attempts)
				}
				if mode == "fallback" && (whole != 1 || len(attempts) != 1) {
					t.Fatalf("unsafe fallback: whole=%d parts=%v", whole, attempts)
				}
				if mode == "cancel" && !errors.Is(err, context.Canceled) {
					t.Fatalf("lost cancellation: %v", err)
				}
				if success {
					assertLocal(t, target, data)
				} else {
					assertLocal(t, target, []byte("original"))
				}
				_, sourceExists := d.files["/file"]
				if sourceExists == success {
					t.Fatal("move removed source before completion, or failed to remove it after success")
				}
				entries, err := os.ReadDir(filepath.Dir(target))
				if err != nil || len(entries) != 1 {
					t.Fatalf("temporary file leaked: %v %v", entries, err)
				}
			})
		})
	}
}

func TestDownloadStdoutNeverReplaysBytes(t *testing.T) {
	d := newMockDisk()
	d.put("/file", []byte("abcdef"), false)
	requests := 0
	cfg := &config{accessToken: "test", client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Hostname() != "download.baidu.com" {
			return d.RoundTrip(req)
		}
		requests++
		if req.Header.Get("Range") != "" {
			t.Fatal("stdout used Range")
		}
		r := response(req, []byte("ab"))
		r.ContentLength = 6
		return r, nil
	})}}
	var out bytes.Buffer
	if err := cfg.download(t.Context(), d.files["/file"], &out); err == nil || out.String() != "ab" || requests != 1 {
		t.Fatalf("stdout replay: requests=%d out=%q err=%v", requests, out.String(), err)
	}
}

func TestDownloadSerialRetryRollsBackProgress(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		data := []byte("abcdef")
		d := newMockDisk()
		d.put("/file", data, false)
		requests := 0
		cfg := &config{accessToken: "test", client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Hostname() != "download.baidu.com" {
				return d.RoundTrip(req)
			}
			requests++
			if req.Header.Get("Range") != "" {
				t.Fatal("serial download used Range")
			}
			r := response(req, data)
			if requests == 1 {
				r.Body = io.NopCloser(bytes.NewReader(data[:2]))
			}
			return r, nil
		})}}
		target := filepath.Join(t.TempDir(), "file")
		f, err := os.Create(target)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		var completed int64
		rollbacks := 0
		err = cfg.downloadFile(t.Context(), d.files["/file"], f, 1, func(n int64) {
			completed += n
			if n < 0 {
				rollbacks++
			}
		})
		if err != nil || requests != 2 || rollbacks != 1 || completed != int64(len(data)) {
			t.Fatalf("serial retry: requests=%d rollbacks=%d completed=%d err=%v", requests, rollbacks, completed, err)
		}
		assertLocal(t, target, data)
	})
}

func TestDownloadChunkedJSONFile(t *testing.T) {
	data := []byte(`{"errno":31360,"message":"this is a user's JSON file"}`)
	d := newMockDisk()
	d.put("/file", data, false)
	cfg := &config{accessToken: "test", client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		r, err := d.RoundTrip(req)
		if req.URL.Hostname() == "download.baidu.com" {
			r.Header.Set("Content-Type", "application/json")
			r.ContentLength = -1
		}
		return r, err
	})}}
	var out bytes.Buffer
	if err := cfg.download(t.Context(), d.files["/file"], &out); err != nil || !bytes.Equal(out.Bytes(), data) {
		t.Fatalf("JSON file mistaken for API error: %q %v", out.Bytes(), err)
	}
}
