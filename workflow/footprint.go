package workflow

import (
	"fmt"
	"strings"
)

type Footprint struct {
	GraphSessions     *int   `json:"graph_derived_sessions,omitempty"`
	RoleCapBound      *int   `json:"role_cap_bound,omitempty"`
	SlotScope         string `json:"slot_scope"`
	DerivedSessions   int    `json:"derived_sessions"`
	EffectiveSessions int    `json:"effective_sessions"`
}
type FootprintResolver func(WorkflowRef) (Document, error)

// DeriveFootprint counts content, never host state. Memory and panes are priced
// by admission using the current reservation and adapter, not authored here.
func DeriveFootprint(d Document, resolve FootprintResolver, maxSessions int) (Footprint, error) {
	return deriveFootprint(d, resolve, maxSessions, map[string]bool{}, &footprintWork{children: map[string]Footprint{}})
}
func deriveFootprint(d Document, resolve FootprintResolver, maxSessions int, stack map[string]bool, work *footprintWork) (Footprint, error) {
	scope := d.SlotScope
	if scope == "" {
		scope = "molecule"
	}
	out := Footprint{SlotScope: scope}
	if scope != "molecule" && scope != "tree" {
		return out, fmt.Errorf("slot_scope: must be molecule or tree")
	}
	if d.SessionCount < 0 || d.SessionCount > 1000000 || (d.sessionCountSet && d.SessionCount == 0) {
		return out, fmt.Errorf("session_count: must be 1..1000000 when declared")
	}
	if d.MaxSessions < -1 || d.MaxSessions > 1000000 || (d.maxSessionsSet && d.MaxSessions == 0) {
		return out, fmt.Errorf("max_sessions: must be -1 or 1..1000000")
	}
	if maxSessions < -1 || maxSessions > 1000000 {
		return out, fmt.Errorf("max_sessions: must be -1 or 1..1000000")
	}
	if maxSessions == 0 {
		maxSessions = d.MaxSessions
	}

	key := d.ID
	if stack[key] {
		if scope == "tree" && maxSessions > 0 {
			out.DerivedSessions = maxSessions
			out.EffectiveSessions = max(maxSessions, d.SessionCount)
			return out, nil
		}
		return out, fmt.Errorf("footprint: cyclic pinned child workflow requires a finite tree max_sessions")
	}
	stack[key] = true
	defer delete(stack, key)
	nextStack := stack
	childCount := func(s Stage) (int, error) {
		if s.Workflow == nil {
			return 0, fmt.Errorf("footprint: SubWorkflow requires pinned child")
		}
		if s.Workflow.ID == "self" || s.Workflow.ID == d.ID {
			if d.Recursion == nil {
				if d.SessionCount > 0 || scope == "tree" && maxSessions > 0 {
					return 0, nil
				}
				return 0, fmt.Errorf("footprint: self-recursion requires max_descendants, max_sessions or session_count")
			}
			return 0, nil
		}
		if resolve == nil {
			return 0, fmt.Errorf("footprint: pinned child %s@%d requires resolver", s.Workflow.ID, s.Workflow.Revision)
		}
		cacheKey := fmt.Sprintf("%s@%d:%s", s.Workflow.ID, s.Workflow.Revision, scope)
		if cached, ok := work.children[cacheKey]; ok {
			return cached.EffectiveSessions, nil
		}
		if err := work.step(); err != nil {
			return 0, err
		}
		child, err := resolve(*s.Workflow)
		if err != nil {
			return 0, err
		}
		child.SlotScope = scope
		child.MaxSessions = 0
		child.maxSessionsSet = false
		f, err := deriveFootprint(child, resolve, 0, nextStack, work)
		if err == nil {
			work.children[cacheKey] = f
		}
		return f.EffectiveSessions, err
	}
	var n int
	var err error
	if scope == "molecule" {
		n, err = moleculeSessions(d, childCount, work)
	} else {
		n, err = treeGraphPeak(d, childCount, work)
	}
	mutual := false
	if err != nil && scope == "tree" && strings.Contains(err.Error(), "cyclic pinned child workflow") {
		if maxSessions > 0 && d.Recursion == nil {
			out.DerivedSessions = maxSessions
			out.EffectiveSessions = max(maxSessions, d.SessionCount)
			return out, nil
		}
		n, err = recursiveTreePeak(d, resolve, work)
		mutual = err == nil
	}
	if err != nil {
		return out, err
	}
	if !mutual && d.Recursion != nil && d.Recursion.MaxDescendants > 0 {
		self := false
		for _, s := range d.Stages {
			if s.Workflow != nil && (s.Workflow.ID == "self" || s.Workflow.ID == d.ID) {
				self = true
			}
		}
		if self {
			if n > 1000000/(d.Recursion.MaxDescendants+1) {
				return out, fmt.Errorf("footprint: derived session count exceeds 1000000")
			}
			n *= d.Recursion.MaxDescendants + 1
		}
	}
	if n < 0 || n > 1000000 {
		return out, fmt.Errorf("footprint: derived session count exceeds 1000000")
	}
	if scope == "tree" {
		graph := n
		out.GraphSessions = &graph
		if d.MaxSessions <= 0 {
			bound, known, err := treeRoleCapBound(d, resolve, work)
			if err != nil {
				return out, err
			}
			if known {
				out.RoleCapBound = &bound
				n = min(n, bound)
			}
		}
	}
	out.DerivedSessions = n
	out.EffectiveSessions = n
	if d.SessionCount > 0 {
		if d.SessionCount < n {
			return out, fmt.Errorf("session_count: declared %d is below derived %d", d.SessionCount, n)
		}
		out.EffectiveSessions = d.SessionCount
	}
	if scope == "tree" && maxSessions > 0 {
		out.EffectiveSessions = max(maxSessions, d.SessionCount)
	}
	return out, nil
}
func FootprintIssues(d Document) []Issue {
	_, err := DeriveFootprint(d, nil, 0)
	if err == nil || strings.Contains(err.Error(), "requires resolver") {
		return nil
	}
	path := "footprint"
	if strings.HasPrefix(err.Error(), "slot_scope:") {
		path = "slot_scope"
	}
	if strings.HasPrefix(err.Error(), "max_sessions:") {
		path = "max_sessions"
	}
	if strings.HasPrefix(err.Error(), "session_count:") {
		path = "session_count"
	}
	return []Issue{{path, err.Error()}}
}

