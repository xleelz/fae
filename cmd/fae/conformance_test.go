package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/controller"
	"github.com/xleelz/fae/daemon"
	"github.com/xleelz/fae/hostguard"
	"github.com/xleelz/fae/scheduler"
)

// conformance kinds: "rpc" needs the daemon and is checked for 0, 1 and 3;
// "offline" runs without one; "lifecycle" rows (long-running or multi-step
// commands) list one argument variant per exit code they can produce against
// the fake daemon. Every row is also checked for usage (2) and --help.
type conformance struct {
	kind string
	args []string
}

// lifecycleCodes accounts for each of the codes 0..3: either codes runs a
// variant here, or proof cites tests in this package that run the row to that
// code and declare it with proves (a code that needs more than the fake
// daemon), or proof starts "not applicable: " with the reason (proofProblems
// says what is checked). stdin is a variant's standard input; stub
// installs a substitute around one variant and returns its restore.
// passthrough marks a broker whose standard output is its child's; quiet a
// server that prints nothing until stopped. Other output must be JSON.
type lifecycleCodes struct {
	codes       map[int][]string
	proof       map[int]string
	stdin       map[int]string
	stub        map[int]func() func()
	passthrough bool
	quiet       bool
}

const notApplicable = "not applicable: "

func conformanceTable(t *testing.T, home string) ([]conformance, map[string]lifecycleCodes) {
	dir := t.TempDir()
	write := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	example, err := os.ReadFile("../../examples/workflow.json")
	if err != nil {
		t.Fatal(err)
	}
	wf := write("workflow.json", string(example))
	script, err := os.ReadFile("../../examples/script.json")
	if err != nil {
		t.Fatal(err)
	}
	scenario := write("script.json", string(script))
	payload := write("backlog.json", `{"items":[{"id":"a","workflow_id":"","revision":0,"tree":"","priority":10,"content":"x","authorizing_message":""}],"edges":[],"authorizing_message":""}`)
	doc := write("doc.json", `{"schema_version":1}`)
	taskCreate := write("task-create.json", `{"id":"t","content":"x","priority":20,"edges":[{"from":"a","to":"t","kind":"blocking"}],"accept_in_flight":false}`)
	taskUpdate := write("task-update.json", `{"id":"t","title":"T","declared_files":["docs/a.md"],"expected_version":3}`)
	artifact := daemon.CensusArtifact{Format: 1, Instance: "conf", Rows: []daemon.CensusRow{{ID: "t", State: "Escalated", Version: 1, SnapshotDigest: strings.Repeat("a", 64), Classification: "obsolete", Reason: "obsolete r", Evidence: "e"}}}
	artifact.Digest = daemon.CensusDigest(artifact)
	reviewed, _ := json.Marshal(artifact)
	census := write("census.json", string(reviewed))
	commands := write("commands.json", `[["go","test","./..."]]`)
	heavy := filepath.Join(dir, "heavy")
	if _, err = scheduler.OpenLedger(heavy, 2); err != nil {
		t.Fatal(err)
	}
	// A successful roll needs a replacement daemon (controller and daemon
	// roll tests); here the client's result only has to map to 0 and JSON.
	rolled := func() func() {
		old := runRoll
		runRoll = func(ctx context.Context, c controller.Client, o controller.Options) (controller.Health, error) {
			return c.RPC(ctx, controller.Params{Action: "prepare", AuthorizingMessage: o.Message, Note: o.Note})
		}
		return func() { runRoll = old }
	}
	// A maintenance marker makes the watchdog write its heartbeat.
	maintained := func() func() {
		layout, err := daemon.Paths(home, "conf")
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(controller.Marker{ID: "conformance", Message: "m", State: "stopped", NextAction: "maintenance start"})
		marker := filepath.Join(layout.Root, "controller-maintenance.json")
		if err = os.WriteFile(marker, raw, 0600); err != nil {
			t.Fatal(err)
		}
		return func() { os.Remove(marker) }
	}
	hookRequest := `{"tool_name":"Bash","tool_input":{"command":"/usr/local/go/bin/go test ./..."}}`
	// A short root: the socket path must fit the 108-byte Unix limit under any TMPDIR.
	launcherDir := filepath.Join(shortHome(t), "launcher")
	if err = os.Mkdir(launcherDir, 0700); err != nil {
		t.Fatal(err)
	}
	m := []string{"--authorizing-message", "m"}
	with := func(args ...string) []string { return append(args, m...) }
	refused := func(args ...string) []string { return append(args, "--authorizing-message", "refused") }
	launcher := []string{"--note", "n", "--launcher-socket", "/run/fae-launcher.sock"}
	thenRoll := []string{"land", "--task", "t", "--target", "main", "--then-roll", "--launcher-socket", "/run/launcher.sock"}
	heavyRun := []string{"heavy-op", "run", "--ledger", heavy, "--weight", "1", "--class", "operator", "--"}
	lifecycle := map[string]lifecycleCodes{
		"serve": {codes: map[int][]string{1: {"serve", "--workspace", dir, "--foreground", "--adapter", "scripted", "--inline-limit", "8192"}, 2: {"serve", "--workspace", dir, "--adapter", "scripted", "--inline-limit", "x"}},
			proof: map[int]string{0: "TestCLINativeServeDetachesWithStartLine (a real start; needs Dolt)", 3: notApplicable + "serve starts the daemon; it never dials one"}},
		"roll": {codes: map[int][]string{0: with(append([]string{"roll", "--binary", "/bin/true"}, launcher...)...), 1: refused(append([]string{"roll", "--binary", "/bin/true"}, launcher...)...), 2: with(append([]string{"roll"}, launcher...)...), 3: with(append([]string{"roll", "--binary", "/bin/true"}, launcher...)...)},
			stub: map[int]func() func(){0: rolled}},
		"maintenance stop": {codes: map[int][]string{1: refused(append([]string{"maintenance", "stop"}, launcher...)...), 2: with("maintenance", "stop"), 3: with(append([]string{"maintenance", "stop"}, launcher...)...)},
			proof: map[int]string{0: "TestMaintenanceStopAndStartCLIExitZero (a daemon that drains, stops and is proven dead)"}},
		"maintenance start": {codes: map[int][]string{1: with("maintenance", "start"), 2: with("maintenance", "start", "--launcher-socket", "relative.sock")},
			proof: map[int]string{0: "TestMaintenanceStopAndStartCLIExitZero (the runtime launcher starts the pinned launch, which the daemon accepts)", 3: notApplicable + "start launches the stopped daemon; an unreachable daemon is its normal input"}},
		"watchdog": {codes: map[int][]string{0: with("watchdog", "--period", "20ms"), 1: refused("watchdog", "--period", "20ms"), 2: with("watchdog", "--period", "x"), 3: with("watchdog")},
			stub: map[int]func() func(){0: maintained}},
		"runtime-launcher": {codes: map[int][]string{0: {"runtime-launcher", "--socket", filepath.Join(launcherDir, "l.sock")}, 1: {"runtime-launcher", "--socket", filepath.Join(dir, "l.sock")}, 2: {"runtime-launcher"}},
			proof: map[int]string{3: notApplicable + "the launcher needs no daemon"}, quiet: true},
		"cleanup scratch": {codes: map[int][]string{0: {"cleanup", "scratch", "--dry-run", "--instance", "scratch"}, 1: {"cleanup", "scratch", "--dry-run"}, 2: {"cleanup", "scratch", "--apply", "--dry-run"}},
			proof: map[int]string{3: notApplicable + "it works offline on a stopped scratch instance, and apply stops a running one itself (daemon.TestScratchNativeStopBackupCleanupAndRepeat)"}},
		"land --then-roll": {codes: map[int][]string{0: with(thenRoll...), 1: refused(thenRoll...), 2: with("land", "--batch", "a,b", "--target", "main", "--then-roll"), 3: with(thenRoll...)},
			stub: map[int]func() func(){0: rolled}},
		"gate run": {codes: map[int][]string{0: {"gate", "run", "--host-ledger", filepath.Join(dir, "ledger"), "--memory-floor-gib", "4", "--", "true"}, 1: {"gate", "run", "--host-ledger", filepath.Join(dir, "ledger"), "--memory-floor-gib", "4", "--", "false"}, 2: {"gate", "run", "--host-ledger", dir}},
			proof: map[int]string{3: notApplicable + "gate run is a host lease broker; its own codes are 75 and 76 (TestGateRunHeldLeaseTimesOutWithDistinctCode)"}, passthrough: true},
		// The broker's refusal is 75; 1 is the child's own code passing through.
		"heavy-op run": {codes: map[int][]string{0: append(heavyRun, "true"), 1: append(heavyRun, "false"), 2: {"heavy-op", "run", "--weight", "1", "--class", "operator"}},
			proof: map[int]string{3: notApplicable + "the broker serves role shims without a daemon; its refusal is 75 (TestHeavyOpRunCLIExitCodes)"}, passthrough: true},
		// A provider pre-tool hook blocks the tool call on exit 2 and runs it on
		// any code but 0 and 2, so the hook fails closed: never 1.
		"heavy-op hook": {codes: map[int][]string{0: {"heavy-op", "hook", "--shim-dir", dir}, 2: {"heavy-op", "hook", "--shim-dir", dir}}, stdin: map[int]string{0: hookRequest, 2: "not a hook request"},
			proof: map[int]string{1: notApplicable + "the provider would run the tool on exit 1; every hook failure exits 2, which blocks it (TestHeavyOpShimPolicyAndHookCLI, TestHeavyOpHookFailsClosedWithoutHomeOrOutput)", 3: notApplicable + "the hook needs no daemon"}},
		"heavy-op run --shim-policy": {codes: map[int][]string{2: {"heavy-op", "run", "--shim-policy", "/p", "--weight", "1"}},
			proof: map[int]string{0: "TestHeavyOpShimPolicyAndHookCLI (a materialized policy runs its tool and exits 0)", 1: notApplicable + "policy refusals are 77 and 78; a child's own 1 passes through (TestHeavyOpShimPolicyAndHookCLI)", 3: notApplicable + "role shims broker without a daemon"}, passthrough: true},
	}
	return []conformance{
		{"rpc", []string{"status"}}, {"rpc", []string{"status", "--tasks", "--include-retired"}},
		{"rpc", []string{"board", "--after", "x", "--limit", "5"}},
		{"rpc", []string{"history", "--task", "t"}}, {"rpc", []string{"show", "--task", "t"}}, {"rpc", []string{"show", "--run-id", "r"}},
		{"rpc", []string{"events", "--after", "3"}},
		{"rpc", []string{"history", "--task", "t", "--records"}}, {"rpc", []string{"topology", "--limit", "5"}}, {"rpc", []string{"provenance", "--commit", "c"}},
		{"rpc", []string{"provider-health"}}, {"rpc", []string{"provider-health", "--window", "1h"}}, {"rpc", []string{"provider-log"}},
		{"rpc", []string{"leases"}}, {"rpc", []string{"panes"}},
		{"rpc", []string{"board", "--state", "Pending,Escalated", "--target", "t", "--workflow", "w", "--mode", "normal", "--molecule", "m", "--run-id", "r", "--min-priority", "20", "--in-flight"}},
		{"rpc", []string{"template", "target-config-validate", "--content", doc}}, {"rpc", []string{"template", "target-config-diff", "--id", "x", "--from", "1", "--to", "2"}},
		{"rpc", []string{"template", "target-config-simulate", "--id", "x", "--changed", "a.go,b_test.go"}}, {"rpc", []string{"template", "target-config-simulate", "--content", doc, "--commands", commands}},
		{"rpc", []string{"message", "--text", "hi"}}, {"rpc", []string{"message", "--text", "hi", "--task", "t"}},
		{"rpc", []string{"reply", "--to", "m", "--text", "hi"}}, {"rpc", []string{"inbox"}}, {"rpc", []string{"inbox", "--task", "t", "--kind", "threads,blocked"}},
		{"rpc", with("attach-design-doc", "--task", "t", "--reference", "https://design.invalid/doc")}, {"rpc", with("attach-design-doc", "--item", "i", "--reference", "docs/design.md")},
		{"rpc", with("migrate", "--task", "t", "--revision", "2")}, {"rpc", []string{"analyze-session", "--task", "t"}},
		{"rpc", with("supply-artifact", "--task", "t", "--file", doc)},
		{"rpc", with("decide", "--task", "t", "--option", "ship")}, {"rpc", with("decide", "--task", "t", "--option", "ship", "--decision", "d", "--rationale", "r")},
		{"rpc", with("reclaim")}, {"rpc", with("reclaim", "--host-wide", "--dry-run")},
		{"rpc", []string{"quarantine"}}, {"rpc", []string{"quarantine", "inspect", "--task", "t"}}, {"rpc", with("quarantine", "release", "--task", "t")}, {"rpc", with("quarantine", "retire", "--task", "t", "--reason", "r")},
		{"rpc", []string{"alerts"}}, {"rpc", []string{"alerts", "list", "--unacknowledged"}}, {"rpc", with("alerts", "ack", "--id", "1")},
		{"rpc", []string{"metrics", "--window", "1h"}}, {"rpc", []string{"host-metrics"}},
		{"rpc", []string{"ui", "token"}},
		{"rpc", with("resolve-uncertain", "--id", "t", "--dispatch", "d", "--action", "redispatch")},
		{"rpc", with("runtime-config", "--pane-limit", "20")},
		{"rpc", with("pool", "resize", "--pool", "p", "--provider", "anthropic", "--credential", "default", "--cap", "2", "--reason", "r")},
		{"rpc", with("quota", "set", "--pool", "p", "--until", "2027-01-01T00:00:00Z", "--reason", "r")},
		{"rpc", with("quota", "clear", "--pool", "p", "--reason", "r")},
		{"rpc", with("pause", "--scope", "instance", "--reason", "r")},
		{"rpc", []string{"pause", "list"}},
		{"rpc", with("unpause", "--scope", "instance", "--reason", "r")},
		{"rpc", with("drain")}, {"rpc", with("shutdown")},
		{"rpc", with("run", "--prompt", "x")}, {"rpc", with("run", "--prompt", "x", "--mode", "normal", "--dry-run")},
		{"rpc", with("start-work-item", "--title", "T", "--content", "x")},
		{"rpc", with("resume", "--task", "t")}, {"rpc", with("resume", "--run-id", "r")}, {"rpc", with("resume")},
		{"rpc", with("retire", "--task", "t", "--reason", "obsolete r")}, {"rpc", with("escalate", "--task", "t", "--reason", "r")},
		{"rpc", with("block", "--task", "t", "--reason", "r")}, {"rpc", with("cancel", "--task", "t", "--reason", "r")},
		{"rpc", with("defer", "--task", "t", "--until", "2027-01-01T00:00:00Z")}, {"rpc", with("undefer", "--task", "t")},
		{"rpc", with("reprioritize", "--task", "t", "--priority", "20")}, {"rpc", with("set-attempt-limit", "--task", "t", "--limit", "5")},
		{"rpc", with("restart", "--task", "t", "--information", "i")}, {"rpc", with("resume-with-context", "--task", "t", "--context", "c")},
		{"rpc", with("notify", "--task", "t", "--kind", "note", "--body", "b")},
		{"rpc", with("follow-up", "--task", "t", "--text", "x")},
		{"rpc", []string{"backlog", "show"}}, {"rpc", with("backlog", "admit", "--file", payload)},
		{"rpc", with("backlog", "edge", "add", "--from", "a", "--to", "b")}, {"rpc", with("backlog", "edge", "remove", "--from", "a", "--to", "b")},
		{"rpc", with("reconcile-done", "--dry-run")},
		{"rpc", with("task", "create", "--id", "t", "--content", "x")},
		{"rpc", with("task", "create", "--id", "t", "--title", "T", "--content", "x", "--priority", "20", "--source-ref", "s", "--write-class", "w", "--declared-file", "docs/a.md", "--test-filter", "x", "--test-shape", "script", "--depends-on", "a", "--blocks", "b", "--relates-to", "c", "--accept-in-flight", "--idempotence-key", "k")},
		{"rpc", with("task", "create", "--file", taskCreate)},
		{"rpc", with("task", "update", "--id", "t", "--title", "T", "--priority", "30")}, {"rpc", with("task", "update", "--id", "t", "--clear-declared-files", "--expected-version", "2")},
		{"rpc", with("task", "update", "--file", taskUpdate)},
		{"rpc", with("task", "delete", "--id", "t", "--reason", "r")}, {"rpc", with("task", "delete", "--id", "t", "--reason", "r", "--superseded-by", "u", "--expected-version", "4")},
		{"rpc", []string{"task", "show", "--id", "t"}}, {"rpc", []string{"task", "show", "--id", "t", "--include-retired"}},
		{"rpc", []string{"prophet", "session", "--task", "t"}}, {"rpc", []string{"prophet", "chat", "--task", "t", "--text", "x"}}, {"rpc", []string{"prophet", "global"}},
		{"rpc", with("prophet", "restart", "--task", "t", "--information", "i")}, {"rpc", with("prophet", "resume", "--task", "t", "--context", "c")},
		{"rpc", with("template", "workflow-put", "--content", wf)}, {"rpc", with("template", "workflow-put", "--content", wf, "--name", "Demo")},
		{"rpc", []string{"template", "workflow-get", "--workflow", "w", "--revision", "1"}}, {"rpc", []string{"template", "workflow-latest", "--workflow", "w"}},
		{"rpc", []string{"template", "workflow-list"}}, {"rpc", with("template", "workflow-retire", "--workflow", "w", "--reason", "r")},
		{"rpc", []string{"template", "workflow-diff", "--workflow", "w", "--from", "1", "--to", "2"}},
		{"rpc", []string{"template", "workflow-simulate", "--content", wf, "--scenario", scenario}}, {"rpc", []string{"template", "workflow-simulate", "--workflow", "w", "--scenario", scenario}},
		{"rpc", with("template", "target-config-put", "--id", "x", "--content", doc)}, {"rpc", []string{"template", "target-config-get", "--id", "x"}},
		{"rpc", []string{"template", "target-config-list"}}, {"rpc", with("template", "target-config-retire", "--id", "x", "--reason", "r")},
		{"rpc", with("template", "adapter-put", "--id", "x", "--content", doc)}, {"rpc", []string{"template", "adapter-get", "--id", "x"}},
		{"rpc", []string{"template", "adapter-list"}}, {"rpc", with("template", "adapter-retire", "--id", "x", "--reason", "r")},
		{"rpc", with("schedule", "create", "--id", "l", "--role", "oracle", "--prompt", "p")}, {"rpc", []string{"schedule", "list"}}, {"rpc", []string{"schedule", "inspect", "--id", "l"}},
		{"rpc", with("schedule", "pause", "--id", "l")}, {"rpc", with("schedule", "resume", "--id", "l")}, {"rpc", with("schedule", "retire", "--id", "l")},
		{"rpc", []string{"drive", "status"}}, {"rpc", with("drive", "stop")}, {"rpc", with("drive", "rearm")},
		{"rpc", []string{"maintenance", "status"}},
		{"rpc", []string{"workspace", "review-plan", "--task", "t"}}, {"rpc", with("workspace", "review-prepare", "--task", "t", "--adapter", "a")},
		{"rpc", []string{"review-health", "--task", "t"}}, {"rpc", []string{"census"}},
		{"rpc", with("cleanup", "retired")}, {"rpc", with("cleanup", "done")}, {"rpc", with("backup", "plane")}, {"rpc", []string{"backup", "quota", "status"}},
		{"rpc", with("backup", "quota", "configure", "--max-backups", "1")}, {"rpc", with("backup", "retention")},
		{"rpc", with("backup", "replicate", "--backup", "b", "--endpoint", "https://replica.invalid")}, {"rpc", with("backup", "unpin", "--backup", "b")},
		{"rpc", []string{"backup", "retention-schedule", "status"}}, {"rpc", []string{"backup", "creation-schedule", "status"}}, {"rpc", []string{"backup", "replication-schedule", "status"}},
		{"offline", []string{"version"}}, {"offline", []string{"help"}}, {"offline", []string{"workflow", "default"}}, {"offline", []string{"workflow", "scenario"}},
		{"offline", []string{"workflow", "validate", wf}}, {"offline", []string{"workflow", "simulate", wf, scenario, "demo"}},
		{"offline", []string{"template", "workflow-validate", "--content", wf}},
		{"offline", []string{"doctor"}}, {"offline", []string{"gates", "self-test", "--offline"}}, {"rpc", []string{"gates", "self-test"}},
		{"rpc", with("census", "apply", "--file", census, "--gap", "0s")},
		{"rpc", []string{"cleanup", "scratch-archive", "--scratch-instance", "s", "--source", "/a", "--survivor", "/b"}}, {"rpc", with("cleanup", "scratch-archive", "--scratch-instance", "s", "--source", "/a", "--survivor", "/b", "--apply", "--plan-digest", "d")},
		{"rpc", []string{"tree", "t", "--stats"}},
		{"rpc", with("resume-at-stage", "--task", "t")},
		{"rpc", []string{"change-request", "list", "--task", "t"}},
		{"rpc", with("change-request", "disposition", "--task", "t", "--id", "r", "--state", "accepted", "--reason", "reviewed")},
		{"rpc", []string{"workflow-limit"}},
		{"rpc", with("workflow-limit", "--max", "4")},
		{"rpc", with("tree-limit", "--task", "t", "--max-sessions", "2")},
		{"rpc", []string{"events", "--follow"}},
		{"rpc", with("land", "--task", "t", "--target", "main")}, {"rpc", with("land", "--batch", "a,b", "--target", "main")},
		{"lifecycle", []string{"serve"}},
		{"lifecycle", []string{"roll"}},
		{"lifecycle", []string{"maintenance", "stop"}},
		{"lifecycle", []string{"maintenance", "start"}},
		{"lifecycle", []string{"watchdog"}},
		{"lifecycle", []string{"runtime-launcher"}},
		{"lifecycle", []string{"cleanup", "scratch"}},
		{"lifecycle", []string{"land", "--then-roll"}},
		{"lifecycle", []string{"gate", "run"}},
		{"lifecycle", []string{"heavy-op", "run"}},
		{"lifecycle", []string{"heavy-op", "hook"}},
		{"lifecycle", []string{"heavy-op", "run", "--shim-policy"}},
	}, lifecycle
}

