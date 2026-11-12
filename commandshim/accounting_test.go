package commandshim

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// accountedRun is an account-mode run's worktree and scratch directory, both
// canonical, under root.
func accountedRun(t *testing.T) (root, worktree, scratch string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	worktree, scratch = filepath.Join(root, "wt"), filepath.Join(root, "scratch")
	for _, d := range []string{worktree, filepath.Join(worktree, "sub"), scratch} {
		if err = os.MkdirAll(d, 0700); err != nil {
			t.Fatal(err)
		}
	}
	return root, worktree, scratch
}

// installAccountedForTest installs account-mode wrappers for policy s whose
// broker is this test binary (TestMain), as the effects broker installs them
// with the fae binary.
func installAccountedForTest(t *testing.T, parent string, s RoleShims, searchPath string) (shims, records string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	s.Broker = exe
	// As the broker does, pin the programs nested go finds by name for cgo
	// to the search path's (NestedGoPrograms).
	nested := NestedGo{}
	if s.NestedGo != nil {
		nested = *s.NestedGo
	}
	if nested.Programs == nil {
		nested.Programs = NestedGoPrograms([]string{"PATH=" + searchPath})
	}
	s.NestedGo = &nested
	shims, records, err = InstallAccounting(parent, s, searchPath)
	if err != nil {
		t.Fatal(err)
	}
	return shims, records
}

// FAE-HOST-014 accounting: a wrapper records each call exactly (arguments with
// quotes, command substitutions and newlines, from an awkward directory) and
// runs the real tool, through the broker's policy, with its exit code.
// Broker-chosen paths with shell metacharacters stay literal, and nothing the
// call names is ever evaluated.
func TestAccountingWrapperRecordsExactCallsAndExecs(t *testing.T) {
	root, worktree, scratch := accountedRun(t)
	awkward := "it's \"$(touch pwned-a)\" `touch pwned-b`\nline;$HOME"
	parent := filepath.Join(root, "parent "+awkward)
	bin := filepath.Join(root, "bin "+awkward)
	cwd := filepath.Join(root, "cwd "+awkward)
	for _, d := range []string{parent, bin, cwd} {
		if err := os.Mkdir(d, 0700); err != nil {
			t.Fatal(err)
		}
	}
	// The real tool echoes its arguments, one per line, and exits 3.
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nfor a in \"$@\"; do printf '<%s>\\n' \"$a\"; done\nexit 3\n"), 0700); err != nil {
		t.Fatal(err)
	}
	shims, records := installAccountedForTest(t, parent, RoleShims{Worktree: worktree, Scratch: scratch, Local: true}, "relative:"+bin+":/usr/bin:/bin")
	// git version runs anywhere, so the policy passes the rest unread.
	args := []string{"version", "it's \"$(touch pwned-c)\"", "two\nlines; touch pwned-d", "$(id)", "", "-x"}
	cmd := exec.Command(filepath.Join(shims, "git"), args...)
	cmd.Dir = cwd
	cmd.Env = []string{"PATH=" + shims + ":" + bin + ":/usr/bin:/bin", "FAE_ACCOUNTING_TEST=kept"}
	out, err := cmd.Output()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 3 {
		t.Fatalf("real tool exit not passed through: %v %s", err, out)
	}
	want := ""
	for _, a := range args {
		want += "<" + a + ">\n"
	}
	if string(out) != want {
		t.Fatalf("real tool got %q, want %q", out, want)
	}
	count, calls, err := ReadAccounted(records, 16)
	if err != nil || count != 1 || len(calls) != 1 {
		t.Fatal(count, calls, err)
	}
	long := false // the directory summary is cut under a long TMPDIR
	if got := calls[0]; !reflect.DeepEqual(got.Argv, append([]string{"git"}, args...)) || got.Dir != boundNestedField(cwd, nestedCallDirBytes, &long) || got.Truncated != long || got.At.IsZero() {
		t.Fatalf("recorded %+v", got)
	}
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err == nil && strings.HasPrefix(d.Name(), "pwned") {
			t.Errorf("argument evaluated: %s", path)
		}
		return nil
	})
	for _, d := range []string{"", cwd} {
		if matches, _ := filepath.Glob(filepath.Join(d, "pwned*")); len(matches) != 0 {
			t.Errorf("argument evaluated: %v", matches)
		}
	}
}

