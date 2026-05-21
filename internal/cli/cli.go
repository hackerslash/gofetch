package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gofetch/internal/bench"
	"gofetch/internal/downloader"
	"gofetch/internal/mirror"
	"gofetch/internal/tui"
)

func Run(args []string, stdout, stderr io.Writer) int {
	return RunContext(context.Background(), args, stdout, stderr)
}

func RunContext(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	switch args[0] {
	case "get":
		return runGet(ctx, args[1:], stdout, stderr)
	case "resume":
		return runResume(ctx, args[1:], stdout, stderr)
	case "mirror":
		return runMirror(ctx, args[1:], stdout, stderr)
	case "bench":
		return runBench(ctx, args[1:], stdout, stderr)
	case "-h", "--help", "help":
		usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command: %s\n", args[0])
		usage(stderr)
		return 2
	}
}

func runGet(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("get", flag.ContinueOnError)
	fs.SetOutput(stderr)
	workers := fs.Int("workers", 4, "number of workers")
	output := fs.String("output", "", "output file for one URL or output directory for URL lists")
	if err := fs.Parse(interspersed(args)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: gofetch get <url|urls.txt> [--workers N] [--output PATH]")
		return 2
	}
	target := fs.Arg(0)
	client := downloader.New()
	urls, fromFile, err := resolveTargets(target)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	for _, raw := range urls {
		out := *output
		if fromFile {
			if out == "" {
				out = "."
			}
			if err := os.MkdirAll(out, 0o755); err != nil {
				fmt.Fprintln(stderr, err)
				return 1
			}
			out = filepath.Join(out, downloader.FilenameFromURL(raw))
		} else if out == "" {
			out = downloader.FilenameFromURL(raw)
		}
		taskPath := downloader.TaskPath(raw, out)
		if tui.IsTerminal(stdout) {
			res := tui.RunDownload(ctx, stdout, client, taskPath, raw, out, func(ctx context.Context) (*downloader.Task, error) {
				return client.Download(ctx, raw, out, *workers)
			})
			if res.Err != nil {
				if res.Paused {
					return 130
				}
				fmt.Fprintf(stderr, "%s: %v\n", raw, res.Err)
				return 1
			}
			continue
		}
		fmt.Fprintf(stdout, "resume task: %s\n", taskPath)
		task, err := client.Download(ctx, raw, out, *workers)
		if err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", raw, err)
			if _, statErr := os.Stat(taskPath); statErr == nil {
				fmt.Fprintf(stderr, "resume with: gofetch resume %s\n", taskPath)
			}
			return 1
		}
		fmt.Fprintf(stdout, "downloaded %s -> %s\n", raw, task.Output)
	}
	return 0
}

func runResume(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(interspersed(args)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: gofetch resume <download.task>")
		return 2
	}
	taskPath := fs.Arg(0)
	client := downloader.New()
	if tui.IsTerminal(stdout) {
		task, err := downloader.LoadTask(taskPath)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		res := tui.RunDownload(ctx, stdout, client, taskPath, task.URL, task.Output, func(ctx context.Context) (*downloader.Task, error) {
			return client.Resume(ctx, taskPath)
		})
		if res.Err != nil {
			if res.Paused {
				return 130
			}
			fmt.Fprintln(stderr, res.Err)
			return 1
		}
		return 0
	}
	task, err := client.Resume(ctx, taskPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "resumed %s -> %s\n", task.URL, task.Output)
	return 0
}

func runMirror(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mirror", flag.ContinueOnError)
	fs.SetOutput(stderr)
	workers := fs.Int("workers", 4, "number of workers")
	depth := fs.Int("depth", 2, "maximum same-host HTML crawl depth")
	output := fs.String("output", "mirror", "output directory")
	if err := fs.Parse(interspersed(args)); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: gofetch mirror <url> [--workers N] [--depth N] [--output DIR]")
		return 2
	}
	res, err := mirror.Run(ctx, http.DefaultClient, fs.Arg(0), mirror.Options{OutputDir: *output, Workers: *workers, MaxDepth: *depth})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "mirrored %d files into %s\n", len(res.Downloaded), *output)
	return 0
}

func runBench(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("bench", flag.ContinueOnError)
	fs.SetOutput(stderr)
	samples := fs.Int("samples", 1, "samples per URL")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(stderr, "usage: gofetch bench <url...> [--samples N]")
		return 2
	}
	results := bench.Run(ctx, &http.Client{Timeout: 30 * time.Second}, fs.Args(), *samples)
	for i, res := range results {
		if res.Error != "" {
			fmt.Fprintf(stdout, "%d. %s error=%s\n", i+1, res.URL, res.Error)
			continue
		}
		fmt.Fprintf(stdout, "%d. %s status=%d latency=%s bytes=%d speed=%.2f MiB/s\n",
			i+1, res.URL, res.StatusCode, res.Latency.Round(time.Millisecond), res.Bytes, res.ThroughputBps/(1024*1024))
	}
	if len(results) > 0 && results[0].Error == "" {
		fmt.Fprintf(stdout, "best: %s\n", results[0].URL)
	}
	return 0
}

func resolveTargets(target string) ([]string, bool, error) {
	if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
		return []string{target}, false, nil
	}
	data, err := os.ReadFile(target)
	if err != nil {
		return nil, false, err
	}
	var urls []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		urls = append(urls, line)
	}
	if len(urls) == 0 {
		return nil, true, fmt.Errorf("%s contains no URLs", target)
	}
	return urls, true, nil
}

func usage(w io.Writer) {
	fmt.Fprintln(w, `gofetch: concurrent curl + segmented download manager

Commands:
  gofetch get <url|urls.txt> [--workers N] [--output PATH]
  gofetch mirror <url> [--workers N] [--depth N] [--output DIR]
  gofetch resume <download.task>
  gofetch bench <url...> [--samples N]`)
}

func interspersed(args []string) []string {
	flags := make([]string, 0, len(args))
	positionals := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "-") {
			flags = append(flags, arg)
			if !strings.Contains(arg, "=") && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				flags = append(flags, args[i+1])
				i++
			}
			continue
		}
		positionals = append(positionals, arg)
	}
	return append(flags, positionals...)
}
