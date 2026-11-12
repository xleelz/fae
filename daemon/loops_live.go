package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/execution"
	"github.com/xleelz/fae/loops"
	"github.com/xleelz/fae/providerhealth"
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
	profile := workflow.Profile{Allowed: []string{"read", "handoff"}, Denied: []string{"edit", "shell", "network"}}
	doc := workflow.Document{ID: "fae-loop:" + r.Spec.ID, Roles: map[string]workflow.Role{r.Spec.Role: {Scope: "task-bound", Cap: 1, Profile: profile, Routes: []workflow.Binding{r.Spec.Binding}}}}
	brief, _ := json.Marshal([]any{map[string]any{"kind": "stage", "stage": "provider-loop", "prompt": r.Spec.Prompt, "iteration": r.Iteration, "persistent_session": r.SessionID}, map[string]any{"kind": "kernel_contract", "output_schema": map[string]any{"type": "object", "properties": map[string]any{"message": map[string]string{"type": "string"}}, "required": []string{"message"}, "additionalProperties": false}}})
	var nonExecuted int64
	for starts := 1; starts <= loops.StartAttempts; starts++ {
		id := fmt.Sprintf("%s-%d-attempt-%d", r.SessionID, r.Iteration, r.Attempt)
		if starts > 1 {
			id = fmt.Sprintf("%s-%d-attempt-%d-start-%d", r.SessionID, r.Iteration, r.Attempt, starts)
		}
		out, retry, startErr := d.runLiveLoopStart(ctx, r, id, revision, profile, doc, brief, starts == loops.StartAttempts)
		nonExecuted += out.StartupNotExecuted
		if retry {
			continue
		}
		out.StartupNotExecuted = nonExecuted
		return out, startErr
	}
	result.StartupNotExecuted = nonExecuted
	return result, err
}

