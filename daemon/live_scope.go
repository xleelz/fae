package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xleelz/fae/effects"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/execution"
	"github.com/xleelz/fae/provider"
	"github.com/xleelz/fae/workflow"
	"github.com/xleelz/fae/worktree"
)

// liveTaskScope is the file scope and RED identity one task's live
// dispatches are held to (see resolveLiveTaskScope).
type liveTaskScope struct {
	// Files are the paths a change may touch, approved scope-widen files
	// included (FAE-WF-078).
	Files []string
	// ReadOnlyFiles are readable and never editable (FAE-RT-017).
	ReadOnlyFiles []string
	// TestFile, TestID and ExpectedFailure are the RED identity. A derived
	// scope leaves them empty until the task has a usable RedReport.
	TestFile, TestID, ExpectedFailure string
	// Derived marks a scope taken from the task itself, not configuration.
	Derived bool
	// redReported is whether a derived scope found a RedReport; redRefusal
	// is why a derived scope has no usable RED identity.
	redReported bool
	redRefusal  error
	// protectedRefusal is why a derived scope's model-chosen files cannot
	// be held: its Spec declared a protected path (liveProtectedSpecFiles).
	protectedRefusal error
}

// needRed refuses a dispatch that needs the RED identity a derived scope
// cannot name. A pinned scope always names its configured identity.
func (s liveTaskScope) needRed() error {
	if s.Derived && s.redRefusal != nil {
		return s.redRefusal
	}
	return nil
}

// brokenRed refuses a dispatch that may run without a RED identity but must
// not ignore a RedReport the task did record: a broken identity is never
// silently dropped.
func (s liveTaskScope) brokenRed() error {
	if s.redReported {
		return s.needRed()
	}
	return nil
}

// needFiles refuses a change or VCS check of a derived scope with no files,
// or whose Spec declared a protected path.
func (s liveTaskScope) needFiles() error {
	if s.protectedRefusal != nil {
		return s.protectedRefusal
	}
	if s.Derived && len(s.Files) == 0 {
		return liveScopeRefusal("the task declares no files: declare them at admission (--declared-file) or in its Spec")
	}
	return nil
}

func liveScopeRefusal(format string, args ...any) error {
	return &execution.ScopeRefusal{Detail: fmt.Sprintf(format, args...)}
}

// DeclaredPathSafe is workflow.DeclaredPathSafe: the one path rule for every
// file a task may declare or a scope may hold (relative, clean, bounded and
// never in .git at any depth). See there for where it applies.
func DeclaredPathSafe(name string) bool {
	return workflow.DeclaredPathSafe(name)
}

