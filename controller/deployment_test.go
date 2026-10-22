package controller

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestOverlapProtection(t *testing.T) {
	for m := 0; m < 32; m++ {
		a, b, c, d, e := m&1 != 0, m&2 != 0, m&4 != 0, m&8 != 0, m&16 != 0
		if MayReleaseOverlap(a, b, c, d, e) != (a || (b && !c && !d && !e)) {
			t.Fatal(m)
		}
	}
}
func deploymentRepository(t *testing.T) (string, string, string) {
	t.Helper()
	root := t.TempDir()
	ctx := context.Background()
	run := func(args ...string) string {
		t.Helper()
		out, e := Git(ctx, root, args...)
		if e != nil {
			t.Fatal(args, e)
		}
		return out
	}
	run("init")
	run("config", "user.name", "Test")
	run("config", "user.email", "test@example.invalid")
	os.WriteFile(filepath.Join(root, "file"), []byte("one"), 0600)
	run("add", ".")
	run("commit", "-m", "one")
	first := run("rev-parse", "HEAD")
	os.WriteFile(filepath.Join(root, "file"), []byte("two"), 0600)
	run("commit", "-am", "two")
	second := run("rev-parse", "HEAD")
	return root, first, second
}
func TestDeploymentFastForward(t *testing.T) {
	root, first, second := deploymentRepository(t)
	ctx := context.Background()
	Git(ctx, root, "update-ref", "refs/heads/integration", first)
	if e := FastForward(ctx, root, "refs/heads/integration", second); e != nil {
		t.Fatal(e)
	}
	if e := FastForward(ctx, root, "refs/heads/integration", second); e != nil {
		t.Fatal(e)
	}
	if e := FastForward(ctx, root, "refs/heads/integration", first); e == nil {
		t.Fatal("rewind accepted")
	}
	got, _ := Git(ctx, root, "rev-parse", "refs/heads/integration")
	if got != second {
		t.Fatal(got)
	}
	if e := FastForward(ctx, root, "HEAD", first); e == nil {
		t.Fatal("nonbranch")
	}
}
func TestBuildGatedPinsCleanSource(t *testing.T) {
	root, _, _ := deploymentRepository(t)
	ctx := context.Background()
	run := func(args ...string) string {
		out, e := Git(ctx, root, args...)
		if e != nil {
			t.Fatal(args, e)
		}
		return out
	}
	os.MkdirAll(filepath.Join(root, "cmd", "fae"), 0700)
	os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.invalid/fixture\n\ngo 1.23\n"), 0600)
	os.WriteFile(filepath.Join(root, "cmd", "fae", "main.go"), []byte("package main\nfunc main() {}\n"), 0600)
	run("add", ".")
	run("commit", "-m", "fixture")
	tip := run("rev-parse", "HEAD")
	run("update-ref", "refs/fae/gated/test", tip)
	// Dirty operator checkout must not leak into the build.
	os.WriteFile(filepath.Join(root, "cmd", "fae", "main.go"), []byte("not valid Go"), 0600)
	t.Setenv("GOFLAGS", "-overlay=/nonexistent/poison.json")
	t.Setenv("GOWORK", "/nonexistent/go.work")
	storage := t.TempDir()
	binary, dir, got, e := BuildGated(ctx, root, "HEAD", "refs/fae/gated/test", storage)
	if e != nil {
		t.Fatal(e)
	}
	defer os.RemoveAll(dir)
	if filepath.Dir(dir) != storage {
		t.Fatal("deployment not durably placed")
	}
	if _, e := os.Stat(filepath.Join(dir, "source")); !os.IsNotExist(e) {
		t.Fatal("source checkout retained", e)
	}
	if got != tip {
		t.Fatal(got)
	}
	if revision, e := SourceRevision(binary); e != nil || revision != tip {
		t.Fatal(revision, e)
	}
	if e := exec.Command(binary).Run(); e != nil {
		t.Fatal(e)
	}
	if _, _, _, e = BuildGated(ctx, root, "HEAD~1", "refs/fae/gated/test"); e == nil {
		t.Fatal("ungated revision")
	}
	if _, _, _, e = BuildGated(ctx, root, "HEAD", "HEAD"); e == nil {
		t.Fatal("ungated namespace")
	}
	if _, e = SourceRevision(filepath.Join(root, "file")); e == nil {
		t.Fatal("invalid executable")
	}
}
