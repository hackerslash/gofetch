package downloader

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type ProbeInfo struct {
	URL          string
	TotalSize    int64
	AcceptRanges bool
	ETag         string
	LastModified string
	ContentType  string
	StatusCode   int
}

type Client struct {
	HTTPClient *http.Client
	UserAgent  string
	Progress   ProgressFunc
}

type ByteRange struct {
	Start int64
	End   int64
}

type ProgressFunc func(ProgressEvent)

type ProgressEvent struct {
	TaskPath       string
	URL            string
	Output         string
	Segment        int
	SegmentStart   int64
	SegmentEnd     int64
	SegmentWritten int64
	SegmentSize    int64
	TotalSize      int64
	Done           bool
	Completed      bool
}

func New() *Client {
	return &Client{
		HTTPClient: &http.Client{Timeout: 0},
		UserAgent:  "gofetch/0.1 (+https://github.com/hackerslash/gofetch)",
	}
}

func (c *Client) httpClient() *http.Client {
	if c != nil && c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}

func (c *Client) newRequest(ctx context.Context, method, raw string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, raw, body)
	if err != nil {
		return nil, err
	}
	userAgent := "gofetch/0.1 (+https://github.com/hackerslash/gofetch)"
	if c != nil && c.UserAgent != "" {
		userAgent = c.UserAgent
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "*/*")
	return req, nil
}

func FilenameFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "download"
	}
	if u.Path == "" || strings.HasSuffix(u.Path, "/") {
		return "index.html"
	}
	name := path.Base(u.Path)
	if name == "." || name == "/" || name == "" {
		return "index.html"
	}
	name = strings.TrimSpace(name)
	name = strings.ReplaceAll(name, string(os.PathSeparator), "_")
	if name == "" {
		return "download"
	}
	return name
}

func SplitRanges(total int64, workers int) []ByteRange {
	if total <= 0 {
		return nil
	}
	if workers < 1 {
		workers = 1
	}
	if int64(workers) > total {
		workers = int(total)
	}
	chunk := total / int64(workers)
	rem := total % int64(workers)
	ranges := make([]ByteRange, 0, workers)
	var start int64
	for i := 0; i < workers; i++ {
		size := chunk
		if int64(i) < rem {
			size++
		}
		end := start + size - 1
		ranges = append(ranges, ByteRange{Start: start, End: end})
		start = end + 1
	}
	return ranges
}

func (c *Client) Probe(ctx context.Context, raw string) (ProbeInfo, error) {
	req, err := c.newRequest(ctx, http.MethodHead, raw, nil)
	if err != nil {
		return ProbeInfo{}, err
	}
	resp, err := c.httpClient().Do(req)
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 400 {
			info := probeFromResponse(raw, resp)
			if strings.EqualFold(resp.Header.Get("Accept-Ranges"), "bytes") {
				info.AcceptRanges = true
			}
			if info.AcceptRanges || info.TotalSize > 0 {
				return info, nil
			}
		}
	}

	req, err = c.newRequest(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return ProbeInfo{}, err
	}
	req.Header.Set("Range", "bytes=0-0")
	resp, err = c.httpClient().Do(req)
	if err != nil {
		return ProbeInfo{}, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	info := probeFromResponse(raw, resp)
	if resp.StatusCode == http.StatusPartialContent {
		info.AcceptRanges = true
		if total := parseContentRangeTotal(resp.Header.Get("Content-Range")); total > 0 {
			info.TotalSize = total
		}
	}
	if resp.StatusCode >= 400 {
		return info, fmt.Errorf("probe failed: %s", resp.Status)
	}
	return info, nil
}

func probeFromResponse(raw string, resp *http.Response) ProbeInfo {
	return ProbeInfo{
		URL:          raw,
		TotalSize:    resp.ContentLength,
		AcceptRanges: strings.EqualFold(resp.Header.Get("Accept-Ranges"), "bytes"),
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
		ContentType:  resp.Header.Get("Content-Type"),
		StatusCode:   resp.StatusCode,
	}
}

func parseContentRangeTotal(v string) int64 {
	slash := strings.LastIndex(v, "/")
	if slash < 0 || slash == len(v)-1 {
		return 0
	}
	var total int64
	if _, err := fmt.Sscanf(v[slash+1:], "%d", &total); err != nil {
		return 0
	}
	return total
}

func (c *Client) Download(ctx context.Context, raw, output string, workers int) (*Task, error) {
	if output == "" {
		output = FilenameFromURL(raw)
	}
	if workers < 1 {
		workers = 1
	}
	probe, err := c.Probe(ctx, raw)
	if err != nil {
		return nil, err
	}
	task := newTask(raw, output, workers, probe)
	taskPath := TaskPath(raw, output)
	if err := SaveTask(taskPath, task); err != nil {
		return nil, err
	}
	return c.runTask(ctx, taskPath, task)
}

