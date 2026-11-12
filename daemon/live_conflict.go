package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/effects"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/worktree"
)

type conflictPlan struct {
	TaskID, DispatchID string
	Plan               effects.ReconcileEvidence
	Paths              []string
}

func conflictPlanKey(task, dispatch string) string {
	raw, _ := json.Marshal([]string{task, dispatch})
	h, _ := domain.ContentHash(raw)
	return "conflict-plan/" + h
}
func (x *liveExecutor) resolveConflict(ctx context.Context, d engine.Dispatch, w worktree.Binding, plan effects.ReconcileEvidence, conflict *effects.ReconcileError, r engine.Result, scope liveTaskScope) (engine.Result, error) {
	// The original intent remains authoritative; only regular, declared production
	// paths may enter automated remediation. Frozen tests are never marked up.
	allowed := map[string]bool{}
	for _, p := range scope.Files {
		allowed[p] = true
	}
	if d.Config.Reverify == "" || len(conflict.Paths) == 0 {
		return r, fmt.Errorf("conflict remediation requires bounded reconciliation route")
	}
	for _, path := range conflict.Paths {
		if path == scope.TestFile || !allowed[path] {
			return r, fmt.Errorf("automatic conflict remediation refuses protected or undeclared path %s", path)
		}
	}
	previous, e := x.activeConflict(ctx, d.TaskID)
	if e != nil {
		return r, e
	}
	if previous != nil {
		return r, fmt.Errorf("additional conflict requires operator review; original own intent retained")
	}
	c := conflictPlan{TaskID: d.TaskID, DispatchID: d.ID, Plan: plan, Paths: append([]string{}, conflict.Paths...)}
	key := conflictPlanKey(d.TaskID, d.ID)
	raw, _ := json.Marshal(c)
	if e := x.daemon.store.PutMarker(ctx, controlplane.Audit{Actor: "daemon", Action: "prepare conflict remediation", Outcome: "conflicted"}, key, raw); e != nil {
		return r, e
	}
	if e := x.daemon.store.PutMarker(ctx, controlplane.Audit{Actor: "daemon", Action: "pin original conflict intent", Outcome: "active"}, activeConflictKey(d.TaskID), raw); e != nil {
		return r, e
	}
	if e := x.broker.ApplyConflict(ctx, w, plan, c.Paths); e != nil {
		return r, e
	}
	r.NewTree = plan.Tree
	r.NewBase = plan.Base
	r.ProducedAt = time.Now().UTC()
	r.Output, _ = json.Marshal(map[string]any{"result": "conflicted", "patch_identical": false, "paths": c.Paths, "original_base": plan.OwnBase, "original_tree": plan.OriginalTree, "base": plan.Base, "plan_key": key})
	return r, nil
}
func activeConflictKey(task string) string {
	raw, _ := json.Marshal(task)
	h, _ := domain.ContentHash(raw)
	return "active-conflict/" + h
}
func (x *liveExecutor) activeConflict(ctx context.Context, task string) (*conflictPlan, error) {
	raw, err := x.daemon.store.GetMarker(ctx, activeConflictKey(task))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var c conflictPlan
	if err = json.Unmarshal(raw, &c); err != nil {
		return nil, err
	}
	if c.TaskID != task {
		return nil, fmt.Errorf("active conflict belongs to another task")
	}
	original, err := x.daemon.store.GetMarker(ctx, conflictPlanKey(task, c.DispatchID))
	if err != nil {
		return nil, err
	}
	var pinned conflictPlan
	if err = json.Unmarshal(original, &pinned); err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(c, pinned) {
		return nil, fmt.Errorf("active conflict intent mismatch")
	}
	return &c, nil
}
func (x *liveExecutor) checkConflictResolution(ctx context.Context, d engine.Dispatch, w worktree.Binding) error {
	c, err := x.activeConflict(ctx, d.TaskID)
	if err != nil {
		return err
	}
	if c == nil {
		for _, a := range d.Inputs {
			if a.Type == "ReconcileReport" && a.Fields["result"] == "conflicted" {
				return fmt.Errorf("missing immutable conflict plan")
			}
		}
		return nil
	}
	matched := false
	for _, a := range d.Inputs {
		if a.Type != "ReconcileReport" || a.Fields["result"] != "conflicted" {
			continue
		}
		key, ok := a.Fields["plan_key"].(string)
		if !ok || key != conflictPlanKey(d.TaskID, c.DispatchID) || a.TaskID != d.TaskID || a.Tree != c.Plan.Tree || d.Base != c.Plan.Base || a.Fields["base"] != c.Plan.Base || a.Fields["original_base"] != c.Plan.OwnBase || a.Fields["original_tree"] != c.Plan.OriginalTree {
			return fmt.Errorf("conflict plan binding mismatch")
		}
		encoded, _ := json.Marshal(a.Fields["paths"])
		var paths []string
		if json.Unmarshal(encoded, &paths) != nil || !reflect.DeepEqual(paths, c.Paths) {
			return fmt.Errorf("conflict path binding mismatch")
		}
		matched = true
	}
	if !matched {
		return fmt.Errorf("active conflict requires named original context for implementation")
	}
	return x.broker.VerifyConflictResolution(ctx, w, c.Plan)
}

// Independently recheck the original contract at VCS boundaries so omitting an
// input in a custom graph cannot bypass preservation. A second changed base is
// conservatively refused rather than replacing the original intent silently.
func (x *liveExecutor) verifyActiveConflict(ctx context.Context, d engine.Dispatch, w worktree.Binding) error {
	c, err := x.activeConflict(ctx, d.TaskID)
	if err != nil {
		return err
	}
	if c == nil {
		return nil
	}
	return x.broker.VerifyConflictResolution(ctx, w, c.Plan)
}
