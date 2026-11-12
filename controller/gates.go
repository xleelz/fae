package controller

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/xleelz/fae/hostguard"
	"github.com/xleelz/fae/workflow"
)

// GateResult is one built-in gate verdict. Count is the gate's positive
// evidence (items, tests, packages or checks examined); a gate that examined
// nothing refuses unless it is explicitly not applicable.
type GateResult struct {
	Gate       string   `json:"gate"`
	Passed     bool     `json:"passed"`
	Applicable bool     `json:"applicable"`
	Count      int      `json:"count"`
	Failures   []string `json:"failures,omitempty"`
	Reason     string   `json:"reason,omitempty"`
}

func verdict(gate string, applicable bool, count int, failures []string, empty string) GateResult {
	r := GateResult{Gate: gate, Applicable: applicable, Count: count, Failures: failures}
	r.Passed = GatePasses(applicable, count, len(failures))
	switch {
	case len(failures) > 0:
		r.Reason = strings.Join(failures, "; ")
	case !r.Passed:
		r.Reason = empty
	}
	return r
}

// MemoryGate is the landing memory floor (FAE-HOST-007, default 12 GiB).
func MemoryGate(s hostguard.Sample, floor int64) GateResult {
	known := s.Memory.Known && !math.IsNaN(s.Memory.Value) && !math.IsInf(s.Memory.Value, 0) && s.Memory.Value >= 0
	available := int64(0)
	if known {
		available = int64(math.Min(s.Memory.Value, math.MaxInt64/2))
	}
	r := GateResult{Gate: "memory", Applicable: true, Count: 1}
	if r.Passed = MeetsLandingFloor(known, available, floor); !r.Passed {
		r.Reason = fmt.Sprintf("memory available %s below landing floor %d", formatBytes(s.Memory), floor)
		r.Failures = []string{r.Reason}
	}
	return r
}
func formatBytes(r hostguard.Reading) string {
	if !r.Known {
		return "unknown"
	}
	return fmt.Sprintf("%.0f", r.Value)
}

// IOGate records one reading in the FAE-HOST-010 two-reading I/O hold. The gate
// passes only when the hold proceeds; a single reading never lifts it.
func IOGate(h *hostguard.Hold, at time.Time, s hostguard.Sample, c hostguard.Config) (GateResult, hostguard.Step) {
	step := h.Record(at, s, c)
	r := GateResult{Gate: "io", Applicable: true, Count: 1, Passed: step.Outcome == hostguard.OutcomeProceed}
	if !r.Passed {
		r.Reason = string(step.Outcome) + ": " + step.Reason
		r.Failures = []string{r.Reason}
	}
	return r, step
}

// OverlapInput is one roll's file-overlap question for one in-flight task.
type OverlapInput struct {
	Changed, Touched                                           []string
	QueryFailed, Acknowledged, Expired, Critical, TopNearLimit bool
}

// OverlapGate applies the production roll guards: an overlap or failed query
// holds unless MayReleaseOverlap releases it (FAE-CTL-001 step 2).
func OverlapGate(in OverlapInput) GateResult {
	touched := map[string]bool{}
	for _, f := range in.Touched {
		touched[f] = true
	}
	overlap := []string{}
	for _, f := range in.Changed {
		if f != "" && touched[f] {
			overlap = append(overlap, f)
		}
	}
	held := (len(overlap) > 0 || in.QueryFailed) && !MayReleaseOverlap(in.Acknowledged, in.Expired, in.Critical, in.TopNearLimit, in.QueryFailed)
	r := GateResult{Gate: "overlap", Applicable: true, Count: len(in.Changed)}
	r.Passed = CanEnterMaintenance(true, false, true, !held, false)
	if !r.Passed {
		r.Reason = "overlap hold: " + strings.Join(overlap, ",")
		if in.QueryFailed {
			r.Reason = "overlap query failed (fail closed)"
		}
		r.Failures = []string{r.Reason}
	}
	return r
}

// FormatItem is one changed top-level item and its formatting state.
type FormatItem struct {
	File, Item                            string
	ExistedBase, FormattedBase, Formatted bool
}

