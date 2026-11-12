package tui

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/xleelz/fae/tui/client"
)

// Options are fae-tui's flags.
type Options struct {
	Home, Instance string
	// Write enables the task-editing writes; without it the TUI is
	// read-only, as a UI child is by default (FAE-API-032).
	Write bool
	// Refresh is the base polling interval: each poll reads what the
	// current view shows, and the interval stretches up to backoffMax times
	// while nothing changes (pollInterval).
	Refresh time.Duration
}

// minWidth and minHeight are the smallest terminal the views draw in; a
// smaller one shows only a notice (render).
const (
	minWidth  = 40
	minHeight = 12
)

// Polling backs off after backoffAfter unchanged refreshes, doubling the
// interval each further unchanged refresh up to backoffMax times the base.
const (
	backoffAfter = 5
	backoffMax   = 8
)

type viewID int

const (
	viewMolecules viewID = iota
	viewGraph
	viewAttention
	viewStream
	viewPools
	viewSymphony
	viewSecurity
)

var viewNames = []string{"Molecules", "Graph", "Attention", "Stream", "Pools", "Symphony", "Security"}

// tickMsg polls the daemon; clockMsg redraws the snapshot age.
type (
	tickMsg  struct{}
	clockMsg struct{}
)

// Model is the TUI's bubbletea model. Every daemon call runs in a command
// (spawn), never in Update or View, so a slow or absent daemon never blocks
// the interface.
type Model struct {
	opts  Options
	d     Daemon
	spawn func(name string, f func() tea.Msg) tea.Cmd
	after func(d time.Duration, msg tea.Msg) tea.Cmd
	now   func() time.Time

	width, height int
	view          viewID
	help          bool

	snap        snapshot
	errs        map[string]error
	unreachable error
	fetching    bool
	// refreshAgain asks for one more refresh as soon as the one in flight
	// lands: the view changed while it ran.
	refreshAgain bool
	// unchanged counts the consecutive refreshes that brought no new data.
	unchanged int

	includeRetired bool
	hideFinished   bool

	mol      moleculesState
	graph    graphState
	att      attentionState
	stream   streamState
	pools    poolsState
	symphony symphonyState
	security securityState
	detail   *detailState

	filter    string
	filtering bool

	modal   modal
	writing bool
	// pending is an edit or delete waiting for its task's detail read;
	// pendingEdits holds the field values modified before a stale-version
	// refusal so reloading the task preserves what the operator typed.
	pending      string
	pendingEdits map[string]string

	since     map[string]time.Time
	sinceKey  map[string]int64
	sinceBusy bool
	// pendingDel is each escalated task's stored pending delete, from the
	// same TaskGet reads as since (nil: none).
	pendingDel map[string]*client.PendingDelete

	flash          string
	flashError     bool
	childJumpIdx   int
	childJumpTasks []string
	extVisited     bool
}

// New returns the model for a daemon.
func New(opts Options, d Daemon) *Model {
	if opts.Refresh <= 0 {
		opts.Refresh = 2 * time.Second
	}
	return &Model{
		opts:  opts,
		d:     d,
		spawn: func(_ string, f func() tea.Msg) tea.Cmd { return f },
		after: func(d time.Duration, msg tea.Msg) tea.Cmd {
			return tea.Tick(d, func(time.Time) tea.Msg { return msg })
		},
		now:        time.Now,
		snap:       snapshot{details: map[string]client.TaskDetail{}, loaded: map[string]bool{}},
		errs:       map[string]error{},
		since:      map[string]time.Time{},
		sinceKey:   map[string]int64{},
		pendingDel: map[string]*client.PendingDelete{},
		width:      100,
		height:     30,
	}
}

// Init starts the first refresh, the poll and the age clock.
func (m *Model) Init() tea.Cmd {
	return tea.Batch(m.refresh(), m.after(m.opts.Refresh, tickMsg{}), m.after(time.Second, clockMsg{}))
}

// refresh starts a refresh unless one is in flight (then one more follows
// it). The first refresh reads every part; later ones read what the current
// view shows (partsFor).
func (m *Model) refresh() tea.Cmd {
	if m.fetching {
		m.refreshAgain = true
		return nil
	}
	m.fetching = true
	wanted := parts
	if m.snap.generation > 0 {
		wanted = partsFor(m.view)
	}
	return m.spawn("refresh", fetchSnapshot(m.d, wanted, m.includeRetired, m.now))
}

