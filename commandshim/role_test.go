package commandshim

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

	"github.com/xleelz/fae/scheduler"
)

// The test binary doubles as the `fae heavy-op run --shim-policy` broker the
// wrappers exec, so the shim path runs end to end without building cmd/fae.
// Role wrappers reach it with COMMANDSHIM_TEST_BROKER set; account-mode
// wrappers call `fae --home DIR heavy-op run ...`, which it recognises.
const testBroker = "COMMANDSHIM_TEST_BROKER"

func TestMain(m *testing.M) {
	a := os.Args[1:]
	accounted := len(a) > 2 && a[0] == "--home" && a[2] == "heavy-op"
	if accounted {
		a = a[2:]
	}
	if os.Getenv(testBroker) == "1" || accounted {
		if len(a) < 6 || a[0] != "heavy-op" || a[1] != "run" || a[2] != "--shim-policy" || a[4] != "--" {
			fmt.Fprintln(os.Stderr, "bad broker invocation", a)
			os.Exit(2)
		}
		code, err := RunShim(context.Background(), a[3], HeavyOp{Argv: a[5:], Observe: healthyHost, Policy: fastPolicy, Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr})
		if err != nil {
			fmt.Fprintln(os.Stderr, "fae:", err)
		}
		os.Exit(code)
	}
	os.Exit(m.Run())
}

type roleFixture struct {
	worktree, shims, ledger, out string
	env                          []string
}

// roleShims installs wrappers whose real `go` records its argv and environment
// and, when asked, runs a nested `go build` through the same PATH.
func roleShims(t *testing.T, s RoleShims) roleFixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := roleFixture{worktree: filepath.Join(root, "wt"), ledger: heavyLedger(t, 2), out: filepath.Join(root, "out")}
	tools := filepath.Join(root, "tools")
	for _, d := range []string{f.worktree, filepath.Join(f.worktree, "sub"), tools, f.out} {
		if err = os.MkdirAll(d, 0700); err != nil {
			t.Fatal(err)
		}
	}
	fake := "#!/bin/sh\nn=$(ls " + f.out + " | wc -l)\nprintf '%s\\n' \"$@\" > " + f.out + "/args.$n\nenv > " + f.out + "/env.$n\nif [ -n \"$NEST\" ]; then NEST= go build .; echo $? > " + f.out + "/nested; fi\nexit 5\n"
	for _, name := range []string{"go", "gofmt", "git", "cargo"} {
		if err = os.WriteFile(filepath.Join(tools, name), []byte(fake), 0700); err != nil {
			t.Fatal(err)
		}
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	s.Broker, s.Ledger, s.Worktree = exe, f.ledger, f.worktree
	if f.shims, err = InstallRoleShims(root, s, tools+":/usr/bin:/bin"); err != nil {
		t.Fatal(err)
	}
	f.env = PrependPath([]string{"PATH=/usr/bin:/bin", testBroker + "=1", LeaseMarker + "=forged-permit", "GOFLAGS=-toolexec=/bin/sh"}, f.shims)
	return f
}

func (f roleFixture) run(t *testing.T, dir string, env []string, argv ...string) (int, string) {
	t.Helper()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir, cmd.Env = dir, append(append([]string(nil), f.env...), env...)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatal(err)
	}
	return cmd.ProcessState.ExitCode(), string(out)
}

func (f roleFixture) audit(t *testing.T) []string {
	t.Helper()
	l, err := scheduler.OpenDynamicLedger(f.ledger, 2)
	if err != nil {
		t.Fatal(err)
	}
	pool, _ := l.Snapshot(context.Background())
	if len(pool.Holders) != 0 || len(pool.Waiting) != 0 {
		t.Fatalf("lease left behind %+v", pool)
	}
	rows, _ := l.Audit(context.Background())
	out := []string{}
	for _, a := range rows {
		out = append(out, a.Action)
	}
	return out
}

