package daemon

import (
	"bytes"
	"context"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/xleelz/fae/commandshim"
	"github.com/xleelz/fae/effects"
	"github.com/xleelz/fae/secretshape"
)

// secretValues are the secret forms redaction replaces in captured output
// with "[redacted]" (FAE-SEC-003): at each place, the longest that occurs
// there, so that a secret is never left partly replaced by a shorter one it
// contains.
//
// A form is found wherever its bytes appear, each byte as itself or escaped
// (secretUnitsAt): percent-encoded (%XX, or + for a space), as a JSON or Go
// string escape (\xNN, \NNN in octal, \uXXXX including surrogate pairs,
// \UXXXXXXXX, \n, \t, \", \\, \/ and the like), with hex digits in either
// case, written once or JSON-escaped once more (go test -json's view of a
// test's %q), or as an HTML
// character reference (&#NNN;, &#xHH;, &amp;, &lt;, &gt;, &quot;, &apos;),
// its & written as itself or as \u0026 (go test -json's view of a test's
// &). A form made only of hex digits, spaces, commas and x (hex, and Go's bytes),
// or of base32's letters and digits, is found with its letters in either
// case (secretFoldText). A form is found wrapped too: with a line break inside it, at up to
// secretBreakMax places with at least one byte of the form between two
// (secretBreakAt), each written raw or escaped once or twice, followed by up
// to secretBreakIndent spaces or tabs of indentation, and either inserted or
// standing for one of the form's spaces; a break escaped once may also end a
// go test -json output event, the form going on in the next one's. A go
// test -json output event may also end partway through the form with no
// break, as test2json cuts a line over 1 KiB, the form going on in the next
// event of the same test (secretshape.EventText), the cut falling between
// two of the form's units or inside an escape (secretCutUnitsAt); each such
// cut counts as a break. Every provider-shaped credential (secretshape.Spans) is replaced
// too, whatever its source, and a raw secret written as a hex dump
// (secretDumpSpans). Not found: a value split by other text (an ANSI code,
// or a log prefix on its next line), escaped twice by anything else, or
// written in any encoding not named here (credentialForms).
type secretValues []secretForm

// secretForm is one text a secret is found as.
type secretForm struct {
	text []byte
	// fold finds the text's letters in either case (secretFoldText).
	fold bool
	// lead and trail mark a base64 core (base64Cores): the base64 character
	// just before (lead) or after (trail) it encodes some bits of the secret
	// too, and is replaced with it.
	lead, trail bool
	// raw marks a secret's own bytes, which a hex dump is decoded and
	// searched for (secretDumpSpans).
	raw bool
	// pattern is an optional compiled regex from ContentPolicy.DenyPatterns (FAE-SEC-009).
	pattern *regexp.Regexp
}

// newSecretValues finds each value as itself (and escaped, and in a hex
// dump).
func newSecretValues(values ...[]byte) secretValues {
	forms := make([]secretForm, 0, len(values))
	for _, v := range values {
		forms = append(forms, secretForm{text: v, raw: true})
	}
	return newSecretForms(forms)
}

// newSecretForms keeps each distinct nonempty form once, longest first; a
// form whose letters fold has its other cases as the same form.
func newSecretForms(forms []secretForm) secretValues {
	var s secretValues
	var patterns []secretForm
	for _, f := range forms {
		if f.pattern != nil {
			patterns = append(patterns, f)
			continue
		}
		if len(f.text) == 0 {
			continue
		}
		f.fold = f.fold || secretFoldText(f.text, f.raw)
		if k := slices.IndexFunc(s, func(g secretForm) bool {
			return bytes.Equal(f.text, g.text) || f.fold && g.fold && bytes.EqualFold(f.text, g.text)
		}); k >= 0 {
			s[k].raw = s[k].raw || f.raw
			s[k].fold = s[k].fold || f.fold
			continue
		}
		f.text = bytes.Clone(f.text)
		s = append(s, f)
	}
	slices.SortStableFunc(s, func(a, b secretForm) int { return len(b.text) - len(a.text) })
	return append(s, patterns...)
}

// secretFoldText reports whether text's letters are found in either case:
// it is only hex digits, spaces, commas and x (a secret's hex, and Go's %#v
// and % #x of its bytes), or, when it is not a secret's own bytes, only
// base32's upper-case letters, digits 2 to 7 and padding. A secret of
// capitals, such as a placeholder like DISABLED, is not found as ordinary
// words in lower case.
func secretFoldText(text []byte, raw bool) bool {
	hexText, base32Text := true, true
	for _, c := range text {
		if _, ok := secretHex([]byte{c}, 0, 1); !ok && c != ' ' && c != ',' && c != 'x' {
			hexText = false
		}
		if !('A' <= c && c <= 'Z' || '2' <= c && c <= '7' || c == '=') {
			base32Text = false
		}
	}
	return hexText || base32Text && !raw
}

// secretFold is c's other case, for an ASCII letter; else c.
func secretFold(c byte) byte {
	switch {
	case 'a' <= c && c <= 'z':
		return c - 'a' + 'A'
	case 'A' <= c && c <= 'Z':
		return c - 'A' + 'a'
	}
	return c
}

