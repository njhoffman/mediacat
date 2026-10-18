package catalog

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/njhoffman/mediacat/internal/filter"
)

// Value returns a file's value for a field: a number for numeric fields or a string for
// text fields. known is false for values that were never recorded (0 or empty).
func Value(f File, field string) (num float64, str string, numeric, known bool) {
	m := f.Meta
	n := func(v float64) (float64, string, bool, bool) { return v, "", true, v > 0 }
	t := func(v string) (float64, string, bool, bool) { return 0, v, false, v != "" }
	switch field {
	case filter.FieldPath:
		return t(f.VolumeKey + "\x00" + f.RelPath)
	case filter.FieldName:
		return t(strings.ToLower(filepath.Base(f.RelPath)))
	case filter.FieldDir:
		return t(filepath.Dir(f.RelPath))
	case filter.FieldExt:
		return t(Ext(f.RelPath))
	case filter.FieldType:
		return t(f.Kind)
	case filter.FieldSize:
		return float64(f.Size), "", true, true
	case filter.FieldDuration:
		return n(float64(m.DurationMS))
	case filter.FieldRes:
		return n(float64(m.Width) * float64(m.Height))
	case filter.FieldWidth:
		return n(float64(m.Width))
	case filter.FieldHeight:
		return n(float64(m.Height))
	case filter.FieldFPS:
		return n(m.FPS)
	case filter.FieldBitrate:
		return n(float64(m.Bitrate))
	case filter.FieldVBitrate:
		return n(float64(m.VideoBitrate))
	case filter.FieldABitrate:
		return n(float64(m.AudioBitrate))
	case filter.FieldCodec:
		return t(strings.Trim(m.VideoCodec+"/"+m.AudioCodec, "/"))
	case filter.FieldVCodec:
		return t(m.VideoCodec)
	case filter.FieldACodec:
		return t(m.AudioCodec)
	case filter.FieldContainer:
		return t(m.Container)
	case filter.FieldChannels:
		return n(float64(m.Channels))
	case filter.FieldSampleRate:
		return n(float64(m.SampleRate))
	case filter.FieldMtime:
		return float64(f.MtimeNS), "", true, true
	case filter.FieldScanned:
		return float64(f.ScannedAt.Unix()), "", true, true
	case filter.FieldArtist:
		return t(strings.ToLower(m.Tags.Artist))
	case filter.FieldAlbum:
		return t(strings.ToLower(m.Tags.Album))
	case filter.FieldTitle:
		return t(strings.ToLower(m.Tags.Title))
	case filter.FieldTrack:
		return n(float64(m.Tags.Track))
	case filter.FieldYear:
		return n(float64(m.Tags.Year))
	case filter.FieldGenre:
		return t(strings.ToLower(m.Tags.Genre))
	case filter.FieldError:
		return t(f.ProbeError)
	case filter.FieldCheck:
		return t(f.Check.Status)
	case filter.FieldDup:
		if f.Copies > 1 {
			return float64(f.Copies), "", true, true
		}
		return 0, "", true, false
	}
	return 0, "", false, false
}

// SortFiles orders files by keys, stably. Unknown values sort last in either direction;
// path is the final tie-break.
func SortFiles(files []File, keys []filter.SortKey) {
	if len(keys) == 0 {
		return
	}
	sort.SliceStable(files, func(i, j int) bool {
		for _, k := range keys {
			an, as, numeric, aok := Value(files[i], k.Key)
			bn, bs, _, bok := Value(files[j], k.Key)
			if aok != bok {
				return aok
			}
			if !aok {
				continue
			}
			var c int
			switch {
			case numeric && an < bn, !numeric && as < bs:
				c = -1
			case numeric && an > bn, !numeric && as > bs:
				c = 1
			}
			if c == 0 {
				continue
			}
			if k.Desc {
				return c > 0
			}
			return c < 0
		}
		return files[i].VolumeKey+"\x00"+files[i].RelPath < files[j].VolumeKey+"\x00"+files[j].RelPath
	})
}