// pollInterval is the time to the next poll: the base --refresh interval,
// stretched while nothing changes (after backoffAfter unchanged refreshes it
// doubles per refresh) and raised to at least the daemon's own snapshot age,
// then capped at backoffMax times the base (and at most 10 minutes).
// Anything that changes, a key or a write brings it back to the base.
func (m *Model) pollInterval() time.Duration {
	base := m.opts.Refresh
	d := base
	if m.unchanged >= backoffAfter {
		d = base << min(m.unchanged-backoffAfter+1, 3)
	}
	if age := m.snap.board.SnapshotAge; m.snap.loaded[partBoard] && age > d {
		d = age
	}
	return min(d, backoffMax*base, 10*time.Minute)
}

// switchView shows a view and refreshes what it shows.
func (m *Model) switchView(v viewID) tea.Cmd {
	m.detail = nil
	m.view = v
	if v >= viewStream {
		m.extVisited = true
	}
	m.unchanged = 0
	return m.refresh()
}

func (m *Model) hasExtendedViews() bool {
	if m.extVisited {
		return true
	}
	if _, ok := m.d.(ExtendedDaemon); ok {
		return true
	}
	return len(m.snap.panes) > 0 || len(m.snap.pools) > 0 || len(m.snap.pauses) > 0 || len(m.snap.health) > 0 ||
		len(m.snap.targets) > 0 || len(m.snap.events) > 0 || len(m.snap.symphony.Running) > 0 ||
		len(m.snap.symphony.Retrying) > 0 || len(m.snap.symphony.KnownIssues) > 0
}

func (m *Model) say(text string, isError bool) {
	m.flash, m.flashError = text, isError
}

// Update handles one message.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		if f, ok := m.modal.(*form); ok {
			f.resize(m.width)
		}
		return m, nil
	case tickMsg:
		return m, tea.Batch(m.refresh(), m.after(m.pollInterval(), tickMsg{}))
	case clockMsg:
		return m, m.after(time.Second, clockMsg{})
	case snapshotMsg:
		return m, m.applySnapshot(msg)
	case detailMsg:
		if msg.err == nil {
			if m.snap.details == nil {
				m.snap.details = map[string]client.TaskDetail{}
			} else {
				m.snap.details = maps(m.snap.details)
			}
			m.snap.details[msg.id] = msg.detail
		}
		if msg.err == nil && m.snap.tasks != nil {
			if t, ok := m.snap.tasks[msg.id]; ok {
				tasks := maps(m.snap.tasks)
				if msg.detail.Parent != nil {
					t.Parent = msg.detail.Parent
				}
				if msg.detail.Child != nil {
					t.Child = msg.detail.Child
				}
				tasks[msg.id] = t
				m.snap.tasks = tasks
			}
		}
		if m.detail == nil || m.detail.id != msg.id {
			return m, nil
		}
		m.detail.loading = false
		if msg.err != nil {
			m.detail.err = msg.err
			if m.pending != "" {
				m.pending = ""
				m.pendingEdits = nil
				m.say("could not read task "+msg.id+": "+msg.err.Error(), true)
			}
			return m, nil
		}
		detail := msg.detail
		m.detail.data, m.detail.err = &detail, nil
		if detail.PendingDelete != nil {
			p := *detail.PendingDelete
			m.pendingDel[msg.id] = &p
		} else {
			delete(m.pendingDel, msg.id)
		}
		switch action := m.pending; action {
		case "edit":
			m.pending = ""
			return m, m.startUpdate(msg.id)
		case "delete":
			m.pending = ""
			return m, m.startDelete(msg.id)
		}
		return m, nil
	case sinceMsg:
		m.sinceBusy = false
		for id, at := range msg.since {
			m.since[id], m.sinceKey[id] = at, msg.keys[id]
			if p := msg.pending[id]; p != nil {
				m.pendingDel[id] = p
			} else {
				delete(m.pendingDel, id)
			}
		}
		return m, nil
	case writeMsg:
		m.writing = false
		m.unchanged = 0
		m.modal = newResult(msg)
		if needsAcceptance(msg.err) && msg.accept != nil {
			m.modal = &confirm{title: msg.verb + " needs your acceptance in flight", lines: []string{
				"The daemon refused: " + msg.err.Error() + ".",
				"The dependent was dispatched before. Accepting sets a condition for its next release; it does not interrupt a turn in flight.",
				"Send it again, accepting the edge in flight?",
			}, yes: msg.accept}
		}
		if action := map[string]string{"TaskUpdate": "edit", "TaskDelete": "delete"}[msg.verb]; client.StaleVersion(msg.err) && action != "" && msg.task != "" {
			// The form was filled from a task that has since changed:
			// nothing is retried; the task is read again and the dialog
			// reopens on its current version while keeping the user's edits.
			id, edits := msg.task, msg.edits
			m.modal = &confirm{title: msg.verb + " refused: task " + id + " changed since you opened the form", lines: []string{
				"The daemon refused: " + msg.err.Error() + ".",
				"Nothing was changed. Reload the task and open the " + action + " dialog again on its current values?",
			}, yes: func(m *Model) tea.Cmd {
				m.modal = nil
				m.pending = action
				m.pendingEdits = edits
				m.say("reading task "+id+" again…", false)
				return m.openDetail(id)
			}}
		}
		if msg.err != nil {
			m.say(msg.verb+" refused", true)
		} else {
			m.say(msg.verb+" done", false)
		}
		if msg.verb == "TaskDelete" && msg.err == nil {
			// The task is retired: its view would only say it is hidden.
			m.detail = nil
		}
		cmds := []tea.Cmd{m.refresh()}
		if m.detail != nil && !m.detail.loading {
			cmds = append(cmds, m.loadDetail())
		}
		return m, tea.Batch(cmds...)
	case tea.KeyPressMsg:
		return m, m.key(msg)
	}
	if m.modal != nil {
		return m, m.modal.update(m, msg)
	}
	return m, nil
}

