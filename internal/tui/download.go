package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"gofetch/internal/downloader"
)

type DownloadFunc func(context.Context) (*downloader.Task, error)

type Result struct {
	Task   *downloader.Task
	Err    error
	Paused bool
}

type segmentState struct {
	start   int64
	end     int64
	size    int64
	written int64
	done    bool
}

type model struct {
	ctx      context.Context
	cancel   context.CancelFunc
	progress <-chan downloader.ProgressEvent
	fn       DownloadFunc

	url      string
	output   string
	taskPath string
	status   string

	segments  map[int]segmentState
	lastSegs  map[int]int64
	segSpeeds map[int]float64
	totalSize int64
	total     int64
	speed     float64
	lastTotal int64
	lastTick  time.Time
	started   time.Time

	task   *downloader.Task
	err    error
	done   bool
	paused bool
}

type progressMsg downloader.ProgressEvent
type doneMsg struct {
	task *downloader.Task
	err  error
}
type tickMsg time.Time

var (
	accent       = lipgloss.Color("86")
	accentDim    = lipgloss.Color("36")
	ink          = lipgloss.Color("252")
	muted        = lipgloss.Color("244")
	subtle       = lipgloss.Color("238")
	success      = lipgloss.Color("42")
	warning      = lipgloss.Color("214")
	danger       = lipgloss.Color("196")
	titleStyle   = lipgloss.NewStyle().Bold(true).Foreground(accent)
	mutedStyle   = lipgloss.NewStyle().Foreground(muted)
	labelStyle   = lipgloss.NewStyle().Foreground(muted).Width(9)
	valueStyle   = lipgloss.NewStyle().Foreground(ink).Bold(true)
	doneStyle    = lipgloss.NewStyle().Foreground(success)
	warnStyle    = lipgloss.NewStyle().Foreground(warning)
	errStyle     = lipgloss.NewStyle().Foreground(danger)
	panelStyle   = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(subtle).Padding(1, 2)
	trackStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("237"))
	fillStyle    = lipgloss.NewStyle().Foreground(accent)
	doneFill     = lipgloss.NewStyle().Foreground(success)
	percentStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("250")).Width(7).Align(lipgloss.Right)
)

func IsTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

func RunDownload(ctx context.Context, stdout io.Writer, client *downloader.Client, taskPath, url, output string, fn DownloadFunc) Result {
	ctx, cancel := context.WithCancel(ctx)
	progress := make(chan downloader.ProgressEvent, 512)
	oldProgress := client.Progress
	client.Progress = func(ev downloader.ProgressEvent) {
		if oldProgress != nil {
			oldProgress(ev)
		}
		select {
		case progress <- ev:
		case <-ctx.Done():
		}
	}
	defer func() {
		client.Progress = oldProgress
	}()

	m := model{
		ctx:       ctx,
		cancel:    cancel,
		progress:  progress,
		fn:        fn,
		url:       url,
		output:    output,
		taskPath:  taskPath,
		status:    "probing remote file",
		segments:  map[int]segmentState{},
		lastSegs:  map[int]int64{},
		segSpeeds: map[int]float64{},
		started:   time.Now(),
		lastTick:  time.Now(),
	}
	p := tea.NewProgram(m, tea.WithOutput(stdout))
	final, err := p.Run()
	if err != nil {
		cancel()
		return Result{Err: err}
	}
	got := final.(model)
	return Result{Task: got.task, Err: got.err, Paused: got.paused}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(runDownload(m.ctx, m.fn), waitProgress(m.progress), tick())
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "p", "P", "ctrl+c", "q":
			if !m.done {
				m.paused = true
				m.status = "pausing and saving resume task"
				m.cancel()
				return m, nil
			}
			return m, tea.Quit
		}
	case tea.WindowSizeMsg:
		return m, nil
	case progressMsg:
		ev := downloader.ProgressEvent(msg)
		m.url = ev.URL
		m.output = ev.Output
		m.taskPath = ev.TaskPath
		if ev.TotalSize > 0 {
			m.totalSize = ev.TotalSize
		}
		m.segments[ev.Segment] = segmentState{
			start:   ev.SegmentStart,
			end:     ev.SegmentEnd,
			size:    ev.SegmentSize,
			written: ev.SegmentWritten,
			done:    ev.Done,
		}
		m.total = m.totalWritten()
		if ev.Completed {
			m.status = "complete"
		} else if m.paused {
			m.status = "pausing and saving resume task"
		} else {
			m.status = "downloading"
		}
		return m, waitProgress(m.progress)
	case tickMsg:
		now := time.Time(msg)
		elapsed := now.Sub(m.lastTick).Seconds()
		if elapsed > 0 {
			m.speed = float64(m.total-m.lastTotal) / elapsed
			for idx, seg := range m.segments {
				m.segSpeeds[idx] = float64(seg.written-m.lastSegs[idx]) / elapsed
				m.lastSegs[idx] = seg.written
			}
		}
		m.lastTotal = m.total
		m.lastTick = now
		if m.done {
			return m, nil
		}
		return m, tick()
	case doneMsg:
		m.task = msg.task
		m.err = msg.err
		m.done = true
		if msg.err != nil {
			if m.paused || errors.Is(msg.err, context.Canceled) {
				m.paused = true
				m.err = msg.err
				m.status = "paused"
			} else {
				m.status = "failed"
			}
		} else {
			m.status = "complete"
		}
		return m, tea.Quit
	}
	return m, nil
}

