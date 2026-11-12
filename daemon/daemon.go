package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/controller"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/cron"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/external"
	"github.com/xleelz/fae/hostguard"
	"github.com/xleelz/fae/loops"
	"github.com/xleelz/fae/processowner"
	"github.com/xleelz/fae/provider"
	agentruntime "github.com/xleelz/fae/runtime"
	"github.com/xleelz/fae/scratchfs"
	"github.com/xleelz/fae/symphony"
	"github.com/xleelz/fae/tracker"
	"github.com/xleelz/fae/workflow"
	"github.com/xleelz/fae/workspace"
)

// SymphonyConfig configures an embedded LinearOrchestrator (LinearSync) and
// optional SymphonyHTTPServer attached to the running Daemon (`fae serve --workflow-md`).
type SymphonyConfig struct {
	WorkflowPath   string                 `json:"workflow_path,omitempty"`
	HTTPAddr       string                 `json:"http_addr,omitempty"`
	Adapter        tracker.Adapter        `json:"-"`
	ServiceConfig  symphony.ServiceConfig `json:"-"`
	PromptTemplate string                 `json:"-"`
}

type Config struct {
	IsolationProbe            func(context.Context, string) provider.IsolationCapability
	WorkflowSlotRetryInterval time.Duration
	WorkflowSlotNow           func() time.Time
	RecursionCeiling          int // zero uses 8; configurable instance recursion policy
	WorkflowLimit             int
	WorkflowLedgerPath        string
	WorkflowSlotSample        func(context.Context) (hostguard.Sample, error)
	WorkflowSlotTotalMemory   int64
	Scratch                   bool // Creation-only; existing scratch identity survives ordinary restarts.
	ProphetRoutes             []workflow.Binding
	ProphetMaxFailovers       int // zero defaults to one; negative disables failover

	ProphetProvider     string
	ProphetModel        string
	ProphetRunner       func(context.Context, string, json.RawMessage) (json.RawMessage, error) `json:"-"`
	PrincipalConfig     string                                                                  // owner-only JSON array of delegated credential bindings
	EventRetentionAge   time.Duration
	EventRetentionCount int

	MessageMaxAge time.Duration // zero selects the 24-hour default
	// RouteExhaustion bounds a no-eligible-route park before ProviderUnavailable;
	// DrainBound bounds graceful drain-down of in-flight commit/integrate steps.
	RouteExhaustion, DrainBound time.Duration
	// StuckCommitBound is the VCS stuck commit/integrate-stage detector bound.
	StuckCommitBound time.Duration
	LoopRunner       loops.Runner `json:"-"`
	// ConfigureIdleSessions is an opt-in trusted adapter hook. The adapter owns
	// admission, gated process registration, and Begin/End for every reused turn.
	// The built-in one-shot bridge requires no persistent panes and leaves it nil.
	ConfigureIdleSessions                        func(context.Context, *agentruntime.OwnedIdleSessions, string) error `json:"-"`
	Root, Instance, Workspace, DoltBinary, RunID string
	DefaultMode                                  string // Local (default) or Normal for submissions that state none
	Port, MaxInFlight                            int
	Executor                                     engine.Executor
	Live                                         *LiveConfig
	// MaxMolecules and MaxWorkflowMolecules bound running molecules instance-wide
	// and per workflow (FAE-SCH-007); zero selects the −1 (unbounded) default.
	MaxMolecules, MaxWorkflowMolecules int
	UI                                 *UIConfig       // nil: no UI child (FAE-API-033)
	TUI                                *TUIConfig      // nil: no TUI child
	WorkflowMD                         string          // optional WORKFLOW.md path for unified Symphony LinearSync
	SymphonyHTTPAddr                   string          // optional host:port override for Symphony HTTP listener
	Symphony                           *SymphonyConfig `json:"-"`

	// DoltStopWait bounds the Dolt child's graceful stop before a forced kill
	// (zero selects 30 s); DoltRestartAttempts bounds the restarts of one
	// plane outage (zero selects 3) (FAE-CP-002, -006, Appendix A).
	DoltStopWait        time.Duration
	DoltRestartAttempts int
	// RearmBudget and RearmWindow bound durable drive re-arms per cause
	// (FAE-CTL-020; zero selects 3 per hour).
	RearmBudget int
	RearmWindow time.Duration
	// InlineLimit bounds every plane row column in bytes; a longer payload is
	// stored as a blob (FAE-CP-012; zero selects 64 KiB).
	InlineLimit int
	// QuarantineUnrunnableRevisions makes this start quarantine the active
	// tasks pinned to a stored workflow revision this binary cannot run,
	// instead of refusing to start (`serve --quarantine-unrunnable-revisions`;
	// FAE-WF-093, startup_checks.go). A no-op with no such task.
	QuarantineUnrunnableRevisions bool
}
type Daemon struct {
	isolationFact         atomic.Value
	workflowSlots         *workflowSlotManager
	graphs                sync.Map // molecule id -> moleculeGraph (immutable content)
	keyedRequests         keyedRequestTurns
	admissionWrites       admissionWriteBook // guarded by mu
	shutdownEvent         sync.Once
	replicationScheduleMu sync.Mutex
	backupScheduleMu      sync.Mutex
	retentionScheduleMu   sync.Mutex
	// Operator controls waiting on each schedule (schedule_control.go).
	replicationControls, backupControls, retentionControls atomic.Int32
	scheduleWakes                                          scheduleWakes
	backupMu                                               sync.Mutex
	reviewProvisionMu                                      sync.Mutex
	backlogUncertain                                       bool
	backlogUncertainIDs                                    []string
	prophetActive                                          map[string]bool
	readPool                                               readPool
	statusExtras                                           statusExtrasState
	statusRefresh                                          statusRefresher
	defaultWorkflow                                        atomic.Value // DefaultWorkflowStatus
	startupReport                                          atomic.Value // StartupChecks
	ui                                                     *uiChild
	tui                                                    *tuiChild
	linearOrch                                             *LinearOrchestrator
	symphonyHTTP                                           *SymphonyHTTPServer

	ticketActive    atomic.Bool
	ticketRound     atomic.Uint64
	ticketStatus    atomic.Value
	ticketDiscovery *external.Discovery
	// Startup executable identity is immutable even if its pathname is replaced.
	binaryPath, binaryHash string
	driveAlive             atomic.Bool
	driveStop              atomic.Bool
	drivePending           atomic.Bool
	driveGeneration        atomic.Uint64
	driveCause             atomic.Value
	driveRearm             chan struct{}
	loopCheckpoint         *loopCheckpoint
	pendingCheckpoint      *jobCheckpoint
	pendingCompletion      *completion
	pendingLoop            *loopCompletion

	// checkpointing marks a drive checkpoint whose plane round trip runs
	// without the writer lock (FAE-ROB-021); checkpointDone wakes waiters.
	checkpointing  bool
	checkpointDone *sync.Cond

	// Drain-protected workers (commit/integrate) outlive ctx until drainCancel.
	drainCtx    context.Context
	drainCancel context.CancelFunc
	drainMu     sync.Mutex
	drained     []completion
	// quarantined tasks are excluded from the drive until an operator verb
	// releases or retires them (FAE-ROB-042).
	quarantined    map[string]string
	quarantineBook quarantineBook
	// unrunnableRevisionTasks are the tasks this start quarantined for a
	// pinned revision this binary cannot run (startup_checks.go), by task;
	// a task leaves it by migration or quarantine retirement.
	unrunnableRevisionTasks      map[string]UnloadableRevisionRef
	restoredUnrunnableQuarantine map[string]bool
	// storedTerminal counts, by state, the terminal tasks startup did not
	// materialise (FAE-CP-009).
	storedTerminal map[string]int
	// finished caches those tasks' Board rows, and topologyCache the Topology
	// read (finished_rows.go, topology_cache.go; FAE-ROB-020).
	finished      finishedRows
	topologyCache topologyCache
	// outage is a detected hung plane; revalidate asks the drive to compare
	// its checkpoints with a restarted plane (FAE-CP-006).
	outage     atomic.Pointer[planeOutage]
	revalidate atomic.Bool
	restartMu  sync.Mutex
	// stepFaults are housekeeping steps failing this episode (FAE-ROB-001).
	stepFaults      map[string]string
	healthRows      map[string]healthRow
	commitRest      map[string]commitRest
	lastTaskFault   taskFaultRecord
	testTaskFault   func(string, taskOutcome, error)
	testFault       func(point, id string) error
	driveRounds     atomic.Uint64
	withheld        atomic.Pointer[withheldSnapshot]
	lastTransition  atomic.Int64
	crons           map[string]cron.Schedule
	loopPriority    bool
	loops           map[string]loops.Schedule
	loopActive      map[string]context.CancelFunc
	loopResults     chan loopCompletion
	idleSessions    *agentruntime.OwnedIdleSessions
	draining        bool
	live            *liveExecutor
	workers         sync.WaitGroup
	workerMu        sync.Mutex
	workersClosed   bool
	maintenance     atomic.Pointer[maintenanceState]
	config          Config
	layout          Layout
	lock            *instanceLock
	log             logWriter
	obs             observer
	child           *controlplane.Server
	childMu         sync.RWMutex
	store           *controlplane.Store
	planeDB         *sql.DB
	rpc             *api.Server
	ctx             context.Context
	childCtx        context.Context
	childCancel     context.CancelFunc
	cancel          context.CancelFunc
	mu              sync.Mutex
	jobs            map[string]controlplane.JobSnapshot
	active          map[string]bool
	inFlightCancel  map[string]context.CancelFunc
	inFlightPreempt map[string]string
	sessions        *provider.SessionRegistry
	executor        engine.Executor
	wake            chan struct{}
	results         chan completion
	done            chan struct{}
	stopOnce        sync.Once
	closeOnce       sync.Once
	err             error
	started         time.Time
	reads           chan struct{}
	status          atomic.Value
	eventMu         sync.Mutex
	eventWake       chan struct{}
	subscribers     chan struct{}
	contentPolicies []ContentPolicyConfig
}

