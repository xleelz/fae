// Package execution connects durable dispatches to real agent bridge processes.
package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/kernel"
	"github.com/xleelz/fae/provider"
	"github.com/xleelz/fae/providerhealth"
	"github.com/xleelz/fae/runtime"
	"github.com/xleelz/fae/workflow"
)

type Binding struct {
	OutputOwnershipRegistry       string
	ExcludedOutputDirs            []string
	SharedWriteRegistry           string
	RuntimeMountIdentities        map[string]string
	RuntimeMounts, CacheDirs      []string
	ShellRuntimeMountsWithoutView bool
	AncestorOutputDirs            []string
	TemplateJob                   *engine.Job
	Scratch                       []string
	OnToolEvent                   func(engine.Dispatch, domain.ToolEvent)
	OutputDir, StagingDir         string
	ArtifactsOnly                 bool
	TaskWrites                    []string
	DeniedTestCommands            [][]string
	ExecutionHead                 string
	Worktree                      string
	Base                          string
	Document                      workflow.Document
	OperatorContext, Mode         string
	Notifications                 []engine.TaskNotification
	// Scope is the task's resolved file scope, reported to every agent turn
	// in the task_scope brief block.
	Scope                 TaskScope
	InlineFileBytes       int
	InlineTurnBytes       int
	CheckpointIntervalSec int
	TaskReads             *workflow.ReadPolicy
	DenyPaths             []string
	ReadRoots             map[string]string
	ReadRootViews         map[string]provider.ReadRootView
	ResumeSession         string
	Provenance            []engine.Provenance
	OnRefusedRead         func(task, role, path string)
	VerifyReadRoot        func(ctx context.Context, name string) (string, error)
	FinalizeBranch        func(ctx context.Context, r *engine.Result) error
	CleanupBranch         func()
}

// TaskScope is a task's current scope: the files a turn may read in full
// and a change phase may edit (approved scope-widen files included),
// read-only input files (FAE-RT-017), and the RED test file once the task
// has one. The trusted executor resolves it from pinned configuration or
// the task's own admission and evidence.
type TaskScope struct {
	Files         []string
	ReadOnlyFiles []string
	TestFile      string
}

// ScopeRefusal refuses a dispatch whose task scope cannot hold it: an unsafe
// declared path, or a RED identity the task's evidence does not name exactly.
// Nothing has launched: the dispatch is refused before launch
// (engine.ClassRefusedBeforeLaunch), charging nothing.
type ScopeRefusal struct{ Detail string }

func (e *ScopeRefusal) Error() string { return "task scope refused: " + e.Detail }

// taskScopeBlock is the task_scope brief block: files sorted, deduplicated
// and never null.
func taskScopeBlock(s TaskScope) map[string]any {
	files := append([]string{}, s.Files...)
	sort.Strings(files)
	block := map[string]any{"kind": "task_scope", "files": slices.Compact(files), "test_file": s.TestFile}
	if len(s.ReadOnlyFiles) > 0 {
		ro := append([]string{}, s.ReadOnlyFiles...)
		sort.Strings(ro)
		block["read_only_files"] = slices.Compact(ro)
	}
	return block
}

// ResidueError is a Tree measurement proving the task branch holds work the
// task did not record (for example a commit made outside the VCS broker).
type ResidueError struct{ Detail string }

func (e *ResidueError) Error() string { return "stale branch residue: " + e.Detail }

// OwnHunkLossError is a CheckChange refusal: remediation dropped own hunks.
type OwnHunkLossError struct{ Detail string }

func (e *OwnHunkLossError) Error() string { return "own-hunk-loss: " + e.Detail }

type Admission interface {
	Acquire(context.Context, engine.Dispatch, Binding, engine.Session) (func() error, error)
}

// AdmissionLifecycle persists launch/retirement boundaries when supported.
type AdmissionLifecycle interface {
	Launching(context.Context, string) error
	Retired(context.Context, string, bool) error
}
type Executor struct {
	// StallSafeReplay asserts the restricted-effects contract for every turn. It
	// is no longer needed by default: a turn whose effective profile passes
	// RestrictedEffects carries that proof itself. Unchanged tree alone is
	// insufficient proof either way.
	StallSafeReplay bool
	Observe         func(context.Context, engine.Dispatch, engine.Session, providerhealth.Outcome) error
	ObserveDetail   func(context.Context, engine.Dispatch, engine.Session, providerhealth.ObservationDetail) error
	CheckChange     func(context.Context, engine.Dispatch, Binding) error
	Runtime         interface {
		Run(context.Context, runtime.Launch) (runtime.Collected, error)
	}
	Resolve   func(context.Context, engine.Dispatch) (Binding, error)
	Tree      func(context.Context, Binding) (string, error)
	Admission Admission
	Effects   engine.Executor
	// Feedback returns the failed host-command runs among a dispatch's input
	// artifacts with their stored failure excerpts (the engine_feedback
	// block). Nil or no runs omits the block; an error fails the dispatch
	// before launch.
	Feedback func(context.Context, engine.Dispatch, workflow.Document) ([]FeedbackRun, error)
	// CheckOutput, when set, checks a change turn's schema-valid output
	// against the task's trusted state after CheckChange passes; a refusal
	// is handled exactly as a CheckChange refusal.
	CheckOutput func(context.Context, engine.Dispatch, Binding, json.RawMessage) error
	// LaterSessionAdmissionWait is how long a later session of a review
	// dispatch keeps asking for admission after a deferral, retrying every
	// LaterSessionAdmissionRetry, before the dispatch gives up with a typed
	// deferral: giving up discards the earlier reviewers' completed turns,
	// which the stage's next dispatch runs again (FAE-WF-033), so a later
	// session waits for the host where a first session would park. Zero
	// selects the defaults (DefaultLaterSessionAdmissionWait, 5 s).
	LaterSessionAdmissionWait, LaterSessionAdmissionRetry time.Duration
	// InlineFileBytes and InlineTurnBytes configure inline brief size bounds (FAE-RT-017).
	InlineFileBytes, InlineTurnBytes int
	// CheckpointIntervalSec configures periodic turn checkpointing in seconds (FAE-RT-019).
	CheckpointIntervalSec int
	// SaveCheckpoint and LoadCheckpoint persist and load durable agentic turn
	// checkpoints in the controlplane store (FAE-RT-019).
	SaveCheckpoint func(context.Context, engine.Dispatch, engine.TurnCheckpoint) error
	LoadCheckpoint func(context.Context, engine.Dispatch) (*engine.TurnCheckpoint, error)
}

// DefaultLaterSessionAdmissionWait is the default
// Executor.LaterSessionAdmissionWait: the daemon's own admission bound, so a
// later session waits for the host as long as a first session's admission
// may before it is refused.
const DefaultLaterSessionAdmissionWait = 30 * time.Minute

