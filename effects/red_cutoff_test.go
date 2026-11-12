package effects

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/worktree"
)

// redCutoffSource is a module whose tests end in the ways re-audit 9 found
// a RED could be judged wrongly. In package tm a TestMain goroutine prints a
// fixture's failure and calls os.Exit while TestSleeps is still sleeping
// before its assertion. In package stack a test is killed by a signal from
// outside its test binary (as the OOM killer would), an httptest handler
// panics, which net/http logs with a goroutine stack before the test's own
// assertion fails, and a test prints its stack with debug.PrintStack before
// it fails. In package flood TestMain prints past the output bound after
// TestFailsThenFlood has reported its own result, and TestFloods prints past
// the bound before it would fail.
var redCutoffSource = map[string]string{
	"go.mod": "module example.com/redcutoff\n\ngo 1.23\n",
	"tm/tm_test.go": `package tm

import (
	"fmt"
	"os"
	"testing"
	"time"
)

var started = make(chan struct{})

func TestMain(m *testing.M) {
	go func() {
		<-started
		time.Sleep(200 * time.Millisecond)
		fmt.Println("fixture server crashed: bind: address already in use")
		os.Exit(1)
	}()
	os.Exit(m.Run())
}

func TestSleeps(t *testing.T) {
	close(started)
	time.Sleep(time.Minute)
	t.Fatal("answer = 0, want 42")
}
`,
	"stack/stack_test.go": `package stack

import (
	"net/http"
	"net/http/httptest"
	"os"
	"runtime/debug"
	"syscall"
	"testing"
	"time"
)

func TestKilled(t *testing.T) {
	t.Log("allocating the widget table")
	go func() { _ = syscall.Kill(os.Getpid(), syscall.SIGKILL) }()
	time.Sleep(time.Minute)
	t.Fatal("answer = 0, want 42")
}

func TestHandlerPanic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var hits map[string]int
		hits[r.URL.Path]++
	}))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/widget")
	if err == nil {
		resp.Body.Close()
	}
	t.Errorf("widget lookup failed: status unknown")
}

func TestPrintStack(t *testing.T) {
	debug.PrintStack()
	t.Fatal("widget missing from registry")
}
`,
	"flood/flood_test.go": `package flood

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

var ran bool

func TestMain(m *testing.M) {
	code := m.Run()
	if ran {
		for i := 0; i < 4000; i++ {
			fmt.Println("teardown " + strings.Repeat("x", 100))
		}
	}
	os.Exit(code)
}

func TestFailsThenFlood(t *testing.T) {
	ran = true
	t.Fatal("answer = 0, want 42")
}

func TestFloods(t *testing.T) {
	for i := 0; i < 4000; i++ {
		fmt.Println("progress " + strings.Repeat("y", 100))
	}
	t.Fatal("widget never arrived")
}
`,
}

// redModuleSetup commits source to a new repository and returns a broker
// allowed the RED stage's go test vector there, in a task worktree.
func redModuleSetup(t *testing.T, source map[string]string) (*Broker, worktree.Binding) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "repo")
	gitTest(t, root, "init", repo)
	for name, body := range source {
		if err = os.MkdirAll(filepath.Join(repo, filepath.Dir(name)), 0700); err != nil {
			t.Fatal(err)
		}
		write(t, repo, name, body)
	}
	gitTest(t, repo, "add", ".")
	gitTest(t, repo, "commit", "-m", "cases")
	m := &worktree.Manager{Repository: repo, Root: filepath.Join(root, "trees")}
	w, err := m.Ensure(context.Background(), "run", "task", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return &Broker{Manager: m, Environment: []string{"GOCACHE=" + os.Getenv("GOCACHE")}, Allowed: [][]string{{"go", "test", "-json", "-count=1", "./..."}}}, w
}

// FAE-WF-036/073 (re-audit 9): a test cut off while it runs by anything but
// go test's own -timeout reported no failure of its own, so it is not counted
// as executed or matched, and nothing printed while it ran is its expected
// failure: here a TestMain goroutine's os.Exit and a SIGKILL from outside the
// test binary. The process ended on its own and go test reported the
// package's fail, so the run is a failed run the workflow routes (the RED
// gate fails it into its rework loop, QA into remediation), never an error
// that parks the task UncertainDispatch. The rework turn's excerpt shows what
// the test printed and says it was not counted. Before, each test counted as
// executed and failed, and its printed text satisfied the RED gate.
func TestBrokerTestCutOffFromOutsideIsNotExecuted(t *testing.T) {
	b, w := redModuleSetup(t, redCutoffSource)
	for id, printed := range map[string][]string{
		"TestSleeps": {"fixture server crashed: bind: address already in use", "address already in use"},
		"TestKilled": {"signal: killed", "allocating the widget table"},
	} {
		r, _ := redOutcomeRun(t, b, w, b.Allowed[0], id, time.Minute)
		if r.TimedOut || r.Truncated || r.ExitCode != 1 || r.ExecutedCount != 0 || r.MatchedCount != 0 || r.RedPassed {
			t.Fatalf("%s run %+v", id, r)
		}
		for _, fragment := range append(printed, "answer = 0, want 42") {
			if matchesRed(t, r, id, fragment) {
				t.Errorf("%s: fragment %q proves a failure the test never reported", id, fragment)
			}
		}
		text, _ := FailureExcerpt(b.Allowed[0], r, nil, w.Path)
		for _, want := range []string{"=== RUN   " + id, printed[0], id + " reported no result of its own", "exit code 1, executed 0, matched 0\n"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s excerpt lacks %q:\n%s", id, want, text)
			}
		}
	}
}

