package workflow

import (
	"fmt"
	"sort"
	"strings"

	"github.com/xleelz/fae/domain"
)

// FanIn join rules; k-of-n is shared with Review aggregation.
const (
	JoinAll    = "all"
	JoinAny    = "any"
	JoinSettle = "settle"
)

// BranchesFromSpec configures a FanOut stage whose branch count is derived at
// runtime from an array field of a prior stage's output (FAE-WF-096).
type BranchesFromSpec struct {
	Type       string      `json:"type,omitempty"`
	Input      string      `json:"input,omitempty"`
	Projection *Projection `json:"projection,omitempty"`
	Stage      string      `json:"stage"`           // stage ID
	Field      string      `json:"field,omitempty"` // array field in stage output
	Min        int         `json:"min,omitempty"`   // >= 0
	Max        int         `json:"max,omitempty"`   // 1..256 (default 256)
	Empty      string      `json:"empty,omitempty"` // required route stage ID when Min == 0
}

// FanOutSpec groups FanOut configuration fields (FAE-WF-096).
type FanOutSpec struct {
	Branches       []FanBranch       `json:"branches,omitempty"`
	BranchTemplate *FanBranch        `json:"branch_template,omitempty"`
	BranchesFrom   *BranchesFromSpec `json:"branches_from,omitempty"`
	Parallel       bool              `json:"parallel,omitempty"`
	WriteScope     []string          `json:"write_scope,omitempty"`
	Join           string            `json:"join,omitempty"`
}

// EffectiveBranches returns the static branches of s, or its single branch
// template when s uses BranchesFrom.
func EffectiveBranches(s Stage) []FanBranch {
	if s.BranchesFrom != nil {
		if s.BranchTemplate != nil {
			b := *s.BranchTemplate
			if b.ID == "" {
				b.ID = "template"
			}
			if len(b.WriteScope) == 0 && len(s.WriteScope) > 0 {
				b.WriteScope = append([]string(nil), s.WriteScope...)
			}
			return []FanBranch{b}
		}
		if len(s.Branches) == 1 {
			b := s.Branches[0]
			if b.ID == "" {
				b.ID = "template"
			}
			if len(b.WriteScope) == 0 && len(s.WriteScope) > 0 {
				b.WriteScope = append([]string(nil), s.WriteScope...)
			}
			return []FanBranch{b}
		}
		return nil
	}
	if len(s.WriteScope) == 0 {
		return s.Branches
	}
	out := make([]FanBranch, len(s.Branches))
	for i, b := range s.Branches {
		if len(b.WriteScope) == 0 {
			b.WriteScope = append([]string(nil), s.WriteScope...)
		}
		out[i] = b
	}
	return out
}

// fanReentry lets a FanIn enter the next branch of the FanOut that names it:
// branches run one after another inside one task, each entered at its start.
func fanReentry(d Document, from, to string) bool {
	if j, ok := d.Stages[from]; ok && j.Primitive == FanIn {
		for _, s := range d.Stages {
			if s.Primitive == FanOut && s.Join == from {
				for _, b := range EffectiveBranches(s) {
					if b.Start == to {
						return true
					}
				}
			}
		}
	}
	for id, s := range d.Stages {
		if s.Primitive != FanOut {
			continue
		}
		if s.BranchesFrom != nil && s.BranchesFrom.Empty != "" && from == id && to == s.BranchesFrom.Empty {
			return true
		}
		if s.Parallel || s.BranchesFrom != nil {
			members := BranchOf(d, id)
			_, fromIn := members[from]
			_, toIn := members[to]
			fromFan := fromIn || from == id || from == s.Join
			toFan := toIn || to == id || to == s.Join
			if fromFan && toFan {
				return true
			}
		}
	}
	return false
}

// FanOutOf returns the FanOut stage that names join.
func FanOutOf(d Document, join string) (string, Stage, bool) {
	ids := []string{}
	for id, s := range d.Stages {
		if s.Primitive == FanOut && s.Join == join {
			ids = append(ids, id)
		}
	}
	if len(ids) != 1 {
		return "", Stage{}, false
	}
	return ids[0], d.Stages[ids[0]], true
}

// branchRegions returns, per branch of the FanOut fan, the stages reachable
// from the branch start without passing the join, the FanOut or a kernel
// state. Escalate stages escalate the whole task and belong to no branch.
func branchRegions(d Document, fan string) map[string]map[string]bool {
	s := d.Stages[fan]
	edges := map[string][]string{}
	for _, e := range d.Edges {
		edges[e.From] = append(edges[e.From], e.To)
	}
	out := map[string]map[string]bool{}
	for _, b := range EffectiveBranches(s) {
		region := map[string]bool{}
		stack := []string{b.Start}
		for len(stack) > 0 {
			n := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			x, ok := d.Stages[n]
			if !ok || region[n] || n == s.Join || n == fan || x.Primitive == Escalate {
				continue
			}
			region[n] = true
			stack = append(stack, edges[n]...)
		}
		out[b.ID] = region
	}
	return out
}

