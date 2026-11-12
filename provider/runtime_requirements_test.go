package provider

import (
	"context"
	"encoding/json"
	agentruntime "github.com/xleelz/fae/runtime"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/xleelz/fae/workflow"
)

func TestSelectiveIsolationRequirements(t *testing.T) {
	writable, readonly := t.TempDir(), t.TempDir()
	if err := os.Chmod(readonly, 0555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(readonly, 0700)
	for _, tc := range []struct {
		name           string
		shell, command bool
		policy         *workflow.ReadPolicy
		roots          map[string]ReadRootView
		want           bool
	}{
		{name: "shellless-filter", policy: &workflow.ReadPolicy{Deny: []string{"secret"}}},
		{name: "unrestricted-shell", shell: true},
		{name: "restricted-shell", shell: true, policy: &workflow.ReadPolicy{Deny: []string{"secret"}}, want: true},
		{name: "writable-root-shell", shell: true, roots: map[string]ReadRootView{"input": {Path: writable}}, want: true},
		{name: "writable-root-command", command: true, roots: map[string]ReadRootView{"input": {Path: writable}}, want: true},
		{name: "output-only-command", command: true, roots: map[string]ReadRootView{"output": {Path: writable}}},
		{name: "readonly-root-shell", shell: true, roots: map[string]ReadRootView{"input": {Path: readonly}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RequiresSessionIsolation(tc.shell, tc.command, tc.policy, nil, tc.roots); got != tc.want {
				t.Fatalf("requires=%v want=%v", got, tc.want)
			}
		})
	}
	guard := ReadGuard{Worktree: writable, WritableViews: map[string]func(string) bool{writable: func(string) bool { return true }}}
	cmd := exec.Command("true")
	cmd.Dir = writable
	view, err := guard.ConfigureSessionView(cmd, t.TempDir(), nil)
	if err != nil {
		t.Fatal("output-only session requires unavailable namespaces", err)
	}
	defer view.Close()
	if view.ownedPrivate != "" {
		t.Fatal("unnecessary private view")
	}
}
func TestIsolationProbeReportsMissingNamespaceTool(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	fact := ProbeSessionIsolation(context.Background(), t.TempDir())
	if fact.Available || fact.Step != "namespace-tool" || fact.Remedy == "" {
		t.Fatalf("missing capability not diagnosed: %+v", fact)
	}
}
func TestFilteredViewRetainsInRootSymlinks(t *testing.T) {
	root, copy := t.TempDir(), filepath.Join(t.TempDir(), "view")
	if err := os.WriteFile(filepath.Join(root, "actual"), []byte("readable"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "actual"), filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	if err := copySessionTree(root, copy, func(string) bool { return true }); err != nil {
		t.Fatal(err)
	}
	target, err := os.Readlink(filepath.Join(copy, "alias"))
	if err != nil || filepath.IsAbs(target) {
		t.Fatal("absolute link not rebased inside copy", target, err)
	}
	data, err := os.ReadFile(filepath.Join(copy, "alias"))
	if err != nil || string(data) != "readable" {
		t.Fatal("in-root link unreadable", err)
	}
	if err = os.Symlink(filepath.Join(t.TempDir(), "outside"), filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if err = copySessionTree(root, filepath.Join(t.TempDir(), "bad"), func(string) bool { return true }); err == nil {
		t.Fatal("escaping link accepted")
	}
}
func TestViewSnapshotsStreamContentsAndCleanupOnlyOwnedCopy(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "large"), make([]byte, 2<<20), 0600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := snapshotViewDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot["large"].data) != 0 || snapshot["large"].digest == "" {
		t.Fatal("snapshot retains file contents instead of digest")
	}
	owned := filepath.Join(t.TempDir(), "view")
	if err = os.Mkdir(owned, 0700); err != nil {
		t.Fatal(err)
	}
	view := &SessionView{ownedPrivate: owned}
	if err = view.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(owned); !os.IsNotExist(err) {
		t.Fatal("private copy left behind", err)
	}
	if _, err = os.Stat(filepath.Join(root, "large")); err != nil {
		t.Fatal("source removed", err)
	}
}

func TestNativeDirectoryCollectorRestoresForbiddenAndRetainsAllowedWrites(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "protected"), []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	collector, err := NewNativeDirectoryCollector(root, t.TempDir(), func(path string) bool { return path == "allowed" })
	if err != nil {
		t.Fatal(err)
	}
	defer collector.Close()
	for path, content := range map[string]string{"allowed": "generated", "protected": "changed", "forbidden-new": "bad"} {
		if err = os.WriteFile(filepath.Join(root, path), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err = collector.Verify(); err == nil {
		t.Fatal("unauthorized writes accepted")
	}
	if data, e := os.ReadFile(filepath.Join(root, "protected")); e != nil || string(data) != "original" {
		t.Fatal("protected contents not restored", e)
	}
	if data, e := os.ReadFile(filepath.Join(root, "allowed")); e != nil || string(data) != "generated" {
		t.Fatal("allowed output lost", e)
	}
	if _, e := os.Stat(filepath.Join(root, "forbidden-new")); !os.IsNotExist(e) {
		t.Fatal("forbidden new output retained", e)
	}
}

func TestNativeCollectorPreservesFinishedForeignGrantWrites(t *testing.T) {
	root, logs := t.TempDir(), t.TempDir()
	registry := filepath.Join(t.TempDir(), "grants.json")
	document := workflow.SharedWriteRegistry{Epoch: 1, Grants: []workflow.SharedWriteGrant{{Session: "own", Active: true, Scopes: []workflow.SharedWriteScope{{Root: root, Layers: [][]string{{"left/**"}}}}}, {Session: "peer", Active: false, Scopes: []workflow.SharedWriteScope{{Root: root, Layers: [][]string{{"right/**"}}}}}}}
	raw, _ := json.Marshal(document)
	if err := os.WriteFile(registry, raw, 0600); err != nil {
		t.Fatal(err)
	}
	collector, err := NewNativeDirectoryCollector(root, logs, func(path string) bool { return strings.HasPrefix(path, "left/") })
	if err != nil {
		t.Fatal(err)
	}
	defer collector.Close()
	collector.SharedWrites(registry, "own")
	for _, dir := range []string{"left", "right"} {
		if err = os.Mkdir(filepath.Join(root, dir), 0700); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(root, dir, "file"), []byte(dir), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err = collector.Verify(); err != nil {
		t.Fatal("legitimate peer write refused", err)
	}
	if data, e := os.ReadFile(filepath.Join(root, "right/file")); e != nil || string(data) != "right" {
		t.Fatal("finished peer write removed", e)
	}
}
func TestNativeCollectorRefreshesDescendantOwnershipAfterLaunch(t *testing.T) {
	root, logs := t.TempDir(), t.TempDir()
	child := filepath.Join(root, "child")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(child, "original"), []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	registry := filepath.Join(t.TempDir(), "owners.json")
	write := func(owners []any) {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"version": 1, "owners": owners})
		if err := os.WriteFile(registry, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	parent := map[string]any{"task": "parent", "output_dir": root, "ancestors": []string{}}
	write([]any{parent})
	collector, err := NewNativeDirectoryCollector(root, logs, func(string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	defer collector.Close()
	collector.OutputOwnership(registry, "parent")
	write([]any{parent, map[string]any{"task": "child", "output_dir": child, "ancestors": []string{"parent"}}})
	if err = os.WriteFile(filepath.Join(child, "original"), []byte("parent-change"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = collector.Verify(); err == nil {
		t.Fatal("post-launch descendant ownership ignored")
	}
	if data, e := os.ReadFile(filepath.Join(child, "original")); e != nil || string(data) != "before" {
		t.Fatal("descendant output not restored", e)
	}
}
func TestClaudeAgenticArgsKeepRegisteredHooksEnabled(t *testing.T) {
	launch := agentruntime.BridgeConfig{Launch: agentruntime.Launch{Session: "agentic", ClaudeSettings: "/session/settings.json", Binding: workflow.Binding{Provider: "claude", Model: "configured-model"}, Profile: workflow.Profile{Allowed: []string{"read", "handoff"}}}}
	args, _, err := claudeArgs(launch, nil, []byte(`{"type":"object"}`))
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(args, "--safe-mode") || !slices.Contains(args, "--settings=/session/settings.json") {
		t.Fatal("native hooks disabled or unregistered", args)
	}
}

func TestReadOnlyRootMustIncludeNonwritableLeaves(t *testing.T) {
	root := t.TempDir()
	leaf := filepath.Join(root, "existing")
	if err := os.WriteFile(leaf, []byte("mutable"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(root, 0700)
	if !ReadRootWritable(root) {
		t.Fatal("nonwritable root directory hid writable existing file")
	}
	if err := os.Chmod(leaf, 0444); err != nil {
		t.Fatal(err)
	}
	if ReadRootWritable(root) {
		t.Fatal("fully nonwritable root reported writable")
	}
}
