package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	case "shims":
		for _, name := range []string{"go", "gofmt", "git", "cargo"} {
			cmd := exec.Command(name, "--version")
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 77 || !strings.Contains(string(out), "broker dispatch") {
				os.Exit(29)
			}
		}
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
	st, e := os.Stat(filepath.Join(out.LogDirectory, "config.json"))
	if e != nil || st.Mode().Perm() != 0600 {
		t.Fatal(st, e)
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
	d.Environment["PATH"] = "/usr/bin:/bin:/usr/local/bin"
	if _, err := d.Run(context.Background(), l); err != nil {
		t.Fatal(err)
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