func TestRoleShimRunsHeavyCommandsUnderTheLease(t *testing.T) {
	f := roleShims(t, RoleShims{Build: true, Test: true, Format: true, Local: true})
	// A whole suite, a targeted test by the wrapper's absolute path, and an
	// absolute in-worktree target all run leased with the command's own code.
	for i, c := range []struct {
		argv []string
		want string
	}{
		{[]string{"/bin/sh", "-c", "go test -parallel=99 -count=1 ./..."}, "test\n-p=1\n-parallel=1\n-count=1\n./...\n"},
		{[]string{filepath.Join(f.shims, "go"), "test", "-p", "8", "-run=TestX", "./sub"}, "test\n-p=2\n-parallel=2\n-run=TestX\n./sub\n"},
		{[]string{"/bin/sh", "-c", "go build " + filepath.Join(f.worktree, "sub")}, "build\n-p=2\n" + filepath.Join(f.worktree, "sub") + "\n"},
	} {
		code, out := f.run(t, f.worktree, nil, c.argv...)
		if code != 5 {
			t.Fatalf("%v: exit %d %s", c.argv, code, out)
		}
		args, _ := os.ReadFile(filepath.Join(f.out, fmt.Sprintf("args.%d", 2*i)))
		if string(args) != c.want {
			t.Fatalf("%v ran %q", c.argv, args)
		}
		env, _ := os.ReadFile(filepath.Join(f.out, fmt.Sprintf("env.%d", 2*i)))
		if strings.Contains(string(env), "FAE_HEAVY_OP_") || !strings.Contains(string(env), NestedMarker+"=1") || strings.Contains(string(env), "toolexec") || !strings.Contains(string(env), "GOENV=off") {
			t.Fatalf("child environment %s", env)
		}
	}
	if got := strings.Join(f.audit(t), ","); got != "queued,granted,released,queued,granted,released,queued,granted,released" {
		t.Fatal(got)
	}
	if ShimWeight([]string{"go", "test", "-p=1", "-parallel=1", "./..."}) != 2 || ShimWeight([]string{"go", "build", "-p=2", "./..."}) != 2 || ShimWeight([]string{"go", "test", "-p=2", "-parallel=2", "-run=X", "./..."}) != 1 || ShimWeight([]string{"gofmt", "-l", "a.go"}) != 1 {
		t.Fatal("weights")
	}
}

func TestRoleShimNestedInvocationIsRefused(t *testing.T) {
	f := roleShims(t, RoleShims{Build: true, Test: true})
	code, out := f.run(t, f.worktree, []string{"NEST=1"}, "/bin/sh", "-c", "go test ./sub")
	nested, _ := os.ReadFile(filepath.Join(f.out, "nested"))
	if code != 5 || strings.TrimSpace(string(nested)) != "77" || !strings.Contains(out, "FAE-HOST-014") {
		t.Fatalf("exit %d nested %q: %s", code, nested, out)
	}
	// Only the parent took a lease; the nested build never reached the ledger.
	if got := strings.Join(f.audit(t), ","); got != "queued,granted,released" {
		t.Fatal(got)
	}
}

