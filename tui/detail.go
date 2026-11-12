package tui

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/xleelz/fae/tui/client"
)

// detailState is the open task view.
type detailState struct {
	id      string
	data    *client.TaskDetail
	err     error
	loading bool
	offset  int
}

func (m *Model) detailKey(s string) tea.Cmd {
	switch s {
	case "esc", "backspace":
		m.detail = nil
		return nil
	}
	if delta, ok := moveKey(s, m.height-6); ok {
		m.detail.offset = max(m.detail.offset+delta, 0)
	}
	return nil
}

func (m *Model) detailBody(w, h int) []string {
	d := m.detail
	lines := m.detailLines(w)
	maxOffset := max(len(lines)-h, 0)
	d.offset = min(d.offset, maxOffset)
	out := window(lines, d.offset, h)
	if maxOffset > 0 && len(out) > 0 {
		out[len(out)-1] = styleFaint.Render(fmt.Sprintf("── %d/%d lines; ↑↓ pgup pgdn scroll ──", min(d.offset+h, len(lines)), len(lines)))
	}
	for i := range out {
		out[i] = clip(out[i], w)
	}
	return out
}

// detailLines renders the task: fields, state, escalation, description and
// acceptance, structured fields, edges, then the trace with its edits.
func (m *Model) detailLines(w int) []string {
	d := m.detail
	now := m.now()
	if d.data == nil {
		switch {
		case d.err != nil:
			return []string{styleError.Render("could not read task " + d.id + ": " + d.err.Error()), styleFaint.Render("r retries · esc goes back")}
		default:
			return []string{styleFaint.Render("reading task " + d.id + "…")}
		}
	}
	t := *d.data
	state := m.detailStateOf(t)
	title := t.Title
	if title == "" {
		title = styleFaint.Render("(no title)")
	}
	lines := []string{styleBold.Render("Task "+t.ID) + "  " + title}
	if d.err != nil {
		lines = append(lines, styleWarn.Render("last refresh failed: "+d.err.Error()+" (showing the previous read)"))
	}
	field := func(name, value string) {
		if value != "" {
			for i, l := range wrap(value, max(w-16, 20)) {
				if i > 0 {
					name = ""
				}
				lines = append(lines, fmt.Sprintf("  %-12s %s", name, l))
			}
		}
	}
	field("State", state.render(state.glyph()+" "+state.Long))
	stage := t.Stage
	if stage == "" && !kernel(t.State) {
		stage = t.State
	}
	switch {
	case t.InFlight:
		stage += " (turn in flight)"
	case t.Parked && !kernel(t.State):
		stage += " (parked)"
	case t.State == "Escalated" || t.State == "Blocked":
		stage = strings.TrimSpace(stage + " (" + strings.ToLower(t.State) + " from it)")
	}
	field("Stage", stage)
	if t.Reason != "" && t.State != "Escalated" {
		field("Reason", t.Reason)
	}
	if t.EscalationKind != "" || t.EscalationReason != "" || t.State == "Escalated" {
		esc := escalation(t.EscalationKind, t.EscalationReason)
		if at := escalatedSince(t); !at.IsZero() {
			esc += styleFaint.Render("  (since " + stamp(now, at) + ", " + ago(now, at) + ")")
		}
		field("Escalation", styleError.Render(esc))
		if t.EscalationDetail != "" && !strings.Contains(esc, t.EscalationDetail) {
			field("", t.EscalationDetail)
		}
		if t.State == "Escalated" {
			field("Un-escalate", styleKey.Render("press I (or u / R / a) then Enter to un-escalate & restart this task"))
		}
	}
	uncertain := m.isUncertainDelete(t)
	if p := t.PendingDelete; p != nil {
		text := pendingDeleteText(t.ID, p, uncertain)
		if !p.RequestedAt.IsZero() {
			text += ", requested " + stamp(now, p.RequestedAt)
		}
		field("Deleting", styleWarn.Render(text))
	} else if onBoard, ok := m.snap.tasks[t.ID]; ok && onBoard.PendingDelete {
		if uncertain {
			field("Deleting", styleWarn.Render("delete pending: turn in flight is uncertain after restart; run fae resolve-uncertain --action escalate --id "+t.ID+" to finish retiring"))
		} else {
			field("Deleting", styleWarn.Render("delete pending (the Board flags it; this read did not report its status)"))
		}
	}
	if t.Deferred {
		until := "indefinitely"
		if !t.DeferredUntil.IsZero() {
			until = "until " + stamp(now, t.DeferredUntil)
		}
		field("Deferred", until)
	}
	field("Priority", fmt.Sprint(t.Priority))
	if t.AttemptLimit > 0 {
		field("Attempts", fmt.Sprintf("%d of %d", t.Attempts, t.AttemptLimit))
	}
	if t.Workflow != "" {
		field("Workflow", fmt.Sprintf("%s rev %d", t.Workflow, t.WorkflowRevision))
	}
	if t.Target != "" {
		target := t.Target
		if t.TargetRevision > 0 {
			target += fmt.Sprintf(" rev %d", t.TargetRevision)
		}
		field("Target", target)
	}
	field("Molecule", t.Molecule)
	field("Source", t.SourceRef)
	field("Write class", t.WriteClass)
	if t.State == "Pending" {
		field("Readiness", t.Readiness)
	}
	if t.FollowUpOf != "" {
		lines = append(lines, "  Follow-up of: "+t.FollowUpOf)
	}
	if tb := t.TurnBudget; tb != nil {
		maxTurns := tb.MaxAssistantTurns
		if maxTurns == 0 {
			maxTurns = tb.MaxTurns
		}
		parts := []string{}
		if tb.WallSeconds > 0 {
			parts = append(parts, fmt.Sprintf("wall %ds", tb.WallSeconds))
		}
		if maxTurns > 0 {
			parts = append(parts, fmt.Sprintf("max turns %d", maxTurns))
		}
		if tb.OnExhaustion != "" {
			parts = append(parts, "on exhaustion "+tb.OnExhaustion)
		}
		if len(parts) > 0 {
			lines = append(lines, "  Turn budget: "+strings.Join(parts, ", "))
		}
	}
	parent, child := t.Parent, t.Child
	if onBoard, ok := m.snap.tasks[t.ID]; ok {
		if parent == nil {
			parent = onBoard.Parent
		}
		if child == nil {
			child = onBoard.Child
		}
	}
	if parent != nil || child != nil {
		lines = append(lines, "", styleHeading.Render("Hierarchy"))
		if parent != nil {
			lines = append(lines, fmt.Sprintf("  Parent: %s via stage %s (visit %d) [press p to jump]", parent.Task, parent.Stage, parent.Visit))
		}
		if child != nil {
			lines = append(lines, fmt.Sprintf("  Child: %s on %s@r%d at stage %s (visit %d) [press c to jump]", child.Task, child.WorkflowID, child.Revision, child.Stage, child.Visit))
			for idx, sub := range child.Children {
				st := m.snap.stateOf(sub.Task).Short
				lines = append(lines, fmt.Sprintf("    [%d] %s (%s@r%d) — %s", idx, sub.Task, sub.WorkflowID, sub.Revision, st))
			}
		}
	}
	if f := t.Fan; f != nil {
		lines = append(lines, "", styleHeading.Render(fmt.Sprintf("FanOut (%d branches)", len(f.Branches))))
		join := f.Join
		if join == "" {
			join = "default"
		}
		lines = append(lines, fmt.Sprintf("  FanOut stage: %s · join: %s · parallel: %t", f.Stage, join, f.Parallel))
		for _, b := range f.Branches {
			bline := fmt.Sprintf("    %s  status: %s", b.ID, b.Status)
			if b.Stage != "" {
				bline += "  stage: " + b.Stage
			}
			if len(b.WriteScope) > 0 {
				bline += "  scope: " + strings.Join(b.WriteScope, ", ")
			}
			if b.CommitSHA != "" {
				bline += "  commit: " + b.CommitSHA
			}
			lines = append(lines, bline)
		}
	}
	if len(t.TurnCheckpoints) > 0 {
		lines = append(lines, "", styleHeading.Render("Turn checkpoints"))
		stages := make([]string, 0, len(t.TurnCheckpoints))
		for st := range t.TurnCheckpoints {
			stages = append(stages, st)
		}
		sort.Strings(stages)
		for _, st := range stages {
			cp := t.TurnCheckpoints[st]
			stageName := firstNonEmpty(cp.Stage, st)
			cpline := fmt.Sprintf("  %s: session %s", stageName, cp.ProviderSession)
			if cp.WorktreeTree != "" {
				cpline += " · tree " + cp.WorktreeTree
			}
			if cp.AssistantTurns > 0 {
				cpline += fmt.Sprintf(" · %d turns", cp.AssistantTurns)
			}
			if cp.WallSeconds > 0 {
				cpline += fmt.Sprintf(" · %ds", cp.WallSeconds)
			}
			if !cp.UpdatedAt.IsZero() {
				cpline += " · updated " + stamp(now, cp.UpdatedAt)
			}
			lines = append(lines, cpline)
		}
	}
	text := func(heading, body, missing string) {
		lines = append(lines, "", styleHeading.Render(heading))
		if body == "" {
			lines = append(lines, "  "+styleFaint.Render(missing))
			return
		}
		for _, line := range wrap(body, w-4) {
			lines = append(lines, "  "+line)
		}
	}
	missing := func(name string) string {
		for _, f := range t.Missing {
			if f == name {
				return "not reported by this daemon (it has no TaskGet)"
			}
		}
		return "(none)"
	}
	description := t.Content
	if t.ContentOmitted {
		description = fmt.Sprintf("(a description of %d bytes, too large to show here)", t.ContentBytes)
	}
	text("Description", description, missing("description"))
	acceptance := t.Acceptance
	if acceptance == "" && t.AcceptanceDigest != "" {
		acceptance = "digest " + t.AcceptanceDigest
		if t.Content != "" {
			acceptance = "the description above is the acceptance text (" + acceptance + ")"
		}
	}
	text("Acceptance", acceptance, missing("acceptance"))
	if len(t.DeclaredFiles) > 0 || t.TestFilter != "" || t.TestShape != "" || t.SupersededFields != nil || len(t.ReadOnlyFiles) > 0 || t.Reads != nil || len(t.ApprovedScope) > 0 || len(t.DenyPaths) > 0 || t.PendingRequest != nil {
		lines = append(lines, "", styleHeading.Render("Structured fields"))
		if len(t.DeclaredFiles) > 0 {
			lines = append(lines, "  declared files  "+strings.Join(t.DeclaredFiles, ", "))
		}
		if len(t.ReadOnlyFiles) > 0 {
			lines = append(lines, "  Read-only files: "+strings.Join(t.ReadOnlyFiles, ", "))
		}
		if r := t.Reads; r != nil {
			if len(r.Allow) > 0 {
				lines = append(lines, "  Reads allow: "+strings.Join(r.Allow, ", "))
			}
			if len(r.Deny) > 0 {
				lines = append(lines, "  Reads deny: "+strings.Join(r.Deny, ", "))
			}
		}
		if len(t.DenyPaths) > 0 {
			lines = append(lines, "  Deny paths: "+strings.Join(t.DenyPaths, ", "))
		}
		if len(t.ApprovedScope) > 0 {
			lines = append(lines, "  Approved widened scope: "+strings.Join(t.ApprovedScope, ", "))
		}
		if pr := t.PendingRequest; pr != nil {
			paths := append([]string(nil), pr.Paths...)
			paths = append(paths, pr.Files...)
			if pr.TestFile != "" {
				paths = append(paths, pr.TestFile)
			}
			reqLine := "  Pending request: " + pr.Kind
			if len(paths) > 0 {
				reqLine += " (" + strings.Join(paths, ", ") + ")"
			}
			if pr.Reason != "" {
				reqLine += " — " + pr.Reason
			}
			lines = append(lines, styleWarn.Render(reqLine))
		}
		if t.TestFilter != "" {
			lines = append(lines, "  test filter     "+t.TestFilter)
		}
		if t.TestShape != "" {
			lines = append(lines, "  test shape      "+t.TestShape)
		}
		if s := t.SupersededFields; s != nil {
			lines = append(lines, "  "+styleWarn.Render("turn in flight runs under (edits above apply from the next dispatch):"))
			files := "(none)"
			if len(s.DeclaredFiles) > 0 {
				files = strings.Join(s.DeclaredFiles, ", ")
			}
			filter := "(none)"
			if s.TestFilter != "" {
				filter = s.TestFilter
			}
			shape := "(none)"
			if s.TestShape != "" {
				shape = s.TestShape
			}
			lines = append(lines,
				"    declared files  "+files,
				"    test filter     "+filter,
				"    test shape      "+shape,
			)
		}
	}
	if len(t.Notifications) > 0 {
		lines = append(lines, "", styleHeading.Render(fmt.Sprintf("Notifications (%d)", len(t.Notifications))))
		for _, n := range t.Notifications {
			nline := fmt.Sprintf("  %s (%s): %s", n.Kind, n.Delivery, n.Summary)
			if n.FromTask != "" {
				nline += " [from " + n.FromTask + "]"
			}
			if !n.DeliveredAt.IsZero() {
				nline += " · delivered " + stamp(now, n.DeliveredAt)
			}
			lines = append(lines, nline)
		}
	}
	dead := m.deadEdges()
	snap := m.snapWithDetail(t)
	edges := func(heading, arrow string, refs []client.EdgeRef) {
		lines = append(lines, "", styleHeading.Render(fmt.Sprintf("%s (%d)", heading, len(refs))))
		if len(refs) == 0 {
			lines = append(lines, "  "+styleFaint.Render("none"))
		}
		for _, r := range refs {
			s := snap.stateOf(r.Other)
			if r.OtherState != "" && r.OtherState != "unknown" {
				if _, onBoard := snap.tasks[r.Other]; !onBoard || r.OtherState == "Done" {
					s = implState{r.OtherState, r.OtherState, classOf(r.OtherState)}
				}
			}
			line := fmt.Sprintf("  %s %-28s %-9s %s", arrow, r.Other, r.Kind, s.render(s.glyph()+" "+s.Long))
			if r.Satisfied != nil {
				line += styleFaint.Render(map[bool]string{true: "  releases it", false: "  holds it"}[*r.Satisfied])
			}
			if r.Dead || (arrow == "⇐" && dead[client.Edge{From: r.Other, To: t.ID, Kind: r.Kind}]) {
				line += styleError.Render("  ✗ dead edge: this prerequisite can no longer release it")
			}
			lines = append(lines, line)
		}
	}
	edges("Edges in: prerequisites", "⇐", t.EdgesIn)
	edges("Edges out: dependents", "⇒", t.EdgesOut)
	lines = append(lines, m.traceLines(t, now)...)
	if len(t.Messages) > 0 {
		lines = append(lines, "", styleHeading.Render(fmt.Sprintf("Operator messages (%d)", len(t.Messages))))
		for _, msg := range t.Messages {
			first := strings.SplitN(msg.Text, "\n", 2)[0]
			lines = append(lines, fmt.Sprintf("  %s %s: %s", stamp(now, msg.Created), msg.Author, first))
		}
	}
	lines = append(lines, "")
	switch {
	case t.Source == "TaskHistory":
		for _, l := range wrap("Read with Show and TaskHistory: this daemon has no TaskGet, so the trace is rebuilt from the transition log and "+strings.Join(t.Missing, ", ")+" are not available.", w) {
			lines = append(lines, styleFaint.Render(l))
		}
	case d.loading:
		lines = append(lines, styleFaint.Render("refreshing…"))
	}
	return lines
}

