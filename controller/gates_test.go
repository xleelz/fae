package controller

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/hostguard"
)

func TestGateGuardsExhaustive(t *testing.T) {
	for _, applicable := range []bool{false, true} {
		for examined := 0; examined < 3; examined++ {
			for failures := 0; failures < 3; failures++ {
				if GatePasses(applicable, examined, failures) != (failures == 0 && (examined > 0 || !applicable)) {
					t.Fatal(applicable, examined, failures)
				}
			}
		}
	}
	for mask := 0; mask < 16; mask++ {
		a, b, c, d := mask&1 != 0, mask&2 != 0, mask&4 != 0, mask&8 != 0
		if CanAdvanceTarget(a, b, c, d) != (a && b && c && d) || MayDeployGated(a, b, c) != (a && b && c) {
			t.Fatal(mask)
		}
	}
	if MeetsLandingFloor(false, 20, 10) || MeetsLandingFloor(true, 9, 10) || MeetsLandingFloor(true, 20, 0) || !MeetsLandingFloor(true, 10, 10) {
		t.Fatal("landing floor boundary")
	}
}

func TestGateSelfTestProvesEveryGateCanFail(t *testing.T) {
	report, ok := SelfTest(BuiltinSelfTests())
	if !ok {
		t.Fatalf("%+v", report)
	}
	want := map[string]bool{"memory": true, "io": true, "overlap": true, "format": true, "filter": true, "own-hunk": true, "compile": true, "suite": true, "frozen": true, "qa": true, "landing": true}
	for _, r := range report {
		if !r.CanFail || !r.CanPass {
			t.Fatal(r)
		}
		delete(want, r.Gate)
	}
	if len(want) != 0 {
		t.Fatal("gates without self-test", want)
	}
	// Deliberately breaking any one gate's assertion makes the self-test fail
	// and names the broken gate.
	for i := range BuiltinSelfTests() {
		cases := BuiltinSelfTests()
		original := cases[i].Run
		cases[i].Run = func(v any) GateResult { r := original(v); r.Passed = true; return r }
		report, ok := SelfTest(cases)
		if ok || report[i].CanFail || len(report[i].Problems) == 0 {
			t.Fatal("broken gate passed self-test", cases[i].Gate, report[i])
		}
		cases = BuiltinSelfTests()
		cases[i].Run = func(any) GateResult { return GateResult{} }
		if _, ok = SelfTest(cases); ok {
			t.Fatal("gate that never passes accepted", cases[i].Gate)
		}
	}
	if _, ok := SelfTest(nil); ok {
		t.Fatal("empty self-test accepted")
	}
	panicky := BuiltinSelfTests()[:1]
	panicky[0].Run = func(any) GateResult { panic("broken") }
	if _, ok := SelfTest(panicky); ok {
		t.Fatal("panicking gate accepted")
	}
}