// secretPrefix reports whether v begins with p, letters in either case when
// fold.
func secretPrefix(v, p []byte, fold bool) bool {
	if len(v) < len(p) {
		return false
	}
	if fold {
		return bytes.EqualFold(v[:len(p)], p)
	}
	return bytes.Equal(v[:len(p)], p)
}

// redactedMark replaces each secret found.
const redactedMark = secretshape.Mark

// redact returns b with every secret replaced; it copies b only when one
// occurs, since a run's output can be megabytes long.
func (s secretValues) redact(b []byte) []byte {
	return redactSpans(b, s.spans(b, false))
}

func (s secretValues) redactString(text string) string {
	b := []byte(text)
	if spans := s.spans(b, false); len(spans) > 0 {
		return string(redactSpans(b, spans))
	}
	return text
}

// redactCut is redactString for a field cut at a bound (a nested call's
// summary): its tail is replaced too when it is the start of a secret,
// redactedCutPrefix bytes of it or more, that the cut left incomplete.
func (s secretValues) redactCut(text string) string {
	b := []byte(text)
	if spans := s.spans(b, true); len(spans) > 0 {
		return string(redactSpans(b, spans))
	}
	return text
}

// redactSpans returns b with each of spans replaced by redactedMark.
func redactSpans(b []byte, spans [][2]int) []byte {
	if len(spans) == 0 {
		return b
	}
	out := make([]byte, 0, len(b))
	at := 0
	for _, sp := range spans {
		out = append(append(out, b[at:sp[0]]...), redactedMark...)
		at = sp[1]
	}
	return append(out, b[at:]...)
}

// redactRun returns run with every secret replaced in its stdout and stderr.
func (s secretValues) redactRun(run effects.RunEvidence) effects.RunEvidence {
	run.Output, run.Stderr = s.redactString(run.Output), s.redactString(run.Stderr)
	if len(run.Captured) > 0 {
		captured := map[string]string{}
		for key, text := range run.Captured {
			captured[key] = s.redactString(text)
		}
		run.Captured = captured
	}
	return run
}

// redactedCutPrefix is the shortest start of a secret redactCut replaces at
// a cut: a shorter one would garble ordinary text, and keeps too little of a
// secret to matter.
const redactedCutPrefix = 4

// secretEscapeWidth is the most text one byte of a secret takes in a form
// secretUnitsAt reads: an HTML character reference of up to secretEntityWidth
// bytes after its &, such as &#0115;, with the & written as \u0026, as go
// test -json writes a test's &.
const secretEscapeWidth = secretEntityWidth + len(`\u0026`)

// secretEntityWidth is the most text an HTML character reference takes per
// byte of its character after its &: `#0115;` for one byte, or `\\u00XX`, a
// \u escape JSON-escaped once more.
const secretEntityWidth = 7

// secretCutUnitMax is the most text one unit of a form secretUnitsAt reads
// spans, which bounds the text a go test -json cut inside a unit leaves on
// each side of it (secretCutUnitsAt): a surrogate pair JSON-escaped once more
// (`\\ud83d\\ude00`), or a reference whose & is `\u0026`.
const secretCutUnitMax = 14

// secretBreakIndent is the most indentation the next line of a wrapped form
// starts with: go test indents a log message's later lines by 8 spaces, a
// subtest's further.
const secretBreakIndent = 16

// secretEventEnd and secretEventGap bound the go test -json text between two
// output events a form is wrapped across: what ends the first event's line
// after its output, and the next event's fields before its output
// (secretshape.EventText).
const (
	secretEventEnd = secretshape.EventEnd
	secretEventGap = secretshape.EventGap
)

// secretBreakWidth is the most text one line break inside a form spans
// (secretBreakAt): a newline escaped twice (`\\r\\n`), an event gap, and
// indentation with each tab escaped twice.
const secretBreakWidth = 6 + secretEventEnd + secretEventGap + 3*secretBreakIndent

// secretBreakMax is how many line breaks one occurrence of a form may hold.
const secretBreakMax = 32

// hold is the most text one occurrence of a secret can span, a base64
// core's trail character included, which a stream holds back between reads
// (copyRedacted): its form escaped and wrapped as far as the matcher reads
// it, or the lines of a hex dump of it. A core not yet whole when a read ends
// starts after the first byte held back, so its lead character is held back
// too.
func (s secretValues) hold() int {
	hold := secretshape.Max
	if len(s) > 0 {
		n := len(s[0].text) // s is longest first
		hold = max(hold, n*secretEscapeWidth+secretBreakMax*secretBreakWidth+1)
	}
	for _, f := range s {
		if f.raw {
			hold = max(hold, secretDumpSpanMax(len(f.text)))
			break // the longest raw form
		}
	}
	return hold
}

