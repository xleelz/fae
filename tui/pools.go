package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

type poolRowKind int

const (
	poolRowPool poolRowKind = iota
	poolRowBinding
	poolRowPause
)

type poolRow struct {
	kind    poolRowKind
	section string
	pool    *ProviderPoolView
	binding *ProviderBindingHealthView
	pause   *DispatchPauseView
}

func (m *Model) activePools() []ProviderPoolView {
	if len(m.snap.pools) > 0 {
		return m.snap.pools
	}
	return m.snap.status.ProviderPools
}

func (m *Model) activeHealth() []ProviderBindingHealthView {
	if len(m.snap.health) > 0 {
		return m.snap.health
	}
	return m.snap.status.ProviderHealth
}

func (m *Model) activePauses() []DispatchPauseView {
	if len(m.snap.pauses) > 0 {
		return m.snap.pauses
	}
	return m.snap.status.DispatchPauses
}

func (m *Model) poolsRows() []poolRow {
	q := strings.ToLower(strings.TrimSpace(m.filter))
	rows := []poolRow{}

	pools := m.activePools()
	for i := range pools {
		p := pools[i]
		if q != "" {
			hay := strings.ToLower(strings.Join([]string{
				p.ID, p.Provider, p.CredentialIdentity, p.PauseActor, p.PauseReason, p.QuotaReason, p.CutOffTaskID,
				strings.Join(p.Holders, " "),
			}, " "))
			if !strings.Contains(hay, q) {
				continue
			}
		}
		pCopy := p
		if pCopy.ID == "" && (pCopy.Provider != "" || pCopy.CredentialIdentity != "") {
			pCopy.ID = strings.Trim(pCopy.Provider+"/"+pCopy.CredentialIdentity, "/")
		}
		rows = append(rows, poolRow{
			kind:    poolRowPool,
			section: "Provider Concurrency Pools (FAE-SCH-015)",
			pool:    &pCopy,
		})
	}

	health := m.activeHealth()
	for i := range health {
		b := health[i]
		if q != "" {
			hay := strings.ToLower(strings.Join([]string{
				b.Provider, b.Model, b.BindingID, b.Outcome, b.CutOffTaskID,
			}, " "))
			if !strings.Contains(hay, q) {
				continue
			}
		}
		bCopy := b
		rows = append(rows, poolRow{
			kind:    poolRowBinding,
			section: "Provider Binding Health & Backoff Probes (FAE-RT-018)",
			binding: &bCopy,
		})
	}

	pauses := m.activePauses()
	for i := range pauses {
		pa := pauses[i]
		if q != "" {
			hay := strings.ToLower(strings.Join([]string{
				pa.Scope, pa.Target, pa.Actor, pa.Reason,
			}, " "))
			if !strings.Contains(hay, q) {
				continue
			}
		}
		paCopy := pa
		rows = append(rows, poolRow{
			kind:    poolRowPause,
			section: "Active Scoped Pauses (DispatchPauseList)",
			pause:   &paCopy,
		})
	}

	return rows
}

func (m *Model) poolsMove(delta int) {
	rows := m.poolsRows()
	m.pools.cursor = selectable(m.pools.cursor+delta, len(rows))
}

func (m *Model) poolsSelectedPool() *ProviderPoolView {
	rows := m.poolsRows()
	if len(rows) > 0 {
		idx := selectable(m.pools.cursor, len(rows))
		r := rows[idx]
		if r.pool != nil {
			cp := *r.pool
			return &cp
		}
		if r.binding != nil {
			for _, p := range m.activePools() {
				if p.Provider == r.binding.Provider {
					cp := p
					return &cp
				}
			}
		}
		if r.pause != nil && r.pause.Scope == "pool" {
			for _, p := range m.activePools() {
				if p.ID == r.pause.Target || p.Provider == r.pause.Target {
					cp := p
					return &cp
				}
			}
		}
	}
	pools := m.activePools()
	if len(pools) > 0 {
		cp := pools[selectable(m.pools.cursor, len(pools))]
		if cp.ID == "" && (cp.Provider != "" || cp.CredentialIdentity != "") {
			cp.ID = strings.Trim(cp.Provider+"/"+cp.CredentialIdentity, "/")
		}
		return &cp
	}
	return nil
}

