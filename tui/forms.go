package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/xleelz/fae/tui/client"
)

// modal is a dialog that takes every key while it is open. view returns
// its lines for an inner width and the line to keep in sight.
type modal interface {
	update(m *Model, msg tea.Msg) tea.Cmd
	view(width int) (lines []string, focus int)
	keys() [][2]string
}

// boxed frames a dialog's lines, scrolled to keep its focus line in sight
// when it is taller than the room.
func boxed(lines []string, focus, width, height int) []string {
	w := modalWidth(width)
	room := max(height-2, 3)
	if len(lines) > room {
		offset := max(0, min(focus-room/2, len(lines)-room))
		shown := append([]string(nil), lines[offset:offset+room]...)
		if offset > 0 {
			shown[0] = styleFaint.Render(fmt.Sprintf("↑ %d more lines", offset))
		}
		if rest := len(lines) - offset - room; rest > 0 {
			shown[room-1] = styleFaint.Render(fmt.Sprintf("↓ %d more lines", rest))
		}
		lines = shown
	}
	for i := range lines {
		lines[i] = clip(lines[i], w)
	}
	return strings.Split(styleBox.Render(strings.Join(lines, "\n")), "\n")
}

// modalWidth is a dialog's inner width.
func modalWidth(width int) int { return max(min(width-4, 116), 26) }

// field is one form input: a line, or a text area for long text.
type field struct {
	key, label, hint string
	multiline        bool
	input            textinput.Model
	area             textarea.Model
}

func newField(key, label, hint, value string) *field {
	f := &field{key: key, label: label, hint: hint, input: textinput.New()}
	f.input.Prompt = ""
	f.input.Placeholder = hint
	f.input.SetValue(value)
	return f
}

func newArea(key, label, hint, value string) *field {
	f := &field{key: key, label: label, hint: hint, multiline: true, area: textarea.New()}
	f.area.Placeholder = hint
	f.area.ShowLineNumbers = false
	f.area.SetHeight(5)
	f.area.SetValue(value)
	return f
}

func (f *field) value() string {
	if f.multiline {
		return f.area.Value()
	}
	return f.input.Value()
}

func (f *field) focus() tea.Cmd {
	if f.multiline {
		return f.area.Focus()
	}
	return f.input.Focus()
}

func (f *field) blur() {
	if f.multiline {
		f.area.Blur()
	} else {
		f.input.Blur()
	}
}

func (f *field) update(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	if f.multiline {
		f.area, cmd = f.area.Update(msg)
	} else {
		f.input, cmd = f.input.Update(msg)
	}
	return cmd
}

// form is a modal list of fields with a submit action.
type form struct {
	title  string
	notes  []string
	fields []*field
	focus  int
	err    string
	submit func(m *Model, f *form) tea.Cmd
}

func newForm(title string, notes []string, fields []*field, submit func(*Model, *form) tea.Cmd) (*form, tea.Cmd) {
	f := &form{title: title, notes: notes, fields: fields, submit: submit}
	return f, f.fields[0].focus()
}

func (f *form) get(key string) string {
	for _, fl := range f.fields {
		if fl.key == key {
			return strings.TrimSpace(fl.value())
		}
	}
	return ""
}

func (f *form) resize(width int) {
	w := max(modalWidth(width)-22, 8)
	for _, fl := range f.fields {
		if fl.multiline {
			fl.area.SetWidth(w)
		} else {
			fl.input.SetWidth(w)
		}
	}
}

func (f *form) move(delta int) tea.Cmd {
	f.fields[f.focus].blur()
	f.focus = (f.focus + delta + len(f.fields)) % len(f.fields)
	return f.fields[f.focus].focus()
}

func (f *form) update(m *Model, msg tea.Msg) tea.Cmd {
	k, isKey := msg.(tea.KeyPressMsg)
	if !isKey {
		return f.fields[f.focus].update(msg)
	}
	current := f.fields[f.focus]
	switch k.String() {
	case "esc":
		m.modal = nil
		m.say("cancelled", false)
		return nil
	case "ctrl+s":
		return f.submit(m, f)
	case "tab":
		return f.move(1)
	case "shift+tab":
		return f.move(-1)
	case "down":
		if !current.multiline {
			return f.move(1)
		}
	case "up":
		if !current.multiline {
			return f.move(-1)
		}
	case "enter":
		if !current.multiline {
			if f.focus == len(f.fields)-1 {
				return f.submit(m, f)
			}
			return f.move(1)
		}
	}
	return current.update(k)
}

