package effects

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/worktree"
)

// redOutcomeSource is a module whose tests each fail one way: by assertion,
// by a setup failure (after logging an unrelated line, in TestLogFirst), by a
// runtime panic, by hanging, by os.Exit and by a panic outside the test's
// goroutine. The last three leave go test (1.27) with no result of the test's
// own, only the package's fail.
var redOutcomeSource = map[string]string{
	"go.mod": "module example.com/redoutcome\n\ngo 1.23\n",
	"outcome.go": `package redoutcome

type box struct{ v int }

func answer() int                             { return 0 }
func lookup(m map[string]*box, k string) int { return m[k].v }
func index(xs []int, i int) int               { return xs[i] }
`,
	"outcome_test.go": `package redoutcome

import (
	"os"
	"testing"
	"time"
)

func TestAssert(t *testing.T) {
	if got := answer(); got != 42 {
		t.Errorf("answer() = %d, want 42", got)
	}
}
func TestSetup(t *testing.T) { t.Fatal("setup: open testdata/fixture.json: no such file") }
func TestLogFirst(t *testing.T) {
	t.Log("widget cache missing")
	t.Fatal("setup: open testdata/fixture.json: no such file")
}
func TestNilPointer(t *testing.T) { _ = lookup(map[string]*box{}, "x") }
func TestIndex(t *testing.T)      { _ = index([]int{1, 2}, 3) }
func TestNilMap(t *testing.T) {
	var m map[string]int
	m["x"] = 1
}
func TestHang(t *testing.T) {
	t.Log("waiting for the reply")
	time.Sleep(time.Hour)
}
func TestExit(t *testing.T) { os.Exit(3) }
func TestBackground(t *testing.T) {
	go func() { panic("background boom") }()
	time.Sleep(time.Hour)
}
`,
}

func redOutcomeSetup(t *testing.T) (*Broker, worktree.Binding) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "repo")
	if err = os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	gitTest(t, repo, "init")
	for name, body := range redOutcomeSource {
		write(t, repo, name, body)
	}
	gitTest(t, repo, "add", ".")
	gitTest(t, repo, "commit", "-m", "outcomes")
	m := &worktree.Manager{Repository: repo, Root: filepath.Join(root, "trees")}
	w, err := m.Ensure(context.Background(), "run", "task", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	argv := []string{"go", "test", "-json", "-count=1", "./..."}
	return &Broker{Manager: m, Environment: []string{"GOCACHE=" + os.Getenv("GOCACHE")}, Allowed: [][]string{argv, append(slices.Clone(argv[:4]), "-timeout=0", "./...")}}, w
}

// redOutcomeRun runs one outcome test the way the RED stage does: shaped to
// the test, under a deadline, in a broker with no residue left behind.
func redOutcomeRun(t *testing.T, b *Broker, w worktree.Binding, argv []string, id string, deadline time.Duration) (RunEvidence, time.Duration) {
	t.Helper()
	start := time.Now()
	r, err := b.Run(context.Background(), w, Command{Argv: argv, Timeout: deadline, TestID: id, Shape: "go-unit"})
	if err != nil {
		t.Fatalf("%s: %v (%+v)", id, err, r)
	}
	residue, err := filepath.Glob(filepath.Join(filepath.Dir(w.Path), "command-*"))
	if err != nil || len(residue) != 0 {
		t.Fatal("host command residue", residue, err)
	}
	return r, time.Since(start)
}

// matchesRed reports whether fragment is the expected failure r proves for
// test id.
func matchesRed(t *testing.T, r RunEvidence, id, fragment string) bool {
	t.Helper()
	again := RunEvidence{ExitCode: r.ExitCode, Output: r.Output, TimedOut: r.TimedOut, Truncated: r.Truncated}
	if err := parseTests(&again, Command{TestID: id, ExpectedFailure: fragment}); err != nil {
		t.Fatal(err)
	}
	return again.ExpectedFailure
}

