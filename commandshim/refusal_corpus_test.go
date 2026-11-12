package commandshim

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// shimRefusals is keyed by the exact source text of every refuse(...) call in
// shim.go: each case must be refused as a *Refusal naming that reason
// (FAE-HOST-011/-012/-020, FAE-MODE-002; FAE-VER-001 firing controls).
var shimRefusals = map[string]struct {
	reason string
	run    func(t *testing.T, p Policy) error
}{
	`"target outside own worktree: " + name`: {"target outside own worktree: /", prepare("go", "test", "/")},
	`"empty command"`:                        {"empty command", prepare()},
	`"code role, own worktree, a heavy lease and non-nested dispatch are required"`: {"heavy lease", func(t *testing.T, p Policy) error {
		p.LeaseHeld = false
		return prepare("go", "test", ".")(t, p)
	}},
	`"VCS commands in role command paths are disabled in Local mode; use the dedicated VCS broker"`: {"disabled in Local mode", prepare("git", "push")},
	`"use the dedicated VCS broker"`: {"use the dedicated VCS broker", func(t *testing.T, p Policy) error {
		p.Local = false
		return prepare("git", "remote", "add", "evil", "url")(t, p)
	}},
	`"unsupported build wrapper requires a scoped broker adapter (including version probes)"`: {"unsupported build wrapper", prepare("make", "--version")},
	`"unsupported formatter flag"`:                                {"unsupported formatter flag", prepare("gofmt", "-r", "a -> b", ".")},
	`"formatter requires explicit per-file targets"`:              {"explicit per-file targets", prepare("gofmt", "-l")},
	`"unsupported Go verb; probes also require broker admission"`: {"unsupported Go verb", prepare("go", "run", ".")},
	`"flags must precede package targets"`:                        {"flags must precede package targets", prepare("go", "test", ".", "-run=X")},
	`"missing parallelism value"`:                                 {"missing parallelism value", prepare("go", "test", "-p")},
	`"missing output path"`:                                       {"missing output path", prepare("go", "build", "-o")},
	`"RAM-backed temporary build output: " + dest`: {"RAM-backed temporary build output", func(t *testing.T, p Policy) error {
		p.RAMRoots = []string{p.Worktree}
		return prepare("go", "build", "-o", "output", ".")(t, p)
	}},
	`"unsupported Go flag: " + a`:                    {"unsupported Go flag: -overlay=file", prepare("go", "build", "-overlay=file", ".")},
	`"target must be an explicit own-worktree path"`: {"explicit own-worktree path", prepare("go", "test", "example.com/other")},
	`"formatter requires a regular per-file target; use item-scoped formatting"`: {"regular per-file target", func(t *testing.T, p Policy) error {
		fifo := filepath.Join(p.Worktree, "pipe.go")
		if e := syscall.Mkfifo(fifo, 0600); e != nil {
			t.Skip("no FIFO support", e)
		}
		return prepare("gofmt", "-l", "pipe.go")(t, p)
	}},
	`path + " exceeds 1 MiB; use item-scoped formatting"`: {"exceeds 1 MiB", func(t *testing.T, p Policy) error {
		write(t, filepath.Join(p.Worktree, "huge.go"), strings.Repeat("x", (1<<20)+1))
		return prepare("gofmt", "-l", "huge.go")(t, p)
	}},
	`path + " exceeds 30k lines; use item-scoped formatting"`: {"exceeds 30k lines", func(t *testing.T, p Policy) error {
		write(t, filepath.Join(p.Worktree, "long.go"), strings.Repeat("\n", 30001))
		return prepare("gofmt", "-l", "long.go")(t, p)
	}},
	`"formatter tree exceeds 10000 entries; use item-scoped formatting"`: {"exceeds 10000 entries", func(t *testing.T, p Policy) error {
		dir := filepath.Join(p.Worktree, "many")
		if e := os.Mkdir(dir, 0700); e != nil {
			t.Fatal(e)
		}
		for i := 0; i <= 10000; i++ {
			write(t, filepath.Join(dir, strconv.Itoa(i)+".txt"), "")
		}
		return prepare("gofmt", "-l", "many")(t, p)
	}},
	`"symlink in formatter tree; use item-scoped formatting"`: {"symlink in formatter tree", func(t *testing.T, p Policy) error {
		dir := filepath.Join(p.Worktree, "linked")
		if e := os.Mkdir(dir, 0700); e != nil {
			t.Fatal(e)
		}
		write(t, filepath.Join(p.Worktree, "real.go"), "package x\n")
		if e := os.Symlink(filepath.Join(p.Worktree, "real.go"), filepath.Join(dir, "link.go")); e != nil {
			t.Fatal(e)
		}
		return prepare("gofmt", "-l", "linked")(t, p)
	}},
	`"formatter tree exceeds 64 MiB; use item-scoped formatting"`: {"exceeds 64 MiB", func(t *testing.T, p Policy) error {
		dir := filepath.Join(p.Worktree, "big")
		if e := os.Mkdir(dir, 0700); e != nil {
			t.Fatal(e)
		}
		for i := 0; i < 65; i++ {
			// Sparse files of exactly 1 MiB stay within the per-file bound.
			name := filepath.Join(dir, strconv.Itoa(i)+".go")
			write(t, name, "")
			if e := os.Truncate(name, 1<<20); e != nil {
				t.Fatal(e)
			}
		}
		return prepare("gofmt", "-l", "big")(t, p)
	}},
	// PrepareAccounted: an account-mode run's nested calls (FAE-HOST-014).
	`"nested go flag -" + name + " runs other programs or leaves the run's directories"`: {"nested go flag -toolexec runs other programs", accounted(nil, "go", "build", "-toolexec=/bin/sh", ".")},
	`"nested build, test or format outside the run's worktree and scratch directory: " + p.CWD`: {"outside the run's worktree and scratch directory", func(t *testing.T, p Policy) error {
		p.CWD = filepath.Dir(p.Worktree)
		return accounted(nil, "go", "test", ".")(t, p)
	}},
	`"nested go env -w and -u change the user's Go environment; nested calls may only read it"`:                                 {"go env -w and -u change the user's Go environment", accounted(nil, "go", "env", "-w", "GOFLAGS=-p=64")},
	`"unsupported nested Go verb " + strconv.Quote(verb) + "; nested calls may run go build, test, vet, list, version and env"`: {`unsupported nested Go verb "generate"`, accounted(nil, "go", "generate", "./...")},
}

