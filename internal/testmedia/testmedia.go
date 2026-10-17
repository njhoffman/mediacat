// Package testmedia generates tiny media fixtures with ffmpeg for tests.
package testmedia

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Files are the paths of generated fixtures.
type Files struct {
	Video     string // 320x240, 1s, 25fps, h264 + aac
	Audio     string // 1s mp3 with ID3 tags
	Image     string // 64x48 png
	Truncated string // first 2KB of Video
}

// Generate writes fixtures into dir, skipping the test when ffmpeg is unavailable.
func Generate(t testing.TB, dir string) Files {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}
	f := Files{
		Video:     filepath.Join(dir, "clip.mp4"),
		Audio:     filepath.Join(dir, "song.mp3"),
		Image:     filepath.Join(dir, "pic.png"),
		Truncated: filepath.Join(dir, "broken.mp4"),
	}
	run(t, "-f", "lavfi", "-i", "testsrc=size=320x240:rate=25:duration=1",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=1",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest", f.Video)
	run(t, "-f", "lavfi", "-i", "sine=frequency=440:duration=1:sample_rate=44100",
		"-c:a", "libmp3lame", "-id3v2_version", "3",
		"-metadata", "artist=Test Artist", "-metadata", "album=Test Album",
		"-metadata", "title=Test Title", "-metadata", "genre=Rock",
		"-metadata", "track=3/10", "-metadata", "date=2021", f.Audio)
	run(t, "-f", "lavfi", "-i", "testsrc=size=64x48:duration=1", "-frames:v", "1", f.Image)
	b, err := os.ReadFile(f.Video)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.Truncated, b[:2048], 0o644); err != nil {
		t.Fatal(err)
	}
	return f
}

func run(t testing.TB, args ...string) {
	t.Helper()
	cmd := exec.Command("ffmpeg", append([]string{"-hide_banner", "-loglevel", "error", "-y"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg %v: %v\n%s", args, err, out)
	}
}
