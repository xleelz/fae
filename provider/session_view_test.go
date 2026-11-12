package provider

import (
	"encoding/json"
	"github.com/xleelz/fae/commandshim"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/xleelz/fae/workflow"
)

func TestPrivateSessionViewsNeverMutateSharedFiles(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a", "b"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0640); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	for _, denied := range []string{"a", "b"} {
		wg.Add(1)
		go func(denied string) {
			defer wg.Done()
			g := ReadGuard{Worktree: root, Policy: &workflow.ReadPolicy{Deny: []string{denied}}}
			dest := filepath.Join(t.TempDir(), "view")
			if err := copySessionTree(root, dest, g.AllowPath); err != nil {
				t.Error(err)
				return
			}
			if _, err := os.Stat(filepath.Join(dest, denied)); !os.IsNotExist(err) {
				t.Error("denied file copied")
			}
			other := "a"
			if denied == other {
				other = "b"
			}
			if _, err := os.Stat(filepath.Join(dest, other)); err != nil {
				t.Error(err)
			}
		}(denied)
	}
	wg.Wait()
	for _, name := range []string{"a", "b"} {
		info, err := os.Stat(filepath.Join(root, name))
		if err != nil || info.Mode().Perm() != 0640 {
			t.Fatal("shared file changed", name, err)
		}
	}
	g := ReadGuard{Worktree: root, Policy: &workflow.ReadPolicy{Deny: []string{"a"}}}
	restore, err := g.MaskWorktree()
	if err == nil {
		t.Fatal("legacy shared mask did not fail closed")
	}
	defer restore()
	if _, err = os.Stat(filepath.Join(root, "a")); err != nil {
		t.Fatal("shared mask removed file", err)
	}
	cmd := exec.Command("true")
	cmd.Dir = root
	if _, err = g.ConfigureSessionView(cmd, t.TempDir(), nil); err != nil {
		if _, statErr := os.Stat(filepath.Join(root, "a")); statErr != nil {
			t.Fatal(statErr)
		}
		t.Log("namespace capability unavailable; launch refused before mutation:", err)
	}
}

func TestPrivateNamespaceRefusesHeavyShimUntilHostOwnershipSupported(t *testing.T) {
	root := t.TempDir()
	shims := filepath.Join(t.TempDir(), "command-shims")
	if err := os.Mkdir(shims, 0700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(commandshim.RoleShims{Build: true, Test: true, Worktree: root, Ledger: filepath.Join(t.TempDir(), "ledger")})
	if err := os.WriteFile(filepath.Join(shims, ".policy.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("true")
	cmd.Dir = root
	cmd.Env = []string{"PATH=" + shims + ":/usr/bin:/bin"}
	guard := ReadGuard{Worktree: root, ForceIsolation: true}
	if _, err := guard.ConfigureSessionView(cmd, t.TempDir(), nil); err == nil || !strings.Contains(err.Error(), "heavy-operation host ownership") {
		t.Fatal("unsupported namespace ownership not refused", err)
	}
}

func TestPrivateViewCollectChecksWriteScopeBeforeTouchingSource(t *testing.T) {
	root, private := t.TempDir(), t.TempDir()
	for _, dir := range []string{root, private} {
		if err := os.WriteFile(filepath.Join(dir, "allowed"), []byte("before"), 0640); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "outside"), []byte("retained"), 0640); err != nil {
			t.Fatal(err)
		}
	}
	before, err := SnapshotTaskDirectory(private)
	if err != nil {
		t.Fatal(err)
	}
	guard := &ReadGuard{Worktree: root, CanWrite: func(path string) bool { return path == "allowed" }}
	view := &SessionView{guard: guard, source: root, private: private, before: before}
	if err = os.WriteFile(filepath.Join(private, "outside"), []byte("forbidden"), 0640); err != nil {
		t.Fatal(err)
	}
	if err = view.Collect(); err == nil {
		t.Fatal("outside write accepted")
	}
	if raw, err := os.ReadFile(filepath.Join(root, "outside")); err != nil || string(raw) != "retained" {
		t.Fatal("outside source touched", err)
	}
	if err = os.WriteFile(filepath.Join(private, "outside"), []byte("retained"), 0640); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(private, "allowed"), []byte("checkpoint1"), 0640); err != nil {
		t.Fatal(err)
	}
	if err = view.Collect(); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(private, "allowed"), []byte("checkpoint2"), 0640); err != nil {
		t.Fatal(err)
	}
	if err = view.Collect(); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(filepath.Join(root, "allowed")); err != nil || string(raw) != "checkpoint2" {
		t.Fatal("incremental checkpoint lost", err)
	}
}

