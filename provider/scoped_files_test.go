package provider

import (
	"context"
	"github.com/xleelz/fae/workflow"
	"os"
	"path/filepath"
	"testing"
)

func TestGlobWritesNewFilesAndOutOfScopeRevert(t *testing.T) {
	root := t.TempDir()
	start, err := SnapshotWorktreeTree(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	tb := AgenticToolbox{Worktree: root, Profile: workflow.Profile{Allowed: []string{"write", "read"}}, WriteScope: []string{"**"}, WriteScopes: [][]string{{"gen/**"}, {"gen/a/**"}}}
	if _, err = tb.Call(context.Background(), AgenticToolCall{Tool: "write", Path: "gen/a/new.yaml", Content: "ok"}); err != nil {
		t.Fatal(err)
	}
	if _, err = tb.Call(context.Background(), AgenticToolCall{Tool: "write", Path: "gen/b/x.yaml", Content: "bad"}); err == nil {
		t.Fatal("scope intersection bypass")
	}
	if err = os.MkdirAll(filepath.Join(root, "gen/b"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "gen/b/x.yaml"), []byte("bad"), 0600); err != nil {
		t.Fatal(err)
	}
	offending, err := VerifyAndRevertScopedWorktree(context.Background(), root, start, nil, nil, false, tb.WriteScopes, nil, false)
	if err != nil || len(offending) != 1 || offending[0] != "gen/b/x.yaml" {
		t.Fatalf("scope collection %v %v", offending, err)
	}
	if _, err = os.Stat(filepath.Join(root, "gen/a/new.yaml")); err != nil {
		t.Fatal("allowed new file lost", err)
	}
	if _, err = os.Stat(filepath.Join(root, "gen/b/x.yaml")); !os.IsNotExist(err) {
		t.Fatal("offending file not reverted")
	}
}
func TestArtifactsOutputDirectoryAndStagingPermissions(t *testing.T) {
	root, out, stage := t.TempDir(), t.TempDir(), t.TempDir()
	tb := AgenticToolbox{Worktree: root, OutputDir: out, StagingDir: stage, ArtifactsOnly: true, Profile: workflow.Profile{Allowed: []string{"write", "read"}}}
	if _, err := tb.Call(context.Background(), AgenticToolCall{Tool: "write", Path: "report.yaml", Content: "report"}); err != nil {
		t.Fatal(err)
	}
	got, err := tb.Call(context.Background(), AgenticToolCall{Tool: "read", Path: filepath.Join(out, "report.yaml"), StartLine: 1, EndLine: 1})
	if err != nil || got.Content != "report" {
		t.Fatalf("later stage read %q %v", got.Content, err)
	}
	if _, err := tb.Call(context.Background(), AgenticToolCall{Tool: "write", Path: filepath.Join(root, "bad"), Content: "bad"}); err == nil {
		t.Fatal("artifact task wrote worktree")
	}
	if _, err := tb.Call(context.Background(), AgenticToolCall{Tool: "write", Path: filepath.Join(stage, "a.yaml"), Content: "stage"}); err == nil {
		t.Fatal("read-only staging written")
	}
	tb.StagingWrite = true
	if _, err := tb.Call(context.Background(), AgenticToolCall{Tool: "write", Path: filepath.Join(stage, "a.yaml"), Content: "stage"}); err != nil {
		t.Fatal(err)
	}
}

func TestArtifactsBranchScopeEnforcedInsideOutputDirectory(t *testing.T) {
	root, out := t.TempDir(), t.TempDir()
	toolbox := AgenticToolbox{Worktree: root, OutputDir: out, ArtifactsOnly: true, WriteScopes: [][]string{{"fragments/**"}, {"fragments/a.yaml"}}, Profile: workflow.Profile{Allowed: []string{"read", "write"}}}
	if _, err := toolbox.Call(context.Background(), AgenticToolCall{Tool: "write", Path: "fragments/a.yaml", Content: "own"}); err != nil {
		t.Fatal(err)
	}
	if _, err := toolbox.Call(context.Background(), AgenticToolCall{Tool: "write", Path: "fragments/b.yaml", Content: "sibling"}); err == nil {
		t.Fatal("sibling output write accepted")
	}
	before, err := SnapshotTaskDirectory(out)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(out, "fragments/b.yaml"), []byte("bypass"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err = CheckTaskDirectoryWrites(out, before, nil, true, toolbox.WriteScopes); err == nil {
		t.Fatal("native sibling output bypass accepted")
	}
	if _, err = os.Stat(filepath.Join(out, "fragments/b.yaml")); !os.IsNotExist(err) {
		t.Fatal("sibling output not reverted", err)
	}
}
