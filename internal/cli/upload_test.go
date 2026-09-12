package cli

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestUploadFileAcceptsRapidUpload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file.txt")
	if err := os.WriteFile(path, []byte("already uploaded"), 0o600); err != nil {
		t.Fatal(err)
	}

	requests := 0
	cfg := &config{
		accessToken: "token",
		client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			requests++
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"errno":0,"return_type":2,"info":{"fs_id":1,"path":"/file.txt","size":16,"isdir":0}}`)),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		})},
	}
	if err := uploadFile(context.Background(), cfg, path, "/file.txt", false, 1, nil); err != nil {
		t.Fatal(err)
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want 1", requests)
	}
}

func TestUploadFileRejectsUnconfirmedRapidUpload(t *testing.T) {
	local := localFile(t, "file.txt", []byte("already uploaded"))
	for _, tc := range []struct {
		name string
		body string
	}{
		{"missing-info", `{"errno":0,"return_type":2,"fs_id":1}`},
		{"missing-id", `{"errno":0,"return_type":2,"info":{}}`},
		{"zero-id", `{"errno":0,"return_type":2,"info":{"fs_id":0}}`},
		{"negative-id", `{"errno":0,"return_type":2,"info":{"fs_id":-1}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			cfg := &config{
				accessToken: "token",
				client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					requests++
					return response(req, []byte(tc.body)), nil
				})},
			}
			err := uploadFile(context.Background(), cfg, local, "/file.txt", false, 1, nil)
			if err == nil || !strings.Contains(err.Error(), "not confirmed") || requests != 1 {
				t.Fatalf("unconfirmed rapid upload: err=%v requests=%d", err, requests)
			}
		})
	}
}

func TestUploadMembershipPartSizes(t *testing.T) {
	for _, tc := range []struct{ vip, size int }{{0, 4 << 20}, {1, 16 << 20}, {2, 32 << 20}, {99, 4 << 20}} {
		t.Run(strconv.Itoa(tc.vip), func(t *testing.T) {
			d := newMockDisk()
			d.fail = func(c diskCall) string {
				if c.method == "uinfo" {
					if c.query.Get("vip_version") != "v2" {
						t.Error("membership did not request v2")
					}
					return fmt.Sprintf(`{"errno":0,"vip_type":%d}`, tc.vip)
				}
				return ""
			}
			data := bytes.Repeat([]byte("x"), tc.size+17)
			copy(data[tc.size:], "last partial part")
			local := localFile(t, "large", data)
			cfg := &config{accessToken: "test", client: &http.Client{Transport: d}}
			var completed int64
			if err := uploadFile(context.Background(), cfg, local, "/large", false, 3, func(n int64) { completed += n }); err != nil {
				t.Fatal(err)
			}
			if len(d.parts["/large"]) != 2 || len(d.parts["/large"][0]) != tc.size ||
				completed != int64(len(data)) || !bytes.Equal(d.data[d.files["/large"].ID], data) {
				t.Fatalf("wrong part sizes/content/progress: parts=%d completed=%d", len(d.parts["/large"]), completed)
			}
			for _, c := range d.calls {
				if c.method == "precreate" || c.method == "create" {
					var hashes []string
					_ = json.Unmarshal([]byte(c.form.Get("block_list")), &hashes)
					if len(hashes) != 2 || hashes[0] != fmt.Sprintf("%x", md5.Sum(data[:tc.size])) ||
						hashes[1] != fmt.Sprintf("%x", md5.Sum(data[tc.size:])) {
						t.Fatalf("%s used different chunk boundaries", c.method)
					}
				}
			}
		})
	}
}

