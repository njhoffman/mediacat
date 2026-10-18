package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/njhoffman/mediacat/internal/probe"
	"github.com/njhoffman/mediacat/internal/scan"
	"github.com/njhoffman/mediacat/internal/ui"
)

func (a *app) scanCmd() *cobra.Command {
	var o scan.Options
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "scan [path]",
		Short: "Recursively catalog media files under path (default: current directory)",
		Long: `Recursively catalog media files under path (default: current directory).

Only new or changed files (by size and modification time) are probed, so re-running
a scan is cheap. Renamed files are detected and keep their metadata. Files no longer
on disk are marked missing; --prune removes them from the catalog instead.

Ctrl-C stops after the files in progress and saves everything finished so far;
running the same scan again continues where it left off.

Names under scan.exclude in the config (trash and NAS metadata folders by default)
are always skipped: ` + strings.Join(a.cfg.Scan.Exclude, " "),
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			if err := a.open(); err != nil {
				return err
			}
			o.BaseExcludes = a.cfg.Scan.Exclude
			return a.runScan(dir, o, timeout)
		},
	}
	f := cmd.Flags()
	f.IntVarP(&o.Workers, "workers", "j", a.cfg.Scan.Workers, "parallel probes (0: 2 on network or spinning disks, up to 8 otherwise)")
	f.BoolVar(&o.Prune, "prune", false, "remove catalog entries for files no longer on disk instead of marking them missing")
	f.BoolVar(&o.Force, "force", false, "re-probe every file, even unchanged ones")
	f.StringSliceVar(&o.Excludes, "exclude", nil, "skip files and directories whose name matches this glob (repeatable)")
	f.DurationVar(&timeout, "probe-timeout", a.cfg.ProbeTimeout, "give up on a single file after this long (retried next scan)")
	return cmd
}

func (a *app) runScan(dir string, o scan.Options, timeout time.Duration) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	var sum *scan.Summary
	prog := ui.Progress{Out: a.stderr, Cancel: cancel}
	err := prog.Run(ctx, func(report func(ui.Update)) error {
		o.Progress = func(e scan.Event) {
			switch e.Phase {
			case scan.PhaseWalk:
				report(ui.Update{Label: "Walking", Counting: true, Found: e.Found})
			case scan.PhaseProbe:
				report(ui.Update{Label: "Probing", Done: e.Done, Total: e.Total, Bytes: e.Bytes, TotalBytes: e.TotalBytes, Current: e.Current})
			}
		}
		o.Now = a.now
		s := &scan.Scanner{DB: a.db, Resolver: a.res, Prober: probe.Prober{Timeout: timeout}, Options: o}
		var err error
		sum, err = s.Run(ctx, dir)
		return err
	})
	if sum != nil {
		a.printScanSummary(sum)
	}
	if err != nil {
		return err
	}
	if sum.Interrupted {
		return errInterrupted{}
	}
	return nil
}

func (a *app) printScanSummary(s *scan.Summary) {
	w := a.stdout
	where, _ := s.Volume.AbsPath(s.Root)
	status := ui.OK.Render("Scan complete")
	if s.Interrupted {
		status = ui.Warn.Render("Scan interrupted")
	}
	fmt.Fprintf(w, "%s  %s  %s\n", status, ui.Bold.Render(where),
		ui.Dim.Render(fmt.Sprintf("(%s, %s, %d workers)", s.Volume.Key, ui.Elapsed(s.Elapsed), s.Workers)))

	rows := []struct {
		label, note string
		n           int
		show        bool
	}{
		{"media files", "", s.Seen, true},
		{"added", "", s.Added, true},
		{"changed", "", s.Changed, true},
		{"renamed", "metadata kept", s.Renamed, s.Renamed > 0},
		{"unchanged", "", s.Unchanged, true},
		{"restored", "were missing, found again", s.Restored, s.Restored > 0},
		{"missing", "marked missing; --prune removes them", s.Missing, s.Missing > 0},
		{"pruned", "removed from catalog", s.Pruned, s.Pruned > 0},
		{"failed", "unreadable or not valid media; recorded", len(s.Failures), len(s.Failures) > 0},
		{"deferred", "network or timeout errors; retried next scan", len(s.Deferred), len(s.Deferred) > 0},
		{"walk errors", "directories not read; their files were left as they were", len(s.WalkErrors), len(s.WalkErrors) > 0},
	}
	for _, r := range rows {
		if !r.show {
			continue
		}
		label := fmt.Sprintf("  %-12s %10s", r.label, ui.Count(r.n))
		switch {
		case r.label == "failed" || r.label == "walk errors":
			label = ui.Err.Render(label)
		case r.label == "missing" || r.label == "deferred":
			label = ui.Warn.Render(label)
		}
		if r.note != "" {
			label += "  " + ui.Dim.Render(r.note)
		}
		fmt.Fprintln(w, label)
	}
	a.printFailures("Failed to probe", s.Failures, ui.Err)
	a.printFailures("Deferred", s.Deferred, ui.Warn)
	a.printFailures("Could not read", s.WalkErrors, ui.Err)
	if s.Interrupted {
		fmt.Fprintf(w, "\n%s Run %s again to continue.\n", ui.Warn.Render("Progress was saved."), ui.Bold.Render("mediacat scan "+ui.Quote(where)))
	}
}

func (a *app) printFailures(title string, fs []scan.Failure, st interface{ Render(...string) string }) {
	if len(fs) == 0 {
		return
	}
	const limit = 10
	fmt.Fprintf(a.stdout, "\n%s (%d)\n", ui.Bold.Render(title), len(fs))
	width := ui.Width(a.stdout)
	for i, f := range fs {
		if i == limit {
			fmt.Fprintf(a.stdout, "  %s\n", ui.Dim.Render(fmt.Sprintf("... and %d more (mediacat list --all shows errors in red)", len(fs)-limit)))
			break
		}
		line := f.RelPath + "  " + f.Err
		if width > 0 {
			line = ui.TruncateMiddle(line, width-2)
		}
		fmt.Fprintf(a.stdout, "  %s\n", st.Render(line))
	}
}
