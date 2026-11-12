package provider

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	r "github.com/xleelz/fae/runtime"
	"github.com/xleelz/fae/workflow"
)

const testThread = "12345678-1234-1234-1234-123456789abc"

func persistentLaunch(t *testing.T) r.BridgeConfig {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gitRepository(t, root)
	return r.BridgeConfig{Protocol: "fae-agent-v1", Launch: r.Launch{WorkflowID: "workflow", WorkflowRevision: "revision", SessionPolicy: "workflow-global", Role: "oracle", TaskID: "one", DispatchID: "dispatch", SessionID: "session", Worktree: root, Binding: workflow.Binding{Provider: "codex", Model: "fixture"}, Profile: workflow.Profile{Allowed: []string{"handoff", "read"}}, Brief: json.RawMessage(`[{"kind":"stage","stage":"spec"},{"kind":"kernel_contract","output_schema":{"type":"object","properties":{"accepted":{"type":"boolean"}},"required":["accepted"],"additionalProperties":false}}]`)}, Handoff: filepath.Join(root, "handoff.json"), LogDirectory: root}
}
func sessionRoot(t *testing.T) string {
	t.Helper()
	p, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	if e = os.Chmod(p, 0700); e != nil {
		t.Fatal(e)
	}
	return p
}
func TestPersistentScopeAndRecovery(t *testing.T) {
	l := persistentLaunch(t)
	root := sessionRoot(t)
	s, e := openProviderSession(context.Background(), root, l)
	if e != nil {
		t.Fatal(e)
	}
	if s.resumed {
		t.Fatal("new scope resumed")
	}
	key := s.state.Scope
	if e = s.complete(testThread); e != nil {
		t.Fatal(e)
	}
	s.close()
	l.TaskID = "two"
	l.Worktree = sessionRoot(t)
	s, e = openProviderSession(context.Background(), root, l)
	if e != nil {
		t.Fatal(e)
	}
	if !s.resumed || s.state.Scope != key || s.state.ThreadID != testThread {
		t.Fatal("global context did not survive task/restart", s.state)
	}
	// Simulate death after dispatch: pending context must never be resumed.
	s.close()
	s, e = openProviderSession(context.Background(), root, l)
	if e != nil {
		t.Fatal(e)
	}
	if s.resumed {
		t.Fatal("uncertain context reused")
	}
	s.close()
	for _, change := range []func(*r.BridgeConfig){func(v *r.BridgeConfig) { v.WorkflowRevision = "new" }, func(v *r.BridgeConfig) { v.Role = "planner" }, func(v *r.BridgeConfig) { v.Binding.Model = "other" }, func(v *r.BridgeConfig) { v.Mode = "local" }, func(v *r.BridgeConfig) { v.Profile.Allowed = []string{"handoff"} }} {
		v := l
		change(&v)
		s, e = openProviderSession(context.Background(), root, v)
		if e != nil {
			t.Fatal(e)
		}
		if s.state.Scope == key {
			t.Fatal("scope boundary ignored")
		}
		s.close()
	}
	for _, change := range []func(*r.BridgeConfig){func(v *r.BridgeConfig) { v.FreshContext = true }} {
		v := l
		change(&v)
		s, e = openProviderSession(context.Background(), root, v)
		if e != nil || s != nil {
			t.Fatal("fresh session retained", s, e)
		}
	}
}
func TestPersistentLockCancellationAndValidation(t *testing.T) {
	l := persistentLaunch(t)
	root := sessionRoot(t)
	s, e := openProviderSession(context.Background(), root, l)
	if e != nil {
		t.Fatal(e)
	}
	defer s.close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, e = openProviderSession(ctx, root, l); e == nil {
		t.Fatal("concurrent turn not serialized")
	}
	if e = s.complete("--last"); e == nil {
		t.Fatal("ambient identity accepted")
	}
	if e = os.Chmod(root, 0755); e != nil {
		t.Fatal(e)
	}
	if _, e = openProviderSession(context.Background(), root, l); e == nil {
		t.Fatal("public context cache accepted")
	}
}
func TestPersistentCorruptAndSymlinkCache(t *testing.T) {
	l := persistentLaunch(t)
	root := sessionRoot(t)
	s, e := openProviderSession(context.Background(), root, l)
	if e != nil {
		t.Fatal(e)
	}
	dir := s.dir
	s.close()
	if e = os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"scope":"other","thread_id":"invalid"}`), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = openProviderSession(context.Background(), root, l); e == nil {
		t.Fatal("corrupt context accepted")
	}
	alias := filepath.Join(sessionRoot(t), "alias")
	os.Symlink(root, alias)
	if _, e = openProviderSession(context.Background(), alias, l); e == nil {
		t.Fatal("symlink root accepted")
	}
}
func TestPersistentBridgeResumesWithFreshTaskHeader(t *testing.T) {
	l := persistentLaunch(t)
	root := sessionRoot(t)
	fake := filepath.Join(l.Worktree, "fake-codex")
	script := `#!/bin/sh
