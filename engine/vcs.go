package engine

import (
	"errors"

	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/workflow"
)

// Fork records a new fork point for a released live task that has produced
// nothing yet (FAE-VCS-001/-008): its worktree now starts at base with tree.
// The exact base is durable in the task and named in its history. Carried
// pre-code evidence stays informational on any tree (CarryForward).
func Fork(j Job, base, tree string) Job {
	if j.ExecutionKind != "live" || j.State != domain.Pending || j.InFlight != nil || j.Sequence > 0 || base == "" || tree == "" {
		return j
	}
	out := clone(j)
	out.Base, out.Tree, out.ExecutionHead = base, tree, ""
	events := []Event{}
	event(&out, Event{Kind: "worktree-forked", Stage: string(domain.Pending), Attempt: out.stagedAttempt(), Cause: "forked at integration tip " + base}, &events)
	return out
}

// ReviewCarry records a clean, patch-identical reconcile (FAE-VCS-005a): the
// task's own diff on To is exactly the reviewed diff on From, so approvals of
// From stay valid on To. Test and QA evidence never carries.
type ReviewCarry struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Dispatch string `json:"dispatch"`
}

// carryReviews records a carry for a Merge completion that changed the tree
// or base, when the trusted executor reported it clean and patch-identical.
func carryReviews(j *Job, d Dispatch, tree string, records []workflow.Evidence, events *[]Event) bool {
	clean := len(records) == 1 && records[0].Fields["result"] == "clean"
	if !CanCarryReview(j.ExecutionKind == "live", clean, clean && records[0].Fields["patch_identical"] == true, d.Tree != "" && tree != "") {
		return false
	}
	j.Carries = append(j.Carries, ReviewCarry{From: d.Tree, To: tree, Dispatch: d.ID})
	event(j, Event{Kind: "reviews-carried", Stage: d.Stage, DispatchID: d.ID, Attempt: d.Attempt, Cause: "patch-identical reconcile: approvals carry to the reconciled tree; QA re-runs on it"}, events)
	return true
}

// carriedReviews returns the latest approvals of trees that patch-identical
// reconciles carried to the current tree, restamped to it for the protected
// check. Any later change producer or reconcile that was not patch-identical
// invalidates them, exactly as for current-tree reviews.
func carriedReviews(j Job, position func(workflow.Evidence) int) []workflow.Evidence {
	if len(j.Carries) == 0 {
		return nil
	}
	trees, carried := map[string]bool{j.Tree: true}, map[string]bool{}
	for i := len(j.Carries) - 1; i >= 0; i-- {
		if c := j.Carries[i]; trees[c.To] {
			trees[c.From] = true
		}
		carried[j.Carries[i].Dispatch] = true
	}
	order := map[string]int{}
	for i, e := range j.History {
		if e.Kind == "dispatch" {
			order[e.DispatchID] = i + 1
		}
	}
	last := 0
	latest := latestInputs(j)
	for _, a := range latest {
		if workflow.ProducesChanges(j.Document,j.Document.Stages[a.Stage]) {
			last = max(last, position(a))
		}
	}
	for _, e := range j.History {
		if e.Kind == "tree-reconciled" && !carried[e.DispatchID] {
			last = max(last, order[e.DispatchID])
		}
	}
	var out []workflow.Evidence
	for _, a := range latest {
		// A base-only reconcile carries approvals of the unchanged tree too.
		if j.Document.Stages[a.Stage].Primitive != workflow.Review || !trees[a.Tree] || position(a) <= last {
			continue
		}
		a.Tree = j.Tree
		out = append(out, a)
	}
	return out
}

// ClassVcsRefused is a VcsCommit or Integrate outcome with no write left to
// prove and no route that can still land it (FAE-VCS-009): it escalates with
// a typed reason instead of resting in the committing stage. The VCS effect
// was attempted (a commit that failed after its retry, an integration refused
// after its retry), so the dispatch keeps its attempt.
const ClassVcsRefused = "vcs-refused"

// ClassCommitRefusedBeforeWrite is a VcsCommit refused before any commit
// object or ref was written: the reviewed snapshot could not be taken or
// differs from the task tree, or the commit could not be prepared. It
// escalates as ClassVcsRefused does (CommitRefused, for the operator), but
// the dispatch ran nothing, so its attempt and visit are refunded (FAE-RT-004,
// operator ruling 2026-10-29).
const ClassCommitRefusedBeforeWrite = "commit-refused-before-write"

func vcsRefused(input, j Job, d Dispatch, r Result, events []Event) (Job, []Event, bool, error) {
	if r.Class != ClassVcsRefused && r.Class != ClassCommitRefusedBeforeWrite {
		return j, events, false, nil
	}
	if j.ExecutionKind != "live" || r.ExecutionKind != "live" || (d.Config.Primitive != workflow.VcsCommit && d.Config.Primitive != workflow.Integrate) || len(r.Output) != 0 || len(r.Reviews) != 0 || r.NewTree != "" || r.NewBase != "" || r.Detail == "" || (r.Class == ClassCommitRefusedBeforeWrite && d.Config.Primitive != workflow.VcsCommit) {
		return input, nil, true, errors.New("invalid VCS refusal")
	}
	kind, detail := domain.IntegrationConflict, bounded(r.Detail)
	if d.Config.Primitive == workflow.VcsCommit {
		kind, detail = domain.HumanDecisionRequired, bounded("CommitRefused: "+r.Detail)
	}
	if r.Class == ClassCommitRefusedBeforeWrite {
		refund(&j, d)
	}
	stop(&j, kind, detail, &events)
	return j, events, true, nil
}
