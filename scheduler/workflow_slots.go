package scheduler

import (
	"context"
	"errors"
	"github.com/xleelz/fae/hostguard"
	"github.com/xleelz/fae/processowner"
	"math"
	"sort"
	"time"
)

type WorkflowHolder struct {
	BaseMemory       int64          `json:"base_memory_bytes,omitempty"`
	WaitingTask      string         `json:"waiting_task,omitempty"`
	Sessions         int            `json:"sessions,omitempty"`
	BaseSessions     int            `json:"base_sessions,omitempty"`
	DetachedSessions map[string]int `json:"detached_sessions,omitempty"`
	Birth            string         `json:"birth,omitempty"`
	WaitSince        time.Time      `json:"wait_since,omitempty"`
	WaitReason       string         `json:"wait_reason,omitempty"`
	IgnoreFIFO       bool           `json:"ignore_fifo,omitempty"`
	ID               string         `json:"id"`
	Instance         string         `json:"instance"`
	Scope            string         `json:"scope"`
	Workflow         string         `json:"workflow"`
	Revision         int            `json:"revision"`
	MoleculeLimit    int            `json:"molecule_limit,omitempty"`
	InstanceLimit    int            `json:"instance_limit,omitempty"`
	Owner            Owner          `json:"owner"`
	Memory           int64          `json:"memory_bytes"`
	Panes            int            `json:"panes"`
	Priority         int            `json:"priority"`
	Ready            time.Time      `json:"ready_at"`
	Resident         int64          `json:"resident_bytes"`
	LivePanes        int            `json:"live_panes"`
	Measured         time.Time      `json:"measured_at"`
}
type WorkflowRequest struct {
	Birth string `json:"birth,omitempty"`
	Limit int    `json:"limit"`
	Owner Owner  `json:"owner"`
}
type WorkflowInstance struct {
	Owner Owner  `json:"owner"`
	Birth string `json:"birth"`
}

type WorkflowSlotState struct {
	Instances map[string]WorkflowInstance `json:"instances"`
	Holders   map[string]WorkflowHolder   `json:"holders"`
	Waiters   map[string]WorkflowHolder   `json:"waiters"`
	Requests  map[string]WorkflowRequest  `json:"requests"`
}
type WorkflowCapacity struct {
	ReferenceSessions                    int
	Sample                               hostguard.Sample
	Config                               hostguard.Config
	PressureHold                         bool
	PaneAdapter                          bool
	ReferenceMemory                      int64
	ReferencePanes                       int
	TotalMemory                          int64
	InstanceMolecules, WorkflowMolecules int
	StaleAfter                           time.Duration
	Now                                  time.Time
}
type WorkflowSlotDecision struct {
	PendingWaiters           int              `json:"pending_waiters,omitempty"`
	GrantedHolder            *WorkflowHolder  `json:"granted_holder,omitempty"`
	Repriced                 bool             `json:"repriced,omitempty"`
	LegacyOwners             []Owner          `json:"legacy_owners,omitempty"`
	Detail                   string           `json:"detail,omitempty"`
	PaneAdapter              bool             `json:"pane_adapter"`
	AboveLimit               bool             `json:"above_limit"`
	WaitSince                time.Time        `json:"wait_since,omitempty"`
	WaitChanged              bool             `json:"wait_changed,omitempty"`
	Granted                  bool             `json:"granted"`
	Reestablished            bool             `json:"reestablished,omitempty"`
	Impossible               bool             `json:"impossible,omitempty"`
	Gap                      bool             `json:"telemetry_gap"`
	Reason                   string           `json:"reason,omitempty"`
	Binding                  string           `json:"binding"`
	EffectiveRequestInstance string           `json:"effective_request_instance,omitempty"`
	BelowRequest             bool             `json:"below_request"`
	Requested                int              `json:"requested"`
	Ceiling                  int              `json:"host_ceiling"`
	Limit                    int              `json:"effective_limit"`
	Held                     int              `json:"held"`
	MemoryReservation        int64            `json:"session_reservation_bytes"`
	PaneReservation          int              `json:"session_panes"`
	HeldByInstance           map[string]int   `json:"held_by_instance"`
	UncommittedMemory        int64            `json:"uncommitted_memory_bytes"`
	UncommittedPanes         int              `json:"uncommitted_panes"`
	Holders                  []WorkflowHolder `json:"holders"`
}

