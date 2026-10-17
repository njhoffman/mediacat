package ui

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Bytes formats n with binary units, e.g. "1.4 GiB".
func Bytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 5; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// Count formats n with thousands separators.
func Count(n int) string {
	s := strconv.Itoa(n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// Duration formats milliseconds as h:mm:ss or m:ss; zero is "-".
func Duration(ms int64) string {
	if ms <= 0 {
		return "-"
	}
	s := (ms + 500) / 1000
	h, m, sec := s/3600, (s%3600)/60, s%60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, sec)
	}
	return fmt.Sprintf("%d:%02d", m, sec)
}

// Elapsed formats a wall-clock duration compactly, e.g. "3m12s".
func Elapsed(d time.Duration) string {
	if d < time.Second {
		return d.Round(time.Millisecond).String()
	}
	return d.Round(time.Second).String()
}

// Res formats a frame size; unknown is "-".
func Res(w, h int) string {
	if w <= 0 || h <= 0 {
		return "-"
	}
	return fmt.Sprintf("%dx%d", w, h)
}

// Bitrate formats bits per second.
func Bitrate(bps int64) string {
	switch {
	case bps <= 0:
		return "-"
	case bps >= 1_000_000:
		return fmt.Sprintf("%.1f Mb/s", float64(bps)/1e6)
	default:
		return fmt.Sprintf("%d kb/s", bps/1000)
	}
}

// Age formats how long ago t was relative to now.
func Age(t, now time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 60*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	case d < 2*365*24*time.Hour:
		return fmt.Sprintf("%dmo ago", int(d.Hours()/24/30))
	default:
		return fmt.Sprintf("%dy ago", int(d.Hours()/24/365))
	}
}

// Stale returns a warning when last is older than days, or when there is no scan.
// It returns "" when the data is fresh.
func Stale(last *time.Time, days int, now time.Time, what string) string {
	if last == nil {
		return fmt.Sprintf("No completed scan covers %s. Run: mediacat scan %s", what, what)
	}
	if days > 0 && now.Sub(*last) > time.Duration(days)*24*time.Hour {
		return fmt.Sprintf("Catalog for %s was last updated %s (%s); results may be out of date. Run: mediacat scan %s",
			what, Age(*last, now), last.Format("2006-01-02"), what)
	}
	return ""
}