// copyRedacted copies src to dst with every secret replaced. Between reads
// it holds back the text an occurrence starting there could still need, so a
// secret split across reads is replaced whole.
func (s secretValues) copyRedacted(dst io.Writer, src io.Reader) error {
	const chunk = 256 << 10
	hold := s.hold()
	buf := make([]byte, 0, chunk+hold)
	for {
		buf = slices.Grow(buf, chunk)
		n, err := src.Read(buf[len(buf) : len(buf)+chunk])
		buf = buf[:len(buf)+n]
		final := errors.Is(err, io.EOF)
		if err != nil && !final {
			return err
		}
		if !final && len(buf) < hold+chunk {
			continue
		}
		spans := s.spans(buf, false)
		emit := len(buf)
		if !final {
			// An occurrence that ends at or before emit is whole and final:
			// all the text one starting before it could need is in buf.
			emit -= hold
			for _, sp := range spans {
				if sp[0] < emit && sp[1] > emit {
					emit = sp[0]
				}
			}
		}
		kept := 0
		for kept < len(spans) && spans[kept][1] <= emit {
			kept++
		}
		if _, err = dst.Write(redactSpans(buf[:emit], spans[:kept])); err != nil {
			return err
		}
		if final {
			return nil
		}
		buf = buf[:copy(buf, buf[emit:])]
	}
}

// spans are the occurrences in b of s's secrets, of provider-shaped
// credentials and of hex dumps of raw secrets, in order and apart; when cut,
// a trailing start of a secret too (redactCut).
func (s secretValues) spans(b []byte, cut bool) [][2]int {
	if len(s) == 0 {
		return secretshape.Spans(b)
	}
	var byFirst [256][]int // indexes into s, longest first
	var patternSpans [][2]int
	for i, f := range s {
		if f.pattern != nil {
			for _, m := range f.pattern.FindAllIndex(b, -1) {
				if m[1] > m[0] {
					patternSpans = append(patternSpans, [2]int{m[0], m[1]})
				}
			}
			continue
		}
		c := f.text[0]
		byFirst[c] = append(byFirst[c], i)
		if o := secretFold(c); f.fold && o != c {
			byFirst[o] = append(byFirst[o], i)
		}
	}
	var spans [][2]int
	var units [4]secretUnit
	var cuts [2]secretCutUnit
	for j := 0; j < len(b); {
		c, k, kc := b[j], 1, 0
		units[0] = secretUnit{b: [utf8.UTFMax]byte{c}, size: 1, n: 1}
		if c == '%' || c == '+' || c == '\\' || c == '&' {
			k = secretUnitsAt(b, j, &units)
			kc = secretCutUnitsAt(b, j, &cuts)
		}
		best, end := -1, -1
		for u := range units[:k] {
			for _, i := range byFirst[units[u].b[0]] {
				if best >= 0 && len(s[i].text) <= len(s[best].text) {
					break
				}
				budget := 16 + 8*len(s[i].text)
				if e := secretEnd(b, j, s[i], 0, cut, secretBreakMax, false, &budget); e > j {
					best, end = i, e
					break
				}
			}
		}
		// The form's first unit is cut by a go test -json event's end: the
		// occurrence starts here and goes on, past the unit, in the next event.
		for _, u := range cuts[:kc] {
			for _, i := range byFirst[u.b[0]] {
				if best >= 0 && len(s[i].text) <= len(s[best].text) {
					break
				}
				if !secretPrefix(s[i].text, u.b[:u.size], s[i].fold) {
					continue
				}
				budget := 16 + 8*len(s[i].text)
				if e := secretEnd(b, u.resume, s[i], u.size, cut, secretBreakMax-1, false, &budget); e > j {
					best, end = i, e
					break
				}
			}
		}
		if best < 0 {
			j++
			continue
		}
		start := j
		if s[best].lead && start > 0 && base64Byte(b[start-1]) && (len(spans) == 0 || spans[len(spans)-1][1] < start) {
			start--
		}
		if s[best].trail && end < len(b) && base64Byte(b[end]) {
			end++
		}
		spans = append(spans, [2]int{start, end})
		j = end
	}
	spans = secretshape.Merge(spans, patternSpans)
	spans = secretshape.Merge(spans, secretshape.Spans(b))
	return secretshape.Merge(spans, secretDumpSpans(b, s))
}

// base64Byte reports whether c is in either base64 alphabet.
func base64Byte(c byte) bool {
	return secretshape.RunByte(c) || c == '+' || c == '/'
}

// secretSpecial reports whether c can start text that stands for a secret's
// bytes other than itself (an escape, or a + for a space) or a line break.
func secretSpecial(c byte) bool {
	return c == '%' || c == '+' || c == '\\' || c == '&' || c == '\n' || c == '\r'
}

