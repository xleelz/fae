package effects

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"unicode/utf8"
)

// recordedRun loads a recorded run from testdata/excerpt. The go test
// fixtures are real `go test -json -count=1 -a -x` output (Go 1.27) against a
// deliberately broken module, with paths neutralised and most of the standard
// library's -x trace dropped; counts come from the production parser.
func recordedRun(t *testing.T, name string, exit int, testID string) RunEvidence {
	t.Helper()
	out, err := os.ReadFile("testdata/excerpt/" + name + ".stdout")
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := os.ReadFile("testdata/excerpt/" + name + ".stderr")
	if err != nil {
		t.Fatal(err)
	}
	r := RunEvidence{Output: string(out), Stderr: string(stderr), ExitCode: exit}
	if name != "format" {
		if err = parseTests(&r, Command{TestID: testID}); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

var excerptGoTestArgv = []string{"go", "test", "-json", "-count=1", "-a", "-x", "./..."}

// Engine feedback: a rework turn sees the build errors and failed tests that
// failed its run, never the -x command trace or passing tests' output.
func TestFailureExcerptRecordedRuns(t *testing.T) {
	for _, c := range []struct {
		name, fixture, testID string
		argv                  []string
		exit                  int
		want                  string
	}{
		{name: "build failure", fixture: "build", testID: "TestNope", argv: []string{"go", "test", "-json", "-count=1", "-a", "-x", "-run", "^TestNope$", "./engine/"}, exit: 1, want: `# example.com/fixture/engine [example.com/fixture/engine.test]
engine/engine.go:4:9: undefined: undefinedName
engine/engine.go:7:30: cannot use 7 (untyped int constant) as string value in return statement

FAIL	example.com/fixture/engine [build failed]

exit code 1, executed 0, matched 0
`},
		{name: "test failure", fixture: "test", testID: "TestBehavior", argv: excerptGoTestArgv, exit: 1, want: `=== RUN   TestBehavior
computing the answer
    failing_test.go:11: got 0 want 42
--- FAIL: TestBehavior (0.00s)
=== RUN   TestSub
--- FAIL: TestSub (0.00s)
=== RUN   TestSub/inner
    failing_test.go:21: inner failure
--- FAIL: TestSub/inner (0.00s)

FAIL	example.com/fixture/failing	0.002s
ok  	example.com/fixture/good	0.001s

exit code 1, executed 5, matched 1
`},
		{name: "mixed packages", fixture: "mixed", testID: "TestBehavior", argv: excerptGoTestArgv, exit: 1, want: `# example.com/fixture/engine [example.com/fixture/engine.test]
engine/engine.go:4:9: undefined: undefinedName
engine/engine.go:7:30: cannot use 7 (untyped int constant) as string value in return statement

=== RUN   TestBehavior
computing the answer
    failing_test.go:11: got 0 want 42
--- FAIL: TestBehavior (0.00s)
=== RUN   TestSub
--- FAIL: TestSub (0.00s)
=== RUN   TestSub/inner
    failing_test.go:21: inner failure
--- FAIL: TestSub/inner (0.00s)

FAIL	example.com/fixture/engine [build failed]
FAIL	example.com/fixture/failing	0.002s
ok  	example.com/fixture/good	0.002s

exit code 1, executed 5, matched 1
`},
		{name: "vet failure", fixture: "vet", argv: excerptGoTestArgv, exit: 1, want: `# [example.com/fixture/vetfail]
vetfail/vet_test.go:9:14: fmt.Printf format %d has arg "not a number" of wrong type string

FAIL	example.com/fixture/vetfail [build failed]

exit code 1, executed 0, matched 0
`},
		{name: "continuation lines", fixture: "cont", argv: excerptGoTestArgv, exit: 1, want: `# example.com/m2/p
p/p.go:4:12: not enough arguments in call to F
	have ()
	want (int)
p/p.go:6:11: cannot use 3 (constant of type int) as I value in variable declaration: int does not implement I (missing method M)

FAIL	example.com/m2/p [build failed]

exit code 1, executed 0, matched 0
`},
		{name: "setup failure", fixture: "missing", argv: excerptGoTestArgv, exit: 1, want: `# example.com/fixture/missing
missing/missing.go:3:8: cannot find module providing package example.com/nothere/pkg: unrecognized import path "example.com/nothere/pkg": reading https://example.com/nothere/pkg?go-get=1: 404 Not Found

FAIL	example.com/fixture/missing [setup failed]

exit code 1, executed 0, matched 0
`},
		{name: "go command failure on stderr", fixture: "badmod", argv: excerptGoTestArgv, exit: 1, want: `go: errors parsing go.mod:
go.mod:5: syntax error (unterminated block started at /work/fixture/go.mod:4:1)

exit code 1, executed 0, matched 0
`},
		{name: "format", fixture: "format", argv: []string{"gofmt", "-l", "."}, exit: 2, want: `gofmt -l lists files whose formatting differs:
a.go
sub/b.go

broken.go:2:6: expected 'IDENT', found '{'

exit code 2, 2 files listed
`},
	} {
		t.Run(c.name, func(t *testing.T) {
			run := recordedRun(t, c.fixture, c.exit, c.testID)
			got, truncated := FailureExcerpt(c.argv, run, nil)
			if got != c.want || truncated {
				t.Fatalf("excerpt (truncated %v):\n%s\nwant:\n%s", truncated, got, c.want)
			}
		})
	}
}

// A panicking test keeps its stack, worktree paths read relative to the
// worktree, and the -x trace of the standard library never reaches the
// excerpt.
func TestFailureExcerptPanicAndTrace(t *testing.T) {
	run := recordedRun(t, "panic", 1, "TestPanics")
	got, _ := FailureExcerpt(excerptGoTestArgv, run, nil)
	for _, want := range []string{"=== RUN   TestPanics\n--- FAIL: TestPanics (0.00s)\npanic: assignment to entry in nil map", "panics_test.go:7", "FAIL\texample.com/fixture/panics\t0.004s\n\nexit code 1, executed 1, matched 1\n"} {
		if !strings.Contains(got, want) {
			t.Fatalf("excerpt lacks %q:\n%s", want, got)
		}
	}
	// Paths under the worktree read relative to it.
	if got, _ := FailureExcerpt(excerptGoTestArgv, run, nil, "/elsewhere", "/work/fixture/"); !strings.Contains(got, "\tpanics/panics_test.go:7 +0x28\n") || strings.Contains(got, "/work/fixture") {
		t.Fatalf("worktree paths not relative:\n%s", got)
	}
	listed := RunEvidence{Output: "/w/tree/answer.go\n/w/tree/sub/b.go\n"}
	if got, _ := FailureExcerpt([]string{"gofmt", "-l", "."}, listed, nil, "/w/tree"); got != "gofmt -l lists files whose formatting differs:\nanswer.go\nsub/b.go\n\nexit code 0, 2 files listed\n" {
		t.Fatalf("format paths not relative:\n%s", got)
	}
	for _, name := range []string{"build", "test", "mixed", "vet", "panic", "missing", "cont"} {
		got, _ := FailureExcerpt(excerptGoTestArgv, recordedRun(t, name, 1, ""), nil)
		for _, line := range strings.Split(got, "\n") {
			for _, trace := range []string{"mkdir ", "cd ", "cat ", "WORK=", "/usr/local/go/pkg/tool/", "packagefile ", "EOF", "# get ", "go: finding "} {
				if strings.HasPrefix(line, trace) || strings.Contains(line, "/compile ") {
					t.Fatalf("%s: -x trace line kept: %q", name, line)
				}
			}
		}
	}
}

// A QA run folds the target's required checks: a failed check adds its own
// section after the suite's, and the final line states the folded counts.
func TestFailureExcerptRequiredChecks(t *testing.T) {
	recorded := recordedRun(t, "test", 0, "")
	var passing []string
	for _, line := range strings.Split(recorded.Output, "\n") {
		if strings.Contains(line, `"Package":"example.com/fixture/good"`) {
			passing = append(passing, line)
		}
	}
	suite := RunEvidence{Output: strings.Join(passing, "\n") + "\n"}
	if err := parseTests(&suite, Command{}); err != nil || suite.ExecutedCount != 1 {
		t.Fatal(err, suite.ExecutedCount)
	}
	check := RunEvidence{ExitCode: 2, Output: "checking imports\n", Stderr: "cmd/fae/main.go: forbidden import\n"}
	passed := RunEvidence{ExitCode: 0, Output: "all good\n"}
	folded := suite
	folded.ExitCode = 2
	got, truncated := FailureExcerpt(excerptGoTestArgv, folded, []RunEvidence{passed, check})
	want := "ok  \texample.com/fixture/good\t0.001s\n\nrequired check 2 failed (exit code 2, executed 0):\nchecking imports\n\ncmd/fae/main.go: forbidden import\n\nexit code 2, executed 1, matched 0\n"
	if truncated || got != want {
		t.Fatalf("excerpt:\n%s\nwant:\n%s", got, want)
	}
}

// Engine feedback is bounded per run: a longer excerpt keeps its head and
// tail around a marked cut, at line boundaries, as valid UTF-8.
func TestFailureExcerptTruncation(t *testing.T) {
	var stream strings.Builder
	stream.WriteString(`{"Action":"start","Package":"example.com/big"}` + "\n")
	stream.WriteString(`{"Action":"run","Package":"example.com/big","Test":"TestBig"}` + "\n")
	stream.WriteString(`{"Action":"output","Package":"example.com/big","Test":"TestBig","Output":"=== RUN   TestBig\n"}` + "\n")
	for i := 0; i < 2000; i++ {
		fmt.Fprintf(&stream, `{"Action":"output","Package":"example.com/big","Test":"TestBig","Output":"    big_test.go:9: line %d ünïcödé\n"}`+"\n", i)
	}
	stream.WriteString(`{"Action":"output","Package":"example.com/big","Test":"TestBig","Output":"--- FAIL: TestBig (0.00s)\n"}` + "\n")
	stream.WriteString(`{"Action":"fail","Package":"example.com/big","Test":"TestBig"}` + "\n")
	stream.WriteString(`{"Action":"output","Package":"example.com/big","Output":"FAIL\texample.com/big\t0.010s\n"}` + "\n")
	stream.WriteString(`{"Action":"fail","Package":"example.com/big"}` + "\n")
	run := RunEvidence{Output: stream.String(), ExitCode: 1}
	if err := parseTests(&run, Command{TestID: "TestBig"}); err != nil {
		t.Fatal(err)
	}
	got, truncated := FailureExcerpt(excerptGoTestArgv, run, nil)
	if !truncated || len(got) > ExcerptLimit || len(got) < ExcerptLimit-200 || !utf8.ValidString(got) {
		t.Fatalf("truncated %v, %d bytes", truncated, len(got))
	}
	if !strings.HasPrefix(got, "=== RUN   TestBig\n    big_test.go:9: line 0 ünïcödé\n") || !strings.HasSuffix(got, "--- FAIL: TestBig (0.00s)\n\nFAIL\texample.com/big\t0.010s\n\nexit code 1, executed 1, matched 1\n") {
		t.Fatalf("head or tail lost:\n%s", got)
	}
	head, tail, ok := strings.Cut(got, "[... ")
	if !ok || !strings.HasSuffix(head, "\n") || !strings.Contains(tail, " bytes cut ...]\n    big_test.go:9: line ") || strings.Count(got, "bytes cut") != 1 {
		t.Fatalf("cut not marked at a line boundary:\n%s", got)
	}

	for _, c := range []struct {
		text  string
		limit int
		want  string
		cut   bool
	}{
		{"short\n", 64, "short\n", false},
		{"bad \xff byte", 64, "bad \uFFFD byte", false},
		{strings.Repeat("é", 100), 10, "ééééé", true},
		{strings.Repeat("a\n", 100), 70, "a\na\na\na\na\n[... 180 bytes cut ...]\na\na\na\na\na\n", true},
	} {
		got, cut := BoundExcerpt(c.text, c.limit)
		if got != c.want || cut != c.cut || len(got) > c.limit || !utf8.ValidString(got) {
			t.Errorf("BoundExcerpt(%q, %d) = %q %v, want %q %v", c.text, c.limit, got, cut, c.want, c.cut)
		}
	}
}
