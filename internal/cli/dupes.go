package cli

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/njhoffman/mediacat/internal/actions"
	"github.com/njhoffman/mediacat/internal/catalog"
	"github.com/njhoffman/mediacat/internal/dupes"
	"github.com/njhoffman/mediacat/internal/filter"
	"github.com/njhoffman/mediacat/internal/scan"
	"github.com/njhoffman/mediacat/internal/stash"
	"github.com/njhoffman/mediacat/internal/ui"
	"github.com/njhoffman/mediacat/internal/volume"
)

type dupesFlags struct {
	all       bool
	depth     string
	kind      string
	minSize   string
	keep      string
	prefer    []string
	avoid     []string
	trash     bool
	del       bool
	moveTo    string
	execute   bool
	yes       bool
	asJSON    bool
	export    string
	limit     int
	workers   int
	showFiles bool
}

func (a *app) dupesCmd() *cobra.Command {
	var df dupesFlags
	rules := make([]string, 0, len(dupes.Rules))
	for r := range dupes.Rules {
		rules = append(rules, r)
	}
	sort.Strings(rules)
	var ruleHelp strings.Builder
	for _, r := range rules {
		fmt.Fprintf(&ruleHelp, "  %-9s %s\n", r, dupes.Rules[r])
	}
	cmd := &cobra.Command{
		Use:   "dupes [path]",
		Short: "Find duplicate files and trash, delete or move the extra copies",
		Long: `Find duplicate media files under path (default: current directory) from the catalog.

Depths:
  size     same size only (fast, many false matches; report only)
  partial  same size and same first/middle/last 64 KiB (report only)
  full     identical content by full hash (default; required for actions)

Hashes are saved as they are computed, so an interrupted run continues where it
stopped. Files changed since the last scan are skipped.

One copy per group is kept, chosen by --keep rules applied in order:
` + ruleHelp.String() + `
Actions are a dry run unless --execute is given. --trash only uses the trash on the
files' own drive; --move-to keeps the layout below path; --delete is permanent and
always asks you to type "delete".`,
		Example: `  mediacat dupes                                  # report duplicates here
  mediacat dupes /mnt/nfs/Stash --prefer /mnt/nfs/Stash/Sorted --trash
  mediacat dupes --trash --execute
  mediacat dupes --move-to /mnt/nfs/trashbox --execute --yes
  mediacat dupes --export dupes.csv`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			return a.runDupes(dir, df)
		},
	}
	f := cmd.Flags()
	f.BoolVar(&df.all, "all", false, "search every mounted volume instead of path")
	f.StringVar(&df.depth, "depth", "full", "size, partial or full")
	f.StringVarP(&df.kind, "type", "t", "", "only video, audio or image")
	f.StringVar(&df.minSize, "min-size", "1", "ignore files smaller than this, e.g. 1M")
	f.StringVar(&df.keep, "keep", dupes.DefaultPolicy, "rules choosing the copy to keep, in order")
	f.StringSliceVar(&df.prefer, "prefer", nil, "keep copies under this directory (repeatable, first wins)")
	f.StringSliceVar(&df.avoid, "avoid", nil, "avoid keeping copies under this directory (repeatable)")
	f.BoolVar(&df.trash, "trash", false, "move extra copies to the trash on their own drive")
	f.BoolVar(&df.del, "delete", false, "permanently delete extra copies")
	f.StringVar(&df.moveTo, "move-to", "", "move extra copies under this directory")
	f.BoolVar(&df.execute, "execute", false, "apply the action (otherwise a dry run)")
	f.BoolVarP(&df.yes, "yes", "y", false, "skip the confirmation for --trash and --move-to (never for --delete)")
	f.BoolVar(&df.asJSON, "json", false, "print groups as JSON")
	f.StringVar(&df.export, "export", "", "write all groups to a .csv or .json file")
	f.IntVarP(&df.limit, "limit", "n", 20, "groups to show (0 for all)")
	f.IntVarP(&df.workers, "workers", "j", 0, "parallel hashing (default: 2 network, 1 spinning disk, 4 otherwise)")
	f.BoolVar(&df.showFiles, "dry-run-files", false, "list every file the action would touch")
	return cmd
}

func (df dupesFlags) action() (string, error) {
	n := 0
	kind := ""
	if df.trash {
		n, kind = n+1, catalog.ActionTrash
	}
	if df.del {
		n, kind = n+1, catalog.ActionDelete
	}
	if df.moveTo != "" {
		n, kind = n+1, catalog.ActionMove
	}
	if n > 1 {
		return "", errors.New("choose one of --trash, --delete and --move-to")
	}
	if df.execute && kind == "" {
		return "", errors.New("--execute needs an action: --trash, --delete or --move-to")
	}
	return kind, nil
}

