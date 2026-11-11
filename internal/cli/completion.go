package cli

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/njhoffman/mediacat/internal/catalog"
	"github.com/njhoffman/mediacat/internal/dupes"
	"github.com/njhoffman/mediacat/internal/filter"
)

// Shell completion for argument and flag values. Command and flag names come from cobra.

var kinds = []string{"video\tvideo files", "audio\taudio files", "image\timage files"}

// completeValues offers a fixed set of values ("value\tdescription").
func completeValues(values ...string) cobra.CompletionFunc {
	return func(_ *cobra.Command, _ []string, _ string) ([]cobra.Completion, cobra.ShellCompDirective) {
		return values, cobra.ShellCompDirectiveNoFileComp
	}
}

// completeList completes the last element of a comma-separated list, e.g. "prefer,av"
// offers "prefer,avoid". Elements already given are not offered again. prefixes are
// optional markers an element may start with, such as "-" for a descending sort.
func completeList(values []string, prefixes ...string) cobra.CompletionFunc {
	return func(_ *cobra.Command, _ []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
		return listCompletions(values, prefixes, toComplete), cobra.ShellCompDirectiveNoFileComp | cobra.ShellCompDirectiveNoSpace
	}
}

func listCompletions(values, prefixes []string, toComplete string) []string {
	head, last := "", toComplete
	if i := strings.LastIndex(toComplete, ","); i >= 0 {
		head, last = toComplete[:i+1], toComplete[i+1:]
	}
	used := map[string]bool{}
	for u := range strings.SplitSeq(head, ",") {
		for _, p := range prefixes {
			u = strings.TrimPrefix(u, p)
		}
		used[strings.TrimSpace(u)] = true
	}
	marker := ""
	for _, p := range prefixes {
		if rest, ok := strings.CutPrefix(last, p); ok {
			marker, last = p, rest
			break
		}
	}
	var out []string
	for _, v := range values {
		name, desc, _ := strings.Cut(v, "\t")
		if used[name] || !strings.HasPrefix(name, last) {
			continue
		}
		c := head + marker + name
		if desc != "" {
			c += "\t" + desc
		}
		out = append(out, c)
	}
	return out
}

func completeDirs(_ *cobra.Command, _ []string, _ string) ([]cobra.Completion, cobra.ShellCompDirective) {
	return nil, cobra.ShellCompDirectiveFilterDirs
}

// completeDirArg completes a single optional directory argument.
func completeDirArg(_ *cobra.Command, args []string, _ string) ([]cobra.Completion, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return nil, cobra.ShellCompDirectiveFilterDirs
}

func completeExt(exts ...string) cobra.CompletionFunc {
	return func(_ *cobra.Command, _ []string, _ string) ([]cobra.Completion, cobra.ShellCompDirective) {
		return exts, cobra.ShellCompDirectiveFilterFileExt
	}
}

// completeBatches offers recent batch numbers from the catalog, newest first; undoable
// limits them to trash and move batches not yet undone.
func (a *app) completeBatches(undoable bool) cobra.CompletionFunc {
	return func(_ *cobra.Command, args []string, _ string) ([]cobra.Completion, cobra.ShellCompDirective) {
		if len(args) > 0 || a.open() != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		batches, err := a.db.Batches(50)
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		var out []string
		for _, b := range batches {
			if undoable && (b.UndoneAt != nil || b.Kind == catalog.ActionDelete) {
				continue
			}
			out = append(out, fmt.Sprintf("%s\t%s %s, %d files, %s", strconv.FormatInt(b.ID, 10), b.StartedAt.Format("2006-01-02"), b.Kind, b.Done, b.Root))
		}
		return out, cobra.ShellCompDirectiveNoFileComp | cobra.ShellCompDirectiveKeepOrder
	}
}

func keepRules() []string {
	var out []string
	for r, desc := range dupes.Rules {
		out = append(out, r+"\t"+desc)
	}
	sort.Strings(out)
	return out
}

func similarLevels() []string {
	var out []string
	for _, l := range dupes.Levels {
		out = append(out, fmt.Sprintf("%s\tphashes up to %d bits apart", l.Name, l.Distance))
	}
	return out
}

func mustRegister(cmd *cobra.Command, flag string, fn cobra.CompletionFunc) {
	if err := cmd.RegisterFlagCompletionFunc(flag, fn); err != nil {
		panic(err)
	}
}

// registerCompletions attaches value completion to every command's arguments and flags.
func (a *app) registerCompletions(root *cobra.Command) {
	mustRegister(root, "config", completeExt("yaml", "yml"))
	mustRegister(root, "stash-config", completeExt("yaml", "yml"))
	mustRegister(root, "db", completeExt("db", "sqlite"))
	for _, cmd := range root.Commands() {
		switch cmd.Name() {
		case "scan", "list", "check", "trash":
			cmd.ValidArgsFunction = completeDirArg
		case "dupes":
			cmd.ValidArgsFunction = completeDirArg
			mustRegister(cmd, "depth", completeValues("full\tidentical content (needed for actions)",
				"partial\tsame size and sampled blocks", "size\tsame size only"))
			mustRegister(cmd, "keep", completeList(keepRules()))
			mustRegister(cmd, "similar", completeValues(similarLevels()...))
			for _, f := range []string{"prefer", "avoid", "move-to", "only-under"} {
				mustRegister(cmd, f, completeDirs)
			}
			mustRegister(cmd, "export", completeExt("csv", "json"))
			mustRegister(cmd, "prefer-path", cobra.NoFileCompletions)
		case "info":
			cmd.ValidArgsFunction = func(_ *cobra.Command, args []string, _ string) ([]cobra.Completion, cobra.ShellCompDirective) {
				if len(args) > 0 {
					return nil, cobra.ShellCompDirectiveNoFileComp
				}
				return nil, cobra.ShellCompDirectiveDefault
			}
		case "undo":
			cmd.ValidArgsFunction = a.completeBatches(true)
		case "history":
			cmd.ValidArgsFunction = a.completeBatches(false)
		}
		if cmd.Flags().Lookup("type") != nil {
			mustRegister(cmd, "type", completeValues(kinds...))
		}
		if cmd.Name() == "list" {
			mustRegister(cmd, "sort", completeList(filter.Fields, "-"))
			mustRegister(cmd, "columns", completeList(filter.Fields, "+", "-"))
			mustRegister(cmd, "check", completeList(filter.CheckStatuses))
		}
		// Every other value flag takes free text (sizes, durations, names): no file names.
		cmd.Flags().VisitAll(func(f *pflag.Flag) {
			if t := f.Value.Type(); t != "string" && t != "stringSlice" {
				return
			}
			if _, ok := cmd.GetFlagCompletionFunc(f.Name); !ok {
				mustRegister(cmd, f.Name, cobra.NoFileCompletions)
			}
		})
	}
}