func (m *Model) applySnapshot(msg snapshotMsg) tea.Cmd {
	m.fetching = false
	if m.flash == "refreshing…" {
		m.say("", false)
	}
	m.unreachable = msg.unreachable()
	m.errs = msg.errs
	var changed bool
	m.snap, changed = m.snap.merge(msg)
	if m.mol.wantTask != "" && m.selectMoleculeTask(m.mol.wantTask) {
		m.mol.wantTask = ""
	}
	if changed || len(msg.errs) > 0 {
		m.unchanged = 0
	} else {
		m.unchanged++
	}
	cmds := []tea.Cmd{}
	if m.refreshAgain {
		m.refreshAgain = false
		cmds = append(cmds, m.refresh())
	}
	if m.detail != nil && !m.detail.loading {
		cmds = append(cmds, m.loadDetail())
	}
	if cmd := m.loadSince(); cmd != nil {
		cmds = append(cmds, cmd)
	}
	return tea.Batch(cmds...)
}

// loadSince reads when each escalated task escalated, once per Board version.
func (m *Model) loadSince() tea.Cmd {
	if m.sinceBusy || m.unreachable != nil {
		return nil
	}
	ids := []string{}
	versions := map[string]int64{}
	for _, t := range m.snap.sortedTasks() {
		if t.State != "Escalated" && !t.PendingDelete {
			continue
		}
		if v, ok := m.sinceKey[t.ID]; ok && v == t.Version {
			continue
		}
		ids, versions[t.ID] = append(ids, t.ID), t.Version
		if len(ids) == maxSinceReads {
			break
		}
	}
	if len(ids) == 0 {
		return nil
	}
	m.sinceBusy = true
	return m.spawn("since", fetchSince(m.d, ids, versions, m.snap.have(), m.includeRetired))
}

// openDetail pushes the task view and reads it.
func (m *Model) openDetail(id string) tea.Cmd {
	if id == "" {
		return nil
	}
	m.detail = &detailState{id: id}
	return m.loadDetail()
}

func (m *Model) loadDetail() tea.Cmd {
	if m.detail == nil {
		return nil
	}
	m.detail.loading = true
	return m.spawn("detail", fetchDetail(m.d, m.detail.id, m.snap.have(), m.includeRetired))
}