func absDirs(dirs []string) ([]string, error) {
	var out []string
	for _, d := range dirs {
		abs, err := filepath.Abs(d)
		if err != nil {
			return nil, err
		}
		if real, err := filepath.EvalSymlinks(abs); err == nil {
			abs = real
		}
		out = append(out, abs)
	}
	return out, nil
}

func (a *app) runDupes(dir string, df dupesFlags) error {
	kind, err := df.action()
	if err != nil {
		return err
	}
	depth, err := dupes.ParseDepth(df.depth)
	if err != nil {
		return err
	}
	if kind != "" && depth != dupes.DepthFull {
		return errors.New("actions need --depth full: smaller depths only find possible duplicates")
	}
	if df.asJSON && kind != "" {
		return errors.New("--json reports only; drop it to run an action")
	}
	minSize, err := filter.ParseSize(df.minSize)
	if err != nil {
		return err
	}
	flt := filter.Filter{Kind: strings.ToLower(df.kind), MinSize: max(minSize, 1)}
	if err := flt.Validate(); err != nil {
		return err
	}
	prefer, err := absDirs(df.prefer)
	if err != nil {
		return err
	}
	avoid, err := absDirs(df.avoid)
	if err != nil {
		return err
	}
	policy, err := dupes.ParsePolicy(df.keep, prefer, avoid)
	if err != nil {
		return err
	}
	if err := a.open(); err != nil {
		return err
	}

	var volID int64
	var prefix, root string
	workers := df.workers
	if !df.all {
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
		if workers == 0 {
			switch {
			case v.Network:
				workers = 2
			case v.Rotational:
				workers = 1
			default:
				workers = 4
			}
		}
	}
	files, err := a.db.Query(volID, prefix, flt)
	if err != nil {
		return err
	}
	vols := a.mountedOnly(files)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	var res *dupes.Result
	prog := ui.Progress{Out: a.stderr, Cancel: cancel}
	err = prog.Run(ctx, func(report func(ui.Update)) error {
		finder := &dupes.Finder{DB: a.db, Options: dupes.Options{Depth: depth, Workers: workers, Policy: policy,
			Progress: func(e dupes.Event) {
				report(ui.Update{Label: e.Stage, Done: e.Done, Total: e.Total, Bytes: e.Bytes, TotalBytes: e.TotalBytes, Current: e.Current})
			}}}
		var err error
		res, err = finder.Find(ctx, files)
		return err
	})
	if err != nil {
		return err
	}
	if res.Interrupted {
		fmt.Fprintf(a.stdout, "%s Hashes computed so far are saved; run the same command again to continue.\n", ui.Warn.Render("Interrupted."))
		return errInterrupted{}
	}

	if df.export != "" {
		if err := exportGroups(df.export, res.Groups); err != nil {
			return err
		}
	}
	if df.asJSON {
		return writeGroupsJSON(a.stdout, res.Groups)
	}
	where := root
	if df.all {
		where = "all mounted volumes"
	}
	a.printDupes(res, where, df.limit)
	if df.export != "" {
		fmt.Fprintf(a.stdout, "\nExported %d groups to %s\n", len(res.Groups), df.export)
	}
	if len(res.Groups) == 0 {
		return nil
	}
	if kind == "" {
		if depth == dupes.DepthFull {
			fmt.Fprintf(a.stdout, "\n%s\n", ui.Dim.Render("To act on the extra copies add --trash, --move-to DIR or --delete (a dry run until --execute)."))
		} else {
			fmt.Fprintf(a.stdout, "\n%s\n", ui.Dim.Render("These are possible duplicates only; use --depth full to confirm them."))
		}
		return nil
	}
	if root == "" {
		root = "/"
	}
	return a.act(ctx, kind, root, df, res.Groups, vols)
}

