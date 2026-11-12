package daemon

import (
	"bytes"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/iotest"
)

// redactionWrapSecrets is the daemon's secret set for the wave-11 form
// tests: a 32-byte operator credential of varied bytes, and a configured
// value, of the live configuration's environment.
func redactionWrapSecrets(t *testing.T, configured string) (secretValues, []byte) {
	t.Helper()
	root := t.TempDir()
	d := &Daemon{layout: Layout{Credential: filepath.Join(root, "credential"), Run: root}}
	sum := sha256.Sum256([]byte("planted operator credential"))
	if err := os.WriteFile(d.layout.Credential, sum[:], 0600); err != nil {
		t.Fatal(err)
	}
	writeRedactionCredential(t, filepath.Join(root, "plane-credential"), 'p')
	secrets, err := d.failureSecrets(map[string]string{"PLANTED_WRAP_TOKEN": configured})
	if err != nil {
		t.Fatal(err)
	}
	return secrets, sum[:]
}

// redactionWrap breaks text every width bytes with brk.
func redactionWrap(text string, width int, brk string) string {
	var b strings.Builder
	for len(text) > width {
		b.WriteString(text[:width] + brk)
		text = text[width:]
	}
	b.WriteString(text)
	return b.String()
}

// redactionStreamMatches checks that copyRedacted, one byte per read, gives
// what redact gives over the whole text.
func redactionStreamMatches(t *testing.T, s secretValues, name, text string) {
	t.Helper()
	var out bytes.Buffer
	if err := s.copyRedacted(&out, iotest.OneByteReader(strings.NewReader(text))); err != nil || out.String() != string(s.redact([]byte(text))) {
		t.Errorf("%s: the stream differs from the whole:\n%q\n%q", name, out.String(), s.redact([]byte(text)))
	}
}

// FAE-SEC-003: a configured value, and a file credential's hex, base64 and
// spaced hex, wrapped across lines are redacted whole: broken at every
// column once; every few bytes; with "\r\n", indentation (go test's 8 spaces
// for a log message's next line, or a tab), or a line break that stands for
// a space of the form (od -A n's lines); with the break escaped in a JSON
// string; and split across two go test -json output events, which stay valid
// JSON. A copy through copyRedacted gives the same text. Text holding only
// the start of a value is kept. On the base, each leaked whole.
func TestFailureSecretsFindASecretWrappedAcrossLines(t *testing.T) {
	configured := "Pl4nted-Wr4pped-Value-0123456789"
	s, credential := redactionWrapSecrets(t, configured)
	cases := map[string]string{}
	for c := 1; c < len(configured); c++ {
		cases[fmt.Sprintf("broken at %d", c)] = configured[:c] + "\n" + configured[c:]
	}
	for _, width := range []int{3, 5, 8, 16} {
		for name, brk := range map[string]string{"newline": "\n", "CRLF": "\r\n", "indented": "\n        ", "tab": "\n\t"} {
			cases[fmt.Sprintf("every %d, %s", width, name)] = redactionWrap(configured, width, brk)
		}
	}
	h := hex.EncodeToString(credential)
	cases["credential hex at 60 (xxd -p)"] = redactionWrap(h, 60, "\n")
	cases["credential hex at 16, upper case"] = redactionWrap(strings.ToUpper(h), 16, "\n")
	cases["credential base64 at 20"] = redactionWrap(base64.StdEncoding.EncodeToString(credential), 20, "\n")
	spaced := fmt.Sprintf("% x", credential)
	cases["od -A n -t x1"] = " " + spaced[:47] + "\n " + spaced[48:]
	quoted, _ := json.Marshal(configured[:9] + "\n        " + configured[9:])
	cases["in a JSON string"] = string(quoted)
	for name, text := range cases {
		got := s.redactString("a " + text + " b")
		kept := text[:len(text)-len(strings.TrimLeft(text, ` "`))] // od's leading space, a JSON string's quote
		if !strings.HasPrefix(got, "a "+kept+"[redacted]") || strings.Contains(got, configured[len(configured)-6:]) || strings.Contains(got, h[len(h)-8:]) {
			t.Errorf("%s: %q", name, got)
		}
		redactionStreamMatches(t, s, name, "a "+text+" b")
	}
	event := func(output, extra string) string {
		b, _ := json.Marshal(output)
		return `{"Time":"2026-10-29T00:00:00.123456789Z","Action":"output","Package":"example.invalid/planted","Test":"TestPlanted/case_one","Output":` + string(b) + extra + "}\n"
	}
	for _, extra := range []string{"", `,"OutputType":"error"`} {
		for _, c := range []int{1, 9, len(configured) - 1} {
			stream := `{"Action":"run","Package":"example.invalid/planted","Test":"TestPlanted/case_one"}` + "\n" +
				event("    planted_test.go:9: token "+configured[:c]+"\n", extra) +
				event("        "+configured[c:]+" end\n", "") +
				`{"Action":"pass","Package":"example.invalid/planted","Test":"TestPlanted/case_one"}` + "\n"
			got := string(s.redact([]byte(stream)))
			if strings.Contains(got, configured[c:]) && c < len(configured)-1 || strings.Contains(got, configured[:c]) && c > 1 || !strings.Contains(got, `token [redacted] end\n`) {
				t.Errorf("go test -json broken at %d%s:\n%s", c, extra, got)
			}
			for _, line := range strings.Split(strings.TrimSpace(got), "\n") {
				var e struct{ Action, Output string }
				if err := json.Unmarshal([]byte(line), &e); err != nil {
					t.Errorf("go test -json broken at %d: a line is no longer JSON: %v\n%s", c, err, line)
				}
			}
			redactionStreamMatches(t, s, fmt.Sprintf("go test -json at %d", c), stream)
		}
	}
	for _, kept := range []string{
		"a " + configured[:12] + "\nother text b",
		"a " + configured[:12] + "\n\n" + configured[12:] + " b",
		"a " + configured[:12] + "\n" + strings.Repeat(" ", 17) + configured[12:] + " b",
	} {
		if got := s.redactString(kept); got != kept {
			t.Errorf("text that is not a wrapped value changed: %q -> %q", kept, got)
		}
	}
}

