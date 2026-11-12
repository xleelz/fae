package tui

import (
	"fmt"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/xleelz/fae/tui/client"
)

// moleculesState is the Molecules view's selection: a molecule, a task row
// within its stage graph, and the scroll offset.
type moleculesState struct {
	id       string
	row      int
	offset   int
	wantTask string
	// compact hides the stages that hold no task.
	compact bool
}

// molecules lists the running molecules by workflow, then age, then id.
func (m *Model) molecules() []client.Molecule {
	out := append([]client.Molecule(nil), m.snap.topology.Molecules...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.WorkflowID != b.WorkflowID {
			return a.WorkflowID < b.WorkflowID
		}
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		return a.ID < b.ID
	})
	return out
}

// selectedMolecule returns the selected molecule's index, keeping the
// selection on the same molecule across refreshes.
func (m *Model) selectedMolecule(ms []client.Molecule) int {
	for i, mol := range ms {
		if mol.ID == m.mol.id {
			return i
		}
	}
	if len(ms) > 0 {
		m.mol.id = ms[0].ID
	}
	return 0
}

// moleculeTaskRows lists a molecule's tasks in stage order: the normal path
// first, then the other stages that hold tasks.
func moleculeTaskRows(mol client.Molecule) []string {
	rows := []string{}
	for _, normal := range []bool{true, false} {
		for _, st := range mol.Stages {
			if st.NormalPath == normal {
				rows = append(rows, st.Tasks...)
			}
		}
	}
	return rows
}

func (m *Model) taskMatchesFilter(id string) bool {
	if m.filter == "" {
		return true
	}
	t := m.snap.tasks[id]
	return matchesFilter(m.filter, id, t.ID, t.State, t.Reason, t.MoleculeID)
}

func (m *Model) filteredMoleculeTaskRows(mol client.Molecule) []string {
	rows := moleculeTaskRows(mol)
	if m.filter == "" {
		return rows
	}
	out := make([]string, 0, len(rows))
	for _, id := range rows {
		if m.taskMatchesFilter(id) {
			out = append(out, id)
		}
	}
	return out
}

func (m *Model) selectedMoleculeTask() string {
	ms := m.molecules()
	if len(ms) == 0 {
		return ""
	}
	rows := m.filteredMoleculeTaskRows(ms[m.selectedMolecule(ms)])
	if len(rows) == 0 {
		return ""
	}
	return rows[selectable(m.mol.row, len(rows))]
}

func (m *Model) moleculesKey(s string) tea.Cmd {
	ms := m.molecules()
	if len(ms) == 0 {
		return nil
	}
	i := m.selectedMolecule(ms)
	switch s {
	case "z":
		m.mol.compact = !m.mol.compact
		m.mol.offset = 0
		return nil
	case "left", "h":
		m.mol.id, m.mol.row, m.mol.offset = ms[(i+len(ms)-1)%len(ms)].ID, 0, 0
		return nil
	case "right", "l":
		m.mol.id, m.mol.row, m.mol.offset = ms[(i+1)%len(ms)].ID, 0, 0
		return nil
	case "enter":
		return m.openDetail(m.selectedMoleculeTask())
	}
	if delta, ok := moveKey(s, m.height/2); ok {
		rows := m.filteredMoleculeTaskRows(ms[i])
		m.mol.row = selectable(m.mol.row+delta, len(rows))
	}
	return nil
}

func (m *Model) moleculesBody(w, h int) []string {
	ms := m.molecules()
	if len(ms) == 0 {
		if !m.snap.loaded[partTopology] {
			return []string{styleFaint.Render("waiting for the Molecules read…")}
		}
		return []string{"No running molecules.", styleFaint.Render("A molecule runs a workflow revision's tasks; admit work to start one.")}
	}
	sel := m.selectedMolecule(ms)
	// The molecule list: a left column on a wide screen, else a strip above.
	list := []string{styleHeading.Render(fmt.Sprintf("Molecules (%d)", len(ms)))}
	listWidth := min(36, w/3)
	for i, mol := range ms {
		tasks := 0
		for _, st := range mol.Stages {
			tasks += len(st.Tasks)
		}
		line := fmt.Sprintf("%s@%d %s · %d", mol.WorkflowID, mol.Revision, shortID(mol.ID), tasks)
		if i == sel {
			line = styleSelected.Render(fit(line, listWidth-1))
		}
		list = append(list, line)
	}
	if w < 90 {
		strip := window(list, scrollTo(0, sel+1, 4, len(list)), 4)
		out := []string{}
		for _, line := range strip {
			out = append(out, fit(line, w))
		}
		return append(out, m.moleculeGraph(ms[sel], w, h-len(out))...)
	}
	graph := m.moleculeGraph(ms[sel], w-listWidth-2, h)
	left := []string{}
	for _, line := range window(list, scrollTo(0, sel+1, h, len(list)), h) {
		left = append(left, fit(line, listWidth))
	}
	return strings.Split(lipgloss.JoinHorizontal(lipgloss.Top, strings.Join(left, "\n"), "  ", strings.Join(graph, "\n")), "\n")
}

