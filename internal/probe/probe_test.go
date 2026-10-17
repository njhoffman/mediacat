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
	if v.DurationMS < 900 || v.DurationMS > 1200 || v.FPS < 24.9 || v.FPS > 25.1 || len(v.Raw) == 0 {
		t.Errorf("video duration/fps/raw: %d %f %d", v.DurationMS, v.FPS, len(v.Raw))
	}

	a, err := p.Probe(ctx, files.Audio, filter.KindAudio)
	if err != nil {
		t.Fatal(err)
	}
	want := Tags{Artist: "Test Artist", Album: "Test Album", Title: "Test Title", Genre: "Rock", Track: 3, Year: 2021}
	if a.AudioCodec != "mp3" || a.VideoCodec != "" || a.Tags != want || a.SampleRate != 44100 {
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
