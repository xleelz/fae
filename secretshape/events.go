package secretshape

import (
	"bytes"
	"slices"
)

// EventEnd and EventGap bound the go test -json text between two output
// events' texts: what ends the first event's line after its output (the
// output string's closing quote, an OutputType field, the closing brace),
// and the next event's fields before its output.
const (
	EventEnd = 64
	EventGap = 640
)

// eventLine bounds how far back EventText looks for the start of the output
// event that ends: go test -json writes at most 1 KiB of output to one
// event, each byte escaped in at most 6 (\u00XX), after its time, action,
// package and test.
const eventLine = 8 << 10

// EventText is where the next go test -json output event's text starts, when
// b[p:] ends an output event's string: a quote, the rest of that event's
// line (EventEnd bytes at most, ending with '}'), and the next line up to its
// "Output" field's opening quote (EventGap bytes at most), in an "output"
// event of the same package and test as the one ending (eventKey). Else -1.
// The event ending is looked for back to its line's start, eventLine bytes at
// most; when its start is not in b (a stream's earlier text), its package and
// test are not compared.
//
// test2json writes each line of a test's output as its own event, and cuts a
// line longer than 1 KiB into several events with no newline between them,
// so a text found across events is the test's own text.
func EventText(b []byte, p int) int {
	if p < 0 || p >= len(b) || b[p] != '"' {
		return -1
	}
	nl := bytes.IndexByte(b[p:min(len(b), p+EventEnd)], '\n')
	if nl < 1 || b[p+nl-1] != '}' {
		return -1
	}
	next := p + nl + 1
	line := b[next:min(len(b), next+EventGap)]
	if end := bytes.IndexByte(line, '\n'); end >= 0 {
		line = line[:end]
	}
	key, at := eventKey(line)
	if len(line) == 0 || line[0] != '{' || at < 0 || !bytes.HasPrefix(key, []byte(`"Action":"output"`)) {
		return -1
	}
	from := max(0, p-eventLine)
	start := from + bytes.LastIndexByte(b[from:p], '\n') + 1
	if ended, known := eventKey(b[start:p]); known >= 0 && !bytes.Equal(ended, key) {
		return -1
	}
	return next + at + len(`"Output":"`)
}

// eventKey is a go test -json event's fields from its "Action" up to its
// "Output" field (its action, package and test, as test2json writes them in
// that order), and where its "Output" field starts in line; -1 when line does
// not hold both. A quote inside the output is escaped, so the first
// `"Output":"` is the field.
func eventKey(line []byte) ([]byte, int) {
	at := bytes.Index(line, []byte(`"Output":"`))
	if at < 0 {
		return nil, -1
	}
	action := bytes.Index(line[:at], []byte(`"Action":`))
	if action < 0 {
		return nil, -1
	}
	return line[action:at], at
}

// outputTypeField is the field test2json writes after an output event's
// output, naming its kind ("frame", "error", "error-continue").
var outputTypeField = []byte(`","OutputType":"`)

// eventTextEnd is where the output string closes in the go test -json event
// whose line ends at the newline b[n]: the line ends with `"}` after it, or
// with an OutputType field. Else -1.
func eventTextEnd(b []byte, n int) int {
	if n < 2 || b[n-1] != '}' || b[n-2] != '"' {
		return -1
	}
	from := max(0, n-EventEnd)
	if t := bytes.LastIndex(b[from:n], outputTypeField); t >= 0 && bytes.IndexAny(b[from+t+len(outputTypeField):n-2], `"\`) < 0 {
		return from + t
	}
	return n - 2
}

// eventGap is the go test -json text between two joined output events'
// texts, b[from:to], which joinedEvents leaves out at view offset at.
type eventGap struct{ at, from, to int }

// eventView is a text as Spans reads it (joinedEvents): b with the text
// between joined events left out, and what was left out.
type eventView struct {
	b    []byte
	gaps []eventGap
	// shift[k] is the text the first k gaps left out.
	shift []int
}

// joinedEvents is b with the text between each two output events that
// EventText joins left out, so that a test's output reads as one escaped
// text. Its b is b itself, with no gaps, when no two events join.
func joinedEvents(b []byte) eventView {
	var gaps []eventGap
	for n := bytes.IndexByte(b, '\n'); n >= 0; {
		if p := eventTextEnd(b, n); p >= 0 {
			if q := EventText(b, p); q > n {
				gaps = append(gaps, eventGap{from: p, to: q})
			}
		}
		next := bytes.IndexByte(b[n+1:], '\n')
		if next < 0 {
			break
		}
		n += 1 + next
	}
	if len(gaps) == 0 {
		return eventView{b: b}
	}
	v := eventView{b: make([]byte, 0, len(b)), gaps: gaps, shift: make([]int, len(gaps)+1)}
	at := 0
	for k := range gaps {
		v.b = append(v.b, b[at:gaps[k].from]...)
		gaps[k].at = len(v.b)
		at = gaps[k].to
		v.shift[k+1] = v.shift[k] + gaps[k].to - gaps[k].from
	}
	v.b = append(v.b, b[at:]...)
	return v
}

// offset is view offset i in the text the view was made from. A span
// starts after a gap at its start, and ends before a gap at its end.
func (v eventView) offset(i int, end bool) int {
	k, _ := slices.BinarySearchFunc(v.gaps, i, func(g eventGap, i int) int {
		if g.at < i || g.at == i && !end {
			return -1
		}
		return 1
	})
	if v.shift == nil {
		return i
	}
	return i + v.shift[k]
}

// limit is the view offset a credential starting at view offset start may
// run to: Max bytes of the text the view was made from, the text between
// joined events included, so that a stream that holds back Max bytes
// (Writer, the daemon's copy) holds a credential whole.
func (v eventView) limit(start int) int {
	limit := min(len(v.b), start+Max)
	if len(v.gaps) == 0 {
		return limit
	}
	// The last view offset whose span from start is Max bytes of the text
	// or fewer.
	from := v.offset(start, false)
	lo, hi := start, limit
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if v.offset(mid, true)-from <= Max {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo
}

// unjoin maps spans found in the view back to the text it was made from: a
// span across a gap takes the gap's text with it, so replacing it leaves the
// first event's line, now ending with the second's, one JSON event.
func (v eventView) unjoin(spans [][2]int) [][2]int {
	if len(v.gaps) == 0 {
		return spans
	}
	out := make([][2]int, len(spans))
	for i, sp := range spans {
		out[i] = [2]int{v.offset(sp[0], false), v.offset(sp[1], true)}
	}
	return out
}