// key routes a key press: a modal takes every key; then global keys; then
// the task view or the current view.
func (m *Model) key(k tea.KeyPressMsg) tea.Cmd {
	s := k.String()
	if s == "ctrl+c" {
		return tea.Quit
	}
	if m.modal != nil {
		return m.modal.update(m, k)
	}
	if m.help {
		m.help = false
		return nil
	}
	if m.filtering {
		switch s {
		case "enter", "esc":
			m.filtering = false
			return nil
		case "backspace":
			r := []rune(m.filter)
			if len(r) > 0 {
				m.filter = string(r[:len(r)-1])
			}
			return nil
		}
		if k.Text != "" {
			m.filter += k.Text
		} else if len([]rune(s)) == 1 {
			m.filter += s
		}
		return nil
	}
	if m.detail == nil && s == "esc" && m.filter != "" {
		m.filter = ""
		return nil
	}
	if m.view == viewAttention && m.detail == nil && m.opts.Write {
		switch s {
		case "d", "u", "R", "I", "w", "G", "X":
			if ok, cmd := m.attentionAct(s); ok {
				return cmd
			}
		}
	}
	if m.view == viewPools && m.detail == nil {
		switch s {
		case "s":
			poolID := ""
			if p := m.poolsSelectedPool(); p != nil {
				poolID = p.ID
			}
			return m.startPoolResize(poolID)
		case "q":
			if m.opts.Write {
				prov, mod, poolID := m.defaultQuotaTarget()
				if p := m.poolsSelectedPool(); p != nil {
					poolID = p.ID
					if prov == "" {
						prov = p.Provider
					}
				}
				return m.startQuotaSet(prov, mod, poolID)
			}
		case "Q":
			prov, mod, poolID := m.defaultQuotaTarget()
			if p := m.poolsSelectedPool(); p != nil {
				poolID = p.ID
				if prov == "" {
					prov = p.Provider
				}
			}
			return m.startQuotaClear(prov, mod, poolID)
		case "p":
			scope, target := "instance", ""
			if pa := m.poolsSelectedPause(); pa != nil && pa.Scope != "" {
				scope, target = pa.Scope, pa.Target
			} else if p := m.poolsSelectedPool(); p != nil && p.ID != "" {
				scope, target = "pool", p.ID
			}
			return m.startPause(scope, target)
		case "u":
			scope, target := "instance", ""
			if pa := m.poolsSelectedPause(); pa != nil && pa.Scope != "" {
				scope, target = pa.Scope, pa.Target
			} else if p := m.poolsSelectedPool(); p != nil && p.ID != "" {
				scope, target = "pool", p.ID
			}
			return m.startUnpause(scope, target)
		}
	}
	switch s {
	case "q":
		return tea.Quit
	case "?":
		m.help = true
		return nil
	case ":", "ctrl+p":
		return m.startPalette()
	case "/":
		if m.detail == nil && m.view != viewGraph {
			m.filtering = true
			return nil
		}
	case "r":
		m.say("refreshing…", false)
		m.unchanged = 0
		cmds := []tea.Cmd{m.refresh()}
		if m.detail != nil && !m.detail.loading {
			cmds = append(cmds, m.loadDetail())
		}
		return tea.Batch(cmds...)
	case "R":
		if m.detail != nil && m.opts.Write && m.isEscalatedTask(m.detail.id) {
			return m.startLifecycle(m.detail.id, "RestartWithInformation", "operator resume escalated task")
		}
		m.includeRetired = !m.includeRetired
		m.say(map[bool]string{true: "showing retired tasks", false: "hiding retired tasks"}[m.includeRetired], false)
		m.unchanged = 0
		return m.refresh()
	case "I":
		return m.startLifecycle(m.currentTask(), "RestartWithInformation", "operator un-escalate and restart")
	case "u":
		if m.isUncertainTask(m.currentTask()) {
			return m.startResolveUncertain(m.currentTask())
		}
		return m.startLifecycle(m.currentTask(), "RestartWithInformation", "operator un-escalate and restart")
	case "1", "2", "3", "4", "5", "6", "7":
		return m.switchView(viewID(s[0] - '1'))
	case "tab":
		return m.switchView((m.view + 1) % viewID(len(viewNames)))
	case "shift+tab":
		if m.view > 0 {
			return m.switchView(m.view - 1)
		}
		if m.hasExtendedViews() {
			return m.switchView(viewID(len(viewNames) - 1))
		}
		return m.switchView(viewAttention)
	case "[":
		return m.jumpToParent()
	case "]":
		return m.jumpToChild()
	case "p":
		if m.view == viewGraph && m.detail == nil {
			return m.graphKey(s)
		}
		return m.jumpToParent()
	case "C":
		if m.detail == nil {
			return m.startCreate()
		}
		return nil
	case "c":
		if c := m.currentChild(); c != nil && (c.Task != "" || len(c.Children) > 0) {
			return m.jumpToChild()
		}
		if len(m.childJumpTasks) > 1 && slices.Contains(m.childJumpTasks, m.currentTask()) {
			return m.jumpToChild()
		}
		if m.detail == nil && m.opts.Write {
			return m.startCreate()
		}
		if m.detail == nil && !m.opts.Write {
			m.say("No active child task (read-only: start fae-tui with --write to create)", true)
			return nil
		}
		m.say("No active child task", true)
		return nil
	case "a":
		if !m.opts.Write {
			m.say("read-only: start fae-tui with --write to run actions", true)
			return nil
		}
		return m.startActionMenu(m.currentTask())
	case "n":
		if m.view == viewGraph && m.detail == nil && !m.opts.Write {
			return m.graphKey(s)
		}
		return m.startNotify(m.currentTask(), false)
	case "N":
		return m.startNotify(m.currentTask(), true)
	case "f":
		if m.view == viewGraph && m.detail == nil {
			return m.graphKey(s)
		}
		return m.startFollowUp(m.currentTask())
	case "w":
		return m.startApproveWiden(m.currentTask())
	case "L":
		return m.startLifecycle(m.currentTask(), "Land", "operator land")
	case "S":
		return m.startSyncSymphony()
	case "e":
		m.pendingEdits = nil
		return m.startUpdate(m.currentTask())
	case "D":
		m.pendingEdits = nil
		return m.startDelete(m.currentTask())
	case "+":
		return m.startEdgeAdd(m.currentTask())
	case "-":
		return m.startEdgeRemove(m.currentTask())
	}
	if m.detail != nil {
		return m.detailKey(s)
	}
	switch m.view {
	case viewMolecules:
		return m.moleculesKey(s)
	case viewGraph:
		return m.graphKey(s)
	case viewAttention:
		return m.attentionKey(s)
	case viewStream:
		return m.streamKey(s)
	case viewPools:
		return m.poolsKey(s)
	case viewSymphony:
		return m.symphonyKey(s)
	case viewSecurity:
		return m.securityKey(s)
	default:
		return m.attentionKey(s)
	}
}

