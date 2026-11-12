package tui

import (
	"fmt"
	"sort"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/xleelz/fae/tui/client"
)

type streamRow struct {
	taskID  string
	section string
	state   string
	stage   string
	role    string
	reason  string
	pane    *PaneSessionView
}

func (m *Model) streamPanes() []PaneSessionView {
	if len(m.snap.panes) > 0 {
		return m.snap.panes
	}
	return m.snap.status.Panes
}

func (m *Model) streamTasks() []client.Task {
	if len(m.snap.tasks) > 0 {
		return m.snap.sortedTasks()
	}
	if len(m.snap.board.Tasks) > 0 {
		out := append([]client.Task(nil), m.snap.board.Tasks...)
		sort.Slice(out, func(i, j int) bool {
			if out[i].Priority != out[j].Priority {
				return out[i].Priority > out[j].Priority
			}
			return out[i].ID < out[j].ID
		})
		return out
	}
	return nil
}

func (m *Model) cachedDetail(id string) (client.TaskDetail, bool) {
	if id == "" {
		return client.TaskDetail{}, false
	}
	if m.snap.details != nil {
		if d, ok := m.snap.details[id]; ok {
			return d, true
		}
	}
	if m.detail != nil && m.detail.id == id && m.detail.data != nil {
		return *m.detail.data, true
	}
	return client.TaskDetail{}, false
}