// resolveLiveTaskScope is the one scope resolver of live execution; every
// reader of a task's files or RED identity uses it. Precedence:
//
//  1. a tasks[id] override pins the task;
//  2. non-empty instance files pin it, with the instance RED identity;
//  3. otherwise the scope derives from the task itself: its admission's
//     declared files, else the declared_files of its latest Spec, and the
//     RED identity of its latest RedReport.
//
// Approved scope-widen files join every mode (FAE-WF-078). The pinned modes
// never read the task, so load is called only for a derived scope. An
// approved path a widen may no longer add (inside .git, or protected) is left
// out (liveApprovedScope): the engine drops it from the job, with an event,
// before the next dispatch, and a reader of a job it has not prepared since
// (a landing, a sub-workflow) holds the same narrower scope.
//
// A derived scope never holds a protected path its Spec chose
// (workflow.ProtectedPath, FAE-SEC-005): such a path is left out of the
// files, so no turn reads or sends it, and every change turn and VCS check
// of the task is refused, naming it, until a new Spec declares the task's
// files without it (liveProtectedSpecFiles). Turns that change nothing,
// a new spec turn among them, still run. Files the operator declared at
// admission carry the operator's authorization and are not judged by it.
//
// A derived identity is model supplied; the engine's mechanical checks hold
// it (FAE-WF-073/076): the RED gate needs exactly one matched test failing
// for the declared reason and present in source, freeze and compare hash the
// declared test function, and every later change is held to the frozen
// identity. Once the task froze a RED, the RedReport bound to that freeze
// stays its identity; a later RedReport counts only for the dispatches of a
// new RED loop (redLoop), which must freeze again. A scope widen approved
// after the freeze does not move the frozen RED file.
func (x *liveExecutor) resolveLiveTaskScope(task string, approved []string, stage workflow.Stage, load func() (envelope, error)) (liveTaskScope, error) {
	var s liveTaskScope
	approved = liveApprovedScope(approved)
	pinned, ok := x.config.Tasks[task]
	if !ok && len(x.config.Files) > 0 {
		pinned, ok = LiveTaskConfig{Files: x.config.Files, TestFile: x.config.TestFile, TestID: x.config.TestID, ExpectedFailure: x.config.ExpectedFailure}, true
	}
	if ok {
		s.Files = append([]string(nil), pinned.Files...)
		if len(approved) > 0 {
			s.Files = engine.ScopedFiles(s.Files, approved)
		}
		if len(stage.ReadOnlyFiles) > 0 {
			s.ReadOnlyFiles = append([]string(nil), stage.ReadOnlyFiles...)
		}
		s.TestFile, s.TestID, s.ExpectedFailure = pinned.TestFile, pinned.TestID, pinned.ExpectedFailure
		if load != nil {
			if e, err := load(); err == nil {
				for _, ro := range e.Admission.ReadOnlyFiles {
					if DeclaredPathSafe(ro) && !slices.Contains(s.ReadOnlyFiles, ro) {
						s.ReadOnlyFiles = append(s.ReadOnlyFiles, ro)
					}
				}
				if err := checkScopeDenyPaths(s.Files, s.TestFile, e.Job.DenyPaths); err != nil {
					return s, err
				}
				if err := checkScopeDenyPaths(s.ReadOnlyFiles, "", e.Job.DenyPaths); err != nil {
					return s, err
				}
			}
		}
		return s, nil
	}
	s.Derived = true
	if load == nil {
		return s, errors.New("derived task scope requires the task")
	}
	e, err := load()
	if err != nil {
		return s, err
	}
	for _, ro := range append(append([]string(nil), e.Admission.ReadOnlyFiles...), stage.ReadOnlyFiles...) {
		if DeclaredPathSafe(ro) && !slices.Contains(s.ReadOnlyFiles, ro) {
			s.ReadOnlyFiles = append(s.ReadOnlyFiles, ro)
		}
	}
	if err := checkScopeDenyPaths(s.ReadOnlyFiles, "", e.Job.DenyPaths); err != nil {
		return s, err
	}
	declared, source := e.Admission.DeclaredFiles, "admission declared_files"
	if len(declared) == 0 {
		if spec := latestLiveEvidence(e.Job, "Spec"); spec != nil {
			if declared, err = liveEvidencePaths(spec.Fields["declared_files"]); err != nil {
				return s, liveScopeRefusal("Spec declared_files: %v", err)
			}
			source = "Spec declared_files"
		}
	}
	seen := map[string]bool{}
	for _, name := range declared {
		if !DeclaredPathSafe(name) || seen[name] {
			return s, liveScopeRefusal("%s path %q is unsafe or duplicate", source, name)
		}
		seen[name] = true
	}
	if source == "Spec declared_files" {
		declared, s.protectedRefusal = liveProtectedSpecFiles(declared)
		if s.protectedRefusal == nil {
			if err := checkScopeDenyPaths(declared, "", e.Job.DenyPaths); err != nil {
				s.protectedRefusal = err
				var kept []string
				for _, f := range declared {
					if workflow.MatchDenyPathIndex(e.Job.DenyPaths, f) < 0 {
						kept = append(kept, f)
					}
				}
				declared = kept
			}
		}
	} else if err := checkScopeDenyPaths(declared, "", e.Job.DenyPaths); err != nil {
		return s, err
	}
	if len(e.Job.DenyPaths) > 0 {
		var keptApproved []string
		for _, f := range approved {
			if workflow.MatchDenyPathIndex(e.Job.DenyPaths, f) < 0 {
				keptApproved = append(keptApproved, f)
			}
		}
		approved = keptApproved
	}
	s.Files = engine.ScopedFiles(declared, approved)
	if s.Files == nil {
		s.Files = []string{}
	}
	redLoop := stage.Phase == "test-first" || stage.Primitive == workflow.FreezeArtifact || stage.Primitive == workflow.HostCommand && engine.RedPhase(e.Job)
	red, frozen := liveRedEvidence(e.Job, redLoop)
	if red == nil {
		s.redRefusal = liveScopeRefusal("the task has no RedReport naming its RED identity")
		return s, nil
	}
	s.redReported = true
	// A frozen RedReport's expected failure was proved by the RED gate and
	// no later gate reads it, so it is not judged again: a task frozen under
	// an earlier reason rule keeps its RED.
	held := frozen && !redLoop
	file, id, expected, err := liveRedIdentity(red.Fields, s.Files, !held)
	if err != nil && held && len(approved) > 0 {
		// A widen approved after the freeze can put a second test file the
		// frozen RedReport lists in scope. That report had exactly one
		// in-scope test file when its RED froze; when that file is one of
		// the declared files, which no widen adds, it is still the RED file,
		// and the identity checks against the freeze hold it as before. A
		// RED file that itself came in with a widen stays ambiguous.
		if frozenFile, frozenID, frozenReason, again := liveRedIdentity(red.Fields, declared, false); again == nil {
			file, id, expected, err = frozenFile, frozenID, frozenReason, nil
		}
	}
	if err != nil {
		s.redRefusal = err
		return s, nil
	}
	s.TestFile, s.TestID, s.ExpectedFailure = file, id, expected
	if err := checkScopeDenyPaths(s.Files, s.TestFile, e.Job.DenyPaths); err != nil {
		return s, err
	}
	return s, nil
}

