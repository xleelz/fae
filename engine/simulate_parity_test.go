package engine_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

// simulatorExcluded are the corpus runs whose recorded inputs lie outside the
// simulator's input language (role outputs, host-command results, a changed
// base or moved target, decisions): crashes, route health, operator verbs,
// the daemon's pane inventory, typed live executor refusals, unprojected
// content, and the instance's review lifecycle and dry-run disposition.
var simulatorExcluded = map[string]string{
	"routes-exhausted":                  "route eligibility is provider health, not a recorded result",
	"crash-recovery-exhausted":          "daemon crashes are not recorded results",
	"external-effect-outside-normal":    "unprojected shipped content outside Normal; the simulator pins the Local projection",
	"normal-live-delivery":              "the review lifecycle is instance configuration",
	"normal-review-remediation":         "the review lifecycle is instance configuration",
	"normal-review-revision-cap":        "the review lifecycle is instance configuration",
	"normal-review-polls":               "the review lifecycle is instance configuration",
	"normal-merge-barrier":              "the review lifecycle is instance configuration",
	"normal-dry-run":                    "a Normal simulation is live; a dry run is a disposition",
	"normal-effect-refused":             "the review lifecycle is instance configuration",
	"implement-stalls":                  "a stall is a typed live executor outcome",
	"stale-branch-residue":              "residue is a typed live executor outcome",
	"operator-escalates":                "an operator verb is not a recorded result",
	"pane-cap-starvation":               "pane starvation is the daemon's inventory proof",
	"no-red-violation":                  "the diff classifier refusal is a typed live executor outcome",
	"integration-moved-stale-approvals": "a moved target without a new base is a live executor outcome",
}

// FAE-WF-091 against FAE-WF-094: the simulator, fed each recorded run of the
// default as a scenario, takes exactly the recorded path to the recorded end,
// with the recorded gate results. Every run is compared or excluded above.
func TestSimulatorAgreesWithParityCorpus(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "parity", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatal("no parity corpus", err)
	}
	compared, seen := 0, map[string]bool{}
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var run parityRun
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err = dec.Decode(&run); err != nil {
			t.Fatal(file, err)
		}
		seen[run.Name] = true
		if _, skip := simulatorExcluded[run.Name]; skip {
			continue
		}
		compared++
		t.Run(run.Name, func(t *testing.T) {
			doc := parityDocument(t, run.Document)
			script, err := scenarioOf(doc, run)
			if err != nil {
				t.Fatal(err)
			}
			sim, err := engine.Simulate(doc, "parity", script, workflow.SimulationOptions{Normal: run.Disposition.Mode == domain.Normal})
			if err != nil {
				t.Fatal(err)
			}
			want := []string{}
			for _, tr := range run.Expected.Transitions {
				want = append(want, string(tr.From)+">"+string(tr.To))
			}
			got := []string{}
			for _, tr := range sim.Transitions {
				got = append(got, string(tr.From)+">"+string(tr.To))
			}
			if strings.Join(got, ",") != strings.Join(want, ",") || sim.State != run.Expected.State || !strings.HasPrefix(sim.Reason, string(run.Expected.Kind)) {
				t.Fatalf("simulated path diverged from the engine's recorded run\n got %s %s %q\nwant %s %s %s", strings.Join(got, ","), sim.State, sim.Reason, strings.Join(want, ","), run.Expected.State, run.Expected.Kind)
			}
			gates := []workflow.GateResult{}
			for _, g := range run.Expected.Gates {
				gates = append(gates, g.Result)
			}
			if a, b := canonical(t, sim.Gates), canonical(t, gates); !bytes.Equal(a, b) {
				t.Fatalf("simulated gate results diverged\n got %s\nwant %s", a, b)
			}
		})
	}
	for name := range simulatorExcluded {
		if !seen[name] {
			t.Errorf("excluded run %s is not in the corpus", name)
		}
	}
	if compared < 20 {
		t.Fatalf("only %d corpus runs compared", compared)
	}
}

// scenarioOf restates a recorded run as a simulator scenario: each output as
// a scripted record, a moved target or rebased merge as its reverify route.
func scenarioOf(doc workflow.Document, run parityRun) (workflow.Script, error) {
	script := workflow.Script{}
	for stage, steps := range run.Script {
		s := doc.Stages[stage]
		if s.Primitive == workflow.FreezeArtifact || s.Primitive == workflow.CompareFrozen {
			continue // the simulator computes identities like the scripted adapter
		}
		padded := append([]engine.ScriptStep(nil), steps...)
		for len(padded) < 64 {
			padded = append(padded, steps[len(steps)-1])
		}
		for visit, step := range padded {
			out := workflow.ScriptStep{}
			switch {
			case step.Class == "target-moved" && s.Primitive == workflow.Integrate:
				out.Next = s.Reverify
			case step.Class != "":
				return nil, fmt.Errorf("%s visit %d: typed outcome %s is not a simulator result", stage, visit+1, step.Class)
			}
			for _, rb := range run.Rebase[stage] {
				if rb.Visit == visit+1 {
					out.Next = s.Reverify
				}
			}
			outputs := step.Reviews
			if len(outputs) == 0 && step.Class == "" {
				outputs = []json.RawMessage{step.Output}
			}
			for i, o := range outputs {
				var fields map[string]any
				dec := json.NewDecoder(bytes.NewReader(o))
				dec.UseNumber()
				if err := dec.Decode(&fields); err != nil {
					return nil, fmt.Errorf("%s: %w", stage, err)
				}
				out.Evidence = append(out.Evidence, workflow.Evidence{ID: fmt.Sprintf("%s/%d/%d", stage, visit, i), TaskID: "parity", Type: s.Output, Version: doc.Artifacts[s.Output].Version, Stage: stage, Provenance: true, Fields: fields})
			}
			script[stage] = append(script[stage], out)
		}
	}
	return script, nil
}
