# Live task verbs: create, update, delete and read one task

`fae backlog admit --file graph.json` admits a whole graph ([backlog](BACKLOG.md)).
These four verbs create, edit, delete and read one task, and its edges, while the
daemon runs it. Each write takes `--authorizing-message` (RPC
`authorizing_message`) like every other control, is audited, has an RPC with the
same validation as its CLI verb (FAE-API-010), is refused under a maintenance
marker or once shutdown begins (FAE-API-011), and replays its original result
under an idempotence key (`--idempotence-key`, RPC request `key`; FAE-API-012).
Nothing here assumes a language: the structured fields are optional and the
messages name no toolchain.

| CLI | RPC | Permission |
| --- | --- | --- |
| `fae task create` | TaskCreate | task class (`authorizing_message`) |
| `fae task update` | TaskUpdate | task class |
| `fae task delete` | TaskDelete | task class |
| `fae task show` | TaskGet | read |

Edges keep their own verb: `fae backlog edge add|remove` (BacklogEdge,
FAE-OPS-014). An edge added to or removed from a running task (with
`--accept-in-flight`) is in its TaskGet edges and edits at once, and in
`fae backlog show`.

## TaskCreate

```
fae task create --id ID [--title TEXT] --content TEXT|--content-file FILE [--priority N]
  [--source-ref REF] [--write-class CLASS] [--declared-file PATH]... [--test-filter REGEX]
  [--test-shape SHAPE] [--workflow ID [--revision N]] [--target-config ID [--target-revision N]]
  [--task-class CLASS] [--mode local | --mode normal --dry-run|--commit-external]
  [--depends-on ID]... [--blocks ID]... [--relates-to ID]... [--accept-in-flight]
  --authorizing-message ID
fae task create --file PARAMS.json --authorizing-message ID
```

Parameters: every field of a `backlog admit` item (`id`, `title`, `content`,
`priority`, `source_ref`, `write_class`, `declared_files`, `test_filter`,
`test_shape`, `workflow_id`, `revision`, `target_config`, `target_revision`, the
disposition fields, `task_class`, `carry_from`), plus `edges`
(`[{"from","to","kind"}]`, kind `blocking` or `relates`) and `accept_in_flight`.
`--depends-on P` is the edge `P -> ID` (blocking), `--blocks D` is `ID -> D`
(blocking), `--relates-to R` is `ID -> R` (relates).

The task goes through backlog admission's validation and transaction: nonblank
content, priority at least 10 (an omitted priority takes 10, as `run` does; a
priority given below 10, `0` included, is refused by the CLI before the daemon
is dialled, exit 2, and by the RPC, FAE-CLI-002: the RPC's `priority` is absent
or an integer, so `0` is a value, not a default), the structured fields
(BACKLOG.md; `test_shape` is validated, stored, shown and consumed during
`HostCommand` execution and landing test receipt resolution), a resolvable, unretired workflow,
edges only to
tasks on the plane (refused `invalid edge FROM -> TO (KIND)`), no self-edge, no
cycle (refused `blocking cycle: A -> B -> A`). Every edge must name the new task;
edges between existing tasks belong to `backlog edge add`. A new edge that holds
an already dispatched dependent's next release is refused unless the operator
passes `accept_in_flight`, as `backlog edge add --accept-in-flight` requires.
An existing identity, in any state, is refused: a create never updates. The
task, its backlog row and its edges commit in one transaction (edges first), and
the drive releases it like any admitted item. Each existing task an edge joins
gets a `backlog_edge_changed` event naming the edge.

Result: `{"id", "state", "version", "edges_added": [{"from","to","kind"}],
"readiness": {"state", "ready", "withheld_by": ["PREREQUISITE: STATE"]}}`.

## TaskUpdate

```
fae task update --id ID [--title TEXT] [--content TEXT|--content-file FILE] [--priority N]
  [--source-ref REF] [--write-class CLASS] [--declared-file PATH]... [--clear-declared-files]
  [--test-filter REGEX] [--test-shape SHAPE] [--expected-version N] --authorizing-message ID
fae task update --file PARAMS.json --authorizing-message ID
```