func workflowState(s *ledgerState) *WorkflowSlotState {
	if s.WorkflowSlots == nil {
		s.WorkflowSlots = &WorkflowSlotState{}
	}
	w := s.WorkflowSlots
	if w.Instances == nil {
		w.Instances = map[string]WorkflowInstance{}
	}
	if w.Holders == nil {
		w.Holders = map[string]WorkflowHolder{}
	}
	if w.Waiters == nil {
		w.Waiters = map[string]WorkflowHolder{}
	}
	if w.Requests == nil {
		w.Requests = map[string]WorkflowRequest{}
	}
	return w
}
func reconcileWorkflowSlots(s *ledgerState) error {
	if s.WorkflowSlots == nil {
		return nil
	}
	w := workflowState(s)
	type proof struct {
		owner Owner
		birth string
	}
	observed := map[proof]bool{}
	dead := func(owner Owner, birth string) bool {
		key := proof{owner, birth}
		if value, ok := observed[key]; ok {
			return value
		}
		value := false
		if birth != "" {
			value = processowner.ProvedDead(owner.PID, birth)
		} else {
			value = ProbeOwner(owner) == Dead
		}
		observed[key] = value
		return value
	}
	for id, instance := range w.Instances {
		if id == "" || instance.Owner.PID <= 1 || instance.Owner.Started <= 0 {
			return errors.New("invalid workflow instance")
		}
		if dead(instance.Owner, instance.Birth) {
			delete(w.Instances, id)
			s.Audit = append(s.Audit, LeaseAudit{At: time.Now().UTC(), Action: "workflow-instance-reclaim", ID: id, Owner: instance.Owner})
		}
	}
	for id, h := range w.Holders {
		if id == "" || h.ID != id || h.Memory < 0 || h.Panes < 0 || h.Sessions < 0 || h.Sessions > 1000000 || h.Resident < 0 || h.LivePanes < 0 || h.Owner.PID <= 1 || h.Owner.Started <= 0 {
			return errors.New("invalid workflow holder ledger")
		}
		if dead(h.Owner, h.Birth) {
			delete(w.Holders, id)
			s.Audit = append(s.Audit, LeaseAudit{At: time.Now().UTC(), Action: "workflow-reclaim", ID: id, Owner: h.Owner})
		}
	}
	for id, h := range w.Waiters {
		if id == "" || workflowWaitKey(h) != id || h.Instance == "" || h.Memory < 0 || h.Panes < 0 || h.Sessions < 0 || h.Sessions > 1000000 || h.Owner.PID <= 1 || h.Owner.Started <= 0 {
			return errors.New("invalid workflow waiter ledger")
		}
		if dead(h.Owner, h.Birth) {
			delete(w.Waiters, id)
		}
	}
	for id, r := range w.Requests {
		if r.Limit < -1 || r.Limit == 0 || r.Limit > 1000000 || r.Owner.PID <= 1 || r.Owner.Started <= 0 {
			return errors.New("invalid workflow request ledger")
		}
		if dead(r.Owner, r.Birth) {
			delete(w.Requests, id)
			s.Audit = append(s.Audit, LeaseAudit{At: time.Now().UTC(), Action: "workflow-request-reclaim", ID: id, Owner: r.Owner})
		}
	}
	return nil
}
func slotContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, time.Second)
}
func (l *Ledger) RegisterWorkflowRequest(ctx context.Context, instance string, owner Owner, limit int) error {
	if instance == "" || limit < -1 || limit == 0 || limit > 1000000 {
		return errors.New("workflow limit must be -1 or positive")
	}
	ctx, cancel := slotContext(ctx)
	defer cancel()
	return l.transact(ctx, true, func(s *ledgerState) error {
		w := workflowState(s)
		if old, ok := w.Requests[instance]; ok && old.Owner != owner && ProbeOwner(old.Owner) != Dead {
			return errors.New("workflow request has another live owner")
		}
		birth, birthErr := processowner.Birth(owner.PID)
		if birthErr != nil {
			return birthErr
		}
		w.Instances[instance] = WorkflowInstance{Owner: owner, Birth: birth}
		if limit == -1 {
			delete(w.Requests, instance)
		} else {
			w.Requests[instance] = func() WorkflowRequest {
				birth, _ := processowner.Birth(owner.PID)
				return WorkflowRequest{Limit: limit, Owner: owner, Birth: birth}
			}()
		}
		return nil
	})
}
func workflowSessionMemory(sessions int, per int64) int64 {
	if sessions < 0 || per <= 0 || int64(sessions) > math.MaxInt64/per {
		return math.MaxInt64
	}
	return int64(sessions) * per
}
func effectiveWorkflowCapacity(s *ledgerState, c WorkflowCapacity) (WorkflowCapacity, error) {
	if len(s.HostLimits) > 0 {
		_, shared, err := reconcileHost(s)
		if err != nil {
			return c, err
		}
		c.Config = hostguard.Conservative(c.Config, shared)
	}
	if c.ReferenceSessions > 0 {
		c.ReferenceMemory = workflowSessionMemory(c.ReferenceSessions, c.Config.PerPane)
	}
	return c, nil
}
func slotDecision(w *WorkflowSlotState, c WorkflowCapacity) WorkflowSlotDecision {
	if c.Now.IsZero() {
		c.Now = time.Now()
	}
	if c.StaleAfter <= 0 {
		c.StaleAfter = time.Minute
	}
	d := WorkflowSlotDecision{Requested: -1, Held: len(w.Holders), UncommittedPanes: math.MaxInt32, MemoryReservation: c.Config.PerPane, PaneReservation: 1, HeldByInstance: map[string]int{}, Binding: "none", PaneAdapter: c.PaneAdapter}
	memory := c.Sample.Memory
	d.Gap = !c.Sample.IO10.Known || !c.Sample.IO60.Known || !c.Sample.Memory10.Known || !c.Sample.Memory60.Known || !c.Sample.LoadPerCore.Known
	invalidMemory := !memory.Known || memory.Value < 0 || memory.Value >= float64(math.MaxInt64) || math.IsNaN(memory.Value) || math.IsInf(memory.Value, 0)
	if !invalidMemory {
		d.UncommittedMemory = int64(memory.Value) - c.Config.Soft
	}
	if c.PaneAdapter {
		d.UncommittedPanes = c.Config.PaneLimit - c.Sample.LivePanes
	}
	for _, h := range w.Holders {
		used, panes := int64(0), 0
		if !h.Measured.IsZero() && !h.Measured.After(c.Now) && c.Now.Sub(h.Measured) <= c.StaleAfter {
			used, panes = h.Resident, h.LivePanes
		}
		reserved := max(int64(0), h.Memory-used)
		if d.UncommittedMemory < math.MinInt64+reserved {
			d.UncommittedMemory = math.MinInt64
		} else {
			d.UncommittedMemory -= reserved
		}
		if c.PaneAdapter {
			d.UncommittedPanes -= max(0, h.Panes-panes)
		}
		d.Holders = append(d.Holders, h)
		d.HeldByInstance[h.Instance]++
	}
	sort.Slice(d.Holders, func(i, j int) bool { return d.Holders[i].ID < d.Holders[j].ID })
	for instance, r := range w.Requests {
		if d.Requested == -1 || r.Limit < d.Requested || r.Limit == d.Requested && instance < d.EffectiveRequestInstance {
			d.EffectiveRequestInstance = instance
			d.Requested = r.Limit
		}
	}
	if invalidMemory {
		d.Gap = true
		d.UncommittedMemory = 0
		d.Reason = "telemetry_gap"
		d.Binding = "telemetry_gap"
		return d
	}
	memorySlots := 0
	if c.ReferenceMemory > 0 {
		memorySlots = int(max(int64(0), d.UncommittedMemory) / c.ReferenceMemory)
	}
	paneSlots := math.MaxInt32
	if c.PaneAdapter && c.ReferencePanes > 0 {
		paneSlots = max(0, d.UncommittedPanes) / c.ReferencePanes
	}
	d.Binding = "host_memory"
	if paneSlots < memorySlots {
		d.Binding = "pane_limit"
	}
	if c.PressureHold {
		d.Binding = "pressure_hold"
	}
	d.Ceiling = d.Held + min(memorySlots, paneSlots)
	d.Limit = d.Ceiling
	if d.Requested != -1 {
		d.Limit = min(d.Limit, d.Requested)
		d.BelowRequest = d.Ceiling < d.Requested
		if d.Requested < d.Ceiling && !c.PressureHold {
			d.Binding = "request"
		}
	}
	d.AboveLimit = d.Held > d.Limit
	return d
}
func waiterBefore(a, b WorkflowHolder) bool {
	if a.Priority != b.Priority {
		return a.Priority > b.Priority
	}
	if !a.Ready.Equal(b.Ready) {
		return a.Ready.Before(b.Ready)
	}
	return a.ID < b.ID
}

