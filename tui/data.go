package tui

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/xleelz/fae/tui/client"
)

// Daemon is everything the TUI asks of the daemon; *client.Client
// implements it over the instance socket, and tests use a fake.
type Daemon interface {
	Status(ctx context.Context) (client.Status, error)
	Board(ctx context.Context, includeRetired bool) (client.Board, error)
	Backlog(ctx context.Context, includeRetired bool) (client.Backlog, error)
	Topology(ctx context.Context) (client.Topology, error)
	Alerts(ctx context.Context) ([]client.Alert, error)
	Inbox(ctx context.Context) (client.Inbox, error)
	Detail(ctx context.Context, id string, have client.Snapshot, includeRetired bool) (client.TaskDetail, error)
	Message(ctx context.Context, text, task string) (string, error)
	TaskCreate(ctx context.Context, p client.TaskCreateParams) (client.TaskCreateResult, error)
	TaskUpdate(ctx context.Context, p client.TaskUpdateParams) (client.TaskUpdateResult, error)
	TaskDelete(ctx context.Context, p client.TaskDeleteParams) (client.TaskDeleteResult, error)
	BacklogEdge(ctx context.Context, p client.EdgeParams) (json.RawMessage, error)
	AlertAcknowledge(ctx context.Context, id int64, message string) error
}

// readTimeout bounds one refresh: every read runs at once, each under the
// daemon's own 5-second deadline.
const readTimeout = 8 * time.Second

// writeTimeout bounds one write: the authorizing message, then the RPC.
const writeTimeout = 12 * time.Second

// Read part names, used for per-part errors.
const (
	partStatus   = "Status"
	partBoard    = "Board"
	partBacklog  = "BacklogShow"
	partTopology = "Topology"
	partAlerts   = "Alerts"
	partInbox    = "Inbox"
	partStream   = "Stream"
	partPools    = "Pools"
	partSymphony = "Symphony"
	partSecurity = "Security"
)

var parts = []string{partStatus, partBoard, partBacklog, partTopology, partAlerts, partInbox}

// partsFor lists the reads a view shows. Status, Board, Alerts and Inbox
// serve every view (the header, the tab counts including Attention, each
// task's state); the Molecules view adds Topology, and the Graph the
// backlog graph. The first refresh reads everything once (viewAll).
func partsFor(view viewID) []string {
	switch view {
	case viewMolecules:
		return []string{partStatus, partBoard, partTopology, partAlerts, partInbox}
	case viewGraph:
		return []string{partStatus, partBoard, partBacklog, partAlerts, partInbox}
	case viewAttention:
		return []string{partStatus, partBoard, partAlerts, partInbox}
	case viewStream:
		return []string{partStatus, partBoard, partAlerts, partInbox, partStream}
	case viewPools:
		return []string{partStatus, partBoard, partAlerts, partInbox, partPools}
	case viewSymphony:
		return []string{partStatus, partBoard, partBacklog, partAlerts, partInbox, partSymphony}
	case viewSecurity:
		return []string{partStatus, partBoard, partAlerts, partInbox, partSecurity}
	}
	return parts
}

// ExtendedDaemon is the optional interface for Wave 19-21 reads and writes.
// *client.Client implements it; test fakes that only implement Daemon continue
// to work without changes.
type ExtendedDaemon interface {
	Leases(ctx context.Context) ([]client.ProviderPoolView, error)
	ProviderHealth(ctx context.Context) ([]client.ProviderBindingHealthView, error)
	DispatchPauses(ctx context.Context) ([]client.DispatchPauseView, error)
	Panes(ctx context.Context) ([]client.PaneSessionView, error)
	AnalyzeSession(ctx context.Context, taskID string) (client.SessionAnalysisView, error)
	TargetConfigs(ctx context.Context) ([]client.TargetConfigView, error)
	Events(ctx context.Context, limit int) ([]client.EventView, error)
	ProvenanceChain(ctx context.Context, taskID string) ([]client.ProvenanceEntryView, error)
	LandStatus(ctx context.Context, taskID string) (client.LandStatusView, error)
	SymphonyState(ctx context.Context) (client.SymphonyStateView, error)
	SymphonyRefresh(ctx context.Context) error
	Notify(ctx context.Context, p client.NotifyParams) error
	FollowUp(ctx context.Context, p client.FollowUpParams) (client.FollowUpResult, error)
	PoolResize(ctx context.Context, p client.PoolResizeParams) error
	QuotaSet(ctx context.Context, p client.QuotaSetParams) error
	QuotaClear(ctx context.Context, p client.QuotaClearParams) error
	DispatchPause(ctx context.Context, p client.DispatchPauseParams) error
	DispatchUnpause(ctx context.Context, p client.DispatchPauseParams) error
	Decide(ctx context.Context, p client.DecideParams) error
	ResolveUncertain(ctx context.Context, p client.ResolveUncertainParams) error
	Lifecycle(ctx context.Context, p client.LifecycleParams) error
}

