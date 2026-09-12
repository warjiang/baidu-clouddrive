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
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

type diskCall struct {
	method string
	query  url.Values
	form   url.Values
}

type mockDisk struct {
	mu      sync.Mutex
	files   map[string]remoteFile
	data    map[int64][]byte
	parts   map[string]map[int][]byte
	calls   []diskCall
	nextID  int64
	fail    func(diskCall) string
	content func([]byte) []byte
}

func newMockDisk() *mockDisk {
	d := &mockDisk{files: map[string]remoteFile{}, data: map[int64][]byte{}, parts: map[string]map[int][]byte{}}
	d.put("/", nil, true)
	return d
}

func (d *mockDisk) put(name string, data []byte, dir bool) {
	if name != "/" {
		if _, ok := d.files[path.Dir(name)]; !ok {
			d.put(path.Dir(name), nil, true)
		}
	}
	d.nextID++
	f := remoteFile{Path: name, ID: d.nextID, Size: int64(len(data)), ServerMtime: 1000}
	if dir {
		f.IsDir = 1
	}
	d.files[name] = f
	d.data[f.ID] = bytes.Clone(data)
}

func response(req *http.Request, body []byte) *http.Response {
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)), Request: req}
}

func (d *mockDisk) RoundTrip(req *http.Request) (*http.Response, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	q := req.URL.Query()
	if req.URL.Hostname() == "download.baidu.com" {
		id, _ := strconv.ParseInt(q.Get("id"), 10, 64)
		b := d.data[id]
		if d.content != nil {
			b = d.content(b)
		}
		return response(req, b), nil
	}
	var form url.Values
	if req.Method == http.MethodPost && !strings.HasPrefix(req.Header.Get("Content-Type"), "multipart/") {
		if err := req.ParseForm(); err != nil {
			return nil, err
		}
		form = req.PostForm
	}
	call := diskCall{q.Get("method"), q, form}
	d.calls = append(d.calls, call)
	if d.fail != nil {
		if body := d.fail(call); body != "" {
			return response(req, []byte(body)), nil
		}
	}
	var result any
	switch call.method {
	case "uinfo":
		result = map[string]any{"errno": 0, "vip_type": 0}
	case "locateupload":
		result = map[string]any{"error_code": 0, "expire": 60,
			"servers": []map[string]string{{"server": "https://c3.pcs.baidu.com"}}}
	case "list":
		if f, ok := d.files[q.Get("dir")]; !ok || f.IsDir == 0 {
			result = map[string]any{"errno": -9}
			break
		}
		list := []remoteFile{}
		for name, f := range d.files {
			if name != "/" && path.Dir(name) == q.Get("dir") {
				list = append(list, f)
			}
		}
		slices.SortFunc(list, func(a, b remoteFile) int { return strings.Compare(a.Path, b.Path) })
		start, _ := strconv.Atoi(q.Get("start"))
		limit, _ := strconv.Atoi(q.Get("limit"))
		result = map[string]any{"errno": 0, "list": list[min(start, len(list)):min(start+limit, len(list))]}
	case "filemetas":
		var ids []int64
		_ = json.Unmarshal([]byte(q.Get("fsids")), &ids)
		list := []remoteFile{}
		for _, f := range d.files {
			if len(ids) == 1 && f.ID == ids[0] {
				f.Dlink = fmt.Sprintf("https://download.baidu.com/file?id=%d&signature=secret", f.ID)
				list = append(list, f)
			}
		}
		result = map[string]any{"errno": 0, "list": list}
	case "precreate":
		name := form.Get("path")
		if _, exists := d.files[name]; exists && form.Get("rtype") == "0" {
			result = map[string]any{"errno": -8}
			break
		}
		d.parts[name] = map[int][]byte{}
		result = map[string]any{"errno": 0, "uploadid": "upload", "block_list": []int{}}
	case "upload":
		if err := req.ParseMultipartForm(chunkSize + 4096); err != nil {
			return nil, err
		}
		defer req.MultipartForm.RemoveAll()
		f, _, err := req.FormFile("file")
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			return nil, err
		}
		index, _ := strconv.Atoi(q.Get("partseq"))
		d.parts[q.Get("path")][index] = data
		sum := md5.Sum(data)
		result = map[string]any{"md5": hex.EncodeToString(sum[:])}
	case "create":
		name := form.Get("path")
		if _, exists := d.files[name]; exists && form.Get("rtype") == "0" {
			result = map[string]any{"errno": -8}
			break
		}
		var data []byte
		for i := 0; i < len(d.parts[name]); i++ {
			data = append(data, d.parts[name][i]...)
		}
		d.put(name, data, form.Get("isdir") == "1")
		f := d.files[name]
		if f.IsDir == 0 {
			f.ServerMtime = time.Now().Unix()
			d.files[name] = f
		}
		result = struct {
			Errno int `json:"errno"`
			remoteFile
		}{0, f}
	case "filemanager":
		operation := q.Get("opera")
		if form.Get("async") != "0" {
			return nil, errors.New("test requires synchronous file management")
		}
		var source string
		if operation == "delete" {
			var names []string
			_ = json.Unmarshal([]byte(form.Get("filelist")), &names)
			source = names[0]
			delete(d.files, source)
		} else {
			var entries []map[string]string
			_ = json.Unmarshal([]byte(form.Get("filelist")), &entries)
			source = entries[0]["path"]
			target := path.Join(entries[0]["dest"], entries[0]["newname"])
			if _, exists := d.files[target]; exists && form.Get("ondup") == "fail" {
				result = map[string]any{"errno": -8}
				break
			}
			d.put(target, d.data[d.files[source].ID], false)
		}
		result = map[string]any{"errno": 0, "info": []map[string]any{{"errno": 0, "path": source}}}
	default:
		return nil, fmt.Errorf("unexpected API method %q", call.method)
	}
	body, err := json.Marshal(result)
	return response(req, body), err
}

func (d *mockDisk) writes() int {
	n := 0
	for _, c := range d.calls {
		if c.method == "create" || c.method == "precreate" || c.method == "upload" || c.method == "filemanager" {
			n++
		}
	}
	return n
}

func executeMock(d *mockDisk, stdin io.Reader, args ...string) (string, string, error) {
	root := newRoot("test", &config{client: &http.Client{Transport: d}})
	var out, diagnostics bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&diagnostics)
	if stdin != nil {
		root.SetIn(stdin)
	}
	root.SetArgs(append([]string{"--access-token", "test-token"}, args...))
	err := root.Execute()
	return out.String(), diagnostics.String(), err
}

func localFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	name = filepath.Join(dir, name)
	if err := os.WriteFile(name, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return name
}

func assertLocal(t *testing.T, name string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(name)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("file %s = %q, %v; want %q", name, got, err, want)
	}
}

func TestTransferDirections(t *testing.T) {
	for _, verb := range []string{"cp", "mv", "sync"} {
		for _, direction := range []string{"upload", "download", "copy"} {
			t.Run(verb+"/"+direction, func(t *testing.T) {
				d := newMockDisk()
				content := []byte("hello 网盘\n")
				local := localFile(t, "空 格.txt", content)
				source, dest := local, "bd://new/nested/目标.txt"
				remoteSource := "/src/空 格.txt"
				localDest := filepath.Join(t.TempDir(), "new", "目标.txt")
				if direction != "upload" {
					d.put(remoteSource, content, false)
					source = "bd://" + strings.TrimPrefix(remoteSource, "/")
				}
				if direction == "download" {
					dest = localDest
				}
				if verb == "sync" {
					if direction == "upload" {
						source = filepath.Dir(local)
					} else {
						source = "bd://src/"
					}
					if direction == "download" {
						dest = filepath.Dir(localDest)
						localDest = filepath.Join(dest, "空 格.txt")
					} else {
						dest = "bd://new/"
					}
				}
				out, _, err := executeMock(d, nil, verb, source, dest, "--no-progress", "--page-size", "1")
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(out, ": ") {
					t.Fatalf("no action: %q", out)
				}
				if direction == "download" {
					assertLocal(t, localDest, content)
				} else {
					remote := "/new/nested/目标.txt"
					if verb == "sync" {
						remote = "/new/空 格.txt"
					}
					if !bytes.Equal(d.data[d.files[remote].ID], content) {
						t.Fatalf("unexpected remote file at %s", remote)
					}
				}
				if verb == "mv" {
					if direction == "upload" {
						if _, err := os.Stat(local); !errors.Is(err, os.ErrNotExist) {
							t.Fatalf("source still exists: %v", err)
						}
					} else if _, ok := d.files[remoteSource]; ok {
						t.Fatal("remote source still exists")
					}
				}
				if verb == "sync" {
					before := d.writes()
					out, _, err = executeMock(d, nil, verb, source, dest, "--no-progress")
					if err != nil || out != "" || d.writes() != before {
						t.Fatalf("second sync not empty: %q %v", out, err)
					}
				}
			})
		}
	}
}

