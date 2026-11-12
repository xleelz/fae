package daemon

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/effects"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

// Secret value redaction keeps each distinct value once, replaces the
// longest first, and drops empty ones; a binary credential is matched in
// each of its printed forms.
func TestSecretValuesRedactEveryForm(t *testing.T) {
	credential := []byte("0123456789abcdef0123456789abcdef")
	s := newSecretValues(append(credentialForms(credential), []byte("token-1"), []byte("token-12"), nil, []byte("token-1"))...)
	for _, form := range []string{string(credential), hex.EncodeToString(credential), strings.ToUpper(hex.EncodeToString(credential)), base64.StdEncoding.EncodeToString(credential), base64.RawURLEncoding.EncodeToString(credential)} {
		if got := s.redactString("a " + form + " b"); got != "a [redacted] b" {
			t.Errorf("form %q: %q", form, got)
		}
	}
	if got := s.redactString("token-12 token-1"); got != "[redacted] [redacted]" {
		t.Fatalf("a shorter secret replaced first: %q", got)
	}
	for _, name := range []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "DB_PASSWORD", "client_secret"} {
		if !credentialVariable(name) {
			t.Errorf("%s not a credential variable", name)
		}
	}
	for _, name := range []string{"CLAUDE_CONFIG_DIR", "PATH", "GOFLAGS"} {
		if credentialVariable(name) {
			t.Errorf("%s taken for a credential variable", name)
		}
	}
}

