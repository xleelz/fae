package effects

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// TestShapes are the declared test-shape forms (FAE-WF-036): one named Go
// test across the declared targets; one named Go test in exactly one named
// integration target package; a dedicated test script inside the worktree;
// and a declared runner. Script and runner forms receive the pinned test
// identifier as their final argument and report TAP on standard output.
var TestShapes = []string{"go-unit", "go-integration", "script", "runner", "rust-unit", "python-pytest", "node-test"}

var safeTestID = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.:/\[\]=,@+-]*$`)

// ShapeTestCommand narrows an operator's configured test command for workflow
// and landing verification through the same runner-specific implementation.
func ShapeTestCommand(argv []string, shape, test string) ([]string, error) {
	return shapeVector(argv, shape, test)
}

// SafeTestID accepts a test identifier that is one literal argument and
// never a pattern: no whitespace, wildcards or regular-expression operators.
func SafeTestID(id string) bool { return len(id) <= 512 && safeTestID.MatchString(id) }

// shapeVector narrows an allowlisted vector to exactly the pinned test.
func shapeVector(argv []string, shape, test string) ([]string, error) {
	switch shape {
	case "rust-unit":
		return rustShapeVector(argv, test)
	case "python-pytest":
		return PythonTestVector(argv, test, "")
	case "node-test":
		return ShapeNodeTest(argv, test)
	case "go-unit":
		return shapeArgv(argv, shape, test)
	case "go-integration":
		targets := 0
		for i := 2; i < len(argv); i++ {
			if argv[i] == "-p" || argv[i] == "-parallel" || argv[i] == "-test.parallel" || argv[i] == "-o" {
				i++
				continue
			}
			if !strings.HasPrefix(argv[i], "-") {
				if strings.HasSuffix(argv[i], "...") {
					return nil, fmt.Errorf("integration-target shape requires one named target package, not a pattern")
				}
				targets++
			}
		}
		if targets != 1 {
			return nil, fmt.Errorf("integration-target shape requires exactly one named target package")
		}
		return shapeArgv(argv, "go-unit", test)
	case "script", "runner":
		if len(argv) == 0 || !SafeTestID(test) {
			return nil, fmt.Errorf("unsupported declared test shape %q for %q", shape, test)
		}
		local := strings.HasPrefix(argv[0], "./") && filepath.Clean(argv[0][2:]) == argv[0][2:] && filepath.IsLocal(argv[0][2:])
		if (shape == "script") != local || shape == "runner" && (filepath.Base(argv[0]) == "go" || filepath.IsAbs(argv[0]) || strings.Contains(argv[0], "/")) {
			return nil, fmt.Errorf("%s shape requires %s", shape, map[string]string{"script": "a dedicated ./ script inside the worktree", "runner": "a declared runner program, not go or a path"}[shape])
		}
		return append(append([]string(nil), argv...), test), nil
	}
	return nil, fmt.Errorf("unsupported declared test shape %q for %q", shape, test)
}

var tapResult = regexp.MustCompile(`^(not )?ok\b(?:\s+\d+)?(?:\s*-)?\s*(.*)$`)
var tapPlan = regexp.MustCompile(`^1\.\.(\d+)\b`)

// parseTAP reads a TAP stream: a plan and exactly that many results. A
// result whose description is the pinned identifier is the declared test;
// SKIP and TODO results do not count as executed. The expected failure is
// matched against the command output only, never the test source.
func parseTAP(r *RunEvidence, c Command) error {
	scan := bufio.NewScanner(strings.NewReader(r.Output))
	scan.Buffer(make([]byte, 4096), 4<<20)
	planned, results := -1, 0
	r.ExecutedCount, r.MatchedCount, r.RedPassed = 0, 0, false
	for scan.Scan() {
		line := strings.TrimRight(scan.Text(), "\r")
		if strings.HasPrefix(line, "Bail out!") {
			return fmt.Errorf("TAP stream bailed out")
		}
		if m := tapPlan.FindStringSubmatch(line); m != nil {
			if planned >= 0 {
				return fmt.Errorf("duplicate TAP plan")
			}
			planned, _ = strconv.Atoi(m[1])
			continue
		}
		m := tapResult.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		results++
		description, directive, _ := strings.Cut(m[2], "#")
		directive = strings.ToUpper(strings.TrimSpace(directive))
		if strings.HasPrefix(directive, "SKIP") || strings.HasPrefix(directive, "TODO") {
			continue
		}
		r.ExecutedCount++
		if strings.TrimSpace(description) == c.TestID && c.TestID != "" {
			r.MatchedCount++
			r.RedPassed = m[1] == ""
		}
	}
	if err := scan.Err(); err != nil {
		return err
	}
	if planned < 0 || planned != results {
		return fmt.Errorf("incomplete TAP stream")
	}
	r.FreshBuild = true // the broker never serves cached results for a declared runner
	r.ExpectedFailure = c.ExpectedFailure != "" && r.ExitCode != 0 && r.MatchedCount == 1 && !r.RedPassed && strings.Contains(r.Output+r.Stderr, c.ExpectedFailure)
	return nil
}

// lookCommand resolves a ./ program inside the worktree (a dedicated test
// script) as a physical executable file; any other name is found on PATH.
func lookCommand(worktree, name string) (string, error) {
	rest, local := strings.CutPrefix(name, "./")
	if !local {
		return exec.LookPath(name)
	}
	root, err := filepath.EvalSymlinks(worktree)
	if err != nil {
		return "", err
	}
	path := filepath.Join(root, rest)
	physical, err := filepath.EvalSymlinks(path)
	if err != nil || physical != path || !filepath.IsLocal(rest) {
		return "", fmt.Errorf("worktree program must be a physical file inside the worktree")
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0100 == 0 {
		return "", fmt.Errorf("worktree program must be an executable regular file")
	}
	return path, nil
}
