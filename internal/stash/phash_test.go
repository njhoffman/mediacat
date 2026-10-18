package stash

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeDB builds the part of Stash's schema mediacat reads.
func fakeDB(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		"CREATE TABLE folders (id integer primary key, path varchar(255) NOT NULL)",
		"CREATE TABLE files (id integer primary key, basename varchar(255), zip_file_id integer, parent_folder_id integer, size integer, mod_time datetime)",
		"CREATE TABLE video_files (file_id integer primary key, duration float)",
		"CREATE TABLE files_fingerprints (file_id integer, type varchar(255), fingerprint blob)",
		"INSERT INTO folders VALUES (1, '/lib/Sorted'), (2, '/lib/Sorted/a b')",
		"INSERT INTO files VALUES (1, 'x.mp4', NULL, 1, 100, '2024-08-20T14:14:36-05:00'), (2, 'y.mp4', NULL, 2, 200, '2025-01-02T03:04:05Z'), (3, 'pic.jpg', NULL, 1, 5, '2025-01-02T03:04:05Z'), (4, 'z.mp4', NULL, 1, 7, '2025-01-02T03:04:05Z')",
		"INSERT INTO video_files VALUES (1, 10.5), (2, 20), (4, 3)",
		"INSERT INTO files_fingerprints VALUES (1, 'phash', -6659840245270445659), (2, 'phash', 42), (3, 'phash', 9), (1, 'oshash', 'abc'), (4, 'md5', 'x')",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

func TestPHashes(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "stash-go.sqlite")
	fakeDB(t, dbPath)
	before, _ := os.ReadFile(dbPath)

	got, err := PHashes(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 video phashes, got %v", got)
	}
	x := got["/lib/Sorted/x.mp4"]
	if x.PHash != 0xa3937e950fce01a5 || x.Size != 100 {
		t.Errorf("x = %+v (%x)", x, x.PHash)
	}
	mod := time.Date(2024, 8, 20, 19, 14, 36, 0, time.UTC)
	if !x.Matches(100, mod.UnixNano()+999_999_999) || x.Matches(100, mod.UnixNano()+int64(time.Second)) || x.Matches(101, mod.UnixNano()) {
		t.Error("Matches compares size and whole seconds")
	}
	if got["/lib/Sorted/a b/y.mp4"].PHash != 42 {
		t.Errorf("y = %+v", got["/lib/Sorted/a b/y.mp4"])
	}
	if after, _ := os.ReadFile(dbPath); string(after) != string(before) {
		t.Error("the Stash database was modified")
	}
	if _, err := PHashes(filepath.Join(dir, "none.sqlite")); err == nil {
		t.Error("missing database should fail")
	}
}

func TestDatabase(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yml")
	for _, c := range []struct{ yaml, want string }{
		{"database: /x/stash.sqlite\n", "/x/stash.sqlite"},
		{"database: rel.sqlite\n", filepath.Join(dir, "rel.sqlite")},
		{"stash: []\n", filepath.Join(dir, "stash-go.sqlite")},
	} {
		os.WriteFile(cfg, []byte(c.yaml), 0o644)
		if got, err := Database(cfg); err != nil || got != c.want {
			t.Errorf("%q: %q %v", c.yaml, got, err)
		}
	}
	if got, err := Database(filepath.Join(dir, "none.yml")); got != "" || err != nil {
		t.Errorf("missing config: %q %v", got, err)
	}
}
