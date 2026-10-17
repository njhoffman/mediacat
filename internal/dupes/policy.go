package dupes

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/njhoffman/mediacat/internal/catalog"
)

// Rules accepted in a keep policy, applied in order until one decides.
var Rules = map[string]string{
	"prefer":   "keep copies under a --prefer directory (earlier directories win)",
	"avoid":    "keep copies outside --avoid directories",
	"plain":    `keep names without copy markers like "(1)", " - Copy", "_2"`,
	"oldest":   "keep the oldest modification time",
	"newest":   "keep the newest modification time",
	"shortest": "keep the shortest path",
	"longest":  "keep the longest path",
}

// DefaultPolicy is used when no --keep is given.
const DefaultPolicy = "prefer,avoid,plain,oldest,shortest"

// Policy chooses which copy in a group to keep.
type Policy struct {
	Rules  []string
	Prefer []string // absolute directories
	Avoid  []string
}

// ParsePolicy parses a comma-separated rule list.
func ParsePolicy(spec string, prefer, avoid []string) (Policy, error) {
	p := Policy{Prefer: prefer, Avoid: avoid}
	for _, r := range strings.Split(spec, ",") {
		r = strings.TrimSpace(strings.ToLower(r))
		if r == "" {
			continue
		}
		if _, ok := Rules[r]; !ok {
			return p, fmt.Errorf("unknown keep rule %q", r)
		}
		p.Rules = append(p.Rules, r)
	}
	if len(p.Rules) == 0 {
		return p, fmt.Errorf("keep policy is empty")
	}
	return p, nil
}

var copyMarker = regexp.MustCompile(`(?i)(\(\d+\)|\bcopy( \d+)?|\bcopy of\b|[ _-]\d{1,2})$`)

// copyMarkers counts trailing copy markers, so "a 2 (1)" scores worse than "a 2".
func copyMarkers(path string) int {
	base := filepath.Base(path)
	stem := strings.TrimSpace(strings.TrimSuffix(base, filepath.Ext(base)))
	n := 0
	for n < 5 {
		loc := copyMarker.FindStringIndex(stem)
		if loc == nil || loc[0] == 0 {
			break
		}
		stem = strings.TrimSpace(stem[:loc[0]])
		n++
	}
	return n
}

func under(path string, dirs []string) int {
	for i, d := range dirs {
		if path == d || strings.HasPrefix(path, strings.TrimSuffix(d, "/")+"/") {
			return i
		}
	}
	return -1
}

func (p Policy) score(rule string, f catalog.File) int64 {
	switch rule {
	case "prefer":
		if i := under(f.AbsPath, p.Prefer); i >= 0 {
			return int64(i)
		}
		return int64(len(p.Prefer))
	case "avoid":
		if under(f.AbsPath, p.Avoid) >= 0 {
			return 1
		}
		return 0
	case "plain":
		return int64(copyMarkers(f.AbsPath))
	case "oldest":
		return f.MtimeNS
	case "newest":
		return -f.MtimeNS
	case "shortest":
		return int64(len(f.AbsPath))
	case "longest":
		return -int64(len(f.AbsPath))
	}
	return 0
}

// Choose returns the index of the file to keep. Ties after all rules go to the
// lexically first path so the choice is deterministic.
func (p Policy) Choose(files []catalog.File) int {
	best := 0
	for i := 1; i < len(files); i++ {
		if p.less(files[i], files[best]) {
			best = i
		}
	}
	return best
}

func (p Policy) less(a, b catalog.File) bool {
	for _, r := range p.Rules {
		sa, sb := p.score(r, a), p.score(r, b)
		if sa != sb {
			return sa < sb
		}
	}
	return a.AbsPath < b.AbsPath
}
