package main

import (
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// A lifecycle row's exit code that the conformance table does not run is
// either "not applicable: " with a reason, or proven by tests in this package
// that run fae ROW to that code. Such a test declares proves(t, ROW, CODE).
// proofProblems statically requires that declaration in every test a code
// proof cites; proves then fails the test unless one of its runs of ROW (help
// aside) exited CODE. A test that skips after declaring proves shows nothing:
// without FAE_TEST_DOLT_BINARY a Dolt-gated proof skips by design, but with
// it set a skip fails the test, so a run with Dolt cannot pass on a proof
// that never ran. A test in another package cannot run this CLI, so it can
// only support a not-applicable reason.

// cliRun is one execute call: its arguments and exit code.
type cliRun struct {
	args []string
	code int
}

var (
	cliRunsMu sync.Mutex
	cliRuns   = map[string][]cliRun{} // by top-level test name
)

func init() {
	executed = func(args []string, code int) {
		if test := runningTest(); test != "" {
			cliRunsMu.Lock()
			defer cliRunsMu.Unlock()
			cliRuns[test] = append(cliRuns[test], cliRun{slices.Clone(args), code})
		}
	}
}

// runningTest names the top-level test on the calling goroutine's stack (a
// subtest runs in "TestX.func1"), or "" off any test goroutine.
func runningTest() string {
	pcs := make([]uintptr, 256)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs)])
	for {
		frame, more := frames.Next()
		_, name, _ := strings.Cut(frame.Function[strings.LastIndex(frame.Function, "/")+1:], ".")
		if test, _, _ := strings.Cut(name, "."); strings.HasPrefix(test, "Test") && test != "TestMain" {
			return test
		}
		if !more {
			return ""
		}
	}
}

// proves declares that this test runs fae row to exit code. A conformance
// proof may then cite it for that code, and the test fails at its end unless
// one of its runs of row (help aside) exited code. The code must be a literal
// for proofProblems to read it. Declare it before any skip, so that a skip
// with FAE_TEST_DOLT_BINARY set fails the proof instead of passing silently.
func proves(t *testing.T, row string, code int) {
	t.Helper()
	test, _, _ := strings.Cut(t.Name(), "/")
	t.Cleanup(func() {
		if t.Skipped() {
			if os.Getenv("FAE_TEST_DOLT_BINARY") != "" {
				t.Errorf("%s skipped with FAE_TEST_DOLT_BINARY set, so it never showed fae %s exiting %d", test, row, code)
			}
			return
		}
		if problem := unproven(test, row, code); problem != "" {
			t.Error(problem)
		}
	})
}

// unproven says why test has not shown fae row exiting code, or "".
func unproven(test, row string, code int) string {
	cliRunsMu.Lock()
	defer cliRunsMu.Unlock()
	codes := []int{}
	for _, run := range cliRuns[test] {
		if runsRow(run.args, row) {
			if run.code == code {
				return ""
			}
			codes = append(codes, run.code)
		}
	}
	return fmt.Sprintf("%s proves fae %s exits %d, but its runs of it exited %v", test, row, code, codes)
}

// runsRow reports whether execute's args run row: past the common flags the
// leading words are exactly the row's verb path, each flag the row names is
// given before any "--", and no help is asked for.
func runsRow(args []string, row string) bool {
	for len(args) > 0 && strings.HasPrefix(args[0], "--") && !isHelp(args[0]) {
		if args[0] == "--json" {
			args = args[1:]
		} else if len(args) > 1 {
			args = args[2:]
		} else {
			return false
		}
	}
	if i := slices.Index(args, "--"); i >= 0 {
		args = args[:i]
	}
	words := strings.Fields(row)
	n := slices.IndexFunc(words, func(w string) bool { return strings.HasPrefix(w, "-") })
	if n < 0 {
		n = len(words)
	}
	if slices.ContainsFunc(args, isHelp) || len(args) < n || !slices.Equal(args[:n], words[:n]) || len(args) > n && !strings.HasPrefix(args[n], "-") {
		return false
	}
	for _, flag := range words[n:] {
		if !slices.ContainsFunc(args, func(a string) bool { return a == flag || strings.HasPrefix(a, flag+"=") }) {
			return false
		}
	}
	return true
}

// citedTest matches a cited test: TestName in this package, or pkg.TestName.
var citedTest = regexp.MustCompile(`\b(?:([a-z]+)\.)?(Test[A-Z0-9]\w*)`)

