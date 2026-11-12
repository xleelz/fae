package daemon

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xleelz/fae/controller"
)

// redactionEventOutputs joins each test's go test -json output events, in
// order, after checking that every line is still one JSON event.
func redactionEventOutputs(t *testing.T, stream []byte) map[string]string {
	t.Helper()
	outputs := map[string]string{}
	for _, line := range bytes.Split(bytes.TrimSpace(stream), []byte("\n")) {
		var e struct{ Action, Package, Test, Output string }
		if err := json.Unmarshal(line, &e); err != nil {
			t.Fatalf("a line is no longer one JSON event: %v\n%s", err, line)
		}
		if e.Action == "output" {
			outputs[e.Test] += e.Output
		}
	}
	return outputs
}

// FAE-SEC-003: test2json cuts a test's output line longer than 1 KiB into
// several output events with no newline between them. A configured secret
// printed across that cut, by fmt.Println, t.Logf and t.Errorf at offsets
// around the first and second cut, is redacted whole in a real go test -json
// stream from this host's go, through copyRedacted as a landing keeps its gate
// output: each line, its events joined, reads its text with the secret
// replaced by one [redacted], every line of the copy is still one event, and
// the copy's test counts equal the stream's. A long line holding only the
// secret's start before the cut, going on with other text, is kept. So are
// shaped credentials no value names: a 2 KiB JSON Web Token, an Anthropic
// token and a refresh token field's value, each cut. On the base both halves
// of each cut secret stayed, and each cut token leaked whole or in part.
func TestCopyRedactedJoinsGoTestJSONEventsCutWithoutANewline(t *testing.T) {
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go on PATH: the stream is go test -json's own")
	}
	configured := "Fae0fakeSecretValue9XyZ"
	s, _ := redactionWrapSecrets(t, configured)
	dir := t.TempDir()
	type line struct{ name, before, after string }
	var lines []line
	var body strings.Builder
	body.WriteString("package cut\n\nimport (\n\t\"fmt\"\n\t\"testing\"\n)\n\nconst secret = " + fmt.Sprintf("%q", configured) + "\n\n")
	for i, k := range []int{1, 4, 9, len(configured) - 1} {
		name := fmt.Sprintf("TestPrintln%d", i)
		before, after := strings.Repeat("x", 1024-k), strings.Repeat("y", 50)
		lines = append(lines, line{name, before, after + "\n"})
		fmt.Fprintf(&body, "func %s(t *testing.T) { fmt.Println(%q + secret + %q) }\n\n", name, before, after)
		name = fmt.Sprintf("TestSecondCut%d", i)
		before = strings.Repeat("v", 2048-k)
		lines = append(lines, line{name, before, after + "\n"})
		fmt.Fprintf(&body, "func %s(t *testing.T) { fmt.Println(%q + secret + %q) }\n\n", name, before, after)
	}
	// go test prefixes a log line with four spaces and "cut_test.go:N: ".
	fmt.Fprintf(&body, "func TestLogf(t *testing.T) { t.Logf(\"%%s%%s\", %q, secret) }\n\n", strings.Repeat("z", 1000))
	fmt.Fprintf(&body, "func TestErrorf(t *testing.T) { t.Errorf(\"payload: %%s\", %q+secret) }\n\n", strings.Repeat("w", 990))
	kept := strings.Repeat("k", 1024-9) + configured[:9] + "omething else entirely"
	fmt.Fprintf(&body, "func TestKept(t *testing.T) { fmt.Println(%q) }\n\n", kept)
	// Shaped credentials, no value known: a JSON Web Token of 2 KiB whose
	// header the first cut splits, an Anthropic token cut in its first 16
	// bytes, and a refresh token field's value cut.
	segment := func(v string) string { return base64.RawURLEncoding.EncodeToString([]byte(v)) }
	jwt := segment(`{"alg":"RS256","typ":"JWT"}`) + "." + segment(`{"sub":"planted-subject","aud":"`+strings.Repeat("planted-audience-", 80)+`"}`) + "." + segment(strings.Repeat("\x9c\x01\xfe", 40))
	anthropic := "sk-ant-api03-" + strings.Repeat("Ab3_9z-Q", 8)
	refresh := `{"tokens":{"refresh_token":"rt1.Zq8vLm2Np4Rs6Tu8Vw0Xy1Za3Bc5De7Fg9Hi1Jk3-_"}}`
	shaped := []line{
		{"TestWebTokenCut", strings.Repeat("j", 1024-10) + " ", " end\n"},
		{"TestAnthropicCut", strings.Repeat("a", 1024-12) + " ", " end\n"},
		{"TestRefreshCut", strings.Repeat("r", 1024-strings.Index(refresh, "rt1.")-6) + ` {"tokens":{"refresh_token":"`, `"}}` + "\n"},
	}
	for i, token := range []string{jwt, anthropic, refresh[strings.Index(refresh, "rt1."):strings.LastIndex(refresh, `"}}`)]} {
		fmt.Fprintf(&body, "func %s(t *testing.T) { fmt.Println(%q) }\n\n", shaped[i].name, shaped[i].before+token+strings.TrimSuffix(shaped[i].after, "\n"))
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.invalid/cut\n\ngo 1.21\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cut_test.go"), []byte(body.String()), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(gobin, "test", "-json", "-count=1", ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOFLAGS=", "GOWORK=off")
	raw, err := cmd.Output()
	if err == nil {
		t.Fatal("TestErrorf did not fail the run")
	}
	// The stream itself must cut the secret: an output event that ends, with
	// no newline, in the secret's first 4 bytes or more.
	cuts := 0
	for _, l := range bytes.Split(raw, []byte("\n")) {
		var e struct{ Action, Output string }
		if json.Unmarshal(l, &e) != nil || e.Action != "output" || strings.HasSuffix(e.Output, "\n") {
			continue
		}
		for k := 4; k < len(configured); k++ {
			if strings.HasSuffix(e.Output, configured[:k]) {
				cuts++
				break
			}
		}
	}
	if cuts < 4 {
		t.Fatalf("the stream cuts the secret between events only %d times:\n%s", cuts, raw)
	}
	var out bytes.Buffer
	if err := s.copyRedacted(&out, bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	outputs := redactionEventOutputs(t, out.Bytes())
	for _, l := range lines {
		if want := l.before + redactedMark + l.after; !strings.Contains(outputs[l.name], want) {
			t.Errorf("%s: the secret across the cut is not one [redacted]:\n%q", l.name, outputs[l.name])
		}
	}
	for _, name := range []string{"TestLogf", "TestErrorf"} {
		if !strings.Contains(outputs[name], redactedMark) {
			t.Errorf("%s: no [redacted]:\n%q", name, outputs[name])
		}
	}
	for _, l := range shaped {
		if want := l.before + redactedMark + l.after; !strings.Contains(outputs[l.name], want) {
			t.Errorf("%s: the token across the cut is not one [redacted]:\n%q", l.name, outputs[l.name])
		}
	}
	if !strings.Contains(outputs["TestKept"], kept+"\n") {
		t.Errorf("a line with only the secret's start was changed:\n%q", outputs["TestKept"])
	}
	for _, part := range []string{configured[:9], configured[len(configured)-9:]} {
		if n := bytes.Count(out.Bytes(), []byte(part)); part == configured[:9] && n != 1 || part != configured[:9] && n != 0 {
			t.Errorf("the copy holds %q %d times:\n%s", part, n, out.Bytes())
		}
	}
	want, rawErr := controller.GoTestCounts(raw, "")
	got, copyErr := controller.GoTestCounts(out.Bytes(), "")
	if rawErr != nil || copyErr != nil || fmt.Sprint(want.Executed, want.Packages, want.Failed) != fmt.Sprint(got.Executed, got.Packages, got.Failed) {
		t.Fatalf("the copy counts differently: %v %v %+v %+v", rawErr, copyErr, want, got)
	}
	if !bytes.Equal(out.Bytes(), s.redact(raw)) {
		t.Fatal("copyRedacted differs from redact over the whole stream")
	}
}

