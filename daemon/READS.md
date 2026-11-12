# Reads and event resumption

Plane read RPCs run in a keyed pool (8 running queries, at most 64 outstanding
keys). Identical canonical method/parameter reads for the same authenticated
actor and task scope share a query. Authorization occurs before joining. Each
waiter can cancel independently; the last waiter cancels its query. An abandoned
query retains its bounded slot until it actually returns. Read deadlines default
to 5 seconds. The optional request `deadline` is an RFC3339 timestamp and can only
shorten the transport's 5-second limit. Errors use `deadline-exceeded`,
`cancelled`, or `read-busy`. Status, Board and Topology are cached reads:
they answer from snapshots, take no pool slot, and stay responsive when the
pool is saturated, with the one exception below. Board reports
`snapshot_age_ms` and rejects snapshots older than 10 seconds with
`snapshot-stale`; Status reports `snapshot.age_ms`.

Startup materialises only active jobs (FAE-CP-009), so the snapshot holds the
tasks finished since the start but not those finished before it. Status counts
and `total` include them from startup's per-state counts. Those tasks never
change again, so their Board rows are cached too, in a finished-task summary:
the first Board or `status --tasks` read that needs them starts one build off
the request path, which pages them once from the job and task columns
(`TerminalRowsAfter`, 1,000 rows a statement, no snapshot read), and every
later read merges the summary's rows by id with the snapshot's, from memory.
The summary holds one row per task, as the snapshot does for the tasks
finished since the start; a daemon nobody reads that way never builds it.
Until it is built (and for 5 s after a failed build), those reads are the
exception: they page the plane in the read pool, so a saturated pool can
answer them `read-busy` or `deadline-exceeded`. Each page is one statement
with every Board filter in it (state, target, workflow, molecule, run, mode,
minimum priority): per state, a range over the jobs state index joined to the
task row by key, merged by id. So a filtered Board reads only the finished
rows it shows. A page with no run, mode or priority filter costs its own rows;
one with them scans the states' ranges in the plane (about 0.2 s over 100,000
finished tasks on the development host, where the summary build of those
tasks took under a second).
A finished task is never in flight, so `board --in-flight` reads none.
`status --tasks` refuses with `response-too-large` once those rows alone
exceed the transport limit, without reading a row when their count already
proves it. Rows come from the task row's columns: schema 11 adds
`execution_kind` and `mode`, and migration 11 fills them for every finished
task from its snapshot. A row the columns cannot reproduce (no task row, an
undecodable snapshot, a priority below 10 or a value longer than 255 bytes)
is decoded from its snapshot. Either way a restart changes no Board or Status
read (`TestRestartLeavesTerminalReadsUnchanged`, which compares both sources,
and `TestFinishedReadsAfterRestartReadOnlyMatchingRows`, which counts the rows
read over 300 finished tasks). Every other Board and Status read stays
snapshot-only. The global Prophet's task list reads finished tasks the same
way, from the summary once built, else paged from the columns only as far as
its bound of 1,000 summaries (PROPHET.md); it never lists the job table.
The writer refreshes snapshots on state changes and drive rounds. While no
round runs (an idle or stopped drive) nothing re-snapshots on a timer: a read
that finds the snapshot older than 5 s wakes a keeper that re-snapshots the
in-memory state off the request path with no plane read, and a Board, Schedule
or Controller read past the 10 s bound waits up to 1 s for it, so they answer
while idle (FAE-ROB-020) and an idle daemon nobody reads does no snapshot work
(FAE-ARC-002). Status never waits; it reports the age it found.

