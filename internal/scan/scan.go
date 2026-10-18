// Package scan walks a directory tree, detects new, changed, renamed and missing media
// files against the catalog, and probes what changed.
package scan

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/njhoffman/mediacat/internal/catalog"
	"github.com/njhoffman/mediacat/internal/probe"
	"github.com/njhoffman/mediacat/internal/volume"
)

// Phase is the current stage of a scan.
type Phase int

// Scan phases, in order.
const (
	PhaseWalk Phase = iota
	PhaseProbe
)

// Event reports scan progress.
type Event struct {
	Phase      Phase
	Found      int // media files found so far while walking
	Done       int // files probed
	Total      int // files to probe
	Bytes      int64
	TotalBytes int64
	Current    string // relpath most recently finished
}

// Prober extracts metadata; probe.Prober satisfies it.
type Prober interface {
	Probe(ctx context.Context, path, kind string) (probe.Result, error)
}

// DefaultExcludes are directory names never descended into: trash and OS/NAS metadata.
var DefaultExcludes = []string{".Trash", ".Trash-*", ".Trashes", "trashbox", "$RECYCLE.BIN", "System Volume Information", "@eaDir", "#recycle", "lost+found"}

// Options configure a scan.
type Options struct {
	Workers int  // 0 picks a default from the volume type
	Prune   bool // delete rows for files no longer on disk instead of marking them missing
	Force   bool // re-probe every file
	// BaseExcludes replaces DefaultExcludes when non-nil (the config's scan.exclude list);
	// Excludes adds to it.
	BaseExcludes []string
	Excludes     []string
	Progress     func(Event)
	Now          func() time.Time
	// BatchSize and BatchInterval bound how much probe work is lost if the process dies.
	BatchSize     int
	BatchInterval time.Duration
}

// Failure is a file that could not be probed.
type Failure struct {
	RelPath string
	Err     string
}

// Summary is the outcome of a scan.
type Summary struct {
	Volume      volume.Volume
	Root        string // relpath of the scanned directory
	ScanID      int64
	Workers     int
	Seen        int
	Added       int
	Changed     int
	Renamed     int
	Unchanged   int
	Restored    int // previously missing, found again unchanged
	Missing     int // newly marked missing
	Pruned      int
	Failures    []Failure // permanent probe errors, recorded in the catalog
	Deferred    []Failure // transient errors, not recorded; retried next scan
	WalkErrors  []Failure
	Interrupted bool
	Elapsed     time.Duration
}

// Scanner runs scans against a catalog.
type Scanner struct {
	DB       *catalog.DB
	Resolver *volume.Resolver
	Prober   Prober
	Options  Options
}

type found struct {
	rel     string
	abs     string
	kind    string
	size    int64
	mtimeNS int64
	inode   uint64
}

type identity struct {
	inode   uint64
	size    int64
	mtimeNS int64
}

// Run scans dir recursively. A cancelled ctx stops the scan after committing finished
// work; the returned summary then has Interrupted set and err is nil.
func (s *Scanner) Run(ctx context.Context, dir string) (*Summary, error) {
	o := s.Options
	now := o.Now
	if now == nil {
		now = time.Now
	}
	start := now()
	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}
	vol, root, err := s.Resolver.Resolve(dir)
	if err != nil {
		return nil, err
	}
	absRoot, ok := vol.AbsPath(root)
	if !ok {
		return nil, fmt.Errorf("cannot map %s back to its mount", dir)
	}
	sum := &Summary{Volume: vol, Root: root, Workers: workers(o.Workers, vol)}
	volID, err := s.DB.UpsertVolume(vol, start)
	if err != nil {
		return nil, err
	}
	sum.ScanID, err = s.DB.StartScan(volID, root, start)
	if err != nil {
		return nil, err
	}
	// The scan record is closed on every exit path, including interrupts.
	finish := func(status string, cause error) (*Summary, error) {
		sum.Elapsed = now().Sub(start)
		sum.Interrupted = status == catalog.ScanInterrupted
		ferr := s.DB.FinishScan(sum.ScanID, status, sum.Seen, sum.Added, sum.Changed, sum.Missing+sum.Pruned, now())
		if cause != nil {
			return sum, cause
		}
		return sum, ferr
	}
	base := o.BaseExcludes
	if base == nil {
		base = DefaultExcludes
	}
	excludes := append(append([]string{}, base...), o.Excludes...)

	files, walkErrDirs, err := s.walk(ctx, absRoot, root, excludes, sum)
	if err != nil {
		if ctx.Err() != nil {
			return finish(catalog.ScanInterrupted, nil)
		}
		return finish(catalog.ScanFailed, err)
	}
	sum.Seen = len(files)

	known, err := s.DB.Known(volID, root)
	if err != nil {
		return finish(catalog.ScanFailed, err)
	}
	toProbe, err := s.reconcile(files, known, walkErrDirs, excludes, root, sum, now())
	if err != nil {
		return finish(catalog.ScanFailed, err)
	}
	if err := s.probeAll(ctx, volID, toProbe, known, sum, now); err != nil {
		return finish(catalog.ScanFailed, err)
	}
	if ctx.Err() != nil {
		return finish(catalog.ScanInterrupted, nil)
	}
	return finish(catalog.ScanComplete, nil)
}

