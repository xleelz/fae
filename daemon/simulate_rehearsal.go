package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

// rehearsalSimulationScript reads a scenario written for the scripted adapter
// (fae workflow scenario prints the built-in one; serve --scenario replays
// one) as the simulator's records, so fae workflow simulate and the
// WorkflowSimulate RPC take either form (FAE-WF-091). raw is canonical JSON;
// ok is false when it is not that form, and the caller keeps its own decode
// error.
//
// It replays what the scripted adapter would: a "task/stage" key applies to
// that task only, and a plain "stage" key to every task without its own.
// Each step's output, or each review of a Review stage, becomes one record of
// the stage's output artifact. Freeze and compare identities are computed by
// both from the declared test source, and gates, branches, loops, decisions
// and sub-workflows are evaluated or scripted in the simulator's own form, so
// their steps, and keys naming no stage of the simulated (Local) workflow,
// are not replayed. A step typed target-moved on an Integrate stage selects
// its reverify route; a step of any other type is refused, as the simulator
// has no such outcome. A step's delay is not simulated.
func rehearsalSimulationScript(d workflow.Document, task string, raw []byte) (script workflow.Script, inferred string, ok bool, err error) {
	var rehearsal map[string][]engine.ScriptStep
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&rehearsal) != nil || dec.Decode(new(any)) != io.EOF {
		return nil, "", false, nil
	}
	if task == "" {
		named := []string{}
		for key := range rehearsal {
			if t, _, scoped := strings.Cut(key, "/"); scoped && !slices.Contains(named, t) {
				named = append(named, t)
			}
		}
		if len(named) != 1 {
			return nil, "", true, errors.New("simulation task identity required: the scripted-adapter scenario names no single task; select one with --task")
		}
		task = named[0]
	}
	local := d
	if projected, e := workflow.LocalProjection(d); e == nil {
		local = projected
	}
	script = workflow.Script{}
	for key, steps := range rehearsal {
		stage := key
		if t, s, scoped := strings.Cut(key, "/"); scoped {
			if t != task {
				continue
			}
			stage = s
		} else if _, own := rehearsal[task+"/"+key]; own {
			continue
		}
		s, known := local.Stages[stage]
		if !known || !rehearsalReplayed(s.Primitive) {
			continue
		}
		version := local.Artifacts[s.Output].Version
		for visit, step := range steps {
			out := workflow.ScriptStep{}
			switch {
			case step.Class == "target-moved" && s.Primitive == workflow.Integrate:
				out.Next = s.Reverify
			case step.Class != "":
				return nil, task, true, fmt.Errorf("scenario %s visit %d is a %s outcome, which the simulator cannot replay", key, visit+1, step.Class)
			}
			outputs := step.Reviews
			if s.Primitive != workflow.Review {
				outputs = []json.RawMessage{step.Output}
			}
			if out.Next != "" {
				outputs = nil
			}
			for i, o := range outputs {
				var fields map[string]any
				dec := json.NewDecoder(bytes.NewReader(o))
				dec.UseNumber()
				if e := dec.Decode(&fields); e != nil || fields == nil {
					return nil, task, true, fmt.Errorf("scenario %s visit %d: output %d is not a JSON object the simulator can record", key, visit+1, i+1)
				}
				out.Evidence = append(out.Evidence, workflow.Evidence{ID: fmt.Sprintf("%s/%s/%d/%d", task, stage, visit+1, i+1), TaskID: task, Type: s.Output, Version: version, Stage: stage, Provenance: true, Fields: fields})
			}
			script[stage] = append(script[stage], out)
		}
	}
	return script, task, true, nil
}

// rehearsalReplayed reports the primitives whose scripted-adapter steps the
// simulator replays as records: every dispatching stage but freeze and
// compare, whose identities both compute.
func rehearsalReplayed(p workflow.Primitive) bool {
	switch p {
	case workflow.AgentTurn, workflow.Review, workflow.HostCommand, workflow.VcsCommit, workflow.Merge, workflow.Integrate, workflow.ExternalEffect:
		return true
	}
	return false
}