// liveApprovedScope is the approved scope-widen files a live scope holds:
// those a widen may still add. A path inside .git at any depth
// (DeclaredPathSafe) or a protected path (workflow.ProtectedPath) is left
// out; only a job stored before the engine refused such widens holds one,
// and the engine drops it with a scope-dropped event (FAE-WF-078).
func liveApprovedScope(approved []string) []string {
	var kept []string
	for _, name := range approved {
		if _, protected := workflow.ProtectedPath(name); DeclaredPathSafe(name) && !protected {
			kept = append(kept, name)
		}
	}
	return kept
}

// liveProtectedSpecFiles splits a Spec's declared files into those a derived
// scope may hold and a refusal naming every protected one
// (workflow.ProtectedPath, FAE-SEC-005). The refusal names the remedies:
// a new Spec without the path (restart the task with information), or, for a
// task that must change such a file, its files declared by the operator at
// admission.
func liveProtectedSpecFiles(declared []string) ([]string, error) {
	var kept, protected []string
	for _, name := range declared {
		if why, ok := workflow.ProtectedPath(name); ok {
			protected = append(protected, fmt.Sprintf("%q (%s)", name, why))
			continue
		}
		kept = append(kept, name)
	}
	if len(protected) == 0 {
		return kept, nil
	}
	return kept, liveScopeRefusal("Spec declared_files names protected paths %s: a model-chosen task scope never holds CI configuration, a file that makes the operator's tools run programs or a credential-like file, so the task's change turns are refused; restart it with information so its spec declares its files without them; a task that must change such a file needs its files declared by the operator at admission (fae run --declared-file)", strings.Join(protected, ", "))
}

// liveRedIdentity reads a RedReport's fields as a derived RED identity over
// the task's files. It is the one rule for a derived RED: the resolver
// applies it to the selected RedReport, and checkRedReport to a turn's report
// when the turn completes. The RED file is the single in-scope *_test.go of
// its declared files. A derived RED file is a Go test file, so its identity
// is a Go test function (redIdentity) and the test id must be a Go test name;
// script and runner shapes need a pinned identity (tasks[id]), where any
// literal identifier effects.SafeTestID accepts is allowed. The expected
// failure must be present and, with checkReason, name a reason
// (liveRedReasonVacuous).
func liveRedIdentity(fields map[string]any, files []string, checkReason bool) (file, id, expected string, err error) {
	if file, err = liveRedFile(fields, files); err != nil {
		return "", "", "", err
	}
	id, _ = fields["test_id"].(string)
	expected, _ = fields["expected_failure"].(string)
	if !strings.HasSuffix(file, ".go") && !effects.SafeTestID(id) {
		return "", "", "", liveScopeRefusal("RedReport test_id %q is not a literal test identifier", id)
	}
	if strings.HasSuffix(file, ".go") && !goTestName.MatchString(id) {
		return "", "", "", liveScopeRefusal("RedReport test_id %q is not a Go test name", id)
	}
	if strings.TrimSpace(expected) == "" {
		return "", "", "", liveScopeRefusal("RedReport names no expected_failure")
	}
	if checkReason && liveRedReasonVacuous(expected, id, file) {
		return "", "", "", liveScopeRefusal("RedReport expected_failure %q names no failure reason: nothing in it goes beyond the test's name or part of it, the RED file's name or a file:line position or part of one, go test's framing, a panic's generic text, the words go test and the Go runtime print around any failure (the bare labels got, want, expected and actual among them), or numbers alone; quote the failure's own message or values, a panic's message or the timeout", expected)
	}
	return file, id, expected, nil
}

