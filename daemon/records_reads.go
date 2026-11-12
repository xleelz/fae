package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/workflow"
)

// TopologyParams pages running molecules, optionally of one workflow.
type TopologyParams struct {
	WorkflowID string `json:"workflow_id,omitempty"`
	After      string `json:"after,omitempty"`
	Limit      int    `json:"limit,omitempty"`
	// IncludeRetired lists a running molecule's Retired tasks too, at a
	// "Retired" stage (FAE-OPS-011); by default only non-terminal tasks.
	IncludeRetired bool `json:"include_retired,omitempty"`
}

// TopologyStage is one stage of a molecule's graph with the tasks at it.
type TopologyStage struct {
	Stage     string                `json:"stage"`
	Primitive workflow.Primitive    `json:"primitive,omitempty"`
	Roles     []string              `json:"roles,omitempty"`
	Workflow  *workflow.WorkflowRef `json:"workflow,omitempty"`
	// NormalPath marks the stages of the success path from the start stage.
	NormalPath bool     `json:"normal_path"`
	Tasks      []string `json:"tasks"`
}
type TopologyMolecule struct {
	controlplane.Molecule
	RoleOrder []string        `json:"role_order"`
	Stages    []TopologyStage `json:"stages"`
	// Error is why the molecule's stage graph could not be resolved: its
	// revision is one this binary cannot run (FAE-WF-093), its content no
	// longer reproduces the molecule, or the revision could not be read.
	// Its tasks are then listed by state alone, with no graph, and the
	// other molecules of the page are unaffected.
	Error string `json:"error,omitempty"`
}

// TopologyPage is a page of the cached topology snapshot. Statements, Rows
// and ElapsedMS are the plane read that built the snapshot, at SnapshotAt.
type TopologyPage struct {
	Molecules         []TopologyMolecule `json:"molecules"`
	NextCursor        string             `json:"next_cursor,omitempty"`
	Statements        int                `json:"statements"`
	Rows              int                `json:"rows"`
	ElapsedMS         float64            `json:"elapsed_ms"`
	SnapshotAt        time.Time          `json:"snapshot_at"`
	SnapshotAgeMillis int64              `json:"snapshot_age_ms"`
}

// moleculeGraph is the immutable stage graph of one molecule's revision, or
// (err) why the revision has none for this binary.
type moleculeGraph struct {
	roles  []string
	stages []TopologyStage
	index  map[string]int
	err    string
}

// graphOf walks the success path from the start stage, then lists every other
// stage in name order, with the kernel Pending, Blocked and Escalated states
// as stages of their own. It is cached per molecule: content is immutable.
func graphOf(doc workflow.Document) moleculeGraph {
	g := moleculeGraph{index: map[string]int{}}
	add := func(name string, normal bool) {
		if _, seen := g.index[name]; seen {
			return
		}
		s := doc.Stages[name]
		var wf *workflow.WorkflowRef
		if s.Workflow != nil {
			cp := *s.Workflow
			wf = &cp
		}
		node := TopologyStage{Stage: name, Primitive: s.Primitive, Workflow: wf, NormalPath: normal, Tasks: []string{}}
		if s.Role != "" && len(s.Reviewers) == 0 {
			node.Roles = []string{s.Role}
		}
		node.Roles = append(node.Roles, s.Reviewers...)
		g.index[name] = len(g.stages)
		g.stages = append(g.stages, node)
		if normal {
			for _, r := range node.Roles {
				known := false
				for _, x := range g.roles {
					known = known || x == r
				}
				if !known {
					g.roles = append(g.roles, r)
				}
			}
		}
	}
	add("Pending", true)
	stage := doc.Start
	for stage != "" && doc.Stages[stage].Primitive != "" {
		if _, seen := g.index[stage]; seen {
			break
		}
		add(stage, true)
		// The normal case leaves by a forward edge: not a failure, reverify or
		// escalation route, and not back to a visited stage; a branch's default
		// wins among those.
		s, next := doc.Stages[stage], ""
		for _, e := range doc.Edges {
			_, seen := g.index[e.To]
			if e.From != stage || seen || e.To == s.Failure || e.To == s.Reverify || doc.Stages[e.To].Primitive == workflow.Escalate {
				continue
			}
			if next == "" || e.To == s.Default {
				next = e.To
			}
		}
		stage = next
	}
	if stage == "Done" {
		add(stage, true)
	}
	names := []string{}
	for name := range doc.Stages {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		add(name, false)
	}
	for _, name := range []string{"Done", "Blocked", "Escalated"} {
		add(name, false)
	}
	return g
}

