package dupes

import (
	"os"
	"syscall"
	"testing"

	"github.com/njhoffman/mediacat/internal/catalog"
)

func inodeOf(info os.FileInfo) uint64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return st.Ino
	}
	return 0
}

func file(path string, mtime int64) catalog.File {
	return catalog.File{AbsPath: path, MtimeNS: mtime}
}

func TestCopyMarkers(t *testing.T) {
	cases := map[string]int{
		"/x/Movie.mp4": 0, "/x/Movie (1).mp4": 1, "/x/Movie - Copy.mp4": 1, "/x/Movie copy 2.mkv": 1,
		"/x/Movie_2.mp4": 1, "/x/Part 2.mp4": 1, "/x/Part 2 (1).mp4": 2, "/x/2.mp4": 0, "/x/Scene 160.mp4": 0,
	}
	for in, want := range cases {
		if got := copyMarkers(in); got != want {
			t.Errorf("copyMarkers(%q) = %d want %d", in, got, want)
		}
	}
}

func TestPolicyRules(t *testing.T) {
	files := []catalog.File{
		file("/lib/Unsorted/incoming/movie (1).mp4", 100),
		file("/lib/Sorted/Stars/movie.mp4", 300),
		file("/lib/a/movie.mp4", 200),
	}
	cases := []struct {
		spec          string
		prefer, avoid []string
		want          int
	}{
		{DefaultPolicy, nil, nil, 2},                     // plain, then oldest of the plain ones
		{"oldest", nil, nil, 0},                          // copy markers ignored
		{"newest", nil, nil, 1},                          //
		{"shortest", nil, nil, 2},                        //
		{"longest", nil, nil, 0},                         //
		{DefaultPolicy, []string{"/lib/Sorted"}, nil, 1}, // prefer wins first
		{DefaultPolicy, []string{"/lib/x", "/lib/Unsorted"}, nil, 0},
		{DefaultPolicy, nil, []string{"/lib/a"}, 1},          // avoid pushes to the next rule
		{"avoid,oldest", nil, []string{"/lib/Unsorted/"}, 2}, // trailing slash tolerated
	}
	for _, c := range cases {
		p, err := ParsePolicy(c.spec, c.prefer, c.avoid)
		if err != nil {
			t.Fatal(err)
		}
		if got := p.Choose(files); got != c.want {
			t.Errorf("%s prefer=%v avoid=%v: kept %d, want %d", c.spec, c.prefer, c.avoid, got, c.want)
		}
	}
}

func TestPolicyTieBreakAndErrors(t *testing.T) {
	p, _ := ParsePolicy("oldest", nil, nil)
	if got := p.Choose([]catalog.File{file("/b", 1), file("/a", 1)}); got != 1 {
		t.Errorf("tie should go to the first path, got %d", got)
	}
	for _, spec := range []string{"", " , ", "largest"} {
		if _, err := ParsePolicy(spec, nil, nil); err == nil {
			t.Errorf("ParsePolicy(%q) should fail", spec)
		}
	}
	if p, _ := ParsePolicy(" Oldest , SHORTEST ", nil, nil); len(p.Rules) != 2 || p.Rules[1] != "shortest" {
		t.Errorf("normalization: %v", p.Rules)
	}
}

func TestPathsRule(t *testing.T) {
	files := []catalog.File{
		file("/mnt/nfs/Stash/Enhanced/Incoming/a.mp4", 100),
		file("/mnt/nfs/Stash/SORTED/Stars/a.mp4", 300),
		file("/mnt/nfs/Stash/Unsorted/a.mp4", 200),
	}
	p, _ := ParsePolicy("paths,oldest", nil, nil)
	if err := p.SetPathPatterns([]string{"/sorted", "^incoming"}); err != nil {
		t.Fatal(err)
	}
	if got := p.Choose(files); got != 1 {
		t.Errorf("sorted should win (case-insensitive): kept %d", got)
	}
	// Without a preferred match, avoiding incoming leaves Unsorted, despite incoming being oldest.
	if got := p.Choose([]catalog.File{files[0], files[2]}); got != 1 {
		t.Errorf("^incoming should lose: kept %d", got)
	}
	// Plain "sorted" also matches "Unsorted": the earlier pattern ranks, ties fall to oldest.
	q, _ := ParsePolicy("paths,oldest", nil, nil)
	q.SetPathPatterns([]string{"sorted", "^incoming"})
	if got := q.Choose([]catalog.File{files[1], files[2]}); got != 1 {
		t.Errorf("both contain 'sorted', oldest should decide: kept %d", got)
	}
	// An avoided match outweighs a preferred one.
	r, _ := ParsePolicy("paths", nil, nil)
	r.SetPathPatterns([]string{"sorted", "^incoming"})
	both := file("/lib/Sorted/Incoming/a.mp4", 1)
	plain := file("/lib/other/a.mp4", 2)
	if got := r.Choose([]catalog.File{both, plain}); got != 1 {
		t.Errorf("avoid should outweigh prefer: kept %d", got)
	}
	if err := r.SetPathPatterns([]string{"^"}); err == nil {
		t.Error("empty pattern accepted")
	}
}