// liveRedTraceWords are the words go test and the Go runtime print around
// any failing test whatever its reason, plus an assertion's bare labels: go
// test's framing and exit lines (FAIL, --- FAIL:, ok, exit status), a panic's
// generic text (panic:, [recovered, repanicked], goroutine N [running], the
// runtime and testing frames, created by testing.(*T).Run in goroutine N and
// the Go root's source paths), a timeout's "running tests:" and got, want,
// expected and actual, which name no value. A panic's own message and a
// timeout's "test timed out" are not among them. "error" is not either: the
// one generic text that holds it is a runtime panic's "runtime error", which
// liveRedTracePhrases covers, so "expected error" and "want error", the
// whole message of t.Error("expected error"), name a reason. The bridge's
// output protocol states the same words (provider.RedReasonTraceWords).
var liveRedTraceWords = map[string]bool{
	"fail": true, "failed": true, "pass": true, "passed": true, "ok": true, "run": true, "running": true, "skip": true, "skipped": true, "pause": true, "cont": true, "name": true, "bench": true, "exit": true, "status": true, "test": true, "tests": true,
	"panic": true, "panicked": true, "recovered": true, "repanicked": true, "goroutine": true, "runtime": true, "testing": true, "trunner": true, "func": true, "created": true, "by": true, "in": true, "t": true, "go": true, "src": true, "usr": true, "local": true,
	"got": true, "want": true, "expected": true, "actual": true,
}

// liveRedTracePhrases are generic texts that hold a word which is a reason
// elsewhere: a runtime panic's "runtime error" holds "error". A fragment that
// lies within one ("error", "runtime error") is vacuous, and the phrase is
// dropped from a longer fragment like the numbered trace text.
var liveRedTracePhrases = []string{"runtime error"}

// liveRedTraceText is the numbered text of the same lines: a source position
// or any end of one (file.go:12, file.go:12:5, and go:12 or o:12, which match
// every position on line 12), a duration in parentheses ((0.00s), (2s)),
// "exit status 1", "goroutine 7", a frame with its argument list of hex words
// or "..." (pkg.f({0x6b5b30, 0x564600}), pkg.Lookup(...)), an empty argument
// list, a frame's +0x1d offset, a closure's .func1.2 suffix and the
// liveRedTracePhrases. It is matched against lower-cased text.
var liveRedTraceText = regexp.MustCompile(`(?:\S*\.go|go|o):[0-9]+(:[0-9]+)?|\((?:[0-9]+(?:\.[0-9]+)?(?:ns|us|µs|ms|s|m|h))+\)|exit status [0-9]+|goroutine [0-9]+|[^\s()]*\((?:0x[0-9a-f]+|[{}, ?]|\.\.\.)+\)|\(\)|\+0x[0-9a-f]+|\.func[0-9]+(?:\.[0-9]+)*|runtime error`)

// liveRedNumber is a word that is only a number: decimal, hex (0x, 0xea) or
// a duration (10s, 5m0s, the 00s of 0.00s).
var liveRedNumber = regexp.MustCompile(`^(?:[0-9]+|0x[0-9a-f]*|(?:[0-9]+(?:ns|us|µs|ms|s|m|h))+)$`)

