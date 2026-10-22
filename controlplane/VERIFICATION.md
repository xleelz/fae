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
root access and performs bounded recovery of its owned child. Parent SIGKILL
cleanup on macOS remains unsupported.

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
  permits disabling this for SQL mocks. Reads are keyed/paginated, but the
  specified 100k-row topology latency benchmark has not been implemented.
- SQL callbacks and the database owner can issue arbitrary SQL; there are no
  database privileges/triggers preventing raw updates to append-only tables.
  Workflow/artifact APIs expose no update or delete operation, but that is not
  a database-level immutability proof. Every schema family has initial tables;
  complete business invariants and operations for each family are not implemented.
- Dolt/MySQL DDL may implicitly commit. Migration statements are idempotent for
  interrupted replay, but all-or-nothing migration atomicity is not guaranteed.
- Linux installs Pdeathsig; macOS relies on explicit Stop. SIGKILL parent-death
  cleanup on macOS, owned-orphan discovery/reaping, health-driven bounded restart,
  credentials/privilege provisioning and consistency recovery remain unimplemented.
  StartServer refuses occupied ports but uses a check-then-bind sequence; it does
  not adopt or signal any existing server.
- Backup-before-deletion, target-configuration management, complete provenance
  schema validation, task/revision reconciliation, and automatic molecule ending
  remain for subsequent integration work. No destructive cleanup API is exposed.
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
