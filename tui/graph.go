package tui

import (
	"fmt"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/xleelz/fae/tui/client"
	"github.com/xleelz/fae/tui/dag"
)

// graphState is the Graph view's selection, scroll, mode and cached layout.
type graphState struct {
	sel    string
	offset int
	first  int
	last   int
	list   bool
	// The layout is rebuilt only when the snapshot or a filter changes.
	layout   dag.Layout
	edges    []client.Edge
	built    int
	builtFor string
}

// graphNodes lists the graph's tasks, highest priority first: every Board
// task and backlog item, without finished ones when they are hidden.
func (m *Model) graphNodes() ([]string, map[string]bool) {
	seen := map[string]bool{}
	nodes := []string{}
	hidden := map[string]bool{}
	add := func(id, state string) {
		if seen[id] {
			return
		}
		seen[id] = true
		if m.hideFinished && terminal(state) {
			hidden[id] = true
			return
		}
		nodes = append(nodes, id)
	}
	for _, t := range m.snap.sortedTasks() {
		add(t.ID, t.State)
	}
	items := append([]client.BacklogItem(nil), m.snap.backlog.Items...)
	slices.SortFunc(items, func(a, b client.BacklogItem) int {
		if a.Priority != b.Priority {
			return b.Priority - a.Priority
		}
		return strings.Compare(a.ID, b.ID)
	})
	for _, item := range items {
		add(item.ID, item.State)
	}
	return nodes, hidden
}

// ensureLayout lays the graph out again when its inputs changed.
func (m *Model) ensureLayout() {
	key := fmt.Sprintf("%t/%t", m.hideFinished, m.includeRetired)
	if m.graph.built == m.snap.generation && m.graph.builtFor == key && m.graph.layout.Layer != nil {
		return
	}
	nodes, hidden := m.graphNodes()
	edges := []dag.Edge{}
	kept := []client.Edge{}
	for _, e := range m.snap.backlog.Edges {
		if hidden[e.From] || hidden[e.To] {
			continue
		}
		edges = append(edges, dag.Edge{From: e.From, To: e.To, Layered: e.Kind == "blocking"})
		kept = append(kept, e)
	}
	m.graph.layout, m.graph.edges = dag.Build(nodes, edges), kept
	m.graph.built, m.graph.builtFor = m.snap.generation, key
	if _, ok := m.graph.layout.Layer[m.graph.sel]; !ok {
		m.graph.sel = ""
		if len(m.graph.layout.Layers) > 0 {
			m.graph.sel = m.graph.layout.Layers[0][0]
		}
	}
}

// flat lists the nodes layer by layer, in row order.
func (g *graphState) flat() []string {
	out := []string{}
	for _, layer := range g.layout.Layers {
		out = append(out, layer...)
	}
	return out
}

func (m *Model) graphKey(s string) tea.Cmd {
	m.ensureLayout()
	l := m.graph.layout
	switch s {
	case "v":
		m.graph.list = !m.graph.list
		m.graph.offset = 0
		return nil
	case "f":
		m.hideFinished = !m.hideFinished
		m.say(map[bool]string{true: "hiding Done and Cancelled tasks", false: "showing finished tasks"}[m.hideFinished], false)
		m.ensureLayout()
		return nil
	case "/":
		m.modal = newFind()
		return nil
	case "enter":
		return m.openDetail(m.graph.sel)
	case "p":
		if p := m.currentParent(); p != nil && p.Task != "" {
			return m.jumpToParent()
		}
		// Jump along an edge: p to the first prerequisite.
		for _, e := range m.graph.edges {
			if e.To == m.graph.sel {
				m.graph.sel = e.From
				return nil
			}
		}
		m.say("No parent task", true)
		return nil
	case "n":
		// Jump along an edge: n to the first dependent.
		for _, e := range m.graph.edges {
			if e.From == m.graph.sel {
				m.graph.sel = e.To
				return nil
			}
		}
		return nil
	}
	if len(l.Layers) == 0 || m.graph.sel == "" {
		return nil
	}
	layer, row := l.Layer[m.graph.sel], l.Row[m.graph.sel]
	if m.graph.list {
		flat := m.graph.flat()
		i := slices.Index(flat, m.graph.sel)
		switch s {
		case "left", "h":
			if layer > 0 {
				m.graph.sel = l.Layers[layer-1][0]
			}
			return nil
		case "right", "l":
			if layer+1 < len(l.Layers) {
				m.graph.sel = l.Layers[layer+1][0]
			}
			return nil
		}
		if delta, ok := moveKey(s, m.height/2); ok {
			m.graph.sel = flat[selectable(i+delta, len(flat))]
		}
		return nil
	}
	switch s {
	case "left", "h":
		if layer > 0 {
			m.graph.sel = l.Layers[layer-1][selectable(row, len(l.Layers[layer-1]))]
		}
		return nil
	case "right", "l":
		if layer+1 < len(l.Layers) {
			m.graph.sel = l.Layers[layer+1][selectable(row, len(l.Layers[layer+1]))]
		}
		return nil
	}
	if delta, ok := moveKey(s, m.height/2); ok {
		m.graph.sel = l.Layers[layer][selectable(row+delta, len(l.Layers[layer]))]
	}
	return nil
}

