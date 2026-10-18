// Package config loads ~/.config/mediacat.yaml, creating a commented default on first run.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/njhoffman/mediacat/internal/filter"
)

// Template is written when no config exists. It must parse to Default().
const Template = `# mediacat configuration. Command-line flags override everything here.

# Catalog database. Kept on local disk so listings work while drives are unplugged.
catalog: ~/.local/share/mediacat/catalog.db

# Warn when the catalog for a path is older than this many days (0 disables).
stale_days: 30

# Stash config, read only to remind you to run Clean after changing files in its libraries.
stash_config: ~/.stash/config.yml

scan:
  # Parallel probes; 0 picks 2 for network or spinning disks, up to 8 otherwise.
  workers: 0
  # Give up on one file after this long (it is retried on the next scan).
  probe_timeout: 2m
  # Directory and file names never scanned (globs). Trash folders are listed so trashed
  # files are never reported as duplicates.
  exclude:
    - .Trash
    - .Trash-*
    - .Trashes
    - trashbox
    - $RECYCLE.BIN
    - System Volume Information
    - "@eaDir"
    - "#recycle"
    - lost+found

dupes:
  # size, partial or full. Only full can trash, move or delete.
  depth: full
  # Rules choosing the copy to keep, in order:
  # prefer, avoid, paths, plain, oldest, newest, shortest, longest
  keep: prefer,avoid,paths,plain,oldest,shortest
  # Keep copies under these directories (first wins) / avoid keeping copies under these.
  prefer: []
  avoid: []
  # For the "paths" rule: keep the copy whose path contains one of these (case-insensitive,
  # earlier entries win). A leading "^" means avoid keeping copies whose path contains it.
  # "/sorted" matches a folder named Sorted but not "Unsorted"; plain "sorted" would match both.
  preferred_paths:
    - /sorted
    - "^incoming"
  # Ignore files smaller than this.
  min_size: 1
  # Parallel hashing; 0 picks 2 on network volumes, 1 on spinning disks, 4 otherwise.
  workers: 0
  # Groups shown in the report (0 for all).
  limit: 20

list:
  # Fields for sort and columns: path name dir ext type size duration res width height fps
  # bitrate vbitrate abitrate codec vcodec acodec container channels samplerate mtime
  # scanned artist album title track year genre error
  #
  # Default sort: comma-separated fields, "-" prefix for descending, e.g. -size,duration
  sort: path
  # Columns shown by default, and for --type audio.
  columns: size,duration,res,codec,path
  audio_columns: size,duration,acodec,abitrate,artist,title,path
  # Values above "warn" are tinted yellow, shading to bold red at "max" and beyond.
  # Leave warn empty to turn highlighting off for that column.
  highlight:
    size:
      warn: 500M
      max: 4G
    duration:
      warn: 30m
      max: 2h
    resolution:
      warn: 1920x1080
      max: 3840x2160
`

// Range is a highlight threshold pair as written in the file.
type Range struct {
	Warn string `yaml:"warn"`
	Max  string `yaml:"max"`
}

// Config mirrors the YAML file.
type Config struct {
	Catalog     string `yaml:"catalog"`
	StaleDays   int    `yaml:"stale_days"`
	StashConfig string `yaml:"stash_config"`
	Scan        struct {
		Workers      int      `yaml:"workers"`
		ProbeTimeout string   `yaml:"probe_timeout"`
		Exclude      []string `yaml:"exclude"`
	} `yaml:"scan"`
	Dupes struct {
		Depth          string   `yaml:"depth"`
		Keep           string   `yaml:"keep"`
		Prefer         []string `yaml:"prefer"`
		Avoid          []string `yaml:"avoid"`
		PreferredPaths []string `yaml:"preferred_paths"`
		MinSize        string   `yaml:"min_size"`
		Workers        int      `yaml:"workers"`
		Limit          int      `yaml:"limit"`
	} `yaml:"dupes"`
	List struct {
		Sort         string `yaml:"sort"`
		Columns      string `yaml:"columns"`
		AudioColumns string `yaml:"audio_columns"`
		Highlight    struct {
			Size       Range `yaml:"size"`
			Duration   Range `yaml:"duration"`
			Resolution Range `yaml:"resolution"`
		} `yaml:"highlight"`
	} `yaml:"list"`

	// Parsed values, filled by Load.
	ProbeTimeout time.Duration `yaml:"-"`
	Highlights   Highlights    `yaml:"-"`
	Path         string        `yaml:"-"`
}

// Scale is a parsed warn/max pair. Enabled is false when warn is empty.
type Scale struct {
	Enabled   bool
	Warn, Max float64
}

// Level returns how far v is past warn: 0 at or below warn, rising to 1 at max and above.
func (s Scale) Level(v float64) float64 {
	if !s.Enabled || v <= s.Warn {
		return 0
	}
	if v >= s.Max {
		return 1
	}
	return (v - s.Warn) / (s.Max - s.Warn)
}

