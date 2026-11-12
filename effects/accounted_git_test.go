package effects

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// FAE-MODE-002, FAE-VCS-002, FAE-VCS-010 through the broker: in account mode
// a brokered command's nested git, run in the task worktree, cannot move the
// operator's branches, change its remotes or configuration, or remove another
// task's worktree; a configuration injected into a read is refused, and a
// plain read runs. The test's own repository under TMPDIR is committed,
// branched and configured as called. Each call passes the real wrapper, the
// fae broker's heavy-op run (this test binary) and the nested-command policy,
// which asks the real git where the call's repository is. A Local task's
// remote change exits 78, a Normal task's 77. The operator's repository is
// unchanged.
func TestBrokerAccountedGitCannotChangeTheOperatorsRepository(t *testing.T) {
	b, w, repo := setup(t)
	other, err := b.Manager.Ensure(context.Background(), "run", "other-task", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	state := func() string {
		config, err := os.ReadFile(filepath.Join(repo, ".git", "config"))
		if err != nil {
			t.Fatal(err)
		}
		return gitTest(t, repo, "for-each-ref") + string(config) + gitTest(t, repo, "worktree", "list", "--porcelain")
	}
	before := state()
	script := `git update-ref refs/heads/master HEAD; echo "update-ref=$?"
git -C "$OP" branch -f fae/integration HEAD; echo "branch=$?"
git config remote.origin.url /elsewhere; echo "remote=$?"
git config core.hooksPath "$TMPDIR"; echo "hooks=$?"
git worktree remove --force "$OTHER"; echo "worktree=$?"
git -c alias.p=push p; echo "alias=$?"
git -c core.fsmonitor=/bin/false status; echo "inject=$?"
git status --short; echo "status=$?"
cd "$TMPDIR" && git init -q own && cd own && git -c user.name=x -c user.email=y commit -q --allow-empty -m own && git branch side && git config remote.origin.url /x && echo "own=$?"`
	b.Allowed = [][]string{{"/bin/sh", "-c", script}}
	accountMode(t, b)
	for _, c := range []struct {
		local  bool
		remote string
	}{{true, "remote=78"}, {false, "remote=77"}} {
		r, err := b.Run(nestedRunContext(c.local), w, Command{Argv: b.Allowed[0], Timeout: time.Minute, Environment: []string{"OP=" + repo, "OTHER=" + other.Path}})
		if err != nil {
			t.Fatal(err)
		}
		want := "update-ref=77\nbranch=77\n" + c.remote + "\nhooks=77\nworktree=77\nalias=77\ninject=77\nstatus=0\nown=0\n"
		if r.Output != want {
			t.Errorf("local=%t: output %q, want %q (stderr %s)", c.local, r.Output, want, r.Stderr)
		}
		for _, reason := range []string{"may only read it", "manages worktrees", `nested git "p" is not a git command`, `nested git -c "core.fsmonitor"`} {
			if !strings.Contains(r.Stderr, reason) {
				t.Errorf("local=%t: stderr lacks %q: %s", c.local, reason, r.Stderr)
			}
		}
		if r.NestedCount != 12 {
			t.Errorf("local=%t: %d nested calls recorded, want 12", c.local, r.NestedCount)
		}
	}
	if after := state(); after != before {
		t.Errorf("the operator's repository changed:\n%s\nwas\n%s", after, before)
	}
	if _, err := os.Stat(other.Path); err != nil {
		t.Errorf("the other task's worktree is gone: %v", err)
	}
}

// FAE-MODE-002, FAE-EXT-001, FAE-GEN-010 through the broker in account mode,
// for a Local task: a nested git call that drops the broker's
// GIT_CONFIG_GLOBAL and GIT_CONFIG_NOSYSTEM and points HOME at a
// configuration naming a program (a core.fsmonitor hook) reads the task
// worktree without running it; a -c setting naming a program is refused on
// the test's own repository as well; a repository off this host is refused,
// by the policy when the call names its URL (77) and by git itself when a
// remote's configuration names it; a local fetch runs. 127.0.0.1:1 refuses
// connections, so nothing leaves the host.
func TestBrokerAccountedGitRunsNoProgramTheCallChose(t *testing.T) {
	b, w, repo := setup(t)
	evil := t.TempDir()
	write(t, evil, "monitor", "#!/bin/sh\n: >'"+filepath.Join(evil, "ran")+"'\nexit 1\n")
	if err := os.Chmod(filepath.Join(evil, "monitor"), 0700); err != nil {
		t.Fatal(err)
	}
	write(t, evil, ".gitconfig", "[core]\n\tfsmonitor = "+filepath.Join(evil, "monitor")+"\n")
	script := `env -u GIT_CONFIG_GLOBAL -u GIT_CONFIG_NOSYSTEM HOME="$EVIL" git status --short >/dev/null; echo "home=$?"
test -e "$EVIL/ran"; echo "ran=$?"
cd "$TMPDIR" && git init -q own && cd own || exit 9
git -c core.sshCommand=/bin/false fetch -q "$OP"; echo "ssh=$?"
git fetch -q https://127.0.0.1:1/x.git; echo "url=$?"
git config remote.web.url https://127.0.0.1:1/x.git
git fetch -q web 2>"$TMPDIR/web.err"; echo "web=$?"; grep -c "transport 'https' not allowed" "$TMPDIR/web.err"
git fetch -q "$OP" HEAD; echo "local=$?"`
	b.Allowed = [][]string{{"/bin/sh", "-c", script}}
	accountMode(t, b)
	r, err := b.Run(nestedRunContext(true), w, Command{Argv: b.Allowed[0], Timeout: time.Minute, Environment: []string{"OP=" + repo, "EVIL=" + evil, "HOME=" + t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	if want := "home=0\nran=1\nssh=77\nurl=77\nweb=128\n1\nlocal=0\n"; r.Output != want {
		t.Errorf("output %q, want %q (stderr %s)", r.Output, want, r.Stderr)
	}
	for _, reason := range []string{`nested git -c "core.sshCommand" names a program for git to run`, "a repository off this host"} {
		if !strings.Contains(r.Stderr, reason) {
			t.Errorf("stderr lacks %q: %s", reason, r.Stderr)
		}
	}
	if r.NestedCount != 7 {
		t.Errorf("%d nested calls recorded, want 7", r.NestedCount)
	}
}

// FAE-MODE-002 on the default path: in refuse mode a Local task's nested git
// push or remote change exits 78, found past git's global options, while
// every other nested git call exits 77, as every nested call of a Normal
// task does. A run whose context names no task is treated as Local.
func TestBrokerRefuseModeGivesLocalPushItsCode(t *testing.T) {
	b, w, _ := setup(t)
	script := `git push origin HEAD; echo "push=$?"
git -C . remote -v; echo "remote=$?"
git status; echo "status=$?"`
	b.Allowed = [][]string{{"/bin/sh", "-c", script}}
	for _, c := range []struct {
		ctx  context.Context
		want string
	}{
		{nestedRunContext(true), "push=78\nremote=78\nstatus=77\n"},
		{context.Background(), "push=78\nremote=78\nstatus=77\n"},
		{nestedRunContext(false), "push=77\nremote=77\nstatus=77\n"},
	} {
		r, err := b.Run(c.ctx, w, Command{Argv: b.Allowed[0], Timeout: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		if r.Output != c.want || r.NestedCount != 0 {
			t.Errorf("output %q, want %q (stderr %s)", r.Output, c.want, r.Stderr)
		}
		if strings.Contains(c.want, "78") != strings.Contains(r.Stderr, "fae: command refused in Local mode (exit 78): git push") {
			t.Errorf("stderr %s", r.Stderr)
		}
	}
}
