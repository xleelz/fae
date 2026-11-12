package daemon

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

// redactionMixedCase writes every other letter of text in upper case.
func redactionMixedCase(text string) string {
	b := []byte(text)
	upper := false
	for i, c := range b {
		if 'a' <= c && c <= 'z' {
			if upper {
				b[i] = c - 'a' + 'A'
			}
			upper = !upper
		}
	}
	return string(b)
}

// redactionSpacedHex is Go's "% x" of b.
func redactionSpacedHex(b []byte) string { return fmt.Sprintf("% x", b) }

// FAE-SEC-003: every secret the daemon holds is found in the same forms,
// whatever its source: a file credential, a value from the daemon's own
// environment and one from the live configuration's. Each is found as hex in
// mixed case and with a space between bytes (Go's "% x"), as base64 and
// query-escaped base64, and as base64 inside a larger value at any of the
// three alignments within base64's groups (`Authorization: Basic` of
// "user:"+key), where the characters that carry any of its bits are replaced
// and its neighbours' are kept. On the base, configured and environment
// values were matched only raw, and no secret at an offset in base64 or in
// mixed-case or spaced hex.
func TestFailureSecretsFindEverySecretInEveryForm(t *testing.T) {
	root := t.TempDir()
	d := &Daemon{layout: Layout{Credential: filepath.Join(root, "credential"), Run: root}}
	operator := []byte("\x00\x8f\"\\/+ =&<>\xc3\xa9\x7fA-0123456789abcdef")
	if err := os.WriteFile(d.layout.Credential, operator, 0600); err != nil {
		t.Fatal(err)
	}
	writeRedactionCredential(t, filepath.Join(root, "plane-credential"), 'p')
	stamp := time.Now().UnixNano()
	daemonKey := fmt.Sprintf("planted-daemon-key-%d", stamp)
	liveValue := fmt.Sprintf("planted live&token<%d>", stamp)
	t.Setenv("FAE_PLANTED_FORMS_API_KEY", daemonKey)
	secrets, err := d.failureSecrets(map[string]string{"PLANTED_FORMS_TOKEN": liveValue})
	if err != nil {
		t.Fatal(err)
	}
	for name, secret := range map[string][]byte{"operator credential": operator, "daemon environment key": []byte(daemonKey), "live configuration value": []byte(liveValue)} {
		h := hex.EncodeToString(secret)
		forms := map[string]string{
			"hex":                    h,
			"upper-case hex":         strings.ToUpper(h),
			"mixed-case hex":         redactionMixedCase(h),
			"spaced hex":             redactionSpacedHex(secret),
			"mixed-case spaced hex":  redactionMixedCase(redactionSpacedHex(secret)),
			"base64":                 base64.StdEncoding.EncodeToString(secret),
			"URL-safe base64":        base64.RawURLEncoding.EncodeToString(secret),
			"query-escaped base64":   url.QueryEscape(base64.StdEncoding.EncodeToString(secret)),
			"base64 in a JSON value": redactionJSON(base64.StdEncoding.EncodeToString(secret), true),
		}
		for form, text := range forms {
			if got := secrets.redactString("a " + text + " b"); got != "a [redacted] b" {
				t.Errorf("%s as %s %q: %q", name, form, text, got)
			}
		}
		n := len(secret)
		for offset := 0; offset < 6; offset++ {
			for suffix := 1; suffix <= 3; suffix++ {
				value := append(append([]byte(strings.Repeat("u", offset)), secret...), strings.Repeat("!", suffix)...)
				for alphabet, e := range map[string]*base64.Encoding{"standard": base64.RawStdEncoding, "URL-safe": base64.RawURLEncoding} {
					text := e.EncodeToString(value)
					// The characters wholly of the prefix, the mark, and those wholly
					// of the suffix.
					want := "Authorization: Basic " + text[:8*offset/6] + "[redacted]" + text[(8*(offset+n)+5)/6:] + "\n"
					if got := secrets.redactString("Authorization: Basic " + text + "\n"); got != want {
						t.Errorf("%s in %s base64 at offset %d, %d bytes after:\n got %q\nwant %q", name, alphabet, offset, suffix, got, want)
					}
				}
			}
		}
	}
}

