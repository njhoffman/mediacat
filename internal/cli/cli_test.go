package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/njhoffman/mediacat/internal/testmedia"
	"github.com/njhoffman/mediacat/internal/ui"
)

type harness struct {
	t   *testing.T
	db  string
	dir string
	now time.Time
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ui.DisableColor()
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	return &harness{t: t, db: filepath.Join(t.TempDir(), "catalog.db"), dir: dir, now: time.Now()}
}

func (h *harness) run(args ...string) (stdout, stderr string, code int) {
	h.t.Helper()
	var out, errb bytes.Buffer
	a := &app{stdout: &out, stderr: &errb, now: func() time.Time { return h.now }}
	code = a.run("test", append([]string{"--db", h.db}, args...))
	return out.String(), errb.String(), code
}

func (h *harness) ok(args ...string) (string, string) {
	h.t.Helper()
	out, errs, code := h.run(args...)
	if code != 0 {
		h.t.Fatalf("mediacat %v exited %d\nstdout:\n%s\nstderr:\n%s", args, code, out, errs)
	}
	return out, errs
}

func TestEmptyDashboard(t *testing.T) {
	h := newHarness(t)
	out, _ := h.ok()
	if !strings.Contains(out, "The catalog is empty") {
		t.Errorf("dashboard: %s", out)
	}
}

func TestScanListInfoDashboard(t *testing.T) {
	h := newHarness(t)
	files := testmedia.Generate(t, h.dir)

	out, _ := h.ok("scan", h.dir)
	for _, want := range []string{"Scan complete", "media files", "added", "failed", "broken.mp4"} {
		if !strings.Contains(out, want) {
			t.Errorf("scan output missing %q:\n%s", want, out)
		}
	}

	out, errs := h.ok("list", h.dir)
	if strings.Contains(errs, "warning") {
		t.Errorf("fresh scan should not warn: %s", errs)
	}
	for _, want := range []string{"clip.mp4", "song.mp3", "pic.png", "320x240", "h264/aac", "error"} {
		if !strings.Contains(out, want) {
			t.Errorf("list missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(errs, "4 files") {
		t.Errorf("list footer: %s", errs)
	}

	out, _ = h.ok("list", h.dir, "--type", "audio")
	if !strings.Contains(out, "Test Artist") || !strings.Contains(out, "ARTIST") || strings.Contains(out, "clip.mp4") {
		t.Errorf("audio list:\n%s", out)
	}

	out, _ = h.ok("list", h.dir, "--min-res", "240p", "--json")
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("json: %v\n%s", err, out)
	}
	if len(rows) != 1 || rows[0]["video_codec"] != "h264" || rows[0]["path"] != files.Video {
		t.Errorf("json rows: %v", rows)
	}

	out, _ = h.ok("list", h.dir, "--errors")
	if !strings.Contains(out, "broken.mp4") || strings.Contains(out, "clip.mp4") {
		t.Errorf("--errors:\n%s", out)
	}

	out, _ = h.ok("info", files.Audio)
	for _, want := range []string{"Test Title", "Test Album", "mp3", "44100 Hz", "cataloged"} {
		if !strings.Contains(out, want) {
			t.Errorf("info missing %q:\n%s", want, out)
		}
	}
	out, _ = h.ok("info", files.Video, "--json")
	var info map[string]any
	if err := json.Unmarshal([]byte(out), &info); err != nil || info["cataloged"] != true || info["ffprobe"] == nil {
		t.Errorf("info json: %v %v", err, info)
	}

	out, _ = h.ok()
	for _, want := range []string{"VOLUME", "Recent scans", "complete", "just now"} {
		if !strings.Contains(out, want) {
			t.Errorf("dashboard missing %q:\n%s", want, out)
		}
	}

	// Staleness: pretend 45 days passed.
	h.now = h.now.Add(45 * 24 * time.Hour)
	_, errs = h.ok("list", h.dir)
	if !strings.Contains(errs, "warning") || !strings.Contains(errs, "45d ago") {
		t.Errorf("stale list should warn: %s", errs)
	}
	_, errs = h.ok("--stale-days", "0", "list", h.dir)
	if strings.Contains(errs, "warning") {
		t.Errorf("--stale-days 0 should disable warnings: %s", errs)
	}
}

func TestListUncatalogedAndInfoLive(t *testing.T) {
	h := newHarness(t)
	files := testmedia.Generate(t, h.dir)
	_, errs := h.ok("list", h.dir)
	if !strings.Contains(errs, "No completed scan covers") {
		t.Errorf("uncataloged list: %s", errs)
	}
	out, errs := h.ok("info", files.Video)
	if !strings.Contains(errs, "probing live") || !strings.Contains(out, "320x240") || !strings.Contains(out, "cataloged") {
		t.Errorf("live info:\nstdout %s\nstderr %s", out, errs)
	}
}

func TestBadFlagsAndArgs(t *testing.T) {
	h := newHarness(t)
	notes := filepath.Join(h.dir, "notes.txt")
	os.WriteFile(notes, []byte("x"), 0o644)
	cases := [][]string{
		{"list", "--min-size", "huge"},
		{"list", "--min-res", "1080p", "--max-res", "720p"},
		{"list", "--sort", "name"},
		{"list", "--type", "pdf"},
		{"scan", filepath.Join(h.dir, "missing")},
		{"info", notes},
	}
	for _, args := range cases {
		if _, errs, code := h.run(args...); code != 1 || !strings.Contains(errs, "error:") {
			t.Errorf("%v: code=%d stderr=%s", args, code, errs)
		}
	}
}

func TestMissingAndPrune(t *testing.T) {
	h := newHarness(t)
	files := testmedia.Generate(t, h.dir)
	h.ok("scan", h.dir)
	os.Remove(files.Image)
	out, _ := h.ok("scan", h.dir)
	if !strings.Contains(out, "missing") || !strings.Contains(out, "--prune") {
		t.Errorf("scan after delete:\n%s", out)
	}
	out, _ = h.ok("list", h.dir, "--missing")
	if !strings.Contains(out, "pic.png") {
		t.Errorf("--missing:\n%s", out)
	}
	out, _ = h.ok()
	if !strings.Contains(out, "MISSING") {
		t.Errorf("dashboard:\n%s", out)
	}
	h.ok("scan", h.dir, "--prune")
	if _, errs := h.ok("list", h.dir, "--missing"); !strings.Contains(errs, "no matching files") {
		t.Errorf("after prune: %s", errs)
	}
}
