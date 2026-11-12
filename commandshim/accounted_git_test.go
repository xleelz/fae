package commandshim

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// accountedGitRun is an account-mode run on a real repository, laid out as
// the daemon lays it out: the operator's repository, the run's task worktree
// and another task's, both linked worktrees of it, and the run's scratch
// directory holding a test's own repository.
type accountedGitRun struct {
	git, op, worktree, other, scratch, repo string
	env                                     []string
}

func newAccountedGitRun(t *testing.T) accountedGitRun {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	r := accountedGitRun{git: git, op: filepath.Join(root, "op"), worktree: filepath.Join(root, "trees", "a"), other: filepath.Join(root, "trees", "b"), scratch: filepath.Join(root, "scratch")}
	r.repo = filepath.Join(r.scratch, "repo")
	// The broker's environment: no user or system git configuration.
	r.env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@localhost", "GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@localhost"}
	for _, args := range [][]string{
		{"init", "-q", "-b", "main", r.op},
		{"-C", r.op, "config", "user.name", "Operator"},
		// Not git remote add: account mode refuses git remote everywhere, and
		// this fixture is set up under account mode when Fae develops itself.
		{"-C", r.op, "config", "remote.origin.url", "https://example.invalid/op.git"},
		{"-C", r.op, "commit", "-q", "--allow-empty", "-m", "base"},
		{"-C", r.op, "worktree", "add", "-q", "-b", "fae/task/a", r.worktree},
		{"-C", r.op, "worktree", "add", "-q", "-b", "fae/task/b", r.other},
		{"init", "-q", "-b", "main", r.repo},
		{"-C", r.repo, "commit", "-q", "--allow-empty", "-m", "fixture"},
	} {
		r.run(t, root, nil, args...)
	}
	if err = os.Mkdir(filepath.Join(r.scratch, "plain"), 0700); err != nil {
		t.Fatal(err)
	}
	return r
}

// run runs the real git, failing the test unless it succeeds.
func (r accountedGitRun) run(t *testing.T, dir string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command(r.git, args...)
	cmd.Dir, cmd.Env = dir, append(append([]string(nil), r.env...), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %q in %s: %v %s", args, dir, err, out)
	}
	return string(out)
}

// state is everything of the operator's repository a nested call must not
// change: its refs, configuration, hooks and registered worktrees.
func (r accountedGitRun) state(t *testing.T) string {
	t.Helper()
	config, err := os.ReadFile(filepath.Join(r.op, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	hooks, err := os.ReadDir(filepath.Join(r.op, ".git", "hooks"))
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, h := range hooks {
		names = append(names, h.Name())
	}
	reflog, err := os.ReadFile(filepath.Join(r.op, ".git", "logs", "HEAD"))
	if err != nil {
		t.Fatal(err)
	}
	return r.run(t, r.op, nil, "for-each-ref") + string(config) + strings.Join(names, ",") + r.run(t, r.op, nil, "worktree", "list", "--porcelain") + string(reflog)
}

// testRepo creates a test's own repository, with one commit, in the run's
// scratch directory.
func (r accountedGitRun) testRepo(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(r.scratch, name)
	r.run(t, r.scratch, nil, "init", "-q", "-b", "main", dir)
	r.run(t, dir, nil, "commit", "-q", "--allow-empty", "-m", name)
	return dir
}

// linkInto replaces the entry of repo's .git directory with a symbolic link
// to the operator's, as a hostile test can; the repository borrows the
// operator's objects, so the operator's commits resolve in it.
func (r accountedGitRun) linkInto(t *testing.T, repo, entry string) {
	t.Helper()
	at := filepath.Join(repo, ".git", entry)
	if err := os.RemoveAll(at); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(r.op, ".git", entry), at); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(repo, ".git", "objects", "info", "alternates"), filepath.Join(r.op, ".git", "objects")+"\n")
}

