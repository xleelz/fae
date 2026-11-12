package engine_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
)

// FAE-WF-078 with the one declared-path rule: a scope widen naming a path
// inside .git at any depth, in any letter case, is a malformed request where
// the request is validated. It never reaches the judge, so no approval can
// put a path in the approved scope that the live resolver refuses on every
// later dispatch. Ordinary dot files still widen. A stored approved scope is
// still read with the relative-path rule, so a job holding an older nested
// widen loads (TestStoredRefusedWidenIsDroppedAndTheTaskContinues).
func TestScopeWidenRefusesNestedGitMetadata(t *testing.T) {
	for _, name := range []string{"sub/.git/config", "a/b/.git/hooks/pre-commit", ".GIT/config", "sub/.Git/HEAD", "sub/.git"} {
		bad, _ := json.Marshal(map[string]any{"request": map[string]any{"kind": "scope-widen", "files": []string{"pkg/extra.go", name}, "reason": "x"}})
		j, err := engine.New("task", 1, requestWorkflow(), "tree")
		if err != nil {
			t.Fatal(err)
		}
		j = drive(t, j, engine.NewScripted(map[string][]engine.ScriptStep{"work": steps(string(bad), string(bad), string(bad), string(bad))}), true)
		if j.EscalationKind != domain.SameGate || !strings.Contains(j.Reason, "MalformedOutput") || !strings.Contains(j.Reason, "inside .git") || j.Counts["judge"] != 0 || len(j.RequestCounts) != 0 || len(j.ApprovedScope) != 0 {
			t.Errorf("widen %q: %s %s judge=%d scope=%v", name, j.EscalationKind, j.Reason, j.Counts["judge"], j.ApprovedScope)
		}
	}
	dotted := `{"request":{"kind":"scope-widen","files":["docs/.nojekyll","sub/.gitignore"],"reason":"docs"}}`
	j, err := engine.New("task", 1, requestWorkflow(), "tree")
	if err != nil {
		t.Fatal(err)
	}
	j = drive(t, j, engine.NewScripted(map[string][]engine.ScriptStep{"work": steps(dotted, `{}`), "judge": steps(`{"approved":true}`)}), true)
	if j.State != domain.Done || len(j.ApprovedScope) != 2 {
		t.Fatal("dot-file widen", j.State, j.Reason, j.ApprovedScope)
	}
	stored := roundTrip(t, j)
	stored.ApprovedScope = []string{"pkg/extra.go", "sub/.git/config"}
	if err = engine.Validate(stored); err != nil {
		t.Fatal("a stored nested-.git approved scope no longer loads, so it cannot be retired:", err)
	}
}

// FAE-SEC-005 with FAE-WF-078: a widen is model chosen, so one naming a
// protected path (CI or forge configuration, a file that makes the operator's
// tools run programs, a credential-like file) is a malformed request that
// never reaches the judge, in any letter case and whatever the other files.
func TestScopeWidenRefusesProtectedPaths(t *testing.T) {
	for _, name := range []string{".github/workflows/release.yml", ".GitHub/CODEOWNERS", ".gitlab-ci.yml", "Jenkinsfile", ".claude/settings.json", ".vscode/tasks.json", ".envrc", "deploy/.env", "keys/server.pem", "home/.ssh/config", ".gitmodules"} {
		bad, _ := json.Marshal(map[string]any{"request": map[string]any{"kind": "scope-widen", "files": []string{"pkg/extra.go", name}, "reason": "x"}})
		j, err := engine.New("task", 1, requestWorkflow(), "tree")
		if err != nil {
			t.Fatal(err)
		}
		j = drive(t, j, engine.NewScripted(map[string][]engine.ScriptStep{"work": steps(string(bad), string(bad), string(bad), string(bad))}), true)
		if j.EscalationKind != domain.SameGate || !strings.Contains(j.Reason, "MalformedOutput") || !strings.Contains(j.Reason, "never adds a protected path") || j.Counts["judge"] != 0 || len(j.ApprovedScope) != 0 {
			t.Errorf("widen %q: %s %s judge=%d scope=%v", name, j.EscalationKind, j.Reason, j.Counts["judge"], j.ApprovedScope)
		}
	}
}

// FAE-WF-078: a job stored with an approved path a widen may no longer add
// (a pre-wave-9 widen into a nested .git, or a protected path approved before
// the protected-path rule) no longer refuses every later dispatch, which left
// Retire as the only way out. Before the next dispatch the engine drops the
// path from the approved scope with a scope-dropped event naming it and why;
// the dispatch's scope is the rest of the approved scope, the request counts
// are unchanged, and the task runs on to Done. A job with only allowed paths
// is untouched.
func TestStoredRefusedWidenIsDroppedAndTheTaskContinues(t *testing.T) {
	j, err := engine.New("task", 1, requestWorkflow(), "tree")
	if err != nil {
		t.Fatal(err)
	}
	if j, _, _, err = engine.Prepare(j); err != nil || j.State != "work" {
		t.Fatal("admission", j.State, err)
	}
	stored := roundTrip(t, j)
	stored.RequestCounts = map[string]int{"scope-widen": 2}
	stored.ApprovedScope = []string{".github/workflows/ci.yml", "pkg/extra.go", "sub/.git/config"}
	if err = engine.Validate(stored); err != nil {
		t.Fatal("the stored job does not load:", err)
	}
	next, d, events, err := engine.Prepare(stored)
	if err != nil || d == nil {
		t.Fatal("no dispatch", err)
	}
	if !slices.Equal(d.Scope, []string{"pkg/extra.go"}) || !slices.Equal(next.ApprovedScope, []string{"pkg/extra.go"}) || next.RequestCounts["scope-widen"] != 2 {
		t.Fatalf("dispatch scope %v, approved %v, counts %v", d.Scope, next.ApprovedScope, next.RequestCounts)
	}
	var dropped *engine.Event
	for i := range events {
		if events[i].Kind == "scope-dropped" {
			dropped = &events[i]
		}
	}
	if dropped == nil || !strings.Contains(dropped.Cause, "sub/.git/config (inside .git)") || !strings.Contains(dropped.Cause, ".github/workflows/ci.yml (CI or forge configuration)") || dropped.Stage != "work" {
		t.Fatalf("no scope-dropped event naming each path: %+v", events)
	}
	if err = engine.Validate(next); err != nil {
		t.Fatal(err)
	}
	r, err := engine.NewScripted(map[string][]engine.ScriptStep{"work": steps(`{}`)}).Execute(context.Background(), *d)
	if err != nil {
		t.Fatal(err)
	}
	if next, _, err = engine.Complete(next, d.ID, r); err != nil {
		t.Fatal(err)
	}
	done := drive(t, next, engine.NewScripted(nil), true)
	if done.State != "Done" || !slices.Equal(done.ApprovedScope, []string{"pkg/extra.go"}) {
		t.Fatal("the task did not continue", done.State, done.Reason, done.ApprovedScope)
	}

	clean := roundTrip(t, j)
	clean.ApprovedScope = []string{"pkg/extra.go"}
	after, _, events, err := engine.Prepare(clean)
	if err != nil || !slices.Equal(after.ApprovedScope, []string{"pkg/extra.go"}) || slices.ContainsFunc(events, func(e engine.Event) bool { return e.Kind == "scope-dropped" }) {
		t.Fatal("an allowed approved scope changed", after.ApprovedScope, events, err)
	}
}
