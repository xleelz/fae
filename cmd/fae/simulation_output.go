package main

import (
	"encoding/json"
	"fmt"
	"github.com/xleelz/fae/daemon"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/workflow"
	"io"
	"sort"
)

func catalogWithScenario(raw []byte, catalog map[string]workflow.Document) map[string]workflow.Document {
	out := map[string]workflow.Document{}
	var s workflow.Scenario
	_ = json.Unmarshal(raw, &s)
	for k, v := range s.Workflows {
		out[k] = v
	}
	for k, v := range catalog {
		out[k] = v
	}
	return out
}
func mergeSimulationCatalog(raw []byte, catalog map[string]workflow.Document) ([]byte, error) {
	if len(catalog) == 0 {
		return raw, nil
	}
	var s map[string]json.RawMessage
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	if _, ok := s["rules"]; !ok {
		if _, ok = s["workflows"]; !ok {
			return nil, fmt.Errorf("--catalog simulation requires a scripted-adapter scenario with rules")
		}
	}
	var existing map[string]workflow.Document
	if b, ok := s["workflows"]; ok {
		if err := json.Unmarshal(b, &existing); err != nil {
			return nil, err
		}
	}
	if existing == nil {
		existing = map[string]workflow.Document{}
	}
	for k, v := range catalog {
		if old, ok := existing[k]; ok {
			a, _ := json.Marshal(old)
			b, _ := json.Marshal(v)
			if string(a) != string(b) {
				return nil, fmt.Errorf("conflicting catalog workflow %s", k)
			}
		}
		existing[k] = v
	}
	s["workflows"], _ = json.Marshal(existing)
	return json.Marshal(s)
}
func simulationResult(result workflow.Simulation, tree bool, out io.Writer) error {
	if tree {
		renderSimulationTree(out, result)
	} else if err := jsonOut(out, result); err != nil {
		return err
	}
	if result.State != domain.Done {
		return fmt.Errorf("simulation root ended %s: %s", result.State, result.Reason)
	}
	return nil
}
func renderSimulationTree(out io.Writer, result workflow.Simulation) {
	for _, n := range result.Tree {
		fmt.Fprintf(out, "%s %s@%d state=%s depth=%d detached=%t", n.Task, n.Workflow, n.Revision, n.State, n.Depth, n.Detached)
		if n.Reason != "" {
			fmt.Fprintf(out, " reason=%s", n.Reason)
		}
		fmt.Fprintln(out)
		for _, r := range n.IncomingRequests {
			fmt.Fprintf(out, "  change-request %s\n", r)
		}
		for _, r := range n.FailureRoutes {
			raw, _ := json.Marshal(r)
			fmt.Fprintf(out, "  failure-route %s\n", raw)
		}
		for _, r := range n.Refusals {
			raw, _ := json.Marshal(r)
			fmt.Fprintf(out, "  refusal %s\n", raw)
		}
		for _, b := range n.Branches {
			fmt.Fprintf(out, "  branch %s/%s visit=%d state=%s outcome=%s\n", b.FanStage, b.Path, b.Visit, b.State, b.Outcome)
		}
	}
	for _, r := range result.Refusals {
		fmt.Fprintf(out, "  refusal parent=%s task=%s reason=%s\n", r.Parent, r.Task, r.Reason)
	}
}
func renderRunTree(out io.Writer, page daemon.TreePage) {
	nodes := append([]daemon.TreeNode(nil), page.Nodes...)
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].Depth != nodes[j].Depth {
			return nodes[i].Depth < nodes[j].Depth
		}
		return nodes[i].ID < nodes[j].ID
	})
	for _, n := range nodes {
		fmt.Fprintf(out, "%s %s@%d state=%s depth=%d detached=%t", n.ID, n.Workflow, n.Revision, n.State, n.Depth, n.Detached)
		if n.Reason != "" {
			fmt.Fprintf(out, " reason=%s", n.Reason)
		}
		fmt.Fprintln(out)
		if n.ChangeRequests != nil {
			raw, _ := json.Marshal(n.ChangeRequests)
			if string(raw) != "[]" {
				fmt.Fprintf(out, "  change-requests %s\n", raw)
			}
		}
		for _, r := range n.FailureRoutes {
			raw, _ := json.Marshal(r)
			fmt.Fprintf(out, "  failure-route %s\n", raw)
		}
		for _, r := range n.Refusals {
			raw, _ := json.Marshal(r)
			fmt.Fprintf(out, "  refusal %s\n", raw)
		}
		for _, e := range n.RecursionRefusals {
			fmt.Fprintf(out, "  refusal %s %s\n", e.Stage, e.Cause)
		}
	}
}