// FAE-MODE-002, FAE-EXT-001, FAE-VCS-002, FAE-VCS-010: in account mode a
// nested git call may only read the run's own repository (the operator's,
// whose refs, configuration and hooks its task worktrees share) and makes no
// external effect anywhere, while a test's own repository in the run's scratch
// directory is changed as called. Every case asks the real git where the
// call's repository is. A Local task's remote change exits 78, as its push
// does; every other refusal is 77. The operator's repository is unchanged
// afterwards, and every refusal in accounted_git.go fires.
func TestAccountedGitProtectsTheRunsRepository(t *testing.T) {
	r := newAccountedGitRun(t)
	// The fixture stands in a temporary directory whatever TMPDIR is (Fae's
	// own account-mode runs set it to their scratch directory).
	saved := accountedGitTemporaryRoots
	accountedGitTemporaryRoots = []string{filepath.Dir(r.scratch)}
	defer func() { accountedGitTemporaryRoots = saved }()
	before := r.state(t)
	tmpl := filepath.Join(r.scratch, "template")
	if err := os.MkdirAll(filepath.Join(tmpl, "hooks"), 0700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(tmpl, "hooks", "pre-commit"), "#!/bin/sh\nexit 0\n")
	if err := os.Chmod(filepath.Join(tmpl, "hooks", "pre-commit"), 0700); err != nil {
		t.Fatal(err)
	}
	// Configuration files a call names, and test repositories linked into
	// the operator's: by a symbolic link and a reference store.
	named, broken := filepath.Join(r.scratch, "named.cfg"), filepath.Join(r.scratch, "broken.cfg")
	write(t, named, "[core]\n\tfsmonitor = "+tmpl+"/hooks/pre-commit\n")
	write(t, broken, "[core\n")
	linked, stored := r.testRepo(t, "linked"), r.testRepo(t, "stored")
	r.linkInto(t, linked, "refs")
	r.run(t, stored, nil, "config", "core.repositoryFormatVersion", "1")
	r.run(t, stored, nil, "config", "extensions.refStorage", "files://"+filepath.Join(r.op, ".git"))
	dangling := filepath.Join(r.scratch, "dangling")
	if err := os.Symlink(filepath.Join(r.scratch, "nowhere", "file"), dangling); err != nil {
		t.Fatal(err)
	}
	prepare := func(local bool, dir string, env []string, args ...string) ([]string, error) {
		p := Policy{Worktree: r.worktree, Scratch: r.scratch, CWD: dir, Local: local}
		return p.PrepareAccounted(append([]string{r.git}, args...), append(append([]string(nil), r.env...), env...))
	}
	seen := []string{}
	for _, c := range []struct {
		local  bool
		dir    string
		env    []string
		args   []string
		code   int
		reason string
	}{
		// The operator's repository, from the task worktree or by path: reads only.
		{true, r.worktree, nil, []string{"config", "remote.origin.url", "/elsewhere"}, LocalModeRefused, "Local mode"},
		{true, r.worktree, nil, []string{"config", "--add", "remote.x.url", "y"}, LocalModeRefused, "Local mode"},
		{true, r.worktree, nil, []string{"config", "set", "url.https://evil/.insteadOf", "https://example.invalid/"}, LocalModeRefused, "Local mode"},
		{true, r.scratch, nil, []string{"config", "--file", filepath.Join(r.op, ".git", "config"), "remote.origin.pushurl", "x"}, LocalModeRefused, "Local mode"},
		{false, r.worktree, nil, []string{"config", "remote.origin.url", "/elsewhere"}, Refused, "may only read it"},
		{true, r.worktree, nil, []string{"config", "alias.p", "push"}, Refused, "writes configuration"},
		{true, r.worktree, nil, []string{"config", "core.hooksPath", tmpl}, Refused, "may only read it"},
		{true, r.scratch, nil, []string{"config", "--system", "alias.p", "push"}, Refused, "nested git config would write /etc/gitconfig"},
		{true, r.worktree, nil, []string{"update-ref", "refs/heads/main", "HEAD"}, Refused, "nested git update-ref is not a read-only command"},
		{true, r.scratch, nil, []string{"-C", r.op, "branch", "-f", "main", "HEAD"}, Refused, "changes references"},
		{true, r.worktree, nil, []string{"branch", "-f", "main", "HEAD"}, Refused, "changes references"},
		{true, r.worktree, nil, []string{"branch", "--del", "fae/task/b"}, Refused, "changes references"},
		{true, r.worktree, nil, []string{"branch", "new"}, Refused, "creates a reference"},
		{true, r.worktree, nil, []string{"tag", "v1"}, Refused, "creates a reference"},
		{true, r.worktree, nil, []string{"symbolic-ref", "HEAD", "refs/heads/main"}, Refused, "changes a reference"},
		{true, r.worktree, nil, []string{"worktree", "remove", "--force", r.other}, Refused, "manages worktrees"},
		{true, r.worktree, nil, []string{"worktree", "prune"}, Refused, "manages worktrees"},
		{true, r.worktree, nil, []string{"gc", "--prune=now"}, Refused, "is not a read-only command"},
		{true, r.worktree, nil, []string{"commit", "--allow-empty", "-m", "x"}, Refused, "is not a read-only command"},
		{true, r.worktree, nil, []string{"fetch", r.repo, "main:main"}, Refused, "is not a read-only command"},
		{true, r.worktree, nil, []string{"diff", "--output=out.txt"}, Refused, "writes a file (--output)"},
		{true, r.worktree, nil, []string{"log", "--outp=out.txt"}, Refused, "writes a file (--output)"},
		{true, r.worktree, nil, []string{"init"}, Refused, "would reinitialize the repository at " + filepath.Join(r.op, ".git")},
		{true, r.scratch, nil, []string{"init", r.op}, Refused, "would reinitialize the repository"},
		{true, r.scratch, nil, []string{"init", "--template=" + tmpl, filepath.Join(r.scratch, "t1")}, Refused, "nested git init --template copies the hooks and the configuration, which can name programs, of a directory the call names"},
		{true, r.scratch, nil, []string{"--git-dir=" + filepath.Join(r.repo, ".git"), "--work-tree=" + r.other, "checkout", "-f", "HEAD"}, Refused, "may only read it"},
		{true, r.scratch, []string{"GIT_DIR=" + filepath.Join(r.op, ".git")}, []string{"update-ref", "refs/heads/main", "HEAD"}, Refused, "may only read it"},
		// No configuration or program injected into a read.
		{true, r.worktree, nil, []string{"-c", "core.fsmonitor=" + tmpl + "/hooks/pre-commit", "status"}, Refused, `nested git -c "core.fsmonitor"`},
		{true, r.worktree, nil, []string{"--config-env=diff.external=HOME", "diff"}, Refused, `nested git -c "diff.external"`},
		{true, r.worktree, []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.pager", "GIT_CONFIG_VALUE_0=sh"}, []string{"log"}, Refused, "with GIT_CONFIG_KEY_0 set"},
		{true, r.worktree, []string{"GIT_EXTERNAL_DIFF=/bin/false"}, []string{"diff"}, Refused, "with GIT_EXTERNAL_DIFF set"},
		{true, r.worktree, nil, []string{"-c", "foo", "status"}, Refused, "cannot tell which repository the call uses"},
		{true, r.worktree, nil, []string{"-c", "diff.renames=copies", "diff"}, Refused, `nested git -c "diff.renames" on the repository at ` + filepath.Join(r.op, ".git")},
		{true, r.worktree, []string{"GIT_CONFIG_GLOBAL=" + filepath.Join(r.scratch, "global.cfg")}, []string{"status"}, Refused, "with GIT_CONFIG_GLOBAL set on the repository at"},
		// What a call itself gives git that names a program, a file of
		// configuration or bundles to download is refused on any
		// repository, a test's own included (FAE-MODE-002, FAE-EXT-001).
		{true, r.repo, nil, []string{"-c", "core.sshCommand=" + tmpl + "/hooks/pre-commit", "fetch", r.op}, Refused, `nested git -c "core.sshCommand" names a program for git to run; nested calls choose none, on any repository`},
		{true, r.repo, nil, []string{"-c", "core.hooksPath=" + tmpl + "/hooks", "commit", "--allow-empty", "-m", "x"}, Refused, `nested git -c "core.hooksPath" names a program`},
		{true, r.repo, nil, []string{"-c", "remote.origin.uploadpack=" + tmpl + "/hooks/pre-commit", "fetch", "origin"}, Refused, `nested git -c "remote.origin.uploadpack" names a program`},
		{true, r.repo, nil, []string{"-c", "filter.x.clean=cat", "add", "."}, Refused, `nested git -c "filter.x.clean" names a program`},
		{true, r.repo, nil, []string{"-c", "fetch.bundleURI=https://127.0.0.1:1/b.bundle", "fetch", r.op}, Refused, `nested git -c "fetch.bundleURI" has git download bundles over HTTP`},
		{true, r.repo, nil, []string{"-c", "include.path=" + filepath.Join(r.scratch, "x.cfg"), "status"}, Refused, `nested git -c "include.path" names a file of configuration for git to load`},
		{true, r.repo, nil, []string{"clone", "-q", "--config", "core.hooksPath=" + tmpl + "/hooks", r.repo, filepath.Join(r.scratch, "c4")}, Refused, `nested git clone --config "core.hooksPath" names a program`},
		{true, r.repo, nil, []string{"clone", "-q", "--template=" + tmpl, r.repo, filepath.Join(r.scratch, "c5")}, Refused, "--template copies the hooks and the configuration"},
		{true, r.repo, nil, []string{"ls-remote", "--exec=/bin/false", r.op}, Refused, "--exec runs a program the call names"},
		{true, r.repo, nil, []string{"merge", "-sevil", "HEAD"}, Refused, `nested git merge --strategy "evil" runs git-merge-evil from PATH`},
		{true, r.repo, nil, []string{"rebase", "--strategy", "evil", "HEAD"}, Refused, `nested git rebase --strategy "evil" runs git-merge-evil`},
		{true, r.repo, []string{"GIT_CONFIG_PARAMETERS='core.editor'='vi'"}, []string{"status"}, Refused, "with GIT_CONFIG_PARAMETERS set"},
		{true, r.repo, []string{"GIT_CONFIG_COUNT=many"}, []string{"status"}, Refused, `with GIT_CONFIG_COUNT set to "many"`},
		{true, r.repo, []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=credential.helper", "GIT_CONFIG_VALUE_0=!sh"}, []string{"status"}, Refused, "with GIT_CONFIG_KEY_0 set names a program"},
		{true, r.repo, []string{"GIT_SSH_COMMAND=sh"}, []string{"fetch", r.op}, Refused, "with GIT_SSH_COMMAND set names a program"},
		// A trace appended to a file outside the call's reach would change it.
		{true, r.repo, []string{"GIT_TRACE=" + filepath.Join(r.op, ".git", "config")}, []string{"status"}, Refused, "writes its trace to " + filepath.Join(r.op, ".git", "config")},
		{true, r.scratch, []string{"GIT_TRACE2_EVENT=af_unix:" + filepath.Join(r.scratch, "s")}, []string{"version"}, Refused, "traces to a socket"},
		// Only local repositories: paths and file:// URLs (FAE-GEN-010).
		{true, r.repo, nil, []string{"fetch", "https://127.0.0.1:1/x.git"}, Refused, `nested git fetch names "https://127.0.0.1:1/x.git", a repository off this host`},
		{true, r.scratch, nil, []string{"clone", "-q", "--depth", "1", "host.invalid:x.git", filepath.Join(r.scratch, "c6")}, Refused, "a repository off this host"},
		{true, r.repo, nil, []string{"pull", "-s", "ort", "ext::sh -c x", "main"}, Refused, "a repository off this host"},
		{true, r.repo, nil, []string{"ls-remote", "--sort", "refname", "fake::x"}, Refused, "a repository off this host"},
		{true, r.repo, nil, []string{"fetch", "--multiple", r.op, "ssh://h.invalid/x"}, Refused, "a repository off this host"},
		{true, r.repo, nil, []string{"archive", "--remote=git://h.invalid/x", "HEAD"}, Refused, "a repository off this host"},
		{true, r.repo, nil, []string{"submodule", "add", "-b", "main", "https://h.invalid/x", "sub"}, Refused, "a repository off this host"},
		{true, r.scratch, nil, []string{"clone", "--bundle-uri=https://127.0.0.1:1/b.bundle", r.repo, filepath.Join(r.scratch, "c7")}, Refused, "a repository off this host"},
		// A test's own repository may not reach into the operator's git directory.
		{true, r.repo, nil, []string{"bundle", "create", filepath.Join(r.op, ".git", "config"), "HEAD"}, Refused, "inside the run's own repository's git directory"},
		{true, r.repo, nil, []string{"worktree", "add", filepath.Join(r.op, ".git", "refs", "heads", "x")}, Refused, "inside the run's own repository's git directory"},
		{true, r.repo, nil, []string{"worktree", "add", "/proc/fae-nowhere"}, Refused, "would change the worktree at /proc/fae-nowhere"},
		{true, r.repo, nil, []string{"worktree", "remove", "--force", r.other}, Refused, "would change the worktree at " + r.other + ", outside the run's directories and the temporary ones, or one of the run's own repository"},
		// Files a call names for git to write lie where it may write; and a
		// patch directory its configuration names.
		{true, r.repo, nil, []string{"archive", "-o", "/proc/fae-nowhere/x.tar", "HEAD"}, Refused, "nested git archive would write /proc/fae-nowhere/x.tar"},
		{true, r.repo, nil, []string{"-c", "format.outputDirectory=" + filepath.Join(r.op, ".git", "pp"), "format-patch", "-1"}, Refused, "nested git format-patch would write its patches to " + filepath.Join(r.op, ".git", "pp") + " (format.outputDirectory)"},
		{true, r.repo, nil, []string{"apply", "--unsafe-paths", "x.patch"}, Refused, "nested git apply --unsafe-paths writes files wherever the patch names them"},
		// A path with a component the system cannot follow: open creates
		// the file a link leading nowhere names.
		{true, r.repo, nil, []string{"log", "-1", "--output=" + dangling}, Refused, "nested git: the path " + dangling + " cannot be followed at " + dangling},
		{true, r.worktree, nil, []string{"rev-list", "--output=" + filepath.Join(r.scratch, "x"), "HEAD"}, Refused, "writes a file (--output)"},
		// No GIT_* variable the policy does not know, and no configuration a
		// call names or writes that names a program.
		{true, r.worktree, []string{"GIT_TEST_FSMONITOR=" + tmpl + "/hooks/pre-commit"}, []string{"status"}, Refused, "nested git with GIT_TEST_FSMONITOR set: git may read it as a program to run"},
		{true, r.repo, []string{"GIT_REFERENCE_BACKEND=files://" + filepath.Join(r.op, ".git")}, []string{"update-ref", "refs/heads/x", "HEAD"}, Refused, "nested git with GIT_REFERENCE_BACKEND set"},
		{true, r.repo, []string{"GIT_CONFIG_GLOBAL=" + named}, []string{"status"}, Refused, `", whose "core.fsmonitor" names a program for git to run; nested calls choose none`},
		{true, r.repo, []string{"GIT_CONFIG_SYSTEM=" + broken, "GIT_CONFIG_NOSYSTEM=0"}, []string{"status"}, Refused, "which git cannot read as configuration"},
		{true, r.repo, []string{"GIT_CONFIG_GLOBAL=named.cfg"}, []string{"status"}, Refused, "a relative path git reads from whatever directory it is in"},
		{true, r.repo, nil, []string{"config", "core.hooksPath", tmpl + "/hooks"}, Refused, `nested git config "core.hooksPath" names a program for git to run; nested calls write no such setting`},
		{true, r.repo, nil, []string{"config", "set", "hook.x.command", "true"}, Refused, `nested git config "hook.x.command" names a program`},
		{true, r.scratch, nil, []string{"config", "--file", filepath.Join(r.scratch, "x.cfg"), "--add", "include.path", named}, Refused, `nested git config "include.path" names a file of configuration`},
		{true, r.repo, nil, []string{"config", "--rename-section", "x", "filter.x"}, Refused, "nested git config renames a section"},
		// A test repository linked into the operator's: its refs or its
		// reference store.
		{true, linked, nil, []string{"update-ref", "refs/heads/main", "HEAD"}, Refused, "links " + filepath.Join(linked, ".git", "refs") + " to " + filepath.Join(r.op, ".git", "refs") + ", outside the run's directories"},
		{true, stored, nil, []string{"update-ref", "refs/heads/main", "HEAD"}, Refused, "keeps its references at " + filepath.Join(r.op, ".git") + " (extensions.refStorage)"},
		{true, r.scratch, nil, []string{"init", "-q", "/proc/fae-nowhere"}, Refused, "would create or reinitialize a repository at /proc/fae-nowhere"},
		// External effects and programs git would start, everywhere.
		{true, r.repo, nil, []string{"push", r.op, "HEAD:main"}, LocalModeRefused, "Local mode"},
		{true, r.repo, nil, []string{"remote", "-v"}, LocalModeRefused, "Local mode"},
		{false, r.repo, nil, []string{"-C", ".", "push"}, Refused, "an engine run makes no external effect outside the daemon's effect grants"},
		{true, r.repo, nil, []string{"-c", "alias.p=push", "p", r.op}, Refused, `nested git "p" is not a git command`},
		{true, r.repo, nil, []string{"imap-send"}, Refused, "nested git imap-send sends mail"},
		{true, r.repo, nil, []string{"subtree", "push", "--prefix=x", r.op, "main"}, Refused, "nested git subtree pushes"},
		{true, r.repo, nil, []string{"submodule", "foreach", "git push"}, Refused, "foreach runs a command"},
		{true, r.repo, nil, []string{"rebase", "-x", "git push", "HEAD"}, Refused, "--exec runs a program"},
		{true, r.repo, nil, []string{"clone", "--upload-pack=/bin/false", r.worktree, filepath.Join(r.scratch, "c2")}, Refused, "--upload-pack runs a program"},
		{true, r.repo, nil, []string{"bisect", "run", "true"}, Refused, "run runs a command"},
		{true, r.repo, nil, []string{"--exec-path=/tmp", "status"}, Refused, "--exec-path=DIR"},
		{true, r.repo, nil, []string{"maintenance", "register"}, Refused, "schedules jobs on the host"},
		{true, r.repo, nil, []string{"--bogus", "status"}, Refused, `nested git option "--bogus"`},
	} {
		argv, err := prepare(c.local, c.dir, c.env, c.args...)
		var refused *Refusal
		var local *LocalRefusal
		reason := ""
		switch {
		case errors.As(err, &refused):
			reason = refused.Reason
			if c.code != Refused {
				t.Errorf("%q: refused with 77, want %d: %v", c.args, c.code, err)
			}
		case errors.As(err, &local):
			reason = local.Reason
			if c.code != LocalModeRefused {
				t.Errorf("%q: refused with 78, want %d: %v", c.args, c.code, err)
			}
		default:
			t.Errorf("%q in %s: not refused: %q %v", c.args, c.dir, argv, err)
			continue
		}
		if !strings.Contains(err.Error(), c.reason) || argv != nil {
			t.Errorf("%q: %v, want %q", c.args, err, c.reason)
		}
		seen = append(seen, reason)
	}

	// Two refusals are reached only when a repository's configuration
	// changes between the locate and the read (the documented check-then-exec
	// window), or git fails: through PrepareAccounted a broken configuration
	// fails the locate first. Each fires on the located repository once its
	// configuration is broken.
	g, err := Policy{Worktree: r.worktree, Scratch: r.scratch, CWD: r.repo, Local: true}.accountedGit(r.git, r.env)
	if err != nil {
		t.Fatal(err)
	}
	located, err := g.locate(r.repo, nil, r.env)
	if err != nil || located == nil {
		t.Fatal(located, err)
	}
	call, err := parseAccountedGit(r.repo, []string{"format-patch", "-1"}, r.env)
	if err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(r.repo, ".git", "config")
	intact, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	write(t, config, string(intact)+"[extensions]\n\trefStorage = files://x\n[broken\n")
	_, storeErr := g.refStorage(located)
	patchErr := g.checkFormatPatchDirectory(call, located)
	write(t, config, string(intact))
	for _, c := range []struct {
		err  error
		want string
	}{
		{storeErr, "nested git: cannot read extensions.refStorage of the repository at " + filepath.Join(r.repo, ".git") + " ("},
		{patchErr, "nested git format-patch: cannot read format.outputDirectory ("},
	} {
		var refused *Refusal
		if !errors.As(c.err, &refused) || !strings.Contains(c.err.Error(), c.want) || !strings.Contains(c.err.Error(), "fatal: bad config line") {
			t.Errorf("a configuration broken after the locate: %v, want %q with fatal: bad config line", c.err, c.want)
			continue
		}
		seen = append(seen, refused.Reason)
	}

	// Reads of the operator's repository run, with --no-optional-locks; a
	// test's own repository and a new one are changed as called. A call on
	// a repository runs pinned to it: -C its canonical working directory
	// and its git directory and work tree as the policy found them, in place
	// of the -C, --git-dir and --work-tree it gave, after its other global
	// options. Each prepared call is run.
	pin := map[string][]string{
		"wt":   {"--git-dir=" + filepath.Join(r.op, ".git", "worktrees", "a"), "--work-tree=" + r.worktree},
		"op":   {"--git-dir=" + filepath.Join(r.op, ".git"), "--work-tree=" + r.op},
		"repo": {"--git-dir=" + filepath.Join(r.repo, ".git"), "--work-tree=" + r.repo},
	}
	for _, c := range []struct {
		dir      string
		args     []string
		repo     string // pin's key; "" runs as called
		readOnly bool
	}{
		{r.worktree, []string{"status", "--short"}, "wt", true},
		{r.worktree, []string{"-c", "log.showsignature=false", "log", "-1", "--format=%H:%ct"}, "wt", true}, // Go's VCS stamping
		{r.worktree, []string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "diff", "HEAD"}, "wt", true},
		{r.worktree, []string{"rev-parse", "--show-toplevel"}, "wt", true},
		{r.worktree, []string{"ls-files"}, "wt", true},
		{r.worktree, []string{"branch", "--show-current"}, "wt", true},
		{r.worktree, []string{"branch", "--list", "fae/*"}, "wt", true},
		{r.worktree, []string{"tag", "--list"}, "wt", true},
		{r.worktree, []string{"config", "user.name"}, "wt", true},
		{r.worktree, []string{"config", "get", "remote.origin.url"}, "wt", true},
		{r.worktree, []string{"worktree", "list", "--porcelain"}, "wt", true},
		{r.worktree, []string{"symbolic-ref", "--short", "HEAD"}, "wt", true},
		{r.scratch, []string{"-C", r.op, "log", "--oneline"}, "op", true},
		{r.worktree, []string{"version"}, "", false},
		{r.scratch, []string{"init", "-q", filepath.Join(r.scratch, "new")}, "", false},
		{r.worktree, []string{"init", "-q", "nested"}, "", false},
		{r.scratch, []string{"clone", "-q", r.worktree, filepath.Join(r.scratch, "clone")}, "", false},
		{r.repo, []string{"-c", "user.name=x", "commit", "-q", "--allow-empty", "-m", "own"}, "repo", false},
		// Settings that name no program, and local repositories by path or file:// URL.
		{r.repo, []string{"-c", "core.hooksPath=/dev/null", "-c", "credential.helper=", "-c", "core.pager=cat", "-c", "pager.log=false", "-c", "core.fsmonitor=off", "status", "--short"}, "repo", false},
		{r.repo, []string{"fetch", "-q", r.op, "main:refs/heads/from-op"}, "repo", false},
		{r.repo, []string{"fetch", "-q", "file://" + r.op, "main:refs/heads/from-op-url"}, "repo", false},
		{r.repo, []string{"ls-remote", "--sort=refname", r.op}, "repo", false},
		{r.repo, []string{"merge", "-q", "-s", "ours", "--allow-unrelated-histories", "-m", "m", "from-op"}, "repo", false},
		{r.repo, []string{"branch", "-f", "side", "HEAD"}, "repo", false},
		{r.repo, []string{"config", "remote.origin.url", r.op}, "repo", false},
		{r.repo, []string{"config", "set", "user.name", "Own"}, "repo", false},
		{r.repo, []string{"update-ref", "refs/heads/other", "HEAD"}, "repo", false},
		{r.repo, []string{"worktree", "add", "-q", filepath.Join(r.scratch, "repo-wt")}, "repo", false},
		{r.repo, []string{"format-patch", "-q", "-o", filepath.Join(r.scratch, "patches"), "-1"}, "repo", false},
		{r.repo, []string{"gc", "-q", "--prune=now"}, "repo", false},
		{r.scratch, []string{"config", "--file", filepath.Join(r.scratch, "x.cfg"), "alias.p", "push"}, "", false},
	} {
		argv, err := prepare(true, c.dir, nil, c.args...)
		want := append([]string{r.git}, c.args...)
		if c.repo != "" {
			want = []string{r.git}
			if c.readOnly {
				want = append(want, "--no-optional-locks")
			}
			at, keep, rest := c.dir, []string{}, c.args
			for len(rest) > 1 && (rest[0] == "-c" || rest[0] == "-C") {
				if rest[0] == "-C" {
					at = rest[1]
				} else {
					keep = append(keep, rest[:2]...)
				}
				rest = rest[2:]
			}
			want = append(append(append(append(want, "-C", at), keep...), pin[c.repo]...), rest...)
		}
		if err != nil || strings.Join(argv, "\x00") != strings.Join(want, "\x00") {
			t.Errorf("%q in %s: prepared %q, want %q, %v", c.args, c.dir, argv, want, err)
			continue
		}
		r.run(t, c.dir, nil, argv[1:]...)
	}
	// No repository: git runs as called and fails on its own. The ceiling
	// stops discovery at the scratch directory, which test.sh puts inside a
	// checkout.
	if argv, err := prepare(true, filepath.Join(r.scratch, "plain"), []string{"GIT_CEILING_DIRECTORIES=" + r.scratch}, "status"); err != nil || len(argv) != 2 {
		t.Errorf("status outside any repository: %q %v", argv, err)
	}
	if got := r.run(t, r.repo, nil, "config", "remote.origin.url"); strings.TrimSpace(got) != r.op {
		t.Errorf("the test's own repository's config was not written: %q", got)
	}
	// A repository with more files than the policy reads is refused.
	savedLimit := accountedGitWalkLimit
	accountedGitWalkLimit = 3
	_, err = prepare(true, r.repo, nil, "status")
	accountedGitWalkLimit = savedLimit
	var refused *Refusal
	if !errors.As(err, &refused) || !strings.Contains(err.Error(), "files besides its objects to check for links out of it") {
		t.Errorf("a repository too large to check: %v", err)
	} else {
		seen = append(seen, refused.Reason)
	}
	if after := r.state(t); after != before {
		t.Errorf("the operator's repository changed:\n%s\nwas\n%s", after, before)
	}

	// The census: every refusal source in accounted_git.go fired, each
	// refuse(...) and each Local mode refusal (&LocalRefusal{...}, exit 78).
	raw, err := os.ReadFile("accounted_git.go")
	if err != nil {
		t.Fatal(err)
	}
	missing := []string{}
	sources := regexp.MustCompile(`(?m)refuse\((.*)\)\s*$`).FindAllStringSubmatch(string(raw), -1)
	locals := regexp.MustCompile(`(?m)&LocalRefusal\{(.*)\}\s*$`).FindAllStringSubmatch(string(raw), -1)
	if len(locals) < 3 {
		t.Fatalf("%d Local mode refusal sources found", len(locals))
	}
	sources = append(sources, locals...)
	for _, m := range sources {
		// A refusal fired when one reason holds all its literal text.
		literals := []string{}
		for _, l := range regexp.MustCompile(`"((?:[^"\\]|\\.)*)"`).FindAllString(m[1], -1) {
			text, err := strconv.Unquote(l)
			if err != nil {
				t.Fatal(err)
			}
			literals = append(literals, text)
		}
		fired := false
		for _, reason := range seen {
			all := true
			for _, l := range literals {
				all = all && strings.Contains(reason, l)
			}
			fired = fired || all
		}
		if !fired {
			missing = append(missing, m[1])
		}
	}
	if len(sources) < 15 {
		t.Fatalf("%d refusal sources found", len(sources))
	}
	sort.Strings(missing)
	if len(missing) != 0 {
		t.Errorf("refusals never fired: %q", missing)
	}
}

// accountedGitResolve resolves a path as the system takes it when git
// creates and writes it: an existing component through its links, a missing
// one as the directory git creates, and a ".." up from where the previous
// component lies; nothing is cleaned as text. A component that exists but
// cannot be followed is refused.
func TestAccountedGitResolveAsGitCreates(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Join(root, "a", "b"), 0700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "f"), "file\n")
	for link, target := range map[string]string{"l": filepath.Join(root, "a", "b"), "dl": filepath.Join(root, "nowhere", "file"), "loop": filepath.Join(root, "loop")} {
		if err = os.Symlink(target, filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		dir, p, want string
		at           string // the component a refusal names
	}{
		{root, "l/../x", filepath.Join(root, "a", "x"), ""},
		{root, "missing/../l/../x", filepath.Join(root, "a", "x"), ""},
		{root, "missing/../l/..", filepath.Join(root, "a"), ""},
		{root, "l/missing/../x", filepath.Join(root, "a", "b", "x"), ""},
		{root, "l/missing/deeper/../../../x", filepath.Join(root, "a", "x"), ""},
		{root, "missing/x/y", filepath.Join(root, "missing", "x", "y"), ""},
		{root, "missing/x/../../f", filepath.Join(root, "f"), ""},
		{root, "./a/./b/", filepath.Join(root, "a", "b"), ""},
		{root, "a/b/../../f", filepath.Join(root, "f"), ""},
		{root, "a/b", filepath.Join(root, "a", "b"), ""},
		{filepath.Join(root, "a"), "../f", filepath.Join(root, "f"), ""},
		{"/", root + "/l/../x", filepath.Join(root, "a", "x"), ""}, // absolute: dir is unused
		{root, "/..", "/", ""},
		{root, "dl", "", "dl"},
		{root, "dl/x", "", "dl"},
		{root, "missing/../dl", "", "dl"},
		{root, "loop/x", "", "loop"},
		{root, "f/x", "", "f/x"},
	} {
		got, err := accountedGitResolve(c.dir, c.p)
		var refused *Refusal
		if c.at != "" {
			if !errors.As(err, &refused) || !strings.Contains(err.Error(), "cannot be followed at "+filepath.Join(root, c.at)+" (") {
				t.Errorf("%s from %s: %q %v, want a refusal naming %s", c.p, c.dir, got, err, c.at)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%s from %s: %q %v, want %q", c.p, c.dir, got, err, c.want)
		}
	}
}

// Outside the run's directories and the system temporary ones, every
// repository is protected like the run's own: a self-hosting clone's nested
// git may not change the operator's checkout it was cloned from.
func TestAccountedGitProtectsRepositoriesOutsideTheRun(t *testing.T) {
	r := newAccountedGitRun(t)
	saved := accountedGitTemporaryRoots
	accountedGitTemporaryRoots = nil // the fixture's temporary directory stands for the rest of the disk
	defer func() { accountedGitTemporaryRoots = saved }()
	elsewhere := filepath.Join(filepath.Dir(r.scratch), "elsewhere")
	r.run(t, r.scratch, nil, "init", "-q", elsewhere)
	p := Policy{Worktree: r.worktree, Scratch: r.scratch, CWD: elsewhere}
	if _, err := p.PrepareAccounted([]string{r.git, "commit", "--allow-empty", "-m", "x"}, r.env); err == nil || !strings.Contains(err.Error(), "nested git commit is not a read-only command, and the repository at "+filepath.Join(elsewhere, ".git")) {
		t.Fatalf("a repository outside the run was changed: %v", err)
	}
	if _, err := p.PrepareAccounted([]string{r.git, "init", "-q", filepath.Join(filepath.Dir(elsewhere), "new")}, r.env); err == nil || !strings.Contains(err.Error(), "would create or reinitialize a repository") {
		t.Fatalf("a repository was created outside the run: %v", err)
	}
	p.CWD = r.repo
	if _, err := p.PrepareAccounted([]string{r.git, "commit", "--allow-empty", "-m", "x"}, r.env); err != nil {
		t.Fatalf("the run's scratch repository was protected: %v", err)
	}
}

// wrappers installs account-mode wrappers for r's run, whose broker is this
// test binary (TestMain) and whose real git is the one on PATH.
func (r accountedGitRun) wrappers(t *testing.T, local bool) string {
	t.Helper()
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	shims, _ := installAccountedForTest(t, parent, RoleShims{Worktree: r.worktree, Scratch: r.scratch, Local: local}, filepath.Dir(r.git)+":/usr/bin:/bin")
	return shims
}

// wrapped runs the git wrapper in dir with only env and the wrappers first on
// PATH, and returns its exit code and output.
func wrapped(t *testing.T, shims, dir string, env []string, args ...string) (int, string) {
	t.Helper()
	cmd := exec.Command(filepath.Join(shims, "git"), args...)
	cmd.Dir = dir
	cmd.Env = append([]string{"PATH=" + shims + ":/usr/bin:/bin"}, env...)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatal(err)
	}
	return cmd.ProcessState.ExitCode(), string(out)
}

