package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/xleelz/fae/tui/client"
)

// attentionState is the Attention view's selection and scroll.
type attentionState struct {
	row    int
	offset int
}

// attentionRow is one item that needs the operator: a task, or an alert.
type attentionRow struct {
	section string
	task    string
	alert   *client.Alert
	text    string
}

// attentionRows lists what needs the operator, most urgent first: escalated
// tasks (kind, reason, since when), quarantined tasks, uncertain dispatches,
// pending decisions, blocked tasks, Prophet questions, unacknowledged alerts.
func (m *Model) attentionRows() []attentionRow {
	rows := []attentionRow{}
	now := m.now()
	escalated := []client.Task{}
	for _, t := range m.snap.tasks {
		if t.State == "Escalated" {
			escalated = append(escalated, t)
		}
	}
	reasons := map[string]string{}
	for _, item := range m.snap.inbox.Escalated {
		reasons[item.Task] = item.Reason
		if _, ok := m.snap.tasks[item.Task]; !ok {
			escalated = append(escalated, client.Task{ID: item.Task, State: "Escalated", Reason: item.Reason, WorkflowID: item.Workflow, WorkflowRevision: item.Revision})
		}
	}
	sort.Slice(escalated, func(i, j int) bool {
		a, b := m.since[escalated[i].ID], m.since[escalated[j].ID]
		if !a.Equal(b) {
			return a.Before(b)
		}
		return escalated[i].ID < escalated[j].ID
	})
	for _, t := range escalated {
		kind := t.EscalationKind
		if kind == "" {
			kind = "Escalated"
		}
		reason := strings.TrimSpace(strings.TrimPrefix(firstNonEmpty(t.Reason, reasons[t.ID]), kind+":"))
		since := "since: reading…"
		if at, ok := m.since[t.ID]; ok {
			since = "since " + ago(now, at)
			if at.IsZero() {
				since = "since: unknown"
			}
		}
		text := fmt.Sprintf("%s  %s  %s", t.ID, styleError.Render(kind), styleFaint.Render(since))
		// The Board's stored flag marks a pending delete; its TaskGet
		// object, read with the escalation time, says where it stands.
		uncertain := m.isUncertainTask(t.ID)
		switch p := m.pendingDel[t.ID]; {
		case p != nil:
			text += "  " + styleWarn.Render("delete pending: "+pendingDeleteText(t.ID, p, uncertain))
		case t.PendingDelete && uncertain:
			text += "  " + styleWarn.Render("delete pending: turn in flight is uncertain after restart; run fae resolve-uncertain --action escalate --id "+t.ID+" to finish retiring")
		case t.PendingDelete:
			text += "  " + styleWarn.Render("delete pending")
		case reason != "" && reason != kind:
			text += "  " + reason
		}
		rows = append(rows, attentionRow{section: "Escalated", task: t.ID, text: text})
	}
	if sum := m.snap.status.Snapshot; sum != nil {
		ids := make([]string, 0, len(sum.Quarantined))
		for id := range sum.Quarantined {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			rows = append(rows, attentionRow{section: "Quarantined (out of the drive until restart)", task: id, text: id + "  " + sum.Quarantined[id]})
		}
	}
	inboxTasks := func(section string, items []client.InboxTask) {
		for _, item := range items {
			text := item.Task
			if item.Stage != "" {
				text += "  at " + item.Stage
			}
			if item.BlockedVia != "" {
				text += "  remediation " + item.BlockedVia
			}
			if item.Reason != "" {
				text += "  " + item.Reason
			}
			if len(item.Decision) > 0 && string(item.Decision) != "null" {
				var d struct {
					Question string `json:"question"`
				}
				if json.Unmarshal(item.Decision, &d) == nil && d.Question != "" {
					text += "  — " + d.Question
				}
			}
			if section == "Uncertain dispatch (resolve-uncertain)" {
				if p := m.pendingDel[item.Task]; p != nil {
					text += "  " + styleWarn.Render("delete pending: "+pendingDeleteText(item.Task, p, true))
				} else if t, ok := m.snap.tasks[item.Task]; ok && t.PendingDelete {
					text += "  " + styleWarn.Render("delete pending: turn in flight is uncertain after restart; run fae resolve-uncertain --action escalate --id "+item.Task+" to finish retiring")
				}
			}
			rows = append(rows, attentionRow{section: section, task: item.Task, text: text})
		}
	}
	uncertainItems := append([]client.InboxTask(nil), m.snap.inbox.Uncertain...)
	seenUncertain := map[string]bool{}
	for _, item := range uncertainItems {
		seenUncertain[item.Task] = true
	}
	for _, t := range m.snap.sortedTasks() {
		if t.State == "Parked" && t.Reason == "UncertainDispatch" && !seenUncertain[t.ID] {
			uncertainItems = append(uncertainItems, client.InboxTask{Task: t.ID, State: t.State, Reason: t.Reason, Workflow: t.WorkflowID, Revision: t.WorkflowRevision})
		}
	}
	inboxTasks("Uncertain dispatch (resolve-uncertain)", uncertainItems)
	inboxTasks("Waiting for a decision", m.snap.inbox.Decisions)
	inboxTasks("Blocked", m.snap.inbox.Blocked)
	for _, q := range m.snap.inbox.Questions {
		text := q.Reason
		if q.Task != "" {
			text = q.Task + "  " + text
		}
		if len(q.Proposals) > 0 {
			text += ": " + strings.Join(q.Proposals, "; ")
		}
		rows = append(rows, attentionRow{section: "Prophet questions", task: q.Task, text: text})
	}
	alerts := append([]client.Alert(nil), m.snap.alerts...)
	sort.SliceStable(alerts, func(i, j int) bool { return alerts[i].ID > alerts[j].ID })
	for i := range alerts {
		a := alerts[i]
		text := fmt.Sprintf("#%d %s %s", a.ID, severity(a.Severity), a.Source)
		if a.Task != "" {
			text += "  task " + a.Task
		}
		text += styleFaint.Render("  " + ago(now, a.CreatedAt))
		if detail := alertDetail(a.Detail); detail != "" {
			text += "  " + detail
		}
		rows = append(rows, attentionRow{section: "Unacknowledged alerts", task: a.Task, alert: &alerts[i], text: text})
	}
	return rows
}

