package tui

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/xleelz/fae/tui/client"
)

// fixedNow is the fake clock every model test reads.
var fixedNow = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

// fakeDaemon answers the TUI's reads from fixtures and records every call.
type fakeDaemon struct {
	mu       sync.Mutex
	calls    []string
	status   client.Status
	board    client.Board
	backlog  client.Backlog
	topology client.Topology
	alerts   []client.Alert
	inbox    client.Inbox
	details  map[string]client.TaskDetail
	errs     map[string]error
	// edgeCheck, when set, refuses some BacklogEdge calls.
	edgeCheck func(client.EdgeParams) error

	messages []string
	creates  []client.TaskCreateParams
	updates  []client.TaskUpdateParams
	deletes  []client.TaskDeleteParams
	edges    []client.EdgeParams
	acks     []int64
}

func (f *fakeDaemon) record(method string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, method)
	return f.errs[method]
}

func (f *fakeDaemon) called(methods ...string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []string{}
	for _, c := range f.calls {
		for _, m := range methods {
			if c == m {
				out = append(out, c)
			}
		}
	}
	return out
}

func (f *fakeDaemon) fail(method string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.errs == nil {
		f.errs = map[string]error{}
	}
	if err == nil {
		delete(f.errs, method)
	} else {
		f.errs[method] = err
	}
}

func (f *fakeDaemon) Status(context.Context) (client.Status, error) {
	return f.status, f.record("Status")
}
func (f *fakeDaemon) Board(context.Context, bool) (client.Board, error) {
	return f.board, f.record("Board")
}
func (f *fakeDaemon) Backlog(context.Context, bool) (client.Backlog, error) {
	return f.backlog, f.record("BacklogShow")
}
func (f *fakeDaemon) Topology(context.Context) (client.Topology, error) {
	return f.topology, f.record("Topology")
}
func (f *fakeDaemon) Alerts(context.Context) ([]client.Alert, error) {
	return f.alerts, f.record("Alerts")
}
func (f *fakeDaemon) Inbox(context.Context) (client.Inbox, error) {
	return f.inbox, f.record("Inbox")
}
func (f *fakeDaemon) Detail(_ context.Context, id string, _ client.Snapshot, _ bool) (client.TaskDetail, error) {
	if err := f.record("Detail"); err != nil {
		return client.TaskDetail{}, err
	}
	d, ok := f.details[id]
	if !ok {
		return client.TaskDetail{}, errors.New("no such task " + id)
	}
	return d, nil
}
func (f *fakeDaemon) Message(_ context.Context, text, _ string) (string, error) {
	if err := f.record("Message"); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.messages = append(f.messages, text)
	return "msg-" + string(rune('0'+len(f.messages))), nil
}
func (f *fakeDaemon) TaskCreate(_ context.Context, p client.TaskCreateParams) (client.TaskCreateResult, error) {
	if err := f.record("TaskCreate"); err != nil {
		return client.TaskCreateResult{}, err
	}
	f.creates = append(f.creates, p)
	return client.TaskCreateResult{ID: p.ID, State: "Pending", EdgesAdded: json.RawMessage(`1`), Readiness: json.RawMessage(`{"ready":false}`)}, nil
}
func (f *fakeDaemon) TaskUpdate(_ context.Context, p client.TaskUpdateParams) (client.TaskUpdateResult, error) {
	f.updates = append(f.updates, p)
	if err := f.record("TaskUpdate"); err != nil {
		return client.TaskUpdateResult{}, err
	}
	return client.TaskUpdateResult{ID: p.ID, Changed: p.Changed(), Effect: "from-next-stage"}, nil
}
func (f *fakeDaemon) TaskDelete(_ context.Context, p client.TaskDeleteParams) (client.TaskDeleteResult, error) {
	f.deletes = append(f.deletes, p)
	if err := f.record("TaskDelete"); err != nil {
		return client.TaskDeleteResult{}, err
	}
	return client.TaskDeleteResult{ID: p.ID, Action: "retired", CollectedInFlight: true, Pending: true, HeldDependents: []string{"api"}}, nil
}
func (f *fakeDaemon) BacklogEdge(_ context.Context, p client.EdgeParams) (json.RawMessage, error) {
	if err := f.record("BacklogEdge"); err != nil {
		return nil, err
	}
	if f.edgeCheck != nil {
		if err := f.edgeCheck(p); err != nil {
			return nil, err
		}
	}
	f.edges = append(f.edges, p)
	return json.RawMessage(`{"readiness_effect":{"effect":"held"}}`), nil
}
func (f *fakeDaemon) AlertAcknowledge(_ context.Context, id int64, _ string) error {
	if err := f.record("AlertAcknowledge"); err != nil {
		return err
	}
	f.acks = append(f.acks, id)
	return nil
}

