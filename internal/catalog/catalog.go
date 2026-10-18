// Package catalog is the persistent SQLite store of volumes, files and scans.
package catalog

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	_ "modernc.org/sqlite"

	"github.com/njhoffman/mediacat/internal/filter"
	"github.com/njhoffman/mediacat/internal/probe"
	"github.com/njhoffman/mediacat/internal/volume"
)

var migrations = []string{
	`CREATE TABLE volumes (
		id              INTEGER PRIMARY KEY,
		key             TEXT NOT NULL UNIQUE,
		source          TEXT NOT NULL,
		fstype          TEXT NOT NULL,
		uuid            TEXT NOT NULL DEFAULT '',
		last_mount_path TEXT NOT NULL,
		last_mount_root TEXT NOT NULL DEFAULT '/',
		last_seen_at    INTEGER NOT NULL
	);
	CREATE TABLE files (
		id            INTEGER PRIMARY KEY,
		volume_id     INTEGER NOT NULL REFERENCES volumes(id) ON DELETE CASCADE,
		relpath       TEXT NOT NULL,
		ext           TEXT NOT NULL,
		size          INTEGER NOT NULL,
		mtime_ns      INTEGER NOT NULL,
		inode         INTEGER NOT NULL DEFAULT 0,
		kind          TEXT NOT NULL,
		container     TEXT NOT NULL DEFAULT '',
		duration_ms   INTEGER NOT NULL DEFAULT 0,
		width         INTEGER NOT NULL DEFAULT 0,
		height        INTEGER NOT NULL DEFAULT 0,
		video_codec   TEXT NOT NULL DEFAULT '',
		audio_codec   TEXT NOT NULL DEFAULT '',
		bitrate       INTEGER NOT NULL DEFAULT 0,
		fps           REAL NOT NULL DEFAULT 0,
		sample_rate   INTEGER NOT NULL DEFAULT 0,
		channels      INTEGER NOT NULL DEFAULT 0,
		tag_artist    TEXT NOT NULL DEFAULT '',
		tag_album     TEXT NOT NULL DEFAULT '',
		tag_title     TEXT NOT NULL DEFAULT '',
		tag_track     INTEGER NOT NULL DEFAULT 0,
		tag_year      INTEGER NOT NULL DEFAULT 0,
		tag_genre     TEXT NOT NULL DEFAULT '',
		probe_json    BLOB,
		probe_error   TEXT NOT NULL DEFAULT '',
		scanned_at    INTEGER NOT NULL,
		missing_since INTEGER,
		UNIQUE (volume_id, relpath)
	);
	CREATE INDEX files_identity ON files (volume_id, inode, size, mtime_ns);
	CREATE INDEX files_size ON files (size);
	CREATE TABLE scans (
		id           INTEGER PRIMARY KEY,
		volume_id    INTEGER NOT NULL REFERENCES volumes(id) ON DELETE CASCADE,
		root_relpath TEXT NOT NULL,
		started_at   INTEGER NOT NULL,
		finished_at  INTEGER,
		files_seen   INTEGER NOT NULL DEFAULT 0,
		added        INTEGER NOT NULL DEFAULT 0,
		changed      INTEGER NOT NULL DEFAULT 0,
		removed      INTEGER NOT NULL DEFAULT 0,
		status       TEXT NOT NULL,
		pid          INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX scans_volume ON scans (volume_id, status, finished_at);`,

	// Content hashes are valid only while hash_size/hash_mtime_ns match the file's current
	// size/mtime, so a changed file invalidates them without extra bookkeeping.
	`ALTER TABLE files ADD COLUMN partial_hash TEXT NOT NULL DEFAULT '';
	ALTER TABLE files ADD COLUMN full_hash TEXT NOT NULL DEFAULT '';
	ALTER TABLE files ADD COLUMN hash_size INTEGER NOT NULL DEFAULT -1;
	ALTER TABLE files ADD COLUMN hash_mtime_ns INTEGER NOT NULL DEFAULT 0;
	CREATE INDEX files_full_hash ON files (full_hash) WHERE full_hash != '';
	CREATE TABLE batches (
		id          INTEGER PRIMARY KEY,
		kind        TEXT NOT NULL,
		root        TEXT NOT NULL,
		dest        TEXT NOT NULL DEFAULT '',
		started_at  INTEGER NOT NULL,
		finished_at INTEGER,
		status      TEXT NOT NULL,
		undone_at   INTEGER
	);
	CREATE TABLE actions (
		id           INTEGER PRIMARY KEY,
		batch_id     INTEGER NOT NULL REFERENCES batches(id) ON DELETE CASCADE,
		volume_key   TEXT NOT NULL,
		relpath      TEXT NOT NULL,
		source_path  TEXT NOT NULL,
		size         INTEGER NOT NULL,
		full_hash    TEXT NOT NULL,
		keeper_path  TEXT NOT NULL,
		dest_path    TEXT NOT NULL DEFAULT '',
		info_path    TEXT NOT NULL DEFAULT '',
		status       TEXT NOT NULL,
		error        TEXT NOT NULL DEFAULT '',
		at           INTEGER NOT NULL
	);
	CREATE INDEX actions_batch ON actions (batch_id);`,

	`ALTER TABLE files ADD COLUMN video_bitrate INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE files ADD COLUMN audio_bitrate INTEGER NOT NULL DEFAULT 0;`,

	// Choices made in the interactive review, keyed by group content hash so they survive
	// re-runs and rescans as long as the group's content is unchanged.
	`CREATE TABLE review_decisions (
		group_key    TEXT PRIMARY KEY,
		keep_volume  TEXT NOT NULL,
		keep_relpath TEXT NOT NULL,
		action       TEXT NOT NULL,
		updated_at   INTEGER NOT NULL
	);`,
}

