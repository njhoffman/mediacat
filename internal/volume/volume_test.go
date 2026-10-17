package volume

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fixture = `22 1 259:2 / / rw,relatime shared:1 - ext4 /dev/nvme0n1p2 rw,errors=remount-ro
23 22 0:21 / /proc rw,nosuid shared:2 - proc proc rw
24 22 259:1 / /boot/efi rw,relatime shared:3 - vfat /dev/nvme0n1p1 rw
90 22 0:55 / /mnt/nfs rw,nosuid,nodev,relatime shared:50 - cifs //192.168.1.248/share rw,vers=3.1.1
91 22 8:17 / /media/nicholas/My\040Drive rw,relatime shared:51 - exfat /dev/sdb1 rw
92 22 259:2 /home/nicholas/media /srv/media rw,relatime shared:1 - ext4 /dev/nvme0n1p2 rw
93 22 0:60 / /mnt/nfs/Stash rw - nfs4 nas:/export/stash/ rw
`

func testResolver(t *testing.T) *Resolver {
	t.Helper()
	mounts, err := ParseMountInfo(strings.NewReader(fixture))
	if err != nil {
		t.Fatal(err)
	}
	return &Resolver{
		Mounts: mounts,
		UUIDOf: func(dev string) string {
			return map[string]string{"/dev/nvme0n1p2": "root-uuid", "/dev/sdb1": "ABCD-1234"}[dev]
		},
		RotationalOf: func(maj, min int) bool { return maj == 8 },
	}
}

func TestParseMountInfo(t *testing.T) {
	r := testResolver(t)
	if len(r.Mounts) != 7 {
		t.Fatalf("got %d mounts", len(r.Mounts))
	}
	m := r.Mounts[4]
	if m.MountPoint != "/media/nicholas/My Drive" || m.FSType != "exfat" || m.Major != 8 || m.Minor != 17 {
		t.Errorf("escaped mount parsed wrong: %+v", m)
	}
	if r.Mounts[5].Root != "/home/nicholas/media" {
		t.Errorf("bind root = %q", r.Mounts[5].Root)
	}
}

func TestParseMountInfoErrors(t *testing.T) {
	for _, in := range []string{"1 2 3:4 / /x rw", "1 2 34 / /x rw - ext4 /dev/x rw", "1 2 - ext4"} {
		if _, err := ParseMountInfo(strings.NewReader(in)); err == nil {
			t.Errorf("expected error for %q", in)
		}
	}
}

func TestFind(t *testing.T) {
	r := testResolver(t)
	cases := map[string]string{
		"/home/nicholas":             "/",
		"/mnt/nfs":                   "/mnt/nfs",
		"/mnt/nfs/Stash/Sorted/x":    "/mnt/nfs/Stash",
		"/mnt/nfsother":              "/",
		"/boot/efi/EFI":              "/boot/efi",
		"/media/nicholas/My Drive/a": "/media/nicholas/My Drive",
	}
	for path, want := range cases {
		m, ok := r.Find(path)
		if !ok || m.MountPoint != want {
			t.Errorf("Find(%q) = %q, want %q", path, m.MountPoint, want)
		}
	}
}

func TestVolumeKeys(t *testing.T) {
	r := testResolver(t)
	cases := []struct {
		mountPoint, key string
		network, rot    bool
	}{
		{"/mnt/nfs", "net://192.168.1.248/share", true, false},
		{"/mnt/nfs/Stash", "net:nas:/export/stash", true, false},
		{"/media/nicholas/My Drive", "uuid:ABCD-1234", false, true},
		{"/", "uuid:root-uuid", false, false},
		{"/boot/efi", "dev:/dev/nvme0n1p1", false, false},
		{"/proc", "mnt:proc:/proc", false, false},
	}
	for _, c := range cases {
		m, _ := r.Find(c.mountPoint)
		v := r.volumeFor(m)
		if v.Key != c.key || v.Network != c.network || v.Rotational != c.rot {
			t.Errorf("%s: got key=%q net=%v rot=%v", c.mountPoint, v.Key, v.Network, v.Rotational)
		}
	}
}

func TestBindMountSharesVolumeAndRelpath(t *testing.T) {
	r := testResolver(t)
	direct, _ := r.Find("/home/nicholas/media/a.mp4")
	bind, _ := r.Find("/srv/media/a.mp4")
	if r.volumeFor(direct).Key != r.volumeFor(bind).Key {
		t.Fatal("bind mount should share the volume key")
	}
	if p, ok := AbsPath(bind.MountPoint, bind.Root, "home/nicholas/media/a.mp4"); !ok || p != "/srv/media/a.mp4" {
		t.Errorf("AbsPath via bind = %q %v", p, ok)
	}
	if _, ok := AbsPath(bind.MountPoint, bind.Root, "etc/passwd"); ok {
		t.Error("path outside bind root should not resolve")
	}
	if p, ok := AbsPath("/mnt/nfs", "/", "Stash/a.mp4"); !ok || p != "/mnt/nfs/Stash/a.mp4" {
		t.Errorf("AbsPath = %q", p)
	}
}

func TestResolveRealPath(t *testing.T) {
	r, err := NewResolver()
	if err != nil {
		t.Skip("no mountinfo:", err)
	}
	dir := t.TempDir()
	sub := filepath.Join(dir, "a b")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	v, rel, err := r.Resolve(sub)
	if err != nil {
		t.Fatal(err)
	}
	if v.Key == "" || !strings.HasSuffix(rel, "a b") || strings.HasPrefix(rel, "/") {
		t.Errorf("Resolve: key=%q rel=%q", v.Key, rel)
	}
	if back, ok := v.AbsPath(rel); !ok {
		t.Errorf("AbsPath(%q) failed", rel)
	} else if real, _ := filepath.EvalSymlinks(sub); back != real {
		t.Errorf("round trip %q != %q", back, real)
	}
	if _, _, err := r.Resolve(filepath.Join(dir, "missing")); err == nil {
		t.Error("expected error for missing path")
	}
}

func TestUnescape(t *testing.T) {
	if got := unescape(`a\040b\011c\134d\x`); got != "a b\tc\\d\\x" {
		t.Errorf("unescape = %q", got)
	}
}

func TestLookup(t *testing.T) {
	r := testResolver(t)
	v, ok := r.Lookup("uuid:root-uuid")
	if !ok || v.MountPoint != "/" {
		t.Errorf("Lookup root = %+v %v", v, ok)
	}
	if v, ok := r.Lookup("net://192.168.1.248/share"); !ok || v.MountPoint != "/mnt/nfs" {
		t.Errorf("Lookup share = %+v", v)
	}
	if _, ok := r.Lookup("uuid:unplugged"); ok {
		t.Error("unplugged volume should not resolve")
	}
}
