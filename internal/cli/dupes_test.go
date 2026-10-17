package cli

import (
	"encoding/csv"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/njhoffman/mediacat/internal/testmedia"
)

// dupeFixture creates real media plus two exact copies of the video, and catalogs them.
func dupeFixture(t *testing.T) (*harness, []string) {
	t.Helper()
	h := newHarness(t)
	files := testmedia.Generate(t, h.dir)
	b, _ := os.ReadFile(files.Video)
	var copies []string
	for i, rel := range []string{"backup/clip.mp4", "clip (1).mp4"} {
		p := filepath.Join(h.dir, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, b, 0o644)
		mt := time.Now().Add(time.Duration(i+1) * time.Hour)
		os.Chtimes(p, mt, mt)
		copies = append(copies, p)
	}
	old := time.Now().Add(-24 * time.Hour)
	os.Chtimes(files.Video, old, old) // oldest: the keeper
	h.ok("scan", h.dir)
	return h, append([]string{files.Video}, copies...)
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

func TestDupesReportAndExport(t *testing.T) {
	h, paths := dupeFixture(t)
	out, _ := h.ok("dupes", h.dir)
	for _, want := range []string{"Duplicates", "1 groups", "3 files", "2 extra copies", "keep", "dupe", "--trash"} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q:\n%s", want, out)
		}
	}
	keepLine := ""
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "keep ") {
			keepLine = l
		}
	}
	if !strings.HasSuffix(keepLine, paths[0]) {
		t.Errorf("keeper should be the oldest plain copy: %q", keepLine)
	}

	out, _ = h.ok("dupes", h.dir, "--json")
	var groups []map[string]any
	if err := json.Unmarshal([]byte(out), &groups); err != nil || len(groups) != 1 || groups[0]["keep"] != paths[0] {
		t.Fatalf("json: %v %s", err, out)
	}

	csvPath := filepath.Join(t.TempDir(), "d.csv")
	h.ok("dupes", h.dir, "--export", csvPath)
	f, _ := os.Open(csvPath)
	rows, err := csv.NewReader(f).ReadAll()
	f.Close()
	if err != nil || len(rows) != 4 || rows[0][1] != "role" {
		t.Errorf("csv rows: %v %v", rows, err)
	}
	if _, errs, code := h.run("dupes", h.dir, "--export", filepath.Join(t.TempDir(), "d.txt")); code != 1 || !strings.Contains(errs, ".csv or .json") {
		t.Errorf("bad export ext: %d %s", code, errs)
	}

	out, _ = h.ok("dupes", h.dir, "--depth", "size")
	if !strings.Contains(out, "same size") || !strings.Contains(out, "--depth full") {
		t.Errorf("size depth:\n%s", out)
	}
}

func TestDupesTrashFlow(t *testing.T) {
	h, paths := dupeFixture(t)

	out, _ := h.ok("dupes", h.dir, "--trash")
	if !strings.Contains(out, "DRY RUN: 2 files") || !strings.Contains(out, "would be MOVED TO TRASH") || !strings.Contains(out, "Nothing was changed") {
		t.Errorf("dry run:\n%s", out)
	}
	if !exists(paths[1]) || !exists(paths[2]) {
		t.Fatal("dry run changed files")
	}

	if _, errs, code := h.run("dupes", h.dir, "--trash", "--execute"); code != 1 || !strings.Contains(errs, "--yes") {
		t.Errorf("non-interactive execute without --yes should fail: %s", errs)
	}

	h.interactive, h.stdin = true, "n\n"
	_, errs := h.ok("dupes", h.dir, "--trash", "--execute")
	if !strings.Contains(errs, "will be MOVED TO TRASH") || !strings.Contains(errs, "Cancelled") || !exists(paths[1]) {
		t.Errorf("declined prompt: %s", errs)
	}

	h.stdin = "y\n"
	out, errs = h.ok("dupes", h.dir, "--trash", "--execute")
	if !strings.Contains(out, "Done: 2 files") || !strings.Contains(out, "mediacat undo 1") || exists(paths[1]) || exists(paths[2]) || !exists(paths[0]) {
		t.Fatalf("execute:\n%s\n%s", out, errs)
	}
	trashFiles := filepath.Join(os.Getenv("XDG_DATA_HOME"), "Trash", "files")
	if entries, _ := os.ReadDir(trashFiles); len(entries) != 2 {
		t.Errorf("trash has %d entries", len(entries))
	}

	out, _ = h.ok("history")
	if !strings.Contains(out, "trash") || !strings.Contains(out, "complete") {
		t.Errorf("history:\n%s", out)
	}
	out, _ = h.ok("history", "1")
	if !strings.Contains(out, "Batch #1") || strings.Count(out, "done") != 2 {
		t.Errorf("history 1:\n%s", out)
	}
	out, _ = h.ok("trash", h.dir)
	if !strings.Contains(out, "put there by mediacat") || !strings.Contains(out, "2 files") {
		t.Errorf("trash:\n%s", out)
	}

	// The dupes are gone from the listing; undo brings them back.
	out, _ = h.ok("dupes", h.dir)
	if !strings.Contains(out, "No duplicates found") {
		t.Errorf("after trash:\n%s", out)
	}
	h.ok("undo", "1", "--yes")
	if !exists(paths[1]) || !exists(paths[2]) {
		t.Fatal("undo did not restore")
	}
	if out, _ = h.ok("dupes", h.dir); !strings.Contains(out, "2 extra copies") {
		t.Errorf("after undo:\n%s", out)
	}
	if _, errs, code := h.run("undo", "1", "--yes"); code != 1 || !strings.Contains(errs, "nothing left") {
		t.Errorf("second undo: %s", errs)
	}
}

