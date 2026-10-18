package dupes

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zeebo/xxh3"

	"github.com/njhoffman/mediacat/internal/catalog"
	"github.com/njhoffman/mediacat/internal/phash"
)

// Levels are the named perceptual similarity levels: the largest Hamming distance between
// 64-bit phashes each accepts, as in Stash's duplicate checker.
var Levels = []struct {
	Name     string
	Distance int
}{{"exact", 0}, {"high", 4}, {"medium", 8}, {"low", 10}}

// ParseLevel parses a level name or a distance from 0 to 32.
func ParseLevel(s string) (int, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	for _, l := range Levels {
		if l.Name == s {
			return l.Distance, nil
		}
	}
	if n, err := strconv.Atoi(s); err == nil && n >= 0 && n <= 32 {
		return n, nil
	}
	return 0, fmt.Errorf("unknown similarity %q (want exact, high, medium, low, or a distance from 0 to 32)", s)
}

// LevelName names a distance, e.g. "high (<= 4)".
func LevelName(d int) string {
	for _, l := range Levels {
		if l.Distance == d {
			return fmt.Sprintf("%s, distance <= %d", l.Name, d)
		}
	}
	return fmt.Sprintf("distance <= %d", d)
}

// Hasher computes perceptual hashes; phash.Hasher is the real one.
type Hasher interface {
	Video(ctx context.Context, path string, duration float64) (uint64, error)
	Image(ctx context.Context, path string) (uint64, error)
}

// SimilarOptions configure a perceptual search.
type SimilarOptions struct {
	Distance int
	// Videos are compared only when their durations differ by at most DurationPct percent
	// of the longer one, or DurationMin, whichever is larger.
	DurationPct  float64
	DurationMin  time.Duration
	MinImageSize int // images narrower or shorter than this (in pixels) are not compared
	Workers      int
	Policy       Policy
	Hasher       Hasher
	// Stash returns Stash's phash for a file when Stash's record still matches it.
	Stash    func(catalog.File) (uint64, bool)
	Progress func(Event)
	// BatchSize and BatchInterval bound how much hashing work is lost if the process dies.
	BatchSize     int
	BatchInterval time.Duration
}

// SimilarFinder searches catalog files for perceptual duplicates.
type SimilarFinder struct {
	DB      *catalog.DB
	Options SimilarOptions
}

// Find groups videos and images (with AbsPath set for mounted volumes) whose phashes are
// within the distance, computing and storing the phashes that are missing. Groups are
// connected sets: every file is within the distance of at least one other in its group.
func (sf *SimilarFinder) Find(ctx context.Context, files []catalog.File) (*Result, error) {
	o := sf.Options
	if o.Workers <= 0 {
		o.Workers = 2
	}
	res := &Result{Depth: DepthFull, Similar: true, Distance: o.Distance}

	var todo, stashed, ready []*catalog.File
	for _, fl := range available(files, res) {
		switch {
		case fl.Kind != "video" && fl.Kind != "image":
			res.Skipped++
			continue
		case fl.Kind == "image" && o.MinImageSize > 0 && fl.Meta.Width > 0 &&
			(fl.Meta.Width < o.MinImageSize || fl.Meta.Height < o.MinImageSize):
			res.SmallImages++
			continue
		case fl.PHash.Source == catalog.PHashNone:
			if strings.Contains(fl.PHash.Error, phash.ErrTooSmall.Error()) {
				res.SmallImages++
			} else {
				res.Unhashable = append(res.Unhashable, Failure{fl.AbsPath, fl.PHash.Error})
			}
			continue
		}
		res.Candidates++
		switch {
		case fl.PHash.OK():
			ready = append(ready, fl)
		case fl.Kind == "video" && o.Stash != nil:
			if v, ok := o.Stash(*fl); ok {
				fl.PHash = catalog.PHash{Value: v, Source: catalog.PHashStash}
				stashed = append(stashed, fl)
				continue
			}
			todo = append(todo, fl)
		default:
			todo = append(todo, fl)
		}
	}

	st := newStore(sf.DB, o.BatchSize, o.BatchInterval)
	for _, fl := range stashed {
		st.put(fl)
	}
	ready = append(ready, stashed...)
	if len(todo) > 0 {
		ready = append(ready, sf.hashAll(ctx, todo, res, st)...)
	}
	if err := st.close(); err != nil {
		return res, err
	}
	if ctx.Err() != nil {
		res.Interrupted = true
		return res, nil
	}
	for _, fl := range ready {
		if fl.PHash.Source == catalog.PHashStash {
			res.FromStash++
		}
	}

	for _, set := range cluster(ready, o) {
		g := Group{Confirmed: true, Similar: true}
		for _, fl := range set {
			g.Files = append(g.Files, *fl)
		}
		sort.Slice(g.Files, func(i, j int) bool { return g.Files[i].AbsPath < g.Files[j].AbsPath })
		g.Keep = o.Policy.Choose(g.Files)
		g.Size = g.Files[g.Keep].Size
		g.Key = similarKey(g.Files)
		res.Groups = append(res.Groups, g)
	}
	sort.Slice(res.Groups, func(i, j int) bool {
		a, b := res.Groups[i], res.Groups[j]
		if a.Reclaimable() != b.Reclaimable() {
			return a.Reclaimable() > b.Reclaimable()
		}
		return a.Key < b.Key
	})
	return res, nil
}

