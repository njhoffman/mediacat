// Package review is the interactive duplicate reviewer: a scrolling list of duplicate
// groups where you pick the copy to keep and what happens to the others.
package review

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/njhoffman/mediacat/internal/catalog"
	"github.com/njhoffman/mediacat/internal/dupes"
	"github.com/njhoffman/mediacat/internal/ui"
)

// Choice is what to do with one group: which copy to keep and the action for the rest.
// Action "" leaves the group untouched.
type Choice struct {
	Keep   int
	Action string
}

// Options configure the reviewer.
type Options struct {
	MoveTo string                    // destination for move; empty disables it
	Save   func(group int, c Choice) // called on every change so progress survives quitting
	Open   func(path string) error   // previews a file
	// Message is shown in the footer when the reviewer opens.
	Message string
}

// Result is how the review ended.
type Result struct {
	Choices []Choice
	Apply   bool // false: quit without applying (choices are still saved)
}

type keyMap struct {
	Down, Up, NextGroup, PrevGroup, PageDown, PageUp, Top, Bottom key.Binding
	Keep, Trash, Move, Delete, Leave                              key.Binding
	TrashAll, MoveAll, DeleteAll, LeaveAll                        key.Binding
	Open, Apply, Quit, Help                                       key.Binding
}

var keys = keyMap{
	Down:      key.NewBinding(key.WithKeys("j", "down"), key.WithHelp("j/k", "move")),
	Up:        key.NewBinding(key.WithKeys("k", "up")),
	NextGroup: key.NewBinding(key.WithKeys("]", "tab"), key.WithHelp("]/[", "next/prev group")),
	PrevGroup: key.NewBinding(key.WithKeys("[", "shift+tab")),
	PageDown:  key.NewBinding(key.WithKeys("pgdown", "ctrl+d"), key.WithHelp("pgdn/pgup", "page")),
	PageUp:    key.NewBinding(key.WithKeys("pgup", "ctrl+u")),
	Top:       key.NewBinding(key.WithKeys("g", "home"), key.WithHelp("g/G", "top/bottom")),
	Bottom:    key.NewBinding(key.WithKeys("G", "end")),
	Keep:      key.NewBinding(key.WithKeys(" ", "enter"), key.WithHelp("space", "keep this copy")),
	Trash:     key.NewBinding(key.WithKeys("t"), key.WithHelp("t", "trash others")),
	Move:      key.NewBinding(key.WithKeys("m"), key.WithHelp("m", "move others")),
	Delete:    key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "delete others")),
	Leave:     key.NewBinding(key.WithKeys("s", "x"), key.WithHelp("s", "leave group")),
	TrashAll:  key.NewBinding(key.WithKeys("T"), key.WithHelp("T/M/D/S", "same for every group")),
	MoveAll:   key.NewBinding(key.WithKeys("M")),
	DeleteAll: key.NewBinding(key.WithKeys("D")),
	LeaveAll:  key.NewBinding(key.WithKeys("S")),
	Open:      key.NewBinding(key.WithKeys("o"), key.WithHelp("o", "open file")),
	Apply:     key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "apply")),
	Quit:      key.NewBinding(key.WithKeys("q", "esc", "ctrl+c"), key.WithHelp("q", "quit (choices saved)")),
	Help:      key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
}

// Model is the reviewer state.
type Model struct {
	groups  []dupes.Group
	choices []Choice
	opts    Options
	g, f    int // cursor: group, file within group
	offset  int // first visible list line
	width   int
	height  int
	help    bool
	msg     string
	msgAt   time.Time
	apply   bool
	done    bool
}

// New builds a reviewer. choices must have one entry per group.
func New(groups []dupes.Group, choices []Choice, opts Options) Model {
	m := Model{groups: groups, choices: choices, opts: opts, width: 100, height: 30}
	if opts.Message != "" {
		m.say("%s", opts.Message)
	}
	return m
}

// Run shows the reviewer full-screen and returns the final choices.
func Run(groups []dupes.Group, choices []Choice, opts Options) (Result, error) {
	final, err := tea.NewProgram(New(groups, choices, opts), tea.WithAltScreen()).Run()
	if err != nil {
		return Result{}, err
	}
	m := final.(Model)
	return Result{Choices: m.choices, Apply: m.apply}, nil
}

