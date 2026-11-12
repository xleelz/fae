package tui

import (
	"fmt"
	"strings"
)

type symphonyRow struct {
	section string
	issue   LinearIssueView
}

func (m *Model) activeOutbox() TicketOutboxView {
	ob := m.snap.symphony.Outbox
	if ob.Pending == 0 && ob.OldestAgeSeconds == 0 && ob.LastError == "" && ob.CircuitOpenUntil.IsZero() && m.snap.status.TicketOutbox != nil {
		return *m.snap.status.TicketOutbox
	}
	return ob
}

func (m *Model) symphonyRows() []symphonyRow {
	sym := m.snap.symphony
	byKey := map[string]*LinearIssueView{}
	order := []string{}
	sections := map[string]string{}

	keyOf := func(iss LinearIssueView) string {
		return firstNonEmpty(iss.Identifier, iss.ID, iss.TaskID)
	}

	mergeIssue := func(sec string, iss LinearIssueView) {
		k := keyOf(iss)
		if k == "" {
			return
		}
		if existing, ok := byKey[k]; ok {
			if existing.TaskID == "" {
				existing.TaskID = iss.TaskID
			}
			if existing.Title == "" {
				existing.Title = iss.Title
			}
			if existing.State == "" {
				existing.State = iss.State
			}
			if existing.Stage == "" {
				existing.Stage = iss.Stage
			}
			if existing.TurnCount == 0 {
				existing.TurnCount = iss.TurnCount
			}
			if existing.TokensTotal == 0 {
				existing.TokensTotal = iss.TokensTotal
			}
			if existing.RetryAttempt == 0 {
				existing.RetryAttempt = iss.RetryAttempt
			}
			if existing.RetryDueAt.IsZero() {
				existing.RetryDueAt = iss.RetryDueAt
			}
			if existing.LastError == "" {
				existing.LastError = iss.LastError
			}
			if existing.URL == "" {
				existing.URL = iss.URL
			}
			if len(existing.BlockedBy) == 0 {
				existing.BlockedBy = iss.BlockedBy
			}
			if len(existing.SubIssues) == 0 {
				existing.SubIssues = iss.SubIssues
			}
			return
		}
		cp := iss
		byKey[k] = &cp
		sections[k] = sec
		order = append(order, k)
	}

	for _, iss := range sym.Running {
		mergeIssue("Running Issues", iss)
	}
	for _, iss := range sym.Retrying {
		mergeIssue("Retrying Issues", iss)
	}
	for _, iss := range sym.KnownIssues {
		mergeIssue("Known Tracker Issues", iss)
	}

	seenTask := map[string]bool{}
	seenRef := map[string]bool{}
	for _, k := range order {
		iss := byKey[k]
		if iss.TaskID != "" {
			seenTask[iss.TaskID] = true
		}
		if iss.Identifier != "" {
			seenRef[iss.Identifier] = true
		}
	}

	sourceRefByTask := map[string]string{}
	for _, item := range m.snap.backlog.Items {
		if item.SourceRef != "" {
			sourceRefByTask[item.ID] = item.SourceRef
		}
	}

	includeAllTasks := len(sym.KnownIssues) == 0 && len(sym.Running) == 0 && len(sym.Retrying) == 0
	for _, t := range m.streamTasks() {
		if seenTask[t.ID] {
			continue
		}
		d, hasD := m.cachedDetail(t.ID)
		ref := sourceRefByTask[t.ID]
		if ref == "" && hasD {
			ref = d.SourceRef
		}
		if ref == "" && !includeAllTasks {
			continue
		}
		if ref != "" && seenRef[ref] {
			continue
		}
		seenTask[t.ID] = true
		if ref != "" {
			seenRef[ref] = true
		}
		ident := firstNonEmpty(ref, t.ID)
		stage := t.State
		if hasD && d.Stage != "" {
			stage = d.Stage
		}
		title := ""
		if hasD {
			title = d.Title
		}
		var blockedBy []string
		for _, e := range m.snap.backlog.Edges {
			if e.To == t.ID && e.Kind == "blocking" {
				blockedBy = append(blockedBy, e.From)
			}
		}
		var subIssues []string
		if t.Child != nil {
			for _, ch := range t.Child.Children {
				if ch.Task != "" {
					subIssues = append(subIssues, ch.Task)
				}
			}
			if len(subIssues) == 0 && t.Child.Task != "" {
				subIssues = append(subIssues, t.Child.Task)
			}
		}
		mergeIssue("Linked Fae Tasks", LinearIssueView{
			ID:           t.ID,
			Identifier:   ident,
			Title:        title,
			State:        t.State,
			Priority:     t.Priority,
			Dispatchable: len(blockedBy) == 0 && (t.State == "Pending" || t.InFlight || !kernel(t.State)),
			TaskID:       t.ID,
			Stage:        stage,
			BlockedBy:    blockedBy,
			SubIssues:    subIssues,
		})
	}

	for _, item := range m.snap.backlog.Items {
		if item.SourceRef == "" || seenTask[item.ID] || seenRef[item.SourceRef] {
			continue
		}
		seenTask[item.ID] = true
		seenRef[item.SourceRef] = true
		mergeIssue("Linked Fae Tasks", LinearIssueView{
			ID:           item.ID,
			Identifier:   item.SourceRef,
			State:        item.State,
			Priority:     item.Priority,
			Dispatchable: item.State == "Pending",
			TaskID:       item.ID,
		})
	}

	q := strings.ToLower(strings.TrimSpace(m.filter))
	rows := make([]symphonyRow, 0, len(order))
	for _, k := range order {
		iss := *byKey[k]
		// Enrich from board/detail if TaskID is known
		if iss.TaskID != "" {
			if t, ok := m.snap.tasks[iss.TaskID]; ok {
				if iss.Stage == "" {
					iss.Stage = t.State
				}
				if len(iss.SubIssues) == 0 && t.Child != nil {
					for _, ch := range t.Child.Children {
						if ch.Task != "" {
							iss.SubIssues = append(iss.SubIssues, ch.Task)
						}
					}
					if len(iss.SubIssues) == 0 && t.Child.Task != "" {
						iss.SubIssues = append(iss.SubIssues, t.Child.Task)
					}
				}
			}
			if d, ok := m.cachedDetail(iss.TaskID); ok {
				if iss.Title == "" {
					iss.Title = d.Title
				}
				if iss.Stage == "" {
					iss.Stage = d.Stage
				}
			}
		}
		if q != "" {
			hay := strings.ToLower(strings.Join([]string{
				iss.Identifier, iss.ID, iss.Title, iss.State, iss.TaskID, iss.Stage, iss.LastError,
				strings.Join(iss.BlockedBy, " "), strings.Join(iss.SubIssues, " "),
			}, " "))
			if !strings.Contains(hay, q) {
				continue
			}
		}
		rows = append(rows, symphonyRow{
			section: sections[k],
			issue:   iss,
		})
	}
	return rows
}