func (m *Model) poolsSelectedPause() *DispatchPauseView {
	rows := m.poolsRows()
	if len(rows) > 0 {
		idx := selectable(m.pools.cursor, len(rows))
		r := rows[idx]
		if r.pause != nil {
			cp := *r.pause
			return &cp
		}
		if r.pool != nil && r.pool.Paused {
			for _, pa := range m.activePauses() {
				if pa.Scope == "pool" && (pa.Target == r.pool.ID || pa.Target == r.pool.Provider) {
					cp := pa
					return &cp
				}
			}
			return &DispatchPauseView{
				Scope:  "pool",
				Target: r.pool.ID,
				Paused: true,
				Actor:  r.pool.PauseActor,
				Reason: r.pool.PauseReason,
			}
		}
	}
	pauses := m.activePauses()
	if len(pauses) > 0 {
		cp := pauses[selectable(m.pools.cursor, len(pauses))]
		return &cp
	}
	return nil
}

func (m *Model) poolsView(height int) []string {
	w := m.width
	if w <= 0 {
		w = 100
	}
	if height <= 0 {
		return nil
	}
	leftW, rightW := splitWidths(w)
	rows := m.poolsRows()
	m.pools.cursor = selectable(m.pools.cursor, len(rows))

	leftLines := m.poolsLeftPane(rows, leftW, height)
	var sel *poolRow
	if len(rows) > 0 {
		sel = &rows[m.pools.cursor]
	}
	rightLines := m.poolsRightPane(sel, rightW, height)
	return renderSplitPane(w, height, leftLines, rightLines)
}