// Choices returns the current choices.
func (m Model) Choices() []Choice { return m.choices }

// Applied reports whether the user chose to apply.
func (m Model) Applied() bool { return m.apply }

func (m Model) Init() tea.Cmd { return nil }

func (m *Model) set(group int, c Choice) {
	if m.choices[group] == c {
		return
	}
	m.choices[group] = c
	if m.opts.Save != nil {
		m.opts.Save(group, c)
	}
}

func (m *Model) say(format string, a ...any) {
	m.msg, m.msgAt = fmt.Sprintf(format, a...), time.Now()
}

func (m *Model) step(delta int) {
	for ; delta > 0; delta-- {
		if m.f < len(m.groups[m.g].Files)-1 {
			m.f++
		} else if m.g < len(m.groups)-1 {
			m.g, m.f = m.g+1, 0
		}
	}
	for ; delta < 0; delta++ {
		if m.f > 0 {
			m.f--
		} else if m.g > 0 {
			m.g--
			m.f = len(m.groups[m.g].Files) - 1
		}
	}
}

func (m *Model) action(kind string, all bool) {
	if kind == catalog.ActionMove && m.opts.MoveTo == "" {
		m.say("No move destination: pass --move-to or set dupes.move_to in the config")
		return
	}
	targets := []int{m.g}
	if all {
		targets = targets[:0]
		for i := range m.groups {
			targets = append(targets, i)
		}
	}
	for _, i := range targets {
		c := m.choices[i]
		c.Action = kind
		m.set(i, c)
	}
	if all {
		m.say("%s for all %d groups", describe(kind), len(targets))
	}
}

func describe(kind string) string {
	switch kind {
	case catalog.ActionTrash:
		return "Trash the other copies"
	case catalog.ActionMove:
		return "Move the other copies"
	case catalog.ActionDelete:
		return "Permanently delete the other copies"
	}
	return "Leave untouched"
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tea.KeyMsg:
		// Keys typed in quick succession can arrive as one multi-rune event; handle each.
		if msg.Type == tea.KeyRunes && len(msg.Runes) > 1 && !msg.Paste {
			var model tea.Model = m
			var cmd tea.Cmd
			for _, r := range msg.Runes {
				model, cmd = model.(Model).Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
				if model.(Model).done {
					break
				}
			}
			return model, cmd
		}
		if len(m.groups) == 0 {
			m.done = true
			return m, tea.Quit
		}
		switch {
		case key.Matches(msg, keys.Quit):
			m.done = true
			return m, tea.Quit
		case key.Matches(msg, keys.Apply):
			if !m.anyAction() {
				m.say("Nothing to apply: choose t, m or d for at least one group")
				break
			}
			m.apply, m.done = true, true
			return m, tea.Quit
		case key.Matches(msg, keys.Down):
			m.step(1)
		case key.Matches(msg, keys.Up):
			m.step(-1)
		case key.Matches(msg, keys.NextGroup):
			if m.g < len(m.groups)-1 {
				m.g, m.f = m.g+1, 0
			}
		case key.Matches(msg, keys.PrevGroup):
			if m.g > 0 {
				m.g, m.f = m.g-1, 0
			}
		case key.Matches(msg, keys.PageDown):
			m.step(max(m.listHeight()-2, 1))
		case key.Matches(msg, keys.PageUp):
			m.step(-max(m.listHeight()-2, 1))
		case key.Matches(msg, keys.Top):
			m.g, m.f = 0, 0
		case key.Matches(msg, keys.Bottom):
			m.g = len(m.groups) - 1
			m.f = len(m.groups[m.g].Files) - 1
		case key.Matches(msg, keys.Keep):
			c := m.choices[m.g]
			c.Keep = m.f
			m.set(m.g, c)
		case key.Matches(msg, keys.Trash):
			m.action(catalog.ActionTrash, false)
		case key.Matches(msg, keys.Move):
			m.action(catalog.ActionMove, false)
		case key.Matches(msg, keys.Delete):
			m.action(catalog.ActionDelete, false)
		case key.Matches(msg, keys.Leave):
			m.action("", false)
		case key.Matches(msg, keys.TrashAll):
			m.action(catalog.ActionTrash, true)
		case key.Matches(msg, keys.MoveAll):
			m.action(catalog.ActionMove, true)
		case key.Matches(msg, keys.DeleteAll):
			m.action(catalog.ActionDelete, true)
		case key.Matches(msg, keys.LeaveAll):
			m.action("", true)
		case key.Matches(msg, keys.Open):
			p := m.groups[m.g].Files[m.f].AbsPath
			if m.opts.Open == nil {
				break
			}
			if err := m.opts.Open(p); err != nil {
				m.say("Could not open: %v", err)
			} else {
				m.say("Opened %s", p)
			}
		case key.Matches(msg, keys.Help):
			m.help = !m.help
		}
	}
	m.scroll()
	return m, nil
}