func moleculeSessions(d Document, child func(Stage) (int, error), work *footprintWork) (int, error) {
	edges := map[string][]string{}
	for _, e := range d.Edges {
		edges[e.From] = append(edges[e.From], e.To)
	}
	type keys map[string]int
	clone := func(in keys) keys {
		out := keys{}
		for k, v := range in {
			out[k] = v
		}
		return out
	}
	merge := func(a, b keys) keys {
		o := clone(a)
		for k, v := range b {
			if v > o[k] {
				o[k] = v
			}
		}
		return o
	}
	var walk func(string, string, map[string]bool) ([]keys, error)
	walk = func(id, stop string, seen map[string]bool) ([]keys, error) {
		if err := work.step(); err != nil {
			return nil, err
		}
		if id == stop || id == "Done" || seen[id] {
			return []keys{{}}, nil
		}
		s, ok := d.Stages[id]
		if !ok {
			return []keys{{}}, nil
		}
		seen[id] = true
		defer delete(seen, id)
		next := seen
		own := keys{}
		switch s.Primitive {
		case AgentTurn:
			own["role:"+s.Role] = 1
		case Review:
			n := max(1, s.Count) * max(1, len(s.Reviewers))
			own["review:"+id] = n
		case SubWorkflow:
			n, err := child(s)
			if err != nil {
				return nil, err
			}
			own["child:"+id] = n
		}
		prefix := []keys{own}
		combine := func(left, right []keys) ([]keys, error) {
			out := []keys{}
			for _, a := range left {
				for _, b := range right {
					if err := work.allocate(len(a) + len(b)); err != nil {
						return nil, err
					}
					out = append(out, merge(a, b))
					if len(out) > 4096 {
						return nil, fmt.Errorf("footprint: graph path analysis exceeds 4096 alternatives")
					}
				}
			}
			return out, nil
		}
		if s.Primitive == FanOut {
			for _, b := range EffectiveBranches(s) {
				part, err := walk(b.Start, s.Join, next)
				if err != nil {
					return nil, err
				}
				prefix, err = combine(prefix, part)
				if err != nil {
					return nil, err
				}
			}
			tail, err := walk(s.Join, stop, next)
			if err != nil {
				return nil, err
			}
			return combine(prefix, tail)
		}
		tails := []keys{}
		for _, to := range edges[id] {
			part, err := walk(to, stop, next)
			if err != nil {
				return nil, err
			}
			tails = append(tails, part...)
			if len(tails) > 4096 {
				return nil, fmt.Errorf("footprint: graph path analysis exceeds 4096 alternatives")
			}
		}
		if len(tails) == 0 {
			return prefix, nil
		}
		return combine(prefix, tails)
	}
	paths, err := walk(d.Start, "", map[string]bool{})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, p := range paths {
		total := 0
		for _, weight := range p {
			total += weight
		}
		if total > n {
			n = total
		}
	}
	return n, nil
}

// One budget covers the root, every child revision and both graph analyses.
// It bounds DAG expansion as well as recursion; graph depth alone does not.
type footprintWork struct {
	steps    int
	entries  int
	children map[string]Footprint
}

func (w *footprintWork) step() error {
	w.steps++
	if w.steps > 100000 {
		return fmt.Errorf("footprint: graph analysis exceeds 100000 steps")
	}
	return nil
}

// Bound total map-entry copying too: a long path otherwise costs quadratic
// allocation despite remaining within the node-visit budget.
func (w *footprintWork) allocate(n int) error {
	w.entries += n
	if w.entries > 1000000 {
		return fmt.Errorf("footprint: graph analysis exceeds 1000000 copied entries")
	}
	return nil
}
