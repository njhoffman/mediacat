package dupes

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/njhoffman/mediacat/internal/catalog"
	"github.com/njhoffman/mediacat/internal/phash"
	"github.com/njhoffman/mediacat/internal/probe"
)

// fakeHasher returns preset hashes or errors by base name and counts calls.
type fakeHasher struct {
	mu     sync.Mutex
	hashes map[string]uint64
	errs   map[string]error
	calls  map[string]int
}

func (h *fakeHasher) get(path string) (uint64, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	name := filepath.Base(path)
	h.calls[name]++
	if err := h.errs[name]; err != nil {
		return 0, err
	}
	v, ok := h.hashes[name]
	if !ok {
		return 0, fmt.Errorf("no hash for %s", name)
	}
	return v, nil
}

func (h *fakeHasher) Video(_ context.Context, path string, _ float64) (uint64, error) {
	return h.get(path)
}
func (h *fakeHasher) Image(_ context.Context, path string) (uint64, error) { return h.get(path) }

func (h *fakeHasher) total() int {
	n := 0
	for _, c := range h.calls {
		n += c
	}
	return n
}

// put writes a file and catalogs it with the given metadata.
func (fx *fixture) put(name, kind string, size int, m probe.Result) string {
	fx.t.Helper()
	p := fx.write(name, make([]byte, size), 0)
	st, _ := os.Stat(p)
	tx, _ := fx.db.Begin()
	f := catalog.File{VolumeID: fx.vol, RelPath: filepath.ToSlash(filepath.Join(fx.rel, name)), Size: st.Size(),
		MtimeNS: st.ModTime().UnixNano(), Kind: kind, Meta: m, ScannedAt: time.Now()}
	if err := tx.PutFile(f); err != nil {
		fx.t.Fatal(err)
	}
	tx.Commit()
	return p
}

func video(min, w, h int, vbr int64) probe.Result {
	return probe.Result{DurationMS: int64(min) * 60_000, Width: w, Height: h, VideoBitrate: vbr}
}

func (fx *fixture) similar(h Hasher, opts ...func(*SimilarOptions)) *Result {
	fx.t.Helper()
	pol, _ := ParsePolicy(DefaultSimilarPolicy, nil, nil)
	o := SimilarOptions{Distance: 4, DurationPct: 5, DurationMin: 5 * time.Second, MinImageSize: 64,
		Workers: 2, Policy: pol, Hasher: h}
	for _, fn := range opts {
		fn(&o)
	}
	r, err := (&SimilarFinder{DB: fx.db, Options: o}).Find(context.Background(), fx.files())
	if err != nil {
		fx.t.Fatal(err)
	}
	return r
}