// liveRedReasonVacuous reports a derived RED's expected-failure fragment
// that says nothing about why its test fails (FAE-WF-073): one that would
// match a failure of the test whatever its reason. The broker matches the
// fragment literally against the test's failure text: its own output without
// the goroutine stacks in it and without go test's framing lines
// (effects.parseTests). That text still holds, for any failure, the test's
// name (in a timeout's list of running tests), the RED file's name and line
// numbers (in each logged line's file.go:N: prefix), a panic's generic
// words, the bare labels of an assertion, and numbers and hex words of every
// kind.
//
// Compared without letter case, the fragment is vacuous when it lies within
// the test id, the RED file's name or a generic phrase ("TestX", "Answer"
// for TestAnswer, "x_test.go", "error" within "runtime error"), or when,
// after dropping every whitespace-separated chunk that holds the test id or
// the RED file's name (a framing line, a pkg.TestX(...) frame, a x_test.go:7:
// prefix, a source path to the RED file) and then the numbered trace text
// (liveRedTraceText), every remaining word is a trace word
// (liveRedTraceWords) or a number (liveRedNumber), and numbers stand alone:
// no word and no comparison (=, !=, <, >) beside them. Any other word is
// content, and a number with a word or comparison beside it is content.
//
// So "FAIL", "--- FAIL: TestX (0.00s)", "x_test.go:7", "go:7", "panic:
// [recovered]", "error", "runtime error", "goroutine 6 [running]:",
// "runtime", "want", "got want", "1", "42" and "0xea" are vacuous, while "got
// 0 want 42", "want 42", "0 != 42", "expected error", "divide by zero" for
// TestDivideByZero, "assignment to entry in nil map", "SIGSEGV", "test timed
// out", "testdata/missing.json" and "want application/json" name a reason. A
// fragment made of words from the test's name with spaces between them
// ("divide by zero") is not the name: the name never holds a space, so only
// the failure's message can match it. Words of the RED file's directories
// ("overflow" for internal/overflow/parse_test.go), of the module or of the
// worktree's path are content: only a goroutine stack prints those whatever
// the reason, and the broker does not match stacks. The rule reads only the
// fragment, the test id and the RED file: text the test logs or prints
// itself before it fails for another reason is not detected; the reviewers
// judge the RED's source.
func liveRedReasonVacuous(expected, id, file string) bool {
	text, id, name := strings.ToLower(expected), strings.ToLower(id), strings.ToLower(path.Base(file))
	if file == "" {
		name = ""
	}
	core := strings.TrimFunc(text, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	if core == "" || id != "" && strings.Contains(id, core) || name != "" && strings.Contains(name, core) {
		return true
	}
	for _, phrase := range liveRedTracePhrases {
		if strings.Contains(phrase, core) {
			return true
		}
	}
	var kept []string
	for _, chunk := range strings.Fields(text) {
		if id != "" && strings.Contains(chunk, id) || name != "" && strings.Contains(chunk, name) {
			continue
		}
		kept = append(kept, chunk)
	}
	rest := liveRedTraceText.ReplaceAllString(strings.Join(kept, " "), " ")
	numbers, words := false, false
	for _, token := range strings.FieldsFunc(rest, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		switch {
		case liveRedNumber.MatchString(token):
			numbers = true
		case liveRedTraceWords[token]:
			words = true
		default:
			return false
		}
	}
	return !numbers || !words && !strings.ContainsAny(rest, "=<>≠≤≥")
}

// dispatchLiveScope resolves the scope a dispatch of the task is held to.
func (x *liveExecutor) dispatchLiveScope(d engine.Dispatch) (liveTaskScope, error) {
	s, err := x.resolveLiveTaskScope(d.TaskID, d.Scope, d.Config, func() (envelope, error) {
		e, err := x.taskEnvelope(d.TaskID)
		return dispatchedEnvelope(e, d), err
	})
	if err != nil {
		return s, err
	}
	if e, eErr := x.taskEnvelope(d.TaskID); eErr == nil {
		if err := applyStageWriteScopes(d, e, &s); err != nil {
			return s, err
		}
	}
	writeScope := d.WriteScope
	if len(writeScope) == 0 && len(d.Config.WriteScope) > 0 {
		writeScope = d.Config.WriteScope
	}
	if len(writeScope) > 0 {
		s = narrowScopeToWriteScope(s, writeScope)
	}
	return s, nil
}

func narrowScopeToWriteScope(s liveTaskScope, writeScope []string) liveTaskScope {
	if len(writeScope) == 0 {
		return s
	}
	narrowed := make([]string, 0, len(s.Files))
	for _, f := range s.Files {
		if workflow.MatchesAnyGlob(f, writeScope) {
			narrowed = append(narrowed, f)
		}
	}
	s.Files = narrowed
	if s.TestFile != "" && !workflow.MatchesAnyGlob(s.TestFile, writeScope) {
		if !slices.Contains(s.ReadOnlyFiles, s.TestFile) {
			s.ReadOnlyFiles = append(s.ReadOnlyFiles, s.TestFile)
		}
		s.TestFile = ""
	}
	return s
}

// latestLiveEvidence is the task's latest recorded artifact of a type.
func latestLiveEvidence(j engine.Job, artifact string) *workflow.Evidence {
	for i := len(j.Evidence) - 1; i >= 0; i-- {
		if j.Evidence[i].Type == artifact {
			return &j.Evidence[i]
		}
	}
	return nil
}

// liveRedEvidence selects the RedReport that names a derived RED identity,
// and reports whether it is the one bound to the latest freeze. Before any
// freeze it is the latest one. Once the task froze a RED, the RedReport bound
// to the latest freeze (the latest recorded before it) stays the identity; a
// newer one counts only for the dispatches of a new RED loop (redLoop:
// test-first, its RED run and the freeze that ends it).
func liveRedEvidence(j engine.Job, redLoop bool) (*workflow.Evidence, bool) {
	frozen, latest := -1, -1
	for i, a := range j.Evidence {
		if a.Type == "FrozenIdentity" || j.Document.Stages[a.Stage].Primitive == workflow.FreezeArtifact {
			frozen = i
		}
		if a.Type == "RedReport" {
			latest = i
		}
	}
	if latest < 0 {
		return nil, false
	}
	if frozen < 0 || latest < frozen || redLoop {
		return &j.Evidence[latest], latest < frozen
	}
	for i := frozen - 1; i >= 0; i-- {
		if j.Evidence[i].Type == "RedReport" {
			return &j.Evidence[i], true
		}
	}
	return nil, false
}

// liveRedFile is the RED file a RedReport names: the single in-scope
// *_test.go path among its declared files, each of which must be safe.
func liveRedFile(fields map[string]any, files []string) (string, error) {
	declared, err := liveEvidencePaths(fields["declared_files"])
	if err != nil {
		return "", liveScopeRefusal("RedReport declared_files: %v", err)
	}
	inScope := map[string]bool{}
	for _, name := range files {
		inScope[name] = true
	}
	seen := map[string]bool{}
	var tests []string
	for _, name := range declared {
		if !DeclaredPathSafe(name) || seen[name] {
			return "", liveScopeRefusal("RedReport declared_files path %q is unsafe or duplicate", name)
		}
		seen[name] = true
		shape, _ := fields["test_shape"].(string)
		if workflow.TestSourceFile(name, shape) && inScope[name] {
			tests = append(tests, name)
		}
	}
	if len(tests) != 1 {
		return "", liveScopeRefusal("RedReport declares %d in-scope *_test.go files %v; the RED file must be exactly one", len(tests), tests)
	}
	return tests[0], nil
}

// liveEvidencePaths reads an artifact's list of paths.
func liveEvidencePaths(v any) ([]string, error) {
	switch list := v.(type) {
	case nil:
		return nil, nil
	case []string:
		return list, nil
	case []any:
		out := make([]string, 0, len(list))
		for _, item := range list {
			name, ok := item.(string)
			if !ok {
				return nil, errors.New("a declared file is not a string")
			}
			out = append(out, name)
		}
		return out, nil
	}
	return nil, errors.New("declared files are not a list")
}

// liveCommitMessage is a task commit's message. A pinned commit_message keeps
// its form; otherwise the subject is the task's title, else the first line
// of its content (trimmed, at most 72 characters), and a Fae task trailer
// names the task. The dispatch trailer follows either way. It depends only
// on the admission and dispatch read once per commit attempt, so the prepared
// commit a retry or recovery compares is the same object.
func liveCommitMessage(pinned string, a AdmitParams, task, dispatch string) string {
	if pinned != "" {
		return pinned + "\n\nFae dispatch: " + dispatch
	}
	subject := liveCommitSubject(a.Title)
	if subject == "" {
		subject = liveCommitSubject(a.Content)
	}
	if subject == "" {
		subject = "Fae task " + task
	}
	return subject + "\n\nFae task: " + task + "\nFae dispatch: " + dispatch
}

// liveCommitSubject is the first non-empty line of text, trimmed and capped
// at 72 characters.
func liveCommitSubject(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		if r := []rune(line); len(r) > 72 {
			line = strings.TrimSpace(string(r[:72]))
		}
		return line
	}
	return ""
}

