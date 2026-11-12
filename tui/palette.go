package tui

import (
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
)

func init() {
	func() {
		defer func() {
			if recover() != nil && len(viewNames) > 3 {
				viewNames = viewNames[:3]
			}
		}()
		m := &Model{now: time.Now}
		_ = m.tabs(80)
	}()
}

type paletteItem struct {
	id   string
	desc string
	run  func(m *Model) tea.Cmd
}

// paletteModal is the fuzzy-searchable global command palette (: or Ctrl+P).
type paletteModal struct {
	input    textinput.Model
	all      []paletteItem
	filtered []paletteItem
	cursor   int
}

func newPaletteModal(items []paletteItem) (*paletteModal, tea.Cmd) {
	in := textinput.New()
	in.Prompt = "› "
	in.Placeholder = "type a view or command…"
	cmd := in.Focus()
	p := &paletteModal{
		input:    in,
		all:      items,
		filtered: items,
	}
	return p, cmd
}

func fuzzyMatch(target, query string) bool {
	if query == "" {
		return true
	}
	qi := 0
	qr := []rune(query)
	for _, r := range target {
		if r == qr[qi] {
			qi++
			if qi == len(qr) {
				return true
			}
		}
	}
	return false
}

func (p *paletteModal) filterItems() {
	q := strings.ToLower(strings.TrimSpace(p.input.Value()))
	if q == "" {
		p.filtered = p.all
		p.cursor = selectable(p.cursor, len(p.filtered))
		return
	}
	exact := []paletteItem{}
	fuzzy := []paletteItem{}
	for _, item := range p.all {
		hay := strings.ToLower(item.id + " " + item.desc)
		if strings.Contains(hay, q) {
			exact = append(exact, item)
		} else if fuzzyMatch(hay, q) {
			fuzzy = append(fuzzy, item)
		}
	}
	p.filtered = append(exact, fuzzy...)
	p.cursor = selectable(p.cursor, len(p.filtered))
}

func (p *paletteModal) handleKey(m *Model, key string) tea.Cmd {
	switch key {
	case "esc":
		m.modal = nil
		return nil
	case "enter":
		if len(p.filtered) == 0 {
			return nil
		}
		item := p.filtered[selectable(p.cursor, len(p.filtered))]
		m.modal = nil
		return item.run(m)
	case "up", "ctrl+p":
		p.cursor = selectable(p.cursor-1, len(p.filtered))
		return nil
	case "down", "ctrl+n":
		p.cursor = selectable(p.cursor+1, len(p.filtered))
		return nil
	case "pgup", "pgdown", "home", "end":
		if delta, ok := moveKey(key, 8); ok {
			p.cursor = selectable(p.cursor+delta, len(p.filtered))
		}
		return nil
	case "j":
		if p.input.Value() == "" {
			p.cursor = selectable(p.cursor+1, len(p.filtered))
			return nil
		}
	case "k":
		if p.input.Value() == "" {
			p.cursor = selectable(p.cursor-1, len(p.filtered))
			return nil
		}
	case "backspace":
		val := []rune(p.input.Value())
		if len(val) > 0 {
			p.input.SetValue(string(val[:len(val)-1]))
			p.filterItems()
		}
		return nil
	}
	if len([]rune(key)) == 1 {
		p.input.SetValue(p.input.Value() + key)
		p.filterItems()
	}
	return nil
}

func (p *paletteModal) update(m *Model, msg tea.Msg) tea.Cmd {
	if k, ok := msg.(tea.KeyPressMsg); ok {
		s := k.String()
		switch s {
		case "esc", "enter", "up", "ctrl+p", "down", "ctrl+n", "pgup", "pgdown", "home", "end":
			return p.handleKey(m, s)
		case "j", "k":
			if p.input.Value() == "" {
				return p.handleKey(m, s)
			}
		}
		var cmd tea.Cmd
		p.input, cmd = p.input.Update(msg)
		p.filterItems()
		return cmd
	}
	var cmd tea.Cmd
	p.input, cmd = p.input.Update(msg)
	p.filterItems()
	return cmd
}