// RegisterContentPolicy registers a ContentPolicyConfig for daemon secret redaction (FAE-SEC-009).
func (d *Daemon) RegisterContentPolicy(cp ContentPolicyConfig) {
	d.contentPolicies = append(d.contentPolicies, cp)
}

type completion struct {
	id, dispatch string
	result       engine.Result
	err          error
}
type envelope struct {
	Job       engine.Job  `json:"job"`
	Admission AdmitParams `json:"admission"`
	// AdmittedAt is the daemon's admission time of every task (FAE-SCH-003).
	AdmittedAt time.Time `json:"admitted_at,omitempty"`
	// Superseded is the acceptance text an in-flight turn was dispatched with
	// before an edit (FAE-ROB-040).
	Superseded *supersededAcceptance `json:"superseded_acceptance,omitempty"`
	// SupersededFields are the declared files, test filter and test shape an
	// in-flight turn was dispatched under before an edit (daemon/TASKS.md).
	SupersededFields *supersededStructuredFields `json:"superseded_fields,omitempty"`
	// Deletion is a TaskDelete waiting for the task's turn in flight
	// (daemon/TASKS.md).
	Deletion *pendingTaskDelete `json:"pending_delete,omitempty"`
}

func Start(ctx context.Context, c Config) (d *Daemon, err error) {
	if c.RecursionCeiling < 0 || c.RecursionCeiling > 64 {
		return nil, errors.New("recursion ceiling must be 1..64 or zero for default 8")
	}
	if _, err = prophetRoutes(c); err != nil {
		return nil, err
	}
	if c.EventRetentionAge < 0 || c.EventRetentionCount < 0 {
		return nil, errors.New("event retention must be positive or zero for defaults")
	}
	if c.Live != nil && c.Executor != nil {
		return nil, errors.New("live config and custom executor are mutually exclusive")
	}
	safe, probeErr := controller.Unsandboxed(os.Getpid())
	if probeErr != nil || !safe {
		return nil, controller.SandboxRefusal("fae serve", os.Getpid(), probeErr)
	}
	binaryPath, err := os.Executable()
	if err != nil {
		return nil, err
	}
	binaryHash, err := controller.HashFile(binaryPath)
	if err != nil {
		return nil, err
	}
	if c.MessageMaxAge == 0 {
		c.MessageMaxAge = 24 * time.Hour
	}
	if c.MessageMaxAge < 0 {
		return nil, errors.New("message max age must be positive")
	}
	if c.Instance == "" {
		c.Instance = "default"
	}
	if c.RunID == "" {
		c.RunID = "backlog-drain"
	}
	// FAE-SCH-004: tasks in flight default to -1, unbounded.
	if c.MaxInFlight == 0 {
		c.MaxInFlight = -1
	}
	if !validTasksInFlight(c.MaxInFlight) {
		return nil, errors.New("max in flight must be -1 (unbounded) or 1..1000000")
	}
	for _, limit := range []*int{&c.MaxMolecules, &c.MaxWorkflowMolecules} {
		if *limit == 0 {
			*limit = -1
		}
		if !validMoleculeLimit(*limit) {
			return nil, errors.New("running molecule limits must be -1 (unbounded) or 1..1000000")
		}
	}
	if c.InlineLimit != 0 && !controlplane.ValidInlineLimit(c.InlineLimit) {
		return nil, fmt.Errorf("inline limit must be %d..%d bytes", controlplane.MinInlineLimit, controlplane.DefaultInlineLimit)
	}
	l, err := Paths(c.Root, c.Instance)
	if err != nil {
		return nil, err
	}
	doltPort, _ := controlplane.Port(l.Data, c.Port)
	if c.UI, err = normalizeUI(c.UI, l, doltPort); err != nil {
		return nil, err
	}
	if c.TUI, err = normalizeTUI(c.TUI); err != nil {
		return nil, err
	}
	if c.Symphony == nil && (strings.TrimSpace(c.WorkflowMD) != "" || strings.TrimSpace(c.SymphonyHTTPAddr) != "") {
		if c.Symphony, err = ResolveSymphonyConfig(c.WorkflowMD, c.SymphonyHTTPAddr); err != nil {
			return nil, err
		}
	}
	if err = checkLayout(c, l); err != nil {
		return nil, err
	}
	unlockLifecycle, err := lifecycleLock(ctx, c.Root, c.Instance)
	if err != nil {
		return nil, err
	}
	defer unlockLifecycle()
	_, existingErr := os.Lstat(l.Root)
	freshInstance := os.IsNotExist(existingErr)
	if existingErr != nil && !freshInstance {
		return nil, existingErr
	}
	if err = checkLayout(c, l); err != nil {
		return nil, err
	}
	for _, p := range []string{l.Root, l.Run, l.Data, l.Worktrees, filepath.Join(l.Root, "backup"), filepath.Join(l.Root, "archive"), filepath.Join(l.Root, "dolt-config")} {
		if err = mkdirPrivate(p); err != nil {
			return nil, err
		}
	}
	if err = prepareScratch(c, l, freshInstance); err != nil {
		return nil, err
	}
	lock, err := acquire(filepath.Join(l.Run, "lock"))
	if err != nil {
		return nil, err
	}
	_ = processowner.StartReaper(processowner.ReaperGrace) // FAE-HOST-021/-023 (Linux)
	ownctx, cancel := context.WithCancel(context.Background())
	childctx, childcancel := context.WithCancel(context.Background())
	drainctx, draincancel := context.WithCancel(context.Background())
	d = &Daemon{drainCtx: drainctx, drainCancel: draincancel, quarantined: map[string]string{}, binaryPath: binaryPath, binaryHash: binaryHash, config: c, layout: l, lock: lock, ctx: ownctx, cancel: cancel, childCtx: childctx, childCancel: childcancel, jobs: map[string]controlplane.JobSnapshot{}, active: map[string]bool{}, inFlightCancel: map[string]context.CancelFunc{}, inFlightPreempt: map[string]string{}, sessions: provider.NewSessionRegistry(filepath.Join(l.Root, "sessions")), wake: make(chan struct{}, 1), results: make(chan completion, completionBuffer(c.MaxInFlight)), done: make(chan struct{}), started: time.Now().UTC(), reads: make(chan struct{}, 8), executor: c.Executor, eventWake: make(chan struct{}), subscribers: make(chan struct{}, 16)}
	owned := d
	defer func() {
		if err != nil {
			cancel()
			childcancel()
			draincancel()
			if owned.idleSessions != nil {
				cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
				_ = owned.idleSessions.Close(cleanup)
				stop()
			}
			cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
			err = errors.Join(err, owned.releasePaneConfiguration(cleanup))
			stop()
			if owned.symphonyHTTP != nil {
				cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
				_ = owned.symphonyHTTP.Close(cleanup)
				stop()
			}
			if owned.rpc != nil {
				cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
				_ = owned.rpc.Shutdown(cleanup)
				stop()
			}
			if owned.store != nil {
				owned.store.Close()
			}
			if owned.child != nil {
				owned.child.Stop()
			}
			if owned.log != nil {
				owned.log.Close()
			}
			lock.close()
		}
	}()
	d.log, err = openSupervisionLog(l.Log, c.Instance, supervisionStderr)
	if err != nil {
		return nil, err
	}
	if err = d.startPlane(ctx); err != nil {
		return nil, err
	}
	stats, err := d.store.TicketStats(ctx)
	if err != nil {
		return nil, err
	}
	d.ticketStatus.Store(ticketStatus{TicketOutboxStats: stats})
	d.lifecycle("daemon-starting", nil)
	if err = d.probeIsolation(ctx); err != nil {
		return nil, err
	}
	if err = scratchfs.ReapSessionCopies(filepath.Join(l.Root, "sessions")); err != nil {
		return nil, fmt.Errorf("reclaim ended session copies: %w", err)
	}
	d.lifecycle("dolt-started", map[string]any{"dolt_pid": d.child.PID(), "port": d.child.Port()})
	if lock.stale {
		if err = d.store.PutMarker(ctx, controlplane.Audit{Actor: "daemon", Action: "reclaim dead owner", Outcome: "ok"}, "owner-reclaimed", json.RawMessage(`true`)); err != nil {
			return nil, err
		}
	}
	if c.Live != nil {
		if err = d.setupLive(ctx); err != nil {
			return nil, err
		}
	}
	if d.executor == nil {
		d.executor = engine.NewScripted(defaults.HappyScript())
	}
	rawMarker, _ := d.store.GetMarker(ctx, "clean-stop")
	marker := parseStopMarker(rawMarker)
	snapshots, err := d.loadActiveJobs(ctx)
	if err != nil {
		return nil, err
	}
	if err = d.installDefault(ctx); err != nil {
		return nil, err
	}
	if _, registryErr := os.Stat(d.outputOwnershipPath()); os.IsNotExist(registryErr) {
		if err = d.writeOutputOwnership(outputOwnership{Version: 1, Owners: []outputOwner{}}); err != nil {
			return nil, err
		}
	}
	if err = d.setupScenario(ctx); err != nil {
		return nil, err
	}
	if snapshots, err = d.startupChecks(ctx, snapshots, marker); err != nil {
		return nil, err
	}
	if err = d.loadStoredJobs(ctx, snapshots, marker); err != nil {
		return nil, err
	}
	if err = d.rebuildOutputOwnership(ctx); err != nil {
		return nil, err
	}
	if err = d.setupWorkflowSlots(ctx); err != nil {
		return nil, err
	}
	if err = d.store.PutMarker(ctx, controlplane.Audit{Actor: "daemon", Action: "starting", Outcome: "ok"}, "clean-stop", json.RawMessage(`false`)); err != nil {
		return nil, err
	}
	if err = d.loadLoops(ctx); err != nil {
		return nil, err
	}
	if err = d.recoverReplicationSchedule(ctx); err != nil {
		return nil, err
	}
	if err = d.recoverBackupSchedule(ctx); err != nil {
		return nil, err
	}
	if err = d.recoverRetentionSchedule(ctx); err != nil {
		return nil, err
	}
	if err = d.loadCrons(ctx); err != nil {
		return nil, err
	}
	if _, err = d.reconcileBacklogDone(ctx, false, controlplane.Audit{Actor: "daemon", Action: "startup reconcile Done", Outcome: "ok"}); err != nil {
		return nil, err
	}
	if err = d.restoreControllerMarker(ctx); err != nil {
		return nil, err
	}
	d.lastTransition.Store(time.Now().UnixNano())
	d.publishStatus()
	token, err := api.Credential(l.Credential)
	if err != nil {
		return nil, err
	}
	if info, e := os.Lstat(l.Socket); e == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, errors.New("refusing non-socket at socket path")
		}
		if err = os.Remove(l.Socket); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(e) {
		return nil, e
	}
	d.rpc, err = api.New(token, d.pairedHandle)
	if err != nil {
		return nil, err
	}
	if err = installPrincipals(d.rpc, c.PrincipalConfig); err != nil {
		return nil, err
	}
	d.setupUI(ctx)
	d.setupTUI(ctx)
	if err = d.setupSymphony(); err != nil {
		return nil, err
	}
	d.rpc.SetEventStream(d.stream)
	if err = d.rpc.Listen(l.Socket); err != nil {
		return nil, err
	}
	pinned, err := pinBuildWorkspace(l.Binding, c.Workspace) // FAE-ARC-023
	if err != nil {
		d.rpc.Shutdown(context.Background())
		return nil, err
	}
	binding, _ := json.Marshal(struct {
		Workspace      string                    `json:"workspace"`
		BuildWorkspace *workspace.BuildWorkspace `json:"build_workspace,omitempty"`
	}{c.Workspace, pinned})
	if err = writePrivate(l.Binding, binding); err != nil {
		d.rpc.Shutdown(context.Background())
		return nil, err
	}
	pid, _ := json.Marshal(lock.owner)
	if err = writePrivate(filepath.Join(l.Run, "pid.json"), pid); err != nil {
		d.rpc.Shutdown(context.Background())
		return nil, err
	}
	if err = d.advanceEventWindow(ctx); err != nil {
		return nil, err
	}
	d.workers.Add(1)
	go d.maintainEventWindow(d.store)
	go d.maintainSessionCopies()
	d.workers.Add(1)
	go d.maintainRetentionSchedule()
	d.workers.Add(1)
	go d.maintainBackupSchedule()
	d.workers.Add(1)
	go d.maintainReplicationSchedule()
	d.workers.Add(1)
	go d.maintainObservability()
	d.workers.Add(1)
	go d.monitorPlane()
	d.workers.Add(1)
	go d.maintainStatusSnapshot()
	d.driveRearm = make(chan struct{}, 1)
	d.checkpointDone = sync.NewCond(&d.mu)
	go func() {
		if e := d.rpc.Serve(); e != nil {
			d.fail(e)
		}
	}()
	d.lifecycle("daemon-ready", map[string]any{"socket": l.Socket})
	go d.drive()
	go func() {
		select {
		case <-ctx.Done():
			d.cancel()
		case <-d.done:
		}
	}()
	d.signal()
	d.startUI()
	d.startTUI()
	d.startSymphony()
	return d, nil
}

