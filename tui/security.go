package tui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/xleelz/fae/tui/client"
)

type securityRowKind int

const (
	securityRowTask securityRowKind = iota
	securityRowTarget
	securityRowEvent
)

type securityRow struct {
	kind    securityRowKind
	section string
	taskID  string
	task    *client.Task
	detail  *client.TaskDetail
	target  *TargetConfigView
	event   *EventView
}

func (m *Model) securityRows() []securityRow {
	q := strings.ToLower(strings.TrimSpace(m.filter))
	rows := []securityRow{}

	matches := func(parts ...string) bool {
		if q == "" {
			return true
		}
		return strings.Contains(strings.ToLower(strings.Join(parts, " ")), q)
	}

	seenTasks := map[string]bool{}
	allTasks := m.streamTasks()
	var widenRows, scopedRows, fallbackTaskRows []securityRow

	addTaskCandidate := func(id string, t *client.Task, d *client.TaskDetail) {
		if id == "" || seenTasks[id] {
			return
		}
		seenTasks[id] = true

		var escKind, reason, state string
		if t != nil {
			escKind = t.EscalationKind
			reason = t.Reason
			state = t.State
		}
		if d != nil {
			escKind = firstNonEmpty(d.EscalationKind, escKind)
			reason = firstNonEmpty(d.Reason, d.EscalationReason, reason)
			state = firstNonEmpty(d.State, state)
		}

		isWiden := (d != nil && d.PendingRequest != nil) ||
			strings.EqualFold(escKind, "ScopeWiden") ||
			strings.Contains(strings.ToLower(reason), "widen") ||
			strings.Contains(strings.ToLower(escKind), "widen")

		hasScopePolicy := d != nil && (len(d.ReadOnlyFiles) > 0 ||
			d.Reads != nil ||
			len(d.ApprovedScope) > 0 ||
			len(d.DeclaredFiles) > 0 ||
			len(d.DenyPaths) > 0)

		var hay []string
		hay = append(hay, id, state, escKind, reason)
		if d != nil {
			hay = append(hay, d.DeclaredFiles...)
			hay = append(hay, d.ApprovedScope...)
			hay = append(hay, d.ReadOnlyFiles...)
			hay = append(hay, d.DenyPaths...)
			if d.PendingRequest != nil {
				hay = append(hay, d.PendingRequest.Kind, d.PendingRequest.Reason)
				hay = append(hay, d.PendingRequest.Paths...)
				hay = append(hay, d.PendingRequest.Files...)
			}
			if d.Reads != nil {
				hay = append(hay, d.Reads.Allow...)
				hay = append(hay, d.Reads.Deny...)
			}
		}
		if !matches(hay...) {
			return
		}

		row := securityRow{
			kind:    securityRowTask,
			section: "Pending Scope-Widen & Rework Requests",
			taskID:  id,
			task:    t,
			detail:  d,
		}
		switch {
		case isWiden:
			widenRows = append(widenRows, row)
		case hasScopePolicy:
			scopedRows = append(scopedRows, row)
		default:
			fallbackTaskRows = append(fallbackTaskRows, row)
		}
	}

	for i := range allTasks {
		t := allTasks[i]
		tCopy := t
		var dPtr *client.TaskDetail
		if d, ok := m.cachedDetail(t.ID); ok {
			dCopy := d
			dPtr = &dCopy
		}
		addTaskCandidate(t.ID, &tCopy, dPtr)
	}

	if len(m.snap.details) > 0 {
		ids := make([]string, 0, len(m.snap.details))
		for id := range m.snap.details {
			if !seenTasks[id] {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		for _, id := range ids {
			d := m.snap.details[id]
			dCopy := d
			addTaskCandidate(id, nil, &dCopy)
		}
	}

	rows = append(rows, widenRows...)
	rows = append(rows, scopedRows...)
	if len(widenRows) == 0 && len(scopedRows) == 0 {
		rows = append(rows, fallbackTaskRows...)
	}

	// Section 2: Target ReadRoots, DenyPaths & GitPush Policies
	for i := range m.snap.targets {
		tg := m.snap.targets[i]
		hay := []string{tg.ID, tg.PushMode, tg.PushRemote, tg.PushBranch}
		hay = append(hay, tg.DenyPaths...)
		hay = append(hay, tg.DocumentGlobs...)
		for k, rr := range tg.ReadRoots {
			hay = append(hay, k, rr.Name, rr.Path, rr.Commit, rr.Digest)
			hay = append(hay, rr.Deny...)
		}
		if !matches(hay...) {
			continue
		}
		tgCopy := tg
		rows = append(rows, securityRow{
			kind:    securityRowTarget,
			section: "Target ReadRoots, DenyPaths & GitPush Policies",
			target:  &tgCopy,
		})
	}

	// Section 3: Security & Refused-Read Audit Events
	var secEvents, otherEvents []securityRow
	for i := range m.snap.events {
		ev := m.snap.events[i]
		if !matches(ev.Kind, ev.Task, ev.Actor, ev.Summary, string(ev.Payload)) {
			continue
		}
		evCopy := ev
		row := securityRow{
			kind:    securityRowEvent,
			section: "Security & Refused-Read Audit Events",
			taskID:  ev.Task,
			event:   &evCopy,
		}
		if isSecurityEvent(ev) {
			secEvents = append(secEvents, row)
		} else {
			otherEvents = append(otherEvents, row)
		}
	}
	if len(secEvents) > 0 {
		rows = append(rows, secEvents...)
	} else {
		rows = append(rows, otherEvents...)
	}

	return rows
}

func isSecurityEvent(ev EventView) bool {
	hay := strings.ToLower(ev.Kind + " " + ev.Summary + " " + string(ev.Payload))
	for _, kw := range []string{"refused", "scope", "widen", "external", "git-push", "deny", "security", "provenance", "land"} {
		if strings.Contains(hay, kw) {
			return true
		}
	}
	return false
}

func (m *Model) securityMove(delta int) {
	rows := m.securityRows()
	m.security.cursor = selectable(m.security.cursor+delta, len(rows))
}

func (m *Model) securitySelectedTaskID() string {
	rows := m.securityRows()
	if len(rows) == 0 {
		return ""
	}
	idx := selectable(m.security.cursor, len(rows))
	if rows[idx].taskID != "" {
		return rows[idx].taskID
	}
	for _, r := range rows {
		if r.taskID != "" {
			return r.taskID
		}
	}
	return ""
}

func (m *Model) securityView(height int) []string {
	w := m.width
	if w <= 0 {
		w = 100
	}
	if height <= 0 {
		return nil
	}
	leftW, rightW := splitWidths(w)
	rows := m.securityRows()
	m.security.cursor = selectable(m.security.cursor, len(rows))

	leftLines := m.securityLeftPane(rows, leftW, height)
	var sel *securityRow
	if len(rows) > 0 {
		sel = &rows[m.security.cursor]
	}
	rightLines := m.securityRightPane(sel, rightW, height)
	return renderSplitPane(w, height, leftLines, rightLines)
}

func (m *Model) securityLeftPane(rows []securityRow, w, h int) []string {
	now := m.now()
	head := []string{styleHeading.Render(fmt.Sprintf("Security, Scope & Provenance (%d)", len(rows)))}
	if len(rows) == 0 {
		return append(head,
			styleFaint.Render("No pending scope-widen requests, target policies, or security audit events."),
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
		if i == m.security.cursor {
			cursorLine = len(lines)
		}

		var line string
		switch r.kind {
		case securityRowTask:
			badges := []string{}
			d := r.detail
			t := r.task
			if d != nil && d.PendingRequest != nil {
				reqFiles := append([]string(nil), d.PendingRequest.Paths...)
				reqFiles = append(reqFiles, d.PendingRequest.Files...)
				badges = append(badges, styleError.Render(fmt.Sprintf("REQ:%s(%d)", firstNonEmpty(d.PendingRequest.Kind, "ScopeWiden"), len(reqFiles))))
			} else if (t != nil && t.EscalationKind == "ScopeWiden") || (d != nil && d.EscalationKind == "ScopeWiden") {
				badges = append(badges, styleError.Render("ESC:ScopeWiden"))
			}
			if d != nil {
				if len(d.ApprovedScope) > 0 {
					badges = append(badges, styleOK.Render(fmt.Sprintf("scope:%d", len(d.ApprovedScope))))
				} else if len(d.DeclaredFiles) > 0 {
					badges = append(badges, fmt.Sprintf("decl:%d", len(d.DeclaredFiles)))
				}
				if len(d.ReadOnlyFiles) > 0 {
					badges = append(badges, fmt.Sprintf("ro:%d", len(d.ReadOnlyFiles)))
				}
				if d.Reads != nil {
					badges = append(badges, fmt.Sprintf("reads:+%d/-%d", len(d.Reads.Allow), len(d.Reads.Deny)))
				}
				if len(d.DenyPaths) > 0 {
					badges = append(badges, styleWarn.Render(fmt.Sprintf("deny:%d", len(d.DenyPaths))))
				}
			}
			if len(badges) == 0 && t != nil {
				badges = append(badges, t.State)
				if t.Reason != "" {
					badges = append(badges, styleWarn.Render(t.Reason))
				}
			}
			line = fmt.Sprintf("  %s  %s", styleBold.Render(r.taskID), strings.Join(badges, " · "))

		case securityRowTarget:
			tg := r.target
			push := firstNonEmpty(tg.PushMode, "disabled")
			line = fmt.Sprintf("  target:%s@r%d  roots:%d  deny:%d  docs:%d  push:%s",
				styleBold.Render(tg.ID), tg.Revision, len(tg.ReadRoots), len(tg.DenyPaths), len(tg.DocumentGlobs), push)

		case securityRowEvent:
			ev := r.event
			kindBadge := ev.Kind
			if isSecurityEvent(*ev) {
				kindBadge = styleWarn.Render(ev.Kind)
			}
			taskStr := ""
			if ev.Task != "" {
				taskStr = " [" + ev.Task + "]"
			}
			when := ""
			if !ev.At.IsZero() {
				when = " " + styleFaint.Render(stamp(now, ev.At))
			}
			line = fmt.Sprintf("  #%d %s%s %s%s", ev.ID, kindBadge, taskStr, ev.Summary, when)
		}

		if i == m.security.cursor {
			line = styleSelected.Render(fit(ansiStrip(line), w))
		} else {
			line = clip(line, w)
		}
		lines = append(lines, line)
	}

	room := max(h-len(head), 1)
	m.security.scroll = scrollTo(m.security.scroll, cursorLine, room, len(lines))
	return append(head, window(lines, m.security.scroll, room)...)
}

func (m *Model) securityRightPane(sel *securityRow, w, h int) []string {
	now := m.now()
	lines := []string{styleHeading.Render("Scope & Security Policy Inspector")}
	hints := fmt.Sprintf("%s:approve-widen  %s:land-task  %s:inspect-task",
		styleKey.Render("w"), styleKey.Render("L"), styleKey.Render("enter"))

	if sel == nil {
		lines = append(lines, styleFaint.Render("Select a task, target policy, or security audit event on the left."))
		lines = append(lines, "", hints)
		return clipLines(lines, w, h)
	}

	switch sel.kind {
	case securityRowTask:
		d := sel.detail
		t := sel.task
		state := ""
		reason := ""
		if t != nil {
			state = t.State
			reason = t.Reason
		}
		if d != nil {
			state = firstNonEmpty(d.State, state)
			reason = firstNonEmpty(d.Reason, d.EscalationReason, reason)
		}
		lines = append(lines,
			fmt.Sprintf("  %-18s %s", "Task ID", styleBold.Render(sel.taskID)),
			fmt.Sprintf("  %-18s %s", "State", firstNonEmpty(state, "—")),
		)
		if reason != "" {
			lines = append(lines, fmt.Sprintf("  %-18s %s", "Reason", styleWarn.Render(reason)))
		}

		if d != nil && d.PendingRequest != nil {
			pr := d.PendingRequest
			reqPaths := append([]string(nil), pr.Paths...)
			reqPaths = append(reqPaths, pr.Files...)
			lines = append(lines,
				"",
				styleHeading.Render("PendingRequest ("+firstNonEmpty(pr.Kind, "ScopeWiden")+")"),
				fmt.Sprintf("  %-18s %s", "Kind", styleError.Render(firstNonEmpty(pr.Kind, "ScopeWiden"))),
				fmt.Sprintf("  %-18s %s", "Paths / Files", firstNonEmpty(strings.Join(reqPaths, ", "), "—")),
				fmt.Sprintf("  %-18s %s", "Reason", firstNonEmpty(pr.Reason, "—")),
			)
			if pr.TestFile != "" {
				lines = append(lines, fmt.Sprintf("  %-18s %s", "TestFile", pr.TestFile))
			}
		} else {
			lines = append(lines, "", styleHeading.Render("PendingRequest"), "  "+styleFaint.Render("none"))
		}

		var decl, appr, ro, deny []string
		var reads *ReadPolicyView
		if d != nil {
			decl = d.DeclaredFiles
			appr = d.ApprovedScope
			ro = d.ReadOnlyFiles
			deny = d.DenyPaths
			reads = d.Reads
		}
		lines = append(lines,
			"",
			styleHeading.Render("Write & Read Scope (FAE-RT-017 / FAE-SEC-008 / FAE-SEC-009)"),
			fmt.Sprintf("  %-18s %s", "DeclaredFiles", firstNonEmpty(strings.Join(decl, ", "), "(none)")),
			fmt.Sprintf("  %-18s %s", "ApprovedScope", firstNonEmpty(strings.Join(appr, ", "), "(none)")),
			fmt.Sprintf("  %-18s %s", "ReadOnlyFiles", firstNonEmpty(strings.Join(ro, ", "), "(none)")),
			fmt.Sprintf("  %-18s %s", "DenyPaths", firstNonEmpty(strings.Join(deny, ", "), "(none)")),
		)
		if reads != nil {
			lines = append(lines,
				fmt.Sprintf("  %-18s %s", "Reads.Allow", firstNonEmpty(strings.Join(reads.Allow, ", "), "(none)")),
				fmt.Sprintf("  %-18s %s", "Reads.Deny", firstNonEmpty(strings.Join(reads.Deny, ", "), "(none)")),
			)
		} else {
			lines = append(lines, fmt.Sprintf("  %-18s %s", "Reads Policy", styleFaint.Render("(default)")))
		}

	case securityRowTarget:
		tg := sel.target
		lines = append(lines,
			fmt.Sprintf("  %-18s %s (rev %d)", "Target ID", styleBold.Render(tg.ID), tg.Revision),
			fmt.Sprintf("  %-18s %s", "PushMode", firstNonEmpty(tg.PushMode, "disabled")),
		)
		if tg.PushRemote != "" || tg.PushBranch != "" {
			lines = append(lines, fmt.Sprintf("  %-18s %s / %s", "Push Target", firstNonEmpty(tg.PushRemote, "origin"), firstNonEmpty(tg.PushBranch, "main")))
		}
		lines = append(lines,
			fmt.Sprintf("  %-18s %s", "DenyPaths", firstNonEmpty(strings.Join(tg.DenyPaths, ", "), "(none)")),
			fmt.Sprintf("  %-18s %s", "DocumentGlobs", firstNonEmpty(strings.Join(tg.DocumentGlobs, ", "), "(none)")),
		)
		lines = append(lines, "", styleHeading.Render(fmt.Sprintf("Pinned ReadRoots (%d)", len(tg.ReadRoots))))
		if len(tg.ReadRoots) > 0 {
			names := make([]string, 0, len(tg.ReadRoots))
			for k := range tg.ReadRoots {
				names = append(names, k)
			}
			sort.Strings(names)
			for _, name := range names {
				rr := tg.ReadRoots[name]
				lines = append(lines,
					fmt.Sprintf("  [%s] path=%s", styleBold.Render(firstNonEmpty(rr.Name, name)), rr.Path),
					fmt.Sprintf("    commit=%s · digest=%s · deny=%s",
						firstNonEmpty(shortID(rr.Commit), "—"),
						firstNonEmpty(shortID(rr.Digest), "—"),
						firstNonEmpty(strings.Join(rr.Deny, ","), "none")),
				)
			}
		} else {
			lines = append(lines, "  "+styleFaint.Render("no external read roots configured"))
		}

	case securityRowEvent:
		ev := sel.event
		lines = append(lines,
			fmt.Sprintf("  %-18s #%d", "Event ID", ev.ID),
			fmt.Sprintf("  %-18s %s", "Kind", styleWarn.Render(ev.Kind)),
			fmt.Sprintf("  %-18s %s", "Task", firstNonEmpty(ev.Task, "—")),
			fmt.Sprintf("  %-18s %s", "Actor", firstNonEmpty(ev.Actor, "—")),
			fmt.Sprintf("  %-18s %s", "Timestamp", stamp(now, ev.At)),
			fmt.Sprintf("  %-18s %s", "Summary", firstNonEmpty(ev.Summary, "—")),
		)
		if len(ev.Payload) > 0 && string(ev.Payload) != "null" {
			lines = append(lines, "", styleHeading.Render("Audit Payload"))
			for _, l := range wrap(string(ev.Payload), max(w-4, 20)) {
				lines = append(lines, "  "+l)
			}
		}
	}

	lines = append(lines, "", hints)
	return clipLines(lines, w, h)
}
