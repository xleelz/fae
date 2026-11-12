package commandshim

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// FAE-HOST-011: a shimmed tool run as a command under another fixed name is
// denied like the dashed subcommands: a Go toolchain as go1.N or gotip (which
// runs that toolchain's go by path, skipping the go shim's clamp), pytest as
// a Python module (skipping the pytest shim's refusal) and rustup run (which
// puts a toolchain's cargo, rustc and rustfmt ahead of their shims). Before,
// all three passed. The names stay command-position checks, so a tag, a
// module or a file that only mentions them passes, as do ordinary python and
// rustup commands.
func TestPreToolHookDeniesShimmedToolsUnderAnotherName(t *testing.T) {
	shims := "/s/sessions/session-1/command-shims"
	for _, tc := range []struct {
		command string
		deny    bool
	}{
		// A Go toolchain under another name.
		{"go1.22.0 build ./...", true},
		{"go1.21 test .", true},
		{"go1.23rc1 vet ./...", true},
		{"gotip test ./...", true},
		{"~/go/bin/go1.21.5 vet .", true},
		{"cd sub && go1.22.0 build .", true},
		{"exec go1.22.0 build .", true},
		{"bash -c 'gotip build .'", true},
		// pytest as a Python module.
		{"python -m pytest", true},
		{"python3 -m pytest -x tests", true},
		{"python3.12 -mpytest", true},
		{"/usr/bin/python3 -Bm pytest", true},
		{"python -W ignore -m pytest", true},
		{"python -X dev -m pytest tests", true},
		{"python -m py.test", true},
		{"python -m pytest.__main__", true},
		{"pypy3 -m pytest", true},
		{"cd tests; python -m pytest", true},
		// rustup run, with or without a +toolchain.
		{"rustup run stable cargo build", true},
		{"rustup run nightly rustc x.rs", true},
		{"rustup +nightly run nightly rustfmt x.rs", true},
		{"/home/u/.cargo/bin/rustup run stable bash", true},
		// Ordinary commands still pass.
		{"go build ./...", false},
		{"git checkout go1.22.0", false},
		{"git tag -l 'go1.*'", false},
		{"ls ~/sdk/gotip", false},
		{"pytest -x tests", false},
		{"python manage.py test", false},
		{"python -m pip install -r requirements.txt", false},
		{"python -m pytest_cov_report", false},
		{"python3 -c 'print(1)'", false},
		{"python script.py -m pytest", false},
		{`grep -rn "python -m pytest" docs`, false},
		{"echo python -m pytest", false},
		{"rustup show", false},
		{"rustup toolchain list", false},
		{"rustup which cargo", false},
		{"cargo +nightly build", false},
		{"cat docs/rustup-run.md", false},
	} {
		if _, got := BypassesShim(tc.command, shims); got != tc.deny {
			t.Errorf("%q: deny=%t, want %t", tc.command, got, tc.deny)
		}
	}
	for command, reason := range map[string]string{
		"go1.22.0 build ./...":          "go1.22.0 runs a Go toolchain without the go shim and its clamp; call go by name",
		"python3 -m pytest":             "python3 -m pytest runs pytest without the pytest shim; call pytest by name",
		"rustup run stable cargo build": "rustup run puts a Rust toolchain's cargo, rustc and rustfmt ahead of their role command shims",
	} {
		if got, _ := BypassesShim(command, shims); !strings.Contains(got, reason) {
			t.Errorf("%q: reason %q, want %q", command, got, reason)
		}
	}
	out, err := PreToolDecision(strings.NewReader(`{"tool_name":"Bash","tool_input":{"command":"python -m pytest"}}`), shims)
	if err != nil || !strings.Contains(string(out), `"permissionDecision":"deny"`) || !strings.Contains(string(out), "pytest shim") {
		t.Fatalf("%s %v", out, err)
	}
}

