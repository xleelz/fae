package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/controller"
	"github.com/xleelz/fae/controlplane"
)

// FAE-WF-005 with FAE-ROB-021: an admission whose row commits but whose
// acknowledgement is lost (the "admit-acknowledgement" fault seam) leaves a
// stored task the drive does not hold. A concurrent Admit of the same
// identity under another key, whose existing-task read ran before that
// commit, is still refused by identity before any fork: the plane is read
// again under the writer lock for that identity. The stored task's worktree,
// branch and recorded base stay as they were, though the integration tip has
// moved.
func TestAdmitRereadsAnIdentityWhoseAdmissionOutcomeIsUnknown(t *testing.T) {
	cfg, ctx, base := liveDaemonFixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	if _, err = controllerCall(t, d, controller.Params{Action: "stop-drive", AuthorizingMessage: "message"}); err != nil {
		t.Fatal(err)
	}
	awaitDrive(t, d, false, 1)
	raw, _ := json.Marshal(AdmitParams{ID: "bound", Content: "a second binding", AuthorizingMessage: "message"})
	// The second admission reads the plane before the first commits, as an
	// Admit RPC does before it takes the writer lock.
	second := d.readBeforeWriterLock(context.Background(), "Admit", raw)
	if stored, read := readsBefore(second).existingTask("bound"); !read || stored {
		t.Fatal("the second admission's read did not precede the first binding", read, stored)
	}
	d.mu.Lock()
	d.testFault = func(point, id string) error {
		if point == "admit-acknowledgement" && id == "bound" {
			return errors.New("admission acknowledgement lost")
		}
		return nil
	}
	d.mu.Unlock()
	if _, err = call(t, d, "Admit", AdmitParams{ID: "bound", AuthorizingMessage: "message"}, "bound"); err == nil || !strings.Contains(err.Error(), "acknowledgement lost") {
		t.Fatal("the first admission did not lose its acknowledgement", err)
	}
	d.mu.Lock()
	d.testFault = nil
	_, held := d.jobs["bound"]
	d.mu.Unlock()
	stored, err := d.store.GetJob(ctx, "bound")
	if err != nil || held {
		t.Fatal("the first admission did not commit uncached", held, err)
	}
	advanceIntegration(t, cfg, "MOVED.md")
	w := d.live.manager.Binding(d.config.RunID, "bound")
	// The second admission takes the writer lock after the first released it.
	_, refused := admitted(t, "second admission", func() (any, error) {
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.admit(second, "bound-again", raw, controlplane.Audit{Actor: "operator", Action: "Admit", MessageID: "message", Inputs: raw, Outcome: "ok"})
	})
	after, err := d.store.GetJob(ctx, "bound")
	if err != nil {
		t.Fatal(err)
	}
	if e := snapshotEnvelope(t, after); e.Job.Base != base || after.Version != stored.Version {
		t.Fatal("existing binding changed", e.Job.Base, after.Version)
	}
	if head := liveGit(t, w.Path, "rev-parse", "HEAD"); head != base || liveGit(t, cfg.Workspace, "rev-parse", "refs/heads/"+w.Branch) != base {
		t.Fatal("existing task's worktree re-forked to", head)
	}
	if status := liveGit(t, w.Path, "status", "--porcelain"); status != "" {
		t.Fatal("existing task's worktree changed", status)
	}
	if refused == nil || !strings.Contains(refused.Error(), "already exists") {
		t.Fatal("existing task not refused by identity", refused)
	}
}

// resumeRunOutcome decodes a ResumeRun result: its run, resumed tasks and the
// reason each skipped task was skipped.
func resumeRunOutcome(t *testing.T, result json.RawMessage) (string, []ResumedTask, map[string]string) {
	t.Helper()
	var out struct {
		RunID   string        `json:"run_id"`
		Resumed []ResumedTask `json:"resumed"`
		Skipped []SkippedTask `json:"skipped"`
	}
	if err := json.Unmarshal(result, &out); err != nil {
		t.Fatal(err)
	}
	skipped := map[string]string{}
	for _, s := range out.Skipped {
		skipped[s.ID] = s.Reason
	}
	return out.RunID, out.Resumed, skipped
}

