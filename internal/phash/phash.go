// Package phash computes 64-bit perceptual hashes of videos and images. Video hashes
// reproduce Stash's algorithm exactly (25 frames between 5% and 95% of the duration, each
// scaled to 160px wide, tiled 5x5, then goimagehash's PerceptionHash), so they compare
// directly with the phashes Stash stores.
package phash

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"math"
	"math/bits"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	// Image decoders, matching the formats the integrity check reads in Go.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	"github.com/corona10/goimagehash"
	"github.com/disintegration/imaging"
	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"
)

// ErrTransient marks failures worth retrying later (network errors, timeouts).
var ErrTransient = errors.New("transient error")

// ErrTooSmall marks images below the minimum size, which are not hashed.
var ErrTooSmall = errors.New("image too small to compare")

const (
	frameWidth = 160
	columns    = 5
	rows       = 5
)

// Hasher runs ffmpeg to extract frames.
type Hasher struct {
	FFmpeg  string        // default "ffmpeg"
	FFprobe string        // default "ffprobe"; reads the video stream's duration for retries
	Timeout time.Duration // per ffmpeg run; default 2 minutes
	MinSize int           // images narrower or shorter than this are ErrTooSmall
}

func (h Hasher) bin() string {
	if h.FFmpeg != "" {
		return h.FFmpeg
	}
	return "ffmpeg"
}

// Distance is the number of differing bits between two hashes.
func Distance(a, b uint64) int { return bits.OnesCount64(a ^ b) }

// Video hashes a video of the given duration in seconds (the container's, as Stash uses).
// When the video stream is shorter (audio that runs past the picture) and the last sample
// frames would fall after the picture ends, it uses the stream's duration instead: no
// frame decodes there, so neither Stash nor mediacat could hash the file otherwise. Every
// other file gets exactly Stash's hash.
func (h Hasher) Video(ctx context.Context, path string, duration float64) (uint64, error) {
	if duration <= 0 {
		return 0, errors.New("unknown duration")
	}
	return h.video(ctx, path, h.hashDuration(ctx, path, duration))
}

// hashDuration picks the duration to sample frames from. A failed probe keeps the
// container duration; frame extraction then reports the real error.
func (h Hasher) hashDuration(ctx context.Context, path string, container float64) float64 {
	stream, err := h.streamDuration(ctx, path)
	if err != nil || stream <= 0 || round2(stream) >= round2(container) {
		return container
	}
	d := round2(container)
	if last := 0.05*d + float64(columns*rows-1)*(0.9*d/float64(columns*rows)); last < stream {
		return container
	}
	return stream
}

// round2 rounds seconds to hundredths, as Stash does before choosing frame times.
func round2(s float64) float64 { return math.Round(s*100) / 100 }

func (h Hasher) video(ctx context.Context, path string, duration float64) (uint64, error) {
	duration = round2(duration)
	n := columns * rows
	offset, step := 0.05*duration, 0.9*duration/float64(n)
	frames := make([]image.Image, 0, n)
	slow := false
	for i := range n {
		t := offset + float64(i)*step
		img, err := h.frame(ctx, path, t, slow)
		if err != nil && !slow && ctx.Err() == nil && !errors.Is(err, ErrTransient) {
			// Like Stash: when a fast seek fails, use accurate seeks from here on.
			slow = true
			img, err = h.frame(ctx, path, t, slow)
		}
		if err != nil {
			return 0, fmt.Errorf("frame %d of %d at %.2fs of %.2fs: %w", i+1, n, t, duration, err)
		}
		frames = append(frames, img)
	}
	return hashImage(montage(frames))
}

// streamDuration reads the first video stream's duration in seconds, from the stream or,
// for Matroska, its DURATION tag. It returns 0 when neither is set.
func (h Hasher) streamDuration(ctx context.Context, path string) (float64, error) {
	bin := h.FFprobe
	if bin == "" {
		bin = "ffprobe"
	}
	out, err := h.exec(ctx, bin, "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=duration:stream_tags=DURATION", "-of", "default=noprint_wrappers=1", path)
	if err != nil {
		return 0, err
	}
	var tag float64
	for _, line := range strings.Split(string(out), "\n") {
		k, v, _ := strings.Cut(strings.TrimSpace(line), "=")
		switch k {
		case "duration":
			if d, err := strconv.ParseFloat(v, 64); err == nil && d > 0 {
				return d, nil
			}
		case "TAG:DURATION":
			tag = parseClock(v)
		}
	}
	return tag, nil
}

