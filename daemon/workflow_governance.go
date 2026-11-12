package daemon

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

// WorkflowParams selects stored revisions for the governance reads and retirement.
type WorkflowParams struct {
	ID                 string          `json:"id,omitempty"`
	Revision           int64           `json:"revision,omitempty"`
	From               int64           `json:"from,omitempty"`
	To                 int64           `json:"to,omitempty"`
	Reason             string          `json:"reason,omitempty"`
	Document           json.RawMessage `json:"document,omitempty"`
	Scenario           json.RawMessage `json:"scenario,omitempty"`
	Task               string          `json:"task,omitempty"`
	AuthorizingMessage string          `json:"authorizing_message,omitempty"`
}
type WorkflowRead struct {
	Footprint      *workflow.Footprint              `json:"footprint,omitempty"`
	FootprintError string                           `json:"footprint_error,omitempty"`
	Revision       controlplane.Revision            `json:"revision"`
	Document       json.RawMessage                  `json:"document"`
	Retirement     *controlplane.WorkflowRetirement `json:"retirement"`
}
type WorkflowValidation struct {
	Warnings       []workflow.Issue    `json:"warnings,omitempty"`
	FootprintError string              `json:"footprint_error,omitempty"`
	Footprint      *workflow.Footprint `json:"footprint,omitempty"`
	Valid          bool                `json:"valid"`
	Workflow       string              `json:"workflow,omitempty"`
	ContentHash    string              `json:"content_hash,omitempty"`
	Issues         []workflow.Issue    `json:"issues"`
}
type WorkflowDiffResult struct {
	WorkflowID   string `json:"workflow_id"`
	FromRevision int64  `json:"from_revision"`
	ToRevision   int64  `json:"to_revision"`
	workflow.DiffReport
}

var workflowReads = map[string]bool{"Workflows": true, "WorkflowList": true, "WorkflowDiff": true, "WorkflowValidate": true, "WorkflowSimulate": true}

