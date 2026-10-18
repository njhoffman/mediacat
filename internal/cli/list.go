package cli

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/njhoffman/mediacat/internal/catalog"
	"github.com/njhoffman/mediacat/internal/filter"
	"github.com/njhoffman/mediacat/internal/probe"
	"github.com/njhoffman/mediacat/internal/ui"
	"github.com/njhoffman/mediacat/internal/volume"
)

type listFlags struct {
	all, asJSON, desc, missing, errOnly bool
	kind                                string
	minSize, maxSize                    string
	minDur, maxDur                      string
	minRes, maxRes                      string
	minBR, maxBR                        string
	minVBR, maxVBR                      string
	minABR, maxABR                      string
	minFPS, maxFPS                      float64
	minYear, maxYear                    int
	codecs, vcodecs, acodecs            []string
	containers, exts                    []string
	name, path, artist, album           string
	title, genre                        string
	after, before                       string
	sort, columns                       string
	checks                              []string
	duplicates                          bool
	limit                               int
}

func (a *app) listCmd() *cobra.Command {
	var lf listFlags
	cmd := &cobra.Command{
		Use:   "list [path]",
		Short: "List cataloged media under path (default: current directory)",
		Long: `List cataloged media under path (default: current directory), read from the
catalog without touching the files. Use --all to list every volume, including ones
that are not mounted.

Fields (for --sort and --columns):
  ` + wrapFields() + `

Sorting: --sort takes comma-separated fields, "-" prefix for descending
(--sort -size,duration). Files missing a value always sort last. Fields you sort by
are added to the table when they are not already shown.

Columns: --columns size,duration,path replaces the columns; +field adds to and
-field removes from the defaults (--columns +vbitrate,+abitrate,-codec).

Sizes take binary units (500M, 1.5G). Durations take seconds, m:ss, h:mm:ss or Go
durations (90, 5:00, 1:30:00, 1h30m). Resolutions compare the short side of the
frame (720p, 1080p, 4k, 1920x1080). Bitrates take decimal units (128k, 4.5M).
Dates take 2026-01-31 or an age (7d, 2w, 3mo). Text filters match anywhere,
ignoring case.

Size, duration, resolution and video bitrate cells are tinted from pale yellow to red
as values exceed the list.highlight thresholds in the config. The first column marks
files with a known identical copy (` + dupMarker + `), from the full hashes mediacat dupes computes.`,
		Example: `  mediacat list --type video --min-res 1080p --min-duration 10m --sort -size
  mediacat list --type audio --artist beatles --sort album,track --columns +album,+track
  mediacat list --vcodec mpeg4,wmv3 --columns name,size,vcodec,vbitrate --sort -vbitrate
  mediacat list --min-vbitrate 20M --sort -vbitrate
  mediacat list --modified-after 7d --sort -mtime
  mediacat list --all --missing --json`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			flt, err := lf.parse(a.now())
			if err != nil {
				return err
			}
			if err := a.open(); err != nil {
				return err
			}
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			return a.runList(dir, lf, flt)
		},
	}
	f := cmd.Flags()
	f.BoolVar(&lf.all, "all", false, "list every cataloged volume instead of path")
	f.BoolVar(&lf.asJSON, "json", false, "print JSON")
	f.StringVarP(&lf.kind, "type", "t", "", "video, audio or image")
	f.StringVar(&lf.minSize, "min-size", "", "minimum file size, e.g. 500M")
	f.StringVar(&lf.maxSize, "max-size", "", "maximum file size, e.g. 2G")
	f.StringVar(&lf.minDur, "min-duration", "", "minimum duration, e.g. 5m or 1:30:00")
	f.StringVar(&lf.maxDur, "max-duration", "", "maximum duration")
	f.StringVar(&lf.minRes, "min-res", "", "minimum resolution, e.g. 720p, 4k, 1920x1080")
	f.StringVar(&lf.maxRes, "max-res", "", "maximum resolution")
	f.StringVar(&lf.minBR, "min-bitrate", "", "minimum overall bitrate, e.g. 5M")
	f.StringVar(&lf.maxBR, "max-bitrate", "", "maximum overall bitrate")
	f.StringVar(&lf.minVBR, "min-vbitrate", "", "minimum video stream bitrate")
	f.StringVar(&lf.maxVBR, "max-vbitrate", "", "maximum video stream bitrate")
	f.StringVar(&lf.minABR, "min-abitrate", "", "minimum audio stream bitrate, e.g. 192k")
	f.StringVar(&lf.maxABR, "max-abitrate", "", "maximum audio stream bitrate")
	f.Float64Var(&lf.minFPS, "min-fps", 0, "minimum frame rate")
	f.Float64Var(&lf.maxFPS, "max-fps", 0, "maximum frame rate")
	f.IntVar(&lf.minYear, "min-year", 0, "minimum tag year")
	f.IntVar(&lf.maxYear, "max-year", 0, "maximum tag year")
	f.StringSliceVar(&lf.codecs, "codec", nil, "video or audio codec, e.g. h264,hevc,mp3 (repeatable)")
	f.StringSliceVar(&lf.vcodecs, "vcodec", nil, "video codec (repeatable)")
	f.StringSliceVar(&lf.acodecs, "acodec", nil, "audio codec (repeatable)")
	f.StringSliceVar(&lf.containers, "container", nil, "container format, e.g. matroska,mp4 (repeatable)")
	f.StringSliceVar(&lf.exts, "ext", nil, "file extension, e.g. mkv,avi (repeatable)")
	f.StringVar(&lf.name, "name", "", "file name contains")
	f.StringVar(&lf.path, "path-contains", "", "path contains")
	f.StringVar(&lf.artist, "artist", "", "artist tag contains")
	f.StringVar(&lf.album, "album", "", "album tag contains")
	f.StringVar(&lf.title, "title", "", "title tag contains")
	f.StringVar(&lf.genre, "genre", "", "genre tag contains")
	f.StringVar(&lf.after, "modified-after", "", "modified on or after, e.g. 2026-01-31 or 7d")
	f.StringVar(&lf.before, "modified-before", "", "modified on or before")
	f.StringVar(&lf.sort, "sort", a.cfg.List.Sort, `fields to sort by, "-" for descending, e.g. -size,duration`)
	f.BoolVar(&lf.desc, "desc", false, "reverse every sort field")
	f.StringVar(&lf.columns, "columns", "", "columns to show; +field/-field adjusts the defaults")
	f.IntVarP(&lf.limit, "limit", "n", 0, "show at most this many files")
	f.BoolVar(&lf.missing, "missing", false, "only files the last scan did not find on disk")
	f.BoolVar(&lf.errOnly, "errors", false, "only files that could not be probed")
	f.BoolVar(&lf.duplicates, "duplicates", false, "only files with a known identical copy (marked "+dupMarker+"; found by mediacat dupes)")
	f.StringSliceVar(&lf.checks, "check", nil, "integrity status: "+strings.Join(filter.CheckStatuses, ", ")+" (repeatable)")
	return cmd
}

