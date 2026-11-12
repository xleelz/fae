# Opt-in live execution

Direct and multiplexer modes run a configured **trusted agent bridge**, real Go test commands,
and guarded local Git effects in registered task worktrees. No provider account
is selected automatically. The bridge protocol is documented in
[execution/README.md](execution/README.md). It must enforce the role's provider
and tool profile; this is not an OS sandbox for hostile agents. A stock agent CLI
cannot simply replace the bridge executable because handoff and profile contracts
must be implemented by the wrapper. The included `cmd/fae-claude-bridge` (the
default provider) and `cmd/fae-codex-bridge` supply restricted Claude Code and
Codex wrappers: model processes have no command or file tools, and the bridge
validates declared-file edits before applying them. It applies a turn's edits
all or nothing: when a write fails, it puts back every file it had written and
records the typed refusal `edits-not-applied`, which the executor retries as
ValidationFailed, charging no provider. It is still a trusted bridge, not a
general hostile-agent sandbox. See [provider/README.md](provider/README.md).

A Claude target pins the bridge and an explicit credential source, for example:

```json
"command": ["/absolute/path/bin/fae-claude-bridge", "--claude", "/absolute/path/to/claude", "--config", "{config}", "--files", "answer.go,answer_test.go", "--test-file", "answer_test.go"],
"environment": {"CLAUDE_CONFIG_DIR": "/absolute/home/.claude"}
```

and workflow routes such as `{"provider":"claude","model":"claude-opus-5-5","effort":"medium"}`.
`serve` defaults Prophet to `--prophet-provider claude --prophet-model claude-opus-5-5`.

Create a dedicated, un-checked-out integration branch from the full commit SHA
selected as the base. Supply a JSON configuration like this with real paths:

```json
{
  "command": ["/absolute/path/to/trusted-agent-bridge", "{config}"],
  "environment": {},
  "allowed_commands": [
    ["go", "test", "-json", "-count=1", "-a", "-x", "./..."],
    ["gofmt", "-l", "."]
  ],
  "files": ["answer.go", "answer_test.go"],
  "base": "FULL_IMMUTABLE_COMMIT_SHA",
  "integration_ref": "refs/heads/fae/integration",
  "commit_message": "Implement verified task",
  "test_file": "answer_test.go",
  "test_id": "TestBehavior",
  "expected_failure": "specific expected assertion failure",
  "host_ledger": "/absolute/physical/path/shared-by-all-instances/host-leases",
  "heavy_capacity": 2,
  "timeout_seconds": 300
}
```

The file must be an owner-only (0600) regular file outside every repository and
worktree; `serve` refuses it otherwise, since `environment` may carry credentials.
The `go test` vector is the default workflow's test command: the broker narrows
RED and green runs to `-run=^TestID$` at heavy weight 1, and QA runs the whole
suite at weight 2 with package and test parallelism capped at one.

### Task scope

A task's scope is the files its changes may touch and its RED identity (test
file, test id, expected failure). The example above pins one scope for every
task. Leave out `files`, `test_file`, `test_id` and `expected_failure` to take
each task's scope from the task itself instead: one running instance then takes
any number of new tasks with no configuration edit or restart. The executor
resolves a task's scope in this order:

1. a `tasks[id]` override (see below);
2. non-empty instance `files`, with the instance `test_file`, `test_id` and
   `expected_failure`;
3. derived scope. The files are the admission's declared files
   (`fae run --declared-file PATH`, repeatable), else the `declared_files` of
   the task's latest Spec, plus the files an approved scope widen adds. The RED
   identity is the latest RedReport's `test_id` and `expected_failure`; its RED
   file is the one `*_test.go` path in the report's `declared_files` that is in
   scope. Before the task has a RedReport, all three are empty.

With empty `files`, an instance-level `test_file`, `test_id` or
`expected_failure` is refused at startup and by `fae doctor`. Derived paths,
approved widens, RedReport paths and `tasks[id]` files follow the one path rule
that `fae run --declared-file` and admission also apply: relative, clean,
unique, and with no `.git` component at any depth, so `sub/.git/config` is
refused as well as `.git/config`. Instance `files` and `test_file` follow it
too, apart from uniqueness, at startup and in `fae doctor`. An unsafe declared
or RedReport path refuses the dispatch before launch, charging no attempt, and
is never dropped.
The engine refuses a scope-widen request that names a path in `.git` as
malformed output, so the judge never sees it, and the bridges refuse such a path
pinned by `--files`, `--test-file` or `--task-scopes`, or named in the
`task_scope` block or the stage block's `approved_scope`, before the provider
runs. The VCS broker passes a task's file names to git as file names, never
as pathspec patterns or magic: every git call it makes runs with
`GIT_LITERAL_PATHSPECS=1`, and the derived scope's `ls-tree` with
`--literal-pathspecs`. A declared `*` stages only a file named `*`, and a
committed file named `:(exclude)*` is compared as itself when reconciliation,
a conflict check or integration's containment check compares the task's
files with upstream (before, it excluded every file, and integration reported
a commit the ref never received as landed).

A scope a model chose never holds a protected path (FAE-SEC-005,
`workflow.ProtectedPath`): CI and forge configuration at the repository root
(`.github/`, `.gitlab/`, `.gitlab-ci.yml`, `.circleci/`, `.buildkite/`,
`.travis.yml`, `Jenkinsfile` and other pipeline files), configuration that
makes the operator's tools run programs (`.husky/`, `.pre-commit-config.yaml`,
`.envrc`, `.devcontainer/`, `.vscode/`, `.claude/`, `.codex/`, `.cursor/`,
`.mcp.json`), a `.gitmodules` file, and credential-like files at any depth
(`.ssh/`, `.aws/`, `.env` and `.env.*` but not `.env.example`, `.netrc`,
`.npmrc`, private keys and key stores such as `id_rsa` or `*.pem`, except key
fixtures under a `testdata` directory). The engine refuses a scope-widen request
that names one as malformed output. When a task's Spec declares one, the path
is left out of the task's files, so no turn reads or sends it, and every change
turn, reconciliation and commit of the task is refused before launch, naming
the path (no attempt is charged, and the fourth such refusal in a row at a
stage escalates SameGate); turns that change nothing still run, so a restart with
information lets the spec declare the files again without it. The operator can
still give a task such a file: the files declared at admission
(`--declared-file`) carry the operator's authorization, and a pinned scope is
configuration, so the rule does not judge either.

A malformed request (a `.git` widen, an unknown field, a widen with no files)
is refused as MalformedOutput and the turn runs again. When that turn also
changed files, its edits, which passed the bridge's and the executor's change
checks, become the task's tree: the retry starts from them, and restart and
Resume still match the workspace. Four refusals in a row for the same reason
escalate SameGate, which a restart with information recovers.