func TestGoTestCountsAndFilter(t *testing.T) {
	const p = "example.invalid/a/b"
	stream := goStream(goTest(p, "TestRequired", "pass"), goTest(p, "TestRequired/sub", "pass"), goTest(p, "TestSkipped", "skip"), goTest(p, "TestOther", "fail"), goPackage(p, "fail"), goPackage("example.invalid/empty", "skip"),
		`{"ImportPath":"example.invalid/broken","Action":"build-output","Output":"bad"}`, `{"ImportPath":"example.invalid/broken","Action":"build-fail"}`, `{"Action":"fail","Package":"example.invalid/broken","FailedBuild":"example.invalid/broken"}`)
	c, e := GoTestCounts([]byte(stream), "TestRequired")
	if e != nil {
		t.Fatal(e)
	}
	if c.Executed != 3 || c.Matched != 1 || c.MatchedPassed != 1 || c.Packages != 3 || len(c.Failed) != 1 || c.Failed[0] != p+":TestOther" || len(c.BuildFailed) != 1 || c.BuildFailed[0] != "example.invalid/broken" {
		t.Fatalf("%+v", c)
	}
	if r := FilterGate(c, "TestRequired"); r.Passed || !strings.Contains(r.Reason, "build failed") {
		t.Fatal(r)
	}
	if _, e = GoTestCounts([]byte("not json\n"), ""); e == nil {
		t.Fatal("malformed stream accepted")
	}
	zero, _ := GoTestCounts([]byte(goStream(goPackage(p, "pass"))), "TestRequired")
	if r := FilterGate(zero, "TestRequired"); r.Passed || r.Count != 0 || !strings.Contains(r.Reason, "zero tests") {
		t.Fatal("vacuous filter", r)
	}
	if r := FilterGate(TestCounts{Matched: 1, MatchedPassed: 1, ExitCode: 2}, "TestRequired"); r.Passed {
		t.Fatal("nonzero exit accepted")
	}
	if r := FilterGate(TestCounts{Matched: 1, MatchedPassed: 1}, ""); r.Passed {
		t.Fatal("missing filter accepted")
	}
	if r := SuiteGate(c, []string{p + ":TestOther"}, nil); r.Passed {
		t.Fatal("build failure tolerated as flake", r)
	}
	c.BuildFailed = nil
	for flakes, want := range map[string]bool{"TestOther": true, p + ":TestOther": true, "example.invalid/x:TestOther": false, "TestOth": false, "": false} {
		if r := SuiteGate(c, strings.Fields(flakes), nil); r.Passed != want || r.Count != 3 {
			t.Fatal(flakes, r)
		}
	}
	sub, _ := GoTestCounts([]byte(goStream(goTest(p, "TestFlaky", "fail"), goTest(p, "TestFlaky/case", "fail"), goTest(p, "TestOther", "pass"), goPackage(p, "fail"))), "")
	if r := SuiteGate(sub, []string{"TestFlaky"}, nil); !r.Passed {
		t.Fatal("listed flake subtest not tolerated", r)
	}
}

func TestFormatItemsAreItemScoped(t *testing.T) {
	base := "package p\n\nfunc Old( ) {}\n\nfunc Keep() {}\n"
	tip := "package p\n\nfunc Old( ) {}\n\nfunc Keep() {}\n\nfunc New( a int) {}\n\n// Doc.\ntype T struct{ A int } // trailing\n"
	diff := "@@ -5,0 +6,4 @@\n+\n+func New( a int) {}\n+\n+// Doc.\n@@ -6 +10 @@\n+type T struct{ A int } // trailing\n"
	items := GoFormatItems("p.go", []byte(base), []byte(tip), ChangedLines(diff))
	if len(items) != 2 || items[0].Item != "func New" || items[0].Formatted || items[0].ExistedBase || items[1].Item != "type T" || !items[1].Formatted {
		t.Fatalf("%+v", items)
	}
	if r := FormatGate(items); r.Passed || !strings.Contains(r.Reason, "func New") || r.Count != 2 {
		t.Fatal(r)
	}
	// Pre-existing debt on a touched but still-unformatted item is not new debt.
	items = GoFormatItems("p.go", []byte(base), []byte(strings.Replace(base, "func Old( ) {}", "func Old( ) { }", 1)), []LineRange{{3, 3}})
	if r := FormatGate(items); !r.Passed || r.Count != 1 {
		t.Fatalf("%+v %+v", r, items)
	}
	if r := FormatGate(GoFormatItems("p.go", nil, []byte("package p\nfunc (\n"), []LineRange{{1, 2}})); r.Passed {
		t.Fatal("unparseable tip accepted")
	}
	if got := ChangedLines("@@ -1,2 +1 @@\n@@ -9 +8,0 @@\n@@ bad\n"); len(got) != 2 || got[0] != (LineRange{1, 1}) || got[1] != (LineRange{8, 9}) {
		t.Fatal(got)
	}
	methods := GoFormatItems("m.go", nil, []byte("package p\n\nfunc (r *R[T]) M() {}\n\nfunc (R) M() {}\n\nvar a, b = 1, 2\n"), []LineRange{{1, 7}})
	if len(methods) != 3 || methods[0].Item != "func (*R) M" || methods[1].Item != "func (R) M" || methods[2].Item != "var a,b" {
		t.Fatalf("%+v", methods)
	}
}