// snapWithDetail returns a snapshot whose Board task map includes t's own
// state from TaskGet, so dependent and prerequisite state labels in the task
// detail view reflect t's latest read even before BacklogShow is polled.
func (m *Model) snapWithDetail(t client.TaskDetail) snapshot {
	s := m.snap
	if t.ID == "" || t.State == "" {
		return s
	}
	tasks := maps(s.tasks)
	row := tasks[t.ID]
	row.ID, row.State, row.InFlight, row.Reason = t.ID, t.State, t.InFlight, t.Reason
	if t.EscalationKind != "" {
		row.EscalationKind = t.EscalationKind
	}
	if t.PendingDelete != nil {
		row.PendingDelete = true
	}
	if t.Parent != nil {
		row.Parent = t.Parent
	}
	if t.Child != nil {
		row.Child = t.Child
	}
	tasks[t.ID] = row
	s.tasks = tasks
	return s
}

// detailStateOf derives the open task's implementation state from its
// TaskGet read and the Board, using TaskGet's own incoming edges when the
// task is Pending so opening a task outside the Graph view never shows a
// stale BacklogShow withheld label.
func (m *Model) detailStateOf(t client.TaskDetail) implState {
	snap := m.snapWithDetail(t)
	if t.State == "Pending" && t.Source != "TaskHistory" && !t.Deferred && t.PendingDelete == nil && !snap.tasks[t.ID].PendingDelete {
		reason := ""
		if snap.status.Snapshot != nil {
			reason = snap.status.Snapshot.Scheduler.WithheldTasks[t.ID]
		}
		if len(t.EdgesIn) > 0 {
			withheld := []string{}
			for _, r := range t.EdgesIn {
				if r.Kind != "blocking" {
					continue
				}
				otherState := r.OtherState
				if pt, onBoard := snap.tasks[r.Other]; onBoard && pt.State != "" && otherState != "Done" {
					otherState = pt.State
				}
				satisfied := (r.Satisfied != nil && *r.Satisfied) || otherState == "Done"
				if !satisfied {
					if otherState == "" {
						otherState = "unknown"
					}
					withheld = append(withheld, r.Other+": "+otherState)
				}
			}
			if len(withheld) > 0 {
				return implState{"withheld", "withheld by " + strings.Join(withheld, "; "), classWaiting}
			}
			if reason != "" {
				return implState{"withheld", "withheld: " + reason, classWaiting}
			}
			return implState{"ready", "Pending, ready", classReady}
		}
		if t.Readiness != "" {
			if strings.HasPrefix(t.Readiness, "withheld") {
				return implState{"withheld", t.Readiness, classWaiting}
			}
			if reason != "" {
				return implState{"withheld", "withheld: " + reason, classWaiting}
			}
			if t.Readiness == "ready" {
				return implState{"ready", "Pending, ready", classReady}
			}
			return implState{t.Readiness, t.Readiness, classWaiting}
		}
		if t.Source == "TaskGet" || !slices.Contains(t.Missing, "edges") {
			if reason != "" {
				return implState{"withheld", "withheld: " + reason, classWaiting}
			}
			return implState{"ready", "Pending, ready", classReady}
		}
	}
	state := snap.stateOf(t.ID)
	if _, onBoard := m.snap.tasks[t.ID]; !onBoard && t.State != "" && state.Class == classUnknown {
		state = implState{t.State, t.State, classOf(t.State)}
	}
	return state
}

