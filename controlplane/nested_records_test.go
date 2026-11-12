package controlplane

import (
	"encoding/json"
	"testing"

	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

func TestRecordsAcceptSuccessiveFanCursorJoinsInOneCheckpoint(t *testing.T) {
	job := engine.Job{TaskID: "task", State: domain.Done, Document: workflow.Document{Stages: map[string]workflow.Stage{"one": {Primitive: workflow.FanOut, Join: "onejoin"}, "onejoin": {Primitive: workflow.FanIn}, "two": {Primitive: workflow.FanOut, Join: "twojoin"}, "twojoin": {Primitive: workflow.FanIn}}}, LastFan: &engine.FanState{Stage: "two", Join: "twojoin", Baseline: &engine.Job{}}, History: []engine.Event{
		{Kind: "fan-cursor", Stage: "one", From: "one", To: "onejoin"}, {Kind: "join", Stage: "onejoin"}, {Kind: "transition", Stage: "onejoin", From: "onejoin", To: "two", Cause: "stage-complete"},
		{Kind: "fan-cursor", Stage: "two", From: "two", To: "twojoin"}, {Kind: "join", Stage: "twojoin"}, {Kind: "transition", Stage: "twojoin", From: "twojoin", To: domain.Done, Cause: "stage-complete"},
	}}
	if _, err := planRecords(JobSnapshot{ID: "task", State: "Done"}, job, taskCursor{State: "one"}, "molecule", "engine"); err != nil {
		t.Fatal(err)
	}
	// A cursor projection without the paired actual join cannot authorize a skip.
	job.History = append(job.History[:1], job.History[2:]...)
	if _, err := planRecords(JobSnapshot{ID: "task", State: "Done"}, job, taskCursor{State: "one"}, "molecule", "engine"); err == nil {
		t.Fatal("unpaired cursor accepted")
	}
}

func TestPlanArtifactPersistsTypedListNotMetadataWrapper(t *testing.T) {
	raw := json.RawMessage(`[{"status":"passed","output":{"text":"kept"}}]`)
	id, canonical, err := ContentHash(raw)
	if err != nil {
		t.Fatal(err)
	}
	var itemsValue []any
	_ = json.Unmarshal(raw, &itemsValue)
	artifact := workflow.Evidence{ID: id, TaskID: "task", Stage: "join", Type: "Notes", Version: 1, Session: "join-session", Value: raw, Fields: map[string]any{"items": itemsValue}}
	provenance := engine.Provenance{ArtifactID: id, Stage: "join", Session: "join-session", TaskID: "task", Primitive: workflow.FanIn}
	planned, err := planArtifact(engine.Job{TaskID: "task", Evidence: []workflow.Evidence{artifact}}, provenance, "molecule")
	if err != nil {
		t.Fatal(err)
	}
	if string(planned.Content) != string(canonical) || planned.SchemaID != "Notes" || planned.Version != 1 {
		t.Fatal(string(planned.Content), planned.SchemaID, planned.Version)
	}
	var items []any
	if err = json.Unmarshal(planned.Content, &items); err != nil || len(items) != 1 {
		t.Fatal(err, string(planned.Content))
	}
}

func TestListArtifactRejectsUnhashedGateMetadata(t *testing.T) {
	raw := json.RawMessage(`[{"branch":"a","status":"failed","error":"grounded"}]`)
	id, _, err := ContentHash(raw)
	if err != nil {
		t.Fatal(err)
	}
	var items []any
	_ = json.Unmarshal(raw, &items)
	artifact := workflow.Evidence{ID: id, TaskID: "task", Stage: "join", Type: "Notes", Version: 1, Session: "join-session", Value: raw, Fields: map[string]any{"items": items, "error": "forged", "first_failed_branch": "other"}}
	provenance := engine.Provenance{ArtifactID: id, Stage: "join", Session: "join-session", TaskID: "task", Primitive: workflow.FanIn}
	if _, err = planArtifact(engine.Job{TaskID: "task", Evidence: []workflow.Evidence{artifact}}, provenance, "molecule"); err == nil {
		t.Fatal("unhashed gate-visible fields accepted")
	}
}
