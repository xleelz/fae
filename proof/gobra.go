package proof

import (
	"fmt"
	"regexp"
	"strings"
)

// Gobra DEBUG statistics count contract-bearing members, not SMT VCs. The
// specified count is also emitted for failed runs: zero-error completion is
// checked by native before calling this helper.
var gobraMembersTotal = regexp.MustCompile(`Gobra found ([0-9]+) methods and functions\.`)
var gobraMembersSpecified = regexp.MustCompile(`(?m)(?:^| - )([0-9]+) members? are explicitly specified\.`)
var gobraMembersTrusted = regexp.MustCompile(`(?m)(?:^| - )([0-9]+) specified members of the package under verification are trusted or abstract\.`)

func gobraMembers(r *Result, text string) (bool, error) {
	total := gobraMembersTotal.FindAllStringSubmatch(text, -1)
	specified := gobraMembersSpecified.FindAllStringSubmatch(text, -1)
	trusted := gobraMembersTrusted.FindAllStringSubmatch(text, -1)
	if len(total) == 0 && len(specified) == 0 && len(trusted) == 0 {
		return false, nil
	}
	if len(total) != 1 || len(specified) != 1 || len(trusted) != 1 {
		r.Detail = "incomplete or ambiguous Gobra DEBUG member statistics"
		return true, nil
	}
	n, s, t := number(total[0][1]), number(specified[0][1]), number(trusted[0][1])
	if n < 0 || s < 0 || t < 0 || s > n || t > s {
		return true, fmt.Errorf("inconsistent Gobra member statistics")
	}
	r.Obligations = s
	r.Verified = s - t
	r.Unknown = t
	r.Detail = "Gobra counted explicitly specified members, not individual SMT verification conditions"
	if t > 0 {
		r.Outcome = Unknown
		r.Detail += "; trusted or abstract specified members remain unverified"
		return true, nil
	}
	lower := strings.ToLower(text)
	for _, marker := range []string{"timed out", "verification timeout", "verification was skipped", "verification aborted", "verification cancelled", "verification canceled"} {
		if strings.Contains(lower, marker) {
			r.Outcome = Unknown
			r.Unknown = s
			r.Verified = 0
			r.Detail = "Gobra verification did not complete all members"
			return true, nil
		}
	}
	r.Outcome = Proved
	return true, nil
}
