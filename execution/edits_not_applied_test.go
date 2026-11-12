package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/providerhealth"
	"github.com/xleelz/fae/runtime"
	"github.com/xleelz/fae/workflow"
)

// FAE-ROB-016, FAE-VCS-003, FAE-RT-009: the bridge's edits-not-applied
// refusal (it could not write a completed turn's edits and restored the
// worktree) is a ValidationFailed retry. The provider answered, so its health
// is observed as answering (ok, unless its own log shows a failure); the
// failed write is the bridge's. It is rejected-change if the tree moved after
// all, and uncertain without proven retirement or a recorded observation.
func TestEditsNotAppliedClassing(t *testing.T) {
	refusal := &runtime.TurnError{Class: "edits-not-applied", Role: "coder", DispatchID: "dispatch", Cause: &runtime.BridgeRefusal{Class: "edits-not-applied", Detail: `the provider's edits were not applied and the worktree is as the turn found it: edit "c.go": permission denied`}}
	for _, tc := range []struct {
		name, want                     string
		retired, changed, observeFails bool
	}{
		{"tree restored", engine.ClassValidationFailed, true, false, false},
		{"tree moved", engine.ClassRejectedChange, true, true, false},
		{"retirement unproven", "uncertain", false, false, false},
		{"health write failed", "uncertain", true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x, d, a := fixture(t)
			d.Config.ChangeProducer = true
			x.Runtime = refusingRuntime{dir: t.TempDir(), err: refusal, retired: tc.retired}
			calls := 0
			x.Tree = func(context.Context, Binding) (string, error) {
				if calls++; calls > 1 && tc.changed {
					return "changed", nil
				}
				return "tree", nil
			}
			var observed []providerhealth.Outcome
			x.Observe = func(_ context.Context, _ engine.Dispatch, _ engine.Session, o providerhealth.Outcome) error {
				observed = append(observed, o)
				if tc.observeFails {
					return errors.New("plane unavailable")
				}
				return nil
			}
			r, e := x.Execute(context.Background(), d)
			if e == nil || r.Class != tc.want || a.released != 1 || fmt.Sprint(observed) != fmt.Sprint([]providerhealth.Outcome{providerhealth.OK}) {
				t.Fatalf("class %q err %v observed %v released %d", r.Class, e, observed, a.released)
			}
			if tc.want != "uncertain" && !strings.Contains(r.Detail, `edit "c.go": permission denied`) {
				t.Fatal("typed outcome lacks its cause", r.Detail)
			}
		})
	}
}

// worktreeDigest is a test's tree identity: every file's path, mode and
// content under root, .git aside.
func worktreeDigest(root string) (string, error) {
	var lines []string
	e := filepath.WalkDir(root, func(path string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.Name() == ".git" {
			return filepath.SkipDir
		}
		rel, _ := filepath.Rel(root, path)
		info, e := d.Info()
		if e != nil {
			return e
		}
		line := rel + " " + info.Mode().String()
		if info.Mode().IsRegular() {
			data, e := os.ReadFile(path)
			if e != nil {
				return e
			}
			line += " " + string(data)
		}
		lines = append(lines, line)
		return nil
	})
	sort.Strings(lines)
	return strings.Join(lines, "\n"), e
}

