package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/providerhealth"
	"github.com/xleelz/fae/runtime"
	"github.com/xleelz/fae/workflow"
)

// Spelled out so the tests pin the wire values of
// engine.ClassLaterReviewerNotStarted and runtime.ProviderFailed.
const (
	laterReviewerNotStartedClass = "later-reviewer-not-started"
	providerFailedClass          = "provider-failed"
)

// sessionScriptRuntime answers each launch with the next scripted result.
type sessionScriptRuntime struct {
	calls   int
	results []func() (runtime.Collected, error)
}

func (s *sessionScriptRuntime) Run(context.Context, runtime.Launch) (runtime.Collected, error) {
	f := s.results[s.calls]
	s.calls++
	return f()
}

// laterReviewerDeferral is a runtime's typed no-launch admission deferral.
type laterReviewerDeferral struct{}

func (laterReviewerDeferral) Error() string           { return "admission deferred" }
func (laterReviewerDeferral) AdmissionDeferred() bool { return true }

// FAE-RT-004, FAE-WF-033: a review dispatch whose second session provably
// never started (the transport's non-execution proof, or the bridge's
// provider-not-started), with its processes retired and the tree unchanged,
// is the typed later-reviewer start failure carrying the review that ran,
// not an uncertain dispatch. No health is observed for the session that never
// started. A moved tree or unproven retirement keeps it uncertain. A runtime
// admission deferral of a later session is a deferral carrying the review
// that ran, which the engine parks without a charge (FAE-HOST-006, FAE-RT-004).
func TestLaterReviewerStartFailureIsTypedNotUncertain(t *testing.T) {
	ran := func() (runtime.Collected, error) {
		return runtime.Collected{ProcessesRetired: true, Output: []byte(`{"pass":true}`)}, nil
	}
	notStarted := &runtime.TurnError{Class: runtime.ProviderNotStarted, Role: "reviewer", DispatchID: "dispatch", Cause: &runtime.BridgeRefusal{Class: runtime.ProviderNotStarted, Detail: "provider exited before acknowledging the prompt"}}
	for _, tc := range []struct {
		name, want string
		second     func() (runtime.Collected, error)
		changed    bool
	}{
		{"transport never released the provider", laterReviewerNotStartedClass, func() (runtime.Collected, error) {
			return runtime.Collected{NonExecution: true, ProcessesRetired: true}, &runtime.TurnError{Class: "startup-not-executed", Cause: errors.New("pane never hosted the agent")}
		}, false},
		{"provider never acknowledged", laterReviewerNotStartedClass, func() (runtime.Collected, error) {
			return runtime.Collected{ProcessesRetired: true, LogDirectory: t.TempDir()}, notStarted
		}, false},
		{"provider never acknowledged, tree moved", "uncertain", func() (runtime.Collected, error) {
			return runtime.Collected{ProcessesRetired: true, LogDirectory: t.TempDir()}, notStarted
		}, true},
		{"provider never acknowledged, retirement unproven", "uncertain", func() (runtime.Collected, error) {
			return runtime.Collected{LogDirectory: t.TempDir()}, notStarted
		}, false},
		{"runtime admission deferral", "infrastructure-deferred", func() (runtime.Collected, error) {
			return runtime.Collected{NonExecution: true, ProcessesRetired: true}, laterReviewerDeferral{}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x, d, a := fixture(t)
			d.Config.Primitive = workflow.Review
			d.Sessions = append(d.Sessions, engine.Session{ID: "two", Role: "reviewer"})
			x.Runtime = &sessionScriptRuntime{results: []func() (runtime.Collected, error){ran, tc.second}}
			trees := 0
			x.Tree = func(context.Context, Binding) (string, error) {
				// The dispatch's measure, the first session's, then the
				// failed second session's.
				if trees++; tc.changed && trees > 2 {
					return "changed", nil
				}
				return "tree", nil
			}
			var observed []providerhealth.Outcome
			x.Observe = func(_ context.Context, _ engine.Dispatch, s engine.Session, o providerhealth.Outcome) error {
				observed = append(observed, o)
				return nil
			}
			r, e := x.Execute(context.Background(), d)
			if e == nil || r.Class != tc.want || a.released != 2 {
				t.Fatalf("class %q err %v released %d", r.Class, e, a.released)
			}
			if fmt.Sprint(observed) != fmt.Sprint([]providerhealth.Outcome{providerhealth.OK}) {
				t.Fatalf("observed %v: the session that never started was charged", observed)
			}
			if tc.want != "uncertain" && (len(r.Reviews) != 1 || r.Reviews[0].Session != "one" || len(r.Output) != 0 || r.NewTree != "") {
				t.Fatalf("result %+v: want the first review only", r)
			}
		})
	}
}

