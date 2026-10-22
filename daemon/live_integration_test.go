package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/defaults"
	agentruntime "github.com/xleelz/fae/runtime"
	"github.com/xleelz/fae/workflow"
)

const liveTestSource = "package sample\n\nimport \"testing\"\n\nfunc TestBehavior(t *testing.T) {\n\tif answer() != 42 {\n\t\tt.Fatal(\"got 0 want 42\")\n\t}\n}\n"

// This subprocess is an explicit protocol fixture, not a provider impersonation.
// It makes real isolated source edits and emits the same typed handoffs a bridge
// must emit; every host and VCS effect is performed by the production broker.
func TestLiveDaemonBridgeFixture(t *testing.T) {
	if os.Getenv("LIVE_DAEMON_BRIDGE") != "1" {
		return
	}
	var c agentruntime.BridgeConfig
	raw, err := os.ReadFile(os.Args[len(os.Args)-1])
	if err != nil || json.Unmarshal(raw, &c) != nil {
		os.Exit(21)
	}
	if os.Getenv("LIVE_DAEMON_WAIT") == "1" {
		for {
			time.Sleep(time.Second)
		}
	}
	var brief []struct{ Kind, Stage string }
	if json.Unmarshal(c.Brief, &brief) != nil {
		os.Exit(22)
	}
	stage := ""
	for _, b := range brief {
		if b.Kind == "stage" {
			stage = b.Stage
		}
	}
	fixtures := defaults.HappyScript()
	steps := fixtures[stage]
	if len(steps) == 0 {
		os.Exit(23)
	}
	output := steps[0].Output
	if len(steps[0].Reviews) > 0 {
		index := 0
		switch c.Role {
		case "reviewer-quality":
			index = 1
		case "reviewer-security":
			index = 2
		case "reviewer-architecture":
			index = 3
		}
		output = steps[0].Reviews[index]
	}
	if stage == "test-first" || stage == "implement" {
		var fields map[string]any
		if json.Unmarshal(output, &fields) != nil {
			os.Exit(24)
		}
		turnTestSource := liveTestSource
		if os.Getenv("LIVE_DAEMON_RESTART_TEST") == "1" {
			prior, _ := os.ReadFile(filepath.Join(c.Worktree, "answer.go"))
			tests, _ := os.ReadFile(filepath.Join(c.Worktree, "answer_test.go"))
			if strings.Contains(string(prior), "return 42") || strings.Contains(string(tests), "!= 43") {
				turnTestSource = strings.Replace(liveTestSource, "!= 42", "!= 43", 1)
			}
		}
		fields["source"] = turnTestSource
		output, _ = json.Marshal(fields)
		name, contents := "answer_test.go", turnTestSource
		if stage == "implement" {
			name, contents = "answer.go", "package sample\n\nfunc answer() int { return 42 }\n"
			if strings.Contains(turnTestSource, "!= 43") {
				contents = strings.Replace(contents, "return 42", "return 43", 1)
			}
			prior, _ := os.ReadFile(filepath.Join(c.Worktree, name))
			if strings.Contains(string(prior), "<<<<<<< ") {
				// A conflict fixture resolves only after the engine supplied
				// named immutable conflict context in the bound artifact.
				if !strings.Contains(string(c.Brief), `"result":"conflicted"`) || !strings.Contains(string(c.Brief), `"paths":["answer.go"]`) {
					os.Exit(27)
				}
				if os.Getenv("LIVE_DAEMON_DROP_HUNK") == "1" {
					contents = "package sample\n\nfunc answer() int { return 7 }\n"
				}
			}
		}
		if os.WriteFile(filepath.Join(c.Worktree, name), []byte(contents), 0600) != nil {
			os.Exit(25)
		}
	}
	out, _ := json.Marshal(agentruntime.Handoff{DispatchID: c.DispatchID, SessionID: c.SessionID, TaskID: c.TaskID, Output: output})
	if os.WriteFile(c.Handoff, out, 0600) != nil {
		os.Exit(26)
	}
	os.Exit(0)
}
func liveGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(cleanGitEnv(), "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@localhost", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@localhost")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
func liveDaemonFixture(t *testing.T) (Config, context.Context, string) {
	cfg, ctx := fixture(t)
	liveGit(t, cfg.Workspace, "init")
	for name, body := range map[string]string{"go.mod": "module example.com/live\n\ngo 1.23\n", "answer.go": "package sample\n\nfunc answer() int { return 0 }\n"} {
		if err := os.WriteFile(filepath.Join(cfg.Workspace, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	liveGit(t, cfg.Workspace, "add", ".")
	liveGit(t, cfg.Workspace, "commit", "-m", "base")
	base := liveGit(t, cfg.Workspace, "rev-parse", "HEAD")
	liveGit(t, cfg.Workspace, "branch", "fae/integration")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Live = &LiveConfig{Command: []string{exe, "-test.run=^TestLiveDaemonBridgeFixture$", "--", "{config}"}, Environment: map[string]string{"LIVE_DAEMON_BRIDGE": "1"}, AllowedCommands: [][]string{{"go", "test", "-json", "-count=1", "-a", "-x", "./..."}, {"gofmt", "-l", "."}}, Files: []string{"answer.go", "answer_test.go"}, Base: base, IntegrationRef: "refs/heads/fae/integration", CommitMessage: "live task verified bytes", TestFile: "answer_test.go", TestID: "TestBehavior", ExpectedFailure: "got 0 want 42", HostLedger: filepath.Join(filepath.Dir(cfg.Root), "host-ledger.json"), HeavyCapacity: 4, TimeoutSeconds: 30}
	return cfg, ctx, base
}
func TestDaemonLiveEffectsLifecycle(t *testing.T) {
	cfg, ctx, base := liveDaemonFixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	doc := defaults.Workflow()
	for id, stage := range doc.Stages {
		if stage.Primitive == workflow.HostCommand && stage.TestRun {
			stage.Command = cfg.Live.AllowedCommands[0]
			doc.Stages[id] = stage
		}
	}
	if _, err = call(t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = call(t, d, "Admit", AdmitParams{ID: "live-one", AuthorizingMessage: "message"}, "live-one"); err != nil {
		t.Fatal(err)
	}
	snapshot := awaitState(t, d, "live-one", "Done")
	var e envelope
	if err = json.Unmarshal(snapshot.Payload, &e); err != nil {
		t.Fatal(err)
	}
	if e.Job.ExecutionKind != "live" || len(e.Job.Provenance) < 20 {
		t.Fatal("missing live chain")
	}
	for _, p := range e.Job.Provenance {
		if p.ExecutionKind != "live" {
			t.Fatal("mixed scripted evidence")
		}
	}
	tip := liveGit(t, cfg.Workspace, "rev-parse", cfg.Live.IntegrationRef)
	if tip == base {
		t.Fatal("integration never advanced")
	}
	if got := liveGit(t, cfg.Workspace, "show", tip+":answer.go"); !strings.Contains(got, "return 42") {
		t.Fatal("real implementation absent", got)
	}
	if got := liveGit(t, cfg.Workspace, "rev-parse", "HEAD"); got != base {
		t.Fatal("primary head changed")
	}
	if got := liveGit(t, cfg.Workspace, "status", "--porcelain"); got != "" {
		t.Fatal("primary checkout modified", got)
	}
	pool, err := d.live.guard.host.Snapshot(ctx)
	if err != nil || len(pool.Holders) != 0 || len(pool.Waiting) != 0 {
		t.Fatal("leaked host leases", pool, err)
	}
	role, err := d.store.LeaseSnapshot(ctx, fmt.Sprintf("role/%x", sha256.Sum256([]byte("default\x00global\x00spec"))))
	if err != nil || len(role.Holders) != 0 || len(role.Waiting) != 0 {
		t.Fatal("leaked role leases", role, err)
	}
	stop(t, d)
	d, err = Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	again := awaitState(t, d, "live-one", "Done")
	if again.Version != snapshot.Version {
		t.Fatal("completed live job replayed")
	}
}
func TestDaemonLiveInterruptedTurnParks(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	cfg.Live.Environment["LIVE_DAEMON_WAIT"] = "1"
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	doc := defaults.Workflow()
	for id, stage := range doc.Stages {
		if stage.Primitive == workflow.HostCommand && stage.TestRun {
			stage.Command = cfg.Live.AllowedCommands[0]
			doc.Stages[id] = stage
		}
	}
	if _, err = call(t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = call(t, d, "Admit", AdmitParams{ID: "interrupted", AuthorizingMessage: "message"}, "interrupted"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	held := false
	for time.Now().Before(deadline) {
		pool, e := d.live.guard.host.Snapshot(ctx)
		if e == nil && len(pool.Holders) > 0 {
			held = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !held {
		t.Fatal("live turn never acquired host lease")
	}
	stop(t, d)
	d, err = Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := d.store.GetJob(ctx, "interrupted")
	if err != nil {
		t.Fatal(err)
	}
	var e envelope
	if err = json.Unmarshal(snapshot.Payload, &e); err != nil {
		t.Fatal(err)
	}
	if e.Job.ExecutionKind != "live" || !e.Job.Parked || e.Job.Reason != "UncertainDispatch" {
		t.Fatal("uncertain execution not held")
	}
}