// snapshot is the last good value of every read. A failed read keeps the
// previous value and records its error.
type snapshot struct {
	status   client.Status
	board    client.Board
	backlog  client.Backlog
	topology client.Topology
	alerts   []client.Alert
	inbox    client.Inbox
	pools    []client.ProviderPoolView
	pauses   []client.DispatchPauseView
	panes    []client.PaneSessionView
	health   []client.ProviderBindingHealthView
	symphony client.SymphonyStateView
	targets  []client.TargetConfigView
	events   []client.EventView
	details  map[string]client.TaskDetail
	// tasks indexes the Board rows by id; it is replaced, never mutated.
	tasks map[string]client.Task
	// loaded names the reads that have succeeded at least once.
	loaded map[string]bool
	// at is when the last read finished; generation counts refreshes.
	at         time.Time
	generation int
}

// snapshotMsg carries one refresh's results: the parts it read, each
// value, and the error of each part that failed.
type snapshotMsg struct {
	parts    []string
	status   client.Status
	board    client.Board
	backlog  client.Backlog
	topology client.Topology
	alerts   []client.Alert
	inbox    client.Inbox
	pools    []client.ProviderPoolView
	pauses   []client.DispatchPauseView
	panes    []client.PaneSessionView
	health   []client.ProviderBindingHealthView
	symphony client.SymphonyStateView
	targets  []client.TargetConfigView
	events   []client.EventView
	errs     map[string]error
	at       time.Time
}

