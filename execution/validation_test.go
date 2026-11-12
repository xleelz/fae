package execution

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/providerhealth"
	"github.com/xleelz/fae/runtime"
	"github.com/xleelz/fae/workflow"
)

// FAE-ROB-002 / FAE-WF-034: a completed turn's invalid report is a typed,
// bounded outcome, never an uncertain park. Unrecorded edits cannot be retried.
func TestLiveReportValidationClasses(t *testing.T) {
	cases := []struct {
		name, want         string
		schema             string
		producer, changed  bool
		check, observation error
		residue            bool
	}{
		{name: "schema unchanged tree", want: engine.ClassMalformedOutput, schema: `{"type":"object","required":["missing"]}`, producer: true},
		{name: "schema with edits", want: engine.ClassRejectedChange, schema: `{"type":"object","required":["missing"]}`, producer: true, changed: true},
		{name: "read-only edits", want: engine.ClassRejectedChange, changed: true},
		{name: "policy with edits", want: engine.ClassRejectedChange, producer: true, changed: true, check: errors.New("RED turn changed a non-test file")},
		{name: "policy unchanged tree", want: engine.ClassValidationFailed, producer: true, check: errors.New("implementation lacks frozen identity")},
		{name: "commit outside broker", want: engine.ClassRejectedChange, producer: true, residue: true},
		{name: "health unrecorded", want: "uncertain", schema: `{"type":"object","required":["missing"]}`, producer: true, observation: errors.New("plane down")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			x, d, a := fixture(t)
			d.Config.ChangeProducer = tc.producer
			if tc.schema != "" {
				resolve := x.Resolve
				x.Resolve = func(ctx context.Context, d engine.Dispatch) (Binding, error) {
					b, e := resolve(ctx, d)
					b.Document.Artifacts["output"] = workflow.ArtifactType{Schema: json.RawMessage(tc.schema)}
					return b, e
				}
			}
			calls := 0
			x.Tree = func(context.Context, Binding) (string, error) {
				calls++
				if calls > 1 && tc.residue {
					return "", &ResidueError{Detail: "agent changed task branch HEAD outside VCS broker"}
				}
				if calls > 1 && tc.changed {
					return "changed", nil
				}
				return "tree", nil
			}
			x.CheckChange = func(context.Context, engine.Dispatch, Binding) error { return tc.check }
			var observed []providerhealth.Outcome
			x.Observe = func(_ context.Context, _ engine.Dispatch, _ engine.Session, o providerhealth.Outcome) error {
				observed = append(observed, o)
				return tc.observation
			}
			r, e := x.Execute(context.Background(), d)
			if e == nil || r.Class != tc.want || len(r.Output) != 0 || r.NewTree != "" || a.released != 1 {
				t.Fatalf("class %q err %v output %s released %d", r.Class, e, r.Output, a.released)
			}
			if tc.want != "uncertain" && r.Detail == "" {
				t.Fatal("typed outcome lacks its cause")
			}
			if tc.schema != "" && (len(observed) != 1 || observed[0] != providerhealth.MalformedOutput) {
				t.Fatal("malformed output did not feed provider health", observed)
			}
		})
	}
}

func TestStaleResidueRefusesBeforeLaunch(t *testing.T) {
	for name, tree := range map[string]func(context.Context, Binding) (string, error){
		"tree":   func(context.Context, Binding) (string, error) { return "other", nil },
		"commit": func(context.Context, Binding) (string, error) { return "", &ResidueError{Detail: "HEAD moved"} },
	} {
		t.Run(name, func(t *testing.T) {
			x, d, a := fixture(t)
			x.Tree = tree
			r, e := x.Execute(context.Background(), d)
			if e == nil || r.Class != engine.ClassStaleResidue || r.Detail == "" || a.acquired != 0 {
				t.Fatal(r.Class, e, a.acquired)
			}
		})
	}
}

func TestConflictRemediationOwnHunkLossIsTyped(t *testing.T) {
	x, d, _ := fixture(t)
	d.Config.ChangeProducer = true
	calls := 0
	x.Tree = func(context.Context, Binding) (string, error) {
		if calls++; calls > 1 {
			return "changed", nil
		}
		return "tree", nil
	}
	x.CheckChange = func(context.Context, engine.Dispatch, Binding) error {
		return &OwnHunkLossError{Detail: "answer.go"}
	}
	r, e := x.Execute(context.Background(), d)
	if e == nil || r.Class != engine.ClassOwnHunkLoss || r.Detail != "own-hunk-loss: answer.go" {
		t.Fatal(r.Class, r.Detail, e)
	}
}

type throttledRuntime struct {
	dir     string
	retired bool
}

func (r throttledRuntime) Run(context.Context, runtime.Launch) (runtime.Collected, error) {
	return runtime.Collected{LogDirectory: r.dir, ProcessesRetired: r.retired}, errors.New("bridge exited 1")
}

// FAE-RT-010: a throttled turn on an untouched tree is a typed failover, after
// the binding's health is recorded; unproven retirement stays uncertain.
func TestThrottledTurnFailsOver(t *testing.T) {
	for _, retired := range []bool{true, false} {
		x, d, _ := fixture(t)
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte(`{"type":"error","error":{"status":429,"message":"rate_limit exceeded"}}`+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		x.Runtime = throttledRuntime{dir: dir, retired: retired}
		var observed []providerhealth.Outcome
		x.Observe = func(_ context.Context, _ engine.Dispatch, _ engine.Session, o providerhealth.Outcome) error {
			observed = append(observed, o)
			return nil
		}
		r, e := x.Execute(context.Background(), d)
		want := engine.ClassProviderFailover
		if !retired {
			want = "uncertain"
		}
		if e == nil || r.Class != want || len(observed) != 1 || observed[0] != providerhealth.RateLimited {
			t.Fatal(retired, r.Class, observed, e)
		}
	}
}
