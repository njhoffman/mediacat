// Package integrity checks whether media files decode: a quick check that decodes a
// second at the start and near the end (catching truncated files cheaply), and a full
// check that decodes everything.
package integrity

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"

	"github.com/njhoffman/mediacat/internal/catalog"
	"github.com/njhoffman/mediacat/internal/filter"
)

// Checker runs ffmpeg-based checks. The zero value uses ffmpeg from PATH.
type Checker struct {
	FFmpeg  string
	FFprobe string
	// Timeout bounds each quick-check ffmpeg run; full checks scale with duration.
	Timeout time.Duration
}

func (c Checker) bin() string {
	if c.FFmpeg != "" {
		return c.FFmpeg
	}
	return "ffmpeg"
}

func result(status, detail string) catalog.Check {
	return catalog.Check{Status: status, Detail: detail}
}

// Check examines one file at the given level. progress, if set, receives bytes of the
// file accounted for as a full decode advances.
func (c Checker) Check(ctx context.Context, f catalog.File, level string, progress func(int64)) catalog.Check {
	var r catalog.Check
	switch {
	case f.ProbeError != "":
		r = result(catalog.CheckUnreadable, "not valid media: "+f.ProbeError)
	case f.Kind == filter.KindImage:
		r = c.image(ctx, f)
		level = catalog.LevelFull // images are always decoded completely
	case level == catalog.LevelFull:
		r = c.full(ctx, f, progress)
	default:
		r = c.quick(ctx, f)
	}
	r.Level = level
	return r
}

// streamMap selects the stream to decode: the first video stream, else the first audio.
func streamMap(f catalog.File) string {
	if f.Meta.VideoCodec != "" && f.Kind == filter.KindVideo {
		return "0:v:0"
	}
	return "0:a:0"
}

var addr = regexp.MustCompile(` @ 0x[0-9a-f]+`)

// errorLines tidies ffmpeg's stderr: drops empty lines and pointer addresses.
func errorLines(b []byte) []string {
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(addr.ReplaceAllString(l, "")); l != "" {
			out = append(out, l)
		}
	}
	return out
}

var transientText = []string{"Input/output error", "Resource temporarily unavailable", "Connection timed out",
	"Host is down", "Stale file handle", "Transport endpoint is not connected", "Connection reset"}

func transient(lines []string) bool {
	for _, l := range lines {
		for _, t := range transientText {
			if strings.Contains(l, t) {
				return true
			}
		}
	}
	return false
}

// run executes a short ffmpeg/ffprobe command with the quick-check timeout.
func (c Checker) run(ctx context.Context, bin string, args ...string) ([]byte, []string, error) {
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 2 * time.Minute
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil, nil, ctx.Err()
	}
	if cctx.Err() != nil {
		return nil, nil, errTimeout
	}
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			return nil, nil, fmt.Errorf("running %s: %w", bin, err)
		}
	}
	return stdout.Bytes(), errorLines(stderr.Bytes()), nil
}

// frames decodes the first frame of stream and returns how many frames came out. A
// stream starts with a keyframe, so this needs no other frames and stays cheap. (Skipping
// non-key frames explicitly is avoided: HEVC decoders then warn about parameter sets.)
func (c Checker) frames(ctx context.Context, path, stream string) (int, []string, error) {
	out, lines, err := c.run(ctx, c.bin(), "-nostdin", "-hide_banner", "-v", "error",
		"-i", path, "-map", stream, "-frames", "1", "-f", "framecrc", "-")
	n := 0
	for _, l := range strings.Split(string(out), "\n") {
		if l != "" && !strings.HasPrefix(l, "#") {
			n++
		}
	}
	return n, lines, err
}