func TestRecursiveFiltersAndPagination(t *testing.T) {
	d := newMockDisk()
	for _, name := range []string{"/src/a.txt", "/src/b.txt", "/src/sub/好.txt", "/src/sub/no.log"} {
		d.put(name, []byte(name), false)
	}
	d.put("/src/empty", nil, true)
	out, _, err := executeMock(d, nil, "cp", "bd://src/", "bd://dest/", "--recursive", "--page-size", "1",
		"--exclude", "*", "--include", "*.txt", "--exclude", "b.*")
	if err != nil {
		t.Fatal(err)
	}
	if len(strings.Split(strings.TrimSpace(out), "\n")) != 2 {
		t.Fatalf("unexpected actions %q", out)
	}
	for _, name := range []string{"/dest/a.txt", "/dest/sub/好.txt"} {
		if _, ok := d.files[name]; !ok {
			t.Fatalf("missing %s", name)
		}
	}
	for _, name := range []string{"/dest/b.txt", "/dest/sub/no.log", "/dest/empty"} {
		if _, ok := d.files[name]; ok {
			t.Fatalf("unexpected %s", name)
		}
	}
	out, _, err = executeMock(d, nil, "ls", "bd://src", "--recursive", "--human-readable", "--summarize", "--page-size", "1")
	if err != nil || !strings.Contains(out, "Total Objects: 4") || !strings.Contains(out, "src/sub/好.txt") {
		t.Fatalf("ls: %q %v", out, err)
	}
}

type panicReader struct{}

func (panicReader) Read([]byte) (int, error) { panic("dryrun must not read stdin") }

func TestDryrunDoesNotWrite(t *testing.T) {
	for _, verb := range []string{"cp", "mv", "rm", "sync"} {
		t.Run(verb, func(t *testing.T) {
			d := newMockDisk()
			d.put("/source/a", []byte("a"), false)
			local := t.TempDir()
			args := []string{verb, "bd://source/", filepath.Join(local, "absent"), "--dryrun"}
			if verb == "rm" {
				args = []string{verb, "bd://source/", "--dryrun"}
			}
			if verb != "sync" {
				args = append(args, "--recursive")
			}
			out, _, err := executeMock(d, panicReader{}, args...)
			if err != nil || !strings.Contains(out, "(dryrun)") || d.writes() != 0 {
				t.Fatalf("dryrun %q %v writes=%d", out, err, d.writes())
			}
			files, _ := os.ReadDir(local)
			if len(files) != 0 {
				t.Fatal("dryrun created local files")
			}
		})
	}
	d := newMockDisk()
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	_, _, err := executeMock(d, panicReader{}, "cp", "-", "bd://absent/file", "--expected-size", "100", "--dryrun")
	files, _ := os.ReadDir(tmp)
	if err != nil || d.writes() != 0 || len(files) != 0 {
		t.Fatalf("stdin dryrun wrote: %v", err)
	}
	local := localFile(t, "upload", []byte("contents"))
	_, _, err = executeMock(d, nil, "cp", local, "bd://absent/file", "--dryrun")
	if err != nil || d.writes() != 0 {
		t.Fatalf("upload dryrun wrote: %v", err)
	}
}

func TestFailedMoveRetainsSourceAndDestination(t *testing.T) {
	for _, direction := range []string{"upload", "copy", "download"} {
		t.Run(direction, func(t *testing.T) {
			d := newMockDisk()
			d.put("/src/a", []byte("source"), false)
			local := localFile(t, "a", []byte("original"))
			source, dest := "bd://src/a", "bd://dst/a"
			switch direction {
			case "upload":
				source = local
				d.fail = func(c diskCall) string {
					if c.method == "create" && c.form.Get("isdir") == "0" {
						return `{"errno":-1}`
					}
					return ""
				}
			case "copy":
				d.fail = func(c diskCall) string {
					if c.method == "filemanager" {
						return `{"errno":0,"info":[{"errno":-9}]}`
					}
					return ""
				}
			case "download":
				dest = local
				d.content = func(b []byte) []byte { return b[:2] }
			}
			_, _, err := executeMock(d, nil, "mv", source, dest)
			if err == nil {
				t.Fatal("expected failure")
			}
			assertLocal(t, local, []byte("original"))
			if _, exists := d.files["/src/a"]; !exists {
				t.Fatal("remote source deleted")
			}
			files, _ := os.ReadDir(filepath.Dir(local))
			if len(files) != 1 {
				t.Fatal("download left a temporary file")
			}
			for _, call := range d.calls {
				if call.query.Get("opera") == "delete" {
					t.Fatal("failed move attempted delete")
				}
			}
		})
	}
}

func TestSyncDeleteSafety(t *testing.T) {
	for _, scenario := range []string{"success", "scan-failure", "transfer-failure", "skipped"} {
		t.Run(scenario, func(t *testing.T) {
			d := newMockDisk()
			d.put("/src/new", []byte("new"), false)
			d.put("/dst/old", []byte("old"), false)
			d.put("/dst/keep.log", []byte("keep"), false)
			args := []string{"sync", "bd://src", "bd://dst", "--delete", "--exclude", "*.log"}
			d.fail = func(c diskCall) string {
				if scenario == "scan-failure" && c.method == "list" && c.query.Get("dir") == "/src" {
					return `{"errno":0}`
				}
				if scenario == "transfer-failure" && c.query.Get("opera") == "copy" {
					return `{"errno":0,"taskid":1}`
				}
				return ""
			}
			if scenario == "skipped" {
				d.put("/dst/new", []byte("existing"), false)
				args = append(args, "--no-overwrite")
			}
			_, _, err := executeMock(d, nil, args...)
			_, oldExists := d.files["/dst/old"]
			if scenario == "success" {
				if err != nil || oldExists {
					t.Fatalf("cleanup didn't run: %v", err)
				}
			} else if err == nil || !oldExists {
				t.Fatalf("unsafe cleanup: %v", err)
			}
			if _, ok := d.files["/dst/keep.log"]; !ok {
				t.Fatal("excluded destination deleted")
			}
			if scenario == "scan-failure" && d.writes() != 0 {
				t.Fatal("incomplete scan performed writes")
			}
		})
	}
}

func TestStreamsAndMultipart(t *testing.T) {
	for _, size := range []int{0, 17, chunkSize + 3} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			d := newMockDisk()
			data := bytes.Repeat([]byte{'x'}, size)
			out, _, err := executeMock(d, bytes.NewReader(data), "cp", "-", "bd://stream", "--expected-size", "1", "--quiet")
			if err != nil || out != "" || !bytes.Equal(d.data[d.files["/stream"].ID], data) {
				t.Fatalf("stdin upload: %v %q", err, out)
			}
			out, _, err = executeMock(d, nil, "cp", "bd://stream", "-", "--no-progress")
			if err != nil || !bytes.Equal([]byte(out), data) {
				t.Fatalf("stdout download differs: %v", err)
			}
		})
	}
}