func TestEmptyTrash(t *testing.T) {
	h, paths := dupeFixture(t)
	h.ok("dupes", h.dir, "--trash", "--execute", "--yes")
	// Something else's trash entry must survive a plain --empty.
	other := filepath.Join(os.Getenv("XDG_DATA_HOME"), "Trash", "files", "not-ours.txt")
	os.WriteFile(other, []byte("x"), 0o644)

	if _, errs, code := h.run("trash", h.dir, "--empty"); code != 1 || !strings.Contains(errs, "terminal") {
		t.Errorf("non-interactive empty: %s", errs)
	}
	h.interactive, h.stdin = true, "yes\n"
	if _, errs := h.ok("trash", h.dir, "--empty"); !strings.Contains(errs, "Cancelled") {
		t.Errorf("only the exact word confirms: %s", errs)
	}
	h.stdin = "delete\n"
	out, errs := h.ok("trash", h.dir, "--empty")
	if !strings.Contains(errs, "PERMANENTLY DELETED") || !strings.Contains(out, "2 items removed") || !exists(other) {
		t.Errorf("empty:\n%s\n%s", out, errs)
	}
	if _, errs, code := h.run("undo", "1", "--yes"); code != 1 {
		t.Errorf("undo after purge should fail: %s", errs)
	}
	_ = paths
}

func TestDupesDelete(t *testing.T) {
	h, paths := dupeFixture(t)
	if _, errs, code := h.run("dupes", h.dir, "--delete", "--execute", "--yes"); code != 1 || !strings.Contains(errs, "terminal") {
		t.Errorf("--yes must not bypass delete confirmation: %s", errs)
	}
	h.interactive, h.stdin = true, "y\n"
	if _, errs := h.ok("dupes", h.dir, "--delete", "--execute"); !strings.Contains(errs, "PERMANENTLY DELETED") || !strings.Contains(errs, "Cancelled") {
		t.Errorf("y is not enough for delete: %s", errs)
	}
	h.stdin = "delete\n"
	out, _ := h.ok("dupes", h.dir, "--delete", "--execute")
	if !strings.Contains(out, "permanently deleted") || exists(paths[1]) || exists(paths[2]) || !exists(paths[0]) {
		t.Errorf("delete:\n%s", out)
	}
	if strings.Contains(out, "Undo with") {
		t.Error("delete should not offer undo")
	}
}

func TestDupesMove(t *testing.T) {
	h, paths := dupeFixture(t)
	hold := filepath.Join(h.dir, "trashbox")
	out, _ := h.ok("dupes", h.dir, "--move-to", hold, "--execute", "--yes")
	if !strings.Contains(out, "Done: 2 files") || !exists(filepath.Join(hold, "backup/clip.mp4")) || exists(paths[1]) {
		t.Errorf("move:\n%s", out)
	}
	// trashbox is excluded from scans, so the moved copies are not found again.
	h.ok("scan", h.dir)
	if out, _ := h.ok("dupes", h.dir); !strings.Contains(out, "No duplicates found") {
		t.Errorf("moved copies reappeared:\n%s", out)
	}
}

func TestDupesStashReminder(t *testing.T) {
	h, _ := dupeFixture(t)
	os.WriteFile(h.stashCfg, []byte("stash:\n  - path: "+h.dir+"/backup\n"), 0o644)
	out, _ := h.ok("dupes", h.dir, "--trash", "--execute", "--yes")
	if !strings.Contains(out, "Stash: 1 of these files") {
		t.Errorf("stash reminder:\n%s", out)
	}
}

func TestDupesFlagErrors(t *testing.T) {
	h := newHarness(t)
	cases := [][]string{
		{"dupes", "--trash", "--delete"},
		{"dupes", "--execute"},
		{"dupes", "--depth", "size", "--trash"},
		{"dupes", "--json", "--trash"},
		{"dupes", "--depth", "deep"},
		{"dupes", "--keep", "largest"},
		{"trash", "--everything"},
		{"undo", "x"},
		{"undo", "99", "--yes"},
		{"history", "99"},
	}
	for _, args := range cases {
		if _, errs, code := h.run(args...); code != 1 || !strings.Contains(errs, "error:") {
			t.Errorf("%v: code=%d %s", args, code, errs)
		}
	}
}
