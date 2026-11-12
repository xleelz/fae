package daemon

import (
	"bytes"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
)

// copyRedacted replaces a secret wherever the reads split it, escaped or
// not, and an Anthropic-shaped token too: one byte per read, and at every
// offset around the chunk boundary of a large stream. Its output equals
// redact over the whole stream, and a read error is returned.
func TestSecretValuesCopyRedactedAcrossReads(t *testing.T) {
	secret := "planted&secret/value-0123456789"
	s := newSecretValues([]byte(secret))
	token := "sk-ant-api03-" + strings.Repeat("Zq7_", 8)
	encoded := []string{secret, redactionJSON(secret, true), url.QueryEscape(secret), redactionJSON(redactionQuoted(secret), true), token}
	small := []byte("start " + strings.Join(encoded, " | ") + " end\n")
	var out bytes.Buffer
	if err := s.copyRedacted(&out, iotest.OneByteReader(bytes.NewReader(small))); err != nil {
		t.Fatal(err)
	}
	if want := "start " + strings.Repeat("[redacted] | ", len(encoded)-1) + "[redacted] end\n"; out.String() != want {
		t.Fatalf("one byte per read:\n got %q\nwant %q", out.String(), want)
	}
	const chunk = 256 << 10
	hold := s.hold()
	for _, form := range encoded {
		for _, at := range []int{chunk - hold - len(form), chunk - hold - 1, chunk - hold, chunk - len(form)/2, chunk - 1, chunk, chunk + hold - 1, 2*chunk + hold - len(form)} {
			stream := append(append(bytes.Repeat([]byte{'.'}, at), form...), bytes.Repeat([]byte{'.'}, chunk)...)
			out.Reset()
			if err := s.copyRedacted(&out, bytes.NewReader(stream)); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(out.Bytes(), s.redact(stream)) || bytes.Contains(out.Bytes(), []byte(form)) || bytes.Count(out.Bytes(), []byte(redactedMark)) != 1 {
				t.Fatalf("%q at %d not replaced whole (%d marks)", form, at, bytes.Count(out.Bytes(), []byte(redactedMark)))
			}
		}
	}
	failing := errors.New("planted read failure")
	if err := s.copyRedacted(&out, iotest.ErrReader(failing)); !errors.Is(err, failing) {
		t.Fatal("a read error was not returned", err)
	}
}

// A field the shim cut at its bound keeps no trailing start of a secret of
// redactedCutPrefix bytes or more; a shorter start is kept, and an uncut
// field is redacted only of whole secrets.
func TestSecretValuesRedactCutTail(t *testing.T) {
	secret := "0123456789abcdef0123456789abcdef"
	s := newSecretValues([]byte(secret), []byte("planted/secret+value"))
	for in, want := range map[string]string{
		"--token=" + secret[:4]:            "--token=[redacted]",
		"--token=" + secret[:20]:           "--token=[redacted]",
		"x " + secret + " y " + secret[:9]: "x [redacted] y [redacted]",
		"--token=" + secret[:3]:            "--token=" + secret[:3],
		"--header=Bearer%20" + secret[:6]:  "--header=Bearer%20[redacted]",
		"?key=planted%2Fsec":               "?key=[redacted]",
		"?key=planted%2":                   "?key=[redacted]",
		"?key=pla%2":                       "?key=pla%2",
	} {
		if got := s.redactCut(in); got != want {
			t.Errorf("redactCut(%q) = %q, want %q", in, got, want)
		}
	}
	if in := "--token=" + secret[:20]; s.redactString(in) != in {
		t.Error("an uncut field lost the start of a secret")
	}
}

// A landing's gate output is copied redacted when the secrets can be read;
// when they cannot, nothing is copied and the reason is returned.
func TestRedactLandingOutputWithholdsWhenSecretsUnreadable(t *testing.T) {
	root := t.TempDir()
	d := &Daemon{layout: Layout{Credential: filepath.Join(root, "credential"), Run: root}}
	operator := writeRedactionCredential(t, d.layout.Credential, 'k')
	writeRedactionCredential(t, filepath.Join(root, "plane-credential"), 'q')
	var out bytes.Buffer
	if err := d.redactLandingOutput(&out, strings.NewReader("operator "+string(operator)+"\n")); err != nil || out.String() != "operator [redacted]\n" {
		t.Fatalf("%q %v", out.String(), err)
	}
	if err := os.Remove(d.layout.Credential); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	err := d.redactLandingOutput(&out, strings.NewReader("operator "+string(operator)+"\n"))
	if err == nil || !strings.Contains(err.Error(), "gate output withheld: the secrets it must be redacted of could not be read") || out.Len() != 0 {
		t.Fatalf("unreadable secrets: %q %v", out.String(), err)
	}
}

// The UI child's log writer flushes an unterminated flood but keeps a tail
// long enough for any encoded secret the next write completes.
func TestUIRedactWriterKeepsAnEncodedSecretAcrossAFlood(t *testing.T) {
	secret := strings.Repeat("0123456789abcdef", 4)
	encoded := ""
	for i := 0; i < len(secret); i++ {
		encoded += "%" + strings.ToUpper(hexByte(secret[i]))
	}
	var out bytes.Buffer
	w := newRedactWriter(&out, []string{secret})
	split := len(encoded) - 20
	if _, err := w.Write([]byte(strings.Repeat("a", 70<<10) + encoded[:split])); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(encoded[split:] + "\n")); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); strings.Contains(got, encoded[:24]) || strings.Contains(got, encoded[split:]) || !strings.HasSuffix(got, redactedMark+"\n") {
		t.Fatalf("the flood leaked part of the percent-encoded secret: ...%q", got[len(got)-min(len(got), 300):])
	}
}

func hexByte(b byte) string {
	const digits = "0123456789abcdef"
	return string([]byte{digits[b>>4], digits[b&15]})
}