// acquireSession admits one session of a dispatch. A later session whose
// admission is deferred asks again, within x.LaterSessionAdmissionWait, so
// the earlier sessions' turns are not spent for nothing while the host is
// busy; a first session's deferral, like any other admission failure, is
// returned at once. deferred reports whether the error returned is a typed
// admission deferral.
func (x *Executor) acquireSession(ctx context.Context, d engine.Dispatch, binding Binding, session engine.Session, later bool) (release func() error, deferred bool, err error) {
	wait, retry := x.LaterSessionAdmissionWait, x.LaterSessionAdmissionRetry
	if wait <= 0 {
		wait = DefaultLaterSessionAdmissionWait
	}
	if retry <= 0 {
		retry = 5 * time.Second
	}
	deadline := time.Now().Add(wait)
	for {
		release, err = x.Admission.Acquire(ctx, d, binding, session)
		var refusal interface{ AdmissionDeferred() bool }
		deferred = err != nil && errors.As(err, &refusal) && refusal.AdmissionDeferred()
		if err == nil || !deferred || !later || ctx.Err() != nil || !time.Now().Add(retry).Before(deadline) {
			return release, deferred, err
		}
		select {
		case <-ctx.Done():
			return nil, deferred, err
		case <-time.After(retry):
		}
	}
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
	// Until the first session's admission nothing of the dispatch has
	// launched, so any failure is a refusal before launch: the engine charges
	// no attempt for it and bounds its repeats by SameGate (FAE-RT-004). A
	// scope refusal, an unresolvable binding or tree, unreadable feedback
	// and a role with no route or brief all end here. A class set below
	// (stale residue) keeps its meaning, and a cancellation is no refusal.
	beforeLaunch := true
	defer func() {
		if err != nil && beforeLaunch && r.Class == "" && ctx.Err() == nil {
			r.Class, r.Detail = engine.ClassRefusedBeforeLaunch, err.Error()
		}
	}()
	if x.Runtime == nil || x.Resolve == nil || x.Tree == nil || x.Admission == nil {
		return r, errors.New("live runtime, binding, tree and durable admission required")
	}
	binding, e := x.Resolve(ctx, d)
	if e != nil {
		return r, e
	}
	if binding.CleanupBranch != nil {
		defer binding.CleanupBranch()
	}
	for _, ro := range d.Config.ReadOnlyFiles {
		if !slices.Contains(binding.Scope.ReadOnlyFiles, ro) {
			binding.Scope.ReadOnlyFiles = append(binding.Scope.ReadOnlyFiles, ro)
		}
	}
	revision, e := workflow.Hash(binding.Document)
	if e != nil {
		return r, e
	}
	resumeCP := d.ResumeCheckpoint
	if resumeCP == nil && x.LoadCheckpoint != nil {
		resumeCP, _ = x.LoadCheckpoint(ctx, d)
	}
	if resumeCP == nil && len(d.Sessions) > 0 && d.Sessions[0].ResumeSessionID != "" {
		resumeCP = &engine.TurnCheckpoint{
			TaskID:          d.TaskID,
			StageID:         d.Stage,
			ProviderSession: d.Sessions[0].ResumeSessionID,
			WorktreeTree:    d.Tree,
		}
	}
	before, e := x.Tree(ctx, binding)
	var residue *ResidueError
	if errors.As(e, &residue) {
		r.Class, r.Detail = engine.ClassStaleResidue, e.Error()
	}
	if e != nil {
		return r, e
	}
	if before != d.Tree && (resumeCP == nil || resumeCP.ProviderSession == "") {
		// Nothing has launched; the worktree holds work the task never recorded.
		r.Class, r.Detail = engine.ClassStaleResidue, "task tree changed before agent dispatch"
		return r, errors.New(r.Detail)
	}
	// A rework turn sees why the runs that routed it back failed.
	var feedback []FeedbackRun
	if x.Feedback != nil {
		if feedback, e = x.Feedback(ctx, d, binding.Document); e != nil {
			return r, e
		}
	}
	plans, e := planSessionLaunchesWithContext(ctx, d, binding, feedback)
	if e != nil {
		return r, e
	}
	beforeLaunch = false
	for sessionIndex, plan := range plans {
		session, role, route, brief := plan.session, plan.role, plan.session.Route, plan.brief
		release, deferred, e := x.acquireSession(ctx, d, binding, session, sessionIndex > 0)
		if e != nil {
			// Only a typed no-launch infrastructure refusal is retryable,
			// charging nothing (FAE-HOST-006): the first session's, and a
			// later review session's, which carries the reviews of the
			// sessions that ran before it (FAE-RT-004) once it waited its
			// bound for admission. Any other admission failure stays
			// uncertain.
			r.Class = "uncertain"
			if deferred {
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
				if releaseErr != nil && r.Class != engine.ClassRefusedBeforeLaunch {
					r.Class = "uncertain"
				}
				err = errors.Join(err, releaseErr)
			}()
			lifecycle, _ := x.Admission.(AdmissionLifecycle)
			if lifecycle != nil {
				if e := lifecycle.Launching(ctx, session.ID); e != nil {
					// The launch boundary could not be recorded, so nothing
					// launched: refused before launch, charging nothing
					// (FAE-RT-004).
					if ctx.Err() == nil {
						r.Class, r.Detail = engine.ClassRefusedBeforeLaunch, e.Error()
					}
					return e
				}
			}
			started := time.Now().UTC()
			profile := workflow.EffectiveProfile(role.Profile, d.Config)
			writeScopes, scopeErr := BoundWriteScopes(role, d, binding)
			if scopeErr != nil {
				return scopeErr
			}
			scratchScopes := append(append(append([]string{}, binding.Scratch...), role.Scratch...), d.Config.Scratch...)

			editableScope := slices.Clone(binding.Scope.Files)
			if d.Stage == "implement" && binding.Scope.TestFile != "" {
				editableScope = slices.DeleteFunc(editableScope, func(p string) bool { return p == binding.Scope.TestFile })
			}
			var startTree string
			if role.Session == "agentic" && binding.Worktree != "" {
				startTree, _ = provider.SnapshotWorktreeTree(ctx, binding.Worktree)
				if resumeCP != nil && resumeCP.WorktreeTree != "" {
					if startTree != "" && d.Tree != "" && startTree != d.Tree {
						// FAE-RT-019: keep in-scope edits made after the checkpoint and revert any out-of-scope edits back to d.Tree.
						_, _ = provider.VerifyAndRevertScopedWorktree(ctx, binding.Worktree, d.Tree, editableScope, binding.Scope.ReadOnlyFiles, workflow.ProducesChanges(binding.Document, d.Config), writeScopes, scratchScopes, binding.ArtifactsOnly)
						startTree = d.Tree
					} else {
						_ = provider.RestoreCheckpointPreservingScratch(ctx, binding.Worktree, resumeCP.WorktreeTree, scratchScopes)
						if d.Tree != "" {
							startTree = d.Tree
						}
					}
				}
			}
			inlineFileBytes := x.InlineFileBytes
			if binding.InlineFileBytes > 0 {
				inlineFileBytes = binding.InlineFileBytes
			}
			inlineTurnBytes := x.InlineTurnBytes
			if binding.InlineTurnBytes > 0 {
				inlineTurnBytes = binding.InlineTurnBytes
			}
			checkpointInterval := x.CheckpointIntervalSec
			if binding.CheckpointIntervalSec > 0 {
				checkpointInterval = binding.CheckpointIntervalSec
			}
			var wallSeconds, maxTurns int
			if d.TurnBudget != nil {
				wallSeconds, maxTurns = d.TurnBudget.WallSeconds, d.TurnBudget.MaxTurns
			}
			effectiveTurnBudget := workflow.EffectiveTurnBudget(role, d.Config)
			if d.TurnBudget != nil {
				if effectiveTurnBudget == nil {
					effectiveTurnBudget = &workflow.TurnBudgetSpec{OnExhaustion: "fail"}
				}
				if d.TurnBudget.WallSeconds > 0 && (effectiveTurnBudget.WallSeconds == 0 || d.TurnBudget.WallSeconds < effectiveTurnBudget.WallSeconds) {
					effectiveTurnBudget.WallSeconds = d.TurnBudget.WallSeconds
				}
				if d.TurnBudget.MaxTurns > 0 && (effectiveTurnBudget.MaxAssistantTurns == 0 || d.TurnBudget.MaxTurns < effectiveTurnBudget.MaxAssistantTurns) {
					effectiveTurnBudget.MaxAssistantTurns = d.TurnBudget.MaxTurns
				}
			}
			readOnlyFiles := plan.readOnlyFiles
			if readOnlyFiles == nil && binding.TaskReads == nil && len(binding.DenyPaths) == 0 {
				readOnlyFiles = binding.Scope.ReadOnlyFiles
			}
			rootPaths := []string{}
			for _, name := range role.ReadRoots {
				if root, ok := binding.ReadRootViews[name]; ok && root.Path != "" {
					rootPaths = append(rootPaths, root.Path)
				}
			}
			isolationRoots := map[string]provider.ReadRootView{}
			for name, path := range plan.readRoots {
				isolationRoots[name] = provider.ReadRootView{Path: path}
			}
			launchSpec := runtime.Launch{OutputOwnershipRegistry: binding.OutputOwnershipRegistry, ExcludedOutputDirs: slices.Clone(binding.ExcludedOutputDirs), SharedWriteRegistry: binding.SharedWriteRegistry, RuntimeMountIdentities: binding.RuntimeMountIdentities, RuntimeMounts: slices.Clone(binding.RuntimeMounts), CacheDirs: slices.Clone(binding.CacheDirs), ShellRuntimeMountsWithoutView: binding.ShellRuntimeMountsWithoutView, IsolationRequired: provider.RequiresSessionIsolation(runtime.AgenticShellAllowed(role.Profile), false, plan.reads, binding.DenyPaths, isolationRoots) && !(binding.ShellRuntimeMountsWithoutView && provider.RuntimeShellWithoutView(role.Profile, binding.RuntimeMounts)), Reads: plan.reads, DenyPaths: slices.Clone(binding.DenyPaths), ReadRootPaths: rootPaths, NativeReadDirs: slices.Clone(binding.AncestorOutputDirs),
				OutputDir: binding.OutputDir, StagingDir: binding.StagingDir, ArtifactsOnly: binding.ArtifactsOnly,
				StagingWrite:          role.Staging == "write" || d.Config.Staging == "write",
				Scratch:               append(append(append([]string{}, binding.Scratch...), role.Scratch...), d.Config.Scratch...),
				WriteScopes:           writeScopes,
				DeniedTestCommands:    binding.DeniedTestCommands,
				WorkflowID:            binding.Document.ID,
				WorkflowRevision:      revision,
				SessionPolicy:         role.Scope,
				BranchInstance:        d.BranchInstance,
				FreshContext:          d.Config.Primitive == workflow.Review || role.Fresh,
				FreshRole:             role.Fresh,
				WallSeconds:           wallSeconds,
				MaxTurns:              maxTurns,
				DispatchID:            d.ID,
				SessionID:             session.ID,
				TaskID:                d.TaskID,
				Role:                  session.Role,
				Worktree:              binding.Worktree,
				Mode:                  binding.Mode,
				Profile:               profile,
				Binding:               route,
				Brief:                 brief,
				Session:               role.Session,
				ChangeProducer:        workflow.ProducesChanges(binding.Document, d.Config) || binding.ArtifactsOnly,
				TurnBudget:            effectiveTurnBudget,
				ReadOnlyFiles:         readOnlyFiles,
				InlineFileBytes:       inlineFileBytes,
				InlineTurnBytes:       inlineTurnBytes,
				CheckpointIntervalSec: checkpointInterval,
			}
			if !role.Fresh {
				switch {
				case resumeCP != nil && resumeCP.ProviderSession != "":
					launchSpec.ResumeSession = resumeCP.ProviderSession
				case session.ResumeSessionID != "":
					launchSpec.ResumeSession = session.ResumeSessionID
				case binding.ResumeSession != "" && d.BranchInstance == "":
					launchSpec.ResumeSession = binding.ResumeSession
				}
				if role.Session == "agentic" && resumeCP != nil && resumeCP.WorktreeTree != "" {
					launchSpec.ResumeTree = resumeCP.WorktreeTree
				}
				if launchSpec.ResumeSession != "" && resumeCP != nil && resumeCP.ProviderSession != "" {
					r.ResumedFromCheckpoint = true
				}
			} else {
				launchSpec.ResumeSession = ""
			}
			if role.Session == "agentic" && x.SaveCheckpoint != nil {
				launchSpec.OnCheckpoint = func(cp runtime.TurnCheckpoint) {
					if cp.ProviderSession == "" && cp.WorktreeTree == "" {
						return
					}
					engCP := engine.TurnCheckpoint{
						TaskID:          d.TaskID,
						StageID:         d.Stage,
						Visit:           d.Visit,
						Attempt:         d.Attempt,
						ProviderSession: cp.ProviderSession,
						WorktreeTree:    cp.WorktreeTree,
						AssistantTurns:  cp.AssistantTurns,
						UpdatedAt:       time.Now().UTC(),
					}
					_ = x.SaveCheckpoint(context.WithoutCancel(ctx), d, engCP)
				}
			}
			runCtx := ctx
			var wallCancel context.CancelFunc
			var wallBudgetTimedOut bool
			if effectiveTurnBudget != nil && effectiveTurnBudget.WallSeconds > 0 {
				var wallCtx context.Context
				wallCtx, wallCancel = context.WithCancel(ctx)
				runCtx = wallCtx
				wallTimer := time.AfterFunc(time.Duration(effectiveTurnBudget.WallSeconds)*time.Second, func() {
					wallBudgetTimedOut = true
					wallCancel()
				})
				defer wallTimer.Stop()
				defer wallCancel()
			}
			launchSpec.OnToolEvent = func(event domain.ToolEvent) {
				if binding.OnToolEvent != nil {
					binding.OnToolEvent(d, event)
				}
			}
			runtimeStarted := time.Now().UTC()
			out, runErr := x.Runtime.Run(runCtx, launchSpec)
			r.RuntimeMounts = out.RuntimeMounts
			r.IsolationPolicy = out.IsolationPolicy
			r.ViewConstructionMS += out.ViewConstructionMS
			r.ViewCopiedBytes += out.ViewCopiedBytes
			stat := domain.NewTurnStat(out.ProviderStats, runtimeStarted, time.Now().UTC())
			stat.RuntimeMounts = out.RuntimeMounts
			stat.ViewConstructionMS = out.ViewConstructionMS
			stat.ViewCopiedBytes = out.ViewCopiedBytes
			stat.IsolationPolicy = out.IsolationPolicy
			stat.Task, stat.Stage, stat.FanStage, stat.Branch, stat.Role = d.TaskID, d.Stage, d.FanStage, d.BranchID, session.Role
			stat.Dispatch, stat.Session, stat.DispatchAttempt, stat.Visit = d.ID, session.ID, d.Attempt, d.Visit
			r.TurnStats = append(r.TurnStats, stat)
			if out.LogDirectory != "" {
				forwardBridgeRefusedReads(out.LogDirectory, binding.OnRefusedRead)
			}
			if wallBudgetTimedOut && ctx.Err() == nil {
				runErr = &runtime.BridgeRefusal{Class: runtime.TurnBudgetExhausted, Detail: "agentic turn_budget wall_seconds exhausted"}
			}
			if role.Session == "agentic" {
				cp, cpErr := runtime.ReadTurnCheckpoint(out.LogDirectory)
				if (cpErr != nil || cp == nil || cp.ProviderSession == "") && out.ProviderSession != "" {
					cp = &runtime.TurnCheckpoint{
						ProviderSession: out.ProviderSession,
						WorktreeTree:    out.CheckpointTree,
						AssistantTurns:  out.AssistantTurns,
					}
					cpErr = nil
				}
				if cpErr == nil && cp != nil && cp.ProviderSession != "" {
					engCP := engine.TurnCheckpoint{
						TaskID:          d.TaskID,
						StageID:         d.Stage,
						Visit:           d.Visit,
						Attempt:         d.Attempt,
						ProviderSession: cp.ProviderSession,
						WorktreeTree:    cp.WorktreeTree,
						AssistantTurns:  cp.AssistantTurns,
						UpdatedAt:       time.Now().UTC(),
					}
					r.Checkpoint = &engCP
					if x.SaveCheckpoint != nil {
						_ = x.SaveCheckpoint(context.WithoutCancel(ctx), d, engCP)
					}
				}
			}
			// schemaErr is a completed turn's refused report: the executor's
			// own schema check, or the trusted bridge's typed refusal of the
			// provider's answer before it wrote any edit (FAE-WF-034).
			// refusedLocally is the bridge's typed refusal before it launched
			// the provider (task scope, brief, role policy, a local step);
			// refusedBinding is its refusal of the session's binding as this
			// instance configures it (FAE-RT-010); notStarted is its report
			// that the provider never acknowledged the prompt (FAE-RT-004);
			// providerFailed is its report that the provider acknowledged the
			// prompt and ended without an answer, the bridge having written
			// nothing (FAE-WF-034); editsNotApplied is its report that the
			// provider answered and the bridge, failing to write the edits,
			// restored the worktree. launchRefused is the runtime's own
			// refusal before the bridge started (runtime.LaunchRefused, the
			// runtime's statement that no process started): Fae's failure,
			// not the provider's.
			var schemaErr, refusedLocally, refusedBinding, notStarted, providerFailed, editsNotApplied, launchRefused, budgetExhausted error
			var refusal *runtime.BridgeRefusal
			var turn *runtime.TurnError
			if role.Session == "agentic" && startTree != "" && binding.Worktree != "" {
				offending, revErr := provider.VerifyAndRevertScopedWorktree(context.WithoutCancel(ctx), binding.Worktree, startTree, editableScope, readOnlyFiles, workflow.ProducesChanges(binding.Document, d.Config), writeScopes, scratchScopes, binding.ArtifactsOnly)
				if runErr == nil && revErr == nil && len(offending) > 0 {
					_ = provider.RestoreCheckpointPreservingScratch(context.WithoutCancel(ctx), binding.Worktree, startTree, scratchScopes)
					refusal = &runtime.BridgeRefusal{
						Class:  runtime.RefusedOutput,
						Detail: fmt.Sprintf("out-of-scope worktree modifications reverted: %s", strings.Join(offending, ", ")),
					}
					runErr = refusal
				}
			}
			switch {
			case runErr == nil:
				schemaErr = workflow.ValidateArtifact(binding.Document.Artifacts[d.Config.Output].Schema, out.Output)
				runErr = schemaErr
			case errors.As(runErr, &turn) && turn.Class == runtime.LaunchRefused:
				launchRefused = turn
			case errors.As(runErr, &refusal) && runtime.IsTurnBudgetExhaustedClass(refusal.Class):
				budgetExhausted = refusal
			case errors.As(runErr, &refusal) && refusal.Class == runtime.RefusedOutput:
				schemaErr = refusal
			case errors.As(runErr, &refusal) && refusal.Class == runtime.RefusedBeforeProvider:
				refusedLocally = refusal
			case errors.As(runErr, &refusal) && refusal.Class == runtime.RefusedBinding:
				refusedBinding = refusal
			case errors.As(runErr, &refusal) && refusal.Class == runtime.ProviderNotStarted:
				notStarted = refusal
			case errors.As(runErr, &refusal) && refusal.Class == runtime.ProviderFailed:
				providerFailed = refusal
			case errors.As(runErr, &refusal) && refusal.Class == runtime.EditsNotApplied:
				editsNotApplied = refusal
			}
			_ = errors.As(runErr, &turn)
			observationFailed := false
			outcome := providerhealth.OK
			var detail providerhealth.ObservationDetail
			if !out.NonExecution && refusedLocally == nil && refusedBinding == nil && notStarted == nil && launchRefused == nil {
				stalled := turn != nil && turn.Class == "stalled"
				if budgetExhausted != nil {
					outcome = providerhealth.TurnBudgetExhausted
					detail = providerhealth.ObservationDetail{Outcome: outcome}
				} else {
					detail = providerhealth.ClassifyDetailed(out.LogDirectory, runErr == nil || editsNotApplied != nil, errors.Is(runErr, context.DeadlineExceeded) || stalled)
					outcome = detail.Outcome
				}
			}
			isQuotaCutoff := runErr != nil && outcome == providerhealth.RateLimited && !detail.ResetAt.IsZero()
			isCheckpointedAgenticCrash := role.Session == "agentic" && r.Checkpoint != nil && r.Checkpoint.ProviderSession != "" && !isQuotaCutoff && budgetExhausted == nil && schemaErr == nil && refusedLocally == nil && refusedBinding == nil && notStarted == nil && launchRefused == nil && editsNotApplied == nil && (providerFailed != nil || (turn != nil && (turn.Class == "process-failed" || turn.Class == "interrupted" || turn.Class == "missing-handoff")))
			if role.Session == "agentic" && startTree != "" && binding.Worktree != "" && runErr != nil {
				if budgetExhausted != nil || isQuotaCutoff || (r.Checkpoint != nil && r.Checkpoint.ProviderSession != "") {
					inScopeTree, _ := provider.SnapshotInScopeTree(context.WithoutCancel(ctx), binding.Worktree, startTree, editableScope)
					if inScopeTree == "" {
						inScopeTree = startTree
					}
					if r.Checkpoint == nil {
						sid := detail.SessionID
						if sid == "" && out.ProviderSession != "" {
							sid = out.ProviderSession
						}
						if sid == "" {
							if tr := Transcript(out.LogDirectory, started, out.ProducedAt); tr != nil && tr.ProviderSession != "" {
								sid = tr.ProviderSession
							}
						}
						if sid == "" && launchSpec.ResumeSession != "" {
							sid = launchSpec.ResumeSession
						}
						if sid == "" {
							sid = session.ID
						}
						engCP := engine.TurnCheckpoint{
							TaskID:          d.TaskID,
							StageID:         d.Stage,
							Visit:           d.Visit,
							Attempt:         d.Attempt,
							ProviderSession: sid,
							WorktreeTree:    inScopeTree,
							AssistantTurns:  out.AssistantTurns,
							UpdatedAt:       time.Now().UTC(),
						}
						r.Checkpoint = &engCP
					} else if inScopeTree != "" {
						r.Checkpoint.WorktreeTree = inScopeTree
						r.Checkpoint.UpdatedAt = time.Now().UTC()
					}
					if x.SaveCheckpoint != nil && r.Checkpoint != nil {
						_ = x.SaveCheckpoint(context.WithoutCancel(ctx), d, *r.Checkpoint)
					}
				}
				_ = provider.RestoreCheckpointPreservingScratch(context.WithoutCancel(ctx), binding.Worktree, startTree, scratchScopes)
			}
			// A proven pre-provider failure is a transport failure, not evidence
			// of unhealthy routing. Charging it could suppress the bounded retry.
			// A bridge refusal before the provider ran is not the provider's
			// either (FAE-RT-009), nor is a provider start that never
			// acknowledged the prompt: an exhausted start bound charges the
			// binding instead (FAE-RT-004). A provider whose answer the bridge
			// could not write answered: it is observed as answering, so a
			// healthy one is recorded ok (the failed write is the bridge's).
			// The runtime's own launch refusal charges no provider either.
			if (x.Observe != nil || x.ObserveDetail != nil) && !out.NonExecution && refusedLocally == nil && refusedBinding == nil && notStarted == nil && launchRefused == nil && !isCheckpointedAgenticCrash {
				var obsErr error
				if x.ObserveDetail != nil {
					obsErr = x.ObserveDetail(ctx, d, session, detail)
				} else if x.Observe != nil {
					obsErr = x.Observe(ctx, d, session, outcome)
				}
				if obsErr != nil {
					observationFailed = true
					runErr = errors.Join(runErr, obsErr)
				}
			}
			if lifecycle != nil {
				if e := lifecycle.Retired(ctx, session.ID, out.ProcessesRetired); e != nil {
					if (launchRefused != nil || refusedLocally != nil) && out.ProcessesRetired {
						r.Class = "uncertain"
						cause := launchRefused
						if cause == nil {
							cause = refusedLocally
						}
						rejected(ctx, x, binding, before, engine.ClassRefusedBeforeLaunch, cause, &r)
					}
					return errors.Join(runErr, e)
				}
			}
			if runErr != nil {
				r.Class = "uncertain"
				// The bridge's report that its provider never acknowledged
				// the prompt is a non-execution proof like the transport's:
				// a first session's turn is retried within the start bound.
				// The runtime's own launch refusal is no start of the
				// provider at all: it is refused before launch (below), so
				// it counts toward no binding's start bound.
				notExecuted := (out.NonExecution || notStarted != nil) && !observationFailed && launchRefused == nil
				var deferral interface{ AdmissionDeferred() bool }
				deferred := errors.As(runErr, &deferral) && deferral.AdmissionDeferred()
				if kernel.CanRetryStartup(notExecuted, out.ProcessesRetired, sessionIndex == 0) {
					after, treeErr := x.Tree(ctx, binding)
					if treeErr == nil && after == before {
						r.Class = "startup-retry"
						if notStarted != nil {
							r.Detail = notStarted.Error()
						}
						if deferred {
							r.Class = "infrastructure-deferred"
						}
					}
				} else if notExecuted && out.ProcessesRetired && sessionIndex > 0 && d.Config.Primitive == workflow.Review {
					// FAE-RT-004, FAE-WF-033, FAE-HOST-006: a later review
					// session that provably never started, after earlier
					// sessions ran, is a start failure of that session (or,
					// deferred by the runtime's admission, a deferral),
					// carrying the reviews that ran. kernel.CanRetryStartup
					// decides only a dispatch's first session; this decision
					// adds the engine's check that the reviews are the
					// earlier sessions'. With the tree unchanged the engine
					// charges nothing, counts the start toward that
					// session's binding's start bound and runs every
					// reviewer again in fresh sessions.
					after, treeErr := x.Tree(ctx, binding)
					if treeErr == nil && after == before {
						r.Class, r.Detail = engine.ClassLaterReviewerNotStarted, runErr.Error()
						if notStarted != nil {
							r.Detail = notStarted.Error()
						}
						if deferred {
							r.Class = "infrastructure-deferred"
						}
					}
				}
				if errors.As(runErr, &turn) && turn.Class == "stalled" {
					after, treeErr := x.Tree(ctx, binding)
					restricted := x.StallSafeReplay || RestrictedEffects(profile.Allowed)
					if kernel.CanRetryStall(restricted && !observationFailed, out.ProcessesRetired, treeErr == nil && after == before, sessionIndex == 0) {
						r.Class = "stall-retry"
					}
				}
				if r.Class == "uncertain" && isCheckpointedAgenticCrash && out.ProcessesRetired {
					if after, treeErr := x.Tree(ctx, binding); treeErr == nil && after == before {
						r.Class, r.Detail = engine.ClassCheckpointInterrupted, runErr.Error()
					}
				}
				if r.Class == "uncertain" && runErr != nil && !observationFailed && out.ProcessesRetired && outcome == providerhealth.RateLimited && !detail.ResetAt.IsZero() {
					// FAE-RT-018, FAE-RT-019: an in-flight turn cut off by a usage
					// limit with reset time T keeps its in-scope worktree changes and
					// provider session ID, parks ProviderQuota at zero charge, and is
					// re-dispatched first at T + grace.
					if after, treeErr := x.Tree(ctx, binding); treeErr == nil {
						canKeep := after == before
						if after != before && workflow.ProducesChanges(binding.Document, d.Config) && (d.Config.Primitive == workflow.AgentTurn || d.Config.Primitive == workflow.Review) {
							if x.CheckChange == nil || x.CheckChange(ctx, d, binding) == nil {
								canKeep = true
								r.NewTree = after
							}
						}
						if canKeep {
							r.Class = engine.ClassQuotaCutoff
							r.Detail = string(outcome) + ": " + runErr.Error()
							r.QuotaResetAt = detail.ResetAt
							r.InterruptedSession = detail.SessionID
							if r.InterruptedSession == "" && r.Checkpoint != nil && r.Checkpoint.ProviderSession != "" {
								r.InterruptedSession = r.Checkpoint.ProviderSession
							}
							if r.InterruptedSession == "" {
								r.InterruptedSession = session.ID
							}
						}
					}
				}
				if r.Class == "uncertain" && runErr != nil && !observationFailed && out.ProcessesRetired && (outcome == providerhealth.RateLimited || outcome == providerhealth.Unavailable || outcome == providerhealth.AuthFailure) {
					// FAE-RT-010: the recorded binding failure made it ineligible;
					// an untouched tree lets the turn rotate to the next route.
					if after, treeErr := x.Tree(ctx, binding); treeErr == nil && after == before {
						r.Class, r.Detail = engine.ClassProviderFailover, string(outcome)+": "+runErr.Error()
					}
				}
				if r.Class == "uncertain" && schemaErr != nil && !observationFailed && out.ProcessesRetired {
					// The turn completed: its malformed report is recorded health
					// and a bounded stage retry, unless it left unrecorded edits.
					rejected(ctx, x, binding, before, engine.ClassMalformedOutput, schemaErr, &r)
				}
				if r.Class == "uncertain" && providerFailed != nil && !observationFailed && out.ProcessesRetired && RestrictedEffects(profile.Allowed) {
					// FAE-WF-034: the provider ended without an answer and the
					// bridge wrote nothing; the provider had no tool that
					// writes, and the profile grants no effect beyond edits
					// through the bridge. Its health is recorded above. With
					// the tree unchanged after retirement the turn is malformed
					// output, a bounded stage retry. A moved or unmeasurable
					// tree keeps it uncertain: edits may have landed.
					if after, treeErr := x.Tree(ctx, binding); treeErr == nil && after == before {
						r.Class, r.Detail = engine.ClassMalformedOutput, providerFailed.Error()
					}
				}
				if r.Class == "uncertain" && refusedLocally != nil && out.ProcessesRetired {
					// Like a daemon-side scope refusal, it is refused before
					// launch: no provider ran, so it charges no attempt and no
					// provider, and SameGate bounds its repeats.
					rejected(ctx, x, binding, before, engine.ClassRefusedBeforeLaunch, refusedLocally, &r)
				}
				if r.Class == "uncertain" && launchRefused != nil && out.ProcessesRetired {
					// The runtime refused the launch before the bridge
					// started (Fae's own session directory, configuration,
					// shim, brief or pipe): no process ran, so, with the tree
					// unchanged, it is refused before launch, charging no
					// attempt and no binding (FAE-RT-004, FAE-RT-009); a later
					// review session's carries the reviews that ran.
					rejected(ctx, x, binding, before, engine.ClassRefusedBeforeLaunch, launchRefused, &r)
				}
				if r.Class == "uncertain" && editsNotApplied != nil && !observationFailed && out.ProcessesRetired {
					// The provider answered; the failed write was the
					// bridge's, which put the worktree back. With the tree
					// unchanged it fails as ValidationFailed, a bounded stage
					// retry that charges the attempt the provider ran in.
					rejected(ctx, x, binding, before, engine.ClassValidationFailed, editsNotApplied, &r)
				}
				if r.Class == "uncertain" && refusedBinding != nil && out.ProcessesRetired {
					// No retry of this binding can succeed until its
					// configuration changes: the engine records it and moves
					// the turn to the next route, charging nothing.
					if rejected(ctx, x, binding, before, engine.ClassBindingRefused, refusedBinding, &r); r.Class == engine.ClassBindingRefused {
						refused := route
						r.RefusedRoute = &refused
					}
				}
				if r.Class == "uncertain" && budgetExhausted != nil && !observationFailed && out.ProcessesRetired {
					rejected(ctx, x, binding, before, engine.ClassTurnBudgetExhausted, budgetExhausted, &r)
				}
				if refusal != nil && (r.Class == engine.ClassMalformedOutput || r.Class == engine.ClassValidationFailed || r.Class == engine.ClassBindingRefused || r.Class == engine.ClassRefusedBeforeLaunch || r.Class == engine.ClassTurnBudgetExhausted) {
					// The bridge's own detail, without the per-dispatch
					// handoff path, is the failure's reason, so a repeated
					// refusal counts toward the same-reason bound (SameGate).
					return refusal
				}
				return runErr
			}
			after, e := x.Tree(ctx, binding)
			if e != nil {
				r.Class = "uncertain"
				if errors.As(e, &residue) && out.ProcessesRetired {
					r.Class, r.Detail = engine.ClassRejectedChange, e.Error()
				}
				return e
			}
			if binding.ArtifactsOnly || !workflow.ProducesChanges(binding.Document, d.Config) {
				if after != before {
					r.Class = "uncertain"
					e := errors.New("read-only role modified task tree")
					if out.ProcessesRetired {
						r.Class, r.Detail = engine.ClassRejectedChange, e.Error()
					}
					return e
				}
			} else {
				if x.CheckChange == nil {
					r.Class = "uncertain"
					return errors.New("live change validation required")
				}
				if e = x.CheckChange(ctx, d, binding); e == nil && x.CheckOutput != nil {
					e = x.CheckOutput(ctx, d, binding, out.Output)
				}
				if e != nil {
					r.Class = "uncertain"
					if out.ProcessesRetired {
						rejected(ctx, x, binding, before, engine.ClassValidationFailed, e, &r)
					}
					return e
				}
				r.NewTree = after
				if binding.FinalizeBranch != nil {
					if e = binding.FinalizeBranch(ctx, &r); e != nil {
						r.Class = "uncertain"
						if out.ProcessesRetired {
							rejected(ctx, x, binding, before, engine.ClassValidationFailed, e, &r)
						}
						return e
					}
				}
			}
			r.ProducedAt = out.ProducedAt
			transcript := Transcript(out.LogDirectory, started, out.ProducedAt)
			if d.Config.Primitive == workflow.Review {
				r.Reviews = append(r.Reviews, engine.ReviewResult{Session: session.ID, Role: session.Role, Output: out.Output, Transcript: transcript, ReadRoots: plan.readRoots, Reads: plan.reads})
			} else {
				r.Output, r.Transcript, r.ReadRoots, r.Reads = out.Output, transcript, plan.readRoots, plan.reads
			}
			return nil
		}()
		if sessionErr != nil {
			switch r.Class {
			case "stall-retry", "startup-retry", "infrastructure-deferred", engine.ClassMalformedOutput, engine.ClassValidationFailed, engine.ClassRejectedChange, engine.ClassOwnHunkLoss, engine.ClassProviderFailover, engine.ClassQuotaCutoff, engine.ClassBindingRefused, engine.ClassLaterReviewerNotStarted, engine.ClassRefusedBeforeLaunch, engine.ClassTurnBudgetExhausted, engine.ClassCheckpointInterrupted:
			default:
				r.Class = "uncertain"
			}
			return r, sessionErr
		}

	}
	return r, nil
}