func TestReadableTransferProgress(t *testing.T) {
	for _, multiline := range []bool{false, true} {
		name := "single-line"
		if multiline {
			name = "multiline"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				root := New("test")
				var out, diagnostics bytes.Buffer
				root.SetOut(&out)
				root.SetErr(&diagnostics)
				o := &fileOptions{progressFrequency: 1, progressMultiline: multiline}
				p := newProgress(root, o, 2<<30, "课程 01.mp4")
				time.Sleep(2 * time.Second)
				p.add(1 << 30)
				want := `课程 01.mp4: 50.0% | 1.0 GiB/2.0 GiB | 512.0 MiB/s | ETA 2s`
				if !strings.Contains(diagnostics.String(), want) {
					t.Fatalf("progress = %q, want %q", diagnostics.String(), want)
				}
				previous := diagnostics.String()
				p.add(1 << 30)
				if diagnostics.String() != previous {
					t.Fatal("progress ignored the update frequency")
				}
				p.complete()
				p.finish()
				want = `课程 01.mp4: 100.0% | 2.0 GiB/2.0 GiB | 1.0 GiB/s | ETA 0s`
				if !strings.Contains(diagnostics.String(), want) || !strings.HasSuffix(diagnostics.String(), "\n") {
					t.Fatalf("final progress = %q", diagnostics.String())
				}
				if out.Len() != 0 {
					t.Fatalf("progress polluted stdout: %q", out.String())
				}
				if multiline && (strings.Contains(diagnostics.String(), "\r") || strings.Count(diagnostics.String(), "\n") != 2) {
					t.Fatalf("multiline progress = %q", diagnostics.String())
				}
			})
		})
	}
}

func TestSingleFileProgressHasNoFilename(t *testing.T) {
	for _, mode := range []string{"upload", "download", "stdin", "stdout", "filtered-sync"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				d := newMockDisk()
				d.put("/src/课程 01.mp4", []byte("contents"), false)
				d.put("/src/excluded.mp4", []byte("excluded"), false)
				local := localFile(t, "课程 01.mp4", []byte("contents"))
				cfg := &config{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					if req.URL.Query().Get("method") == "upload" || req.URL.Hostname() == "download.baidu.com" {
						time.Sleep(2 * time.Second)
					}
					return d.RoundTrip(req)
				})}}
				root := newRoot("test", cfg)
				var diagnostics bytes.Buffer
				root.SetOut(io.Discard)
				root.SetErr(&diagnostics)
				root.SetIn(strings.NewReader("contents"))
				args := []string{"cp", local, "bd://target/课程 01.mp4"}
				switch mode {
				case "download":
					args = []string{"cp", "bd://src/课程 01.mp4", filepath.Join(filepath.Dir(local), "download.mp4")}
				case "stdin":
					args[1] = "-"
				case "stdout":
					args = []string{"cp", "bd://src/课程 01.mp4", "-"}
				case "filtered-sync":
					args = []string{"sync", "bd://src", filepath.Dir(local), "--exclude", "excluded.mp4", "--exact-timestamps"}
				}
				root.SetArgs(append(args, "--access-token", "test", "--concurrency", "4", "--progress-multiline"))
				if err := root.Execute(); err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(diagnostics.String(), "100.0%") {
					t.Fatalf("missing completed progress: %q", diagnostics.String())
				}
				for _, line := range strings.Split(strings.TrimSpace(diagnostics.String()), "\n") {
					if line == "" || line[0] < '0' || line[0] > '9' || strings.Contains(line, ".mp4") {
						t.Fatalf("single-file progress has a filename: %q", line)
					}
				}
			})
		})
	}
}

func TestProgressFilenameEscapesControls(t *testing.T) {
	root := New("test")
	var diagnostics bytes.Buffer
	root.SetErr(&diagnostics)
	p := newProgress(root, &fileOptions{progressMultiline: true}, 100, "课程\x1b[31m\n.mp4")
	p.write(time.Now())
	if !strings.HasPrefix(diagnostics.String(), `课程\x1b[31m\n.mp4: 0.0%`) {
		t.Fatalf("unsafe or quoted filename: %q", diagnostics.String())
	}
}

func TestProgressFailureAndSuppression(t *testing.T) {
	for _, tc := range []struct {
		name    string
		options fileOptions
		total   int64
		written int64
		want    string
	}{
		{"partial", fileOptions{}, 100, 25, "25.0%"},
		{"empty", fileOptions{}, 0, 0, "100.0%"},
		{"no-progress", fileOptions{noProgress: true}, 100, 100, ""},
		{"quiet", fileOptions{quiet: true}, 100, 100, ""},
		{"errors-only", fileOptions{onlyErrors: true}, 100, 100, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				root := New("test")
				var diagnostics bytes.Buffer
				root.SetErr(&diagnostics)
				tc.options.progressFrequency = 1
				p := newProgress(root, &tc.options, tc.total, "file")
				time.Sleep(time.Second)
				p.add(tc.written)
				if tc.name == "empty" {
					p.complete()
				}
				p.finish() // Also runs after a failed or canceled transfer.
				got := diagnostics.String()
				if tc.want == "" {
					if got != "" {
						t.Fatalf("progress was not suppressed: %q", got)
					}
				} else if !strings.Contains(got, tc.want) || strings.Contains(got, "NaN") || strings.Contains(got, "Inf") {
					t.Fatalf("progress = %q, want %q", got, tc.want)
				}
				if tc.name == "partial" && strings.Contains(got, "100.0%") {
					t.Fatalf("failed transfer was marked complete: %q", got)
				}
			})
		})
	}
}

func TestProgressRollingSpeedAndConfirmation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var diagnostics bytes.Buffer
		root := New("test")
		root.SetErr(&diagnostics)
		p := newProgress(root, &fileOptions{progressFrequency: 1, progressMultiline: true}, 1<<20, "file")
		time.Sleep(time.Second)
		p.add(1 << 20)
		if strings.Contains(diagnostics.String(), "100.0%") || !strings.Contains(diagnostics.String(), "verifying") {
			t.Fatal("unconfirmed bytes were displayed as complete")
		}
		time.Sleep(6 * time.Second)
		p.add(0)
		if !strings.Contains(diagnostics.String(), "0.0 Bytes/s") {
			t.Fatalf("stalled speed did not decay: %s", diagnostics.String())
		}
		p.add(-(1 << 20))
		p.add(1 << 20)
		p.complete()
		p.finish()
		if strings.Count(diagnostics.String(), "100.0%") != 1 || p.completed != 1<<20 {
			t.Fatalf("retry counted twice: %s", diagnostics.String())
		}
	})
}

func TestUploadRequestFilenames(t *testing.T) {
	for _, tc := range []struct {
		name, dest, target string
		stdin, recursive   bool
	}{
		{"file-to-directory", "bd://pi-tutorial/", "/pi-tutorial/课程 01.mp4", false, false},
		{"recursive", "bd://pi-tutorial", "/pi-tutorial/课程 01.mp4", false, true},
		{"explicit-name", "bd://pi-tutorial/renamed.mp4", "/pi-tutorial/renamed.mp4", false, false},
		{"stdin", "bd://pi-tutorial/课程 01.mp4", "/pi-tutorial/课程 01.mp4", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newMockDisk()
			data := []byte("video contents")
			local := localFile(t, "课程 01.mp4", data)
			if tc.stdin {
				local = "-"
			} else if tc.recursive {
				local = filepath.Dir(local)
			}
			parts := 0
			root := newRoot("test", &config{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				resp, err := d.RoundTrip(req)
				if req.URL.Query().Get("method") == "upload" && err == nil {
					parts++
					got := req.MultipartForm.File["file"][0].Filename
					if got != path.Base(tc.target) || req.URL.Query().Get("path") != tc.target {
						t.Errorf("multipart filename/path = %q / %q, want %q", got, req.URL.Query().Get("path"), tc.target)
					}
				}
				return resp, err
			})}})
			root.SetIn(bytes.NewReader(data))
			root.SetOut(io.Discard)
			root.SetErr(io.Discard)
			args := []string{"--access-token", "test-token", "cp", local, tc.dest}
			if tc.recursive {
				args = append(args, "--recursive")
			}
			root.SetArgs(args)
			if err := root.Execute(); err != nil {
				t.Fatal(err)
			}
			if parts != 1 || !bytes.Equal(d.data[d.files[tc.target].ID], data) {
				t.Fatalf("upload did not preserve destination/content: parts=%d files=%v", parts, d.files)
			}
			for _, c := range d.calls {
				if c.method == "precreate" || (c.method == "create" && c.form.Get("isdir") == "0") {
					if c.form.Get("path") != tc.target || c.form.Get("rtype") != "3" {
						t.Fatalf("%s changed the requested name or overwrite policy: %v", c.method, c.form)
					}
				}
			}
		})
	}
}