// FAE-WF-036/073: a RED that hangs fails for a reason the RED gate can read.
// The broker gives a shaped go test run go test's own -timeout of half the
// run's deadline, so go test's alarm fires inside the run and prints "panic:
// test timed out after …" as the hung test's output. go test 1.27 reports no
// result for the test itself, only its package's fail; the broker records the
// test as executed and failed, and its fragment matches. Before, the broker's
// deadline killed go test first and the run was an error, which the live
// HostCommand turns into an UncertainDispatch park.
func TestBrokerHungTestFailsWithGoTestTimeout(t *testing.T) {
	b, w := redOutcomeSetup(t)
	r, took := redOutcomeRun(t, b, w, b.Allowed[0], "TestHang", 20*time.Second)
	if r.TimedOut || r.ExitCode != 1 || r.ExecutedCount != 1 || r.MatchedCount != 1 || r.RedPassed || took >= 20*time.Second {
		t.Fatalf("hung test run %+v in %s", r, took)
	}
	for fragment, want := range map[string]bool{"test timed out": true, "panic: test timed out after 10s": true, "waiting for the reply": true, "TestHang (10s)": true, "goroutine": false, "time.Sleep": false, "outcome_test.go:": true, "redoutcome.TestHang": false} {
		if got := matchesRed(t, r, "TestHang", fragment); got != want {
			t.Errorf("fragment %q: expected failure %v, want %v", fragment, got, want)
		}
	}
	// The rework turn's excerpt carries the hung test's output.
	text, _ := FailureExcerpt(b.Allowed[0], r, nil, w.Path)
	for _, want := range []string{"=== RUN   TestHang", "waiting for the reply", "panic: test timed out after 10s", "FAIL\texample.com/redoutcome", "exit code 1, executed 1, matched 1\n"} {
		if !strings.Contains(text, want) {
			t.Errorf("excerpt lacks %q:\n%s", want, text)
		}
	}
}

// A test whose binary ends while it runs, by os.Exit or by a panic outside
// the test's goroutine, gets no result of its own from go test, only its
// package's fail. It reported no failure itself, so it is not counted as
// executed or matched, and a background panic's message is not its failure
// text (re-audit 9). The run is still evidence, not an error: a failed run
// whose excerpt shows the test's output and says it was not counted.
func TestBrokerTestBinaryEndedWithoutTestResult(t *testing.T) {
	b, w := redOutcomeSetup(t)
	for id, reason := range map[string]string{"TestExit": "", "TestBackground": "panic: background boom"} {
		r, _ := redOutcomeRun(t, b, w, b.Allowed[0], id, time.Minute)
		if r.TimedOut || r.ExitCode != 1 || r.ExecutedCount != 0 || r.MatchedCount != 0 || r.RedPassed {
			t.Fatalf("%s run %+v", id, r)
		}
		if reason != "" && matchesRed(t, r, id, reason) {
			t.Errorf("%s: fragment %q proves a failure the test never reported", id, reason)
		}
		text, _ := FailureExcerpt(b.Allowed[0], r, nil, w.Path)
		if !strings.Contains(text, "=== RUN   "+id) || !strings.Contains(text, reason) || !strings.Contains(text, id+" reported no result of its own") || !strings.Contains(text, "executed 0, matched 0") {
			t.Errorf("%s excerpt:\n%s", id, text)
		}
	}
}

// A run the broker kills at its deadline is a failed run, not an error: its
// process group is retired, it is marked TimedOut with exit code -1, its
// output is kept, and it proves no expected failure. The test still running
// reported no result of its own, so it is not counted as executed. A
// caller's cancellation is still an error: that run's outcome is unknown.
func TestBrokerDeadlineKillIsTimedOutFailure(t *testing.T) {
	b, w := redOutcomeSetup(t)
	// The declared vector disables go test's own alarm, so only the broker's
	// deadline ends the hung test.
	r, took := redOutcomeRun(t, b, w, b.Allowed[1], "TestHang", 8*time.Second)
	if !r.TimedOut || r.ExitCode != -1 || r.ExecutedCount != 0 || r.MatchedCount != 0 || r.RedPassed || r.ExpectedFailure || took > 15*time.Second {
		t.Fatalf("deadline run %+v in %s", r, took)
	}
	if matchesRed(t, r, "TestHang", "waiting for the reply") {
		t.Fatal("a run killed at its deadline proved an expected failure")
	}
	text, _ := FailureExcerpt(b.Allowed[1], r, nil, w.Path)
	if !strings.Contains(text, "waiting for the reply") || !strings.Contains(text, "TestHang reported no result of its own") || !strings.HasSuffix(text, "exit code -1, executed 0, matched 0, killed at the run's deadline\n") {
		t.Fatalf("deadline excerpt:\n%s", text)
	}
	// Any other command killed at its deadline is a timed-out failure too.
	b.Allowed = append(b.Allowed, []string{"sleep", "30"})
	sleep, err := b.Run(context.Background(), w, Command{Argv: b.Allowed[2], Timeout: time.Second})
	if err != nil || !sleep.TimedOut || sleep.ExitCode != -1 {
		t.Fatalf("sleep past its deadline: %+v %v", sleep, err)
	}
	// A cancelled caller leaves the outcome unknown.
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	if cancelled, err := b.Run(ctx, w, Command{Argv: b.Allowed[2], Timeout: time.Minute}); err == nil || cancelled.TimedOut {
		t.Fatalf("cancelled run %+v %v", cancelled, err)
	}
}

