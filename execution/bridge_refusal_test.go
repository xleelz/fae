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
	"time"

	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/provider"
	"github.com/xleelz/fae/providerhealth"
	"github.com/xleelz/fae/runtime"
	"github.com/xleelz/fae/workflow"
)

type refusingRuntime struct {
	dir     string
	err     error
	retired bool
}

func (r refusingRuntime) Run(context.Context, runtime.Launch) (runtime.Collected, error) {
	return runtime.Collected{LogDirectory: r.dir, ProcessesRetired: r.retired}, r.err
}

// FAE-WF-034, FAE-RT-009, FAE-RT-010: the bridge's typed refusals are classed
// by where the turn stopped. A refused provider answer is malformed output
// charged to the binding; a refusal before the provider ran is refused before
// launch and charges nothing (FAE-RT-004); a refused binding is binding-refused, names the
// session's binding for the engine to fail over from, and charges nothing.
// Each is rejected-change if the tree moved, and uncertain without proven
// retirement; an untyped bridge failure stays uncertain. A provider that never
// acknowledged the prompt is a startup retry charging nothing (FAE-RT-004),
// and uncertain if the tree moved or retirement is unproven. A provider that
// failed without an answer, the bridge having written nothing, is charged its
// health and is malformed output (FAE-WF-034); it stays uncertain if the tree
// moved (edits may have landed) or retirement is unproven.
func TestBridgeRefusalClassing(t *testing.T) {
	refusal := func(class string) error {
		return &runtime.TurnError{Class: class, Role: "coder", DispatchID: "dispatch", Cause: &runtime.BridgeRefusal{Class: class, Detail: "edit \"answer.go\" replacements[0]: old text not found"}}
	}
	cases := []struct {
		name, want string
		err        error
		retired    bool
		changed    bool
		observed   []providerhealth.Outcome
	}{
		{"output refused", engine.ClassMalformedOutput, refusal(runtime.RefusedOutput), true, false, []providerhealth.Outcome{providerhealth.MalformedOutput}},
		{"output refused, tree moved", engine.ClassRejectedChange, refusal(runtime.RefusedOutput), true, true, []providerhealth.Outcome{providerhealth.MalformedOutput}},
		{"output refused, retirement unproven", "uncertain", refusal(runtime.RefusedOutput), false, false, []providerhealth.Outcome{providerhealth.MalformedOutput}},
		{"refused before the provider", engine.ClassRefusedBeforeLaunch, refusal(runtime.RefusedBeforeProvider), true, false, nil},
		{"refused before the provider, tree moved", engine.ClassRejectedChange, refusal(runtime.RefusedBeforeProvider), true, true, nil},
		{"refused before the provider, retirement unproven", "uncertain", refusal(runtime.RefusedBeforeProvider), false, false, nil},
		{"binding refused", engine.ClassBindingRefused, refusal(runtime.RefusedBinding), true, false, nil},
		{"binding refused, tree moved", engine.ClassRejectedChange, refusal(runtime.RefusedBinding), true, true, nil},
		{"binding refused, retirement unproven", "uncertain", refusal(runtime.RefusedBinding), false, false, nil},
		{"untyped bridge failure", "uncertain", &runtime.TurnError{Class: "process-failed", Cause: errors.New("exit status 1")}, true, false, []providerhealth.Outcome{providerhealth.MalformedOutput}},
		{"provider never acknowledged", "startup-retry", refusal(runtime.ProviderNotStarted), true, false, nil},
		{"provider never acknowledged, tree moved", "uncertain", refusal(runtime.ProviderNotStarted), true, true, nil},
		{"provider never acknowledged, retirement unproven", "uncertain", refusal(runtime.ProviderNotStarted), false, false, nil},
		{"provider failed without an answer", engine.ClassMalformedOutput, refusal("provider-failed"), true, false, []providerhealth.Outcome{providerhealth.MalformedOutput}},
		{"provider failed without an answer, tree moved", "uncertain", refusal("provider-failed"), true, true, []providerhealth.Outcome{providerhealth.MalformedOutput}},
		{"provider failed without an answer, retirement unproven", "uncertain", refusal("provider-failed"), false, false, []providerhealth.Outcome{providerhealth.MalformedOutput}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			x, d, a := fixture(t)
			d.Config.ChangeProducer = true
			x.Runtime = refusingRuntime{dir: t.TempDir(), err: tc.err, retired: tc.retired}
			calls := 0
			x.Tree = func(context.Context, Binding) (string, error) {
				if calls++; calls > 1 && tc.changed {
					return "changed", nil
				}
				return "tree", nil
			}
			var observed []providerhealth.Outcome
			x.Observe = func(_ context.Context, _ engine.Dispatch, _ engine.Session, o providerhealth.Outcome) error {
				observed = append(observed, o)
				return nil
			}
			r, e := x.Execute(context.Background(), d)
			if e == nil || r.Class != tc.want || a.released != 1 || fmt.Sprint(observed) != fmt.Sprint(tc.observed) {
				t.Fatalf("class %q err %v observed %v released %d", r.Class, e, observed, a.released)
			}
			if tc.want != "uncertain" && !strings.Contains(r.Detail, "old text not found") {
				t.Fatal("typed outcome lacks its cause", r.Detail)
			}
			// Only a binding refusal names the binding, the session's route
			// (here the role's first, as no route was selected).
			if refused := r.RefusedRoute; (refused != nil) != (tc.want == engine.ClassBindingRefused) || refused != nil && *refused != (workflow.Binding{Provider: "provider", Model: "model"}) {
				t.Fatalf("refused route %+v for class %q", refused, r.Class)
			}
		})
	}
}

