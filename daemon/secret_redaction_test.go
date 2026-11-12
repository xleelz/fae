package daemon

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/format"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/xleelz/fae/commandshim"
	"github.com/xleelz/fae/controller"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/effects"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/hostguard"
	"github.com/xleelz/fae/workflow"
)

// redactionJSON is the inside of v's JSON string, with or without Go's HTML
// escapes.
func redactionJSON(v string, html bool) string {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(html)
	_ = e.Encode(v)
	out := strings.TrimSpace(b.String())
	return out[1 : len(out)-1]
}

// redactionASCIIJSON is the inside of v's JSON string as an ASCII-only
// encoder writes it: every non-ASCII character as \uXXXX, a surrogate pair
// beyond the BMP.
func redactionASCIIJSON(v string) string {
	var b strings.Builder
	for _, r := range redactionJSON(v, true) {
		if r < 0x80 {
			b.WriteRune(r)
			continue
		}
		for _, u := range utf16.Encode([]rune{r}) {
			fmt.Fprintf(&b, `\u%04x`, u)
		}
	}
	return b.String()
}

// redactionQuoted is the inside of Go's %q of v.
func redactionQuoted(v string) string {
	q := strconv.Quote(v)
	return q[1 : len(q)-1]
}

// redactionEveryByteEscaped percent-encodes every byte of v, hex in lower
// case.
func redactionEveryByteEscaped(v string) string {
	var b strings.Builder
	for _, c := range []byte(v) {
		fmt.Fprintf(&b, "%%%02x", c)
	}
	return b.String()
}

// FAE-SEC-003: a secret value is redacted in the encodings captured output
// carries it in: percent-encoding (query and path escaping, either hex case,
// every byte escaped, + for a space, base64 inside a query), JSON (with and
// without HTML escapes, escaped slashes, ASCII only with surrogate pairs),
// Go's %q and %+q (the raw bytes of a binary credential as \xNN), and go test
// -json's view of a %q (escaped twice). An Anthropic-shaped credential is
// redacted with no value known. Ordinary text with %, + and backslashes is
// left alone. A value split across lines is redacted whole (wave 11; before,
// it was kept whole).
func TestSecretValuesRedactEscapedForms(t *testing.T) {
	credential := []byte("\x00\x8f\"\\/+ =&<>\xc3\xa9\x7fA-0123456789abcdef")
	configured := "pl@nted key/v&lue<x>\"q\\\u00e9\U0001F600+%"
	if len(credential) != 32 {
		t.Fatal("fixture credential is not 32 bytes", len(credential))
	}
	s := newSecretValues(append(credentialForms(credential), []byte(configured))...)
	forms := map[string]string{
		"query-escaped":                     url.QueryEscape(configured),
		"path-escaped":                      url.PathEscape(configured),
		"query-escaped, lower-case hex":     strings.ToLower(url.QueryEscape(configured)),
		"every byte percent-encoded":        redactionEveryByteEscaped(configured),
		"base64 in a query":                 url.QueryEscape(base64.StdEncoding.EncodeToString(credential)),
		"JSON":                              redactionJSON(configured, true),
		"JSON without HTML escapes":         redactionJSON(configured, false),
		"JSON with escaped slashes":         strings.ReplaceAll(redactionJSON(configured, false), "/", `\/`),
		"JSON, ASCII only":                  redactionASCIIJSON(configured),
		"Go %q":                             redactionQuoted(configured),
		"Go %+q":                            strings.Trim(strconv.QuoteToASCII(configured), `"`),
		"Go %q of the raw credential":       redactionQuoted(string(credential)),
		"go test -json of a %q":             redactionJSON(redactionQuoted(configured), true),
		"go test -json of the credential":   redactionJSON(redactionQuoted(string(credential)), true),
		"upper-case hex of the credential":  strings.ToUpper(hex.EncodeToString(credential)),
		"URL-safe base64 of the credential": base64.RawURLEncoding.EncodeToString(credential),
	}
	for name, form := range forms {
		if form == configured || form == string(credential) {
			t.Fatalf("%s is not an encoding", name)
		}
		if got := s.redactString("a " + form + " b"); got != "a [redacted] b" {
			t.Errorf("%s %q: %q", name, form, got)
		}
		if got := string(s.redact([]byte("a " + form + " b"))); got != "a [redacted] b" {
			t.Errorf("%s %q (bytes): %q", name, form, got)
		}
	}
	token := "sk-ant-oat01-" + strings.Repeat("Ab3_9z-", 8)
	if got := newSecretValues().redactString("key=" + token + "\n"); got != "key=[redacted]\n" {
		t.Errorf("an Anthropic-shaped credential was kept: %q", got)
	}
	for _, kept := range []string{
		"keys look like sk-ant-api03-…",
		`coverage: 80.0% of statements\n{"Output":"a\\b \u0026 c+d"}%zz \x`,
		"go test -run '^TestX$' ./... ok 0.1s",
	} {
		if got := s.redactString(kept); got != kept {
			t.Errorf("ordinary text changed: %q -> %q", kept, got)
		}
	}
	split := configured[:5] + "\n" + configured[5:]
	if got := s.redactString(split); got != "[redacted]" {
		t.Errorf("a value split across lines was not redacted whole: %q", got)
	}
}