// secretEnd is the end of an occurrence of f.text[i:] at b[j:], or -1, with
// at most breaks line breaks inside it, none at b[j] when broke (b[j] follows
// one). When cut, b ending partway through the text, after redactedCutPrefix
// bytes or more of it (or partway through an escape after them), is an
// occurrence. budget bounds the alternatives tried.
func secretEnd(b []byte, j int, f secretForm, i int, cut bool, breaks int, broke bool, budget *int) int {
	v := f.text
	for i < len(v) {
		if j >= len(b) {
			if cut && i >= redactedCutPrefix {
				return j
			}
			return -1
		}
		if c := b[j]; !secretSpecial(c) {
			if c == '"' && i > 0 && breaks > 0 {
				// An output event of go test -json ends partway through the
				// form with no line break, and the next goes on with it:
				// test2json cuts a line over 1 KiB into several events. Try
				// the event join before matching '"' as a byte of v so a
				// cut right before a '"' in v is not mistaken for the
				// event's closing quote matching v[i].
				if *budget--; *budget < 0 {
					return -1
				}
				if q := secretEventText(b, j); q >= 0 {
					if end := secretEnd(b, q, f, i, cut, breaks-1, false, budget); end >= 0 {
						return end
					}
				}
			}
			if c == v[i] || f.fold && secretFold(c) == v[i] {
				i, j, broke = i+1, j+1, false
				continue
			}
			return -1
		}
		if *budget--; *budget < 0 {
			return -1
		}
		var units [4]secretUnit
		// An escape is tried before the byte itself: where both end the
		// secret (a trailing % before 25), the occurrence takes the escape.
		for k := secretUnitsAt(b, j, &units) - 1; k >= 0; k-- {
			u := units[k]
			if !secretPrefix(v[i:], u.b[:u.size], f.fold) {
				continue
			}
			if end := secretEnd(b, j+u.n, f, i+u.size, cut, breaks, false, budget); end >= 0 {
				return end
			}
		}
		if breaks > 0 {
			// A go test -json output event ends inside the escape here, and
			// the next event of the same test goes on with the rest of it:
			// the unit is read across the cut.
			var cuts [2]secretCutUnit
			for k := secretCutUnitsAt(b, j, &cuts) - 1; k >= 0; k-- {
				u := cuts[k]
				if !secretPrefix(v[i:], u.b[:u.size], f.fold) {
					continue
				}
				if *budget--; *budget < 0 {
					return -1
				}
				if end := secretEnd(b, u.resume, f, i+u.size, cut, breaks-1, false, budget); end >= 0 {
					return end
				}
			}
		}
		if i > 0 && breaks > 0 && !broke {
			// A line break wraps the form here: it is inserted, or it stands
			// for one of the form's spaces.
			var ends [secretBreakIndent + 1]int
			for _, e := range ends[:secretBreakAt(b, j, &ends)] {
				if *budget--; *budget < 0 {
					return -1
				}
				if end := secretEnd(b, e, f, i, cut, breaks-1, true, budget); end >= 0 {
					return end
				}
				if v[i] == ' ' {
					if end := secretEnd(b, e, f, i+1, cut, breaks-1, true, budget); end >= 0 {
						return end
					}
				}
			}
		}
		if c := b[j]; cut && i >= redactedCutPrefix && len(b)-j < secretEscapeWidth && (c == '%' || c == '\\' || c == '&') {
			return len(b) // the cut fell inside an escape
		}
		return -1
	}
	return j
}

// secretBreakAt stores in ends where the text after a line break at b[j:]
// may start, longest indentation first, and returns how many (0 for none). A
// line break is a newline ("\n" or "\r\n") written raw, escaped once (`\n`,
// as JSON and Go quote it) or escaped twice (`\\n`); after one escaped once
// that ends a go test -json output event, the break goes on to the next
// output event's text (secretshape.EventText). The indentation is spaces, and
// tabs written as the newline is, up to secretBreakIndent of them.
func secretBreakAt(b []byte, j int, ends *[secretBreakIndent + 1]int) int {
	p, level := secretNewlineAt(b, j)
	if p < 0 {
		return 0
	}
	if level == 1 {
		if q := secretEventText(b, p); q >= 0 {
			p = q
		}
	}
	tab := []byte([...]string{"\t", `\t`, `\\t`}[level])
	ends[0] = p
	n := 1
	for ; n <= secretBreakIndent; n++ {
		switch {
		case p < len(b) && b[p] == ' ':
			p++
		case bytes.HasPrefix(b[p:], tab):
			p += len(tab)
		default:
			slices.Reverse(ends[:n])
			return n
		}
		ends[n] = p
	}
	slices.Reverse(ends[:n])
	return n
}

// secretNewlineAt is the end of a newline at b[j:] and how many times it is
// escaped (0, 1 or 2), or -1.
func secretNewlineAt(b []byte, j int) (int, int) {
	for level, newlines := range [...][]string{{"\n", "\r\n"}, {`\n`, `\r\n`}, {`\\n`, `\\r\\n`}} {
		for _, nl := range newlines {
			if bytes.HasPrefix(b[j:], []byte(nl)) {
				return j + len(nl), level
			}
		}
	}
	return -1, 0
}

// secretUnit is one way text stands for the bytes b[:size] of a secret, in
// n bytes of text.
type secretUnit struct {
	b       [utf8.UTFMax]byte
	size, n int
}