// FAE-WF-073: the expected failure is matched against the test's failure
// text, not the goroutine dump a panic or timeout prints (nor any other stack
// in its output: TestRedFailureTextReadsPastAStackTheTestContinues). The dump names the
// module, the worktree's path, the production functions and files and holds
// hex words whatever the reason, so fragments taken from it never match;
// each failure's own message matches only that failure. Text a test logs
// before it fails for another reason still matches (TestLogFirst): the
// broker cannot tell it from the failure's own message, and the reviewers
// judge the RED's source. Generic words of a runtime panic's message
// ("runtime error") and bare numbers still match failures of any cause; the
// daemon's reason rule refuses those.
func TestRedFailureTextExcludesGoroutineDump(t *testing.T) {
	b, w := redOutcomeSetup(t)
	runs := map[string]RunEvidence{}
	for _, id := range []string{"TestAssert", "TestSetup", "TestLogFirst", "TestNilPointer", "TestIndex", "TestNilMap", "TestHang"} {
		runs[id], _ = redOutcomeRun(t, b, w, b.Allowed[0], id, 20*time.Second)
	}
	for _, c := range []struct {
		fragment string
		matches  []string
	}{
		// From the dump: the module, paths, frames, production files.
		{"example.com", nil},
		{"example.com/redoutcome", nil},
		{"redoutcome.lookup", nil},
		{"lookup(...)", nil},
		{"outcome.go", nil},
		{w.Path, nil},
		{"created by testing", nil},
		{"+0x", nil},
		// Each failure's own message.
		{"answer() = 0, want 42", []string{"TestAssert"}},
		{"open testdata/fixture.json", []string{"TestLogFirst", "TestSetup"}},
		{"nil pointer dereference", []string{"TestNilPointer"}},
		{"index out of range [3] with length 2", []string{"TestIndex"}},
		{"assignment to entry in nil map", []string{"TestNilMap"}},
		{"test timed out", []string{"TestHang"}},
		// What the broker still matches, refused or documented elsewhere.
		{"widget cache missing", []string{"TestLogFirst"}},
		{"runtime error", []string{"TestIndex", "TestNilPointer"}},
		{"0x", []string{"TestNilPointer"}},
	} {
		var got []string
		for id, r := range runs {
			if matchesRed(t, r, id, c.fragment) {
				got = append(got, id)
			}
		}
		slices.Sort(got)
		if !slices.Equal(got, c.matches) {
			t.Errorf("fragment %q matched %v, want %v", c.fragment, got, c.matches)
		}
	}
	// The dump is still in the run's output, for the rework turn.
	if !strings.Contains(runs["TestNilPointer"].Output, "redoutcome.lookup(...)") {
		t.Fatal("the nil pointer run lost its trace")
	}
}

// goTestEvents is a go test -json stream of the given events, each
// {Action, Package, Test, Output}.
func goTestEvents(events ...[4]string) string {
	var b strings.Builder
	for _, e := range events {
		raw, _ := json.Marshal(map[string]string{"Action": e[0], "Package": e[1], "Test": e[2], "Output": e[3]})
		b.Write(append(raw, '\n'))
	}
	return b.String()
}