func wrapFields() string {
	var lines []string
	var cur []string
	for _, f := range filter.Fields {
		cur = append(cur, f)
		if len(strings.Join(cur, " ")) > 72 {
			lines = append(lines, strings.Join(cur[:len(cur)-1], " "))
			cur = []string{f}
		}
	}
	return strings.Join(append(lines, strings.Join(cur, " ")), "\n  ")
}

func (lf listFlags) parse(now time.Time) (filter.Filter, error) {
	f := filter.Filter{Kind: strings.ToLower(lf.kind), Limit: lf.limit, Missing: lf.missing, Errors: lf.errOnly,
		Codecs: filter.SplitList(lf.codecs), VCodecs: filter.SplitList(lf.vcodecs), ACodecs: filter.SplitList(lf.acodecs),
		Containers: filter.SplitList(lf.containers), Exts: filter.SplitList(lf.exts),
		MinFPS: lf.minFPS, MaxFPS: lf.maxFPS, MinYear: lf.minYear, MaxYear: lf.maxYear,
		Name: lf.name, Path: lf.path, Artist: lf.artist, Album: lf.album, Title: lf.title, Genre: lf.genre,
		Checks: filter.SplitList(lf.checks), Duplicates: lf.duplicates}
	var err error
	num := func(s string, fn func(string) (int64, error), dst *int64) {
		if s != "" && err == nil {
			*dst, err = fn(s)
		}
	}
	num(lf.minSize, filter.ParseSize, &f.MinSize)
	num(lf.maxSize, filter.ParseSize, &f.MaxSize)
	num(lf.minDur, filter.ParseDuration, &f.MinDurationMS)
	num(lf.maxDur, filter.ParseDuration, &f.MaxDurationMS)
	num(lf.minBR, filter.ParseBitrate, &f.MinBitrate)
	num(lf.maxBR, filter.ParseBitrate, &f.MaxBitrate)
	num(lf.minVBR, filter.ParseBitrate, &f.MinVBitrate)
	num(lf.maxVBR, filter.ParseBitrate, &f.MaxVBitrate)
	num(lf.minABR, filter.ParseBitrate, &f.MinABitrate)
	num(lf.maxABR, filter.ParseBitrate, &f.MaxABitrate)
	res := func(s string, dst *int) {
		if s != "" && err == nil {
			*dst, err = filter.ParseRes(s)
		}
	}
	res(lf.minRes, &f.MinRes)
	res(lf.maxRes, &f.MaxRes)
	when := func(s string, dst *time.Time) {
		if s != "" && err == nil {
			*dst, err = filter.ParseTime(s, now)
		}
	}
	when(lf.after, &f.ModifiedAfter)
	when(lf.before, &f.ModifiedBefore)
	if err == nil {
		f.Sorts, err = filter.ParseSort(lf.sort)
	}
	if err != nil {
		return f, err
	}
	if lf.desc {
		for i := range f.Sorts {
			f.Sorts[i].Desc = !f.Sorts[i].Desc
		}
		if len(f.Sorts) == 0 {
			f.Sorts = []filter.SortKey{{Key: filter.FieldPath, Desc: true}}
		}
	}
	// Plain path order is what the catalog returns already; skip the extra sort.
	if len(f.Sorts) == 1 && f.Sorts[0] == (filter.SortKey{Key: filter.FieldPath}) {
		f.Sorts = nil
	}
	return f, f.Validate()
}

