package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/njhoffman/mediacat/internal/catalog"
	"github.com/njhoffman/mediacat/internal/ui"
)

// decisionRow is one decision as imported and listed: the group key, the absolute path of
// the copy to keep, and the action for the other copies.
type decisionRow struct {
	Group  string `json:"group"`
	Keep   string `json:"keep"`
	Action string `json:"action"`
	At     string `json:"updated_at,omitempty"`
}

// decisionActions maps the action names a file may use to stored actions ("" leaves the
// group alone).
var decisionActions = map[string]string{
	catalog.ActionTrash: catalog.ActionTrash, catalog.ActionMove: catalog.ActionMove,
	catalog.ActionDelete: catalog.ActionDelete, "leave": "",
}

func (a *app) decisionsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "decisions",
		Short: "List, import or clear the per-group choices that dupes --review saves",
		Long: `Duplicate groups decided in dupes --review keep their choice (the copy to keep and
trash, move, delete or leave) until it is applied. These commands manage those choices
directly, so another tool can prepare them; apply them with dupes --apply-saved.

Import files hold one decision per line, as TSV or JSON Lines (a JSON array also works):
  group   the group's key: the "hash" from dupes --json or --export
  keep    absolute path of the copy to keep
  action  trash, move, delete or leave
TSV columns are in that order; a first line starting with "group" is a header.

Every row is checked first: the copy to keep must be cataloged and, for identical groups,
have the group's content hash. Similar groups' keys are checked when applied. One bad
row rejects the whole file, so nothing is half imported.`,
		Example: `  mediacat decisions
  mediacat dupes /mnt/nfs/Stash --json | jq -r '.[] | [.hash, .keep, "trash"] | @tsv' > d.tsv
  mediacat decisions import d.tsv
  mediacat dupes /mnt/nfs/Stash --apply-saved --execute --yes
  mediacat decisions clear --all`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return a.listDecisions(false) },
	}
	var asJSON bool
	list := &cobra.Command{
		Use:   "list",
		Short: "List saved decisions, newest first",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return a.listDecisions(asJSON) },
	}
	list.Flags().BoolVar(&asJSON, "json", false, "print JSON Lines in the import format")
	imp := &cobra.Command{
		Use:   "import <file>",
		Short: `Import decisions from a TSV or JSON Lines file ("-" reads stdin)`,
		Args:  cobra.ExactArgs(1),
		ValidArgsFunction: func(_ *cobra.Command, args []string, _ string) ([]cobra.Completion, cobra.ShellCompDirective) {
			if len(args) > 0 {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			return []string{"tsv", "jsonl", "json"}, cobra.ShellCompDirectiveFilterFileExt
		},
		RunE: func(cmd *cobra.Command, args []string) error { return a.importDecisions(args[0]) },
	}
	var all, yes bool
	clr := &cobra.Command{
		Use:   "clear [group...]",
		Short: "Forget saved decisions for the given groups, or --all",
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.clearDecisions(args, all, yes)
		},
		ValidArgsFunction: a.completeDecisionKeys,
	}
	clr.Flags().BoolVar(&all, "all", false, "forget every saved decision")
	clr.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation for --all")
	cmd.AddCommand(list, imp, clr)
	return cmd
}

// keepPath shows where a decision's kept copy is: its absolute path when the volume is
// mounted, else volume:relpath.
func (a *app) keepPath(d catalog.Decision) string {
	if v, ok := a.res.Lookup(d.KeepVolume); ok {
		if p, ok := v.AbsPath(d.KeepRelPath); ok {
			return p
		}
	}
	return d.KeepVolume + ":" + d.KeepRelPath
}

func actionName(stored string) string {
	if stored == "" {
		return "leave"
	}
	return stored
}

func (a *app) listDecisions(asJSON bool) error {
	if err := a.open(); err != nil {
		return err
	}
	ds, err := a.db.Decisions()
	if err != nil {
		return err
	}
	if asJSON {
		enc := json.NewEncoder(a.stdout)
		for _, d := range ds {
			if err := enc.Encode(decisionRow{Group: d.Key, Keep: a.keepPath(d.Decision), Action: actionName(d.Action), At: d.UpdatedAt.Format("2006-01-02T15:04:05Z07:00")}); err != nil {
				return err
			}
		}
		return nil
	}
	if len(ds) == 0 {
		fmt.Fprintln(a.stdout, "No saved decisions. dupes --review saves them; decisions import adds them from a file.")
		return nil
	}
	counts := map[string]int{}
	for _, d := range ds {
		counts[actionName(d.Action)]++
	}
	fmt.Fprintf(a.stdout, "%s  %s\n", ui.Title.Render("Saved decisions"), ui.Dim.Render(fmt.Sprintf("(%d: %d trash, %d move, %d delete, %d leave)",
		len(ds), counts["trash"], counts["move"], counts["delete"], counts["leave"])))
	for _, d := range ds {
		fmt.Fprintf(a.stdout, "  %s  %-6s  %s  %s\n", ui.Dim.Render(d.UpdatedAt.Format("2006-01-02")), actionName(d.Action), ui.Dim.Render(d.Key), a.keepPath(d.Decision))
	}
	fmt.Fprintf(a.stdout, "\n%s\n", ui.Dim.Render("Apply with: mediacat dupes PATH --apply-saved (a dry run until --execute)"))
	return nil
}

