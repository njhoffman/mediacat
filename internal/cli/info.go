package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/njhoffman/mediacat/internal/catalog"
	"github.com/njhoffman/mediacat/internal/probe"
	"github.com/njhoffman/mediacat/internal/ui"
)

func (a *app) infoCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "info <file>",
		Short: "Show all metadata for one file",
		Long: `Show all metadata for one file. Cataloged files are read from the catalog; files
not yet cataloged are probed live (and not added; run scan for that).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.open(); err != nil {
				return err
			}
			return a.runInfo(args[0], asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON, including the full ffprobe output")
	return cmd
}

func (a *app) runInfo(path string, asJSON bool) error {
	v, rel, err := a.res.Resolve(path)
	if err != nil {
		return err
	}
	abs, _ := v.AbsPath(rel)
	var f *catalog.File
	if stored, err := a.db.VolumeByKey(v.Key); err != nil {
		return err
	} else if stored != nil {
		if f, err = a.db.GetFile(stored.ID, rel); err != nil {
			return err
		}
	}
	live := f == nil
	if live {
		kind := probe.KindOf(abs)
		if kind == "" {
			return fmt.Errorf("%s is not a recognized media file", abs)
		}
		a.warn("not in the catalog; probing live")
		f, err = probeLive(abs, kind)
		if err != nil {
			return err
		}
		f.VolumeKey, f.RelPath = v.Key, rel
	}
	f.AbsPath = abs

	if asJSON {
		out := struct {
			fileJSON
			Cataloged bool            `json:"cataloged"`
			FFprobe   json.RawMessage `json:"ffprobe,omitempty"`
		}{toJSON(*f), !live, f.Meta.Raw}
		enc := json.NewEncoder(a.stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	a.printInfo(*f, live)
	return nil
}

func probeLive(abs, kind string) (*catalog.File, error) {
	st, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	f := &catalog.File{Kind: kind, Size: st.Size(), MtimeNS: st.ModTime().UnixNano(), ScannedAt: time.Now()}
	res, err := probe.Prober{}.Probe(context.Background(), abs, kind)
	if err != nil {
		f.ProbeError = err.Error()
	} else {
		f.Meta = res
	}
	return f, nil
}

func (a *app) printInfo(f catalog.File, live bool) {
	m := f.Meta
	type kv struct{ k, v string }
	rows := []kv{
		{"path", f.AbsPath},
		{"volume", f.VolumeKey},
		{"type", f.Kind},
		{"size", fmt.Sprintf("%s (%s bytes)", ui.Bytes(f.Size), ui.Count(int(f.Size)))},
		{"modified", time.Unix(0, f.MtimeNS).Format("2006-01-02 15:04:05")},
		{"container", m.Container},
	}
	if f.Kind != "image" {
		rows = append(rows, kv{"duration", ui.Duration(m.DurationMS)}, kv{"bitrate", ui.Bitrate(m.Bitrate)})
	}
	if m.Width > 0 {
		rows = append(rows, kv{"resolution", ui.Res(m.Width, m.Height)})
	}
	if m.FPS > 0 {
		rows = append(rows, kv{"frame rate", fmt.Sprintf("%.3f fps", m.FPS)})
	}
	if m.VideoCodec != "" {
		rows = append(rows, kv{"video codec", m.VideoCodec}, kv{"video bitrate", ui.Bitrate(m.VideoBitrate)})
		if bpp := m.BPP(); bpp > 0 {
			rows = append(rows, kv{"bits per pixel", fmt.Sprintf("%.3f per pixel per frame", bpp)})
		}
	}
	if m.AudioCodec != "" {
		rows = append(rows, kv{"audio codec", m.AudioCodec}, kv{"audio bitrate", ui.Bitrate(m.AudioBitrate)},
			kv{"sample rate", fmt.Sprintf("%d Hz", m.SampleRate)}, kv{"channels", fmt.Sprint(m.Channels)})
	}
	t := m.Tags
	for _, tag := range []kv{{"artist", t.Artist}, {"album", t.Album}, {"title", t.Title}, {"genre", t.Genre}} {
		if tag.v != "" {
			rows = append(rows, tag)
		}
	}
	if t.Track > 0 {
		rows = append(rows, kv{"track", fmt.Sprint(t.Track)})
	}
	if t.Year > 0 {
		rows = append(rows, kv{"year", fmt.Sprint(t.Year)})
	}
	if c := f.Check; c.Status != "" {
		rows = append(rows, kv{"check", fmt.Sprintf("%s (%s, %s): %s", c.Status, c.Level, c.At.Format("2006-01-02"), c.Detail)})
	}
	switch p := f.PHash; {
	case p.OK():
		src := map[string]string{catalog.PHashStash: "from Stash", catalog.PHashMediacat: "computed by mediacat"}[p.Source]
		rows = append(rows, kv{"phash", fmt.Sprintf("%016x (%s)", p.Value, src)})
	case p.Source == catalog.PHashNone:
		rows = append(rows, kv{"phash", "none: " + p.Error})
	}
	if live {
		rows = append(rows, kv{"cataloged", "no"})
	} else {
		rows = append(rows, kv{"cataloged", fmt.Sprintf("%s (%s)", f.ScannedAt.Format("2006-01-02 15:04"), ui.Age(f.ScannedAt, a.now()))})
	}
	for _, r := range rows {
		fmt.Fprintf(a.stdout, "%s %s\n", ui.Header.Render(fmt.Sprintf("%12s", r.k)), r.v)
	}
	if f.MissingSince != nil {
		fmt.Fprintf(a.stdout, "%s %s\n", ui.Header.Render(fmt.Sprintf("%12s", "missing")),
			ui.Warn.Render("since "+f.MissingSince.Format("2006-01-02")))
	}
	if f.ProbeError != "" {
		fmt.Fprintf(a.stdout, "%s %s\n", ui.Header.Render(fmt.Sprintf("%12s", "error")), ui.Err.Render(strings.TrimSpace(f.ProbeError)))
	}
}
