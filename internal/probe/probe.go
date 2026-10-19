// Package probe extracts media metadata from a file using ffprobe, image headers and
// audio tag readers.
package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dhowden/tag"
	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"

	"github.com/njhoffman/mediacat/internal/filter"
)

var kinds = map[string]string{}

func init() {
	for _, e := range strings.Fields("mp4 m4v mkv webm avi wmv mov mpg mpeg m2ts mts ts flv 3gp ogv vob asf rm rmvb divx f4v") {
		kinds[e] = filter.KindVideo
	}
	for _, e := range strings.Fields("mp3 flac m4a aac ogg oga opus wav wma aiff aif alac ape wv mka dsf") {
		kinds[e] = filter.KindAudio
	}
	for _, e := range strings.Fields("jpg jpeg png gif webp bmp tif tiff heic heif avif jxl") {
		kinds[e] = filter.KindImage
	}
}

// KindOf classifies a path by extension, returning "" for non-media files.
func KindOf(path string) string {
	return kinds[strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")]
}

// Tags are the audio tags the catalog stores as columns.
type Tags struct {
	Artist, Album, Title, Genre string
	Track, Year                 int
}

// Result is the metadata extracted from one file.
type Result struct {
	Container  string
	DurationMS int64
	Width      int
	Height     int
	VideoCodec string
	AudioCodec string
	Bitrate    int64 // overall container rate, bits/s
	FPS        float64
	// Average stream rates in bits/s; 0 when neither ffprobe nor the stream tags report one.
	VideoBitrate int64
	AudioBitrate int64
	SampleRate   int
	Channels     int
	Tags         Tags
	Raw          []byte // ffprobe JSON, nil when the file was read without ffprobe
}

// BPP is the video bitrate per pixel per frame, a measure of how heavily a video is
// encoded that does not depend on its resolution or frame rate. It is 0 when any input is
// unknown.
func (r Result) BPP() float64 {
	if r.VideoBitrate <= 0 || r.Width <= 0 || r.Height <= 0 || r.FPS <= 0 {
		return 0
	}
	return float64(r.VideoBitrate) / (float64(r.Width) * float64(r.Height) * r.FPS)
}

// ErrTransient marks failures that say nothing about the file itself (network errors,
// timeouts). Callers should retry later rather than record the file as broken.
var ErrTransient = errors.New("transient error")

// Prober runs metadata extraction. The zero value uses ffprobe from PATH.
type Prober struct {
	FFprobe string
	Timeout time.Duration
}

// Probe extracts metadata for path of the given kind.
func (p Prober) Probe(ctx context.Context, path, kind string) (Result, error) {
	if kind == filter.KindImage {
		if r, err := imageHeader(path); err == nil {
			return r, nil
		} else if isTransient(err) {
			return Result{}, fmt.Errorf("%w: %v", ErrTransient, err)
		}
	}
	r, err := p.ffprobe(ctx, path, kind)
	if err != nil {
		return r, err
	}
	if kind == filter.KindAudio {
		mergeTags(&r.Tags, readTags(path))
	}
	return r, nil
}

func imageHeader(path string) (Result, error) {
	f, err := os.Open(path)
	if err != nil {
		return Result{}, err
	}
	defer f.Close()
	cfg, format, err := image.DecodeConfig(f)
	if err != nil {
		return Result{}, err
	}
	return Result{Container: format, Width: cfg.Width, Height: cfg.Height, VideoCodec: format}, nil
}

type ffStream struct {
	CodecType    string            `json:"codec_type"`
	CodecName    string            `json:"codec_name"`
	Width        int               `json:"width"`
	Height       int               `json:"height"`
	AvgFrameRate string            `json:"avg_frame_rate"`
	SampleRate   string            `json:"sample_rate"`
	Channels     int               `json:"channels"`
	BitRate      string            `json:"bit_rate"`
	Duration     string            `json:"duration"`
	Disposition  map[string]int    `json:"disposition"`
	Tags         map[string]string `json:"tags"`
}

type ffOutput struct {
	Streams []ffStream `json:"streams"`
	Format  struct {
		FormatName string            `json:"format_name"`
		Duration   string            `json:"duration"`
		BitRate    string            `json:"bit_rate"`
		Tags       map[string]string `json:"tags"`
	} `json:"format"`
}

func (p Prober) ffprobe(ctx context.Context, path, kind string) (Result, error) {
	bin := p.FFprobe
	if bin == "" {
		bin = "ffprobe"
	}
	timeout := p.Timeout
	if timeout == 0 {
		timeout = 2 * time.Minute
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, bin, "-v", "error", "-print_format", "json", "-show_format", "-show_streams", "--", path)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return Result{}, ctx.Err()
	}
	if cctx.Err() != nil {
		return Result{}, fmt.Errorf("%w: ffprobe timed out after %s", ErrTransient, timeout)
	}
	msg := strings.TrimSpace(stderr.String())
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			return Result{}, fmt.Errorf("running ffprobe: %w", err)
		}
		if transientMsg(msg) {
			return Result{}, fmt.Errorf("%w: %s", ErrTransient, firstLine(msg))
		}
		if msg == "" {
			msg = err.Error()
		}
		return Result{}, errors.New(firstLine(msg))
	}
	return FromJSON(stdout.Bytes(), kind)
}

