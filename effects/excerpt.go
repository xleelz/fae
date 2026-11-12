package effects

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// ExcerptLimit bounds the failure excerpt of one engine run. A rework turn's
// brief carries at most three full excerpts (48 KiB in total).
const ExcerptLimit = 16 << 10

// FailureExcerpt is the plain-text excerpt of a failed run that the turn the
// failure routes back to receives (the engine_feedback brief block). It never
// changes what the run proved; it only keeps the lines that say why it failed:
//
//   - go test -json: the "# package" headers and "file:line:col: message"
//     lines of build failures (from stderr and from build-output events, never
//     the -x command trace), each failed test's output in order with its
//     "=== RUN" and "--- FAIL" lines (a test left without a result of its own,
//     running when a timeout, os.Exit, crash or signal ended its test binary
//     or the broker cut the run, is shown too, with a line saying it is not
//     counted as executed unless go test's own -timeout named it), the output
//     of a failed package that had no failed test, and the package
//     "FAIL"/"ok" summary lines;
//   - gofmt -l: the listed files and any gofmt error;
//   - anything else: its stdout and stderr.
//
// checks are runs folded into this one (a QA run's required checks); each
// failed check adds its own section. A final line states the exit code and the
// counts the gate judged, and that the run was killed at its deadline when it
// was (RunEvidence.TimedOut) or its output cut at the byte bound when it was
// (RunEvidence.Truncated). Paths under any of roots (the worktree the command
// ran in, as given and as resolved) are shown relative to it. The result is
// valid UTF-8, at most ExcerptLimit bytes, cut by BoundExcerpt when longer.
func FailureExcerpt(argv []string, run RunEvidence, checks []RunEvidence, roots ...string) (string, bool) {
	var sections []string
	final := fmt.Sprintf("exit code %d, executed %d, matched %d", run.ExitCode, run.ExecutedCount, run.MatchedCount)
	switch {
	case excerptGoTest(argv):
		sections = append(sections, goTestExcerpt(run.Output, run.Stderr))
	case len(argv) > 0 && filepath.Base(argv[0]) == "gofmt":
		listed := strings.Fields(run.Output)
		sections = append(sections, formatExcerpt(listed, run.Stderr))
		final = fmt.Sprintf("exit code %d, %d files listed", run.ExitCode, len(listed))
		if len(listed) == 1 {
			final = fmt.Sprintf("exit code %d, 1 file listed", run.ExitCode)
		}
	default:
		sections = append(sections, rawExcerpt(run.Output, run.Stderr))
	}
	if run.TimedOut {
		final += ", killed at the run's deadline"
	}
	if run.Truncated {
		final += ", output cut at the run's byte bound"
	}
	for i, c := range checks {
		if c.ExitCode == 0 {
			continue
		}
		body := rawExcerpt(c.Output, c.Stderr)
		if excerptTestJSON(c.Output) {
			body = goTestExcerpt(c.Output, c.Stderr)
		}
		sections = append(sections, strings.TrimRight(fmt.Sprintf("required check %d failed (exit code %d, executed %d):\n%s", i+1, c.ExitCode, c.ExecutedCount, body), "\n"))
	}
	var b strings.Builder
	for _, s := range sections {
		if s = strings.TrimRight(s, "\n"); s != "" {
			b.WriteString(s + "\n\n")
		}
	}
	b.WriteString(final + "\n")
	text := b.String()
	for _, root := range roots {
		if root = strings.TrimRight(root, "/"); root != "" {
			text = strings.ReplaceAll(text, root+"/", "")
		}
	}
	return BoundExcerpt(text, ExcerptLimit)
}

// BoundExcerpt returns text as valid UTF-8 of at most limit bytes. A longer
// text keeps its head and tail, at line boundaries where it can, around one
// "[... N bytes cut ...]" line, and reports the cut.
func BoundExcerpt(text string, limit int) (string, bool) {
	text = strings.ToValidUTF8(text, "�")
	if len(text) <= limit {
		return text, false
	}
	const reserve = 48 // the cut line with a separator and a 20-digit count
	if limit <= reserve {
		return excerptPrefix(text, max(limit, 0)), true
	}
	room := limit - reserve
	head := excerptPrefix(text, room/2)
	if i := strings.LastIndexByte(head, '\n'); i >= len(head)/2 {
		head = head[:i+1]
	}
	tail := excerptSuffix(text, room-room/2)
	if i := strings.IndexByte(tail, '\n'); i >= 0 && i < len(tail)/2 {
		tail = tail[i+1:]
	}
	separator := ""
	if head != "" && !strings.HasSuffix(head, "\n") {
		separator = "\n"
	}
	return head + separator + fmt.Sprintf("[... %d bytes cut ...]\n", len(text)-len(head)-len(tail)) + tail, true
}

