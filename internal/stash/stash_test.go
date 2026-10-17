package stash

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLibrariesAndAffected(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yml")
	cfg := `database: /x/stash-go.sqlite
stash:
    - excludeimage: false
      path: /mnt/nfs/Stash/Sorted/Stars
    - path: /mnt/nfs/Stash/Sorted/
calculate_md5: false
`
	os.WriteFile(p, []byte(cfg), 0o644)
	libs, err := Libraries(p)
	if err != nil || len(libs) != 2 || libs[1] != "/mnt/nfs/Stash/Sorted" {
		t.Fatalf("Libraries = %v %v", libs, err)
	}
	got := Affected([]string{"/mnt/nfs/Stash/Sorted/a.mp4", "/mnt/nfs/Stash/Unsorted/b.mp4", "/mnt/nfs/Stash/SortedX/c.mp4"}, libs)
	if len(got) != 1 || got[0] != "/mnt/nfs/Stash/Sorted/a.mp4" {
		t.Errorf("Affected = %v", got)
	}
	if libs, err := Libraries(filepath.Join(t.TempDir(), "none.yml")); err != nil || libs != nil {
		t.Errorf("missing config: %v %v", libs, err)
	}
	os.WriteFile(p, []byte("stash: [unterminated"), 0o644)
	if _, err := Libraries(p); err == nil {
		t.Error("bad yaml should error")
	}
}

func TestRealConfigParses(t *testing.T) {
	libs, err := Libraries(DefaultConfig())
	if err != nil {
		t.Fatalf("real Stash config: %v", err)
	}
	t.Logf("%d Stash libraries", len(libs))
}
