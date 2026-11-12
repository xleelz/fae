package controller

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type landFixture struct {
	t                  *testing.T
	root, repo, base   string
	scratch            string
	admitted, released int
}

func newLandFixture(t *testing.T) *landFixture {
	t.Helper()
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	f := &landFixture{t: t, root: root, repo: filepath.Join(root, "repo"), scratch: filepath.Join(root, "landings")}
	os.MkdirAll(f.repo, 0700)
	f.git("init", "-b", "main")
	f.write("go.mod", "module example.invalid/land\n\ngo 1.23\n")
	f.write("answer.go", "package land\n\nfunc answer() int { return 0 }\n")
	f.git("add", ".")
	f.git("commit", "-m", "base")
	f.base = f.git("rev-parse", "HEAD")
	return f
}
func (f *landFixture) git(args ...string) string {
	f.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", f.repo}, args...)...)
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "GIT_") {
			cmd.Env = append(cmd.Env, v)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_AUTHOR_NAME=Task", "GIT_AUTHOR_EMAIL=task@localhost", "GIT_COMMITTER_NAME=Task", "GIT_COMMITTER_EMAIL=task@localhost", "GIT_CONFIG_GLOBAL=/dev/null")
	out, e := cmd.CombinedOutput()
	if e != nil {
		f.t.Fatalf("git %v: %v %s", args, e, out)
	}
	return strings.TrimSpace(string(out))
}
func (f *landFixture) write(name, body string) {
	f.t.Helper()
	os.MkdirAll(filepath.Dir(filepath.Join(f.repo, name)), 0700)
	if e := os.WriteFile(filepath.Join(f.repo, name), []byte(body), 0600); e != nil {
		f.t.Fatal(e)
	}
}

