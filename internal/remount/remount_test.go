package remount

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/njhoffman/mediacat/internal/volume"
)

// fake mounts by changing what lookup returns.
type fake struct {
	mounted  bool
	ro       bool
	busy     bool
	failRO   bool
	calls    []string
	mountDir string
}

func (f *fake) Unmount(_ context.Context, dev string) error {
	f.calls = append(f.calls, "unmount")
	if f.busy {
		return errors.New("target is busy")
	}
	f.mounted = false
	return nil
}

func (f *fake) Mount(_ context.Context, dev string, ro bool) error {
	f.calls = append(f.calls, map[bool]string{true: "mount-ro", false: "mount-rw"}[ro])
	if ro && f.failRO {
		return errors.New("mount option ro not allowed")
	}
	f.mounted, f.ro = true, ro
	return nil
}

func (f *fake) lookup(key string) (volume.Volume, bool) {
	if !f.mounted {
		return volume.Volume{}, false
	}
	opts := "rw,relatime"
	if f.ro {
		opts = "ro,relatime"
	}
	return volume.Volume{Key: key, Source: "/dev/sdz1", MountPoint: f.mountDir, Options: opts, Removable: true}, true
}

func TestReadOnlySession(t *testing.T) {
	f := &fake{mounted: true, mountDir: "/media/u/wd1"}
	v, _ := f.lookup("uuid:1")
	s, nv, err := ReadOnly(context.Background(), f, f.lookup, v)
	if err != nil || !nv.ReadOnly() || !s.Changed() || nv.MountPoint != "/media/u/wd1" {
		t.Fatalf("ReadOnly: %+v %v", nv, err)
	}
	// After a drop-out the desktop mounts it writable again; Reapply fixes that.
	f.ro = false
	if v, err := s.Reapply(context.Background()); err != nil || !v.ReadOnly() {
		t.Errorf("Reapply: %+v %v", v, err)
	}
	if err := s.Restore(context.Background()); err != nil || f.ro || !f.mounted {
		t.Errorf("Restore: %v ro=%v", err, f.ro)
	}
	if strings.Join(f.calls, " ") != "unmount mount-ro unmount mount-ro unmount mount-rw" {
		t.Errorf("calls: %v", f.calls)
	}
}

func TestReadOnlyEdgeCases(t *testing.T) {
	// Already read-only: nothing is changed and nothing restored.
	f := &fake{mounted: true, ro: true}
	v, _ := f.lookup("k")
	s, _, err := ReadOnly(context.Background(), f, f.lookup, v)
	if err != nil || s.Changed() || len(f.calls) != 0 || s.Restore(context.Background()) != nil || len(f.calls) != 0 {
		t.Errorf("already read-only: %v %v", err, f.calls)
	}
	// Not removable.
	if _, _, err := ReadOnly(context.Background(), f, f.lookup, volume.Volume{Source: "/dev/nvme0n1p2"}); !errors.Is(err, ErrNotRemovable) {
		t.Errorf("internal drive: %v", err)
	}
	// Busy: refused, left mounted.
	f = &fake{mounted: true, busy: true}
	v, _ = f.lookup("k")
	if _, _, err := ReadOnly(context.Background(), f, f.lookup, v); err == nil || !strings.Contains(err.Error(), "in use") || !f.mounted {
		t.Errorf("busy: %v", err)
	}
	// Read-only mount refused: mounted again as before.
	f = &fake{mounted: true, failRO: true}
	v, _ = f.lookup("k")
	if _, _, err := ReadOnly(context.Background(), f, f.lookup, v); err == nil || !strings.Contains(err.Error(), "mounted as before") || !f.mounted || f.ro {
		t.Errorf("ro refused: %v %+v", err, f)
	}
}