func (m *Model) poolsLeftPane(rows []poolRow, w, h int) []string {
	now := m.now()
	head := []string{styleHeading.Render(fmt.Sprintf("Providers, Concurrency Pools & Quotas (%d)", len(rows)))}
	if len(rows) == 0 {
		return append(head,
			styleFaint.Render("No provider pools, binding health records, or active dispatch pauses."),
			styleFaint.Render("Use s:resize-pool, q:quota-set, or p:pause-scope to configure controls."),
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
		if i == m.pools.cursor {
			cursorLine = len(lines)
		}

		var line string
		switch r.kind {
		case poolRowPool:
			p := r.pool
			id := p.ID
			if id == "" {
				id = strings.Trim(p.Provider+"/"+p.CredentialIdentity, "/")
			}
			capVal := p.AdmissionLimit
			if capVal <= 0 {
				capVal = p.Capacity
			}
			used := len(p.Holders)
			gauge := fmt.Sprintf("%s %d/%d", asciiGauge(used, capVal, 4), used, capVal)
			badge := poolStateBadge(*p, now)
			line = fmt.Sprintf("  %s  %s  %s  holders:%d  wait:%d",
				styleBold.Render(id), gauge, badge, used, p.Waiting)
			if !p.QuotaExhaustedUntil.IsZero() {
				line += "  " + styleWarn.Render("resets in "+countdown(now, p.QuotaExhaustedUntil))
			} else if !p.QuotaResetAt.IsZero() && p.QuotaResetAt.After(now) {
				line += "  " + styleWarn.Render("resets in "+countdown(now, p.QuotaResetAt))
			}

		case poolRowBinding:
			b := r.binding
			pm := strings.Trim(b.Provider+"/"+b.Model, "/")
			if b.BindingID != "" && pm == "" {
				pm = b.BindingID
			}
			elig := styleOK.Render("ELIG")
			if !b.Eligible {
				elig = styleError.Render("INELIG")
			}
			line = fmt.Sprintf("  %s  score:%.2f  %s  probes:%d",
				styleBold.Render(pm), b.EffectiveScore, elig, b.RateLimitProbes)
			if !b.NextProbeAt.IsZero() {
				line += "  " + styleWarn.Render("probe in "+countdown(now, b.NextProbeAt))
			} else if !b.QuotaExhaustedUntil.IsZero() {
				line += "  " + styleWarn.Render("resets in "+countdown(now, b.QuotaExhaustedUntil))
			}

		case poolRowPause:
			pa := r.pause
			target := firstNonEmpty(pa.Target, "*")
			actor := firstNonEmpty(pa.Actor, "operator")
			reason := firstNonEmpty(pa.Reason, "(no reason)")
			line = fmt.Sprintf("  [%s] %s  by %s — %s",
				styleWarn.Render(pa.Scope), styleBold.Render(target), actor, reason)
		}

		if i == m.pools.cursor {
			line = styleSelected.Render(fit(ansiStrip(line), w))
		} else {
			line = clip(line, w)
		}
		lines = append(lines, line)
	}

	room := max(h-len(head), 1)
	m.pools.scroll = scrollTo(m.pools.scroll, cursorLine, room, len(lines))
	return append(head, window(lines, m.pools.scroll, room)...)
}

func (m *Model) poolsRightPane(sel *poolRow, w, h int) []string {
	now := m.now()
	lines := []string{styleHeading.Render("Pool / Binding / Pause Inspector")}
	hints := fmt.Sprintf("%s:resize-pool  %s:quota-set  %s:quota-clear  %s:pause-scope  %s:unpause-scope",
		styleKey.Render("s"), styleKey.Render("q"), styleKey.Render("Q"), styleKey.Render("p"), styleKey.Render("u"))

	if sel == nil {
		lines = append(lines, styleFaint.Render("Select a pool, binding, or scoped pause on the left."))
		lines = append(lines, "", hints)
		return clipLines(lines, w, h)
	}

	switch sel.kind {
	case poolRowPool:
		p := sel.pool
		id := firstNonEmpty(p.ID, strings.Trim(p.Provider+"/"+p.CredentialIdentity, "/"))
		capVal := p.AdmissionLimit
		if capVal <= 0 {
			capVal = p.Capacity
		}
		lines = append(lines,
			fmt.Sprintf("  %-18s %s", "Pool ID", styleBold.Render(id)),
			fmt.Sprintf("  %-18s %s", "Provider", firstNonEmpty(p.Provider, "—")),
			fmt.Sprintf("  %-18s %s", "Credential", firstNonEmpty(p.CredentialIdentity, "—")),
			fmt.Sprintf("  %-18s %s", "State", poolStateBadge(*p, now)),
			fmt.Sprintf("  %-18s %s (%d active / %d limit, cap %d)",
				"Slots", asciiGauge(len(p.Holders), capVal, 6), len(p.Holders), capVal, p.Capacity),
			fmt.Sprintf("  %-18s %d", "Waiting Queue", p.Waiting),
		)
		if p.Paused {
			lines = append(lines,
				fmt.Sprintf("  %-18s %s (by %s)", "Paused", styleWarn.Render("yes"), firstNonEmpty(p.PauseActor, "operator")),
				fmt.Sprintf("  %-18s %s", "Pause Reason", firstNonEmpty(p.PauseReason, "—")),
			)
		}
		if !p.QuotaResetAt.IsZero() {
			lines = append(lines, fmt.Sprintf("  %-18s %s (in %s)", "QuotaResetAt", stamp(now, p.QuotaResetAt), countdown(now, p.QuotaResetAt)))
		}
		if !p.QuotaExhaustedUntil.IsZero() {
			lines = append(lines, fmt.Sprintf("  %-18s %s (resets in %s)", "QuotaExhausted", stamp(now, p.QuotaExhaustedUntil), countdown(now, p.QuotaExhaustedUntil)))
		}
		if p.CutOffTaskID != "" {
			lines = append(lines, fmt.Sprintf("  %-18s %s", "CutOffTaskID", styleWarn.Render(p.CutOffTaskID)))
		}
		if p.QuotaReason != "" {
			lines = append(lines, fmt.Sprintf("  %-18s %s", "QuotaReason", p.QuotaReason))
		}
		lines = append(lines, "", styleHeading.Render(fmt.Sprintf("Active Slot Holders (%d)", len(p.Holders))))
		if len(p.Holders) > 0 {
			for _, hTask := range p.Holders {
				lines = append(lines, "  • "+hTask)
			}
		} else {
			lines = append(lines, "  "+styleFaint.Render("no active slot holders"))
		}

	case poolRowBinding:
		b := sel.binding
		pm := strings.Trim(b.Provider+"/"+b.Model, "/")
		lines = append(lines,
			fmt.Sprintf("  %-18s %s", "Binding", styleBold.Render(pm)),
			fmt.Sprintf("  %-18s %s", "Binding ID", firstNonEmpty(b.BindingID, "—")),
			fmt.Sprintf("  %-18s %.2f (base %.2f)", "EffectiveScore", b.EffectiveScore, b.Score),
			fmt.Sprintf("  %-18s %t", "Eligible", b.Eligible),
			fmt.Sprintf("  %-18s %s (failures: %d)", "Last Outcome", firstNonEmpty(b.Outcome, "ok"), b.Failures),
			fmt.Sprintf("  %-18s %d", "RateLimitProbes", b.RateLimitProbes),
		)
		if !b.QuotaResetAt.IsZero() {
			lines = append(lines, fmt.Sprintf("  %-18s %s (in %s)", "QuotaResetAt", stamp(now, b.QuotaResetAt), countdown(now, b.QuotaResetAt)))
		}
		if !b.QuotaExhaustedUntil.IsZero() {
			lines = append(lines, fmt.Sprintf("  %-18s %s (resets in %s)", "QuotaExhausted", stamp(now, b.QuotaExhaustedUntil), countdown(now, b.QuotaExhaustedUntil)))
		}
		if !b.NextProbeAt.IsZero() {
			lines = append(lines, fmt.Sprintf("  %-18s %s (in %s)", "NextProbeAt", stamp(now, b.NextProbeAt), countdown(now, b.NextProbeAt)))
		}
		if b.CutOffTaskID != "" {
			lines = append(lines, fmt.Sprintf("  %-18s %s", "CutOffTaskID", styleWarn.Render(b.CutOffTaskID)))
		}

	case poolRowPause:
		pa := sel.pause
		lines = append(lines,
			fmt.Sprintf("  %-18s %s", "Scope", styleWarn.Render(pa.Scope)),
			fmt.Sprintf("  %-18s %s", "Target", styleBold.Render(firstNonEmpty(pa.Target, "*"))),
			fmt.Sprintf("  %-18s %t", "Paused", pa.Paused),
			fmt.Sprintf("  %-18s %s", "Actor", firstNonEmpty(pa.Actor, "operator")),
			fmt.Sprintf("  %-18s %s", "Reason", firstNonEmpty(pa.Reason, "—")),
		)
		if !pa.UpdatedAt.IsZero() {
			lines = append(lines, fmt.Sprintf("  %-18s %s (%s)", "UpdatedAt", stamp(now, pa.UpdatedAt), ago(now, pa.UpdatedAt)))
		}
	}

	// Scheduler WithheldTasks section
	withheld := map[string]string{}
	if m.snap.status.Snapshot != nil {
		withheld = m.snap.status.Snapshot.Scheduler.WithheldTasks
	}
	lines = append(lines, "", styleHeading.Render(fmt.Sprintf("WithheldTasks (%d)", len(withheld))))
	if len(withheld) > 0 {
		ids := make([]string, 0, len(withheld))
		for id := range withheld {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			lines = append(lines, fmt.Sprintf("  • %s: %s", styleBold.Render(id), styleWarn.Render(withheld[id])))
		}
	} else {
		lines = append(lines, "  "+styleFaint.Render("no tasks withheld by scheduler"))
	}

	lines = append(lines, "", hints)
	return clipLines(lines, w, h)
}

func poolStateBadge(p ProviderPoolView, now time.Time) string {
	if p.Paused {
		return styleWarn.Render("PAUSED")
	}
	if !p.QuotaExhaustedUntil.IsZero() && (now.IsZero() || p.QuotaExhaustedUntil.After(now)) {
		return styleError.Render("QUOTA!")
	}
	capVal := p.AdmissionLimit
	if capVal <= 0 {
		capVal = p.Capacity
	}
	if capVal > 0 && len(p.Holders) >= capVal {
		return styleWarn.Render("BUSY")
	}
	return styleOK.Render("ELIG")
}

func countdown(now, target time.Time) string {
	if target.IsZero() {
		return "—"
	}
	if now.IsZero() {
		return target.Format("15:04:05")
	}
	d := target.Sub(now)
	if d <= 0 {
		return "0s"
	}
	return age(d)
}
