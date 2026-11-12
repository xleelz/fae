package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xleelz/fae/processowner"
	"github.com/xleelz/fae/workflow"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/execution"
	"github.com/xleelz/fae/hostguard"
	agentruntime "github.com/xleelz/fae/runtime"
	"github.com/xleelz/fae/scheduler"
)

type liveAdmission struct {
	sharedWrites       sharedWriteGrants
	treeSessions       treeSessionGate
	daemon             *Daemon
	host               *scheduler.Ledger
	panes              *scheduler.PaneLedger
	paneLimit          int
	dynamicPaneLimit   atomic.Int64
	requestedPaneLimit atomic.Int64
	dynamicHeavy       atomic.Int64
	dynamicTasks       atomic.Int64
	dynamicHost        atomic.Pointer[hostguard.Config]
	inventory          func(context.Context) (map[string]int, error)
	owner              scheduler.Owner
	journal            *scheduler.Journal
	birth              string
	expectedProcesses  int
	observe            func(context.Context) (hostguard.Sample, error)
	// Pressure admission (FAE-HOST-002, -005, -010). Nil selects the system
	// clock and livePressurePolicy; tests inject a deterministic clock.
	clock         hostguard.Clock
	policy        *hostguard.Policy
	pressureOnce  sync.Once
	pressureState *hostPressure
	pressureErr   error
	// probe reports a foreign heavy-pool owner's liveness (FAE-HOST-031); nil
	// selects scheduler.ProbeOwner. owners caches its results.
	probe      func(scheduler.Owner) scheduler.Liveness
	owners     *scheduler.OwnerCache
	ownersOnce sync.Once
	// view is the drive's latest scheduling view.
	view atomic.Pointer[scheduleView]
	// hardAlerted is set while a hard-limit alert episode is open.
	hardAlerted atomic.Bool
	// waits and refusals classify withheld tasks (FAE-SCH-010): the stage a
	// task's admission waits in, and the stage of its last deferred refusal.
	waits    sync.Map
	refusals sync.Map
	// rolePools are the role pools this daemon used; held names the requests
	// an admission of this process still owns (FAE-ROB-015).
	rolePools sync.Map
	held      sync.Map
	// queueClock dates role and heavy queue advances; nil selects the system
	// clock. heavyTasks names the task of each of this daemon's heavy waiters.
	queueClock func() time.Time
	heavyTasks sync.Map
}

// livePressurePolicy supplies the pressure policy of every live admission that
// does not inject one. Production uses the requirement defaults.
var livePressurePolicy = hostguard.DefaultPolicy

// liveObserve samples host telemetry for every live admission that does not
// inject a sampler. Production reads the host.
var liveObserve = hostguard.Observe

// hostPressure is the hysteresis state shared by all admissions of one daemon:
// the soft pressure hold and the host-heavy I/O gate. Every sample admission
// takes is recorded in both at one serialized clock reading, so pressure seen
// by any launch also resets the I/O chain, and readings stay ordered in time.
type hostPressure struct {
	mu     sync.Mutex
	clock  hostguard.Clock
	policy hostguard.Policy
	hold   *hostguard.Hold
	gate   *hostguard.Hold
}

func (p *hostPressure) record(s hostguard.Sample, c hostguard.Config) (hold, gate hostguard.Step) {
	p.mu.Lock()
	defer p.mu.Unlock()
	at := p.clock.Now()
	return p.hold.Record(at, s, c), p.gate.Record(at, s, c)
}

func (a *liveAdmission) pressure() (*hostPressure, error) {
	a.pressureOnce.Do(func() {
		p := &hostPressure{clock: a.clock, policy: livePressurePolicy()}
		if p.clock == nil {
			p.clock = hostguard.SystemClock{}
		}
		if a.policy != nil {
			p.policy = *a.policy
		}
		if p.hold, a.pressureErr = hostguard.NewPressureHold(p.policy); a.pressureErr == nil {
			p.gate, a.pressureErr = hostguard.NewIOGate(p.policy)
		}
		a.pressureState = p
	})
	return a.pressureState, a.pressureErr
}

// hostHeavy names the primitives that pass the FAE-HOST-010 I/O gate before
// taking the heavy-op lease: host commands, commits and integration.
func hostHeavy(p workflow.Primitive) bool {
	return p == workflow.HostCommand || p == workflow.VcsCommit || p == workflow.Integrate
}

func newLiveAdmission(d *Daemon, c LiveConfig) (*liveAdmission, error) {
	l, e := scheduler.OpenDynamicLedger(c.HostLedger, c.HeavyCapacity)
	if e != nil {
		return nil, e
	}
	owner, e := scheduler.CurrentOwner()
	if e != nil {
		return nil, e
	}
	j, e := scheduler.OpenJournal(filepath.Join(d.layout.Run, "lease-proofs"))
	if e != nil {
		return nil, e
	}
	birth, e := processowner.Birth(owner.PID)
	if e != nil {
		return nil, e
	}
	a := &liveAdmission{daemon: d, host: l, owner: owner, journal: j, birth: birth, expectedProcesses: 1}
	registered, complete := false, false
	defer func() {
		if registered && !complete {
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = a.panes.Unconfigure(cleanup, d.layout.Root, a.owner, a.birth)
		}
	}()
	if c.Adapter == "multiplexer" {
		a.expectedProcesses = 2
		a.paneLimit = c.PaneLimit
		if a.paneLimit == 0 {
			a.paneLimit = 60
		}
		a.paneLimit, e = d.runtimePaneLimit(context.Background(), a.paneLimit)
		if e != nil {
			return nil, e
		}
		if a.paneLimit < 2 {
			return nil, fmt.Errorf("pane limit %d must exceed workflow-global floor 1", a.paneLimit)
		}
		a.panes, e = scheduler.OpenPaneLedger(filepath.Join(c.HostLedger, "panes"), a.paneLimit)
		if e != nil {
			return nil, e
		}
		if e = a.panes.Configure(context.Background(), d.layout.Root, a.paneLimit); e != nil {
			return nil, e
		}
		registered = true
		effective, err := a.panes.Limit(context.Background())
		if err != nil {
			return nil, err
		}
		a.dynamicPaneLimit.Store(int64(effective))
		a.requestedPaneLimit.Store(int64(a.paneLimit))
		if _, e = a.panes.Snapshot(context.Background()); e != nil {
			return nil, e
		}
		a.inventory = func(ctx context.Context) (map[string]int, error) {
			return agentruntime.HerdrInventory(ctx, c.HerdrBinary)
		}
	}
	if e = a.recoverProofs(context.Background()); e != nil {
		return nil, e
	}
	complete = true
	return a, nil
}

