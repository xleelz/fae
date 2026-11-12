package commandshim

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// roleRefusals is keyed by the exact source text of every refusal RunShim
// returns in role.go (deny(refuse(...)) and deny(fmt.Errorf(...))): each case
// must exit Refused with a *Refusal naming that reason before any lease
// (FAE-HOST-011/-014, FAE-WF-042, FAE-VCS-002, FAE-MODE-002; FAE-VER-001).
var roleRefusals = map[string][]struct {
	reason string
	shims  RoleShims
	env    []string
	argv   []string
	policy string
}{
	`"shim policy: %v", err`: {{reason: "shim policy: open", argv: []string{"go", "test", "."}, policy: "/nonexistent/fae-role-policy.json"}},
	`"empty command"`:        {{reason: "empty command", shims: RoleShims{Test: true}}},
	`"nested " + name + " inside a leased command is refused (FAE-HOST-014); run it as its own command"`: {
		{reason: "nested go inside a leased command is refused (FAE-HOST-014)", shims: RoleShims{Build: true, Test: true}, env: []string{NestedMarker + "=1"}, argv: []string{"go", "test", "."}},
	},
	`name + " is not an allowed command shim of this role's tool profile"`: {
		{reason: "go is not an allowed command shim of this role's tool profile", shims: RoleShims{Test: true, Shims: []string{"gofmt"}}, argv: []string{"go", "test", "."}},
	},
	`name + " requires a scoped broker adapter (including version probes)"`: {
		{reason: "cargo requires a scoped broker adapter", shims: RoleShims{Build: true}, argv: []string{"cargo", "--version"}},
		{reason: "make requires a scoped broker adapter", shims: RoleShims{Build: true}, argv: []string{"make"}},
	},
	`"this role has no " + capability + " capability (pre-code roles never build, test or format)"`: {
		{reason: "this role has no build capability", argv: []string{"go", "build", "."}},
		{reason: "this role has no test capability", argv: []string{"go", "test", "."}},
		{reason: "this role has no formatter capability", argv: []string{"gofmt", "-l", "a.go"}},
	},
	`name + " is not on the session's base PATH"`: {{reason: "gofmt is not on the session's base PATH", shims: RoleShims{Format: true}, argv: []string{"gofmt", "-l", "a.go"}}},
}

// roleLocalRefusals is keyed by the exact source text of every Local mode
// refusal RunShim returns in role.go (&LocalRefusal{...}): each case must exit
// LocalModeRefused (78) with a *LocalRefusal naming that reason, before any
// lease and distinct from a policy refusal (FAE-MODE-002; FAE-VER-001).
var roleLocalRefusals = map[string][]struct {
	reason string
	argv   []string
}{
	`"git " + sub + ": a Local task makes no external effect, so pushes and remote changes are refused"`: {
		{reason: "git push: a Local task makes no external effect", argv: []string{"git", "push"}},
		{reason: "git remote: a Local task makes no external effect", argv: []string{"git", "remote", "add", "evil", "url"}},
	},
}

// denialMessage is the fail-closed wrapper text InstallDenials installs when
// a session has no broker (every daemon test binary, which is not fae).
const denialMessage = `fae: command refused (exit 77): role tools and nested heavy commands require a separate broker dispatch; use a scoped host-command stage`

