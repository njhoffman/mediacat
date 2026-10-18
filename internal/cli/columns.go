package cli

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/njhoffman/mediacat/internal/catalog"
	"github.com/njhoffman/mediacat/internal/config"
	"github.com/njhoffman/mediacat/internal/filter"
	"github.com/njhoffman/mediacat/internal/ui"
)

// dupMarker flags files with a known identical copy. A plain Unicode shape, not an emoji,
// so it renders in any terminal font.
const dupMarker = "◆"

type columnDef struct {
	title string
	right bool
	style lipgloss.Style
}

var columnDefs = map[string]columnDef{
	filter.FieldPath: {title: "PATH"}, filter.FieldName: {title: "NAME"}, filter.FieldDir: {title: "DIR"},
	filter.FieldExt: {title: "EXT"}, filter.FieldType: {title: "TYPE"},
	filter.FieldSize: {title: "SIZE", right: true}, filter.FieldDuration: {title: "DURATION", right: true},
	filter.FieldRes: {title: "RES", right: true}, filter.FieldWidth: {title: "WIDTH", right: true},
	filter.FieldHeight: {title: "HEIGHT", right: true}, filter.FieldFPS: {title: "FPS", right: true},
	filter.FieldBitrate: {title: "BITRATE", right: true}, filter.FieldVBitrate: {title: "V-BITRATE", right: true},
	filter.FieldABitrate: {title: "A-BITRATE", right: true}, filter.FieldCodec: {title: "CODEC"},
	filter.FieldVCodec: {title: "VCODEC"}, filter.FieldACodec: {title: "ACODEC"}, filter.FieldContainer: {title: "CONTAINER"},
	filter.FieldChannels: {title: "CH", right: true}, filter.FieldSampleRate: {title: "RATE", right: true},
	filter.FieldMtime: {title: "MODIFIED"}, filter.FieldScanned: {title: "SCANNED"},
	filter.FieldArtist: {title: "ARTIST", style: ui.Accent}, filter.FieldAlbum: {title: "ALBUM"},
	filter.FieldTitle: {title: "TITLE"}, filter.FieldTrack: {title: "TRACK", right: true},
	filter.FieldYear: {title: "YEAR", right: true}, filter.FieldGenre: {title: "GENRE"},
	filter.FieldError: {title: "ERROR", style: ui.Err},
	filter.FieldCheck: {title: "CHECK"},
	filter.FieldDup:   {title: ""},
}

