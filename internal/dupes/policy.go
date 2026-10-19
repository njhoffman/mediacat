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
	"paths":    `keep copies whose path contains a preferred pattern; "^pattern" avoids it (case-sensitive)`,
	"plain":    `keep names without copy markers like "(1)", " - Copy", "_2"`,
	"oldest":   "keep the oldest modification time",
	"newest":   "keep the newest modification time",
	"shortest": "keep the shortest path",
	"longest":  "keep the longest path",
	"res":      "keep the highest resolution (pixel count)",
	"bitrate":  "keep the highest video bitrate (overall bitrate when unknown)",
	"size":     "keep the largest file",
}

// DefaultPolicy is used when no --keep is given.
const DefaultPolicy = "prefer,avoid,paths,plain,oldest,shortest"

// DefaultSimilarPolicy is used for similar (not identical) groups when no --keep is
// given: the copies differ, so quality comes first.
const DefaultSimilarPolicy = "res,bitrate,prefer,avoid,paths,plain,size,oldest"

// Policy chooses which copy in a group to keep.
type Policy struct {
	Rules  []string
	Prefer []string // absolute directories
	Avoid  []string
	// Path patterns for the "paths" rule, lowercased: substrings to prefer, and ones to
	// avoid (written with a leading "^").
	PathPrefer []string
	PathAvoid  []string
}

// SetPathPatterns parses preferred-path patterns such as ["/Sorted", "^/Incoming"]. They
// match case-sensitively.
func (p *Policy) SetPathPatterns(patterns []string) error {
	p.PathPrefer, p.PathAvoid = nil, nil
	for _, pat := range patterns {
		avoid := strings.HasPrefix(pat, "^")
		pat = strings.TrimSpace(strings.TrimPrefix(pat, "^"))
		if pat == "" {
			return fmt.Errorf("empty preferred path pattern")
		}
		if avoid {
			p.PathAvoid = append(p.PathAvoid, pat)
		} else {
			p.PathPrefer = append(p.PathPrefer, pat)
		}
	}
	return nil
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
	case "paths":
		return p.pathScore(f.AbsPath)
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
	case "res":
		return -int64(f.Meta.Width) * int64(f.Meta.Height)
	case "bitrate":
		if f.Meta.VideoBitrate > 0 {
			return -f.Meta.VideoBitrate
		}
		return -f.Meta.Bitrate
	case "size":
		return -f.Size
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

// pathScore ranks a path by its patterns: each avoided pattern it contains outweighs any
// preference; among preferred patterns, earlier ones rank higher. Lower is better.
func (p Policy) pathScore(path string) int64 {
	score := int64(len(p.PathPrefer))
	for i, pat := range p.PathPrefer {
		if strings.Contains(path, pat) {
			score = int64(i)
			break
		}
	}
	for _, pat := range p.PathAvoid {
		if strings.Contains(path, pat) {
			score += int64(len(p.PathPrefer)) + 1
		}
	}
	return score
}