// similarKey identifies a group by its members' identities and versions, so saved review
// choices apply only while the group is unchanged.
func similarKey(files []catalog.File) string {
	var b strings.Builder
	for _, f := range files {
		fmt.Fprintf(&b, "%s\x00%s\x00%d\x00%d\n", f.VolumeKey, f.RelPath, f.Size, f.MtimeNS)
	}
	h := xxh3.HashString128(b.String()).Bytes()
	return fmt.Sprintf("similar:%x", h[:])
}

// cluster links files within the distance (videos only when their durations are close)
// and returns the connected sets with more than one file.
func cluster(files []*catalog.File, o SimilarOptions) [][]*catalog.File {
	parent := make([]int, len(files))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		for parent[i] != i {
			parent[i] = parent[parent[i]]
			i = parent[i]
		}
		return i
	}
	union := func(a, b int) {
		if ra, rb := find(a), find(b); ra != rb {
			parent[rb] = ra
		}
	}
	near := func(a, b *catalog.File) bool {
		return phash.Distance(a.PHash.Value, b.PHash.Value) <= o.Distance
	}

	var videos, images []int
	for i, f := range files {
		if f.Kind == "video" {
			videos = append(videos, i)
		} else {
			images = append(images, i)
		}
	}
	// Videos sorted by duration: once the next one is too long to match, so are the rest.
	sort.SliceStable(videos, func(a, b int) bool { return files[videos[a]].Meta.DurationMS < files[videos[b]].Meta.DurationMS })
	minMS := float64(o.DurationMin.Milliseconds())
	for a, i := range videos {
		di := float64(files[i].Meta.DurationMS)
		for _, j := range videos[a+1:] {
			dj := float64(files[j].Meta.DurationMS)
			if dj-di > max(o.DurationPct/100*dj, minMS) {
				break
			}
			if near(files[i], files[j]) {
				union(i, j)
			}
		}
	}
	for a, i := range images {
		for _, j := range images[a+1:] {
			if near(files[i], files[j]) {
				union(i, j)
			}
		}
	}

	sets := map[int][]*catalog.File{}
	var roots []int
	for i, f := range files {
		r := find(i)
		if _, ok := sets[r]; !ok {
			roots = append(roots, r)
		}
		sets[r] = append(sets[r], f)
	}
	var out [][]*catalog.File
	for _, r := range roots {
		if len(sets[r]) > 1 {
			out = append(out, sets[r])
		}
	}
	return out
}

// store writes phashes in batches.
type store struct {
	db        *catalog.DB
	tx        *catalog.Tx
	pending   int
	last      time.Time
	batchSize int
	interval  time.Duration
	err       error
}

