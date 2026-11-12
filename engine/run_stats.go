package engine

import (
	"encoding/json"
	"fmt"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/workflow"
	"time"
)

func runStatsEvidence(j Job, scope string) (workflow.Evidence, error) {
	if j.RunStats == nil {
		return workflow.Evidence{}, fmt.Errorf("engine run_stats snapshot required before dispatch")
	}
	fields := j.RunStats
	if scoped, ok := j.RunStats[scope].(map[string]any); ok {
		fields = scoped
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		return workflow.Evidence{}, err
	}
	id, err := domain.ContentHash(raw)
	if err != nil {
		return workflow.Evidence{}, err
	}
	return workflow.Evidence{ID: id, TaskID: j.TaskID, Type: "engine:run_stats", Version: 1, Stage: "engine:run_stats", Tree: j.Tree, Role: "engine", Session: "engine:run_stats", Provenance: true, Fields: fields}, nil
}
func recordRunStats(j *Job, s workflow.Stage) error {
	if s.RunStats == "" {
		return nil
	}
	a, err := runStatsEvidence(*j, s.RunStats)
	if err != nil {
		return err
	}
	for _, old := range j.Evidence {
		if old.ID == a.ID && old.Stage == a.Stage {
			return nil
		}
	}
	j.Evidence = append(j.Evidence, a)
	j.Provenance = append(j.Provenance, Provenance{ArtifactID: a.ID, DispatchID: fmt.Sprintf("engine:run_stats:%s:%d", j.State, j.Sequence), TaskID: j.TaskID, Stage: a.Stage, Primitive: workflow.AgentTurn, Role: "engine", Session: a.Session, Tree: j.Tree, ExecutionKind: j.ExecutionKind, ProducedAt: time.Now().UTC(), WorkflowHash: j.WorkflowHash, WorkflowRevision: j.Revision})
	return nil
}
func withRunStats(j Job, evidence []workflow.Evidence) []workflow.Evidence {
	out := make([]workflow.Evidence, 0, len(evidence)+1)
	for _, a := range evidence {
		if a.Stage != "engine:run_stats" {
			out = append(out, a)
		}
	}
	if s := j.Document.Stages[string(j.State)]; s.RunStats != "" {
		if a, err := runStatsEvidence(j, s.RunStats); err == nil {
			out = append(out, a)
		}
	}
	return out
}
func bindRunStats(j *Job, d *Dispatch) error {
	if err := recordRunStats(j, d.Config); err != nil {
		return err
	}
	if d.Config.RunStats == "" {
		return nil
	}
	for _, a := range d.Inputs {
		if a.Stage == "engine:run_stats" {
			return nil
		}
	}
	a, err := runStatsEvidence(*j, d.Config.RunStats)
	if err != nil {
		return err
	}
	d.Inputs = append(d.Inputs, a)
	return nil
}
func validateRunStatsEvidence(j Job, a workflow.Evidence) (bool, error) {
	if a.Stage != "engine:run_stats" {
		return false, nil
	}
	raw, _ := json.Marshal(a.Fields)
	id, _ := domain.ContentHash(raw)
	if a.Type != "engine:run_stats" || a.TaskID != j.TaskID || a.Role != "engine" || a.Session != "engine:run_stats" || !a.Provenance || a.ID != id {
		return true, fmt.Errorf("invalid engine run_stats artifact")
	}
	for _, p := range j.Provenance {
		if p.ArtifactID == a.ID && p.Stage == a.Stage && p.TaskID == a.TaskID && p.Role == "engine" && !p.ProducedAt.IsZero() {
			return true, nil
		}
	}
	return true, fmt.Errorf("run_stats artifact has no engine provenance")
}
