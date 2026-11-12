package daemon

import (
	"encoding/json"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
	"testing"
)

func TestTreeRoleCapSmallestLiveRevisionExcludesClosed(t *testing.T) {
	row := func(id string, state domain.State, cap int) controlplane.JobSnapshot {
		raw, _ := json.Marshal(envelope{Job: engine.Job{TaskID: id, TreeRoot: "root", State: state, Document: workflow.Document{Roles: map[string]workflow.Role{"coder": {CapScope: "tree", Cap: cap}}}}})
		return controlplane.JobSnapshot{ID: id, Payload: raw}
	}
	jobs := map[string]controlplane.JobSnapshot{"closed": row("closed", domain.Done, 1), "active": row("active", "code", 4)}
	a := &liveAdmission{}
	v, err := newScheduleView(jobs)
	if err != nil {
		t.Fatal(err)
	}
	a.view.Store(v)
	role := workflow.Role{CapScope: "tree", Cap: 4}
	_, cap, err := a.rolePool(workflow.Document{}, "active", "coder", role)
	if err != nil || cap != 4 {
		t.Fatal("closed cap constrained live task", cap, err)
	}
	jobs["other"] = row("other", "code", 2)
	v, err = newScheduleView(jobs)
	if err != nil {
		t.Fatal(err)
	}
	a.view.Store(v)
	_, cap, err = a.rolePool(workflow.Document{}, "active", "coder", role)
	if err != nil || cap != 2 {
		t.Fatal("smallest live cap not applied", cap, err)
	}
}
