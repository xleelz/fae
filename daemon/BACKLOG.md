# Backlog dependency orchestration

`fae backlog admit --file graph.json --authorizing-message MESSAGE` admits a graph
through the authenticated daemon writer. `BacklogAdmit` takes `items`, `edges`,
and `authorizing_message`. Each item contains the normal admission fields plus
`source_ref` and `write_class`; `content` is the acceptance text. Identity,
nonblank content and priority >= 10 are required. Workflow selectors are resolved
before admission; omitted selectors use the instance's built-in default. Existing
items preserve their pinned selector if omitted during upsert.

```json
{
  "items": [
    {"id":"library","content":"Implement the library and tests","priority":20},
    {"id":"consumer","content":"Use the library","priority":10}
  ],
  "edges": [{"from":"library","to":"consumer","kind":"blocking"}]
}
```

Blocking edges point from prerequisite to dependent. Done and a validated
contained retirement release a blocking prerequisite. Supersession transfers
blocking dependents to the named successor atomically; obsolete retirement,
Cancelled, Escalated and missing outcomes hold them. Relates edges never gate
execution. See [retirement](RETIREMENT.md). Dispatch reads the durable graph
before preparing Pending tasks. Admission validates the combined DAG, including
existing tasks; cycles name their path. Self edges, unknown endpoints and kinds,
duplicate item identities, terminal upserts and invalid selectors fail closed.

The batch commits edges before items inside one transaction, together with job
snapshots, content blobs, mirror rows, events and authorization consumption. A
failed insert or audit rolls the whole transaction back. Live preparation removes
newly created owned worktrees on subsequent preparation failure, preserves any
preexisting worktree, and suppresses refusal-side health-marker writes. A SQL
error retains prepared worktrees because a commit acknowledgement may be lost.
The writer reloads the authoritative jobs and checks exact versions and canonical
payloads; unavailable readback blocks task mutation, collection, admission,
dispatch and deployment until resolved. A daemon restart loads the plane again.
Messages and read-only inspection remain available during this barrier.

Upsert can change priority, source reference, write class, title and acceptance
content at any time (FAE-ROB-040): an acceptance edit reaches the next composed
brief, headed with its SHA-256 digest, without a restart, while a turn already in
flight keeps the text it was dispatched with; the item's acceptance digest is
updated in the same write. A structured field the item names replaces the pinned
one from the task's next dispatch: a turn in flight keeps the declared files,
test filter and test shape it was dispatched under (`superseded_fields` in the
row), as it keeps its acceptance text ([TASKS.md](TASKS.md) says when each
field takes effect). An upsert that changes a field records a `task-edited` event
naming the fields (`"via": "BacklogAdmit"`) in the same transaction. Workflow
binding, target, run identity, tree and disposition cannot change. To create,
edit, delete or read one task while the daemon runs, see the
[live task verbs](TASKS.md) (`fae task create|update|delete|show`), which use
this validation and upsert. Terminal identities must be re-admitted with a new ID.
All item writes are atomic; a validation failure returns one error and admits zero
items. Success reports admitted, updated and refused counts.

`fae backlog edge add|remove --from A --to B --kind blocking|relates
--authorizing-message MESSAGE` edits one edge. Adding to a previously dispatched
dependent requires the operator's explicit `--accept-in-flight`. This accepts a
future Pending release condition; it does not interrupt an active turn. Controllers
cannot supply that operator override. Missing-edge removal and duplicate addition
are refused. Each edit records a `backlog_edge_changed` event naming the edge
(`from`, `to`, `kind`, `removed`) on both of its tasks, which their TaskGet edits
list; an admission's new edge to a task already on the plane records one on that
task. The result reports the dependent's readiness before and after
(state, `ready`, `withheld_by`) and the effect; finished prerequisites that
startup did not materialise are read from the plane by key, so a restart does
not change it. An edge edit never reads the whole graph (FAE-CP-009). It
checks its own two tasks, reads its dependent's edges by key, and for a
blocking add looks for the cycle the edge would close: a walk from the edge's
two ends, the dependent's dependents forward and the prerequisite's
prerequisites back, one keyed statement per level and the cheaper side first,
until the two meet (the refusal names the cycle) or either side runs out.
Relates edges are not followed and never close a cycle. Other edges in the
graph are not validated by an edit, so an unrelated edge naming a quarantined
task does not refuse it. Before the writer lock the edit reads whether the
plane stores its two tasks, the dependent's edges and the rows of the dependent
and its prerequisites. Under the lock it reads the dependent's edges again,
re-checks those rows (reading any it missed) and runs the cycle walk; the walk
is not read before the lock. `fae backlog show [--state STATE]` reports items, edges, ready IDs,
withheld prerequisites/outcomes, dead edges/outcomes and deferrals.