// bitrateMigration is the schema version that added per-stream bitrates; rows probed
// before it are backfilled from their stored ffprobe JSON.
const bitrateMigration = 3

// DB wraps the catalog database.
type DB struct {
	db   *sql.DB
	Path string
}

// DefaultPath is $XDG_DATA_HOME/mediacat/catalog.db.
func DefaultPath() string {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "mediacat", "catalog.db")
}

// Open opens (creating if needed) and migrates the catalog at path.
func Open(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	c := &DB{db: db, Path: path}
	if err := c.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrating %s: %w", path, err)
	}
	return c, nil
}

// Close closes the database.
func (c *DB) Close() error { return c.db.Close() }

func (c *DB) migrate() error {
	var v int
	if err := c.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	if v > len(migrations) {
		return fmt.Errorf("catalog schema version %d is newer than this mediacat (%d)", v, len(migrations))
	}
	for i := v; i < len(migrations); i++ {
		tx, err := c.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return err
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	if v < bitrateMigration && len(migrations) >= bitrateMigration {
		return c.backfillBitrates()
	}
	return nil
}

// backfillBitrates derives stream bitrates for already-probed files from the ffprobe
// output stored with them, so upgrading does not require re-probing the library.
func (c *DB) backfillBitrates() error {
	rows, err := c.db.Query(`SELECT id, kind, probe_json FROM files WHERE probe_json IS NOT NULL AND length(probe_json) > 0`)
	if err != nil {
		return err
	}
	type upd struct{ id, video, audio int64 }
	var updates []upd
	for rows.Next() {
		var id int64
		var kind string
		var raw []byte
		if err := rows.Scan(&id, &kind, &raw); err != nil {
			rows.Close()
			return err
		}
		if r, err := probe.FromJSON(raw, kind); err == nil && (r.VideoBitrate > 0 || r.AudioBitrate > 0) {
			updates = append(updates, upd{id, r.VideoBitrate, r.AudioBitrate})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	tx, err := c.db.Begin()
	if err != nil {
		return err
	}
	for _, u := range updates {
		if _, err := tx.Exec(`UPDATE files SET video_bitrate = ?, audio_bitrate = ? WHERE id = ?`, u.video, u.audio, u.id); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// Volume is a stored volume row.
type Volume struct {
	ID            int64
	Key           string
	Source        string
	FSType        string
	UUID          string
	LastMountPath string
	LastMountRoot string
	LastSeenAt    time.Time
}

// UpsertVolume records v as seen now and returns its id.
func (c *DB) UpsertVolume(v volume.Volume, now time.Time) (int64, error) {
	var id int64
	err := c.db.QueryRow(`
		INSERT INTO volumes (key, source, fstype, uuid, last_mount_path, last_mount_root, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (key) DO UPDATE SET source = excluded.source, fstype = excluded.fstype,
			uuid = excluded.uuid, last_mount_path = excluded.last_mount_path,
			last_mount_root = excluded.last_mount_root, last_seen_at = excluded.last_seen_at
		RETURNING id`,
		v.Key, v.Source, v.FSType, v.UUID, v.MountPoint, v.MountRoot, now.Unix()).Scan(&id)
	return id, err
}

// Volumes lists all known volumes.
func (c *DB) Volumes() ([]Volume, error) {
	rows, err := c.db.Query(`SELECT id, key, source, fstype, uuid, last_mount_path, last_mount_root, last_seen_at FROM volumes ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Volume
	for rows.Next() {
		var v Volume
		var seen int64
		if err := rows.Scan(&v.ID, &v.Key, &v.Source, &v.FSType, &v.UUID, &v.LastMountPath, &v.LastMountRoot, &seen); err != nil {
			return nil, err
		}
		v.LastSeenAt = time.Unix(seen, 0)
		out = append(out, v)
	}
	return out, rows.Err()
}

// VolumeByKey returns the volume with key, or nil.
func (c *DB) VolumeByKey(key string) (*Volume, error) {
	vs, err := c.Volumes()
	if err != nil {
		return nil, err
	}
	for i := range vs {
		if vs[i].Key == key {
			return &vs[i], nil
		}
	}
	return nil, nil
}

// File is a stored file row. Meta.Raw is only populated by GetFile.
type File struct {
	ID           int64
	VolumeID     int64
	RelPath      string
	Size         int64
	MtimeNS      int64
	Inode        uint64
	Kind         string
	Meta         probe.Result
	ProbeError   string
	ScannedAt    time.Time
	MissingSince *time.Time

	// Content hashes; empty when not computed or no longer valid for the current size/mtime.
	PartialHash string
	FullHash    string

	// Populated by queries that join volumes.
	VolumeKey string
	AbsPath   string // empty when the path lies outside the last known mount
}

// Ext returns the lowercase extension of relpath without the dot.
func Ext(relpath string) string {
	return strings.TrimPrefix(strings.ToLower(filepath.Ext(relpath)), ".")
}

// Known is the change-detection view of a stored file.
type Known struct {
	ID      int64
	Size    int64
	MtimeNS int64
	Inode   uint64
	Missing bool
}

// prefixClause restricts relpath to prefix and everything below it. It uses a byte-range
// comparison ('/' + 1 == '0') so it is index-friendly and needs no LIKE escaping.
func prefixClause(prefix string) (string, []any) {
	if prefix == "" {
		return "1=1", nil
	}
	return "(relpath = ? OR (relpath >= ? AND relpath < ?))", []any{prefix, prefix + "/", prefix + "0"}
}

// Known returns stored files for the volume under prefix, keyed by relpath.
func (c *DB) Known(volumeID int64, prefix string) (map[string]Known, error) {
	clause, args := prefixClause(prefix)
	rows, err := c.db.Query(`SELECT id, relpath, size, mtime_ns, inode, missing_since IS NOT NULL FROM files WHERE volume_id = ? AND `+clause,
		append([]any{volumeID}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Known{}
	for rows.Next() {
		var k Known
		var rel string
		var inode int64
		if err := rows.Scan(&k.ID, &rel, &k.Size, &k.MtimeNS, &inode, &k.Missing); err != nil {
			return nil, err
		}
		k.Inode = uint64(inode)
		out[rel] = k
	}
	return out, rows.Err()
}

// Tx batches writes made during a scan.
type Tx struct{ tx *sql.Tx }

// Begin starts a write batch.
func (c *DB) Begin() (*Tx, error) {
	tx, err := c.db.Begin()
	if err != nil {
		return nil, err
	}
	return &Tx{tx: tx}, nil
}

// Commit commits the batch.
func (t *Tx) Commit() error { return t.tx.Commit() }

// Rollback abandons the batch.
func (t *Tx) Rollback() error { return t.tx.Rollback() }

// PutFile inserts or replaces the file at (VolumeID, RelPath) and clears missing_since.
func (t *Tx) PutFile(f File) error {
	m := f.Meta
	_, err := t.tx.Exec(`
		INSERT INTO files (volume_id, relpath, ext, size, mtime_ns, inode, kind, container, duration_ms,
			width, height, video_codec, audio_codec, bitrate, video_bitrate, audio_bitrate, fps, sample_rate, channels,
			tag_artist, tag_album, tag_title, tag_track, tag_year, tag_genre,
			probe_json, probe_error, scanned_at, missing_since)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)
		ON CONFLICT (volume_id, relpath) DO UPDATE SET ext = excluded.ext, size = excluded.size,
			mtime_ns = excluded.mtime_ns, inode = excluded.inode, kind = excluded.kind,
			container = excluded.container, duration_ms = excluded.duration_ms,
			width = excluded.width, height = excluded.height, video_codec = excluded.video_codec,
			audio_codec = excluded.audio_codec, bitrate = excluded.bitrate, fps = excluded.fps,
			video_bitrate = excluded.video_bitrate, audio_bitrate = excluded.audio_bitrate,
			sample_rate = excluded.sample_rate, channels = excluded.channels,
			tag_artist = excluded.tag_artist, tag_album = excluded.tag_album,
			tag_title = excluded.tag_title, tag_track = excluded.tag_track,
			tag_year = excluded.tag_year, tag_genre = excluded.tag_genre,
			probe_json = excluded.probe_json, probe_error = excluded.probe_error,
			scanned_at = excluded.scanned_at, missing_since = NULL`,
		f.VolumeID, f.RelPath, Ext(f.RelPath), f.Size, f.MtimeNS, int64(f.Inode), f.Kind, m.Container, m.DurationMS,
		m.Width, m.Height, m.VideoCodec, m.AudioCodec, m.Bitrate, m.VideoBitrate, m.AudioBitrate, m.FPS, m.SampleRate, m.Channels,
		m.Tags.Artist, m.Tags.Album, m.Tags.Title, m.Tags.Track, m.Tags.Year, m.Tags.Genre,
		m.Raw, f.ProbeError, f.ScannedAt.Unix())
	return err
}

// Rename moves a stored file to a new relpath, keeping its metadata.
func (t *Tx) Rename(id int64, relpath string) error {
	_, err := t.tx.Exec(`UPDATE files SET relpath = ?, ext = ?, missing_since = NULL WHERE id = ?`, relpath, Ext(relpath), id)
	return err
}

// Restore clears missing_since for a file that reappeared unchanged.
func (t *Tx) Restore(id int64) error {
	_, err := t.tx.Exec(`UPDATE files SET missing_since = NULL WHERE id = ?`, id)
	return err
}

// MarkMissing records that a file was not found, keeping the earliest missing time.
func (t *Tx) MarkMissing(id int64, at time.Time) error {
	_, err := t.tx.Exec(`UPDATE files SET missing_since = COALESCE(missing_since, ?) WHERE id = ?`, at.Unix(), id)
	return err
}

// Delete removes a stored file row.
func (t *Tx) Delete(id int64) error {
	_, err := t.tx.Exec(`DELETE FROM files WHERE id = ?`, id)
	return err
}

// Scan is a stored scan record.
type Scan struct {
	ID         int64
	VolumeID   int64
	VolumeKey  string
	Root       string
	StartedAt  time.Time
	FinishedAt *time.Time
	FilesSeen  int
	Added      int
	Changed    int
	Removed    int
	Status     string
}

// Scan statuses.
const (
	ScanRunning     = "running"
	ScanComplete    = "complete"
	ScanInterrupted = "interrupted"
	ScanFailed      = "failed"
)

// StartScan records the start of a scan.
func (c *DB) StartScan(volumeID int64, root string, now time.Time) (int64, error) {
	res, err := c.db.Exec(`INSERT INTO scans (volume_id, root_relpath, started_at, status, pid) VALUES (?, ?, ?, ?, ?)`,
		volumeID, root, now.Unix(), ScanRunning, os.Getpid())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// FinishScan records the outcome of a scan.
func (c *DB) FinishScan(id int64, status string, seen, added, changed, removed int, now time.Time) error {
	_, err := c.db.Exec(`UPDATE scans SET status = ?, finished_at = ?, files_seen = ?, added = ?, changed = ?, removed = ? WHERE id = ?`,
		status, now.Unix(), seen, added, changed, removed, id)
	return err
}

const scanCols = `s.id, s.volume_id, v.key, s.root_relpath, s.started_at, s.finished_at, s.files_seen, s.added, s.changed, s.removed, s.status`

func scanScan(rows interface{ Scan(...any) error }) (Scan, error) {
	var s Scan
	var started int64
	var finished sql.NullInt64
	err := rows.Scan(&s.ID, &s.VolumeID, &s.VolumeKey, &s.Root, &started, &finished, &s.FilesSeen, &s.Added, &s.Changed, &s.Removed, &s.Status)
	s.StartedAt = time.Unix(started, 0)
	if finished.Valid {
		t := time.Unix(finished.Int64, 0)
		s.FinishedAt = &t
	}
	return s, err
}

// LastCompleteScan returns the most recent complete scan whose root covers relpath, or nil.
func (c *DB) LastCompleteScan(volumeID int64, relpath string) (*Scan, error) {
	row := c.db.QueryRow(`SELECT `+scanCols+` FROM scans s JOIN volumes v ON v.id = s.volume_id
		WHERE s.volume_id = ? AND s.status = ?
		  AND (s.root_relpath = '' OR s.root_relpath = ? OR substr(?, 1, length(s.root_relpath) + 1) = s.root_relpath || '/')
		ORDER BY s.finished_at DESC LIMIT 1`, volumeID, ScanComplete, relpath, relpath)
	s, err := scanScan(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// RecentScans returns the latest scans across all volumes.
func (c *DB) RecentScans(limit int) ([]Scan, error) {
	rows, err := c.db.Query(`SELECT `+scanCols+` FROM scans s JOIN volumes v ON v.id = s.volume_id ORDER BY s.started_at DESC, s.id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Scan
	for rows.Next() {
		s, err := scanScan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ReconcileRunning marks scans left "running" by a process that no longer exists as
// interrupted, so a killed scan is not reported as still in progress.
func (c *DB) ReconcileRunning(alive func(pid int) bool) error {
	rows, err := c.db.Query(`SELECT id, pid FROM scans WHERE status = ?`, ScanRunning)
	if err != nil {
		return err
	}
	var dead []int64
	for rows.Next() {
		var id int64
		var pid int
		if err := rows.Scan(&id, &pid); err != nil {
			rows.Close()
			return err
		}
		if pid == 0 || !alive(pid) {
			dead = append(dead, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range dead {
		if _, err := c.db.Exec(`UPDATE scans SET status = ? WHERE id = ?`, ScanInterrupted, id); err != nil {
			return err
		}
	}
	return nil
}

// ProcessAlive reports whether pid refers to a running process.
func ProcessAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

const fileCols = `f.id, f.volume_id, f.relpath, f.size, f.mtime_ns, f.inode, f.kind, f.container, f.duration_ms,
	f.width, f.height, f.video_codec, f.audio_codec, f.bitrate, f.video_bitrate, f.audio_bitrate, f.fps, f.sample_rate, f.channels,
	f.tag_artist, f.tag_album, f.tag_title, f.tag_track, f.tag_year, f.tag_genre,
	f.probe_error, f.scanned_at, f.missing_since, v.key, v.last_mount_path, v.last_mount_root,
	CASE WHEN f.hash_size = f.size AND f.hash_mtime_ns = f.mtime_ns THEN f.partial_hash ELSE '' END,
	CASE WHEN f.hash_size = f.size AND f.hash_mtime_ns = f.mtime_ns THEN f.full_hash ELSE '' END`

func scanFile(rows interface{ Scan(...any) error }, extra ...any) (File, error) {
	var f File
	var inode, scanned int64
	var missing sql.NullInt64
	var mountPath, mountRoot string
	m := &f.Meta
	dest := []any{&f.ID, &f.VolumeID, &f.RelPath, &f.Size, &f.MtimeNS, &inode, &f.Kind, &m.Container, &m.DurationMS,
		&m.Width, &m.Height, &m.VideoCodec, &m.AudioCodec, &m.Bitrate, &m.VideoBitrate, &m.AudioBitrate, &m.FPS, &m.SampleRate, &m.Channels,
		&m.Tags.Artist, &m.Tags.Album, &m.Tags.Title, &m.Tags.Track, &m.Tags.Year, &m.Tags.Genre,
		&f.ProbeError, &scanned, &missing, &f.VolumeKey, &mountPath, &mountRoot, &f.PartialHash, &f.FullHash}
	if err := rows.Scan(append(dest, extra...)...); err != nil {
		return f, err
	}
	f.Inode = uint64(inode)
	f.ScannedAt = time.Unix(scanned, 0)
	if missing.Valid {
		t := time.Unix(missing.Int64, 0)
		f.MissingSince = &t
	}
	f.AbsPath, _ = volume.AbsPath(mountPath, mountRoot, f.RelPath)
	return f, nil
}

// GetFile returns one file including its raw probe JSON, or nil when not cataloged.
func (c *DB) GetFile(volumeID int64, relpath string) (*File, error) {
	var raw []byte
	row := c.db.QueryRow(`SELECT `+fileCols+`, f.probe_json FROM files f JOIN volumes v ON v.id = f.volume_id WHERE f.volume_id = ? AND f.relpath = ?`, volumeID, relpath)
	f, err := scanFile(row, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	f.Meta.Raw = raw
	return &f, nil
}

// Query lists files matching flt. volumeID 0 means all volumes; prefix applies only when a
// volume is given. Results are in path order unless flt.Sorts says otherwise.
func (c *DB) Query(volumeID int64, prefix string, flt filter.Filter) ([]File, error) {
	var where []string
	var args []any
	add := func(clause string, a ...any) { where = append(where, clause); args = append(args, a...) }
	rng := func(col string, lo, hi any, loSet, hiSet bool) {
		if loSet {
			add(col+" >= ?", lo)
		}
		if hiSet {
			add(col+" <= ? AND "+col+" > 0", hi) // unknown (0) values never satisfy a maximum
		}
	}
	in := func(expr string, vals []string) {
		if len(vals) == 0 {
			return
		}
		a := make([]any, len(vals))
		for i, v := range vals {
			a[i] = v
		}
		add(expr+" IN ("+placeholders(len(vals))+")", a...)
	}
	contains := func(col, sub string) {
		if sub != "" {
			add("instr(lower("+col+"), lower(?)) > 0", sub)
		}
	}
	if volumeID != 0 {
		add("f.volume_id = ?", volumeID)
		clause, a := prefixClause(prefix)
		add(strings.ReplaceAll(clause, "relpath", "f.relpath"), a...)
	}
	if flt.Missing {
		add("f.missing_since IS NOT NULL")
	} else {
		add("f.missing_since IS NULL")
	}
	if flt.Errors {
		add("f.probe_error != ''")
	}
	if flt.Kind != "" {
		add("f.kind = ?", flt.Kind)
	}
	if flt.MinSize > 0 {
		add("f.size >= ?", flt.MinSize)
	}
	if flt.MaxSize > 0 {
		add("f.size <= ?", flt.MaxSize)
	}
	rng("f.duration_ms", flt.MinDurationMS, flt.MaxDurationMS, flt.MinDurationMS > 0, flt.MaxDurationMS > 0)
	rng("min(f.width, f.height)", flt.MinRes, flt.MaxRes, flt.MinRes > 0, flt.MaxRes > 0)
	rng("f.bitrate", flt.MinBitrate, flt.MaxBitrate, flt.MinBitrate > 0, flt.MaxBitrate > 0)
	rng("f.video_bitrate", flt.MinVBitrate, flt.MaxVBitrate, flt.MinVBitrate > 0, flt.MaxVBitrate > 0)
	rng("f.audio_bitrate", flt.MinABitrate, flt.MaxABitrate, flt.MinABitrate > 0, flt.MaxABitrate > 0)
	rng("f.fps", flt.MinFPS, flt.MaxFPS, flt.MinFPS > 0, flt.MaxFPS > 0)
	rng("f.tag_year", flt.MinYear, flt.MaxYear, flt.MinYear > 0, flt.MaxYear > 0)
	if len(flt.Codecs) > 0 {
		ph := placeholders(len(flt.Codecs))
		a := make([]any, 0, 2*len(flt.Codecs))
		for range 2 {
			for _, c := range flt.Codecs {
				a = append(a, c)
			}
		}
		add("(lower(f.video_codec) IN ("+ph+") OR lower(f.audio_codec) IN ("+ph+"))", a...)
	}
	in("lower(f.video_codec)", flt.VCodecs)
	in("lower(f.audio_codec)", flt.ACodecs)
	in("f.ext", flt.Exts)
	if len(flt.Containers) > 0 {
		var ors []string
		for _, c := range flt.Containers {
			ors = append(ors, "instr(lower(f.container), ?) > 0")
			args = append(args, strings.ToLower(c))
		}
		where = append(where, "("+strings.Join(ors, " OR ")+")")
	}
	contains("f.relpath", flt.Path)
	contains("f.tag_artist", flt.Artist)
	contains("f.tag_album", flt.Album)
	contains("f.tag_title", flt.Title)
	contains("f.tag_genre", flt.Genre)
	if !flt.ModifiedAfter.IsZero() {
		add("f.mtime_ns >= ?", flt.ModifiedAfter.UnixNano())
	}
	if !flt.ModifiedBefore.IsZero() {
		add("f.mtime_ns <= ?", flt.ModifiedBefore.UnixNano())
	}
	q := `SELECT ` + fileCols + ` FROM files f JOIN volumes v ON v.id = f.volume_id WHERE ` + strings.Join(where, " AND ") + ` ORDER BY v.key, f.relpath`
	// The limit can only go into SQL when nothing is filtered or sorted afterwards.
	if flt.Limit > 0 && flt.Name == "" && len(flt.Sorts) == 0 {
		q += fmt.Sprintf(" LIMIT %d", flt.Limit)
	}
	rows, err := c.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []File
	name := strings.ToLower(flt.Name)
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, err
		}
		if name != "" && !strings.Contains(strings.ToLower(filepath.Base(f.RelPath)), name) {
			continue
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	SortFiles(out, flt.Sorts)
	if flt.Limit > 0 && len(out) > flt.Limit {
		out = out[:flt.Limit]
	}
	return out, nil
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// KindStats aggregates one volume's files of one kind.
type KindStats struct {
	VolumeID int64
	Kind     string
	Count    int
	Bytes    int64
	Missing  int
	Errors   int
}

// Stats aggregates files per volume and kind.
func (c *DB) Stats() ([]KindStats, error) {
	rows, err := c.db.Query(`SELECT volume_id, kind,
			count(*) FILTER (WHERE missing_since IS NULL),
			coalesce(sum(size) FILTER (WHERE missing_since IS NULL), 0),
			count(*) FILTER (WHERE missing_since IS NOT NULL),
			count(*) FILTER (WHERE probe_error != '' AND missing_since IS NULL)
		FROM files GROUP BY volume_id, kind ORDER BY volume_id, kind`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []KindStats
	for rows.Next() {
		var s KindStats
		if err := rows.Scan(&s.VolumeID, &s.Kind, &s.Count, &s.Bytes, &s.Missing, &s.Errors); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// LatestCompleteScans returns each volume's most recent complete scan of any root.
func (c *DB) LatestCompleteScans() (map[int64]Scan, error) {
	rows, err := c.db.Query(`SELECT `+scanCols+` FROM scans s JOIN volumes v ON v.id = s.volume_id
		WHERE s.status = ? AND s.finished_at = (
			SELECT max(finished_at) FROM scans WHERE volume_id = s.volume_id AND status = ?)`, ScanComplete, ScanComplete)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]Scan{}
	for rows.Next() {
		s, err := scanScan(rows)
		if err != nil {
			return nil, err
		}
		out[s.VolumeID] = s
	}
	return out, rows.Err()
}

// SetHashes stores content hashes computed for a file at the given size and mtime. An
// empty full hash leaves any existing full hash untouched only if it is still valid.
func (t *Tx) SetHashes(id, size, mtimeNS int64, partial, full string) error {
	_, err := t.tx.Exec(`UPDATE files SET
			full_hash = CASE WHEN ? != '' THEN ?
				WHEN hash_size = ? AND hash_mtime_ns = ? THEN full_hash ELSE '' END,
			partial_hash = ?, hash_size = ?, hash_mtime_ns = ?
		WHERE id = ?`, full, full, size, mtimeNS, partial, size, mtimeNS, id)
	return err
}

// FileByID returns one file, or nil.
func (c *DB) FileByID(id int64) (*File, error) {
	row := c.db.QueryRow(`SELECT `+fileCols+` FROM files f JOIN volumes v ON v.id = f.volume_id WHERE f.id = ?`, id)
	f, err := scanFile(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &f, nil
}

// Action kinds.
const (
	ActionTrash  = "trash"
	ActionDelete = "delete"
	ActionMove   = "move"
)

// Action statuses.
const (
	ActionDone    = "done"
	ActionFailed  = "failed"
	ActionSkipped = "skipped"
	ActionUndone  = "undone"
)

// Batch is one execution of an action over a set of duplicates.
type Batch struct {
	ID         int64
	Kind       string
	Root       string
	Dest       string
	StartedAt  time.Time
	FinishedAt *time.Time
	Status     string
	UndoneAt   *time.Time
	Done       int
	Failed     int
	Bytes      int64
}

// Action is one file operation inside a batch.
type Action struct {
	ID         int64
	BatchID    int64
	VolumeKey  string
	RelPath    string
	SourcePath string
	Size       int64
	FullHash   string
	KeeperPath string
	DestPath   string // move destination or trash files/ entry
	InfoPath   string // trash .trashinfo file
	Status     string
	Error      string
	At         time.Time
}

// StartBatch records the start of an action batch.
func (c *DB) StartBatch(kind, root, dest string, now time.Time) (int64, error) {
	res, err := c.db.Exec(`INSERT INTO batches (kind, root, dest, started_at, status) VALUES (?, ?, ?, ?, ?)`,
		kind, root, dest, now.Unix(), ScanRunning)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// FinishBatch records a batch's final status.
func (c *DB) FinishBatch(id int64, status string, now time.Time) error {
	_, err := c.db.Exec(`UPDATE batches SET status = ?, finished_at = ? WHERE id = ?`, status, now.Unix(), id)
	return err
}

// MarkBatchUndone records that a batch was reverted.
func (c *DB) MarkBatchUndone(id int64, now time.Time) error {
	_, err := c.db.Exec(`UPDATE batches SET undone_at = ? WHERE id = ?`, now.Unix(), id)
	return err
}

// RecordAction appends an action to the log and, for completed actions, updates the
// catalog in the same transaction: trashed and moved files are marked missing (so undo
// can restore the row without re-probing), deleted files are removed.
func (c *DB) RecordAction(a Action, fileID int64, kind string) error {
	tx, err := c.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO actions (batch_id, volume_key, relpath, source_path, size, full_hash, keeper_path,
			dest_path, info_path, status, error, at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.BatchID, a.VolumeKey, a.RelPath, a.SourcePath, a.Size, a.FullHash, a.KeeperPath,
		a.DestPath, a.InfoPath, a.Status, a.Error, a.At.Unix()); err != nil {
		return err
	}
	if a.Status == ActionDone {
		if kind == ActionDelete {
			_, err = tx.Exec(`DELETE FROM files WHERE id = ?`, fileID)
		} else {
			_, err = tx.Exec(`UPDATE files SET missing_since = COALESCE(missing_since, ?) WHERE id = ?`, a.At.Unix(), fileID)
		}
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

const batchCols = `b.id, b.kind, b.root, b.dest, b.started_at, b.finished_at, b.status, b.undone_at,
	(SELECT count(*) FROM actions WHERE batch_id = b.id AND status IN ('done', 'undone')),
	(SELECT count(*) FROM actions WHERE batch_id = b.id AND status = 'failed'),
	(SELECT coalesce(sum(size), 0) FROM actions WHERE batch_id = b.id AND status IN ('done', 'undone'))`

func scanBatch(row interface{ Scan(...any) error }) (Batch, error) {
	var b Batch
	var started int64
	var finished, undone sql.NullInt64
	err := row.Scan(&b.ID, &b.Kind, &b.Root, &b.Dest, &started, &finished, &b.Status, &undone, &b.Done, &b.Failed, &b.Bytes)
	b.StartedAt = time.Unix(started, 0)
	if finished.Valid {
		t := time.Unix(finished.Int64, 0)
		b.FinishedAt = &t
	}
	if undone.Valid {
		t := time.Unix(undone.Int64, 0)
		b.UndoneAt = &t
	}
	return b, err
}

// Batches lists the most recent batches.
func (c *DB) Batches(limit int) ([]Batch, error) {
	rows, err := c.db.Query(`SELECT `+batchCols+` FROM batches b ORDER BY b.id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Batch
	for rows.Next() {
		b, err := scanBatch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// GetBatch returns a batch and its actions, or nil.
func (c *DB) GetBatch(id int64) (*Batch, []Action, error) {
	b, err := scanBatch(c.db.QueryRow(`SELECT `+batchCols+` FROM batches b WHERE b.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	rows, err := c.db.Query(`SELECT id, batch_id, volume_key, relpath, source_path, size, full_hash, keeper_path,
			dest_path, info_path, status, error, at FROM actions WHERE batch_id = ? ORDER BY id`, id)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var acts []Action
	for rows.Next() {
		var a Action
		var at int64
		if err := rows.Scan(&a.ID, &a.BatchID, &a.VolumeKey, &a.RelPath, &a.SourcePath, &a.Size, &a.FullHash, &a.KeeperPath,
			&a.DestPath, &a.InfoPath, &a.Status, &a.Error, &at); err != nil {
			return nil, nil, err
		}
		a.At = time.Unix(at, 0)
		acts = append(acts, a)
	}
	return &b, acts, rows.Err()
}

// MarkActionUndone records that one action was reverted and restores the file's catalog
// row when it still exists.
func (c *DB) MarkActionUndone(a Action) error {
	tx, err := c.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE actions SET status = ? WHERE id = ?`, ActionUndone, a.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE files SET missing_since = NULL
		WHERE relpath = ? AND volume_id = (SELECT id FROM volumes WHERE key = ?)`, a.RelPath, a.VolumeKey); err != nil {
		return err
	}
	return tx.Commit()
}

// TrashedEntries returns completed trash actions that have not been undone, for emptying
// only what mediacat put in a trash directory.
func (c *DB) TrashedEntries(volumeKey string) ([]Action, error) {
	rows, err := c.db.Query(`SELECT a.id, a.batch_id, a.volume_key, a.relpath, a.source_path, a.size, a.full_hash,
			a.keeper_path, a.dest_path, a.info_path, a.status, a.error, a.at
		FROM actions a JOIN batches b ON b.id = a.batch_id
		WHERE b.kind = ? AND a.status = ? AND a.volume_key = ? ORDER BY a.id`, ActionTrash, ActionDone, volumeKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Action
	for rows.Next() {
		var a Action
		var at int64
		if err := rows.Scan(&a.ID, &a.BatchID, &a.VolumeKey, &a.RelPath, &a.SourcePath, &a.Size, &a.FullHash,
			&a.KeeperPath, &a.DestPath, &a.InfoPath, &a.Status, &a.Error, &at); err != nil {
			return nil, err
		}
		a.At = time.Unix(at, 0)
		out = append(out, a)
	}
	return out, rows.Err()
}

// MarkActionPurged records that a trashed file was permanently removed from the trash.
func (c *DB) MarkActionPurged(id int64) error {
	_, err := c.db.Exec(`UPDATE actions SET status = 'purged' WHERE id = ?`, id)
	return err
}

// Decision is a saved review choice for one duplicate group.
type Decision struct {
	KeepVolume  string
	KeepRelPath string
	Action      string // an Action* kind, or "" to leave the group alone
}

// LoadDecisions returns saved decisions for the given group keys.
func (c *DB) LoadDecisions(keys []string) (map[string]Decision, error) {
	out := map[string]Decision{}
	for start := 0; start < len(keys); start += 500 {
		chunk := keys[start:min(start+500, len(keys))]
		args := make([]any, len(chunk))
		for i, k := range chunk {
			args[i] = k
		}
		rows, err := c.db.Query(`SELECT group_key, keep_volume, keep_relpath, action FROM review_decisions
			WHERE group_key IN (`+placeholders(len(chunk))+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var k string
			var d Decision
			if err := rows.Scan(&k, &d.KeepVolume, &d.KeepRelPath, &d.Action); err != nil {
				rows.Close()
				return nil, err
			}
			out[k] = d
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// SaveDecision stores the choice for one group.
func (c *DB) SaveDecision(key string, d Decision, now time.Time) error {
	_, err := c.db.Exec(`INSERT INTO review_decisions (group_key, keep_volume, keep_relpath, action, updated_at)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT (group_key) DO UPDATE SET keep_volume = excluded.keep_volume,
		keep_relpath = excluded.keep_relpath, action = excluded.action, updated_at = excluded.updated_at`,
		key, d.KeepVolume, d.KeepRelPath, d.Action, now.Unix())
	return err
}

// DeleteDecisions forgets saved choices, e.g. once they have been applied.
func (c *DB) DeleteDecisions(keys []string) error {
	for _, k := range keys {
		if _, err := c.db.Exec(`DELETE FROM review_decisions WHERE group_key = ?`, k); err != nil {
			return err
		}
	}
	return nil
}
