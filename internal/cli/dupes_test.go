package cli

import (
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
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

func TestListMarksDuplicates(t *testing.T) {
	h, paths := dupeFixture(t)
	out, _ := h.ok("list", h.dir)
	if strings.Contains(out, dupMarker) {
		t.Errorf("no duplicates are known before dupes runs:\n%s", out)
	}
	h.ok("dupes", h.dir)
	out, _ = h.ok("list", h.dir)
	marked := 0
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, dupMarker) {
			marked++
			if !strings.Contains(l, "clip") {
				t.Errorf("wrong file marked: %q", l)
			}
		}
	}
	if marked != 3 {
		t.Errorf("marked %d files, want 3:\n%s", marked, out)
	}
	out, _ = h.ok("list", h.dir, "--duplicates", "--sort", "-dup,name", "--columns", "dup,name")
	if lines := strings.Split(strings.TrimSpace(out), "\n"); len(lines) != 4 || !strings.HasPrefix(lines[1], dupMarker+"  clip (1).mp4") {
		t.Errorf("--duplicates:\n%s", out)
	}
	_ = paths
}

func TestDupesOnlyUnder(t *testing.T) {
	h, paths := dupeFixture(t)
	backup := filepath.Join(h.dir, "backup")

	if _, errs, code := h.run("dupes", h.dir, "--only-under", backup); code != 1 || !strings.Contains(errs, "limits an action") {
		t.Errorf("--only-under without an action: %d %s", code, errs)
	}
	out, _ := h.ok("dupes", h.dir, "--trash", "--only-under", backup)
	if !strings.Contains(out, "DRY RUN: 1 files") || !strings.Contains(out, "1 other copies") || !strings.Contains(out, "outside --only-under") {
		t.Errorf("dry run:\n%s", out)
	}
	out, _ = h.ok("dupes", h.dir, "--trash", "--only-under", filepath.Join(h.dir, "nowhere"))
	if !strings.Contains(out, "Nothing to do") || !strings.Contains(out, "2 other copies") {
		t.Errorf("nothing under the directory:\n%s", out)
	}
	out, _ = h.ok("dupes", h.dir, "--trash", "--only-under", backup, "--execute", "--yes")
	if !strings.Contains(out, "Done: 1 files") || exists(paths[1]) || !exists(paths[0]) || !exists(paths[2]) {
		t.Fatalf("execute:\n%s", out)
	}
}

func TestDupesKeepAllPreferred(t *testing.T) {
	h, paths := dupeFixture(t)
	b, _ := os.ReadFile(paths[0])
	second := filepath.Join(h.dir, "backup", "b2", "clip.mp4")
	os.MkdirAll(filepath.Dir(second), 0o755)
	os.WriteFile(second, b, 0o644)
	h.ok("scan", h.dir)

	if _, errs, code := h.run("dupes", h.dir, "--keep-all-preferred"); code != 1 || !strings.Contains(errs, "limits an action") {
		t.Errorf("without an action: %d %s", code, errs)
	}
	if _, errs, code := h.run("dupes", h.dir, "--trash", "--keep-all-preferred", "--prefer-path", ""); code != 1 || !strings.Contains(errs, "needs preferred path patterns") {
		t.Errorf("no patterns: %d %s", code, errs)
	}
	out, _ := h.ok("dupes", h.dir, "--trash", "--keep-all-preferred", "--prefer-path", "/backup/")
	if !strings.Contains(out, "DRY RUN: 2 files") || !strings.Contains(out, "1 other copies") || !strings.Contains(out, "--keep-all-preferred") {
		t.Errorf("dry run:\n%s", out)
	}
	// Combined with --only-under, both guards apply.
	out, _ = h.ok("dupes", h.dir, "--trash", "--keep-all-preferred", "--prefer-path", "/backup/", "--only-under", filepath.Join(h.dir, "nowhere"))
	if !strings.Contains(out, "Nothing to do") || !strings.Contains(out, "outside --only-under") {
		t.Errorf("both guards:\n%s", out)
	}
	h.ok("dupes", h.dir, "--trash", "--keep-all-preferred", "--prefer-path", "/backup/", "--execute", "--yes")
	if exists(paths[0]) || exists(paths[2]) || !exists(paths[1]) || !exists(second) {
		t.Errorf("wrong files trashed: original %v, copy(1) %v, backup %v, backup/b2 %v",
			exists(paths[0]), exists(paths[2]), exists(paths[1]), exists(second))
	}
}

