package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/xleelz/fae/processowner"
)

func tuiState(d *Daemon) TUIStatus {
	if d.tui != nil {
		if s := d.tui.state.Load(); s != nil {
			return *s
		}
	}
	return TUIStatus{}
}

func awaitTUI(t *testing.T, d *Daemon, what string, ok func(TUIStatus) bool) TUIStatus {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if s := tuiState(d); ok(s) {
			return s
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("TUI child never %s: %+v", what, tuiState(d))
	return TUIStatus{}
}

func buildTUIChild(t *testing.T, dir string) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	out := filepath.Join(dir, TUIBinaryName)
	gobin, err := exec.LookPath("go")
	if err != nil {
		gobin = filepath.Join(runtime.GOROOT(), "bin", "go")
	}
	cmd := exec.Command(gobin, "build", "-o", out, "./cmd/fae-tui")
	cmd.Dir = filepath.Dir(filepath.Dir(file))
	if raw, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fae-tui: %v\n%s", err, raw)
	}
	return out
}

func TestTUIChildLifecycle(t *testing.T) {
	cfg, ctx := fixture(t)
	fastUISupervision(t, 10)
	cfg.TUI = &TUIConfig{
		Binary:      buildTUIChild(t, filepath.Dir(cfg.Workspace)),
		AllowWrites: true,
		Refresh:     500 * time.Millisecond,
	}
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	defer func() {
		if !closed {
			stop(t, d)
		}
	}()

	first := awaitTUI(t, d, "became healthy", func(s TUIStatus) bool { return s.Health == "healthy" && s.PID != nil })
	if !first.Enabled || first.Restarts != 0 || !first.WritesEnabled || first.Binary != cfg.TUI.Binary {
		t.Fatalf("TUI status %+v", first)
	}

	records, err := processowner.Inventory(filepath.Join(d.layout.Run, "tui-processes"))
	if err != nil {
		t.Fatal(err)
	}
	foundRole := false
	for _, r := range records {
		foundRole = foundRole || (r.PID == *first.PID && r.Role == "tui")
	}
	if !foundRole {
		t.Fatalf("TUI child %d not inventoried with role tui: %+v", *first.PID, records)
	}

	reply, err := call(t, d, "Status", map[string]any{}, "")
	if err != nil {
		t.Fatal(err)
	}
	var status struct {
		TUI map[string]any `json:"tui"`
	}
	if err = json.Unmarshal(reply.Result, &status); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"enabled", "pid", "health", "restarts", "writes_enabled"} {
		if _, ok := status.TUI[field]; !ok {
			t.Fatalf("status tui lacks %s: %v", field, status.TUI)
		}
	}

	// Kill the TUI child while the daemon is up: it must restart automatically
	// and stay running while the daemon executes tasks.
	if err = syscall.Kill(*first.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	authorize(t, d)
	if _, err = call(t, d, "Admit", AdmitParams{ID: "during-tui-restart", Tree: "tree", AuthorizingMessage: "message"}, "during-tui-restart"); err != nil {
		t.Fatal(err)
	}
	second := awaitTUI(t, d, "restarted", func(s TUIStatus) bool {
		return s.Health == "healthy" && s.PID != nil && *s.PID != *first.PID
	})
	if second.Restarts != 1 || !strings.Contains(second.LastExit, "killed") {
		t.Fatalf("restart status %+v", second)
	}
	awaitState(t, d, "during-tui-restart", "Done")

	// Stopping the daemon stops and reaps the TUI child.
	closed = true
	stop(t, d)
	if err = syscall.Kill(*second.PID, 0); err != syscall.ESRCH {
		t.Fatalf("TUI child %d survived daemon shutdown: %v", *second.PID, err)
	}
	if s := tuiState(d); s.Health != "stopped" || s.PID != nil {
		t.Fatalf("stopped TUI status %+v", s)
	}
	active, _ := filepath.Glob(filepath.Join(d.layout.Run, "tui-processes", "process-*.json"))
	if len(active) != 0 {
		t.Fatalf("unretired TUI process records %v", active)
	}
}