// decodeWorkflow is strict like workflow.Parse but leaves semantic validation to
// the caller, so stored revisions remain readable and diffable after a validator change.
func decodeWorkflow(raw []byte) (workflow.Document, error) {
	var d workflow.Document
	b, err := domain.CanonicalJSON(raw)
	if err != nil {
		return d, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	if err = dec.Decode(&d); err != nil {
		return d, err
	}
	return d, nil
}

// ValidateWorkflowContent is the authoring validator shared by the offline
// CLI and daemon RPC; stored revision reads retain their compatibility rules.
func ValidateWorkflowContent(raw []byte) WorkflowValidation {
	return ValidateWorkflowContentWithCeiling(raw, 0)
}

func ValidateWorkflowContentWithCeiling(raw []byte, ceiling int) WorkflowValidation {
	d, err := workflow.ParseWriteWithRecursionCeiling(raw, ceiling)
	if err == nil {
		hash, e := workflow.Hash(d)
		if e != nil {
			return WorkflowValidation{Issues: []workflow.Issue{{Message: e.Error()}}}
		}
		result := WorkflowValidation{Valid: true, Workflow: d.ID, ContentHash: hash, Issues: []workflow.Issue{}, Warnings: workflow.ReadPolicyWarnings(d)}
		if f, err := workflow.DeriveFootprint(d, nil, 0); err == nil {
			result.Footprint = &f
		} else {
			result.FootprintError = err.Error()
		}
		return result
	}
	out := WorkflowValidation{Issues: []workflow.Issue{{Message: err.Error()}}}
	if invalid := (*workflow.ValidationError)(nil); errors.As(err, &invalid) {
		out.Workflow, out.Issues = d.ID, invalid.Issues
	}
	return out
}

// SimulateWorkflow replays a scenario offline; it never reads or writes the plane.
func SimulateWorkflow(d workflow.Document, task string, scenario []byte) (workflow.Simulation, error) {
	raw, err := domain.CanonicalJSON(scenario)
	if err != nil {
		return workflow.Simulation{}, err
	}
	var shape map[string]json.RawMessage
	if json.Unmarshal(raw, &shape) == nil {
		if _, rules := shape["rules"]; rules {
			var plan workflow.Scenario
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.DisallowUnknownFields()
			decoder.UseNumber()
			if err = decoder.Decode(&plan); err != nil {
				return workflow.Simulation{}, err
			}
			return engine.SimulateScenario(d, task, plan, workflow.SimulationOptions{})
		}
	}
	var script workflow.Script
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	if err = dec.Decode(&script); err != nil {
		// A scripted-adapter scenario (fae workflow scenario) is replayed
		// as the simulator's records (rehearsalSimulationScript).
		rehearsed, named, ok, e := rehearsalSimulationScript(d, task, raw)
		if !ok {
			return workflow.Simulation{}, err
		}
		if e != nil {
			return workflow.Simulation{}, e
		}
		return engine.Simulate(d, named, rehearsed, workflow.SimulationOptions{})
	}
	var trailing any
	if dec.Decode(&trailing) != io.EOF {
		return workflow.Simulation{}, errors.New("trailing scenario content")
	}
	if task == "" {
		// Infer the task from a scenario whose evidence names exactly one task.
		for _, steps := range script {
			for _, step := range steps {
				for _, e := range step.Evidence {
					if task != "" && e.TaskID != task {
						return workflow.Simulation{}, errors.New("scenario names several tasks; select one with --task")
					}
					task = e.TaskID
				}
			}
		}
		if task == "" {
			return workflow.Simulation{}, errors.New("simulation task identity required")
		}
	}
	return engine.Simulate(d, task, script, workflow.SimulationOptions{})
}

func (d *Daemon) storedWorkflow(ctx context.Context, id string, revision int64) (controlplane.Revision, []byte, error) {
	if id == "" || revision < 0 {
		return controlplane.Revision{}, nil, errors.New("workflow identity and nonnegative revision required")
	}
	r, raw, err := d.store.GetWorkflow(ctx, id, revision)
	if errors.Is(err, sql.ErrNoRows) {
		if revision == 0 {
			return r, nil, fmt.Errorf("unknown workflow %q", id)
		}
		return r, nil, fmt.Errorf("unknown workflow %q revision %d", id, revision)
	}
	return r, raw, err
}

func (d *Daemon) workflowRead(ctx context.Context, r api.Request) (any, error) {
	var p WorkflowParams
	if err := decode(r.Params, &p); err != nil {
		return nil, err
	}
	switch r.Method {
	case "WorkflowList":
		return d.store.ListWorkflows(ctx, p.ID)
	case "WorkflowValidate":
		if len(p.Document) == 0 {
			return nil, errors.New("workflow document required")
		}
		v := ValidateWorkflowContentWithCeiling(p.Document, d.recursionCeiling())
		if v.Valid {
			doc, _ := decodeWorkflow(p.Document)
			if f, e := d.resolveFootprint(ctx, doc, 0); e == nil {
				v.Footprint = &f
				v.FootprintError = ""
			} else {
				v.Valid = false
				v.Issues = append(v.Issues, workflow.Issue{Path: "footprint", Message: e.Error()})
				v.FootprintError = e.Error()
			}
		}
		if v.Valid && d.live != nil && d.live.guard.panes != nil {
			doc, _ := decodeWorkflow(p.Document)
			if err := validatePaneFloor(doc, d.live.guard.effectivePaneLimit()); err != nil {
				v.Valid, v.Issues = false, []workflow.Issue{{Path: "roles", Message: err.Error()}}
			}
		}
		return v, nil
	case "WorkflowDiff":
		if p.From < 1 || p.To < 1 {
			return nil, errors.New("diff requires positive from and to revisions")
		}
		docs := [2]workflow.Document{}
		for i, n := range []int64{p.From, p.To} {
			_, raw, err := d.storedWorkflow(ctx, p.ID, n)
			if err != nil {
				return nil, err
			}
			if docs[i], err = decodeWorkflow(raw); err != nil {
				return nil, fmt.Errorf("revision %d is not loadable: %w", n, err)
			}
		}
		report, err := workflow.Diff(docs[0], docs[1])
		return WorkflowDiffResult{p.ID, p.From, p.To, report}, err
	case "WorkflowSimulate":
		if (len(p.Document) == 0) == (p.ID == "") || len(p.Scenario) == 0 {
			return nil, errors.New("simulate requires a scenario and exactly one of a document or a stored workflow")
		}
		raw := []byte(p.Document)
		if p.ID != "" {
			var err error
			if _, raw, err = d.storedWorkflow(ctx, p.ID, p.Revision); err != nil {
				return nil, err
			}
		}
		doc, err := d.parseWorkflow(raw)
		if err != nil {
			return nil, err
		}
		return SimulateWorkflow(doc, p.Task, p.Scenario)
	}
	rev, raw, err := d.storedWorkflow(ctx, p.ID, p.Revision)
	if err != nil {
		return nil, err
	}
	retired, err := d.store.WorkflowRetirement(ctx, p.ID)
	result := WorkflowRead{Revision: rev, Document: raw, Retirement: retired}
	if f, e := d.storedFootprint(ctx, p.ID, rev.Number); e == nil {
		result.Footprint = f
	} else if !errors.Is(e, sql.ErrNoRows) {
		result.FootprintError = e.Error()
	} else if doc, e := decodeWorkflow(raw); e == nil {
		if f, e := d.resolveFootprint(ctx, doc, 0); e == nil {
			result.Footprint = &f
		} else {
			result.FootprintError = e.Error()
		}
	}
	return result, err
}

func (d *Daemon) workflowRetire(ctx context.Context, raw json.RawMessage, audit controlplane.Audit) (any, error) {
	var p WorkflowParams
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	if p.ID == "" || strings.TrimSpace(p.Reason) == "" || len(p.Reason) > 4096 {
		return nil, errors.New("workflow retirement requires id and a bounded reason")
	}
	if err := d.store.RetireWorkflow(ctx, audit, p.ID, p.Reason); errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("unknown workflow %q", p.ID)
	} else if err != nil {
		return nil, err
	}
	return d.store.WorkflowRetirement(ctx, p.ID)
}
