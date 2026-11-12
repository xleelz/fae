package commandshim

import (
	"bytes"
	"errors"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// Account mode's nested git policy: PrepareAccounted's git branch
// (FAE-MODE-002, FAE-EXT-001, FAE-VCS-002, FAE-VCS-010, FAE-GEN-010;
// FAE-HOST-014's accounted branch).
//
// A task worktree is a linked worktree of the operator's repository: it shares
// that repository's refs, objects, configuration and hooks, and the other task
// worktrees are registered in it. A nested git call that ran as called could
// therefore move the operator's branches, rewrite its remotes, install a hook
// or remove another task's worktree. So the policy asks the real git which
// repository the call uses, with the call's own working directory, global
// options and environment, and decides by where that repository is:
//
//   - The call may change a repository whose common and own git directories,
//     object directory, index and work tree all lie in the run's directories
//     (its TMPDIR, the scratch directory, and repositories nested in its
//     worktree) or in a system temporary directory (accountedGitTemporaryRoots),
//     and that is not the run's own repository. Tests keep their repositories
//     there. Its git directories may hold no symbolic link git would write
//     through to anywhere else, and its references no store elsewhere
//     (checkRepositoryLinks): a test repository whose .git/refs or
//     .git/worktrees links into the operator's could otherwise move the
//     operator's branches or remove another task's worktree.
//   - Every other repository is protected: the run's own (the operator's,
//     with its task worktrees) and any repository elsewhere on disk, such as
//     the operator's checkout a self-hosting clone was made from. A call there
//     may only read (accountedGitReadOnly), writes no file (--output), sets
//     no configuration but a short harmless list (-c, --config-env and
//     GIT_CONFIG_COUNT settings outside accountedGitReadConfig, GIT_CONFIG,
//     and GIT_CONFIG_GLOBAL or GIT_CONFIG_SYSTEM other than /dev/null are
//     refused), and runs with --no-optional-locks, so even git status leaves
//     the index alone. A Local task's remote change there exits 78; every
//     other refusal exits 77.
//   - git init and git clone must create their repository in the run's
//     directories or a temporary directory, and init may not reinitialize a
//     protected repository (which would copy hooks into it); a config write to
//     a named file (--file, --global, --system) must write there too, and so
//     must every file a call names for git to write (writeTargets:
//     --output, format-patch -o, archive -o, bundle create, ...; and
//     format-patch's format.outputDirectory, checkFormatPatchDirectory).
//     Every path a call names is checked as the system takes it when git
//     creates and writes it: links followed, a directory git would create
//     counted as one, a ".." leading up from where the previous component
//     lies, a link leading nowhere refused (accountedGitResolve).
//   - Everywhere: a push or remote change is refused (78 for a Local task, 77
//     otherwise), as are git's mail, foreign-VCS and server commands, and any
//     subcommand it does not know, which covers aliases and git-* programs on
//     PATH. So is whatever the call itself gives git that names a program for
//     git to run: options and subcommands (submodule foreach, rebase --exec,
//     bisect run, --upload-pack, ls-remote --exec, init and clone --template,
//     a merge strategy git does not have, which runs git-merge-NAME from
//     PATH), settings (-c, --config-env, GIT_CONFIG_COUNT and clone --config
//     keys such as core.sshCommand, core.hooksPath, credential.helper,
//     filter.*, *.command and *.textconv, any hook.* setting but one that
//     switches a hook off, and include.path, which loads a file of them;
//     accountedGitChosenSetting), the same settings in a global
//     or system file it names (refuseNamedConfigFiles) or written by a nested
//     git config (refuseProgram), and environment variables (GIT_SSH_COMMAND,
//     GIT_EDITOR, ...; accountedGitProgramEnv). A GIT_* variable the policy
//     does not know is refused (accountedGitKnownEnv: an allowlist, since
//     GIT_TEST_FSMONITOR runs a program on every git status). So are
//     GIT_CONFIG_PARAMETERS, --exec-path=DIR, an argument naming a path
//     inside the run's own git directory, a GIT_TRACE* file outside the
//     directories a call may write or a trace socket, and a repository named
//     by a URL off this host (accountedGitOffHost).
//
// Every call that passes runs the real git with accountedGitExecEnv:
// GIT_CONFIG_GLOBAL=/dev/null and GIT_CONFIG_NOSYSTEM=1 unless the call names
// those files itself, so HOME and XDG_CONFIG_HOME choose no configuration;
// GIT_ALLOW_PROTOCOL=file, so git reaches no repository off this host whatever
// a remote's URL or a url.<base>.insteadOf says; GIT_TRACE2, GIT_TRACE2_PERF
// and GIT_TRACE2_EVENT 0 unless the call sets them, so no configuration file
// sends git's trace anywhere; and, as command-line settings that outrank
// every file, no hook (core.hooksPath=/dev/null and hook.<event>.enabled=false
// for every event; git reads the call's own -c and --config-env after them,
// which is why the call's hook settings are refused above), no file-system
// monitor and no bundle URIs, which git downloads over HTTP past its protocol
// list. A call on a repository runs
// pinned to it (pinned): -C its canonical working directory and its
// canonical git directory and work tree, as the policy found them, so a
// symbolic link the call named, changed after the check, cannot point it at
// another repository. The rev-parse the policy runs to find a call's
// repository loads no global or system configuration either (locate).
//
// The boundary. Programs a test's own repository's configuration names when
// the test wrote it there itself (filter and diff drivers, an editor, a file
// remote's upload-pack, gpg) still run, as the test set them up; so does a
// protected repository's own configured program (a diff driver), on a read.
// Programs git itself starts run the real git, since git puts its own
// directory first on their PATH, and never reach the wrappers; they keep
// GIT_ALLOW_PROTOCOL. A test's submodules are not checked (a submodule's .git
// file can name another repository's git directory), nor is a file in a
// test repository's .git hard-linked to one of the operator's (git appends to
// a reflog in place). The checks and the exec are separate steps: a
// concurrent process that changes the repository between them (renames a
// directory on the canonical path, plants a link in its .git, rewrites its
// configuration or a named configuration file) is not seen. Like every
// account-mode policy, this is evidence and guidance, not containment: the
// command can run /usr/bin/git itself.

// accountedGitTemporaryRoots are the system temporary directories in which a
// nested git call may change repositories, besides the run's own directories.
var accountedGitTemporaryRoots = []string{"/tmp", "/var/tmp", "/dev/shm"}

// accountedGitCommands are the git subcommands a nested call may run. Every
// other name is refused: an alias or a git-* program on PATH runs what the
// wrappers never see.
var accountedGitCommands = func() map[string]bool {
	known := map[string]bool{}
	for _, name := range strings.Fields(`add am annotate apply archive backfill bisect blame branch bundle cat-file
		check-attr check-ignore check-mailmap check-ref-format checkout checkout-index cherry cherry-pick clean
		clone column commit commit-graph commit-tree config count-objects describe diff diff-files diff-index
		diff-pairs diff-tree fast-export fast-import fetch fmt-merge-msg for-each-ref format-patch format-rev
		fsck fsck-objects gc get-tar-commit-id grep hash-object help history index-pack init init-db
		interpret-trailers last-modified log ls-files ls-remote ls-tree mailinfo mailsplit maintenance merge
		merge-base merge-file merge-tree mktag mktree multi-pack-index mv name-rev notes pack-objects
		pack-redundant pack-refs patch-id pickaxe prune prune-packed pull range-diff read-tree rebase reflog
		refs repack replace replay repo rerere reset restore rev-list rev-parse revert rm shortlog show
		show-branch show-index show-ref sparse-checkout stage stash status stripspace submodule switch
		symbolic-ref tag unpack-file unpack-objects update-index update-ref update-server-info url-parse var
		verify-commit verify-pack verify-tag version whatchanged worktree write-tree`) {
		known[name] = true
	}
	return known
}()

// accountedGitRefused names, for git commands that are refused everywhere,
// what they do.
var accountedGitRefused = map[string]string{
	"imap-send": "sends mail", "send-email": "sends mail", "request-pull": "asks someone to pull",
	"svn": "talks to a foreign version-control system", "p4": "talks to a foreign version-control system",
	"cvsexportcommit": "talks to a foreign version-control system", "cvsimport": "talks to a foreign version-control system",
	"cvsserver": "serves repositories", "archimport": "talks to a foreign version-control system",
	"quiltimport": "runs a script of its own", "daemon": "serves repositories", "instaweb": "serves repositories",
	"http-backend": "serves repositories", "upload-pack": "serves repositories", "upload-archive": "serves repositories",
	"receive-pack": "writes the repository a push names", "shell": "serves repositories",
	"http-fetch": "speaks a transport protocol directly", "fetch-pack": "speaks a transport protocol directly",
	"credential": "talks to credential helpers", "credential-cache": "talks to credential helpers",
	"credential-store": "talks to credential helpers", "subtree": "pushes from a script of its own",
	"difftool": "runs a program of its own", "mergetool": "runs a program of its own",
	"filter-branch": "runs a program of its own", "for-each-repo": "runs git in other repositories",
	"hook": "runs a hook", "merge-index": "runs a program of its own", "merge-one-file": "runs a script of its own",
	"citool": "starts a GUI", "gui": "starts a GUI", "gitk": "starts a GUI", "bugreport": "collects host details",
	"diagnose": "collects host details",
}

// accountedGitCall is a nested git call split at its subcommand.
type accountedGitCall struct {
	dir      string      // the working directory after the call's -C options
	replay   []string    // the global options that choose the repository or set configuration, in order
	keep     []string    // the global options other than -C, --git-dir and --work-tree, as given (pinned)
	config   [][2]string // -c and --config-env settings: key and value
	gitDir   string      // --git-dir, relative to dir
	workTree string      // --work-tree, relative to dir
	bare     bool
	execPath bool // --exec-path=DIR
	sub      string
	args     []string
}

// accountedGitFlags are git's global options that take no value.
var accountedGitFlags = map[string]bool{
	"-p": true, "--paginate": true, "-P": true, "--no-pager": true, "--no-replace-objects": true,
	"--no-lazy-fetch": true, "--no-optional-locks": true, "--no-advice": true, "--bare": true,
	"--literal-pathspecs": true, "--glob-pathspecs": true, "--noglob-pathspecs": true, "--icase-pathspecs": true,
	"--html-path": true, "--man-path": true, "--info-path": true, "--exec-path": true,
	"-v": true, "--version": true, "-h": true, "--help": true,
}

// accountedGitAttached are git's global options that take an =VALUE.
var accountedGitAttached = map[string]bool{
	"--git-dir": true, "--work-tree": true, "--namespace": true, "--config-env": true, "--attr-source": true,
	"--super-prefix": true, "--exec-path": true, "--list-cmds": true,
}

// parseAccountedGit splits a git call's arguments (after the program) into its
// global options, subcommand and the subcommand's arguments. An option git
// does not have is refused, so none is mistaken for a subcommand's.
func parseAccountedGit(cwd string, argv, env []string) (accountedGitCall, error) {
	c := accountedGitCall{dir: cwd}
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if !strings.HasPrefix(a, "-") {
			c.sub, c.args = a, argv[i+1:]
			return c, nil
		}
		name, value, attached := a, "", false
		if strings.HasPrefix(a, "--") {
			name, value, attached = strings.Cut(a, "=")
		}
		given := []string{a}
		switch {
		case attached && accountedGitAttached[name]:
		case !attached && gitValueOptions[a]:
			if i+1 == len(argv) {
				return c, nil // git refuses the missing value itself
			}
			i++
			value = argv[i]
			given = append(given, value)
		case !attached && accountedGitFlags[a]:
			if a == "--bare" {
				c.bare, c.replay = true, append(c.replay, a)
			}
			c.keep = append(c.keep, a)
			continue
		default:
			return c, refuse("nested git option " + accountedGitQuote(a) + " is not one the nested-command policy knows")
		}
		if name != "-C" && name != "--git-dir" && name != "--work-tree" {
			c.keep = append(c.keep, given...)
		}
		switch name {
		case "-C":
			c.replay = append(c.replay, "-C", value)
			if value != "" {
				c.dir = accountedGitChdir(c.dir, value)
			}
		case "-c":
			c.replay = append(c.replay, "-c", value)
			key, v, set := strings.Cut(value, "=")
			if !set {
				v = "true" // git -c NAME sets a boolean
			}
			c.config = append(c.config, [2]string{key, v})
		case "--config-env":
			c.replay = append(c.replay, "--config-env="+value)
			if at := strings.LastIndex(value, "="); at >= 0 {
				c.config = append(c.config, [2]string{value[:at], accountedGitEnv(env, value[at+1:])})
			}
		case "--git-dir":
			c.gitDir, c.replay = value, append(c.replay, "--git-dir="+value)
		case "--work-tree":
			c.workTree, c.replay = value, append(c.replay, "--work-tree="+value)
		case "--exec-path":
			c.execPath = true
		}
	}
	return c, nil
}