func (m *Model) isUncertainTask(id string) bool {
	if p := m.pendingDel[id]; p != nil && (p.Status == "uncertain" || p.UncertainDispatch) {
		return true
	}
	if t, ok := m.snap.tasks[id]; ok && (t.Reason == "UncertainDispatch" || t.EscalationKind == "UncertainDispatch") {
		return true
	}
	for _, item := range m.snap.inbox.Escalated {
		if item.Task == id && item.Reason == "UncertainDispatch" {
			return true
		}
	}
	for _, item := range m.snap.inbox.Uncertain {
		if item.Task == id {
			return true
		}
	}
	if m.detail != nil && m.detail.id == id && m.detail.data != nil {
		d := m.detail.data
		if d.Reason == "UncertainDispatch" || d.EscalationReason == "UncertainDispatch" || d.EscalationKind == "UncertainDispatch" {
			return true
		}
	}
	return false
}

func (m *Model) isEscalatedTask(id string) bool {
	if id == "" {
		return false
	}
	if t, ok := m.snap.tasks[id]; ok && t.State == "Escalated" {
		return true
	}
	for _, item := range m.snap.inbox.Escalated {
		if item.Task == id {
			return true
		}
	}
	if m.detail != nil && m.detail.id == id && m.detail.data != nil && m.detail.data.State == "Escalated" {
		return true
	}
	return false
}

func severity(s string) string {
	switch s {
	case "error", "critical":
		return styleError.Render(s)
	case "warning":
		return styleWarn.Render(s)
	}
	return s
}

// alertDetail is a one-line summary of an alert's detail document.
func alertDetail(raw json.RawMessage) string {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return strings.TrimSpace(string(raw))
	}
	for _, k := range []string{"message", "reason", "detail", "kind"} {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := []string{}
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, m[k]))
	}
	return strings.Join(parts, " ")
}

func (m *Model) selectedAttention() attentionRow {
	rows := m.attentionRows()
	if len(rows) == 0 {
		return attentionRow{}
	}
	return rows[selectable(m.att.row, len(rows))]
}

func (m *Model) selectedAttentionTask() string {
	return m.selectedAttention().task
}

