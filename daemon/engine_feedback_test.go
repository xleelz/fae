package daemon

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

// Engine feedback: input runs that passed produce no entry (so no block) and
// never reach the store; the run-time and brief-time failure test agree.
func TestEngineFeedbackSkipsPassingRuns(t *testing.T) {
	doc := defaults.Workflow()
	passed := workflow.Evidence{ID: "green", Stage: "run-tests", Session: "t/h/4/session/0", Fields: map[string]any{"exit_code": json.Number("0"), "executed_count": json.Number("1"), "matched_count": json.Number("1")}}
	clean := workflow.Evidence{ID: "fmt", Stage: "format", Session: "t/h/5/session/0", Fields: map[string]any{"new_debt": float64(0)}}
	x := &liveExecutor{} // no store: passing inputs must not read it
	runs, err := x.engineFeedback(context.Background(), engine.Dispatch{Inputs: []workflow.Evidence{passed, clean}}, doc)
	if err != nil || len(runs) != 0 {
		t.Fatal("passing runs produced feedback", runs, err)
	}
	for _, c := range []struct {
		stage  workflow.Stage
		fields map[string]any
		failed bool
	}{
		{doc.Stages["run-tests"], map[string]any{"exit_code": 1, "executed_count": 1, "matched_count": 1}, true},
		{doc.Stages["run-tests"], map[string]any{"exit_code": 0, "executed_count": 1, "matched_count": 0}, true},
		{doc.Stages["qa"], map[string]any{"exit_code": 0, "executed_count": 0, "matched_count": 0}, true},
		{doc.Stages["qa"], map[string]any{"exit_code": 0, "executed_count": 9, "matched_count": 0}, false},
		{doc.Stages["format"], map[string]any{"new_debt": json.Number("2")}, true},
	} {
		if hostRunFailed(c.stage, c.fields) != c.failed {
			t.Errorf("hostRunFailed(%+v) != %v", c.fields, c.failed)
		}
	}
}

// Engine feedback against a real store: a missing record yields an empty
// excerpt, a stale record of another artifact is ignored, a current one shown.
func TestEngineFeedbackMissingAndStaleRecords(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	doc := defaults.Workflow()
	failed := workflow.Evidence{ID: "red-artifact", Stage: "run-tests", Session: "t/h/4/session/0", Fields: map[string]any{"exit_code": json.Number("1"), "executed_count": json.Number("1"), "matched_count": json.Number("1")}}
	x := &liveExecutor{daemon: d}
	dispatch := engine.Dispatch{Inputs: []workflow.Evidence{failed}}
	runs, err := x.engineFeedback(ctx, dispatch, doc)
	if err != nil || len(runs) != 1 || runs[0].Excerpt != "" || runs[0].DispatchID != "t/h/4" || runs[0].ExitCode != 1 || runs[0].Command[0] != "go" {
		t.Fatal("legacy failed run", runs, err)
	}
	ref, err := d.store.PutBlob(ctx, controlplane.Audit{Actor: "test", Action: "seed", Outcome: "ok"}, []byte("stale excerpt\n"))
	if err != nil {
		t.Fatal(err)
	}
	stale, _ := json.Marshal(engineFeedbackRecord{Artifact: "other-artifact", DispatchID: "t/h/4", Command: []string{"stale"}, Excerpt: ref})
	if err = d.store.PutMarker(ctx, controlplane.Audit{Actor: "test", Action: "seed", Outcome: "ok"}, engineFeedbackKey(failed.Session), stale); err != nil {
		t.Fatal(err)
	}
	if runs, err = x.engineFeedback(ctx, dispatch, doc); err != nil || len(runs) != 1 || runs[0].Excerpt != "" || runs[0].Command[0] != "go" {
		t.Fatal("stale record shown", runs, err)
	}
	current, _ := json.Marshal(engineFeedbackRecord{Artifact: failed.ID, DispatchID: "t/h/4", Command: []string{"go", "test"}, Excerpt: ref, Truncated: true})
	if err = d.store.PutMarker(ctx, controlplane.Audit{Actor: "test", Action: "seed", Outcome: "ok"}, engineFeedbackKey(failed.Session), current); err != nil {
		t.Fatal(err)
	}
	if runs, err = x.engineFeedback(ctx, dispatch, doc); err != nil || len(runs) != 1 || runs[0].Excerpt != "stale excerpt\n" || !runs[0].ExcerptTruncated {
		t.Fatal("current record", runs, err)
	}
}