func TestMemoryIOAndOverlapGates(t *testing.T) {
	sample := func(gib float64) hostguard.Sample {
		return hostguard.Sample{Memory: hostguard.Reading{Value: gib * float64(hostguard.GiB), Known: true}}
	}
	if !MemoryGate(sample(12), 12*hostguard.GiB).Passed || MemoryGate(sample(11.99), 12*hostguard.GiB).Passed {
		t.Fatal("landing floor boundary")
	}
	hold, _ := hostguard.NewIOGate(hostguard.DefaultPolicy())
	start := time.Unix(100, 0)
	clear := hostguard.Sample{IO10: hostguard.Reading{Value: 1, Known: true}}
	if r, _ := IOGate(hold, start, clear, hostguard.DefaultConfig()); r.Passed {
		t.Fatal("one reading lifted I/O gate")
	}
	if r, step := IOGate(hold, start.Add(30*time.Second), clear, hostguard.DefaultConfig()); !r.Passed || !step.Lifted {
		t.Fatal("two readings 30s apart refused", r, step)
	}
	if r := OverlapGate(OverlapInput{Changed: []string{"a"}, Touched: []string{"a"}, Acknowledged: true}); !r.Passed {
		t.Fatal("acknowledged overlap refused")
	}
	if r := OverlapGate(OverlapInput{Changed: []string{"a"}, Touched: []string{"a"}, Expired: true}); !r.Passed {
		t.Fatal("expired ordinary overlap held")
	}
	if r := OverlapGate(OverlapInput{Changed: []string{"a"}, Touched: []string{"a"}, Expired: true, TopNearLimit: true}); r.Passed {
		t.Fatal("protected overlap released")
	}
}

func TestHunkCompileFrozenQAAndLandingGates(t *testing.T) {
	if r := FrozenGate(nil); !r.Passed || r.Applicable {
		t.Fatal("undeclared frozen region must be explicitly not applicable", r)
	}
	if r := CompileGate([]CommandRun{{Argv: []string{"go"}, Packages: 1}, {Argv: []string{"go"}, ExitCode: 2}}); r.Passed {
		t.Fatal(r)
	}
	if r := QAGate([]QACheck{{Counts: TestCounts{Executed: 1}}, {Counts: TestCounts{Executed: 0}}}); r.Passed || r.Count != 2 {
		t.Fatal(r)
	}
	if r := OwnHunkGate([]HunkPresence{{File: "a", Present: true}}, []string{"a", "b"}, []string{"a"}); r.Passed || !strings.Contains(r.Reason, "b") {
		t.Fatal(r)
	}
	if r := LandingGate([]GateResult{FrozenGate(nil), {Gate: "suite", Passed: true, Applicable: true, Count: 1}}); !r.Passed || r.Count != 1 {
		t.Fatal(r)
	}
	if r := LandingGate([]GateResult{FrozenGate(nil)}); r.Passed {
		t.Fatal("landing with only inapplicable gates accepted", r)
	}
}