type admissionDeferredError struct{ error }

func (admissionDeferredError) AdmissionDeferred() bool { return true }
func (e admissionDeferredError) Unwrap() error         { return e.error }

type errProviderPoolExhausted struct {
	PoolID string
	Reason string
}

func (e errProviderPoolExhausted) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("provider pool %s unavailable: %s", e.PoolID, e.Reason)
	}
	return fmt.Sprintf("provider pool %s at capacity", e.PoolID)
}

func (errProviderPoolExhausted) AdmissionDeferred() bool { return true }

func (a *liveAdmission) resolveCredentialIdentity(s engine.Session) string {
	if strings.TrimSpace(s.CredentialIdentity) != "" {
		return strings.TrimSpace(s.CredentialIdentity)
	}
	if a.daemon != nil && a.daemon.config.Live != nil && len(a.daemon.config.Live.Environment) > 0 {
		env := a.daemon.config.Live.Environment
		prov := strings.ToUpper(strings.TrimSpace(s.Route.Provider))
		if prov != "" {
			if v := strings.TrimSpace(env["FAE_CREDENTIAL_"+prov]); v != "" {
				return v
			}
		}
		if v := strings.TrimSpace(env["FAE_CREDENTIAL_IDENTITY"]); v != "" {
			return v
		}
		if v := strings.TrimSpace(env["CREDENTIAL_IDENTITY"]); v != "" {
			return v
		}
	}
	return "default"
}

func (a *liveAdmission) mirrorProviderPool(ctx context.Context, snap scheduler.ProviderPoolSnapshot) {
	if a.daemon == nil || a.daemon.store == nil || snap.Config.ID == "" || snap.Pool.Capacity <= 0 {
		return
	}
	_ = a.daemon.store.MirrorProviderPool(ctx, snap.Config.ID, snap.Pool)
}

// Heavy-op priority classes (FAE-SCH-011). They break ties between equal
// waits only; a higher class never overtakes an older waiter.
const (
	roleClass = iota
	controllerClass
	operatorClass
)

// agentSession names the primitives that launch agent sessions. A session is
// not a heavy op (§1.2): it passes host and pane admission and its role lease
// but takes no heavy-op capacity, so the heavy pool never caps concurrent
// sessions (FAE-SCH-004, §14).
func agentSession(p workflow.Primitive) bool {
	return p == workflow.AgentTurn || p == workflow.Review
}

// backoffRefusal names the refusals that back off (bounded) before refusing
// and parking (FAE-HOST-002, -006): soft pressure, panes at the limit and the
// per-pane memory reservation. Hard and configuration refusals stop at once.
func backoffRefusal(reason string) bool {
	return reason == "HostPressure" || reason == "PaneLimit" || reason == "MemoryReservation"
}

func (a *liveAdmission) schedule() *scheduleView {
	if v := a.view.Load(); v != nil {
		return v
	}
	return &scheduleView{}
}

func (a *liveAdmission) Acquire(ctx context.Context, d engine.Dispatch, b execution.Binding, s engine.Session) (func() error, error) {
	return a.acquire(ctx, d, b, s, hostHeavy(d.Config.Primitive), roleClass)
}

// AcquireHostIO admits a daemon-internal I/O-heavy operation (plane backup,
// replication, retention, cleanup, review workspaces) as a host-heavy command:
// the FAE-HOST-010 I/O gate, then the heavy-op lease at weight 1 in the
// controller class.
func (a *liveAdmission) AcquireHostIO(ctx context.Context, id string) (func() error, error) {
	ctx = context.WithValue(ctx, hostIOOperation{}, id)
	return a.acquire(ctx, engine.Dispatch{ID: id, TaskID: id, Config: workflow.Stage{Weight: 1}}, execution.Binding{}, engine.Session{ID: id}, true, controllerClass)
}

