package catalog

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/njhoffman/mediacat/internal/filter"
	"github.com/njhoffman/mediacat/internal/probe"
	"github.com/njhoffman/mediacat/internal/volume"
)

var t0 = time.Unix(1_700_000_000, 0)

func openTest(t *testing.T) (*DB, int64) {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "sub", "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	id, err := db.UpsertVolume(volume.Volume{Key: "net://h/share", Source: "//h/share", FSType: "cifs", MountPoint: "/mnt/nfs", MountRoot: "/"}, t0)
	if err != nil {
		t.Fatal(err)
	}
	return db, id
}

func put(t *testing.T, db *DB, files ...File) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f.ScannedAt.IsZero() {
			f.ScannedAt = t0
		}
		if err := tx.PutFile(f); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func video(vol int64, rel string, size int64, w, h int, durMS int64, codec string) File {
	f := File{VolumeID: vol, RelPath: rel, Size: size, MtimeNS: 1, Kind: filter.KindVideo,
		Meta: probe.Result{Width: w, Height: h, DurationMS: durMS, VideoCodec: codec, AudioCodec: "aac", Bitrate: size / 10,
			VideoBitrate: size / 12, AudioBitrate: 128_000, FPS: 25}}
	if codec == "av1" {
		f.Meta.FPS = 60
	}
	if strings.HasSuffix(rel, ".mkv") {
		f.Meta.Container = "matroska,webm"
	}
	return f
}

func sorts(spec string) []filter.SortKey {
	k, err := filter.ParseSort(spec)
	if err != nil {
		panic(err)
	}
	return k
}