// FAE-SEC-003: Go's views of a []byte, base32 and HTML character references
// are redacted: %v and %d, %#v (also wrapped 8 bytes a line, as go-cmp and
// xxd -i print it), % #x and % #X, base32 padded and not and in lower case,
// and, for a text value, every character as a decimal or hex reference and
// html.EscapeString of it holding & < > " '. On the base, each leaked.
func TestFailureSecretsFindGoByteViewsBase32AndHTMLReferences(t *testing.T) {
	configured := `Pl4nted&<Html>"Value'-0123456789`
	s, credential := redactionWrapSecrets(t, configured)
	references := func(v []byte, format string) string {
		var b strings.Builder
		for _, c := range v {
			fmt.Fprintf(&b, format, c)
		}
		return b.String()
	}
	for name, secret := range map[string][]byte{"configured value": []byte(configured), "credential": credential} {
		goBytes := fmt.Sprintf("%#v", secret)
		elements := strings.Split(strings.TrimSuffix(strings.TrimPrefix(goBytes, "[]byte{"), "}"), ", ")
		var wrapped strings.Builder
		wrapped.WriteString("[]byte{")
		for i, e := range elements {
			if i%8 == 0 {
				wrapped.WriteString("\n\t")
			} else {
				wrapped.WriteString(" ")
			}
			wrapped.WriteString(e + ",")
		}
		wrapped.WriteString("\n}")
		forms := map[string]string{
			"%v":            fmt.Sprintf("%v", secret),
			"%d":            fmt.Sprintf("%d", secret),
			"%#v":           goBytes,
			"%#v wrapped":   wrapped.String(),
			"% #x":          fmt.Sprintf("% #x", secret),
			"% #X":          fmt.Sprintf("% #X", secret),
			"base32":        base32.StdEncoding.EncodeToString(secret),
			"base32, raw":   base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(secret),
			"base32, lower": strings.ToLower(base32.StdEncoding.EncodeToString(secret)),
		}
		if name == "configured value" {
			// A reference names a character, so only text has them.
			forms["&#NNN; per character"] = references(secret, "&#%d;")
			forms["&#xHH; per character"] = references(secret, "&#x%02x;")
			forms["&#XHH; per character"] = references(secret, "&#X%02X;")
			forms["html.EscapeString"] = html.EscapeString(configured)
		}
		for form, text := range forms {
			got := s.redactString("a " + text + " b")
			if !strings.Contains(got, "[redacted]") || strings.Contains(got, text[len(text)/2:len(text)/2+6]) && len(text) > 40 {
				t.Errorf("%s as %s %q: %q", name, form, text, got)
			}
			if form == "%#v wrapped" && got != "a []byte{\n\t[redacted],\n} b" || form == "%v" && got != "a [[redacted]] b" || form == "base32" && got != "a [redacted] b" {
				t.Errorf("%s as %s: %q", name, form, got)
			}
			redactionStreamMatches(t, s, name+" as "+form, "a "+text+" b")
		}
	}
	for _, kept := range []string{"&amp; &lt;b&gt; &#83;&#51; [1 2 3] 0x01, 0x02 MFRGG===", fmt.Sprintf("%v", []byte(configured[:20]))} {
		if got := s.redactString(kept); got != kept {
			t.Errorf("ordinary text changed: %q -> %q", kept, got)
		}
	}
	// A value of capitals, such as a placeholder, is found in its own case
	// only, though its base32 is found in either.
	caps, _ := redactionWrapSecrets(t, "PLACEHOLDERVALUE")
	if got := caps.redactString("PLACEHOLDERVALUE placeholdervalue " + strings.ToLower(base32.StdEncoding.EncodeToString([]byte("PLACEHOLDERVALUE")))); got != "[redacted] placeholdervalue [redacted]" {
		t.Errorf("a value of capitals: %q", got)
	}
}

