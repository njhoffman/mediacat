package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// twoTrashBatches trashes the fixture's backup copy ten days ago (batch 1) and its other
// extra copy now (batch 2).
func twoTrashBatches(t *testing.T) (*harness, []string, string) {
	t.Helper()
	h, paths := dupeFixture(t)
	now := h.now
	h.now = now.Add(-10 * 24 * time.Hour)
	h.ok("dupes", h.dir, "--trash", "--only-under", filepath.Join(h.dir, "backup"), "--execute", "--yes")
	h.now = now
	h.ok("dupes", h.dir, "--trash", "--execute", "--yes")
	return h, paths, filepath.Join(os.Getenv("XDG_DATA_HOME"), "Trash")
}

func TestTrashEmptyOlderThan(t *testing.T) {
	h, _, trash := twoTrashBatches(t)

	if _, errs, code := h.run("trash", h.dir, "--older-than", "7d"); code != 1 || !strings.Contains(errs, "add --empty") {
		t.Errorf("--older-than without --empty: %d %s", code, errs)
	}
	if _, errs, code := h.run("trash", h.dir, "--empty", "--older-than", "soon"); code != 1 || !strings.Contains(errs, "invalid time") {
		t.Errorf("bad age: %d %s", code, errs)
	}
	out, _ := h.ok("trash", h.dir, "--empty", "--older-than", "30d")
	if !strings.Contains(out, "Nothing was trashed before") {
		t.Errorf("nothing that old:\n%s", out)
	}

	h.interactive, h.stdin = true, "delete\n"
	out, errs := h.ok("trash", h.dir, "--empty", "--older-than", "7d")
	if !strings.Contains(errs, "1 items") || !strings.Contains(errs, "trashed before") || !strings.Contains(out, "Done: 1 items removed") {
		t.Fatalf("empty older:\n%s\n%s", out, errs)
	}
	entries, _ := os.ReadDir(filepath.Join(trash, "files"))
	if len(entries) != 1 || entries[0].Name() != "clip (1).mp4" {
		t.Errorf("the recent batch should stay: %v", entries)
	}
	out, _ = h.ok("trash", h.dir)
	if strings.Contains(out, "#1 ") || !strings.Contains(out, "#2 ") || !strings.Contains(out, "1 files") {
		t.Errorf("after emptying batch 1:\n%s", out)
	}
	// The recent batch is still undoable.
	h.interactive = false
	h.ok("undo", "2", "--yes")
}

func TestTrashEmptyEverythingOlderThan(t *testing.T) {
	h, _, trash := twoTrashBatches(t)
	// Other programs' items: one old, one recent, one without a date.
	for name, date := range map[string]string{
		"old.txt":    h.now.Add(-20 * 24 * time.Hour).Format("2006-01-02T15:04:05"),
		"recent.txt": h.now.Add(-time.Hour).Format("2006-01-02T15:04:05"),
		"nodate.txt": "",
	} {
		os.WriteFile(filepath.Join(trash, "files", name), []byte("x"), 0o644)
		info := "[Trash Info]\nPath=/elsewhere/" + name + "\n"
		if date != "" {
			info += "DeletionDate=" + date + "\n"
		}
		os.WriteFile(filepath.Join(trash, "info", name+".trashinfo"), []byte(info), 0o644)
	}
	h.interactive, h.stdin = true, "delete\n"
	out, errs := h.ok("trash", h.dir, "--empty", "--everything", "--older-than", "7d")
	if !strings.Contains(errs, "EVERYTHING trashed before") || !strings.Contains(out, "Done: 2 items removed") {
		t.Fatalf("everything older:\n%s\n%s", out, errs)
	}
	left := map[string]bool{}
	entries, _ := os.ReadDir(filepath.Join(trash, "files"))
	for _, e := range entries {
		left[e.Name()] = true
	}
	if len(left) != 3 || !left["recent.txt"] || !left["nodate.txt"] || !left["clip (1).mp4"] {
		t.Errorf("left in trash: %v", left)
	}
	if _, err := os.Stat(filepath.Join(trash, "info", "old.txt.trashinfo")); err == nil {
		t.Error("the removed item's .trashinfo should go too")
	}
	// mediacat's old entry is recorded as purged, so the listing no longer shows batch 1.
	out, _ = h.ok("trash", h.dir)
	if strings.Contains(out, "#1 ") || !strings.Contains(out, "#2 ") {
		t.Errorf("listing after:\n%s", out)
	}
}