func newStore(db *catalog.DB, batchSize int, interval time.Duration) *store {
	if batchSize <= 0 {
		batchSize = 20
	}
	if interval <= 0 {
		interval = 2 * time.Second
	}
	return &store{db: db, last: time.Now(), batchSize: batchSize, interval: interval}
}

func (s *store) put(f *catalog.File) {
	if s.err != nil {
		return
	}
	if s.tx == nil {
		if s.tx, s.err = s.db.Begin(); s.err != nil {
			return
		}
	}
	if err := s.tx.SetPHash(f.ID, f.Size, f.MtimeNS, f.PHash); err != nil {
		s.err = err
		return
	}
	s.pending++
	if s.pending >= s.batchSize || time.Since(s.last) >= s.interval {
		s.flush()
	}
}

func (s *store) flush() {
	if s.tx != nil {
		if err := s.tx.Commit(); err != nil && s.err == nil {
			s.err = err
		}
		s.tx, s.pending, s.last = nil, 0, time.Now()
	}
}

func (s *store) close() error {
	s.flush()
	return s.err
}

type phashJob struct {
	f   *catalog.File
	val uint64
	err error
}

// hashAll computes phashes for todo and returns the files that got one.
func (sf *SimilarFinder) hashAll(ctx context.Context, todo []*catalog.File, res *Result, st *store) []*catalog.File {
	o := sf.Options
	ev := Event{Stage: "Perceptual hashing", Total: len(todo)}
	if o.Progress != nil {
		o.Progress(ev)
	}
	jobs := make(chan *catalog.File)
	results := make(chan phashJob)
	var wg sync.WaitGroup
	for range o.Workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for fl := range jobs {
				results <- sf.hashOne(ctx, fl)
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, fl := range todo {
			select {
			case jobs <- fl:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() { wg.Wait(); close(results) }()

	var out []*catalog.File
	for r := range results {
		fl := r.f
		ev.Done++
		ev.Current = fl.AbsPath
		if o.Progress != nil {
			o.Progress(ev)
		}
		switch {
		case r.err != nil && ctx.Err() != nil:
			continue
		case errors.Is(r.err, errChanged):
			res.Changed = append(res.Changed, fl.AbsPath)
			continue
		case errors.Is(r.err, phash.ErrTransient):
			res.Deferred = append(res.Deferred, Failure{fl.AbsPath, r.err.Error()})
			continue
		case errors.Is(r.err, phash.ErrTooSmall):
			res.SmallImages++
			res.Candidates--
			fl.PHash = catalog.PHash{Source: catalog.PHashNone, Error: r.err.Error()}
		case r.err != nil:
			res.Unhashable = append(res.Unhashable, Failure{fl.AbsPath, r.err.Error()})
			res.Candidates--
			fl.PHash = catalog.PHash{Source: catalog.PHashNone, Error: r.err.Error()}
		default:
			res.Hashed++
			fl.PHash = catalog.PHash{Value: r.val, Source: catalog.PHashMediacat}
			out = append(out, fl)
		}
		st.put(fl)
	}
	return out
}

func (sf *SimilarFinder) hashOne(ctx context.Context, fl *catalog.File) phashJob {
	if err := ctx.Err(); err != nil {
		return phashJob{f: fl, err: err}
	}
	st, err := os.Stat(fl.AbsPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return phashJob{f: fl, err: errChanged}
		}
		return phashJob{f: fl, err: fmt.Errorf("%w: %v", phash.ErrTransient, err)}
	}
	if st.Size() != fl.Size || st.ModTime().UnixNano() != fl.MtimeNS {
		return phashJob{f: fl, err: errChanged}
	}
	var v uint64
	if fl.Kind == "video" {
		v, err = sf.Options.Hasher.Video(ctx, fl.AbsPath, float64(fl.Meta.DurationMS)/1000)
	} else {
		v, err = sf.Options.Hasher.Image(ctx, fl.AbsPath)
	}
	return phashJob{f: fl, val: v, err: err}
}