func TestStdinCancellationWhileBlocked(t *testing.T) {
	tmp := t.TempDir()
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(key, tmp)
	}
	synctest.Test(t, func(t *testing.T) {
		reader, writer := io.Pipe()
		defer reader.Close()
		defer writer.Close()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		d := newMockDisk()
		root := newRoot("test", &config{client: &http.Client{Transport: d}})
		root.SetIn(reader)
		root.SetOut(io.Discard)
		root.SetErr(io.Discard)
		root.SetArgs([]string{"--access-token", "test-token", "cp", "-", "bd://stream"})
		done := make(chan error, 1)
		go func() { done <- root.ExecuteContext(ctx) }()
		synctest.Wait()
		files, err := os.ReadDir(tmp)
		if err != nil || len(files) != 1 {
			t.Fatalf("expected stdin spool before cancellation: %v %v", files, err)
		}
		cancel()
		synctest.Wait()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) || ExitCode(err) != 130 {
				t.Fatalf("canceled stdin upload: %v (exit %d)", err, ExitCode(err))
			}
		default:
			t.Fatal("stdin upload remained blocked after cancellation")
		}
		files, err = os.ReadDir(tmp)
		if err != nil || len(files) != 0 || d.writes() != 0 {
			t.Fatalf("cancellation cleanup: files=%v err=%v writes=%d", files, err, d.writes())
		}
	})
}

func TestCopyStdinStopsCancellationHook(t *testing.T) {
	for _, readErr := range []error{nil, errors.New("broken stdin")} {
		name := "eof"
		if readErr != nil {
			name = "read-error"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				reader, writer := io.Pipe()
				defer reader.Close()
				defer writer.Close()
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				go func() {
					_, _ = writer.Write([]byte("input"))
					_ = writer.CloseWithError(readErr)
				}()
				var dst bytes.Buffer
				if err := copyStdin(ctx, &dst, reader); !errors.Is(err, readErr) || dst.String() != "input" {
					t.Fatalf("stdin completion: data=%q err=%v", dst.String(), err)
				}
				cancel()
				synctest.Wait()
				want := readErr
				if want == nil {
					want = io.EOF
				}
				if _, err := reader.Read(make([]byte, 1)); !errors.Is(err, want) {
					t.Fatalf("completed copy closed its input on later cancellation: %v", err)
				}
			})
		})
	}
}

func TestNoOverwriteAndTrailingSlash(t *testing.T) {
	d := newMockDisk()
	d.put("/src/a", []byte("new"), false)
	d.put("/a", []byte("old"), false)
	out, _, err := executeMock(d, nil, "mv", "bd://src/a", "bd://", "--no-overwrite")
	if ExitCode(err) != 2 || out != "" || d.writes() != 0 {
		t.Fatalf("no overwrite: %v %q", err, out)
	}
	if _, exists := d.files["/src/a"]; !exists {
		t.Fatal("skipped move removed source")
	}
	_, _, err = executeMock(d, nil, "cp", "bd://src/a", "bd://")
	if err != nil || string(d.data[d.files["/a"].ID]) != "new" {
		t.Fatalf("root directory destination: %v", err)
	}
	local := localFile(t, "a", []byte("original"))
	_, _, err = executeMock(d, nil, "cp", "bd://a", local, "--no-overwrite")
	if ExitCode(err) != 2 {
		t.Fatalf("expected local skip, got %v", err)
	}
	assertLocal(t, local, []byte("original"))
	for _, separator := range []string{"/", string(filepath.Separator)} {
		dest := filepath.Join(t.TempDir(), "new") + separator
		_, _, err := executeMock(d, nil, "cp", "bd://src/a", dest)
		if err != nil {
			t.Fatal(err)
		}
		assertLocal(t, filepath.Join(dest, "a"), []byte("new"))
	}
}

func TestInvalidUsageBeforeHTTP(t *testing.T) {
	for _, args := range [][]string{
		{"cp"}, {"cp", "a", "b"}, {"cp", "a", "bd://../escape"},
		{"cp", "a", "bd://x", "--acl", "public-read"}, {"cp", "a", "bd://x", "--page-size", "0"},
		{"cp", "a", "bd://x", "--case-conflict", "bad"}, {"rm", "/local"},
		{"cp", "a", "bd://x", "--follow-symlinks", "--no-follow-symlinks"},
		{"cp", "a", "bd://x", "--expected-size", "1"},
		{"cp", "a", "bd://x", "--concurrency", "0"},
		{"mv", "a", "bd://x", "--concurrency", "-1"},
		{"sync", "a", "bd://x", "--concurrency", "invalid"},
		{"cp", "a", "bd://x", "--part-concurrency", "0"},
		{"mv", "a", "bd://x", "--part-concurrency", "-1"},
		{"sync", "a", "bd://x", "--part-concurrency", "33"},
		{"cp", "-", "bd://x", "--recursive"}, {"mv", "-", "bd://x"},
		{"mb", "bd://x"}, {"rb", "bd://x"}, {"presign", "bd://x"}, {"website", "bd://x"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			d := newMockDisk()
			_, _, err := executeMock(d, nil, args...)
			if ExitCode(err) != 252 || len(d.calls) != 0 {
				t.Fatalf("err=%v code=%d calls=%d", err, ExitCode(err), len(d.calls))
			}
		})
	}
}

func TestConcurrentTransfers(t *testing.T) {
	for _, direction := range []string{"upload", "download"} {
		for _, verb := range []string{"cp", "mv", "sync"} {
			for _, concurrency := range []int{0, 1, 3, 10} {
				t.Run(fmt.Sprintf("%s/%s/%d", direction, verb, concurrency), func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						d := newMockDisk()
						local := filepath.Dir(localFile(t, "0.txt", []byte("file-0")))
						for i := 1; i < 5; i++ {
							if err := os.WriteFile(filepath.Join(local, fmt.Sprintf("%d.txt", i)), []byte(fmt.Sprintf("file-%d", i)), 0o600); err != nil {
								t.Fatal(err)
							}
						}
						source, dest := local, "bd://dst/nested/"
						if direction == "download" {
							for i := range 5 {
								d.put(fmt.Sprintf("/src/%d.txt", i), []byte(fmt.Sprintf("file-%d", i)), false)
							}
							source, dest = "bd://src/", filepath.Join(t.TempDir(), "nested")
						}
						var mu sync.Mutex
						active, peak := 0, 0
						transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
							if req.URL.Query().Get("method") == "upload" || req.URL.Hostname() == "download.baidu.com" {
								mu.Lock()
								active++
								peak = max(peak, active)
								mu.Unlock()
								defer func() {
									mu.Lock()
									active--
									mu.Unlock()
								}()
								time.Sleep(2 * time.Second) // Synthetic time; lets all workers overlap.
							}
							return d.RoundTrip(req)
						})
						root := newRoot("test", &config{client: &http.Client{Transport: transport}})
						var out, diagnostics bytes.Buffer
						root.SetOut(&out)
						root.SetErr(&diagnostics)
						args := []string{"--access-token", "test", verb, source, dest}
						if verb != "sync" {
							args = append(args, "--recursive")
						}
						if concurrency > 0 {
							args = append(args, "--concurrency", strconv.Itoa(concurrency))
						}
						root.SetArgs(args)
						if err := root.Execute(); err != nil {
							t.Fatal(err)
						}
						if peak != min(5, max(1, concurrency)) || active != 0 {
							t.Fatalf("peak=%d active=%d concurrency=%d", peak, active, concurrency)
						}
						if strings.Count(out.String(), "\n") != 5 {
							t.Fatalf("interleaved action output: %q", out.String())
						}
						for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
							if strings.Count(line, " to ") != 1 {
								t.Fatalf("broken action line: %q", line)
							}
						}
						if concurrency > 1 {
							if strings.Contains(diagnostics.String(), "\r") || strings.Count(diagnostics.String(), "100.0%") != 5 {
								t.Fatalf("expected one complete progress line per file: %q", diagnostics.String())
							}
						}
						for i := range 5 {
							filename := fmt.Sprintf("%d.txt", i)
							if !strings.Contains(diagnostics.String(), filename+": ") ||
								strings.Contains(diagnostics.String(), `"`+filename+`"`) {
								t.Fatalf("missing or quoted multi-file label: %q", diagnostics.String())
							}
							content := []byte(fmt.Sprintf("file-%d", i))
							if direction == "download" {
								assertLocal(t, filepath.Join(dest, filename), content)
								if _, exists := d.files["/src/"+filename]; exists == (verb == "mv") {
									t.Fatalf("unexpected source existence for %s", filename)
								}
							} else {
								if !bytes.Equal(d.data[d.files["/dst/nested/"+filename].ID], content) {
									t.Fatalf("incorrect upload %s", filename)
								}
								_, err := os.Stat(filepath.Join(local, filename))
								if (verb == "mv" && !errors.Is(err, os.ErrNotExist)) || (verb != "mv" && err != nil) {
									t.Fatalf("unexpected source state: %v", err)
								}
							}
						}
						if direction == "download" {
							entries, err := os.ReadDir(dest)
							if err != nil || len(entries) != 5 {
								t.Fatalf("temporary files remain: %v %v", entries, err)
							}
						}
					})
				})
			}
		}
	}
}

