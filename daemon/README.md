# Durable daemon

`Start(ctx, Config)` binds a canonical workspace (which must equal the process's
current directory), acquires the singleton lock, initializes and supervises a
private authenticated Dolt server, migrates the plane, loads validated durable
jobs, and starts the Unix API and asynchronous drive. Call `Run(ctx)` to wait and
close, or `Close(ctx)` when embedding. The root is an explicit parent directory
containing named instances; production instance roots must be outside Git
repositories. A new instance has `run`, `plane`, `worktrees`, `backup`, `archive`,
`dolt-config`, workspace binding and supervision log entries.

The scripted executor is selected by default. `Config.Live` / CLI `--adapter direct`
selects a configured trusted bridge with real host commands and guarded Git effects;
see [LIVE.md](../LIVE.md). Its outputs are simulated
rehearsal evidence: reaching Done does not mean a real command ran, a worktree
changed, or a commit was integrated. Arbitrary custom executors are an embedding
interface, not an implemented production agent adapter.

## RPC

Every call requires the mode-0600 credential, as 64 hexadecimal characters in the
Authorization header. POST `/v1/rpc` uses `api.Request` version 1. Available reads
are Status, Board, History/TaskHistory, Workflows and Events. Available writes are
Message, WorkflowInstall, Admit and Shutdown. Every write after Message requires
an `authorizing_message` identifying a previously persisted operator message.
Admit's idempotency key compares the original canonical client request, before
resolving a moving workflow revision or workspace HEAD. Retries return the
original lightweight identity/version/bindings response and never rewind the current checkpoint.

Board reads an immutable cached projection and accepts `after` (task ID) and
`limit` (default 100, maximum 500), returning `items`, `next_cursor` and
`snapshot_at`. History retains its full response for small tasks; larger tasks
return typed `response-too-large` guidance. `History` with `summary:true`, `after`
(event sequence) and `limit` returns events and the artifact/provenance references
for their completed dispatches, without document or artifact bodies. Both paged
reads shrink to a 900 KiB encoded budget. The transport independently bounds all
unary responses below 1 MiB and returns a typed error instead of truncating JSON.
Status includes counts and snapshot age; detailed task rows are on Board.

GET `/v1/events?after=N` streams committed events as NDJSON using the same
credential. Connections are capped at 16, each batch at 100 events, and writes
have a deadline. Commit notifications wake readers, so idle streams do not poll.
The current implementation retains all events; pruning and gap signaling for a
finite retention window are not implemented.

## Execution and restart

One serialized writer persists engine decisions before asynchronous dispatch and
persists collection before subsequent stages. At most `MaxInFlight` executions
run concurrently. Idle drives block on completion/admission/child events.
Status reads a cached projection without acquiring the writer mutex or querying
SQL. Other reads have a pool bound of eight and the transport's deadline.

Graceful stop cancels executors, preserves in-flight tokens and writes a clean
marker before stopping the child. Restart validates payload hashes, task and
workflow bindings, and target workspace. Only the concrete deterministic scripted
executor may replay an interrupted token; other executors are parked as uncertain.
A faulty completion fails only its task's turn. Repeated failures escalate the
task while siblings continue. Child death has three restart attempts; the existing
SQL pool reconnects, schema checks run again, and uncertain checkpoint commits
are reconciled by version plus exact canonical payload before any retry.

## Remaining scope

This is a working durable scripted path, not phase-complete production orchestration.
Missing pieces include sandbox launch detection and an unsandboxed launcher,
macOS parent-death guarantees and orphan reconciliation, multiplexer/provider-specific runtime integration,
changed-base reconciliation, complete process/lease lifecycle
inventory, bounded log rotation, read coalescing, finite event retention, full
operational RPCs and the full process-level SIGKILL matrix.

## Tests

`FAE_TEST_DOLT_BINARY=/absolute/path/to/dolt go test -race ./daemon ./api`
starts only isolated private children. Daemon integration scratch uses
`/private/tmp` on Darwin or `/var/tmp` elsewhere (disk-backed fixture locations),
never a developer instance. Tests cover concurrent full scripted workflows,
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
