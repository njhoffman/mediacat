package dupes

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/njhoffman/mediacat/internal/catalog"
	"github.com/njhoffman/mediacat/internal/filter"
	"github.com/njhoffman/mediacat/internal/hash"
	"github.com/njhoffman/mediacat/internal/volume"
)

type fixture struct {
	t   *testing.T
	dir string
	db  *catalog.DB
	vol int64
	rel string
	v   volume.Volume
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	res, err := volume.NewResolver()
	if err != nil {
		t.Skip(err)
	}
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	v, rel, err := res.Resolve(dir)
	if err != nil {
		t.Fatal(err)
	}
	db, err := catalog.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	id, err := db.UpsertVolume(v, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{t: t, dir: dir, db: db, vol: id, rel: rel, v: v}
}

var base = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

// write creates a file with an explicit mtime offset (days after base).
func (fx *fixture) write(name string, data []byte, day int) string {
	fx.t.Helper()
	p := filepath.Join(fx.dir, name)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		fx.t.Fatal(err)
	}
	mt := base.Add(time.Duration(day) * 24 * time.Hour)
	os.Chtimes(p, mt, mt)
	return p
}

// catalog records every file under dir the way a scan would.
func (fx *fixture) catalog() {
	fx.t.Helper()
	tx, _ := fx.db.Begin()
	filepath.Walk(fx.dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		sub, _ := filepath.Rel(fx.dir, p)
		rel := filepath.ToSlash(filepath.Join(fx.rel, sub))
		f := catalog.File{VolumeID: fx.vol, RelPath: rel, Size: info.Size(), MtimeNS: info.ModTime().UnixNano(),
			Kind: filter.KindVideo, ScannedAt: time.Now()}
		f.Inode = inodeOf(info)
		if err := tx.PutFile(f); err != nil {
			fx.t.Fatal(err)
		}
		return nil
	})
	if err := tx.Commit(); err != nil {
		fx.t.Fatal(err)
	}
}

func (fx *fixture) files() []catalog.File {
	fx.t.Helper()
	fs, err := fx.db.Query(fx.vol, fx.rel, filter.Filter{MinSize: 1})
	if err != nil {
		fx.t.Fatal(err)
	}
	return fs
}

func (fx *fixture) find(depth Depth, opts ...func(*Options)) *Result {
	fx.t.Helper()
	pol, _ := ParsePolicy(DefaultPolicy, nil, nil)
	o := Options{Depth: depth, Policy: pol, Workers: 2}
	for _, fn := range opts {
		fn(&o)
	}
	r, err := (&Finder{DB: fx.db, Options: o}).Find(context.Background(), fx.files())
	if err != nil {
		fx.t.Fatal(err)
	}
	return r
}

func names(g Group) []string {
	var out []string
	for _, f := range g.Files {
		out = append(out, filepath.Base(f.AbsPath))
	}
	sort.Strings(out)
	return out
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func data(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7) ^ seed
	}
	return b
}

func setup(t *testing.T) *fixture {
	fx := newFixture(t)
	big := data(1<<20, 1)
	fx.write("a.mp4", big, 5)
	fx.write("dir/b.mp4", big, 1) // oldest
	fx.write("a (1).mp4", big, 0) // older still, but has a copy marker
	c := bytes.Clone(big)
	c[hash.SampleSize+10] ^= 1 // outside the sampled regions
	fx.write("c.mp4", c, 2)
	d := bytes.Clone(big)
	d[0] ^= 1 // inside a sampled region
	fx.write("d.mp4", d, 2)
	fx.write("unique.mp4", data(5000, 9), 2)
	fx.write("small1.mp4", data(1000, 3), 3)
	fx.write("small2.mp4", data(1000, 3), 4)
	if err := os.Link(filepath.Join(fx.dir, "a.mp4"), filepath.Join(fx.dir, "hardlink.mp4")); err != nil {
		t.Fatal(err)
	}
	fx.catalog()
	return fx
}

func TestDepths(t *testing.T) {
	fx := setup(t)

	r := fx.find(DepthSize)
	if len(r.Groups) != 2 || r.Hardlinks != 1 || r.Hashed != 0 || r.Groups[0].Confirmed {
		t.Fatalf("size depth: %+v", r)
	}
	if got := names(r.Groups[0]); !eq(got, []string{"a (1).mp4", "a.mp4", "b.mp4", "c.mp4", "d.mp4"}) {
		t.Errorf("size group = %v", got)
	}

	r = fx.find(DepthPartial)
	if len(r.Groups) != 2 || r.Groups[0].Confirmed {
		t.Fatalf("partial: %+v", r)
	}
	if got := names(r.Groups[0]); !eq(got, []string{"a (1).mp4", "a.mp4", "b.mp4", "c.mp4"}) {
		t.Errorf("partial group = %v (c collides on samples, d does not)", got)
	}

	r = fx.find(DepthFull)
	if len(r.Groups) != 2 || !r.Groups[0].Confirmed {
		t.Fatalf("full: %+v", r)
	}
	g := r.Groups[0]
	if got := names(g); !eq(got, []string{"a (1).mp4", "a.mp4", "b.mp4"}) {
		t.Errorf("full group = %v", got)
	}
	if keep := filepath.Base(g.Files[g.Keep].AbsPath); keep != "b.mp4" {
		t.Errorf("keeper = %s; want b.mp4 (plain beats oldest)", keep)
	}
	if g.Reclaimable() != 2<<20 {
		t.Errorf("reclaimable = %d", g.Reclaimable())
	}
	if got := names(r.Groups[1]); !eq(got, []string{"small1.mp4", "small2.mp4"}) {
		t.Errorf("small group = %v", got)
	}
}

