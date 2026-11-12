package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/xleelz/fae/daemon"
	"github.com/xleelz/fae/defaults"
)

// TestCLINativeGovernanceAndIntake drives the built CLI dispatcher against a
// real daemon and private Dolt child (set FAE_TEST_DOLT_BINARY).
func TestCLINativeGovernanceAndIntake(t *testing.T) {
	binary := os.Getenv("FAE_TEST_DOLT_BINARY")
	if binary == "" {
		t.Skip("FAE_TEST_DOLT_BINARY required for the native CLI test")
	}
	root := planeRoot(t, "fae-cli-native-", "native")
	var err error
	t.Cleanup(func() { reapProcesses(t, root); os.RemoveAll(root) })
	ws := filepath.Join(root, "workspace")
	if err = os.Mkdir(ws, 0700); err != nil {
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
	d, err := daemon.Start(ctx, daemon.Config{Root: home, Workspace: ws, Instance: "native", DoltBinary: binary, MaxInFlight: 2})
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
		err := execute(ctx, append(args, "--home", home, "--instance", "native", "--json"), &out)
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
			t.Fatal(args, err)
		}
		return body
	}
	// A plain message authorizes one class; the structured form authorizes several.
	must("message", "--id", "auth", "--text", `{"text":"Govern workflows, queue work and stop this instance.","authorize":{"classes":["governance","task","runtime"]}}`)
	doc := defaults.Workflow()
	doc.ID = "native-flow"
	raw, _ := json.Marshal(doc)
	file := filepath.Join(root, "flow.json")
	if err = os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	put := must("template", "workflow-put", "--content", file, "--name", "Native flow", "--authorizing-message", "auth")
	if put["Number"] != float64(1) || put["Name"] != "Native flow" {
		t.Fatal(put)
	}
	coder := doc.Roles["coder"]
	coder.Cap = 2
	doc.Roles["coder"] = coder
	raw, _ = json.Marshal(doc)
	if err = os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if put = must("template", "workflow-put", "--content", file, "--authorizing-message", "auth"); put["Number"] != float64(2) || put["Parent"] != float64(1) {
		t.Fatal(put)
	}
	if diff := must("template", "workflow-diff", "--workflow", "native-flow", "--from", "1", "--to", "2"); len(diff["changes"].([]any)) != 1 {
		t.Fatal(diff)
	}
	if latest := must("template", "workflow-latest", "--workflow", "native-flow"); latest["revision"].(map[string]any)["Number"] != float64(2) {
		t.Fatal(latest)
	}
	// start-work-item queues through backlog admission and appears in the backlog.
	item := must("start-work-item", "--id", "queued-item", "--title", "Queued", "--content", "Queue me", "--workflow", "native-flow", "--priority", "20", "--authorizing-message", "auth")
	if item["id"] != "queued-item" || item["admitted"] != float64(1) {
		t.Fatal(item)
	}
	found := false
	for _, row := range must("backlog", "show")["items"].([]any) {
		found = found || row.(map[string]any)["id"] == "queued-item"
	}
	if !found {
		t.Fatal("start-work-item did not reach the backlog")
	}
	admitted := must("run", "--id", "ran", "--prompt", "Scripted", "--run-id", "native-run", "--authorizing-message", "auth")
	if admitted["run_id"] != "native-run" || admitted["disposition"].(map[string]any)["mode"] != "Local" || admitted["dry_run"] != false {
		t.Fatal(admitted)
	}
	retired := must("template", "workflow-retire", "--workflow", "native-flow", "--reason", "replaced", "--authorizing-message", "auth")
	if retired["reason"] != "replaced" {
		t.Fatal(retired)
	}
	if _, err = fae("start-work-item", "--id", "late", "--content", "x", "--workflow", "native-flow", "--authorizing-message", "auth"); exitCode(err) != 1 || !strings.Contains(err.Error(), "retired") {
		t.Fatal("retired workflow accepted a new binding", err)
	}
	if _, err = fae("template", "workflow-get", "--workflow", "native-flow", "--revision", "7"); exitCode(err) != 1 {
		t.Fatal("unknown revision", err)
	}
	if status := must("status", "--tasks"); status["snapshot"].(map[string]any)["tasks"] == nil || status["version"] == nil {
		t.Fatal(status)
	}
	if shown := must("show", "--run-id", "native-run"); len(shown["tasks"].([]any)) != 1 {
		t.Fatal(shown)
	}
	// The structured message also authorizes the later runtime-class shutdown.
	if body := must("shutdown", "--authorizing-message", "auth"); body["shutting_down"] != true {
		t.Fatal(body)
	}
}

