package proof

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

const MaxReportBytes = 8 << 20

func Parse(c Config, run Run) (Result, error) {
	r := Result{Outcome: Unknown, Kind: toolKind(c.Tool), Tool: c.Tool, Parser: c.Parser, SourceDigest: run.SourceDigest, ToolVersion: run.ToolVersion, Bounds: map[string]string{}, Assumptions: append([]string(nil), c.Assumptions...)}
	for k, v := range c.Bounds {
		r.Bounds[k] = v
	}
	if r.Kind == "" {
		r.Kind = c.Kind
	}
	if r.Parser == "" {
		r.Parser = "native"
	}
	fail := func(e error) (Result, error) { r.Outcome = Error; r.Passed = false; r.Detail = e.Error(); return r, e }
	if e := Validate(c); e != nil {
		return fail(e)
	}
	if run.TimedOut || run.Truncated {
		r.Detail = "verifier report incomplete (timeout or truncation)"
		return r, nil
	}
	if len(run.Stdout) > MaxReportBytes || len(run.Stderr) > MaxReportBytes {
		r.Detail = "verifier report exceeds bound"
		return r, nil
	}
	var e error
	if r.Parser == "json" {
		e = mappedJSON(&r, run.Stdout, c.JSONFields)
	} else if r.Parser == "normalized-json" {
		e = normalized(&r, run.Stdout)
	} else {
		e = native(&r, run.Stdout+"\n"+run.Stderr)
	}
	if e != nil {
		return fail(e)
	}
	if c.ToolVersion != "" && r.ToolVersion != c.ToolVersion {
		return fail(fmt.Errorf("formal tool version does not match pinned version"))
	}
	if r.Obligations < 0 || r.Verified < 0 || r.Failed < 0 || r.Unknown < 0 || r.Skipped < 0 || r.Obligations > 1000000000 || r.Verified > r.Obligations || r.Failed > r.Obligations || r.Unknown > r.Obligations || r.Skipped > r.Obligations || r.Verified+r.Failed+r.Unknown+r.Skipped != r.Obligations {
		return fail(fmt.Errorf("formal report obligation counts are inconsistent"))
	}
	if r.Outcome == Proved && (r.Failed != 0 || r.Unknown != 0 || r.Skipped != 0 || r.Verified != r.Obligations) {
		return fail(fmt.Errorf("proved report contains unresolved obligations"))
	}
	if r.Outcome == Disproved && r.Failed == 0 {
		return fail(fmt.Errorf("disproved report contains no failed obligations"))
	}
	if run.ExitCode != 0 && r.Outcome != Disproved && r.Outcome != Unsupported && r.Outcome != Skipped {
		r.Outcome = Error
		r.Detail = "verifier exited unsuccessfully"
	}
	minimum := c.MinObligations
	if minimum == 0 {
		minimum = 1
	}
	if r.Outcome == Proved && r.Obligations < minimum {
		r.Outcome = Unknown
		r.Detail = "no sufficient positive obligation evidence"
	}
	if r.Kind == CounterexampleSearch && r.Outcome == Proved {
		r.Outcome = Unknown
		r.Detail = "counterexample search is not a deductive proof"
	}
	r.Passed = run.ExitCode == 0 && r.Outcome == Proved && r.Obligations >= minimum && r.Verified == r.Obligations && r.Failed == 0 && r.Unknown == 0 && r.Skipped == 0
	return r, nil
}
func normalized(r *Result, raw string) error {
	if e := uniqueReportKeys(raw); e != nil {
		return e
	}
	var report struct {
		Outcome     Outcome `json:"outcome"`
		Kind        Kind    `json:"kind,omitempty"`
		Obligations *int    `json:"obligations"`
		Verified    *int    `json:"verified"`
		Failed      *int    `json:"failed"`
		Unknown     *int    `json:"unknown"`
		Skipped     *int    `json:"skipped"`
		Detail      string  `json:"detail,omitempty"`
		ToolVersion string  `json:"tool_version,omitempty"`
	}
	d := json.NewDecoder(bytes.NewBufferString(raw))
	d.DisallowUnknownFields()
	if e := d.Decode(&report); e != nil {
		return fmt.Errorf("malformed formal JSON report: %w", e)
	}
	var extra any
	if e := d.Decode(&extra); e != io.EOF {
		return fmt.Errorf("formal report must contain exactly one JSON object")
	}
	switch report.Outcome {
	case Proved, Disproved, Unknown, Error, Unsupported, Skipped:
	default:
		return fmt.Errorf("unknown formal report outcome %q", report.Outcome)
	}
	if report.Kind != "" && report.Kind != r.Kind {
		return fmt.Errorf("report evidence kind differs from configured tool")
	}
	if report.Obligations == nil || report.Verified == nil || report.Failed == nil || report.Unknown == nil || report.Skipped == nil {
		return fmt.Errorf("formal JSON report requires all obligation counts")
	}
	r.Outcome = report.Outcome
	r.Obligations = *report.Obligations
	r.Verified = *report.Verified
	r.Failed = *report.Failed
	r.Unknown = *report.Unknown
	r.Skipped = *report.Skipped
	r.Detail = report.Detail
	if report.ToolVersion != "" {
		if r.ToolVersion != "" && r.ToolVersion != report.ToolVersion {
			return fmt.Errorf("conflicting observed tool versions")
		}
		r.ToolVersion = report.ToolVersion
	}
	return nil
}