// loadJob validates and recovers one durable job at startup. Errors that are
// instance configuration conflicts are infrastructureError; others are the job's.
func (d *Daemon) loadJob(ctx context.Context, snapshot controlplane.JobSnapshot, marker stopMarker) error {
	e, err := d.validateStoredJob(ctx, snapshot)
	if err != nil {
		return err
	}
	if e.Job.InFlight != nil {
		recovered, err := d.recoverInFlight(ctx, e, marker)
		if err != nil {
			return taskErr("recover", err)
		}
		recoveryEvents := recovered.History[len(e.Job.History):]
		e.Job = recovered
		if snapshot, err = d.persist(ctx, snapshot, e, recoveryEvents); err != nil {
			d.pendingCheckpoint = nil
			return err
		}
	} else if resumed, events, err := engine.RevalidateResume(e.Job); err != nil {
		return taskErr("resume", err)
	} else if len(events) > 0 {
		// The durable stage's evidence no longer re-validates: ResumeEvidence.
		e.Job = resumed
		if snapshot, err = d.persist(ctx, snapshot, e, events); err != nil {
			d.pendingCheckpoint = nil
			return err
		}
	}
	d.jobs[snapshot.ID] = snapshot
	return nil
}

// validateStoredJob checks one durable row without writing. Only the pane-limit
// floor (FAE-SCH-012) is an instance conflict; a row this daemon cannot honor,
// such as another executor kind, fails closed for itself (FAE-ROB-042).
func (d *Daemon) validateStoredJob(ctx context.Context, snapshot controlplane.JobSnapshot) (envelope, error) {
	var e envelope
	if err := decodeEnvelope(snapshot.Payload, &e); err != nil {
		return e, taskErr("load", fmt.Errorf("invalid durable job %s: %w", snapshot.ID, err))
	}
	if err := engine.Validate(e.Job); err != nil {
		return e, taskErr("load", err)
	}
	if !e.Job.State.Terminal() {
		e.Job.Document.ValidationRecursionCeiling = d.recursionCeiling()
		if issues := workflow.Validate(e.Job.Document); len(issues) > 0 {
			return e, taskErr("load", &workflow.ValidationError{Issues: issues})
		}
	}
	if d.live != nil && d.live.guard.panes != nil && !e.Job.State.Terminal() {
		if err := validatePaneFloor(e.Job.Document, d.live.guard.effectivePaneLimit()); err != nil {
			return e, &infrastructureError{fmt.Errorf("durable job %s: %w", snapshot.ID, err)}
		}
	}
	mode := "scripted"
	if d.live != nil {
		mode = "live"
	}
	if e.Job.ExecutionKind != mode {
		return e, taskErr("load", fmt.Errorf("stored job executor kind %s differs from daemon mode %s", e.Job.ExecutionKind, mode))
	}
	if err := dispositionErr(e.Job); err != nil {
		return e, taskErr("load", err)
	}
	if e.Job.TaskID != snapshot.ID || int64(e.Job.Revision) != snapshot.WorkflowRevision || e.Job.Document.ID != snapshot.WorkflowID || string(e.Job.State) != snapshot.State || (e.Admission.TargetConfig == "" && snapshot.TargetID != d.config.Workspace) || (e.Admission.TargetConfig != "" && (snapshot.TargetID != e.Admission.TargetConfig || snapshot.TargetRevision != e.Admission.TargetRevision)) {
		return e, taskErr("load", errors.New("durable job binding mismatch"))
	}
	targetCtx := ctx
	if e.Job.State == "Retired" {
		targetCtx = context.WithValue(ctx, retiredTargetRecoveryKey{}, true)
	}
	if err := d.checkPinnedTarget(targetCtx, e.Admission, e.Job.Document); err != nil {
		return e, taskErr("load", fmt.Errorf("durable target %s: %w", snapshot.ID, err))
	}
	return e, nil
}
func (d *Daemon) signal() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}
func (d *Daemon) fail(err error) {
	d.mu.Lock()
	if d.err == nil {
		d.err = err
	}
	d.mu.Unlock()
	d.cancel()
}
func (d *Daemon) Run(ctx context.Context) error {
	select {
	case <-ctx.Done():
		d.cancel()
	case <-d.done:
	}
	closectx, cancel := context.WithTimeout(context.Background(), d.drainBound()+time.Minute)
	defer cancel()
	return d.Close(closectx)
}
func (d *Daemon) drainBound() time.Duration {
	if d.config.DrainBound > 0 {
		return d.config.DrainBound
	}
	return DefaultDrainBound
}