// FAE-HOST-011: the hook is a lexical check, and these documented passes stay
// passes (commandshim/README.md lists them): programs that run a command
// elsewhere with a PATH of their own, a program that puts its own tools first
// before running one, an interpreter that execs a tool itself, and compilers
// that are not shimmed tools (gccgo, tinygo, gcc). A change that starts
// denying one must update the README's list.
func TestPreToolHookDocumentedPassesStillPass(t *testing.T) {
	shims := "/s/sessions/session-1/command-shims"
	for _, command := range []string{
		"tmux new -d 'go build'",
		"screen -S x -X stuff 'go build'",
		"at now <<< 'go build'",
		"batch <<< 'go build'",
		"crontab f",
		"systemctl --user start x",
		"docker run golang go build",
		"nix-shell --run 'go build'",
		"podman run golang go build",
		"uv run pytest",
		"poetry run pytest",
		"pipenv run pytest",
		"conda run -n env pytest",
		"nice go1.22.0 build .",
		"xargs python -m pytest < dirs",
		"sudo go build",
		"ssh host go build",
		"python3 -c 'import subprocess; subprocess.run([\"pytest\"])'",
		"/usr/bin/$x build",
		"./g push",
		"gccgo -o x x.go",
		"tinygo build -o x .",
		"tinygo test ./...",
		"gcc -o x x.c",
	} {
		if reason, deny := BypassesShim(command, shims); deny {
			t.Errorf("%q: denied (%s); the README lists it as passing", command, reason)
		}
	}
}

// FAE-HOST-011: pytest's other names (its legacy entry point py.test and the
// versioned pytest-3 and py.test-3 distributions install) run pytest itself,
// so where a command starts they are denied like python -m pytest. Before,
// they passed. As with the other fixed names, text that only mentions them
// and a command another program starts still pass.
func TestPreToolHookDeniesPytestUnderItsOtherNames(t *testing.T) {
	shims := "/s/sessions/session-1/command-shims"
	for _, tc := range []struct {
		command string
		deny    bool
	}{
		{"py.test", true},
		{"py.test -x tests", true},
		{"pytest-3 -q", true},
		{"pytest-3.12 tests", true},
		{"py.test-3 tests", true},
		{"/usr/bin/pytest-3 -q", true},
		{"cd tests && py.test", true},
		{"bash -c 'pytest-3 -x'", true},
		{"pytest -x tests", false},
		{"cat py.test", false},
		{"grep -rn pytest-3 docs", false},
		{"pip install pytest-3", false},
		{"xargs py.test < dirs", false},
		{"pytest-cov --help", false},
	} {
		if _, got := BypassesShim(tc.command, shims); got != tc.deny {
			t.Errorf("%q: deny=%t, want %t", tc.command, got, tc.deny)
		}
	}
	if got, _ := BypassesShim("pytest-3 -q", shims); got != "pytest-3 runs pytest under another name without the pytest shim; call pytest by name" {
		t.Errorf("reason %q", got)
	}
}

