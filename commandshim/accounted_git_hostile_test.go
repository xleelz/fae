package commandshim

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The routes below are re-audit 9's nested git probes (scratchpad ra9E,
// ra9F): each passed the wrappers and changed the operator's repository.
// Every call passes the real wrapper, the broker (this test binary) and the
// policy, and runs the real git with the environment the policy gives it.
// Each needs deliberately hostile test code; a hostile test can also run
// /usr/bin/git itself, so this is guidance, not containment.

// planter writes a program that records that it ran and then, with the real
// git and none of its caller's repository variables, plants the branch name
// at the operator's main in the operator's repository, as any program git
// starts can. It returns the program and the file it records in.
func (r accountedGitRun) planter(t *testing.T, name string) (program, marker string) {
	t.Helper()
	root := filepath.Dir(r.scratch)
	program, marker = filepath.Join(root, name+"-program"), filepath.Join(root, name+".ran")
	main := strings.TrimSpace(r.run(t, r.op, nil, "rev-parse", "main"))
	write(t, program, "#!/bin/sh\n: >'"+marker+"'\nunset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_COMMON_DIR GIT_OBJECT_DIRECTORY GIT_PREFIX\n'"+
		r.git+"' -C '"+r.op+"' update-ref refs/heads/"+name+" "+main+"\nexit 0\n")
	if err := os.Chmod(program, 0700); err != nil {
		t.Fatal(err)
	}
	return program, marker
}

// unchanged fails the test unless the operator's repository is as before and
// the program that records in marker never ran.
func (r accountedGitRun) unchanged(t *testing.T, before, marker string) {
	t.Helper()
	if after := r.state(t); after != before {
		t.Errorf("the operator's repository changed:\n%s\nwas\n%s", after, before)
	}
	if marker == "" {
		return
	}
	if _, err := os.Stat(marker); err == nil {
		t.Errorf("the program %s records in ran", marker)
	}
}

// FAE-MODE-002, FAE-EXT-001: an environment variable git reads as a program
// (GIT_TEST_FSMONITOR, which git status runs as its file-system monitor
// hook) is refused, on the operator's repository as on any other, along with
// every GIT_* variable the policy does not know.
func TestAccountedGitRunsNoProgramAnEnvironmentVariableNames(t *testing.T) {
	r := newAccountedGitRun(t)
	shims := r.wrappers(t, true)
	program, marker := r.planter(t, "planted")
	before := r.state(t)
	for _, dir := range []string{r.worktree, r.repo} {
		code, out := wrapped(t, shims, dir, []string{"GIT_TEST_FSMONITOR=" + program}, "status", "--short")
		if code != Refused || !strings.Contains(out, "nested git with GIT_TEST_FSMONITOR set") {
			t.Errorf("status in %s with GIT_TEST_FSMONITOR: exit %d %s", dir, code, out)
		}
	}
	r.unchanged(t, before, marker)
	// Without it, status runs.
	if code, out := wrapped(t, shims, r.worktree, nil, "status", "--short"); code != 0 {
		t.Errorf("status: exit %d %s", code, out)
	}
}

// FAE-VCS-010: a global configuration file a call on a test's own repository
// names may not send git's trace into the operator's repository
// (trace2.eventTarget, read only from global and system files, appended
// JSON to the operator's .git/config and broke every later git call there).
// Every nested call runs with GIT_TRACE2, GIT_TRACE2_PERF and
// GIT_TRACE2_EVENT set to 0 unless it sets them itself, which outranks the
// file; the rest of the file is read.
func TestAccountedGitTracesNowhereAConfigurationFileNames(t *testing.T) {
	r := newAccountedGitRun(t)
	shims := r.wrappers(t, true)
	global := filepath.Join(r.scratch, "trace.cfg")
	write(t, global, "[trace2]\n\teventTarget = "+filepath.Join(r.op, ".git", "config")+"\n\tnormalTarget = "+filepath.Join(r.op, ".git", "description")+
		"\n\tperfTarget = "+filepath.Join(r.op, ".git", "description")+"\n[user]\n\tname = Traced\n")
	description, err := os.ReadFile(filepath.Join(r.op, ".git", "description"))
	if err != nil {
		t.Fatal(err)
	}
	before := r.state(t)
	env := []string{"GIT_CONFIG_GLOBAL=" + global}
	if code, out := wrapped(t, shims, r.repo, env, "status", "--short"); code != 0 {
		t.Errorf("status with a named global file: exit %d %s", code, out)
	}
	if code, out := wrapped(t, shims, r.repo, env, "config", "user.name"); code != 0 || out != "Traced\n" {
		t.Errorf("the named global file was not read: exit %d %q", code, out)
	}
	r.unchanged(t, before, "")
	if after, err := os.ReadFile(filepath.Join(r.op, ".git", "description")); err != nil || string(after) != string(description) {
		t.Errorf("the operator's .git/description changed: %q %v", after, err)
	}
	// A trace the call sends to a file it may write is kept.
	events := filepath.Join(r.scratch, "events.json")
	if code, out := wrapped(t, shims, r.repo, append(env, "GIT_TRACE2_EVENT="+events), "status", "--short"); code != 0 {
		t.Errorf("status with its own trace file: exit %d %s", code, out)
	}
	if st, err := os.Stat(events); err != nil || st.Size() == 0 {
		t.Errorf("the call's own trace file was not written: %v", err)
	}
}