// redactionXXD is xxd's layout of b: an offset with a colon, two-byte groups
// and the bytes as text.
func redactionXXD(b []byte) string {
	var out strings.Builder
	for off := 0; off < len(b); off += 16 {
		end := min(off+16, len(b))
		fmt.Fprintf(&out, "%08x: ", off)
		for i := off; i < off+16; i += 2 {
			for k := i; k < i+2; k++ {
				if k < end {
					fmt.Fprintf(&out, "%02x", b[k])
				} else {
					out.WriteString("  ")
				}
			}
			out.WriteString(" ")
		}
		text := []byte(string(b[off:end]))
		for i, c := range text {
			if c < 0x20 || c > 0x7e {
				text[i] = '.'
			}
		}
		fmt.Fprintf(&out, " %s\n", text)
	}
	return out.String()
}

// redactionOD is od -t x1's layout of b: octal offsets, single-byte groups.
func redactionOD(b []byte) string {
	var out strings.Builder
	for off := 0; off < len(b); off += 16 {
		fmt.Fprintf(&out, "%07o", off)
		for _, c := range b[off:min(off+16, len(b))] {
			fmt.Fprintf(&out, " %02x", c)
		}
		out.WriteString("\n")
	}
	fmt.Fprintf(&out, "%07o\n", len(b))
	return out.String()
}

// redactionDumpProbes are, for each line of a dump of b with 16 bytes a
// line, a piece of its hex columns, and whether the line holds a byte of
// b[from:to].
func redactionDumpProbes(dump string, from, to int) map[string]bool {
	probes := map[string]bool{}
	k := 0
	for _, line := range strings.Split(dump, "\n") {
		fields := strings.Fields(line)
		offset := slices.IndexFunc(fields, func(f string) bool {
			f = strings.TrimSuffix(f, ":")
			return len(f) >= 6 && strings.Trim(f, "0123456789abcdef") == ""
		})
		if offset < 0 || len(fields) < offset+3 {
			continue
		}
		probe := fields[offset+1 : offset+3]
		if len(fields) > offset+3 && strings.Trim(fields[offset+3], "0123456789abcdef") == "" {
			probe = fields[offset+1 : offset+4]
		}
		probes[strings.Join(probe, " ")] = 16*k < to && 16*k+16 > from
		k++
	}
	return probes
}