// Eligibility is derived from current commitments, never a cached last refusal.
func moleculeWaiterBlocked(w *WorkflowSlotState, h WorkflowHolder) bool {
	if h.Scope != "molecule" {
		return false
	}
	instance, molecule := 0, 0
	for _, held := range w.Holders {
		if held.ID == h.ID || held.Instance != h.Instance || held.Scope != "molecule" {
			continue
		}
		instance++
		if held.Workflow == h.Workflow {
			molecule++
		}
	}
	return h.InstanceLimit > 0 && instance >= h.InstanceLimit || h.MoleculeLimit > 0 && molecule >= h.MoleculeLimit
}

func workflowWaitKey(h WorkflowHolder) string {
	if h.WaitingTask != "" {
		return h.ID + "/wait/" + h.WaitingTask
	}
	return h.ID
}

// AcquireWorkflowSlot is the only normal slot acquisition point. All capacity
// arithmetic and commitment updates happen under the same cross-process lock.
func (l *Ledger) AcquireWorkflowSlot(ctx context.Context, h WorkflowHolder, c WorkflowCapacity) (WorkflowSlotDecision, error) {
	var result WorkflowSlotDecision
	waitID := workflowWaitKey(h)
	if h.ID == "" || h.Instance == "" || h.Memory < 0 || h.Panes < 0 || h.Sessions < 0 || h.Sessions > 1000000 || h.Owner.PID <= 1 || h.Owner.Started <= 0 {
		return result, errors.New("invalid workflow holder")
	}
	ctx, cancel := slotContext(ctx)
	defer cancel()
	err := l.transact(ctx, true, func(s *ledgerState) error {
		w := workflowState(s)
		defer func() {
			for _, waiting := range w.Waiters {
				if waiting.ID == h.ID {
					result.PendingWaiters++
				}
			}
		}()
		var pricingErr error
		c, pricingErr = effectiveWorkflowCapacity(s, c)
		if pricingErr != nil {
			return pricingErr
		}
		if h.BaseSessions > 0 {
			base := h.BaseSessions
			children := map[string]int{}
			if old, exists := w.Holders[h.ID]; exists {
				base = max(base, old.BaseSessions)
				if old.BaseSessions == 0 {
					base = max(base, old.Sessions)
				}
				for id, n := range old.DetachedSessions {
					children[id] = n
				}
			}
			for id, n := range h.DetachedSessions {
				children[id] = max(children[id], n)
			}
			h.BaseSessions, h.DetachedSessions, h.Sessions = base, children, base
			for _, n := range children {
				if n > 1000000-h.Sessions {
					return errors.New("detached footprint exceeds 1000000 sessions")
				}
				h.Sessions += n
			}
		}
		if h.BaseSessions > 0 {
			h.BaseMemory = workflowSessionMemory(h.BaseSessions, c.Config.PerPane)
			if old, ok := w.Holders[h.ID]; ok {
				base := old.BaseMemory
				if base == 0 && old.BaseSessions > 0 && old.Sessions > 0 {
					base = old.Memory * int64(old.BaseSessions) / int64(old.Sessions)
				}
				if base == 0 && len(old.DetachedSessions) == 0 {
					base = old.Memory
				}
				h.BaseMemory = max(h.BaseMemory, base)
			}
		}
		if h.Sessions > 0 {
			old, exists := w.Holders[h.ID]
			if exists && old.Workflow == h.Workflow && old.Revision == h.Revision && h.Sessions <= old.Sessions {
				h.Memory = old.Memory
			} else {
				h.Memory = workflowSessionMemory(h.Sessions, c.Config.PerPane)
			}
		}
		if h.BaseSessions > 0 {
			childSessions := h.Sessions - h.BaseSessions
			childMemory := workflowSessionMemory(childSessions, c.Config.PerPane)
			if childMemory > math.MaxInt64-h.BaseMemory {
				h.Memory = math.MaxInt64
			} else {
				h.Memory = max(h.Memory, h.BaseMemory+childMemory)
			}
		}
		result = slotDecision(w, c)
		var previous *WorkflowHolder
		if old, ok := w.Holders[h.ID]; ok {
			if old.Owner != h.Owner {
				return errors.New("workflow slot has another live owner")
			}
			if h.Memory <= old.Memory && h.Panes <= old.Panes {
				if h.Workflow != old.Workflow || h.Revision != old.Revision || h.Sessions > old.Sessions || len(h.DetachedSessions) != len(old.DetachedSessions) {
					old.Workflow, old.Revision = h.Workflow, h.Revision
					old.Sessions = max(old.Sessions, h.Sessions)
					old.BaseSessions, old.DetachedSessions, old.BaseMemory = h.BaseSessions, h.DetachedSessions, h.BaseMemory
					w.Holders[h.ID] = old
					result.Repriced = true
					s.Audit = append(s.Audit, LeaseAudit{At: time.Now().UTC(), Action: "workflow-repriced", ID: h.ID, Owner: h.Owner})
				}
				if result.Repriced {
					delete(w.Waiters, waitID) // A superseding revision is already funded.
				}
				result.Granted = true
				result.GrantedHolder = &old
				return nil
			}
			previous = &old
		}

		if h.Ready.IsZero() {
			h.Ready = c.Now
			if h.Ready.IsZero() {
				h.Ready = time.Now()
			}
		}
		if old, ok := w.Waiters[waitID]; ok {
			h.Ready = old.Ready
			h.WaitSince = old.WaitSince
			h.WaitReason = old.WaitReason
		}
		if h.WaitSince.IsZero() {
			h.WaitSince = c.Now
			if h.WaitSince.IsZero() {
				h.WaitSince = time.Now()
			}
		}
		h.IgnoreFIFO = false
		h.InstanceLimit = c.InstanceMolecules
		if c.WorkflowMolecules > 0 && (h.MoleculeLimit <= 0 || c.WorkflowMolecules < h.MoleculeLimit) {
			h.MoleculeLimit = c.WorkflowMolecules
		}
		w.Waiters[waitID] = h
		defer func() {
			if waiting, exists := w.Waiters[waitID]; exists && !result.Granted {
				result.WaitSince = waiting.WaitSince
				result.WaitChanged = waiting.WaitReason != result.Reason
				waiting.WaitReason = result.Reason
				w.Waiters[waitID] = waiting
			}
		}()
		impossibleMemory, impossiblePanes := h.Memory, h.Panes
		if len(h.DetachedSessions) > 0 {
			impossibleMemory, impossiblePanes = 0, 0
			// Existing children remain grandfathered, but a newly acquired or
			// enlarged root baseline is itself a spawn obligation. A child's
			// presence must not hide an impossible migration of that baseline.
			baselineChanged := previous == nil
			if previous != nil {
				oldBase := previous.BaseSessions
				if oldBase == 0 {
					oldBase = previous.Sessions
					for _, n := range previous.DetachedSessions {
						oldBase -= n
					}
				}
				baselineChanged = h.BaseSessions > oldBase || h.Workflow != previous.Workflow || h.Revision != previous.Revision
			}
			if baselineChanged {
				impossibleMemory = workflowSessionMemory(h.BaseSessions, c.Config.PerPane)
				if c.PaneAdapter {
					impossiblePanes = h.BaseSessions
				}
			}
			for id, n := range h.DetachedSessions {
				if previous != nil && previous.DetachedSessions[id] >= n {
					continue
				}
				impossibleMemory = max(impossibleMemory, workflowSessionMemory(n, c.Config.PerPane))
				if c.PaneAdapter {
					impossiblePanes = max(impossiblePanes, n)
				}
			}
		}
		if c.TotalMemory > 0 && impossibleMemory > c.TotalMemory-c.Config.Soft || c.PaneAdapter && impossiblePanes > c.Config.PaneLimit {
			result.Impossible = true
			result.Reason = "FootprintExceedsHost"
			delete(w.Waiters, waitID)
			return nil
		}

		for _, legacy := range s.HostLimits {
			known := false
			for _, participant := range w.Instances {
				if participant.Owner == legacy.Owner {
					known = true
					break
				}
			}
			if !known {
				result.Reason = "host_ledger"
				result.LegacyOwners = append(result.LegacyOwners, legacy.Owner)
				result.Detail = "live ledger participant must upgrade or stop before new workflow acquisitions"
				return nil
			}
		}
		if previous != nil {
			old := *previous
			resident, panes := int64(0), 0
			stale := c.StaleAfter
			if stale <= 0 {
				stale = time.Minute
			}
			now := c.Now
			if now.IsZero() {
				now = time.Now()
			}
			if !old.Measured.IsZero() && !old.Measured.After(now) && now.Sub(old.Measured) <= stale {
				resident, panes = old.Resident, old.LivePanes
			}
			memory := max(old.Memory, h.Memory)
			paneCount := max(old.Panes, h.Panes)
			deltaMemory := max(int64(0), memory-resident) - max(int64(0), old.Memory-resident)
			deltaPanes := max(0, paneCount-panes) - max(0, old.Panes-panes)
			if result.Reason != "" {
				return nil
			}
			if (deltaMemory > 0 || c.PaneAdapter && deltaPanes > 0) && (c.PressureHold || result.UncommittedMemory < deltaMemory || c.PaneAdapter && result.UncommittedPanes < deltaPanes) {
				result.Reason = "host_capacity"
				return nil
			}
			for id, waiting := range w.Waiters {
				if id != waitID && !moleculeWaiterBlocked(w, waiting) && waiterBefore(waiting, h) {
					result.Reason = "host_capacity"
					return nil
				}
			}
			old.Memory, old.Panes, old.Workflow, old.Revision = memory, paneCount, h.Workflow, h.Revision
			old.Sessions = max(old.Sessions, h.Sessions)
			old.BaseSessions, old.DetachedSessions, old.BaseMemory = h.BaseSessions, h.DetachedSessions, h.BaseMemory
			w.Holders[h.ID] = old
			delete(w.Waiters, waitID)
			result.Granted = true
			result.Repriced = true
			result.GrantedHolder = &old
			s.Audit = append(s.Audit, LeaseAudit{At: now, Action: "workflow-repriced", ID: h.ID, Owner: h.Owner})
			return nil
		}
		if h.Scope == "molecule" {
			instance, workflow := 0, 0
			for _, held := range w.Holders {
				if held.Instance == h.Instance && held.Scope == "molecule" {
					instance++
					if held.Workflow == h.Workflow {
						workflow++
					}
				}
			}
			workflowLimit := c.WorkflowMolecules
			if h.MoleculeLimit > 0 && (workflowLimit <= 0 || h.MoleculeLimit < workflowLimit) {
				workflowLimit = h.MoleculeLimit
			}
			if c.InstanceMolecules > 0 && instance >= c.InstanceMolecules || workflowLimit > 0 && workflow >= workflowLimit {
				result.Reason = "molecule_limit"
				h.IgnoreFIFO = true
				w.Waiters[waitID] = h
				return nil
			}
		}
		if result.Reason != "" {
			return nil
		}
		if c.PressureHold {
			result.Reason = "host_capacity"
			return nil
		}
		if result.Requested != -1 && result.Held >= result.Requested {
			result.Reason = "workflow_limit"
			return nil
		}
		if result.UncommittedMemory < h.Memory || c.PaneAdapter && result.UncommittedPanes < h.Panes {
			result.Reason = "host_capacity"
			return nil
		}
		for id, waiting := range w.Waiters {
			if id != waitID && !moleculeWaiterBlocked(w, waiting) && waiterBefore(waiting, h) {
				result.Reason = "host_capacity"
				return nil
			}
		}
		w.Holders[h.ID] = h
		delete(w.Waiters, waitID)
		result.Granted = true
		copy := h
		result.GrantedHolder = &copy
		result.Held++
		s.Audit = append(s.Audit, LeaseAudit{At: time.Now().UTC(), Action: "workflow-acquire", ID: h.ID, Owner: h.Owner})
		return nil
	})
	if err != nil {
		result.Reason = "host_ledger"
	}
	return result, err
}
func (l *Ledger) ReestablishWorkflowSlot(ctx context.Context, h WorkflowHolder) error {
	ctx, cancel := slotContext(ctx)
	defer cancel()
	return l.transact(ctx, true, func(s *ledgerState) error {
		w := workflowState(s)
		if h.Sessions > 0 {
			effective, err := effectiveWorkflowCapacity(s, WorkflowCapacity{Config: hostguard.DefaultConfig()})
			if err != nil {
				return err
			}
			h.Memory = max(h.Memory, workflowSessionMemory(h.Sessions, effective.Config.PerPane))
		}
		if old, ok := w.Holders[h.ID]; ok && old.Owner != h.Owner {
			return errors.New("workflow holder has another live owner")
		}
		w.Holders[h.ID] = h
		delete(w.Waiters, h.ID)
		s.Audit = append(s.Audit, LeaseAudit{At: time.Now().UTC(), Action: "workflow-reestablish", ID: h.ID, Owner: h.Owner})
		return nil
	})
}
func (l *Ledger) ReleaseWorkflowSlot(ctx context.Context, id string, owner Owner) error {
	ctx, cancel := slotContext(ctx)
	defer cancel()
	return l.transact(ctx, true, func(s *ledgerState) error {
		w := workflowState(s)
		if old, ok := w.Holders[id]; ok {
			if old.Owner != owner {
				return errors.New("workflow holder ownership mismatch")
			}
			delete(w.Holders, id)
			s.Audit = append(s.Audit, LeaseAudit{At: time.Now().UTC(), Action: "workflow-release", ID: id, Owner: owner})
		}
		for key, waiting := range w.Waiters {
			if waiting.ID == id {
				delete(w.Waiters, key)
			}
		}
		return nil
	})
}
func (l *Ledger) PublishWorkflowUsage(ctx context.Context, id string, owner Owner, memory int64, panes int, at time.Time) error {
	ctx, cancel := slotContext(ctx)
	defer cancel()
	return l.transact(ctx, true, func(s *ledgerState) error {
		w := workflowState(s)
		h, ok := w.Holders[id]
		if !ok || h.Owner != owner {
			return errors.New("workflow holder missing")
		}
		if memory < 0 || panes < 0 {
			return errors.New("negative workflow usage")
		}
		h.Resident, h.LivePanes, h.Measured = memory, panes, at
		w.Holders[id] = h
		return nil
	})
}
func (l *Ledger) WorkflowSlotSnapshot(ctx context.Context, c WorkflowCapacity) (WorkflowSlotDecision, WorkflowSlotState, error) {
	var d WorkflowSlotDecision
	var state WorkflowSlotState
	ctx, cancel := slotContext(ctx)
	defer cancel()
	err := l.transact(ctx, true, func(s *ledgerState) error {
		w := workflowState(s)
		var err error
		c, err = effectiveWorkflowCapacity(s, c)
		if err != nil {
			return err
		}
		d = slotDecision(w, c)
		for _, legacy := range s.HostLimits {
			known := false
			for _, participant := range w.Instances {
				if participant.Owner == legacy.Owner {
					known = true
					break
				}
			}
			if !known {
				d.Reason = "host_ledger"
				d.LegacyOwners = append(d.LegacyOwners, legacy.Owner)
				d.Detail = "live ledger participant must upgrade or stop before new workflow acquisitions"
			}
		}
		state = *w
		return nil
	})
	return d, state, err
}