// acquire admits one launch. No wait holds a scarce resource it does not
// need (FAE-SCH-008): a leased operation backs off on host pressure and, when
// host-heavy, passes the I/O gate before any reservation; a role waiter holds
// no heavy-op unit and no pane; an agent session backs off on host and pane
// admission holding only its granted role slot; and a granted heavy-op lease
// is checked by one fresh reading, never held through a back-off.
func (a *liveAdmission) acquire(ctx context.Context, d engine.Dispatch, b execution.Binding, s engine.Session, ioGate bool, class int) (release func() error, admissionErr error) {
	stage := withheldHostGuard
	wait := func(reason string) {
		stage = reason
		a.waiting(d.TaskID, reason)
	}
	defer func() {
		a.waiting(d.TaskID, "")
		if admissionErr != nil {
			a.refused(d.TaskID, stage, admissionErr)
			admissionErr = admissionDeferredError{admissionErr}
		} else {
			a.refusals.Delete(d.TaskID)
		}
	}()

	// Register attribution before acquiring any role, pool, pane or heavy lease.
	sharedRelease, sharedErr := a.acquireSharedWrite(d, b, s)
	if sharedErr != nil {
		return nil, sharedErr
	}
	defer func() {
		if admissionErr != nil {
			_ = sharedRelease()
		} else {
			inner := release
			release = func() error {
				var innerErr error
				if inner != nil {
					innerErr = inner()
				}
				sharedErr := sharedRelease()
				return errors.Join(innerErr, sharedErr)
			}
		}
	}()
	wait("tree_limit")
	treeRelease, treeErr := a.acquireTree(ctx, d, s)
	if treeErr != nil {
		return nil, treeErr
	}
	defer func() {
		if admissionErr != nil {
			treeRelease()
		} else {
			inner := release
			release = func() error {
				defer treeRelease()
				if inner != nil {
					return inner()
				}
				return nil
			}
		}
	}()
	_, cfg, err := a.host.HostLimits(ctx)
	if err != nil {
		return nil, err
	}
	a.dynamicHost.Store(&cfg)
	if a.panes != nil {
		limit, err := a.panes.Limit(ctx)
		if err != nil {
			return nil, err
		}
		a.dynamicPaneLimit.Store(int64(limit))
		if err := validatePaneFloor(b.Document, limit); err != nil {
			return nil, err
		}
	}
	agent := agentSession(d.Config.Primitive)
	weight := max(d.Config.Weight, 1)
	id := a.proofID(s.ID)
	waitctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	pressure, err := a.pressure()
	if err != nil {
		return nil, err
	}
	observe := a.observe
	if observe == nil {
		observe = liveObserve
	}
	if a.journal != nil {
		if _, e := a.journal.Read(id); e == nil {
			return nil, errors.New("session already has durable admission proof")
		} else if !os.IsNotExist(e) {
			return nil, e
		}
	}
	// FAE-HOST-010: host-heavy commands pass the two-reading I/O gate before
	// the heavy-op lease is even requested. A gate refusal leaves no durable
	// reservation and no queue entry behind.
	gatePassed := false
	if !agent {
		wait(withheldHostGuard)
		if err := a.waitBeforeLease(waitctx, pressure, observe, id, s.ID, ioGate); err != nil {
			return nil, err
		}
		gatePassed = ioGate
	}
	if !CanEnqueueHeavyLease(ioGate, gatePassed) {
		return nil, &scheduler.AdmissionRefusal{Reason: "IOPressure"}
	}
	if a.journal != nil {
		expected := 0
		registry, processOwner := "", ""
		if agent {
			expected = a.expectedProcesses
			processOwner = filepath.Join(a.daemon.layout.Root, "sessions")
			registry = filepath.Join(processOwner, ".processes")
		}
		if hostHeavy(d.Config.Primitive) {
			expected = 1
			registry = filepath.Join(a.daemon.layout.Run, "effect-processes")
			processOwner = a.daemon.layout.Worktrees
		}
		if e := a.journal.Put(scheduler.OperationProof{ID: id, Session: s.ID, Instance: a.daemon.layout.Root, Owner: a.owner, Birth: a.birth, State: "reserved", Expected: expected, Registry: registry, ProcessOwner: processOwner}); e != nil {
			return nil, e
		}
	}
	request := scheduler.Request{ID: id, Owner: a.owner, Weight: weight, Class: class, Priority: a.schedule().priority[d.TaskID], Queued: time.Now().UTC()}
	audit := controlplane.Audit{Actor: "daemon", Action: "resource lease", Outcome: "ok"}
	roleKey, providerPoolKey, hostQueued, paneReserved, launchReserved, admitted := "", "", false, false, false, false
	defer func() {
		if !admitted {
			a.abandon(id, roleKey, providerPoolKey, hostQueued, paneReserved, launchReserved, audit)
		}
	}()
	// FAE-SCH-005 & FAE-SCH-015: any agent or Prophet session targeting a
	// pooled (provider, credential_identity) acquires its host-ledger provider
	// concurrency pool lease before serialized Dolt SQL role pool commits or
	// pane/launch reservations so concurrent dispatches in the same round
	// enter TryAcquireProviderPool without Dolt SQL commit staggering,
	// preserving strict FAE-SCH-003 ready-set ordering.
	if (agent || s.Role == "prophet") && strings.TrimSpace(s.Route.Provider) != "" && a.host != nil {
		credID := strings.TrimSpace(s.CredentialIdentity)
		if credID == "" {
			credID = a.resolveCredentialIdentity(s)
		}
		poolSnap, matched, matchErr := a.host.MatchProviderPool(waitctx, s.Route.Provider, credID)
		if matchErr != nil {
			return nil, matchErr
		}
		if matched {
			poolID := poolSnap.Config.ID
			now := time.Now().UTC()
			if poolSnap.Pause != nil && poolSnap.Pause.Paused {
				wait(withheldDispatchPaused)
				return nil, errProviderPoolExhausted{PoolID: poolID, Reason: "paused"}
			}
			if poolSnap.Quota != nil && poolSnap.Quota.Active(now) {
				wait(withheldProviderQuota)
				return nil, errProviderPoolExhausted{PoolID: poolID, Reason: "quota-exhausted"}
			}
			if a.journal != nil {
				proof, e := a.journal.Read(id)
				if e != nil {
					return nil, e
				}
				proof.ProviderPool = poolID
				if e = a.journal.Put(proof); e != nil {
					return nil, e
				}
			}
			providerPoolKey = poolID
			taskID := s.TaskID
			if taskID == "" {
				taskID = d.TaskID
			}
			priority := s.Priority
			if priority == 0 && taskID != "" {
				priority = a.schedule().priority[taskID]
			}
			admittedAt := s.Admitted
			if admittedAt.IsZero() {
				admittedAt = now
			}
			ppReq := scheduler.Request{
				ID:                 id,
				Owner:              a.owner,
				TaskID:             taskID,
				Weight:             1,
				Class:              roleClass,
				Priority:           priority,
				Admitted:           admittedAt,
				CutOffByQuota:      s.CutOffByQuota,
				CredentialIdentity: credID,
				Queued:             now,
				Unbounded:          true,
			}
			wait(withheldProviderPool)
			_, granted, ppErr := a.host.TryAcquireProviderPool(waitctx, poolID, ppReq, 25*time.Millisecond)
			if ppErr != nil {
				return nil, ppErr
			}
			if snaps, e := a.host.ProviderPoolsSnapshot(waitctx); e == nil {
				if s, ok := snaps[poolID]; ok {
					a.mirrorProviderPool(waitctx, s)
				}
			}
			if !granted {
				return nil, errProviderPoolExhausted{PoolID: poolID}
			}
		}
	}
	// FAE-SCH-005: the role slot is leased next, holding no pane or heavy op.
	// A cap of -1 is unbounded (FAE-WF-040): the session takes no role slot.
	role, hasRole := b.Document.Roles[s.Role]
	key, capacity := "", -1
	if hasRole {
		if key, capacity, err = a.rolePool(b.Document, d.TaskID, s.Role, role); err != nil {
			return nil, err
		}
	}
	if capacity != -1 {
		if err = a.daemon.store.EnsureLeasePool(waitctx, audit, controlplane.LeasePoolConfig{Key: key, Kind: "role", Capacity: capacity}); err != nil {
			return nil, err
		}
		a.rolePools.Store(key, struct{}{})
		if a.journal != nil {
			proof, e := a.journal.Read(id)
			if e != nil {
				return nil, e
			}
			proof.RolePool = key
			if e = a.journal.Put(proof); e != nil {
				return nil, e
			}
		}
		roleKey = key
		a.held.Store(id, key)
		wait(withheldCap)
		// FAE-SCH-008: a role waiter keeps its queue position until it is
		// granted (or cancelled), so the longest waiter is granted next; the
		// admission bound restarts at the grant.
		roleRequest := request
		roleRequest.Weight, roleRequest.Class, roleRequest.Unbounded = 1, roleClass, true
		if err = a.daemon.store.EnqueueLease(waitctx, audit, key, roleRequest); err != nil {
			return nil, err
		}
		roleWaitStart := time.Now()
		roleWaited, roleErr := a.awaitRole(ctx, audit, key, id)
		if roleWaited {
			a.traceLeaseWait(d, "role:"+s.Role, roleWaitStart, roleErr == nil)
		}
		if roleErr != nil {
			return nil, roleErr
		}
		var granted context.CancelFunc
		waitctx, granted = context.WithTimeout(ctx, 30*time.Minute)
		defer granted()
	}
	if !agent {
		wait(withheldHostGuard)
		if err = a.host.Enqueue(waitctx, request); err != nil {
			return nil, err
		}
		hostQueued = true
		heavyWaitStart := time.Now()
		a.waitingHeavy(id, d.TaskID)
		heavyWaited, heavyErr := a.awaitHeavy(waitctx, id)
		a.heavyTasks.Delete(id)
		if heavyWaited {
			a.traceLeaseWait(d, "heavy-op", heavyWaitStart, heavyErr == nil)
		}
		if heavyErr != nil {
			return nil, heavyErr
		}
		if err = a.checkUnderLease(waitctx, pressure, observe, id, s.ID); err != nil {
			return nil, err
		}
	} else {
		// FAE-HOST-001/-002/-005/-006: sample before every launch; back off
		// (bounded) on soft pressure, a pressure hold in force, panes at the
		// limit and the memory reservation; refuse at once on the hard breaker
		// (FAE-HOST-003) and on other non-pressure refusals.
		residentTurn := d.Config.Primitive != workflow.Review && a.daemon.config.Live != nil && a.daemon.config.Live.ResidentSessions
		reservation := scheduler.PaneReservation{ID: id, Owner: a.owner, Birth: a.birth, Socket: filepath.Join(a.daemon.layout.Root, "sessions", "pending-"+id)}
		wait(withheldHostGuard)
		step, checks, err := hostguard.Wait(waitctx, pressure.clock, pressure.policy, func(ctx context.Context, n int) (step hostguard.Step, err error) {
			switch {
			case a.panes != nil && !residentTurn:
				step, err = a.paneStep(ctx, pressure, observe, id, s.ID, n, reservation, &paneReserved)
			case residentTurn:
				step, err = a.residentStep(ctx, pressure, observe, id, s.ID, n)
			default:
				step, err = a.directStep(ctx, pressure, observe, id, s.ID, n, &launchReserved)
			}
			if step.Reason == "PaneLimit" {
				wait(withheldPaneLimit)
			} else {
				wait(withheldHostGuard)
			}
			return step, err
		})
		if err != nil {
			// The park names only infrastructure-deferred; the line names the
			// cause, such as an unreadable Herdr inventory (failing closed).
			a.logf("host admission failed gate=launch id=%s session=%q error=%q\n", id, s.ID, err.Error())
			return nil, err
		}
		if step.Outcome != hostguard.OutcomeProceed {
			return nil, a.refuse("pressure", id, s.ID, checks, step)
		}
	}
	if e := waitctx.Err(); e != nil {
		return nil, e
	}
	admitted = true
	roleDone, providerPoolDone, paneDone, hostDone, launchDone := roleKey == "", providerPoolKey == "", !paneReserved, !hostQueued, !launchReserved
	return a.checkedRelease(id, func() error {
		// The session is retired: once this release has been attempted, a role
		// lease it failed to free is stale and is swept by a later round.
		defer a.held.Delete(id)
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var errs []error
		if !launchDone {
			e := a.host.ReleaseLaunch(cleanup, id, a.owner)
			launchDone, errs = e == nil, append(errs, e)
		}
		if !providerPoolDone {
			e := a.host.ReleaseProviderPool(cleanup, providerPoolKey, id, a.owner, "session completed")
			if e == nil {
				if snaps, sErr := a.host.ProviderPoolsSnapshot(cleanup); sErr == nil {
					if s, ok := snaps[providerPoolKey]; ok {
						a.mirrorProviderPool(cleanup, s)
					}
				}
			}
			providerPoolDone, errs = e == nil, append(errs, e)
		}
		if !roleDone {
			e := a.daemon.store.ReleaseLease(cleanup, audit, roleKey, id, a.owner, "session completed")
			roleDone, errs = e == nil, append(errs, e)
		}
		if !paneDone {
			e := a.panes.Release(cleanup, id, a.owner, a.birth)
			paneDone, errs = e == nil, append(errs, e)
		}
		if !hostDone {
			e := a.host.Release(cleanup, id, a.owner, "operation completed")
			hostDone, errs = e == nil, append(errs, e)
		}
		return errors.Join(errs...)
	}), nil
}

