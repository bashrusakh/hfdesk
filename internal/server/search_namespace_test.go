package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestSearchDoesNotReportSuccessForIncompleteNamespace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test requires creating a dangling directory symlink")
	}
	base := t.TempDir()
	local := filepath.Join(base, "local")
	if err := os.MkdirAll(local, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "missing"), filepath.Join(local, "broken-owner")); err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"owner/model"}]`))
	}))
	defer upstream.Close()

	s := New(Config{CacheDir: filepath.Join(base, "cache"), LocalScanDirs: []string{local}, Endpoint: upstream.URL})
	w := cacheRequest(t, s, "GET", "/api/search?q=owner", "")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("search status = %d, want incomplete-observation error: %s", w.Code, w.Body.String())
	}
}
