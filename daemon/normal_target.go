package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xleelz/fae/external"
	"github.com/xleelz/fae/workflow"
)

// retiredTargetRecoveryKey is set only by startup for an engine-validated Retired job.
// Its intentionally archived workspace is no longer an execution prerequisite.
type retiredTargetRecoveryKey struct{}

// NormalTargetPolicy freezes installed adapter identity and argument policy.
// It never installs capabilities or modifies the daemon's shared live config.
type NormalTargetPolicy struct {
	Adapter                   *AdapterBinding          `json:"adapter,omitempty"`
	Destination               string                   `json:"destination"`
	PushNamespace             string                   `json:"push_namespace"`
	TicketRoom                string                   `json:"ticket_room,omitempty"`
	Tools                     map[string]external.Tool `json:"tools"`
	ReviewTask                string                   `json:"review_task,omitempty"`
	Review                    *external.ReviewPolicy   `json:"review,omitempty"`
	TicketWorkflow            *TicketWorkflowConfig    `json:"ticket_workflow,omitempty"`
	DisableReviewPublishMerge bool                     `json:"disable_review_publish_merge,omitempty"`
}

func targetEqual(a, b any) bool {
	x, e := json.Marshal(a)
	y, f := json.Marshal(b)
	return e == nil && f == nil && string(x) == string(y)
}
func validateNormalTargetShape(c TargetConfig) error {
	n := c.Normal
	if c.SchemaVersion == 1 && n != nil {
		return errors.New("target v1 does not accept Normal policy")
	}
	if (c.SchemaVersion == 2 || c.SchemaVersion == 3) && n == nil {
		return errors.New("target v2 requires Normal policy")
	}
	if n == nil {
		return nil
	}
	if (c.SchemaVersion == 3) != (n.Adapter != nil) {
		return errors.New("target v3 requires adapter binding; earlier schemas forbid it")
	}
	if n.Destination == "" || n.PushNamespace == "" || len(n.Tools) == 0 {
		return errors.New("Normal target requires destination, push namespace and exact tools")
	}
	if (n.Review == nil) != (n.ReviewTask == "") {
		return errors.New("Normal review policy requires exact review_task binding")
	}
	if n.Review != nil && (!n.Review.BaselineApproved || n.Review.Package == "" || n.Review.Workspace == "" || n.Review.Branch == "" || n.Review.IntegrationTip == "" || n.Review.BaselineTool == "" || n.Review.StatusTool == "") {
		return errors.New("Normal review target requires workspace, branch, package and approved baseline/read policy")
	}
	if n.DisableReviewPublishMerge && n.Review == nil {
		return errors.New("review disable policy requires review binding")
	}
	if n.TicketWorkflow != nil && n.TicketRoom == "" {
		return errors.New("Normal ticket target requires ticket room")
	}
	for alias, t := range n.Tools {
		if alias == "" {
			return errors.New("empty Normal tool alias")
		}
		if err := t.Validate(); err != nil {
			return fmt.Errorf("Normal tool %s: %w", alias, err)
		}
	}
	return nil
}
func (d *Daemon) validateNormalTarget(ctx context.Context, c TargetConfig, p AdmitParams, doc workflow.Document, health bool) error {
	n := c.Normal
	if p.Mode == "Normal" && n == nil {
		return errors.New("Normal disposition requires target schema v2 Normal policy")
	}
	if n == nil {
		return nil
	}
	if d.live == nil {
		return errors.New("Normal target requires installed live adapters")
	}
	selectedLive, err := d.liveForTarget(ctx, c)
	if err != nil {
		return err
	}
	installed := selectedLive.config
	// Clone the tool map because discovery caches schema on its map values.
	selected := installed
	selected.ExternalTools = map[string]external.Tool{}
	for alias, want := range n.Tools {
		got, ok := installed.ExternalTools[alias]
		if !NormalTargetAuthorized(ok, targetEqual(want, got)) {
			return fmt.Errorf("Normal target installed adapter drift: %s", alias)
		}
		selected.ExternalTools[alias] = got
	}
	if n.Review != nil && c.SchemaVersion == 3 {
		record, err := d.reviewWorkspaceRecord(ctx, n.ReviewTask)
		if err != nil {
			return fmt.Errorf("dynamic review workspace not prepared: %w", err)
		}
		if record.State != "ready" || !targetEqual(record.Adapter, *n.Adapter) || !targetEqual(record.Policy, *n.Review) || (p.ID != "" && p.Mode == "Normal" && (p.ID != record.Task || p.RunID != record.RunID)) {
			return errors.New("dynamic review target differs from ready workspace binding")
		}
		if retired, _ := ctx.Value(retiredTargetRecoveryKey{}).(bool); !retired {
			if err := d.validatePreparedWorkspace(ctx, record); err != nil {
				return err
			}
		}
	}
	if n.Review != nil {
		if !targetEqual(*n.Review, installed.ReviewPolicies[n.ReviewTask]) || n.DisableReviewPublishMerge != installed.DisableReviewPublishMerge {
			return errors.New("Normal target installed review policy drift")
		}
		for _, alias := range []string{n.Review.BaselineTool, n.Review.StatusTool} {
			if _, ok := n.Tools[alias]; !ok {
				return fmt.Errorf("Normal target missing review tool %s", alias)
			}
		}
	}
	if n.TicketWorkflow != nil && !targetEqual(n.TicketWorkflow, installed.TicketWorkflow) {
		return errors.New("Normal target installed ticket policy drift")
	}
	if health {
		if err := ValidateExternalSchemas(ctx, selected); err != nil {
			return err
		}
		if n.Review != nil {
			_, err := selectedLive.checkReviewTarget(ctx, n.ReviewTask)
			return err
		}
		return nil
	}
	if p.Mode != "Normal" {
		return nil
	}
	if p.Destination != n.Destination || p.PushNamespace != n.PushNamespace || p.TicketRoom != n.TicketRoom {
		return errors.New("Normal disposition differs from target destination/push namespace/ticket room pins")
	}
	for name, s := range doc.Stages {
		if s.Primitive != workflow.ExternalEffect {
			continue
		}
		t, ok := n.Tools[s.Subject]
		if !ok {
			return fmt.Errorf("Normal target missing stage tool %s", name)
		}
		if s.Role != t.Role || !targetEqual(s.Pins, t.Arguments) {
			return fmt.Errorf("Normal target stage %s differs from exact tool role/argument pins", name)
		}
		if reviewOperation(installed.reviewOperation(p.ID, s.Subject)) && (n.Review == nil || p.ID != n.ReviewTask) {
			return errors.New("Normal target missing exact task review workspace policy")
		}
		if installed.TicketWorkflow != nil {
			for _, action := range installed.TicketWorkflow.Actions {
				if action.Tool == s.Subject && n.TicketWorkflow == nil {
					return errors.New("Normal target missing installed ticket workflow policy")
				}
			}
		}
	}
	return nil
}

// checkTaskTarget also protects asynchronous outbox mutations outside dispatch.
func (d *Daemon) checkTaskTarget(ctx context.Context, task string) error {
	d.mu.Lock()
	snapshot, ok := d.jobs[task]
	d.mu.Unlock()
	if !ok {
		return errors.New("target policy task missing")
	}
	var env envelope
	if err := decodeEnvelope(snapshot.Payload, &env); err != nil {
		return err
	}
	return d.checkPinnedTarget(ctx, env.Admission, env.Job.Document)
}