// Close is the single graceful path (FAE-ROB-010): the drive stops, turns other
// than commit/integrate are cancelled for resume, commit/integrate may finish
// within the drain-down bound, finished turns are collected, the marker lists
// the in-flight set left, and children are always reaped, even on overrun.
func (d *Daemon) Close(ctx context.Context) error {
	d.closeWorkers()
	d.shutdownEvent.Do(func() { d.lifecycle("daemon-shutting-down", nil) })
	d.cancel()
	drain, stopDrain := context.WithTimeout(ctx, d.drainBound())
	defer stopDrain()
	// The drive launches workers until it observes the stop, so join it before
	// waiting on them: a WaitGroup Add may not race the Wait.
	workersDone := make(chan struct{})
	var joining sync.Once
	joinWorkers := func() { joining.Do(func() { go func() { d.workers.Wait(); close(workersDone) }() }) }
	driveDone := waitFor(drain, d.done)
	if driveDone {
		joinWorkers()
	}
	finished := driveDone && waitFor(drain, workersDone)
	overrun := !finished
	if overrun {
		d.taskLog("shutdown overrun: drive stopped %t, workers finished %t after %s; cancelling\n", driveDone, false, d.drainBound())
		d.drainCancel()
		grace, stopGrace := context.WithTimeout(context.Background(), 5*time.Second)
		if driveDone = waitFor(grace, d.done); driveDone {
			joinWorkers()
		}
		finished = driveDone && waitFor(grace, workersDone)
		stopGrace()
	}
	d.drainCancel()
	d.closeOnce.Do(func() {
		cleanup, stopCleanup := context.WithTimeout(context.Background(), 30*time.Second)
		defer stopCleanup()
		locked := driveDone && d.lockWithin(cleanup)
		if locked {
			d.drainCompletions(cleanup)
		}
		d.stopUI(cleanup)
		d.stopTUI(cleanup)
		d.stopSymphony(cleanup)
		_ = d.rpc.Shutdown(cleanup)
		var err error
		if d.live != nil && d.live.resident != nil {
			err = errors.Join(err, d.live.resident.Close(cleanup))
		}
		if d.idleSessions != nil {
			err = errors.Join(err, d.idleSessions.Close(cleanup))
		}
		if locked && finished {
			err = errors.Join(err, d.closeWorkflowSlots(cleanup))
		}
		err = errors.Join(err, d.releasePaneConfiguration(cleanup))
		if locked {
			d.err = errors.Join(d.err, err)
			if d.err == nil {
				d.err = d.writeStopMarker(cleanup, overrun)
			}
			d.mu.Unlock()
		}
		d.lifecycle("daemon-stopped", map[string]any{"clean": locked && d.err == nil})
		_ = d.store.Close()
		d.stopPlane()
		d.childCancel()
		_, _ = processowner.KillAdopted(cleanup)
		_ = os.Remove(filepath.Join(d.layout.Run, "pid.json"))
		_ = d.log.Close()
		d.lock.close()
	})
	final, stopFinal := context.WithTimeout(context.Background(), time.Second)
	defer stopFinal()
	if !d.lockWithin(final) {
		return errDrainOverrun
	}
	defer d.mu.Unlock()
	return d.err
}