func (f *form) view(width int) ([]string, int) {
	w := modalWidth(width)
	f.resize(width)
	lines := []string{styleBold.Render(f.title)}
	for _, n := range f.notes {
		lines = append(lines, wrap(n, w)...)
	}
	lines = append(lines, "")
	focus := 0
	for i, fl := range f.fields {
		label := fmt.Sprintf("  %-16s", fl.label)
		if i == f.focus {
			label = styleKey.Render(fmt.Sprintf("› %-16s", fl.label))
			focus = len(lines)
		}
		if fl.multiline {
			lines = append(lines, label)
			for _, l := range strings.Split(fl.area.View(), "\n") {
				lines = append(lines, "    "+l)
			}
			continue
		}
		lines = append(lines, label+" "+fl.input.View())
	}
	if f.err != "" {
		lines = append(lines, "", styleError.Render(f.err))
		focus = len(lines) - 1
	}
	return lines, focus
}

func (f *form) keys() [][2]string {
	return [][2]string{{"tab/↑↓", "field"}, {"enter", "next (submits on the last)"}, {"ctrl+s", "submit"}, {"esc", "cancel"}}
}

// confirm asks yes or no before a write.
type confirm struct {
	title string
	lines []string
	yes   func(m *Model) tea.Cmd
}

func (c *confirm) update(m *Model, msg tea.Msg) tea.Cmd {
	k, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return nil
	}
	switch k.String() {
	case "y", "Y":
		return c.yes(m)
	case "n", "N", "esc":
		m.modal = nil
		m.say("cancelled", false)
	}
	return nil
}

func (c *confirm) view(width int) ([]string, int) {
	lines := []string{styleBold.Render(c.title), ""}
	for _, l := range c.lines {
		lines = append(lines, wrap(l, modalWidth(width))...)
	}
	lines = append(lines, "", styleKey.Render("y")+" confirm · "+styleKey.Render("n")+" or "+styleKey.Render("esc")+" cancel")
	return lines, len(lines) - 1
}

func (c *confirm) keys() [][2]string { return [][2]string{{"y", "confirm"}, {"n/esc", "cancel"}} }

// choice picks one of several options.
type choice struct {
	title   string
	options []string
	cursor  int
	pick    func(m *Model, i int) tea.Cmd
}

func (c *choice) update(m *Model, msg tea.Msg) tea.Cmd {
	k, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return nil
	}
	switch k.String() {
	case "esc":
		m.modal = nil
		return nil
	case "enter":
		return c.pick(m, c.cursor)
	}
	if delta, ok := moveKey(k.String(), 5); ok {
		c.cursor = selectable(c.cursor+delta, len(c.options))
	}
	return nil
}

func (c *choice) view(width int) ([]string, int) {
	lines := []string{styleBold.Render(c.title), ""}
	for i, o := range c.options {
		if i == c.cursor {
			o = styleSelected.Render("› " + o)
		} else {
			o = "  " + o
		}
		lines = append(lines, o)
	}
	return lines, 2 + c.cursor
}

func (c *choice) keys() [][2]string {
	return [][2]string{{"↑↓", "move"}, {"enter", "choose"}, {"esc", "cancel"}}
}

// result shows a write's outcome or its refusal text.
type result struct{ msg writeMsg }

func newResult(msg writeMsg) *result { return &result{msg} }

func (r *result) update(m *Model, msg tea.Msg) tea.Cmd {
	if k, ok := msg.(tea.KeyPressMsg); ok {
		switch k.String() {
		case "enter", "esc", "space", "q":
			m.modal = nil
		}
	}
	return nil
}

func (r *result) view(width int) ([]string, int) {
	w := modalWidth(width)
	lines := []string{}
	if r.msg.err != nil {
		lines = append(lines, styleError.Render(r.msg.verb+" refused"), "")
		lines = append(lines, wrap(r.msg.err.Error(), w)...)
	} else {
		lines = append(lines, styleOK.Render(r.msg.verb+" done"), "")
		for _, l := range r.msg.lines {
			lines = append(lines, wrap(l, w)...)
		}
	}
	if r.msg.message != "" {
		lines = append(lines, "", styleFaint.Render("authorizing message "+r.msg.message))
	}
	lines = append(lines, "", styleFaint.Render("enter or esc closes"))
	return lines, 0
}

func (r *result) keys() [][2]string { return [][2]string{{"enter/esc", "close"}} }

// newFind asks for part of a task id to select in the graph.
func newFind() *form {
	f, _ := newForm("Find a task", nil, []*field{newField("text", "Task id contains", "part of an id", "")}, func(m *Model, f *form) tea.Cmd {
		m.modal = nil
		m.find(f.get("text"))
		return nil
	})
	return f
}

