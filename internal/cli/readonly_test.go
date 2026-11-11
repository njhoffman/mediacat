package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/njhoffman/mediacat/internal/testmedia"
)

func TestReadOnlyFlag(t *testing.T) {
	h := newHarness(t)
	testmedia.Generate(t, h.dir)
	h.ok("scan", h.dir)
	// The temp directory is not on a removable drive: an explicit --readonly says so.
	for _, args := range [][]string{{"scan", h.dir, "--readonly"}, {"check", h.dir, "--readonly"}, {"dupes", h.dir, "--readonly"}} {
		if _, errs, code := h.run(args...); code != 1 || !strings.Contains(errs, "only removable drives") {
			t.Errorf("%v: %d %s", args, code, errs)
		}
	}
	for _, args := range [][]string{{"dupes", h.dir, "--readonly", "--trash"}, {"dupes", h.dir, "--readonly", "--review"}, {"dupes", "--all", "--readonly"}} {
		if _, errs, code := h.run(args...); code != 1 || !strings.Contains(errs, "--readonly is for reports") {
			t.Errorf("%v: %d %s", args, code, errs)
		}
	}
	if _, errs, code := h.run("check", "--all", "--readonly"); code != 1 || !strings.Contains(errs, "drop --all") {
		t.Errorf("check --all --readonly: %d %s", code, errs)
	}

	// From the config the default only applies where it can: elsewhere runs go ahead.
	os.WriteFile(h.config, []byte("scan:\n  readonly_removable: true\n"), 0o644)
	h.ok("scan", h.dir)
	h.ok("dupes", h.dir)
	h.ok("dupes", h.dir, "--trash")
	h.ok("check", h.dir)
}
