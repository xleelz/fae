package daemon

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/effects"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/execution"
	"github.com/xleelz/fae/external"
	"github.com/xleelz/fae/kernel"
	"github.com/xleelz/fae/processowner"
	"github.com/xleelz/fae/providerhealth"
	agentruntime "github.com/xleelz/fae/runtime"
	"github.com/xleelz/fae/workflow"
	"github.com/xleelz/fae/worktree"
)

// LiveConfig is an operator-installed trust boundary, pinned for an instance.
// Command names a trusted fae-agent-v1 bridge that enforces provider tool profiles.
type LiveConfig struct {
	TicketWorkflow            *TicketWorkflowConfig            `json:"ticket_workflow,omitempty"`
	ReviewPolicies            map[string]external.ReviewPolicy `json:"review_policies,omitempty"`
	DisableReviewPublishMerge bool                             `json:"disable_review_publish_merge,omitempty"`
	ExternalTools             map[string]external.Tool         `json:"external_tools,omitempty"`
	ResidentSessions          bool                             `json:"resident_sessions,omitempty"`
	PersistentLoops           bool                             `json:"persistent_loops,omitempty"`
	StartSeconds              int                              `json:"start_seconds,omitempty"`
	StallSeconds              int                              `json:"stall_seconds,omitempty"`
	StallSafeReplay           bool                             `json:"stall_safe_replay,omitempty"`
	PaneLimit                 int                              `json:"pane_limit,omitempty"`
	Tasks                     map[string]LiveTaskConfig        `json:"tasks,omitempty"`
	Adapter                   string                           `json:"adapter,omitempty"`
	HerdrBinary               string                           `json:"herdr_binary,omitempty"`
	Command                   []string                         `json:"command"`
	Environment               map[string]string                `json:"environment,omitempty"`
	AllowedCommands           [][]string                       `json:"allowed_commands"`
	Files                     []string                         `json:"files"`
	Base                      string                           `json:"base"`
	IntegrationRef            string                           `json:"integration_ref"`
	CommitMessage             string                           `json:"commit_message"`
	TestFile                  string                           `json:"test_file"`
	TestID                    string                           `json:"test_id"`
	ExpectedFailure           string                           `json:"expected_failure"`
	HostLedger                string                           `json:"host_ledger"`
	HeavyCapacity             int                              `json:"heavy_capacity"`
	TimeoutSeconds            int                              `json:"timeout_seconds"`
}
type liveExecutor struct {
	resident *agentruntime.ResidentMultiplexer
	runner   interface {
		Run(context.Context, agentruntime.Launch) (agentruntime.Collected, error)
	}
	daemon  *Daemon
	config  LiveConfig
	manager *worktree.Manager
	broker  *effects.Broker
	guard   *liveAdmission
}

