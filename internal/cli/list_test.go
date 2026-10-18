package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/njhoffman/mediacat/internal/config"
	"github.com/njhoffman/mediacat/internal/filter"
	"github.com/njhoffman/mediacat/internal/testmedia"
)

func header(out string) string { return strings.SplitN(out, "\n", 2)[0] }

func TestConfigCreatedOnFirstRun(t *testing.T) {
	h := newHarness(t)
	_, errs := h.ok()
	if !strings.Contains(errs, "Created default config at "+h.config) {
		t.Errorf("notice: %s", errs)
	}
	if b, err := os.ReadFile(h.config); err != nil || string(b) != config.Template {
		t.Fatalf("config not written: %v", err)
	}
	if _, errs := h.ok(); strings.Contains(errs, "Created default config") {
		t.Error("notice repeated on second run")
	}
	os.WriteFile(h.config, []byte("stale_dayz: 1\n"), 0o644)
	if _, errs, code := h.run(); code != 1 || !strings.Contains(errs, "stale_dayz") {
		t.Errorf("bad config should fail: %d %s", code, errs)
	}
}

func TestConfigSuppliesDefaults(t *testing.T) {
	h := newHarness(t)
	testmedia.Generate(t, h.dir)
	os.WriteFile(h.config, []byte("list:\n  sort: -size\n  columns: name,size\n"), 0o644)
	h.ok("scan", h.dir)
	out, _ := h.ok("list", h.dir)
	if header(out) != "NAME           SIZE" && !strings.HasPrefix(header(out), "NAME") {
		t.Errorf("config columns: %q", header(out))
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if !strings.HasPrefix(lines[1], "clip.mp4") {
		t.Errorf("config sort -size should put the video first:\n%s", out)
	}
	// Flags still win over the config.
	out, _ = h.ok("list", h.dir, "--sort", "name", "--columns", "name")
	if lines := strings.Split(strings.TrimSpace(out), "\n"); lines[1] != "broken.mp4" {
		t.Errorf("flag override:\n%s", out)
	}
}

func TestListColumnsAndSorting(t *testing.T) {
	h := newHarness(t)
	testmedia.Generate(t, h.dir)
	h.ok("scan", h.dir)

	out, _ := h.ok("list", h.dir, "--columns", "+vbitrate,+abitrate,-codec")
	if hd := header(out); !strings.Contains(hd, "V-BITRATE") || !strings.Contains(hd, "A-BITRATE") || strings.Contains(hd, "CODEC") ||
		!strings.HasSuffix(hd, "PATH") {
		t.Errorf("adjusted columns: %q", hd)
	}
	if !strings.Contains(out, "kb/s") {
		t.Errorf("bitrates missing:\n%s", out)
	}

	out, _ = h.ok("list", h.dir, "--sort", "-mtime,fps")
	if hd := header(out); !strings.Contains(hd, "MODIFIED") || !strings.Contains(hd, "FPS") || !strings.HasSuffix(hd, "PATH") {
		t.Errorf("sorted fields should be added before PATH: %q", hd)
	}
	out, _ = h.ok("list", h.dir, "--sort", "-mtime", "--columns", "name,size")
	if hd := header(out); strings.Contains(hd, "MODIFIED") {
		t.Errorf("explicit columns are kept exactly: %q", hd)
	}

	out, _ = h.ok("list", h.dir, "--sort", "-duration", "--columns", "name")
	names := strings.Fields(out)[1:]
	// The mp3 runs slightly longer than the video because of encoder padding.
	if strings.Join(names, " ") != "song.mp3 clip.mp4 broken.mp4 pic.png" {
		t.Errorf("unknown durations should sort last: %v", names)
	}
	out, _ = h.ok("list", h.dir, "--sort", "duration", "--columns", "name")
	if names := strings.Fields(out)[1:]; strings.Join(names, " ") != "clip.mp4 song.mp3 broken.mp4 pic.png" {
		t.Errorf("ascending, unknown still last: %v", names)
	}
	out, _ = h.ok("list", h.dir, "--desc", "--columns", "name")
	if names := strings.Fields(out)[1:]; names[0] != "song.mp3" {
		t.Errorf("--desc alone reverses path order: %v", names)
	}

	cases := map[string][]string{
		"name":      {"--name", "CLIP"},
		"vcodec":    {"--vcodec", "h264"},
		"min-fps":   {"--min-fps", "24"},
		"vbitrate":  {"--min-vbitrate", "1k"},
		"container": {"--container", "mp4"},
		"modified":  {"--modified-after", "1d"},
		"path":      {"--path-contains", "clip"},
	}
	for name, args := range cases {
		out, _ := h.ok(append([]string{"list", h.dir, "--columns", "name", "--type", "video"}, args...)...)
		if !strings.Contains(out, "clip.mp4") || strings.Contains(out, "broken.mp4") && name != "path" && name != "modified" && name != "container" && name != "name" {
			t.Errorf("%s filter:\n%s", name, out)
		}
	}
	out, _ = h.ok("list", h.dir, "--type", "audio", "--artist", "test art", "--columns", "name,artist,album,track,year,abitrate")
	if !strings.Contains(out, "song.mp3") || !strings.Contains(out, "Test Album") || !strings.Contains(out, "2021") {
		t.Errorf("audio tag columns:\n%s", out)
	}
	out, _ = h.ok("list", h.dir, "--type", "audio")
	if hd := header(out); !strings.Contains(hd, "ARTIST") || !strings.Contains(hd, "A-BITRATE") {
		t.Errorf("audio default columns: %q", hd)
	}
	out, _ = h.ok("info", filepath.Join(h.dir, "clip.mp4"))
	if !strings.Contains(out, "video bitrate") || !strings.Contains(out, "audio bitrate") {
		t.Errorf("info bitrates:\n%s", out)
	}
}

func TestListHighlightsLargeValues(t *testing.T) {
	h := newHarness(t)
	testmedia.Generate(t, h.dir)
	// Thresholds tiny enough that the test clip exceeds them, but the song's size does not.
	os.WriteFile(h.config, []byte(`list:
  highlight:
    size: {warn: 20k, max: 21k}
    duration: {warn: "0.5", max: "0.9"}
    resolution: {warn: 100x100, max: 200x200}
`), 0o644)
	h.ok("scan", h.dir)
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(termenv.Ascii) })

	out, _ := h.ok("list", h.dir, "--name", "clip", "--columns", "size,duration,res,name")
	line := strings.Split(strings.TrimSpace(out), "\n")[1]
	if strings.Count(line, "\x1b[") < 3 || !strings.Contains(line, "\x1b[1;") {
		t.Errorf("clip should have three bold highlighted cells: %q", line)
	}
	out, _ = h.ok("list", h.dir, "--name", "pic", "--columns", "res,name")
	line = strings.Split(strings.TrimSpace(out), "\n")[1]
	if strings.Contains(line, "\x1b[") {
		t.Errorf("64x48 image is under the resolution threshold: %q", line)
	}
}

func TestResolveColumns(t *testing.T) {
	sorts := func(s string) []filter.SortKey { k, _ := filter.ParseSort(s); return k }
	cases := []struct {
		defaults, spec, sort string
		want                 string
	}{
		{"size,duration,path", "", "", "size,duration,path"},
		{"size,duration,path", "", "-mtime", "size,duration,mtime,path"},
		{"size,duration,path", "name,size", "-mtime", "name,size"},
		{"size,duration,path", "+fps,-duration", "", "size,fps,path"},
		{"size,duration,path", "+size", "", "size,duration,path"},
		{"size,codec", "+name", "res", "size,codec,res,name"},
	}
	for _, c := range cases {
		got, err := resolveColumns(c.defaults, c.spec, sorts(c.sort))
		if err != nil || strings.Join(got, ",") != c.want {
			t.Errorf("resolveColumns(%q, %q, %q) = %v %v; want %s", c.defaults, c.spec, c.sort, got, err, c.want)
		}
	}
	if _, err := resolveColumns("size", "-size", nil); err == nil {
		t.Error("removing every column should fail")
	}
}

func writeConfig(t *testing.T, h *harness, body string) {
	t.Helper()
	if err := os.WriteFile(h.config, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