// abandon withdraws an unadmitted launch. A queued request is cancelled, or
// released once granted (a grant never returns to the queue). The never-
// launched journal proof is removed only when every reservation is proven
// gone; otherwise it stays for recovery.
func (a *liveAdmission) abandon(id, roleKey, providerPoolKey string, hostQueued, paneReserved, launchReserved bool, audit controlplane.Audit) {
	cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	clean := true
	if paneReserved && a.panes.Release(cleanup, id, a.owner, a.birth) != nil {
		clean = false
	}
	if launchReserved && a.host.ReleaseLaunch(cleanup, id, a.owner) != nil {
		clean = false
	}
	if providerPoolKey != "" {
		cErr := a.host.CancelProviderPool(cleanup, providerPoolKey, id, a.owner, "admission cancelled")
		rErr := a.host.ReleaseProviderPool(cleanup, providerPoolKey, id, a.owner, "unlaunched admission cleanup")
		if rErr == nil {
			if snaps, sErr := a.host.ProviderPoolsSnapshot(cleanup); sErr == nil {
				if s, ok := snaps[providerPoolKey]; ok {
					a.mirrorProviderPool(cleanup, s)
				}
			}
		} else if cErr != nil {
			snaps, e := a.host.ProviderPoolsSnapshot(cleanup)
			if e == nil {
				if s, ok := snaps[providerPoolKey]; ok {
					clean = clean && !poolHolds(s.Pool, id, a.owner) && !poolWaits(s.Pool, id)
				} else {
					clean = false
				}
			} else {
				clean = false
			}
		}
	}
	if hostQueued && a.host.Cancel(cleanup, id, a.owner, "admission cancelled") != nil && a.host.Release(cleanup, id, a.owner, "unlaunched admission cleanup") != nil {
		p, e := a.host.Snapshot(cleanup)
		clean = clean && e == nil && !poolHolds(p, id, a.owner) && !poolWaits(p, id)
	}
	if roleKey != "" && a.daemon.store.CancelLease(cleanup, audit, roleKey, id, a.owner, "admission cancelled") != nil && a.daemon.store.ReleaseLease(cleanup, audit, roleKey, id, a.owner, "unlaunched admission cleanup") != nil {
		p, e := a.daemon.store.LeaseSnapshot(cleanup, roleKey)
		clean = clean && e == nil && !poolHolds(p, id, a.owner) && !poolWaits(p, id)
	}
	a.held.Delete(id)
	if clean && a.journal != nil {
		if p, e := a.journal.Read(id); e == nil && p.State == "reserved" && p.Owner == a.owner && p.Birth == a.birth {
			_ = a.journal.Remove(id)
		}
	}
}

