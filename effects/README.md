# Real effects

`Broker` runs commands only in a registered Fae task worktree. Its caller must
hold durable task and weighted host leases across validation, execution, and
collection. The allowlist contains exact argument vectors; no shell or template
expansion occurs. Configured command vectors are operator trust inputs. Dynamic
typed-placeholder templates are not implemented yet. The one exception is a
workflow-declared `go-unit` test shape: after the allowlist match, the broker
inserts `-run=^TestID$` for the task's pinned test and, unless the vector sets
one, go test's own `-timeout` at half the run's timeout. Build and test scratch
(`TMPDIR`) lives on disk beside the worktree and is removed after each command.

Commands receive a minimal environment, a timeout, process-group cleanup on both
normal exit and cancellation,
and bounded output: 4 MiB of stdout and stderr together unless `MaxOutput` is set
(live config `max_output_bytes`, 1 to 64 MiB). A caller's cancellation, malformed
Go test JSON, a test stream that can't be read, and tree mutation refuse usable
evidence. A command killed at its own timeout is evidence of a failed run
instead (`TimedOut`, exit code -1, its output kept), once its process group is
retired. So is a command whose output went over the bound (`Truncated`): the
head of each stream is kept within the bound, the exit code stands, and a go
test stream counts only the results its kept part reports. Go test evidence
requires `-json -count=1`, counts executed non-skipped tests from actual events,
and binds expected failure text to the named test's failing output. A test
counts as executed only when it reported its own result or go test's own
`-timeout` named it: the alarm's `panic: test timed out after …` lists the
tests still running under `running tests:`, and go test 1.27 then reports only
the package's fail. A test cut off any other way while it ran (`os.Exit` or
`log.Fatal`, a panic outside the test's goroutine, a `TestMain` goroutine, a
signal such as an OOM kill, which prints `signal: killed` under it, or the
broker's deadline or output bound) reported no failure of its own: it is not
counted as executed or matched and proves no expected failure, whatever was
printed while it ran. The broker trusts test2json's events, so a `TestMain`
that never calls `m.Run` but prints go test's framing for the declared test
(`\x16=== RUN   TestX`, a message, `\x16--- FAIL: TestX`) before it exits
makes a test that never ran count as one executed test failing with that
message (re-audit 10's probe). That takes deliberate `TestMain` code in the
RED file; the green gate must still pass for real later, and review is the
only check against it. The run is still evidence when go test reported the
package's fail or the broker cut the stream itself; a test without a result in a
package that passed, or with no package result in a stream the broker didn't
cut (the go process itself was killed), is refused as incomplete. The shape's go
test `-timeout` lets a hung test fail inside the run, with go test's
`panic: test timed out after …` as its output, rather than be killed with no
result. The text is matched literally against that test's failure text: its own
output without the goroutine stacks in it and with go test's framing lines
(`=== RUN`, `--- FAIL:` and the like) removed. A stack is left out from its
`goroutine N [state]:` header through its frames, whether a panic or timeout
prints it as the binary ends or the test prints one and goes on (net/http's log
of a handler's recovered panic, `debug.PrintStack`), so `FAIL`, a framing line,
or a stack's frames, source paths and offsets never match, and the test's own
later message does. The test id still can: a timeout lists the running test.
So can text the test logs before it fails, whatever the failure. A timed-out
run never proves an expected failure. The broker does not judge the text; for a derived RED identity the daemon refuses one that
says nothing beyond the test's identity and that generic text (see "Task scope"
in [LIVE.md](../LIVE.md)). A fresh-build
attestation additionally requires `-a -x` plus an observed compiler invocation
naming source within the task worktree, from Go build-output events or compiler
stderr. Linking or compiling generated testmain alone does not qualify. Zero tests remain
zero for the engine's positive-count gates. The broker does not infer source
identity from test output; the caller must verify the frozen test bytes.

`FailureExcerpt` turns a failed run's output into the plain text a rework turn
receives (the `engine_feedback` brief block); it changes nothing the run proved.
For `go test -json` it keeps the `# package` headers and `file:line:col: message`
lines of build failures, from stderr and from build-output events but never the
`-x` command trace; each failed test's output in order, with its `=== RUN` and
`--- FAIL` lines (a test left without a result of its own is shown too, with a line saying it is not counted as executed unless go test's own `-timeout` named it); the output of a failed package with no failed test; and the
package `FAIL`/`ok` summary lines. For `gofmt -l` it keeps the listed files and
any gofmt error; any other command keeps its stdout and stderr. Each failed
required check folded into a QA run adds its own section, paths under the
worktree read relative to it, and a final line states the exit code and the
counts the gate judged, and that the run was killed at its deadline or its output cut at the byte bound when it was. An excerpt is valid UTF-8 of at most 16 KiB
(`ExcerptLimit`); `BoundExcerpt` cuts a longer one, keeping its head and tail
around a `[... N bytes cut ...]` line, and the cut is flagged. Table tests run
it over recorded real `go test -json -a -x` and `gofmt -l` output in
`testdata/excerpt`.

Tree measurements use temporary Git indexes and include all tracked and
nonignored untracked files. Snapshot accepts exact file paths, refuses directory
and path traversal declarations, and refuses changes outside the reviewed set.
Every git call the broker makes on a worktree runs with
`GIT_LITERAL_PATHSPECS=1` (`literalPathGitEnv`), so a task's file names,
which a model chooses, reach git as file names and never as pathspec patterns
or magic: snapshot staging, the reconcile and containment comparisons, and
the conflict check's per-file diffs. Before, only snapshot staging did: a task
commit that also added a file named `:(exclude)*` excluded every file from
`Integrate`'s containment comparison, which then reported a commit the
integration ref never received as landed; a reconcile called an upstream
without the task's work "already contained"; and a conflict resolution that
dropped that file's content passed.
Commit writes the measured reviewed tree with `commit-tree` and compare-and-swap
`update-ref` and bypasses hooks. While holding the real Git index lock, it accepts only the old-base or reviewed index tree, prepares the reviewed index, advances the branch, then atomically installs and syncs the index without changing working files. Foreign staging refuses before the branch advances. Callers must
serialize worktree writers and remeasure evidence before using it. This is not
a sandbox against a malicious process or filesystem race.

Integration only advances an un-checked-out `refs/heads/fae/` integration branch,
using Git's atomic compare-and-swap ref lock. Ancestor, patch-equivalent, and
all-touched-path blob/mode containment return the containing tip without moving
it. Touched paths come from the actual commit rather than a caller-supplied list;
empty changes are contained. Changed bases refuse direct integration with a
`TargetMovedError` only when the broker knows no write occurred: either it
observed a different tip before attempting the write, or Git explicitly rejected
the expected-old-value compare-and-swap. After a CAS refusal it inspects current
containment first. Unknown outcomes, cancellation, permissions and lock failures
are not classified as safe retries.

The live daemon admits the typed refusal through `CanRetryIntegration`, then a
bounded integration retry loop. Reconciliation uses the durable task base rather
than the already-committed task HEAD, preserving the complete committed own
delta. Old approvals are invalidated; the merged result repeats tests, reviews,
QA and commit before a new integration attempt. A two-writer temporary Git test
covers the committed loser, union tree, fresh Go compilation/tests and landing.

`PrepareReconcile` makes a three-way merge using the immutable reviewed base,
reviewed tree (including uncommitted changes), and the current integration tip.
`PrepareReconcileFrom` additionally takes the durable original task fork point,
so committed and uncommitted own work are both included. It writes Git objects
but leaves refs and checkouts alone. Conflicts name paths. Exact zero-context own
patch comparison ignores hunk line offsets and index blob IDs while retaining
added/deleted bytes, modes and binary patches. A missing or changed own hunk
refuses with paths. Fully blob-contained changes are separately identified;
partially upstream own work conservatively refuses when its own diff shrinks.

Persist `ReconcileEvidence` before calling `ApplyReconcile`. Application
recomputes the plan, checks the tip and pre/post task state, compare-and-swaps
the task ref, and installs the merged tree only in its task worktree. The real
Git index lock spans checkout through atomic index installation: working files
contain the merged tree while the index returns to the new base, so subsequent
implementation and conflict-resolution edits remain unstaged. Only index trees
from the persisted plan are accepted; foreign staging and existing locks refuse.
Retrying an interrupted ref-before-checkout application is supported; unknown
edits or moved targets refuse. A process crash that leaves a Git lock requires
explicit operator repair, as with commit index recovery. The primary checkout and integration ref are never
rewritten. The new tree is deliberately uncommitted: the caller must recapture
reviewed evidence and rerun required QA before commit/integration. Conflicts
leave the worktree untouched until a separate immutable conflict plan is persisted.
`ApplyConflict` re-derives the plan and exact path set, checks the same production
application guard, then installs text conflict markers only in the task worktree.
The live daemon refuses frozen-test, undeclared and nonregular/binary conflicts.
Supported conflicts return named context to a bounded implementation loop,
followed by tests, frozen-file checks, reviews, judgement and QA.

`VerifyConflictResolution` checks the immutable pre-conflict snapshot rather than
a replacement agent report: every original added hunk block must remain in the
resolved own delta, original deletion blocks must be absent, and task-owned file
mode changes must remain. Missing files and lost hunks identify their original
paths and hunk ranges. Unresolved marker lines refuse. Text offsets may move;
ambiguous repeated-text deletion/move cases and partially upstream shrinking
deltas conservatively refuse. This is a byte-level preservation guard, not a
proof of semantic equivalence; refreshed tests and review still apply.
The daemon retains an independent per-task original-intent record and rechecks
it at implementation, merge and commit boundaries, so omitting a graph input
cannot bypass preservation. A second upstream advance after a conflict requires
operator review; the current implementation refuses rather than overwrite the
original contract. Successful cleanup and landing do not erase its audit trail. These Git checks are
not a proof of merge semantics or protection against a malicious concurrent
filesystem writer; callers must retain their durable writer lease throughout.

Production callers configure `ProcessRegistry` and `ProcessOwner` so host
commands use durable exact process ownership and a parent-death watchdog.
Unresolved cleanup refuses evidence. The embedding daemon must recover the
same registry before reclaiming its resource leases.

Tests use newly created temporary repositories. They execute Go tests, host
commands, exact commits, integration and containment, while asserting the
primary checkout remains unchanged, measurements preserve the actual index, and successful task commits leave their owned index aligned with HEAD.

Journaled commit adoption completes interrupted index installation only after proving the exact prepared commit, parent and working tree, and accepting the same two trusted index states. Unknown or stale Git index locks are never removed automatically. A SIGKILL inside the index-locked region can therefore require explicit operator investigation and lock repair before retry; refusal preserves the lock, index, branch and working bytes. A completed ref CAS with no remaining lock is recoverable through the recorded commit intent.

## Per-run process limit (FAE-HOST-015)

When the live daemon starts, `ProbeProcessLimit` checks that the user's systemd
manager has the cgroup v2 `pids` controller delegated and that a probe scope
enforces `TasksMax`. `Broker.Run` then executes every brokered host command as
`systemd-run --user --scope --collect --property=TasksMax=N -- COMMAND...`: the
scope exec()s the command in place (same PID and process group, so ownership and
group retirement are unchanged) and `pids.max` bounds every task of the run,
threads included. N defaults to 512 (`DefaultProcessLimit`; the spec gives no
value) and is set by live config `process_limit` (`-1` disables). Evidence
records the cap as `process_limit`. A host without delegation runs uncapped and
the daemon logs the gap. `TestBrokerForkStormStaysWithinProcessLimit` shows a
fork storm refused at the cap with `pids.peak` within it.

## Nested commands (FAE-HOST-014)

A brokered command finds PATH-first wrappers for go, gofmt, cargo, rustc,
rustfmt, make, cmake, ninja, pytest and git; only the admitted command itself
runs its resolved binary directly. go test starts its tests with `$GOROOT/bin`
first on PATH, so when the run has a Go toolchain (`commandshim.GoRoot`: a
stage's `GOROOT`, else the GOROOT of the admitted go or of the go on the run's
PATH) the broker also makes a per-run GOROOT mirror beside the wrappers
(`commandshim.InstallGoRoot`, links to the real GOROOT whose `bin/go` and
`bin/gofmt` are the wrappers) and runs the command with `GOROOT` set to it,
`GOTOOLCHAIN=local` and `GOENV=off` (`commandshim.GoRunEnv`), so the go and
gofmt a test starts meet the wrappers too. The mirror is sealed read-only
(`commandshim.SealGoRoot`; the command can make it writable again, so this is
not containment) and removed with the run's policy directory. The broker
removes the policy directory and TMPDIR on every exit path with
`scratchfs.RemoveAll`, so a read-only tree a test left there (Go's module
cache under its own `GOPATH`) goes too; one it still cannot remove is
reported to `Broker.ScratchLeft`, which the daemon logs, and the next start's
sweep removes or quarantines it (daemon/SCRATCH.md). A go outside
any GOROOT gives no mirror; `GoRootGap` reports it, and the daemon logs it at
start. `NestedCommands` selects the wrappers. The default, `NestedRefuse` (or empty), installs
`commandshim.InstallDenials`: every nested call that reaches a wrapper exits
77. A Local task's run (or one whose context names no task) gets
`commandshim.InstallLocalDenials` instead, whose git exits 78 for a push or
remote change (FAE-MODE-002).

`NestedAccount` (live config `nested_commands: "account"`) installs
`commandshim.InstallAccounting` with the run's policy: its worktree, its
on-disk TMPDIR and whether the task is Local. It needs `NestedBroker`, the
`fae` executable (the daemon passes `commandshim.SelfBroker()`); without it
the run fails before it starts. Each nested call is recorded in the run's
private directory, then passes through `fae heavy-op run --shim-policy`, which
applies `commandshim.Policy.PrepareAccounted` and execs the tool in its place:

- nested `go build`, `test`, `vet` and `list` get the top-level command's
  clamp (`-p=2`, whole-suite tests 1) and must stay in the worktree, TMPDIR
  or an on-disk `/var/tmp` (where Fae's own tests keep fixtures);
  their flags are an allowlist, in the arguments and in `GOFLAGS`, with every
  path flag in the scope and never under a RAM-backed root, no tool flag
  that runs a program, no `-cpu` above the clamp and, after `-args`, no test
  binary flag that writes a file; they run with the run's GOROOT mirror,
  `GOTOOLCHAIN=local` and `GOENV=off`, and their environment is an allowlist
  that keeps the programs go runs (`CC`, `CGO_*FLAGS`, `PKG_CONFIG`, ...) and
  any unknown Go setting at the run's own values, and `GOMAXPROCS` within the
  clamp; the C compiler and `pkg-config` go finds by name are pinned to where
  the run's own PATH finds them (`commandshim.NestedGoPrograms`, recorded by
  the broker in the policy); `go version` and `go env` run as probes; other
  go verbs are refused;
- nested `gofmt` gets the formatter limits;
- nested `git` may change only a repository in the run's TMPDIR, nested in
  its worktree or in a system temporary directory, never the run's own (the
  operator's, which every task worktree shares) or one elsewhere, which it may
  only read, nor one whose `.git` links git would write through lead
  anywhere else; files it names for git to write must lie there too; a push
  or `git remote` call is refused everywhere, 78 for a Local task and 77 for
  any other, since external effects go through the daemon's effect grants; on
  any repository, what the call itself gives git that names a program for git
  to run (options, `-c` settings and the same settings in a file it names or
  a `git config` it makes, variables such as `GIT_SSH_COMMAND`), any `GIT_*`
  variable the policy does not know, and a URL off this host are refused with
  77, as is any `hook.*` setting the call gives git but one that switches a
  hook off; `git worktree repair` checks every worktree it would rewrite,
  the ones its repository lists included; every path a call names is checked
  as the system takes it when git creates and writes it, never cleaned as
  text (a link is followed, a directory git would create counts as one, a
  `..` leads up from where the previous component lies, and a link leading
  nowhere is refused; a link switched between the check and the exec remains
  the documented race); and the
  real git runs with
  `GIT_CONFIG_GLOBAL=/dev/null` and
  `GIT_CONFIG_NOSYSTEM=1` unless the call names those files, so `HOME`
  chooses no configuration, with `GIT_ALLOW_PROTOCOL=file`, so it reaches only
  local repositories, with no hook, file-system monitor, bundle URI or trace2
  target from a file, and pinned to the canonical repository the policy
  checked;
- other build tools are refused with 77.

The task's mode comes from the run's context: the daemon sets `NestedRun`
(task, dispatch, Local) with `WithNestedRun` around a dispatch's host
commands, and a run without one is treated as Local. An accounted call takes
no lease of its own. It stays in the run's process group, task cap and lease
window, and is retired with the run. The lease bounds the run's weight, not
how many nested calls run at once: each call is clamped, but only the task cap
bounds how many run together (a test may start them from several goroutines). The wrappers apply policy to what
reaches them and do not contain the command: a test can still run a tool by
absolute path, and programs git itself starts (the configured programs of a
test's own repository that the test wrote there itself, and the operator's
repository's own configured programs on a read) run the real git, which
never reaches the wrappers. No hook runs on a call that passes the policy:
hooks are switched off for every call, and a call's own `hook.*` setting that
would switch one back on is refused (git reads it after the settings that
switch hooks off). A test's submodules are not checked, and a process that
changes a repository between the policy's check and git's start is not seen:
re-audit 10's loop, toggling a test repository's `.git/refs` between a
directory and a link into the operator's, got 36 of 150 `update-ref` calls
through to create refs in the operator's repository. See
commandshim/README.md.

Evidence then carries `nested_count`, the exact number of calls, refused ones
included, and `nested`, the first 16 calls in time order (argv, directory,
time), each bounded. Every run's evidence also carries `command` and
`started`, its identity. Before `Run` returns account-mode evidence, it passes
the evidence to `RecordNested` when that is set, with the run's context. A
recording error fails the run. The live daemon stores each run's calls as
marker `nested-commands/<run id>`: one per run, keyed like the engine run row
by dispatch, command and start (the broker's start, so the key is not the
row's ID), and naming the task and dispatch, so two runs with the same output
keep a record each. See [commandshim/README.md](../commandshim/README.md) for
the wrapper script, the policy and their bounds.
