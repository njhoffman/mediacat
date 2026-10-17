// Package dupes finds exact duplicate files in tiers (size, partial hash, full hash),
// caching hashes in the catalog so interrupted runs resume where they stopped.
package dupes

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/njhoffman/mediacat/internal/catalog"
	"github.com/njhoffman/mediacat/internal/hash"
)

// Depth is how thoroughly candidates are compared.
type Depth int

// Depths, from cheapest to exact.
const (
	DepthSize Depth = iota
	DepthPartial
	DepthFull
)

// ParseDepth parses "size", "partial" or "full".
func ParseDepth(s string) (Depth, error) {
	switch s {
	case "size":
		return DepthSize, nil
	case "partial":
		return DepthPartial, nil
	case "full", "":
		return DepthFull, nil
	}
	return 0, fmt.Errorf("unknown depth %q (want size, partial or full)", s)
}

func (d Depth) String() string {
	return [...]string{"size", "partial", "full"}[d]
}

// Group is a set of files with identical content (or identical size/partial hash at
// lower depths). Files[Keep] is the copy chosen to keep.
type Group struct {
	Key       string
	Size      int64
	Files     []catalog.File
	Keep      int
	Confirmed bool // full-hash match: safe to act on
}

// Reclaimable is the space freed by removing every copy but one.
func (g Group) Reclaimable() int64 { return g.Size * int64(len(g.Files)-1) }

// Event reports hashing progress.
type Event struct {
	Stage      string
	Done       int
	Total      int
	Bytes      int64
	TotalBytes int64
	Current    string
}

// Failure is a file that could not be hashed.
type Failure struct {
	Path string
	Err  string
}

// Result is the outcome of a search.
type Result struct {
	Depth       Depth
	Groups      []Group
	Candidates  int // files sharing a size with at least one other file
	Hashed      int // hashes computed in this run (the rest came from the catalog)
	Hardlinks   int // extra paths to an already-counted inode, ignored
	Unavailable int // files on volumes that are not mounted
	Changed     []string
	Deferred    []Failure
	Failed      []Failure
	Interrupted bool
}

// Options configure a search.
type Options struct {
	Depth    Depth
	Workers  int
	Policy   Policy
	Progress func(Event)
	// BatchSize and BatchInterval bound how much hashing work is lost if the process dies.
	BatchSize     int
	BatchInterval time.Duration
}

// Finder searches catalog files for duplicates.
type Finder struct {
	DB      *catalog.DB
	Options Options
}

type inodeKey struct {
	volume int64
	inode  uint64
}