out=''
for arg in "$@"; do printf '%s\n' "$arg"; done > args.txt
while [ "$#" -gt 0 ]; do
 if [ "$1" = '--output-last-message' ]; then shift; out="$1"; fi
 shift
done
cat > prompt.json
printf '%s\n' '{"type":"thread.started","thread_id":"12345678-1234-1234-1234-123456789abc"}'
printf '%s\n' '{"type":"turn.completed"}'
printf '%s' '{"output":{"accepted":true},"edits":[]}' > "$out"
`
	if e := os.WriteFile(fake, []byte(script), 0700); e != nil {
		t.Fatal(e)
	}
	c := Config{Binary: fake, SessionRoot: root, Files: []string{"answer.go"}, Timeout: time.Second}
	for i := 0; i < 2; i++ {
		if i == 1 {
			l.TaskID = "two"
			l.DispatchID = "second"
			l.Worktree = sessionRoot(t)
			gitRepository(t, l.Worktree)
			l.Handoff = filepath.Join(l.Worktree, "handoff.json")
			l.LogDirectory = l.Worktree
		}
		config := filepath.Join(l.Worktree, "config.json")
		raw, _ := json.Marshal(l)
		os.WriteFile(config, raw, 0600)
		if e := Run(context.Background(), c, config); e != nil {
			t.Fatal(e)
		}
		dirs, _ := filepath.Glob(filepath.Join(root, "*", "cwd"))
		if len(dirs) != 1 {
			t.Fatal(dirs)
		}
		args, _ := os.ReadFile(filepath.Join(dirs[0], "args.txt"))
		prompt, _ := os.ReadFile(filepath.Join(dirs[0], "prompt.json"))
		if strings.Contains(string(args), "--ephemeral") || strings.Contains(string(args), "--last") || !strings.Contains(string(args), "--ignore-user-config") || !strings.Contains(string(args), "--ignore-rules") {
			t.Fatal(string(args))
		}
		if (i == 1) != strings.Contains(string(args), "resume\n") {
			t.Fatal(string(args))
		}
		p := promptBlocks(t, prompt)
		if !strings.Contains(string(p["task_header"]), l.TaskID) || !strings.Contains(string(p["task_header"]), l.Worktree) {
			t.Fatal(string(prompt))
		}
		audits, _ := filepath.Glob(filepath.Join(l.LogDirectory, "provider-*", "provider-session.json"))
		if len(audits) != 1 {
			t.Fatal(audits)
		}
		data, _ := os.ReadFile(audits[0])
		var audit SessionAudit
		json.Unmarshal(data, &audit)
		if audit.ThreadID != testThread || audit.Resumed != (i == 1) || audit.TaskID != l.TaskID {
			t.Fatal(audit)
		}
	}
}
func TestAnnouncedThreadIdentity(t *testing.T) {
	for _, raw := range []string{`{"type":"thread.started","thread_id":"bad"}`, `{"type":"turn.completed"}`, `{"type":"thread.started","thread_id":"12345678-1234-1234-1234-123456789abc"}` + "\n" + `{"type":"thread.started","thread_id":"12345678-1234-1234-1234-123456789def"}`} {
		path := filepath.Join(t.TempDir(), "events")
		os.WriteFile(path, []byte(raw), 0600)
		if _, e := announcedThread(path); e == nil {
			t.Fatal(raw)
		}
	}
}

func TestPersistentFreshReviewAndFailedTurnRotation(t *testing.T) {
	l := persistentLaunch(t)
	root := sessionRoot(t)
	fake := filepath.Join(l.Worktree, "fake-codex")
	script := `#!/bin/sh
out=''
for arg in "$@"; do printf '%s\n' "$arg"; done > args.txt
while [ "$#" -gt 0 ]; do
 if [ "$1" = '--output-last-message' ]; then shift; out="$1"; fi
 shift
