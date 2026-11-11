package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestListCompletions(t *testing.T) {
	values := []string{"avoid\tkeep outside", "oldest", "paths", "prefer"}
	cases := []struct {
		prefixes []string
		in       string
		want     []string
	}{
		{nil, "", []string{"avoid\tkeep outside", "oldest", "paths", "prefer"}},
		{nil, "p", []string{"paths", "prefer"}},
		{nil, "prefer,", []string{"prefer,avoid\tkeep outside", "prefer,oldest", "prefer,paths"}},
		{nil, "prefer,avoid,o", []string{"prefer,avoid,oldest"}},
		{nil, "x", nil},
		{[]string{"-"}, "-ol", []string{"-oldest"}},
		{[]string{"-"}, "-oldest,p", []string{"-oldest,paths", "-oldest,prefer"}},
		{[]string{"+", "-"}, "+paths,-pr", []string{"+paths,-prefer"}},
	}
	for _, c := range cases {
		if got := listCompletions(values, c.prefixes, c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("listCompletions(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// complete runs cobra's hidden completion command and returns the candidates and the
// directive line.
func (h *harness) complete(args ...string) ([]string, string) {
	h.t.Helper()
	out, _ := h.ok(append([]string{"__complete"}, args...)...)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var names []string
	for _, l := range lines[:len(lines)-1] {
		name, _, _ := strings.Cut(l, "\t")
		names = append(names, name)
	}
	return names, lines[len(lines)-1]
}

func TestFlagCompletion(t *testing.T) {
	h := newHarness(t)
	cases := []struct {
		args []string
		want []string
	}{
		{[]string{"dupes", "--depth", ""}, []string{"full", "partial", "size"}},
		{[]string{"dupes", "--keep", "prefer,a"}, []string{"prefer,avoid"}},
		{[]string{"dupes", "--similar=h"}, []string{"high"}},
		{[]string{"list", "--type", ""}, []string{"video", "audio", "image"}},
		{[]string{"check", "--type", "i"}, []string{"image"}},
		{[]string{"list", "--sort=-dur"}, []string{"-duration"}},
		{[]string{"list", "--columns", "+vb"}, []string{"+vbitrate"}},
		{[]string{"list", "--check", "ok,da"}, []string{"ok,damaged"}},
	}
	for _, c := range cases {
		got, _ := h.complete(c.args...)
		// cobra filters single values by prefix itself only for ValidArgs, so compare the
		// candidates that start with what was typed.
		var match []string
		typed := c.args[len(c.args)-1]
		if _, v, ok := strings.Cut(typed, "="); ok {
			typed = v
		}
		for _, g := range got {
			if strings.HasPrefix(g, typed) {
				match = append(match, g)
			}
		}
		if !reflect.DeepEqual(match, c.want) {
			t.Errorf("%v: got %q, want %q", c.args, match, c.want)
		}
	}

	// Directory arguments and flags ask the shell for directories (directive 16).
	for _, args := range [][]string{{"scan", ""}, {"dupes", ""}, {"dupes", "--only-under", ""}, {"man", ""}} {
		if _, d := h.complete(args...); d != ":16" {
			t.Errorf("%v: directive %s, want directories", args, d)
		}
	}
	// Free-text flags offer no file names (directive 4).
	for _, args := range [][]string{{"list", "--min-size", ""}, {"list", "--name", ""}, {"dupes", "--prefer-path", ""}} {
		if got, d := h.complete(args...); d != ":4" || len(got) != 0 {
			t.Errorf("%v: %q %s, want no completions", args, got, d)
		}
	}
	// No batches yet: nothing to offer for undo.
	if got, _ := h.complete("undo", ""); len(got) != 0 {
		t.Errorf("undo with no batches: %q", got)
	}
}

func TestManPages(t *testing.T) {
	h := newHarness(t)
	dir := filepath.Join(t.TempDir(), "man1")
	h.ok("man", dir)
	for _, name := range []string{"mediacat.1", "mediacat-dupes.1", "mediacat-list.1", "mediacat-undo.1"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("missing %s", name)
		}
	}
	b, err := os.ReadFile(filepath.Join(dir, "mediacat-dupes.1"))
	if err != nil {
		t.Fatal(err)
	}
	page := string(b)
	for _, want := range []string{`.TH "MEDIACAT-DUPES" "1"`, "only-under", ".EX", "shortest  keep the shortest path"} {
		if !strings.Contains(page, want) {
			t.Errorf("dupes man page missing %q", want)
		}
	}
	if strings.Contains(page, "Auto generated") {
		t.Error("man page carries the generator footer")
	}
	if out, _ := h.ok("--help"); strings.Contains(out, "  man ") {
		t.Errorf("man command should be hidden:\n%s", out)
	}
}

func TestManMarkdown(t *testing.T) {
	in := "Rules:\n  a  first\n  b  second\nAfter."
	want := "Rules:\n\n      a  first\n      b  second\n\nAfter."
	if got := manMarkdown(in); got != want {
		t.Errorf("manMarkdown:\n%q\nwant\n%q", got, want)
	}
}
