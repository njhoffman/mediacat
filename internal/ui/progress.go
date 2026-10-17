package ui

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/bubbles/progress"
	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
)

// Update is one progress report.
type Update struct {
	Label      string
	Counting   bool // discovering work: show Found instead of a bar
	Found      int
	Done       int
	Total      int
	Bytes      int64
	TotalBytes int64
	Current    string
}

// Progress displays a long-running operation. On a terminal it renders an animated bar
// and treats Ctrl-C as a request to stop gracefully (a second Ctrl-C exits immediately);
// otherwise it logs a line every few seconds.
type Progress struct {
	Out    io.Writer
	Cancel context.CancelFunc
}

type updateMsg Update
type doneMsg struct{}
type interruptMsg struct{}

// Run executes work, passing it a report function, and returns work's error.
func (p Progress) Run(ctx context.Context, work func(report func(Update)) error) error {
	if IsTerminal(p.Out) && IsTerminal(os.Stdin) {
		return p.runTUI(ctx, work)
	}
	return p.runPlain(work)
}

func (p Progress) runTUI(ctx context.Context, work func(report func(Update)) error) error {
	m := newModel(p.Cancel)
	prog := tea.NewProgram(m, tea.WithOutput(p.Out), tea.WithoutSignalHandler())
	var mu sync.Mutex
	var last time.Time
	report := func(u Update) {
		mu.Lock()
		defer mu.Unlock()
		if time.Since(last) < 50*time.Millisecond && (u.Counting || u.Done < u.Total) {
			return
		}
		last = time.Now()
		prog.Send(updateMsg(u))
	}
	var werr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		werr = work(report)
		prog.Send(doneMsg{})
	}()
	go func() {
		select {
		case <-ctx.Done():
			prog.Send(interruptMsg{})
		case <-done:
		}
	}()
	if _, err := prog.Run(); err != nil {
		p.Cancel()
		<-done
		return err
	}
	<-done
	return werr
}

// runPlain logs the final state of each phase, plus a line every 10 seconds while work
// is actually advancing.
func (p Progress) runPlain(work func(report func(Update)) error) error {
	var mu sync.Mutex
	var last Update
	var lastPrint time.Time
	var shown Update
	started, printed := false, true
	print := func(u Update) {
		switch {
		case u.Counting:
			fmt.Fprintf(p.Out, "%s: %s found\n", u.Label, Count(u.Found))
		case u.Total == 0:
			fmt.Fprintf(p.Out, "%s: nothing to do\n", u.Label)
		case u.TotalBytes > 0:
			fmt.Fprintf(p.Out, "%s: %s/%s  %s/%s\n", u.Label, Count(u.Done), Count(u.Total), Bytes(u.Bytes), Bytes(u.TotalBytes))
		default:
			fmt.Fprintf(p.Out, "%s: %s/%s\n", u.Label, Count(u.Done), Count(u.Total))
		}
		lastPrint, printed, shown = time.Now(), true, u
	}
	err := work(func(u Update) {
		mu.Lock()
		defer mu.Unlock()
		if started && u.Label != last.Label && !printed {
			print(last)
		}
		phaseChanged := !started || u.Label != last.Label
		started, last, printed = true, u, false
		moved := u.Done != shown.Done || u.Bytes != shown.Bytes || u.Found != shown.Found
		if !phaseChanged && moved && time.Since(lastPrint) >= 10*time.Second {
			print(u)
		}
	})
	mu.Lock()
	defer mu.Unlock()
	if started && !printed {
		print(last)
	}
	return err
}

type model struct {
	cancel       context.CancelFunc
	spin         spinner.Model
	bar          progress.Model
	u            Update
	phaseStart   time.Time
	width        int
	interrupting bool
	done         bool
}

func newModel(cancel context.CancelFunc) model {
	return model{
		cancel:     cancel,
		spin:       spinner.New(spinner.WithSpinner(spinner.Dot), spinner.WithStyle(Accent)),
		bar:        progress.New(progress.WithDefaultGradient(), progress.WithoutPercentage(), progress.WithWidth(30)),
		phaseStart: time.Now(),
		width:      80,
	}
}

func (m model) Init() tea.Cmd { return m.spin.Tick }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if msg.Type == tea.KeyCtrlC {
			if m.interrupting {
				fmt.Fprintln(os.Stderr, "\nforced exit; work since the last commit is lost")
				os.Exit(130)
			}
			m.interrupting = true
			m.cancel()
		}
	case interruptMsg:
		m.interrupting = true
	case tea.WindowSizeMsg:
		m.width = msg.Width
	case updateMsg:
		if msg.Label != m.u.Label {
			m.phaseStart = time.Now()
		}
		m.u = Update(msg)
	case doneMsg:
		m.done = true
		return m, tea.Quit
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m model) View() string {
	if m.done {
		return ""
	}
	u := m.u
	label := u.Label
	if label == "" {
		label = "Starting"
	}
	elapsed := time.Since(m.phaseStart)
	var line string
	switch {
	case u.Counting:
		line = fmt.Sprintf("%s %s  %s found  %s", m.spin.View(), Bold.Render(label), Count(u.Found), Dim.Render(Elapsed(elapsed)))
	case u.Total == 0:
		line = fmt.Sprintf("%s %s", m.spin.View(), Bold.Render(label))
	default:
		pct := float64(u.Done) / float64(u.Total)
		parts := []string{
			m.spin.View(), Bold.Render(label), m.bar.ViewAs(pct), fmt.Sprintf("%3.0f%%", pct*100),
			fmt.Sprintf("%s/%s", Count(u.Done), Count(u.Total)),
		}
		if u.TotalBytes > 0 {
			parts = append(parts, Dim.Render(fmt.Sprintf("%s/%s", Bytes(u.Bytes), Bytes(u.TotalBytes))))
		}
		if secs := elapsed.Seconds(); u.Done > 0 && secs > 1 {
			rate := float64(u.Done) / secs
			eta := time.Duration(float64(u.Total-u.Done)/rate) * time.Second
			parts = append(parts, Dim.Render(fmt.Sprintf("%.1f/s  ETA %s", rate, Elapsed(eta))))
		}
		line = strings.Join(parts, " ")
	}
	cur := ""
	if u.Current != "" {
		cur = "\n  " + Dim.Render(TruncateMiddle(u.Current, max(m.width-4, 20)))
	}
	if m.interrupting {
		cur += "\n  " + Warn.Render("Interrupting: finishing in-flight files and saving progress (Ctrl-C again to force)")
	}
	return line + cur + "\n"
}