func (c *Client) Resume(ctx context.Context, taskPath string) (*Task, error) {
	task, err := LoadTask(taskPath)
	if err != nil {
		return nil, err
	}
	if task.Completed {
		if _, err := os.Stat(task.Output); err == nil {
			return task, nil
		}
		task.Completed = false
	}
	probe, err := c.Probe(ctx, task.URL)
	if err != nil {
		return nil, err
	}
	if validatorsChanged(task, probe) {
		for i := range task.Ranges {
			_ = os.Remove(task.Ranges[i].PartPath)
			task.Ranges[i].Done = false
			task.Ranges[i].BytesWritten = 0
		}
		task.TotalSize = probe.TotalSize
		task.AcceptRanges = probe.AcceptRanges
		task.ETag = probe.ETag
		task.LastModified = probe.LastModified
	}
	if err := SaveTask(taskPath, task); err != nil {
		return nil, err
	}
	return c.runTask(ctx, taskPath, task)
}

func newTask(raw, output string, workers int, probe ProbeInfo) *Task {
	now := time.Now().UTC()
	task := &Task{
		Version:      taskVersion,
		URL:          raw,
		Output:       output,
		TotalSize:    probe.TotalSize,
		AcceptRanges: probe.AcceptRanges,
		ETag:         probe.ETag,
		LastModified: probe.LastModified,
		Workers:      workers,
		TempDir:      TempDir(raw, output),
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if probe.AcceptRanges && probe.TotalSize > 0 && workers > 1 {
		partDir := task.TempDir
		for i, br := range SplitRanges(probe.TotalSize, workers) {
			task.Ranges = append(task.Ranges, RangeTask{
				Start:    br.Start,
				End:      br.End,
				PartPath: filepath.Join(partDir, fmt.Sprintf("part-%04d", i)),
			})
		}
		return task
	}
	end := probe.TotalSize - 1
	if probe.TotalSize <= 0 {
		end = -1
	}
	task.Ranges = []RangeTask{{Start: 0, End: end, PartPath: filepath.Join(task.TempDir, "download.part")}}
	return task
}

func (c *Client) runTask(ctx context.Context, taskPath string, task *Task) (*Task, error) {
	if len(task.Ranges) == 0 {
		return nil, errors.New("task has no ranges")
	}
	c.emitTaskProgress(taskPath, task)
	if task.AcceptRanges && task.TotalSize > 0 && len(task.Ranges) > 1 {
		return c.runSegmented(ctx, taskPath, task)
	}
	return c.runSingle(ctx, taskPath, task)
}

func (c *Client) runSingle(ctx context.Context, taskPath string, task *Task) (*Task, error) {
	if err := os.MkdirAll(filepath.Dir(task.Output), 0o755); err != nil && filepath.Dir(task.Output) != "." {
		return nil, err
	}
	rt := &task.Ranges[0]
	var start int64
	mode := os.O_CREATE | os.O_WRONLY
	if task.AcceptRanges {
		if st, err := os.Stat(rt.PartPath); err == nil {
			start = st.Size()
		}
		mode |= os.O_APPEND
	} else {
		mode |= os.O_TRUNC
	}
	if err := os.MkdirAll(filepath.Dir(rt.PartPath), 0o755); err != nil {
		return nil, err
	}
	req, err := c.newRequest(ctx, http.MethodGet, task.URL, nil)
	if err != nil {
		return nil, err
	}
	if task.AcceptRanges && start > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", start))
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("download failed: %s", resp.Status)
	}
	if start > 0 && resp.StatusCode != http.StatusPartialContent {
		start = 0
		mode &^= os.O_APPEND
		mode |= os.O_TRUNC
	}
	out, err := os.OpenFile(rt.PartPath, mode, 0o644)
	if err != nil {
		return nil, err
	}
	writer := &progressWriter{
		writer: out,
		onWrite: func(n int64) {
			rt.BytesWritten = start + n
			c.emitProgress(taskPath, task, 0, *rt, false, false)
		},
	}
	written, copyErr := io.Copy(writer, resp.Body)
	closeErr := out.Close()
	rt.BytesWritten = start + written
	if copyErr != nil {
		_ = SaveTask(taskPath, task)
		return nil, copyErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if err := moveFile(rt.PartPath, task.Output); err != nil {
		return nil, err
	}
	rt.Done = true
	task.Completed = true
	c.emitProgress(taskPath, task, 0, *rt, true, true)
	if err := SaveTask(taskPath, task); err != nil {
		return nil, err
	}
	return task, cleanupTask(taskPath, task)
}

func (c *Client) runSegmented(ctx context.Context, taskPath string, task *Task) (*Task, error) {
	if err := os.MkdirAll(filepath.Dir(task.Output), 0o755); err != nil && filepath.Dir(task.Output) != "." {
		return nil, err
	}
	for _, rt := range task.Ranges {
		if err := os.MkdirAll(filepath.Dir(rt.PartPath), 0o755); err != nil {
			return nil, err
		}
	}
	jobs := make(chan int)
	errs := make(chan error, 1)
	var wg sync.WaitGroup
	var mu sync.Mutex
	workers := task.Workers
	if workers < 1 {
		workers = 1
	}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				if err := c.downloadRange(ctx, taskPath, task, idx, task.URL, &task.Ranges[idx]); err != nil {
					select {
					case errs <- err:
					default:
					}
					continue
				}
				mu.Lock()
				task.Ranges[idx].Done = true
				task.Ranges[idx].BytesWritten = task.Ranges[idx].End - task.Ranges[idx].Start + 1
				c.emitProgress(taskPath, task, idx, task.Ranges[idx], true, false)
				_ = SaveTask(taskPath, task)
				mu.Unlock()
			}
		}()
	}
	go func() {
		defer close(jobs)
		for i := range task.Ranges {
			if task.Ranges[i].Done {
				continue
			}
			select {
			case jobs <- i:
			case <-ctx.Done():
				return
			}
		}
	}()
	wg.Wait()
	select {
	case err := <-errs:
		_ = SaveTask(taskPath, task)
		return nil, err
	default:
	}
	if err := ctx.Err(); err != nil {
		_ = SaveTask(taskPath, task)
		return nil, err
	}
	if err := mergeParts(task); err != nil {
		return nil, err
	}
	task.Completed = true
	c.emitTaskProgress(taskPath, task)
	if err := SaveTask(taskPath, task); err != nil {
		return nil, err
	}
	return task, cleanupTask(taskPath, task)
}

