// Package hash computes the content hashes used for exact duplicate detection.
package hash

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/zeebo/xxh3"
)

// SampleSize is the size of each of the three samples in a partial hash.
const SampleSize = 64 << 10

// ErrTransient marks I/O failures that say nothing about the file (network drops).
var ErrTransient = errors.New("transient I/O error")

func encode(h *xxh3.Hasher128) string {
	b := h.Sum128().Bytes()
	return hex.EncodeToString(b[:])
}

// Partial hashes the file size plus the first, middle and last SampleSize bytes. Files
// no larger than three samples are hashed whole, so their partial hash is exact.
func Partial(path string, size int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", classify(err)
	}
	defer f.Close()
	h := xxh3.New128()
	var sz [8]byte
	binary.LittleEndian.PutUint64(sz[:], uint64(size))
	h.Write(sz[:])
	if size <= 3*SampleSize {
		if _, err := io.Copy(h, f); err != nil {
			return "", classify(err)
		}
		return encode(h), nil
	}
	buf := make([]byte, SampleSize)
	for _, off := range []int64{0, size/2 - SampleSize/2, size - SampleSize} {
		if _, err := f.ReadAt(buf, off); err != nil {
			return "", classify(err)
		}
		h.Write(buf)
	}
	return encode(h), nil
}

// Full hashes the whole file, calling progress with the bytes read since the last call.
func Full(ctx context.Context, path string, progress func(n int64)) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", classify(err)
	}
	defer f.Close()
	h := xxh3.New128()
	if err := Copy(ctx, h, f, progress); err != nil {
		return "", err
	}
	return encode(h), nil
}

// Copy streams src to dst in large chunks, honoring ctx and reporting progress.
func Copy(ctx context.Context, dst io.Writer, src io.Reader, progress func(n int64)) error {
	buf := make([]byte, 4<<20) // matches the share's 4MB rsize
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := src.Read(buf)
		if n > 0 {
			if werr := readLimit.wait(ctx, n); werr != nil {
				return werr
			}
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return classify(werr)
			}
			if progress != nil {
				progress(int64(n))
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return classify(err)
		}
	}
}

// Writer returns a hasher whose Sum matches Full, for hashing data while copying it.
func Writer() *Hasher { return &Hasher{h: xxh3.New128()} }

// Hasher accumulates a full hash.
type Hasher struct{ h *xxh3.Hasher128 }

func (h *Hasher) Write(p []byte) (int, error) { return h.h.Write(p) }

// Sum returns the hex hash of everything written.
func (h *Hasher) Sum() string { return encode(h.h) }

func classify(err error) error {
	for _, e := range []error{syscall.EIO, syscall.EAGAIN, syscall.ETIMEDOUT, syscall.EHOSTDOWN, syscall.ENOTCONN, syscall.ESTALE, syscall.ECONNRESET,
		syscall.ENODEV, syscall.ENXIO} {
		if errors.Is(err, e) {
			return fmt.Errorf("%w: %v", ErrTransient, err)
		}
	}
	return err
}

// limiter paces reads to a byte rate shared by every reader in the process.
type limiter struct {
	mu   sync.Mutex
	rate float64   // bytes per second; 0 is unlimited
	next time.Time // when the bytes read so far are paid for
}

var readLimit = &limiter{}

// SetReadLimit caps the combined rate of full-file reads (hashing, copying, verifying) in
// bytes per second, to read gently from a fragile drive. 0 removes the cap.
func SetReadLimit(bytesPerSecond int64) {
	readLimit.mu.Lock()
	defer readLimit.mu.Unlock()
	readLimit.rate, readLimit.next = float64(bytesPerSecond), time.Time{}
}

func (l *limiter) wait(ctx context.Context, n int) error {
	l.mu.Lock()
	if l.rate <= 0 {
		l.mu.Unlock()
		return nil
	}
	now := time.Now()
	if l.next.Before(now) {
		l.next = now
	}
	l.next = l.next.Add(time.Duration(float64(n) / l.rate * float64(time.Second)))
	delay := l.next.Sub(now)
	l.mu.Unlock()
	if delay <= 0 {
		return nil
	}
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