// writeMethods are the calls a read-only session must never make.
var writeMethods = []string{"Message", "TaskCreate", "TaskUpdate", "TaskDelete", "BacklogEdge", "AlertAcknowledge"}

// fixture is a small live instance: a finished library, a parser in flight,
// an api waiting on both, an app after it, an escalated docs task, a
// cancelled task holding a dead edge, and a related notes task.
func fixture() *fakeDaemon {
	at := func(minutesAgo int) time.Time { return fixedNow.Add(-time.Duration(minutesAgo) * time.Minute) }
	left := func(minutesAgo int) *time.Time { t := at(minutesAgo); return &t }
	tasks := []client.Task{
		{ID: "lib", State: "Done", Priority: 30, WorkflowID: "default", WorkflowRevision: 3, Version: 9},
		{ID: "parser", State: "implement", Priority: 20, InFlight: true, WorkflowID: "default", WorkflowRevision: 3, MoleculeID: "m1", Version: 5},
		{ID: "api", State: "Pending", Priority: 20, WorkflowID: "default", WorkflowRevision: 3, Version: 1},
		{ID: "app", State: "Pending", Priority: 10, WorkflowID: "default", WorkflowRevision: 3, Version: 1},
		{ID: "docs", State: "Escalated", Priority: 15, EscalationKind: "AttemptCap", Reason: "attempt limit 3 reached", WorkflowID: "default", WorkflowRevision: 3, MoleculeID: "m1", Version: 7},
		{ID: "old", State: "Cancelled", Priority: 10, WorkflowID: "default", WorkflowRevision: 3, Version: 2},
		{ID: "cleanup", State: "Pending", Priority: 10, WorkflowID: "default", WorkflowRevision: 3, Version: 1},
		{ID: "notes", State: "Pending", Priority: 10, WorkflowID: "default", WorkflowRevision: 3, Version: 1},
	}
	d := &fakeDaemon{
		status: client.Status{Instance: "dev", PID: 4242, Uptime: 3600, Adapter: "scripted", Snapshot: &client.Summary{Active: 3}},
		board:  client.Board{Tasks: tasks, SnapshotAt: at(0), SnapshotAge: 1500 * time.Millisecond},
		backlog: client.Backlog{
			Items: []client.BacklogItem{{ID: "api", Priority: 20, State: "Pending"}, {ID: "app", Priority: 10, State: "Pending"}},
			Edges: []client.Edge{
				{From: "lib", To: "api", Kind: "blocking"}, {From: "parser", To: "api", Kind: "blocking"},
				{From: "api", To: "app", Kind: "blocking"}, {From: "old", To: "cleanup", Kind: "blocking"},
				{From: "docs", To: "notes", Kind: "relates"},
			},
			Ready:     []string{"notes"},
			Withheld:  map[string][]string{"api": {"parser: implement"}, "app": {"api: Pending"}, "cleanup": {"old: Cancelled"}},
			DeadEdges: []client.DeadEdge{{Edge: &client.Edge{From: "old", To: "cleanup", Kind: "blocking"}, Outcome: "Cancelled"}},
		},
		topology: client.Topology{Molecules: []client.Molecule{{
			ID: "m1", WorkflowID: "default", Revision: 3, CreatedAt: at(120), RoleOrder: []string{"spec", "coder", "reviewer"},
			Stages: []client.Stage{
				{Stage: "Pending", NormalPath: true, Tasks: []string{}},
				{Stage: "spec", Primitive: "AgentTurn", Roles: []string{"spec"}, NormalPath: true, Tasks: []string{}},
				{Stage: "implement", Primitive: "AgentTurn", Roles: []string{"coder"}, NormalPath: true, Tasks: []string{"parser"}},
				{Stage: "review", Primitive: "Review", Roles: []string{"reviewer"}, NormalPath: true, Tasks: []string{}},
				{Stage: "Done", NormalPath: true, Tasks: []string{}},
				{Stage: "fix", Primitive: "AgentTurn", Roles: []string{"coder"}, Tasks: []string{}},
				{Stage: "Blocked", Tasks: []string{}},
				{Stage: "Escalated", Tasks: []string{"docs"}},
			},
		}}},
		alerts: []client.Alert{{ID: 7, Severity: "warning", Source: "route-exhaustion", Task: "docs", Detail: json.RawMessage(`{"reason":"no eligible route for 30m"}`), CreatedAt: at(5)}},
		inbox:  client.Inbox{Escalated: []client.InboxTask{{Task: "docs", State: "Escalated", Reason: "attempt limit 3 reached", Workflow: "default", Revision: 3}}},
		details: map[string]client.TaskDetail{
			"parser": {
				ID: "parser", Version: 5, Title: "Write the parser", Content: "Parse the input format.\nReject malformed input.", AcceptanceDigest: "abc123",
				Priority: 20, State: "implement", Stage: "implement", InFlight: true, Workflow: "default", WorkflowRevision: 3, Molecule: "m1",
				DeclaredFiles: []string{"src/parser.x", "tests/parser.t"}, TestFilter: "parser",
				EdgesOut: []client.EdgeRef{{Other: "api", Kind: "blocking", OtherState: "Pending"}},
				Trace: []client.TraceEntry{
					{Stage: "Pending", EnteredAt: at(60), LeftAt: left(50), Outcome: "released"},
					{Stage: "spec", EnteredAt: at(50), LeftAt: left(30), Outcome: "completed d-1", Attempt: 1},
					{Stage: "implement", EnteredAt: at(30), Attempt: 1},
				},
				Edits:  []client.Edit{{At: at(40), Fields: []string{"title", "priority"}, Message: "msg-edit"}},
				Source: "TaskGet",
			},
			"docs": {
				ID: "docs", Version: 7, Title: "Write the docs", Content: "Document it.", Priority: 15, State: "Escalated", EscalationKind: "AttemptCap",
				EscalationReason: "attempt limit 3 reached", Workflow: "default", WorkflowRevision: 3,
				Trace: []client.TraceEntry{
					{Stage: "Pending", EnteredAt: at(240), LeftAt: left(200)},
					{Stage: "write", EnteredAt: at(200), LeftAt: left(120), Attempt: 3, Outcome: "attempt cap"},
					{Stage: "Escalated", EnteredAt: at(120)},
				},
				Source: "TaskGet",
			},
			"api": {
				ID: "api", Version: 1, Title: "API layer", Content: "Expose it.", Priority: 20, State: "Pending",
				EdgesIn:  []client.EdgeRef{{Other: "lib", Kind: "blocking", OtherState: "Done"}, {Other: "parser", Kind: "blocking", OtherState: "implement"}},
				EdgesOut: []client.EdgeRef{{Other: "app", Kind: "blocking", OtherState: "Pending"}},
				Trace:    []client.TraceEntry{{Stage: "Pending", EnteredAt: at(70)}},
				Source:   "TaskGet",
			},
		},
	}
	return d
}

