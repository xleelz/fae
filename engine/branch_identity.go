package engine

import (
	"fmt"
	"sort"
	"strings"
)

// fanVisitSuffix qualifies later fan instances without changing first-visit
// hierarchical task IDs or exposing a content hash as their identity.
func fanVisitSuffix(context *BranchContext) string {
	if context == nil || len(context.VisitGroups) == 0 {
		return ""
	}
	keys := make([]string, 0, len(context.VisitGroups))
	later := false
	for key, visit := range context.VisitGroups {
		keys = append(keys, key)
		later = later || visit > 1
	}
	if !later {
		return ""
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := strings.Count(keys[i], "/"), strings.Count(keys[j], "/")
		if a != b {
			return a < b
		}
		return keys[i] < keys[j]
	})
	suffix := ".fan"
	for _, key := range keys {
		suffix += fmt.Sprintf(".%d", context.VisitGroups[key])
	}
	return suffix
}
