package proof

import (
	"fmt"
	"regexp"
	"strings"
)

func kani(r *Result, text string) error {
	summaries := kaniSummary.FindAllStringSubmatch(text, -1)
	complete := strings.Count(text, "VERIFICATION:- SUCCESSFUL") + strings.Count(text, "VERIFICATION:- FAILED")
	if len(summaries) == 0 || complete != len(summaries) {
		r.Detail = "Kani report has no complete, counted harness summaries"
		return nil
	}
	for _, m := range summaries {
		f, n, u := number(m[1]), number(m[2]), 0
		if m[3] != "" {
			u = number(m[3])
		}
		if f < 0 || n < 0 || u < 0 || f+u > n {
			return fmt.Errorf("invalid Kani counts")
		}
		r.Obligations += n
		r.Verified += n - f - u
		r.Failed += f
		r.Skipped += u
	}
	r.Outcome = Proved
	if r.Failed > 0 {
		r.Outcome = Disproved
	} else if r.Skipped > 0 || strings.Contains(text, "UNDETERMINED") {
		r.Outcome = Unknown
		r.Detail = "unreachable or undetermined Kani checks do not discharge all obligations"
	}
	if strings.Contains(text, "VERIFICATION:- FAILED") && r.Failed == 0 {
		r.Outcome = Unknown
		r.Detail = "Kani failed without a counted counterexample"
	}
	return nil
}

var gomelaResult = regexp.MustCompile(`^Result for (?:source|negative):(.+)$`)
var gomelaStates = regexp.MustCompile(`^Number of states\s*:\s*([0-9]+)\s*$`)
var gomelaProperty = regexp.MustCompile(`^(Send on close safety error|Close safety error|Negative counter safety error|Double unlock error|Model deadlock|Panic reached)\s*:\s*(true|false)\.$`)

func gomela(r *Result, text string) error {
	models, completed, states := 0, 0, 0
	properties := map[string]bool{}
	active := false
	finish := func() bool {
		if !active {
			return true
		}
		if len(properties) != 6 || states <= 0 {
			return false
		}
		completed++
		return true
	}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if gomelaResult.MatchString(line) {
			if !finish() {
				r.Detail = "incomplete Gomela model report"
				return nil
			}
			models++
			active = true
			properties = map[string]bool{}
			states = 0
			continue
		}
		if !active {
			continue
		}
		if m := gomelaStates.FindStringSubmatch(line); m != nil {
			states = number(m[1])
		}
		if m := gomelaProperty.FindStringSubmatch(line); m != nil {
			if properties[m[1]] {
				return fmt.Errorf("duplicate Gomela property")
			}
			properties[m[1]] = true
			r.Obligations++
			if m[2] == "true" {
				r.Failed++
			} else {
				r.Verified++
			}
		}
	}
	if !finish() || models == 0 || completed != models {
		r.Detail = "no complete Gomela model reports"
		r.Unknown = r.Obligations - r.Verified - r.Failed
		return nil
	}
	if strings.Contains(text, "Could not find type") || strings.Contains(text, "error parsing") {
		r.Outcome = Error
		r.Detail = "Gomela translation did not complete cleanly"
		return nil
	}
	r.Outcome = Proved
	if r.Failed > 0 {
		r.Outcome = Disproved
	}
	return nil
}

var crosshairDiagnostic = regexp.MustCompile(`^.+:[0-9]+:(?:(?:[0-9]+):)?\s*(info|error|warning):\s*(.*)$`)

func crosshair(r *Result, text string) error {
	for _, line := range strings.Split(text, "\n") {
		m := crosshairDiagnostic.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		msg := m[2]
		switch {
		case strings.Contains(msg, "Confirmed over all paths"):
			r.Obligations++
			r.Verified++
		case m[1] == "error" && (strings.Contains(msg, "false when calling") || strings.Contains(msg, "AssertionError") || strings.Contains(msg, "yields false")):
			r.Obligations++
			r.Failed++
		case strings.Contains(msg, "Not confirmed") || strings.Contains(msg, "Cannot confirm"):
			r.Obligations++
			r.Unknown++
		default:
			if m[1] == "error" {
				r.Obligations++
				r.Unknown++
			}
		}
	}
	if r.Failed > 0 {
		r.Outcome = Disproved
	} else {
		r.Outcome = Unknown
		r.Detail = "CrossHair search is not accepted as unbounded deductive proof"
	}
	return nil
}
