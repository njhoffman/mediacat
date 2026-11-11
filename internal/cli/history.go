package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/njhoffman/mediacat/internal/actions"
	"github.com/njhoffman/mediacat/internal/catalog"
	"github.com/njhoffman/mediacat/internal/filter"
	"github.com/njhoffman/mediacat/internal/ui"
	"github.com/njhoffman/mediacat/internal/volume"
)

func (a *app) historyCmd() *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "history [batch]",
		Short: "List past trash, delete and move batches, or the files in one batch",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.open(); err != nil {
				return err
			}
			if len(args) == 1 {
				id, err := strconv.ParseInt(args[0], 10, 64)
				if err != nil {
					return fmt.Errorf("batch must be a number: %w", err)
				}
				return a.showBatch(id)
			}
			return a.listBatches(limit)
		},
	}
	cmd.Flags().IntVarP(&limit, "limit", "n", 20, "batches to show")
	return cmd
}

func batchStatus(b catalog.Batch) (string, *ui.Cell) {
	okS, warnS, dimS := ui.OK, ui.Warn, ui.Dim
	switch {
	case b.UndoneAt != nil:
		return "undone", &ui.Cell{Style: &dimS}
	case b.Status == catalog.ScanComplete && b.Failed == 0:
		return "complete", &ui.Cell{Style: &okS}
	case b.Status == catalog.ScanComplete:
		return "complete with failures", &ui.Cell{Style: &warnS}
	default:
		return b.Status, &ui.Cell{Style: &warnS}
	}
}

func (a *app) listBatches(limit int) error {
	bs, err := a.db.Batches(limit)
	if err != nil {
		return err
	}
	if len(bs) == 0 {
		fmt.Fprintln(a.stdout, ui.Dim.Render("No actions have been run yet."))
		return nil
	}
	cols := []ui.Column{{Title: "BATCH", Right: true}, {Title: "WHEN"}, {Title: "ACTION"}, {Title: "FILES", Right: true},
		{Title: "SIZE", Right: true}, {Title: "FAILED", Right: true}, {Title: "STATUS"}, {Title: "WHERE", Flex: true}}
	var rows [][]ui.Cell
	for _, b := range bs {
		st, sc := batchStatus(b)
		where := b.Root
		if b.Dest != "" {
			where += " -> " + b.Dest
		}
		rows = append(rows, []ui.Cell{{Text: fmt.Sprint(b.ID)}, {Text: ui.Age(b.StartedAt, a.now())}, {Text: b.Kind},
			{Text: ui.Count(b.Done)}, {Text: ui.Bytes(b.Bytes)}, {Text: ui.Count(b.Failed)}, {Text: st, Style: sc.Style}, {Text: where}})
	}
	ui.Table(a.stdout, cols, rows, ui.Width(a.stdout))
	return nil
}

func (a *app) showBatch(id int64) error {
	b, acts, err := a.db.GetBatch(id)
	if err != nil {
		return err
	}
	if b == nil {
		return fmt.Errorf("no batch %d", id)
	}
	st, _ := batchStatus(*b)
	fmt.Fprintf(a.stdout, "%s  %s  %s  %s\n\n", ui.Title.Render(fmt.Sprintf("Batch #%d", b.ID)), b.Kind,
		b.StartedAt.Format("2006-01-02 15:04"), st)
	cols := []ui.Column{{Title: "STATUS"}, {Title: "SIZE", Right: true}, {Title: "FILE", Flex: true}}
	var rows [][]ui.Cell
	okS, warnS, errS := ui.OK, ui.Warn, ui.Err
	for _, act := range acts {
		s := &okS
		switch act.Status {
		case catalog.ActionFailed:
			s = &errS
		case catalog.ActionSkipped:
			s = &warnS
		}
		text := act.SourcePath
		if act.Error != "" {
			text += "  (" + act.Error + ")"
		}
		rows = append(rows, []ui.Cell{{Text: act.Status, Style: s}, {Text: ui.Bytes(act.Size)}, {Text: text}})
	}
	ui.Table(a.stdout, cols, rows, ui.Width(a.stdout))
	return nil
}

func (a *app) undoCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "undo <batch>",
		Short: "Put files from a trash or move batch back where they were, or remove a copy batch's copies",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("batch must be a number: %w", err)
			}
			if err := a.open(); err != nil {
				return err
			}
			return a.runUndo(id, yes)
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation")
	return cmd
}

