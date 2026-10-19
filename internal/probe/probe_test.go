package probe

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/njhoffman/mediacat/internal/filter"
	"github.com/njhoffman/mediacat/internal/testmedia"
)

func TestKindOf(t *testing.T) {
	cases := map[string]string{
		"a.MP4": filter.KindVideo, "b.mkv": filter.KindVideo, "c.mp3": filter.KindAudio,
		"d.FLAC": filter.KindAudio, "e.jpg": filter.KindImage, "f.heic": filter.KindImage,
		"g.txt": "", "Thumbs.db": "", "noext": "", "x.trashinfo": "",
	}
	for in, want := range cases {
		if got := KindOf(in); got != want {
			t.Errorf("KindOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestProbeMedia(t *testing.T) {
	dir := t.TempDir()
	files := testmedia.Generate(t, dir)
	p := Prober{}
	ctx := context.Background()

	v, err := p.Probe(ctx, files.Video, filter.KindVideo)
	if err != nil {
		t.Fatal(err)
	}
	if v.Width != 320 || v.Height != 240 || v.VideoCodec != "h264" || v.AudioCodec != "aac" {
		t.Errorf("video: %+v", v)
	}
	if v.VideoBitrate <= 0 || v.AudioBitrate <= 0 {
		t.Errorf("stream bitrates: video=%d audio=%d", v.VideoBitrate, v.AudioBitrate)
	}
	if v.DurationMS < 900 || v.DurationMS > 1200 || v.FPS < 24.9 || v.FPS > 25.1 || len(v.Raw) == 0 {
		t.Errorf("video duration/fps/raw: %d %f %d", v.DurationMS, v.FPS, len(v.Raw))
	}

	a, err := p.Probe(ctx, files.Audio, filter.KindAudio)
	if err != nil {
		t.Fatal(err)
	}
	want := Tags{Artist: "Test Artist", Album: "Test Album", Title: "Test Title", Genre: "Rock", Track: 3, Year: 2021}
	if a.AudioCodec != "mp3" || a.VideoCodec != "" || a.Tags != want || a.SampleRate != 44100 || a.AudioBitrate <= 0 || a.VideoBitrate != 0 {
		t.Errorf("audio: %+v", a)
	}

	i, err := p.Probe(ctx, files.Image, filter.KindImage)
	if err != nil {
		t.Fatal(err)
	}
	if i.Width != 64 || i.Height != 48 || i.Container != "png" || i.DurationMS != 0 {
		t.Errorf("image: %+v", i)
	}

	if _, err := p.Probe(ctx, files.Truncated, filter.KindVideo); err == nil || errors.Is(err, ErrTransient) {
		t.Errorf("truncated file: want permanent error, got %v", err)
	}
}

func TestProbeCancelled(t *testing.T) {
	dir := t.TempDir()
	files := testmedia.Generate(t, dir)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (Prober{}).Probe(ctx, files.Video, filter.KindVideo); !errors.Is(err, context.Canceled) {
		t.Errorf("want context.Canceled, got %v", err)
	}
}

func TestProbeTimeoutIsTransient(t *testing.T) {
	dir := t.TempDir()
	slow := filepath.Join(dir, "slow-ffprobe")
	if err := os.WriteFile(slow, []byte("#!/bin/sh\nsleep 5\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	_, err := Prober{FFprobe: slow, Timeout: 100 * time.Millisecond}.Probe(context.Background(), slow, filter.KindVideo)
	if !errors.Is(err, ErrTransient) {
		t.Errorf("want ErrTransient, got %v", err)
	}
}

func TestTransientStderr(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "eio-ffprobe")
	script := "#!/bin/sh\necho 'x.mp4: Input/output error' >&2\nexit 1\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := Prober{FFprobe: fake}.Probe(context.Background(), fake, filter.KindVideo)
	if !errors.Is(err, ErrTransient) {
		t.Errorf("want ErrTransient, got %v", err)
	}
}

func TestBPP(t *testing.T) {
	r := Result{VideoBitrate: 4_147_200, Width: 1920, Height: 1080, FPS: 25}
	if got := r.BPP(); got < 0.07999 || got > 0.08001 {
		t.Errorf("BPP = %f, want 0.08", got)
	}
	for _, missing := range []Result{{Width: 1920, Height: 1080, FPS: 25}, {VideoBitrate: 1, Height: 1, FPS: 1}, {VideoBitrate: 1, Width: 1, Height: 1}} {
		if missing.BPP() != 0 {
			t.Errorf("%+v: BPP should be 0 when an input is unknown", missing)
		}
	}
}

func TestHelpers(t *testing.T) {
	if r := parseRate("30000/1001"); r < 29.96 || r > 29.98 {
		t.Errorf("parseRate = %f", r)
	}
	if parseRate("0/0") != 0 || parseRate("25") != 25 {
		t.Error("parseRate edge cases")
	}
	if secondsToMS("1.2345") != 1235 || secondsToMS("N/A") != 0 {
		t.Error("secondsToMS")
	}
	tg := tagsFromMap(map[string]string{"ARTIST": "A", "track": "3/12", "date": "2019-05-01"})
	if tg.Artist != "A" || tg.Track != 3 || tg.Year != 2019 {
		t.Errorf("tagsFromMap = %+v", tg)
	}
}

func TestFromJSONBitrates(t *testing.T) {
	stream := func(typ, codec, br string, tags string) string {
		return `{"codec_type":"` + typ + `","codec_name":"` + codec + `","bit_rate":"` + br + `","tags":{` + tags + `}}`
	}
	doc := func(formatBR string, streams ...string) []byte {
		out := `{"format":{"format_name":"matroska,webm","duration":"60.0","bit_rate":"` + formatBR + `"},"streams":[`
		for i, s := range streams {
			if i > 0 {
				out += ","
			}
			out += s
		}
		return []byte(out + "]}")
	}
	cases := []struct {
		name         string
		raw          []byte
		kind         string
		video, audio int64
	}{
		{"stream rates", doc("5000000", stream("video", "h264", "4800000", ""), stream("audio", "aac", "192000", "")), filter.KindVideo, 4800000, 192000},
		{"mkv BPS tags", doc("5000000", stream("video", "hevc", "", `"BPS-eng":"4500000"`), stream("audio", "opus", "", `"BPS":"128000"`)), filter.KindVideo, 4500000, 128000},
		{"derived video", doc("5000000", stream("video", "h264", "", ""), stream("audio", "aac", "", `"BPS":"200000"`)), filter.KindVideo, 4800000, 200000},
		{"unknown audio blocks derivation", doc("5000000", stream("video", "h264", "", ""), stream("audio", "aac", "", "")), filter.KindVideo, 0, 0},
		{"audio only", doc("320000", stream("audio", "flac", "", "")), filter.KindAudio, 0, 320000},
		{"mpeg VBR marker is derived instead", doc("865676", stream("video", "mpeg1video", "104857200", ""), stream("audio", "mp2", "160000", "")), filter.KindVideo, 705676, 160000},
		{"mpeg VBR marker, unknown audio", doc("865676", stream("video", "mpeg2video", "104857200", ""), stream("audio", "mp2", "", "")), filter.KindVideo, 0, 0},
	}
	for _, c := range cases {
		r, err := FromJSON(c.raw, c.kind)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if r.VideoBitrate != c.video || r.AudioBitrate != c.audio {
			t.Errorf("%s: video=%d audio=%d, want %d %d", c.name, r.VideoBitrate, r.AudioBitrate, c.video, c.audio)
		}
	}
	if _, err := FromJSON([]byte("not json"), filter.KindVideo); err == nil {
		t.Error("bad JSON should error")
	}
}
