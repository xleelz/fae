package engine

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/workflow"
)

func recordJoinArtifact(j *Job, s workflow.Stage, f *FanState, events *[]Event) error {
	if s.Output == "" {
		return nil
	} // stored pre-extension revisions remain runnable
	items := []any{}
	firstFailed := ""
	firstError := ""
	for index, b := range f.Branches {
		item := map[string]any{"index": index, "branch": b.ID, "item": b.Item, "status": b.Status}
		if b.Status == BranchFailed && firstFailed == "" {
			firstFailed = b.ID
			firstError = b.Error
		}
		if b.FailureReason != "" {
			item["reason"] = b.FailureReason
		}
		if b.Error != "" {
			item["error"] = b.Error
		}
		if b.Status == BranchPassed {
			typ := b.Output
			if typ == "" {
				types := map[string]bool{}
				for stage, owner := range workflow.BranchOf(j.Document, f.Stage) {
					if owner == b.ID || f.Dynamic {
						if out := j.Document.Stages[stage].Output; out != "" {
							types[out] = true
						}
					}
				}
				if len(types) == 1 {
					for name := range types {
						typ = name
					}
				}
			}
			for i := len(j.Evidence) - 1; i >= 0; i-- {
				a := j.Evidence[i]
				if typ != "" && a.Type != typ {
					continue
				}
				owned := false
				path := b.ID
				if j.BranchContext != nil {
					path = branchPath(j.BranchContext.Path, b.ID)
				}
				for _, p := range j.Provenance {
					if p.ArtifactID == a.ID && p.Session == a.Session && p.BranchID == path {
						owned = true
						break
					}
				}
				if owned {
					item["output"] = a.Fields
					if len(a.Value) > 0 {
						var value any
						if err := json.Unmarshal(a.Value, &value); err != nil {
							return err
						}
						item["output"] = value
					}
					item["artifact_id"] = a.ID
					break
				}
			}
		}
		items = append(items, item)
	}
	fields := map[string]any{"items": items, "first_failed_branch": firstFailed, "error": firstError}
	typ, ok := j.Document.Artifacts[s.Output]
	if !ok {
		return fmt.Errorf("join output type %s is not declared", s.Output)
	}
	raw, _ := json.Marshal(fields)
	var schema struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(typ.Schema, &schema)
	var value json.RawMessage
	if schema.Type == "array" {
		raw, _ = json.Marshal(items)
		value = append(json.RawMessage(nil), raw...)
		fields = map[string]any{"items": items}
	}
	if err := workflow.ValidateArtifact(typ.Schema, raw); err != nil {
		return fmt.Errorf("join artifact: %w", err)
	}
	id, err := domain.ContentHash(raw)
	if err != nil {
		return err
	}
	dispatch := fmt.Sprintf("%s/%s/join/%d", j.TaskID, f.Stage, f.Visit)
	if j.BranchContext != nil {
		dispatch += "/" + j.BranchContext.Path
	}
	stage := f.Join
	j.Evidence = append(j.Evidence, workflow.Evidence{Value: value, ID: id, TaskID: j.TaskID, Type: s.Output, Version: typ.Version, Stage: stage, Tree: j.Tree, Role: "engine", Session: dispatch, Provenance: true, Fields: fields})
	branch := ""
	if j.BranchContext != nil {
		branch = j.BranchContext.Path
	}
	j.Provenance = append(j.Provenance, Provenance{BranchID: branch, ArtifactID: id, DispatchID: dispatch, TaskID: j.TaskID, Stage: stage, Primitive: workflow.FanIn, Role: "engine", Session: dispatch, Tree: j.Tree, ProducedAt: time.Now().UTC(), ExecutionKind: j.ExecutionKind, WorkflowHash: j.WorkflowHash, WorkflowRevision: j.Revision})
	event(j, Event{Kind: "join-artifact", Stage: stage, Attempt: j.stagedAttempt(), DispatchID: dispatch, Cause: fmt.Sprintf("%d ordered branch outputs; first failure %s: %s", len(items), firstFailed, firstError)}, events)
	return nil
}
