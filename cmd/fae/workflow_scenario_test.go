package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/workflow"
)

// FAE-WF-091 and FAE-CLI-020: the scenario fae workflow scenario prints (the
// scripted adapter's built-in rehearsal) simulates offline against the
// workflow fae workflow default prints, Local projection included, to Done.
// Before, fae workflow simulate refused it ("json: unknown field output"),
// though the help lists the two forms together. A task-scoped key applies to
// its own task only, and a step typed with an outcome the simulator has no
// form for is refused by name.
func TestWorkflowScenarioRoundTripsThroughSimulate(t *testing.T) {
	dir := t.TempDir()
	print := func(name string, args ...string) string {
		t.Helper()
		var out bytes.Buffer
		if err := execute(context.Background(), args, &out); err != nil {
			t.Fatal(args, err)
		}
		file := filepath.Join(dir, name)
		if err := os.WriteFile(file, out.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
		return file
	}
	shipped := print("default.json", "workflow", "default")
	local := print("local.json", "workflow", "default", "--local")
	scenario := print("scenario.json", "workflow", "scenario")
	simulate := func(wf, script, task string) (workflow.Simulation, error) {
		var out bytes.Buffer
		err := execute(context.Background(), []string{"workflow", "simulate", wf, script, task}, &out)
		var s workflow.Simulation
		if err == nil {
			err = json.Unmarshal(out.Bytes(), &s)
		}
		return s, err
	}
	for _, wf := range []string{shipped, local} {
		s, err := simulate(wf, scenario, "demo")
		if err != nil {
			t.Fatalf("%s with the printed scenario: %v", filepath.Base(wf), err)
		}
		if s.State != domain.Done || len(s.Path) == 0 || s.Path[len(s.Path)-1] != "Done" || len(s.Gates) == 0 {
			t.Fatalf("%s: state %s %s, path %v", filepath.Base(wf), s.State, s.Reason, s.Path)
		}
	}

	// Task-scoped keys: demo's spec review rejects once, and another task's
	// unusable intake is never replayed for demo.
	raw, err := os.ReadFile(scenario)
	if err != nil {
		t.Fatal(err)
	}
	var steps map[string][]map[string]any
	if err = json.Unmarshal(raw, &steps); err != nil {
		t.Fatal(err)
	}
	approved := steps["spec-review"][0]
	reviews := approved["reviews"].([]any)
	rejected := map[string]any{"reviews": []any{map[string]any{"approved": false, "findings": []any{map[string]any{"finding": "untestable criterion", "severity": "blocking"}}}, reviews[1]}}
	steps["demo/spec-review"] = []map[string]any{rejected, approved}
	steps["spec"] = append(steps["spec"], steps["spec"][0])
	steps["other/intake"] = []map[string]any{{"output": "not an object"}}
	scoped := filepath.Join(dir, "scoped.json")
	if raw, err = json.Marshal(steps); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(scoped, raw, 0600); err != nil {
		t.Fatal(err)
	}
	s, err := simulate(shipped, scoped, "demo")
	if err != nil {
		t.Fatal("task-scoped scenario:", err)
	}
	if got := strings.Count(","+strings.Join(s.Path, ",")+",", ",spec-review,"); s.State != domain.Done || got != 2 {
		t.Fatalf("task-scoped scenario: state %s, spec-review %d times in %v", s.State, got, s.Path)
	}
	if _, err = simulate(shipped, scoped, "other"); err == nil || !strings.Contains(err.Error(), "intake visit 1") {
		t.Fatalf("another task's unusable intake was not refused for that task: %v", err)
	}

	// An outcome the simulator cannot replay is refused, naming the step.
	steps["intake"] = []map[string]any{{"class": "uncertain"}}
	delete(steps, "other/intake")
	typed := filepath.Join(dir, "typed.json")
	if raw, err = json.Marshal(steps); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(typed, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = simulate(shipped, typed, "demo"); err == nil || !strings.Contains(fmt.Sprint(err), "intake visit 1 is a uncertain outcome") {
		t.Fatalf("a typed step was not refused by name: %v", err)
	}
	// The simulator's own form is unchanged.
	example := filepath.Join("..", "..", "examples")
	if s, err = simulate(filepath.Join(example, "workflow.json"), filepath.Join(example, "script.json"), "demo"); err != nil || s.State != domain.Done {
		t.Fatalf("the simulator's own form: %v %s", err, s.State)
	}
}
