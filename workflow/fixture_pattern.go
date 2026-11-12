package workflow

import (
	"fmt"
	"regexp"
	"regexp/syntax"
	"strings"
)

const fixtureStringLimit = 4096

// patternFixture constructs a bounded witness rather than hoping an arbitrary
// generated string matches. Final JSON Schema validation remains authoritative.
func patternFixture(pattern string, seed uint64, minimum, maximum int) (string, error) {
	tree, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return "", fmt.Errorf("rehearsal pattern needs a literal scenario output: %w", err)
	}
	matcher, err := regexp.Compile(pattern)
	if err != nil {
		return "", err
	}
	g := patternGenerator{seed: seed, remaining: 65536, widths: map[*syntax.Regexp]patternWidth{}}
	width, err := g.width(tree, 0)
	if err != nil {
		return "", err
	}
	value, err := g.generate(tree, min(minimum, width.max), maximum, 0)
	if err != nil {
		return "", err
	}
	// JSON Schema patterns are searches unless anchored. Test padding rather
	// than assuming that an arbitrary prefix preserves zero-width assertions.
	padding := max(0, minimum-len(value))
	for _, extra := range []int{padding, max(2, padding)} {
		if len(value)+extra > maximum {
			continue
		}
		for _, before := range []int{0, extra, extra / 2} {
			candidate := strings.Repeat("x", before) + string(value) + strings.Repeat("x", extra-before)
			if matcher.MatchString(candidate) {
				return candidate, nil
			}
		}
	}
	return "", fmt.Errorf("pattern has no supported bounded rehearsal witness; provide a literal scenario output")
}

type patternWidth struct{ min, max int }
type patternGenerator struct {
	seed      uint64
	remaining int
	widths    map[*syntax.Regexp]patternWidth
}

