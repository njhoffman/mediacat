package dropout

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/njhoffman/mediacat/internal/volume"
)

func TestAwait(t *testing.T) {
	back := t.TempDir()
	var calls atomic.Int32
	var msgs []string
	w := &Waiter{Timeout: time.Second, Poll: 10 * time.Millisecond, Notify: func(m string) { msgs = append(msgs, m) },
		Lookup: func(key string) (volume.Volume, bool) {
			// Gone for the first three looks, then back at a new mount point.
			if calls.Add(1) <= 3 {
				return volume.Volume{}, false
			}
			return volume.Volume{Key: key, MountPoint: back}, true
		}}
	if !w.Gone("uuid:x") {
		t.Error("the volume should look gone")
	}
	v, err := w.Await(context.Background(), "uuid:x")
	if err != nil || v.MountPoint != back {
		t.Fatalf("Await = %+v %v", v, err)
	}
	if len(msgs) != 2 {
		t.Errorf("notices: %q", msgs)
	}
	if w.Gone("uuid:x") {
		t.Error("the volume is back")
	}
}

func TestAwaitTimesOut(t *testing.T) {
	w := &Waiter{Timeout: 50 * time.Millisecond, Poll: 10 * time.Millisecond,
		Lookup: func(string) (volume.Volume, bool) { return volume.Volume{}, false }}
	if _, err := w.Await(context.Background(), "k"); !errors.Is(err, ErrNotBack) {
		t.Errorf("want ErrNotBack, got %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w.Timeout = time.Hour
	if _, err := w.Await(ctx, "k"); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled wait: %v", err)
	}
	var off *Waiter
	if _, err := off.Await(context.Background(), "k"); !errors.Is(err, ErrNotBack) || off.Gone("k") {
		t.Errorf("a nil waiter never waits and never reports gone: %v", err)
	}
	// A mount point that cannot be read counts as gone.
	w.Lookup = func(key string) (volume.Volume, bool) { return volume.Volume{MountPoint: "/nonexistent/mount"}, true }
	if !w.Gone("k") {
		t.Error("unreadable mount point should be gone")
	}
}

func TestCandidateAndRemap(t *testing.T) {
	if !Candidate(fmt.Errorf("read: %w", syscall.EIO)) || !Candidate(syscall.ENODEV) || Candidate(errors.New("bad data")) || Candidate(nil) {
		t.Error("Candidate")
	}
	for _, c := range [][4]string{
		{"/media/u/wd/a/b.mp4", "/media/u/wd", "/media/u/wd1", "/media/u/wd1/a/b.mp4"},
		{"/media/u/wd", "/media/u/wd", "/media/u/wd1", "/media/u/wd1"},
		{"/media/u/wdx/a", "/media/u/wd", "/media/u/wd1", "/media/u/wdx/a"},
		{"/a/b", "/a", "/a", "/a/b"},
	} {
		if got := Remap(c[0], c[1], c[2]); got != c[3] {
			t.Errorf("Remap(%s) = %s, want %s", c[0], got, c[3])
		}
	}
}