func excerptPrefix(s string, n int) string {
	for n > 0 && n < len(s) && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:min(n, len(s))]
}

func excerptSuffix(s string, n int) string {
	start := max(len(s)-n, 0)
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[start:]
}

func excerptGoTest(argv []string) bool {
	if len(argv) < 2 || filepath.Base(argv[0]) != "go" || argv[1] != "test" {
		return false
	}
	for _, a := range argv[2:] {
		if a == "-json" {
			return true
		}
	}
	return false
}

// excerptTestJSON reports whether stdout is a test2json event stream.
func excerptTestJSON(stdout string) bool {
	line, _, _ := strings.Cut(strings.TrimSpace(stdout), "\n")
	var e struct{ Action string }
	return json.Unmarshal([]byte(line), &e) == nil && e.Action != ""
}

var (
	// excerptHeader is a compiler or vet package header: "# path",
	// "# [path]" or "# path [path.test]". The -x trace's "# get URL" and
	// "# cd dir; git ..." lines do not match.
	excerptHeader = regexp.MustCompile(`^# \[?[^\s\[\]]+\]?( \[[^\s\[\]]+\])?$`)
	// excerptPosition is a "file:line[:col]: message" diagnostic.
	excerptPosition = regexp.MustCompile(`^\S+?:\d+(:\d+)?: `)
)

// buildLines keeps the diagnostics of a build stream and drops its -x
// command trace (mkdir, cd, cat, WORK=, tool paths, heredoc bodies). A header
// is kept only when a diagnostic follows it; tab-indented continuation lines
// (have/want) and "too many errors" are kept after a diagnostic.
type buildLines struct {
	out     []string
	header  string
	inError bool
	goLines bool
}

func (b *buildLines) add(line string) {
	switch {
	case excerptHeader.MatchString(line):
		b.header, b.inError = line, false
	case excerptPosition.MatchString(line):
		if b.header != "" {
			b.out = append(b.out, b.header)
			b.header = ""
		}
		b.out, b.inError = append(b.out, line), true
	case b.inError && (strings.HasPrefix(line, "\t") || line == "too many errors"):
		b.out = append(b.out, line)
	case b.goLines && strings.HasPrefix(line, "go: ") && !strings.HasPrefix(line, "go: downloading ") && !strings.HasPrefix(line, "go: finding ") && !strings.HasPrefix(line, "go: extracting "):
		// The go command's own failures (a malformed go.mod) are on stderr.
		b.out, b.inError = append(b.out, line), true
	default:
		b.inError = false
	}
}

