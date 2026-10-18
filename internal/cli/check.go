package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/njhoffman/mediacat/internal/catalog"
	"github.com/njhoffman/mediacat/internal/filter"
	"github.com/njhoffman/mediacat/internal/integrity"
	"github.com/njhoffman/mediacat/internal/ui"
)

type checkFlags struct {
	all, full, recheck, asJSON bool
	kind                       string
	workers                    int
}

func (a *app) checkCmd() *cobra.Command {
	var cf checkFlags
	cmd := &cobra.Command{
		Use:   "check [path]",
		Short: "Check cataloged media for corruption (quick, or --full decode)",
		Long: `Check whether cataloged media under path (default: current directory) decodes.

The quick check decodes one second at the start and one near the end. It reads very
little, even over the network, and catches truncated and unreadable files. --full
decodes every file completely and finds damage anywhere, at the cost of reading all
of it (roughly the speed of the network or disk).

Results:
  ok          decodes cleanly
  damaged     decodes with errors, or data is missing (truncated)
  unreadable  not valid media, or nothing decodes
  unverified  the check itself failed (network error, timeout); retried next run

Results are saved and stay valid until the file changes, so re-running only checks
what is new; a full result also counts for quick runs. Ctrl-C keeps finished results.
Find problems later with: mediacat list --check damaged,unreadable`,
		Example: `  mediacat check /mnt/nfs/Stash
  mediacat check /mnt/nfs/Stash/Sorted --full
  mediacat list --check damaged,unreadable --columns +check`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			if err := a.open(); err != nil {
				return err
			}
			return a.runCheck(dir, cf)
		},
	}
	f := cmd.Flags()
	f.BoolVar(&cf.all, "all", false, "check every mounted volume instead of path")
	f.BoolVar(&cf.full, "full", a.cfg.Check.Level == catalog.LevelFull, "decode every file completely")
	f.BoolVar(&cf.recheck, "recheck", false, "check again even files with a current result")
	f.BoolVar(&cf.asJSON, "json", false, "print the problems found as JSON")
	f.StringVarP(&cf.kind, "type", "t", "", "only video, audio or image")
	f.IntVarP(&cf.workers, "workers", "j", a.cfg.Check.Workers, "parallel checks (0: 4, or 2 for --full on network or spinning disks)")
	return cmd
}

func (a *app) runCheck(dir string, cf checkFlags) error {
	flt := filter.Filter{Kind: cf.kind}
	if err := flt.Validate(); err != nil {
		return err
	}
	level := catalog.LevelQuick
	if cf.full {
		level = catalog.LevelFull
	}
	var volID int64
	var prefix, root string
	workers := cf.workers
	if cf.all {
		root = "all mounted volumes"
	} else {
		v, rel, err := a.res.Resolve(dir)
		if err != nil {
			return err
		}
		root, _ = v.AbsPath(rel)
		stored, err := a.db.VolumeByKey(v.Key)
		if err != nil {
			return err
		}
		if stored == nil {
			a.warn(ui.Stale(nil, a.staleDays, a.now(), root))
			return nil
		}
		volID, prefix = stored.ID, rel
		if err := a.staleness(volID, rel, root); err != nil {
			return err
		}
		// Quick checks wait on latency more than bandwidth, so network volumes get more
		// workers; full decodes read everything, so they get fewer.
		if workers == 0 && v.Slow() {
			workers = map[bool]int{true: 2, false: 4}[cf.full]
		}
	}
	if workers == 0 {
		workers = 4
	}
	files, err := a.db.Query(volID, prefix, flt)
	if err != nil {
		return err
	}
	a.mountedOnly(files)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	var sum *integrity.Summary
	label := "Checking"
	if cf.full {
		label = "Decoding"
	}
	err = ui.Progress{Out: a.stderr, Cancel: cancel}.Run(ctx, func(report func(ui.Update)) error {
		var err error
		sum, err = integrity.Run(ctx, a.db, integrity.Checker{Timeout: a.cfg.ProbeTimeout}, files, integrity.Options{
			Level: level, Workers: workers, Recheck: cf.recheck, Now: a.now,
			Progress: func(e integrity.Event) {
				report(ui.Update{Label: label, Done: e.Done, Total: e.Total, Bytes: e.Bytes, TotalBytes: e.TotalBytes, Current: e.Current})
			},
		})
		return err
	})
	if err != nil {
		return err
	}
	problems, err := a.db.Query(volID, prefix, filter.Filter{Kind: cf.kind, Checks: []string{catalog.CheckDamaged, catalog.CheckUnreadable, catalog.CheckUnverified}})
	if err != nil {
		return err
	}
	a.remap(problems)
	if cf.asJSON {
		out := make([]fileJSON, len(problems))
		for i, f := range problems {
			out[i] = toJSON(f)
		}
		enc := json.NewEncoder(a.stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			return err
		}
	} else {
		a.printCheck(sum, level, workers, root, problems)
	}
	if sum.Interrupted {
		return errInterrupted{}
	}
	return nil
}

