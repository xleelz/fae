package workflow

import "fmt"

// A mutually recursive pinned graph has no finite static expansion. The root's
// descendant budget bounds live tasks; each task's own parallel peak is finite.
// Multiplying the largest such peak by the root family size is conservative
// without multiplying the same descendant budget at every cycle edge.
func recursiveTreePeak(d Document, resolve FootprintResolver, work *footprintWork) (int, error) {
	if d.Recursion == nil {
		return 0, fmt.Errorf("footprint: cyclic pinned child workflow requires a finite tree max_sessions or recursion budget")
	}
	seen := map[string]bool{}
	largest := 0
	var visit func(Document, string) error
	visit = func(doc Document, key string) error {
		if seen[key] {
			return nil
		}
		seen[key] = true
		if err := work.step(); err != nil {
			return err
		}
		own, err := treeGraphPeak(doc, func(Stage) (int, error) { return 0, nil }, work)
		if err != nil {
			return err
		}
		largest = max(largest, own)
		if d.Recursion.MaxDescendants == 0 {
			return nil
		}
		for _, stage := range doc.Stages {
			if stage.Primitive != SubWorkflow || stage.Workflow == nil || stage.Workflow.ID == "self" {
				continue
			}
			if resolve == nil {
				return fmt.Errorf("footprint: pinned child %s@%d requires resolver", stage.Workflow.ID, stage.Workflow.Revision)
			}
			child, err := resolve(*stage.Workflow)
			if err != nil {
				return err
			}
			if err = visit(child, fmt.Sprintf("%s@%d", stage.Workflow.ID, stage.Workflow.Revision)); err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit(d, d.ID); err != nil {
		return 0, err
	}
	if largest > 1000000/(d.Recursion.MaxDescendants+1) {
		return 0, fmt.Errorf("footprint: derived session count exceeds 1000000")
	}
	return largest * (d.Recursion.MaxDescendants + 1), nil
}
