// Package cli wires the mediacat commands.
package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/njhoffman/mediacat/internal/catalog"
	"github.com/njhoffman/mediacat/internal/config"
	"github.com/njhoffman/mediacat/internal/ui"
	"github.com/njhoffman/mediacat/internal/volume"
)

// ExitInterrupted is returned when a scan stopped early on request.
const ExitInterrupted = 130

type errInterrupted struct{}

func (errInterrupted) Error() string { return "interrupted" }

type app struct {
	dbPath    string
	staleDays int
	noColor   bool
	stashCfg  string
	stdin     io.Reader
	stdout    io.Writer
	stderr    io.Writer
	now       func() time.Time
	// interactive reports whether prompts can be answered; tests override it.
	interactive func() bool

	cfg        config.Config
	configPath string

	db  *catalog.DB
	res *volume.Resolver
}

// Execute runs the CLI and returns the process exit code.
func Execute(version string) int {
	a := &app{stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr, now: time.Now,
		interactive: func() bool { return ui.IsTerminal(os.Stdin) && ui.IsTerminal(os.Stderr) }}
	return a.run(version, os.Args[1:])
}

func (a *app) run(version string, args []string) int {
	// The config supplies flag defaults, so it is loaded before the commands are built.
	a.configPath = configPathFrom(args)
	cfg, created, err := config.Load(a.configPath)
	if err != nil {
		fmt.Fprintln(a.stderr, ui.Err.Render("error: ")+err.Error())
		return 1
	}
	a.cfg = cfg
	if created {
		fmt.Fprintln(a.stderr, ui.Dim.Render("Created default config at "+a.configPath))
	}
	root := a.rootCmd(version)
	root.SetArgs(args)
	root.SetOut(a.stdout)
	root.SetErr(a.stderr)
	err = root.Execute()
	if a.db != nil {
		a.db.Close()
	}
	switch {
	case err == nil:
		return 0
	case errors.As(err, new(errInterrupted)):
		return ExitInterrupted
	default:
		fmt.Fprintln(a.stderr, ui.Err.Render("error: ")+err.Error())
		return 1
	}
}

// configPathFrom finds --config in args before cobra parses them, falling back to
// MEDIACAT_CONFIG and then ~/.config/mediacat.yaml.
func configPathFrom(args []string) string {
	for i, arg := range args {
		if arg == "--" {
			break
		}
		if v, ok := strings.CutPrefix(arg, "--config="); ok {
			return config.Expand(v)
		}
		if arg == "--config" && i+1 < len(args) {
			return config.Expand(args[i+1])
		}
	}
	if v := os.Getenv("MEDIACAT_CONFIG"); v != "" {
		return config.Expand(v)
	}
	return config.DefaultPath()
}

func (a *app) rootCmd(version string) *cobra.Command {
	staleDefault := a.cfg.StaleDays
	if v, err := strconv.Atoi(os.Getenv("MEDIACAT_STALE_DAYS")); err == nil {
		staleDefault = v
	}
	root := &cobra.Command{
		Use:   "mediacat",
		Short: "Catalog audio, image and video files and manage duplicates",
		Long: `mediacat keeps a persistent catalog of media files and their metadata
(codecs, resolution, duration, tags) and finds and manages duplicates.

Run without arguments for a dashboard of what has been cataloged.`,
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.NoArgs,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if a.noColor {
				ui.DisableColor()
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error { return a.dashboard() },
	}
	f := root.PersistentFlags()
	var configFlag string
	f.StringVar(&configFlag, "config", a.configPath, "config file (env MEDIACAT_CONFIG); created with defaults if missing")
	f.StringVar(&a.dbPath, "db", a.cfg.Catalog, "catalog database path")
	f.IntVar(&a.staleDays, "stale-days", staleDefault, "warn when the catalog for a path is older than this many days (0 disables; env MEDIACAT_STALE_DAYS)")
	f.BoolVar(&a.noColor, "no-color", false, "disable colored output (also honors NO_COLOR)")
	f.StringVar(&a.stashCfg, "stash-config", a.cfg.StashConfig, "Stash config, read to remind you when files in Stash libraries change")
	root.AddCommand(a.scanCmd(), a.listCmd(), a.infoCmd(), a.dupesCmd(), a.trashCmd(), a.historyCmd(), a.undoCmd())
	return root
}

// open lazily opens the catalog and mount table.
func (a *app) open() error {
	if a.db == nil {
		db, err := catalog.Open(a.dbPath)
		if err != nil {
			return err
		}
		a.db = db
		if err := db.ReconcileRunning(catalog.ProcessAlive); err != nil {
			return err
		}
	}
	if a.res == nil {
		res, err := volume.NewResolver()
		if err != nil {
			return err
		}
		a.res = res
	}
	return nil
}

// warn prints a staleness or other warning to stderr so it never pollutes --json output.
func (a *app) warn(msg string) {
	if msg != "" {
		fmt.Fprintln(a.stderr, ui.Warn.Render("warning: ")+msg)
	}
}

// staleness warns when the newest complete scan covering rel on volume volID is old.
func (a *app) staleness(volID int64, rel, display string) error {
	s, err := a.db.LastCompleteScan(volID, rel)
	if err != nil {
		return err
	}
	var last *time.Time
	if s != nil {
		last = s.FinishedAt
	}
	a.warn(ui.Stale(last, a.staleDays, a.now(), display))
	return nil
}

// mountedPath maps a stored volume to where it is mounted now, falling back to the last
// known mount. ok is false when the volume is not currently mounted.
func (a *app) mountedPath(key, lastPath, lastRoot string) (string, string, bool) {
	if v, ok := a.res.Lookup(key); ok {
		return v.MountPoint, v.MountRoot, true
	}
	return lastPath, lastRoot, false
}

// confirm asks a question on stderr and reads the answer. With want == "" any answer
// starting with y accepts; otherwise the answer must equal want exactly.
func (a *app) confirm(prompt, want string) (bool, error) {
	if a.interactive == nil || !a.interactive() {
		return false, errors.New("confirmation needs an interactive terminal")
	}
	fmt.Fprint(a.stderr, prompt)
	line, err := bufio.NewReader(a.stdin).ReadString('\n')
	if err != nil && line == "" {
		return false, nil
	}
	line = strings.TrimSpace(line)
	if want == "" {
		return strings.HasPrefix(strings.ToLower(line), "y"), nil
	}
	return line == want, nil
}

// mountedOnly points AbsPath at the current mount and clears it for files on volumes
// that are not mounted, so nothing touches a stale path.
func (a *app) mountedOnly(files []catalog.File) map[string]volume.Volume {
	vols := map[string]volume.Volume{}
	missing := map[string]bool{}
	for i := range files {
		f := &files[i]
		v, ok := vols[f.VolumeKey]
		if !ok && !missing[f.VolumeKey] {
			if v, ok = a.res.Lookup(f.VolumeKey); ok {
				vols[f.VolumeKey] = v
			} else {
				missing[f.VolumeKey] = true
			}
		}
		if !ok {
			f.AbsPath = ""
			continue
		}
		f.AbsPath, _ = v.AbsPath(f.RelPath)
	}
	return vols
}