// fetchSnapshot runs the named reads concurrently, so one slow read delays
// the refresh by no more than its own deadline, and none blocks the UI.
func fetchSnapshot(d Daemon, wanted []string, includeRetired bool, now func() time.Time) func() tea.Msg {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), readTimeout)
		defer cancel()
		var mu sync.Mutex
		out := snapshotMsg{parts: slices.Clone(wanted), errs: map[string]error{}}
		fail := func(part string, err error) {
			if err != nil {
				mu.Lock()
				out.errs[part] = err
				mu.Unlock()
			}
		}
		var wg sync.WaitGroup
		run := func(part string, f func()) {
			if !slices.Contains(wanted, part) {
				return
			}
			wg.Add(1)
			go func() { defer wg.Done(); f() }()
		}
		run(partStatus, func() {
			s, err := d.Status(ctx)
			fail(partStatus, err)
			mu.Lock()
			out.status = s
			mu.Unlock()
		})
		run(partBoard, func() {
			b, err := d.Board(ctx, includeRetired)
			fail(partBoard, err)
			mu.Lock()
			out.board = b
			mu.Unlock()
		})
		run(partBacklog, func() {
			b, err := d.Backlog(ctx, includeRetired)
			fail(partBacklog, err)
			mu.Lock()
			out.backlog = b
			mu.Unlock()
		})
		run(partTopology, func() {
			var (
				t   client.Topology
				err error
			)
			if r, ok := d.(interface {
				TopologyWithOptions(ctx context.Context, includeRetired bool) (client.Topology, error)
			}); ok {
				t, err = r.TopologyWithOptions(ctx, includeRetired)
			} else {
				t, err = d.Topology(ctx)
			}
			fail(partTopology, err)
			mu.Lock()
			out.topology = t
			mu.Unlock()
		})
		run(partAlerts, func() {
			a, err := d.Alerts(ctx)
			fail(partAlerts, err)
			mu.Lock()
			out.alerts = a
			mu.Unlock()
		})
		run(partInbox, func() {
			i, err := d.Inbox(ctx)
			fail(partInbox, err)
			mu.Lock()
			out.inbox = i
			mu.Unlock()
		})
		if ext, ok := d.(ExtendedDaemon); ok {
			run(partStream, func() {
				p, err := ext.Panes(ctx)
				if !client.Unsupported(err) {
					fail(partStream, err)
				}
				mu.Lock()
				out.panes = p
				mu.Unlock()
			})
			run(partPools, func() {
				pools, err1 := ext.Leases(ctx)
				health, err2 := ext.ProviderHealth(ctx)
				pauses, err3 := ext.DispatchPauses(ctx)
				for _, err := range []error{err1, err2, err3} {
					if err != nil && !client.Unsupported(err) {
						fail(partPools, err)
						break
					}
				}
				mu.Lock()
				out.pools = pools
				out.health = health
				out.pauses = pauses
				mu.Unlock()
			})
			run(partSymphony, func() {
				sym, err := ext.SymphonyState(ctx)
				if !client.Unsupported(err) {
					fail(partSymphony, err)
				}
				mu.Lock()
				out.symphony = sym
				mu.Unlock()
			})
			run(partSecurity, func() {
				targets, err1 := ext.TargetConfigs(ctx)
				events, err2 := ext.Events(ctx, 100)
				for _, err := range []error{err1, err2} {
					if err != nil && !client.Unsupported(err) {
						fail(partSecurity, err)
						break
					}
				}
				mu.Lock()
				out.targets = targets
				out.events = events
				mu.Unlock()
			})
		}
		wg.Wait()
		out.at = now()
		return out
	}
}