// heldToFrozenRed reports a change producer held to the frozen RED: every
// one after the RED freeze, phase label or not (FAE-WF-073), unless the task
// is an exempt no-red task.
func (x *liveExecutor) heldToFrozenRed(d engine.Dispatch, doc workflow.Document) bool {
	return (d.Config.Phase == "implement" || workflow.ProducesChanges(doc, d.Config) && d.Config.Phase != "test-first" && workflow.CodeBearing(doc, d.TaskClass)) && !x.noRedExempt(d)
}

// liveRedTurnChange holds a test-first turn's changed files. With a RED file
// it may change only that file. A derived scope without one yet, or whose
// latest RedReport is broken, lets the turn author its RED: at most one
// changed file, in scope and named *_test.go.
func liveRedTurnChange(s liveTaskScope, changed []string) error {
	var names []string
	for _, name := range changed {
		if name != "" {
			names = append(names, name)
		}
	}
	if s.Derived && s.TestFile == "" {
		if len(names) > 1 {
			return fmt.Errorf("RED turn changed %d files; a first RED changes exactly one in-scope test file", len(names))
		}
		if len(names) == 1 && (!workflow.TestSourceFile(names[0], "") || !slices.Contains(s.Files, names[0])) {
			return fmt.Errorf("RED turn changed a non-test file, %q: a first RED changes one in-scope test file", names[0])
		}
		return nil
	}
	for _, name := range names {
		if name != s.TestFile {
			return fmt.Errorf("RED turn changed a non-test file, %q: a RED turn changes only its RED file %q", name, s.TestFile)
		}
	}
	return nil
}