// FromJSON builds a Result from ffprobe's -show_format -show_streams JSON output. It is
// also used to re-derive fields from JSON already stored in the catalog.
func FromJSON(raw []byte, kind string) (Result, error) {
	var out ffOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		return Result{}, fmt.Errorf("parsing ffprobe output: %w", err)
	}
	r := Result{Container: out.Format.FormatName, Raw: compact(raw)}
	r.DurationMS = secondsToMS(out.Format.Duration)
	r.Bitrate, _ = strconv.ParseInt(out.Format.BitRate, 10, 64)
	var audioTotal int64
	audioStreams, audioKnown := 0, 0
	for _, s := range out.Streams {
		switch s.CodecType {
		case "video":
			// Cover art in audio files shows up as a single-frame video stream.
			if s.Disposition["attached_pic"] == 1 || r.VideoCodec != "" {
				continue
			}
			r.VideoCodec, r.Width, r.Height = s.CodecName, s.Width, s.Height
			r.FPS = parseRate(s.AvgFrameRate)
			r.VideoBitrate = streamBitrate(s)
			if r.DurationMS == 0 {
				r.DurationMS = secondsToMS(s.Duration)
			}
		case "audio":
			audioStreams++
			br := streamBitrate(s)
			if br > 0 {
				audioKnown++
				audioTotal += br
			}
			if r.AudioCodec != "" {
				continue
			}
			r.AudioCodec, r.Channels = s.CodecName, s.Channels
			r.SampleRate, _ = strconv.Atoi(s.SampleRate)
			r.AudioBitrate = br
			if r.DurationMS == 0 {
				r.DurationMS = secondsToMS(s.Duration)
			}
		}
	}
	// Containers like MKV often omit per-stream rates. Derive what we can from the overall
	// rate: video is what remains after every audio stream, audio-only files are all audio.
	if r.VideoCodec != "" && r.VideoBitrate == 0 && r.Bitrate > 0 && audioKnown == audioStreams && r.Bitrate > audioTotal {
		r.VideoBitrate = r.Bitrate - audioTotal
	}
	if r.VideoCodec == "" && r.AudioBitrate == 0 && audioStreams == 1 {
		r.AudioBitrate = r.Bitrate
	}
	if kind == filter.KindImage {
		r.DurationMS, r.FPS, r.VideoBitrate = 0, 0, 0
	}
	if kind == filter.KindAudio {
		r.Tags = tagsFromMap(out.Format.Tags)
		if len(out.Streams) > 0 && r.Tags == (Tags{}) {
			r.Tags = tagsFromMap(out.Streams[0].Tags)
		}
	}
	if r.VideoCodec == "" && r.AudioCodec == "" {
		return r, errors.New("no audio or video streams")
	}
	return r, nil
}

