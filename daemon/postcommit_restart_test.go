package daemon

import (
	"context"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTaskControlNativeLivePostCommitRestartPreservesOwnPatch(t *testing.T) {
	cfg, _, base := liveDaemonFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second+pressureAllowance)
	defer cancel()
	cfg.Live.Environment["LIVE_DAEMON_RESTART_TEST"] = "1"
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	awaitDrive(t, d, true, 1)
	d.driveStop.Store(true)
	d.signal()
	awaitDrive(t, d, false, 1)
	doc := defaults.Workflow()
	// Two full workflows plus changed-base verification intentionally exceed
	// the default single-workflow test-run budget.
	doc.Limits.TestRuns = 16
	for id, stage := range doc.Stages {
		if stage.Primitive == workflow.HostCommand && stage.TestRun {
			stage.Command = cfg.Live.AllowedCommands[0]
			doc.Stages[id] = stage
		}
	}
	if _, err = call(t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = call(t, d, "Admit", AdmitParams{ID: "postcommit", AuthorizingMessage: "message"}, "postcommit"); err != nil {
		t.Fatal(err)
	}
	for n := 0; ; n++ {
		if n > 100 {
			t.Fatal("no commit")
		}
		dispatch := stepPostCommitTask(t, ctx, d, "postcommit")
		if dispatch != nil && dispatch.Config.Primitive == workflow.VcsCommit {
			break
		}
	}
	if _, err = call(t, d, "Escalate", TaskControlParams{ID: "postcommit", Reason: "inspect commit", AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	before := d.jobs["postcommit"]
	var env envelope
	if err = decodeEnvelope(before.Payload, &env); err != nil {
		t.Fatal(err)
	}
	head := env.Job.ExpectedHead()
	if head == base || env.Job.Base != base {
		t.Fatal("execution HEAD confused with original base")
	}
	w := d.live.manager.Binding(env.Admission.RunID, env.Job.TaskID)
	// A foreign commit with exactly the same tree is not a broker-proved HEAD.
	tree := liveGit(t, w.Path, "rev-parse", "HEAD^{tree}")
	foreign := liveGit(t, w.Path, "commit-tree", tree, "-p", head, "-m", "unrecorded")
	liveGit(t, w.Path, "update-ref", "HEAD", foreign, head)
	if _, err = call(t, d, "RestartWithInformation", TaskControlParams{ID: "postcommit", Information: "reject foreign", AuthorizingMessage: "message"}, ""); err == nil {
		t.Fatal("foreign HEAD accepted")
	}
	liveGit(t, w.Path, "update-ref", "HEAD", head, foreign)
	if err = os.WriteFile(filepath.Join(w.Path, "answer.go"), []byte("tamper"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = call(t, d, "RestartWithInformation", TaskControlParams{ID: "postcommit", Information: "reject tamper", AuthorizingMessage: "message"}, ""); err == nil {
		t.Fatal("tampered tree accepted")
	}
	// Restore explicitly from the proved commit when undoing this test mutation.
	liveGit(t, w.Path, "restore", "--source=HEAD", "--worktree", "answer.go")
	if restored, err := d.live.broker.Tree(ctx, w); err != nil || restored != env.Job.Tree {
		t.Fatalf("restore durable tree: got %q, want %q: %v", restored, env.Job.Tree, err)
	}
	if !sameJobCheckpoint(before, d.jobs["postcommit"]) {
		t.Fatal("refusals mutated checkpoint")
	}
	// Older durable jobs omitted ExecutionHead. Preserve the genuine broker
	// receipt and prove that restart upgrades that snapshot without replacing Base.
	env.Job.ExecutionHead = ""
	d.mu.Lock()
	legacy, err := d.persist(ctx, d.jobs["postcommit"], env, nil)
	if err == nil {
		d.jobs["postcommit"] = legacy
	}
	d.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	// Restart the actual daemon before accepting the operator restart.
	stop(t, d)
	d, err = Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	awaitDrive(t, d, true, 1)
	d.driveStop.Store(true)
	d.signal()
	awaitDrive(t, d, false, 1)
	// This scenario repeats the complete workflow and then the changed-base
	// verification loop. Authorize their durable attempt budget explicitly;
	// restart must not reset the attempts already consumed by the first run.
	if _, err = call(t, d, "SetAttemptLimit", TaskControlParams{ID: "postcommit", AttemptLimit: 40, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = call(t, d, "RestartWithInformation", TaskControlParams{ID: "postcommit", Information: "extend answer to 43", AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	// Upstream changes concurrently after the original commit. The final tree
	// must include both complete task intent and the independent upstream file.
	tip := reconcileUpstreamCommit(t, cfg.Workspace, base, "upstream.txt", "upstream retained\n")
	liveGit(t, cfg.Workspace, "update-ref", cfg.Live.IntegrationRef, tip, base)
	for n := 0; n < 180; n++ {
		_ = decodeEnvelope(d.jobs["postcommit"].Payload, &env)
		if env.Job.State == domain.Done {
			break
		}
		if env.Job.State == domain.Escalated {
			t.Fatalf("restart escalated: %s; history: %+v", env.Job.Reason, env.Job.History)
		}
		stepPostCommitTask(t, ctx, d, "postcommit")
	}
	if env.Job.State != domain.Done {
		t.Fatalf("restart state %s", env.Job.State)
	}
	landed := liveGit(t, cfg.Workspace, "rev-parse", cfg.Live.IntegrationRef)
	if !strings.Contains(liveGit(t, cfg.Workspace, "show", landed+":answer.go"), "return 43") {
		t.Fatal("own patch lost")
	}
	if liveGit(t, cfg.Workspace, "show", landed+":upstream.txt") != "upstream retained" {
		t.Fatal("upstream patch lost")
	}
	if env.Job.Base != tip || env.Job.ExpectedHead() != landed {
		t.Fatal("final origins incorrect")
	}
	counts := map[string]int{}
	for _, p := range env.Job.Provenance {
		counts[p.Stage]++
	}
	for _, stage := range []string{"run-tests", "qa", "reviews", "oracle"} {
		if counts[stage] < 2 {
			t.Fatalf("missing fresh %s", stage)
		}
	}
}

// stepPostCommitTask steps one task by hand with the drive stopped. It holds
// the writer lock as the drive would: the status keeper reads d.jobs.
func stepPostCommitTask(t *testing.T, ctx context.Context, d *Daemon, id string) *engine.Dispatch {
	t.Helper()
	d.mu.Lock()
	locked := true
	defer func() {
		if locked {
			d.mu.Unlock()
		}
	}()
	s := d.jobs[id]
	var e envelope
	if err := decodeEnvelope(s.Payload, &e); err != nil {
		t.Fatal(err)
	}
	next, dispatch, events, err := engine.Prepare(e.Job)
	if err != nil {
		t.Fatal(err)
	}
	e.Job = next
	s, err = d.persist(ctx, s, e, events)
	if err != nil {
		t.Fatal(err)
	}
	d.jobs[id] = s
	if dispatch == nil {
		return nil
	}
	d.mu.Unlock()
	locked = false
	result, err := executeAdmitted(t, ctx, d.executor, *dispatch)
	if err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	locked = true
	e.Job, events, err = engine.Complete(e.Job, dispatch.ID, result)
	if err != nil {
		t.Fatal(err)
	}
	s, err = d.persist(ctx, s, e, events)
	if err != nil {
		t.Fatal(err)
	}
	d.jobs[id] = s
	return dispatch
}
