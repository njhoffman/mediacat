package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/njhoffman/mediacat/internal/testmedia"
)

func TestCheckCommand(t *testing.T) {
	h := newHarness(t)
	testmedia.GenerateBroken(t, h.dir)
	h.ok("scan", h.dir)

	out, _ := h.ok("check", h.dir)
	for _, want := range []string{"Check complete", "(quick, 4 workers)", "checked             5", "damaged             2",
		"Problems under", "truncated.mp4", "cut.jpg", "probably truncated", "mediacat list --check"} {
		if !strings.Contains(out, want) {
			t.Errorf("quick check output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "good.mp4") || strings.Contains(out, "corrupt.mp4") {
		t.Errorf("quick check flagged a file it cannot judge:\n%s", out)
	}

	out, _ = h.ok("check", h.dir)
	if !strings.Contains(out, "checked             0") || !strings.Contains(out, "current             5") || !strings.Contains(out, "Problems under") {
		t.Errorf("second run should reuse results but still report problems:\n%s", out)
	}

	out, _ = h.ok("check", h.dir, "--full", "--type", "video")
	if !strings.Contains(out, "(full, 4 workers)") || !strings.Contains(out, "checked             3") || !strings.Contains(out, "decode errors") ||
		!strings.Contains(out, "corrupt.mp4") {
		t.Errorf("full check:\n%s", out)
	}

	out, _ = h.ok("list", h.dir, "--check", "damaged", "--columns", "name,check")
	if !strings.Contains(out, "CHECK") || !strings.Contains(out, "damaged (full)") || !strings.Contains(out, "cut.jpg") || strings.Contains(out, "good.mp4") {
		t.Errorf("list --check:\n%s", out)
	}
	out, _ = h.ok("list", h.dir, "--check", "ok", "--columns", "name")
	if !strings.Contains(out, "good.mp4") || !strings.Contains(out, "ok.png") || strings.Contains(out, "corrupt") {
		t.Errorf("list --check ok:\n%s", out)
	}

	out, _ = h.ok("check", h.dir, "--json")
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil || len(rows) != 3 { // truncated, corrupt (full), cut
		t.Fatalf("json: %v\n%s", err, out)
	}
	if c, _ := rows[0]["check"].(map[string]any); c == nil || c["status"] != "damaged" {
		t.Errorf("json check field: %v", rows[0])
	}
	if _, errs, code := h.run("check", h.dir, "--type", "pdf"); code != 1 || !strings.Contains(errs, "unknown type") {
		t.Errorf("bad type: %s", errs)
	}
}
