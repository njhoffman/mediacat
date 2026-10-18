package review

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/njhoffman/mediacat/internal/catalog"
	"github.com/njhoffman/mediacat/internal/dupes"
	"github.com/njhoffman/mediacat/internal/ui"
)

func groups(sizes ...int) []dupes.Group {
	var out []dupes.Group
	for gi, n := range sizes {
		g := dupes.Group{Key: fmt.Sprint("h", gi), Size: 1 << 20, Confirmed: true}
		for i := range n {
			g.Files = append(g.Files, catalog.File{AbsPath: fmt.Sprintf("/lib/g%d/copy%d.mp4", gi, i), Size: 1 << 20})
		}
		out = append(out, g)
	}
	return out
}

func press(m Model, keys ...string) Model {
	for _, k := range keys {
		var msg tea.KeyMsg
		switch k {
		case "down", "up", "pgdown", "pgup", "home", "end", "tab", "esc", "enter":
			msg = tea.KeyMsg{Type: map[string]tea.KeyType{"down": tea.KeyDown, "up": tea.KeyUp, "pgdown": tea.KeyPgDown,
				"pgup": tea.KeyPgUp, "home": tea.KeyHome, "end": tea.KeyEnd, "tab": tea.KeyTab, "esc": tea.KeyEsc, "enter": tea.KeyEnter}[k]}
		case " ":
			msg = tea.KeyMsg{Type: tea.KeySpace, Runes: []rune(" ")}
		default:
			msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
		}
		next, _ := m.Update(msg)
		m = next.(Model)
	}
	return m
}

func fresh(gs []dupes.Group) []Choice { return make([]Choice, len(gs)) }

func TestNavigationAndKeep(t *testing.T) {
	ui.DisableColor()
	gs := groups(2, 3)
	m := New(gs, fresh(gs), Options{})
	m = press(m, "j", "j") // second file of group 0, then first of group 1
	if m.g != 1 || m.f != 0 {
		t.Fatalf("cursor = %d/%d", m.g, m.f)
	}
	m = press(m, "j", " ")
	if m.choices[1].Keep != 1 {
		t.Errorf("keep = %d", m.choices[1].Keep)
	}
	m = press(m, "k", "k", "k", "k") // clamps at the top
	if m.g != 0 || m.f != 0 {
		t.Errorf("top clamp: %d/%d", m.g, m.f)
	}
	m = press(m, "G")
	if m.g != 1 || m.f != 2 {
		t.Errorf("bottom: %d/%d", m.g, m.f)
	}
	m = press(m, "[", "]")
	if m.g != 1 || m.f != 0 {
		t.Errorf("group jumps: %d/%d", m.g, m.f)
	}
}

func TestActionsAndSaving(t *testing.T) {
	gs := groups(2, 2, 2)
	saved := map[int]Choice{}
	m := New(gs, fresh(gs), Options{Save: func(g int, c Choice) { saved[g] = c }})
	m = press(m, "t")
	if m.choices[0].Action != catalog.ActionTrash || saved[0].Action != catalog.ActionTrash {
		t.Errorf("trash: %+v saved %+v", m.choices[0], saved)
	}
	m = press(m, "m")
	if m.choices[0].Action != catalog.ActionTrash || !strings.Contains(m.msg, "move destination") {
		t.Errorf("move without destination should be refused: %+v %q", m.choices[0], m.msg)
	}
	m = press(m, "D")
	for i, c := range m.choices {
		if c.Action != catalog.ActionDelete {
			t.Errorf("group %d: %+v", i, c)
		}
	}
	m = press(m, "]", "s")
	if m.choices[1].Action != "" || m.choices[0].Action != catalog.ActionDelete {
		t.Errorf("leave: %+v", m.choices)
	}
	n := len(saved)
	m = press(m, "s") // no change, no save
	if len(saved) != n {
		t.Error("unchanged choice should not be saved again")
	}
	tot := Summarize(gs, m.choices)
	if tot.Files[catalog.ActionDelete] != 2 || tot.Bytes[catalog.ActionDelete] != 2<<20 || tot.Groups[""] != 1 {
		t.Errorf("totals: %+v", tot)
	}

	mv := New(gs, fresh(gs), Options{MoveTo: "/hold"})
	mv = press(mv, "M")
	if mv.choices[2].Action != catalog.ActionMove || !strings.Contains(mv.View(), "to /hold") {
		t.Errorf("move all: %+v", mv.choices)
	}
}