// FAE-CTL-030: a failure is kept under its name. Two packages whose import
// paths a redaction made one, the first failing, and two tests made one in
// the same package, still fail the suite (on the base the last pass won, the
// shape re-audit 8's probe printed as a pass); so does a test that fails and
// then passes in a repeated run. A test that started more often than it ended
// leaves the stream incomplete. A suite that exited nonzero with nothing in
// its stream to explain it fails.
func TestGoTestCountsKeepAFailureUnderOneName(t *testing.T) {
	const p = "example.invalid/m/[redacted]"
	for name, stream := range map[string]string{
		"two packages":          goStream(goTest(p, "TestA", "fail"), goPackage(p, "fail"), goTest(p, "TestA", "pass"), goPackage(p, "pass")),
		"two tests":             goStream(goTest(p, "TestA[redacted]", "fail"), goTest(p, "TestA[redacted]", "pass"), goPackage(p, "fail")),
		"a repeated run":        goStream(goTest(p, "TestA", "fail"), goTest(p, "TestA", "pass"), goPackage(p, "fail")),
		"a failing package run": goStream(goTest(p, "TestA", "pass"), goPackage(p, "fail"), goTest(p, "TestA", "pass"), goPackage(p, "pass")),
	} {
		c, e := GoTestCounts([]byte(stream), "TestA")
		if e != nil {
			t.Fatal(name, e)
		}
		if r := SuiteGate(c, nil, nil); r.Passed {
			t.Errorf("%s: the suite passed: %+v %+v", name, c, r)
		}
		if r := FilterGate(c, "TestA"); r.Passed {
			t.Errorf("%s: the filter passed: %+v %+v", name, c, r)
		}
	}
	twice := goStream(goTest(p, "TestA", "pass"), `{"Action":"run","Package":"`+p+`","Test":"TestA"}`, goPackage(p, "pass"))
	if _, e := GoTestCounts([]byte(twice), ""); e == nil || !strings.Contains(e.Error(), "incomplete test stream") {
		t.Fatal("a test that started twice and ended once was complete", e)
	}
	passing, _ := GoTestCounts([]byte(goStream(goTest(p, "TestA", "pass"), goPackage(p, "pass"))), "")
	passing.ExitCode = 1
	if r := SuiteGate(passing, nil, nil); r.Passed || !strings.Contains(r.Reason, "suite exited 1 with no failed test or build") {
		t.Fatal("an unexplained nonzero exit passed", r)
	}
}

// FAE-CTL-030: a failing listed flake does not explain a nonzero exit on its
// own. A package whose TestMain exits 1 after its tests pass, beside a
// failing listed flake in the same package, prints the same go test stream as
// the flake alone, and a package with no events at all leaves none; on the
// base either passed the suite gate. The exit is explained only by the suite
// run again without the listed flakes (SuiteFlakeSkip), which must exit 0,
// fail nothing and report every package the first run did. SuiteFlakeSkip
// asks for that run only when every failure is a listed flake.
func TestSuiteGateListedFlakeExplainsAnExitOnlyByACleanRun(t *testing.T) {
	const p, q = "example.invalid/m/p", "example.invalid/m/q"
	flakes := []string{"TestFlaky", q + ":TestOtherFlaky"}
	flaky, _ := GoTestCounts([]byte(goStream(goTest(p, "TestA", "pass"), goTest(p, "TestFlaky", "fail"), goPackage(p, "fail"), goTest(q, "TestB", "pass"), goPackage(q, "pass"))), "")
	flaky.ExitCode = 1
	if skip := SuiteFlakeSkip(flaky, flakes); skip != "^(TestFlaky|TestOtherFlaky)$" {
		t.Fatalf("skip pattern %q", skip)
	}
	if r := SuiteGate(flaky, flakes, nil); r.Passed || !strings.Contains(r.Reason, "only listed flakes failing, and no run without them") {
		t.Fatal("a failing listed flake explained the exit on its own", r)
	}
	run := func(exit int, lines ...string) *TestCounts {
		c, e := GoTestCounts([]byte(goStream(lines...)), "")
		if e != nil {
			t.Fatal(e)
		}
		c.ExitCode = exit
		return &c
	}
	for name, c := range map[string]struct {
		clean *TestCounts
		want  string
	}{
		"TestMain exits 1":        {run(1, goTest(p, "TestA", "pass"), goPackage(p, "fail"), goTest(q, "TestB", "pass"), goPackage(q, "pass")), "package failed with no failed test: " + p + ", exited 1"},
		"an unlisted test fails":  {run(1, goTest(p, "TestA", "fail"), goPackage(p, "fail"), goTest(q, "TestB", "pass"), goPackage(q, "pass")), "test failed: " + p + ":TestA, exited 1"},
		"a package has no events": {run(1, goTest(p, "TestA", "pass"), goPackage(p, "pass")), "exited 1, reported 1 of the suite's 2 packages"},
		"exit 0 short a package":  {run(0, goTest(p, "TestA", "pass"), goPackage(p, "pass")), "reported 1 of the suite's 2 packages"},
	} {
		if r := SuiteGate(flaky, flakes, c.clean); r.Passed || !strings.Contains(r.Reason, "but its run without them: "+c.want) {
			t.Errorf("%s: %+v", name, r)
		}
	}
	clean := run(0, goTest(p, "TestA", "pass"), goPackage(p, "pass"), goTest(q, "TestB", "pass"), goPackage(q, "pass"))
	if r := SuiteGate(flaky, flakes, clean); !r.Passed || r.Count != 3 {
		t.Fatal("a listed flake whose clean run passed was not tolerated", r)
	}
	// No run is asked for when the exit is 0, nothing failed, or a charged
	// failure fails the gate anyway.
	charged, _ := GoTestCounts([]byte(goStream(goTest(p, "TestFlaky", "fail"), goTest(p, "TestA", "fail"), goPackage(p, "fail"))), "")
	charged.ExitCode = 1
	passed := *clean
	for name, c := range map[string]TestCounts{"exit 0": passed, "a charged failure": charged, "nothing failed": {ExitCode: 1, Executed: 1, Packages: 1}} {
		if skip := SuiteFlakeSkip(c, flakes); skip != "" {
			t.Errorf("%s: a run without the flakes was asked for: %q", name, skip)
		}
	}
	if r := SuiteGate(charged, flakes, clean); r.Passed {
		t.Fatal("a clean run excused a charged failure", r)
	}
}