func TestRoleShimRefusalsExit77WithoutALease(t *testing.T) {
	f := roleShims(t, RoleShims{Build: true, Test: true, Format: true, Local: true})
	pre := roleShims(t, RoleShims{Local: true})
	for _, c := range []struct {
		f      roleFixture
		dir    string
		argv   []string
		reason string
	}{
		{pre, pre.worktree, []string{"go", "build", "."}, "pre-code"},
		{pre, pre.worktree, []string{"gofmt", "-l", "."}, "pre-code"},
		{pre, pre.worktree, []string{"go", "test", "."}, "pre-code"},
		{f, filepath.Dir(f.worktree), []string{"go", "test", "./wt"}, "own worktree"},
		{f, f.worktree, []string{"go", "test", "/"}, "outside own worktree"},
		{f, f.worktree, []string{"go", "test", "../"}, "outside own worktree"},
		{f, f.worktree, []string{"git", "commit", "-m", "forbidden"}, "VCS broker"},
		{f, f.worktree, []string{"cargo", "--version"}, "scoped broker adapter"},
		{f, f.worktree, []string{"make"}, "scoped broker adapter"},
		{f, f.worktree, []string{"go", "version"}, "unsupported Go verb"},
		{f, f.worktree, []string{"go", "test", "-exec=/bin/sh", "."}, "unsupported Go flag"},
	} {
		code, out := c.f.run(t, c.dir, nil, append([]string{"/bin/sh", "-c", `"$0" "$@"`}, c.argv...)...)
		if code != Refused || !strings.Contains(out, c.reason) {
			t.Fatalf("%v: exit %d %s", c.argv, code, out)
		}
	}
	// FAE-MODE-002: a Local push or remote change has its own exit code and
	// says why, behind any of git's global options, in a profile whose shims
	// omit git, and nested in a leased command; other git commands keep 77.
	gitless := roleShims(t, RoleShims{Test: true, Local: true, Shims: []string{"go"}})
	for _, c := range []struct {
		f    roleFixture
		env  []string
		argv []string
	}{
		{f, nil, []string{"git", "push"}},
		{f, nil, []string{"git", "remote", "add", "x", "y"}},
		{f, nil, []string{"git", "-C", "sub", "push", "origin"}},
		{f, nil, []string{"git", "-c", "push.default=current", "push"}},
		{f, nil, []string{"git", "--git-dir", ".git", "--work-tree=.", "--no-pager", "remote", "-v"}},
		{f, nil, []string{"git", "-c", "k=v", "-C", ".", "--namespace", "n", "--bare", "push"}},
		{gitless, nil, []string{"git", "push"}},
		{f, []string{NestedMarker + "=1"}, []string{"git", "-C", "sub", "push"}},
	} {
		code, out := c.f.run(t, c.f.worktree, c.env, append([]string{"/bin/sh", "-c", `"$0" "$@"`}, c.argv...)...)
		if code != LocalModeRefused || LocalModeRefused == Refused || !strings.Contains(out, "Local mode") || !strings.Contains(out, "no external effect") {
			t.Errorf("%v: exit %d %s", c.argv, code, out)
		}
	}
	for _, argv := range [][]string{{"git", "commit", "-m", "forbidden"}, {"git", "checkout", "-b", "forbidden"}, {"git", "worktree", "add", "forbidden"}} {
		if code, out := f.run(t, f.worktree, nil, append([]string{"/bin/sh", "-c", `"$0" "$@"`}, argv...)...); code != Refused || !strings.Contains(out, "VCS broker") {
			t.Errorf("%v: exit %d %s", argv, code, out)
		}
	}
	// A RAM-backed build output directory and a tool missing from the base
	// PATH are refused with 77 naming the reason.
	s, err := ReadRoleShims(filepath.Join(f.shims, ".policy.json"))
	if err != nil {
		t.Fatal(err)
	}
	s.RAMRoots = []string{filepath.Join(f.worktree, "ram")}
	os.Mkdir(s.RAMRoots[0], 0700)
	delete(s.Tools, "gofmt")
	raw, _ := json.Marshal(s)
	policy := filepath.Join(t.TempDir(), "policy.json")
	os.WriteFile(policy, raw, 0600)
	for argv, reason := range map[string]string{"go build -o ram/bin .": "RAM-backed", "gofmt -l .": "not on the session's base PATH"} {
		code, err := RunShim(context.Background(), policy, HeavyOp{Argv: strings.Fields(argv), Dir: f.worktree, Env: []string{"PATH=/usr/bin:/bin"}, Observe: healthyHost, Policy: fastPolicy})
		if code != Refused || err == nil || !strings.Contains(err.Error(), reason) {
			t.Fatal(argv, code, err)
		}
	}
	// An accounted call with an explicit Dir resolves Dir and changes to it
	// before exec so the policy check and exec share the same working directory.
	origDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origDir)
	subDir := filepath.Join(f.worktree, "sub")
	if code, err := runAccounted(context.Background(), RoleShims{Worktree: f.worktree, Tools: map[string]string{"go": filepath.Join(f.worktree, "missing", "go")}}, HeavyOp{Argv: []string{"go", "version"}, Dir: subDir}); code != 126 || err == nil {
		t.Fatalf("accounted missing binary: %d %v", code, err)
	}
	if cwd, err := os.Getwd(); err != nil || cwd != subDir {
		t.Fatalf("runAccounted did not chdir to %s: cwd=%s err=%v", subDir, cwd, err)
	}
	for _, x := range []roleFixture{f, pre, gitless} {
		if got := x.audit(t); len(got) != 0 {
			t.Fatal("refusal touched the lease", got)
		}
		if entries, _ := os.ReadDir(x.out); len(entries) != 0 {
			t.Fatal("refused command ran")
		}
	}
}