// hookDenials is keyed by the exact source text of each denial reason that
// BypassesShim returns; each case lists command lines that must be denied
// with that reason. The census below fails when a denial is added without a
// case here, and scripts/check-ledger.py maps every key (FAE-VER-001).
var hookDenials = map[string]struct {
	reason   string
	commands []string
}{
	`"interpreter build and test tools require a scoped HostCommand stage"`: {"interpreter build and test tools require a scoped HostCommand stage", []string{"node --test x.test.js", "python3 -m compileall .", "python3 -m py_compile a.py"}},
	`"a brace expansion too large to check could name a shimmed tool by path"`: {"a brace expansion too large to check could name a shimmed tool by path",
		[]string{"echo {a,b}{a,b}{a,b}{a,b}{a,b}{a,b}{a,b}{a,b}{a,b}{a,b}{a,b}"}},
	`reset`: {"resetting PATH bypasses the role command shims",
		[]string{"PATH=/usr/bin go build", "BASH_ENV=x bash -c true", "env -i go build", "/usr/bin/env -uPATH go", "exec -c go build", "command -p go build", "bash -l -c 'go build'"}},
	`"a sourced file can reset PATH, which bypasses the role command shims; call the tool by name without sourcing"`: {"a sourced file can reset PATH",
		[]string{". venv/bin/activate", "source ~/.profile && go build"}},
	`base + " by path " + t.raw + " bypasses the role command shim; call " + base + " by name"`: {"go by path /usr/bin/go bypasses the role command shim; call go by name",
		[]string{"/usr/bin/go build ./..."}},
	`t.raw + " runs " + tool + " " + sub + " without the " + tool + " shim; call " + tool + " " + sub + " instead"`: {"git-push runs git push without the git shim; call git push instead",
		[]string{"git-push origin main"}},
	`"Go toolchain internal " + t.raw + " by path bypasses the go shim; call go by name"`: {"Go toolchain internal /usr/local/go/pkg/tool/linux_amd64/compile by path bypasses the go shim",
		[]string{"/usr/local/go/pkg/tool/linux_amd64/compile -V"}},
	`t.raw + " runs a Go toolchain without the go shim and its clamp; call go by name"`: {"gotip runs a Go toolchain without the go shim and its clamp",
		[]string{"gotip test ./..."}},
	`t.raw + " -m pytest runs pytest without the pytest shim; call pytest by name"`: {"python3 -m pytest runs pytest without the pytest shim",
		[]string{"python3 -m pytest"}},
	`t.raw + " runs pytest under another name without the pytest shim; call pytest by name"`: {"py.test runs pytest under another name without the pytest shim",
		[]string{"py.test -x"}},
	`"rustup run puts a Rust toolchain's cargo, rustc and rustfmt ahead of their role command shims; call the tool by name"`: {"rustup run puts a Rust toolchain's cargo, rustc and rustfmt ahead of their role command shims",
		[]string{"rustup run stable cargo build"}},
	`"systemd-run runs its command under the service manager with the manager's PATH, outside the session's process tree and the role command shims"`: {"systemd-run runs its command under the service manager",
		[]string{"systemd-run --user -P go build ."}},
}