// secretUnitsAt stores in units the ways b[j:] begins and returns how many:
// the byte itself; a + for a space; %XX; a backslash escape, written once
// (\xNN) or JSON-escaped once more (\\xNN); and an HTML character reference,
// its & written as itself or as \u0026.
func secretUnitsAt(b []byte, j int, units *[4]secretUnit) int {
	c := b[j]
	units[0] = secretUnit{b: [utf8.UTFMax]byte{c}, size: 1, n: 1}
	k := 1
	switch c {
	case '+':
		units[k] = secretUnit{b: [utf8.UTFMax]byte{' '}, size: 1, n: 1}
		k++
	case '%':
		if v, ok := secretHex(b, j+1, 2); ok {
			units[k] = secretUnit{b: [utf8.UTFMax]byte{byte(v)}, size: 1, n: 3}
			k++
		}
	case '\\':
		if u, ok := secretEscapeAt(b, j+1, 1); ok {
			units[k] = u
			k++
		}
		if j+1 < len(b) && b[j+1] == '\\' {
			if u, ok := secretEscapeAt(b, j+2, 2); ok {
				units[k] = u
				k++
			}
		}
		if u, ok := secretEntityAt(b, j); ok {
			units[k] = u
			k++
		}
	case '&':
		if u, ok := secretEntityAt(b, j); ok {
			units[k] = u
			k++
		}
	}
	return k
}

// secretCutUnit is a unit of a form that a go test -json event cut ends
// partway through: the unit, read across the cut, and where the form goes on
// in the next event's text, after the rest of the unit.
type secretCutUnit struct {
	secretUnit
	resume int
}

// secretEventText is secretshape.EventText(b, p) when b[p] is the closing
// quote of the first output event's "Output" field (followed by "}" or by
// test2json's OutputType field), and not an escaped quote earlier on the line.
func secretEventText(b []byte, p int) int {
	if p+1 >= len(b) || b[p] != '"' || b[p+1] != '}' && !bytes.HasPrefix(b[p:], []byte(`","OutputType":"`)) {
		return -1
	}
	return secretshape.EventText(b, p)
}

// secretCutUnitsAt stores in cuts the escapes b[j:] begins that a go test
// -json output event ends inside, going on in the next output event of the
// same test (secretshape.EventText), and returns how many (0 for none). The
// text before the cut and the text after it, up to secretCutUnitMax bytes
// each, are read together as one unit; only a unit longer than the text
// before the cut is one the cut fell inside.
func secretCutUnitsAt(b []byte, j int, cuts *[2]secretCutUnit) int {
	if c := b[j]; c != '%' && c != '\\' && c != '&' {
		return 0
	}
	before := b[j:min(len(b), j+secretCutUnitMax)]
	q := bytes.IndexByte(before, '"')
	if q < 1 {
		return 0
	}
	next := secretEventText(b, j+q)
	if next < 0 {
		return 0
	}
	var joined [2 * secretCutUnitMax]byte
	n := copy(joined[:], before[:q])
	n += copy(joined[n:], b[next:min(len(b), next+secretCutUnitMax)])
	var units [4]secretUnit
	k := 0
	for _, u := range units[1:secretUnitsAt(joined[:n], 0, &units)] {
		if u.n > q && k < len(cuts) {
			cuts[k] = secretCutUnit{secretUnit: u, resume: next + u.n - q}
			k++
		}
	}
	return k
}

// secretEntities are the named HTML character references secretEntityAt
// reads: those html.EscapeString and html/template write.
var secretEntities = []struct {
	name string
	c    byte
}{{"&amp;", '&'}, {"&lt;", '<'}, {"&gt;", '>'}, {"&quot;", '"'}, {"&apos;", '\''}}

// secretAmpersandAt is the length of the & at b[j:], written as itself or as
// the JSON escape \u0026 (hex in either case), which go test -json writes a
// test's & as; 0 for neither.
func secretAmpersandAt(b []byte, j int) int {
	switch {
	case b[j] == '&':
		return 1
	case b[j] == '\\' && j+6 <= len(b) && b[j+1] == 'u':
		if v, ok := secretHex(b, j+2, 4); ok && v == '&' {
			return 6
		}
	}
	return 0
}

// secretEntityAt decodes the HTML character reference at b[j:], whose &
// (secretAmpersandAt) b[j] starts: a named one (secretEntities), or &#NNN;
// or &#xHH; (x in either case) for one character, in at most
// secretEntityWidth bytes after the & per byte of the character's UTF-8.
func secretEntityAt(b []byte, j int) (secretUnit, bool) {
	amp := secretAmpersandAt(b, j)
	if amp == 0 {
		return secretUnit{}, false
	}
	rest := b[j+amp-1:]
	for _, e := range secretEntities {
		if bytes.HasPrefix(rest[1:], []byte(e.name[1:])) {
			return secretUnit{b: [utf8.UTFMax]byte{e.c}, size: 1, n: amp - 1 + len(e.name)}, true
		}
	}
	if len(rest) < 4 || rest[1] != '#' {
		return secretUnit{}, false
	}
	p, base := 2, uint32(10)
	if rest[2] == 'x' || rest[2] == 'X' {
		p, base = 3, 16
	}
	var v uint32
	digits := p
	for ; p < len(rest) && p-digits < 8; p++ {
		d, ok := secretHex(rest, p, 1)
		if !ok || d >= base {
			break
		}
		v = v*base + d
	}
	if p == digits || p >= len(rest) || rest[p] != ';' || v > utf8.MaxRune || !utf8.ValidRune(rune(v)) {
		return secretUnit{}, false
	}
	u := secretUnit{n: amp + p}
	u.size = utf8.EncodeRune(u.b[:], rune(v))
	if p > secretEntityWidth*u.size {
		return secretUnit{}, false
	}
	return u, true
}

