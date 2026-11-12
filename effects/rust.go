package effects

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var rustTestID = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(::[A-Za-z_][A-Za-z0-9_]*)*$`)

func rustShapeVector(argv []string, test string) ([]string, error) {
	if len(argv) < 2 || filepath.Base(argv[0]) != "cargo" || argv[1] != "test" || !rustTestID.MatchString(test) {
		return nil, fmt.Errorf("rust-unit requires cargo test and one literal Rust test name")
	}
	out := append([]string(nil), argv...)
	targets := 0
	for i := 2; i < len(argv); i++ {
		switch argv[i] {
		case "--lib":
			targets++
		case "--test":
			if i+1 >= len(argv) {
				return nil, fmt.Errorf("missing Rust target")
			}
			targets++
			i++
		case "--manifest-path", "--package", "-p", "--features", "-F", "--jobs", "-j":
			i++
		case "--locked", "--offline", "--frozen", "--release", "--no-default-features", "--all-features":
		default:
			return nil, fmt.Errorf("rust-unit refuses existing filters or unsupported target options: %s", argv[i])
		}
	}
	if targets > 1 {
		return nil, fmt.Errorf("rust-unit requires exactly one test binary")
	}
	if targets == 0 {
		out = append(out, "--lib")
	}
	return append(out, test, "--", "--exact", "--test-threads=1", "--color=never"), nil
}

var rustResult = regexp.MustCompile(`^test ([A-Za-z_][A-Za-z0-9_:]*) \.\.\. (ok|FAILED|ignored(?:,.*)?)$`)
var rustSummary = regexp.MustCompile(`^test result: (?:ok|FAILED)\. ([0-9]+) passed; ([0-9]+) failed; ([0-9]+) ignored; ([0-9]+) measured; ([0-9]+) filtered out; finished in .*$`)

// parseRustTests accepts complete libtest harness records, never compile errors
// or ignored/no-match tests as selected execution evidence.
func parseRustTests(r *RunEvidence, c Command) error {
	if r.Truncated || r.TimedOut {
		return fmt.Errorf("incomplete Rust test evidence")
	}
	if c.TestID != "" && !rustTestID.MatchString(c.TestID) {
		return fmt.Errorf("invalid selected Rust test")
	}
	running, results, passed, failed, ignored := 0, 0, 0, 0, 0
	selectedFailed := false
	failureBody := false
	var selected strings.Builder
	for _, line := range strings.Split(r.Output, "\n") {
		if strings.HasPrefix(line, "running ") && strings.HasSuffix(line, " test") || strings.HasPrefix(line, "running ") && strings.HasSuffix(line, " tests") {
			var n int
			if _, e := fmt.Sscanf(line, "running %d", &n); e == nil {
				running += n
			}
		}
		if m := rustResult.FindStringSubmatch(line); m != nil {
			results++
			switch {
			case m[2] == "ok":
				passed++
				r.ExecutedCount++
			case m[2] == "FAILED":
				failed++
				r.ExecutedCount++
			default:
				ignored++
			}
			if m[1] == c.TestID && !strings.HasPrefix(m[2], "ignored") {
				r.MatchedCount++
				r.RedPassed = m[2] == "ok"
				selectedFailed = m[2] == "FAILED"
			}
		}
		if strings.HasPrefix(line, "---- ") && strings.HasSuffix(line, " stdout ----") {
			failureBody = line == "---- "+c.TestID+" stdout ----"
			continue
		}
		if line == "failures:" || strings.HasPrefix(line, "test result:") {
			failureBody = false
		}
		if failureBody {
			selected.WriteString(line)
			selected.WriteByte('\n')
		}
		if m := rustSummary.FindStringSubmatch(line); m != nil {
			p, _ := strconv.Atoi(m[1])
			f, _ := strconv.Atoi(m[2])
			ig, _ := strconv.Atoi(m[3])
			if p != passed || f != failed || ig != ignored || running != results {
				return fmt.Errorf("inconsistent Rust harness summary")
			}
			r.FreshBuild = true
			running, results, passed, failed, ignored = 0, 0, 0, 0, 0
		}
	}
	if !r.FreshBuild || running != 0 || results != 0 || (c.TestID != "" && r.MatchedCount != 1) {
		return fmt.Errorf("Rust evidence requires exactly one executed selected test and complete harness")
	}
	r.ExpectedFailure = c.TestID != "" && r.ExitCode != 0 && selectedFailed && c.ExpectedFailure != "" && strings.Contains(selected.String(), c.ExpectedFailure)
	return nil
}
