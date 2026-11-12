# Control-plane implementation and verification

Verified locally on 2026-10-16 using Go 1.27.2 (darwin/arm64), Dolt 2.5.0,
MySQL driver v1.9.3, and sqlmock v1.5.2.

Reproduce isolated integration (the test starts and stops its own Dolt child and
creates/removes its own data and configuration directories):

```sh
FAE_TEST_DOLT_BINARY=/absolute/path/to/dolt go test -race -cover ./controlplane -v
```

The initial storage-only run passed with **69.9% statement coverage**; current
whole-suite results are recorded in [TESTING.md](../TESTING.md). Without the explicit binary,
the real integration test skips; all deterministic unit tests still execute.
No test accepts a DSN or writes an existing instance's control plane. A sandbox
must permit the temporary loopback listener and local MySQL connection.

The subsequent v2 integration adds CAS-versioned job snapshots, original-request
admission idempotence, ordered events in the same audited transaction, operator
message records and schema-upgrade replay. Tests exercise actual Dolt child
kill/restart, rollback after partial snapshot mutation, and interrupted v1-to-v2
migration. The daemon now provisions a private database user, rejects passwordless
root access and performs bounded recovery of its owned child. (Superseded: with an
`OwnerRegistry`, parent SIGKILL cleanup uses `processowner` records and a pipe
watchdog on Darwin and Linux.)

Initial storage baseline verified behavior:

- Real Dolt migration, idempotent migration replay, SQL schema acceptance.
- Real SQL transaction rollback of an injected partial mutation, including no
  audit row on failure; committed mutation and audit share a transaction.
- Dolt commit log contains a version commit naming actor and logical batch.
- Greater-than-1-MiB remediation payload round-trips via bounded SQL blob chunks;
  chunk order, size, count and SHA-256 integrity are checked on reads.
- Workflow revision creation, immutable molecule binding, task admission,
  artifact recording, state transition, and stale-state refusal on real Dolt.
- Mock fault injection at each transition/audit write; rollback expected at
  every point. Serialization failures during body and commit re-run the whole
  callback; non-serialization errors propagate; exhaustion returns RetryableError.
- Newer-schema refusal, first-attempt/no-extra-retry behavior, duplicate JSON
  key refusal, precise canonical decimal normalization, invalid disposition and
  non-hex evidence rejection, terminal-state and missing-evidence refusal.
- Canonical JSON idempotence fuzz seed corpus and a short generated fuzz run.

Initial storage-helper boundaries (daemon integration above supersedes its
credentials and health-restart gaps; these are not completed requirement claims):

- This is a storage API and child-process helper, not the daemon or drive.
  The required transition validator is trusted daemon code: it must check the
  pinned workflow, complete input bindings, provenance and operator authority.
  The store independently checks durable current state and a recorded task-local
  evidence digest. The callback must perform SQL only because retries repeat it.
- Set Options.VersionCommits=true for versioned logical writes. Low-level New
  permits disabling this for SQL mocks. Most reads use keyed or indexed
  predicates and page their results. Three paths still read a large family
  whole (FAE-CP-009):
  - `Backlog`: every backlog item and edge, failing closed past 100000 of
    either (BacklogAdmit, TaskCreate, BacklogShow and Land);
  - `TaskHeads`: every task row (BacklogShow);
  - a supersession Retire's edge transfer (`transferRetiredEdges`): every
    backlog edge, inside the retirement's transaction.

  TaskGet and TaskUpdate read one task by key (`BacklogItem`,
  `TaskDependencies`, `TaskDependents` on the edge key's prefix,
  `TaskEventsOfKinds` and `TaskTransitions` on their indexes).
  An edge edit (BacklogEdge) reads only its dependent's edges, by key
  (`DependentEdges`), and for a blocking add checks for a cycle with
  `BlockingPath`: a walk from the edge's two ends, one keyed statement per
  level and the cheaper side first. `TicketStats` (startup and every live
  drive round) is one aggregate over the `ticket_outbox` projection's pending
  rows and reads no emission payload; migration 9 alone reads every emission,
  once, to build that projection. Small configuration tables (resource
  pools, provider health, target configurations) are read whole. `ListJobs`
  reads every job snapshot and refuses past its limit; only tests call it
  (the global Prophet pages `TerminalRowsAfter` instead).
  Tests count what the scoped reads take: TestTopologyScopedOverLargePlane
  (statements and rows per page), TestEdgeEditReadsScopedOverLargePlane
  (statements and rows per walk) and TestTicketStatsReadsNoEmissionPayload.
  The 100k transition topology budget runs as a test (see schema 8 and 9
  below).