// secretEscapeAt decodes the escape whose body starts at b[p:], after a
// backslash written in lead bytes (`\`, or `\\` when JSON-escaped again);
// a 3-digit octal byte escape, \000 to \377, is one byte as \xNN is.
func secretEscapeAt(b []byte, p, lead int) (secretUnit, bool) {
	if p >= len(b) {
		return secretUnit{}, false
	}
	one := func(c byte, n int) (secretUnit, bool) {
		return secretUnit{b: [utf8.UTFMax]byte{c}, size: 1, n: n}, true
	}
	switch c := b[p]; c {
	case 'x':
		if v, ok := secretHex(b, p+1, 2); ok {
			return one(byte(v), lead+3)
		}
	case '0', '1', '2', '3':
		if p+3 > len(b) || b[p+1] < '0' || b[p+1] > '7' || b[p+2] < '0' || b[p+2] > '7' {
			break
		}
		return one((c-'0')<<6|(b[p+1]-'0')<<3|(b[p+2]-'0'), lead+3)
	case 'u', 'U':
		digits := 4
		if c == 'U' {
			digits = 8
		}
		v, ok := secretHex(b, p+1, digits)
		if !ok || v > utf8.MaxRune {
			break
		}
		r, n := rune(v), lead+1+digits
		if c == 'u' && utf16.IsSurrogate(r) {
			// JSON writes a character beyond the BMP as two \u escapes.
			q := p + 1 + digits
			if q+lead+5 > len(b) || b[q] != '\\' || lead == 2 && b[q+1] != '\\' || b[q+lead] != 'u' {
				break
			}
			low, ok := secretHex(b, q+lead+1, 4)
			if !ok {
				break
			}
			r, n = utf16.DecodeRune(r, rune(low)), n+lead+5
		}
		if !utf8.ValidRune(r) {
			break
		}
		u := secretUnit{n: n}
		u.size = utf8.EncodeRune(u.b[:], r)
		return u, true
	case 'n':
		return one('\n', lead+1)
	case 't':
		return one('\t', lead+1)
	case 'r':
		return one('\r', lead+1)
	case 'b':
		return one('\b', lead+1)
	case 'f':
		return one('\f', lead+1)
	case 'v':
		return one('\v', lead+1)
	case 'a':
		return one('\a', lead+1)
	case '/', '\'':
		return one(c, lead+1)
	case '"':
		if lead == 1 {
			return one(c, 2)
		}
	case '\\':
		if lead == 1 {
			return one(c, 2)
		}
		// JSON-escaped again, \" and \\ are \\\" and \\\\.
		if p+1 < len(b) && (b[p+1] == '"' || b[p+1] == '\\') {
			return one(b[p+1], 4)
		}
	}
	return secretUnit{}, false
}

// secretHex reads n hex digits, in either case, at b[p:].
func secretHex(b []byte, p, n int) (uint32, bool) {
	if p+n > len(b) {
		return 0, false
	}
	var v uint32
	for _, c := range b[p : p+n] {
		switch {
		case '0' <= c && c <= '9':
			c -= '0'
		case 'a' <= c && c <= 'f':
			c -= 'a' - 10
		case 'A' <= c && c <= 'F':
			c -= 'A' - 10
		default:
			return 0, false
		}
		v = v<<4 | uint32(c)
	}
	return v, true
}

// A provider login the daemon is not given as a value (a CLAUDE_CONFIG_DIR's,
// a CODEX_HOME's) is never read to be named: its credentials are found by
// their shape, and an OAuth token by the field that holds it, whatever their
// source (secretshape.Spans).

