package secretshape

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// redact replaces Spans in text, as the daemon does.
func redact(text string) string {
	b := []byte(text)
	var out []byte
	at := 0
	for _, sp := range Spans(b) {
		out = append(append(out, b[at:sp[0]]...), Mark...)
		at = sp[1]
	}
	return string(append(out, b[at:]...))
}

// shapeSegment is a JSON Web Token segment.
func shapeSegment(v string) string { return base64.RawURLEncoding.EncodeToString([]byte(v)) }

// FAE-SEC-003: an OAuth token field's value is found by its field, whatever
// its shape: a Codex ChatGPT login's (synthetic) refresh token, whose format
// is not published, printed whole as auth.json, as go test -json shows it
// (quotes escaped once and twice), in a form body, as YAML, in camel case and
// in upper case. Another field, a short or empty value, null, and the field's
// name as a value are kept. On the base, nothing found the refresh token.
func TestSpansFindTokenFieldValues(t *testing.T) {
	refresh := "rt1.Zq8vLm2Np4Rs6Tu8Vw0Xy1Za3Bc5De7Fg9Hi1Jk3-_~+/="
	login, err := json.MarshalIndent(map[string]any{
		"OPENAI_API_KEY": nil,
		"tokens":         map[string]string{"refresh_token": refresh, "account_id": "planted-account-0123456789"},
		"last_refresh":   "2026-10-28T00:00:00Z",
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	escaped, _ := json.Marshal(string(login))
	twice, _ := json.Marshal(string(escaped))
	for name, text := range map[string]string{
		"auth.json":       string(login),
		"escaped once":    string(escaped),
		"escaped twice":   string(twice),
		"form body":       "grant_type=refresh_token&refresh_token=" + refresh + "&client_id=planted-client",
		"YAML":            "refresh_token: " + refresh + "\n",
		"camel case":      `{"refreshToken":"` + refresh + `"}`,
		"upper case":      "REFRESH_TOKEN=" + refresh + "\n",
		"access token":    "access_token=" + refresh + "\n",
		"id token, quote": "id_token = '" + refresh + "'\n",
	} {
		got := redact(text)
		if strings.Contains(got, refresh[:12]) || strings.Contains(got, refresh[len(refresh)-12:]) || !strings.Contains(got, Mark) {
			t.Errorf("%s: the token was kept:\n%s", name, got)
		}
		if strings.Contains(text, "planted-account") && !strings.Contains(got, "planted-account-0123456789") {
			t.Errorf("%s: another field was redacted:\n%s", name, got)
		}
		if strings.Contains(text, "client_id") && !strings.HasSuffix(got, "&client_id=planted-client") {
			t.Errorf("%s: the next field was redacted: %s", name, got)
		}
	}
	for _, kept := range []string{
		`"refresh_token": null`, `"refresh_token": ""`, "refresh_token=short-value", "grant_type=refresh_token&scope=openid",
		"a refresh_token is kept by the CLI", "prefetch_token_count=1234567890123456789",
	} {
		if got := redact(kept); got != kept {
			t.Errorf("ordinary text changed: %q -> %q", kept, got)
		}
	}
}

// FAE-SEC-003: an OpenAI key without the T3BlbkFJ marker is found where no
// letter, digit, '-' or '_' comes before its "sk-" and its run holds 40 or
// more bytes with an upper-case letter, a lower-case letter and a digit. On
// the base, only a key with the marker was. A run inside a word, one in one
// case, and short runs are kept.
func TestSpansFindAnOpenAIKeyWithoutItsMarker(t *testing.T) {
	for _, key := range []string{
		"sk-proj-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789abcdEFGHijklmnop",
		"sk-svcacct-" + strings.Repeat("Zq7x_W2-", 6),
		"sk-" + strings.Repeat("A1b2C3d4E5", 5),
	} {
		for _, text := range []string{"OPENAI_API_KEY=" + key + "\n", `{"api_key":"` + key + `"}`, key} {
			if got := redact(text); strings.Contains(got, key[:16]) || strings.Contains(got, key[len(key)-10:]) {
				t.Errorf("a key was kept: %q", got)
			}
		}
	}
	for _, kept := range []string{
		"fae/task/task-0123456789abcdef0123456789abcdef0123456789abcdef",
		"risk-free disk-0 sk-1234 ask-me sk-learn",
		"sk-" + strings.Repeat("0123456789abcdef", 4),
		"sk-some-very-long-lowercase-kebab-case-identifier-name-here",
		"mask-" + strings.Repeat("A1b2C3d4E5", 5),
		"xsk-" + strings.Repeat("A1b2C3d4E5", 5),
	} {
		if got := redact(kept); got != kept {
			t.Errorf("ordinary text changed: %q -> %q", kept, got)
		}
	}
}

// FAE-SEC-003: a token a line break cuts is found across it where the part
// before the break is too short to be found alone: an Anthropic token whose
// run the break cuts under 16 bytes, and a JSON Web Token cut inside its
// header or payload, or before a dot. On the base each leaked whole. A token
// whose part before the break has the shape keeps the part after it, as
// documented.
func TestSpansFindATokenWrappedAcrossLines(t *testing.T) {
	anthropic := "sk-ant-api03-" + strings.Repeat("Ab3_9z-Q", 8)
	jwt := shapeSegment(`{"alg":"RS256","typ":"JWT"}`) + "." + shapeSegment(`{"sub":"planted-subject","aud":["planted"]}`) + "." + shapeSegment(strings.Repeat("\x9c\x01\xfe", 40))
	header, payload := strings.Index(jwt, "."), strings.LastIndex(jwt, ".")
	for name, text := range map[string]string{
		"anthropic after 5":         anthropic[:12] + "\n" + anthropic[12:],
		"anthropic, indented":       anthropic[:15] + "\n        " + anthropic[15:],
		"anthropic, CRLF":           anthropic[:10] + "\r\n" + anthropic[10:],
		"web token inside header":   jwt[:header-6] + "\n" + jwt[header-6:],
		"web token inside payload":  jwt[:header+12] + "\n" + jwt[header+12:],
		"web token before a dot":    jwt[:payload] + "\n  " + jwt[payload:],
		"web token header and body": jwt[:8] + "\n" + jwt[8:header+20] + "\n" + jwt[header+20:],
	} {
		if got := redact("token " + text + "\nnext line"); got != "token "+Mark+"\nnext line" {
			t.Errorf("%s: %q", name, got)
		}
	}
	// The part after a break is kept once the part before it has the shape.
	if got := redact(anthropic[:40] + "\n" + anthropic[40:]); got != Mark+"\n"+anthropic[40:] {
		t.Errorf("a wrapped token whose head has the shape: %q", got)
	}
	if got := redact(jwt[:payload+20] + "\n" + jwt[payload+20:]); got != Mark+"\n"+jwt[payload+20:] {
		t.Errorf("a web token wrapped in its signature: %q", got)
	}
}

// FAE-SEC-003: the provider log writer replaces credentials and given values
// wherever the writes split them, one byte per write and across a flood with
// no line break, holds an unfinished line until Flush, and keeps every other
// byte. A failed write is returned.
func TestWriterRedactsAcrossWrites(t *testing.T) {
	value := "planted-provider-env-value-0123"
	token := "sk-ant-oat01-" + strings.Repeat("Ab3_9z-", 8)
	text := `{"type":"error","message":"key ` + token + ` env ` + value + `"}` + "\n" + `{"type":"done"}` + "\npartial " + value
	want := `{"type":"error","message":"key ` + Mark + ` env ` + Mark + `"}` + "\n" + `{"type":"done"}` + "\npartial " + Mark
	var out bytes.Buffer
	w := NewWriter(&out, [][]byte{[]byte(value), nil})
	for _, c := range []byte(text) {
		if n, err := w.Write([]byte{c}); n != 1 || err != nil {
			t.Fatal(n, err)
		}
	}
	if strings.Contains(out.String(), "partial") {
		t.Fatalf("an unfinished line was written before Flush: %q", out.String())
	}
	if err := w.Flush(); err != nil || out.String() != want {
		t.Fatalf("%v\n got %q\nwant %q", err, out.String(), want)
	}
	for _, at := range []int{writerFlood - Max, writerFlood - len(token)/2, writerFlood, writerFlood + Max - 3} {
		flood := strings.Repeat("x", at) + " " + token + " " + strings.Repeat("y", writerFlood)
		out.Reset()
		w = NewWriter(&out, nil)
		for rest := flood; len(rest) > 0; {
			n := min(len(rest), 32<<10)
			if _, err := w.Write([]byte(rest[:n])); err != nil {
				t.Fatal(err)
			}
			rest = rest[n:]
		}
		if err := w.Flush(); err != nil {
			t.Fatal(err)
		}
		if got := out.String(); strings.Contains(got, token[:20]) || strings.Contains(got, token[len(token)-12:]) || len(got) != len(flood)-len(token)+len(Mark) {
			t.Fatalf("a token at %d in a flood was kept or bytes were lost (%d of %d)", at, len(got), len(flood)-len(token)+len(Mark))
		}
	}
	broken := NewWriter(errorWriter{}, nil)
	if _, err := broken.Write([]byte("line\n")); err == nil {
		t.Fatal("a failed write was not returned")
	}
	if err := broken.Flush(); err == nil {
		t.Fatal("a failed write was forgotten")
	}
}

type errorWriter struct{}

func (errorWriter) Write([]byte) (int, error) { return 0, errPlantedWrite }

var errPlantedWrite = &plantedError{}

type plantedError struct{}

func (*plantedError) Error() string { return "planted write failure" }

// CredentialVariable names a credential by a part of its name, except the
// names that carry none; EnvironmentValues keeps such values of 8 bytes or
// more.
func TestCredentialVariableAndEnvironmentValues(t *testing.T) {
	for name, want := range map[string]bool{"GIT_AUTHOR_NAME": false, "git_author_email": false, "SSH_AUTH_SOCK": false, "SUDO_ASKPASS": false, "HTTP_AUTHORIZATION": true, "NPM_AUTH": true, "GIT_PASSWORD": true, "SSH_KEY": true, "PATH": false} {
		if CredentialVariable(name) != want {
			t.Errorf("CredentialVariable(%s) = %v", name, !want)
		}
	}
	values := EnvironmentValues([]string{"OPENAI_API_KEY=planted-openai-value", "GH_TOKEN=short", "PATH=/usr/bin", "GIT_AUTHOR_NAME=Planted Author", "malformed"})
	if len(values) != 1 || string(values[0]) != "planted-openai-value" {
		t.Fatalf("%q", values)
	}
}