func (m *Model) streamKey(s string) tea.Cmd {
	if s == "enter" {
		return m.openDetail(m.streamSelectedTaskID())
	}
	if delta, ok := moveKey(s, m.height/2); ok {
		m.streamMove(delta)
	}
	return nil
}

func (m *Model) poolsKey(s string) tea.Cmd {
	if delta, ok := moveKey(s, m.height/2); ok {
		m.poolsMove(delta)
	}
	return nil
}

func (m *Model) symphonyKey(s string) tea.Cmd {
	if s == "enter" {
		return m.openDetail(m.symphonySelectedTaskID())
	}
	if delta, ok := moveKey(s, m.height/2); ok {
		m.symphonyMove(delta)
	}
	return nil
}

func (m *Model) securityKey(s string) tea.Cmd {
	if s == "enter" {
		return m.openDetail(m.securitySelectedTaskID())
	}
	if delta, ok := moveKey(s, m.height/2); ok {
		m.securityMove(delta)
	}
	return nil
}

// currentTask is the task a write key acts on: the open task, else the
// selection of the current view.
func (m *Model) currentTask() string {
	if m.detail != nil {
		return m.detail.id
	}
	switch m.view {
	case viewMolecules:
		return m.selectedMoleculeTask()
	case viewGraph:
		return m.graph.sel
	case viewStream:
		return m.streamSelectedTaskID()
	case viewSymphony:
		return m.symphonySelectedTaskID()
	case viewSecurity:
		return m.securitySelectedTaskID()
	case viewPools:
		return ""
	default:
		return m.selectedAttentionTask()
	}
}

func (m *Model) currentParent() *client.ParentRef {
	if m.detail != nil && m.detail.data != nil && m.detail.data.Parent != nil {
		return m.detail.data.Parent
	}
	id := m.currentTask()
	if t, ok := m.snap.tasks[id]; ok {
		return t.Parent
	}
	return nil
}

func (m *Model) currentChild() *client.ChildRef {
	if m.detail != nil && m.detail.data != nil && m.detail.data.Child != nil {
		return m.detail.data.Child
	}
	id := m.currentTask()
	if t, ok := m.snap.tasks[id]; ok {
		return t.Child
	}
	return nil
}

