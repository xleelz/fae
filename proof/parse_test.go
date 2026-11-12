package proof

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

func report(status string, n, v, f, u, s int) string {
	return fmt.Sprintf(`{"outcome":%q,"obligations":%d,"verified":%d,"failed":%d,"unknown":%d,"skipped":%d}`, status, n, v, f, u, s)
}
func TestNormalizedVerifierProfiles(t *testing.T) {
	for _, tool := range []string{"gobra", "gomela", "kani", "verus", "nagini", "crosshair", "theorem", "tscert", "custom-checker"} {
		t.Run(tool, func(t *testing.T) {
			c := Config{Tool: tool, Parser: "normalized-json", Bounds: map[string]string{"unwind": "8"}, Assumptions: []string{"input contract"}}
			if tool == "custom-checker" {
				c.Kind = Deductive
			}
			r, e := Parse(c, Run{Stdout: report("proved", 2, 2, 0, 0, 0), SourceDigest: "actual-tree"})
			if e != nil {
				t.Fatal(e)
			}
			if r.Passed != (tool != "crosshair") || r.SourceDigest != "actual-tree" || r.Bounds["unwind"] != "8" {
				t.Fatalf("%+v", r)
			}
			r.Bounds["unwind"] = "changed"
			if c.Bounds["unwind"] != "8" {
				t.Fatal("mutated config")
			}
			bad, e := Parse(c, Run{Stdout: report("disproved", 2, 1, 1, 0, 0), ExitCode: 1})
			if e != nil || bad.Passed || bad.Outcome != Disproved {
				t.Fatalf("%+v %v", bad, e)
			}
		})
	}
}
func TestReportFailureModes(t *testing.T) {
	c := Config{Tool: "verus", Parser: "normalized-json"}
	for _, tc := range []struct {
		name, raw string
		run       Run
		want      Outcome
		wantErr   bool
	}{{"empty count", report("proved", 0, 0, 0, 0, 0), Run{}, Unknown, false}, {"unresolved", report("unknown", 2, 1, 0, 1, 0), Run{}, Unknown, false}, {"skipped", report("skipped", 1, 0, 0, 0, 1), Run{}, Skipped, false}, {"unsupported", report("unsupported", 0, 0, 0, 0, 0), Run{}, Unsupported, false}, {"error", report("error", 0, 0, 0, 0, 0), Run{}, Error, false}, {"timeout", report("proved", 1, 1, 0, 0, 0), Run{TimedOut: true}, Unknown, false}, {"truncated", report("proved", 1, 1, 0, 0, 0), Run{Truncated: true}, Unknown, false}, {"exit only", "", Run{}, Error, true}, {"bad counts", report("proved", 1, 0, 0, 0, 0), Run{}, Error, true}, {"false proof", report("proved", 1, 0, 0, 1, 0), Run{}, Error, true}, {"malformed", "{", Run{}, Error, true}, {"unknown field", strings.TrimSuffix(report("proved", 1, 1, 0, 0, 0), "}") + `,"passed":true}`, Run{}, Error, true}, {"duplicate", strings.TrimSuffix(report("proved", 1, 1, 0, 0, 0), "}") + `,"outcome":"proved"}`, Run{}, Error, true}, {"failed exit", report("proved", 1, 1, 0, 0, 0), Run{ExitCode: 2}, Error, false}} {
		t.Run(tc.name, func(t *testing.T) {
			tc.run.Stdout = tc.raw
			r, e := Parse(c, tc.run)
			if (e != nil) != tc.wantErr || r.Passed || r.Outcome != tc.want {
				t.Fatalf("%+v %v", r, e)
			}
		})
	}
}
func TestNativeVerusAndKani(t *testing.T) {
	for _, tc := range []struct {
		tool, text string
		status     Outcome
		passed     bool
	}{{"verus", "verification results:: 3 verified, 0 errors\n", Proved, true}, {"verus", "verification results:: 1 verified, 1 errors\n", Disproved, false}, {"verus", "verification results:: 0 verified, 0 errors\n", Unknown, false}, {"kani", "SUMMARY:\n ** 0 of 8 failed\nVERIFICATION:- SUCCESSFUL\n", Proved, true}, {"kani", "SUMMARY:\n ** 0 of 8 failed (1 unreachable)\nVERIFICATION:- SUCCESSFUL\n", Unknown, false}, {"kani", "SUMMARY:\n ** 1 of 8 failed\nVERIFICATION:- FAILED\n", Disproved, false}, {"kani", " ** 0 of 8 failed\n", Unknown, false}, {"gobra", "Gobra found 0 errors.\n", Unknown, false}, {"nagini", "Verification successful.\n", Unknown, false}, {"theorem", "all verified\n", Unsupported, false}, {"tscert", `{"status":"verified"}`, Unsupported, false}, {"crosshair", "a.py:12: info: Confirmed over all paths.\n", Unknown, false}, {"crosshair", "a.py:12: error: false when calling f(2)\n", Disproved, false}} {
		t.Run(tc.tool+tc.text, func(t *testing.T) {
			r, e := Parse(Config{Tool: tc.tool}, Run{Stdout: tc.text})
			if e != nil || r.Outcome != tc.status || r.Passed != tc.passed {
				t.Fatalf("%+v %v", r, e)
			}
		})
	}
}
func TestGomelaActualCapturedReports(t *testing.T) {
	for _, name := range []string{"positive", "negative"} {
		b, e := os.ReadFile("../verification/logs/gomela-" + name + ".log")
		if e != nil {
			t.Fatal(e)
		}
		r, e := Parse(Config{Tool: "gomela"}, Run{Stdout: string(b)})
		if e != nil {
			t.Fatal(e)
		}
		if name == "positive" && (!r.Passed || r.Obligations <= 0) {
			t.Fatalf("%+v", r)
		}
		if name == "negative" && r.Passed {
			t.Fatalf("%+v", r)
		}
	}
}
func TestPinnedVersionObserved(t *testing.T) {
	c := Config{Tool: "theorem", Parser: "normalized-json", ToolVersion: "1.2"}
	raw := strings.TrimSuffix(report("proved", 1, 1, 0, 0, 0), "}") + `,"tool_version":"1.2"}`
	r, e := Parse(c, Run{Stdout: raw})
	if e != nil || !r.Passed || r.ToolVersion != "1.2" {
		t.Fatalf("%+v %v", r, e)
	}
	r, e = Parse(c, Run{Stdout: report("proved", 1, 1, 0, 0, 0)})
	if e == nil || r.Passed {
		t.Fatalf("accepted unobserved pin: %+v", r)
	}
}
func TestConfigAndMinimum(t *testing.T) {
	for _, c := range []Config{{Tool: "custom"}, {Tool: "crosshair", Kind: Deductive}, {Tool: "verus", Parser: "exit"}, {Tool: "gobra", Sources: []string{"../escape"}}, {Tool: "gobra", MinObligations: -1}} {
		if Validate(c) == nil {
			t.Fatalf("accepted %+v", c)
		}
	}
	r, e := Parse(Config{Tool: "verus", MinObligations: 2}, Run{Stdout: "verification results:: 1 verified, 0 errors\n"})
	if e != nil || r.Passed || r.Outcome != Unknown {
		t.Fatalf("%+v %v", r, e)
	}
}

func TestGobraCompilerErrorsAreNotCounterexamples(t *testing.T) {
	for _, tc := range []struct {
		text    string
		outcome Outcome
	}{{"Error at: <f.go:3:1> Postcondition might not hold.\nGobra found 1 error.\n", Disproved}, {"Error at: <f.go:3:1> unknown identifier foo\nGobra found 1 error.\n", Error}} {
		r, e := Parse(Config{Tool: "gobra"}, Run{Stdout: tc.text, ExitCode: 1})
		if e != nil || r.Outcome != tc.outcome || r.Passed {
			t.Fatalf("%+v %v", r, e)
		}
	}
}
