// Package remount mounts a removable drive read-only for the length of a run, so a drive
// that drops out mid-scan cannot be left with a damaged filesystem, and restores it after.
package remount

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/njhoffman/mediacat/internal/volume"
)

// Mounter mounts and unmounts block devices as the user (no root needed).
type Mounter interface {
	Unmount(ctx context.Context, dev string) error
	Mount(ctx context.Context, dev string, readOnly bool) error
}

// Udisks uses udisksctl, which the desktop's polkit rules allow for removable drives.
type Udisks struct{}

func (Udisks) Unmount(ctx context.Context, dev string) error {
	return run(ctx, "udisksctl", "unmount", "--no-user-interaction", "-b", dev)
}

func (Udisks) Mount(ctx context.Context, dev string, readOnly bool) error {
	args := []string{"mount", "--no-user-interaction", "-b", dev}
	if readOnly {
		args = append(args, "-o", "ro")
	}
	return run(ctx, "udisksctl", args...)
}

func run(ctx context.Context, name string, args ...string) error {
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(out.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("%s %s: %s", name, args[0], msg)
	}
	return nil
}

// ErrNotRemovable is returned for drives that are not removable block devices.
var ErrNotRemovable = errors.New("only removable drives (USB, card readers) are remounted read-only")

// Session is a drive remounted read-only for a run.
type Session struct {
	Mounter Mounter
	Lookup  func(key string) (volume.Volume, bool)
	Key     string
	Device  string
	changed bool // whether this session made it read-only (and must restore it)
}

// ReadOnly makes v read-only and returns the session and the volume as now mounted (its
// mount point may change). A drive that is already read-only is left as it is, and Restore
// then does nothing. A busy drive is not touched.
func ReadOnly(ctx context.Context, m Mounter, lookup func(string) (volume.Volume, bool), v volume.Volume) (*Session, volume.Volume, error) {
	if !v.Removable || !strings.HasPrefix(v.Source, "/dev/") {
		return nil, v, ErrNotRemovable
	}
	s := &Session{Mounter: m, Lookup: lookup, Key: v.Key, Device: v.Source}
	if v.ReadOnly() {
		return s, v, nil
	}
	nv, err := s.apply(ctx)
	if err != nil {
		return nil, v, err
	}
	return s, nv, nil
}

// Reapply makes the drive read-only again after it came back from a drop-out (the
// desktop automounts it read-write).
func (s *Session) Reapply(ctx context.Context) (volume.Volume, error) {
	if s == nil {
		return volume.Volume{}, errors.New("no read-only session")
	}
	v, ok := s.Lookup(s.Key)
	if ok && v.ReadOnly() {
		return v, nil
	}
	return s.apply(ctx)
}

func (s *Session) apply(ctx context.Context) (volume.Volume, error) {
	if err := s.Mounter.Unmount(ctx, s.Device); err != nil {
		return volume.Volume{}, fmt.Errorf("cannot remount %s read-only, it is probably in use (close programs using it): %w", s.Device, err)
	}
	if err := s.Mounter.Mount(ctx, s.Device, true); err != nil {
		// Put it back the way it was.
		if rerr := s.Mounter.Mount(context.WithoutCancel(ctx), s.Device, false); rerr != nil {
			return volume.Volume{}, fmt.Errorf("mounting %s read-only failed (%v) and so did mounting it again: %w", s.Device, err, rerr)
		}
		return volume.Volume{}, fmt.Errorf("mounting %s read-only failed; it is mounted as before: %w", s.Device, err)
	}
	s.changed = true
	v, ok := s.Lookup(s.Key)
	if !ok {
		return volume.Volume{}, fmt.Errorf("%s was remounted read-only but is not in the mount table", s.Device)
	}
	return v, nil
}

// Changed reports whether the session made the drive read-only (it was writable before).
func (s *Session) Changed() bool { return s != nil && s.changed }

// Restore mounts the drive writable again when this session made it read-only.
func (s *Session) Restore(ctx context.Context) error {
	if !s.Changed() {
		return nil
	}
	ctx = context.WithoutCancel(ctx) // restore even after Ctrl-C
	if err := s.Mounter.Unmount(ctx, s.Device); err != nil {
		return fmt.Errorf("could not restore %s to read-write (it stays read-only; unmount and mount it again): %w", s.Device, err)
	}
	if err := s.Mounter.Mount(ctx, s.Device, false); err != nil {
		return fmt.Errorf("could not mount %s again after the run: %w", s.Device, err)
	}
	s.changed = false
	return nil
}