func TestHashesAreCached(t *testing.T) {
	fx := setup(t)
	first := fx.find(DepthFull)
	if first.Hashed == 0 {
		t.Fatal("first run should hash")
	}
	if again := fx.find(DepthFull); again.Hashed != 0 || len(again.Groups) != 2 {
		t.Errorf("second run hashed %d files", again.Hashed)
	}
	// A changed file invalidates its cached hashes once the catalog sees the change.
	fx.write("small2.mp4", data(1000, 4), 6)
	fx.catalog()
	r := fx.find(DepthFull)
	if len(r.Groups) != 1 || r.Hashed != 1 {
		t.Errorf("after change: groups=%d hashed=%d", len(r.Groups), r.Hashed)
	}
}

func TestChangedSinceScanIsSkipped(t *testing.T) {
	fx := setup(t)
	later := base.Add(100 * 24 * time.Hour)
	os.Chtimes(filepath.Join(fx.dir, "dir/b.mp4"), later, later)
	r := fx.find(DepthFull)
	if len(r.Changed) != 1 || filepath.Base(r.Changed[0]) != "b.mp4" {
		t.Fatalf("changed = %v", r.Changed)
	}
	if got := names(r.Groups[0]); !eq(got, []string{"a (1).mp4", "a.mp4"}) {
		t.Errorf("group without changed file = %v", got)
	}
	os.Remove(filepath.Join(fx.dir, "a.mp4"))
	os.Remove(filepath.Join(fx.dir, "hardlink.mp4"))
	if r := fx.find(DepthFull); len(r.Changed) < 1 {
		t.Errorf("deleted file should count as changed: %+v", r)
	}
}

func TestInterruptResumes(t *testing.T) {
	fx := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	pol, _ := ParsePolicy(DefaultPolicy, nil, nil)
	finder := &Finder{DB: fx.db, Options: Options{Depth: DepthFull, Policy: pol, Workers: 1, BatchSize: 1,
		Progress: func(e Event) {
			if e.Stage == "Hashing" && e.Bytes > 0 {
				cancel()
			}
		}}}
	r, err := finder.Find(ctx, fx.files())
	if err != nil || !r.Interrupted || len(r.Groups) != 0 {
		t.Fatalf("interrupted: %+v %v", r, err)
	}
	done := r.Hashed
	r = fx.find(DepthFull)
	if r.Interrupted || len(r.Groups) != 2 {
		t.Fatalf("resume: %+v", r)
	}
	// 7 partial hashes (same-size candidates) + 6 full hashes (partial matches), each once.
	if done >= 13 || done+r.Hashed != 13 {
		t.Errorf("resume: first=%d second=%d, want them to split 13", done, r.Hashed)
	}
}

func TestDefaultWorkers(t *testing.T) {
	fx := setup(t)
	r := fx.find(DepthFull, func(o *Options) { o.Workers = 0 })
	if len(r.Groups) != 2 || r.Hashed == 0 {
		t.Errorf("zero Workers should fall back to a default: %+v", r)
	}
}

func TestUnavailable(t *testing.T) {
	fx := setup(t)
	fs := fx.files()
	fs[0].AbsPath = ""
	pol, _ := ParsePolicy(DefaultPolicy, nil, nil)
	r, _ := (&Finder{DB: fx.db, Options: Options{Depth: DepthSize, Policy: pol}}).Find(context.Background(), fs)
	if r.Unavailable != 1 {
		t.Errorf("unavailable = %d", r.Unavailable)
	}
}

func TestParseDepth(t *testing.T) {
	for in, want := range map[string]Depth{"size": DepthSize, "partial": DepthPartial, "full": DepthFull, "": DepthFull} {
		if d, err := ParseDepth(in); err != nil || d != want {
			t.Errorf("ParseDepth(%q) = %v %v", in, d, err)
		}
	}
	if _, err := ParseDepth("deep"); err == nil {
		t.Error("bad depth accepted")
	}
	if DepthPartial.String() != "partial" {
		t.Error("String")
	}
}
