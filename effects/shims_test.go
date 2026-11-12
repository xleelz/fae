package effects

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/commandshim"
)

// The test binary doubles as the fae broker that account-mode wrappers exec
// (`fae --home DIR heavy-op run --shim-policy FILE -- TOOL ARGS...`), so the
// nested-command policy runs end to end without building cmd/fae.
func TestMain(m *testing.M) {
	if a := os.Args[1:]; len(a) > 7 && a[0] == "--home" && a[2] == "heavy-op" && a[3] == "run" && a[4] == "--shim-policy" && a[6] == "--" {
		code, err := commandshim.RunShim(context.Background(), a[5], commandshim.HeavyOp{Argv: a[7:], Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(code)
	}
	os.Exit(m.Run())
}

// accountMode puts b in account mode with this test binary as its broker.
func accountMode(t *testing.T, b *Broker) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b.NestedCommands, b.NestedBroker = NestedAccount, exe
}

// nestedRunContext is a dispatch's context for a Local or Normal task.
func nestedRunContext(local bool) context.Context {
	return WithNestedRun(context.Background(), NestedRun{TaskID: "task-n", DispatchID: "dispatch-n", Local: local})
}

func TestBrokerNestedHeavyCommandRefused(t *testing.T) {
	b, w, _ := setup(t)
	b.Allowed = [][]string{{"/bin/sh", "-c", "go version"}}
	r, e := b.Run(context.Background(), w, Command{Argv: b.Allowed[0], Timeout: time.Second})
	if e != nil {
		t.Fatal(e)
	}
	if r.ExitCode != 77 || !strings.Contains(r.Stderr, "broker dispatch") {
		t.Fatalf("nested command escaped: %+v", r)
	}
}
func TestBrokerClampsAdmittedGoBuild(t *testing.T) {
	b, w, _ := setup(t)
	dir := t.TempDir()
	binary := filepath.Join(dir, "go")
	if e := os.WriteFile(binary, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0700); e != nil {
		t.Fatal(e)
	}
	b.Allowed = [][]string{{binary, "build", "-p=999", "."}}
	r, e := b.Run(context.Background(), w, Command{Argv: b.Allowed[0], Timeout: time.Second})
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(r.Output, "-p=2") || strings.Contains(r.Output, "999") {
		t.Fatalf("%+v", r)
	}
}

// FAE-HOST-014 accounting: in account mode a brokered command's nested git
// and go calls run the real tools and the evidence lists them; RecordNested
// sees the same evidence before Run returns, and its failure fails the run.
// Refuse mode, explicit or default, still exits 77; an unknown mode is refused.
func TestBrokerAccountsNestedCommands(t *testing.T) {
	b, w, _ := setup(t)
	script := `cd "$TMPDIR" && git init -q nested-repo && test -d nested-repo/.git && go version && pwd`
	b.Allowed = [][]string{{"/bin/sh", "-c", script}}
	accountMode(t, b)
	var recorded []RunEvidence
	b.RecordNested = func(_ context.Context, r RunEvidence) error { recorded = append(recorded, r); return nil }
	r, err := b.Run(context.Background(), w, Command{Argv: b.Allowed[0], Timeout: time.Minute})
	if err != nil || r.ExitCode != 0 || !strings.Contains(r.Output, "go version go") {
		t.Fatalf("%+v %v", r, err)
	}
	scratch := strings.TrimSpace(r.Output[strings.LastIndex(strings.TrimSpace(r.Output), "\n")+1:])
	if r.NestedCount != 2 || len(r.Nested) != 2 {
		t.Fatalf("nested calls not accounted: %d %+v", r.NestedCount, r.Nested)
	}
	got := map[string]bool{}
	for _, c := range r.Nested {
		// A directory past the summary bound (a long TMPDIR) is cut, and marked.
		got[strings.Join(c.Argv, " ")] = !c.At.IsZero() && (c.Dir == scratch && !c.Truncated || c.Truncated && strings.HasPrefix(scratch, c.Dir))
	}
	if !got["git init -q nested-repo"] || !got["go version"] {
		t.Fatalf("accounted %+v, want git init -q nested-repo and go version in %s", r.Nested, scratch)
	}
	if len(recorded) != 1 || recorded[0].NestedCount != 2 || recorded[0].OutputDigest != r.OutputDigest || !reflect.DeepEqual(recorded[0].Command, b.Allowed[0]) || recorded[0].Started.IsZero() {
		t.Fatalf("RecordNested saw %+v", recorded)
	}
	b.RecordNested = func(context.Context, RunEvidence) error { return errors.New("store unavailable") }
	if _, err = b.Run(context.Background(), w, Command{Argv: b.Allowed[0], Timeout: time.Minute}); err == nil || err.Error() != "store unavailable" {
		t.Fatal("unrecorded accounting accepted", err)
	}
	for _, mode := range []NestedMode{"", NestedRefuse} {
		b.NestedCommands = mode
		r, err = b.Run(context.Background(), w, Command{Argv: b.Allowed[0], Timeout: time.Minute})
		if err != nil || r.ExitCode != 77 || r.NestedCount != 0 || r.Nested != nil || !strings.Contains(r.Stderr, "broker dispatch") {
			t.Fatalf("mode %q: %+v %v", mode, r, err)
		}
	}
	b.NestedCommands = "acount"
	if _, err = b.Run(context.Background(), w, Command{Argv: b.Allowed[0], Timeout: time.Minute}); err == nil || !strings.Contains(err.Error(), "unknown nested command mode") {
		t.Fatal("unknown nested mode ran", err)
	}
	// Account mode needs the fae broker that applies the policy.
	b.NestedCommands, b.NestedBroker = NestedAccount, ""
	if _, err = b.Run(context.Background(), w, Command{Argv: b.Allowed[0], Timeout: time.Minute}); err == nil || !strings.Contains(err.Error(), "needs the fae executable") {
		t.Fatal("account mode ran without a broker", err)
	}
}

// An accounted nested tool missing from the run's base PATH fails clearly and
// is still counted.
func TestBrokerAccountedToolMissingFromPath(t *testing.T) {
	b, w, _ := setup(t)
	b.Allowed = [][]string{{"/bin/sh", "-c", "git --version"}}
	accountMode(t, b)
	r, err := b.Run(context.Background(), w, Command{Argv: b.Allowed[0], Timeout: time.Minute, Environment: []string{"PATH=" + t.TempDir()}})
	if err != nil || r.ExitCode != 127 || !strings.Contains(r.Stderr, "fae: nested git not found on the base PATH") || r.NestedCount != 1 {
		t.Fatalf("%+v %v", r, err)
	}
}

// The accounting wrappers never evaluate a nested call's arguments or working
// directory: quotes, command substitutions and newlines reach the real tool
// and the evidence verbatim, and the real tool is found on the stage's PATH
// even in a directory whose name holds shell metacharacters.
func TestBrokerAccountingResistsArgvInjection(t *testing.T) {
	b, w, _ := setup(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	awkward := "it's \"$(touch pwned-a)\" `touch pwned-b`\n;$HOME"
	bin, cwd := filepath.Join(root, "bin "+awkward), filepath.Join(root, "cwd "+awkward)
	for _, d := range []string{bin, cwd} {
		if err = os.Mkdir(d, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err = os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nfor a in \"$@\"; do printf '<%s>\\n' \"$a\"; done\n"), 0700); err != nil {
		t.Fatal(err)
	}
	args := []string{"version", "it's \"$(touch pwned-c)\" `touch pwned-d`", "two\nlines; touch pwned-e", "$(id)"}
	b.Allowed = [][]string{append([]string{"/bin/sh", "-c", `cd "$1" && shift && git "$@"`, "sh", cwd}, args...)}
	accountMode(t, b)
	r, err := b.Run(context.Background(), w, Command{Argv: b.Allowed[0], Timeout: time.Minute, Environment: []string{"PATH=" + bin + ":/usr/bin:/bin"}})
	if err != nil || r.ExitCode != 0 {
		t.Fatalf("%+v %v", r, err)
	}
	want := ""
	for _, a := range args {
		want += "<" + a + ">\n"
	}
	if r.Output != want {
		t.Fatalf("real tool got %q, want %q", r.Output, want)
	}
	// Under a long TMPDIR the directory summary is cut to its bound.
	if c := r.Nested; r.NestedCount != 1 || !reflect.DeepEqual(c[0].Argv, append([]string{"git"}, args...)) || !strings.HasPrefix(cwd, c[0].Dir) || (c[0].Dir == cwd) == c[0].Truncated {
		t.Fatalf("recorded %+v", r.Nested)
	}
	for _, d := range []string{root, w.Path, filepath.Dir(w.Path)} {
		_ = filepath.WalkDir(d, func(path string, e os.DirEntry, err error) error {
			if err == nil && strings.HasPrefix(e.Name(), "pwned") {
				t.Errorf("argument evaluated: %s", path)
			}
			return nil
		})
	}
}

// MaxOutput raises the output bound: 5 MiB is cut at the 4 MiB default and
// kept whole under a 6 MiB bound.
func TestBrokerMaxOutputBound(t *testing.T) {
	b, w, _ := setup(t)
	b.Allowed = [][]string{{"/bin/sh", "-c", "head -c 5242880 /dev/zero | tr '\\000' x"}}
	c := Command{Argv: b.Allowed[0], Timeout: time.Minute}
	if r, err := b.Run(context.Background(), w, c); err != nil || !r.Truncated || len(r.Output) != DefaultMaxOutput {
		t.Fatal("5 MiB kept whole under the default bound", len(r.Output), err)
	}
	b.MaxOutput = 6 << 20
	r, err := b.Run(context.Background(), w, c)
	if err != nil || len(r.Output) != 5<<20 || r.Truncated || r.OutputDigest == "" {
		t.Fatalf("%d bytes truncated=%v: %v", len(r.Output), r.Truncated, err)
	}
}

// FAE-HOST-014's accounted branch through the broker (FAE-HOST-012/-015,
// FAE-MODE-002): in account mode a brokered command's nested go test is
// clamped as the top-level command is, a nested gofmt over the formatter
// limit is refused, and a nested git push is refused, 78 for a Local task and
// 77 for a Normal one. The real tools are fakes that print their arguments,
// so a call that reached one shows in the output.
func TestBrokerAccountedNestedCallsAreClampedAndConfined(t *testing.T) {
	b, w, _ := setup(t)
	bin := t.TempDir()
	for _, name := range []string{"go", "gofmt", "git"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nprintf '%s ran:' \"${0##*/}\"\nprintf ' %s' \"$@\"\necho\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	script := `go test -parallel=64 -p 64 -json ./...; echo "go=$?"
head -c 1048577 /dev/zero | tr '\000' x > "$TMPDIR/huge.go"; gofmt -l "$TMPDIR/huge.go"; echo "gofmt=$?"
git push origin HEAD; echo "push=$?"`
	b.Allowed = [][]string{{"/bin/sh", "-c", script}}
	accountMode(t, b)
	for _, c := range []struct {
		local       bool
		push, cause string
	}{
		{true, "push=78", "Local mode"},
		{false, "push=77", "an engine run makes no external effect"},
	} {
		r, err := b.Run(nestedRunContext(c.local), w, Command{Argv: b.Allowed[0], Timeout: time.Minute, Environment: []string{"PATH=" + bin + ":/usr/bin:/bin"}})
		if err != nil {
			t.Fatal(err)
		}
		if want := "go ran: test -p=1 -parallel=1 -json ./...\ngo=0\ngofmt=77\n" + c.push + "\n"; r.Output != want {
			t.Errorf("local=%t: output %q, want %q (stderr %s)", c.local, r.Output, want, r.Stderr)
		}
		if !strings.Contains(r.Stderr, "exceeds 1 MiB; use item-scoped formatting") || !strings.Contains(r.Stderr, c.cause) || r.NestedCount != 3 {
			t.Errorf("local=%t: %d calls, stderr %s", c.local, r.NestedCount, r.Stderr)
		}
	}
}
