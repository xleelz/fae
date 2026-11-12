package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/engine"
)

// hangingRedSource is a RED whose test hangs instead of failing.
const hangingRedSource = "package sample\n\nimport (\n\t\"testing\"\n\t\"time\"\n)\n\nfunc TestBehavior(t *testing.T) {\n\tt.Log(\"waiting for the answer\")\n\ttime.Sleep(time.Hour)\n}\n"

// liveHungRedRun executes the live RED run (run-red) of a task whose RED test
// hangs, with the stage's command and deadline replaced by argv and seconds,
// and returns its result, its error and the failure excerpt it stored.
func liveHungRedRun(t *testing.T, argv []string, seconds int, expected string) (engine.Result, error, string) {
	t.Helper()
	return liveRedSourceRun(t, hangingRedSource, 0, argv, seconds, expected)
}

// liveRedSourceRun is liveHungRedRun for a RED file holding source, under the
// live config's max_output_bytes (0 for the default).
func liveRedSourceRun(t *testing.T, source string, maxOutput int, argv []string, seconds int, expected string) (engine.Result, error, string) {
	t.Helper()
	cfg, ctx, _ := liveDaemonFixture(t)
	cfg.Live.AllowedCommands = append(cfg.Live.AllowedCommands, argv)
	cfg.Live.ExpectedFailure = expected
	cfg.Live.MaxOutputBytes = maxOutput
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	d.mu.Lock()
	d.draining = true // no drive round dispatches the fixture task
	d.mu.Unlock()
	authorize(t, d)
	w, err := d.live.manager.Ensure(ctx, "effect-run", "effect-work", cfg.Live.Base)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(w.Path, "answer_test.go"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := d.live.broker.Snapshot(ctx, w, cfg.Live.Files)
	if err != nil {
		t.Fatal(err)
	}
	j := effectJob(t, snapshot.Tree, snapshot.Base, "run-red")
	payload, _ := json.Marshal(envelope{Job: j, Admission: AdmitParams{ID: j.TaskID, RunID: "effect-run", AuthorizingMessage: "message"}})
	stored, err := d.store.AdmitJob(ctx, controlplane.Audit{Actor: "test", Action: "admit hung RED fixture", Outcome: "ok"}, "hung-red-fixture", controlplane.JobSnapshot{ID: j.TaskID, WorkflowID: j.Document.ID, WorkflowRevision: 1, TargetID: cfg.Workspace, TargetRevision: 1, State: string(j.State), Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	d.jobs[j.TaskID] = stored
	d.mu.Unlock()
	dispatch := *j.InFlight
	if dispatch.Stage != "run-red" || dispatch.Config.TestShape != "go-unit" {
		t.Fatalf("fixture dispatch %+v", dispatch.Config)
	}
	dispatch.Config.Command, dispatch.Config.TimeoutSeconds = argv, seconds
	r, runErr := executeAdmitted(t, ctx, d.live, dispatch)
	excerpt := ""
	if raw, err := d.store.GetMarker(ctx, engineFeedbackKey(dispatch.Sessions[0].ID)); err == nil {
		var record engineFeedbackRecord
		if err = json.Unmarshal(raw, &record); err != nil {
			t.Fatal(err)
		}
		text, err := d.store.GetBlob(ctx, record.Excerpt)
		if err != nil {
			t.Fatal(err)
		}
		excerpt = string(text)
	}
	return r, runErr, excerpt
}

// A RED run whose test hangs completes as a failed run the RED gate can
// route, never an uncertain result (which parks the task UncertainDispatch
// for an operator). The broker gives the shaped run go test's own -timeout
// of half the stage's deadline: go test's alarm fires inside the run, the
// hung test is recorded as executed and failed with "test timed out" in its
// output, and that output is the rework turn's excerpt. Here the operator
// pinned "test timed out" as the expected failure, so the run is RED-gate
// evidence. When the stage's deadline itself kills the run (a vector that
// disables go test's alarm), the run is a timed-out failure (exit code -1)
// that proves no expected failure, still not uncertain; the test it cut off
// reported no result of its own and is not counted as executed.
// Needs Dolt and git.
func TestLiveHungRedRunIsATypedFailure(t *testing.T) {
	t.Run("go test timeout", func(t *testing.T) {
		r, err, excerpt := liveHungRedRun(t, []string{"go", "test", "-json", "-count=1", "./..."}, 20, "test timed out")
		var output map[string]any
		if err != nil || r.Class != "" || json.Unmarshal(r.Output, &output) != nil {
			t.Fatalf("hung RED run: class %q, error %v, output %s", r.Class, err, r.Output)
		}
		for field, want := range map[string]any{"exit_code": 1.0, "executed_count": 1.0, "matched_count": 1.0, "expected_failure": true, "test_in_source": true, "red_passed": false} {
			if output[field] != want {
				t.Errorf("%s = %v, want %v (%s)", field, output[field], want, r.Output)
			}
		}
		for _, want := range []string{"waiting for the answer", "panic: test timed out after 10s", "exit code 1, executed 1, matched 1"} {
			if !strings.Contains(excerpt, want) {
				t.Errorf("excerpt lacks %q:\n%s", want, excerpt)
			}
		}
	})
	t.Run("stage deadline", func(t *testing.T) {
		r, err, excerpt := liveHungRedRun(t, []string{"go", "test", "-json", "-count=1", "-timeout=0", "./..."}, 8, "waiting for the answer")
		var output map[string]any
		if err != nil || r.Class != "" || json.Unmarshal(r.Output, &output) != nil {
			t.Fatalf("killed RED run: class %q, error %v, output %s", r.Class, err, r.Output)
		}
		for field, want := range map[string]any{"exit_code": -1.0, "executed_count": 0.0, "matched_count": 0.0, "expected_failure": false, "red_passed": false} {
			if output[field] != want {
				t.Errorf("%s = %v, want %v (%s)", field, output[field], want, r.Output)
			}
		}
		if !strings.Contains(excerpt, "waiting for the answer") || !strings.Contains(excerpt, "killed at the run's deadline") {
			t.Errorf("excerpt:\n%s", excerpt)
		}
	})
}

// A RED test cut off from outside its own goroutine reported no result: here
// a SIGKILL from outside its test binary (as the OOM killer sends) and a
// TestMain goroutine's os.Exit. It is not counted as executed, and what it
// printed is not its expected failure, even when the operator pinned that
// text. go test ended on its own and reported the package's fail, so the run
// is a failed run the RED gate routes into its rework loop, not an uncertain
// result that parks the task UncertainDispatch. The excerpt says the test
// was not counted. Before (wave 10), the test counted as executed and failed
// and its printed text satisfied the RED gate. Needs Dolt and git.
func TestLiveRedRunCutOffFromOutsideIsATypedFailure(t *testing.T) {
	argv := []string{"go", "test", "-json", "-count=1", "./..."}
	for name, c := range map[string]struct{ source, printed string }{
		"killed":   {"package sample\n\nimport (\n\t\"os\"\n\t\"syscall\"\n\t\"testing\"\n\t\"time\"\n)\n\nfunc TestBehavior(t *testing.T) {\n\tgo func() { _ = syscall.Kill(os.Getpid(), syscall.SIGKILL) }()\n\ttime.Sleep(time.Minute)\n\tt.Fatal(\"got 0 want 42\")\n}\n", "signal: killed"},
		"TestMain": {"package sample\n\nimport (\n\t\"fmt\"\n\t\"os\"\n\t\"testing\"\n\t\"time\"\n)\n\nvar started = make(chan struct{})\n\nfunc TestMain(m *testing.M) {\n\tgo func() {\n\t\t<-started\n\t\tfmt.Println(\"fixture server crashed: bind: address already in use\")\n\t\tos.Exit(1)\n\t}()\n\tos.Exit(m.Run())\n}\n\nfunc TestBehavior(t *testing.T) {\n\tclose(started)\n\ttime.Sleep(time.Minute)\n\tt.Fatal(\"got 0 want 42\")\n}\n", "address already in use"},
	} {
		t.Run(name, func(t *testing.T) {
			r, err, excerpt := liveRedSourceRun(t, c.source, 0, argv, 60, c.printed)
			var output map[string]any
			if err != nil || r.Class != "" || json.Unmarshal(r.Output, &output) != nil {
				t.Fatalf("cut-off RED run: class %q, error %v, output %s", r.Class, err, r.Output)
			}
			for field, want := range map[string]any{"exit_code": 1.0, "executed_count": 0.0, "matched_count": 0.0, "expected_failure": false, "red_passed": false} {
				if output[field] != want {
					t.Errorf("%s = %v, want %v (%s)", field, output[field], want, r.Output)
				}
			}
			for _, want := range []string{c.printed, "TestBehavior reported no result of its own", "exit code 1, executed 0, matched 0"} {
				if !strings.Contains(excerpt, want) {
					t.Errorf("excerpt lacks %q:\n%s", want, excerpt)
				}
			}
		})
	}
}

// FAE-ROB-016 (HANDOFF next-work item 2): a RED run whose output passes the
// live config's max_output_bytes ended on its own, so it is evidence the RED
// gate routes, not an uncertain result that parks the task UncertainDispatch.
// Its test stream counts only what its kept head reports: a test whose own
// result came before the cut proves its expected failure; one whose result
// was cut off is not counted. Before, every run over the bound was an error
// and the task parked. Needs Dolt and git.
func TestLiveRedRunOverTheOutputBoundIsEvidence(t *testing.T) {
	argv := []string{"go", "test", "-json", "-count=1", "./..."}
	flood := "\t\tfor i := 0; i < 12000; i++ {\n\t\t\tfmt.Println(strings.Repeat(\"x\", 100))\n\t\t}\n"
	for name, c := range map[string]struct {
		source  string
		counted float64
	}{
		"result cut off":    {"package sample\n\nimport (\n\t\"fmt\"\n\t\"strings\"\n\t\"testing\"\n)\n\nfunc TestBehavior(t *testing.T) {\n\tif answer() != 42 {\n" + flood + "\t\tt.Fatal(\"got 0 want 42\")\n\t}\n}\n", 0},
		"result before cut": {"package sample\n\nimport (\n\t\"fmt\"\n\t\"os\"\n\t\"strings\"\n\t\"testing\"\n)\n\nfunc TestMain(m *testing.M) {\n\tcode := m.Run()\n\tif code != 0 {\n" + flood + "\t}\n\tos.Exit(code)\n}\n\nfunc TestBehavior(t *testing.T) {\n\tif answer() != 42 {\n\t\tt.Fatal(\"got 0 want 42\")\n\t}\n}\n", 1},
	} {
		t.Run(name, func(t *testing.T) {
			r, err, excerpt := liveRedSourceRun(t, c.source, 1<<20, argv, 60, "got 0 want 42")
			var output map[string]any
			if err != nil || r.Class != "" || json.Unmarshal(r.Output, &output) != nil {
				t.Fatalf("RED run over the bound: class %q, error %v, output %s", r.Class, err, r.Output)
			}
			for field, want := range map[string]any{"exit_code": 1.0, "executed_count": c.counted, "matched_count": c.counted, "expected_failure": c.counted == 1, "red_passed": false} {
				if output[field] != want {
					t.Errorf("%s = %v, want %v (%s)", field, output[field], want, r.Output)
				}
			}
			if !strings.Contains(excerpt, "output cut at the run's byte bound") {
				t.Errorf("excerpt:\n%s", excerpt)
			}
		})
	}
}