// FAE-CTL-030: the suite run again without the listed flakes explains the
// first run's exit only if it ran what the first run ran but the skipped
// flakes, and a flake's name is counted skipped only in its own package. On
// the base each of these passed the suite gate (re-audit 10's probe cases 5
// and 13 among them):
//   - the rerun ran no test at all, or skipped (t.Skip) or lost one test the
//     first run ran;
//   - the first run was killed before a second package (exit -1), so a test
//     there named as a package-qualified flake ran in neither run;
//   - a test named as another package's flake sits in a package the first run
//     failed through its own listed flake, and only the rerun, which skipped
//     it, passed that package.
//
// A namesake that passed in a package the first run passed is shown by that
// run, and the gate passes.
func TestSuiteGateRerunMustRunWhatTheSuiteRanButItsFlakes(t *testing.T) {
	const a, b = "example.invalid/m/a", "example.invalid/m/b"
	counts := func(exit int, lines ...string) TestCounts {
		c, e := GoTestCounts([]byte(goStream(lines...)), "")
		if e != nil {
			t.Fatal(e)
		}
		c.ExitCode = exit
		return c
	}
	first := counts(1, goTest(a, "TestFlaky", "fail"), goTest(a, "TestX", "pass"), goTest(a, "TestY", "pass"), goPackage(a, "fail"))
	flakes := []string{"TestFlaky"}
	for name, c := range map[string]struct {
		first, clean TestCounts
		flakes       []string
		want         string
	}{
		"the rerun ran nothing": {first, counts(0, goPackage(a, "pass")), flakes, "did not run 2 test(s) the suite ran: " + a + ":TestX, " + a + ":TestY"},
		"the rerun skipped one": {first, counts(0, goTest(a, "TestX", "pass"), goTest(a, "TestY", "skip"), goPackage(a, "pass")), flakes, "did not run 1 test(s) the suite ran: " + a + ":TestY"},
		"the rerun lost one":    {first, counts(0, goTest(a, "TestX", "pass"), goPackage(a, "pass")), flakes, "did not run 1 test(s) the suite ran: " + a + ":TestY"},
		"the suite was cut before a namesake's package": {
			counts(-1, goTest(a, "TestFlaky", "fail"), goTest(a, "TestX", "pass"), goPackage(a, "fail")),
			counts(0, goTest(a, "TestX", "pass"), goPackage(a, "pass"), goTest(b, "TestZ", "pass"), goPackage(b, "pass")),
			[]string{a + ":TestFlaky"}, "reported 1 package(s) the suite did not finish: " + b},
		"a namesake in a package the suite failed": {
			counts(1, goTest(a, "TestFlaky", "fail"), goTest(a, "TestX", "pass"), goPackage(a, "fail"), goTest(b, "TestFlaky", "pass"), goTest(b, "TestOtherFlaky", "fail"), goTest(b, "TestZ", "pass"), goPackage(b, "fail")),
			counts(0, goTest(a, "TestX", "pass"), goPackage(a, "pass"), goTest(b, "TestZ", "pass"), goPackage(b, "pass")),
			[]string{a + ":TestFlaky", b + ":TestOtherFlaky"}, "skipped 1 test(s) that are not listed flakes in their package, in a package the suite failed or did not finish: " + b + ":TestFlaky"},
	} {
		if skip := SuiteFlakeSkip(c.first, c.flakes); skip == "" {
			t.Fatalf("%s: no rerun asked for", name)
		}
		if r := SuiteGate(c.first, c.flakes, &c.clean); r.Passed || !strings.Contains(r.Reason, "but its run without them: "+c.want) {
			t.Errorf("%s: %+v", name, r)
		}
	}
	for name, c := range map[string]struct {
		first, clean TestCounts
		flakes       []string
	}{
		"the rerun ran the rest": {first, counts(0, goTest(a, "TestX", "pass"), goTest(a, "TestY", "pass"), goPackage(a, "pass")), flakes},
		"a namesake passed in a package the suite passed": {
			counts(1, goTest(a, "TestFlaky", "fail"), goTest(a, "TestX", "pass"), goPackage(a, "fail"), goTest(b, "TestFlaky", "pass"), goTest(b, "TestZ", "pass"), goPackage(b, "pass")),
			counts(0, goTest(a, "TestX", "pass"), goPackage(a, "pass"), goTest(b, "TestZ", "pass"), goPackage(b, "pass")),
			[]string{a + ":TestFlaky"}},
	} {
		if r := SuiteGate(c.first, c.flakes, &c.clean); !r.Passed {
			t.Errorf("%s: %+v", name, r)
		}
	}
}

