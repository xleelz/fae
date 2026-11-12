package daemon

import (
	"context"
	"encoding/json"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
	"testing"
	"time"
)

func TestTurnStatsAndToolEventsNativePersistWithExactReadings(t *testing.T) {
	cfg, ctx := fixture(t)
	var d *Daemon
	script := engine.NewScripted(map[string][]engine.ScriptStep{"draft": {{Output: json.RawMessage(`{"text":"first"}`)}}, "next": {{Output: json.RawMessage(`{"text":"second"}`)}}})
	cfg.Executor = funcExecutor(func(ctx context.Context, dispatch engine.Dispatch) (engine.Result, error) {
		r, err := script.Execute(ctx, dispatch)
		if err != nil {
			return r, err
		}
		var readings *domain.ProviderSessionStats
		if dispatch.Stage == "draft" {
			turns := 7
			cost := 0.42
			readings = &domain.ProviderSessionStats{NumTurns: &turns, CostUSD: &cost}
			for _, path := range []string{"a.go", "b.go", "c.go"} {
				d.recordToolEvent(dispatch, domain.ToolEvent{Tool: "read", Paths: []string{path}, Allowed: true, At: time.Now().UTC()})
			}
		}
		stat := domain.NewTurnStat(readings, time.Now().Add(-time.Second), time.Now())
		stat.Task = dispatch.TaskID
		stat.Dispatch = dispatch.ID
		stat.Session = dispatch.Sessions[0].ID
		stat.Role = dispatch.Sessions[0].Role
		stat.Stage = dispatch.Stage
		stat.Visit = dispatch.Visit
		r.TurnStats = []domain.TurnStat{stat}
		return r, nil
	})
	var err error
	d, err = Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	doc := childWorkflow()
	doc.ID = "native-stats"
	doc.Stages = map[string]workflow.Stage{"draft": doc.Stages["draft"], "next": doc.Stages["draft"]}
	doc.Edges = []workflow.Edge{{From: "draft", To: "next"}, {From: "next", To: "Done"}}
	if _, err = call(t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = call(t, d, "Admit", AdmitParams{ID: "native-stats", WorkflowID: doc.ID, Tree: "tree", AuthorizingMessage: "message"}, "native-stats"); err != nil {
		t.Fatal(err)
	}
	awaitState(t, d, "native-stats", "Done")
	row, err := d.store.GetJob(ctx, "native-stats")
	if err != nil {
		t.Fatal(err)
	}
	var e envelope
	if err = decodeEnvelope(row.Payload, &e); err != nil {
		t.Fatal(err)
	}
	stats := e.Job.TurnStats
	if len(stats) != 2 || stats[0].Attempt != 1 || stats[1].Attempt != 1 || stats[1].Sequence != 2 || stats[0].AssistantTurns.Value == nil || *stats[0].AssistantTurns.Value != 7 || stats[0].ProviderCostUSD.Value == nil || *stats[0].ProviderCostUSD.Value != 0.42 || stats[0].ProviderCostUSD.Label != "exact" || stats[1].ProviderCostUSD.Label != "absent" {
		t.Fatal(stats)
	}
	events, err := d.store.JobEvents(ctx, "native-stats", 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	turns, tools := 0, 0
	for _, event := range events {
		if event.Kind == "turn-stats" {
			turns++
		}
		if event.Kind == "tool-call" {
			tools++
		}
	}
	if turns != 2 || tools != 3 {
		t.Fatal("event counts", turns, tools)
	}
}