func (a *app) runUndo(id int64, yes bool) error {
	b, acts, err := a.db.GetBatch(id)
	if err != nil {
		return err
	}
	if b == nil {
		return fmt.Errorf("no batch %d", id)
	}
	if b.Kind == catalog.ActionDelete {
		return errors.New("permanently deleted files cannot be restored")
	}
	n, bytes := 0, int64(0)
	for _, act := range acts {
		if act.Status == catalog.ActionDone {
			n++
			bytes += act.Size
		}
	}
	if n == 0 {
		return fmt.Errorf("batch %d has nothing left to undo", id)
	}
	label, done := "Restoring", "restored"
	if b.Kind == catalog.ActionCopy {
		label, done = "Removing copies", "copies removed"
		fmt.Fprintf(a.stderr, "%s\n", ui.Banner(ui.LevelWarn,
			fmt.Sprintf("%s copies (%s) will be REMOVED from %s", ui.Count(n), ui.Bytes(bytes), b.Dest),
			"The originals are not touched. A copy whose original is gone or changed is kept."))
	} else {
		fmt.Fprintf(a.stderr, "%s\n", ui.Banner(ui.LevelInfo,
			fmt.Sprintf("%s files (%s) will be RESTORED to their original locations", ui.Count(n), ui.Bytes(bytes)),
			"Files whose original path is now occupied are left where they are."))
	}
	if !yes {
		ok, err := a.confirm("Proceed? [y/N] ", "")
		if err != nil {
			return fmt.Errorf("%w (or pass --yes)", err)
		}
		if !ok {
			fmt.Fprintln(a.stderr, "Cancelled.")
			return nil
		}
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	ex := &actions.Executor{DB: a.db, Now: a.now}
	var out *actions.Outcome
	err = ui.Progress{Out: a.stderr, Cancel: cancel}.Run(ctx, func(report func(ui.Update)) error {
		ex.Progress = func(e actions.Event) {
			report(ui.Update{Label: label, Done: e.Done, Total: e.Total, Bytes: e.Bytes, TotalBytes: e.TotalBytes, Current: e.Current})
		}
		var err error
		out, err = ex.Undo(ctx, id)
		return err
	})
	if out != nil {
		fmt.Fprintf(a.stdout, "%s %s files (%s) %s.\n", ui.OK.Render("Done:"), ui.Count(out.Done), ui.Bytes(out.Bytes), done)
		for _, f := range out.Failed {
			fmt.Fprintf(a.stdout, "  %s  %s\n", ui.Err.Render(f.Path), ui.Dim.Render(f.Err))
		}
		a.stashReminder(out.Paths)
	}
	return err
}

type trashOpts struct {
	empty, everything bool
	olderThan         string
	cutoff            time.Time // zero: no age limit
	confirm           string    // "delete" confirms without a terminal
}

func (a *app) trashCmd() *cobra.Command {
	var o trashOpts
	cmd := &cobra.Command{
		Use:   "trash [path]",
		Short: "Show the trash on path's drive, and empty what mediacat put there",
		Long: `Show the trash on the drive holding path (default: current directory): how much it
holds and how much of that mediacat put there.

--empty permanently removes the files mediacat trashed. --everything also removes items
other programs trashed. Both always ask you to type "delete".

--older-than AGE (7d, 2w, 36h) or DATE (2026-11-01) empties only what was trashed before
then: mediacat's batches by when they ran, other programs' items (with --everything) by
the DeletionDate in their .trashinfo. Recent batches stay undoable.

--confirm delete gives the confirmation up front, for scripts and timers: the files
mediacat trashed are removed without asking. It does not extend to --everything, which
removes other programs' items and is always confirmed in a terminal.`,
		Example: `  mediacat trash /mnt/nfs/Stash
  mediacat trash /mnt/nfs/Stash --empty
  mediacat trash /mnt/nfs/Stash --empty --older-than 7d
  mediacat trash /mnt/nfs/Stash --empty --older-than 7d --confirm delete`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			if o.everything && !o.empty {
				return errors.New("--everything needs --empty")
			}
			if o.confirm != "" {
				switch {
				case o.confirm != "delete":
					return fmt.Errorf(`--confirm takes the word "delete", not %q`, o.confirm)
				case !o.empty:
					return errors.New("--confirm delete confirms --empty; add --empty")
				case o.everything:
					return errors.New("--everything removes other programs' items too, so it is confirmed in a terminal only; drop --confirm or --everything")
				}
			}
			if o.olderThan != "" {
				if !o.empty {
					return errors.New("--older-than limits --empty; add --empty")
				}
				cutoff, err := filter.ParseTime(o.olderThan, a.now())
				if err != nil {
					return fmt.Errorf("--older-than: %w", err)
				}
				o.cutoff = cutoff
			}
			if err := a.open(); err != nil {
				return err
			}
			return a.runTrash(dir, o)
		},
	}
	cmd.Flags().BoolVar(&o.empty, "empty", false, "permanently remove files mediacat moved to this trash")
	cmd.Flags().BoolVar(&o.everything, "everything", false, "with --empty, also remove items other programs trashed")
	cmd.Flags().StringVar(&o.confirm, "confirm", "", `with --empty, "delete" confirms without a terminal (not for --everything)`)
	cmd.Flags().StringVar(&o.olderThan, "older-than", "", "with --empty, only what was trashed before this age or date, e.g. 7d or 2026-11-01")
	return cmd
}

