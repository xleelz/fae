package secretshape

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// shapeEvent is one go test -json output event of test in package p.
func shapeEvent(p, test, output string) string {
	b, _ := json.Marshal(output)
	return `{"Time":"2026-10-30T00:00:00.123Z","Action":"output","Package":"` + p + `","Test":"` + test + `","Output":` + string(b) + "}\n"
}

// shapeOutputs joins each test's output events, in order, after checking that
// every line is still one JSON event.
func shapeOutputs(t *testing.T, stream string) map[string]string {
	t.Helper()
	outputs := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(stream), "\n") {
		var e struct{ Action, Test, Output string }
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("a line is no longer one JSON event: %v\n%s", err, line)
		}
		if e.Action == "output" {
			outputs[e.Test] += e.Output
		}
	}
	return outputs
}

// FAE-SEC-003: go test -json writes each line of a test's output as its own
// event, and cuts a line longer than 1 KiB into several events with no
// newline between them. A shaped credential test2json cut, a JSON Web Token in
// its header, payload and signature, an Anthropic token in its first 16
// bytes, and a refresh token field's value, is found whole across the
// events, and so is a web token a t.Log wrapped onto its next line. The
// events it spans become one, still JSON, reading the test's text with the
// token replaced. The same text split between two tests' events, or before
// another action, is kept. On the base, each cut token leaked whole or kept
// the part after the cut.
func TestSpansReadATestsGoTestJSONEventsAsOneText(t *testing.T) {
	jwt := shapeSegment(`{"alg":"RS256","typ":"JWT"}`) + "." + shapeSegment(`{"sub":"planted-subject","aud":["planted"],"scp":["openid","profile","email","offline_access"]}`) + "." + shapeSegment(strings.Repeat("\x9c\x01\xfe", 40))
	header, payload := strings.Index(jwt, "."), strings.LastIndex(jwt, ".")
	anthropic := "sk-ant-api03-" + strings.Repeat("Ab3_9z-Q", 8)
	refresh := `{"refresh_token":"rt1.Zq8vLm2Np4Rs6Tu8Vw0Xy1Za3Bc5De7Fg9Hi1Jk3-_"}`
	value := strings.Index(refresh, "rt1.")
	cut := func(text string, at ...int) string {
		var stream strings.Builder
		from := 0
		for _, a := range at {
			stream.WriteString(shapeEvent("p", "TestCut", text[from:a]))
			from = a
		}
		stream.WriteString(shapeEvent("p", "TestCut", text[from:]))
		return stream.String()
	}
	for name, c := range map[string]struct{ text, want string }{
		"web token cut in its header":          {cut("x "+jwt+" y\n", 6), "x " + Mark + " y\n"},
		"web token cut in its payload":         {cut("x "+jwt+" y\n", 2+header+15), "x " + Mark + " y\n"},
		"web token cut before its dot":         {cut("x "+jwt+" y\n", 2+payload), "x " + Mark + " y\n"},
		"web token cut in its signature":       {cut("x "+jwt+" y\n", 2+payload+9), "x " + Mark + " y\n"},
		"web token cut three times":            {cut("x "+jwt+" y\n", 4, 2+header+3, 2+payload+1), "x " + Mark + " y\n"},
		"anthropic cut in its first 16":        {cut("key "+anthropic+" end\n", 4+len("sk-ant-")+5), "key " + Mark + " end\n"},
		"anthropic cut after its first 16":     {cut("key "+anthropic+" end\n", 4+40), "key " + Mark + " end\n"},
		"refresh token cut in its value":       {cut(refresh+"\n", value+7), `{"refresh_token":"` + Mark + `"}` + "\n"},
		"web token wrapped by t.Log, indented": {shapeEvent("p", "TestCut", "    x_test.go:9: token "+jwt[:header+12]+"\n") + shapeEvent("p", "TestCut", "        "+jwt[header+12:]+"\n"), "    x_test.go:9: token " + Mark + "\n"},
	} {
		stream := `{"Action":"run","Package":"p","Test":"TestCut"}` + "\n" + c.text + `{"Action":"pass","Package":"p","Test":"TestCut"}` + "\n"
		got := redact(stream)
		if out := shapeOutputs(t, got)["TestCut"]; out != c.want {
			t.Errorf("%s:\n got %q\nwant %q", name, out, c.want)
		}
	}
	for name, text := range map[string]string{
		"another test":    shapeEvent("p", "TestA", "x "+jwt[:header+12]) + shapeEvent("p", "TestB", jwt[header+12:]+" y\n"),
		"another package": shapeEvent("p", "TestA", "key "+anthropic[:12]) + shapeEvent("q", "TestA", anthropic[12:]+"\n"),
		"another action":  shapeEvent("p", "TestA", "key "+anthropic[:12]) + `{"Action":"pass","Package":"p","Test":"TestA","Output":"` + anthropic[12:] + `"}` + "\n",
	} {
		if got := redact(text); got != text {
			t.Errorf("%s: changed:\n%s", name, got)
		}
	}
	// Events that end in a newline join too, so a line of ordinary text
	// after a token on its own line is kept as the raw text's is.
	lines := shapeEvent("p", "TestA", "key "+anthropic+"\n") + shapeEvent("p", "TestA", "next_variable_name=1\n")
	if out := shapeOutputs(t, redact(lines))["TestA"]; out != "key "+Mark+"\nnext_variable_name=1\n" {
		t.Errorf("the line after a token changed: %q", out)
	}
}

