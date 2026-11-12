package daemon

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// redactionTest2JSON writes lines as go test -json writes a test's output
// (cmd/internal/test2json): each line as its own output event, a line longer
// than 1 KiB cut into events of 1 KiB with no newline between them, and the
// output JSON-encoded with encoding/json's HTML escaping (& as \u0026).
func redactionTest2JSON(pkg, test string, lines ...string) string {
	var b strings.Builder
	event := func(output string) {
		enc, _ := json.Marshal(output)
		fmt.Fprintf(&b, `{"Time":"2026-10-30T00:00:00Z","Action":"output","Package":%q,"Test":%q,"Output":%s}`+"\n", pkg, test, enc)
	}
	for _, line := range lines {
		for len(line) > 1024 {
			event(line[:1024])
			line = line[1024:]
		}
		event(line)
	}
	return b.String()
}

// redactionEscapedForms are a secret's bytes written one escape per byte, as
// a test's output holds them: percent-encoded, Go's \xNN and \uXXXX (as a %q
// of binary prints them, which go test -json JSON-escapes once more), and an
// HTML character reference per character, decimal or hex, with
// html.EscapeString for the characters it escapes.
func redactionEscapedForms(secret string) map[string]string {
	each := func(format string) string {
		var b strings.Builder
		for i := 0; i < len(secret); i++ {
			fmt.Fprintf(&b, format, secret[i])
		}
		return b.String()
	}
	return map[string]string{
		"%XX":    each("%%%02X"),
		`\xNN`:   each(`\x%02x`),
		`\uXXXX`: each(`\u%04x`),
		"&#NN;":  each("&#%d;"),
		"&#xHH;": each("&#x%x;"),
		"html":   html.EscapeString(secret),
	}
}

