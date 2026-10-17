package scan

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/njhoffman/mediacat/internal/catalog"
	"github.com/njhoffman/mediacat/internal/filter"
	"github.com/njhoffman/mediacat/internal/probe"
	"github.com/njhoffman/mediacat/internal/testmedia"
	"github.com/njhoffman/mediacat/internal/volume"
)

type fakeProber struct {
	mu    sync.Mutex
	calls []string
	errs  map[string]error // by base name
	hook  func(n int)      // called after each probe with the call count
}

func (p *fakeProber) Probe(ctx context.Context, path, kind string) (probe.Result, error) {
	if err := ctx.Err(); err != nil {
		return probe.Result{}, err
	}
	p.mu.Lock()
	p.calls = append(p.calls, filepath.Base(path))
	n := len(p.calls)
	err := p.errs[filepath.Base(path)]
	p.mu.Unlock()
	if p.hook != nil {
		p.hook(n)
	}
	if err != nil {
		return probe.Result{}, err
	}
	r := probe.Result{Container: "fake", Width: 1920, Height: 1080, DurationMS: 1000, VideoCodec: "h264"}
	if kind == filter.KindAudio {
		r = probe.Result{AudioCodec: "mp3", DurationMS: 2000, Tags: probe.Tags{Artist: "Artist"}}
	}
	return r, nil
}

func (p *fakeProber) reset() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	c := p.calls
	p.calls = nil
	sort.Strings(c)
	return c
}

type env struct {
	t      *testing.T
	dir    string
	db     *catalog.DB
	prober *fakeProber
	res    *volume.Resolver
}

func newEnv(t *testing.T) *env {
	t.Helper()
	res, err := volume.NewResolver()
	if err != nil {
		t.Skip("no mountinfo:", err)
	}
	db, err := catalog.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	return &env{t: t, dir: dir, db: db, prober: &fakeProber{}, res: res}
}