// prepareAccountedGit is PrepareAccounted's git branch; args[0] is the real
// git. p's paths are canonical.
func (p Policy) prepareAccountedGit(args, env []string) ([]string, error) {
	c, err := parseAccountedGit(p.CWD, args[1:], env)
	if err != nil {
		return nil, err
	}
	sub := c.sub
	if gitExternalSubcommands[sub] {
		if p.Local {
			return nil, &LocalRefusal{"git " + sub + ": a Local task makes no external effect, so pushes and remote changes are refused"}
		}
		return nil, refuse("nested git " + sub + ": an engine run makes no external effect outside the daemon's effect grants, so pushes and remote changes are refused")
	}
	if what, ok := accountedGitRefused[sub]; ok {
		return nil, refuse("nested git " + sub + " " + what + "; nested calls make no external effect and start no program git runs past the wrappers")
	}
	if sub != "" && !accountedGitCommands[sub] {
		return nil, refuse("nested git " + accountedGitQuote(sub) + " is not a git command the nested-command policy knows: git aliases and git-* programs run past the wrappers")
	}
	if c.execPath {
		return nil, refuse("nested git --exec-path=DIR runs git's helper programs from DIR")
	}
	if what := c.runsProgram(); what != "" {
		return nil, refuse("nested git " + sub + " " + what + ": a program git starts runs the real git, past the wrappers, so nested calls start none")
	}
	if err = c.refuseChosen(env); err != nil {
		return nil, err
	}
	if what := c.changesHost(); what != "" {
		return nil, refuse("nested git maintenance " + what + " schedules jobs on the host or writes the user's git configuration, outside the run")
	}
	if url := c.offHost(); url != "" {
		return nil, refuse("nested git " + sub + " names " + accountedGitQuote(url) + ", a repository off this host: nested calls reach only local repositories (paths and file:// URLs)")
	}
	targets, what := c.writeTargets()
	if what != "" {
		return nil, refuse("nested git " + sub + " " + what)
	}
	none := sub == "" || sub == "version" || sub == "help"
	if none && accountedGitTraceTarget(env) == "" {
		return args, nil // no repository
	}
	g, err := p.accountedGit(args[0], env)
	if err != nil {
		return nil, err
	}
	if err = g.refuseTraceTargets(); err != nil || none {
		return accountedGitPrepared(args, err)
	}
	if err = g.refuseNamedConfigFiles(c); err != nil {
		return nil, err
	}
	switch sub {
	case "init", "init-db", "clone":
		if err = g.refuseRunGitDirArgument(c); err == nil {
			err = g.checkCreate(c)
		}
		return accountedGitPrepared(args, err)
	case "config":
		if cfg := parseAccountedGitConfig(c.args); cfg.write {
			if files := cfg.files(env); len(files) > 0 {
				if err = g.checkConfigFiles(c, cfg, files); err == nil {
					if err = g.refuseRunGitDirArgument(c); err == nil {
						err = cfg.refuseProgram()
					}
				}
				return accountedGitPrepared(args, err)
			}
		}
	}
	r, err := g.locate(p.CWD, c.replay, env)
	if err != nil {
		return nil, err
	}
	protected := false
	if r != nil {
		if protected, err = g.protects(r); err != nil {
			return nil, err
		}
	}
	if protected {
		if err = g.readOnly(c, r.common); err != nil {
			return nil, err
		}
		return c.pinned(args[0], r, env, true)
	}
	// A repository the call may change, or none: git fails on its own or
	// needs none (diff --no-index, ls-remote).
	if err = g.refuseRunGitDirArgument(c); err != nil {
		return nil, err
	}
	if err = g.checkWorktreePaths(c, r); err != nil {
		return nil, err
	}
	if err = g.refuseWriteTargets(c, targets, r); err != nil {
		return nil, err
	}
	if cfg := parseAccountedGitConfig(c.args); sub == "config" && cfg.write {
		if err = cfg.refuseProgram(); err != nil {
			return nil, err
		}
	}
	if r == nil {
		return args, nil
	}
	if err = g.checkRepositoryLinks(r); err != nil {
		return nil, err
	}
	if err = g.checkFormatPatchDirectory(c, r); err != nil {
		return nil, err
	}
	return c.pinned(args[0], r, env, false)
}

// pinned is the argv a call on repository r runs with: its working
// directory, git directory and work tree as the policy resolved them
// (canonical, so without symbolic links), in place of the -C, --git-dir and
// --work-tree it gave, after its other global options; readOnly puts
// --no-optional-locks first, so even git status leaves the index alone. A
// symbolic link changed after the check therefore cannot point the call at
// another repository. A call with no work tree (in a bare repository or
// inside a git directory) keeps git's own discovery from its canonical
// working directory unless it named its git directory (--git-dir, --bare,
// GIT_DIR): git would otherwise take that directory as the work tree.
func (c accountedGitCall) pinned(git string, r *accountedGitRepository, env []string, readOnly bool) ([]string, error) {
	globals, err := c.pinnedGlobals(r, env)
	if err != nil {
		return nil, err
	}
	argv := []string{git}
	if readOnly {
		argv = append(argv, "--no-optional-locks")
	}
	return append(append(append(argv, globals...), c.sub), c.args...), nil
}

// pinnedGlobals are pinned's global options: -C the canonical working
// directory, the call's other global options, then the canonical git
// directory and work tree.
func (c accountedGitCall) pinnedGlobals(r *accountedGitRepository, env []string) ([]string, error) {
	dir, err := filepath.EvalSymlinks(c.dir) // git just ran there (locate)
	if err != nil {
		return nil, err
	}
	globals := append([]string{"-C", dir}, c.keep...)
	switch {
	case r.top != "":
		globals = append(globals, "--git-dir="+r.gitDir, "--work-tree="+r.top)
	case c.gitDir != "" || c.bare || accountedGitEnv(env, "GIT_DIR") != "":
		globals = append(globals, "--git-dir="+r.gitDir)
	}
	return globals, nil
}

// accountedGitPrepared is args, or nil when err refuses them.
func accountedGitPrepared(args []string, err error) ([]string, error) {
	if err != nil {
		return nil, err
	}
	return args, nil
}

// accountedGit decides a nested git call against the run's directories.
type accountedGit struct {
	p     Policy
	git   string   // the real git
	env   []string // the call's environment
	clean []string // the call's environment without git's variables
	run   string   // the run's repository's common directory, "" when its worktree is in none
	roots []string // canonical directories whose repositories a call may change
}

func (p Policy) accountedGit(git string, env []string) (*accountedGit, error) {
	g := &accountedGit{p: p, git: git, env: env}
	for _, kv := range env {
		if !strings.HasPrefix(kv, "GIT_") {
			g.clean = append(g.clean, kv)
		}
	}
	run, err := g.locate(p.Worktree, nil, g.clean)
	if err != nil {
		return nil, err
	}
	if run != nil {
		g.run = run.common
	}
	for _, root := range append([]string{p.Scratch, p.Worktree}, accountedGitTemporaryRoots...) {
		if root == "" {
			continue
		}
		if canonical, err := filepath.EvalSymlinks(root); err == nil {
			g.roots = append(g.roots, canonical)
		}
	}
	return g, nil
}

// writable reports whether a nested call may change what is at the canonical
// path x: inside the run's directories or a temporary root, and not inside the
// run's own repository's git directory.
func (g *accountedGit) writable(x string) bool {
	if g.run != "" && inside(g.run, x) {
		return false
	}
	for _, root := range g.roots {
		if inside(root, x) {
			return true
		}
	}
	return false
}

// accountedGitRepository is a repository as git locates it for a call: its common and
// own git directories, object directory, index and work tree ("" if none).
type accountedGitRepository struct{ common, gitDir, objects, index, top string }

