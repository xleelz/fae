// Package execution connects durable dispatches to real agent bridge processes.
package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/kernel"
	"github.com/xleelz/fae/providerhealth"
	"github.com/xleelz/fae/runtime"
	"github.com/xleelz/fae/workflow"
)

type Binding struct {
	ExecutionHead         string
	Worktree              string
	Base                  string
	Document              workflow.Document
	OperatorContext, Mode string
}
type Admission interface {
	Acquire(context.Context, engine.Dispatch, Binding, engine.Session) (func() error, error)
}

// AdmissionLifecycle persists launch/retirement boundaries when supported.
type AdmissionLifecycle interface {
	Launching(context.Context, string) error
	Retired(context.Context, string, bool) error
}
type Executor struct {
	// StallSafeReplay is an explicit trusted bridge contract: no external effects
	// before validated edits; unchanged tree alone is insufficient proof.
	StallSafeReplay bool
	Observe         func(context.Context, engine.Dispatch, engine.Session, providerhealth.Outcome) error
	CheckChange     func(context.Context, engine.Dispatch, Binding) error
	Runtime         interface {
		Run(context.Context, runtime.Launch) (runtime.Collected, error)
	}
	Resolve   func(context.Context, engine.Dispatch) (Binding, error)
	Tree      func(context.Context, Binding) (string, error)
	Admission Admission
	Effects   engine.Executor
}