func (m *Model) symphonyMove(delta int) {
	rows := m.symphonyRows()
	m.symphony.cursor = selectable(m.symphony.cursor+delta, len(rows))
}

func (m *Model) symphonySelectedTaskID() string {
	rows := m.symphonyRows()
	if len(rows) == 0 {
		return ""
	}
	idx := selectable(m.symphony.cursor, len(rows))
	iss := rows[idx].issue
	return firstNonEmpty(iss.TaskID, iss.ID)
}

func (m *Model) symphonyView(height int) []string {
	w := m.width
	if w <= 0 {
		w = 100
	}
	if height <= 0 {
		return nil
	}
	summary := m.symphonySummaryLine(w)
	bodyH := max(height-1, 1)
	leftW, rightW := splitWidths(w)
	rows := m.symphonyRows()
	m.symphony.cursor = selectable(m.symphony.cursor, len(rows))

	leftLines := m.symphonyLeftPane(rows, leftW, bodyH)
	var sel *symphonyRow
	if len(rows) > 0 {
		sel = &rows[m.symphony.cursor]
	}
	rightLines := m.symphonyRightPane(sel, rightW, bodyH)
	split := renderSplitPane(w, bodyH, leftLines, rightLines)
	return append([]string{fit(summary, w)}, split...)
}