func (e *env) write(rel, content string) string {
	e.t.Helper()
	p := filepath.Join(e.dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *env) scan(dir string, o Options) *Summary {
	e.t.Helper()
	sum, err := e.run(context.Background(), dir, o)
	if err != nil {
		e.t.Fatal(err)
	}
	return sum
}

func (e *env) run(ctx context.Context, dir string, o Options) (*Summary, error) {
	if o.Workers == 0 {
		o.Workers = 2
	}
	s := &Scanner{DB: e.db, Resolver: e.res, Prober: e.prober, Options: o}
	return s.Run(ctx, dir)
}

func (e *env) query(f filter.Filter) []string {
	e.t.Helper()
	files, err := e.db.Query(0, "", f)
	if err != nil {
		e.t.Fatal(err)
	}
	var out []string
	for _, f := range files {
		out = append(out, filepath.Base(f.RelPath))
	}
	sort.Strings(out)
	return out
}

func same(a, b []string) bool { return fmt.Sprint(a) == fmt.Sprint(b) }

func TestScanLifecycle(t *testing.T) {
	e := newEnv(t)
	a := e.write("a.mp4", "video-a")
	e.write("sub/b.mp3", "audio-b")
	c := e.write("c.jpg", "image-c")
	e.write("notes.txt", "not media")
	e.write(".Trash-1000/files/trashed.mp4", "trashed")
	e.write("@eaDir/thumb.jpg", "nas metadata")
	e.write("trashbox/old.mp4", "manual trash")
	if err := os.Symlink(a, filepath.Join(e.dir, "link.mp4")); err != nil {
		t.Fatal(err)
	}

	sum := e.scan(e.dir, Options{})
	if sum.Seen != 3 || sum.Added != 3 || sum.Interrupted {
		t.Fatalf("first scan: %+v", sum)
	}
	if got := e.prober.reset(); !same(got, []string{"a.mp4", "b.mp3", "c.jpg"}) {
		t.Errorf("probed %v", got)
	}

	sum = e.scan(e.dir, Options{})
	if sum.Unchanged != 3 || sum.Added+sum.Changed != 0 || len(e.prober.reset()) != 0 {
		t.Errorf("second scan should probe nothing: %+v", sum)
	}

	// Content change.
	os.WriteFile(a, []byte("video-a-longer"), 0o644)
	sum = e.scan(e.dir, Options{})
	if sum.Changed != 1 || !same(e.prober.reset(), []string{"a.mp4"}) {
		t.Errorf("change: %+v", sum)
	}

	// Rename keeps metadata and is not re-probed.
	os.Rename(filepath.Join(e.dir, "sub/b.mp3"), filepath.Join(e.dir, "sub/renamed.mp3"))
	sum = e.scan(e.dir, Options{})
	if sum.Renamed != 1 || sum.Missing != 0 || sum.Added != 0 || len(e.prober.reset()) != 0 {
		t.Errorf("rename: %+v", sum)
	}
	files, _ := e.db.Query(0, "", filter.Filter{Kind: filter.KindAudio})
	if len(files) != 1 || filepath.Base(files[0].RelPath) != "renamed.mp3" || files[0].Meta.Tags.Artist != "Artist" {
		t.Errorf("renamed row: %+v", files)
	}

	// Deletion marks missing once; reappearing unchanged restores without probing.
	info, _ := os.Stat(c)
	os.Remove(c)
	if sum = e.scan(e.dir, Options{}); sum.Missing != 1 {
		t.Errorf("missing: %+v", sum)
	}
	if got := e.query(filter.Filter{Missing: true}); !same(got, []string{"c.jpg"}) {
		t.Errorf("missing list = %v", got)
	}
	if sum = e.scan(e.dir, Options{}); sum.Missing != 0 {
		t.Errorf("already-missing file counted again: %+v", sum)
	}
	e.write("c.jpg", "image-c")
	os.Chtimes(c, info.ModTime(), info.ModTime())
	if sum = e.scan(e.dir, Options{}); sum.Restored != 1 || len(e.prober.reset()) != 0 {
		t.Errorf("restore: %+v", sum)
	}

	// Prune removes rows instead of marking them.
	os.Remove(c)
	if sum = e.scan(e.dir, Options{Prune: true}); sum.Pruned != 1 {
		t.Errorf("prune: %+v", sum)
	}
	if got := e.query(filter.Filter{Missing: true}); len(got) != 0 {
		t.Errorf("pruned file still listed: %v", got)
	}

	// Force re-probes everything.
	e.prober.reset()
	if sum = e.scan(e.dir, Options{Force: true}); sum.Changed != 2 || len(e.prober.reset()) != 2 {
		t.Errorf("force: %+v", sum)
	}

	scans, _ := e.db.RecentScans(1)
	if scans[0].Status != catalog.ScanComplete {
		t.Errorf("scan status %s", scans[0].Status)
	}
}

func TestSubdirScanLeavesSiblingsAlone(t *testing.T) {
	e := newEnv(t)
	e.write("top.mp4", "t")
	e.write("sub/inner.mp4", "i")
	e.scan(e.dir, Options{})
	os.Remove(filepath.Join(e.dir, "sub/inner.mp4"))
	sum := e.scan(filepath.Join(e.dir, "sub"), Options{})
	if sum.Missing != 1 || !same(e.query(filter.Filter{}), []string{"top.mp4"}) {
		t.Errorf("subdir scan: %+v, present=%v", sum, e.query(filter.Filter{}))
	}
}

func TestExcludesAndUnreadableDirsAreNotMarkedMissing(t *testing.T) {
	e := newEnv(t)
	e.write("keep/a.mp4", "a")
	e.write("skip/b.mp4", "b")
	e.write("locked/c.mp4", "c")
	e.scan(e.dir, Options{})

	sum := e.scan(e.dir, Options{Excludes: []string{"skip"}})
	if sum.Missing != 0 || sum.Seen != 2 {
		t.Errorf("exclude: %+v", sum)
	}

	if os.Geteuid() == 0 {
		t.Skip("root can read unreadable dirs")
	}
	locked := filepath.Join(e.dir, "locked")
	os.Chmod(locked, 0)
	t.Cleanup(func() { os.Chmod(locked, 0o755) })
	sum = e.scan(e.dir, Options{})
	if sum.Missing != 0 || len(sum.WalkErrors) == 0 {
		t.Errorf("unreadable dir: %+v", sum)
	}
}

func TestFailuresTransientVsPermanent(t *testing.T) {
	e := newEnv(t)
	e.write("ok.mp4", "1")
	e.write("flaky.mp4", "2")
	e.write("broken.mp4", "3")
	e.prober.errs = map[string]error{
		"flaky.mp4":  fmt.Errorf("%w: Input/output error", probe.ErrTransient),
		"broken.mp4": errors.New("moov atom not found"),
	}
	sum := e.scan(e.dir, Options{})
	if sum.Added != 2 || len(sum.Deferred) != 1 || len(sum.Failures) != 1 {
		t.Fatalf("summary: %+v", sum)
	}
	if got := e.query(filter.Filter{}); !same(got, []string{"broken.mp4", "ok.mp4"}) {
		t.Errorf("stored = %v", got)
	}
	e.prober.reset()
	e.prober.errs = nil
	sum = e.scan(e.dir, Options{})
	if got := e.prober.reset(); !same(got, []string{"flaky.mp4"}) || sum.Added != 1 {
		t.Errorf("retry probed %v, %+v", got, sum)
	}
}

func TestInterruptAndResume(t *testing.T) {
	e := newEnv(t)
	for i := range 10 {
		e.write(fmt.Sprintf("v%02d.mp4", i), fmt.Sprint(i))
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.prober.hook = func(n int) {
		if n == 3 {
			cancel()
		}
	}
	var events atomic.Int32
	sum, err := e.run(ctx, e.dir, Options{Workers: 1, BatchSize: 1000, Progress: func(Event) { events.Add(1) }})
	if err != nil {
		t.Fatal(err)
	}
	if !sum.Interrupted || sum.Added < 1 || sum.Added > 3 || events.Load() == 0 {
		t.Fatalf("interrupted scan: %+v", sum)
	}
	scans, _ := e.db.RecentScans(1)
	if scans[0].Status != catalog.ScanInterrupted || scans[0].FinishedAt == nil {
		t.Errorf("status = %+v", scans[0])
	}
	stored := sum.Added

	e.prober.hook = nil
	e.prober.reset()
	sum = e.scan(e.dir, Options{})
	if probed := len(e.prober.reset()); probed != 10-stored || sum.Added != 10-stored || sum.Interrupted {
		t.Errorf("resume probed %d (stored %d): %+v", probed, stored, sum)
	}
}

func TestCancelledBeforeWalk(t *testing.T) {
	e := newEnv(t)
	e.write("a.mp4", "a")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sum, err := e.run(ctx, e.dir, Options{})
	if err != nil || !sum.Interrupted || sum.Added != 0 {
		t.Errorf("got %+v, %v", sum, err)
	}
}

func TestRunErrors(t *testing.T) {
	e := newEnv(t)
	f := e.write("file.mp4", "x")
	if _, err := e.run(context.Background(), f, Options{}); err == nil {
		t.Error("scanning a file should fail")
	}
	if _, err := e.run(context.Background(), filepath.Join(e.dir, "nope"), Options{}); err == nil {
		t.Error("scanning a missing dir should fail")
	}
}

func TestWorkersDefault(t *testing.T) {
	if workers(0, volume.Volume{Network: true}) != 2 || workers(0, volume.Volume{Rotational: true}) != 2 {
		t.Error("slow volumes should default to 2 workers")
	}
	if workers(5, volume.Volume{Network: true}) != 5 {
		t.Error("explicit workers should win")
	}
	if workers(0, volume.Volume{}) < 1 {
		t.Error("fast volume workers")
	}
}

func TestRealProbe(t *testing.T) {
	e := newEnv(t)
	files := testmedia.Generate(t, e.dir)
	s := &Scanner{DB: e.db, Resolver: e.res, Prober: probe.Prober{Timeout: 30 * time.Second}}
	sum, err := s.Run(context.Background(), e.dir)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Added != 4 || len(sum.Failures) != 1 || filepath.Base(sum.Failures[0].RelPath) != filepath.Base(files.Truncated) {
		t.Fatalf("summary: %+v", sum)
	}
	vids, _ := e.db.Query(0, "", filter.Filter{Kind: filter.KindVideo, MinRes: 240})
	if len(vids) != 1 || vids[0].Meta.VideoCodec != "h264" || vids[0].Meta.DurationMS < 900 {
		t.Errorf("video row: %+v", vids)
	}
	songs, _ := e.db.Query(0, "", filter.Filter{Kind: filter.KindAudio})
	if len(songs) != 1 || songs[0].Meta.Tags.Title != "Test Title" {
		t.Errorf("audio row: %+v", songs)
	}
}

func TestExcludedByDefault(t *testing.T) {
	if !ExcludedByDefault("/mnt/nfs/trashbox/x", "/mnt/nfs") || !ExcludedByDefault("/mnt/nfs/a/.Trash-1000", "/mnt/nfs") {
		t.Error("trash dirs should be excluded")
	}
	if ExcludedByDefault("/mnt/nfs/holding", "/mnt/nfs") {
		t.Error("ordinary dir excluded")
	}
}