// sessionLaunchPlan is one session of a dispatch ready to launch: the
// session with the route it runs, its role and its brief.
type sessionLaunchPlan struct {
	session       engine.Session
	role          workflow.Role
	brief         []byte
	readRoots     map[string]string
	reads         *workflow.ReadPolicy
	readOnlyFiles []string
}

// planSessionLaunches checks every session's role and route and builds its
// brief before the dispatch's first launch, so a role with no route or brief
// template refuses the dispatch before any session runs, rather than after
// earlier review sessions ran. No brief depends on another session's output
// (FAE-WF-033).
func planSessionLaunches(d engine.Dispatch, binding Binding, feedback []FeedbackRun) ([]sessionLaunchPlan, error) {
	return planSessionLaunchesWithContext(context.Background(), d, binding, feedback)
}

func planSessionLaunchesWithContext(ctx context.Context, d engine.Dispatch, binding Binding, feedback []FeedbackRun) ([]sessionLaunchPlan, error) {
	plans := make([]sessionLaunchPlan, 0, len(d.Sessions))
	for _, session := range d.Sessions {
		role, ok := workflow.EffectiveRole(binding.Document, d.TaskClass, session.Role)
		if !ok || len(role.Routes) == 0 {
			return nil, fmt.Errorf("role %q has no route", session.Role)
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
			return nil, errors.New("session route not declared")
		}
		template := d.Config.Brief
		if template == "" {
			template = role.Briefs[d.Config.Phase]
		}
		if template == "" {
			template = role.Briefs["default"]
		}
		if template == "" && len(role.Briefs) > 0 {
			// Declared briefs that no phase selects are a content defect
			// (FAE-WF-043); never launch with an empty stage template.
			return nil, fmt.Errorf("role %q has no brief template for phase %q", session.Role, d.Config.Phase)
		}
		var sessionReadRoots map[string]string
		if len(role.ReadRoots) > 0 {
			sessionReadRoots = make(map[string]string, len(role.ReadRoots))
			for _, rootName := range role.ReadRoots {
				pinned := binding.ReadRoots[rootName]
				if binding.VerifyReadRoot != nil {
					current, err := binding.VerifyReadRoot(ctx, rootName)
					if err != nil {
						if !strings.Contains(err.Error(), "read-root-changed:") {
							return nil, fmt.Errorf("read-root-changed: %s (%w)", rootName, err)
						}
						return nil, err
					}
					if pinned != "" && current != "" && current != pinned {
						return nil, fmt.Errorf("read-root-changed: %s", rootName)
					}
					if current != "" {
						pinned = current
					}
				}
				if pinned != "" {
					sessionReadRoots[rootName] = pinned
				}
			}
			if len(sessionReadRoots) == 0 {
				sessionReadRoots = nil
			}
		}
		effectiveReads := workflow.EffectiveRoleStageReadPolicy(binding.Document, d.TaskClass, session.Role, d.Stage, binding.TaskReads, binding.DenyPaths)
		boundReads, readErr := boundReadPolicy(effectiveReads, d, binding)
		if readErr != nil {
			return nil, readErr
		}
		effectiveReads = boundReads

		inputs, err := filterSessionInputs(d, binding, session.Role, effectiveReads)
		if err != nil {
			return nil, err
		}
		sessionScope := filterSessionScope(d.TaskID, session.Role, binding.Scope, effectiveReads, binding.DenyPaths, binding.OnRefusedRead)
		tsBlock := taskScopeBlock(sessionScope)
		if binding.OutputDir != "" {
			tsBlock["output_dir"] = binding.OutputDir
			tsBlock["artifacts_only"] = binding.ArtifactsOnly
		}
		if binding.StagingDir != "" {
			tsBlock["staging_dir"] = binding.StagingDir
			tsBlock["staging_write"] = role.Staging == "write" || d.Config.Staging == "write"
		}
		if len(binding.AncestorOutputDirs) > 0 {
			tsBlock["ancestor_output_dirs"] = binding.AncestorOutputDirs
		}

		if effectiveReads != nil {
			tsBlock["reads"] = effectiveReads
		}
		if len(binding.DenyPaths) > 0 {
			tsBlock["deny_paths"] = binding.DenyPaths
		}
		if len(sessionReadRoots) > 0 && len(binding.ReadRootViews) > 0 {
			views := make(map[string]provider.ReadRootView, len(sessionReadRoots))
			for rootName, rev := range sessionReadRoots {
				if v, ok := binding.ReadRootViews[rootName]; ok {
					if v.Name == "" {
						v.Name = rootName
					}
					if v.Commit == "" {
						v.Commit = rev
					}
					if v.Digest == "" {
						v.Digest = rev
					}
					views[rootName] = v
				}
			}
			if len(views) > 0 {
				tsBlock["read_roots"] = views
			}
		}
		notifications := d.Notifications
		if len(notifications) == 0 {
			notifications = binding.Notifications
		}
		renderedTemplate, renderErr := renderStageTemplate(template, d, binding)
		if renderErr != nil {
			return nil, renderErr
		}
		// Ordered blocks retain typed artifact data and place the kernel contract last.
		brief, e := json.Marshal(briefBlocks(
			map[string]any{"kind": "role_instructions", "text": role.Instructions},
			map[string]any{"kind": "stage", "template": renderedTemplate, "task_id": d.TaskID, "stage": d.Stage, "inputs": inputs, "approved_scope": d.Scope},
			tsBlock,
			engineFeedbackBlock(feedback),
			priorRejectionBlock(d.PriorRejection),
			notificationsBlock(notifications),
			map[string]any{"kind": "operator_context", "text": binding.OperatorContext},
			map[string]any{"kind": "kernel_contract", "output_schema": binding.Document.Artifacts[d.Config.Output].Schema, "dispatch_id": d.ID, "session_id": session.ID, "task_id": d.TaskID, "frozen_artifacts": frozenRule(binding.Document), "obligations": "Return exactly one structured handoff for this dispatch; the trusted bridge writes it to the absolute handoff_path it adds to this contract. No external effects beyond the granted profile.", "gates": obligations(binding.Document, d.Stage)},
		))
		if e != nil {
			return nil, e
		}
		plans = append(plans, sessionLaunchPlan{session: session, role: role, brief: brief, readRoots: sessionReadRoots, reads: effectiveReads, readOnlyFiles: sessionScope.ReadOnlyFiles})
	}
	return plans, nil
}

