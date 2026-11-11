package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/njhoffman/mediacat/internal/dropout"
	"github.com/njhoffman/mediacat/internal/remount"
	"github.com/njhoffman/mediacat/internal/ui"
)

// withReadOnly runs fn with dir's drive mounted read-only when want is set, and mounts it
// writable again afterwards. explicit is whether --readonly was given: then a drive that
// cannot be remounted is an error; from the config default it is silently skipped.
func (a *app) withReadOnly(ctx context.Context, dir string, want, explicit bool, fn func(dir string) error) error {
	if !want {
		return fn(dir)
	}
	v, _, err := a.res.Resolve(dir)
	if err != nil {
		return err
	}
	lookup := a.lookup
	if lookup == nil {
		lookup = dropout.Lookup
	}
	m := a.mounter
	if m == nil {
		m = remount.Udisks{}
	}
	s, nv, err := remount.ReadOnly(ctx, m, lookup, v)
	switch {
	case errors.Is(err, remount.ErrNotRemovable) && !explicit:
		return fn(dir)
	case err != nil:
		return fmt.Errorf("--readonly: %w", err)
	}
	if s.Changed() {
		fmt.Fprintln(a.stderr, ui.Dim.Render(fmt.Sprintf("Mounted %s read-only at %s for this run.", s.Device, nv.MountPoint)))
	}
	a.roSession = s
	defer func() {
		a.roSession = nil
		if err := s.Restore(ctx); err != nil {
			a.warn(err.Error())
		} else if s.Changed() {
			fmt.Fprintln(a.stderr, ui.Dim.Render(fmt.Sprintf("Mounted %s writable again.", s.Device)))
		}
	}()
	if nv.MountPoint != v.MountPoint {
		if err := a.refreshMounts(); err != nil {
			return err
		}
	}
	return fn(dropout.Remap(dir, v.MountPoint, nv.MountPoint))
}

// reapplyReadOnly makes a drive that came back from a drop-out read-only again when this
// run had made it so (the desktop automounts it writable).
func (a *app) reapplyReadOnly(ctx context.Context, key string) error {
	if a.roSession == nil || a.roSession.Key != key {
		return nil
	}
	if _, err := a.roSession.Reapply(ctx); err != nil {
		return fmt.Errorf("--readonly after the drive came back: %w", err)
	}
	return a.refreshMounts()
}
