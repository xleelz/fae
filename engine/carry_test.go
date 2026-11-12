package engine_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

// FAE-WF-086 acceptance: a re-admission with carry-forward starts at
// test-first with the carried pre-code artifacts linked by provenance.
func TestCarryForwardPreCodeEvidence(t *testing.T) {
	script := defaults.HappyScript()
	failing := []engine.ScriptStep{{Class: "execution-error"}, {Class: "execution-error"}, {Class: "execution-error"}, {Class: "execution-error"}}
	script["first/test-first"] = failing
	first, err := engine.New("first", 1, defaults.Workflow(), "fixture-tree")
	if err != nil {
		t.Fatal(err)
	}
	first = drive(t, first, engine.NewScripted(script), false)
	if first.State != domain.Escalated || first.EscalationKind != domain.SameGate {
		t.Fatal("fixture did not escalate at test-first", first.State, first.Reason)
	}
	second, err := engine.New("second", 1, defaults.Workflow(), "fixture-tree")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = engine.CarryForward(second, roundTrip(t, newJob(t))); err == nil {
		t.Fatal("active source carried")
	}
	other, _ := engine.New("third", 1, defaults.Workflow(), "other-tree")
	if _, err = engine.CarryForward(other, first); err == nil {
		t.Fatal("different tree carried")
	}
	carried, err := engine.CarryForward(second, first)
	if err != nil {
		t.Fatal(err)
	}
	// The re-admission waits for release like any other task, then enters
	// the carried entry along the kernel's carry-entry edge.
	if carried.State != domain.Pending || carried.CarryEntry != "red-loop" {
		t.Fatal("carried task not held for release at red-loop", carried.State, carried.CarryEntry)
	}
	if released, _, _, err := engine.Prepare(carried); err != nil || released.State != "red-loop" || released.CarryEntry != "" {
		t.Fatal("released at", released.State, released.CarryEntry, err)
	}
	// Migration maps a Pending task to the new revision's start stage; the
	// carry, pinned to its source's revision, no longer applies.
	migrated, _, err := engine.Migrate(carried, defaults.Workflow(), 2, "")
	if err != nil || migrated.CarryEntry != "" {
		t.Fatal("migrated carry entry", migrated.CarryEntry, err)
	}
	if released, _, _, err := engine.Prepare(migrated); err != nil || string(released.State) != released.Document.Start {
		t.Fatal("migrated task released at", released.State, err)
	}
	stages := map[string]bool{}
	for _, p := range carried.Provenance {
		if p.CarriedFrom != "first" || p.TaskID != "second" {
			t.Fatal("carried provenance", p)
		}
		stages[p.Stage] = true
	}
	if !stages["intake"] || !stages["spec"] || !stages["spec-review"] || !stages["plan"] || stages["test-first"] {
		t.Fatal("carried stages", stages)
	}
	done := drive(t, carried, engine.NewScripted(defaults.HappyScript()), true)
	if done.State != domain.Done || done.Counts["spec"] != 0 || done.Counts["plan"] != 0 || done.Counts["test-first"] != 1 {
		t.Fatal("carried task", done.State, done.Reason, done.Counts)
	}
}

// carryLive runs a live job on scripted outputs relabelled live; each change
// producer moves the tree, and a refused dispatch completes as a rejected
// change.
func carryLive(t *testing.T, j engine.Job, refuse func(engine.Dispatch) bool) engine.Job {
	t.Helper()
	return liveDrive(t, j, engine.NewScripted(defaults.HappyScript()), func(d *engine.Dispatch, r *engine.Result) {
		if d.Config.ChangeProducer {
			r.NewTree = d.Tree + "+" + d.Stage
		}
		if refuse(*d) {
			*r = engine.Result{DispatchID: d.ID, TaskID: d.TaskID, Tree: d.Tree, Session: d.Sessions[0].ID, ProducedAt: time.Now().UTC(), ExecutionKind: "live", Class: engine.ClassRejectedChange, Detail: "fixture refusal"}
		}
	})
}

