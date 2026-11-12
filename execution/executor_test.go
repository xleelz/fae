package execution

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/runtime"
	"github.com/xleelz/fae/workflow"
)

func TestExecutionBridge(t *testing.T) {
	if os.Getenv("EXECUTION_HELPER") != "1" {
		return
	}
	var c runtime.BridgeConfig
	b, _ := os.ReadFile(os.Args[len(os.Args)-1])
	if json.Unmarshal(b, &c) != nil {
		os.Exit(2)
	}
	b, _ = json.Marshal(runtime.Handoff{DispatchID: c.DispatchID, SessionID: c.SessionID, TaskID: c.TaskID, Output: json.RawMessage(`{"pass":true}`)})
	if os.WriteFile(c.Handoff, b, 0600) != nil {
		os.Exit(3)
	}
	os.Exit(0)
}

type admission struct {
	acquired, released int
	reject             bool
}

func (a *admission) Acquire(context.Context, engine.Dispatch, Binding, engine.Session) (func() error, error) {
	a.acquired++
	if a.reject {
		return nil, errors.New("at capacity")
	}
	return func() error { a.released++; return nil }, nil
}

type effects struct{ called bool }

func (e *effects) Execute(context.Context, engine.Dispatch) (engine.Result, error) {
	e.called = true
	return engine.Result{}, nil
}
func fixture(t *testing.T) (*Executor, engine.Dispatch, *admission) {
	t.Helper()
	root, _ := filepath.EvalSymlinks(t.TempDir())
	exe, _ := os.Executable()
	a := &admission{}
	role := workflow.Role{Instructions: "instructions", Routes: []workflow.Binding{{Provider: "provider", Model: "model"}}}
	x := &Executor{Runtime: &runtime.Direct{Root: root, Command: []string{exe, "-test.run=^TestExecutionBridge$", "--", "{config}"}, Environment: map[string]string{"EXECUTION_HELPER": "1"}}, Resolve: func(context.Context, engine.Dispatch) (Binding, error) {
		return Binding{Worktree: root, Mode: "local", Document: workflow.Document{Roles: map[string]workflow.Role{"coder": role, "reviewer": role}, Artifacts: map[string]workflow.ArtifactType{"output": {Schema: json.RawMessage(`{"type":"object"}`)}}}}, nil
	}, Tree: func(context.Context, Binding) (string, error) { return "tree", nil }, Admission: a, CheckChange: func(context.Context, engine.Dispatch, Binding) error { return nil }}
	d := engine.Dispatch{ExecutionKind: "live", ID: "dispatch", TaskID: "task", Stage: "code", Tree: "tree", Config: workflow.Stage{Primitive: workflow.AgentTurn, Output: "output", Role: "coder"}, Sessions: []engine.Session{{ID: "one", Role: "coder"}}}
	return x, d, a
}
func TestAgentExecutionAndAdmission(t *testing.T) {
	x, d, a := fixture(t)
	r, e := x.Execute(context.Background(), d)
	if e != nil {
		t.Fatal(e)
	}
	if string(r.Output) != `{"pass":true}` || r.ExecutionKind != "live" || a.acquired != 1 || a.released != 1 {
		t.Fatal(r, a)
	}
}
func TestReviewIsolation(t *testing.T) {
	x, d, a := fixture(t)
	d.Config.Primitive = workflow.Review
	d.Sessions = append(d.Sessions, engine.Session{ID: "two", Role: "reviewer"})
	r, e := x.Execute(context.Background(), d)
	if e != nil {
		t.Fatal(e)
	}
	if len(r.Reviews) != 2 || r.Reviews[0].Session == r.Reviews[1].Session || a.acquired != 2 || a.released != 2 {
		t.Fatal(r, a)
	}
	entries, _ := os.ReadDir(x.Runtime.(*runtime.Direct).Root)
	var dirs []os.DirEntry
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "session-") {
			dirs = append(dirs, entry)
		}
	}
	if len(dirs) != 2 {
		t.Fatal("sessions not isolated", len(dirs))
	}
}
func TestTreeChanges(t *testing.T) {
	for _, producer := range []bool{false, true} {
		t.Run(map[bool]string{false: "readonly", true: "producer"}[producer], func(t *testing.T) {
			x, d, a := fixture(t)
			d.Config.ChangeProducer = producer
			calls := 0
			x.Tree = func(context.Context, Binding) (string, error) {
				calls++
				if calls == 1 {
					return "tree", nil
				}
				return "changed", nil
			}
			r, e := x.Execute(context.Background(), d)
			if producer {
				if e != nil || r.NewTree != "changed" {
					t.Fatal(r, e)
				}
			} else if e == nil || r.Class != engine.ClassRejectedChange {
				t.Fatal(r, e)
			}
			if a.released != 1 {
				t.Fatal("lease leaked")
			}
		})
	}
}
func TestNoLaunchWithoutLease(t *testing.T) {
	x, d, a := fixture(t)
	a.reject = true
	if _, e := x.Execute(context.Background(), d); e == nil {
		t.Fatal("accepted")
	}
	entries, _ := os.ReadDir(x.Runtime.(*runtime.Direct).Root)
	var dirs []os.DirEntry
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "session-") {
			dirs = append(dirs, entry)
		}
	}
	if len(dirs) != 0 || a.released != 0 {
		t.Fatal("launched without lease")
	}
}
func TestDispatchDelegationAndStaleTree(t *testing.T) {
	x, d, a := fixture(t)
	d.Tree = "wrong"
	if _, e := x.Execute(context.Background(), d); e == nil || a.acquired != 0 {
		t.Fatal("stale launch")
	}
	ef := &effects{}
	x.Effects = ef
	d.Config.Primitive = workflow.HostCommand
	if _, e := x.Execute(context.Background(), d); e != nil || !ef.called {
		t.Fatal(e)
	}
}
func TestMandatoryDependencies(t *testing.T) {
	x, d, _ := fixture(t)
	x.Admission = nil
	if _, e := x.Execute(context.Background(), d); e == nil {
		t.Fatal("missing lease accepted")
	}
}

