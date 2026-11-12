package daemon

import (
	"encoding/json"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
	"testing"
)

func TestWorkflowFootprintNativeReportsAndPinsCount(t *testing.T) {
	cfg, ctx := fixture(t)
	cfg.Executor = engine.NewScripted(map[string][]engine.ScriptStep{"draft": {{Output: json.RawMessage(`{"text":"note"}`)}}})
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	doc := childWorkflow()
	doc.ID = "footprint-native"
	doc.Stages = map[string]workflow.Stage{"draft": doc.Stages["draft"]}
	doc.Edges = []workflow.Edge{{From: "draft", To: "Done"}}
	doc.SlotScope = "tree"
	doc.MaxSessions = 6
	doc.SessionCount = 8
	raw, _ := json.Marshal(doc)
	response, err := call(t, d, "WorkflowValidate", WorkflowParams{Document: raw}, "")
	if err != nil {
		t.Fatal(err)
	}
	var valid WorkflowValidation
	_ = json.Unmarshal(response.Result, &valid)
	if !valid.Valid || valid.Footprint == nil || valid.Footprint.DerivedSessions != 6 || valid.Footprint.EffectiveSessions != 8 {
		t.Fatal(string(response.Result))
	}
	if _, err = call(t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	pinned, err := d.storedFootprint(ctx, doc.ID, 1)
	if err != nil || pinned.DerivedSessions != 6 || pinned.EffectiveSessions != 8 {
		t.Fatal("revision footprint not atomically stored", pinned, err)
	}
	if _, err = call(t, d, "Admit", AdmitParams{ID: "footprint-task", WorkflowID: doc.ID, Tree: "tree", AuthorizingMessage: "message"}, "footprint-task"); err != nil {
		t.Fatal(err)
	}
	awaitState(t, d, "footprint-task", "Done")
	row, err := d.store.GetJob(ctx, "footprint-task")
	if err != nil {
		t.Fatal(err)
	}
	var e envelope
	if err = decodeEnvelope(row.Payload, &e); err != nil {
		t.Fatal(err)
	}
	if e.Job.MaxSessions != 6 || e.Job.Footprint == nil || e.Job.Footprint.DerivedSessions != 6 || e.Job.Footprint.EffectiveSessions != 8 {
		t.Fatal(e.Job.MaxSessions, e.Job.Footprint)
	}
	response, err = call(t, d, "WorkflowSimulate", WorkflowParams{Document: raw, Task: "sim", Scenario: json.RawMessage(`{"draft":[{"output":{"text":"note"}}]}`)}, "")
	if err != nil {
		t.Fatal(err)
	}
	var sim workflow.Simulation
	_ = json.Unmarshal(response.Result, &sim)
	if sim.Footprint == nil || sim.Footprint.EffectiveSessions != 8 {
		t.Fatal(string(response.Result))
	}
	doc.SessionCount = 2
	raw, _ = json.Marshal(doc)
	response, err = call(t, d, "WorkflowValidate", WorkflowParams{Document: raw}, "")
	if err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(response.Result, &valid)
	if valid.Valid {
		t.Fatal("understated footprint accepted", string(response.Result))
	}
}