// topology is the FAE-API-001 Molecules read: one page of running molecules
// with each stage graph (cached by molecule id), the normal-case role order
// and the non-terminal tasks at each stage, from the cached snapshot
// (topology_cache.go; FAE-ROB-020, FAE-CP-009).
func (d *Daemon) topology(ctx context.Context, raw json.RawMessage) (TopologyPage, error) {
	var p TopologyParams
	if err := decode(raw, &p); err != nil {
		return TopologyPage{}, err
	}
	limit, err := readLimit(p.Limit)
	if err != nil {
		return TopologyPage{}, err
	}
	snap, err := d.cachedTopology(ctx, p.IncludeRetired)
	if err != nil {
		return TopologyPage{}, err
	}
	molecules, next, err := snap.page(p.WorkflowID, p.After, limit)
	if err != nil {
		return TopologyPage{}, err
	}
	out := TopologyPage{Molecules: molecules, NextCursor: next, Statements: snap.statements, Rows: snap.rows, ElapsedMS: float64(snap.elapsed.Microseconds()) / 1000, SnapshotAt: snap.at, SnapshotAgeMillis: max(0, time.Since(snap.at).Milliseconds())}
	for !fits(out) {
		if len(out.Molecules) <= 1 {
			return TopologyPage{}, tooLarge("one molecule's topology exceeds the response limit")
		}
		out.Molecules = out.Molecules[:len(out.Molecules)-1]
		out.NextCursor = out.Molecules[len(out.Molecules)-1].ID
	}
	return out, nil
}

// moleculeGraph resolves and caches a molecule's stage graph. The cache entry
// is keyed by the molecule id, whose content can never change, so a revision
// this binary cannot run (workflow.Parse; FAE-WF-093) is cached as a graph
// with that error: the molecule is rendered with it, never failing the page.
func (d *Daemon) moleculeGraph(ctx context.Context, m controlplane.Molecule) (moleculeGraph, error) {
	if g, ok := d.graphs.Load(m.ID); ok {
		// Shared, never changed: placeTopologyTasks copies the index.
		return g.(moleculeGraph), nil
	}
	rev, raw, err := d.store.GetWorkflow(ctx, m.WorkflowID, m.Revision)
	if err != nil {
		return moleculeGraph{}, err
	}
	if rev.Hash != m.ContentDigest {
		return moleculeGraph{}, controlplane.ErrMoleculeMismatch
	}
	g := moleculeGraph{index: map[string]int{}}
	if doc, err := d.parseWorkflow(raw); err == nil {
		g = graphOf(doc)
	} else {
		g.err = "revision cannot be run by this binary (FAE-WF-093): " + err.Error()
	}
	d.graphs.Store(m.ID, g)
	return g, nil
}

// ChainParams names a commit (or landing) sha whose provenance chain to read.
type ChainParams struct {
	Commit string `json:"commit"`
	Limit  int    `json:"limit,omitempty"`
}

func (d *Daemon) provenanceChain(ctx context.Context, raw json.RawMessage) (any, error) {
	var p ChainParams
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	if p.Limit == 0 {
		p.Limit = 1000
	}
	hops, err := d.store.ProvenanceChain(ctx, p.Commit, p.Limit)
	if err != nil {
		return nil, err
	}
	if len(hops) == 0 {
		return nil, errors.New("no recorded artifact names commit " + p.Commit)
	}
	out := map[string]any{"commit": p.Commit, "hops": hops}
	if !fits(out) {
		return nil, tooLarge("provenance chain exceeds the response limit; lower limit")
	}
	return out, nil
}

