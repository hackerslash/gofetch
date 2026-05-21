package downloader

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestSplitRanges(t *testing.T) {
	got := SplitRanges(10, 3)
	want := []ByteRange{{0, 3}, {4, 6}, {7, 9}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("SplitRanges() = %#v, want %#v", got, want)
	}
}

func TestFilenameFromURL(t *testing.T) {
	tests := map[string]string{
		"https://example.com/file.zip":       "file.zip",
		"https://example.com/path/":          "index.html",
		"https://example.com":                "index.html",
		"https://example.com/archive.tar.gz": "archive.tar.gz",
	}
	for raw, want := range tests {
		if got := FilenameFromURL(raw); got != want {
			t.Fatalf("FilenameFromURL(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestTaskRoundTrip(t *testing.T) {
	dir := t.TempDir()
	task := &Task{
		Version:      taskVersion,
		URL:          "https://example.com/file",
		Output:       filepath.Join(dir, "file"),
		TotalSize:    3,
		AcceptRanges: true,
		Workers:      2,
		Ranges:       []RangeTask{{Start: 0, End: 2, PartPath: filepath.Join(dir, "part")}},
	}
	path := filepath.Join(dir, "download.task")
	if err := SaveTask(path, task); err != nil {
		t.Fatal(err)
	}
	got, err := LoadTask(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.URL != task.URL || got.Ranges[0].End != 2 {
		t.Fatalf("loaded task mismatch: %#v", got)
	}
}

func TestSegmentedDownload(t *testing.T) {
	data := bytes.Repeat([]byte("abcdef"), 1024)
	srv := rangeServer(data, true)
	defer srv.Close()

	dir := t.TempDir()
	out := filepath.Join(dir, "file.bin")
	raw := srv.URL + "/file.bin"
	task, err := New().Download(context.Background(), raw, out, 4)
	if err != nil {
		t.Fatal(err)
	}
	if !task.Completed || len(task.Ranges) != 4 {
		t.Fatalf("expected completed segmented task, got %#v", task)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("downloaded bytes differ")
	}
	if _, err := os.Stat(out + ".parts"); !os.IsNotExist(err) {
		t.Fatalf("parts directory should not be created next to output, err=%v", err)
	}
	if _, err := os.Stat(TaskPath(raw, out)); !os.IsNotExist(err) {
		t.Fatalf("task file should be removed after success, err=%v", err)
	}
	if _, err := os.Stat(task.TempDir); !os.IsNotExist(err) {
		t.Fatalf("temp dir should be removed after success, err=%v", err)
	}
}

func TestNoRangeFallbackDownload(t *testing.T) {
	data := []byte("plain response")
	srv := rangeServer(data, false)
	defer srv.Close()

	dir := t.TempDir()
	out := filepath.Join(dir, "plain.txt")
	task, err := New().Download(context.Background(), srv.URL+"/plain.txt", out, 8)
	if err != nil {
		t.Fatal(err)
	}
	if !task.Completed || len(task.Ranges) != 1 {
		t.Fatalf("expected single-stream task, got %#v", task)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Fatalf("got %q, want %q", got, data)
	}
}

func TestDownloadSendsUserAgent(t *testing.T) {
	data := []byte("needs user agent")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("User-Agent"), "gofetch/") {
			http.Error(w, "missing user agent", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		if r.Method == http.MethodHead {
			return
		}
		_, _ = w.Write(data)
	}))
	defer srv.Close()

	dir := t.TempDir()
	out := filepath.Join(dir, "ua.txt")
	if _, err := New().Download(context.Background(), srv.URL+"/ua.txt", out, 2); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Fatalf("got %q, want %q", got, data)
	}
}

func TestResumeSegmentedTask(t *testing.T) {
	data := []byte("0123456789abcdefghij")
	srv := rangeServer(data, true)
	defer srv.Close()

	dir := t.TempDir()
	out := filepath.Join(dir, "resume.bin")
	partDir := out + ".parts"
	if err := os.MkdirAll(partDir, 0o755); err != nil {
		t.Fatal(err)
	}
	part0 := filepath.Join(partDir, "part-0000")
	part1 := filepath.Join(partDir, "part-0001")
	if err := os.WriteFile(part0, data[:10], 0o644); err != nil {
		t.Fatal(err)
	}
	task := &Task{
		Version:      taskVersion,
		URL:          srv.URL + "/resume.bin",
		Output:       out,
		TotalSize:    int64(len(data)),
		AcceptRanges: true,
		ETag:         `"test-etag"`,
		Workers:      2,
		Ranges: []RangeTask{
			{Start: 0, End: 9, Done: true, PartPath: part0, BytesWritten: 10},
			{Start: 10, End: 19, PartPath: part1},
		},
	}
	taskPath := filepath.Join(dir, "download.task")
	if err := SaveTask(taskPath, task); err != nil {
		t.Fatal(err)
	}
	resumed, err := New().Resume(context.Background(), taskPath)
	if err != nil {
		t.Fatal(err)
	}
	if !resumed.Completed {
		t.Fatal("task was not completed")
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("resumed bytes differ")
	}
}

func rangeServer(data []byte, ranges bool) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"test-etag"`)
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		if ranges {
			w.Header().Set("Accept-Ranges", "bytes")
		}
		if r.Method == http.MethodHead {
			return
		}
		if ranges && r.Header.Get("Range") != "" {
			start, end := parseRange(r.Header.Get("Range"), int64(len(data)))
			w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(data[start : end+1])
			return
		}
		_, _ = w.Write(data)
	}))
}

func parseRange(header string, total int64) (int64, int64) {
	spec := strings.TrimPrefix(header, "bytes=")
	parts := strings.Split(spec, "-")
	start, _ := strconv.ParseInt(parts[0], 10, 64)
	end := total - 1
	if len(parts) > 1 && parts[1] != "" {
		end, _ = strconv.ParseInt(parts[1], 10, 64)
	}
	return start, end
}
