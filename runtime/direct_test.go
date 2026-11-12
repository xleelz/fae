package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xleelz/fae/commandshim"
	"github.com/xleelz/fae/workflow"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestBridgeHelper(t *testing.T) {
	if os.Getenv("BRIDGE_HELPER") != "1" {
		return
	}
	var c BridgeConfig
	raw, e := os.ReadFile(os.Args[len(os.Args)-1])
	if e != nil {
		os.Exit(11)
	}
	if json.Unmarshal(raw, &c) != nil {
		os.Exit(12)
	}
	switch os.Getenv("BRIDGE_MODE") {
	case "persist-state":
		home, tmp := os.Getenv("HOME"), os.Getenv("TMPDIR")
		if home == c.LogDirectory || tmp == c.LogDirectory || home == tmp {
			os.Exit(41)
		}
		token := filepath.Join(home, "resume-token")
		if _, err := os.Stat(token); os.IsNotExist(err) {
			if os.WriteFile(token, []byte(c.SessionID), 0600) != nil {
				os.Exit(42)
			}
		}

	case "no-shims":
		if strings.Contains(os.Getenv("PATH"), "command-shims") || os.Getenv("CLAUDE_SETTINGS_PATH") != "" {
			os.Exit(31)
		}
	case "shims":
		for _, name := range []string{"go", "gofmt", "git", "cargo"} {
			cmd := exec.Command(name, "--version")
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 77 || !strings.Contains(string(out), "broker dispatch") {
				os.Exit(29)
			}
		}
	case "brokered-shims":
		dir := strings.SplitN(os.Getenv("PATH"), ":", 2)[0]
		script, _ := os.ReadFile(filepath.Join(dir, "go"))
		policy, _ := os.ReadFile(filepath.Join(dir, ".policy.json"))
		_ = os.WriteFile(filepath.Join(c.Worktree, "shim-evidence"), append(append(script, '\n'), policy...), 0600)
	case "child":
		time.Sleep(700 * time.Millisecond)
		_ = os.WriteFile(filepath.Join(c.Worktree, "child-survived"), []byte("bad"), 0600)
		os.Exit(0)
	case "descendants":
		cmd := exec.Command(os.Args[0], os.Args[1:]...)
		cmd.Env = append(os.Environ(), "BRIDGE_MODE=child")
		if cmd.Start() != nil {
			os.Exit(19)
		}
		_ = os.WriteFile(filepath.Join(c.Worktree, "bridge-started"), []byte("ready"), 0600)
		for {
			time.Sleep(time.Second)
		}
	case "wait":
		for {
			time.Sleep(time.Second)
		}
	case "missing-provider":
		_ = os.WriteFile(filepath.Join(c.LogDirectory, "events.jsonl"), []byte(`{"type":"error","message":"HTTP 429"}`), 0600)
		os.Exit(0)
	case "missing":
		os.Exit(0)
	case "malformed":
		_ = os.WriteFile(c.Handoff, []byte(`{"dispatch_id":"a","dispatch_id":"b"}`), 0600)
		os.Exit(0)
	case "fifo":
		_ = syscall.Mkfifo(c.Handoff, 0600)
		os.Exit(0)
	case "symlink":
		_ = os.Symlink(filepath.Join(c.LogDirectory, "config.json"), c.Handoff)
		os.Exit(0)
	case "large":
		_ = os.WriteFile(c.Handoff, []byte(strings.Repeat("x", 2048)), 0600)
		os.Exit(0)
	case "stdout":
		fmt.Println(`{"output":{"ok":true}}`)
		os.Exit(0)
	case "nonzero":
		os.Exit(17)
	case "echo-brief":
		output, _ := json.Marshal(map[string]any{"brief": c.Brief})
		b, _ := json.Marshal(Handoff{DispatchID: c.DispatchID, SessionID: c.SessionID, TaskID: c.TaskID, Output: output})
		_ = os.WriteFile(c.Handoff, b, 0600)
		os.Exit(0)
	}
	if os.Getenv("BRIDGE_MODE") == "exit-with-child" {
		cmd := exec.Command(os.Args[0], os.Args[1:]...)
		cmd.Env = append(os.Environ(), "BRIDGE_MODE=child")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if cmd.Start() != nil {
			os.Exit(19)
		}
	}
	cwd, _ := os.Getwd()
	output, _ := json.Marshal(map[string]any{"cwd": cwd, "secret": os.Getenv("FAE_SECRET"), "git": os.Getenv("GIT_DIR"), "number": json.Number("9007199254740993"), "args": os.Args[3 : len(os.Args)-1]})
	h := Handoff{DispatchID: c.DispatchID, SessionID: c.SessionID, TaskID: c.TaskID, Output: output}
	if os.Getenv("BRIDGE_MODE") == "stale" {
		h.DispatchID = "old"
	}
	if os.Getenv("BRIDGE_MODE") == "idless" {
		h.DispatchID = ""
	}
	b, _ := json.Marshal(h)
	_ = os.WriteFile(c.Handoff, b, 0600)
	if os.Getenv("BRIDGE_MODE") == "oldtime" {
		old := time.Now().Add(-time.Hour)
		_ = os.Chtimes(c.Handoff, old, old)
	}
	fmt.Print(strings.Repeat("x", 4096))
	os.Exit(0)
}
func directFixture(t *testing.T, mode string) (*Direct, Launch) {
	t.Helper()
	root, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	exe, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	d := &Direct{Root: root, Command: []string{exe, "-test.run=^TestBridgeHelper$", "--", "$(touch nope); literal", "{config}"}, Environment: map[string]string{"BRIDGE_HELPER": "1", "BRIDGE_MODE": mode, "GORACE": "atexit_sleep_ms=0"}, TailBytes: 128, MaxHandoffBytes: 1024}
	return d, Launch{DispatchID: "dispatch", SessionID: "session", TaskID: "task", Role: "role", Worktree: root, Mode: "local", Brief: json.RawMessage(`[]`)}
}
func TestDirectRealProcess(t *testing.T) {
	t.Setenv("FAE_SECRET", "secret")
	t.Setenv("GIT_DIR", "hostile")
	d, l := directFixture(t, "")
	l.Profile = workflow.Profile{
		Allowed: []string{"read", "handoff"},
		Denied:  []string{"edit"},
		Test:    true,
		Shims:   []string{"go", "git"},
		Effects: []string{"github"},
	}
	out, e := d.Run(context.Background(), l)
	if e != nil {
		t.Fatal(e)
	}
	if len(out.Tail) != 128 {
		t.Fatal(len(out.Tail))
	}
	if len(d.Sessions()) != 0 {
		t.Fatal("session retained")
	}
	if !strings.Contains(string(out.Output), `9007199254740993`) || !strings.Contains(string(out.Output), `"secret":""`) || !strings.Contains(string(out.Output), `"git":""`) || !strings.Contains(string(out.Output), `$(touch nope); literal`) {
		t.Fatal(string(out.Output))
	}
	if _, e = d.Run(context.Background(), l); e == nil {
		t.Fatal("duplicate session accepted")
	}
	configPath := filepath.Join(out.LogDirectory, "config.json")
	st, e := os.Stat(configPath)
	if e != nil || st.Mode().Perm() != 0600 {
		t.Fatal(st, e)
	}
	// FAE-RT-013: the generated configuration file records the mode, session
	// identity, worktree, role, and exact tool profile grant for the launch.
	rawConfig, e := os.ReadFile(configPath)
	if e != nil {
		t.Fatal(e)
	}
	var recorded BridgeConfig
	if e = json.Unmarshal(rawConfig, &recorded); e != nil {
		t.Fatal(e)
	}
	if recorded.Protocol != "fae-agent-v1" || recorded.SessionID != l.SessionID || recorded.Worktree != l.Worktree || recorded.Role != l.Role || recorded.Mode != l.Mode || !reflect.DeepEqual(recorded.Profile, l.Profile) {
		t.Fatalf("recorded grant mismatch: got %+v want %+v", recorded, l)
	}
}
func TestDirectFailures(t *testing.T) {
	for mode, class := range map[string]string{"missing": "missing-handoff", "stdout": "missing-handoff", "symlink": "missing-handoff", "large": "malformed-output", "fifo": "malformed-output", "malformed": "malformed-output", "stale": "stale-handoff", "oldtime": "stale-handoff", "idless": "id-less-handoff", "nonzero": "process-failed"} {
		t.Run(mode, func(t *testing.T) {
			d, l := directFixture(t, mode)
			_, e := d.Run(context.Background(), l)
			var te *TurnError
			if !errors.As(e, &te) || te.Class != class || !filepath.IsAbs(te.Path) {
				t.Fatal(e)
			}
		})
	}
}
func TestDirectCancellation(t *testing.T) {
	d, l := directFixture(t, "wait")
	d.Timeout = 100 * time.Millisecond
	start := time.Now()
	_, e := d.Run(context.Background(), l)
	var te *TurnError
	if !errors.As(e, &te) || te.Class != "interrupted" {
		t.Fatal(e)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("cancellation unbounded")
	}
	if len(d.Sessions()) != 0 {
		t.Fatal("live process remains")
	}
}
func TestDirectLaunchRefusals(t *testing.T) {
	for _, which := range []string{"relative", "symlink", "config", "env", "mode"} {
		t.Run(which, func(t *testing.T) {
			d, l := directFixture(t, "")
			switch which {
			case "relative":
				l.Worktree = "."
			case "symlink":
				link := filepath.Join(d.Root, "link")
				if e := os.Symlink(d.Root, link); e != nil {
					t.Fatal(e)
				}
				l.Worktree = link
			case "config":
				d.Command = d.Command[:len(d.Command)-1]
			case "env":
				d.Environment["DOLT_PASSWORD"] = "secret"
			case "mode":
				l.Mode = ""
			}
			if _, e := d.Run(context.Background(), l); e == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestDirectCancellationReapsGroup(t *testing.T) {
	d, l := directFixture(t, "descendants")
	d.Timeout = 300 * time.Millisecond
	_, e := d.Run(context.Background(), l)
	if e == nil {
		t.Fatal("unexpected success")
	}
	time.Sleep(800 * time.Millisecond)
	if _, e := os.Stat(filepath.Join(l.Worktree, "child-survived")); !os.IsNotExist(e) {
		t.Fatal("child survived process group cleanup", e)
	}
}

func TestDirectCompletionReapsGroup(t *testing.T) {
	d, l := directFixture(t, "exit-with-child")
	if _, e := d.Run(context.Background(), l); e != nil {
		t.Fatal(e)
	}
	time.Sleep(800 * time.Millisecond)
	if _, e := os.Stat(filepath.Join(l.Worktree, "child-survived")); !os.IsNotExist(e) {
		t.Fatal("completed leader left child alive", e)
	}
}

func TestRoleSessionPATHShimsPrecedeConfiguredPath(t *testing.T) {
	d, l := directFixture(t, "shims")
	l.Session = "agentic"
	l.Profile.Allowed = []string{"handoff", "shell"}
	d.Environment["PATH"] = "/usr/bin:/bin:/usr/local/bin"
	if _, err := d.Run(context.Background(), l); err != nil {
		t.Fatal(err)
	}
}

// FAE-RT-015(d), FAE-HOST-011 (2026-11-05 amendment): a tool-less session has
// no role shims on PATH; an agentic session with shell refuses to launch if
// shims are not first on PATH or the PreToolUse hook is not registered.
func TestToolLessSessionHasNoRoleShimsAndAgenticShellRequiresShimsAndHook(t *testing.T) {
	d, l := directFixture(t, "no-shims")
	d.Environment["PATH"] = "/usr/bin:/bin:/usr/local/bin"
	if _, err := d.Run(context.Background(), l); err != nil {
		t.Fatalf("tool-less session failed or mounted role shims: %v", err)
	}

	d2, l2 := directFixture(t, "shims")
	l2.Session = "agentic"
	l2.Profile.Allowed = []string{"handoff", "shell"}
	d2.OmitRoleShimsOnPath = true
	if _, err := d2.Run(context.Background(), l2); err == nil || !strings.Contains(err.Error(), "shims first on PATH") {
		t.Fatalf("agentic shell without shims first on PATH accepted: %v", err)
	}

	d3, l3 := directFixture(t, "shims")
	l3.Session = "agentic"
	l3.Profile.Allowed = []string{"handoff", "shell"}
	d3.DisablePreToolHook = true
	if _, err := d3.Run(context.Background(), l3); err == nil || !strings.Contains(err.Error(), "PreToolUse hook") {
		t.Fatalf("agentic shell without PreToolUse hook accepted: %v", err)
	}
}

// FAE-HOST-011: with a broker configured, the role's wrappers call the broker
// verb with a policy carrying this launch's worktree, mode and tool profile.
func TestRoleSessionShimsCarryTheLaunchPolicy(t *testing.T) {
	d, l := directFixture(t, "brokered-shims")
	d.Shims = commandshim.RoleShims{Broker: "/opt/fae/bin/fae", Ledger: "/var/lib/fae-host"}
	l.Session = "agentic"
	l.Profile.Allowed = []string{"handoff", "shell"}
	l.Profile.Test = true
	if _, err := d.Run(context.Background(), l); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(l.Worktree, "shim-evidence"))
	if err != nil {
		t.Fatal(err)
	}
	script, policy, _ := strings.Cut(string(raw), "\n\n")
	var s commandshim.RoleShims
	if err = json.Unmarshal([]byte(policy), &s); err != nil {
		t.Fatal(err, string(raw))
	}
	if !strings.Contains(script, "'/opt/fae/bin/fae' heavy-op run --shim-policy") || s.Worktree != l.Worktree || !s.Local || !s.Test || s.Build || s.Format || s.Ledger != "/var/lib/fae-host" {
		t.Fatalf("%s", raw)
	}
}

func TestMissingHandoffSharesProviderLogReader(t *testing.T) {
	d, l := directFixture(t, "missing-provider")
	_, e := d.Run(context.Background(), l)
	var turn *TurnError
	if !errors.As(e, &turn) || turn.Class != "missing-handoff" || !strings.Contains(e.Error(), "rate-limited") {
		t.Fatal(e)
	}
}

func TestWriteClaudePreToolHookIncludesShimDir(t *testing.T) {
	dir := t.TempDir()
	path, err := WriteClaudePreToolHook(dir, "/opt/fae/bin/fae")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "heavy-op hook") || !strings.Contains(string(raw), "--shim-dir") {
		t.Fatalf("expected heavy-op hook and --shim-dir in %s", string(raw))
	}
	// Corrupt settings to omit --shim-dir and verify VerifyAgenticShellLaunch rejects it.
	shimDir := filepath.Join(dir, "command-shims")
	if err := os.MkdirAll(shimDir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"go", "git", "gofmt"} {
		if err := os.WriteFile(filepath.Join(shimDir, tool), []byte("#!/bin/sh\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	env := []string{"PATH=" + shimDir + ":/usr/bin:/bin"}
	if err := VerifyAgenticShellLaunch(env, path); err != nil {
		t.Fatalf("expected valid hook to pass VerifyAgenticShellLaunch, got %v", err)
	}
	noShimDir := strings.ReplaceAll(string(raw), "--shim-dir", "--other-flag")
	if err := os.WriteFile(path, []byte(noShimDir), 0600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyAgenticShellLaunch(env, path); err == nil || !strings.Contains(err.Error(), "PreToolUse hook") {
		t.Fatalf("expected VerifyAgenticShellLaunch to reject hook without --shim-dir, got %v", err)
	}
}

func TestDirectProviderHomePersistsAcrossLaunchesButNotBranches(t *testing.T) {
	d, l := directFixture(t, "persist-state")
	for i, id := range []string{"first", "resumed"} {
		l.DispatchID = fmt.Sprintf("d%d", i)
		l.SessionID = id
		if _, err := d.Run(context.Background(), l); err != nil {
			t.Fatal(err)
		}
	}
	files, err := filepath.Glob(filepath.Join(d.Root, "provider-state", "*", "home", "resume-token"))
	if err != nil || len(files) != 1 {
		t.Fatalf("provider state not shared across launches: %v %v", files, err)
	}
	data, err := os.ReadFile(files[0])
	if err != nil || string(data) != "first" {
		t.Fatal("resumed provider lost prior state", string(data), err)
	}
	l.DispatchID, l.SessionID, l.BranchInstance = "d-other", "other", "other-branch"
	if _, err = d.Run(context.Background(), l); err != nil {
		t.Fatal(err)
	}
	files, err = filepath.Glob(filepath.Join(d.Root, "provider-state", "*", "home", "resume-token"))
	if err != nil || len(files) != 2 {
		t.Fatal("branch provider states were shared", files, err)
	}
}
