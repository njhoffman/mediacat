package hash

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func write(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func pattern(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*31) ^ seed
	}
	return b
}

func TestPartialAndFull(t *testing.T) {
	big := pattern(1<<20, 1)
	a := write(t, "a", big)
	b := write(t, "b", big)

	// Same size, differs only outside the sampled regions: partial collides, full does not.
	mod := bytes.Clone(big)
	mod[SampleSize+100] ^= 0xff
	c := write(t, "c", mod)

	pa, _ := Partial(a, int64(len(big)))
	pb, _ := Partial(b, int64(len(big)))
	pc, _ := Partial(c, int64(len(mod)))
	if pa != pb || pa != pc || len(pa) != 32 {
		t.Errorf("partial: %s %s %s", pa, pb, pc)
	}
	var seen int64
	fa, _ := Full(context.Background(), a, func(n int64) { seen += n })
	fb, _ := Full(context.Background(), b, nil)
	fc, _ := Full(context.Background(), c, nil)
	if fa != fb || fa == fc || seen != int64(len(big)) {
		t.Errorf("full: %s %s %s seen=%d", fa, fb, fc, seen)
	}

	// A change in a sampled region changes the partial hash.
	mod2 := bytes.Clone(big)
	mod2[len(mod2)-1] ^= 0xff
	if pd, _ := Partial(write(t, "d", mod2), int64(len(mod2))); pd == pa {
		t.Error("tail change not detected by partial hash")
	}
}

func TestSmallFilesAreExact(t *testing.T) {
	x := write(t, "x", pattern(100_000, 1))
	y := write(t, "y", pattern(100_000, 2))
	px, _ := Partial(x, 100_000)
	py, _ := Partial(y, 100_000)
	if px == py {
		t.Error("small files with different content should differ")
	}
	// Size is part of the partial hash.
	z := write(t, "z", nil)
	pz0, _ := Partial(z, 0)
	pz1, _ := Partial(z, 1)
	if pz0 == pz1 {
		t.Error("size should be mixed into the partial hash")
	}
}

func TestWriterMatchesFull(t *testing.T) {
	data := pattern(5<<20, 3)
	p := write(t, "w", data)
	full, _ := Full(context.Background(), p, nil)
	w := Writer()
	w.Write(data[:1000])
	w.Write(data[1000:])
	if w.Sum() != full {
		t.Error("Writer sum differs from Full")
	}
}

func TestErrors(t *testing.T) {
	if _, err := Partial(filepath.Join(t.TempDir(), "missing"), 10); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Full(ctx, write(t, "c", []byte("x")), nil); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
	if err := classify(&os.PathError{Op: "read", Path: "x", Err: syscall.EIO}); !errors.Is(err, ErrTransient) {
		t.Errorf("EIO should be transient: %v", err)
	}
	if err := classify(os.ErrPermission); errors.Is(err, ErrTransient) {
		t.Error("permission denied is not transient")
	}
}
