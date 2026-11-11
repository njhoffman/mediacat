package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/njhoffman/mediacat/internal/actions"
	"github.com/njhoffman/mediacat/internal/catalog"
	"github.com/njhoffman/mediacat/internal/dupes"
	"github.com/njhoffman/mediacat/internal/filter"
	"github.com/njhoffman/mediacat/internal/stash"
	"github.com/njhoffman/mediacat/internal/ui"
	"github.com/njhoffman/mediacat/internal/volume"
)

type copyFlags struct {
	to        string
	mapFile   string
	minFree   string
	kind      string
	minSize   string
	execute   bool
	yes       bool
	noVerify  bool
	workers   int
	showFiles bool
	limit     int
}

func (a *app) copyCmd() *cobra.Command {
	var cf copyFlags
	cmd := &cobra.Command{
		Use:   "copy <source> --to <dir>",
		Short: "Copy media that has no identical copy on another drive",
		Long: `Copy the cataloged media under source whose content has no identical copy anywhere
on the drive holding --to (compared by full hash, as dupes does), keeping each file's path
below source under --to. Both drives must have been scanned; only files whose size matches
something on the target are hashed, since any other file cannot have a copy there.

--map FILE lays files out by rules instead: one "from<TAB>to" line per rule, where from
is a directory under source (relative to it, or absolute) and to a directory (relative
to --to, or absolute). The longest matching rule wins; other files keep their path.

Existing files are never overwritten: name clashes are listed and skipped. Copying stops
before free space on the target would drop below --min-free. Each copy is written under
a temporary name, flushed, read back from the drive and compared, then renamed into
place with the original's modification time, and cataloged, so running the command
again continues where it stopped.

A dry run until --execute; --yes skips the confirmation. Undo a batch with mediacat
undo, which removes copies only while their originals are still in place.`,
		Example: `  mediacat copy /media/nicholas/wd/Performers --to /mnt/nfs/Stash/Incoming/wd
  mediacat copy /media/nicholas/wd --to /mnt/nfs/Stash/Incoming --map wd-layout.tsv --execute
  mediacat copy /media/nicholas/wd --to /mnt/nfs/Stash/Incoming --min-free 50G --dry-run-files`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error { return a.runCopy(args[0], cf) },
	}
	f := cmd.Flags()
	f.StringVar(&cf.to, "to", "", "destination directory (required); its drive is searched for existing copies")
	f.StringVar(&cf.mapFile, "map", "", "TSV of from<TAB>to directory rules for the layout")
	f.StringVar(&cf.minFree, "min-free", "10G", "stop before free space on the target drops below this")
	f.StringVarP(&cf.kind, "type", "t", "", "only video, audio or image")
	f.StringVar(&cf.minSize, "min-size", "1", "ignore files smaller than this, e.g. 1M")
	f.BoolVar(&cf.execute, "execute", false, "copy (otherwise a dry run)")
	f.BoolVarP(&cf.yes, "yes", "y", false, "skip the confirmation")
	f.BoolVar(&cf.noVerify, "no-verify", false, "skip reading each copy back from the drive")
	f.IntVarP(&cf.workers, "workers", "j", 0, "parallel hashing (0: 2 network, 1 spinning disk, 4 otherwise)")
	f.BoolVar(&cf.showFiles, "dry-run-files", false, "list every file that would be copied")
	f.IntVarP(&cf.limit, "limit", "n", 20, "files and clashes to list (0 for all)")
	cmd.MarkFlagRequired("to")
	return cmd
}

// copyRule maps files under from to the directory to.
type copyRule struct{ from, to string }

// parseCopyMap reads from<TAB>to rules; relative paths are under source and dest.
func parseCopyMap(path, source, dest string) ([]copyRule, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var rules []copyRule
	sc := bufio.NewScanner(f)
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimRight(sc.Text(), "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
			return nil, fmt.Errorf("%s line %d: want from<TAB>to", path, n)
		}
		from, to := filepath.Clean(parts[0]), filepath.Clean(parts[1])
		if !filepath.IsAbs(from) {
			from = filepath.Join(source, from)
		}
		if !filepath.IsAbs(to) {
			to = filepath.Join(dest, to)
		}
		if !inDir(from, source) {
			return nil, fmt.Errorf("%s line %d: %s is not under %s", path, n, from, source)
		}
		rules = append(rules, copyRule{from, to})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(rules, func(i, j int) bool { return len(rules[i].from) > len(rules[j].from) })
	return rules, nil
}

