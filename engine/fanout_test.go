package engine_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

func approvedOnce(stage string) *workflow.Expr {
	x := bin("eq", workflow.Expr{Op: "count", Stage: stage, Args: []workflow.Expr{bin("eq", fieldOf(stage, "approved"), lit(true))}}, lit(1))
	return &x
}

// fanDoc runs a correctness review and a security review as parallel
// branches between the test gate and the commit.
func fanDoc(rule string, quorum int) workflow.Document {
	d := floorDoc()
	d.Roles["security"] = d.Roles["reviewer"]
	d.Stages["fan"] = workflow.Stage{Primitive: workflow.FanOut, Join: "join", Branches: []workflow.FanBranch{{ID: "correctness", Start: "review", Pass: approvedOnce("review")}, {ID: "security", Start: "secure", Pass: approvedOnce("secure")}}}
	d.Stages["secure"] = workflow.Stage{Primitive: workflow.Review, Role: "security", Output: "report", Inputs: []workflow.Input{{Name: "reports", Type: "report", Version: 1, Optional: true}}}
	d.Stages["join"] = workflow.Stage{Primitive: workflow.FanIn, Rule: rule, Quorum: quorum, Failure: "stop"}
	replaceEdge(&d, workflow.Edge{From: "gate", To: "review"}, workflow.Edge{From: "gate", To: "fan"}, workflow.Edge{From: "fan", To: "review"}, workflow.Edge{From: "fan", To: "secure"})
	replaceEdge(&d, workflow.Edge{From: "review", To: "commit"}, workflow.Edge{From: "review", To: "join"}, workflow.Edge{From: "secure", To: "join"}, workflow.Edge{From: "join", To: "commit"}, workflow.Edge{From: "join", To: "stop"})
	return d
}
func fanScript(correctness, security string) map[string][]engine.ScriptStep {
	script := floorScript(2)
	script["review"] = []engine.ScriptStep{reviewStep(correctness), reviewStep(correctness)}
	script["secure"] = []engine.ScriptStep{reviewStep(security), reviewStep(security)}
	return script
}

// recording keeps every dispatch it executes.
type recording struct {
	inner      engine.Executor
	dispatches []engine.Dispatch
}

func (r *recording) Execute(ctx context.Context, d engine.Dispatch) (engine.Result, error) {
	r.dispatches = append(r.dispatches, d)
	return r.inner.Execute(ctx, d)
}
func kinds(j engine.Job) string {
	out := []string{}
	for _, e := range j.History {
		switch e.Kind {
		case "fan-out", "branch-joined", "branch-verdict", "branch-skipped", "join", "fan-abandoned":
			out = append(out, e.Kind+":"+e.Cause)
		}
	}
	return strings.Join(out, " | ")
}

// FAE-WF-030 FanOut/FanIn conformance on the scripted adapter: durable branch
// state across a restart at every boundary, join rules, early settlement
// skipping unrun branches, recorded branch verdicts and sibling isolation.
func TestFanOutJoinRules(t *testing.T) {
	approved, rejected := `{"approved":true}`, `{"approved":false}`
	for _, c := range []struct {
		rule           string
		quorum         int
		correct, secur string
		done, skipped  bool
	}{
		{workflow.JoinAll, 0, approved, approved, true, false},
		{workflow.JoinAll, 0, approved, rejected, false, false},
		{workflow.JoinAll, 0, rejected, approved, false, true},
		{workflow.JoinAny, 0, approved, rejected, true, true},
		{workflow.JoinAny, 0, rejected, approved, true, false},
		{workflow.JoinAny, 0, rejected, rejected, false, false},
		{workflow.KOfN, 2, approved, rejected, false, false},
		{workflow.KOfN, 1, approved, rejected, true, true},
	} {
		x := &recording{inner: engine.NewScripted(fanScript(c.correct, c.secur))}
		j := drive(t, newPrimitiveJob(t, fanDoc(c.rule, c.quorum)), x, true)
		if (j.State == domain.Done) != c.done || !c.done && (j.State != domain.Escalated || j.EscalationDetail != "workflow stage stop") || j.Fan != nil {
			t.Fatalf("%s/%d %s %s: %s %s", c.rule, c.quorum, c.correct, c.secur, j.State, j.Reason)
		}
		ran := false
		for _, d := range x.dispatches {
			if d.Stage != "secure" {
				continue
			}
			ran = true
			for _, in := range d.Inputs {
				if in.Stage == "review" {
					t.Fatal("security branch saw the correctness review before the join")
				}
			}
		}
		if ran == c.skipped || strings.Contains(kinds(j), "branch-skipped:security") != c.skipped {
			t.Fatalf("%s %s %s: skip %v: %s", c.rule, c.correct, c.secur, c.skipped, kinds(j))
		}
		var join *engine.Event
		for i := range j.History {
			if j.History[i].Kind == "join" {
				join = &j.History[i]
			}
		}
		if join == nil || join.Gate == nil || join.Gate.Passed != c.done || len(join.Gate.Leaves) != 2 {
			t.Fatal("join verdict", join)
		}
		if err := engine.ReplayGates(roundTrip(t, j)); err != nil {
			t.Fatal(err)
		}
	}
}

