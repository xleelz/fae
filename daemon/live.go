package daemon

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xleelz/fae/commandshim"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	"github.com/xleelz/fae/provider"
	"github.com/xleelz/fae/providerhealth"
	agentruntime "github.com/xleelz/fae/runtime"
	"github.com/xleelz/fae/scheduler"
	"github.com/xleelz/fae/workflow"
	"github.com/xleelz/fae/worktree"
)

// LiveConfig is an operator-installed trust boundary, pinned for an instance.
// Command names a trusted fae-agent-v1 bridge that enforces provider tool profiles.
// Non-empty Files pin every task without a Tasks override to Files and the
// instance RED identity; empty Files select derived scope, where each task's
// scope comes from its own admission and evidence (resolveLiveTaskScope).
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
	ProcessLimit              int                              `json:"process_limit,omitempty"` // FAE-HOST-015 tasks per brokered run; 0 default, -1 off
	// NestedCommands is "refuse" (default) or "account" (FAE-HOST-014);
	// MaxOutputBytes bounds brokered run output (0 is 4 MiB, else 1-64 MiB).
	NestedCommands string                         `json:"nested_commands,omitempty"`
	MaxOutputBytes int                            `json:"max_output_bytes,omitempty"`
	ProviderPools  []scheduler.ProviderPoolConfig `json:"provider_pools,omitempty"`
}
type liveExecutor struct {
	resident *agentruntime.ResidentMultiplexer
	runner   interface {
		Run(context.Context, agentruntime.Launch) (agentruntime.Collected, error)
	}
	daemon            *Daemon
	config            LiveConfig
	manager           *worktree.Manager
	broker            *effects.Broker
	guard             *liveAdmission
	queue             *integrationQueue
	recoveredLaunches map[string]bool
	recoveredReserved map[string]bool
}