// splitList splits comma, space or newline separated values.
func splitList(s string) []string {
	out := []string{}
	for _, v := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '\n' || r == ' ' || r == '\t' }) {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// note appends the operator's own words to an authorizing message.
func note(summary, words string) string {
	if words = strings.TrimSpace(words); words != "" {
		return summary + " Operator note: " + words
	}
	return summary
}

func (m *Model) startCreate() tea.Cmd {
	if !m.writable("create tasks") {
		return nil
	}
	fields := []*field{
		newField("id", "ID", "a new task id", ""),
		newField("title", "Title", "a short title", ""),
		newArea("content", "Description", "what to do and how to accept it (the acceptance text)", ""),
		newField("priority", "Priority", "10 or more; higher runs first", "10"),
		newField("source_ref", "Source", "optional: a ticket or document reference", ""),
		newField("write_class", "Write class", "optional", ""),
		newField("declared_files", "Declared files", "optional: paths the work may change, comma-separated", ""),
		newField("test_filter", "Test filter", "optional: go test -run regular expression (per-/ level)", ""),
		newField("test_shape", "Test shape", "optional: go-unit, go-integration, rust-unit, python-pytest, node-test, script or runner (recorded; not yet consumed)", ""),
		newField("workflow", "Workflow", "optional: workflow id (default: the instance default)", ""),
		newField("revision", "Revision", "optional: pinned workflow revision (default: the latest)", ""),
		newField("target", "Target", "optional: target configuration id", ""),
		newField("target_revision", "Target rev", "optional: target revision (default: the latest)", ""),
		newField("task_class", "Task class", "optional: a task class the workflow declares", ""),
		newField("mode", "Mode", "optional: local or normal (default: the instance default)", ""),
		newField("disposition", "Disposition", "normal mode only: dry-run or commit-external (needs approval phrase in Note)", ""),
		newField("blocked_by", "Blocked by", "optional: prerequisite task ids (blocking edges into it)", ""),
		newField("blocks", "Blocks", "optional: dependent task ids (blocking edges out of it)", ""),
		newField("relates", "Relates to", "optional: related task ids (relates edges)", ""),
		newField("note", "Note", "optional: why, recorded in the authorizing message (commit-external requires: I approve live external effects)", ""),
		newField("turn_wall_seconds", "Wall seconds", "optional: turn wall clock budget in seconds (FAE-RT-015)", ""),
		newField("turn_max_turns", "Max turns", "optional: max assistant turns per stage (FAE-RT-015)", ""),
		newField("read_only_files", "Read-only files", "optional: read-only reference files, comma-separated (FAE-SEC-008)", ""),
		newField("reads_allow", "Reads allow", "optional: allowed external read roots/globs, comma-separated (FAE-SEC-008)", ""),
		newField("reads_deny", "Reads deny", "optional: denied read paths/globs, comma-separated (FAE-SEC-008)", ""),
	}
	f, cmd := newForm("Create a task", []string{"The new task joins the live backlog graph. Create refuses an id that already exists.", "A normal-mode task states exactly one disposition: dry-run records intended external effects, commit-external pins live ones (Note must include \"I approve live external effects\")."}, fields, submitCreate)
	f.resize(m.width)
	m.modal = f
	return cmd
}

func priorityOf(s string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 10 {
		return 0, fmt.Errorf("priority must be a whole number of at least 10")
	}
	return n, nil
}

// revisionOf parses an optional pinned revision: empty means the latest.
func revisionOf(what, s string) (int64, error) {
	if s = strings.TrimSpace(s); s == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("the %s is a whole number (empty: the latest)", what)
	}
	return n, nil
}

// modeOf checks the mode and disposition fields before daemon contact
// (FAE-MODE-001: a normal task states exactly one disposition), and returns
// the mode the daemon takes: "Local", "Normal" or "" for the instance default.
func modeOf(mode, disposition string) (string, bool, bool, error) {
	mode, disposition = strings.ToLower(strings.TrimSpace(mode)), strings.ToLower(strings.TrimSpace(disposition))
	dryRun, commitExternal := disposition == "dry-run", disposition == "commit-external"
	if disposition != "" && !dryRun && !commitExternal {
		return "", false, false, fmt.Errorf("the disposition is dry-run or commit-external")
	}
	if mode == "" && (dryRun || commitExternal) {
		mode = "normal"
	}
	if mode != "" && mode != "local" && mode != "normal" {
		return "", false, false, fmt.Errorf("mode must be local or normal")
	}
	if mode == "local" && (dryRun || commitExternal) {
		return "", false, false, fmt.Errorf("dry-run and commit-external require mode normal")
	}
	if !client.ModeComplete(mode == "normal", dryRun, commitExternal) {
		return "", false, false, fmt.Errorf("mode normal requires exactly one of dry-run or commit-external")
	}
	return map[string]string{"local": "Local", "normal": "Normal"}[mode], dryRun, commitExternal, nil
}