func (d *Daemon) setupLive(ctx context.Context) error {
	c := *d.config.Live
	d.ticketDiscovery = &external.Discovery{}
	if err := validateTicketConfig(c); err != nil {
		return err
	}
	if c.TicketWorkflow != nil {
		d.ticketDiscovery = &external.Discovery{TTL: time.Duration(c.TicketWorkflow.RefreshSeconds) * time.Second}
	}
	if err := ValidateExternalSchemas(ctx, c); err != nil {
		return err
	}
	for _, tool := range c.ExternalTools {
		if e := tool.Validate(); e != nil {
			return e
		}
	}
	if c.ResidentSessions && c.Adapter != "multiplexer" {
		return errors.New("resident sessions require Herdr multiplexer")
	}
	if c.StartSeconds < 0 {
		return errors.New("start_seconds must be nonnegative")
	}
	if c.StallSeconds < 0 {
		return errors.New("stall_seconds must be nonnegative")
	}
	if err := validateLiveTasks(c.Tasks); err != nil {
		return err
	}
	if len(c.Command) == 0 || !filepath.IsAbs(c.Command[0]) || c.Base == "" || c.TimeoutSeconds < 1 || c.HeavyCapacity < 1 || !filepath.IsAbs(c.HostLedger) || len(c.Files) == 0 {
		return errors.New("live config requires absolute bridge and shared host ledger, base, files, positive timeout/capacity")
	}
	if decoded, e := hex.DecodeString(c.Base); e != nil || (len(decoded) != 20 && len(decoded) != 32) {
		return errors.New("live base must be an immutable full commit SHA")
	}
	placeholder := false
	for _, arg := range c.Command {
		if arg == "{config}" {
			placeholder = true
		}
	}
	if !placeholder {
		return errors.New("bridge command must contain exact {config} argument")
	}
	if !strings.HasPrefix(c.IntegrationRef, "refs/heads/fae/") {
		return errors.New("live integration requires a dedicated refs/heads/fae/ target")
	}
	raw, _ := json.Marshal(c)
	hash, err := domain.ContentHash(raw)
	if err != nil {
		return err
	}
	prior, e := d.store.GetMarker(ctx, "live-config")
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return e
	}
	if len(prior) > 0 && string(prior) != `"`+hash+`"` {
		return errors.New("live configuration differs from pinned instance configuration")
	}
	if err = d.store.PutMarker(ctx, controlplane.Audit{Actor: "daemon", Action: "pin live configuration", Outcome: "ok"}, "live-config", json.RawMessage(`"`+hash+`"`)); err != nil {
		return err
	}
	x := &liveExecutor{daemon: d, config: c, manager: &worktree.Manager{Repository: d.config.Workspace, Root: d.layout.Worktrees}}
	effectsRegistry := filepath.Join(d.layout.Run, "effect-processes")
	var recoveredRecords []processowner.Record
	for _, owned := range [][2]string{{filepath.Join(d.layout.Root, "sessions", ".processes"), filepath.Join(d.layout.Root, "sessions")}, {effectsRegistry, d.layout.Worktrees}} {
		records, e := processowner.Recover(ctx, owned[0], owned[1])
		if e != nil {
			return e
		}
		recoveredRecords = append(recoveredRecords, records...)
		for _, record := range records {
			raw, _ := json.Marshal(record)
			key := fmt.Sprintf("recovered-process-%d-%s", record.PID, record.Birth)
			if e = d.store.PutMarker(ctx, controlplane.Audit{Actor: "daemon", Action: "owned process recovered", Outcome: "retired"}, key, raw); e != nil {
				return e
			}
		}
	}
	if err = agentruntime.RecoverResidentSockets(filepath.Join(d.layout.Root, "sessions"), recoveredRecords); err != nil {
		return err
	}
	x.broker = &effects.Broker{Manager: x.manager, Allowed: c.AllowedCommands, ProcessRegistry: effectsRegistry, ProcessOwner: d.layout.Worktrees}
	x.guard, err = newLiveAdmission(d, c)
	if err != nil {
		return err
	}
	d.live = x // Publish acquired guards so startup failure cleanup releases registrations.
	// Exact orphan termination was audited above. Pane Snapshot and lease
	// recovery have now durably acknowledged old owners; idle lifetime receipts
	// have no remaining admission purpose.
	for _, record := range recoveredRecords {
		if strings.HasPrefix(record.Session, "resident-") && processowner.ProvedDead(record.ParentPID, record.ParentBirth) {
			if err = processowner.AcknowledgeLinkedRetirement(filepath.Dir(record.Path), record.Owner, record.Session, record.ParentPID, record.ParentBirth); err != nil {
				return err
			}
		}
	}
	if err = d.refreshRuntimeConfig(context.Background()); err != nil {
		return err
	}
	if err = mkdirPrivate(filepath.Join(d.layout.Root, "sessions")); err != nil {
		return err
	}
	direct := &agentruntime.Direct{Command: c.Command, Root: filepath.Join(d.layout.Root, "sessions"), Environment: c.Environment, Timeout: time.Duration(c.TimeoutSeconds) * time.Second, StallTimeout: time.Duration(c.StallSeconds) * time.Second, StartTimeout: time.Duration(c.StartSeconds) * time.Second}
	var runner interface {
		Run(context.Context, agentruntime.Launch) (agentruntime.Collected, error)
	} = direct
	if c.Adapter == "multiplexer" {
		runner = &agentruntime.Multiplexer{Direct: direct, Binary: c.HerdrBinary, Instance: d.config.Instance, BeforeStart: x.guard.BindPaneSocket}
	} else if c.Adapter != "" && c.Adapter != "direct" {
		return errors.New("unsupported live adapter")
	}
	d.live = x
	if c.Adapter == "multiplexer" {
		d.idleSessions, err = agentruntime.NewOwnedIdleSessions(filepath.Join(d.layout.Root, "idle-context"), direct.Root)
		if err != nil {
			return err
		}
		if d.config.ConfigureIdleSessions != nil {
			if err = d.config.ConfigureIdleSessions(ctx, d.idleSessions, direct.Root); err != nil {
				return err
			}
		}
	} else if d.config.ConfigureIdleSessions != nil {
		return errors.New("idle pane adapter requires Herdr")
	}
	if c.ResidentSessions {
		x.resident = &agentruntime.ResidentMultiplexer{Direct: direct, Binary: c.HerdrBinary, Instance: d.config.Instance, Idle: d.idleSessions, BeforeStart: x.guard.ReserveResidentPane, AfterStop: x.guard.ReleaseResidentPane, OneShotBeforeStart: x.guard.BindPaneSocket, Classify: classifyResident}
		runner = x.resident
	}
	x.runner = runner
	d.executor = &execution.Executor{StallSafeReplay: c.StallSafeReplay, Runtime: runner, Resolve: x.resolve, Tree: func(ctx context.Context, b execution.Binding) (string, error) {
		cmd := exec.CommandContext(ctx, "git", "rev-parse", "HEAD")
		cmd.Dir = b.Worktree
		cmd.Env = cleanGitEnv()
		out, e := cmd.Output()
		if e != nil {
			return "", e
		}
		if strings.TrimSpace(string(out)) != b.ExecutionHead {
			return "", errors.New("agent changed task branch HEAD outside VCS broker")
		}
		return x.broker.Tree(ctx, x.bindingPath(b.Worktree))
	}, Admission: x.guard, Effects: x, CheckChange: x.checkChange, Observe: func(ctx context.Context, dispatch engine.Dispatch, session engine.Session, outcome providerhealth.Outcome) error {
		d.mu.Lock()
		defer d.mu.Unlock()
		route := session.Route
		err := d.store.ObserveProvider(ctx, controlplane.ProviderObservation{ID: session.ID, TaskID: dispatch.TaskID, Provider: route.Provider, Model: route.Model, Outcome: outcome, At: time.Now().UTC()})
		if err == nil {
			d.notifyEvents()
		}
		return err
	}}
	return nil
}
func (x *liveExecutor) bindingPath(path string) worktree.Binding {
	return worktree.Binding{Path: path, Branch: "fae/task/" + filepath.Base(path)}
}
func (x *liveExecutor) resolve(ctx context.Context, dispatch engine.Dispatch) (execution.Binding, error) {
	x.daemon.mu.Lock()
	snapshot, ok := x.daemon.jobs[dispatch.TaskID]
	x.daemon.mu.Unlock()
	if !ok {
		return execution.Binding{}, errors.New("unknown live task")
	}
	var e envelope
	if err := decodeEnvelope(snapshot.Payload, &e); err != nil {
		return execution.Binding{}, err
	}
	if err := x.daemon.checkPinnedTarget(ctx, e.Admission, e.Job.Document); err != nil {
		return execution.Binding{}, err
	}
	w, err := x.manager.Ensure(ctx, e.Admission.RunID, dispatch.TaskID, x.config.Base)
	if err != nil {
		return execution.Binding{}, err
	}
	if dispatch.ExecutionHead == "" {
		dispatch.ExecutionHead = e.Job.ExpectedHead()
	}
	return execution.Binding{ExecutionHead: dispatch.ExecutionHead, Base: e.Job.Base, Worktree: w.Path, Document: e.Job.Document, OperatorContext: e.Admission.Content + dispatch.OperatorContext, Mode: strings.ToLower(e.Admission.Mode)}, nil
}
func (x *liveExecutor) testSource(b execution.Binding) ([]byte, error) {
	path := filepath.Join(b.Worktree, x.config.TestFile)
	rel, err := filepath.Rel(b.Worktree, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") || x.config.TestFile == "" {
		return nil, errors.New("invalid pinned test file")
	}
	physical, err := filepath.EvalSymlinks(path)
	if err != nil || physical != path {
		return nil, errors.New("test file must be physical")
	}
	return os.ReadFile(path)
}
func (x *liveExecutor) checkChange(ctx context.Context, d engine.Dispatch, b execution.Binding) error {
	x = x.forTask(d.TaskID)
	w := x.bindingPath(b.Worktree)
	snapshot, err := x.broker.Snapshot(ctx, w, x.config.Files)
	if err != nil {
		return err
	}
	if d.Config.Phase == "test-first" {
		cmd := exec.CommandContext(ctx, "git", "diff", "--name-only", d.Tree, snapshot.Tree)
		cmd.Dir = b.Worktree
		cmd.Env = cleanGitEnv()
		out, err := cmd.Output()
		if err != nil {
			return err
		}
		for _, name := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if name != "" && name != x.config.TestFile {
				return errors.New("RED turn changed a non-test file")
			}
		}
	}
	if d.Config.Phase == "implement" {
		source, err := x.testSource(b)
		if err != nil {
			return err
		}
		identity, err := workflow.GoTestIdentity(source, []string{x.config.TestID})
		if err != nil {
			return err
		}
		found := false
		for _, a := range d.Inputs {
			if a.Type == "FrozenIdentity" {
				cmd := exec.CommandContext(ctx, "git", "show", a.Tree+":"+x.config.TestFile)
				cmd.Dir = b.Worktree
				cmd.Env = cleanGitEnv()
				frozenBytes, e := cmd.Output()
				if e != nil {
					return e
				}
				if !bytes.Equal(frozenBytes, source) {
					return errors.New("implementation altered frozen RED file bytes")
				}
				found = true
				if a.Fields["identity"] != identity {
					return errors.New("implementation altered frozen RED identity")
				}
			}
		}
		if !found {
			return errors.New("implementation lacks frozen identity")
		}
	}
	if d.Config.Phase == "implement" {
		if err := x.checkConflictResolution(ctx, d, w); err != nil {
			return err
		}
	}
	return nil
}
func (x *liveExecutor) Execute(ctx context.Context, d engine.Dispatch) (r engine.Result, err error) {
	selected, selectErr := x.daemon.liveForTask(ctx, d.TaskID)
	if selectErr != nil {
		return r, selectErr
	}
	x = selected.forTask(d.TaskID)
	if len(d.Sessions) > 0 {
		copyX := *x
		copyBroker := *x.broker
		copyBroker.ProcessSession = d.Sessions[0].ID
		copyX.broker = &copyBroker
		x = &copyX
	}
	r = engine.Result{ExecutionKind: "live", DispatchID: d.ID, TaskID: d.TaskID, Tree: d.Tree, ProducedAt: time.Now().UTC()}
	if len(d.Sessions) == 0 {
		return r, errors.New("session required")
	}
	r.Session = d.Sessions[0].ID
	b, err := x.resolve(ctx, d)
	if err != nil {
		return r, err
	}
	w := x.bindingPath(b.Worktree)
	release, err := x.guard.Acquire(ctx, d, b, d.Sessions[0])
	if err != nil {
		var refusal interface{ AdmissionDeferred() bool }
		if errors.As(err, &refusal) && refusal.AdmissionDeferred() {
			r.Class = "infrastructure-deferred"
		}
		return r, err
	}
	launched := false
	startEffect := func() error {
		e := x.guard.Launching(ctx, d.Sessions[0].ID)
		if e == nil {
			launched = true
		}
		return e
	}
	if !supportedEffect(d.Config.Primitive) {
		if err = startEffect(); err != nil {
			_ = release()
			return r, err
		}
	}
	defer func() {
		if launched && !errors.Is(err, processowner.ErrRetirementUnproven) {
			err = errors.Join(err, x.guard.Retired(ctx, d.Sessions[0].ID, true))
		}
		if e := release(); e != nil {
			r.Class = "uncertain"
			err = e
		}
	}()
	before, err := x.broker.Tree(ctx, w)
	if err != nil {
		return r, err
	}
	if before != d.Tree {
		return r, errors.New("tree changed before effect")
	}
	if d.Config.Primitive == workflow.ExternalEffect && x.config.reviewOperation(d.TaskID, d.Config.Subject) == "review_create" && x.reviewIsLive(d.TaskID) {
		if _, healthErr := x.checkReviewTargetEvidence(ctx, d.TaskID); healthErr != nil {
			r.Class = "infrastructure-deferred"
			r.Detail = healthErr.Error()
			raw, _ := json.Marshal(map[string]string{"task": d.TaskID, "defect": healthErr.Error()})
			if recordErr := x.daemon.store.PutMarker(ctx, controlplane.Audit{Actor: "daemon", Action: "review target health", Outcome: "blocked", Inputs: raw}, "review-target/"+d.TaskID, raw); recordErr != nil {
				return r, recordErr
			}
			return r, healthErr
		}
	}
	// Once an effect starts, any failure is uncertain until explicitly reconciled.
	defer func() {
		r.ProducedAt = time.Now().UTC()
		if err != nil {
			r.Class = "uncertain"
		}
	}()
	var intent *effectIntent
	if d.Config.Primitive != workflow.VcsCommit {
		intent, err = x.effectIntent(ctx, d, w, nil, startEffect)
		if err != nil {
			return r, err
		}
	}
	var output any
	switch d.Config.Primitive {
	case workflow.ExternalEffect:
		output, err = x.executeExternal(ctx, d)
		if err != nil {
			var refusal *reviewRefusal
			if errors.As(err, &refusal) {
				r.Class = "review-gate-failed"
				if refusal.wait {
					r.Class = "review-wait"
				}
				r.Detail = err.Error()
				return r, nil
			}
			return r, err
		}
	case workflow.HostCommand:
		run, e := x.broker.Run(ctx, w, effects.Command{Argv: d.Config.Command, Timeout: time.Duration(d.Config.TimeoutSeconds) * time.Second, TestID: x.config.TestID, ExpectedFailure: x.config.ExpectedFailure})
		if e != nil {
			return r, e
		}
		if d.Config.TestRun {
			source, e := x.testSource(b)
			if e != nil {
				return r, e
			}
			_, e = workflow.GoTestIdentity(source, []string{x.config.TestID})
			output = map[string]any{"exit_code": run.ExitCode, "executed_count": run.ExecutedCount, "matched_count": run.MatchedCount, "expected_failure": run.ExpectedFailure, "test_in_source": e == nil, "red_passed": run.RedPassed, "fresh_build": run.FreshBuild, "output_digest": run.OutputDigest}
		} else {
			if len(d.Config.Command) != 3 || d.Config.Command[0] != "gofmt" || d.Config.Command[1] != "-l" || d.Config.Command[2] != "." {
				return r, errors.New("unsupported non-test command artifact")
			}
			debt := len(strings.Fields(run.Output))
			if run.ExitCode != 0 {
				debt++
			}
			output = map[string]any{"new_debt": debt}
		}
	case workflow.FreezeArtifact, workflow.CompareFrozen:
		source, e := x.testSource(b)
		if e != nil {
			return r, e
		}
		identity, e := workflow.GoTestIdentity(source, []string{x.config.TestID})
		if e != nil {
			return r, e
		}
		fields := map[string]any{"identity": identity}
		if d.Config.Primitive == workflow.CompareFrozen {
			frozen := ""
			for _, a := range d.Inputs {
				if a.Type == "FrozenIdentity" {
					frozen, _ = a.Fields["identity"].(string)
				}
			}
			if frozen == "" {
				return r, errors.New("missing frozen identity")
			}
			fields["unchanged"] = frozen == identity
		}
		output = fields
	case workflow.Merge:
		if e := x.verifyActiveConflict(ctx, d, w); e != nil {
			return r, e
		}
		snapshot, e := x.broker.Snapshot(ctx, w, x.config.Files)
		if e != nil {
			return r, e
		}
		plan, e := x.broker.PrepareReconcileFrom(ctx, w, snapshot, d.Base, x.config.IntegrationRef)
		if e != nil {
			var conflict *effects.ReconcileError
			if errors.As(e, &conflict) {
				if conflict.Kind == "conflicted" {
					return x.resolveConflict(ctx, d, w, plan, conflict, r)
				}
				raw, _ := json.Marshal(conflict)
				hash, _ := domain.ContentHash([]byte(fmt.Sprintf("%q", d.ID)))
				if recordErr := x.daemon.store.PutMarker(ctx, controlplane.Audit{Actor: "daemon", Action: "reconciliation refused", Outcome: conflict.Kind}, "reconcile-failure/"+hash, raw); recordErr != nil {
					return r, recordErr
				}
			}
			return r, e
		}
		if plan.Changed {
			active, e := x.activeConflict(ctx, d.TaskID)
			if e != nil {
				return r, e
			}
			if active != nil {
				return r, errors.New("integration advanced again after conflict; original own intent retained for operator review")
			}
			if d.Config.Reverify == "" {
				return r, errors.New("changed-base reconciliation requires workflow reverify route")
			}
			raw, _ := json.Marshal(plan)
			hash, _ := domain.ContentHash([]byte(fmt.Sprintf("%q", d.ID)))
			if e = x.daemon.store.PutMarker(ctx, controlplane.Audit{Actor: "daemon", Action: "record reconcile intent", Outcome: "prepared"}, "reconcile/"+hash, raw); e != nil {
				return r, e
			}
			if e = x.broker.ApplyReconcile(ctx, w, plan); e != nil {
				return r, e
			}
			r.NewTree = plan.Tree
			r.NewBase = plan.Base
		}
		output = map[string]any{"result": "clean", "patch_identical": plan.PatchIdentical || plan.AlreadyContained}
	case workflow.VcsCommit:
		if e := x.verifyActiveConflict(ctx, d, w); e != nil {
			return r, e
		}
		snapshot, e := x.broker.Snapshot(ctx, w, x.config.Files)
		if e != nil {
			return r, e
		}
		if snapshot.Tree != d.Tree {
			return r, errors.New("reviewed tree mismatch")
		}
		prepared, e := x.broker.PrepareCommit(ctx, w, snapshot, x.config.CommitMessage+"\n\nFae dispatch: "+d.ID)
		if e != nil {
			return r, e
		}
		intent, e = x.effectIntent(ctx, d, w, &prepared, startEffect)
		if e != nil {
			return r, e
		}
		commit, e := x.broker.ApplyCommit(ctx, w, prepared)
		if e != nil {
			return r, e
		}
		output = map[string]any{"sha": commit.SHA, "files": commit.Files}
	case workflow.Integrate:
		sha := ""
		for _, a := range d.Inputs {
			if a.Type == "Commit" {
				sha, _ = a.Fields["sha"].(string)
			}
		}
		if sha == "" {
			return r, errors.New("missing commit artifact")
		}
		land, e := x.broker.Integrate(ctx, w, effects.CommitEvidence{SHA: sha, Tree: d.Tree, Files: x.config.Files}, x.config.IntegrationRef, b.Base)
		if e != nil {
			var moved *effects.TargetMovedError
			if errors.As(e, &moved) && kernel.CanRetryIntegration(moved.NoWrite, moved.Actual != moved.Expected, moved.OwnCommitKnown, d.Config.Reverify != "") {
				r.Class = "target-moved"
				r.Detail = e.Error()
				return r, nil
			}
			return r, e
		}
		output = map[string]any{"landed": land.Landed, "sha": land.SHA}
	default:
		return r, fmt.Errorf("unsupported live primitive %s", d.Config.Primitive)
	}
	after, e := x.broker.Tree(ctx, w)
	if e != nil {
		return r, e
	}
	expectedTree := d.Tree
	if r.NewTree != "" {
		expectedTree = r.NewTree
	}
	if after != expectedTree {
		return r, errors.New("host/VCS effect changed verified tree")
	}
	r.Output, err = json.Marshal(output)
	if err == nil {
		r.ProducedAt = time.Now().UTC()
		err = x.recordEffectResult(ctx, intent, r)
	}
	return r, err
}