// FAE-CTL-030: go test rewrites spaces in subtest names and -run filter levels
// to underscores before matching and emitting events (TestSub/a b is emitted
// as TestSub/a_b). Both ExactTestFilter("TestSub/a b") and
// NewTestFilter("TestSub/a b") match TestSub/a_b emitted by go test -json and
// construct -run arguments that go test accepts.
func TestSubtestSpaceFilterMatchesGoTestOutput(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.invalid/subspace\n\ngo 1.23\n"), 0600); err != nil {
		t.Fatal(err)
	}
	src := "package subspace\n\nimport \"testing\"\n\nfunc TestSub(t *testing.T) {\n\tt.Run(\"a b\", func(t *testing.T) {})\n\tt.Run(\"other case\", func(t *testing.T) { t.Fatal(\"should not match\") })\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "sub_test.go"), []byte(src), 0600); err != nil {
		t.Fatal(err)
	}
	for _, filter := range []string{"TestSub/a b", ExactTestFilter("TestSub/a b")} {
		tf, err := NewTestFilter(filter)
		if err != nil {
			t.Fatalf("%q: %v", filter, err)
		}
		if !tf.Matches("TestSub/a_b") || !tf.Matches("TestSub/a b") || tf.Matches("TestSub/other_case") {
			t.Fatalf("%q: unexpected Matches behavior", filter)
		}
		argv := GoFilterCommand(filter)
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GOFLAGS=", "GOWORK=off")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("%q (%v): %v\n%s", filter, argv, err, out)
		}
		counts, err := GoTestCounts(out, filter)
		if err != nil {
			t.Fatalf("%q: %v", filter, err)
		}
		if counts.Matched != 1 || counts.MatchedPassed != 1 {
			t.Fatalf("%q: counts=%+v, want 1 matched passing\n%s", filter, counts, out)
		}
		if r := FilterGate(counts, filter); !r.Passed || r.Count != 1 {
			t.Fatalf("%q: FilterGate=%+v", filter, r)
		}
	}
}