func (g *patternGenerator) step(depth int) error {
	g.remaining--
	if depth > 64 || g.remaining < 0 {
		return fmt.Errorf("rehearsal pattern analysis exceeds bounds")
	}
	return nil
}
func (g *patternGenerator) choice(n int) int {
	if n <= 1 {
		return 0
	}
	g.seed = g.seed*6364136223846793005 + 1442695040888963407
	return int((g.seed >> 1) % uint64(n))
}
func (g *patternGenerator) width(r *syntax.Regexp, depth int) (patternWidth, error) {
	if w, ok := g.widths[r]; ok {
		return w, nil
	}
	if err := g.step(depth); err != nil {
		return patternWidth{}, err
	}
	w := patternWidth{}
	switch r.Op {
	case syntax.OpNoMatch:
		return w, fmt.Errorf("pattern has no match")
	case syntax.OpLiteral:
		w = patternWidth{len(r.Rune), len(r.Rune)}
	case syntax.OpCharClass, syntax.OpAnyCharNotNL, syntax.OpAnyChar:
		w = patternWidth{1, 1}
	case syntax.OpCapture:
		var err error
		w, err = g.width(r.Sub[0], depth+1)
		if err != nil {
			return w, err
		}
	case syntax.OpConcat:
		for _, s := range r.Sub {
			v, err := g.width(s, depth+1)
			if err != nil {
				return w, err
			}
			w.min = min(fixtureStringLimit+1, w.min+v.min)
			w.max = min(fixtureStringLimit, w.max+v.max)
		}
	case syntax.OpAlternate:
		w.min = fixtureStringLimit + 1
		for _, s := range r.Sub {
			v, err := g.width(s, depth+1)
			if err != nil {
				return w, err
			}
			w.min = min(w.min, v.min)
			w.max = max(w.max, v.max)
		}
	case syntax.OpStar, syntax.OpPlus, syntax.OpQuest, syntax.OpRepeat:
		v, err := g.width(r.Sub[0], depth+1)
		if err != nil {
			return w, err
		}
		low, high := patternRepeat(r)
		w.min = min(fixtureStringLimit+1, v.min*low)
		if high < 0 {
			w.max = fixtureStringLimit
		} else {
			w.max = min(fixtureStringLimit, v.max*high)
		}
	case syntax.OpEmptyMatch, syntax.OpBeginLine, syntax.OpEndLine, syntax.OpBeginText, syntax.OpEndText, syntax.OpWordBoundary, syntax.OpNoWordBoundary:
	default:
		return w, fmt.Errorf("unsupported rehearsal pattern operation %s", r.Op)
	}
	g.widths[r] = w
	return w, nil
}
func patternRepeat(r *syntax.Regexp) (int, int) {
	switch r.Op {
	case syntax.OpStar:
		return 0, -1
	case syntax.OpPlus:
		return 1, -1
	case syntax.OpQuest:
		return 0, 1
	default:
		return r.Min, r.Max
	}
}
func (g *patternGenerator) generate(r *syntax.Regexp, need, room, depth int) ([]rune, error) {
	if err := g.step(depth); err != nil {
		return nil, err
	}
	w := g.widths[r]
	if room < w.min || room < 0 {
		return nil, fmt.Errorf("pattern exceeds fixture string bound")
	}
	switch r.Op {
	case syntax.OpLiteral:
		return append([]rune(nil), r.Rune...), nil
	case syntax.OpCharClass:
		pairs := len(r.Rune) / 2
		if pairs == 0 {
			return nil, fmt.Errorf("empty pattern character class")
		}
		i := 2 * g.choice(pairs)
		return []rune{r.Rune[i] + rune(g.choice(int(r.Rune[i+1]-r.Rune[i])+1))}, nil
	case syntax.OpAnyChar, syntax.OpAnyCharNotNL:
		return []rune{'a' + rune(g.choice(26))}, nil
	case syntax.OpCapture:
		return g.generate(r.Sub[0], need, room, depth+1)
	case syntax.OpConcat:
		out := []rune{}
		suffix := make([]patternWidth, len(r.Sub)+1)
		for i := len(r.Sub) - 1; i >= 0; i-- {
			v := g.widths[r.Sub[i]]
			suffix[i] = patternWidth{min(fixtureStringLimit+1, v.min+suffix[i+1].min), min(fixtureStringLimit, v.max+suffix[i+1].max)}
		}
		for i, s := range r.Sub {
			target := max(0, need-len(out)-suffix[i+1].max)
			// Prefer earlier variable parts for the requested minimum, while keeping
			// enough room for the suffix's mandatory literals.
			target = max(target, min(g.widths[s].max, max(0, need-len(out)-suffix[i+1].min)))
			part, err := g.generate(s, target, room-len(out)-suffix[i+1].min, depth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, part...)
		}
		return out, nil
	case syntax.OpAlternate:
		start := g.choice(len(r.Sub))
		for n := range len(r.Sub) {
			s := r.Sub[(start+n)%len(r.Sub)]
			v := g.widths[s]
			if v.min <= room && v.max >= need {
				return g.generate(s, need, room, depth+1)
			}
		}
		return nil, fmt.Errorf("no pattern alternative fits fixture bounds")
	case syntax.OpStar, syntax.OpPlus, syntax.OpQuest, syntax.OpRepeat:
		child := r.Sub[0]
		v := g.widths[child]
		low, high := patternRepeat(r)
		if v.min > 0 {
			if high < 0 {
				high = room / v.min
			} else {
				high = min(high, room/v.min)
			}
		} else {
			if high < 0 {
				high = max(low, min(room+1, fixtureStringLimit))
			}
		}
		if v.max > 0 {
			low = max(low, (need+v.max-1)/v.max)
		}
		if low > high {
			return nil, fmt.Errorf("pattern repetition cannot fit fixture bounds")
		}
		count := low + g.choice(min(8, high-low+1))
		out := []rune{}
		for i := 0; i < count; i++ {
			target := max(0, need-len(out)-(count-i-1)*v.max)
			target = max(target, min(v.max, max(0, need-len(out)-(count-i-1)*v.min)))
			part, err := g.generate(child, target, room-len(out)-(count-i-1)*v.min, depth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, part...)
		}
		return out, nil
	default:
		return nil, nil
	}
}