func TestUploadRejectsInvalidPreparation(t *testing.T) {
	local := localFile(t, "large", make([]byte, chunkSize+1))
	for _, tc := range []struct{ method, body string }{
		{"uinfo", `{"errno":0}`},
		{"uinfo", `{"errno":-6}`},
		{"precreate", `{"errno":0,"uploadid":"upload","block_list":[0,2]}`},
		{"precreate", `{"errno":0,"uploadid":"upload","block_list":[0,-1]}`},
		{"precreate", `{"errno":0,"uploadid":"upload","block_list":[0,0]}`},
	} {
		t.Run(tc.body, func(t *testing.T) {
			d := newMockDisk()
			d.fail = func(c diskCall) string {
				if c.method == tc.method {
					return tc.body
				}
				return ""
			}
			cfg := &config{accessToken: "test", client: &http.Client{Transport: d}}
			if err := uploadFile(context.Background(), cfg, local, "/large", false, 3, nil); err == nil {
				t.Fatal("invalid preparation succeeded")
			}
			for _, c := range d.calls {
				if c.method == "upload" || c.method == "create" || c.method == "locateupload" {
					t.Fatalf("continued after invalid preparation: %s", c.method)
				}
			}
		})
	}
}

func TestUploadOnlyMissingParts(t *testing.T) {
	d := newMockDisk()
	data := bytes.Repeat([]byte("x"), chunkSize+3)
	d.fail = func(c diskCall) string {
		if c.method == "precreate" {
			d.parts["/file"] = map[int][]byte{0: data[:chunkSize]}
			return `{"errno":0,"uploadid":"upload","block_list":[1]}`
		}
		return ""
	}
	cfg := &config{accessToken: "test", client: &http.Client{Transport: d}}
	var completed int64
	err := uploadFile(context.Background(), cfg, localFile(t, "file", data), "/file", false, 3, func(n int64) { completed += n })
	if err != nil || completed != 3 || !bytes.Equal(d.data[d.files["/file"].ID], data) {
		t.Fatalf("partial upload: completed=%d err=%v", completed, err)
	}
	for _, c := range d.calls {
		if c.method == "upload" && c.query.Get("partseq") != "1" {
			t.Fatal("reuploaded an existing part")
		}
	}
}

func TestUploadStreamingProgressAndRetryRollback(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		data := bytes.Repeat([]byte("x"), 128<<10)
		local := localFile(t, "file", data)
		d := newMockDisk()
		attempts, updates, rollbacks := 0, 0, 0
		var completed int64
		cfg := &config{accessToken: "test", client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			r, err := d.RoundTrip(req)
			if req.URL.Query().Get("method") == "upload" {
				attempts++
				// Progress must already be visible before the acknowledgement.
				if completed != int64(len(data)) || updates < 2 {
					t.Errorf("no streaming progress: completed=%d updates=%d", completed, updates)
				}
				if attempts == 1 {
					r.StatusCode = 503
				}
			}
			return r, err
		})}}
		err := uploadFile(t.Context(), cfg, local, "/file", false, 1, func(n int64) {
			completed += n
			if n > 0 {
				updates++
			} else if n < 0 {
				rollbacks++
				if completed != 0 {
					t.Errorf("failed part retained progress: %d", completed)
				}
			}
		})
		if err != nil || attempts != 2 || rollbacks != 1 || completed != int64(len(data)) {
			t.Fatalf("retry progress: attempts=%d rollbacks=%d completed=%d err=%v", attempts, rollbacks, completed, err)
		}
	})
}

func TestUploadIdleFailureIsBounded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d := newMockDisk()
		attempts := 0
		cfg := &config{accessToken: "test", client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Query().Get("method") != "upload" {
				return d.RoundTrip(req)
			}
			attempts++
			<-req.Context().Done()
			return nil, req.Context().Err()
		})}}
		err := uploadFile(t.Context(), cfg, localFile(t, "file", []byte("data")), "/file", false, 1, nil)
		if !errors.Is(err, context.DeadlineExceeded) || attempts != 3 || !strings.Contains(err.Error(), "no data for") {
			t.Fatalf("idle failure: attempts=%d err=%v", attempts, err)
		}
		if _, created := d.files["/file"]; created {
			t.Fatal("committed an idle upload")
		}
	})
}

