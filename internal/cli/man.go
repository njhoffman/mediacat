package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/cobra/doc"
)

// manCmd writes the man pages: mediacat(1) and one page per command, from the same help
// text as --help. Hidden: it is for packaging and `just build`, not everyday use.
func (a *app) manCmd(version string) *cobra.Command {
	return &cobra.Command{
		Use:    "man <dir>",
		Short:  "Write man pages for mediacat and each command to dir",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		ValidArgsFunction: func(_ *cobra.Command, _ []string, _ string) ([]cobra.Completion, cobra.ShellCompDirective) {
			return nil, cobra.ShellCompDirectiveFilterDirs
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := os.MkdirAll(args[0], 0o755); err != nil {
				return err
			}
			root := cmd.Root()
			root.DisableAutoGenTag = true
			preformatHelp(root)
			header := &doc.GenManHeader{Section: "1", Source: "mediacat " + version, Manual: "mediacat manual"}
			if err := doc.GenManTree(root, header, args[0]); err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "Wrote man pages to %s\n", args[0])
			return nil
		},
	}
}

// preformatHelp keeps the help text's indented tables (rules, depths, fields) aligned in
// the man pages: the generator reads the text as Markdown and would refill those lines, so
// each run of indented lines becomes a code block.
func preformatHelp(cmd *cobra.Command) {
	cmd.Long = manMarkdown(cmd.Long)
	for _, c := range cmd.Commands() {
		preformatHelp(c)
	}
}

func manMarkdown(text string) string {
	var out []string
	inBlock := false
	for _, line := range strings.Split(text, "\n") {
		indented := strings.HasPrefix(line, "  ") && strings.TrimSpace(line) != ""
		switch {
		case indented && !inBlock:
			if len(out) > 0 && out[len(out)-1] != "" {
				out = append(out, "")
			}
			inBlock = true
		case !indented && inBlock:
			if line != "" {
				out = append(out, "")
			}
			inBlock = false
		}
		if indented {
			line = "    " + line
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}
