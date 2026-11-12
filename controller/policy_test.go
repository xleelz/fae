package controller

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xleelz/fae/workflow"
)

func TestLandEnforcesCommitIdentityAndContentPolicy(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()

	runGit := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = ws
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, string(out))
		}
		return strings.TrimSpace(string(out))
	}

	runGit("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(ws, "go.mod"), []byte("module example.com/test\n\ngo 1.22\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "calc.go"), []byte("package test\nfunc Add(a, b int) int { return a + b }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "calc_test.go"), []byte("package test\nimport \"testing\"\nfunc TestAdd(t *testing.T) { if Add(1, 2) != 3 { t.Fatal() } }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit("add", ".")
	runGit("-c", "user.name=Init", "-c", "user.email=init@example.com", "commit", "-q", "-m", "init")
	baseSHA := runGit("rev-parse", "HEAD")
	runGit("branch", "target-branch", baseSHA)

	// Create task commit on a task branch
	runGit("checkout", "-q", "-b", "task-branch", baseSHA)
	if err := os.WriteFile(filepath.Join(ws, "calc.go"), []byte("package test\nfunc Add(a, b int) int { return a + b }\nfunc Sub(a, b int) int { return a - b }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "calc.go")
	runGit("-c", "user.name=Orig", "-c", "user.email=orig@example.com", "commit", "-q", "-m", "add Sub")
	taskCommit := runGit("rev-parse", "HEAD")
	runGit("checkout", "-q", "main")

	// 1. Land with ContentPolicy matching the commit diff holds on "content-policy" with index only
	resRefused, err := Land(ctx, LandRequest{
		ID:        "land-1",
		Task:      "task-1",
		Workspace: ws,
		Target:    "refs/heads/target-branch",
		Held:      "refs/heads/fae/held/land-1",
		Gated:     "refs/fae/gated/target-branch",
		Commits:   []string{taskCommit},
		Files:     []string{"calc.go", "calc_test.go"},
		Filter:    "^TestAdd$",
		Policy:    DefaultLandPolicy(),
		Scratch:   filepath.Join(t.TempDir(), "scratch-1"),
		Admit:     func(context.Context) (func() error, error) { return func() error { return nil }, nil },
		ContentPolicy: workflow.ContentPolicy{
			DenyLiterals: []string{"func Sub"},
		},
	})
	if err != nil {
		t.Fatalf("Land returned error: %v", err)
	}
	if resRefused.Outcome != LandHeld || resRefused.FailingGate != "content-policy" {
		t.Fatalf("expected LandHeld on content-policy gate, got %+v", resRefused)
	}
	if !strings.Contains(resRefused.Reason, "content_policy.deny_literals[0]") || strings.Contains(resRefused.Reason, "func Sub") {
		t.Fatalf("expected index-only reason without secret text, got: %s", resRefused.Reason)
	}

	// 2. Land with CommitIdentity and passing ContentPolicy lands commit with configured identity and Co-authored-by
	resLanded, err := Land(ctx, LandRequest{
		ID:        "land-2",
		Task:      "task-1",
		Workspace: ws,
		Target:    "refs/heads/target-branch",
		Held:      "refs/heads/fae/held/land-2",
		Gated:     "refs/fae/gated/target-branch",
		Commits:   []string{taskCommit},
		Files:       []string{"calc.go", "calc_test.go"},
		Filter:      "^TestAdd$",
		QA:          [][]string{{"go", "test", "-json", "-count=1", "./..."}},
		Policy:      DefaultLandPolicy(),
		Environment: os.Environ(),
		Scratch:     filepath.Join(t.TempDir(), "scratch-2"),
		Admit:     func(context.Context) (func() error, error) { return func() error { return nil }, nil },
		CommitIdentity: &workflow.CommitIdentity{
			Name:     "Grace Hopper",
			Email:    "grace@navy.mil",
			CoAuthor: "Jean Bartik <jean@eniac.org>",
		},
		ContentPolicy: workflow.ContentPolicy{
			DenyLiterals: []string{"NEVER_MATCHES_SECRET"},
		},
	})
	if err != nil || resLanded.Outcome != LandLanded {
		t.Fatalf("expected LandLanded, got %+v, err=%v", resLanded, err)
	}
	meta := runGit("log", "-1", "--format=%an|%ae|%cn|%ce|%B", resLanded.Landed)
	if !strings.HasPrefix(meta, "Grace Hopper|grace@navy.mil|Grace Hopper|grace@navy.mil|") {
		t.Fatalf("unexpected landed commit identity: %s", meta)
	}
	if !strings.Contains(meta, "Co-authored-by: Jean Bartik <jean@eniac.org>") {
		t.Fatalf("expected Co-authored-by trailer on landed commit, got: %s", meta)
	}
}