// FAE-MODE-002, FAE-EXT-001: a nested git call loads no configuration HOME or
// XDG_CONFIG_HOME chooses, though it drops the broker's GIT_CONFIG_GLOBAL and
// GIT_CONFIG_NOSYSTEM: not on a read of the run's own worktree, where such a
// file could name a program (here a core.fsmonitor hook) that runs the real
// git past the wrappers, and not on a test's own repository. Each call passes
// the real wrapper, the broker (this test binary) and the policy, and runs
// the real git with the environment the policy gives it. A global file the
// call names itself on a test's own repository is read, once the policy has
// checked that it names no program.
func TestAccountedGitLoadsNoHomeConfiguration(t *testing.T) {
	r := newAccountedGitRun(t)
	shims := r.wrappers(t, true)
	root := filepath.Dir(r.scratch)
	monitor := func(name string) string {
		hook := filepath.Join(root, name+"-monitor")
		write(t, hook, "#!/bin/sh\n: >'"+filepath.Join(root, name+".ran")+"'\nexit 1\n")
		if err := os.Chmod(hook, 0700); err != nil {
			t.Fatal(err)
		}
		return hook
	}
	home, xdg := filepath.Join(root, "home"), filepath.Join(root, "xdg")
	if err := os.MkdirAll(filepath.Join(xdg, "git"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(home, ".gitconfig"), "[core]\n\tfsmonitor = "+monitor("home")+"\n")
	write(t, filepath.Join(xdg, "git", "config"), "[core]\n\tfsmonitor = "+monitor("xdg")+"\n")
	for _, c := range []struct {
		dir    string
		env    []string
		marker string
	}{
		{r.worktree, []string{"HOME=" + home}, "home"},
		{r.worktree, []string{"HOME=" + root, "XDG_CONFIG_HOME=" + xdg}, "xdg"},
		{r.worktree, []string{"HOME=" + home, "GIT_CONFIG_NOSYSTEM=0", "GIT_CONFIG_GLOBAL="}, "home"},
		{r.repo, []string{"HOME=" + home}, "home"},
	} {
		if code, out := wrapped(t, shims, c.dir, c.env, "status", "--short"); code != 0 {
			t.Errorf("status in %s with %q: exit %d %s", c.dir, c.env, code, out)
		}
		if _, err := os.Stat(filepath.Join(root, c.marker+".ran")); err == nil {
			t.Errorf("status in %s with %q ran the program its HOME's configuration names", c.dir, c.env)
		}
	}
	// The control: that configuration does make git run the program, named
	// by GIT_CONFIG_GLOBAL. When git on PATH is an outer run's wrapper (Fae's
	// own suite in account mode), whose policy refuses such a file, there is
	// no control.
	if !accountedGitWrapped(r.git) {
		cmd := exec.Command(r.git, "status", "--short")
		cmd.Dir, cmd.Env = r.repo, []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_GLOBAL=" + filepath.Join(home, ".gitconfig")}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatal(err, string(out))
		}
		if _, err := os.Stat(filepath.Join(root, "home.ran")); err != nil {
			t.Fatal("the control did not run the configured program:", err)
		}
	}
	// A global file named by the call on its own repository is read.
	own := filepath.Join(root, "own.cfg")
	write(t, own, "[user]\n\tname = Own\n\temail = own@localhost\n")
	if code, out := wrapped(t, shims, r.repo, []string{"GIT_CONFIG_GLOBAL=" + own}, "commit", "-q", "--allow-empty", "-m", "own"); code != 0 {
		t.Fatalf("commit with its own global file: exit %d %s", code, out)
	}
	if got := strings.TrimSpace(r.run(t, r.repo, nil, "log", "-1", "--format=%an")); got != "Own" {
		t.Errorf("author %q, want the call's own global file's", got)
	}
}