func TestTUIConfigValidationAndMissingBinary(t *testing.T) {
	root := t.TempDir()
	ws := filepath.Join(root, "ws")
	if err := os.Mkdir(ws, 0700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(ws)
	home := filepath.Join(root, "instances")
	if _, err := Start(context.Background(), Config{Root: home, Instance: "tuibad", Workspace: ws, DoltBinary: "/nonexistent/dolt", TUI: &TUIConfig{Binary: "relative/fae-tui"}}); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("relative TUI binary accepted: %v", err)
	}
	if _, err := Start(context.Background(), Config{Root: home, Instance: "tuibad", Workspace: ws, DoltBinary: "/nonexistent/dolt", TUI: &TUIConfig{Refresh: 50 * time.Millisecond}}); err == nil || !strings.Contains(err.Error(), "250ms..10m") {
		t.Fatalf("invalid TUI refresh accepted: %v", err)
	}

	cfg, ctx := fixture(t)
	cfg.TUI = &TUIConfig{}
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	sibling := DefaultTUIBinary(d.binaryPath)
	if s := tuiState(d); s.Health != "missing-binary" || s.PID != nil || s.Binary != sibling || filepath.Base(sibling) != "fae-tui" {
		t.Fatalf("missing binary status %+v", s)
	}
	stop(t, d)
	logRaw, _ := os.ReadFile(d.layout.Log)
	if !bytes.Contains(logRaw, []byte(sibling+" is missing")) {
		t.Fatalf("missing TUI binary not warned in supervision log")
	}
}

func TestTUIHerdrPaneMarkerAndCleanup(t *testing.T) {
	tmp := t.TempDir()
	binDir := filepath.Join(tmp, "bin")
	logFile := filepath.Join(tmp, "herdr-core.log")
	if err := os.MkdirAll(binDir, 0700); err != nil {
		t.Fatal(err)
	}

	herdrScript := `#!/usr/bin/env bash
set -euo pipefail
echo "herdr-core $*" >> "` + logFile + `"
cmd="${1:-}"
case "$cmd" in
  run)
    printf '{"address":"t7","name":"fae-tui-default","state":"live","turn":0}\n'
    ;;
  get)
    printf '{"address":"t7","pid":12345,"tty":"/dev/null","state":"live"}\n'
    ;;
  kill)
    printf '{"address":"%s","state":"finished","reason":"killed"}\n' "${2:-t7}"
    ;;
  rm)
    printf '{"removed":"%s"}\n' "${2:-t7}"
    ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(binDir, "herdr-core"), []byte(herdrScript), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	root := filepath.Join(tmp, "instance-root")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(root, "tui-herdr-pane.json")
	staleMarker := `{"pane_id":"t-stale-1","tty":"/dev/pts/99"}`
	if err := os.WriteFile(markerPath, []byte(staleMarker), 0600); err != nil {
		t.Fatal(err)
	}

	tc := &tuiChild{
		herdrMarkerPath: markerPath,
	}
	tc.reapStaleHerdrPane(root)
	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Fatalf("expected stale tui-herdr-pane.json to be removed, stat err = %v", err)
	}
	rawLog, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rawLog), "herdr-core kill t-stale-1 --json") || !strings.Contains(string(rawLog), "herdr-core rm t-stale-1 --json") {
		t.Fatalf("expected stale herdr-core kill and rm in log, got:\n%s", rawLog)
	}

	// Verify acquireHerdrTTY provisions a terminal via herdr-core run & get and writes marker.
	tc.allowTestHerdrPane = true
	d := &Daemon{
		config: Config{Instance: "default", Workspace: root},
	}
	d.layout.Root = root
	ttyPath := tc.acquireHerdrTTY(d)
	if ttyPath != "/dev/null" || tc.herdrPane != "t7" || tc.herdrShellPID != 12345 {
		t.Fatalf("unexpected acquireHerdrTTY result: tty=%q pane=%q pid=%d", ttyPath, tc.herdrPane, tc.herdrShellPID)
	}
	rawMarker, err := os.ReadFile(markerPath)
	if err != nil || !strings.Contains(string(rawMarker), `"pane_id":"t7"`) {
		t.Fatalf("expected tui-herdr-pane.json with t7, got %q (err=%v)", rawMarker, err)
	}

	// Verify releaseHerdrTTY kills and removes the terminal without signaling foreign shell PID.
	tc.herdrShellPID = os.Getpid()
	tc.releaseHerdrTTY()
	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Fatalf("expected tui-herdr-pane.json removed on releaseHerdrTTY, stat err = %v", err)
	}
	rawLog2, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"herdr-core run --bg --no-parent --no-notify --keep --name fae-tui-default --cwd " + root + " --json",
		"herdr-core get t7 --json",
		"herdr-core kill t7 --json",
		"herdr-core rm t7 --json",
	} {
		if !strings.Contains(string(rawLog2), want) {
			t.Fatalf("expected %q in log, got:\n%s", want, rawLog2)
		}
	}
}