// copyDest is where src goes: under the longest matching rule, else its path below source
// under dest.
func copyDest(src, source, dest string, rules []copyRule) string {
	for _, r := range rules {
		if inDir(src, r.from) {
			rel, _ := filepath.Rel(r.from, src)
			return filepath.Join(r.to, rel)
		}
	}
	rel, _ := filepath.Rel(source, src)
	return filepath.Join(dest, rel)
}

type copyItem struct {
	src  catalog.File
	dest string
}

// uniqueFiles returns the source files whose content has no copy on the target, given the
// groups found among the candidates. Source files outside every group are unique.
func uniqueFiles(src []catalog.File, groups []dupes.Group, targetKey string, skip map[string]bool) (unique []catalog.File, present int, presentBytes int64) {
	hasCopy := map[string]bool{}
	for _, g := range groups {
		onTarget := false
		for _, f := range g.Files {
			if f.VolumeKey == targetKey {
				onTarget = true
				break
			}
		}
		if !onTarget {
			continue
		}
		for _, f := range g.Files {
			if f.VolumeKey != targetKey {
				hasCopy[f.AbsPath] = true
			}
		}
	}
	for _, f := range src {
		switch {
		case skip[f.AbsPath]:
		case hasCopy[f.AbsPath]:
			present++
			presentBytes += f.Size
		default:
			unique = append(unique, f)
		}
	}
	return unique, present, presentBytes
}

// existingDir is the nearest existing ancestor of p (p itself when it exists).
func existingDir(p string) string {
	for {
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			return p
		}
		parent := filepath.Dir(p)
		if parent == p {
			return p
		}
		p = parent
	}
}