- SQL callbacks and the database owner can issue arbitrary SQL. Triggers make
  provenance, artifacts, transitions and decisions append-only (schema 8);
  other tables rely on the API. Every record family has a production writer
  (schema 9).
- Dolt/MySQL DDL may implicitly commit. Migration statements are idempotent for
  interrupted replay, but all-or-nothing migration atomicity is not guaranteed.
- Without `OwnerRegistry`, Linux installs Pdeathsig and macOS relies on explicit
  Stop. The daemon always passes `OwnerRegistry`: its Dolt child gets an exact
  `processowner` record and pipe watchdog (parent-death cleanup on Darwin and
  Linux), startup recovers a proved owned orphan, and child death gets three
  bounded restart attempts; it also provisions the private database user.
  Outage detection, recording and re-validation are in schema 9 below.
  StartServer probes the port by connecting (FAE-ARC-006): only a refused
  connection proves it free; an answering endpoint is refused naming its
  listener (an unrecorded Dolt on this data directory, or another process) and
  the remedy, and no existing server is adopted or signalled.
- Complete provenance schema validation remains open; molecules end with their
  last task (schema 8).
- Gobra/Gomela proofs live in the repository verification area and concern the
  modeled safety core; the SQL driver and Dolt engine are not formally verified.

## Durable leases (schema 3)

Resource pools persist ordered waiting requests and weighted holders in one row
per scope. Every mutation shares a serializable SQL transaction with its audit,
version increment and typed events. Eight concurrent independent Store clients
against a private real Dolt server cannot exceed capacity two. Tests also cover
FIFO, owner-start mismatch, dead-owner reclamation, queued cancellation, CAS
rollback, corrupted ledgers, forward migration and held leases surviving a
SIGKILL/restart of the owned Dolt child. Configuration changes conflict explicitly;
live cap migration is not implemented. Pools are local to one plane; host-wide
heavy operations use the separate shared file ledger in `scheduler/ledger.go`.

## Task records, molecules and provenance (schema 8)

Every job write (`AdmitJob`, `AdmitBacklog`, `SaveJob`, `MigrateJob`) binds or
reads the task's molecule and projects the job checkpoint into relational rows
in the same transaction: the `tasks` row (state, attempts, priority,
disposition, escalation, commit and review references), one `transitions` row
per state change with an evidence digest (a stored blob naming the event, the
events since the previous transition and the artifacts a completion recorded),
`decisions` rows (gate results, escalations, operator controls, migrations;
each supersedes the task's previous decision of its kind) and content-addressed
`artifacts` with an append-only `provenance` row per production (stage,
primitive, role, provider and model, session id, transcript reference, input
artifact ids, tree, molecule, time). Cursors on the task row name how much of
the job's append-only history and provenance is recorded, so each entry is
written once. A transition chain that does not lead from the projected state to
the checkpoint's state, a completion that recorded no artifact, or provenance
naming no stored artifact fails closed (`ErrTransitionEvidence`). The job
snapshot remains the engine's working state; the rows serve reads and audit.

A molecule's id is the SHA-256 of its resolved content (workflow id, revision
and content hash), so revisions with identical content are distinct molecules.
Admission refuses a retired or unknown workflow revision and a stored molecule
that re-resolution does not reproduce. A molecule's `ended_at` is set when its
last non-terminal task ends and cleared when a new task joins it.

Triggers make `provenance`, `artifacts`, `transitions` and `decisions`
append-only in the plane itself: an UPDATE, DELETE or REPLACE of an existing
key is refused whoever issues it. Migration 8 creates the provenance table,
the triggers, the job molecule column and the task cursors (columns are added
only when absent, since Dolt has no `ADD COLUMN IF NOT EXISTS`), rekeys
molecules bound under the colliding bare-content identity, and backfills every
existing job in the migration's transaction. Legacy history that has no
transition chain to the job's state stays in its snapshot and the task row's
cursors start after it; a payload that is not a task envelope is left unbound
(the daemon quarantines it at load).

Tests (`records_test.go`, `topology_test.go`, real Dolt): fault injection at
every write of every checkpoint of a scripted default-workflow run leaves every
record unchanged; the commit-to-spec chain query; immutability refusals; ten
admissions across two identical revisions bind two molecules; mismatch,
unknown-revision and retired refusals; an interrupted migration-8 backfill
leaves version 7 and no rows, and its replay completes once. The topology read
pages running molecules in two keyed statements over the molecule and task
tables only. `TestTopologyScopedOverLargePlane` seeds 100k transitions (1,050
molecules, 21k tasks) whenever the Dolt suite runs and fails a page over the
200 ms budget; it logs the slowest page (24 ms on the wave-3 host).

