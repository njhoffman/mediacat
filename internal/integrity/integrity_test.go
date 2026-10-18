package integrity

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/njhoffman/mediacat/internal/catalog"
	"github.com/njhoffman/mediacat/internal/filter"
	"github.com/njhoffman/mediacat/internal/probe"
	"github.com/njhoffman/mediacat/internal/scan"
	"github.com/njhoffman/mediacat/internal/testmedia"
	"github.com/njhoffman/mediacat/internal/volume"
)

type env struct {
	t   *testing.T
	dir string
	db  *catalog.DB
	b   testmedia.Broken
}

func newEnv(t *testing.T) *env {
	t.Helper()
	res, err := volume.NewResolver()
	if err != nil {
		t.Skip(err)
	}
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	b := testmedia.GenerateBroken(t, dir)
	os.WriteFile(filepath.Join(dir, "notmedia.mp4"), []byte("plain text, not a video"), 0o644)
	db, err := catalog.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s := &scan.Scanner{DB: db, Resolver: res, Prober: probe.Prober{}}
	if _, err := s.Run(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	return &env{t: t, dir: dir, db: db, b: b}
}

func (e *env) files() []catalog.File {
	e.t.Helper()
	fs, err := e.db.Query(0, "", filter.Filter{})
	if err != nil {
		e.t.Fatal(err)
	}
	return fs
}

func (e *env) file(name string) catalog.File {
	e.t.Helper()
	for _, f := range e.files() {
		if filepath.Base(f.RelPath) == name {
			return f
		}
	}
	e.t.Fatalf("no %s", name)
	return catalog.File{}
}

func TestQuickAndFullChecks(t *testing.T) {
	e := newEnv(t)
	c := Checker{}
	ctx := context.Background()
	cases := []struct {
		name          string
		quick, full   string
		detailContain string
	}{
		{"good.mp4", catalog.CheckOK, catalog.CheckOK, ""},
		{"truncated.mp4", catalog.CheckDamaged, catalog.CheckDamaged, "data ends at 0:11 of 0:20"},
		// Damage in the middle is beyond what the quick check reads; only --full sees it.
		{"corrupt.mp4", catalog.CheckOK, catalog.CheckDamaged, ""},
		{"ok.png", catalog.CheckOK, catalog.CheckOK, ""},
		{"cut.jpg", catalog.CheckDamaged, catalog.CheckDamaged, "truncated"},
		{"notmedia.mp4", catalog.CheckUnreadable, catalog.CheckUnreadable, "not valid media"},
	}
	for _, tc := range cases {
		f := e.file(tc.name)
		q := c.Check(ctx, f, catalog.LevelQuick, nil)
		var progressed int64
		full := c.Check(ctx, f, catalog.LevelFull, func(n int64) { progressed += n })
		if q.Status != tc.quick || full.Status != tc.full {
			t.Errorf("%s: quick=%s (%s) full=%s (%s)", tc.name, q.Status, q.Detail, full.Status, full.Detail)
		}
		if tc.detailContain != "" && !strings.Contains(q.Detail, tc.detailContain) {
			t.Errorf("%s quick detail: %q", tc.name, q.Detail)
		}
		if full.Level != catalog.LevelFull {
			t.Errorf("%s full level = %s", tc.name, full.Level)
		}
		if tc.name == "good.mp4" && progressed != f.Size {
			t.Errorf("full progress reported %d of %d bytes", progressed, f.Size)
		}
	}
	if f := c.Check(ctx, e.file("corrupt.mp4"), catalog.LevelFull, nil); !strings.Contains(f.Detail, "decode errors") {
		t.Errorf("corrupt full detail: %q", f.Detail)
	}
	if q := c.Check(ctx, e.file("ok.png"), catalog.LevelQuick, nil); q.Level != catalog.LevelFull {
		t.Error("images are always fully decoded")
	}
}

func TestRunStoresAndResumes(t *testing.T) {
	e := newEnv(t)
	sum, err := Run(context.Background(), e.db, Checker{}, e.files(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Checked != 6 || sum.Counts[catalog.CheckOK] != 3 || sum.Counts[catalog.CheckDamaged] != 2 || sum.Counts[catalog.CheckUnreadable] != 1 {
		t.Fatalf("summary: %+v", sum)
	}
	if f := e.file("truncated.mp4"); f.Check.Status != catalog.CheckDamaged || f.Check.Level != catalog.LevelQuick || f.Check.At.IsZero() {
		t.Errorf("stored: %+v", f.Check)
	}
	// Quick results are current for a quick run; a full run redoes the media files.
	if sum, _ = Run(context.Background(), e.db, Checker{}, e.files(), Options{}); sum.Checked != 0 || sum.Current != 6 {
		t.Errorf("second quick run: %+v", sum)
	}
	sum, _ = Run(context.Background(), e.db, Checker{}, e.files(), Options{Level: catalog.LevelFull})
	if sum.Checked != 4 || sum.Current != 2 { // the images were already fully decoded
		t.Errorf("full run: %+v", sum)
	}
	if sum, _ = Run(context.Background(), e.db, Checker{}, e.files(), Options{}); sum.Checked != 0 {
		t.Errorf("full results satisfy quick runs: %+v", sum)
	}
	if sum, _ = Run(context.Background(), e.db, Checker{}, e.files(), Options{Recheck: true}); sum.Checked != 6 {
		t.Errorf("recheck: %+v", sum)
	}

	// A file changed since the scan is reported, not checked.
	later := time.Now().Add(time.Hour)
	os.Chtimes(e.b.Good, later, later)
	if sum, _ = Run(context.Background(), e.db, Checker{}, e.files(), Options{Recheck: true}); len(sum.Changed) != 1 {
		t.Errorf("changed: %+v", sum)
	}
}

func TestInterrupt(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	sum, err := Run(ctx, e.db, Checker{}, e.files(), Options{Workers: 1, BatchSize: 1, Level: catalog.LevelFull,
		Progress: func(ev Event) {
			if ev.Done >= 1 {
				cancel()
			}
		}})
	if err != nil || !sum.Interrupted || sum.Checked >= 6 {
		t.Fatalf("interrupted: %+v %v", sum, err)
	}
	first := sum.Checked
	sum, _ = Run(context.Background(), e.db, Checker{}, e.files(), Options{Level: catalog.LevelFull})
	if first+sum.Checked != 6 {
		t.Errorf("resume: first %d + second %d != 6", first, sum.Checked)
	}
}

func TestTransientIsUnverifiedAndRetried(t *testing.T) {
	e := newEnv(t)
	fake := filepath.Join(t.TempDir(), "ffmpeg")
	os.WriteFile(fake, []byte("#!/bin/sh\necho '[mov @ 0x55] good.mp4: Input/output error' >&2\nexit 1\n"), 0o755)
	f := e.file("good.mp4")
	// The tail read fails transiently even though the start decodes.
	tailFail := Checker{FFprobe: fake}
	if r := tailFail.Check(context.Background(), f, catalog.LevelQuick, nil); r.Status != catalog.CheckUnverified {
		t.Errorf("tail read error: %+v", r)
	}
	for _, level := range []string{catalog.LevelQuick, catalog.LevelFull} {
		if r := (Checker{FFmpeg: fake}).Check(context.Background(), f, level, nil); r.Status != catalog.CheckUnverified {
			t.Errorf("%s: %+v", level, r)
		}
	}
	Run(context.Background(), e.db, Checker{FFmpeg: fake}, []catalog.File{f}, Options{})
	f = e.file("good.mp4")
	if f.Check.Status != catalog.CheckUnverified {
		t.Fatalf("stored: %+v", f.Check)
	}
	if sum, _ := Run(context.Background(), e.db, Checker{}, []catalog.File{f}, Options{}); sum.Checked != 1 || sum.Counts[catalog.CheckOK] != 1 {
		t.Errorf("unverified results are retried: %+v", sum)
	}
}

func TestHelpers(t *testing.T) {
	lines := errorLines([]byte("[h264 @ 0x5b8af43b6700] Invalid NAL unit\n\n  [aac @ 0xab] x \n"))
	if len(lines) != 2 || lines[0] != "[h264] Invalid NAL unit" {
		t.Errorf("errorLines = %q", lines)
	}
	var b limitedBuffer
	for range maxKeptErrors + 50 {
		b.Write([]byte("err\n"))
	}
	if b.lines != maxKeptErrors+50 || strings.Count(string(b.Bytes()), "\n") != maxKeptErrors {
		t.Errorf("limitedBuffer: lines=%d kept=%d", b.lines, strings.Count(string(b.Bytes()), "\n"))
	}
	if !satisfied(catalog.Check{Status: "ok", Level: "full"}, "quick") || satisfied(catalog.Check{Status: "ok", Level: "quick"}, "full") ||
		satisfied(catalog.Check{Status: "unverified", Level: "full"}, "quick") || satisfied(catalog.Check{}, "quick") {
		t.Error("satisfied")
	}
}