// locate asks the real git which repository a call run in dir with these
// global options and environment uses; nil when there is none. The rev-parse
// loads no global or system configuration, whatever HOME, XDG_CONFIG_HOME or
// the call's GIT_CONFIG_GLOBAL says (none of them moves a repository), and
// the call's -c settings reach it only after refuseChosen.
func (g *accountedGit) locate(dir string, replay, env []string) (*accountedGitRepository, error) {
	argv := append(append([]string(nil), replay...), "rev-parse", "--path-format=absolute", "--git-common-dir", "--absolute-git-dir", "--git-path", "objects", "--git-path", "index", "--show-toplevel")
	cmd := exec.Command(g.git, argv...)
	cmd.Dir = dir
	for _, kv := range env {
		if !strings.HasPrefix(kv, "LANGUAGE=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	// exec.Cmd passes the last value of a key.
	cmd.Env = append(cmd.Env, "LC_ALL=C", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_ALLOW_PROTOCOL=file")
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	var exit *exec.ExitError
	fatal := errors.As(err, &exit) && exit.ExitCode() == 128
	switch msg := errOut.String(); {
	case err == nil && len(lines) == 5:
	case fatal && len(lines) == 4 && strings.Contains(msg, "must be run in a work tree"):
		lines = append(lines, "")
	case fatal && (strings.Contains(msg, "not a git repository") || strings.Contains(msg, "cannot change to")):
		return nil, nil
	default:
		if msg == "" && err != nil {
			msg = err.Error()
		}
		first, _, _ := strings.Cut(strings.TrimSpace(msg), "\n")
		return nil, refuse("nested git: cannot tell which repository the call uses (" + accountedGitQuote(first) + "), so it is refused")
	}
	paths := make([]string, len(lines))
	for i, line := range lines {
		if line == "" {
			continue
		}
		if paths[i], err = accountedGitResolve("/", line); err != nil {
			return nil, err
		}
	}
	return &accountedGitRepository{paths[0], paths[1], paths[2], paths[3], paths[4]}, nil
}

// protects reports whether a nested call may only read repository r: any of
// its directories lies outside the writable ones, or its work tree belongs to
// the run's own repository while its git directory does not.
func (g *accountedGit) protects(r *accountedGitRepository) (bool, error) {
	for _, x := range []string{r.common, r.gitDir, r.objects, r.index, r.top} {
		if x != "" && !g.writable(x) {
			return true, nil
		}
	}
	if r.top == "" || g.run == "" || filepath.Join(r.top, ".git") == r.gitDir {
		return false, nil
	}
	owner, err := g.locate(r.top, nil, g.clean)
	if err != nil {
		return true, err
	}
	return owner != nil && owner.common == g.run, nil
}

// accountedGitWalkLimit bounds the entries checkRepositoryLinks reads in one
// repository's git directories.
var accountedGitWalkLimit = 20000

// checkRepositoryLinks checks a repository a nested call may change for the
// links out of it that git writes through, where the policy's own rev-parse
// does not look (locate resolves only the git, common and object
// directories, the index and the work tree):
//   - a symbolic link anywhere in its git directories (common and own) or
//     object directory to a place the call may not change: refs,
//     packed-refs, logs, worktrees, config, hooks, info, modules or
//     objects/pack linked into the operator's repository would let update-ref
//     move its branches, worktree remove delete another task's worktree or
//     gc --prune=now delete its objects;
//   - a reference store its configuration keeps elsewhere (extensions.
//     refStorage with a path, which git reads from the common directory's
//     config file alone), which must lie where the call may write and is
//     checked the same way.
//
// Loose objects and packs are not read: git only adds them as new files, and
// removing one removes a link, not what it points to. Objects a repository
// borrows (objects/info/alternates, GIT_ALTERNATE_OBJECT_DIRECTORIES) are
// only read. Not checked: a file there hard-linked to one of the operator's
// (git appends to a reflog and rewrites FETCH_HEAD and its message files in
// place, so such a link reaches the operator's), and a test's submodules (a
// submodule's .git file the test points at another repository's git
// directory is followed).
func (g *accountedGit) checkRepositoryLinks(r *accountedGitRepository) error {
	roots := []string{r.common}
	objects := []bool{false}
	for i, x := range []string{r.gitDir, r.objects} {
		if x != "" && !inside(r.common, x) {
			roots, objects = append(roots, x), append(objects, i == 1)
		}
	}
	stores, err := g.refStorage(r)
	if err != nil {
		return err
	}
	for _, x := range stores {
		if !g.writable(x) {
			return refuse("nested git: the repository at " + r.common + " keeps its references at " + x + " (extensions.refStorage), outside the run's directories and the temporary ones or in the run's own repository")
		}
		roots, objects = append(roots, x), append(objects, false)
	}
	w := accountedGitWalk{g: g, repo: r.common, seen: map[string]bool{}, budget: accountedGitWalkLimit}
	for i, root := range roots {
		if err = w.dir(root, objects[i]); err != nil {
			return err
		}
	}
	return nil
}

// accountedGitWalk is one checkRepositoryLinks walk.
type accountedGitWalk struct {
	g      *accountedGit
	repo   string
	seen   map[string]bool
	budget int
}

// dir checks the entries of the git directory dir, and those of every
// directory under it or linked from it, once each; objects marks an object
// directory, whose loose-object and pack directories are skipped.
func (w *accountedGitWalk) dir(dir string, objects bool) error {
	if w.seen[dir] {
		return nil
	}
	w.seen[dir] = true
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if w.budget--; w.budget < 0 {
			return refuse("nested git: the repository at " + w.repo + " has more than " + strconv.Itoa(accountedGitWalkLimit) + " files besides its objects to check for links out of it, so the call is refused")
		}
		name, p := e.Name(), filepath.Join(dir, e.Name())
		skip := objects && (name == "pack" || len(name) == 2 && strings.Trim(name, "0123456789abcdef") == "")
		sub := name == "objects" && !objects
		if e.Type()&fs.ModeSymlink != 0 {
			x, err := accountedGitResolve("/", p)
			if err != nil {
				return err
			}
			if !w.g.writable(x) {
				return refuse("nested git: the repository at " + w.repo + " links " + p + " to " + x + ", outside the run's directories and the temporary ones or in the run's own repository, and git writes through such a link")
			}
			if st, err := os.Stat(x); err == nil && st.IsDir() && !skip {
				if err = w.dir(x, sub); err != nil {
					return err
				}
			}
			continue
		}
		if e.IsDir() && !skip {
			if err = w.dir(p, sub); err != nil {
				return err
			}
		}
	}
	return nil
}

// refStorage are the directories repository r's configuration keeps its
// references in instead of its common directory: the path of
// extensions.refStorage (FORMAT://PATH), which git reads from the common
// directory's config file alone. A relative path is resolved against both
// the git and the common directory.
func (g *accountedGit) refStorage(r *accountedGitRepository) ([]string, error) {
	config := filepath.Join(r.common, "config")
	raw, err := os.ReadFile(config)
	if errors.Is(err, fs.ErrNotExist) || err == nil && !bytes.Contains(bytes.ToLower(raw), []byte("refstorage")) {
		return nil, nil // git never escapes or splits a variable's name
	}
	cmd := exec.Command(g.git, "config", "--file", config, "--no-includes", "-z", "--get-all", "extensions.refstorage")
	cmd.Dir, cmd.Env = "/", append(append([]string(nil), g.clean...), "LC_ALL=C", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return nil, nil // not set
	}
	if err != nil {
		// Reached when the configuration changed since locate read it, or git
		// failed: where the repository keeps its references cannot be told.
		return nil, refuse("nested git: cannot read extensions.refStorage of the repository at " + r.common + " (" + accountedGitFirstLine(err) + "), so where it keeps its references cannot be told and the call is refused")
	}
	var dirs []string
	for _, v := range strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00") {
		_, path, found := strings.Cut(v, "://")
		if !found || path == "" {
			continue // a format alone: the common directory
		}
		for _, base := range []string{r.gitDir, r.common} {
			x, err := accountedGitResolve(base, path)
			if err != nil {
				return nil, err
			}
			dirs = append(dirs, x)
		}
	}
	return dirs, nil
}

// checkFormatPatchDirectory checks where git format-patch writes its patches
// when the call names no directory, file or --stdout: format.outputDirectory
// from the repository's configuration or the call's own, read as the call
// will run, which must lie where the call may write.
func (g *accountedGit) checkFormatPatchDirectory(c accountedGitCall, r *accountedGitRepository) error {
	if c.sub != "format-patch" {
		return nil
	}
	for _, a := range c.args {
		if a == "--" {
			break
		}
		if strings.HasPrefix(a, "--") && accountedGitLong(a, "stdout", "output", "output-directory") || accountedGitHasShort([]string{a}, "o", "vGSOl") {
			return nil // writeTargets has the call's own
		}
	}
	globals, err := c.pinnedGlobals(r, g.env)
	if err != nil {
		return err
	}
	cmd := exec.Command(g.git, append(globals, "config", "--get", "format.outputDirectory")...)
	cmd.Env = append(accountedGitExecEnv(g.env), "LC_ALL=C")
	out, err := cmd.Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return nil // not set: the working directory
	}
	if err != nil {
		// Reached when the configuration changed since locate read it, or git
		// failed: where the patches would go cannot be told.
		return refuse("nested git format-patch: cannot read format.outputDirectory (" + accountedGitFirstLine(err) + "), so where its patches would go cannot be told and the call is refused")
	}
	dir := strings.TrimSuffix(string(out), "\n")
	bases := []string{c.dir}
	if r.top != "" && !filepath.IsAbs(dir) {
		bases = append(bases, r.top)
	}
	for _, base := range bases {
		x, err := accountedGitResolve(base, dir)
		if err != nil {
			return err
		}
		if !g.writable(x) {
			return refuse("nested git format-patch would write its patches to " + x + " (format.outputDirectory), outside the run's directories and the temporary ones, or in the run's own repository")
		}
	}
	return nil
}

// refuseNamedConfigFiles checks a global or system configuration file the
// call names (GIT_CONFIG_GLOBAL, GIT_CONFIG_SYSTEM, other than /dev/null) as
// it checks the call's -c settings: the file is configuration the call
// chose, so no setting in it may name a program for git to run, a file of
// configuration to load or bundles to download (accountedGitChosenSetting).
// A relative name is refused, since git reads it from whatever directory it
// is in; a missing file is empty, as git reads it, and a system file is not
// read under GIT_CONFIG_NOSYSTEM. (On a protected repository readOnly
// refuses any named file.)
func (g *accountedGit) refuseNamedConfigFiles(c accountedGitCall) error {
	noSystem := accountedGitEnv(g.env, "GIT_CONFIG_NOSYSTEM")
	for _, name := range []string{"GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM"} {
		f := accountedGitEnv(g.env, name)
		if f == "" || f == "/dev/null" || name == "GIT_CONFIG_SYSTEM" && accountedGitBool(noSystem) && !accountedGitFalse(noSystem) {
			continue // git reads no system file
		}
		if !filepath.IsAbs(f) {
			return refuse("nested git with " + name + " naming " + accountedGitQuote(f) + ", a relative path git reads from whatever directory it is in; nested calls name their configuration file by an absolute path")
		}
		if _, err := os.Stat(f); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		cmd := exec.Command(g.git, "config", "--file", f, "--no-includes", "-z", "--list")
		cmd.Dir, cmd.Env = "/", append(append([]string(nil), g.clean...), "LC_ALL=C", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		out, err := cmd.Output()
		if err != nil {
			return refuse("nested git with " + name + " naming " + accountedGitQuote(f) + ", which git cannot read as configuration, so it is refused")
		}
		for _, entry := range strings.Split(string(out), "\x00") {
			if entry == "" {
				continue
			}
			key, value, set := strings.Cut(entry, "\n")
			if !set {
				value = "true" // a key alone is a boolean true
			}
			if what := accountedGitChosenSetting(key, value); what != "" {
				return refuse("nested git with " + name + " naming " + accountedGitQuote(f) + ", whose " + accountedGitQuote(key) + " " + what + "; nested calls choose none, on any repository")
			}
		}
	}
	return nil
}

// refuseRunGitDirArgument refuses an argument, or an option's value, that
// names a path inside the run's own git directory: a call on another
// repository can still write a file there (bundle create, --output, a
// worktree's directory).
func (g *accountedGit) refuseRunGitDirArgument(c accountedGitCall) error {
	if g.run == "" {
		return nil
	}
	for _, a := range c.args {
		candidates := []string{a}
		if _, value, ok := strings.Cut(a, "="); ok && strings.HasPrefix(a, "-") {
			candidates = append(candidates, value)
		} else if len(a) > 2 && a[0] == '-' && a[1] != '-' {
			candidates = append(candidates, a[2:])
		}
		for _, x := range candidates {
			x = strings.TrimPrefix(x, "file://")
			if x == "" || x == "-" {
				continue
			}
			if path, err := accountedGitResolve(c.dir, x); err == nil && inside(g.run, path) {
				return refuse("nested git " + c.sub + " names " + path + ", inside the run's own repository's git directory, which nested calls never change")
			}
		}
	}
	return nil
}

// checkCreate checks git init and git clone: the repository they create, or
// that init reinitializes, must be one a nested call may change.
func (g *accountedGit) checkCreate(c accountedGitCall) error {
	long := []string{"template", "separate-git-dir", "initial-branch", "object-format", "ref-format"}
	short := "b"
	if c.sub == "clone" {
		long, short = accountedGitCloneValues, "objuc"
	}
	positional, values := accountedGitPositional(c.args, long, short)
	dest := "."
	if c.sub == "clone" && len(positional) > 1 {
		dest = positional[1] // without one, clone creates its directory inside dir
	} else if c.sub != "clone" && len(positional) > 0 {
		dest = positional[0]
	}
	targets := append([]string{dest}, values["separate-git-dir"]...)
	targets = append(targets, c.gitDir, c.workTree)
	for _, name := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_OBJECT_DIRECTORY", "GIT_INDEX_FILE"} {
		targets = append(targets, accountedGitEnv(g.env, name))
	}
	for _, t := range targets {
		if t == "" {
			continue
		}
		x, err := accountedGitResolve(c.dir, t)
		if err != nil {
			return err
		}
		if !g.writable(x) {
			return refuse("nested git " + c.sub + " would create or reinitialize a repository at " + x + ", outside the run's directories and the temporary ones, or in the run's own repository")
		}
	}
	if c.sub == "clone" {
		return nil
	}
	// init in an existing work tree or git directory reinitializes that
	// repository, copying its template's hooks into it.
	dir, err := accountedGitResolve(c.dir, dest)
	if err != nil {
		return err
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return nil // a new directory: a new repository
	}
	r, err := g.locate(dir, nil, g.clean)
	if err != nil || r == nil || r.top != dir && r.gitDir != dir {
		return err // none, or one enclosing dir: init creates a new one
	}
	if protected, err := g.protects(r); err != nil || !protected {
		return err
	}
	return refuse("nested git init would reinitialize the repository at " + r.common + ", which nested calls may only read")
}

