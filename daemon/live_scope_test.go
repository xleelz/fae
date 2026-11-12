package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/effects"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/execution"
	"github.com/xleelz/fae/workflow"
	"github.com/xleelz/fae/worktree"
)

func scopeEnvelope(declared []string, evidence ...workflow.Evidence) envelope {
	return envelope{Admission: AdmitParams{ID: "task", DeclaredFiles: declared}, Job: engine.Job{TaskID: "task", Document: defaults.Workflow(), Evidence: evidence}}
}

func specEvidence(files ...any) workflow.Evidence {
	return workflow.Evidence{Type: "Spec", Stage: "spec", Fields: map[string]any{"declared_files": files}}
}

func redEvidence(id string, files ...any) workflow.Evidence {
	return workflow.Evidence{Type: "RedReport", Stage: "test-first", Fields: map[string]any{"test_id": id, "test_shape": "go-unit", "declared_files": files, "expected_failure": "got 0 want 42"}}
}

func isScopeRefusal(err error) bool {
	var refusal *execution.ScopeRefusal
	return errors.As(err, &refusal)
}

// An instance with empty files selects derived task scope: startup and
// fae doctor accept it, and refuse an instance RED identity it cannot own.
func TestLiveDerivedScopeConfigAccepted(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	c := LiveConfig{Command: []string{exe, "{config}"}, AllowedCommands: [][]string{defaults.TestCommand()}, Base: strings.Repeat("a", 40), IntegrationRef: "refs/heads/fae/integration", HostLedger: "/var/fae/host-ledger", HeavyCapacity: 2, TimeoutSeconds: 60}
	if issues := CheckLiveConfig(c); len(issues) != 0 {
		t.Fatal("derived scope configuration refused", issues)
	}
	if err = validateLiveScopeConfig(c); err != nil {
		t.Fatal(err)
	}
	pinned := c
	pinned.Files, pinned.TestFile, pinned.TestID, pinned.ExpectedFailure = []string{"a.go", "a_test.go"}, "a_test.go", "TestA", "want"
	if issues := CheckLiveConfig(pinned); len(issues) != 0 {
		t.Fatal("pinned configuration refused", issues)
	}
	for _, set := range []func(*LiveConfig){func(c *LiveConfig) { c.TestFile = "a_test.go" }, func(c *LiveConfig) { c.TestID = "TestA" }, func(c *LiveConfig) { c.ExpectedFailure = "want" }} {
		bad := c
		set(&bad)
		if issues := CheckLiveConfig(bad); len(issues) != 1 || !strings.Contains(issues[0], "derived task scope") || validateLiveScopeConfig(bad) == nil {
			t.Fatal("instance RED identity accepted with derived scope", issues)
		}
	}
}