func filterSessionInputs(d engine.Dispatch, binding Binding, roleName string, consumerReads *workflow.ReadPolicy) ([]workflow.Evidence, error) {
	if consumerReads == nil || len(d.Inputs) == 0 {
		return d.Inputs, nil
	}
	kept := make([]workflow.Evidence, 0, len(d.Inputs))
	for _, in := range d.Inputs {
		var producerReads *workflow.ReadPolicy
		foundProv := false
		for i := len(binding.Provenance) - 1; i >= 0; i-- {
			p := binding.Provenance[i]
			if (p.ArtifactID == in.ID || p.ArtifactID == "") && p.Stage == in.Stage {
				producerReads = p.Reads
				foundProv = true
				break
			}
		}
		if !foundProv && in.Stage != "" {
			prodStage := binding.Document.Stages[in.Stage]
			producerReads = workflow.EffectiveRoleStageReadPolicy(binding.Document, d.TaskClass, prodStage.Role, in.Stage, binding.TaskReads, binding.DenyPaths)
		}
		ok, deniedPath := workflow.CanBindStageReadPolicy(producerReads, consumerReads)
		if !ok {
			auditPath := deniedPath
			if auditPath == "" || auditPath == "*" {
				auditPath = "stage:" + in.Stage
			}
			if binding.OnRefusedRead != nil {
				binding.OnRefusedRead(d.TaskID, roleName, auditPath)
			}
			optional := false
			for _, decl := range d.Config.Inputs {
				if decl.Type == in.Type && (decl.Stage == "" || decl.Stage == in.Stage) && decl.Optional {
					optional = true
				}
			}
			if !optional {
				return nil, fmt.Errorf("read-policy refused bound input %s from stage %s for role %s (denied %s)", in.ID, in.Stage, roleName, auditPath)
			}
			continue
		}
		kept = append(kept, in)
	}
	return kept, nil
}

