package mirror

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

type Options struct {
	OutputDir string
	Workers   int
	MaxDepth  int
}

type Result struct {
	Downloaded []string
	Skipped    []string
}

type item struct {
	URL   *url.URL
	Depth int
}

var (
	attrRE = regexp.MustCompile(`(?i)(?:href|src)\s*=\s*["']([^"'#]+)["']`)
	cssRE  = regexp.MustCompile(`(?i)url\(\s*["']?([^"')#]+)["']?\s*\)`)
)

func Run(ctx context.Context, client *http.Client, start string, opts Options) (Result, error) {
	if client == nil {
		client = http.DefaultClient
	}
	if opts.OutputDir == "" {
		opts.OutputDir = "mirror"
	}
	if opts.Workers < 1 {
		opts.Workers = 4
	}
	if opts.MaxDepth < 0 {
		opts.MaxDepth = 0
	}
	root, err := url.Parse(start)
	if err != nil {
		return Result{}, err
	}
	if root.Scheme == "" || root.Host == "" {
		return Result{}, fmt.Errorf("mirror URL must be absolute")
	}

	var result Result
	seen := map[string]bool{}
	var seenMu sync.Mutex
	jobs := make(chan item)
	var pending sync.WaitGroup
	var workers sync.WaitGroup
	var firstErr error
	var errMu sync.Mutex

	enqueue := func(it item) {
		if it.URL.Scheme != root.Scheme || it.URL.Host != root.Host {
			seenMu.Lock()
			result.Skipped = append(result.Skipped, it.URL.String())
			seenMu.Unlock()
			return
		}
		key := normalized(it.URL)
		seenMu.Lock()
		if seen[key] {
			seenMu.Unlock()
			return
		}
		seen[key] = true
		pending.Add(1)
		seenMu.Unlock()
		go func() {
			select {
			case jobs <- it:
			case <-ctx.Done():
				pending.Done()
			}
		}()
	}

	for i := 0; i < opts.Workers; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for it := range jobs {
				local, links, err := fetch(ctx, client, root, it.URL, opts.OutputDir)
				if err != nil {
					errMu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					errMu.Unlock()
					pending.Done()
					continue
				}
				seenMu.Lock()
				result.Downloaded = append(result.Downloaded, local)
				seenMu.Unlock()
				for _, link := range links {
					if it.Depth < opts.MaxDepth || !looksHTML(link) {
						enqueue(item{URL: link, Depth: it.Depth + 1})
					}
				}
				pending.Done()
			}
		}()
	}
	enqueue(item{URL: root, Depth: 0})
	pending.Wait()
	close(jobs)
	workers.Wait()
	if firstErr != nil {
		return result, firstErr
	}
	return result, ctx.Err()
}

func fetch(ctx context.Context, client *http.Client, root, u *url.URL, outputDir string) (string, []*url.URL, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", nil, fmt.Errorf("%s: %s", u.String(), resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, err
	}
	local := localPath(u, outputDir, resp.Header.Get("Content-Type"))
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		return "", nil, err
	}
	if err := os.WriteFile(local, body, 0o644); err != nil {
		return "", nil, err
	}
	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	links := extractLinks(u, body, contentType)
	return local, links, nil
}

func extractLinks(base *url.URL, body []byte, contentType string) []*url.URL {
	var matches [][][]byte
	if strings.Contains(contentType, "text/css") || strings.HasSuffix(base.Path, ".css") {
		matches = cssRE.FindAllSubmatch(body, -1)
	} else if strings.Contains(contentType, "text/html") || contentType == "" || strings.HasSuffix(base.Path, ".html") {
		matches = attrRE.FindAllSubmatch(body, -1)
		matches = append(matches, cssRE.FindAllSubmatch(body, -1)...)
	}
	out := make([]*url.URL, 0, len(matches))
	for _, m := range matches {
		if len(m) < 2 {
			continue
		}
		raw := strings.TrimSpace(string(m[1]))
		if raw == "" || strings.HasPrefix(raw, "mailto:") || strings.HasPrefix(raw, "javascript:") || strings.HasPrefix(raw, "data:") {
			continue
		}
		ref, err := url.Parse(raw)
		if err != nil {
			continue
		}
		out = append(out, base.ResolveReference(ref))
	}
	return out
}

func localPath(u *url.URL, outputDir, contentType string) string {
	clean := path.Clean("/" + u.EscapedPath())
	if clean == "/" || strings.HasSuffix(u.Path, "/") || strings.Contains(strings.ToLower(contentType), "text/html") && path.Ext(clean) == "" {
		clean = path.Join(clean, "index.html")
	}
	clean = strings.TrimPrefix(clean, "/")
	if clean == "" {
		clean = "index.html"
	}
	return filepath.Join(outputDir, filepath.FromSlash(clean))
}

func normalized(u *url.URL) string {
	cp := *u
	cp.Fragment = ""
	return cp.String()
}

func looksHTML(u *url.URL) bool {
	ext := strings.ToLower(path.Ext(u.Path))
	return ext == "" || ext == ".html" || ext == ".htm" || strings.HasSuffix(u.Path, "/")
}
