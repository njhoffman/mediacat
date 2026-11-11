package stash

import (
	"fmt"
	"os"
	"path/filepath"
)

// Role is what a file is to Stash.
type Role int

// Roles, from least to most important to keep.
const (
	Untracked Role = iota // not in Stash's database
	Tracked               // in Stash's database, but not the primary file of a scene or image
	Primary               // the primary file of a scene or an image
)

// Files reads every file Stash tracks, keyed by absolute path, marking the primary file of
// each scene and image. Files inside zip archives are left out. The database is opened
// read-only.
func Files(dbPath string) (map[string]Role, error) {
	if _, err := os.Stat(dbPath); err != nil {
		return nil, err
	}
	db, err := openReadOnly(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT fo.path, f.basename,
			EXISTS (SELECT 1 FROM scenes_files sf WHERE sf.file_id = f.id AND sf."primary" = 1)
			OR EXISTS (SELECT 1 FROM images_files i WHERE i.file_id = f.id AND i."primary" = 1)
		FROM files f
		JOIN folders fo ON fo.id = f.parent_folder_id
		WHERE f.zip_file_id IS NULL`)
	if err != nil {
		return nil, fmt.Errorf("reading Stash files: %w", err)
	}
	defer rows.Close()
	out := map[string]Role{}
	for rows.Next() {
		var dir, base string
		var primary bool
		if err := rows.Scan(&dir, &base, &primary); err != nil {
			return nil, fmt.Errorf("reading Stash files: %w", err)
		}
		r := Tracked
		if primary {
			r = Primary
		}
		out[filepath.Join(dir, base)] = r
	}
	return out, rows.Err()
}