func splitFields(spec string) []string {
	var out []string
	for _, p := range strings.Split(spec, ",") {
		if p = strings.ToLower(strings.TrimSpace(p)); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func indexOf(list []string, s string) int {
	for i, v := range list {
		if v == s {
			return i
		}
	}
	return -1
}

// insertBeforeFlex adds field before the wide text column so it stays last.
func insertBeforeFlex(cols []string, field string) []string {
	at := indexOf(cols, flexColumn(cols))
	if at < 0 {
		return append(cols, field)
	}
	return append(cols[:at], append([]string{field}, cols[at:]...)...)
}

// resolveColumns combines the default columns with --columns. A spec of plain fields
// replaces the defaults; +field/-field entries adjust them. Unless the columns were given
// outright, sorted fields that are not shown are added.
func resolveColumns(defaults, spec string, sorts []filter.SortKey) ([]string, error) {
	cols := splitFields(defaults)
	items := splitFields(spec)
	explicit := false
	if len(items) > 0 {
		adjust := strings.HasPrefix(items[0], "+") || strings.HasPrefix(items[0], "-")
		explicit = !adjust
		if explicit {
			cols = nil
		}
		for _, it := range items {
			isAdjust := strings.HasPrefix(it, "+") || strings.HasPrefix(it, "-")
			if isAdjust != adjust {
				return nil, fmt.Errorf("--columns: use either a full list or +field/-field adjustments, not both")
			}
			name := strings.TrimLeft(it, "+-")
			if !filter.IsField(name) {
				return nil, fmt.Errorf("--columns: unknown field %q (want %s)", name, strings.Join(filter.Fields, ", "))
			}
			switch {
			case strings.HasPrefix(it, "-"):
				if i := indexOf(cols, name); i >= 0 {
					cols = append(cols[:i], cols[i+1:]...)
				}
			case indexOf(cols, name) < 0 && adjust:
				cols = insertBeforeFlex(cols, name)
			case indexOf(cols, name) < 0:
				cols = append(cols, name)
			}
		}
	}
	if !explicit {
		for _, k := range sorts {
			if indexOf(cols, k.Key) < 0 {
				cols = insertBeforeFlex(cols, k.Key)
			}
		}
	}
	if len(cols) == 0 {
		return nil, fmt.Errorf("--columns: no columns left to show")
	}
	return cols, nil
}

// flexColumn picks the column that shrinks to fit the terminal: the first long text field.
func flexColumn(cols []string) string {
	for _, f := range []string{filter.FieldPath, filter.FieldName, filter.FieldDir, filter.FieldTitle, filter.FieldError} {
		if indexOf(cols, f) >= 0 {
			return f
		}
	}
	return ""
}

func cellFor(field string, f catalog.File, base string, h config.Highlights) ui.Cell {
	m := f.Meta
	errStyle := ui.Err
	missStyle := ui.Warn
	text := ""
	switch field {
	case filter.FieldPath:
		c := ui.Cell{Text: displayPath(f, base)}
		if f.MissingSince != nil {
			c.Style = &missStyle
		}
		return c
	case filter.FieldName:
		text = filepath.Base(f.RelPath)
	case filter.FieldDir:
		if p := displayPath(f, base); p != "" {
			text = filepath.Dir(p)
		}
	case filter.FieldExt:
		text = catalog.Ext(f.RelPath)
	case filter.FieldType:
		text = f.Kind
	case filter.FieldSize:
		return ui.Cell{Text: ui.Bytes(f.Size), Style: ui.Heat(h.Size.Level(float64(f.Size)))}
	case filter.FieldDuration:
		return ui.Cell{Text: ui.Duration(m.DurationMS), Style: ui.Heat(h.Duration.Level(float64(m.DurationMS)))}
	case filter.FieldRes:
		return ui.Cell{Text: ui.Res(m.Width, m.Height), Style: ui.Heat(h.Resolution.Level(float64(m.Width) * float64(m.Height)))}
	case filter.FieldWidth:
		text = intOrDash(m.Width)
	case filter.FieldHeight:
		text = intOrDash(m.Height)
	case filter.FieldFPS:
		text = "-"
		if m.FPS > 0 {
			text = fmt.Sprintf("%.2f", m.FPS)
		}
	case filter.FieldBitrate:
		text = ui.Bitrate(m.Bitrate)
	case filter.FieldVBitrate:
		return ui.Cell{Text: ui.Bitrate(m.VideoBitrate), Style: ui.Heat(h.VBitrate.Level(float64(m.VideoBitrate)))}
	case filter.FieldABitrate:
		text = ui.Bitrate(m.AudioBitrate)
	case filter.FieldCodec:
		if f.ProbeError != "" {
			return ui.Cell{Text: "error", Style: &errStyle}
		}
		text = strings.Trim(m.VideoCodec+"/"+m.AudioCodec, "/")
	case filter.FieldVCodec:
		text = m.VideoCodec
	case filter.FieldACodec:
		text = m.AudioCodec
	case filter.FieldContainer:
		text = m.Container
	case filter.FieldChannels:
		text = intOrDash(m.Channels)
	case filter.FieldSampleRate:
		text = "-"
		if m.SampleRate > 0 {
			text = fmt.Sprintf("%.1f kHz", float64(m.SampleRate)/1000)
		}
	case filter.FieldMtime:
		text = time.Unix(0, f.MtimeNS).Format("2006-01-02 15:04")
	case filter.FieldScanned:
		text = f.ScannedAt.Format("2006-01-02 15:04")
	case filter.FieldArtist:
		text = m.Tags.Artist
	case filter.FieldAlbum:
		text = m.Tags.Album
	case filter.FieldTitle:
		text = m.Tags.Title
	case filter.FieldTrack:
		text = intOrDash(m.Tags.Track)
	case filter.FieldYear:
		text = intOrDash(m.Tags.Year)
	case filter.FieldGenre:
		text = m.Tags.Genre
	case filter.FieldError:
		text = f.ProbeError
	case filter.FieldCheck:
		return checkCell(f.Check)
	case filter.FieldDup:
		if f.Copies > 1 {
			st := ui.Warn
			return ui.Cell{Text: dupMarker, Style: &st}
		}
		return ui.Cell{Text: ""}
	}
	return ui.Cell{Text: text}
}

func intOrDash(n int) string {
	if n <= 0 {
		return "-"
	}
	return fmt.Sprint(n)
}

// checkCell shows an integrity status, colored by severity, with the level for full checks.
func checkCell(c catalog.Check) ui.Cell {
	st := checkStyle(c.Status)
	text := c.Status
	switch {
	case text == "":
		text = "-"
	case c.Level == catalog.LevelFull:
		text += " (full)"
	}
	return ui.Cell{Text: text, Style: &st}
}

func checkStyle(status string) lipgloss.Style {
	switch status {
	case catalog.CheckOK:
		return ui.OK
	case catalog.CheckDamaged:
		return ui.Warn
	case catalog.CheckUnreadable:
		return ui.Err
	}
	return ui.Dim
}