// lastPacket reads (without decoding) every packet from about ss to the end and returns
// the latest timestamp seen, or -1 when no packet was read. A truncated file stops early
// or makes the demuxer report a partial file.
func (c Checker) lastPacket(ctx context.Context, path string, ss float64) (float64, []string, error) {
	out, lines, err := c.run(ctx, c.probeBin(), "-v", "error", "-read_intervals", strconv.FormatFloat(ss, 'f', 3, 64)+"%+86400",
		"-show_entries", "packet=pts_time,dts_time", "-of", "csv=p=0", path)
	last := -1.0
	for _, l := range strings.Split(string(out), "\n") {
		for _, v := range strings.Split(l, ",") {
			if t, perr := strconv.ParseFloat(strings.TrimSpace(v), 64); perr == nil && t > last {
				last = t
			}
		}
	}
	return last, lines, err
}

func (c Checker) probeBin() string {
	if c.FFprobe != "" {
		return c.FFprobe
	}
	return "ffprobe"
}

var errTimeout = errors.New("timed out")

// truncationSlack is how far before the stated duration the data may end before the
// file counts as truncated (last-frame durations and stream offsets are not damage).
const truncationSlack = 2.0

func (c Checker) quick(ctx context.Context, f catalog.File) catalog.Check {
	start, startErrs, err := c.frames(ctx, f.AbsPath, streamMap(f))
	if r, done := runError(err); done {
		return r
	}
	if transient(startErrs) {
		return result(catalog.CheckUnverified, "read error: "+startErrs[0])
	}
	if start == 0 {
		return result(catalog.CheckUnreadable, firstOr(startErrs, "nothing decodes at the start"))
	}
	if dur := float64(f.Meta.DurationMS) / 1000; dur > 4 {
		last, endErrs, err := c.lastPacket(ctx, f.AbsPath, dur-3)
		if r, done := runError(err); done {
			return r
		}
		if transient(endErrs) {
			return result(catalog.CheckUnverified, "read error: "+endErrs[0])
		}
		partial := firstMatch(endErrs, "partial file")
		switch {
		case last < 0:
			return result(catalog.CheckDamaged, "no data near the end; the file is probably truncated")
		case partial != "" || last < dur-truncationSlack:
			return result(catalog.CheckDamaged, fmt.Sprintf("data ends at %s of %s; the file is probably truncated",
				clock(last), clock(dur)))
		}
	}
	if len(startErrs) > 0 {
		return result(catalog.CheckDamaged, "decode errors at the start: "+startErrs[0])
	}
	return result(catalog.CheckOK, "start decodes and data runs to the end")
}

func clock(sec float64) string {
	s := int(sec + 0.5)
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, s/60%60, s%60)
	}
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

func runError(err error) (catalog.Check, bool) {
	switch {
	case err == nil:
		return catalog.Check{}, false
	case errors.Is(err, context.Canceled):
		return result(catalog.CheckUnverified, "interrupted"), true
	case errors.Is(err, errTimeout):
		return result(catalog.CheckUnverified, "timed out"), true
	}
	return result(catalog.CheckUnverified, err.Error()), true
}

func firstOr(lines []string, def string) string {
	if len(lines) > 0 {
		return lines[0]
	}
	return def
}

func firstMatch(lines []string, sub string) string {
	for _, l := range lines {
		if strings.Contains(l, sub) {
			return l
		}
	}
	return ""
}

const maxKeptErrors = 200