// FAE-ROB-020/-021 with FAE-CLI-013: run-wide resume reads the active backlog
// and, when no run is named, the latest run before it takes the writer lock.
// While either read is slowed, an admission and a task control return in
// under 1 s, and resume still resolves its run. An admission that lands
// between the latest-run read and the lock makes resume read the latest run
// again under the lock.
func TestResumeRunReadsTheBacklogAndLatestRunBeforeTheWriterLock(t *testing.T) {
	d := restartedWithFinished(t, "r", "done-a")
	if _, err := call(t, d, "Admit", AdmitParams{ID: "pending", RunID: "r", Tree: "tree", AuthorizingMessage: "message"}, "pending"); err != nil {
		t.Fatal(err)
	}
	started := slowPlaneRead(d, "active-backlog", 3*time.Second)
	slowed := slowedCall(t, d, started, "ResumeRun", ResumeRunParams{RunID: "r", AuthorizingMessage: "message"}, "")
	writesDuring(t, d, slowed, "fast-a")
	run, resumed, skipped := resumeRunOutcome(t, slowedResult(t, slowed))
	if run != "r" || len(resumed) != 0 || skipped["done-a"] != "terminal" || skipped["pending"] == "" || len(skipped) != 2 {
		t.Fatal("run resume", run, resumed, skipped)
	}
	// The default run is the latest: fast-a's, the instance run.
	started = slowPlaneRead(d, "latest-run", 3*time.Second)
	slowed = slowedCall(t, d, started, "ResumeRun", ResumeRunParams{AuthorizingMessage: "message"}, "")
	writesDuring(t, d, slowed, "fast-b")
	run, resumed, skipped = resumeRunOutcome(t, slowedResult(t, slowed))
	if run != d.config.RunID || len(resumed) != 0 || skipped["fast-a"] == "" || skipped["fast-b"] == "" || len(skipped) != 2 {
		t.Fatal("default run resume", run, resumed, skipped)
	}
	// The latest run becomes r, and is read as r before the lock; fast-c,
	// admitted to the instance run while the members of r are read, makes
	// the instance run the latest again.
	if _, err := call(t, d, "Admit", AdmitParams{ID: "late", RunID: "r", Tree: "tree", AuthorizingMessage: "message"}, "late"); err != nil {
		t.Fatal(err)
	}
	started = slowPlaneRead(d, "run-tasks/r", 3*time.Second)
	slowed = slowedCall(t, d, started, "ResumeRun", ResumeRunParams{AuthorizingMessage: "message"}, "")
	writesDuring(t, d, slowed, "fast-c")
	run, _, skipped = resumeRunOutcome(t, slowedResult(t, slowed))
	if run != d.config.RunID || skipped["fast-c"] == "" || skipped["late"] != "" {
		t.Fatal("resume kept a latest run read before an admission", run, skipped)
	}
}

// FAE-ROB-020/-021 with FAE-CLI-015: a backlog edge edit reads which of the
// graph's tasks the plane stores before it takes the writer lock. While that
// read is slowed, an admission, a task control and another edge edit return
// in under 1 s. The edit still knows both the finished prerequisite read
// before the lock and the one whose edge was added during that read, which
// it reads under the lock.
func TestBacklogEdgeReadsKnownTasksBeforeTheWriterLock(t *testing.T) {
	d := restartedWithFinished(t, "r", "done-a", "done-b")
	if _, err := call(t, d, "BacklogAdmit", BacklogParams{Items: []BacklogItemParams{{AdmitParams: AdmitParams{ID: "dependent", Tree: "tree", Priority: 10, Content: "accept dependent"}}}, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	started := slowPlaneRead(d, "existing/done-a", 3*time.Second)
	slowed := slowedCall(t, d, started, "BacklogEdge", BacklogEdgeParams{BacklogEdge: controlplane.BacklogEdge{From: "done-a", To: "dependent", Kind: "blocking"}, AuthorizingMessage: "message"}, "")
	writesDuring(t, d, slowed, "fast")
	begun := time.Now()
	_, err := callOnce(t, d, "BacklogEdge", BacklogEdgeParams{BacklogEdge: controlplane.BacklogEdge{From: "done-b", To: "dependent", Kind: "blocking"}, AuthorizingMessage: "message"}, "")
	if elapsed := time.Since(begun); err != nil || elapsed >= time.Second {
		t.Fatalf("an edge edit waited for the slowed known-task read: %s: %v", elapsed, err)
	}
	if body := string(slowedResult(t, slowed)); !strings.Contains(body, `"effect":"unchanged"`) || !strings.Contains(body, `"after":{"ready":true`) {
		t.Fatalf("finished prerequisites not known and satisfied: %s", body)
	}
}