// FAE-WF-036/073 (re-audit 9): a goroutine stack printed while the test runs
// is left out of the matched text, and the test's own later message is
// matched past it: net/http logs a handler's recovered panic with a stack
// (any httptest handler), and debug.PrintStack prints one. The panic's
// message in net/http's log line still matches; the stack's frames and paths
// never do. Before, matching stopped at the first stack header, so the
// test's own message never matched and a correct RED looped to the test-run
// cap.
func TestRedFailureTextReadsPastAStackTheTestContinues(t *testing.T) {
	b, w := redModuleSetup(t, redCutoffSource)
	for id, fragments := range map[string]map[string]bool{
		"TestHandlerPanic": {
			"widget lookup failed: status unknown": true,
			"http: panic serving":                  true,
			"assignment to entry in nil map":       true,
			"stack_test.go:30: widget lookup":      true,
			"net/http.(*conn).serve":               false,
			"example.com/redcutoff":                false,
			"created by net/http":                  false,
			"server.go":                            false,
		},
		"TestPrintStack": {
			"widget missing from registry": true,
			"runtime/debug.PrintStack":     false,
			"debug/stack.go":               false,
			"example.com/redcutoff":        false,
			"testing.tRunner":              false,
		},
	} {
		r, _ := redOutcomeRun(t, b, w, b.Allowed[0], id, time.Minute)
		if r.ExitCode != 1 || r.ExecutedCount != 1 || r.MatchedCount != 1 || r.RedPassed {
			t.Fatalf("%s run %+v", id, r)
		}
		if !strings.Contains(r.Output, `goroutine `) || !strings.Contains(r.Output, "+0x") {
			t.Fatalf("%s printed no stack:\n%s", id, r.Output)
		}
		for fragment, want := range fragments {
			if got := matchesRed(t, r, id, fragment); got != want {
				t.Errorf("%s fragment %q: expected failure %v, want %v", id, fragment, got, want)
			}
		}
	}
}

// FAE-ROB-016, FAE-WF-073 (HANDOFF next-work item 2): output past the run's
// byte bound is a completed run, so it is evidence, not an error that parks
// the task UncertainDispatch. Its exit code stands and its test stream counts
// only the results its kept head reports: a RED whose own result came before
// the cut proves its expected failure; one whose result was cut off is not
// counted, so the RED gate fails it into its rework loop. The excerpt says
// the output was cut. Any other command over the bound is evidence too.
// Before, every run over the bound was refused as an error.
func TestBrokerOutputOverBoundIsEvidence(t *testing.T) {
	b, w := redModuleSetup(t, redCutoffSource)
	b.MaxOutput = 256 << 10
	r, _ := redOutcomeRun(t, b, w, b.Allowed[0], "TestFailsThenFlood", time.Minute)
	if !r.Truncated || r.TimedOut || r.ExitCode != 1 || r.ExecutedCount != 1 || r.MatchedCount != 1 || r.RedPassed || r.OutputDigest == "" || r.Tree == "" {
		t.Fatalf("run whose test reported before the cut: %+v", r)
	}
	if !matchesRed(t, r, "TestFailsThenFlood", "answer = 0, want 42") || matchesRed(t, r, "TestFailsThenFlood", "teardown") {
		t.Error("the test's own reported failure is not its evidence, or the flood after it is")
	}
	r, _ = redOutcomeRun(t, b, w, b.Allowed[0], "TestFloods", time.Minute)
	if !r.Truncated || r.ExitCode != 1 || r.ExecutedCount != 0 || r.MatchedCount != 0 || r.RedPassed {
		t.Fatalf("run whose test's result was cut off: %+v", r)
	}
	for _, fragment := range []string{"progress", "widget never arrived"} {
		if matchesRed(t, r, "TestFloods", fragment) {
			t.Errorf("fragment %q proves a failure the kept output never reports", fragment)
		}
	}
	text, _ := FailureExcerpt(b.Allowed[0], r, nil, w.Path)
	if !strings.HasSuffix(text, "exit code 1, executed 0, matched 0, output cut at the run's byte bound\n") || !strings.Contains(text, "TestFloods reported no result of its own") {
		t.Errorf("excerpt:\n%s", text)
	}
	b.Allowed = append(b.Allowed, []string{"/bin/sh", "-c", "head -c 300000 /dev/zero | tr '\\000' x; exit 3"})
	sh, err := b.Run(context.Background(), w, Command{Argv: b.Allowed[1], Timeout: time.Minute})
	if err != nil || !sh.Truncated || sh.ExitCode != 3 || len(sh.Output) != 256<<10 || sh.OutputDigest == "" {
		t.Fatalf("command over the bound: %d bytes %+v %v", len(sh.Output), sh.Truncated, err)
	}
	if residue, _ := filepath.Glob(filepath.Join(filepath.Dir(w.Path), "command-*")); len(residue) != 0 {
		t.Fatal("host command residue", residue)
	}
}