func (a *app) printDupes(res *dupes.Result, where string, limit int) {
	w := a.stdout
	var files, extra int
	var reclaim int64
	for _, g := range res.Groups {
		files += len(g.Files)
		extra += len(g.Files) - 1
		reclaim += g.Reclaimable()
	}
	label := map[dupes.Depth]string{dupes.DepthFull: "identical content", dupes.DepthPartial: "same size and samples", dupes.DepthSize: "same size"}[res.Depth]
	fmt.Fprintf(w, "%s  %s  %s\n", ui.Title.Render("Duplicates"), ui.Bold.Render(where),
		ui.Dim.Render(fmt.Sprintf("(%s; %s candidates, %s hashed now)", label, ui.Count(res.Candidates), ui.Count(res.Hashed))))
	if len(res.Groups) == 0 {
		fmt.Fprintln(w, ui.OK.Render("  No duplicates found."))
	} else {
		fmt.Fprintf(w, "  %s groups, %s files, %s extra copies, %s reclaimable\n",
			ui.Bold.Render(ui.Count(len(res.Groups))), ui.Count(files), ui.Count(extra), ui.Bold.Render(ui.Bytes(reclaim)))
	}
	width := ui.Width(w)
	for i, g := range res.Groups {
		if limit > 0 && i == limit {
			fmt.Fprintf(w, "\n%s\n", ui.Dim.Render(fmt.Sprintf("%d more groups not shown (--limit 0 shows all)", len(res.Groups)-limit)))
			break
		}
		key := g.Key
		if len(key) > 12 {
			key = key[:12]
		}
		fmt.Fprintf(w, "\n%s  %s  %s  %s\n", ui.Accent.Render(fmt.Sprintf("#%d", i+1)),
			fmt.Sprintf("%d copies of %s", len(g.Files), ui.Bytes(g.Size)),
			ui.Bold.Render(ui.Bytes(g.Reclaimable())+" reclaimable"), ui.Dim.Render(key))
		for j, f := range g.Files {
			tag, st := "dupe", ui.Warn
			if j == g.Keep {
				tag, st = "keep", ui.OK
			}
			if !g.Confirmed {
				tag, st = "  ? ", ui.Dim
			}
			line := f.AbsPath
			prefix := fmt.Sprintf("  %s  %s  ", st.Render(tag), ui.Dim.Render(time.Unix(0, f.MtimeNS).Format("2006-01-02")))
			if width > 0 {
				line = ui.TruncateMiddle(line, max(width-20, 20))
			}
			fmt.Fprintln(w, prefix+line)
		}
	}
	notes := []struct {
		n    int
		text string
		st   interface{ Render(...string) string }
	}{
		{len(res.Changed), "files changed since the last scan were skipped; run mediacat scan first", ui.Warn},
		{len(res.Deferred), "files had network errors and were skipped; run again to retry", ui.Warn},
		{len(res.Failed), "files could not be read", ui.Err},
		{res.Unavailable, "cataloged files are on volumes that are not mounted", ui.Dim},
		{res.Hardlinks, "hard links to files already counted were ignored", ui.Dim},
	}
	first := true
	for _, n := range notes {
		if n.n == 0 {
			continue
		}
		if first {
			fmt.Fprintln(w)
			first = false
		}
		fmt.Fprintf(w, "%s\n", n.st.Render(fmt.Sprintf("  %s %s", ui.Count(n.n), n.text)))
	}
	for _, f := range append(res.Failed, res.Deferred...) {
		fmt.Fprintf(w, "    %s  %s\n", f.Path, ui.Dim.Render(f.Err))
	}
}

// act builds the plan, shows exactly what will happen, asks, and runs it.
func (a *app) act(ctx context.Context, kind, root string, df dupesFlags, groups []dupes.Group, vols map[string]volume.Volume) error {
	moveTo := ""
	if kind == catalog.ActionMove {
		abs, err := filepath.Abs(df.moveTo)
		if err != nil {
			return err
		}
		moveTo = abs
	}
	plan, err := actions.BuildPlan(kind, root, moveTo, groups, vols)
	if err != nil {
		return err
	}
	if len(plan.Ops) == 0 {
		fmt.Fprintln(a.stdout, "\nNothing to do.")
		return nil
	}
	trasher := actions.NewTrasher(a.res)
	level, lines, err := a.describePlan(plan, trasher)
	if err != nil {
		return err
	}
	if !df.execute {
		lines[0] = "DRY RUN: " + strings.Replace(lines[0], " will be ", " would be ", 1)
		fmt.Fprintf(a.stdout, "\n%s\n", ui.Banner(level, lines...))
		if df.showFiles {
			for _, op := range plan.Ops {
				fmt.Fprintf(a.stdout, "  %s\n", op.File.AbsPath)
			}
		}
		fmt.Fprintf(a.stdout, "%s\n", ui.Dim.Render("Nothing was changed. Add --execute to apply."))
		return nil
	}
	fmt.Fprintf(a.stderr, "\n%s\n", ui.Banner(level, lines...))
	switch {
	case kind == catalog.ActionDelete:
		ok, err := a.confirm(ui.Err.Render(`Type "delete" to permanently delete these files: `), "delete")
		if err != nil {
			return fmt.Errorf("permanent deletion must be confirmed in a terminal: %w", err)
		}
		if !ok {
			fmt.Fprintln(a.stderr, "Cancelled; nothing was deleted.")
			return nil
		}
	case !df.yes:
		ok, err := a.confirm("Proceed? [y/N] ", "")
		if err != nil {
			return fmt.Errorf("%w (or pass --yes)", err)
		}
		if !ok {
			fmt.Fprintln(a.stderr, "Cancelled; nothing was changed.")
			return nil
		}
	}

	ex := &actions.Executor{DB: a.db, Trasher: trasher, Now: a.now}
	label := map[string]string{catalog.ActionTrash: "Trashing", catalog.ActionDelete: "Deleting", catalog.ActionMove: "Moving"}[kind]
	var out *actions.Outcome
	prog := ui.Progress{Out: a.stderr, Cancel: func() {}}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	prog.Cancel = cancel
	err = prog.Run(ctx, func(report func(ui.Update)) error {
		ex.Progress = func(e actions.Event) {
			report(ui.Update{Label: label, Done: e.Done, Total: e.Total, Bytes: e.Bytes, TotalBytes: e.TotalBytes, Current: e.Current})
		}
		var err error
		out, err = ex.Execute(ctx, plan)
		return err
	})
	if out != nil {
		a.printOutcome(kind, plan, out)
	}
	if err != nil {
		return err
	}
	if out.Interrupted {
		return errInterrupted{}
	}
	return nil
}

