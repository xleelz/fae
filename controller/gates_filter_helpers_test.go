package controller

import (
	"strings"
	"testing"
)

// ExactTestFilter names exactly one test, and splitTestFilter splits a filter
// as go test's -run does (testing's splitRegexp). These helpers are new in
// wave 13, so this file does not compile on the base; the behaviour they
// serve is shown by TestTestFilterReadsAsGoTestRun and the landing tests.
func TestExactTestFilterAndSplitFollowGoTestRun(t *testing.T) {
	if got := ExactTestFilter("TestA.B/sub (x)"); got != `^TestA\.B$/^sub \(x\)$` {
		t.Fatal(got)
	}
	for filter, want := range map[string][][]string{
		"TestA":                {{"TestA"}},
		"TestA/one":            {{"TestA", "one"}},
		"TestA|TestB":          {{"TestA"}, {"TestB"}},
		"A/b|C":                {{"A", "b"}, {"C"}},
		"(a|b)/[/|]c":          {{"(a|b)", "[/|]c"}},
		`a\/b`:                 {{`a\/b`}},
		"^(TestA|TestB)$/case": {{"^(TestA|TestB)$", "case"}},
	} {
		got := splitTestFilter(filter)
		if len(got) != len(want) {
			t.Fatalf("%q: %q", filter, got)
		}
		for i := range want {
			if strings.Join(got[i], "\x00") != strings.Join(want[i], "\x00") {
				t.Fatalf("%q: %q", filter, got)
			}
		}
	}
	// The parser of git diff --raw -z: a type change, an addition and a
	// modification, with a path git would quote on a line.
	raw := ":120000 100644 abc def T\x00link.go\x00:000000 100644 000 111 A\x00new \"q\".go\x00:100644 100755 222 333 M\x00dir/x.go\x00"
	changes := landRawChanges(raw)
	if len(changes) != 3 || changes[0] != (landRawChange{"120000", "100644", "T", "link.go"}) || changes[1].path != `new "q".go` || changes[2].tipMode != "100755" {
		t.Fatalf("%+v", changes)
	}
	if landRegularFileMode("120000") || landRegularFileMode("000000") || landRegularFileMode("160000") || !landRegularFileMode("100644") || !landRegularFileMode("100755") {
		t.Fatal("regular file modes")
	}
}
