package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/njhoffman/mediacat/internal/actions"
	"github.com/njhoffman/mediacat/internal/dupes"
	"github.com/njhoffman/mediacat/internal/ui"
)

// Area buckets for dupes --summary-by, judged by the copy the keep rules choose.
var areaBuckets = []struct{ key, label string }{
	{"kept-in-rest-outside", "kept copy inside, the others outside"},
	{"several-in", "kept copy inside, 2+ copies inside"},
	{"kept-outside", "kept copy outside, some copies inside"},
	{"none-in", "no copy inside"},
}

type areaRow struct {
	Bucket      string `json:"bucket"`
	Label       string `json:"label"`
	Groups      int    `json:"groups"`
	ExtraCopies int    `json:"extra_copies"`
	Reclaimable int64  `json:"reclaimable"` // what the current keep rules and guards would remove
	Spared      int64  `json:"spared"`      // extra copies the guards leave in place
}

type areaSummary struct {
	Dir     string    `json:"dir"`
	Buckets []areaRow `json:"buckets"`
}

func inDir(path, dir string) bool {
	return path == dir || strings.HasPrefix(path, strings.TrimSuffix(dir, "/")+"/")
}

// summarizeAreas sorts each group into a bucket per directory and totals what the current
// keep rules and guards would remove.
func summarizeAreas(groups []dupes.Group, dirs []string, guard actions.Guard) []areaSummary {
	var out []areaSummary
	for _, dir := range dirs {
		rows := make([]areaRow, len(areaBuckets))
		for i, b := range areaBuckets {
			rows[i] = areaRow{Bucket: b.key, Label: b.label}
		}
		for _, g := range groups {
			inside := 0
			for _, f := range g.Files {
				if inDir(f.AbsPath, dir) {
					inside++
				}
			}
			keptIn := inDir(g.Files[g.Keep].AbsPath, dir)
			b := 3
			switch {
			case keptIn && inside == 1:
				b = 0
			case keptIn:
				b = 1
			case inside > 0:
				b = 2
			}
			r := &rows[b]
			r.Groups++
			r.ExtraCopies += len(g.Files) - 1
			for i, f := range g.Files {
				if i == g.Keep {
					continue
				}
				if guard != nil && guard(f) != "" {
					r.Spared += f.Size
				} else {
					r.Reclaimable += f.Size
				}
			}
		}
		out = append(out, areaSummary{Dir: dir, Buckets: rows})
	}
	return out
}

func writeAreasJSON(w io.Writer, s []areaSummary) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(s)
}

func (a *app) printAreas(s []areaSummary) {
	w := a.stdout
	for _, area := range s {
		fmt.Fprintf(w, "\n%s  %s\n", ui.Title.Render("By area"), ui.Bold.Render(area.Dir))
		fmt.Fprintf(w, "  %-44s %8s %8s %12s %12s\n", "", "GROUPS", "EXTRA", "RECLAIMABLE", "SPARED")
		var groups, extra int
		var rec, spared int64
		for _, r := range area.Buckets {
			line := fmt.Sprintf("  %-44s %8s %8s %12s %12s", r.Label, ui.Count(r.Groups), ui.Count(r.ExtraCopies), ui.Bytes(r.Reclaimable), ui.Bytes(r.Spared))
			if r.Groups == 0 {
				line = ui.Dim.Render(line)
			}
			fmt.Fprintln(w, line)
			groups, extra, rec, spared = groups+r.Groups, extra+r.ExtraCopies, rec+r.Reclaimable, spared+r.Spared
		}
		fmt.Fprintln(w, ui.Bold.Render(fmt.Sprintf("  %-44s %8s %8s %12s %12s", "total", ui.Count(groups), ui.Count(extra), ui.Bytes(rec), ui.Bytes(spared))))
	}
	fmt.Fprintf(w, "\n%s\n", ui.Dim.Render("Reclaimable: what the current --keep rules and guards (--only-under, --keep-all-preferred, Stash's primary files) would remove; spared: copies the guards leave."))
}