func submitCreate(m *Model, f *form) tea.Cmd {
	id := f.get("id")
	if id == "" || strings.ContainsAny(id, " \t\n") {
		f.err = "the id is required and has no spaces"
		return nil
	}
	if f.get("content") == "" {
		f.err = "the description is required"
		return nil
	}
	priority, err := priorityOf(f.get("priority"))
	if err != nil {
		f.err = err.Error()
		return nil
	}
	revision, err := revisionOf("workflow revision", f.get("revision"))
	if err != nil {
		f.err = err.Error()
		return nil
	}
	targetRevision, err := revisionOf("target revision", f.get("target_revision"))
	if err != nil {
		f.err = err.Error()
		return nil
	}
	if targetRevision > 0 && f.get("target") == "" {
		f.err = "a target revision needs the target id"
		return nil
	}
	mode, dryRun, commitExternal, err := modeOf(f.get("mode"), f.get("disposition"))
	if err != nil {
		f.err = err.Error()
		return nil
	}
	if commitExternal && !strings.Contains(f.get("note"), "I approve live external effects") {
		f.err = `commit-external requires "I approve live external effects" in the note`
		return nil
	}
	wallSeconds, err := revisionOf("turn wall seconds", f.get("turn_wall_seconds"))
	if err != nil {
		f.err = err.Error()
		return nil
	}
	maxTurns, err := revisionOf("max turns", f.get("turn_max_turns"))
	if err != nil {
		f.err = err.Error()
		return nil
	}
	p := client.TaskCreateParams{TaskFields: client.TaskFields{
		ID: id, Title: f.get("title"), Content: f.get("content"), Priority: priority,
		SourceRef: f.get("source_ref"), WriteClass: f.get("write_class"), DeclaredFiles: splitList(f.get("declared_files")),
		TestFilter: f.get("test_filter"), TestShape: f.get("test_shape"), WorkflowID: f.get("workflow"), Revision: revision,
		TargetConfig: f.get("target"), TargetRevision: targetRevision, TaskClass: f.get("task_class"),
		Mode: mode, DryRun: dryRun, CommitExternal: commitExternal,
	}}
	for _, other := range splitList(f.get("blocked_by")) {
		p.Edges = append(p.Edges, client.Edge{From: other, To: id, Kind: "blocking"})
	}
	for _, other := range splitList(f.get("blocks")) {
		p.Edges = append(p.Edges, client.Edge{From: id, To: other, Kind: "blocking"})
	}
	for _, other := range splitList(f.get("relates")) {
		p.Edges = append(p.Edges, client.Edge{From: id, To: other, Kind: "relates"})
	}
	for _, e := range p.Edges {
		if e.From == e.To {
			f.err = "a task cannot depend on itself"
			return nil
		}
	}
	summary := fmt.Sprintf("fae-tui: create task %s", id)
	if p.Title != "" {
		summary += fmt.Sprintf(" (%s)", p.Title)
	}
	switch {
	case p.Mode == "Normal":
		summary += " in normal mode, " + map[bool]string{true: "dry-run", false: "commit-external"}[p.DryRun]
	case p.Mode != "":
		summary += " in " + strings.ToLower(p.Mode) + " mode"
	}
	if len(p.Edges) > 0 {
		summary += fmt.Sprintf(" with %d edges", len(p.Edges))
	}
	if wallSeconds > 0 || maxTurns > 0 {
		summary += fmt.Sprintf(" (budget wall=%ds turns=%d)", wallSeconds, maxTurns)
	}
	if ro := splitList(f.get("read_only_files")); len(ro) > 0 {
		summary += fmt.Sprintf(" (read_only_files=%s)", strings.Join(ro, ","))
	}
	if ra := splitList(f.get("reads_allow")); len(ra) > 0 {
		summary += fmt.Sprintf(" (reads.allow=%s)", strings.Join(ra, ","))
	}
	if rd := splitList(f.get("reads_deny")); len(rd) > 0 {
		summary += fmt.Sprintf(" (reads.deny=%s)", strings.Join(rd, ","))
	}
	var send func(accept bool) tea.Cmd
	send = func(accept bool) tea.Cmd {
		p.AcceptInFlight = accept
		text := summary + "."
		if accept {
			text += " The operator accepts its edges to dependents already dispatched."
		}
		var retry func(*Model) tea.Cmd
		if !accept {
			retry = func(*Model) tea.Cmd { return send(true) }
		}
		return m.runWrite(writeAction{verb: "TaskCreate", summary: note(text, f.get("note")), class: "task", accept: retry,
			run: func(ctx context.Context, d Daemon, message string) ([]string, error) {
				p.AuthorizingMessage = message
				r, err := d.TaskCreate(ctx, p)
				if err != nil {
					return nil, err
				}
				lines := []string{fmt.Sprintf("created %s, state %s", r.ID, r.State)}
				if len(r.EdgesAdded) > 0 && string(r.EdgesAdded) != "null" {
					lines = append(lines, "edges added: "+compact(r.EdgesAdded))
				}
				if len(r.Readiness) > 0 && string(r.Readiness) != "null" {
					lines = append(lines, "readiness: "+compact(r.Readiness))
				}
				return lines, nil
			}})
	}
	if dispatched := m.dispatchedDependents(p.Edges, id); len(dispatched) > 0 {
		m.modal = &confirm{title: "Dependents already dispatched", lines: []string{
			fmt.Sprintf("%s already left Pending. An edge from %s to a dispatched task sets a condition for its next release; it does not interrupt a turn in flight.", joinIDs(dispatched), id),
			"Accept these edges in flight?",
		}, yes: func(m *Model) tea.Cmd { return send(true) }}
		return nil
	}
	return send(false)
}

