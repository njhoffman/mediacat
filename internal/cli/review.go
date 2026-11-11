package cli

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

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
	moveTo, err := a.moveDestination(df)
	if err != nil {
		return err
	}
	saved, err := a.savedChoices(groups)
	if err != nil {
		return err
	}
	choices := make([]review.Choice, len(groups))
	resumed := 0
	for i, g := range groups {
		c := review.Choice{Keep: g.Keep, Action: kind}
		if sc, ok := saved[i]; ok {
			c = sc.choice
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

	step := df
	step.execute, step.yes, step.moveTo = true, false, moveTo
	return a.applyChoices(ctx, root, step, groups, result.Choices, vols)
}

// applyChoices runs each action kind over the groups chosen for it, through the usual
// summary and confirmation, and forgets the saved choices of the groups it applied.
func (a *app) applyChoices(ctx context.Context, root string, df dupesFlags, groups []dupes.Group, choices []review.Choice, vols map[string]volume.Volume) error {
	for _, k := range []string{catalog.ActionTrash, catalog.ActionMove, catalog.ActionDelete} {
		var sel []dupes.Group
		var selKeys []string
		for i, g := range groups {
			if choices[i].Action != k {
				continue
			}
			g.Keep = choices[i].Keep
			sel = append(sel, g)
			selKeys = append(selKeys, g.Key)
		}
		if len(sel) == 0 {
			continue
		}
		fmt.Fprintf(a.stdout, "\n%s\n", ui.Bold.Render(fmt.Sprintf("%d groups: %s", len(sel), describeAction(k))))
		applied, err := a.act(ctx, k, root, df, sel, vols)
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

// moveDestination is where "move" sends copies: --move-to, else dupes.move_to, as an
// absolute path; empty when neither is set.
func (a *app) moveDestination(df dupesFlags) (string, error) {
	moveTo := df.moveTo
	if moveTo == "" {
		moveTo = a.cfg.Dupes.MoveTo
	}
	if moveTo == "" {
		return "", nil
	}
	return filepath.Abs(moveTo)
}

type savedChoice struct {
	choice review.Choice
	// stale: the kept copy the decision names is no longer in the group.
	stale bool
	keep  string
}

// savedChoices loads the saved decisions for groups, by group index.
func (a *app) savedChoices(groups []dupes.Group) (map[int]savedChoice, error) {
	keys := make([]string, len(groups))
	for i, g := range groups {
		keys[i] = g.Key
	}
	saved, err := a.db.LoadDecisions(keys)
	if err != nil {
		return nil, err
	}
	out := map[int]savedChoice{}
	for i, g := range groups {
		d, ok := saved[g.Key]
		if !ok {
			continue
		}
		sc := savedChoice{choice: review.Choice{Keep: g.Keep, Action: d.Action}, stale: true, keep: d.KeepVolume + ":" + d.KeepRelPath}
		for fi, f := range g.Files {
			if f.VolumeKey == d.KeepVolume && f.RelPath == d.KeepRelPath {
				sc.choice.Keep, sc.stale = fi, false
			}
		}
		out[i] = sc
	}
	return out, nil
}

// runApplySaved applies the saved decisions for the groups found, without the review
// screen. Groups without a decision, or decided "leave", are left alone; a decision whose
// kept copy is no longer in its group is skipped. The flags decide dry run, --execute and
// --yes as for any action.
func (a *app) runApplySaved(ctx context.Context, root string, df dupesFlags, res *dupes.Result, vols map[string]volume.Volume) error {
	groups := res.Groups
	saved, err := a.savedChoices(groups)
	if err != nil {
		return err
	}
	moveTo, err := a.moveDestination(df)
	if err != nil {
		return err
	}
	choices := make([]review.Choice, len(groups))
	counts := map[string]int{}
	var stale, noDest []string
	undecided := 0
	for i, g := range groups {
		choices[i] = review.Choice{Keep: g.Keep}
		sc, ok := saved[i]
		switch {
		case !ok:
			undecided++
		case sc.stale:
			stale = append(stale, fmt.Sprintf("%s (keep %s)", shortKey(g.Key), sc.keep))
		case sc.choice.Action == catalog.ActionMove && moveTo == "":
			noDest = append(noDest, shortKey(g.Key))
		default:
			choices[i] = sc.choice
			counts[sc.choice.Action]++
		}
	}
	fmt.Fprintf(a.stdout, "%s  %s  %s\n", ui.Title.Render("Saved decisions"), ui.Bold.Render(root),
		ui.Dim.Render(fmt.Sprintf("(%s groups found)", ui.Count(len(groups)))))
	fmt.Fprintf(a.stdout, "  %d trash, %d move, %d delete, %d leave; %d groups without a decision are left alone\n",
		counts[catalog.ActionTrash], counts[catalog.ActionMove], counts[catalog.ActionDelete], counts[""], undecided)
	if len(stale) > 0 {
		a.warn(fmt.Sprintf("%d decisions skipped: the copy to keep is no longer in the group (decide again with --review): %s",
			len(stale), strings.Join(stale, ", ")))
	}
	if len(noDest) > 0 {
		a.warn(fmt.Sprintf("%d move decisions skipped: no destination (--move-to or dupes.move_to): %s", len(noDest), strings.Join(noDest, ", ")))
	}
	if counts[catalog.ActionTrash]+counts[catalog.ActionMove]+counts[catalog.ActionDelete] == 0 {
		fmt.Fprintln(a.stdout, "\nNothing to do.")
		return nil
	}
	step := df
	step.moveTo = moveTo
	return a.applyChoices(ctx, root, step, groups, choices, vols)
}

func shortKey(k string) string {
	if len(k) > 12 {
		return k[:12]
	}
	return k
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