// FAE-WF-034: the provider-failed class is a bounded retry only with its
// health recorded and a profile that grants no effect beyond edits through
// the bridge; otherwise it stays uncertain.
func TestProviderFailedNeedsObservationAndRestrictedProfile(t *testing.T) {
	failed := &runtime.TurnError{Class: providerFailedClass, Role: "coder", DispatchID: "dispatch", Cause: &runtime.BridgeRefusal{Class: providerFailedClass, Detail: "provider exited with a failure after acknowledging the prompt: exit status 3"}}
	for _, tc := range []struct {
		name, want   string
		allowed      []string
		observeError bool
	}{
		{"restricted and observed", engine.ClassMalformedOutput, []string{"handoff", "read", "edit"}, false},
		{"observation failed", "uncertain", []string{"handoff", "read", "edit"}, true},
		{"profile grants exec", "uncertain", []string{"handoff", "exec"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x, d, a := fixture(t)
			resolve := x.Resolve
			x.Resolve = func(ctx context.Context, d engine.Dispatch) (Binding, error) {
				b, e := resolve(ctx, d)
				role := b.Document.Roles["coder"]
				role.Profile = workflow.Profile{Allowed: tc.allowed}
				b.Document.Roles["coder"] = role
				return b, e
			}
			x.Runtime = refusingRuntime{dir: t.TempDir(), err: failed, retired: true}
			x.Observe = func(context.Context, engine.Dispatch, engine.Session, providerhealth.Outcome) error {
				if tc.observeError {
					return errors.New("commit unknown")
				}
				return nil
			}
			r, e := x.Execute(context.Background(), d)
			if e == nil || r.Class != tc.want || a.released != 1 {
				t.Fatalf("class %q err %v released %d", r.Class, e, a.released)
			}
			// The bridge's own detail, with no per-turn path, is the reason.
			if tc.want == engine.ClassMalformedOutput && (e.Error() != failed.Cause.Error() || !strings.Contains(r.Detail, "exit status 3")) {
				t.Fatalf("reason %q detail %q", e, r.Detail)
			}
		})
	}
}

// A provider-failed turn whose log holds a structured rate limit is charged
// rate-limited and, with the tree unchanged, fails over to the next route
// (FAE-RT-010), as any launched failure with that log does.
func TestProviderFailedWithARateLimitFailsOver(t *testing.T) {
	x, d, a := fixture(t)
	logs := t.TempDir()
	if e := os.WriteFile(filepath.Join(logs, "events.jsonl"), []byte(`{"type":"error","error":{"code":"rate_limit","message":"HTTP 429"}}`+"\n"), 0600); e != nil {
		t.Fatal(e)
	}
	x.Runtime = refusingRuntime{dir: logs, retired: true, err: &runtime.TurnError{Class: providerFailedClass, Cause: &runtime.BridgeRefusal{Class: providerFailedClass, Detail: "provider exited with a failure after acknowledging the prompt: exit status 1"}}}
	var observed []providerhealth.Outcome
	x.Observe = func(_ context.Context, _ engine.Dispatch, _ engine.Session, o providerhealth.Outcome) error {
		observed = append(observed, o)
		return nil
	}
	r, e := x.Execute(context.Background(), d)
	if e == nil || r.Class != engine.ClassProviderFailover || fmt.Sprint(observed) != fmt.Sprint([]providerhealth.Outcome{providerhealth.RateLimited}) || a.released != 1 {
		t.Fatalf("class %q err %v observed %v", r.Class, e, observed)
	}
}

// startFailureChain is an executor over Direct and the production Claude
// bridge (TestExecutionProviderBridge) with a scripted CLI, a git worktree
// holding answer.go, and recorded health observations.
type startFailureChain struct {
	x                      *Executor
	a                      *admission
	observed               *[]providerhealth.Outcome
	worktree, answer, mark string
}