// FAE-WF-086 with FAE-VCS-001: a live re-admission forks at the current
// integration tip, so its tree differs from the source's. The pre-code
// evidence still carries, the task re-forks at release with it, and every
// late stage runs on the new tree: the carried artifacts are informational
// inputs there, never current-tree gate or floor evidence.
func TestLiveCarryForwardOntoAMovedTip(t *testing.T) {
	first, err := engine.NewLive("first", 1, defaults.Workflow(), "source-tree")
	if err != nil {
		t.Fatal(err)
	}
	first = carryLive(t, first, func(d engine.Dispatch) bool { return d.Stage == "test-first" })
	if first.State != domain.Escalated || first.EscalationKind != domain.StaleBranchResidue {
		t.Fatal("fixture did not escalate at test-first", first.State, first.Reason)
	}
	second, err := engine.NewLive("second", 1, defaults.Workflow(), "tip-tree")
	if err != nil {
		t.Fatal(err)
	}
	second.Base = "tip"
	carried, err := engine.CarryForward(second, first)
	if err != nil {
		t.Fatal("carry onto a moved tip refused", err)
	}
	forked := engine.Fork(carried, "later-tip", "later-tree")
	if forked.Base != "later-tip" || forked.Tree != "later-tree" || forked.CarryEntry != "red-loop" || len(forked.Evidence) != len(carried.Evidence) {
		t.Fatal("carried task did not re-fork at release", forked.Base, forked.Tree, forked.CarryEntry)
	}
	for _, a := range forked.Evidence {
		if a.Tree != "source-tree" {
			t.Fatal("carried artifact restamped to the new tree", a.Stage, a.Tree)
		}
	}
	done := carryLive(t, forked, func(engine.Dispatch) bool { return false })
	if done.State != domain.Done || done.Counts["spec"] != 0 || done.Counts["plan"] != 0 || done.Counts["test-first"] != 1 || done.Counts["reviews"] < 1 || done.Counts["qa"] < 1 {
		t.Fatal("carried task", done.State, done.Reason, done.Counts)
	}
	released := false
	for _, e := range done.History {
		released = released || e.Kind == "transition" && e.From == domain.Pending && e.To == "red-loop"
	}
	if !released {
		t.Fatal("carried task did not enter red-loop at release")
	}
	carriedIDs := map[string]bool{}
	for _, p := range done.Provenance {
		if p.CarriedFrom == "first" {
			carriedIDs[p.ArtifactID] = true
		}
	}
	// Only a branch reads a carried artifact: the task-level no-red
	// declaration, which survives every tree change of any live task.
	for _, r := range done.GateRecords {
		for _, ref := range r.Evidence {
			if carriedIDs[ref.ID] && (r.Kind != workflow.RecordBranchArm || !slices.Contains(done.Document.NoRedStages, ref.Stage)) {
				t.Fatal("a gate on the new tree read carried evidence", r.Kind, r.Stage, ref.Stage)
			}
		}
	}
	if len(carriedIDs) < 4 || len(done.GateRecords) == 0 {
		t.Fatal("carry or gates missing", len(carriedIDs), len(done.GateRecords))
	}
}

// carriedPending is a carry-forward re-admission still Pending with its carry
// entry, as the daemon holds it until release (FAE-WF-086).
func carriedPending(t *testing.T) engine.Job {
	t.Helper()
	script := defaults.HappyScript()
	script["first/test-first"] = []engine.ScriptStep{{Class: "execution-error"}, {Class: "execution-error"}, {Class: "execution-error"}, {Class: "execution-error"}}
	first, err := engine.New("first", 1, defaults.Workflow(), "fixture-tree")
	if err != nil {
		t.Fatal(err)
	}
	first = drive(t, first, engine.NewScripted(script), false)
	second, err := engine.New("second", 1, defaults.Workflow(), "fixture-tree")
	if err != nil {
		t.Fatal(err)
	}
	carried, err := engine.CarryForward(second, first)
	if err != nil || carried.State != domain.Pending || carried.CarryEntry != "red-loop" {
		t.Fatal("fixture", carried.State, carried.CarryEntry, err)
	}
	return roundTrip(t, carried)
}

// FAE-WF-021, FAE-OPS-010: a control that moves a carried Pending task out of
// Pending drops its carry entry, so the result validates and an escalated
// carried task keeps both Escalated out-edges (Retire, and restart at the
// workflow's first stage). Defer, Undefer and a Pending restart with
// information keep it Pending with its entry, and the release still enters it.
func TestCarriedPendingTaskControls(t *testing.T) {
	carried := carriedPending(t)
	// apply validates every result.
	escalated := apply(t, carried, engine.Control{Method: "Escalate", Reason: "operator review"})
	if escalated.State != domain.Escalated || escalated.CarryEntry != "" {
		t.Fatal("escalated carried task", escalated.State, escalated.CarryEntry)
	}
	if retired := apply(t, escalated, engine.Control{Method: "Retire", Reason: "obsolete ruling/carried-task"}); retired.State != domain.Retired {
		t.Fatal("escalated carried task not retired", retired.State)
	}
	restarted := apply(t, escalated, engine.Control{Method: "RestartWithInformation", Information: "acceptance clarified"})
	if string(restarted.State) != restarted.Document.Start || restarted.CarryEntry != "" {
		t.Fatal("escalated carried task not restarted at the first stage", restarted.State, restarted.CarryEntry)
	}
	if done := drive(t, restarted, engine.NewScripted(defaults.HappyScript()), true); done.State != domain.Done || done.Counts["intake"] != 1 || done.Counts["spec"] != 1 {
		t.Fatal("restarted carried task", done.State, done.Reason, done.Counts)
	}
	if cancelled := apply(t, carried, engine.Control{Method: "Cancel", Reason: "not needed"}); cancelled.State != domain.Cancelled || cancelled.CarryEntry != "" {
		t.Fatal("cancelled carried task", cancelled.State, cancelled.CarryEntry)
	}
	faulted, _, err := engine.Fault(carried, errors.New("fixture fault"))
	if err != nil || faulted.State != domain.Escalated || faulted.CarryEntry != "" {
		t.Fatal("faulted carried task", faulted.State, faulted.CarryEntry, err)
	}
	deferred := apply(t, carried, engine.Control{Method: "Defer", Until: time.Now().Add(time.Hour)})
	informed := apply(t, deferred, engine.Control{Method: "RestartWithInformation", Information: "more context"})
	undeferred := apply(t, informed, engine.Control{Method: "Undefer"})
	for _, j := range []engine.Job{deferred, informed, undeferred} {
		if j.State != domain.Pending || j.CarryEntry != "red-loop" {
			t.Fatal("Pending control dropped the carry entry", j.State, j.CarryEntry)
		}
	}
	if released, _, _, err := engine.Prepare(undeferred); err != nil || released.State != "red-loop" || released.CarryEntry != "" {
		t.Fatal("released at", released.State, released.CarryEntry, err)
	}
}