func TestConcurrentFilesAndUploadParts(t *testing.T) {
	for _, verb := range []string{"cp", "mv", "sync", "stdin"} {
		t.Run(verb, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				d := newMockDisk()
				data := bytes.Repeat([]byte("x"), chunkSize+17)
				local := localFile(t, "a", data)
				dir := filepath.Dir(local)
				if err := os.WriteFile(filepath.Join(dir, "b"), data, 0o600); err != nil {
					t.Fatal(err)
				}
				var mu sync.Mutex
				active, peak := 0, 0
				routes := map[string]int{}
				transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
					q := req.URL.Query()
					if q.Get("method") == "locateupload" {
						mu.Lock()
						routes[q.Get("path")]++
						mu.Unlock()
					}
					if q.Get("method") == "upload" {
						mu.Lock()
						active++
						peak = max(peak, active)
						mu.Unlock()
						defer func() {
							mu.Lock()
							active--
							mu.Unlock()
						}()
						time.Sleep(2 * time.Second)
					}
					return d.RoundTrip(req)
				})
				root := newRoot("test", &config{client: &http.Client{Transport: transport}})
				var out, diagnostics bytes.Buffer
				root.SetOut(&out)
				root.SetErr(&diagnostics)
				args := []string{verb, dir, "bd://dest/"}
				files := 2
				if verb == "stdin" {
					root.SetIn(bytes.NewReader(data))
					args = []string{"cp", "-", "bd://dest/a"}
					files = 1
				} else if verb != "sync" {
					args = append(args, "--recursive")
				}
				root.SetArgs(append(args, "--access-token", "test", "--concurrency", "2", "--part-concurrency", "3"))
				if err := root.Execute(); err != nil {
					t.Fatal(err)
				}
				if active != 0 || peak != files*2 || len(routes) != files {
					t.Fatalf("active=%d peak=%d routes=%v", active, peak, routes)
				}
				for name, calls := range routes {
					if calls != 1 || !bytes.Equal(d.data[d.files[name].ID], data) {
						t.Fatalf("wrong file/routing: %s calls=%d", name, calls)
					}
				}
				if strings.Count(out.String(), "\n") != files || strings.Count(diagnostics.String(), "| ETA 0s") != files {
					t.Fatalf("invalid parallel output: %q / %q", out.String(), diagnostics.String())
				}
				if verb == "mv" {
					if _, err := os.Stat(local); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("successful move did not remove source: %v", err)
					}
				}
			})
		})
	}
}

func TestFailedPartPreservesSourceAndSuppressesCleanup(t *testing.T) {
	for _, verb := range []string{"mv", "sync"} {
		t.Run(verb, func(t *testing.T) {
			d := newMockDisk()
			d.put("/dest/old", []byte("keep"), false)
			local := localFile(t, "source", []byte("keep source"))
			d.fail = func(c diskCall) string {
				if c.method == "upload" {
					return `{"error_code":31364}`
				}
				return ""
			}
			args := []string{verb, filepath.Dir(local), "bd://dest/", "--part-concurrency", "3"}
			if verb == "sync" {
				args = append(args, "--delete")
			} else {
				args = append(args, "--recursive")
			}
			if _, _, err := executeMock(d, nil, args...); err == nil {
				t.Fatal("failed part succeeded")
			}
			assertLocal(t, local, []byte("keep source"))
			if _, ok := d.files["/dest/old"]; !ok {
				t.Fatal("failed upload deleted destination")
			}
			for _, call := range d.calls {
				if call.method == "create" || call.method == "filemanager" {
					t.Fatalf("failed part triggered %s", call.method)
				}
			}
		})
	}
}

func TestConcurrentCancellation(t *testing.T) {
	for _, direction := range []string{"upload", "download"} {
		t.Run(direction, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				d := newMockDisk()
				local := filepath.Dir(localFile(t, "0", []byte("keep")))
				for i := 1; i < 5; i++ {
					if err := os.WriteFile(filepath.Join(local, strconv.Itoa(i)), []byte("keep"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				args := []string{"mv", local, "bd://dst/", "--recursive"}
				if direction == "download" {
					for i := range 5 {
						d.put("/src/"+strconv.Itoa(i), []byte("new"), false)
					}
					local = filepath.Dir(localFile(t, "extra", []byte("keep")))
					args = []string{"sync", "bd://src/", local, "--delete"}
				}
				active, started := 0, 0
				var mu sync.Mutex
				transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
					if req.URL.Query().Get("method") == "upload" || req.URL.Hostname() == "download.baidu.com" {
						mu.Lock()
						active++
						started++
						mu.Unlock()
						<-req.Context().Done()
						mu.Lock()
						active--
						mu.Unlock()
						return nil, req.Context().Err()
					}
					return d.RoundTrip(req)
				})
				root := newRoot("test", &config{client: &http.Client{Transport: transport}})
				var out, diagnostics bytes.Buffer
				root.SetOut(&out)
				root.SetErr(&diagnostics)
				root.SetArgs(append(args, "--access-token", "test", "--concurrency", "3"))
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan error, 1)
				go func() { done <- root.ExecuteContext(ctx) }()
				synctest.Wait()
				mu.Lock()
				count := active
				mu.Unlock()
				if count != 3 {
					t.Fatalf("active transfers=%d, want 3", count)
				}
				cancel()
				if err := <-done; ExitCode(err) != 130 {
					t.Fatalf("cancellation: %v", err)
				}
				if active != 0 || started != 3 || out.Len() != 0 {
					t.Fatalf("unfinished/queued work: active=%d started=%d output=%q", active, started, out.String())
				}
				entries, err := os.ReadDir(local)
				wantFiles := 5
				if direction == "download" {
					wantFiles = 1
					assertLocal(t, filepath.Join(local, "extra"), []byte("keep"))
				}
				if err != nil || len(entries) != wantFiles {
					t.Fatalf("source loss or spool leak: %v %v", entries, err)
				}
				for _, call := range d.calls {
					if call.method == "create" && call.form.Get("isdir") == "0" || call.query.Get("opera") == "delete" {
						t.Fatalf("canceled transfer committed or deleted a file: %+v", call)
					}
				}
			})
		})
	}
}

