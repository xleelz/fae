// Package secretshape finds credentials in captured output where no value is
// known to look for (FAE-SEC-003): a provider credential by its shape, and an
// OAuth token by the field that holds it. The daemon's redaction
// (daemon/secret_redaction.go) replaces what it finds in every output it
// keeps, and the provider bridges replace it in their raw logs (Writer).
package secretshape

import (
	"bytes"
	"slices"
	"strings"
)

// Mark replaces each credential found.
const Mark = "[redacted]"

// What is found, each replaced up to Max bytes of the text in all:
//   - an Anthropic credential (an API key, or a token Claude Code holds):
//     "sk-ant-" and a run of anthropicRun or more letters, digits, '-' and
//     '_'. A run a line break cuts short of that goes on at the next line,
//     after its indentation, over up to 4 breaks: "sk-ant-" starts no
//     ordinary word;
//   - an OpenAI API key: "sk-" and a run of openAIRun or more of those bytes
//     that holds openAIMarker, which OpenAI's keys carry ("T3BlbkFJ" is
//     "OpenAI" in base64); or, without the marker, "sk-" where no such byte
//     precedes it and a run of openAIUnmarkedRun or more holding an upper-case
//     letter, a lower-case letter and a digit, as a key's random part does. A
//     bare "sk-" also ends ordinary words ("task-1", "disk-") and starts a few
//     ("sk-learn"), which neither rule takes;
//   - a JSON Web Token, as a ChatGPT login Codex holds keeps its ID and access
//     tokens: "eyJ" (`{"` in base64url) and webTokenPart or more base64url
//     characters, '.', the same again, '.', and the signature's run. A line
//     break inside the header or the payload, or before a dot, is taken with
//     them;
//   - the value of an OAuth token field (tokenFields: refresh_token,
//     access_token and id_token, in snake, camel or kebab case, in any case),
//     such as the refresh token of a Codex ChatGPT login, whose format is not
//     published: tokenFieldValue or more of the bytes a token holds
//     (tokenValueByte), after the field's name, a ':' or '=' and quotes
//     written plain or escaped once or twice (JSON, and go test -json's view
//     of it), with up to fieldSpaces spaces each side of the ':' or '=';
//   - the credential after an HTTP "Bearer" scheme (bearerSpans).
//
// A line break is a newline written raw or escaped once or twice
// (lineBreakEnd). In go test -json output, a test's consecutive output
// events read as one text (EventText), so a token test2json cut into two
// events, or a line break split, is found as in the test's own output.
//
// The shapes' bytes are left alone by percent-encoding and by JSON and Go
// quoting, so each shape is found only as itself, never as hex or base64. A
// shaped token wrapped across lines keeps the part after the break when the
// part before it already has the shape (a break inside a JSON Web Token's
// signature): taking the next line's leading run as well would also take the
// ordinary text that follows a token on its own line, such as the next
// variable's name, and the part kept cannot be used without the part
// replaced. A go test -json cut is not a line break: the text on both sides
// is one line, and the token is found whole.
const (
	anthropicPrefix   = "sk-ant-"
	anthropicRun      = 16
	openAIPrefix      = "sk-"
	openAIMarker      = "T3BlbkFJ"
	openAIRun         = 32
	openAIUnmarkedRun = 40
	webTokenPrefix    = "eyJ"
	webTokenPart      = 10
	tokenFieldValue   = 16
	// breakIndent is the most indentation a wrapped token's next line
	// starts with.
	breakIndent = 16
	// Max is the most text one credential found spans.
	Max = 8 << 10
)

// tokenFields name the OAuth token fields whose values are credentials,
// compared in any case.
var tokenFields = []string{"refresh_token", "refreshtoken", "refresh-token", "access_token", "accesstoken", "access-token", "id_token", "idtoken", "id-token"}