func (m *Model) selectMoleculeTask(id string) bool {
	ms := m.molecules()
	for _, mol := range ms {
		rows := moleculeTaskRows(mol)
		if idx := slices.Index(rows, id); idx >= 0 {
			m.mol.id, m.mol.row, m.mol.offset = mol.ID, idx, 0
			return true
		}
	}
	if t, ok := m.snap.tasks[id]; ok && t.MoleculeID != "" {
		m.mol.id, m.mol.row, m.mol.offset = t.MoleculeID, 0, 0
		return true
	}
	return false
}

func (m *Model) selectTask(id string) {
	if id == "" {
		return
	}
	m.graph.sel = id
	if !m.selectMoleculeTask(id) {
		m.mol.wantTask = id
	} else {
		m.mol.wantTask = ""
	}
	for i, r := range m.attentionRows() {
		if r.task == id {
			m.att.row = i
			break
		}
	}
}

func (m *Model) refreshWithTopology() tea.Cmd {
	if m.fetching {
		m.refreshAgain = true
		return nil
	}
	m.fetching = true
	wanted := parts
	if m.snap.generation > 0 {
		wanted = slices.Clone(partsFor(m.view))
		if !slices.Contains(wanted, partTopology) {
			wanted = append(wanted, partTopology)
		}
	}
	return m.spawn("refresh", fetchSnapshot(m.d, wanted, m.includeRetired, m.now))
}

func (m *Model) jumpToTask(id, relation string) tea.Cmd {
	return m.jumpToTaskWithFlash(id, fmt.Sprintf("Jumped to %s task %s", relation, id))
}

func (m *Model) jumpToTaskWithFlash(id, flash string) tea.Cmd {
	m.selectTask(id)
	m.unchanged = 0
	m.say(flash, false)
	var detailCmd tea.Cmd
	if m.detail != nil {
		m.detail = &detailState{id: id}
		detailCmd = m.loadDetail()
	} else {
		detailCmd = m.spawn("detail", fetchDetail(m.d, id, m.snap.have(), m.includeRetired))
	}
	return tea.Batch(detailCmd, m.refreshWithTopology())
}

func (m *Model) jumpToParent() tea.Cmd {
	p := m.currentParent()
	if p == nil || p.Task == "" {
		m.say("No parent task", true)
		return nil
	}
	return m.jumpToTask(p.Task, "parent")
}

func (m *Model) jumpToChild() tea.Cmd {
	c := m.currentChild()
	if (c == nil || (c.Task == "" && len(c.Children) == 0)) && len(m.childJumpTasks) > 1 && slices.Contains(m.childJumpTasks, m.currentTask()) {
		idx := m.childJumpIdx % len(m.childJumpTasks)
		m.childJumpIdx = (idx + 1) % len(m.childJumpTasks)
		target := m.childJumpTasks[idx]
		return m.jumpToTaskWithFlash(target, fmt.Sprintf("Jumped to child task %s (%d/%d)", target, idx+1, len(m.childJumpTasks)))
	}
	if c == nil || (c.Task == "" && len(c.Children) == 0) {
		m.say("No active child task", true)
		return nil
	}
	if len(c.Children) > 1 {
		tasks := make([]string, 0, len(c.Children))
		for _, sub := range c.Children {
			tasks = append(tasks, sub.Task)
		}
		if !slices.Equal(m.childJumpTasks, tasks) {
			m.childJumpTasks = tasks
			m.childJumpIdx = 0
		}
		idx := m.childJumpIdx % len(c.Children)
		m.childJumpIdx = (idx + 1) % len(c.Children)
		target := c.Children[idx].Task
		return m.jumpToTaskWithFlash(target, fmt.Sprintf("Jumped to child task %s (%d/%d)", target, idx+1, len(c.Children)))
	}
	m.childJumpTasks = nil
	m.childJumpIdx = 0
	target := c.Task
	if len(c.Children) == 1 && c.Children[0].Task != "" {
		target = c.Children[0].Task
	}
	return m.jumpToTask(target, "child")
}

// writable reports whether writes are on, and says why not when they are off.
func (m *Model) writable(action string) bool {
	if !m.opts.Write {
		m.say("read-only: start fae-tui with --write to "+action, true)
		return false
	}
	if m.writing {
		m.say("a write is already in flight", true)
		return false
	}
	return true
}

// View renders the screen.
func (m *Model) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	v.WindowTitle = "fae-tui " + m.opts.Instance
	return v
}