// Highlights holds the list thresholds. Resolution is compared by pixel count.
type Highlights struct {
	Size       Scale // bytes
	Duration   Scale // milliseconds
	Resolution Scale // width * height
}

// DefaultPath is $XDG_CONFIG_HOME/mediacat.yaml (~/.config/mediacat.yaml).
func DefaultPath() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "mediacat.yaml")
}

// Default returns the built-in configuration (identical to Template).
func Default() Config {
	c, err := parse(nil)
	if err != nil {
		panic("config template is invalid: " + err.Error())
	}
	return c
}

// Load reads the config at path, writing the default template first when it does not
// exist. created reports whether the file was written. If it cannot be written the
// defaults are used and err is nil but created is false.
func Load(path string) (cfg Config, created bool, err error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		b = []byte(Template)
		if mkErr := os.MkdirAll(filepath.Dir(path), 0o755); mkErr == nil {
			created = os.WriteFile(path, b, 0o644) == nil
		}
	} else if err != nil {
		return cfg, false, err
	}
	cfg, err = parse(b)
	if err != nil {
		return cfg, false, fmt.Errorf("%s: %w", path, err)
	}
	cfg.Path = path
	return cfg, created, nil
}

// parse decodes the template and then b over it, so keys missing from b keep their
// defaults rather than becoming zero values.
func parse(b []byte) (Config, error) {
	var c Config
	for i, doc := range [][]byte{[]byte(Template), b} {
		dec := yaml.NewDecoder(bytes.NewReader(doc))
		dec.KnownFields(true) // catch typos instead of silently ignoring them
		if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
			if i == 0 {
				panic("config template is invalid: " + err.Error())
			}
			return c, err
		}
	}
	return c, c.finish()
}

// finish expands paths and parses the typed values.
func (c *Config) finish() error {
	c.Catalog = Expand(c.Catalog)
	c.StashConfig = Expand(c.StashConfig)
	for i := range c.Dupes.Prefer {
		c.Dupes.Prefer[i] = Expand(c.Dupes.Prefer[i])
	}
	for i := range c.Dupes.Avoid {
		c.Dupes.Avoid[i] = Expand(c.Dupes.Avoid[i])
	}
	if c.Scan.ProbeTimeout != "" {
		d, err := time.ParseDuration(c.Scan.ProbeTimeout)
		if err != nil {
			return fmt.Errorf("scan.probe_timeout: %w", err)
		}
		c.ProbeTimeout = d
	}
	for name, cols := range map[string]string{"list.columns": c.List.Columns, "list.audio_columns": c.List.AudioColumns} {
		for _, col := range strings.Split(cols, ",") {
			if col = strings.TrimSpace(col); col != "" && !filter.IsField(col) {
				return fmt.Errorf("%s: unknown field %q", name, col)
			}
		}
	}
	if c.List.Sort != "" {
		if _, err := filter.ParseSort(c.List.Sort); err != nil {
			return fmt.Errorf("list.sort: %w", err)
		}
	}
	h := c.List.Highlight
	var err error
	if c.Highlights.Size, err = scale("size", h.Size, func(s string) (float64, error) {
		n, err := filter.ParseSize(s)
		return float64(n), err
	}); err != nil {
		return err
	}
	if c.Highlights.Duration, err = scale("duration", h.Duration, func(s string) (float64, error) {
		n, err := filter.ParseDuration(s)
		return float64(n), err
	}); err != nil {
		return err
	}
	c.Highlights.Resolution, err = scale("resolution", h.Resolution, func(s string) (float64, error) {
		w, hh, ok := strings.Cut(strings.ToLower(s), "x")
		var wi, hi int
		if _, e := fmt.Sscan(w, &wi); !ok || e != nil {
			return 0, fmt.Errorf("want WIDTHxHEIGHT, got %q", s)
		}
		if _, e := fmt.Sscan(hh, &hi); e != nil || wi <= 0 || hi <= 0 {
			return 0, fmt.Errorf("want WIDTHxHEIGHT, got %q", s)
		}
		return float64(wi * hi), nil
	})
	return err
}

func scale(name string, r Range, parse func(string) (float64, error)) (Scale, error) {
	if strings.TrimSpace(r.Warn) == "" {
		return Scale{}, nil
	}
	w, err := parse(r.Warn)
	if err != nil {
		return Scale{}, fmt.Errorf("list.highlight.%s.warn: %w", name, err)
	}
	if strings.TrimSpace(r.Max) == "" {
		return Scale{}, fmt.Errorf("list.highlight.%s.max is required when warn is set", name)
	}
	m, err := parse(r.Max)
	if err != nil {
		return Scale{}, fmt.Errorf("list.highlight.%s.max: %w", name, err)
	}
	if m <= w {
		return Scale{}, fmt.Errorf("list.highlight.%s: max must be larger than warn", name)
	}
	return Scale{Enabled: true, Warn: w, Max: m}, nil
}

// Expand replaces a leading ~ with the home directory.
func Expand(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}