func newStartFailureChain(t *testing.T, script string, allowed []string) startFailureChain {
	t.Helper()
	worktree, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	root, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	git := exec.Command("git", "init", "-q", worktree)
	git.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	if out, e := git.CombinedOutput(); e != nil {
		t.Fatal(e, string(out))
	}
	answerGo := filepath.Join(worktree, "answer.go")
	if e = os.WriteFile(answerGo, []byte("original"), 0600); e != nil {
		t.Fatal(e)
	}
	scratch := t.TempDir()
	cli, answer, mark := filepath.Join(scratch, "claude"), filepath.Join(scratch, "answer.json"), filepath.Join(scratch, "ran")
	if e = os.WriteFile(cli, []byte(script), 0700); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(answer, []byte(`{"output":{"accepted":true},"edits":[]}`), 0600); e != nil {
		t.Fatal(e)
	}
	exe, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	route := workflow.Binding{Provider: "claude", Model: "fixture-model"}
	role := workflow.Role{Instructions: "work", Routes: []workflow.Binding{route}, Profile: workflow.Profile{Allowed: allowed}}
	environment := map[string]string{"EXECUTION_PROVIDER_BRIDGE": "1", "EXECUTION_PROVIDER_CLI": cli, "EXECUTION_PROVIDER_ANSWER": answer, "EXECUTION_PROVIDER_MARK": mark, "EXECUTION_PROVIDER_WORKTREE_FILE": answerGo, "CLAUDE_CODE_OAUTH_TOKEN": "fixture-token", "GORACE": "atexit_sleep_ms=0"}
	document := workflow.Document{Roles: map[string]workflow.Role{"coder": role, "reviewer": role}, Artifacts: map[string]workflow.ArtifactType{"output": {Schema: json.RawMessage(`{"type":"object","required":["accepted"],"additionalProperties":false,"properties":{"accepted":{"type":"boolean"}}}`)}}}
	c := startFailureChain{a: &admission{}, observed: &[]providerhealth.Outcome{}, worktree: worktree, answer: answerGo, mark: mark}
	c.x = &Executor{
		Runtime: &runtime.Direct{Root: root, Command: []string{exe, "-test.run=^TestExecutionProviderBridge$", "--", "{config}"}, Environment: environment},
		Resolve: func(context.Context, engine.Dispatch) (Binding, error) {
			return Binding{Worktree: worktree, Mode: "local", Document: document, Scope: TaskScope{Files: []string{"answer.go"}}}, nil
		},
		Tree: func(context.Context, Binding) (string, error) {
			data, e := os.ReadFile(answerGo)
			return "tree:" + string(data), e
		},
		Admission:   c.a,
		CheckChange: func(context.Context, engine.Dispatch, Binding) error { return nil },
		Observe: func(_ context.Context, _ engine.Dispatch, _ engine.Session, o providerhealth.Outcome) error {
			*c.observed = append(*c.observed, o)
			return nil
		},
	}
	return c
}

// claudeFailingCLI announces a Claude Code session with the given tools,
// optionally writes into the task's worktree, and exits 3 with no result.
func claudeFailingCLI(tools string, writes bool) string {
	write := ""
	if writes {
		write = "printf 'landed' > \"$EXECUTION_PROVIDER_WORKTREE_FILE\"\n"
	}
	return "#!/bin/sh\ncat > /dev/null\ntouch \"$EXECUTION_PROVIDER_MARK\"\n" +
		`printf '{"type":"system","subtype":"init","session_id":"12345678-1234-4234-8234-123456789abc","tools":` + tools + `,"mcp_servers":[]}\n'` + "\n" +
		write + "echo 'fatal: stream closed' >&2\nexit 3\n"
}

