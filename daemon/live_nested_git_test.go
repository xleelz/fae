package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FAE-MODE-002, FAE-VCS-010 on the live daemon path, in the self-hosting
// shape: in account mode a Local task's engine run, through the fae binary's
// nested-command policy, cannot change the workspace from its task worktree.
// The workspace's own test (run by every go test of the task) creates a
// branch, plants an integration tip and points a remote: each is refused, 77
// and, for the remote, 78; git status still reads the worktree, and the test's
// own repository under TMPDIR is committed and branched as called. The task
// reaches Done, and the workspace has no planted branch and no remote. On a
// policy that runs these calls the nested test fails, so the task never does.
func TestLiveAccountModeNestedGitCannotChangeTheWorkspace(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	nested := `package sample

import (
	"os/exec"
	"path/filepath"
	"testing"
)

func TestNestedGitPolicy(t *testing.T) {
	for _, c := range []struct {
		args []string
		code int
	}{
		{[]string{"branch", "planted", "HEAD"}, 77},
		{[]string{"update-ref", "refs/heads/fae/integration", "HEAD"}, 77},
		{[]string{"config", "remote.origin.url", "/elsewhere"}, 78},
		{[]string{"status", "--porcelain"}, 0},
	} {
		cmd := exec.Command("git", c.args...)
		out, _ := cmd.CombinedOutput()
		if cmd.ProcessState.ExitCode() != c.code {
			t.Fatalf("nested git %v: exit %d, want %d: %s", c.args, cmd.ProcessState.ExitCode(), c.code, out)
		}
	}
	own := filepath.Join(t.TempDir(), "own")
	for _, args := range [][]string{{"init", "-q", own}, {"-C", own, "-c", "user.name=x", "-c", "user.email=y", "commit", "-q", "--allow-empty", "-m", "own"}, {"-C", own, "branch", "side"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("nested git %v in the test's own repository: %v %s", args, err, out)
		}
	}
}
`
	if err := os.WriteFile(filepath.Join(cfg.Workspace, "nested_git_test.go"), []byte(nested), 0600); err != nil {
		t.Fatal(err)
	}
	liveGit(t, cfg.Workspace, "add", "nested_git_test.go")
	liveGit(t, cfg.Workspace, "commit", "-m", "a test whose nested git reaches for the workspace")
	cfg.Live.Base = liveGit(t, cfg.Workspace, "rev-parse", "HEAD")
	liveGit(t, cfg.Workspace, "branch", "-f", "fae/integration", cfg.Live.Base)
	useBuiltFaeBroker(t)
	cfg.Live.NestedCommands, cfg.Live.MaxOutputBytes = "account", 8<<20
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	if _, err = call(t, d, "Admit", AdmitParams{ID: "nested-git", AuthorizingMessage: "message"}, "nested-git"); err != nil {
		t.Fatal(err)
	}
	awaitState(t, d, "nested-git", "Done")
	if planted := liveGit(t, cfg.Workspace, "branch", "--list", "planted"); planted != "" {
		t.Fatalf("a nested git call created a workspace branch: %q", planted)
	}
	config, err := os.ReadFile(filepath.Join(cfg.Workspace, ".git", "config"))
	if err != nil || strings.Contains(string(config), "elsewhere") || strings.Contains(string(config), "[remote") {
		t.Fatalf("a nested git call changed the workspace's remotes: %v\n%s", err, config)
	}
	markers, err := d.store.MarkersWithPrefix(ctx, "nested-commands/", 1000)
	recorded := false
	for _, raw := range markers {
		recorded = recorded || strings.Contains(string(raw), `"planted"`)
	}
	if err != nil || !recorded {
		t.Fatalf("no nested-commands record names the refused call: %v", err)
	}
}
