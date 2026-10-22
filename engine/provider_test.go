package engine_test

import (
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/providerhealth"
	"github.com/xleelz/fae/workflow"
	"testing"
	"time"
)

func TestProviderRoutesParkHealAndPin(t *testing.T) {
	doc := defaults.Workflow()
	for key, role := range doc.Roles {
		role.Routes = []workflow.Binding{{Provider: "p", Model: "one"}, {Provider: "p", Model: "two"}}
		doc.Roles[key] = role
	}
	j, e := engine.NewLive("route-task", 1, doc, "tree")
	if e != nil {
		t.Fatal(e)
	}
	j, _, _, e = engine.Prepare(j)
	if e != nil {
		t.Fatal(e)
	}
	now := time.Now()
	health := map[string]providerhealth.Health{"one": providerhealth.Health{}.Observe(providerhealth.RateLimited, now), "two": providerhealth.Health{}.Observe(providerhealth.Unavailable, now)}
	selectRoute := func(routes []workflow.Binding) (workflow.Binding, bool) {
		for _, r := range routes {
			if health[r.Model].Eligible(now) {
				return r, true
			}
		}
		return workflow.Binding{}, false
	}
	before := j
	j, d, events, e := engine.PrepareWithRoutes(j, selectRoute)
	if e != nil || d != nil || !j.Parked || j.Reason != "all-routes-unavailable" || j.Attempts != before.Attempts || j.Sequence != before.Sequence || len(events) != 1 {
		t.Fatal(j, d, events, e)
	}
	j = roundTrip(t, j)
	j, d, events, e = engine.PrepareWithRoutes(j, selectRoute)
	if e != nil || d != nil || len(events) != 0 {
		t.Fatal("duplicate park", events, e)
	}
	health["two"] = providerhealth.Health{}
	j, d, _, e = engine.PrepareWithRoutes(j, selectRoute)
	if e != nil || d == nil || j.Parked || d.Sessions[0].Route.Model != "two" {
		t.Fatal(j, d, e)
	}
	j = roundTrip(t, j)
	health["two"] = providerhealth.Health{}.Observe(providerhealth.Unavailable, now)
	unchanged, other, _, e := engine.PrepareWithRoutes(j, selectRoute)
	if e != nil || other != nil || unchanged.InFlight.Sessions[0].Route.Model != "two" {
		t.Fatal("changed durable binding")
	}
	j.InFlight.Sessions[0].Route.Model = "undeclared"
	if engine.Validate(j) == nil {
		t.Fatal("accepted forged binding")
	}
	// Time healing alone restores first ordered route; no attempt was charged
	// while all routes were unavailable.
	j = before
	now = now.Add(2 * time.Hour)
	j, d, _, e = engine.PrepareWithRoutes(j, selectRoute)
	if e != nil || d == nil || d.Sessions[0].Route.Model != "one" {
		t.Fatal("did not heal", j, d, e)
	}
}
func TestInfrastructureDeferralPreservesBudgetAndSequence(t *testing.T) {
	j, _ := engine.NewLive("deferral", 1, defaults.Workflow(), "tree")
	j, _, _, _ = engine.Prepare(j)
	j, d, _, e := engine.Prepare(j)
	if e != nil || d == nil {
		t.Fatal(e)
	}
	result := engine.Result{ExecutionKind: "live", DispatchID: d.ID, TaskID: d.TaskID, Tree: d.Tree, Session: d.Sessions[0].ID, Class: "infrastructure-deferred", ProducedAt: time.Now()}
	j, _, e = engine.Complete(j, d.ID, result)
	if e != nil || j.Attempts != 0 || j.Counts[d.Stage] != 0 || j.Sequence != 1 || j.InFlight != nil {
		t.Fatal(j, e)
	}
	j.AdmissionRetryAfter = time.Now().Add(-time.Second)
	j, next, _, e := engine.PrepareWithRoutes(j, func(routes []workflow.Binding) (workflow.Binding, bool) { return routes[0], true })
	if e != nil || next == nil || next.ID == d.ID || j.Attempts != 1 {
		t.Fatal(j, next, e)
	}
}