func (a *app) printCheck(s *integrity.Summary, level string, workers int, root string, problems []catalog.File) {
	w := a.stdout
	head := ui.OK.Render("Check complete")
	if s.Interrupted {
		head = ui.Warn.Render("Check interrupted")
	}
	fmt.Fprintf(w, "%s  %s  %s\n", head, ui.Bold.Render(root), ui.Dim.Render(fmt.Sprintf("(%s, %d workers)", level, workers)))
	rows := []struct {
		label string
		n     int
		note  string
		st    interface{ Render(...string) string }
	}{
		{"checked", s.Checked, "", ui.Plain},
		{"ok", s.Counts[catalog.CheckOK], "", ui.OK},
		{"damaged", s.Counts[catalog.CheckDamaged], "errors or missing data", ui.Warn},
		{"unreadable", s.Counts[catalog.CheckUnreadable], "not valid media", ui.Err},
		{"unverified", s.Counts[catalog.CheckUnverified], "network or timeout errors; run again to retry", ui.Dim},
		{"current", s.Current, "already checked (--recheck to redo)", ui.Dim},
		{"changed", len(s.Changed), "changed since the last scan; run mediacat scan first", ui.Warn},
		{"unavailable", s.Unavailable, "on volumes that are not mounted", ui.Dim},
	}
	for _, r := range rows {
		if r.n == 0 && r.label != "checked" && r.label != "ok" {
			continue
		}
		line := r.st.Render(fmt.Sprintf("  %-12s %8s", r.label, ui.Count(r.n)))
		if r.note != "" {
			line += "  " + ui.Dim.Render(r.note)
		}
		fmt.Fprintln(w, line)
	}
	if len(problems) == 0 {
		fmt.Fprintf(w, "\n%s\n", ui.OK.Render("No known problems under "+root+"."))
		return
	}
	fmt.Fprintf(w, "\n%s\n", ui.Bold.Render(fmt.Sprintf("Problems under %s (%d, from this and earlier checks)", root, len(problems))))
	width := ui.Width(w)
	fit := func(text string, indent int) string {
		if width > 0 {
			return ui.TruncateMiddle(text, max(width-indent, 20))
		}
		return text
	}
	for _, f := range problems {
		c := checkCell(f.Check)
		fmt.Fprintf(w, "  %s  %9s  %s\n", c.Style.Render(fmt.Sprintf("%-17s", c.Text)), ui.Bytes(f.Size), fit(f.AbsPath, 32))
		fmt.Fprintf(w, "  %s\n", ui.Dim.Render(fit(f.Check.Detail, 4)))
	}
	fmt.Fprintf(w, "\n%s\n", ui.Dim.Render("List them any time: mediacat list --check damaged,unreadable --columns +check"))
}
