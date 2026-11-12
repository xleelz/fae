package proof

import (
	"strings"
	"testing"
)

func futureConfig() Config {
	return Config{Tool: "future-solver", Parser: "json", Kind: Deductive, JSONFields: &JSONFields{Outcome: "report.status", Obligations: "report.counts.total", Verified: "report.counts.proved", Failed: "report.counts.counterexamples", Unknown: "report.counts.inconclusive", Skipped: "report.counts.omitted", ToolVersion: "solver.version", StatusMap: map[string]Outcome{"success": Proved, "counterexample": Disproved, "inconclusive": Unknown}}}
}

const futureReport = `{"report":{"status":"success","counts":{"total":2,"proved":2,"counterexamples":0,"inconclusive":0,"omitted":0}},"solver":{"version":"v3"},"source_digest":"untrusted-report-digest"}`

func TestConfigDrivenFutureSolverJSON(t *testing.T) {
	c := futureConfig()
	c.ToolVersion = "v3"
	r, e := Parse(c, Run{Stdout: futureReport, SourceDigest: "actual-bound-tree"})
	if e != nil || !r.Passed || r.Obligations != 2 || r.ToolVersion != "v3" || r.SourceDigest != "actual-bound-tree" {
		t.Fatalf("%+v %v", r, e)
	}
	for _, tc := range []struct {
		status string
		f, u   int
		want   Outcome
	}{{"counterexample", 1, 0, Disproved}, {"inconclusive", 0, 1, Unknown}} {
		raw := strings.Replace(futureReport, `"status":"success"`, `"status":"`+tc.status+`"`, 1)
		raw = strings.Replace(raw, `"proved":2`, `"proved":1`, 1)
		if tc.f > 0 {
			raw = strings.Replace(raw, `"counterexamples":0`, `"counterexamples":1`, 1)
		} else {
			raw = strings.Replace(raw, `"inconclusive":0`, `"inconclusive":1`, 1)
		}
		r, e := Parse(c, Run{Stdout: raw})
		if e != nil || r.Passed || r.Outcome != tc.want {
			t.Fatalf("%+v %v", r, e)
		}
	}
}
func TestMappedJSONRefusesAmbiguityAndCoercion(t *testing.T) {
	for _, raw := range []string{strings.Replace(futureReport, `"proved":2`, `"proved":"2"`, 1), strings.Replace(futureReport, `"proved":2`, `"proved":2.0`, 1), strings.Replace(futureReport, `"proved":2`, `"proved":2e0`, 1), strings.Replace(futureReport, `"proved":2`, `"proved":-1`, 1), strings.Replace(futureReport, `"proved":2`, `"proved":999999999999999999999`, 1), strings.Replace(futureReport, `"proved":2`, `"proved":2,"proved":2`, 1), strings.Replace(futureReport, `"status":"success"`, `"status":"unmapped"`, 1), strings.Replace(futureReport, `"status":"success"`, `"status":true`, 1), strings.Replace(futureReport, `"omitted":0`, `"different":0`, 1), futureReport + futureReport, `null`, `[]`} {
		r, e := Parse(futureConfig(), Run{Stdout: raw})
		if e == nil || r.Passed {
			t.Fatalf("accepted %s: %+v %v", raw, r, e)
		}
	}
}
func TestMappedJSONConfigRefusals(t *testing.T) {
	for _, change := range []func(*Config){func(c *Config) { c.JSONFields = nil }, func(c *Config) { c.JSONFields.StatusMap = nil }, func(c *Config) { c.JSONFields.StatusMap["success"] = "verified" }, func(c *Config) { c.JSONFields.Obligations = "$.report.counts.total" }, func(c *Config) { c.JSONFields.Obligations = "report.counts[0]" }, func(c *Config) { c.JSONFields.ToolVersion = "solver.*" }, func(c *Config) { c.Parser = "normalized-json" }, func(c *Config) { c.Kind = "" }} {
		c := futureConfig()
		change(&c)
		if Validate(c) == nil {
			t.Fatalf("accepted %+v", c)
		}
	}
}
func TestMappedJSONBoundsAndCrossHairClassification(t *testing.T) {
	raw := `{"unused":` + strings.Repeat(`{"nested":`, 18) + `0` + strings.Repeat(`}`, 18) + `}`
	r, e := Parse(futureConfig(), Run{Stdout: raw})
	if e == nil || r.Passed {
		t.Fatalf("accepted overdepth: %+v %v", r, e)
	}
	c := futureConfig()
	c.Tool = "crosshair"
	c.Kind = CounterexampleSearch
	r, e = Parse(c, Run{Stdout: futureReport})
	if e != nil || r.Passed || r.Kind != CounterexampleSearch || r.Outcome != Unknown {
		t.Fatalf("%+v %v", r, e)
	}
}
