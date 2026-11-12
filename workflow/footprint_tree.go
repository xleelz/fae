package workflow

import "fmt"

// A profile maps the number of children a visit may start to its greatest
// concurrent session count. Parallel branches share a child budget instead of
// each multiplying the entire budget independently.
type treePeakProfile []int

func treeGraphPeak(d Document, child func(Stage) (int, error), work *footprintWork) (int, error) {
	budget := 16
	if d.Recursion != nil {
		budget = d.Recursion.MaxChildrenPerVisit
	}
	budget = max(1, min(256, budget))
	edges := map[string][]string{}
	for _, e := range d.Edges {
		edges[e.From] = append(edges[e.From], e.To)
	}
	alternatives := func(a, b treePeakProfile) treePeakProfile {
		n := max(len(a), len(b))
		out := make(treePeakProfile, n)
		for i := range n {
			out[i] = max(a[min(i, len(a)-1)], b[min(i, len(b)-1)])
		}
		return out
	}
	parallel := func(a, b treePeakProfile) (treePeakProfile, error) {
		if err := work.allocate(len(a) * len(b)); err != nil {
			return nil, err
		}
		out := make(treePeakProfile, min(budget+1, len(a)+len(b)-1))
		for i, av := range a {
			for j, bv := range b {
				if i+j < len(out) {
					out[i+j] = max(out[i+j], av+bv)
				}
			}
		}
		for i := 1; i < len(out); i++ {
			out[i] = max(out[i], out[i-1])
		}
		return out, nil
	}
	var visit func(string, string, map[string]bool) (treePeakProfile, error)
	visit = func(id, stop string, seen map[string]bool) (treePeakProfile, error) {
		if err := work.step(); err != nil {
			return nil, err
		}
		if id == stop || id == "Done" || seen[id] {
			return treePeakProfile{0}, nil
		}
		stage, ok := d.Stages[id]
		if !ok {
			return treePeakProfile{0}, nil
		}
		seen[id] = true
		defer delete(seen, id)
		own := treePeakProfile{0}
		switch stage.Primitive {
		case AgentTurn:
			own[0] = 1
		case Review:
			own[0] = max(1, stage.Count) * max(1, len(stage.Reviewers))
		case SubWorkflow:
			n, err := child(stage)
			if err != nil {
				return nil, err
			}
			children := 1
			if stage.ItemsFrom != nil {
				children = budget
				if d.Recursion == nil {
					children = 16
				}
			}
			if d.Recursion == nil {
				if n > 1000000/max(1, children) {
					return nil, fmt.Errorf("footprint: derived session count exceeds 1000000")
				}
				own[0] = n * children
				break
			}
			own = make(treePeakProfile, min(budget, children)+1)
			for i := range own {
				if n > 1000000/max(1, i) {
					return nil, fmt.Errorf("footprint: derived session count exceeds 1000000")
				}
				own[i] = n * i
			}
		case FanOut:
			own = treePeakProfile{0}
			for _, branch := range EffectiveBranches(stage) {
				part, err := visit(branch.Start, stage.Join, seen)
				if err != nil {
					return nil, err
				}
				if stage.Parallel {
					own, err = parallel(own, part)
					if err != nil {
						return nil, err
					}
				} else {
					own = alternatives(own, part)
				}
			}
			if stage.Parallel && stage.BranchesFrom != nil {
				width := stage.BranchesFrom.Max
				if width == 0 {
					width = 256
				}
				template := own
				own = treePeakProfile{0}
				for range width {
					var err error
					own, err = parallel(own, template)
					if err != nil {
						return nil, err
					}
				}
			}
			tail, err := visit(stage.Join, stop, seen)
			if err != nil {
				return nil, err
			}
			return alternatives(own, tail), nil
		}
		for _, to := range edges[id] {
			tail, err := visit(to, stop, seen)
			if err != nil {
				return nil, err
			}
			own = alternatives(own, tail)
		}
		return own, nil
	}
	profile, err := visit(d.Start, "", map[string]bool{})
	if err != nil {
		return 0, err
	}
	return profile[len(profile)-1], nil
}