// credentialForms are the forms a secret's bytes take in captured output:
//   - the bytes themselves;
//   - their hex (the Dolt password is the plane credential's hex), plain or
//     with a space between bytes (Go's "% x"), found in either case;
//   - Go's other views of a []byte: %v and %d ("[115 101 …]"), %#v
//     ("[]byte{0x73, 0x65, …}", a byte under 0x10 as one digit) and % #x
//     ("0x73 0x65 …"), the last two in either case, %o ("[163 145 …]") and
//     %b ("[1110011 1100101 …]");
//   - Go's views of the secret as text, a []rune: %q ("['s' 'e' …]"), %U
//     ("[U+0073 U+0065 …]") and %v, each byte that is not UTF-8 as U+FFFD,
//     as Go converts it;
//   - their base64, standard or URL-safe, padded or not;
//   - their base32, standard or extended-hex alphabet (base32hex), padded or
//     not, in either case;
//   - the secret as text, as Go converts it, in UTF-16, little- or
//     big-endian.
//
// Each form is also found escaped and wrapped (secretValues). credentialForms
// returns every form but base32hex, which credentialSecrets adds
// (base32HexForms) so that it is found in either case.
func credentialForms(raw []byte) [][]byte {
	if len(raw) == 0 {
		return nil
	}
	lower := hex.EncodeToString(raw)
	var spaced, decimal, goBytes, prefixed []byte
	for i, c := range raw {
		if i > 0 {
			spaced = append(spaced, ' ')
			decimal = append(decimal, ' ')
			goBytes = append(goBytes, ", "...)
			prefixed = append(prefixed, ' ')
		}
		spaced = append(spaced, lower[2*i:2*i+2]...)
		decimal = strconv.AppendUint(decimal, uint64(c), 10)
		goBytes = strconv.AppendUint(append(goBytes, "0x"...), uint64(c), 16)
		prefixed = append(append(prefixed, "0x"...), lower[2*i:2*i+2]...)
	}
	forms := [][]byte{raw, []byte(lower), []byte(strings.ToUpper(lower)), spaced, decimal, goBytes, prefixed}
	// fmt writes a slice's elements in brackets, which are left out.
	runes := []rune(string(raw))
	for _, view := range []string{fmt.Sprintf("%o", raw), fmt.Sprintf("%b", raw), fmt.Sprintf("%q", runes), fmt.Sprintf("%U", runes), fmt.Sprintf("%v", runes)} {
		forms = append(forms, []byte(view[1:len(view)-1]))
	}
	for _, e := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		forms = append(forms, []byte(e.EncodeToString(raw)))
	}
	for _, e := range []*base32.Encoding{base32.StdEncoding, base32.StdEncoding.WithPadding(base32.NoPadding)} {
		forms = append(forms, []byte(e.EncodeToString(raw)))
	}
	var little, big []byte
	for _, u := range utf16.Encode(runes) {
		little = append(little, byte(u), byte(u>>8))
		big = append(big, byte(u>>8), byte(u))
	}
	return append(forms, little, big)
}

// base32HexForms are raw's base32 in the extended-hex alphabet, padded or
// not, found in either case: unlike the standard alphabet's, its text has
// digits that keep secretFoldText from folding it.
func base32HexForms(raw []byte) []secretForm {
	var forms []secretForm
	for _, e := range []*base32.Encoding{base32.HexEncoding, base32.HexEncoding.WithPadding(base32.NoPadding)} {
		forms = append(forms, secretForm{text: []byte(e.EncodeToString(raw)), fold: true})
	}
	return forms
}

// base64Cores find raw's base64 inside a larger encoded value, such as
// `Authorization: Basic` and base64("user:" + key). Base64 writes 3 bytes as
// 4 characters, so the secret's characters depend on where it starts within
// a group: for each of the three alignments, in each alphabet, the core is
// the characters that encode the secret's bits alone. Where the alignment
// leaves a character before or after the core encoding some of the secret's
// bits and some of its neighbour's, the core is marked lead or trail, and
// that character is replaced with it. A core shorter than
// minimumConfiguredSecret is not kept.
func base64Cores(raw []byte) []secretForm {
	var cores []secretForm
	for align := 0; align < 3; align++ {
		bits := 8 * (align + len(raw))
		first, last := (8*align+5)/6, bits/6 // the characters [first, last) are the secret's alone
		if last-first < minimumConfiguredSecret {
			continue
		}
		padded := append(make([]byte, align), raw...)
		for _, e := range []*base64.Encoding{base64.RawStdEncoding, base64.RawURLEncoding} {
			text := e.EncodeToString(padded)
			cores = append(cores, secretForm{text: []byte(text[first:last]), lead: 8*align%6 != 0, trail: bits%6 != 0})
		}
	}
	return cores
}

// credentialSecrets finds each secret in every form credentialForms names,
// as base64 at any offset in a larger value (base64Cores), and in a hex dump
// of its bytes (secretDumpSpans).
func credentialSecrets(secrets ...[]byte) secretValues {
	var forms []secretForm
	for _, raw := range secrets {
		for i, text := range credentialForms(raw) {
			forms = append(forms, secretForm{text: text, raw: i == 0})
		}
		forms = append(forms, base32HexForms(raw)...)
		forms = append(forms, base64Cores(raw)...)
	}
	return newSecretForms(forms)
}

// credentialVariable reports whether a configured environment variable
// carries a credential by its name, as workflow environments are refused
// such names: a provider key or token, a password or a secret. The names in
// secretshape.NotCredentialVariables (a commit's author, the SSH agent's
// socket and the like) do not.
func credentialVariable(name string) bool { return secretshape.CredentialVariable(name) }

// minimumConfiguredSecret is the shortest configured value redacted: a
// shorter one would cut ordinary words out of the output, and no real
// credential is that short.
const minimumConfiguredSecret = secretshape.MinimumValue

// readSecretFile reads a small owner file without following a link and
// without creating it (api.Credential creates a missing credential).
func readSecretFile(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, 1<<20))
}

