// Package config loads ~/.config/mediacat.yaml, creating a commented default on first run.
package config

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/njhoffman/mediacat/internal/filter"
)

// Template is written when no config exists. It must parse to Default(). When changing it,
// add the SHA-256 of the version being replaced to previousTemplates so unedited copies
// of it are upgraded.
const Template = `# mediacat configuration. Command-line flags override everything here.

# Catalog database. Kept on local disk so listings work while drives are unplugged.
catalog: ~/.local/share/mediacat/catalog.db

# Warn when the catalog for a path is older than this many days (0 disables).
stale_days: 30

# Stash config, read only: to remind you to run Clean after changing files in its
# libraries, and to reuse the video phashes in its database for dupes --similar.
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

check:
  # quick decodes the first frame and reads the data near the end (catches truncated
  # files, reads little); full decodes everything (finds damage anywhere, reads it all).
  level: quick
  # Parallel checks; 0 picks 4, or 2 for full checks on network or spinning disks.
  workers: 0

dupes:
  # size, partial or full. Only full can trash, move or delete.
  depth: full
  # Rules choosing the copy to keep, in order:
  # prefer, avoid, paths, plain, oldest, newest, shortest, longest, res, bitrate, size
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
  # Default destination for "move" in dupes --review and when --move-to is not given.
  move_to: ""
  # Ignore files smaller than this.
  min_size: 1
  # Parallel hashing; 0 picks 2 on network volumes, 1 on spinning disks, 4 otherwise.
  workers: 0
  # Groups shown in the report (0 for all).
  limit: 20
  # dupes --similar: perceptual matches (videos and images that look alike). Level used
  # when --similar has no value: exact, high, medium or low (up to 0, 4, 8 or 10 of 64
  # phash bits different), or a number of bits.
  similar: high
  # Keep rules for similar groups. The copies differ, so quality comes first.
  similar_keep: res,bitrate,prefer,avoid,paths,plain,size,oldest
  # Videos are compared only when their durations differ by at most this percentage of
  # the longer one, or similar_duration_min, whichever is larger.
  similar_duration: 5
  similar_duration_min: 5s
  # Images narrower or shorter than this many pixels are not compared (icons, thumbnails).
  min_image_size: 64
  # Parallel perceptual hashing. It is CPU-bound (25 frame decodes per video), so 0 picks
  # half the CPUs.
  phash_workers: 0

list:
  # Fields for sort and columns: path name dir ext type size duration res width height fps
  # bitrate vbitrate bpp abitrate codec vcodec acodec container channels samplerate mtime
  # scanned artist album title track year genre error check dup phash
  #
  # Default sort: comma-separated fields, "-" prefix for descending, e.g. -size,duration
  sort: path
  # Columns shown by default, and for --type audio. "dup" marks files with a known identical
  # copy (found by mediacat dupes).
  columns: dup,size,duration,res,codec,vbitrate,bpp,path
  audio_columns: dup,size,duration,acodec,abitrate,artist,title,path
  # Values above "warn" are tinted pale yellow, shading to red at "max" and beyond (with a
  # finer gradient on 24-bit color terminals).
  # Leave warn empty to turn highlighting off for that column.
  highlight:
    size:
      warn: 1G
      max: 4G
    duration:
      warn: 1h
      max: 3h
    resolution:
      warn: 1920x1080
      max: 3840x2160
    # Video stream bitrate. 10 Mb/s is already generous for 1080p (streaming services use
    # 5-8); 4K remuxes run 40-80, so 50 Mb/s and above gets the strongest color.
    vbitrate:
      warn: 10M
      max: 50M
    # Video bits per pixel per frame: how heavily a video is encoded at any resolution.
    # Typical H.264 encodes are 0.05-0.1, so 0.15 is generous and 0.4 is bloated.
    bpp:
      warn: 0.15
      max: 0.4
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
	Check struct {
		Level   string `yaml:"level"`
		Workers int    `yaml:"workers"`
	} `yaml:"check"`
	Dupes struct {
		Depth          string   `yaml:"depth"`
		Keep           string   `yaml:"keep"`
		Prefer         []string `yaml:"prefer"`
		Avoid          []string `yaml:"avoid"`
		PreferredPaths []string `yaml:"preferred_paths"`
		MoveTo         string   `yaml:"move_to"`
		MinSize        string   `yaml:"min_size"`
		Workers        int      `yaml:"workers"`
		Limit          int      `yaml:"limit"`
		Similar        string   `yaml:"similar"`
		SimilarKeep    string   `yaml:"similar_keep"`
		SimilarDurPct  float64  `yaml:"similar_duration"`
		SimilarDurMin  string   `yaml:"similar_duration_min"`
		MinImageSize   int      `yaml:"min_image_size"`
		PHashWorkers   int      `yaml:"phash_workers"`
	} `yaml:"dupes"`
	List struct {
		Sort         string `yaml:"sort"`
		Columns      string `yaml:"columns"`
		AudioColumns string `yaml:"audio_columns"`
		Highlight    struct {
			Size       Range `yaml:"size"`
			Duration   Range `yaml:"duration"`
			Resolution Range `yaml:"resolution"`
			VBitrate   Range `yaml:"vbitrate"`
			BPP        Range `yaml:"bpp"`
		} `yaml:"highlight"`
	} `yaml:"list"`

	// Parsed values, filled by Load.
	ProbeTimeout       time.Duration `yaml:"-"`
	SimilarDurationMin time.Duration `yaml:"-"`
	Highlights         Highlights    `yaml:"-"`
	Path               string        `yaml:"-"`
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
	VBitrate   Scale // bits/s
	BPP        Scale // video bits per pixel per frame
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

// previousTemplates holds the SHA-256 of every Template an earlier release wrote. A file
// that still matches one was never edited, so it is safe to replace with the current one.
var previousTemplates = map[string]bool{
	"d85d50d42b7e5317dea4d4e94051fc8307e8c392da956df3f3d30ae63b53fc1c": true,
	"c5f5aad43b613cc32fd10741a45944ad4ac87265eff377e6e55d05eeb5a151ac": true,
	"82a6812b1af27d67afc60b5eedfaa85c59fd05a7baa6e0c295a524ba1772817f": true,
	"619f4449a73fbc916ace72615830b6c9cfc0e5ece35f4820080542f9e9ea4af5": true,
	"c539fe87dffa52429da781eba1a4053ee19cd183178403521a204085dd52f47e": true,
}

// Status reports what Load did to the file.
type Status int

// Load outcomes.
const (
	Loaded   Status = iota // read as is
	Created                // written with the defaults
	Upgraded               // an unedited older default replaced with the current one
)

// Load reads the config at path, writing the default template first when it does not
// exist and refreshing an unedited default from an earlier release. If the file cannot
// be written the defaults are still used.
func Load(path string) (cfg Config, status Status, err error) {
	b, err := os.ReadFile(path)
	if err == nil && previousTemplates[fmt.Sprintf("%x", sha256.Sum256(b))] {
		if os.WriteFile(path, []byte(Template), 0o644) == nil {
			b, status = []byte(Template), Upgraded
		}
	}
	if errors.Is(err, os.ErrNotExist) {
		b = []byte(Template)
		if mkErr := os.MkdirAll(filepath.Dir(path), 0o755); mkErr == nil && os.WriteFile(path, b, 0o644) == nil {
			status = Created
		}
	} else if err != nil {
		return cfg, Loaded, err
	}
	cfg, err = parse(b)
	if err != nil {
		return cfg, Loaded, fmt.Errorf("%s: %w", path, err)
	}
	cfg.Path = path
	return cfg, status, nil
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
	c.Dupes.MoveTo = Expand(c.Dupes.MoveTo)
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
	if c.Dupes.SimilarDurMin != "" {
		d, err := time.ParseDuration(c.Dupes.SimilarDurMin)
		if err != nil || d < 0 {
			return fmt.Errorf("dupes.similar_duration_min: want a duration like 5s, got %q", c.Dupes.SimilarDurMin)
		}
		c.SimilarDurationMin = d
	}
	if c.Dupes.SimilarDurPct < 0 || c.Dupes.SimilarDurPct > 100 {
		return fmt.Errorf("dupes.similar_duration: want a percentage from 0 to 100, got %v", c.Dupes.SimilarDurPct)
	}
	for name, cols := range map[string]string{"list.columns": c.List.Columns, "list.audio_columns": c.List.AudioColumns} {
		for _, col := range strings.Split(cols, ",") {
			if col = strings.TrimSpace(col); col != "" && !filter.IsField(col) {
				return fmt.Errorf("%s: unknown field %q", name, col)
			}
		}
	}
	if c.Check.Level != "quick" && c.Check.Level != "full" {
		return fmt.Errorf("check.level: want quick or full, got %q", c.Check.Level)
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
	if c.Highlights.VBitrate, err = scale("vbitrate", h.VBitrate, func(s string) (float64, error) {
		n, err := filter.ParseBitrate(s)
		return float64(n), err
	}); err != nil {
		return err
	}
	if c.Highlights.BPP, err = scale("bpp", h.BPP, func(s string) (float64, error) {
		n, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("want a positive number like 0.15, got %q", s)
		}
		return n, nil
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
