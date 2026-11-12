package proof

import (
	"os"
	"strings"
	"testing"
)

func TestGobraRealDebugMemberReports(t *testing.T) {
	for _, n := range []string{"positive", "negative"} {
		raw, e := os.ReadFile("testdata/gobra-debug-" + n + ".log")
		if e != nil {
			t.Fatal(e)
		}
		run := Run{Stdout: string(raw)}
		if n == "negative" {
			run.ExitCode = 1
		}
		r, e := Parse(Config{Tool: "gobra"}, run)
		if e != nil {
			t.Fatal(e)
		}
		if n == "positive" && (!r.Passed || r.Obligations != 1 || r.Verified != 1) {
			t.Fatalf("%+v", r)
		}
		if n == "negative" && (r.Passed || r.Outcome != Disproved || r.Failed != 1) {
			t.Fatalf("%+v", r)
		}
	}
}
func TestGobraDebugStatisticsFailClosed(t *testing.T) {
	b, e := os.ReadFile("testdata/gobra-debug-positive.log")
	if e != nil {
		t.Fatal(e)
	}
	for _, tc := range []struct {
		name, text string
		run        Run
	}{{"trusted", strings.Replace(string(b), "0 specified members of the package", "1 specified members of the package", 1), Run{}}, {"no specs", strings.Replace(string(b), "1 member are explicitly specified", "0 member are explicitly specified", 1), Run{}}, {"missing trust stats", strings.Replace(string(b), "specified members of the package under verification are trusted or abstract", "omitted statistics", 1), Run{}}, {"timeout", string(b), Run{TimedOut: true}}, {"internal timeout", string(b) + "verification timed out\n", Run{}}, {"skipped", string(b) + "verification was skipped\n", Run{}}, {"stats only", strings.Replace(string(b), "Gobra found 0 errors.", "verification status missing", 1), Run{}}, {"duplicate", string(b) + "Gobra found 1 methods and functions.\n", Run{}}} {
		t.Run(tc.name, func(t *testing.T) {
			tc.run.Stdout = tc.text
			r, _ := Parse(Config{Tool: "gobra"}, tc.run)
			if r.Passed {
				t.Fatalf("%+v", r)
			}
		})
	}
}