func (d *Daemon) setupLive(ctx context.Context) error {
	if err := d.liveBuildWorkspace(); err != nil {
		return err
	}
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
	if err := errors.Join(liveCommandBoundsIssues(c)...); err != nil {
		return err
	}
	if err := validateLiveTasks(c.Tasks); err != nil {
		return err
	}
	if err := validateLiveScopeConfig(c); err != nil {
		return err
	}
	if c.HostLedger == "" {
		// FAE-HOST-031: an unset ledger is the stable host-scoped default.
		ledger, err := scheduler.DefaultHostLedger()
		if err != nil {
			return err
		}
		c.HostLedger = ledger
	}
	if len(c.Command) == 0 || !filepath.IsAbs(c.Command[0]) || c.Base == "" || c.TimeoutSeconds < 1 || c.HeavyCapacity < 1 || !filepath.IsAbs(c.HostLedger) {
		return errors.New("live config requires absolute bridge and shared host ledger, base, positive timeout/capacity")
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
	x := &liveExecutor{daemon: d, config: c, manager: &worktree.Manager{Repository: d.config.Workspace, Root: d.layout.Worktrees}, queue: &integrationQueue{}}
	effectsRegistry := filepath.Join(d.layout.Run, "effect-processes")
	var recoveredRecords []processowner.Record
	for _, owned := range [][2]string{{filepath.Join(d.layout.Root, "sessions", ".processes"), filepath.Join(d.layout.Root, "sessions")}, {effectsRegistry, d.layout.Worktrees}} {
		records, cleared, e := processowner.RecoverClearing(ctx, owned[0], owned[1])
		d.logClearedProcessRecords("host", cleared)
		if e = errors.Join(e, d.auditClearedProcessRecords(ctx, cleared)); e != nil {
			return fmt.Errorf("recover owned agent sessions and host commands: %w", e)
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
	x.broker = &effects.Broker{Manager: x.manager, Allowed: c.AllowedCommands, ProcessRegistry: effectsRegistry, ProcessOwner: d.layout.Worktrees, ProcessLimit: d.processLimit(ctx, c.ProcessLimit), NestedCommands: effects.NestedMode(c.NestedCommands), RecordNested: d.recordNestedCommands, MaxOutput: c.MaxOutputBytes}
	x.broker.NestedBroker = liveNestedBroker()
	x.broker.ScratchLeft = d.logScratchLeft
	if _, gap := x.broker.GoRootGap(); gap != "" {
		d.logLine("host", "%s\n", gap)
	}
	x.recoveredLaunches = map[string]bool{}
	x.recoveredReserved = map[string]bool{}
	for _, record := range recoveredRecords {
		x.recoveredLaunches[record.Session] = true
	}
	if j, e := scheduler.OpenJournal(filepath.Join(d.layout.Run, "lease-proofs")); e == nil {
		if proofs, e := j.Entries(); e == nil {
			for _, p := range proofs {
				if p.State == "reserved" {
					x.recoveredReserved[p.Session] = true
				} else {
					x.recoveredLaunches[p.Session] = true
				}
			}
		}
	}
	x.guard, err = newLiveAdmission(d, c)
	if err != nil {
		return err
	}
	d.live = x // Publish acquired guards so startup failure cleanup releases registrations.
	if len(c.ProviderPools) > 0 && x.guard.host != nil {
		if err = x.guard.host.ConfigureProviderPools(ctx, c.ProviderPools); err != nil {
			return err
		}
		if snaps, e := x.guard.host.ProviderPoolsSnapshot(ctx); e == nil {
			for _, snap := range snaps {
				x.guard.mirrorProviderPool(ctx, snap)
			}
		}
	}
	if err = d.sweepCommandScratch(x.guard.journal); err != nil {
		return err
	}
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
	if err = mkdirPrivate(d.hostAlertSpool()); err != nil {
		return err
	}
	direct.Shims = commandshim.RoleShims{Broker: commandshim.SelfBroker(), Ledger: c.HostLedger, AlertSpool: d.hostAlertSpool()}
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
		x.resident.ReconciliationDeferred = x.guard.residentReconciliationDeferred
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
			return "", &execution.ResidueError{Detail: "task branch HEAD moved outside the VCS broker"}
		}
		copyBroker := *x.broker
		copyBroker.Scratch = b.Scratch
		return copyBroker.Tree(ctx, x.bindingPath(b.Worktree))
	}, Admission: x.guard, Effects: x, CheckChange: x.checkChange, CheckOutput: x.checkRedReport, Observe: func(ctx context.Context, dispatch engine.Dispatch, session engine.Session, outcome providerhealth.Outcome) error {
		d.mu.Lock()
		defer d.mu.Unlock()
		route := session.Route
		return d.observeProvider(ctx, dispatch, controlplane.ProviderObservation{ID: session.ID, TaskID: dispatch.TaskID, Provider: route.Provider, Model: route.Model, Outcome: outcome, At: time.Now().UTC()})
	}, ObserveDetail: func(ctx context.Context, dispatch engine.Dispatch, session engine.Session, detail providerhealth.ObservationDetail) error {
		d.mu.Lock()
		defer d.mu.Unlock()
		route := session.Route
		cutOffTaskID := ""
		if detail.Outcome == providerhealth.RateLimited && !detail.ResetAt.IsZero() {
			cutOffTaskID = dispatch.TaskID
			if x.guard != nil && x.guard.host != nil {
				credID := x.guard.resolveCredentialIdentity(session)
				if poolSnap, matched, _ := x.guard.host.MatchProviderPool(ctx, route.Provider, credID); matched {
					_ = x.guard.host.SetProviderPoolQuota(ctx, scheduler.ProviderPoolQuota{
						PoolID:       poolSnap.Config.ID,
						ResetAt:      detail.ResetAt.UTC(),
						GraceSeconds: 60,
						CutOffTaskID: dispatch.TaskID,
						Actor:        "daemon",
						Reason:       "usage limit observed",
					})
				}
			}
		} else if detail.Outcome == providerhealth.OK && x.guard != nil && x.guard.host != nil {
			credID := x.guard.resolveCredentialIdentity(session)
			if poolSnap, matched, _ := x.guard.host.MatchProviderPool(ctx, route.Provider, credID); matched && poolSnap.Quota != nil && !poolSnap.Quota.Active(time.Now().UTC()) {
				_ = x.guard.host.ClearProviderPoolQuota(ctx, poolSnap.Config.ID, "daemon", "quota reset elapsed")
			}
		}
		return d.observeProvider(ctx, dispatch, controlplane.ProviderObservation{
			ID:           session.ID,
			TaskID:       dispatch.TaskID,
			Provider:     route.Provider,
			Model:        route.Model,
			Outcome:      detail.Outcome,
			At:           time.Now().UTC(),
			ResetAt:      detail.ResetAt,
			GraceSeconds: 60,
			CutOffTaskID: cutOffTaskID,
		})
	}, Feedback: x.engineFeedback,
		InlineFileBytes:       DefaultInlineFileBytes,
		InlineTurnBytes:       DefaultInlineTurnBytes,
		CheckpointIntervalSec: DefaultCheckpointIntervalSec,
		SaveCheckpoint: func(ctx context.Context, dispatch engine.Dispatch, cp engine.TurnCheckpoint) error {
			return d.store.SaveTurnCheckpoint(ctx, controlplane.TurnCheckpointRecord{
				TaskID:          cp.TaskID,
				StageID:         engine.CheckpointStorageStage(dispatch),
				Visit:           int64(cp.Visit),
				Attempt:         int64(cp.Attempt),
				ProviderSession: cp.ProviderSession,
				WorktreeTree:    cp.WorktreeTree,
				AssistantTurns:  int64(cp.AssistantTurns),
				UpdatedAt:       cp.UpdatedAt,
			})
		},
		LoadCheckpoint: func(ctx context.Context, dispatch engine.Dispatch) (*engine.TurnCheckpoint, error) {
			cp, ok, err := d.store.LatestTurnCheckpoint(ctx, dispatch.TaskID, engine.CheckpointStorageStage(dispatch))
			if err != nil || !ok {
				return nil, err
			}
			return &engine.TurnCheckpoint{
				TaskID:          cp.TaskID,
				StageID:         dispatch.Stage,
				Visit:           int(cp.Visit),
				Attempt:         int(cp.Attempt),
				ProviderSession: cp.ProviderSession,
				WorktreeTree:    cp.WorktreeTree,
				AssistantTurns:  int(cp.AssistantTurns),
				UpdatedAt:       cp.UpdatedAt,
			}, nil
		},
	}
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
	e = dispatchedEnvelope(e, dispatch)
	if err := x.daemon.checkPinnedTarget(ctx, e.Admission, e.Job.Document); err != nil {
		return execution.Binding{}, err
	}
	target, err := x.daemon.pinnedTarget(ctx, e.Admission)
	if err != nil {
		return execution.Binding{}, err
	}
	base := e.Job.Base
	if base == "" {
		base = x.config.Base
	}
	w, err := x.manager.Ensure(ctx, e.Admission.RunID, dispatch.TaskID, base)
	if err != nil {
		return execution.Binding{}, err
	}
	if dispatch.ExecutionHead == "" {
		dispatch.ExecutionHead = e.Job.ExpectedHead()
	}
	scope, err := x.resolveLiveTaskScope(dispatch.TaskID, dispatch.Scope, dispatch.Config, func() (envelope, error) { return e, nil })
	if err != nil {
		return execution.Binding{}, err
	}
	if err := applyStageWriteScopes(dispatch, e, &scope); err != nil {
		return execution.Binding{}, err
	}
	if err := checkScopeDenyPaths(scope.Files, scope.TestFile, e.Job.DenyPaths); err != nil {
		return execution.Binding{}, err
	}
	if err := checkScopeDenyPaths(scope.ReadOnlyFiles, "", e.Job.DenyPaths); err != nil {
		return execution.Binding{}, err
	}
	if (dispatch.Config.Primitive == workflow.AgentTurn || dispatch.Config.Primitive == workflow.Review) && dispatch.Config.ChangeProducer {
		// A change turn its scope cannot hold is refused before it launches.
		// A test-first turn needs no RED identity: it writes the RedReport,
		// so a broken latest one is what it replaces, never a reason to
		// refuse it (Appendix C step 7).
		if err = scope.needFiles(); err == nil && x.heldToFrozenRed(dispatch, e.Job.Document) {
			err = scope.needRed()
		}
		if err != nil {
			return execution.Binding{}, err
		}
	}
	var finalizeBranch func(context.Context, *engine.Result) error
	var cleanupBranch func()
	if dispatch.BranchID != "" && (workflow.ProducesChanges(e.Job.Document, dispatch.Config)) && (dispatch.Config.Primitive == workflow.AgentTurn || dispatch.Config.Primitive == workflow.Review || dispatch.Config.Primitive == workflow.HostCommand) && len(dispatch.WriteScope) > 0 && x.broker != nil {
		fanStage := dispatch.FanStage
		if fanStage == "" && e.Job.Fan != nil {
			fanStage = e.Job.Fan.Stage
		}
		if fanStage == "" {
			fanStage = "fan"
		}
		bw, forkErr := branchWorktree(ctx, x.broker, w, e.Job, dispatch.BranchID)
		if forkErr != nil {
			return execution.Binding{}, forkErr
		}
		w = bw.Binding
		dispatch.ExecutionHead = bw.BaseCommit
		cleanupBranch = func() {
			_ = x.broker.CleanupBranchWorktree(context.Background(), bw)
		}
		finalizeBranch = func(fCtx context.Context, r *engine.Result) error {
			msg := fmt.Sprintf("fae fan-branch %s/%s (%s)", fanStage, dispatch.BranchID, dispatch.TaskID)
			committed, cErr := x.broker.CommitBranchWorktree(fCtx, bw, scope.Files, msg)
			if cErr != nil {
				return cErr
			}
			r.BranchCommit = committed.CommitSHA
			r.BranchRef = committed.Ref
			r.BranchBaseTree = dispatch.Tree
			if committed.Tree != "" {
				r.NewTree = committed.Tree
			}
			return nil
		}
	}
	var bindingReadRoots map[string]string
	if len(e.Job.ReadRoots) > 0 {
		bindingReadRoots = make(map[string]string, len(e.Job.ReadRoots))
		for name, id := range e.Job.ReadRoots {
			bindingReadRoots[name] = id
		}
	} else if target != nil && len(target.ReadRoots) > 0 {
		bindingReadRoots = make(map[string]string, len(target.ReadRoots))
		for name, rc := range target.ReadRoots {
			id := rc.Commit
			if id == "" {
				id = rc.Digest
			}
			bindingReadRoots[name] = id
		}
	}
	var bindingReadRootViews map[string]provider.ReadRootView
	if target != nil && len(target.ReadRoots) > 0 {
		bindingReadRootViews = make(map[string]provider.ReadRootView, len(target.ReadRoots))
		for name, rc := range target.ReadRoots {
			id := ""
			if bindingReadRoots != nil {
				id = bindingReadRoots[name]
			}
			if id == "" {
				id = rc.Commit
			}
			if id == "" {
				id = rc.Digest
			}
			bindingReadRootViews[name] = provider.ReadRootView{
				Name:   name,
				Path:   rc.Path,
				Commit: id,
				Digest: id,
				Deny:   slices.Clone(rc.Deny),
			}
		}
	}
	resumeSession := ""
	if role, ok := workflow.EffectiveRole(e.Job.Document, e.Job.TaskClass, dispatch.Config.Role); ok && !role.Fresh && x.daemon != nil {
		if sess, found := x.daemon.sessionRegistry().GetTaskSession(dispatch.TaskID); found && sess.ThreadID != "" {
			resumeSession = sess.ThreadID
		}
	}
	verifyRoot := func(vCtx context.Context, name string) (string, error) {
		if target == nil {
			return "", fmt.Errorf("read-root-changed: %s", name)
		}
		rc, ok := target.ReadRoots[name]
		if !ok {
			return "", fmt.Errorf("read-root-changed: %s", name)
		}
		actual, err := VerifyReadRoot(vCtx, name, rc, target.DenyPaths)
		if err != nil {
			return actual, fmt.Errorf("read-root-changed: %s", name)
		}
		if pinned := e.Job.ReadRoots[name]; pinned != "" && actual != pinned {
			return actual, fmt.Errorf("read-root-changed: %s", name)
		}
		return actual, nil
	}
	onRefusedRead := func(task, role, path string) {
		if x.daemon != nil && x.daemon.store != nil {
			_ = x.daemon.store.RecordRefusedRead(ctx, task, role, path)
		}
	}
	// A turn retried because its RedReport was refused is told why.
	operator := acceptanceContext(e, dispatch) + dispatch.OperatorContext + liveRedRefusalNote(e.Job, dispatch.Stage)
	if len(dispatch.Notifications) > 0 && !strings.Contains(operator, "<notifications") {
		operator += "\n" + provider.FormatNotificationsQuoted(dispatch.Notifications)
	}
	targetCfg := DefaultTargetConfig()
	if target, tErr := x.daemon.pinnedTarget(ctx, e.Admission); tErr != nil {
		return execution.Binding{}, tErr
	} else if target != nil {
		targetCfg = *target
	}
	ancestorDirs, ancestorErr := x.ancestorOutputDirectories(ctx, e.Job)
	if ancestorErr != nil {
		return execution.Binding{}, ancestorErr
	}
	excludedDirs := []string{}
	// Stored legacy jobs without an output directory have no output-tree write
	// authority and cannot overlap descendant outputs.
	if e.Job.OutputDir != "" {
		var excludedErr error
		excludedDirs, excludedErr = x.daemon.descendantOutputDirectories(ctx, e.Job.TaskID)
		if excludedErr != nil {
			return execution.Binding{}, excludedErr
		}
	}

	return execution.Binding{OutputOwnershipRegistry: x.daemon.outputOwnershipPath(), SharedWriteRegistry: filepath.Join(x.daemon.layout.Run, "shared-write-grants.json"), RuntimeMountIdentities: targetCfg.RuntimeMountIdentities, RuntimeMounts: slices.Clone(targetCfg.RuntimeMounts), CacheDirs: slices.Clone(targetCfg.CacheDirs), ShellRuntimeMountsWithoutView: targetCfg.ShellRuntimeMountsWithoutView, AncestorOutputDirs: ancestorDirs, ExcludedOutputDirs: excludedDirs,
		TemplateJob: &e.Job, Scratch: taskScratch(e.Job.Document),
		OutputDir: e.Job.OutputDir, StagingDir: e.Job.StagingDir, ArtifactsOnly: e.Job.Document.Produces == "artifacts", TaskWrites: e.Admission.Writes, DeniedTestCommands: taskTestCommands(e.Job.Document, target),
		ExecutionHead:         dispatch.ExecutionHead,
		Base:                  e.Job.Base,
		Worktree:              w.Path,
		Document:              e.Job.Document,
		OperatorContext:       operator,
		Notifications:         dispatch.Notifications,
		Mode:                  strings.ToLower(e.Admission.Mode),
		Scope:                 scope.briefScope(),
		InlineFileBytes:       targetCfg.EffectiveInlineFileBytes(),
		InlineTurnBytes:       targetCfg.EffectiveInlineTurnBytes(),
		CheckpointIntervalSec: targetCfg.EffectiveCheckpointIntervalSec(),
		TaskReads:             e.Job.Reads,
		DenyPaths:             slices.Clone(e.Job.DenyPaths),
		ReadRoots:             bindingReadRoots,
		ReadRootViews:         bindingReadRootViews,
		ResumeSession:         resumeSession,
		Provenance:            slices.Clone(e.Job.Provenance),
		OnRefusedRead:         onRefusedRead,
		OnToolEvent: func(dispatch engine.Dispatch, event domain.ToolEvent) {
			if x.daemon != nil {
				x.daemon.recordToolEvent(dispatch, event)
			}
		},
		VerifyReadRoot: verifyRoot,
		FinalizeBranch: finalizeBranch,
		CleanupBranch:  cleanupBranch,
	}, nil
}
func (x *liveExecutor) ancestorOutputDirectories(ctx context.Context, job engine.Job) ([]string, error) {
	dirs := []string{}
	artifactRoot := filepath.Join(x.daemon.layout.Root, "artifacts")
	for _, id := range job.Ancestors {
		ancestor, err := x.taskEnvelope(id)
		if err != nil {
			snapshot, ok, lookupErr := x.daemon.lockedStoredJob(ctx, id)
			if lookupErr != nil {
				return nil, lookupErr
			}
			if !ok {
				return nil, fmt.Errorf("ancestor output task %s is unavailable", id)
			}
			if err = decodeEnvelope(snapshot.Payload, &ancestor); err != nil {
				return nil, err
			}
		}
		dir := ancestor.Job.OutputDir
		if dir == "" {
			continue
		}
		if !strings.HasPrefix(dir, artifactRoot+string(filepath.Separator)) {
			return nil, fmt.Errorf("ancestor output directory is outside instance artifacts")
		}
		if err = provider.PhysicalTaskDirectory(dir); err != nil {
			return nil, err
		}
		if !slices.Contains(dirs, dir) {
			dirs = append(dirs, dir)
		}
	}
	return dirs, nil
}

