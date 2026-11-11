package cli

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/njhoffman/mediacat/internal/catalog"
	"github.com/njhoffman/mediacat/internal/dupes"
)

func TestSummarizeAreas(t *testing.T) {
	f := func(p string, size int64) catalog.File { return catalog.File{AbsPath: p, Size: size} }
	groups := []dupes.Group{
		{Files: []catalog.File{f("/lib/Sorted/a", 10), f("/lib/Incoming/a", 10)}, Keep: 0},                        // kept in, rest out
		{Files: []catalog.File{f("/lib/Sorted/b", 5), f("/lib/Sorted/x/b", 5), f("/lib/Incoming/b", 5)}, Keep: 0}, // several in
		{Files: []catalog.File{f("/lib/Sorted/c", 7), f("/lib/Unsorted/c", 7)}, Keep: 1},                          // kept out
		{Files: []catalog.File{f("/lib/Incoming/d", 3), f("/lib/Unsorted/d", 3)}, Keep: 0},                        // none in
		{Files: []catalog.File{f("/lib/Sortedx/e", 2), f("/lib/Incoming/e", 2)}, Keep: 0},                         // a sibling prefix is outside
	}
	// A guard sparing copies under /lib/Sorted, like --keep-all-preferred would.
	guard := func(fl catalog.File) string {
		if inDir(fl.AbsPath, "/lib/Sorted") {
			return "preferred"
		}
		return ""
	}
	got := summarizeAreas(groups, []string{"/lib/Sorted"}, guard)
	if len(got) != 1 || got[0].Dir != "/lib/Sorted" {
		t.Fatalf("areas: %+v", got)
	}
	want := []areaRow{
		{Bucket: "kept-in-rest-outside", Groups: 1, ExtraCopies: 1, Reclaimable: 10},
		{Bucket: "several-in", Groups: 1, ExtraCopies: 2, Reclaimable: 5, Spared: 5},
		{Bucket: "kept-outside", Groups: 1, ExtraCopies: 1, Spared: 7},
		{Bucket: "none-in", Groups: 2, ExtraCopies: 2, Reclaimable: 5},
	}
	for i, w := range want {
		g := got[0].Buckets[i]
		g.Label = ""
		if g != w {
			t.Errorf("bucket %d = %+v, want %+v", i, g, w)
		}
	}
	if none := summarizeAreas(groups, []string{"/lib/Sorted"}, nil)[0].Buckets[2]; none.Reclaimable != 7 || none.Spared != 0 {
		t.Errorf("without guards everything extra is reclaimable: %+v", none)
	}
}

func TestDupesSummaryBy(t *testing.T) {
	h, _ := dupeFixture(t)
	backup := filepath.Join(h.dir, "backup")

	if _, errs, code := h.run("dupes", h.dir, "--summary-by", backup, "--trash"); code != 1 || !strings.Contains(errs, "only reports") {
		t.Errorf("with an action: %d %s", code, errs)
	}
	out, _ := h.ok("dupes", h.dir, "--summary-by", backup, "--summary-by", h.dir)
	for _, want := range []string{"By area", backup, "kept copy outside, some copies inside", "kept copy inside, 2+ copies inside", "RECLAIMABLE", "total"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "dupe ") {
		t.Errorf("the summary replaces the group listing:\n%s", out)
	}

	out, _ = h.ok("dupes", h.dir, "--summary-by", backup, "--keep-all-preferred", "--prefer-path", "/backup/", "--json")
	var areas []areaSummary
	if err := json.Unmarshal([]byte(out), &areas); err != nil || len(areas) != 1 {
		t.Fatalf("json: %v %s", err, out)
	}
	// The kept copy is in backup now (paths rule), and the others are outside.
	b := areas[0].Buckets[0]
	if b.Bucket != "kept-in-rest-outside" || b.Groups != 1 || b.ExtraCopies != 2 || b.Reclaimable == 0 || b.Spared != 0 {
		t.Errorf("json bucket: %+v", areas[0].Buckets)
	}
}
