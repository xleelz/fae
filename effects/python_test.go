package effects

import (
	"fmt"
	"github.com/xleelz/fae/commandshim"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPythonTestVectorPinsLiteralNodeAndReport(t *testing.T) {
	node := "tests/test_x.py::TestMath::test_value[one + two]"
	v, err := PythonTestVector([]string{"python3", "-m", "pytest", "-q", "tests"}, node, "")
	if err != nil || strings.Join(v, "|") != "python3|-m|pytest|-q|"+node {
		t.Fatal(v, err)
	}
	v, err = AddPythonJUnitReport(v, "/var/tmp/fae/private.xml")
	if err != nil || !strings.Contains(strings.Join(v, "|"), "--junitxml=/var/tmp/fae/private.xml") {
		t.Fatal(v, err)
	}
	for _, id := range []string{"tests", "tests/test_x.py", "../test_x.py::test_x", "tests/*.py::test_x", "tests/test_x.py::test_*", "tests/test_x.py::TestMath::"} {
		if SafePytestNodeID(id) {
			t.Fatal("broad/unsafe node", id)
		}
	}
	if _, err = PythonTestVector([]string{"pytest", "-k", "other", "tests"}, node, ""); err == nil {
		t.Fatal("selection override accepted")
	}
}
func junitCase(body string) []byte {
	return []byte(`<testsuites><testsuite tests="1" errors="0"><testcase classname="tests.test_x.TestMath" file="tests/test_x.py" name="test_value[case]">` + body + `</testcase></testsuite></testsuites>`)
}
func TestPytestEvidenceRejectsSkipsCollectionErrorsAndFraming(t *testing.T) {
	c := Command{TestID: "tests/test_x.py::TestMath::test_value[case]", ExpectedFailure: "wanted 42"}
	for _, tt := range []struct {
		name, body       string
		exit             int
		executed         int
		matched          int
		expected, passed bool
	}{
		{"assertion", `<failure message="assert 0 == 42">wanted 42</failure>`, 1, 1, 1, true, false},
		{"pass", "", 0, 1, 1, false, true},
		{"skip", `<skipped message="skip"/>`, 0, 0, 0, false, false},
		{"xfail", `<skipped type="pytest.xfail" message="wanted 42"/>`, 0, 0, 0, false, false},
		{"setup", `<error message="wanted 42">fixture failed</error>`, 1, 0, 0, false, false},
		{"collection", `<error message="wanted 42">syntax error</error>`, 2, 0, 0, false, false},
		{"interrupt", `<failure>wanted 42</failure>`, 2, 1, 1, false, false},
		{"stdout", `<failure>different reason</failure><system-out>wanted 42</system-out>`, 1, 1, 1, false, false},
		{"strict-xpass", `<failure message="[XPASS(strict)] wanted 42">[XPASS(strict)] wanted 42</failure>`, 1, 1, 1, false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := RunEvidence{ExitCode: tt.exit, Output: "wanted 42 global log", Stderr: "wanted 42"}
			if err := ParsePytestJUnit(&r, c, junitCase(tt.body)); err != nil {
				t.Fatal(err)
			}
			if r.ExecutedCount != tt.executed || r.MatchedCount != tt.matched || r.ExpectedFailure != tt.expected || r.RedPassed != tt.passed {
				t.Fatal(r)
			}
		})
	}
	for _, cut := range []RunEvidence{{ExitCode: 1, TimedOut: true}, {ExitCode: 1, Truncated: true}} {
		r := cut
		if err := ParsePytestJUnit(&r, c, junitCase(`<failure>wanted 42</failure>`)); err != nil || r.ExpectedFailure {
			t.Fatal(r, err)
		}
	}
	if err := ParsePytestJUnit(&RunEvidence{ExitCode: 1}, c, []byte(`<testsuites>`)); err == nil {
		t.Fatal("truncated report accepted")
	}
	r := RunEvidence{ExitCode: 5}
	if err := ParsePytestJUnit(&r, c, []byte(`<testsuites><testsuite tests="0" errors="0"/></testsuites>`)); err != nil || r.ExecutedCount != 0 || r.ExpectedFailure {
		t.Fatal(r, err)
	}
	wrong := strings.Replace(string(junitCase(`<failure>wanted 42</failure>`)), `file="tests/test_x.py"`, `file="other/test_x.py"`, 1)
	r = RunEvidence{ExitCode: 1}
	if err := ParsePytestJUnit(&r, c, []byte(wrong)); err != nil || r.MatchedCount != 0 || r.ExpectedFailure {
		t.Fatal(r, err)
	}
}
func TestPytestJUnitNative(t *testing.T) {
	python := ""
	for _, candidate := range []string{"python3", "/usr/bin/python3"} {
		path, err := exec.LookPath(candidate)
		if err != nil {
			continue
		}
		if exec.Command(path, "-m", "pytest", "--version").Run() == nil {
			python = path
			break
		}
	}
	if python == "" {
		t.Skip("pytest is not installed in either available Python; no dependency installation performed")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "test_sample.py"), []byte("def test_value():\n    assert 0 == 42, 'wanted 42'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	argv, err := PythonTestVector([]string{python, "-m", "pytest"}, "test_sample.py::test_value", "")
	if err != nil {
		t.Fatal(err)
	}
	scratch := t.TempDir()
	report := filepath.Join(scratch, "pytest.xml")
	argv, err = AddPythonJUnitReport(argv, report)
	if err != nil {
		t.Fatal(err)
	}
	env, err := commandshim.PythonEnvironment(argv, os.Environ(), scratch)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = env
	out, runErr := cmd.CombinedOutput()
	exit := 0
	if runErr != nil {
		if e, ok := runErr.(*exec.ExitError); ok {
			exit = e.ExitCode()
		} else {
			t.Fatal(runErr)
		}
	}
	raw, err := os.ReadFile(report)
	if err != nil {
		t.Fatal(string(out), err)
	}
	r := RunEvidence{ExitCode: exit, Output: string(out)}
	if err = ParsePytestJUnit(&r, Command{TestID: "test_sample.py::test_value", ExpectedFailure: "wanted 42"}, raw); err != nil || !r.ExpectedFailure || r.MatchedCount != 1 {
		t.Fatal(fmt.Sprintf("%s\n%s", out, raw), r, err)
	}
}

func TestPytestWholeSuiteCountsOnlyCompletedTests(t *testing.T) {
	raw := []byte(`<testsuites><testsuite tests="3" errors="0"><testcase classname="a" file="a.py" name="test_pass"/><testcase classname="a" file="a.py" name="test_fail"><failure>wanted 42</failure></testcase><testcase classname="a" file="a.py" name="test_skip"><skipped/></testcase></testsuite></testsuites>`)
	r := RunEvidence{ExitCode: 1}
	if err := ParsePytestJUnit(&r, Command{ExpectedFailure: "wanted 42"}, raw); err != nil || r.ExecutedCount != 2 || r.MatchedCount != 0 || r.ExpectedFailure || r.RedPassed {
		t.Fatal(r, err)
	}
}
