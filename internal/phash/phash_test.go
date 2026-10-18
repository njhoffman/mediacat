package phash

import (
	"context"
	"errors"
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
