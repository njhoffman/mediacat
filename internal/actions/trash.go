// Package actions trashes, deletes and moves duplicate files, logs every operation, and
// undoes trash and move batches.
package actions

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/njhoffman/mediacat/internal/volume"
)

// Trasher moves files into the freedesktop.org trash on the file's own drive. It never
// falls back to a trash on another device, which would turn a rename into a copy.
type Trasher struct {
	// HomeTrash is $XDG_DATA_HOME/Trash; used for files on the same volume as it.
	HomeTrash     string
	HomeVolumeKey string
	UID           int
	// TopDir returns the directory holding a volume's .Trash-$UID; defaults to the mount point.
	TopDir func(volume.Volume) string
}

// NewTrasher configures a Trasher for the current user.
func NewTrasher(res *volume.Resolver) *Trasher {
	data := os.Getenv("XDG_DATA_HOME")
	if data == "" {
		home, _ := os.UserHomeDir()
		data = filepath.Join(home, ".local", "share")
	}
	t := &Trasher{HomeTrash: filepath.Join(data, "Trash"), UID: os.Getuid()}
	// The home trash may not exist yet; resolve the volume of its nearest existing parent.
	for p := t.HomeTrash; p != "/" && p != "."; p = filepath.Dir(p) {
		if v, _, err := res.Resolve(p); err == nil {
			t.HomeVolumeKey = v.Key
			break
		}
	}
	return t
}

// Location describes where a volume's trash is.
type Location struct {
	Dir    string // trash directory containing files/ and info/
	TopDir string // "" for the home trash; otherwise paths in .trashinfo are relative to it
	Exists bool
}

// Locate returns the trash for files on v without creating anything.
func (t *Trasher) Locate(v volume.Volume) Location {
	if v.Key == t.HomeVolumeKey && t.HomeTrash != "" {
		return Location{Dir: t.HomeTrash, Exists: isDir(t.HomeTrash)}
	}
	top := v.MountPoint
	if t.TopDir != nil {
		top = t.TopDir(v)
	}
	// Spec: $topdir/.Trash/$uid if .Trash is a real sticky directory, else $topdir/.Trash-$uid.
	shared := filepath.Join(top, ".Trash")
	if st, err := os.Lstat(shared); err == nil && st.IsDir() && st.Mode()&os.ModeSticky != 0 {
		dir := filepath.Join(shared, fmt.Sprint(t.UID))
		return Location{Dir: dir, TopDir: top, Exists: isDir(dir)}
	}
	dir := filepath.Join(top, fmt.Sprintf(".Trash-%d", t.UID))
	return Location{Dir: dir, TopDir: top, Exists: isDir(dir)}
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func deviceOf(p string) (uint64, error) {
	st, err := os.Stat(p)
	if err != nil {
		return 0, err
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, errors.New("no device information")
	}
	return uint64(sys.Dev), nil
}

// Check verifies that abs can be trashed into loc by rename: the trash (or the directory
// it will be created in) must be on the same device as the file.
func (t *Trasher) Check(abs string, loc Location) error {
	fileDev, err := deviceOf(abs)
	if err != nil {
		return err
	}
	probe := loc.Dir
	for !isDir(probe) {
		parent := filepath.Dir(probe)
		if parent == probe {
			break
		}
		probe = parent
	}
	trashDev, err := deviceOf(probe)
	if err != nil {
		return err
	}
	if fileDev != trashDev {
		return fmt.Errorf("trash %s is on a different device than %s; refusing to copy into it", loc.Dir, abs)
	}
	return nil
}

// Trash moves abs into the trash, returning the trashed file's path and its .trashinfo.
func (t *Trasher) Trash(abs string, loc Location, now time.Time) (string, string, error) {
	if err := t.Check(abs, loc); err != nil {
		return "", "", err
	}
	files, info := filepath.Join(loc.Dir, "files"), filepath.Join(loc.Dir, "info")
	for _, d := range []string{files, info} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return "", "", fmt.Errorf("creating trash: %w", err)
		}
	}
	orig := abs
	if loc.TopDir != "" {
		if rel, err := filepath.Rel(loc.TopDir, abs); err == nil && !strings.HasPrefix(rel, "..") {
			orig = rel
		}
	}
	content := fmt.Sprintf("[Trash Info]\nPath=%s\nDeletionDate=%s\n", escapePath(orig), now.Format("2006-01-02T15:04:05"))

	// Reserve a unique name by creating the .trashinfo exclusively, as the spec requires.
	base := filepath.Base(abs)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	for i := 1; i < 10000; i++ {
		name := base
		if i > 1 {
			name = fmt.Sprintf("%s.%d%s", stem, i, ext)
		}
		infoPath := filepath.Join(info, name+".trashinfo")
		f, err := os.OpenFile(infoPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", "", err
		}
		target := filepath.Join(files, name)
		if _, err := os.Lstat(target); err == nil {
			f.Close()
			os.Remove(infoPath)
			continue
		}
		_, werr := f.WriteString(content)
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			os.Remove(infoPath)
			return "", "", werr
		}
		if err := renameNoReplace(abs, target); err != nil {
			os.Remove(infoPath)
			if errors.Is(err, syscall.EXDEV) {
				return "", "", fmt.Errorf("trash is on a different device; refusing to copy: %w", err)
			}
			return "", "", err
		}
		return target, infoPath, nil
	}
	return "", "", errors.New("could not find a free name in the trash")
}

// escapePath percent-encodes each path segment as the trash spec requires.
func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

// Usage summarizes a trash directory's contents.
type Usage struct {
	Items int
	Bytes int64
}

// Usage walks loc's files/ directory.
func (t *Trasher) Usage(loc Location) (Usage, error) {
	var u Usage
	files := filepath.Join(loc.Dir, "files")
	entries, err := os.ReadDir(files)
	if errors.Is(err, os.ErrNotExist) {
		return u, nil
	}
	if err != nil {
		return u, err
	}
	u.Items = len(entries)
	err = filepath.WalkDir(files, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				u.Bytes += info.Size()
			}
		}
		return nil
	})
	return u, err
}
