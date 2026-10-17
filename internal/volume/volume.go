// Package volume maps filesystem paths to stable volume identities so the catalog
// survives a drive being remounted at a different path.
package volume

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Mount is one entry from /proc/self/mountinfo.
type Mount struct {
	Major, Minor int
	Root         string // path inside the filesystem that is mounted (non-"/" for bind mounts)
	MountPoint   string
	FSType       string
	Source       string
}

// Volume identifies a filesystem independent of where it is currently mounted.
type Volume struct {
	Key        string // stable identity, e.g. "net://192.168.1.248/share" or "uuid:967e..."
	Source     string
	FSType     string
	UUID       string
	MountPoint string
	MountRoot  string
	Network    bool
	Rotational bool // spinning disk; network volumes report false
}

// Slow reports whether the volume should be accessed with low concurrency.
func (v Volume) Slow() bool { return v.Network || v.Rotational }

// AbsPath converts a catalog relpath back to an absolute path at the current mount.
func (v Volume) AbsPath(relpath string) (string, bool) {
	return AbsPath(v.MountPoint, v.MountRoot, relpath)
}

// AbsPath converts a filesystem-relative path to an absolute path given a mount point and
// the filesystem root that is mounted there. It returns false when relpath lies outside
// the mounted subtree.
func AbsPath(mountPoint, mountRoot, relpath string) (string, bool) {
	root := strings.Trim(mountRoot, "/")
	switch {
	case root == "":
	case relpath == root:
		relpath = ""
	case strings.HasPrefix(relpath, root+"/"):
		relpath = relpath[len(root)+1:]
	default:
		return "", false
	}
	return filepath.Join(mountPoint, relpath), true
}

var networkFS = map[string]bool{
	"cifs": true, "smb3": true, "smbfs": true, "nfs": true, "nfs4": true,
	"fuse.sshfs": true, "9p": true, "afs": true, "ceph": true, "glusterfs": true,
	"fuse.rclone": true, "davfs": true,
}

// ParseMountInfo parses the /proc/self/mountinfo format.
func ParseMountInfo(r io.Reader) ([]Mount, error) {
	var mounts []Mount
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		pre, post, ok := strings.Cut(line, " - ")
		if !ok {
			return nil, fmt.Errorf("mountinfo: missing separator: %q", line)
		}
		f := strings.Fields(pre)
		g := strings.Fields(post)
		if len(f) < 5 || len(g) < 2 {
			return nil, fmt.Errorf("mountinfo: short line: %q", line)
		}
		maj, min, ok := strings.Cut(f[2], ":")
		if !ok {
			return nil, fmt.Errorf("mountinfo: bad device %q", f[2])
		}
		m := Mount{
			Root:       unescape(f[3]),
			MountPoint: unescape(f[4]),
			FSType:     g[0],
			Source:     unescape(g[1]),
		}
		m.Major, _ = strconv.Atoi(maj)
		m.Minor, _ = strconv.Atoi(min)
		mounts = append(mounts, m)
	}
	return mounts, sc.Err()
}

// unescape decodes the octal escapes (\040 etc.) the kernel uses in mountinfo.
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// Resolver resolves paths to volumes. The lookup functions are injectable for tests.
type Resolver struct {
	Mounts []Mount
	// UUIDOf returns the filesystem UUID for a block device path, or "".
	UUIDOf func(device string) string
	// RotationalOf reports whether the block device major:minor is a spinning disk.
	RotationalOf func(major, minor int) bool
}

// NewResolver reads the live mount table.
func NewResolver() (*Resolver, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	mounts, err := ParseMountInfo(f)
	if err != nil {
		return nil, err
	}
	return &Resolver{Mounts: mounts, UUIDOf: uuidFromDisk("/dev/disk/by-uuid"), RotationalOf: sysRotational}, nil
}

// Find returns the mount containing abs, which must be an absolute, symlink-free path.
// When mounts are stacked on the same mount point the last one wins, as in the kernel.
func (r *Resolver) Find(abs string) (Mount, bool) {
	best, found := Mount{}, false
	for _, m := range r.Mounts {
		if !within(abs, m.MountPoint) {
			continue
		}
		if !found || len(m.MountPoint) >= len(best.MountPoint) {
			best, found = m, true
		}
	}
	return best, found
}

func within(path, dir string) bool {
	if dir == "/" {
		return strings.HasPrefix(path, "/")
	}
	return path == dir || strings.HasPrefix(path, dir+"/")
}

// Resolve returns the volume containing path and path's location relative to the
// filesystem root (no leading slash; "" for the root itself).
func (r *Resolver) Resolve(path string) (Volume, string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return Volume{}, "", err
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return Volume{}, "", err
	}
	m, ok := r.Find(abs)
	if !ok {
		return Volume{}, "", fmt.Errorf("no mount found for %s", abs)
	}
	v := r.volumeFor(m)
	under := strings.TrimPrefix(strings.TrimPrefix(abs, m.MountPoint), "/")
	rel := strings.Trim(filepath.ToSlash(filepath.Join(strings.Trim(m.Root, "/"), under)), "/")
	if rel == "." {
		rel = ""
	}
	return v, rel, nil
}

func (r *Resolver) volumeFor(m Mount) Volume {
	v := Volume{Source: m.Source, FSType: m.FSType, MountPoint: m.MountPoint, MountRoot: m.Root}
	switch {
	case networkFS[m.FSType]:
		v.Network = true
		v.Key = "net:" + strings.TrimRight(m.Source, "/")
	default:
		if r.UUIDOf != nil {
			v.UUID = r.UUIDOf(m.Source)
		}
		if v.UUID != "" {
			v.Key = "uuid:" + v.UUID
		} else if strings.HasPrefix(m.Source, "/dev/") {
			v.Key = "dev:" + m.Source
		} else {
			v.Key = "mnt:" + m.FSType + ":" + m.MountPoint
		}
		if r.RotationalOf != nil {
			v.Rotational = r.RotationalOf(m.Major, m.Minor)
		}
	}
	return v
}

// uuidFromDisk maps a device path to its UUID by scanning the by-uuid symlink directory.
func uuidFromDisk(dir string) func(string) string {
	return func(device string) string {
		if !strings.HasPrefix(device, "/dev/") {
			return ""
		}
		target, err := filepath.EvalSymlinks(device)
		if err != nil {
			return ""
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return ""
		}
		for _, e := range entries {
			p, err := filepath.EvalSymlinks(filepath.Join(dir, e.Name()))
			if err == nil && p == target {
				return e.Name()
			}
		}
		return ""
	}
}

// sysRotational reads queue/rotational for a block device, walking up to the parent disk
// for partitions.
func sysRotational(major, minor int) bool {
	dev, err := filepath.EvalSymlinks(fmt.Sprintf("/sys/dev/block/%d:%d", major, minor))
	if err != nil {
		return false
	}
	for _, p := range []string{filepath.Join(dev, "queue/rotational"), filepath.Join(filepath.Dir(dev), "queue/rotational")} {
		if b, err := os.ReadFile(p); err == nil {
			return strings.TrimSpace(string(b)) == "1"
		}
	}
	return false
}

// Lookup returns the currently mounted volume with key. When the same filesystem is
// mounted more than once, the mount of its root is preferred.
func (r *Resolver) Lookup(key string) (Volume, bool) {
	var best Volume
	found := false
	for _, m := range r.Mounts {
		v := r.volumeFor(m)
		if v.Key != key {
			continue
		}
		if !found || (m.Root == "/" && best.MountRoot != "/") {
			best, found = v, true
		}
	}
	return best, found
}
