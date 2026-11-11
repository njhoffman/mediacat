package cli

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/njhoffman/mediacat/internal/testmedia"
	"github.com/njhoffman/mediacat/internal/volume"
)

// realLookup finds a volume in the live mount table.
func realLookup(key string) (volume.Volume, bool) {
	r, err := volume.NewResolver()
	if err != nil {
		return volume.Volume{}, false
	}
	return r.Lookup(key)
}

func TestScanDroppedDrive(t *testing.T) {
	h := newHarness(t)
	testmedia.Generate(t, h.dir)
	h.ok("scan", h.dir)
	// The drive is gone for good: the scan stops without marking anything missing.
	os.Remove(filepath.Join(h.dir, "song.mp3"))
	h.lookup = func(string) (volume.Volume, bool) { return volume.Volume{}, false }
	_, errs, code := h.run("scan", h.dir, "--wait", "0")
	if code != 1 || !strings.Contains(errs, "did not come back") || !strings.Contains(errs, "nothing was marked missing") {
		t.Fatalf("dropped scan: %d %s", code, errs)
	}
	h.lookup = nil
	if out, _ := h.ok("list", h.dir, "--missing"); strings.Contains(out, "song.mp3") {
		t.Errorf("a dropped scan must not mark files missing:\n%s", out)
	}

	// Gone for a moment, then back: the scan waits and completes.
	var looks atomic.Int32
	h.lookup = func(key string) (volume.Volume, bool) {
		if looks.Add(1) <= 2 {
			return volume.Volume{}, false
		}
		return realLookup(key)
	}
	out, errs := h.ok("scan", h.dir, "--wait", "5s")
	if !strings.Contains(errs, "dropped out") || !strings.Contains(errs, "is back") || !strings.Contains(out, "Scan complete") {
		t.Errorf("recovered scan:\n%s\n%s", out, errs)
	}
}

func TestCopyDroppedDrive(t *testing.T) {
	h := newHarness(t)
	files := testmedia.Generate(t, h.dir)
	target := otherDrive(t, h.dir)
	h.ok("scan", h.dir)
	h.ok("scan", target)

	// The source becomes unreadable mid-run (as a drive dropping out would) and "comes
	// back" a few looks later; the file it was on is copied after the wait.
	os.Chmod(files.Audio, 0)
	defer os.Chmod(files.Audio, 0o644)
	var looks atomic.Int32
	h.lookup = func(key string) (volume.Volume, bool) {
		if looks.Add(1) <= 3 {
			return volume.Volume{}, false
		}
		os.Chmod(files.Audio, 0o644)
		return realLookup(key)
	}
	dest := filepath.Join(target, "in")
	out, errs := h.ok("copy", h.dir, "--to", dest, "--type", "audio", "--execute", "--yes", "--min-free", "1", "--wait", "5s")
	if !strings.Contains(errs, "dropped out") || !strings.Contains(out, "Done: 1 files") || !exists(filepath.Join(dest, "song.mp3")) {
		t.Fatalf("copy across a drop-out:\n%s\n%s", out, errs)
	}
}