// dispatchedDependents lists the dependents of from's new edges that have
// left Pending: the daemon refuses an edge of either kind to an already
// dispatched dependent unless the operator accepts it in flight
// (BACKLOG.md). A Pending task that ran before is caught by the daemon's
// refusal, which offers the same acceptance.
func (m *Model) dispatchedDependents(edges []client.Edge, from string) []string {
	out := []string{}
	for _, e := range edges {
		if t, ok := m.snap.tasks[e.To]; ok && e.From == from && t.State != "Pending" {
			out = append(out, e.To)
		}
	}
	return out
}

// needsAcceptance reports the daemon's refusal of an edge to an already
// dispatched dependent.
func needsAcceptance(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "accept-in-flight") || strings.Contains(err.Error(), "accept_in_flight"))
}

// compact renders a JSON value on one line.
func compact(raw json.RawMessage) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return string(raw)
	}
	out, _ := json.Marshal(v)
	return string(out)
}

// running reports a task that has started and not finished: edits reach it
// from its next stage.
func running(state string, inFlight bool) bool {
	return inFlight || (!kernel(state) && state != "")
}

// editTarget returns the open task's detail, or reads it first; the edit
// starts again when it arrives.
func (m *Model) editTarget(id, action string) (*client.TaskDetail, tea.Cmd) {
	if id == "" {
		m.say("select a task to "+action, true)
		return nil, nil
	}
	if t, ok := m.snap.tasks[id]; ok && terminal(t.State) {
		m.say(fmt.Sprintf("task %s is %s: a terminal task cannot be changed", id, t.State), true)
		return nil, nil
	}
	if m.detail != nil && m.detail.id == id && m.detail.data != nil {
		return m.detail.data, nil
	}
	m.pending = action
	m.say("reading task "+id+"…", false)
	return nil, m.openDetail(id)
}

func (m *Model) startUpdate(id string) tea.Cmd {
	if !m.writable("edit tasks") {
		return nil
	}
	t, cmd := m.editTarget(id, "edit")
	if t == nil {
		return cmd
	}
	if terminal(t.State) {
		m.say(fmt.Sprintf("task %s is %s: a terminal task cannot be edited", id, t.State), true)
		return nil
	}
	notes := []string{"Only the fields you change are sent. The pinned workflow, target and disposition cannot change."}
	if running(t.State, t.InFlight) {
		notes = append(notes, fmt.Sprintf("Task %s is running (in %s): edits apply from its next stage; the turn in flight keeps the text it was dispatched with.", id, firstNonEmpty(t.Stage, t.State)))
		if t.InFlight {
			notes = append(notes, "Its turn in flight keeps the declared files, test filter and test shape it was dispatched under; a change applies from the next dispatch.")
		}
	} else {
		notes = append(notes, "The task is not running: edits apply at once, and its next stage uses them.")
	}
	if t.Source == "TaskHistory" {
		notes = append(notes, "This daemon has no TaskGet, so the current title, description and structured fields are unknown here; fields left empty are not sent.")
	}
	before := *t
	carried := m.pendingEdits
	m.pendingEdits = nil
	val := func(key, base string) string {
		if v, ok := carried[key]; ok {
			return v
		}
		return base
	}
	fields := []*field{
		newField("title", "Title", "", val("title", t.Title)),
		newArea("content", "Description", "the acceptance text", val("content", t.Content)),
		newField("priority", "Priority", "10 or more", val("priority", strconv.Itoa(t.Priority))),
		newField("source_ref", "Source", "", val("source_ref", t.SourceRef)),
		newField("write_class", "Write class", "", val("write_class", t.WriteClass)),
		newField("declared_files", "Declared files", "comma-separated paths", val("declared_files", strings.Join(t.DeclaredFiles, ", "))),
		newField("test_filter", "Test filter", "go test -run regular expression (per-/ level)", val("test_filter", t.TestFilter)),
		newField("test_shape", "Test shape", "go-unit, go-integration, rust-unit, python-pytest, node-test, script or runner", val("test_shape", t.TestShape)),
		newField("note", "Note", "optional: why, recorded in the authorizing message", val("note", "")),
	}
	f, cmd := newForm("Edit task "+id, notes, fields, func(m *Model, f *form) tea.Cmd { return submitUpdate(m, f, before) })
	f.resize(m.width)
	m.modal = f
	m.say("", false)
	return cmd
}

