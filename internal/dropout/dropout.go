// Package dropout tells when a drive has dropped out (unplugged, reset, or its mount gone)
// and waits for it to come back, possibly at another mount point, so a long run can
// continue instead of failing every remaining file.
package dropout

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/njhoffman/mediacat/internal/volume"
)

// Waiter watches volumes by key. Its zero Timeout disables waiting.
type Waiter struct {
	// Lookup finds a volume in the current mount table (read fresh on every call).
	Lookup  func(key string) (volume.Volume, bool)
	Timeout time.Duration
	Poll    time.Duration // default 2s
	// Notify reports waiting and recovery.
	Notify func(msg string)

	mu sync.Mutex // one wait at a time; other workers wait on the same recovery
}

// NewWaiter watches volumes through fresh mount tables.
func NewWaiter(timeout time.Duration, notify func(string)) *Waiter {
	return &Waiter{Lookup: Lookup, Timeout: timeout, Notify: notify}
}

// Lookup reads the mount table and finds the volume with key.
func Lookup(key string) (volume.Volume, bool) {
	r, err := volume.NewResolver()
	if err != nil {
		return volume.Volume{}, false
	}
	return r.Lookup(key)
}

// Gone reports whether the volume is unusable right now: not mounted, or its mount point
// cannot be read.
func (w *Waiter) Gone(key string) bool {
	if w == nil || w.Lookup == nil {
		return false
	}
	v, ok := w.Lookup(key)
	if !ok {
		return true
	}
	_, err := os.ReadDir(v.MountPoint)
	return err != nil
}

// Candidate reports whether err could come from a drive dropping out, so that Gone is
// worth checking.
func Candidate(err error) bool {
	if err == nil {
		return false
	}
	for _, e := range []error{syscall.EIO, syscall.ENODEV, syscall.ENXIO, syscall.ENOENT, syscall.ESTALE,
		syscall.ENOTCONN, syscall.EHOSTDOWN, syscall.ETIMEDOUT, syscall.ESHUTDOWN} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// ErrNotBack is returned when the volume did not return in time.
var ErrNotBack = errors.New("the drive did not come back")

// Await waits until the volume is mounted and readable again and returns it, at its new
// mount point if it moved. It returns at once when the volume is fine.
func (w *Waiter) Await(ctx context.Context, key string) (volume.Volume, error) {
	if w == nil || w.Timeout <= 0 {
		return volume.Volume{}, fmt.Errorf("%w (waiting is off)", ErrNotBack)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	poll := w.Poll
	if poll <= 0 {
		poll = 2 * time.Second
	}
	ready := func() (volume.Volume, bool) {
		v, ok := w.Lookup(key)
		if !ok {
			return v, false
		}
		_, err := os.ReadDir(v.MountPoint)
		return v, err == nil
	}
	if v, ok := ready(); ok {
		return v, nil
	}
	w.notify(fmt.Sprintf("Drive %s dropped out; waiting up to %s for it to come back (Ctrl-C stops)", key, w.Timeout))
	deadline := time.Now().Add(w.Timeout)
	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return volume.Volume{}, ctx.Err()
		case <-t.C:
		}
		if v, ok := ready(); ok {
			w.notify(fmt.Sprintf("Drive %s is back at %s; continuing", key, v.MountPoint))
			return v, nil
		}
		if time.Now().After(deadline) {
			return volume.Volume{}, fmt.Errorf("%w within %s: %s", ErrNotBack, w.Timeout, key)
		}
	}
}

func (w *Waiter) notify(msg string) {
	if w.Notify != nil {
		w.Notify(msg)
	}
}

// Remap moves path from one mount point to another (for a volume that came back
// elsewhere). Paths outside oldMount are returned unchanged.
func Remap(path, oldMount, newMount string) string {
	if oldMount == newMount || oldMount == "/" {
		return path
	}
	if path == oldMount {
		return newMount
	}
	if strings.HasPrefix(path, oldMount+"/") {
		return newMount + path[len(oldMount):]
	}
	return path
}