func (m *Model) render() string {
	w, h := m.width, m.height
	if w <= 0 || h <= 0 {
		return ""
	}
	if w < minWidth || h < minHeight {
		return m.tooSmall(w, h)
	}
	top := m.header(w)
	top = append(top, m.tabs(w))
	bottom := m.footer(w)
	bodyHeight := h - len(top) - len(bottom)
	var body []string
	switch {
	case m.help:
		body = m.helpScreen(w)
	case m.modal != nil:
		lines, focus := m.modal.view(w)
		body = boxed(lines, focus, w, bodyHeight)
	case m.detail != nil:
		body = m.detailBody(w, bodyHeight)
	case m.view == viewMolecules:
		body = m.moleculesBody(w, bodyHeight)
	case m.view == viewGraph:
		body = m.graphBody(w, bodyHeight)
	case m.view == viewAttention:
		body = m.attentionBody(w, bodyHeight)
	case m.view == viewStream:
		body = m.streamView(bodyHeight)
	case m.view == viewPools:
		body = m.poolsView(bodyHeight)
	case m.view == viewSymphony:
		body = m.symphonyView(bodyHeight)
	case m.view == viewSecurity:
		body = m.securityView(bodyHeight)
	default:
		body = m.attentionBody(w, bodyHeight)
	}
	lines := append([]string{}, top...)
	for i := range bodyHeight {
		line := ""
		if i < len(body) {
			line = body[i]
		}
		lines = append(lines, line)
	}
	lines = append(lines, bottom...)
	for i, line := range lines {
		lines[i] = fit(line, w)
	}
	return strings.Join(lines, "\n")
}

// tooSmall fills a terminal below the minimum size with a notice, so
// nothing is drawn past its edges.
func (m *Model) tooSmall(w, h int) string {
	lines := []string{
		styleBold.Render("terminal too small"),
		fmt.Sprintf("%dx%d < %dx%d", w, h, minWidth, minHeight),
		"q quits",
	}
	if m.unreachable != nil {
		lines = append(lines, styleError.Render("daemon unreachable"))
	}
	out := make([]string, 0, h)
	for i := range h {
		line := ""
		if i < len(lines) {
			line = lines[i]
		}
		out = append(out, fit(line, w))
	}
	return strings.Join(out, "\n")
}

func (m *Model) header(w int) []string {
	now := m.now()
	mode := styleFaint.Render("read-only")
	if m.opts.Write {
		mode = styleWrite.Render(" WRITES ON ")
	}
	fields := []string{styleBold.Render("fae-tui"), "instance " + m.opts.Instance, mode}
	if st := m.snap.status; m.snap.loaded[partStatus] {
		fields = append(fields, fmt.Sprintf("daemon pid %d, up %s", st.PID, age(time.Duration(st.Uptime*float64(time.Second)))))
		if st.ShuttingDown {
			fields = append(fields, styleError.Render("shutting down"))
		}
		if sum := st.Snapshot; sum != nil && sum.Draining {
			fields = append(fields, styleWarn.Render("draining"))
		}
	}
	switch {
	case m.snap.generation == 0 && m.fetching:
		fields = append(fields, "connecting…")
	case m.snap.loaded[partBoard]:
		fields = append(fields, "snapshot "+age(m.snap.board.SnapshotAge+now.Sub(m.snap.at))+" old")
	}
	if m.fetching && m.snap.generation > 0 {
		fields = append(fields, styleFaint.Render("refreshing"))
	}
	if d := m.pollInterval(); d != m.opts.Refresh {
		fields = append(fields, styleFaint.Render("idle: polling every "+age(d)))
	}
	lines := []string{strings.Join(fields, " · ")}
	if m.unreachable != nil {
		line := styleError.Render("daemon unreachable") + ": " + m.unreachable.Error() + fmt.Sprintf(" — retrying every %s", m.opts.Refresh)
		if m.snap.loaded[partBoard] {
			line += fmt.Sprintf("; showing data from %s", ago(now, m.snap.at))
		}
		lines = append(lines, line)
	} else if len(m.errs) > 0 {
		failed := []string{}
		for _, part := range parts {
			if err := m.errs[part]; err != nil {
				failed = append(failed, part+": "+err.Error())
			}
		}
		lines = append(lines, styleWarn.Render("read failed")+" (previous data kept) "+strings.Join(failed, "; "))
	}
	return lines
}