// FAE-VCS-010: format-patch writes its patches only where the call may
// write, wherever its directory comes from: -c format.outputDirectory, a
// global file the call names, or the test repository's own configuration.
// Each wrote into the operator's .git while format-patch -o there was
// refused.
func TestAccountedGitWritesPatchesOnlyWhereTheCallMay(t *testing.T) {
	r := newAccountedGitRun(t)
	shims := r.wrappers(t, true)
	repo := filepath.Join(r.scratch, "patched")
	r.run(t, r.scratch, nil, "init", "-q", "-b", "main", repo)
	write(t, filepath.Join(repo, "a.txt"), "a\n")
	r.run(t, repo, nil, "add", "a.txt")
	r.run(t, repo, nil, "commit", "-q", "-m", "a")
	before := r.state(t)
	pp := filepath.Join(r.op, ".git", "pp")
	if code, out := wrapped(t, shims, repo, nil, "-c", "format.outputDirectory="+pp, "format-patch", "-1"); code != Refused || !strings.Contains(out, "(format.outputDirectory)") {
		t.Errorf("format-patch with -c format.outputDirectory: exit %d %s", code, out)
	}
	global := filepath.Join(r.scratch, "format.cfg")
	write(t, global, "[format]\n\toutputDirectory = "+pp+"2\n")
	if code, out := wrapped(t, shims, repo, []string{"GIT_CONFIG_GLOBAL=" + global}, "format-patch", "-1"); code != Refused || !strings.Contains(out, "(format.outputDirectory)") {
		t.Errorf("format-patch with a named global file's format.outputDirectory: exit %d %s", code, out)
	}
	config, err := os.OpenFile(filepath.Join(repo, ".git", "config"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = config.WriteString("[format]\n\toutputDirectory = " + pp + "3\n"); err != nil {
		t.Fatal(err)
	}
	if err = config.Close(); err != nil {
		t.Fatal(err)
	}
	if code, out := wrapped(t, shims, repo, nil, "format-patch", "-1"); code != Refused || !strings.Contains(out, "(format.outputDirectory)") {
		t.Errorf("format-patch with its repository's format.outputDirectory: exit %d %s", code, out)
	}
	for _, dir := range []string{pp, pp + "2", pp + "3"} {
		if _, err := os.Stat(dir); err == nil {
			t.Errorf("format-patch wrote %s", dir)
		}
	}
	r.unchanged(t, before, "")
	// Patches go where the call may write.
	if code, out := wrapped(t, shims, repo, nil, "format-patch", "-q", "-o", filepath.Join(r.scratch, "patches"), "-1"); code != 0 {
		t.Errorf("format-patch -o: exit %d %s", code, out)
	}
	if matches, _ := filepath.Glob(filepath.Join(r.scratch, "patches", "*.patch")); len(matches) != 1 {
		t.Errorf("format-patch -o wrote %q", matches)
	}
}

// FAE-VCS-002, FAE-VCS-010: a test repository whose .git/refs,
// .git/worktrees or .git/objects/pack is a symbolic link into the operator's
// repository (here with the operator's objects borrowed through alternates)
// can no longer move the operator's branches with update-ref, remove another
// task's worktree with worktree remove, or delete the operator's objects with
// gc --prune=now: the policy reads the repository's git directories for links
// git would write through.
func TestAccountedGitRefusesRepositoriesLinkedIntoTheOperators(t *testing.T) {
	r := newAccountedGitRun(t)
	shims := r.wrappers(t, true)
	r.run(t, r.op, nil, "gc", "-q")
	refs, worktrees, packs := r.testRepo(t, "refs"), r.testRepo(t, "worktrees"), r.testRepo(t, "packs")
	r.linkInto(t, refs, "refs")
	r.linkInto(t, worktrees, "worktrees")
	r.linkInto(t, packs, filepath.Join("objects", "pack"))
	main := strings.TrimSpace(r.run(t, r.op, nil, "rev-parse", "main"))
	before := r.state(t)
	if code, out := wrapped(t, shims, refs, nil, "update-ref", "refs/heads/planted", main); code != Refused || !strings.Contains(out, "links "+filepath.Join(refs, ".git", "refs")) {
		t.Errorf("update-ref through a linked refs: exit %d %s", code, out)
	}
	if code, out := wrapped(t, shims, worktrees, nil, "worktree", "remove", "--force", r.other); code != Refused {
		t.Errorf("worktree remove through a linked worktrees: exit %d %s", code, out)
	}
	if code, out := wrapped(t, shims, worktrees, nil, "worktree", "prune"); code != Refused || !strings.Contains(out, "links "+filepath.Join(worktrees, ".git", "worktrees")) {
		t.Errorf("worktree prune through a linked worktrees: exit %d %s", code, out)
	}
	if code, out := wrapped(t, shims, packs, nil, "gc", "-q", "--prune=now"); code != Refused || !strings.Contains(out, "links "+filepath.Join(packs, ".git", "objects", "pack")) {
		t.Errorf("gc through a linked objects/pack: exit %d %s", code, out)
	}
	r.unchanged(t, before, "")
	r.run(t, r.op, nil, "fsck", "--no-dangling")
	if _, err := os.Stat(r.other); err != nil {
		t.Errorf("the other task's worktree is gone: %v", err)
	}
}

// FAE-MODE-002, FAE-VCS-010: no nested call runs a hook. A nested git config
// may not install one (core.hooksPath is refused as on the command line), and
// hooks the test put in its own repository itself, in .git/hooks or as a
// configured hook (hook.<name>.command), do not run either: every call runs
// with core.hooksPath=/dev/null and hook.<event>.enabled=false. A hook runs
// the real git, past the wrappers; before, one moved the operator's
// branches.
func TestAccountedGitRunsNoHook(t *testing.T) {
	r := newAccountedGitRun(t)
	shims := r.wrappers(t, true)
	program, marker := r.planter(t, "hooked")
	hooks := filepath.Join(r.scratch, "hooks")
	if err := os.Mkdir(hooks, 0700); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(program)
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{hooks, filepath.Join(r.repo, ".git", "hooks")} {
		if err = os.WriteFile(filepath.Join(dir, "post-commit"), body, 0700); err != nil {
			t.Fatal(err)
		}
	}
	config, err := os.OpenFile(filepath.Join(r.repo, ".git", "config"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = config.WriteString("[hook \"planter\"]\n\tcommand = " + program + "\n\tevent = pre-commit\n"); err != nil {
		t.Fatal(err)
	}
	if err = config.Close(); err != nil {
		t.Fatal(err)
	}
	before := r.state(t)
	if code, out := wrapped(t, shims, r.repo, nil, "config", "core.hooksPath", hooks); code != Refused || !strings.Contains(out, `nested git config "core.hooksPath" names a program`) {
		t.Errorf("config core.hooksPath: exit %d %s", code, out)
	}
	if code, out := wrapped(t, shims, r.repo, nil, "commit", "-q", "--allow-empty", "-m", "hooked"); code != 0 {
		t.Errorf("commit: exit %d %s", code, out)
	}
	r.unchanged(t, before, marker)
	if got := strings.TrimSpace(r.run(t, r.repo, nil, "log", "-1", "--format=%s")); got != "hooked" {
		t.Errorf("the test's own repository's commit is %q", got)
	}
	// The control: git as called runs the hooks, unless git on PATH is an
	// outer run's wrapper (Fae's own suite in account mode), which runs none.
	cmd := exec.Command(r.git, "commit", "-q", "--allow-empty", "-m", "control")
	cmd.Dir, cmd.Env = r.repo, r.env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	if _, err := os.Stat(marker); err != nil && !accountedGitWrapped(r.git) {
		t.Errorf("the control ran no hook: %v", err)
	}
}

// FAE-MODE-002, FAE-EXT-001, FAE-VCS-010: a call cannot switch a hook back
// on. git reads a call's -c and --config-env settings after GIT_CONFIG_COUNT,
// where the policy puts hook.<event>.enabled=false, so the call's own setting
// won: re-audit 10's probe P1 ran `git -c hook.pre-commit.enabled=true
// commit` in a test repository whose configuration named a pre-commit hook,
// and the hook moved the operator's main. Every hook.* setting a call gives
// git but enabled=false is now refused, on every repository and through
// every source a call can set configuration with: -c, --config-env,
// GIT_CONFIG_COUNT, clone --config, a configuration file it names, and a
// nested git config write. Switching a hook off still passes, and the
// commit then runs with no hook.
func TestAccountedGitCallCannotSwitchAHookBackOn(t *testing.T) {
	r := newAccountedGitRun(t)
	shims := r.wrappers(t, true)
	program, marker := r.planter(t, "rehooked")
	config, err := os.OpenFile(filepath.Join(r.repo, ".git", "config"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = config.WriteString("[hook \"planter\"]\n\tcommand = " + program + "\n\tevent = pre-commit\n"); err != nil {
		t.Fatal(err)
	}
	if err = config.Close(); err != nil {
		t.Fatal(err)
	}
	named := filepath.Join(r.scratch, "hooks.cfg")
	write(t, named, "[hook \"pre-commit\"]\n\tenabled = true\n")
	commit := []string{"commit", "-q", "--allow-empty", "-m", "rehooked"}
	before := r.state(t)
	head := r.run(t, r.repo, nil, "rev-parse", "HEAD")
	for _, c := range []struct {
		dir    string
		env    []string
		args   []string
		reason string
	}{
		{r.repo, nil, append([]string{"-c", "hook.pre-commit.enabled=true"}, commit...), `nested git -c "hook.pre-commit.enabled" configures a hook`},
		{r.repo, nil, append([]string{"-c", "hook.pre-commit.enabled"}, commit...), `nested git -c "hook.pre-commit.enabled" configures a hook`},
		{r.repo, nil, append([]string{"-c", "Hook.Pre-Commit.Enabled=yes"}, commit...), `nested git -c "Hook.Pre-Commit.Enabled" configures a hook`},
		{r.repo, []string{"ON=true"}, append([]string{"--config-env=hook.pre-commit.enabled=ON"}, commit...), `nested git -c "hook.pre-commit.enabled" configures a hook`},
		{r.repo, []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=hook.pre-commit.enabled", "GIT_CONFIG_VALUE_0=true"}, commit, "nested git with GIT_CONFIG_KEY_0 set configures a hook"},
		{r.repo, []string{"GIT_CONFIG_PARAMETERS='hook.pre-commit.enabled'='true'"}, commit, "with GIT_CONFIG_PARAMETERS set"},
		{r.repo, []string{"GIT_CONFIG_GLOBAL=" + named}, commit, `whose "hook.pre-commit.enabled" configures a hook`},
		{r.repo, nil, append([]string{"-c", "hook.other.event=pre-commit"}, commit...), `nested git -c "hook.other.event" configures a hook`},
		{r.repo, nil, append([]string{"-c", "hook.other.command=" + program}, commit...), `nested git -c "hook.other.command" names a program`},
		{r.repo, nil, append([]string{"-c", "hook.jobs=4"}, commit...), `nested git -c "hook.jobs" configures a hook`},
		{r.repo, nil, []string{"config", "hook.pre-commit.enabled", "true"}, `nested git config "hook.pre-commit.enabled" configures a hook`},
		{r.repo, nil, []string{"config", "set", "hook.planter.event", "post-commit"}, `nested git config "hook.planter.event" configures a hook`},
		{r.scratch, nil, []string{"config", "--file", filepath.Join(r.scratch, "x.cfg"), "hook.pre-commit.enabled", "on"}, `nested git config "hook.pre-commit.enabled" configures a hook`},
		{r.scratch, nil, []string{"clone", "-q", "--config", "hook.pre-commit.enabled=true", r.repo, filepath.Join(r.scratch, "rehooked-clone")}, `nested git clone --config "hook.pre-commit.enabled" configures a hook`},
		// The run's own repository, which a call may only read.
		{r.worktree, nil, []string{"-c", "hook.pre-commit.enabled=true", "status"}, `nested git -c "hook.pre-commit.enabled" configures a hook`},
	} {
		if code, out := wrapped(t, shims, c.dir, c.env, c.args...); code != Refused || !strings.Contains(out, c.reason) {
			t.Errorf("%q with %q in %s: exit %d %s, want %d %q", c.args, c.env, c.dir, code, out, Refused, c.reason)
		}
	}
	r.unchanged(t, before, marker)
	if now := r.run(t, r.repo, nil, "rev-parse", "HEAD"); now != head {
		t.Errorf("a refused commit committed: %s", now)
	}
	if _, err := os.Stat(filepath.Join(r.scratch, "rehooked-clone")); err == nil {
		t.Error("a refused clone ran")
	}
	// Switching hooks off passes, through each source, and the commit runs
	// no hook.
	for _, c := range []struct {
		env  []string
		args []string
	}{
		{nil, append([]string{"-c", "hook.pre-commit.enabled=false", "-c", "hook.planter.enabled=off"}, commit...)},
		{[]string{"OFF=0"}, append([]string{"--config-env=hook.pre-commit.enabled=OFF"}, commit...)},
		{[]string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=hook.pre-commit.enabled", "GIT_CONFIG_VALUE_0=no"}, commit},
		{nil, []string{"config", "hook.planter.enabled", "false"}},
	} {
		if code, out := wrapped(t, shims, r.repo, c.env, c.args...); code != 0 {
			t.Errorf("%q with %q: exit %d %s", c.args, c.env, code, out)
		}
	}
	r.unchanged(t, before, marker)
}

// FAE-VCS-002, FAE-VCS-010: git worktree repair rewrites the .git file of
// every worktree its repository lists, whatever paths it names, and "." when
// it names none. Re-audit 10's probe P2: a test repository whose
// .git/worktrees/x/gitdir named another task's worktree, and a bare
// `git worktree repair`, which the policy checked nothing for, pointed that
// worktree's .git file at the test repository. Every listed worktree is now
// checked as a named one is, its gitdir absolute or relative; a repair of the
// test's own worktrees still runs.
func TestAccountedGitWorktreeRepairRewritesNoOtherTasksWorktree(t *testing.T) {
	r := newAccountedGitRun(t)
	shims := r.wrappers(t, true)
	dotgit := filepath.Join(r.other, ".git")
	other, err := os.ReadFile(dotgit)
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(filepath.Join(r.scratch, "rel", ".git", "worktrees", "x"), dotgit)
	if err != nil {
		t.Fatal(err)
	}
	before := r.state(t)
	for name, gitdir := range map[string]string{"abs": dotgit, "rel": relative} {
		repo := r.testRepo(t, name)
		admin := filepath.Join(repo, ".git", "worktrees", "x")
		if err := os.MkdirAll(admin, 0700); err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(admin, "gitdir"), gitdir+"\n")
		write(t, filepath.Join(admin, "commondir"), "../..\n")
		write(t, filepath.Join(admin, "HEAD"), "ref: refs/heads/main\n")
		for _, args := range [][]string{{"worktree", "repair"}, {"worktree", "repair", "."}} {
			if code, out := wrapped(t, shims, repo, nil, args...); code != Refused || !strings.Contains(out, "would change the worktree at "+r.other) {
				t.Errorf("%s: %q: exit %d %s", name, args, code, out)
			}
		}
	}
	if now, err := os.ReadFile(dotgit); err != nil || string(now) != string(other) {
		t.Errorf("the other task's .git file is %q (%v), was %q", now, err, other)
	}
	// A listed directory whose .git file names a git directory in the run's
	// own repository that no longer exists is still the run's: git cannot
	// locate a repository from it, and the policy reads the file.
	stale := filepath.Join(r.scratch, "stale")
	if err := os.Mkdir(stale, 0700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(stale, ".git"), "gitdir: "+filepath.Join(r.op, ".git", "worktrees", "gone")+"\n")
	repo := r.testRepo(t, "stale-lister")
	admin := filepath.Join(repo, ".git", "worktrees", "s")
	if err := os.MkdirAll(admin, 0700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(admin, "gitdir"), filepath.Join(stale, ".git")+"\n")
	if code, out := wrapped(t, shims, repo, nil, "worktree", "repair"); code != Refused || !strings.Contains(out, "would change the worktree at "+stale) {
		t.Errorf("repair listing a stale worktree of the run's repository: exit %d %s", code, out)
	}
	r.unchanged(t, before, "")
	// A test's own worktree, whose .git file the test broke, is repaired.
	own := filepath.Join(r.scratch, "own-wt")
	r.run(t, r.repo, nil, "worktree", "add", "-q", "-b", "own", own)
	want, err := os.ReadFile(filepath.Join(own, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(own, ".git"), "gitdir: "+filepath.Join(r.scratch, "nowhere")+"\n")
	if code, out := wrapped(t, shims, r.repo, nil, "worktree", "repair"); code != 0 {
		t.Errorf("repair of the test's own worktree: exit %d %s", code, out)
	}
	if got, err := os.ReadFile(filepath.Join(own, ".git")); err != nil || string(got) != string(want) {
		t.Errorf("the test's own worktree's .git file is %q (%v), want %q", got, err, want)
	}
}

// FAE-VCS-002, FAE-VCS-010: a path a call names is checked where the system
// takes it. A ".." after a symbolic link leads up from where the link points,
// not from the link: with scratch/l a link to the operator's .git/refs,
// scratch/l/../config is the operator's .git/config, while the path cleaned
// as text is scratch/config, which a call may write. Each of these used to
// pass the policy and write into the operator's git directory.
func TestAccountedGitChecksPathsWhereTheSystemTakesThem(t *testing.T) {
	r := newAccountedGitRun(t)
	shims := r.wrappers(t, true)
	link := filepath.Join(r.scratch, "l")
	if err := os.Symlink(filepath.Join(r.op, ".git", "refs"), link); err != nil {
		t.Fatal(err)
	}
	up := link + string(filepath.Separator) + ".."
	description, err := os.ReadFile(filepath.Join(r.op, ".git", "description"))
	if err != nil {
		t.Fatal(err)
	}
	before := r.state(t)
	for _, c := range []struct {
		dir  string
		args []string
	}{
		{r.repo, []string{"bundle", "create", up + "/config", "HEAD"}},
		{r.repo, []string{"log", "-1", "--output=" + up + "/description"}},
		{r.repo, []string{"worktree", "add", "-q", up + "/wt"}},
		{r.scratch, []string{"init", "-q", up + "/nested"}},
		{r.scratch, []string{"clone", "-q", r.repo, up + "/cloned"}},
		{r.scratch, []string{"config", "--file", up + "/config", "user.name", "x"}},
	} {
		if code, out := wrapped(t, shims, c.dir, nil, c.args...); code != Refused {
			t.Errorf("%q in %s: exit %d %s", c.args, c.dir, code, out)
		}
	}
	r.unchanged(t, before, "")
	if now, err := os.ReadFile(filepath.Join(r.op, ".git", "description")); err != nil || string(now) != string(description) {
		t.Errorf("the operator's .git/description is %q (%v)", now, err)
	}
	for _, name := range []string{"wt", "nested", "cloned"} {
		if _, err := os.Lstat(filepath.Join(r.op, ".git", name)); err == nil {
			t.Errorf("a nested call created %s in the operator's .git", name)
		}
	}
	// A ".." after a link that leads where the call may write still passes.
	inner := filepath.Join(r.scratch, "inner", "deep")
	if err := os.MkdirAll(inner, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(inner, filepath.Join(r.scratch, "ok")); err != nil {
		t.Fatal(err)
	}
	if code, out := wrapped(t, shims, r.repo, nil, "bundle", "create", filepath.Join(r.scratch, "ok")+"/../b.bundle", "HEAD"); code != 0 {
		t.Errorf("bundle create through a link inside the scratch: exit %d %s", code, out)
	}
	if _, err := os.Stat(filepath.Join(r.scratch, "inner", "b.bundle")); err != nil {
		t.Errorf("the bundle is not where the system put it: %v", err)
	}
}

// FAE-MODE-002, FAE-EXT-001, FAE-VCS-002, FAE-VCS-010: a path a call names
// is checked as git creates and writes it. Wave 12 followed links only while
// the components existed and cleaned the rest as text, so with scratch/l a
// link to the operator's .git/refs, scratch/n1/../l/../PWN was checked as
// scratch/PWN while git created n1, followed l and created the operator's
// .git/PWN (re-audit 11's probe: git init there, format-patch -o into
// .git/hooks, worktree add, clone). And a link leading nowhere was checked
// as the link itself while open followed it and created the file it names
// in the operator's .git. Each is refused; a missing directory and a ".."
// that lead where the call may write still pass.
func TestAccountedGitChecksPathsAsGitCreatesThem(t *testing.T) {
	r := newAccountedGitRun(t)
	shims := r.wrappers(t, true)
	if err := os.Symlink(filepath.Join(r.op, ".git", "refs"), filepath.Join(r.scratch, "l")); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(r.scratch, "dl")
	if err := os.Symlink(filepath.Join(r.op, ".git", "planted"), dangling); err != nil {
		t.Fatal(err)
	}
	// via is a path through a directory that does not exist yet, then up,
	// then through the link, then up into the operator's .git.
	via := func(missing, rest string) string {
		return filepath.Join(r.scratch, missing) + "/../l/../" + rest
	}
	hooksBefore, err := os.ReadDir(filepath.Join(r.op, ".git", "hooks"))
	if err != nil {
		t.Fatal(err)
	}
	description, err := os.ReadFile(filepath.Join(r.op, ".git", "description"))
	if err != nil {
		t.Fatal(err)
	}
	before := r.state(t)
	for _, c := range []struct {
		dir  string
		args []string
	}{
		{r.scratch, []string{"init", "-q", via("n1", "PWN")}},
		{r.repo, []string{"format-patch", "-1", "-o", via("nn", "hooks")}},
		{r.repo, []string{"worktree", "add", "-q", via("mm", "stolenwt")}},
		{r.scratch, []string{"clone", "-q", r.repo, via("cc", "cloned")}},
		{r.repo, []string{"bundle", "create", via("bb", "config"), "HEAD"}},
		{r.repo, []string{"log", "-1", "--output=" + via("oo", "description")}},
		{r.scratch, []string{"config", "--file", via("ff", "config"), "user.name", "x"}},
		// A ".." after a directory under the link, reached after a missing one.
		{r.repo, []string{"bundle", "create", filepath.Join(r.scratch, "zz") + "/../l/heads/../../config", "HEAD"}},
		// A link leading nowhere: open creates the file it names.
		{r.repo, []string{"log", "-1", "--output=" + dangling}},
		{r.repo, []string{"bundle", "create", filepath.Join(r.scratch, "yy") + "/../dl", "HEAD"}},
	} {
		if code, out := wrapped(t, shims, c.dir, nil, c.args...); code != Refused {
			t.Errorf("%q in %s: exit %d %s", c.args, c.dir, code, out)
		}
	}
	r.unchanged(t, before, "")
	if now, err := os.ReadFile(filepath.Join(r.op, ".git", "description")); err != nil || string(now) != string(description) {
		t.Errorf("the operator's .git/description is %q (%v)", now, err)
	}
	for _, name := range []string{"PWN", "stolenwt", "cloned", "planted"} {
		if _, err := os.Lstat(filepath.Join(r.op, ".git", name)); err == nil {
			t.Errorf("a nested call created %s in the operator's .git", name)
		}
	}
	if hooksAfter, err := os.ReadDir(filepath.Join(r.op, ".git", "hooks")); err != nil || len(hooksAfter) != len(hooksBefore) {
		t.Errorf("the operator's .git/hooks has %d entries (%v), had %d", len(hooksAfter), err, len(hooksBefore))
	}
	// Where the created directories and the ".." lead inside the scratch,
	// the call runs and writes where the system puts it (format-patch and
	// init create the leading directories; bundle create does not, and fails
	// on its own).
	if err := os.MkdirAll(filepath.Join(r.scratch, "inner", "deep"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(r.scratch, "inner", "deep"), filepath.Join(r.scratch, "ok")); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		dir   string
		args  []string
		wrote string
	}{
		{r.repo, []string{"format-patch", "-q", "-1", "-o", filepath.Join(r.scratch, "newdir") + "/../out2"}, filepath.Join(r.scratch, "out2", "0001-fixture.patch")},
		{r.repo, []string{"format-patch", "-q", "-1", "-o", filepath.Join(r.scratch, "ok") + "/new/../out3"}, filepath.Join(r.scratch, "inner", "deep", "out3", "0001-fixture.patch")},
		{r.scratch, []string{"init", "-q", filepath.Join(r.scratch, "deep", "newer", "..", "made")}, filepath.Join(r.scratch, "deep", "made", ".git")},
		{r.repo, []string{"format-patch", "-q", "-1", "-o", filepath.Join(r.scratch, "pp", ".", "out")}, filepath.Join(r.scratch, "pp", "out", "0001-fixture.patch")},
	} {
		if code, out := wrapped(t, shims, c.dir, nil, c.args...); code != 0 {
			t.Errorf("%q in %s: exit %d %s", c.args, c.dir, code, out)
		}
		if _, err := os.Stat(c.wrote); err != nil {
			t.Errorf("%q: not where the system put it: %v", c.args, err)
		}
	}
	r.unchanged(t, before, "")
}

// accountedGitWrapped reports whether git is a Fae wrapper, which applies an
// outer run's policy (Fae's own suite in account mode): its policy file is
// beside it.
func accountedGitWrapped(git string) bool {
	_, err := os.Stat(filepath.Join(filepath.Dir(git), ".policy.json"))
	return err == nil
}

// FAE-VCS-010: the policy decides on its own rev-parse, so the call execs
// on the repository that rev-parse found, not on the path it gave: -C its
// canonical working directory and --git-dir and --work-tree its canonical
// git directory and work tree. A symbolic link the call named, switched to
// the operator's repository after the check (re-audit 9 won 25 of 150 such
// races), changes nothing there.
func TestAccountedGitRunsOnTheRepositoryItChecked(t *testing.T) {
	r := newAccountedGitRun(t)
	sw := filepath.Join(r.scratch, "sw")
	if err := os.Symlink(r.repo, sw); err != nil {
		t.Fatal(err)
	}
	p := Policy{Worktree: r.worktree, Scratch: r.scratch, CWD: r.scratch, Local: true}
	argv, err := p.PrepareAccounted([]string{r.git, "-C", "sw", "update-ref", "refs/heads/raced", "HEAD"}, r.env)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{r.git, "-C", r.repo, "--git-dir=" + filepath.Join(r.repo, ".git"), "--work-tree=" + r.repo, "update-ref", "refs/heads/raced", "HEAD"}
	if strings.Join(argv, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("prepared %q, want %q", argv, want)
	}
	before := r.state(t)
	if err = os.Remove(sw); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(r.op, sw); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir, cmd.Env = r.scratch, accountedGitExecEnv(r.env)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	r.unchanged(t, before, "")
	r.run(t, r.repo, nil, "rev-parse", "--verify", "refs/heads/raced")

	// The working directory is the one git changes to: through a link,
	// then "..", not the lexical join. A call that named its git directory
	// with --bare, which git takes from the directory before -C, keeps it.
	physical := filepath.Join(r.scratch, "a")
	if err = os.MkdirAll(filepath.Join(physical, "b"), 0700); err != nil {
		t.Fatal(err)
	}
	r.run(t, r.scratch, nil, "init", "-q", "-b", "main", physical)
	r.run(t, physical, nil, "commit", "-q", "--allow-empty", "-m", "a")
	if err = os.Symlink(filepath.Join(physical, "b"), filepath.Join(r.scratch, "ab")); err != nil {
		t.Fatal(err)
	}
	bare := filepath.Join(r.scratch, "bare.git")
	r.run(t, r.scratch, nil, "clone", "-q", "--bare", r.repo, bare)
	for _, c := range []struct {
		cwd  string
		args []string
		want []string
	}{
		{r.scratch, []string{"-C", "ab/..", "update-ref", "refs/heads/up", "HEAD"}, []string{"-C", physical, "--git-dir=" + filepath.Join(physical, ".git"), "--work-tree=" + physical}},
		{bare, []string{"--bare", "-C", r.scratch, "update-ref", "refs/heads/bare", "HEAD"}, []string{"-C", r.scratch, "--bare", "--git-dir=" + bare}},
	} {
		p.CWD = c.cwd
		argv, err := p.PrepareAccounted(append([]string{r.git}, c.args...), r.env)
		want := append(append([]string{r.git}, c.want...), c.args[len(c.args)-3:]...)
		if err != nil || strings.Join(argv, "\x00") != strings.Join(want, "\x00") {
			t.Errorf("%q in %s: prepared %q, want %q, %v", c.args, c.cwd, argv, want, err)
			continue
		}
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Dir, cmd.Env = c.cwd, accountedGitExecEnv(r.env)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("%q: %v %s", argv, err, out)
		}
	}
	r.run(t, physical, nil, "rev-parse", "--verify", "refs/heads/up")
	r.run(t, bare, nil, "rev-parse", "--verify", "refs/heads/bare")
}
