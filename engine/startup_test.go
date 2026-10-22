package engine_test

import (
	"encoding/json"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
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
		if j.Failures["startup-recovery:"+d.Stage+""] != n || j.InFlight != nil {
			t.Fatal("lost durable recovery counter")
		}
		if j.Attempts != 0 || j.Counts[d.Stage] != 0 {
			t.Fatal("startup consumed budget")
		}
		if n < 3 && j.State == domain.Escalated {
			t.Fatal("early escalation")
		}
	}
	if j.State != domain.Escalated || j.Reason != "StartupExhausted" {
		t.Fatalf("%s %s", j.State, j.Reason)
	}
	if j.Failures["dispatch-recovery"] != 0 {
		t.Fatal("mixed recovery classes")
	}
}
