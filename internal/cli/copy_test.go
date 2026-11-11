package cli

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/njhoffman/mediacat/internal/testmedia"
)

func TestCopyDest(t *testing.T) {
	dir := t.TempDir()
	mapFile := filepath.Join(dir, "map.tsv")
	os.WriteFile(mapFile, []byte("# layout\nPerformers/A\tStars/A\nPerformers\t/abs/People\n"), 0o644)
	rules, err := parseCopyMap(mapFile, "/src", "/dst")
	if err != nil {
		t.Fatal(err)
	}
	for src, want := range map[string]string{
		"/src/Performers/A/x.mp4": "/dst/Stars/A/x.mp4", // the longest rule wins
		"/src/Performers/B/y.mp4": "/abs/People/B/y.mp4",
		"/src/Other/z.mp4":        "/dst/Other/z.mp4", // no rule: path kept
	} {
		if got := copyDest(src, "/src", "/dst", rules); got != want {
			t.Errorf("copyDest(%s) = %s, want %s", src, got, want)
		}
	}
	os.WriteFile(mapFile, []byte("/elsewhere\tx\n"), 0o644)
	if _, err := parseCopyMap(mapFile, "/src", "/dst"); err == nil || !strings.Contains(err.Error(), "not under") {
		t.Errorf("rule outside the source: %v", err)
	}
	os.WriteFile(mapFile, []byte("only-one-column\n"), 0o644)
	if _, err := parseCopyMap(mapFile, "/src", "/dst"); err == nil {
		t.Error("a malformed rule should fail")
	}
}

// otherDrive returns a directory on a different filesystem than dir, or skips the test.
func otherDrive(t *testing.T, dir string) string {
	t.Helper()
	other, err := os.MkdirTemp(".", "copytest-")
	if err != nil {
		t.Skip(err)
	}
	other, _ = filepath.Abs(other)
	other, _ = filepath.EvalSymlinks(other)
	t.Cleanup(func() { os.RemoveAll(other) })
	var a, b syscall.Stat_t
	if syscall.Stat(dir, &a) != nil || syscall.Stat(other, &b) != nil || a.Dev == b.Dev {
		t.Skip("needs two filesystems (the temp dir and the package dir)")
	}
	return other
}

func TestCopyFlow(t *testing.T) {
	h := newHarness(t)
	files := testmedia.Generate(t, h.dir) // the source drive: clip.mp4, song.mp3, photo.jpg, ...
	target := otherDrive(t, h.dir)
	lib := filepath.Join(target, "lib")
	os.MkdirAll(lib, 0o755)
	// The target already holds the video under another name; its other files differ.
	b, _ := os.ReadFile(files.Video)
	os.WriteFile(filepath.Join(lib, "renamed.mp4"), b, 0o644)
	h.ok("scan", h.dir)

	dest := filepath.Join(lib, "incoming")
	if _, errs, code := h.run("copy", h.dir, "--to", dest); code != 1 || !strings.Contains(errs, "target drive has not been scanned") {
		t.Fatalf("unscanned target: %d %s", code, errs)
	}
	h.ok("scan", lib)
	if _, errs, code := h.run("copy", h.dir, "--to", filepath.Join(h.dir, "sub")); code != 1 || !strings.Contains(errs, "inside the source") {
		t.Errorf("dest inside source: %d %s", code, errs)
	}
	if _, errs, code := h.run("copy", h.dir, "--to", t.TempDir()); code != 1 || !strings.Contains(errs, "same drive") {
		t.Errorf("same drive: %d %s", code, errs)
	}

	// A clash: something else already sits where the image would go.
	img, _ := filepath.Rel(h.dir, files.Image)
	os.MkdirAll(filepath.Dir(filepath.Join(dest, img)), 0o755)
	os.WriteFile(filepath.Join(dest, img), []byte("not the image"), 0o644)

	out, _ := h.ok("copy", h.dir, "--to", dest, "--dry-run-files")
	if !strings.Contains(out, "already on the target") || !strings.Contains(out, "name clashes (skipped)") || !strings.Contains(out, "DRY RUN") {
		t.Fatalf("dry run:\n%s", out)
	}
	if strings.Contains(out, "-> "+filepath.Join(dest, "clip.mp4")) {
		t.Errorf("the video has a copy on the target and must not be copied:\n%s", out)
	}
	audio := filepath.Join(dest, filepath.Base(files.Audio))
	if !strings.Contains(out, "-> "+audio) || exists(audio) {
		t.Errorf("the audio should be planned, not copied:\n%s", out)
	}

	if _, errs, code := h.run("copy", h.dir, "--to", dest, "--execute"); code != 1 || !strings.Contains(errs, "--yes") {
		t.Errorf("execute needs a terminal or --yes: %s", errs)
	}
	out, _ = h.ok("copy", h.dir, "--to", dest, "--execute", "--yes", "--min-free", "1")
	if !strings.Contains(out, "Done:") || !exists(audio) || !strings.Contains(out, "mediacat undo 1") {
		t.Fatalf("execute:\n%s", out)
	}
	ast, _ := os.Stat(files.Audio)
	cst, _ := os.Stat(audio)
	if !cst.ModTime().Equal(ast.ModTime()) {
		t.Errorf("the copy should keep the original's mtime")
	}
	// The copies are cataloged with their hash, so a second run finds nothing to do.
	out, _ = h.ok("copy", h.dir, "--to", dest)
	if !strings.Contains(out, "Nothing to copy") {
		t.Errorf("rerun:\n%s", out)
	}
	out, _ = h.ok("list", dest)
	if !strings.Contains(out, filepath.Base(files.Audio)) {
		t.Errorf("copy not cataloged:\n%s", out)
	}

	// Undo removes the copies (originals are still there) and their catalog rows.
	out, errs := h.ok("undo", "1", "--yes")
	if exists(audio) || !exists(files.Audio) || !strings.Contains(errs, "will be REMOVED from "+dest) || !strings.Contains(out, "copies removed") {
		t.Fatalf("undo:\n%s\n%s", out, errs)
	}
	out, _ = h.ok("history", "1")
	if !strings.Contains(out, "copy") {
		t.Errorf("history:\n%s", out)
	}
	if out, _ := h.ok("copy", h.dir, "--to", dest); strings.Contains(out, "Nothing to copy") {
		t.Errorf("after undo the files are missing from the target again:\n%s", out)
	}
}

func TestCopyMinFree(t *testing.T) {
	h := newHarness(t)
	testmedia.Generate(t, h.dir)
	target := otherDrive(t, h.dir)
	h.ok("scan", h.dir)
	h.ok("scan", target)
	out, _ := h.ok("copy", h.dir, "--to", filepath.Join(target, "in"), "--min-free", "100T")
	if !strings.Contains(out, "Only 0 of") || exists(filepath.Join(target, "in")) {
		t.Errorf("a margin larger than the drive copies nothing:\n%s", out)
	}
}
