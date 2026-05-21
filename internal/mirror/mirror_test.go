package mirror

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestMirrorSameHostStaticAssets(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<html><head><link href="/style.css"></head><body><img src="/img/logo.png"><a href="/about">About</a><a href="https://external.example/x">x</a></body></html>`))
		case "/about":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<html>About</html>`))
		case "/style.css":
			w.Header().Set("Content-Type", "text/css")
			_, _ = w.Write([]byte(`body{background:url("/img/bg.png")}`))
		case "/img/logo.png", "/img/bg.png":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("png"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	res, err := Run(context.Background(), srv.Client(), srv.URL, Options{OutputDir: dir, Workers: 2, MaxDepth: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"index.html", "style.css", "img/logo.png", "img/bg.png", "about/index.html"} {
		if _, err := os.Stat(filepath.Join(dir, rel)); err != nil {
			t.Fatalf("missing mirrored file %s: %v; downloaded=%v", rel, err, res.Downloaded)
		}
	}
	if len(res.Skipped) == 0 {
		t.Fatal("expected external URL to be skipped")
	}
}