Every job checkpoint mirrors its lifecycle state in its backlog row in the same
transaction. `fae reconcile-done --authorizing-message MESSAGE [--dry-run]` repairs
stale mirrors of Done tasks using this task's own live integration artifact and
provenance, with a 40- or 64-hex landing SHA. It does not infer landing from branch
ancestry, foreign artifacts or scripted results. Consistent rows produce no
mutation/audit. Missing landing proof and genuinely non-Done tasks are reported
and left alone. The same audited pass runs once during startup; it does not run
as a periodic background repair.

Bounds: payloads <= 64 KiB, 1..1000 items and <= 10000 supplied edges; the
whole-graph reads of admission, `backlog show` and landing fail closed above
100000 rows in either table, and an edge edit's walk above 100000 tasks or
edge rows. Versioned Local target configuration selectors are supported and pinned; see
[target policies](TARGETS.md). The optional structured fields (FAE-CLI-014) are
validated and pinned with the item: `declared_files` (at most 1000 unique clean
relative paths), `test_filter` (a regular expression of at most 1024 bytes that
compiles both as a whole and at each `-run` `/` and `|` level) and `test_shape` (one of the supported workflow or landing test shapes (`go-unit`, `go-integration`, `script`, `runner`, `run`, `pkg`, `file`, `custom`); consumed during `HostCommand` execution and landing test receipt resolution); a malformed value is
refused. Landing uses the declared files, test filter and test shape in place of the target's
defaults (recording a `landing-test-receipt-<id>` marker and `landing-test-shape/<landing>/<task>` receipt in the control-plane store), and a live instance without pinned `files` takes the task's scope from
the declared files ([LIVE.md](../LIVE.md)); during live `HostCommand` test runs, the task's effective `test_shape` (`Admission.TestShape`, or `SupersededFields.TestShape` while an edited turn is in flight) is resolved and recorded.
The landing's filter gate hands `test_filter` to `go test -run` as written and
counts the tests it names the way `-run` reads it: split on `|` and `/` outside
brackets and parentheses, each level matched unanchored, so `TestA` matches by
substring (`TestAB` too) and `^TestA$` names one test; a filter with no `/`
names top-level tests only (their subtests run and fail with them), and
`TestA/case` names TestA's subtests matching `case`. Every test named must run
and pass, and a filter naming none holds the landing. A task without a filter
lands on its derived-scope RED test, quoted and anchored. Ordinary `run` admissions can be dependency
endpoints but receive a backlog item row only when explicitly upserted through
backlog admission. Cleanup failures can leave an owned unadmitted worktree for
inspection; ambiguous SQL outcomes intentionally preserve worktrees.

Tests cover graph validation, edge-before-item transaction order, insert/audit
rollback, task controls/mirroring, selector pinning, terminal refusal, restart,
readback barriers, drain/deployment refusal and Done reconciliation. Native tests
exercise three workflows with a deferred prerequisite across restart, a relates
edge that proceeds independently, and dependent dispatch only after durable Done.
`CanReleaseDependent` is a Gobra-annotated production guard; the positive/negative
Gomela backlog models cover commit-before-release synchronization. Consult the
phase verification record for which tests were actually executed.
