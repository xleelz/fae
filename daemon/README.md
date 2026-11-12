# Durable daemon

`Start(ctx, Config)` binds a canonical workspace (which must equal the process's
current directory), acquires the singleton lock, initializes and supervises a
private authenticated Dolt server, migrates the plane, loads validated durable
jobs, and starts the Unix API and asynchronous drive. Call `Run(ctx)` to wait and
close, or `Close(ctx)` when embedding. The root is an explicit parent directory
containing named instances; production instance roots must be outside Git
repositories. A new instance has `run`, `plane`, `worktrees`, `backup`, `archive`,
`dolt-config`, workspace binding and supervision log entries.

Without `Config.Live` the scripted executor is selected. `Config.Live` (CLI
`--adapter direct`, or `multiplexer`, the CLI default) selects a configured trusted
bridge with real host commands and guarded Git effects; see [LIVE.md](../LIVE.md).
Scripted outputs are simulated rehearsal evidence: reaching Done does not mean a real command ran, a worktree
changed, or a commit was integrated. Arbitrary custom executors are an embedding
interface, not an implemented production agent adapter.

## RPC

Every call requires the mode-0600 credential, as 64 hexadecimal characters in the
Authorization header. POST `/v1/rpc` uses `api.Request` version 1. `authority.go`
lists every method with its read flag or action class; `fae help` lists the CLI
verbs over them. Every write other than Message requires an `authorizing_message`
identifying a previously persisted operator message.
Admit's idempotency key compares the original canonical client request, before
resolving a moving workflow revision or workspace HEAD. Retries return the
original lightweight identity/version/bindings response and never rewind the current checkpoint.
Every other keyed mutation holds only its own key's turn across its lookup,
the mutation and the recorded result: a retry under the same key waits for
the original and replays it, and mutations under different keys never wait
on each other.

Board reads an immutable cached projection, merged by id with the plane's rows of
tasks finished before the daemon started ([reads](READS.md)), and accepts `after` (task ID) and
`limit` (default 100, maximum 500), returning `items`, `next_cursor` and
`snapshot_at`. History retains its full response for small tasks; larger tasks
return typed `response-too-large` guidance. `History` with `summary:true`, `after`
(event sequence) and `limit` returns events and the artifact/provenance references
for their completed dispatches, without document or artifact bodies. Both paged
reads shrink to a 900 KiB encoded budget. The transport independently bounds all
unary responses below 1 MiB and returns a typed error instead of truncating JSON.
Status includes counts and snapshot age (task rows with `tasks:true`); detailed
task rows are on Board.

GET `/v1/events?after=N` streams committed events as NDJSON using the same
credential. Connections are capped at 16, each batch at 100 events, and writes
have a deadline. Commit notifications wake readers, so idle streams do not poll.
Retention windows, the durable logical floor and explicit gaps are described in
[READS.md](READS.md); archived event rows are never deleted.

## Execution and restart

One serialized writer persists engine decisions before asynchronous dispatch and
persists collection before subsequent stages. `MaxInFlight` caps tasks with work in
flight; the default -1 is unbounded, and `runtime-config --tasks-in-flight` changes it. Idle drives block on completion/admission/child events.
Status reads a cached projection without acquiring the writer mutex or querying
SQL. Other reads have a pool bound of eight and the transport's deadline.

Graceful stop (`Close`) stops the drive, cancels turns other than commit and
integrate (which may finish within `Config.DrainBound`, default 120 s), collects
finished turns, and writes a clean-stop marker listing the in-flight set before
stopping the child; on overrun it cancels the rest and still reaps children.
Close joins every cancelled turn (bounded by the drain bound) before writing the
marker, and a launch cancelled inside the stop-before-exec gate is proven retired,
so a planned stop with proven retirement replays deterministically.
Restart validates payload hashes, task and workflow bindings, executor kind and
target workspace; an invalid job is quarantined (audited alert, `quarantined` in
status) instead of refusing the instance, and stays out of the drive until
`fae quarantine release|retire` (see `TASK_CONTROLS.md`); only a pane limit
below the workflow floor refuses startup. The scripted executor replays an
interrupted token; a live local effect whose completed daemon receipt survives
is adopted with remeasured postconditions and no charge; a live turn listed by
the marker replays without charge once its workspace and session retirement
are proved; others park as uncertain. A lease proof left by an in-process
restart (same pid and birth) cannot prove its owner dead and is kept, so that
turn parks. A per-task error is classified once (`taskFault`): plane transients
defer the task, a diverged cache re-adopts the durable row, invalid rows are
quarantined, and only an unrecoverable plane ends a drive generation. A failing
housekeeping step (backlog checkpoint, runtime config, idle sessions, crons,
loops, stuck-commit detection) or round read (provider health, backlog, order)
is skipped for that round with a `drive-step-fault` alert per episode and
`drive.step_faults` in status; a loop collection that cannot be checkpointed
is retried, and one for no in-flight loop is dropped with a
`loop-completion-dropped` alert. A generation ended by infrastructure re-arms
itself with bounded backoff (1 s doubling to 60 s, one `drive-rearm-refused`
alert per episode) until the 3-per-cause-per-hour budget is spent. Every round
records why each ready task was not dispatched (FAE-SCH-010): besides the
scheduling reasons, `edge_unread` (its dependency rows could not be read),
`release_changed` (it changed while its release ran without the writer lock;
the next round decides it afresh), `resident_unretired` (its resident turn
deferred because the retirement of its slot is unproven; the next attempt runs
on another slot) and, for every task of a round that ends
before deciding any, `plane_unconfirmed` (checkpoints not yet re-confirmed
after a plane restart), `checkpoint_unsettled`, `draining` or `round_read` (a
failed provider-health, backlog or order read). A round that meets a pending
checkpoint mid-round also ends there, withholding the tasks it had not yet
decided as `checkpoint_unsettled`.