func (m model) View() string {
	var b strings.Builder
	contentWidth := 88
	fmt.Fprintf(&b, "%s %s\n", titleStyle.Render("gofetch"), statusLabel(m.status))
	if m.url != "" {
		fmt.Fprintf(&b, "%s\n", mutedStyle.Render(trimMiddle(m.url, contentWidth)))
	}
	if m.output != "" {
		fmt.Fprintf(&b, "%s%s\n", labelStyle.Render("output"), valueStyle.Render(filepath.Clean(m.output)))
	}
	if m.taskPath != "" && !m.done {
		fmt.Fprintf(&b, "%s%s\n", labelStyle.Render("resume"), mutedStyle.Render(m.taskPath))
	}

	fmt.Fprintf(&b, "\n%s\n", bar(m.total, m.totalSize, 58, false))
	fmt.Fprintf(&b, "%s  %s  %s\n",
		percentStyle.Render(percent(m.total, m.totalSize)),
		valueStyle.Render(formatBytes(m.total)+"/"+formatBytes(m.totalSize)),
		doneStyle.Render(formatSpeed(m.speed)),
	)
	fmt.Fprintf(&b, "%s %s   %s %d\n\n",
		mutedStyle.Render("elapsed"),
		valueStyle.Render(time.Since(m.started).Round(time.Second).String()),
		mutedStyle.Render("workers"),
		len(m.segments),
	)
	fmt.Fprintf(&b, "%s\n", mutedStyle.Render("Segments"))
	for _, idx := range sortedKeys(m.segments) {
		seg := m.segments[idx]
		name := fmt.Sprintf("thread-%02d", idx+1)
		if seg.done {
			name = doneStyle.Render(name)
		} else {
			name = valueStyle.Render(name)
		}
		rangeLabel := mutedStyle.Render(formatRange(seg.start, seg.end))
		speed := mutedStyle.Render(formatSpeed(m.segSpeeds[idx]))
		if seg.done {
			speed = doneStyle.Render("done")
		}
		fmt.Fprintf(&b, "%s %s %s %s %s\n",
			lipgloss.NewStyle().Width(11).Render(name),
			bar(seg.written, seg.size, 30, seg.done),
			percentStyle.Render(percent(seg.written, seg.size)),
			lipgloss.NewStyle().Width(12).Render(speed),
			rangeLabel,
		)
	}
	if len(m.segments) == 0 {
		fmt.Fprintf(&b, "%s\n", mutedStyle.Render("waiting for worker telemetry..."))
	}
	fmt.Fprintf(&b, "\n%s\n", mutedStyle.Render("p / Ctrl+C pause  ·  q quit after completion"))
	if m.done {
		switch {
		case m.err == nil:
			fmt.Fprintf(&b, "\n%s %s\n", doneStyle.Render("saved"), m.output)
		case m.paused:
			fmt.Fprintf(&b, "\n%s resume with: gofetch resume %s\n", warnStyle.Render("paused"), m.taskPath)
		default:
			fmt.Fprintf(&b, "\n%s %v\n", errStyle.Render("error"), m.err)
		}
	}
	return panelStyle.Width(contentWidth).Render(b.String())
}

func runDownload(ctx context.Context, fn DownloadFunc) tea.Cmd {
	return func() tea.Msg {
		task, err := fn(ctx)
		return doneMsg{task: task, err: err}
	}
}

func waitProgress(ch <-chan downloader.ProgressEvent) tea.Cmd {
	return func() tea.Msg {
		ev := <-ch
		return progressMsg(ev)
	}
}

func tick() tea.Cmd {
	return tea.Tick(250*time.Millisecond, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func (m model) totalWritten() int64 {
	var total int64
	for _, seg := range m.segments {
		total += seg.written
	}
	return total
}

func sortedKeys(m map[int]segmentState) []int {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	return keys
}

func statusLabel(status string) string {
	switch status {
	case "complete":
		return doneStyle.Render(status)
	case "paused", "pausing and saving resume task":
		return warnStyle.Render(status)
	case "failed":
		return errStyle.Render(status)
	default:
		return mutedStyle.Render(status)
	}
}

func bar(written, total int64, width int, done bool) string {
	if width < 1 {
		width = 1
	}
	if total <= 0 {
		return trackStyle.Render(strings.Repeat("·", width))
	}
	ratio := float64(written) / float64(total)
	if ratio < 0 {
		ratio = 0
	}
	if ratio > 1 {
		ratio = 1
	}
	filled := int(math.Round(ratio * float64(width)))
	style := fillStyle
	if done {
		style = doneFill
	}
	return style.Render(strings.Repeat("━", filled)) + trackStyle.Render(strings.Repeat("─", width-filled))
}

func percent(written, total int64) string {
	if total <= 0 {
		return "  --.-%"
	}
	return fmt.Sprintf("%6.1f%%", float64(written)*100/float64(total))
}

func formatBytes(n int64) string {
	if n < 0 {
		return "?"
	}
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	v := float64(n)
	for _, unit := range units {
		if v < 1024 || unit == units[len(units)-1] {
			return fmt.Sprintf("%.1f %s", v, unit)
		}
		v /= 1024
	}
	return fmt.Sprintf("%d B", n)
}

func formatSpeed(bytesPerSecond float64) string {
	if bytesPerSecond < 1 {
		return "0 B/s"
	}
	return formatBytes(int64(bytesPerSecond)) + "/s"
}

func formatRange(start, end int64) string {
	if end < start || end < 0 {
		return "stream"
	}
	return fmt.Sprintf("%s-%s", formatBytes(start), formatBytes(end))
}

func trimMiddle(s string, max int) string {
	if len(s) <= max || max < 8 {
		return s
	}
	left := (max - 3) / 2
	right := max - 3 - left
	return s[:left] + "..." + s[len(s)-right:]
}