// MPEGVariableBitrate is the bit_rate ffprobe reports for MPEG-1/2 video streams whose
// header declares a variable rate (0x3FFFF x 400 bit/s): a marker, not a measurement.
const MPEGVariableBitrate = 104857200

// streamBitrate reads a stream's average bit rate, falling back to the BPS statistics tag
// mkvmerge writes ("BPS" or "BPS-eng").
func streamBitrate(s ffStream) int64 {
	if n, err := strconv.ParseInt(s.BitRate, 10, 64); err == nil && n > 0 && n != MPEGVariableBitrate {
		return n
	}
	for k, v := range s.Tags {
		if strings.EqualFold(k, "BPS") || strings.HasPrefix(strings.ToUpper(k), "BPS-") {
			if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil && n > 0 {
				return n
			}
		}
	}
	return 0
}

// readTags reads embedded tags with dhowden/tag, which handles ID3v1/v2, MP4, FLAC and
// Vorbis comments more completely than ffprobe's format tags.
func readTags(path string) Tags {
	f, err := os.Open(path)
	if err != nil {
		return Tags{}
	}
	defer f.Close()
	m, err := tag.ReadFrom(f)
	if err != nil {
		return Tags{}
	}
	track, _ := m.Track()
	return Tags{
		Artist: strings.TrimSpace(m.Artist()), Album: strings.TrimSpace(m.Album()),
		Title: strings.TrimSpace(m.Title()), Genre: strings.TrimSpace(m.Genre()),
		Track: track, Year: m.Year(),
	}
}

// mergeTags prefers the tag reader's values and keeps ffprobe's where the reader had none.
func mergeTags(dst *Tags, src Tags) {
	if src.Artist != "" {
		dst.Artist = src.Artist
	}
	if src.Album != "" {
		dst.Album = src.Album
	}
	if src.Title != "" {
		dst.Title = src.Title
	}
	if src.Genre != "" {
		dst.Genre = src.Genre
	}
	if src.Track != 0 {
		dst.Track = src.Track
	}
	if src.Year != 0 {
		dst.Year = src.Year
	}
}

func tagsFromMap(m map[string]string) Tags {
	get := func(keys ...string) string {
		for k, v := range m {
			for _, want := range keys {
				if strings.EqualFold(k, want) {
					return strings.TrimSpace(v)
				}
			}
		}
		return ""
	}
	t := Tags{Artist: get("artist", "album_artist"), Album: get("album"), Title: get("title"), Genre: get("genre")}
	t.Track = leadingInt(get("track", "tracknumber"))
	t.Year = leadingInt(get("date", "year"))
	return t
}

func leadingInt(s string) int {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	n, _ := strconv.Atoi(s[:i])
	return n
}

func secondsToMS(s string) int64 {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 {
		return 0
	}
	return int64(f*1000 + 0.5)
}

func parseRate(s string) float64 {
	n, d, ok := strings.Cut(s, "/")
	if !ok {
		f, _ := strconv.ParseFloat(s, 64)
		return f
	}
	nf, _ := strconv.ParseFloat(n, 64)
	df, _ := strconv.ParseFloat(d, 64)
	if df == 0 {
		return 0
	}
	return nf / df
}

func compact(b []byte) []byte {
	var buf bytes.Buffer
	if json.Compact(&buf, b) != nil {
		return b
	}
	return buf.Bytes()
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

func isTransient(err error) bool {
	return errors.Is(err, syscall.EIO) || errors.Is(err, syscall.EAGAIN) ||
		errors.Is(err, syscall.ETIMEDOUT) || errors.Is(err, syscall.EHOSTDOWN) ||
		errors.Is(err, syscall.ENOTCONN) || errors.Is(err, syscall.ESTALE)
}

// transientMsg recognizes OS-level read failures in ffprobe's stderr, which on a network
// share usually mean the connection dropped rather than that the file is damaged.
func transientMsg(msg string) bool {
	for _, s := range []string{"Input/output error", "Resource temporarily unavailable", "Connection timed out", "Host is down", "Stale file handle", "Transport endpoint is not connected"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}