func (m Model) anyAction() bool {
	for _, c := range m.choices {
		if c.Action != "" {
			return true
		}
	}
	return false
}

// listHeight is the number of rows available for groups.
func (m Model) listHeight() int {
	reserved := 4 // status, rule, rule, footer
	if m.help {
		reserved += 6
	}
	return max(m.height-reserved, 3)
}

// cursorLine is the cursor's index in the flattened list (headers, files, blank lines).
func (m Model) cursorLine() int {
	line := 0
	for i := 0; i < m.g; i++ {
		line += len(m.groups[i].Files) + 2
	}
	return line + 1 + m.f
}

func (m *Model) scroll() {
	h := m.listHeight()
	c := m.cursorLine()
	if c-1 < m.offset { // keep the group header visible too
		m.offset = max(c-1-m.f, 0)
	}
	if c >= m.offset+h {
		m.offset = c - h + 1
	}
}

var (
	cursorStyle = lipgloss.NewStyle().Reverse(true)
	keepStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "#1A7F37", Dark: "#56D364"})
)

func actionStyle(kind string) lipgloss.Style {
	switch kind {
	case catalog.ActionTrash:
		return ui.Warn
	case catalog.ActionMove:
		return ui.Accent
	case catalog.ActionDelete:
		return ui.Err.Bold(true)
	}
	return ui.Dim
}

func actionLabel(kind string) string {
	switch kind {
	case catalog.ActionTrash:
		return "trash "
	case catalog.ActionMove:
		return "move  "
	case catalog.ActionDelete:
		return "DELETE"
	}
	return "leave "
}

// Totals summarizes the choices: copies and bytes per action.
type Totals struct {
	Groups map[string]int
	Files  map[string]int
	Bytes  map[string]int64
}

// Summarize counts what the choices would do.
func Summarize(groups []dupes.Group, choices []Choice) Totals {
	t := Totals{Groups: map[string]int{}, Files: map[string]int{}, Bytes: map[string]int64{}}
	for i, g := range groups {
		a := choices[i].Action
		t.Groups[a]++
		t.Files[a] += len(g.Files) - 1
		t.Bytes[a] += g.ReclaimableKeeping(choices[i].Keep)
	}
	return t
}

func (m Model) View() string {
	if m.done {
		return ""
	}
	if len(m.groups) == 0 {
		return "No duplicate groups to review. Press any key.\n"
	}
	var b strings.Builder
	t := Summarize(m.groups, m.choices)
	part := func(kind, label string) string {
		s := fmt.Sprintf("%s %d", label, t.Files[kind])
		if t.Bytes[kind] > 0 {
			s += " (" + ui.Bytes(t.Bytes[kind]) + ")"
		}
		return actionStyle(kind).Render(s)
	}
	title := "Review duplicates"
	if m.groups[0].Similar {
		title = "Review similar files"
	}
	status := fmt.Sprintf("%s  group %d/%d   %s  %s  %s  %s",
		ui.Title.Render(title), m.g+1, len(m.groups),
		part(catalog.ActionTrash, "trash"), part(catalog.ActionMove, "move"),
		part(catalog.ActionDelete, "delete"), ui.Dim.Render(fmt.Sprintf("undecided groups %d", t.Groups[""])))
	b.WriteString(status + "\n")
	b.WriteString(ui.Dim.Render(strings.Repeat("─", max(m.width, 10))) + "\n")

	lines := m.lines()
	h := m.listHeight()
	for i := m.offset; i < min(m.offset+h, len(lines)); i++ {
		b.WriteString(lines[i] + "\n")
	}
	for i := len(lines) - m.offset; i < h; i++ {
		b.WriteString("\n")
	}
	b.WriteString(ui.Dim.Render(strings.Repeat("─", max(m.width, 10))) + "\n")
	if m.help {
		b.WriteString(m.helpView())
	}
	if m.msg != "" && time.Since(m.msgAt) < 8*time.Second {
		b.WriteString(ui.Warn.Render(m.msg))
	} else {
		b.WriteString(ui.Dim.Render("j/k move  space keep  t trash  m move  d delete  s leave  T/M/D/S all  o open  a apply  q quit  ? help"))
	}
	return b.String()
}

