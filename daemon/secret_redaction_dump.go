package daemon

import (
	"bytes"

	"github.com/xleelz/fae/secretshape"
)

// A hex dump writes a secret's bytes as hex columns, a line at a time, each
// line led by an offset and often followed by the same bytes as text: xxd,
// hexdump -C, od -t x1 (its offsets octal or hex), and Go's hex.Dump, which
// testify prints a []byte with. secretDumpSpans decodes each run of such
// lines and replaces the lines holding the bytes of a raw secret it finds
// whole there, offsets and text columns included.
//
// A dump line (secretDumpLineAt) is, after up to 64 bytes of indentation and
// diff markers (spaces, tabs, '+' and '-') and a go test log prefix
// ("name.go:12: "), an offset of 4 to 16 hex digits with an optional ':', then
// groups of 2, 4 or 8 hex digits, one space apart (two after the eighth
// byte, as hexdump -C writes), holding 1 to secretDumpLineCap bytes; the
// groups end where any other text starts. In go test -json, the line is an
// output event's text. A run is consecutive dump lines, each but the last
// holding secretDumpLineBytes bytes or more.
//
// Not decoded: dumps of words wider than a byte (od -x, hexdump without -C),
// whose byte order is the host's; a dump with no offsets, such as xxd -p or
// od -A n, whose lines are a secret's hex wrapped, found as such
// (secretValues); a dump cut inside the secret; and lines of fewer than
// secretDumpLineBytes bytes.
const (
	// secretDumpLineMax is the longest line read as a dump line, a go test
	// -json event holding one included.
	secretDumpLineMax = 1 << 10
	// secretDumpLineBytes is the fewest bytes a dump line other than a run's
	// last holds: the tools write 16.
	secretDumpLineBytes = 8
	// secretDumpLineCap is the most bytes one dump line holds.
	secretDumpLineCap = 32
)

// secretDumpSpanMax is the most text the dump lines holding an n-byte secret
// span: n/secretDumpLineBytes lines, and one more at each end.
func secretDumpSpanMax(n int) int {
	return (n/secretDumpLineBytes + 2) * (secretDumpLineMax + 1)
}

// secretDumpLine is one decoded dump line.
type secretDumpLine struct {
	// start and end are the text replaced: from the offset to the end of the
	// line, or of its go test -json output before the escaped line break.
	start, end int
	// data are the bytes its hex columns hold.
	data []byte
}

// secretDumpSpans are the dump lines in b that hold one of s's raw secrets
// whole, in order and apart.
func secretDumpSpans(b []byte, s secretValues) [][2]int {
	var raws [][]byte
	for _, f := range s {
		if f.raw {
			raws = append(raws, f.text)
		}
	}
	if len(raws) == 0 {
		return nil
	}
	var spans [][2]int
	var lines []secretDumpLine
	var data []byte
	search := func() {
		for _, raw := range raws {
			for at := 0; ; {
				i := bytes.Index(data[at:], raw)
				if i < 0 {
					break
				}
				first, last := at+i, at+i+len(raw)-1
				from, to, offset := -1, -1, 0
				for k, l := range lines {
					if from < 0 && first < offset+len(l.data) {
						from = k
					}
					if last < offset+len(l.data) {
						to = k
						break
					}
					offset += len(l.data)
				}
				spans = append(spans, [2]int{lines[from].start, lines[to].end})
				at = last + 1
			}
		}
		lines, data = lines[:0], data[:0]
	}
	for at := 0; at < len(b); {
		end := bytes.IndexByte(b[at:], '\n')
		if end < 0 {
			end = len(b)
		} else {
			end += at
		}
		if l, ok := secretDumpLineAt(b, at, end); ok {
			if len(lines) > 0 && len(lines[len(lines)-1].data) < secretDumpLineBytes {
				search() // a short line ends a run
			}
			lines = append(lines, l)
			data = append(data, l.data...)
		} else {
			search()
		}
		at = end + 1
	}
	search()
	return secretshape.Merge(nil, spans)
}