// job is one spawned command, run by the test instead of the runtime.
type job struct {
	name string
	f    func() tea.Msg
}

// harness drives a Model without a terminal: spawned commands queue, ticks
// are recorded, and the test runs both explicitly.
type harness struct {
	t     *testing.T
	m     *Model
	d     *fakeDaemon
	jobs  []job
	ticks []tea.Msg
	// delays are the intervals the ticks were scheduled with.
	delays []time.Duration
}

func newHarness(t *testing.T, opts Options, d *fakeDaemon) *harness {
	t.Helper()
	h := harnessFor(t, opts, d)
	h.d = d
	h.m.now = func() time.Time { return fixedNow }
	return h
}

// harnessFor drives a Model over any Daemon, on the real clock.
func harnessFor(t *testing.T, opts Options, d Daemon) *harness {
	t.Helper()
	if opts.Instance == "" {
		opts.Instance = "dev"
	}
	h := &harness{t: t, m: New(opts, d)}
	h.m.spawn = func(name string, f func() tea.Msg) tea.Cmd {
		h.jobs = append(h.jobs, job{name, f})
		return nil
	}
	h.m.after = func(d time.Duration, msg tea.Msg) tea.Cmd {
		h.ticks = append(h.ticks, msg)
		h.delays = append(h.delays, d)
		return nil
	}
	h.m.width, h.m.height = 160, 45
	return h
}