// FAE-SEC-003: a form is joined across two go test -json output events with
// no newline between them only when the second is an output event of the
// same package and test as the first, as test2json writes a cut line; the
// same text split between two tests' events, or before another action's
// event, is kept. The earlier join after an escaped newline follows the same
// rule. An event whose start precedes the text in view (a stream's earlier
// text) is joined.
func TestSecretEventJoinNeedsTheSameTestAndPackage(t *testing.T) {
	configured := "Pl4nted-Event-Value-0123456789"
	s, _ := redactionWrapSecrets(t, configured)
	event := func(pkg, test, output string) string {
		b, _ := json.Marshal(output)
		return `{"Time":"2026-10-30T00:00:00.1Z","Action":"output","Package":"` + pkg + `","Test":"` + test + `","Output":` + string(b) + "}\n"
	}
	for _, c := range []int{1, 9, len(configured) - 1} {
		for name, brk := range map[string]string{"no newline": "", "escaped newline": "\n"} {
			joined := event("p", "TestA", "x "+configured[:c]+brk) + event("p", "TestA", configured[c:]+" y\n")
			if got := string(s.redact([]byte(joined))); strings.Contains(got, configured[c:]) && c < len(configured)-1 || strings.Contains(got, configured[:c]) && c > 1 || !strings.Contains(got, `x [redacted] y\n`) {
				t.Errorf("%s at %d, same test: %s", name, c, got)
			}
			redactionStreamMatches(t, s, fmt.Sprintf("%s at %d", name, c), joined)
			for other, second := range map[string]string{
				"another test":    event("p", "TestB", configured[c:]+" y\n"),
				"another package": event("q", "TestA", configured[c:]+" y\n"),
				"another action":  `{"Action":"pass","Package":"p","Test":"TestA","Output":"` + configured[c:] + `"}` + "\n",
			} {
				text := event("p", "TestA", "x "+configured[:c]+brk) + second
				if got := string(s.redact([]byte(text))); got != text {
					t.Errorf("%s at %d, %s: changed:\n%s", name, c, other, got)
				}
			}
		}
	}
	// The first event's start is not in view: its package and test are
	// unknown, and the events are joined.
	first := event("p", "TestA", "x "+configured[:9])
	tail := first[strings.Index(first, `"Output":"`)+len(`"Output":"`):]
	if got := string(s.redact([]byte(tail + event("q", "TestB", configured[9:]+" y\n")))); strings.Contains(got, configured[9:]) || strings.Contains(got, configured[:9]) {
		t.Errorf("an event whose start is out of view was not joined: %s", got)
	}
}
