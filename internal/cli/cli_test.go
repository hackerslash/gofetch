package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestGetURLList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(r.URL.Path))
	}))
	defer srv.Close()

	dir := t.TempDir()
	list := filepath.Join(dir, "urls.txt")
	if err := os.WriteFile(list, []byte(srv.URL+"/one\n"+srv.URL+"/two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "downloads")
	var stdout, stderr bytes.Buffer
	code := Run([]string{"get", list, "--workers", "2", "--output", out}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("Run exited %d: %s", code, stderr.String())
	}
	for _, name := range []string{"one", "two"} {
		if _, err := os.Stat(filepath.Join(out, name)); err != nil {
			t.Fatalf("missing %s: %v", name, err)
		}
	}
}
