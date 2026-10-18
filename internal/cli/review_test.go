package cli

import (
	"strings"
	"testing"

	"github.com/njhoffman/mediacat/internal/catalog"
	"github.com/njhoffman/mediacat/internal/dupes"
	"github.com/njhoffman/mediacat/internal/review"
)

// pick returns a reviewer stand-in that keeps the copy whose path ends with keep and
// applies action, recording what it was shown.
func pick(keep, action string, apply bool, seen *[]review.Choice, opts *review.Options) func([]dupes.Group, []review.Choice, review.Options) (review.Result, error) {
	return func(gs []dupes.Group, cs []review.Choice, o review.Options) (review.Result, error) {
		if seen != nil {
			*seen = append([]review.Choice(nil), cs...)
		}
		if opts != nil {
			*opts = o
		}
		for fi, f := range gs[0].Files {
			if strings.HasSuffix(f.AbsPath, keep) {
				cs[0].Keep = fi
			}
		}
		cs[0].Action = action
		o.Save(0, cs[0])
		return review.Result{Choices: cs, Apply: apply}, nil
	}
}

func TestReviewApplies(t *testing.T) {
	h, paths := dupeFixture(t)
	h.interactive, h.stdin = true, "y\n"
	h.reviewRun = pick("backup/clip.mp4", catalog.ActionTrash, true, nil, nil)
	out, errs := h.ok("dupes", h.dir, "--review")
	if !strings.Contains(out, "1 groups: trash the other copies") || !strings.Contains(errs, "will be MOVED TO TRASH") || !strings.Contains(out, "Done: 2 files") {
		t.Fatalf("review apply:\n%s\n%s", out, errs)
	}
	if !exists(paths[1]) || exists(paths[0]) || exists(paths[2]) {
		t.Error("the chosen copy should be the one kept")
	}
	// Applied choices are forgotten.
	var seen []review.Choice
	h.ok("undo", "1", "--yes")
	h.reviewRun = func(gs []dupes.Group, cs []review.Choice, o review.Options) (review.Result, error) {
		seen = cs
		return review.Result{Choices: cs}, nil
	}
	h.ok("dupes", h.dir, "--review")
	if len(seen) != 1 || seen[0].Action != "" {
		t.Errorf("applied choice should not be resumed: %+v", seen)
	}
}

func TestReviewQuitSavesChoices(t *testing.T) {
	h, paths := dupeFixture(t)
	h.interactive = true
	h.reviewRun = pick("clip (1).mp4", catalog.ActionDelete, false, nil, nil)
	out, _ := h.ok("dupes", h.dir, "--review")
	if !strings.Contains(out, "Choices are saved (1 groups decided)") || !exists(paths[1]) || !exists(paths[2]) {
		t.Fatalf("quit:\n%s", out)
	}
	var seen []review.Choice
	var opts review.Options
	h.reviewRun = func(gs []dupes.Group, cs []review.Choice, o review.Options) (review.Result, error) {
		seen, opts = cs, o
		return review.Result{Choices: cs}, nil
	}
	h.ok("dupes", h.dir, "--review")
	if len(seen) != 1 || seen[0].Action != catalog.ActionDelete || !strings.Contains(opts.Message, "Resumed saved choices for 1 of 1") {
		t.Fatalf("resume: %+v %q", seen, opts.Message)
	}
	// The resumed keeper is the saved one, not the policy's choice.
	h.reviewRun = pick("clip (1).mp4", catalog.ActionDelete, true, nil, nil)
	h.stdin = "delete\n"
	out, errs := h.ok("dupes", h.dir, "--review")
	if !strings.Contains(errs, "PERMANENTLY DELETED") || !exists(paths[2]) || exists(paths[0]) || exists(paths[1]) {
		t.Errorf("delete via review:\n%s\n%s", out, errs)
	}
}

func TestReviewMoveUsesConfigDestination(t *testing.T) {
	h, _ := dupeFixture(t)
	h.interactive = true
	var opts review.Options
	h.reviewRun = func(gs []dupes.Group, cs []review.Choice, o review.Options) (review.Result, error) {
		opts = o
		return review.Result{Choices: cs}, nil
	}
	h.ok("dupes", h.dir, "--review")
	if opts.MoveTo != "" {
		t.Errorf("no destination configured: %q", opts.MoveTo)
	}
	writeConfig(t, h, "dupes:\n  move_to: "+h.dir+"/trashbox\n")
	h.ok("dupes", h.dir, "--review")
	if opts.MoveTo != h.dir+"/trashbox" {
		t.Errorf("config move_to: %q", opts.MoveTo)
	}
}

func TestReviewFlagErrors(t *testing.T) {
	h := newHarness(t)
	for _, args := range [][]string{
		{"dupes", "--review"},
		{"dupes", "--review", "--execute", "--trash"},
		{"dupes", "--review", "--depth", "partial"},
	} {
		if _, errs, code := h.run(args...); code != 1 || !strings.Contains(errs, "--review") {
			t.Errorf("%v: %d %s", args, code, errs)
		}
	}
}