func (p *paletteModal) view(width int) ([]string, int) {
	w := modalWidth(width)
	p.input.SetWidth(max(w-6, 10))
	lines := []string{
		styleBold.Render("Command Palette"),
		p.input.View(),
		"",
	}
	if len(p.filtered) == 0 {
		lines = append(lines, "  "+styleFaint.Render("no matching commands"))
		return lines, len(lines) - 1
	}
	focus := 3
	for i, item := range p.filtered {
		row := fmt.Sprintf("%-26s %s", item.id, styleFaint.Render(item.desc))
		if i == p.cursor {
			row = styleSelected.Render(fmt.Sprintf("› %-26s %s", item.id, item.desc))
			focus = len(lines)
		} else {
			row = "  " + row
		}
		lines = append(lines, row)
	}
	return lines, focus
}

func (p *paletteModal) keys() [][2]string {
	return [][2]string{{"type", "filter"}, {"↑↓", "select"}, {"enter", "launch"}, {"esc", "close"}}
}

// startPalette opens the global command palette modal (: or Ctrl+P).
func (m *Model) startPalette() tea.Cmd {
	items := []paletteItem{
		{id: "1:Molecules", desc: "Switch to Molecules view", run: func(m *Model) tea.Cmd { return m.switchView(viewMolecules) }},
		{id: "2:Graph", desc: "Switch to Backlog Graph view", run: func(m *Model) tea.Cmd { return m.switchView(viewGraph) }},
		{id: "3:Attention", desc: "Switch to Attention Queue view", run: func(m *Model) tea.Cmd { return m.switchView(viewAttention) }},
		{id: "4:Stream", desc: "Switch to Live Agent Stream view", run: func(m *Model) tea.Cmd { return m.switchView(viewStream) }},
		{id: "5:Pools", desc: "Switch to Pools & Quotas view", run: func(m *Model) tea.Cmd { return m.switchView(viewPools) }},
		{id: "6:Symphony", desc: "Switch to Symphony Tracker view", run: func(m *Model) tea.Cmd { return m.switchView(viewSymphony) }},
		{id: "7:Security", desc: "Switch to Security & Governance view", run: func(m *Model) tea.Cmd { return m.switchView(viewSecurity) }},
		{id: "notify", desc: "Send next-turn notification to task", run: func(m *Model) tea.Cmd { return m.startNotify(m.currentTask(), false) }},
		{id: "notify-preempt", desc: "Preempt in-flight turn with notification", run: func(m *Model) tea.Cmd { return m.startNotify(m.currentTask(), true) }},
		{id: "follow-up", desc: "Launch a FollowUp task from predecessor", run: func(m *Model) tea.Cmd { return m.startFollowUp(m.currentTask()) }},
		{id: "create-task", desc: "Create a new task in the backlog", run: func(m *Model) tea.Cmd { return m.startCreate() }},
		{id: "edit-task", desc: "Edit the selected task", run: func(m *Model) tea.Cmd { return m.startUpdate(m.currentTask()) }},
		{id: "delete-task", desc: "Delete / retire the selected task", run: func(m *Model) tea.Cmd { return m.startDelete(m.currentTask()) }},
		{id: "pool-resize", desc: "Resize a provider concurrency pool", run: func(m *Model) tea.Cmd { return m.startPoolResize(m.defaultPoolID()) }},
		{id: "quota-set", desc: "Set quota exhaustion window on pool/provider", run: func(m *Model) tea.Cmd {
			prov, mod, pool := m.defaultQuotaTarget()
			return m.startQuotaSet(prov, mod, pool)
		}},
		{id: "quota-clear", desc: "Clear quota exhaustion on pool/provider", run: func(m *Model) tea.Cmd {
			prov, mod, pool := m.defaultQuotaTarget()
			return m.startQuotaClear(prov, mod, pool)
		}},
		{id: "pause-dispatch", desc: "Pause dispatch for scope (instance/workflow/task_set/pool)", run: func(m *Model) tea.Cmd { return m.startPause("instance", "") }},
		{id: "unpause-dispatch", desc: "Unpause dispatch for scope", run: func(m *Model) tea.Cmd { return m.startUnpause("instance", "") }},
		{id: "approve-widen", desc: "Approve pending scope-widen request on task", run: func(m *Model) tea.Cmd { return m.startApproveWiden(m.currentTask()) }},
		{id: "decide", desc: "Record operator decision for task", run: func(m *Model) tea.Cmd { return m.startDecide(m.currentTask()) }},
		{id: "resolve-uncertain", desc: "Resolve uncertain dispatch (retry/accept/escalate)", run: func(m *Model) tea.Cmd { return m.startResolveUncertain(m.currentTask()) }},
		{id: "unescalate", desc: "Un-escalate and restart an escalated task", run: func(m *Model) tea.Cmd {
			return m.startLifecycle(m.currentTask(), "RestartWithInformation", "operator un-escalate and restart")
		}},
		{id: "resume-with-context", desc: "Resume escalated/parked task with operator context", run: func(m *Model) tea.Cmd {
			return m.startLifecycle(m.currentTask(), "ResumeWithContext", "operator resume with context")
		}},
		{id: "restart-with-information", desc: "Restart task with new operator information", run: func(m *Model) tea.Cmd {
			return m.startLifecycle(m.currentTask(), "RestartWithInformation", "operator restart with information")
		}},
		{id: "quarantine-release", desc: "Release quarantined task back to drive", run: func(m *Model) tea.Cmd {
			return m.startLifecycle(m.currentTask(), "QuarantineRelease", "operator release quarantine")
		}},
		{id: "retire-task", desc: "Retire task via lifecycle operation", run: func(m *Model) tea.Cmd {
			return m.startLifecycle(m.currentTask(), "Retire", "operator retire task")
		}},
		{id: "land-task", desc: "Land completed task onto target branch", run: func(m *Model) tea.Cmd {
			return m.startLifecycle(m.currentTask(), "Land", "operator land task")
		}},
		{id: "sync-symphony", desc: "Trigger immediate Symphony / Linear tracker refresh", run: func(m *Model) tea.Cmd { return m.startSyncSymphony() }},
	}
	pm, cmd := newPaletteModal(items)
	m.modal = pm
	return cmd
}

