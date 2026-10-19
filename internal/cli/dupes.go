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
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"

	"github.com/njhoffman/mediacat/internal/actions"
	"github.com/njhoffman/mediacat/internal/catalog"
	"github.com/njhoffman/mediacat/internal/dupes"
	"github.com/njhoffman/mediacat/internal/filter"
	"github.com/njhoffman/mediacat/internal/phash"
	"github.com/njhoffman/mediacat/internal/review"
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
	paths     []string
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
	review    bool
	similar   string

	// Whether these flags were given, so --similar can pick its own defaults.
	depthSet, keepSet, workersSet bool
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
--similar[=LEVEL] finds videos and images that look alike instead: re-encodes, resizes,
upscales and format changes. LEVEL is exact, high (default), medium or low, or a number
of differing phash bits (0-32). Video phashes match Stash's, and are read from Stash's
database for files it has hashed; the rest are computed (about 25 frame decodes per
video) and saved. Similar groups keep the best quality copy by default (dupes.similar_keep),
and --delete is only available through --review.

--review opens an interactive list of the groups: choose the copy to keep and trash,
move, delete or leave each group (o opens a copy to check it). Choices are saved as you
go; applying asks for the same confirmations as below.

Actions are a dry run unless --execute is given. --trash only uses the trash on the
files' own drive; --move-to keeps the layout below path; --delete is permanent and
always asks you to type "delete".`,
		Example: `  mediacat dupes                                  # report duplicates here
  mediacat dupes /mnt/nfs/Stash --prefer /mnt/nfs/Stash/Sorted --trash
  mediacat dupes /mnt/nfs/Stash --review
  mediacat dupes /mnt/nfs/Stash --similar --review
  mediacat dupes ~/Pictures --similar=medium --type image
  mediacat dupes --trash --execute
  mediacat dupes --move-to /mnt/nfs/trashbox --execute --yes
  mediacat dupes --export dupes.csv`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			fl := cmd.Flags()
			df.depthSet, df.keepSet, df.workersSet = fl.Changed("depth"), fl.Changed("keep"), fl.Changed("workers")
			return a.runDupes(dir, df)
		},
	}
	f := cmd.Flags()
	f.BoolVar(&df.all, "all", false, "search every mounted volume instead of path")
	d := a.cfg.Dupes
	f.StringVar(&df.depth, "depth", d.Depth, "size, partial or full")
	f.StringVarP(&df.kind, "type", "t", "", "only video, audio or image")
	f.StringVar(&df.minSize, "min-size", d.MinSize, "ignore files smaller than this, e.g. 1M")
	f.StringVar(&df.keep, "keep", d.Keep, "rules choosing the copy to keep, in order")
	f.StringSliceVar(&df.prefer, "prefer", d.Prefer, "keep copies under this directory (repeatable, first wins)")
	f.StringSliceVar(&df.avoid, "avoid", d.Avoid, "avoid keeping copies under this directory (repeatable)")
	f.StringSliceVar(&df.paths, "prefer-path", d.PreferredPaths, `"paths" rule: prefer copies whose path contains this; "^x" avoids x (repeatable, case-sensitive)`)
	f.BoolVar(&df.trash, "trash", false, "move extra copies to the trash on their own drive")
	f.BoolVar(&df.del, "delete", false, "permanently delete extra copies")
	f.StringVar(&df.moveTo, "move-to", "", "move extra copies under this directory")
	f.BoolVar(&df.execute, "execute", false, "apply the action (otherwise a dry run)")
	f.BoolVarP(&df.yes, "yes", "y", false, "skip the confirmation for --trash and --move-to (never for --delete)")
	f.BoolVar(&df.asJSON, "json", false, "print groups as JSON")
	f.StringVar(&df.export, "export", "", "write all groups to a .csv or .json file")
	f.IntVarP(&df.limit, "limit", "n", d.Limit, "groups to show (0 for all)")
	f.IntVarP(&df.workers, "workers", "j", d.Workers, "parallel hashing (0: 2 network, 1 spinning disk, 4 otherwise)")
	f.BoolVar(&df.showFiles, "dry-run-files", false, "list every file the action would touch")
	f.BoolVarP(&df.review, "review", "r", false, "choose the copy to keep and the action per group interactively")
	f.StringVar(&df.similar, "similar", "", "find similar videos and images: exact, high, medium, low or a distance")
	f.Lookup("similar").NoOptDefVal = d.Similar
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
	similar := df.similar != ""
	distance := 0
	if similar {
		if distance, err = dupes.ParseLevel(df.similar); err != nil {
			return err
		}
		switch {
		case df.depthSet:
			return errors.New("--similar compares perceptual hashes; it cannot be combined with --depth")
		case kind == catalog.ActionDelete && !df.review:
			return errors.New("similar files are not identical, so --delete is only available in --review, where you check each group (or use --trash, which can be undone)")
		case strings.EqualFold(df.kind, "audio"):
			return errors.New("--similar compares videos and images; audio is not supported")
		}
		depth = dupes.DepthFull
		if !df.keepSet {
			df.keep = a.cfg.Dupes.SimilarKeep
		}
	}
	if kind != "" && depth != dupes.DepthFull {
		return errors.New("actions need --depth full: smaller depths only find possible duplicates")
	}
	if df.asJSON && kind != "" {
		return errors.New("--json reports only; drop it to run an action")
	}
	if df.review {
		switch {
		case depth != dupes.DepthFull:
			return errors.New("--review needs --depth full")
		case df.asJSON || df.execute:
			return errors.New("--review cannot be combined with --json or --execute; it asks for confirmation before applying")
		case a.interactive == nil || !a.interactive():
			return errors.New("--review needs an interactive terminal")
		}
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
	if err := policy.SetPathPatterns(df.paths); err != nil {
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
		progress := func(e dupes.Event) {
			report(ui.Update{Label: e.Stage, Done: e.Done, Total: e.Total, Bytes: e.Bytes, TotalBytes: e.TotalBytes, Current: e.Current})
		}
		var err error
		if similar {
			res, err = a.findSimilar(ctx, files, distance, policy, df, progress)
			return err
		}
		finder := &dupes.Finder{DB: a.db, Options: dupes.Options{Depth: depth, Workers: workers, Policy: policy, Progress: progress}}
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
	if df.review {
		if root == "" {
			root = "/"
		}
		return a.runReview(ctx, kind, root, df, res, vols)
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
		if similar {
			fmt.Fprintf(a.stdout, "\n%s\n", ui.Dim.Render("Check groups with --review (o opens a copy), or add --trash or --move-to DIR (a dry run until --execute)."))
			return nil
		}
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
	_, err = a.act(ctx, kind, root, df, res.Groups, vols)
	return err
}

// findSimilar runs the perceptual search, reusing Stash's video phashes where Stash's
// record still matches the file.
func (a *app) findSimilar(ctx context.Context, files []catalog.File, distance int, policy dupes.Policy, df dupesFlags, progress func(dupes.Event)) (*dupes.Result, error) {
	d := a.cfg.Dupes
	workers := d.PHashWorkers
	if df.workersSet {
		workers = df.workers
	}
	if workers <= 0 {
		workers = max(runtime.NumCPU()/2, 1)
	}
	o := dupes.SimilarOptions{Distance: distance, DurationPct: d.SimilarDurPct, DurationMin: a.cfg.SimilarDurationMin,
		MinImageSize: d.MinImageSize, Workers: workers, Policy: policy, Progress: progress,
		Hasher: phash.Hasher{MinSize: d.MinImageSize}}
	if prints := a.stashPHashes(); len(prints) > 0 {
		o.Stash = func(f catalog.File) (uint64, bool) {
			fp, ok := prints[f.AbsPath]
			if !ok || !fp.Matches(f.Size, f.MtimeNS) {
				return 0, false
			}
			return fp.PHash, true
		}
	}
	return (&dupes.SimilarFinder{DB: a.db, Options: o}).Find(ctx, files)
}

// stashPHashes reads Stash's video phashes, read-only. Without a Stash install it returns
// nothing; a database that cannot be read is reported and skipped.
func (a *app) stashPHashes() map[string]stash.Fingerprint {
	db, err := stash.Database(a.stashCfg)
	if err != nil || db == "" {
		return nil
	}
	if _, err := os.Stat(db); err != nil {
		return nil
	}
	prints, err := stash.PHashes(db)
	if err != nil {
		a.warn("not using Stash's phashes: " + err.Error())
		return nil
	}
	return prints
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
	title, detail := "Duplicates", fmt.Sprintf("(%s; %s candidates, %s hashed now)", label, ui.Count(res.Candidates), ui.Count(res.Hashed))
	if res.Similar {
		title = "Similar files"
		detail = fmt.Sprintf("(%s; %s videos and images: %s phashes from Stash, %s computed now)",
			dupes.LevelName(res.Distance), ui.Count(res.Candidates), ui.Count(res.FromStash), ui.Count(res.Hashed))
	}
	fmt.Fprintf(w, "%s  %s  %s\n", ui.Title.Render(title), ui.Bold.Render(where), ui.Dim.Render(detail))
	switch {
	case len(res.Groups) == 0 && res.Similar:
		fmt.Fprintln(w, ui.OK.Render("  No similar files found."))
	case len(res.Groups) == 0:
		fmt.Fprintln(w, ui.OK.Render("  No duplicates found."))
	case res.Similar:
		fmt.Fprintf(w, "  %s groups, %s files, %s extra copies, %s reclaimable  %s\n",
			ui.Bold.Render(ui.Count(len(res.Groups))), ui.Count(files), ui.Count(extra), ui.Bold.Render(ui.Bytes(reclaim)),
			ui.Warn.Render("similar, not identical: check before removing"))
	default:
		fmt.Fprintf(w, "  %s groups, %s files, %s extra copies, %s reclaimable\n",
			ui.Bold.Render(ui.Count(len(res.Groups))), ui.Count(files), ui.Count(extra), ui.Bold.Render(ui.Bytes(reclaim)))
	}
	width := ui.Width(w)
	for i, g := range res.Groups {
		if limit > 0 && i == limit {
			fmt.Fprintf(w, "\n%s\n", ui.Dim.Render(fmt.Sprintf("%d more groups not shown (--limit 0 shows all)", len(res.Groups)-limit)))
			break
		}
		if g.Similar {
			a.printSimilarGroup(i, g, width)
			continue
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
	// Each note is followed by the files it covers, when there are details to show.
	notes := []struct {
		n     int
		text  string
		st    interface{ Render(...string) string }
		files []dupes.Failure
	}{
		{len(res.Changed), "files changed since the last scan were skipped; run mediacat scan first", ui.Warn, nil},
		{len(res.Deferred), "files had network errors and were skipped; run again to retry", ui.Warn, res.Deferred},
		{len(res.Failed), "files could not be read", ui.Err, res.Failed},
		{len(res.Unhashable), "videos and images cannot be phashed (no frames decode); see mediacat info", ui.Dim, res.Unhashable},
		{res.SmallImages, fmt.Sprintf("images smaller than %dpx were not compared", a.cfg.Dupes.MinImageSize), ui.Dim, nil},
		{res.Skipped, "audio files are not compared for similarity", ui.Dim, nil},
		{res.Unavailable, "cataloged files are on volumes that are not mounted", ui.Dim, nil},
		{res.Hardlinks, "hard links to files already counted were ignored", ui.Dim, nil},
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
		for _, f := range n.files {
			fmt.Fprintf(w, "    %s  %s\n", f.Path, ui.Dim.Render(f.Err))
		}
	}
}

// printSimilarGroup shows a similar group with the qualities that tell its copies apart.
func (a *app) printSimilarGroup(i int, g dupes.Group, width int) {
	w := a.stdout
	maxDist := 0
	for j := range g.Files {
		maxDist = max(maxDist, g.Distance(j, g.Keep))
	}
	fmt.Fprintf(w, "\n%s  %s  %s  %s\n", ui.Accent.Render(fmt.Sprintf("#%d", i+1)),
		fmt.Sprintf("%d similar files", len(g.Files)), ui.Bold.Render(ui.Bytes(g.Reclaimable())+" reclaimable"),
		ui.Dim.Render(fmt.Sprintf("up to %d bits apart", maxDist)))
	for j, f := range g.Files {
		tag, st := "dupe", ui.Warn
		if j == g.Keep {
			tag, st = "keep", ui.OK
		}
		m := f.Meta
		info := fmt.Sprintf("%9s  %9s  %8s  %9s  %-9s", ui.Bytes(f.Size), ui.Res(m.Width, m.Height),
			ui.Duration(m.DurationMS), ui.Bitrate(m.VideoBitrate), review.Likeness(g, j, g.Keep))
		if f.Kind == "image" {
			info = fmt.Sprintf("%9s  %9s  %8s  %9s  %-9s", ui.Bytes(f.Size), ui.Res(m.Width, m.Height), "", "", review.Likeness(g, j, g.Keep))
		}
		prefix := fmt.Sprintf("  %s  %s  ", st.Render(tag), ui.Dim.Render(info))
		line := f.AbsPath
		if width > 0 {
			line = ui.TruncateMiddle(line, max(width-lipgloss.Width(prefix), 20))
		}
		fmt.Fprintln(w, prefix+line)
	}
}

// act builds the plan, shows exactly what will happen, asks, and runs it. applied reports
// whether anything was executed (false for dry runs and declined prompts).
func (a *app) act(ctx context.Context, kind, root string, df dupesFlags, groups []dupes.Group, vols map[string]volume.Volume) (applied bool, err error) {
	moveTo := ""
	if kind == catalog.ActionMove {
		abs, err := filepath.Abs(df.moveTo)
		if err != nil {
			return false, err
		}
		moveTo = abs
	}
	plan, err := actions.BuildPlan(kind, root, moveTo, groups, vols)
	if err != nil {
		return false, err
	}
	if len(plan.Ops) == 0 {
		fmt.Fprintln(a.stdout, "\nNothing to do.")
		return false, nil
	}
	trasher := actions.NewTrasher(a.res)
	level, lines, err := a.describePlan(plan, trasher)
	if err != nil {
		return false, err
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
		return false, nil
	}
	fmt.Fprintf(a.stderr, "\n%s\n", ui.Banner(level, lines...))
	switch {
	case kind == catalog.ActionDelete:
		ok, err := a.confirm(ui.Err.Render(`Type "delete" to permanently delete these files: `), "delete")
		if err != nil {
			return false, fmt.Errorf("permanent deletion must be confirmed in a terminal: %w", err)
		}
		if !ok {
			fmt.Fprintln(a.stderr, "Cancelled; nothing was deleted.")
			return false, nil
		}
	case !df.yes:
		ok, err := a.confirm("Proceed? [y/N] ", "")
		if err != nil {
			return false, fmt.Errorf("%w (or pass --yes)", err)
		}
		if !ok {
			fmt.Fprintln(a.stderr, "Cancelled; nothing was changed.")
			return false, nil
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
		return out != nil && out.Done > 0, err
	}
	if out.Interrupted {
		return out.Done > 0, errInterrupted{}
	}
	return true, nil
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
	var similar []string
	if p.Similar {
		similar = []string{"These are SIMILAR files, not identical copies: each group keeps the copy marked keep."}
	}
	switch p.Kind {
	case catalog.ActionDelete:
		return ui.LevelDanger, append(append([]string{head}, similar...),
			"They will NOT go to the trash. This cannot be undone.",
			fmt.Sprintf("One copy of each of the %s groups is kept.", ui.Count(countGroups(p))),
		), nil
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
		lines := append([]string{head}, similar...)
		for _, d := range order {
			lines = append(lines, fmt.Sprintf("trash: %s (%s files, same drive)", d, ui.Count(perDir[d])))
		}
		lines = append(lines,
			fmt.Sprintf("One copy of each of the %s groups is kept.", ui.Count(countGroups(p))),
			"Restorable with mediacat undo. Space is freed only when the trash is emptied.")
		return ui.LevelWarn, lines, nil
	default:
		lines := append([]string{head + " to " + p.MoveTo}, similar...)
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
		if strings.HasPrefix(p.MoveTo+"/", p.Root+"/") && !scan.Excluded(p.MoveTo, p.Root, a.cfg.Scan.Exclude) {
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
		fmt.Fprintf(w, "%s\n", ui.Dim.Render("Space is freed when the trash is emptied: mediacat trash "+ui.Quote(p.Root)+" --empty"))
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
	Similar     bool           `json:"similar,omitempty"`
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
	// Similar groups only.
	Size      int64  `json:"size,omitempty"`
	PHash     string `json:"phash,omitempty"`
	Distance  *int   `json:"distance,omitempty"` // phash bits from the kept copy
	Identical bool   `json:"identical,omitempty"`
}

func toGroupJSON(gs []dupes.Group) []groupJSON {
	out := make([]groupJSON, 0, len(gs))
	for _, g := range gs {
		j := groupJSON{Hash: g.Key, Size: g.Size, Confirmed: g.Confirmed, Similar: g.Similar, Reclaimable: g.Reclaimable(), Keep: g.Files[g.Keep].AbsPath}
		for i, f := range g.Files {
			role := "duplicate"
			if i == g.Keep {
				role = "keep"
			}
			r := groupFileRow{Path: f.AbsPath, Role: role, Modified: time.Unix(0, f.MtimeNS).UTC(), Volume: f.VolumeKey, RelPath: f.RelPath}
			if g.Similar {
				d := g.Distance(i, g.Keep)
				r.Size, r.PHash, r.Distance, r.Identical = f.Size, fmt.Sprintf("%016x", f.PHash.Value), &d, i != g.Keep && g.Identical(i, g.Keep)
			}
			j.Files = append(j.Files, r)
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
		cw.Write([]string{"group", "role", "size", "hash", "confirmed", "path", "modified", "volume", "relpath", "similar", "phash", "distance"})
		for i, g := range toGroupJSON(gs) {
			for _, r := range g.Files {
				size, dist := g.Size, ""
				if g.Similar {
					size, dist = r.Size, fmt.Sprint(*r.Distance)
				}
				cw.Write([]string{fmt.Sprint(i + 1), r.Role, fmt.Sprint(size), g.Hash, fmt.Sprint(g.Confirmed), r.Path,
					r.Modified.Format(time.RFC3339), r.Volume, r.RelPath, fmt.Sprint(g.Similar), r.PHash, dist})
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