// sweepRoleLeases reclaims, within one drive round, the role leases and
// queue entries of this daemon that no admission of this process owns any
// more: their session is gone with no turn in flight (FAE-ROB-015). A lease
// whose journal proof records a launch with unproven retirement is kept, as
// is any entry whose proof cannot be read.
func (a *liveAdmission) sweepRoleLeases(ctx context.Context) {
	if a.daemon == nil || a.daemon.store == nil {
		return
	}
	audit := controlplane.Audit{Actor: "daemon", Action: "stale role lease sweep", Outcome: "reclaimed"}
	stale := func(id string) bool {
		if _, owned := a.held.Load(id); owned {
			return false
		}
		if a.journal == nil {
			return true
		}
		p, e := a.journal.Read(id)
		return os.IsNotExist(e) || (e == nil && p.State != "launching")
	}
	a.rolePools.Range(func(k, _ any) bool {
		key := k.(string)
		p, e := a.daemon.store.LeaseSnapshot(ctx, key)
		if e != nil {
			a.logf("stale role lease sweep failed pool=%s error=%q\n", key, e.Error())
			return true
		}
		for _, h := range p.Holders {
			if h.Owner == a.owner && stale(h.ID) {
				if e = a.daemon.store.ReleaseLease(ctx, audit, key, h.ID, a.owner, "stale role lease: session gone"); e != nil {
					a.logf("stale role lease sweep failed pool=%s id=%s error=%q\n", key, h.ID, e.Error())
				}
			}
		}
		for _, w := range p.Waiting {
			if w.Owner == a.owner && stale(w.ID) {
				if e = a.daemon.store.CancelLease(ctx, audit, key, w.ID, a.owner, "stale role request: session gone"); e != nil {
					a.logf("stale role lease sweep failed pool=%s id=%s error=%q\n", key, w.ID, e.Error())
				}
			}
		}
		return true
	})
}

// sweepStaleLeases runs the FAE-ROB-015 sweep of a live daemon's round.
func (d *Daemon) sweepStaleLeases() {
	if d.live != nil && d.live.guard != nil {
		d.live.guard.sweepRoleLeases(d.ctx)
	}
}

func poolHolds(p scheduler.Pool, id string, owner scheduler.Owner) bool {
	for _, h := range p.Holders {
		if h.ID == id && h.Owner == owner {
			return true
		}
	}
	return false
}

func poolWaits(p scheduler.Pool, id string) bool {
	for _, r := range p.Waiting {
		if r.ID == id {
			return true
		}
	}
	return false
}

