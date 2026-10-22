package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/controller"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/cron"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/external"
	"github.com/xleelz/fae/loops"
	agentruntime "github.com/xleelz/fae/runtime"
	"github.com/xleelz/fae/workflow"
)

type Config struct {
	Scratch             bool // Creation-only; existing scratch identity survives ordinary restarts.
	ProphetRoutes       []workflow.Binding
	ProphetMaxFailovers int // zero defaults to one; negative disables failover

	ProphetProvider     string
	ProphetModel        string
	ProphetRunner       func(context.Context, string, json.RawMessage) (json.RawMessage, error) `json:"-"`
	PrincipalConfig     string                                                                  // owner-only JSON array of delegated credential bindings
	EventRetentionAge   time.Duration
	EventRetentionCount int

	MessageMaxAge time.Duration // zero selects the 24-hour default
	LoopRunner    loops.Runner  `json:"-"`
	// ConfigureIdleSessions is an opt-in trusted adapter hook. The adapter owns
	// admission, gated process registration, and Begin/End for every reused turn.
	// The built-in one-shot bridge requires no persistent panes and leaves it nil.
	ConfigureIdleSessions                        func(context.Context, *agentruntime.OwnedIdleSessions, string) error `json:"-"`
	Root, Instance, Workspace, DoltBinary, RunID string
	Port, MaxInFlight                            int
	Executor                                     engine.Executor
	Live                                         *LiveConfig
}
type Daemon struct {
	replicationScheduleMu sync.Mutex
	backupScheduleMu      sync.Mutex
	retentionScheduleMu   sync.Mutex
	backupMu              sync.Mutex
	reviewProvisionMu     sync.Mutex
	backlogUncertain      bool
	prophetActive         map[string]bool
	readPool              readPool

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
	driveRounds            atomic.Uint64
	lastTransition         atomic.Int64
	crons                  map[string]cron.Schedule
	loopPriority           bool
	loops                  map[string]loops.Schedule
	loopActive             map[string]context.CancelFunc
	loopResults            chan loopCompletion
	idleSessions           *agentruntime.OwnedIdleSessions
	draining               bool
	live                   *liveExecutor
	workers                sync.WaitGroup
	config                 Config
	layout                 Layout
	lock                   *instanceLock
	log                    *os.File
	child                  *controlplane.Server
	childMu                sync.RWMutex
	store                  *controlplane.Store
	rpc                    *api.Server
	ctx                    context.Context
	childCtx               context.Context
	childCancel            context.CancelFunc
	cancel                 context.CancelFunc
	mu                     sync.Mutex
	jobs                   map[string]controlplane.JobSnapshot
	active                 map[string]bool
	executor               engine.Executor
	wake                   chan struct{}
	results                chan completion
	done                   chan struct{}
	stopOnce               sync.Once
	closeOnce              sync.Once
	err                    error
	started                time.Time
	reads                  chan struct{}
	status                 atomic.Value
	eventMu                sync.Mutex
	eventWake              chan struct{}
	subscribers            chan struct{}
}
type completion struct {
	id, dispatch string
	result       engine.Result
	err          error
}
type envelope struct {
	Job       engine.Job  `json:"job"`
	Admission AdmitParams `json:"admission"`
}