// FAE-ROB-012, FAE-ROB-016, FAE-VCS-003 through the shipped chain (Executor,
// Direct, the production bridge and its exit status): a completed turn whose
// third of four edits cannot be written leaves the worktree exactly as it
// was and ends ValidationFailed, a bounded retry; the provider, which
// answered, is observed ok.
// Before, the first two edits stayed written with no handoff, and the turn
// was uncertain: the task parked UncertainDispatch, and since the tree no
// longer matched the checkpoint only Retire or manual cleanup recovered it.
func TestBridgeEditBatchFailureIsATypedRetryThroughDirect(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("a read-only directory does not stop root")
	}
	worktree, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	root, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	git := exec.Command("git", "init", "-q", worktree)
	git.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	if out, e := git.CombinedOutput(); e != nil {
		t.Fatal(e, string(out))
	}
	for name, content := range map[string]string{"answer.go": "original", "b.go": "b original"} {
		if e = os.WriteFile(filepath.Join(worktree, name), []byte(content), 0600); e != nil {
			t.Fatal(e)
		}
	}
	ro := filepath.Join(worktree, "ro")
	if e = os.Mkdir(ro, 0500); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = os.Chmod(ro, 0700) })
	scratch := t.TempDir()
	cli, answer, mark := filepath.Join(scratch, "claude"), filepath.Join(scratch, "answer.json"), filepath.Join(scratch, "ran")
	if e = os.WriteFile(cli, []byte(executionProviderCLI), 0700); e != nil {
		t.Fatal(e)
	}
	edits := `{"output":{"accepted":true},"edits":[` +
		`{"path":"answer.go","content":"rewritten","replacements":[]},` +
		`{"path":"b.go","content":null,"replacements":[{"old":"original","new":"replaced"}]},` +
		`{"path":"ro/c.go","content":"cannot be written","replacements":[]},` +
		`{"path":"sub/new.go","content":"new file","replacements":[]}]}`
	if e = os.WriteFile(answer, []byte(edits), 0600); e != nil {
		t.Fatal(e)
	}
	exe, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	route := workflow.Binding{Provider: "claude", Model: "fixture-model"}
	role := workflow.Role{Instructions: "implement", Routes: []workflow.Binding{route}, Profile: workflow.Profile{Allowed: []string{"handoff", "read", "edit"}}}
	environment := map[string]string{"EXECUTION_PROVIDER_BRIDGE": "1", "EXECUTION_PROVIDER_CLI": cli, "EXECUTION_PROVIDER_ANSWER": answer, "EXECUTION_PROVIDER_MARK": mark, "CLAUDE_CODE_OAUTH_TOKEN": "fixture-token", "GORACE": "atexit_sleep_ms=0"}
	document := workflow.Document{Roles: map[string]workflow.Role{"coder": role}, Artifacts: map[string]workflow.ArtifactType{"output": {Schema: json.RawMessage(`{"type":"object","required":["accepted"],"additionalProperties":false,"properties":{"accepted":{"type":"boolean"}}}`)}}}
	before, e := worktreeDigest(worktree)
	if e != nil {
		t.Fatal(e)
	}
	a := &admission{}
	var observed []providerhealth.Outcome
	x := &Executor{
		Runtime: &runtime.Direct{Root: root, Command: []string{exe, "-test.run=^TestExecutionProviderBridge$", "--", "{config}"}, Environment: environment},
		Resolve: func(context.Context, engine.Dispatch) (Binding, error) {
			return Binding{Worktree: worktree, Mode: "local", Document: document, Scope: TaskScope{Files: []string{"answer.go", "b.go", "ro/c.go", "sub/new.go"}}}, nil
		},
		Tree:        func(context.Context, Binding) (string, error) { return worktreeDigest(worktree) },
		Admission:   a,
		CheckChange: func(context.Context, engine.Dispatch, Binding) error { return nil },
		Observe: func(_ context.Context, _ engine.Dispatch, _ engine.Session, o providerhealth.Outcome) error {
			observed = append(observed, o)
			return nil
		},
	}
	d := engine.Dispatch{ExecutionKind: "live", ID: "dispatch", TaskID: "task", Stage: "implement", Tree: before, Config: workflow.Stage{Primitive: workflow.AgentTurn, Output: "output", Role: "coder", ChangeProducer: true}, Sessions: []engine.Session{{ID: "one", Role: "coder"}}}
	r, e := x.Execute(context.Background(), d)
	if e == nil || r.Class != engine.ClassValidationFailed || !strings.Contains(r.Detail, "bridge could not apply the provider's edits and restored the worktree") || !strings.Contains(r.Detail, `edit "ro/c.go"`) || r.NewTree != "" {
		t.Fatalf("class %q detail %q err %v", r.Class, r.Detail, e)
	}
	if _, e := os.Stat(mark); e != nil {
		t.Fatal("the provider did not run", e)
	}
	if fmt.Sprint(observed) != fmt.Sprint([]providerhealth.Outcome{providerhealth.OK}) || a.released != 1 {
		t.Fatalf("observed %v released %d", observed, a.released)
	}
	after, e := worktreeDigest(worktree)
	if e != nil || after != before {
		t.Fatal(fmt.Sprintf("the worktree changed:\nbefore:\n%s\nafter:\n%s", before, after), e)
	}
}
