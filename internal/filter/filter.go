// Package filter parses the human-friendly size, duration and resolution flags used by
// list and holds the resulting query criteria.
package filter

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Kinds of media the catalog tracks.
const (
	KindVideo = "video"
	KindAudio = "audio"
	KindImage = "image"
)

// Filter is the parsed set of list criteria. Zero values mean "no constraint". Text
// matches are case-insensitive substrings.
type Filter struct {
	Kind           string
	MinSize        int64
	MaxSize        int64
	MinDurationMS  int64
	MaxDurationMS  int64
	MinRes         int // compared against the short side of the frame
	MaxRes         int
	MinBitrate     int64 // overall, bits/s
	MaxBitrate     int64
	MinVBitrate    int64
	MaxVBitrate    int64
	MinABitrate    int64
	MaxABitrate    int64
	MinFPS         float64
	MaxFPS         float64
	MinYear        int
	MaxYear        int
	Codecs         []string // matches video or audio codec
	VCodecs        []string
	ACodecs        []string
	Containers     []string // substring of ffprobe's format name, e.g. "matroska"
	Exts           []string // lowercase, no dot
	Name           string   // file name
	Path           string   // anywhere in the path
	Artist         string
	Album          string
	Title          string
	Genre          string
	ModifiedAfter  time.Time
	ModifiedBefore time.Time
	Missing        bool // only files whose last scan did not find them
	Errors         bool // only files that could not be probed
	Sorts          []SortKey
	Limit          int
}

// Field names shared by --sort and --columns.
const (
	FieldPath       = "path"
	FieldName       = "name"
	FieldDir        = "dir"
	FieldExt        = "ext"
	FieldType       = "type"
	FieldSize       = "size"
	FieldDuration   = "duration"
	FieldRes        = "res"
	FieldWidth      = "width"
	FieldHeight     = "height"
	FieldFPS        = "fps"
	FieldBitrate    = "bitrate"
	FieldVBitrate   = "vbitrate"
	FieldABitrate   = "abitrate"
	FieldCodec      = "codec"
	FieldVCodec     = "vcodec"
	FieldACodec     = "acodec"
	FieldContainer  = "container"
	FieldChannels   = "channels"
	FieldSampleRate = "samplerate"
	FieldMtime      = "mtime"
	FieldScanned    = "scanned"
	FieldArtist     = "artist"
	FieldAlbum      = "album"
	FieldTitle      = "title"
	FieldTrack      = "track"
	FieldYear       = "year"
	FieldGenre      = "genre"
	FieldError      = "error"
)

// Fields lists every field, in the order help text shows them.
var Fields = []string{
	FieldPath, FieldName, FieldDir, FieldExt, FieldType, FieldSize, FieldDuration, FieldRes, FieldWidth,
	FieldHeight, FieldFPS, FieldBitrate, FieldVBitrate, FieldABitrate, FieldCodec, FieldVCodec, FieldACodec,
	FieldContainer, FieldChannels, FieldSampleRate, FieldMtime, FieldScanned, FieldArtist, FieldAlbum,
	FieldTitle, FieldTrack, FieldYear, FieldGenre, FieldError,
}

// IsField reports whether name is a known field.
func IsField(name string) bool {
	for _, f := range Fields {
		if f == name {
			return true
		}
	}
	return false
}

// Validate checks cross-field consistency after parsing.
func (f Filter) Validate() error {
	if f.Kind != "" && f.Kind != KindVideo && f.Kind != KindAudio && f.Kind != KindImage {
		return fmt.Errorf("unknown type %q (want video, audio or image)", f.Kind)
	}
	ranges := []struct {
		name     string
		min, max float64
	}{
		{"size", float64(f.MinSize), float64(f.MaxSize)},
		{"duration", float64(f.MinDurationMS), float64(f.MaxDurationMS)},
		{"resolution", float64(f.MinRes), float64(f.MaxRes)},
		{"bitrate", float64(f.MinBitrate), float64(f.MaxBitrate)},
		{"video bitrate", float64(f.MinVBitrate), float64(f.MaxVBitrate)},
		{"audio bitrate", float64(f.MinABitrate), float64(f.MaxABitrate)},
		{"fps", f.MinFPS, f.MaxFPS},
		{"year", float64(f.MinYear), float64(f.MaxYear)},
	}
	for _, r := range ranges {
		if r.max > 0 && r.min > r.max {
			return fmt.Errorf("min %s is larger than max %s", r.name, r.name)
		}
	}
	if !f.ModifiedAfter.IsZero() && !f.ModifiedBefore.IsZero() && f.ModifiedAfter.After(f.ModifiedBefore) {
		return fmt.Errorf("--modified-after is later than --modified-before")
	}
	return nil
}

var sizeUnits = map[string]int64{
	"": 1, "b": 1,
	"k": 1 << 10, "kb": 1 << 10, "kib": 1 << 10,
	"m": 1 << 20, "mb": 1 << 20, "mib": 1 << 20,
	"g": 1 << 30, "gb": 1 << 30, "gib": 1 << 30,
	"t": 1 << 40, "tb": 1 << 40, "tib": 1 << 40,
}

// ParseSize parses sizes like "500M", "1.5G", "700kb" or "1024". Units are binary.
func ParseSize(s string) (int64, error) {
	t := strings.ToLower(strings.TrimSpace(s))
	i := strings.IndexFunc(t, func(r rune) bool { return (r < '0' || r > '9') && r != '.' })
	num, unit := t, ""
	if i >= 0 {
		num, unit = t[:i], strings.TrimSpace(t[i:])
	}
	mult, ok := sizeUnits[unit]
	if !ok || num == "" {
		return 0, fmt.Errorf("invalid size %q (examples: 500M, 1.5G)", s)
	}
	n, err := strconv.ParseFloat(num, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid size %q (examples: 500M, 1.5G)", s)
	}
	return int64(math.Round(n * float64(mult))), nil
}

