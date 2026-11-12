package effects

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"github.com/xleelz/fae/commandshim"
	"io"
	"path/filepath"
	"regexp"
	"strings"
)

var pythonIdentifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// SafePytestNodeID accepts one literal, file-qualified test, including a single
// exact parametrized case. A file or class selector alone cannot prove one test.
func SafePytestNodeID(node string) bool {
	if len(node) == 0 || len(node) > 2048 || strings.ContainsAny(node, "\x00\r\n") {
		return false
	}
	parts := strings.Split(node, "::")
	if len(parts) < 2 {
		return false
	}
	file := parts[0]
	if !filepath.IsLocal(file) || filepath.ToSlash(filepath.Clean(file)) != file || !strings.HasSuffix(file, ".py") || strings.ContainsAny(file, "*?[]\\") {
		return false
	}
	for i, name := range parts[1:] {
		if i == len(parts)-2 {
			base, param, has := strings.Cut(name, "[")
			if !pythonIdentifier.MatchString(base) {
				return false
			}
			if has && (!strings.HasSuffix(param, "]") || strings.Contains(param, "::")) {
				return false
			}
		} else if !pythonIdentifier.MatchString(name) {
			return false
		}
	}
	return true
}
func pytestPrefix(argv []string) (int, error) {
	kind, ok := commandshim.PythonCommandKind(argv)
	if !ok || kind != "test" {
		return 0, fmt.Errorf("python-pytest shape requires pytest or python -m pytest")
	}
	if filepath.Base(argv[0]) == "pytest" {
		return 1, nil
	}
	i := 1
	if argv[i] == "-B" {
		i++
	}
	return i + 2, nil
}

// PythonTestVector replaces broad file/directory selectors with the exact pin.
// reportPath must be empty here; private report creation belongs after policy.
func PythonTestVector(argv []string, nodeID, reportPath string) ([]string, error) {
	if !SafePytestNodeID(nodeID) {
		return nil, fmt.Errorf("python-pytest requires a literal file::test node id (including an exact parameter when parametrized)")
	}
	start, err := pytestPrefix(argv)
	if err != nil {
		return nil, err
	}
	out := append([]string(nil), argv[:start]...)
	for i := start; i < len(argv); i++ {
		arg := argv[i]
		if strings.HasPrefix(arg, "-") {
			switch {
			case arg == "-o":
				i++
				if i >= len(argv) || argv[i] != "addopts=" {
					return nil, fmt.Errorf("pytest configuration override is broker-owned")
				}
				out = append(out, "-o", "addopts=")
			case arg == "-q" || arg == "-v" || arg == "-vv" || arg == "-x" || arg == "--strict-markers" || arg == "--strict-config" || arg == "--disable-warnings" || arg == "--no-header" || arg == "--no-summary" || arg == "--showlocals" || strings.HasPrefix(arg, "--tb=") || strings.HasPrefix(arg, "--maxfail="):
				out = append(out, arg)
			default:
				return nil, fmt.Errorf("pytest shape refuses selection, plugin, parallel or output override %s", arg)
			}
		}
	}
	out = append(out, nodeID)
	if reportPath != "" {
		return AddPythonJUnitReport(out, reportPath)
	}
	return out, nil
}

// AddPythonJUnitReport is for the trusted broker, after validating user argv.
// rootdir fixes the relation between the exact selector and JUnit file paths.
func AddPythonJUnitReport(argv []string, reportPath string) ([]string, error) {
	if _, err := pytestPrefix(argv); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(reportPath) || filepath.Clean(reportPath) != reportPath {
		return nil, fmt.Errorf("pytest report must be a canonical private absolute path")
	}
	out := append([]string(nil), argv...)
	return append(out, "--rootdir=.", "-p", "no:cacheprovider", "--junitxml="+reportPath, "-o", "junit_family=xunit1", "-o", "junit_logging=no", "-o", "junit_log_passing_tests=false"), nil
}

type pytestProblem struct {
	Message string `xml:"message,attr"`
	Text    string `xml:",chardata"`
}
type pytestCase struct {
	Name     string          `xml:"name,attr"`
	Class    string          `xml:"classname,attr"`
	File     string          `xml:"file,attr"`
	Failures []pytestProblem `xml:"failure"`
	Errors   []pytestProblem `xml:"error"`
	Skipped  []pytestProblem `xml:"skipped"`
}
type pytestSuite struct {
	Tests  string        `xml:"tests,attr"`
	Errors string        `xml:"errors,attr"`
	Cases  []pytestCase  `xml:"testcase"`
	Suites []pytestSuite `xml:"testsuite"`
}