// FormatGate refuses new formatting debt on changed items (FAE-CTL-030 step 4).
// Pre-existing debt on an item that was already unformatted is not new.
func FormatGate(items []FormatItem) GateResult {
	failures := []string{}
	for _, i := range items {
		if !i.Formatted && (!i.ExistedBase || i.FormattedBase) {
			failures = append(failures, "new formatting debt: "+i.File+": "+i.Item)
		}
	}
	return verdict("format", true, len(items), failures, "no changed item examined")
}

// TestCounts summarises a `go test -json` stream. Test identities are
// "<package>:<Test>[/<subtest>]".
type TestCounts struct {
	ExitCode      int      `json:"exit_code"`
	Executed      int      `json:"executed"`
	Matched       int      `json:"matched"`
	MatchedPassed int      `json:"matched_passed"`
	Packages      int      `json:"packages"`
	Failed        []string `json:"failed,omitempty"`
	BuildFailed   []string `json:"build_failed,omitempty"`
	// suitePackages is each package the stream reported an outcome for, with
	// that outcome, and suiteExecuted each top-level test that ran and did
	// not end skipped ("<package>:<Test>"): what a suite's rerun without its
	// listed flakes is compared with (SuiteGate).
	suitePackages map[string]string
	suiteExecuted map[string]bool
}

// GoTestCounts parses a complete go test JSON stream. A test that started more
// often than it ended makes the stream incomplete, which is an error, never a
// pass. A failure is kept: a test or package that failed once counts as
// failed whatever later events under the same name say (a repeated run, or
// two names a redaction made one). filter is the required test filter, read
// as go test -run reads it (TestFilter); the tests it names are counted as
// matched. A filter that does not compile is an error.
func GoTestCounts(stream []byte, filter string) (TestCounts, error) {
	return GoTestCountsReader(bytes.NewReader(stream), filter)
}

// TestFilter is a required test filter as go test -run reads it: one or more
// alternatives, each a regular expression per level of a test's name (its
// top-level name, then each subtest's), split on '|' and '/' outside brackets
// and parentheses as go test splits them. Each level's expression is matched
// unanchored, so an unanchored filter matches by substring; the filter is
// anchored by writing ^ and $ in it. A test is one the filter names when it
// has exactly as many levels as one alternative and each level matches: a
// filter with no '/' names top-level tests only (their subtests run with
// them, and fail them), and "TestA/case" names TestA's subtests matching
// "case".
type TestFilter [][]*regexp.Regexp

// CompileTestFilter compiles filter as TestFilter; "" is nil, which names no
// test.
func CompileTestFilter(filter string) (TestFilter, error) {
	if filter == "" {
		return nil, nil
	}
	var f TestFilter
	for _, alternative := range splitTestFilter(filter) {
		var levels []*regexp.Regexp
		for _, level := range alternative {
			re, err := regexp.Compile(rewriteTestFilter(level))
			if err != nil {
				return nil, fmt.Errorf("test filter %q: %w", filter, err)
			}
			levels = append(levels, re)
		}
		f = append(f, levels)
	}
	return f, nil
}

// NewTestFilter compiles filter as TestFilter (CompileTestFilter).
func NewTestFilter(filter string) (TestFilter, error) {
	return CompileTestFilter(filter)
}

// rewriteTestFilter rewrites spaces and non-printable runes as testing's
// matcher rewrites subtest names and -run filter levels before matching.
func rewriteTestFilter(s string) string {
	var b []byte
	for _, r := range s {
		switch {
		case isTestFilterSpace(r):
			b = append(b, '_')
		case !strconv.IsPrint(r):
			q := strconv.QuoteRune(r)
			b = append(b, q[1:len(q)-1]...)
		default:
			b = append(b, string(r)...)
		}
	}
	return string(b)
}

func isTestFilterSpace(r rune) bool {
	if r < 0x2000 {
		switch r {
		case '\t', '\n', '\v', '\f', '\r', ' ', 0x85, 0xA0, 0x1680:
			return true
		}
	} else {
		if r <= 0x200a {
			return true
		}
		switch r {
		case 0x2028, 0x2029, 0x202f, 0x205f, 0x3000:
			return true
		}
	}
	return false
}