func (m *Model) symphonySummaryLine(w int) string {
	now := m.now()
	ob := m.activeOutbox()
	tokens := m.snap.symphony.TotalTokens
	if tokens == 0 {
		for _, r := range m.symphonyRows() {
			tokens += r.issue.TokensTotal
		}
	}
	parts := []string{
		styleHeading.Render("Symphony / Linear Outbox"),
		fmt.Sprintf("pending:%d", ob.Pending),
		fmt.Sprintf("oldest:%.0fs", ob.OldestAgeSeconds),
		fmt.Sprintf("tokens:%d", tokens),
	}
	if !ob.CircuitOpenUntil.IsZero() && (now.IsZero() || ob.CircuitOpenUntil.After(now)) {
		parts = append(parts, styleError.Render("circuit_open_until:"+countdown(now, ob.CircuitOpenUntil)))
	} else {
		parts = append(parts, styleOK.Render("circuit:closed"))
	}
	if ob.LastError != "" {
		parts = append(parts, styleWarn.Render("last_error:"+ob.LastError))
	}
	return clip(strings.Join(parts, " · "), w)
}

func (m *Model) symphonyLeftPane(rows []symphonyRow, w, h int) []string {
	now := m.now()
	head := []string{styleHeading.Render(fmt.Sprintf("Tracker Issues & Molecule Linkage (%d)", len(rows)))}
	if len(rows) == 0 {
		return append(head,
			styleFaint.Render("No Linear/Symphony issues or linked tasks found."),
			styleFaint.Render("Press S to trigger a tracker sync refresh."),
		)
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
		if i == m.symphony.cursor {
			cursorLine = len(lines)
		}

		iss := r.issue
		ident := firstNonEmpty(iss.Identifier, iss.ID, iss.TaskID)
		dispBadge := styleOK.Render("DISP")
		if len(iss.BlockedBy) > 0 {
			dispBadge = styleWarn.Render(fmt.Sprintf("BLOCKED(%d)", len(iss.BlockedBy)))
		} else if !iss.Dispatchable {
			dispBadge = styleFaint.Render("HOLD")
		}

		meta := []string{firstNonEmpty(iss.State, "—"), dispBadge}
		if iss.TaskID != "" && iss.TaskID != ident {
			meta = append(meta, "task:"+iss.TaskID)
		}
		if iss.Stage != "" {
			meta = append(meta, "stage:"+iss.Stage)
		}
		if iss.TurnCount > 0 || iss.TokensTotal > 0 {
			meta = append(meta, fmt.Sprintf("%dt/%dtok", iss.TurnCount, iss.TokensTotal))
		}
		if iss.RetryAttempt > 0 || !iss.RetryDueAt.IsZero() {
			retryStr := fmt.Sprintf("retry#%d", iss.RetryAttempt)
			if !iss.RetryDueAt.IsZero() {
				retryStr += " in " + countdown(now, iss.RetryDueAt)
			}
			meta = append(meta, styleWarn.Render(retryStr))
		}

		line := fmt.Sprintf("  %s  %s", styleBold.Render(ident), strings.Join(meta, " · "))
		if i == m.symphony.cursor {
			line = styleSelected.Render(fit(ansiStrip(line), w))
		} else {
			line = clip(line, w)
		}
		lines = append(lines, line)
	}

	room := max(h-len(head), 1)
	m.symphony.scroll = scrollTo(m.symphony.scroll, cursorLine, room, len(lines))
	return append(head, window(lines, m.symphony.scroll, room)...)
}

