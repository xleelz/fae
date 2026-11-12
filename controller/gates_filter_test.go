package controller

import (
	"strings"
	"testing"
)

// FAE-CTL-030, FAE-CLI-014: the required test filter is read as go test -run
// reads it, so the regular expression admission accepts is the one the
// landing runs and counts. An anchored name, an alternation and a substring
// each count the top-level tests they match; a pattern with a '/' counts the
// subtests at its depth; a derived-scope RED identity is quoted and anchored
// first (ExactTestFilter), so a name with a namesake prefix counts itself
// alone; a pattern that matches nothing holds; one that does not compile is
// an error. On the base, `-run` got the quoted filter and only a test named
// exactly as written counted, so `^TestA$` and `TestA|TestB` matched zero.
func TestTestFilterReadsAsGoTestRun(t *testing.T) {
	const p = "example.invalid/a"
	stream := goStream(goTest(p, "TestA", "pass"), goTest(p, "TestA/one", "pass"), goTest(p, "TestA/two", "pass"), goTest(p, "TestAB", "pass"), goTest(p, "TestB", "fail"), goTest(p, "TestC", "pass"), goTest(p, "TestSkip", "skip"), goTest(p, "TestSub", "pass"), goTest(p, "TestSub/x", "pass"), goTest(p, "TestSub/skipme", "skip"), goPackage(p, "fail"))
	for filter, want := range map[string][2]int{
		"^TestA$":                  {1, 1},
		"TestA|TestB":              {3, 2},
		"^(TestA|TestB)$":          {2, 1},
		"estA":                     {2, 2},
		"TestA/":                   {2, 2},
		"TestA/one":                {1, 1},
		"^TestA$/^two$":            {1, 1},
		"TestA/one|TestC":          {2, 2},
		"^TestSkip$":               {1, 0},
		"TestA$|TestSkip":          {2, 1},
		"TestSub/x|TestSub/skipme": {2, 1},
		"TestSub/":                 {2, 1},
		"[/]":                      {0, 0},
		"^TestZ$":                  {0, 0},
		`^TestA$/^one$`:            {1, 1},
		`^TestA\.B$`:               {0, 0},
		`^TestA$/^one\.x$`:         {0, 0},
	} {
		c, e := GoTestCounts([]byte(stream), filter)
		if e != nil {
			t.Fatalf("%q: %v", filter, e)
		}
		if c.Matched != want[0] || c.MatchedPassed != want[1] {
			t.Errorf("%q: matched %d passing %d, want %d and %d", filter, c.Matched, c.MatchedPassed, want[0], want[1])
		}
		r := FilterGate(c, filter)
		if r.Passed != (want[0] > 0 && want[0] == want[1]) || r.Count != want[0] {
			t.Errorf("%q: %+v", filter, r)
		}
		if want[0] == 0 && !strings.Contains(r.Reason, "matched zero tests") {
			t.Errorf("%q: %+v", filter, r)
		}
		if argv := GoFilterCommand(filter); argv[4] != "-run" || argv[5] != filter || len(argv) != 7 {
			t.Errorf("%q: go test gets %q", filter, argv)
		}
	}
	for _, total := range []string{".*", "Test", "TestA|.*", ".*/one"} {
		if argv := GoFilterCommand(total); len(argv) != 9 || argv[6] != "-p" || argv[7] != "1" {
			t.Errorf("total filter %q: go test gets %q", total, argv)
		}
	}
	for _, bad := range []string{"TestA(", `\Q/\E`, `x\Qa|b\E`} {
		if _, e := GoTestCounts([]byte(stream), bad); e == nil || !strings.Contains(e.Error(), "test filter") {
			t.Fatalf("a filter that does not compile (%q): %v", bad, e)
		}
	}
}