// find selects the first task whose id contains text, in layout order.
func (m *Model) find(text string) {
	m.ensureLayout()
	text = strings.ToLower(strings.TrimSpace(text))
	for _, id := range m.graph.flat() {
		if strings.Contains(strings.ToLower(id), text) {
			m.graph.sel = id
			m.say("found "+id, false)
			return
		}
	}
	m.say(fmt.Sprintf("no task matches %q", text), true)
}

// relation marks a node by its edge to the selected node.
func (m *Model) relations() map[string]string {
	rel := map[string]string{}
	for _, e := range m.graph.edges {
		switch {
		case e.Kind != "blocking" && (e.From == m.graph.sel || e.To == m.graph.sel):
			other := e.From
			if other == m.graph.sel {
				other = e.To
			}
			rel[other] = "~"
		case e.To == m.graph.sel:
			rel[e.From] = "↑"
		case e.From == m.graph.sel:
			rel[e.To] = "↓"
		}
	}
	return rel
}

func (m *Model) graphBody(w, h int) []string {
	m.ensureLayout()
	l := m.graph.layout
	if len(l.Layers) == 0 {
		if !m.snap.loaded[partBoard] {
			return []string{styleFaint.Render("waiting for the Board and backlog reads…")}
		}
		return []string{"No tasks.", styleFaint.Render("Press c to create one (with --write).")}
	}
	// The panel below describes the selection; the graph takes the rest.
	room := max(h-graphPanel-1, 3)
	var body []string
	if m.graph.list {
		body = m.graphList(w, room)
	} else {
		body = m.graphColumns(w, room)
	}
	for len(body) < room {
		body = append(body, "")
	}
	info := m.graphInfo(w)
	if len(info) > graphPanel {
		info = info[:graphPanel]
	}
	return append(append(body, styleFaint.Render(strings.Repeat("─", w))), info...)
}

// graphPanel is the height of the selection panel under the graph.
const graphPanel = 6