// Kernel floors across branches: a passing join never substitutes for the
// protected-commit evidence the runtime checks before committing.
func TestFanJoinCannotLaunderCommitEvidence(t *testing.T) {
	d := fanDoc(workflow.JoinAny, 0)
	f := d.Stages["fan"]
	present := bin("ge", workflow.Expr{Op: "count", Stage: "review"}, lit(1))
	f.Branches[0].Pass = &present
	d.Stages["fan"] = f
	j := drive(t, newPrimitiveJob(t, d), engine.NewScripted(fanScript(`{"approved":false}`, `{"approved":false}`)), true)
	if j.State != domain.Escalated || j.EscalationKind != domain.ResumeEvidence || !strings.Contains(j.EscalationDetail, "no independent approved review") {
		t.Fatal("unapproved reviews committed through a passing join", j.State, j.EscalationDetail)
	}
}

// Cancellation and restart: an operator can block and cancel a task inside a
// branch; an escalation restart abandons the fan and a fresh visit starts.
func TestFanCancellationRestartAndMigration(t *testing.T) {
	x := engine.NewScripted(fanScript(`{"approved":true}`, `{"approved":true}`))
	j := newPrimitiveJob(t, fanDoc(workflow.JoinAll, 0))
	for i := 0; i < 50 && !(j.Fan != nil && j.InFlight == nil && j.Fan.Branches[0].Status == engine.BranchPassed && j.Fan.Branches[1].Status == engine.BranchPending); i++ {
		var d *engine.Dispatch
		var e error
		if j, d, _, e = engine.Prepare(j); e != nil {
			t.Fatal(e)
		}
		if d != nil {
			r, e := x.Execute(context.Background(), *d)
			if e != nil {
				t.Fatal(e)
			}
			if j, _, e = engine.Complete(j, d.ID, r); e != nil {
				t.Fatal(e)
			}
		}
		j = roundTrip(t, j)
	}
	if j.State != "fan" || j.Fan == nil || j.Fan.Branches[0].Status != engine.BranchPassed || j.Fan.Branches[1].Status != engine.BranchPending {
		t.Fatal("durable branch state", j.State, j.Fan)
	}
	if _, _, e := engine.Migrate(j, fanDoc(workflow.JoinAny, 0), 2, ""); e == nil || !strings.Contains(e.Error(), "fan-out") {
		t.Fatal("migration inside a fan-out", e)
	}
	now := time.Now().UTC()
	blocked, _, e := engine.ApplyControl(j, engine.Control{Method: "Block", Reason: "hold"}, now)
	if e != nil {
		t.Fatal(e)
	}
	cancelled, _, e := engine.ApplyControl(blocked, engine.Control{Method: "Cancel", Reason: "not needed"}, now)
	if e != nil || cancelled.State != domain.Cancelled || engine.Validate(roundTrip(t, cancelled)) != nil {
		t.Fatal("cancel inside a branch", e)
	}
	escalated, _, e := engine.ApplyControl(j, engine.Control{Method: "Escalate", Reason: "rethink"}, now)
	if e != nil {
		t.Fatal(e)
	}
	restarted, _, e := engine.ApplyControl(escalated, engine.Control{Method: "RestartWithInformation", Information: "again"}, now)
	if e != nil {
		t.Fatal(e)
	}
	done := drive(t, restarted, x, true)
	if done.State != domain.Done || done.Counts["fan"] != 2 || !strings.Contains(kinds(done), "fan-abandoned:visit 1 left at change") {
		t.Fatal("restart must abandon the fan and start a fresh visit", done.State, kinds(done))
	}
	tampered := roundTrip(t, j)
	tampered.Fan.Branches[0].Start = "commit"
	if engine.Validate(tampered) == nil {
		t.Fatal("tampered fan state validated")
	}
	tampered = roundTrip(t, j)
	tampered.Fan.Branches[0].Status = engine.BranchRunning
	tampered.Fan.Branches[1].Status = engine.BranchRunning
	if engine.Validate(tampered) == nil {
		t.Fatal("two running branches validated")
	}
	raw, _ := json.Marshal(j.Fan)
	if !strings.Contains(string(raw), `"status":"passed"`) || !strings.Contains(string(raw), `"status":"pending"`) || !strings.Contains(string(raw), `"frame"`) {
		t.Fatal(string(raw))
	}
}