// FAE-SEC-003: a token field's value is found after a kebab-case name
// (refresh-token, as a header or flag writes it), after its name and value
// aligned in columns (up to 16 spaces each side of the ':'), and after an
// HTTP "Bearer" scheme, in any case, plain or in JSON. Prose after "Bearer",
// a short value, a value with no digit, and a name inside a longer word are
// kept. On the base, each of these tokens leaked.
func TestSpansFindKebabAlignedAndBearerTokens(t *testing.T) {
	token := "rt_" + strings.Repeat("Ab9", 10)
	for name, text := range map[string]string{
		"kebab refresh-token":   "refresh-token: " + token + "\n",
		"kebab header":          "X-Access-Token: " + token + "\n",
		"kebab flag":            "--id-token=" + token + " --verbose\n",
		"five spaces":           "refresh_token:     " + token + "\n",
		"aligned both sides":    "refresh_token" + strings.Repeat(" ", 12) + ":" + strings.Repeat(" ", 16) + token + "\n",
		"Authorization: Bearer": "Authorization: Bearer " + token + "\n",
		"bearer in JSON":        `{"Authorization":"bearer ` + token + `"}`,
		"curl header":           "curl -H 'Authorization: BEARER  " + token + "' https://example.invalid\n",
		"bearer, hex token":     "Authorization: Bearer 3f2a9c0d4e5b6a7f8091a2b3c4d5e6f7\n",
	} {
		if got := redact(text); strings.Contains(got, token[3:20]) || strings.Contains(got, "3f2a9c0d4e5b") || !strings.Contains(got, Mark) {
			t.Errorf("%s: the token was kept: %q", name, got)
		}
	}
	for _, kept := range []string{
		"Bearer tokens are described in RFC 6750, section 2.1",
		"bearer authorization_header_value_for_the_test",
		"Authorization: Bearer abc123",
		"TestBearer 0123456789abcdefABCDEF",
		"bearer-token-count: 12345678901234567890",
		"valid-token: 0123456789abcdefABCDEF",
		"refresh-token-count: 12345678901234567890",
	} {
		if got := redact(kept); got != kept {
			t.Errorf("ordinary text changed: %q -> %q", kept, got)
		}
	}
}

// FAE-SEC-003: a token wrapped across a line break escaped once (a JSON or Go
// string) or twice (go test -json's view of a %q) is found as a raw break is:
// a JSON Web Token with an escaped "\n" in its payload, with "\r\n" and an
// escaped tab of indentation, and an Anthropic token whose first run the break
// cuts under 16. On the base each leaked whole.
func TestSpansFindATokenAcrossAnEscapedLineBreak(t *testing.T) {
	jwt := shapeSegment(`{"alg":"HS256"}`) + "." + shapeSegment(`{"sub":"1234567890","name":"planted"}`) + "." + shapeSegment(strings.Repeat("\x9c\x01\xfe", 20))
	header := strings.Index(jwt, ".")
	anthropic := "sk-ant-api03-" + strings.Repeat("Ab3_9z-Q", 8)
	for name, brk := range map[string]string{"escaped once": `\n`, "CRLF escaped once": `\r\n`, "escaped twice": `\\n`, "escaped tab": `\n\t\t`, "twice-escaped tab": `\\n\\t`} {
		for what, text := range map[string]string{
			"web token payload": `"` + jwt[:header+10] + brk + jwt[header+10:] + `"`,
			"anthropic":         `"key ` + anthropic[:11] + brk + anthropic[11:] + `"`,
		} {
			if got := redact(text); strings.Contains(got, jwt[len(jwt)-12:]) || strings.Contains(got, anthropic[len(anthropic)-12:]) || strings.Contains(got, jwt[:12]) || strings.Contains(got, anthropic[:10]) {
				t.Errorf("%s, %s: %q", what, name, got)
			}
		}
	}
	// A line after an escaped break that holds no token is kept.
	text := fmt.Sprintf(`"header %s\nnext line"`, shapeSegment(`{"alg":"RS256","typ":"JWT"}`))
	if got := redact(text); got != text {
		t.Errorf("ordinary text changed: %q", got)
	}
}