// Spans are b's credentials, in order and apart. In go test -json output,
// a test's consecutive output events are read as one text (joinedEvents), so
// a credential that test2json cut into two events, or a line break split, is
// found whole; its span then takes the JSON between the events with it, and
// the events become one, still JSON.
func Spans(b []byte) [][2]int {
	v := joinedEvents(b)
	spans := anthropicSpans(v.b, v.limit)
	spans = Merge(spans, openAISpans(v.b, v.limit))
	spans = Merge(spans, webTokenSpans(v.b, v.limit))
	spans = Merge(spans, bearerSpans(v.b, v.limit))
	return v.unjoin(Merge(spans, tokenFieldSpans(v.b, v.limit)))
}

// Merge is the union of two ordered lists of spans.
func Merge(a, b [][2]int) [][2]int {
	if len(b) == 0 {
		return a
	}
	all := append(slices.Clip(a), b...)
	slices.SortStableFunc(all, func(x, y [2]int) int { return x[0] - y[0] })
	merged := all[:1]
	for _, sp := range all[1:] {
		if last := &merged[len(merged)-1]; sp[0] < last[1] {
			last[1] = max(last[1], sp[1])
			continue
		}
		merged = append(merged, sp)
	}
	return merged
}

// RunByte is a byte of a shaped credential's run: a letter, a digit, '-' or
// '_' (base64url's alphabet).
func RunByte(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '-' || c == '_'
}

// run is the end of the run of RunBytes at b[p:limit].
func run(b []byte, p, limit int) int {
	for p < limit && RunByte(b[p]) {
		p++
	}
	return p
}

// lineBreaks are the ways lineBreakEnd reads a line break, with the tab of
// its indentation: raw, escaped once (inside a JSON or Go string) and escaped
// twice (go test -json's view of a test's %q).
var lineBreaks = [...]struct {
	newlines [2]string
	tab      string
}{
	{[2]string{"\n", "\r\n"}, "\t"},
	{[2]string{`\n`, `\r\n`}, `\t`},
	{[2]string{`\\n`, `\\r\\n`}, `\\t`},
}

// lineBreakEnd is where the text after a line break at b[p] and its
// indentation starts, or -1. The break is a newline ("\n" or "\r\n") written
// raw or escaped once or twice; the indentation is up to breakIndent spaces,
// and tabs written as the newline is.
func lineBreakEnd(b []byte, p, limit int) int {
	for _, brk := range lineBreaks {
		for _, nl := range brk.newlines {
			if !bytes.HasPrefix(b[p:limit], []byte(nl)) {
				continue
			}
			p += len(nl)
			for n := 0; n < breakIndent && p < limit; n++ {
				switch {
				case b[p] == ' ':
					p++
				case bytes.HasPrefix(b[p:limit], []byte(brk.tab)):
					p += len(brk.tab)
				default:
					return p
				}
			}
			return p
		}
	}
	return -1
}

// anthropicSpans are b's Anthropic credentials, in order.
func anthropicSpans(b []byte, limitAt func(int) int) [][2]int {
	var spans [][2]int
	for j := 0; ; {
		i := bytes.Index(b[j:], []byte(anthropicPrefix))
		if i < 0 {
			return spans
		}
		start := j + i
		limit := limitAt(start)
		end := run(b, min(limit, start+len(anthropicPrefix)), limit)
		// A token starting later in the run ends where this one does.
		j = end
		taken := end - start - len(anthropicPrefix)
		for breaks := 0; taken < anthropicRun && breaks < 4; breaks++ {
			next := lineBreakEnd(b, end, limit)
			if next < 0 || next >= limit || !RunByte(b[next]) {
				break
			}
			end = run(b, next, limit)
			taken += end - next
		}
		if taken >= anthropicRun {
			spans = append(spans, [2]int{start, end})
			j = end
		}
	}
}