// FAE-SEC-003: a hex dump of a raw secret is redacted, the lines holding its
// bytes, offsets, hex and text columns included: xxd, hexdump -C (Go's
// hex.Dump), od -t x1, the secret alone or inside a longer dump, a dump
// printed by t.Log (a log prefix on its first line, indentation after), in a
// testify-style diff, and in go test -json events, which stay valid JSON. The
// dump's other lines are kept, and so is a dump of other bytes or of part of
// the secret; a copy through copyRedacted gives the same text. On the base,
// each leaked.
func TestFailureSecretsFindHexDumps(t *testing.T) {
	configured := "Pl4nted-Dump-Value-0123456789xyz"
	s, credential := redactionWrapSecrets(t, configured)
	for name, secret := range map[string][]byte{"configured value": []byte(configured), "credential": credential} {
		h := hex.EncodeToString(secret)
		around := append(append([]byte("Head~Of~Dump-A:"), secret...), []byte(" and the tail end, ok")...)
		from, to := len("Head~Of~Dump-A:"), len("Head~Of~Dump-A:")+len(secret)
		logged := strings.Split(strings.TrimSuffix(hex.Dump(around), "\n"), "\n")
		dumps := map[string]struct {
			text     string
			from, to int
		}{
			"xxd":          {redactionXXD(secret), 0, len(secret)},
			"hexdump -C":   {hex.Dump(secret), 0, len(secret)},
			"od -t x1":     {redactionOD(secret), 0, len(secret)},
			"xxd, inside":  {redactionXXD(around), from, to},
			"hex.Dump, in": {hex.Dump(around), from, to},
			"od, inside":   {redactionOD(around), from, to},
			"t.Log":        {"    planted_test.go:12: " + strings.Join(logged, "\n        ") + "\n", from, to},
			"testify diff": {"\t            \t ([]uint8) (len=" + fmt.Sprint(len(around)) + ") {\n\t            \t-  " + strings.Join(logged, "\n\t            \t-  ") + "\n\t            \t }\n", from, to},
		}
		for form, dump := range dumps {
			text := "before\n" + dump.text + "after\n"
			got := s.redactString(text)
			if !strings.HasPrefix(got, "before\n") || !strings.HasSuffix(got, "after\n") || !strings.Contains(got, "[redacted]") || strings.Contains(got, h[10:18]) {
				t.Errorf("%s as %s:\n%s", name, form, got)
			}
			for probe, holds := range redactionDumpProbes(strings.ReplaceAll(dump.text, "planted_test.go:12: ", ""), dump.from, dump.to) {
				if strings.Contains(got, probe) == holds {
					t.Errorf("%s as %s: the line with %q (holding the secret: %v) is wrongly kept or dropped:\n%s", name, form, probe, holds, got)
				}
			}
			if name == "configured value" && strings.Contains(got, configured[8:14]) {
				t.Errorf("%s as %s keeps its text column:\n%s", name, form, got)
			}
			redactionStreamMatches(t, s, name+" as "+form, text)
		}
		// A dump in go test -json events, one line each.
		var stream strings.Builder
		for _, line := range logged {
			b, _ := json.Marshal("        " + line + "\n")
			stream.WriteString(`{"Action":"output","Package":"example.invalid/planted","Test":"TestPlanted","Output":` + string(b) + "}\n")
		}
		got := string(s.redact([]byte(stream.String())))
		if strings.Contains(got, h[10:18]) || !strings.Contains(got, "[redacted]") || !strings.Contains(got, "|, ok|") {
			t.Errorf("%s in go test -json:\n%s", name, got)
		}
		for _, line := range strings.Split(strings.TrimSpace(got), "\n") {
			var e struct{ Action, Output string }
			if err := json.Unmarshal([]byte(line), &e); err != nil {
				t.Errorf("%s in go test -json: a line is no longer JSON: %v\n%s", name, err, line)
			}
		}
		redactionStreamMatches(t, s, name+" in go test -json", stream.String())
	}
	other := sha256.Sum256([]byte("planted other bytes"))
	for _, kept := range []string{hex.Dump(other[:]), redactionXXD(other[:]), hex.Dump([]byte(configured[:20])), redactionOD(credential[:24])} {
		if got := s.redactString(kept); got != kept {
			t.Errorf("a dump of other bytes changed:\n%s\n%s", kept, got)
		}
	}
}