func TestRoleShimRefusalCorpus(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	worktree, ledger := filepath.Join(root, "wt"), filepath.Join(root, "never", "ledger")
	if err = os.Mkdir(worktree, 0700); err != nil {
		t.Fatal(err)
	}
	for key, cases := range roleRefusals {
		for i, c := range cases {
			s := c.shims
			s.Worktree, s.Ledger, s.RAMRoots = worktree, ledger, DefaultRAMRoots
			if s.Tools == nil && !strings.Contains(c.reason, "base PATH") {
				s.Tools = map[string]string{"go": "/bin/false", "gofmt": "/bin/false", "git": "/bin/false"}
			}
			path := c.policy
			if path == "" {
				raw, _ := json.Marshal(s)
				path = filepath.Join(root, "policy.json")
				if err = os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			code, err := RunShim(context.Background(), path, HeavyOp{Argv: c.argv, Dir: worktree, Env: append([]string{"PATH=/usr/bin:/bin"}, c.env...), Observe: healthyHost})
			var refused *Refusal
			if code != Refused || !errors.As(err, &refused) || !strings.Contains(refused.Reason, c.reason) {
				t.Errorf("%s case %d %v: exit %d %v", key, i, c.argv, code, err)
			}
		}
	}
	// A Local task's push or remote change is its own refusal, exit 78
	// (FAE-MODE-002), distinct from a policy refusal.
	for key, cases := range roleLocalRefusals {
		for i, c := range cases {
			raw, _ := json.Marshal(RoleShims{Local: true, Worktree: worktree, Ledger: ledger, RAMRoots: DefaultRAMRoots, Tools: map[string]string{"git": "/bin/false"}})
			path := filepath.Join(root, "local.json")
			if err = os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			code, err := RunShim(context.Background(), path, HeavyOp{Argv: c.argv, Dir: worktree, Env: []string{"PATH=/usr/bin:/bin"}, Observe: healthyHost})
			var local *LocalRefusal
			if code != LocalModeRefused || !errors.As(err, &local) || !strings.Contains(local.Reason, c.reason) {
				t.Errorf("%s case %d %v: exit %d %v", key, i, c.argv, code, err)
			}
		}
	}
	if _, err = os.Stat(filepath.Dir(ledger)); !os.IsNotExist(err) {
		t.Fatal("a refusal touched the heavy-op ledger", err)
	}
	// The positive control: a granted, leased test of the own worktree runs.
	tools := filepath.Join(root, "tools")
	if err = os.Mkdir(tools, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(tools, "go"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(RoleShims{Test: true, Worktree: worktree, Ledger: heavyLedger(t, 2), RAMRoots: DefaultRAMRoots, Tools: map[string]string{"go": filepath.Join(tools, "go")}})
	path := filepath.Join(root, "granted.json")
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if code, err := RunShim(context.Background(), path, HeavyOp{Argv: []string{"go", "test", "."}, Dir: worktree, Env: []string{"PATH=/usr/bin:/bin"}, Observe: healthyHost}); code != 0 || err != nil {
		t.Fatal("granted role test refused", code, err)
	}
	// Without a broker every wrapper refuses with the denial text and exit 77.
	dir, err := InstallDenials(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"go", "gofmt", "git", "cargo"} {
		cmd := exec.Command(filepath.Join(dir, name), "version")
		out, err := cmd.CombinedOutput()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != Refused || strings.TrimSpace(string(out)) != denialMessage {
			t.Fatalf("denial wrapper %s: %v %q", name, err, out)
		}
	}
}

// The census: the corpus keys are exactly RunShim's refusal sources in role.go
// (deny(refuse(...)) and deny(fmt.Errorf(...)) in roleRefusals, and
// &LocalRefusal{...} in roleLocalRefusals), and the denial text is exactly the
// wrapper InstallDenials writes.
func TestRoleShimRefusalCensus(t *testing.T) {
	raw, err := os.ReadFile("role.go")
	if err != nil {
		t.Fatal(err)
	}
	policy, local := map[string]bool{}, map[string]bool{}
	for k := range roleRefusals {
		policy[k] = true
	}
	for k := range roleLocalRefusals {
		local[k] = true
	}
	diff := []string{}
	for _, census := range []struct {
		patterns []string
		corpus   map[string]bool
		least    int
	}{
		{[]string{`(?m)deny\(refuse\((.*)\)\)\s*$`, `(?m)deny\(fmt\.Errorf\((.*)\)\)\s*$`}, policy, 7},
		{[]string{`(?m)&LocalRefusal\{(.*)\}\s*$`}, local, 1},
	} {
		found := map[string]bool{}
		for _, re := range census.patterns {
			for _, m := range regexp.MustCompile(re).FindAllStringSubmatch(string(raw), -1) {
				found[m[1]] = true
			}
		}
		for k := range found {
			if !census.corpus[k] {
				diff = append(diff, "missing "+k)
			}
		}
		for k := range census.corpus {
			if !found[k] {
				diff = append(diff, "stale "+k)
			}
		}
		if len(found) < census.least {
			diff = append(diff, "too few sources found: "+strconv.Itoa(len(found)))
		}
	}
	sort.Strings(diff)
	if len(diff) != 0 {
		t.Fatal(diff)
	}
	shim, err := os.ReadFile("shim.go")
	if err != nil || !strings.Contains(string(shim), "'"+denialMessage+"'") {
		t.Fatal("InstallDenials wrapper text changed", err)
	}
}