// A test without a result of its own counts as executed and failed only when
// go test's own -timeout alarm named it in its package that failed. Cut off
// any other way (its package failed for another reason, the broker killed
// the run at its deadline or cut its output at the bound, possibly
// mid-event) it is not counted, and the run is still evidence. A test
// without a result in a package that passed, or with no package result in a
// stream the broker did not cut, means the stream can't be read: refused as
// incomplete.
func TestParseTestsTestsWithoutOwnResult(t *testing.T) {
	run := [4]string{"run", "p", "TestX", ""}
	panicLine := [4]string{"output", "p", "TestX", "panic: test timed out after 1s\n"}
	list := [4]string{"output", "p", "TestX", "\trunning tests:\n"}
	named := [4]string{"output", "p", "TestX", "\t\tTestX (1s)\n"}
	other := [4]string{"output", "p", "TestX", "\t\tTestY (1s)\n"}
	printed := [4]string{"output", "p", "TestX", "fixture server crashed: test timed out\n"}
	failed := [4]string{"fail", "p", "", ""}
	for name, c := range map[string]struct {
		output              string
		timedOut, truncated bool
		err                 bool
		executed, matched   int
		passed, expected    bool
	}{
		"alarm named it":          {output: goTestEvents(run, panicLine, list, named, failed), executed: 1, matched: 1, expected: true},
		"alarm named another":     {output: goTestEvents(run, panicLine, list, other, failed)},
		"alarm without list":      {output: goTestEvents(run, panicLine, failed)},
		"binary ended":            {output: goTestEvents(run, printed, failed)},
		"package passed":          {output: goTestEvents(run, printed, [4]string{"pass", "p", "", ""}), err: true},
		"no package result":       {output: goTestEvents(run, printed), err: true},
		"other package failed":    {output: goTestEvents(run, printed, [4]string{"fail", "q", "", ""}), err: true},
		"killed at deadline":      {output: goTestEvents(run, printed), timedOut: true},
		"killed mid-write":        {output: goTestEvents(run, printed) + `{"Action":"outp`, timedOut: true},
		"alarm, then the kill":    {output: goTestEvents(run, panicLine, list, named, failed), timedOut: true, executed: 1, matched: 1},
		"cut at the bound":        {output: goTestEvents(run, printed) + `{"Action":"outp`, truncated: true},
		"partial without kill":    {output: goTestEvents(run, printed) + `{"Action":"outp`, err: true},
		"own result kept":         {output: goTestEvents(run, printed, [4]string{"pass", "p", "TestX", ""}, failed), executed: 1, matched: 1, passed: true},
		"own result before cut":   {output: goTestEvents(run, printed, [4]string{"fail", "p", "TestX", ""}) + `{"Action":"outp`, truncated: true, executed: 1, matched: 1, expected: true},
		"subtest named like it":   {output: goTestEvents(run, [4]string{"run", "p", "TestY/TestX", ""}, [4]string{"fail", "p", "TestY/TestX", ""}, [4]string{"fail", "p", "TestX", ""}, failed), executed: 2, matched: 1},
		"subtest cut off with it": {output: goTestEvents(run, [4]string{"run", "p", "TestX/sub", ""}, printed, failed)},
	} {
		r := RunEvidence{ExitCode: 1, Output: c.output, TimedOut: c.timedOut, Truncated: c.truncated}
		err := parseTests(&r, Command{TestID: "TestX", ExpectedFailure: "test timed out"})
		if (err != nil) != c.err {
			t.Errorf("%s: error %v", name, err)
			continue
		}
		if err == nil && (r.ExecutedCount != c.executed || r.MatchedCount != c.matched || r.RedPassed != c.passed || r.ExpectedFailure != c.expected) {
			t.Errorf("%s: %+v", name, r)
		}
	}
}

// The go test alarm goes before the -run anchor of a shaped go test vector
// that sets no -timeout of its own, at half the run's deadline.
func TestBrokerGoTestAlarmPlacement(t *testing.T) {
	shaped := []string{"go", "test", "-json", "-count=1", "-a", "-x", "-run=^TestX$", "./..."}
	for _, c := range []struct {
		argv     []string
		shape    string
		deadline time.Duration
		want     []string
	}{
		{shaped, "go-unit", 10 * time.Minute, []string{"go", "test", "-json", "-count=1", "-a", "-x", "-timeout=5m0s", "-run=^TestX$", "./..."}},
		{[]string{"go", "test", "-json", "-count=1", "-run=^TestX$", "./pkg"}, "go-integration", 9 * time.Second, []string{"go", "test", "-json", "-count=1", "-timeout=4.5s", "-run=^TestX$", "./pkg"}},
		{[]string{"go", "test", "-json", "-count=1", "-timeout=0", "-run=^TestX$", "./..."}, "go-unit", time.Minute, nil},
		{[]string{"go", "test", "-json", "-count=1", "-test.timeout=1m", "-run=^TestX$", "./..."}, "go-unit", time.Minute, nil},
		{[]string{"go", "test", "-json", "-count=1", "./..."}, "go-unit", time.Minute, nil},
		{shaped, "", time.Minute, nil},
		{[]string{"./run-tests", "TestX"}, "script", time.Minute, nil},
	} {
		want := c.want
		if want == nil {
			want = c.argv
		}
		if got := brokerGoTestAlarm(c.argv, c.shape, c.deadline); !slices.Equal(got, want) {
			t.Errorf("%v %s: got %v want %v", c.argv, c.shape, got, want)
		}
	}
}