func (x *liveExecutor) testSource(b execution.Binding, testFile string) ([]byte, error) {
	path := filepath.Join(b.Worktree, testFile)
	rel, err := filepath.Rel(b.Worktree, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") || testFile == "" {
		return nil, errors.New("invalid RED test file")
	}
	physical, err := filepath.EvalSymlinks(path)
	if err != nil || physical != path {
		return nil, errors.New("test file must be physical")
	}
	return os.ReadFile(path)
}
func (x *liveExecutor) checkChange(ctx context.Context, d engine.Dispatch, b execution.Binding) error {
	scope, err := x.dispatchLiveScope(d)
	if err == nil {
		err = scope.needFiles()
	}
	if err != nil {
		return err
	}
	w := x.bindingPath(b.Worktree)
	files, err := scope.snapshotFiles(ctx, w)
	if err != nil {
		return err
	}
	snapshot, err := x.broker.Snapshot(ctx, w, files)
	if err != nil {
		return err
	}
	if len(d.WriteScope) > 0 {
		cmd := exec.CommandContext(ctx, "git", "diff", "--name-only", d.Tree, snapshot.Tree)
		cmd.Dir = b.Worktree
		cmd.Env = cleanGitEnv()
		out, err := cmd.Output()
		if err != nil {
			return err
		}
		for _, f := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			f = strings.TrimSpace(f)
			if f == "" {
				continue
			}
			if !workflow.MatchesAnyGlob(f, d.WriteScope) {
				return fmt.Errorf("fan-out branch %s modified %q outside branch write_scope %v", d.BranchID, f, d.WriteScope)
			}
		}
	}
	if d.Config.Phase == "test-first" {
		cmd := exec.CommandContext(ctx, "git", "diff", "--name-only", d.Tree, snapshot.Tree)
		cmd.Dir = b.Worktree
		cmd.Env = cleanGitEnv()
		out, err := cmd.Output()
		if err != nil {
			return err
		}
		// A broken latest RedReport leaves the scope without a RED file, so
		// the turn replacing it is held to the first-RED rule.
		if err = liveRedTurnChange(scope, strings.Split(strings.TrimSpace(string(out)), "\n")); err != nil {
			return err
		}
	}
	// Every change producer after the RED freeze is held to it, phase label or
	// not (FAE-WF-073). Token identity, not raw bytes, decides (FAE-WF-076).
	if x.heldToFrozenRed(d, b.Document) {
		if err = scope.needRed(); err != nil {
			return err
		}
		source, err := x.testSource(b, scope.TestFile)
		if err != nil {
			return err
		}
		identity, err := redIdentity(scope.TestFile, source, scope.TestID)
		if err != nil {
			return err
		}
		found := false
		for _, a := range d.Inputs {
			if a.Type == "FrozenIdentity" {
				cmd := exec.CommandContext(ctx, "git", "show", a.Tree+":"+scope.TestFile)
				cmd.Dir = b.Worktree
				cmd.Env = cleanGitEnv()
				frozenBytes, e := cmd.Output()
				if e != nil {
					return e
				}
				if e = checkFrozenRed(scope.TestFile, frozenBytes, source, scope.TestID); e != nil {
					return fmt.Errorf("implementation altered frozen RED file: %w", e)
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
			if ownHunkLoss(err) {
				return &execution.OwnHunkLossError{Detail: err.Error()}
			}
			return err
		}
	}
	return nil
}

// vcsRefusal types a commit refusal before any commit object or ref was
// written (the reviewed snapshot, its tree or the prepared commit): nothing
// ran, so the engine escalates it refunded (engine.ClassCommitRefusedBeforeWrite,
// FAE-RT-004). An interrupted attempt stays uncertain for restart recovery.
func vcsRefusal(ctx context.Context, r engine.Result, cause error) (engine.Result, error) {
	if ctx.Err() != nil {
		return r, cause
	}
	r.Class, r.Detail = engine.ClassCommitRefusedBeforeWrite, cause.Error()
	return r, nil
}
func ownHunkLoss(err error) bool {
	var loss *effects.ReconcileError
	return errors.As(err, &loss) && loss.Kind == "own-hunk-loss"
}

// refuseOwnHunkLoss types a no-write own-hunk refusal (MergeOwnHunkLoss) after
// remeasuring that nothing changed; anything else remains uncertain.
func (x *liveExecutor) refuseOwnHunkLoss(ctx context.Context, d engine.Dispatch, w worktree.Binding, intent *effectIntent, cause error, r engine.Result) (engine.Result, error) {
	after, err := x.broker.Tree(ctx, w)
	if err != nil {
		return r, err
	}
	if after != d.Tree {
		return r, errors.Join(cause, errors.New("tree changed during refused reconciliation"))
	}
	r.Class, r.Detail, r.ProducedAt = engine.ClassOwnHunkLoss, cause.Error(), time.Now().UTC()
	return r, x.recordEffectResult(ctx, intent, r)
}
func (x *liveExecutor) Execute(ctx context.Context, d engine.Dispatch) (r engine.Result, err error) {
	r = engine.Result{ExecutionKind: "live", DispatchID: d.ID, TaskID: d.TaskID, Tree: d.Tree, ProducedAt: time.Now().UTC()}
	if len(d.Sessions) == 0 {
		return r, errors.New("session required")
	}
	r.Session = d.Sessions[0].ID
	// Until the effect starts, a failure is a refusal before launch: no test
	// run, freeze, compare, merge, commit or call ran, so the engine charges
	// neither an attempt nor a test run, and SameGate bounds its repeats
	// (FAE-RT-004, FAE-WF-074). A scope that cannot hold the dispatch, a task
	// or worktree that can't be resolved and a refused command policy end
	// here. A cancellation is no refusal.
	refuseBeforeLaunch := func(e error) (engine.Result, error) {
		if ctx.Err() == nil {
			r.Class, r.Detail = engine.ClassRefusedBeforeLaunch, e.Error()
		}
		return r, e
	}
	selected, selectErr := x.daemon.liveForTask(ctx, d.TaskID)
	if selectErr != nil {
		return refuseBeforeLaunch(selectErr)
	}
	x = selected
	copyX := *x
	copyBroker := *x.broker
	copyBroker.ProcessSession = d.Sessions[0].ID
	if task, err := x.taskEnvelope(d.TaskID); err == nil {
		copyBroker.Scratch = taskScratch(task.Job.Document)
	}
	copyX.broker = &copyBroker
	x = &copyX
	// The task's scope is resolved once, before anything starts.
	scope, err := x.dispatchLiveScope(d)
	if err == nil {
		err = scope.effectCheck(d)
	}
	if err != nil {
		return refuseBeforeLaunch(err)
	}
	b, err := x.resolve(ctx, d)
	if err != nil {
		return refuseBeforeLaunch(err)
	}
	if b.CleanupBranch != nil {
		defer b.CleanupBranch()
	}
	policy, err := x.policyFor(ctx, d)
	if err != nil {
		return refuseBeforeLaunch(err)
	}
	r.ReadRoots = policy.readRoots
	r.Reads = workflow.EffectiveRoleStageReadPolicy(b.Document, policy.task.Job.TaskClass, d.Config.Role, d.Stage, b.TaskReads, b.DenyPaths)
	w := x.bindingPath(b.Worktree)
	if d.Config.Primitive == workflow.Merge {
		// Integration is a serialised queue (FAE-VCS-008): wait, uncharged.
		if err = x.integrationTurn(d.TaskID, d.ID); err != nil {
			r.Class = "infrastructure-deferred"
			return r, err
		}
	}
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
			// The launch boundary could not be recorded: nothing ran.
			_ = release()
			return refuseBeforeLaunch(err)
		}
		if err = x.daemon.store.RecordEffect(ctx, effectKey(d)+"/started", d.TaskID, "started", json.RawMessage(`true`)); err != nil {
			_ = release()
			return refuseBeforeLaunch(err)
		}
	}
	defer func() {
		if launched && !errors.Is(err, processowner.ErrRetirementUnproven) {
			err = errors.Join(err, x.guard.Retired(ctx, d.Sessions[0].ID, true))
		}
		if e := release(); e != nil {
			if r.Class != engine.ClassRefusedBeforeLaunch {
				r.Class = "uncertain"
			}
			err = errors.Join(err, e)
		}
	}()
	before, err := x.broker.Tree(ctx, w)
	if err != nil {
		// A tree that can't be measured ran nothing, launched or not.
		return refuseBeforeLaunch(err)
	}
	if before != d.Tree {
		// Nothing started: the worktree holds work the task never recorded.
		r.Class, r.Detail = engine.ClassStaleResidue, "tree changed before effect"
		return r, errors.New(r.Detail)
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
	// Once an effect's command, measurement or VCS write started, any
	// failure is uncertain until explicitly reconciled. Until then, a
	// failure is a refusal before launch: the intent could not be recorded,
	// a test run's verification worktree, source or command could not be
	// prepared (the broker's RefusedBeforeExec), a freeze or compare could
	// not read or derive its identity, an integration lacks its commit. A
	// refusal after the intent was recorded is recorded against the intent
	// (recordEffectRefusal), so recovery reads it as a non-execution too.
	defer func() {
		r.ProducedAt = time.Now().UTC()
		if err != nil && r.Class != engine.ClassRefusedBeforeLaunch {
			r.Class = "uncertain"
		}
	}()
	var intent *effectIntent
	refuseBeforeExec := func(e error) (engine.Result, error) {
		if ctx.Err() == nil && (intent != nil || !supportedEffect(d.Config.Primitive)) {
			if recordErr := x.recordDispatchRefusal(ctx, d, e); recordErr != nil {
				return r, errors.Join(e, recordErr)
			}
		}
		return refuseBeforeLaunch(e)
	}
	if d.Config.Primitive != workflow.VcsCommit {
		intent, err = x.effectIntent(ctx, d, w, nil, startEffect)
		if err != nil {
			return refuseBeforeLaunch(err)
		}
	}
	var output any
	switch d.Config.Primitive {
	case workflow.ExternalEffect:
		if d.Config.IsGitPush() {
			output, err = x.executeGitPush(ctx, d, w, policy.task, policy.target)
			if err != nil {
				r.Class, r.Detail = engine.ClassExternalRefused, err.Error()
				return r, nil
			}
			break
		}
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
			// A definitive remote refusal is typed (Appendix B); its reported id
			// is already recorded with the observation.
			if status := (*external.StatusError)(nil); errors.As(err, &status) && status.Refused() {
				r.Class, r.Detail = engine.ClassExternalRefused, err.Error()
				return r, nil
			}
			return r, err
		}
	case workflow.HostCommand:
		runIn, release, e := x.redRunWorktree(ctx, d, b, w, policy.task, scope.TestFile)
		if e != nil {
			return refuseBeforeExec(e)
		}
		defer release()
		if runIn != w {
			r.Detail = "RED verified against the task base " + d.Base
		}
		started := time.Now().UTC()
		dispatched := dispatchedEnvelope(policy.task, d)
		effectiveShape := resolveHostCommandTestShape(d.Config.TestShape, dispatched.Admission.TestShape, d.Stage, d.Config.Role, policy.target != nil, policy.argv, scope.TestID)
		if d.Config.TestRun && dispatched.Admission.TestShape != "" {
			lj := landingJobFromEnvelope(dispatched, scope)
			if lj.TaskID == "" {
				lj.TaskID = d.TaskID
			}
			if receipt, rErr := resolveLandingTestReceipt(lj, policy.argv); rErr == nil {
				_ = x.daemon.recordLandingTestReceipt(ctx, d.ID, receipt, controlplane.Audit{Actor: "daemon", Action: "landing-test-shape", Outcome: "ok"})
			}
		}
		ctx := effects.WithNestedRun(ctx, effects.NestedRun{TaskID: d.TaskID, DispatchID: d.ID, Local: policy.task.Admission.Mode != "Normal"})
		// A run that ended is evidence the gates route, failed or not: one
		// killed at its deadline, one whose output passed the bound, and one
		// whose test binary ended under a test (that test is not counted).
		// Only an error leaves the run uncertain: the caller's cancellation,
		// or a test stream that can't be read (the go process itself killed).
		var checkWrites []string
		if d.Config.Check != nil {
			checkWrites = d.Config.Check.Writes
		}
		checkWrites, bindWritesErr := execution.BoundScopePatterns(checkWrites, d, b)
		if bindWritesErr != nil {
			return refuseBeforeExec(bindWritesErr)
		}
		generator := d.Config.Generate
		if generator != nil {
			bound := *generator
			bound.Writes, bindWritesErr = execution.BoundScopePatterns(generator.Writes, d, b)
			if bindWritesErr != nil {
				return refuseBeforeExec(bindWritesErr)
			}
			generator = &bound
		}
		commandRole, _ := workflow.EffectiveRole(b.Document, dispatched.Job.TaskClass, d.Config.Role)
		commandScopes, scopeErr := execution.BoundWriteScopes(commandRole, d, b)
		if scopeErr != nil {
			return r, scopeErr
		}
		commandReads, readsErr := execution.BoundReadPolicy(workflow.EffectiveRoleStageReadPolicy(b.Document, dispatched.Job.TaskClass, d.Config.Role, d.Stage, b.TaskReads, b.DenyPaths), d, b)
		if readsErr != nil {
			return refuseBeforeExec(readsErr)
		}
		commandRootViews := map[string]provider.ReadRootView{}
		for name := range policy.readRoots {
			if view, ok := b.ReadRootViews[name]; ok {
				commandRootViews[name] = view
			}
		}

		run, e := policy.broker.Run(ctx, runIn, effects.Command{TaskID: d.TaskID, OutputOwnershipRegistry: b.OutputOwnershipRegistry, SharedWriteOwner: d.ID, ExcludedOutputDirs: slices.Clone(b.ExcludedOutputDirs), SharedWriteRegistry: b.SharedWriteRegistry, RuntimeMountIdentities: b.RuntimeMountIdentities, RuntimeMounts: slices.Clone(b.RuntimeMounts), CacheDirs: slices.Clone(b.CacheDirs), Reads: commandReads, DenyPaths: b.DenyPaths, ReadRootViews: commandRootViews, ReadRootPaths: policy.rootPaths, Argv: policy.argv, Timeout: time.Duration(d.Config.TimeoutSeconds) * time.Second, TestID: scope.TestID, ExpectedFailure: scope.ExpectedFailure, Shape: effectiveShape, Check: d.Config.Check, Formal: d.Config.Formal, Adapter: d.Config.CommandAdapter, Writes: checkWrites, DeclaredFiles: scope.Files,
			EffectiveWriteScopes: commandScopes, Generate: generator, Capture: d.Config.Capture, OutputSchema: b.Document.Artifacts[d.Config.Output].Schema, CaptureBytes: commandCaptureBytes(policy.target), Scratch: taskScratch(b.Document),
			Environment:   append(workflow.EnvironmentList(d.Config.Environment), taskDirectoryEnvironment(b)...),
			ArtifactsOnly: b.ArtifactsOnly, OutputDir: b.OutputDir, StagingDir: b.StagingDir, StagingWrite: d.Config.Staging == "write", Accept: d.Config.Accept})
		if e != nil {
			var refused *effects.RefusedBeforeExec
			if errors.As(e, &refused) {
				return refuseBeforeExec(e)
			}
			return r, e
		}
		r.RuntimeMounts = run.RuntimeMounts
		r.ViewConstructionMS = run.ViewConstructionMS
		r.ViewCopiedBytes = run.ViewCopiedBytes
		r.IsolationPolicy = run.IsolationPolicy
		if len(run.Captured) > 0 {
			secrets, secretErr := x.daemon.failureSecrets(x.config.Environment)
			if secretErr != nil {
				return r, secretErr
			}
			for key, text := range run.Captured {
				run.Captured[key] = secrets.redactString(text)
				if run.CheckPayload != nil {
					run.CheckPayload[key] = run.Captured[key]
				}
			}
		}
		if e = x.recordEngineRun(ctx, d, policy.argv, workflow.EnvironmentList(d.Config.Environment), run, started); e != nil {
			return r, e
		}
		if run.CheckError != "" {
			r.Class, r.Detail = engine.ClassMalformedOutput, run.CheckError
			return r, nil
		}
		if run.TimedOut && d.Config.Check != nil {
			r.Class, r.Detail = "command-timeout", "check command timed out"
			return r, nil
		}
		var required []effects.RunEvidence
		if (d.Config.TestRun && d.Config.TestShape == "" || d.Config.Check != nil) && policy.target != nil {
			// A whole-suite (QA) or check run also runs the target's required checks.
			extra, e := x.runRequired(ctx, d, w, *policy.target, policy.task.Admission)
			if e != nil {
				return r, e
			}
			run, required = foldRequired(run, extra), extra
			if run.CheckPayload != nil {
				run.CheckPayload["exit_code"] = run.ExitCode
				run.CheckPayload["output_digest"] = run.OutputDigest
			}
		}
		if d.Config.Formal != nil {
			if run.Proof == nil {
				return r, errors.New("formal command produced no verification receipt")
			}
			raw, marshalErr := json.Marshal(run.Proof)
			if marshalErr != nil {
				return r, marshalErr
			}
			fields := map[string]any{}
			if decodeErr := json.Unmarshal(raw, &fields); decodeErr != nil {
				return r, decodeErr
			}
			fields["exit_code"], fields["tree"], fields["output_digest"] = run.ExitCode, run.Tree, run.OutputDigest
			fields["executable_digest"] = run.ExecutableDigest
			fields["runtime_mounts"] = run.RuntimeMounts
			fields["timed_out"], fields["truncated"] = run.TimedOut, run.Truncated
			output = fields
		} else if d.Config.TestRun {
			// A no-red task may have no RED file: the test is then not in
			// source. A derived scope without a RedReport names no file.
			inSource := false
			if !scope.Derived || scope.TestFile != "" {
				source, e := x.testSource(b, scope.TestFile)
				if e != nil && !errors.Is(e, fs.ErrNotExist) {
					return r, e
				}
				if e == nil {
					_, e = redIdentity(scope.TestFile, source, scope.TestID)
				}
				inSource = e == nil
			}
			output = map[string]any{"exit_code": run.ExitCode, "executed_count": run.ExecutedCount, "matched_count": run.MatchedCount, "expected_failure": run.ExpectedFailure, "test_in_source": inSource, "red_passed": run.RedPassed, "fresh_build": run.FreshBuild, "output_digest": run.OutputDigest}
		} else if d.Config.Generate != nil {
			output = map[string]any{"exit_code": run.ExitCode, "timed_out": run.TimedOut, "changed": run.Changed, "changed_count": run.ChangedCount, "truncated": run.Truncated}
			if !b.ArtifactsOnly {
				r.NewTree = run.Tree
			}
		} else if d.Config.Check != nil {
			if run.CheckPayload != nil {
				run.CheckPayload["exit_code"] = run.ExitCode
				run.CheckPayload["output_digest"] = run.OutputDigest
			}
			output = run.CheckPayload
			if payload, ok := output.(map[string]any); ok {
				for key, text := range run.Captured {
					payload[key] = text
				}
				payload["truncated"] = run.Truncated
			}
		} else if d.Config.CommandAdapter == "configured" {
			output = map[string]any{"exit_code": run.ExitCode, "executed": true, "timed_out": run.TimedOut, "truncated": run.Truncated, "tree": run.Tree, "output_digest": run.OutputDigest}
		} else {
			if len(d.Config.Command) != 3 || d.Config.Command[0] != "gofmt" || d.Config.Command[1] != "-l" || d.Config.Command[2] != "." {
				return r, errors.New("unsupported non-test command artifact")
			}
			debt, e := formatDebt(ctx, b.Worktree, d.Base, d.Tree, run.Output, run.ExitCode)
			if e != nil {
				return r, e
			}
			if run.Truncated {
				// A listing cut at the output bound may leave out files with
				// new debt: it fails the format gate, never passes it.
				debt++
			}
			output = map[string]any{"new_debt": debt}
		}
		// A failed run keeps its excerpt for the turn it routes back to.
		if e = x.recordEngineFeedback(ctx, d, runIn.Path, policy.argv, run, required, output); e != nil {
			return r, e
		}
	case workflow.FreezeArtifact, workflow.CompareFrozen:
		source, e := x.testSource(b, scope.TestFile)
		if e != nil {
			return refuseBeforeExec(e)
		}
		identity, e := redIdentity(scope.TestFile, source, scope.TestID)
		if e != nil {
			return refuseBeforeExec(e)
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
				return refuseBeforeExec(errors.New("missing frozen identity"))
			}
			fields["unchanged"] = frozen == identity
		}
		output = fields
	case workflow.Merge:
		if e := x.verifyActiveConflict(ctx, d, w); e != nil {
			if ownHunkLoss(e) {
				return x.refuseOwnHunkLoss(ctx, d, w, intent, e, r)
			}
			return refuseBeforeExec(e)
		}
		files, e := scope.snapshotFiles(ctx, w)
		if e != nil {
			return refuseBeforeExec(e)
		}
		snapshot, e := x.broker.Snapshot(ctx, w, files)
		if e != nil {
			return refuseBeforeExec(e)
		}
		plan, e := x.broker.PrepareReconcileFrom(ctx, w, snapshot, d.Base, x.config.IntegrationRef)
		if e != nil {
			var conflict *effects.ReconcileError
			if errors.As(e, &conflict) {
				if conflict.Kind == "conflicted" {
					return x.resolveConflict(ctx, d, w, plan, conflict, r, scope)
				}
				raw, _ := json.Marshal(conflict)
				hash, _ := domain.ContentHash([]byte(fmt.Sprintf("%q", d.ID)))
				if recordErr := x.daemon.store.PutMarker(ctx, controlplane.Audit{Actor: "daemon", Action: "reconciliation refused", Outcome: conflict.Kind}, "reconcile-failure/"+hash, raw); recordErr != nil {
					return refuseBeforeExec(recordErr)
				}
				if conflict.Kind == "own-hunk-loss" {
					return x.refuseOwnHunkLoss(ctx, d, w, intent, e, r)
				}
			}
			return refuseBeforeExec(e)
		}
		if plan.Changed {
			active, e := x.activeConflict(ctx, d.TaskID)
			if e != nil {
				return refuseBeforeExec(e)
			}
			if active != nil {
				return refuseBeforeExec(errors.New("integration advanced again after conflict; original own intent retained for operator review"))
			}
			if d.Config.Reverify == "" {
				return refuseBeforeExec(errors.New("changed-base reconciliation requires workflow reverify route"))
			}
			raw, _ := json.Marshal(plan)
			hash, _ := domain.ContentHash([]byte(fmt.Sprintf("%q", d.ID)))
			if e = x.daemon.store.PutMarker(ctx, controlplane.Audit{Actor: "daemon", Action: "record reconcile intent", Outcome: "prepared"}, "reconcile/"+hash, raw); e != nil {
				return refuseBeforeExec(e)
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
			if ownHunkLoss(e) {
				return x.refuseOwnHunkLoss(ctx, d, w, nil, e, r)
			}
			return vcsRefusal(ctx, r, e)
		}
		// Refusals before the commit intent wrote nothing (FAE-VCS-009).
		files, e := scope.snapshotFiles(ctx, w)
		snapshot := effects.ReviewedSnapshot{}
		if e == nil {
			snapshot, e = x.broker.Snapshot(ctx, w, files)
		}
		if e == nil && snapshot.Tree != d.Tree {
			e = errors.New("reviewed tree mismatch")
		}
		if e != nil {
			return vcsRefusal(ctx, r, e)
		}
		if refusal, e := x.commitPolicy(ctx, d, w, policy.task, policy.target); e != nil || refusal != nil {
			if refusal != nil {
				r.Class, r.Detail = refusal.class, refusal.detail
				return r, nil
			}
			// The commit floor could not be checked: nothing was written.
			return refuseBeforeLaunch(e)
		}
		prepared, e := policy.broker.PrepareCommit(ctx, w, snapshot, liveCommitMessage(x.config.CommitMessage, policy.task.Admission, d.TaskID, d.ID))
		if e != nil {
			return vcsRefusal(ctx, r, e)
		}
		intent, e = x.effectIntent(ctx, d, w, &prepared, startEffect)
		if e != nil {
			return refuseBeforeLaunch(e)
		}
		commit, e := policy.broker.ApplyCommit(ctx, w, prepared)
		for retried := false; e != nil; retried = true {
			head, headErr := effectGit(ctx, w, "rev-parse", "HEAD")
			switch CommitDisposition(ctx.Err() != nil || headErr != nil, head == prepared.SHA, head == prepared.Snapshot.Base, retried) {
			case "uncertain":
				return r, errors.Join(e, headErr)
			case "committed":
				if e = policy.broker.ReconcileCommitIndex(ctx, w, prepared); e != nil {
					return r, e
				}
				commit = effects.CommitEvidence{SHA: prepared.SHA, Tree: prepared.Snapshot.Tree, Files: append([]string{}, prepared.Snapshot.Files...)}
			case "retry":
				commit, e = policy.broker.ApplyCommit(ctx, w, prepared)
			default:
				r.Class, r.Detail = engine.ClassVcsRefused, "task commit failed after one retry: "+e.Error()
				return r, x.recordEffectResult(ctx, intent, r)
			}
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
			return refuseBeforeExec(errors.New("missing commit artifact"))
		}
		evidence := effects.CommitEvidence{SHA: sha, Tree: d.Tree, Files: scope.Files}
		land, e := policy.broker.Integrate(ctx, w, evidence, x.config.IntegrationRef, b.Base)
		if e != nil && strings.Contains(e.Error(), "content policy refused:") {
			r.Class, r.Detail = engine.ClassVcsRefused, e.Error()
			return r, x.recordEffectResult(ctx, intent, r)
		}
		// Every failed attempt takes exactly one disposition (FAE-VCS-009).
		for retried := false; e != nil; retried = true {
			var moved *effects.TargetMovedError
			proven := errors.As(e, &moved) && kernel.CanRetryIntegration(moved.NoWrite, moved.Actual != moved.Expected, moved.OwnCommitKnown, d.Config.Reverify != "")
			switch IntegrateDisposition(ctx.Err() != nil, proven, retried) {
			case "uncertain":
				return r, e
			case "reverify":
				r.Class = "target-moved"
				r.Detail = e.Error()
				return r, nil
			case "retry":
				land, e = policy.broker.Integrate(ctx, w, evidence, x.config.IntegrationRef, b.Base)
			default:
				r.Class, r.Detail = engine.ClassVcsRefused, "integration refused after one retry: "+e.Error()
				return r, x.recordEffectResult(ctx, intent, r)
			}
		}
		out := map[string]any{"landed": land.Landed, "sha": land.SHA}
		if land.ContainedBy != "" {
			// Already upstream: record the containing tip (FAE-VCS-007).
			out["contained_by"] = land.ContainedBy
			r.Detail = "contained by " + land.ContainedBy + " (" + land.Containment + ")"
		}
		output = out
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
	if d.Config.Generate != nil && b.FinalizeBranch != nil && r.NewTree != "" {
		if err = b.FinalizeBranch(ctx, &r); err != nil {
			r.Class = "uncertain"
			return r, err
		}
	}
	r.Output, err = json.Marshal(output)
	if err == nil {
		r.ProducedAt = time.Now().UTC()
		err = x.recordEffectResult(ctx, intent, r)
	}
	return r, err
}