var actionVerb = map[string]string{
	catalog.ActionTrash:  "MOVED TO TRASH",
	catalog.ActionDelete: "PERMANENTLY DELETED",
	catalog.ActionMove:   "MOVED",
}

// describePlan states plainly what will happen, and fails before any prompt when the
// plan cannot be carried out as described (a trash on another device, no space).
func (a *app) describePlan(p actions.Plan, trasher *actions.Trasher) (int, []string, error) {
	head := fmt.Sprintf("%s files (%s) will be %s", ui.Count(len(p.Ops)), ui.Bytes(p.Bytes()), actionVerb[p.Kind])
	switch p.Kind {
	case catalog.ActionDelete:
		return ui.LevelDanger, []string{head,
			"They will NOT go to the trash. This cannot be undone.",
			fmt.Sprintf("One copy of each of the %s groups is kept.", ui.Count(countGroups(p))),
		}, nil
	case catalog.ActionTrash:
		perDir := map[string]int{}
		var order []string
		for _, op := range p.Ops {
			v := p.Volumes[op.File.VolumeKey]
			loc := trasher.Locate(v)
			if err := trasher.Check(op.File.AbsPath, loc); err != nil {
				return 0, nil, err
			}
			if perDir[loc.Dir] == 0 {
				order = append(order, loc.Dir)
			}
			perDir[loc.Dir]++
		}
		lines := []string{head}
		for _, d := range order {
			lines = append(lines, fmt.Sprintf("trash: %s (%s files, same drive)", d, ui.Count(perDir[d])))
		}
		lines = append(lines,
			fmt.Sprintf("One copy of each of the %s groups is kept.", ui.Count(countGroups(p))),
			"Restorable with mediacat undo. Space is freed only when the trash is emptied.")
		return ui.LevelWarn, lines, nil
	default:
		lines := []string{head + " to " + p.MoveTo}
		existing := p.MoveTo
		for !isDir(existing) {
			existing = filepath.Dir(existing)
		}
		same, cross := 0, int64(0)
		for _, op := range p.Ops {
			if ok, err := actions.SameDevice(op.File.AbsPath, existing); err == nil && ok {
				same++
			} else {
				cross += op.File.Size
			}
		}
		if same > 0 {
			lines = append(lines, fmt.Sprintf("%s files on the same drive: renamed in place.", ui.Count(same)))
		}
		if cross > 0 {
			free, err := actions.FreeBytes(existing)
			if err == nil && free < uint64(cross) {
				return 0, nil, fmt.Errorf("moving needs %s at %s but only %s is free", ui.Bytes(cross), p.MoveTo, ui.Bytes(int64(free)))
			}
			lines = append(lines, fmt.Sprintf("%s from another drive: copied, verified, then removed (%s free).", ui.Bytes(cross), ui.Bytes(int64(free))))
		}
		if strings.HasPrefix(p.MoveTo+"/", p.Root+"/") && !scan.ExcludedByDefault(p.MoveTo, p.Root) {
			lines = append(lines, "Note: the target is inside the searched directory, so moved copies will be found again.")
		}
		lines = append(lines, fmt.Sprintf("One copy of each of the %s groups is kept. Undo with mediacat undo.", ui.Count(countGroups(p))))
		return ui.LevelWarn, lines, nil
	}
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func countGroups(p actions.Plan) int {
	seen := map[string]bool{}
	for _, op := range p.Ops {
		seen[op.Keeper.AbsPath] = true
	}
	return len(seen)
}

func (a *app) printOutcome(kind string, p actions.Plan, out *actions.Outcome) {
	w := a.stdout
	past := map[string]string{catalog.ActionTrash: "moved to trash", catalog.ActionDelete: "permanently deleted", catalog.ActionMove: "moved to " + p.MoveTo}[kind]
	status := ui.OK.Render("Done:")
	if out.Interrupted {
		status = ui.Warn.Render("Interrupted:")
	}
	fmt.Fprintf(w, "\n%s %s files (%s) %s.  %s\n", status, ui.Count(out.Done), ui.Bytes(out.Bytes), past, ui.Dim.Render(fmt.Sprintf("batch #%d", out.BatchID)))
	for _, s := range []struct {
		label string
		list  []dupes.Failure
		st    interface{ Render(...string) string }
	}{{"skipped", out.Skipped, ui.Warn}, {"failed", out.Failed, ui.Err}} {
		if len(s.list) == 0 {
			continue
		}
		fmt.Fprintf(w, "%s\n", s.st.Render(fmt.Sprintf("  %s %d (left in place)", s.label, len(s.list))))
		for _, f := range s.list {
			fmt.Fprintf(w, "    %s  %s\n", f.Path, ui.Dim.Render(f.Err))
		}
	}
	if kind != catalog.ActionDelete && out.Done > 0 {
		fmt.Fprintf(w, "Undo with: %s\n", ui.Bold.Render(fmt.Sprintf("mediacat undo %d", out.BatchID)))
	}
	if kind == catalog.ActionTrash && out.Done > 0 {
		fmt.Fprintf(w, "%s\n", ui.Dim.Render("Space is freed when the trash is emptied: mediacat trash "+p.Root+" --empty"))
	}
	a.stashReminder(out.Paths)
}

func (a *app) stashReminder(paths []string) {
	libs, err := stash.Libraries(a.stashCfg)
	if err != nil || len(libs) == 0 {
		return
	}
	if n := len(stash.Affected(paths, libs)); n > 0 {
		fmt.Fprintf(a.stdout, "%s\n", ui.Warn.Render(fmt.Sprintf(
			"Stash: %d of these files are in Stash libraries. Run Tasks > Clean in Stash to drop its stale entries.", n)))
	}
}

type groupJSON struct {
	Hash        string         `json:"hash"`
	Size        int64          `json:"size"`
	Confirmed   bool           `json:"confirmed"`
	Reclaimable int64          `json:"reclaimable"`
	Keep        string         `json:"keep"`
	Files       []groupFileRow `json:"files"`
}

type groupFileRow struct {
	Path     string    `json:"path"`
	Role     string    `json:"role"`
	Modified time.Time `json:"modified"`
	Volume   string    `json:"volume"`
	RelPath  string    `json:"relpath"`
}

func toGroupJSON(gs []dupes.Group) []groupJSON {
	out := make([]groupJSON, 0, len(gs))
	for _, g := range gs {
		j := groupJSON{Hash: g.Key, Size: g.Size, Confirmed: g.Confirmed, Reclaimable: g.Reclaimable(), Keep: g.Files[g.Keep].AbsPath}
		for i, f := range g.Files {
			role := "duplicate"
			if i == g.Keep {
				role = "keep"
			}
			j.Files = append(j.Files, groupFileRow{f.AbsPath, role, time.Unix(0, f.MtimeNS).UTC(), f.VolumeKey, f.RelPath})
		}
		out = append(out, j)
	}
	return out
}

func writeGroupsJSON(w io.Writer, gs []dupes.Group) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(toGroupJSON(gs))
}

func exportGroups(path string, gs []dupes.Group) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".json":
		err = writeGroupsJSON(f, gs)
	case ".csv":
		cw := csv.NewWriter(f)
		cw.Write([]string{"group", "role", "size", "hash", "confirmed", "path", "modified", "volume", "relpath"})
		for i, g := range toGroupJSON(gs) {
			for _, r := range g.Files {
				cw.Write([]string{fmt.Sprint(i + 1), r.Role, fmt.Sprint(g.Size), g.Hash, fmt.Sprint(g.Confirmed), r.Path,
					r.Modified.Format(time.RFC3339), r.Volume, r.RelPath})
			}
		}
		cw.Flush()
		err = cw.Error()
	default:
		err = fmt.Errorf("export file must end in .csv or .json")
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
	}
	return err
}
