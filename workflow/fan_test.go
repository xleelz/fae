package workflow

import (
	"testing"
)

func approvedOnce(stage string) *Expr {
	x := bin("eq", Expr{Op: "count", Stage: stage, Args: []Expr{bin("eq", field(stage, "approved"), lit(true))}}, lit(1))
	return &x
}

// fanDoc runs two isolated reviews as parallel branches between the test gate
// and the commit, joined by an all-pass FanIn.
func fanDoc() Document {
	d := protected()
	d.Roles["security"] = d.Roles["reviewer"]
	d.Stages["fan"] = Stage{Primitive: FanOut, Branches: []FanBranch{{ID: "correctness", Start: "review", Pass: approvedOnce("review")}, {ID: "security", Start: "secure", Pass: approvedOnce("secure")}}, Join: "join"}
	d.Stages["secure"] = Stage{Primitive: Review, Role: "security", Output: "report"}
	d.Stages["join"] = Stage{Primitive: FanIn, Rule: JoinAll, Failure: "stop"}
	edges := []Edge{}
	for _, e := range d.Edges {
		switch e {
		case Edge{"gate", "review"}:
			edges = append(edges, Edge{"gate", "fan"})
		case Edge{"review", "commit"}:
			edges = append(edges, Edge{"review", "join"})
		default:
			edges = append(edges, e)
		}
	}
	d.Edges = append(edges, Edge{"fan", "review"}, Edge{"fan", "secure"}, Edge{"secure", "join"}, Edge{"join", "commit"}, Edge{"join", "stop"})
	return d
}

// FAE-WF-030 FanOut/FanIn conformance: branch and join configuration, branch
// disjointness and isolation, and the floors across branches.
func TestFanValidation(t *testing.T) {
	if issues := Validate(fanDoc()); len(issues) > 0 {
		t.Fatal(issues)
	}
	edit := func(stage string, f func(*Stage)) Document {
		d := fanDoc()
		s := d.Stages[stage]
		f(&s)
		d.Stages[stage] = s
		return d
	}
	edges := func(f func(*Document)) Document {
		d := fanDoc()
		f(&d)
		return d
	}
	for _, c := range []struct {
		name, path, message string
		d                   Document
	}{
		{"one branch", "stages.fan.branches", "between 2 and 16", edit("fan", func(s *Stage) { s.Branches = s.Branches[:1] })},
		{"duplicate branch", "stages.fan.branches[1].id", "unique branch id", edit("fan", func(s *Stage) { s.Branches[1].ID = "correctness" })},
		{"start is join", "stages.fan.branches[1].start", "distinct, existing", edit("fan", func(s *Stage) { s.Branches[1].Start = "join" })},
		{"extra fan edge", "stages.fan", "is not a branch start", edges(func(d *Document) { d.Edges = append(d.Edges, Edge{"fan", "commit"}) })},
		{"start without edge", "stages.fan", "needs an edge", edges(func(d *Document) {
			s := d.Stages["fan"]
			s.Branches = append(s.Branches, FanBranch{ID: "third", Start: "change"})
			d.Stages["fan"] = s
		})},
		{"join not a fan-in", "stages.fan.join", "must name a FanIn", edit("fan", func(s *Stage) { s.Join = "commit" })},
		{"unknown rule", "stages.join.rule", "all, any or k-of-n", edit("join", func(s *Stage) { s.Rule = "most" })},
		{"k-of-n without quorum", "stages.join.quorum", "between 1 and the 2 branches", edit("join", func(s *Stage) { s.Rule = KOfN })},
		{"quorum above branches", "stages.join.quorum", "between 1 and the 2 branches", edit("join", func(s *Stage) { s.Rule, s.Quorum = KOfN, 3 })},
		{"join without failure", "stages.join.failure", "failure route", edit("join", func(s *Stage) { s.Failure = "" })},
		{"rule on gate", "stages.gate.rule", "belongs to FanIn", edit("gate", func(s *Stage) { s.Rule = JoinAll })},
		{"branches on review", "stages.review", "belong to FanOut", edit("review", func(s *Stage) { s.Join = "join" })},
		{"shared stage", "stages.review", "branches are disjoint", edges(func(d *Document) { d.Edges = append(d.Edges, Edge{"secure", "review"}) })},
		{"entered from outside", "edges[11]", "entered only from its FanOut", edges(func(d *Document) { d.Edges = append(d.Edges, Edge{"gate", "secure"}) })},
		{"join from outside", "edges[11]", "entered only from its branches", edges(func(d *Document) { d.Edges = append(d.Edges, Edge{"gate", "join"}) })},
		{"branch reaches Done", "stages.secure", "leaves only through its join", edges(func(d *Document) { d.Edges = append(d.Edges, Edge{"secure", "Done"}) })},
		{"change producer in branch", "stages.fix", "no change producers", edges(func(d *Document) {
			d.Stages["fix"] = Stage{Primitive: AgentTurn, Role: "coder", Output: "report", ChangeProducer: true}
			for i, e := range d.Edges {
				if e == (Edge{"secure", "join"}) {
					d.Edges[i] = Edge{"secure", "fix"}
				}
			}
			d.Edges = append(d.Edges, Edge{"fix", "join"})
		})},
		{"commit in branch", "stages.commit", "no protected effects", edges(func(d *Document) {
			for i, e := range d.Edges {
				if e == (Edge{"secure", "join"}) {
					d.Edges[i] = Edge{"secure", "commit"}
				}
			}
		})},
		{"sibling input", "stages.secure.inputs[0]", "may not bind sibling branch correctness", edit("secure", func(s *Stage) {
			s.Inputs = []Input{{Name: "peer", Type: "report", Stage: "review", Version: 1}}
		})},
		{"sibling verdict", "stages.fan.branches[1].pass", "may not read sibling branch correctness", edit("fan", func(s *Stage) { s.Branches[1].Pass = approvedOnce("review") })},
		{"vacuous verdict", "stages.fan.branches[0].pass", "positive evidence", edit("fan", func(s *Stage) {
			x := Expr{Op: "not", Args: []Expr{bin("eq", field("review", "approved"), lit(false))}}
			s.Branches[0].Pass = &x
		})},
		// Branch facts are checked per path: a test gate in one branch and a
		// review in another never combine into commit evidence.
		{"split floors", "stages.commit", "without independent review and passing test gate", func() Document {
			d := fanDoc()
			delete(d.Stages, "review")
			f := d.Stages["fan"]
			f.Branches[0] = FanBranch{ID: "tests", Start: "test"}
			d.Stages["fan"] = f
			d.Edges = []Edge{{"change", "fan"}, {"fan", "test"}, {"fan", "secure"}, {"test", "gate"}, {"gate", "join"}, {"gate", "stop"}, {"secure", "join"}, {"join", "commit"}, {"join", "stop"}, {"commit", "Done"}}
			return d
		}()},
	} {
		expectIssue(t, c.name, c.d, c.path, c.message)
	}
	if !CanTransition(fanDoc(), "join", "secure") || CanTransition(fanDoc(), "join", "change") || CanTransition(fanDoc(), "commit", "secure") {
		t.Fatal("fan re-entry legality")
	}
}
