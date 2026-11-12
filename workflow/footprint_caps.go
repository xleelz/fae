package workflow

import "fmt"

// treeRoleCapBound is a conservative bound over pinned content. An unbounded
// or unknown used role leaves the bound absent; it never narrows the graph on
// the strength of a missing cap. Across alternative revisions the larger cap
// is safe even when only one of those revisions is currently active.
func treeRoleCapBound(d Document, resolve FootprintResolver, work *footprintWork) (int, bool, error) {
	caps := map[string]int{}
	taskCaps := map[string]int{}
	visited := map[string]bool{}
	bounded := true
	var collect func(Document, string) error
	collect = func(doc Document, key string) error {
		if visited[key] {
			return nil
		}
		visited[key] = true
		if err := work.step(); err != nil {
			return err
		}
		used := map[string]bool{}
		for _, stage := range doc.Stages {
			if err := work.step(); err != nil {
				return err
			}
			if stage.Primitive == AgentTurn {
				used[stage.Role] = true
			}
			if stage.Primitive == Review {
				for _, name := range stage.Reviewers {
					used[name] = true
				}
			}
		}
		for name := range used {
			role, ok := doc.Roles[name]
			if !ok || role.Cap < 1 {
				bounded = false
				continue
			}
			switch role.CapScope {
			case "task":
				k := doc.ID + "/" + name
				taskCaps[k] = max(taskCaps[k], role.Cap)
			case "tree":
				k := "tree/" + name
				caps[k] = max(caps[k], role.Cap)
			default:
				k := "workflow/" + doc.ID + "/" + name
				caps[k] = max(caps[k], role.Cap)
			}
		}
		for _, stage := range doc.Stages {
			if stage.Primitive != SubWorkflow || stage.Workflow == nil {
				continue
			}
			ref := *stage.Workflow
			if ref.ID == "self" || ref.ID == doc.ID {
				continue
			}
			if resolve == nil {
				return fmt.Errorf("footprint: pinned child %s@%d requires resolver", ref.ID, ref.Revision)
			}
			child, err := resolve(ref)
			if err != nil {
				return err
			}
			if err = collect(child, fmt.Sprintf("%s@%d", ref.ID, ref.Revision)); err != nil {
				return err
			}
		}
		return nil
	}
	if err := collect(d, d.ID); err != nil {
		return 0, false, err
	}
	if !bounded {
		return 0, false, nil
	}
	tasks := 1
	if d.Recursion != nil {
		tasks += d.Recursion.MaxDescendants
	} else {
		var count func(Document, map[string]bool) (int, error)
		count = func(doc Document, seen map[string]bool) (int, error) {
			if seen[doc.ID] {
				return 1000000, nil
			}
			seen[doc.ID] = true
			defer delete(seen, doc.ID)
			total := 1
			loopMultiplier := 1
			for _, stage := range doc.Stages {
				if stage.Primitive == Loop {
					loopMultiplier = capProduct(loopMultiplier, max(1, stage.MaxIterations))
				}
			}
			for stageID, stage := range doc.Stages {
				if stage.Primitive != SubWorkflow || stage.Workflow == nil {
					continue
				}
				if err := work.step(); err != nil {
					return 0, err
				}
				if stage.Workflow.ID == "self" || stage.Workflow.ID == doc.ID {
					return 1000000, nil
				}
				child, err := resolve(*stage.Workflow)
				if err != nil {
					return 0, err
				}
				n, err := count(child, seen)
				if err != nil {
					return 0, err
				}
				multiplier := loopMultiplier
				if stage.ItemsFrom != nil {
					children := 16
					if doc.Recursion != nil {
						children = doc.Recursion.MaxChildrenPerVisit
					}
					multiplier = capProduct(multiplier, children)
				}
				for fanID, fan := range doc.Stages {
					if fan.Primitive == FanOut && fan.BranchesFrom != nil && BranchOf(doc, fanID)[stageID] != "" {
						multiplier = capProduct(multiplier, max(1, fan.BranchesFrom.Max))
					}
				}
				total = min(1000000, total+capProduct(n, multiplier))
			}
			return total, nil
		}
		var err error
		tasks, err = count(d, map[string]bool{})
		if err != nil {
			return 0, false, err
		}
	}
	bound := 0
	for _, cap := range caps {
		bound = min(1000000, bound+cap)
	}
	for _, cap := range taskCaps {
		bound = min(1000000, bound+capProduct(cap, tasks))
	}
	return bound, true, nil
}
func capProduct(a, b int) int {
	if a > 1000000/max(1, b) {
		return 1000000
	}
	return a * b
}