// FAE-HOST-011, FAE-VER-001: every denial the hook can return fires on a
// command line, with its own reason; the census checks the cases are exactly
// BypassesShim's denials.
func TestPreToolHookDenialCorpus(t *testing.T) {
	shims := "/s/sessions/session-1/command-shims"
	for key, c := range hookDenials {
		for _, command := range c.commands {
			if reason, deny := BypassesShim(command, shims); !deny || !strings.Contains(reason, c.reason) {
				t.Errorf("denial %s: %q gave deny=%t %q", key, command, deny, reason)
			}
		}
	}
	raw, err := os.ReadFile("hook.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	start := strings.Index(body, "func BypassesShim(")
	end := strings.Index(body[start:], "\n}\n")
	found := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^\s*return (.*), true\s*$`).FindAllStringSubmatch(body[start:start+end], -1) {
		found[m[1]] = true
	}
	for k := range found {
		if _, ok := hookDenials[k]; !ok {
			t.Errorf("denial without a firing case: %s", k)
		}
	}
	for k := range hookDenials {
		if !found[k] {
			t.Errorf("case for no denial: %s", k)
		}
	}
	if len(found) < 10 {
		t.Fatalf("%d denials found", len(found))
	}
}

// FAE-MODE-002: PreToolDecision denies Bash commands invoking git push or git
// remote (including with leading global flags such as git -C ... push or
// git -c ... remote), while allowing non-bypassing git commands like git status
// and git diff.
func TestPreToolDecisionDeniesGitPushAndRemote(t *testing.T) {
	shims := "/s/sessions/session-1/command-shims"
	for _, tc := range []struct {
		command string
		want    string
	}{
		{"git push", "git push is forbidden inside a session"},
		{"git push origin main", "git push is forbidden inside a session"},
		{"git -C repo push origin main", "git push is forbidden inside a session"},
		{"git -c credential.helper= -C /tmp/repo push --force", "git push is forbidden inside a session"},
		{"cd repo && git push", "git push is forbidden inside a session"},
		{"env GIT_TRACE=1 git -C repo push", "git push is forbidden inside a session"},
		{"git remote add origin https://example.invalid/repo.git", "git remote is forbidden inside a session"},
		{"git remote set-url origin https://example.invalid/repo.git", "git remote is forbidden inside a session"},
		{"git -C repo -c core.sshCommand=false remote set-url origin https://example.invalid/repo.git", "git remote is forbidden inside a session"},
		{shims + "/git -C repo remote add up https://example.invalid/repo.git", "git remote is forbidden inside a session"},
		{"git status", ""},
		{"git diff", ""},
		{"git -C repo status --short", ""},
		{"git -c color.ui=false diff HEAD~1", ""},
		{"echo git push", ""},
	} {
		req, err := json.Marshal(map[string]any{
			"tool_name":  "Bash",
			"tool_input": map[string]string{"command": tc.command},
		})
		if err != nil {
			t.Fatal(err)
		}
		out, err := PreToolDecision(strings.NewReader(string(req)), shims)
		if err != nil {
			t.Fatalf("%q: %v", tc.command, err)
		}
		if tc.want == "" {
			if string(out) != "{}\n" {
				t.Errorf("%q: expected allowed decision, got %s", tc.command, out)
			}
			continue
		}
		var parsed struct {
			HookSpecificOutput struct {
				PermissionDecision       string `json:"permissionDecision"`
				PermissionDecisionReason string `json:"permissionDecisionReason"`
			} `json:"hookSpecificOutput"`
		}
		if err := json.Unmarshal(out, &parsed); err != nil {
			t.Fatalf("%q: unmarshal %s: %v", tc.command, out, err)
		}
		if parsed.HookSpecificOutput.PermissionDecision != "deny" || !strings.Contains(parsed.HookSpecificOutput.PermissionDecisionReason, tc.want) {
			t.Errorf("%q: got decision=%q reason=%q, want deny containing %q", tc.command, parsed.HookSpecificOutput.PermissionDecision, parsed.HookSpecificOutput.PermissionDecisionReason, tc.want)
		}
	}
}

func TestRestrictedShellHookRefusesAndAudits(t *testing.T) {
	root := t.TempDir()
	work := t.TempDir()
	audit := filepath.Join(root, "shell-refusals.jsonl")
	shims, err := InstallRoleShims(root, RoleShims{Worktree: work, Broker: "/bin/true", Ledger: root, ShellAllow: [][]string{{"scripts/mcheck", "*"}}, ShellAuditPath: audit}, "/usr/bin:/bin")
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"cd x && scripts/mcheck a", "FOO=1 scripts/mcheck a", "scripts/mcheck a | cat", `sh -c "scripts/mcheck a"`, "go test ./..."} {
		request, _ := json.Marshal(map[string]any{"tool_name": "Bash", "tool_input": map[string]string{"command": command}})
		result, err := PreToolDecision(strings.NewReader(string(request)), shims)
		if err != nil || !strings.Contains(string(result), `"permissionDecision":"deny"`) {
			t.Fatalf("hook allowed %s: %s %v", command, result, err)
		}
	}
	raw, err := os.ReadFile(audit)
	if err != nil || len(strings.Split(strings.TrimSpace(string(raw)), "\n")) != 5 {
		t.Fatalf("refusal audit %s %v", raw, err)
	}
	request := `{"tool_name":"Bash","tool_input":{"command":"scripts/mcheck a.yaml"}}`
	result, err := PreToolDecision(strings.NewReader(request), shims)
	if err != nil || string(result) != "{}\n" {
		t.Fatalf("allowed checker refused %s %v", result, err)
	}
}