func TestConcurrentSyncCleanup(t *testing.T) {
	for _, scenario := range []string{"success", "failure", "skip"} {
		t.Run(scenario, func(t *testing.T) {
			d := newMockDisk()
			d.put("/src/a", []byte("a"), false)
			d.put("/src/b", []byte("b"), false)
			d.put("/dst/extra", []byte("keep"), false)
			d.fail = func(c diskCall) string {
				if c.query.Get("opera") == "copy" && strings.Contains(c.form.Get("filelist"), "/src/a") {
					if scenario == "failure" {
						return `{"errno":-1}`
					}
					if scenario == "skip" {
						return `{"errno":-8}`
					}
				}
				if c.query.Get("opera") == "delete" {
					if _, ok := d.files["/dst/a"]; !ok {
						t.Error("cleanup started before a finished")
					}
					if _, ok := d.files["/dst/b"]; !ok {
						t.Error("cleanup started before b finished")
					}
				}
				return ""
			}
			_, diagnostics, err := executeMock(d, nil, "sync", "bd://src", "bd://dst", "--delete", "--concurrency", "3", "--no-overwrite")
			wantCode := map[string]int{"success": 0, "failure": 1, "skip": 2}[scenario]
			_, extraExists := d.files["/dst/extra"]
			if ExitCode(err) != wantCode || extraExists != (scenario != "success") {
				t.Fatalf("cleanup: extra=%v err=%v diagnostics=%q", extraExists, err, diagnostics)
			}
			if _, ok := d.files["/dst/b"]; !ok {
				t.Fatal("unrelated transfer did not finish")
			}
		})
	}
}

func TestConcurrentDryrunAndCaseCollisions(t *testing.T) {
	for _, dryrun := range []bool{false, true} {
		t.Run(fmt.Sprintf("dryrun=%v", dryrun), func(t *testing.T) {
			d := newMockDisk()
			d.put("/src/A", []byte("A"), false)
			d.put("/src/a", []byte("a"), false)
			target := t.TempDir()
			args := []string{"cp", "bd://src/", target, "--recursive"}
			if dryrun {
				args = append(args, "--dryrun")
			}
			serial, _, err := executeMock(d, nil, args...)
			if err != nil {
				t.Fatal(err)
			}
			parallel, diagnostics, err := executeMock(d, nil, append(args, "--concurrency", "3")...)
			if err != nil || serial != parallel {
				t.Fatalf("serial=%q parallel=%q err=%v", serial, parallel, err)
			}
			if dryrun {
				entries, err := os.ReadDir(target)
				if err != nil || len(entries) != 0 || d.writes() != 0 {
					t.Fatalf("dryrun wrote files: %v %v", entries, err)
				}
			} else {
				if !strings.Contains(diagnostics, "require serial transfers") {
					t.Fatalf("missing collision warning: %q", diagnostics)
				}
				assertLocal(t, filepath.Join(target, "a"), []byte("a"))
			}
		})
	}
}

func TestCredentialsCancellationAndRedaction(t *testing.T) {
	t.Setenv("BAIDU_ACCESS_TOKEN", "")
	root := New("test")
	root.SetArgs([]string{"ls", "--token-file", filepath.Join(t.TempDir(), "missing")})
	if err := root.Execute(); ExitCode(err) != 253 {
		t.Fatalf("credential exit: %v", err)
	}
	root = newRoot("test", &config{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return nil, &url.Error{Op: "Get", URL: req.URL.String(), Err: errors.New("failure")}
	})}})
	root.SetArgs([]string{"ls", "--access-token", "SUPERSECRET"})
	err := root.Execute()
	if err == nil || strings.Contains(err.Error(), "SUPERSECRET") || strings.Contains(err.Error(), "https") {
		t.Fatalf("leaked credentials: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	root = newRoot("test", &config{client: &http.Client{Transport: newMockDisk()}})
	root.SetArgs([]string{"ls", "--access-token", "test"})
	if err := root.ExecuteContext(ctx); ExitCode(err) != 130 {
		t.Fatalf("cancel code: %v", err)
	}
}

func TestFiltersAndSyncComparison(t *testing.T) {
	for _, tc := range []struct {
		pattern, name string
		want          bool
	}{
		{"*.txt", "sub/好.txt", true}, {"a?c", "abc", true}, {"a?c", "a/c", true},
		{"[!a-c]*", "z", true}, {"[!a-c]*", "b", false}, {"[[]", "[", true},
		{"x[", "x[", true}, {"file[0-9]", "file8", true},
	} {
		re, err := globRegex(tc.pattern)
		if err != nil || re.MatchString(tc.name) != tc.want {
			t.Errorf("glob %q name %q: %v", tc.pattern, tc.name, err)
		}
	}
	var rules []filterRule
	include := filterFlag{&rules, true}
	exclude := filterFlag{&rules, false}
	_ = include.Set("*.txt")
	if !included(rules, "other.bin") {
		t.Fatal("include alone must not exclude")
	}
	_ = exclude.Set("*")
	_ = include.Set("a*")
	if included(rules, "b") || !included(rules, "a") {
		t.Fatal("filter order lost")
	}
	for _, download := range []bool{false, true} {
		for _, delta := range []int64{-1, 0, 1} {
			src := fileEntry{size: 1, mtime: time.Unix(100, 0), loc: location{remote: download}}
			dst := fileEntry{size: 1, mtime: time.Unix(100+delta, 0), loc: location{remote: !download}}
			want := delta < 0
			if download {
				want = delta > 0
			}
			if needsSync(src, dst, &fileOptions{}) != want {
				t.Fatalf("sync download=%v delta=%d", download, delta)
			}
			if needsSync(src, dst, &fileOptions{sizeOnly: true}) {
				t.Fatal("size-only compared timestamps")
			}
			if download && needsSync(src, dst, &fileOptions{exactTimestamps: true}) != (delta != 0) {
				t.Fatal("exact timestamps comparison")
			}
		}
	}
}

func TestSymlinksTraversalAndCaseConflicts(t *testing.T) {
	t.Run("loop", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Symlink(dir, filepath.Join(dir, "loop")); err != nil {
			t.Skip(err)
		}
		d := newMockDisk()
		_, _, err := executeMock(d, nil, "cp", dir, "bd://dst", "--recursive")
		if err == nil || d.writes() != 0 {
			t.Fatalf("symlink loop: %v", err)
		}
		_, _, err = executeMock(d, nil, "cp", dir, "bd://dst", "--recursive", "--no-follow-symlinks")
		if ExitCode(err) != 2 || d.writes() != 0 {
			t.Fatalf("nofollow: %v", err)
		}
	})
	t.Run("download-escape", func(t *testing.T) {
		dir, outside := t.TempDir(), t.TempDir()
		if err := os.Symlink(outside, filepath.Join(dir, "sub")); err != nil {
			t.Skip(err)
		}
		d := newMockDisk()
		d.put("/src/sub/file", []byte("bad"), false)
		_, _, err := executeMock(d, nil, "cp", "bd://src", dir, "--recursive")
		if err == nil {
			t.Fatal("followed escaping destination symlink")
		}
		entries, _ := os.ReadDir(outside)
		if len(entries) != 0 {
			t.Fatal("wrote outside destination")
		}
	})
	for _, mode := range []string{"error", "skip", "warn", "ignore"} {
		t.Run("case-"+mode, func(t *testing.T) {
			d := newMockDisk()
			d.put("/src/A", []byte("A"), false)
			d.put("/src/a", []byte("a"), false)
			target := t.TempDir()
			out, _, err := executeMock(d, nil, "cp", "bd://src", target, "--recursive", "--case-conflict", mode)
			switch mode {
			case "error":
				if err == nil || out != "" {
					t.Fatalf("case error: %q %v", out, err)
				}
			case "skip":
				if ExitCode(err) != 2 {
					t.Fatalf("case skip: %v", err)
				}
			default:
				if err != nil {
					t.Fatal(err)
				}
			}
		})
	}
	t.Run("hostile-list", func(t *testing.T) {
		d := newMockDisk()
		d.fail = func(c diskCall) string {
			if c.method == "list" {
				return `{"errno":0,"list":[{"path":"/../escape","fs_id":3,"size":1}]}`
			}
			return ""
		}
		_, _, err := executeMock(d, nil, "ls")
		if err == nil {
			t.Fatal("accepted traversal path")
		}
	})
	t.Run("overlap", func(t *testing.T) {
		d := newMockDisk()
		d.put("/src/file", []byte("keep"), false)
		_, _, err := executeMock(d, nil, "mv", "bd://src", "bd://src/sub", "--recursive")
		if ExitCode(err) != 252 || d.writes() != 0 {
			t.Fatalf("overlap %v", err)
		}
	})
}