// moleculeGraph renders a molecule's stage graph: every normal-path stage in
// the workflow's normal-case order, then the off-path stages that hold
// tasks, each with its tasks and their implementation state.
func (m *Model) moleculeGraph(mol client.Molecule, w, h int) []string {
	head := []string{
		styleBold.Render("Molecule "+mol.ID) + styleFaint.Render(fmt.Sprintf("  workflow %s rev %d · started %s", mol.WorkflowID, mol.Revision, ago(m.now(), mol.CreatedAt))),
	}
	if mol.Error != "" {
		head = append(head, styleError.Render("stage graph unavailable: "+mol.Error))
	}
	if len(mol.RoleOrder) > 0 {
		head = append(head, styleFaint.Render("normal-case roles: "+strings.Join(mol.RoleOrder, " → ")))
	}
	if m.filter != "" || m.filtering {
		head = append(head, styleWarn.Render("filter: "+m.filter))
	}
	rows := m.filteredMoleculeTaskRows(mol)
	cursor := selectable(m.mol.row, len(rows))
	if len(rows) > 0 {
		if t, ok := m.snap.tasks[rows[cursor]]; ok {
			hier := []string{}
			if t.Parent != nil {
				hier = append(hier, fmt.Sprintf("parent: %s (%s#%d)", t.Parent.Task, t.Parent.Stage, t.Parent.Visit))
			}
			if t.Child != nil {
				hier = append(hier, m.formatChildRef(t.Child, true))
			}
			if len(hier) > 0 {
				head = append(head, styleFaint.Render(strings.Join(hier, " · ")))
			}
		}
	}
	lines := []string{}
	cursorLine, row, hidden := 0, 0, 0
	stage := func(st client.Stage, onPath bool) {
		tasks := st.Tasks
		if m.filter != "" {
			filtered := make([]string, 0, len(st.Tasks))
			for _, id := range st.Tasks {
				if m.taskMatchesFilter(id) {
					filtered = append(filtered, id)
				}
			}
			tasks = filtered
		}
		marker := "●"
		if !onPath {
			marker = "○"
		}
		desc := []string{}
		if st.Primitive != "" {
			desc = append(desc, st.Primitive)
		}
		if st.Workflow != nil {
			desc = append(desc, fmt.Sprintf("→ %s@r%d", st.Workflow.ID, st.Workflow.Revision))
		}
		if len(st.Roles) > 0 {
			desc = append(desc, strings.Join(st.Roles, ", "))
		}
		title := fmt.Sprintf("%s %s", marker, st.Stage)
		if len(desc) > 0 {
			title += styleFaint.Render("  " + strings.Join(desc, " · "))
		}
		lines = append(lines, styleBold.Render(title)+fmt.Sprintf("  (%d)", len(tasks)))
		for _, id := range tasks {
			state := m.snap.stateOf(id)
			text := fmt.Sprintf("    %s %s", state.glyph(), id)
			if t, ok := m.snap.tasks[id]; ok {
				text += styleFaint.Render(fmt.Sprintf("  p%d", t.Priority))
			}
			text += "  " + state.render(state.Long)
			if d, ok := m.snap.details[id]; ok && d.Fan != nil {
				text += "  " + styleWarn.Render(fmt.Sprintf("[FanOut %d branches]", len(d.Fan.Branches)))
			}
			if t, ok := m.snap.tasks[id]; ok {
				if t.Child != nil {
					text += styleFaint.Render("  " + m.formatChildRef(t.Child, false))
				}
				if t.Parent != nil {
					text += styleFaint.Render(fmt.Sprintf("  parent: %s (%s#%d)", t.Parent.Task, t.Parent.Stage, t.Parent.Visit))
				}
			}
			if row == cursor {
				cursorLine = len(lines)
				text = styleSelected.Render(ansiStrip(text))
			}
			lines = append(lines, text)
			row++
		}
	}
	emptyPath := 0
	for _, st := range mol.Stages {
		if !st.NormalPath {
			continue
		}
		if m.mol.compact && len(st.Tasks) == 0 {
			emptyPath++
			continue
		}
		stage(st, true)
	}
	if emptyPath > 0 {
		lines = append(lines, styleFaint.Render(fmt.Sprintf("(%d empty stages on the normal path hidden; z shows them)", emptyPath)))
	}
	offPath := false
	for _, st := range mol.Stages {
		if st.NormalPath {
			continue
		}
		if len(st.Tasks) == 0 {
			hidden++
			continue
		}
		if !offPath {
			lines = append(lines, styleFaint.Render("── off the normal path ──"))
			offPath = true
		}
		stage(st, false)
	}
	if hidden > 0 {
		lines = append(lines, styleFaint.Render(fmt.Sprintf("(%d other stages hold no task)", hidden)))
	}
	if len(rows) == 0 {
		lines = append(lines, styleFaint.Render("(no task at any stage)"))
	}
	room := h - len(head)
	m.mol.offset = scrollTo(m.mol.offset, cursorLine, room, len(lines))
	out := append(head, window(lines, m.mol.offset, room)...)
	for i := range out {
		out[i] = clip(out[i], w)
	}
	return out
}

// shortID shortens a long hash-like id for a list.
func shortID(id string) string {
	if len(id) > 14 {
		return id[:12] + "…"
	}
	return id
}

func (m *Model) formatChildRef(c *client.ChildRef, includeSingleState bool) string {
	if len(c.Children) <= 1 {
		childText := fmt.Sprintf("child: %s (%s@r%d)", c.Task, c.WorkflowID, c.Revision)
		if includeSingleState {
			if ct, ok := m.snap.tasks[c.Task]; ok && ct.State != "" {
				childText += " · " + m.snap.stateOf(c.Task).Short
			}
		}
		return childText
	}
	ids := make([]string, 0, len(c.Children))
	for _, sub := range c.Children {
		ids = append(ids, sub.Task)
	}
	return fmt.Sprintf("children (%d): %s (%s@r%d)", len(c.Children), strings.Join(ids, ", "), c.WorkflowID, c.Revision)
}