// FAE-SEC-003: a secret written with 3-digit Go/C octal byte escapes, \NNN
// or JSON-escaped once more as \\NNN, every byte or only some, is redacted,
// in a string and streamed through tiny reads. An escape whose first digit
// is over 3, that is short, or that holds an 8 or 9 is not decoded.
func TestSecretRedactionOctalByteEscapes(t *testing.T) {
	for _, lead := range []int{1, 2} {
		u, ok := secretEscapeAt([]byte("163"), 0, lead)
		if want := (secretUnit{b: [utf8.UTFMax]byte{0163}, size: 1, n: lead + 3}); !ok || u != want {
			t.Errorf("octal-escaped secret not redacted: secretEscapeAt(%q, lead %d) = %+v, %v", "163", lead, u, ok)
		}
		for _, text := range []string{"463", "16", "183"} {
			if u, ok := secretEscapeAt([]byte(text), 0, lead); ok {
				t.Errorf("secretEscapeAt(%q, lead %d) decoded %+v", text, lead, u)
			}
		}
	}
	secret := "octal-Planted_secret.value9"
	s := newSecretValues([]byte(secret))
	octal := func(v, lead string) string {
		var b strings.Builder
		for _, c := range []byte(v) {
			b.WriteString(lead + fmt.Sprintf("%03o", c))
		}
		return b.String()
	}
	var mixed strings.Builder
	for i, c := range []byte(secret) {
		if i%2 == 0 {
			fmt.Fprintf(&mixed, `\%03o`, c)
		} else {
			mixed.WriteByte(c)
		}
	}
	forms := []struct{ name, text string }{
		{`every byte as \NNN`, octal(secret, `\`)},
		{`every byte as \\NNN`, octal(secret, `\\`)},
		{`raw bytes mixed with \NNN`, mixed.String()},
	}
	for _, f := range forms {
		if got, want := s.redactString("a "+f.text+" b"), "a "+redactedMark+" b"; got != want {
			t.Errorf("octal-escaped secret not redacted (%s): got %q want %q", f.name, got, want)
		}
	}
	for _, f := range forms[:2] {
		var out bytes.Buffer
		if err := s.copyRedacted(&out, iotest.OneByteReader(strings.NewReader("a "+f.text+" b"))); err != nil {
			t.Fatalf("%s: %v", f.name, err)
		}
		got := out.String()
		if !strings.Contains(got, redactedMark) || strings.Contains(got, f.text) || strings.Contains(got, secret) {
			t.Errorf("octal-escaped secret not redacted in stream (%s): got %q", f.name, got)
		}
	}
}

// writeRedactionCredential writes a 32-byte owner credential.
func writeRedactionCredential(t *testing.T, path string, fill byte) []byte {
	t.Helper()
	b := bytes.Repeat([]byte{fill}, 32)
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	return b
}

// FAE-SEC-003: the secret set holds the values of the daemon's own
// credential-named environment variables, which no run is given but any run
// can read through /proc, beside the live configuration's; a short value, or
// one under another name, is not a secret.
func TestFailureSecretsCoverTheDaemonEnvironment(t *testing.T) {
	root := t.TempDir()
	d := &Daemon{layout: Layout{Credential: filepath.Join(root, "credential"), Run: root}}
	operator := writeRedactionCredential(t, d.layout.Credential, 'o')
	writeRedactionCredential(t, filepath.Join(root, "plane-credential"), 'p')
	daemonKey := fmt.Sprintf("planted-daemon-key-%d", time.Now().UnixNano())
	t.Setenv("FAE_PLANTED_PROVIDER_API_KEY", daemonKey)
	t.Setenv("FAE_PLANTED_SHORT_TOKEN", "short")
	t.Setenv("FAE_PLANTED_SETTING", "ordinary-setting-value")
	secrets, err := d.failureSecrets(map[string]string{"PLANTED_LIVE_SECRET": "planted-live-secret-value"})
	if err != nil {
		t.Fatal(err)
	}
	text := "daemon " + daemonKey + " live planted-live-secret-value operator " + hex.EncodeToString(operator) + " short ordinary-setting-value"
	if got, want := secrets.redactString(text), "daemon [redacted] live [redacted] operator [redacted] short ordinary-setting-value"; got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

// FAE-SEC-003: an account-mode run's nested calls are stored with every
// secret the daemon holds replaced in their arguments and directories: the
// operator credential's hex in a git header, a provider key from the daemon's
// own environment percent-encoded in a URL and named in a directory, an
// Anthropic-shaped token, and the start of the plane credential's hex that
// the shim's cut left at the end of an argument. The record keeps the run's
// command and its key, and the calls' other arguments.
func TestNestedCommandRecordsRedactSecrets(t *testing.T) {
	cfg, ctx := fixture(t)
	daemonKey := fmt.Sprintf("planted+nested/key=%d", time.Now().UnixNano())
	t.Setenv("FAE_PLANTED_NESTED_API_KEY", daemonKey)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	operator, err := os.ReadFile(d.layout.Credential)
	if err != nil {
		t.Fatal(err)
	}
	plane, err := os.ReadFile(filepath.Join(d.layout.Run, "plane-credential"))
	if err != nil {
		t.Fatal(err)
	}
	token := "sk-ant-api03-" + strings.Repeat("Zq7_", 10)
	cutPrefix := hex.EncodeToString(plane)[:8]
	secrets := map[string]string{
		"operator credential (hex)":    hex.EncodeToString(operator),
		"daemon key":                   daemonKey,
		"daemon key (query-escaped)":   url.QueryEscape(daemonKey),
		"Anthropic token":              token,
		"plane credential's cut start": cutPrefix,
	}
	started := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	command := []string{"go", "test", "-json", "./..."}
	run := effects.RunEvidence{OutputDigest: strings.Repeat("1", 64), Tree: "tree", Command: command, Started: started, NestedCount: 3, Nested: []commandshim.NestedCall{
		{Argv: []string{"git", "-c", "http.extraHeader=Authorization: Bearer " + hex.EncodeToString(operator), "push"}, Dir: "/work/" + daemonKey, At: started},
		{Argv: []string{"git", "fetch", "https://example.invalid/?key=" + url.QueryEscape(daemonKey), token}, Dir: "/work", At: started.Add(time.Second)},
		{Argv: []string{"git", "commit", "-m", strings.Repeat("-", 240) + cutPrefix}, Dir: "/work", At: started.Add(2 * time.Second), Truncated: true},
	}}
	if err = d.recordNestedCommands(nestedTestContext(ctx, "task-secret", "dispatch-secret"), run); err != nil {
		t.Fatal(err)
	}
	key := nestedCommandsKey("dispatch-secret", command, started)
	raw, err := d.store.GetMarker(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	var rec nestedRecordView
	if err = json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	var stored []string
	for _, c := range rec.Calls {
		stored = append(append(stored, c.Argv...), c.Dir)
	}
	for name, secret := range secrets {
		if bytes.Contains(raw, []byte(secret)) || strings.Contains(strings.Join(stored, "\x00"), secret) {
			t.Errorf("the nested-commands record carries the %s:\n%s", name, raw)
		}
	}
	if strings.Join(rec.Command, " ") != strings.Join(command, " ") || rec.Count != 3 || len(rec.Calls) != 3 {
		t.Fatalf("record lost its command or calls: %s", raw)
	}
	if got := rec.Calls[0].Argv; len(got) != 4 || got[0] != "git" || got[2] != "http.extraHeader=Authorization: Bearer [redacted]" || got[3] != "push" || rec.Calls[0].Dir != "/work/[redacted]" {
		t.Errorf("first call: %q in %q", got, rec.Calls[0].Dir)
	}
	if got := rec.Calls[1].Argv; len(got) != 4 || got[2] != "https://example.invalid/?key=[redacted]" || got[3] != "[redacted]" {
		t.Errorf("second call: %q", got)
	}
	if got := rec.Calls[2].Argv; len(got) != 4 || got[3] != strings.Repeat("-", 240)+"[redacted]" || !rec.Calls[2].Truncated {
		t.Errorf("cut call: %q", got)
	}
}

// FAE-SEC-003: when the secrets cannot be read (here the principal
// configuration became unreadable after start), a failed run's excerpt is
// withheld with the reason, both when it is stored and when a brief would
// carry it, and an account-mode run's nested calls are stored without their
// arguments and directories, with the reason. None of them carries the
// secret the output held.
func TestUnreadableSecretsWithholdExcerptsAndNestedCalls(t *testing.T) {
	cfg, ctx := fixture(t)
	configPath := filepath.Join(filepath.Dir(cfg.Root), "principals.json")
	principalPath := filepath.Join(filepath.Dir(cfg.Root), "controller-token")
	raw, _ := json.Marshal([]PrincipalCredential{{CredentialFile: principalPath, Actor: "controller"}})
	if err := os.WriteFile(configPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	cfg.PrincipalConfig = configPath
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	operator, err := os.ReadFile(d.layout.Credential)
	if err != nil {
		t.Fatal(err)
	}
	secret := hex.EncodeToString(operator)
	if err = os.Remove(configPath); err != nil {
		t.Fatal(err)
	}
	x := &liveExecutor{daemon: d}
	doc := defaults.Workflow()
	session := "t/h/5/session/0"
	dispatch := engine.Dispatch{ID: "t/h/5", TaskID: "t", Stage: "run-tests", Config: doc.Stages["run-tests"], Sessions: []engine.Session{{ID: session}}}
	run := effects.RunEvidence{ExitCode: 1, ExecutedCount: 1, MatchedCount: 1, Output: "operator " + secret + "\nplanted failure\n"}
	if err = x.recordEngineFeedback(ctx, dispatch, cfg.Workspace, []string{"sh", "-c", "planted"}, run, nil, map[string]any{"exit_code": 1, "executed_count": 1, "matched_count": 1}); err != nil {
		t.Fatal(err)
	}
	marker, err := d.store.GetMarker(ctx, engineFeedbackKey(session))
	if err != nil {
		t.Fatal(err)
	}
	var record engineFeedbackRecord
	if err = json.Unmarshal(marker, &record); err != nil {
		t.Fatal(err)
	}
	stored, err := d.store.GetBlob(ctx, record.Excerpt)
	if err != nil {
		t.Fatal(err)
	}
	withheld := func(where, excerpt string) {
		t.Helper()
		if strings.Contains(excerpt, secret) || strings.Contains(excerpt, "planted failure") || !strings.HasPrefix(excerpt, "[failure excerpt withheld: the secrets it must be redacted of could not be read: principal configuration:") {
			t.Errorf("%s is not withheld: %q", where, excerpt)
		}
	}
	withheld("the stored excerpt", string(stored))
	// An excerpt stored before (here, planted unredacted) is withheld from
	// the brief too.
	ref, err := d.store.PutBlob(ctx, controlplane.Audit{Actor: "test", Action: "seed", Outcome: "ok"}, []byte(run.Output))
	if err != nil {
		t.Fatal(err)
	}
	record.Excerpt = ref
	raw, _ = json.Marshal(record)
	if err = d.store.PutMarker(ctx, controlplane.Audit{Actor: "test", Action: "seed", Outcome: "ok"}, engineFeedbackKey(session), raw); err != nil {
		t.Fatal(err)
	}
	failed := workflow.Evidence{ID: record.Artifact, Stage: "run-tests", Session: session, Fields: map[string]any{"exit_code": json.Number("1"), "executed_count": json.Number("1"), "matched_count": json.Number("1")}}
	runs, err := x.engineFeedback(ctx, engine.Dispatch{Inputs: []workflow.Evidence{failed}}, doc)
	if err != nil || len(runs) != 1 {
		t.Fatal(runs, err)
	}
	withheld("the brief's excerpt", runs[0].Excerpt)

	started := time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)
	nested := effects.RunEvidence{OutputDigest: strings.Repeat("2", 64), Tree: "tree", Command: []string{"go", "test", "./..."}, Started: started, NestedCount: 1, Nested: []commandshim.NestedCall{{Argv: []string{"git", "commit", "-m", secret}, Dir: "/work/" + secret, At: started}}}
	if err = d.recordNestedCommands(nestedTestContext(ctx, "task-withheld", "dispatch-withheld"), nested); err != nil {
		t.Fatal(err)
	}
	raw, err = d.store.GetMarker(ctx, nestedCommandsKey("dispatch-withheld", nested.Command, started))
	if err != nil {
		t.Fatal(err)
	}
	var view struct {
		Count         int                      `json:"count"`
		Calls         []commandshim.NestedCall `json:"calls"`
		CallsWithheld string                   `json:"calls_withheld"`
	}
	if err = json.Unmarshal(raw, &view); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(secret)) || view.Count != 1 || len(view.Calls) != 1 || view.Calls[0].Argv != nil || view.Calls[0].Dir != "" || !view.Calls[0].At.Equal(started) || !strings.Contains(view.CallsWithheld, "could not be read: principal configuration:") {
		t.Fatalf("nested calls not withheld: %s", raw)
	}
}

// FAE-SEC-003: the instance root, which holds the credential and the plane,
// is refused inside a repository or a linked worktree (an ancestor with a
// .git directory or file), before anything is created under it.
func TestInstanceRootInsideRepositoryRefused(t *testing.T) {
	cfg, ctx := fixture(t)
	parent := filepath.Dir(cfg.Root)
	for name, plant := range map[string]func(string) error{
		"repository": func(dir string) error { return os.Mkdir(filepath.Join(dir, ".git"), 0700) },
		"worktree": func(dir string) error {
			return os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: "+filepath.Join(parent, "elsewhere")+"\n"), 0600)
		},
	} {
		repo := filepath.Join(parent, name)
		if err := os.Mkdir(repo, 0700); err != nil {
			t.Fatal(err)
		}
		if err := plant(repo); err != nil {
			t.Fatal(err)
		}
		inside := cfg
		inside.Root = filepath.Join(repo, "deeper", "instances")
		d, err := Start(ctx, inside)
		if err == nil {
			stop(t, d)
			t.Fatalf("an instance root inside a %s started", name)
		}
		if !strings.Contains(err.Error(), "instance root must be outside a repository") {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err = os.Lstat(filepath.Join(repo, "deeper")); !os.IsNotExist(err) {
			t.Fatalf("a refused instance root inside a %s was created: %v", name, err)
		}
	}
}

// FAE-SEC-003 acceptance on the live path: a task's test reads the instance
// credential and planted values (a provider key the daemon's own environment
// carries, a credential-named live-configuration value and an
// Anthropic-shaped token) and prints each raw, percent-encoded and with %q.
// After its failing RED run and its landing, a scan of the plane (every
// table, current and historical rows) and of every other file of the
// instance, the landing scratch's gate output included, finds none of them
// in any printed form, plain or JSON-escaped; the stored excerpt and the
// kept gate output show the redaction.
func TestLiveLandingLeavesNoPlantedSecret(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	independentTask(t, cfg, "planted-land")
	stamp := time.Now().UnixNano()
	daemonKey := fmt.Sprintf("planted-daemon-key+%d/x", stamp)
	liveValue := fmt.Sprintf("planted live&token<%d>", stamp)
	token := fmt.Sprintf("sk-ant-oat01-planted%dTokenValue_x", stamp)
	t.Setenv("FAE_PLANTED_LANDING_API_KEY", daemonKey)
	cfg.Live.Environment["PLANTED_LANDING_TOKEN"] = liveValue
	planted := filepath.Join(filepath.Dir(cfg.Root), "planted-values")
	if err := os.WriteFile(planted, []byte(daemonKey+"\n"+liveValue+"\n"+token+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	layout, err := Paths(cfg.Root, cfg.Instance)
	if err != nil {
		t.Fatal(err)
	}
	source, err := format.Source(fmt.Appendf(nil, `package sample

import (
	"encoding/base64"
	"encoding/hex"
	"net/url"
	"os"
	"strings"
	"testing"
)

func TestBonus(t *testing.T) {
	credential, err := os.ReadFile(%q)
	if err != nil {
		t.Fatal(err)
	}
	planted, err := os.ReadFile(%q)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("operator %%s %%s %%q", hex.EncodeToString(credential), url.QueryEscape(base64.StdEncoding.EncodeToString(credential)), credential)
	for _, line := range strings.Split(strings.TrimSpace(string(planted)), "\n") {
		t.Logf("planted %%s %%q %%s", line, line, url.QueryEscape(line))
	}
	if bonus != 1 {
		t.Fatalf("got %%d want 1", bonus)
	}
}
`, layout.Credential, planted))
	if err != nil {
		t.Fatal(err)
	}
	liveVariants(t, cfg, map[string]map[string]string{"planted-land": {"TestFile": "bonus_test.go", "Test": string(source), "ImplFile": "bonus.go", "Impl": "package sample\n\nvar bonus = 1\n"}})
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	d.live.guard.observe = func(context.Context) (hostguard.Sample, error) { return hostSample(1, 0.5), nil }
	authorize(t, d)
	if _, err = call(t, d, "Admit", AdmitParams{ID: "planted-land", AuthorizingMessage: "message"}, "planted-land"); err != nil {
		t.Fatal(err)
	}
	awaitState(t, d, "planted-land", "Done")
	branch := liveGit(t, cfg.Workspace, "symbolic-ref", "--short", "HEAD")
	rec, err := startLanding(t, d, LandParams{Tasks: []string{"planted-land"}, Target: branch, AuthorizingMessage: "message"})
	if err != nil {
		t.Fatal(err)
	}
	if rec = awaitLanding(t, d, rec.ID); rec.State != "finished" || len(rec.Results) != 1 || rec.Results[0].Outcome != controller.LandLanded {
		t.Fatalf("%+v", rec)
	}
	credential, err := os.ReadFile(d.layout.Credential)
	if err != nil {
		t.Fatal(err)
	}
	printed := map[string]string{
		"operator credential (hex)":              hex.EncodeToString(credential),
		"operator credential (base64, escaped)":  url.QueryEscape(base64.StdEncoding.EncodeToString(credential)),
		"operator credential (%q)":               redactionQuoted(string(credential)),
		"daemon environment key":                 daemonKey,
		"daemon environment key (%q)":            redactionQuoted(daemonKey),
		"daemon environment key (query-escaped)": url.QueryEscape(daemonKey),
		"live configuration value":               liveValue,
		"live configuration value (escaped)":     url.QueryEscape(liveValue),
		"Anthropic token":                        token,
	}
	forms := map[string]string{}
	for name, value := range printed {
		forms[name] = value
		forms[name+" in JSON"] = redactionJSON(value, true)
	}
	var leaks []string
	excerpts := 0
	check := func(where string, value []byte) {
		for name, form := range forms {
			if bytes.Contains(value, []byte(form)) {
				leaks = append(leaks, name+" in "+where)
			}
		}
		if strings.HasPrefix(where, "SELECT") && bytes.Contains(value, []byte("planted [redacted]")) {
			excerpts++ // the failing RED run's stored excerpt
		}
	}
	err = d.store.Mutate(ctx, controlplane.Audit{Actor: "test", Action: "planted secret scan", Outcome: "ok"}, func(ctx context.Context, tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT table_name FROM information_schema.tables WHERE table_schema = DATABASE() AND table_type = 'BASE TABLE'`)
		if err != nil {
			return err
		}
		var tables []string
		for rows.Next() {
			var name string
			if err = rows.Scan(&name); err != nil {
				rows.Close()
				return err
			}
			tables = append(tables, name)
		}
		rows.Close()
		for _, table := range tables {
			for _, q := range []string{"SELECT * FROM `" + table + "`", "SELECT * FROM `dolt_history_" + table + "`"} {
				rows, err := tx.QueryContext(ctx, q)
				if err != nil {
					return fmt.Errorf("%s: %w", q, err)
				}
				columns, _ := rows.Columns()
				values := make([]sql.RawBytes, len(columns))
				dest := make([]any, len(columns))
				for i := range values {
					dest[i] = &values[i]
				}
				for rows.Next() {
					if err = rows.Scan(dest...); err != nil {
						rows.Close()
						return err
					}
					for _, v := range values {
						check(q, v)
					}
				}
				rows.Close()
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	private := map[string]bool{d.layout.Credential: true, filepath.Join(d.layout.Run, "plane-credential"): true}
	gateOutputs, redacted := 0, 0
	err = filepath.WalkDir(d.layout.Root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && path == d.layout.Data {
			return filepath.SkipDir // scanned through SQL above
		}
		if !entry.Type().IsRegular() || private[path] || strings.HasPrefix(entry.Name(), "controller-environment-") {
			return nil
		}
		if strings.HasPrefix(entry.Name(), "gate-capture-") {
			leaks = append(leaks, "a named capture file "+path)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		check(path, b)
		if strings.HasPrefix(entry.Name(), "gate-output-") {
			gateOutputs++
			if bytes.Contains(b, []byte("planted [redacted]")) {
				redacted++
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(leaks) > 0 {
		t.Errorf("planted secrets leaked: %v", leaks)
	}
	if gateOutputs == 0 || redacted == 0 || excerpts == 0 {
		t.Fatalf("the landing kept %d gate outputs, %d showing the redaction; %d plane values show it", gateOutputs, redacted, excerpts)
	}
}