Parameters: `{"id", "title", "content", "priority", "source_ref", "write_class",
"declared_files", "test_filter", "test_shape", "expected_version",
"authorizing_message"}`; only the fields present change (`null` or absent leaves
a field; `"declared_files": []` clears them). The pinned fields (`workflow_id`,
`revision`, `target_config`, `target_revision`, `target_hash`, `mode`,
`commit_external`, `ticket_room`, `destination`, `push_namespace`, `run_id`) may
be named only with their current values; a change is refused (`upsert cannot
change pinned workflow, execution target or disposition`), as a backlog upsert
refuses it. `expected_version` refuses an edit of a task that moved on since it
was read (TaskGet's `version`).

It works on every non-terminal task: Pending (deferred or not), parked, in
flight, Blocked or Escalated. It goes through backlog admission's keyed upsert
(FAE-ROB-040): every field is accepted at any time, and a turn already in flight
is never changed under (operator decision, 2026-10-30: an edit of a running task
applies from its next stage). When each field takes effect:

| Field | Takes effect |
| --- | --- |
| `title`, `source_ref`, `write_class` | At once (the backlog row and reads); no turn reads them. |
| `priority` | The next scheduling decision, as Reprioritize does. |
| `content` | The next dispatch's brief; the turn in flight keeps the acceptance text it was dispatched with (`superseded_acceptance` in the row, until that dispatch's sequence). |
| `declared_files`, `test_filter`, `test_shape` | The next dispatch; the turn in flight keeps the values it was dispatched under (`superseded_fields` in the row, until that dispatch's sequence): its live scope, `task_scope` brief block and change check all read those. The declared files feed the next turn's derived scope and the landing; the test filter and test shape feed `HostCommand` test-shape resolution and landing test receipts. |

"The next dispatch" is the next stage, or a redispatch of the same stage (a
stall retry, an uncertainty resolution's redispatch). TaskGet shows the new
values as soon as the edit commits and, while the turn in flight is still out,
the values it runs under in `superseded_fields`; the `task-edited` event carries
`"effect": "from-next-stage"` and the dispatch kept on the old values. A landing
reads the task's current declared files and test filter when it runs. A
terminal task (Done, Cancelled, Retired) is refused: admit the work under a new
identity. An update gives the task a backlog row if it had none (a `run`
admission), as a backlog upsert does.

Each update that changes something writes the task's new version, its backlog
row and a `task-edited` event in one transaction, with the audit row. The event
names the changed fields: `{"fields": [...], "via": "TaskUpdate", "effect",
"actor", "message_id"}`, plus `acceptance_sha256` for a content change,
`priority` for a priority change and `in_flight` (the dispatch that keeps its
text). TaskHistory shows it and TaskGet lists it. A `backlog admit` upsert that
changes an item writes the same event with `"via": "BacklogAdmit"`, and also
replaces the structured fields its item names. An update that names only
current values writes nothing.

Result: `{"id", "changed": [field names, in the order title, content, priority,
source_ref, write_class, declared_files, test_filter, test_shape], "effect":
"applied" | "from-next-stage", "state", "version"}`. `from-next-stage` means a
turn was in flight (it keeps the content and structured fields it was
dispatched under); `changed` is empty when nothing changed.

## TaskDelete

```
fae task delete --id ID --reason TEXT [--superseded-by ID] [--expected-version N] --authorizing-message ID
```

Parameters: `{"id", "reason", "superseded_by", "expected_version",
"authorizing_message"}`. Delete means retire and keep history (operator
decision, 2026-10-30): nothing is removed from the plane. The task's history,
events, artifacts, decisions and audit rows stay; reads hide it as they hide
every retired task (`--include-retired` shows it).

Delete composes the kernel's own edges, so no new transition exists
(FAE-WF-021: Escalated leaves only by Retire or a restart; `CanTransition` is
unchanged):

| The task is | Delete |
| --- | --- |
| Pending, deferred or not | Escalate then Retire, now. |
| Parked at a stage, Blocked, or waiting for a decision | Escalate then Retire, now. |
| Running a turn | Escalate, which holds the turn; the drive collects the turn's result (never abandons it) and then retires the task; `pending: true` until then. |
| Escalated, nothing in flight | Retire, now. |
| Escalated holding a turn | As running: retired once the turn is collected. |
| Done, Cancelled or Retired | Refused: a finished task cannot be deleted. |

The `action` is `withdrawn` for a task that never dispatched a turn (a Pending
task, or one escalated or parked before its first dispatch) and `retired` for
any other; both end in state Retired. Withdrawal is a retirement rather than
`cancel` so that a deleted task is hidden like every retired one and a named
successor can take over its dependents.

The retirement reason is `obsolete REASON`, or `superseded-by SUCCESSOR` with
`superseded_by` (RETIREMENT.md); with a successor the reason is kept in the
escalation's reason (`TaskDelete: REASON`) and the audit's inputs, not in the
retirement. Obsolete keeps the task's blocking dependents held (they report a
dead prerequisite); supersession re-points them to the successor in the
retirement's transaction, recording a `backlog_edge_changed` event for each
moved edge on the dependent and on the successor (`{"from": SUCCESSOR, "to":
DEPENDENT, "kind": "blocking", "removed": false, "repointed_from": ID}`), which
their TaskGet edits list. The successor must be another existing task; one that
is itself a blocking dependent of the deleted task, or a re-pointed edge that
would close a cycle (named), is refused before anything is written. Relates
edges keep their meaning. The retirement checkpoint carries a `task-deleted`
event (`{"action", "retirement", "actor", "message_id"}`).

A delete that waits for a turn stores a pending delete in the task's row
(`pending_delete`: retirement, action, requested_at, actor, message_id, the
dispatch it waits for), in the same checkpoint as the escalation. It survives a
daemon restart. While it is pending, a second delete, `restart`, `resume` and
`block` of the task are refused. `retire` is refused too while the turn is in
flight (the kernel refuses Retire with a turn in flight, as for any Escalated
task); once nothing is in flight it ends the delete, which is how a delete whose
own retirement failed is finished by hand. Once nothing is in flight (the drive
collected the held turn, or, after a restart, the operator resolved the
uncertain held turn with `resolve-uncertain --action escalate`), the next drive
round retires the task, audited as `TaskDelete` by `daemon` with the original
message; the retirement dispatches nothing and frees capacity, so it is not
held behind the tasks-in-flight cap (an unsettled checkpoint still settles
first). A plane transient waits for the next round. A retirement the retirement
checks refuse, or a third one the plane refuses (`task-delete-refused` events
count them; each event's row commits before its log line), ends the delete with
a `task-delete-failed` event (`{"retirement", "cause", "message_id", "refused",
"requested_at"}`) and alert: the task stays Escalated for the operator to retire
or delete again. While the instance drains, pending deletes wait.

Readers see the delete without parsing reason text: each Board item carries
`"pending_delete": true` while a delete is pending (absent otherwise), and
TaskGet's `pending_delete` describes it (below).

Result: `{"id", "action": "withdrawn" | "retired", "collected_in_flight": bool,
"held_dependents": [ids], "repointed_dependents": [ids], "reason_class":
"obsolete" | "superseded-by", "pending": bool, "state", "version"}`, plus
`"dispatch"` when pending. `collected_in_flight` is true when the task had a turn
in flight: the delete waits for it, and `pending` is true until the drive retires
the task. A delete with nothing in flight answers `"state": "Retired"`.

## TaskGet

```
fae task show --id ID [--include-retired]
```

Parameters: `{"id", "include_retired"}`. A retired task is hidden unless
`include_retired`, as History and Show hide it. It is a read in the read pool
(READS.md) and reads only this task by key (FAE-CP-009): its row and snapshot,
its backlog row, its transitions (pages of 1,000, at most 20,000), its edit
events (at most 1,000) and the edges into and out of it with the other ends'
task rows. No whole table and no other task's snapshot.

Result (`daemon.TaskView`):

- Admission: `id`, `version`, `title`, `content` (the description and
  acceptance text), `content_bytes`, `content_sha256`, `priority`, `source_ref`,
  `write_class`, `declared_files`, `test_filter`, `test_shape`, `task_class`,
  `superseded_fields` (present while a turn in flight runs under structured
  fields that a mid-turn edit has since replaced: `{"declared_files",
  "test_filter", "test_shape", "dispatch", "until"}`), `workflow_id`,
  `workflow_revision`, `target_id`, `target_config`, `target_revision`,
  `target_hash`, `mode`, `run_id`, `admitted_at`.
- Where it is: `state`, `stage` (the workflow stage it is at, running, blocked
  from or escalated from; empty before its first stage), `molecule_id`,
  `in_flight`, `dispatch`, `parked`, `reason`, `deferred`, `deferred_until`,
  `attempts`, `attempt_limit`; when Escalated `escalation_kind`,
  `escalation_reason`, `escalation_detail`, `escalated_at`.
- `pending_delete`: `null` when the task has no delete; otherwise `{"reason",
  "superseded_by", "requested_at", "status"}` plus the stored delete's
  `retirement`, `action`, `dispatch`, `actor`, `message_id`, `refused`,
  `uncertain_dispatch` (when `status` is `uncertain`) and, when failed, `cause`.
  `reason` is the operator's reason of an obsolete delete (empty with a
  successor), `superseded_by` the successor (empty otherwise). `status` is
  `waiting-for-turn` (the task's turn in flight is being collected), `uncertain`
  (the turn in flight was interrupted by a daemon restart, is parked
  `UncertainDispatch` and awaits `resolve-uncertain --action escalate`),
  `retiring` (nothing is in flight; the next drive round retires it) or `failed`
  (the delete ended with `task-delete-failed` since the task was last escalated
  and the task stays Escalated for the operator). A retired task has `null`.
- `edges`: `{"in": [{"from","to","kind","state","satisfied"}], "out":
  [{"from","to","kind","state"}]}`, `state` being the other end's state
  (`missing` when the plane has none); `readiness`: `{"state", "ready",
  "withheld_by"}` (plus `deferred`).
- `trace`: the ordered stage visits, `[{"stage", "entered_at", "left_at",
  "outcome", "to", "attempt", "edits"}]`, from the task's transitions: the first
  visit (Pending) starts at admission, each transition ends one visit
  (`outcome` is its recorded cause, `to` where it went) and starts the next; the
  current visit has `left_at: null`. Each visit lists the edits made while the
  task was there.
- `edits`: the newest 1,000 edits in order, `[{"sequence", "at", "kind",
  "detail"}]`: `task-edited`, `backlog_edge_changed` (`{"from","to","kind",
  "removed"}`, plus `repointed_from` for a supersession's moved edge),
  `task-deleted`, `task-delete-refused`, `task-delete-failed`; `detail` is the
  event's payload. `edits_truncated: true` when older edits were left out.

A view past the read limit drops the oldest visits first (`trace_omitted`
counts them), then the content (`content_omitted`, with `content_bytes` and
`content_sha256`); `trace_truncated` marks a task with more than 20,000
transitions.

## Tests

`daemon/tasks_test.go` (Dolt; scripted adapter): a create joined to a running
graph and dispatched once its prerequisite is Done, with the refusals (priority
5 and 0 among them; an absent priority takes 10) and the in-flight acceptance;
an edge added to and removed from a running task in TaskGet and BacklogShow; an
update of a task in flight reaching the next stage's brief but not the turn in
flight, the structured fields stored for the next dispatch (`dispatchAdmission`,
`dispatchLiveScope`, `resolve` and `checkChange` of the turn in flight keep the
old ones) and shown by TaskGet at once alongside `superseded_fields`, with the
`task-edited` events in History and TaskGet, the malformed, pinned,
stale-version and terminal refusals; deletes of a Pending task (withdrawn), an
Escalated one, with supersession re-pointing (the moved edge among both tasks'
edits) and the refused successor; a delete of a running task (held turn
collected, then retired; held dependent named; TaskGet `waiting-for-turn` and
the Board flag, cleared once retired); a pending delete across a restart
(`uncertain` while the interrupted turn awaits `resolve-uncertain`, `retiring`
once resolved before the retirement); a collected delete retiring under a full
tasks-in-flight cap even when ordered after terminal and ready tasks; a delete
that fails once collected (`failed` with its cause, the Board flag cleared, a
second delete retiring the task) and TaskGet keeping the newest edits with
`edits_truncated`; idempotent replay.
`TestMutationsRefusedTypedUnderMaintenanceMarker` covers the maintenance
refusal of every mutating RPC, these included. `TestEditTaskRules` (the
superseded fields and `dispatchAdmission`), `TestTaskTraceAndCurrentStage` and
`TestLogAfterCommitWritesNoLineForAFailedCommit` need no plane. `cmd/fae`
conformance covers the four verbs' exit codes, help and their one RPC each;
`TestTaskVerbsSendTheFieldsTheirFlagsName` covers the priority refusals (flag
exit 2, `--file` exit 1), mode/revision validation before daemon contact and
that an omitted priority is not sent.
