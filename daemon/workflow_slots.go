package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/hostguard"
	"github.com/xleelz/fae/processowner"
	"github.com/xleelz/fae/scheduler"
	"github.com/xleelz/fae/workflow"
	"math"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

type workflowRequestInfo struct {
	Limit     int       `json:"limit"`
	Principal string    `json:"principal"`
	Message   string    `json:"authorizing_message"`
	At        time.Time `json:"at"`
}

type workflowSlotWait struct {
	Memory  int64
	Panes   int
	Reason  string
	Since   time.Time
	Alerted bool
}
type workflowSlotManager struct {
	usageTimer        *time.Timer
	birth             string
	roundLedgerFailed bool
	mu                sync.RWMutex
	requestInfo       workflowRequestInfo
	ledger            *scheduler.Ledger
	owner             scheduler.Owner
	instance          string
	request           int
	held              map[string]scheduler.WorkflowHolder
	wait              map[string]workflowSlotWait
	waitTasks         map[string]string
	timer             *time.Timer
	lastUsage         time.Time
}

func (d *Daemon) slotNow() time.Time {
	if d.config.WorkflowSlotNow != nil {
		return d.config.WorkflowSlotNow().UTC()
	}
	return time.Now().UTC()
}

func runningSlotState(s domain.State) bool {
	return s != domain.Pending && s != domain.Escalated && !s.Terminal()
}
func (d *Daemon) deriveJobFootprint(ctx context.Context, j *engine.Job) error {
	defaultMax := j.Document.MaxSessions
	if defaultMax == 0 {
		defaultMax = -1
	}
	if j.MaxSessions == defaultMax {
		pinned, err := d.storedFootprint(ctx, j.Document.ID, int64(j.Revision))
		if err == nil {
			j.Footprint = pinned
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}

	f, err := workflow.DeriveFootprint(j.Document, func(ref workflow.WorkflowRef) (workflow.Document, error) {
		if ref.ID == "self" {
			return j.Document, nil
		}
		_, raw, e := d.storedWorkflow(ctx, ref.ID, int64(ref.Revision))
		if e != nil {
			return workflow.Document{}, e
		}
		return d.parseWorkflow(raw)
	}, j.MaxSessions)
	if err == nil {
		j.Footprint = &f
	}
	return err
}
func (d *Daemon) slotSpec(ctx context.Context, e envelope) (scheduler.WorkflowHolder, error) {
	manager := d.workflowSlots
	if manager == nil {
		return scheduler.WorkflowHolder{}, errors.New("workflow ledger not ready")
	}
	owner := e
	seen := map[string]bool{}
	for owner.Job.Parent != nil {
		id := owner.Job.Parent.Task
		if seen[id] {
			return scheduler.WorkflowHolder{}, errors.New("slot parent cycle")
		}
		seen[id] = true
		parent, err := d.taskEnvelope(ctx, id)
		if err != nil {
			return scheduler.WorkflowHolder{}, err
		}
		owner = *parent
	}
	if owner.Job.Footprint == nil {
		if err := d.deriveJobFootprint(ctx, &owner.Job); err != nil {
			return scheduler.WorkflowHolder{}, err
		}
	}
	scope := owner.Job.Footprint.SlotScope
	id := manager.instance + "/molecule/" + owner.Job.Document.ID + "/" + strconv.Itoa(owner.Job.Revision)
	if scope == "tree" {
		id = manager.instance + "/tree/" + engine.RootTask(owner.Job)
	}
	config := hostguard.DefaultConfig()
	if d.live != nil && d.live.guard != nil {
		config = d.live.guard.hostConfig()
	}
	n := owner.Job.Footprint.EffectiveSessions
	// A detached admission is not part of the pinned SubWorkflow graph. For
	// unbounded trees reserve each live detached subtree in addition to that
	// graph before making its first task dispatchable. Keep old commitments
	// until holder release, so finishing one child cannot fund another twice.
	dynamic := false
	detachedSessions := map[string]int{}
	activeAncestors := map[string]bool{}
	for _, snapshot := range d.jobs {
		var member envelope
		if decodeEnvelope(snapshot.Payload, &member) == nil && runningSlotState(member.Job.State) && member.Job.Reason != "WorkflowSlot" {
			for _, id := range member.Job.Ancestors {
				activeAncestors[id] = true
			}
		}
	}
	if owner.Job.MaxSessions <= 0 {
		seenTasks := map[string]bool{}
		addDetached := func(candidate envelope) error {
			if seenTasks[candidate.Job.TaskID] || !candidate.Job.Detached {
				return nil
			}
			if engine.RootTask(candidate.Job) != engine.RootTask(owner.Job) {
				if scope != "molecule" {
					return nil
				}
				root, err := d.taskEnvelope(ctx, engine.RootTask(candidate.Job))
				if err != nil {
					return err
				}
				if root.Job.Document.ID != owner.Job.Document.ID || root.Job.Revision != owner.Job.Revision {
					return nil
				}
			}
			seenTasks[candidate.Job.TaskID] = true
			if candidate.Job.TaskID != e.Job.TaskID && (!runningSlotState(candidate.Job.State) || candidate.Job.Reason == "WorkflowSlot") && !activeAncestors[candidate.Job.TaskID] {
				return nil
			}
			if candidate.Job.Footprint == nil {
				if err := d.deriveJobFootprint(ctx, &candidate.Job); err != nil {
					return err
				}
			}
			if candidate.Job.Footprint.SlotScope != scope {
				doc := candidate.Job.Document
				doc.SlotScope = scope
				footprint, err := workflow.DeriveFootprint(doc, func(ref workflow.WorkflowRef) (workflow.Document, error) {
					if ref.ID == "self" {
						return doc, nil
					}
					_, raw, e := d.storedWorkflow(ctx, ref.ID, int64(ref.Revision))
					if e != nil {
						return workflow.Document{}, e
					}
					return d.parseWorkflow(raw)
				}, -1)
				if err != nil {
					return err
				}
				candidate.Job.Footprint = &footprint
			}

			dynamic = true
			if candidate.Job.Footprint.EffectiveSessions > 1000000-n {
				return errors.New("detached footprint exceeds 1000000 sessions")
			}
			n += candidate.Job.Footprint.EffectiveSessions
			detachedSessions[candidate.Job.TaskID] = candidate.Job.Footprint.EffectiveSessions
			return nil
		}
		if err := addDetached(e); err != nil {
			return scheduler.WorkflowHolder{}, err
		}
		for _, snapshot := range d.jobs {
			var candidate envelope
			if err := decodeEnvelope(snapshot.Payload, &candidate); err != nil {
				return scheduler.WorkflowHolder{}, err
			}
			if err := addDetached(candidate); err != nil {
				return scheduler.WorkflowHolder{}, err
			}
		}
	}
	if e.Job.Parent != nil && runningSlotState(e.Job.State) && e.Job.SlotHolder != "" {
		if inherited, held := manager.held[e.Job.SlotHolder]; held && detachedCovered(inherited, detachedSessions) {
			inherited.Priority, inherited.Ready = e.Admission.Priority, e.AdmittedAt
			return inherited, nil
		}
		if e.Job.SlotBinding != nil && detachedCovered(d.bindingSpec(e, *e.Job.SlotBinding), detachedSessions) {
			return d.bindingSpec(e, *e.Job.SlotBinding), nil
		}
	}
	sessions := n
	panes := 0
	if d.live != nil && d.live.guard != nil && d.live.guard.panes != nil {
		panes = n
	}
	memory := int64(math.MaxInt64)
	if int64(n) <= math.MaxInt64/config.PerPane {
		memory = int64(n) * config.PerPane
	}
	if prior, held := manager.held[id]; held && prior.Workflow == owner.Job.Document.ID && prior.Revision == owner.Job.Revision && n <= prior.Sessions {
		memory = prior.Memory
		panes = prior.Panes
	}
	return scheduler.WorkflowHolder{ID: id, Instance: manager.instance, Scope: scope, Workflow: owner.Job.Document.ID, Revision: owner.Job.Revision, MoleculeLimit: workflow.MaxRunningMolecules(owner.Job.Document), Owner: manager.owner, Birth: manager.birth, Memory: memory, WaitingTask: func() string {
		if dynamic {
			return e.Job.TaskID
		}
		return ""
	}(), Sessions: sessions, BaseSessions: owner.Job.Footprint.EffectiveSessions, DetachedSessions: detachedSessions, Panes: panes, Priority: e.Admission.Priority, Ready: e.AdmittedAt, WaitSince: e.Job.SlotWaitSince, WaitReason: e.Job.SlotWaitReason}, nil
}
func (d *Daemon) slotCapacity(ctx context.Context) (scheduler.WorkflowCapacity, error) {
	cfg := hostguard.DefaultConfig()
	observe := d.config.WorkflowSlotSample
	pane := false
	if d.live != nil && d.live.guard != nil {
		guard := d.live.guard
		cfg = guard.hostConfig()
		if observe == nil {
			observe = guard.observe
		}
		pane = guard.panes != nil
	}
	if observe == nil {
		observe = hostguard.Observe
	}
	sample, err := observe(ctx)
	if pane && d.live.guard.inventory == nil {
		return scheduler.WorkflowCapacity{}, errors.New("pane inventory unavailable")
	}
	if pane && d.live.guard.inventory != nil {
		bounded, cancel := context.WithTimeout(ctx, time.Second)
		counts, e := d.live.guard.inventory(bounded)
		if e != nil {
			cancel()
			return scheduler.WorkflowCapacity{}, e
		}
		sample.LivePanes = 0
		for _, count := range counts {
			sample.LivePanes += count
		}
		limit, e := d.live.guard.panes.Limit(bounded)
		cancel()
		if e != nil {
			return scheduler.WorkflowCapacity{}, e
		}
		cfg.PaneLimit = limit
	}
	capacity := scheduler.WorkflowCapacity{Sample: sample, Config: cfg, PaneAdapter: pane, Now: d.slotNow(), InstanceMolecules: d.config.MaxMolecules, WorkflowMolecules: d.config.MaxWorkflowMolecules, ReferenceMemory: cfg.PerPane, ReferencePanes: 1}
	if d.live != nil && d.live.guard != nil {
		pressure, e := d.live.guard.pressure()
		if e != nil {
			return capacity, e
		}
		hold, _ := pressure.record(sample, cfg)
		capacity.PressureHold = hold.Outcome != hostguard.OutcomeProceed
	}
	// Total memory has its own measurement; available memory is not a total.
	capacity.TotalMemory, _ = hostguard.TotalMemory(ctx)
	if d.config.WorkflowSlotTotalMemory > 0 {
		capacity.TotalMemory = d.config.WorkflowSlotTotalMemory
	}

	revision, raw, referenceErr := d.store.GetWorkflow(ctx, "default", 0)
	if referenceErr != nil {
		return capacity, referenceErr
	}
	reference, referenceErr := d.storedFootprint(ctx, "default", revision.Number)
	if errors.Is(referenceErr, sql.ErrNoRows) {
		doc, parseErr := d.parseWorkflow(raw)
		if parseErr != nil {
			return capacity, parseErr
		}
		derived, deriveErr := d.resolveFootprint(ctx, doc, doc.MaxSessions)
		if deriveErr != nil {
			return capacity, deriveErr
		}
		reference = &derived
		referenceErr = nil
	}
	if referenceErr != nil {
		return capacity, referenceErr
	}
	capacity.ReferenceMemory = math.MaxInt64
	if int64(max(1, reference.EffectiveSessions)) <= math.MaxInt64/cfg.PerPane {
		capacity.ReferenceMemory = int64(max(1, reference.EffectiveSessions)) * cfg.PerPane
	}
	capacity.ReferencePanes = max(1, reference.EffectiveSessions)
	capacity.ReferenceSessions = max(1, reference.EffectiveSessions)

	return capacity, err
}

func (d *Daemon) setupWorkflowSlots(ctx context.Context) error {
	path := d.config.WorkflowLedgerPath
	if path == "" && d.config.Live != nil {
		path = d.config.Live.HostLedger
	}
	if path == "" {
		var err error
		path, err = scheduler.DefaultHostLedger()
		if err != nil {
			return err
		}
	}
	ledger, err := scheduler.OpenWorkflowLedger(path)
	if err != nil {
		return err
	}
	owner, err := scheduler.CurrentOwner()
	if err != nil {
		return err
	}
	birth, err := processowner.Birth(owner.PID)
	if err != nil {
		return err
	}
	info := workflowRequestInfo{}
	request := d.config.WorkflowLimit
	if request == 0 {
		request = -1
	}
	raw, readErr := d.store.GetMarker(ctx, "workflow-slot-request")
	if readErr != nil && !errors.Is(readErr, sql.ErrNoRows) {
		return readErr
	}
	if readErr == nil && len(raw) > 0 {
		if json.Unmarshal(raw, &info) == nil && info.Limit != 0 {
			request = info.Limit
		} else {
			_ = json.Unmarshal(raw, &request)
		}
	}
	if info.Limit == 0 {
		info = workflowRequestInfo{Limit: request, Principal: "configuration", At: time.Now().UTC()}
	}
	d.workflowSlots = &workflowSlotManager{ledger: ledger, owner: owner, birth: birth, instance: filepath.Clean(d.layout.Root), request: request, requestInfo: info, held: map[string]scheduler.WorkflowHolder{}, wait: map[string]workflowSlotWait{}, waitTasks: map[string]string{}}
	if readErr != nil {
		raw, _ := json.Marshal(info)
		err = ledger.CommitWorkflowRequest(ctx, d.workflowSlots.instance, owner, request, func() error {
			return d.store.SetWorkflowSlotRequest(ctx, controlplane.Audit{Actor: "configuration", Action: "workflow-slot-request", Outcome: "ok"}, raw)
		})
	} else {
		err = ledger.RegisterWorkflowRequest(ctx, d.workflowSlots.instance, owner, request)
	}
	if err != nil {
		return err
	}
	restore := map[string]scheduler.WorkflowHolder{}
	for _, snapshot := range d.jobs {
		var e envelope
		if decodeEnvelope(snapshot.Payload, &e) != nil || !runningSlotState(e.Job.State) || e.Job.Reason == "WorkflowSlot" || e.Job.SlotWaitReason != "" {
			continue
		}
		spec, err := d.restoreSlotSpec(ctx, e)
		if err != nil {
			return err
		}
		if old, exists := restore[spec.ID]; exists {
			spec.Memory = max(spec.Memory, old.Memory)
			spec.Panes = max(spec.Panes, old.Panes)
			spec.Sessions = max(spec.Sessions, old.Sessions)
			spec.BaseSessions = max(spec.BaseSessions, old.BaseSessions)
			spec.BaseMemory = max(spec.BaseMemory, old.BaseMemory)
			if spec.DetachedSessions == nil {
				spec.DetachedSessions = map[string]int{}
			}
			for id, n := range old.DetachedSessions {
				spec.DetachedSessions[id] = max(spec.DetachedSessions[id], n)
			}
		}
		restore[spec.ID] = spec
	}
	for _, spec := range restore {
		total := spec.BaseSessions
		for _, n := range spec.DetachedSessions {
			total += n
		}
		spec.Sessions = max(spec.Sessions, total)
		if err = ledger.ReestablishWorkflowSlot(ctx, spec); err != nil {
			return err
		}
		_, state, snapshotErr := ledger.WorkflowSlotSnapshot(ctx, scheduler.WorkflowCapacity{})
		if snapshotErr != nil {
			return snapshotErr
		}
		if actual, ok := state.Holders[spec.ID]; ok {
			spec = actual
		}
		d.workflowSlots.held[spec.ID] = spec
		if err = d.slotAudit(ctx, "workflow-slot-reestablished", spec.ID, "startup"); err != nil {
			return err
		}
	}
	return nil
}
func (d *Daemon) slotAudit(ctx context.Context, kind, id, reason string) error {
	raw, _ := json.Marshal(map[string]any{"holder": id, "reason": reason})
	return d.store.ControllerAction(ctx, controlplane.Audit{Actor: "daemon", Action: kind, Outcome: "ok"}, raw, false)
}
func (d *Daemon) slotWake() {
	m := d.workflowSlots
	if m == nil {
		return
	}
	if m.timer == nil {
		interval := d.config.WorkflowSlotRetryInterval
		if interval <= 0 {
			interval = 30 * time.Second
		}
		m.timer = time.AfterFunc(interval, func() { d.signal() })
	} else {
		interval := d.config.WorkflowSlotRetryInterval
		if interval <= 0 {
			interval = 30 * time.Second
		}
		m.timer.Reset(interval)
	}
}

// ensureWorkflowSlot is the single daemon acquisition barrier. All paths to
// dispatchability call it before writing the stage checkpoint or moving a tree.
func (d *Daemon) ensureWorkflowSlot(ctx context.Context, e *envelope) (scheduler.WorkflowSlotDecision, error) {
	var decision scheduler.WorkflowSlotDecision
	if d.workflowSlots == nil {
		decision.Granted = true
		return decision, nil
	}
	m := d.workflowSlots
	spec, err := d.slotSpec(ctx, *e)
	if err != nil {
		return scheduler.WorkflowSlotDecision{Reason: "host_ledger"}, err
	}
	e.Job.SlotHolder = spec.ID
	if held, ok := m.held[spec.ID]; ok && grantedWorkflowSlotCovers(held, spec) {
		applyGrantedWorkflowSlot(&e.Job, held)
		return scheduler.WorkflowSlotDecision{Granted: true, GrantedHolder: &held}, nil
	}
	if m.roundLedgerFailed {
		d.slotWake()
		return scheduler.WorkflowSlotDecision{Reason: "host_ledger"}, nil
	}
	capacity, sampleErr := d.slotCapacity(ctx)
	if sampleErr != nil {
		capacity.Sample.Memory.Known = false
	}
	decision, err = m.ledger.AcquireWorkflowSlot(ctx, spec, capacity)
	if err != nil {
		m.roundLedgerFailed = true
		decision.Reason = "host_ledger"
	}
	if decision.Granted {
		if spec.WaitingTask != "" {
			delete(m.waitTasks, spec.WaitingTask)
		}
		if decision.Repriced {
			if auditErr := d.slotAudit(ctx, "workflow-slot-repriced", spec.ID, ""); auditErr != nil {
				decision.Granted = false
				decision.Reason = "host_ledger"
				return decision, auditErr
			}
		}
		if _, held := m.held[spec.ID]; !held {
			if auditErr := d.slotAudit(ctx, "workflow-slot-acquired", spec.ID, ""); auditErr != nil {
				_ = m.ledger.ReleaseWorkflowSlot(ctx, spec.ID, m.owner)
				decision.Granted = false
				decision.Reason = "host_ledger"
				return decision, auditErr
			}
			m.held[spec.ID] = spec
		}
		if decision.GrantedHolder != nil {
			m.held[spec.ID] = *decision.GrantedHolder
		}
		granted := m.held[spec.ID]
		applyGrantedWorkflowSlot(&e.Job, granted)
		if waiting, exists := m.wait[spec.ID]; exists {
			actual := m.held[spec.ID]
			if decision.PendingWaiters == 0 && (decision.Repriced || actual.Memory >= waiting.Memory && actual.Panes >= waiting.Panes) {
				delete(m.wait, spec.ID)
				d.signal()
			}
		}
		e.Job.SlotWaitReason = ""
		e.Job.SlotWaitSince = time.Time{}
		if e.Job.Reason == "WorkflowSlot" {
			e.Job.Parked = false
			e.Job.Reason = ""
		}
		return decision, nil
	}
	if decision.Reason == "" {
		decision.Reason = "host_ledger"
	}
	if decision.Impossible {
		if spec.WaitingTask != "" {
			delete(m.waitTasks, spec.WaitingTask)
		}
		delete(m.wait, spec.ID)
		return decision, nil
	}
	if spec.WaitingTask != "" {
		if m.waitTasks == nil {
			m.waitTasks = map[string]string{}
		}
		m.waitTasks[spec.WaitingTask] = spec.ID
	}
	old, waiting := m.wait[spec.ID]
	if !waiting {
		old = workflowSlotWait{Since: decision.WaitSince}
		if old.Since.IsZero() {
			old.Since = d.slotNow()
		}
	}
	if old.Reason != decision.Reason {
		old.Reason = decision.Reason
		if decision.WaitChanged {
			_ = d.slotAudit(ctx, "workflow-slot-wait", spec.ID, decision.Reason)
		}
	}
	if !old.Alerted && d.slotNow().Sub(old.Since) >= 30*time.Minute {
		raw, _ := json.Marshal(map[string]any{"holder": spec.ID, "footprint": spec, "reason": decision.Reason, "uncommitted_memory": decision.UncommittedMemory, "uncommitted_panes": decision.UncommittedPanes, "holders": decision.Holders})
		_, alertErr := d.store.WorkflowSlotWaitAlert(ctx, spec.ID+"/"+old.Since.Format(time.RFC3339Nano), raw)
		if alertErr == nil {
			old.Alerted = true
		}
	}

	old.Memory, old.Panes = spec.Memory, spec.Panes
	m.wait[spec.ID] = old
	d.slotWake()
	e.Job.SlotWaitReason = decision.Reason
	e.Job.SlotWaitSince = old.Since
	if e.Job.State != domain.Pending {
		e.Job.Parked = true
		e.Job.Reason = "WorkflowSlot"
	}
	return decision, nil
}
func (d *Daemon) reconcileWorkflowSlots(ctx context.Context) error {
	m := d.workflowSlots
	if m == nil {
		return nil
	}
	if err := d.cleanupWorkflowWaitTasks(ctx); err != nil {
		return err
	}
	keep := map[string]map[string]int{}
	for _, snapshot := range d.jobs {
		var e envelope
		if decodeEnvelope(snapshot.Payload, &e) != nil || !runningSlotState(e.Job.State) || e.Job.Reason == "WorkflowSlot" {
			continue
		}
		spec, err := d.slotSpec(ctx, e)
		if err != nil {
			return err
		}
		if keep[spec.ID] == nil {
			keep[spec.ID] = map[string]int{}
		}
		for child, n := range spec.DetachedSessions {
			keep[spec.ID][child] = n
		}
	}
	for id, held := range m.held {
		release := false
		for child := range held.DetachedSessions {
			if _, live := keep[id][child]; !live {
				release = true
				break
			}
		}
		if release {
			updated, err := m.ledger.ReleaseDetachedCommitments(ctx, id, m.owner, keep[id])
			if err != nil {
				return err
			}
			m.held[id] = updated
			d.signal()
		}
	}
	running := map[string]bool{}
	wanted := map[string]bool{}
	for _, snapshot := range d.jobs {
		var e envelope
		if decodeEnvelope(snapshot.Payload, &e) != nil || e.Job.State.Terminal() || e.Job.State == domain.Escalated {
			continue
		}
		spec, err := d.slotSpec(ctx, e)
		if err != nil {
			return err
		}
		wanted[spec.ID] = true
		if !runningSlotState(e.Job.State) || e.Job.Reason == "WorkflowSlot" {
			continue
		}
		spec, err = d.slotSpec(ctx, e)
		if err != nil {
			return err
		}
		running[spec.ID] = true
	}
	for id := range m.wait {
		if !wanted[id] {
			if err := m.ledger.ReleaseWorkflowSlot(ctx, id, m.owner); err != nil {
				return err
			}
			delete(m.wait, id)
		}
	}
	for id, spec := range m.held {
		if !running[id] && d.pendingCheckpoint == nil {
			if err := m.ledger.ReleaseWorkflowSlot(ctx, id, m.owner); err != nil {
				return err
			}
			if err := d.slotAudit(ctx, "workflow-slot-released", id, ""); err != nil {
				return err
			}
			delete(m.held, id)
			d.signal()
		} else {
			_ = spec
		}

	}
	if len(m.held) > 0 && time.Since(m.lastUsage) >= 30*time.Second {
		d.publishWorkflowUsage(ctx)
		m.lastUsage = time.Now()
	}
	if len(m.wait) > 0 {
		d.slotWake()
	} else if m.timer != nil {
		m.timer.Stop()
	}
	if len(m.held) > 0 {
		d.scheduleWorkflowUsage()
	} else if m.usageTimer != nil {
		m.usageTimer.Stop()
	}
	return nil
}
func (d *Daemon) workflowSlotSnapshot(ctx context.Context) (any, error) {
	if d.workflowSlots == nil {
		return nil, errors.New("workflow ledger not ready")
	}
	capacity, err := d.slotCapacity(ctx)
	if err != nil {
		capacity.Sample.Memory.Known = false
	}
	decision, state, err := d.workflowSlots.ledger.WorkflowSlotSnapshot(ctx, capacity)
	m := d.workflowSlots
	m.mu.RLock()
	request, info := m.request, m.requestInfo
	m.mu.RUnlock()
	decision.BelowRequest = request > 0 && decision.Limit < request
	return map[string]any{"request": info, "requested": request, "host": decision, "holders": state.Holders, "waiters": state.Waiters, "requests": state.Requests, "instances": state.Instances}, err
}
func (d *Daemon) setWorkflowRequest(ctx context.Context, limit int, audit controlplane.Audit) (any, error) {
	if limit < -1 || limit == 0 || limit > 1000000 {
		return nil, errors.New("workflow limit must be -1 or 1..1000000")
	}
	m := d.workflowSlots
	if m == nil {
		return nil, errors.New("workflow ledger not ready")
	}
	info := workflowRequestInfo{Limit: limit, Principal: audit.Actor, Message: audit.MessageID, At: time.Now().UTC()}
	raw, _ := json.Marshal(info)
	err := m.ledger.CommitWorkflowRequest(ctx, m.instance, m.owner, limit, func() error { return d.store.SetWorkflowSlotRequest(ctx, audit, raw) })
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.request, m.requestInfo = limit, info
	m.mu.Unlock()
	d.signal()
	return d.workflowSlotSnapshot(ctx)
}

// publishWorkflowUsage credits only process trees with exact registry ownership.
// Missing observations remain zero commitments, never guessed free capacity.
func (d *Daemon) publishWorkflowUsage(ctx context.Context) {
	m := d.workflowSlots
	if m == nil {
		return
	}
	type usage struct {
		memory int64
		panes  int
	}
	totals := map[string]usage{}
	sessions := map[string]string{}
	proofs := map[string]string{}
	for _, snapshot := range d.jobs {
		var e envelope
		if decodeEnvelope(snapshot.Payload, &e) != nil {
			continue
		}
		spec, err := d.slotSpec(ctx, e)
		if err != nil {
			continue
		}
		if _, held := m.held[spec.ID]; !held {
			continue
		}
		for _, p := range e.Job.Provenance {
			if p.Session != "" {
				sessions[p.Session] = spec.ID
			}
		}
		for _, dispatch := range engine.BranchDispatches(e.Job) {
			for _, session := range dispatch.Sessions {
				sessions[session.ID] = spec.ID
			}
		}
		if e.Job.InFlight != nil {
			for _, session := range e.Job.InFlight.Sessions {
				sessions[session.ID] = spec.ID
			}
		}
	}
	if d.live != nil && d.live.guard != nil {
		for session, id := range sessions {
			proofs[d.live.guard.proofID(session)] = id
		}
	}
	records, recordErr := processowner.Inventory(filepath.Join(d.layout.Root, "sessions", ".processes"))
	commands, commandErr := processowner.Inventory(filepath.Join(d.layout.Run, "effect-processes"))
	records = append(records, commands...)
	var processes []processowner.Process
	var censusErr error
	if len(records) > 0 {
		processes, censusErr = processowner.Census()
	}
	owners := map[int]string{}
	if recordErr == nil && commandErr == nil && censusErr == nil {
		for _, record := range records {
			holder := sessions[record.Session]
			if holder == "" {
				continue
			}
			birth, err := processowner.Birth(record.PID)
			if err == nil && birth == record.Birth {
				owners[record.PID] = holder
			}
		}
		for step := 0; step < len(processes); step++ {
			changed := false
			for _, p := range processes {
				if owners[p.PID] == "" && owners[p.PPID] != "" {
					owners[p.PID] = owners[p.PPID]
					changed = true
				}
			}
			if !changed {
				break
			}
		}
		for _, p := range processes {
			if id := owners[p.PID]; id != "" {
				u := totals[id]
				u.memory += max(int64(0), p.RSS)
				totals[id] = u
			}
		}
	}
	if d.live != nil && d.live.guard != nil && d.live.guard.panes != nil && d.live.guard.inventory != nil {
		bounded, cancel := context.WithTimeout(ctx, time.Second)
		reservations, e := d.live.guard.panes.Snapshot(bounded)
		counts, e2 := d.live.guard.inventory(bounded)
		cancel()
		if e == nil && e2 == nil {
			for _, reservation := range reservations {
				if id := proofs[reservation.ID]; id != "" && counts[reservation.Socket] > 0 {
					u := totals[id]
					u.panes++
					totals[id] = u
				}
			}
		}
	}
	for id, spec := range m.held {
		u := totals[id]
		at := time.Now().UTC()
		if recordErr != nil || commandErr != nil || censusErr != nil {
			at = time.Time{}
		}
		_ = m.ledger.PublishWorkflowUsage(ctx, id, spec.Owner, u.memory, u.panes, at)
	}
}

func (d *Daemon) closeWorkflowSlots(ctx context.Context) error {
	m := d.workflowSlots
	if m == nil {
		return nil
	}
	if m.timer != nil {
		m.timer.Stop()
	}
	if m.usageTimer != nil {
		m.usageTimer.Stop()
	}
	var result error
	for id := range m.held {
		if err := m.ledger.ReleaseWorkflowSlot(ctx, id, m.owner); err != nil {
			result = errors.Join(result, err)
			continue
		}
		result = errors.Join(result, d.slotAudit(ctx, "workflow-slot-released", id, "shutdown"))
		delete(m.held, id)
	}
	for id := range m.wait {
		result = errors.Join(result, m.ledger.ReleaseWorkflowSlot(ctx, id, m.owner))
		delete(m.wait, id)
	}
	return errors.Join(result, m.ledger.UnregisterWorkflowInstance(ctx, m.instance, m.owner))
}

// Usage refresh is separate from spawn retry: a running holder with no slot
// waiters never causes a periodic drive round merely to poll acquisition.
func (d *Daemon) scheduleWorkflowUsage() {
	m := d.workflowSlots
	if m == nil {
		return
	}
	if m.usageTimer != nil {
		m.usageTimer.Reset(30 * time.Second)
		return
	}
	m.usageTimer = time.AfterFunc(30*time.Second, func() {
		select {
		case <-d.ctx.Done():
			return
		default:
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.ctx.Err() != nil || len(m.held) == 0 {
			return
		}
		d.publishWorkflowUsage(d.ctx)
		m.lastUsage = time.Now()
		m.usageTimer.Reset(30 * time.Second)
	})
}

func (d *Daemon) bindingSpec(e envelope, b engine.WorkflowSlotBinding) scheduler.WorkflowHolder {
	return scheduler.WorkflowHolder{ID: b.Holder, Instance: d.workflowSlots.instance, Scope: b.Scope, Workflow: b.Workflow, Revision: b.Revision, Sessions: b.Sessions, BaseSessions: b.BaseSessions, BaseMemory: b.BaseMemory, DetachedSessions: cloneDetachedSessions(b.DetachedSessions), Memory: b.Memory, Panes: b.Panes, Owner: d.workflowSlots.owner, Birth: d.workflowSlots.birth, Priority: e.Admission.Priority, Ready: e.AdmittedAt, WaitSince: e.Job.SlotWaitSince, WaitReason: e.Job.SlotWaitReason}
}
func (d *Daemon) restoreSlotSpec(ctx context.Context, e envelope) (scheduler.WorkflowHolder, error) {
	spec, err := d.slotSpec(ctx, e)
	if err != nil {
		return spec, err
	}
	if e.Job.SlotBinding != nil {
		spec = d.bindingSpec(e, *e.Job.SlotBinding)
	}
	parent := e.Job.Parent
	seen := map[string]bool{}
	for parent != nil && !seen[parent.Task] {
		seen[parent.Task] = true
		ancestor, err := d.taskEnvelope(ctx, parent.Task)
		if err != nil {
			return spec, err
		}
		if binding := ancestor.Job.SlotBinding; binding != nil && binding.Holder == spec.ID {
			if binding.Memory >= spec.Memory && binding.Panes >= spec.Panes && binding.Sessions >= spec.Sessions {
				spec.Workflow, spec.Revision = binding.Workflow, binding.Revision
			}
			spec.Memory = max(spec.Memory, binding.Memory)
			spec.Panes = max(spec.Panes, binding.Panes)
			spec.Sessions = max(spec.Sessions, binding.Sessions)
			spec.BaseSessions = max(spec.BaseSessions, binding.BaseSessions)
			spec.BaseMemory = max(spec.BaseMemory, binding.BaseMemory)
			if spec.DetachedSessions == nil {
				spec.DetachedSessions = map[string]int{}
			}
			for id, n := range binding.DetachedSessions {
				spec.DetachedSessions[id] = max(spec.DetachedSessions[id], n)
			}
		}
		parent = ancestor.Job.Parent
	}
	if d.live != nil && d.live.guard != nil && d.live.guard.panes != nil {
		spec.Panes = max(spec.Panes, spec.Sessions)
	}
	return spec, nil
}

func cloneDetachedSessions(in map[string]int) map[string]int {
	out := map[string]int{}
	for id, n := range in {
		out[id] = n
	}
	return out
}

func detachedCovered(held scheduler.WorkflowHolder, want map[string]int) bool {
	for id, n := range want {
		if held.DetachedSessions[id] < n {
			return false
		}
	}
	return true
}

func (d *Daemon) cleanupWorkflowWaitTasks(ctx context.Context) error {
	m := d.workflowSlots
	for task, holder := range m.waitTasks {
		snapshot, exists := d.jobs[task]
		var e envelope
		if exists && decodeEnvelope(snapshot.Payload, &e) == nil && !e.Job.State.Terminal() && e.Job.State != domain.Escalated {
			continue
		}
		if err := m.ledger.CancelWorkflowWaiter(ctx, holder, m.owner, task); err != nil {
			return err
		}
		delete(m.waitTasks, task)
		waiting := false
		for _, other := range m.waitTasks {
			if other == holder {
				waiting = true
				break
			}
		}
		if !waiting {
			for _, snapshot := range d.jobs {
				var member envelope
				if decodeEnvelope(snapshot.Payload, &member) == nil && !member.Job.State.Terminal() && member.Job.State != domain.Escalated && member.Job.SlotHolder == holder && member.Job.SlotWaitReason != "" {
					waiting = true
					break
				}
			}
		}
		if !waiting {
			delete(m.wait, holder)
		}
	}
	return nil
}
