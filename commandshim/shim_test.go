package commandshim

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func policy(t *testing.T) Policy {
	t.Helper()
	root := t.TempDir()
	root, e := filepath.EvalSymlinks(root)
	if e != nil {
		t.Fatal(e)
	}
	return Policy{Worktree: root, CWD: root, CodeRole: true, LeaseHeld: true, Local: true}
}
func TestPolicyRefusalMatrix(t *testing.T) {
	for name, argv := range map[string][]string{
		"absolute escape": {"go", "test", "/"}, "relative escape": {"go", "build", "../"}, "remote package": {"go", "test", "example.com/other"},
		"exec bypass": {"go", "test", "-exec=/bin/sh", "."}, "tool bypass": {"go", "build", "-toolexec=/bin/sh", "."}, "cwd flag": {"go", "-C", "/", "test"},
		"remote vcs": {"git", "remote", "add", "evil", "url"}, "push": {"git", "push"}, "wrapper probe": {"cargo", "--version"}, "rust module unsupported": {"rustfmt", "lib.rs"},
		"flags after target": {"go", "test", ".", "-run=X"}, "unknown flag": {"go", "build", "-overlay=file", "."}, "format stdin": {"gofmt"},
	} {
		t.Run(name, func(t *testing.T) {
			p := policy(t)
			_, e := p.Prepare(argv)
			var refused *Refusal
			if !errors.As(e, &refused) {
				t.Fatalf("expected refusal, got %v", e)
			}
		})
	}
	for _, field := range []string{"role", "lease", "nested", "cwd"} {
		t.Run(field, func(t *testing.T) {
			p := policy(t)
			switch field {
			case "role":
				p.CodeRole = false
			case "lease":
				p.LeaseHeld = false
			case "nested":
				p.Nested = true
			case "cwd":
				p.CWD = filepath.Dir(p.Worktree)
			}
			if _, e := p.Prepare([]string{"go", "test", "."}); e == nil {
				t.Fatal("unsafe dispatch admitted")
			}
		})
	}
}
func TestParallelismClampAndActualInvocation(t *testing.T) {
	p := policy(t)
	binary := filepath.Join(p.Worktree, "go")
	if e := os.WriteFile(binary, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0700); e != nil {
		t.Fatal(e)
	}
	// FAE-HOST-015: a whole-suite run (recursive targets, no -run filter) uses
	// one thread; targeted runs and builds keep the clamp of 2.
	for _, c := range []struct {
		argv []string
		want string
	}{
		{[]string{binary, "test", "-p", "999", "-parallel=999", "-count=1", "./..."}, "-p=1\n-parallel=1"},
		{[]string{binary, "test", "-p", "999", "-parallel=999", "-count=1", "-run=TestX", "./..."}, "-p=2\n-parallel=2"},
		{[]string{binary, "test", "-parallel=999", "-count=1", "./sub"}, "-p=2\n-parallel=2"},
		{[]string{binary, "build", "-p=999", "./..."}, "-p=2\n./..."},
	} {
		a, e := p.Prepare(c.argv)
		if e != nil {
			t.Fatal(e)
		}
		out, e := exec.Command(a[0], a[1:]...).CombinedOutput()
		if e != nil {
			t.Fatal(e)
		}
		if strings.Contains(string(out), "999") || !strings.Contains(string(out), c.want) {
			t.Fatalf("%v uncapped %s", c.argv, out)
		}
	}
}
func TestOutputAndSymlinkScope(t *testing.T) {
	p := policy(t)
	outside := t.TempDir()
	if e := os.Symlink(outside, filepath.Join(p.Worktree, "escape")); e != nil {
		t.Fatal(e)
	}
	for _, argv := range [][]string{{"go", "build", "./escape"}, {"go", "build", "-o", "escape/missing/output", "."}} {
		if _, e := p.Prepare(argv); e == nil {
			t.Fatal("symlink escape admitted")
		}
	}
	p.RAMRoots = []string{p.Worktree}
	_, e := p.Prepare([]string{"go", "build", "-o", "output", "."})
	if e == nil || !strings.Contains(e.Error(), "RAM-backed") {
		t.Fatalf("%v", e)
	}
}
func TestFormatterBounds(t *testing.T) {
	p := policy(t)
	small := filepath.Join(p.Worktree, "small.go")
	os.WriteFile(small, []byte("package x\n"), 0600)
	for _, data := range []string{strings.Repeat("x", (1<<20)+1), strings.Repeat("\n", 30001)} {
		os.WriteFile(filepath.Join(p.Worktree, "huge.go"), []byte(data), 0600)
		_, e := p.Prepare([]string{"gofmt", "-l", "."})
		if e == nil || !strings.Contains(e.Error(), "item-scoped") {
			t.Fatalf("%v", e)
		}
		if _, e = p.Prepare([]string{"gofmt", "-l", "./small.go"}); e != nil {
			t.Fatal(e)
		}
	}
	os.Remove(filepath.Join(p.Worktree, "huge.go"))
	os.Symlink(small, filepath.Join(p.Worktree, "link.go"))
	if _, e := p.Prepare([]string{"gofmt", "-l", "."}); e == nil {
		t.Fatal("symlink followed")
	}
}
func TestNestedWrappersActuallyRefuse(t *testing.T) {
	dir, e := InstallDenials(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"go", "gofmt", "cargo", "rustfmt", "git", "pytest"} {
		t.Run(name, func(t *testing.T) {
			cmd := exec.Command("/bin/sh", "-c", name+" --version")
			cmd.Env = PrependPath([]string{"PATH=/usr/bin:/bin", "PATH=/evil"}, dir)
			out, e := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(e, &exit) || exit.ExitCode() != 77 || !strings.Contains(string(out), "broker dispatch") {
				t.Fatalf("%s %v", out, e)
			}
		})
	}
}
func TestGuardTruthTable(t *testing.T) {
	for i := 0; i < 32; i++ {
		r, s, o, l, n := i&1 != 0, i&2 != 0, i&4 != 0, i&8 != 0, i&16 != 0
		if got := CanExecute(r, s, o, l, n); got != (r && s && o && l && !n) {
			t.Fatalf("%d", i)
		}
	}
}