func TestParallelUploadParts(t *testing.T) {
	for _, mode := range []string{"serial", "parallel", "retry", "exhausted", "checksum", "cancel", "expiry"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				d := newMockDisk()
				d.parts["/file"] = map[int][]byte{}
				data := []byte("0123456789abcdefghijklmnopqrstuvwxyz")
				const size = 7
				hashes, _, _, _ := hashReader(bytes.NewReader(data), size)
				needed := make([]int, len(hashes))
				for i := range needed {
					needed[i] = i
				}
				f, err := os.Open(localFile(t, "parts", data))
				if err != nil {
					t.Fatal(err)
				}
				defer f.Close()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				var mu sync.Mutex
				active, peak, routes := 0, 0, 0
				attempts := map[int]int{}
				transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
					if req.URL.Query().Get("method") == "locateupload" {
						mu.Lock()
						routes++
						host := fmt.Sprintf("https://c%d.pcs.baidu.com", routes)
						mu.Unlock()
						ttl := 60
						if mode == "expiry" {
							ttl = 1
						}
						return response(req, fmt.Appendf(nil, `{"error_code":0,"expire":%d,"servers":[{"server":%q}]}`, ttl, host)), nil
					}
					index, _ := strconv.Atoi(req.URL.Query().Get("partseq"))
					mu.Lock()
					active++
					peak = max(peak, active)
					attempts[index]++
					attempt := attempts[index]
					mu.Unlock()
					defer func() {
						mu.Lock()
						active--
						mu.Unlock()
					}()
					if mode == "cancel" || (mode == "checksum" && index != 0) {
						<-req.Context().Done()
						return nil, req.Context().Err()
					}
					// Out-of-order completion, without wall-clock sleeps.
					time.Sleep(time.Duration(3-index%3) * time.Second)
					if index == 0 {
						if mode == "exhausted" || (mode == "retry" && attempt <= 2) {
							if attempt == 1 {
								return nil, errors.New("temporary network failure")
							}
							r := response(req, nil)
							r.StatusCode = 503
							return r, nil
						}
						if mode == "checksum" {
							return response(req, []byte(`{"md5":"wrong"}`)), nil
						}
					}
					return d.RoundTrip(req)
				})
				cfg := &config{accessToken: "test", client: &http.Client{Transport: transport}}
				workers := 3
				if mode == "serial" {
					workers = 1
				}
				var completed int64
				result := make(chan error, 1)
				go func() {
					result <- cfg.uploadParts(ctx, f, "/file", "upload", hashes, needed, size, int64(len(data)), workers,
						func(n int64) { completed += n })
				}()
				if mode == "cancel" {
					synctest.Wait()
					cancel()
				}
				err = <-result
				wantErr := mode == "checksum" || mode == "exhausted" || mode == "cancel"
				if (err != nil) != wantErr || (mode == "cancel" && !errors.Is(err, context.Canceled)) {
					t.Fatalf("result=%v mode=%s", err, mode)
				}
				if active != 0 || peak != workers {
					t.Fatalf("active=%d peak=%d want=%d", active, peak, workers)
				}
				if !wantErr {
					var uploaded []byte
					for i := range needed {
						uploaded = append(uploaded, d.parts["/file"][i]...)
					}
					if !bytes.Equal(uploaded, data) || completed != int64(len(data)) {
						t.Fatalf("content/progress mismatch: completed=%d", completed)
					}
				}
				if mode == "retry" || mode == "exhausted" {
					if attempts[0] != 3 || routes != 3 {
						t.Fatalf("retry/refresh not bounded: attempts=%v routes=%d", attempts, routes)
					}
				} else if mode == "expiry" {
					if routes < 2 {
						t.Fatal("expired route was reused")
					}
				} else if routes != 1 {
					t.Fatalf("unexpected route requests: %d", routes)
				}
			})
		})
	}
}