func TestSimilarGroupsAndCaching(t *testing.T) {
	fx := newFixture(t)
	fx.put("a.mp4", "video", 100, video(30, 640, 480, 1e6))
	fx.put("b.mkv", "video", 300, video(30, 1920, 1080, 8e6))
	fx.put("e.mp4", "video", 200, video(31, 1280, 720, 4e6)) // within 5% of 30m
	fx.put("c.mp4", "video", 100, video(30, 640, 480, 1e6))
	fx.put("d.mp4", "video", 100, video(90, 640, 480, 1e6)) // close hash, far duration
	fx.put("broken.mp4", "video", 100, video(30, 640, 480, 1e6))
	fx.put("flaky.mp4", "video", 100, video(30, 640, 480, 1e6))
	fx.put("song.mp3", "audio", 100, probe.Result{DurationMS: 1000})
	fx.put("icon.png", "image", 100, probe.Result{Width: 32, Height: 32})
	fx.put("p1.jpg", "image", 100, probe.Result{Width: 800, Height: 600})
	fx.put("p2.png", "image", 150, probe.Result{Width: 1600, Height: 1200})
	fx.put("p3.jpg", "image", 100, probe.Result{}) // size unknown until decoded
	h := &fakeHasher{
		hashes: map[string]uint64{"a.mp4": 0, "b.mkv": 0b111, "e.mp4": 0b111_1000_0000, "c.mp4": ^uint64(0),
			"d.mp4": 0b11, "p1.jpg": 0xF0, "p2.png": 0xF1},
		errs: map[string]error{"broken.mp4": errors.New("no frame decoded"),
			"flaky.mp4": fmt.Errorf("%w: Input/output error", phash.ErrTransient),
			"p3.jpg":    fmt.Errorf("%w (16x16)", phash.ErrTooSmall)},
		calls: map[string]int{},
	}
	stashCalls := 0
	stashLookup := func(f catalog.File) (uint64, bool) {
		stashCalls++
		return 0, filepath.Base(f.AbsPath) == "a.mp4"
	}
	r := fx.similar(h, func(o *SimilarOptions) { o.Stash = stashLookup })

	if len(r.Groups) != 2 {
		t.Fatalf("groups: %+v", r.Groups)
	}
	g := r.Groups[0]
	if !eq(names(g), []string{"a.mp4", "b.mkv", "e.mp4"}) || !g.Similar || !g.Confirmed {
		t.Fatalf("video group: %v", names(g))
	}
	if keep := filepath.Base(g.Files[g.Keep].AbsPath); keep != "b.mkv" {
		t.Errorf("keeper %s, want the highest resolution", keep)
	}
	for i, f := range g.Files {
		want := map[string]int{"a.mp4": 3, "b.mkv": 0, "e.mp4": 7}[filepath.Base(f.AbsPath)]
		if d := g.Distance(i, g.Keep); d != want {
			t.Errorf("%s distance %d, want %d (chained through a.mp4)", f.AbsPath, d, want)
		}
	}
	if g.Reclaimable() != 300 || g.Size != 300 {
		t.Errorf("reclaimable %d size %d", g.Reclaimable(), g.Size)
	}
	if img := r.Groups[1]; !eq(names(img), []string{"p1.jpg", "p2.png"}) || filepath.Base(img.Files[img.Keep].AbsPath) != "p2.png" {
		t.Errorf("image group: %v", names(img))
	}
	if h.calls["a.mp4"] != 0 || r.FromStash != 1 {
		t.Errorf("a.mp4 should come from Stash: calls=%d fromStash=%d", h.calls["a.mp4"], r.FromStash)
	}
	if r.Skipped != 1 || r.SmallImages != 2 || len(r.Unhashable) != 1 || len(r.Failed) != 0 || len(r.Deferred) != 1 || r.Hashed != 6 {
		t.Errorf("counts: skipped=%d small=%d unhashable=%v failed=%v deferred=%v hashed=%d",
			r.Skipped, r.SmallImages, r.Unhashable, r.Failed, r.Deferred, r.Hashed)
	}
	if h.calls["icon.png"] != 0 {
		t.Error("images known to be small should not be decoded")
	}

	// Stored: a second run hashes only the file that failed transiently.
	before := h.total()
	r2 := fx.similar(h, func(o *SimilarOptions) { o.Stash = stashLookup })
	if h.total()-before != 1 || h.calls["flaky.mp4"] != 2 {
		t.Errorf("second run hashed %d files: %v", h.total()-before, h.calls)
	}
	if r2.FromStash != 1 || len(r2.Unhashable) != 1 || r2.Unhashable[0].Err == "" || r2.SmallImages != 2 || len(r2.Groups) != 2 || r2.Groups[0].Key != g.Key {
		t.Errorf("second run: %+v", r2)
	}
	for _, f := range fx.files() {
		if filepath.Base(f.AbsPath) == "a.mp4" && f.PHash.Source != catalog.PHashStash {
			t.Errorf("a.mp4 source %q", f.PHash.Source)
		}
		if filepath.Base(f.AbsPath) == "broken.mp4" && (f.PHash.Source != catalog.PHashNone || f.PHash.Error != "no frame decoded") {
			t.Errorf("broken.mp4 %+v", f.PHash)
		}
	}

	// Lower thresholds split the chain; a wider duration tolerance pulls in d.mp4.
	if r := fx.similar(h, func(o *SimilarOptions) { o.Distance = 3 }); len(r.Groups) != 2 || !hasGroup(r, "a.mp4", "b.mkv") {
		t.Errorf("distance 3: %v", r.Groups)
	}
	if r := fx.similar(h, func(o *SimilarOptions) { o.DurationPct = 80 }); !eq(names(r.Groups[0]), []string{"a.mp4", "b.mkv", "d.mp4", "e.mp4"}) {
		t.Errorf("wide duration tolerance: %v", names(r.Groups[0]))
	}
}