// checkConfigFiles checks a git config write to named files: each must be one
// a nested call may change.
func (g *accountedGit) checkConfigFiles(c accountedGitCall, cfg accountedGitConfig, files []string) error {
	for _, f := range files {
		x, err := accountedGitResolve(c.dir, f)
		if err != nil {
			return err
		}
		if !g.writable(x) {
			if g.p.Local && cfg.remote() {
				return &LocalRefusal{"git config " + cfg.key + ", written to " + x + ", changes a remote; a Local task makes no external effect, so pushes and remote changes are refused"}
			}
			return refuse("nested git config would write " + x + ", outside the run's directories and the temporary ones, or in the run's own repository")
		}
	}
	return nil
}

// checkWorktreePaths checks git worktree on repository r, which a nested
// call may change (nil: none): add and move must put the worktree where the
// call may write, and the worktrees that remove deletes, move moves and
// repair rewrites must lie there too (a repository's worktree list, or a
// worktree's .git file, can name any directory). repair rewrites more than
// the paths it names: "." when it names none, and always the .git file of
// every worktree r lists (accountedGitListedWorktrees).
func (g *accountedGit) checkWorktreePaths(c accountedGitCall, r *accountedGitRepository) error {
	if c.sub != "worktree" || len(c.args) == 0 {
		return nil
	}
	positional, _ := accountedGitPositional(c.args[1:], []string{"reason"}, "bB")
	var paths []string
	switch c.args[0] {
	case "add":
		paths = positional[:min(len(positional), 1)]
	case "move":
		paths = positional[:min(len(positional), 2)]
	case "remove":
		paths = positional[:min(len(positional), 1)]
	case "repair":
		paths = positional
		if len(paths) == 0 {
			paths = []string{"."}
		}
		if r != nil {
			listed, err := accountedGitListedWorktrees(r.common)
			if err != nil {
				return err
			}
			paths = append(paths, listed...)
		}
	}
	for i, p := range paths {
		x, err := accountedGitResolve(c.dir, p)
		if err != nil {
			return err
		}
		owned := false
		existing := c.args[0] != "add" && (c.args[0] != "move" || i == 0)
		if st, err := os.Stat(x); err == nil && st.IsDir() && existing && g.run != "" {
			// Another task's worktree belongs to the run's own repository.
			if owned, err = g.runOwnsWorktree(x); err != nil {
				return err
			}
		}
		if owned || !g.writable(x) {
			return refuse("nested git worktree " + c.args[0] + " would change the worktree at " + x + ", outside the run's directories and the temporary ones, or one of the run's own repository")
		}
	}
	return nil
}

// runOwnsWorktree reports whether the existing directory x is a work tree of
// the run's own repository: git locates that repository from it, or, when
// git can locate no repository from x because its .git file names no valid
// one (the worktree git worktree repair mends), that .git file names a git
// directory inside the run's.
func (g *accountedGit) runOwnsWorktree(x string) (bool, error) {
	owner, err := g.locate(x, nil, g.clean)
	var refused *Refusal
	if err == nil || !errors.As(err, &refused) {
		return owner != nil && owner.common == g.run, err
	}
	raw, readErr := os.ReadFile(filepath.Join(x, ".git"))
	named, gitfile := strings.CutPrefix(string(raw), "gitdir: ")
	if readErr != nil || !gitfile {
		return false, err // not a broken worktree: git's own reason stands
	}
	named = strings.TrimRight(named, " \t\n\v\f\r")
	if !filepath.IsAbs(named) {
		named = x + string(filepath.Separator) + named
	}
	at, err := accountedGitResolve("/", named)
	if err != nil {
		return false, err
	}
	return inside(g.run, at), nil
}

// accountedGitListedWorktrees are the linked worktrees the repository whose
// common directory is common lists, as git finds them: each
// <common>/worktrees/<id>/gitdir names the worktree's .git file, absolute or
// relative to <id>'s directory, and git skips an entry whose gitdir it cannot
// read. git worktree repair rewrites the .git file of every one that exists
// (repair_worktrees), whatever paths the call names; a test can write such an
// entry naming another task's worktree. The paths are as git reads them;
// checkWorktreePaths resolves them as the system does (accountedGitResolve).
func accountedGitListedWorktrees(common string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(common, "worktrees"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, e := range entries {
		admin := filepath.Join(common, "worktrees", e.Name())
		raw, err := os.ReadFile(filepath.Join(admin, "gitdir"))
		if err != nil {
			continue
		}
		p := strings.TrimSuffix(strings.TrimRight(string(raw), " \t\n\v\f\r"), "/.git")
		if p == "" {
			continue
		}
		if !filepath.IsAbs(p) {
			p = admin + string(filepath.Separator) + p
		}
		paths = append(paths, p)
	}
	return paths, nil
}

// accountedGitResolve is the canonical path that p, relative to dir, names as
// the system takes it when git creates and writes it, component by
// component: an existing component is followed through its symbolic links; a
// component that does not exist yet is the directory git creates there
// (mkdir -p), or the file it writes; and a ".." leads up from where the
// previous component lies, from where a link points or from the real parent
// of a directory git would create, as open, mkdir and chdir take it. A
// component that exists but cannot be followed (a symbolic link to nothing,
// a loop, a file where a directory is needed) is refused, since open creates
// the file a dangling link names: log --output=scratch/dl, with dl a link to
// a name inside the operator's .git, wrote that file.
//
// Nothing is cleaned as text. resolve cleans ".." first, so with scratch/l a
// link to the operator's .git/refs, scratch/l/../config was checked as
// scratch/config, and git bundle create wrote the operator's .git/config
// (wave 12). Handing the rest to resolve from the first missing component
// repeated that: scratch/n1/../l/../PWN was checked as scratch/PWN, while
// git created n1 and then followed l, and git init created the operator's
// .git/PWN (re-audit 11).
func accountedGitResolve(dir, p string) (string, error) {
	if !filepath.IsAbs(p) {
		p = dir + string(filepath.Separator) + p
	}
	at := string(filepath.Separator)
	for _, part := range strings.Split(p, string(filepath.Separator)) {
		switch part {
		case "", ".":
			continue
		case "..":
			at = filepath.Dir(at)
			continue
		}
		next := filepath.Join(at, part)
		if _, err := os.Lstat(next); errors.Is(err, fs.ErrNotExist) {
			at = next // git creates a directory here, or writes the file
			continue
		}
		followed, err := filepath.EvalSymlinks(next)
		if err != nil {
			return "", refuse("nested git: the path " + p + " cannot be followed at " + next + " (" + err.Error() + "): a symbolic link leading nowhere, a loop, or a file where a directory is needed, so where git would write cannot be told")
		}
		at = followed
	}
	return at, nil
}

// readOnly checks a call on a protected repository (whose common directory is
// where): a read-only subcommand with read-only arguments and no
// configuration set but harmless settings. A Local task's remote change
// exits 78.
func (g *accountedGit) readOnly(c accountedGitCall, where string) error {
	for _, kv := range c.config {
		if !accountedGitReadConfig(kv[0], kv[1]) {
			return refuse("nested git -c " + accountedGitQuote(kv[0]) + " on the repository at " + where + ": nested calls inject no configuration into a repository they may only read")
		}
	}
	if name := accountedGitInjectedEnv(g.env); name != "" {
		return refuse("nested git with " + name + " set on the repository at " + where + ": nested calls inject no configuration into a repository they may only read")
	}
	check, known := accountedGitReadOnly[c.sub]
	what := ""
	if targets, _ := c.writeTargets(); !known {
		what = "is not a read-only command"
	} else if len(targets) > 0 {
		what = "writes a file (--output)"
	} else if check != nil {
		what = check(c.args)
	}
	if what == "" {
		return nil
	}
	if cfg := parseAccountedGitConfig(c.args); c.sub == "config" && g.p.Local && cfg.remote() {
		return &LocalRefusal{"git config " + cfg.key + " on the repository at " + where + " changes a remote; a Local task makes no external effect, so pushes and remote changes are refused"}
	}
	return refuse("nested git " + c.sub + " " + what + ", and the repository at " + where + " is the run's own or outside its directories: nested calls may only read it")
}

// accountedGitReadOnly are the subcommands a nested call may run on a
// protected repository, each with a check of its arguments (nil: none
// writes); with --no-optional-locks none of them writes the index, and
// readOnly refuses any file one names to write (writeTargets: --output,
// which every command that takes revision or diff options has).
var accountedGitReadOnly = map[string]func(args []string) string{
	"status": nil, "rev-parse": nil, "rev-list": nil, "ls-files": nil, "ls-tree": nil, "cat-file": nil,
	"for-each-ref": nil, "show-ref": nil, "merge-base": nil, "name-rev": nil, "describe": nil, "blame": nil,
	"annotate": nil, "pickaxe": nil, "cherry": nil, "show-branch": nil, "count-objects": nil, "check-ignore": nil,
	"check-attr": nil, "check-ref-format": nil, "check-mailmap": nil, "var": nil, "verify-commit": nil,
	"verify-tag": nil, "shortlog": nil, "grep": nil, "log": nil, "show": nil, "whatchanged": nil, "diff": nil,
	"diff-files": nil, "diff-index": nil, "diff-tree": nil, "range-diff": nil,
	"config": func(args []string) string {
		if parseAccountedGitConfig(args).write {
			return "writes configuration"
		}
		return ""
	},
	"branch": func(args []string) string {
		return accountedGitListOnly(args, []string{"delete", "move", "copy", "force", "set-upstream-to", "set-upstream", "unset-upstream", "edit-description", "track", "no-track", "create-reflog", "recurse-submodules"}, "dDmMcCfut", []string{"list", "show-current", "contains", "no-contains", "merged", "no-merged", "points-at"}, "l", []string{"contains", "no-contains", "merged", "no-merged", "points-at", "format", "sort"}, "")
	},
	"tag": func(args []string) string {
		return accountedGitListOnly(args, []string{"annotate", "sign", "no-sign", "local-user", "force", "delete", "message", "file", "edit", "cleanup", "create-reflog", "trailer"}, "asufdmFe", []string{"list", "verify", "contains", "no-contains", "merged", "no-merged", "points-at"}, "lv", []string{"contains", "no-contains", "merged", "no-merged", "points-at", "format", "sort"}, "n")
	},
	"symbolic-ref": func(args []string) string {
		positional, values := accountedGitPositional(args, []string{"delete"}, "dm")
		if len(positional) > 1 || len(values["delete"]) > 0 || accountedGitHasShort(args, "dm", "") {
			return "changes a reference"
		}
		return ""
	},
	"worktree": func(args []string) string {
		if len(args) == 0 || args[0] != "list" {
			return "manages worktrees"
		}
		return ""
	},
	"hash-object": func(args []string) string {
		if accountedGitHasShort(args, "w", "t") {
			return "writes an object"
		}
		return ""
	},
}

// writeTargets are the files and directories a call names for git to write
// besides its repository (refuseWriteTargets checks where they are):
//   - any subcommand's --output, a diff and revision option that log, show,
//     diff, rev-list, blame, shortlog, cherry-pick and many more take;
//   - format-patch -o, archive -o, bundle create's file, index-pack -o and
//     the pack it indexes (git writes the index beside it), pack-objects'
//     base name, fast-export's and fast-import's --export-marks and
//     --export-pack-edges, mailsplit -o, mailinfo's message and patch files,
//     interpret-trailers --in-place's files, merge-file's first file (unless
//     -p), checkout-index --prefix, and commit-graph's and
//     multi-pack-index's --object-dir.
//
// what names an option whose targets the policy cannot read: apply
// --unsafe-paths writes wherever the patch says. format-patch's
// format.outputDirectory is checkFormatPatchDirectory's.
func (c accountedGitCall) writeTargets() (targets []string, what string) {
	args := c.args
	if i := slices.Index(args, "--"); i >= 0 {
		args = args[:i]
	}
	for i := 0; i < len(args); i++ {
		if a := args[i]; strings.HasPrefix(a, "--") && accountedGitLong(a, "output") {
			if _, v, ok := strings.Cut(a, "="); ok {
				targets = append(targets, v)
			} else if i+1 < len(args) {
				i++
				targets = append(targets, args[i])
			}
		}
	}
	first := ""
	if len(args) > 0 {
		first = args[0]
	}
	switch c.sub {
	case "format-patch":
		targets = append(targets, accountedGitOptionValues(args, "output-directory", 'o', "vGSOl")...)
	case "archive":
		targets = append(targets, accountedGitOptionValues(args, "output", 'o', "")...)
	case "bundle":
		if first == "create" {
			positional, _ := accountedGitPositional(c.args[1:], []string{"version"}, "")
			targets = append(targets, positional[:min(len(positional), 1)]...)
		}
	case "index-pack":
		// -o has no long name.
		positional, _ := accountedGitPositional(c.args, []string{"progress-title"}, "o")
		targets = append(append(targets, accountedGitOptionValues(args, "", 'o', "")...), positional...)
	case "pack-objects":
		if !slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, "--") && accountedGitLong(a, "stdout") }) {
			positional, _ := accountedGitPositional(c.args, nil, "")
			targets = append(targets, positional[:min(len(positional), 1)]...)
		}
	case "fast-export", "fast-import":
		for _, name := range []string{"export-marks", "export-pack-edges"} {
			targets = append(targets, accountedGitOptionValues(args, name, 0, "")...)
		}
	case "mailsplit":
		for _, a := range args {
			if len(a) > 2 && strings.HasPrefix(a, "-o") {
				targets = append(targets, a[2:])
			}
		}
	case "mailinfo":
		positional, _ := accountedGitPositional(c.args, nil, "")
		targets = append(targets, positional...)
	case "interpret-trailers":
		if slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, "--") && accountedGitLong(a, "in-place") }) {
			positional, _ := accountedGitPositional(c.args, []string{"trailer", "where", "if-exists", "if-missing"}, "")
			targets = append(targets, positional...)
		}
	case "merge-file":
		if !accountedGitHasShort(args, "p", "L") && !slices.ContainsFunc(args, func(a string) bool {
			return strings.HasPrefix(a, "--") && (accountedGitLong(a, "stdout") || accountedGitLong(a, "object-id"))
		}) {
			positional, _ := accountedGitPositional(c.args, []string{"diff-algorithm"}, "L")
			targets = append(targets, positional[:min(len(positional), 1)]...)
		}
	case "checkout-index":
		targets = append(targets, accountedGitOptionValues(args, "prefix", 0, "")...)
	case "commit-graph", "multi-pack-index":
		targets = append(targets, accountedGitOptionValues(args, "object-dir", 0, "")...)
	case "apply":
		if slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, "--") && accountedGitLong(a, "unsafe-paths") }) {
			return nil, "--unsafe-paths writes files wherever the patch names them, outside the work tree; nested calls apply patches inside it"
		}
	}
	return targets, ""
}

