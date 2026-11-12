package effects

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func executable(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
}

// FAE-WF-036: every declared form runs exactly one named test; a run that
// matches zero or more than one test is visible as such to the RED gate.
func TestDeclaredTestShapeForms(t *testing.T) {
	b, w, _ := setup(t)
	write(t, w.Path, "y_test.go", "package test\nimport \"testing\"\nfunc TestXY(t *testing.T) {}\n")
	if err := os.MkdirAll(filepath.Join(w.Path, "integration"), 0700); err != nil {
		t.Fatal(err)
	}
	write(t, w.Path, "integration/it_test.go", "package integration\nimport \"testing\"\nfunc TestIT(t *testing.T) { t.Fatal(\"got 1 want 2\") }\nfunc TestX(t *testing.T) {}\n")
	tap := "#!/bin/sh\necho '1..3'\necho 'ok 1 - other'\necho \"not ok 2 - $1\"\necho '# got 1 want 2'\necho 'ok 3 - skipped # SKIP no fixture'\nexit 1\n"
	executable(t, w.Path, "red.sh", tap)
	executable(t, w.Path, "twice.sh", "#!/bin/sh\necho '1..2'\necho \"not ok 1 - $1\"\necho \"not ok 2 - $1\"\nexit 1\n")
	executable(t, w.Path, "runner.sh", tap)
	executable(t, w.Path, "fixed.sh", "#!/bin/sh\necho '1..1'\necho 'not ok 1 - TestFixed'\nexit 1\n")
	unit := []string{"go", "test", "-json", "-count=1", "-a", "-x", "./..."}
	integration := []string{"go", "test", "-json", "-count=1", "-a", "-x", "./integration"}
	b.Allowed = [][]string{unit, integration, {"./red.sh"}, {"./twice.sh"}, {"./fixed.sh"}, {"sh", "runner.sh"}}
	ctx := context.Background()
	for _, c := range []struct {
		name                       string
		cmd                        Command
		executed, matched          int
		expected, passed, rejected bool
	}{
		{name: "unit", cmd: Command{Argv: unit, TestID: "TestXY", Shape: "go-unit"}, executed: 1, matched: 1, passed: true},
		{name: "unit twice", cmd: Command{Argv: unit, TestID: "TestX", Shape: "go-unit"}, executed: 2, matched: 2, passed: true},
		{name: "unit none", cmd: Command{Argv: unit, TestID: "TestNone", Shape: "go-unit"}, executed: 0, matched: 0},
		{name: "integration", cmd: Command{Argv: integration, TestID: "TestIT", ExpectedFailure: "got 1 want 2", Shape: "go-integration"}, executed: 1, matched: 1, expected: true},
		{name: "integration pattern", cmd: Command{Argv: unit, TestID: "TestIT", Shape: "go-integration"}, rejected: true},
		{name: "script", cmd: Command{Argv: []string{"./red.sh"}, TestID: "TestRed", ExpectedFailure: "got 1 want 2", Shape: "script"}, executed: 2, matched: 1, expected: true},
		{name: "script twice", cmd: Command{Argv: []string{"./twice.sh"}, TestID: "TestRed", ExpectedFailure: "TestRed", Shape: "script"}, executed: 2, matched: 2},
		{name: "script none", cmd: Command{Argv: []string{"./fixed.sh"}, TestID: "TestRed", Shape: "script"}, executed: 1, matched: 0},
		{name: "runner", cmd: Command{Argv: []string{"sh", "runner.sh"}, TestID: "tests/a.py::test_b[1]", ExpectedFailure: "got 1 want 2", Shape: "runner"}, executed: 2, matched: 1, expected: true},
		{name: "runner pattern id", cmd: Command{Argv: []string{"sh", "runner.sh"}, TestID: "test_*", Shape: "runner"}, rejected: true},
		{name: "script as runner", cmd: Command{Argv: []string{"./red.sh"}, TestID: "TestRed", Shape: "runner"}, rejected: true},
		{name: "runner as script", cmd: Command{Argv: []string{"sh", "runner.sh"}, TestID: "TestRed", Shape: "script"}, rejected: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			c.cmd.Timeout = time.Minute
			r, err := b.Run(ctx, w, c.cmd)
			if c.rejected {
				if err == nil {
					t.Fatal("refused shape ran", r)
				}
				return
			}
			if err != nil || r.ExecutedCount != c.executed || r.MatchedCount != c.matched || r.ExpectedFailure != c.expected || r.RedPassed != c.passed || !r.FreshBuild {
				t.Fatalf("%+v %v", r, err)
			}
		})
	}
	got, err := shapeVector([]string{"./red.sh", "--tap"}, "script", "TestRed")
	if err != nil || !slices.Equal(got, []string{"./red.sh", "--tap", "TestRed"}) {
		t.Fatal(got, err)
	}
	for _, argv := range [][]string{{"go", "test", "./a", "./b"}, {"go", "test", "./..."}} {
		if _, err = shapeVector(argv, "go-integration", "TestIT"); err == nil {
			t.Fatal("ambiguous integration target accepted", argv)
		}
	}
}

func TestTAPParserRefusesIncompleteStreams(t *testing.T) {
	for name, out := range map[string]string{"no plan": "ok 1 - TestA\n", "short": "1..2\nok 1 - TestA\n", "bail": "1..1\nBail out! db\n", "two plans": "1..1\n1..1\nok 1 - TestA\n"} {
		r := RunEvidence{Output: out}
		if parseTAP(&r, Command{TestID: "TestA"}) == nil {
			t.Error(name, "accepted")
		}
	}
	r := RunEvidence{Output: "1..2\nok 1 - TestA\nnot ok 2 - TestB # TODO later\n"}
	if parseTAP(&r, Command{TestID: "TestA"}) != nil || r.ExecutedCount != 1 || r.MatchedCount != 1 || !r.RedPassed {
		t.Fatal(r)
	}
}