// beginWorker counts a worker that an RPC handler starts (a landing, a
// Prophet turn) against Close's join, or refuses once Close has begun, so no
// WaitGroup Add can race Close's Wait. Workers the drive starts need no such
// guard: Close joins the drive before it waits on them.
func (d *Daemon) beginWorker() error {
	d.workerMu.Lock()
	defer d.workerMu.Unlock()
	if d.workersClosed {
		return shuttingDownError()
	}
	d.workers.Add(1)
	return nil
}
func (d *Daemon) closeWorkers() {
	d.workerMu.Lock()
	d.workersClosed = true
	d.workerMu.Unlock()
}
func (d *Daemon) Layout() Layout { return d.layout }

// LinearOrchestrator returns the attached Symphony LinearOrchestrator, if configured.
func (d *Daemon) LinearOrchestrator() *LinearOrchestrator { return d.linearOrch }

// SymphonyHTTPServer returns the attached Symphony HTTP server, if configured.
func (d *Daemon) SymphonyHTTPServer() *SymphonyHTTPServer { return d.symphonyHTTP }

// SymphonyURL returns the base URL of the attached Symphony HTTP listener, or "".
func (d *Daemon) SymphonyURL() string {
	if d.symphonyHTTP == nil {
		return ""
	}
	return d.symphonyHTTP.URL()
}