**Startup checks.** As soon as the control plane is up, before `Migrate`, the
default workflow's install/upgrade or any lifecycle event or marker touches
the plane, an existing plane's schema bound, event clock and stored workflow
revisions are checked (FAE-WF-093, FAE-CP-006), so a refused start leaves an
older schema at its version for the binary that wrote it. Once migrated, the
start checks the plane's full consistency (`CheckConsistency`, by key and
index, every active task a page of 1,000 diverged tasks at a time, each read
within 10 s). An event clock behind the plane's own events refuses the start,
naming the damage, and so does a check that fails or whose read takes longer
than 10 s. The stored-revision check reads every stored workflow revision
(FAE-WF-093), a page of 500 at a time with each read within 10 s and each
distinct content checked once, with no bound on the whole; the revision this
start's default install writes counts as checked, since its write validated
it. Every revision ever written must stay loadable by later binaries, and a
stored revision falls in one of two classes (operator decision, 2026-10-30).
One this binary cannot read (`workflow.Load`: not canonical JSON, an unknown
or mistyped field, a schema version it does not know) refuses the start: the
error and a supervision line per revision name the workflow, revision, hash
and error, the active (non-terminal) tasks pinned to it (directly or through a
`sub_workflow` stage) and whether new admissions bind it, with the recovery
(start the binary that wrote it again; restore the plane from a backup if its
stored content is damaged), and an audited `unloadable-workflow-revision`
alert records it. A revision that cannot be read within the bound refuses the
start too. One this binary reads but cannot run (the rule admission uses,
`workflow.Parse`: a structural rule the engine relies on, such as a Gate's
expression, the start and edge endpoints, primitives and Loop caps, or a rule
added after it was stored, such as the old default's `main` brief keys)
refuses the start while a non-terminal active task is pinned to it directly or
through a `sub_workflow` stage (a terminal task loaded only for ticket-outbox
delivery or a row an operator retired from quarantine is not an active pin):
the error, a supervision line and an audited `unrunnable-workflow-revision`
alert name the workflow, revision, hash, failing rules and tasks, and the
recovery, which needs no older binary. `fae serve ...
--quarantine-unrunnable-revisions` makes one start proceed and quarantine
exactly those tasks (an audited `task-quarantined` alert and event each,
reason `unrunnable-revision: ...`; `fae status` lists them under
`snapshot.quarantined` and `startup_checks.revisions.quarantined`); the flag
is stripped from `run --start` and controller maintenance/roll launch specs
(`RelaunchArgs`), is a no-op once no such task is left, and subsequent starts
without the flag keep those already-quarantined tasks in quarantine until they
are migrated or retired. A quarantined task leaves quarantine by `fae migrate`
to a revision this binary validates, after which its unrunnable-revision entry
is cleared and it is loaded as a release would load it (re-quarantining under
the new cause if another check fails), or by `fae quarantine retire`, which
keeps its history; `fae quarantine release` refuses while the revision stays
unrunnable, and `fae quarantine inspect` says so. When stored jobs load, each
non-terminal job's embedded workflow document is also validated
(`workflow.Validate`), quarantining any row whose embedded document is
damaged. A task whose turn was in flight when its daemon stopped keeps that
dispatch in quarantine, so `fae migrate` refuses it (in flight) and `fae
quarantine retire` is its way out. An unrunnable revision no active task is
pinned to does not refuse the start: a supervision line names it, status lists
it under `startup_checks.revisions.invalid`, an admission or migration that
binds it is refused, and the default upgrade replaces a builtin default among
them (its listing then says it no longer admits).
Then a task whose task row, or
backlog row, no longer records its snapshot's state is quarantined, as after
a plane restart; `fae quarantine inspect` names the divergence, and `fae
quarantine release` sets those rows to the snapshot's state in an audited
write (`quarantine-release-repair`) before it loads the task, so the next
start does not quarantine it again (a missing task row refuses the release).
The start then removes the scratch of killed gate runs and reports what
`scratch-quarantine` holds (see [scratch](SCRATCH.md)). Status reports these
checks as `startup_checks`. When the start recovers its
process registries (Dolt's, sessions, host commands and the UI), an empty
record an earlier process left (last written more than 50 ms before this
process started) is removed with a supervision line naming it and an audited
`empty-process-record-removed` event; any other unreadable record refuses the
start, naming its file.

**Rolling back across wave 9.** The stored task snapshot is decoded strictly
(`decodeEnvelope` refuses unknown fields), so an older binary can't load a
snapshot that holds a field added after it. Wave 9 added two:
`route_generation` on a dispatch and `refused_routes` on the task. Every live
agent dispatch sets `route_generation`. A snapshot holds it while an agent turn
is in flight (`in_flight`), and for good once an uncertain agent dispatch has
been resolved (`resolved_dispatches`).
`refused_routes` stays from a binding refusal until Resume or a restart with
information clears it. At its start, a wave-8 or older binary quarantines each
task whose snapshot holds one of these fields (`invalid durable job <id>: ...
unknown field`). That includes a task stopped with an agent turn in flight and a
finished task that once had an uncertain agent dispatch resolved. The task's
row is left as it was, and the other tasks and the instance run. The plane
schema is unchanged (11), so rolling forward again to a wave-9 or later binary
loads those rows as before: quarantine is held in memory, and only an
operator's `fae quarantine retire` is stored. Don't retire them under the older
binary unless you mean to retire the tasks. Fields earlier waves added (for
example `carries`, `carry_entry`, `admitted_at` and `superseded_acceptance`)
roll back the same way.

**Rolling back across wave 10.** Wave 10 added no stored field, but it parks a
task whose every route the bridge refused with the reason
`all-routes-unavailable: every route for role R was refused by the bridge ...`.
Wave 9 recognises only the exact reason `all-routes-unavailable`: it never
decides such a park again (so neither a restart nor the route-exhaustion bound
moves it), raises no `route-exhaustion` alert for it, withholds it as
`parked`, and its Resume refuses it. Under wave 9 the task stays parked until
an operator escalates it (`fae escalate --task ID`) and restarts it with
information, which re-enters the workflow's first stage.

**Rolling back across wave 11.** Wave 11 adds no stored field, but it keeps a
task's `routes_unavailable_since` where wave 10 cleared it: on a
ProviderUnavailable escalation, and from a dispatch until that dispatch's
provider runs (wave 10 clears it when it dispatches). Wave 10 clears it only at
a dispatch or an escalation, so under wave 10 a task still carrying it (a
ProviderUnavailable escalation restarted with information, or a dispatch wave
11 issued and wave 10 collects) that parks for want of a route before its next
dispatch counts from the old clock, and escalates ProviderUnavailable at once
if that clock is past the bound; the escalation clears it. Wave 10 also dates
the `route-exhaustion` alert by the task's latest park event, so an episode
wave 11 already alerted can alert again, and it raises none for a
ProviderUnavailable escalation.

Wave 11's other changes roll back as follows; roll the provider bridges back
with the daemon.
- **Bridge refusal classes.** A wave-10 runtime doesn't know `provider-failed`
  or `edits-not-applied`, so a wave-11 bridge's record of either reads as an
  untyped `process-failed` turn and parks UncertainDispatch. Under wave 10 a
  later reviewer's start failure parks UncertainDispatch again, and Prophet
  stops relaunching or charging a provider that never acknowledged the prompt.
- **Events and status.** Task history may hold `scope-dropped` events and the
  plane `empty-process-record-removed` and `unloadable-workflow-revision`
  events; wave 10 reads them as plain events. An approved path wave 11 dropped
  stays dropped. The status field `startup_checks` disappears.
- **RED reasons.** A derived RedReport that hasn't frozen and whose fragment
  wave 11 accepts but wave 10 refuses (`expected error`, `want error`,
  `got error`) is refused again by the wave-10 resolver, so its RED dispatches
  fail ValidationFailed; restart the task with information so test-first writes
  a new report. Frozen REDs are not judged again.
- **Scratch.** Wave 11 adds `<instance>/scratch-quarantine/`, where the start
  sweep moves a scratch tree it can't remove. Wave 10 ignores that directory,
  but its own start sweep still fails `Start` on a read-only tree left under
  `worktrees/command-*`. Before rolling back, make such trees writable and
  remove them (`chmod -R u+w`, then `rm -rf`).

**Rolling back across wave 12.** Wave 12 adds one stored field, a task's
`pending_delete`, and otherwise changes how outcomes are counted and shown:
- **Live task verbs.** Rolling back removes the TaskCreate, TaskUpdate,
  TaskDelete and TaskGet RPCs and `fae task`. A wave-11 daemon quarantines a
  task whose row holds a `pending_delete` (a delete waiting for its turn in
  flight), so let pending deletes finish, or retire those tasks, first. The
  `task-edited`, `task-deleted`, `task-delete-refused` and `task-delete-failed`
  events, the `task-delete-failed` alerts and the payload-bearing
  `backlog_edge_changed` events (now on both tasks) stay as inert history.
- **Never-executed runs.** Wave 12 counts agent starts per binding under
  `startup-recovery:<stage>:<provider>/<model>`, and refusals before launch under
  `same-gate:<stage>:refused-before-launch` with failure causes starting
  `RefusedBeforeLaunch:`. Wave 11 ignores the per-binding start keys (a turn in
  flight counts its starts from zero until the turn ends and clears them) and
  treats the same-gate key as one more reason; the `refused-before-launch`
  result class is never stored. After a rollback, refusals before launch, and a
  later reviewer's start failure, refusal or deferral, charge attempts again,
  and a later session's deferral parks UncertainDispatch.
- **Bridges.** Wave 12's bridges add `--start-timeout`; a wave-11 bridge rejects
  that flag (exit 2), so roll back any bridge command that passes it along with
  the bridges. Without the flag a wave-12 bridge uses 180 s. Under wave 11 a
  provider silent until the bridge timeout is `provider-failed` again, and a
  silent clean exit is refused output again. Loop turns' provider observations
  are ordinary `provider-observation` audit rows, events and health updates,
  keyed by the loop session (`loop-<id>-<iteration>-attempt-<n>`) with job id
  `loop-<id>`; a wave-11 daemon reads them as health and stops adding new ones.
- **Startup, scratch and cleanup.** `quarantine release` writes a
  `quarantine-release-repair` audit row and the start a `scratch-quarantine`
  alert, which wave 11 lists as plain audit rows and alerts. `gate run` writes
  `gate-runs/<id>.owner` beside its scratch; wave 11 neither sweeps `gate-runs`
  nor removes those records, and wave 12 leaves in place, logged, a scratch an
  older `gate run` left without one. Wave 11's strict decoder refuses
  `CleanupRetired`'s `done` parameter, so `fae cleanup done` fails against it,
  and status loses `startup_checks.scratch_quarantine` and the process report's
  `defunct_in_commands`. Wave 11 starts on a stored revision wave 12 refuses (it
  only reports it); that is the recovery the refusal names.
- **Landing and redaction.** Rolling back restores pathspec-interpreted task file
  names in landings and integration, the flake rerun that skips a flake's
  namesakes everywhere and doesn't compare the tests run 2 ran, and the
  redaction leak at `go test -json`'s 1 KiB event cut. Nested git's hook,
  `worktree repair` and physical-path checks go too. Nothing is stored for them.
- **`fae-tui`** is a client only: the daemon stores nothing for it, and dropping
  `cmd/fae-tui`, `tui/` and the charm requirements (restoring
  `golang.org/x/sys` v0.30.0) removes it.

**Rolling back across wave 13.**
- **Startup and topology.** Wave 13 adds `serve --quarantine-unrunnable-revisions`,
  the `unrunnable-workflow-revision` alert and event, the quarantine reason
  prefix `unrunnable-revision:`, status's `startup_checks.revisions.quarantined`,
  Topology's `include_retired` parameter and a molecule's `error` field; nothing
  is stored for them but the alert and event rows, which a wave-12 daemon lists
  as plain audit rows and alerts. A wave-12 binary rejects the serve flag (exit
  2) and its strict decoder refuses Topology's `include_retired`, so `fae
  topology --include-retired` fails against it; a wave-12 daemon starts on a
  revision wave 13 refuses (it lists it and drives the tasks pinned to it, which
  is the failure the refusal prevents), and fails the whole Topology page again
  on a molecule whose revision it cannot run. Quarantine is held in memory, so a
  task wave 13 quarantined for its revision is driven by wave 12 unless it was
  migrated or retired first.
- **Bounds and refunds.** Wave 13 adds the per-turn counters
  `agent-starts:<stage>` and `admission-deferred:<stage>` to a task's failures,
  which wave 12 keeps as unknown keys until the task's state changes, and the
  `effect/<hash>/refused-before-exec` marker beside an effect intent, which wave
  12 ignores, reading such a dispatch as started and keeping it uncertain. The
  `commit-refused-before-write` result class is never stored. After a rollback,
  a later session's deferral and a start failure on a binding another task
  heals are unbounded again, and a runtime `launch-refused`, a failed
  `Launching` write and an effect or commit refused before it ran are charged
  again, as in wave 12.
- **Live task verbs.** Wave 13 adds one stored field, a task's
  `superseded_fields` (the declared files, test filter and test shape a turn in
  flight was dispatched under, written when they are edited during a turn).
  Wave 12's strict decoder quarantines a task row holding it, so let such
  turns finish and the task reach a terminal state, or retire it, before
  rolling back (the field is inert once its dispatch has ended but stays in the
  row). TaskGet's `pending_delete` object and `edits_truncated`, the Board's
  `pending_delete`, the `requested_at` in delete-refusal events and the
  `repointed_from` edge events are results and history a wave-12 daemon
  doesn't read. A wave-12 `fae task create` sends `priority: 0` for an omitted
  flag, which a wave-13 daemon refuses; use the wave-13 CLI with it.
- **Cleanup, recovery and leaks.** Wave 13 records a `worktree-cleaned` event
  (with an audit row) on each task whose worktree and branch `fae cleanup`
  removed, and `CleanupRetired` takes `limit`. A wave-12 daemon keeps those
  events as history, plans the cleaned tasks again (finding them already
  removed, with archive evidence) and refuses `limit`. The `plane-recovery`
  marker gains `checked` and is written twice per recovery (audit outcomes
  `unchecked`, then `ok` or `inconsistent`); wave 12 reads its other fields
  unchanged. Wave 12 ignores the process report's and `process-leak` alerts'
  `defunct_parents`, and again exempts every defunct process inside a running
  command.
- **Landing and redaction.** Under wave 13+, a plain-name `test_filter` stored
  before wave 13 (`TestA`) is handed to `go test -run` unanchored and matches
  by substring (`TestAB` too) unless edited to `^TestA$`. Rolling back to wave
  12 restores a landing filter gate that quoted the task's `test_filter` and
  counted one literal name, so a task admitted with a regular expression
  (`^TestA$`, `TestA|TestB`) is held at landing with "matched zero tests" until
  its filter is edited to a bare name.
  It also restores the roll overlap guard that didn't see renames, the format
  and own-hunk gates that skipped or held every type change, and the redaction
  leak at a 1 KiB cut inside an escape or at an HTML reference in `-json`
  output. Nothing is stored for them.
- **Nested git and the TUI.** Wave 12's resolver takes a missing directory
  before a link as text again, so `scratch/n/../l/..` can write into the
  operator's `.git`; a dangling link is followed again. `fae-tui` from wave 13
  sends `expected_version` and reads `pending_delete` when the daemon reports
  it; a wave-12 daemon accepts the first and doesn't report the second, so the
  TUI falls back to the stored delete's older fields. Nothing is stored.

The writer lock is taken per task and per housekeeping step, so mutating RPCs
never wait for a whole round. These run without it, so an admission never
waits for them: a drive checkpoint's plane round trips; a released task's
dependency read and its refork onto the integration tip (git commands and fork
checkpoint); idle-session maintenance (`herdr-core` census, session retirement and
audit rows); and the plane reads a writer only checks, which the RPC makes
before taking the lock and re-checks under it: whether the plane stores an
admitted task's identity; run-wide resume's active backlog, latest run, run
members and their finished rows; and whether the plane stores a backlog edge
edit's two tasks, its dependent's edges (by key) and the rows of the
dependent and its prerequisites. Under the lock, the drive's state
comes first, and a read that failed or no longer stands is made again: the
latest run once any admission row was written after it was read, a
prerequisite missed because the dependent's edges changed, and the identity
of a task whose admission row write returned an error (it may have committed with the
acknowledgement lost, leaving a stored task the drive does not hold). Other
mutations wait for a drive checkpoint, dependency read or refork in flight
rather than observe it half done. A refork's tree move runs outside the
worktree manager lock, so another task's worktree creation never waits for it.
These still hold the writer lock across plane, git, host-ledger or other I/O,
so any mutation, an admission included, can wait for one of them
(FAE-ROB-021):
- every mutating RPC's own plane reads and writes, apart from the reads made
  before the lock: authorization, Admit's idempotency lookup, the read-back
  of an uncertain backlog batch (on every mutation until it settles), and
  each RPC's rows. Beyond plain rows:
  - an admission (Admit, a backlog item, a cron occurrence or a sub-workflow
    child): its workflow, target and adapter reads, a version-3 review
    target's prepared-workspace `git` verification, a live review
    submission's review-target check (`git` and the external baseline read),
    the integration tip, worktree fork and admission row; a backlog
    admission also reads the graph and its items;
  - an edge edit's dependent's edges, read again by key, and a blocking
    add's cycle check: a walk from the edge's two ends, one keyed statement
    per level, never the whole graph (FAE-CP-009); run-wide resume's reads
    that failed or no longer stand, then one Resume task control per task;
  - task controls: a finished task's row, Cancel and Retire's held-dependent
    read, Retire's successor row and a `contained` retirement's
    `git rev-parse --verify` and salvage receipt read, live Resume and
    RestartWithInformation's workspace check (worktree verification,
    `git rev-parse`, `merge-base`, broker tree), and the checkpoint;
  - Controller: the binary hash, the watchdog grant read, action rows;
    `prepare`'s `git rev-parse`/`diff`, gate marker and, per runnable task,
    worktree `Ensure` and `git diff`/`ls-files`; `accept`'s integration
    fast-forward; `rearm` (a drive re-arm, below);
  - ResolveUncertain: worktree `Ensure`, effect receipts, broker tree and
    `git` checks, commit-index reconciliation, receipt lease recovery (role
    lease rows and the host ledger) and the checkpoint;
  - RuntimeConfig: its marker and the host and pane ledgers; Reclaim: lease
    pool rows, the host and pane ledgers' dead-owner reclaim, journal lease
    recovery and audit rows;
  - TargetConfigPut: adapter reads, the review-workspace check and, for a
    live Normal target, external tool schema discovery and the review
    target's `git` and baseline checks;
  - QuarantineRelease: target re-validation, receipt adoption (worktree
    verification, host-ledger lease recovery) and the checkpoint;
    QuarantineRetire, Migrate, Decide and AttachDesignDoc: their reads and
    checkpoint;
  - ReconcileDone: the mismatch read and a row per task, dry runs included,
    and a write per reconciled item; Schedule: loop and cron rows and a
    cron's workflow and target reads;
  - AlertAcknowledge: the alert row and the alerts mirror, whose own lock an
    observability round holds across its plane read;
- the housekeeping steps: backlog checkpoint reconciliation, runtime config
  (its plane read and the host and pane ledgers), stale role leases, crons
  (each occurrence's admission, its existing-task read and `git worktree add`
  included, and the schedule rows), loops, stuck-commit detection and
  sub-workflow reconciliation (a child's admission and fork);
- the drive's own I/O: the drive-exit audit; a drive re-arm, automatic,
  after a plane recovery or the controller's (its budget marker, loop
  checkpoint reconciliation, settling, a `GetJob` of every cached task and
  the refusal alert); settling an unverified checkpoint (its `GetJob`),
  collection's startup-exhaustion provider observation, loop collection
  checkpoints, re-validation after a plane restart (its stale-row reads,
  target re-validation and quarantines), quarantine, step-fault and
  dropped-loop alerts, checkpoints written outside a drive checkpoint
  (pane-starvation escalation), and a plane restart met during a checkpoint
  retry;
- worker callbacks: each provider observation (two plane round trips), a
  Prophet turn's reply rows and completion marker and the task control it
  enacts, and a salvage landing's Retire task control. A Prophet session
  start holds the lock only to reserve its slot, with no I/O; its reads,
  message, marker and chat rows, and route observations, run without it;
- Close: the final completion drain, the UI and RPC server stops, session
  retirement, the pane and host ledgers' release, and the stop marker.

**Redaction (FAE-SEC-003).** The daemon redacts three kinds of captured
output before it keeps them:

- **A failed live host command's excerpt** (the `engine_feedback` block; see
  [execution/README.md](../execution/README.md)). It is redacted before it is
  stored and again before it reaches a brief, so an excerpt stored before
  this rule is redacted too.
- **An account-mode run's nested calls** (marker `nested-commands/<run id>`).
  Each call's arguments and directory are redacted before the marker is
  written. Where the shim cut a field at its bound, a trailing start of a
  secret, 4 bytes or more, is replaced too. The record's command is the run's
  own, as its engine run row records it.
- **A landing's gate output.** Each gate command writes to files that are
  unlinked as soon as they are created. The landing scratch
  (`<instance>/landings/<landing>-<n>/`) keeps only redacted copies, as
  `gate-output-*` (stdout) and `gate-stderr-*`. The gate results, failing test
  names included, are parsed from the copy. The unredacted stdout is parsed
  too, in memory, and only its counts are compared with the copy's: executed,
  matched and matched-passing tests, packages, failures, listed flakes among
  them, and build failures. If any count differs, or only one of the two
  parses, the landing stops with the `infrastructure` outcome, giving both sets
  of counts and no names, and keeps the attempt on its held branch: nothing
  about the change failed. So a redaction that renames a test only changes the
  name shown, but one that merges two packages or tests into one name, or hides
  an event, stops the landing (FAE-CTL-030).

These secrets become `[redacted]`:

- the operator credential, the plane credential (its hex is the Dolt
  password), each delegated principal's credential, the UI token and the UI's
  read and write credentials;
- the values, of 8 bytes or more, of credential-named environment variables
  (names containing TOKEN, SECRET, PASS, CREDENTIAL, KEY or AUTH): the live
  configuration's, and the daemon's own. No engine run or landing gate is
  given the daemon's own, but any of them can read them through `/proc`.
  These names contain one of those parts but carry no secret, so they are
  left out: `GIT_AUTHOR_NAME`, `GIT_AUTHOR_EMAIL` and `GIT_AUTHOR_DATE`,
  `SSH_AUTH_SOCK`, `XAUTHORITY`, `SSH_ASKPASS`, `SSH_ASKPASS_REQUIRE`,
  `SUDO_ASKPASS`, `GIT_ASKPASS`, `GNOME_KEYRING_CONTROL` and
  `PASSWORD_STORE_DIR`. Redacting them would rewrite ordinary output, and the
  test names gate results are parsed from;
- any provider-shaped credential, whatever its source (see below).

Every secret in the first two groups is found in the same forms:

- its bytes;
- its hex, in any mixture of cases, written plain or with a space between
  bytes (Go's `% x`);
- Go's other views of its bytes: `%v` and `%d` (`[115 101 …]`), `%#v`
  (`[]byte{0x73, 0x65, …}`) and `% #x` (`0x73 0x65 …`), the last two in
  either case, `%o` (`[163 145 …]`) and `%b` (`[1110011 1100101 …]`);
- Go's views of it as text, a `[]rune`: `%q` (`['s' 'e' …]`), `%U`
  (`[U+0073 U+0065 …]`) and `%v`, each byte that isn't UTF-8 as U+FFFD, as
  Go converts it;
- its base64, standard or URL-safe, padded or not;
- its base64 inside a larger encoded value, such as an `Authorization: Basic`
  header's `user:key`, wherever the secret starts within base64's 3-byte
  groups. The characters that encode any of the secret's bits are replaced,
  including the one on each side shared with its neighbours. A shared
  character that is itself percent-escaped is kept, and with it at most 4 of
  the secret's bits;
- its base32, in the standard or the extended-hex alphabet (base32hex),
  padded or not, in either case;
- its UTF-16, little- or big-endian (as raw bytes, or with each NUL escaped,
  as `go test -json` writes them);
- a hex dump of its bytes with offsets: `xxd`, `hexdump -C`, `od -t x1`, and
  Go's `hex.Dump`, which testify prints a `[]byte` with. A run of dump lines is
  decoded, and every line holding a byte of the secret is replaced whole,
  offset and text column included; the dump's other lines are kept. A dump
  line may follow indentation, diff markers and a `go test` log prefix
  (`name_test.go:12: `), and may be a `go test -json` event's output. Lines
  other than a run's last must hold 8 bytes or more (the tools write 16).

Each form is also found encoded: percent-encoded (`%XX` with hex in either
case, or `+` for a space), with JSON or Go string escapes (`\xNN`, `\uXXXX`
including surrogate pairs, `\UXXXXXXXX`, `\n`, `\"`, `\\`, `\/` and the
like), or as HTML character references (`&#115;`, `&#x73;`, `&amp;`, `&lt;`,
`&gt;`, `&quot;`, `&apos;`, as `html.EscapeString` and `html/template` write
them). An escape may be JSON-escaped once more, as `go test -json` prints a
test's `%q`. In `go test -json` output, where test2json writes a test's `&`
as `\u0026`, a reference is found with its `&` so written (`\u0026#115;`).

Each form is also found wrapped across lines, at any column: up to 32 line
breaks inside one occurrence, with at least one byte of the form between two.
A break is a newline (`\n` or `\r\n`) written raw, escaped once (inside a
JSON or Go string) or escaped twice, followed by up to 16 spaces or tabs of
indentation (`go test` indents a log message's later lines by 8). A break may
stand for one of the form's spaces, as `od -A n` and a wrapped `%#v` write
them. In `go test -json` output, where each line is its own event, a value
wrapped across two consecutive `output` events of the same package and test
is found too. So is a value cut with no line break: `go test -json` cuts a
line longer than 1 KiB into several `output` events, and the value goes on
in the next event of the same test; each cut counts as one of the 32
breaks. The cut may fall between two of the value's bytes or inside one of
its escapes (`%4|6`, `\\x4|6`, `\\u004|6`, `\u0026#7|0;`), which is read
across it. The events it spans become one, which is still JSON. The whole
wrapped value, its breaks included, becomes one `[redacted]`.

Not covered:

- a value split by other text: an ANSI code inside it, a log prefix at the
  start of its next line (each part logged by its own `t.Log`), or `go test
  -json` events of another test or package, or another action, between its
  parts;
- a value escaped twice in any other way (percent-encoded twice, say);
- an octal dump (`od -b`, which pads each byte to 3 digits), Go's `%#o` and
  `%O`, `%x` of a `[]rune` holding a character beyond ASCII, base32 at an
  offset inside a larger value, the hex or base64 of a value's UTF-16, and a
  reference per byte of a binary credential (a reference names a character,
  not a byte);
- a dump of words wider than a byte (`od -x`, `hexdump` without `-C`), whose
  byte order is the host's; a dump cut inside the secret; and dump lines of
  fewer than 8 bytes (`xxd -c 4`). A dump without offsets (`xxd -p`,
  `od -A n`) is the secret's hex wrapped, and is found as such;
- the value's characters printed one by one with a space between (`%c` of
  a `[]rune` or `[]byte`, or `strings.Split(s, "")` printed, with or without
  `%q`: `[F a e …]`), `%#v` of a `[]rune` (`[]int32{70, 97, …}`), a JSON
  array of its bytes (`[70,97,…]`), colon-separated hex (`46:61:…`), and its
  bytes reversed;
- any other encoding.

The secrets are read afresh each time, from the instance's files, the live
configuration and the daemon's environment. If they cannot be read, nothing
is kept unredacted, and the reason is given: the excerpt is withheld, and the
nested calls are stored with only their times. A landing stops with the
`infrastructure` outcome, not as a failing gate, and keeps the attempt on its
held branch; landing it again once the secrets can be read gates it afresh.

**Provider logins the daemon is not given as values.** The daemon never reads
a provider login it is not given as a value, such as the files under a
`CLAUDE_CONFIG_DIR` or a `CODEX_HOME`. Reading a login in order to redact it
would widen its exposure and still not be reliable:

- the daemon would hold the provider's long-lived refresh token in memory and
  pass it through every redaction path;
- it would depend on each CLI's private file format, which can change
  between versions;
- it would still miss a token the CLI refreshed after a run printed the old
  one.

So the boundary is this. A login's credentials are redacted by their shape,
or by the field that holds them, wherever they appear (`secretshape`):

- **Anthropic** (an API key, or a token Claude Code holds): `sk-ant-`
  followed by 16 or more letters, digits, `-` or `_`;
- **OpenAI API keys**: `sk-` followed by a run of 32 or more of those
  characters that contains `T3BlbkFJ`, which OpenAI's API keys carry (it is
  "OpenAI" in base64); or, without it, `sk-` with no letter, digit, `-` or `_`
  before it, followed by a run of 40 or more that holds an upper-case letter,
  a lower-case letter and a digit, as a key's random part does. A bare `sk-`
  also ends ordinary words such as `task-1` and starts a few such as
  `sk-learn`, which neither rule takes;
- **JSON Web Tokens**, such as the ID and access tokens of a ChatGPT login
  in Codex's `auth.json`: `eyJ` and 10 or more base64url characters, a dot,
  the same again, a dot, and the signature;
- **OAuth token fields**: the value of a `refresh_token`, `access_token` or
  `id_token` field (or `refreshToken`, `accessToken`, `idToken`, or
  `refresh-token`, `access-token`, `id-token` as a header or flag writes
  them; in any case), 16 or more of the characters a token holds, after a `:`
  or `=` and quotes written plain or escaped once or twice, with up to 16
  spaces each side of the `:` or `=`. This is how the refresh token of a
  Codex ChatGPT login is found: its format is not published, so it has no
  shape to match;
- **HTTP Bearer credentials**: after `Bearer` (in any case, with no letter,
  digit or `_` before it) and one to 16 spaces, 16 or more of the characters
  a token holds, with a letter and a digit among them, as in `Authorization:
  Bearer <token>`. Prose such as "Bearer tokens are…" is kept.

Each is replaced up to 8 KiB of the text it was found in. A token wrapped
across lines is found whole when the part before the break is too short to
be found alone: an `sk-ant-` run cut under 16 characters goes on over up to
4 line breaks, and a JSON Web Token over breaks inside its header or payload
or before a dot. A break is a newline written raw or escaped once or twice
(inside a JSON or Go string, or `go test -json`'s view of a `%q`). In `go
test -json` output, a test's consecutive `output` events are read as one
text, so a token `go test -json` cut at 1 KiB is found whole (a JSON Web
Token of a ChatGPT login is often longer than that), and the 8 KiB includes
the JSON between the events. Not covered:

- a token printed alone with none of these shapes or fields, such as a bare
  refresh token or a token after an `Authorization: token` scheme;
- a token whose text, with the JSON between the events it spans, is longer
  than 8 KiB;
- a shaped token's hex or base64. Its percent-encoded and JSON or Go quoted
  forms are found, since those leave its characters unchanged;
- the part of a token after a line break when the part before it already has
  the shape (for a JSON Web Token, a break inside its signature). Taking the
  next line's leading run as well would also take the ordinary text that
  follows a token on its own line, such as the next variable's name; and the
  part kept can't be used without the part replaced.

On 3.4 MB of this repository's own `go test -json` output (two full runs and a
fresh one), raw and decoded, the shapes and fields found nothing. With the
event joins, the Bearer and kebab-case rules and escaped breaks, they still
found nothing on 5.3 MB of it (four earlier runs and two fresh ones), raw,
and 0.8 MB decoded.

An engine run runs as the operator's user, so it can read the login
directory. To have a Claude credential redacted in every form above, give it
as a value instead: `CLAUDE_CODE_OAUTH_TOKEN` (from `claude setup-token`) in
the live configuration's `environment`. Any credential-named value there is
redacted in every form above.

The UI child's output goes through the same matching, for the UI token, the
hex of the UI's credentials (in any case) and provider-shaped credentials.
It is redacted a write's complete lines at a time, so a value wrapped across
two writes is not joined.

**Provider raw logs.** A bridge keeps its provider's event stream and stderr
as `events.jsonl` and `stderr.log` (owner-only files in owner-only
`<instance>/sessions/session-*/provider-*/` directories), for diagnosis. They
are written through `secretshape.Writer`, a whole line at a time, with every
shape and token field above, and the value (8 bytes or more) of each
credential-named variable of the provider's environment, replaced. Those
values are found only as their bytes, not in the other forms above. The
daemon's own secrets (its credentials, the UI's, the plane's) are never in a
bridge's environment, and a provider sees only redacted briefs and has no
tools, so they can't reach these logs. Nothing reads the logs back into
evidence: provider health takes only an outcome class from them, the bridge
its thread id, and the stall watch an event count.

Tasks resting in commit/integrate
with no worker beyond `Config.StuckCommitBound` (default 10 min) raise an alert. Child death has three restart attempts; the existing
SQL pool reconnects, schema checks run again, and uncertain checkpoint commits
are reconciled by version plus exact canonical payload before any retry.

## Remaining scope

This is not phase-complete production orchestration. Launcher sandbox checks,
parent-death watchdogs, Herdr/provider runtimes, changed-base reconciliation,
supervision-log rotation, read coalescing and finite event retention now exist.
Missing pieces include complete cross-user process/lease inventory, physical event
compaction, live-mode crash-matrix boundaries (`TestCrashMatrixEveryDurableBoundary`
covers every scripted durable boundary) and the open items in
[IMPLEMENTATION.md](../IMPLEMENTATION.md).

## Tests

`FAE_TEST_DOLT_BINARY=/absolute/path/to/dolt go test -race ./daemon ./api`
starts only isolated private children. Daemon integration scratch uses
`/private/tmp` on Darwin or `/var/tmp` elsewhere (disk-backed fixture locations),
never a developer instance. Live tests admit against real host pressure through
the helpers in `pressure_helpers_test.go` (see [TESTING.md](../TESTING.md)). Tests cover concurrent full scripted workflows,
credential and authority refusals, passwordless database access refusal, singleton
ownership, foreign-port refusal, idempotent admission, event resume including the
stream, restart with an in-flight token, faulty-task isolation, and child crash
recovery. Unit regressions cover no-write path guards, symlinks, private file
modes, environment isolation and exact large-number decoding. Formal verification
scope is documented in `../verification/README.md`.

The direct lifecycle test executes the default workflow through protocol fixture
processes that actually edit source, forced Go compilation and real test results,
reviewed-byte commits and dedicated-ref integration. Restart preserves completed
effects and parks interrupted sessions. Resource-admission tests cover host
saturation, telemetry rejection, cancellation cleanup and role scope isolation.
This is not a real authenticated model-provider dogfood run.