An approved widen file the bridge can't load into a turn (a symlink anywhere
on its path, a directory or other non-regular file, a file over 1 MiB, or one
that would take the turn's files over 4 MiB) doesn't refuse the turn: approved
scope is never removed, so that refusal would recur on every later turn. The
bridge lists the file in an `unavailable_scope` block with the reason, never
reads it (it doesn't follow the symlink), doesn't send it, and refuses an edit
to it in that turn. That holds for the turn's RED test file too when a widen
brought it in: the RED identity doesn't change, the engine still runs the RED
test from the worktree, and a test-first turn can only report the RED as it
stands. A declared file the bridge can't load still refuses the turn before the
provider runs.

A task stored with an approved widen path that a widen may no longer add (a
nested `.git` widen approved before wave 9, such as `sub/.git/config`, or a
protected path approved before wave 11) still loads. Before its next dispatch
the engine drops the path from the approved scope, with a `scope-dropped`
event that names it and why, and the task continues on the rest of its scope;
its widen count is unchanged. A landing or sub-workflow that reads the task
before then leaves the path out the same way.

A derived RedReport must name exactly one in-scope `*_test.go` file among its
`declared_files`, a Go test name (`^Test[A-Za-z0-9_]*$`) as `test_id`, and an
`expected_failure` that says why the test fails. The broker matches the fragment
literally against the RED test's failure text, for pinned and derived
identities alike: the test's own output without the goroutine stacks in it,
with go test's framing lines (`=== RUN`, `--- FAIL:`) removed. A stack is left
out from its `goroutine N [state]:` header through its frames, both the dump a
panic or timeout prints as the test binary ends and a stack printed while the
test goes on (net/http's log of an `httptest` handler's recovered panic,
`debug.PrintStack`), so the test's own later message still matches. A stack
names the module, the worktree's path and the source files whatever the reason,
so nothing in it matches. A derived fragment is refused when nothing in it goes
beyond what that text holds for any failure: the test's name or part of it, the
RED file's name, a `file:line` position or any end of one (`go:9` matches every
position on line 9), go test's framing (`=== RUN`, `--- FAIL:`, a duration,
`exit status N`), a panic's generic text (`runtime error`, `goroutine N`, stack
frames), the words go test and the runtime print around any failure (`fail`,
`ok`, `panic`, `recovered`, `running`, `test`, `go`, `in`, `local`, `name` and
the others the protocol lists, the bare labels `got`, `want`, `expected` and
`actual` among them), or numbers alone (`1`, `42`, `0x`, `0xea`). `error` alone
is refused because it lies within `runtime error`, but beside a word that isn't
on that list it is a reason: `expected error` is the whole message of
`t.Error("expected error")`. A number counts beside a word or a comparison
(`got 0 want 42`, `want 42`, `0 != 42`). Anything else counts as a reason: a
message, also one whose words make up the test's name (`divide by zero` for
`TestDivideByZero`; a name never holds a space), a path in a message
(`testdata/missing.json`, `want application/json`), a word of the RED file's
directories (`overflow` for `internal/overflow/parse_test.go`), a panic's own
message (`assignment to entry in nil map`, `SIGSEGV`) or a timeout (`test timed
out`). A word of the RED file's own name stays refused (`config` for
`config_test.go`): every logged line's `file:line:` prefix prints it. The rule
reads only the fragment, the test id and the RED file, so it does not catch
text the test logs or prints itself before it fails for another reason; the
reviewers judge that. The rule is applied when the report's turn ends and until
its RED freezes; a frozen report's fragment, which the RED gate proved, is not
judged again. The bridges' output protocol states the rule in every turn's
kernel contract, with the list of refused words, and the shape the RED gate
needs: one top-level test without `t.Run` subtests (each subtest counts as an
executed test, and the gate needs exactly one), which reports its own failure
and fails rather than hangs. A derived RedReport whose test function calls
`t.Run` on its own `*testing.T` is refused when its turn ends, like a vacuous
fragment; subtests started through a helper are left to the RED gate. A pinned
`expected_failure` is the operator's and is not held to the reason rule.

A RED that hangs still fails inside its run. For a shaped test run (`go-unit`,
`go-integration`) the broker adds go test's own `-timeout` at half the stage's
deadline, unless the command vector sets one: 300 s of the default targeted
stage's 600 s. go test then prints `panic: test timed out after 5m0s` and lists
the hung test under `running tests:` as that test's output and, in Go 1.27,
reports only the package's failure. The broker records a test that alarm names
as executed and failed. A test cut off any other way while it ran reported no
failure of its own and is not counted as executed or matched, whatever it or
anything else printed meanwhile: a test whose binary `os.Exit`s (`log.Fatal`
included) or panics outside the test's goroutine, one a `TestMain` goroutine
ends, one killed by a signal from outside its test binary (an OOM kill prints
`signal: killed` under it), and one still running when the broker kills the run
at the stage's deadline. That run is a failed run: the RED gate fails it into
the RED loop and QA into remediation, and the rework turn's excerpt shows the
test's output with a line saying it was not counted. A run the broker kills at
the stage's deadline has exit code -1, keeps its output and proves no expected
failure. A run whose output goes over `max_output_bytes` is a failed or passed
run by what it kept: its exit code stands, its test stream counts only the
results reported before the cut, and its excerpt says the output was cut. None
of these parks the task `UncertainDispatch`. A test stream that can't be read
(a test without a result in a package that passed, or no result for the
package, as when the go process itself is killed) is still refused as
incomplete and leaves the run uncertain.

The report is checked when the turn that writes it completes. A malformed one
is refused there as ValidationFailed: the turn's RED file is put back as it was
when the turn started, and the engine runs the turn again, whose operator
context says why the report was refused. A retry spends an attempt but no test
run. The same refusal four times in a row escalates SameGate, and the task
attempt limit bounds the rest. A recorded RedReport can still turn out broken,
for example when a restarted task's new Spec puts a second test file it names
in scope. The next test-first turn then runs as usual and replaces it. Until it
does, a dispatch that needs the RED identity (a targeted test run, freeze,
compare, or a change turn held to the frozen RED) is refused before launch,
charging neither an attempt nor a test run, and a suite run or merge refuses
rather than ignore the broken report. A derived RED is therefore a Go test; script and runner test
shapes need a pinned `tasks[id]` identity, whose `test_id` may be any literal
identifier.

A scope widen approved after the freeze that adds a second test file the frozen
RedReport names does not break it: a frozen RED file among the declared files
stays the RED file. A frozen RED file that itself came in with an earlier widen
is the exception: after such a second widen, implement is refused before
launch, charging nothing, until the fourth refusal escalates SameGate, and only
a restart with information, which
runs the RED loop again on the test runs left, recovers the task.

A derived identity is model supplied. The engine's mechanical checks hold it,
not operator pinning (FAE-WF-073/076): the RED gate needs exactly one matched
test that fails for the declared reason and is present in source, freeze and
compare hash the declared test function, and every later change is held to the
frozen identity. Once a RED is frozen, the RedReport it froze stays the
identity; a later RedReport counts only inside a new RED loop, which must
freeze again. Before a task has a RED file, or while its latest RedReport is
broken, its test-first turn may change at most one file, in scope and named
`*_test.go`; after that, only the RED file. A declared file the task has not
created yet is left out of change snapshots until it exists. A SubWorkflow child
task is admitted with its parent's resolved files as its declared files, so a
derived-scope child works within its parent's scope.

Every agent turn, reviews included, gets a `task_scope` brief block after the
`stage` block: `files` (sorted, and `[]` while the scope is unknown, as at
intake when the admission declared none) and `test_file` (`""` until the task
has a RED file). Pinned scopes are reported the same way. A bridge serving
derived scope takes its edit scope from that block, so leave `--files`,
`--test-file` and `--task-scopes` out of its command vector: a pinned bridge
scope wins over the block.

A turn whose inputs include a failed host command run also gets an
`engine_feedback` block after `task_scope`: a bounded excerpt of that run's
output, such as compiler errors or failing tests. See "Engine feedback" in
[execution/README.md](execution/README.md).

An empty `commit_message` builds each task commit's message from the task: its
title, else the first non-empty line of its content (trimmed, at most 72
characters), then the trailers `Fae task: ID` and `Fae dispatch: ID`. A
non-empty `commit_message` is used as before, with the dispatch trailer.

Start from the canonical bound repository root:

```sh
./bin/fae serve --workspace "$PWD" --instance live --adapter direct \
  --live-config /absolute/path/target.json --foreground
```

`serve` defaults to `--adapter multiplexer`. Without `--foreground` it detaches
once ready and logs to `HOME/INSTANCE.serve.log`. `--max-panes N` and
`--max-heavy-weight N` override `pane_limit` and `heavy_capacity` (2 when
neither is set); `--max-tasks-in-flight`, `--max-molecules` and
`--max-workflow-molecules` default to -1 (unbounded);
`--default-mode normal` makes Normal dry-run the disposition of submissions that
state none.

