package execution

// RestrictedEffects is the restricted-effects proof for replaying a stalled
// turn (FAE-RT-005). The trusted fae-agent-v1 bridge enforces the turn's
// effective profile, and handoff, read and edit change nothing beyond the
// measured task tree and the handoff; brokered build, test and format runs are
// local, and their writes fall under the unchanged-tree proof. Any other
// capability withholds the proof, so such a stall stays uncertain.
func RestrictedEffects(allowed []string) (ok bool) {
	for _, c := range allowed {
		if c != "handoff" && c != "read" && c != "edit" {
			return false
		}
	}
	return true
}