// full decodes the whole file, reporting progress from ffmpeg's -progress output.
func (c Checker) full(ctx context.Context, f catalog.File, progress func(int64)) catalog.Check {
	dur := time.Duration(f.Meta.DurationMS) * time.Millisecond
	cctx, cancel := context.WithTimeout(ctx, 3*dur+5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, c.bin(), "-nostdin", "-hide_banner", "-v", "error", "-nostats",
		"-progress", "pipe:1", "-i", f.AbsPath, "-f", "null", "-")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return result(catalog.CheckUnverified, err.Error())
	}
	var stderr limitedBuffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return result(catalog.CheckUnverified, "running ffmpeg: "+err.Error())
	}
	var reported int64
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		v, ok := strings.CutPrefix(sc.Text(), "out_time_us=")
		if !ok || progress == nil || dur <= 0 {
			continue
		}
		us, err := strconv.ParseInt(v, 10, 64)
		if err != nil || us < 0 {
			continue
		}
		done := int64(float64(f.Size) * min(float64(us)/float64(dur.Microseconds()), 1))
		if done > reported {
			progress(done - reported)
			reported = done
		}
	}
	err = cmd.Wait()
	if progress != nil && reported < f.Size {
		progress(f.Size - reported)
	}
	if ctx.Err() != nil {
		return result(catalog.CheckUnverified, "interrupted")
	}
	if cctx.Err() != nil {
		return result(catalog.CheckUnverified, "timed out")
	}
	lines := errorLines(stderr.Bytes())
	if transient(lines) {
		return result(catalog.CheckUnverified, "read error: "+firstMatchAny(lines, transientText))
	}
	if err != nil && len(lines) == 0 {
		return result(catalog.CheckUnreadable, "ffmpeg failed: "+err.Error())
	}
	if len(lines) == 0 {
		return result(catalog.CheckOK, "decodes completely without errors")
	}
	count := fmt.Sprint(stderr.lines)
	if stderr.lines > maxKeptErrors {
		count = fmt.Sprintf("%d+", maxKeptErrors)
	}
	detail := fmt.Sprintf("%s decode errors; first: %s", count, lines[0])
	if l := firstMatch(lines, "partial file"); l != "" {
		detail = "truncated: " + l
	}
	return result(catalog.CheckDamaged, detail)
}

func firstMatchAny(lines, subs []string) string {
	for _, s := range subs {
		if l := firstMatch(lines, s); l != "" {
			return l
		}
	}
	return lines[0]
}

// limitedBuffer keeps the first maxKeptErrors lines of output but counts them all, so a
// badly damaged file cannot fill memory with error messages.
type limitedBuffer struct {
	buf   bytes.Buffer
	lines int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	for _, line := range bytes.SplitAfter(p, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		if bytes.HasSuffix(line, []byte("\n")) {
			b.lines++
		}
		if b.lines <= maxKeptErrors {
			b.buf.Write(line)
		}
	}
	return len(p), nil
}

func (b *limitedBuffer) Bytes() []byte { return b.buf.Bytes() }

// image decodes the whole image in Go, falling back to ffmpeg for formats Go cannot read.
func (c Checker) image(ctx context.Context, f catalog.File) catalog.Check {
	fh, err := os.Open(f.AbsPath)
	if err != nil {
		if transientErr(err) {
			return result(catalog.CheckUnverified, "read error: "+err.Error())
		}
		return result(catalog.CheckUnreadable, err.Error())
	}
	defer fh.Close()
	_, _, err = image.Decode(fh)
	switch {
	case err == nil:
		return result(catalog.CheckOK, "decodes completely")
	case errors.Is(err, image.ErrFormat):
		n, lines, ferr := c.frames(ctx, f.AbsPath, "0:v:0")
		if r, done := runError(ferr); done {
			return r
		}
		if n == 0 {
			return result(catalog.CheckUnreadable, firstOr(lines, "image does not decode"))
		}
		if len(lines) > 0 {
			return result(catalog.CheckDamaged, lines[0])
		}
		return result(catalog.CheckOK, "decodes completely")
	case transientErr(err):
		return result(catalog.CheckUnverified, "read error: "+err.Error())
	case errors.Is(err, io.ErrUnexpectedEOF), strings.Contains(err.Error(), "short Huffman data"):
		return result(catalog.CheckDamaged, "image data ends early (truncated): "+err.Error())
	}
	return result(catalog.CheckDamaged, err.Error())
}

func transientErr(err error) bool {
	for _, e := range []error{syscall.EIO, syscall.EAGAIN, syscall.ETIMEDOUT, syscall.EHOSTDOWN, syscall.ENOTCONN, syscall.ESTALE, syscall.ECONNRESET} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}