// attentionAct handles in-place triage actions when m.opts.Write is enabled
// on the currently selected attention item.
func (m *Model) attentionAct(key string) (bool, tea.Cmd) {
	if !m.opts.Write {
		return false, nil
	}
	item := m.selectedAttention()
	if item.task == "" {
		return false, nil
	}
	switch key {
	case "d":
		return true, m.startDecide(item.task)
	case "u":
		if !m.isUncertainTask(item.task) && m.isEscalatedTask(item.task) {
			return true, m.startLifecycle(item.task, "RestartWithInformation", "operator un-escalate and restart")
		}
		return true, m.startResolveUncertain(item.task)
	case "R":
		if m.isEscalatedTask(item.task) {
			return true, m.startLifecycle(item.task, "RestartWithInformation", "operator resume escalated task")
		}
		return true, m.startLifecycle(item.task, "ResumeWithContext", "operator resume with context")
	case "I":
		return true, m.startLifecycle(item.task, "RestartWithInformation", "operator restart with information")
	case "w":
		return true, m.startApproveWiden(item.task)
	case "G":
		return true, m.startLifecycle(item.task, "QuarantineRelease", "operator release quarantine")
	case "X":
		return true, m.startLifecycle(item.task, "Retire", "operator retire from attention")
	}
	return false, nil
}

func (m *Model) attentionKey(s string) tea.Cmd {
	rows := m.attentionRows()
	if len(rows) == 0 {
		return nil
	}
	m.att.row = selectable(m.att.row, len(rows))
	row := rows[m.att.row]
	if handled, cmd := m.attentionAct(s); handled {
		return cmd
	}
	switch s {
	case "enter":
		return m.openDetail(row.task)
	case "A":
		if row.alert == nil {
			m.say("select an alert to acknowledge", true)
			return nil
		}
		if !m.writable("acknowledge alerts") {
			return nil
		}
		id := row.alert.ID
		return m.runWrite(writeAction{
			verb:    "AlertAcknowledge",
			summary: fmt.Sprintf("fae-tui: acknowledge alert %d (%s).", id, row.alert.Source),
			class:   "runtime",
			run: func(ctx context.Context, d Daemon, message string) ([]string, error) {
				return []string{fmt.Sprintf("alert %d acknowledged", id)}, d.AlertAcknowledge(ctx, id, message)
			},
		})
	}
	if delta, ok := moveKey(s, m.height/2); ok {
		m.att.row = selectable(m.att.row+delta, len(rows))
	}
	return nil
}

func (m *Model) attentionBody(w, h int) []string {
	rows := m.attentionRows()
	if len(rows) == 0 {
		if !m.snap.loaded[partBoard] && !m.snap.loaded[partInbox] {
			return []string{styleFaint.Render("waiting for the Board, Inbox and Alerts reads…")}
		}
		return []string{styleOK.Render("Nothing needs your attention."), styleFaint.Render("No escalated, quarantined, uncertain, blocked or decision-waiting task, no open question and no unacknowledged alert.")}
	}
	cursor := selectable(m.att.row, len(rows))
	lines := []string{}
	cursorLine := 0
	section := ""
	counts := map[string]int{}
	for _, r := range rows {
		counts[r.section]++
	}
	for i, r := range rows {
		if r.section != section {
			section = r.section
			if len(lines) > 0 {
				lines = append(lines, "")
			}
			lines = append(lines, styleHeading.Render(fmt.Sprintf("%s (%d)", section, counts[section])))
		}
		text := "  " + r.text
		if len(wrap(ansiStrip(r.text), max(w-2, 8))) == 1 {
			if i == cursor {
				cursorLine = len(lines)
				text = styleSelected.Render(clip(ansiStrip(text), w))
			}
			lines = append(lines, text)
			continue
		}
		if i == cursor {
			cursorLine = len(lines)
		}
		for j, wl := range wrap(r.text, max(w-4, 8)) {
			indent := "  "
			if j > 0 {
				indent = "    "
			}
			line := indent + wl
			if i == cursor {
				line = styleSelected.Render(clip(ansiStrip(line), w))
			}
			lines = append(lines, line)
		}
	}
	if m.opts.Write {
		lines = append(lines, "", styleFaint.Render("Actions (--write): d:decide  u:resolve-uncertain  R:resume-ctx  I:restart-info  w:approve-widen  G:release  X:retire"))
	}
	m.att.offset = scrollTo(m.att.offset, cursorLine, h, len(lines))
	out := window(lines, m.att.offset, h)
	for i := range out {
		out[i] = clip(out[i], w)
	}
	return out
}
