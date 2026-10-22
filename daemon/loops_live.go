package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/execution"
	"github.com/xleelz/fae/loops"
	agentruntime "github.com/xleelz/fae/runtime"
	"github.com/xleelz/fae/scheduler"
	"github.com/xleelz/fae/workflow"
	"os/exec"
	"strings"
	"time"
)

func (d *Daemon) runLiveLoop(ctx context.Context, r loops.Request) (result loops.Result, err error) {
	raw, _ := json.Marshal(r.Spec)
	revision := fmt.Sprintf("%x", sha256.Sum256(raw))
	id := fmt.Sprintf("%s-%d-attempt-%d", r.SessionID, r.Iteration, r.Attempt)
	profile := workflow.Profile{Allowed: []string{"read", "handoff"}, Denied: []string{"edit", "shell", "network"}}
	doc := workflow.Document{ID: "fae-loop:" + r.Spec.ID, Roles: map[string]workflow.Role{r.Spec.Role: {Scope: "task-bound", Cap: 1, Profile: profile, Routes: []workflow.Binding{r.Spec.Binding}}}}
	dispatch := engine.Dispatch{ID: id, TaskID: r.SessionID, Stage: "loop", Config: workflow.Stage{Primitive: workflow.AgentTurn, Weight: 1}, Sessions: []engine.Session{{ID: id, Role: r.Spec.Role, Route: r.Spec.Binding}}}
	release, e := d.live.guard.Acquire(ctx, dispatch, execution.Binding{Worktree: d.config.Workspace, Document: doc, Mode: "local"}, dispatch.Sessions[0])
	if e != nil {
		var refusal *scheduler.AdmissionRefusal
		if errors.As(e, &refusal) {
			return result, &loops.DeferredError{Cause: e}
		}
		return result, e
	}
	defer func() {
		if releaseErr := release(); releaseErr != nil {
			err = errors.Join(err, releaseErr)
		}
	}()
	worktreeBinding, e := d.live.manager.Ensure(ctx, "provider-loops", r.SessionID, d.live.config.Base)
	if e != nil {
		return result, e
	}
	clean := func(ctx context.Context) error {
		cmd := exec.CommandContext(ctx, "git", "status", "--porcelain=v1", "--untracked-files=all")
		cmd.Dir = worktreeBinding.Path
		cmd.Env = cleanGitEnv()
		out, e := cmd.Output()
		if e != nil {
			return e
		}
		if len(out) != 0 {
			return errors.New("read-only loop worktree changed")
		}
		cmd = exec.CommandContext(ctx, "git", "rev-parse", "HEAD")
		cmd.Dir = worktreeBinding.Path
		cmd.Env = cleanGitEnv()
		out, e = cmd.Output()
		if e != nil {
			return e
		}
		if strings.TrimSpace(string(out)) != d.live.config.Base {
			return errors.New("read-only loop base changed")
		}
		return nil
	}
	if e = clean(ctx); e != nil {
		return result, e
	}
	if e = d.live.guard.Launching(ctx, id); e != nil {
		return result, e
	}
	brief, _ := json.Marshal([]any{map[string]any{"kind": "stage", "stage": "provider-loop", "prompt": r.Spec.Prompt, "iteration": r.Iteration, "persistent_session": r.SessionID}, map[string]any{"kind": "kernel_contract", "output_schema": map[string]any{"type": "object", "properties": map[string]any{"message": map[string]string{"type": "string"}}, "required": []string{"message"}, "additionalProperties": false}}})
	collected, e := d.live.runner.Run(ctx, agentruntime.Launch{WorkflowID: doc.ID, WorkflowRevision: revision, SessionPolicy: "task-bound", DispatchID: id, SessionID: id, TaskID: r.SessionID, Role: r.Spec.Role, Worktree: worktreeBinding.Path, Mode: "local", Profile: profile, Binding: r.Spec.Binding, Brief: brief})
	cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	retirementErr := d.live.guard.Retired(cleanup, id, collected.ProcessesRetired)
	cleanErr := clean(cleanup)
	var refusal interface{ AdmissionDeferred() bool }
	if e != nil && errors.As(e, &refusal) && refusal.AdmissionDeferred() && collected.NonExecution && collected.ProcessesRetired && retirementErr == nil && cleanErr == nil {
		return result, &loops.DeferredError{Cause: e}
	}
	e = errors.Join(e, retirementErr, cleanErr)
	if e != nil {
		return result, e
	}
	if !collected.ProcessesRetired {
		return result, errors.New("loop transport retirement unproven")
	}
	var output struct {
		Message *string `json:"message"`
	}
	if e = decode(collected.Output, &output); e != nil || output.Message == nil {
		return result, errors.New("loop output must match message schema")
	}
	state, _ := json.Marshal(map[string]any{"session": r.SessionID, "iterations": r.Iteration, "scope_revision": revision})
	return loops.Result{Context: state, Output: collected.Output}, nil
}