func (x *Executor) Execute(ctx context.Context, d engine.Dispatch) (r engine.Result, err error) {
	if d.Config.Primitive != workflow.AgentTurn && d.Config.Primitive != workflow.Review {
		if x.Effects == nil {
			return r, errors.New("no real effects executor configured")
		}
		return x.Effects.Execute(ctx, d)
	}
	if d.ExecutionKind != "live" {
		return r, errors.New("live executor requires live dispatch")
	}
	r = engine.Result{ExecutionKind: "live", DispatchID: d.ID, TaskID: d.TaskID, Tree: d.Tree, ProducedAt: time.Now().UTC()}
	if len(d.Sessions) == 0 {
		return r, errors.New("dispatch sessions required")
	}
	r.Session = d.Sessions[0].ID
	if x.Runtime == nil || x.Resolve == nil || x.Tree == nil || x.Admission == nil {
		return r, errors.New("live runtime, binding, tree and durable admission required")
	}
	binding, e := x.Resolve(ctx, d)
	if e != nil {
		return r, e
	}
	revision, e := workflow.Hash(binding.Document)
	if e != nil {
		return r, e
	}
	before, e := x.Tree(ctx, binding)
	if e != nil {
		return r, e
	}
	if before != d.Tree {
		return r, errors.New("task tree changed before agent dispatch")
	}
	for sessionIndex, session := range d.Sessions {
		role, ok := binding.Document.Roles[session.Role]
		if !ok || len(role.Routes) == 0 {
			return r, fmt.Errorf("role %q has no route", session.Role)
		}
		route := session.Route
		if route.Provider == "" {
			route = role.Routes[0]
		}
		declared := false
		for _, candidate := range role.Routes {
			if candidate == route {
				declared = true
			}
		}
		session.Route = route
		if !declared {
			return r, errors.New("session route not declared")
		}
		template := role.Briefs[d.Config.Phase]
		if template == "" {
			template = role.Briefs["default"]
		}
		// Ordered blocks retain typed artifact data and place the kernel contract last.
		brief, e := json.Marshal([]any{
			map[string]any{"kind": "role_instructions", "text": role.Instructions},
			map[string]any{"kind": "stage", "template": template, "task_id": d.TaskID, "stage": d.Stage, "inputs": d.Inputs},
			map[string]any{"kind": "operator_context", "text": binding.OperatorContext},
			map[string]any{"kind": "kernel_contract", "output_schema": binding.Document.Artifacts[d.Config.Output].Schema, "dispatch_id": d.ID, "session_id": session.ID, "task_id": d.TaskID, "frozen_artifacts": "Do not modify frozen tests; request rework instead", "obligations": "Return structured handoff through configured absolute path; no external effects beyond granted profile"},
		})
		if e != nil {
			return r, e
		}
		release, e := x.Admission.Acquire(ctx, d, binding, session)
		if e != nil {
			// Only a typed no-launch infrastructure refusal is retryable.
			// A partially executed multi-review dispatch remains uncertain.
			var refusal interface{ AdmissionDeferred() bool }
			r.Class = "uncertain"
			if errors.As(e, &refusal) && refusal.AdmissionDeferred() && sessionIndex == 0 {
				r.Class = "infrastructure-deferred"
			}
			return r, e
		}
		if release == nil {
			return r, errors.New("admission supplied no release")
		}
		// Keep admission through handoff collection, tree measurement and policy
		// checks. A failed validation still releases exactly once.
		sessionErr := func() (err error) {
			defer func() {
				releaseErr := release()
				if releaseErr != nil {
					r.Class = "uncertain"
				}
				err = errors.Join(err, releaseErr)
			}()
			lifecycle, _ := x.Admission.(AdmissionLifecycle)
			if lifecycle != nil {
				if e := lifecycle.Launching(ctx, session.ID); e != nil {
					return e
				}
			}
			out, runErr := x.Runtime.Run(ctx, runtime.Launch{WorkflowID: binding.Document.ID, WorkflowRevision: revision, SessionPolicy: role.Scope, FreshContext: d.Config.Primitive == workflow.Review, DispatchID: d.ID, SessionID: session.ID, TaskID: d.TaskID, Role: session.Role, Worktree: binding.Worktree, Mode: binding.Mode, Profile: role.Profile, Binding: route, Brief: brief})
			if runErr == nil {
				runErr = workflow.ValidateArtifact(binding.Document.Artifacts[d.Config.Output].Schema, out.Output)
			}
			observationFailed := false
			// A proven pre-provider failure is a transport failure, not evidence
			// of unhealthy routing. Charging it could suppress the bounded retry.
			if x.Observe != nil && !out.NonExecution {
				var turnError *runtime.TurnError
				stalled := errors.As(runErr, &turnError) && turnError.Class == "stalled"
				outcome := providerhealth.Classify(out.LogDirectory, runErr == nil, errors.Is(runErr, context.DeadlineExceeded) || stalled)
				if e := x.Observe(ctx, d, session, outcome); e != nil {
					observationFailed = true
					runErr = errors.Join(runErr, e)
				}
			}
			if lifecycle != nil {
				if e := lifecycle.Retired(ctx, session.ID, out.ProcessesRetired); e != nil {
					return errors.Join(runErr, e)
				}
			}
			if runErr != nil {
				r.Class = "uncertain"
				var turn *runtime.TurnError
				if kernel.CanRetryStartup(out.NonExecution && !observationFailed, out.ProcessesRetired, sessionIndex == 0) {
					after, treeErr := x.Tree(ctx, binding)
					if treeErr == nil && after == before {
						r.Class = "startup-retry"
						var refusal interface{ AdmissionDeferred() bool }
						if errors.As(runErr, &refusal) && refusal.AdmissionDeferred() {
							r.Class = "infrastructure-deferred"
						}
					}
				}
				if errors.As(runErr, &turn) && turn.Class == "stalled" {
					after, treeErr := x.Tree(ctx, binding)
					if kernel.CanRetryStall(x.StallSafeReplay && !observationFailed, out.ProcessesRetired, treeErr == nil && after == before, sessionIndex == 0) {
						r.Class = "stall-retry"
					}
				}
				return runErr
			}
			after, e := x.Tree(ctx, binding)
			if e != nil {
				r.Class = "uncertain"
				return e
			}
			if !d.Config.ChangeProducer || d.Config.Primitive == workflow.Review {
				if after != before {
					r.Class = "uncertain"
					return errors.New("read-only role modified task tree")
				}
			} else {
				if x.CheckChange == nil {
					r.Class = "uncertain"
					return errors.New("live change validation required")
				}
				if e := x.CheckChange(ctx, d, binding); e != nil {
					r.Class = "uncertain"
					return e
				}
				r.NewTree = after
			}
			r.ProducedAt = out.ProducedAt
			if d.Config.Primitive == workflow.Review {
				r.Reviews = append(r.Reviews, engine.ReviewResult{Session: session.ID, Role: session.Role, Output: out.Output})
			} else {
				r.Output = out.Output
			}
			return nil
		}()
		if sessionErr != nil {
			if r.Class != "stall-retry" && r.Class != "startup-retry" && r.Class != "infrastructure-deferred" {
				r.Class = "uncertain"
			}
			return r, sessionErr
		}

	}
	return r, nil
}
