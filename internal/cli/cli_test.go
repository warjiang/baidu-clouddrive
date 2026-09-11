package cli

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHashFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data")
	data := make([]byte, chunkSize+3)
	copy(data[chunkSize:], "abc")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	blocks, full, slice, err := hashFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 2 || full == "" || slice == "" {
		t.Fatalf("unexpected hashes: blocks=%d full=%q slice=%q", len(blocks), full, slice)
	}
}

func TestMutatingCommandNeedsConfirmation(t *testing.T) {
	cmd := New("dev")
	cmd.SetArgs([]string{"file", "delete", "--filelist", `["/x"]`})
	if err := cmd.Execute(); err == nil {
		t.Fatal("delete should require --yes")
	}
}

func TestCommandRejectsUnexpectedArgs(t *testing.T) {
	cmd := New("dev")
	cmd.SetArgs([]string{"file", "list", "unexpected"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("file list should reject positional arguments")
	}
}

func TestUploadPartRejectsNegativeSequence(t *testing.T) {
	cmd := New("dev")
	cmd.SetArgs([]string{"file", "upload-part", "--path", "/x", "--uploadid", "id", "--file", "part", "--partseq", "-1", "--yes"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("upload-part should reject a negative part sequence")
	}
}

func TestVersion(t *testing.T) {
	var output bytes.Buffer
	cmd := New("dev")
	cmd.SetOut(&output)
	cmd.SetArgs([]string{"--version"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if output.String() != "bdpan version dev\n" {
		t.Fatalf("unexpected version output: %q", output.String())
	}
}

func TestAPICommandMapsQueryAndForm(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != "pan.baidu.com" {
			t.Errorf("unexpected user agent: %q", r.Header.Get("User-Agent"))
		}
		if r.URL.Query().Get("method") != "example" || r.URL.Query().Get("access_token") != "token" {
			t.Errorf("unexpected query: %s", r.URL.RawQuery)
		}
		data, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(data))
		if form.Get("filelist") != `["/x"]` {
			t.Errorf("unexpected form: %s", data)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"errno":0}`))
	}))
	defer server.Close()

	cfg := &config{accessToken: "token", timeout: time.Second, client: server.Client()}
	cmd := newAPICommand(cfg, apiSpec{
		use: "example", method: http.MethodPost, base: server.URL, path: "/",
		query:  values("method", "example"),
		fields: []field{{name: "filelist", required: true, body: true}},
	})
	cmd.SetArgs([]string{"--filelist", `["/x"]`})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
}