// refuseWriteTargets refuses a file or directory a call names for git to
// write (writeTargets) outside the places it may change: the run's
// directories and the temporary ones, never the run's own git directory. A
// relative name is checked against the call's working directory and, where
// git resolves it there instead (checkout-index --prefix), the top of the
// work tree of r.
func (g *accountedGit) refuseWriteTargets(c accountedGitCall, targets []string, r *accountedGitRepository) error {
	for _, t := range targets {
		if t == "" || t == "-" {
			continue
		}
		bases := []string{c.dir}
		if r != nil && r.top != "" && !filepath.IsAbs(t) {
			bases = append(bases, r.top)
		}
		for _, base := range bases {
			x, err := accountedGitResolve(base, t)
			if err != nil {
				return err
			}
			if !g.writable(x) {
				return refuse("nested git " + c.sub + " would write " + x + ", outside the run's directories and the temporary ones, or in the run's own repository")
			}
		}
	}
	return nil
}

// accountedGitListOnly checks git branch or tag: no option that writes (long names
// write, short letters writeShort) and, unless an option selects list mode
// (list, listShort), no name to create. Options in values take the next
// argument; letters in valueShort take the rest of their cluster.
func accountedGitListOnly(args, write []string, writeShort string, list []string, listShort string, values []string, valueShort string) string {
	listing, positional := false, 0
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			positional += len(args) - i - 1
			i = len(args)
		case strings.HasPrefix(a, "--"):
			if accountedGitLong(a, write...) {
				return "changes references"
			}
			listing = listing || accountedGitLong(a, list...)
			if !strings.Contains(a, "=") && accountedGitLong(a, values...) && i+1 < len(args) {
				i++
			}
		case strings.HasPrefix(a, "-") && a != "-":
			letters, _ := accountedGitShortCluster(a, writeShort+valueShort)
			if strings.ContainsAny(letters, writeShort) {
				return "changes references"
			}
			listing = listing || strings.ContainsAny(letters, listShort)
		default:
			positional++
		}
	}
	if positional > 0 && !listing {
		return "creates a reference"
	}
	return ""
}

// runsProgram names the option or subcommand of a call that has git run a
// program the call chose, or "".
func (c accountedGitCall) runsProgram() string {
	first := ""
	for _, a := range c.args {
		if !strings.HasPrefix(a, "-") {
			first = a
			break
		}
	}
	if c.sub == "init" || c.sub == "init-db" || c.sub == "clone" {
		for _, a := range c.args {
			if a == "--" {
				break
			}
			if strings.HasPrefix(a, "--") && accountedGitLong(a, "template") {
				return "--template copies the hooks and the configuration, which can name programs, of a directory the call names"
			}
		}
	}
	// long[0] is short's long name. ls-remote --exec is --upload-pack.
	var long []string
	short, valueShort := "", ""
	switch c.sub {
	case "submodule":
		if first == "foreach" {
			return "foreach runs a command the call names in each submodule"
		}
	case "bisect":
		if first == "run" {
			return "run runs a command the call names at each step"
		}
	case "rebase":
		long, short, valueShort = []string{"exec"}, "x", "sXCxS"
	case "clone":
		long, short, valueShort = []string{"upload-pack"}, "u", "objuc"
	case "fetch", "pull":
		long = []string{"upload-pack"}
	case "ls-remote":
		long = []string{"upload-pack", "exec"}
	case "archive":
		long = []string{"exec"}
	case "grep":
		long, short, valueShort = []string{"open-files-in-pager"}, "O", "ABCefmO"
	}
	for _, a := range c.args {
		if a == "--" {
			break
		}
		for _, l := range long {
			if strings.HasPrefix(a, "--") && accountedGitLong(a, l) {
				return "--" + l + " runs a program the call names"
			}
		}
		if short != "" && accountedGitHasShort([]string{a}, short, valueShort) {
			return "--" + long[0] + " runs a program the call names"
		}
	}
	return ""
}

// changesHost names git maintenance's subcommand when it schedules jobs on
// the host or registers the repository in the user's configuration, or "".
func (c accountedGitCall) changesHost() string {
	if c.sub != "maintenance" {
		return ""
	}
	for _, a := range c.args {
		if !strings.HasPrefix(a, "-") {
			if a == "start" || a == "stop" || a == "register" || a == "unregister" {
				return a
			}
			return ""
		}
	}
	return ""
}

// accountedGitStrategies are git's own merge strategies. A strategy git does
// not have runs git-merge-NAME from PATH.
var accountedGitStrategies = map[string]bool{"ort": true, "recursive": true, "resolve": true, "octopus": true, "ours": true, "subtree": true}

// accountedGitProgramEnv are the environment variables that name a program
// for git to run, each with the values besides "" that name none. git starts
// an editor without a terminal (a commit with no -m), a pager only on one.
var accountedGitProgramEnv = map[string][]string{
	"GIT_SSH": nil, "GIT_SSH_COMMAND": nil, "GIT_PROXY_COMMAND": nil, "GIT_ASKPASS": nil, "SSH_ASKPASS": nil,
	"GIT_EXTERNAL_DIFF": nil, "GIT_EXEC_PATH": nil, "GIT_TEMPLATE_DIR": nil,
	"GIT_EDITOR": {":", "true"}, "GIT_SEQUENCE_EDITOR": {":", "true"}, "EDITOR": {":", "true"}, "VISUAL": {":", "true"},
	"GIT_PAGER": {"cat"}, "PAGER": {"cat"},
}

// accountedGitProgramEnvNames are accountedGitProgramEnv's names in order.
var accountedGitProgramEnvNames = slices.Sorted(maps.Keys(accountedGitProgramEnv))

// accountedGitFreeEnv are the GIT_* variables a nested call may set to any
// value, from git(1), git-config(1) and the names git 2.55 reads: none names
// a program for git to run, a file for it to write or a place for it to keep
// references. They set identities and dates; where the repository is
// (locate finds it with them in effect, and the policy decides by where it
// is); how pathspecs, replace refs, notes, merges and progress behave; and
// HTTP transports, which nested calls never reach (GIT_ALLOW_PROTOCOL=file).
// GIT_ALTERNATE_OBJECT_DIRECTORIES only adds objects to read. The other
// variables a call may set are accountedGitKnownEnv's.
var accountedGitFreeEnv = func() map[string]bool {
	free := map[string]bool{}
	for _, name := range strings.Fields(`GIT_AUTHOR_NAME GIT_AUTHOR_EMAIL GIT_AUTHOR_DATE GIT_COMMITTER_NAME
		GIT_COMMITTER_EMAIL GIT_COMMITTER_DATE GIT_DIR GIT_WORK_TREE GIT_COMMON_DIR GIT_INDEX_FILE
		GIT_OBJECT_DIRECTORY GIT_ALTERNATE_OBJECT_DIRECTORIES GIT_CEILING_DIRECTORIES
		GIT_DISCOVERY_ACROSS_FILESYSTEM GIT_IMPLICIT_WORK_TREE GIT_NAMESPACE GIT_DEFAULT_HASH
		GIT_DEFAULT_REF_FORMAT GIT_INDEX_VERSION GIT_LITERAL_PATHSPECS GIT_GLOB_PATHSPECS GIT_NOGLOB_PATHSPECS
		GIT_ICASE_PATHSPECS GIT_OPTIONAL_LOCKS GIT_NO_REPLACE_OBJECTS GIT_REPLACE_REF_BASE GIT_REF_PARANOIA
		GIT_COMMIT_GRAPH_PARANOIA GIT_NO_LAZY_FETCH GIT_ATTR_SOURCE GIT_ATTR_NOSYSTEM GIT_TERMINAL_PROMPT
		GIT_FLUSH GIT_PROGRESS_DELAY GIT_ADVICE GIT_PRINT_SHA1_ELLIPSIS GIT_PAGER_IN_USE GIT_MERGE_VERBOSITY
		GIT_MERGE_AUTOEDIT GIT_REFLOG_ACTION GIT_CHERRY_PICK_HELP GIT_NOTES_REF GIT_NOTES_DISPLAY_REF
		GIT_NOTES_REWRITE_REF GIT_NOTES_REWRITE_MODE GIT_DIFF_OPTS GIT_DIFF_PATH_COUNTER GIT_DIFF_PATH_TOTAL
		GIT_EXTERNAL_DIFF_TRUST_EXIT_CODE GIT_PREFIX GIT_ALLOW_PROTOCOL GIT_PROTOCOL_FROM_USER GIT_SSH_VARIANT
		GIT_USER_AGENT GIT_REDACT_COOKIES GIT_SSL_NO_VERIFY GIT_SSL_CERT GIT_SSL_KEY GIT_SSL_CERT_TYPE
		GIT_SSL_KEY_TYPE GIT_SSL_CERT_PASSWORD_PROTECTED GIT_SSL_CAINFO GIT_SSL_CAPATH GIT_SSL_VERSION
		GIT_SSL_CIPHER_LIST GIT_PROXY_SSL_CAINFO GIT_PROXY_SSL_CERT GIT_PROXY_SSL_KEY
		GIT_PROXY_SSL_CERT_PASSWORD_PROTECTED GIT_HTTP_KEEPALIVE_COUNT GIT_HTTP_KEEPALIVE_IDLE
		GIT_HTTP_KEEPALIVE_INTERVAL GIT_HTTP_LOW_SPEED_LIMIT GIT_HTTP_LOW_SPEED_TIME GIT_HTTP_MAX_REQUESTS
		GIT_HTTP_MAX_RETRIES GIT_HTTP_MAX_RETRY_TIME GIT_HTTP_PROXY_AUTHMETHOD GIT_HTTP_RETRY_AFTER
		GIT_HTTP_USER_AGENT GIT_CURL_VERBOSE GIT_CURL_FTP_NO_EPSV GIT_SMART_HTTP GIT_TCP_KEEPCNT
		GIT_TCP_KEEPIDLE GIT_TCP_KEEPINTVL`) {
		free[name] = true
	}
	return free
}()