func (m *Model) streamRows() []streamRow {
	panes := m.streamPanes()
	paneByTask := map[string]PaneSessionView{}
	for _, p := range panes {
		if p.TaskID != "" {
			paneByTask[p.TaskID] = p
		}
	}

	var active, parked, done []streamRow
	seen := map[string]bool{}

	matchesFilter := func(r streamRow, d client.TaskDetail, hasDetail bool) bool {
		q := strings.ToLower(strings.TrimSpace(m.filter))
		if q == "" {
			return true
		}
		hay := strings.ToLower(strings.Join([]string{
			r.taskID, r.section, r.state, r.stage, r.role, r.reason,
			d.Title, d.FollowUpOf,
		}, " "))
		if r.pane != nil {
			hay += " " + strings.ToLower(r.pane.ID+" "+r.pane.Provider+" "+r.pane.Model)
		}
		if hasDetail {
			for k, ck := range d.TurnCheckpoints {
				hay += " " + strings.ToLower(k+" "+ck.ProviderSession)
			}
		}
		return strings.Contains(hay, q)
	}

	for _, t := range m.streamTasks() {
		d, hasD := m.cachedDetail(t.ID)
		stage := t.State
		if hasD && d.Stage != "" {
			stage = d.Stage
		}
		var panePtr *PaneSessionView
		role := ""
		if p, ok := paneByTask[t.ID]; ok {
			pCopy := p
			panePtr = &pCopy
			role = p.Role
			if p.Stage != "" {
				stage = p.Stage
			}
		}
		reason := t.Reason
		if reason == "" && hasD {
			reason = d.Reason
		}

		isParkedQuotaOrBudget := t.State == "Parked" ||
			strings.Contains(reason, "ProviderQuota") ||
			strings.Contains(reason, "TurnBudgetExhausted") ||
			(hasD && d.Parked)

		switch {
		case t.InFlight || panePtr != nil || (!kernel(t.State) && !isParkedQuotaOrBudget):
			row := streamRow{
				taskID:  t.ID,
				section: "Active Agent Sessions",
				state:   t.State,
				stage:   stage,
				role:    role,
				reason:  reason,
				pane:    panePtr,
			}
			if matchesFilter(row, d, hasD) {
				active = append(active, row)
			}
			seen[t.ID] = true
		case isParkedQuotaOrBudget:
			row := streamRow{
				taskID:  t.ID,
				section: "Parked (Quota / Turn Budget)",
				state:   t.State,
				stage:   stage,
				role:    role,
				reason:  reason,
				pane:    panePtr,
			}
			if matchesFilter(row, d, hasD) {
				parked = append(parked, row)
			}
			seen[t.ID] = true
		case t.State == "Done":
			row := streamRow{
				taskID:  t.ID,
				section: "Follow-Up Ready (Done)",
				state:   t.State,
				stage:   stage,
				role:    role,
				reason:  reason,
				pane:    panePtr,
			}
			if matchesFilter(row, d, hasD) {
				done = append(done, row)
			}
			seen[t.ID] = true
		}
	}

	// Include any PaneSessionView whose task was not on the board.
	for _, p := range panes {
		id := p.TaskID
		if id == "" {
			id = p.ID
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		pCopy := p
		d, hasD := m.cachedDetail(id)
		row := streamRow{
			taskID:  id,
			section: "Active Agent Sessions",
			state:   firstNonEmpty(p.Stage, "Running"),
			stage:   p.Stage,
			role:    p.Role,
			pane:    &pCopy,
		}
		if matchesFilter(row, d, hasD) {
			active = append(active, row)
		}
	}

	// Include any cached details with TurnBudget/TurnCheckpoints/Notifications not yet listed.
	if len(m.snap.details) > 0 {
		ids := make([]string, 0, len(m.snap.details))
		for id := range m.snap.details {
			if !seen[id] {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		for _, id := range ids {
			d := m.snap.details[id]
			if d.TurnBudget == nil && len(d.TurnCheckpoints) == 0 && len(d.Notifications) == 0 && d.FollowUpOf == "" && !d.InFlight && !d.Parked && d.State != "Done" {
				continue
			}
			seen[id] = true
			sec := "Active Agent Sessions"
			if d.Parked || d.State == "Parked" {
				sec = "Parked (Quota / Turn Budget)"
			} else if d.State == "Done" {
				sec = "Follow-Up Ready (Done)"
			}
			row := streamRow{
				taskID:  id,
				section: sec,
				state:   firstNonEmpty(d.State, "Running"),
				stage:   firstNonEmpty(d.Stage, d.State),
				reason:  d.Reason,
			}
			if matchesFilter(row, d, true) {
				switch sec {
				case "Parked (Quota / Turn Budget)":
					parked = append(parked, row)
				case "Follow-Up Ready (Done)":
					done = append(done, row)
				default:
					active = append(active, row)
				}
			}
		}
	}

	out := make([]streamRow, 0, len(active)+len(parked)+len(done))
	out = append(out, active...)
	out = append(out, parked...)
	out = append(out, done...)
	return out
}

func (m *Model) streamMove(delta int) {
	rows := m.streamRows()
	m.stream.cursor = selectable(m.stream.cursor+delta, len(rows))
}

func (m *Model) streamSelectedTaskID() string {
	rows := m.streamRows()
	if len(rows) == 0 {
		return ""
	}
	idx := selectable(m.stream.cursor, len(rows))
	return rows[idx].taskID
}

func (m *Model) streamView(height int) []string {
	w := m.width
	if w <= 0 {
		w = 100
	}
	if height <= 0 {
		return nil
	}
	leftW, rightW := splitWidths(w)
	rows := m.streamRows()
	m.stream.cursor = selectable(m.stream.cursor, len(rows))

	leftLines := m.streamLeftPane(rows, leftW, height)
	var sel *streamRow
	if len(rows) > 0 {
		sel = &rows[m.stream.cursor]
	}
	rightLines := m.streamRightPane(sel, rightW, height)
	return renderSplitPane(w, height, leftLines, rightLines)
}

func (m *Model) streamLeftPane(rows []streamRow, w, h int) []string {
	title := "Active Agent Sessions & Follow-Up Ready"
	if m.filter != "" {
		title += fmt.Sprintf(" [filter: %s]", m.filter)
	}
	head := []string{styleHeading.Render(fmt.Sprintf("%s (%d)", title, len(rows)))}
	if len(rows) == 0 {
		msg := "No active agent sessions, parked quota/budget tasks, or follow-up ready tasks."
		if m.filter != "" {
			msg = fmt.Sprintf("No sessions matching filter %q.", m.filter)
		}
		return append(head, styleFaint.Render(msg))
	}

	lines := []string{}
	cursorLine := 0
	section := ""
	for i, r := range rows {
		if r.section != section {
			section = r.section
			if len(lines) > 0 {
				lines = append(lines, "")
			}
			lines = append(lines, styleBold.Render("── "+section+" ──"))
		}
		if i == m.stream.cursor {
			cursorLine = len(lines)
		}

		st := m.snap.stateOf(r.taskID)
		if st.Class == classUnknown && r.state != "" {
			st = implState{Short: r.state, Long: r.state, Class: classOf(r.state)}
		}
		badge := st.render(st.glyph() + " " + firstNonEmpty(r.stage, r.state))
		meta := []string{}
		if r.role != "" {
			meta = append(meta, "role:"+r.role)
		}
		if r.pane != nil && r.pane.Provider != "" {
			prov := r.pane.Provider
			if r.pane.Model != "" {
				prov += "/" + r.pane.Model
			}
			meta = append(meta, prov)
		}
		if r.reason != "" {
			meta = append(meta, styleWarn.Render(r.reason))
		}

		d, hasD := m.cachedDetail(r.taskID)
		if hasD {
			if g := formatTurnBudgetGauge(d); g != "" {
				meta = append(meta, g)
			}
			if ck := latestCheckpointSession(d); ck != "" {
				meta = append(meta, styleOK.Render("ckpt: "+ck))
			}
			if d.FollowUpOf != "" {
				meta = append(meta, styleFaint.Render("↪ "+d.FollowUpOf))
			}
		}

		line := fmt.Sprintf("  %s  %s", styleBold.Render(r.taskID), badge)
		if len(meta) > 0 {
			line += "  " + strings.Join(meta, " · ")
		}
		if i == m.stream.cursor {
			line = styleSelected.Render(fit(ansiStrip(line), w))
		} else {
			line = clip(line, w)
		}
		lines = append(lines, line)
	}

	room := max(h-len(head), 1)
	m.stream.scroll = scrollTo(m.stream.scroll, cursorLine, room, len(lines))
	return append(head, window(lines, m.stream.scroll, room)...)
}

func (m *Model) streamRightPane(sel *streamRow, w, h int) []string {
	lines := []string{styleHeading.Render("Live Session Telemetry & Steering")}
	hints := fmt.Sprintf("%s:notify (next-turn)  %s:preempt (in-flight)  %s:follow-up  %s:detail",
		styleKey.Render("n"), styleKey.Render("N"), styleKey.Render("f"), styleKey.Render("enter"))

	if sel == nil {
		lines = append(lines, styleFaint.Render("Select a task or session on the left to inspect telemetry and steer."))
		lines = append(lines, "", hints)
		return clipLines(lines, w, h)
	}

	now := m.now()
	d, hasD := m.cachedDetail(sel.taskID)
	state := sel.state
	stage := sel.stage
	role := sel.role
	if hasD {
		state = firstNonEmpty(d.State, state)
		stage = firstNonEmpty(d.Stage, stage)
	}

	lines = append(lines,
		fmt.Sprintf("  %-14s %s", "Task ID", styleBold.Render(sel.taskID)),
		fmt.Sprintf("  %-14s %s", "State", firstNonEmpty(state, "—")),
		fmt.Sprintf("  %-14s %s", "Stage", firstNonEmpty(stage, "—")),
		fmt.Sprintf("  %-14s %s", "Role", firstNonEmpty(role, "—")),
	)
	if sel.reason != "" {
		lines = append(lines, fmt.Sprintf("  %-14s %s", "Reason", styleWarn.Render(sel.reason)))
	}
	if sel.pane != nil {
		p := sel.pane
		paneDesc := p.ID
		if p.Provider != "" || p.Model != "" {
			paneDesc += fmt.Sprintf(" (%s/%s)", p.Provider, p.Model)
		}
		if p.PID > 0 {
			paneDesc += fmt.Sprintf(" pid %d", p.PID)
		}
		if !p.StartedAt.IsZero() {
			paneDesc += " up " + age(now.Sub(p.StartedAt))
		}
		lines = append(lines, fmt.Sprintf("  %-14s %s", "Pane", paneDesc))
	}

	if hasD && d.FollowUpOf != "" {
		lines = append(lines, fmt.Sprintf("  %-14s %s", "FollowUpOf", styleOK.Render(d.FollowUpOf)))
	} else {
		lines = append(lines, fmt.Sprintf("  %-14s %s", "FollowUpOf", styleFaint.Render("(root task)")))
	}

	// TurnBudget breakdown
	lines = append(lines, "", styleHeading.Render("TurnBudget"))
	if hasD && d.TurnBudget != nil {
		tb := d.TurnBudget
		maxTurns := tb.MaxAssistantTurns
		if maxTurns == 0 {
			maxTurns = tb.MaxTurns
		}
		exhaust := firstNonEmpty(tb.OnExhaustion, "park")
		if g := formatTurnBudgetGauge(d); g != "" {
			lines = append(lines, "  Gauge          "+g)
		}
		lines = append(lines,
			fmt.Sprintf("  WallSeconds    %ds", tb.WallSeconds),
			fmt.Sprintf("  MaxAsstTurns   %d", maxTurns),
			fmt.Sprintf("  OnExhaustion   %s", exhaust),
		)
	} else {
		lines = append(lines, "  "+styleFaint.Render("no custom turn budget reported"))
	}

	// TurnCheckpoints
	lines = append(lines, "", styleHeading.Render(fmt.Sprintf("TurnCheckpoints (%d)", len(d.TurnCheckpoints))))
	if hasD && len(d.TurnCheckpoints) > 0 {
		keys := make([]string, 0, len(d.TurnCheckpoints))
		for k := range d.TurnCheckpoints {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			ck := d.TurnCheckpoints[k]
			upd := "—"
			if !ck.UpdatedAt.IsZero() {
				upd = stamp(now, ck.UpdatedAt) + " (" + ago(now, ck.UpdatedAt) + ")"
			}
			lines = append(lines,
				fmt.Sprintf("  [%s] session=%s", styleBold.Render(k), firstNonEmpty(ck.ProviderSession, "—")),
				fmt.Sprintf("    tree=%s · turns=%d · wall=%ds · updated=%s",
					firstNonEmpty(shortID(ck.WorktreeTree), "—"), ck.AssistantTurns, ck.WallSeconds, upd),
			)
		}
	} else {
		lines = append(lines, "  "+styleFaint.Render("no turn checkpoints recorded"))
	}

	// Notifications
	lines = append(lines, "", styleHeading.Render(fmt.Sprintf("Notifications (%d)", len(d.Notifications))))
	if hasD && len(d.Notifications) > 0 {
		for _, n := range d.Notifications {
			deliv := n.Delivery
			if deliv == "in-flight" {
				deliv = styleWarn.Render("in-flight")
			} else {
				deliv = styleOK.Render(firstNonEmpty(deliv, "next-turn"))
			}
			delivAt := "pending"
			if !n.DeliveredAt.IsZero() {
				delivAt = "delivered " + stamp(now, n.DeliveredAt)
			}
			lines = append(lines,
				fmt.Sprintf("  • [%s / %s] %s (%s)", styleBold.Render(n.Kind), deliv, n.Summary, styleFaint.Render(delivAt)),
			)
		}
	} else {
		lines = append(lines, "  "+styleFaint.Render("no steering notifications"))
	}

	lines = append(lines, "", hints)
	return clipLines(lines, w, h)
}

func formatTurnBudgetGauge(d client.TaskDetail) string {
	var usedTurns, usedWall int
	for _, ck := range d.TurnCheckpoints {
		if ck.AssistantTurns > usedTurns {
			usedTurns = ck.AssistantTurns
		}
		if ck.WallSeconds > usedWall {
			usedWall = ck.WallSeconds
		}
	}
	if d.TurnBudget == nil {
		if len(d.TurnCheckpoints) == 0 {
			return ""
		}
		return fmt.Sprintf("%dt · %ds", usedTurns, usedWall)
	}
	maxTurns := d.TurnBudget.MaxAssistantTurns
	if maxTurns == 0 {
		maxTurns = d.TurnBudget.MaxTurns
	}
	maxWall := d.TurnBudget.WallSeconds
	bar := asciiGauge(usedTurns, maxTurns, 6)
	if maxTurns == 0 && maxWall > 0 {
		bar = asciiGauge(usedWall, maxWall, 6)
	}
	return fmt.Sprintf("%s %d/%dt · %ds/%ds", bar, usedTurns, maxTurns, usedWall, maxWall)
}

func latestCheckpointSession(d client.TaskDetail) string {
	if len(d.TurnCheckpoints) == 0 {
		return ""
	}
	if d.Stage != "" {
		if ck, ok := d.TurnCheckpoints[d.Stage]; ok && ck.ProviderSession != "" {
			return ck.ProviderSession
		}
	}
	keys := make([]string, 0, len(d.TurnCheckpoints))
	for k := range d.TurnCheckpoints {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var best string
	var bestAt int64 = -1
	for _, k := range keys {
		ck := d.TurnCheckpoints[k]
		if ck.ProviderSession == "" {
			continue
		}
		ts := ck.UpdatedAt.UnixNano()
		if ts > bestAt {
			bestAt = ts
			best = ck.ProviderSession
		}
	}
	return best
}

func asciiGauge(used, total, width int) string {
	if width <= 0 {
		width = 6
	}
	if total <= 0 {
		return "[" + strings.Repeat("░", width) + "]"
	}
	filled := (used*width + total/2) / total
	if used > 0 && filled == 0 {
		filled = 1
	}
	filled = min(max(filled, 0), width)
	return "[" + strings.Repeat("█", filled) + strings.Repeat("░", width-filled) + "]"
}

func splitWidths(w int) (leftW, rightW int) {
	if w >= 96 {
		leftW = max((w-3)*45/100, 24)
		rightW = max(w-leftW-3, 24)
		return leftW, rightW
	}
	return w, w
}

func renderSplitPane(w, h int, left, right []string) []string {
	if h <= 0 {
		return nil
	}
	if w < 96 {
		topH := max(h/2, 1)
		botH := max(h-topH-1, 1)
		out := make([]string, 0, h)
		for i := range topH {
			line := ""
			if i < len(left) {
				line = left[i]
			}
			out = append(out, fit(line, w))
		}
		if len(out) < h {
			out = append(out, styleFaint.Render(strings.Repeat("─", max(w, 1))))
		}
		for i := range botH {
			if len(out) >= h {
				break
			}
			line := ""
			if i < len(right) {
				line = right[i]
			}
			out = append(out, fit(line, w))
		}
		return out
	}

	leftW, rightW := splitWidths(w)
	lCol := make([]string, h)
	rCol := make([]string, h)
	sep := make([]string, h)
	div := styleFaint.Render(" │ ")
	for i := range h {
		if i < len(left) {
			lCol[i] = fit(left[i], leftW)
		} else {
			lCol[i] = strings.Repeat(" ", leftW)
		}
		if i < len(right) {
			rCol[i] = fit(right[i], rightW)
		} else {
			rCol[i] = strings.Repeat(" ", rightW)
		}
		sep[i] = div
	}
	joined := lipgloss.JoinHorizontal(
		lipgloss.Top,
		strings.Join(lCol, "\n"),
		strings.Join(sep, "\n"),
		strings.Join(rCol, "\n"),
	)
	return strings.Split(joined, "\n")
}

func clipLines(lines []string, w, h int) []string {
	if h <= 0 {
		return nil
	}
	if len(lines) > h {
		hint := lines[len(lines)-1]
		lines = append(append([]string(nil), lines[:h-1]...), hint)
	}
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		out = append(out, clip(l, w))
	}
	return out
}