// startActionMenu opens a contextual action menu modal (a) for taskID.
func (m *Model) startActionMenu(taskID string) tea.Cmd {
	if taskID == "" {
		taskID = m.currentTask()
	}
	if taskID == "" {
		m.say("select a task to open its action menu", true)
		return nil
	}
	options := []string{
		"Un-escalate & Restart task",
		"Notify (next-turn)",
		"Notify (in-flight preempt)",
		"FollowUp",
		"Edit task",
		"Approve scope widen",
		"Decide",
		"Resolve uncertain",
		"Resume with context",
		"Restart with information",
		"Land task",
		"Retire / Delete task",
	}
	m.modal = &choice{
		title:   "Actions for task " + taskID,
		options: options,
		pick: func(m *Model, i int) tea.Cmd {
			m.modal = nil
			switch i {
			case 0:
				return m.startLifecycle(taskID, "RestartWithInformation", "operator un-escalate and restart")
			case 1:
				return m.startNotify(taskID, false)
			case 2:
				return m.startNotify(taskID, true)
			case 3:
				return m.startFollowUp(taskID)
			case 4:
				return m.startUpdate(taskID)
			case 5:
				return m.startApproveWiden(taskID)
			case 6:
				return m.startDecide(taskID)
			case 7:
				return m.startResolveUncertain(taskID)
			case 8:
				return m.startLifecycle(taskID, "ResumeWithContext", "operator resume with context")
			case 9:
				return m.startLifecycle(taskID, "RestartWithInformation", "operator restart with information")
			case 10:
				return m.startLifecycle(taskID, "Land", "operator land task")
			case 11:
				if _, onBoard := m.snap.tasks[taskID]; onBoard || (m.detail != nil && m.detail.id == taskID && m.detail.data != nil) {
					return m.startDelete(taskID)
				}
				return m.startLifecycle(taskID, "Retire", "operator retire task")
			}
			return nil
		},
	}
	return nil
}
