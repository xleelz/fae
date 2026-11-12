package engine_test

import (
	"encoding/json"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
	"strings"
	"testing"
	"time"
)

func TestStartupRecoveryDurableBoundAndFreshSession(t *testing.T) {
	j, _ := engine.NewLive("starts", 1, defaults.Workflow(), "tree")
	previous := ""
	for n := 1; n <= 3; n++ {
		var d *engine.Dispatch
		var e error
		for d == nil {
			j, d, _, e = engine.Prepare(j)
			if e != nil || j.Parked || j.State == domain.Escalated {
				t.Fatalf("prepare: %v %+v", e, j)
			}
		}
		if d.Sessions[0].ID == previous {
			t.Fatal("session reused")
		}
		previous = d.Sessions[0].ID
		r := engine.Result{DispatchID: d.ID, TaskID: d.TaskID, Tree: d.Tree, Session: d.Sessions[0].ID, ProducedAt: time.Now().UTC()}
		r.ExecutionKind = "live"
		r.Class = "startup-retry"
		j, _, e = engine.Complete(j, d.ID, r)
		if e != nil {
			t.Fatal(e)
		}
		b, _ := json.Marshal(j)
		var restored engine.Job
		if e = json.Unmarshal(b, &restored); e != nil {
			t.Fatal(e)
		}
		j = restored
		if (n < 3 && j.Failures[engine.AgentStartCountKey(d.Stage, d.Sessions[0].Route)] != n) || j.InFlight != nil {
			t.Fatal("lost durable recovery counter")
		}
		if j.Attempts != 0 || j.Counts[d.Stage] != 0 {
			t.Fatal("startup consumed budget")
		}
		if j.State == domain.Escalated || (n < 3 && j.Parked) {
			t.Fatal("startup exhaustion escalated or parked early")
		}
	}
	// FAE-RT-004: exhaustion parks ProviderUnavailable (the daemon records it
	// against the binding) with a fresh start bound for the next route.
	if !j.Parked || j.Reason != engine.ParkProviderUnavailable || agentStarts(j, "intake") != 0 || j.Attempts != 0 {
		t.Fatalf("%s %s %v", j.State, j.Reason, j.Failures)
	}
	last := j.History[len(j.History)-1]
	if last.Kind != "park" || last.Attempt < 1 || last.DispatchID == "" || !strings.Contains(last.Cause, "claude/claude-opus-5-5") {
		t.Fatal(last)
	}
	if j.Failures["dispatch-recovery"] != 0 {
		t.Fatal("mixed recovery classes")
	}
	if k, d, _, e := engine.Prepare(j); e != nil || d != nil || !k.Parked {
		t.Fatal("park released without a route selector")
	}
	next := func(routes []workflow.Binding) (workflow.Binding, bool) { return routes[1], true }
	k, d, _, e := engine.PrepareWithRoutes(j, next)
	if e != nil || d == nil || k.Parked || d.Sessions[0].Route != j.Document.Roles["spec"].Routes[1] || k.Attempts != 1 {
		t.Fatal("did not rotate to the next eligible binding", e, k.Reason)
	}
}