func (m *Model) symphonyRightPane(sel *symphonyRow, w, h int) []string {
	now := m.now()
	lines := []string{styleHeading.Render("Issue & Tracker Inspector")}
	hints := fmt.Sprintf("%s:sync-refresh  %s:inspect-task",
		styleKey.Render("S"), styleKey.Render("enter"))

	if sel == nil {
		lines = append(lines, styleFaint.Render("Select an issue or linked task on the left to inspect."))
		lines = append(lines, "", hints)
		return clipLines(lines, w, h)
	}

	iss := sel.issue
	ident := firstNonEmpty(iss.Identifier, iss.ID, iss.TaskID)
	lines = append(lines,
		fmt.Sprintf("  %-16s %s", "Identifier", styleBold.Render(ident)),
		fmt.Sprintf("  %-16s %s", "Title", firstNonEmpty(iss.Title, "(untitled)")),
		fmt.Sprintf("  %-16s %s", "State", firstNonEmpty(iss.State, "—")),
		fmt.Sprintf("  %-16s %t", "Dispatchable", iss.Dispatchable),
		fmt.Sprintf("  %-16s %s", "Linked TaskID", firstNonEmpty(iss.TaskID, "—")),
		fmt.Sprintf("  %-16s %s", "Stage", firstNonEmpty(iss.Stage, "—")),
		fmt.Sprintf("  %-16s %d turns · %d tokens", "Telemetry", iss.TurnCount, iss.TokensTotal),
	)
	if iss.URL != "" {
		lines = append(lines, fmt.Sprintf("  %-16s %s", "URL", iss.URL))
	}
	if iss.SessionID != "" {
		lines = append(lines, fmt.Sprintf("  %-16s %s", "SessionID", iss.SessionID))
	}
	if iss.RetryAttempt > 0 || !iss.RetryDueAt.IsZero() || iss.LastError != "" {
		lines = append(lines, "", styleHeading.Render("Retry & Error State"))
		lines = append(lines, fmt.Sprintf("  %-16s %d", "RetryAttempt", iss.RetryAttempt))
		if !iss.RetryDueAt.IsZero() {
			lines = append(lines, fmt.Sprintf("  %-16s %s (in %s)", "RetryDueAt", stamp(now, iss.RetryDueAt), countdown(now, iss.RetryDueAt)))
		}
		if iss.LastError != "" {
			lines = append(lines, fmt.Sprintf("  %-16s %s", "LastError", styleError.Render(iss.LastError)))
		}
	}

	lines = append(lines, "", styleHeading.Render(fmt.Sprintf("BlockedBy Chain (%d)", len(iss.BlockedBy))))
	if len(iss.BlockedBy) > 0 {
		for _, b := range iss.BlockedBy {
			lines = append(lines, "  ⇐ "+styleWarn.Render(b))
		}
	} else {
		lines = append(lines, "  "+styleFaint.Render("none (unblocked)"))
	}

	subIssues := append([]string(nil), iss.SubIssues...)
	if iss.TaskID != "" {
		if t, ok := m.snap.tasks[iss.TaskID]; ok && t.Child != nil {
			for _, ch := range t.Child.Children {
				if ch.Task != "" && !strings.Contains(strings.Join(subIssues, " "), ch.Task) {
					subIssues = append(subIssues, fmt.Sprintf("%s (%s@r%d)", ch.Task, ch.WorkflowID, ch.Revision))
				}
			}
		}
	}
	lines = append(lines, "", styleHeading.Render(fmt.Sprintf("SubIssues Hierarchy (%d)", len(subIssues))))
	if len(subIssues) > 0 {
		for _, sub := range subIssues {
			lines = append(lines, "  ↳ "+sub)
		}
	} else {
		lines = append(lines, "  "+styleFaint.Render("no sub-issues / child tasks"))
	}

	lines = append(lines, "", hints)
	return clipLines(lines, w, h)
}