func TestPrivateNamespacePhysicallyDeniesOutsideAndReadRootWrites(t *testing.T) {
	root, readRoot, logs := t.TempDir(), t.TempDir(), t.TempDir()
	ancestor := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	for path, data := range map[string]string{filepath.Join(root, "allowed"): "visible", filepath.Join(root, "denied"): "secret", filepath.Join(readRoot, "rootfile"): "read-only", outside: "outside", filepath.Join(ancestor, "denied"): "ancestor-secret", filepath.Join(ancestor, "public"): "ancestor-visible"} {
		if err := os.WriteFile(path, []byte(data), 0640); err != nil {
			t.Fatal(err)
		}
	}
	initGitWorktree(t, root)
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	script := "test ! -e " + quote(filepath.Join(ancestor, "denied")) + " || exit 27; cat " + quote(filepath.Join(ancestor, "public")) + "; pwd; git rev-parse --show-toplevel; git show HEAD:allowed; if git show HEAD:denied; then exit 25; fi; cat allowed; test ! -e denied || exit 21; test ! -e " + quote(outside) + " || exit 22; cat " + quote(filepath.Join(readRoot, "rootfile")) + "; if printf BAD > " + quote(filepath.Join(readRoot, "rootfile")) + "; then exit 23; fi; test ! -e /home/unrelated || exit 24"
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Dir = root
	cmd.Env = os.Environ()
	guard := ReadGuard{Worktree: root, ForceIsolation: true, Policy: &workflow.ReadPolicy{Deny: []string{"denied"}}, ReadRoots: map[string]ReadRootView{"external": {Path: readRoot}, "ancestor-output-0": {Path: ancestor}}}
	if _, err := guard.ConfigureSessionView(cmd, logs, nil); err != nil {
		if strings.Contains(err.Error(), "session filesystem isolation unavailable") {
			t.Skip(err)
		}
		t.Fatal(err)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatal("namespace confinement failed", err, string(out))
	} else if !strings.Contains(string(out), root) || !strings.Contains(string(out), "visible") || !strings.Contains(string(out), "read-only") {
		t.Fatal(string(out))
	}
	if raw, err := os.ReadFile(filepath.Join(root, "denied")); err != nil || string(raw) != "secret" {
		t.Fatal("shared denied content changed", err)
	}
	if raw, err := os.ReadFile(filepath.Join(readRoot, "rootfile")); err != nil || string(raw) != "read-only" {
		t.Fatal("shared read root changed", err)
	}
}

func TestConcurrentPrivateDirectoryWritesPreserveSiblingOutputs(t *testing.T) {
	shared := t.TempDir()
	makeView := func(name string) *SessionView {
		private := t.TempDir()
		before, err := SnapshotTaskDirectory(private)
		if err != nil {
			t.Fatal(err)
		}
		return &SessionView{source: shared, private: private, before: before, guard: &ReadGuard{Worktree: shared, CanWrite: func(path string) bool { return path == name }}}
	}
	a, b := makeView("a.yaml"), makeView("b.yaml")
	if err := os.WriteFile(filepath.Join(a.private, "a.yaml"), []byte("a"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b.private, "b.yaml"), []byte("b"), 0600); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for _, view := range []*SessionView{a, b} {
		wg.Add(1)
		go func(view *SessionView) {
			defer wg.Done()
			if err := view.Collect(); err != nil {
				t.Error(err)
			}
		}(view)
	}
	wg.Wait()
	for _, name := range []string{"a.yaml", "b.yaml"} {
		if _, err := os.Stat(filepath.Join(shared, name)); err != nil {
			t.Fatal("sibling output lost", name, err)
		}
	}
	if err := os.WriteFile(filepath.Join(a.private, "b.yaml"), []byte("rogue"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.Collect(); err == nil {
		t.Fatal("sibling write accepted")
	}
	if raw, err := os.ReadFile(filepath.Join(shared, "b.yaml")); err != nil || string(raw) != "b" {
		t.Fatal("sibling's committed output changed", err)
	}
}

func TestSessionStateAndExplicitCredentialMountsPersistUpdates(t *testing.T) {
	logs, private, credentials := t.TempDir(), t.TempDir(), t.TempDir()
	for _, name := range []string{"home", "tmp"} {
		if err := os.Mkdir(filepath.Join(logs, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(credentials, ".credentials.json"), []byte("synthetic-token"), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("true")
	cmd.Env = []string{"HOME=" + filepath.Join(logs, "home"), "TMPDIR=" + filepath.Join(logs, "tmp"), "CLAUDE_CONFIG_DIR=" + credentials}
	mounted := map[string]string{}
	err := bindSessionState(cmd, logs, private, func(source, target string, ro bool) {
		if ro {
			t.Error("state mounted read-only")
		}
		mounted[target] = source
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"home", "tmp"} {
		p := filepath.Join(logs, name)
		if mounted[p] != p {
			t.Errorf("state mount absent: %s", p)
		}
	}
	copy := mounted[credentials]
	if copy != credentials {
		t.Fatal("explicit configuration directory was not mounted at its configured path")
	}
	if err = os.WriteFile(filepath.Join(copy, ".credentials.json"), []byte("updated"), 0600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(credentials, ".credentials.json"))
	if err != nil || string(data) != "updated" {
		t.Fatal("configured credential update did not persist", err)
	}
}

func TestSessionCollectionIndependentReadWriteScopes(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(map[bool]string{false: "new-output", true: "blind-existing-conflict"}[conflict], func(t *testing.T) {
			source, private := t.TempDir(), t.TempDir()
			g := &ReadGuard{Worktree: source, Policy: &workflow.ReadPolicy{Allow: []string{"inputs/**"}}, CanWrite: func(name string) bool { return strings.HasPrefix(name, "gen/") }}
			before, err := SnapshotTaskDirectory(private)
			if err != nil {
				t.Fatal(err)
			}
			view := &SessionView{guard: g, source: source, private: private, before: before}
			for _, root := range []string{source, private} {
				if err = os.Mkdir(filepath.Join(root, "gen"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if conflict {
				if err = os.WriteFile(filepath.Join(source, "gen/new.yaml"), []byte("existing-hidden"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err = os.WriteFile(filepath.Join(private, "gen/new.yaml"), []byte("generated"), 0600); err != nil {
				t.Fatal(err)
			}
			err = view.Collect()
			if conflict {
				if err == nil || !strings.Contains(err.Error(), "created concurrently") {
					t.Fatal("blind overwrite not refused", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(source, "gen/new.yaml"))
			if err != nil || string(data) != "generated" {
				t.Fatal("output missing", err)
			}
		})
	}
}