func (a *app) runList(dir string, lf listFlags, flt filter.Filter) error {
	defaults := a.cfg.List.Columns
	if flt.Kind == filter.KindAudio {
		defaults = a.cfg.List.AudioColumns
	}
	cols, err := resolveColumns(defaults, lf.columns, flt.Sorts)
	if err != nil {
		return err
	}
	var volID int64
	var prefix, base string
	if !lf.all {
		v, rel, err := a.res.Resolve(dir)
		if err != nil {
			return err
		}
		base, _ = v.AbsPath(rel)
		stored, err := a.db.VolumeByKey(v.Key)
		if err != nil {
			return err
		}
		if stored == nil {
			a.warn(ui.Stale(nil, a.staleDays, a.now(), base))
			if lf.asJSON {
				fmt.Fprintln(a.stdout, "[]")
			}
			return nil
		}
		volID, prefix = stored.ID, rel
		if err := a.staleness(volID, rel, base); err != nil {
			return err
		}
	}
	files, err := a.db.Query(volID, prefix, flt)
	if err != nil {
		return err
	}
	a.remap(files)
	if lf.asJSON {
		return a.listJSON(files)
	}
	a.listTable(files, cols, base)
	return nil
}

func (a *app) listTable(files []catalog.File, fields []string, base string) {
	if len(files) == 0 {
		fmt.Fprintln(a.stderr, ui.Dim.Render("no matching files"))
		return
	}
	fields = dropEmptyDup(fields, files)
	flex := flexColumn(fields)
	cols := make([]ui.Column, len(fields))
	for i, name := range fields {
		def := columnDefs[name]
		cols[i] = ui.Column{Title: def.title, Right: def.right, Flex: name == flex, Style: def.style}
	}
	h := a.cfg.Highlights
	var total int64
	rows := make([][]ui.Cell, 0, len(files))
	for _, f := range files {
		total += f.Size
		row := make([]ui.Cell, len(fields))
		for i, name := range fields {
			row[i] = cellFor(name, f, base, h)
		}
		rows = append(rows, row)
	}
	ui.Table(a.stdout, cols, rows, ui.Width(a.stdout))
	fmt.Fprintln(a.stderr, ui.Dim.Render(fmt.Sprintf("%s files, %s", ui.Count(len(files)), ui.Bytes(total))))
}

// remap rewrites AbsPath for volumes now mounted somewhere other than their last mount.
func (a *app) remap(files []catalog.File) {
	cache := map[string][2]string{}
	for i := range files {
		f := &files[i]
		m, ok := cache[f.VolumeKey]
		if !ok {
			if v, mounted := a.res.Lookup(f.VolumeKey); mounted {
				m = [2]string{v.MountPoint, v.MountRoot}
			}
			cache[f.VolumeKey] = m
		}
		if m[0] != "" {
			f.AbsPath, _ = volume.AbsPath(m[0], m[1], f.RelPath)
		}
	}
}