done
cat >/dev/null
printf '%s\n' '{"type":"thread.started","thread_id":"12345678-1234-1234-1234-123456789abc"}'
printf '%s' '{"output":{"accepted":true},"edits":[]}' > "$out"
`
	os.WriteFile(fake, []byte(script), 0700)
	c := Config{Binary: fake, SessionRoot: root, Files: []string{"answer.go"}, Timeout: time.Second}
	run := func() error {
		config := filepath.Join(l.Worktree, "config.json")
		raw, _ := json.Marshal(l)
		os.WriteFile(config, raw, 0600)
		return Run(context.Background(), c, config)
	}
	l.FreshContext = true
	if e := run(); e != nil {
		t.Fatal(e)
	}
	files, _ := filepath.Glob(filepath.Join(l.LogDirectory, "provider-*", "cwd", "args.txt"))
	if len(files) != 1 {
		t.Fatal(files)
	}
	args, _ := os.ReadFile(files[0])
	if !strings.Contains(string(args), "--ephemeral") {
		t.Fatal("review reused context", string(args))
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Fatal("review created persistent context")
	}
	l.FreshContext = false
	l.Handoff = filepath.Join(l.Worktree, "next-handoff.json")
	os.WriteFile(fake, []byte(script+"exit 1\n"), 0700)
	if e := run(); e == nil {
		t.Fatal("failed provider accepted")
	}
	s, e := openProviderSession(context.Background(), root, l)
	if e != nil {
		t.Fatal(e)
	}
	if s.resumed {
		t.Fatal("failed context reused")
	}
	if e = s.complete(testThread); e != nil {
		t.Fatal(e)
	}
	s.close()
	// Resumed provider identity mismatch must be rejected before handoff/edit.
	os.WriteFile(fake, []byte(strings.ReplaceAll(script, testThread, "12345678-1234-1234-1234-123456789def")), 0700)
	if e := run(); e == nil || !strings.Contains(e.Error(), "different thread") {
		t.Fatal(e)
	}
	if _, e := os.Stat(l.Handoff); !os.IsNotExist(e) {
		t.Fatal("mismatched context produced handoff", e)
	}
}

func TestTaskBoundPersistentScope(t *testing.T) {
	l := persistentLaunch(t)
	l.SessionPolicy = "task-bound"
	root := sessionRoot(t)
	s, e := openProviderSession(context.Background(), root, l)
	if e != nil {
		t.Fatal(e)
	}
	firstScope := s.state.Scope
	if s.resumed {
		t.Fatal("first task started with context")
	}
	if e = s.complete(testThread); e != nil {
		t.Fatal(e)
	}
	s.close()
	l.DispatchID = "next-dispatch"
	s, e = openProviderSession(context.Background(), root, l)
	if e != nil {
		t.Fatal(e)
	}
	if !s.resumed || s.state.ThreadID != testThread || s.state.Scope != firstScope {
		t.Fatal("same task lost context", s.state)
	}
	if e = s.complete(testThread); e != nil {
		t.Fatal(e)
	}
	s.close()
	l.TaskID = "other-task"
	s, e = openProviderSession(context.Background(), root, l)
	if e != nil {
		t.Fatal(e)
	}
	if s.resumed || s.state.Scope == firstScope {
		t.Fatal("task context crossed isolation boundary", s.state)
	}
	s.close()
	l.TaskID = "one"
	l.FreshContext = true
	s, e = openProviderSession(context.Background(), root, l)
	if e != nil || s != nil {
		t.Fatal("task-bound review reused context", s, e)
	}
	l.FreshContext = false
	l.SessionPolicy = "workflow-global"
	s, e = openProviderSession(context.Background(), root, l)
	if e != nil {
		t.Fatal(e)
	}
	if s.resumed || s.state.Scope == firstScope {
		t.Fatal("task-bound context reused by global role")
	}
	s.close()
}

func TestPersistentPolicyRefusal(t *testing.T) {
	for _, tc := range []struct{ policy, task string }{{"", "one"}, {"unknown", "one"}, {"task-bound", ""}} {
		l := persistentLaunch(t)
		l.SessionPolicy = tc.policy
		l.TaskID = tc.task
		if _, e := openProviderSession(context.Background(), sessionRoot(t), l); e == nil {
			t.Fatal("invalid policy accepted", tc)
		}
	}
}