func (m Model) helpView() string {
	rows := [][2]string{
		{"j/k, arrows", "move between copies"}, {"] / [", "next / previous group"},
		{"space", "keep the copy under the cursor"}, {"t / m / d", "trash / move / permanently delete the other copies"},
		{"s", "leave this group untouched"}, {"T / M / D / S", "the same, for every group"},
		{"o", "open the copy under the cursor"}, {"a", "apply: you confirm on the next screen; q quits, choices are saved"},
	}
	var b strings.Builder
	for i, r := range rows {
		if i%2 == 0 && i > 0 {
			b.WriteString("\n")
		} else if i%2 == 1 {
			b.WriteString("    ")
		}
		b.WriteString(fmt.Sprintf("%s %-46s", ui.Bold.Render(fmt.Sprintf("%-14s", r[0])), r[1]))
	}
	return b.String() + "\n\n"
}

// lines renders every group; the cursor row is highlighted.
func (m Model) lines() []string {
	var out []string
	for gi, g := range m.groups {
		c := m.choices[gi]
		head := fmt.Sprintf("#%d  %d copies of %s", gi+1, len(g.Files), ui.Bytes(g.Size))
		if g.Similar {
			head = fmt.Sprintf("#%d  %d similar files, %s reclaimable", gi+1, len(g.Files), ui.Bytes(g.ReclaimableKeeping(c.Keep)))
		}
		act := actionStyle(c.Action).Render(describe(c.Action))
		if c.Action == catalog.ActionMove {
			act += ui.Dim.Render(" to " + m.opts.MoveTo)
		}
		out = append(out, ui.Bold.Render(head)+"  "+act)
		for fi, f := range g.Files {
			out = append(out, m.fileLine(gi, fi, f, c))
		}
		out = append(out, "")
	}
	return out
}

func (m Model) fileLine(gi, fi int, f catalog.File, c Choice) string {
	label, style := actionLabel(c.Action), actionStyle(c.Action)
	if fi == c.Keep {
		label, style = "keep  ", keepStyle
	}
	meta := f.Meta
	info := fmt.Sprintf("%s  %9s  %7s  %-5s %10s", time.Unix(0, f.MtimeNS).Format("2006-01-02"),
		ui.Res(meta.Width, meta.Height), ui.Duration(meta.DurationMS), short(meta.VideoCodec, 5), ui.Bitrate(meta.VideoBitrate))
	if g := m.groups[gi]; g.Similar {
		info = fmt.Sprintf("%9s  %-9s  ", ui.Bytes(f.Size), Likeness(g, fi, c.Keep)) + info
	}
	room := max(m.width-len(label)-len(info)-7, 20)
	path := ui.TruncateMiddle(f.AbsPath, room)
	if gi == m.g && fi == m.f {
		// Unstyled text so the reverse-video highlight spans the whole row.
		row := "  " + label + "  " + info + "  " + path
		return cursorStyle.Render(row + strings.Repeat(" ", max(m.width-lipgloss.Width(row), 0)))
	}
	return "  " + style.Render(label) + "  " + ui.Dim.Render(info) + "  " + path
}

// Likeness describes how Files[i] compares with the kept copy in a similar group:
// "identical" for the same content, otherwise the phash distance ("" for the keeper).
func Likeness(g dupes.Group, i, keep int) string {
	switch {
	case i == keep:
		return ""
	case g.Identical(i, keep):
		return "identical"
	}
	return fmt.Sprintf("%d bits", g.Distance(i, keep))
}

func short(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