func workers(n int, v volume.Volume) int {
	if n > 0 {
		return n
	}
	if v.Slow() {
		return 2
	}
	return min(runtime.NumCPU(), 8)
}

// joinRel joins a root relpath and a slash-separated path below it.
func joinRel(root, sub string) string {
	if root == "" {
		return sub
	}
	if sub == "" {
		return root
	}
	return root + "/" + sub
}

func excluded(name string, patterns []string) bool {
	for _, p := range patterns {
		if ok, _ := path.Match(p, name); ok {
			return true
		}
	}
	return false
}

// excludedPath reports whether any component of relpath below root matches an exclude.
func excludedPath(rel, root string, patterns []string) bool {
	sub := strings.TrimPrefix(strings.TrimPrefix(rel, root), "/")
	for _, part := range strings.Split(sub, "/") {
		if excluded(part, patterns) {
			return true
		}
	}
	return false
}

func (s *Scanner) walk(ctx context.Context, absRoot, root string, excludes []string, sum *Summary) ([]found, []string, error) {
	var files []found
	var errDirs []string
	lastEmit := time.Time{}
	emit := func(force bool) {
		if s.Options.Progress != nil && (force || time.Since(lastEmit) > 100*time.Millisecond) {
			lastEmit = time.Now()
			s.Options.Progress(Event{Phase: PhaseWalk, Found: len(files)})
		}
	}
	err := filepath.WalkDir(absRoot, func(p string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		sub, _ := filepath.Rel(absRoot, p)
		sub = filepath.ToSlash(sub)
		if sub == "." {
			sub = ""
		}
		rel := joinRel(root, sub)
		if err != nil {
			if p == absRoot {
				return err
			}
			sum.WalkErrors = append(sum.WalkErrors, Failure{RelPath: rel, Err: err.Error()})
			if d == nil || d.IsDir() {
				errDirs = append(errDirs, rel)
				return nil
			}
			return nil
		}
		if d.IsDir() {
			if p != absRoot && excluded(d.Name(), excludes) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || excluded(d.Name(), excludes) {
			return nil // symlinks, devices, sockets: not followed
		}
		kind := probe.KindOf(d.Name())
		if kind == "" {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			sum.WalkErrors = append(sum.WalkErrors, Failure{RelPath: rel, Err: err.Error()})
			errDirs = append(errDirs, rel) // don't mark it missing either
			return nil
		}
		f := found{rel: rel, abs: p, kind: kind, size: info.Size(), mtimeNS: info.ModTime().UnixNano()}
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			f.inode = st.Ino
		}
		files = append(files, f)
		emit(false)
		return nil
	})
	emit(true)
	return files, errDirs, err
}

func under(rel string, dirs []string) bool {
	for _, d := range dirs {
		if rel == d || strings.HasPrefix(rel, d+"/") {
			return true
		}
	}
	return false
}

