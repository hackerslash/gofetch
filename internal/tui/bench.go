package tui

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"gofetch/internal/bench"
)

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

type benchResultMsg bench.Result
type benchDoneMsg struct{}
type benchSpinMsg time.Time

type benchModel struct {
	ctx     context.Context
	cancel  context.CancelFunc
	ch      <-chan bench.Result
	urls    []string
	results []bench.Result
	done    bool
	frame   int
}

func RunBench(ctx context.Context, stdout io.Writer, client *http.Client, urls []string, samples int) []bench.Result {
	ctx, cancel := context.WithCancel(ctx)
	ch := make(chan bench.Result, len(urls)+1)

	go func() {
		bench.Run(ctx, client, urls, samples, func(r bench.Result) {
			ch <- r
		})
		close(ch)
	}()

	m := benchModel{ctx: ctx, cancel: cancel, ch: ch, urls: urls}
	p := tea.NewProgram(m, tea.WithOutput(stdout))
	final, _ := p.Run()
	cancel()
	return final.(benchModel).results
}

func (m benchModel) Init() tea.Cmd {
	return tea.Batch(awaitBenchResult(m.ch), benchSpin())
}

func (m benchModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if msg.String() == "q" || msg.String() == "ctrl+c" {
			m.cancel()
			return m, tea.Quit
		}
	case benchResultMsg:
		m.results = append(m.results, bench.Result(msg))
		return m, awaitBenchResult(m.ch)
	case benchDoneMsg:
		sort.SliceStable(m.results, func(i, j int) bool {
			return m.results[i].ThroughputBps > m.results[j].ThroughputBps
		})
		m.done = true
		return m, tea.Quit
	case benchSpinMsg:
		m.frame = (m.frame + 1) % len(spinnerFrames)
		if m.done {
			return m, nil
		}
		return m, benchSpin()
	}
	return m, nil
}

func (m benchModel) View() string {
	var b strings.Builder
	status := "testing"
	if m.done {
		status = "complete"
	}
	fmt.Fprintf(&b, "%s %s\n", titleStyle.Render("gofetch bench"), statusLabel(status))

	if !m.done && len(m.results) < len(m.urls) {
		current := m.urls[len(m.results)]
		fmt.Fprintf(&b, "\n%s %s\n", mutedStyle.Render(spinnerFrames[m.frame]), mutedStyle.Render(trimMiddle(current, 76)))
	}

	if len(m.results) > 0 {
		fmt.Fprintf(&b, "\n")
		for i, r := range m.results {
			num := mutedStyle.Render(fmt.Sprintf("%2d.", i+1))
			u := valueStyle.Render(trimMiddle(r.URL, 52))
			if r.Error != "" {
				fmt.Fprintf(&b, " %s  %s  %s\n", num, u, errStyle.Render(r.Error))
				continue
			}
			lat := mutedStyle.Render(r.Latency.Round(time.Millisecond).String())
			spd := doneStyle.Render(formatSpeed(r.ThroughputBps))
			fmt.Fprintf(&b, " %s  %s  %s  %s\n", num, u, lat, spd)
		}
	}

	if m.done && len(m.results) > 0 {
		for _, r := range m.results {
			if r.Error == "" {
				fmt.Fprintf(&b, "\n%s %s\n", labelStyle.Render("best"), valueStyle.Render(trimMiddle(r.URL, 74)))
				break
			}
		}
	}

	return panelStyle.Width(88).Render(b.String())
}

func awaitBenchResult(ch <-chan bench.Result) tea.Cmd {
	return func() tea.Msg {
		r, ok := <-ch
		if !ok {
			return benchDoneMsg{}
		}
		return benchResultMsg(r)
	}
}

func benchSpin() tea.Cmd {
	return tea.Tick(100*time.Millisecond, func(t time.Time) tea.Msg {
		return benchSpinMsg(t)
	})
}
