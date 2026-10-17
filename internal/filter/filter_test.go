package filter

import (
	"reflect"
	"testing"
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
	good := []Filter{{}, {Kind: KindVideo, Sort: "size"}, {MinSize: 1, MaxSize: 1}}
	for _, f := range good {
		if err := f.Validate(); err != nil {
			t.Errorf("%+v: %v", f, err)
		}
	}
	bad := []Filter{
		{Kind: "pdf"}, {MinSize: 2, MaxSize: 1}, {MinDurationMS: 2, MaxDurationMS: 1},
		{MinRes: 1080, MaxRes: 720}, {Sort: "name"},
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