// CommitWorkflowRequest obtains and validates the ledger before invoking the
// durable instance audit callback. An unreadable/locked ledger never stores a
// requested change in the plane.
func (l *Ledger) CommitWorkflowRequest(ctx context.Context, instance string, owner Owner, limit int, commit func() error) error {
	if instance == "" || limit < -1 || limit == 0 || limit > 1000000 || commit == nil {
		return errors.New("invalid workflow request")
	}
	ctx, cancel := slotContext(ctx)
	defer cancel()
	return l.transact(ctx, true, func(s *ledgerState) error {
		w := workflowState(s)
		if old, ok := w.Requests[instance]; ok && old.Owner != owner {
			return errors.New("workflow request owner mismatch")
		}
		if err := commit(); err != nil {
			return err
		}
		birth, birthErr := processowner.Birth(owner.PID)
		if birthErr != nil {
			return birthErr
		}
		w.Instances[instance] = WorkflowInstance{Owner: owner, Birth: birth}
		if limit == -1 {
			delete(w.Requests, instance)
		} else {
			w.Requests[instance] = func() WorkflowRequest {
				birth, _ := processowner.Birth(owner.PID)
				return WorkflowRequest{Limit: limit, Owner: owner, Birth: birth}
			}()
		}
		return nil
	})
}

