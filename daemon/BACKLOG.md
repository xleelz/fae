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

Upsert can change priority, source reference and write class. Acceptance content
and title can change only before the first dispatch; later updates must use the
append-only context controls. Workflow binding, target, run identity, tree and
disposition cannot change. Terminal identities must be re-admitted with a new ID.
All item writes are atomic; a validation failure returns one error and admits zero
items. Success reports admitted, updated and refused counts.

`fae backlog edge add|remove --from A --to B --kind blocking|relates
--authorizing-message MESSAGE` edits one edge. Adding to a previously dispatched
dependent requires the operator's explicit `--accept-in-flight`. This accepts a
future Pending release condition; it does not interrupt an active turn. Controllers
cannot supply that operator override. Missing-edge removal and duplicate addition
are refused. `fae backlog show [--state STATE]` reports items, edges, ready IDs,
withheld prerequisites/outcomes, dead edges/outcomes and deferrals.

Every job checkpoint mirrors its lifecycle state in its backlog row in the same
transaction. `fae reconcile-done --authorizing-message MESSAGE [--dry-run]` repairs
stale mirrors of Done tasks using this task's own live integration artifact and
provenance, with a 40- or 64-hex landing SHA. It does not infer landing from branch
ancestry, foreign artifacts or scripted results. Consistent rows produce no
mutation/audit. Missing landing proof and genuinely non-Done tasks are reported
and left alone. The same audited pass runs once during startup; it does not run
as a periodic background repair.

Bounds: payloads <= 64 KiB, 1..1000 items and <= 10000 supplied edges; graph reads
fail closed above 100000 rows in either table. Versioned Local target configuration selectors are supported and pinned; see
[target policies](TARGETS.md). Unsupported structured test-command admission fields
are rejected rather than silently accepted. Ordinary `run` admissions can be dependency
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
