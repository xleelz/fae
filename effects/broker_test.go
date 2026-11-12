package effects

import (
	"context"
	"encoding/json"
	"github.com/xleelz/fae/worktree"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func setup(t *testing.T) (*Broker, worktree.Binding, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "repo")
	os.Mkdir(repo, 0700)
	gitTest(t, repo, "init")
	gitTest(t, repo, "config", "user.name", "Tester")
	gitTest(t, repo, "config", "user.email", "test@localhost")
	write(t, repo, "go.mod", "module example.com/test\n\ngo 1.23\n")
	write(t, repo, "x.go", "package test\nfunc X() int { return 1 }\n")
	write(t, repo, "x_test.go", "package test\nimport \"testing\"\nfunc TestX(t *testing.T) { if X()!=1 {t.Fatal(\"wrong value\")} }\n")
	gitTest(t, repo, "add", ".")
	gitTest(t, repo, "commit", "-m", "initial")
	m := &worktree.Manager{Repository: repo, Root: filepath.Join(root, "trees")}
	w, err := m.Ensure(context.Background(), "run", "task", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return &Broker{Manager: m, Environment: []string{"GOCACHE=" + os.Getenv("GOCACHE")}}, w, repo
}
func gitTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = cleanEnv(nil, dir)
	cmd.Env = append(cmd.Env, "GIT_AUTHOR_NAME=Tester", "GIT_AUTHOR_EMAIL=test@localhost", "GIT_COMMITTER_NAME=Tester", "GIT_COMMITTER_EMAIL=test@localhost")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
func write(t *testing.T, dir, name, value string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
}
func TestRealGoTestEvidence(t *testing.T) {
	b, w, _ := setup(t)
	b.Allowed = [][]string{{"go", "test", "-json", "-count=1", "-a", "-x", "./..."}}
	ctx := context.Background()
	c := Command{Argv: b.Allowed[0], Timeout: time.Minute, TestID: "TestX", ExpectedFailure: "wrong value"}
	r, err := b.Run(ctx, w, c)
	if err != nil || !r.FreshBuild || r.ExecutedCount != 1 || r.MatchedCount != 1 || !r.RedPassed || r.ExitCode != 0 || r.Tree == "" {
		t.Fatalf("%+v %v", r, err)
	}
	write(t, w.Path, "x.go", "package test\nfunc X() int { return 2 }\n")
	r, err = b.Run(ctx, w, c)
	if err != nil || !r.ExpectedFailure || r.RedPassed || r.ExitCode == 0 {
		t.Fatalf("%+v %v", r, err)
	}
}
func TestLiteralAllowlistTimeoutAndOverflow(t *testing.T) {
	b, w, _ := setup(t)
	ownerRoot := filepath.Dir(w.Path)
	defer func() {
		residue, e := filepath.Glob(filepath.Join(ownerRoot, "command-*"))
		if e != nil || len(residue) != 0 {
			t.Error("host command ownership residue", residue, e)
		}
	}()
	ctx := context.Background()
	argv := []string{"printf", "%s", "$(touch BAD);literal"}
	b.Allowed = [][]string{argv, {"sleep", "5"}}
	r, err := b.Run(ctx, w, Command{Argv: argv, Timeout: time.Second})
	if err != nil || r.Output != argv[2] {
		t.Fatalf("%+v %v", r, err)
	}
	if _, err = os.Stat(filepath.Join(w.Path, "BAD")); !os.IsNotExist(err) {
		t.Fatal("shell expansion")
	}
	if _, err = b.Run(ctx, w, Command{Argv: []string{"echo", "bad"}, Timeout: time.Second}); err == nil {
		t.Fatal("allowlist bypass")
	}
	// A command killed at its deadline is a timed-out failure, not an error.
	start := time.Now()
	if r, err = b.Run(ctx, w, Command{Argv: b.Allowed[1], Timeout: 500 * time.Millisecond}); err != nil || !r.TimedOut || r.ExitCode != -1 || time.Since(start) > 3*time.Second {
		t.Fatal("timeout", r, err)
	}
	// Output past the bound is cut; the run is still evidence (Truncated).
	b.MaxOutput = 2
	r, err = b.Run(ctx, w, Command{Argv: argv, Timeout: time.Second})
	if err != nil || !r.Truncated || r.Output != argv[2][:2] || r.ExitCode != 0 {
		t.Fatalf("overflow %+v %v", r, err)
	}
}
func TestExactCommitIntegrationAndCheckoutPreservation(t *testing.T) {
	b, w, repo := setup(t)
	ctx := context.Background()
	base := gitTest(t, repo, "rev-parse", "HEAD")
	gitTest(t, repo, "branch", "fae/integration")
	write(t, w.Path, "x.go", "package test\nfunc X() int { return 2 }\n")
	s, err := b.Snapshot(ctx, w, []string{"x.go"})
	if err != nil {
		t.Fatal(err)
	}
	write(t, w.Path, "other", "unrelated")
	if _, err = b.Commit(ctx, w, s, "task"); err == nil {
		t.Fatal("unreviewed file accepted")
	}
	os.Remove(filepath.Join(w.Path, "other"))
	write(t, w.Path, "x.go", "package test\nfunc X() int { return 3 }\n")
	if _, err = b.Commit(ctx, w, s, "task"); err == nil {
		t.Fatal("post-review edit accepted")
	}
	write(t, w.Path, "x.go", "package test\nfunc X() int { return 2 }\n")
	c, err := b.Commit(ctx, w, s, "task\n\nexact bytes")
	if err != nil {
		t.Fatal(err)
	}
	if got := gitTest(t, w.Path, "rev-parse", "HEAD^{tree}"); got != s.Tree {
		t.Fatal(got, s.Tree)
	}
	landed, err := b.Integrate(ctx, w, c, "refs/heads/fae/integration", base)
	if err != nil || !landed.Landed {
		t.Fatal(landed, err)
	}
	again, err := b.Integrate(ctx, w, c, "refs/heads/fae/integration", base)
	if err != nil || again.ContainedBy != c.SHA || again.Containment != ContainedAncestor {
		t.Fatal(again, err)
	}
	if got := gitTest(t, repo, "rev-parse", "HEAD"); got != base {
		t.Fatal("developer head modified")
	}
	if got := gitTest(t, repo, "status", "--porcelain"); got != "" {
		t.Fatal("developer checkout modified", got)
	}
}
func TestRefAndWorktreeGuards(t *testing.T) {
	b, w, repo := setup(t)
	ctx := context.Background()
	if _, err := b.Tree(ctx, worktree.Binding{Path: repo, Branch: w.Branch}); err == nil {
		t.Fatal("primary accepted")
	}
	for _, files := range [][]string{{}, {"../x"}, {".git/config"}, {"x.go", "x.go"}} {
		if _, err := b.Snapshot(ctx, w, files); err == nil {
			t.Fatal("bad files", files)
		}
	}
	s, err := b.Snapshot(ctx, w, []string{"x.go"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := b.Commit(ctx, w, s, "empty tree allowed")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.Integrate(ctx, w, c, "refs/heads/main", s.Base); err == nil {
		t.Fatal("unowned branch")
	}
	gitTest(t, repo, "branch", "fae/integration")
	if result, err := b.Integrate(ctx, w, c, "refs/heads/fae/integration", "changed"); err != nil || result.ContainedBy == "" {
		t.Fatal("empty change should be contained without moving tip", result, err)
	}
}
func TestTestParserRefusesVacuityAndMalformed(t *testing.T) {
	for _, input := range []string{"not json\n", `{"Action":"run","Package":"p","Test":"TestX"}`} {
		r := RunEvidence{Output: input}
		if err := parseTests(&r, Command{TestID: "TestX"}); err == nil {
			t.Fatal("bad evidence", input)
		}
	}
	r := RunEvidence{Output: "{\"Action\":\"run\",\"Package\":\"p\",\"Test\":\"TestX\"}\n{\"Action\":\"skip\",\"Package\":\"p\",\"Test\":\"TestX\"}\n"}
	if err := parseTests(&r, Command{TestID: "TestX"}); err != nil || r.ExecutedCount != 0 {
		t.Fatal(r, err)
	}
}

func TestIndexPreservedAndHostileEnvironment(t *testing.T) {
	b, w, _ := setup(t)
	ctx := context.Background()
	write(t, w.Path, "x.go", "package test\nfunc X() int { return 7 }\n")
	gitTest(t, w.Path, "add", "x.go")
	before := gitTest(t, w.Path, "write-tree")
	write(t, w.Path, "x.go", "package test\nfunc X() int { return 8 }\n")
	t.Setenv("GIT_INDEX_FILE", filepath.Join(t.TempDir(), "hostile-index"))
	t.Setenv("GIT_WORK_TREE", t.TempDir())
	tree, err := b.Tree(ctx, w)
	if err != nil || tree == before {
		t.Fatal(tree, err)
	}
	if after := gitTest(t, w.Path, "write-tree"); after != before {
		t.Fatal("real index modified")
	}
	b.Allowed = [][]string{{"env"}}
	r, err := b.Run(ctx, w, Command{Argv: b.Allowed[0], Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.Output, "hostile-index") || strings.Contains(r.Output, "GIT_WORK_TREE=") {
		t.Fatal("hostile environment inherited")
	}
}
func TestMutationAndCheckedOutTargetRefused(t *testing.T) {
	b, w, repo := setup(t)
	ctx := context.Background()
	b.Allowed = [][]string{{"touch", "new-file"}}
	if _, err := b.Run(ctx, w, Command{Argv: b.Allowed[0], Timeout: time.Second}); err == nil {
		t.Fatal("mutating host command accepted")
	}
	os.Remove(filepath.Join(w.Path, "new-file"))
	gitTest(t, repo, "checkout", "-b", "fae/integration")
	s, err := b.Snapshot(ctx, w, []string{"x.go"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := b.Commit(ctx, w, s, "task")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = b.Integrate(ctx, w, c, "refs/heads/fae/integration", s.Base); err == nil {
		t.Fatal("checked-out integration accepted")
	}
	if _, err = b.Commit(ctx, w, s, "retry stale snapshot"); err == nil {
		t.Fatal("stale base accepted")
	}
	if err = b.Reconcile(ctx, w, s.Base); err == nil {
		t.Fatal("changed reconcile base accepted")
	}
}

func TestFreshCompileObservation(t *testing.T) {
	root := t.TempDir()
	write(t, root, "x.go", "package x\n")
	args := []string{"go", "test", "-json", "-count=1", "-a", "-x", "./..."}
	valid := "/tool/compile -pack ./x.go\n"
	if !observedCompile("", valid, root, args) {
		t.Fatal("actual source compile missed")
	}
	event, _ := json.Marshal(map[string]string{"Action": "build-output", "Output": valid})
	if !observedCompile(string(event), "", root, args) {
		t.Fatal("Go JSON build compile missed")
	}
	for _, line := range []string{"/tool/link ./x.go", "/tool/compile /usr/local/go/src/runtime/runtime.go", "/tool/compile $WORK/_testmain.go"} {
		if observedCompile("", line, root, args) {
			t.Fatal("non-worktree compile accepted", line)
		}
	}
	spoof, _ := json.Marshal(map[string]string{"Action": "output", "Output": valid})
	if observedCompile(string(spoof), "", root, args) {
		t.Fatal("test output spoof accepted")
	}
	if observedCompile("", valid, root, args[:4]) {
		t.Fatal("cached compilation marked fresh")
	}
}

func TestCommandCancelledBeforeRegistrationRemovesEmptyRegistry(t *testing.T) {
	b, w, _ := setup(t)
	root := filepath.Dir(w.Path)
	b.Allowed = [][]string{{"sleep", "5"}}
	// Timeout begins after worktree validation and tree capture. One nanosecond
	// expires during shim installation, before StartLinked can register a process.
	_, err := b.Run(context.Background(), w, Command{Argv: b.Allowed[0], Timeout: time.Nanosecond})
	if err == nil {
		t.Fatal("expired command launched")
	}
	residue, err := filepath.Glob(filepath.Join(root, "command-*"))
	if err != nil || len(residue) != 0 {
		t.Fatal("cancelled launch leaked owner registry", residue, err)
	}
}

// FAE-HOST-020: the system temporary directory is RAM-backed. A brokered
// build or test gets a private scratch TMPDIR on disk beside its worktree,
// removed on every exit path; ownership registries and index files stay on
// disk too.
func TestBrokerScratchOnDiskAndRemoved(t *testing.T) {
	b, w, _ := setup(t)
	ram, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", ram)
	parent := filepath.Dir(w.Path)
	for _, exit := range []string{"0", "3"} {
		argv := []string{"/bin/sh", "-c", `printf %s "$TMPDIR" && mkdir "$TMPDIR/scratch" && exit ` + exit}
		b.Allowed = [][]string{argv}
		r, err := b.Run(context.Background(), w, Command{Argv: argv, Timeout: 5 * time.Second})
		if err != nil || r.ExitCode != map[string]int{"0": 0, "3": 3}[exit] {
			t.Fatalf("%+v %v", r, err)
		}
		if filepath.Dir(r.Output) != parent || !strings.HasPrefix(filepath.Base(r.Output), "command-tmp-") {
			t.Fatal("scratch not beside the worktree", r.Output)
		}
		if _, err = os.Stat(r.Output); !os.IsNotExist(err) {
			t.Fatal("scratch survived exit", exit, err)
		}
	}
	if _, err = b.Snapshot(context.Background(), w, []string{"x.go"}); err != nil {
		t.Fatal(err)
	}
	if residue, err := os.ReadDir(ram); err != nil || len(residue) != 0 {
		t.Fatal("system temporary directory used", residue, err)
	}
}

// A HostCommand environment policy sets exactly its declared variables over
// the sanitized base; daemon variables of the same kind are not inherited.
func TestCommandEnvironmentPolicy(t *testing.T) {
	b, w, _ := setup(t)
	t.Setenv("GOFLAGS", "-daemon-leak")
	b.Allowed = [][]string{{"env"}}
	r, err := b.Run(context.Background(), w, Command{Argv: b.Allowed[0], Timeout: time.Second})
	if err != nil || strings.Contains(r.Output, "GOFLAGS=") {
		t.Fatal("undeclared variable inherited", err)
	}
	r, err = b.Run(context.Background(), w, Command{Argv: b.Allowed[0], Timeout: time.Second, Environment: []string{"CGO_ENABLED=0", "GOFLAGS=-mod=mod"}})
	if err != nil || !strings.Contains(r.Output, "GOFLAGS=-mod=mod\n") || !strings.Contains(r.Output, "CGO_ENABLED=0\n") || strings.Contains(r.Output, "-daemon-leak") {
		t.Fatal("declared environment not applied", r.Output, err)
	}
	t.Setenv("FAE_HOME", "inherited-daemon-home")
	t.Setenv("FAE_INSTANCE", "inherited-daemon-instance")
	r, err = b.Run(context.Background(), w, Command{Argv: b.Allowed[0], Timeout: time.Second, Environment: []string{"FAE_HOME=/isolated/test-home", "FAE_INSTANCE=test-fixture"}})
	if err != nil || !strings.Contains(r.Output, "FAE_HOME=/isolated/test-home\n") || !strings.Contains(r.Output, "FAE_INSTANCE=test-fixture\n") || strings.Contains(r.Output, "inherited-daemon-") {
		t.Fatal("explicit runtime selection did not override sanitized parent", r.Output, err)
	}

}