func TestRoleShimsWithoutABrokerFailClosed(t *testing.T) {
	dir, err := InstallRoleShims(t.TempDir(), RoleShims{Build: true, Test: true}, "/usr/bin:/bin")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", "go test .")
	cmd.Env = PrependPath([]string{"PATH=/usr/bin:/bin"}, dir)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != Refused || !strings.Contains(string(out), "broker dispatch") {
		t.Fatalf("%s %v", out, err)
	}
}

func TestPreToolHookDeniesAbsolutePathBypass(t *testing.T) {
	shims := "/s/sessions/session-1/command-shims"
	for command, deny := range map[string]bool{
		"go test ./...":                          false,
		"cd sub && go build .":                   false,
		shims + "/go test ./...":                 false,
		"/usr/local/go/bin/go test ./...":        true,
		"ls && \"/usr/local/go/bin/gofmt\" -l .": true,
		"$GOROOT/bin/go build .":                 true,
		"./go build .":                           true,
		"x=$(/usr/bin/git push)":                 true,
		"PATH=/usr/local/go/bin go test .":       true,
		"export PATH=/usr/bin; go test .":        true,
		"env -i go test .":                       true,
		"command -p go version":                  true,
		"cat /etc/hosts | grep go":               false,
		"/usr/bin/env":                           false,
		// Quoting and escapes inside a word: the shell still runs go.
		`/usr/bin/g''o test ./...`:    true,
		`/usr/bin/g""o test ./...`:    true,
		`/usr/bin/g\o test ./...`:     true,
		`/usr/bin/\g\o\f\m\t -l .`:    true,
		`/usr/bin/"g"o test ./...`:    true,
		`"/usr/bin/"go test ./...`:    true,
		`/usr/bin/$'\x67o' test .`:    true,
		`/usr/bin/$'\147\157' test .`: true,
		`/usr/bin/g$'o' test .`:       true,
		`/usr/bin/$"git" push`:        true,
		"/usr/bin/g\\\no test .":      true,
		`e''nv -i go test .`:          true,
		`\env -u PATH go test .`:      true,
		`co""mmand -p go version`:     true,
		// Ordinary commands with quoting still pass.
		`"go" test ./...`:                      false,
		`g''o test -run 'TestA|TestB' ./pkg`:   false,
		`printf 'a/b\n' > out.txt`:             false,
		`grep -rn "func main" ./cmd`:           false,
		`echo "don't" && go vet ./...`:         false,
		`git -c "user.name=x y" commit -m 'm'`: false,
		shims + `/"go" test ./...`:             false,
	} {
		if _, got := BypassesShim(command, shims); got != deny {
			t.Errorf("%q: deny=%t", command, got)
		}
	}
	out, err := PreToolDecision(strings.NewReader(`{"tool_name":"Bash","tool_input":{"command":"/usr/local/go/bin/go test ./..."}}`), shims)
	if err != nil || !strings.Contains(string(out), `"permissionDecision":"deny"`) || !strings.Contains(string(out), "by name") {
		t.Fatalf("%s %v", out, err)
	}
	out, err = PreToolDecision(strings.NewReader(`{"tool_name":"Bash","tool_input":{"command":"/usr/bin/g''o test ./..."}}`), shims)
	if err != nil || !strings.Contains(string(out), `"permissionDecision":"deny"`) || !strings.Contains(string(out), "g''o") {
		t.Fatalf("%s %v", out, err)
	}
	if out, err = PreToolDecision(strings.NewReader(`{"tool_name":"Bash","tool_input":{"command":"go test ./..."}}`), shims); err != nil || string(out) != "{}\n" {
		t.Fatalf("%s %v", out, err)
	}
	if _, err = PreToolDecision(strings.NewReader(`not json`), shims); err == nil {
		t.Fatal("malformed hook input accepted")
	}
}

