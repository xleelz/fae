package daemon

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/xleelz/fae/commandshim"
	"github.com/xleelz/fae/effects"
)

// nestedGoPolicyTest is a workspace test that starts nested go calls the
// account-mode policy refuses, each with 77 and its reason: a flag that runs
// another program (-toolexec), an environment that names one (CC), a -cpu
// above the call's clamp and an unchecked test binary output flag after
// -args. A nested go within the policy runs. Each nested go test has -run=^$,
// so it never runs this test again.
const nestedGoPolicyTest = `package sample

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func nestedGo(t *testing.T, env []string, args ...string) (int, string) {
	cmd := exec.Command("go", args...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if _, exit := err.(*exec.ExitError); err != nil && !exit {
		t.Fatal(err)
	}
	return cmd.ProcessState.ExitCode(), string(out)
}

func TestNestedGoPolicy(t *testing.T) {
	for _, c := range []struct {
		env    []string
		args   []string
		reason string
	}{
		{nil, []string{"vet", "-toolexec=/bin/true", "."}, "nested go flag -toolexec runs other programs"},
		{[]string{"CC=/bin/true"}, []string{"vet", "-tags=faeccprobe", "."}, "nested go with CC=/bin/true: CC may name a program go runs"},
		{nil, []string{"test", "-run=^$", "-cpu=64", "."}, "nested go test -cpu 64 runs tests at more threads than the call's clamp"},
		{nil, []string{"test", "-run=^$", ".", "-args", "-test.gocoverdir=/dev/shm"}, "nested go test passes -test.gocoverdir=/dev/shm to the test binary after -args"},
	} {
		if code, out := nestedGo(t, c.env, c.args...); code != 77 || !strings.Contains(out, c.reason) {
			t.Errorf("nested go %v %v: exit %d, want 77 %q: %s", c.env, c.args, code, c.reason, out)
		}
	}
	if code, out := nestedGo(t, []string{"GOMAXPROCS=1"}, "vet", "."); code != 0 {
		t.Errorf("nested go vet within the policy: exit %d %s", code, out)
	}
}
`

// FAE-HOST-014 on the daemon path (FAE-VER-002): a live daemon in account
// mode runs the shipped default workflow, whose unshaped go test runs a
// workspace test (nestedGoPolicyTest) that starts nested go calls the
// policy refuses. The test passes only if each is refused with 77 and its
// reason, so the task reaching Done shows the refusals; every call, refused
// or not, is in the run's durable nested-commands record in Dolt.
func TestLiveAccountModeRefusesNestedGoOutsideThePolicy(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain unavailable")
	}
	if err := os.WriteFile(filepath.Join(cfg.Workspace, "nested_go_test.go"), []byte(nestedGoPolicyTest), 0600); err != nil {
		t.Fatal(err)
	}
	liveGit(t, cfg.Workspace, "add", "nested_go_test.go")
	liveGit(t, cfg.Workspace, "commit", "-m", "a test with nested go calls")
	cfg.Live.Base = liveGit(t, cfg.Workspace, "rev-parse", "HEAD")
	liveGit(t, cfg.Workspace, "branch", "-f", "fae/integration", cfg.Live.Base)
	useBuiltFaeBroker(t)
	cfg.Live.NestedCommands = "account"
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	if d.live.broker.NestedCommands != effects.NestedAccount {
		t.Fatalf("live broker in mode %q", d.live.broker.NestedCommands)
	}
	authorize(t, d)
	if _, err = call(t, d, "Admit", AdmitParams{ID: "nested-go", AuthorizingMessage: "message"}, "nested-go"); err != nil {
		t.Fatal(err)
	}
	awaitState(t, d, "nested-go", "Done")
	markers, err := d.store.MarkersWithPrefix(ctx, "nested-commands/", 1000)
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"go", "vet", "-toolexec=/bin/true", "."}, {"go", "vet", "-tags=faeccprobe", "."}, {"go", "test", "-run=^$", "-cpu=64", "."},
		{"go", "test", "-run=^$", ".", "-args", "-test.gocoverdir=/dev/shm"}, {"go", "vet", "."},
	}
	recorded := 0
	for key, raw := range markers {
		var rec nestedRecordView
		if err = json.Unmarshal(raw, &rec); err != nil || rec.TaskID != "nested-go" {
			t.Fatalf("record %s %s: %v", key, raw, err)
		}
		found := 0
		for _, argv := range want {
			for _, c := range rec.Calls {
				if reflect.DeepEqual(c.Argv, argv) && c.Dir != "" && !c.At.IsZero() {
					found++
					break
				}
			}
		}
		if found == len(want) {
			recorded++
		}
	}
	if recorded == 0 {
		t.Fatalf("no run's nested-commands record holds the refused nested go calls: %d records", len(markers))
	}
}

// FAE-HOST-014, FAE-OBS-001: a daemon whose PATH puts a go outside any
// GOROOT first (a version manager's shim) gives its brokered runs no GOROOT
// mirror, and says so at start in a timestamped host line of its
// supervision log.
func TestLiveStartLogsAGoOutsideAnyGoRoot(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain unavailable")
	}
	if _, gap := commandshim.GoRootGap([]string{"PATH=" + os.Getenv("PATH")}); gap != "" {
		t.Skip("the go on PATH already has a gap: " + gap)
	}
	shims := t.TempDir()
	shim := filepath.Join(shims, "go")
	if err = os.WriteFile(shim, []byte("#!/bin/sh\nexec "+gobin+" \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shims+string(os.PathListSeparator)+os.Getenv("PATH"))
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	stop(t, d)
	raw, err := os.ReadFile(filepath.Join(cfg.Root, cfg.Instance, "supervision.log"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.Contains(line, "the go on PATH, "+shim+", sits outside any GOROOT") {
			if !strings.Contains(line, " instance="+cfg.Instance+" component=host ") || !strings.Contains(line, "no GOROOT mirror") {
				t.Fatalf("gap line %q", line)
			}
			return
		}
	}
	t.Fatalf("no GOROOT gap line in the supervision log:\n%s", raw)
}

// FAE-HOST-020: the start sweep removes a killed run's policy directory even
// when it holds the run's sealed, read-only GOROOT mirror.
func TestStartSweepRemovesASealedGoRootMirror(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	root := t.TempDir()
	d := &Daemon{layout: Layout{Root: root, Run: filepath.Join(root, "run"), Worktrees: filepath.Join(root, "worktrees")}}
	policy := filepath.Join(d.layout.Worktrees, "command-policy-3")
	real := filepath.Join(root, "goroot-real")
	for _, dir := range []string{policy, filepath.Join(real, "pkg", "tool"), filepath.Join(real, "bin"), filepath.Join(d.layout.Worktrees, "task-a")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	mirror, err := commandshim.InstallGoRoot(policy, real)
	if err != nil {
		t.Fatal(err)
	}
	if err = commandshim.SealGoRoot(mirror); err != nil {
		t.Fatal(err)
	}
	if err = d.sweepCommandScratch(nil); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(d.layout.Worktrees)
	if len(entries) != 1 || entries[0].Name() != "task-a" {
		t.Fatal("sealed policy directory not swept, or a worktree removed", entries)
	}
}
