package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/commandshim"
	"github.com/xleelz/fae/daemon"
	"github.com/xleelz/fae/hostguard"
	"github.com/xleelz/fae/scheduler"
)

// operatorFake serves one instance socket and records every RPC it receives.
func operatorFake(t *testing.T) (string, chan api.Request) {
	t.Helper()
	// Unix socket paths are short; use the disk-backed base the daemon fixture uses.
	home, err := os.MkdirTemp("/var/tmp", "fae-cli-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	layout, err := daemon.Paths(home, "default")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(layout.Run, 0700); err != nil {
		t.Fatal(err)
	}
	token, err := api.Credential(layout.Credential)
	if err != nil {
		t.Fatal(err)
	}
	requests := make(chan api.Request, 4)
	server, err := api.New(token, func(_ context.Context, r api.Request) (any, error) {
		requests <- r
		return map[string]bool{"ok": true}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = server.Listen(layout.Socket); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve() }()
	t.Cleanup(func() {
		server.Shutdown(context.Background())
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	return home, requests
}

type thinCase struct {
	args   []string
	method string
	params map[string]any
}

func checkThinRPCs(t *testing.T, home string, requests chan api.Request, cases []thinCase) {
	t.Helper()
	for _, tc := range cases {
		var out bytes.Buffer
		if err := execute(context.Background(), append(append([]string{}, tc.args...), "--home", home), &out); err != nil {
			t.Fatal(tc.args, err)
		}
		r := <-requests
		var params map[string]any
		if err := json.Unmarshal(r.Params, &params); err != nil {
			t.Fatal(err)
		}
		if r.Method != tc.method || !reflect.DeepEqual(params, tc.params) {
			t.Fatalf("%v: %s %v", tc.args, r.Method, params)
		}
	}
}

func checkUsage(t *testing.T, cases [][]string) { t.Helper(); checkExit(t, 2, cases) }

// checkRefused: a mutating verb without --authorizing-message refuses with 1 (FAE-CLI-001).
func checkRefused(t *testing.T, cases [][]string) { t.Helper(); checkExit(t, 1, cases) }

func checkExit(t *testing.T, code int, cases [][]string) {
	t.Helper()
	for _, args := range cases {
		if err := execute(context.Background(), args, &bytes.Buffer{}); err == nil || exitCode(err) != code {
			t.Fatal(args, err)
		}
	}
}

func TestMessageReplyInboxCLI(t *testing.T) {
	home, requests := operatorFake(t)
	checkThinRPCs(t, home, requests, []thinCase{
		{[]string{"message", "--text", "look at alpha", "--task", "alpha"}, "Message", map[string]any{"text": "look at alpha", "task": "alpha"}},
		{[]string{"message", "--text", "plain", "--id", "m1"}, "Message", map[string]any{"text": "plain", "id": "m1"}},
		{[]string{"reply", "--to", "m1", "--text", "answer"}, "Message", map[string]any{"text": "answer", "reply_to": "m1"}},
		{[]string{"inbox"}, "Inbox", map[string]any{"limit": float64(50), "per_thread": float64(20)}},
		{[]string{"inbox", "--task", "alpha", "--kind", "threads,blocked", "--limit", "5"}, "Inbox", map[string]any{"task": "alpha", "kinds": []any{"threads", "blocked"}, "limit": float64(5), "per_thread": float64(20)}},
	})
	checkUsage(t, [][]string{{"reply", "--text", "x"}, {"reply", "--to", "m"}, {"message"}, {"message", "--text", "x", "--to", "m"}, {"inbox", "--limit", "0"}, {"inbox", "--kind", "bogus"}, {"inbox", "extra"}})
}

func TestAttachDesignDocCLI(t *testing.T) {
	home, requests := operatorFake(t)
	checkThinRPCs(t, home, requests, []thinCase{
		{[]string{"attach-design-doc", "--task", "alpha", "--reference", "docs/design.md", "--authorizing-message", "m"}, "AttachDesignDoc", map[string]any{"task": "alpha", "reference": "docs/design.md", "authorizing_message": "m"}},
		{[]string{"attach-design-doc", "--item", "beta", "--reference", "https://example.test/d", "--authorizing-message", "m"}, "AttachDesignDoc", map[string]any{"item": "beta", "reference": "https://example.test/d", "authorizing_message": "m"}},
	})
	checkRefused(t, [][]string{{"attach-design-doc", "--task", "a", "--reference", "r"}})
	checkUsage(t, [][]string{{"attach-design-doc", "--task", "a", "--item", "a", "--reference", "r", "--authorizing-message", "m"}, {"attach-design-doc", "--task", "a", "--authorizing-message", "m"}, {"attach-design-doc", "--reference", "r", "--authorizing-message", "m"}})
}

func TestReclaimCLI(t *testing.T) {
	home, requests := operatorFake(t)
	checkThinRPCs(t, home, requests, []thinCase{
		{[]string{"reclaim", "--authorizing-message", "m"}, "Reclaim", map[string]any{"authorizing_message": "m"}},
		{[]string{"reclaim", "--host-wide", "--dry-run", "--authorizing-message", "m"}, "Reclaim", map[string]any{"host_wide": true, "dry_run": true, "authorizing_message": "m"}},
	})
	checkRefused(t, [][]string{{"reclaim"}, {"reclaim", "--dry-run"}})
	checkUsage(t, [][]string{{"reclaim", "--authorizing-message", "m", "extra"}, {"reclaim", "extra"}})
}

func TestAnalyzeSessionCLI(t *testing.T) {
	home, requests := operatorFake(t)
	checkThinRPCs(t, home, requests, []thinCase{
		{[]string{"analyze-session", "--task", "alpha"}, "AnalyzeSession", map[string]any{"task": "alpha"}},
	})
	checkUsage(t, [][]string{{"analyze-session"}, {"analyze-session", "--task", "a", "x"}})
}

func TestHeavyOpRunCLIExitCodes(t *testing.T) {
	proves(t, "heavy-op run", 75)
	ledger := filepath.Join(t.TempDir(), "host")
	if _, e := scheduler.OpenLedger(ledger, 2); e != nil {
		t.Fatal(e)
	}
	heavyOpObserve = func(context.Context) (hostguard.Sample, error) {
		r := func(v float64) hostguard.Reading { return hostguard.Reading{Value: v, Known: true} }
		return hostguard.Sample{Memory: r(64 * float64(hostguard.GiB)), Swap: r(1), IO10: r(0), IO60: r(0), Memory10: r(0), Memory60: r(0), LoadPerCore: r(0.1)}, nil
	}
	heavyOpPolicy = hostguard.Policy{Window: 30 * time.Millisecond, MaxGap: time.Second, Interval: 3 * time.Millisecond, Checks: 10, IOGate: 5}
	defer func() { heavyOpObserve, heavyOpPolicy = nil, hostguard.Policy{} }()
	err := execute(context.Background(), []string{"heavy-op", "run", "--instance", "default", "--json", "--ledger", ledger, "--weight", "1", "--class", "role", "--", "/bin/sh", "-c", "exit 4"}, &bytes.Buffer{})
	if exitCode(err) != 4 || err.Error() != "" {
		t.Fatalf("child code not passed through: %v %d", err, exitCode(err))
	}
	t.Setenv("FAE_HOST_LEDGER", ledger)
	if err = execute(context.Background(), []string{"heavy-op", "run", "--weight", "2", "--class", "operator", "--", "/bin/true"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if err = execute(context.Background(), []string{"heavy-op", "run", "--ledger", filepath.Join(t.TempDir(), "absent"), "--weight", "1", "--class", "role", "--", "/bin/true"}, &bytes.Buffer{}); exitCode(err) != commandshim.HeavyOpRefused || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("refusal code %v %d", err, exitCode(err))
	}
	checkUsage(t, [][]string{{"heavy-op"}, {"heavy-op", "status"}, {"heavy-op", "run", "--weight", "1", "--class", "role"}, {"heavy-op", "run", "--weight", "x", "--", "/bin/true"}})
}

func TestHeavyOpShimPolicyAndHookCLI(t *testing.T) {
	proves(t, "heavy-op run --shim-policy", 0)
	proves(t, "heavy-op run --shim-policy", 77)
	proves(t, "heavy-op run --shim-policy", 78)
	proves(t, "heavy-op hook", 0)
	proves(t, "heavy-op hook", 2)
	// The in-process CLI reads the process environment as a role wrapper's
	// broker does: an ambient nesting marker must not decide this run.
	t.Setenv(commandshim.NestedMarker, "")
	os.Unsetenv(commandshim.NestedMarker)
	ledger := filepath.Join(t.TempDir(), "host")
	if _, e := scheduler.OpenLedger(ledger, 2); e != nil {
		t.Fatal(e)
	}
	heavyOpObserve = func(context.Context) (hostguard.Sample, error) {
		r := func(v float64) hostguard.Reading { return hostguard.Reading{Value: v, Known: true} }
		return hostguard.Sample{Memory: r(64 * float64(hostguard.GiB)), Swap: r(1), IO10: r(0), IO60: r(0), Memory10: r(0), Memory60: r(0), LoadPerCore: r(0.1)}, nil
	}
	heavyOpPolicy = hostguard.Policy{Window: 30 * time.Millisecond, MaxGap: time.Second, Interval: 3 * time.Millisecond, Checks: 10, IOGate: 5}
	defer func() { heavyOpObserve, heavyOpPolicy = nil, hostguard.Policy{} }()
	root, _ := filepath.EvalSymlinks(t.TempDir())
	worktree, tools := filepath.Join(root, "wt"), filepath.Join(root, "tools")
	os.MkdirAll(worktree, 0700)
	os.MkdirAll(tools, 0700)
	// The tool exits with the code its working directory names, else 6.
	os.WriteFile(filepath.Join(tools, "go"), []byte("#!/bin/sh\nexit $(cat code 2>/dev/null || echo 6)\n"), 0700)
	install := func(parent string, s commandshim.RoleShims) string {
		s.Broker, s.Ledger, s.Worktree = "/usr/local/bin/fae", ledger, worktree
		dir, err := commandshim.InstallRoleShims(parent, s, tools)
		if err != nil {
			t.Fatal(err)
		}
		return filepath.Join(dir, ".policy.json")
	}
	coder, planner := install(t.TempDir(), commandshim.RoleShims{Test: true, Local: true}), install(t.TempDir(), commandshim.RoleShims{Local: true})
	t.Chdir(worktree)
	if err := execute(context.Background(), []string{"heavy-op", "run", "--shim-policy", coder, "--", "go", "test", "."}, &bytes.Buffer{}); exitCode(err) != 6 {
		t.Fatalf("leased shim run %v %d", err, exitCode(err))
	}
	// A materialized policy's tool succeeding exits 0; its own 1 passes through.
	for _, code := range []int{0, 1} {
		os.WriteFile(filepath.Join(worktree, "code"), []byte(strconv.Itoa(code)), 0600)
		if err := execute(context.Background(), []string{"heavy-op", "run", "--shim-policy", coder, "--", "go", "test", "."}, &bytes.Buffer{}); code == 0 && err != nil || code != 0 && exitCode(err) != code {
			t.Fatalf("leased shim run want %d: %v", code, err)
		}
	}
	os.Remove(filepath.Join(worktree, "code"))
	if err := execute(context.Background(), []string{"heavy-op", "run", "--shim-policy", planner, "--", "go", "test", "."}, &bytes.Buffer{}); exitCode(err) != commandshim.Refused || !strings.Contains(err.Error(), "pre-code") {
		t.Fatalf("pre-code refusal %v %d", err, exitCode(err))
	}
	// FAE-MODE-002: a Local push has its own code, past git's global options.
	if err := execute(context.Background(), []string{"heavy-op", "run", "--shim-policy", coder, "--", "git", "-C", ".", "push"}, &bytes.Buffer{}); exitCode(err) != commandshim.LocalModeRefused || !strings.Contains(err.Error(), "Local mode") {
		t.Fatalf("Local push refusal %v %d", err, exitCode(err))
	}
	stdin, _ := os.CreateTemp(t.TempDir(), "hook")
	stdin.WriteString(`{"tool_name":"Bash","tool_input":{"command":"/usr/local/go/bin/go test ./..."}}`)
	stdin.Seek(0, 0)
	saved := os.Stdin
	os.Stdin = stdin
	defer func() { os.Stdin = saved }()
	var out bytes.Buffer
	if err := execute(context.Background(), []string{"heavy-op", "hook", "--shim-dir", filepath.Dir(coder)}, &out); err != nil || !strings.Contains(out.String(), `"deny"`) {
		t.Fatalf("hook %v %s", err, out.String())
	}
	// A request that bypasses nothing gets no decision; an unreadable one fails
	// closed with 2, which a provider pre-tool hook treats as a block, never 1.
	for _, tc := range []struct {
		request string
		code    int
	}{{`{"tool_name":"Bash","tool_input":{"command":"go test ./..."}}`, 0}, {`not json`, 2}, {`{"tool_input":{"command":7}}`, 2}} {
		restore := withStdin(t, tc.request)
		out.Reset()
		err := execute(context.Background(), []string{"heavy-op", "hook", "--shim-dir", filepath.Dir(coder)}, &out)
		restore()
		if tc.code == 0 && (err != nil || out.String() != "{}\n") || tc.code != 0 && (exitCode(err) != tc.code || !strings.Contains(err.Error(), "blocked")) {
			t.Fatalf("hook request %q: %v %q", tc.request, err, out.String())
		}
	}
	checkUsage(t, [][]string{{"heavy-op", "hook"}, {"heavy-op", "hook", "--shim-dir", "rel"}, {"heavy-op", "run", "--shim-policy", coder, "--class", "operator", "--", "go"}})
}

// brokenOutput is a standard output whose writes fail.
type brokenOutput struct{}

func (brokenOutput) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

// FAE-HOST-011: the pre-tool hook answers 0 with its decision on standard
// output, or 2, which blocks the tool call. A provider runs the tool on 1, so
// a decision it cannot write and a session without $HOME both block.
func TestHeavyOpHookFailsClosedWithoutHomeOrOutput(t *testing.T) {
	proves(t, "heavy-op hook", 2)
	shims := t.TempDir()
	hook := func(out io.Writer) error {
		restore := withStdin(t, `{"tool_name":"Bash","tool_input":{"command":"/usr/local/go/bin/go test ./..."}}`)
		defer restore()
		return execute(context.Background(), []string{"heavy-op", "hook", "--shim-dir", shims}, out)
	}
	var out bytes.Buffer
	if err := hook(&out); err != nil || !strings.Contains(out.String(), `"deny"`) {
		t.Fatalf("hook %v %q", err, out.String())
	}
	if err := hook(brokenOutput{}); exitCode(err) != 2 || !strings.Contains(err.Error(), "tool call blocked") {
		t.Fatalf("unwritten decision: exit %d %v", exitCode(err), err)
	}
	t.Setenv("HOME", "")
	os.Unsetenv("HOME")
	out.Reset()
	if err := hook(&out); exitCode(err) != 2 || out.Len() != 0 || !strings.Contains(err.Error(), "home directory") {
		t.Fatalf("no $HOME: exit %d %v %q", exitCode(err), err, out.String())
	}
	// Other verbs still refuse without $HOME; help needs no home.
	if err := execute(context.Background(), []string{"status"}, &bytes.Buffer{}); exitCode(err) != 1 {
		t.Fatalf("status without $HOME: exit %d %v", exitCode(err), err)
	}
	if err := execute(context.Background(), []string{"--help"}, &bytes.Buffer{}); err != nil {
		t.Fatalf("help without $HOME: %v", err)
	}
}

// FAE-HOST-011: the hook binary exits 2 whenever its decision cannot reach
// the provider: standard output a pipe whose reader has gone (it must not die
// of SIGPIPE), closed at exec (which the Go runtime reopens on /dev/null) or
// /dev/null itself. A readable output still gets the decision and 0.
func TestHeavyOpHookBinaryBlocksWhenItsDecisionCannotBeDelivered(t *testing.T) {
	shims, dir := t.TempDir(), t.TempDir()
	request := filepath.Join(dir, "request.json")
	if err := os.WriteFile(request, []byte(`{"tool_name":"Bash","tool_input":{"command":"/usr/local/go/bin/go test ./..."}}`), 0600); err != nil {
		t.Fatal(err)
	}
	// hook runs this test binary as fae, through /bin/sh when redirect is set.
	hook := func(stdout *os.File, redirect string) (int, string) {
		cmd := exec.Command(os.Args[0], "heavy-op", "hook", "--shim-dir", shims)
		if redirect != "" {
			cmd = exec.Command("/bin/sh", "-c", `exec "$0" heavy-op hook --shim-dir "$1" `+redirect, os.Args[0], shims)
		}
		in, err := os.Open(request)
		if err != nil {
			t.Fatal(err)
		}
		defer in.Close()
		var stderr bytes.Buffer
		cmd.Env, cmd.Stdin, cmd.Stdout, cmd.Stderr = append(os.Environ(), "CLI_TEST_AS_MAIN=1"), in, stdout, &stderr
		err = cmd.Run()
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
				return 128 + int(status.Signal()), stderr.String()
			}
			return exit.ExitCode(), stderr.String()
		}
		if err != nil {
			t.Fatal(err)
		}
		return 0, stderr.String()
	}
	file, err := os.Create(filepath.Join(dir, "decision"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if code, stderr := hook(file, ""); code != 0 {
		t.Fatalf("readable output: exit %d %s", code, stderr)
	}
	if raw, _ := os.ReadFile(file.Name()); !strings.Contains(string(raw), `"deny"`) {
		t.Fatalf("decision %q", raw)
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	reader.Close()
	defer writer.Close()
	for name, run := range map[string]func() (int, string){
		"reader gone":     func() (int, string) { return hook(writer, "") },
		"closed at exec":  func() (int, string) { return hook(file, ">&-") },
		"discarded":       func() (int, string) { return hook(nil, "") },
		"discarded by sh": func() (int, string) { return hook(file, ">/dev/null") },
		"stderr gone too": func() (int, string) { return hook(writer, "2>&1") },
	} {
		if code, stderr := run(); code != 2 || name != "stderr gone too" && !strings.Contains(stderr, "tool call blocked") {
			t.Errorf("%s: exit %d, want 2 (%s)", name, code, stderr)
		}
	}
}

func TestDoctorReportsMissingAgentCLIWithRemedy(t *testing.T) {
	env := doctorEnv{
		home: t.TempDir(), instance: "default", dolt: "dolt",
		lookPath: func(name string) (string, error) {
			if name == "claude" || name == "herdr-core" || name == "herdr" {
				return "", os.ErrNotExist
			}
			return "/usr/bin/" + name, nil
		},
		version:   func(string, ...string) (string, error) { return "git version 2.45.1", nil },
		statfs:    func(string) (uint64, uint64, error) { return 512 << 20, 50, nil },
		sandboxed: func() (bool, error) { return false, nil },
	}
	checks := doctorChecks(env)
	byName := map[string]doctorCheck{}
	for _, c := range checks {
		if c.Status != "pass" && c.Remedy == "" {
			t.Fatalf("%s %s without remedy", c.Name, c.Status)
		}
		byName[c.Name] = c
	}
	if c := byName["agent-cli:claude"]; c.Status != "fail" || !strings.Contains(c.Remedy, "claude") {
		t.Fatalf("missing claude %+v", c)
	}
	if byName["agent-cli:codex"].Status != "pass" || byName["git"].Status != "pass" || byName["dolt"].Status != "pass" {
		t.Fatal(checks)
	}
	if byName["multiplexer:herdr"].Status != "warn" || byName["instance-layout"].Status != "warn" || byName["sandbox"].Status != "pass" {
		t.Fatal(checks)
	}
	if byName["filesystem:instance-home"].Status != "fail" || !strings.Contains(byName["filesystem:instance-home"].Detail, "inodes") {
		t.Fatal(byName["filesystem:instance-home"])
	}
	env.sandboxed = func() (bool, error) { return true, nil }
	env.version = func(string, ...string) (string, error) { return "git version 2.39.0", nil }
	for _, c := range doctorChecks(env) {
		if (c.Name == "sandbox" || c.Name == "git") && c.Status != "fail" {
			t.Fatal(c)
		}
	}
	var out bytes.Buffer
	if err := execute(context.Background(), []string{"doctor", "--home", t.TempDir()}, &out); err != nil && exitCode(err) != 1 {
		t.Fatal(err)
	}
	var report struct {
		OK     bool          `json:"ok"`
		Checks []doctorCheck `json:"checks"`
	}
	if err := json.Unmarshal(out.Bytes(), &report); err != nil || len(report.Checks) < 8 {
		t.Fatal(out.String(), err)
	}
	checkUsage(t, [][]string{{"doctor", "extra"}})
}

func TestMigrateCLI(t *testing.T) {
	home, requests := operatorFake(t)
	checkThinRPCs(t, home, requests, []thinCase{
		{[]string{"migrate", "--task", "alpha", "--revision", "2", "--authorizing-message", "m"}, "Migrate", map[string]any{"id": "alpha", "revision": float64(2), "authorizing_message": "m"}},
		{[]string{"migrate", "--task", "alpha", "--revision", "3", "--stage", "plan", "--authorizing-message", "m"}, "Migrate", map[string]any{"id": "alpha", "revision": float64(3), "stage": "plan", "authorizing_message": "m"}},
	})
	checkRefused(t, [][]string{{"migrate", "--task", "a", "--revision", "2"}})
	checkUsage(t, [][]string{{"migrate", "--task", "a"}, {"migrate", "--task", "a", "--authorizing-message", "m"}, {"migrate", "--revision", "2", "--authorizing-message", "m"}, {"migrate", "--task", "a", "--revision", "0", "--authorizing-message", "m"}})
}

func TestDecideCLI(t *testing.T) {
	home, requests := operatorFake(t)
	checkThinRPCs(t, home, requests, []thinCase{
		{[]string{"decide", "--task", "alpha", "--option", "ship", "--authorizing-message", "m"}, "Decide", map[string]any{"id": "alpha", "option": "ship", "authorizing_message": "m"}},
		{[]string{"decide", "--task", "alpha", "--option", "rework", "--decision", "decision-1", "--rationale", "why", "--authorizing-message", "m"}, "Decide", map[string]any{"id": "alpha", "option": "rework", "decision": "decision-1", "rationale": "why", "authorizing_message": "m"}},
	})
	checkRefused(t, [][]string{{"decide", "--task", "a", "--option", "ship"}})
	checkUsage(t, [][]string{{"decide", "--task", "a", "--authorizing-message", "m"}, {"decide", "--option", "ship", "--authorizing-message", "m"}, {"decide", "--task", "a", "--option", "ship", "extra", "--authorizing-message", "m"}})
}

// The help inventory names every flag a command accepts (FAE-CLI-001 audit:
// the run usage line omitted accepted flags). Common flags, deprecated aliases
// and other verbs' flags are exempt.
func TestHelpUsageNamesEveryAcceptedFlag(t *testing.T) {
	usageOf := map[string]string{}
	for _, row := range commandList {
		usageOf[row[0]] = row[1]
	}
	exempt := map[string]bool{"home": true, "instance": true, "json": true, "destination": true, "source-ref": true}
	for _, args := range [][]string{{"run"}, {"cleanup", "scratch-archive"}, {"serve"}} {
		var out bytes.Buffer
		if err := execute(context.Background(), append(append([]string{}, args...), "--help", "--json"), &out); err != nil {
			t.Fatal(args, err)
		}
		var help struct {
			Flags []struct {
				Name string `json:"name"`
			} `json:"flags"`
		}
		if err := json.Unmarshal(out.Bytes(), &help); err != nil || len(help.Flags) == 0 {
			t.Fatal(args, err, out.String())
		}
		for _, f := range help.Flags {
			if args[0] == "serve" && f.Name == "authorizing-message" { // a common flag serve ignores
				continue
			}
			if !exempt[f.Name] && !strings.Contains(usageOf[args[0]]+" ", "--"+f.Name+" ") && !strings.Contains(usageOf[args[0]], "--"+f.Name+"]") && !strings.Contains(usageOf[args[0]], "--"+f.Name+"|") {
				t.Errorf("%v usage line omits --%s: %s", args, f.Name, usageOf[args[0]])
			}
		}
	}
}

// FAE-CLI-054: doctor checks configuration: the live configuration offline,
// and each target configuration through a running daemon.
func TestDoctorChecksConfiguration(t *testing.T) {
	dir := shortHome(t) // outside the checkout: a live configuration inside one is refused (FAE-SEC-003)
	write := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	c := common{home: shortHome(t), instance: "doc"}
	if got := doctorLiveConfig("", c); got.Status != "pass" {
		t.Fatal("no live configuration named", got)
	}
	if got := doctorLiveConfig(write("unknown.json", `{"bogus":1}`), c); got.Status != "fail" || !strings.Contains(got.Detail, "bogus") || got.Remedy == "" {
		t.Fatal("unknown field", got)
	}
	if got := doctorLiveConfig(write("incomplete.json", `{"integration_ref":"refs/heads/main","base":"xyz"}`), c); got.Status != "fail" || !strings.Contains(got.Detail, "refs/heads/fae/") || !strings.Contains(got.Detail, "immutable full commit SHA") {
		t.Fatal("each live issue named", got)
	}
	open := filepath.Join(dir, "open.json")
	if err := os.WriteFile(open, []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}
	if got := doctorLiveConfig(open, c); got.Status != "fail" {
		t.Fatal("world-readable live configuration accepted", got)
	}
	if got := doctorTargets(context.Background(), c); got.Status != "warn" {
		t.Fatal("stopped daemon", got)
	}
	layout, err := daemon.Paths(c.home, c.instance)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(layout.Run, 0700); err != nil {
		t.Fatal(err)
	}
	token, err := api.Credential(layout.Credential)
	if err != nil {
		t.Fatal(err)
	}
	server, err := api.New(token, func(_ context.Context, r api.Request) (any, error) {
		switch r.Method {
		case "TargetConfigList":
			return []map[string]any{{"id": "good", "revision": 1}, {"id": "bad", "revision": 1}, {"id": "bad", "revision": 2}, {"id": "gone", "revision": 1, "retired": true}}, nil
		case "TargetConfigGet":
			var p map[string]any
			_ = json.Unmarshal(r.Params, &p)
			return map[string]any{"document": map[string]any{"id": p["id"]}}, nil
		case "TargetConfigValidate":
			var p struct {
				Document struct {
					ID string `json:"id"`
				} `json:"document"`
			}
			_ = json.Unmarshal(r.Params, &p)
			if p.Document.ID == "gone" {
				t.Error("retired target validated")
			}
			if p.Document.ID == "bad" {
				return map[string]any{"valid": false, "errors": []string{"integration ref not protected"}}, nil
			}
			return map[string]any{"valid": true, "errors": []string{}}, nil
		}
		return nil, errors.New("unexpected " + r.Method)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = server.Listen(layout.Socket); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve() }()
	defer func() { server.Shutdown(context.Background()); <-done }()
	if got := doctorTargets(context.Background(), c); got.Status != "fail" || !strings.Contains(got.Detail, "bad: integration ref not protected") || strings.Contains(got.Detail, "good") {
		t.Fatal("invalid target not reported", got)
	}
}