// commitOn records files as one commit on a fresh branch from parent without
// touching the operator checkout, as a task worktree commit would.
func (f *landFixture) commitOn(parent, message string, files map[string]string) string {
	f.t.Helper()
	index := filepath.Join(f.root, "index-"+strings.ReplaceAll(message, " ", "-"))
	run := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", f.repo}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+index, "GIT_AUTHOR_NAME=Task", "GIT_AUTHOR_EMAIL=task@localhost", "GIT_COMMITTER_NAME=Task", "GIT_COMMITTER_EMAIL=task@localhost")
		out, e := cmd.CombinedOutput()
		if e != nil {
			f.t.Fatalf("git %v: %v %s", args, e, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("read-tree", parent)
	for name, body := range files {
		blob := filepath.Join(f.root, "blob")
		os.WriteFile(blob, []byte(body), 0600)
		sha := run("hash-object", "-w", blob)
		run("update-index", "--add", "--cacheinfo", "100644,"+sha+","+name)
	}
	tree := run("write-tree")
	cmd := exec.Command("git", "-C", f.repo, "commit-tree", tree, "-p", parent, "-m", message)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Task", "GIT_AUTHOR_EMAIL=task@localhost", "GIT_COMMITTER_NAME=Task", "GIT_COMMITTER_EMAIL=task@localhost")
	out, e := cmd.CombinedOutput()
	if e != nil {
		f.t.Fatal(e, string(out))
	}
	return strings.TrimSpace(string(out))
}

const landTest = "package land\n\nimport \"testing\"\n\nfunc TestBehavior(t *testing.T) {\n\tif answer() != 42 {\n\t\tt.Fatal(\"got 0 want 42\")\n\t}\n}\n"

func (f *landFixture) task() string {
	return f.commitOn(f.base, "task", map[string]string{"answer.go": "package land\n\nfunc answer() int { return 42 }\n", "answer_test.go": landTest})
}

// upstream advances main through the operator checkout with an unrelated file.
func (f *landFixture) upstream() string {
	f.write("other.go", "package land\n\nfunc other() int { return 1 }\n")
	f.git("add", "other.go")
	f.git("commit", "-m", "upstream")
	return f.git("rev-parse", "HEAD")
}

func (f *landFixture) request(id string, commits ...string) LandRequest {
	env := []string{}
	for _, v := range os.Environ() {
		key, _, _ := strings.Cut(v, "=")
		if key == "PATH" || key == "HOME" || key == "GOCACHE" || key == "GOMODCACHE" || key == "GOPATH" || key == "GOTOOLCHAIN" {
			env = append(env, v)
		}
	}
	policy := DefaultLandPolicy()
	return LandRequest{ID: id, Task: "task-" + id, Workspace: f.repo, Target: "refs/heads/main", Commits: commits, Files: []string{"answer.go", "answer_test.go"}, Filter: "TestBehavior", QA: [][]string{{"go", "test", "-json", "-count=1", "./..."}}, Policy: policy, Scratch: filepath.Join(f.scratch, id), Held: "refs/heads/fae/held/" + id, Gated: "refs/fae/gated/main", Environment: env,
		Admit: func(context.Context) (func() error, error) {
			f.admitted++
			return func() error { f.released++; return nil }, nil
		}}
}

func gate(r LandResult, name string) GateResult {
	for _, g := range r.Gates {
		if g.Gate == name {
			return g
		}
	}
	return GateResult{}
}

func TestLandCleanLandingRecordsEveryGateCount(t *testing.T) {
	f := newLandFixture(t)
	own := f.task()
	upstream := f.upstream()
	advanced := false
	r := f.request("clean", own)
	r.Advance = func(_ context.Context, res LandResult) error {
		advanced = true
		if f.git("rev-parse", "main") != upstream || res.Tip == "" || !LandingGate(res.Gates).Passed {
			t.Fatal("write-ahead record not before the target write", res)
		}
		return nil
	}
	res, e := Land(context.Background(), r)
	if e != nil || res.Outcome != LandLanded || !advanced {
		t.Fatalf("%v %+v", e, res)
	}
	if main := f.git("rev-parse", "main"); main != res.Landed || f.git("rev-parse", "main^") != upstream || res.Start != upstream {
		t.Fatal("target not fast-forwarded onto the moved tip", main, res)
	}
	if f.git("rev-parse", "refs/fae/gated/main") != res.Landed || res.Gated != "refs/fae/gated/main" {
		t.Fatal("gated reference not advanced to the landed sha")
	}
	if status := f.git("status", "--porcelain"); status != "" || !strings.Contains(f.git("show", "HEAD:answer.go"), "return 42") {
		t.Fatal("checked-out target not fast-forwarded cleanly", status)
	}
	for _, name := range []string{"own-hunk", "format", "compile", "filter", "qa", "suite"} {
		g := gate(res, name)
		if !g.Passed || !g.Applicable || g.Count < 1 {
			t.Fatalf("gate %s did not record a positive count: %+v", name, g)
		}
	}
	if g := gate(res, "frozen"); !g.Passed || g.Applicable {
		t.Fatal("undeclared frozen region", g)
	}
	if g := gate(res, "filter"); g.Count != 1 {
		t.Fatal("filter count", g)
	}
	if f.admitted != 1 || f.released != 1 {
		t.Fatal("admission not taken and released once", f.admitted, f.released)
	}
	if author := f.git("log", "-1", "--format=%an %cn", "main"); author != "Task Fae" {
		t.Fatal("landing did not preserve own authorship", author)
	}
	if _, e := os.Stat(filepath.Join(r.Scratch, "tree")); !os.IsNotExist(e) {
		t.Fatal("landing worktree retained", e)
	}
	if list := f.git("worktree", "list"); strings.Contains(list, r.Scratch) {
		t.Fatal("landing worktree still registered", list)
	}
	again, e := Land(context.Background(), f.request("again", own))
	if e != nil || again.Outcome != LandNothingOwn || f.git("rev-parse", "main") != res.Landed {
		t.Fatal("already landed work must be nothing of its own, never a success", again, e)
	}
}

func TestLandZeroMatchFilterHoldsAndKeepsTarget(t *testing.T) {
	f := newLandFixture(t)
	own := f.task()
	f.git("checkout", "--detach")
	start := f.git("rev-parse", "main")
	r := f.request("vacuous", own)
	r.Filter = "TestDoesNotExist"
	res, e := Land(context.Background(), r)
	if e != nil || res.Outcome != LandHeld || res.FailingGate != "filter" || !strings.Contains(res.Reason, "zero tests") {
		t.Fatalf("%v %+v", e, res)
	}
	if f.git("rev-parse", "main") != start {
		t.Fatal("held landing moved the target")
	}
	if f.git("rev-parse", res.Held) != res.Tip || res.Held != "refs/heads/fae/held/vacuous" {
		t.Fatal("attempt not kept on the held branch")
	}
	if _, e := exec.Command("git", "-C", f.repo, "rev-parse", "--verify", "--quiet", "refs/fae/gated/main").Output(); e == nil {
		t.Fatal("held landing advanced the gated reference")
	}
}

func TestLandFormattingDebtConflictAndAdmission(t *testing.T) {
	f := newLandFixture(t)
	debt := f.commitOn(f.base, "debt", map[string]string{"answer.go": "package land\n\nfunc answer( ) int { return 42 }\n", "answer_test.go": landTest})
	res, e := Land(context.Background(), f.request("debt", debt))
	if e != nil || res.Outcome != LandHeld || res.FailingGate != "format" || !strings.Contains(res.Reason, "func answer") {
		t.Fatalf("%v %+v", e, res)
	}
	own := f.task()
	f.write("answer.go", "package land\n\nfunc answer() int { return 7 }\n")
	f.git("commit", "-am", "conflicting upstream")
	start := f.git("rev-parse", "main")
	res, e = Land(context.Background(), f.request("conflict", own))
	if e != nil || res.Outcome != LandHeld || res.FailingGate != "conflict" || !strings.Contains(res.Reason, "answer.go") || f.git("rev-parse", "main") != start {
		t.Fatalf("%v %+v", e, res)
	}
	r := f.request("busy", own)
	r.Admit = func(context.Context) (func() error, error) {
		return nil, errors.New("heavy admission wait expired at memory")
	}
	if res, e = Land(context.Background(), r); e != nil || res.Outcome != LandInfrastructure || res.Held != "" {
		t.Fatal("admission timeout must be infrastructure, not failure", res, e)
	}
	r = f.request("moved", own)
	r.Expected = f.base
	if res, e = Land(context.Background(), r); e != nil || res.Outcome != LandInfrastructure {
		t.Fatal(res, e)
	}
	for _, bad := range []func(*LandRequest){
		func(r *LandRequest) { r.Target = "main" },
		func(r *LandRequest) { r.Target = "refs/heads/fae/task/x" },
		func(r *LandRequest) { r.Held = "refs/heads/held" },
		func(r *LandRequest) { r.Gated = "refs/heads/gated" },
		func(r *LandRequest) { r.Source = "HEAD"; r.Base = f.base },
		func(r *LandRequest) { r.Admit = nil },
	} {
		r := f.request("invalid", own)
		bad(&r)
		if _, e := Land(context.Background(), r); e == nil {
			t.Fatal("invalid landing request accepted", r)
		}
	}
}

func TestLandSalvageTrailAndDroppedProductionHunk(t *testing.T) {
	f := newLandFixture(t)
	passing := f.task()
	tree := f.git("rev-parse", passing+"^{tree}")
	f.upstream()
	start := f.git("rev-parse", "main")
	// The declaration omits the production file: salvage would ship the test
	// without its production code, so own-hunk presence must hold it.
	r := f.request("salvage-dropped")
	r.Source, r.Base, r.Files = tree, f.base, []string{"answer_test.go"}
	res, e := Land(context.Background(), r)
	if e != nil || res.Outcome != LandHeld || res.FailingGate != "own-hunk" || !strings.Contains(res.Reason, "answer.go") || f.git("rev-parse", "main") != start {
		t.Fatalf("%v %+v", e, res)
	}
	r = f.request("salvage")
	r.Source, r.Base = tree, f.base
	res, e = Land(context.Background(), r)
	if e != nil || res.Outcome != LandLanded || res.SourceTree != tree || res.Base != f.base || !strings.Contains(res.Patch, "return 42") || !gate(res, "own-hunk").Passed || gate(res, "own-hunk").Count < 2 {
		t.Fatalf("%v %+v", e, res)
	}
	if f.git("rev-parse", "main^") != start || !strings.Contains(f.git("show", "main:answer_test.go"), "TestBehavior") {
		t.Fatal("salvage not landed on the exact moved tip")
	}
}

func TestLandBatchStopsAtFirstHold(t *testing.T) {
	f := newLandFixture(t)
	f.git("checkout", "--detach")
	makeTask := func(name string) string {
		test := strings.ReplaceAll(strings.ReplaceAll(landTest, "TestBehavior", "Test"+name), "answer()", name+"()")
		return f.commitOn(f.base, "task "+name, map[string]string{name + ".go": "package land\n\nfunc " + name + "() int { return 42 }\n", name + "_test.go": test})
	}
	requests := []LandRequest{}
	for _, name := range []string{"First", "Second", "Third"} {
		r := f.request(name, makeTask(name))
		r.Files, r.Filter, r.Task = []string{name + ".go", name + "_test.go"}, "Test"+name, name
		requests = append(requests, r)
	}
	requests[1].Filter = "TestMissing"
	seen := 0
	results, e := LandBatch(context.Background(), requests, func(LandResult) { seen++ })
	if e != nil || len(results) != 3 || seen != 3 {
		t.Fatal(results, e)
	}
	if results[0].Outcome != LandLanded || results[1].Outcome != LandHeld || results[1].FailingGate != "filter" || results[2].Outcome != LandNotAttempted {
		t.Fatalf("%+v", results)
	}
	if f.git("rev-parse", "main") != results[0].Landed || results[1].Start != results[0].Landed {
		t.Fatal("batch did not chain the previous landing's tip")
	}
}

func TestOwnHunkPresenceDetectsDroppedHunk(t *testing.T) {
	f := newLandFixture(t)
	f.write("big.go", "package land\n\nfunc a() int { return 1 }\n\nfunc b() int { return 2 }\n\nfunc c() int { return 3 }\n\nfunc d() int { return 4 }\n")
	f.git("add", ".")
	f.git("commit", "-m", "big")
	base := f.git("rev-parse", "HEAD")
	own := f.commitOn(base, "two hunks", map[string]string{"big.go": "package land\n\nfunc a() int { return 10 }\n\nfunc b() int { return 2 }\n\nfunc c() int { return 3 }\n\nfunc d() int { return 40 }\n"})
	f.git("checkout", "--detach", own)
	hunks, reviewed, e := OwnHunkPresence(context.Background(), f.repo, f.repo, base, own)
	if e != nil || len(hunks) != 2 || !hunks[0].Present || !hunks[1].Present || len(reviewed) != 1 {
		t.Fatal(hunks, reviewed, e)
	}
	// The landed tree keeps the first hunk but lost the second.
	f.write("big.go", "package land\n\nfunc a() int { return 10 }\n\nfunc b() int { return 2 }\n\nfunc c() int { return 3 }\n\nfunc d() int { return 4 }\n")
	hunks, _, e = OwnHunkPresence(context.Background(), f.repo, f.repo, base, own)
	if e != nil || !hunks[0].Present || hunks[1].Present {
		t.Fatal(hunks, e)
	}
	if r := OwnHunkGate(hunks, reviewed, reviewed); r.Passed || !strings.Contains(r.Reason, "big.go") {
		t.Fatal(r)
	}
}

func TestLandRefusesDirtyCheckedOutTarget(t *testing.T) {
	f := newLandFixture(t)
	own := f.task()
	start := f.git("rev-parse", "main")
	f.write("answer.go", "package land\n\nfunc answer() int { return -1 }\n")
	res, e := Land(context.Background(), f.request("dirty", own))
	if e != nil || res.Outcome != LandHeld || res.FailingGate != "target-write" || f.git("rev-parse", "main") != start {
		t.Fatalf("%v %+v", e, res)
	}
	if body, _ := os.ReadFile(filepath.Join(f.repo, "answer.go")); !strings.Contains(string(body), "return -1") {
		t.Fatal("operator's local change was touched")
	}
	if _, e := exec.Command("git", "-C", f.repo, "rev-parse", "--verify", "--quiet", "refs/fae/gated/main").Output(); e == nil {
		t.Fatal("refused landing advanced the gated reference")
	}
}

func TestDependencyOrder(t *testing.T) {
	order, e := DependencyOrder([]string{"c", "b", "a"}, [][2]string{{"a", "b"}, {"b", "c"}, {"x", "a"}, {"a", "y"}})
	if e != nil || strings.Join(order, ",") != "a,b,c" {
		t.Fatal(order, e)
	}
	if order, e = DependencyOrder([]string{"b", "a"}, nil); e != nil || strings.Join(order, ",") != "b,a" {
		t.Fatal("independent tasks must keep the requested order", order, e)
	}
	if _, e = DependencyOrder([]string{"a", "b"}, [][2]string{{"a", "b"}, {"b", "a"}}); e == nil {
		t.Fatal("cycle accepted")
	}
	if _, e = DependencyOrder([]string{"a", "a"}, nil); e == nil {
		t.Fatal("duplicate accepted")
	}
}

func TestLandBatchRecordsInternalErrorsAsInfrastructure(t *testing.T) {
	f := newLandFixture(t)
	r := f.request("missing", strings.Repeat("0", 40))
	results, e := LandBatch(context.Background(), []LandRequest{r, f.request("next", f.task())}, nil)
	if len(results) != 2 || results[0].Outcome != LandInfrastructure || results[1].Outcome != LandNotAttempted {
		t.Fatal(results, e)
	}
	r = f.request("no-go", f.task())
	r.Environment = []string{"PATH=/nonexistent"}
	res, e := Land(context.Background(), r)
	if e != nil || res.Outcome != LandHeld || res.FailingGate != "compile" || !strings.Contains(res.Reason, "not found") {
		t.Fatal("missing toolchain must hold, not pass", res, e)
	}
}
