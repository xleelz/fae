package controlplane

import (
	"encoding/json"
	"errors"
	"testing"
)

// A decision row commits with the job checkpoint that applies it, and a
// recorded decision id is never written twice (the whole checkpoint refuses).
func TestDecisionCommitsWithCheckpointOnce(t *testing.T) {
	ctx, s := doltStore(t)
	// Admission binds the job to its stored revision's molecule.
	if _, e := s.WriteWorkflow(ctx, audit(), "wf", "wf", json.RawMessage(`{"id":"wf"}`), func([]byte) error { return nil }); e != nil {
		t.Fatal(e)
	}
	j, e := s.AdmitJob(ctx, Audit{Actor: "operator", Action: "admit", Outcome: "ok"}, "", jobFixture())
	if e != nil {
		t.Fatal(e)
	}
	d := Decision{ID: "decision-1", Kind: "HumanDecision", Payload: json.RawMessage(`{"option":"ship","authorizing_message":"m"}`), Rationale: "ready", Role: "operator", SessionID: "decision-1"}
	next := j
	next.State, next.Payload, next.Decisions = "commit", taskPayload("job", "commit", map[string]any{"kind": "transition", "stage": "Pending", "from": "Pending", "to": "commit", "attempt": 1, "cause": "decided"}), []Decision{d}
	saved, e := s.SaveJob(ctx, Audit{Actor: "operator", Action: "Decide", Outcome: "ok"}, next, j.Version)
	if e != nil {
		t.Fatal(e)
	}
	if len(saved.Decisions) != 0 {
		t.Fatal("a saved snapshot still carries its committed decisions")
	}
	rows, e := s.TaskDecisionPayloads(ctx, "job", 10)
	if e != nil || len(rows) != 1 || rows[0].ID != "decision-1" || rows[0].TaskID != "job" || rows[0].Role != "operator" || rows[0].Session != "decision-1" || rows[0].Supersedes != "" || rows[0].Rationale != "ready" || string(rows[0].Payload) != `{"authorizing_message":"m","option":"ship"}` {
		t.Fatal(rows, e)
	}
	again := saved
	again.State, again.Payload, again.Decisions = "Done", taskPayload("job", "Done", map[string]any{"kind": "transition", "stage": "Pending", "from": "Pending", "to": "commit", "attempt": 1, "cause": "decided"}, map[string]any{"kind": "transition", "stage": "commit", "from": "commit", "to": "Done", "attempt": 1, "cause": "again"}), []Decision{d}
	if _, e = s.SaveJob(ctx, Audit{Actor: "operator", Action: "Decide", Outcome: "ok"}, again, saved.Version); !errors.Is(e, ErrDuplicateDecision) {
		t.Fatal("duplicate decision accepted", e)
	}
	if current, e := s.GetJob(ctx, "job"); e != nil || current.Version != saved.Version || current.State != "commit" {
		t.Fatal("refused decision changed the checkpoint", current.State, e)
	}
	if _, e = s.TaskDecisionPayloads(ctx, "", 10); e == nil {
		t.Fatal("unkeyed decision read accepted")
	}
}