func TestMoveRejectsSymlinkAncestors(t *testing.T) {
	for _, recursive := range []bool{false, true} {
		t.Run(fmt.Sprintf("recursive=%v", recursive), func(t *testing.T) {
			actual := localFile(t, "file", []byte("keep"))
			base, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(base, "link")
			if err := os.Symlink(filepath.Dir(actual), link); err != nil {
				t.Skip(err)
			}
			source := filepath.Join(link, "file")
			if recursive {
				if err := os.Mkdir(filepath.Join(filepath.Dir(actual), "sub"), 0o700); err != nil {
					t.Fatal(err)
				}
				actual = filepath.Join(filepath.Dir(actual), "sub", "file")
				if err := os.WriteFile(actual, []byte("keep"), 0o600); err != nil {
					t.Fatal(err)
				}
				source = filepath.Join(link, "sub")
			}
			for _, flags := range [][]string{nil, {"--dryrun"}, {"--no-follow-symlinks"}} {
				d := newMockDisk()
				args := []string{"mv", source, "bd://dest"}
				if recursive {
					args = append(args, "--recursive")
				}
				args = append(args, flags...)
				_, _, err := executeMock(d, nil, args...)
				if err == nil || !strings.Contains(err.Error(), "directory symlink") || d.writes() != 0 {
					t.Fatalf("move through ancestor (%v): err=%v writes=%d", flags, err, d.writes())
				}
				assertLocal(t, actual, []byte("keep"))
			}
			d := newMockDisk()
			args := []string{"cp", source, "bd://dest"}
			if recursive {
				args = append(args, "--recursive")
			}
			if _, _, err := executeMock(d, nil, args...); err != nil {
				t.Fatalf("copy should still follow ancestors: %v", err)
			}
			assertLocal(t, actual, []byte("keep"))
		})
	}
}

func TestSyncDeleteCaseAliases(t *testing.T) {
	for _, relative := range []string{"Foo", "Dir/Foo"} {
		t.Run(relative, func(t *testing.T) {
			target := t.TempDir()
			local := filepath.Join(target, filepath.FromSlash(strings.ToLower(relative)))
			if err := os.MkdirAll(filepath.Dir(local), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(local, []byte("old"), 0o600); err != nil {
				t.Fatal(err)
			}
			alias := filepath.Join(target, filepath.FromSlash(relative))
			_, err := os.Stat(alias)
			caseInsensitive := err == nil
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			t.Logf("case-insensitive destination: %v", caseInsensitive)
			extra := filepath.Join(target, "zz-extra")
			if err := os.WriteFile(extra, []byte("remove"), 0o600); err != nil {
				t.Fatal(err)
			}
			d := newMockDisk()
			d.put("/src/"+relative, []byte("new contents"), false)
			args := []string{"sync", "bd://src", target, "--delete"}
			out, _, err := executeMock(d, nil, append(args, "--dryrun")...)
			if err != nil || (caseInsensitive && strings.Contains(out, "delete: "+local)) {
				t.Fatalf("unsafe preview: %q err=%v", out, err)
			}
			_, _, err = executeMock(d, nil, args...)
			if err != nil {
				t.Fatal(err)
			}
			assertLocal(t, alias, []byte("new contents"))
			if _, err := os.Stat(extra); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("extra destination was not deleted: %v", err)
			}
			if !caseInsensitive {
				if _, err := os.Stat(local); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("distinct case-sensitive destination was not deleted: %v", err)
				}
			}
			out, _, err = executeMock(d, nil, args...)
			if err != nil || out != "" {
				t.Fatalf("second sync should be empty: %q err=%v", out, err)
			}
		})
	}
}

func TestCaseConflictSkipPreservesAcceptedDirectory(t *testing.T) {
	for _, verb := range []string{"cp", "mv", "sync"} {
		t.Run(verb, func(t *testing.T) {
			d := newMockDisk()
			for _, name := range []string{"A/1", "a/2", "a/3"} {
				d.put("/src/"+name, []byte(name), false)
			}
			target := t.TempDir()
			args := []string{verb, "bd://src", target, "--case-conflict", "skip"}
			if verb != "sync" {
				args = append(args, "--recursive")
			}
			out, diagnostics, err := executeMock(d, nil, args...)
			if ExitCode(err) != 2 || strings.Count(diagnostics, "case collision") != 2 {
				t.Fatalf("case skip: out=%q diagnostics=%q err=%v", out, diagnostics, err)
			}
			assertLocal(t, filepath.Join(target, "A", "1"), []byte("A/1"))
			for _, name := range []string{"a/2", "a/3"} {
				if _, err := os.Stat(filepath.Join(target, filepath.FromSlash(name))); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("skipped %s was downloaded: %v", name, err)
				}
				if _, ok := d.files["/src/"+name]; !ok {
					t.Fatalf("skipped source %s was removed", name)
				}
			}
		})
	}
}

func TestCaseCollisionDoesNotReserveSkippedChildren(t *testing.T) {
	dir := t.TempDir()
	seen := map[string]string{}
	for _, tc := range []struct {
		name     string
		conflict bool
	}{
		{"A/first", false},
		{"a/Rejected", true},
		{"A/rejected", false},
		{"a/third", true},
	} {
		conflict, err := caseCollision(filepath.Join(dir, filepath.FromSlash(tc.name)), seen, true)
		if err != nil || (conflict != "") != tc.conflict {
			t.Fatalf("case collision for %s: conflict=%q err=%v", tc.name, conflict, err)
		}
	}
}

func TestSyncDestinationTypeConflict(t *testing.T) {
	d := newMockDisk()
	d.put("/src/a", []byte("file"), false)
	d.put("/dst/a/keep", []byte("keep"), false)
	_, _, err := executeMock(d, nil, "sync", "bd://src", "bd://dst", "--delete")
	if err == nil || d.writes() != 0 {
		t.Fatalf("sync replaced a directory: %v", err)
	}
}

func TestSyncFiltersBeforeLocalNames(t *testing.T) {
	for _, relative := range []string{"CON", "sub/CON"} {
		for _, dryrun := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/dryrun=%v", relative, dryrun), func(t *testing.T) {
				d := newMockDisk()
				d.put("/src/"+relative, []byte("excluded"), false)
				d.put("/src/good.txt", []byte("good"), false)
				target := filepath.Dir(localFile(t, "old.txt", []byte("old")))
				keep := filepath.Join(target, "keep.log")
				if err := os.WriteFile(keep, []byte("keep"), 0o600); err != nil {
					t.Fatal(err)
				}
				// Included incompatible names must still fail validation.
				_, _, err := executeMock(d, nil, "sync", "bd://src", target, "--dryrun")
				if err == nil || !strings.Contains(err.Error(), "unsafe local filename") {
					t.Fatalf("included unsafe name was accepted: %v", err)
				}
				args := []string{"sync", "bd://src", target, "--delete", "--exclude", relative, "--exclude", "*.log"}
				if dryrun {
					args = append(args, "--dryrun")
				}
				out, _, err := executeMock(d, nil, args...)
				if err != nil || !strings.Contains(out, "good.txt") || strings.Contains(out, "bd://src/"+relative) {
					t.Fatalf("excluded name blocked sync: out=%q err=%v", out, err)
				}
				assertLocal(t, keep, []byte("keep"))
				if dryrun {
					assertLocal(t, filepath.Join(target, "old.txt"), []byte("old"))
					if _, err := os.Stat(filepath.Join(target, "good.txt")); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("dryrun wrote destination: %v", err)
					}
				} else {
					assertLocal(t, filepath.Join(target, "good.txt"), []byte("good"))
					if _, err := os.Stat(filepath.Join(target, "old.txt")); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("sync cleanup did not run: %v", err)
					}
				}
			})
		}
	}
}

func TestPaginationRejectsRepeatedAndIncompletePages(t *testing.T) {
	for _, reply := range []string{
		`{"errno":0,"has_more":1,"list":[]}`,
		`{"errno":0,"list":[{"path":"/repeat","fs_id":1,"size":0,"isdir":0}]}`,
		`{"errno":0,"list":[{"path":"/file","size":0}]}`,
		`{"errno":0,"list":[{"path":"/unknown-type","fs_id":1,"size":0}]}`,
	} {
		d := newMockDisk()
		d.fail = func(diskCall) string { return reply }
		_, _, err := executeMock(d, nil, "ls", "--page-size", "1")
		if err == nil || len(d.calls) > 2 {
			t.Fatalf("unbounded or incomplete listing: %v (%d calls)", err, len(d.calls))
		}
	}
}

