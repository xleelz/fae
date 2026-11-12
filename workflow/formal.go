package workflow

import (
	"fmt"
	"github.com/xleelz/fae/proof"
	"sort"
	"strings"
)

// FormalGateAccepts requires the parser-derived passed bit on every passing
// arm, using exactly the requested stage/type selector. A zero exit code or
// an outcome label alone cannot establish a successful proof.
func FormalGateAccepts(x Expr, stage, typ string) bool {
	var proves func(Expr) bool
	field := func(e Expr) bool {
		return e.Op == "field" && !e.All && e.Stage == stage && e.Type == typ && len(e.Path) == 1 && e.Path[0] == "passed"
	}
	literalTrue := func(e Expr) bool { v, ok := e.Value.(bool); return e.Op == "literal" && ok && v }
	proves = func(e Expr) bool {
		if len(e.Args) != 2 {
			return false
		}
		switch e.Op {
		case "and":
			return proves(e.Args[0]) || proves(e.Args[1])
		case "or":
			return proves(e.Args[0]) && proves(e.Args[1])
		case "eq":
			return field(e.Args[0]) && literalTrue(e.Args[1]) || field(e.Args[1]) && literalTrue(e.Args[0])
		}
		return false
	}
	return proves(x)
}
func formalSelectors(d Document, x Expr) []selector {
	formal := map[string]bool{}
	for id, s := range d.Stages {
		if s.Primitive == HostCommand && s.Formal != nil {
			formal["\x00"+id] = true
			formal[s.Output+"\x00"] = true
			formal[s.Output+"\x00"+id] = true
		}
	}
	found := map[selector]bool{}
	var walk func(Expr)
	walk = func(e Expr) {
		if (e.Op == "field" || e.Op == "count" || e.Op == "exists") && formal[e.Type+"\x00"+e.Stage] {
			found[selector{Type: e.Type, Stage: e.Stage}] = true
		}
		for _, a := range e.Args {
			walk(a)
		}
	}
	walk(x)
	out := []selector{}
	for sel := range found {
		out = append(out, sel)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Stage != out[j].Stage {
			return out[i].Stage < out[j].Stage
		}
		return out[i].Type < out[j].Type
	})
	return out
}

// FormalPositiveGate is supplemental: it never satisfies code-change testing,
// RED, review or protected-effect floors. Other evidence retains its normal
// non-vacuity requirements.
func FormalPositiveGate(d Document, x Expr) bool {
	selectors := formalSelectors(d, x)
	if len(selectors) == 0 || !positiveCollections(x) {
		return false
	}
	for _, sel := range selectors {
		if !FormalGateAccepts(x, sel.Stage, sel.Type) {
			return false
		}
	}
	return true
}
func validateFormal(d Document, bad func(string, string)) {
	for _, id := range signatureNames(d.Stages) {
		s := d.Stages[id]
		p := "stages." + id
		if s.Formal != nil {
			if s.Primitive != HostCommand {
				bad(p+".formal", "formal verification belongs only to HostCommand")
			}
			if s.TestRun || s.TestShape != "" || s.Check != nil || s.Generate != nil || s.Accept || len(s.Capture) > 0 {
				bad(p+".formal", "formal verification cannot combine test_run, test_shape, check, generate, capture or accept")
			}
			if err := proof.Validate(*s.Formal); err != nil {
				bad(p+".formal", err.Error())
			}

			seen := map[string]bool{}
			for i, source := range s.Formal.Sources {
				if !DeclaredPathSafe(source) || strings.ContainsAny(source, "*?[]") || seen[source] {
					bad(fmt.Sprintf("%s.formal.sources[%d]", p, i), "formal source must be a unique literal task-relative path")
				}
				seen[source] = true
			}
			if s.Output == "" {
				bad(p+".output", "formal verification requires a recorded artifact")
			}
		}
		if s.Primitive == Gate && s.Expr != nil {
			for _, sel := range formalSelectors(d, *s.Expr) {
				if !FormalGateAccepts(*s.Expr, sel.Stage, sel.Type) {
					bad(p+".expression", "formal gate must require passed == true on every passing arm for "+sel.label())
				}
				if sel.Stage == "" && sel.Type != "" {
					for source, producer := range d.Stages {
						if StageProducesType(producer, sel.Type) && producer.Formal == nil {
							bad(p+".expression", "formal type selector also names a non-formal producer: "+source)
						}
					}
				}
			}
		}
	}
}
