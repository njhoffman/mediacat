package cli

import (
	"fmt"
	"strings"

	"github.com/njhoffman/mediacat/internal/catalog"
	"github.com/njhoffman/mediacat/internal/filter"
	"github.com/njhoffman/mediacat/internal/ui"
	"github.com/njhoffman/mediacat/internal/volume"
)

func (a *app) dashboard() error {
	if err := a.open(); err != nil {
		return err
	}
	w := a.stdout
	now := a.now()
	fmt.Fprintf(w, "%s  %s\n\n", ui.Title.Render("mediacat"), ui.Dim.Render("catalog "+a.db.Path))

	vols, err := a.db.Volumes()
	if err != nil {
		return err
	}
	if len(vols) == 0 {
		fmt.Fprintf(w, "The catalog is empty. Run %s in a directory to start.\n", ui.Bold.Render("mediacat scan"))
		return nil
	}
	stats, err := a.db.Stats()
	if err != nil {
		return err
	}
	latest, err := a.db.LatestCompleteScans()
	if err != nil {
		return err
	}
	type agg struct {
		byKind                 map[string]int
		bytes                  int64
		missing, errors, total int
	}
	per := map[int64]*agg{}
	for _, s := range stats {
		g := per[s.VolumeID]
		if g == nil {
			g = &agg{byKind: map[string]int{}}
			per[s.VolumeID] = g
		}
		g.byKind[s.Kind] += s.Count
		g.total += s.Count
		g.bytes += s.Bytes
		g.missing += s.Missing
		g.errors += s.Errors
	}

	cols := []ui.Column{
		{Title: "VOLUME", Style: ui.Bold}, {Title: "MOUNTED AT"},
		{Title: "VIDEO", Right: true}, {Title: "AUDIO", Right: true}, {Title: "IMAGE", Right: true},
		{Title: "SIZE", Right: true}, {Title: "ERRORS", Right: true}, {Title: "MISSING", Right: true},
		{Title: "LAST SCAN"},
	}
	errS, warnS, dimS, okS := ui.Err, ui.Warn, ui.Dim, ui.OK
	var rows [][]ui.Cell
	var warnings []string
	for _, v := range vols {
		g := per[v.ID]
		if g == nil {
			g = &agg{byKind: map[string]int{}}
		}
		mount, _, mounted := a.mountedPath(v.Key, v.LastMountPath, v.LastMountRoot)
		mc := ui.Cell{Text: mount}
		if !mounted {
			mc = ui.Cell{Text: "not mounted", Style: &dimS}
		}
		num := func(n int, st *ui.Cell) ui.Cell {
			c := ui.Cell{Text: ui.Count(n)}
			if n > 0 && st != nil {
				c.Style = st.Style
			}
			return c
		}
		last := ui.Cell{Text: "never", Style: &warnS}
		if s, ok := latest[v.ID]; ok && s.FinishedAt != nil {
			last = ui.Cell{Text: ui.Age(*s.FinishedAt, now), Style: &okS}
			if msg := ui.Stale(s.FinishedAt, a.staleDays, now, mount); msg != "" {
				last.Style = &warnS
				warnings = append(warnings, msg)
			}
		}
		rows = append(rows, []ui.Cell{
			{Text: v.Key}, mc,
			num(g.byKind[filter.KindVideo], nil), num(g.byKind[filter.KindAudio], nil), num(g.byKind[filter.KindImage], nil),
			{Text: ui.Bytes(g.bytes)}, num(g.errors, &ui.Cell{Style: &errS}), num(g.missing, &ui.Cell{Style: &warnS}),
			last,
		})
	}
	ui.Table(w, cols, rows, ui.Width(w))

	if err := a.recentScans(vols); err != nil {
		return err
	}
	a.cwdStatus()
	for _, msg := range warnings {
		a.warn(msg)
	}
	return nil
}

func (a *app) recentScans(vols []catalog.Volume) error {
	scans, err := a.db.RecentScans(5)
	if err != nil || len(scans) == 0 {
		return err
	}
	byID := map[int64]catalog.Volume{}
	for _, v := range vols {
		byID[v.ID] = v
	}
	w := a.stdout
	fmt.Fprintf(w, "\n%s\n", ui.Bold.Render("Recent scans"))
	cols := []ui.Column{{Title: "WHEN"}, {Title: "STATUS"}, {Title: "SEEN", Right: true}, {Title: "ADDED", Right: true},
		{Title: "CHANGED", Right: true}, {Title: "REMOVED", Right: true}, {Title: "ROOT", Flex: true}}
	okS, warnS, errS := ui.OK, ui.Warn, ui.Err
	var rows [][]ui.Cell
	for _, s := range scans {
		v := byID[s.VolumeID]
		mount, root, _ := a.mountedPath(v.Key, v.LastMountPath, v.LastMountRoot)
		where, ok := volume.AbsPath(mount, root, s.Root)
		if !ok {
			where = v.Key + ":" + s.Root
		}
		st := &okS
		switch s.Status {
		case catalog.ScanInterrupted, catalog.ScanRunning:
			st = &warnS
		case catalog.ScanFailed:
			st = &errS
		}
		rows = append(rows, []ui.Cell{
			{Text: ui.Age(s.StartedAt, a.now())}, {Text: s.Status, Style: st},
			{Text: ui.Count(s.FilesSeen)}, {Text: ui.Count(s.Added)}, {Text: ui.Count(s.Changed)}, {Text: ui.Count(s.Removed)},
			{Text: where},
		})
	}
	ui.Table(w, cols, rows, ui.Width(w))
	return nil
}

// cwdStatus reports how fresh the catalog is for the current directory.
func (a *app) cwdStatus() {
	v, rel, err := a.res.Resolve(".")
	if err != nil {
		return
	}
	abs, _ := v.AbsPath(rel)
	stored, err := a.db.VolumeByKey(v.Key)
	if err != nil {
		return
	}
	var line string
	if stored != nil {
		if s, err := a.db.LastCompleteScan(stored.ID, rel); err == nil && s != nil && s.FinishedAt != nil {
			line = fmt.Sprintf("Current directory last scanned %s.", ui.Age(*s.FinishedAt, a.now()))
			if ui.Stale(s.FinishedAt, a.staleDays, a.now(), abs) != "" {
				line = ui.Warn.Render(line)
			}
		}
	}
	if line == "" {
		line = ui.Dim.Render(fmt.Sprintf("Current directory %s is not cataloged.", abs))
	}
	fmt.Fprintf(a.stdout, "\n%s\n", strings.TrimSpace(line))
}
