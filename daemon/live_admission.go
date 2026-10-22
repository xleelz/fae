package daemon

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"github.com/xleelz/fae/processowner"
	"github.com/xleelz/fae/workflow"
	"os"
	"path/filepath"
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
func (a *liveAdmission) Acquire(ctx context.Context, d engine.Dispatch, b execution.Binding, s engine.Session) (release func() error, admissionErr error) {
	defer func() {
		if admissionErr != nil {
			admissionErr = admissionDeferredError{admissionErr}
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
	// Reserve weighted host capacity for every agent and command. This deliberately
	// applies the same pool to all launches; no live operation has an exemption.
	weight := d.Config.Weight
	if weight < 1 {
		weight = 1
	}
	id := fmt.Sprintf("%x", sha256.Sum256([]byte(a.daemon.layout.Root+"\x00"+s.ID)))
	if a.journal != nil {
		if _, e := a.journal.Read(id); e == nil {
			return nil, errors.New("session already has durable admission proof")
		} else if !os.IsNotExist(e) {
			return nil, e
		}
		expected := 0
		registry, processOwner := "", ""
		if d.Config.Primitive == workflow.AgentTurn || d.Config.Primitive == workflow.Review {
			expected = a.expectedProcesses
			processOwner = filepath.Join(a.daemon.layout.Root, "sessions")
			registry = filepath.Join(processOwner, ".processes")
		}
		if d.Config.Primitive == workflow.HostCommand || d.Config.Primitive == workflow.VcsCommit || d.Config.Primitive == workflow.Integrate {
			expected = 1
			registry = filepath.Join(a.daemon.layout.Run, "effect-processes")
			processOwner = a.daemon.layout.Worktrees
		}
		if e := a.journal.Put(scheduler.OperationProof{ID: id, Session: s.ID, Instance: a.daemon.layout.Root, Owner: a.owner, Birth: a.birth, State: "reserved", Expected: expected, Registry: registry, ProcessOwner: processOwner}); e != nil {
			return nil, e
		}
	}
	request := scheduler.Request{ID: id, Owner: a.owner, Weight: weight, Class: 0, Queued: time.Now().UTC()}
	waitctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	if err := a.host.Enqueue(waitctx, request); err != nil {
		return nil, err
	}
	paneReserved := false
	granted := false
	admitted := false
	defer func() {
		if !admitted {
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = a.host.Release(cleanup, id, a.owner, "unlaunched admission cleanup")
			_ = a.host.Cancel(cleanup, id, a.owner, "admission cancelled")
			if paneReserved {
				_ = a.panes.Release(cleanup, id, a.owner, a.birth)
			}
		}
	}()
	for {
		// A dead daemon does not prove its children are dead on every OS. Retain
		// foreign owners conservatively until process-tree reconciliation exists.
		observed := map[scheduler.Owner]scheduler.Liveness{a.owner: scheduler.Alive}
		_, events, err := a.host.Advance(waitctx, time.Now().UTC(), observed)
		if err != nil {
			return nil, err
		}
		for _, event := range events {
			if event.Kind == "Waiting" && a.daemon.log != nil {
				fmt.Fprintf(a.daemon.log, "host lease waiting id=%s wait=%s holders=%v\n", event.RequestID, event.Wait, event.Holders)
			}
		}
		pool, err := a.host.Snapshot(waitctx)
		if err != nil {
			return nil, err
		}
		for _, h := range pool.Holders {
			if h.ID == id && h.Owner == a.owner {
				granted = true
			}
		}
		if granted {
			break
		}
		queued := false
		for _, r := range pool.Waiting {
			if r.ID == id {
				queued = true
			}
		}
		if !queued {
			return nil, errors.New("host lease wait expired")
		}
		select {
		case <-waitctx.Done():
			return nil, waitctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	releaseHost := func() error {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var paneErr error
		if paneReserved {
			paneErr = a.panes.Release(cleanup, id, a.owner, a.birth)
		}
		return errors.Join(paneErr, a.host.Release(cleanup, id, a.owner, "operation completed"))
	}
	observe := a.observe
	if observe == nil {
		observe = hostguard.Observe
	}
	isAgent := d.Config.Primitive == workflow.AgentTurn || d.Config.Primitive == workflow.Review
	residentTurn := isAgent && d.Config.Primitive != workflow.Review && a.daemon.config.Live != nil && a.daemon.config.Live.ResidentSessions
	if a.panes != nil && isAgent && !residentTurn {
		cfg := a.hostConfig()
		cfg.PaneLimit = a.effectivePaneLimit()
		err = a.panes.Reserve(waitctx, scheduler.PaneReservation{ID: id, Owner: a.owner, Birth: a.birth, Socket: filepath.Join(a.daemon.layout.Root, "sessions", "pending-"+id), Reserved: time.Now().UTC()}, a.inventory, observe, cfg, time.Now().UTC())
		if err != nil {
			_ = releaseHost()
			return nil, err
		}
		paneReserved = true
	} else {
		sample, e := observe(waitctx)
		if e != nil {
			_ = releaseHost()
			return nil, e
		}
		if a.inventory != nil {
			counts, e := a.inventory(waitctx)
			if e != nil {
				_ = releaseHost()
				return nil, e
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
		decision := hostguard.Admit(sample, cfg, 0, true)
		if residentTurn {
			decision = hostguard.AdmitResidentTurn(sample, cfg, true)
		}
		if !decision.Allowed {
			_ = releaseHost()
			return nil, &scheduler.AdmissionRefusal{Reason: decision.Reason}
		}
	}
	if e := waitctx.Err(); e != nil {
		_ = releaseHost()
		return nil, e
	}
	role, hasRole := b.Document.Roles[s.Role]
	if !hasRole {
		admitted = true
		return a.checkedRelease(id, releaseHost), nil
	}
	scope := d.TaskID
	if role.Scope == "workflow-global" {
		scope = "global"
	}
	key := fmt.Sprintf("role/%x", sha256.Sum256([]byte(b.Document.ID+"\x00"+scope+"\x00"+s.Role)))
	audit := controlplane.Audit{Actor: "daemon", Action: "resource lease", Outcome: "ok"}
	if err = a.daemon.store.EnsureLeasePool(waitctx, audit, controlplane.LeasePoolConfig{Key: key, Kind: "role", Capacity: role.Cap}); err != nil {
		_ = releaseHost()
		return nil, err
	}
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
	request.Weight = 1
	if err = a.daemon.store.EnqueueLease(waitctx, audit, key, request); err != nil {
		_ = releaseHost()
		return nil, err
	}
	roleHeld := false
	defer func() {
		if !roleHeld {
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = a.daemon.store.ReleaseLease(cleanup, audit, key, id, a.owner, "unlaunched admission cleanup")
			_ = a.daemon.store.CancelLease(cleanup, audit, key, id, a.owner, "admission cancelled")
		}
	}()
	for {
		update, e := a.daemon.store.AdvanceLeases(waitctx, audit, key, time.Now().UTC(), map[scheduler.Owner]scheduler.Liveness{a.owner: scheduler.Alive})
		if e != nil {
			_ = releaseHost()
			return nil, e
		}
		held := false
		for _, h := range update.Pool.Holders {
			if h.ID == id && h.Owner == a.owner {
				held = true
			}
		}
		if held {
			roleHeld = true
			break
		}
		queued := false
		for _, r := range update.Pool.Waiting {
			if r.ID == id {
				queued = true
			}
		}
		if !queued {
			_ = releaseHost()
			return nil, errors.New("role lease wait expired")
		}
		select {
		case <-waitctx.Done():
			_ = releaseHost()
			return nil, waitctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	admitted = true
	return a.checkedRelease(id, func() error {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		first := a.daemon.store.ReleaseLease(cleanup, audit, key, id, a.owner, "session completed")
		second := releaseHost()
		return errors.Join(first, second)
	}), nil
}

func (a *liveAdmission) BindPaneSocket(ctx context.Context, session, socket string) error {
	if a.panes == nil {
		return nil
	}
	return a.panes.Bind(ctx, a.proofID(session), a.owner, a.birth, socket)
}
func validatePaneFloor(doc workflow.Document, limit int) error {
	floor := 1
	for _, role := range doc.Roles {
		if role.Scope == "workflow-global" {
			if role.Cap > limit-floor {
				return fmt.Errorf("pane limit %d must exceed workflow-global slots plus one (%d or more)", limit, floor+role.Cap)
			}
			floor += role.Cap
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
