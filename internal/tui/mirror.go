package tui

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"gofetch/internal/mirror"
)

const recentMax = 6

type mirrorFileMsg string
type mirrorDoneMsg struct {
	result mirror.Result
	err    error
}
type mirrorSpinMsg time.Time

type mirrorModel struct {
	ctx    context.Context
	cancel context.CancelFunc
	fileCh <-chan string
	doneCh <-chan mirrorDoneMsg
	url    string
	output string
	count  int
	recent []string
	done   bool
	err    error
	result mirror.Result
	frame  int
}

func RunMirror(ctx context.Context, stdout io.Writer, client *http.Client, start string, opts mirror.Options) (mirror.Result, error) {
	ctx, cancel := context.WithCancel(ctx)
	fileCh := make(chan string, 512)
	doneCh := make(chan mirrorDoneMsg, 1)

	opts.OnFile = func(path string) {
		select {
		case fileCh <- path:
		default:
		}
	}

	go func() {
		result, err := mirror.Run(ctx, client, start, opts)
		close(fileCh)
		doneCh <- mirrorDoneMsg{result, err}
	}()

	m := mirrorModel{
		ctx:    ctx,
		cancel: cancel,
		fileCh: fileCh,
		doneCh: doneCh,
		url:    start,
		output: opts.OutputDir,
	}
	p := tea.NewProgram(m, tea.WithOutput(stdout))
	final, _ := p.Run()
	cancel()
	got := final.(mirrorModel)
	return got.result, got.err
}

func (m mirrorModel) Init() tea.Cmd {
	return tea.Batch(awaitMirrorFile(m.fileCh), awaitMirrorDone(m.doneCh), mirrorSpin())
}

func (m mirrorModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			m.cancel()
			return m, tea.Quit
		}
	case mirrorFileMsg:
		m.count++
		path := string(msg)
		m.recent = append(m.recent, path)
		if len(m.recent) > recentMax {
			m.recent = m.recent[len(m.recent)-recentMax:]
		}
		return m, awaitMirrorFile(m.fileCh)
	case mirrorDoneMsg:
		m.done = true
		m.result = msg.result
		m.err = msg.err
		if len(msg.result.Downloaded) > 0 {
			m.count = len(msg.result.Downloaded)
		}
		return m, tea.Quit
	case mirrorSpinMsg:
		m.frame = (m.frame + 1) % len(spinnerFrames)
		if m.done {
			return m, nil
		}
		return m, mirrorSpin()
	}
	return m, nil
}

func (m mirrorModel) View() string {
	var b strings.Builder
	status := "mirroring"
	if m.done {
		status = "complete"
	}
	fmt.Fprintf(&b, "%s %s\n", titleStyle.Render("gofetch mirror"), statusLabel(status))
	fmt.Fprintf(&b, "%s%s\n", labelStyle.Render("url"), mutedStyle.Render(trimMiddle(m.url, 74)))
	fmt.Fprintf(&b, "%s%s\n\n", labelStyle.Render("output"), valueStyle.Render(m.output))

	if !m.done {
		fmt.Fprintf(&b, "%s fetching\n\n", mutedStyle.Render(spinnerFrames[m.frame]))
	}

	fmt.Fprintf(&b, "%s%s\n", labelStyle.Render("files"), valueStyle.Render(fmt.Sprintf("%d", m.count)))

	if len(m.recent) > 0 {
		fmt.Fprintf(&b, "\n%s\n", mutedStyle.Render("recent"))
		for _, f := range m.recent {
			fmt.Fprintf(&b, "  %s\n", mutedStyle.Render(trimMiddle(f, 78)))
		}
	}

	if m.done {
		if m.err != nil {
			fmt.Fprintf(&b, "\n%s %v\n", errStyle.Render("error"), m.err)
		} else {
			fmt.Fprintf(&b, "\n%s saved %d files into %s\n", doneStyle.Render("done"), m.count, m.output)
		}
	}

	return panelStyle.Width(88).Render(b.String())
}

func awaitMirrorFile(ch <-chan string) tea.Cmd {
	return func() tea.Msg {
		f, ok := <-ch
		if !ok {
			return nil
		}
		return mirrorFileMsg(f)
	}
}

func awaitMirrorDone(ch <-chan mirrorDoneMsg) tea.Cmd {
	return func() tea.Msg {
		return <-ch
	}
}

func mirrorSpin() tea.Cmd {
	return tea.Tick(100*time.Millisecond, func(t time.Time) tea.Msg {
		return mirrorSpinMsg(t)
	})
}
