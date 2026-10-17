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

// Filter is the parsed set of list criteria. Zero values mean "no constraint".
type Filter struct {
	Kind          string
	MinSize       int64
	MaxSize       int64
	MinDurationMS int64
	MaxDurationMS int64
	MinRes        int // compared against the short side of the frame
	MaxRes        int
	Codecs        []string // matches video or audio codec
	Exts          []string // lowercase, no dot
	Missing       bool     // only files whose last scan did not find them
	Sort          string
	Desc          bool
	Limit         int
}

// SortKeys are the accepted --sort values.
var SortKeys = []string{"path", "size", "duration", "res", "mtime", "bitrate"}

// Validate checks cross-field consistency after parsing.
func (f Filter) Validate() error {
	if f.Kind != "" && f.Kind != KindVideo && f.Kind != KindAudio && f.Kind != KindImage {
		return fmt.Errorf("unknown type %q (want video, audio or image)", f.Kind)
	}
	if f.MaxSize > 0 && f.MinSize > f.MaxSize {
		return fmt.Errorf("min size is larger than max size")
	}
	if f.MaxDurationMS > 0 && f.MinDurationMS > f.MaxDurationMS {
		return fmt.Errorf("min duration is longer than max duration")
	}
	if f.MaxRes > 0 && f.MinRes > f.MaxRes {
		return fmt.Errorf("min resolution is higher than max resolution")
	}
	if f.Sort != "" {
		ok := false
		for _, k := range SortKeys {
			ok = ok || k == f.Sort
		}
		if !ok {
			return fmt.Errorf("unknown sort key %q (want one of %s)", f.Sort, strings.Join(SortKeys, ", "))
		}
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