// proofProblems checks the proof of a lifecycle row's unchecked exit code,
// which is "not applicable: " with a reason, or citations of tests. Every
// cited test must exist. A citation proof may cite only tests in this
// package, each declaring proves(t, row, code). A not-applicable reason may
// cite tests of any package, but one in this package must declare proves for
// the row (with any code), so it did run the row. Whether the reason itself
// is true is left to review.
func proofProblems(row string, code int, proof string) []string {
	if proof == "" {
		return []string{"neither checked nor proven elsewhere"}
	}
	cites := citedTest.FindAllStringSubmatch(proof, -1)
	reason, na := strings.CutPrefix(proof, notApplicable)
	if na && strings.TrimSpace(reason) == "" || !na && len(cites) == 0 {
		return []string{fmt.Sprintf("proof %q neither cites a test nor gives a reason it is not applicable", proof)}
	}
	var problems []string
	for _, cite := range cites {
		fn, ok := testSources(cite[1])[cite[2]]
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf("cited test %s does not exist", cite[0]))
		case cite[1] != "" && !na:
			problems = append(problems, fmt.Sprintf("cited test %s is in another package and cannot run fae %s; cite a test here that proves(t, %q, %d)", cite[0], row, row, code))
		case cite[1] == "" && na && len(declaredCodes(fn, row)) == 0:
			problems = append(problems, fmt.Sprintf("cited test %s declares no proves(t, %q, CODE)", cite[0], row))
		case cite[1] == "" && !na && !slices.Contains(declaredCodes(fn, row), code):
			problems = append(problems, fmt.Sprintf("cited test %s does not declare proves(t, %q, %d)", cite[0], row, code))
		}
	}
	return problems
}

// declaredCodes are the literal codes fn declares with proves(t, row, CODE).
func declaredCodes(fn *ast.FuncDecl, row string) []int {
	var codes []int
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 3 {
			return true
		}
		name, _ := call.Fun.(*ast.Ident)
		verb, _ := call.Args[1].(*ast.BasicLit)
		code, _ := call.Args[2].(*ast.BasicLit)
		if name == nil || name.Name != "proves" || verb == nil || verb.Kind != token.STRING || code == nil || code.Kind != token.INT {
			return true
		}
		if v, err := strconv.Unquote(verb.Value); err == nil && v == row {
			if c, err := strconv.Atoi(code.Value); err == nil {
				codes = append(codes, c)
			}
		}
		return true
	})
	return codes
}

var (
	testSourceMu sync.Mutex
	testSource   = map[string]map[string]*ast.FuncDecl{}
)

// testSources maps each test function of a package (this one for "") to its declaration.
func testSources(pkg string) map[string]*ast.FuncDecl {
	testSourceMu.Lock()
	defer testSourceMu.Unlock()
	if tests, ok := testSource[pkg]; ok {
		return tests
	}
	dir := "."
	if pkg != "" {
		dir = filepath.Join("..", "..", pkg)
	}
	tests := map[string]*ast.FuncDecl{}
	files, _ := filepath.Glob(filepath.Join(dir, "*_test.go"))
	for _, path := range files {
		src, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, src, 0)
		if err != nil {
			continue
		}
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && strings.HasPrefix(fn.Name.Name, "Test") {
				tests[fn.Name.Name] = fn
			}
		}
	}
	testSource[pkg] = tests
	return tests
}

// The static half: a citation must bind to the row's verb and the code it
// claims. The heavy-op hook's exit-1 proof cited TestHeavyOpShimPolicyAndHookCLI,
// which runs the hook but never sees 1; the checker refuses that citation.
func TestConformanceProofsRefuseCheapCitations(t *testing.T) {
	for _, tc := range []struct {
		row   string
		code  int
		proof string
		ok    bool
	}{
		{"heavy-op hook", 1, "", false},
		{"heavy-op hook", 1, "TestNoSuchHookTest (a malformed hook request)", false},
		{"heavy-op hook", 1, "TestDoctorChecksConfiguration (another verb)", false},
		{"heavy-op hook", 1, "a prose claim without a test", false},
		{"heavy-op hook", 1, notApplicable, false},
		// The false wave-4 proof: the test runs the hook, but never to exit 1.
		{"heavy-op hook", 1, "TestHeavyOpShimPolicyAndHookCLI (a malformed hook request)", false},
		{"heavy-op hook", 2, "TestHeavyOpShimPolicyAndHookCLI (an unreadable request)", true},
		{"heavy-op hook", 1, notApplicable + "the hook needs no daemon", true},
		{"heavy-op hook", 1, notApplicable + "an unreadable request is 2 (TestHeavyOpShimPolicyAndHookCLI)", true},
		{"heavy-op hook", 1, notApplicable + "an unreadable request is 2 (TestHeavyOpRunCLIExitCodes)", false},
		// A test of a less specific row does not prove a more specific one.
		{"heavy-op run --shim-policy", 75, "TestHeavyOpRunCLIExitCodes", false},
		{"heavy-op run", 75, "TestHeavyOpRunCLIExitCodes", true},
		// The old maintenance proofs never ran fae maintenance.
		{"maintenance stop", 0, "controller.TestMaintenanceWaitsForDrainAndExactDeath (the client's stop)", false},
		{"maintenance start", 0, "controller.TestStartAcceptsOnlyLaunchedPinnedProcess", false},
		{"maintenance stop", 0, "TestMaintenanceStopAndStartCLIExitZero", true},
		{"maintenance start", 0, "TestMaintenanceStopAndStartCLIExitZero", true},
		{"maintenance start", 3, "TestMaintenanceStopAndStartCLIExitZero", false},
		{"roll", 3, notApplicable + "rolls need a replacement (controller.TestRollRejectedReplacementRollsBackToPreviousBinary)", true},
		{"roll", 3, notApplicable + "rolls need a replacement (controller.TestNoSuchRollTest)", false},
	} {
		if problems := proofProblems(tc.row, tc.code, tc.proof); (len(problems) == 0) != tc.ok {
			t.Errorf("%s exit %d %q: problems %v, want ok=%v", tc.row, tc.code, tc.proof, problems, tc.ok)
		}
	}
}