// merge folds a refresh into the snapshot: each read that succeeded replaces
// its part, each that failed or was not read keeps the previous one. changed
// reports whether any part the refresh read holds different data from
// before (the Status read and the snapshot clocks do not count: the uptime
// and the daemon's cache time move on every read).
func (s snapshot) merge(m snapshotMsg) (next snapshot, changed bool) {
	if s.loaded == nil {
		s.loaded = map[string]bool{}
	} else {
		s.loaded = maps(s.loaded)
	}
	if s.details == nil {
		s.details = map[string]client.TaskDetail{}
	} else {
		s.details = maps(s.details)
	}
	ok := func(part string) bool {
		if !slices.Contains(m.parts, part) || m.errs[part] != nil {
			return false
		}
		s.loaded[part] = true
		return true
	}
	if ok(partStatus) {
		s.status = m.status
	}
	if ok(partBoard) {
		changed = changed || !reflect.DeepEqual(s.board.Tasks, m.board.Tasks) || s.board.Truncated != m.board.Truncated
		s.board = m.board
		s.tasks = make(map[string]client.Task, len(m.board.Tasks))
		for _, t := range m.board.Tasks {
			s.tasks[t.ID] = t
		}
	}
	if ok(partBacklog) {
		changed = changed || !reflect.DeepEqual(s.backlog, m.backlog)
		s.backlog = m.backlog
	}
	if ok(partTopology) {
		changed = changed || !reflect.DeepEqual(s.topology.Molecules, m.topology.Molecules)
		s.topology = m.topology
	}
	if ok(partAlerts) {
		changed = changed || !reflect.DeepEqual(s.alerts, m.alerts)
		s.alerts = m.alerts
	}
	if ok(partInbox) {
		changed = changed || !reflect.DeepEqual(s.inbox, m.inbox)
		s.inbox = m.inbox
	}
	if ok(partStream) {
		if m.panes != nil || len(m.status.Panes) == 0 {
			changed = changed || !reflect.DeepEqual(s.panes, m.panes)
			s.panes = m.panes
		}
	} else if m.panes != nil {
		changed = changed || !reflect.DeepEqual(s.panes, m.panes)
		s.panes = m.panes
	}
	if ok(partPools) {
		if m.pools != nil || len(m.status.ProviderPools) == 0 {
			changed = changed || !reflect.DeepEqual(s.pools, m.pools)
			s.pools = m.pools
		}
		if m.health != nil || len(m.status.ProviderHealth) == 0 {
			changed = changed || !reflect.DeepEqual(s.health, m.health)
			s.health = m.health
		}
		if m.pauses != nil || len(m.status.DispatchPauses) == 0 {
			changed = changed || !reflect.DeepEqual(s.pauses, m.pauses)
			s.pauses = m.pauses
		}
	} else {
		if m.pools != nil {
			changed = changed || !reflect.DeepEqual(s.pools, m.pools)
			s.pools = m.pools
		}
		if m.health != nil {
			changed = changed || !reflect.DeepEqual(s.health, m.health)
			s.health = m.health
		}
		if m.pauses != nil {
			changed = changed || !reflect.DeepEqual(s.pauses, m.pauses)
			s.pauses = m.pauses
		}
	}
	if ok(partSymphony) || m.symphony.Running != nil || m.symphony.Retrying != nil || m.symphony.KnownIssues != nil || m.symphony.TotalTokens != 0 || m.symphony.PollIntervalMs != 0 {
		changed = changed || !reflect.DeepEqual(s.symphony, m.symphony)
		s.symphony = m.symphony
	}
	if ok(partSecurity) {
		changed = changed || !reflect.DeepEqual(s.targets, m.targets) || !reflect.DeepEqual(s.events, m.events)
		s.targets = m.targets
		s.events = m.events
	} else {
		if m.targets != nil {
			changed = changed || !reflect.DeepEqual(s.targets, m.targets)
			s.targets = m.targets
		}
		if m.events != nil {
			changed = changed || !reflect.DeepEqual(s.events, m.events)
			s.events = m.events
		}
	}
	if m.status.ProviderPools != nil && len(s.pools) == 0 {
		s.pools = m.status.ProviderPools
	}
	if m.status.DispatchPauses != nil && len(s.pauses) == 0 {
		s.pauses = m.status.DispatchPauses
	}
	if m.status.Panes != nil && len(s.panes) == 0 {
		s.panes = m.status.Panes
	}
	if m.status.ProviderHealth != nil && len(s.health) == 0 {
		s.health = m.status.ProviderHealth
	}
	s.at = m.at
	s.generation++
	return s, changed
}

func maps[K comparable, V any](m map[K]V) map[K]V {
	out := make(map[K]V, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// unreachable reports a refresh in which the daemon could not be reached.
func (m snapshotMsg) unreachable() error {
	for _, part := range parts {
		if err := m.errs[part]; errors.Is(err, client.ErrUnreachable) {
			return err
		}
	}
	return nil
}

// have returns what a detail read can reuse from the snapshot.
func (s snapshot) have() client.Snapshot {
	return client.Snapshot{Tasks: s.tasks, Backlog: s.backlog}
}

// detailMsg carries one task-detail read.
type detailMsg struct {
	id     string
	detail client.TaskDetail
	err    error
}

func fetchDetail(d Daemon, id string, have client.Snapshot, includeRetired bool) func() tea.Msg {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), readTimeout)
		defer cancel()
		detail, err := d.Detail(ctx, id, have, includeRetired)
		return detailMsg{id: id, detail: detail, err: err}
	}
}

// sinceMsg carries, for each escalated task read, when it entered Escalated
// and its stored pending delete, keyed by the Board version it was read at.
type sinceMsg struct {
	since   map[string]time.Time
	pending map[string]*client.PendingDelete
	keys    map[string]int64
}

// maxSinceReads bounds the detail reads one refresh spends on escalation times.
const maxSinceReads = 20