func (a *app) runCopy(source string, cf copyFlags) error {
	minFree, err := filter.ParseSize(cf.minFree)
	if err != nil {
		return fmt.Errorf("--min-free: %w", err)
	}
	minSize, err := filter.ParseSize(cf.minSize)
	if err != nil {
		return err
	}
	flt := filter.Filter{Kind: strings.ToLower(cf.kind), MinSize: max(minSize, 1)}
	if err := flt.Validate(); err != nil {
		return err
	}
	dest, err := filepath.Abs(cf.to)
	if err != nil {
		return err
	}
	if err := a.open(); err != nil {
		return err
	}

	sv, srel, err := a.res.Resolve(source)
	if err != nil {
		return err
	}
	sroot, _ := sv.AbsPath(srel)
	if real, err := filepath.EvalSymlinks(existingDir(dest)); err == nil {
		dest = filepath.Join(real, strings.TrimPrefix(dest, existingDir(dest)))
	}
	if inDir(dest, sroot) {
		return fmt.Errorf("--to %s is inside the source %s", dest, sroot)
	}
	tv, _, err := a.res.Resolve(existingDir(dest))
	if err != nil {
		return err
	}
	if tv.Key == sv.Key {
		return errors.New("source and --to are on the same drive; copy finds files missing from another drive (use dupes for one drive)")
	}
	sStored, err := a.db.VolumeByKey(sv.Key)
	if err != nil {
		return err
	}
	if sStored == nil {
		return fmt.Errorf("%s has not been scanned: run mediacat scan %s", sroot, ui.Quote(sroot))
	}
	tStored, err := a.db.VolumeByKey(tv.Key)
	if err != nil {
		return err
	}
	if tStored == nil {
		return fmt.Errorf("the target drive has not been scanned, so its copies are unknown: run mediacat scan %s", ui.Quote(tv.MountPoint))
	}
	if err := a.staleness(sStored.ID, srel, sroot); err != nil {
		return err
	}
	if err := a.staleness(tStored.ID, "", tv.MountPoint); err != nil {
		return err
	}
	var rules []copyRule
	if cf.mapFile != "" {
		if rules, err = parseCopyMap(cf.mapFile, sroot, dest); err != nil {
			return err
		}
	}

	srcFiles, err := a.db.Query(sStored.ID, srel, flt)
	if err != nil {
		return err
	}
	tgtFiles, err := a.db.Query(tStored.ID, "", filter.Filter{MinSize: 1})
	if err != nil {
		return err
	}
	all := append(append([]catalog.File(nil), srcFiles...), tgtFiles...)
	a.mountedOnly(all)
	srcFiles, tgtFiles = all[:len(srcFiles)], all[len(srcFiles):]

	// Only sizes present on both sides can hold copies; hash just those.
	tgtSizes, srcSizes := map[int64]bool{}, map[int64]bool{}
	for _, f := range tgtFiles {
		tgtSizes[f.Size] = true
	}
	for _, f := range srcFiles {
		srcSizes[f.Size] = true
	}
	var candidates []catalog.File
	for _, f := range all {
		if tgtSizes[f.Size] && srcSizes[f.Size] {
			candidates = append(candidates, f)
		}
	}
	workers := cf.workers
	if workers == 0 {
		switch {
		case sv.Network || tv.Network:
			workers = 2
		case sv.Rotational || tv.Rotational:
			workers = 1
		default:
			workers = 4
		}
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	pol, _ := dupes.ParsePolicy(dupes.DefaultPolicy, nil, nil)
	var res *dupes.Result
	prog := ui.Progress{Out: a.stderr, Cancel: cancel}
	err = prog.Run(ctx, func(report func(ui.Update)) error {
		progress := func(e dupes.Event) {
			report(ui.Update{Label: e.Stage, Done: e.Done, Total: e.Total, Bytes: e.Bytes, TotalBytes: e.TotalBytes, Current: e.Current})
		}
		var err error
		res, err = (&dupes.Finder{DB: a.db, Options: dupes.Options{Depth: dupes.DepthFull, Workers: workers, Policy: pol, Progress: progress}}).Find(ctx, candidates)
		return err
	})
	if err != nil {
		return err
	}
	if res.Interrupted {
		fmt.Fprintf(a.stdout, "%s Hashes computed so far are saved; run the same command again to continue.\n", ui.Warn.Render("Interrupted."))
		return errInterrupted{}
	}

	// Files whose content could not be confirmed are not copied this run.
	skip := map[string]bool{}
	for _, p := range res.Changed {
		skip[p] = true
	}
	for _, fl := range append(append([]dupes.Failure(nil), res.Deferred...), res.Failed...) {
		skip[fl.Path] = true
	}
	unmounted := 0
	var mounted []catalog.File
	for _, f := range srcFiles {
		if f.AbsPath == "" {
			unmounted++
			continue
		}
		mounted = append(mounted, f)
	}
	unique, present, presentBytes := uniqueFiles(mounted, res.Groups, tv.Key, skip)

	var items, clashes []copyItem
	taken := map[string]bool{}
	for _, f := range unique {
		it := copyItem{src: f, dest: copyDest(f.AbsPath, sroot, dest, rules)}
		if _, err := os.Lstat(it.dest); err == nil || taken[it.dest] {
			clashes = append(clashes, it)
			continue
		}
		taken[it.dest] = true
		items = append(items, it)
	}
	var total int64
	for _, it := range items {
		total += it.src.Size
	}
	free, _ := actions.FreeBytes(existingDir(dest))
	fits, fitBytes := 0, int64(0)
	for _, it := range items {
		if int64(free)-fitBytes-it.src.Size < minFree {
			break
		}
		fits++
		fitBytes += it.src.Size
	}

	w := a.stdout
	fmt.Fprintf(w, "%s  %s  %s %s\n", ui.Title.Render("Copy"), ui.Bold.Render(sroot), ui.Dim.Render("->"), ui.Bold.Render(dest))
	fmt.Fprintf(w, "  %-28s %s files, %s\n", "with no copy on the target", ui.Count(len(items)), ui.Bytes(total))
	fmt.Fprintf(w, "  %-28s %s files, %s\n", "already on the target", ui.Count(present), ui.Bytes(presentBytes))
	if len(clashes) > 0 {
		fmt.Fprintf(w, "  %-28s %s\n", "name clashes (skipped)", ui.Warn.Render(ui.Count(len(clashes))))
	}
	fmt.Fprintf(w, "  %-28s %s (keeping %s free)\n", "free on the target", ui.Bytes(int64(free)), ui.Bytes(minFree))
	for _, n := range []struct {
		n    int
		text string
	}{
		{len(res.Changed), "files changed since the last scan were not copied; run mediacat scan first"},
		{len(res.Deferred) + len(res.Failed), "files could not be read and were not copied; run again to retry"},
		{unmounted, "cataloged files are on a volume that is not mounted"},
	} {
		if n.n > 0 {
			fmt.Fprintln(w, ui.Warn.Render(fmt.Sprintf("  %s %s", ui.Count(n.n), n.text)))
		}
	}
	listed := func(label string, list []copyItem, all bool) {
		if len(list) == 0 {
			return
		}
		fmt.Fprintf(w, "\n  %s\n", ui.Bold.Render(label))
		for i, it := range list {
			if !all && cf.limit > 0 && i == cf.limit {
				fmt.Fprintf(w, "%s\n", ui.Dim.Render(fmt.Sprintf("    %d more (--limit 0 or --dry-run-files lists all)", len(list)-i)))
				break
			}
			fmt.Fprintf(w, "    %s  %s\n", it.src.AbsPath, ui.Dim.Render("-> "+it.dest))
		}
	}
	listed("Name clashes: the destination exists with other content", clashes, false)
	if len(items) == 0 {
		fmt.Fprintln(w, "\nNothing to copy.")
		return nil
	}
	if fits < len(items) {
		fmt.Fprintln(w, ui.Warn.Render(fmt.Sprintf("\n  Only %s of %s files (%s) fit before free space drops below %s; copying stops there.",
			ui.Count(fits), ui.Count(len(items)), ui.Bytes(fitBytes), ui.Bytes(minFree))))
		if fits == 0 {
			return nil
		}
	}
	if !cf.execute {
		listed("Would copy", items, cf.showFiles)
		fmt.Fprintf(w, "\n%s\n", ui.Dim.Render("DRY RUN: nothing was copied. Add --execute to copy."))
		return nil
	}
	fmt.Fprintf(a.stderr, "\n%s\n", ui.Banner(ui.LevelWarn,
		fmt.Sprintf("%s files (%s) will be COPIED to %s", ui.Count(fits), ui.Bytes(fitBytes), dest),
		"Nothing is overwritten; originals are not changed. Undo with mediacat undo."))
	if !cf.yes {
		ok, err := a.confirm("Proceed? [y/N] ", "")
		if err != nil {
			return fmt.Errorf("%w (or pass --yes)", err)
		}
		if !ok {
			fmt.Fprintln(a.stderr, "Cancelled; nothing was copied.")
			return nil
		}
	}
	return a.executeCopy(ctx, sroot, dest, items[:fits], tv, tStored.ID, minFree, !cf.noVerify)
}

func (a *app) executeCopy(ctx context.Context, sroot, dest string, items []copyItem, tv volume.Volume, tvolID int64, minFree int64, verify bool) error {
	batch, err := a.db.StartBatch(catalog.ActionCopy, sroot, dest, a.now())
	if err != nil {
		return err
	}
	var total int64
	for _, it := range items {
		total += it.src.Size
	}
	out := &actions.Outcome{BatchID: batch}
	stoppedForSpace := false
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	prog := ui.Progress{Out: a.stderr, Cancel: cancel}
	err = prog.Run(ctx, func(report func(ui.Update)) error {
		var doneBytes int64
		for i, it := range items {
			if ctx.Err() != nil {
				out.Interrupted = true
				return nil
			}
			if free, err := actions.FreeBytes(existingDir(filepath.Dir(it.dest))); err == nil && int64(free)-it.src.Size < minFree {
				stoppedForSpace = true
				return nil
			}
			report(ui.Update{Label: "Copying", Done: i, Total: len(items), Bytes: doneBytes, TotalBytes: total, Current: it.src.AbsPath})
			act := catalog.Action{BatchID: batch, VolumeKey: tv.Key, SourcePath: it.src.AbsPath, Size: it.src.Size,
				DestPath: it.dest, Status: catalog.ActionDone}
			base := doneBytes
			sum, err := actions.CopyFile(ctx, it.src.AbsPath, it.dest, it.src.FullHash, verify, func(n int64) {
				report(ui.Update{Label: "Copying", Done: i, Total: len(items), Bytes: base + n, TotalBytes: total, Current: it.src.AbsPath})
			})
			var copied catalog.File
			if err == nil {
				copied, err = a.copiedFile(it, tv, tvolID)
			}
			act.At, act.FullHash, act.RelPath = a.now(), sum, copied.RelPath
			switch {
			case errors.Is(err, context.Canceled):
				out.Interrupted = true
				return nil
			case err != nil:
				act.Status, act.Error = catalog.ActionFailed, err.Error()
				out.Failed = append(out.Failed, dupes.Failure{Path: it.src.AbsPath, Err: err.Error()})
			default:
				out.Done++
				out.Bytes += it.src.Size
				out.Paths = append(out.Paths, it.dest)
			}
			if err := a.db.RecordCopy(act, copied, sum); err != nil {
				return fmt.Errorf("recording the copy of %s (the file was copied): %w", it.src.AbsPath, err)
			}
			doneBytes += it.src.Size
		}
		return nil
	})
	status := catalog.ScanComplete
	if out.Interrupted || stoppedForSpace {
		status = catalog.ScanInterrupted
	}
	if ferr := a.db.FinishBatch(batch, status, a.now()); err == nil {
		err = ferr
	}
	w := a.stdout
	head := ui.OK.Render("Done:")
	if out.Interrupted {
		head = ui.Warn.Render("Interrupted:")
	}
	fmt.Fprintf(w, "\n%s %s files (%s) copied to %s.  %s\n", head, ui.Count(out.Done), ui.Bytes(out.Bytes), dest, ui.Dim.Render(fmt.Sprintf("batch #%d", batch)))
	if stoppedForSpace {
		fmt.Fprintln(w, ui.Warn.Render("  Stopped before free space on the target dropped below the margin (--min-free)."))
	}
	if len(out.Failed) > 0 {
		fmt.Fprintln(w, ui.Err.Render(fmt.Sprintf("  failed %d (nothing left at the destination)", len(out.Failed))))
		for _, f := range out.Failed {
			fmt.Fprintf(w, "    %s  %s\n", f.Path, ui.Dim.Render(f.Err))
		}
	}
	if out.Done > 0 {
		fmt.Fprintf(w, "Undo with: %s\n", ui.Bold.Render(fmt.Sprintf("mediacat undo %d", batch)))
	}
	if libs, lerr := stash.Libraries(a.stashCfg); lerr == nil {
		if n := len(stash.Affected(out.Paths, libs)); n > 0 {
			fmt.Fprintln(w, ui.Warn.Render(fmt.Sprintf("Stash: %d of the copies are in Stash libraries. Run Tasks > Scan in Stash to add them.", n)))
		}
	}
	if err != nil {
		return err
	}
	if out.Interrupted {
		fmt.Fprintln(w, "Run the same command again to copy the rest.")
		return errInterrupted{}
	}
	return nil
}

// copiedFile is the catalog entry for a new copy: the source's metadata (with its probe
// JSON) at the copy's volume, path and inode.
func (a *app) copiedFile(it copyItem, tv volume.Volume, tvolID int64) (catalog.File, error) {
	v, rel, err := a.res.Resolve(it.dest)
	if err != nil {
		return catalog.File{}, err
	}
	if v.Key != tv.Key {
		return catalog.File{}, fmt.Errorf("%s ended up on %s, not the target drive", it.dest, v.Key)
	}
	f := it.src
	if full, err := a.db.GetFile(it.src.VolumeID, it.src.RelPath); err == nil && full != nil {
		f = *full
	}
	st, err := os.Stat(it.dest)
	if err != nil {
		return catalog.File{}, err
	}
	f.ID, f.VolumeID, f.RelPath, f.VolumeKey, f.AbsPath = 0, tvolID, rel, tv.Key, it.dest
	f.Inode, f.MtimeNS, f.ScannedAt, f.MissingSince = inodeOf(st), st.ModTime().UnixNano(), a.now(), nil
	return f, nil
}

func inodeOf(st os.FileInfo) uint64 {
	if s, ok := st.Sys().(*syscall.Stat_t); ok {
		return s.Ino
	}
	return 0
}
