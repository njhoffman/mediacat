package phash

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/njhoffman/mediacat/internal/testmedia"
)

func TestVideo(t *testing.T) {
	s := testmedia.GenerateSimilar(t, t.TempDir())
	h := Hasher{}
	ctx := context.Background()
	a, err := h.Video(ctx, s.Video, 10)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := h.Video(ctx, s.Video, 10)
	if again != a {
		t.Errorf("not deterministic: %x %x", a, again)
	}
	high, err := h.Video(ctx, s.VideoHigh, 10)
	if err != nil {
		t.Fatal(err)
	}
	other, err := h.Video(ctx, s.VideoOther, 10)
	if err != nil {
		t.Fatal(err)
	}
	if d := Distance(a, high); d > 4 {
		t.Errorf("re-encode at another size: distance %d, want <= 4", d)
	}
	if d := Distance(a, other); d <= 10 {
		t.Errorf("different content: distance %d, want > 10", d)
	}
	if _, err := h.Video(ctx, s.Video, 0); err == nil {
		t.Error("zero duration should fail")
	}
	if _, err := h.Video(ctx, filepath.Join(t.TempDir(), "none.mp4"), 10); err == nil || errors.Is(err, ErrTransient) {
		t.Errorf("missing file should fail permanently: %v", err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := h.Video(cctx, s.Video, 10); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
}

func TestVideoAudioLongerThanPicture(t *testing.T) {
	for _, ext := range []string{".mp4", ".mkv"} { // stream duration, then the DURATION tag
		t.Run(ext, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "long-audio"+ext)
			testmedia.GenerateLongAudio(t, path)
			h := Hasher{}
			ctx := context.Background()
			if _, err := h.video(ctx, path, 8); err == nil {
				t.Fatal("frames past the picture should fail at the container duration")
			}
			stream, err := h.streamDuration(ctx, path)
			if err != nil || stream < 3.9 || stream > 4.1 {
				t.Fatalf("stream duration %v %v, want about 4s", stream, err)
			}
			got, err := h.Video(ctx, path, 8)
			if err != nil {
				t.Fatalf("hash with the stream duration: %v", err)
			}
			if want, _ := h.video(ctx, path, stream); got != want {
				t.Errorf("hash %x, want the stream-duration hash %x", got, want)
			}
		})
	}
}

func TestHashDurationKeepsStashsWhenFramesFit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "long-audio.mp4")
	testmedia.GenerateLongAudio(t, path) // 4s picture, 8s container
	h := Hasher{}
	ctx := context.Background()
	// 4.3s: the last sample (91.4%, 3.93s) is still inside the picture, so Stash's
	// container duration is kept.
	if d := h.hashDuration(ctx, path, 4.3); d != 4.3 {
		t.Errorf("duration %v, want the container's 4.3", d)
	}
	if d := h.hashDuration(ctx, path, 8); d < 3.9 || d > 4.1 {
		t.Errorf("duration %v, want the stream's 4s", d)
	}
	if d := h.hashDuration(ctx, filepath.Join(t.TempDir(), "none.mp4"), 8); d != 8 {
		t.Errorf("unreadable file: duration %v, want the container's", d)
	}
}

func TestParseClock(t *testing.T) {
	for in, want := range map[string]float64{"00:06:41.852969000": 401.852969, "01:00:00": 3600, "41.5": 0, "a:b:c": 0} {
		if got := parseClock(in); math.Abs(got-want) > 1e-6 {
			t.Errorf("parseClock(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestVideoSingleFrame(t *testing.T) {
	still := filepath.Join(t.TempDir(), "still.mp4")
	testmedia.GenerateStill(t, still)
	// Most sample times fall after the only frame, so the video cannot be hashed.
	_, err := Hasher{}.Video(context.Background(), still, 0.033)
	if err == nil || errors.Is(err, ErrTransient) {
		t.Fatalf("single-frame video should fail permanently: %v", err)
	}
	for _, want := range []string{"of 25 at", "of 0.03s", "ffmpeg returned no frame"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

func TestImage(t *testing.T) {
	s := testmedia.GenerateSimilar(t, t.TempDir())
	h := Hasher{MinSize: 64}
	ctx := context.Background()
	hash := func(p string) uint64 {
		t.Helper()
		v, err := h.Image(ctx, p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		return v
	}
	a := hash(s.Image)
	if d := Distance(a, hash(s.ImageSmall)); d > 4 {
		t.Errorf("scaled jpeg: distance %d", d)
	}
	if d := Distance(a, hash(s.ImagePPM)); d != 0 {
		t.Errorf("ppm via ffmpeg: distance %d", d)
	}
	if d := Distance(a, hash(s.ImageOther)); d <= 10 {
		t.Errorf("different picture: distance %d", d)
	}
	if _, err := h.Image(ctx, s.ImageTiny); !errors.Is(err, ErrTooSmall) {
		t.Errorf("tiny: %v", err)
	}
	bad := filepath.Join(t.TempDir(), "bad.jpg")
	os.WriteFile(bad, []byte("not an image"), 0o644)
	if _, err := h.Image(ctx, bad); err == nil || errors.Is(err, ErrTransient) {
		t.Errorf("garbage should fail permanently: %v", err)
	}
}

func TestDistance(t *testing.T) {
	if Distance(0, 0) != 0 || Distance(0, 0xff) != 8 || Distance(^uint64(0), 0) != 64 {
		t.Error("Distance")
	}
}