No pool read takes the writer lock. The reads that need the drive's task
checkpoints (Census, ReviewTargetHealth and the global Prophet's task list)
read them from the published snapshot, which carries a copy of the writer's
tasks and quarantine, so a writer held up in plane, git or ledger I/O under
the lock cannot hold their pool slots past the deadline (FAE-ROB-020). A
change is published before its writer releases the lock (each mutating RPC
before it returns) or, for a drive round's housekeeping step, when that round
ends; once published, these reads answer as a read under the lock would.
While a writer holds the lock they see the tasks as they were before its
changes, as a read that took the lock first would have. They ask the keeper
for a re-snapshot as Status does, and never wait for it. A census proposal is
re-checked against the task's current version and digest when applied, and a
finished task the snapshot lacks is read from the plane by key.

`serve --event-retention-age 24h --event-retention-count 100000` retains the union
of both windows: all recent events and at least the newest configured count.
A durable, monotonic logical floor survives restart and clock rollback. Startup
and an independent maintenance worker advance it through audited transactions;
the worker runs when events are written (at most once a second) and when the
oldest retained event ages out of the window, and otherwise blocks, so an idle
plane is not re-read (FAE-ARC-002). Reads only select the floor. Advancement does not emit recursive
events. Maintenance failures retain the previous, longer window until retry. Increasing
retention later does not resurrect an already expired streaming cursor.
Archival event rows are never deleted: physical storage continues to grow until
a separate backed-up compaction implementation is available. Task history and
internal forensic reads still access the complete append-only archive.

`Events {after,limit}` returns its existing event array for retained cursors. An
expired cursor returns error code `event-gap`; its JSON error detail contains
`kind`, `reason`, `requested_after`, `resume_after`, and `timestamp`.
`GET /v1/events?after=N` returns NDJSON events. An expired cursor receives a gap
first, then EOF; reconnect with `after=resume_after`. This explicitly acknowledges
the unavailable prefix rather than silently skipping it.

Each subscriber has an independent 100-event queue. Historical catch-up applies
backpressure only to that subscriber's producer. Once caught up, overflow emits
a gap with the last delivered cursor and closes the stream; resume from that
cursor in the plane. Write deadlines bound unresponsive connections. A client
whose socket cannot accept even the gap must reconnect using its own last
successfully received cursor. Neither producer nor socket writes hold a drive
lock. Subscribers also share the bounded/coalesced plane read pool.

`Alerts {after,limit,unacknowledged}` pages the plane alerts log with each row's
acknowledgement time; `AlertAcknowledge {id,authorizing_message}` is the audited
acknowledgement verb (class runtime; a repeat is refused). Every raise also emits
an `alert-raised` event (scoped to the task when it names one) and is mirrored to
`HOME/INSTANCE/alerts.log` as JSON lines (`raised`, `acknowledged`); the mirror
resumes from the file after a crash or a torn append. `HostMetrics` returns the
15-minute ring of 5-second host samples (null for an unreadable reading).
`Metrics {window_seconds}` (default 24 h, at most 7 days, newest 20k events)
derives stage wall time, admission-to-release and release-to-Done waits, gate
rejection rates, attempts, parks, lease waits and effect outcomes from event
commit times, and adds tasks by state, withheld counts, lease utilisation, the
heavy-op queue, provider health, the latest host sample and the process census.
Figures not yet recorded are listed under `unavailable`, never estimated.
CLI: `fae alerts [list|ack]`, `fae metrics [--window D]`, `fae host-metrics`.

The supervision log (`HOME/INSTANCE/supervision.log`, mode 0600, symlinks refused)
writes each line as `TIME instance=NAME component=NAME text` with an RFC 3339 UTC
timestamp and a component (`daemon`, `dolt`, `admission`, `alerts`, `leaks`,
`provider`, `observe`, ...). It rotates at 64 MiB keeping four older files (`.1`
newest) and copies each line to standard error through a 4096-line queue that
drops lines rather than block; the drop count is logged at close.

Each 5-second observation round samples the host, censuses the daemon's process
tree (FAE-HOST-022: more than 200 processes or 4 GiB per command name, or more than 20 defunct processes, raises one `process-leak` alert per
episode; foreign commands beyond a bound are logged, never acted on). A
defunct process inside a running brokered command's tree (a descendant of a
live record of the effects registry, or a member of its process group) is that
command's to collect, so for the first two censuses that see it defunct
(`processowner.CommandDefunctGrace`) it is reported as `defunct_in_commands`
and not held against the defunct bound, though it still counts towards its
command name's process bound; from the third census it counts like any other,
whether or not the command still runs. So defunct processes a running test
suite collects a moment after they exit raise no alert, while a command that
lets them accumulate is alerted after about two rounds; defunct processes
outside every running command count at once, including those a command left
when it exited, which wait for the reaper (adopted ones at once, others after
its 5 s grace). A `defunct` leak's alert detail and the process report carry
`defunct_parents`: the processes that have not collected their defunct
children (`command`, `pid`, `defunct`, `graced`, `rounds` seen defunct,
`running` inside a brokered command), most defunct first, so the alert says
which command held them. The round also raises `dead-edge` and
`route-exhaustion` alerts once per episode. A route-exhaustion
episode is the engine's: it starts at a task's first park for want of an
eligible route, survives daemon restarts until a route runs, and restarts on
Resume or restart with information; its alert is raised once the configured
route-exhaustion bound (30 minutes by default) has passed, whether the task is
still parked or the engine has escalated it `ProviderUnavailable`. While the drive is idle, a round skips its plane steps
unless something was written since the last one, and the census unless the
daemon's own descendant set changed, but for an own-tree census every minute;
plane steps and the full census still run at least every 10 minutes.
Host admission raises one `host-admission` alert per
hard-pressure episode (FAE-HOST-003), and a task resting in a commit or integrate
stage with no worker for 10 minutes raises one `stuck-commit-stage` alert per
resting episode (FAE-VCS-009); both use the same alert path.