func submitUpdate(m *Model, f *form, before client.TaskDetail) tea.Cmd {
	// The version the form was filled from: the daemon refuses the edit if
	// the task moved on since, and the TUI then offers to reload it.
	p := client.TaskUpdateParams{ID: before.ID, ExpectedVersion: before.Version}
	edits := map[string]string{}
	text := func(key, was string) *string {
		v := f.get(key)
		if v == strings.TrimSpace(was) {
			return nil
		}
		edits[key] = v
		return &v
	}
	p.Title, p.Content = text("title", before.Title), text("content", before.Content)
	p.SourceRef, p.WriteClass = text("source_ref", before.SourceRef), text("write_class", before.WriteClass)
	p.TestFilter, p.TestShape = text("test_filter", before.TestFilter), text("test_shape", before.TestShape)
	if p.Content != nil && *p.Content == "" {
		f.err = "the description cannot be empty"
		return nil
	}
	if f.get("priority") != strconv.Itoa(before.Priority) {
		n, err := priorityOf(f.get("priority"))
		if err != nil {
			f.err = err.Error()
			return nil
		}
		edits["priority"] = strconv.Itoa(n)
		p.Priority = &n
	}
	if files := splitList(f.get("declared_files")); strings.Join(files, ",") != strings.Join(before.DeclaredFiles, ",") {
		edits["declared_files"] = strings.Join(files, ", ")
		p.DeclaredFiles = &files
	}
	if n := f.get("note"); n != "" {
		edits["note"] = n
	}
	changed := p.Changed()
	if len(changed) == 0 {
		f.err = "nothing changed"
		return nil
	}
	summary := fmt.Sprintf("fae-tui: edit task %s: %s.", before.ID, strings.Join(changed, ", "))
	return m.runWrite(writeAction{verb: "TaskUpdate", summary: note(summary, f.get("note")), class: "task", task: before.ID, edits: edits,
		run: func(ctx context.Context, d Daemon, message string) ([]string, error) {
			p.AuthorizingMessage = message
			r, err := d.TaskUpdate(ctx, p)
			if err != nil {
				return nil, err
			}
			if len(r.Changed) == 0 {
				return []string{"nothing changed: every field already had that value"}, nil
			}
			lines := []string{fmt.Sprintf("changed %s on %s", strings.Join(r.Changed, ", "), r.ID)}
			switch r.Effect {
			case "from-next-stage":
				lines = append(lines,
					"the task is running: the edits apply from its next stage;",
					"the turn in flight keeps its text and dispatched structured fields",
				)
			case "applied":
				lines = append(lines, "applied: the task's next brief carries the edits")
			default:
				lines = append(lines, "effect: "+r.Effect)
			}
			return lines, nil
		}})
}