// The stream holds back enough for a wrapped value, a go test -json pair and
// a dump around each point copyRedacted writes up to: its output equals
// redact over the whole stream, and keeps nothing of the secret.
func TestSecretValuesCopyRedactedKeepsAWrappedValueAndADumpWhole(t *testing.T) {
	configured := "Pl4nted-Stream-Value-0123456789"
	s, credential := redactionWrapSecrets(t, configured)
	quoted, _ := json.Marshal("    x_test.go:3: " + configured[:7] + "\n")
	rest, _ := json.Marshal("        " + configured[7:] + "\n")
	forms := []string{
		redactionWrap(configured, 5, "\n        "),
		`{"Action":"output","Package":"p","Test":"T","Output":` + string(quoted) + "}\n" + `{"Action":"output","Package":"p","Test":"T","Output":` + string(rest) + "}\n",
		hex.Dump(credential),
		redactionXXD([]byte(configured)),
	}
	const chunk = 256 << 10
	hold := s.hold()
	for _, form := range forms {
		for _, at := range []int{chunk - hold - len(form), chunk - hold - 1, chunk - hold, chunk - len(form)/2, chunk - 1, chunk, chunk + hold - 1, 2*chunk + hold - len(form)} {
			stream := append(append(bytes.Repeat([]byte{'.'}, at), "\n"+form+"\n"...), bytes.Repeat([]byte{'.'}, chunk)...)
			var out bytes.Buffer
			if err := s.copyRedacted(&out, bytes.NewReader(stream)); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(out.Bytes(), s.redact(stream)) || bytes.Contains(out.Bytes(), []byte(configured[len(configured)-8:])) || bytes.Contains(out.Bytes(), []byte(hex.EncodeToString(credential[20:24]))) || !bytes.Contains(out.Bytes(), []byte(redactedMark)) {
				t.Fatalf("%q at %d not replaced whole", form[:20], at)
			}
		}
	}
}

// FAE-SEC-003: with every secret the daemon holds, a provider login's
// credentials it never reads are redacted by their field and shape: a Codex
// ChatGPT login's (synthetic) refresh token in its auth.json and in go test
// -json's view of it, an OpenAI key without the T3BlbkFJ marker, and a JSON
// Web Token and an Anthropic token wrapped where the part before the break
// is too short to be found alone. The account id and ordinary words are
// kept. On the base, each leaked.
func TestFailureSecretsFindTokenFieldsAndUnmarkedKeys(t *testing.T) {
	s, _ := redactionWrapSecrets(t, "Pl4nted-Login-Value-0123456789")
	refresh := "rt1.Zq8vLm2Np4Rs6Tu8Vw0Xy1Za3Bc5De7Fg9Hi1Jk3-_"
	segment := func(v string) string { return base64.RawURLEncoding.EncodeToString([]byte(v)) }
	jwt := segment(`{"alg":"RS256","typ":"JWT"}`) + "." + segment(`{"sub":"planted-subject","aud":["planted"]}`) + "." + segment(strings.Repeat("\x9c\x01\xfe", 40))
	unmarked := "sk-proj-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789abcdEFGHijklmnop"
	anthropic := "sk-ant-api03-" + strings.Repeat("Ab3_9z-Q", 8)
	login, _ := json.MarshalIndent(map[string]any{"tokens": map[string]string{"refresh_token": refresh, "account_id": "planted-account-0123456789"}}, "", "  ")
	event, _ := json.Marshal(string(login))
	for name, text := range map[string]string{
		"auth.json":              string(login),
		"go test -json of it":    `{"Action":"output","Output":` + string(event) + `}`,
		"an unmarked OpenAI key": "OPENAI_API_KEY=" + unmarked,
		"a wrapped web token":    "Bearer " + jwt[:14] + "\n        " + jwt[14:],
		"a wrapped Anthropic":    "key=" + anthropic[:11] + "\n" + anthropic[11:],
	} {
		got := s.redactString(text)
		for _, secret := range []string{refresh[4:20], unmarked[10:30], jwt[:12], jwt[len(jwt)-12:], anthropic[:10], anthropic[len(anthropic)-12:]} {
			if strings.Contains(got, secret) {
				t.Errorf("%s keeps %q:\n%s", name, secret, got)
			}
		}
		if strings.Contains(text, "planted-account") && !strings.Contains(got, "planted-account-0123456789") {
			t.Errorf("%s: the account id was redacted:\n%s", name, got)
		}
		redactionStreamMatches(t, s, name, text)
	}
}