func hasGroup(r *Result, want ...string) bool {
	for _, g := range r.Groups {
		if eq(names(g), want) {
			return true
		}
	}
	return false
}

func TestSimilarChangedAndInterrupted(t *testing.T) {
	fx := newFixture(t)
	p := fx.put("a.mp4", "video", 100, video(1, 640, 480, 0))
	fx.put("b.mp4", "video", 100, video(1, 640, 480, 0))
	os.WriteFile(p, []byte("changed"), 0o644)
	h := &fakeHasher{hashes: map[string]uint64{"a.mp4": 1, "b.mp4": 1}, calls: map[string]int{}}
	r := fx.similar(h)
	if len(r.Changed) != 1 || len(r.Groups) != 0 || h.calls["a.mp4"] != 0 {
		t.Errorf("changed: %+v", r)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	pol, _ := ParsePolicy(DefaultSimilarPolicy, nil, nil)
	r, err := (&SimilarFinder{DB: fx.db, Options: SimilarOptions{Distance: 4, Policy: pol, Hasher: h}}).Find(ctx, fx.files())
	if err != nil || !r.Interrupted {
		t.Errorf("interrupted: %v %+v", err, r)
	}
}

func TestSimilarKeyChangesWithMembers(t *testing.T) {
	a := []catalog.File{{VolumeKey: "v", RelPath: "a", Size: 1, MtimeNS: 1}, {VolumeKey: "v", RelPath: "b", Size: 2, MtimeNS: 1}}
	b := []catalog.File{a[0], {VolumeKey: "v", RelPath: "b", Size: 2, MtimeNS: 2}}
	if similarKey(a) == similarKey(b) || similarKey(a) != similarKey(a) {
		t.Error("key must identify members and their versions")
	}
}

func TestParseLevel(t *testing.T) {
	for in, want := range map[string]int{"exact": 0, "High": 4, "medium": 8, "low": 10, "12": 12, "0": 0} {
		if got, err := ParseLevel(in); err != nil || got != want {
			t.Errorf("%s: %d %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "33", "-1", "close"} {
		if _, err := ParseLevel(bad); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
	if LevelName(4) != "high, distance <= 4" || LevelName(6) != "distance <= 6" {
		t.Error(LevelName(4), LevelName(6))
	}
}

func TestQualityRules(t *testing.T) {
	files := []catalog.File{
		{AbsPath: "/a", Size: 10, Meta: probe.Result{Width: 1920, Height: 1080, VideoBitrate: 1e6}},
		{AbsPath: "/b", Size: 30, Meta: probe.Result{Width: 1280, Height: 720, Bitrate: 9e6}},
		{AbsPath: "/c", Size: 20, Meta: probe.Result{Width: 1920, Height: 1080, VideoBitrate: 5e6}},
	}
	for spec, want := range map[string]string{"res": "/a", "res,bitrate": "/c", "bitrate": "/b", "size": "/b"} {
		p, err := ParsePolicy(spec, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := files[p.Choose(files)].AbsPath; got != want {
			t.Errorf("%s: %s, want %s", spec, got, want)
		}
	}
}
