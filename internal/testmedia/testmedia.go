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

// Broken are fixtures for integrity checks.
type Broken struct {
	Good      string // 20s video with the index first (faststart)
	Truncated string // first 60% of Good: index intact, data missing
	Corrupt   string // Good with random bytes over its middle
	Image     string // valid png
	BadImage  string // jpeg cut in half
}

// GenerateBroken writes integrity fixtures into dir.
func GenerateBroken(t testing.TB, dir string) Broken {
	t.Helper()
	Generate(t, t.TempDir()) // skips when ffmpeg is missing
	b := Broken{
		Good: filepath.Join(dir, "good.mp4"), Truncated: filepath.Join(dir, "truncated.mp4"),
		Corrupt: filepath.Join(dir, "corrupt.mp4"), Image: filepath.Join(dir, "ok.png"), BadImage: filepath.Join(dir, "cut.jpg"),
	}
	run(t, "-f", "lavfi", "-i", "testsrc=size=320x240:rate=25:duration=20", "-f", "lavfi", "-i", "sine=duration=20",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", "-movflags", "+faststart", b.Good)
	run(t, "-f", "lavfi", "-i", "testsrc=size=64x48:duration=1", "-frames:v", "1", b.Image)
	jpg := filepath.Join(dir, "full.jpg")
	run(t, "-f", "lavfi", "-i", "testsrc=size=320x240:duration=1", "-frames:v", "1", jpg)
	data, _ := os.ReadFile(b.Good)
	os.WriteFile(b.Truncated, data[:len(data)*6/10], 0o644)
	bad := append([]byte(nil), data...)
	for i := len(bad) / 2; i < len(bad)/2+20000 && i < len(bad); i++ {
		bad[i] = byte(i*7919) ^ 0x5a
	}
	os.WriteFile(b.Corrupt, bad, 0o644)
	j, _ := os.ReadFile(jpg)
	os.WriteFile(b.BadImage, j[:len(j)/2], 0o644)
	os.Remove(jpg)
	return b
}

// Similar are fixtures for perceptual duplicates.
type Similar struct {
	Video      string // 10s 320x240 test pattern
	VideoHigh  string // the same pattern at 640x480 and a higher bitrate
	VideoOther string // different content of the same length
	Image      string // 640x480 png
	ImageSmall string // the same picture as a 320x240 jpeg
	ImageOther string // a different picture
	ImagePPM   string // the same picture in a format Go does not decode
	ImageTiny  string // 32x24 png
}

// GenerateSimilar writes perceptual-duplicate fixtures into dir.
func GenerateSimilar(t testing.TB, dir string) Similar {
	t.Helper()
	Generate(t, t.TempDir()) // skips when ffmpeg is missing
	s := Similar{
		Video: filepath.Join(dir, "movie.mp4"), VideoHigh: filepath.Join(dir, "hd", "movie.mkv"),
		VideoOther: filepath.Join(dir, "other.mp4"), Image: filepath.Join(dir, "photo.png"),
		ImageSmall: filepath.Join(dir, "small", "photo.jpg"), ImageOther: filepath.Join(dir, "fractal.png"),
		ImagePPM: filepath.Join(dir, "photo.ppm"), ImageTiny: filepath.Join(dir, "icon.png"),
	}
	os.MkdirAll(filepath.Dir(s.VideoHigh), 0o755)
	os.MkdirAll(filepath.Dir(s.ImageSmall), 0o755)
	run(t, "-f", "lavfi", "-i", "testsrc=size=320x240:rate=25:duration=10", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-b:v", "300k", s.Video)
	run(t, "-f", "lavfi", "-i", "testsrc=size=320x240:rate=25:duration=10", "-vf", "scale=640:480",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-b:v", "3M", s.VideoHigh)
	run(t, "-f", "lavfi", "-i", "mandelbrot=size=320x240:rate=25", "-t", "10", "-c:v", "libx264", "-pix_fmt", "yuv420p", s.VideoOther)
	run(t, "-f", "lavfi", "-i", "testsrc=size=640x480:duration=1", "-frames:v", "1", s.Image)
	run(t, "-i", s.Image, "-vf", "scale=320:240", "-q:v", "5", s.ImageSmall)
	run(t, "-i", s.Image, s.ImagePPM)
	run(t, "-f", "lavfi", "-i", "mandelbrot=size=640x480", "-frames:v", "1", s.ImageOther)
	run(t, "-f", "lavfi", "-i", "testsrc=size=32x24:duration=1", "-frames:v", "1", s.ImageTiny)
	return s
}

// GenerateStill writes a video holding a single frame, the way some tools save stills.
func GenerateStill(t testing.TB, path string) {
	t.Helper()
	Generate(t, t.TempDir()) // skips when ffmpeg is missing
	run(t, "-f", "lavfi", "-i", "testsrc=size=320x240:rate=30", "-frames:v", "1",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", path)
}

// GenerateLongAudio writes a video whose 4s picture is followed by 4 more seconds of audio,
// so the container is longer than the video stream. The extension picks the container.
func GenerateLongAudio(t testing.TB, path string) {
	t.Helper()
	Generate(t, t.TempDir()) // skips when ffmpeg is missing
	run(t, "-f", "lavfi", "-i", "testsrc=size=320x240:rate=25:duration=4",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=8",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", path)
}
