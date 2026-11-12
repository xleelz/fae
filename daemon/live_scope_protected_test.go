package daemon

import (
	"slices"
	"strings"
	"testing"

	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

// FAE-WF-078: an approved scope-widen path a widen may no longer add (inside
// .git at any depth, as a pre-wave-9 widen could be, or a protected path) is
// left out of the scope, derived or pinned, instead of refusing it. Before,
// the resolver refused every dispatch of a task that held one, so only Retire
// ended the task. The engine drops such a path from the job, with an event,
// before the next dispatch (TestStoredRefusedWidenIsDroppedAndTheTaskContinues);
// a reader of a job it has not prepared since, such as a landing, holds the
// same narrower scope. The task's other approved files stay in scope.
func TestLiveResolverLeavesOutRefusedApprovedPaths(t *testing.T) {
	approved := []string{".github/workflows/ci.yml", "extra.go", "sub/.git/config", ".git/hooks/pre-commit"}
	x := &liveExecutor{config: LiveConfig{Tasks: map[string]LiveTaskConfig{"pinned": {Files: []string{"a.go", "a_test.go"}, TestFile: "a_test.go", TestID: "TestA", ExpectedFailure: "want 42"}}}}
	derived, err := x.resolveLiveTaskScope("task", approved, workflow.Stage{}, func() (envelope, error) { return scopeEnvelope([]string{"a.go"}), nil })
	if err != nil || !slices.Equal(derived.Files, []string{"a.go", "extra.go"}) || derived.needFiles() != nil {
		t.Fatalf("derived scope %v: %v %v", derived.Files, err, derived.needFiles())
	}
	pinned, err := x.resolveLiveTaskScope("pinned", approved, workflow.Stage{}, nil)
	if err != nil || !slices.Equal(pinned.Files, []string{"a.go", "a_test.go", "extra.go"}) {
		t.Fatalf("pinned scope %v: %v", pinned.Files, err)
	}
}

// FAE-SEC-005: a derived scope never holds a protected path its Spec chose
// (CI or forge configuration, a file that makes the operator's tools run
// programs, a credential-like file). Before, any such path the Spec declared
// was in scope, read and sent to the provider and editable, and only spec
// review judged it. Now it is left out of the files, so no turn reads or
// sends it, and every change turn and VCS check is refused naming it and the
// remedy. Turns that change nothing still resolve, so a new spec turn can
// declare the files again. The operator's own admission declared files are
// authorized and kept, and a Go test fixture key under testdata is no
// credential.
func TestLiveDerivedScopeRefusesProtectedSpecPaths(t *testing.T) {
	x := &liveExecutor{}
	resolve := func(e envelope) liveTaskScope {
		t.Helper()
		s, err := x.resolveLiveTaskScope("task", nil, workflow.Stage{}, func() (envelope, error) { return e, nil })
		if err != nil {
			t.Fatal("a protected Spec path refused a dispatch that changes nothing:", err)
		}
		return s
	}
	for _, name := range []string{".github/workflows/ci.yml", ".GITHUB/CODEOWNERS", ".gitlab-ci.yml", ".circleci/config.yml", "Jenkinsfile", ".husky/pre-commit", ".pre-commit-config.yaml", ".envrc", ".devcontainer/devcontainer.json", ".vscode/tasks.json", ".claude/settings.json", ".codex/config.toml", ".mcp.json", "config/.env", ".env.production", "deploy/.npmrc", ".netrc", "keys/id_ed25519", "certs/server.pem", "certs/server.key", "home/.ssh/authorized_keys", "infra/.aws/credentials", "sub/.gitmodules"} {
		s := resolve(scopeEnvelope(nil, specEvidence("a.go", "a_test.go", name)))
		if slices.Contains(s.Files, name) || !slices.Equal(s.Files, []string{"a.go", "a_test.go"}) {
			t.Errorf("%s: protected Spec path in scope %v", name, s.Files)
		}
		refusal := s.needFiles()
		if !isScopeRefusal(refusal) || !strings.Contains(refusal.Error(), name) || !strings.Contains(refusal.Error(), "restart it with information") {
			t.Errorf("%s: change turns not refused naming it: %v", name, refusal)
		}
		if err := s.effectCheck(engine.Dispatch{Config: workflow.Stage{Primitive: workflow.VcsCommit}}); !isScopeRefusal(err) {
			t.Errorf("%s: commit not refused: %v", name, err)
		}
	}
	allowed := []string{"a.go", "a_test.go", ".gitignore", "docs/.nojekyll", ".env.example", "pkg/testdata/server.key", "github/workflow.go", "src/.github.go", "nested/.github/notes.md"}
	if s := resolve(scopeEnvelope(nil, specEvidence(protectedScopeAny(allowed)...))); !slices.Equal(s.Files, protectedScopeSorted(allowed)) || s.needFiles() != nil {
		t.Fatalf("ordinary paths refused: %v %v", s.Files, s.needFiles())
	}
	operator := []string{"a.go", "a_test.go", ".github/workflows/ci.yml"}
	if s := resolve(scopeEnvelope(operator)); !slices.Equal(s.Files, protectedScopeSorted(operator)) || s.needFiles() != nil {
		t.Fatalf("the operator's declared files were judged: %v %v", s.Files, s.needFiles())
	}
	// A new Spec without the path clears the refusal.
	s := resolve(scopeEnvelope(nil, specEvidence("a.go", ".github/workflows/ci.yml"), workflow.Evidence{Type: "Spec", Stage: "spec", Fields: map[string]any{"declared_files": []any{"a.go", "a_test.go"}}}))
	if !slices.Equal(s.Files, []string{"a.go", "a_test.go"}) || s.needFiles() != nil {
		t.Fatal("a new Spec did not clear the refusal", s.Files, s.needFiles())
	}
}

func protectedScopeAny(names []string) []any {
	out := make([]any, len(names))
	for i, name := range names {
		out[i] = name
	}
	return out
}

func protectedScopeSorted(names []string) []string {
	out := slices.Clone(names)
	slices.Sort(out)
	return out
}