// reconcile classifies walked files against the catalog, applies renames, restores and
// missing marks in one transaction, and returns the files that need probing.
func (s *Scanner) reconcile(files []found, known map[string]catalog.Known, errDirs, excludes []string, root string, sum *Summary, now time.Time) ([]found, error) {
	seen := make(map[string]bool, len(files))
	var unchangedRestore []int64
	var fresh, toProbe []found
	for _, f := range files {
		seen[f.rel] = true
		k, ok := known[f.rel]
		switch {
		case !ok:
			fresh = append(fresh, f)
		case !s.Options.Force && k.Size == f.size && k.MtimeNS == f.mtimeNS:
			sum.Unchanged++
			if k.Missing {
				unchangedRestore = append(unchangedRestore, k.ID)
			}
		default:
			toProbe = append(toProbe, f)
		}
	}

	// Files the catalog has that the walk did not see, except where the walk could not
	// look (read errors) or was told not to (excludes).
	gone := map[string]catalog.Known{}
	for rel, k := range known {
		if !seen[rel] && !under(rel, errDirs) && !excludedPath(rel, root, excludes) {
			gone[rel] = k
		}
	}

	// A new path whose (inode, size, mtime) matches exactly one gone file is a rename.
	byID := map[identity][]string{}
	for rel, k := range gone {
		if k.Inode != 0 {
			id := identity{k.Inode, k.Size, k.MtimeNS}
			byID[id] = append(byID[id], rel)
		}
	}
	type rename struct {
		id  int64
		rel string
	}
	var renames []rename
	for _, f := range fresh {
		matches := byID[identity{f.inode, f.size, f.mtimeNS}]
		if f.inode != 0 && len(matches) == 1 && !s.Options.Force {
			old := matches[0]
			renames = append(renames, rename{gone[old].ID, f.rel})
			delete(gone, old)
			delete(byID, identity{f.inode, f.size, f.mtimeNS})
			continue
		}
		toProbe = append(toProbe, f)
	}

	tx, err := s.DB.Begin()
	if err != nil {
		return nil, err
	}
	for _, r := range renames {
		if err := tx.Rename(r.id, r.rel); err != nil {
			tx.Rollback()
			return nil, err
		}
	}
	for _, id := range unchangedRestore {
		if err := tx.Restore(id); err != nil {
			tx.Rollback()
			return nil, err
		}
	}
	for _, k := range gone {
		switch {
		case s.Options.Prune:
			err = tx.Delete(k.ID)
			sum.Pruned++
		case !k.Missing:
			err = tx.MarkMissing(k.ID, now)
			sum.Missing++
		}
		if err != nil {
			tx.Rollback()
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	sum.Renamed = len(renames)
	sum.Restored = len(unchangedRestore)
	sort.Slice(toProbe, func(i, j int) bool { return toProbe[i].rel < toProbe[j].rel })
	return toProbe, nil
}

type result struct {
	f   found
	res probe.Result
	err error
}

func (s *Scanner) probeAll(ctx context.Context, volID int64, files []found, known map[string]catalog.Known, sum *Summary, now func() time.Time) error {
	ev := Event{Phase: PhaseProbe, Total: len(files)}
	for _, f := range files {
		ev.TotalBytes += f.size
	}
	if s.Options.Progress != nil {
		s.Options.Progress(ev)
	}
	if len(files) == 0 {
		return nil
	}
	batchSize := s.Options.BatchSize
	if batchSize <= 0 {
		batchSize = 50
	}
	interval := s.Options.BatchInterval
	if interval <= 0 {
		interval = 2 * time.Second
	}

	// A write failure stops dispatch the same way an interrupt does.
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	jobs := make(chan found)
	results := make(chan result)
	var wg sync.WaitGroup
	for range sum.Workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for f := range jobs {
				r, err := s.Prober.Probe(ctx, f.abs, f.kind)
				results <- result{f, r, err}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, f := range files {
			select {
			case jobs <- f:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() { wg.Wait(); close(results) }()

	var tx *catalog.Tx
	pending := 0
	lastCommit := time.Now()
	commit := func() error {
		if tx == nil {
			return nil
		}
		err := tx.Commit()
		tx, pending, lastCommit = nil, 0, time.Now()
		return err
	}
	var werr error
	for r := range results {
		if werr != nil {
			continue
		}
		// A probe that failed after cancellation was most likely killed; its error says
		// nothing about the file.
		if r.err != nil && ctx.Err() != nil {
			continue
		}
		ev.Done++
		ev.Bytes += r.f.size
		ev.Current = r.f.rel
		if s.Options.Progress != nil {
			s.Options.Progress(ev)
		}
		if errors.Is(r.err, probe.ErrTransient) {
			sum.Deferred = append(sum.Deferred, Failure{r.f.rel, r.err.Error()})
			continue
		}
		file := catalog.File{VolumeID: volID, RelPath: r.f.rel, Size: r.f.size, MtimeNS: r.f.mtimeNS,
			Inode: r.f.inode, Kind: r.f.kind, Meta: r.res, ScannedAt: now()}
		if r.err != nil {
			file.Meta = probe.Result{}
			file.ProbeError = r.err.Error()
			sum.Failures = append(sum.Failures, Failure{r.f.rel, r.err.Error()})
		}
		if tx == nil {
			tx, werr = s.DB.Begin()
		}
		if werr == nil {
			werr = tx.PutFile(file)
		}
		if werr != nil {
			stop()
			continue
		}
		pending++
		if _, ok := known[r.f.rel]; ok {
			sum.Changed++
		} else {
			sum.Added++
		}
		if pending >= batchSize || time.Since(lastCommit) >= interval {
			if werr = commit(); werr != nil {
				stop()
			}
		}
	}
	if werr != nil {
		if tx != nil {
			tx.Rollback()
		}
		return werr
	}
	return commit()
}

// Excluded reports whether a scan of root skips path because a component of it below
// root matches one of patterns.
func Excluded(path, root string, patterns []string) bool {
	return excludedPath(path, root, patterns)
}