// openAISpans are b's OpenAI API keys, in order.
func openAISpans(b []byte, limitAt func(int) int) [][2]int {
	var spans [][2]int
	for j := 0; ; {
		i := bytes.Index(b[j:], []byte(openAIPrefix))
		if i < 0 {
			return spans
		}
		start := j + i
		end := run(b, start+len(openAIPrefix), max(start+len(openAIPrefix), limitAt(start)))
		key := b[start+len(openAIPrefix) : end]
		marked := len(key) >= openAIRun && bytes.Contains(key, []byte(openAIMarker))
		unmarked := (start == 0 || !RunByte(b[start-1])) && len(key) >= openAIUnmarkedRun && mixedRun(key)
		if marked || unmarked {
			spans = append(spans, [2]int{start, end})
		}
		// A prefix later in the run has a shorter run and a letter before it,
		// so it is taken only if this one is.
		j = end
	}
}

// mixedRun reports whether run holds an upper-case letter, a lower-case
// letter and a digit.
func mixedRun(run []byte) bool {
	var upper, lower, digit bool
	for _, c := range run {
		upper = upper || 'A' <= c && c <= 'Z'
		lower = lower || 'a' <= c && c <= 'z'
		digit = digit || '0' <= c && c <= '9'
	}
	return upper && lower && digit
}

// webTokenSpans are b's JSON Web Tokens, in order.
func webTokenSpans(b []byte, limitAt func(int) int) [][2]int {
	var spans [][2]int
	for j := 0; ; {
		i := bytes.Index(b[j:], []byte(webTokenPrefix))
		if i < 0 {
			return spans
		}
		start := j + i
		limit := limitAt(start)
		// part is a header's or payload's run, with any line break that cuts
		// it, or that comes before the dot after it.
		part := func(p int) int {
			for {
				p = run(b, p, limit)
				next := lineBreakEnd(b, p, limit)
				if next < 0 || next >= limit || !RunByte(b[next]) && b[next] != '.' {
					return p
				}
				p = next
			}
		}
		// A token starting later in the header's first run ends where this
		// one does.
		j = run(b, start, limit)
		header := part(start)
		if header-start < webTokenPart || header >= limit || b[header] != '.' || !bytes.HasPrefix(b[header+1:limit], []byte(webTokenPrefix)) {
			continue
		}
		payload := part(header + 1)
		if payload-header-1 < webTokenPart || payload >= limit || b[payload] != '.' {
			continue
		}
		end := run(b, payload+1, limit)
		spans = append(spans, [2]int{start, end})
		j = end
	}
}

// tokenFieldSpans are the values of b's OAuth token fields, in order.
func tokenFieldSpans(b []byte, limitAt func(int) int) [][2]int {
	var spans [][2]int
	for j := 0; j < len(b); j++ {
		if c := b[j] | 0x20; c != 'r' && c != 'a' && c != 'i' || j > 0 && fieldByte(b[j-1]) {
			continue
		}
		key := 0
		for _, f := range tokenFields {
			if len(b)-j >= len(f) && bytes.EqualFold(b[j:j+len(f)], []byte(f)) {
				key = len(f)
				break
			}
		}
		if key == 0 || j+key < len(b) && fieldByte(b[j+key]) {
			continue
		}
		p := skipSpaces(b, skipQuote(b, j+key))
		if p >= len(b) || b[p] != ':' && b[p] != '=' {
			continue
		}
		p = skipQuote(b, skipSpaces(b, p+1))
		end, limit := p, limitAt(p)
		for end < limit && tokenValueByte(b[end]) {
			end++
		}
		if end-p >= tokenFieldValue {
			spans = append(spans, [2]int{p, end})
			j = end - 1
		}
	}
	return spans
}

// fieldByte is a byte of a field name: a letter, a digit or '_'.
func fieldByte(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '_'
}

// tokenValueByte is a byte of a token's value: base64 and base64url's
// alphabets, '.', '~' and '%' (a percent-escaped value).
func tokenValueByte(c byte) bool {
	return RunByte(c) || c == '+' || c == '/' || c == '=' || c == '.' || c == '~' || c == '%'
}

// skipQuote is p after a quote at b[p:]: `"` or `'`, or `"` escaped once
// (`\"`) or twice (`\\\"`); else p.
func skipQuote(b []byte, p int) int {
	for _, q := range []string{`"`, `'`, `\"`, `\\\"`} {
		if bytes.HasPrefix(b[p:], []byte(q)) {
			return p + len(q)
		}
	}
	return p
}