// FAE-HOST-011: literal PATH resets and absolute-path runs the hook denies,
// in every spelling the shell or env accepts, beside ordinary commands that
// must still pass. The check stays lexical: an expansion it cannot evaluate
// (/usr/bin/$x) is outside it.
func TestPreToolHookDeniesLiteralPathResets(t *testing.T) {
	shims := "/s/sessions/session-1/command-shims"
	for _, tc := range []struct {
		command string
		deny    bool
	}{
		// env named by path, and every option form that clears the
		// environment, unsets PATH or sets a search path.
		{"/usr/bin/env -i go test .", true},
		{"env - go test .", true},
		{"env -iv go test .", true},
		{"env -vi go test .", true},
		{"env -0i go test .", true},
		{"env --ignore-env go test .", true},
		{"env --ignore-environment go test .", true},
		{"env --ig go test .", true},
		{"env -uPATH go test .", true},
		{"env --unset=PATH go test .", true},
		{"env --un=PATH go test .", true},
		{"env -v -uPATH go test .", true},
		{"env -C / -i go test .", true},
		{"env -C/ -i go test .", true},
		{"env --chdir / -i go test .", true},
		{"env --chdir=/ -i go test .", true},
		{"env -u GOFLAGS -i go test .", true},
		{"env -S'-i go test .'", true},
		{`env -S "-i go test ."`, true},
		{"env -vS-i go test .", true},
		{"env --split-string='-i go test .'", true},
		{"env -P /usr/local/go/bin go test .", true},
		{"nice env -i go test .", true},
		{`/usr/bin/e''nv -i go test .`, true},
		{"x=$(env -i go version)", true},
		// exec and command forms.
		{"exec -c go test .", true},
		{"exec -lc go test .", true},
		{"exec -l bash -c 'go test .'", true},
		{"exec -a -bash bash -c 'go test .'", true},
		{`\command -p go version`, true},
		{"command -pv go", true},
		// Brace expansion.
		{"{/usr/bin/go,build}", true},
		{"{/usr/bin/go,test} ./...", true},
		{"/usr/bin/{go,true} test .", true},
		{"/usr/{bin,local/go/bin}/go test .", true},
		{"/usr/bin/{g,x}o test .", true},
		{"/usr/bin/{f..h}o test .", true},
		{"{/usr/bin/{g,x}o,y} test .", true},
		{"{env,-i} go test .", true},
		{"{a,b}{a,b}{a,b}{a,b}{a,b}{a,b}{a,b}{a,b}{a,b}{a,b}{a,b}", true},
		// Sourcing a file that may reset PATH, and shells that read profiles.
		{". /etc/environment; go test .", true},
		{"source /etc/environment && go test .", true},
		{"source .venv/bin/activate && pytest", true},
		{"GOFLAGS= . ./env.sh; go build .", true},
		{"{ . /etc/environment; go build .; }", true},
		{"if . /etc/environment; then go build .; fi", true},
		{">/dev/null . /etc/environment; go build .", true},
		{"2> err . /etc/environment; go build .", true},
		{"builtin source /etc/environment", true},
		{"eval '. /etc/environment'; go build .", true},
		{"bash -c '. /etc/environment; go build .'", true},
		{`sh -ec "source /etc/environment; go build ."`, true},
		{"bash -l -c 'go build .'", true},
		{"bash -lc 'go build .'", true},
		{"bash --login -c 'go build .'", true},
		{"/bin/bash --rcfile /etc/environment -c 'go build .'", true},
		{"bash -o pipefail -i", true},
		{"BASH_ENV=/etc/environment bash -c 'go build .'", true},
		{"export BASH_ENV; bash -c 'go build .'", true},
		// Ordinary commands still pass.
		{"go test ./...", false},
		{"env GOFLAGS=-mod=mod go test ./...", false},
		{"env -u GOFLAGS go test .", false},
		{"env -C sub go test .", false},
		{"env -v go build .", false},
		{"/usr/bin/env go version", false},
		{"env -S 'go test ./...'", false},
		{"env FOO=1 -i", false},
		{"exec go test ./...", false},
		{"exec -a mygo go version", false},
		{"command -v go", false},
		{"bash -c 'go test ./...'", false},
		{"sh -e -c 'go vet ./...'", false},
		{"bash --norc -c 'go vet ./...'", false},
		{"bash script.sh -l", false},
		{"find . -name '*.go'", false},
		{"go build . && ls .", false},
		{"cd . && go vet ./...", false},
		{"grep -c '.' notes.txt", false},
		{`grep -c "source" notes.txt`, false},
		{"grep -rn source ./pkg", false},
		{"echo {a,b} && mkdir -p dir/{x,y}", false},
		{"for c in {a..e}; do echo $c; done", false},
		{"for i in {1..100000}; do go test -count=1 .; done", false},
		{"echo ${GOPATH,,}/bin", false},
		{"go test -run 'TestA|TestB' {./a,./b}/...", false},
		{shims + "/{go,gofmt} test .", false},
	} {
		if _, got := BypassesShim(tc.command, shims); got != tc.deny {
			t.Errorf("%q: deny=%t, want %t", tc.command, got, tc.deny)
		}
	}
	if reason, _ := BypassesShim("source .venv/bin/activate && pytest", shims); !strings.Contains(reason, "sourced file") {
		t.Errorf("sourcing reason %q", reason)
	}
	if reason, _ := BypassesShim("{/usr/bin/go,build}", shims); !strings.Contains(reason, "go by path") {
		t.Errorf("brace reason %q", reason)
	}
}