func goTestExcerpt(stdout, stderr string) string {
	type key struct{ pkg, test string }
	var (
		stderrLines = &buildLines{goLines: true}
		builds      = map[string]*buildLines{}
		buildOrder  []string
		pending     = map[string]string{}
		tests       = map[key]*strings.Builder{}
		testOrder   []key
		failedTest  = map[key]bool{}
		endedTest   = map[key]bool{} // the test's own pass, fail or skip
		pkgOutput   = map[string]*strings.Builder{}
		packages    = map[string]*strings.Builder{}
		pkgOrder    []string
		pkgFailed   = map[string]bool{}
		pkgExcused  = map[string]bool{} // a failed test or build already says why
		summary     []string
	)
	for _, line := range strings.Split(stderr, "\n") {
		stderrLines.add(line)
	}
	for _, raw := range strings.Split(stdout, "\n") {
		var e struct{ Action, Package, Test, Output, ImportPath, FailedBuild string }
		if strings.TrimSpace(raw) == "" {
			continue
		}
		if json.Unmarshal([]byte(raw), &e) != nil {
			stderrLines.add(raw)
			continue
		}
		if e.Action == "output" {
			if pkgOutput[e.Package] == nil {
				pkgOutput[e.Package] = &strings.Builder{}
			}
			pkgOutput[e.Package].WriteString(e.Output)
		}
		switch {
		case e.Action == "build-output":
			b := builds[e.ImportPath]
			if b == nil {
				b = &buildLines{}
				builds[e.ImportPath] = b
				buildOrder = append(buildOrder, e.ImportPath)
			}
			// Output may end mid-line; complete lines go to the filter.
			text := pending[e.ImportPath] + e.Output
			lines := strings.Split(text, "\n")
			pending[e.ImportPath] = lines[len(lines)-1]
			for _, l := range lines[:len(lines)-1] {
				b.add(l)
			}
		case e.Test != "":
			k := key{e.Package, e.Test}
			if tests[k] == nil {
				tests[k] = &strings.Builder{}
				testOrder = append(testOrder, k)
			}
			if e.Action == "output" {
				tests[k].WriteString(e.Output)
			}
			if e.Action == "fail" {
				failedTest[k] = true
				pkgExcused[e.Package] = true
			}
			endedTest[k] = endedTest[k] || e.Action == "pass" || e.Action == "fail" || e.Action == "skip"
		case e.Action == "output":
			for _, l := range strings.Split(strings.TrimRight(e.Output, "\n"), "\n") {
				switch {
				case strings.HasPrefix(l, "ok ") || strings.HasPrefix(l, "ok\t") || strings.HasPrefix(l, "FAIL\t"):
					summary = append(summary, l)
				case l == "PASS" || l == "FAIL" || strings.HasPrefix(l, "?"):
				default:
					if packages[e.Package] == nil {
						packages[e.Package] = &strings.Builder{}
						pkgOrder = append(pkgOrder, e.Package)
					}
					packages[e.Package].WriteString(l + "\n")
				}
			}
		case e.Action == "fail":
			pkgFailed[e.Package] = true
			if e.FailedBuild != "" {
				pkgExcused[e.Package] = true
			}
		}
	}
	var sections []string
	build := stderrLines.out
	for _, path := range buildOrder {
		b := builds[path]
		if rest := pending[path]; rest != "" {
			b.add(rest)
		}
		build = append(build, b.out...)
	}
	sections = append(sections, strings.Join(build, "\n"))
	var failures strings.Builder
	for _, k := range testOrder {
		if failedTest[k] || !endedTest[k] {
			pkgExcused[k.pkg] = true
			out := tests[k].String()
			if out != "" && !strings.HasSuffix(out, "\n") {
				out += "\n"
			}
			if !endedTest[k] && !goTestAlarmNamed(pkgOutput[k.pkg].String())[k.test] {
				out += k.test + " reported no result of its own: its test binary ended while it ran, so it is not counted as executed\n"
			}
			failures.WriteString(out)
		}
	}
	sections = append(sections, failures.String())
	var crashed strings.Builder
	for _, p := range pkgOrder {
		if pkgFailed[p] && !pkgExcused[p] {
			crashed.WriteString(packages[p].String())
		}
	}
	// Failed packages lead the summary; passing ones follow in stream order.
	sort.SliceStable(summary, func(i, j int) bool {
		return strings.HasPrefix(summary[i], "FAIL") && !strings.HasPrefix(summary[j], "FAIL")
	})
	sections = append(sections, crashed.String(), strings.Join(summary, "\n"))
	return excerptJoin(sections)
}

func formatExcerpt(listed []string, stderr string) string {
	var sections []string
	if len(listed) > 0 {
		sections = append(sections, "gofmt -l lists files whose formatting differs:\n"+strings.Join(listed, "\n"))
	}
	return excerptJoin(append(sections, stderr))
}

func rawExcerpt(stdout, stderr string) string {
	return excerptJoin([]string{stdout, stderr})
}

// excerptJoin separates the nonempty sections with one blank line.
func excerptJoin(sections []string) string {
	var kept []string
	for _, s := range sections {
		if s = strings.TrimRight(s, "\n"); strings.TrimSpace(s) != "" {
			kept = append(kept, s)
		}
	}
	return strings.Join(kept, "\n\n")
}