// parseDecisions reads TSV, JSON Lines or a JSON array. Line numbers are 1-based; for a
// JSON array they count elements.
func parseDecisions(r io.Reader) ([]decisionRow, []int, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, nil, err
	}
	trimmed := bytes.TrimSpace(b)
	var rows []decisionRow
	var lines []int
	if bytes.HasPrefix(trimmed, []byte("[")) {
		if err := json.Unmarshal(trimmed, &rows); err != nil {
			return nil, nil, fmt.Errorf("reading JSON: %w", err)
		}
		for i := range rows {
			lines = append(lines, i+1)
		}
		return rows, lines, nil
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimRight(sc.Text(), "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		var row decisionRow
		if strings.HasPrefix(strings.TrimSpace(line), "{") {
			if err := json.Unmarshal([]byte(line), &row); err != nil {
				return nil, nil, fmt.Errorf("line %d: %w", n, err)
			}
		} else {
			f := strings.Split(line, "\t")
			if len(rows) == 0 && strings.EqualFold(strings.TrimSpace(f[0]), "group") {
				continue
			}
			if len(f) != 3 {
				return nil, nil, fmt.Errorf("line %d: want 3 tab-separated columns (group, keep, action), got %d", n, len(f))
			}
			row = decisionRow{Group: strings.TrimSpace(f[0]), Keep: f[1], Action: strings.TrimSpace(f[2])}
		}
		rows = append(rows, row)
		lines = append(lines, n)
	}
	return rows, lines, sc.Err()
}

func (a *app) importDecisions(path string) error {
	var r io.Reader = a.stdin
	name := "stdin"
	if path != "-" {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		r, name = f, path
	}
	rows, lines, err := parseDecisions(r)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if len(rows) == 0 {
		return fmt.Errorf("%s: no decisions", name)
	}
	if err := a.open(); err != nil {
		return err
	}
	var out []catalog.SavedDecision
	var problems []string
	seen := map[string]int{}
	bad := func(i int, format string, args ...any) {
		problems = append(problems, fmt.Sprintf("line %d: %s", lines[i], fmt.Sprintf(format, args...)))
	}
	for i, row := range rows {
		action, ok := decisionActions[strings.ToLower(row.Action)]
		switch {
		case row.Group == "":
			bad(i, "no group key")
			continue
		case !ok:
			bad(i, "unknown action %q (want trash, move, delete or leave)", row.Action)
			continue
		case !strings.HasPrefix(row.Keep, "/"):
			bad(i, "the copy to keep must be an absolute path: %q", row.Keep)
			continue
		}
		if prev, dup := seen[row.Group]; dup {
			bad(i, "group %s is also decided on line %d", shortKey(row.Group), prev)
			continue
		}
		seen[row.Group] = lines[i]
		v, rel, err := a.res.Resolve(row.Keep)
		if errors.Is(err, fs.ErrNotExist) {
			bad(i, "%s does not exist", row.Keep)
			continue
		}
		if err != nil {
			bad(i, "%v", err)
			continue
		}
		stored, err := a.db.VolumeByKey(v.Key)
		if err != nil {
			return err
		}
		var f *catalog.File
		if stored != nil {
			if f, err = a.db.GetFile(stored.ID, rel); err != nil {
				return err
			}
		}
		switch {
		case f == nil:
			bad(i, "%s is not in the catalog (run mediacat scan)", row.Keep)
			continue
		case !strings.HasPrefix(row.Group, "similar:") && f.FullHash != row.Group:
			bad(i, "%s does not have the content of group %s (it changed, or the key is wrong)", row.Keep, shortKey(row.Group))
			continue
		}
		out = append(out, catalog.SavedDecision{Key: row.Group, Decision: catalog.Decision{KeepVolume: v.Key, KeepRelPath: rel, Action: action}})
	}
	if len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintf(a.stderr, "  %s\n", p)
		}
		return fmt.Errorf("%s: %d of %d rows rejected; nothing was imported", name, len(problems), len(rows))
	}
	if err := a.db.SaveDecisions(out, a.now()); err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "Imported %d decisions from %s. Apply with: mediacat dupes PATH --apply-saved\n", len(out), name)
	return nil
}

func (a *app) clearDecisions(keys []string, all, yes bool) error {
	switch {
	case all && len(keys) > 0:
		return errors.New("give group keys or --all, not both")
	case !all && len(keys) == 0:
		return errors.New("give the group keys to forget, or --all")
	}
	if err := a.open(); err != nil {
		return err
	}
	if !all {
		saved, err := a.db.LoadDecisions(keys)
		if err != nil {
			return err
		}
		var unknown []string
		for _, k := range keys {
			if _, ok := saved[k]; !ok {
				unknown = append(unknown, k)
			}
		}
		if len(unknown) > 0 {
			return fmt.Errorf("no saved decision for %s", strings.Join(unknown, ", "))
		}
		if err := a.db.DeleteDecisions(keys); err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "Forgot %d decisions.\n", len(keys))
		return nil
	}
	if !yes {
		ok, err := a.confirm("Forget every saved decision? [y/N] ", "")
		if err != nil {
			return fmt.Errorf("%w (or pass --yes)", err)
		}
		if !ok {
			fmt.Fprintln(a.stderr, "Cancelled; nothing was changed.")
			return nil
		}
	}
	n, err := a.db.ClearDecisions()
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "Forgot %d decisions.\n", n)
	return nil
}

func (a *app) completeDecisionKeys(_ *cobra.Command, args []string, _ string) ([]cobra.Completion, cobra.ShellCompDirective) {
	if a.open() != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	ds, err := a.db.Decisions()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	given := map[string]bool{}
	for _, k := range args {
		given[k] = true
	}
	var out []string
	for _, d := range ds {
		if !given[d.Key] {
			out = append(out, d.Key+"\t"+actionName(d.Action)+" keeping "+a.keepPath(d.Decision))
		}
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}