// ParseSymphonyHTTPAddr parses and validates a host:port address for Symphony HTTP listening.
func ParseSymphonyHTTPAddr(addr string) (string, int, error) {
	trimmed := strings.TrimSpace(addr)
	if trimmed == "" {
		return "", 0, errors.New("symphony http address is empty")
	}
	host, portStr, err := net.SplitHostPort(trimmed)
	if err != nil {
		return "", 0, fmt.Errorf("invalid --symphony-http-addr %q: %w", addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 0 || port > 65535 {
		return "", 0, fmt.Errorf("invalid --symphony-http-addr %q: port must be 0..65535", addr)
	}
	if strings.TrimSpace(host) == "" {
		host = "127.0.0.1"
	}
	return host, port, nil
}

// ResolveSymphonyConfig loads and validates a WORKFLOW.md file via symphony.LoadWorkflow
// and resolves a SymphonyConfig ready to attach to a Daemon.
func ResolveSymphonyConfig(workflowMD string, httpAddr string) (*SymphonyConfig, error) {
	wfPath := strings.TrimSpace(workflowMD)
	addr := strings.TrimSpace(httpAddr)
	if wfPath == "" {
		if addr != "" {
			return nil, errors.New("--symphony-http-addr requires --workflow-md")
		}
		return nil, nil
	}
	var overrideHost string
	var overridePort *int
	if addr != "" {
		h, p, err := ParseSymphonyHTTPAddr(addr)
		if err != nil {
			return nil, err
		}
		overrideHost = h
		overridePort = &p
	}
	absPath, err := filepath.Abs(wfPath)
	if err != nil {
		return nil, err
	}
	wfDef, err := symphony.LoadWorkflow(absPath)
	if err != nil {
		return nil, err
	}
	svcCfg, err := symphony.ResolveServiceConfig(wfDef, os.Getenv)
	if err != nil {
		return nil, err
	}
	if len(svcCfg.Tracker.ActiveStates) == 0 {
		svcCfg.Tracker.ActiveStates = []string{"Todo", "In Progress"}
	}
	if len(svcCfg.Tracker.TerminalStates) == 0 {
		svcCfg.Tracker.TerminalStates = []string{"Closed", "Cancelled", "Canceled", "Duplicate", "Done"}
	}
	if overridePort != nil {
		svcCfg.Server.Host = overrideHost
		p := *overridePort
		svcCfg.Server.Port = &p
	}
	switch strings.ToLower(strings.TrimSpace(svcCfg.Tracker.Kind)) {
	case "":
		return nil, errors.New("tracker.kind is required")
	case "linear":
		apiKey, _ := svcCfg.Tracker.Provider["api_key"].(string)
		if strings.TrimSpace(apiKey) == "" {
			apiKey = strings.TrimSpace(os.Getenv("LINEAR_API_KEY"))
			svcCfg.Tracker.Provider["api_key"] = apiKey
		}
		if strings.TrimSpace(apiKey) == "" {
			return nil, fmt.Errorf("%s: missing Linear api_key", tracker.ErrCategoryMissingSecret)
		}

	case "memory":
	default:
		return nil, fmt.Errorf("%s: unsupported tracker kind %q", tracker.ErrCategoryUnsupportedKind, svcCfg.Tracker.Kind)
	}
	if err := symphony.ValidateDispatchConfig(svcCfg); err != nil {
		return nil, err
	}
	if err := validateEmbeddedLinear(*svcCfg); err != nil {
		return nil, err
	}
	adapter, err := buildDaemonTrackerAdapter(*svcCfg)
	if err != nil {
		return nil, err
	}
	return &SymphonyConfig{
		WorkflowPath:   absPath,
		HTTPAddr:       addr,
		Adapter:        adapter,
		ServiceConfig:  *svcCfg,
		PromptTemplate: wfDef.PromptTemplate,
	}, nil
}

func buildDaemonTrackerAdapter(cfg symphony.ServiceConfig) (tracker.Adapter, error) {
	if strings.EqualFold(strings.TrimSpace(cfg.Tracker.Kind), "linear") {
		return tracker.NewAdapter(cfg.Tracker, os.Getenv, nil)
	}
	if !strings.EqualFold(strings.TrimSpace(cfg.Tracker.Kind), "memory") {
		return nil, fmt.Errorf("unsupported tracker kind %q", cfg.Tracker.Kind)
	}
	return &daemonMemoryTrackerAdapter{
		kind:           strings.ToLower(strings.TrimSpace(cfg.Tracker.Kind)),
		activeStates:   append([]string(nil), cfg.Tracker.ActiveStates...),
		terminalStates: append([]string(nil), cfg.Tracker.TerminalStates...),
		issues:         make(map[string]symphony.Issue),
	}, nil
}

type daemonMemoryTrackerAdapter struct {
	mu             sync.RWMutex
	kind           string
	activeStates   []string
	terminalStates []string
	issues         map[string]symphony.Issue
}

func (a *daemonMemoryTrackerAdapter) Kind() string             { return a.kind }
func (a *daemonMemoryTrackerAdapter) ActiveStates() []string   { return a.activeStates }
func (a *daemonMemoryTrackerAdapter) TerminalStates() []string { return a.terminalStates }

func (a *daemonMemoryTrackerAdapter) FetchIssuesByStates(_ context.Context, stateNames []string) ([]symphony.Issue, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	var out []symphony.Issue
	for _, iss := range a.issues {
		if symphony.StateInList(iss.State, stateNames) {
			out = append(out, iss)
		}
	}
	return out, nil
}

func (a *daemonMemoryTrackerAdapter) FetchIssuesByIDs(_ context.Context, issueIDs []string) ([]symphony.Issue, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	var out []symphony.Issue
	for _, id := range issueIDs {
		if iss, ok := a.issues[id]; ok {
			out = append(out, iss)
		}
	}
	return out, nil
}

func (a *daemonMemoryTrackerAdapter) TransitionIssueState(_ context.Context, issueID string, targetStateName string) (symphony.Issue, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	iss, ok := a.issues[issueID]
	if !ok {
		return symphony.Issue{}, fmt.Errorf("issue %s not found", issueID)
	}
	iss.State = targetStateName
	a.issues[issueID] = iss
	return iss, nil
}

func (a *daemonMemoryTrackerAdapter) AddIssueComment(_ context.Context, _ string, _ string) (string, error) {
	return "comment-1", nil
}

func (a *daemonMemoryTrackerAdapter) AgentToolSpecs() []tracker.ToolSpec { return nil }

func (a *daemonMemoryTrackerAdapter) SecretEnvironmentNames() []string {
	return []string{"LINEAR_API_KEY"}
}

func (a *daemonMemoryTrackerAdapter) ExecuteAgentTool(_ context.Context, name string, _ map[string]any, _ tracker.ToolContext) tracker.ToolResult {
	return tracker.ToolResult{Success: true, Output: map[string]any{"tool": name}}
}

func (d *Daemon) setupSymphony() error {
	if d.config.Symphony == nil {
		return nil
	}
	symCfg := d.config.Symphony
	adapter := symCfg.Adapter
	if adapter == nil {
		var err error
		if adapter, err = buildDaemonTrackerAdapter(symCfg.ServiceConfig); err != nil {
			return err
		}
	}
	if err := validateEmbeddedLinear(symCfg.ServiceConfig); err != nil {
		return err
	}
	if err := d.store.RequireScheduleAuthority(d.ctx, symCfg.ServiceConfig.Fae.AuthorizingMessage); err != nil {
		return fmt.Errorf("Linear poller authority: %w", err)
	}
	bridge := d.LinearTaskBridge()
	bridge.config = symCfg.ServiceConfig.Fae
	var manager *symphony.ConfigManager
	if symCfg.WorkflowPath != "" {
		var err error
		manager, err = symphony.NewConfigManager(symCfg.WorkflowPath)
		if err != nil {
			return err
		}
	}
	orch := NewLinearOrchestrator(OrchestratorConfig{
		Adapter: adapter, ServiceConfig: symCfg.ServiceConfig, PromptTemplate: symCfg.PromptTemplate,
		Bridge: bridge, ConfigManager: manager, AdapterFactory: buildDaemonTrackerAdapter, EnableRetryTimers: true,
	})
	if err := orch.ValidateDispatchConfig(); err != nil {
		return err
	}
	d.linearOrch = orch
	if symCfg.ServiceConfig.Server.Port != nil {
		host := symCfg.ServiceConfig.Server.Host
		if strings.TrimSpace(host) == "" {
			host = "127.0.0.1"
		}
		srv := NewSymphonyHTTPServer(orch, host, *symCfg.ServiceConfig.Server.Port)
		if err := srv.Start(); err != nil {
			return err
		}
		d.symphonyHTTP = srv
	}
	return nil
}

func (d *Daemon) startSymphony() {
	if d.linearOrch == nil {
		return
	}
	d.workers.Add(1)
	go func() {
		defer d.workers.Done()
		_ = d.linearOrch.RunLoop(d.ctx, false)
	}()
}

func (d *Daemon) stopSymphony(ctx context.Context) {
	if d.symphonyHTTP != nil {
		_ = d.symphonyHTTP.Close(ctx)
	}
}
