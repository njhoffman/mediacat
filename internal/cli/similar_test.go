package cli

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/njhoffman/mediacat/internal/phash"
	"github.com/njhoffman/mediacat/internal/testmedia"
)

func similarFixture(t *testing.T) (*harness, testmedia.Similar) {
	t.Helper()
	h := newHarness(t)
	s := testmedia.GenerateSimilar(t, h.dir)
	h.ok("scan", h.dir)
	return h, s
}

func TestDupesSimilar(t *testing.T) {
	h, s := similarFixture(t)
	out, _ := h.ok("dupes", h.dir, "--similar")
	for _, want := range []string{"Similar files", "high, distance <= 4", "2 groups", "similar, not identical",
		"images smaller than 64px were not compared", "--review"} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q:\n%s", want, out)
		}
	}
	keeps := map[string]bool{}
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "keep ") {
			keeps[l[strings.LastIndex(l, " ")+1:]] = true
		}
		if strings.Contains(l, "other.mp4") || strings.Contains(l, "fractal.png") {
			t.Errorf("different content grouped: %q", l)
		}
	}
	if !keeps[s.VideoHigh] || !keeps[s.Image] {
		t.Errorf("keepers should be the higher resolution copies: %v\n%s", keeps, out)
	}
	if !strings.Contains(out, "computed now") || strings.Contains(out, " 0 computed now") {
		t.Errorf("first run should compute phashes:\n%s", out)
	}

	// Cached: the second run computes nothing.
	out, _ = h.ok("dupes", h.dir, "--similar")
	if !strings.Contains(out, " 0 computed now") {
		t.Errorf("second run:\n%s", out)
	}

	out, _ = h.ok("dupes", h.dir, "--similar", "--json")
	var groups []map[string]any
	if err := json.Unmarshal([]byte(out), &groups); err != nil || len(groups) != 2 || groups[0]["similar"] != true {
		t.Fatalf("json: %v\n%s", err, out)
	}
	for _, f := range groups[0]["files"].([]any) {
		row := f.(map[string]any)
		if len(row["phash"].(string)) != 16 || row["distance"] == nil {
			t.Errorf("json row: %v", row)
		}
	}

	out, _ = h.ok("dupes", h.dir, "--similar=exact", "--type", "video")
	if !strings.Contains(out, "exact, distance <= 0") {
		t.Errorf("exact:\n%s", out)
	}

	out, _ = h.ok("list", h.dir, "--columns", "name,phash", "--type", "video")
	if !strings.Contains(out, "PHASH") || strings.Count(out, "\n") < 4 {
		t.Errorf("phash column:\n%s", out)
	}
	out, _ = h.ok("info", s.Video)
	if !strings.Contains(out, "computed by mediacat") {
		t.Errorf("info:\n%s", out)
	}
	out, _ = h.ok("info", s.ImageTiny)
	if strings.Contains(out, "phash") {
		t.Errorf("tiny images are skipped before hashing:\n%s", out)
	}
}

func TestDupesSimilarTrash(t *testing.T) {
	h, s := similarFixture(t)
	out, _ := h.ok("dupes", h.dir, "--similar", "--trash", "--type", "video")
	if !strings.Contains(out, "DRY RUN: 1 files") || !strings.Contains(out, "SIMILAR files, not identical") {
		t.Errorf("dry run:\n%s", out)
	}
	out, _ = h.ok("dupes", h.dir, "--similar", "--trash", "--type", "video", "--execute", "--yes")
	if !strings.Contains(out, "Done: 1 files") || exists(s.Video) || !exists(s.VideoHigh) {
		t.Errorf("trash:\n%s", out)
	}
	h.ok("undo", "1", "--yes")
	if !exists(s.Video) {
		t.Error("undo did not restore")
	}
}

func TestDupesSimilarFlagErrors(t *testing.T) {
	h := newHarness(t)
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"dupes", "--similar", "--delete"}, "--review"},
		{[]string{"dupes", "--similar", "--depth", "full"}, "--depth"},
		{[]string{"dupes", "--similar", "--type", "audio"}, "audio"},
		{[]string{"dupes", "--similar=close"}, "unknown similarity"},
	} {
		if _, errs, code := h.run(c.args...); code != 1 || !strings.Contains(errs, c.want) {
			t.Errorf("%v: code=%d %s", c.args, code, errs)
		}
	}
}

// TestDupesSimilarUsesStash checks that a Stash phash is used while Stash's record matches
// the file, and that Stash's database is left untouched.
func TestDupesSimilarUsesStash(t *testing.T) {
	h, s := similarFixture(t)
	want, err := phash.Hasher{}.Video(t.Context(), s.Video, 10)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(s.Video)
	dbPath := filepath.Join(filepath.Dir(h.stashCfg), "stash-go.sqlite")
	os.WriteFile(h.stashCfg, []byte("database: "+dbPath+"\nstash:\n  - path: "+h.dir+"\n"), 0o644)
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"CREATE TABLE folders (id integer primary key, path varchar(255))",
		"CREATE TABLE files (id integer primary key, basename varchar(255), zip_file_id integer, parent_folder_id integer, size integer, mod_time datetime)",
		"CREATE TABLE video_files (file_id integer primary key, duration float)",
		"CREATE TABLE files_fingerprints (file_id integer, type varchar(255), fingerprint blob)",
		fmt.Sprintf("INSERT INTO folders VALUES (1, '%s')", h.dir),
		fmt.Sprintf("INSERT INTO files VALUES (1, 'movie.mp4', NULL, 1, %d, '%s')", st.Size(), st.ModTime().Format(time.RFC3339)),
		"INSERT INTO video_files VALUES (1, 10)",
		fmt.Sprintf("INSERT INTO files_fingerprints VALUES (1, 'phash', %d)", int64(want)),
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	before, _ := os.ReadFile(dbPath)

	out, _ := h.ok("dupes", h.dir, "--similar", "--type", "video")
	if !strings.Contains(out, "1 phashes from Stash") || !strings.Contains(out, "1 groups") {
		t.Errorf("stash phash not used:\n%s", out)
	}
	if out, _ := h.ok("info", s.Video); !strings.Contains(out, "from Stash") {
		t.Errorf("info:\n%s", out)
	}
	if after, _ := os.ReadFile(dbPath); string(after) != string(before) {
		t.Error("Stash's database was modified")
	}
}
