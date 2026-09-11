package cli

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func TestDoJSONPreservesUnknownHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"message":"bad gateway"}`))
	}))
	defer server.Close()

	cfg := &config{client: server.Client(), timeout: time.Second}
	err := cfg.doJSON(&cobra.Command{}, http.MethodGet, server.URL, nil, nil, nil)
	if err == nil {
		t.Fatal("non-2xx response without an API error should fail")
	}
}

func TestSaveTokenEnforcesPrivatePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not expose POSIX file permissions")
	}

	path := filepath.Join(t.TempDir(), "token.json")
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := saveToken(path, []byte(`{"access_token":"secret"}`)); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("token permissions = %o, want 600", got)
	}
}