// graphColumns draws one column per dependency layer, as many as fit,
// scrolled to keep the selection in view.
func (m *Model) graphColumns(w, h int) []string {
	l := m.graph.layout
	const gap = 2
	cellWidth := 30
	if w < 120 {
		cellWidth = 26
	}
	cols := max(1, (w+gap)/(cellWidth+gap))
	layer, row := l.Layer[m.graph.sel], l.Row[m.graph.sel]
	if layer < m.graph.first {
		m.graph.first = layer
	}
	if layer >= m.graph.first+cols {
		m.graph.first = layer - cols + 1
	}
	m.graph.first = selectable(m.graph.first, len(l.Layers))
	last := min(len(l.Layers), m.graph.first+cols)
	tallest := 0
	for i := m.graph.first; i < last; i++ {
		tallest = max(tallest, len(l.Layers[i]))
	}
	rows := h - 1
	m.graph.offset = scrollTo(m.graph.offset, row, rows, tallest)
	rel := m.relations()
	cyclic := map[string]bool{}
	for _, id := range l.Cyclic {
		cyclic[id] = true
	}
	head := ""
	for i := m.graph.first; i < last; i++ {
		title := fmt.Sprintf("layer %d (%d)", i, len(l.Layers[i]))
		if len(l.Cyclic) > 0 && i == len(l.Layers)-1 {
			title = "cycle! (malformed graph)"
		}
		head += fit(styleFaint.Render(title), cellWidth) + strings.Repeat(" ", gap)
	}
	m.graph.last = last
	out := []string{head}
	for r := m.graph.offset; r < m.graph.offset+rows && r < tallest; r++ {
		line := ""
		for i := m.graph.first; i < last; i++ {
			cell := ""
			if r < len(l.Layers[i]) {
				id := l.Layers[i][r]
				state := m.snap.stateOf(id)
				mark := rel[id]
				if mark == "" {
					mark = " "
				}
				label := state.Short
				if cyclic[id] {
					label = "cycle"
				}
				idWidth := max(cellWidth-len([]rune(label))-5, 6)
				text := fmt.Sprintf("%s%s %s %s", mark, state.glyph(), fit(id, idWidth), label)
				if id == m.graph.sel {
					cell = styleSelected.Render(fit(text, cellWidth))
				} else {
					cell = state.render(fit(text, cellWidth))
				}
			}
			line += fit(cell, cellWidth) + strings.Repeat(" ", gap)
		}
		out = append(out, line)
	}
	return out
}

// graphList draws the layers as one scrolling list, each task with its full
// state and its edges.
func (m *Model) graphList(w, h int) []string {
	l := m.graph.layout
	lines := []string{}
	cursor := 0
	ins, outs := map[string][]string{}, map[string][]string{}
	dead := m.deadEdges()
	for _, e := range m.graph.edges {
		arrow := "⇐"
		if e.Kind != "blocking" {
			arrow = "~"
		}
		mark := m.snap.stateOf(e.From).glyph()
		if dead[e] {
			mark = "✗dead"
		}
		ins[e.To] = append(ins[e.To], arrow+e.From+" "+mark)
		outs[e.From] = append(outs[e.From], e.To)
	}
	for i, layer := range l.Layers {
		lines = append(lines, styleFaint.Render(fmt.Sprintf("── layer %d · %s ──", i, plural(len(layer), "task"))))
		for _, id := range layer {
			state := m.snap.stateOf(id)
			text := fmt.Sprintf(" %s %s  %s", state.glyph(), fit(id, 28), state.Long)
			if wf := m.stageWorkflowOf(id); wf != nil {
				text += styleFaint.Render(fmt.Sprintf("  → %s@r%d", wf.ID, wf.Revision))
			}
			if t, ok := m.snap.tasks[id]; ok {
				if t.Child != nil {
					text += styleFaint.Render("  " + m.formatChildRef(t.Child, false))
				}
				if t.Parent != nil {
					text += styleFaint.Render(fmt.Sprintf("  parent: %s (%s#%d)", t.Parent.Task, t.Parent.Stage, t.Parent.Visit))
				}
			}
			edges := []string{}
			if len(ins[id]) > 0 {
				edges = append(edges, strings.Join(ins[id], ", "))
			}
			if len(outs[id]) > 0 {
				edges = append(edges, "⇒ "+strings.Join(outs[id], ", "))
			}
			if len(edges) > 0 {
				text += styleFaint.Render("   " + strings.Join(edges, "   "))
			}
			if id == m.graph.sel {
				cursor = len(lines)
				text = styleSelected.Render(clip(ansiStrip(text), w))
			} else {
				text = state.render(clip(ansiStrip(text), w))
			}
			lines = append(lines, text)
		}
	}
	m.graph.offset = scrollTo(m.graph.offset, cursor, h, len(lines))
	return window(lines, m.graph.offset, h)
}

func (m *Model) deadEdges() map[client.Edge]bool {
	dead := map[client.Edge]bool{}
	for _, d := range m.snap.backlog.DeadEdges {
		if d.Edge != nil {
			dead[*d.Edge] = true
		}
	}
	return dead
}

