package bench

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"
)

const sampleBytes int64 = 4 << 20

type Result struct {
	URL           string
	Status        string
	StatusCode    int
	ContentLength int64
	Latency       time.Duration
	Bytes         int64
	Duration      time.Duration
	ThroughputBps float64
	Error         string
}

func Run(ctx context.Context, client *http.Client, urls []string, samples int, onResult func(Result)) []Result {
	if client == nil {
		client = http.DefaultClient
	}
	if samples < 1 {
		samples = 1
	}
	results := make([]Result, 0, len(urls))
	for _, raw := range urls {
		var merged Result
		merged.URL = raw
		var totalThroughput float64
		var okSamples int
		for i := 0; i < samples; i++ {
			res := sample(ctx, client, raw)
			if res.Error != "" {
				merged = res
				break
			}
			if i == 0 {
				merged = res
			}
			totalThroughput += res.ThroughputBps
			okSamples++
		}
		if okSamples > 0 {
			merged.ThroughputBps = totalThroughput / float64(okSamples)
		}
		results = append(results, merged)
		if onResult != nil {
			onResult(merged)
		}
	}
	sort.SliceStable(results, func(i, j int) bool {
		return results[i].ThroughputBps > results[j].ThroughputBps
	})
	return results
}

func sample(ctx context.Context, client *http.Client, raw string) Result {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return Result{URL: raw, Error: err.Error()}
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", sampleBytes-1))
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return Result{URL: raw, Error: err.Error()}
	}
	latency := time.Since(start)
	defer resp.Body.Close()
	n, err := io.Copy(io.Discard, resp.Body)
	duration := time.Since(start)
	res := Result{
		URL:           raw,
		Status:        resp.Status,
		StatusCode:    resp.StatusCode,
		ContentLength: resp.ContentLength,
		Latency:       latency,
		Bytes:         n,
		Duration:      duration,
	}
	if err != nil {
		res.Error = err.Error()
		return res
	}
	if duration > 0 {
		res.ThroughputBps = float64(n) / duration.Seconds()
	}
	return res
}
