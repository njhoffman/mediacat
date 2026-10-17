// Package cli wires the mediacat commands.
package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/njhoffman/mediacat/internal/catalog"
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
	stdout    io.Writer
	stderr    io.Writer
	now       func() time.Time

	db  *catalog.DB
	res *volume.Resolver
}

// Execute runs the CLI and returns the process exit code.
func Execute(version string) int {
	a := &app{stdout: os.Stdout, stderr: os.Stderr, now: time.Now}
	return a.run(version, os.Args[1:])
}

func (a *app) run(version string, args []string) int {
	root := a.rootCmd(version)
	root.SetArgs(args)
	root.SetOut(a.stdout)
	root.SetErr(a.stderr)
	err := root.Execute()
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

func (a *app) rootCmd(version string) *cobra.Command {
	staleDefault := 30
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
	f.StringVar(&a.dbPath, "db", catalog.DefaultPath(), "catalog database path")
	f.IntVar(&a.staleDays, "stale-days", staleDefault, "warn when the catalog for a path is older than this many days (0 disables; env MEDIACAT_STALE_DAYS)")
	f.BoolVar(&a.noColor, "no-color", false, "disable colored output (also honors NO_COLOR)")
	root.AddCommand(a.scanCmd(), a.listCmd(), a.infoCmd())
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