// fakeRecorder is the conformance fake daemon: it records each served method
// (stream requests as "Events.stream") and refuses when fail is set, an
// uncited mutation, or a mutation citing the message "refused".
type fakeRecorder struct {
	mu      sync.Mutex
	methods []string
}

func (f *fakeRecorder) take() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.methods
	f.methods = nil
	return out
}

func fakeDaemon(t *testing.T, home, instance string, fail *atomic.Bool) *fakeRecorder {
	t.Helper()
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
	rec := &fakeRecorder{}
	landing := map[string]any{"id": "l", "state": "finished", "tasks": []string{"t"}, "results": []map[string]any{{"task": "t", "outcome": "landed"}}}
	server, err := api.New(token, func(_ context.Context, r api.Request) (any, error) {
		var p map[string]any
		_ = json.Unmarshal(r.Params, &p)
		method := r.Method
		if r.Method == "Controller" || r.Method == "Schedule" {
			method += "." + fmt.Sprint(p["action"])
		}
		rec.mu.Lock()
		rec.methods = append(rec.methods, method)
		rec.mu.Unlock()
		// Like the daemon, refuse an uncited mutation; read actions need no citation.
		read := p["action"] == "health" || p["action"] == "status" || p["action"] == "list" || p["action"] == "inspect"
		if v, ok := p["authorizing_message"]; fail.Load() || ok && (v == "" || v == "refused") && !read {
			return nil, errors.New("refused")
		}
		if r.Method == "Land" || r.Method == "LandStatus" {
			if tasks, ok := p["tasks"].([]any); ok {
				landing["tasks"], landing["results"] = tasks, []map[string]any{}
				for _, task := range tasks {
					landing["results"] = append(landing["results"].([]map[string]any), map[string]any{"task": task, "outcome": "landed"})
				}
			}
			return landing, nil
		}
		if r.Method == "GatesSelfTest" {
			return map[string]any{"passed": true, "gates": []any{}}, nil
		}
		if r.Method == "WorkflowSimulate" {
			return map[string]any{"state": "Done"}, nil
		}
		return map[string]any{"ok": true}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	server.SetEventStream(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.methods = append(rec.methods, "Events.stream")
		rec.mu.Unlock()
		if fail.Load() {
			http.Error(w, "refused", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, `{"sequence":1,"kind":"daemon-ready"}`)
	})
	if err = server.Listen(layout.Socket); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve() }()
	t.Cleanup(func() { server.Shutdown(context.Background()); <-done })
	return rec
}

// scratchInstance plants a marked scratch instance root (FAE-CTL-043).
func scratchInstance(t *testing.T, home, instance string) {
	t.Helper()
	layout, err := daemon.Paths(home, instance)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(layout.Run, 0700); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(layout.Root)
	if err != nil {
		t.Fatal(err)
	}
	st := info.Sys().(*syscall.Stat_t)
	raw, _ := json.Marshal(daemon.ScratchIdentity{Device: uint64(st.Dev), Inode: st.Ino, ID: strings.Repeat("ab", 16), Instance: instance, Root: layout.Root, Workspace: "/var/tmp/fae-scratch-workspace", Created: time.Now().UTC()})
	if err = os.WriteFile(filepath.Join(layout.Root, "scratch.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
}

// FAE-CLI-001: every command, with --json, honours 0 success, 1 refusal, 2 usage, 3 unreachable.
func TestCLIConformanceEveryCommand(t *testing.T) {
	home, empty := shortHome(t), shortHome(t)
	var fail atomic.Bool
	rec := fakeDaemon(t, home, "conf", &fail)
	scratchInstance(t, home, "scratch")
	gateTestHost(t, 1)
	heavyOpObserve = func(context.Context) (hostguard.Sample, error) {
		r := func(v float64) hostguard.Reading { return hostguard.Reading{Value: v, Known: true} }
		return hostguard.Sample{Memory: r(64 * float64(hostguard.GiB)), Swap: r(1), IO10: r(0), IO60: r(0), Memory10: r(0), Memory60: r(0), LoadPerCore: r(0.1)}, nil
	}
	oldDoctor := doctorHost
	t.Cleanup(func() { doctorHost, heavyOpObserve = oldDoctor, nil })
	doctorHost = func(context.Context) doctorEnv { // a passing host, so doctor's offline row is deterministic
		return doctorEnv{lookPath: func(name string) (string, error) { return "/usr/bin/" + name, nil },
			version:   func(string, ...string) (string, error) { return "git version 2.45.1", nil },
			statfs:    func(string) (uint64, uint64, error) { return 64 << 30, 1 << 20, nil },
			sandboxed: func() (bool, error) { return false, nil }}
	}
	// Admission takes two I/O readings 30 s apart (FAE-HOST-010); shorten the window.
	heavyOpPolicy = hostguard.Policy{Window: 30 * time.Millisecond, MaxGap: time.Second, Interval: 3 * time.Millisecond, Checks: 10, IOGate: 5}
	t.Cleanup(func() { heavyOpPolicy = hostguard.Policy{} })
	table, lifecycle := conformanceTable(t, home)
	covered := map[string]bool{}
	for _, tt := range table {
		covered[tt.args[0]] = true
	}
	for _, row := range append(append([][2]string{}, commandList...), hiddenCommands...) {
		for _, verb := range strings.Split(row[0], "|") {
			if !covered[verb] {
				t.Errorf("command %q has no conformance row", verb)
			}
		}
	}
	// Common flags lead, so variants may carry their own --instance or a
	// trailing "-- COMMAND"; long-running variants stop with the context.
	runInput := func(stdin, home string, args ...string) (int, string, error) {
		if stdin != "" {
			restore := withStdin(t, stdin)
			defer restore()
		}
		var out bytes.Buffer
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		err := execute(ctx, append([]string{"--home", home, "--instance", "conf"}, args...), &out)
		if err == nil {
			return 0, out.String(), nil
		}
		return exitCode(err), out.String(), err
	}
	run := func(home string, args ...string) (int, string) {
		code, out, _ := runInput("", home, args...)
		return code, out
	}
	withFlag := func(args []string, flag ...string) []string {
		i := indexOf(args, "--")
		if i < 0 {
			i = len(args)
		}
		return append(append(append([]string{}, args[:i]...), flag...), args[i:]...)
	}
	observed := map[string]map[string]bool{}
	for _, tt := range table {
		name := strings.Join(tt.args[:min(len(tt.args), 3)], " ")
		jsonArgs := append(append([]string{}, tt.args...), "--json")
		if tt.args[0] == "workflow" && len(tt.args) > 1 && (tt.args[1] == "validate" || tt.args[1] == "simulate") {
			jsonArgs = tt.args // positional offline forms reject trailing flags after the operands
		}
		valid := tt.args
		switch tt.kind {
		case "rpc":
			fail.Store(false)
			rec.take()
			if code, out := run(home, jsonArgs...); code != 0 || !json.Valid([]byte(out)) {
				t.Errorf("%s: success code=%d output=%q", name, code, out)
			}
			key := verbKey(tt.args)
			if observed[key] == nil {
				observed[key] = map[string]bool{}
			}
			for _, method := range rec.take() {
				observed[key][method] = true
			}
			fail.Store(true)
			if code, _ := run(home, jsonArgs...); code != 1 {
				t.Errorf("%s: daemon refusal code=%d want 1", name, code)
			}
			fail.Store(false)
			if code, _ := run(empty, jsonArgs...); code != 3 {
				t.Errorf("%s: unreachable code=%d want 3", name, code)
			}
		case "offline":
			if code, out := run(empty, jsonArgs...); code != 0 || !jsonStream(out) {
				t.Errorf("%s: offline code=%d output=%q", name, code, out)
			}
		case "lifecycle":
			row := strings.Join(tt.args, " ")
			lc, ok := lifecycle[row]
			if !ok {
				t.Errorf("%s: lifecycle row has no codes", name)
			}
			for _, want := range []int{0, 1, 2, 3} {
				args, ok := lc.codes[want]
				if !ok {
					for _, problem := range proofProblems(row, want, lc.proof[want]) {
						t.Errorf("%s: exit %d: %s", name, want, problem)
					}
					continue
				}
				if lc.proof[want] != "" {
					t.Errorf("%s: exit %d is both checked and proven elsewhere", name, want)
				}
				target := home
				if want == 3 {
					target = empty
				}
				restore := func() {}
				if stub := lc.stub[want]; stub != nil {
					restore = stub()
				}
				code, out, err := runInput(lc.stdin[want], target, append([]string{"--json"}, args...)...)
				restore()
				if code != want {
					t.Errorf("%s: %v code=%d want %d (%v)", name, args, code, want, err)
				}
				// FAE-CLI-001: --json output is machine-readable: nothing but JSON values.
				if !lc.passthrough && (out != "" || want == 0 && !lc.quiet) && !jsonStream(out) {
					t.Errorf("%s: %v --json output is not JSON: %q", name, args, out)
				}
			}
			valid = lc.codes[2]
			for _, want := range []int{1, 0} {
				if args, ok := lc.codes[want]; ok {
					valid = args
				}
			}
		}
		if i := indexOf(tt.args, "--authorizing-message"); i >= 0 && tt.kind == "rpc" {
			uncited := append(append([]string{}, tt.args[:i]...), tt.args[i+2:]...)
			if code, _ := run(home, append(uncited, "--json")...); code != 1 {
				t.Errorf("%s: missing authorizing message code=%d want 1", name, code)
			}
		}
		if code, _ := run(home, withFlag(valid, "--no-such-flag")...); code != 2 {
			t.Errorf("%s: unknown flag code=%d want 2", name, code)
		}
		if tt.args[0] == "workflow" && len(tt.args) > 1 && tt.args[1] != "default" && tt.args[1] != "scenario" {
			continue
		}
		for _, asJSON := range []bool{false, true} {
			args := withFlag(valid, "--help")
			if asJSON {
				args = withFlag(args, "--json")
			}
			code, out := run(home, args...)
			if code != 0 || out == "" || asJSON && !json.Valid([]byte(out)) {
				t.Errorf("%s --help json=%v: code=%d output=%q", name, asJSON, code, out)
			}
		}
	}
	// FAE-API-010: every daemon-backed verb is a thin client of exactly one RPC.
	for key, methods := range observed {
		want, ok := verbRPC[key]
		if !ok {
			t.Errorf("verb %q has no declared RPC (observed %v)", key, methods)
			continue
		}
		if len(methods) != 1 || !methods[want] {
			t.Errorf("verb %q called %v, want only %s", key, methods, want)
		}
	}
	for key := range verbRPC {
		if observed[key] == nil {
			t.Errorf("declared verb %q was never exercised", key)
		}
	}
	for _, args := range [][]string{{}, {"unknown-verb"}, {"template", "shutdown"}, {"template"}, {"backlog", "bogus"}, {"drive", "bogus"}, {"maintenance", "bogus"}, {"workflow", "validate"}, {"workflow", "bogus", "x"}, {"--bogus-global", "x", "status"}, {"gates", "bogus"}, {"gate", "bogus"}, {"heavy-op", "bogus"}, {"alerts", "bogus"}, {"ui"}, {"ui", "bogus"}, {"task"}, {"task", "bogus"}, {"task", "show"}, {"task", "delete", "--id", "t"}, {"task", "create", "--id", "t"}, {"task", "update", "--file", "f", "--title", "t"}, {"cleanup", "scratch-archive", "--source", "a", "--survivor", "b"}} {
		if code, _ := run(home, args...); code != 2 {
			t.Errorf("%v: code=%d want usage 2", args, code)
		}
	}
	if entries, err := os.ReadDir(empty); err != nil || len(entries) != 0 {
		t.Fatal("unreachable commands created instance files", entries, err)
	}
	// A cleanup scratch preview creates no lifecycle lock (FAE-CTL-043).
	if _, err := os.Lstat(filepath.Join(home, ".lifecycle")); !os.IsNotExist(err) {
		t.Fatal("scratch preview created lifecycle state", err)
	}
}

// withStdin feeds body to the in-process CLI's standard input until restored.
func withStdin(t *testing.T, body string) func() {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteString(body); err == nil {
		_, err = f.Seek(0, 0)
	}
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdin
	os.Stdin = f
	return func() { os.Stdin = saved; f.Close() }
}

// jsonStream reports whether out is one or more JSON values and nothing else.
func jsonStream(out string) bool {
	dec := json.NewDecoder(strings.NewReader(out))
	n := 0
	for {
		var v json.RawMessage
		if err := dec.Decode(&v); errors.Is(err, io.EOF) {
			return n > 0
		} else if err != nil {
			return false
		}
		n++
	}
}

// verbKey is a command's verb path: its leading non-flag words.
func verbKey(args []string) string {
	hasWorkflowMax := indexOf(args, "--max") >= 0
	for _, arg := range args {
		if strings.HasPrefix(arg, "--max=") {
			hasWorkflowMax = true
		}
	}
	if args[0] == "workflow-limit" && hasWorkflowMax {
		return "workflow-limit --max"
	}
	if args[0] == "tree" {
		return "tree"
	}
	if args[0] == "events" && indexOf(args, "--follow") >= 0 {
		return "events --follow"
	}
	i := 0
	for i < len(args) && !strings.HasPrefix(args[i], "-") && (i == 0 || !strings.Contains(args[i], "/")) {
		i++
	}
	return strings.Join(args[:i], " ")
}

func TestCLIHelpJSONAndPerCommandHelp(t *testing.T) {
	var out bytes.Buffer
	if err := execute(context.Background(), []string{"help", "--json"}, &out); err != nil {
		t.Fatal(err)
	}
	var body struct {
		Commands  []map[string]string `json:"commands"`
		ExitCodes map[string]string   `json:"exit_codes"`
		Time      string              `json:"time"`
	}
	if err := json.Unmarshal(out.Bytes(), &body); err != nil || len(body.Commands) != len(commandList) || len(body.ExitCodes) != 4 || !strings.Contains(body.Time, "daemon clock") {
		t.Fatalf("help --json %s %v", out.String(), err)
	}
	out.Reset()
	if err := execute(context.Background(), []string{"run", "--help"}, &out); err != nil || !strings.Contains(out.String(), "--prompt") || !strings.Contains(out.String(), "daemon clock") {
		t.Fatalf("run --help %q %v", out.String(), err)
	}
}

// FAE-CLI-001: help requested explicitly is success: exit 0 with the help on
// standard output (JSON under --json). A usage error exits 2 with its usage
// on standard error (the error main prints), and stdout stays empty.
func TestExplicitHelpExitsZeroAndUsageErrorsTwo(t *testing.T) {
	home := shortHome(t)
	help := [][]string{{"--help"}, {"-h"}, {"-help"}, {"--json", "--help"}, {"--help", "--json"}, {"--instance", "x", "--help"}, {"help"},
		{"workflow", "validate", "--help"}, {"workflow", "simulate", "-h"}, {"backlog", "edge", "--help"}, {"backup", "quota", "--help"},
		{"backup", "retention-schedule", "--help"}, {"backup", "creation-schedule", "--help"}, {"backup", "replication-schedule", "-h", "--json"}}
	for _, row := range append(append([][2]string{}, commandList...), hiddenCommands...) {
		for _, verb := range strings.Split(row[0], "|") {
			help = append(help, []string{verb, "--help"}, []string{verb, "-h", "--json"})
		}
	}
	for _, args := range help {
		var out bytes.Buffer
		err := execute(context.Background(), append([]string{"--home", home}, args...), &out)
		if err != nil || out.Len() == 0 || wantsJSON(args) && !json.Valid(out.Bytes()) {
			t.Errorf("%v: help exit %d %v %q", args, exitCode(err), err, out.String())
		}
	}
	for _, args := range [][]string{{}, {"--bogus-global"}, {"--home"}, {"status", "--bogus"}, {"workflow", "validate"}, {"backlog", "edge"}, {"backup", "quota", "bogus"}} {
		var out bytes.Buffer
		err := execute(context.Background(), append([]string{"--home", home}, args...), &out)
		if exitCode(err) != 2 || out.Len() != 0 || !strings.Contains(err.Error(), "help") && !strings.Contains(err.Error(), "usage") && !strings.Contains(err.Error(), "|") {
			t.Errorf("%v: usage exit %d %v %q", args, exitCode(err), err, out.String())
		}
	}
	// A bare fae prints the command list and exit codes on standard error.
	err := execute(context.Background(), []string{"--home", home}, &bytes.Buffer{})
	if exitCode(err) != 2 || !strings.Contains(err.Error(), "fae serve --workspace PATH") || !strings.Contains(err.Error(), "Exit codes:") {
		t.Errorf("bare fae: %v", err)
	}
	if entries, err := os.ReadDir(home); err != nil || len(entries) != 0 {
		t.Fatal("help created instance files", entries, err)
	}
}

func indexOf(args []string, want string) int {
	for i, a := range args {
		if a == want {
			return i
		}
	}
	return -1
}

// serveMention is a serve invocation named in a message: "serve --flag VALUE",
// "fae serve --a X --b" or "start serve with --flag".
var (
	serveMention = regexp.MustCompile(`\bserve(?: with)?((?:\s+(?:--[a-z][a-z0-9-]*|[A-Z][A-Z_]*))+)`)
	flagName     = regexp.MustCompile(`--([a-z][a-z0-9-]*)`)
)

// FAE-CLI-010 failure guidance: every serve flag a refusal or remedy names
// (in any package's string literals) is one serve accepts. The port-clash
// remedy once named a nonexistent serve --port.
func TestRemediesNameOnlyFlagsServeAccepts(t *testing.T) {
	var out bytes.Buffer
	if err := execute(context.Background(), []string{"serve", "--help", "--json"}, &out); err != nil {
		t.Fatal(err)
	}
	var help struct {
		Flags []struct {
			Name string `json:"name"`
		} `json:"flags"`
	}
	if err := json.Unmarshal(out.Bytes(), &help); err != nil {
		t.Fatal(err)
	}
	accepted := map[string]bool{}
	for _, f := range help.Flags {
		accepted[f.Name] = true
	}
	mentions := 0
	err := filepath.WalkDir(filepath.Join("..", ".."), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (strings.HasPrefix(d.Name(), ".") && d.Name() != ".." || d.Name() == "testdata" || d.Name() == "node_modules") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			text, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			for _, m := range serveMention.FindAllStringSubmatch(text, -1) {
				for _, flag := range flagName.FindAllStringSubmatch(m[1], -1) {
					mentions++
					if !accepted[flag[1]] {
						t.Errorf("%s names serve --%s, which serve does not accept: %q", path, flag[1], text)
					}
				}
			}
			return true
		})
		return nil
	})
	if err != nil || mentions < 5 {
		t.Fatalf("scanned %d serve flag mentions: %v", mentions, err)
	}
}