// parseClock parses "HH:MM:SS.fraction" into seconds, or returns 0.
func parseClock(s string) float64 {
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return 0
	}
	var total float64
	for _, p := range parts {
		f, err := strconv.ParseFloat(p, 64)
		if err != nil {
			return 0
		}
		total = total*60 + f
	}
	return total
}

func montage(frames []image.Image) image.Image {
	w, ht := frames[0].Bounds().Dx(), frames[0].Bounds().Dy()
	m := imaging.New(w*columns, ht*rows, color.NRGBA{})
	for i, img := range frames {
		m = imaging.Paste(m, img, image.Pt(w*(i%columns), ht*(i/rows)))
	}
	return m
}

func hashImage(img image.Image) (uint64, error) {
	ih, err := goimagehash.PerceptionHash(img)
	if err != nil {
		return 0, err
	}
	return ih.GetHash(), nil
}

// frame extracts one frame at t seconds as a 160px-wide image, with the same ffmpeg
// arguments Stash uses.
func (h Hasher) frame(ctx context.Context, path string, t float64, slow bool) (image.Image, error) {
	args := []string{"-v", "error", "-y"}
	if !slow {
		args = append(args, "-ss", fmt.Sprint(t))
	}
	args = append(args, "-i", path)
	if slow {
		args = append(args, "-ss", fmt.Sprint(t))
	}
	args = append(args, "-frames:v", "1", "-vf", fmt.Sprintf("scale=%d:-2", frameWidth), "-c:v", "bmp", "-f", "rawvideo", "-")
	out, err := h.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, errors.New("ffmpeg returned no frame")
	}
	img, _, err := image.Decode(bytes.NewReader(out))
	if err != nil {
		return nil, fmt.Errorf("no frame decoded: %w", err)
	}
	return img, nil
}

var transientText = []string{"Input/output error", "Resource temporarily unavailable", "Connection timed out",
	"Host is down", "Stale file handle", "Transport endpoint is not connected", "Connection reset"}

func (h Hasher) run(ctx context.Context, args ...string) ([]byte, error) {
	return h.exec(ctx, h.bin(), args...)
}

func (h Hasher) exec(ctx context.Context, bin string, args ...string) ([]byte, error) {
	timeout := h.Timeout
	if timeout == 0 {
		timeout = 2 * time.Minute
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	switch {
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case cctx.Err() != nil:
		return nil, fmt.Errorf("%w: %s timed out", ErrTransient, filepath.Base(bin))
	}
	msg := strings.TrimSpace(stderr.String())
	for _, t := range transientText {
		if strings.Contains(msg, t) {
			return nil, fmt.Errorf("%w: %s", ErrTransient, firstLine(msg))
		}
	}
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			return nil, fmt.Errorf("running %s: %w", filepath.Base(bin), err)
		}
		if msg == "" {
			msg = err.Error()
		}
		return nil, errors.New(firstLine(msg))
	}
	return stdout.Bytes(), nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// Image hashes an image, decoding it in Go when possible and with ffmpeg otherwise.
func (h Hasher) Image(ctx context.Context, path string) (uint64, error) {
	img, err := decodeFile(path)
	if errors.Is(err, image.ErrFormat) {
		var out []byte
		out, err = h.run(ctx, "-v", "error", "-i", path, "-frames:v", "1", "-c:v", "bmp", "-f", "rawvideo", "-")
		if err == nil {
			img, _, err = image.Decode(bytes.NewReader(out))
			if err != nil {
				err = fmt.Errorf("image does not decode: %w", err)
			}
		}
	}
	if err != nil {
		return 0, err
	}
	b := img.Bounds()
	if h.MinSize > 0 && (b.Dx() < h.MinSize || b.Dy() < h.MinSize) {
		return 0, fmt.Errorf("%w (%dx%d)", ErrTooSmall, b.Dx(), b.Dy())
	}
	return hashImage(img)
}

func decodeFile(path string) (image.Image, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, classify(err)
	}
	defer fh.Close()
	img, _, err := image.Decode(fh)
	if err != nil && !errors.Is(err, image.ErrFormat) {
		return nil, classify(err)
	}
	return img, err
}

func classify(err error) error {
	for _, e := range []error{syscall.EIO, syscall.EAGAIN, syscall.ETIMEDOUT, syscall.EHOSTDOWN, syscall.ENOTCONN, syscall.ESTALE, syscall.ECONNRESET} {
		if errors.Is(err, e) {
			return fmt.Errorf("%w: %v", ErrTransient, err)
		}
	}
	return err
}
