package ui

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

func TestFormatters(t *testing.T) {
	cases := []struct{ got, want string }{
		{Bytes(0), "0 B"}, {Bytes(1023), "1023 B"}, {Bytes(1536), "1.5 KiB"},
		{Bytes(3 << 30), "3.0 GiB"}, {Bytes(3_700_000_000_000), "3.4 TiB"},
		{Count(0), "0"}, {Count(999), "999"}, {Count(1000), "1,000"}, {Count(8460123), "8,460,123"}, {Count(-1234), "-1,234"},
		{Duration(0), "-"}, {Duration(999), "0:01"}, {Duration(185_000), "3:05"}, {Duration(5_400_000), "1:30:00"},
		{Res(0, 1080), "-"}, {Res(1920, 1080), "1920x1080"},
		{Bitrate(0), "-"}, {Bitrate(128_000), "128 kb/s"}, {Bitrate(4_250_000), "4.2 Mb/s"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("got %q want %q", c.got, c.want)
		}
	}
}

func TestAgeAndStale(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	ages := map[time.Duration]string{
		10 * time.Second: "just now", 5 * time.Minute: "5m ago", 3 * time.Hour: "3h ago",
		72 * time.Hour: "3d ago", 90 * 24 * time.Hour: "3mo ago", 3 * 365 * 24 * time.Hour: "3y ago",
	}
	for d, want := range ages {
		if got := Age(now.Add(-d), now); got != want {
			t.Errorf("Age(-%s) = %q want %q", d, got, want)
		}
	}
	fresh := now.Add(-24 * time.Hour)
	old := now.Add(-40 * 24 * time.Hour)
	if Stale(&fresh, 30, now, "/x") != "" {
		t.Error("fresh scan reported stale")
	}
	if w := Stale(&old, 30, now, "/x"); !strings.Contains(w, "40d ago") || !strings.Contains(w, "2026-09-07") {
		t.Errorf("stale warning = %q", w)
	}
	if Stale(&old, 0, now, "/x") != "" {
		t.Error("days=0 disables the warning")
	}
	if w := Stale(nil, 30, now, "/x"); !strings.Contains(w, "No completed scan") {
		t.Errorf("never scanned = %q", w)
	}
}

func TestTruncateMiddle(t *testing.T) {
	if got := TruncateMiddle("short", 10); got != "short" {
		t.Error(got)
	}
	got := TruncateMiddle("Stash/Sorted/Stars/very-long-name.mp4", 20)
	if len([]rune(got)) != 20 || !strings.HasPrefix(got, "Stash/Sor") || !strings.HasSuffix(got, "name.mp4") {
		t.Errorf("TruncateMiddle = %q", got)
	}
	if got := TruncateMiddle("日本語のファイル名.mp4", 10); !strings.HasSuffix(got, ".mp4") {
		t.Errorf("wide chars = %q", got)
	}
}

func TestTable(t *testing.T) {
	DisableColor()
	var buf bytes.Buffer
	cols := []Column{{Title: "SIZE", Right: true}, {Title: "PATH", Flex: true}}
	Table(&buf, cols, [][]Cell{Cells("1.0 GiB", "a/very/long/path/to/a/file.mp4"), Cells("5 B", "b.mp4")}, 30)
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("lines = %q", lines)
	}
	if lines[0] != "   SIZE  PATH" || !strings.HasPrefix(lines[2], "    5 B  b.mp4") {
		t.Errorf("alignment: %q", lines)
	}
	for _, l := range lines {
		if len([]rune(l)) > 30 {
			t.Errorf("line exceeds width: %q", l)
		}
	}
	if !strings.HasSuffix(lines[1], "file.mp4") {
		t.Errorf("flex column should keep the filename: %q", lines[1])
	}
}

func TestPlainProgress(t *testing.T) {
	run := func(updates ...Update) string {
		var buf bytes.Buffer
		p := Progress{Out: &buf, Cancel: func() {}}
		err := p.Run(t.Context(), func(report func(Update)) error {
			for _, u := range updates {
				report(u)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}
	got := run(
		Update{Label: "Walking", Counting: true, Found: 1}, Update{Label: "Walking", Counting: true, Found: 16},
		Update{Label: "Probing", Total: 16}, Update{Label: "Probing", Total: 16, Done: 8}, Update{Label: "Probing", Total: 16, Done: 16},
	)
	if want := "Walking: 16 found\nProbing: 16/16\n"; got != want {
		t.Errorf("phase finals = %q", got)
	}
	got = run(Update{Label: "Walking", Counting: true}, Update{Label: "Probing"})
	if want := "Walking: 0 found\nProbing: nothing to do\n"; got != want {
		t.Errorf("empty probe = %q", got)
	}
	if got := run(); got != "" {
		t.Errorf("no updates = %q", got)
	}
	got = run(Update{Label: "Hashing", Total: 2}, Update{Label: "Hashing", Total: 2, Done: 2, Bytes: 3 << 30, TotalBytes: 3 << 30})
	if want := "Hashing: 2/2  3.0 GiB/3.0 GiB\n"; got != want {
		t.Errorf("bytes = %q", got)
	}
}

func TestBanner(t *testing.T) {
	DisableColor()
	b := Banner(LevelDanger, "3 files will be PERMANENTLY DELETED", "This cannot be undone.")
	lines := strings.Split(b, "\n")
	if len(lines) != 4 || !strings.Contains(lines[1], "PERMANENTLY DELETED") || !strings.HasPrefix(lines[0], "╭") {
		t.Errorf("banner:\n%s", b)
	}
}

func TestQuote(t *testing.T) {
	cases := map[string]string{
		"/mnt/nfs/Stash": "/mnt/nfs/Stash", "/mnt/nfs/Stash/Unsorted/Evan Stone": "'/mnt/nfs/Stash/Unsorted/Evan Stone'",
		"it's": `'it'\''s'`, "": "''", "$HOME": "'$HOME'", "a(1).mp4": "'a(1).mp4'",
	}
	for in, want := range cases {
		if got := Quote(in); got != want {
			t.Errorf("Quote(%q) = %s want %s", in, got, want)
		}
	}
}

func TestHeat(t *testing.T) {
	if Heat(0) != nil || Heat(-1) != nil {
		t.Error("no style at or under the threshold")
	}
	for _, profile := range []termenv.Profile{termenv.ANSI256, termenv.TrueColor} {
		lipgloss.SetColorProfile(profile)
		resetHeat()
		slight, max := Heat(0.01), Heat(5)
		if slight == nil || max == nil || slight == max || Heat(1) != max {
			t.Fatal("levels should map to distinct steps, clamped at 1")
		}
		for _, lv := range []float64{0.01, 0.5, 1} {
			if Heat(lv).GetBold() {
				t.Errorf("level %v is bold", lv)
			}
		}
	}
	if len(heatStyles) != heatStepsTrueColor {
		t.Errorf("true color should use %d steps, got %d", heatStepsTrueColor, len(heatStyles))
	}
	// Neighboring levels get different shades on true color.
	if Heat(0.50) == Heat(0.53) {
		t.Error("true color gradient too coarse")
	}
	lipgloss.SetColorProfile(termenv.Ascii)
	resetHeat()
	if blend(heatDark, 0) != "#ede3a1" || blend(heatDark, 1) != "#ff4d4d" {
		t.Errorf("gradient ends: %s %s", blend(heatDark, 0), blend(heatDark, 1))
	}
}