// TestMain lets the test binary act as the fae executable for a detached serve.
func TestMain(m *testing.M) {
	if os.Getenv("CLI_TEST_AS_MAIN") == "1" {
		main()
		return
	}
	os.Exit(m.Run())
}

// FAE-CLI-010: serve without --foreground detaches once ready and prints a start
// line naming the instance, adapter, plane endpoint and socket.
func TestCLINativeServeDetachesWithStartLine(t *testing.T) {
	proves(t, "serve", 0) // before the skip: with Dolt set, a skip fails the proof
	binary := os.Getenv("FAE_TEST_DOLT_BINARY")
	if binary == "" {
		t.Skip("FAE_TEST_DOLT_BINARY required for the native CLI test")
	}
	root := planeRoot(t, "fae-cli-serve-", "detached")
	var err error
	t.Cleanup(func() { reapProcesses(t, root); os.RemoveAll(root) })
	ws := filepath.Join(root, "workspace")
	if err = os.Mkdir(ws, 0700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(ws)
	t.Setenv("CLI_TEST_AS_MAIN", "1")
	home := filepath.Join(root, "instances")
	var out bytes.Buffer
	if err = execute(context.Background(), []string{"--home", home, "--instance", "detached", "serve", "--workspace", ws, "--adapter", "scripted", "--dolt", binary, "--default-mode", "local"}, &out); err != nil {
		t.Fatal(err)
	}
	var line struct {
		Instance, Adapter, Plane, Socket string
		PID                              int
		Ready                            bool
	}
	if err = json.Unmarshal(out.Bytes(), &line); err != nil || !line.Ready || line.Instance != "detached" || line.Adapter != "scripted" || !strings.HasPrefix(line.Plane, "127.0.0.1:") || line.Socket != filepath.Join(home, "detached", "run", "rpc.sock") || line.PID == os.Getpid() {
		t.Fatalf("start line %s %v", out.String(), err)
	}
	fae := func(args ...string) error {
		return execute(context.Background(), append(args, "--home", home, "--instance", "detached"), &bytes.Buffer{})
	}
	if err = fae("status"); err != nil {
		t.Fatal("detached daemon unreachable", err)
	}
	if err = fae("message", "--id", "auth", "--text", `{"text":"Stop, restart and queue scripted work on this detached test instance.","authorize":{"classes":["task","runtime"]}}`); err != nil {
		t.Fatal(err)
	}
	stopDaemon := func(pid int) {
		t.Helper()
		if err := fae("shutdown", "--authorizing-message", "auth"); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(30 * time.Second)
		for syscall.Kill(pid, 0) == nil {
			if time.Now().After(deadline) {
				_ = syscall.Kill(pid, syscall.SIGKILL)
				t.Fatal("detached daemon did not stop")
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	stopDaemon(line.PID)
	if err = fae("status"); exitCode(err) != 3 {
		t.Fatal("stopped daemon status", err)
	}
	// FAE-CLI-003: only run --start, from the bound workspace, starts the daemon.
	if err = fae("run", "--prompt", "x", "--authorizing-message", "auth"); exitCode(err) != 3 {
		t.Fatal("run without --start", err)
	}
	t.Chdir(root)
	if err = fae("run", "--start", "--prompt", "x", "--authorizing-message", "auth"); exitCode(err) != 1 || !strings.Contains(err.Error(), "bound workspace") {
		t.Fatal("run --start outside the bound workspace", err)
	}
	t.Chdir(ws)
	for _, args := range [][]string{{"init", "-q"}, {"-c", "user.email=t@localhost", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init"}} {
		cmd := exec.Command("git", args...)
		if raw, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, raw)
		}
	}
	out.Reset()
	if err = execute(context.Background(), []string{"run", "--start", "--id", "started", "--prompt", "x", "--authorizing-message", "auth", "--home", home, "--instance", "detached"}, &out); err != nil {
		t.Fatal(err)
	}
	var admitted map[string]any
	if err = json.Unmarshal(out.Bytes(), &admitted); err != nil || admitted["id"] != "started" {
		t.Fatalf("run --start %s %v", out.String(), err)
	}
	var status bytes.Buffer
	if err = execute(context.Background(), []string{"status", "--home", home, "--instance", "detached"}, &status); err != nil {
		t.Fatal(err)
	}
	var body struct {
		PID int `json:"pid"`
	}
	if err = json.Unmarshal(status.Bytes(), &body); err != nil || body.PID == line.PID || body.PID == 0 {
		t.Fatalf("restarted pid %s %v", status.String(), err)
	}
	stopDaemon(body.PID)
}