func OpenWorkflowLedger(path string) (*Ledger, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ledger, err := openLedgerContext(ctx, path, 1, true)
	if err != nil {
		return nil, err
	}
	if err = ledger.transact(ctx, true, func(s *ledgerState) error {
		if err := upgradeWorkflowSchema(s); err != nil {
			return err
		}
		workflowState(s)
		return nil
	}); err != nil {
		return nil, err
	}
	return ledger, nil
}
func (l *Ledger) UnregisterWorkflowInstance(ctx context.Context, instance string, owner Owner) error {
	ctx, cancel := slotContext(ctx)
	defer cancel()
	return l.transact(ctx, true, func(s *ledgerState) error {
		w := workflowState(s)
		if old, ok := w.Instances[instance]; ok && old.Owner != owner {
			return errors.New("workflow instance owner mismatch")
		}
		delete(w.Instances, instance)
		delete(w.Requests, instance)
		return nil
	})
}

func (l *Ledger) CancelWorkflowWaiter(ctx context.Context, id string, owner Owner, task ...string) error {
	ctx, cancel := slotContext(ctx)
	defer cancel()
	return l.transact(ctx, true, func(s *ledgerState) error {
		w := workflowState(s)
		for key, waiting := range w.Waiters {
			if waiting.ID != id || len(task) > 0 && waiting.WaitingTask != task[0] {
				continue
			}
			if waiting.Owner != owner {
				return errors.New("workflow waiter owner mismatch")
			}
			delete(w.Waiters, key)
		}
		return nil
	})
}