// FAE-HOST-011: three more literal bypasses the hook denies. A git or cargo
// dashed subcommand run as a command, or named by path in git's exec
// directory, skips the git shim and its Local push refusal; a Go toolchain
// internal by path skips the go shim's clamp; and systemd-run runs its
// command under the service manager with the manager's PATH. File names that
// merely start with git- and ordinary uses of git, go and the toolchain pass.
func TestPreToolHookDeniesDashedSubcommandsToolchainInternalsAndSystemdRun(t *testing.T) {
	shims := "/s/sessions/session-1/command-shims"
	for _, tc := range []struct {
		command string
		deny    bool
	}{
		// git's dashed builtins and cargo's dashed subcommands.
		{"/usr/lib/git-core/git-push origin", true},
		{"/usr/libexec/git-core/git-remote add x y", true},
		{"/opt/homebrew/opt/git/libexec/git-core/git-send-pack ../x", true},
		{"cd /tmp && '/usr/lib/git-core/git-push' origin", true},
		{"/usr/lib/git-core/git-{push,status} origin", true},
		{"xargs /usr/lib/git-core/git-push < remotes", true},
		{"git-push origin", true},
		{"$HOME/bin/git-lfs push origin", true},
		{"~/.cargo/bin/cargo-clippy", true},
		{"PATH_X=1 git-receive-pack .", true},
		// Go toolchain internals by path.
		{"$GOROOT/pkg/tool/linux_amd64/compile -o x.a x.go", true},
		{"/usr/local/go/pkg/tool/linux_amd64/link -o app x.a", true},
		{"$(go env GOROOT)/pkg/tool/darwin_arm64/asm x.s", true},
		{"nice /usr/lib/go/pkg/tool/linux_arm64/vet cfg", true},
		// systemd-run, which uses the service manager's PATH.
		{"systemd-run --user -P go build .", true},
		{"systemd-run --user --scope go test ./...", true},
		{"/usr/bin/systemd-run --user -t bash", true},
		{"nohup systemd-run --user make", true},
		// Ordinary commands still pass.
		{"git push origin", false},
		{"git status && go build ./...", false},
		{"cat docs/git-workflow.md", false},
		{"cat docs/git-notes", false},
		{"cp scripts/git-hooks.sh .", false},
		{"ls /usr/lib/git-core", false},
		{"go tool compile -help", false},
		{"ls $GOROOT/pkg/tool", false},
		{"go build -o pkg/tool/out ./cmd/x", false},
		{"grep -rn systemd ./docs", false},
	} {
		if _, got := BypassesShim(tc.command, shims); got != tc.deny {
			t.Errorf("%q: deny=%t, want %t", tc.command, got, tc.deny)
		}
	}
	for command, reason := range map[string]string{
		"/usr/lib/git-core/git-push origin":         "runs git push without the git shim; call git push instead",
		"$GOROOT/pkg/tool/linux_amd64/compile x.go": "Go toolchain internal $GOROOT/pkg/tool/linux_amd64/compile by path bypasses the go shim",
		"systemd-run --user -P go build .":          "systemd-run runs its command under the service manager",
	} {
		if got, _ := BypassesShim(command, shims); !strings.Contains(got, reason) {
			t.Errorf("%q: reason %q", command, got)
		}
	}
}