// FAE-CLI-001 classification: a usage error (2) is about the invocation (an
// unknown, missing or conflicting flag or operand, or a flag value that does
// not parse); the content of a file the invocation names is input the command
// validates, so content that does not parse, validate or agree with a flag is
// a validation failure (1). Nothing here reaches a daemon.
func TestNamedFileContentIsAValidationFailure(t *testing.T) {
	dir := shortHome(t) // outside the checkout: a live configuration inside one is refused
	write := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	empty := shortHome(t)
	m := []string{"--home", empty, "--authorizing-message", "m"}
	cron := `{"id":"c","kind":"cron","anchor":"2026-10-18T01:02:03Z","interval_seconds":60,"template":{"workflow_id":"default"}}`
	for _, args := range [][]string{
		{"backlog", "admit", "--file", write("malformed.json", `{"items":`)},
		{"backlog", "admit", "--file", write("unknown.json", `{"unknown":1}`)},
		{"backlog", "admit", "--file", write("trailing.json", `{"items":[],"edges":[]} {}`)},
		{"task", "create", "--file", write("task-malformed.json", `{"id":`)},
		{"task", "create", "--file", write("task-unknown.json", `{"id":"t","unknown":1}`)},
		{"task", "update", "--file", write("task-trailing.json", `{"id":"t"} {}`)},
		{"schedule", "create", "--kind", "cron", "--spec", write("cron-malformed.json", `{"id":`)},
		{"schedule", "create", "--kind", "cron", "--spec", write("cron-unknown.json", `{"id":"c","kind":"cron","unknown":true}`)},
		{"schedule", "create", "--kind", "cron", "--spec", write("cron-trailing.json", cron+` {}`)},
		{"schedule", "create", "--kind", "cron", "--spec", write("cron-loop.json", `{"id":"c","kind":"loop"}`)},
		{"schedule", "create", "--kind", "cron", "--id", "other", "--spec", write("cron-id.json", cron)},
		{"template", "target-config-simulate", "--content", write("doc.json", `{"schema_version":1}`), "--commands", write("commands.json", `{"not":"vectors"}`)},
		{"serve", "--workspace", dir, "--adapter", "direct", "--live-config", write("live-malformed.json", `{"adapter":`)},
		{"serve", "--workspace", dir, "--adapter", "direct", "--live-config", write("live-adapter.json", `{"adapter":"multiplexer"}`)},
	} {
		if err := execute(context.Background(), append(append([]string{}, args...), m...), &bytes.Buffer{}); exitCode(err) != 1 {
			t.Errorf("%v: exit %d (%v), want 1", args[:2], exitCode(err), err)
		}
	}
	// Flag values that do not parse stay usage errors.
	for _, args := range [][]string{
		{"schedule", "create", "--kind", "cron", "--id", "c", "--anchor", "yesterday"},
		{"runtime-config", "--pane-limit", "20", "--host-thresholds", "{not json"},
		{"watchdog", "--period", "x"},
	} {
		if err := execute(context.Background(), append(append([]string{}, args...), m...), &bytes.Buffer{}); exitCode(err) != 2 {
			t.Errorf("%v: exit %d (%v), want 2", args[:2], exitCode(err), err)
		}
	}
	if entries, err := os.ReadDir(empty); err != nil || len(entries) != 0 {
		t.Fatal("refused commands created instance files", entries, err)
	}
}