// displayPath shows files relative to the listed directory, or absolute with --all.
func displayPath(f catalog.File, base string) string {
	p := f.AbsPath
	if p == "" {
		return f.VolumeKey + ":" + f.RelPath
	}
	if base != "" {
		if rel, err := filepath.Rel(base, p); err == nil && !strings.HasPrefix(rel, "..") {
			return rel
		}
	}
	return p
}

type fileJSON struct {
	Path         string     `json:"path"`
	Volume       string     `json:"volume"`
	RelPath      string     `json:"relpath"`
	Kind         string     `json:"kind"`
	Size         int64      `json:"size"`
	Modified     time.Time  `json:"modified"`
	Container    string     `json:"container,omitempty"`
	DurationMS   int64      `json:"duration_ms,omitempty"`
	Width        int        `json:"width,omitempty"`
	Height       int        `json:"height,omitempty"`
	VideoCodec   string     `json:"video_codec,omitempty"`
	AudioCodec   string     `json:"audio_codec,omitempty"`
	Bitrate      int64      `json:"bitrate,omitempty"`
	VideoBitrate int64      `json:"video_bitrate,omitempty"`
	AudioBitrate int64      `json:"audio_bitrate,omitempty"`
	FPS          float64    `json:"fps,omitempty"`
	SampleRate   int        `json:"sample_rate,omitempty"`
	Channels     int        `json:"channels,omitempty"`
	Tags         *tagsJSON  `json:"tags,omitempty"`
	ProbeError   string     `json:"probe_error,omitempty"`
	Check        *checkJSON `json:"check,omitempty"`
	PHash        string     `json:"phash,omitempty"`
	PHashSource  string     `json:"phash_source,omitempty"`
	PHashError   string     `json:"phash_error,omitempty"`
	ScannedAt    time.Time  `json:"scanned_at"`
	MissingSince *time.Time `json:"missing_since,omitempty"`
}

type tagsJSON struct {
	Artist string `json:"artist,omitempty"`
	Album  string `json:"album,omitempty"`
	Title  string `json:"title,omitempty"`
	Genre  string `json:"genre,omitempty"`
	Track  int    `json:"track,omitempty"`
	Year   int    `json:"year,omitempty"`
}

func toJSON(f catalog.File) fileJSON {
	m := f.Meta
	j := fileJSON{Path: f.AbsPath, Volume: f.VolumeKey, RelPath: f.RelPath, Kind: f.Kind, Size: f.Size,
		Modified: time.Unix(0, f.MtimeNS).UTC(), Container: m.Container, DurationMS: m.DurationMS,
		Width: m.Width, Height: m.Height, VideoCodec: m.VideoCodec, AudioCodec: m.AudioCodec,
		Bitrate: m.Bitrate, VideoBitrate: m.VideoBitrate, AudioBitrate: m.AudioBitrate, FPS: m.FPS, SampleRate: m.SampleRate, Channels: m.Channels,
		ProbeError: f.ProbeError, ScannedAt: f.ScannedAt.UTC(), MissingSince: f.MissingSince}
	if f.PHash.OK() {
		j.PHash = fmt.Sprintf("%016x", f.PHash.Value)
	}
	j.PHashSource, j.PHashError = f.PHash.Source, f.PHash.Error
	if f.Check.Status != "" {
		j.Check = &checkJSON{f.Check.Status, f.Check.Level, f.Check.Detail, f.Check.At.UTC()}
	}
	if t := m.Tags; t != (probe.Tags{}) {
		j.Tags = &tagsJSON{t.Artist, t.Album, t.Title, t.Genre, t.Track, t.Year}
	}
	return j
}

func (a *app) listJSON(files []catalog.File) error {
	out := make([]fileJSON, len(files))
	for i, f := range files {
		out[i] = toJSON(f)
	}
	enc := json.NewEncoder(a.stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

type checkJSON struct {
	Status    string    `json:"status"`
	Level     string    `json:"level"`
	Detail    string    `json:"detail,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
}

// dropEmptyDup removes the duplicate-marker column when no listed file has a known copy,
// so it does not leave an empty gutter.
func dropEmptyDup(fields []string, files []catalog.File) []string {
	i := indexOf(fields, filter.FieldDup)
	if i < 0 || len(fields) == 1 {
		return fields
	}
	for _, f := range files {
		if f.Copies > 1 {
			return fields
		}
	}
	return append(fields[:i:i], fields[i+1:]...)
}