func (d *Daemon) runLiveLoopStart(ctx context.Context, r loops.Request, id, revision string, profile workflow.Profile, doc workflow.Document, brief []byte, finalStart bool) (result loops.Result, retry bool, err error) {
	dispatch := engine.Dispatch{ID: id, TaskID: r.SessionID, Stage: "loop", Config: workflow.Stage{Primitive: workflow.AgentTurn, Weight: 1}, Sessions: []engine.Session{{ID: id, Role: r.Spec.Role, Route: r.Spec.Binding}}}
	release, e := d.live.guard.Acquire(ctx, dispatch, execution.Binding{Worktree: d.config.Workspace, Document: doc, Mode: "local"}, dispatch.Sessions[0])
	if e != nil {
		var refusal *scheduler.AdmissionRefusal
		if errors.As(e, &refusal) {
			return result, false, &loops.DeferredError{Cause: e}
		}
		return result, false, e
	}
	defer func() {
		if releaseErr := release(); releaseErr != nil {
			retry = false
			err = errors.Join(err, releaseErr)
		}
	}()
	worktreeBinding, e := d.live.manager.Ensure(ctx, "provider-loops", r.SessionID, d.live.config.Base)
	if e != nil {
		return result, false, e
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
		return result, false, e
	}
	if e = d.live.guard.Launching(ctx, id); e != nil {
		return result, false, e
	}
	collected, e := d.live.runner.Run(ctx, agentruntime.Launch{WorkflowID: doc.ID, WorkflowRevision: revision, SessionPolicy: "task-bound", DispatchID: id, SessionID: id, TaskID: r.SessionID, Role: r.Spec.Role, Worktree: worktreeBinding.Path, Mode: "local", Profile: profile, Binding: r.Spec.Binding, Brief: brief})
	// FAE-RT-009: a loop turn's provider outcome is recorded against its
	// binding like a workflow turn's, so a throttled loop makes the binding
	// ineligible for workflow routing too.
	if outcome, observed := loopProviderOutcome(ctx, collected, e); observed {
		d.mu.Lock()
		observeErr := d.observeProvider(ctx, dispatch, controlplane.ProviderObservation{ID: id, TaskID: r.SessionID, Provider: r.Spec.Binding.Provider, Model: r.Spec.Binding.Model, Outcome: outcome, At: time.Now().UTC()})
		d.mu.Unlock()
		e = errors.Join(e, observeErr)
	}
	cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	retirementErr := d.live.guard.Retired(cleanup, id, collected.ProcessesRetired)
	cleanErr := clean(cleanup)
	var refusal interface{ AdmissionDeferred() bool }
	if e != nil && errors.As(e, &refusal) && refusal.AdmissionDeferred() && collected.NonExecution && collected.ProcessesRetired && retirementErr == nil && cleanErr == nil {
		return result, false, &loops.DeferredError{Cause: e}
	}
	// FAE-RT-004: a loop turn that provably never started (transport
	// non-execution, bridge refusal before the provider, or a provider that
	// never acknowledged the prompt) is relaunched within the agent-start
	// bound without counting as an execution.
	var bridgeRefusal *agentruntime.BridgeRefusal
	var turnErr *agentruntime.TurnError
	providerNotStarted := errors.As(e, &bridgeRefusal) && bridgeRefusal.Class == agentruntime.ProviderNotStarted
	refusedBeforeProvider := errors.As(e, &bridgeRefusal) && bridgeRefusal.Class == agentruntime.RefusedBeforeProvider
	launchRefused := errors.As(e, &turnErr) && turnErr.Class == agentruntime.LaunchRefused
	if e != nil && (collected.NonExecution || providerNotStarted || refusedBeforeProvider || launchRefused) && collected.ProcessesRetired && retirementErr == nil && cleanErr == nil && ctx.Err() == nil {
		result.StartupNotExecuted = 1
		if !finalStart {
			return result, true, e
		}
		if (collected.NonExecution || providerNotStarted) && !refusedBeforeProvider && !launchRefused {
			d.mu.Lock()
			observeErr := d.observeProvider(ctx, dispatch, controlplane.ProviderObservation{ID: id, TaskID: r.SessionID, Provider: r.Spec.Binding.Provider, Model: r.Spec.Binding.Model, Outcome: providerhealth.Unavailable, At: time.Now().UTC()})
			d.mu.Unlock()
			e = errors.Join(e, observeErr)
		}
	}
	e = errors.Join(e, retirementErr, cleanErr)
	if e != nil {
		return result, false, e
	}
	if !collected.ProcessesRetired {
		return result, false, errors.New("loop transport retirement unproven")
	}
	var output struct {
		Message *string `json:"message"`
	}
	if e = decode(collected.Output, &output); e != nil || output.Message == nil {
		return result, false, errors.New("loop output must match message schema")
	}
	state, _ := json.Marshal(map[string]any{"session": r.SessionID, "iterations": r.Iteration, "scope_revision": revision})
	return loops.Result{Context: state, Output: collected.Output}, false, nil
}

// loopProviderOutcome classifies a provider loop turn as the workflow
// executor classifies a turn, through the same session-log reader
// (providerhealth.Classify): a structured provider error in the turn's logs
// wins, then a usable answer is ok, a timeout or stall is Timeout, and
// anything else is MalformedOutput. Nothing is observed for a turn no
// provider ran (the transport's non-execution proof, a bridge refusal before
// the provider, an edit write the bridge undid) or one that never
// acknowledged the prompt (FAE-RT-004 charges that binding only through an
// exhausted start bound), nor for a turn Fae itself stopped: the loop's
// context ended (its expiry, its retirement or the daemon stopping).
func loopProviderOutcome(ctx context.Context, collected agentruntime.Collected, runErr error) (providerhealth.Outcome, bool) {
	if collected.NonExecution || ctx.Err() != nil {
		return "", false
	}
	var refusal *agentruntime.BridgeRefusal
	if errors.As(runErr, &refusal) && (refusal.BeforeProvider() || refusal.Class == agentruntime.ProviderNotStarted || refusal.Class == agentruntime.EditsNotApplied) {
		return "", false
	}
	valid := false
	if runErr == nil {
		var output struct {
			Message *string `json:"message"`
		}
		valid = decode(collected.Output, &output) == nil && output.Message != nil
	}
	var turn *agentruntime.TurnError
	stalled := errors.As(runErr, &turn) && turn.Class == "stalled"
	return providerhealth.Classify(collected.LogDirectory, valid, errors.Is(runErr, context.DeadlineExceeded) || stalled), true
}
