package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func reviewPhysical(t *testing.T) string {
	t.Helper()
	p, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	return p
}

func TestReviewSymlinkRefusalBeforeWrite(t *testing.T) {
	root := reviewPhysical(t)
	foreign := reviewPhysical(t)
	if e := os.Symlink(foreign, filepath.Join(root, "linked")); e != nil {
		t.Fatal(e)
	}
	target := filepath.Join(root, "linked", "must-not-exist")
	if e := mkdirPrivate(target); e == nil {
		t.Fatal("symlink parent accepted")
	}
	if _, e := os.Stat(filepath.Join(foreign, "must-not-exist")); !os.IsNotExist(e) {
		t.Fatal("refusal wrote through symlink", e)
	}
}
func TestReviewStartWorkspaceMismatchWritesNothing(t *testing.T) {
	root := reviewPhysical(t)
	other := reviewPhysical(t)
	_, e := Start(context.Background(), Config{Root: root, Instance: "test", Workspace: other, DoltBinary: "does-not-exist"})
	if e == nil {
		t.Fatal("mismatched workspace accepted")
	}
	entries, e := os.ReadDir(root)
	if e != nil {
		t.Fatal(e)
	}
	if len(entries) != 0 {
		t.Fatal("refused startup wrote instance", entries)
	}
}
func TestReviewPathsRejectNoncanonicalRootAndInstance(t *testing.T) {
	root := reviewPhysical(t)
	for _, r := range []string{root + "/", root + "/../x", "relative"} {
		if _, e := Paths(r, "test"); e == nil {
			t.Fatal("invalid root accepted", r)
		}
	}
	for _, instance := range []string{"..", "a/b", "", "a b"} {
		if _, e := Paths(root, instance); e == nil {
			t.Fatal("invalid instance accepted", instance)
		}
	}
	if _, e := Paths(root, "test_1"); e != nil {
		t.Fatal(e)
	}
}
func TestReviewScopedEnvironment(t *testing.T) {
	t.Setenv("FAE_WORKSPACE", "wrong")
	t.Setenv("DOLT_ROOT_PATH", "wrong")
	env := scopedEnv("/private/example")
	for _, value := range env {
		if value == "FAE_WORKSPACE=wrong" || value == "DOLT_ROOT_PATH=wrong" {
			t.Fatal("ambient control-plane config leaked", value)
		}
	}
}

func TestReviewGitEnvironmentCannotSelectForeignRepository(t *testing.T) {
	local := reviewPhysical(t)
	foreign := reviewPhysical(t)
	for _, dir := range []string{local, foreign} {
		cmd := exec.Command("git", "init", "-q", dir)
		cmd.Env = cleanGitEnv()
		if output, e := cmd.CombinedOutput(); e != nil {
			t.Fatalf("git init: %v %s", e, output)
		}
	}
	t.Setenv("GIT_DIR", filepath.Join(foreign, ".git"))
	t.Setenv("GIT_WORK_TREE", foreign)
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	cmd.Dir = local
	cmd.Env = cleanGitEnv()
	out, e := cmd.Output()
	if e != nil {
		t.Fatal(e)
	}
	if strings.TrimSpace(string(out)) != local {
		t.Fatalf("ambient environment selected foreign repository: %s", out)
	}
}
