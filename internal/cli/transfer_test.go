package cli

import (
	"bytes"
	"context"
	"crypto/md5"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"testing/synctest"
	"time"
)

func TestTransferIdleBudgetAndCancellation(t *testing.T) {
	if transferIdleBudget(32<<20, 8) != 2*time.Minute ||
		transferIdleBudget(8<<20, 1) != 38*time.Second ||
		transferIdleBudget(1<<62, 32) != 2*time.Minute {
		t.Fatal("unexpected automatic idle budgets")
	}
	for _, canceled := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			parent, cancel := context.WithCancel(t.Context())
			defer cancel()
			cfg := &config{client: &http.Client{Timeout: 30 * time.Second}}
			ctx, client, touch, stop := cfg.startTransfer(parent, 1, 1)
			defer stop()
			if client.Timeout != 0 || cfg.client.Timeout != 30*time.Second {
				t.Fatal("transfer must not change the shared API client")
			}
			for range 10 {
				time.Sleep(20 * time.Second)
				touch(1)
				if ctx.Err() != nil {
					t.Fatal("active transfer expired")
				}
			}
			if canceled {
				cancel()
				if !errors.Is(context.Cause(ctx), context.Canceled) {
					t.Fatal("parent cancellation did not propagate")
				}
			} else {
				time.Sleep(31 * time.Second)
				if !errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
					t.Fatalf("idle transfer did not expire: %v", context.Cause(ctx))
				}
			}
		})
	}
}

// Reproduce a healthy body taking longer than the ordinary API timeout.
func TestTransfersOutliveAPIRequestTimeout(t *testing.T) {
	for _, direction := range []string{"upload", "download"} {
		t.Run(direction, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				data := bytes.Repeat([]byte("a"), 65536)
				d := newMockDisk()
				d.put("/file", data, false)
				cfg := &config{accessToken: "test", client: &http.Client{Timeout: 30 * time.Second}}
				cfg.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
					if req.URL.Query().Get("method") == "upload" {
						buf := make([]byte, 4096)
						for {
							_, err := req.Body.Read(buf)
							if err == io.EOF {
								break
							}
							if err != nil {
								return nil, err
							}
							select {
							case <-req.Context().Done():
								return nil, req.Context().Err()
							case <-time.After(5 * time.Second):
							}
						}
						return response(req, fmt.Appendf(nil, `{"md5":"%x"}`, md5.Sum(data))), nil
					}
					if req.URL.Hostname() == "download.baidu.com" {
						r := response(req, data)
						r.Body = &slowTransferBody{ctx: req.Context(), data: bytes.NewReader(data)}
						return r, nil
					}
					return d.RoundTrip(req)
				})
				if direction == "upload" {
					f, err := os.Open(localFile(t, "file", data))
					if err != nil {
						t.Fatal(err)
					}
					defer f.Close()
					_, err = cfg.uploadPart(t.Context(), "https://c3.pcs.baidu.com", "test", "/file",
						"id", 0, fmt.Sprintf("%x", md5.Sum(data)), io.NewSectionReader(f, 0, int64(len(data))))
					if err != nil {
						t.Fatal(err)
					}
				} else {
					var got bytes.Buffer
					if err := cfg.download(t.Context(), d.files["/file"], &got); err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(got.Bytes(), data) {
						t.Fatal("download corrupted")
					}
				}
			})
		})
	}
}

type slowTransferBody struct {
	ctx  context.Context
	data *bytes.Reader
}

func (b *slowTransferBody) Read(p []byte) (int, error) {
	select {
	case <-b.ctx.Done():
		return 0, b.ctx.Err()
	case <-time.After(5 * time.Second):
		return b.data.Read(p[:min(len(p), 4096)])
	}
}

func (b *slowTransferBody) Close() error { return nil }
