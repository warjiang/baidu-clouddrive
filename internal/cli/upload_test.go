package cli

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
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
				Body:       io.NopCloser(strings.NewReader(`{"errno":0,"return_type":2,"fs_id":1}`)),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		})},
	}
	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(&output)

	if err := uploadFile(cmd, cfg, path, "/file.txt", 1); err != nil {
		t.Fatal(err)
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want 1", requests)
	}
	if !strings.Contains(output.String(), `"return_type": 2`) {
		t.Fatalf("unexpected output: %q", output.String())
	}
}