func (m *Model) startDelete(id string) tea.Cmd {
	if !m.writable("delete tasks") {
		return nil
	}
	t, cmd := m.editTarget(id, "delete")
	if t == nil {
		return cmd
	}
	if terminal(t.State) {
		m.say(fmt.Sprintf("task %s is %s: a finished task cannot be deleted", id, t.State), true)
		return nil
	}
	before := *t
	carried := m.pendingEdits
	m.pendingEdits = nil
	val := func(key, base string) string {
		if v, ok := carried[key]; ok {
			return v
		}
		return base
	}
	fields := []*field{
		newField("reason", "Reason", "why the task goes (required)", val("reason", "")),
		newField("successor", "Successor", "optional: a task that replaces it; blocking dependents move to it", val("successor", "")),
		newField("note", "Note", "optional, recorded in the authorizing message", val("note", "")),
	}
	f, cmd := newForm("Delete task "+id, append([]string{"Delete retires the task and keeps its history and audit trail."}, deleteEffect(before, "")...), fields, func(m *Model, f *form) tea.Cmd {
		reason, successor := f.get("reason"), f.get("successor")
		if reason == "" {
			f.err = "a delete needs a reason"
			return nil
		}
		if strings.ContainsAny(successor, " \t,") {
			f.err = "the successor is one task id"
			return nil
		}
		if successor == before.ID {
			f.err = "a task cannot succeed itself"
			return nil
		}
		edits := map[string]string{"reason": reason, "successor": successor, "note": f.get("note")}
		m.modal = &confirm{title: "Delete task " + before.ID + "?", lines: deleteEffect(before, successor), yes: func(m *Model) tea.Cmd {
			p := client.TaskDeleteParams{ID: before.ID, Reason: reason, SupersededBy: successor, ExpectedVersion: before.Version}
			summary := fmt.Sprintf("fae-tui: delete task %s: %s.", before.ID, reason)
			if successor != "" {
				summary += fmt.Sprintf(" It is superseded by %s.", successor)
			}
			return m.runWrite(writeAction{verb: "TaskDelete", summary: note(summary, f.get("note")), class: "task", task: before.ID, edits: edits,
				run: func(ctx context.Context, d Daemon, message string) ([]string, error) {
					p.AuthorizingMessage = message
					r, err := d.TaskDelete(ctx, p)
					if err != nil {
						return nil, err
					}
					lines := []string{fmt.Sprintf("%s %s; its history stays", r.ID, r.Action)}
					if r.Pending {
						lines[0] = fmt.Sprintf("%s is stopped and will be %s once its turn in flight is collected; its history stays", r.ID, r.Action)
						if r.Dispatch != "" {
							lines = append(lines, "waiting for turn "+r.Dispatch)
						}
					} else if r.CollectedInFlight {
						lines = append(lines, "its in-flight turn was collected first")
					}
					if len(r.HeldDependents) > 0 {
						lines = append(lines, "held dependents: "+joinIDs(r.HeldDependents))
					}
					if len(r.RepointedDependents) > 0 {
						lines = append(lines, "re-pointed dependents: "+joinIDs(r.RepointedDependents))
					}
					return lines, nil
				}})
		}}
		return nil
	})
	f.resize(m.width)
	m.modal = f
	m.say("", false)
	return cmd
}

