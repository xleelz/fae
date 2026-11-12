package engine

import (
	"errors"

	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/workflow"
)

// CarryForward seeds a newly admitted task with the accepted pre-code
// evidence of an earlier, inactive task of the same item on the same
// workflow revision (FAE-WF-086). The task stays Pending: like any other it
// is released by the drive, a live task re-forking at the integration tip
// (FAE-VCS-001/-008), and then enters the stage where the earlier task first
// left the pre-code stages, along the kernel's carry-entry edge. Each carried
// artifact keeps its provenance and names the task it came from.
//
// A live task's tree may differ from the source's. Its carried artifacts are
// then informational inputs (latestInputs), never current-tree evidence: no
// gate, review aggregate or protected floor reads them on the new base, and
// every late stage runs there. A branch reads only the task-level no-red
// declaration, which every live task keeps across tree changes. A scripted
// task has no fork and keeps the source's tree.
func CarryForward(j, old Job) (Job, error) {
	if j.State != domain.Pending || len(j.Evidence) > 0 {
		return j, errors.New("carry-forward applies only at admission")
	}
	if err := Validate(old); err != nil {
		return j, err
	}
	live := j.ExecutionKind == "live"
	if old.WorkflowHash != j.WorkflowHash || old.ExecutionKind != j.ExecutionKind || !live && old.Tree != j.Tree {
		return j, errors.New("carry-forward requires the same workflow revision, runtime and tree")
	}
	if old.State != domain.Escalated && old.State != domain.Retired && old.State != domain.Cancelled {
		return j, errors.New("carry-forward source must be escalated, retired or cancelled")
	}
	late := workflow.LateStages(j.Document)
	resume := ""
	for _, e := range old.History {
		if e.Kind == "transition" && !late[string(e.From)] && late[string(e.To)] {
			resume = string(e.To)
			break
		}
	}
	if s, ok := j.Document.Stages[resume]; !ok || s.Primitive == workflow.Escalate {
		return j, errors.New("earlier task never completed its pre-code stages")
	}
	out := clone(j)
	for _, a := range latestInputs(old) {
		if late[a.Stage] || !live && a.Tree != j.Tree {
			continue
		}
		for _, p := range old.Provenance {
			if p.ArtifactID == a.ID && p.Stage == a.Stage && p.Session == a.Session {
				p.TaskID, p.CarriedFrom = j.TaskID, old.TaskID
				a.TaskID = j.TaskID
				out.Evidence = append(out.Evidence, a)
				out.Provenance = append(out.Provenance, p)
				break
			}
		}
	}
	if len(out.Evidence) == 0 {
		return j, errors.New("no accepted pre-code evidence to carry forward")
	}
	out.CarryEntry = resume
	events := []Event{}
	event(&out, Event{Kind: "carry-forward", Stage: string(domain.Pending), Attempt: out.stagedAttempt(), Cause: "pre-code evidence carried from " + old.TaskID + "; enters " + resume + " at release"}, &events)
	return out, Validate(out)
}
