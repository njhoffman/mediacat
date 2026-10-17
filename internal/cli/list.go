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
	all                    bool
	asJSON                 bool
	kind                   string
	minSize, maxSize       string
	minDur, maxDur         string
	minRes, maxRes         string
	codecs, exts           []string
	sort                   string
	desc, missing, errOnly bool
	limit                  int
}

func (a *app) listCmd() *cobra.Command {
	var lf listFlags
	cmd := &cobra.Command{
		Use:   "list [path]",
		Short: "List cataloged media under path (default: current directory)",
		Long: `List cataloged media under path (default: current directory), read from the
catalog without touching the files. Use --all to list every volume, including ones
that are not mounted.

Sizes take binary units (500M, 1.5G). Durations take seconds, m:ss, h:mm:ss or Go
durations (90, 5:00, 1:30:00, 1h30m). Resolutions compare the short side of the
frame, so portrait and landscape video match alike (720p, 1080p, 4k, 1920x1080).`,
		Example: `  mediacat list --type video --min-res 1080p --min-duration 10m
  mediacat list --type audio --codec mp3 --sort size --desc --limit 20
  mediacat list --all --missing
  mediacat list /mnt/nfs/Stash --ext mkv,avi --json`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			flt, err := lf.parse()
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
	f.StringSliceVar(&lf.codecs, "codec", nil, "video or audio codec, e.g. h264,hevc,mp3 (repeatable)")
	f.StringSliceVar(&lf.exts, "ext", nil, "file extension, e.g. mkv,avi (repeatable)")
	f.StringVar(&lf.sort, "sort", "path", "sort by "+strings.Join(filter.SortKeys, ", "))
	f.BoolVar(&lf.desc, "desc", false, "sort descending")
	f.IntVarP(&lf.limit, "limit", "n", 0, "show at most this many files")
	f.BoolVar(&lf.missing, "missing", false, "only files the last scan did not find on disk")
	f.BoolVar(&lf.errOnly, "errors", false, "only files that could not be probed")
	return cmd
}

func (lf listFlags) parse() (filter.Filter, error) {
	f := filter.Filter{Kind: strings.ToLower(lf.kind), Sort: lf.sort, Desc: lf.desc, Limit: lf.limit, Missing: lf.missing,
		Codecs: filter.SplitList(lf.codecs), Exts: filter.SplitList(lf.exts)}
	var err error
	parse := func(s string, fn func(string) (int64, error), dst *int64) {
		if s != "" && err == nil {
			*dst, err = fn(s)
		}
	}
	parse(lf.minSize, filter.ParseSize, &f.MinSize)
	parse(lf.maxSize, filter.ParseSize, &f.MaxSize)
	parse(lf.minDur, filter.ParseDuration, &f.MinDurationMS)
	parse(lf.maxDur, filter.ParseDuration, &f.MaxDurationMS)
	res := func(s string, dst *int) {
		if s != "" && err == nil {
			*dst, err = filter.ParseRes(s)
		}
	}
	res(lf.minRes, &f.MinRes)
	res(lf.maxRes, &f.MaxRes)
	if err != nil {
		return f, err
	}
	return f, f.Validate()
}

func (a *app) runList(dir string, lf listFlags, flt filter.Filter) error {
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
	// --errors filters in Go: it's a display concern and keeps the catalog query simple.
	files, err := a.db.Query(volID, prefix, flt)
	if err != nil {
		return err
	}
	if lf.errOnly {
		kept := files[:0]
		for _, f := range files {
			if f.ProbeError != "" {
				kept = append(kept, f)
			}
		}
		files = kept
	}
	a.remap(files)
	if lf.asJSON {
		return a.listJSON(files)
	}
	a.listTable(files, flt, base)
	return nil
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

func (a *app) listTable(files []catalog.File, flt filter.Filter, base string) {
	if len(files) == 0 {
		fmt.Fprintln(a.stderr, ui.Dim.Render("no matching files"))
		return
	}
	audio := flt.Kind == filter.KindAudio
	cols := []ui.Column{
		{Title: "SIZE", Right: true},
		{Title: "DURATION", Right: true},
	}
	if audio {
		cols = append(cols, ui.Column{Title: "CODEC"}, ui.Column{Title: "BITRATE", Right: true},
			ui.Column{Title: "ARTIST", Style: ui.Accent}, ui.Column{Title: "TITLE"})
	} else {
		cols = append(cols, ui.Column{Title: "RES", Right: true}, ui.Column{Title: "CODEC"})
	}
	cols = append(cols, ui.Column{Title: "PATH", Flex: true})

	errStyle := ui.Err
	missStyle := ui.Warn
	var total int64
	rows := make([][]ui.Cell, 0, len(files))
	for _, f := range files {
		total += f.Size
		m := f.Meta
		codec := ui.Cell{Text: strings.Trim(m.VideoCodec+"/"+m.AudioCodec, "/")}
		if audio {
			codec.Text = m.AudioCodec
		}
		if f.ProbeError != "" {
			codec = ui.Cell{Text: "error", Style: &errStyle}
		}
		cells := []ui.Cell{{Text: ui.Bytes(f.Size)}, {Text: ui.Duration(m.DurationMS)}}
		if audio {
			cells = append(cells, codec, ui.Cell{Text: ui.Bitrate(m.Bitrate)},
				ui.Cell{Text: m.Tags.Artist}, ui.Cell{Text: m.Tags.Title})
		} else {
			cells = append(cells, ui.Cell{Text: ui.Res(m.Width, m.Height)}, codec)
		}
		p := displayPath(f, base)
		pc := ui.Cell{Text: p}
		if f.MissingSince != nil {
			pc.Style = &missStyle
		}
		rows = append(rows, append(cells, pc))
	}
	ui.Table(a.stdout, cols, rows, ui.Width(a.stdout))
	fmt.Fprintln(a.stderr, ui.Dim.Render(fmt.Sprintf("%s files, %s", ui.Count(len(files)), ui.Bytes(total))))
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
	FPS          float64    `json:"fps,omitempty"`
	SampleRate   int        `json:"sample_rate,omitempty"`
	Channels     int        `json:"channels,omitempty"`
	Tags         *tagsJSON  `json:"tags,omitempty"`
	ProbeError   string     `json:"probe_error,omitempty"`
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
		Bitrate: m.Bitrate, FPS: m.FPS, SampleRate: m.SampleRate, Channels: m.Channels,
		ProbeError: f.ProbeError, ScannedAt: f.ScannedAt.UTC(), MissingSince: f.MissingSince}
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
