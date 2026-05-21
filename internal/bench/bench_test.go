package bench

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestBenchRanksFastestFirst(t *testing.T) {
	fast := benchServer(0)
	defer fast.Close()
	slow := benchServer(30 * time.Millisecond)
	defer slow.Close()

	results := Run(context.Background(), http.DefaultClient, []string{slow.URL, fast.URL}, 1, nil)
	if len(results) != 2 {
		t.Fatalf("got %d results", len(results))
	}
	if results[0].URL != fast.URL {
		t.Fatalf("fast server should rank first: %#v", results)
	}
}

func benchServer(delay time.Duration) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if delay > 0 {
			time.Sleep(delay)
		}
		w.Header().Set("Content-Length", "4096")
		_, _ = w.Write(make([]byte, 4096))
	}))
}