// accountedGitKnownEnv reports whether a nested call may set the GIT_*
// variable name: one of accountedGitFreeEnv; one of accountedGitProgramEnv,
// whose value refuseChosen checks; a configuration variable (GIT_CONFIG,
// GIT_CONFIG_GLOBAL, GIT_CONFIG_SYSTEM, GIT_CONFIG_NOSYSTEM, GIT_CONFIG_COUNT
// with its keys and values, GIT_CONFIG_PARAMETERS), checked setting by
// setting or refused; or a trace variable (GIT_TRACE*), whose target
// refuseTraceTargets checks. Every other GIT_* variable a call sets to a
// value is refused (an empty one is left as git reads it): an allowlist, as
// for nested go, because git keeps adding variables that run a
// program or write a file (GIT_TEST_FSMONITOR runs one on every status,
// GIT_TEST_MAINT_SCHEDULER another, GIT_REFERENCE_BACKEND keeps references
// at any path), and a list of the bad ones would miss the next.
func accountedGitKnownEnv(name string) bool {
	if _, program := accountedGitProgramEnv[name]; program || accountedGitFreeEnv[name] {
		return true
	}
	switch name {
	case "GIT_CONFIG", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "GIT_CONFIG_NOSYSTEM", "GIT_CONFIG_COUNT", "GIT_CONFIG_PARAMETERS":
		return true
	}
	return strings.HasPrefix(name, "GIT_TRACE") || strings.HasPrefix(name, "GIT_CONFIG_KEY_") || strings.HasPrefix(name, "GIT_CONFIG_VALUE_")
}

// accountedGitProgramNames are the configuration variable names (a key's
// last part) whose value is a program for git to run or a tool's path, from
// git-config(1) for git 2.55: *.cmd and *.command (which also end other
// names: core.sshCommand, core.alternateRefsCommand,
// gpg.ssh.defaultKeyCommand, sendemail.toCmd), *.program, *.textconv,
// merge.*.driver, credential.*helper, core.editor and sequence.editor,
// browsers, core.askPass, diff.external, core.gitProxy,
// remote.*.uploadpack, receivepack and vcs (a remote helper),
// interactive.diffFilter, difftool.*.path and the other tools' paths,
// init.templateDir (whose hooks a clone runs), the pack-objects and
// recent-objects hooks, imap.tunnel and sendemail.smtpServer.
var accountedGitProgramNames = map[string]bool{
	"cmd": true, "command": true, "program": true, "textconv": true, "driver": true, "helper": true,
	"browser": true, "askpass": true, "external": true, "gitproxy": true, "uploadpack": true,
	"receivepack": true, "vcs": true, "difffilter": true, "path": true, "templatedir": true,
	"packobjectshook": true, "recentobjectshook": true, "tunnel": true, "smtpserver": true,
}

// accountedGitBundles is what accountedGitChosenSetting says a bundle
// setting does.
const accountedGitBundles = "has git download bundles over HTTP"

// accountedGitChosenSetting names what a configuration setting a call gives
// git has it do past the wrappers, or "": name a program for git to run
// (accountedGitProgramNames, filter.*, core.hooksPath other than /dev/null,
// core.fsmonitor on, an editor or pager, submodule.*.update=!..., a pull
// strategy git does not have), configure a hook (any hook.* setting but
// hook.<event>.enabled or hook.<name>.enabled false), a file of
// configuration to load (include.path, includeIf.*.path), or bundles to
// download over HTTP, past GIT_ALLOW_PROTOCOL (fetch.bundleURI,
// transfer.bundleURI). An empty value names nothing, except for
// core.hooksPath and hook.*.
//
// The hook rule matters because git reads a call's -c and --config-env
// settings after GIT_CONFIG_COUNT's: a call's hook.pre-commit.enabled=true
// outranks the hook.pre-commit.enabled=false accountedGitExecSettings forces,
// and git then runs a hook the test repository's own configuration names
// (hook.<name>.command and hook.<name>.event), past the wrappers.
func accountedGitChosenSetting(key, value string) string {
	k := strings.ToLower(key)
	section, _, _ := strings.Cut(k, ".")
	name := k[strings.LastIndexByte(k, '.')+1:]
	const program = "names a program for git to run"
	const bundles = accountedGitBundles
	switch {
	case section == "hook":
		switch {
		case name == "enabled" && strings.Count(k, ".") >= 2 && accountedGitFalse(value):
			return "" // switches a hook off
		case name == "command":
			return program
		}
		return "configures a hook, which git runs past the wrappers"
	case k == "core.hookspath":
		if value == "/dev/null" {
			return ""
		}
		return program
	case k == "core.fsmonitor": // true starts git's own monitor daemon
		if accountedGitFalse(value) {
			return ""
		}
		return program
	case k == "transfer.bundleuri":
		if accountedGitFalse(value) {
			return ""
		}
		return bundles
	case k == "fetch.bundleuri":
		if value == "" {
			return ""
		}
		return bundles
	case section == "submodule" && name == "update":
		if strings.HasPrefix(strings.TrimSpace(value), "!") {
			return program
		}
		return ""
	case k == "pull.twohead", k == "pull.octopus":
		for _, s := range strings.Fields(value) {
			if !accountedGitStrategies[s] {
				return program
			}
		}
		return ""
	case value == "", section == "color", section == "advice":
		return ""
	case section == "filter":
		return program
	case section == "include", section == "includeif":
		if name == "path" {
			return "names a file of configuration for git to load"
		}
		return ""
	case name == "editor":
		if value == ":" || value == "true" {
			return ""
		}
		return program
	case k == "core.pager", section == "pager":
		if value == "cat" || accountedGitBool(value) {
			return ""
		}
		return program
	case accountedGitProgramNames[name], strings.HasSuffix(name, "cmd"), strings.HasSuffix(name, "command"):
		return program
	}
	return ""
}

// accountedGitFalse reports whether value is a git boolean false ("" too).
func accountedGitFalse(value string) bool {
	switch strings.ToLower(value) {
	case "", "false", "no", "off", "0":
		return true
	}
	return false
}

// accountedGitBool reports whether value is a git boolean.
func accountedGitBool(value string) bool {
	switch strings.ToLower(value) {
	case "true", "yes", "on", "1":
		return true
	}
	return accountedGitFalse(value)
}

// refuseChosen refuses, on any repository, what the call itself gives git
// that names a program for git to run, a file of configuration to load or
// bundles to download (accountedGitChosenSetting): its -c and --config-env
// settings, GIT_CONFIG_COUNT's and clone's --config; GIT_CONFIG_PARAMETERS,
// which the policy does not read; the variables in accountedGitProgramEnv
// and every GIT_* variable it does not know (accountedGitKnownEnv); and a
// merge strategy git does not have. A file of configuration the call names
// is refuseNamedConfigFiles', a git config write refuseProgram's; a test's
// own repository's configuration the test wrote itself is its own setup, and
// accountedGitExecEnv keeps HOME's and the system's out of every call.
func (c accountedGitCall) refuseChosen(env []string) error {
	for _, kv := range c.config {
		if what := accountedGitChosenSetting(kv[0], kv[1]); what != "" {
			return refuse("nested git -c " + accountedGitQuote(kv[0]) + " " + what + "; nested calls choose none, on any repository")
		}
	}
	if c.sub == "clone" {
		for _, setting := range accountedGitOptionValues(c.args, "config", 'c', "objuc") {
			key, value, set := strings.Cut(setting, "=")
			if !set {
				value = "true"
			}
			if what := accountedGitChosenSetting(key, value); what != "" {
				return refuse("nested git clone --config " + accountedGitQuote(key) + " " + what + "; nested calls choose none, on any repository")
			}
		}
	}
	if accountedGitEnv(env, "GIT_CONFIG_PARAMETERS") != "" {
		return refuse("nested git with GIT_CONFIG_PARAMETERS set: nested calls set configuration with -c, --config-env or GIT_CONFIG_COUNT, which the nested-command policy reads")
	}
	count := accountedGitEnv(env, "GIT_CONFIG_COUNT")
	n, err := strconv.Atoi(count)
	if count != "" && (err != nil || n < 0 || n > 1000) {
		return refuse("nested git with GIT_CONFIG_COUNT set to " + accountedGitQuote(count) + ", not a count of settings the nested-command policy reads")
	}
	for i := 0; i < n; i++ {
		key := "GIT_CONFIG_KEY_" + strconv.Itoa(i)
		if what := accountedGitChosenSetting(accountedGitEnv(env, key), accountedGitEnv(env, "GIT_CONFIG_VALUE_"+strconv.Itoa(i))); what != "" {
			return refuse("nested git with " + key + " set " + what + "; nested calls choose none, on any repository")
		}
	}
	for _, name := range accountedGitProgramEnvNames {
		if v := accountedGitEnv(env, name); v != "" && !slices.Contains(accountedGitProgramEnv[name], v) {
			return refuse("nested git with " + name + " set names a program for git to run; nested calls choose none, on any repository")
		}
	}
	for _, kv := range env {
		if name, value, _ := strings.Cut(kv, "="); value != "" && strings.HasPrefix(name, "GIT_") && !accountedGitKnownEnv(name) {
			return refuse("nested git with " + name + " set: git may read it as a program to run, a file to write or a place to keep references, and the nested-command policy lets a call set only the GIT_* variables it knows; nested calls leave it unset")
		}
	}
	if s := c.strategy(); s != "" {
		return refuse("nested git " + c.sub + " --strategy " + accountedGitQuote(s) + " runs git-merge-" + s + " from PATH, a program git does not have; nested calls use git's own strategies")
	}
	return nil
}

