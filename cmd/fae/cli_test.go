package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/daemon"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/engine"
)

func TestCLICodes(t *testing.T) {
	home := shortHome(t)
	for _, tt := range []struct {
		args []string
		code int
	}{
		{[]string{"unknown"}, 2},
		{[]string{"workflow-limit", "--max", "0", "--authorizing-message", "m"}, 1},
		{[]string{"workflow-limit", "--max", "1000001", "--authorizing-message", "m"}, 1},
		{[]string{"resolve-uncertain", "--id", "t", "--dispatch", "d", "--action", "bogus", "--authorizing-message", "m"}, 2},
		{[]string{"runtime-config", "--pane-limit", "20"}, 1},
		{[]string{"runtime-config", "--heavy-capacity", "-1", "--authorizing-message", "m"}, 2},
		{[]string{"runtime-config", "--tasks-in-flight", "-2", "--authorizing-message", "m"}, 2},
		{[]string{"runtime-config", "--host-thresholds", "null", "--authorizing-message", "m"}, 2},
		{[]string{"runtime-config", "--pane-limit", "1", "--authorizing-message", "m"}, 2},
		{[]string{"serve", "--workspace", home, "--adapter", "scripted", "--message-max-age", "invalid"}, 2},
		{[]string{"serve", "--workspace", home, "--adapter", "scripted", "--dolt-stop-wait", "soon"}, 2},
		{[]string{"serve", "--workspace", home, "--adapter", "direct"}, 2},
		{[]string{"serve", "--workspace", home, "--adapter", "direct", "--scenario", "file"}, 2},
		{[]string{"serve", "--workspace", home, "--adapter", "scripted", "--live-config", "file"}, 2}, {[]string{"run", "--priority", "bad"}, 2}, {[]string{"run", "--prompt", "x"}, 1},
		{[]string{"run", "--prompt", "x", "--priority", "9", "--authorizing-message", "m"}, 1},
		{[]string{"serve", "--workspace", home}, 2},
		{[]string{"serve", "--workspace", home, "--adapter", "bogus"}, 2},
		{[]string{"serve", "--workspace", home, "--adapter", "scripted", "--default-mode", "normal"}, 2},
		{[]string{"serve", "--workspace", home, "--adapter", "scripted", "--default-mode", "bogus"}, 2},
		{[]string{"serve", "--workspace", home, "--adapter", "scripted", "--max-panes", "5"}, 2},
		{[]string{"serve", "--workspace", home, "--adapter", "scripted", "--max-heavy-weight", "3"}, 2},
		{[]string{"serve", "--workspace", home, "--adapter", "process", "--max-panes", "5", "--live-config", "x"}, 2},
		{[]string{"serve", "--workspace", home, "--adapter", "multiplexer", "--max-heavy-weight", "x", "--live-config", "x"}, 2},
		{[]string{"status", "--home", home}, 3},
		{[]string{"message", "--text", "x", "--file", "missing"}, 2},
	} {
		err := execute(context.Background(), tt.args, &bytes.Buffer{})
		if err == nil || exitCode(err) != tt.code {
			t.Errorf("%v: %v code=%d want=%d", tt.args, err, exitCode(err), tt.code)
		}
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatal("unreachable read created instance files")
	}
}
func TestCLI(t *testing.T) {
	home := shortHome(t)
	layout, err := daemon.Paths(home, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(layout.Run, 0700); err != nil {
		t.Fatal(err)
	}
	credential, err := api.Credential(layout.Credential)
	if err != nil {
		t.Fatal(err)
	}
	requests := make(chan api.Request, 8)
	server, err := api.New(credential, func(_ context.Context, r api.Request) (any, error) {
		requests <- r
		return map[string]any{"ok": true}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = server.Listen(layout.Socket); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve() }()
	defer func() {
		server.Shutdown(context.Background())
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	commands := []struct {
		args   []string
		method string
	}{
		{[]string{"schedule", "create", "--home", home, "--instance", "demo", "--id", "loop", "--role", "oracle", "--prompt", "continue", "--max-iterations", "5", "--authorizing-message", "m"}, "Schedule"},
		{[]string{"runtime-config", "--home", home, "--instance", "demo", "--pane-limit", "20", "--authorizing-message", "m"}, "RuntimeConfig"},
		{[]string{"resolve-uncertain", "--home", home, "--instance", "demo", "--id", "t", "--dispatch", "d", "--action", "redispatch", "--authorizing-message", "m"}, "ResolveUncertain"},
		{[]string{"resolve-uncertain", "--home", home, "--instance", "demo", "--id", "t", "--dispatch", "d", "--action", "accept", "--authorizing-message", "m"}, "ResolveUncertain"},
		{[]string{"runtime-config", "--home", home, "--instance", "demo", "--heavy-capacity", "3", "--tasks-in-flight", "5", "--authorizing-message", "m"}, "RuntimeConfig"},
		{[]string{"--home", home, "--instance", "demo", "status", "--json"}, "Status"},
		{[]string{"provider-health", "--home", home, "--instance", "demo"}, "ProviderHealth"},
		{[]string{"provider-log", "--home", home, "--instance", "demo", "--after", "7", "--limit", "12"}, "ProviderObservations"},
		{[]string{"message", "--home", home, "--instance", "demo", "--text", "Please admit work", "--id", "m"}, "Message"},
		{[]string{"run", "--home", home, "--instance", "demo", "--prompt", "literal $(touch nope)", "--id", "t", "--authorizing-message", "m"}, "Admit"},
		{[]string{"board", "--home", home, "--instance", "demo", "--after", "task-7", "--limit", "12"}, "Board"},
		{[]string{"history", "--home", home, "--instance", "demo", "--task", "t", "--summary", "--after", "7", "--limit", "12"}, "History"},
		{[]string{"shutdown", "--home", home, "--instance", "demo", "--authorizing-message", "m"}, "Shutdown"},
	}
	for _, tt := range commands {
		var out bytes.Buffer
		if err := execute(context.Background(), tt.args, &out); err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		if err := json.Unmarshal(out.Bytes(), &body); err != nil || body["ok"] != true {
			t.Fatalf("bad JSON %s %v", out.String(), err)
		}
		r := <-requests
		if r.Method != tt.method {
			t.Fatal(r.Method)
		}
		if r.Method == "Board" || r.Method == "History" || r.Method == "ProviderObservations" {
			var params map[string]any
			if err := json.Unmarshal(r.Params, &params); err != nil {
				t.Fatal(err)
			}
			if params["limit"] != float64(12) {
				t.Fatal(params)
			}
			if r.Method == "Board" && params["after"] != "task-7" {
				t.Fatal(params)
			}
			if r.Method == "History" && (params["after"] != float64(7) || params["summary"] != true || params["id"] != "t") {
				t.Fatal(params)
			}
		}
		if r.Method == "Admit" {
			var p daemon.AdmitParams
			if err = json.Unmarshal(r.Params, &p); err != nil {
				t.Fatal(err)
			}
			// An unstated mode selects the instance default disposition (serve --default-mode).
			if p.Content != "literal $(touch nope)" || p.Priority != 10 || p.Mode != "" || r.Key != "t" {
				t.Fatalf("bad admission %+v", p)
			}
		}
	}
}
func TestCLIInputLimits(t *testing.T) {
	file := filepath.Join(t.TempDir(), "input")
	if err := os.WriteFile(file, []byte(strings.Repeat("x", (1<<20)+1)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := inputText("", file); err == nil {
		t.Fatal("oversized file accepted")
	}
	if _, err := inputText("", "missing"); err == nil {
		t.Fatal("missing file accepted")
	}
	if _, err := inputText("x", "file"); exitCode(err) != 2 {
		t.Fatal(err)
	}
}
func TestCLIReadCredentialDoesNotCreate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if _, err := readCredential(path); !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("created credential")
	}
}

func TestCLIDefaultContent(t *testing.T) {
	for _, verb := range []string{"default", "scenario"} {
		var out bytes.Buffer
		if err := execute(context.Background(), []string{"workflow", verb, "--json"}, &out); err != nil {
			t.Fatal(err)
		}
		if !json.Valid(out.Bytes()) {
			t.Fatal("invalid default JSON")
		}
	}
}

func TestCLIStream(t *testing.T) {
	home := shortHome(t)
	layout, err := daemon.Paths(home, "s")
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
	server, _ := api.New(token, func(context.Context, api.Request) (any, error) { return nil, nil })
	server.SetEventStream(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("after") != "7" {
			t.Error("cursor not forwarded")
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(`{"sequence":8,"kind":"transition"}` + "\n"))
	})
	if err = server.Listen(layout.Socket); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve() }()
	defer func() { server.Shutdown(context.Background()); <-done }()
	var out bytes.Buffer
	if err = execute(context.Background(), []string{"events", "--home", home, "--instance", "s", "--after", "7", "--follow"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"sequence":8`) {
		t.Fatal(out.String())
	}
}

// FAE-CLI-001: help needs no home, and an explicit --home needs no $HOME.
// Only a command that falls back to the default home refuses without it.
func TestHelpAndExplicitHomeNeedNoHomeVariable(t *testing.T) {
	empty := shortHome(t)
	t.Setenv("HOME", "")
	os.Unsetenv("HOME")
	for _, tc := range []struct {
		args []string
		code int
	}{
		{[]string{"help"}, 0},
		{[]string{"help", "--json"}, 0},
		{[]string{"status", "--help"}, 0},
		{[]string{"serve", "-h", "--json"}, 0},
		{[]string{"maintenance", "stop", "--help"}, 0},
		{[]string{"backlog", "edge", "--help"}, 0},
		{[]string{"backlog", "--help"}, 0},
		{[]string{"--home", empty, "status"}, 3},
		{[]string{"status", "--home", empty}, 3},
		{[]string{"status", "--home=" + empty}, 3},
		{[]string{"--home", empty, "backlog", "edge", "add", "--from", "a", "--to", "b", "--authorizing-message", "m"}, 3},
		{[]string{"status"}, 1},
		{[]string{"backlog", "show"}, 1},
	} {
		var out bytes.Buffer
		err := execute(context.Background(), tc.args, &out)
		if exitCode(err) != tc.code && (tc.code != 0 || err != nil) {
			t.Errorf("%v without $HOME: exit %d %v, want %d", tc.args, exitCode(err), err, tc.code)
		}
		if tc.code == 0 && (err != nil || out.Len() == 0) {
			t.Errorf("%v without $HOME: %v, help %q", tc.args, err, out.String())
		}
		if tc.code == 1 && !strings.Contains(fmt.Sprint(err), "HOME") {
			t.Errorf("%v without $HOME: %v does not name $HOME", tc.args, err)
		}
	}
	if entries, err := os.ReadDir(empty); err != nil || len(entries) != 0 {
		t.Fatal("unreachable commands created instance files", entries, err)
	}
}

// REQUIREMENTS.md conventions: a flag carrying a configurable default falls
// back to it when empty, out of range or not finite, and warns naming the
// flag, the value and the default; a value that does not parse fails to
// parse. The watchdog applies it to its Appendix A bounds.
func TestConfigurableFlagsFallBackOnlyOnEmptyOrOutOfRange(t *testing.T) {
	var warnings bytes.Buffer
	old := configWarnings
	configWarnings = &warnings
	t.Cleanup(func() { configWarnings = old })
	ranges := map[string]valueRange{
		"fraction": {"a fraction strictly between 0 and 1", func(v any) bool { x := v.(float64); return x > 0 && x < 1 }},
		"count":    positiveCount,
		"absent":   positiveCount, // not defined on the flag set: ignored
	}
	parse := func(args ...string) (float64, int, error) {
		warnings.Reset()
		f := flag.NewFlagSet("t", flag.ContinueOnError)
		f.SetOutput(&bytes.Buffer{})
		fraction, count := f.Float64("fraction", 0.8, ""), f.Int("count", 3, "")
		fallBack := configurableFlags(f, ranges)
		err := f.Parse(args)
		if err == nil {
			fallBack()
		}
		return *fraction, *count, err
	}
	for _, tc := range []struct {
		args     []string
		fraction float64
		count    int
		warning  string
	}{
		{[]string{"--fraction", "NaN"}, 0.8, 3, "--fraction NaN is out of range (a fraction strictly between 0 and 1); using the default 0.8\n"},
		{[]string{"--fraction", "+Inf"}, 0.8, 3, "--fraction +Inf is out of range"},
		{[]string{"--fraction", "1.5"}, 0.8, 3, "--fraction 1.5 is out of range"},
		{[]string{"--fraction="}, 0.8, 3, "--fraction is empty; using the default 0.8\n"},
		{[]string{"--count", "0"}, 0.8, 3, "--count 0 is out of range (a positive integer); using the default 3\n"},
		{[]string{"--count", "-2", "--fraction", "0.5"}, 0.5, 3, "--count -2 is out of range"},
		{[]string{"--count", "7", "--fraction", "0.25"}, 0.25, 7, ""},
		{[]string{"--count", "0", "--count", "5"}, 0.8, 5, ""},
	} {
		fraction, count, err := parse(tc.args...)
		if err != nil || fraction != tc.fraction || count != tc.count || tc.warning == "" && warnings.Len() != 0 || !strings.Contains(warnings.String(), tc.warning) {
			t.Errorf("%v: %v fraction %v count %d warnings %q", tc.args, err, fraction, count, warnings.String())
		}
	}
	for _, args := range [][]string{{"--fraction", "x"}, {"--count", "1.5"}, {"--count", "three"}} {
		if _, _, err := parse(args...); err == nil {
			t.Errorf("%v parsed", args)
		}
	}
	// The watchdog's bounds: out of range warns and continues (here to the
	// unreachable daemon, 3); a value that does not parse stays usage (2).
	home := shortHome(t)
	for args, code := range map[string]int{"--fs-threshold NaN": 3, "--fs-threshold 1.5": 3, "--rearm-budget 0": 3, "--stall 0s": 3, "--fs-threshold x": 2, "--stall soon": 2} {
		warnings.Reset()
		err := execute(context.Background(), append([]string{"--home", home, "watchdog", "--authorizing-message", "m"}, strings.Fields(args)...), &bytes.Buffer{})
		if exitCode(err) != code || code == 3 && !strings.Contains(warnings.String(), "--"+strings.Fields(args)[0][2:]+" "+strings.Fields(args)[1]+" is out of range") {
			t.Errorf("watchdog %s: exit %d %v, warnings %q", args, exitCode(err), err, warnings.String())
		}
	}
}

func TestPreVerbJSONFlags(t *testing.T) {
	for _, tc := range []struct {
		args     []string
		wantJSON bool
	}{
		{[]string{"--json", "version"}, true},
		{[]string{"-json", "version"}, true},
		{[]string{"--json=true", "version"}, true},
		{[]string{"-json=true", "version"}, true},
		{[]string{"--json=false", "version"}, false},
		{[]string{"-json=false", "version"}, false},
		{[]string{"--json", "--json=false", "version"}, false},
		{[]string{"--json=false", "-json=true", "version"}, true},
		{[]string{"-json", "--help"}, true},
		{[]string{"--json=true", "--help"}, true},
		{[]string{"--json=false", "--help"}, false},
	} {
		var out bytes.Buffer
		if err := execute(context.Background(), tc.args, &out); err != nil {
			t.Fatalf("%v: unexpected error: %v", tc.args, err)
		}
		gotJSON := json.Valid(out.Bytes())
		if gotJSON != tc.wantJSON {
			t.Fatalf("%v: json.Valid=%v, want %v (output=%q)", tc.args, gotJSON, tc.wantJSON, out.String())
		}
	}
}

func TestResolveUncertainTaskFlagAndAutoDispatchLookup(t *testing.T) {
	home := shortHome(t)
	layout, err := daemon.Paths(home, "unc")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(layout.Run, 0700); err != nil {
		t.Fatal(err)
	}
	credential, err := api.Credential(layout.Credential)
	if err != nil {
		t.Fatal(err)
	}

	var (
		mu          sync.Mutex
		dispatchID  = "disp-auto-99"
		lastResolve daemon.ResolveUncertainParams
	)
	server, err := api.New(credential, func(_ context.Context, r api.Request) (any, error) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case "TaskGet":
			if dispatchID == "" {
				return map[string]any{"id": "task-1", "state": "Parked"}, nil
			}
			return map[string]any{
				"id":        "task-1",
				"state":     "Parked",
				"in_flight": map[string]any{"id": dispatchID, "stage": "plan"},
			}, nil
		case "Status":
			return map[string]any{"tasks": []any{}}, nil
		case "ResolveUncertain":
			if err := json.Unmarshal(r.Params, &lastResolve); err != nil {
				return nil, err
			}
			return map[string]any{"ok": true, "id": lastResolve.ID, "dispatch": lastResolve.DispatchID, "action": lastResolve.Action}, nil
		default:
			return nil, fmt.Errorf("unexpected method %s", r.Method)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = server.Listen(layout.Socket); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve() }()
	defer func() {
		_ = server.Shutdown(context.Background())
		<-done
	}()

	// 1. --task with omitted --dispatch looks up in-flight dispatch automatically via TaskGet.
	var out bytes.Buffer
	err = execute(context.Background(), []string{
		"resolve-uncertain", "--home", home, "--instance", "unc",
		"--task", "task-1", "--action", "redispatch", "--authorizing-message", "auth-msg",
	}, &out)
	if err != nil {
		t.Fatalf("resolve-uncertain --task without --dispatch failed: %v", err)
	}
	mu.Lock()
	if lastResolve.ID != "task-1" || lastResolve.DispatchID != "disp-auto-99" || lastResolve.Action != "redispatch" || lastResolve.AuthorizingMessage != "auth-msg" {
		t.Fatalf("ResolveUncertain params = %+v, want ID=task-1 DispatchID=disp-auto-99 Action=redispatch", lastResolve)
	}
	mu.Unlock()

	// 2. --task with explicit --dispatch uses the provided --dispatch directly.
	out.Reset()
	err = execute(context.Background(), []string{
		"resolve-uncertain", "--home", home, "--instance", "unc",
		"--task", "task-1", "--dispatch", "disp-explicit", "--action", "escalate", "--authorizing-message", "auth-msg",
	}, &out)
	if err != nil {
		t.Fatalf("resolve-uncertain --task with --dispatch failed: %v", err)
	}
	mu.Lock()
	if lastResolve.ID != "task-1" || lastResolve.DispatchID != "disp-explicit" || lastResolve.Action != "escalate" {
		t.Fatalf("ResolveUncertain params = %+v, want ID=task-1 DispatchID=disp-explicit Action=escalate", lastResolve)
	}
	// 3. Omitted --dispatch when task has no in-flight dispatch returns a clear error.
	dispatchID = ""
	mu.Unlock()

	out.Reset()
	err = execute(context.Background(), []string{
		"resolve-uncertain", "--home", home, "--instance", "unc",
		"--task", "task-1", "--action", "redispatch", "--authorizing-message", "auth-msg",
	}, &out)
	if err == nil || !strings.Contains(err.Error(), "no current in-flight or uncertain dispatch") {
		t.Fatalf("resolve-uncertain with no active dispatch err = %v, want clear error", err)
	}

	// 4. Conflicting --id and --task returns usage exit 2.
	err = execute(context.Background(), []string{
		"resolve-uncertain", "--home", home, "--instance", "unc",
		"--id", "task-1", "--task", "task-2", "--dispatch", "d", "--action", "redispatch", "--authorizing-message", "auth-msg",
	}, &out)
	if exitCode(err) != 2 {
		t.Fatalf("conflicting --id and --task exitCode = %d (%v), want 2", exitCode(err), err)
	}
}

type pauseAtStageExecutor struct {
	base       engine.Executor
	targetTask string
	targetStg  string
	once       sync.Once
	reached    chan struct{}
	release    chan struct{}
}

func (p *pauseAtStageExecutor) Execute(ctx context.Context, d engine.Dispatch) (engine.Result, error) {
	if d.TaskID == p.targetTask && d.Stage == p.targetStg {
		p.once.Do(func() {
			close(p.reached)
		})
		select {
		case <-p.release:
		case <-ctx.Done():
			return engine.Result{}, ctx.Err()
		}
	}
	return p.base.Execute(ctx, d)
}

// FAE-CLI-013 / FAE-ROB-012: resume of an interrupted run reaches the same
// outcome as an uninterrupted one.
func TestResumeInterruptedRunReachesSameOutcomeAsUninterrupted(t *testing.T) {
	binary := os.Getenv("FAE_TEST_DOLT_BINARY")
	if binary == "" {
		t.Skip("FAE_TEST_DOLT_BINARY required for native resume acceptance test")
	}
	root := planeRoot(t, "fae-cli-resume-", "resume")
	t.Cleanup(func() { reapProcesses(t, root); os.RemoveAll(root) })
	ws := filepath.Join(root, "workspace")
	if err := os.Mkdir(ws, 0700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"-c", "user.email=t@localhost", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = ws
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	t.Chdir(ws)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	pauseExec := &pauseAtStageExecutor{
		base:       engine.NewScripted(defaults.HappyScript()),
		targetTask: "interrupted-task",
		targetStg:  "spec",
		reached:    make(chan struct{}),
		release:    make(chan struct{}),
	}
	home := filepath.Join(root, "instances")
	d, err := daemon.Start(ctx, daemon.Config{
		Root:        home,
		Workspace:   ws,
		Instance:    "resume",
		DoltBinary:  binary,
		Executor:    pauseExec,
		MaxInFlight: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = d.Close(closeCtx)
	}()

	fae := func(args ...string) (map[string]any, error) {
		var out bytes.Buffer
		err := execute(ctx, append(args, "--home", home, "--instance", "resume", "--json"), &out)
		var body map[string]any
		if err == nil {
			_ = json.Unmarshal(out.Bytes(), &body)
		}
		return body, err
	}
	must := func(args ...string) map[string]any {
		t.Helper()
		body, err := fae(args...)
		if err != nil {
			t.Fatalf("fae %v: %v", args, err)
		}
		return body
	}

	must("message", "--id", "auth", "--text", `{"text":"Authorize workflow put, run admissions, mid-run block and resume.","authorize":{"classes":["governance","task","runtime"]}}`)

	// Configure a workflow identical to defaults.Workflow() with spec's remediation
	// route set to spec-review so an operator Block during spec resumes cleanly at spec-review.
	doc := defaults.Workflow()
	doc.ID = "resumable-flow"
	specStage := doc.Stages["spec"]
	specStage.Failure = "spec-review"
	doc.Stages["spec"] = specStage
	rawFlow, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	flowPath := filepath.Join(root, "resumable-flow.json")
	if err := os.WriteFile(flowPath, rawFlow, 0600); err != nil {
		t.Fatal(err)
	}
	must("template", "workflow-put", "--content", flowPath, "--name", "Resumable flow", "--authorizing-message", "auth")

	waitTaskState := func(taskID, wantState string, wantNoInFlight bool) map[string]any {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			got := must("task", "show", "--id", taskID)
			state, _ := got["state"].(string)
			inFlight, _ := got["in_flight"].(bool)
			if state == wantState && (!wantNoInFlight || !inFlight) {
				return got
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for task %s to reach state %s (noInFlight=%v)", taskID, wantState, wantNoInFlight)
		return nil
	}

	// 1. Run uninterrupted-task in run-uninterrupted to completion.
	must("run", "--id", "uninterrupted-task", "--prompt", "Build feature X", "--workflow", "resumable-flow", "--run-id", "run-uninterrupted", "--authorizing-message", "auth")
	uninterrupted := waitTaskState("uninterrupted-task", "Done", true)

	// 2. Start interrupted-task in run-interrupted, block it mid-run while spec is executing,
	// let the held spec turn collect, and then resume the interrupted run via `fae resume`.
	must("run", "--id", "interrupted-task", "--prompt", "Build feature X", "--workflow", "resumable-flow", "--run-id", "run-interrupted", "--authorizing-message", "auth")
	select {
	case <-pauseExec.reached:
	case <-ctx.Done():
		t.Fatal("timed out waiting for interrupted-task to reach spec stage")
	}

	must("block", "--task", "interrupted-task", "--reason", "interrupted mid-run", "--authorizing-message", "auth")
	close(pauseExec.release)

	// Wait until the task is Parked/Blocked and its held turn has been collected.
	blockedDoc := waitTaskState("interrupted-task", "Blocked", true)
	if blockedDoc["state"] != "Blocked" {
		t.Fatalf("interrupted-task state = %v, want Blocked", blockedDoc["state"])
	}

	// Resume the interrupted run via the `fae resume` CLI verb.
	resumedOut := must("resume", "--run-id", "run-interrupted", "--authorizing-message", "auth")
	resumedList, _ := resumedOut["resumed"].([]any)
	if len(resumedList) != 1 {
		t.Fatalf("fae resume --run-id run-interrupted resumed = %v, want 1 task", resumedOut)
	}

	interrupted := waitTaskState("interrupted-task", "Done", true)

	// 3. Assert that the resumed run reached the exact same outcome as the uninterrupted run.
	for _, field := range []string{"state", "stage", "attempts", "attempt_limit", "mode"} {
		if !reflect.DeepEqual(uninterrupted[field], interrupted[field]) {
			t.Fatalf("mismatch on %s: uninterrupted=%#v vs interrupted=%#v", field, uninterrupted[field], interrupted[field])
		}
	}
	stageVisits := func(doc map[string]any) []string {
		var stages []string
		trace, _ := doc["trace"].([]any)
		for _, raw := range trace {
			m, _ := raw.(map[string]any)
			st, _ := m["stage"].(string)
			if st != "Blocked" {
				stages = append(stages, st)
			}
		}
		return stages
	}
	if gotU, gotI := stageVisits(uninterrupted), stageVisits(interrupted); !reflect.DeepEqual(gotU, gotI) {
		t.Fatalf("stage visit sequence mismatch: uninterrupted=%v vs interrupted=%v", gotU, gotI)
	}
	unintShow := must("show", "--task", "uninterrupted-task")
	intShow := must("show", "--task", "interrupted-task")
	unintTasks, _ := unintShow["tasks"].([]any)
	intTasks, _ := intShow["tasks"].([]any)
	if len(unintTasks) != 1 || len(intTasks) != 1 {
		t.Fatalf("unexpected show tasks: %v vs %v", unintShow, intShow)
	}
	unintArts, _ := unintTasks[0].(map[string]any)["latest_artifacts"].([]any)
	intArts, _ := intTasks[0].(map[string]any)["latest_artifacts"].([]any)
	if len(unintArts) == 0 || len(unintArts) != len(intArts) {
		t.Fatalf("artifact count mismatch: uninterrupted=%d vs interrupted=%d", len(unintArts), len(intArts))
	}
	for i := range unintArts {
		ua := unintArts[i].(map[string]any)
		ia := intArts[i].(map[string]any)
		if ua["stage"] != ia["stage"] || ua["artifact_id"] != ia["artifact_id"] {
			t.Fatalf("artifact[%d] mismatch: uninterrupted=%v vs interrupted=%v", i, ua, ia)
		}
	}
}

type fsEntrySnapshot struct {
	Mode   fs.FileMode
	Size   int64
	Mtime  int64
	SHA256 string
}

func snapshotTree(t *testing.T, root string) map[string]fsEntrySnapshot {
	t.Helper()
	out := map[string]fsEntrySnapshot{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		entry := fsEntrySnapshot{
			Mode: info.Mode(),
			Size: info.Size(),
		}
		// Exclude directory/socket mtime jitter on the Unix RPC socket itself while
		// still recording its presence, mode and size, and recording mtime + SHA-256
		// for every regular file in the workspace and instance root.
		if info.Mode().IsRegular() {
			entry.Mtime = info.ModTime().UnixNano()
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(raw)
			entry.SHA256 = hex.EncodeToString(sum[:])
		}
		out[rel] = entry
		return nil
	})
	if err != nil {
		t.Fatalf("snapshotTree(%s): %v", root, err)
	}
	return out
}

func snapshotChildProcesses(t *testing.T) map[int]string {
	t.Helper()
	self := os.Getpid()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatalf("ReadDir(/proc): %v", err)
	}
	children := map[int]string{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		statBytes, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
		if err != nil {
			continue
		}
		// /proc/[pid]/stat has "(comm) state ppid ..." after the last ')'.
		rparen := bytes.LastIndexByte(statBytes, ')')
		if rparen < 0 || rparen+2 >= len(statBytes) {
			continue
		}
		fields := strings.Fields(string(statBytes[rparen+2:]))
		if len(fields) < 2 {
			continue
		}
		ppid, err := strconv.Atoi(fields[1])
		if err != nil || ppid != self {
			continue
		}
		cmdline, _ := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		children[pid] = strings.ReplaceAll(string(cmdline), "\x00", " ")
	}
	return children
}

// FAE-CLI-030: fae show is strictly read-only; a file-system and process diff
// around the command is empty.
func TestShowCommandFilesystemAndProcessDiffIsEmpty(t *testing.T) {
	binary := os.Getenv("FAE_TEST_DOLT_BINARY")
	if binary == "" {
		t.Skip("FAE_TEST_DOLT_BINARY required for native show acceptance test")
	}
	root := planeRoot(t, "fae-cli-show-", "show")
	t.Cleanup(func() { reapProcesses(t, root); os.RemoveAll(root) })
	ws := filepath.Join(root, "workspace")
	if err := os.Mkdir(ws, 0700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"-c", "user.email=t@localhost", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = ws
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	t.Chdir(ws)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	home := filepath.Join(root, "instances")
	d, err := daemon.Start(ctx, daemon.Config{
		Root:        home,
		Workspace:   ws,
		Instance:    "show",
		DoltBinary:  binary,
		MaxInFlight: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = d.Close(closeCtx)
	}()

	fae := func(args ...string) (string, error) {
		var out bytes.Buffer
		err := execute(ctx, append(args, "--home", home, "--instance", "show"), &out)
		return out.String(), err
	}
	if _, err := fae("message", "--id", "auth-ctrl", "--text", "Pause drive for read-only show verification.", "--json"); err != nil {
		t.Fatal(err)
	}
	if _, err := fae("drive", "stop", "--authorizing-message", "auth-ctrl", "--json"); err != nil {
		t.Fatal(err)
	}
	if _, err := fae("message", "--id", "auth-task", "--text", "Admit a task for read-only show verification.", "--json"); err != nil {
		t.Fatal(err)
	}
	if _, err := fae("run", "--id", "show-target", "--prompt", "Inspect me", "--run-id", "show-run", "--authorizing-message", "auth-task", "--json"); err != nil {
		t.Fatal(err)
	}
	// Warm one read so any lazy SQL connection pool state is settled before snapshotting.
	if _, err := fae("show", "--id", "show-target", "--json"); err != nil {
		t.Fatal(err)
	}
	if _, err := fae("task", "show", "--id", "show-target", "--json"); err != nil {
		t.Fatal(err)
	}

	for _, cmdArgs := range [][]string{
		{"show", "--id", "show-target", "--json"},
		{"show", "--task", "show-target"},
		{"task", "show", "show-target"},
		{"task", "show", "--id", "show-target", "--json"},
		{"show", "--run-id", "show-run", "--json"},
	} {
		beforeFS := snapshotTree(t, root)
		beforeProcs := snapshotChildProcesses(t)

		out, err := fae(cmdArgs...)
		if err != nil || !strings.Contains(out, "show-target") {
			t.Fatalf("fae %v failed: err=%v out=%q", cmdArgs, err, out)
		}

		afterFS := snapshotTree(t, root)
		afterProcs := snapshotChildProcesses(t)

		if !reflect.DeepEqual(beforeFS, afterFS) {
			for k, b := range beforeFS {
				if a, ok := afterFS[k]; !ok {
					t.Errorf("fae %v removed path %s", cmdArgs, k)
				} else if a != b {
					t.Errorf("fae %v modified path %s: before=%+v after=%+v", cmdArgs, k, b, a)
				}
			}
			for k, a := range afterFS {
				if _, ok := beforeFS[k]; !ok {
					t.Errorf("fae %v created path %s (%+v)", cmdArgs, k, a)
				}
			}
			t.Fatalf("fae %v caused non-empty filesystem diff", cmdArgs)
		}
		if !reflect.DeepEqual(beforeProcs, afterProcs) {
			t.Fatalf("fae %v caused non-empty process diff: before=%v after=%v", cmdArgs, beforeProcs, afterProcs)
		}
	}
}