func (m *Model) isUncertainDelete(t client.TaskDetail) bool {
	if t.PendingDelete != nil && (t.PendingDelete.Status == "uncertain" || t.PendingDelete.UncertainDispatch) {
		return true
	}
	if t.Reason == "UncertainDispatch" || t.EscalationReason == "UncertainDispatch" || t.EscalationKind == "UncertainDispatch" {
		return true
	}
	return m.isUncertainTask(t.ID)
}

// traceLines renders the stages the task went through with its edits
// between them, oldest first.
func (m *Model) traceLines(t client.TaskDetail, now time.Time) []string {
	lines := []string{"", styleHeading.Render(fmt.Sprintf("Trace: %s, %s", plural(len(t.Trace), "stage"), plural(len(t.Edits), "edit")))}
	if len(t.Trace) == 0 && len(t.Edits) == 0 {
		return append(lines, "  "+styleFaint.Render("no stage recorded yet"))
	}
	if t.TraceOmitted > 0 {
		lines = append(lines, styleFaint.Render(fmt.Sprintf("  (the %s are left out to fit the read limit)", plural(t.TraceOmitted, "oldest visit"))))
	}
	if t.TraceTruncated {
		lines = append(lines, styleWarn.Render("  (the task has more transitions than the daemon reads)"))
	}
	if t.EditsTruncated {
		lines = append(lines, styleWarn.Render("  (older edits not shown: the task has more edits than TaskGet returns)"))
	}
	lines = append(lines, styleFaint.Render(fmt.Sprintf("  %3s  %-20s %-14s %-14s %-9s %-7s %s", "#", "stage", "entered", "left", "took", "attempt", "outcome")))
	edits := append([]client.Edit(nil), t.Edits...)
	sort.SliceStable(edits, func(i, j int) bool { return edits[i].At.Before(edits[j].At) })
	editLine := func(e client.Edit) string {
		what := editText(e)
		if e.Effect != "" {
			what += " (" + e.Effect + ")"
		}
		who := ""
		if e.Message != "" {
			who = ", message " + e.Message
		}
		return styleWarn.Render(fmt.Sprintf("    ✎  %s at %s%s", what, stamp(now, e.At), who))
	}
	// Each edit goes before the first stage entered after it.
	next := 0
	for i, e := range t.Trace {
		for next < len(edits) && !e.EnteredAt.IsZero() && edits[next].At.Before(e.EnteredAt) {
			lines = append(lines, editLine(edits[next]))
			next++
		}
		left, took := "—", ""
		if e.LeftAt != nil {
			left = stamp(now, *e.LeftAt)
			if !e.EnteredAt.IsZero() {
				took = age(e.LeftAt.Sub(e.EnteredAt))
			}
		} else if !e.EnteredAt.IsZero() {
			took = age(now.Sub(e.EnteredAt)) + "…"
		}
		attempt := ""
		if e.Attempt > 0 {
			attempt = fmt.Sprint(e.Attempt)
		}
		s := implState{Class: classOf(e.Stage)}
		outcome := e.Outcome
		if e.To != "" {
			outcome = strings.TrimSpace(outcome + " → " + e.To)
		}
		lines = append(lines, fmt.Sprintf("  %3d  %s %-14s %-14s %-9s %-7s %s", i+1, s.render(fit(e.Stage, 20)), stamp(now, e.EnteredAt), left, took, attempt, outcome))
	}
	for ; next < len(edits); next++ {
		lines = append(lines, editLine(edits[next]))
	}
	return lines
}