func fetchSince(d Daemon, ids []string, versions map[string]int64, have client.Snapshot, includeRetired bool) func() tea.Msg {
	return func() tea.Msg {
		out := sinceMsg{since: map[string]time.Time{}, pending: map[string]*client.PendingDelete{}, keys: map[string]int64{}}
		for _, id := range ids {
			ctx, cancel := context.WithTimeout(context.Background(), readTimeout)
			detail, err := d.Detail(ctx, id, have, includeRetired)
			cancel()
			// A failed read is not retried until the task changes: zero
			// says the time is unknown.
			out.keys[id], out.since[id] = versions[id], time.Time{}
			if err == nil {
				out.since[id] = escalatedSince(detail)
				if detail.PendingDelete != nil {
					p := *detail.PendingDelete
					if detail.Reason == "UncertainDispatch" || detail.EscalationReason == "UncertainDispatch" || detail.EscalationKind == "UncertainDispatch" || p.Status == "uncertain" {
						p.UncertainDispatch = true
					}
					out.pending[id] = &p
				}
			}
		}
		return out
	}
}

// escalatedSince is when the task escalated: TaskGet's escalated_at, else
// when the trace last entered Escalated (zero if unknown).
func escalatedSince(d client.TaskDetail) time.Time {
	if !d.EscalatedAt.IsZero() {
		return d.EscalatedAt
	}
	for i := len(d.Trace) - 1; i >= 0; i-- {
		if d.Trace[i].Stage == "Escalated" {
			return d.Trace[i].EnteredAt
		}
	}
	return time.Time{}
}

// Kernel states (domain.State): every other state is a workflow stage.
var kernelStates = []string{"Pending", "Blocked", "Parked", "Escalated", "Done", "Retired", "Cancelled"}

func kernel(state string) bool { return slices.Contains(kernelStates, state) }

func terminal(state string) bool {
	return state == "Done" || state == "Retired" || state == "Cancelled"
}

// implState is a task's implementation state: a short label for the graph,
// a full label, and its colour class.
type implState struct {
	Short, Long string
	Class       stateClass
}

type stateClass int

const (
	classUnknown stateClass = iota
	classReady
	classWaiting
	classStage
	classRunning
	classBlocked
	classEscalated
	classDone
	classClosed
)

// stateOf derives a task's implementation state from its Board row, the
// backlog graph's ready set, withheld prerequisites and deferrals, and the
// latest drive round's withheld reasons.
func (s snapshot) stateOf(id string) implState {
	t, ok := s.tasks[id]
	if !ok {
		for _, item := range s.backlog.Items {
			if item.ID == id {
				t, ok = client.Task{ID: id, State: item.State}, true
			}
		}
	}
	if !ok {
		return implState{"unknown", "not on the board (hidden or unknown)", classUnknown}
	}
	reason := ""
	if s.status.Snapshot != nil {
		reason = s.status.Snapshot.Scheduler.WithheldTasks[id]
	}
	switch t.State {
	case "Done":
		return implState{"Done", "Done", classDone}
	case "Cancelled":
		return implState{"Cancelled", "Cancelled", classClosed}
	case "Retired":
		return implState{"Retired", "Retired", classClosed}
	case "Escalated":
		if t.PendingDelete {
			return implState{"deleting", "delete pending: stopped by a TaskDelete that has not finished (open the task for its status)", classClosed}
		}
		long := "Escalated"
		if why := escalation(t.EscalationKind, t.Reason); why != "" {
			long += " " + why
		}
		return implState{"Escalated", long, classEscalated}
	case "Blocked":
		long := "Blocked"
		if t.Reason != "" {
			long += ": " + t.Reason
		}
		return implState{"Blocked", long, classBlocked}
	case "Pending", "Parked":
		if t.PendingDelete {
			if t.Reason == "UncertainDispatch" || t.EscalationKind == "UncertainDispatch" {
				return implState{"deleting", "delete pending: turn in flight is uncertain after restart; run fae resolve-uncertain --action escalate --id " + id + " to finish retiring", classClosed}
			}
			return implState{"deleting", "delete pending: stopped by a TaskDelete that has not finished (open the task for its status)", classClosed}
		}
		if until, deferred := s.backlog.Deferrals[id]; deferred {
			long := "deferred"
			if until != "" && !strings.HasPrefix(until, "0001-") {
				long += " until " + until
			}
			return implState{"deferred", long, classWaiting}
		}
		if prereqs := s.withheldBy(id); len(prereqs) > 0 {
			return implState{"withheld", "withheld by " + strings.Join(prereqs, "; "), classWaiting}
		}
		if reason != "" {
			return implState{"withheld", "withheld: " + reason, classWaiting}
		}
		if s.isReady(id) {
			return implState{"ready", "Pending, ready", classReady}
		}
		return implState{"Pending", "Pending", classReady}
	}
	if t.InFlight {
		return implState{t.State, "in stage " + t.State + " (turn in flight)", classRunning}
	}
	if t.Reason != "" || reason != "" {
		why := t.Reason
		if why == "" {
			why = reason
		} else if reason != "" && reason != why {
			why = reason + ": " + why
		}
		return implState{"parked", "parked in " + t.State + ": " + why, classWaiting}
	}
	return implState{t.State, "in stage " + t.State, classStage}
}