// awaitRole waits FIFO, ties by task priority, for the role slot.
func (a *liveAdmission) awaitRole(ctx context.Context, audit controlplane.Audit, key, id string) (waited bool, err error) {
	for {
		update, e := a.daemon.store.AdvanceLeases(ctx, audit, key, a.queueNow().UTC(), map[scheduler.Owner]scheduler.Liveness{a.owner: scheduler.Alive})
		if e != nil {
			return waited, e
		}
		if poolHolds(update.Pool, id, a.owner) {
			return waited, nil
		}
		if !poolWaits(update.Pool, id) {
			return waited, errors.New("role lease wait expired")
		}
		waited = true
		select {
		case <-ctx.Done():
			return waited, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// awaitHeavy waits FIFO for the heavy-op lease. Entries of a foreign owner
// proven dead by pid and start time are reclaimed (FAE-HOST-031); owners whose
// liveness is ambiguous keep their capacity.
func (a *liveAdmission) awaitHeavy(ctx context.Context, id string) (waited bool, err error) {
	pool, err := a.host.Snapshot(ctx)
	if err != nil {
		return false, err
	}
	for {
		observed := a.ownerLiveness(pool)
		_, events, err := a.host.Advance(ctx, a.queueNow().UTC(), observed)
		if err != nil {
			return waited, err
		}
		for _, event := range events {
			if event.Kind == "Waiting" {
				a.heavyWaiting(event)
			}
		}
		if pool, err = a.host.Snapshot(ctx); err != nil {
			return waited, err
		}
		if poolHolds(pool, id, a.owner) {
			return waited, nil
		}
		if !poolWaits(pool, id) {
			return waited, errors.New("host lease wait expired")
		}
		waited = true
		select {
		case <-ctx.Done():
			return waited, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// ownerLiveness observes the owners of a heavy pool through the shared
// scheduler.OwnerCache: this daemon is Alive, a foreign owner is Dead only when
// proven so, and ambiguous owners keep their capacity.
func (a *liveAdmission) ownerLiveness(p scheduler.Pool) map[scheduler.Owner]scheduler.Liveness {
	a.ownersOnce.Do(func() { a.owners = scheduler.NewOwnerCache(a.owner, a.probe) })
	return a.owners.Observe(p)
}

// waitBeforeLease is the bounded back-off of a leased operation before its
// lease request: soft pressure (FAE-HOST-002, -005) and, for host-heavy
// commands, the two-reading I/O gate (FAE-HOST-010). The hard breaker stops
// at once. Unreadable telemetry proceeds and records the gap (FAE-HOST-004).
func (a *liveAdmission) waitBeforeLease(ctx context.Context, pressure *hostPressure, observe func(context.Context) (hostguard.Sample, error), id, session string, ioGate bool) error {
	gate := "pressure"
	step, checks, err := hostguard.Wait(ctx, pressure.clock, pressure.policy, func(ctx context.Context, n int) (hostguard.Step, error) {
		sample, e := observe(ctx)
		if e != nil {
			return hostguard.Step{}, e
		}
		cfg := a.hostConfig()
		soft, io := pressure.record(sample, cfg)
		hard := a.hardBreaker(sample, cfg)
		if ioGate {
			if hard {
				io.Outcome, io.Reason = hostguard.OutcomeStop, "HostHardLimit"
			}
			a.logReading("io", id, session, n, io)
			if io.Outcome != hostguard.OutcomeProceed {
				gate = "io"
				return io, nil
			}
		}
		if hard {
			soft.Outcome, soft.Reason = hostguard.OutcomeStop, "HostHardLimit"
		}
		a.logReading("pressure", id, session, n, soft)
		gate = "pressure"
		return soft, nil
	})
	if err != nil {
		return err
	}
	if step.Outcome != hostguard.OutcomeProceed {
		return a.refuseIn(ctx, gate, id, session, checks, step)
	}
	return nil
}

// checkUnderLease is the FAE-HOST-001 reading of a leased operation under its
// granted lease. It never backs off holding the lease: the hard breaker stops,
// and pressure or a hold in force refuses and parks.
func (a *liveAdmission) checkUnderLease(ctx context.Context, pressure *hostPressure, observe func(context.Context) (hostguard.Sample, error), id, session string) error {
	sample, e := observe(ctx)
	if e != nil {
		return e
	}
	cfg := a.hostConfig()
	step, _ := pressure.record(sample, cfg)
	if a.hardBreaker(sample, cfg) {
		step.Outcome, step.Reason = hostguard.OutcomeStop, "HostHardLimit"
	}
	a.logReading("pressure", id, session, 0, step)
	if step.Outcome != hostguard.OutcomeProceed {
		return a.refuseIn(ctx, "pressure", id, session, 1, step)
	}
	return nil
}

// residentStep is one launch reading of a resident turn on its already
// reserved pane: host pressure and the hard breaker, no new reservation.
func (a *liveAdmission) residentStep(ctx context.Context, pressure *hostPressure, observe func(context.Context) (hostguard.Sample, error), id, session string, n int) (hostguard.Step, error) {
	sample, e := observe(ctx)
	if e != nil {
		return hostguard.Step{}, e
	}
	if a.inventory != nil {
		counts, e := a.inventory(ctx)
		if e != nil {
			return hostguard.Step{}, e
		}
		sample.LivePanes = 0
		for _, n := range counts {
			sample.LivePanes += n
		}
	}
	cfg := a.hostConfig()
	if a.paneLimit > 0 {
		cfg.PaneLimit = a.effectivePaneLimit()
	}
	decision := hostguard.AdmitResidentTurn(sample, cfg, true)
	if !decision.Hard {
		a.hardAlerted.Store(false)
	}
	soft, _ := pressure.record(sample, cfg)
	if !decision.Allowed && !backoffRefusal(decision.Reason) {
		soft.Outcome, soft.Reason = hostguard.OutcomeStop, decision.Reason
	} else if !decision.Allowed && soft.Outcome == hostguard.OutcomeProceed {
		soft.Outcome, soft.Reason = hostguard.OutcomeRefused, decision.Reason
	}
	a.logReading("pressure", id, session, n, soft)
	return soft, nil
}

// paneStep is one launch reading of an agent session that needs a new Herdr
// pane. The pane ledger samples again under its lock.
func (a *liveAdmission) paneStep(ctx context.Context, pressure *hostPressure, observe func(context.Context) (hostguard.Sample, error), id, session string, n int, r scheduler.PaneReservation, reserved *bool) (hostguard.Step, error) {
	cfg := a.hostConfig()
	cfg.PaneLimit = a.effectivePaneLimit()
	return a.reserveStep(ctx, pressure, observe, id, session, n, cfg, reserved, func(ctx context.Context, capture func(context.Context) (hostguard.Sample, error)) error {
		r.Reserved = time.Now().UTC()
		return a.panes.Reserve(ctx, r, a.inventory, capture, cfg, r.Reserved)
	})
}

// directStep is one launch reading of a Direct agent session. Its memory
// reservation is taken in the host-wide ledger, where every launch still in
// its warm-up counts as pending (FAE-HOST-006).
func (a *liveAdmission) directStep(ctx context.Context, pressure *hostPressure, observe func(context.Context) (hostguard.Sample, error), id, session string, n int, reserved *bool) (hostguard.Step, error) {
	cfg := a.hostConfig()
	return a.reserveStep(ctx, pressure, observe, id, session, n, cfg, reserved, func(ctx context.Context, capture func(context.Context) (hostguard.Sample, error)) error {
		now := time.Now().UTC()
		return a.host.ReserveLaunch(ctx, scheduler.LaunchReservation{ID: id, Owner: a.owner, Birth: a.birth, Reserved: now}, capture, cfg, now)
	})
}

// reserveStep takes one launch reading and, when it proceeds, the launch's
// durable reservation. No reservation is attempted while pressure or a hold
// is in force; pressure proven by the reservation's own reading enters the
// hold. Panes at the limit and the memory reservation back off.
func (a *liveAdmission) reserveStep(ctx context.Context, pressure *hostPressure, observe func(context.Context) (hostguard.Sample, error), id, session string, n int, cfg hostguard.Config, reserved *bool, reserve func(context.Context, func(context.Context) (hostguard.Sample, error)) error) (hostguard.Step, error) {
	sample, e := observe(ctx)
	if e != nil {
		return hostguard.Step{}, e
	}
	soft, _ := pressure.record(sample, cfg)
	if a.hardBreaker(sample, cfg) {
		soft.Outcome, soft.Reason = hostguard.OutcomeStop, "HostHardLimit"
	}
	if soft.Outcome != hostguard.OutcomeProceed {
		a.logReading("pressure", id, session, n, soft)
		return soft, nil
	}
	var reading hostguard.Sample
	seen := false
	e = reserve(ctx, func(ctx context.Context) (hostguard.Sample, error) {
		sample, e := observe(ctx)
		if e == nil {
			reading, seen = sample, true
		}
		return sample, e
	})
	if seen {
		soft, _ = pressure.record(reading, cfg)
	}
	var refusal *scheduler.AdmissionRefusal
	switch {
	case errors.As(e, &refusal) && backoffRefusal(refusal.Reason):
		soft.Outcome, soft.Reason = hostguard.OutcomeRefused, refusal.Reason
	case errors.As(e, &refusal):
		soft.Outcome, soft.Reason = hostguard.OutcomeStop, refusal.Reason
	case e != nil:
		return hostguard.Step{}, e
	default:
		*reserved = true
		soft.Outcome = hostguard.OutcomeProceed
	}
	a.logReading("pressure", id, session, n, soft)
	return soft, nil
}

// hardBreaker applies the FAE-HOST-003 hard circuit breaker to one reading. A
// reading clear of it ends the current alert episode.
func (a *liveAdmission) hardBreaker(s hostguard.Sample, c hostguard.Config) bool {
	hard := hostguard.Admit(s, c, 0, true).Hard
	if !hard {
		a.hardAlerted.Store(false)
	}
	return hard
}

// hostIOOperation marks the context of a daemon-internal host-heavy operation.
type hostIOOperation struct{}

// refuseIn is refuse for the leased paths that daemon-internal operations
// share. Such an operation has no task whose park pairs its refusal, so the
// refusal is its own instance event, and its line is written only after the
// event committed (FAE-OBS-006).
func (a *liveAdmission) refuseIn(ctx context.Context, gate, id, session string, checks int, s hostguard.Step) error {
	op, internal := ctx.Value(hostIOOperation{}).(string)
	if !internal || a.daemon == nil || a.daemon.store == nil {
		return a.refuse(gate, id, session, checks, s)
	}
	body, _ := json.Marshal(map[string]any{"operation": op, "gate": gate, "outcome": s.Outcome, "reason": s.Reason, "checks": checks})
	write, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if a.daemon.store.ControllerAction(write, controlplane.Audit{Actor: "daemon", Action: "host-admission-refused", Outcome: "deferred"}, body, false) == nil {
		a.daemon.notifyEvents()
		a.logRefusal(gate, id, session, checks, s)
	}
	return a.refusal(gate, id, session, s)
}

// refuse ends a bounded wait with a deferred refusal. A hard refusal parks
// the work and raises one alert per hard-pressure episode (FAE-HOST-003).
func (a *liveAdmission) refuse(gate, id, session string, checks int, s hostguard.Step) error {
	a.logRefusal(gate, id, session, checks, s)
	return a.refusal(gate, id, session, s)
}

func (a *liveAdmission) refusal(gate, id, session string, s hostguard.Step) error {
	if s.Outcome == hostguard.OutcomeStop && s.Reason == "HostHardLimit" && a.daemon != nil && a.daemon.store != nil && a.hardAlerted.CompareAndSwap(false, true) {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		body, _ := json.Marshal(map[string]any{"reason": s.Reason, "gate": gate, "id": id, "session": session, "memory_available": formatReading(s.Reading.Sample.Memory), "swap_total": formatReading(s.Reading.Sample.Swap)})
		if _, _, e := a.daemon.raiseAlert(cleanup, controlplane.Alert{Severity: "error", Source: "host-admission", Detail: body}, false); e != nil {
			a.hardAlerted.Store(false)
			a.logf("host admission alert failed reason=%s id=%s error=%q\n", s.Reason, id, e.Error())
		}
	}
	return &scheduler.AdmissionRefusal{Reason: s.Reason}
}

// roleKey names a role's lease pool in the instance plane.
func roleKey(workflowID, scope, role string) string {
	return fmt.Sprintf("role/%x", sha256.Sum256([]byte(workflowID+"\x00"+scope+"\x00"+role)))
}

// rolePool resolves a role's lease pool and cap (FAE-SCH-005, -006). A
// workflow-global pool is shared by every revision of the workflow at the cap
// of the newest revision with a non-terminal task. A task-bound pool belongs to
// the task's molecule, one per (workflow id, revision), so a freed slot takes
// the molecule's next waiting task. Work the drive does not schedule, such as
// loops, keeps a task-scoped pool.
func (a *liveAdmission) rolePool(doc workflow.Document, task, name string, role workflow.Role) (string, int, error) {
	v := a.schedule()
	if role.CapScope == "tree" {
		root := v.treeRoot[task]
		if root == "" {
			root = task
		}
		capacity, err := a.treeRoleCapacity(task, root, name, role.Cap)
		return roleKey("tree", root, name), capacity, err
	}
	if role.CapScope == "task" {
		return roleKey("task", task, name), role.Cap, nil
	}
	molecule, scheduled := v.molecule[task]
	if role.Scope == "workflow-global" {
		capacity, err := v.globalCap(doc.ID, name, molecule, v.revision[task], role.Cap)
		return roleKey(doc.ID, "global", name), capacity, err
	}
	if scheduled {
		return roleKey(doc.ID, "molecule\x00"+molecule, name), role.Cap, nil
	}
	return roleKey(doc.ID, task, name), role.Cap, nil
}

func (a *liveAdmission) BindPaneSocket(ctx context.Context, session, socket string) error {
	if a.panes == nil {
		return nil
	}
	return a.panes.Bind(ctx, a.proofID(session), a.owner, a.birth, socket)
}

// validatePaneFloor refuses a pane limit not above the workflow-global role
// slots plus one (FAE-SCH-012), naming both numbers. The sum saturates.
func validatePaneFloor(doc workflow.Document, limit int) error {
	floor := 1
	for _, role := range doc.Roles {
		if role.Scope == "workflow-global" {
			floor += min(max(role.Cap, 0), math.MaxInt32-floor)
		}
	}
	if limit <= floor {
		return fmt.Errorf("pane limit %d must exceed workflow-global slots plus one (%d)", limit, floor)
	}
	return nil
}

func (a *liveAdmission) effectivePaneLimit() int {
	if n := a.dynamicPaneLimit.Load(); n > 0 {
		return int(n)
	}
	return a.paneLimit
}

func (a *liveAdmission) hostConfig() hostguard.Config {
	if c := a.dynamicHost.Load(); c != nil {
		return *c
	}
	return hostguard.DefaultConfig()
}

func (a *liveAdmission) logf(format string, args ...any) {
	if a.daemon != nil && a.daemon.log != nil {
		fmt.Fprintf(componentLog(a.daemon.log, "admission"), format, args...)
	}
}

// logReading records each reading that a hold or the I/O gate evaluated, every
// back-off and stop, and every telemetry gap, as one structured line with the
// PSI full avg10/avg60 values and their directions (FAE-HOST-005, -004).
func (a *liveAdmission) logReading(gate, id, session string, check int, s hostguard.Step) {
	if !s.HeldBefore && s.Outcome == hostguard.OutcomeProceed && !s.Gap {
		return
	}
	r := s.Reading
	chain := "none"
	if !s.ChainStart.IsZero() {
		chain = s.ChainStart.UTC().Format(time.RFC3339Nano)
	}
	missing := strings.Join(hostguard.Missing(r.Sample), ",")
	if missing == "" {
		missing = "none"
	}
	a.logf("host admission reading gate=%s outcome=%s reason=%s id=%s session=%q check=%d level=%s held=%t lifted=%t chain_start=%s at=%s io_full_avg10=%s io_full_avg60=%s io_avg10_dir=%s io_avg60_dir=%s memory_full_avg10=%s memory_full_avg60=%s memory_avg10_dir=%s memory_avg60_dir=%s memory_available=%s swap_total=%s load_per_core=%s gap=%t missing=%s\n",
		gate, s.Outcome, reasonOrNone(s.Reason), id, session, check+1, s.Level, s.HeldBefore, s.Lifted, chain, r.At.UTC().Format(time.RFC3339Nano),
		formatReading(r.IO.Avg10), formatReading(r.IO.Avg60), r.IO.Dir10, r.IO.Dir60,
		formatReading(r.Memory.Avg10), formatReading(r.Memory.Avg60), r.Memory.Dir10, r.Memory.Dir60,
		formatReading(r.Sample.Memory), formatReading(r.Sample.Swap), formatReading(r.Sample.LoadPerCore), s.Gap, missing)
}

// logRefusal records the end of a bounded wait: outcome=waiting means the
// second clearing reading was still due; outcome=refused means pressure was
// still proven; outcome=stop means an immediate refusal without back-off.
func (a *liveAdmission) logRefusal(gate, id, session string, checks int, s hostguard.Step) {
	a.logf("host admission refused gate=%s outcome=%s reason=%s id=%s session=%q checks=%d level=%s\n", gate, s.Outcome, reasonOrNone(s.Reason), id, session, checks, s.Level)
}

func reasonOrNone(reason string) string {
	if reason == "" {
		return "none"
	}
	return reason
}

func formatReading(r hostguard.Reading) string {
	if !r.Known {
		return "unknown"
	}
	return strconv.FormatFloat(r.Value, 'f', 2, 64)
}