func (c *Client) downloadRange(ctx context.Context, taskPath string, task *Task, segment int, raw string, rt *RangeTask) error {
	need := rt.End - rt.Start + 1
	var have int64
	if st, err := os.Stat(rt.PartPath); err == nil {
		have = st.Size()
	}
	if have == need {
		return nil
	}
	if have > need {
		have = 0
	}
	rt.BytesWritten = have
	c.emitProgress(taskPath, task, segment, *rt, false, false)
	mode := os.O_CREATE | os.O_WRONLY
	if have > 0 {
		mode |= os.O_APPEND
	} else {
		mode |= os.O_TRUNC
	}
	req, err := c.newRequest(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", rt.Start+have, rt.End))
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("range request failed: %s", resp.Status)
	}
	out, err := os.OpenFile(rt.PartPath, mode, 0o644)
	if err != nil {
		return err
	}
	writer := &progressWriter{
		writer: out,
		onWrite: func(n int64) {
			rt.BytesWritten = have + n
			c.emitProgress(taskPath, task, segment, *rt, false, false)
		},
	}
	_, copyErr := io.Copy(writer, resp.Body)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

type progressWriter struct {
	writer  io.Writer
	written int64
	onWrite func(int64)
}

func (w *progressWriter) Write(p []byte) (int, error) {
	n, err := w.writer.Write(p)
	if n > 0 {
		w.written += int64(n)
		if w.onWrite != nil {
			w.onWrite(w.written)
		}
	}
	return n, err
}

func (c *Client) emitTaskProgress(taskPath string, task *Task) {
	if c == nil || c.Progress == nil {
		return
	}
	for i, rt := range task.Ranges {
		c.emitProgress(taskPath, task, i, rt, rt.Done, task.Completed)
	}
}

func (c *Client) emitProgress(taskPath string, task *Task, segment int, rt RangeTask, done, completed bool) {
	if c == nil || c.Progress == nil {
		return
	}
	size := int64(-1)
	if rt.End >= rt.Start {
		size = rt.End - rt.Start + 1
	}
	c.Progress(ProgressEvent{
		TaskPath:       taskPath,
		URL:            task.URL,
		Output:         task.Output,
		Segment:        segment,
		SegmentStart:   rt.Start,
		SegmentEnd:     rt.End,
		SegmentWritten: rt.BytesWritten,
		SegmentSize:    size,
		TotalSize:      task.TotalSize,
		Done:           done,
		Completed:      completed,
	})
}

func mergeParts(task *Task) error {
	sort.Slice(task.Ranges, func(i, j int) bool {
		return task.Ranges[i].Start < task.Ranges[j].Start
	})
	tmp := task.Output + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	for _, rt := range task.Ranges {
		if !rt.Done {
			_ = out.Close()
			return errors.New("cannot merge incomplete task")
		}
		part, err := os.Open(rt.PartPath)
		if err != nil {
			_ = out.Close()
			return err
		}
		_, copyErr := io.Copy(out, part)
		closeErr := part.Close()
		if copyErr != nil {
			_ = out.Close()
			return copyErr
		}
		if closeErr != nil {
			_ = out.Close()
			return closeErr
		}
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, task.Output)
}

func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Remove(src)
}

func cleanupTask(taskPath string, task *Task) error {
	for _, rt := range task.Ranges {
		_ = os.Remove(rt.PartPath)
	}
	if task.TempDir != "" {
		_ = os.RemoveAll(task.TempDir)
	}
	_ = os.Remove(taskPath)
	_ = os.Remove(TempRoot())
	return nil
}