The pool applies to every read method except the cached Status, Board and
Topology (apart from Board and Status rows of tasks finished before the start
while their summary is not built, above), including
`Inbox`, `AnalyzeSession`, `Show`, `TaskGet`, `Workflows`, `WorkflowList`, `WorkflowDiff`,
`WorkflowSimulate`, `LandStatus`, `Census`, `ReviewTargetHealth` and
`ProphetSession`. Schedule list/inspect and controller health
consume immutable cached copies without acquiring the writer mutex. Physical event
compaction is not implemented. Controller health reports its snapshot age and startup binary identity; mutating
controller preflight still re-hashes the executable before making decisions.

Relational record reads (schema 8): `History {id, records: true, after, limit}`
pages the task's transition log by transition id with the artifacts each
completion recorded (content, role, binding, session id, transcript reference,
input ids, molecule) and, on the first page, the task's decisions. It is
derived from the plane's records alone and needs no live session
(FAE-WF-052). Summary history takes its artifact references from the
provenance rows. `Topology {workflow_id, after, limit, include_retired}` pages
a cached snapshot of every running molecule with its stage graph, normal-case
role order and the non-terminal tasks at each stage; with `include_retired`
(FAE-OPS-011, the opt-in Status, Board, History and Events have) a separate
on-demand snapshot slot lists a running molecule's Retired tasks too, at a
`Retired` stage, so the default refresh never reads or counts Retired task
rows against its 10,000-task bound. A molecule
whose stage graph does not resolve, because its revision is one this binary
cannot run (`workflow.Parse`; FAE-WF-093, the tasks pinned to it are
quarantined at start), its content no longer reproduces the molecule or the
revision could not be read, is rendered with an `error` naming why, an empty
`role_order` and its tasks at stages named by their state alone; a graph error
never fails the page or hides the other molecules. One keeper, outside the read
pool, builds each requested snapshot slot on demand as the status keeper does:
a read that finds its snapshot older than 5 s wakes the keeper, and a read
that finds none, or one older than 10 s, waits for the refresh within its
deadline (else `deadline-exceeded`, or the refresh's error). Concurrent reads
share one refresh, and an idle daemon nobody reads does no topology work. A
refresh pages the scoped read (two keyed statements over the molecule and task
tables per page of up to 100 molecules), re-reading a page past its
10,000-task bound narrower; a single molecule past that view's 10,000-task
bound answers that bound error on every page of that view that includes it. The reply carries `snapshot_at` and `snapshot_age_ms`, and
the `statements`, `rows` and `elapsed_ms` of the refresh that built the
snapshot. The graph is cached per molecule id, whose content is immutable.
`ProvenanceChain {commit, limit}` walks from the commit or landing artifacts
naming a sha back through each production's inputs to the spec and intake.
`BacklogShow` reads task states and reasons from the task rows and deferrals
from the backlog mirror; it loads no job snapshot. `TaskGet {id, include_retired}`
reads one task by key: its row and snapshot, its backlog row, its transitions,
its newest 1,000 edit events (`edits_truncated` when older ones are left out)
and the edges into and out of it with the other ends' task rows ([live task
verbs](TASKS.md)); its `superseded_fields` carries the structured fields a turn
in flight still runs under after a mid-turn edit, its `pending_delete` is `null`
or `{"reason", "superseded_by", "requested_at", "status": "waiting-for-turn" |
"uncertain" | "retiring" | "failed", ...}`, and each Board item carries
`"pending_delete": true` while a delete is pending (TASKS.md). Status
`snapshot.scheduler.molecules`
reports running molecules by workflow against `serve --max-molecules` and
`--max-workflow-molecules` (−1 unbounded); a Pending task that would start a
molecule beyond either limit, or beyond its revision's declared
`molecule_concurrency.max_running`, is withheld as `molecule_limit`.