// FAE-GEN-010: a nested git call reaches only local repositories. Besides the
// URLs the policy refuses by name, git itself refuses every other transport
// (GIT_ALLOW_PROTOCOL=file): a remote the test's own repository names by an
// https URL, a url.<base>.insteadOf that rewrites a path to one, whatever
// protocol.*.allow says. Bundles a repository's configuration names are not
// downloaded. Local repositories, by path or file:// URL, are fetched,
// cloned and listed. 127.0.0.1:1 refuses connections, so nothing leaves the
// host even where git would try.
func TestAccountedGitReachesOnlyLocalRepositories(t *testing.T) {
	r := newAccountedGitRun(t)
	shims := r.wrappers(t, true)
	r.run(t, r.repo, nil, "config", "remote.web.url", "https://127.0.0.1:1/x.git")
	env := []string{"HOME=" + filepath.Dir(r.scratch), "GIT_TERMINAL_PROMPT=0"}
	for _, args := range [][]string{
		{"fetch", "web"},
		{"-c", "protocol.allow=always", "-c", "protocol.https.allow=always", "ls-remote", "web"},
		{"-c", "url.https://127.0.0.1:1/.insteadOf=/", "fetch", r.op},
		{"pull", "web", "main"},
	} {
		if code, out := wrapped(t, shims, r.repo, env, args...); code == 0 || !strings.Contains(out, "transport 'https' not allowed") {
			t.Errorf("%q: exit %d %s", args, code, out)
		}
	}
	r.run(t, r.repo, nil, "config", "fetch.bundleURI", "https://127.0.0.1:1/b.bundle")
	if code, out := wrapped(t, shims, r.repo, env, "fetch", "-q", r.op, "main:refs/heads/from-op"); code != 0 || strings.Contains(out, "bundle") {
		t.Errorf("local fetch with a configured bundle URI: exit %d %s", code, out)
	}
	if code, out := wrapped(t, shims, r.scratch, env, "clone", "-q", "file://"+r.op, filepath.Join(r.scratch, "local")); code != 0 {
		t.Errorf("local clone: exit %d %s", code, out)
	}
	if code, out := wrapped(t, shims, r.repo, env, "ls-remote", r.worktree); code != 0 || !strings.Contains(out, "refs/heads/fae/task/a") {
		t.Errorf("local ls-remote: exit %d %s", code, out)
	}
	if code, out := wrapped(t, shims, r.repo, env, "fetch", "https://127.0.0.1:1/x.git"); code != Refused || !strings.Contains(out, "a repository off this host") {
		t.Errorf("a URL named by the call: exit %d %s", code, out)
	}
	// The bundle settings follow the call's own GIT_CONFIG_COUNT settings.
	counted := append(env, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=user.name", "GIT_CONFIG_VALUE_0=Counted")
	if code, out := wrapped(t, shims, r.repo, counted, "config", "user.name"); code != 0 || out != "Counted\n" {
		t.Errorf("the call's own GIT_CONFIG_COUNT setting: exit %d %q", code, out)
	}
}

// FAE-MODE-002 on the default path: the refusal wrappers a Local role session
// without a broker gets, as a Local task's brokered run in refuse mode does,
// exit 78 for a push or remote change, found past git's global options as
// RunShim finds it, and 77 with the denial text for every other call; a
// Normal one's exit 77 for every call.
func TestLocalDenialWrappersGivePushTheLocalCode(t *testing.T) {
	for _, local := range []bool{true, false} {
		dir, err := InstallRoleShims(t.TempDir(), RoleShims{Local: local, Build: true}, "/usr/bin:/bin")
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range []struct {
			script string
			code   int
		}{
			{"git push origin HEAD", LocalModeRefused},
			{"git -C . -c a.b=c --no-pager push", LocalModeRefused},
			{"git --git-dir=.git remote add x y", LocalModeRefused},
			{"git send-pack x", LocalModeRefused},
			{"git http-push x", LocalModeRefused},
			{"git status", Refused},
			{"git -c push=1 log", Refused},
			{"git -C push status", Refused},
			{"git", Refused},
			{"go version", Refused},
		} {
			want, text := c.code, "fae: command refused in Local mode (exit 78): git "
			if !local || want == Refused {
				want, text = Refused, denialMessage
			}
			cmd := exec.Command("/bin/sh", "-c", c.script)
			cmd.Env = PrependPath([]string{"PATH=/usr/bin:/bin"}, dir)
			out, _ := cmd.CombinedOutput()
			if code := cmd.ProcessState.ExitCode(); code != want || !strings.Contains(string(out), text) {
				t.Errorf("local=%t %q: exit %d %q, want %d %q", local, c.script, code, out, want, text)
			}
		}
	}
}