// FAE-WF-034 through the shipped chain (Executor, Direct, the production
// Claude bridge and its exit status): a provider CLI that announced its
// session and then exited with a failure and no structured error is charged
// malformed-output health and fails the turn as malformed output, a bounded
// stage retry, with the task untouched. If the provider wrote into the task's
// worktree, or announced a tool that could write, it stays uncertain.
func TestProviderFailedWithoutAnswerThroughDirect(t *testing.T) {
	for _, tc := range []struct {
		name, tools, want string
		writes            bool
	}{
		{"failed after its init record", `["StructuredOutput"]`, engine.ClassMalformedOutput, false},
		{"failed after writing the task", `["StructuredOutput"]`, "uncertain", true},
		{"failed with a command tool announced", `["StructuredOutput","Bash"]`, "uncertain", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newStartFailureChain(t, claudeFailingCLI(tc.tools, tc.writes), []string{"handoff", "read", "edit"})
			d := engine.Dispatch{ExecutionKind: "live", ID: "dispatch", TaskID: "task", Stage: "implement", Tree: "tree:original", Config: workflow.Stage{Primitive: workflow.AgentTurn, Output: "output", Role: "coder", ChangeProducer: true}, Sessions: []engine.Session{{ID: "one", Role: "coder", Route: workflow.Binding{Provider: "claude", Model: "fixture-model"}}}}
			r, e := c.x.Execute(context.Background(), d)
			if e == nil || r.Class != tc.want || r.NewTree != "" {
				t.Fatalf("class %q detail %q err %v", r.Class, r.Detail, e)
			}
			if tc.want == engine.ClassMalformedOutput && (!strings.Contains(r.Detail, "the provider failed without an answer, and the bridge wrote nothing: provider exited with a failure after acknowledging the prompt: exit status 3") || strings.Contains(e.Error(), c.worktree)) {
				t.Fatalf("detail %q err %q", r.Detail, e)
			}
			if fmt.Sprint(*c.observed) != fmt.Sprint([]providerhealth.Outcome{providerhealth.MalformedOutput}) || c.a.released != 1 {
				t.Fatalf("observed %v released %d", *c.observed, c.a.released)
			}
			if _, e := os.Stat(c.mark); e != nil {
				t.Fatal("provider did not run", e)
			}
			if data, _ := os.ReadFile(c.answer); !tc.writes && string(data) != "original" {
				t.Fatal("the turn changed the task", string(data))
			}
		})
	}
}

// FAE-RT-004, FAE-WF-033 through the shipped chain: a review dispatch whose
// first reviewer answers and whose second reviewer's CLI exits before
// acknowledging the prompt is the later-reviewer start failure carrying the
// first review, not an uncertain dispatch; only the reviewer that ran is
// observed.
func TestLaterReviewerNeverAcknowledgedThroughDirect(t *testing.T) {
	script := `#!/bin/sh
cat > /dev/null
if [ -e "$EXECUTION_PROVIDER_MARK" ]; then echo 'fatal: no session' >&2; exit 1; fi
touch "$EXECUTION_PROVIDER_MARK"
sid=12345678-1234-4234-8234-123456789abc
printf '{"type":"system","subtype":"init","session_id":"%s","tools":["StructuredOutput"],"mcp_servers":[]}\n' "$sid"
printf '{"type":"result","subtype":"success","is_error":false,"session_id":"%s","structured_output":%s}\n' "$sid" "$(cat "$EXECUTION_PROVIDER_ANSWER")"
`
	c := newStartFailureChain(t, script, []string{"handoff", "read"})
	route := workflow.Binding{Provider: "claude", Model: "fixture-model"}
	d := engine.Dispatch{ExecutionKind: "live", ID: "dispatch", TaskID: "task", Stage: "reviews", Tree: "tree:original", Config: workflow.Stage{Primitive: workflow.Review, Output: "output", Role: "reviewer"}, Sessions: []engine.Session{{ID: "one", Role: "reviewer", Route: route}, {ID: "two", Role: "reviewer", Route: route}}}
	r, e := c.x.Execute(context.Background(), d)
	if e == nil || r.Class != laterReviewerNotStartedClass || !strings.Contains(r.Detail, "the provider never acknowledged the prompt: provider exited before acknowledging the prompt") {
		t.Fatalf("class %q detail %q err %v", r.Class, r.Detail, e)
	}
	if len(r.Reviews) != 1 || r.Reviews[0].Session != "one" || string(r.Reviews[0].Output) != `{"accepted":true}` {
		t.Fatalf("reviews %+v", r.Reviews)
	}
	if fmt.Sprint(*c.observed) != fmt.Sprint([]providerhealth.Outcome{providerhealth.OK}) || c.a.released != 2 {
		t.Fatalf("observed %v released %d", *c.observed, c.a.released)
	}
}