// BranchOf maps each stage inside a branch of the FanOut fan to its branch id.
func BranchOf(d Document, fan string) map[string]string {
	owner := map[string]string{}
	for _, b := range EffectiveBranches(d.Stages[fan]) {
		for n := range branchRegions(d, fan)[b.ID] {
			if _, taken := owner[n]; !taken {
				owner[n] = b.ID
			}
		}
	}
	return owner
}

// JoinVerdict decides a join from branch outcomes: true or false once the
// rule is settled, or undecided while unfinished branches could change it.
func JoinVerdict(rule string, quorum, passed, failed, total int) (verdict, decided bool) {
	open := total - passed - failed
	switch rule {
	case JoinSettle:
		if quorum == 0 {
			quorum = total
		}
		return passed >= quorum, open == 0
	case JoinAny:
		return passed > 0, passed > 0 || open == 0
	case KOfN:
		return passed >= quorum, passed >= quorum || passed+open < quorum
	}
	return passed == total, failed > 0 || open == 0
}

// validateFans checks FanOut/FanIn pairs. Branches run one after another in
// one task, so each branch is a disjoint region entered only from its FanOut
// and left only through its join (or an Escalate stage). Branches hold no
// change producers, protected effects, decisions, nested fans or
// sub-workflows: facts established in any branch then persist across the join,
// and the per-path floor checks stay sound. A branch may not bind an input from
// a sibling branch (FAE-WF-033 isolation).
func validateFans(d Document, outgoing map[string][]string, bad func(string, string)) {
	ids := make([]string, 0, len(d.Stages))
	for id := range d.Stages {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		s, p := d.Stages[id], "stages."+id
		if s.Primitive != FanOut && (len(s.Branches) > 0 || s.Join != "" || s.BranchesFrom != nil || s.BranchTemplate != nil || s.Parallel || len(s.WriteScope) > 0) {
			bad(p, "branches and join belong to FanOut stages")
		}
		if s.Primitive != FanIn && s.Rule != "" {
			bad(p+".rule", "a join rule belongs to FanIn stages")
		}
		switch s.Primitive {
		case FanIn:
			fan, f, ok := FanOutOf(d, id)
			if !ok {
				bad(p, "a FanIn is joined by exactly one FanOut")
			}
			n := len(f.Branches)
			if f.BranchesFrom != nil {
				n = f.BranchesFrom.Max
				if n <= 0 {
					n = 256
				}
			}
			if s.Rule != JoinAll && s.Rule != JoinAny && s.Rule != KOfN && s.Rule != JoinSettle {
				bad(p+".rule", "join rule must be all, any or k-of-n")
			}
			if (s.Rule != JoinSettle && (s.Rule == KOfN) != (s.Quorum != 0)) || s.Quorum < 0 || ok && s.Quorum > n {
				bad(p+".quorum", fmt.Sprintf("k-of-n requires a quorum between 1 and the %d branches of %s; other rules take none", n, fan))
			}
			if !contains(outgoing[id], s.Failure) {
				bad(p+".failure", "a join needs a failure route edge")
			}
		case FanOut:
			validateFan(d, id, s, outgoing, bad)
		}
	}
}

