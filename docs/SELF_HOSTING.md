# Running Fae on Fae

`scripts/self-host.py` runs a live Fae instance whose workspace is a clone of this
repository. Each task you submit runs the shipped default workflow with real
Claude turns: the engine builds and tests every change, and verified commits land
on the clone's `fae/integration` branch. Nothing reaches this repository until you
fetch those commits, check them and merge them yourself.

The script uses only existing `fae` verbs and the Python standard library. Run it
with `python3 -I`. Every subcommand takes `--home` (default `~/fae-self/i`); pass
the same value to each one if you change it.

| Subcommand | What it does |
|---|---|
| `setup` | Clones the source at a commit and writes the live config and the workflow. |
| `start` | Serves the instance in the background, authorizes it and installs the workflow on first start. |
| `submit` | Admits a task: a title, a prompt or prompt file, and declared files. |
| `status` | Prints the instance and each task's state, stage, attempts, test runs and any escalation. |
| `stop` | Drains active turns, then shuts the instance down. |
| `land` | Lists the commits on `fae/integration` past the base. `--fetch-into REPO` fetches them into REPO as a branch. |
| `acceptance` | Runs setup, start and submit for one task file, waits for the result and writes evidence JSON. |

## Prerequisites

- Linux, with Go 1.27.2 (`go` and `gofmt` on PATH), Git 2.45 or newer and Dolt.
- A logged-in Claude Code CLI.
- Herdr (`herdr-core`) only if you want the multiplexer adapter. The default is the direct
  (process) adapter.
- The two binaries, built from this checkout:

  ```sh
  go build -o bin/fae ./cmd/fae
  go build -o bin/fae-claude-bridge ./cmd/fae-claude-bridge
  ```

- **A short home.** The engine runs tests with TMPDIR at
  `<home>/self/worktrees/command-tmp-NNNNNNNNN`, and Fae's own tests bind Unix
  sockets under `t.TempDir()` inside it. A socket path is limited to 108 bytes.
  The script refuses a `--home` whose `<home>/self/worktrees` is longer than 40
  bytes, so the home itself can be at most 25 bytes. The default `~/fae-self/i`
  fits when your home directory is 14 bytes or shorter (`/home/operator` is 14).
  Otherwise pass a shorter `--home`. The home must be outside every repository
  and its path must not pass through a symlink. The broker removes each run's
  TMPDIR afterwards, even when a test left a read-only tree in it; one it
  cannot remove is logged, and the next start removes it or moves it to
  `<home>/self/scratch-quarantine` (remove that by hand: `chmod -R u+w`, then
  `rm -rf`); a `scratch-quarantine` warning alert says when it holds anything.
- **Process-leak alerts during QA.** A `process-leak` alert whose reason is
  `defunct` means more than 20 defunct processes in the daemon's tree were
  counted against the bound. A running test suite's own defunct children are
  left to it for two 5 s census rounds; past that they count, so the alert's
  `defunct_parents` names the process that has not collected them (`command`,
  `pid`, `defunct`, `rounds`): a test fixture holding zombies, or a `go` or
  shell process that stopped waiting for its children.
- **The PATH you start with.** The daemon keeps the PATH of the shell that runs
  `start`, and the engine runs `go test`, `gofmt` and nested `git` with it. `start`
  refuses to run if `go`, `gofmt` or `git` is missing from that PATH.

## Credentials

`--claude-config-dir` is required. The script writes it only as
`CLAUDE_CONFIG_DIR` in the live config's `environment`, and nothing else goes
there. The daemon gives each bridge session a fresh HOME and a restricted
environment, so this variable is how Claude Code finds its login. The script
never reads, copies or links anything inside that directory.

Pointing it at your own `~/.claude` makes every turn spend your account's usage.
Don't copy the credentials file into a private directory instead: a copy can
rotate the refresh token and log out your own Claude Code session.

The daemon doesn't read that directory either, so it can't redact the login
by its value (FAE-SEC-003). An engine run runs as your user and could read the
login and print it. Redaction then catches only what in it has a provider
token's shape (for a Claude login, `sk-ant-…`) or sits in an OAuth token
field (`refreshToken`, say), in excerpts, nested-call records, landing gate
output and the bridge's raw provider logs, and only as the token itself, not
its hex or base64. See "Redaction" in [daemon/README.md](../daemon/README.md) for what is
covered and why the daemon doesn't read the login.