// fieldSpaces is the most spaces or tabs skipSpaces skips: a token field's
// name, its ':' or '=' and its value may be aligned in columns.
const fieldSpaces = 16

// skipSpaces is p after up to fieldSpaces spaces or tabs at b[p:].
func skipSpaces(b []byte, p int) int {
	for n := 0; n < fieldSpaces && p < len(b) && (b[p] == ' ' || b[p] == '\t'); n++ {
		p++
	}
	return p
}

// bearerScheme is the HTTP authorization scheme whose credential follows it
// (`Authorization: Bearer <token>`), compared in any case.
const bearerScheme = "bearer"

// bearerSpans are the credentials after b's "Bearer" schemes, in order: a
// scheme no field byte comes before, one to fieldSpaces spaces, and
// tokenFieldValue or more of the bytes a token holds, a letter and a digit
// among them. A word after "Bearer" in prose ("Bearer tokens", "bearer
// authorization_header") has no digit or is short, and is kept.
func bearerSpans(b []byte, limitAt func(int) int) [][2]int {
	var spans [][2]int
	for j := 0; j+len(bearerScheme) < len(b); j++ {
		if b[j]|0x20 != 'b' || j > 0 && fieldByte(b[j-1]) || !bytes.EqualFold(b[j:j+len(bearerScheme)], []byte(bearerScheme)) {
			continue
		}
		p := j + len(bearerScheme)
		if b[p] != ' ' {
			continue
		}
		p = skipSpaces(b, p)
		end, limit := p, limitAt(p)
		letter, digit := false, false
		for end < limit && tokenValueByte(b[end]) {
			c := b[end]
			letter = letter || 'a' <= c|0x20 && c|0x20 <= 'z'
			digit = digit || '0' <= c && c <= '9'
			end++
		}
		if end-p >= tokenFieldValue && letter && digit {
			spans = append(spans, [2]int{p, end})
			j = end - 1
		}
	}
	return spans
}

// NotCredentialVariables are names CredentialVariable's parts match that
// carry no secret: a commit's author (AUTHOR holds AUTH), the SSH agent's
// socket, the X authority file, a password prompt program and when to use
// it, the keyring daemon's control directory and pass's store directory.
// Each is a location, a program or a person's name, and redacting it would
// rewrite ordinary output, test names in landing gate output included.
var NotCredentialVariables = []string{
	"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_AUTHOR_DATE",
	"SSH_AUTH_SOCK", "XAUTHORITY",
	"SSH_ASKPASS", "SSH_ASKPASS_REQUIRE", "SUDO_ASKPASS", "GIT_ASKPASS",
	"GNOME_KEYRING_CONTROL", "PASSWORD_STORE_DIR",
}

// CredentialVariable reports whether an environment variable carries a
// credential by its name, as workflow environments are refused such names: a
// provider key or token, a password or a secret. The names in
// NotCredentialVariables do not.
func CredentialVariable(name string) bool {
	name = strings.ToUpper(name)
	if slices.Contains(NotCredentialVariables, name) {
		return false
	}
	for _, part := range []string{"TOKEN", "SECRET", "PASS", "CREDENTIAL", "KEY", "AUTH"} {
		if strings.Contains(name, part) {
			return true
		}
	}
	return false
}

// MinimumValue is the shortest credential-named value redacted: a shorter
// one would cut ordinary words out of the output, and no real credential is
// that short.
const MinimumValue = 8

// EnvironmentValues are the values, MinimumValue bytes or longer, of env's
// credential-named variables ("NAME=value" entries).
func EnvironmentValues(env []string) [][]byte {
	var values [][]byte
	for _, entry := range env {
		if name, value, ok := strings.Cut(entry, "="); ok && CredentialVariable(name) && len(value) >= MinimumValue {
			values = append(values, []byte(value))
		}
	}
	return values
}