// fakeStash writes a Stash config and a database with the part of Stash's schema mediacat
// reads: primary files belong to scenes, tracked ones are in Stash without a scene.
func fakeStash(t *testing.T, h *harness, primary, tracked []string) string {
	t.Helper()
	dbPath := filepath.Join(filepath.Dir(h.stashCfg), "stash-go.sqlite")
	os.WriteFile(h.stashCfg, []byte("database: "+dbPath+"\nstash:\n  - path: "+h.dir+"\n"), 0o644)
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	qs := []string{
		"CREATE TABLE folders (id integer primary key, path varchar(255))",
		"CREATE TABLE files (id integer primary key, basename varchar(255), zip_file_id integer, parent_folder_id integer, size integer, mod_time datetime)",
		"CREATE TABLE video_files (file_id integer primary key, duration float)",
		"CREATE TABLE files_fingerprints (file_id integer, type varchar(255), fingerprint blob)",
		`CREATE TABLE scenes_files (scene_id integer, file_id integer, "primary" boolean)`,
		`CREATE TABLE images_files (image_id integer, file_id integer, "primary" boolean)`,
	}
	folders := map[string]int{}
	id := 0
	for _, list := range [][]string{primary, tracked} {
		for _, p := range list {
			dir := filepath.Dir(p)
			if folders[dir] == 0 {
				folders[dir] = len(folders) + 1
				qs = append(qs, fmt.Sprintf("INSERT INTO folders VALUES (%d, '%s')", folders[dir], dir))
			}
			id++
			qs = append(qs, fmt.Sprintf("INSERT INTO files VALUES (%d, '%s', NULL, %d, 1, '2025-01-02T03:04:05Z')", id, filepath.Base(p), folders[dir]))
			if len(primary) > 0 && id <= len(primary) {
				qs = append(qs, fmt.Sprintf("INSERT INTO scenes_files VALUES (%d, %d, 1)", id, id))
			}
		}
	}
	for _, q := range qs {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	return dbPath
}

func TestDupesStash(t *testing.T) {
	h, paths := dupeFixture(t)
	dbPath := fakeStash(t, h, []string{paths[2]}, []string{paths[1]})

	// The stash rule keeps the scene's file, though it has a copy marker and is newest.
	out, _ := h.ok("dupes", h.dir)
	keepLine := ""
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "keep ") {
			keepLine = l
		}
	}
	if !strings.HasSuffix(keepLine, paths[2]) {
		t.Errorf("keeper should be Stash's primary file:\n%s", out)
	}

	// Rules without stash keep another copy, but the guard spares the primary file.
	out, _ = h.ok("dupes", h.dir, "--keep", "oldest", "--trash")
	if !strings.Contains(out, "DRY RUN: 1 files") || !strings.Contains(out, "1 other copies") || !strings.Contains(out, "Stash's primary file") {
		t.Errorf("guard:\n%s", out)
	}
	h.ok("dupes", h.dir, "--keep", "oldest", "--trash", "--execute", "--yes")
	if !exists(paths[0]) || exists(paths[1]) || !exists(paths[2]) {
		t.Fatalf("wrong files trashed: %v %v %v", exists(paths[0]), exists(paths[1]), exists(paths[2]))
	}
	h.ok("undo", "1", "--yes")

	// A database that cannot be read stops actions but not reports.
	os.WriteFile(dbPath, []byte("not a database"), 0o644)
	if _, errs, code := h.run("dupes", h.dir, "--trash"); code != 1 || !strings.Contains(errs, "cannot read Stash's database") {
		t.Errorf("unreadable database with an action: %d %s", code, errs)
	}
	if out, errs := h.ok("dupes", h.dir); !strings.Contains(errs, "not using Stash's database") || !strings.Contains(out, "2 extra copies") {
		t.Errorf("unreadable database, report:\n%s\n%s", out, errs)
	}
	if out, _ := h.ok("--stash-config", "", "dupes", h.dir, "--trash"); !strings.Contains(out, "DRY RUN: 2 files") {
		t.Errorf("acting without Stash:\n%s", out)
	}
}