// FAE-SEC-003: a failed run's excerpt never carries a secret the daemon
// holds. A failing run prints, and a failed required check's go test -json
// events carry: the operator credential (raw and hex), the Dolt password
// (the plane credential's hex), a delegated principal's credential (base64),
// the UI token and write credential, and two keys from the live
// configuration's environment, one JSON-escaped in the events. The stored
// excerpt has each replaced and keeps the rest, the CLAUDE_CONFIG_DIR path
// included; the brief gets the same. An excerpt stored unredacted, before
// this rule, is redacted before it reaches a brief.
func TestEngineFeedbackExcerptRedactsKnownSecrets(t *testing.T) {
	cfg, ctx := fixture(t)
	configPath := filepath.Join(filepath.Dir(cfg.Root), "principals.json")
	principalPath := filepath.Join(filepath.Dir(cfg.Root), "controller-token")
	raw, _ := json.Marshal([]PrincipalCredential{{CredentialFile: principalPath, Actor: "controller"}})
	if err := os.WriteFile(configPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	cfg.PrincipalConfig = configPath
	cfg.UI = &UIConfig{Binary: filepath.Join(filepath.Dir(cfg.Root), "absent-ui"), Port: freeLoopbackPort(t), AllowWrites: true}
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	read := func(path string) []byte {
		b, err := os.ReadFile(path)
		if err != nil || len(b) != 32 {
			t.Fatal(path, len(b), err)
		}
		return b
	}
	operator, plane, principal := read(d.layout.Credential), read(filepath.Join(d.layout.Run, "plane-credential")), read(principalPath)
	if d.ui == nil || d.ui.token == "" || len(d.ui.write) == 0 {
		t.Fatal("the UI credentials were not issued")
	}
	providerKey, escapedKey := "sk-ant-planted-provider-key-0123456789", "ghp_planted&token<value>"
	x := &liveExecutor{daemon: d, config: LiveConfig{Environment: map[string]string{"ANTHROPIC_API_KEY": providerKey, "GITHUB_TOKEN": escapedKey, "CLAUDE_CONFIG_DIR": "/home/someone/.claude"}}}
	secrets := map[string]string{
		"operator credential":       string(operator),
		"operator credential (hex)": hex.EncodeToString(operator),
		"Dolt password":             hex.EncodeToString(plane),
		"principal credential":      base64.StdEncoding.EncodeToString(principal),
		"UI token":                  d.ui.token,
		"UI write credential":       strings.ToUpper(hex.EncodeToString(d.ui.write)),
		"provider key":              providerKey,
		"escaped key":               escapedKey,
	}
	run := effects.RunEvidence{ExitCode: 1, ExecutedCount: 1, MatchedCount: 1,
		Output: "config dir /home/someone/.claude\noperator " + secrets["operator credential (hex)"] + "\ndolt " + secrets["Dolt password"] + "\nprincipal " + secrets["principal credential"] + "\nui " + secrets["UI token"] + " " + secrets["UI write credential"] + "\n",
		Stderr: "raw " + secrets["operator credential"] + "\nkey " + providerKey + "\nplanted failure\n"}
	check := effects.RunEvidence{ExitCode: 1, ExecutedCount: 1, Output: strings.Join([]string{
		`{"Action":"run","Package":"p","Test":"TestPlanted"}`,
		`{"Action":"output","Package":"p","Test":"TestPlanted","Output":"    planted_test.go:9: token ghp_planted\u0026token\u003cvalue\u003e leaked\n"}`,
		`{"Action":"fail","Package":"p","Test":"TestPlanted"}`,
		`{"Action":"output","Package":"p","Output":"FAIL\tp\t0.01s\n"}`,
		`{"Action":"fail","Package":"p"}`,
	}, "\n") + "\n"}
	doc := defaults.Workflow()
	session := "t/h/4/session/0"
	dispatch := engine.Dispatch{ID: "t/h/4", TaskID: "t", Stage: "run-tests", Config: doc.Stages["run-tests"], Sessions: []engine.Session{{ID: session}}}
	if err = x.recordEngineFeedback(ctx, dispatch, cfg.Workspace, []string{"sh", "-c", "planted"}, run, []effects.RunEvidence{check}, map[string]any{"exit_code": 1, "executed_count": 1, "matched_count": 1}); err != nil {
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
	clean := func(where string, excerpt []byte) {
		t.Helper()
		for name, secret := range secrets {
			if bytes.Contains(excerpt, []byte(secret)) {
				t.Errorf("%s carries the %s:\n%s", where, name, excerpt)
			}
		}
		for _, kept := range []string{"[redacted]", "/home/someone/.claude", "planted failure", "planted_test.go:9: token [redacted] leaked", "FAIL\tp"} {
			if !bytes.Contains(excerpt, []byte(kept)) {
				t.Errorf("%s lost %q:\n%s", where, kept, excerpt)
			}
		}
	}
	t.Logf("stored excerpt:\n%s", stored)
	clean("the stored excerpt", stored)
	failed := workflow.Evidence{ID: record.Artifact, Stage: "run-tests", Session: session, Fields: map[string]any{"exit_code": json.Number("1"), "executed_count": json.Number("1"), "matched_count": json.Number("1")}}
	runs, err := x.engineFeedback(ctx, engine.Dispatch{Inputs: []workflow.Evidence{failed}}, doc)
	if err != nil || len(runs) != 1 {
		t.Fatal(runs, err)
	}
	clean("the brief's excerpt", []byte(runs[0].Excerpt))
	// An excerpt stored before excerpts were redacted.
	legacy, _ := effects.FailureExcerpt([]string{"sh", "-c", "planted"}, run, []effects.RunEvidence{check})
	if strings.Contains(legacy, "[redacted]") {
		t.Fatal("the legacy excerpt is already redacted")
	}
	ref, err := d.store.PutBlob(ctx, controlplane.Audit{Actor: "test", Action: "seed", Outcome: "ok"}, []byte(legacy))
	if err != nil {
		t.Fatal(err)
	}
	record.Excerpt = ref
	raw, _ = json.Marshal(record)
	if err = d.store.PutMarker(ctx, controlplane.Audit{Actor: "test", Action: "seed", Outcome: "ok"}, engineFeedbackKey(session), raw); err != nil {
		t.Fatal(err)
	}
	if runs, err = x.engineFeedback(ctx, engine.Dispatch{Inputs: []workflow.Evidence{failed}}, doc); err != nil || len(runs) != 1 {
		t.Fatal(runs, err)
	}
	clean("the brief's excerpt of a legacy record", []byte(runs[0].Excerpt))
}