// ParseDuration parses "90" (seconds), "1:30", "1:30:00", or Go-style "5m", "1h30m", "45s".
// It returns milliseconds.
func ParseDuration(s string) (int64, error) {
	t := strings.TrimSpace(s)
	bad := fmt.Errorf("invalid duration %q (examples: 90, 5m, 1h30m, 1:30:00)", s)
	if t == "" {
		return 0, bad
	}
	if strings.Contains(t, ":") {
		parts := strings.Split(t, ":")
		if len(parts) > 3 {
			return 0, bad
		}
		var total float64
		for i, p := range parts {
			n, err := strconv.ParseFloat(p, 64)
			if err != nil || n < 0 || (i > 0 && n >= 60) {
				return 0, bad
			}
			total = total*60 + n
		}
		return int64(math.Round(total * 1000)), nil
	}
	if n, err := strconv.ParseFloat(t, 64); err == nil {
		if n < 0 {
			return 0, bad
		}
		return int64(math.Round(n * 1000)), nil
	}
	d, err := time.ParseDuration(t)
	if err != nil || d < 0 {
		return 0, bad
	}
	return d.Milliseconds(), nil
}

var resAliases = map[string]int{
	"sd": 480, "hd": 720, "fhd": 1080, "qhd": 1440, "2k": 1440, "uhd": 2160, "4k": 2160, "8k": 4320,
}

// ParseRes parses "720p", "1080", "4k" or "1920x1080" into a short-side pixel count, so
// portrait and landscape video of the same quality compare equally.
func ParseRes(s string) (int, error) {
	t := strings.ToLower(strings.TrimSpace(s))
	bad := fmt.Errorf("invalid resolution %q (examples: 720p, 1080p, 4k, 1920x1080)", s)
	if n, ok := resAliases[t]; ok {
		return n, nil
	}
	if w, h, ok := strings.Cut(t, "x"); ok {
		wi, err1 := strconv.Atoi(w)
		hi, err2 := strconv.Atoi(h)
		if err1 != nil || err2 != nil || wi <= 0 || hi <= 0 {
			return 0, bad
		}
		return min(wi, hi), nil
	}
	n, err := strconv.Atoi(strings.TrimSuffix(t, "p"))
	if err != nil || n <= 0 {
		return 0, bad
	}
	return n, nil
}

// SplitList splits comma-separated flag values, lowercasing and dropping empties and
// leading dots.
func SplitList(vals []string) []string {
	var out []string
	for _, v := range vals {
		for _, p := range strings.Split(v, ",") {
			p = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(p)), ".")
			if p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

// SortKey is one --sort field; Desc is set by a "-" prefix.
type SortKey struct {
	Key  string
	Desc bool
}

// ParseSort parses comma-separated keys such as "-size,duration".
func ParseSort(spec string) ([]SortKey, error) {
	var out []SortKey
	for _, part := range strings.Split(spec, ",") {
		part = strings.ToLower(strings.TrimSpace(part))
		if part == "" {
			continue
		}
		k := SortKey{Key: strings.TrimPrefix(part, "-"), Desc: strings.HasPrefix(part, "-")}
		if !IsField(k.Key) {
			return nil, fmt.Errorf("unknown sort key %q (want %s)", k.Key, strings.Join(Fields, ", "))
		}
		out = append(out, k)
	}
	return out, nil
}

var bitrateUnits = map[string]float64{"": 1, "b": 1, "bps": 1, "k": 1e3, "kb": 1e3, "kbps": 1e3, "m": 1e6, "mb": 1e6, "mbps": 1e6}

// ParseBitrate parses decimal bit rates such as "128k", "4.5M" or "8000000". "/s" is
// accepted and ignored ("4M/s").
func ParseBitrate(s string) (int64, error) {
	t := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), "/s")
	i := strings.IndexFunc(t, func(r rune) bool { return (r < '0' || r > '9') && r != '.' })
	num, unit := t, ""
	if i >= 0 {
		num, unit = t[:i], strings.TrimSpace(t[i:])
	}
	mult, ok := bitrateUnits[unit]
	n, err := strconv.ParseFloat(num, 64)
	if !ok || err != nil || n < 0 {
		return 0, fmt.Errorf("invalid bitrate %q (examples: 128k, 4.5M)", s)
	}
	return int64(math.Round(n * mult)), nil
}

// ParseTime parses an absolute date ("2026-01-31", "2026-01-31T15:04") or an age before
// now ("7d", "2w", "36h", "3mo", "1y").
func ParseTime(s string, now time.Time) (time.Time, error) {
	t := strings.TrimSpace(s)
	for _, layout := range []string{"2006-01-02", "2006-01-02T15:04", "2006-01-02 15:04", time.RFC3339} {
		if v, err := time.ParseInLocation(layout, t, time.Local); err == nil {
			return v, nil
		}
	}
	units := []struct {
		suffix string
		d      time.Duration
	}{{"mo", 30 * 24 * time.Hour}, {"y", 365 * 24 * time.Hour}, {"w", 7 * 24 * time.Hour}, {"d", 24 * time.Hour}}
	for _, u := range units {
		if n, err := strconv.ParseFloat(strings.TrimSuffix(t, u.suffix), 64); strings.HasSuffix(t, u.suffix) && err == nil && n >= 0 {
			return now.Add(-time.Duration(n * float64(u.d))), nil
		}
	}
	if d, err := time.ParseDuration(t); err == nil && d >= 0 {
		return now.Add(-d), nil
	}
	return time.Time{}, fmt.Errorf("invalid time %q (examples: 2026-01-31, 7d, 2w, 36h)", s)
}