// Concurrent calls each get a record of their own; the count is exact while
// the summaries stop at the limit, in call order, each argument bounded.
func TestAccountingCountsConcurrentCallsAndBoundsSummaries(t *testing.T) {
	root, worktree, scratch := accountedRun(t)
	shims, records := installAccountedForTest(t, root, RoleShims{Worktree: worktree, Scratch: scratch}, "/usr/bin:/bin")
	script := `git --version >/dev/null; i=0; while [ $i -lt 12 ]; do git --version >/dev/null & i=$((i+1)); done; wait`
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Env = []string{"PATH=" + shims + ":/usr/bin:/bin"}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	count, calls, err := ReadAccounted(records, 4)
	if err != nil || count != 13 || len(calls) != 4 {
		t.Fatal(count, len(calls), err)
	}
	for i, c := range calls {
		if !reflect.DeepEqual(c.Argv, []string{"git", "--version"}) || (i > 0 && c.At.Before(calls[i-1].At)) {
			t.Fatalf("summary %d: %+v", i, c)
		}
	}
	long := "a" + strings.Repeat("é", 200) // a 256-byte cut falls inside a rune
	many := []string{long}
	for i := 0; i < 30; i++ {
		many = append(many, "arg")
	}
	if out, err := exec.Command(filepath.Join(shims, "git"), append(many, "--version")...).CombinedOutput(); err == nil {
		t.Fatalf("git accepted unknown arguments: %s", out)
	}
	count, calls, err = ReadAccounted(records, 100)
	if err != nil || count != 14 {
		t.Fatal(count, err)
	}
	var last NestedCall
	for _, c := range calls {
		if len(c.Argv) > 2 {
			last = c
		}
	}
	if !last.Truncated || len(last.Argv) != nestedCallArgs || len(last.Argv[1]) > nestedCallArgBytes || !strings.HasPrefix(long, last.Argv[1]) || !strings.HasSuffix(last.Argv[1], "é") {
		t.Fatalf("unbounded summary: %d args, first %q", len(last.Argv), last.Argv[1])
	}
}

// A tool missing from the search path is still recorded, then fails with 127
// and says so; a call whose record cannot be written is refused with 77.
func TestAccountingMissingToolAndUnrecordableCall(t *testing.T) {
	root, worktree, scratch := accountedRun(t)
	shims, records := installAccountedForTest(t, root, RoleShims{Worktree: worktree, Scratch: scratch}, t.TempDir())
	cmd := exec.Command(filepath.Join(shims, "cargo"), "--version")
	cmd.Env = []string{"PATH=" + shims}
	out, err := cmd.CombinedOutput()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 127 || !strings.Contains(string(out), "fae: nested cargo not found on the base PATH (exit 127)") {
		t.Fatalf("missing tool: %v %s", err, out)
	}
	if count, calls, err := ReadAccounted(records, 16); err != nil || count != 1 || !reflect.DeepEqual(calls[0].Argv, []string{"cargo", "--version"}) {
		t.Fatal(count, calls, err)
	}
	if err = os.RemoveAll(records); err != nil {
		t.Fatal(err)
	}
	out, err = exec.Command(filepath.Join(shims, "cargo"), "--version").CombinedOutput()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 77 || !strings.Contains(string(out), "fae: nested cargo refused (exit 77): its accounting record could not be written") {
		t.Fatalf("unrecordable call: %v %s", err, out)
	}
}