func TestSourceChangePreventsMoveDeletion(t *testing.T) {
	d := newMockDisk()
	local := localFile(t, "source", []byte("before"))
	d.fail = func(c diskCall) string {
		if c.method == "create" && c.form.Get("isdir") == "0" {
			if err := os.WriteFile(local, []byte("modified source"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return ""
	}
	_, _, err := executeMock(d, nil, "mv", local, "bd://target")
	if err == nil {
		t.Fatal("source change went unnoticed")
	}
	assertLocal(t, local, []byte("modified source"))
}

func TestNoOverwriteRaceRetainsLocalSource(t *testing.T) {
	d := newMockDisk()
	local := localFile(t, "source", []byte("source"))
	d.fail = func(c diskCall) string {
		if c.method == "precreate" {
			d.put("/target", []byte("racer"), false)
		}
		return ""
	}
	_, _, err := executeMock(d, nil, "mv", local, "bd://target", "--no-overwrite")
	if ExitCode(err) != 2 {
		t.Fatalf("expected race skip: %v", err)
	}
	assertLocal(t, local, []byte("source"))
	if string(d.data[d.files["/target"].ID]) != "racer" {
		t.Fatal("overwrote concurrently created destination")
	}
}

func TestStdinNoOverwriteRace(t *testing.T) {
	for _, tc := range []struct {
		name, stage string
		errno       int
		noOverwrite bool
		code        int
	}{
		{"precreate-conflict", "precreate", -8, true, 2},
		{"create-conflict", "create", -8, true, 2},
		{"other-error", "precreate", -1, true, 1},
		{"overwrite-enabled", "precreate", -8, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spool := t.TempDir()
			for _, variable := range []string{"TMPDIR", "TMP", "TEMP"} {
				t.Setenv(variable, spool)
			}
			d := newMockDisk()
			d.fail = func(c diskCall) string {
				if c.method == tc.stage {
					d.put("/target", []byte("racer"), false)
					if tc.errno != -8 || !tc.noOverwrite {
						return fmt.Sprintf(`{"errno":%d}`, tc.errno)
					}
				}
				return ""
			}
			args := []string{"cp", "-", "bd://target", "--no-progress"}
			if tc.noOverwrite {
				args = append(args, "--no-overwrite")
			}
			out, _, err := executeMock(d, strings.NewReader("stdin contents"), args...)
			if ExitCode(err) != tc.code || !errors.Is(err, netdiskError(tc.errno)) || out != "" {
				t.Errorf("stdin race: code=%d want=%d out=%q err=%v", ExitCode(err), tc.code, out, err)
			}
			if string(d.data[d.files["/target"].ID]) != "racer" {
				t.Error("overwrote concurrently created destination")
			}
			entries, err := os.ReadDir(spool)
			if err != nil || len(entries) != 0 {
				t.Fatalf("stdin spool leaked: %v %v", entries, err)
			}
		})
	}
}

func TestLocalSyncDeleteAndSymlinkSource(t *testing.T) {
	d := newMockDisk()
	d.put("/src/new", []byte("new"), false)
	target := t.TempDir()
	for _, name := range []string{"old", "keep.log"} {
		if err := os.WriteFile(filepath.Join(target, name), []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_, _, err := executeMock(d, nil, "sync", "bd://src", target, "--delete", "--exclude", "*.log")
	if err != nil {
		t.Fatal(err)
	}
	assertLocal(t, filepath.Join(target, "new"), []byte("new"))
	assertLocal(t, filepath.Join(target, "keep.log"), []byte("keep"))
	if _, err := os.Stat(filepath.Join(target, "old")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old local file not removed: %v", err)
	}

	actual := localFile(t, "actual", []byte("linked"))
	link := filepath.Join(filepath.Dir(actual), "link")
	if err := os.Symlink(actual, link); err != nil {
		t.Skip(err)
	}
	_, _, err = executeMock(d, nil, "mv", link, "bd://linked")
	if err != nil {
		t.Fatal(err)
	}
	assertLocal(t, actual, []byte("linked"))
	if _, err := os.Lstat(link); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("move did not remove source link")
	}
}

func TestDownloadIntegrityAndUntrustedURLs(t *testing.T) {
	for _, scenario := range []string{"chunked-truncated", "chunked-overlong", "bad-md5", "bad-host", "redirect"} {
		t.Run(scenario, func(t *testing.T) {
			d := newMockDisk()
			d.put("/source", []byte("source"), false)
			target := localFile(t, "target", []byte("original"))
			cfg := &config{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.Hostname() == "evil.example" {
					t.Fatal("sent credentials to untrusted host")
				}
				resp, err := d.RoundTrip(req)
				if err != nil {
					return nil, err
				}
				if req.URL.Query().Get("method") == "filemetas" && scenario == "bad-host" {
					f := d.files["/source"]
					body := fmt.Sprintf(`{"errno":0,"list":[{"fs_id":%d,"size":6,"dlink":"https://evil.example/secret"}]}`, f.ID)
					resp.Body.Close()
					return response(req, []byte(body)), nil
				}
				if req.URL.Hostname() == "download.baidu.com" {
					switch scenario {
					case "chunked-truncated":
						resp.Body.Close()
						resp.Body = io.NopCloser(strings.NewReader("so"))
						resp.ContentLength = -1
					case "chunked-overlong":
						resp.Body.Close()
						resp.Body = io.NopCloser(strings.NewReader("source!"))
						resp.ContentLength = -1
					case "bad-md5":
						resp.Header.Set("Content-MD5", "not-the-digest")
					case "redirect":
						resp.StatusCode = 302
						resp.Header.Set("Location", "https://evil.example/?secret=token")
					}
				}
				return resp, nil
			})}}
			cmd := newRoot("test", cfg)
			cmd.SetArgs([]string{"cp", "bd://source", target, "--access-token", "SECRET"})
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			err := cmd.Execute()
			if err == nil || strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "evil.example") {
				t.Fatalf("unsafe download error: %v", err)
			}
			assertLocal(t, target, []byte("original"))
		})
	}
}

func TestHelpQueryAndAuthRegression(t *testing.T) {
	root := New("test")
	names := []string{"auth", "info", "quota", "search", "docs", "images", "meta", "metas", "metas-path", "ls", "cp", "mv", "rm", "sync"}
	for _, name := range names {
		found, _, err := root.Find([]string{name})
		if err != nil || found == root {
			t.Fatalf("missing command %s", name)
		}
	}
	for _, flow := range []string{"exchange", "device-token", "refresh"} {
		t.Run(flow, func(t *testing.T) {
			tokenFile := filepath.Join(t.TempDir(), "token.json")
			requests := 0
			cfg := &config{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				requests++
				if req.URL.Hostname() != "openapi.baidu.com" ||
					req.URL.Query().Get("client_id") != "app" || req.URL.Query().Get("client_secret") != "secret" {
					t.Fatal("auth request changed")
				}
				wantGrant := map[string]string{"exchange": "authorization_code", "device-token": "device_token", "refresh": "refresh_token"}[flow]
				if req.URL.Query().Get("grant_type") != wantGrant {
					t.Fatal("auth grant changed")
				}
				return response(req, []byte(`{"access_token":"saved","refresh_token":"refresh","expires_in":3600}`)), nil
			})}}
			args := []string{"auth", flow, "--app-key", "app", "--secret-key", "secret", "--token-file", tokenFile}
			switch flow {
			case "exchange":
				args = append(args, "--code", "code", "--redirect-uri", "https://example.com/callback")
			case "device-token":
				args = append(args, "--code", "code")
			case "refresh":
				args = append(args, "--refresh-token", "refresh")
			}
			root := newRoot("test", cfg)
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetArgs(args)
			if err := root.Execute(); err != nil {
				t.Fatal(err)
			}
			token, err := loadToken(tokenFile)
			if err != nil || token.AccessToken != "saved" || requests != 1 || !json.Valid(out.Bytes()) {
				t.Fatalf("auth regression: %v %q", err, out.String())
			}
		})
	}
}