func validateFan(d Document, id string, s Stage, outgoing map[string][]string, bad func(string, string)) {
	p := "stages." + id
	branches := EffectiveBranches(s)
	allowedEdges := map[string]bool{}
	if s.BranchesFrom != nil {
		bf := s.BranchesFrom
		if bf.Projection == nil && bf.Stage == "" && bf.Type == "" && bf.Input == "" {
			bad(p+".branches_from.stage", "branches_from requires stage, type, input or projection")
		}
		if _, ok := ProducerDeclaration(d, bf.Stage); bf.Stage != "" && (!ok || bf.Stage == id) {
			bad(p+".branches_from.stage", "unknown branches_from stage "+bf.Stage)
		}
		if _, ok := ArtifactDeclaration(d, bf.Type); bf.Type != "" && !ok {
			bad(p+".branches_from.type", "unknown artifact type")
		}
		if _, ok := SyntheticArtifact(d, "input:"+bf.Input); bf.Input != "" && !ok {
			bad(p+".branches_from.input", "unknown workflow input")
		}
		if bf.Min < 0 || bf.Min > 256 {
			bad(p+".branches_from.min", "branches_from min must be between 0 and 256")
		}
		maxB := bf.Max
		if maxB == 0 {
			maxB = 256
		}
		if bf.Max < 0 || bf.Max > 256 || maxB < 1 || maxB < bf.Min {
			bad(p+".branches_from.max", "branches_from max must be between 1 and 256 and >= min")
		}
		if bf.Min == 0 {
			if bf.Empty == "" || !contains(outgoing[id], bf.Empty) || bf.Empty == id {
				bad(p+".branches_from.empty", "branches_from with min 0 requires an outgoing empty route stage")
			} else {
				allowedEdges[bf.Empty] = true
			}
		} else if bf.Empty != "" {
			if !contains(outgoing[id], bf.Empty) || bf.Empty == id {
				bad(p+".branches_from.empty", "branches_from empty must be an outgoing edge")
			} else {
				allowedEdges[bf.Empty] = true
			}
		}
		if !((s.BranchTemplate != nil && len(s.Branches) == 0) || (s.BranchTemplate == nil && len(s.Branches) == 1)) {
			bad(p+".branches", "branches_from requires a single branch template")
		}
	} else {
		if s.BranchTemplate != nil {
			bad(p+".branch_template", "branch_template requires branches_from")
		}
		if len(s.Branches) < 2 || len(s.Branches) > 16 {
			bad(p+".branches", "between 2 and 16 branches required")
		}
	}
	ids, starts := map[string]bool{}, map[string]bool{}
	for i, b := range branches {
		bp := fmt.Sprintf("%s.branches[%d]", p, i)
		if s.BranchesFrom != nil && s.BranchTemplate != nil {
			bp = p + ".branch_template"
		}
		if !identifier(b.ID, 64) || ids[b.ID] {
			bad(bp+".id", "unique branch id of letters, digits, '-', '_' or '.' required")
		}
		ids[b.ID] = true
		if x, ok := d.Stages[b.Start]; !ok || starts[b.Start] || b.Start == id || b.Start == s.Join || x.Primitive == Escalate {
			bad(bp+".start", "a distinct, existing, non-Escalate start stage required")
		}
		starts[b.Start] = true
		allowedEdges[b.Start] = true
	}
	if s.Failure != "" && contains(outgoing[id], s.Failure) {
		allowedEdges[s.Failure] = true
	}
	for _, to := range outgoing[id] {
		if !allowedEdges[to] {
			bad(p, "edge to "+to+" is not a branch start; a FanOut's edges are exactly its branch starts")
		}
	}
	for start := range starts {
		if !contains(outgoing[id], start) {
			bad(p, "branch start "+start+" needs an edge from the FanOut")
		}
	}
	if j, ok := d.Stages[s.Join]; !ok || j.Primitive != FanIn {
		bad(p+".join", "must name a FanIn stage")
	}
	regions := branchRegions(d, id)
	owner := map[string]string{}
	changeProducingBranches := map[int]bool{}
	for bi, b := range branches {
		bp := fmt.Sprintf("%s.branches[%d]", p, bi)
		if s.BranchesFrom != nil && s.BranchTemplate != nil {
			bp = p + ".branch_template"
		}
		for wi, wg := range b.WriteScope {
			if !validWriteGlob(wg, s.BranchesFrom != nil) {
				bad(fmt.Sprintf("%s.write_scope[%d]", bp, wi), "invalid branch write_scope glob")
			}
			if s.BranchesFrom != nil && (s.BranchesFrom.Max == 0 || s.BranchesFrom.Max > 1) && !templateTokenPat.MatchString(wg) {
				bad(fmt.Sprintf("%s.write_scope[%d]", bp, wi), "branches_from write_scope glob must be parameterized with {{item}}")
			}
		}
		members := []string{}
		outputTypes := map[string]bool{}
		for n := range regions[b.ID] {
			members = append(members, n)
			if out := d.Stages[n].Output; out != "" {
				outputTypes[out] = true
				for _, m := range d.Stages[n].OutputMap {
					if m.As != "" {
						outputTypes[m.As] = true
					}
				}
			}
		}
		if d.Stages[s.Join].Output != "" {
			if b.Output == "" && len(outputTypes) > 1 {
				bad(bp+".output", "branch producing several artifact types requires output")
			}
			if b.Output != "" && !outputTypes[b.Output] {
				bad(bp+".output", "branch output must name an artifact type its region produces")
			}
		}
		sort.Strings(members)
		for _, n := range members {
			if o, taken := owner[n]; taken && o != b.ID {
				bad("stages."+n, "stage belongs to branches "+o+" and "+b.ID+"; branches are disjoint")
				continue
			}
			owner[n] = b.ID
			x := d.Stages[n]
			if x.Primitive == FanOut && b.WriteScope != nil {
				for _, inner := range EffectiveBranches(x) {
					for _, g := range inner.WriteScope {
						narrowed := false
						for _, outer := range b.WriteScope {
							if GlobScopeNarrows([]string{g}, []string{outer}) {
								narrowed = true
							}
						}
						if !narrowed && !strings.Contains(g, "{{") {
							bad("stages."+n+".write_scope", "nested branch scope widens enclosing branch")
						}
					}
				}
			}
			switch x.Primitive {
			case VcsCommit, Merge, Integrate, ExternalEffect:
				bad("stages."+n, "a fan-out branch holds no protected effects")
			}
			if ProducesChanges(d, x) {
				changeProducingBranches[bi] = true
				if !s.Parallel || len(b.WriteScope) == 0 || !branchOrPostJoinVerified(d, s, regions[b.ID], outgoing) {
					bad("stages."+n, "a fan-out branch holds no change producers without parallel: true and write_scope")
				}
			}
			for _, to := range outgoing[n] {
				if to == id || domain.State(to).Kernel() {
					bad("stages."+n, "a branch leaves only through its join or an Escalate stage")
				}
			}
		}
	}
	if s.BranchesFrom == nil && len(changeProducingBranches) > 0 {
		for i := 0; i < len(branches); i++ {
			for k := i + 1; k < len(branches); k++ {
				if !changeProducingBranches[i] && !changeProducingBranches[k] {
					continue
				}
				for _, g1 := range branches[i].WriteScope {
					for _, g2 := range branches[k].WriteScope {
						if GlobsOverlap(g1, g2) {
							bad(fmt.Sprintf("%s.branches[%d].write_scope", p, k), fmt.Sprintf("branch %s write_scope %q overlaps branch %s write_scope %q", branches[k].ID, g2, branches[i].ID, g1))
						}
					}
				}
			}
		}
	}
	for i, e := range d.Edges {
		if o, inside := owner[e.To]; inside && e.From != id && owner[e.From] != o {
			bad(fmt.Sprintf("edges[%d]", i), "branch "+o+" is entered only from its FanOut")
		}
		if e.To == s.Join && owner[e.From] == "" && !(s.BranchesFrom != nil && s.BranchesFrom.Empty == s.Join && e.From == id) {
			bad(fmt.Sprintf("edges[%d]", i), "join "+s.Join+" is entered only from its branches")
		}
	}
	for n, o := range owner {
		x := d.Stages[n]
		for i, in := range x.Inputs {
			if sibling, inside := owner[in.Stage]; inside && sibling != o {
				bad(fmt.Sprintf("stages.%s.inputs[%d]", n, i), "branch "+o+" may not bind sibling branch "+sibling)
			}
		}
		exprs := []Expr{}
		if x.Expr != nil {
			exprs = append(exprs, *x.Expr)
		}
		for _, c := range x.Cases {
			exprs = append(exprs, c.When)
		}
		for _, e := range exprs {
			if sibling := siblingRead(e, owner, o); sibling != "" {
				bad("stages."+n, "branch "+o+" may not read sibling branch "+sibling)
			}
		}
	}
	for i, b := range branches {
		if b.Pass == nil {
			continue
		}
		bp := fmt.Sprintf("%s.branches[%d].pass", p, i)
		if s.BranchesFrom != nil && s.BranchTemplate != nil {
			bp = p + ".branch_template.pass"
		}
		if err := ValidateExpr(*b.Pass); err != nil {
			bad(bp, err.Error())
		} else if !positiveCollections(*b.Pass) {
			bad(bp, "must assert positive evidence on every passing arm")
		}
		selectors(d, *b.Pass, bp, bad)
		if sibling := siblingRead(*b.Pass, owner, b.ID); sibling != "" {
			bad(bp, "branch "+b.ID+" may not read sibling branch "+sibling)
		}
	}
}

func branchOrPostJoinVerified(d Document, fan Stage, branchRegion map[string]bool, outgoing map[string][]string) bool {
	for n := range branchRegion {
		st := d.Stages[n]
		if (st.Primitive == HostCommand && (st.TestRun || st.Check != nil)) || st.Primitive == Review {
			return true
		}
	}
	if fan.Join == "" {
		return false
	}
	seen := map[string]bool{}
	stack := append([]string(nil), outgoing[fan.Join]...)
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		st, ok := d.Stages[cur]
		if !ok {
			continue
		}
		if (st.Primitive == HostCommand && (st.TestRun || st.Check != nil)) || st.Primitive == Review {
			return true
		}
		stack = append(stack, outgoing[cur]...)
	}
	return false
}

// siblingRead names a sibling branch whose stage x selects, if any.
func siblingRead(x Expr, owner map[string]string, branch string) string {
	if o, inside := owner[x.Stage]; inside && o != branch {
		return o
	}
	for _, a := range x.Args {
		if o := siblingRead(a, owner, branch); o != "" {
			return o
		}
	}
	return ""
}