// withheldBy lists the prerequisites still holding id, reconciling the last
// BacklogShow read against fresher Board states so a prerequisite that has
// since finished on the Board no longer withholds id on views that do not
// poll BacklogShow.
func (s snapshot) withheldBy(id string) []string {
	raw := s.backlog.Withheld[id]
	if len(raw) == 0 {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, entry := range raw {
		prereq, _, ok := strings.Cut(entry, ": ")
		if !ok {
			out = append(out, entry)
			continue
		}
		if pt, onBoard := s.tasks[prereq]; onBoard && pt.State != "" {
			if pt.State == "Done" {
				continue
			}
			out = append(out, prereq+": "+pt.State)
			continue
		}
		out = append(out, entry)
	}
	return out
}

func (s snapshot) isReady(id string) bool {
	if slices.Contains(s.backlog.Ready, id) {
		return true
	}
	return len(s.backlog.Withheld[id]) > 0 && len(s.withheldBy(id)) == 0
}

// sortedTasks lists the Board rows by priority, highest first, then id.
func (s snapshot) sortedTasks() []client.Task {
	out := make([]client.Task, 0, len(s.tasks))
	for _, t := range s.tasks {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority > out[j].Priority
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// escalation joins an escalation's kind and reason without repeating the
// kind, which a recorded reason often starts with ("OperatorEscalate: …").
func escalation(kind, reason string) string {
	reason = strings.TrimSpace(reason)
	switch {
	case kind == "" || reason == kind:
		return reason
	case reason == "":
		return kind
	case strings.HasPrefix(reason, kind):
		return reason
	}
	return kind + ": " + reason
}

// pendingDeleteText says where a stored pending delete stands, from
// TaskGet's object: what the delete does and whether it waits for the turn
// in flight, needs resolve-uncertain after a restart, retires at the next
// drive round, or failed and left the task Escalated. The Board's boolean
// alone says only that one is pending.
func pendingDeleteText(id string, p *client.PendingDelete, uncertain bool) string {
	if p == nil {
		return ""
	}
	what := "retire"
	if p.SupersededBy != "" {
		what = "retire, superseded by " + p.SupersededBy
	}
	if p.Reason != "" {
		what += " (" + p.Reason + ")"
	}
	if p.Status == "uncertain" || p.UncertainDispatch || (p.Status == "waiting-for-turn" && uncertain) {
		target := id
		if target == "" {
			target = "<id>"
		}
		return what + ": turn in flight is uncertain after restart; run fae resolve-uncertain --action escalate --id " + target + " to finish retiring"
	}
	switch p.Status {
	case "waiting-for-turn":
		return what + ": retires once its turn in flight is collected"
	case "retiring":
		return what + ": retiring at the next drive round"
	case "failed":
		return "delete failed" + strings.TrimPrefix(what, "retire") + ": the task stays Escalated; retire it or delete it again"
	}
	return what + ": " + p.Status
}
