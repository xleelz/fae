package provider

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/xleelz/fae/workflow"
)

func TestReadGuardFilteringToolsReadRootsAndShellView(t *testing.T) {
	ctx := context.Background()
	wt := t.TempDir()
	extRoot := t.TempDir()
	logDir := t.TempDir()

	// Initialize git repo in wt with allowed and denied files
	runGit := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, string(out))
		}
	}
	runGit(wt, "init", "-q")
	writeFile := func(base, rel, content string) {
		t.Helper()
		full := filepath.Join(base, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(wt, "src/allowed.go", "package src\nconst Allowed = 1\n")
	writeFile(wt, "src/secret/hidden.go", "package secret\nconst Token = \"topsecret\"\n")
	writeFile(wt, "keys/private.pem", "PRIVATE KEY DATA\n")
	runGit(wt, "add", ".")
	runGit(wt, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-q", "-m", "init")

	// Populate external read root
	writeFile(extRoot, "api/spec.md", "# Public Spec\nhello world\n")
	writeFile(extRoot, "internal/secret.txt", "do not read\n")

	var audited []RefusedReadEvent
	guard := &ReadGuard{
		TaskID:    "task-sec-008",
		Role:      "coder",
		Worktree:  wt,
		Policy:    &workflow.ReadPolicy{Allow: []string{"src/**", "api/**"}, Deny: []string{"src/secret/**"}},
		DenyPaths: []string{"keys/**"},
		ReadRoots: map[string]ReadRootView{
			"docs_root": {
				Name: "docs_root",
				Path: extRoot,
				Deny: []string{"internal/**"},
			},
		},
		LogDirectory: logDir,
		OnRefusedRead: func(task, role, path string) {
			audited = append(audited, RefusedReadEvent{Task: task, Role: role, Path: path})
		},
	}

	// 1. Inline file brief filtering
	filtered := guard.FilterInlineFiles([]string{"src/allowed.go", "src/secret/hidden.go", "keys/private.pem"})
	if !slices.Equal(filtered, []string{"src/allowed.go"}) {
		t.Fatalf("FilterInlineFiles got %v, want [src/allowed.go]", filtered)
	}
	if len(audited) != 2 || audited[0].Path != "src/secret/hidden.go" || audited[1].Path != "keys/private.pem" {
		t.Fatalf("expected 2 refused-read audit events from inline filtering, got %+v", audited)
	}

	// 2. Repository listing filtering
	repoRaw, err := listRepositoryFiltered(ctx, wt, logDir, guard.Policy, guard.DenyPaths)
	if err != nil {
		t.Fatalf("listRepositoryFiltered: %v", err)
	}
	if !strings.Contains(string(repoRaw), "src/allowed.go") || strings.Contains(string(repoRaw), "hidden.go") || strings.Contains(string(repoRaw), "private.pem") {
		t.Fatalf("repository listing leaked denied paths: %s", string(repoRaw))
	}

	// 3. Tool Read, List, SearchGlob, SearchRegex on workspace and read_root
	data, err := guard.Read("src/allowed.go", 0, 0)
	if err != nil || !strings.Contains(string(data), "Allowed = 1") {
		t.Fatalf("guard.Read allowed file failed: %v", err)
	}
	if _, err := guard.Read("src/secret/hidden.go", 0, 0); err == nil {
		t.Fatalf("expected guard.Read on denied path to fail")
	}
	entries, err := guard.List("src")
	if err != nil || !slices.Equal(entries, []string{"src/allowed.go"}) {
		t.Fatalf("guard.List(src) got %v, err=%v", entries, err)
	}
	globMatches, err := guard.SearchGlob("**/*.go")
	if err != nil || !slices.Equal(globMatches, []string{"src/allowed.go"}) {
		t.Fatalf("guard.SearchGlob got %v, err=%v", globMatches, err)
	}
	regexMatches, err := guard.SearchRegex("const", ".")
	if err != nil || len(regexMatches) != 1 || regexMatches[0].Path != "src/allowed.go" {
		t.Fatalf("guard.SearchRegex got %+v, err=%v", regexMatches, err)
	}

	// External read_root reads, lists, searches, and write protection
	extData, err := guard.Read("docs_root:api/spec.md", 0, 0)
	if err != nil || !strings.Contains(string(extData), "Public Spec") {
		t.Fatalf("guard.Read on allowed read_root file failed: %v", err)
	}
	if _, err := guard.Read("docs_root:internal/secret.txt", 0, 0); err == nil {
		t.Fatalf("expected guard.Read on denied read_root file to be refused")
	}
	if err := guard.CheckWrite("docs_root:api/spec.md"); err == nil {
		t.Fatalf("expected CheckWrite on read_root to be refused")
	}
	if err := guard.CheckWrite(filepath.Join(extRoot, "api/spec.md")); err == nil {
		t.Fatalf("expected CheckWrite on read_root absolute path to be refused")
	}
	if err := guard.CheckWrite("keys/private.pem"); err == nil {
		t.Fatalf("expected CheckWrite on deny_paths to be refused")
	}

	// 4. Shell filesystem view masking (MaskWorktree & RunShell)
	out, err := guard.RunShell(ctx, []string{"find", "src", "-type", "f"}, nil)
	if err != nil && !strings.Contains(err.Error(), "session filesystem isolation unavailable") {
		t.Fatalf("RunShell find: %v", err)
	}
	if err == nil && (strings.Contains(string(out), "hidden.go") || strings.Contains(string(out), "private.pem") || !strings.Contains(string(out), "src/allowed.go")) {
		t.Fatalf("shell view leaked denied files: %s", string(out))
	}
	// Verify denied files were restored to the worktree after RunShell completed
	if _, err := os.Stat(filepath.Join(wt, "src/secret/hidden.go")); err != nil {
		t.Fatalf("expected masked file src/secret/hidden.go to be restored after shell command: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt, "keys/private.pem")); err != nil {
		t.Fatalf("expected masked file keys/private.pem to be restored after shell command: %v", err)
	}

	// Direct command referencing denied path is refused and audited
	if _, err := guard.RunShell(ctx, []string{"cat", "src/secret/hidden.go"}, nil); err == nil {
		t.Fatalf("expected RunShell referencing denied path to be refused")
	}

	// Verify bridge refused-reads log file
	logged := ReadRefusedReadsLog(logDir)
	if len(logged) < 4 {
		t.Fatalf("expected at least 4 refused read events in bridge log, got %+v", logged)
	}
}
