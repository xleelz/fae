package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/controller"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/engine"
)

// slowPlaneRead makes the named plane read, which runs without the writer
// lock, take delay once; the channel closes when the read begins.
func slowPlaneRead(d *Daemon, name string, delay time.Duration) <-chan struct{} {
	started := make(chan struct{})
	var once sync.Once
	d.mu.Lock()
	d.testFault = func(point, id string) error {
		if point == "plane-read" && id == name {
			once.Do(func() {
				close(started)
				time.Sleep(delay)
			})
		}
		return nil
	}
	d.mu.Unlock()
	return started
}

type rpcOutcome struct {
	response api.Response
	err      error
}

// slowedCall issues one RPC whose plane read is slowed, and waits until that
// read begins.
func slowedCall(t *testing.T, d *Daemon, started <-chan struct{}, method string, p any, key string) <-chan rpcOutcome {
	t.Helper()
	raw, _ := json.Marshal(p)
	token, err := os.ReadFile(d.layout.Credential)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan rpcOutcome, 1)
	go func() {
		r, err := api.Call(context.Background(), d.layout.Socket, token, api.Request{Version: 1, Method: method, Params: raw, Key: key})
		done <- rpcOutcome{r, err}
	}()
	select {
	case <-started:
	case <-time.After(30 * time.Second):
		t.Fatalf("the slowed plane read of %s never began", method)
	}
	return done
}

// writesDuring asserts that an admission and a task control on it each
// return in under 1 s while slowed is still in flight (FAE-ROB-021).
func writesDuring(t *testing.T, d *Daemon, slowed <-chan rpcOutcome, id string) {
	t.Helper()
	for _, w := range []struct {
		method string
		params any
		key    string
	}{
		{"Admit", AdmitParams{ID: id, Tree: "tree", AuthorizingMessage: "message"}, id},
		{"Reprioritize", TaskControlParams{ID: id, Priority: 20, AuthorizingMessage: "message"}, ""},
	} {
		begun := time.Now()
		_, err := callOnce(t, d, w.method, w.params, w.key)
		if elapsed := time.Since(begun); err != nil || elapsed >= time.Second {
			t.Fatalf("%s waited for the slowed plane read: %s: %v", w.method, elapsed, err)
		}
		select {
		case <-slowed:
			t.Fatalf("the slowed call finished before %s returned", w.method)
		default:
		}
	}
}

// slowedResult waits for the slowed call's result, which must succeed.
func slowedResult(t *testing.T, slowed <-chan rpcOutcome) json.RawMessage {
	t.Helper()
	select {
	case r := <-slowed:
		if r.err != nil {
			t.Fatal("the slowed call failed: ", r.err)
		}
		return r.response.Result
	case <-time.After(30 * time.Second):
		t.Fatal("the slowed call never returned")
	}
	return nil
}

// stopDrive stops the drive so that admitted tasks stay Pending.
func stopDrive(t *testing.T, d *Daemon) {
	t.Helper()
	awaitDrive(t, d, true, 1)
	d.driveStop.Store(true)
	d.signal()
	awaitDrive(t, d, false, 1)
}

// shortFixture is a scripted fixture whose short workflow ends in one turn.
func shortFixture(t *testing.T) (Config, context.Context) {
	t.Helper()
	cfg, ctx := fixture(t)
	ok := engine.ScriptStep{Output: json.RawMessage(`{"ok":true}`)}
	cfg.Executor = engine.NewScripted(map[string][]engine.ScriptStep{"work": {ok, ok, ok, ok}})
	return cfg, ctx
}

// installShort installs the short workflow.
func installShort(t *testing.T, d *Daemon) {
	t.Helper()
	doc := shortWorkflow()
	if _, err := call(t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
}

// restartedWithFinished finishes the given tasks in run, then restarts the
// daemon, which no longer materialises them (FAE-CP-009), with its drive
// stopped.
func restartedWithFinished(t *testing.T, run string, ids ...string) *Daemon {
	t.Helper()
	cfg, ctx := shortFixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	authorize(t, d)
	installShort(t, d)
	for _, id := range ids {
		if _, err = call(t, d, "Admit", AdmitParams{ID: id, RunID: run, WorkflowID: "short", Tree: "tree", AuthorizingMessage: "message"}, id); err != nil {
			stop(t, d)
			t.Fatal(err)
		}
	}
	for _, id := range ids {
		awaitState(t, d, id, "Done")
	}
	stop(t, d)
	if d, err = Start(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stop(t, d) })
	d.mu.Lock()
	_, loaded := d.jobs[ids[0]]
	d.mu.Unlock()
	if loaded {
		t.Fatal("startup materialised a finished task")
	}
	stopDrive(t, d)
	return d
}