func Start(ctx context.Context, c Config) (d *Daemon, err error) {
	if _, err = prophetRoutes(c); err != nil {
		return nil, err
	}
	if c.EventRetentionAge < 0 || c.EventRetentionCount < 0 {
		return nil, errors.New("event retention must be positive or zero for defaults")
	}
	safe, probeErr := controller.Unsandboxed(os.Getpid())
	if probeErr != nil || !safe {
		return nil, errors.Join(probeErr, errors.New("daemon startup requires positively unsandboxed host runtime"))
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
	if c.MaxInFlight == 0 {
		c.MaxInFlight = 4
	}
	if c.MaxInFlight < 1 || c.MaxInFlight > 256 {
		return nil, errors.New("max in flight must be 1..256")
	}
	l, err := Paths(c.Root, c.Instance)
	if err != nil {
		return nil, err
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
	ownctx, cancel := context.WithCancel(context.Background())
	childctx, childcancel := context.WithCancel(context.Background())
	d = &Daemon{binaryPath: binaryPath, binaryHash: binaryHash, config: c, layout: l, lock: lock, ctx: ownctx, cancel: cancel, childCtx: childctx, childCancel: childcancel, jobs: map[string]controlplane.JobSnapshot{}, active: map[string]bool{}, wake: make(chan struct{}, 1), results: make(chan completion, c.MaxInFlight), done: make(chan struct{}), started: time.Now().UTC(), reads: make(chan struct{}, 8), executor: c.Executor, eventWake: make(chan struct{}), subscribers: make(chan struct{}, 16)}
	owned := d
	defer func() {
		if err != nil {
			cancel()
			childcancel()
			if owned.idleSessions != nil {
				cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
				_ = owned.idleSessions.Close(cleanup)
				stop()
			}
			cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
			err = errors.Join(err, owned.releasePaneConfiguration(cleanup))
			stop()
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
	d.log, err = os.OpenFile(l.Log, os.O_CREATE|os.O_APPEND|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
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
	if safe, probeErr = controller.Unsandboxed(d.child.PID()); probeErr != nil || !safe {
		return nil, errors.Join(probeErr, errors.New("Dolt inherited sandbox; startup refused"))
	}
	if lock.stale {
		if err = d.store.PutMarker(ctx, controlplane.Audit{Actor: "daemon", Action: "reclaim dead owner", Outcome: "ok"}, "owner-reclaimed", json.RawMessage(`true`)); err != nil {
			return nil, err
		}
	}
	if c.Live != nil {
		if c.Executor != nil {
			return nil, errors.New("live config and custom executor are mutually exclusive")
		}
		if err = d.setupLive(ctx); err != nil {
			return nil, err
		}
	}
	if d.executor == nil {
		d.executor = engine.NewScripted(defaults.HappyScript())
	}
	if err = d.installDefault(ctx); err != nil {
		return nil, err
	}
	marker, _ := d.store.GetMarker(ctx, "clean-stop")
	graceful := string(marker) == "true"
	snapshots, err := d.store.ListJobs(ctx, 100000)
	if err != nil {
		return nil, err
	}
	for _, snapshot := range snapshots {
		var e envelope
		if err = decodeEnvelope(snapshot.Payload, &e); err != nil {
			return nil, fmt.Errorf("invalid durable job %s: %w", snapshot.ID, err)
		}
		if err = engine.Validate(e.Job); err != nil {
			return nil, err
		}
		if d.live != nil && d.live.guard.panes != nil && !e.Job.State.Terminal() {
			if err = validatePaneFloor(e.Job.Document, d.live.guard.effectivePaneLimit()); err != nil {
				return nil, fmt.Errorf("durable job %s: %w", snapshot.ID, err)
			}
		}
		if (d.live != nil) != (e.Job.ExecutionKind == "live") {
			return nil, errors.New("stored job executor kind differs from daemon mode")
		}
		if e.Job.TaskID != snapshot.ID || int64(e.Job.Revision) != snapshot.WorkflowRevision || e.Job.Document.ID != snapshot.WorkflowID || string(e.Job.State) != snapshot.State || (e.Admission.TargetConfig == "" && snapshot.TargetID != c.Workspace) || (e.Admission.TargetConfig != "" && (snapshot.TargetID != e.Admission.TargetConfig || snapshot.TargetRevision != e.Admission.TargetRevision)) {
			return nil, errors.New("durable job binding mismatch")
		}
		targetCtx := ctx
		if e.Job.State == "Retired" {
			targetCtx = context.WithValue(ctx, retiredTargetRecoveryKey{}, true)
		}
		if err = d.checkPinnedTarget(targetCtx, e.Admission, e.Job.Document); err != nil {
			return nil, fmt.Errorf("durable target %s: %w", snapshot.ID, err)
		}
		if e.Job.InFlight != nil {
			_, safe := d.executor.(*engine.Scripted)
			recovered, _, recoverErr := engine.Recover(e.Job, graceful, safe)
			if recoverErr != nil {
				return nil, recoverErr
			}
			recoveryEvents := recovered.History[len(e.Job.History):]
			e.Job = recovered
			snapshot, err = d.persist(ctx, snapshot, e, recoveryEvents)
			if err != nil {
				return nil, err
			}
		}
		d.jobs[snapshot.ID] = snapshot
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
	d.rpc, err = api.New(token, d.handle)
	if err != nil {
		return nil, err
	}
	if err = installPrincipals(d.rpc, c.PrincipalConfig); err != nil {
		return nil, err
	}
	d.rpc.SetEventStream(d.stream)
	if err = d.rpc.Listen(l.Socket); err != nil {
		return nil, err
	}
	binding, _ := json.Marshal(struct {
		Workspace string `json:"workspace"`
	}{c.Workspace})
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
	d.workers.Add(1)
	go d.maintainRetentionSchedule()
	d.workers.Add(1)
	go d.maintainBackupSchedule()
	d.workers.Add(1)
	go d.maintainReplicationSchedule()
	d.driveRearm = make(chan struct{}, 1)
	go func() {
		if e := d.rpc.Serve(); e != nil {
			d.fail(e)
		}
	}()
	go d.drive()
	go func() {
		select {
		case <-ctx.Done():
			d.cancel()
		case <-d.done:
		}
	}()
	d.signal()
	return d, nil
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
	closectx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return d.Close(closectx)
}
func (d *Daemon) Close(ctx context.Context) error {
	d.cancel()
	select {
	case <-d.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	workersDone := make(chan struct{})
	go func() { d.workers.Wait(); close(workersDone) }()
	select {
	case <-workersDone:
	case <-ctx.Done():
		return ctx.Err()
	}
	d.closeOnce.Do(func() {
		_ = d.rpc.Shutdown(ctx)
		if d.live != nil && d.live.resident != nil {
			d.err = errors.Join(d.err, d.live.resident.Close(ctx))
		}
		if d.idleSessions != nil {
			d.err = errors.Join(d.err, d.idleSessions.Close(ctx))
		}
		d.err = errors.Join(d.err, d.releasePaneConfiguration(ctx))
		if d.err == nil {
			if err := d.store.PutMarker(ctx, controlplane.Audit{Actor: "daemon", Action: "stopped", Outcome: "ok"}, "clean-stop", json.RawMessage(`true`)); err != nil {
				d.err = err
			}
		}
		_ = d.store.Close()
		_ = d.child.Stop()
		d.childCancel()
		_ = os.Remove(filepath.Join(d.layout.Run, "pid.json"))
		_ = d.log.Close()
		d.lock.close()
	})
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.err
}
func (d *Daemon) Layout() Layout { return d.layout }