func TestLeaseHeldThroughValidation(t *testing.T) {
	for _, failure := range []string{"none", "tree", "change"} {
		t.Run(failure, func(t *testing.T) {
			x, d, a := fixture(t)
			d.Config.ChangeProducer = true
			calls := 0
			x.Tree = func(context.Context, Binding) (string, error) {
				calls++
				if calls == 1 {
					return "tree", nil
				}
				if a.released != 0 {
					t.Fatal("lease released before tree measurement")
				}
				if failure == "tree" {
					return "", errors.New("tree failure")
				}
				return "changed", nil
			}
			x.CheckChange = func(context.Context, engine.Dispatch, Binding) error {
				if a.released != 0 {
					t.Fatal("lease released before policy check")
				}
				if failure == "change" {
					return errors.New("change refused")
				}
				return nil
			}
			_, e := x.Execute(context.Background(), d)
			if (e != nil) != (failure != "none") {
				t.Fatal(e)
			}
			if a.released != 1 {
				t.Fatal("lease not released exactly once", a.released)
			}
		})
	}
}

type lifecycleAdmission struct {
	events []string
	proof  bool
}

func (a *lifecycleAdmission) Acquire(context.Context, engine.Dispatch, Binding, engine.Session) (func() error, error) {
	a.events = append(a.events, "acquire")
	return func() error { a.events = append(a.events, "release"); return nil }, nil
}
func (a *lifecycleAdmission) Launching(context.Context, string) error {
	a.events = append(a.events, "launching")
	return nil
}
func (a *lifecycleAdmission) Retired(_ context.Context, _ string, proof bool) error {
	a.events = append(a.events, "retired")
	a.proof = proof
	return nil
}
func TestDurableAdmissionBoundaryOrder(t *testing.T) {
	x, d, _ := fixture(t)
	a := &lifecycleAdmission{}
	x.Admission = a
	if _, e := x.Execute(context.Background(), d); e != nil {
		t.Fatal(e)
	}
	want := []string{"acquire", "launching", "retired", "release"}
	if !a.proof || len(a.events) != len(want) {
		t.Fatal(a)
	}
	for i, e := range a.events {
		if e != want[i] {
			t.Fatal(a.events)
		}
	}
}

type refusedBeforeProviderRuntime struct{}

func (refusedBeforeProviderRuntime) Run(context.Context, runtime.Launch) (runtime.Collected, error) {
	return runtime.Collected{ProcessesRetired: true}, &runtime.TurnError{
		Class:      runtime.RefusedBeforeProvider,
		Role:       "coder",
		DispatchID: "dispatch",
		Cause:      &runtime.BridgeRefusal{Class: runtime.RefusedBeforeProvider, Detail: "scope check refused before provider"},
	}
}

// FAE-RT-004: when the bridge returns RefusedBeforeProvider and lifecycle.Retired
// fails, Execute still preserves ClassRefusedBeforeLaunch rather than downgrading
// the non-executed turn to uncertain.
func TestRefusedBeforeProviderPreservesRefusedBeforeLaunchOnRetiredFailure(t *testing.T) {
	x, d, _ := fixture(t)
	x.Admission = &failingReleaseAfterNonLaunchAdmission{failRetired: true}
	x.Runtime = refusedBeforeProviderRuntime{}
	r, err := x.Execute(context.Background(), d)
	if err == nil || r.Class != engine.ClassRefusedBeforeLaunch {
		t.Fatalf("class=%q err=%v", r.Class, err)
	}
}