// FAE-SEC-003: in go test -json output, a configured secret written as
// escapes is redacted whole when test2json's 1 KiB cut falls anywhere in it,
// inside an escape unit included (`%4|6`, `\\x4|6`, `\\u004|6`,
// `\u0026#7|0;`), as the landing's copyRedacted keeps the gate output: each
// line reads with the form replaced by one [redacted], no part of it kept,
// and every line is still one event. HTML character references are found in
// that output too, where test2json writes their & as \u0026, cut or not. On
// the base a cut inside an escape unit left both halves (46 of 68 offsets of
// the percent form, 69 of 91 of \xNN, 115 of 137 of \uXXXX), and a reference
// in -json output was never found.
func TestFailureSecretsFindAFormCutInsideAnEscapeUnit(t *testing.T) {
	configured := `Fae0fake&Secret<Value>9XyZ`
	s, _ := redactionWrapSecrets(t, configured)
	for name, form := range redactionEscapedForms(configured) {
		var lines, want []string
		for k := 1; k < len(form); k++ {
			prefix := strings.Repeat("a", 1024-k)
			lines = append(lines, fmt.Sprintf("%s%s<%s:%d>\n", prefix, form, name, k))
			want = append(want, fmt.Sprintf("%s%s<%s:%d>\n", prefix, redactedMark, name, k))
		}
		lines = append(lines, "short "+form+" line\n", "    cut_test.go:9: logged "+form+"\n")
		want = append(want, "short "+redactedMark+" line\n", "    cut_test.go:9: logged "+redactedMark+"\n")
		stream := redactionTest2JSON("cutmod", "TestCut", lines...)
		var out bytes.Buffer
		if err := s.copyRedacted(&out, strings.NewReader(stream)); err != nil {
			t.Fatal(err)
		}
		got := redactionEventOutputs(t, out.Bytes())["TestCut"]
		if got != strings.Join(want, "") {
			leaked := 0
			for _, w := range strings.Split(got, "\n") {
				if !strings.Contains(w, redactedMark) && w != "" {
					leaked++
				}
			}
			t.Errorf("%s: %d of %d lines keep the form; first difference:\n%s", name, leaked, len(lines), redactionFirstDifference(got, strings.Join(want, "")))
		}
		if !bytes.Equal(out.Bytes(), s.redact([]byte(stream))) {
			t.Errorf("%s: copyRedacted differs from redact", name)
		}
	}
	// Each cut unit is found by the matcher alone too, at the form's first
	// byte: a form whose very first escape the cut falls inside.
	for name, form := range redactionEscapedForms(configured) {
		for _, k := range []int{1, 2, 3} {
			if k >= len(form) || form[k-1] == ';' {
				continue
			}
			stream := redactionTest2JSON("p", "TestFirst", strings.Repeat("b", 1024-k)+form+"\n")
			if got := redactionEventOutputs(t, s.redact([]byte(stream)))["TestFirst"]; got != strings.Repeat("b", 1024-k)+redactedMark+"\n" {
				t.Errorf("%s cut after %d byte(s) of its first unit: %q", name, k, got[max(0, len(got)-80):])
			}
		}
	}
	// Ordinary text with escapes and references, cut or not, is kept.
	kept := []string{strings.Repeat("c", 1020) + "%41%42 &#65;&amp; \\x41 plain\n", "http://h/?q=a%20b&amp;c=&#x41; \\u0041\n"}
	stream := redactionTest2JSON("p", "TestKept", kept...)
	if got := string(s.redact([]byte(stream))); got != stream {
		t.Errorf("ordinary text changed:\n%s", got)
	}
	// A secret containing '"' (or a binary credential with a '"' byte) cut at
	// any offset in raw, %q, %+q, or hex output is redacted whole without
	// mistaking the first event's closing JSON quote for the secret's '"'.
	for _, secret := range []string{
		`Fae0fake"SecretValue9XyZ`,
		"pl@nted key/v&lue<x>\"q\\\u00e9\U0001F600+%",
		string([]byte{0xF0, 0x9F, 0x98, 0x80, 'k', 0xC3, 0xA9, 0x01, '\t', '"', '\\', '<', '&', 0xFF, 0x80, 'Z', 0xE2, 0x80, 0xA8, 'q', 0x7F, 0x00, 'A', 'b', 0x1B, 'c', 0xF0, 0x9F, 0x8E, 0x89, 'x', '9'}),
	} {
		sq, _ := redactionWrapSecrets(t, secret)
		for name, form := range map[string]string{
			"raw-quote": secret,
			"%q-quote":  fmt.Sprintf("%q", secret),
			"%+q-quote": fmt.Sprintf("%+q", secret),
			"hex-quote": hex.EncodeToString([]byte(secret)),
		} {
			if strings.Contains(form, "\n") || strings.Contains(form, "\t") || !utf8.ValidString(form) {
				continue
			}
			var lines, want []string
			for k := 1; k < len(form); k++ {
				if !utf8.ValidString(form[:k]) {
					continue
				}
				prefix := strings.Repeat("x", 1024-k)
				lines = append(lines, fmt.Sprintf("%s%s<%s:%d>\n", prefix, form, name, k))
				if strings.HasPrefix(form, `"`) {
					want = append(want, fmt.Sprintf("%s\"%s\"<%s:%d>\n", prefix, redactedMark, name, k))
				} else {
					want = append(want, fmt.Sprintf("%s%s<%s:%d>\n", prefix, redactedMark, name, k))
				}
			}
			qStream := redactionTest2JSON("quotemod", "TestQuoteCut", lines...)
			var qOut bytes.Buffer
			if err := sq.copyRedacted(&qOut, strings.NewReader(qStream)); err != nil {
				t.Fatal(err)
			}
			got := redactionEventOutputs(t, qOut.Bytes())["TestQuoteCut"]
			if got != strings.Join(want, "") {
				t.Errorf("%s: quote-cut difference:\n%s", name, redactionFirstDifference(got, strings.Join(want, "")))
			}
			if !bytes.Equal(qOut.Bytes(), sq.redact([]byte(qStream))) {
				t.Errorf("%s: copyRedacted differs from redact", name)
			}
		}
	}
}

// redactionFirstDifference shows where got and want first differ.
func redactionFirstDifference(got, want string) string {
	i := 0
	for i < len(got) && i < len(want) && got[i] == want[i] {
		i++
	}
	from := max(0, i-60)
	return fmt.Sprintf("got  %q\nwant %q", got[from:min(len(got), i+80)], want[from:min(len(want), i+80)])
}

// FAE-SEC-003: the matcher, with the daemon's planted secrets, changes
// nothing in this package's own sources and in the landing's: no form of the
// planted values occurs in ordinary Go source, and the shapes find no
// credential there. (The test files hold the planted values themselves.)
func TestSecretRedactionKeepsTheDaemonsOwnSources(t *testing.T) {
	s, _ := redactionWrapSecrets(t, "Fae0fakeSecretValue9XyZ")
	n := 0
	for _, dir := range []string{".", "../controller", "../secretshape"} {
		names, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range names {
			if strings.HasSuffix(name, "_test.go") {
				continue
			}
			src, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			n += len(src)
			if got := s.redact(src); !bytes.Equal(got, src) {
				t.Errorf("%s: changed:\n%s", name, redactionFirstDifference(string(got), string(src)))
			}
		}
	}
	if n < 1<<20 {
		t.Fatalf("only %d bytes scanned", n)
	}
}
