package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/execution"
	"github.com/xleelz/fae/external"
	"github.com/xleelz/fae/workflow"
	"github.com/xleelz/fae/worktree"
)

type ReviewWorkspaceParams struct {
	Task               string         `json:"task"`
	RunID              string         `json:"run_id,omitempty"`
	Adapter            AdapterBinding `json:"adapter,omitempty"`
	AuthorizingMessage string         `json:"authorizing_message,omitempty"`
}
type ReviewWorkspacePlan struct {
	Task           string `json:"task"`
	RunID          string `json:"run_id"`
	Workspace      string `json:"workspace"`
	Branch         string `json:"branch"`
	Base           string `json:"base"`
	IntegrationTip string `json:"integration_tip"`
}
type ReviewWorkspaceRecord struct {
	ReviewWorkspacePlan
	Adapter   AdapterBinding         `json:"adapter"`
	Policy    external.ReviewPolicy  `json:"policy"`
	State     string                 `json:"state"`
	Health    ReviewTargetHealth     `json:"health"`
	Provision *ReviewProvisionRecord `json:"provision,omitempty"`
}

func reviewWorkspaceKey(task string) string {
	return "review-workspace/" + controlplane.Digest([]byte(task))
}
func (d *Daemon) reviewWorkspacePlan(ctx context.Context, p ReviewWorkspaceParams) (ReviewWorkspacePlan, error) {
	var out ReviewWorkspacePlan
	if d.live == nil || p.Task == "" || len(p.Task) > 255 {
		return out, errors.New("review workspace requires live runtime and task identity")
	}
	if p.RunID == "" {
		p.RunID = d.config.RunID
	}
	if len(p.RunID) > 255 {
		return out, errors.New("run identity exceeds bound")
	}
	binding := d.live.manager.Binding(p.RunID, p.Task)
	repo := worktree.Binding{Path: d.config.Workspace}
	base, err := effectGit(ctx, repo, "rev-parse", "--verify", d.live.config.Base+"^{commit}")
	if err != nil {
		return out, err
	}
	tip, err := effectGit(ctx, repo, "rev-parse", "--verify", d.live.config.IntegrationRef+"^{commit}")
	if err != nil {
		return out, err
	}
	return ReviewWorkspacePlan{Task: p.Task, RunID: p.RunID, Workspace: binding.Path, Branch: binding.Branch, Base: base, IntegrationTip: tip}, nil
}
func (d *Daemon) reviewWorkspaceRecord(ctx context.Context, task string) (ReviewWorkspaceRecord, error) {
	var record ReviewWorkspaceRecord
	raw, err := d.store.GetMarker(ctx, reviewWorkspaceKey(task))
	if err != nil {
		return record, err
	}
	if err = decode(raw, &record); err != nil {
		return record, err
	}
	if record.Task != task {
		return record, errors.New("review workspace identity mismatch")
	}
	return record, nil
}
func (d *Daemon) reviewWorkspaceRPC(ctx context.Context, r api.Request) (any, error) {
	var p ReviewWorkspaceParams
	if err := decode(r.Params, &p); err != nil {
		return nil, err
	}
	if r.Method == "ReviewWorkspacePlan" {
		return d.reviewWorkspacePlan(ctx, p)
	}
	principal, ok := api.AuthenticatedPrincipal(ctx)
	if !ok || principal.Actor != "operator" || principal.Task != "" {
		return nil, errors.New("review workspace preparation requires instance operator")
	}
	var err error
	ctx, err = d.store.AuthorizeAction(ctx, p.AuthorizingMessage, principal.Actor, r.Method, "governance", false, actionClasses())
	if err != nil {
		return nil, err
	}
	// Only explicit preparations serialize here. Neither drive nor read paths use this lock.
	for !d.reviewProvisionMu.TryLock() {
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	defer d.reviewProvisionMu.Unlock()
	audit := controlplane.Audit{Actor: principal.Actor, Action: r.Method, MessageID: p.AuthorizingMessage, Inputs: r.Params, Outcome: "prepared"}
	return d.prepareReviewWorkspace(ctx, p, audit)
}
func (d *Daemon) prepareReviewWorkspace(ctx context.Context, p ReviewWorkspaceParams, audit controlplane.Audit) (ReviewWorkspaceRecord, error) {
	plan, err := d.reviewWorkspacePlan(ctx, p)
	if err != nil {
		return ReviewWorkspaceRecord{}, err
	}
	binding, bundle, err := d.resolveAdapter(ctx, p.Adapter)
	if err != nil {
		return ReviewWorkspaceRecord{}, err
	}
	policy, ok := bundle.ReviewPolicies[p.Task]
	if !CanPrepareReviewWorkspace(ok && policy.Package != "" && policy.BaselineApproved, policy.Workspace == plan.Workspace && policy.Branch == plan.Branch && policy.IntegrationTip != "") {
		return ReviewWorkspaceRecord{}, fmt.Errorf("TargetConfigurationDefect: adapter review policy requires approved package baseline and exact planned binding for %s", p.Task)
	}
	record := ReviewWorkspaceRecord{ReviewWorkspacePlan: plan, Adapter: binding, Policy: policy, State: "pending"}
	existing, readErr := d.reviewWorkspaceRecord(ctx, p.Task)
	if readErr == nil {
		if existing.State != "pending" && existing.State != "ready" {
			return record, errors.New("invalid durable review preparation state")
		}
		// Base and integration tips can advance after preparation. A binding never silently rebases.
		if existing.RunID != record.RunID || existing.Adapter != record.Adapter || !targetEqual(existing.Policy, record.Policy) || existing.Workspace != record.Workspace || existing.Branch != record.Branch {
			return record, errors.New("immutable review workspace binding differs")
		}
		record = existing
	} else if !errors.Is(readErr, sql.ErrNoRows) {
		return record, readErr
	}
	if errors.Is(readErr, sql.ErrNoRows) && policy.IntegrationTip != plan.IntegrationTip {
		return record, errors.New("TargetConfigurationDefect: planned integration tip changed")
	}
	if err = initializeReviewProvision(&record, bundle); err != nil {
		return record, err
	}
	if d.live.guard == nil {
		return record, errors.New("review workspace weighted host admission unavailable")
	}
	sid := "review-prepare/" + controlplane.Digest([]byte(p.Task))
	release, err := d.live.guard.Acquire(ctx, engine.Dispatch{ID: sid, TaskID: p.Task, Config: workflow.Stage{Weight: 1}}, execution.Binding{}, engine.Session{ID: sid})
	if err != nil {
		return record, err
	}
	defer release()
	if errors.Is(readErr, sql.ErrNoRows) {
		raw, _ := json.Marshal(record)
		if err = d.store.PutActiveAdapterMarker(ctx, audit, reviewWorkspaceKey(p.Task), raw, binding.ID); err != nil {
			return record, err
		}
	}
	// Ensure creates only a deterministic registered task worktree, or verifies its exact identity.
	w := d.live.manager.Binding(record.RunID, p.Task)
	if record.State == "ready" {
		err = d.live.manager.Verify(ctx, w)
	} else {
		w, err = d.live.manager.Ensure(ctx, record.RunID, p.Task, record.Base)
	}
	if err != nil {
		return record, err
	}
	if w.Path != record.Workspace || w.Branch != record.Branch {
		return record, errors.New("prepared worktree binding changed")
	}
	if err = d.provisionReviewWorkspace(ctx, &record, bundle, audit); err != nil {
		return record, err
	}
	selected := *d.live
	selected.config = d.live.config
	selected.config.ExternalTools = bundle.Tools
	selected.config.ReviewPolicies = bundle.ReviewPolicies
	selected.config.TicketWorkflow = bundle.TicketWorkflow
	selected.config.DisableReviewPublishMerge = bundle.DisableReviewPublishMerge
	health, err := selected.checkReviewTarget(ctx, p.Task)
	if err != nil {
		return record, err
	}
	if record.State == "ready" {
		return record, nil
	}
	if !CanFinishFleetProvision(record.Provision == nil || record.Provision.Attempted, record.Provision == nil || record.Provision.Receipt != nil, err == nil) {
		return record, errors.New("fleet provisioning evidence incomplete")
	}
	record.State = "ready"
	record.Health = health
	raw, _ := json.Marshal(record)
	if err = d.store.PutMarker(ctx, audit, reviewWorkspaceKey(p.Task), raw); err != nil {
		return record, err
	}
	return record, nil
}

func (d *Daemon) reviewWorkspaceHealth(ctx context.Context, task string) (ReviewTargetHealth, error) {
	d.mu.Lock()
	_, admitted := d.jobs[task]
	d.mu.Unlock()
	if admitted {
		selected, err := d.liveForTask(ctx, task)
		if err != nil {
			return ReviewTargetHealth{}, err
		}
		return selected.checkReviewTarget(ctx, task)
	}
	record, err := d.reviewWorkspaceRecord(ctx, task)
	if errors.Is(err, sql.ErrNoRows) {
		return d.live.checkReviewTarget(ctx, task)
	}
	if err != nil {
		return ReviewTargetHealth{}, err
	}
	if record.Policy.ProvisionTool != "" && (record.Provision == nil || record.Provision.Receipt == nil) {
		return ReviewTargetHealth{}, errors.New("TargetConfigurationDefect: fleet provisioning unproved")
	}
	_, bundle, err := d.resolveAdapter(ctx, record.Adapter)
	if err != nil {
		return ReviewTargetHealth{}, err
	}
	if err = initializeReviewProvision(&record, bundle); err != nil {
		return ReviewTargetHealth{}, err
	}
	selected := *d.live
	selected.config = adapterConfig(d.live.config, bundle)
	return selected.checkReviewTarget(ctx, task)
}

// validatePreparedWorkspace is read-only; hot paths must never recreate a lost prepared binding.
func (d *Daemon) validatePreparedWorkspace(ctx context.Context, record ReviewWorkspaceRecord) error {
	if d.live == nil || record.State != "ready" || record.Task == "" || record.RunID == "" {
		return errors.New("TargetConfigurationDefect: prepared workspace unavailable")
	}
	binding := d.live.manager.Binding(record.RunID, record.Task)
	if binding.Path != record.Workspace || binding.Branch != record.Branch || record.Policy.Workspace != binding.Path || record.Policy.Branch != binding.Branch {
		return errors.New("TargetConfigurationDefect: prepared workspace ownership differs")
	}
	if record.Policy.ProvisionTool != "" {
		_, bundle, err := d.resolveAdapter(ctx, record.Adapter)
		if err != nil {
			return err
		}
		if err = initializeReviewProvision(&record, bundle); err != nil {
			return err
		}
	}
	if err := d.live.manager.Verify(ctx, binding); err != nil {
		return fmt.Errorf("TargetConfigurationDefect: prepared workspace %s unavailable: %w", record.Workspace, err)
	}
	return nil
}