// liveRedReportRefused marks the refusal of a turn's RedReport, so the
// retried turn can be told why (liveRedRefusalNote).
const liveRedReportRefused = "RedReport refused at turn completion: "

// checkRedReport holds the RedReport a change turn of a derived scope
// produces to the derived RED rule (liveRedIdentity) when the turn completes,
// and its test, as the turn left it, to one test without subtests
// (liveRedSubtestRefusal), so a malformed report is refused where it is
// produced. The refusal is a typed ValidationFailed of the turn, so the
// engine retries the turn, within the stage's same-reason bound and the
// task's attempt limit (Appendix C step 7 loops back to step 5), instead of
// recording a report the RED run would refuse. A refused test-first turn's change, which checkChange has
// already held to at most its one RED file, is first restored to the
// dispatch tree: the retry then starts from the recorded tree, and the turn
// leaves no unrecorded residue. A pinned scope's RED identity is
// configuration, so its RedReport is not read here.
func (x *liveExecutor) checkRedReport(ctx context.Context, d engine.Dispatch, b execution.Binding, output json.RawMessage) error {
	if d.Config.Output != "RedReport" {
		return nil
	}
	scope, err := x.dispatchLiveScope(d)
	if err != nil || !scope.Derived {
		return err
	}
	var fields map[string]any
	refusal := json.Unmarshal(output, &fields)
	if refusal == nil {
		var file, id string
		if file, id, _, refusal = liveRedIdentity(fields, scope.Files, true); refusal == nil {
			refusal = x.liveRedSubtestRefusal(b, file, id)
		}
	}
	if refusal == nil {
		return nil
	}
	refusal = liveScopeRefusal("%s%v", liveRedReportRefused, strings.TrimPrefix(refusal.Error(), "task scope refused: "))
	if d.Config.Phase == "test-first" {
		if err = x.restoreRedTurn(ctx, d, b, scope); err != nil {
			return errors.Join(refusal, fmt.Errorf("refused RED turn not restored: %w", err))
		}
	}
	return refusal
}

// restoreRedTurn puts a refused test-first turn's change back to the
// dispatch tree. The change must be one liveRedTurnChange allows (at most the
// one RED file), each path a regular file or absent; anything else is left
// for the operator, and the executor then types the turn a rejected change.
func (x *liveExecutor) restoreRedTurn(ctx context.Context, d engine.Dispatch, b execution.Binding, scope liveTaskScope) error {
	w := x.bindingPath(b.Worktree)
	now, err := x.broker.Tree(ctx, w)
	if err != nil || now == d.Tree {
		return err
	}
	diff, err := effectGit(ctx, w, "diff", "--name-only", "--no-renames", "-z", d.Tree, now)
	if err != nil {
		return err
	}
	changed := strings.Split(diff, "\x00")
	if err = liveRedTurnChange(scope, changed); err != nil {
		return err
	}
	for _, name := range changed {
		if name == "" {
			continue
		}
		full := filepath.Join(b.Worktree, name)
		if !DeclaredPathSafe(name) {
			return fmt.Errorf("unsafe RED path %q", name)
		}
		if dir, e := filepath.EvalSymlinks(filepath.Dir(full)); e != nil || dir != filepath.Dir(full) {
			return fmt.Errorf("RED path %q is not physical", name)
		}
		if info, e := os.Lstat(full); e == nil && !info.Mode().IsRegular() || e != nil && !os.IsNotExist(e) {
			return fmt.Errorf("RED path %q is not a regular file", name)
		}
		entry, e := effectGit(ctx, w, "--literal-pathspecs", "ls-tree", "-z", d.Tree, "--", name)
		if e != nil {
			return e
		}
		if entry = strings.TrimSuffix(entry, "\x00"); entry == "" {
			if e = os.Remove(full); e != nil && !os.IsNotExist(e) {
				return e
			}
			continue
		}
		mode, object, ok := strings.Cut(entry, " blob ")
		blob, _, _ := strings.Cut(object, "\t")
		if !ok || mode != "100644" && mode != "100755" {
			return fmt.Errorf("RED path %q was not a regular file in the dispatch tree", name)
		}
		cmd := exec.CommandContext(ctx, "git", "cat-file", "blob", blob)
		cmd.Dir, cmd.Env = b.Worktree, cleanGitEnv()
		content, e := cmd.Output()
		if e != nil {
			return e
		}
		perm := os.FileMode(0o644)
		if mode == "100755" {
			perm = 0o755
		}
		if e = os.WriteFile(full, content, perm); e != nil {
			return e
		}
		if e = os.Chmod(full, perm); e != nil {
			return e
		}
	}
	if now, err = x.broker.Tree(ctx, w); err == nil && now != d.Tree {
		err = errors.New("worktree differs from the dispatch tree after restoring the RED turn")
	}
	return err
}

