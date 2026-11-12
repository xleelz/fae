package engine_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

// FAE-WF-008 acceptance: a search finds one transition-legality function. The
// only assignments of a task's State are enter (which asks
// workflow.CanTransition), the held-completion projection, which restores
// the held state before returning, and Simulate, which copies a finished
// offline run's state into its result (not a task's state).
func TestOneTransitionLegalityFunction(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]int{"enter": 1, "Complete": 2, "Simulate": 1}
	seen := map[string]int{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if a, ok := n.(*ast.AssignStmt); ok {
					for _, lhs := range a.Lhs {
						if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "State" {
							seen[fn.Name.Name]++
							if allowed[fn.Name.Name] == 0 {
								t.Errorf("%s: %s assigns task state outside enter", name, fn.Name.Name)
							}
						}
					}
				}
				return true
			})
		}
	}
	for fn, n := range allowed {
		if seen[fn] != n {
			t.Errorf("%s assigns state %d times, want %d", fn, seen[fn], n)
		}
	}
}

// Every task control, from every source state, either refuses unchanged or
// moves along an edge workflow.CanTransition accepts, recorded as a transition.
func TestEveryControlMoveIsAKernelEdge(t *testing.T) {
	base := newJob(t)
	sources := []domain.State{domain.Pending, domain.Blocked, domain.Escalated}
	for id := range base.Document.Stages {
		sources = append(sources, domain.State(id))
	}
	controls := []engine.Control{{Method: "Escalate", Reason: "r"}, {Method: "Block", Reason: "r"}, {Method: "Cancel", Reason: "r"}, {Method: "Retire", Reason: "obsolete ruling-1"}, {Method: "RestartWithInformation", Information: "i"}, {Method: "Resume"}, {Method: "Defer"}, {Method: "ResumeWithContext", Context: "c"}}
	moves := 0
	for _, from := range sources {
		for _, c := range controls {
			j := newJob(t)
			j.State = from
			if from == domain.Escalated {
				j.EscalationKind, j.Reason, j.AttemptCapStage, j.TaskAttemptLimit = domain.AttemptCap, "AttemptCap", "plan", 30
			}
			if from == domain.Blocked {
				j.BlockedFrom, j.BlockedRoute = "spec-gate", j.Document.Stages["spec-gate"].Failure
			}
			out, events, err := engine.ApplyControl(j, c, time.Now())
			if err != nil || out.State == from {
				continue
			}
			moves++
			if !workflow.CanTransition(j.Document, from, out.State) {
				t.Errorf("%s from %s reached %s outside the kernel graph", c.Method, from, out.State)
			}
			if len(events) == 0 || events[0].Kind != "transition" || events[0].From != from || events[0].To != out.State {
				t.Errorf("%s from %s: no transition record %v", c.Method, from, events)
			}
		}
	}
	if moves < len(sources) {
		t.Fatal("too few moves exercised", moves)
	}
}
