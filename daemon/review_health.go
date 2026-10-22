package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/external"
	"github.com/xleelz/fae/workflow"
	"github.com/xleelz/fae/worktree"
	"path/filepath"
)

type ReviewTargetHealth struct {
	Package        string `json:"package"`
	IntegrationTip string `json:"integration_tip"`
	Green          bool   `json:"green"`
	Defect         string `json:"defect,omitempty"`
}

func (x *liveExecutor) checkReviewTarget(ctx context.Context, task string) (ReviewTargetHealth, error) {
	p, ok := x.config.ReviewPolicies[task]
	h := ReviewTargetHealth{Package: p.Package, IntegrationTip: p.IntegrationTip}
	if !ok || p.Package == "" || !filepath.IsAbs(p.Workspace) || p.Branch == "" {
		return h, fmt.Errorf("TargetConfigurationDefect: missing pre-provisioned review workspace binding for %s", task)
	}
	real, e := filepath.EvalSymlinks(p.Workspace)
	if e != nil || real != filepath.Clean(p.Workspace) {
		return h, errors.New("TargetConfigurationDefect: review workspace missing or symlinked")
	}
	w := worktree.Binding{Path: p.Workspace, Branch: p.Branch}
	branch, e := effectGit(ctx, w, "symbolic-ref", "--short", "HEAD")
	if e != nil || branch != p.Branch {
		return h, fmt.Errorf("TargetConfigurationDefect: workspace %s branch differs from %s", p.Workspace, p.Branch)
	}
	tip, e := effectGit(ctx, w, "rev-parse", x.config.IntegrationRef+"^{commit}")
	if e != nil || tip == "" {
		return h, errors.New("TargetConfigurationDefect: integration tip unavailable")
	}
	p.IntegrationTip = tip
	h.IntegrationTip = tip
	selected := *x
	selected.config = x.config
	selected.config.ReviewPolicies = map[string]external.ReviewPolicy{}
	for key, value := range x.config.ReviewPolicies {
		selected.config.ReviewPolicies[key] = value
	}
	selected.config.ReviewPolicies[task] = p
	x = &selected
	if !p.BaselineApproved {
		return h, errors.New("TargetConfigurationDefect: fleet baseline mechanism not approved")
	}
	t, ok := x.config.ExternalTools[p.BaselineTool]
	if !ok || p.Operation(p.BaselineTool, t.Name) != "review_baseline" {
		return h, errors.New("TargetConfigurationDefect: installed baseline read capability missing")
	}
	dispatch := engine.Dispatch{ID: task + "/health/0", TaskID: task, Stage: "target-health", Tree: p.IntegrationTip, Config: workflow.Stage{Subject: p.BaselineTool, Role: t.Role, IdempotenceKey: "target-health"}, Sessions: []engine.Session{{ID: "health/" + task, Role: t.Role}}}
	r, e := x.reviewReadWithEvidence(ctx, dispatch, p.BaselineTool, reviewCheckpoint{}, false)
	if e != nil {
		return h, fmt.Errorf("TargetConfigurationDefect: %w", e)
	}
	var b struct {
		Package        string `json:"package"`
		IntegrationTip string `json:"integration_tip"`
		Status         string `json:"status"`
	}
	if e = json.Unmarshal(r.Data, &b); e != nil || b.Package != p.Package || b.IntegrationTip != p.IntegrationTip || b.Status != "Pass" {
		return h, fmt.Errorf("TargetConfigurationDefect: package %s fleet baseline not green", p.Package)
	}
	h.Green = true
	return h, nil
}

// Opt-out removes stages and their edges before hashing the admitted workflow.
// The resulting document is identical to explicitly authoring those stages out.
func reviewDocument(c LiveConfig, task string, doc workflow.Document) (workflow.Document, error) {
	p := c.ReviewPolicies[task]
	if !c.DisableReviewPublishMerge && !p.DisablePublishMerge {
		return doc, nil
	}
	for {
		remove := ""
		for name, s := range doc.Stages {
			op := c.reviewOperation(task, s.Subject)
			if s.Primitive == workflow.ExternalEffect && (op == "review_publish" || op == "review_merge") {
				remove = name
				break
			}
		}
		if remove == "" {
			break
		}
		successor := ""
		for _, e := range doc.Edges {
			if e.From == remove {
				if successor != "" {
					return doc, errors.New("disabled review stage must have one successor")
				}
				successor = e.To
			}
		}
		if successor == "" || successor == remove {
			return doc, errors.New("disabled review stage requires successor")
		}
		edges := []workflow.Edge{}
		for _, e := range doc.Edges {
			if e.From == remove {
				continue
			}
			if e.To == remove {
				e.To = successor
			}
			edges = append(edges, e)
		}
		doc.Edges = edges
		delete(doc.Stages, remove)
		if doc.Start == remove {
			doc.Start = successor
		}
	}
	return doc, nil
}

func (x *liveExecutor) checkReviewTargetEvidence(ctx context.Context, task string) (ReviewTargetHealth, error) {
	h, e := x.checkReviewTarget(ctx, task)
	outcome := "green"
	if e != nil {
		outcome = "blocked"
		h.Defect = e.Error()
	}
	raw, _ := json.Marshal(h)
	if save := x.daemon.store.PutMarker(ctx, controlplane.Audit{Actor: "daemon", Action: "review package baseline", Outcome: outcome, Inputs: raw}, "review-target/"+task, raw); save != nil {
		return h, save
	}
	return h, e
}