func (a *app) runTrash(dir string, o trashOpts) error {
	v, _, err := a.res.Resolve(dir)
	if err != nil {
		return err
	}
	tr := actions.NewTrasher(a.res)
	loc := tr.Locate(v)
	usage, err := tr.Usage(loc)
	if err != nil {
		return err
	}
	ours, err := a.trashedHere(v, loc)
	if err != nil {
		return err
	}
	var ourBytes int64
	for _, act := range ours {
		ourBytes += act.Size
	}
	free, _ := actions.FreeBytes(v.MountPoint)
	fmt.Fprintf(a.stdout, "%s  %s\n", ui.Title.Render("Trash"), ui.Bold.Render(loc.Dir))
	fmt.Fprintf(a.stdout, "  %-22s %s\n", "drive", v.Key)
	fmt.Fprintf(a.stdout, "  %-22s %s items, %s\n", "in trash", ui.Count(usage.Items), ui.Bytes(usage.Bytes))
	fmt.Fprintf(a.stdout, "  %-22s %s files, %s\n", "put there by mediacat", ui.Count(len(ours)), ui.Bytes(ourBytes))
	fmt.Fprintf(a.stdout, "  %-22s %s\n", "free on drive", ui.Bytes(int64(free)))
	batches, err := a.trashBatches(ours)
	if err != nil {
		return err
	}
	if len(batches) > 0 {
		fmt.Fprintf(a.stdout, "\n  %s\n", ui.Bold.Render("mediacat's batches in this trash, oldest first"))
		for _, b := range batches {
			fmt.Fprintf(a.stdout, "  %s  %s  %9s files  %10s  %s\n", ui.Accent.Render(fmt.Sprintf("#%-4d", b.ID)),
				ui.Dim.Render(b.Started.Format("2006-01-02 15:04")), ui.Count(b.Files), ui.Bytes(b.Bytes), b.Root)
		}
	}
	if !o.empty {
		if len(ours) > 0 {
			fmt.Fprintf(a.stdout, "\n%s\n", ui.Dim.Render("Free that space with: mediacat trash "+ui.Quote(dir)+" --empty"))
		}
		return nil
	}

	// What will go: mediacat's entries (of batches older than the cutoff), or every item
	// (trashed before the cutoff).
	var old []actions.Item
	if !o.cutoff.IsZero() {
		var kept []catalog.Action
		for _, b := range batches {
			if !b.Started.Before(o.cutoff) {
				continue
			}
			for _, act := range ours {
				if act.BatchID == b.ID {
					kept = append(kept, act)
				}
			}
		}
		ours = kept
		if o.everything {
			if old, err = tr.ItemsBefore(loc, o.cutoff); err != nil {
				return err
			}
		}
	}
	count, bytes := len(ours), int64(0)
	for _, act := range ours {
		bytes += act.Size
	}
	scope := "the files mediacat trashed"
	if !o.cutoff.IsZero() {
		scope += " before " + o.cutoff.Format("2006-01-02 15:04")
	}
	switch {
	case o.everything && o.cutoff.IsZero():
		count, bytes, scope = usage.Items, usage.Bytes, "EVERYTHING in this trash, including items other programs trashed"
	case o.everything:
		count, bytes = len(old), 0
		for _, it := range old {
			bytes += it.Bytes
		}
		scope = "EVERYTHING trashed before " + o.cutoff.Format("2006-01-02 15:04") + ", including items other programs trashed"
	}
	if count == 0 {
		if !o.cutoff.IsZero() {
			fmt.Fprintf(a.stdout, "\nNothing was trashed before %s.\n", o.cutoff.Format("2006-01-02 15:04"))
			return nil
		}
		fmt.Fprintln(a.stdout, "\nNothing to empty.")
		return nil
	}
	fmt.Fprintf(a.stderr, "\n%s\n", ui.Banner(ui.LevelDanger,
		fmt.Sprintf("%s items (%s) will be PERMANENTLY DELETED", ui.Count(count), ui.Bytes(bytes)),
		"From: "+loc.Dir, "Scope: "+scope, "This cannot be undone."))
	if o.confirm != "delete" {
		ok, err := a.confirm(ui.Err.Render(`Type "delete" to empty the trash: `), "delete")
		if err != nil {
			hint := ` (or pass --confirm delete)`
			if o.everything {
				hint = ""
			}
			return fmt.Errorf("emptying the trash must be confirmed in a terminal%s: %w", hint, err)
		}
		if !ok {
			fmt.Fprintln(a.stderr, "Cancelled; nothing was deleted.")
			return nil
		}
	}
	removed, failed := 0, 0
	switch {
	case o.everything && o.cutoff.IsZero():
		removed, failed = emptyAll(loc)
		for _, act := range ours {
			a.db.MarkActionPurged(act.ID)
		}
	case o.everything:
		for _, it := range old {
			if err := os.RemoveAll(it.Path); err != nil {
				failed++
				fmt.Fprintf(a.stderr, "  %s  %s\n", ui.Err.Render(it.Path), err)
				continue
			}
			os.Remove(it.Info)
			removed++
		}
		// mediacat's entries removed by age (whatever their batch time) are purged.
		all, err := a.trashedHere(v, loc)
		if err != nil {
			return err
		}
		still := map[int64]bool{}
		for _, act := range all {
			still[act.ID] = true
		}
		acts, err := a.db.TrashedEntries(v.Key)
		if err != nil {
			return err
		}
		for _, act := range acts {
			if !still[act.ID] && isWithin(act.DestPath, loc.Dir) {
				a.db.MarkActionPurged(act.ID)
			}
		}
	default:
		for _, act := range ours {
			if err := os.RemoveAll(act.DestPath); err != nil {
				failed++
				fmt.Fprintf(a.stderr, "  %s  %s\n", ui.Err.Render(act.DestPath), err)
				continue
			}
			os.Remove(act.InfoPath)
			a.db.MarkActionPurged(act.ID)
			removed++
		}
	}
	fmt.Fprintf(a.stdout, "%s %s items removed from the trash", ui.OK.Render("Done:"), ui.Count(removed))
	if failed > 0 {
		fmt.Fprintf(a.stdout, ", %s", ui.Err.Render(fmt.Sprintf("%d failed", failed)))
	}
	fmt.Fprintln(a.stdout, ".")
	return nil
}