// strategy is the first merge strategy a merge, pull, rebase, cherry-pick or
// revert names that git does not have, or "".
func (c accountedGitCall) strategy() string {
	short, valueShort := byte(0), ""
	switch c.sub {
	case "merge":
		short, valueShort = 's', "sXmF"
	case "pull":
		short, valueShort = 's', "sXo"
	case "rebase":
		short, valueShort = 's', "CsXx"
	case "cherry-pick", "revert": // -s is --signoff
		valueShort = "mX"
	default:
		return ""
	}
	for _, s := range accountedGitOptionValues(c.args, "strategy", short, valueShort) {
		if !accountedGitStrategies[s] {
			return s
		}
	}
	return ""
}

// accountedGitCloneValues are git clone's long options that take a value.
var accountedGitCloneValues = []string{"origin", "branch", "revision", "upload-pack", "reference", "reference-if-able", "separate-git-dir", "depth", "shallow-since", "shallow-exclude", "config", "server-option", "jobs", "filter", "template", "bundle-uri", "ref-format"}

// accountedGitFetchValues are git fetch's long options that take a value;
// pull takes them too.
var accountedGitFetchValues = []string{"upload-pack", "jobs", "depth", "shallow-since", "shallow-exclude", "deepen", "refmap", "server-option", "negotiation-restrict", "negotiation-tip", "negotiation-include", "filter"}

// offHost is the first repository the call names by a URL off this host
// (accountedGitOffHost), or "": clone's repository and --bundle-uri (git
// downloads bundles over HTTP past GIT_ALLOW_PROTOCOL), the repository of
// fetch, pull and ls-remote (each of fetch --multiple's), archive --remote
// and submodule add's repository. A remote named by its name, a
// url.<base>.insteadOf rewrite and a submodule's URL reach git's own
// protocol list instead (accountedGitExecEnv).
func (c accountedGitCall) offHost() string {
	var named []string
	switch c.sub {
	case "clone":
		positional, values := accountedGitPositional(c.args, accountedGitCloneValues, "objuc")
		named = append(positional[:min(len(positional), 1)], values["bundle-uri"]...)
	case "fetch", "pull", "ls-remote":
		long, short := accountedGitFetchValues, "oj"
		if c.sub == "pull" {
			long, short = append([]string{"strategy", "strategy-option", "cleanup"}, long...), "sXo"
		} else if c.sub == "ls-remote" {
			long, short = []string{"upload-pack", "exec", "sort", "server-option"}, "o"
		}
		positional, _ := accountedGitPositional(c.args, long, short)
		named = positional[:min(len(positional), 1)]
		if c.sub == "fetch" && slices.ContainsFunc(c.args, func(a string) bool { return strings.HasPrefix(a, "--") && accountedGitLong(a, "multiple") }) {
			named = positional
		}
	case "archive":
		_, values := accountedGitPositional(c.args, []string{"remote", "exec", "format", "prefix", "add-file", "add-virtual-file", "output", "mtime"}, "o")
		named = values["remote"]
	case "submodule":
		for i, a := range c.args {
			if !strings.HasPrefix(a, "-") {
				if a == "add" {
					positional, _ := accountedGitPositional(c.args[i+1:], []string{"branch", "name", "reference", "ref-format", "depth"}, "b")
					named = positional[:min(len(positional), 1)]
				}
				break
			}
		}
	}
	for _, n := range named {
		if accountedGitOffHost(n) {
			return n
		}
	}
	return ""
}

// accountedGitOffHost reports whether a repository argument is off this host
// as git reads it: a URL other than file://, a remote helper's
// TRANSPORT::ADDRESS, or ssh's HOST:PATH (a colon before any slash). A path,
// a file:// URL and a remote's name are not.
func accountedGitOffHost(arg string) bool {
	i := 0
	for i < len(arg) && (arg[i] >= 'a' && arg[i] <= 'z' || arg[i] >= 'A' && arg[i] <= 'Z' || i > 0 && (arg[i] >= '0' && arg[i] <= '9' || arg[i] == '+' || arg[i] == '-' || arg[i] == '.')) {
		i++
	}
	if i > 0 && strings.HasPrefix(arg[i:], "://") {
		return arg[:i] != "file"
	}
	if i > 0 && strings.HasPrefix(arg[i:], "::") {
		return true
	}
	colon, slash := strings.IndexByte(arg, ':'), strings.IndexByte(arg, '/')
	return colon >= 0 && (slash < 0 || slash > colon)
}

// accountedGitTraceTarget is the first GIT_TRACE* variable in env whose
// value names a file, directory or socket for git's trace, or "".
func accountedGitTraceTarget(env []string) string {
	for _, kv := range env {
		name, value, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "GIT_TRACE") && (strings.HasPrefix(value, "/") || strings.HasPrefix(value, "af_unix:")) {
			return name
		}
	}
	return ""
}

// refuseTraceTargets refuses a GIT_TRACE* variable that has git write its
// trace to a socket, or to a file or directory where a nested call may not
// write (writable): a trace appends to the file it names, so one inside the
// run's own git directory would change it.
func (g *accountedGit) refuseTraceTargets() error {
	for _, kv := range g.env {
		name, value, _ := strings.Cut(kv, "=")
		if !strings.HasPrefix(name, "GIT_TRACE") {
			continue
		}
		if strings.HasPrefix(value, "af_unix:") {
			return refuse("nested git with " + name + " set traces to a socket; nested calls trace only to standard error or to a file they may write")
		}
		if strings.HasPrefix(value, "/") {
			x, err := accountedGitResolve("/", value)
			if err != nil {
				return err
			}
			if !g.writable(x) {
				return refuse("nested git with " + name + " set writes its trace to " + x + ", outside the run's directories and the temporary ones, or in the run's own repository")
			}
		}
	}
	return nil
}

// accountedGitOptionValues are the values the arguments before -- give the
// option --long (matched by prefix, as git matches it) or -short: --long=V,
// --long V, -sV and -s V. The other letters in valueShort take a value too:
// the rest of their cluster or the next argument.
func accountedGitOptionValues(args []string, long string, short byte, valueShort string) []string {
	var values []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return values
		case strings.HasPrefix(a, "--"):
			if !accountedGitLong(a, long) {
				continue
			}
			if _, v, ok := strings.Cut(a, "="); ok {
				values = append(values, v)
			} else if i+1 < len(args) {
				i++
				values = append(values, args[i])
			}
		case strings.HasPrefix(a, "-") && a != "-":
			for j := 1; j < len(a); j++ {
				if short == 0 || a[j] != short {
					if strings.IndexByte(valueShort, a[j]) < 0 {
						continue
					}
				}
				v := a[j+1:]
				if v == "" && i+1 < len(args) {
					i++
					v = args[i]
				}
				if short != 0 && a[j] == short {
					values = append(values, v)
				}
				break
			}
		}
	}
	return values
}

// accountedGitLastWins is env with each key once, at its last value: the
// value the policy reads (accountedGitEnv) and exec.Cmd passes on.
func accountedGitLastWins(env []string) []string {
	last := make(map[string]int, len(env))
	for i, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		last[key] = i
	}
	out := make([]string, 0, len(last))
	for i, kv := range env {
		if key, _, _ := strings.Cut(kv, "="); last[key] == i {
			out = append(out, kv)
		}
	}
	return out
}

// accountedGitHookEvents are the hook events of githooks(5) for git 2.55.
var accountedGitHookEvents = strings.Fields(`applypatch-msg pre-applypatch post-applypatch pre-commit
	pre-merge-commit prepare-commit-msg commit-msg post-commit pre-rebase post-checkout post-merge pre-push
	pre-receive update proc-receive post-receive post-update reference-transaction push-to-checkout
	pre-auto-gc post-rewrite sendemail-validate fsmonitor-watchman p4-changelist p4-prepare-changelist
	p4-post-changelist p4-pre-submit post-index-change`)

// accountedGitExecSettings are the settings accountedGitExecEnv adds after
// the call's own GIT_CONFIG_COUNT settings. Command-line configuration
// outranks every file, the repository's own included, but git reads the
// call's -c and --config-env after GIT_CONFIG_COUNT, so these hold because
// accountedGitChosenSetting refuses a call's own setting that would undo
// them (hook.*, core.hooksPath, core.fsmonitor, bundle URIs). On every nested
// call:
//   - no hook runs: core.hooksPath=/dev/null for the hooks directory, and
//     hook.<event>.enabled=false for every event, which also stops hooks a
//     configuration names (hook.<name>.command);
//   - no file-system monitor runs (core.fsmonitor=false);
//   - git downloads no bundles over HTTP, which it does past
//     GIT_ALLOW_PROTOCOL (fetch.bundleURI empty, transfer.bundleURI=false).
//
// A hook or monitor runs the real git, past the wrappers, whoever installed
// it: a test's own repository's hooks no longer run under account mode.
var accountedGitExecSettings = func() [][2]string {
	settings := [][2]string{{"core.hooksPath", "/dev/null"}, {"core.fsmonitor", "false"}}
	for _, event := range accountedGitHookEvents {
		settings = append(settings, [2]string{"hook." + event + ".enabled", "false"})
	}
	return append(settings, [2]string{"fetch.bundleURI", ""}, [2]string{"transfer.bundleURI", "false"})
}()

// accountedGitTrace2Targets are the trace2 targets git also reads from a
// system or global configuration file (trace2.normalTarget, perfTarget and
// eventTarget), where a variable that is set outranks the file.
var accountedGitTrace2Targets = []string{"GIT_TRACE2", "GIT_TRACE2_PERF", "GIT_TRACE2_EVENT"}

// accountedGitExecEnv is the environment a nested git call the policy passed
// runs with (runAccounted): the call's, each key once at its last value
// (accountedGitLastWins), with
//   - GIT_CONFIG_GLOBAL=/dev/null unless the call names a global file, and
//     GIT_CONFIG_NOSYSTEM=1 unless it names a system one, so HOME and
//     XDG_CONFIG_HOME choose no configuration (on a protected repository the
//     policy refuses a named file but /dev/null; elsewhere it checks one,
//     refuseNamedConfigFiles);
//   - GIT_ALLOW_PROTOCOL=file, so git reaches only local repositories, paths
//     and file:// URLs, whatever a remote's URL, a url.<base>.insteadOf or a
//     protocol.*.allow setting says (and lets a submodule clone a local one,
//     which git's default leaves to the user);
//   - each of GIT_TRACE2, GIT_TRACE2_PERF and GIT_TRACE2_EVENT the call
//     leaves unset or empty set to 0, so no configuration file sends git's
//     trace anywhere (a trace appends to the file it names, such as the
//     operator's .git/config); one the call sets is refuseTraceTargets';
//   - no hook, no file-system monitor and no bundle URIs
//     (accountedGitExecSettings).
//
// Programs git starts inherit it.
func accountedGitExecEnv(env []string) []string {
	env = accountedGitLastWins(env)
	global, system := accountedGitEnv(env, "GIT_CONFIG_GLOBAL") != "", accountedGitEnv(env, "GIT_CONFIG_SYSTEM") != ""
	count, err := strconv.Atoi(accountedGitEnv(env, "GIT_CONFIG_COUNT"))
	if err != nil || count < 0 {
		count = 0
	}
	slot := map[string]bool{}
	for i := range accountedGitExecSettings {
		slot["GIT_CONFIG_KEY_"+strconv.Itoa(count+i)], slot["GIT_CONFIG_VALUE_"+strconv.Itoa(count+i)] = true, true
	}
	for _, name := range accountedGitTrace2Targets {
		slot[name] = accountedGitEnv(env, name) == ""
	}
	out := make([]string, 0, len(env)+2*len(accountedGitExecSettings)+8)
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		if key == "GIT_ALLOW_PROTOCOL" || key == "GIT_CONFIG_COUNT" || slot[key] || !global && key == "GIT_CONFIG_GLOBAL" || !system && key == "GIT_CONFIG_NOSYSTEM" {
			continue
		}
		out = append(out, kv)
	}
	if !global {
		out = append(out, "GIT_CONFIG_GLOBAL=/dev/null")
	}
	if !system {
		out = append(out, "GIT_CONFIG_NOSYSTEM=1")
	}
	for _, name := range accountedGitTrace2Targets {
		if slot[name] {
			out = append(out, name+"=0")
		}
	}
	out = append(out, "GIT_ALLOW_PROTOCOL=file")
	for i, kv := range accountedGitExecSettings {
		out = append(out, "GIT_CONFIG_KEY_"+strconv.Itoa(count+i)+"="+kv[0], "GIT_CONFIG_VALUE_"+strconv.Itoa(count+i)+"="+kv[1])
	}
	return append(out, "GIT_CONFIG_COUNT="+strconv.Itoa(count+len(accountedGitExecSettings)))
}

