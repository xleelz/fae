package effects

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// goTestParentFixture writes a Go test into w that starts go and gofmt as an
// ordinary Go test does, with exec.Command and the PATH go test gives it
// ($GOROOT/bin first), and prints each call's exit code and output on one
// line: a nested go test of ./inner (whose test prints its -test.parallel and
// the go it would start), gofmt on a file over the formatter's 1 MiB limit,
// and go env from a child whose environment the test cleared.
func goTestParentFixture(t *testing.T, dir string) {
	t.Helper()
	if err := os.Mkdir(filepath.Join(dir, "inner"), 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "parent_test.go", `package test

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func nested(name string, env []string, args ...string) {
	cmd := exec.Command(name, args...)
	if env != nil {
		cmd.Env = env
	}
	out, err := cmd.CombinedOutput()
	code := 0
	if exit, ok := err.(*exec.ExitError); ok {
		code = exit.ExitCode()
	} else if err != nil {
		code = -1
	}
	fmt.Printf("nested %s %s exit=%d out=[%s]\n", name, args[0], code, strings.Join(strings.Fields(string(out)), " "))
}

func TestParent(t *testing.T) {
	gobin, _ := exec.LookPath("go")
	fmt.Printf("parent go=%s\n", gobin)
	nested("go", nil, "test", "-count=1", "-v", "-parallel", "64", "./inner")
	huge := filepath.Join(t.TempDir(), "huge.go")
	if err := os.WriteFile(huge, bytes.Repeat([]byte("x"), 1<<20+1), 0o600); err != nil {
		t.Fatal(err)
	}
	nested("gofmt", nil, "-l", huge)
	nested("go", []string{}, "env", "GOROOT", "GOTOOLCHAIN", "GOENV")
}
`)
	write(t, dir, "inner/inner_test.go", `package inner

import (
	"flag"
	"os/exec"
	"testing"
)

func TestInner(t *testing.T) {
	gobin, _ := exec.LookPath("go")
	t.Logf("inner parallel=%s go=%s", flag.Lookup("test.parallel").Value, gobin)
}
`)
}

// goTestOutput is the text a go test -json run printed, its events joined.
func goTestOutput(t *testing.T, stream string) string {
	t.Helper()
	var b strings.Builder
	for _, line := range strings.Split(stream, "\n") {
		var e struct{ Action, Output string }
		if json.Unmarshal([]byte(line), &e) == nil && e.Action == "output" {
			b.WriteString(e.Output)
		}
	}
	return b.String()
}

// FAE-HOST-014 refuse mode with a real Go test as the parent: go test starts
// its test binary with $GOROOT/bin first on PATH, and the broker runs go
// under the run's GOROOT mirror, so the go and gofmt the test starts are the
// run's refusal wrappers and exit 77, even from a child whose environment the
// test cleared.
func TestBrokerRefusesGoAndGofmtStartedByARealGoTest(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain unavailable")
	}
	b, w, _ := setup(t)
	goTestParentFixture(t, w.Path)
	b.Allowed = [][]string{{"go", "test", "-json", "-count=1", "-run=^TestParent$", "."}}
	r, err := b.Run(context.Background(), w, Command{Argv: b.Allowed[0], Timeout: 5 * time.Minute})
	if err != nil {
		t.Fatalf("%v %s", err, r.Stderr)
	}
	out := goTestOutput(t, r.Output)
	for _, want := range []string{"/goroot/bin/go\n", "nested go test exit=77 out=[fae: command refused (exit 77)", "nested gofmt -l exit=77 out=[fae: command refused (exit 77)", "nested go env exit=77 out=[fae: command refused (exit 77)"} {
		if !strings.Contains(out, want) {
			t.Errorf("refuse mode: no %q in\n%s", want, out)
		}
	}
	if r.ExitCode != 0 || r.NestedCount != 0 {
		t.Errorf("exit %d, %d nested calls accounted in refuse mode", r.ExitCode, r.NestedCount)
	}
}

