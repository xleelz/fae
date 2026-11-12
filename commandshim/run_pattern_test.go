package commandshim

import "testing"

// FAE-HOST-015: goTestRunSelectsEveryTest splits a -run value as the testing
// package does (alternatives at "|", subtest levels at "/", neither inside
// brackets, parentheses or after a backslash) and counts it as selecting
// every test when an alternative's top-level pattern matches every test
// function name (goTestNameProbes). Before, only a fixed list of total forms
// counted, so Test.*, (.*), x*, T and the other total patterns re-audit 10
// probed got two threads; (X|.*), which selects every test, was false.
func TestGoTestRunSelectsEveryTest(t *testing.T) {
	for run, want := range map[string]bool{
		"": true, ".": true, ".*": true, ".+": true, "^": true, "$": true, "^.*$": true, "^.+$": true, ".*$": true,
		"Test": true, "^Test": true, "X|": true, "|X": true, "X|.*": true, "X/Y|.*/Z": true, "/X": true, ".*/X": true, ".*/": true,
		// Re-audit 10's total patterns that the fixed list missed.
		"Test.*": true, "^Test.*": true, "^Test.*$": true, "(.*)": true, "(Test)": true, "(?:)": true, ".?": true, ".*?": true,
		"x*": true, "T": true, "^T": true, `[\s\S]*`: true, "(X|.*)": true, "e": true, "(?i)^test": true, "^Test(|[^a-z].*)$": true,
		"X": false, "^$": false, "TestX": false, "^TestX$": false, "X/.*": false, "[|].*": false, "[.*|]": false,
		"X\\|.*": false, "(.*": false, "X/Y|Z/.*": false, "Benchmark": false, ".*X": false, "Test$": false,
		// Close to total, but each misses a real test name.
		"^Test[A-Z]": false, "^Test.+": false, "Test[^_]": false, "^Test[A-Z_0-9]": false, "[A-Za-z]$": false, "^Test.{0,100}$": false,
	} {
		if got := goTestRunSelectsEveryTest(run); got != want {
			t.Errorf("goTestRunSelectsEveryTest(%q) = %t, want %t", run, got, want)
		}
	}
}