// accountedGitReadConfig reports whether a -c setting on a call that may only
// read is harmless: Go's VCS stamping passes log.showsignature=false, Fae's
// own git calls switch hooks and the file-system monitor off, and
// accountedGitExecEnv switches hooks, the monitor and bundle URIs off (so a
// nested call's own nested calls pass an outer run's policy).
func accountedGitReadConfig(key, value string) bool {
	key = strings.ToLower(key)
	switch {
	case key == "log.showsignature", key == "core.quotepath", key == "safe.directory", key == "user.name",
		key == "user.email", key == "core.abbrev", strings.HasPrefix(key, "color."), strings.HasPrefix(key, "advice."):
		return true
	case key == "core.fsmonitor", key == "transfer.bundleuri", strings.HasPrefix(key, "hook.") && strings.HasSuffix(key, ".enabled"):
		return value == "false" || value == "no" || value == "off" || value == "0"
	case key == "core.hookspath":
		return value == "/dev/null"
	case key == "fetch.bundleuri":
		return value == ""
	}
	return false
}

// accountedGitInjectedEnv names an environment variable that sets
// configuration on a git call that may only read, or "". (refuseChosen has
// refused GIT_CONFIG_PARAMETERS and the variables that name a program.)
func accountedGitInjectedEnv(env []string) string {
	if accountedGitEnv(env, "GIT_CONFIG") != "" {
		return "GIT_CONFIG"
	}
	for _, name := range []string{"GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM"} {
		if v := accountedGitEnv(env, name); v != "" && v != "/dev/null" {
			return name
		}
	}
	count := accountedGitEnv(env, "GIT_CONFIG_COUNT")
	if count == "" || count == "0" {
		return ""
	}
	n, err := strconv.Atoi(count)
	if err != nil || n < 0 || n > 1000 {
		return "GIT_CONFIG_COUNT"
	}
	for i := 0; i < n; i++ {
		if !accountedGitReadConfig(accountedGitEnv(env, "GIT_CONFIG_KEY_"+strconv.Itoa(i)), accountedGitEnv(env, "GIT_CONFIG_VALUE_"+strconv.Itoa(i))) {
			return "GIT_CONFIG_KEY_" + strconv.Itoa(i)
		}
	}
	return ""
}

// accountedGitConfig is what a git config call does: whether it writes, the key or
// section it writes and the value, and where.
type accountedGitConfig struct {
	write          bool
	key, value     string
	unset, rename  bool   // removes the key or section; renames a section
	file           string // --file or -f
	global, system bool
}

// refuseProgram refuses a configuration write that names a program for git
// to run or a file of configuration to load (accountedGitChosenSetting), as
// refuseChosen refuses them on the command line, and a section rename,
// which can turn harmless settings into such ones (x.clean into
// filter.x.clean): a nested call writes no such setting, on any repository.
// A bundle setting is written: every nested call's own settings switch
// bundle URIs off over any file (accountedGitExecSettings).
func (c accountedGitConfig) refuseProgram() error {
	if c.rename {
		return refuse("nested git config renames a section, which can turn its settings into ones that name a program for git to run; nested calls rename none")
	}
	if what := accountedGitChosenSetting(c.key, c.value); c.key != "" && !c.unset && what != "" && what != accountedGitBundles {
		return refuse("nested git config " + accountedGitQuote(c.key) + " " + what + "; nested calls write no such setting, on any repository")
	}
	return nil
}

// parseAccountedGitConfig reads git config's arguments, old style (--get, --unset,
// NAME VALUE) or new (get, set, unset, ...). Long options are matched as git
// matches them, by any prefix, and a prefix that could name a write or a file
// is taken to.
func parseAccountedGitConfig(args []string) accountedGitConfig {
	var c accountedGitConfig
	var positional []string
	read, write := false, false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			positional = append(positional, args[i+1:]...)
			i = len(args)
		case strings.HasPrefix(a, "--"):
			_, value, attached := strings.Cut(a, "=")
			write = write || accountedGitLong(a, "add", "replace-all", "unset", "unset-all", "rename-section", "remove-section", "edit")
			c.unset = c.unset || accountedGitLong(a, "unset", "unset-all", "remove-section")
			c.rename = c.rename || accountedGitLong(a, "rename-section")
			read = read || accountedGitLong(a, "get", "get-all", "get-regexp", "get-urlmatch", "get-color", "get-colorbool", "list")
			c.global = c.global || accountedGitLong(a, "global")
			c.system = c.system || accountedGitLong(a, "system")
			if !attached && accountedGitLong(a, "file", "blob", "type", "default", "comment", "value", "url") && i+1 < len(args) {
				i++
				value = args[i]
			}
			if accountedGitLong(a, "file") {
				c.file = value
			}
		case strings.HasPrefix(a, "-") && a != "-":
			letters, next := accountedGitShortCluster(a, "f")
			write = write || strings.Contains(letters, "e")
			read = read || strings.Contains(letters, "l")
			if strings.HasSuffix(letters, "f") {
				c.file = a[1+len(letters):]
				if next && i+1 < len(args) {
					i++
					c.file = args[i]
				}
			}
		default:
			positional = append(positional, a)
		}
	}
	first := ""
	if len(positional) > 0 {
		first = positional[0]
	}
	switch {
	case first == "set" || first == "unset" || first == "rename-section" || first == "remove-section" || first == "edit":
		c.write = true
		c.unset = c.unset || first == "unset" || first == "remove-section"
		c.rename = c.rename || first == "rename-section"
		if len(positional) > 1 {
			c.key = positional[1]
		}
		if len(positional) > 2 {
			c.value = positional[2]
		}
	case first == "get" || first == "list" || read && !write:
	case write || len(positional) > 1:
		c.write, c.key = true, first
		if len(positional) > 1 {
			c.value = positional[1]
		}
	}
	return c
}

// files are the configuration files a git config write names, or none when
// it writes the repository's own.
func (c accountedGitConfig) files(env []string) []string {
	var files []string
	if c.file != "" {
		files = append(files, c.file)
	}
	if c.global {
		if f := accountedGitEnv(env, "GIT_CONFIG_GLOBAL"); f != "" {
			files = append(files, f)
		} else {
			home, xdg := accountedGitEnv(env, "HOME"), accountedGitEnv(env, "XDG_CONFIG_HOME")
			if xdg == "" {
				xdg = filepath.Join(home, ".config")
			}
			files = append(files, filepath.Join(home, ".gitconfig"), filepath.Join(xdg, "git", "config"))
		}
	}
	if c.system {
		f := accountedGitEnv(env, "GIT_CONFIG_SYSTEM")
		if f == "" {
			f = "/etc/gitconfig"
		}
		files = append(files, f)
	}
	if f := accountedGitEnv(env, "GIT_CONFIG"); len(files) == 0 && f != "" {
		files = append(files, f)
	}
	return files
}

// remote reports whether a config write changes a remote: a remote.* or
// url.* key or section, or a branch's remote.
func (c accountedGitConfig) remote() bool {
	key := strings.ToLower(c.key)
	return key == "remote" || key == "url" || strings.HasPrefix(key, "remote.") || strings.HasPrefix(key, "url.") ||
		strings.HasPrefix(key, "branch.") && (strings.HasSuffix(key, ".remote") || strings.HasSuffix(key, ".pushremote"))
}

// accountedGitPositional splits a subcommand's arguments into its positional ones and
// the values of the long options in long (matched by prefix, as git does),
// skipping the values of short option letters in short.
func accountedGitPositional(args, long []string, short string) ([]string, map[string][]string) {
	var positional []string
	values := map[string][]string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return append(positional, args[i+1:]...), values
		case strings.HasPrefix(a, "--"):
			name, value, attached := strings.Cut(strings.TrimPrefix(a, "--"), "=")
			for _, l := range long {
				if name == "" || !strings.HasPrefix(l, name) {
					continue
				}
				if !attached && i+1 < len(args) {
					i++
					value = args[i]
					attached = true
				}
				values[l] = append(values[l], value)
			}
		case strings.HasPrefix(a, "-") && a != "-":
			if _, next := accountedGitShortCluster(a, short); next && i+1 < len(args) {
				i++
			}
		default:
			positional = append(positional, a)
		}
	}
	return positional, values
}

// accountedGitLong reports whether the long option a (--name or --name=value)
// names any of names: git accepts any prefix of an option's name.
func accountedGitLong(a string, names ...string) bool {
	given, _, _ := strings.Cut(strings.TrimPrefix(a, "--"), "=")
	for _, name := range names {
		if given != "" && strings.HasPrefix(name, given) {
			return true
		}
	}
	return false
}

// accountedGitShortCluster reads a cluster of short options (-abc): the letters up to
// and including the first that takes a value (one of valueShort), and whether
// that value is the next argument.
func accountedGitShortCluster(a, valueShort string) (letters string, next bool) {
	for i := 1; i < len(a); i++ {
		if strings.IndexByte(valueShort, a[i]) >= 0 {
			return a[1 : i+1], i == len(a)-1
		}
	}
	return a[1:], false
}

// accountedGitHasShort reports whether any short-option cluster in args (before --)
// holds one of letters, reading values as accountedGitShortCluster does.
func accountedGitHasShort(args []string, letters, valueShort string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") {
			cluster, _ := accountedGitShortCluster(a, valueShort)
			if strings.ContainsAny(cluster, letters) {
				return true
			}
		}
	}
	return false
}

// accountedGitEnv is the value of name in env (the last entry wins, as for exec).
func accountedGitEnv(env []string, name string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if value, ok := strings.CutPrefix(env[i], name+"="); ok {
			return value
		}
	}
	return ""
}

// accountedGitJoin is p resolved against dir, as git -C resolves it.
func accountedGitJoin(dir, p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(dir, p)
}

// accountedGitChdir is the directory git -C p changes to from dir, without
// symbolic links: the kernel follows a link before a later "..", which a
// lexical join does not. A directory that does not exist is joined
// lexically; git fails to change to it.
func accountedGitChdir(dir, p string) string {
	if !filepath.IsAbs(p) {
		p = dir + string(filepath.Separator) + p
	}
	if physical, err := filepath.EvalSymlinks(p); err == nil {
		return physical
	}
	return accountedGitJoin(dir, p)
}

// accountedGitFirstLine is the first line of what a failed git call said,
// quoted for a refusal: its standard error when it exited, else err.
func accountedGitFirstLine(err error) string {
	var exit *exec.ExitError
	if errors.As(err, &exit) && len(bytes.TrimSpace(exit.Stderr)) > 0 {
		first, _, _ := strings.Cut(strings.TrimSpace(string(exit.Stderr)), "\n")
		return accountedGitQuote(first)
	}
	return accountedGitQuote(err.Error())
}

// accountedGitQuote quotes s for a refusal, bounded to 80 bytes.
func accountedGitQuote(s string) string {
	if len(s) > 80 {
		s = s[:80] + "..."
	}
	return strconv.Quote(s)
}
