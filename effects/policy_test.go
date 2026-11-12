package effects

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xleelz/fae/worktree"
)

func TestPrepareCommitAndIntegrateEnforceCommitIdentityAndContentPolicy(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()

	runGit := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, string(out))
		}
		return strings.TrimSpace(string(out))
	}

	runGit(repo, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "app.go"), []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(repo, "add", "app.go")
	runGit(repo, "-c", "user.name=Init", "-c", "user.email=init@example.com", "commit", "-q", "-m", "initial")
	baseSHA := runGit(repo, "rev-parse", "HEAD")
	runGit(repo, "update-ref", "refs/heads/fae/main", baseSHA)

	mgr := &worktree.Manager{Repository: repo, Root: filepath.Join(t.TempDir(), "wt")}
	w, err := mgr.Ensure(ctx, "run1", "task1", baseSHA)
	if err != nil {
		t.Fatalf("Ensure worktree: %v", err)
	}

	broker := &Broker{
		Manager: mgr,
		CommitIdentity: &CommitIdentity{
			Name:     "Ada Lovelace",
			Email:    "ada@analytical.engine",
			CoAuthor: "Charles Babbage <charles@analytical.engine>",
		},
		ContentPolicy: ContentPolicy{
			DenyLiterals: []string{"SUPER_SECRET_TOKEN_123"},
			DenyPatterns: []string{`aws_secret_access_key\s*=\s*\S+`},
			DenyPaths:    []string{"secrets/**"},
		},
	}

	// 1. Diff containing a denied literal must be refused with index-only error
	if err := os.WriteFile(filepath.Join(w.Path, "app.go"), []byte("package main\n// super_secret_token_123\n"), 0644); err != nil {
		t.Fatal(err)
	}
	snapSecret, err := broker.Snapshot(ctx, w, []string{"app.go"})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	_, err = broker.PrepareCommit(ctx, w, snapSecret, "add feature")
	if err == nil {
		t.Fatalf("expected PrepareCommit to refuse denied literal in diff")
	}
	if !strings.Contains(err.Error(), "content_policy.deny_literals[0]") || strings.Contains(strings.ToLower(err.Error()), "super_secret_token_123") {
		t.Fatalf("error must name only index and never echo secret: %v", err)
	}

	// 2. Diff matching a denied regex pattern must be refused with index-only error
	if err := os.WriteFile(filepath.Join(w.Path, "app.go"), []byte("package main\n// AWS_SECRET_ACCESS_KEY = xyz987\n"), 0644); err != nil {
		t.Fatal(err)
	}
	snapRegex, err := broker.Snapshot(ctx, w, []string{"app.go"})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	_, err = broker.PrepareCommit(ctx, w, snapRegex, "add feature")
	if err == nil {
		t.Fatalf("expected PrepareCommit to refuse denied pattern in diff")
	}
	if !strings.Contains(err.Error(), "content_policy.deny_patterns[0]") || strings.Contains(err.Error(), "xyz987") || strings.Contains(err.Error(), "aws_secret") {
		t.Fatalf("error must name only index and never echo pattern/secret: %v", err)
	}

	// 3. Clean diff with CommitIdentity succeeds and sets author, committer, and Co-authored-by trailer
	if err := os.WriteFile(filepath.Join(w.Path, "app.go"), []byte("package main\nfunc Hello() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	snapClean, err := broker.Snapshot(ctx, w, []string{"app.go"})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	// Commit message containing secret literal must also be refused
	if _, err := broker.PrepareCommit(ctx, w, snapClean, "fix SUPER_SECRET_TOKEN_123 issue"); err == nil || !strings.Contains(err.Error(), "content_policy.deny_literals[0]") {
		t.Fatalf("expected PrepareCommit to refuse denied literal in commit message, got: %v", err)
	}

	prepared, err := broker.PrepareCommit(ctx, w, snapClean, "implement Hello")
	if err != nil {
		t.Fatalf("PrepareCommit clean: %v", err)
	}
	ev, err := broker.ApplyCommit(ctx, w, prepared)
	if err != nil {
		t.Fatalf("ApplyCommit: %v", err)
	}
	meta := runGit(w.Path, "log", "-1", "--format=%an|%ae|%cn|%ce|%B", ev.SHA)
	if !strings.HasPrefix(meta, "Ada Lovelace|ada@analytical.engine|Ada Lovelace|ada@analytical.engine|") {
		t.Fatalf("unexpected commit author/committer metadata: %s", meta)
	}
	if !strings.Contains(meta, "Co-authored-by: Charles Babbage <charles@analytical.engine>") {
		t.Fatalf("expected Co-authored-by trailer in commit message, got: %s", meta)
	}

	// 4. Integrate scans range and refuses if policy now denies something in the commit
	strictBroker := *broker
	strictBroker.ContentPolicy = ContentPolicy{
		DenyLiterals: []string{"func Hello"},
	}
	if _, err := strictBroker.Integrate(ctx, w, ev, "refs/heads/fae/main", baseSHA); err == nil || !strings.Contains(err.Error(), "content_policy.deny_literals[0]") || strings.Contains(err.Error(), "func Hello") {
		t.Fatalf("expected Integrate to refuse on content policy scan with index only, got: %v", err)
	}

	// Integrate with normal policy succeeds
	landed, err := broker.Integrate(ctx, w, ev, "refs/heads/fae/main", baseSHA)
	if err != nil || !landed.Landed {
		t.Fatalf("Integrate clean failed: %+v, err=%v", landed, err)
	}
}