// The dynamic half: proves accepts only a run of the row's verb that exited
// the declared code, so declaring the false exit-1 proof would fail the test.
func TestConformanceProofObservesTheRunAndCode(t *testing.T) {
	hook := func(stdin string, args ...string) {
		restore := withStdin(t, stdin)
		defer restore()
		_ = execute(context.Background(), append([]string{"--json", "heavy-op", "hook", "--shim-dir", t.TempDir()}, args...), &bytes.Buffer{})
	}
	hook("not a hook request")
	hook("", "--help")
	test := t.Name()
	for _, tc := range []struct {
		row  string
		code int
		ok   bool
	}{
		{"heavy-op hook", 2, true},
		{"heavy-op hook", 1, false},
		{"heavy-op hook", 0, false}, // its help exited 0, but help proves nothing
		{"heavy-op", 2, false},
		{"heavy-op run", 2, false},
	} {
		if problem := unproven(test, tc.row, tc.code); (problem == "") != tc.ok {
			t.Errorf("%s exit %d: %q, want ok=%v", tc.row, tc.code, problem, tc.ok)
		}
	}
	for _, tc := range []struct {
		args []string
		row  string
		ok   bool
	}{
		{[]string{"--home", "h", "--instance", "i", "--json", "heavy-op", "run", "--shim-policy", "p", "--", "go"}, "heavy-op run --shim-policy", true},
		{[]string{"heavy-op", "run", "--shim-policy=p", "--", "go"}, "heavy-op run --shim-policy", true},
		{[]string{"heavy-op", "run", "--weight", "1", "--", "go", "--shim-policy"}, "heavy-op run --shim-policy", false},
		{[]string{"heavy-op", "run", "--weight", "1", "--", "go", "--help"}, "heavy-op run", true},
		{[]string{"land", "--task", "t", "--then-roll"}, "land --then-roll", true},
		{[]string{"maintenance", "stop", "-h"}, "maintenance stop", false},
		{[]string{"maintenance", "status"}, "maintenance stop", false},
		{[]string{"maintenance"}, "maintenance stop", false},
		{[]string{"cleanup", "scratch-archive", "--apply"}, "cleanup scratch", false},
	} {
		if runsRow(tc.args, tc.row) != tc.ok {
			t.Errorf("%v runs %q: want %v", tc.args, tc.row, tc.ok)
		}
	}
}

// TestProofSkipHelper declares a proof and skips; TestSkippedProofFailsWithDolt
// runs it in a child process. Run directly, it only skips.
func TestProofSkipHelper(t *testing.T) {
	if os.Getenv("CLI_TEST_PROOF_SKIP") != "1" {
		t.Skip("helper for TestSkippedProofFailsWithDolt")
	}
	proves(t, "version", 0)
	t.Skip("a cited proof that skips after declaring itself")
}

// A cited proof that skips is unchecked without Dolt, where Dolt-gated
// proofs skip by design, and fails its test with FAE_TEST_DOLT_BINARY set.
func TestSkippedProofFailsWithDolt(t *testing.T) {
	for dolt, fails := range map[string]bool{"": false, "/usr/local/bin/dolt": true} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestProofSkipHelper$", "-test.v")
		cmd.Env = append(os.Environ(), "CLI_TEST_PROOF_SKIP=1", "FAE_TEST_DOLT_BINARY="+dolt)
		out, err := cmd.CombinedOutput()
		if failed := err != nil; failed != fails || fails && !strings.Contains(string(out), "skipped with FAE_TEST_DOLT_BINARY set") || !fails && !strings.Contains(string(out), "--- SKIP") {
			t.Errorf("FAE_TEST_DOLT_BINARY=%q: failed %v (%v), want %v:\n%s", dolt, failed, err, fails, out)
		}
	}
}
