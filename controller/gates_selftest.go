package controller

import (
	"fmt"
	"strings"
	"time"

	"github.com/xleelz/fae/hostguard"
)

// SelfTestCase proves one built-in gate can refuse and can pass
// (FAE-CTL-050). Run evaluates the gate exactly as production does.
type SelfTestCase struct {
	Gate   string
	Run    func(input any) GateResult
	Refuse []SelfTestInput
	Pass   SelfTestInput
}
type SelfTestInput struct {
	Name  string
	Value any
}
type SelfTestResult struct {
	Gate     string   `json:"gate"`
	CanFail  bool     `json:"can_fail"`
	CanPass  bool     `json:"can_pass"`
	Problems []string `json:"problems,omitempty"`
}

// SelfTest runs every case; it fails when any gate cannot fail on one of its
// vacuous or failing inputs, or cannot pass its passing input.
func SelfTest(cases []SelfTestCase) ([]SelfTestResult, bool) {
	ok := len(cases) > 0
	out := []SelfTestResult{}
	for _, c := range cases {
		r := SelfTestResult{Gate: c.Gate, CanFail: len(c.Refuse) > 0}
		for _, in := range c.Refuse {
			if got := safeRun(c, in); got.Passed {
				r.CanFail = false
				r.Problems = append(r.Problems, "accepted "+in.Name+" input")
			}
		}
		if len(c.Refuse) == 0 {
			r.Problems = append(r.Problems, "no refusing input")
		}
		got := safeRun(c, c.Pass)
		r.CanPass = got.Passed
		if !got.Passed {
			r.Problems = append(r.Problems, "refused "+c.Pass.Name+" input: "+got.Reason)
		}
		ok = ok && r.CanFail && r.CanPass
		out = append(out, r)
	}
	return out, ok
}

func safeRun(c SelfTestCase, in SelfTestInput) (r GateResult) {
	defer func() {
		if p := recover(); p != nil {
			r = GateResult{Gate: c.Gate, Reason: fmt.Sprint("self-test panic: ", p)}
		}
	}()
	return c.Run(in.Value)
}

type memoryInput struct {
	available float64
	known     bool
	floor     int64
}
type ioReading struct {
	at   time.Duration
	io10 float64
}
type streamInput struct {
	stream string
	filter string
	exit   int
}

// suiteInput is a suite run and, when set, its run without the listed flakes
// (SuiteGate's clean).
type suiteInput struct {
	streamInput
	clean *streamInput
}
type hunkInput struct {
	hunks            []HunkPresence
	reviewed, landed []string
}
type formatInput struct{ base, tip string }

func goStream(lines ...string) string { return strings.Join(lines, "\n") + "\n" }
func goTest(pkg, test, end string) string {
	return fmt.Sprintf(`{"Action":"run","Package":%q,"Test":%q}`, pkg, test) + "\n" + fmt.Sprintf(`{"Action":%q,"Package":%q,"Test":%q}`, end, pkg, test)
}
func goPackage(pkg, end string) string { return fmt.Sprintf(`{"Action":%q,"Package":%q}`, end, pkg) }