// ReleaseDetachedCommitments removes only completed external subtrees. The
// conservative baseline of previously funded revisions remains held.
func (l *Ledger) ReleaseDetachedCommitments(ctx context.Context, id string, owner Owner, keep map[string]int) (WorkflowHolder, error) {
	var result WorkflowHolder
	ctx, cancel := slotContext(ctx)
	defer cancel()
	err := l.transact(ctx, true, func(s *ledgerState) error {
		w := workflowState(s)
		h, ok := w.Holders[id]
		if !ok {
			return errors.New("workflow slot missing")
		}
		if h.Owner != owner {
			return errors.New("workflow slot owner mismatch")
		}
		removed := 0
		for child, n := range h.DetachedSessions {
			if _, live := keep[child]; !live {
				removed += n
				delete(h.DetachedSessions, child)
			}
		}
		if removed > 0 {
			price := int64(0)
			if childSessions := h.Sessions - h.BaseSessions; childSessions > 0 {
				price = max(int64(0), h.Memory-h.BaseMemory) / int64(childSessions)
			}
			h.Memory = max(h.BaseMemory, h.Memory-int64(removed)*price)
			if h.Panes > 0 {
				h.Panes = max(0, h.Panes-removed)
			}
			h.Sessions -= removed
			w.Holders[id] = h
			s.Audit = append(s.Audit, LeaseAudit{At: time.Now().UTC(), Action: "workflow-child-release", ID: id, Owner: owner})
		}
		result = h
		return nil
	})
	return result, err
}