// FAE-ROB-020/-021 with FAE-WF-005: Admit reads whether the plane stores its
// identity before it takes the writer lock. While that read is slowed,
// another admission and a task control return in under 1 s. The identity is
// still refused when it exists: one the drive holds, and one only the plane
// holds, a task finished before a restart.
func TestAdmitDoesNotWaitForAnExistingTaskRead(t *testing.T) {
	d := restartedWithFinished(t, "finished", "finished")
	started := slowPlaneRead(d, "existing/slow", 3*time.Second)
	slowed := slowedCall(t, d, started, "Admit", AdmitParams{ID: "slow", Tree: "tree", AuthorizingMessage: "message"}, "slow")
	writesDuring(t, d, slowed, "fast")
	slowedResult(t, slowed)
	for _, id := range []string{"slow", "finished"} {
		_, err := callOnce(t, d, "Admit", AdmitParams{ID: id, Tree: "tree", AuthorizingMessage: "message"}, id+"-again")
		if err == nil || !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("existing task %s not refused by identity: %v", id, err)
		}
	}
}

// FAE-ROB-020/-021 with FAE-CLI-013: run-wide resume reads the run's members
// and their finished rows before it takes the writer lock. While that read is
// slowed, an admission and a task control return in under 1 s, and resume
// still reports the member finished before the restart as terminal.
func TestResumeRunDoesNotHoldTheWriterLockAcrossItsReads(t *testing.T) {
	d := restartedWithFinished(t, "r", "done-a")
	if _, err := call(t, d, "Admit", AdmitParams{ID: "pending", RunID: "r", Tree: "tree", AuthorizingMessage: "message"}, "pending"); err != nil {
		t.Fatal(err)
	}
	started := slowPlaneRead(d, "run-tasks/r", 3*time.Second)
	slowed := slowedCall(t, d, started, "ResumeRun", ResumeRunParams{RunID: "r", AuthorizingMessage: "message"}, "")
	writesDuring(t, d, slowed, "fast")
	result := slowedResult(t, slowed)
	var out struct {
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
	if len(out.Resumed) != 0 || skipped["done-a"] != "terminal" || skipped["pending"] == "" || len(skipped) != 2 {
		t.Fatalf("run resume %s", result)
	}
}

// FAE-ROB-020/-021 with FAE-CLI-015: a backlog edge edit reads its dependent's
// and prerequisites' rows before it takes the writer lock. While the read of
// a prerequisite finished before a restart is slowed, an admission and a
// task control return in under 1 s, and the edge's readiness effect still
// counts that prerequisite as satisfied.
func TestBacklogEdgeDoesNotHoldTheWriterLockAcrossItsReads(t *testing.T) {
	d := restartedWithFinished(t, "r", "done-a")
	if _, err := call(t, d, "BacklogAdmit", BacklogParams{Items: []BacklogItemParams{{AdmitParams: AdmitParams{ID: "dependent", Tree: "tree", Priority: 10, Content: "accept dependent"}}}, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	started := slowPlaneRead(d, "stored/done-a", 3*time.Second)
	slowed := slowedCall(t, d, started, "BacklogEdge", BacklogEdgeParams{BacklogEdge: controlplane.BacklogEdge{From: "done-a", To: "dependent", Kind: "blocking"}, AuthorizingMessage: "message"}, "")
	writesDuring(t, d, slowed, "fast")
	if body := string(slowedResult(t, slowed)); !strings.Contains(body, `"effect":"unchanged"`) || !strings.Contains(body, `"after":{"ready":true`) {
		t.Fatalf("finished prerequisite not satisfied: %s", body)
	}
}

// FAE-ROB-020/-021: the drive reads a released task's dependency rows without
// the writer lock. While that read is slowed, an admission returns in under
// 1 s, and the released task still runs to Done.
func TestAdmitDoesNotWaitForTheReleaseDependencyRead(t *testing.T) {
	cfg, ctx := shortFixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	installShort(t, d)
	started := slowPlaneRead(d, "release-dependencies/slow", 3*time.Second)
	if _, err = call(t, d, "Admit", AdmitParams{ID: "slow", WorkflowID: "short", Tree: "tree", AuthorizingMessage: "message"}, "slow"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(30 * time.Second):
		t.Fatal("the release dependency read never began")
	}
	begun := time.Now()
	_, err = callOnce(t, d, "Admit", AdmitParams{ID: "fast", WorkflowID: "short", Tree: "tree", AuthorizingMessage: "message"}, "fast")
	if elapsed := time.Since(begun); err != nil || elapsed >= time.Second {
		t.Fatalf("admission waited for the release dependency read: %s: %v", elapsed, err)
	}
	for _, id := range []string{"slow", "fast"} {
		awaitState(t, d, id, "Done")
	}
}

// FAE-SCH-010: a round records a reason for every ready task it does not
// dispatch, including one whose release could not read its dependency rows,
// one that changed while its release ran without the writer lock, and every
// task of a round that ends early: an unverifiable checkpoint, a drain, or a
// failed round read.
func TestDriveRecordsWithheldReasonsForReleaseAndEarlyRounds(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	stopDrive(t, d)
	for _, id := range []string{"raced", "unread"} {
		if _, err = call(t, d, "Admit", AdmitParams{ID: id, Tree: "tree", AuthorizingMessage: "message"}, id); err != nil {
			t.Fatal(err)
		}
	}
	d.mu.Lock()
	original := d.jobs["raced"]
	d.testFault = func(point, id string) error {
		switch {
		case point == "plane-read" && id == "release-dependencies/unread":
			return errors.New("injected dependency read failure")
		case point == "plane-read" && id == "release-dependencies/raced":
			// Another writer's checkpoint lands while the read runs unlocked.
			d.mu.Lock()
			changed := d.jobs["raced"]
			changed.Version++
			d.jobs["raced"] = changed
			d.mu.Unlock()
		}
		return nil
	}
	d.mu.Unlock()
	round := func(want map[string]string) {
		t.Helper()
		if err := d.dispatch(); err != nil {
			t.Fatal(err)
		}
		s := d.status.Load().(statusSnapshot)
		counts, _ := s.Scheduler["withheld"].(map[string]int)
		tasks, _ := s.Scheduler["withheld_tasks"].(map[string]string)
		for id, reason := range want {
			if tasks[id] != reason || counts[reason] == 0 {
				t.Fatalf("withheld %v (counts %v), want %s: %s", tasks, counts, id, reason)
			}
		}
		if len(tasks) != len(want) {
			t.Fatalf("withheld %v, want %v", tasks, want)
		}
	}
	round(map[string]string{"raced": withheldReleaseChanged, "unread": withheldEdgeUnread})
	d.mu.Lock()
	d.jobs["raced"] = original
	d.testFault = nil
	d.draining = true
	d.mu.Unlock()
	round(map[string]string{"raced": withheldDraining, "unread": withheldDraining})
	// A checkpoint whose row the plane cannot confirm holds the round.
	d.mu.Lock()
	d.draining = false
	ghost := original
	ghost.ID = "ghost"
	d.jobs["ghost"] = ghost
	d.pendingCheckpoint = &jobCheckpoint{Before: ghost, Proposed: ghost}
	d.mu.Unlock()
	round(map[string]string{"raced": withheldCheckpointUnsettled, "unread": withheldCheckpointUnsettled, "ghost": withheldCheckpointUnsettled})
	d.mu.Lock()
	delete(d.jobs, "ghost")
	d.pendingCheckpoint = nil
	d.testFault = func(point, id string) error {
		if point == "read" && id == "backlog-read" {
			return errors.New("injected backlog read failure")
		}
		return nil
	}
	d.mu.Unlock()
	round(map[string]string{"raced": withheldRoundRead, "unread": withheldRoundRead})
	d.mu.Lock()
	d.testFault = nil
	d.mu.Unlock()
	raw, err := os.ReadFile(d.layout.Log)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range []string{"raced:release_changed", "unread:edge_unread", "raced:draining", "ghost:checkpoint_unsettled", "unread:round_read"} {
		if !strings.Contains(string(raw), entry) {
			t.Fatalf("withheld trace lacks %s:\n%s", entry, raw)
		}
	}
}

// FAE-SCH-010 with FAE-VCS-008: a released task that changed while its
// refork ran without the writer lock is withheld for that round with a
// reason, not silently skipped.
func TestReleaseReforkChangeIsWithheld(t *testing.T) {
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
	if _, err = call(t, d, "Admit", AdmitParams{ID: "raced", AuthorizingMessage: "message"}, "raced"); err != nil {
		t.Fatal(err)
	}
	advanceIntegration(t, cfg, "MOVED.md")
	started, _ := slowRefork(t, 2*time.Second)
	d.mu.Lock()
	original := d.jobs["raced"]
	withheld := d.withholding(nil)
	d.mu.Unlock()
	go func() {
		for !exists(started) {
			time.Sleep(5 * time.Millisecond)
		}
		d.mu.Lock()
		changed := d.jobs["raced"]
		changed.Version++
		d.jobs["raced"] = changed
		d.mu.Unlock()
	}()
	full, err := d.dispatchTask("raced", engine.Options{}, nil, withheld, []string{"raced"})
	d.mu.Lock()
	reason := withheld.tasks["raced"]
	d.jobs["raced"] = original
	d.mu.Unlock()
	if err != nil || full || reason != withheldReleaseChanged {
		t.Fatalf("a task changed during its refork was withheld as %q (full %v): %v", reason, full, err)
	}
	if e := liveEnvelope(t, d, "raced"); e.Job.Base != base {
		t.Fatal("fork checkpointed over a changed task", e.Job.Base)
	}
}