// BuiltinSelfTests covers every built-in gate named by FAE-CTL-050.
func BuiltinSelfTests() []SelfTestCase {
	streamGate := func(gate func(TestCounts, string) GateResult) func(any) GateResult {
		return func(v any) GateResult {
			in := v.(streamInput)
			c, e := GoTestCounts([]byte(in.stream), in.filter)
			if e != nil {
				return GateResult{Reason: e.Error()}
			}
			c.ExitCode = in.exit
			return gate(c, in.filter)
		}
	}
	const pkg = "example.invalid/selftest"
	required := goStream(goTest(pkg, "TestRequired", "pass"), goTest(pkg, "TestOther", "pass"), goPackage(pkg, "pass"))
	// A failing listed flake: the suite's exit is explained only by a run
	// without it that passes.
	flaky := streamInput{stream: goStream(goTest(pkg, "TestOther", "pass"), goTest(pkg, "TestFlaky", "fail"), goPackage(pkg, "fail")), filter: "TestFlaky", exit: 1}
	// The same flake listed for its own package only, in a run cut short
	// before a second package: the run without it skips the name there too,
	// so a namesake in that package ran in neither run.
	const other = "example.invalid/selftest/other"
	qualified := flaky
	qualified.filter, qualified.exit = pkg+":TestFlaky", -1
	cutClean := &streamInput{stream: goStream(goTest(pkg, "TestOther", "pass"), goPackage(pkg, "pass"), goTest(other, "TestNamesake", "pass"), goPackage(other, "pass"))}
	// Two packages each fail only a flake listed for it, and one also holds a
	// test named as the other's flake, which the run without them skips.
	namesake := suiteInput{
		streamInput: streamInput{stream: goStream(goTest(pkg, "TestFlaky", "fail"), goTest(pkg, "TestOther", "pass"), goPackage(pkg, "fail"), goTest(other, "TestFlaky", "pass"), goTest(other, "TestOtherFlaky", "fail"), goPackage(other, "fail")), filter: pkg + ":TestFlaky " + other + ":TestOtherFlaky", exit: 1},
		clean:       &streamInput{stream: goStream(goTest(pkg, "TestOther", "pass"), goPackage(pkg, "pass"), goPackage(other, "pass"))},
	}
	return []SelfTestCase{
		{Gate: "memory", Run: func(v any) GateResult {
			in := v.(memoryInput)
			return MemoryGate(hostguard.Sample{Memory: hostguard.Reading{Value: in.available, Known: in.known}}, in.floor)
		}, Refuse: []SelfTestInput{
			{"below-floor", memoryInput{11 * float64(hostguard.GiB), true, 12 * hostguard.GiB}},
			{"unreadable", memoryInput{64 * float64(hostguard.GiB), false, 12 * hostguard.GiB}},
			{"vacuous-floor", memoryInput{64 * float64(hostguard.GiB), true, 0}},
		}, Pass: SelfTestInput{"at-floor", memoryInput{12 * float64(hostguard.GiB), true, 12 * hostguard.GiB}}},
		{Gate: "io", Run: func(v any) GateResult {
			hold, e := hostguard.NewIOGate(hostguard.DefaultPolicy())
			if e != nil {
				return GateResult{Reason: e.Error()}
			}
			start := time.Unix(1, 0)
			var r GateResult
			for _, reading := range v.([]ioReading) {
				r, _ = IOGate(hold, start.Add(reading.at), hostguard.Sample{IO10: hostguard.Reading{Value: reading.io10, Known: true}}, hostguard.DefaultConfig())
			}
			return r
		}, Refuse: []SelfTestInput{
			{"pressure", []ioReading{{0, 6}}},
			{"single-reading", []ioReading{{0, 1}}},
			{"unspaced", []ioReading{{0, 1}, {10 * time.Second, 1}}},
			{"at-threshold", []ioReading{{0, 5}, {30 * time.Second, 5}}},
			{"second-above", []ioReading{{0, 1}, {30 * time.Second, 5.5}}},
		}, Pass: SelfTestInput{"two-readings", []ioReading{{0, 1}, {30 * time.Second, 1}}}},
		{Gate: "overlap", Run: func(v any) GateResult { return OverlapGate(v.(OverlapInput)) }, Refuse: []SelfTestInput{
			{"overlap", OverlapInput{Changed: []string{"a.go"}, Touched: []string{"a.go"}}},
			{"query-failed", OverlapInput{Changed: []string{"a.go"}, QueryFailed: true, Expired: true}},
			{"critical-expired", OverlapInput{Changed: []string{"a.go"}, Touched: []string{"a.go"}, Expired: true, Critical: true}},
		}, Pass: SelfTestInput{"disjoint", OverlapInput{Changed: []string{"a.go"}, Touched: []string{"b.go"}}}},
		{Gate: "format", Run: func(v any) GateResult {
			in := v.(formatInput)
			var base []byte
			if in.base != "" {
				base = []byte(in.base)
			}
			return FormatGate(GoFormatItems("item.go", base, []byte(in.tip), []LineRange{{3, 3}}))
		}, Refuse: []SelfTestInput{
			{"vacuous", formatInput{tip: "package p\n\n\n"}},
			{"new-debt", formatInput{base: "package p\n\nfunc F() {}\n", tip: "package p\n\nfunc F( ) {  }\n"}},
		}, Pass: SelfTestInput{"formatted", formatInput{base: "package p\n\nfunc F( ) {}\n", tip: "package p\n\nfunc F() int { return 1 }\n"}}},
		{Gate: "filter", Run: streamGate(FilterGate), Refuse: []SelfTestInput{
			{"zero-matched", streamInput{goStream(goTest(pkg, "TestOther", "pass"), goPackage(pkg, "pass")), "TestRequired", 0}},
			{"matched-failing", streamInput{goStream(goTest(pkg, "TestRequired", "fail"), goPackage(pkg, "fail")), "TestRequired", 1}},
			{"matched-skipped", streamInput{goStream(goTest(pkg, "TestRequired", "pass"), goTest(pkg, "TestRequiredSkipped", "skip"), goPackage(pkg, "pass")), "TestRequired", 0}},
			{"incomplete", streamInput{`{"Action":"run","Package":"p","Test":"TestRequired"}`, "TestRequired", 0}},
		}, Pass: SelfTestInput{"matched-passing", streamInput{required, "TestRequired", 0}}},
		{Gate: "own-hunk", Run: func(v any) GateResult { in := v.(hunkInput); return OwnHunkGate(in.hunks, in.reviewed, in.landed) }, Refuse: []SelfTestInput{
			{"vacuous", hunkInput{}},
			{"dropped-hunk", hunkInput{[]HunkPresence{{"a.go", "@@ -1 +1 @@", true}, {"a_test.go", "@@ -1 +1 @@", false}}, []string{"a.go", "a_test.go"}, []string{"a.go", "a_test.go"}}},
			{"dropped-file", hunkInput{[]HunkPresence{{"a_test.go", "@@ -1 +1 @@", true}}, []string{"a.go", "a_test.go"}, []string{"a_test.go"}}},
		}, Pass: SelfTestInput{"all-present", hunkInput{[]HunkPresence{{"a.go", "@@ -1 +1 @@", true}}, []string{"a.go"}, []string{"a.go"}}}},
		{Gate: "compile", Run: func(v any) GateResult { return CompileGate(v.([]CommandRun)) }, Refuse: []SelfTestInput{
			{"vacuous", []CommandRun{}},
			{"zero-packages", []CommandRun{{Argv: []string{"go", "test"}}}},
			{"failing", []CommandRun{{Argv: []string{"go", "test"}, ExitCode: 1, Packages: 2}}},
		}, Pass: SelfTestInput{"compiled", []CommandRun{{Argv: []string{"go", "test"}, Packages: 2}}}},
		{Gate: "suite", Run: func(v any) GateResult {
			in := v.(suiteInput)
			counts := func(in streamInput) (TestCounts, error) {
				c, e := GoTestCounts([]byte(in.stream), "")
				c.ExitCode = in.exit
				return c, e
			}
			c, e := counts(in.streamInput)
			if e != nil {
				return GateResult{Reason: e.Error()}
			}
			var clean *TestCounts
			if in.clean != nil {
				again, e := counts(*in.clean)
				if e != nil {
					return GateResult{Reason: e.Error()}
				}
				clean = &again
			}
			return SuiteGate(c, strings.Fields(in.filter), clean)
		}, Refuse: []SelfTestInput{
			{"zero-executed", suiteInput{streamInput: streamInput{stream: goStream(goPackage(pkg, "pass"))}}},
			{"unlisted-failure", suiteInput{streamInput: streamInput{stream: goStream(goTest(pkg, "TestOther", "fail"), goPackage(pkg, "fail")), filter: "TestFlaky"}}},
			{"build-failure", suiteInput{streamInput: streamInput{stream: goStream(goTest(pkg, "TestOther", "pass"), `{"Action":"fail","Package":"example.invalid/broken","FailedBuild":"example.invalid/broken"}`)}}},
			{"failure-then-pass-under-one-name", suiteInput{streamInput: streamInput{stream: goStream(goTest(pkg, "TestOther", "fail"), goPackage(pkg, "fail"), goTest(pkg, "TestOther", "pass"), goPackage(pkg, "pass"))}}},
			{"unexplained-exit", suiteInput{streamInput: streamInput{stream: goStream(goTest(pkg, "TestOther", "pass"), goPackage(pkg, "pass")), exit: 1}}},
			{"listed-flake-alone", suiteInput{streamInput: flaky}},
			{"listed-flake-beside-another-exit", suiteInput{streamInput: flaky, clean: &streamInput{stream: goStream(goTest(pkg, "TestOther", "pass"), goPackage(pkg, "fail")), exit: 1}}},
			{"listed-flake-rerun-ran-nothing", suiteInput{streamInput: flaky, clean: &streamInput{stream: goStream(goPackage(pkg, "pass"))}}},
			{"listed-flake-namesake-in-an-unfinished-package", suiteInput{streamInput: qualified, clean: cutClean}},
			{"listed-flake-namesake-in-a-failed-package", namesake},
		}, Pass: SelfTestInput{"listed-flake", suiteInput{streamInput: flaky, clean: &streamInput{stream: goStream(goTest(pkg, "TestOther", "pass"), goPackage(pkg, "pass"))}}}},
		{Gate: "frozen", Run: func(v any) GateResult { return FrozenGate(v.([]CommandRun)) }, Refuse: []SelfTestInput{
			{"failing", []CommandRun{{Argv: []string{"frozen-check"}, ExitCode: 3}}},
		}, Pass: SelfTestInput{"passing", []CommandRun{{Argv: []string{"frozen-check"}}}}},
		{Gate: "qa", Run: func(v any) GateResult { return QAGate(v.([]QACheck)) }, Refuse: []SelfTestInput{
			{"vacuous", []QACheck{}},
			{"zero-executed", []QACheck{{Argv: []string{"go", "test"}}}},
			{"failing", []QACheck{{Argv: []string{"go", "test"}, Counts: TestCounts{ExitCode: 1, Executed: 3}}}},
		}, Pass: SelfTestInput{"passing", []QACheck{{Argv: []string{"go", "test"}, Counts: TestCounts{Executed: 3}}}}},
		{Gate: "landing", Run: func(v any) GateResult { return LandingGate(v.([]GateResult)) }, Refuse: []SelfTestInput{
			{"vacuous", []GateResult{}},
			{"one-failed", []GateResult{{Gate: "suite", Passed: true, Applicable: true, Count: 4}, {Gate: "filter", Applicable: true}}},
			{"forged-pass", []GateResult{{Gate: "filter", Passed: true, Applicable: true, Count: 0}}},
		}, Pass: SelfTestInput{"all-passed", []GateResult{{Gate: "suite", Passed: true, Applicable: true, Count: 4}, {Gate: "frozen", Passed: true}}}},
	}
}