Withheld reasons (FAE-SCH-010). Status `snapshot.scheduler` reports the latest
drive round's withheld tasks: `withheld` (counts by reason), `withheld_tasks`
(each task's reason) and `withheld_round`, with `withheld_totals`, the task-rounds
withheld by reason since the start. `Metrics` reports the same counts as
`withheld_by_reason`, `withheld_totals` and `withheld_round`, and each round that
withholds a task, and the first round after one did, writes a `drive withheld`
supervision line. A task is withheld for one reason; terminal and escalated tasks,
and tasks waiting for an operator decision, are not counted. The reasons
(`daemon/withheld.go`; `TestReadsDocumentsEveryWithheldReason` checks this list
against the code):

- `dispatch_paused`: dispatch is paused by operator policy.
- `provider_pool`: its turn waits for the provider session pool.
- `provider_quota`: all eligible routes are held until provider quota resets.
- `tasks_in_flight`: the tasks-in-flight limit (`serve --max-tasks-in-flight`,
  `runtime-config --tasks-in-flight`) was reached before the round decided the task.
- `cap`: its turn waits for a role slot at the role's cap.
- `host_guard`: its turn waits in, or was deferred by, host admission (pressure,
  the memory reservation, the heavy-operation lease or its I/O gate), or it is
  parked `infrastructure-deferred` with no more specific cause recorded.
- `pane_limit`: host admission deferred its turn at the pane limit.
- `resident_unretired`: its resident turn deferred because the slot's transport is
  stopped or removed but not yet proven retired; the next attempt runs on another
  slot.
- `edge`: a Pending task held by an unsatisfied blocking edge.
- `edge_unread`: a Pending task whose dependency rows could not be read.
- `release_changed`: a Pending task that changed while its release ran without the
  writer lock; the next round decides it afresh.
- `molecule_limit`: a Pending task that would start a molecule beyond a molecule
  limit (above).
- `deferral`: deferred by the operator (`fae defer`), or waiting between polls of
  an external review (`ReviewPending`).
- `route_unhealthy`: parked `all-routes-unavailable`: no route of its role is
  eligible, and not every one was refused by the bridge.
- `route_refused`: parked because the bridge refused every route of its role in
  this route generation; the task's reason names the refusals.
- `parked`: parked for any other reason, such as a missing input or a loop
  back-off; the task's reason says which.
- `plane_unconfirmed`: the round ended before deciding any task because the plane
  has not re-confirmed the checkpoints since a restart.
- `checkpoint_unsettled`: a checkpoint was not yet settled, at the round's start or
  left unsettled earlier in the round; the tasks the round had not decided wait.
- `draining`: the instance drains (`fae drain`), from the round's start or from
  the point the drain began.
- `round_read`: one of the round's own reads failed.

Other status fields: top-level `ui` (`{"enabled": false}` without `serve --ui`;
otherwise `enabled`, `health`, `url`, `pid`, `restarts`, `writes_enabled`, `binary`,
`last_exit`, `checked_at`, never the token) and `default_workflow` (`state`,
`revision`, `hash`, `generation`, `upgraded_from`, `reason` from the start-time
reconciliation of the stored default with the shipped one), `startup_checks`
(what this start's checks found: `plane`, the control plane's consistency
with `event_clock`, `last_event`, `diverged_tasks`, `consistent` and the
`quarantined` tasks; `revisions`, the stored workflow revisions checked
(`checked`, the revision this start installed included; `unloadable` is
always empty on a running daemon, since an unreadable revision refuses the
start; `invalid`, the ones this binary reads but cannot run (`workflow.Parse`
refuses them), each with `workflow_id`, `revision`, `hash`, `error`, the
active pinned `tasks` and `admits`, and `quarantined`, the tasks this start
quarantined for such a revision under `serve --quarantine-unrunnable-revisions`;
a running daemon drives no task on one, since an active task pinned to it
refuses the start otherwise); and `scratch_quarantine`, what
`scratch-quarantine` held after the start's sweeps: `path`, `entries`,
`bytes`, the first 20 `names` and `incomplete`; see [scratch](SCRATCH.md)), and
`snapshot.drive.step_faults` (housekeeping step to its last error, present while a
step is faulting). `UIToken` returns the UI token, URL, one-time login URL and
`writes_enabled` to the instance operator only (`fae ui token`).