`process_limit` caps every brokered host command run (FAE-HOST-015): each runs in
a transient `systemd-run --user --scope` with `TasksMax` (cgroup v2 `pids.max`,
which counts processes and threads) of 512 by default (0 or omitted); `-1`
disables it. A probe scope at start must show the configured `pids.max`, which
needs Linux with the pids controller delegated to the user manager and a value of
at least 8; otherwise brokered runs are uncapped and the gap is logged in the
supervision log. See [effects/README.md](effects/README.md).

`nested_commands` decides what happens when a brokered host command runs a
nested go, gofmt, git or other build tool (FAE-HOST-014). `"refuse"` (the
default when empty) makes every nested call that reaches a wrapper exit 77,
except a Local task's nested `git push`, `git remote` (or `send-pack`,
`http-push`), which exits 78 (FAE-MODE-002). That includes the go and gofmt a Go test starts: go test puts `$GOROOT/bin`
first on its tests' PATH, so the broker runs go with `GOROOT` set to a per-run
mirror of the real GOROOT whose `bin/go` and `bin/gofmt` are the wrappers, and
with `GOTOOLCHAIN=local` and `GOENV=off`. A module whose go line needs a newer
Go than the one on the PATH therefore fails instead of switching toolchains.
A `GOTOOLCHAIN` set in a HostCommand stage's `environment`, or in the live
config's `environment` for a role session's go, is replaced by `local`,
without a warning. `GOENV=off` also drops the operator's `go env -w` settings
(`GOPROXY`, `GOPRIVATE`, `GOFLAGS`, ...) for every brokered run, the top-level
go included. A run's environment is the daemon's `PATH` and `HOME`, `TMPDIR`
(the run's on-disk scratch), `LANG=C`, Git's isolation settings and the
HostCommand stage's `environment`; no Go setting of the daemon's reaches it.
So a Go setting a target needs, such as `GOPROXY` or `GOPRIVATE`, belongs in
the stage's `environment` (a custom workflow revision). The mirror is sealed read-only and removed with the run.
So is the run's TMPDIR, even when a test left a read-only tree in it (Go's
module cache under its own `GOPATH`); one the broker cannot remove is logged,
and the next start removes it, or moves it to `<instance>/scratch-quarantine`
and logs that; status (`startup_checks.scratch_quarantine`) and a
`scratch-quarantine` warning alert report what the quarantine holds.
When the go on the daemon's PATH sits outside any GOROOT (a version manager's
shim), runs get no mirror and a Go test's own go reaches the real toolchain:
the daemon logs that gap at start and `fae doctor` warns; put a go that sits
in its GOROOT first on the daemon's PATH, or set `GOROOT` in the stage's
`environment`.
`"account"` records each call and passes it through the nested-command policy,
an allowlist like a top-level engine command's with limits of its own, then
runs it within the parent run's process group, task cap and heavy lease,
taking no lease of its own. Nested `go build`, `test`, `vet` and `list` are
clamped to `-p=2` (1 for a whole-suite test, `go test all` included, and one
whose `-run` selects every test, such as `-run=` or `-run=.*`), take
only listed flags and are confined to the run's worktree, TMPDIR and an
on-disk `/var/tmp` (where Fae's own tests keep fixtures). They may not change
the programs go runs (`CC`, `CGO_*FLAGS`, `PKG_CONFIG`, ...) or any Go setting
the policy does not know, run tests at more threads than the clamp (`-cpu`,
`GOMAXPROCS`) or hand the test binary an output flag after `-args`. The C
compiler, C++ and Fortran compilers and `pkg-config` that go finds by name
are pinned to where the run's own PATH finds them, so a nested cgo build never
runs a `gcc` a test puts first on its PATH, and a call whose PATH has one the
run's PATH lacks exits 77; nested
`gofmt` gets the formatter limits; other
build tools exit 77. Nested `git` may change only a repository in the run's
TMPDIR, nested in its worktree, or in `/tmp`, `/var/tmp` or `/dev/shm`, whose
`.git` holds no symbolic link git would write through to anywhere else (such
as `.git/refs` linked into the workspace's) and whose references are kept
nowhere else; files a call names for git to write (`--output`,
`format-patch -o` or its `format.outputDirectory`, `bundle create`, ...) must
lie there too, and so must every worktree `git worktree repair` would
rewrite, the ones the repository lists included. Each path is checked as the
system takes it when git creates and writes it, never cleaned as text: a
symbolic link is followed, a directory git would create counts as one, a
`..` leads up from where the previous component lies (so
`scratch/n1/../l/../x`, with `l` a link into the workspace's `.git`, is the
workspace's `.git/x`), and a link leading nowhere is refused; a link switched
between the check and the exec is the race documented below. The run's own repository (the workspace, whose refs, configuration and hooks every
task worktree shares) and any repository elsewhere may only be read: `status`,
`diff`, `log`, `rev-parse` and other read-only commands, setting no
configuration beyond a few harmless `-c` keys. A push or `git remote` call
anywhere, and a `remote.*` or `url.*` configuration write to a protected
repository, exit 78 in a Local task and 77 otherwise. Everywhere, 77 also
refuses aliases, unknown git commands, mail and server commands, and what a
call itself gives git that names a program for git to run: options
(`submodule foreach`, `rebase --exec`, `--upload-pack`, `init` or
`clone --template`, a merge strategy git does not have), settings
(`core.sshCommand`, `core.hooksPath`, `credential.helper`, `filter.*`,
`*.command`, `include.path`, any `hook.*` setting but one that switches a hook
off, and the rest) given with `-c`, `--config-env` or `GIT_CONFIG_COUNT`, in a
global or system file the call names, or written by a nested `git config`,
and variables (`GIT_SSH_COMMAND`, `GIT_EDITOR`,
`GIT_EXTERNAL_DIFF`, ...); any `GIT_*` variable the policy does not know
(`GIT_TEST_FSMONITOR`, which `git status` runs as a program) exits 77 too.
Nested `git` reaches only local
repositories: a URL the call names that is not a path or `file://` exits 77,
and every call runs with `GIT_ALLOW_PROTOCOL=file`, so git itself refuses a
remote whose configured URL, or `url.<base>.insteadOf` rewrite, is off the
host, and with bundle URIs off. It runs with `GIT_CONFIG_GLOBAL=/dev/null` and
`GIT_CONFIG_NOSYSTEM=1` unless it names those files itself, so a `HOME` or
`XDG_CONFIG_HOME` the call points elsewhere loads no configuration; with no
hook and no file-system monitor (`core.hooksPath=/dev/null`,
`hook.<event>.enabled=false`, `core.fsmonitor=false`, which outrank every
configuration file, so a test's own repository's hooks do not run either; git
reads a call's own `-c` after them, which is why a call's `hook.*` setting
that would switch a hook back on is refused);
and with git's trace2 targets off unless the call sets them, so a
configuration file cannot send a trace into the workspace. A call on a
repository runs pinned to the canonical working directory, git directory and
work tree the policy checked, so a symbolic link switched afterwards cannot
redirect it. The policy
runs in the `fae` binary
(`fae heavy-op run --shim-policy`), so setup refuses account mode when the
daemon runs as anything else. Each run's calls are stored as marker
`nested-commands/<run id>`, one per run, keyed like its engine run row by
dispatch, command and start and naming the task and dispatch. Use it when a
command needs nested calls, as Fae's own test suite does when its tests create
Git repositories and build binaries. It is not a sandbox: a test can still run
a tool by absolute path, and programs git itself starts run the real git, not
the wrappers. Those include the programs a test's own repository's
configuration names when the test wrote it there itself (a filter or diff
driver, an editor), and the workspace's own configured programs (a diff
driver) on a read. A test's submodules are not checked, and a process that
changes a repository between the policy's check and git's start (renames a
directory, plants a link in `.git`) is not seen: re-audit 10's loop toggling a
test repository's `.git/refs` to a link into the operator's got 36 of 150
`update-ref` calls through. See
[commandshim/README.md](commandshim/README.md#command-policy).
`max_output_bytes` bounds a run's stdout and stderr together;
it is 4 MiB when omitted and must be between 1 MiB and 64 MiB. Output past it
is cut: the run's exit code stands, its test stream counts only the results
reported before the cut, and a `gofmt -l` listing cut there counts as new
format debt. Setup refuses any
other value of either key instead of correcting it, since they are pinned
configuration, not flags; `fae doctor` reports both.

For Herdr, add `"adapter": "multiplexer"` and
`"herdr_binary": "/absolute/path/to/herdr-core"` to the target configuration, and use
`--adapter multiplexer` instead of `--adapter direct`. Each turn uses a private
`herdr-core` server (`herdr-core serve` / `herdr-core server stop`), session (`HC_SOCKET`, `HC_SESSION`, `$XDG_CONFIG_HOME/herdr-core/config.toml`) and gated pane (`herdr-core run --bg --no-parent --no-notify --name <name> --cwd <cwd> --json`, `herdr-core get <address> --json`, `herdr-core kill <address> --json`, `herdr-core rm <address> --json`); ambient Herdr selection and user configuration are
ignored. No existing pane is adopted. See [runtime/MULTIPLEXER.md](runtime/MULTIPLEXER.md).
The direct and Herdr adapter choices are part of the pinned target configuration.

The shipped default workflow runs live unpatched: its HostCommand vectors are the
allowlisted `go test` and `gofmt` vectors above, and its routes try Claude, then
Codex. On an instance whose bridge serves one provider, the other provider's
bindings are refused when reached and the turn moves on (see Health below).
Install a custom revision through `template workflow-put` to narrow the routes
to the bridge's provider, or when the target needs other commands (`workflow default
--local --json` prints the Local projection, which `scripts/dogfood.py` installs).
An existing instance upgrades its default at start: the shipped content becomes a
new audited revision when the latest stored one fails current validation or is an
older daemon-installed generation; a valid operator revision or a retired default
is kept. Status `default_workflow` reports the outcome; tasks keep their pinned
revisions. The real broker requires compiler evidence for fresh-build claims;
`-count=1` alone only disables cached test results. Use the standard message,
workflow-put, run, history and events verbs with `--instance live`. An operator
message must authorize each mutation.

The target configuration hash is pinned in the instance database. Restarting
with a different configuration refuses rather than silently changing the trust
boundary. Base must be an immutable commit SHA; caller-supplied task tree hashes
are replaced with the actual owned worktree tree at live admission. Each instance
must use the same physical host-ledger directory and capacity to share heavy-op
admission. Conflicting ledger capacity refuses. A workflow-global role pool is
shared by every revision of its workflow and uses the cap of the newest revision
with a live task; a task-bound pool is shared by the tasks of one workflow
revision. A changed cap governs future grants only; status reports
`scheduler.role_caps` (see [scheduler/README.md](scheduler/README.md)).

Each bridge session is a fresh owned process group. Environment inheritance is
restricted, session files are private, handoffs are bounded and identity checked,
and timeout/cancellation retires owned descendants. Host commands use literal
allowlisted argv, bounded output and a timeout. The actual test file supplies RED
identity, not an agent's source report. RED turns may change only that test file;
implementation must preserve both its frozen function identity and all test-file bytes. All changes must stay within
the task's scope, pinned or derived, before they can produce accepted evidence.

Commits use a temporary index and the exact reviewed whole-tree bytes. Integration
updates only a dedicated, un-checked-out `refs/heads/fae/*` branch with compare-and-
swap. Ancestor, patch-equivalent, all-touched-path blob, and empty-change
containment report the containing tip without moving it. No developer branch is
automatically merged.

When the target advances, merge prepares an immutable three-way plan, checks
that every own hunk remains, persists the plan, and applies it only to the task
worktree. The current default workflow repeats tests, frozen-test comparison,
format checks, reviews, Oracle and QA on the merged tree before commit. A known no-write integration-tip race takes a bounded reconciliation retry,
including the already committed own delta and fresh approvals; an unknown write
outcome remains uncertain. A further ref race cannot land unverified code. Supported text conflicts enter bounded implementation remediation with named
paths and an immutable original-change contract, followed by fresh tests and
approvals. Missing original additions/deletions, frozen-test conflicts and unsupported
file conflicts refuse. A second target advance while an original conflict contract
is active requires operator review rather than replacing that contract. Restart adopts an interrupted local host or VCS effect automatically, without charge, only from a completed daemon-written receipt with retired workloads and exactly remeasured postconditions (FAE-ROB-016); anything else stays uncertain. Partially upstream own work
can conservatively refuse if its diff shrinks rather than silently discard hunks.

Failed or interrupted live effects park as uncertain and are never blindly
replayed after restart. An effect that never started is not among them: a
failure before the broker's command workload ran (its worktree, vector,
timeout, shape, policy, scratch, wrappers, executable or launch gate; `RefusedBeforeExec`),
a verification worktree, test source or RED identity that could not be
prepared for a run, freeze or compare, an integration lacking its commit, an
intent or launch boundary that could not be recorded, and a merge refused before
applying its plan (tree measurement, active conflict verification, snapshot,
plan preparation or missing reverify route) are refused before launch, refunded, with the refusal
recorded against the effect intent for recovery; a commit refused before any
object or ref was written (active conflict verification, the commit floor, a reviewed snapshot or prepared
commit the broker could not produce) escalates for the operator, refunded. A host command that ran to its end is not among them,
whatever its exit status: a test that failed, a go test whose test binary ended
while a test ran (that test is not counted as executed), a command the broker
killed at its stage deadline and one whose output went over the bound are runs
the workflow routes. A run interrupted by a stop, a test stream that can't be
read (the go process itself killed) and a changed tree stay uncertain. Successful completed stages remain durable. Graceful stop
waits for workers to retire before closing the database. Exact process birth and
group identities are recorded before workloads are permitted to run. Parent-death
watchdogs handle proved owned groups after SIGKILL, and startup recovery runs
under the instance lock before new execution. Records and retirement receipts
support cleanup audit. Foreign, reused, unreadable or leaderless ambiguous groups
refuse recovery; a dead parent alone never proves that a resource lease is free.
A fsynced admission journal links each permit to its exact owner and session.
Startup can reclaim never-launched reservations, completed retirement records,
or a complete expected set of linked watchdog retirement receipts. Role and host
release replay safely across a partial two-store update. Live, foreign, missing
or partial process evidence retains capacity. See
[processowner/README.md](processowner/README.md) for the stopped-launch window,
Darwin signal boundary and escaped-descendant limits. Ledger audit history has a
bounded size and fails closed when archival is required.


## Planned restart at a durable boundary

`fae drain --authorizing-message ID` stops new admissions and stage dispatches.
Active turns continue through validation and durable collection. Poll `status`
until `snapshot.drained` is true, then call `shutdown` and launch the same pinned
instance configuration. The drain flag clears on startup; completed turns keep
their provenance and remaining stages resume. A parked uncertain dispatch does
not count as drained. This explicit planned-restart path does not adopt unknown
effects. `shutdown` without a drain cancels running turns except commit and
integrate steps, which may finish within 120 s, and its clean-stop marker lists
the interrupted turns. On restart such a live turn (agent, review, host command
or freeze/compare) is replayed without charge only after its unchanged workspace,
session retirement and absence of a completed effect receipt are proved;
otherwise it parks as uncertain.

For independent task scopes, target configuration may pin a `tasks` map keyed by
task ID, each containing `files`, `test_file`, `test_id`, and `expected_failure`.
The included bridge accepts a matching `--task-scopes` JSON literal inside the
pinned command vector; no mutable external scope file is trusted. Derived scope
(see Task scope) serves new tasks without pinning each one.

## Real-provider dogfood

The recorded two-task acceptance passed using Codex `gpt-6-astra`, Herdr 0.9.5
and private Dolt. Both tasks generated genuine RED tests and implemented separate
code changes. After both reached specification review with two artifacts each,
the harness drained active turns, stopped the daemon and restarted the same
instance. Both checkpoint chains remained byte-for-byte unchanged in final history.

The tasks reached Done with 35 artifacts/version 83 and 22 artifacts/version 52.
The first lost the integration race and exercised the bounded reconciliation
retry, repeating tests, reviews and QA before landing both contributions. The
host capacity was 2; 58 grants matched 58 releases, with no remaining holders or
waiters. The developer checkout stayed unchanged. Complete chains, restart
checkpoints, ledger audit and tool versions accompany the machine-readable
[result](verification/results/herdr-dogfood/summary.json).

With a logged-in Claude Code CLI, reproduce this opt-in run using:

```sh
go build -o bin/fae ./cmd/fae
go build -o bin/fae-claude-bridge ./cmd/fae-claude-bridge
go build -o bin/fae-ui ./cmd/fae-ui   # optional operator UI; serve --ui runs it from beside bin/fae
python3 scripts/dogfood.py --dolt /absolute/path/to/dolt
```

The harness defaults to `--provider claude --model claude-opus-5-5` and pins
`CLAUDE_CONFIG_DIR` (default `~/.claude`, override with `--claude-config-dir`).
Use `--provider codex --model MODEL` with `bin/fae-codex-bridge` for Codex. It
accepts `--claude`, `--codex` and `--herdr` (`herdr-core`) paths when those binaries are not on
PATH. It uses the existing provider account and performs model calls. This exercises two genuine code-bearing tasks with a planned drained restart. Persistent interactive sessions,
general brokered agent command tools, complete cross-user host inventory and
native vendor transports for Normal-mode effects remain open.

Protocol-fixture tests remain separate evidence: they exercise real host, Git,
Dolt and lifecycle behavior without contacting a model provider. Neither those
tests nor this real-provider run establish every phase acceptance criterion.

## Health, command policy and pane reservations

Live dispatch selects the first eligible provider/model in each role's ordered
route, persists that selection, and records it in artifact provenance. An unknown
route starts at 100; penalties, one-hour half-life healing and the post-throttle
success hold determine subsequent eligibility. No eligible route parks without
charging an attempt; the daemon periodically rechecks, and a park lasting 30
minutes escalates `ProviderUnavailable`. A launched turn that fails as
rate-limited, unavailable or auth-failed, with its processes retired and the tree
unchanged, rotates once per turn to the next eligible route without charging an
attempt (a review dispatch whose earlier reviewers already ran stays charged,
and every reviewer runs again); a repeat counts as a stage failure. A provider answer the bridge
refused (malformed output, charged to the binding) and a provider that ended
without an answer before the bridge wrote anything (`provider-failed`, charged
from its log, malformed output when the log has no structured error) retry the
stage within its bound, each spending an attempt. A turn the bridge refused
before the provider ran, like any dispatch refused before anything started,
charges no attempt, test run or binding, and the fourth such refusal in a row
at a stage escalates SameGate (see execution/README.md). The relaunches are
bounded whatever another task's turn heals in between: a turn's agent starts
over every binding are counted (3 per binding the stage's roles declare),
and reaching that total escalates ProviderUnavailable naming the
bindings; a first-session admission deferral launches nothing and stays a
traced park re-decided each round without failing the task (FAE-HOST-006),
while a later review session whose admission is deferred asks again for up
to 30 minutes before the dispatch gives up the reviews that ran, and 60 such
later-session deferrals in one turn escalate ProviderUnavailable naming the
host condition. A binding the bridge cannot run as this instance
configures it (a Codex binding on a Claude bridge, an effort Claude Code lacks,
no credentials in the target environment) is refused before its provider runs:
the turn moves to the role's next route without charging an attempt, a
rotation or provider health, and the task skips that binding until the daemon
restarts or the operator resumes or restarts the task. A refusal that comes
after earlier reviewers of the same review dispatch ran charges that dispatch's
attempt, and the stage runs every reviewer again. Each such refusal is a
`route-refused` event with a paired `component=task` line. When the bridge has
refused every route of a role, the task parks like any task without an eligible
route: its reason (`all-routes-unavailable: every route for role ... was
refused ...`, shown in status and withheld as `route_refused`) names each
refusal and what to change. Nothing is charged. The daemon restart that starts
a new route generation resumes the task at its stage, and so does
`fae resume --task`. The 30-minute route-exhaustion bound counts from the first
park until a route runs, so a restart whose retries are refused again does not
restart it, while `fae resume` does; past the bound the task escalates
`ProviderUnavailable`, and the episode raises one `route-exhaustion` alert.
Refused routes beside unhealthy ones park as above.
Installing a workflow (and `workflow validate`) refuses the `minimal` effort on
a Claude route or in a task class overlay that applies to one; a revision stored
before that rule still loads, and the bridge refuses such a binding as above.
Any other launched failure can leave
uncertain edits, so that turn parks rather than being replayed on a fallback.

Inspect effective scores and the transactional operator log with:

```sh
fae provider-health --home /absolute/instance-parent --instance NAME
fae provider-log --home /absolute/instance-parent --instance NAME --after 0 --limit 100
```

`provider-log` returns audit sequence cursors. Each observation commits its row,
log line and event together; repeated collection of the same observation does
not duplicate them. This durable log is the authoritative record, rather than
an independently appended daemon text file.

Multiplexer live configuration accepts `pane_limit` (default 60). The limit must
exceed workflow-global role capacity plus one. All cooperating instances sharing
a host ledger must use the same pane limit. Herdr inventory is measured through
`herdr-core ls --json` (`HC_SOCKET`) for discovered server sockets; an unreadable census fails closed.
Reservations include not-yet-visible launches and memory warmup. Pane limits can be changed through the authenticated runtime configuration API,
with an authorizing operator message; other live-target settings remain pinned.

Supported Go host commands pass the literal allowlist and command policy under
the durable heavy lease. Agent sessions (AgentTurn, Review, Prophet and loop
turns) take a role slot and host/pane admission but no heavy-op unit, so heavy
capacity never caps concurrent sessions. Role sessions receive PATH-first shims
for go, gofmt, cargo, rustc, rustfmt, make, cmake, ninja, pytest and git. When the
daemon runs as the `fae` binary with a host ledger, each shim execs `fae heavy-op
run --shim-policy`, which runs a permitted build, test or format command under a
role-class heavy-op lease (FAE-HOST-011) and refuses the rest with exit 77
before any lease (pre-code roles, paths outside the own worktree, non-Go
wrappers, every git command, nested calls); lease and host refusals exit 75.
A Local session's `git push` or `git remote` exits 78 instead (FAE-MODE-002).
Without a broker or ledger the shims refuse outright, with the same two codes.
The bundled bridges disable command tools, so model edits and broker commands
remain separate stages. See [commandshim/README.md](commandshim/README.md) for
supported flags, formatter bounds and the limits of PATH enforcement.

## Dynamic pane configuration and recovery

```sh
fae message --id capacity-change --text 'Change this instance pane limit to 24'
fae runtime-config --pane-limit 24 --authorizing-message capacity-change
```

The audited value survives restart and is refreshed every dispatch round. Each
cooperating instance registers its requested cap with exact process ownership;
the effective shared cap is the minimum across live registrations. Reducing it
does not terminate existing sessions or invalidate their reservations: it defers
new admissions until capacity is available. The configured floor must exceed
workflow-global slots plus one. Status reports the effective dynamic cap and
which settings remain fixed for the process lifetime.

`stall_seconds` sets the positive-liveness inactivity window (default 300 seconds).
The runtime samples descendant CPU accrual, recognized structured session activity
and handoff growth; a quiet terminal alone is not a stall. An unreadable process
census cannot authorize termination. Use a larger `timeout_seconds` for the
whole-turn deadline so the inactivity detector has time to operate.

A positively proven stall is retried by default (FAE-RT-005) when the turn's
effective profile grants only handoff, read and edit, the capability set the
trusted bundled bridges enforce (`execution.RestrictedEffects`); `stall_safe_replay:
true` is now redundant but still accepted. Retry also requires complete
retirement, unchanged measured tree, successful observation persistence and lease
release, and no earlier executed session in the dispatch. Two durable retries are
allowed; the next stall yields `StallExhausted`. Route health may defer a retry
before then. Without that proof, the turn remains uncertain and is not replayed.

Idle-session shedding is available to trusted adapters through
`daemon.Config.ConfigureIdleSessions` and the owned idle-session registry. It
retires one eligible pre-code global after three own-idle saturated rounds,
protects active/final/QA sessions, and retains context for relaunch after TTL
retirement. The default bridge transport eagerly retires panes; opt-in resident transport is described below.
See [the adapter lifecycle contract](runtime/IDLE_SESSIONS.md); provider CLI processes are not retained; resident Herdr workers and context
continuation are described below.

The fresh [runtime-recovery acceptance run](verification/results/runtime-recovery-dogfood-20261017/result.json)
completed both real-provider tasks after changing the cap to 16 during active
work, preserving it across a drained restart, then raising it to 24. It matched
29 real provider turns to provenance and released all 58 host leases. This run
had no stalls; forced recovery and idle shedding have separate integration tests.
Use `--evidence-dir` with a new directory when running the harness to retain
checkpoint chains, audits, provider observations and verified file/source hashes.

## Persistent provider context and dynamic host configuration

Pass `--session-root /absolute/private/instance/provider-sessions` in the pinned
`fae-claude-bridge` or `fae-codex-bridge` command to enable persistent provider
context. Workflow-global
roles resume only explicitly recorded provider UUIDs. Workflow revision, role,
route, tool profile and mode form the isolation key. Each turn supplies its
current task header and current files; saved conversation content never grants
permissions or serves as workflow evidence. Task-bound roles resume within their own task and start fresh for another task.
Reviews always remain fresh. A durable pending marker invalidates uncertain context after a crash or
failed turn. Reuse of each exact context is serialized. By default Herdr processes/panes retire every turn; provider conversation history
persists independently.

The installed CLI's explicit `exec resume` path and structured events are checked
against the [official non-interactive documentation](https://learn.chatgpt.com/docs/non-interactive-mode).
Each continued turn records a private `provider-session.json` linking dispatch,
task, scope and provider UUID. The root must be private and physical; arbitrary
ambient sessions are never selected. Provider-neutral loops and resident Herdr workers can be enabled as described below.

`runtime-config` now accepts partial updates for `--heavy-capacity`,
`--tasks-in-flight` (-1 for unbounded, the default, or 1..1,000,000), and
`--host-thresholds` (a complete JSON Config), as
well as `--pane-limit`. An audited update preserves omitted values, survives
restart and is refreshed every round. Shared heavy capacity uses the minimum
live registration. Shared memory floors/per-pane estimates use the maximum,
and pressure ceilings use the minimum. Lowering capacity never revokes a held
lease or cancels an active task; new work waits for sufficient capacity.
`Pool.AdmissionLimit` is the current heavy cap; `Pool.Capacity` retains a maximum
needed to validate older held and waiting weights. FIFO waiters remain ordered.
Status reports these settings as dynamic; root and socket remain fixed.

Host threshold keys are `Soft`, `Hard`, `HardNoSwap`, `PerPane` (bytes), `IO`,
`MemoryPressure`, `Load`, and `PaneLimit`. Supply the full threshold object from
status with the intended fields edited; invalid relationships and nonfinite
numbers refuse. The dedicated pane limit remains the Herdr cap.

`start_seconds` bounds transport startup (default 180 seconds), including gated
Herdr readiness. Proven nonexecution can retry up to three total starts per
binding with fresh dispatch/session IDs and a durable counter for each binding
the turn tries, without consuming task or stage attempt budgets. A binding's
third failed start parks `ProviderUnavailable`, recorded against that binding,
and route selection then takes the next eligible route; completed work resets
the stage's startup counters. Retirement and an unchanged
worktree are required. Besides the transport's proof, the shipped bridges report
one provider outcome as nonexecution (`provider-not-started`): the absolute
provider executable did not start, or the CLI wrote nothing to its event stream
and left no answer, whether it exited on its own, the bridge's start bound
stopped it or the bridge's own timeout did. Claude Code and Codex write their
session event first, before the model is asked anything. The start bound is the
bridge's `--start-timeout` (default 180 seconds, Appendix A's agent start): a
CLI that hasn't written anything by then is stopped with the processes it
started. A dispatch's first session retries this way with nothing charged, and
so does a later review session's start failure (the transport's proof or
`provider-not-started`, after earlier reviewers ran): the dispatch is relaunched
as every reviewer, in fresh sessions, charging no attempt; the start counts
toward the bound of that reviewer's binding, whose third failure parks
`ProviderUnavailable` against it. A later review session that host admission
defers parks for the host like a first session, also charging nothing. Prophet
relaunches a provider that never acknowledged on the
same route up to three starts, then charges that binding Unavailable and fails
over. Once the provider has written any output, missing acknowledgment does not
prove nonexecution. A provider that acknowledged and then exited with a
failure, or that the bridge's own timeout stopped after it wrote something,
records `provider-failed`: its health is charged from its log, and with retired
processes and an unchanged tree the stage retries it as malformed output within
its bound (FAE-WF-034). A changed tree keeps it uncertain; a stall follows the
separately bounded stall path. This is not a generic provider acknowledgment
protocol.

Fae's own failures before the bridge starts are told apart from the
provider's in the direct runtime: a failed session directory, configuration,
environment, brief, shim or pipe step is `launch-refused`, the runtime's
statement that no process started, which the executor types refused before
launch: no attempt, no binding health and no start counted (FAE-RT-004,
FAE-RT-009). A launch boundary the admission journal could not record is
refused before launch the same way. In the Herdr and resident runtimes, a
socket path over the platform bound or a failed temporary directory, key,
wrapper or fifo is still the transport's non-execution proof: it counts
toward the start bound, and an exhausted bound charges the binding
Unavailable although no provider ran.

The [persistent-provider acceptance run](verification/results/persistent-host-startup-dogfood-20261017/result.json)
completed two real tasks in 29 turns with eight verified resumptions, including
two across restart and two within task-bound contexts. Three global contexts
served both tasks; task-bound contexts never crossed tasks and reviews stayed
fresh. Pane/heavy/task/load overrides survived restart. All 58 leases and owned
process/pane resources were released. Enable the harness's `--persistent-sessions`
flag to exercise these assertions; evidence is retained with `--evidence-dir`.

## Resident sessions, loops and crash resolution

Set `resident_sessions: true` in the pinned live configuration to keep owned
Herdr worker panes across turns. This requires the multiplexer adapter. The
worker stays resident; each bridge/provider invocation remains a separate owned
turn process. Keep `--session-root` to retain provider conversation context.
`resident-session.json` records each turn's resident ID and server/pane PIDs.

Set `persistent_loops: true` only for a bridge that implements persistent context.
The bundled bridges require `--session-root`. For example:

```sh
fae schedule create --id observe --kind loop --role observer   --prompt 'Report a short observation in the message field.'   --provider claude --model claude-opus-5-5 --effort medium   --interval-seconds 10 --max-iterations 5 --expires-in 1h   --authorizing-message AUTHORIZATION
fae schedule inspect --id observe
fae schedule pause --id observe --authorizing-message AUTHORIZATION
fae schedule resume --id observe --authorizing-message AUTHORIZATION
fae schedule retire --id observe --authorizing-message AUTHORIZATION
```

Loops use a dedicated pinned read-only worktree. A crash during an iteration
parks that schedule as uncertain; retire it explicitly rather than automatically
replaying a possibly executed prompt. Cron is covered by the subsequent increment below.

Each live loop turn records its provider's health against the schedule's
binding, through the same session-log reader and health rows as a workflow turn
(FAE-RT-009): a structured rate limit, outage or auth failure in the turn's log,
otherwise ok for a usable answer, timeout for a timed-out or stalled turn and
malformed output for anything else. So a throttled loop makes its binding
ineligible for workflow routing too. Nothing is recorded for a turn no provider
ran (an admission deferral, a refusal before the provider), one whose provider
never acknowledged the prompt, or one Fae stopped (the loop's expiry or
retirement, or the daemon stopping). A loop turn that fails, including one
whose provider never acknowledged the prompt, still parks the schedule as
uncertain: loops have no start bound of their own.

For a parked workflow turn, inspect history and resolve the exact dispatch:

```sh
fae resolve-uncertain --id TASK --dispatch DISPATCH --action redispatch   --authorizing-message AUTHORIZATION
```

`accept --result FILE` accepts only schema-valid original read-only evidence with
unchanged tree and HEAD. `escalate` stops the task. All actions require retired
turn ownership, an audited operator message and matching uncertain dispatch.
Redispatch preserves budgets and refuses after three genuine crashes.

The dogfood harness adds `--resident-sessions --provider-loop --crash-during-turn`
to exercise physical pane reuse, five real loop prompts and an actual mid-turn
SIGKILL with explicit recovery, alongside the existing drained restart.

The [final resident/loop/crash acceptance run](verification/results/loops-resident-crash-dogfood-20261017-r3/result.json)
passed two workflow tasks and five loop iterations in 34 completed real provider
turns. It proved physical pane reuse, a provider-turn SIGKILL followed by audited
redispatch, and a separate drained restart. All owned leases, processes, terminal
receipts, pane reservations and sockets were cleared. The evidence manifest
includes source/binary hashes captured before execution.

## Cron admission and controller lifecycle

Create an interval-anchored cron schedule using `schedule create --kind cron
--spec FILE --authorizing-message ID`. The JSON contains `id`, `kind: "cron"`,
RFC3339 `anchor`, `interval_seconds`, optional exclusive `end`, optional
`catch_up_limit` (default 10, maximum 100), and `template` with `workflow_id`,
`revision` (zero pins the latest revision at creation), and `content`. Each
occurrence has a deterministic task ID. Admission precedes cursor advancement;
recovery does not repeat admission effects. Older missed occurrences outside the
catch-up bound are recorded as skipped. Creation authority persists until the
schedule ends or is retired; lifecycle mutations require fresh authorization.
This is interval recurrence, not a five-field cron expression parser.

Start the bundled launcher outside agent containment in a private physical
0700 directory, then use the authenticated controller. The launcher serves one
instance: it authenticates callers with a key file written beside its socket and
starts only the launch or replacement that instance's maintenance marker names,
hash-checked against the marker (FAE-SEC-001):

```sh
fae --home HOME --instance NAME runtime-launcher --socket /absolute/private/runtime.sock
fae maintenance stop --launcher-socket /absolute/private/runtime.sock \
  --note 'planned restart' --authorizing-message ID
fae maintenance status
fae maintenance start --authorizing-message ID
```

The launcher checks native sandbox state. Maintenance pins executable hashes,
arguments, environment and process identity. Plane rows, markers and events
name the relaunch environment only by digest; its values live in an owner-only
`controller-environment-DIGEST.json` in the instance root until the relaunch is
accepted. Post-start acceptance checks the
new daemon and Dolt, plane consistency and a completed drive round. The marker
has no expiry. `watchdog --authorizing-message ID` runs independently and honors
maintenance markers, monitors drive progress and filesystem capacity, and
persists pending alerts while the daemon is unavailable. Watchdog alert/rearm
mutations still require a current operator authorization message; renew that
citation for supervision beyond the message freshness window. Health reads and
continuation of an already-authorized maintenance operation do not expire.

`roll --binary PATH` uses this lifecycle. Changed binaries additionally require
clean embedded VCS identities and `--gated-ref refs/fae/gated/NAME` whose tip
carries a durable landing proof (see below). Build-from-ref, overlap holds and
independent drive rearm are described in the deployment section. Do not treat
this as complete coverage of every FAE-CTL requirement.

For an uncertain local host/VCS effect, `resolve-uncertain --id TASK --dispatch
DISPATCH --action accept --authorizing-message ID` loads the daemon's immutable
receipt and rechecks exact postconditions. It does not accept supplied result
JSON. A Git CAS completed before receipt persistence may be reconstructed only
from the pinned intended SHA and exact retirement proof. A host command with no
completed receipt remains uncertain. Explicit redispatch of effects requires
positive proof that execution never started. External service effects are not
covered by local receipts.

The dogfood harness adds `--cron-schedules --controller-lifecycle` to admit both
real-provider tasks through durable schedules and replace the planned restart
with maintenance stop/start through the independent launcher.

The [latest authenticated acceptance](verification/results/cron-controller-effects-dogfood-20261018/result.json)
passed both cron tasks, provider-turn crash recovery, controller restart and
maintenance watchdog suppression. It completed 34 provider turns including five
loop iterations, reused seven resident panes, accounted for all 66 host leases,
and left no owned process/pane/socket residue. Source and evidence hashes were
checked against the final implementation.

## Deployment and independent drive operations

`fae roll --build-ref refs/fae/gated/NAME --launcher-socket /absolute/runtime.sock
--note "deploy gated revision" --authorizing-message MESSAGE` builds in an isolated
clean clone and retains the resulting binary under the instance deployment
folder. `--gated-ref` can pin the approval ref explicitly; the requested source
must equal its tip, and that tip must carry a durable landing proof. Its `go
build` runs under the host heavy-op lease at weight 2 in the controller class
(the whole pool when capacity is below 2). Changed-binary source history must
exist in the bound workspace. `--when-clear` also waits for QA, while commit windows are always
protected. A gate moving during build requires retry. `--overlap-wait` defaults
to two hours; `run --critical` protects a task from timed overlap overrides.
Use `--ack-overlap` only when intentionally authorizing the recorded overlaps.
The existing `--binary /absolute/fae` path supports same-binary rolling restarts.

`fae land --task ID --target BRANCH --authorizing-message MESSAGE` lands a Done
task onto an operator branch in the background; the CLI polls its durable record
(`--poll`, default 2 s, at least 10 ms). Admission needs the I/O gate, a 12 GiB memory floor and a weight-2
heavy lease. The task's own commits not already upstream are replayed onto the
target tip, and the exact merged tip is gated in a temporary worktree: own-hunk
presence, formatting debt, compile, the task's required test, frozen regions, QA
and the whole Go suite. Tolerated flaky tests and allowlisted frozen checks come
from the task's pinned target configuration (schema 4 `landing` policy), never
from the landing request (FAE-CTL-030). A failing listed flake doesn't explain
the suite's nonzero exit on its own: go test prints the same stream when a
package's `TestMain` also exits 1, so the suite is run once more with every
listed flake skipped (`-skip`). That run must exit 0, fail nothing, report
exactly the packages the first run finished, and run every top-level test
the first run ran apart from the skipped flakes. `-skip` names no package, so
a flake listed for one package (`pkg:TestName`) is skipped in every package;
a test of that name elsewhere counts only if it ran in the first run, in a
package that passed there. One case the rerun cannot tell apart: a
`TestMain` exit in the flake's own package that happens beside the flake's
failure and not again in the rerun looks like the flake alone, and passes.
Only then is the gated proof recorded and the target advanced by
compare-and-swap fast-forward; a checked-out target moves only when clean and
still at the starting tip. Any failure leaves the target
unchanged and keeps the attempt on `refs/heads/fae/held/ID`. `--batch A,B` lands
in dependency order and stops at the first hold; `--salvage --source-ref REF`
lands the tree an escalated task's engine runs recorded as passing;
`--then-roll --launcher-socket PATH` rolls to the gated tip once the QA window
clears. A landing then advances `refs/fae/gated/BRANCH`; that is the only way a
gated ref becomes deployable, and `roll` refuses a gated tip without a proof.
`fae gates self-test` (offline) shows every gate can both fail and pass.
`fae gate run --host-ledger DIR -- COMMAND` runs an operator command under the
same I/O gate, 12 GiB floor and controller-class heavy lease, exiting 75 when the
admission wait times out, 76 when admission is refused, and otherwise with the
command's code. A landing uses the task's approved scope, and passes the task's file names to
git as literal paths, never as pathspecs (FAE-SEC-005). Open gaps: conflicts
always hold, gates are Go-only, templated QA stages run literally, and a landing
has no lease journal, so a crash retains its lease and `LandStatus` reports
`interrupted`.

`fae drive status` reports daemon/plane identity and drive generation.
`fae drive stop --authorizing-message MESSAGE` holds the scheduling loop without
stopping workers or Dolt. `fae drive rearm --authorizing-message MESSAGE` validates
durable checkpoints and resumes collection. Active maintenance suppresses both
operations; a failed preflight record does not. Rearm attempts consume a durable
three-per-cause hourly budget.

Starting `fae watchdog --authorizing-message MESSAGE` while the daemon is reachable
registers a narrow persistent grant for alerts and drive rearm. Revoke it with
`fae watchdog --revoke-grant OLD_MESSAGE --authorizing-message FRESH_MESSAGE`.
A revoked citation cannot register again. General mutations still require a
fresh operator citation; `serve --message-max-age 72h` configures a positive
duration, defaulting to 24 hours and retained across controller restarts.

`fae cleanup scratch-archive --scratch-instance NAME --source DIR --survivor DIR`
now goes through the serving instance's daemon (`--instance` names the live
daemon, `--scratch-instance` the removed scratch instance under the same home)
as the `ScratchArchiveCleanup` RPC: instance operator only, preview by default,
`--apply --plan-digest D` with a fresh runtime authorizing message. See
[daemon/SCRATCH.md](daemon/SCRATCH.md).

## Optional UI

`serve --ui` supervises a UI child that shows status, the board, task history and
alerts. It binds only a literal loopback address (`--ui-bind`, default
127.0.0.1; `localhost` is an alias); `0.0.0.0`, `::`, LAN addresses and host names
are refused before any file or listener exists. `--ui-port 0` (the default)
derives a stable per-instance port. Every request needs the token, which never
appears on argv and is regenerated at each start; `fae ui token` prints it with a
one-time login URL for the instance operator. The UI is read-only unless
`--ui-allow-writes`, and its writes still cite an authorizing message that names
the verb. Reach it remotely only through an authenticated port forward. Crashes
restart with backoff (1 s doubling to 60 s, at most 10 per hour, then one
`ui-child` alert). See [ui/README.md](ui/README.md).

For Normal live/dry-run disposition flags, installed external adapters and
query-only recovery, see [external adapter operations](external/README.md).

## Shipped Normal review sub-workflow

The shipped default adds Normal-only stages after integration: `ticket-open`,
`ticket-in-review`, `review-create` (a draft; an existing review routes to
`review-revision` instead), `review-read` with a `review-remediation-loop` back to
`implement` (at most 5 iterations), `review-publish`, `review-merge`,
`ticket-closed`, then Done. Local tasks pin the Local projection and never reach
them. Normal admission, dry-run or live, binds each stage to the installed tool
whose alias is its subject: the pinned target configuration's `normal.tools`,
else the instance's `external_tools`. Install all seven subjects, each with role
`planner`: `ticket_create`, `ticket_update`, `review_create`, `review_revision`,
`review_read`, `review_publish` and `review_merge`. A missing alias or a different
role refuses admission, naming the stage and subject. A dry run walks the stages
recording intended effects (tool, pinned arguments, digest) and calls no
adapter. Live review stages also need the task's `review_policies` entry (below);
publish and merge can be disabled as described there.

## Configuring review and ticket adapters

Install each capability under `external_tools` with an exact endpoint, server,
tool name, daemon role and immutable argument pins. The adapter must expose
`GET /schemas`; configured aliases are schema-checked during startup, including
when subsequent tasks use dry-run. Dry-run itself makes no effect query, status
read or mutation. Remote endpoints require HTTPS; literal loopback may use HTTP.
The [schema contract](external/SCHEMAS.md) defines descriptors, typed arguments,
size/time bounds, refresh behavior and source binding validation. No native
vendor transport or credentials are configured by Fae; the explicitly installed
adapter handles vendor authentication and translates the documented protocol.

`review_policies` is keyed by task ID. Each policy names `package`, absolute
physical `workspace`, `branch`, `integration_tip`, `baseline_tool`, `status_tool`
and an explicitly approved `baseline_approved` verification mechanism. The
workspace must already exist on the task branch and contain the reviewed tree.
Fae never provisions or syncs that workspace during execution. The baseline read resolves the current integration ref and pins that exact commit
in its request; a red or mismatched response is a target-configuration defect.
`fae review-health --task ID` checks that target through read-only adapter calls.

For vendor-specific names, `operations` maps installed aliases to semantic
`review_create`, `review_read`, `review_revision`, `review_publish`, `review_merge`
or `review_baseline` operations. `argument_keys` maps semantic fields such as
`workspace`, `branch`, `package`, `review_id`, `revision`, `draft` and automatic
publish/merge flags to the vendor's actual schema keys. Installed pin values may
use daemon-owned `$workspace`, `$branch`, `$package`, `$integration_tip`, `$review`
and integer `$revision` tokens. Unknown tokens and incompatible schema types
are refused. Baseline/status aliases must expose output schemas and the
normalized read data described in [external/README.md](external/README.md).

Set instance `disable_review_publish_merge` or task policy
`disable_publish_merge` to omit publish/merge stages. Each removed stage must
have a single successor. `remediation_stage` names a workflow `Loop` whose edge
enters a change-producing implementing `AgentTurn`; the review-read stage's
failure edge must target that loop. The workflow must re-establish ordinary
verification evidence before a review revision. The create stage’s alternate
`failure` edge must name the sole `review_revision` stage: once a review ID
exists, returning to creation routes to that revision stage without another
creation call. Read remediation carries a daemon-measured committed HEAD at the
reviewed tree into the next implementation turn. Pre-provisioning the task’s
`worktree.Manager.Binding(run, task)` path lets the same real workspace follow
implementation, commit and review revisions without build-workspace syncing.
`max_revisions` defaults to five and must be within 1..100. Pending vendor
status observations use a separate durable budget: `poll_seconds` defaults to
30 and `max_polls` to 120 (each accepts 1..3600). They do not consume the task’s
implementation-attempt budget. Counts and retry deadlines survive daemon
replacement; reaching the budget escalates with `ReviewPollCap`. A ready read,
remediation handoff, or successful lifecycle operation resets that wait budget.
 A missing, dirty, mismatched or symlinked review
workspace fails closed; updating a separate provisioned workspace is not an
implicit part of remediation.

`ticket_workflow` explicitly lists enabled `workflows`, a `refresh_seconds`
value in 1..300, and `actions` keyed by lifecycle state/stage. `Pending` is the
creation action. Each action names an installed `tool` alias, the exact runtime
room `action` name and `bindings` from actual vendor argument keys to daemon
sources: `task`, `room`, `ticket`, `destination`, `push_namespace`. Each action
must bind room, destination and push namespace; creation cannot require a prior
ticket. Dynamic keys can be omitted from static tool pins, but must exist with
compatible types in the discovered schema. Static pins remain immutable.
The workflow must grant the installed tool's exact role. A managed ticket stage
must correspond to its configured lifecycle action; it uses the same durable
row as background mirroring.

Normal live admission still requires the explicit external-effects approval
phrase and pinned task destinations. Ticket-enabled workflows also require a
room. Local and dry-run dispositions never emit ticket mutations. Inspect
`status`'s `ticket_outbox` for depth, failures and uncertainty; attempts and
last errors remain in each durable row. An
uncertain started row is query-only after restart; do not interpret a missing
receipt as permission to create another ticket.