The live config is written owner-only (0600) under `<home>/.self-host/`, outside
every repository, as `fae serve` requires.

## Set up and start

```sh
python3 -I scripts/self-host.py setup --claude-config-dir ~/.claude --dolt "$(command -v dolt)"
python3 -I scripts/self-host.py start
```

`setup`:
- clones the source (`--source`, default this repository) at `--commit` (default
  `HEAD`) into `--dir` (default `~/fae-self/repo`) with `git clone --no-local`;
  only commits are cloned, never uncommitted changes;
- leaves the clone detached at that commit, gives it a local Git identity, keeps
  one branch, `fae/integration` at the commit, and removes the `origin` remote;
- refuses a non-empty `--dir`, or a home that already holds an instance;
- writes `<home>/.self-host/live.json`, `workflow.json` and `state.json`, and
  prints them.

The live config pins **no task scope**:
- `files` is empty, and there is no `test_file`, `test_id` or `expected_failure`.
  Each task's files come from its declared files and its Spec, its RED identity
  from its RED report, and its commit message from the task (`commit_message` is
  empty). See [LIVE.md](../LIVE.md).
- `allowed_commands` is exactly the default workflow's host commands: `go test
  -json -count=1 -a -x ./...` and `gofmt -l .`.
- `command` runs the Claude bridge with `--timeout` (`--timeout-seconds`, default
  840 s). The session timeout is 60 s longer (900 s at the default).
- `nested_commands` is `account` and `max_output_bytes` is 16 MiB. Fae's suite
  runs nested `git` and `go` commands and prints about 5.6 MB with `-x`. With the
  defaults the broker cuts that output at 4 MiB, so a run counts only the tests
  reported before the cut, and refuses the nested commands. That includes the
  `go` a test starts (for example `go list` in
  `TestCoreBinaryExcludesTheOperatorUI`): the broker runs go under a per-run mirror of GOROOT whose `bin/go` is the wrapper, since go test
  puts `$GOROOT/bin` first on its tests' PATH. In account mode each nested call
  is recorded and then passes the nested-command policy, an allowlist with
  limits of its own: nested `go` takes only listed flags and is clamped
  (`-p=2`, `-parallel=2`, or 1 for a whole suite, including one whose `-run`
  selects every test); its environment is an allowlist too, so it may not
  change the programs go runs (`CC`, `CGO_*FLAGS`, ...) or run tests at more
  threads than the clamp (`-cpu`, `GOMAXPROCS`); the C compiler and
  `pkg-config` that go finds by name are the ones on the daemon's PATH, not a
  `gcc` a test puts first on its own; and `gofmt` gets the formatter limits. Every brokered run uses `GOENV=off`, so your `go env -w`
  settings (`GOPROXY`, `GOPRIVATE`, `GOFLAGS`) don't apply, top-level go
  included, and no Go variable of the daemon's environment reaches a run;
  Fae's own module needs none while its dependencies are in your module
  cache or on the default proxy. A value a run needs goes in a HostCommand
  stage's `environment`, which takes a custom workflow revision. If the `go`
  on the daemon's PATH is a version manager's shim outside any GOROOT, runs
  get no GOROOT mirror and a test's own go reaches the real toolchain:
  `fae doctor` warns and the daemon logs it at start.
  Nested `git` may only read the clone and its task worktrees, and every
  repository outside the run's directories and the system temporary ones, your
  own checkout included; the tests' own repositories under TMPDIR are changed as
  called. A push, a `git remote` call or a remote change to a protected
  repository exits 78 for this Local task; any other write to a protected
  repository exits 77. On any repository, 77 also refuses a program the call
  itself names for git to run (a `-c core.sshCommand`, `GIT_SSH_COMMAND`,
  `rebase --exec`, the same settings in a `GIT_CONFIG_GLOBAL` file or a
  nested `git config`, ...), any `GIT_*` variable the policy does not know
  (such as `GIT_TEST_FSMONITOR`), a file named for git to write outside those
  directories, a test repository whose `.git` links into another repository,
  and a repository named by a URL off the host. Every nested `git` runs with
  `GIT_ALLOW_PROTOCOL=file`, so it reaches only local repositories; with no
  global or system configuration it does not name, so your `~/.gitconfig` and
  a `HOME` a test points elsewhere load nothing; with no hook and no
  file-system monitor, even in a test's own repository; with trace2 targets
  off unless it sets them; and pinned to the repository the policy checked.
  Other programs a test's own repository's configuration names (a filter or
  diff driver) are the test's own setup: they run, and their own git calls
  reach the real git, not the wrappers, as do those of the clone's own
  configured programs on a read. A test's submodules are not checked, and a
  process that changes a repository between the policy's check and git's
  start is not seen. The daemon must be the `fae` binary, which runs the
  policy; see [commandshim/README.md](../commandshim/README.md).
- `heavy_capacity` 2, `stall_seconds` 300 and `stall_safe_replay` are the
  dogfood's working values. `host_ledger` defaults to `~/.fae/host-ledger`, which
  every instance on the host shares; instances sharing a ledger must agree on
  `heavy_capacity`, or the second one refuses to start. `--host-ledger` picks
  another.
- The adapter is `direct`; `--herdr BIN` selects `multiplexer` (`herdr-core`).

The workflow is the shipped default's Local projection, with each role's route
chain narrowed to the Claude bindings (FAE-WF-082), as `scripts/dogfood.py` does.
Nothing else changes. `--model` (default `claude-opus-5-5`) also sets Prophet's
model.

`start` runs `fae serve` without `--foreground`, so it detaches once ready and
logs to `<home>/self.serve.log`. It then sends an authorizing operator message
(classes `task` and `runtime`, plus `governance` on the first start), and on the
first start installs the workflow with `template workflow-put`. Later starts
check that the installed revision is still there. The script records the message
and reuses it for at most 23 hours; the daemon refuses messages older than 24.

## What a task needs

- **A title.**
- **A prompt that states the acceptance:** the behaviour to change, how a test
  shows it, and any constraints. The reviewers and the Oracle judge the change
  against it.
- **Declared files:** every file the task may read in full and change. Include
  exactly one `_test.go` file, which will hold the RED test. Agents have no shell
  or file tools. The bridge gives them the declared files' contents, a listing of
  the repository's tracked paths, and on a rework turn the engine's output from
  the failed runs that sent the task back. Keep the list short. An edit replaces
  a whole file or exact text within one.

```sh
python3 -I scripts/self-host.py submit \
  --title 'Refuse a NaN watchdog filesystem threshold' \
  --prompt 'controller.Watchdog accepts FilesystemThreshold NaN ... Add a test that ...' \
  --declared-file controller/watchdog.go \
  --declared-file controller/watchdog_test.go