// trashBatch totals one batch's files still in a trash.
type trashBatch struct {
	ID      int64
	Started time.Time
	Root    string
	Files   int
	Bytes   int64
}

// trashBatches groups trash actions by batch, oldest first.
func (a *app) trashBatches(acts []catalog.Action) ([]trashBatch, error) {
	by := map[int64]*trashBatch{}
	var order []int64
	for _, act := range acts {
		b := by[act.BatchID]
		if b == nil {
			info, _, err := a.db.GetBatch(act.BatchID)
			if err != nil {
				return nil, err
			}
			b = &trashBatch{ID: act.BatchID, Started: act.At}
			if info != nil {
				b.Started, b.Root = info.StartedAt, info.Root
			}
			by[act.BatchID] = b
			order = append(order, act.BatchID)
		}
		b.Files++
		b.Bytes += act.Size
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
	out := make([]trashBatch, len(order))
	for i, id := range order {
		out[i] = *by[id]
	}
	return out, nil
}

// trashedHere returns mediacat's trash actions whose files are still in loc.
func (a *app) trashedHere(v volume.Volume, loc actions.Location) ([]catalog.Action, error) {
	acts, err := a.db.TrashedEntries(v.Key)
	if err != nil {
		return nil, err
	}
	var out []catalog.Action
	for _, act := range acts {
		if _, err := os.Lstat(act.DestPath); err == nil && isWithin(act.DestPath, loc.Dir) {
			out = append(out, act)
		}
	}
	return out, nil
}

func isWithin(p, dir string) bool {
	return len(p) > len(dir) && p[:len(dir)] == dir && p[len(dir)] == '/'
}

func emptyAll(loc actions.Location) (removed, failed int) {
	for _, sub := range []string{"files", "info"} {
		entries, err := os.ReadDir(loc.Dir + "/" + sub)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if err := os.RemoveAll(loc.Dir + "/" + sub + "/" + e.Name()); err != nil {
				failed++
			} else if sub == "files" {
				removed++
			}
		}
	}
	return removed, failed
}