var verusSummary = regexp.MustCompile(`(?m)^verification results:: ([0-9]+) verified, ([0-9]+) errors\s*$`)
var kaniSummary = regexp.MustCompile(`(?m)^\s*\*\* ([0-9]+) of ([0-9]+) failed(?: \(([0-9]+) unreachable\))?\s*$`)
var gobraErrors = regexp.MustCompile(`Gobra found ([0-9]+) errors?\.`)
var gobraFailedChecks = regexp.MustCompile(`(?mi)^.*Error at:.*(?:might not hold|might fail|assertion|precondition|postcondition).*$`)
var explicitObligations = regexp.MustCompile(`(?mi)^\s*([0-9]+) (?:obligations|assertions) verified[.]?\s*$`)

func number(s string) int {
	n, e := strconv.Atoi(s)
	if e != nil || n > 1000000000 {
		return -1
	}
	return n
}
func native(r *Result, text string) error {
	switch r.Tool {
	case "verus":
		ms := verusSummary.FindAllStringSubmatch(text, -1)
		if len(ms) == 1 {
			m := ms[0]
			r.Verified = number(m[1])
			r.Failed = number(m[2])
			r.Obligations = r.Verified + r.Failed
			r.Outcome = Proved
			if r.Failed > 0 {
				r.Outcome = Disproved
			}
		} else if len(ms) > 1 {
			return fmt.Errorf("ambiguous Verus summary")
		}
	case "kani":
		return kani(r, text)
	case "gomela":
		return gomela(r, text)
	case "gobra":
		ms := gobraErrors.FindAllStringSubmatch(text, -1)
		if len(ms) > 1 {
			return fmt.Errorf("ambiguous Gobra summary")
		}
		if len(ms) == 1 {
			errors := number(ms[0][1])
			if errors > 0 {
				failed := len(gobraFailedChecks.FindAllString(text, -1))
				if failed == errors {
					r.Failed = failed
					r.Obligations = failed
					r.Outcome = Disproved
				} else {
					r.Outcome = Error
					r.Detail = "Gobra errors are not all recognized failed verification conditions"
				}
			} else if handled, e := gobraMembers(r, text); e != nil {
				return e
			} else if !handled {
				if m := explicitObligations.FindStringSubmatch(text); m != nil {
					r.Verified = number(m[1])
					r.Obligations = r.Verified
					r.Outcome = Proved
				} else {
					r.Detail = "Gobra zero-error summary lacks discharged obligation count; use DEBUG statistics or normalized-json adapter"
				}
			}
		}
	case "nagini":
		if strings.Contains(text, "Verification failed") {
			r.Detail = "Nagini failure without obligation counts; use normalized-json adapter"
		} else if strings.Contains(text, "Verification successful") {
			if m := explicitObligations.FindStringSubmatch(text); m != nil {
				r.Verified = number(m[1])
				r.Obligations = r.Verified
				r.Outcome = Proved
			} else {
				r.Detail = "Nagini success lacks discharged obligation count; use normalized-json adapter"
			}
		}
	case "crosshair":
		return crosshair(r, text)
	case "theorem", "tscert":
		r.Outcome = Unsupported
		r.Detail = "this tool requires normalized-json adapter; bare CLI success is insufficient"
	default:
		r.Outcome = Unsupported
		r.Detail = "unknown native verifier parser"
	}
	if r.Outcome == Unknown && r.Detail == "" {
		r.Detail = "no recognized completed verifier report"
	}
	return nil
}

func uniqueReportKeys(raw string) error {
	d := json.NewDecoder(strings.NewReader(raw))
	t, e := d.Token()
	if e != nil || t != json.Delim('{') {
		return fmt.Errorf("formal report must be a JSON object")
	}
	seen := map[string]bool{}
	for d.More() {
		t, e = d.Token()
		if e != nil {
			return e
		}
		key, ok := t.(string)
		if !ok || seen[key] {
			return fmt.Errorf("duplicate or invalid formal report field")
		}
		seen[key] = true
		var v json.RawMessage
		if e = d.Decode(&v); e != nil {
			return e
		}
	}
	_, e = d.Token()
	return e
}
