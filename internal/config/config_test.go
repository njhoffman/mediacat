package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTemplateIsDefault(t *testing.T) {
	c := Default()
	if c.StaleDays != 30 || c.Dupes.Depth != "full" || len(c.Scan.Exclude) != 9 || c.ProbeTimeout.Minutes() != 2 ||
		len(c.Dupes.PreferredPaths) != 2 || c.Dupes.PreferredPaths[0] != "/sorted" || c.Dupes.PreferredPaths[1] != "^incoming" || !strings.Contains(c.Dupes.Keep, "paths") ||
		c.List.Columns == "" || c.List.AudioColumns == "" {
		t.Errorf("defaults: %+v", c)
	}
	h := c.Highlights
	if h.Size.Warn != 500<<20 || h.Size.Max != 4<<30 || h.Duration.Warn != 1_800_000 || h.Duration.Max != 7_200_000 ||
		h.Resolution.Warn != 1920*1080 || h.Resolution.Max != 3840*2160 {
		t.Errorf("highlights: %+v", h)
	}
	if !strings.HasSuffix(c.Catalog, ".local/share/mediacat/catalog.db") || strings.HasPrefix(c.Catalog, "~") {
		t.Errorf("catalog path not expanded: %s", c.Catalog)
	}
}

func TestLoadCreatesAndMerges(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "mediacat.yaml")
	c, created, err := Load(p)
	if err != nil || !created || c.StaleDays != 30 {
		t.Fatalf("first load: %v %v", created, err)
	}
	if b, _ := os.ReadFile(p); string(b) != Template {
		t.Error("created file should be the template")
	}
	os.WriteFile(p, []byte("stale_days: 7\nlist:\n  highlight:\n    size:\n      warn: ''\n"), 0o644)
	c, created, err = Load(p)
	if err != nil || created || c.StaleDays != 7 || c.Dupes.Keep == "" || c.Highlights.Size.Enabled || !c.Highlights.Duration.Enabled {
		t.Errorf("merge: %+v %v", c, err)
	}
}

func TestLoadErrors(t *testing.T) {
	for _, body := range []string{
		"stale_dayz: 3\n",
		"list:\n  sort: loudness\n",
		"list:\n  highlight:\n    duration:\n      warn: 2h\n      max: 30m\n",
		"list:\n  highlight:\n    resolution:\n      warn: big\n",
		"scan:\n  probe_timeout: soon\n",
		"list:\n  columns: size,loudness\n",
	} {
		p := filepath.Join(t.TempDir(), "c.yaml")
		os.WriteFile(p, []byte(body), 0o644)
		if _, _, err := Load(p); err == nil {
			t.Errorf("expected error for %q", body)
		}
	}
}

func TestScaleLevel(t *testing.T) {
	s := Scale{Enabled: true, Warn: 10, Max: 20}
	for v, want := range map[float64]float64{5: 0, 10: 0, 15: 0.5, 20: 1, 99: 1} {
		if got := s.Level(v); got != want {
			t.Errorf("Level(%v) = %v want %v", v, got, want)
		}
	}
	if (Scale{}).Level(100) != 0 {
		t.Error("disabled scale")
	}
}