## Record families, scoped reads and outages (schema 9)

Every FAE-CP-011 family table has a production writer, asserted over the
package's SQL by `TestEveryRecordFamilyHasProductionWriter`. Derived families
are projections written in the transaction of their single authority, and no
decision reads them back (FAE-GEN-011):

| Table | Authority | Writer |
|---|---|---|
| `leases` | `resource_pools` lease ledger | every pool mutation: grants open a row, releases close it with the reason |
| `ticket_outbox` | `ticket_emissions` | `EnqueueTicket`, `SaveTicket` (rebuilt keyed by emission) |
| `external_effects` | effect checkpoints, ticket emissions, the ExternalEffect artifact | `RecordEffect`, `SaveTicket`, checkpoints: one row per call key with tool, argument digest, pinned values, returned id, reconciliation, outcome and session |
| `sessions` | job provenance | checkpoints: session, pane, role, task, binding, launch time, daemon owner |
| `code_reviews` | review-lifecycle marker | `PutMarker` with a `CodeReview` projection |
| `schedules` | cron, loop and interval schedule markers | `PutMarker` of those markers |
| `engine_runs` | itself | `RecordEngineRun`: command, tree, sanitized-environment delta (names only), exit, executed count, output digest, times |

Migration 9 adds the projection columns and indexes and backfills leases,
emissions, schedules, effect checkpoints, sessions and reviews in its
transaction. Migrations, provider observations and drive checkpoints are Dolt
commits naming their version, observation, or round and task.

The drive and startup read only what they act on: non-terminal jobs (and
terminal ones with an unfinished ticket lifecycle), the active backlog items
and the edges into active dependents with each prerequisite's state, one
task's edges by key, terminal counts through the state index, retired jobs by
state, and job rows without snapshots for history identity
(`TestActiveBacklogScopedOverLargePlane`: 25 rows of 20,025 in 18 ms).

Oversized audit inputs, events, notes, reasons, rationales and provenance are
stored as blobs with the column holding `{"blob_ref":{digest,size}}`
(`ResolveText`). So is a lease pool whose holders and waiters outgrow the
limit (`encodePool`/`decodePool`); migration 10 spills any such pool and makes
`resource_pools.payload` a TEXT (`TestLeasePoolPayloadSpillsPastConfiguredLimit`,
`TestMigration10SpillsOversizedPoolsAndBoundsColumn`). The limit is 64 KiB,
configurable down to 4 KiB with `fae serve --inline-limit` (FAE-CP-012). `CheckConsistency` re-validates a restarted plane, and every start (event clock,
and the agreement of each active job's state with its task row and, when it
has one, its backlog row) by key and index, a page of 1,000 diverged tasks at
a time; `RepairTaskRows` sets those two states to the job's for
`quarantine release` (`TestCheckConsistencyFindsEveryDivergedTask`,
`TestQuarantineReleaseRepairsRowsTheStartFoundDiverged`).

## Finished-task Board columns (schema 11)

The daemon does not materialise finished tasks at startup, so Board and
status task rows read them from the plane. `TerminalRowsAfter` pages them in
one statement with every Board filter in it (state, target, workflow,
molecule, run, case-insensitive mode, minimum priority): per state, a range
over the jobs state index joined to the task row by key, the ranges merged by
id. Without a task-column filter each range is limited before the join, so a
page costs its own rows: paging 100k finished tasks 1,000 at a time took
0.74 s on the development host, against 21.7 s with one range over all three
states, which Dolt sorts whole for every page.
Each row is built from the job and task columns with no snapshot read; only a
reason stored by reference costs a blob read. Migration 11 adds the task row's `execution_kind` and
`mode`, written by every checkpoint from then on, and backfills them, with the
run, priority, reason and escalation kind, from each finished job's snapshot
in id-ordered pages of 1,000 (`backfillBoardColumns`; replay fills only rows
still NULL). Rows whose columns cannot reproduce the snapshot's Board row (a
priority below 10, a value longer than 255 bytes, a snapshot that does not
decode, or no task row) keep NULL; they pass the task-column filters and carry
their snapshot for the reader to decode. Tests:
`TestTerminalRowsAfterPagesByIndex` and
`TestMigration11BackfillsFinishedBoardColumns` (real Dolt).
