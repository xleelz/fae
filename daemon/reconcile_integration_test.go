package daemon

import (
	"context"
	"encoding/json"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/engine"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/workflow"
)

func reconcileUpstreamCommit(t *testing.T, repo, base, path, contents string) string {
	t.Helper()
	index := filepath.Join(t.TempDir(), "index")
	run := func(input string, args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(cleanGitEnv(), "GIT_INDEX_FILE="+index, "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@localhost", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@localhost")
		cmd.Stdin = strings.NewReader(input)
		out, e := cmd.CombinedOutput()
		if e != nil {
			t.Fatalf("upstream %v: %v %s", args, e, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("", "read-tree", base)
	blob := run(contents, "hash-object", "-w", "--stdin")
	run("", "update-index", "--add", "--cacheinfo", "100644,"+blob+","+path)
	tree := run("", "write-tree")
	return run("upstream\n", "commit-tree", tree, "-p", base)
}
func installReconcileWorkflow(t *testing.T, d *Daemon, cfg Config) {
	t.Helper()
	doc := defaults.Workflow()
	for id, s := range doc.Stages {
		if s.Primitive == workflow.HostCommand && s.TestRun {
			s.Command = cfg.Live.AllowedCommands[0]
			doc.Stages[id] = s
		}
	}
	if _, e := call(t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"}, ""); e != nil {
		t.Fatal(e)
	}
}
func TestDaemonChangedBaseReverifiesBeforeLanding(t *testing.T) {
	cfg, ctx, base := liveDaemonFixture(t)
	tip := reconcileUpstreamCommit(t, cfg.Workspace, base, "upstream.txt", "independent upstream bytes\n")
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer stop(t, d)
	authorize(t, d)
	installReconcileWorkflow(t, d, cfg)
	if _, e = call(t, d, "Admit", AdmitParams{ID: "reconcile-clean", AuthorizingMessage: "message"}, "reconcile-clean"); e != nil {
		t.Fatal(e)
	}
	liveGit(t, cfg.Workspace, "update-ref", cfg.Live.IntegrationRef, tip, base)
	snapshot := awaitReconcile(t, d, "reconcile-clean", false)
	var env envelope
	if e = json.Unmarshal(snapshot.Payload, &env); e != nil {
		t.Fatal(e)
	}
	counts := map[string]int{}
	latest := map[string]string{}
	for _, p := range env.Job.Provenance {
		counts[p.Stage]++
		latest[p.Stage] = p.Tree
	}
	for _, stage := range []string{"run-tests", "qa", "reviews", "oracle"} {
		if counts[stage] < 2 || latest[stage] != env.Job.Tree {
			t.Fatalf("%s not rerun on integrated tree: counts=%v latest=%v final=%s", stage, counts, latest, env.Job.Tree)
		}
	}
	if env.Job.Base != tip {
		t.Fatalf("durable base not updated: %s != %s", env.Job.Base, tip)
	}
	landed := liveGit(t, cfg.Workspace, "rev-parse", cfg.Live.IntegrationRef)
	if liveGit(t, cfg.Workspace, "rev-parse", landed+"^") != tip {
		t.Fatal("landed commit not based on current upstream")
	}
	if !strings.Contains(liveGit(t, cfg.Workspace, "show", landed+":answer.go"), "return 42") {
		t.Fatal("own implementation lost")
	}
	if liveGit(t, cfg.Workspace, "show", landed+":upstream.txt") != "independent upstream bytes" {
		t.Fatal("upstream lost")
	}
	if liveGit(t, cfg.Workspace, "rev-parse", "HEAD") != base || liveGit(t, cfg.Workspace, "status", "--porcelain") != "" {
		t.Fatal("primary checkout changed")
	}
	events, e := d.store.JobEvents(context.Background(), "reconcile-clean", 0, 1000)
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, event := range events {
		raw, _ := json.Marshal(event)
		found = found || strings.Contains(string(raw), "tree-reconciled")
	}
	if !found {
		t.Fatal("missing durable reconcile event")
	}
}
func TestDaemonConflictLostHunkNeverLands(t *testing.T) {
	cfg, ctx, base := liveDaemonFixture(t)
	cfg.Live.Environment["LIVE_DAEMON_DROP_HUNK"] = "1"
	tip := reconcileUpstreamCommit(t, cfg.Workspace, base, "answer.go", "package sample\n\nfunc answer() int { return 7 }\n")
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer stop(t, d)
	authorize(t, d)
	installReconcileWorkflow(t, d, cfg)
	if _, e = call(t, d, "Admit", AdmitParams{ID: "reconcile-conflict", AuthorizingMessage: "message"}, "reconcile-conflict"); e != nil {
		t.Fatal(e)
	}
	liveGit(t, cfg.Workspace, "update-ref", cfg.Live.IntegrationRef, tip, base)
	snapshot := awaitReconcile(t, d, "reconcile-conflict", true)
	var env envelope
	if err := json.Unmarshal(snapshot.Payload, &env); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range env.Job.History {
		if event.Kind == "execution-diagnostic" && strings.Contains(event.Cause, "own-hunk-loss") && strings.Contains(event.Cause, "answer.go") {
			found = true
		}
	}
	if !found {
		t.Fatal("explicit durable conflict diagnostic absent")
	}
	if !env.Job.Parked || env.Job.Reason != "UncertainDispatch" {
		t.Fatal("conflict not conservatively parked")
	}
	if liveGit(t, cfg.Workspace, "rev-parse", cfg.Live.IntegrationRef) != tip {
		t.Fatal("conflicted task changed tip")
	}
	if liveGit(t, cfg.Workspace, "rev-parse", "HEAD") != base || liveGit(t, cfg.Workspace, "status", "--porcelain") != "" {
		t.Fatal("primary checkout changed")
	}
}

func awaitReconcile(t *testing.T, d *Daemon, id string, parked bool) controlplane.JobSnapshot {
	t.Helper()
	deadline := time.Now().Add(75 * time.Second)
	var snapshot controlplane.JobSnapshot
	var env envelope
	for time.Now().Before(deadline) {
		var err error
		snapshot, err = d.store.GetJob(context.Background(), id)
		if err == nil && json.Unmarshal(snapshot.Payload, &env) == nil {
			if parked && env.Job.Parked || !parked && snapshot.State == "Done" {
				return snapshot
			}
			if !parked && env.Job.Parked {
				t.Fatalf("unexpected park: %s stage=%s", env.Job.Reason, snapshot.State)
			}
		}
		select {
		case <-d.done:
			t.Fatalf("daemon stopped: %v", d.err)
		default:
		}
		time.Sleep(40 * time.Millisecond)
	}
	t.Fatalf("task %s timeout state=%s reason=%s", id, snapshot.State, env.Job.Reason)
	return snapshot
}

func TestDaemonConflictRemediationRefreshesApprovals(t *testing.T) {
	cfg, ctx, base := liveDaemonFixture(t)
	tip := reconcileUpstreamCommit(t, cfg.Workspace, base, "answer.go", "package sample\n\nfunc answer() int { return 7 }\n")
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	installReconcileWorkflow(t, d, cfg)
	if _, err = call(t, d, "Admit", AdmitParams{ID: "conflict-remediated", AuthorizingMessage: "message"}, "conflict-remediated"); err != nil {
		t.Fatal(err)
	}
	liveGit(t, cfg.Workspace, "update-ref", cfg.Live.IntegrationRef, tip, base)
	snapshot := awaitReconcile(t, d, "conflict-remediated", false)
	var env envelope
	if err = json.Unmarshal(snapshot.Payload, &env); err != nil {
		t.Fatal(err)
	}
	if env.Job.Counts["implement"] != 2 || env.Job.Counts["reviews"] != 2 || env.Job.Counts["qa"] != 2 {
		t.Fatalf("conflict did not repeat normal approvals: %+v", env.Job.Counts)
	}
	named := false
	for _, evidence := range env.Job.Evidence {
		if evidence.Type == "ReconcileReport" && evidence.Fields["result"] == "conflicted" {
			raw, _ := json.Marshal(evidence.Fields["paths"])
			named = string(raw) == `["answer.go"]`
			key, _ := evidence.Fields["plan_key"].(string)
			if _, err = d.store.GetMarker(ctx, key); err != nil {
				t.Fatal("conflict intent not durable", err)
			}
		}
	}
	if !named {
		t.Fatal("named conflict artifact missing")
	}
	// An omitted optional input in a custom graph cannot erase the durable
	// contract or authorize an unrelated implementation turn.
	binding := d.live.manager.Binding(env.Admission.RunID, "conflict-remediated")
	if err := d.live.checkConflictResolution(ctx, engine.Dispatch{TaskID: "conflict-remediated", Base: tip}, binding); err == nil || !strings.Contains(err.Error(), "original context") {
		t.Fatal("custom graph omitted conflict guard", err)
	}

	final := liveGit(t, cfg.Workspace, "rev-parse", cfg.Live.IntegrationRef)
	if final == tip || liveGit(t, cfg.Workspace, "rev-parse", final+"^") != tip {
		t.Fatal("resolved commit has wrong parent")
	}
	if !strings.Contains(liveGit(t, cfg.Workspace, "show", final+":answer.go"), "return 42") {
		t.Fatal("own hunk lost")
	}
	// Simulate a custom Commit dispatch after a corrupt role dropped original
	// hunks. The independent VCS-boundary guard must refuse without any inputs.
	liveGit(t, binding.Path, "update-ref", "refs/heads/"+binding.Branch, tip, final)
	if err := os.WriteFile(filepath.Join(binding.Path, "answer.go"), []byte("package sample\n\nfunc answer() int { return 7 }\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := d.live.verifyActiveConflict(ctx, engine.Dispatch{TaskID: "conflict-remediated", Base: tip}, binding); err == nil || !strings.Contains(err.Error(), "own-hunk-loss") {
		t.Fatal("commit bypassed durable original contract", err)
	}
	if liveGit(t, cfg.Workspace, "rev-parse", cfg.Live.IntegrationRef) != final {
		t.Fatal("refusal changed landed target")
	}

	if liveGit(t, cfg.Workspace, "rev-parse", "HEAD") != base || liveGit(t, cfg.Workspace, "status", "--porcelain") != "" {
		t.Fatal("primary checkout changed")
	}
}
