package effects

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func pythonWithPytest(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"python3", "/usr/bin/python3"} {
		binary, err := exec.LookPath(name)
		if err == nil && exec.Command(binary, "-m", "pytest", "--version").Run() == nil {
			return binary
		}
	}
	t.Skip("pytest unavailable; native Python dependency not installed")
	return ""
}
func TestPythonBrokerExactNodeRedThenGreen(t *testing.T) {
	python := pythonWithPytest(t)
	b, w, _ := setup(t)
	argv := []string{python, "-m", "pytest", "-q", "."}
	b.Allowed = [][]string{argv}
	c := Command{Argv: argv, Shape: "python-pytest", TestID: "test_behavior.py::test_selected", ExpectedFailure: "wanted 42", Timeout: 20 * time.Second}
	write(t, w.Path, "pytest.ini", "[pytest]\naddopts = -n 8 --junitxml=UNTRUSTED.xml\n")
	c.Environment = []string{"PYTEST_ADDOPTS=-n 9", "PYTEST_PLUGINS=nonexistent_external_plugin"}
	write(t, w.Path, "test_behavior.py", "def test_selected():\n    assert 0 == 42, 'wanted 42'\n\ndef test_other():\n    assert False, 'UNSELECTED TEST RAN'\n")
	r, err := b.Run(context.Background(), w, c)
	if err != nil || r.ExitCode != 1 || r.ExecutedCount != 1 || r.MatchedCount != 1 || !r.ExpectedFailure || r.RedPassed || strings.Contains(r.Output, "UNSELECTED TEST RAN") {
		t.Fatalf("selected RED %+v %v", r, err)
	}
	write(t, w.Path, "test_behavior.py", "def test_selected():\n    assert 42 == 42\n\ndef test_other():\n    assert False, 'UNSELECTED TEST RAN'\n")
	r, err = b.Run(context.Background(), w, c)
	if err != nil || r.ExitCode != 0 || r.ExecutedCount != 1 || r.MatchedCount != 1 || !r.RedPassed || r.ExpectedFailure || strings.Contains(r.Output, "UNSELECTED TEST RAN") {
		t.Fatalf("selected GREEN %+v %v", r, err)
	}
	c.Shape = ""
	c.TestID = ""
	c.ExpectedFailure = ""
	r, err = b.Run(context.Background(), w, c)
	if err != nil || r.ExitCode != 1 || r.ExecutedCount != 2 || r.MatchedCount != 0 || r.ExpectedFailure {
		t.Fatalf("whole suite %+v %v", r, err)
	}
}
func TestPythonBrokerParameterSkipAndCollectionEvidence(t *testing.T) {
	python := pythonWithPytest(t)
	b, w, _ := setup(t)
	argv := []string{python, "-m", "pytest", "."}
	b.Allowed = [][]string{argv}
	write(t, w.Path, "test_cases.py", "import pytest\n@pytest.mark.parametrize('value', [1, 2], ids=['one', 'two'])\ndef test_parameter(value):\n    assert value == 2, 'wrong parameter'\n\n@pytest.mark.skip(reason='not executed')\ndef test_skipped():\n    assert False\n")
	c := Command{Argv: argv, Shape: "python-pytest", TestID: "test_cases.py::test_parameter[two]", Timeout: 20 * time.Second}
	r, err := b.Run(context.Background(), w, c)
	if err != nil || r.ExecutedCount != 1 || r.MatchedCount != 1 || !r.RedPassed {
		t.Fatal(r, err)
	}
	c.TestID = "test_cases.py::test_skipped"
	r, err = b.Run(context.Background(), w, c)
	if err != nil || r.ExecutedCount != 0 || r.MatchedCount != 0 || r.RedPassed || r.ExpectedFailure {
		t.Fatal(r, err)
	}
	write(t, w.Path, "test_broken.py", "def bad(:\n")
	c.TestID = "test_broken.py::test_missing"
	r, err = b.Run(context.Background(), w, c)
	if err != nil || r.ExitCode != 4 || r.ExecutedCount != 0 || r.ExpectedFailure {
		t.Fatal(r, err)
	}
}

func TestPythonBrokerNoTestsDoesNotProveFailure(t *testing.T) {
	python := pythonWithPytest(t)
	b, w, _ := setup(t)
	write(t, w.Path, "empty.py", "value = 42\n")
	argv := []string{python, "-m", "pytest", "empty.py"}
	b.Allowed = [][]string{argv}
	r, err := b.Run(context.Background(), w, Command{Argv: argv, Timeout: 20 * time.Second, ExpectedFailure: "anything"})
	if err != nil || r.ExitCode != 5 || r.ExecutedCount != 0 || r.MatchedCount != 0 || r.ExpectedFailure {
		t.Fatal(r, err)
	}
}