func prepare(argv ...string) func(*testing.T, Policy) error {
	return func(_ *testing.T, p Policy) error { _, e := p.Prepare(argv); return e }
}

func accounted(env []string, argv ...string) func(*testing.T, Policy) error {
	return func(_ *testing.T, p Policy) error { _, e := p.PrepareAccounted(argv, env); return e }
}
func write(t *testing.T, path, body string) {
	t.Helper()
	if e := os.WriteFile(path, []byte(body), 0600); e != nil {
		t.Fatal(e)
	}
}

func TestShimRefusalCorpus(t *testing.T) {
	for key, c := range shimRefusals {
		t.Run(c.reason, func(t *testing.T) {
			e := c.run(t, policy(t))
			var refused *Refusal
			if !errors.As(e, &refused) || !strings.Contains(refused.Reason, c.reason) {
				t.Fatalf("%s: want refusal %q, got %v", key, c.reason, e)
			}
		})
	}
	// The positive control: a scoped, leased, targeted test is admitted.
	if _, e := policy(t).Prepare([]string{"go", "test", "-run=TestX", "."}); e != nil {
		t.Fatal(e)
	}
}

// The census: the corpus keys are exactly shim.go's refusal sources.
func TestShimRefusalCensus(t *testing.T) {
	raw, err := os.ReadFile("shim.go")
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)refuse\((.*)\)\s*$`).FindAllStringSubmatch(string(raw), -1) {
		found[m[1]] = true
	}
	diff := []string{}
	for k := range found {
		if _, ok := shimRefusals[k]; !ok {
			diff = append(diff, "missing "+k)
		}
	}
	for k := range shimRefusals {
		if !found[k] {
			diff = append(diff, "stale "+k)
		}
	}
	sort.Strings(diff)
	if len(diff) != 0 || len(found) < 20 {
		t.Fatal(diff, len(found))
	}
}