// secretDumpLineAt decodes the line b[start:end] as a dump line.
func secretDumpLineAt(b []byte, start, end int) (secretDumpLine, bool) {
	if end <= start || end-start > secretDumpLineMax {
		return secretDumpLine{}, false
	}
	text, textEnd := start, end
	if b[textEnd-1] == '\r' {
		textEnd--
	}
	if b[start] == '{' {
		// A go test -json output event: the dump line is its text.
		field := []byte(`"Output":"`)
		i := bytes.Index(b[start:end], field)
		if i < 0 {
			return secretDumpLine{}, false
		}
		text = start + i + len(field)
		quote := secretJSONStringEnd(b, text, end)
		if quote < 0 {
			return secretDumpLine{}, false
		}
		textEnd = quote
		if bytes.HasSuffix(b[text:textEnd], []byte(`\n`)) {
			textEnd -= 2
		}
	}
	p := text
	for n := 0; n < 64 && p < textEnd; n++ {
		if c := b[p]; c == ' ' || c == '\t' || c == '+' || c == '-' {
			p++
		} else if c == '\\' && p+1 < textEnd && b[p+1] == 't' {
			p += 2
		} else {
			break
		}
	}
	p = secretLogPrefixEnd(b, p, textEnd)
	offset := p
	for p < textEnd && p-offset <= 16 && secretHexDigit(b[p]) {
		p++
	}
	if n := p - offset; n < 4 || n > 16 {
		return secretDumpLine{}, false
	}
	if p < textEnd && b[p] == ':' {
		p++
	}
	var data []byte
	width := 0
	for len(data) < secretDumpLineCap {
		g := p
		for g < textEnd && b[g] == ' ' {
			g++
		}
		if gap := g - p; gap == 0 || gap > 2 || gap == 2 && len(data) > 0 && len(data) != 8 {
			break
		}
		h := g
		for h < textEnd && h-g <= 8 && secretHexDigit(b[h]) {
			h++
		}
		w := h - g
		if w == 0 || w%2 != 0 || w > 8 || h < textEnd && b[h] != ' ' || width != 0 && w > width {
			break
		}
		if width == 0 {
			width = w
		}
		for k := g; k < h; k += 2 {
			v, _ := secretHex(b, k, 2)
			data = append(data, byte(v))
		}
		p = h
		if w < width {
			break // a shorter group ends the line's columns
		}
	}
	if len(data) == 0 {
		return secretDumpLine{}, false
	}
	return secretDumpLine{start: offset, end: textEnd, data: data}, true
}

// secretHexDigit reports whether c is a hex digit.
func secretHexDigit(c byte) bool {
	_, ok := secretHex([]byte{c}, 0, 1)
	return ok
}

// secretLogPrefixEnd is p after a go test log prefix at b[p:end]
// ("name_test.go:12: "), or p.
func secretLogPrefixEnd(b []byte, p, end int) int {
	i := bytes.Index(b[p:min(end, p+128)], []byte(": "))
	if i < 0 {
		return p
	}
	name := b[p : p+i]
	colon := bytes.LastIndexByte(name, ':')
	if colon < 0 || !bytes.HasSuffix(name[:colon], []byte(".go")) || colon == len(name)-1 {
		return p
	}
	for _, c := range name[colon+1:] {
		if c < '0' || c > '9' {
			return p
		}
	}
	if bytes.ContainsAny(name, " \t") {
		return p
	}
	return p + i + 2
}

// secretJSONStringEnd is the index of the quote that ends the JSON string
// whose text starts at b[p:], before end, or -1.
func secretJSONStringEnd(b []byte, p, end int) int {
	for p < end {
		switch b[p] {
		case '\\':
			p += 2
		case '"':
			return p
		default:
			p++
		}
	}
	return -1
}
