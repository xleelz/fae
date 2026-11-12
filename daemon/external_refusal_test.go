package daemon

import (
	"strings"
	"testing"

	"github.com/xleelz/fae/domain"
)

// Appendix B: a pinned external call the remote side refuses escalates
// ExternalEffectRefused with the reported id, never an UncertainDispatch park,
// and no later effect (merge, ticket closure) runs.
func TestShippedDefaultNormalExternalRefusal(t *testing.T) {
	run := normalRun{task: "normal-refused", live: true, mergeCheck: "Pass"}
	d, _, _, vendor := defaultNormalFixture(t, run)
	vendor.mu.Lock()
	vendor.refuse = "vendor_review_publish"
	vendor.mu.Unlock()
	j := jobOf(t, admitNormal(t, d, run)).Job
	if j.State != domain.Escalated || j.EscalationKind != domain.ExternalEffectRefused || !strings.Contains(j.Reason, "external status 403 (reported id refusal-7)") || j.Parked {
		t.Fatal(j.State, j.EscalationKind, j.Reason, j.Parked)
	}
	if vendor.count("vendor_review_publish") != 1 || vendor.count("vendor_review_merge") != 0 {
		t.Fatal("refused publish replayed or followed by a merge", vendor.posts)
	}
}