func paths(fs []File) []string {
	var out []string
	for _, f := range fs {
		out = append(out, f.RelPath)
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestReopenKeepsDataAndVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := db.UpsertVolume(volume.Volume{Key: "k", MountPoint: "/x", MountRoot: "/"}, t0)
	put(t, db, video(id, "a.mp4", 1, 1, 1, 1, "h264"))
	db.Close()

	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	again, _ := db.UpsertVolume(volume.Volume{Key: "k", MountPoint: "/y", MountRoot: "/"}, t0.Add(time.Hour))
	if again != id {
		t.Errorf("upsert changed id %d -> %d", id, again)
	}
	f, err := db.GetFile(id, "a.mp4")
	if err != nil || f == nil || f.AbsPath != "/y/a.mp4" {
		t.Errorf("after reopen: %+v %v", f, err)
	}
}

func TestPrefixScoping(t *testing.T) {
	db, vol := openTest(t)
	put(t, db,
		video(vol, "Stash/a.mp4", 1, 1, 1, 1, "h264"),
		video(vol, "Stash/Sorted/b.mp4", 1, 1, 1, 1, "h264"),
		video(vol, "Stash0/c.mp4", 1, 1, 1, 1, "h264"),
		video(vol, "Stashed/d.mp4", 1, 1, 1, 1, "h264"),
		video(vol, "top.mp4", 1, 1, 1, 1, "h264"),
	)
	k, err := db.Known(vol, "Stash")
	if err != nil {
		t.Fatal(err)
	}
	if len(k) != 2 {
		t.Errorf("Known(Stash) = %v", k)
	}
	all, _ := db.Known(vol, "")
	if len(all) != 5 {
		t.Errorf("Known('') = %d", len(all))
	}
	got, _ := db.Query(vol, "Stash", filter.Filter{})
	if want := []string{"Stash/Sorted/b.mp4", "Stash/a.mp4"}; !equal(paths(got), want) {
		t.Errorf("Query(Stash) = %v", paths(got))
	}
}

func TestMissingRenameRestoreDelete(t *testing.T) {
	db, vol := openTest(t)
	put(t, db, video(vol, "a.mp4", 1, 1, 1, 1, "h264"), video(vol, "b.mp4", 1, 1, 1, 1, "h264"))
	k, _ := db.Known(vol, "")
	tx, _ := db.Begin()
	tx.MarkMissing(k["a.mp4"].ID, t0)
	tx.MarkMissing(k["a.mp4"].ID, t0.Add(time.Hour)) // keeps earliest
	tx.Rename(k["b.mp4"].ID, "dir/B.MKV")
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	missing, _ := db.Query(vol, "", filter.Filter{Missing: true})
	if len(missing) != 1 || missing[0].MissingSince == nil || !missing[0].MissingSince.Equal(t0) {
		t.Fatalf("missing = %+v", missing)
	}
	present, _ := db.Query(vol, "", filter.Filter{Exts: []string{"mkv"}})
	if !equal(paths(present), []string{"dir/B.MKV"}) {
		t.Errorf("renamed ext filter = %v", paths(present))
	}
	tx, _ = db.Begin()
	tx.Restore(k["a.mp4"].ID)
	tx.Delete(k["b.mp4"].ID)
	tx.Commit()
	present, _ = db.Query(vol, "", filter.Filter{})
	if !equal(paths(present), []string{"a.mp4"}) {
		t.Errorf("after restore/delete = %v", paths(present))
	}
}

func TestQueryFilters(t *testing.T) {
	db, vol := openTest(t)
	put(t, db,
		video(vol, "hd.mp4", 300<<20, 1280, 720, 60_000, "h264"),
		video(vol, "fhd.mkv", 2<<30, 1920, 1080, 3_600_000, "hevc"),
		video(vol, "portrait.mp4", 100<<20, 1080, 1920, 30_000, "h264"),
		video(vol, "uhd.mp4", 8<<30, 3840, 2160, 5_400_000, "av1"),
		File{VolumeID: vol, RelPath: "song.mp3", Size: 5 << 20, MtimeNS: 1, Kind: filter.KindAudio,
			Meta: probe.Result{AudioCodec: "mp3", DurationMS: 200_000}},
		File{VolumeID: vol, RelPath: "broken.mp4", Size: 10, MtimeNS: 1, Kind: filter.KindVideo, ProbeError: "moov atom not found"},
	)
	cases := []struct {
		name string
		f    filter.Filter
		want []string
	}{
		{"kind", filter.Filter{Kind: filter.KindAudio}, []string{"song.mp3"}},
		{"min size", filter.Filter{MinSize: 1 << 30}, []string{"fhd.mkv", "uhd.mp4"}},
		{"max size", filter.Filter{MaxSize: 10 << 20}, []string{"broken.mp4", "song.mp3"}},
		{"duration range", filter.Filter{MinDurationMS: 60_000, MaxDurationMS: 3_600_000}, []string{"fhd.mkv", "hd.mp4", "song.mp3"}},
		{"min res uses short side", filter.Filter{MinRes: 1080}, []string{"fhd.mkv", "portrait.mp4", "uhd.mp4"}},
		{"max res excludes unknown", filter.Filter{MaxRes: 720}, []string{"hd.mp4"}},
		{"codec", filter.Filter{Codecs: []string{"hevc", "mp3"}}, []string{"fhd.mkv", "song.mp3"}},
		{"ext", filter.Filter{Exts: []string{"mkv", "mp3"}}, []string{"fhd.mkv", "song.mp3"}},
		{"sort size desc limit", filter.Filter{Sorts: sorts("-size"), Limit: 2}, []string{"uhd.mp4", "fhd.mkv"}},
		{"sort res by pixels, ties by path", filter.Filter{Kind: filter.KindVideo, MinRes: 1, Sorts: sorts("res")}, []string{"hd.mp4", "fhd.mkv", "portrait.mp4", "uhd.mp4"}},
		{"sort path desc", filter.Filter{Kind: filter.KindVideo, MinRes: 1080, Sorts: sorts("-path")}, []string{"uhd.mp4", "portrait.mp4", "fhd.mkv"}},
		{"unknown duration sorts last both ways", filter.Filter{Sorts: sorts("duration")}, []string{"portrait.mp4", "hd.mp4", "song.mp3", "fhd.mkv", "uhd.mp4", "broken.mp4"}},
		{"unknown last descending", filter.Filter{Sorts: sorts("-duration"), Limit: 6}, []string{"uhd.mp4", "fhd.mkv", "song.mp3", "hd.mp4", "portrait.mp4", "broken.mp4"}},
		{"multi-key", filter.Filter{Sorts: sorts("vcodec,-size")}, []string{"uhd.mp4", "hd.mp4", "portrait.mp4", "fhd.mkv", "song.mp3", "broken.mp4"}},
		{"name filter then limit", filter.Filter{Name: "D.MP", Limit: 1}, []string{"hd.mp4"}},
		{"name filter", filter.Filter{Name: "HD."}, []string{"fhd.mkv", "hd.mp4", "uhd.mp4"}},
		{"path filter", filter.Filter{Path: "PORT"}, []string{"portrait.mp4"}},
		{"vcodec", filter.Filter{VCodecs: []string{"hevc", "av1"}}, []string{"fhd.mkv", "uhd.mp4"}},
		{"acodec", filter.Filter{ACodecs: []string{"mp3"}}, []string{"song.mp3"}},
		{"min fps", filter.Filter{MinFPS: 50}, []string{"uhd.mp4"}},
		{"vbitrate range", filter.Filter{MinVBitrate: 1, MaxVBitrate: 40_000_000}, []string{"hd.mp4", "portrait.mp4"}},
		{"errors only", filter.Filter{Errors: true}, []string{"broken.mp4"}},
		{"container", filter.Filter{Containers: []string{"MATROSKA"}}, []string{"fhd.mkv"}},
	}
	for _, c := range cases {
		got, err := db.Query(vol, "", c.f)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if !equal(paths(got), c.want) {
			t.Errorf("%s: got %v want %v", c.name, paths(got), c.want)
		}
	}
	if all, _ := db.Query(0, "ignored", filter.Filter{}); len(all) != 6 {
		t.Errorf("all-volume query returned %d", len(all))
	}
}

func TestScansCoverage(t *testing.T) {
	db, vol := openTest(t)
	s1, _ := db.StartScan(vol, "Stash", t0)
	db.FinishScan(s1, ScanComplete, 10, 10, 0, 0, t0.Add(time.Minute))
	s2, _ := db.StartScan(vol, "Stash/Sorted", t0.Add(time.Hour))
	db.FinishScan(s2, ScanInterrupted, 1, 1, 0, 0, t0.Add(2*time.Hour))

	cases := map[string]int64{"Stash": s1, "Stash/Sorted/x": s1, "Stash/a.mp4": s1}
	for rel, want := range cases {
		s, err := db.LastCompleteScan(vol, rel)
		if err != nil || s == nil || s.ID != want {
			t.Errorf("LastCompleteScan(%q) = %+v %v", rel, s, err)
		}
	}
	for _, rel := range []string{"", "Stashed", "Other/x"} {
		if s, _ := db.LastCompleteScan(vol, rel); s != nil {
			t.Errorf("LastCompleteScan(%q) should be nil, got %d", rel, s.ID)
		}
	}
	s3, _ := db.StartScan(vol, "", t0.Add(3*time.Hour))
	db.FinishScan(s3, ScanComplete, 1, 0, 0, 0, t0.Add(4*time.Hour))
	if s, _ := db.LastCompleteScan(vol, "Other/x"); s == nil || s.ID != s3 {
		t.Errorf("root scan should cover everything")
	}
	recent, _ := db.RecentScans(2)
	if len(recent) != 2 || recent[0].ID != s3 || recent[0].VolumeKey != "net://h/share" {
		t.Errorf("RecentScans = %+v", recent)
	}
}

func TestReconcileRunning(t *testing.T) {
	db, vol := openTest(t)
	id, _ := db.StartScan(vol, "", t0)
	if err := db.ReconcileRunning(func(int) bool { return true }); err != nil {
		t.Fatal(err)
	}
	if r, _ := db.RecentScans(1); r[0].Status != ScanRunning {
		t.Errorf("live scan reconciled: %s", r[0].Status)
	}
	db.ReconcileRunning(func(int) bool { return false })
	if r, _ := db.RecentScans(1); r[0].ID != id || r[0].Status != ScanInterrupted {
		t.Errorf("dead scan not reconciled: %+v", r[0])
	}
}

func TestStats(t *testing.T) {
	db, vol := openTest(t)
	put(t, db, video(vol, "a.mp4", 100, 1, 1, 1, "h264"), video(vol, "b.mp4", 50, 1, 1, 1, "h264"),
		File{VolumeID: vol, RelPath: "c.mp4", Size: 7, MtimeNS: 1, Kind: filter.KindVideo, ProbeError: "bad"})
	k, _ := db.Known(vol, "")
	tx, _ := db.Begin()
	tx.MarkMissing(k["b.mp4"].ID, t0)
	tx.Commit()
	st, err := db.Stats()
	if err != nil || len(st) != 1 {
		t.Fatalf("Stats = %+v %v", st, err)
	}
	if s := st[0]; s.Count != 2 || s.Bytes != 107 || s.Missing != 1 || s.Errors != 1 {
		t.Errorf("Stats = %+v", s)
	}
}

func TestProcessAlive(t *testing.T) {
	if !ProcessAlive(1) {
		t.Error("pid 1 should be alive")
	}
	if ProcessAlive(1 << 30) {
		t.Error("huge pid should not be alive")
	}
}

func TestLatestCompleteScans(t *testing.T) {
	db, vol := openTest(t)
	other, _ := db.UpsertVolume(volume.Volume{Key: "uuid:x", MountPoint: "/x", MountRoot: "/"}, t0)
	a, _ := db.StartScan(vol, "a", t0)
	db.FinishScan(a, ScanComplete, 0, 0, 0, 0, t0.Add(time.Minute))
	b, _ := db.StartScan(vol, "b", t0.Add(time.Hour))
	db.FinishScan(b, ScanComplete, 0, 0, 0, 0, t0.Add(2*time.Hour))
	c, _ := db.StartScan(vol, "c", t0.Add(3*time.Hour))
	db.FinishScan(c, ScanInterrupted, 0, 0, 0, 0, t0.Add(4*time.Hour))
	got, err := db.LatestCompleteScans()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[vol].ID != b {
		t.Errorf("LatestCompleteScans = %+v", got)
	}
	if _, ok := got[other]; ok {
		t.Error("never-scanned volume should be absent")
	}
}

func TestBitrateBackfillOnUpgrade(t *testing.T) {
	// Build a genuine version-2 catalog, before stream bitrates existed.
	path := filepath.Join(t.TempDir(), "c.db")
	sqldb, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	old := &DB{db: sqldb, Path: path}
	if err := old.migrateTo(2); err != nil {
		t.Fatal(err)
	}
	raw := `{"format":{"format_name":"mov,mp4","duration":"10","bit_rate":"5000000"},
		"streams":[{"codec_type":"video","codec_name":"h264","bit_rate":"4800000"},{"codec_type":"audio","codec_name":"aac","bit_rate":"192000"}]}`
	for _, q := range []string{
		`INSERT INTO volumes (id, key, source, fstype, last_mount_path, last_seen_at) VALUES (1, 'k', 's', 'ext4', '/x', 0)`,
		`INSERT INTO files (volume_id, relpath, ext, size, mtime_ns, kind, scanned_at, probe_json) VALUES (1, 'a.mp4', 'mp4', 1, 1, 'video', 0, '` + raw + `')`,
	} {
		if _, err := sqldb.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	sqldb.Close()

	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	f, _ := db.GetFile(1, "a.mp4")
	if f == nil || f.Meta.VideoBitrate != 4_800_000 || f.Meta.AudioBitrate != 192_000 {
		t.Errorf("backfill: %+v", f)
	}
}

func TestCheckResults(t *testing.T) {
	db, vol := openTest(t)
	put(t, db, video(vol, "a.mp4", 10, 1, 1, 1, "h264"))
	k, _ := db.Known(vol, "")
	tx, _ := db.Begin()
	tx.SetCheck(k["a.mp4"].ID, 10, 1, Check{Status: CheckDamaged, Level: LevelFull, Detail: "3 decode errors", At: t0})
	tx.Commit()
	f, _ := db.GetFile(vol, "a.mp4")
	if f.Check.Status != CheckDamaged || f.Check.Level != LevelFull || !f.Check.At.Equal(t0) {
		t.Errorf("check = %+v", f.Check)
	}
	// A changed file no longer reports the old result.
	changed := video(vol, "a.mp4", 11, 1, 1, 1, "h264")
	put(t, db, changed)
	if f, _ := db.GetFile(vol, "a.mp4"); f.Check.Status != "" {
		t.Errorf("stale check reported: %+v", f.Check)
	}
}

func TestQueryByCheckStatus(t *testing.T) {
	db, vol := openTest(t)
	put(t, db, video(vol, "a.mp4", 1, 1, 1, 1, "h264"), video(vol, "b.mp4", 1, 1, 1, 1, "h264"), video(vol, "c.mp4", 1, 1, 1, 1, "h264"))
	k, _ := db.Known(vol, "")
	tx, _ := db.Begin()
	tx.SetCheck(k["a.mp4"].ID, 1, 1, Check{Status: CheckOK, Level: LevelQuick, At: t0})
	tx.SetCheck(k["b.mp4"].ID, 1, 1, Check{Status: CheckDamaged, Level: LevelFull, At: t0})
	tx.Commit()
	for spec, want := range map[string][]string{
		"damaged": {"b.mp4"}, "ok,damaged": {"a.mp4", "b.mp4"}, "unchecked": {"c.mp4"},
	} {
		got, _ := db.Query(vol, "", filter.Filter{Checks: strings.Split(spec, ",")})
		if !equal(paths(got), want) {
			t.Errorf("%s: %v", spec, paths(got))
		}
	}
}

func TestReviewDecisions(t *testing.T) {
	db, _ := openTest(t)
	d := Decision{KeepVolume: "net://h/share", KeepRelPath: "Sorted/a.mp4", Action: ActionTrash}
	if err := db.SaveDecision("h1", d, t0); err != nil {
		t.Fatal(err)
	}
	d.Action = ActionMove
	db.SaveDecision("h1", d, t0)
	db.SaveDecision("h2", Decision{KeepVolume: "v", KeepRelPath: "b", Action: ""}, t0)
	got, err := db.LoadDecisions([]string{"h1", "h2", "h3"})
	if err != nil || len(got) != 2 || got["h1"].Action != ActionMove || got["h2"].KeepRelPath != "b" {
		t.Fatalf("LoadDecisions = %+v %v", got, err)
	}
	db.DeleteDecisions([]string{"h1"})
	if got, _ := db.LoadDecisions([]string{"h1", "h2"}); len(got) != 1 {
		t.Errorf("after delete: %+v", got)
	}
}

func TestCopiesCount(t *testing.T) {
	db, vol := openTest(t)
	mk := func(rel string, inode uint64) File {
		f := video(vol, rel, 100, 1, 1, 1, "h264")
		f.Inode = inode
		return f
	}
	put(t, db, mk("a.mp4", 1), mk("b.mp4", 2), mk("hardlink-of-a.mp4", 1), mk("gone.mp4", 3), mk("other.mp4", 4))
	k, _ := db.Known(vol, "")
	tx, _ := db.Begin()
	for _, rel := range []string{"a.mp4", "b.mp4", "hardlink-of-a.mp4", "gone.mp4"} {
		tx.SetHashes(k[rel].ID, 100, 1, "p", "samehash")
	}
	tx.MarkMissing(k["gone.mp4"].ID, t0)
	tx.Commit()
	got := map[string]int{}
	fs, _ := db.Query(vol, "", filter.Filter{})
	for _, f := range fs {
		got[f.RelPath] = f.Copies
	}
	// a, b and the hardlink share content; the hardlink is the same file as a, and the
	// missing copy does not count.
	if got["a.mp4"] != 2 || got["b.mp4"] != 2 || got["other.mp4"] != 0 {
		t.Errorf("copies = %v", got)
	}
	dups, _ := db.Query(vol, "", filter.Filter{Duplicates: true})
	if len(dups) != 3 {
		t.Errorf("--duplicates = %v", paths(dups))
	}
}