// classOf colours a bare state or stage name.
func classOf(state string) stateClass {
	switch state {
	case "Done":
		return classDone
	case "Cancelled", "Retired":
		return classClosed
	case "Escalated":
		return classEscalated
	case "Blocked":
		return classBlocked
	case "Pending":
		return classReady
	case "", "unknown":
		return classUnknown
	}
	return classStage
}

// plural counts a noun: "1 edit", "3 edits".
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// editText describes one recorded change of a task.
func editText(e client.Edit) string {
	var d struct {
		From       string `json:"from"`
		To         string `json:"to"`
		Kind       string `json:"kind"`
		Removed    bool   `json:"removed"`
		Action     string `json:"action"`
		Retirement string `json:"retirement"`
	}
	_ = json.Unmarshal(e.Detail, &d)
	switch {
	case e.Kind == "backlog_edge_changed" && d.From != "":
		verb := "added"
		if d.Removed {
			verb = "removed"
		}
		return fmt.Sprintf("edge %s %s → %s (%s)", verb, d.From, d.To, d.Kind)
	case e.Kind == "backlog_edge_changed":
		return "an edge changed"
	case e.Kind == "task-deleted":
		return strings.TrimSpace(fmt.Sprintf("deleted: %s %s", d.Action, d.Retirement))
	case strings.HasPrefix(e.Kind, "task-delete"):
		return strings.ReplaceAll(e.Kind, "-", " ")
	case len(e.Fields) > 0:
		return "edited " + strings.Join(e.Fields, ", ")
	}
	return firstNonEmpty(e.Kind, "edited")
}