func TestApplyAndQuit(t *testing.T) {
	gs := groups(2)
	m := press(New(gs, fresh(gs), Options{}), "a")
	if m.Applied() || m.done || !strings.Contains(m.msg, "Nothing to apply") {
		t.Errorf("apply with nothing chosen: applied=%v done=%v %q", m.Applied(), m.done, m.msg)
	}
	m = press(m, "t", "a")
	if !m.Applied() || !m.done {
		t.Error("apply after choosing should finish")
	}
	q := press(New(gs, fresh(gs), Options{}), "t", "q")
	if q.Applied() || !q.done || q.Choices()[0].Action != catalog.ActionTrash {
		t.Error("quit keeps choices but does not apply")
	}
}

func TestOpen(t *testing.T) {
	gs := groups(2)
	var opened string
	m := press(New(gs, fresh(gs), Options{Open: func(p string) error { opened = p; return nil }}), "j", "o")
	if opened != "/lib/g0/copy1.mp4" {
		t.Errorf("opened %q", opened)
	}
	m = press(New(gs, fresh(gs), Options{Open: func(string) error { return errors.New("no player") }}), "o")
	if !strings.Contains(m.msg, "no player") {
		t.Errorf("open error: %q", m.msg)
	}
}

func TestViewAndScrolling(t *testing.T) {
	ui.DisableColor()
	gs := groups(2, 2, 2, 2, 2, 2, 2, 2, 2, 2)
	m := New(gs, fresh(gs), Options{})
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 12})
	m = next.(Model)
	m = press(m, "t")
	v := m.View()
	if !strings.Contains(v, "group 1/10") || !strings.Contains(v, "trash 1 (1.0 MiB)") || !strings.Contains(v, "keep") ||
		!strings.Contains(v, "Trash the other copies") {
		t.Errorf("view:\n%s", v)
	}
	if lines := strings.Split(v, "\n"); len(lines) > 12 {
		t.Errorf("view taller than the window: %d lines", len(lines))
	}
	m = press(m, "G")
	v = m.View()
	if !strings.Contains(v, "group 10/10") || !strings.Contains(v, "/lib/g9/copy1.mp4") || strings.Contains(v, "/lib/g0/") {
		t.Errorf("scrolled view:\n%s", v)
	}
	m = press(m, "?")
	if !strings.Contains(m.View(), "permanently delete") {
		t.Error("help view")
	}
	empty := New(nil, nil, Options{})
	if !strings.Contains(empty.View(), "No duplicate groups") {
		t.Error("empty view")
	}
}

func TestBurstOfKeys(t *testing.T) {
	gs := groups(2, 2)
	m := New(gs, fresh(gs), Options{})
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("jt]d")})
	m = next.(Model)
	if m.choices[0].Action != catalog.ActionTrash || m.choices[1].Action != catalog.ActionDelete || m.f != 0 || m.g != 1 {
		t.Errorf("burst: %+v cursor %d/%d", m.choices, m.g, m.f)
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("qt")})
	if m = next.(Model); !m.done || m.choices[1].Action != catalog.ActionDelete {
		t.Error("keys after q must be ignored")
	}
}

func TestSimilarGroups(t *testing.T) {
	ui.DisableColor()
	g := dupes.Group{Key: "similar:x", Confirmed: true, Similar: true, Files: []catalog.File{
		{AbsPath: "/lib/a.mp4", Size: 100, FullHash: "h1", PHash: catalog.PHash{Value: 0, Source: catalog.PHashMediacat}},
		{AbsPath: "/lib/b.mp4", Size: 300, FullHash: "h2", PHash: catalog.PHash{Value: 0b111, Source: catalog.PHashStash}},
		{AbsPath: "/lib/c.mp4", Size: 100, FullHash: "h1", PHash: catalog.PHash{Value: 0, Source: catalog.PHashMediacat}},
	}}
	gs := []dupes.Group{g}
	m := New(gs, []Choice{{Keep: 1}}, Options{})
	v := m.View()
	for _, want := range []string{"Review similar files", "3 similar files, 200 B reclaimable", "3 bits"} {
		if !strings.Contains(v, want) {
			t.Errorf("view missing %q:\n%s", want, v)
		}
	}
	// Keeping a.mp4 instead: c.mp4 is then byte-identical to the keeper, b.mp4 3 bits away.
	m = press(m, "g", " ", "t")
	v = m.View()
	if !strings.Contains(v, "identical") || !strings.Contains(v, "400 B reclaimable") {
		t.Errorf("after keeping a.mp4:\n%s", v)
	}
	if tt := Summarize(gs, m.Choices()); tt.Bytes[catalog.ActionTrash] != 400 {
		t.Errorf("summary bytes %d, want the sizes of the copies not kept", tt.Bytes[catalog.ActionTrash])
	}
}
