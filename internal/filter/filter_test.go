package filter

import (
	"reflect"
	"testing"
	"time"
)

func TestParseSize(t *testing.T) {
	ok := map[string]int64{
		"1024": 1024, "0": 0, "500M": 500 << 20, "1.5G": 3 << 29, "700kb": 700 << 10,
		"2 GiB": 2 << 30, "1t": 1 << 40, " 10B ": 10,
	}
	for in, want := range ok {
		got, err := ParseSize(in)
		if err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "M", "-1G", "10X", "1.2.3G", "abc"} {
		if _, err := ParseSize(in); err == nil {
			t.Errorf("ParseSize(%q) should fail", in)
		}
	}
}

func TestParseDuration(t *testing.T) {
	ok := map[string]int64{
		"90": 90_000, "1.5": 1_500, "5m": 300_000, "1h30m": 5_400_000, "45s": 45_000,
		"1:30": 90_000, "1:30:00": 5_400_000, "0:00:01.5": 1_500, "120:00": 7_200_000,
	}
	for in, want := range ok {
		got, err := ParseDuration(in)
		if err != nil || got != want {
			t.Errorf("ParseDuration(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "-5", "1:60", "1:2:3:4", "5x", "-1m", "a:b"} {
		if _, err := ParseDuration(in); err == nil {
			t.Errorf("ParseDuration(%q) should fail", in)
		}
	}
}

func TestParseRes(t *testing.T) {
	ok := map[string]int{
		"720p": 720, "1080": 1080, "4k": 2160, "4K": 2160, "8k": 4320, "fhd": 1080,
		"1920x1080": 1080, "1080x1920": 1080, "640X480": 480,
	}
	for in, want := range ok {
		got, err := ParseRes(in)
		if err != nil || got != want {
			t.Errorf("ParseRes(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "p", "0p", "-720p", "1920x", "x1080", "0x0", "big"} {
		if _, err := ParseRes(in); err == nil {
			t.Errorf("ParseRes(%q) should fail", in)
		}
	}
}

func TestValidate(t *testing.T) {
	good := []Filter{{}, {Kind: KindVideo}, {MinSize: 1, MaxSize: 1}, {MinFPS: 24}}
	for _, f := range good {
		if err := f.Validate(); err != nil {
			t.Errorf("%+v: %v", f, err)
		}
	}
	bad := []Filter{
		{Kind: "pdf"}, {MinSize: 2, MaxSize: 1}, {MinDurationMS: 2, MaxDurationMS: 1},
		{MinRes: 1080, MaxRes: 720}, {MinFPS: 60, MaxFPS: 30}, {MinBPP: 0.4, MaxBPP: 0.1}, {MinABitrate: 2, MaxABitrate: 1},
		{ModifiedAfter: time.Unix(2, 0), ModifiedBefore: time.Unix(1, 0)}, {Checks: []string{"broken"}},
	}
	for _, f := range bad {
		if err := f.Validate(); err == nil {
			t.Errorf("%+v should fail", f)
		}
	}
}

func TestSplitList(t *testing.T) {
	got := SplitList([]string{"MP4,.mkv", " h264 ", ",,"})
	if want := []string{"mp4", "mkv", "h264"}; !reflect.DeepEqual(got, want) {
		t.Errorf("SplitList = %v", got)
	}
}

func TestParseSort(t *testing.T) {
	got, err := ParseSort(" -Size, duration ,,name")
	want := []SortKey{{"size", true}, {"duration", false}, {"name", false}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("ParseSort = %v %v", got, err)
	}
	if _, err := ParseSort("size,loudness"); err == nil {
		t.Error("unknown key accepted")
	}
	for _, f := range Fields {
		if _, err := ParseSort("-" + f); err != nil {
			t.Errorf("field %s not sortable: %v", f, err)
		}
	}
}

func TestParseBitrate(t *testing.T) {
	ok := map[string]int64{"128k": 128_000, "4.5M": 4_500_000, "8000000": 8_000_000, "320kbps": 320_000, "4M/s": 4_000_000}
	for in, want := range ok {
		if got, err := ParseBitrate(in); err != nil || got != want {
			t.Errorf("ParseBitrate(%q) = %d %v", in, got, err)
		}
	}
	for _, in := range []string{"", "fast", "-1k", "5G"} {
		if _, err := ParseBitrate(in); err == nil {
			t.Errorf("ParseBitrate(%q) should fail", in)
		}
	}
}

func TestParseTime(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.Local)
	ok := map[string]time.Time{
		"2026-01-31":       time.Date(2026, 1, 31, 0, 0, 0, 0, time.Local),
		"2026-01-31T15:04": time.Date(2026, 1, 31, 15, 4, 0, 0, time.Local),
		"7d":               now.Add(-7 * 24 * time.Hour),
		"2w":               now.Add(-14 * 24 * time.Hour),
		"36h":              now.Add(-36 * time.Hour),
		"3mo":              now.Add(-90 * 24 * time.Hour),
		"1y":               now.Add(-365 * 24 * time.Hour),
	}
	for in, want := range ok {
		if got, err := ParseTime(in, now); err != nil || !got.Equal(want) {
			t.Errorf("ParseTime(%q) = %v %v", in, got, err)
		}
	}
	for _, in := range []string{"", "yesterday", "-3d", "2026-13-01"} {
		if _, err := ParseTime(in, now); err == nil {
			t.Errorf("ParseTime(%q) should fail", in)
		}
	}
}
