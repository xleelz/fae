package daemon

import (
	"encoding/base32"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf16"
)

// redactionUTF16 is text's UTF-16, little- or big-endian, as Go converts it.
func redactionUTF16(text string, big bool) string {
	var b []byte
	for _, u := range utf16.Encode([]rune(text)) {
		if big {
			b = append(b, byte(u>>8), byte(u))
		} else {
			b = append(b, byte(u), byte(u>>8))
		}
	}
	return string(b)
}

// FAE-SEC-003: Go's views of a secret as a []rune (%q, %U, %v), %o and %b of
// its bytes, its base32 in the extended-hex alphabet (padded or not, in
// either case), and its UTF-16 (either byte order, raw, and a text's as go
// test -json escapes it) are redacted, for a configured value and a binary file
// credential alike. Each view's text is replaced whole, and a copy through
// copyRedacted gives the same. Ordinary text in those shapes, and a view of
// only the secret's start, is kept. On the base, each leaked.
func TestFailureSecretsFindRuneViewsOctalBinaryBase32HexAndUTF16(t *testing.T) {
	configured := `Pl4nted'Rune\View"-0123456789`
	s, credential := redactionWrapSecrets(t, configured)
	for name, secret := range map[string][]byte{"configured value": []byte(configured), "credential": credential} {
		runes := []rune(string(secret))
		hexed := base32.HexEncoding.EncodeToString(secret)
		le, be := redactionUTF16(string(secret), false), redactionUTF16(string(secret), true)
		event, _ := json.Marshal(le)
		forms := map[string]string{
			"%q of []rune":     fmt.Sprintf("%q", runes),
			"%U of []rune":     fmt.Sprintf("%U", runes),
			"%v of []rune":     fmt.Sprintf("%v", runes),
			"%o":               fmt.Sprintf("%o", secret),
			"%b":               fmt.Sprintf("%b", secret),
			"base32hex":        hexed,
			"base32hex, raw":   strings.TrimRight(hexed, "="),
			"base32hex, lower": strings.ToLower(hexed),
			"UTF-16LE":         le,
			"UTF-16BE":         be,
		}
		if name == "configured value" {
			// JSON writes each byte that is not UTF-8 as U+FFFD, so only a
			// text's UTF-16 reaches go test -json whole.
			forms["UTF-16LE in go test -json"] = `{"Action":"output","Package":"p","Test":"T","Output":` + string(event) + "}"
		}
		for form, text := range forms {
			got := s.redactString("a " + text + " b")
			if !strings.Contains(got, redactedMark) || len(text) > 24 && strings.Contains(got, text[len(text)/2:len(text)/2+8]) {
				t.Errorf("%s as %s: %q", name, form, got)
			}
			if strings.HasPrefix(text, "[") && got != "a ["+redactedMark+"] b" || strings.HasPrefix(form, "base32hex") && got != "a "+redactedMark+" b" {
				t.Errorf("%s as %s: not replaced whole: %q", name, form, got)
			}
			redactionStreamMatches(t, s, name+" as "+form, "a "+text+" b")
		}
	}
	short := []rune(configured[:12])
	for _, kept := range []string{
		"['a' 'b' 'c'] [U+0041 U+0042] [101 102 103] [1100001 1100010]",
		fmt.Sprintf("%q %U %o %b", short, short, []byte(configured[:12]), []byte(configured[:12])),
		base32.HexEncoding.EncodeToString([]byte(configured[:12])),
		redactionUTF16(configured[:12], false),
	} {
		if got := s.redactString(kept); got != kept {
			t.Errorf("ordinary text changed: %q -> %q", kept, got)
		}
	}
}
