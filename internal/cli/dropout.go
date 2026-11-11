package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/njhoffman/mediacat/internal/dropout"
	"github.com/njhoffman/mediacat/internal/filter"
	"github.com/njhoffman/mediacat/internal/hash"
	"github.com/njhoffman/mediacat/internal/ui"
	"github.com/njhoffman/mediacat/internal/volume"
)

// defaultWait is how long a run waits for a drive that dropped out to come back.
const defaultWait = 10 * time.Minute

// maxRecoveries bounds how often one run waits for drives, so a drive that keeps failing
// ends the run instead of looping.
const maxRecoveries = 5

// waiter watches drives for drop-outs; wait 0 still detects them but does not wait.
func (a *app) waiter(wait time.Duration) *dropout.Waiter {
	w := dropout.NewWaiter(wait, func(msg string) { fmt.Fprintln(a.stderr, ui.Warn.Render(msg)) })
	if a.lookup != nil {
		w.Lookup = a.lookup
	}
	w.Poll = a.dropPoll
	return w
}

// awaitGone waits for every volume among keys that is gone right now. It reports whether
// any had dropped out (and came back), and refreshes the mount table when so.
func (a *app) awaitGone(ctx context.Context, w *dropout.Waiter, keys []string) (bool, error) {
	recovered := false
	for _, k := range keys {
		if !w.Gone(k) {
			continue
		}
		if _, err := w.Await(ctx, k); err != nil {
			return false, err
		}
		recovered = true
	}
	if recovered {
		res, err := volume.NewResolver()
		if err != nil {
			return false, err
		}
		if a.lookup == nil {
			a.res = res
		}
	}
	return recovered, nil
}

// setReadLimit applies --bwlimit (bytes per second, e.g. 40M).
func setReadLimit(spec string) error {
	if spec == "" || spec == "0" {
		hash.SetReadLimit(0)
		return nil
	}
	n, err := filter.ParseSize(spec)
	if err != nil {
		return fmt.Errorf("--bwlimit: %w", err)
	}
	hash.SetReadLimit(n)
	return nil
}