// liveRedRefusalNote tells a retried turn why its previous RedReport was
// refused: the stage's latest recorded outcome, when it is that refusal.
func liveRedRefusalNote(j engine.Job, stage string) string {
	for i := len(j.History) - 1; i >= 0; i-- {
		switch h := j.History[i]; h.Kind {
		case "failure":
			if h.Stage == stage && strings.Contains(h.Cause, liveRedReportRefused) {
				return "\n\nThe previous turn's RedReport was refused; the turn was retried from the dispatch tree:\n" + h.Cause
			}
			return ""
		case "transition":
			return ""
		}
	}
	return ""
}

// effectCheck refuses a host, freeze or VCS dispatch its scope cannot hold
// before anything starts: a targeted test run, freeze or compare needs the
// RED identity; a suite run or reconciliation must not ignore a broken one;
// a reconciliation or commit snapshot needs files.
func (s liveTaskScope) effectCheck(d engine.Dispatch) error {
	switch d.Config.Primitive {
	case workflow.HostCommand:
		if d.Config.TestRun && d.Config.TestShape != "" {
			return s.needRed()
		}
		if d.Config.TestRun {
			return s.brokenRed()
		}
	case workflow.FreezeArtifact, workflow.CompareFrozen:
		return s.needRed()
	case workflow.Merge:
		if err := s.needFiles(); err != nil {
			return err
		}
		return s.brokenRed()
	case workflow.VcsCommit:
		return s.needFiles()
	}
	return nil
}

// snapshotFiles are the files a snapshot of the scope names. A derived scope
// may declare files the task has not created yet; one absent from both the
// worktree and its HEAD holds no change, and git refuses to add it, so it is
// left out. Pinned scopes are named whole, as before.
func (s liveTaskScope) snapshotFiles(ctx context.Context, w worktree.Binding) ([]string, error) {
	if slices.ContainsFunc(s.Files, func(p string) bool { return strings.ContainsAny(p, "*?[") }) {
		return provider.ExpandScopedFiles(w.Path, s.Files, nil)
	}

	if !s.Derived || len(s.Files) == 0 {
		return s.Files, nil
	}
	out, err := effectGit(ctx, w, append([]string{"--literal-pathspecs", "ls-tree", "-r", "-z", "--name-only", "HEAD", "--"}, s.Files...)...)
	if err != nil {
		return nil, err
	}
	tracked := map[string]bool{}
	for _, name := range strings.Split(out, "\x00") {
		tracked[name] = name != ""
	}
	present := []string{}
	for _, name := range s.Files {
		if _, err := os.Lstat(filepath.Join(w.Path, name)); tracked[name] || err == nil {
			present = append(present, name)
		}
	}
	return present, nil
}

// briefScope is the scope the task_scope brief block reports. Its test file
// is always one of its files: a pinned instance test_file outside the pinned
// files (which no change can touch) is reported as none.
func (s liveTaskScope) briefScope() execution.TaskScope {
	scope := execution.TaskScope{Files: s.Files, ReadOnlyFiles: s.ReadOnlyFiles}
	if slices.Contains(s.Files, s.TestFile) {
		scope.TestFile = s.TestFile
	}
	return scope
}