// FAE-HOST-014's accounted branch, FAE-HOST-012/-015 and FAE-MODE-002: an
// account-mode run's nested call passes through the same shim. A nested go is
// clamped as a top-level engine command is (whole suite 1, otherwise 2, the
// call's own -p, -parallel and -test.parallel dropped) and confined to the
// run's worktree and scratch directory; a nested gofmt gets the formatter
// limits; a nested git never pushes or changes a remote (78 for a Local
// task, 77 otherwise). Every call is recorded, refused or not, and a refused
// call never reaches the real tool.
func TestAccountedNestedCallsPassTheShimPolicy(t *testing.T) {
	root, worktree, scratch := accountedRun(t)
	tools, ram := filepath.Join(root, "tools"), filepath.Join(worktree, "ram")
	for _, d := range []string{tools, ram} {
		if err := os.Mkdir(d, 0700); err != nil {
			t.Fatal(err)
		}
	}
	// Each real tool names itself, prints its arguments one per line and the
	// GOFLAGS it got, and exits 5.
	fake := "#!/bin/sh\nprintf 'ran %s\\n' \"${0##*/}\"\nprintf '%s\\n' \"$@\"\nprintf 'GOFLAGS=%s\\n' \"$GOFLAGS\"\nexit 5\n"
	for _, name := range []string{"go", "gofmt", "git", "cargo"} {
		write(t, filepath.Join(tools, name), fake)
		if err := os.Chmod(filepath.Join(tools, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(worktree, "small.go"), "package x\n")
	write(t, filepath.Join(worktree, "huge.go"), strings.Repeat("x", (1<<20)+1))
	install := func(local bool) (string, string) {
		parent := filepath.Join(root, fmt.Sprintf("policy-local-%t", local))
		if err := os.Mkdir(parent, 0700); err != nil {
			t.Fatal(err)
		}
		return installAccountedForTest(t, parent, RoleShims{Worktree: worktree, Scratch: scratch, Local: local, RAMRoots: []string{ram}}, tools+":/usr/bin:/bin")
	}
	local, localRecords := install(true)
	normal, normalRecords := install(false)
	run := func(shims, dir, script string) (int, string) {
		t.Helper()
		cmd := exec.Command("/bin/sh", "-c", script)
		cmd.Dir = dir
		cmd.Env = []string{"PATH=" + shims + ":/usr/bin:/bin", "S=" + scratch}
		out, err := cmd.CombinedOutput()
		var exit *exec.ExitError
		if err != nil && !errors.As(err, &exit) {
			t.Fatal(err)
		}
		return cmd.ProcessState.ExitCode(), string(out)
	}
	calls := map[string]int{}
	for _, c := range []struct {
		shims, dir, script string
		want               []string
	}{
		// The clamp, as Prepare applies it to a top-level engine command.
		{local, worktree, "go test -parallel=99 -p 8 -json ./...", []string{"go", "test", "-p=1", "-parallel=1", "-json", "./..."}},
		{local, worktree, "go test -run TestX -parallel 7 ./sub -args -test.parallel=9 -v", []string{"go", "test", "-p=2", "-parallel=2", "-run", "TestX", "./sub", "-args", "-v"}},
		{local, worktree, `go build --p=64 -o "$S/bin/x" ./sub`, []string{"go", "build", "-p=2", "-o", filepath.Join(scratch, "bin", "x"), "./sub"}},
		{local, scratch, "go list -deps -f '{{.ImportPath}}' example.com/x", []string{"go", "list", "-p=2", "-deps", "-f", "{{.ImportPath}}", "example.com/x"}},
		{local, worktree, "GOFLAGS=-mod=mod go vet .", []string{"go", "vet", "-p=2", ".", "GOFLAGS=-mod=mod"}},
		// Read-only probes run as called, from anywhere.
		{local, root, "go version", []string{"go", "version"}},
		{local, root, "go env GOPATH", []string{"go", "env", "GOPATH"}},
		// A bounded formatter target is expanded to its absolute file.
		{local, worktree, "gofmt -l small.go", []string{"gofmt", "-l", filepath.Join(worktree, "small.go")}},
		// git version needs no repository; accounted_git_test.go runs git on real ones.
		{normal, worktree, "git -c user.name=x version", []string{"git", "-c", "user.name=x", "version"}},
	} {
		code, out := run(c.shims, c.dir, c.script)
		calls[c.shims]++
		want := "ran " + c.want[0] + "\n" + strings.Join(c.want[1:], "\n") + "\n"
		if !strings.Contains(c.want[len(c.want)-1], "GOFLAGS=") {
			want += "GOFLAGS=\n"
		}
		if code != 5 || out != want {
			t.Errorf("%s: exit %d, ran\n%s\nwant\n%s", c.script, code, out, want)
		}
	}
	for _, c := range []struct {
		shims, dir, script string
		code               int
		reason             string
	}{
		{local, worktree, "go build -toolexec=/bin/sh .", Refused, "nested go flag -toolexec runs other programs"},
		{local, worktree, "GOFLAGS='-mod=mod -exec=/bin/sh' go test .", Refused, "nested go flag -exec runs other programs"},
		{local, worktree, "go vet -vettool=/bin/sh .", Refused, "nested go flag -vettool"},
		{local, worktree, "go -C / build .", Refused, `unsupported nested Go verb "-C"`},
		{local, worktree, "go mod tidy", Refused, `unsupported nested Go verb "mod"`},
		{local, worktree, "go env -w GOFLAGS=-p=64", Refused, "go env -w and -u change the user's Go environment"},
		{local, worktree, "go test /", Refused, "target outside own worktree: /"},
		{local, worktree, "go test ../...", Refused, "target outside own worktree"},
		{local, worktree, "go build -o ram/out .", Refused, "RAM-backed temporary build output"},
		{local, worktree, "go build -o ../out .", Refused, "target outside own worktree"},
		{local, worktree, "go test -p", Refused, "missing parallelism value"},
		{local, root, "go build .", Refused, "outside the run's worktree and scratch directory"},
		{local, root, "gofmt -l wt/small.go", Refused, "outside the run's worktree and scratch directory"},
		{local, worktree, "gofmt -l huge.go", Refused, "exceeds 1 MiB; use item-scoped formatting"},
		{local, worktree, "gofmt -l", Refused, "formatter requires explicit per-file targets"},
		{local, worktree, "gofmt -r 'a -> b' small.go", Refused, "unsupported formatter flag"},
		{local, worktree, "make build", Refused, "unsupported build wrapper"},
		{local, worktree, "git push origin HEAD", LocalModeRefused, "Local mode"},
		{local, worktree, "git -C . remote add x y", LocalModeRefused, "Local mode"},
		{local, worktree, "git send-pack ../x", LocalModeRefused, "Local mode"},
		{local, worktree, "git -c k=v http-push u", LocalModeRefused, "Local mode"},
		{normal, worktree, "git push", Refused, "an engine run makes no external effect outside the daemon's effect grants"},
		{normal, worktree, "git --git-dir .git remote set-url origin x", Refused, "pushes and remote changes are refused"},
	} {
		code, out := run(c.shims, c.dir, c.script)
		calls[c.shims]++
		if code != c.code || !strings.Contains(out, c.reason) || strings.Contains(out, "ran ") {
			t.Errorf("%s: exit %d %s", c.script, code, out)
		}
	}
	for shims, records := range map[string]string{local: localRecords, normal: normalRecords} {
		if count, _, err := ReadAccounted(records, 1); err != nil || count != calls[shims] {
			t.Errorf("%s: %d calls recorded, made %d: %v", records, count, calls[shims], err)
		}
	}
}