// FAE-SEC-003: a provider login the daemon never reads is redacted by its
// tokens' shapes. A Codex login's (fake) auth.json, printed whole, keeps
// neither its OpenAI API key nor its ID and access tokens (JSON Web
// Tokens); a legacy OpenAI key and an Anthropic token are replaced too, with
// no value known and among the daemon's own secrets alike, and a token split
// across reads is replaced whole. Ordinary text that starts like a shape is
// kept: a bare "sk-" run with no OpenAI marker (a task id), a JWT header
// with no token after it, short "sk-" words. A token wrapped across lines
// keeps the part after the break, as daemon/README.md documents. On the base,
// only the Anthropic token was replaced.
func TestShapedProviderCredentialsAreRedacted(t *testing.T) {
	segment := func(v string) string { return base64.RawURLEncoding.EncodeToString([]byte(v)) }
	idToken := segment(`{"alg":"RS256","kid":"planted","typ":"JWT"}`) + "." + segment(`{"email":"planted@example.invalid","sub":"planted-subject"}`) + "." + segment(strings.Repeat("\x9c\x01\xfe", 86))
	accessToken := segment(`{"alg":"RS256","typ":"JWT"}`) + "." + segment(`{"aud":["planted"],"exp":1793232000,"scp":["openid"]}`) + "." + segment(strings.Repeat("\x01\x02\x03", 86))
	projectKey := "sk-proj-" + strings.Repeat("Ab3_9z-Q", 9) + "T3BlbkFJ" + strings.Repeat("Zq7-x_W2", 9)
	legacyKey := "sk-" + "A1b2C3d4E5f6G7h8I9j0" + "T3BlbkFJ" + "K1l2M3n4O5p6Q7r8S9t0"
	anthropic := "sk-ant-oat01-" + strings.Repeat("Ab3_9z-", 8)
	login, err := json.MarshalIndent(map[string]any{
		"OPENAI_API_KEY": projectKey,
		"tokens":         map[string]string{"id_token": idToken, "access_token": accessToken, "account_id": "planted-account"},
		"last_refresh":   "2026-10-28T00:00:00Z",
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	d := &Daemon{layout: Layout{Credential: filepath.Join(root, "credential"), Run: root}}
	writeRedactionCredential(t, d.layout.Credential, 'o')
	writeRedactionCredential(t, filepath.Join(root, "plane-credential"), 'p')
	held, err := d.failureSecrets(nil)
	if err != nil {
		t.Fatal(err)
	}
	tokens := []string{idToken, accessToken, projectKey, legacyKey, anthropic}
	text := string(login) + "\nAuthorization: Bearer " + accessToken + "\nOPENAI_API_KEY=" + legacyKey + " key=" + url.QueryEscape(anthropic) + "\n"
	for name, s := range map[string]secretValues{"no value known": newSecretValues(), "the daemon's secrets": held} {
		got := s.redactString(text)
		for _, token := range tokens {
			if strings.Contains(got, token) || strings.Contains(got, token[len(token)-12:]) {
				t.Errorf("%s: a token was kept: %q in\n%s", name, token[:12], got)
			}
		}
		if !strings.Contains(got, `"account_id": "planted-account"`) || !strings.Contains(got, `"id_token": "[redacted]"`) || !strings.Contains(got, "Bearer [redacted]\n") || !strings.Contains(got, "OPENAI_API_KEY=[redacted] key=[redacted]\n") {
			t.Errorf("%s:\n%s", name, got)
		}
		var out bytes.Buffer
		if err = s.copyRedacted(&out, iotest.OneByteReader(strings.NewReader(text))); err != nil || out.String() != got {
			t.Errorf("%s: one byte per read:\n%s\n%v", name, out.String(), err)
		}
		for _, kept := range []string{
			"fae/task/task-0123456789abcdef0123456789abcdef0123456789abcdef",
			"header " + segment(`{"alg":"RS256","typ":"JWT"}`) + " alone, and " + segment(`{"alg":"none"}`) + "." + segment(`{"sub":"x"}`) + " without a signature dot",
			"risk-free disk-0 sk-1234 ask-me",
		} {
			if got := s.redactString(kept); got != kept {
				t.Errorf("%s: ordinary text changed: %q -> %q", name, kept, got)
			}
		}
		wrapped := anthropic[:30] + "\n" + anthropic[30:]
		if got := s.redactString(wrapped); got != "[redacted]\n"+anthropic[30:] {
			t.Errorf("%s: a wrapped token, which daemon/README.md says keeps its tail: %q", name, got)
		}
	}
}

// FAE-SEC-003, FAE-CTL-030: the daemon's environment names that carry no
// secret though a credential part matches them (a commit's author, the SSH
// agent's socket, the X authority file, a password prompt program) are not
// redacted, from the daemon's environment or the live configuration's, so
// their values stay readable in excerpts and in the landing gate output gate
// results are parsed from. A credential-named value beside them still is. On
// the base, every one of them was redacted.
func TestFailureSecretsKeepNamesThatCarryNoSecret(t *testing.T) {
	root := t.TempDir()
	d := &Daemon{layout: Layout{Credential: filepath.Join(root, "credential"), Run: root}}
	writeRedactionCredential(t, d.layout.Credential, 'o')
	writeRedactionCredential(t, filepath.Join(root, "plane-credential"), 'p')
	kept := map[string]string{
		"GIT_AUTHOR_NAME":  "Planted Author Name",
		"GIT_AUTHOR_EMAIL": "planted.author@example.invalid",
		"SSH_AUTH_SOCK":    "/tmp/planted-ssh-agent.sock",
		"XAUTHORITY":       "/tmp/planted-xauthority",
		"SSH_ASKPASS":      "/usr/bin/planted-askpass",
	}
	for name, value := range kept {
		t.Setenv(name, value)
	}
	token := fmt.Sprintf("planted-auth-token-%d", time.Now().UnixNano())
	t.Setenv("FAE_PLANTED_AUTH_TOKEN", token)
	secrets, err := d.failureSecrets(map[string]string{"GIT_AUTHOR_DATE": "2026-10-28T00:00:00Z", "PLANTED_AUTHORIZATION": "planted-authorization-value"})
	if err != nil {
		t.Fatal(err)
	}
	var text, want strings.Builder
	for name, value := range kept {
		fmt.Fprintf(&text, "%s=%s\n", name, value)
		fmt.Fprintf(&want, "%s=%s\n", name, value)
	}
	text.WriteString("date 2026-10-28T00:00:00Z token " + token + " header planted-authorization-value\n")
	want.WriteString("date 2026-10-28T00:00:00Z token [redacted] header [redacted]\n")
	if got := secrets.redactString(text.String()); got != want.String() {
		t.Fatalf("got\n%s\nwant\n%s", got, want.String())
	}
	for name, want := range map[string]bool{"GIT_AUTHOR_NAME": false, "git_author_email": false, "SSH_AUTH_SOCK": false, "SUDO_ASKPASS": false, "HTTP_AUTHORIZATION": true, "NPM_AUTH": true, "GIT_PASSWORD": true, "SSH_KEY": true} {
		if credentialVariable(name) != want {
			t.Errorf("credentialVariable(%s) = %v", name, !want)
		}
	}
}

// A base64 core takes the character before it, which a stream must not have
// written before the core arrives: with the core starting at and around each
// point copyRedacted writes up to, its output equals redact over the whole
// stream and keeps no character that carries any of the secret's bits.
func TestSecretValuesCopyRedactedKeepsABase64CoresEdges(t *testing.T) {
	root := t.TempDir()
	d := &Daemon{layout: Layout{Credential: filepath.Join(root, "credential"), Run: root}}
	writeRedactionCredential(t, d.layout.Credential, 'o')
	writeRedactionCredential(t, filepath.Join(root, "plane-credential"), 'p')
	secret := []byte("planted&secret/value-0123456789")
	s, err := d.failureSecrets(map[string]string{"PLANTED_CORE_TOKEN": string(secret)})
	if err != nil {
		t.Fatal(err)
	}
	const chunk = 256 << 10
	hold := s.hold()
	for offset := 1; offset < 3; offset++ {
		value := append(append([]byte(strings.Repeat("u", offset)), secret...), '!')
		text := base64.RawStdEncoding.EncodeToString(value)
		lead := 8 * offset / 6 // the shared character before the core
		var ats []int
		// copyRedacted writes up to hold bytes before the end of each
		// k*chunk it has read, k >= 2: put the core just there, so that its
		// shared character falls on either side of a write.
		for k := 2; k <= 3; k++ {
			for d := -2; d <= 2; d++ {
				ats = append(ats, k*chunk-hold-lead-1+d)
			}
		}
		for _, at := range ats {
			stream := append(append(bytes.Repeat([]byte{' '}, at), text...), bytes.Repeat([]byte{' '}, chunk)...)
			var out bytes.Buffer
			if err := s.copyRedacted(&out, bytes.NewReader(stream)); err != nil {
				t.Fatal(err)
			}
			want := text[:lead] + "[redacted]" + text[(8*(offset+len(secret))+5)/6:]
			if !bytes.Equal(out.Bytes(), s.redact(stream)) || !bytes.Contains(out.Bytes(), []byte(want)) {
				t.Fatalf("offset %d at %d: %q", offset, at, bytes.TrimSpace(out.Bytes()))
			}
		}
	}
}