// deleteEffect says what a delete will do: withdraw a task that never
// started, stop a running one (collecting its turn in flight) and retire it,
// and hold or re-point its blocking dependents.
func deleteEffect(t client.TaskDetail, successor string) []string {
	// A task that entered no workflow stage never dispatched a turn.
	started := false
	for _, e := range t.Trace {
		started = started || !kernel(e.Stage)
	}
	out := []string{}
	switch {
	case t.InFlight:
		out = append(out, fmt.Sprintf("It is running a turn (in %s): the delete stops it and waits for that turn's result, then retires the task; until then the delete is pending.", firstNonEmpty(t.Stage, t.State)))
	case !started && (t.State == "Pending" || t.State == "Escalated" || t.State == "Parked"):
		out = append(out, "It never started, so it will be withdrawn.")
	case running(t.State, false):
		out = append(out, fmt.Sprintf("It is in %s with no turn in flight: it will be stopped and retired.", t.State))
	default:
		out = append(out, fmt.Sprintf("It is %s: it will be retired (reported as withdrawn if it never dispatched a turn).", t.State))
	}
	dependents := []string{}
	for _, e := range t.EdgesOut {
		if e.Kind == "blocking" && !terminal(e.OtherState) {
			dependents = append(dependents, e.Other)
		}
	}
	sort.Strings(dependents)
	switch {
	case len(dependents) == 0:
		out = append(out, "No unfinished task depends on it.")
	case successor != "":
		out = append(out, fmt.Sprintf("Its blocking dependents (%s) will wait on %s instead.", joinIDs(dependents), successor))
	default:
		out = append(out, fmt.Sprintf("Its blocking dependents (%s) will be held: the edge becomes dead until you re-point or remove it.", joinIDs(dependents)))
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func (m *Model) startEdgeAdd(id string) tea.Cmd {
	if !m.writable("edit edges") {
		return nil
	}
	fields := []*field{
		newField("from", "From", "the prerequisite task", ""),
		newField("to", "To", "the dependent task", id),
		newField("kind", "Kind", "blocking or relates", "blocking"),
		newField("note", "Note", "optional, recorded in the authorizing message", ""),
	}
	f, cmd := newForm("Add an edge", []string{"A blocking edge holds the dependent Pending until the prerequisite is Done; a relates edge never gates."}, fields, submitEdgeAdd)
	f.resize(m.width)
	m.modal = f
	return cmd
}

func submitEdgeAdd(m *Model, f *form) tea.Cmd {
	e := client.Edge{From: f.get("from"), To: f.get("to"), Kind: f.get("kind")}
	switch {
	case e.From == "" || e.To == "":
		f.err = "both ends are required"
		return nil
	case e.From == e.To:
		f.err = "a task cannot depend on itself"
		return nil
	case e.Kind != "blocking" && e.Kind != "relates":
		f.err = "kind is blocking or relates"
		return nil
	}
	var send func(accept bool) tea.Cmd
	send = func(accept bool) tea.Cmd {
		summary := fmt.Sprintf("fae-tui: add %s edge %s → %s.", e.Kind, e.From, e.To)
		a := writeAction{verb: "BacklogEdge", class: "task"}
		if accept {
			summary += " The operator accepts the edge on a dependent already dispatched."
		} else {
			a.accept = func(*Model) tea.Cmd { return send(true) }
		}
		a.summary = note(summary, f.get("note"))
		return m.runEdge(a, client.EdgeParams{Edge: e, AcceptInFlight: accept})
	}
	if len(m.dispatchedDependents([]client.Edge{e}, e.From)) > 0 {
		m.modal = &confirm{title: "Dependent " + e.To + " has already been dispatched", lines: []string{
			fmt.Sprintf("%s is %s. Adding the %s edge %s → %s accepts a release condition for the next time it is Pending; it does not interrupt a turn in flight.", e.To, m.snap.stateOf(e.To).Long, e.Kind, e.From, e.To),
			"Accept the edge in flight?",
		}, yes: func(m *Model) tea.Cmd { return send(true) }}
		return nil
	}
	return send(false)
}

func (m *Model) startEdgeRemove(id string) tea.Cmd {
	if !m.writable("edit edges") {
		return nil
	}
	if id == "" {
		m.say("select a task to remove one of its edges", true)
		return nil
	}
	// The open task's TaskGet lists its edges by key; otherwise the backlog
	// graph does, when the Graph view has read it.
	edges := []client.Edge{}
	switch {
	case m.detail != nil && m.detail.id == id && m.detail.data != nil:
		for _, r := range m.detail.data.EdgesIn {
			edges = append(edges, client.Edge{From: r.Other, To: id, Kind: r.Kind})
		}
		for _, r := range m.detail.data.EdgesOut {
			edges = append(edges, client.Edge{From: id, To: r.Other, Kind: r.Kind})
		}
	case !m.snap.loaded[partBacklog]:
		m.say("the edges of "+id+" are not loaded here: open the task (enter) and press - there", true)
		return nil
	default:
		for _, e := range m.snap.backlog.Edges {
			if e.From == id || e.To == id {
				edges = append(edges, e)
			}
		}
	}
	if len(edges) == 0 {
		m.say("task "+id+" has no edge", true)
		return nil
	}
	options := []string{}
	for _, e := range edges {
		options = append(options, fmt.Sprintf("%s → %s (%s)", e.From, e.To, e.Kind))
	}
	m.modal = &choice{title: "Remove which edge of " + id + "?", options: options, pick: func(m *Model, i int) tea.Cmd {
		e := edges[i]
		m.modal = &confirm{title: "Remove edge?", lines: []string{fmt.Sprintf("Remove the %s edge %s → %s.", e.Kind, e.From, e.To)}, yes: func(m *Model) tea.Cmd {
			return m.runEdge(writeAction{verb: "BacklogEdge", summary: fmt.Sprintf("fae-tui: remove %s edge %s → %s.", e.Kind, e.From, e.To), class: "task"}, client.EdgeParams{Edge: e, Remove: true})
		}}
		return nil
	}}
	return nil
}

func (m *Model) runEdge(a writeAction, p client.EdgeParams) tea.Cmd {
	a.run = func(ctx context.Context, d Daemon, message string) ([]string, error) {
		p.AuthorizingMessage = message
		raw, err := d.BacklogEdge(ctx, p)
		if err != nil {
			return nil, err
		}
		action := "added"
		if p.Remove {
			action = "removed"
		}
		lines := []string{fmt.Sprintf("%s %s edge %s → %s", action, p.Kind, p.From, p.To)}
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) == nil {
			keys := make([]string, 0, len(fields))
			for k := range fields {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				lines = append(lines, k+": "+compact(fields[k]))
			}
		}
		return lines, nil
	}
	return m.runWrite(a)
}