// start runs Init and every job it queued.
func (h *harness) start() *harness {
	h.m.Init()
	h.drain()
	return h
}

// send delivers one message and renders, as the runtime does after every
// update.
func (h *harness) send(msg tea.Msg) tea.Cmd {
	_, cmd := h.m.Update(msg)
	h.m.View()
	return cmd
}

// queued lists the names of the jobs waiting to run.
func (h *harness) queued() []string {
	out := []string{}
	for _, j := range h.jobs {
		out = append(out, j.name)
	}
	return out
}

// drain runs queued jobs, feeding each result back, until none is left.
func (h *harness) drain() {
	h.t.Helper()
	for n := 0; len(h.jobs) > 0; n++ {
		if n > 200 {
			h.t.Fatal("jobs keep queueing more jobs")
		}
		j := h.jobs[0]
		h.jobs = h.jobs[1:]
		h.send(j.f())
	}
}

func (h *harness) press(keys ...string) {
	for _, k := range keys {
		h.send(key(k))
	}
}

func (h *harness) typeText(s string) {
	for _, r := range s {
		h.send(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
}

func (h *harness) screen() string { return ansi.Strip(h.m.View().Content) }

// focusField tabs through the open form until the named field has focus.
func (h *harness) focusField(key string) {
	h.t.Helper()
	f, ok := h.m.modal.(*form)
	if !ok {
		h.t.Fatalf("no form is open to focus %s", key)
	}
	for range len(f.fields) {
		if f.fields[f.focus].key == key {
			return
		}
		h.press("tab")
	}
	h.t.Fatalf("the form has no field %s", key)
}

// poll delivers a tick and runs the refresh it starts.
func (h *harness) poll() {
	h.send(tickMsg{})
	h.drain()
}

// lastDelay is the interval the latest poll tick was scheduled with.
func (h *harness) lastDelay() time.Duration {
	for i := len(h.ticks) - 1; i >= 0; i-- {
		if h.ticks[i] == (tickMsg{}) {
			return h.delays[i]
		}
	}
	h.t.Fatal("no poll tick was scheduled")
	return 0
}

func (h *harness) contains(want ...string) {
	h.t.Helper()
	screen := h.screen()
	for _, w := range want {
		if !strings.Contains(screen, w) {
			h.t.Fatalf("screen lacks %q:\n%s", w, screen)
		}
	}
}

func (h *harness) lacks(unwanted ...string) {
	h.t.Helper()
	screen := h.screen()
	for _, w := range unwanted {
		if strings.Contains(screen, w) {
			h.t.Fatalf("screen shows %q:\n%s", w, screen)
		}
	}
}

func key(s string) tea.KeyPressMsg {
	special := map[string]tea.KeyPressMsg{
		"enter": {Code: tea.KeyEnter}, "esc": {Code: tea.KeyEscape}, "tab": {Code: tea.KeyTab},
		"shift+tab": {Code: tea.KeyTab, Mod: tea.ModShift}, "up": {Code: tea.KeyUp}, "down": {Code: tea.KeyDown},
		"left": {Code: tea.KeyLeft}, "right": {Code: tea.KeyRight}, "pgdown": {Code: tea.KeyPgDown}, "pgup": {Code: tea.KeyPgUp},
		"backspace": {Code: tea.KeyBackspace}, "ctrl+s": {Code: 's', Mod: tea.ModCtrl}, "ctrl+c": {Code: 'c', Mod: tea.ModCtrl},
	}
	if k, ok := special[s]; ok {
		return k
	}
	r := []rune(s)[0]
	return tea.KeyPressMsg{Code: r, Text: s}
}

// index is the position of text on the screen, failing when absent.
func (h *harness) index(text string) int {
	h.t.Helper()
	i := strings.Index(h.screen(), text)
	if i < 0 {
		h.t.Fatalf("screen lacks %q:\n%s", text, h.screen())
	}
	return i
}