// recordRead serves the relational record reads outside the writer lock.
func (d *Daemon) recordRead(ctx context.Context, method string, raw json.RawMessage) (any, bool, error) {
	switch method {
	case "Topology":
		out, err := d.topology(ctx, raw)
		return out, true, err
	case "ProvenanceChain":
		out, err := d.provenanceChain(ctx, raw)
		return out, true, err
	}
	return nil, false, nil
}

// ArtifactRecord is one production with its artifact's content.
type ArtifactRecord struct {
	ArtifactReference
	Kind   string          `json:"kind"`
	Inputs []string        `json:"inputs"`
	Output json.RawMessage `json:"output,omitempty"`
}

// RecordsPage is the FAE-WF-052 history: derived from the transition log and
// the artifacts with their provenance alone, so it needs no live session. A
// page holds transitions after the cursor, the artifacts their completions
// recorded, and (on the first page) the task's decisions.
type RecordsPage struct {
	Task        JobIdentity                     `json:"task"`
	MoleculeID  string                          `json:"molecule_id,omitempty"`
	Transitions []controlplane.TransitionRecord `json:"transitions"`
	Artifacts   []ArtifactRecord                `json:"artifacts"`
	Decisions   []controlplane.DecisionRecord   `json:"decisions,omitempty"`
	Messages    []controlplane.Message          `json:"messages,omitempty"`
	NextCursor  int64                           `json:"next_cursor,omitempty"`
}

func (d *Daemon) historyRecords(ctx context.Context, p HistoryParams, limit int) (RecordsPage, error) {
	snapshot, err := d.headSnapshot(ctx, p.ID)
	if err != nil {
		return RecordsPage{}, err
	}
	if snapshot.State == "Retired" && !p.IncludeRetired {
		return RecordsPage{}, errors.New("retired task hidden; use include_retired")
	}
	transitions, err := d.store.TaskTransitions(ctx, p.ID, p.After, limit+1)
	if err != nil {
		return RecordsPage{}, err
	}
	out := RecordsPage{Task: identity(snapshot), MoleculeID: snapshot.MoleculeID}
	if p.After == 0 {
		if out.Decisions, err = d.store.TaskDecisions(ctx, p.ID, 500); err != nil {
			return RecordsPage{}, err
		}
	}
	if out.Messages, err = d.historyMessages(ctx, p.ID, p.After); err != nil {
		return RecordsPage{}, err
	}
	n := min(limit, len(transitions))
	dispatches := []string{}
	for _, t := range transitions[:n] {
		if dispatch, ok := strings.CutPrefix(t.Note, "completed "); ok {
			dispatches = append(dispatches, dispatch)
		}
	}
	rows, err := d.store.DispatchProvenance(ctx, p.ID, dispatches)
	if err != nil {
		return RecordsPage{}, err
	}
	d.enrichProvenanceFromMemory(p.ID, rows)
	artifacts := map[string][]ArtifactRecord{}
	for _, row := range rows {
		output, err := d.store.ArtifactContent(ctx, p.ID, row.ArtifactID)
		if err != nil {
			return RecordsPage{}, err
		}
		artifacts[row.DispatchID] = append(artifacts[row.DispatchID], ArtifactRecord{artifactReference(row), row.Kind, row.Inputs, output})
	}
	for {
		out.Transitions, out.Artifacts, out.NextCursor = transitions[:n], []ArtifactRecord{}, 0
		for _, t := range out.Transitions {
			if dispatch, ok := strings.CutPrefix(t.Note, "completed "); ok {
				out.Artifacts = append(out.Artifacts, artifacts[dispatch]...)
			}
		}
		if n < len(transitions) && n > 0 {
			out.NextCursor = transitions[n-1].ID
		}
		if fits(out) {
			return out, nil
		}
		if len(out.Messages) > 0 {
			out.Messages = out.Messages[1:]
			continue
		}
		if n <= 1 {
			return RecordsPage{}, tooLarge("one transition's records exceed the response limit")
		}
		n--
	}
}
