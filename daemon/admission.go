package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
	"os/exec"
	"strings"
)

// admit is the shared operator/controller/schedule admission boundary. The
// serialized daemon writer must hold mu; SQL idempotency precedes host effects.
func (d *Daemon) admit(ctx context.Context, key string, rawParams json.RawMessage, audit controlplane.Audit) (any, error) {
	if d.draining {
		return nil, errors.New("draining: admissions paused until restart")
	}
	if original, found, err := d.store.LookupAdmission(ctx, key, rawParams); err != nil {
		return nil, err
	} else if found {
		return identity(original), nil
	}
	snapshot, err := d.prepareAdmission(ctx, rawParams)
	if err != nil {
		return nil, err
	}
	saved, err := d.store.AdmitJob(ctx, audit, key, snapshot)
	if err != nil {
		return nil, err
	}
	// Replays return the original response; never rewind the live checkpoint.
	if _, exists := d.jobs[snapshot.ID]; !exists {
		d.jobs[snapshot.ID] = saved
	}
	d.publishStatus()
	d.signal()
	d.notifyEvents()
	return identity(saved), nil
}

// prepareAdmission validates and pins a task without writing admission state.
func (d *Daemon) prepareAdmission(ctx context.Context, rawParams json.RawMessage) (controlplane.JobSnapshot, error) {
	var p AdmitParams
	if err := decode(rawParams, &p); err != nil {
		return controlplane.JobSnapshot{}, err
	}
	if p.ID == "" {
		return controlplane.JobSnapshot{}, errors.New("task id required")
	}
	if p.Priority == 0 {
		p.Priority = 10
	}
	if p.Priority < 10 {
		return controlplane.JobSnapshot{}, errors.New("priority must be at least 10")
	}
	if p.Mode == "" {
		p.Mode = "Local"
	}
	if p.Mode != "Local" && p.Mode != "Normal" {
		return controlplane.JobSnapshot{}, errors.New("unknown disposition")
	}
	if p.Mode == "Local" && (p.CommitExternal || p.TicketRoom != "" || p.Destination != "" || p.PushNamespace != "") {
		return controlplane.JobSnapshot{}, errors.New("Local disposition cannot grant external effects")
	}
	if p.Mode == "Normal" && d.live == nil {
		return controlplane.JobSnapshot{}, errors.New("Normal disposition requires live runtime")
	}
	if p.CommitExternal {
		if p.Destination == "" || p.PushNamespace == "" {
			return controlplane.JobSnapshot{}, errors.New("live Normal requires destination and push_namespace")
		}
		if e := d.store.RequireExternalAuthority(ctx, p.AuthorizingMessage); e != nil {
			return controlplane.JobSnapshot{}, e
		}
	}
	if p.RunID == "" {
		p.RunID = d.config.RunID
	}
	if p.WorkflowID == "" {
		p.WorkflowID = defaults.Workflow().ID
	}
	rev, raw, err := d.store.GetWorkflow(ctx, p.WorkflowID, p.Revision)
	if err != nil {
		return controlplane.JobSnapshot{}, err
	}
	p.Revision = rev.Number
	if p.Tree == "" {
		cmd := exec.CommandContext(ctx, "git", "rev-parse", "HEAD^{tree}")
		cmd.Dir = d.config.Workspace
		cmd.Env = cleanGitEnv()
		out, err := cmd.Output()
		if err != nil {
			return controlplane.JobSnapshot{}, fmt.Errorf("resolve workspace tree: %w", err)
		}
		p.Tree = strings.TrimSpace(string(out))
	}
	doc, err := workflow.Parse(raw)
	if err != nil {
		return controlplane.JobSnapshot{}, err
	}
	if p.TargetConfig != "" {
		_, targetRaw, e := d.store.GetTarget(ctx, p.TargetConfig, p.TargetRevision)
		if e != nil {
			return controlplane.JobSnapshot{}, e
		}
		target, e := parseTarget(targetRaw)
		if e != nil {
			return controlplane.JobSnapshot{}, e
		}
		if target.Normal != nil && target.Normal.Adapter != nil {
			if e = d.store.AdapterActive(ctx, target.Normal.Adapter.ID); e != nil {
				return controlplane.JobSnapshot{}, e
			}
		}
	}
	// Validate versioned policy before any review baseline adapter read.
	if err = d.pinTarget(ctx, &p, doc); err != nil {
		return controlplane.JobSnapshot{}, err
	}
	admissionLive := d.live
	if d.live != nil {
		admissionLive, err = d.liveForAdmission(ctx, p)
		if err != nil {
			return controlplane.JobSnapshot{}, err
		}
	}

	if d.live != nil {
		doc, err = reviewDocument(admissionLive.config, p.ID, doc)
		if err != nil {
			return controlplane.JobSnapshot{}, err
		}
		hasReview := false
		for _, stage := range doc.Stages {
			if stage.Primitive == workflow.ExternalEffect && reviewOperation(admissionLive.config.reviewOperation(p.ID, stage.Subject)) {
				hasReview = true
			}
		}
		if hasReview && p.CommitExternal {
			if _, err = admissionLive.checkReviewTargetEvidence(ctx, p.ID); err != nil {
				if ctx.Value(backlogPreparationKey{}) != nil {
					return controlplane.JobSnapshot{}, err
				}
				raw, _ := json.Marshal(map[string]string{"task": p.ID, "defect": err.Error()})
				if save := d.store.PutMarker(ctx, controlplane.Audit{Actor: "daemon", Action: "review target health", Outcome: "blocked", Inputs: raw}, "review-target/"+p.ID, raw); save != nil {
					return controlplane.JobSnapshot{}, save
				}
				return controlplane.JobSnapshot{}, err
			}
		}
	}
	if d.live != nil {
		if e := validateExternalStages(admissionLive.config, doc, p); e != nil {
			return controlplane.JobSnapshot{}, e
		}
	}
	if d.live != nil && d.live.guard.panes != nil {
		if err := validatePaneFloor(doc, d.live.guard.effectivePaneLimit()); err != nil {
			return controlplane.JobSnapshot{}, err
		}
	}
	if p.TargetConfig != "" {
		if err = d.store.TargetActive(ctx, p.TargetConfig); err != nil {
			return controlplane.JobSnapshot{}, err
		}
	}
	if err = d.pinTarget(ctx, &p, doc); err != nil {
		return controlplane.JobSnapshot{}, err
	}
	var job engine.Job
	if d.live != nil {
		w, e := d.live.manager.Ensure(ctx, p.RunID, p.ID, admissionLive.config.Base)
		if e != nil {
			return controlplane.JobSnapshot{}, e
		}
		cmd := exec.CommandContext(ctx, "git", "rev-parse", "HEAD", admissionLive.config.Base+"^{tree}")
		cmd.Dir = w.Path
		cmd.Env = cleanGitEnv()
		raw, e := cmd.Output()
		if e != nil {
			return controlplane.JobSnapshot{}, e
		}
		expected := strings.Fields(string(raw))
		if len(expected) != 2 || expected[0] != admissionLive.config.Base {
			return controlplane.JobSnapshot{}, errors.New("stale task branch residue: HEAD differs from pinned base")
		}
		actual, e := d.live.broker.Tree(ctx, w)
		if e != nil {
			return controlplane.JobSnapshot{}, e
		}
		if actual != expected[1] {
			return controlplane.JobSnapshot{}, errors.New("stale task branch residue: task worktree is not pristine")
		}
		// A live admission takes its tree from the owned task worktree.
		p.Tree = actual
		job, err = engine.NewLive(p.ID, int(rev.Number), doc, actual)
		job.Base = admissionLive.config.Base
	} else {
		job, err = engine.New(p.ID, int(rev.Number), doc, p.Tree)
	}
	if err != nil {
		return controlplane.JobSnapshot{}, err
	}
	job.Disposition = domain.Disposition{Mode: domain.Mode(p.Mode), Live: p.CommitExternal, TicketRoom: p.TicketRoom, Destination: p.Destination, PushNamespace: p.PushNamespace}
	if d.live != nil {
		if err = configureReviewJob(admissionLive.config, &job); err != nil {
			return controlplane.JobSnapshot{}, err
		}
	}
	payload, _ := json.Marshal(envelope{job, p})
	snapshot := controlplane.JobSnapshot{ID: p.ID, WorkflowID: p.WorkflowID, WorkflowRevision: rev.Number, TargetID: d.config.Workspace, TargetRevision: 1, State: string(job.State), Payload: payload}
	if p.TargetConfig != "" {
		snapshot.TargetID = p.TargetConfig
		snapshot.TargetRevision = p.TargetRevision
	}
	return snapshot, nil
}