func (m *Model) tabs(w int) string {
	streamCount := len(m.snap.panes)
	if streamCount == 0 {
		for _, t := range m.snap.tasks {
			if t.InFlight {
				streamCount++
			}
		}
	}
	poolCount := len(m.snap.pools)
	if poolCount == 0 {
		poolCount = len(m.snap.status.ProviderPools)
	}
	symCount := len(m.snap.symphony.Running) + len(m.snap.symphony.Retrying) + len(m.snap.symphony.KnownIssues)
	secCount := len(m.snap.targets) + len(m.snap.events)
	counts := []string{
		fmt.Sprintf("%d", len(m.snap.topology.Molecules)),
		fmt.Sprintf("%d", len(m.snap.tasks)),
		fmt.Sprintf("%d", len(m.attentionRows())),
		fmt.Sprintf("%d", streamCount),
		fmt.Sprintf("%d", poolCount),
		fmt.Sprintf("%d", symCount),
		fmt.Sprintf("%d", secCount),
	}
	out := []string{}
	for i, name := range viewNames {
		label := fmt.Sprintf(" %d %s (%s) ", i+1, name, counts[i])
		if viewID(i) == m.view && m.detail == nil {
			label = styleSelected.Render(label)
		}
		out = append(out, label)
	}
	line := strings.Join(out, "│")
	if m.detail != nil {
		line += "│" + styleSelected.Render(" task "+m.detail.id+" ")
	}
	return line
}

func (m *Model) footerBar() string {
	return m.helpBar()
}

func (m *Model) footer(w int) []string {
	flash := ""
	if m.filtering {
		flash = styleKey.Render("/") + m.filter + "█  " + styleFaint.Render("(type to filter · enter/esc done)")
	} else if m.writing {
		flash = styleWarn.Render("writing…")
	} else if m.flash != "" {
		if m.flashError {
			flash = styleError.Render(m.flash)
		} else {
			flash = styleOK.Render(m.flash)
		}
	} else if m.filter != "" {
		flash = styleWarn.Render("filter: "+m.filter) + styleFaint.Render(" (esc clears)")
	}
	return []string{flash, m.helpBar()}
}

// writeMsg is one write's outcome.
type writeMsg struct {
	verb    string
	task    string
	edits   map[string]string
	message string
	lines   []string
	err     error
	accept  func(m *Model) tea.Cmd
}

// writeAction is one write: the operator message that authorizes it, then
// its RPC.
type writeAction struct {
	verb    string
	summary string
	class   string
	// task is the task an edit or delete acts on, for the stale-version
	// reload offer; edits are the modified field values to preserve.
	task  string
	edits map[string]string
	run   func(ctx context.Context, d Daemon, message string) ([]string, error)
	// accept, when set, sends the write again with the operator's
	// acceptance in flight, if the daemon refuses it for want of one.
	accept func(m *Model) tea.Cmd
}

// runWrite records the authorizing message, then sends the write citing it.
// Nothing is sent when the message is refused.
func (m *Model) runWrite(a writeAction) tea.Cmd {
	if !m.writable("make changes") {
		return nil
	}
	m.writing = true
	m.modal = nil
	m.say("", false)
	d := m.d
	return m.spawn("write:"+a.verb, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
		defer cancel()
		id, err := d.Message(ctx, client.Authorization(a.summary, a.class), "")
		if err != nil {
			return writeMsg{verb: a.verb, err: fmt.Errorf("the authorizing message was refused, so nothing was sent: %w", err)}
		}
		lines, err := a.run(ctx, d, id)
		if client.Unsupported(err) && strings.HasPrefix(a.verb, "Task") {
			err = fmt.Errorf("%w (this daemon has no %s: it predates live task editing)", err, a.verb)
		}
		return writeMsg{verb: a.verb, task: a.task, edits: a.edits, message: id, lines: lines, err: err, accept: a.accept}
	})
}

// selectable clamps i into [0, n).
func selectable(i, n int) int {
	if n <= 0 {
		return 0
	}
	return min(max(i, 0), n-1)
}

func moveKey(s string, page int) (delta int, ok bool) {
	switch s {
	case "up", "k":
		return -1, true
	case "down", "j":
		return 1, true
	case "pgup":
		return -max(page, 1), true
	case "pgdown":
		return max(page, 1), true
	case "home":
		return -1 << 30, true
	case "end":
		return 1 << 30, true
	}
	return 0, false
}

// joinIDs lists ids for a message, at most ten.
func joinIDs(ids []string) string {
	if len(ids) > 10 {
		return strings.Join(ids[:10], ", ") + fmt.Sprintf(" and %d more", len(ids)-10)
	}
	return strings.Join(slices.Clone(ids), ", ")
}