// Derived files come from the admission's declared files, else the latest
// Spec's declared_files, plus approved scope-widen files; a tasks override
// and non-empty instance files still pin exactly as before. An unsafe
// declared path is a typed refusal, never dropped; an unsafe stored approved
// path is left out (the engine drops it from the job with an event).
func TestLiveDerivedScopeFiles(t *testing.T) {
	x := &liveExecutor{config: LiveConfig{Tasks: map[string]LiveTaskConfig{"pinned": {Files: []string{"p.go", "p_test.go"}, TestFile: "p_test.go", TestID: "TestP", ExpectedFailure: "p"}}}}
	resolve := func(task string, approved []string, e envelope) (liveTaskScope, error) {
		return x.resolveLiveTaskScope(task, approved, workflow.Stage{}, func() (envelope, error) { return e, nil })
	}
	s, err := resolve("task", nil, scopeEnvelope([]string{"z.go", "a.go"}, specEvidence("spec.go")))
	if err != nil || !s.Derived || !slices.Equal(s.Files, []string{"a.go", "z.go"}) {
		t.Fatal("admission declared files", s, err)
	}
	s, err = resolve("task", nil, scopeEnvelope(nil, specEvidence("old.go"), specEvidence("pkg/b.go", "pkg/a.go")))
	if err != nil || !slices.Equal(s.Files, []string{"pkg/a.go", "pkg/b.go"}) {
		t.Fatal("latest Spec declared files", s, err)
	}
	s, err = resolve("task", []string{"pkg/c.go", "pkg/a.go"}, scopeEnvelope(nil, specEvidence("pkg/b.go", "pkg/a.go")))
	if err != nil || !slices.Equal(s.Files, []string{"pkg/a.go", "pkg/b.go", "pkg/c.go"}) {
		t.Fatal("approved scope widen", s, err)
	}
	s, err = resolve("task", nil, scopeEnvelope(nil))
	if raw, _ := json.Marshal(s.Files); err != nil || string(raw) != "[]" || s.TestFile != "" {
		t.Fatal("scope before it is known", s, err)
	}
	for name, e := range map[string]envelope{
		"admission metadata": scopeEnvelope([]string{".git/config"}),
		"spec parent":        scopeEnvelope(nil, specEvidence("../outside.go")),
		"spec absolute":      scopeEnvelope(nil, specEvidence("/etc/passwd")),
		"spec unclean":       scopeEnvelope(nil, specEvidence("a/../b.go")),
		"spec duplicate":     scopeEnvelope(nil, specEvidence("a.go", "a.go")),
		"spec not a string":  scopeEnvelope(nil, specEvidence("a.go", 7)),
	} {
		if _, err = resolve("task", nil, e); !isScopeRefusal(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if s, err = resolve("task", []string{".git/hooks/pre-commit"}, scopeEnvelope([]string{"a.go"})); err != nil || !slices.Equal(s.Files, []string{"a.go"}) {
		t.Error("unsafe widen held or refused", s.Files, err)
	}
	loaded := false
	pinned, err := x.resolveLiveTaskScope("pinned", []string{"extra.go"}, workflow.Stage{}, func() (envelope, error) { loaded = true; return envelope{}, nil })
	if err != nil || !loaded || pinned.Derived || !slices.Equal(pinned.Files, []string{"extra.go", "p.go", "p_test.go"}) || pinned.TestID != "TestP" {
		t.Fatal("override no longer pinned", pinned, err, loaded)
	}
	pinnedMeta := scopeEnvelope(nil)
	pinnedMeta.Admission.ReadOnlyFiles = []string{"locked.md"}
	protectedPinned, err := resolve("pinned", nil, pinnedMeta)
	if err != nil || !slices.Contains(protectedPinned.ReadOnlyFiles, "locked.md") || !slices.Equal(protectedPinned.Files, []string{"p.go", "p_test.go"}) {
		t.Fatal("pinned readonly admission lost", protectedPinned, err)
	}
	pinnedMeta.Job.DenyPaths = []string{"p.go"}
	if _, err = resolve("pinned", nil, pinnedMeta); !isScopeRefusal(err) {
		t.Fatal("pinned deny policy ignored", err)
	}
	x.config.Files, x.config.TestFile, x.config.TestID = []string{"i.go", "i_test.go"}, "i_test.go", "TestI"
	instance, err := resolve("task", nil, scopeEnvelope([]string{"a.go"}))
	if err != nil || instance.Derived || !slices.Equal(instance.Files, []string{"i.go", "i_test.go"}) || instance.TestID != "TestI" {
		t.Fatal("instance files no longer pinned", instance, err)
	}
	// The brief's test file is always one of its files, in every mode.
	if b := instance.briefScope(); b.TestFile != "i_test.go" || !slices.Equal(b.Files, instance.Files) {
		t.Fatal("pinned brief scope", b)
	}
	x.config.Files = []string{"i.go"}
	outside, err := resolve("task", nil, scopeEnvelope(nil))
	if b := outside.briefScope(); err != nil || outside.TestFile != "i_test.go" || b.TestFile != "" || !slices.Equal(b.Files, []string{"i.go"}) {
		t.Fatal("brief test file outside its files", b, err)
	}
	if b := s.briefScope(); b.TestFile != "" || b.Files == nil {
		t.Fatal("derived brief scope before RED", b)
	}
}

// The RED identity comes from the latest RedReport: its test id and expected
// failure, and the single in-scope *_test.go of its declared files. None or
// several such files, an unsafe path or a test id that is not a Go test name
// refuse a dispatch that needs it. After a freeze the frozen RedReport stays
// the identity; a newer one counts only inside a new RED loop.
func TestLiveDerivedRedIdentity(t *testing.T) {
	x := &liveExecutor{}
	doc := defaults.Workflow()
	declared := []string{"answer.go", "answer_test.go", "other_test.go"}
	resolve := func(stage workflow.Stage, e envelope) liveTaskScope {
		t.Helper()
		s, err := x.resolveLiveTaskScope("task", nil, stage, func() (envelope, error) { return e, nil })
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	none := resolve(workflow.Stage{}, scopeEnvelope(declared))
	if none.TestFile != "" || none.TestID != "" || none.ExpectedFailure != "" || !isScopeRefusal(none.needRed()) || none.brokenRed() != nil {
		t.Fatal("identity before any RedReport", none)
	}
	if !isScopeRefusal(none.effectCheck(engine.Dispatch{Config: doc.Stages["run-red"]})) || !isScopeRefusal(none.effectCheck(engine.Dispatch{Config: doc.Stages["freeze-red"]})) || none.effectCheck(engine.Dispatch{Config: doc.Stages["run-suite"]}) != nil {
		t.Fatal("RED-less dispatch checks", none)
	}
	s := resolve(workflow.Stage{}, scopeEnvelope(declared, redEvidence("TestAnswer", "answer.go", "answer_test.go")))
	if s.TestFile != "answer_test.go" || s.TestID != "TestAnswer" || s.ExpectedFailure != "got 0 want 42" || s.needRed() != nil {
		t.Fatal("identity from RedReport", s)
	}
	broken := redEvidence("TestAnswer", "answer_test.go")
	broken.Fields["expected_failure"] = " "
	for name, red := range map[string]workflow.Evidence{
		"two test files":     redEvidence("TestAnswer", "answer_test.go", "other_test.go"),
		"no test file":       redEvidence("TestAnswer", "answer.go"),
		"out-of-scope test":  redEvidence("TestAnswer", "pkg/answer_test.go"),
		"unsafe path":        redEvidence("TestAnswer", "answer_test.go", "../escape_test.go"),
		"metadata path":      redEvidence("TestAnswer", "answer_test.go", ".git/hooks"),
		"duplicate path":     redEvidence("TestAnswer", "answer_test.go", "answer_test.go"),
		"pattern test id":    redEvidence("TestA|TestB", "answer_test.go"),
		"non-Go test id":     redEvidence("test_answer", "answer_test.go"),
		"no expected reason": broken,
	} {
		s := resolve(workflow.Stage{}, scopeEnvelope(declared, red))
		if s.TestFile != "" || s.TestID != "" || !isScopeRefusal(s.needRed()) || !isScopeRefusal(s.brokenRed()) || !isScopeRefusal(s.effectCheck(engine.Dispatch{Config: doc.Stages["run-suite"]})) {
			t.Errorf("%s: %+v", name, s)
		}
	}
	// A RedReport after the freeze: the frozen one holds every check outside
	// a new RED loop (the RED rework's turn, its RED run and its freeze).
	frozen := scopeEnvelope(declared, redEvidence("TestAnswer", "answer_test.go"), workflow.Evidence{Type: "FrozenIdentity", Stage: "freeze-red", Fields: map[string]any{"identity": "x"}}, redEvidence("TestOther", "other_test.go"))
	dispatch := func(stages ...string) envelope {
		e := frozen
		for _, stage := range stages {
			e.Job.History = append(e.Job.History, engine.Event{Kind: "dispatch", Stage: stage})
		}
		return e
	}
	for name, tc := range map[string]struct {
		stage workflow.Stage
		e     envelope
		want  string
	}{
		"implement":      {doc.Stages["implement"], frozen, "TestAnswer"},
		"compare":        {doc.Stages["compare-red"], frozen, "TestAnswer"},
		"landing":        {workflow.Stage{}, frozen, "TestAnswer"},
		"green run":      {doc.Stages["run-tests"], dispatch("test-first", "freeze-red", "implement", "run-tests"), "TestAnswer"},
		"rework turn":    {doc.Stages["test-first"], dispatch("test-first", "freeze-red", "test-first"), "TestOther"},
		"rework RED run": {doc.Stages["run-red"], dispatch("test-first", "freeze-red", "test-first", "run-red"), "TestOther"},
		"rework freeze":  {doc.Stages["freeze-red"], dispatch("test-first", "freeze-red", "test-first", "run-red", "freeze-red"), "TestOther"},
	} {
		s := resolve(tc.stage, tc.e)
		if s.TestID != tc.want || s.TestFile != map[string]string{"TestAnswer": "answer_test.go", "TestOther": "other_test.go"}[tc.want] {
			t.Errorf("%s: got %s in %s, want %s", name, s.TestID, s.TestFile, tc.want)
		}
	}
}

// A test-first turn of a derived scope with no RED file yet may change at
// most one file, in scope and named *_test.go; once a RedReport names the
// RED file, only that file. An implementation is held to the derived frozen
// RED exactly like a pinned one.
func TestLiveDerivedTestFirstCheckChange(t *testing.T) {
	ctx := context.Background()
	root := reviewPhysical(t)
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	liveGit(t, repo, "init")
	if err := os.WriteFile(filepath.Join(repo, "answer.go"), []byte("package sample\n\nfunc answer() int { return 0 }\n"), 0600); err != nil {
		t.Fatal(err)
	}
	liveGit(t, repo, "add", ".")
	liveGit(t, repo, "commit", "-m", "base")
	manager := &worktree.Manager{Repository: repo, Root: filepath.Join(root, "trees")}
	w, err := manager.Ensure(ctx, "run", "task", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	daemon := &Daemon{store: controlplane.New(db, controlplane.Options{}), jobs: map[string]controlplane.JobSnapshot{}}
	x := &liveExecutor{daemon: daemon, manager: manager, broker: &effects.Broker{Manager: manager}}
	setJob := func(e envelope) {
		raw, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		daemon.jobs["task"] = controlplane.JobSnapshot{ID: "task", Payload: raw}
	}
	tree, err := x.broker.Tree(ctx, w)
	if err != nil {
		t.Fatal(err)
	}
	reset := func() {
		liveGit(t, w.Path, "checkout", "--", ".")
		liveGit(t, w.Path, "clean", "-fdq")
	}
	write := func(files map[string]string) {
		for name, body := range files {
			if err := os.WriteFile(filepath.Join(w.Path, name), []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	declared := []string{"answer.go", "answer_test.go", "other_test.go"}
	d := engine.Dispatch{TaskID: "task", Tree: tree, Config: workflow.Stage{Phase: "test-first", ChangeProducer: true}}
	binding := execution.Binding{Worktree: w.Path}
	check := func(files map[string]string) error {
		t.Helper()
		reset()
		write(files)
		return x.checkChange(ctx, d, binding)
	}
	setJob(scopeEnvelope(declared))
	if err = check(map[string]string{"answer_test.go": liveTestSource}); err != nil {
		t.Fatal("first RED refused", err)
	}
	if err = check(map[string]string{"other_test.go": liveTestSource}); err != nil {
		t.Fatal("first RED in another declared test file refused", err)
	}
	if err = check(nil); err != nil {
		t.Fatal("unchanged RED turn refused", err)
	}
	for name, tc := range map[string]struct {
		files map[string]string
		want  string
	}{
		"two test files":     {map[string]string{"answer_test.go": liveTestSource, "other_test.go": liveTestSource}, "changed 2 files"},
		"production file":    {map[string]string{"answer.go": "package sample\n\nfunc answer() int { return 42 }\n"}, "non-test file"},
		"out-of-scope test":  {map[string]string{"extra_test.go": liveTestSource}, "outside declared files"},
		"test and prod file": {map[string]string{"answer_test.go": liveTestSource, "answer.go": "package sample\n"}, "changed 2 files"},
	} {
		if err = check(tc.files); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	setJob(scopeEnvelope(nil))
	if err = check(map[string]string{"answer_test.go": liveTestSource}); !isScopeRefusal(err) {
		t.Fatal("change turn without any declared file", err)
	}
	// Once a RedReport names the RED file, a RED turn may change only it.
	setJob(scopeEnvelope(declared, redEvidence("TestBehavior", "answer_test.go")))
	if err = check(map[string]string{"answer_test.go": liveTestSource}); err != nil {
		t.Fatal("RED file change refused", err)
	}
	if err = check(map[string]string{"other_test.go": liveTestSource}); err == nil || !strings.Contains(err.Error(), "non-test file") {
		t.Fatal("RED turn changed another test file", err)
	}
	// A broken latest RedReport names no RED file: the turn that replaces it
	// is held to the first-RED rule, never refused for the report it replaces.
	setJob(scopeEnvelope(declared, redEvidence("TestBehavior", "answer_test.go", "other_test.go")))
	if err = check(map[string]string{"other_test.go": liveTestSource}); err != nil {
		t.Fatal("turn replacing an ambiguous RedReport refused", err)
	}
	if err = check(map[string]string{"answer_test.go": liveTestSource, "other_test.go": liveTestSource}); err == nil || !strings.Contains(err.Error(), "changed 2 files") {
		t.Fatal("turn replacing an ambiguous RedReport changed two files", err)
	}
	if err = check(map[string]string{"answer.go": "package sample\n"}); err == nil || !strings.Contains(err.Error(), "non-test file") {
		t.Fatal("turn replacing an ambiguous RedReport changed a production file", err)
	}
	// Implementation after the derived RED froze is held to it.
	reset()
	write(map[string]string{"answer_test.go": liveTestSource})
	liveGit(t, w.Path, "add", ".")
	liveGit(t, w.Path, "commit", "-m", "red")
	frozenTree, err := x.broker.Tree(ctx, w)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := workflow.GoTestIdentity([]byte(liveTestSource), []string{"TestBehavior"})
	if err != nil {
		t.Fatal(err)
	}
	frozen := workflow.Evidence{Type: "FrozenIdentity", Stage: "freeze-red", Tree: frozenTree, Fields: map[string]any{"identity": identity}}
	setJob(scopeEnvelope(declared, redEvidence("TestBehavior", "answer_test.go"), frozen))
	d = engine.Dispatch{TaskID: "task", Tree: frozenTree, Config: workflow.Stage{Phase: "implement", ChangeProducer: true}, Inputs: []workflow.Evidence{frozen}}
	mock.ExpectQuery("SELECT payload_digest,payload_size FROM daemon_markers").WithArgs(activeConflictKey("task")).WillReturnError(sql.ErrNoRows)
	if err = check(map[string]string{"answer.go": "package sample\n\nfunc answer() int { return 42 }\n"}); err != nil {
		t.Fatal("implementation refused", err)
	}
	if err = check(map[string]string{"answer_test.go": strings.Replace(liveTestSource, "!= 42", "!= 41", 1)}); err == nil || !strings.Contains(err.Error(), "frozen RED") {
		t.Fatal("implementation altered the derived frozen RED", err)
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// Without a pinned commit_message a task commit names its task: a subject
// from the title, else the first content line, capped at 72 characters, then
// the task and dispatch trailers. A pinned message keeps its form.
func TestLiveCommitMessagePerTask(t *testing.T) {
	long := strings.Repeat("é", 80)
	for _, tc := range []struct {
		pinned string
		a      AdmitParams
		want   string
	}{
		{"Implement verified task", AdmitParams{Title: "ignored"}, "Implement verified task\n\nFae dispatch: d1"},
		{"", AdmitParams{Title: "  Make answer return 42  ", Content: "body"}, "Make answer return 42\n\nFae task: t1\nFae dispatch: d1"},
		{"", AdmitParams{Content: "\n  \n# Fix the answer\nmore detail\n"}, "# Fix the answer\n\nFae task: t1\nFae dispatch: d1"},
		{"", AdmitParams{Title: long}, strings.Repeat("é", 72) + "\n\nFae task: t1\nFae dispatch: d1"},
		{"", AdmitParams{}, "Fae task t1\n\nFae task: t1\nFae dispatch: d1"},
	} {
		got := liveCommitMessage(tc.pinned, tc.a, "t1", "d1")
		if got != tc.want || got != liveCommitMessage(tc.pinned, tc.a, "t1", "d1") {
			t.Errorf("%+v: got %q want %q", tc.a, got, tc.want)
		}
	}
}

// FAE-WF-073: a derived RED's expected failure must name a reason. A fragment
// that is the test id or part of it, the RED file's name or a file:line
// position, go test's framing, a panic's generic text, a bare got/want label
// or a number alone would match a failure of the test whatever its reason, so
// the RED identity refuses it. A number beside a word or a comparison, a
// panic's message and a timeout are reasons.
func TestLiveDerivedRedRefusesVacuousReason(t *testing.T) {
	x := &liveExecutor{}
	declared := []string{"answer.go", "answer_test.go"}
	identity := func(expected string) liveTaskScope {
		t.Helper()
		red := redEvidence("TestAnswer", "answer_test.go")
		red.Fields["expected_failure"] = expected
		s, err := x.resolveLiveTaskScope("task", nil, workflow.Stage{}, func() (envelope, error) { return scopeEnvelope(declared, red), nil })
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	for _, vacuous := range []string{"TestAnswer", "Answer", "testanswer", "Answer:", "FAIL", "--- FAIL: TestAnswer", "--- FAIL: TestAnswer (0.00s)", "=== RUN   TestAnswer", "answer_test.go", "answer_test.go:7:", "answer_test.go:7: ", "ok", "exit status 1",
		"panic", "panic: [recovered]", "[recovered, repanicked]", "goroutine 6 [running]:", "runtime", "testing.tRunner.func1()", "created by testing.(*T).Run in goroutine 1", "/usr/local/go/src/runtime/panic.go:859 +0x125", "example.com/live.TestAnswer(0x3e4d077ba248?)", "/home/u/worktrees/run/task/answer_test.go:17",
		"got", "want", "got want", "want:", "expected:", "actual", "42", "runtime error", "error", "go:7", "o:7", "go:7:"} {
		if s := identity(vacuous); s.TestID != "" || !isScopeRefusal(s.needRed()) {
			t.Errorf("vacuous expected failure %q accepted: %+v", vacuous, s)
		}
	}
	for _, reason := range []string{"got 0 want 42", "want 42", "panic: assignment to entry in nil map", "answer() = 0, want 42", "TestAnswer: got 0 want 42", "answer_test.go:7: got 0 want 42", "0 != 42", "test timed out after 30s", "timed out", "SIGSEGV", "runtime error: integer divide by zero", "nil pointer dereference", "expected error", "answer_test.go:7: expected error"} {
		if s := identity(reason); s.TestID != "TestAnswer" || s.ExpectedFailure != reason || s.needRed() != nil {
			t.Errorf("expected failure %q refused: %v", reason, s.needRed())
		}
	}
}

// FAE-WF-073: the reason rule refuses only what says nothing about why the
// test fails, never a real message because its words also occur in the
// test's name. A name never holds a space, so "divide by zero" can only match
// the failure's message, while "DivideByZero" also matches the panic frame
// that names the test. Numbers beside a word or a comparison and timeouts are
// reasons; a number alone is not. The examples the bridge's output protocol
// quotes (provider.outputProtocolRules) classify as it says. Every line a
// panic's trace prints whatever its cause is refused, its message line is
// not.
func TestLiveDerivedRedReasonAcceptsRealMessages(t *testing.T) {
	for _, tc := range []struct {
		id, fragment string
		vacuous      bool
	}{
		{"TestDivideByZero", "divide by zero", false},
		{"TestDivideByZero", "integer divide by zero", false},
		{"TestDivideByZero", "DivideByZero", true},
		{"TestNilPointer", "nil pointer", false},
		{"TestNilPointer", "NilPointer", true},
		{"TestParseErrorMissingField", "parse error: missing field", false},
		{"TestAnswerReturns42", "returns 42", false},
		{"TestAnswerReturns42", "answer returns 0", false},
		{"TestAnswerReturns42", "42", true},
		{"TestAnswer", "42", true},
		{"TestAnswer", "0 != 42", false},
		{"TestHang", "timed out", false},
		{"TestHang", "test timed out", false},
		// The output protocol's examples.
		{"TestAnswer", "got 0 want 42", false},
		{"TestAnswer", "assignment to entry in nil map", false},
		{"TestAnswer", "FAIL", true},
		{"TestAnswer", "ok", true},
		{"TestAnswer", "exit status 2", true},
		{"TestAnswer", "(0.00s)", true},
		{"TestAnswer", "panic:", true},
		{"TestAnswer", "[recovered]", true},
		{"TestAnswer", "goroutine 7", true},
		{"TestAnswer", "got", true},
		{"TestAnswer", "want:", true},
		{"TestAnswer", "expected", true},
		{"TestAnswer", "actual", true},
		{"TestAnswer", "answer_test.go:12", true},
		{"TestAnswer", "Answer", true},
	} {
		if got := liveRedReasonVacuous(tc.fragment, tc.id, "pkg/answer_test.go"); got != tc.vacuous {
			t.Errorf("%s %q: vacuous %v, want %v", tc.id, tc.fragment, got, tc.vacuous)
		}
	}
	// go test -json output of TestAnswer writing to a nil map (Go 1.27), its
	// framing lines removed as the broker removes them.
	trace := `panic: assignment to entry in nil map [recovered, repanicked]

goroutine 6 [running]:
testing.tRunner.func1.2({0x6b5b30, 0x564600})
	/usr/local/go/src/testing/testing.go:2125 +0x232
testing.tRunner.func1()
	/usr/local/go/src/testing/testing.go:2128 +0x329
panic({0x6b5b30?, 0x564600?})
	/usr/local/go/src/runtime/panic.go:859 +0x125
example.com/live/pkg.TestAnswer(0x3ba0fce58248?)
	/home/u/fae-self/worktrees/run/task/pkg/answer_test.go:17 +0x25
testing.tRunner(0x3ba0fce58248, 0x6d6458)
	/usr/local/go/src/testing/testing.go:2195 +0xea
created by testing.(*T).Run in goroutine 1
	/usr/local/go/src/testing/testing.go:2260 +0x4d4`
	for i, line := range strings.Split(trace, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if vacuous := liveRedReasonVacuous(strings.TrimSpace(line), "TestAnswer", "pkg/answer_test.go"); vacuous != (i > 0) {
			t.Errorf("trace line %q: vacuous %v", line, vacuous)
		}
	}
}

// FAE-WF-073, re-audit 8's and 9's probes: the fragments that matched
// failures of one test for unrelated reasons are refused, and the legitimate
// ones refused are accepted. The broker matches the test's failure text,
// without the goroutine stacks in it (effects
// TestRedFailureTextExcludesGoroutineDump runs the same failures for real):
// what is left there for any failure is the test's name, the RED file's name
// and line numbers, a panic's generic words, the bare labels and numbers. So
// a number alone, bare hex, error alone, runtime error and any end of a
// file:line position (go:9) are refused, while expected error, words of the
// module's or worktree's path, of the RED file's directories and of a
// path-only message, found only in a stack or in the message itself, are
// accepted. A word of the RED file's own name stays refused: every logged
// line's prefix prints it ("config" for config_test.go).
func TestLiveRedReasonRuleProbeFragments(t *testing.T) {
	const id, file = "TestAnswerReturns42", "pkg/answer_test.go"
	for _, tc := range []struct {
		fragment string
		file     string
		vacuous  bool
	}{
		// Numbers and hex alone: positions, goroutine ids, addresses.
		{"1", file, true},
		{"0", file, true},
		{"2", file, true},
		{"42", file, true},
		{"0x", file, true},
		{"+0x", file, true},
		{"0xc", file, true},
		{"0xea", file, true},
		{"0.00s", file, true},
		{"[3]", file, true},
		// A runtime panic's generic words. "error" alone lies within
		// "runtime error"; next to a word that isn't a trace word it is the
		// message of t.Error("expected error") and the like (re-audit 9).
		{"error", file, true},
		{"Error:", file, true},
		{"time error", file, true},
		{"runtime error", file, true},
		{"panic: runtime error", file, true},
		{"runtime error 42", file, true},
		{"running tests:", file, true},
		{"expected error", file, false},
		{"want error", file, false},
		{"got error", file, false},
		{"unexpected error: EOF", file, false},
		{"err_test.go:9: expected error", "pkg/err_test.go", false},
		{"go:9: expected error", file, false},
		// A source position or any end of one matches every position on its
		// line, whatever the failure (re-audit 9: go:9 matched).
		{"go:9", file, true},
		{"go:9:", file, true},
		{"go:9:5", file, true},
		{"o:12", file, true},
		{":12", file, true},
		{"want :8080", file, false},
		{"info:12 missing", file, false},
		{".go:12", file, true},
		{"t.go:12", file, true},
		{"helper.go:12", file, true},
		// The dump's frames and positions.
		{"(...)", file, true},
		{"Lookup(...)", file, true},
		{"example.com/live/pkg.Lookup(...)", file, true},
		{"testing.tRunner(0x3ba0fce58248, 0x6d6458)", file, true},
		{"answer.go:7", file, true},
		{"pkg.TestAnswerReturns42", file, true},
		{"/tmp/x/sample/pkg/answer_test.go:17 +0x21c", file, true},
		// The RED file's own name, printed on every logged line.
		{"answer_test", file, true},
		{"config", "internal/config/config_test.go", true},
		{"EOF", "reader/eof_test.go", true},
		// Words of paths only the dump prints for any failure.
		{"example.com", file, false},
		{"example.com/live/pkg", file, false},
		{"tmp", file, false},
		{"worktrees", file, false},
		{"pkg", file, false},
		{"overflow", "internal/overflow/parse_test.go", false},
		{"config", "internal/config/load_test.go", false},
		{"EOF", "internal/eof/read_test.go", false},
		// Path-only and slashed messages.
		{"testdata/missing.json", file, false},
		{"open testdata/missing.json: no such file", file, false},
		{"want application/json", file, false},
		{"got text/plain want application/json", file, false},
		{"want https://example.com/login", file, false},
		// What wave 9 accepted for good reason.
		{"0 != 42", file, false},
		{"42 > 0", file, false},
		{"answer() = 0, want 42", file, false},
		{"= 0, want 42", file, false},
		{"got 0", file, false},
		{"want 42", file, false},
		{"returns 42", file, false},
		{"answer returns 0", file, false},
		{"nil pointer dereference", file, false},
		{"invalid memory address or nil pointer dereference", file, false},
		{"index out of range", file, false},
		{"index out of range [3] with length 2", file, false},
		{"assignment to entry in nil map", file, false},
		{"SIGSEGV", file, false},
		{"addr=0x0", file, false},
		{"test timed out", file, false},
		{"panic: test timed out after 5m0s", file, false},
		{"no such file", file, false},
	} {
		if got := liveRedReasonVacuous(tc.fragment, id, tc.file); got != tc.vacuous {
			t.Errorf("%q (RED file %s): vacuous %v, want %v", tc.fragment, tc.file, got, tc.vacuous)
		}
	}
	if !liveRedReasonVacuous("divide", "TestDivideByZero", "") || liveRedReasonVacuous("divide by zero", "TestDivideByZero", "") {
		t.Error("the test's name and a message made of its words")
	}
}

// One path rule refuses repository metadata at any depth, in any letter
// case, wherever a task's files come from: admission, a Spec, a RedReport and
// a pinned tasks override. A stored approved widen holding such a path is
// left out of the scope instead of refusing it
// (TestLiveResolverLeavesOutRefusedApprovedPaths). Files named like .git are
// still files.
func TestDeclaredFilesRefuseNestedGitMetadata(t *testing.T) {
	x := &liveExecutor{}
	resolve := func(approved []string, e envelope) (liveTaskScope, error) {
		return x.resolveLiveTaskScope("task", approved, workflow.Stage{}, func() (envelope, error) { return e, nil })
	}
	for _, name := range []string{".git", ".git/config", "sub/.git/config", "sub/.git", "a/b/.git/hooks/pre-commit", ".GIT/config", "sub/.Git/HEAD"} {
		if err := validateStructuredFields(AdmitParams{DeclaredFiles: []string{"a.go", name}}); err == nil {
			t.Errorf("admission accepted %q", name)
		}
		for source, e := range map[string]envelope{"admission": scopeEnvelope([]string{"a.go", name}), "spec": scopeEnvelope(nil, specEvidence("a.go", name))} {
			if _, err := resolve(nil, e); !isScopeRefusal(err) {
				t.Errorf("%s files accepted %q: %v", source, name, err)
			}
		}
		if s, err := resolve([]string{name}, scopeEnvelope([]string{"a.go"})); err != nil || slices.Contains(s.Files, name) {
			t.Errorf("approved widen %q held or refused: %v %v", name, s.Files, err)
		}
		if s, err := resolve(nil, scopeEnvelope([]string{"a.go", "a_test.go"}, redEvidence("TestA", "a_test.go", name))); err != nil || !isScopeRefusal(s.needRed()) {
			t.Errorf("RedReport path accepted %q: %v", name, err)
		}
		if err := validateLiveTasks(map[string]LiveTaskConfig{"t": {Files: []string{"a_test.go", name}, TestFile: "a_test.go", TestID: "TestA", ExpectedFailure: "want"}}); err == nil {
			t.Errorf("tasks override accepted %q", name)
		}
	}
	files := []string{"a.go", ".gitignore", "sub/.gitattributes", ".github/workflows/test.yml", "sub/.gitkeep", "x.git/y.go"}
	if err := validateStructuredFields(AdmitParams{DeclaredFiles: files}); err != nil {
		t.Fatal(err)
	}
	if s, err := resolve(nil, scopeEnvelope(files)); err != nil || len(s.Files) != len(files) {
		t.Fatal("ordinary dot files refused", s.Files, err)
	}
}

// A test-first turn launches over a broken latest RedReport: it writes the
// report that replaces it (Appendix C step 7), so the broken one never
// refuses it. A turn retried because its RedReport was refused is told why in
// its operator context; a later transition ends that note. A turn that needs
// the RED identity is still refused.
func TestLiveDerivedTestFirstLaunchesOverBrokenRedReport(t *testing.T) {
	ctx := context.Background()
	root := reviewPhysical(t)
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	liveGit(t, repo, "init")
	if err := os.WriteFile(filepath.Join(repo, "answer.go"), []byte("package sample\n\nfunc answer() int { return 0 }\n"), 0600); err != nil {
		t.Fatal(err)
	}
	liveGit(t, repo, "add", ".")
	liveGit(t, repo, "commit", "-m", "base")
	base := liveGit(t, repo, "rev-parse", "HEAD")
	manager := &worktree.Manager{Repository: repo, Root: filepath.Join(root, "trees")}
	if _, err := manager.Ensure(ctx, "run", "task", base); err != nil {
		t.Fatal(err)
	}
	daemon := &Daemon{jobs: map[string]controlplane.JobSnapshot{}}
	x := &liveExecutor{daemon: daemon, manager: manager, broker: &effects.Broker{Manager: manager}}
	doc := defaults.Workflow()
	refusal := "ValidationFailed: task scope refused: RedReport refused at turn completion: RedReport declares 0 in-scope *_test.go files []; the RED file must be exactly one"
	e := scopeEnvelope([]string{"answer.go", "answer_test.go", "other_test.go"}, redEvidence("TestBehavior", "answer_test.go", "other_test.go"))
	e.Admission.RunID, e.Job.Base = "run", base
	e.Job.History = []engine.Event{{Kind: "transition", Stage: "plan"}, {Kind: "execution-diagnostic", Stage: "test-first"}, {Kind: "failure", Stage: "test-first", Cause: refusal}, {Kind: "dispatch", Stage: "test-first"}}
	resolve := func(stage string) (execution.Binding, error) {
		t.Helper()
		raw, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		daemon.jobs["task"] = controlplane.JobSnapshot{ID: "task", Payload: raw}
		return x.resolve(ctx, engine.Dispatch{TaskID: "task", Stage: stage, Config: doc.Stages[stage]})
	}
	b, err := resolve("test-first")
	if err != nil {
		t.Fatal("test-first refused over a broken RedReport", err)
	}
	if b.Scope.TestFile != "" || !strings.Contains(b.OperatorContext, "RedReport was refused") || !strings.Contains(b.OperatorContext, refusal) {
		t.Fatalf("retried test-first binding: test file %q, operator context %q", b.Scope.TestFile, b.OperatorContext)
	}
	if _, err = resolve("implement"); !isScopeRefusal(err) {
		t.Fatal("implement launched without a RED identity", err)
	}
	e.Job.History = append(e.Job.History, engine.Event{Kind: "transition", Stage: "test-first"}, engine.Event{Kind: "dispatch", Stage: "test-first"})
	if b, err = resolve("test-first"); err != nil || strings.Contains(b.OperatorContext, "RedReport was refused") {
		t.Fatalf("note after a transition: %q %v", b.OperatorContext, err)
	}
}

// FAE-WF-078 with WF-073: a scope widen approved after the RED froze that
// adds a test file the frozen RedReport also lists leaves the RED file where
// it froze, so implement and every later stage keep the frozen identity and
// the widened file is in scope. A new RED loop is still held to the strict
// rule (its test-first turn replaces the report), a RED file that itself came
// in with a widen stays ambiguous, and without a freeze nothing changes.
func TestLiveFrozenRedSurvivesWidenOfListedTestFile(t *testing.T) {
	x := &liveExecutor{}
	doc := defaults.Workflow()
	frozen := workflow.Evidence{Type: "FrozenIdentity", Stage: "freeze-red", Fields: map[string]any{"identity": "frozen"}}
	red := redEvidence("TestAnswer", "answer.go", "answer_test.go", "other_test.go")
	resolve := func(e envelope, approved []string, stage string) liveTaskScope {
		t.Helper()
		s, err := x.resolveLiveTaskScope("task", approved, doc.Stages[stage], func() (envelope, error) { return e, nil })
		if err != nil {
			t.Fatal(stage, err)
		}
		return s
	}
	e := scopeEnvelope([]string{"answer.go", "answer_test.go"}, red, frozen)
	for _, stage := range []string{"implement", "run-tests", "compare-red", "run-suite", "merge", "reviews", "oracle"} {
		s := resolve(e, []string{"other_test.go"}, stage)
		if s.TestFile != "answer_test.go" || s.TestID != "TestAnswer" || s.needRed() != nil || s.brokenRed() != nil || !slices.Contains(s.Files, "other_test.go") {
			t.Errorf("%s after the widen: file %q id %q files %v refusal %v", stage, s.TestFile, s.TestID, s.Files, s.needRed())
		}
	}
	if s := resolve(e, []string{"other_test.go"}, "test-first"); s.TestFile != "" || !isScopeRefusal(s.needRed()) {
		t.Errorf("a new RED loop resolved the ambiguous report: %+v", s)
	}
	viaWiden := scopeEnvelope([]string{"answer.go"}, red, frozen)
	if s := resolve(viaWiden, []string{"answer_test.go", "other_test.go"}, "implement"); s.TestFile != "" || !isScopeRefusal(s.needRed()) {
		t.Errorf("a RED file that came with a widen was disambiguated: %+v", s)
	}
	unfrozen := scopeEnvelope([]string{"answer.go", "answer_test.go"}, red)
	if s := resolve(unfrozen, []string{"other_test.go"}, "implement"); s.TestFile != "" || !isScopeRefusal(s.needRed()) {
		t.Errorf("an unfrozen report was disambiguated: %+v", s)
	}
}

// A frozen RedReport's expected failure is not judged again: the RED gate
// proved it and no later gate reads it, so a task frozen under an earlier
// reason rule keeps its RED after an upgrade. Before the freeze, and inside a
// new RED loop, the rule holds.
func TestLiveFrozenRedReasonIsNotRejudged(t *testing.T) {
	x := &liveExecutor{}
	doc := defaults.Workflow()
	frozen := workflow.Evidence{Type: "FrozenIdentity", Stage: "freeze-red", Fields: map[string]any{"identity": "frozen"}}
	resolve := func(stage string, evidence ...workflow.Evidence) liveTaskScope {
		t.Helper()
		s, err := x.resolveLiveTaskScope("task", nil, doc.Stages[stage], func() (envelope, error) {
			return scopeEnvelope([]string{"answer.go", "answer_test.go"}, evidence...), nil
		})
		if err != nil {
			t.Fatal(stage, err)
		}
		return s
	}
	for _, reason := range []string{"FAIL", "want"} {
		red := redEvidence("TestAnswer", "answer.go", "answer_test.go")
		red.Fields["expected_failure"] = reason
		for _, stage := range []string{"implement", "run-tests", "compare-red", "merge"} {
			if s := resolve(stage, red, frozen); s.TestFile != "answer_test.go" || s.ExpectedFailure != reason || s.needRed() != nil {
				t.Errorf("frozen %q at %s: %+v %v", reason, stage, s, s.needRed())
			}
		}
		if s := resolve("test-first", red, frozen); !isScopeRefusal(s.needRed()) {
			t.Errorf("a new RED loop accepted %q", reason)
		}
		if s := resolve("implement", red); !isScopeRefusal(s.needRed()) {
			t.Errorf("an unfrozen report kept %q", reason)
		}
	}
}
