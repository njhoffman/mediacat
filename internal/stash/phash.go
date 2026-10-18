package stash

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
	_ "modernc.org/sqlite"
)

// Fingerprint is the phash Stash stored for a video file, with the size and modification
// time the file had when Stash recorded it.
type Fingerprint struct {
	Size    int64
	ModTime time.Time
	PHash   uint64
}

// Matches reports whether the fingerprint still describes a file of this size and
// modification time. Stash keeps whole seconds.
func (f Fingerprint) Matches(size, mtimeNS int64) bool {
	return f.Size == size && f.ModTime.Unix() == time.Unix(0, mtimeNS).Unix()
}

// Database returns the path of Stash's database from its config ("database:", relative
// to the config's directory), defaulting to stash-go.sqlite beside the config. It is
// empty when the config does not exist.
func Database(configPath string) (string, error) {
	b, err := os.ReadFile(configPath)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var cfg struct {
		Database string `yaml:"database"`
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return "", err
	}
	db := cfg.Database
	if db == "" {
		db = "stash-go.sqlite"
	}
	if !filepath.IsAbs(db) {
		db = filepath.Join(filepath.Dir(configPath), db)
	}
	return db, nil
}

// PHashes reads every video phash from Stash's database, keyed by absolute path. The
// database is opened read-only; mediacat never writes to it.
func PHashes(dbPath string) (map[string]Fingerprint, error) {
	if _, err := os.Stat(dbPath); err != nil {
		return nil, err
	}
	dsn := (&url.URL{Scheme: "file", Path: dbPath, RawQuery: "mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(5000)"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT fo.path, f.basename, f.size, f.mod_time, fp.fingerprint
		FROM files f
		JOIN folders fo ON fo.id = f.parent_folder_id
		JOIN video_files vf ON vf.file_id = f.id
		JOIN files_fingerprints fp ON fp.file_id = f.id AND fp.type = 'phash'
		WHERE f.zip_file_id IS NULL`)
	if err != nil {
		return nil, fmt.Errorf("reading Stash phashes: %w", err)
	}
	defer rows.Close()
	out := map[string]Fingerprint{}
	for rows.Next() {
		var dir, base, mod string
		var fp Fingerprint
		var hash int64
		if err := rows.Scan(&dir, &base, &fp.Size, &mod, &hash); err != nil {
			return nil, fmt.Errorf("reading Stash phashes: %w", err)
		}
		if fp.ModTime, err = time.Parse(time.RFC3339Nano, mod); err != nil {
			continue
		}
		fp.PHash = uint64(hash)
		out[filepath.Join(dir, base)] = fp
	}
	return out, rows.Err()
}