// ParsePytestJUnit consumes the private, completed XML report. Captured stdout,
// setup/collection errors, skipped/xfail cases and process failures never prove
// an expected assertion failure. Only the exact pinned case's failure text does.
func ParsePytestJUnit(r *RunEvidence, c Command, report []byte) error {
	r.ExecutedCount, r.MatchedCount, r.ExpectedFailure, r.RedPassed = 0, 0, false, false
	if len(report) == 0 {
		if r.TimedOut || r.ExitCode >= 2 {
			return nil
		}
		return fmt.Errorf("pytest did not produce a JUnit report")
	}
	if len(report) > DefaultMaxOutput {
		return fmt.Errorf("pytest JUnit report exceeds evidence bound")
	}
	if c.TestID != "" && !SafePytestNodeID(c.TestID) {
		return fmt.Errorf("invalid pinned pytest node id")
	}
	decoder := xml.NewDecoder(bytes.NewReader(report))
	token, err := decoder.Token()
	for err == nil {
		if _, ok := token.(xml.StartElement); ok {
			break
		}
		token, err = decoder.Token()
	}
	if err != nil {
		return fmt.Errorf("invalid pytest JUnit: %w", err)
	}
	start := token.(xml.StartElement)
	var root pytestSuite
	if start.Name.Local != "testsuite" && start.Name.Local != "testsuites" {
		return fmt.Errorf("pytest JUnit root must be testsuite or testsuites")
	}
	if err = decoder.DecodeElement(&root, &start); err != nil {
		return fmt.Errorf("invalid pytest JUnit: %w", err)
	}
	for {
		token, err = decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("invalid pytest JUnit: %w", err)
		}
		if data, ok := token.(xml.CharData); ok && len(bytes.TrimSpace(data)) == 0 {
			continue
		}
		return fmt.Errorf("trailing pytest JUnit content")
	}
	cases := []pytestCase{}
	hasErrors := false
	hasFailures := false
	var walk func(pytestSuite)
	walk = func(s pytestSuite) {
		hasErrors = hasErrors || (s.Errors != "" && s.Errors != "0")
		cases = append(cases, s.Cases...)
		for _, child := range s.Suites {
			walk(child)
		}
	}
	walk(root)
	expected := strings.Split(c.TestID, "::")
	expectedFile := expected[0]
	expectedName := expected[len(expected)-1]
	expectedClass := strings.ReplaceAll(strings.TrimSuffix(expectedFile, ".py"), "/", ".")
	if len(expected) > 2 {
		expectedClass += "." + strings.Join(expected[1:len(expected)-1], ".")
	}
	failureText := ""
	matchedFailure := false
	for _, test := range cases {
		hasFailures = hasFailures || len(test.Failures) > 0
		if len(test.Errors) > 0 {
			hasErrors = true
			continue
		}
		if len(test.Skipped) > 0 {
			continue
		}
		r.ExecutedCount++
		if c.TestID == "" || filepath.ToSlash(test.File) != expectedFile || test.Name != expectedName || test.Class != expectedClass {
			continue
		}
		r.MatchedCount++
		r.RedPassed = len(test.Failures) == 0
		if len(test.Failures) > 0 {
			matchedFailure = true
			for _, failure := range test.Failures {
				if strings.HasPrefix(strings.TrimSpace(failure.Message), "[XPASS") || strings.HasPrefix(strings.TrimSpace(failure.Text), "[XPASS") {
					matchedFailure = false
				}
				failureText += failure.Message + "\n" + failure.Text + "\n"
			}
		}
	}
	if r.ExitCode == 0 && (hasErrors || hasFailures) {
		return fmt.Errorf("pytest JUnit failures conflict with successful process exit")
	}
	r.FreshBuild = !r.TimedOut // this adapter never serves cached test results
	r.ExpectedFailure = !r.TimedOut && !r.Truncated && r.ExitCode == 1 && !hasErrors && len(cases) == 1 && r.ExecutedCount == 1 && r.MatchedCount == 1 && matchedFailure && c.ExpectedFailure != "" && strings.Contains(failureText, c.ExpectedFailure)
	return nil
}