// Find groups files (which must have AbsPath set for mounted volumes; files without one
// are treated as unavailable) into duplicate sets.
func (f *Finder) Find(ctx context.Context, files []catalog.File) (*Result, error) {
	o := f.Options
	if o.Workers <= 0 {
		o.Workers = 2
	}
	f = &Finder{DB: f.DB, Options: o} // stages below read the defaulted options
	res := &Result{Depth: o.Depth}

	var avail []*catalog.File
	seenInode := map[inodeKey]bool{}
	for i := range files {
		fl := &files[i]
		if fl.AbsPath == "" {
			res.Unavailable++
			continue
		}
		if fl.Inode != 0 {
			k := inodeKey{fl.VolumeID, fl.Inode}
			if seenInode[k] {
				res.Hardlinks++
				continue
			}
			seenInode[k] = true
		}
		avail = append(avail, fl)
	}

	groups := groupBy(avail, func(fl *catalog.File) string { return fmt.Sprint(fl.Size) })
	for _, g := range groups {
		res.Candidates += len(g)
	}
	if o.Depth >= DepthPartial && ctx.Err() == nil {
		groups = f.refine(ctx, res, groups, "Comparing samples", false)
	}
	if o.Depth >= DepthFull && ctx.Err() == nil {
		groups = f.refine(ctx, res, groups, "Hashing", true)
	}
	if ctx.Err() != nil {
		res.Interrupted = true
		return res, nil
	}

	for _, g := range groups {
		out := Group{Size: g[0].Size, Confirmed: o.Depth == DepthFull}
		switch o.Depth {
		case DepthFull:
			out.Key = g[0].FullHash
		case DepthPartial:
			out.Key = g[0].PartialHash
		default:
			out.Key = fmt.Sprintf("size:%d", g[0].Size)
		}
		for _, fl := range g {
			out.Files = append(out.Files, *fl)
		}
		out.Keep = o.Policy.Choose(out.Files)
		res.Groups = append(res.Groups, out)
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

// groupBy buckets files by key and keeps buckets with more than one file, in a stable order.
func groupBy(files []*catalog.File, key func(*catalog.File) string) [][]*catalog.File {
	m := map[string][]*catalog.File{}
	var order []string
	for _, fl := range files {
		k := key(fl)
		if _, ok := m[k]; !ok {
			order = append(order, k)
		}
		m[k] = append(m[k], fl)
	}
	var out [][]*catalog.File
	for _, k := range order {
		if g := m[k]; len(g) > 1 {
			sort.Slice(g, func(i, j int) bool { return g[i].AbsPath < g[j].AbsPath })
			out = append(out, g)
		}
	}
	return out
}

type job struct {
	f   *catalog.File
	val string
	err error
}

// refine hashes every file in groups that lacks a valid hash for this stage, then
// regroups by it. Files that changed since the last scan or failed are dropped.
func (f *Finder) refine(ctx context.Context, res *Result, groups [][]*catalog.File, stage string, full bool) [][]*catalog.File {
	o := f.Options
	have := func(fl *catalog.File) string {
		if full {
			return fl.FullHash
		}
		return fl.PartialHash
	}
	var todo []*catalog.File
	ev := Event{Stage: stage}
	for _, g := range groups {
		for _, fl := range g {
			if have(fl) == "" {
				todo = append(todo, fl)
				ev.TotalBytes += fl.Size
			}
		}
	}
	ev.Total = len(todo)
	if o.Progress != nil {
		o.Progress(ev)
	}

	dropped := map[*catalog.File]bool{}
	if len(todo) > 0 {
		f.hashAll(ctx, todo, full, ev, res, dropped)
	}

	var kept []*catalog.File
	for _, g := range groups {
		for _, fl := range g {
			if !dropped[fl] && have(fl) != "" {
				kept = append(kept, fl)
			}
		}
	}
	return groupBy(kept, func(fl *catalog.File) string { return fmt.Sprintf("%d:%s", fl.Size, have(fl)) })
}

func (f *Finder) hashAll(ctx context.Context, todo []*catalog.File, full bool, ev Event, res *Result, dropped map[*catalog.File]bool) {
	o := f.Options
	batchSize, interval := o.BatchSize, o.BatchInterval
	if batchSize <= 0 {
		batchSize = 50
	}
	if interval <= 0 {
		interval = 2 * time.Second
	}
	// ev is shared with workers through the byte-progress callback; every access holds mu.
	var mu sync.Mutex
	var lastEmit time.Time
	emit := func(force bool) {
		mu.Lock()
		if !force && time.Since(lastEmit) < 200*time.Millisecond {
			mu.Unlock()
			return
		}
		lastEmit = time.Now()
		snap := ev
		mu.Unlock()
		if o.Progress != nil {
			o.Progress(snap)
		}
	}
	progress := func(n int64) {
		mu.Lock()
		ev.Bytes += n
		mu.Unlock()
		emit(false)
	}

	jobs := make(chan *catalog.File)
	results := make(chan job)
	var wg sync.WaitGroup
	for range o.Workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for fl := range jobs {
				results <- f.hashOne(ctx, fl, full, progress)
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

	var tx *catalog.Tx
	pending := 0
	last := time.Now()
	flush := func() {
		if tx != nil {
			tx.Commit()
			tx, pending, last = nil, 0, time.Now()
		}
	}
	defer flush()

	for r := range results {
		fl := r.f
		mu.Lock()
		if !full {
			ev.Bytes += fl.Size // samples stand in for the whole file
		}
		ev.Done++
		ev.Current = fl.AbsPath
		finished := ev.Done == ev.Total
		mu.Unlock()
		emit(finished)
		switch {
		case r.err != nil && ctx.Err() != nil:
			dropped[fl] = true
			continue
		case errors.Is(r.err, errChanged):
			res.Changed = append(res.Changed, fl.AbsPath)
			dropped[fl] = true
			continue
		case errors.Is(r.err, hash.ErrTransient):
			res.Deferred = append(res.Deferred, Failure{fl.AbsPath, r.err.Error()})
			dropped[fl] = true
			continue
		case r.err != nil:
			res.Failed = append(res.Failed, Failure{fl.AbsPath, r.err.Error()})
			dropped[fl] = true
			continue
		}
		if full {
			fl.FullHash = r.val
		} else {
			fl.PartialHash = r.val
		}
		res.Hashed++
		if tx == nil {
			var err error
			if tx, err = f.DB.Begin(); err != nil {
				tx = nil
				continue
			}
		}
		// Store both so a full hash never outlives the partial hash it was computed with.
		if err := tx.SetHashes(fl.ID, fl.Size, fl.MtimeNS, fl.PartialHash, fl.FullHash); err == nil {
			pending++
		}
		if pending >= batchSize || time.Since(last) >= interval {
			flush()
		}
	}
}

var errChanged = errors.New("changed since last scan")

func (f *Finder) hashOne(ctx context.Context, fl *catalog.File, full bool, progress func(int64)) job {
	if err := ctx.Err(); err != nil {
		return job{f: fl, err: err}
	}
	st, err := os.Stat(fl.AbsPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return job{f: fl, err: errChanged}
		}
		return job{f: fl, err: err}
	}
	if st.Size() != fl.Size || st.ModTime().UnixNano() != fl.MtimeNS {
		return job{f: fl, err: errChanged}
	}
	var val string
	if full {
		val, err = hash.Full(ctx, fl.AbsPath, progress)
	} else {
		val, err = hash.Partial(fl.AbsPath, fl.Size)
	}
	return job{f: fl, val: val, err: err}
}