// FAE-HOST-014's accounted branch, FAE-HOST-012 and FAE-HOST-015 with a real
// Go test as the parent: the go and gofmt it starts reach the accounting
// wrappers, so each call is recorded and passes PrepareAccounted. The nested
// go test is clamped (its test sees -test.parallel=2, not 64) and its own
// test again finds the run's wrapper first; the nested gofmt over 1 MiB is
// refused; and go from a cleared environment runs with the run's GOROOT
// mirror and GOTOOLCHAIN=local (and GOENV=off, which go env prints as
// nothing).
func TestBrokerAccountsGoAndGofmtStartedByARealGoTest(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain unavailable")
	}
	b, w, _ := setup(t)
	goTestParentFixture(t, w.Path)
	b.Allowed = [][]string{{"go", "test", "-json", "-count=1", "-run=^TestParent$", "."}}
	accountMode(t, b)
	r, err := b.Run(nestedRunContext(true), w, Command{Argv: b.Allowed[0], Timeout: 5 * time.Minute})
	if err != nil {
		t.Fatalf("%v %s", err, r.Stderr)
	}
	out := goTestOutput(t, r.Output)
	mirror := filepath.Join(filepath.Dir(w.Path), "command-policy-")
	for _, want := range []string{
		"parent go=" + mirror, "/goroot/bin/go\n",
		"nested go test exit=0 out=[", "inner parallel=2 go=" + mirror,
		"nested gofmt -l exit=77 out=[command refused (exit 77):", "exceeds 1 MiB; use item-scoped formatting",
		"nested go env exit=0 out=[" + mirror, "/goroot local]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("account mode: no %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "inner parallel=64") {
		t.Error("nested go test ran unclamped")
	}
	calls := map[string]bool{}
	for _, c := range r.Nested {
		calls[strings.Join(c.Argv, " ")] = true
	}
	for _, want := range []string{"go test -count=1 -v -parallel 64 ./inner", "gofmt -l", "go env GOROOT GOTOOLCHAIN GOENV"} {
		found := calls[want]
		for c := range calls {
			found = found || want == "gofmt -l" && strings.HasPrefix(c, want+" ")
		}
		if !found {
			t.Errorf("%q not accounted: %d calls %v", want, r.NestedCount, calls)
		}
	}
	if r.ExitCode != 0 || r.NestedCount != 3 {
		t.Errorf("exit %d, %d nested calls, want 3: %v", r.ExitCode, r.NestedCount, calls)
	}
}