// failureSecrets are the secret values the daemon holds that captured
// output could carry, each in every form credentialSecrets names
// (FAE-SEC-003):
//   - the operator credential (the instance socket's);
//   - the plane credential, whose hex is the Dolt password;
//   - each delegated principal's credential, from PrincipalConfig;
//   - the UI token and the UI's read and write credentials;
//   - the values of environment's credential-named variables (the live
//     configuration's, which reaches the bridge: a provider key or token);
//   - the values of the daemon's own credential-named environment variables
//     (a provider key exported in the shell that started it), which no run
//     is given but any run can read through /proc.
//
// They are read afresh each time, from the files the daemon read at start
// and from memory. A provider login the daemon is not given as a value, such
// as the files under a CLAUDE_CONFIG_DIR or a CODEX_HOME, is never read to be
// named: its tokens are redacted by their shape (shapedCredentialSpans), and
// nothing else in it is redacted.
func (d *Daemon) failureSecrets(environment map[string]string) (secretValues, error) {
	var values [][]byte
	files := []string{d.layout.Credential, filepath.Join(d.layout.Run, "plane-credential")}
	if d.config.PrincipalConfig != "" {
		raw, err := readSecretFile(d.config.PrincipalConfig)
		if err != nil {
			return nil, fmt.Errorf("principal configuration: %w", err)
		}
		var principals []PrincipalCredential
		if err = json.Unmarshal(raw, &principals); err != nil {
			return nil, fmt.Errorf("principal configuration: %w", err)
		}
		for _, p := range principals {
			files = append(files, p.CredentialFile)
		}
	}
	for _, path := range files {
		raw, err := readSecretFile(path)
		if err != nil {
			return nil, err
		}
		if len(raw) == 0 {
			return nil, errors.New("empty credential file " + path)
		}
		values = append(values, raw)
	}
	if u := d.ui; u != nil {
		values = append(values, []byte(u.token), u.read, u.write)
	}
	for name, value := range environment {
		if credentialVariable(name) && len(value) >= minimumConfiguredSecret {
			values = append(values, []byte(value))
		}
	}
	for _, entry := range os.Environ() {
		if name, value, ok := strings.Cut(entry, "="); ok && credentialVariable(name) && len(value) >= minimumConfiguredSecret {
			values = append(values, []byte(value))
		}
	}
	base := credentialSecrets(values...)
	if extra := d.targetContentPolicyForms(); len(extra) > 0 {
		base = newSecretForms(append(slices.Clone(base), extra...))
	}
	return base, nil
}

func (d *Daemon) targetContentPolicyForms() []secretForm {
	var forms []secretForm
	for _, cp := range d.contentPolicies {
		forms = append(forms, contentPolicySecretForms(cp)...)
	}
	if d.store == nil {
		return forms
	}
	targets, err := d.store.ListTargets(context.Background())
	if err != nil {
		return forms
	}
	for _, t := range targets {
		_, raw, err := d.store.GetTarget(context.Background(), t.ID, t.Revision)
		if err != nil {
			continue
		}
		var cfg TargetConfig
		if json.Unmarshal(raw, &cfg) == nil {
			forms = append(forms, contentPolicySecretForms(cfg.ContentPolicy)...)
		}
	}
	return forms
}

// liveSecretEnvironment is the live configuration's environment, whose
// credential-named values are secrets; nil without a live executor.
func (d *Daemon) liveSecretEnvironment() map[string]string {
	if d.live == nil {
		return nil
	}
	return d.live.config.Environment
}

// withheldExcerpt replaces a failure excerpt that could not be redacted.
func withheldExcerpt(err error) string {
	return "[failure excerpt withheld: the secrets it must be redacted of could not be read: " + err.Error() + "]\n"
}

// redactedNestedCalls are an account-mode run's nested calls with every
// secret the daemon holds replaced in each argument and directory
// (FAE-SEC-003), including the tail of a field the shim cut (redactCut).
// When the secrets cannot be read, each call keeps only its time and whether
// it was cut, and the reason is returned.
func (d *Daemon) redactedNestedCalls(nested []commandshim.NestedCall) ([]commandshim.NestedCall, string) {
	calls := make([]commandshim.NestedCall, 0, len(nested))
	if len(nested) == 0 {
		return calls, ""
	}
	secrets, err := d.failureSecrets(d.liveSecretEnvironment())
	if err != nil {
		for _, c := range nested {
			calls = append(calls, commandshim.NestedCall{At: c.At, Truncated: c.Truncated})
		}
		return calls, "nested call arguments and directories withheld: the secrets they must be redacted of could not be read: " + err.Error()
	}
	for _, c := range nested {
		redact := secrets.redactString
		if c.Truncated {
			redact = secrets.redactCut
		}
		argv := make([]string, len(c.Argv))
		for i, a := range c.Argv {
			argv[i] = redact(a)
		}
		calls = append(calls, commandshim.NestedCall{Argv: argv, Dir: redact(c.Dir), At: c.At, Truncated: c.Truncated})
	}
	return calls, ""
}

// redactLandingOutput is a landing's RedactOutput (controller.LandRequest):
// it copies a gate command's output into the landing scratch with every
// secret the daemon holds replaced, so neither the kept file nor the gate
// results parsed from it carry one. When the secrets cannot be read it copies
// nothing, and the landing stops as infrastructure with the reason.
func (d *Daemon) redactLandingOutput(dst io.Writer, src io.Reader) error {
	secrets, err := d.failureSecrets(d.liveSecretEnvironment())
	if err != nil {
		return errors.New("gate output withheld: the secrets it must be redacted of could not be read: " + err.Error())
	}
	return secrets.copyRedacted(dst, src)
}