// splitTestFilter splits filter into alternatives and their levels as go
// test's -run does (testing's splitRegexp): on '|' and '/' outside brackets
// and parentheses, with a backslash escaping the byte after it.
func splitTestFilter(filter string) [][]string {
	var alternatives [][]string
	var levels []string
	s := filter
	brackets, parens := 0, 0
	for i := 0; i < len(s); {
		switch s[i] {
		case '[':
			brackets++
		case ']':
			if brackets--; brackets < 0 { // An unmatched ']' is legal.
				brackets = 0
			}
		case '(':
			if brackets == 0 {
				parens++
			}
		case ')':
			if brackets == 0 {
				parens--
			}
		case '\\':
			i++
		case '/':
			if brackets == 0 && parens == 0 {
				levels = append(levels, s[:i])
				s = s[i+1:]
				i = 0
				continue
			}
		case '|':
			if brackets == 0 && parens == 0 {
				levels = append(levels, s[:i])
				s = s[i+1:]
				i = 0
				alternatives = append(alternatives, levels)
				levels = nil
				continue
			}
		}
		i++
	}
	return append(alternatives, append(levels, s))
}

// Matches reports whether the filter names the test called name
// ("<Test>[/<subtest>...]").
func (f TestFilter) Matches(name string) bool {
	if len(f) == 0 {
		return false
	}
	elements := strings.Split(name, "/")
	for i := 1; i < len(elements); i++ {
		elements[i] = rewriteTestFilter(elements[i])
	}
	for _, levels := range f {
		if len(levels) != len(elements) {
			continue
		}
		matched := true
		for i, re := range levels {
			if !re.MatchString(elements[i]) {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

var goTestNameProbes = []string{
	"Test", "TestA", "TestZ", "Test_", "Test0", "Test9", "Test_x", "TestX_y9", "Test_Test", "TestTest",
	"TestÄö", "TestΣσ", "Testǅ", "Test日本", "TestHTTP2Server", "Test" + strings.Repeat("Q", 120),
}

// selectsEveryTest reports whether filter's top-level pattern in any
// alternative selects every top-level test, so that a recursive run with it is
// a whole-suite run (FAE-HOST-015).
func selectsEveryTest(filter string) bool {
	f, err := CompileTestFilter(filter)
	if err != nil {
		return false
	}
	for _, levels := range f {
		if len(levels) == 0 {
			continue
		}
		all := true
		for _, name := range goTestNameProbes {
			if !levels[0].MatchString(name) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

// ExactTestFilter is the filter naming exactly the test called name, and no
// other: each level of the name quoted and anchored. A derived-scope RED
// identity, which is a name rather than a pattern, reaches the landing's
// filter gate this way.
func ExactTestFilter(name string) string {
	levels := strings.Split(name, "/")
	for i, level := range levels {
		levels[i] = "^" + regexp.QuoteMeta(level) + "$"
	}
	return strings.Join(levels, "/")
}

// testOutcomeRank orders the outcomes a name can end with: the stronger one
// is kept.
var testOutcomeRank = map[string]int{"skip": 1, "pass": 2, "fail": 3, "build-fail": 4}

// keepTestOutcome records outcome for name unless a stronger one is recorded.
func keepTestOutcome(outcomes map[string]string, name, outcome string) {
	if testOutcomeRank[outcome] > testOutcomeRank[outcomes[name]] {
		outcomes[name] = outcome
	}
}

// GoTestCountsReader is GoTestCounts over a stream too large to hold in memory.
func GoTestCountsReader(stream io.Reader, filter string) (TestCounts, error) {
	c := TestCounts{suitePackages: map[string]string{}, suiteExecuted: map[string]bool{}}
	required, err := CompileTestFilter(filter)
	if err != nil {
		return c, err
	}
	scan := bufio.NewScanner(stream)
	scan.Buffer(make([]byte, 4096), 8<<20)
	started, ends, ended := map[string]int{}, map[string]int{}, map[string]string{}
	packages, failing := map[string]string{}, map[string]bool{}
	for scan.Scan() {
		line := bytes.TrimSpace(scan.Bytes())
		if len(line) == 0 {
			continue
		}
		var e struct{ Action, Package, Test, ImportPath, FailedBuild string }
		if err := json.Unmarshal(line, &e); err != nil {
			return c, fmt.Errorf("malformed go test JSON: %w", err)
		}
		switch {
		case e.Package == "" && e.ImportPath != "":
			if e.Action == "build-fail" {
				keepTestOutcome(packages, e.ImportPath, "build-fail")
			}
		case e.Package == "":
		case e.Test == "":
			if e.Action == "pass" || e.Action == "fail" || e.Action == "skip" {
				keepTestOutcome(packages, e.Package, e.Action)
				if e.FailedBuild != "" {
					keepTestOutcome(packages, e.Package, "build-fail")
				}
			}
		default:
			key := e.Package + ":" + e.Test
			switch e.Action {
			case "run":
				started[key]++
			case "pass", "fail", "skip":
				ends[key]++
				keepTestOutcome(ended, key, e.Action)
			}
		}
	}
	if err := scan.Err(); err != nil {
		return c, err
	}
	keys := make([]string, 0, len(started))
	for k := range started {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		end := ended[key]
		if started[key] > ends[key] {
			return c, errors.New("incomplete test stream: " + key)
		}
		pkg, test, _ := strings.Cut(key, ":")
		if required.Matches(test) {
			c.Matched++
			if end == "pass" {
				c.MatchedPassed++
			}
		}
		if end == "skip" {
			continue
		}
		c.Executed++
		if !strings.Contains(test, "/") {
			c.suiteExecuted[key] = true
		}
		if end == "fail" {
			c.Failed = append(c.Failed, key)
			failing[pkg] = true
		}
	}
	names := make([]string, 0, len(packages))
	for p := range packages {
		names = append(names, p)
	}
	sort.Strings(names)
	for _, p := range names {
		c.Packages++
		c.suitePackages[p] = packages[p]
		if packages[p] == "build-fail" || packages[p] == "fail" && !failing[p] {
			c.BuildFailed = append(c.BuildFailed, p)
		}
	}
	return c, nil
}

// FilterGate requires every test the task's required test filter names
// (TestFilter) to execute and pass across all test targets; a filter matching
// zero tests is vacuous and holds.
func FilterGate(c TestCounts, filter string) GateResult {
	failures := []string{}
	if filter == "" {
		failures = append(failures, "no required test filter declared")
	}
	if c.Matched != c.MatchedPassed {
		failures = append(failures, fmt.Sprintf("required test %s failed %d of %d", filter, c.Matched-c.MatchedPassed, c.Matched))
	}
	if c.ExitCode != 0 && len(failures) == 0 && c.Matched > 0 {
		failures = append(failures, fmt.Sprintf("filter run exited %d", c.ExitCode))
	}
	failures = append(failures, prefixed("build failed: ", c.BuildFailed)...)
	return verdict("filter", true, c.Matched, failures, "required test filter "+filter+" matched zero tests")
}

// CommandRun is one gate command's exit status and compiled package count.
type CommandRun struct {
	Argv     []string `json:"argv"`
	ExitCode int      `json:"exit_code"`
	Packages int      `json:"packages"`
}

// CompileGate requires every compile command (all targets including tests) to
// succeed and to have compiled at least one package.
func CompileGate(runs []CommandRun) GateResult {
	failures, packages := []string{}, 0
	for _, r := range runs {
		packages += r.Packages
		if r.ExitCode != 0 {
			failures = append(failures, fmt.Sprintf("%s exited %d", strings.Join(r.Argv, " "), r.ExitCode))
		}
	}
	if len(runs) > 0 && packages == 0 && len(failures) == 0 {
		return verdict("compile", true, 0, nil, "compile examined zero packages")
	}
	return verdict("compile", true, packages, failures, "no compile command declared")
}

// SuiteGate runs the full suite tolerating only known, listed flakes. A flake
// is listed by top-level test name, optionally package-qualified. A suite that
// exited nonzero fails unless a failure the gate judged explains the exit:
//   - a failed unlisted test or a failed build explains it, and fails the gate;
//   - nothing failed: nothing in what was parsed explains it, and it fails;
//   - only listed flakes failed: they do not explain it on their own. go test
//     prints the same stream when a package's only failure is the flake and
//     when its TestMain also exits 1 (a leak check, say), and a package with
//     no events at all leaves no trace in it. clean is then the same suite run
//     again with every listed flake skipped (SuiteFlakeSkip). It explains the
//     exit only when it exited 0, failed nothing, reported exactly the
//     packages the first run finished, and ran every top-level test the first
//     run ran but the skipped ones (suiteRerunGaps). Without it, or when it
//     falls short, the gate fails. One case the rerun cannot tell apart: a
//     TestMain exit in the flake's own package that happened beside the
//     flake's failure and does not recur in the rerun looks like the flake
//     alone, and passes.
func SuiteGate(c TestCounts, flakes []string, clean *TestCounts) GateResult {
	failures := prefixed("build failed: ", c.BuildFailed)
	for _, key := range c.Failed {
		if !listedFlake(key, flakes) {
			failures = append(failures, "test failed: "+key)
		}
	}
	switch {
	case c.ExitCode == 0 || len(failures) > 0:
	case len(c.Failed) == 0:
		failures = append(failures, fmt.Sprintf("suite exited %d with no failed test or build in its output", c.ExitCode))
	case clean == nil:
		failures = append(failures, fmt.Sprintf("suite exited %d with only listed flakes failing, and no run without them shows that nothing else failed", c.ExitCode))
	default:
		var unexplained []string
		for _, key := range clean.Failed {
			unexplained = append(unexplained, "test failed: "+key)
		}
		unexplained = append(unexplained, prefixed("package failed with no failed test: ", clean.BuildFailed)...)
		if clean.ExitCode != 0 {
			unexplained = append(unexplained, fmt.Sprintf("exited %d", clean.ExitCode))
		}
		unexplained = append(unexplained, suiteRerunGaps(c, *clean, flakes)...)
		if len(unexplained) > 0 {
			failures = append(failures, fmt.Sprintf("suite exited %d with only listed flakes failing, but its run without them: %s", c.ExitCode, strings.Join(unexplained, ", ")))
		}
	}
	return verdict("suite", true, c.Executed, failures, "full suite executed zero tests")
}

// suiteRerunShown is how many names suiteRerunGaps lists in one reason.
const suiteRerunShown = 5

// suiteRerunGaps is what clean, the suite run again with every listed flake's
// name skipped (SuiteFlakeSkip), left unshown of c:
//   - a package c finished that clean did not report, or one clean reported
//     that c did not finish (c was cut short before it, so a namesake there,
//     below, ran in neither run);
//   - a top-level test c ran, other than a skipped name, that clean did not
//     run (ended skipped, or never started), so a clean run with no tests in
//     it explains nothing;
//   - a namesake: go test's -skip names no package, so clean skipped a
//     package-qualified flake's name in every package. The skip is counted
//     only in the flake's own package. Elsewhere the namesake must have run
//     in c, in a package that passed there; a namesake in a package c failed
//     or did not finish ran in neither run, as far as the gate can show.
func suiteRerunGaps(c, clean TestCounts, flakes []string) []string {
	var gaps []string
	shown := func(names []string) string {
		if len(names) > suiteRerunShown {
			return strings.Join(names[:suiteRerunShown], ", ") + fmt.Sprintf(" and %d more", len(names)-suiteRerunShown)
		}
		return strings.Join(names, ", ")
	}
	var missing, unfinished []string
	for p := range c.suitePackages {
		if _, ok := clean.suitePackages[p]; !ok {
			missing = append(missing, p)
		}
	}
	for p := range clean.suitePackages {
		if _, ok := c.suitePackages[p]; !ok {
			unfinished = append(unfinished, p)
		}
	}
	sort.Strings(missing)
	sort.Strings(unfinished)
	if len(missing) > 0 {
		gaps = append(gaps, fmt.Sprintf("reported %d of the suite's %d packages", c.Packages-len(missing), c.Packages))
	}
	if len(unfinished) > 0 {
		gaps = append(gaps, fmt.Sprintf("reported %d package(s) the suite did not finish: %s", len(unfinished), shown(unfinished)))
	}
	skipped := map[string]bool{}
	for _, f := range flakes {
		if i := strings.LastIndexByte(f, ':'); i >= 0 {
			f = f[i+1:]
		}
		skipped[f] = true
	}
	var unrun, namesakes []string
	for key := range c.suiteExecuted {
		pkg, test, _ := strings.Cut(key, ":")
		switch {
		case !skipped[test]:
			if !clean.suiteExecuted[key] {
				unrun = append(unrun, key)
			}
		case listedFlake(key, flakes):
		case c.suitePackages[pkg] != "pass":
			namesakes = append(namesakes, key)
		}
	}
	sort.Strings(unrun)
	sort.Strings(namesakes)
	if len(unrun) > 0 {
		gaps = append(gaps, fmt.Sprintf("did not run %d test(s) the suite ran: %s", len(unrun), shown(unrun)))
	}
	if len(namesakes) > 0 {
		gaps = append(gaps, fmt.Sprintf("skipped %d test(s) that are not listed flakes in their package, in a package the suite failed or did not finish: %s", len(namesakes), shown(namesakes)))
	}
	return gaps
}

// SuiteFlakeSkip is the go test -skip pattern that runs the suite again
// without its listed flakes, when c needs that run to explain its exit
// (SuiteGate): it exited nonzero and every failure in it is a listed flake.
// Otherwise it is "". The pattern names each listed flake's top-level test,
// so its subtests are skipped with it. go test's -skip names no package, so
// a package-qualified flake's name is skipped in every package; SuiteGate
// counts it skipped only in the flake's own package (suiteRerunGaps).
func SuiteFlakeSkip(c TestCounts, flakes []string) string {
	if c.ExitCode == 0 || len(c.Failed) == 0 || len(c.BuildFailed) > 0 {
		return ""
	}
	for _, key := range c.Failed {
		if !listedFlake(key, flakes) {
			return ""
		}
	}
	var names []string
	for _, f := range flakes {
		if i := strings.LastIndexByte(f, ':'); i >= 0 {
			f = f[i+1:]
		}
		if f = regexp.QuoteMeta(f); f != "" && !slices.Contains(names, f) {
			names = append(names, f)
		}
	}
	sort.Strings(names)
	return "^(" + strings.Join(names, "|") + ")$"
}

// listedFlake tolerates a failing test (or subtest) whose top-level test is
// listed as "<Test>" or "<package>:<Test>".
func listedFlake(key string, flakes []string) bool {
	pkg, test, _ := strings.Cut(key, ":")
	top, _, _ := strings.Cut(test, "/")
	for _, f := range flakes {
		if f != "" && (f == top || f == pkg+":"+top) {
			return true
		}
	}
	return false
}

// FrozenGate runs every frozen-region check a target declares; a target that
// declares none makes the gate explicitly not applicable.
func FrozenGate(runs []CommandRun) GateResult {
	failures := []string{}
	for _, r := range runs {
		if r.ExitCode != 0 {
			failures = append(failures, fmt.Sprintf("frozen check %s exited %d", strings.Join(r.Argv, " "), r.ExitCode))
		}
	}
	return verdict("frozen", len(runs) > 0, len(runs), failures, "")
}

// QACheck is one QA-declared check re-run on the landing tip.
type QACheck struct {
	Argv   []string   `json:"argv"`
	Counts TestCounts `json:"counts"`
}

// QAGate re-runs every QA-declared check on the exact tip; each must exit 0
// with a positive executed count. No declared check is vacuous.
func QAGate(checks []QACheck) GateResult {
	failures := []string{}
	for _, q := range checks {
		name := strings.Join(q.Argv, " ")
		if q.Counts.ExitCode != 0 {
			failures = append(failures, fmt.Sprintf("QA check %s exited %d", name, q.Counts.ExitCode))
		}
		if q.Counts.Executed < 1 {
			failures = append(failures, "QA check "+name+" executed zero tests")
		}
	}
	return verdict("qa", true, len(checks), failures, "no QA-declared check re-run")
}

// HunkPresence is one own hunk and whether the landed tip still contains it.
type HunkPresence struct {
	File    string `json:"file"`
	Header  string `json:"header"`
	Present bool   `json:"present"`
}

// OwnHunkGate requires every reviewed file and hunk of the task in the landed
// diff (FAE-CTL-030). Files are compared by path; hunks by reverse application.
func OwnHunkGate(hunks []HunkPresence, reviewed, landed []string) GateResult {
	failures := []string{}
	for _, h := range hunks {
		if !h.Present {
			failures = append(failures, "own hunk missing: "+h.File+" "+h.Header)
		}
	}
	for _, f := range reviewed {
		if !slices.Contains(landed, f) {
			failures = append(failures, "reviewed file absent from landed diff: "+f)
		}
	}
	return verdict("own-hunk", true, len(hunks), failures, "no own hunk examined")
}

// LandingGate is the composite decision over every landing gate.
func LandingGate(results []GateResult) GateResult {
	failures, applicable := []string{}, 0
	for _, r := range results {
		if !r.Passed || !GatePasses(r.Applicable, r.Count, len(r.Failures)) {
			failures = append(failures, r.Gate)
		}
		if r.Applicable {
			applicable++
		}
	}
	return verdict("landing", true, applicable, failures, "no landing gate ran")
}

// isDocumentOnlyDiff reports whether diffFiles is non-empty, documentGlobs is
// non-empty, and every changed file in diffFiles matches at least one pattern
// in documentGlobs (FAE-CTL-052).
func isDocumentOnlyDiff(diffFiles, documentGlobs []string) bool {
	if len(diffFiles) == 0 || len(documentGlobs) == 0 {
		return false
	}
	hasFile := false
	for _, f := range diffFiles {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		hasFile = true
		if !workflow.MatchesAnyGlob(f, documentGlobs) {
			return false
		}
	}
	return hasFile
}

// IsDocumentOnlyDiff is the exported form of isDocumentOnlyDiff (FAE-CTL-052).
func IsDocumentOnlyDiff(diffFiles, documentGlobs []string) bool {
	return isDocumentOnlyDiff(diffFiles, documentGlobs)
}

// EvaluateLanding evaluates landing gates for either a document-only diff (when
// all DiffFiles match DocumentGlobs and DocumentChecks are configured, FAE-CTL-052)
// or a code diff (full FAE-CTL-030 gates).
func EvaluateLanding(in LandingEvaluationInput) (GateResult, []GateResult) {
	var gates []GateResult
	if len(in.Hunks) > 0 || len(in.ReviewedFiles) > 0 {
		landed := in.LandedFiles
		if len(landed) == 0 {
			landed = in.DiffFiles
		}
		gates = append(gates, OwnHunkGate(in.Hunks, in.ReviewedFiles, landed))
	}
	if isDocumentOnlyDiff(in.DiffFiles, in.DocumentGlobs) && len(in.DocumentChecks) > 0 {
		count, failures := EvaluateDocumentChecks(in.DocumentChecks, in.RequiredTests)
		for _, gName := range []string{"format", "compile", "filter"} {
			gates = append(gates, documentGateResult(gName, count, failures))
		}
		gates = append(gates, FrozenGate(in.FrozenRuns))
		if len(in.QAChecks) > 0 {
			gates = append(gates, QAGate(in.QAChecks))
		} else {
			gates = append(gates, documentGateResult("qa", count, failures))
		}
		gates = append(gates, documentGateResult("suite", count, failures))
		return LandingGate(gates), gates
	}
	gates = append(gates,
		FormatGate(in.FormatItems),
		CompileGate(in.CompileRuns),
		FilterGate(in.FilterCounts, in.Filter),
		FrozenGate(in.FrozenRuns),
		QAGate(in.QAChecks),
		SuiteGate(in.SuiteCounts, in.Flakes, in.CleanSuite),
	)
	return LandingGate(gates), gates
}

func prefixed(prefix string, values []string) []string {
	out := []string{}
	for _, v := range values {
		out = append(out, prefix+v)
	}
	return out
}
