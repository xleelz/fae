package effects

import (
	"bufio"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

func ShapeNodeTest(argv []string, testID string) ([]string, error) {
	if len(argv) == 0 || !SafeTestID(testID) {
		return nil, fmt.Errorf("node-test requires one literal named test")
	}
	if filepath.Base(argv[0]) != "node" && filepath.Base(argv[0]) != "nodejs" {
		return nil, fmt.Errorf("node-test requires node --test")
	}
	result := []string{argv[0]}
	mode := false
	for i := 1; i < len(argv); i++ {
		key, value, assigned := strings.Cut(argv[i], "=")
		if key == "--test-name-pattern" {
			if !assigned {
				if i+1 >= len(argv) {
					return nil, fmt.Errorf("missing Node selector")
				}
				i++
			}
			continue
		}
		if key == "--test" {
			mode = true
		}
		_ = value
		result = append(result, argv[i])
	}
	if !mode {
		return nil, fmt.Errorf("node-test requires explicit --test")
	}
	return append(result, "--test-name-pattern=^"+regexp.QuoteMeta(testID)+"$"), nil
}

var nodeResult = regexp.MustCompile(`^( *)(not )?ok ([0-9]+) - (.*)$`)
var nodePlan = regexp.MustCompile(`^( *)1\.\.([0-9]+)(?:\s*#.*)?$`)
var nodeSummary = regexp.MustCompile(`^# (tests|suites|pass|fail|cancelled|skipped|todo) ([0-9]+)$`)

type nodeOutcome struct {
	indent           int
	name, full, kind string
	passed, skipped  bool
}

// ParseNodeTestEvidence accepts complete built-in Node TAP, never an arbitrary
// script's ad-hoc count. Exact shapes require one executed named leaf; suite
// runs with no TestID count leaves without inventing RED matching evidence.
func ParseNodeTestEvidence(r *RunEvidence, c Command) error {
	r.ExecutedCount, r.MatchedCount, r.RedPassed, r.ExpectedFailure = 0, 0, false, false
	if r.Truncated || r.TimedOut {
		return fmt.Errorf("Node test evidence is incomplete or timed out")
	}
	scan := bufio.NewScanner(strings.NewReader(r.Output))
	scan.Buffer(make([]byte, 4096), 4<<20)
	plans, counts := map[int]int{}, map[int]int{}
	summary := map[string]int{}
	titles := map[int]string{}
	outcomes := []nodeOutcome{}
	last := -1
	version := false
	for scan.Scan() {
		line := strings.TrimRight(scan.Text(), "\r")
		trimmed := strings.TrimSpace(line)
		if line == "TAP version 13" {
			if version {
				return fmt.Errorf("duplicate Node TAP header")
			}
			version = true
			continue
		}
		if strings.HasPrefix(trimmed, "Bail out!") {
			return fmt.Errorf("Node TAP bailed out")
		}
		if strings.HasPrefix(trimmed, "# Subtest: ") {
			indent := len(line) - len(strings.TrimLeft(line, " "))
			titles[indent] = strings.TrimPrefix(trimmed, "# Subtest: ")
			for depth := range titles {
				if depth > indent {
					delete(titles, depth)
				}
			}
			continue
		}
		if m := nodePlan.FindStringSubmatch(line); m != nil {
			depth := len(m[1])
			if _, exists := plans[depth]; exists {
				return fmt.Errorf("duplicate Node TAP plan")
			}
			plans[depth], _ = strconv.Atoi(m[2])
			continue
		}
		if m := nodeSummary.FindStringSubmatch(line); m != nil {
			if _, exists := summary[m[1]]; exists {
				return fmt.Errorf("duplicate Node TAP summary")
			}
			summary[m[1]], _ = strconv.Atoi(m[2])
			continue
		}
		if m := nodeResult.FindStringSubmatch(line); m != nil {
			depth := len(m[1])
			number, _ := strconv.Atoi(m[3])
			counts[depth]++
			if number != counts[depth] {
				return fmt.Errorf("nonsequential Node TAP results")
			}
			name, directive, _ := strings.Cut(m[4], " # ")
			directive = strings.ToUpper(directive)
			depths := []int{}
			for d := range titles {
				if d < depth {
					depths = append(depths, d)
				}
			}
			sort.Ints(depths)
			chain := []string{}
			for _, d := range depths {
				chain = append(chain, titles[d])
			}
			chain = append(chain, strings.TrimSpace(name))
			outcomes = append(outcomes, nodeOutcome{indent: depth, name: strings.TrimSpace(name), full: strings.Join(chain, " "), passed: m[2] == "", skipped: strings.HasPrefix(directive, "SKIP") || strings.HasPrefix(directive, "TODO")})
			last = len(outcomes) - 1
			continue
		}
		if last >= 0 {
			if trimmed == "type: 'test'" || trimmed == "type: test" {
				outcomes[last].kind = "test"
			}
			if trimmed == "type: 'suite'" || trimmed == "type: suite" {
				outcomes[last].kind = "suite"
			}
		}
	}
	if err := scan.Err(); err != nil {
		return err
	}
	if !version {
		return fmt.Errorf("missing Node TAP header")
	}
	if _, ok := plans[0]; !ok {
		return fmt.Errorf("missing Node TAP plan")
	}
	for depth, planned := range plans {
		if planned != counts[depth] {
			return fmt.Errorf("incomplete Node TAP plan")
		}
	}
	for _, key := range []string{"tests", "suites", "pass", "fail", "cancelled", "skipped", "todo"} {
		if _, ok := summary[key]; !ok {
			return fmt.Errorf("incomplete Node TAP summary")
		}
	}
	if summary["cancelled"] != 0 {
		return fmt.Errorf("Node tests were cancelled")
	}
	if summary["tests"] != summary["pass"]+summary["fail"]+summary["skipped"]+summary["todo"] {
		return fmt.Errorf("inconsistent Node TAP summary")
	}
	tests, suites, passed, failed, skipped := 0, 0, 0, 0, 0
	file := ""
	args := []string{}
	if len(c.Argv) > 1 {
		args = c.Argv[1:]
	}
	for _, arg := range args {
		if !strings.HasPrefix(arg, "-") {
			switch filepath.Ext(arg) {
			case ".js", ".cjs", ".mjs", ".ts", ".mts", ".cts":
				file = filepath.ToSlash(filepath.Clean(arg))
			}
		}
	}
	for _, outcome := range outcomes {
		if outcome.kind == "suite" {
			suites++
			continue
		}
		if outcome.kind == "" && summary["suites"] > 0 {
			isSuite := false
			for _, other := range outcomes {
				if other.indent > outcome.indent && strings.HasPrefix(other.full, outcome.full+" ") {
					isSuite = true
					break
				}
			}
			if isSuite {
				suites++
				continue
			}
		}
		if file != "" && (outcome.name == file || strings.HasSuffix(filepath.ToSlash(outcome.name), "/"+strings.TrimPrefix(file, "./"))) {
			return fmt.Errorf("Node reported a file wrapper instead of registered test cases")
		}
		tests++
		if outcome.skipped {
			skipped++
			continue
		}
		r.ExecutedCount++
		if outcome.passed {
			passed++
		} else {
			failed++
		}
		if c.TestID != "" && outcome.full == c.TestID {
			r.MatchedCount++
			r.RedPassed = outcome.passed
		}
	}
	if tests != summary["tests"] || suites != summary["suites"] || passed != summary["pass"] || failed != summary["fail"] || skipped != summary["skipped"]+summary["todo"] {
		return fmt.Errorf("Node TAP results disagree with summary")
	}
	if c.TestID != "" && (r.ExecutedCount != 1 || r.MatchedCount != 1) {
		return fmt.Errorf("Node test selector did not execute exactly the declared test")
	}
	if r.ExitCode == 0 && failed > 0 || r.ExitCode != 0 && failed == 0 {
		return fmt.Errorf("Node exit status disagrees with test outcomes")
	}
	if r.ExecutedCount == 0 {
		return fmt.Errorf("Node ran no executable test cases")
	}
	r.FreshBuild = true // real uncached runner execution, not proof of a tsc compile
	r.ExpectedFailure = c.TestID != "" && c.ExpectedFailure != "" && r.ExitCode != 0 && r.MatchedCount == 1 && !r.RedPassed && strings.Contains(r.Output+r.Stderr, c.ExpectedFailure)
	return nil
}
