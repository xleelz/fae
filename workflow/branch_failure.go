package workflow

// BranchJoin identifies the nearest region whose join is target. Joins are
// excluded from their own region, so nested pairs remain unambiguous.
func BranchJoin(d Document, stage, target string) bool {
	id := EnclosingBranchFan(d, stage)
	return id != "" && d.Stages[id].Join == target
}
func EnclosingBranchFan(d Document, stage string) string {
	best := ""
	size := int(^uint(0) >> 1)
	for id, fan := range d.Stages {
		if fan.Primitive != FanOut {
			continue
		}
		region := BranchOf(d, id)
		if _, ok := region[stage]; ok && (len(region) < size || len(region) == size && id < best) {
			best, size = id, len(region)
		}
	}
	return best
}

// SharedJoinSuccess distinguishes a single graph edge used for both outcomes.
func SharedJoinSuccess(d Document, stage, target string) bool {
	if !BranchJoin(d, stage, target) {
		return false
	}
	found := false
	for _, edge := range d.Edges {
		if edge.From != stage {
			continue
		}
		if edge.To != target {
			return false
		}
		found = true
	}
	return found
}
func BranchFailureJoin(d Document, stage string) string {
	id := EnclosingBranchFan(d, stage)
	if id != "" && d.Stages[id].BranchFailure == "fail-branch" {
		return d.Stages[id].Join
	}
	return ""
}