// stageWorkflowOf returns the pinned child WorkflowRef when task id is at a
// SubWorkflow stage in the topology snapshot (or waiting on a child).
func (m *Model) stageWorkflowOf(id string) *client.WorkflowRef {
	t, hasTask := m.snap.tasks[id]
	for _, mol := range m.snap.topology.Molecules {
		if hasTask && t.MoleculeID != "" && mol.ID != t.MoleculeID {
			continue
		}
		for _, st := range mol.Stages {
			if st.Workflow == nil {
				continue
			}
			if slices.Contains(st.Tasks, id) || (hasTask && st.Stage == t.State) {
				return st.Workflow
			}
		}
	}
	if hasTask && t.Child != nil && t.Child.WorkflowID != "" {
		return &client.WorkflowRef{ID: t.Child.WorkflowID, Revision: t.Child.Revision}
	}
	return nil
}

// graphInfo describes the selected task: its state and every edge.
func (m *Model) graphInfo(w int) []string {
	id := m.graph.sel
	if id == "" {
		return nil
	}
	state := m.snap.stateOf(id)
	head := styleBold.Render(id) + "  " + state.render(state.glyph()+" "+state.Long)
	hier := []string{}
	if wf := m.stageWorkflowOf(id); wf != nil {
		hier = append(hier, fmt.Sprintf("→ %s@r%d", wf.ID, wf.Revision))
	}
	if t, ok := m.snap.tasks[id]; ok {
		head += styleFaint.Render(fmt.Sprintf("  p%d · %s@%d", t.Priority, t.WorkflowID, t.WorkflowRevision))
		if t.MoleculeID != "" {
			head += styleFaint.Render(" · molecule " + shortID(t.MoleculeID))
		}
		if t.Parent != nil {
			hier = append(hier, fmt.Sprintf("parent: %s (%s#%d)", t.Parent.Task, t.Parent.Stage, t.Parent.Visit))
		}
		if t.Child != nil {
			hier = append(hier, m.formatChildRef(t.Child, true))
		}
	}
	in, out := client.EdgesOf(id, client.Backlog{Edges: m.graph.edges, DeadEdges: m.snap.backlog.DeadEdges}, m.snap.tasks)
	describe := func(refs []client.EdgeRef) string {
		if len(refs) == 0 {
			return styleFaint.Render("none")
		}
		parts := []string{}
		for _, r := range refs {
			s := m.snap.stateOf(r.Other)
			text := s.render(s.glyph()+" "+r.Other) + styleFaint.Render(" ("+r.Kind+", "+s.Long+")")
			if r.Dead {
				text += styleError.Render(" ✗ dead")
			}
			parts = append(parts, text)
		}
		return strings.Join(parts, "  ")
	}
	lines := []string{head}
	if len(hier) > 0 {
		lines = append(lines, styleFaint.Render(strings.Join(hier, " · ")))
	}
	lines = append(lines,
		"prerequisites ↑ "+describe(in),
		"dependents    ↓ "+describe(out),
	)
	if len(m.graph.layout.Cyclic) > 0 {
		lines = append(lines, styleError.Render("the graph has a blocking cycle through "+joinIDs(m.graph.layout.Cyclic)+"; the daemon should have refused it"))
	}
	filters := []string{}
	if m.hideFinished {
		filters = append(filters, "finished tasks hidden (f)")
	}
	if m.includeRetired {
		filters = append(filters, "retired tasks shown (R)")
	}
	if !m.graph.list && (m.graph.first > 0 || m.graph.last < len(m.graph.layout.Layers)) {
		filters = append(filters, fmt.Sprintf("showing layers %d–%d of %d (←→ scrolls)", m.graph.first, m.graph.last-1, len(m.graph.layout.Layers)))
	}
	if m.snap.board.Truncated {
		filters = append(filters, styleWarn.Render("board truncated at 10,000 tasks"))
	}
	if len(filters) > 0 {
		lines = append(lines, styleFaint.Render(strings.Join(filters, " · ")))
	}
	for i := range lines {
		lines[i] = clip(lines[i], w)
	}
	return lines
}