// TestExecutionProviderBridge is the production bridge (provider.Run with the
// Claude Code protocol and the bridges' exit status) over a scripted CLI.
func TestExecutionProviderBridge(t *testing.T) {
	if os.Getenv("EXECUTION_PROVIDER_BRIDGE") != "1" {
		return
	}
	err := provider.Run(context.Background(), provider.Config{Provider: "claude", Binary: os.Getenv("EXECUTION_PROVIDER_CLI"), Timeout: 30 * time.Second}, os.Args[len(os.Args)-1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	os.Exit(provider.ExitCode(err))
}

// The scripted CLI marks that it ran and answers with the answer file.
const executionProviderCLI = `#!/bin/sh
cat > /dev/null
touch "$EXECUTION_PROVIDER_MARK"
sid=12345678-1234-4234-8234-123456789abc
printf '{"type":"system","subtype":"init","session_id":"%s","tools":["StructuredOutput"],"mcp_servers":[]}\n' "$sid"
printf '{"type":"result","subtype":"success","is_error":false,"session_id":"%s","structured_output":%s}\n' "$sid" "$(cat "$EXECUTION_PROVIDER_ANSWER")"
`

// FAE-WF-034, FAE-RT-004/RT-009/RT-010 through the shipped chain: Executor,
// Direct, the production bridge and its exit status. A schema failure or an
// unmatched replacement is malformed output charged to the binding with the
// tree untouched; a scope refusal or a failed repository listing never
// reaches the provider and charges nothing. Each is a bounded retry, never
// uncertain. A binding this Claude bridge cannot run (a Codex binding, no
// credentials in the target environment) never reaches the provider either:
// it is binding-refused, naming the binding to fail over from.
func TestProviderBridgeRefusalsThroughDirect(t *testing.T) {
	codex := workflow.Binding{Provider: "codex", Model: "fixture-model"}
	for _, tc := range []struct {
		name, answer, want, detail string
		scope                      TaskScope
		noGit, ran                 bool
		observed                   []providerhealth.Outcome
		route                      workflow.Binding
		noCredentials              bool
	}{
		{"accepted", `{"output":{"accepted":true},"edits":[{"path":"answer.go","content":"package answer\n","replacements":[]}]}`, "", "", TaskScope{Files: []string{"answer.go"}}, false, true, []providerhealth.Outcome{providerhealth.OK}, workflow.Binding{}, false},
		{"schema", `{"output":{"accepted":"yes"},"edits":[]}`, engine.ClassMalformedOutput, "bridge refused the provider's output", TaskScope{Files: []string{"answer.go"}}, false, true, []providerhealth.Outcome{providerhealth.MalformedOutput}, workflow.Binding{}, false},
		{"unmatched replacement", `{"output":{"accepted":true},"edits":[{"path":"answer.go","content":null,"replacements":[{"old":"missing text","new":"x"}]}]}`, engine.ClassMalformedOutput, "old text not found", TaskScope{Files: []string{"answer.go"}}, false, true, []providerhealth.Outcome{providerhealth.MalformedOutput}, workflow.Binding{}, false},
		{"scope refused", `{"output":{"accepted":true},"edits":[]}`, engine.ClassRefusedBeforeLaunch, "before the provider ran: task_scope test_file", TaskScope{Files: []string{"answer.go"}, TestFile: "answer_test.go"}, false, false, nil, workflow.Binding{}, false},
		{"repository listing fails", `{"output":{"accepted":true},"edits":[]}`, engine.ClassRefusedBeforeLaunch, "before the provider ran: repository listing failed", TaskScope{Files: []string{"answer.go"}}, true, false, nil, workflow.Binding{}, false},
		{"another provider's binding", `{"output":{"accepted":true},"edits":[]}`, engine.ClassBindingRefused, `bridge refused the binding before the provider ran: this claude bridge does not serve "codex" bindings`, TaskScope{Files: []string{"answer.go"}}, false, false, nil, codex, false},
		{"no credentials", `{"output":{"accepted":true},"edits":[]}`, engine.ClassBindingRefused, "bridge refused the binding before the provider ran: Claude credentials must be supplied explicitly", TaskScope{Files: []string{"answer.go"}}, false, false, nil, workflow.Binding{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			worktree, e := filepath.EvalSymlinks(t.TempDir())
			if e != nil {
				t.Fatal(e)
			}
			root, e := filepath.EvalSymlinks(t.TempDir())
			if e != nil {
				t.Fatal(e)
			}
			if !tc.noGit {
				git := exec.Command("git", "init", "-q", worktree)
				git.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
				if out, e := git.CombinedOutput(); e != nil {
					t.Fatal(e, string(out))
				}
			}
			answerGo := filepath.Join(worktree, "answer.go")
			if e = os.WriteFile(answerGo, []byte("original"), 0600); e != nil {
				t.Fatal(e)
			}
			scratch := t.TempDir()
			cli, answer, mark := filepath.Join(scratch, "claude"), filepath.Join(scratch, "answer.json"), filepath.Join(scratch, "ran")
			if e = os.WriteFile(cli, []byte(executionProviderCLI), 0700); e != nil {
				t.Fatal(e)
			}
			if e = os.WriteFile(answer, []byte(tc.answer), 0600); e != nil {
				t.Fatal(e)
			}
			exe, e := os.Executable()
			if e != nil {
				t.Fatal(e)
			}
			route := tc.route
			if route.Provider == "" {
				route = workflow.Binding{Provider: "claude", Model: "fixture-model"}
			}
			role := workflow.Role{Instructions: "implement", Routes: []workflow.Binding{route}, Profile: workflow.Profile{Allowed: []string{"handoff", "read", "edit"}}}
			environment := map[string]string{"EXECUTION_PROVIDER_BRIDGE": "1", "EXECUTION_PROVIDER_CLI": cli, "EXECUTION_PROVIDER_ANSWER": answer, "EXECUTION_PROVIDER_MARK": mark, "CLAUDE_CODE_OAUTH_TOKEN": "fixture-token", "GORACE": "atexit_sleep_ms=0"}
			if tc.noCredentials {
				delete(environment, "CLAUDE_CODE_OAUTH_TOKEN")
			}
			document := workflow.Document{Roles: map[string]workflow.Role{"coder": role}, Artifacts: map[string]workflow.ArtifactType{"output": {Schema: json.RawMessage(`{"type":"object","required":["accepted"],"additionalProperties":false,"properties":{"accepted":{"type":"boolean"}}}`)}}}
			a := &admission{}
			var observed []providerhealth.Outcome
			x := &Executor{
				Runtime: &runtime.Direct{Root: root, Command: []string{exe, "-test.run=^TestExecutionProviderBridge$", "--", "{config}"}, Environment: environment},
				Resolve: func(context.Context, engine.Dispatch) (Binding, error) {
					return Binding{Worktree: worktree, Mode: "local", Document: document, Scope: tc.scope}, nil
				},
				Tree: func(context.Context, Binding) (string, error) {
					data, e := os.ReadFile(answerGo)
					return "tree:" + string(data), e
				},
				Admission:   a,
				CheckChange: func(context.Context, engine.Dispatch, Binding) error { return nil },
				Observe: func(_ context.Context, _ engine.Dispatch, _ engine.Session, o providerhealth.Outcome) error {
					observed = append(observed, o)
					return nil
				},
			}
			d := engine.Dispatch{ExecutionKind: "live", ID: "dispatch", TaskID: "task", Stage: "implement", Tree: "tree:original", Config: workflow.Stage{Primitive: workflow.AgentTurn, Output: "output", Role: "coder", ChangeProducer: true}, Sessions: []engine.Session{{ID: "one", Role: "coder"}}}
			r, e := x.Execute(context.Background(), d)
			if tc.want == "" {
				if e != nil || r.Class != "" || r.NewTree != "tree:package answer\n" {
					t.Fatalf("accepted turn: class %q err %v tree %q", r.Class, e, r.NewTree)
				}
			} else if e == nil || r.Class != tc.want || !strings.Contains(r.Detail, tc.detail) || r.NewTree != "" {
				t.Fatalf("class %q detail %q err %v", r.Class, r.Detail, e)
			}
			if fmt.Sprint(observed) != fmt.Sprint(tc.observed) || a.released != 1 {
				t.Fatalf("observed %v released %d", observed, a.released)
			}
			if refused := r.RefusedRoute; (refused != nil) != (tc.want == engine.ClassBindingRefused) || refused != nil && *refused != route {
				t.Fatalf("refused route %+v, want %+v for class %q", refused, route, r.Class)
			}
			if _, e := os.Stat(mark); (e == nil) != tc.ran {
				t.Fatalf("provider ran: %v, want %v", e == nil, tc.ran)
			}
			if data, _ := os.ReadFile(answerGo); tc.want != "" && string(data) != "original" {
				t.Fatal("refused turn changed the task", string(data))
			}
		})
	}
}

// FAE-RT-004 through the shipped chain (Executor, Direct, the production
// bridge and its exit status): a provider CLI that exits before acknowledging
// the prompt, or an absolute provider executable that does not exist, is a
// startup retry, the engine's bounded relaunch that charges no attempt, not
// an uncertain dispatch. No health is observed and the task is untouched.
func TestProviderNeverAcknowledgedRetriesThroughDirect(t *testing.T) {
	for _, tc := range []struct {
		name, detail string
		missing, ran bool
	}{
		{"CLI exits before acknowledging", "provider exited before acknowledging the prompt", false, true},
		{"absolute CLI missing", "provider executable did not start", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			cli, mark := filepath.Join(scratch, "claude"), filepath.Join(scratch, "ran")
			if !tc.missing {
				if e = os.WriteFile(cli, []byte("#!/bin/sh\ncat > /dev/null\ntouch \"$EXECUTION_PROVIDER_MARK\"\necho 'fatal: no session' >&2\nexit 1\n"), 0700); e != nil {
					t.Fatal(e)
				}
			}
			exe, e := os.Executable()
			if e != nil {
				t.Fatal(e)
			}
			route := workflow.Binding{Provider: "claude", Model: "fixture-model"}
			role := workflow.Role{Instructions: "implement", Routes: []workflow.Binding{route}, Profile: workflow.Profile{Allowed: []string{"handoff", "read", "edit"}}}
			environment := map[string]string{"EXECUTION_PROVIDER_BRIDGE": "1", "EXECUTION_PROVIDER_CLI": cli, "EXECUTION_PROVIDER_MARK": mark, "CLAUDE_CODE_OAUTH_TOKEN": "fixture-token", "GORACE": "atexit_sleep_ms=0"}
			document := workflow.Document{Roles: map[string]workflow.Role{"coder": role}, Artifacts: map[string]workflow.ArtifactType{"output": {Schema: json.RawMessage(`{"type":"object","required":["accepted"],"additionalProperties":false,"properties":{"accepted":{"type":"boolean"}}}`)}}}
			a := &admission{}
			var observed []providerhealth.Outcome
			x := &Executor{
				Runtime: &runtime.Direct{Root: root, Command: []string{exe, "-test.run=^TestExecutionProviderBridge$", "--", "{config}"}, Environment: environment},
				Resolve: func(context.Context, engine.Dispatch) (Binding, error) {
					return Binding{Worktree: worktree, Mode: "local", Document: document, Scope: TaskScope{Files: []string{"answer.go"}}}, nil
				},
				Tree: func(context.Context, Binding) (string, error) {
					data, e := os.ReadFile(answerGo)
					return "tree:" + string(data), e
				},
				Admission:   a,
				CheckChange: func(context.Context, engine.Dispatch, Binding) error { return nil },
				Observe: func(_ context.Context, _ engine.Dispatch, _ engine.Session, o providerhealth.Outcome) error {
					observed = append(observed, o)
					return nil
				},
			}
			d := engine.Dispatch{ExecutionKind: "live", ID: "dispatch", TaskID: "task", Stage: "implement", Tree: "tree:original", Config: workflow.Stage{Primitive: workflow.AgentTurn, Output: "output", Role: "coder", ChangeProducer: true}, Sessions: []engine.Session{{ID: "one", Role: "coder", Route: route}}}
			r, e := x.Execute(context.Background(), d)
			if e == nil || r.Class != "startup-retry" || !strings.Contains(r.Detail, "the provider never acknowledged the prompt: "+tc.detail) || r.NewTree != "" || r.RefusedRoute != nil {
				t.Fatalf("class %q detail %q err %v", r.Class, r.Detail, e)
			}
			if len(observed) != 0 || a.released != 1 {
				t.Fatalf("observed %v released %d", observed, a.released)
			}
			if _, e := os.Stat(mark); (e == nil) != tc.ran {
				t.Fatalf("provider ran: %v, want %v", e == nil, tc.ran)
			}
			if data, _ := os.ReadFile(answerGo); string(data) != "original" {
				t.Fatal("the turn changed the task", string(data))
			}
		})
	}
}