// FAE-HOST-014: an account-mode nested go runs under the run's GOROOT mirror
// and never switches toolchains. A call that points GOROOT at the real
// toolchain, whose bin/ go would put ahead of the wrappers, or names another
// toolchain is refused with 77; a plain call reports the run's mirror.
func TestBrokerAccountedGoKeepsTheRunsGoRoot(t *testing.T) {
	out, err := exec.Command("go", "env", "GOROOT").Output()
	if err != nil {
		t.Skip("go toolchain unavailable", err)
	}
	b, w, _ := setup(t)
	script := `GOROOT="$1" go version; echo "goroot=$?"; GOTOOLCHAIN=go1.99.0+path go version; echo "toolchain=$?"; go env GOROOT; echo "env=$?"`
	b.Allowed = [][]string{{"/bin/sh", "-c", script, "sh", strings.TrimSpace(string(out))}}
	accountMode(t, b)
	r, err := b.Run(nestedRunContext(true), w, Command{Argv: b.Allowed[0], Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	mirror := filepath.Join(filepath.Dir(w.Path), "command-policy-")
	if !strings.Contains(r.Output, "goroot=77\ntoolchain=77\n"+mirror) || !strings.Contains(r.Output, "/goroot\nenv=0\n") {
		t.Errorf("output %q\nstderr %s", r.Output, r.Stderr)
	}
	if !strings.Contains(r.Stderr, "nested go with GOROOT=") || !strings.Contains(r.Stderr, "nested go with GOTOOLCHAIN=go1.99.0+path") || r.NestedCount != 3 {
		t.Errorf("%d nested calls, stderr %s", r.NestedCount, r.Stderr)
	}
}

// An account-mode run's scope takes an on-disk /var/tmp (Fae's own tests keep
// short-path fixtures there and build inside them), while a nested build
// still may not write under /dev/shm (FAE-HOST-020).
func TestBrokerAccountedScopeTakesAnOnDiskVarTmp(t *testing.T) {
	fixture, err := os.MkdirTemp("/var/tmp", "fae-gr-scope-")
	if err != nil {
		t.Skip("no /var/tmp", err)
	}
	defer os.RemoveAll(fixture)
	b, w, _ := setup(t)
	bin := t.TempDir()
	if err = os.WriteFile(filepath.Join(bin, "go"), []byte("#!/bin/sh\nprintf 'go ran:'\nprintf ' %s' \"$@\"\necho\n"), 0700); err != nil {
		t.Fatal(err)
	}
	script := `cd "$1" && go build -o "$1/x" .; echo "fixture=$?"; go build -o /dev/shm/fae-gr-x .; echo "shm=$?"`
	b.Allowed = [][]string{{"/bin/sh", "-c", script, "sh", fixture}}
	accountMode(t, b)
	r, err := b.Run(nestedRunContext(true), w, Command{Argv: b.Allowed[0], Timeout: time.Minute, Environment: []string{"PATH=" + bin + ":/usr/bin:/bin"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Output, "go ran: build -p=2 -o "+fixture+"/x .\nfixture=0\n") && !strings.Contains(r.Stderr, "RAM-backed temporary build output: /var/tmp") {
		t.Errorf("nested build in an on-disk /var/tmp: %q %s", r.Output, r.Stderr)
	}
	if !strings.Contains(r.Output, "shm=77\n") || !strings.Contains(r.Stderr, "/dev/shm/fae-gr-x") {
		t.Errorf("nested build output under /dev/shm: %q %s", r.Output, r.Stderr)
	}
}

// FAE-HOST-014: the broker seals the run's GOROOT mirror, so the command
// cannot add, replace or rename an entry by mistake (the go and gofmt
// wrappers among them; the run can still chmod it back, so this is not
// containment), and removes the sealed mirror with the rest of the run's
// policy directory.
func TestBrokerSealsTheRunsGoRootMirror(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain unavailable")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	b, w, _ := setup(t)
	script := `case "$GOROOT" in */goroot) ;; *) echo "no mirror: $GOROOT"; exit 3;; esac
ln -s /bin/true "$GOROOT/bin/x" 2>/dev/null; echo "add=$?"
rm -f "$GOROOT/bin/go" 2>/dev/null; echo "remove=$?"
mv "$GOROOT/bin" "$GOROOT/old" 2>/dev/null; echo "rename=$?"
test -L "$GOROOT/bin/go"; echo "wrapper=$?"`
	b.Allowed = [][]string{{"/bin/sh", "-c", script}}
	for _, account := range []bool{false, true} {
		if account {
			accountMode(t, b)
		}
		r, err := b.Run(nestedRunContext(true), w, Command{Argv: b.Allowed[0], Timeout: time.Minute})
		if err != nil || r.ExitCode != 0 || r.Output != "add=1\nremove=1\nrename=1\nwrapper=0\n" {
			t.Errorf("account %t: %v exit %d %q %s", account, err, r.ExitCode, r.Output, r.Stderr)
		}
		if residue, err := filepath.Glob(filepath.Join(filepath.Dir(w.Path), "command-*")); err != nil || len(residue) != 0 {
			t.Errorf("account %t: run left %v %v", account, residue, err)
		}
	}
}

// FAE-HOST-014: a go on PATH outside any GOROOT (here a version manager's
// shim script) gives a run no GOROOT mirror; the broker reports that gap
// (the daemon logs it at start and fae doctor shows it), and none for a go
// in a GOROOT.
func TestBrokerReportsAGoOutsideAnyGoRoot(t *testing.T) {
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain unavailable")
	}
	b, _, _ := setup(t)
	if binary, gap := b.GoRootGap(); binary == "" || gap != "" {
		t.Skipf("the go on PATH, %q, already has a gap: %s", binary, gap)
	}
	shims := t.TempDir()
	shim := filepath.Join(shims, "go")
	if err = os.WriteFile(shim, []byte("#!/bin/sh\nexec "+gobin+" \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shims+string(os.PathListSeparator)+os.Getenv("PATH"))
	binary, gap := b.GoRootGap()
	if binary != shim || !strings.Contains(gap, "the go on PATH, "+shim+", sits outside any GOROOT") || !strings.Contains(gap, "no GOROOT mirror") {
		t.Fatalf("GoRootGap = %q, %q", binary, gap)
	}
}

// FAE-HOST-014 through the real broker in account mode: a nested go that
// names a program for go to run (CC), sets more threads than its clamp
// (GOMAXPROCS, -cpu) or hands the test binary an unchecked output flag after
// -args is refused with 77 and recorded, and a nested go within the policy
// runs.
func TestBrokerAccountedGoRefusesEnvironmentAndTestBinaryFlags(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain unavailable")
	}
	b, w, _ := setup(t)
	script := `CC=/bin/sh go env CC; echo "cc=$?"
GOMAXPROCS=64 go version; echo "procs=$?"
go test -count=1 -cpu=64 .; echo "cpu=$?"
go test -count=1 . -args -test.coverprofile=/dev/shm/fae-gr-cover; echo "args=$?"
GOMAXPROCS=1 GOOS=linux go version >/dev/null; echo "ok=$?"`
	b.Allowed = [][]string{{"/bin/sh", "-c", script}}
	accountMode(t, b)
	r, err := b.Run(nestedRunContext(true), w, Command{Argv: b.Allowed[0], Timeout: 5 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Output, "cc=77\nprocs=77\ncpu=77\nargs=77\nok=0\n") {
		t.Errorf("output %q\nstderr %s", r.Output, r.Stderr)
	}
	for _, reason := range []string{"nested go with CC=/bin/sh: CC may name a program go runs", "nested go with GOMAXPROCS=64: more threads than the call's clamp, -p=2",
		"nested go test -cpu 64 runs tests at more threads than the call's clamp, -p=2", "nested go test passes -test.coverprofile=/dev/shm/fae-gr-cover to the test binary after -args"} {
		if !strings.Contains(r.Stderr, reason) {
			t.Errorf("no %q in stderr %s", reason, r.Stderr)
		}
	}
	if _, err = os.Stat("/dev/shm/fae-gr-cover"); err == nil {
		t.Error("a refused nested test wrote its profile")
		os.Remove("/dev/shm/fae-gr-cover")
	}
	if r.NestedCount != 5 {
		t.Errorf("%d nested calls recorded, want 5: %v", r.NestedCount, r.Nested)
	}
}
