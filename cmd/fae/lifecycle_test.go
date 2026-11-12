package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/controller"
	"github.com/xleelz/fae/daemon"
	"github.com/xleelz/fae/processowner"
)

// recordingDaemon answers every RPC with result and records the requests.
func recordingDaemon(t *testing.T, instance string, result any) (string, chan api.Request) {
	t.Helper()
	home := shortHome(t)
	layout, err := daemon.Paths(home, instance)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(layout.Run, 0700); err != nil {
		t.Fatal(err)
	}
	token, err := api.Credential(layout.Credential)
	if err != nil {
		t.Fatal(err)
	}
	requests := make(chan api.Request, 4)
	server, err := api.New(token, func(_ context.Context, r api.Request) (any, error) {
		requests <- r
		return result, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = server.Listen(layout.Socket); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve() }()
	t.Cleanup(func() { server.Shutdown(context.Background()); <-done })
	return home, requests
}
func params(t *testing.T, r api.Request) map[string]any {
	t.Helper()
	var p map[string]any
	if err := json.Unmarshal(r.Params, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestStatusTasksFlagReachesDaemon(t *testing.T) {
	home, requests := recordingDaemon(t, "st", map[string]bool{"ok": true})
	for _, tt := range []struct {
		args  []string
		tasks bool
	}{{[]string{"status"}, false}, {[]string{"status", "--tasks"}, true}} {
		if err := execute(context.Background(), append(tt.args, "--home", home, "--instance", "st"), &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
		if r := <-requests; r.Method != "Status" || params(t, r)["tasks"] != tt.tasks {
			t.Fatalf("%v: %s %s", tt.args, r.Method, r.Params)
		}
	}
	var out bytes.Buffer
	if err := execute(context.Background(), []string{"version", "--json"}, &out); err != nil || !json.Valid(out.Bytes()) || !bytes.Contains(out.Bytes(), []byte(`"commit"`)) {
		t.Fatal(out.String(), err)
	}
}

func TestRunAndStartWorkItemSubmissions(t *testing.T) {
	home, requests := recordingDaemon(t, "sub", map[string]any{"admitted": 1, "updated": 0, "refused": 0})
	run := func(args ...string) (api.Request, string, error) {
		var out bytes.Buffer
		err := execute(context.Background(), append(args, "--home", home, "--instance", "sub", "--authorizing-message", "m"), &out)
		if err != nil {
			return api.Request{}, out.String(), err
		}
		return <-requests, out.String(), nil
	}
	r, _, err := run("run", "--prompt", "x", "--id", "t1")
	if err != nil || r.Method != "Admit" || params(t, r)["mode"] != nil || r.Key != "t1" {
		t.Fatalf("instance default mode %s %v", r.Params, err)
	}
	r, _, err = run("run", "--prompt", "x", "--mode", "normal", "--dry-run", "--review-destination-branch", "main", "--push-namespace", "refs/fae")
	if p := params(t, r); err != nil || p["mode"] != "Normal" || p["commit_external"] != nil || p["dry_run"] != true || p["destination"] != "main" {
		t.Fatalf("dry run %s %v", r.Params, err)
	}
	r, _, err = run("run", "--prompt", "x", "--commit-external", "--destination", "main", "--push-namespace", "refs/fae")
	if p := params(t, r); err != nil || p["mode"] != "Normal" || p["commit_external"] != true || p["dry_run"] != nil {
		t.Fatalf("live %s %v", r.Params, err)
	}
	for _, args := range [][]string{
		{"run", "--prompt", "x", "--dry-run", "--commit-external"}, {"run", "--prompt", "x", "--mode", "local", "--dry-run"},
		{"run", "--prompt", "x", "--mode", "remote"}, {"run", "--prompt", "x", "--destination", "a", "--review-destination-branch", "b"},
		{"run", "--prompt", "x", "--content", "y"}, {"start-work-item"}, {"run", "--prompt", "x", "--source-ref", "s"},
		// FAE-CLI-011: Normal names exactly one of --dry-run or --commit-external.
		{"run", "--prompt", "x", "--mode", "normal"}, {"run", "--prompt", "x", "--mode", "normal", "--review-destination-branch", "main", "--push-namespace", "refs/fae"},
	} {
		if _, _, err = run(args...); exitCode(err) != 2 {
			t.Fatalf("%v: %v", args, err)
		}
	}
	r, out, err := run("start-work-item", "--id", "item-1", "--title", "Queued", "--content", "acceptance text", "--priority", "40", "--workflow", "custom", "--source-ref", "doc#3")
	if err != nil || r.Method != "BacklogAdmit" {
		t.Fatal(r.Method, err)
	}
	var payload daemon.BacklogParams
	if err = json.Unmarshal(r.Params, &payload); err != nil || len(payload.Items) != 1 || payload.AuthorizingMessage != "m" {
		t.Fatalf("payload %s %v", r.Params, err)
	}
	if i := payload.Items[0]; i.ID != "item-1" || i.Title != "Queued" || i.Content != "acceptance text" || i.Priority != 40 || i.WorkflowID != "custom" || i.Source != "doc#3" {
		t.Fatalf("item %+v", i)
	}
	var body map[string]any
	if err = json.Unmarshal([]byte(out), &body); err != nil || body["id"] != "item-1" || body["admitted"] != float64(1) {
		t.Fatalf("output %s %v", out, err)
	}
}

func TestResumeAndShowCLI(t *testing.T) {
	shown := map[string]any{"run_id": "r1", "tasks": []map[string]any{{"id": "a", "state": "Escalated", "stage": "Escalated", "run_id": "r1", "workflow_id": "default", "workflow_revision": 1, "escalation": "AttemptCap", "disposition": map[string]any{"mode": "Local"}, "latest_artifacts": []map[string]any{{"stage": "spec", "artifact_id": "sha-1", "role": "spec"}}}}}
	home, requests := recordingDaemon(t, "rs", shown)
	for _, tt := range []struct {
		args []string
		want map[string]any
	}{
		{[]string{"resume", "--authorizing-message", "m"}, map[string]any{"authorizing_message": "m"}},
		{[]string{"resume", "--run-id", "r1", "--authorizing-message", "m"}, map[string]any{"run_id": "r1", "authorizing_message": "m"}},
		{[]string{"resume", "--task", "a", "--authorizing-message", "m"}, map[string]any{"task": "a", "authorizing_message": "m"}},
	} {
		if err := execute(context.Background(), append(tt.args, "--home", home, "--instance", "rs"), &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
		r := <-requests
		if p := params(t, r); r.Method != "ResumeRun" || len(p) != len(tt.want) || p["run_id"] != tt.want["run_id"] || p["task"] != tt.want["task"] {
			t.Fatalf("%v: %s %s", tt.args, r.Method, r.Params)
		}
	}
	var out bytes.Buffer
	if err := execute(context.Background(), []string{"show", "--run-id", "r1", "--home", home, "--instance", "rs"}, &out); err != nil {
		t.Fatal(err)
	}
	if r := <-requests; r.Method != "Show" || params(t, r)["run_id"] != "r1" {
		t.Fatal(r.Method, string(r.Params))
	}
	for _, want := range []string{"run r1: 1 task(s)", "task a [Escalated]", "escalation: AttemptCap", "spec: sha-1"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("text snapshot lacks %q: %s", want, out.String())
		}
	}
	if err := execute(context.Background(), []string{"show", "--home", home, "--instance", "rs"}, &bytes.Buffer{}); exitCode(err) != 2 {
		t.Fatal(err)
	}
}

func TestServeLimitFlagsOverrideLiveConfiguration(t *testing.T) {
	live := daemon.LiveConfig{PaneLimit: 60}
	applyServeLimits(&live, 0, 0)
	if live.PaneLimit != 60 || live.HeavyCapacity != 2 {
		t.Fatalf("defaults %+v", live)
	}
	live = daemon.LiveConfig{PaneLimit: 60, HeavyCapacity: 5}
	applyServeLimits(&live, 12, 0)
	if live.PaneLimit != 12 || live.HeavyCapacity != 5 {
		t.Fatalf("pane override %+v", live)
	}
	applyServeLimits(&live, 0, 3)
	if live.PaneLimit != 12 || live.HeavyCapacity != 3 {
		t.Fatalf("heavy override %+v", live)
	}
}

// stubServe replaces a foreground serve's daemon start: it records the
// configuration serve resolved and refuses, so nothing starts. It also
// captures the configured-value warnings.
func stubServe(t *testing.T) (*daemon.Config, *bytes.Buffer, *int) {
	t.Helper()
	var got daemon.Config
	var warnings bytes.Buffer
	starts := new(int)
	oldStart, oldWarnings := startDaemon, configWarnings
	startDaemon = func(_ context.Context, c daemon.Config) (*daemon.Daemon, error) {
		got, *starts = c, *starts+1
		return nil, errors.New("stub start")
	}
	configWarnings = &warnings
	t.Cleanup(func() { startDaemon, configWarnings = oldStart, oldWarnings })
	return &got, &warnings, starts
}

// FAE-CP-012: serve --inline-limit configures the plane's row-column limit
// (default 64 KiB). A value that does not parse is a usage error before
// start; one outside 4..64 KiB falls back to the default with a warning.
func TestServeInlineLimitFlag(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "instances")
	config, warnings, starts := stubServe(t)
	for _, value := range []string{"x", "4k", "65536.0"} {
		err := execute(context.Background(), []string{"--home", home, "serve", "--workspace", root, "--adapter", "scripted", "--inline-limit", value}, &bytes.Buffer{})
		if exitCode(err) != 2 || !strings.Contains(err.Error(), "inline-limit") {
			t.Fatalf("--inline-limit %s: code %d %v", value, exitCode(err), err)
		}
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) || *starts != 0 {
		t.Fatalf("refused serve created %s (%v) or started %d", home, err, *starts)
	}
	for value, want := range map[string]int{"100": 65536, "4095": 65536, "65537": 65536, "4096": 4096, "8192": 8192} {
		warnings.Reset()
		err := execute(context.Background(), []string{"--home", home, "serve", "--workspace", root, "--adapter", "scripted", "--foreground", "--inline-limit", value}, &bytes.Buffer{})
		if !strings.Contains(fmt.Sprint(err), "stub start") || config.InlineLimit != want {
			t.Fatalf("--inline-limit %s: %v, limit %d, want %d", value, err, config.InlineLimit, want)
		}
		if warned := strings.Contains(warnings.String(), "--inline-limit "+value+" is out of range (4096..65536 bytes); using the default 65536"); warned != (want != 8192 && want != 4096) {
			t.Fatalf("--inline-limit %s warning %q", value, warnings.String())
		}
	}
	var out bytes.Buffer
	if err := execute(context.Background(), []string{"serve", "--help", "--json"}, &out); err != nil || !strings.Contains(out.String(), `"name": "inline-limit"`) || !strings.Contains(out.String(), `"default": "65536"`) {
		t.Fatalf("serve help %v %s", err, out.String())
	}
}

// REQUIREMENTS.md conventions, FAE-CLI-010: a serve flag carrying an Appendix
// A configurable default falls back to the default, with a warning naming the
// flag, the value and the default, when its value is empty or out of range.
// A value that does not parse stays a usage error (exit 2) and starts nothing.
func TestServeConfiguredValuesFallBackToDefaults(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "instances")
	config, warnings, starts := stubServe(t)
	serve := func(flag string) error {
		warnings.Reset()
		*config = daemon.Config{}
		return execute(context.Background(), []string{"--home", home, "serve", "--workspace", root, "--adapter", "scripted", "--foreground", flag}, &bytes.Buffer{})
	}
	for _, tc := range []struct {
		flag, value, fallback string
		ok                    func(daemon.Config) bool
	}{
		{"dolt-stop-wait", "0s", "30s", func(c daemon.Config) bool { return c.DoltStopWait == 30*time.Second }},
		{"dolt-stop-wait", "-3s", "30s", func(c daemon.Config) bool { return c.DoltStopWait == 30*time.Second }},
		{"dolt-stop-wait", "", "30s", func(c daemon.Config) bool { return c.DoltStopWait == 30*time.Second }},
		{"message-max-age", "0s", "24h0m0s", func(c daemon.Config) bool { return c.MessageMaxAge == 24*time.Hour }},
		{"message-max-age", "-1s", "24h0m0s", func(c daemon.Config) bool { return c.MessageMaxAge == 24*time.Hour }},
		{"event-retention-age", "-1s", "24h0m0s", func(c daemon.Config) bool { return c.EventRetentionAge == 24*time.Hour }},
		{"event-retention-count", "0", "100000", func(c daemon.Config) bool { return c.EventRetentionCount == 100000 }},
		{"event-retention-count", "-5", "100000", func(c daemon.Config) bool { return c.EventRetentionCount == 100000 }},
		{"max-tasks-in-flight", "0", "-1", func(c daemon.Config) bool { return c.MaxInFlight == -1 }},
		{"max-tasks-in-flight", "2000000", "-1", func(c daemon.Config) bool { return c.MaxInFlight == -1 }},
		{"max-molecules", "-7", "-1", func(c daemon.Config) bool { return c.MaxMolecules == -1 }},
		{"max-workflow-molecules", "0", "-1", func(c daemon.Config) bool { return c.MaxWorkflowMolecules == -1 }},
		{"rearm-budget", "0", "3", func(c daemon.Config) bool { return c.RearmBudget == 3 }},
		{"rearm-window", "0s", "1h0m0s", func(c daemon.Config) bool { return c.RearmWindow == time.Hour }},
		{"dolt-port", "80", "0", func(c daemon.Config) bool { return c.Port == 0 }},
		{"dolt-port", "70000", "0", func(c daemon.Config) bool { return c.Port == 0 }},
		{"prophet-max-failovers", "9", "0", func(c daemon.Config) bool { return c.ProphetMaxFailovers == 0 }},
		{"prophet-max-failovers", "-2", "0", func(c daemon.Config) bool { return c.ProphetMaxFailovers == 0 }},
		{"inline-limit", "", "65536", func(c daemon.Config) bool { return c.InlineLimit == 65536 }},
		{"run-id", "", "backlog-drain", func(c daemon.Config) bool { return c.RunID == "backlog-drain" }},
		{"max-panes", "-5", "0", func(daemon.Config) bool { return true }},
		{"max-heavy-weight", "-1", "0", func(daemon.Config) bool { return true }},
	} {
		err := serve("--" + tc.flag + "=" + tc.value)
		want := "--" + tc.flag + " " + tc.value + " is out of range ("
		if tc.value == "" {
			want = "--" + tc.flag + " is empty"
		}
		if !strings.Contains(fmt.Sprint(err), "stub start") || !tc.ok(*config) || !strings.Contains(warnings.String(), want) || !strings.Contains(warnings.String(), "using the default "+tc.fallback+"\n") {
			t.Errorf("--%s=%q: %v, config %+v, warnings %q", tc.flag, tc.value, err, *config, warnings.String())
		}
	}
	// In-range values pass unchanged and unwarned.
	for flag, ok := range map[string]func(daemon.Config) bool{
		"--dolt-stop-wait=5s":          func(c daemon.Config) bool { return c.DoltStopWait == 5*time.Second },
		"--dolt-port=4000":             func(c daemon.Config) bool { return c.Port == 4000 },
		"--max-tasks-in-flight=4":      func(c daemon.Config) bool { return c.MaxInFlight == 4 },
		"--prophet-max-failovers=-1":   func(c daemon.Config) bool { return c.ProphetMaxFailovers == -1 },
		"--event-retention-count=1000": func(c daemon.Config) bool { return c.EventRetentionCount == 1000 },
		"--run-id=nightly":             func(c daemon.Config) bool { return c.RunID == "nightly" },
	} {
		if err := serve(flag); !strings.Contains(fmt.Sprint(err), "stub start") || !ok(*config) || warnings.Len() != 0 {
			t.Errorf("%s: %v, config %+v, warnings %q", flag, err, *config, warnings.String())
		}
	}
	*starts = 0
	for _, flag := range []string{"--dolt-stop-wait=soon", "--max-panes=1.5", "--rearm-budget=three", "--dolt-port=x", "--event-retention-age=1d"} {
		if err := serve(flag); exitCode(err) != 2 {
			t.Errorf("%s: exit %d %v, want usage 2", flag, exitCode(err), err)
		}
	}
	if *starts != 0 {
		t.Fatalf("a malformed value started serve %d times", *starts)
	}
	var out bytes.Buffer
	if err := execute(context.Background(), []string{"help"}, &out); err != nil || !strings.Contains(out.String(), "falls back to its default with a warning") {
		t.Fatalf("help does not state the fallback rule: %v %s", err, out.String())
	}
}

// FAE-CLI-001, FAE-CTL-002: fae maintenance stop and start exit 0 with JSON
// through the real controller client. Stop succeeds once the daemon has
// drained, stopped and its recorded process is proven dead; start once the
// runtime launcher started the marker's pinned launch and the daemon reports
// and accepts exactly that process.
func TestMaintenanceStopAndStartCLIExitZero(t *testing.T) {
	proves(t, "maintenance stop", 0)
	proves(t, "maintenance start", 0)
	home, dir := shortHome(t), shortHome(t) // dir is the private launcher directory
	layout, err := daemon.Paths(home, "maint")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(layout.Run, 0700); err != nil {
		t.Fatal(err)
	}
	// The pinned launch stands in for the daemon: it records its PID and sleeps.
	shell, err := os.ReadFile("/bin/sh")
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "daemon")
	if err = os.WriteFile(binary, shell, 0700); err != nil {
		t.Fatal(err)
	}
	hash, err := controller.HashFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=/usr/bin:/bin"}
	socket := filepath.Join(dir, "launcher.sock")
	marker := controller.Marker{ID: "maint", Message: "m", Reason: "planned", State: "stopped", OldPID: 99999999, OldBirth: "gone", Launcher: socket,
		Launch: controller.LaunchSpec{Binary: binary, Arguments: []string{"-c", "echo $$ > pid; exec sleep 60"}, Directory: dir, Environment: env, SHA256: hash}}
	launched := func() (int, string) {
		raw, err := os.ReadFile(filepath.Join(dir, "pid"))
		if err != nil {
			return 0, ""
		}
		pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
		birth, _ := processowner.Birth(pid)
		return pid, birth
	}
	t.Cleanup(func() {
		if pid, _ := launched(); pid > 1 {
			syscall.Kill(-pid, syscall.SIGKILL)
		}
	})
	// The daemon drains, then is gone after stop until the launch reports.
	var stopped atomic.Bool
	token, err := api.Credential(layout.Credential)
	if err != nil {
		t.Fatal(err)
	}
	server, err := api.New(token, func(ctx context.Context, r api.Request) (any, error) {
		if r.Method == "ControllerOperation" {
			var p daemon.ControllerOperationParams
			if err := json.Unmarshal(r.Params, &p); err != nil {
				return nil, err
			}
			if p.AuthorizingMessage != "m" || p.Operation.Options.Message != "m" {
				return nil, errors.New("unauthorized operation")
			}
			p.Operation.Authorization = controller.SignOperation(p.Operation, token)
			return controller.OperationRPC(ctx, socket, p.Operation)
		}
		var p controller.Params
		if err := json.Unmarshal(r.Params, &p); r.Method != "Controller" || err != nil {
			return nil, fmt.Errorf("unexpected %s %v", r.Method, err)
		}
		pid, birth := launched()
		switch {
		case p.Action == "prepare":
			m := marker
			return controller.Health{Maintenance: &m}, nil
		case p.Action == "stop":
			stopped.Store(true)
			return controller.Health{}, nil
		case (p.Action == "health" || p.Action == "accept") && pid > 1:
			return controller.Health{PID: pid, DoltPID: pid, ProcessBirth: birth, BinaryHash: hash, EnvironmentHash: controller.EnvironmentHash(env), DriveAlive: true, DriveRounds: 1, Consistent: true}, nil
		case p.Action == "health" && !stopped.Load():
			return controller.Health{}, nil
		}
		return nil, errors.New("daemon stopped")
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = server.Listen(layout.Socket); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve() }()
	t.Cleanup(func() { server.Shutdown(context.Background()); <-done })
	fae := func(args ...string) map[string]any {
		t.Helper()
		var out bytes.Buffer
		if err := execute(context.Background(), append([]string{"--home", home, "--instance", "maint", "--json", "maintenance"}, args...), &out); err != nil {
			t.Fatalf("maintenance %v: exit %d %v", args, exitCode(err), err)
		}
		var body map[string]any
		if err := json.Unmarshal(out.Bytes(), &body); err != nil {
			t.Fatalf("maintenance %v output is not JSON: %q", args, out.String())
		}
		return body
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() {
		served <- controller.ServeLauncher(ctx, socket, filepath.Join(layout.Root, "controller-maintenance.json"))
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-served; err != nil {
			t.Error(err)
		}
	})
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err = os.Stat(controller.LauncherKeyPath(socket)); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("launcher not ready", err)
		}
	}
	if body := fae("stop", "--note", "planned", "--launcher-socket", socket, "--authorizing-message", "m"); body["id"] != "maint" || body["state"] != "stopped" || body["old_pid"] != float64(99999999) {
		t.Fatalf("stop %v", body)
	}
	// The daemon recorded the marker at prepare; the launcher starts only what it authorizes.
	raw, _ := json.Marshal(marker)
	if err = os.WriteFile(filepath.Join(layout.Root, "controller-maintenance.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	body := fae("start", "--authorizing-message", "m")
	if pid, _ := launched(); pid <= 1 || body["pid"] != float64(pid) || body["accepted"] != true || body["binary_sha256"] != hash {
		t.Fatalf("start %v (launched pid %d)", body, pid)
	}
}

func TestControllerOperationPinsRelativeBinaryToCallerDirectory(t *testing.T) {
	home, requests := recordingDaemon(t, "relative", controller.OperationResult{ID: "operation", Done: true})
	dir := t.TempDir()
	t.Chdir(dir)
	var out bytes.Buffer
	err := execute(context.Background(), []string{"--home", home, "--instance", "relative", "--json", "roll", "--binary", "bin/fae", "--note", "planned", "--launcher-socket", "/private/launcher.sock", "--authorizing-message", "m"}, &out)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case request := <-requests:
		var params daemon.ControllerOperationParams
		if err := json.Unmarshal(request.Params, &params); err != nil {
			t.Fatal(err)
		}
		if request.Method != "ControllerOperation" || params.Operation.Options.Binary != filepath.Join(dir, "bin", "fae") {
			t.Fatalf("binary not pinned to caller directory: %+v", params)
		}
	case <-time.After(time.Second):
		t.Fatal("operation was not submitted")
	}
}
