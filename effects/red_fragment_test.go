package effects

import (
	"encoding/json"
	"strings"
	"testing"
)

// goTestJSON is the go test -json stream of one failing test TestX whose own
// output is "x_test.go:7: got 0 want 42", framed as go test frames every test.
func goTestJSON(lines ...string) string {
	events := []map[string]string{{"Action": "run", "Package": "p", "Test": "TestX"}}
	for _, line := range append([]string{"=== RUN   TestX\n"}, append(lines, "--- FAIL: TestX (0.00s)\n")...) {
		events = append(events, map[string]string{"Action": "output", "Package": "p", "Test": "TestX", "Output": line})
	}
	events = append(events, map[string]string{"Action": "fail", "Package": "p", "Test": "TestX"}, map[string]string{"Action": "output", "Package": "p", "Output": "FAIL\tp\t0.01s\n"}, map[string]string{"Action": "fail", "Package": "p"})
	var b strings.Builder
	for _, e := range events {
		raw, _ := json.Marshal(e)
		b.Write(append(raw, '\n'))
	}
	return b.String()
}

// FAE-WF-036/073: a RED's declared fragment is matched against the test's own
// output with go test's framing lines removed, so "FAIL" or a "--- FAIL:" line
// never stands in for the declared reason; a fragment of the test's own
// failure message still matches. The test id still matches when the test
// panics, because the trace names the test function: the daemon's derived-RED
// reason rule, not the broker, refuses an id-only fragment.
func TestRedFragmentIgnoresGoTestFraming(t *testing.T) {
	output := goTestJSON("    x_test.go:7: got 0 want 42\n")
	for fragment, want := range map[string]bool{
		"got 0 want 42":   true,
		"x_test.go:7":     true,
		"TestX":           false,
		"FAIL":            false,
		"--- FAIL: TestX": false,
		"=== RUN":         false,
		"(0.00s)":         false,
		"p\t0.01s":        false,
	} {
		r := RunEvidence{ExitCode: 1, Output: output}
		if err := parseTests(&r, Command{TestID: "TestX", ExpectedFailure: fragment}); err != nil {
			t.Fatal(err)
		}
		if r.MatchedCount != 1 || r.ExecutedCount != 1 || r.RedPassed {
			t.Fatalf("counts %+v", r)
		}
		if r.ExpectedFailure != want {
			t.Errorf("fragment %q: expected failure %v, want %v", fragment, r.ExpectedFailure, want)
		}
	}
	// A subtest's framing is indented; a panic's message is the test's own,
	// and its trace names the test.
	r := RunEvidence{ExitCode: 2, Output: goTestJSON("    --- FAIL: TestX/sub (0.00s)\n", "panic: boom [recovered]\n", "p.TestX(0xc000012345?)\n")}
	for fragment, want := range map[string]bool{"panic: boom": true, "--- FAIL: TestX/sub": false, "TestX": true} {
		if err := parseTests(&r, Command{TestID: "TestX", ExpectedFailure: fragment}); err != nil {
			t.Fatal(err)
		}
		if r.ExpectedFailure != want {
			t.Errorf("fragment %q: expected failure %v, want %v", fragment, r.ExpectedFailure, want)
		}
		r.ExecutedCount, r.MatchedCount = 0, 0
	}
}
