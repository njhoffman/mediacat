package cli

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"

	"github.com/njhoffman/mediacat/internal/catalog"
	"github.com/njhoffman/mediacat/internal/dupes"
	"github.com/njhoffman/mediacat/internal/review"
	"github.com/njhoffman/mediacat/internal/ui"
	"github.com/njhoffman/mediacat/internal/volume"
)

// openFile previews a file with the desktop's default application, detached.
func openFile(path string) error {
	cmd := exec.Command("xdg-open", path)
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}

// runReview lets the user decide group by group, then applies each action kind through
// the usual confirmation. Choices are saved as they are made, so quitting loses nothing.
func (a *app) runReview(ctx context.Context, kind, root string, df dupesFlags, res *dupes.Result, vols map[string]volume.Volume) error {
	groups := res.Groups
	if len(groups) == 0 {
		a.printDupes(res, root, 0)
		return nil
	}
	moveTo := df.moveTo
	if moveTo == "" {
		moveTo = a.cfg.Dupes.MoveTo
	}
	if moveTo != "" {
		abs, err := filepath.Abs(moveTo)
		if err != nil {
			return err
		}
		moveTo = abs
	}

	keys := make([]string, len(groups))
	for i, g := range groups {
		keys[i] = g.Key
	}
	saved, err := a.db.LoadDecisions(keys)
	if err != nil {
		return err
	}
	choices := make([]review.Choice, len(groups))
	resumed := 0
	for i, g := range groups {
		c := review.Choice{Keep: g.Keep, Action: kind}
		if d, ok := saved[g.Key]; ok {
			for fi, f := range g.Files {
				if f.VolumeKey == d.KeepVolume && f.RelPath == d.KeepRelPath {
					c.Keep = fi
				}
			}
			c.Action = d.Action
			resumed++
		}
		if c.Action == catalog.ActionMove && moveTo == "" {
			c.Action = ""
		}
		choices[i] = c
	}

	var saveErr error
	opts := review.Options{
		MoveTo: moveTo,
		Open:   openFile,
		Save: func(gi int, c review.Choice) {
			f := groups[gi].Files[c.Keep]
			if err := a.db.SaveDecision(groups[gi].Key, catalog.Decision{KeepVolume: f.VolumeKey, KeepRelPath: f.RelPath, Action: c.Action}, a.now()); err != nil && saveErr == nil {
				saveErr = err
			}
		},
	}
	if resumed > 0 {
		opts.Message = fmt.Sprintf("Resumed saved choices for %d of %d groups", resumed, len(groups))
	}
	run := a.reviewRun
	if run == nil {
		run = review.Run
	}
	result, err := run(groups, choices, opts)
	if err != nil {
		return err
	}
	if saveErr != nil {
		a.warn("some choices could not be saved: " + saveErr.Error())
	}
	if !result.Apply {
		t := review.Summarize(groups, result.Choices)
		fmt.Fprintf(a.stdout, "Nothing was changed. Choices are saved (%d groups decided); run the same command again to continue.\n",
			len(groups)-t.Groups[""])
		return nil
	}

	for _, k := range []string{catalog.ActionTrash, catalog.ActionMove, catalog.ActionDelete} {
		var sel []dupes.Group
		var selKeys []string
		for i, g := range groups {
			if result.Choices[i].Action != k {
				continue
			}
			g.Keep = result.Choices[i].Keep
			sel = append(sel, g)
			selKeys = append(selKeys, g.Key)
		}
		if len(sel) == 0 {
			continue
		}
		step := df
		step.execute, step.yes, step.moveTo = true, false, moveTo
		fmt.Fprintf(a.stdout, "\n%s\n", ui.Bold.Render(fmt.Sprintf("%d groups: %s", len(sel), describeAction(k))))
		applied, err := a.act(ctx, k, root, step, sel, vols)
		if applied {
			if derr := a.db.DeleteDecisions(selKeys); derr != nil {
				a.warn("could not clear applied choices: " + derr.Error())
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func describeAction(kind string) string {
	switch kind {
	case catalog.ActionTrash:
		return "trash the other copies"
	case catalog.ActionMove:
		return "move the other copies"
	}
	return "permanently delete the other copies"
}
