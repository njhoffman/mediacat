package integrity

import (
	"context"
	"errors"
	"os"
	"sync"
	"time"

	"github.com/njhoffman/mediacat/internal/catalog"
)

// Event reports progress.
type Event struct {
	Done       int
	Total      int
	Bytes      int64
	TotalBytes int64
	Current    string
}

// Options configure a run.
type Options struct {
	Level    string
	Workers  int
	Recheck  bool // check even files with a current result
	Progress func(Event)
	Now      func() time.Time
	// BatchSize and BatchInterval bound how much work is lost if the process dies.
	BatchSize     int
	BatchInterval time.Duration
}

// Summary is the outcome of a run.
type Summary struct {
	Checked     int            // files checked in this run
	Counts      map[string]int // results of this run by status
	Current     int            // files skipped because their result is still current
	Changed     []string       // files that changed since the last scan; not checked
	Unavailable int            // files on volumes that are not mounted
	Interrupted bool
}

// satisfied reports whether an existing result already covers the requested level.
func satisfied(c catalog.Check, level string) bool {
	if c.Status == "" || c.Status == catalog.CheckUnverified {
		return false
	}
	return c.Level == catalog.LevelFull || level == catalog.LevelQuick
}

// Run checks files (with AbsPath set for mounted volumes), storing each result as it
// completes so an interrupted run resumes with the files that are left.
func Run(ctx context.Context, db *catalog.DB, checker Checker, files []catalog.File, o Options) (*Summary, error) {
	if o.Level == "" {
		o.Level = catalog.LevelQuick
	}
	if o.Workers <= 0 {
		o.Workers = 2
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	batchSize, interval := o.BatchSize, o.BatchInterval
	if batchSize <= 0 {
		batchSize = 20
	}
	if interval <= 0 {
		interval = 2 * time.Second
	}
	sum := &Summary{Counts: map[string]int{}}
	var todo []*catalog.File
	ev := Event{}
	for i := range files {
		f := &files[i]
		switch {
		case f.AbsPath == "":
			sum.Unavailable++
		case !o.Recheck && satisfied(f.Check, o.Level):
			sum.Current++
		default:
			todo = append(todo, f)
			ev.TotalBytes += f.Size
		}
	}
	ev.Total = len(todo)

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
	emit(true)
	if len(todo) == 0 {
		return sum, nil
	}

	type res struct {
		f       *catalog.File
		check   catalog.Check
		changed bool
		counted int64
	}
	jobs := make(chan *catalog.File)
	results := make(chan res)
	var wg sync.WaitGroup
	for range o.Workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for f := range jobs {
				st, err := os.Stat(f.AbsPath)
				if err != nil && !errors.Is(err, os.ErrNotExist) {
					results <- res{f: f, check: result(catalog.CheckUnverified, err.Error())}
					continue
				}
				if err != nil || st.Size() != f.Size || st.ModTime().UnixNano() != f.MtimeNS {
					results <- res{f: f, changed: true}
					continue
				}
				var counted int64
				c := checker.Check(ctx, *f, o.Level, func(n int64) {
					counted += n
					mu.Lock()
					ev.Bytes += n
					mu.Unlock()
					emit(false)
				})
				results <- res{f: f, check: c, counted: counted}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, f := range todo {
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
	last := time.Now()
	var werr error
	flush := func() {
		if tx != nil {
			if err := tx.Commit(); err != nil && werr == nil {
				werr = err
			}
			tx, pending, last = nil, 0, time.Now()
		}
	}
	for r := range results {
		mu.Lock()
		ev.Done++
		ev.Bytes += r.f.Size - r.counted // files checked without byte progress count whole
		ev.Current = r.f.AbsPath
		finished := ev.Done == ev.Total
		mu.Unlock()
		emit(finished)
		if r.changed {
			sum.Changed = append(sum.Changed, r.f.AbsPath)
			continue
		}
		// Results cut short by the interrupt say nothing about the file.
		if ctx.Err() != nil && r.check.Status == catalog.CheckUnverified {
			continue
		}
		r.check.At = now()
		r.f.Check = r.check
		sum.Checked++
		sum.Counts[r.check.Status]++
		if werr != nil {
			continue
		}
		if tx == nil {
			if tx, werr = db.Begin(); werr != nil {
				continue
			}
		}
		if err := tx.SetCheck(r.f.ID, r.f.Size, r.f.MtimeNS, r.check); err != nil && werr == nil {
			werr = err
		}
		pending++
		if pending >= batchSize || time.Since(last) >= interval {
			flush()
		}
	}
	flush()
	sum.Interrupted = ctx.Err() != nil
	return sum, werr
}