func TestUploadRoutingValidation(t *testing.T) {
	for _, tc := range []struct {
		body string
		want string
	}{
		{`{"error_code":0,"expire":60,"servers":[{"server":"http://c3.pcs.baidu.com"},{"server":"https://c4.pcs.baidu.com"}]}`, "https://c4.pcs.baidu.com"},
		{`{"error_code":0,"servers":[{"server":"https://upload.baidupcs.com/"}]}`, "https://upload.baidupcs.com"},
		{`{"error_code":0,"servers":[]}`, ""},
		{`{"error_code":0,"servers":[{"server":"https://c3.pcs.baidu.com.evil.test"}]}`, ""},
		{`{"error_code":0,"servers":[{"server":"https://user@c3.pcs.baidu.com"}]}`, ""},
		{`{"error_code":0,"servers":[{"server":"https://c3.pcs.baidu.com:123"}]}`, ""},
		{`{"error_code":0,"servers":[{"server":"https://c3.pcs.baidu.com/path"}]}`, ""},
		{`{"error_code":0,"servers":[{"server":"https://c3.pcs.baidu.com/?secret"}]}`, ""},
		{`{"error_code":0,"servers":[{"server":"https://c3.pcs.baidu.com/#secret"}]}`, ""},
		{`{"error_code":31023,"error_msg":"secret-token"}`, ""},
		{`{"errno":0}`, ""},
		{`not json secret-token`, ""},
	} {
		t.Run(tc.body, func(t *testing.T) {
			cfg := &config{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				q := req.URL.Query()
				if req.Method != http.MethodGet || req.URL.Host != "d.pcs.baidu.com" ||
					req.URL.Path != "/rest/2.0/pcs/file" || q.Get("method") != "locateupload" ||
					q.Get("appid") != "250528" || q.Get("access_token") != "secret-token" ||
					q.Get("path") != "/课程/file" || q.Get("uploadid") != "id" || q.Get("upload_version") != "2.0" {
					t.Error("invalid locateupload request")
				}
				return response(req, []byte(tc.body)), nil
			})}}
			host, _, err := cfg.locateUpload(context.Background(), "secret-token", "/课程/file", "id")
			if host != tc.want || (err == nil) != (tc.want != "") {
				t.Fatalf("host=%q err=%v", host, err)
			}
			if err != nil && strings.Contains(err.Error(), "secret-token") {
				t.Fatal("routing error leaked response/credentials")
			}
		})
	}
}

func TestUploadRoutingRejectsRedirect(t *testing.T) {
	calls := 0
	cfg := &config{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		r := response(req, nil)
		r.StatusCode = http.StatusTemporaryRedirect
		r.Header.Set("Location", "https://evil.test/steal")
		return r, nil
	})}}
	_, _, err := cfg.locateUpload(context.Background(), "secret-token", "/file", "id")
	if err == nil || calls != 1 || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("redirected routing request: calls=%d err=%v", calls, err)
	}
}

func TestUploadPartStreamingAndRedirect(t *testing.T) {
	data := []byte("video contents")
	f, err := os.Open(localFile(t, "spool-random", data))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, status := range []int{200, 307, 403, 429, 500} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			calls := 0
			cfg := &config{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				body, err := io.ReadAll(req.Body)
				if err != nil || int64(len(body)) != req.ContentLength || req.Header.Get("User-Agent") != "pan.baidu.com" {
					t.Errorf("invalid streaming request: length=%d declared=%d err=%v", len(body), req.ContentLength, err)
				}
				req.Body = io.NopCloser(bytes.NewReader(body))
				if err := req.ParseMultipartForm(4096); err != nil {
					t.Fatal(err)
				}
				defer req.MultipartForm.RemoveAll()
				part, header, err := req.FormFile("file")
				if err != nil {
					t.Fatal(err)
				}
				got, _ := io.ReadAll(part)
				part.Close()
				if header.Filename != "课程 01.mp4" || !bytes.Equal(got, data) {
					t.Error("streamed multipart changed filename/content")
				}
				r := response(req, fmt.Appendf(nil, `{"md5":"%x"}`, md5.Sum(data)))
				r.StatusCode = status
				r.Header.Set("Location", "https://evil.test/steal")
				return r, nil
			})}}
			retry, err := cfg.uploadPart(context.Background(), "https://c3.pcs.baidu.com", "secret", "/课程 01.mp4", "id",
				0, fmt.Sprintf("%x", md5.Sum(data)), io.NewSectionReader(f, 0, int64(len(data))))
			if calls != 1 || (err == nil) != (status == 200) || retry != (status == 429 || status == 500) {
				t.Fatalf("calls=%d retry=%v err=%v", calls, retry, err)
			}
		})
	}
}