```

Use `--file task.md` instead of `--prompt` for a longer prompt. `submit` warns
when there is not exactly one `_test.go` file. It is a warning, not a refusal,
because a plan may declare a task no-red (for example a documentation change).

## What the default workflow does with it

1. **Intake** (the spec role) accepts the task or rejects it. A rejection
   escalates.
2. **Spec and spec review**, for up to three rounds. The task's files are the
   ones you declared with `submit`; a Spec's own `declared_files` would set them
   only for a task submitted without any, and then never a protected path (CI
   configuration, a file that makes your tools run programs, a credential-like
   file): one the Spec names is left out and the task's change turns are
   refused, naming it, until a restart with information lets the spec declare
   the files without it. A protected file you declare yourself with `submit` is
   kept.
3. **Plan.**
4. **RED.** The coder writes one failing test in the test file (`test-first`)
   and a RedReport naming that file, the test and a fragment of its failure
   message. The engine checks the report when the turn ends. A report that
   names no test file or two, a test id that is not a Go test name, or a
   fragment that says nothing about why the test fails is refused: the test
   file goes back to how it was, and the turn runs again with the reason. That
   costs an attempt, not a test run. A fragment says nothing when it is only
   the test's name or part of it, its file's name or line (`go:9` too), go
   test's framing (`FAIL`, `--- FAIL:`, `ok`), a panic's generic text
   (`panic:`, `runtime error`, `goroutine`, stack frames), a bare `got` or
   `want`, `error` alone, or a number on its own. A message (`expected error`
   included), a number beside a word or a comparison (`want 42`, `0 != 42`), a
   panic's own message or a timeout is a reason. A test that calls `t.Run` is
   refused the same way: the gate needs exactly one executed test, and each
   subtest counts. The bridge states these rules in every turn's instructions,
   so the coder knows them before its first try. Then the engine runs the test
   alone (`run-red`). The gate needs it to report its own failure with that
   fragment in its own output, goroutine stacks left out, with exactly one
   test executed. A test whose binary ends under it (`os.Exit`, `log.Fatal`, a
   panic in another goroutine, a `TestMain` goroutine, a kill from outside
   such as the OOM killer's) reported no result: it doesn't count as executed,
   so the gate sends the RED back to the coder. A test that hangs fails when go
   test's own timeout fires, after 5 minutes (half the stage's 600 s), with
   `test timed out`. Then the engine freezes the test's identity.
5. **Implement.** The coder changes the declared files, but not the test file.
   When it needs another file, it asks for a scope widen, which the Oracle
   judges; approved files join the task's scope. A widen naming a path inside
   `.git`, or a protected path (CI configuration such as `.github/`, a file
   that makes the operator's tools run programs such as `.claude/` or
   `.envrc`, or a credential-like file such as `.env` or a private key), is
   refused as malformed output before the Oracle sees it, and the turn runs
   again. Any files that turn changed are kept: the retry starts from them, so
   the task doesn't escalate `StaleBranchResidue` and can still be restarted.
   An approved file the bridge can't load (a symlink, a non-regular file, over
   1 MiB, or past the turn's 4 MiB of files), the RED test file included, is
   listed to the coder as unavailable, with the reason, instead of stopping
   every later turn. A widen that adds another test file the RedReport named
   leaves the frozen test where it was. A task stored with an approved path
   that a widen can no longer add (a nested `.git` widen from before wave 9)
   drops it, with a `scope-dropped` event, and carries on with the rest of its
   scope. The bridge writes a turn's edits all or nothing, so a failed write
   leaves the worktree as it was and the turn is retried.
6. **Green.** The engine runs the test again and checks that the frozen test is
   unchanged, then checks `gofmt`.
7. **Reviews** (correctness, quality, security and architecture), then the
   **Oracle's verdict**.
8. **QA:** the whole suite, `go test -json -count=1 -a -x ./...`.
9. **Merge** with the current `fae/integration` tip. If the tip moved, the tests
   and QA run again on the merged tree.
10. **Commit and integrate** onto `fae/integration`. The task is Done.

A failed gate sends the task back to implementation, or to the RED test, with
the gate's reasons. The budgets are per task: 20 attempts, 8 test runs and 3
scope widens.

## Watching it

```sh
python3 -I scripts/self-host.py status            # or --json
F="bin/fae --home $HOME/fae-self/i --instance self"
$F events --follow                                # every transition as it happens
$F show --task ID                                 # current stage and latest artifacts
$F history --task ID --records                    # transitions, artifacts and decisions
$F inbox                                          # escalated, blocked and uncertain items
```

The logs are `<home>/self.serve.log` and `<home>/self/supervision.log`. Provider
session logs are under `<home>/self/sessions/`.

## Escalations

An escalated task has stopped and needs you. `status` prints its kind and detail.

- **Intake or Oracle stop:** the task was judged unworkable as written. Rewrite
  the prompt or the declared files and submit it again.
- **`AttemptCap`, `TestRunCap` or `LoopCap`:** a budget is spent. Read the history
  to see where it went round. Usually the task is too large or its acceptance is
  unclear: retire it and submit a smaller one. `fae run --carry-from TASK` keeps
  its accepted pre-code evidence.
- **`SameGate` at `test-first`:** the coder's RedReport was refused four times
  running for the same reason, which the detail names. Check that the declared
  files hold exactly one `_test.go` file, then restart the task with information
  that says how to state the RED (for example which failure message to quote).
  A restart is enough: the restarted task's next `test-first` turn runs even if
  its latest RedReport is broken, and replaces it.
- **`SameGate` at `implement` after a scope widen:** rare. The frozen test file
  came into scope through an earlier widen, and a later approved widen added a
  second test file the RedReport named, so the engine can't tell which one is
  the RED file and refuses every implement turn. Restart the task with
  information; that runs the RED loop again, and works only while the task has
  test runs left (`TestRunCap` otherwise).
- **`StaleBranchResidue`:** the task's worktree holds changes the task never
  recorded, for example from a change report refused after it had edited
  files, from a bridge killed while it was writing a turn's edits, or from an
  edit made outside Fae. Restart and Resume are refused while
  the worktree differs from the task's checkpoint. Retire the task and submit
  it again; `fae run --carry-from TASK` keeps its accepted pre-code evidence.
- **`IntegrationConflict` or `MergeOwnHunkLoss`:** another landed change conflicts
  with this one. Submit it again; it starts from the new tip.
- **`ProviderUnavailable` or `StallExhausted`:** Claude failed or went quiet, or
  no route was eligible for 30 minutes, for example because the bridge refused
  every route before Claude ran (the detail then names each refusal, such as
  missing Claude credentials in the target environment). Check
  `fae provider-health`, your account and the target environment; after a
  change to the environment or the bridge, restart the instance. Then restart
  the task with `fae restart --task ID --information TEXT`.
- **A task parked `UncertainDispatch`** (not escalated): a turn or host command
  ended without a recorded outcome, for example because the daemon was killed,
  or the `go` process of a test run was killed so its test stream can't be
  read. A test run that fails, hangs until its timeout or stage deadline,
  whose test binary exits or is killed mid-test, or whose output goes over
  `max_output_bytes` is a run the gates judge (a failed one goes back to the
  coder), not a park.
  Look at it, then use `fae resolve-uncertain --id TASK --dispatch ID --action
  redispatch|accept|escalate`. A malformed model response is retried, not
  parked: the bridge refuses it before writing any edit, provider health records
  malformed output, and the stage retries within its bound (the same refusal four
  times in a row escalates `SameGate`). A turn the bridge refuses before Claude
  runs, such as a task scope it can't hold or a failed `git ls-files`, and any
  dispatch Fae refuses before it starts (a test run its RED identity can't
  hold, a task whose scope is unsafe), is retried charging nothing: no
  attempt, test run or provider; the fourth such refusal in a row at a stage
  escalates `SameGate`. A binding the bridge can't run (no
  credentials, an unsupported effort) moves the turn to the next route instead,
  charging nothing. A Claude CLI that writes nothing (never acknowledges the
  prompt) before it exits, before the bridge's 180-second start bound or before
  its `--timeout`, or a configured Claude executable that doesn't exist, is
  relaunched up to 3 times, charging nothing, then parks
  `ProviderUnavailable` against that binding and moves to the next route. When
  that happens to a later reviewer of the `reviews` stage, after earlier
  reviewers ran, every reviewer runs again, still charging no attempt; the
  failing reviewer's binding's third failed start parks `ProviderUnavailable`
  against it. Those relaunches end even when another task's turn keeps healing
  the binding: a turn gets 3 starts per binding its roles declare in all, and
  the start past that escalates `ProviderUnavailable` naming the bindings. A
  later reviewer the host won't admit (panes, pressure, memory) waits up to 30
  minutes before the earlier reviewers' turns are given up; an agent turn
  deferred by the host 60 times in a row escalates `ProviderUnavailable`
  naming the condition. Fae's own launch failures (a session directory, shim
  or brief it could not write) and a test run, freeze, compare, merge or
  integration refused before anything ran are refused before launch too,
  charging nothing; a commit refused before any write (the no-red classifier,
  unproven required checks) escalates for you with the attempt refunded. A Claude CLI that started its session and then exited
  with a failure, or that the bridge's `--timeout` then stopped, is retried like a
  malformed response: provider health is charged and the stage retries within
  its bound (the same failure four times in a row escalates `SameGate`).
- **A task parked `all-routes-unavailable: every route for role ... was
  refused ...`** (not escalated): the bridge refused every route of the role,
  for example because the target environment has no Claude credentials; the
  narrowed chains have only Claude routes. The reason names each refusal.
  Nothing is charged. Fix the environment and restart the instance: the new
  start tries every route again, and the task resumes at its stage. `fae resume
  --task ID` also tries every route again without a restart. After 30 minutes
  with no route running, counted from the first park (a restart whose retries
  are refused again doesn't reset it; `fae resume` does), the task escalates
  `ProviderUnavailable` instead.

Each control needs an operator message that authorizes it, for example
`$F message --id fix-1 --text '{"text":"Retire task X.","authorize":{"classes":["task"]}}'`,
then `$F retire --task X --reason '...' --authorizing-message fix-1`. See
[daemon/TASK_CONTROLS.md](../daemon/TASK_CONTROLS.md).

## Taking landed commits into the main repository

```sh
python3 -I scripts/self-host.py land                               # list them with their diff stats
python3 -I scripts/self-host.py land --fetch-into ~/workplace/fae  # branch fae-self/<UTC date>
```

`land` never merges or pushes. `--fetch-into` is a plain `git fetch` into a new
branch `fae-self/<date>`. It refuses to move an existing branch other than
forward.

**The engine's QA runs Fae's suite without Dolt-backed tests, and without
Herdr-backed tests unless `herdr-core` is on the daemon's PATH.** Dolt-backed tests
need `FAE_TEST_DOLT_BINARY`, and the engine passes no such variable to its runs.
Those tests are about half of the daemon package. So a landed commit has not
passed the whole suite. Before you take it:

1. Review the change, as [DEVELOPMENT.md](DEVELOPMENT.md) asks of every change.
2. Run `scripts/test.sh` (with `FAE_TEST_DOLT_BINARY` set and `herdr-core` installed) and
   `scripts/verify.sh` on the fetched branch.
3. Merge it yourself.

A Done task keeps its worktree (a full checkout under `<home>/self/worktrees`)
and its `fae/task/*` branch until you remove them. With `$F` as under
[Watching it](#watching-it), `$F backup plane --authorizing-message ID` makes a
verified plane backup; `$F cleanup done` lists what would go, and
`$F cleanup done --apply --backup PATH --authorizing-message ID` removes the
worktrees and branches of Done tasks whose landing is in `fae/integration`,
keeping an archive ref of each branch tip ([cleanup](../daemon/CLEANUP.md)).

## Stopping and starting over

`stop` asks the daemon to drain, waits until no turn is running (up to
`--drain-wait`, default 1200 s), then shuts it down. `stop --now` skips the
drain and interrupts active turns, which then need resolving. A dispatch already
parked as uncertain stays in flight across the stop; resolve it after the next
`start`, which resumes the instance where it stopped.

The instance's database pins the live config's hash, so a changed config is
refused at the next start. To start over, for example from a newer commit: stop
the instance, remove `~/fae-self` (the clone, the home and its database), and run
`setup` again.

## Acceptance run

```sh
python3 -I scripts/self-host.py acceptance --claude-config-dir ~/.claude \
  --dolt "$(command -v dolt)" --task task.json [--wait 14400] [--evidence out.json]
```

`task.json` is `{"title": "...", "prompt": "...", "declared_files": ["...", "..._test.go"]}`.
The command runs setup, start and submit, prints each change of state, and waits
until the task is Done or Escalated, or until `--wait` expires. It then writes
evidence JSON (default `<home>/.self-host/acceptance-<time>.json`) and stops the
instance, unless you pass `--keep-running`. It exits 0 only when the task is Done.

The evidence holds:
- the task's history summary: transitions, artifacts, attempts, test runs and any
  escalation;
- the landed commit and its diff stat;
- the engine runs, each with its wall time from dispatch to completion (including
  any host-lease wait) and its recorded result;
- the provider turns from the session logs, with Claude's reported durations;
- the final instance and task status.

## Known limits

- The engine's QA skips Dolt-backed tests, and Herdr-backed tests unless `herdr-core` is
  on the daemon's PATH (above). Run `scripts/test.sh` and `scripts/verify.sh`
  yourself.
- Every engine test run rebuilds everything (`-a`). A RED or green run is
  narrowed to one test but still compiles every package, in about 11 s on this
  host. The broker runs a whole-suite run one package at a time (FAE-HOST-015),
  so QA takes about 5 minutes, plus host-lease waits.
- Agents see only what the bridge gives them: the declared files, the repository
  listing and failure excerpts. Each declared file is read in full, so large
  files make turns slow.
- The clone's base is fixed. Tasks start from the current `fae/integration` tip,
  never from newer commits in this repository. Start a new instance to pick
  those up.
- The instance runs `bin/fae-claude-bridge` in place. Rebuilding it while the
  instance runs changes the bridge the next turn uses; stop the instance first.
- One provider. The route chains are narrowed to Claude, so a Claude outage stops
  every role.
- One instance, named `self`, per home.