func filterSessionScope(taskID, roleName string, scope TaskScope, reads *workflow.ReadPolicy, denyPaths []string, onRefused func(task, role, path string)) TaskScope {
	if reads == nil && len(denyPaths) == 0 {
		return scope
	}
	var kept []string
	for _, f := range scope.Files {
		if !workflow.AllowsPath(reads, denyPaths, f) {
			if onRefused != nil {
				onRefused(taskID, roleName, f)
			}
			continue
		}
		kept = append(kept, f)
	}
	var keptRO []string
	for _, f := range scope.ReadOnlyFiles {
		if !workflow.AllowsPath(reads, denyPaths, f) {
			if onRefused != nil {
				onRefused(taskID, roleName, f)
			}
			continue
		}
		keptRO = append(keptRO, f)
	}
	testFile := scope.TestFile
	if testFile != "" && !workflow.AllowsPath(reads, denyPaths, testFile) {
		if onRefused != nil {
			onRefused(taskID, roleName, testFile)
		}
		testFile = ""
	}
	return TaskScope{Files: kept, ReadOnlyFiles: keptRO, TestFile: testFile}
}

func forwardBridgeRefusedReads(logDir string, onRefused func(task, role, path string)) {
	if logDir == "" || onRefused == nil {
		return
	}
	var paths []string
	_ = filepath.WalkDir(logDir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() && d.Name() == "refused-reads.jsonl" {
			paths = append(paths, p)
		}
		return nil
	})
	seen := map[string]bool{}
	for _, p := range paths {
		if seen[p] {
			continue
		}
		seen[p] = true
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		for _, line := range slices.Compact(splitLines(string(raw))) {
			var ev struct {
				Task string `json:"task"`
				Role string `json:"role"`
				Path string `json:"path"`
			}
			if json.Unmarshal([]byte(line), &ev) == nil && ev.Path != "" {
				onRefused(ev.Task, ev.Role, ev.Path)
			}
		}
	}
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			if line := s[start:i]; line != "" {
				out = append(out, line)
			}
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// rejected classes a refused report: retryable when the tree is unchanged,
// otherwise its unrecorded edits make a retry unsafe. Unknown stays uncertain.
func rejected(ctx context.Context, x *Executor, binding Binding, before, retryable string, cause error, r *engine.Result) {
	var loss *OwnHunkLossError
	if errors.As(cause, &loss) {
		r.Class, r.Detail = engine.ClassOwnHunkLoss, cause.Error()
		return
	}
	after, e := x.Tree(ctx, binding)
	switch {
	case e == nil && after == before:
		r.Class = retryable
	case e == nil:
		r.Class = engine.ClassRejectedChange
	default:
		var residue *ResidueError
		if !errors.As(e, &residue) {
			return
		}
		r.Class = engine.ClassRejectedChange
	}
	r.Detail = cause.Error()
}

// Transcript is the FAE-OBS-007 correlation reference of one turn: the bridge
// log directory (the provider's own stream) and, for a persistent context, the
// provider thread the bridge recorded. Missing records leave fields empty.
func Transcript(logs string, started, finished time.Time) *engine.Transcript {
	if logs == "" {
		return nil
	}
	t := &engine.Transcript{LogDirectory: logs, StartedAt: started, FinishedAt: finished}
	paths, _ := filepath.Glob(filepath.Join(logs, "provider-*", "provider-session.json"))
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		var audit struct {
			ThreadID  string `json:"thread_id"`
			Resumable *bool  `json:"resumable"`
		}
		if err == nil && json.Unmarshal(raw, &audit) == nil && audit.ThreadID != "" && (audit.Resumable == nil || *audit.Resumable) {
			t.ProviderSession = audit.ThreadID
		}
	}
	return t
}

func priorRejectionBlock(messages []string) any {
	if len(messages) == 0 {
		return nil
	}
	return map[string]any{"kind": "prior_rejection", "value": messages}
}
