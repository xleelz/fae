# Fae: Requirements Specification

- **Module:** `github.com/xleelz/fae` (Go 1.27.2, per `go.mod`)
- **Status:** Draft 1, written for a from-scratch build.
- **Nature of this document:** it states the *intended* system, which is the ideal version of a prior Rust implementation. Behaviour that the prior system got wrong is stated here as the behaviour it should have had, and the reason is recorded. Nothing here describes the prior code. Where a number comes from measured operation of the prior system it is marked as a default, and it is configurable unless stated otherwise.
- **Conventions:**
  - The key words MUST, MUST NOT, SHOULD, SHOULD NOT and MAY follow RFC 2119.
  - Each requirement has an ID of the form `FAE-<AREA>-<NNN>`, a level, a statement, a short rationale and an acceptance criterion. The criterion says how a test or an inspection decides the requirement.
  - "Default N (configurable)" means the value is the out-of-the-box setting, can be changed through instance configuration, and falls back to the default when the configured value is absent, empty, unparseable, non-finite or out of range. Appendix A collects every default.
  - "Audited" means the action writes an immutable audit row to the control plane in the same transaction as its effect. The row names the actor, the authorizing operator message (where one is required), the inputs, a digest of the arguments, the outcome and a timestamp.
  - The document contains no code. Schemas are described in words and tables.
  > Proposed 2026-11-05 (mosaic model-building): a block quote that starts with "Proposed <date> (<source>)" is a proposal, not yet accepted. It either adds a requirement under the next free ID of its section's area, or amends the requirement it follows without changing that requirement's text. The owner accepts a proposal by removing its quote markers, and drops it by deleting the block.

---

## Table of contents

1. Purpose, scope, glossary and non-goals
2. Architecture
3. The control plane (Dolt)
4. The generic workflow engine
5. Scheduling and parallelism
6. The agent runtime adapter, routes and provider health
7. Worktrees, merge-reconcile and integration
8. Robustness requirements
9. Modes and external effects
10. Operator authority and task controls
11. Command-line interface
12. Built-in controller operations
13. Daemon API, event stream and the optional UI child
14. Host safety
15. Observability
16. Security
17. Testing and verification of Fae itself
18. Phased delivery plan
19. Open questions
- Appendix A: defaults
- Appendix B: typed escalation kinds
- Appendix C: the default workflow, stated as content
- Appendix D: lessons that produced requirements

---

## 1. Purpose, scope, glossary and non-goals

### 1.1 Purpose

Fae is a local-first, AI-native orchestration framework for software engineering. One operator runs it on one host. It takes units of engineering work, drives each through a versioned, gated workflow of independent AI agent roles, and isolates each unit in its own version-control worktree. Every transition is backed by durable evidence and an audit trail. The framework is a supervised force multiplier: it enforces process mechanically rather than trusting agent memory, it never invents a waiver, and the human operator can steer any task.

Fae's distinguishing property is that **the workflow is data**. The engine knows a small, fixed library of stage primitives and a set of non-negotiable kernel safety floors. It knows nothing about any particular role, such as "spec analyst" or "coder". The default software-delivery pipeline is shipped as workflow content, and a workflow author can drop, add, reorder or parallelise stages within the kernel floors.

### 1.2 Glossary

| Term | Definition |
|---|---|
| **Instance** | A named, isolated Fae deployment on one host: one daemon, one control plane, one bound workspace, one process tree, one IPC socket. Several instances may coexist on a host. |
| **Daemon** | The long-lived per-instance process. It is the sole writer of the instance control plane and the only component that executes workflow transitions. |
| **Control plane** | The instance's durable, versioned SQL store (Dolt). It is the single source of truth for every mutable record. |
| **Operator** | The single human who owns the instance. The operator is the only authority for governance changes and for task-control verbs. |
| **Controller** | Any automation acting on the operator's behalf, for example a supervising agent session or a script. A controller has no authority of its own: it acts only through the same audited API as the operator, citing an operator message. |
| **Task** | One unit of work: a title, content (prompt or document), priority, mode and disposition, a target configuration, and a binding to one molecule. It moves through the states of its molecule. |
| **Backlog item** | The admission record of a task: identity, priority, acceptance text, optional write class, and edges. A backlog row mirrors its task's lifecycle state. |
| **Edge** | A directed dependency between two backlog items. A **blocking** edge gates release of the dependent until the prerequisite is satisfied. A **relates** edge is metadata only. |
| **Workflow template** | A named, versioned definition of a workflow: roles, stage graph, gates, caps, limits and policy hooks. It is identified by a stable workflow id. |
| **Workflow revision** | One immutable version of a workflow template's content, numbered 1, 2, 3 and so on, each with a parent revision and a content hash. |
| **Molecule** | The executing instance of one workflow revision. There is exactly one molecule per (workflow id, revision). Every task bound to that revision joins the same molecule's execution graph at its own stage. A molecule's content is immutable and its identity is a content hash. |
| **Role** | A data-declared agent persona: name, instructions, brief template, ordered model routes, tool profile, slot scope, cap and output schema. |
| **Role slot** | A capacity token for a role. Its scope is declared by the workflow: **workflow-global** (shared by every molecule of that workflow across all revisions) or **per-molecule task-bound** (one slot bound to one task, with fresh agent context per task). |
| **Stage** | A node in a workflow graph. Every stage is an instance of one engine primitive (Section 4.3). |
| **Session** | A running agent CLI process (for example Kiro or Claude) hosted by the runtime adapter, usually in a terminal-multiplexer pane. |
| **Turn** | One dispatch of a brief to a role session and the collection of its structured handoff. |
| **Brief** | The input composed for a turn: role instructions, the stage template rendered over bound artifacts, and the kernel-appended contract (output schema, handoff path and rules). |
| **Handoff** | The structured output file a role writes for a turn. The framework reads the handoff file and never the terminal scrape. |
| **Artifact** | A typed, schema-validated, content-addressed product of a stage, for example a spec, plan, RED test record, implementation report, review, verdict or test run. |
| **Evidence** | An artifact, or a fact derived from one, that a gate may read. Every evidence item carries provenance. |
| **Provenance** | The immutable chain recording how an artifact came to be: producer stage, role, binding (provider and model), session reference, input artifact hashes, tree hash and time. |
| **Gate** | A stage that evaluates a predicate over evidence and yields pass or fail with reasons. Gates are expressed in a small, total expression language. |
| **Kernel floor** | A platform safety property that workflow content can tighten but never weaken. Floors are validated as graph properties at validation time and as evidence checks at run time. |
| **Lease** | A durable, owner-identified, time-bounded grant of a scarce resource: a role slot, a heavy-op permit, a deployment target or a pane reservation. |
| **Heavy op** | A host-expensive command: a compile, a test run, a formatter over a large file, a build, a deployment, or a broad filesystem scan. |
| **Prophet** | The operator's liaison agent. A **per-task Prophet** holds the context of one task. The **global Prophet** holds instance-wide context. A Prophet relays and proposes; it holds no quality or transition authority. |
| **Oracle** | The default workflow's final-judgement role. It emits a typed decision per finding. It is content, not an engine concept. |
| **Target configuration** | A versioned, task-bound description of where and how a task's work happens: repository, workspace binding, build, test and deployment mechanisms, required tests, external-effect pins and policy. |
| **Mode** | A task's external-effect posture: **Local** (no external effects) or **Normal** (external effects permitted), with Normal split into **dry-run** and **live**. |
| **Disposition** | The persisted per-task bundle of mode, dry-run versus live, ticket room, code-review destination branch and push namespace. |
| **Drive** | The daemon's continuous scheduling loop. It runs in rounds; each round collects finished turns, evaluates gates, applies transitions and dispatches new turns. |
| **Park** | A per-task, per-round deferral for a transient reason (for example host pressure or an unhealthy route). A park consumes no attempt budget and is retried on a later round. |
| **Escalated** | A task state meaning "needs an operator decision". It is entered with a typed escalation kind. |
| **Retired** | A terminal state reachable only from Escalated, with a reason class (superseded-by, contained, obsolete). |

### 1.3 Scope

**FAE-GEN-001 (MUST)** Fae MUST provide a complete local orchestration stack: the daemon, the Dolt control plane, the workflow engine, the scheduler, the runtime adapter, the worktree manager, the CLI, the daemon API and event stream, and the built-in controller operations of Section 12.
- *Rationale:* the prior system relied on external scripts and a supervising agent for landing, rolling, watchdogs and cleanup. Those operations belong in the product.
- *Acceptance:* a fresh host with git, Dolt and one agent CLI can install Fae, start an instance, admit work, drive it to Done and perform a rolling restart using only Fae commands.

**FAE-GEN-002 (MUST)** Fae MUST be provider-neutral above the runtime adapter. No workflow semantics, gate or audit rule may depend on which agent CLI or model executed a turn.
- *Rationale:* routes fail over across providers, and evidence must mean the same thing whichever binding produced it.
- *Acceptance:* the full default pipeline passes on the scripted adapter, on adapter A and on adapter B with identical transition sequences for an identical recorded run.

**FAE-GEN-003 (MUST)** Fae MUST govern only Fae-owned resources: its own role sessions, worktrees, process tree, leases and the commands brokered through its own shims. It MUST NOT signal, kill or reconfigure processes it did not launch.
- *Rationale:* other sessions share the host; killing a foreign process is unsafe and outside Fae's authority.
- *Acceptance:* a test plants foreign panes and processes that consume host resources; Fae samples their pressure but never signals them.

**FAE-GEN-004 (MUST)** Fae MUST support trusted workspaces only, and MUST assume the agent CLIs it launches are permitted to operate inside the worktree they are launched in.
- *Rationale:* Fae is a single-operator tool on the operator's own host.
- *Acceptance:* the documentation states this, and starting an instance against an untrusted path is outside the supported contract.

### 1.4 Non-goals

**FAE-GEN-010 (MUST)** The following are out of scope and MUST NOT be built into the core:
1. multi-tenant or multi-operator operation;
2. a hosted or remote service mode;
3. any off-host network listener (Section 16);
4. spending money directly, or deploying to production;
5. external communication other than through the external-effect capability of Section 9 (ticket and code-review systems), and then only in Normal live mode;
6. the operator web UI itself, which will be specified separately (Section 13 specifies only what a UI needs from the daemon);
7. a general-purpose CI system.
- *Rationale:* these keep the trust boundary small and auditable.
- *Acceptance:* a review of the built binary's listeners, outbound effects and command surface finds none of the excluded capabilities.

**FAE-GEN-011 (MUST NOT)** Fae MUST NOT dual-write authoritative state to two stores, and MUST NOT keep authoritative state in agent memory, session transcripts or terminal scrapes.
- *Rationale:* a single durable authority is what makes resume, audit and verification possible.
- *Acceptance:* an inventory of every write path shows one authority (the control plane); transcripts are referenced by pointer only.

---

## 2. Architecture

### 2.1 Component model

| Component | Talks to | Through |
|---|---|---|
| Operator, using the `fae` CLI | Daemon | Instance socket (typed RPC) |
| Controller automation | Daemon | The same socket and RPCs; no private path |
| Optional UI process (a supervised child) | Daemon | The same socket and RPCs |
| Daemon | Dolt sql-server (its child) | MySQL wire protocol on loopback |
| Daemon | Agent sessions (panes or processes) | Runtime adapter |
| Daemon | Task worktrees | Worktree manager (git) |
| Daemon and role sessions | Host commands (builds, tests, formatters) | Command shims, heavy-op lease, host guard |

**FAE-ARC-001 (MUST)** Each instance MUST run exactly one long-lived daemon that holds an instance singleton lock, owns the instance control plane as its sole writer, binds the instance IPC socket, owns the instance process tree, and runs the drive.
- *Rationale:* one authoritative interpreter per instance gives deterministic transitions and one fleet.
- *Acceptance:* starting a second daemon for a live instance fails with a message naming the live owner's pid; a lock whose recorded owner is dead (pid and process start time both checked) is reclaimed with an audit row.

**FAE-ARC-002 (MUST)** The daemon MUST be idle-cheap: with no admitted work it MUST block on events and MUST NOT busy-poll.
- *Rationale:* the daemon runs for days.
- *Acceptance:* an idle daemon over 10 minutes accrues under 1 second of CPU time (default bound, measured from process accounting).

**FAE-ARC-003 (MUST)** All clients (CLI, controller automation, UI) MUST reach the daemon only through the typed RPC API on the instance socket. No client may open the control plane for writing.
- *Rationale:* every mutation must pass the validated state machine and the audit writer.
- *Acceptance:* the control-plane credentials are readable only by the daemon's user and are not exposed by any RPC; a CLI test against a running daemon performs every mutating verb with zero direct database connections.

**FAE-ARC-004 (MUST)** The IPC transport MUST be a per-instance Unix-domain socket inside an owner-only directory (mode 0700) under the instance root, with a schema-first, versioned RPC definition. Unary and server-streaming calls MUST be supported. A stale socket file MAY be removed before bind because the instance lock is already held.
- *Rationale:* filesystem permissions scope reachability to the operator; a schema gives typed, evolvable contracts.
- *Acceptance:* the socket file mode is 0600 within a 0700 directory; the RPC schema is a checked-in artifact; adding a field to a message does not break an older client (additive evolution test).

**FAE-ARC-005 (MUST)** Every RPC MUST present a per-instance credential of at least 32 random bytes, stored in a mode-0600 file inside the instance run directory and compared in constant time. Socket reachability alone MUST NOT grant control.
- *Rationale:* defence in depth on a shared host.
- *Acceptance:* a call without the credential, or with a wrong one, is refused with an authentication error and no side effect; a timing test shows no length- or prefix-dependent early exit.

**FAE-ARC-006 (MUST)** The daemon MUST NOT bind any TCP listener in production. A loopback TCP listener MAY exist only as a test seam, and then MUST refuse non-loopback peers.
- *Rationale:* no off-host exposure.
- *Acceptance:* in a running instance, the list of listening sockets owned by the daemon process contains only the Unix socket.

**FAE-ARC-007 (MUST)** The runtime adapter MUST be an interface boundary. Workflow policy, task state and audit semantics MUST live above it. At least three adapters MUST exist: a terminal-multiplexer adapter (hosting agent CLIs in panes), a direct-process adapter, and a deterministic **scripted** adapter that replays declared role outputs for tests and dry-run simulation.
- *Rationale:* the pipeline must be runnable end to end offline, and live runtimes change.
- *Acceptance:* the full default pipeline runs on the scripted adapter in the unit-test suite with no network, no agent CLI and no multiplexer installed.

**FAE-ARC-008 (MUST)** Each file-mutating task MUST execute in its own version-control worktree on its own branch, at a deterministic path under the instance's worktree root and with a deterministic branch name derived from the run id and task id.
- *Rationale:* worktrees isolate concurrent work, and merge-reconcile integrates it.
- *Acceptance:* two concurrent tasks editing the same file never observe each other's uncommitted bytes; the path and branch for a (run, task) pair are stable across restarts.

**FAE-ARC-009 (SHOULD)** The Go implementation SHOULD be organised as packages with acyclic dependencies: a pure domain package (types and errors, no I/O), a pure workflow package (graph, validation, gate language, transition legality), the control-plane package, the scheduler, the runtime adapters, the worktree manager, the host guard, the daemon API, and the CLI.
- *Rationale:* pure packages are the targets for exhaustive and property-based tests.
- *Acceptance:* a dependency check in CI fails on a cycle or on I/O imports in the pure packages.

### 2.2 Instance layout

**FAE-ARC-020 (MUST)** Each instance MUST have a root directory under a configurable home (default `~/.fae/instances/<name>/`) holding: the run directory (lock, pid file, socket, credential), the Dolt data directory, the workspace binding record, the supervision log, the backup and archive directory, and the worktree root.
- *Rationale:* one predictable place per instance makes reclaim, backup and cleanup safe.
- *Acceptance:* a fresh instance creates exactly these entries, and nothing outside the instance root and the bound workspace's ignored state directory.

**FAE-ARC-021 (MUST)** The first successful start MUST bind the instance to one workspace (a canonical absolute path). A later start against a different workspace MUST be refused. The start MUST also be refused when the current directory is not the declared workspace. Both checks MUST run before the lock, the plane or any write.
- *Rationale:* a daemon that drifts to another checkout writes into the wrong repository.
- *Acceptance:* tests cover both refusals and assert that no file was created.

**FAE-ARC-022 (MUST)** Workspace paths and task bindings MUST be validated on raw bytes and rejected, never normalised, when they are non-absolute or contain `.` or `..` components, doubled or trailing separators, NUL bytes or invalid UTF-8. Containment of any symlink MUST be checked at the time of use.
- *Rationale:* byte equality must equal path equality, and a transformed path hides an escape.
- *Acceptance:* a table test covers every rejected form; a symlink planted after binding that points outside the root is refused at use.

**FAE-ARC-023 (MUST)** A workspace that is a package inside a multi-package build workspace (for example a Brazil workspace) MUST be detected, and the task worktree provider chosen accordingly. Fae MUST NOT run expensive build-workspace creation, sync or merge operations on the drive's hot path.
- *Rationale:* a build-workspace sync on this class of host was measured at I/O pressure around 60 for about 5 minutes.
- *Acceptance:* a test with a planted build-workspace marker selects the matching provider; a trace of a full drive shows zero build-workspace sync invocations.

---

## 3. The control plane (Dolt)

**FAE-CP-001 (MUST)** The control plane MUST be Dolt. Each instance MUST run its own Dolt sql-server, launched and supervised by the daemon as a child process over the MySQL wire protocol on a loopback address.
- *Rationale:* Dolt provides SQL, versioned commits, diffs and branches over the control plane. This is a hard requirement, not one option among several.
- *Acceptance:* `fae status` reports the plane as Dolt with its pid, port and data directory; the Dolt process's parent is the daemon.

**FAE-CP-002 (MUST)** The Dolt child MUST die with the daemon: it MUST receive a parent-death signal, and it MUST be stopped explicitly (graceful termination with a flush, then a bounded wait of default 30 s, then a forced kill) on daemon shutdown.
- *Rationale:* a killed daemon previously left its Dolt server orphaned to init.
- *Acceptance:* killing the daemon with SIGKILL leaves no Dolt process for that data directory within 5 s; a graceful shutdown logs the Dolt termination and its duration.

**FAE-CP-003 (MUST)** At startup the daemon MUST reap an orphaned Dolt server for its own data directory (parent is init, data directory matches, recorded in the instance pid file), using the same terminate-then-kill sequence, and MUST verify its socket is gone before starting a new one. It MUST NOT touch a Dolt server of any other data directory.
- *Rationale:* orphan reaping is safe only when ownership is proven.
- *Acceptance:* a test plants an orphan for the instance and one for a foreign data directory; only the first is reaped, and an audit row records it.

**FAE-CP-004 (MUST)** The Dolt port MUST be derived deterministically from the data directory (default base 3658 plus a hash of the canonical data directory modulo 4096) unless pinned. Startup MUST refuse an endpoint already served by another data directory, and MUST refuse to adopt a server on its own data directory that is not recorded in its pid file.
- *Rationale:* two instances on one port corrupt each other, and an unrecorded server is of unknown provenance.
- *Acceptance:* tests cover the foreign-owned refusal and the unrecorded-server refusal, each naming the remedy.

**FAE-CP-005 (MUST)** The Dolt binary location MUST be configurable and MUST default to resolving `dolt` on PATH. No absolute user-specific default path may exist.
- *Rationale:* portability.
- *Acceptance:* a clean environment with `dolt` on PATH starts without configuration.

**FAE-CP-006 (MUST)** Startup MUST verify the server, the repository, the schema version and control-plane consistency before the daemon accepts work. On a transient failure the daemon MUST attempt a bounded restart and reconnect (default 3 attempts with backoff) and re-validate consistency, recording the outage and the recovery. It MUST fail affected work only when it cannot return the plane to a known-consistent state, and MUST never guess a transition or repeat an irreversible action because of an outage.
- *Rationale:* a service failure must not become a semantic failure.
- *Acceptance:* a fault-injection test kills the Dolt child mid-drive; the daemon restarts it, re-validates, resumes, and the transition log has no duplicate or invented row.

**FAE-CP-007 (MUST)** Every control-plane mutation MUST run inside a transaction through one commit seam. On a serialization failure (MySQL error 1213 / SQLSTATE 40001) the seam MUST retry with bounded, jittered backoff (default 5 attempts), re-running the **entire transaction body** each time. It MUST NOT re-issue only the commit statement. After the bound it MUST return a typed retryable error, which the drive treats as a per-round transient (Section 8), never as a drive exit.
- *Rationale:* concurrent writers (for example an operator retirement batch) caused 10 of 43 writes to fail and killed the drive, although Dolt's own message says to restart the transaction.
- *Acceptance:* tests: an injected 1213 on the first N−1 attempts commits on attempt N, and the body's side-effect counter equals N; a non-1213 error propagates unchanged with no retry; exhausting the bound yields the typed error and the drive survives the round; the first-attempt success path makes no extra round trip.

**FAE-CP-008 (MUST)** The daemon MUST create a Dolt commit for every logical batch of control-plane writes (at least once per drive round that wrote anything, and at every audited operator verb), with a message naming the round or verb and the actor.
- *Rationale:* versioned history is the reason for choosing Dolt; it supports diff, blame and point-in-time inspection of the plane.
- *Acceptance:* after a drive of three tasks, the Dolt log shows one commit per writing round and per verb, and a diff between two commits shows exactly that round's rows.

**FAE-CP-009 (MUST)** Reads MUST be scoped. Every read path MUST select only the tables, columns and rows it needs, using keyed or indexed predicates. No read may materialise every table.
- *Rationale:* full-table materialisation made one read RPC take a median of 7.3 s (maximum 15.6 s) and starved other reads.
- *Acceptance:* an instrumentation test counts the statements and rows read per RPC against a synthetic plane of 100k transition rows; the topology read touches at most the molecule, task and lease tables, and completes in under 200 ms (default budget).

**FAE-CP-010 (MUST)** The schema MUST be versioned and migrated forward only, by numbered migrations that run at startup inside a transaction, each recorded in a migrations table. A plane whose schema version is newer than the binary MUST be refused.
- *Rationale:* rolling restarts and rollbacks must not corrupt the plane.
- *Acceptance:* tests cover a migration, the refusal of a newer schema, and idempotent re-run.

**FAE-CP-011 (MUST)** The control plane MUST hold, at minimum, these record families. Each is one or more tables; a field list in words follows.

| Family | Key fields and constraints |
|---|---|
| Workflows | workflow id (text, stable); revision (integer, ≥1, assigned as latest+1); parent revision (integer or null); name; content (canonical JSON); content hash (hex, derived, never set by a caller); created by; created at |
| Workflow status | workflow id; retired at (first value kept); retired by; reason |
| Molecules | molecule id (content hash of resolved content); workflow id; revision; content hash; resolved content; created at; ended at (null while it has non-terminal tasks) |
| Target configurations | id; revision; content (canonical JSON); content hash; created at |
| Tasks | task id; run id; molecule id; target config id and revision; state label; attempt count; attempt limit (per task, mutable); priority; disposition (mode, dry-run or live, room, destination branch, push namespace); escalation kind and reason; commit ref; review ref; created and updated at |
| Backlog items | item id (keyed upsert identity); source reference; priority (integer ≥10, larger runs first); acceptance (text); write class tags (informational); state label; deferred until; created and updated at |
| Backlog edges | from id; to id; kind (blocking or relates); created by; created at |
| Transitions | id (monotone); task id; from state; to state; owner (stage or verb); attempt; evidence digest; note; authorizing message id (when operator-initiated); time |
| Decisions | id; task id; kind; payload (canonical JSON); rationale; initiating role and session reference; supersedes id; time |
| Artifacts | artifact id (content hash); task id; kind; schema id and version; content or content reference; provenance (producer stage, role, binding, session reference, input artifact ids, tree hash); time |
| Leases | lease id; kind (role slot, heavy op, deployment, pane reservation); scope key; owner (pid and process start time, or task and stage); weight; priority class; granted at; expires at; released at; release reason |
| Panes and sessions | session id; pane id; role; task; binding; owner pid and start time; launched at; reaped at; process-tree root |
| Provider health | provider; model; class of last outcome; score; consecutive failures; last ok at; last failure at; hold until |
| Operator messages | message id; thread root; author; text; created at; consumed-by references |
| External effects | effect id; task id; tool (canonical name); argument digest; pinned values; reported external id; reconciliation state (reported, validated, refused-but-recorded, adopted); time |
| Code reviews | review id (external); task id; branch; tree hash; revision count; publish state; merge state; last status snapshot |
| Ticket outbox | task id; ticket id; desired lifecycle step; emitted flag; attempts; last error |
| Schedules | schedule id; kind (cron-like or loop); specification; lifecycle state; provenance |
| Engine runs | run id; task id; command; tree hash; sanitized-environment delta; exit code; executed count; output digest; started and finished at |
| Alerts | id; severity; source; text; time; acknowledged at |

- *Rationale:* these are the records the engine, scheduler and verbs need.
- *Acceptance:* a schema test asserts every family and its key constraints exist.

**FAE-CP-012 (MUST)** Long text payloads that can grow without bound (remediation lists, review bodies, test output) MUST be stored by reference: as a content-addressed artifact, with the table row holding the digest and size. No single row column may exceed a configured limit (default 64 KiB).
- *Rationale:* a 90 KB remediation payload once overflowed a column and exited the drive twice.
- *Acceptance:* a test writes a 1 MiB remediation set; the row holds a reference, the artifact round-trips, and the drive continues.

**FAE-CP-013 (MUST)** Before any destructive cleanup of instance data, a scratch instance or a retired task's refs, Fae MUST take a backup: a Dolt backup or a pushed Dolt branch of the plane, plus an archive ref for each git branch to be deleted. It MUST verify the backup or archive exists before deleting anything.
- *Rationale:* sole-copy work has been lost to cleanups; nothing is deleted without a verified copy.
- *Acceptance:* a cleanup test with the archive step failing deletes nothing and reports the failure.

**FAE-CP-014 (MUST)** A legacy single-workspace store, if present, MUST be reachable only through the explicit one-way import command (Section 11). No default path may create or require it.
- *Rationale:* one authority.
- *Acceptance:* a search of default code paths finds no legacy-store open outside the import command.

**FAE-CP-015 (MUST)** Every canonical-JSON content hash MUST use a cryptographic hash (SHA-256), computed over canonical JSON: object keys sorted recursively, array order kept, no insignificant whitespace, numbers in a single canonical form.
- *Rationale:* identity and tamper detection need a cryptographic hash; the prior system used a non-cryptographic one.
- *Acceptance:* a property test shows that semantically equal content with different key order produces the same hash, and that a one-byte change produces a different hash.

---

## 4. The generic workflow engine

This section is the heart of Fae. The engine is a generic interpreter of workflow graphs. All domain behaviour, including the default software-delivery pipeline, is content.

### 4.1 Workflows as data

**FAE-WF-001 (MUST)** A workflow template MUST be a named, versioned document. Its content MUST declare: schema version; roles; artifact types; stages (each an instance of one primitive); edges between stages; gates; limits (attempt limit, per-loop caps, test-run cap); role slot scopes and caps; molecule concurrency; task-class overlays; optional policy hooks; and the default target-configuration requirements.
- *Rationale:* workflow authors control execution; the engine stays generic.
- *Acceptance:* a schema exists for workflow content; the default workflow validates against it; a workflow lacking a required section fails validation with a message naming it.

**FAE-WF-002 (MUST)** Workflow revisions MUST be immutable and append-only. Writing content creates revision latest+1 with parent = latest; a caller cannot choose the number. There MUST be no in-place edit and no delete. Retirement MUST be an object-level tombstone that keeps the first retirement time and refuses new bindings to the retired workflow, while bound molecules continue unaffected.
- *Rationale:* reproducibility and audit.
- *Acceptance:* tests cover numbering, parent linkage, immutability (an update attempt is refused), retirement semantics, and that a retired workflow still serves its running molecules.

**FAE-WF-003 (MUST)** Content MUST be validated on write. A write of invalid content MUST be refused with all validation errors listed, and nothing stored.
- *Rationale:* the prior system stored unvalidated content and discovered errors only at bind time or at a transition.
- *Acceptance:* a write with three independent errors is refused with all three named; the revision counter does not advance.

**FAE-WF-004 (MUST)** Parsing MUST be fail-closed and strict: unknown keys at any level, unknown labels (state, role, primitive, artifact type, reasoning effort, scope), wrong types and missing required fields MUST all be errors.
- *Rationale:* silently ignored keys hide authoring mistakes.
- *Acceptance:* a table test injects each class of error and asserts a refusal naming the path.

**FAE-WF-005 (MUST)** A task MUST bind to a molecule when it is admitted to execution. The binding MUST be durable and MUST be read before any write: a task that already has a binding is never bound again. A task MUST finish on its pinned revision even after newer revisions exist.
- *Rationale:* in-flight work never silently adopts later workflow edits.
- *Acceptance:* a test writes revision N+1 mid-run; the in-flight task completes on N, and a newly admitted task binds N+1.

**FAE-WF-006 (MUST)** There MUST be exactly one molecule per (workflow id, revision). Every task bound to that revision MUST join the same molecule. A content-hash mismatch between a stored molecule and a re-resolution of the same revision MUST be refused. A molecule MUST end when it has no non-terminal tasks.
- *Rationale:* operator ruling: one execution graph per workflow and version holds many tasks.
- *Acceptance:* ten tasks bound to one revision share one molecule id; a planted hash mismatch is refused.

**FAE-WF-007 (MUST)** Every admission path (CLI run, backlog admission, intake, Prophet restart) MUST be able to carry a per-item workflow selector (workflow id and optional revision; no revision means the latest non-retired). An empty selector MUST mean the instance default workflow. An unknown id or revision MUST fail closed at admission, not at bind.
- *Rationale:* different tasks need different workflows in the same instance.
- *Acceptance:* a backlog payload with three items on three workflows binds three molecules.

**FAE-WF-008 (MUST)** The engine MUST interpret only the data-driven graph. There MUST be no code-constant fallback state machine for unbound tasks.
- *Rationale:* two paths drift apart.
- *Acceptance:* a search finds one transition-legality function, driven by molecule content plus the kernel rules.

> Proposed 2026-11-06 (mosaic code-to-model audit). Source: mosaic `requirements/fae-hierarchical-workflows.md` FAE-WF-101 (carried, widened); catalog D-47, D-67, P-55, P-88, P-91, C-04, C-69, C-74, C-82, C-83, C-106; audit in mosaic `requirements/fae-code-to-model-audit-2026-11-06.md` (rows workflow W03, W19; gates G4, G13, G32; control 1).
>
> **FAE-WF-101 (MUST)** A workflow MAY declare a **signature**: `inputs`, a map of names to a JSON Schema, and `outputs`, a map of names to an artifact type (optionally narrowed by a JSON Schema); an input MAY carry a `default`, and an input without one is required. Admission (FAE-API-034) and every SubWorkflow input mapping MUST bind every required input, fill the defaults and validate every value before any stage is dispatched; missing inputs, unknown inputs and schema failures MUST be refused together, in one refusal that names each input. Each bound input MUST be pinned on the task and recorded as an artifact with provenance `input:<name>` that stages, gates, Branch arms, `branches_from` (FAE-WF-096) and HostCommand typed placeholders (FAE-WF-035) can select. Each declared output MUST name an artifact type, and the validator MUST refuse an output that no stage can produce. A task MUST reach Done only when every declared output has a recorded artifact of its type, whichever stage produced it; otherwise it MUST escalate, naming each missing output. A SubWorkflow's `output_map` MAY name a child's declared output instead of a child stage, and the validator MUST check input and output mappings against the child's signature.
> - *Rationale:* a model-building run starts from a per-repository profile, a scope, a parent contract and a list of component ids. Before fae's 2026-11-06 implementation pass these reached a task only as free-text admission content, which no gate, fan-out or command could read, and a child's output written by its failure handler was missed because outputs mapped by child stage (audit row workflow W03); the implementation is `workflow/signature.go`, `daemon/rpc.go:21-24`, `engine/tree_inputs.go:125-148` and `daemon/subworkflow.go:57-87`.
> - *Acceptance:* tests: (a) an admission missing two required inputs and carrying one unknown input is refused once, naming all three; (b) an omitted defaulted input reads its default; (c) a gate and a Branch arm read input `profile` as an artifact with provenance `input:profile`; (d) a `branches_from` over an input list expands one branch per element; (e) a SubWorkflow output mapping that names an undeclared child output fails validation; (f) a child whose declared output was produced by a handler stage binds into the parent; (g) a task whose declared output is missing at its end escalates instead of reaching Done.

> Proposed 2026-11-07 (mosaic code-to-model re-audit). Source: mosaic `.claude/skills/mosaic-generate/compose/zoom.yaml:56-60`, `:71-98`; `internal/compose/stages.go:332-341` (compose records each mapped child output as its own parent artifact); fae `engine/subworkflow.go:572-596`, `workflow/signature.go:215`; probe `reaudit/hier/p/h8_test.go` TestH8. Covers the e2e re-audit's proposal that a signature output be a projection of a child artifact.
>
> **FAE-WF-121 (MUST)** A SubWorkflow `output_map` entry that names a child's declared output (FAE-WF-101) MAY declare `as: <artifact type>`. The engine MUST then also record that child output as a parent artifact of type `as` when the child binds. Its stage is the SubWorkflow stage, its fields are the child artifact's, and its provenance names the child task and the child's artifact id. The validator MUST check that the child output's type and schema satisfy `as`, and MUST count a SubWorkflow stage with such an entry as a producer of `as` for the parent's own declared outputs, for `branches_from` and gate selection by type, and for a later `from_parent` binding (FAE-WF-102). The composite SubWorkflow artifact is still recorded unchanged.
> - *Rationale:* mosaic's zoom template is a thin wrapper that runs the full or the light ontology pipeline as a child and must expose the child's `ontology`, `refines`, `timing` and `change_requests` as its own outputs, so that a later zoom can bind `from_parent: {parent: ontology}` from it and the run tree lists them. Today a SubWorkflow records only one composite artifact, so the wrapper's signature fails validation ("no stage produces declared output"), and a `from_parent` bound to it would receive `{child_task, outputs: {...}}` instead of the ontology.
> - *Acceptance:* tests: (a) a wrapper whose signature declares `ontology: Ontology`, with one SubWorkflow stage mapping the child's `ontology` output `as: Ontology`, validates and reaches Done with an `Ontology` artifact whose provenance names the child's artifact; (b) two alternative SubWorkflow stages (one reached through a gate's failure edge) mapping `as` the same type both satisfy the wrapper's declared output; (c) a detached child of the wrapper binding `from_parent: {parent: ontology}` receives the child's ontology fields, not the composite; (d) `as` naming a type whose schema the child output does not satisfy fails validation.

> Proposed 2026-11-06 (mosaic code-to-model audit). Source: mosaic `requirements/fae-hierarchical-workflows.md` FAE-WF-104 and its update in `fae-model-building-assessment-execution.md` §3.2 (carried); `internal/compose/stages.go:42`, `internal/compose/engine.go:156-172`, `internal/compose/executor_claude.go:264-307` (catalog C-22, C-43, C-44); catalog D-60, D-70; audit in mosaic `requirements/fae-code-to-model-audit-2026-11-06.md` (row gates G21).
>
> **FAE-WF-104 (MUST)** A workflow MAY declare `produces: artifacts`. Its tasks then produce only artifacts and files in an **output directory**, and no commits:
> 1. Such a workflow MUST NOT need VcsCommit, Merge or Integrate stages, and the validator MUST refuse those stages and a code-review ExternalEffect in it, and the floors for code-bearing classes (protected commit, test first, frozen test) MUST NOT apply to it; its Done needs only its own gates. A workflow whose documents must be committed uses FAE-CTL-052 instead.
> 2. The output directory MUST be an absolute path the daemon creates before the first dispatch: `<instance root>/artifacts/<task id>/out` for a root task, and `<parent output dir>/<child task id>` for a SubWorkflow or detached child (FAE-WF-102), also when the parent has finished and also when the parent is not a `produces: artifacts` task (every task has an output directory). It MAY lie outside every worktree, and it is passed to HostCommands through the typed placeholder `{{output_dir}}` (FAE-CTL-107) and to sessions through a fixed environment variable.
> 3. The task's file scope is its bound inputs and read roots for reading and its output directory for writing. The output directory, the staging area and declared scratch paths (FAE-WF-111) MUST be the only places its sessions and commands may write, and every `writes` glob of the task (FAE-SEC-011, FAE-WF-095, FAE-WF-116) is relative to the output directory: the adapter grants write tools on the directory (for Claude Code, write and edit rules for it plus `--add-dir`), and a write anywhere else is reverted and the turn refused (FAE-RT-015).
> 4. Its files MUST be readable by later stages of the task and, read-only and under their read policies (FAE-SEC-008), by its descendants' sessions and commands (a reachable path beside their own output directory under FAE-RT-015), are never committed, and are kept for as long as the task's records are.
> - *Rationale:* a zoom child's deliverable is a model and its reports, not a code change. It belongs under its parent's run by task id, and a child must not run with its parent's code-change scope. At the 08:25 UTC audit only change-producer stages could write, their work was committed, and the only paths outside the worktree were read-only read roots (`workflow/agentic.go:112-121` then); fae's working tree at 09:58 UTC implements `produces: artifacts` (`workflow/classes.go:23-35`, `daemon/task_directories.go:18-80`, `workflow/agentic.go:112`) but does not yet refuse VcsCommit, Merge or Integrate stages in such a workflow.
> - *Acceptance:* tests: (a) a `produces: artifacts` template made only of AgentTurn and Gate stages validates and runs to Done without a commit; (b) a turn that writes outside the output directory is reverted and refused; (c) a child's files land under `<parent output dir>/<child task id>/`, also for a detached child of a Done parent; (d) a later stage reads a file an earlier stage wrote there; (e) a `produces: artifacts` template containing a VcsCommit stage fails validation.

### 4.2 Task state model

**FAE-WF-020 (MUST)** The engine MUST distinguish **kernel states**, which exist in every workflow, from **stage states**, which are the workflow's own stages. The kernel states are: Pending (admitted, not released); Blocked (awaiting remediation); Parked (a transient per-round deferral, a derived view, never a stored terminal); Escalated (needs an operator decision); Done (terminal success); Retired (terminal, only from Escalated); and Cancelled (terminal by operator verb, from Pending or Blocked only).
- *Rationale:* lifecycle controls must work identically across workflows.
- *Acceptance:* a property test over generated workflows shows that these states and their legal kernel edges exist in every validated molecule.

**FAE-WF-021 (MUST)** Kernel edges MUST be exactly the following, plus the workflow's own stage edges. Every non-terminal state has an edge to Escalated. Escalated has exactly two out-edges: to Retired (operator verb with reason) and to the workflow's first stage (operator restart with new information, Section 10). Done, Retired and Cancelled have no out-edges.
- *Rationale:* the prior system had no exit from Escalated and accumulated 50 unresolvable escalated rows.
- *Acceptance:* an exhaustive test over the kernel graph checks every pair; a property test checks that terminal states have no out-edges in every generated molecule.

> Proposed 2026-11-06 (mosaic code-to-model audit), amendment to FAE-WF-021: Escalated gains a third out-edge, to the stage that escalated, taken only by the audited verb ResumeAtStage (FAE-OPS-035). The stage that escalated is the stage the task was in when it entered Escalated; when that is an Escalate stage, it is the stage whose failure route led there, which the engine MUST record with the escalation. The other two out-edges and every other kernel edge stay as written. Source: mosaic `internal/compose/engine.go:70-89`, `:370-417`, `:459-472` (catalog C-89); at the 08:25 UTC audit a resumed Escalated task returned to the first stage (`engine/control.go:176-200` then); fae's working tree at 09:58 UTC implements the edge (`engine/control.go:67-119`) and still sends Resume and RestartWithInformation to the first stage (`:201-260`); audit in mosaic `requirements/fae-code-to-model-audit-2026-11-06.md` (row gates G18). Acceptance: the exhaustive kernel-graph test admits Escalated to the recorded stage only through ResumeAtStage, and a task escalated through an Escalate stage resumes at the stage whose route led there.

**FAE-WF-022 (MUST)** Every transition MUST be applied by the daemon in one transaction that appends the transition row, updates the task projection, writes the evidence digest and, where applicable, the decision row. A transition whose evidence is incomplete MUST fail closed.
- *Rationale:* atomicity makes resume deterministic.
- *Acceptance:* fault injection between any two writes leaves either the full transition or none.

**FAE-WF-023 (MUST)** After a task enters a terminal state the engine MUST NOT dispatch, collect or transition it further. An in-memory forward transition attempted from a stale pre-terminal state MUST be refused and logged as a defect.
- *Rationale:* the prior engine kept driving a task after escalating it, asking the verifier to record a forward transition from a stale state.
- *Acceptance:* a test escalates a task with a turn in flight; collection of that turn records evidence but applies no transition.

### 4.3 Stage primitives

**FAE-WF-030 (MUST)** The engine MUST provide exactly the following primitive library. Every stage in every workflow MUST be an instance of one primitive. Adding a primitive is an engine change; workflows cannot define new primitives.

| Primitive | Purpose | Required configuration | Produces |
|---|---|---|---|
| **AgentTurn** | Dispatch one turn to one role. | role; brief template; input bindings (named artifacts); output artifact type (schema); optional phase label; optional tool profile override (tighten only) | one artifact of the declared type, with provenance |
| **Review** (Vote) | N independent turns over the same subject, aggregated. | reviewer role or roles (one per dimension); count per dimension; subject binding; finding schema; aggregation rule: all-pass, k-of-n, or any-blocking-fails | N review artifacts plus an aggregate verdict |
| **Gate** | Evaluate a predicate over evidence. | expression (Section 4.6); failure route; human-readable name | a gate-result artifact (pass/fail, reasons, the evidence ids read) |
| **HostCommand** | Run a brokered command (tests, build, lint) in the task worktree. | command template (argument vector, never a shell string); expected-shape declaration (for example "exactly one test named X"); heavy-op weight; timeout; environment policy | an engine-run artifact (exit code, executed count, matched tests, output digest, environment delta, tree hash) |
| **FreezeArtifact** | Record an immutable identity of an artifact or of a set of declared paths. | subject binding; identity function (normalised content of declared test functions, or exact bytes of declared files) | a frozen-identity artifact |
| **CompareFrozen** | Assert that a frozen identity still holds, or detect a change. | frozen binding; current subject | a compare artifact (unchanged or changed, with a diff summary) |
| **VcsCommit** | Commit exactly the task's declared and reviewed files in the task worktree. | file-set binding; message template; target (task branch) | a commit artifact (sha, tree hash, file list) |
| **Merge** (Reconcile) | Bring the task worktree onto the current integration tip, preserving the task's own work. | integration target | a reconcile artifact (clean, conflicted with paths, or refused with the missing hunks) |
| **Integrate** | Land a committed task onto the integration branch or target branch. | target; containment policy | an integration artifact (landed sha, contained-by sha, or conflict) |
| **ExternalEffect** | Perform one external action through a granted role or adapter with daemon-pinned arguments. | effect kind (ticket create/update; code-review create, revise, publish, merge, comment, read status); pinned argument set; idempotence key; mode requirement | an external-effect artifact (canonical tool, argument digest, external id, reconciliation state) |
| **Escalate** | Move the task to Escalated with a typed kind. | kind; reason template | an escalation decision |
| **HumanDecision** | Wait for an operator decision on a declared question with declared options. | question; options; timeout behaviour (stay waiting, never auto-decide) | a decision artifact citing the operator message |
| **Loop** (Retry) | Repeat a sub-graph until a gate passes or a bound is hit. | body; exit gate; maximum iterations; backoff; behaviour on exhaustion (escalate with kind) | loop bookkeeping |
| **Branch** | Route on a gate result or on a typed decision field. | discriminant; arms; default arm required | none (routing only) |
| **FanOut / FanIn** | Run parallel sub-paths and join them. | branches; join rule (all, any, k-of-n) | join verdict |
| **SubWorkflow** | Run another workflow revision as a child task graph. | workflow selector; input mapping; output mapping | child artifacts mapped into the parent |

- *Rationale:* a small, fixed, well-tested library over which kernel floors can be proved.
- *Acceptance:* the validator rejects any stage whose primitive is not in the table; each primitive has its own conformance test suite on the scripted adapter.

> Proposed 2026-11-05 (mosaic model-building), amendment to FAE-WF-030: the HostCommand row's "Produces" is extended by FAE-WF-095 (a check command yields a parsed, schema-validated artifact instead of test counts), and the FanOut / FanIn row's "Required configuration" is extended by FAE-WF-096 (a branch list bound at run time, concurrent branches, change-producing branches). No primitive is added.

> Proposed 2026-11-06 (mosaic code-to-model audit), amendment to FAE-WF-030: the table gains the primitive **ChangeRequest** (FAE-WF-103): purpose "Record a request addressed to an ancestor task about one of its artifacts"; required configuration "target (`parent` or an ancestor's task id); the artifact it is about; bound request value"; produces "a request record in the target's request list; no change to the target". Two rows change text: the SubWorkflow purpose becomes "Run a workflow revision (another, or under FAE-WF-100 the task's own) as a child task graph", and HumanDecision's options MAY be omitted when `item_options` are declared (FAE-CTL-108). Existing rows are otherwise extended without changing their text: AgentTurn, Review, HostCommand and SubWorkflow by an optional `failure` route (FAE-WF-112); AgentTurn and Review by a stage `brief` (FAE-CTL-107), and AgentTurn by `operator_may_supply` (FAE-WF-118); HostCommand by `capture` (2026-11-06 amendment to FAE-WF-095) and `generate` (FAE-WF-116); HumanDecision by `items_from` and `item_options` (FAE-CTL-108); FanOut / FanIn by nested regions and different static branches (FAE-WF-113), selection by artifact type, a `failure` route, a join artifact and `rule: settle` (2026-11-06 amendments to FAE-WF-096); SubWorkflow by `workflow: self` under a recursion budget (FAE-WF-100), signature-checked mappings (FAE-WF-101), placement inside a FanOut branch (FAE-WF-099) and `on_child_escalation` (FAE-WF-114), projections in `input_map` (FAE-WF-115) and a read policy and read-only files bound for the child (FAE-WF-117); FanOut also by projections in `branches_from` (FAE-WF-115); AgentTurn also by `schema_retries` (2026-11-06 amendment to FAE-WF-034) and a stage `turn_budget` (2026-11-06 amendment to FAE-RT-015); HostCommand also by in-argument placeholders and `{{item.<field>}}` (2026-11-06 amendment to FAE-WF-035) and `accept` (FAE-WF-111); any stage by `writes` (FAE-SEC-011), `scratch` and `staging` (FAE-WF-111), `run_stats` (FAE-WF-120) and, inside a FanOut branch, element-parameterised `reads` (2026-11-06 amendment to FAE-SEC-008). Acceptance: the validator accepts each new configuration and still refuses any primitive outside the table. Source: audit in mosaic `requirements/fae-code-to-model-audit-2026-11-06.md` (rows workflow W04, W18, W20, W25).

**FAE-WF-031 (MUST)** No-bypass: a stage MUST run only when every declared input binding resolves to recorded evidence in the control plane for this task, at the required schema version, with valid provenance.
- *Rationale:* a stage that runs on missing inputs launders a gate.
- *Acceptance:* a property test deletes a random input artifact from a generated run; the dependent stage never dispatches and the task parks with a reason naming the missing input.

**FAE-WF-032 (MUST)** Stage executors MUST be asynchronous with respect to the drive. Dispatching a turn, waiting for a host command, or waiting for an external status MUST NOT block the drive thread; the drive collects completions as events.
- *Rationale:* a synchronous verification turn per transition was measured at 4 to 7 hours of drive-thread blocking per 48 hours.
- *Acceptance:* a test with a 10 s scripted turn on task A shows task B's stages dispatching and collecting during that wait.

**FAE-WF-033 (MUST)** A Review stage MUST run each dimension's reviewers in separate, isolated sessions with fresh context, and MUST NOT give any reviewer access to another reviewer's output before aggregation. A reviewer MUST NOT be the session or role-turn that produced the subject.
- *Rationale:* a merged dimension is graded as a footnote inside the host dimension's priorities; this was measured, when two fresh dimensions graded as blocking what a merged reviewer had filed as non-blocking.
- *Acceptance:* tests assert distinct session ids per dimension, that briefs contain no sibling review, and that a review produced by the subject's producing session is rejected.

**FAE-WF-034 (MUST)** An AgentTurn's output MUST be validated against its declared JSON Schema. A schema failure MUST be recorded as a typed outcome (malformed output) that feeds provider health and counts against the stage's bounded retry, and MUST NOT exit the drive.
- *Rationale:* malformed output is a per-turn fact, not an engine failure.
- *Acceptance:* a scripted turn returns invalid JSON; the outcome is recorded, health is penalised, the stage retries within its bound, then the task escalates with a typed kind.

> Proposed 2026-11-06 (mosaic code-to-model audit), amendment to FAE-WF-034: an AgentTurn MAY declare `schema_retries: N` (default 1, 0 allowed, at most 16; the default applies to revisions written after this amendment, and stored revisions keep their bound under FAE-WF-093). A schema failure MUST then be re-dispatched at most N times, counted apart from the same-gate bound, each time as a new turn whose brief carries a `prior_rejection` data block listing every validation error of the rejected output (bounded like engine feedback). After N the stage takes its failure route (FAE-WF-112), or, when it has none, escalates with kind SameGate. A turn-budget exhaustion is not a schema failure and does not use these retries. Source: mosaic catalog P-17, P-94, D-64, C-26, C-32; at the 08:25 UTC audit a malformed output was re-dispatched until 3 identical reasons and the next brief carried no record of the rejection (cited then as `engine/engine.go:716`, `:1596-1604`, `:1970-1988`, `execution/executor.go:946-955`, lines that now hold other code); fae's working tree at 09:58 UTC implements `schema_retries` (`engine/engine.go:2145-2166`, `workflow/workflow.go:330-337`) and a `prior_rejection` block holding the last rejection reason (`execution/executor.go:986`, `:1177-1182`); audit in mosaic `requirements/fae-code-to-model-audit-2026-11-06.md` (row agent AE-10).
> - *Acceptance:* tests: (a) a scripted turn whose output misses two required fields is retried once, and its next brief names both fields; (b) a second invalid output takes the failure route; (c) `schema_retries: 0` fails on the first invalid output.

**FAE-WF-035 (MUST)** HostCommand templates MUST be argument vectors with typed placeholders bound from artifacts, never shell strings. The engine MUST validate the bound vector against an allowlist of command shapes declared by the target configuration.
- *Rationale:* prevents injection from task text, and lets the gate know what the command means.
- *Acceptance:* a task whose acceptance text contains shell metacharacters produces a vector with those characters as literal arguments; a vector outside the allowlist is refused.

> Proposed 2026-11-06 (mosaic code-to-model audit), amendment to FAE-WF-035 (SHOULD): a placeholder, whether a named binding or a FAE-CTL-107 reference (`{{input.<path>}}`, `{{item.<path>}}`, `{{output_dir}}` and the others), SHOULD be allowed inside an argument between a literal prefix and suffix (`fragments/{{item.id}}.yaml`, `--seed={{input.seed}}`), never as the executable. The bound value MUST be a scalar and is still checked against its declared type (the binding's type, the input's signature type or the item schema) before composition, and the composed argument is matched against an allowlist shape with the same prefix and suffix. Inside a FanOut branch, fields of the branch element MAY be bound directly as `{{item.<field>}}`. Source: mosaic `.claude/skills/mosaic-generate/compose/ontology-pipeline.yaml:507-516` (catalog P-48), `internal/compose/stages.go:169-210` (C-52); at the 08:25 UTC audit a partial placeholder was refused (`workflow/command.go:201-203`, `:314-316`); fae's working tree at 09:58 UTC composes FAE-CTL-107 references inside an argument without a type check (`workflow/command_references.go:26-100`) and still refuses a partial named binding (`workflow/command.go:200-203`); audit in mosaic `requirements/fae-code-to-model-audit-2026-11-06.md` (row gates G2).
> - *Acceptance:* tests: (a) `--seed={{input.seed}}` with input `seed` typed integer binds `--seed=7`; (b) a bound value `7;rm` for that input is refused before the command runs; (c) `{{item.id}}` in a branch binds the element's id; (d) a composed argument whose literal prefix or suffix matches no allowlist shape is refused; (e) a placeholder as the executable fails validation.

**FAE-WF-036 (MUST)** Test-shape declarations MUST support at least these forms: the unit-test form (one named test, exact match); the integration-target form (a named test target plus one named test, exact match); a dedicated test script inside the worktree; and, for non-Rust or non-Go stacks, a declared runner plus a test identifier. The expected-failure check for a RED MUST tolerate formatting differences between source and output (for example interpolated messages and line continuations) by matching a declared stable fragment of the failure message against the output only, plus the test identifier against the source.
- *Rationale:* rigid shape checks rejected valid integration-test REDs and non-Rust stacks, burning attempts.
- *Acceptance:* a table test accepts each form and rejects a form with more than one matched test or zero matched tests.

> Proposed 2026-11-05 (mosaic model-building). Source: mosaic `requirements/fae-model-building-assessment-controller.md`, "Gate commands", FAE-CTL-105; re-checked in `requirements/fae-recheck-2026-11-05.md`.
>
> **FAE-WF-095 (MUST)** A HostCommand MAY declare a **check command**: `check: {format: json | lines, output: <artifact type>, count_field: <field>, writes: [globs]}`. A check command is not a test run. The broker MUST run it under the same allowlist (FAE-WF-035), environment policy, timeout and heavy-op rules as a test run, and MUST parse its standard output, as one JSON document or as `key: value` lines, into the declared artifact type, validated against its schema. A parse or schema failure MUST be a failed run, never a pass. Exit code, timed-out and truncated MUST be recorded beside the parsed fields. Gates read the parsed fields like any artifact; FAE-WF-062 applies through `count_field`, on which the validator MUST require a positive lower bound. The broker's refusal of a command that changes the source tree MUST exempt only paths matching `writes`; those paths MUST lie outside the task's declared files and MUST NOT be committed. Refusing every non-test command other than the formatter check MUST end for declared check commands.
> - *Rationale:* a model-building gate is a deterministic checker whose verdict is a set of numbers (coverage, ratchet values, an accept state) and which writes its own staging directory; exit code and test counts lose that, and a fake test-suite wrapper hides it.
> - *Acceptance:* tests: (a) a check command printing `{"failures": 0, "checked": 412, "accepted": true}` yields an artifact a gate reads; (b) a gate over it with no bound on `checked` fails validation; (c) malformed output fails the run; (d) a write under a `writes` glob is allowed and a write elsewhere is refused as today; (e) a `writes` glob that overlaps declared files fails validation.

> Proposed 2026-11-06 (mosaic code-to-model audit), amendment to FAE-WF-095: (1) a HostCommand MAY declare `capture: [stdout, stderr]`; its artifact then also carries those streams as text fields bounded by a target setting (target setting `capture_bytes`, default 256 KiB each; Appendix A gains the row "HostCommand | captured stream bound | 256 KiB per stream"), with `truncated` set when cut and redacted under FAE-SEC-003, so that a later stage or another role can bind a checker's findings. (2) A check command whose output parses MUST record a non-zero exit code as a field of the run, never as a run failure; only a launch failure, a timeout, or a parse or schema failure fails the run, and gates decide on the exit code. (3) Paths a check command writes under `writes` MUST stay in the task worktree after the run as scratch (FAE-WF-111, whose overlap and exclusion rules then apply to those globs), readable by later stages of the same task and never committed, instead of being restored. (4) The broker's tree comparison MUST treat ignored and unignored files the same way the agentic turn check (FAE-RT-015) does. (5) The rule that `writes` lie outside the declared files does not apply to generator commands (FAE-WF-116). Source: mosaic `docs/DESIGN.md:1774-1777` (catalog D-35), `internal/compose/stages.go:202-215` (C-53); catalog D-17, D-26, D-58, P-43, P-46, C-52; at the 08:25 UTC audit the artifact carried no stderr, check-command writes were restored after the run, unparseable output failed the run, and the broker's tree check skipped ignored files while the turn check counted them (cited then as `workflow/command.go:739-760`, `effects/broker.go:476-508`, `:509`, `:843-870`, `effects/wave19_wf_test.go:40-64`, `provider/agentic.go:773`); fae's working tree at 09:58 UTC implements (1) to (4) (`effects/broker.go:580-602` with redaction at `daemon/live.go:935-943`, `engine/engine.go:1445-1462`, `effects/broker.go:550-578`, `:960`); audit in mosaic `requirements/fae-code-to-model-audit-2026-11-06.md` (rows gates G1, G9, G33).
> - *Acceptance:* tests: (a) a check command with `capture: [stdout]` that prints 300 KiB records 256 KiB and `truncated: true`, and a later stage of another role binds it; (b) a check that prints valid JSON and exits 1 records `exit_code: 1` in a completed run, and a gate on `exit_code == 0` fails; (c) a file written under `writes` is readable by the next stage and is in no commit; (d) an ignored file created outside `writes` is refused by the broker as an unignored one is.

> Proposed 2026-11-05 (mosaic model-building). Source: mosaic `requirements/fae-model-building-assessment-execution.md` §1.1 and §3.2 (updates to FAE-WF-097 and FAE-WF-098 of `requirements/fae-hierarchical-workflows.md`, numbered FAE-WF-095 and FAE-WF-096 there before the 2026-11-05 renumbering), and `fae-model-building-assessment-controller.md` FAE-CTL-104; re-checked in `requirements/fae-recheck-2026-11-05.md`.
>
> **FAE-WF-096 (MUST)** A FanOut MUST support three extensions, each validated as a graph property:
> 1. **Width at run time:** `branches_from` binds a list-typed artifact field; one branch instance runs per element, between a declared minimum and maximum (maximum at most 256). An empty list takes the declared empty route. Each element is bound into its branch as typed data (FAE-WF-043).
> 2. **Concurrency:** with `parallel: true`, branch instances MUST be dispatched concurrently within role caps, host admission and FAE-SCH-015, instead of one after another. Sibling isolation and the join rules stay as they are.
> 3. **Change producers:** a branch of a `parallel` FanOut MAY hold change producers when each branch's write scope is parameterised by its element and the validator proves the scopes disjoint. Each branch instance then edits in its own worktree forked at the visit's tree and commits to its own ref; the join integrates the branch refs in branch order, and per-path floor checks run per branch.
> - *Rationale:* a model-building round fans out to one writing agent per area, with the area count decided by an earlier inventory stage; today branches are 2 to 16, listed in the template, run one after another, and may not produce changes.
> - *Acceptance:* tests: (a) a FanOut over a three-element list runs three branches and over an empty list takes the empty route; (b) two `parallel` branches with 10 s scripted turns finish in about 10 s, not 20 s; (c) two parallel branches each edit their own file and the join lands both, in branch order; (d) overlapping branch write scopes fail validation.

> Proposed 2026-11-06 (mosaic code-to-model audit), amendment to FAE-WF-096 (selection and width): (1) `branches_from` MAY select its list by artifact type as well as by producing stage, taking the latest artifact of that type, so that a list written by a failure-route handler (FAE-WF-112) or bound as a task input (FAE-WF-101) is found. (2) A FanOut MAY declare a `failure` route. A list outside `[min, max]`, a missing source, or a field that is not an array MUST take it with reason `width` (a projection that reads a missing field inside an element, an `object` or a `where` takes `binding` instead, FAE-WF-115) before any branch is dispatched, without truncating the list, and MUST NOT fault or quarantine the task; without the route the task escalates with that reason. Source: mosaic `requirements/fae-hierarchical-workflows.md` FAE-WF-097 (failure route on width), the part FAE-WF-096 did not carry; catalog P-28, P-29, C-57, C-65; before fae's 2026-11-06 implementation pass the list was found by producing stage only, a breach was a `Prepare` error that quarantined the task, and a FanOut could not have a failure edge (audit rows W10, W12; line numbers of that tree); the pass added selection by `type`, `input` and `projection` (`workflow/fan.go:20-29`, `engine/fanout.go:400-441`), a FanOut `failure` edge (`workflow/fan.go:305-307`) and the `width` route (`engine/engine.go:971-979`), but without a route a breach is still a `Prepare` error that the daemon quarantines (`engine/engine.go:973-974`, `daemon/drive_isolation.go:54-108`); audit in mosaic `requirements/fae-code-to-model-audit-2026-11-06.md` (rows workflow W10, W12).
> - *Acceptance:* tests: (a) a list of the declared type produced by a handler stage expands; (b) 9 elements with `max: 8` take the failure route with `width`, and nothing is dispatched; (c) a missing source takes the same route, and the task is not quarantined; (d) without a `failure` route the same breach escalates the task with a reason naming `width`, and the daemon does not quarantine it; (e) a `branches_from` whose `where` reads a field missing from an element takes the route with reason `binding`.

> Proposed 2026-11-06 (mosaic code-to-model audit), amendment to FAE-WF-096 (join): (1) **Join artifact.** A FanIn MUST record one join artifact, of a list type it declares as `output`: one element per branch in branch-index order, whatever order the branches finished in, holding the branch index, its item, its status (passed, failed, cancelled or skipped) and, for a passed branch, its declared output artifact (`branch.output`, or the only artifact type its region produces; the validator MUST refuse a region that produces several types without `branch.output`). Post-join stages, gates, Branch arms and later `branches_from` MUST be able to bind it like any artifact. A `min: 0` FanOut that takes its empty route MUST record the same artifact with an empty list. (2) **Settle.** A FanIn MAY declare `rule: settle`: every branch then runs to its own end, a failed branch never skips or cancels a sibling, and the verdict is computed only when no branch is pending or running: it passes when at least the declared quorum passed (default all), and the passed branches' refs integrate even when others failed. (3) **Early decision.** Under `all`, `any` and k-of-n, when the verdict is decided while parallel branches are in flight, the engine MUST cancel their sessions, process trees included, record one `branch-cancelled` event for each, and not integrate their refs. (4) A failed join's event MUST name the first failed branch and its error, and the join artifact's element for every failed branch MUST carry its error. A branch that reaches the join through a failure route MUST be recorded `failed` at every nesting level and for SubWorkflow branches (FAE-WF-099, FAE-WF-113). Source: mosaic `requirements/fae-hierarchical-workflows.md` FAE-WF-098 (join artifact in branch order, cancellation), the parts FAE-WF-096 did not carry; `docs/DESIGN.md:2076-2079`, `:2161` (catalog D-31); `.claude/skills/mosaic-generate/compose/ontology-pipeline.yaml:309`, `:404`, `:533`, `:578` (P-32); `internal/compose/stages.go:488-507` (C-60); catalog D-07, D-54, P-33, P-93, C-59, C-61, C-62; before fae's 2026-11-06 implementation pass a FanIn recorded a verdict event only and a post-join stage bound the latest dispatch per stage (a scripted probe's post-join turn received one of three branch outputs), and a decided join marked running branches skipped without cancelling their sessions (audit rows W14, W15; line numbers of that tree); the pass added the join artifact (`engine/fan_join.go:12-113`, a list output required on new writes by `workflow/fan_write.go:7-27`), `settle` (`workflow/fan.go:172-186`) and cancellation (`engine/fanout.go:601-617`, `daemon/drive.go:922-935`); audit in mosaic `requirements/fae-code-to-model-audit-2026-11-06.md` (rows workflow W14, W15, W16; gates G29, G30).
> - *Acceptance:* tests: (a) three branches that finish in reverse order yield a join artifact of three outputs in branch order, and a post-join AgentTurn binds all three; (b) the empty route records `[]`; (c) a region producing two types without `branch.output` fails validation; (d) with `settle` and 4 parallel branches where branch 2 fails at once, branches 1, 3 and 4 run to their ends and integrate; (e) an `any` join over three parallel 10 s branches decided by the first cancels the other two sessions and records two `branch-cancelled` events; (f) a failed `all` join names the failing branch and its error; (g) in a FanOut whose branches are SubWorkflows, a branch whose child fails and whose failure route leads straight to the join is recorded `failed` and the `all` join fails; (h) a passed SubWorkflow branch's element carries the SubWorkflow's output artifact.

> Proposed 2026-11-07 (mosaic code-to-model re-audit), amendment to FAE-WF-096 (join): in (4), "a branch that reaches the join through a failure route" includes a turn-budget exhaustion route (FAE-RT-015 `on_exhaustion: <join>`), and any other route that records a `failure` artifact (FAE-WF-112). Such a branch MUST be recorded `failed`, with the failure's error, at every nesting level. Acceptance gains: (i) a branch stage whose role or stage `turn_budget.on_exhaustion` is the join, scripted to exhaust, is recorded `failed`, and the `all` join fails, at the top level and inside an outer branch. Source: probe `reaudit/hier/p/h3_test.go` (today recorded `passed` and the task reaches Done; `engine/nested_fan.go:426` and `engine/fanout.go:273` test only `stage.failure`, while the budget route goes through `engine/engine.go:1588`).

> Proposed 2026-11-06 (mosaic code-to-model audit). Source: mosaic `requirements/fae-hierarchical-workflows.md` FAE-WF-099 and its update in `fae-model-building-assessment-execution.md` §3.2 (carried); catalog C-63, P-56; audit in mosaic `requirements/fae-code-to-model-audit-2026-11-06.md` (rows workflow W16, W18).
>
> **FAE-WF-099 (MUST)** A FanOut branch region MAY contain SubWorkflow stages, so that one stage visit starts N child tasks. Each branch instance's child MUST get the task id `<parent>.<stage>.<visit>.<branch-index>` and a parent link naming its branch. The children of a `parallel` FanOut MUST run concurrently, each as its own task, under the same role caps, provider pools (FAE-SCH-015) and tree session limit (FAE-SCH-025) as their parent. A child's mapped output MUST become its branch's output, and a child that ends without Done MUST take that SubWorkflow's failure route inside its own branch only. The validator's refusal of a SubWorkflow in a branch region MUST end. SubWorkflow's implicit multi-child `items` form MUST NOT admit a child for an empty list.
> - *Rationale:* mosaic zooms into N components of a model in one visit, N decided by the model it has just built. Before fae's 2026-11-06 implementation pass a branch region refused SubWorkflow and the fixed 2-to-16 `items` form admitted one child for an empty list, so "zoom into none" could not be expressed (audit rows W16, W18; line numbers of that tree). The pass lifted the refusal (`workflow/fan.go:376-379`), runs branch children from nested frames (`engine/nested_fan.go:486-573`, `daemon/branch_subworkflow.go:13-99`) with ids built by `engine/subworkflow.go:57-77`, and admits no child for an empty list (`engine/subworkflow.go:100-107`).
> - *Acceptance:* tests: (a) a `branches_from` FanOut of 3 elements whose branch is one SubWorkflow admits 3 children with distinct ids and parent links, runs them concurrently, and joins their outputs in branch order; (b) one child ending Retired routes only its branch through the failure route, the branch is recorded `failed` also when that route leads straight to the join, and the join rule decides; (c) a SubWorkflow whose `items` input is an empty list admits no child; (d) a template with a SubWorkflow in a branch validates.

> Proposed 2026-11-07 (mosaic code-to-model re-audit). Source: fae `engine/subworkflow.go:145-183`, `:262-309`, `engine/scenario.go:282-292`, `daemon/branch_subworkflow.go:26-34`; mosaic `.claude/skills/mosaic-generate/compose/ontology-pipeline-light.yaml:658-681` (children fan, one child per zoom id).
>
> **FAE-WF-140 (MUST)** A SubWorkflow stage MUST start more than one child per visit only when it declares the list explicitly (`items_from`, a projection (FAE-WF-115), as on a HumanDecision (FAE-CTL-108)). This replaces the implicit multi-child `items` form that FAE-WF-099 mentions. The presence of a field named `items` in a mapped or bound artifact, including every array-typed FanIn join artifact (FAE-WF-096), MUST NOT change how many children start. An `input_map` entry that declares only a projection MUST select only what its projection reads, never every parent artifact. Each child of a multi-child visit MUST be bound with its own signature inputs (FAE-WF-101), with the item available to its projections, never with the admission wrapper (`parent_task`, `parent_stage`, `inputs`, `item`, `item_index`) as its inputs.
> - *Rationale:* a zoom SubWorkflow inside a FanOut branch, with projection-only `input_map` entries, saw the earlier `areas_join` artifact's `items` and started 2 children instead of 1. The rehearsal bound each child's inputs to the wrapper object and refused admission. The daemon path would bind the same inputs to every child.
> - *Acceptance:* tests: (a) a parent with a 2-branch join artifact followed by a SubWorkflow whose `input_map` has only projections starts exactly 1 child, in rehearsal and in the daemon; (b) a SubWorkflow with `items_from` over a 3-element list starts 3 children, each bound to its own element through `item` projections, and each child's `input:<name>` evidence equals its projection; (c) a mapped artifact with an `items` field and no `items_from` starts 1 child.

> Proposed 2026-11-07 (mosaic code-to-model third audit). Source: fae `workflow/GRAMMAR.md` SubWorkflow ("multiple children use `<parent>.<stage>.<visit>.<idx>`"); third-audit outputs `reaudit2/e2e/out/sim-simulate-light.json` (`sim.children.1`) and `out/sim-light-2zoom.json` (`sim.children.1.0`, `sim.children.1.1`).
>
> **FAE-WF-145 (SHOULD)** A SubWorkflow stage that declares `items_from` SHOULD name every child `<parent>.<stage>.<visit>.<idx>`, including a list of one element. Child task ids, change-request ids and decision ids of an explicit list MUST NOT depend on the list's length.
> - *Rationale:* a zoom over one component names its child `sim.children.1`. Adding a second zoom id renames it `sim.children.1.0`. Scenario globs, change-request ids and operator scripts then break.
> - *Acceptance:* tests: (a) `items_from` over 1 element starts `<p>.<stage>.1.0`; (b) over 2 elements, `<p>.<stage>.1.0` and `.1`, and the first child's change-request id is the same in both runs.

> Proposed 2026-11-06 (mosaic code-to-model audit). Source: mosaic `requirements/fae-hierarchical-workflows.md` FAE-WF-100 (carried, corrected for pinned revisions); catalog D-38, D-40, D-48, P-57, C-70, C-71, C-72; audit in mosaic `requirements/fae-code-to-model-audit-2026-11-06.md` (rows workflow W20, W21; control 20).
>
> **FAE-WF-100 (MUST)** When a workflow document declares `recursion: {max_depth, max_descendants, max_children_per_visit}`, a SubWorkflow in it MAY select `workflow: self`, which binds the child to the parent task's own pinned workflow id and revision, and the workflow MAY be admitted as a detached child (FAE-WF-102) of its own tasks. Depth counts from 1 at the run-tree root, and `max_depth` MUST NOT exceed an instance ceiling (default 8, configurable; Appendix A gains the row "recursion depth ceiling: 8"). Before admitting any child, SubWorkflow or detached, the engine MUST refuse it with reason `recursion_budget` when its depth would exceed the parent's or the child's `max_depth` or the ceiling, when the run tree's non-root tasks, detached ones included, would exceed the root document's `max_descendants`, or when the children started by one stage visit would exceed `max_children_per_visit`, counting together every child that the branches of one FanOut visit start (FAE-WF-099). A refusal MUST fail that visit through its failure route (FAE-WF-112), never the parent's admission; a detached admission is refused to its caller with the same reason. For documents with `recursion` these bounds replace the fixed nesting limit of 4; documents without it keep the earlier refusals of self-selection and of any child deeper than depth 5 (four SubWorkflow levels below the root). Mutual recursion MUST be refused unless every document in the cycle declares `recursion`.
> - *Rationale:* zooming into a component runs the same pipeline at a smaller scope, and its components can be zoomed in turn, down to the machine level. A document cannot name its own revision before it is written (FAE-WF-002), so `self` must resolve at run time. Before fae's 2026-11-06 implementation pass, nesting was hard-coded to 4 and refused at the parent's admission, and self-selection was refused (audit row W20); documents without `recursion` still are (`daemon/subworkflow.go:19-20`, `:39-40`, `workflow/subworkflow.go:28-35`).
> - *Acceptance:* tests: (a) with `max_depth: 3` a child and a grandchild are admitted, and a great-grandchild's visit takes its failure route with `recursion_budget`; (b) `max_descendants: 5` refuses the sixth descendant anywhere in the tree, counting a detached child; (c) `max_children_per_visit: 2` refuses the third child of a 3-branch visit; (d) a document without `recursion` that selects `self` fails validation; (e) `max_depth: 9` fails validation against the default ceiling.

> Proposed 2026-11-06 (mosaic code-to-model audit). Source: mosaic `requirements/fae-hierarchical-workflows.md` FAE-WF-102 (carried); catalog D-43, D-44, D-67, P-61, C-76, C-77; audit in mosaic `requirements/fae-code-to-model-audit-2026-11-06.md` (rows workflow W24; control 2; gates G32).
>
> **FAE-WF-102 (MUST)** A controller or the operator MUST be able to admit a **detached child**: a task of any installed workflow whose admission (FAE-API-034) names `parent_task`, an existing task in any state, Done included. The admission MAY give `from_parent: {<input>: <parent artifact or input name>}`, which binds the parent's recorded artifacts and inputs to the child's signature inputs (FAE-WF-101), with provenance naming the parent's artifact ids; naming one input both directly and through `from_parent` MUST be refused. The child gets the id `<parent>.detached.<n>` (the first free n); an id given at admission MUST start with `<parent>.detached.` (FAE-WF-106), depth parent + 1 and the parent's tree root, and it counts toward the tree's recursion budget (FAE-WF-100). The link MUST be recorded at admission and appear in the run tree (FAE-API-035), in events (FAE-API-036) and in both tasks' histories. Admitting or running the child MUST NOT change the parent's state or artifacts, and the child's file scope MUST NOT default to the parent's code-change scope. The child MUST start from the parent's last recorded tree (as FAE-WF-117 does for SubWorkflow children), so that paths named in the parent's artifacts resolve. A finished parent's inputs and artifacts MUST stay readable for this for as long as its records are kept.
> - *Rationale:* an operator zooms into a component after the parent run has finished, often days later, and a later round seeds itself from an earlier one; the parent cannot wait in a SubWorkflow stage for that. FollowUp (FAE-OPS-034) re-runs the same revision with the predecessor's context, which is a different need, and before fae's 2026-11-06 implementation pass admission had no parent (audit row W24); `parent_task` and `from_parent` are now `daemon/rpc.go:23-24`.
> - *Acceptance:* tests: (a) a detached child of a Done task, of a different workflow, binds the parent's `contract` artifact and `scope` input through `from_parent`, and the tree read shows depth 2 and the link; (b) the parent's checkpoint, state and artifacts are byte-identical before and after the child runs, and its history differs only by the appended child-link event; (c) an unknown parent, or an unknown parent artifact, refuses admission naming it; (d) a detached child past the root's `max_depth` or `max_descendants` is refused with `recursion_budget`.

> Proposed 2026-11-06 (mosaic code-to-model audit). Source: mosaic `requirements/fae-hierarchical-workflows.md` FAE-WF-103 (carried, as a stage primitive); catalog D-42, D-59, P-63, P-64, P-65, C-79, C-80, C-81; audit in mosaic `requirements/fae-code-to-model-audit-2026-11-06.md` (rows workflow W25; control 11).
>
> **FAE-WF-103 (MUST)** A **ChangeRequest** primitive MUST record the value of a bound artifact, or of a field of it, as a change request addressed to `target` (`parent`, or an ancestor's task id) about a named artifact of the target. The engine, not the role, records it, with id `<task>.<stage>.<visit>[.<branch>]`, state `pending`, the sender and the artifact it is about. A target that is not an ancestor, or that has no recorded artifact of the named type, MUST fail the stage through its failure route with reason `change_request`. Recording MUST append an event to both tasks' histories, whatever the target's state, Done included, and MUST NOT change the target's state, artifacts or briefs; a request MUST NOT be delivered into any agent turn of the target. A task MUST be able to forward requests addressed to it to its own parent with a ChangeRequest stage that binds them, and the forwarding task is recorded. A read MUST list the requests addressed to a task (FAE-API-035), and an audited verb MUST let the operator, or a controller citing an authorizing message, mark one `accepted` or `declined` with a reason; nothing in Fae acts on a request otherwise. Because the engine records it from an artifact, this does not widen what workflow roles may do (FAE-OPS-001).
> - *Rationale:* a child that finds its parent's contract wrong must say so, and the parent's owners decide; silent divergence between levels is what mosaic is built to prevent. FAE-OPS-033 messages go into the target's next turn and refuse finished targets (`engine/control.go:584-586`), so they cannot carry this.
> - *Acceptance:* tests: (a) a child's request against its Done parent's `contract` is listed on the parent, both histories show it, and the parent's records are unchanged; (b) a request addressed to a sibling fails the stage with `change_request`; (c) a forwarded request reaches the grandparent with the forwarding task recorded; (d) marking a request declined is audited and shown in the listing; (e) no brief of the target contains the request text.

> Proposed 2026-11-07 (mosaic code-to-model re-audit), amendment to FAE-WF-103: replace "or that has no recorded artifact of the named type" with "or that does not declare the named artifact (an artifact type, a signature output name or an input name, FAE-WF-101)". A declared artifact that the target has not recorded yet, because it is waiting at the SubWorkflow visit that started the sender, MUST be accepted. The request records the name it was addressed to. Rationale: in mosaic's zoom the pipeline child's request about its parent's `ontology` reaches a wrapper that records `ontology` only when that child finishes. With the current wording it is refused (`daemon/change_requests.go:160-169`), so one pipeline template cannot file requests under both a `self` parent and the zoom wrapper. Source: mosaic `ontology-pipeline.yaml:799-806`, `zoom.yaml:99-107`.

> Proposed 2026-11-06 (mosaic code-to-model audit). Source: mosaic `.claude/skills/mosaic-generate/compose/ontology-pipeline.yaml:154-156`, `:562-563` and `compose/README.md:90` (catalog P-36); catalog P-31, P-35, P-37, P-39, P-94, C-13, C-14, C-15, C-16, C-17, C-42, D-57, D-72; audit in mosaic `requirements/fae-code-to-model-audit-2026-11-06.md` (rows workflow W07, W08; gates G14; agent BT-03).
>
> **FAE-WF-112 (MUST)** Every AgentTurn, Review, HostCommand, SubWorkflow and ChangeRequest (FAE-WF-103) stage, and a FanOut under the 2026-11-06 amendment to FAE-WF-096, MAY declare a `failure` route; a Gate's failure edge (FAE-WF-030) is a failure route for this requirement. A stage that fails after its bounded retries (output still malformed after FAE-WF-034's bound, turn budget exhausted, output refused, command error or timeout, child ended without Done, child admission refused) MUST then take that route instead of escalating. Whenever a stage takes a failure route or a turn-budget route (FAE-RT-015), the engine MUST record in the same transaction a `failure` artifact `{stage, visit, attempt, reason, error}`, where `reason` is one of `schema`, `budget`, `refused`, `command`, `gate`, `child_failed`, `admission`, `recursion_budget`, `width`, `binding` or `change_request`, `error` is redacted (FAE-SEC-003), and provenance names the failed dispatch. Only stages entered through that route MAY bind it, and the validator MUST refuse any other binding. A handler MAY produce the failed stage's artifact type, and downstream stages then bind it as if the primary had produced it. A handler that fails follows its own route or escalates. Taking a failure route counts against the per-task attempt limit (FAE-WF-074). A stage without a failure route escalates as today.
> - *Rationale:* mosaic routes each failed turn once: builds and coordinator turns to a retry that resumes from the files the first attempt left, judges and oracle agents to a gap turn that records what is unmeasured. Different stages of one role need different handlers, and each handler must know why the first attempt failed. At the 08:25 UTC audit the only AgentTurn route was the role-level turn-budget route, and a failure was an event, not an artifact (cited then as `workflow/agentic.go:38-76`, `engine/engine.go:1386-1421`, `:1979`); fae's working tree at 09:58 UTC records the `failure` artifact (`engine/failure.go:26-68`) but routes a failed child with reason `SubWorkflowFailed` (`engine/subworkflow.go:605`) instead of `child_failed`.
> - *Acceptance:* tests: (a) an AgentTurn whose output stays invalid past the retry bound takes its failure route, and the handler binds `failure.reason == "schema"`; (b) two stages of one role that exhaust their budgets route to two different handlers, each seeing `budget`; (c) a failed gate's handler binds a `failure` with reason `gate`; (d) a stage after the handler binds the handler's artifact of the primary's type; (e) a stage outside the route that binds `failure` fails validation; (f) a stage without a failure route escalates as today.

> Proposed 2026-11-06 (mosaic code-to-model audit). Source: mosaic catalog P-24, D-52, C-08; audit in mosaic `requirements/fae-code-to-model-audit-2026-11-06.md` (rows workflow W06; control 19).
>
> **FAE-WF-113 (MUST)** A FanOut branch region MAY contain a complete FanOut/FanIn pair, nested at least 3 deep, and a static FanOut's branches MAY be different sub-graphs, so that independent stages of one task, fan-outs and SubWorkflows included, run at the same time. The engine MUST keep fan state per nesting level, MUST allow one dispatch in flight per running branch at every level, and MUST apply sibling isolation (FAE-WF-096(2); FAE-WF-033 for reviewers), the join rules and FAE-WF-096's write-scope disjointness per level. The kernel-floor proof (FAE-WF-070) MUST cover nested regions.
> - *Rationale:* mosaic's pipeline writes the benchmark beside the area fan-out and starts about six stages, four of them fan-outs, after its merge gate. Run one after another, a round takes hours longer. Mosaic compiles its data-flow templates into Fae graphs, so independent stages must become parallel branches; before fae's 2026-11-06 implementation pass a branch could hold no fan-out or SubWorkflow and a task had one fan state (audit row W06; line numbers of that tree); the pass added per-branch frames (`engine/fanout.go:50-66`, `engine/nested_fan.go:149-292`) and lifted the refusal (`workflow/fan.go:376-379`).
> - *Acceptance:* tests: (a) a `parallel` static FanOut whose branch A is one 10 s turn and whose branch B is a `parallel` dynamic FanOut of three 10 s turns finishes in about 10 s; (b) an inner join failure takes the inner failure route inside the outer branch; (c) a stage in an inner branch cannot bind or read a sibling at either level; (d) the validator refuses a nested region whose change producers lack disjoint write scopes; (e) the FAE-WF-070 property test generates workflows with FanOuts and SubWorkflows nested in branches at least 3 deep and still rejects every floor weakening; (f) an inner branch that reaches its join through a failure route is recorded `failed` at its level, and the enclosing joins count it under their own rules.

> Proposed 2026-11-07 (mosaic code-to-model third audit), amendment to FAE-WF-113: acceptance gains (g): the nested `parallel` dynamic FanOut of three 10 s turns from (a), placed inside a branch of a **sequential** outer FanOut, finishes its three turns in about 10 s on the daemon path, and so does the scripted rehearsal. Source: native probe `reaudit2/hier/overlay/zz3_hier_probe_test.go` TestZZ3NestedParallelConcurrency. Today the inner turns run one after another (peak 1), because the daemon launches extra branch dispatches only when the outermost fan is parallel (`daemon/drive_stages.go:175`, `daemon/drive.go:675`).

> Proposed 2026-11-06 (mosaic code-to-model audit). Source: mosaic catalog P-37, P-93; audit in mosaic `requirements/fae-code-to-model-audit-2026-11-06.md` (row workflow W22).
>
> **FAE-WF-114 (MUST)** A SubWorkflow child that enters Escalated MUST end its parent's visit as a child failure (the failure route, else the parent escalates) without waiting for an operator to retire the child. The child stays Escalated for the operator. A SubWorkflow MAY declare `on_child_escalation: wait` to keep the parent waiting instead.
> - *Rationale:* a mosaic tree runs unattended for hours; one failed turn deep in a zoom must fail that branch so that its siblings and the root can finish, and the operator decides about the failed child afterwards. Before fae's 2026-11-06 implementation pass, Escalated was not terminal and the parent kept waiting (audit row W22); the escalated-child route is now `engine/subworkflow.go:384-394`, `daemon/subworkflow.go:204-224`, `:305-311` and `daemon/branch_subworkflow.go:50-69`.
> - *Acceptance:* tests: (a) a child that escalates on an unrouted failure makes its parent take the failure route in the same drive round, and the child is still Escalated, not retired as an obsolete child, after two further drive rounds; (b) with `on_child_escalation: wait`, the parent stays parked until the child is retired.

> Proposed 2026-11-06 (mosaic code-to-model audit). Source: mosaic catalog P-28, P-53, C-65; audit in mosaic `requirements/fae-code-to-model-audit-2026-11-06.md` (row workflow W11).
>
> **FAE-WF-115 (SHOULD)** SubWorkflow `input_map` entries and `branches_from` SHOULD accept a projection: a field path into a selected artifact, an input (FAE-WF-101), `item` or `tree` (FAE-WF-106); an object built from named projections; and, for a list, a `where` filter in the gate language over each element, whose lookups MAY select any evidence the stage can bind (inputs, `tree`, artifacts) besides `item`. A projection MUST be evaluated within the gate language's bounds (FAE-WF-060) and recorded with the evidence it read, and a projection that reads a missing field MUST fail the stage with reason `binding` (FAE-WF-112), except that a `branches_from` whose selected list is missing or not an array takes `width` (2026-11-06 amendment to FAE-WF-096).
> - *Rationale:* mosaic zooms only into the components the caller asked for, and builds each child's scope from the item (`{repo, component: item.id, paths: item.code_paths}`). A check command per computed value works for lists but cannot build per-branch child inputs; before fae's 2026-11-06 implementation pass `branches_from` took a stage and field only and `input_map` selected whole artifacts (audit row W11); projections are now `workflow/projection.go:12-60` and `engine/failure.go:115-226`.
> - *Acceptance:* tests: (a) `branches_from` over a model's components `where` the id is in an input list expands only those; (b) an `input_map` entry builds an object from an input field and two item fields; (c) a projection over a missing field, in an `input_map` entry and inside a `branches_from` object or `where`, fails the stage with `failure.reason == "binding"`; (d) an `input_map` projection to a child without a signature binds the projected value, not the latest artifact.

> Proposed 2026-11-07 (mosaic code-to-model re-audit). Source: mosaic `.claude/skills/mosaic-generate/compose/zoom.yaml:76-87`, `ontology-pipeline.yaml:912-918`; fae `workflow/projection.go:12-20`, `engine/failure.go:116-245`; mosaic `ontology-pipeline-light.yaml:672-678`. Merges the e2e re-audit's first/default/literal proposal.
>
> **FAE-WF-122 (SHOULD)** A projection (FAE-WF-115) SHOULD also accept these steps, evaluated within the same bounds and recorded with the evidence they read:
> 1. `index: N` (0-based; negative counts from the end), applied to the list a `where` produced or a list field selected, followed by `then: <field path>` into the selected element;
> 2. `default: <JSON value>`, used instead of failing with `binding` when the selected field, element or source is missing; the default is recorded as such;
> 3. `lookup: {table: {<key>: <JSON value>, ...}, key: <projection>}`, which selects a value from a literal table by a projected string key and fails with `binding` when the key is absent and no `default` is given.
> 4. `literal: <JSON value>`, usable as a member of `object` (FAE-WF-115), binding that value as is.
> - *Rationale:* a zoom child's scope and contract are fields of the one component, in the parent's model, that the caller named, and its level is the component's `next_level` or the level below the parent's, taken from a fixed table. With only field paths, objects and `where`, the selected component stays a one-element list, an optional field fails the stage, and the level table cannot be written. Each workflow would need an extra command stage or an agent turn just to compute inputs.
> - *Acceptance:* tests: (a) `{input: parent, field: components, where: item.id == input:component, index: 0, then: code_paths}` binds the named component's code paths; (b) `{source: item, field: scenarios, default: []}` binds `[]` for an element without `scenarios` and records that the default was used; (c) `lookup` maps `service` to `logic` through a table keyed by an input; (d) an `object` member `{literal: []}` binds an empty list; (e) `index` past the end of the list without `default` fails with `failure.reason == "binding"`.

> Proposed 2026-11-07 (mosaic code-to-model third audit), amendment to FAE-WF-122: `default` applies only when the selected source, field, list element or lookup key is missing. A `where` filter that evaluates to unknown, or a type mismatch on a step, MUST still fail with `binding`. The validator MUST refuse a projection that names an undeclared input, stage or artifact type, or (when the source schema is known) a `field`/`then` path outside it. A default used by a ChangeRequest `value` MUST be recorded like any other. Acceptance: (f) a `where` reading a field that one element lacks, with a `default`, fails with `binding`; (g) `{input: nope, default: "x"}` is refused at validation. Source: third-audit probes L1 f, L1 i, L1c (`engine/failure.go:142` defaults on any error containing "missing"; `engine/change_request.go:39`).

> Proposed 2026-11-07 (mosaic code-to-model third audit). Source: fae `engine/failure.go:140-150` (`default` is a literal, applied only on a `missing` error), `workflow/catalog.go:289-330` (flow-insensitive projection typing; `lookup` loses its enum), `workflow/catalog.go:76-104`; mosaic `.claude/skills/mosaic-generate/compose/zoom.yaml:88-90` (`level != "" ? level : (has(c.next_level) ? c.next_level : below[parent_level])`), `ontology-pipeline-light.yaml:678`; third-audit outputs `reaudit2/e2e/out/validate-zoom-level-override.json`.
>
> **FAE-WF-144 (SHOULD)** A projection (FAE-WF-115, FAE-WF-122) SHOULD accept `coalesce: [<projection>, ...]`, whose value is the first member that resolves to a present, non-null value, failing with `binding` when none does, and `cases: [{when: <gate expression>, value: <projection>}, ...]` with an optional `else: <projection>`, whose value is the first case whose positive-evidence `when` passes. Both forms keep the projection bounds and record the evidence each member read and the member chosen. Static child-binding validation MUST type `coalesce` as the union of its members' schemas without null, `cases` as the union of its arms, and `lookup` as the union of its table values' literal schemas (enums kept).
> - *Rationale:* mosaic picks a child's level from an explicit override, else the component's stated level, else the level below the parent's. Without a fallback form the producer must always state `next_level`, and the zoom's override needs a Branch over SubWorkflow stages. fae's flow-insensitive check now refuses that Branch arm, because the override's `""` sentinel is in its schema, so the override had to be dropped.
> - *Acceptance:* tests: (a) `{coalesce: [{source: item, field: next_level}, {lookup: {table: {system: service, service: logic}, key: {input: parent_level}}}]}` binds the item's level when present and the looked-up level when not; (b) `cases` with `when: input.level != ""` binds `input.level`, else the item's level, and validates against a child enum when every arm's schema satisfies it; (c) a 2-entry `lookup` of enum strings satisfies a child enum containing both.

> Proposed 2026-11-06 (mosaic code-to-model audit). Source: mosaic `docs/DESIGN.md:908-909`, `:1022-1025`, `:2091-2093`, `:2205-2209` (catalog D-17, D-29, D-33) and `.claude/skills/mosaic-generate/compose/ontology-pipeline.yaml:435-456` (P-46); catalog D-14; audit in mosaic `requirements/fae-code-to-model-audit-2026-11-06.md` (row gates G9, which found FAE-WF-095's `writes` rule in CONFLICT with this need).
>
> **FAE-WF-116 (MUST)** A HostCommand MAY declare `generate: {writes: [globs]}`. A generator command is a change producer: its changes to paths matching `writes`, which MUST lie inside the stage's effective write scope (FAE-SEC-011) and MAY overlap the task's declared files, MUST be kept as the task's own work, recorded as the task's new tree, and committed and landed through the same floors as an agent's change (FAE-WF-071, FAE-CTL-052); in a `produces: artifacts` task (FAE-WF-104) its `writes` globs are relative to the output directory, its changes stay there, and nothing is committed. A change outside `writes` MUST be reverted and fail the run, naming the paths. The artifact MUST record `exit_code`, `timed_out`, `changed` (the changed paths) and `changed_count`. A gate over it MAY assert `exit_code == 0` without a count only when the gate is marked emptiness-permitted (FAE-WF-062).
> - *Rationale:* mosaic's merge step writes `ontology.json` and coverage tables, `assign_inventory.py` writes `worlds.yaml`, `regen.py` reruns generators to a fixed point, and the ratchet step tightens the profile's gate thresholds. These are deterministic programs whose output is the deliverable. At the 08:25 UTC audit every HostCommand change was refused or restored (`effects/broker.go:476-508` then), so such work had to be faked as an agent turn; fae's working tree at 09:58 UTC implements `generate` (`workflow/filesystem.go:129-145`, `effects/broker.go:302-312`, `:550-578`).
> - *Acceptance:* tests: (a) a generator writing `ontology.json` under `writes` leaves it in the task tree, and the task's commit contains it; (b) a write outside `writes` is reverted and the run fails naming it; (c) the artifact lists the changed paths with an exact count; (d) a generator whose `writes` falls outside the stage's write scope fails validation; (e) in a `produces: artifacts` task a generator writing `ontology.json` under `writes` leaves it in the output directory and no commit is made.

> Proposed 2026-11-06 (mosaic code-to-model audit). Source: mosaic `.claude/skills/mosaic-generate/compose/README.md:63`, `.claude/skills/mosaic-generate/compose/ontology-pipeline.yaml:914-915`, `docs/PIPELINE.md:787-792` (catalog P-58, P-84), `docs/DESIGN.md:1824-1828` (D-41); audit in mosaic `requirements/fae-code-to-model-audit-2026-11-06.md` (row gates G31).
>
> **FAE-WF-117 (MUST)** A SubWorkflow child task MUST start from the parent's tree as recorded at the SubWorkflow visit, not from the parent's admission tree, so that files the parent produced earlier in its run, and paths named in its mapped artifacts, exist in the child's worktree; for a `produces: artifacts` parent they are read from the parent's output directory by its absolute path (FAE-WF-104). The SubWorkflow stage MAY bind a read policy and read-only files for the child from the parent's artifacts and the branch item, narrowing the child's roles' policies (FAE-SEC-008), and the child's admission MUST record them.
> - *Rationale:* a zoom child models one component of the parent's merged model, reads that model and the parent's test-target list by path, and must see only its component's code. Before fae's 2026-11-06 implementation pass the child was admitted at the parent's admission tree with no read narrowing (audit row G31); it now forks from the parent's visit tree (`daemon/worktree_base.go:134-151`) and takes the stage's literal `reads` (`daemon/subworkflow.go:350-353`), and still takes the parent's file scope (`daemon/subworkflow.go:411-433`).
> - *Acceptance:* tests: (a) a parent writes `ontology.json` in stage 3 and its stage-5 child reads it at the same path; (b) a child bound `reads: {allow: [src/a/**]}` cannot read `src/b/x.go`, and in a FanOut over items `{path: src/a}` and `{path: src/b}` a SubWorkflow bound `reads: {allow: ["{{item.path}}/**"]}` gives each child only its own item's path; (c) the child's admission records the parent's visit tree.

> Proposed 2026-11-06 (mosaic code-to-model audit). Source: mosaic catalog P-89; audit in mosaic `requirements/fae-code-to-model-audit-2026-11-06.md` (row control 12).
>
> **FAE-WF-118 (SHOULD)** An AgentTurn stage MAY declare `operator_may_supply: true`. While such a stage is the task's current stage with no turn in flight, parked or not (or the start stage of a Pending task), the operator, or a controller citing an authorizing message, MAY supply its output artifact through an audited verb. The artifact MUST validate against the stage's output schema, MUST record the operator as producer in provenance, and MUST then be consumed exactly as a role's output, with no turn dispatched for that visit. The validator MUST refuse the flag on a change-producer stage or a stage with a write scope (FAE-SEC-011), since a supplied artifact never changes the tree. Stages without the flag MUST refuse a supplied artifact, and independence rules (FAE-WF-033) MUST treat the operator as a distinct producer.
> - *Rationale:* mosaic's coordinator stage can be done by one agent or by a person; HumanDecision records only an option choice, and Decide takes only `decision`, `option` and `rationale`.
> - *Acceptance:* tests: (a) a supplied artifact for a flagged pending stage advances the task with operator provenance and no turn; (b) an invalid artifact is refused, naming the schema error; (c) a stage without the flag refuses it; (d) the flag on a change-producer stage fails validation.

> Proposed 2026-11-06 (mosaic code-to-model audit). Source: mosaic `requirements/fae-model-building-assessment-controller.md` FAE-CTL-108 (carried with its wording); catalog P-88; audit in mosaic `requirements/fae-code-to-model-audit-2026-11-06.md` (row control 10). It keeps its mosaic ID, which Fae does not use.
>
> **FAE-CTL-108 (MUST)** A HumanDecision MAY declare `items_from: <artifact field path>` (a list) and `item_options` (1 to 8 ids, each with a description), instead of or as well as its routing `options`. `fae decide` MUST then accept one option per item (by item index or item `id`) and a rationale per item, and the decision MUST be refused until every item has an option. The decision artifact MUST record the items, each choice, the authorizing message and the decider. Gates and later briefs read it like any artifact (for example "count of items decided `accept`"); routing still uses the routed `options`, chosen in the same call; a HumanDecision with item options and no routed `options` MUST have exactly one outgoing route, which every complete decision takes.
> - *Rationale:* a round's coordinator decides lists: which components to zoom into, which "the judge is wrong" claims to accept, where each breaking change goes, and which deadlocked seam's owner wins. One option per stage cannot record that, and splitting it into N stages needs a dynamic graph.
> - *Acceptance:* tests: (a) a decision over five items with one missing is refused; (b) a complete decision records every item; (c) a gate counts the accepted items; (d) the simulator accepts scripted per-item decisions.

> Proposed 2026-11-07 (mosaic code-to-model re-audit). Source: mosaic `.claude/skills/mosaic-generate/compose/ontology-pipeline.yaml:304-352`, `:399-433`, `:527-700` (six fan-outs of up to 32 branches, each branch stage with its own `on_fail` handler), `internal/compose/stages.go:346-509` (each branch instance has its own stage states); fae `engine/engine.go:1149` (attempt boundary on `Counts[stage]`), `:1569` (resume marker per stage), `:2164-2186` (schema-retry count and prior rejection per stage), `:2222-2250` (same-gate per stage), `engine/job.go:36-64` and `engine/engine.go:1271-1279` (checkpoints per stage), `:555-569` (dispatch counter check); re-audit probes P4, P4b, P5, P5b, P5c and P12 in mosaic's session scratchpad `reaudit/gates/`.
>
> **FAE-WF-130 (MUST)** Inside a FanOut, every per-stage counter and record the engine keeps MUST be keyed by branch instance (branch path and stage) as well as by stage. That covers visit numbers (dispatch `visit`, the `failure` artifact's `visit`, change-request and child ids), schema-retry counts and `prior_rejection` (FAE-WF-034), turn-budget resume markers (FAE-RT-015), turn checkpoints (FAE-RT-019), same-gate repeat counts, agent-start, stall and failover bounds, and failure-handler aliases (FAE-WF-112). A branch instance MUST never be offered another instance's prior rejection, checkpoint or provider session. A stage's first visit in a branch instance MUST NOT count as a repeated execution: it charges no task attempt (FAE-WF-074), while that instance's retries, re-entries and failure routes still do. A refund of one branch's dispatch (resume-once, checkpoint interruption, launch deferral, quota) MUST NOT invalidate any sibling's in-flight dispatch, and each sibling's result MUST still be accepted.
> - *Rationale:* a mosaic round runs six fan-outs of up to 32 branches, and every branch runs the same template stages with their handlers. Today the branches share the stage's counters. One branch's schema failure uses up the next branch's retry, and its prior rejection reaches a sibling's brief. Four branches that fail a branch gate the same way escalate the whole task as SameGate. Every branch's first turn is charged as an attempt, so the per-task attempt limit measures width, not retries. In a parallel fan-out, a refunded dispatch makes the engine refuse sibling completions, and the daemon cancels every branch turn. A non-fresh role can resume a sibling's provider session.
> - *Acceptance:* tests: (a) a 4-branch fan-out in which each branch's first output fails schema once gives each branch one retry with only its own `prior_rejection`, and its own `visit` 1 then 2; (b) a branch gate failing identically in 5 branches under `settle` records 5 failed branches and no SameGate escalation; (c) a 32-branch fan-out with one schema retry charges at most 2 task attempts beyond the first; (d) in a parallel fan-out, branch 0 refunded by resume-once, by checkpoint interruption and by launch deferral (each case) leaves branch 1's in-flight completion accepted; (e) after both parallel branches checkpoint and are interrupted, each re-dispatch resumes only its own checkpoint and provider session.

> Proposed 2026-11-07 (mosaic code-to-model re-audit). Source: mosaic `docs/DESIGN.md:2076-2079`, `:2161` (catalog D-31, "a broken fragment must fail only its own branch"), `internal/compose/scope.go:112-114` and `stages.go:426-447` (a handler that fails ends only its branch; catalog C-16, P-93); fae `engine/failure.go:43-46` (no route means a task-level `stop`), the validator's "no success route" when a stage's success and failure edges share the join; re-audit probes P9, P9b, P9c and P12 in `reaudit/gates/`.
>
> **FAE-WF-131 (MUST)** A stage inside a FanOut branch region (AgentTurn, Review, HostCommand, SubWorkflow, ChangeRequest or Gate) MAY name its region's FanIn as its `failure` route even when its success route also reaches that FanIn. The validator MUST accept the shared target and MUST keep the two routes distinct. Taking that route MUST end the branch instance as `failed`, with the `failure` artifact's `reason` and `error` as the branch's error in the join artifact (2026-11-06 join amendment to FAE-WF-096 (4)). It MUST NOT escalate the task, and no other branch is stopped except as the FanIn's rule decides. A FanOut MAY also declare `branch_failure: fail-branch`. Under it, a region stage that fails with no failure route of its own (after its bounded retries) ends its branch the same way instead of escalating the task. Without it, such a stage escalates as today.
> - *Rationale:* mosaic needs a broken area to fail only its own branch. With `rule: settle` (FAE-WF-096) every other area then still runs to its end. Today a failed handler escalates the whole task from inside the branch, so settle cannot deliver that. The only way to mark the branch failed is an always-failing gate whose failure edge goes to the join and whose success edge goes to a never-taken Escalate stage. A per-branch gate needs a pass-through stage for the same reason.
> - *Acceptance:* tests: (a) under `settle`, a branch whose handler exhausts its budget with `failure: <join>` is recorded `failed` with reason `budget`, and the other two branches run and pass; (b) a branch Gate with both edges to the join validates and records failed or passed per branch; (c) with `branch_failure: fail-branch` and no stage routes, the same outcome as (a); (d) without either, the task escalates as today; (e) ResumeAtStage after the join's failure re-runs only the failed branch.

> Proposed 2026-11-07 (mosaic code-to-model third audit), amendment to FAE-WF-131: in (e), "re-runs only the failed branch" means every branch that ended failed, cancelled or skipped, and only those, whether the FanOut is sequential (one at a time, in branch order) or parallel. After the resume, the join artifact's element for a branch that then passes MUST carry no `reason` or `error` from its earlier failure. Acceptance gains: (f) under `settle` with a sequential FanOut, 2 of 3 branches fail. ResumeAtStage re-runs both, one after the other, and the join passes with neither element carrying the old reason. Source: probe `reaudit2/hier/g/n_test.go` TestN5. Today the sequential case is refused ("more than one running branch", `engine/control.go:393`, `engine/fanout.go:112-113`), and a resumed branch keeps `reason` (`engine/control.go:346-348`, `engine/fan_join.go:25-27`).

### 4.4 Roles as data

**FAE-WF-040 (MUST)** A workflow MUST declare each role it uses with: a name; instructions (prose); one or more brief templates (by phase); ordered model routes; a tool profile; a slot scope (workflow-global or per-molecule task-bound); a cap (positive integer, or −1 meaning unbounded); permitted output schemas; and a session policy (long-lived for workflow-global roles; fresh per task for task-bound roles).
- *Rationale:* the engine knows no role; caps and scopes come only from the template, with no framework default.
- *Acceptance:* a workflow that references an undeclared role fails validation; an undeclared role is granted nothing at run time.

**FAE-WF-041 (MUST)** A route MUST be an ordered list of bindings, each with a provider (non-empty), a model (non-empty), an optional reasoning effort from the set minimal, low, medium, high, xhigh, and an optional auto-approval flag (default false). The list MUST be non-empty.
- *Rationale:* ordered fallbacks keep work moving when providers throttle.
- *Acceptance:* parse tests for each field and for an empty list.

**FAE-WF-042 (MUST)** A tool profile MUST be enforced at session launch by the runtime adapter (for example through the agent CLI's allowed and disallowed tool configuration, a pre-tool hook, and the command shims), not by instruction alone. Tool profiles MUST support: allowed tools; denied tools; allowed command shims; whether build, test and formatter commands are permitted; and external-effect tool grants (Section 9). A stage MAY tighten its role's profile; it MUST NOT widen it.
- *Rationale:* a pre-code role with full tools ran a 12 to 20 GiB formatter on a huge file; enforcement beats instruction.
- *Acceptance:* a pre-code role session that attempts a build or formatter is refused by the shim with a message; the generated agent configuration file records the mode and the exact grant.

**FAE-WF-043 (MUST)** The brief MUST be composed in this order: the role's instructions; the stage template rendered over bound artifacts by **structured binding** (values inserted as quoted, typed data blocks, never by string concatenation into instruction text); the current per-task operator context (Section 10.4); and finally the kernel contract, which the workflow cannot override: the output schema, the absolute handoff path, the frozen-artifact rules, and the gate obligations the output will face.
- *Rationale:* prompt injection from task text into instructions; and roles must know the contract they will be judged by.
- *Acceptance:* a test with task text containing an instruction-like line shows it inside a quoted data block; a test asserts the kernel contract appears last and verbatim.

**FAE-WF-044 (SHOULD)** A role SHOULD support per-task-class overrides of reasoning effort and of the brief template.
- *Rationale:* documentation tasks need less effort than engine changes.
- *Acceptance:* a workflow with an overlay for class "docs" dispatches its spec turn at the overlay's effort.

**FAE-WF-045 (MUST)** Long-lived workflow-global role sessions MUST receive each task's step data with an explicit task header and MUST NOT carry one task's working files into another task's worktree. Task-bound sessions MUST launch with the task worktree as their absolute working directory.
- *Rationale:* the prior system launched some roles at the workspace root instead of the task worktree, and a relative working directory caused agent CLIs to re-enter onboarding and hang.
- *Acceptance:* a launch test asserts every task-bound session's working directory is the canonical absolute task worktree; a pooled-session reuse test shows the working directory is re-validated before reuse.

> Proposed 2026-11-06 (mosaic code-to-model audit). Source: mosaic `requirements/fae-model-building-assessment-controller.md` FAE-CTL-107 (carried, widened to per-stage briefs); catalog P-52, D-53, C-20; audit in mosaic `requirements/fae-code-to-model-audit-2026-11-06.md` (rows agent AE-15; workflow W26, W27). It keeps its mosaic ID, which Fae does not use.
>
> **FAE-CTL-107 (MUST)** An AgentTurn or Review stage MAY declare its own `brief` template, which replaces its role's brief for that stage; the role's instructions still come first (FAE-WF-043). A stage or role brief template MAY reference `{{input.<path>}}` (FAE-WF-101), `{{artifact.<type>.<path>}}`, `{{item.<path>}}` inside a FanOut branch, `{{tree.<field>}}` (FAE-WF-106), `{{failure.<field>}}` (FAE-WF-112), `{{output_dir}}` (FAE-WF-104) and `{{staging_dir}}` (FAE-WF-111). The executor MUST render each reference as a quoted, typed data block positioned at the reference (FAE-WF-043's structured binding), never by splicing text into instructions. The same references MAY appear in write scopes (FAE-SEC-011), read policies (FAE-SEC-008) and HostCommand typed placeholders (FAE-WF-035). The validator MUST refuse a reference to an undeclared input, to an artifact type the stage does not bind, a path outside the item's schema, and `failure` in a stage that no failure route enters.
> - *Rationale:* one area template, parameterised by area id, owned paths and fix list, replaces ten roles or ten revisions per round, and mosaic's coordinator role runs about ten stages with different briefs; one role per stage would split the role's cap. At proposal (fae tree of 2026-11-06 08:25) the brief was `role.Briefs[phase]`, with `phase` closed to `test-first` and `implement`, sent unrendered beside the inputs; the working tree has since added a stage `brief` and typed-reference rendering and validation (`execution/executor.go:899-909`, `execution/template.go`, `workflow/templates.go:12-77`), and `phase` is still closed (`workflow/workflow.go:659`).
> - *Acceptance:* tests: (a) two stages of one role dispatch their own briefs; (b) `{{input.area}}` renders the area as a data block at that position, and an instruction-like value stays inside its block; (c) a reference to an undeclared input, and one to a declared artifact type the stage does not bind, each fail validation; (d) a FanOut over three items yields three briefs with different `item` blocks and three write scopes; (e) a stage without `brief` uses its role's brief as today.

> Proposed 2026-11-07 (mosaic code-to-model re-audit). Source: fae `workflow/templates.go:22-29`, `execution/template.go:42-98`, `engine/failure.go:198-207`; probe `scratchpad/reaudit/e2e/probe/probe_test.go`; mosaic `.claude/skills/mosaic-generate/compose/ontology-pipeline.yaml` (14 references `{{.inputs.contract.component}}` under `{{if .inputs.contract}}`).
>
> **FAE-WF-141 (MUST)** The validator MUST refuse a typed reference, in a brief, a command argument, a write scope or a read policy, whose path passes through a value that its schema allows to be null or absent (a `null` type, or a property not listed in `required`), unless the reference is written in the optional form `{{<path>?}}`. An optional reference whose value is null or absent MUST render as an explicit typed `absent` data block in a brief. In a command argument, a write scope or a read policy it MUST fail the stage with reason `binding` (FAE-WF-112), never with an escalation at dispatch. A brief MAY additionally contain a section `{{#present <path>}} … {{/present}}` (with an optional `{{#absent <path>}}` counterpart) that the renderer keeps or drops as instruction text. That decision is recorded with the dispatch; the section text is never spliced from data.
> - *Rationale:* one workflow serves the root (`contract` null) and its children. A reference `{{input.contract.component}}` passes validation and then fails every root turn at render. Without a presence form, the brief must ship both arms and ask the agent to pick, which moves workflow logic into the prompt.
> - *Acceptance:* tests: (a) `{{input.contract.component}}` with `contract` typed `[object, null]` is refused at validation, naming the path; (b) `{{input.contract.component?}}` renders `absent` at a root with null contract and the value at a child; (c) a `{{#present input.contract}}` section is kept for the child and dropped for the root, and both renders are recorded with their dispatches.

> Proposed 2026-11-07 (mosaic code-to-model third audit), amendment to FAE-WF-141: a value that may be absent includes an `{{artifact.<type>.…}}` reference bound by a stage input declared `optional`. The validator MUST refuse such a reference without `?` exactly as for a nullable schema path. Inside a `{{#present <path>}}` section, a reference below `<path>` MUST be judged as if `<path>` were present, so it needs `?` only for its own nullable or non-required steps. Acceptance: (d) `{{artifact.Obj.text}}` with the `Obj` input optional is refused at validation; (e) `{{#present input.contract}}{{input.contract.component}}{{/present}}` validates when `component` is required within `contract`. Source: third-audit probes L2 a3, a8, L2c: the first escalates at dispatch with "binding: typed reference artifact.Obj.text is absent" (`workflow/template_schema.go:37-48` reads only the schema), and the second is refused.

> Proposed 2026-11-06 (mosaic code-to-model audit). Source: mosaic catalog P-07 (`Bash(.claude/skills/mosaic-generate/scripts/mcheck *)` in the generation pipeline), P-78, P-81, D-09, C-29; `.claude/skills/mosaic-generate/templates/ORACLE.md:17-18`; audit in mosaic `requirements/fae-code-to-model-audit-2026-11-06.md` (rows agent AE-06; gates G27).
>
> **FAE-WF-119 (MUST)** A tool profile that grants a shell (FAE-RT-015) MAY restrict it with `shell_allow`: a list of argument-vector shapes in FAE-WF-035's form, each an executable (worktree-relative or absolute) with fixed leading arguments and an optional final `*` that admits any further literal arguments. A session whose effective profile has `shell_allow` MUST be able to run only one simple command that matches one entry. A command with `cd`, an environment assignment, a pipe, a redirection, command substitution, a list operator (`;`, `&&`, `||`), an `sh -c` wrapper or a second command MUST be refused. A profile MAY also declare `deny_test_runs: true`, which refuses every test runner the target configuration names, in any language, not only the shimmed ones. Enforcement MUST be mechanical: through the agent CLI's permission rules where it has them (for Claude Code, `Bash(<prefix> *)` rules under `dontAsk`) and through the pre-tool hook; the command shims (FAE-HOST-011) still apply to anything the command runs. A stage MAY narrow `shell_allow` and MUST NOT widen it. Every refusal MUST be audited with the refused command.
> - *Rationale:* oracle, judge and measure agents of model-building must run one checker script and nothing else: a full shell lets them run the repository's tests or read blind files by other means, and no shell leaves them unable to check their work. At the 08:25 UTC audit `shell` was all or nothing and the hook denied only shim bypasses (cited then as `workflow/agentic.go:27`, `runtime/claude_settings.go:107-108`, `provider/agentic.go:600`, `commandshim/hook.go:40-80`); fae's working tree at 09:58 UTC implements `shell_allow` and `deny_test_runs` (`workflow/shell_allow.go:11-66`, `:143`, `workflow/filesystem.go:99-121`, `commandshim/hook.go:620`, `provider/agentic.go:634`).
> - *Acceptance:* tests: (a) `scripts/mcheck a.yaml` runs under `shell_allow: [[scripts/mcheck, "*"]]`; (b) `cd x && scripts/mcheck a.yaml`, `FOO=1 scripts/mcheck a`, `scripts/mcheck a | cat`, `sh -c "scripts/mcheck a"` and `go test ./...` are each refused and audited; (c) a profile with `deny_test_runs` cannot run `pytest`, `go test` or `npm test`; (d) a stage that widens `shell_allow` fails validation; (e) a profile without `shell_allow` keeps today's shell.

> Proposed 2026-11-07 (mosaic code-to-model re-audit). Source: mosaic `.claude/skills/mosaic-generate/compose/ontology-pipeline.yaml:288`, `:620`, `:685-732` (paths hard-coded to round 1: `BENCHMARK1.md`, `oracle1/`, `judge1/`), `docs/PIPELINE.md` fix rounds ("round n+1, excluding every earlier sample"), catalog P-86, P-87; fae `workflow/templates.go:16-60` and `workflow/command_references.go:10-12` (the references offer no visit or iteration); re-audit probe P8 (a Loop around a FanOut works, but its body cannot name its round).
>
> **FAE-CTL-114 (SHOULD)** Briefs, HostCommand argv placeholders (FAE-WF-035), write scopes (FAE-SEC-011) and read policies (FAE-SEC-008) SHOULD accept `{{visit}}`, the current stage's visit number in its task or branch instance (FAE-WF-130), and `{{loop.<loop id>.iteration}}`, the 1-based iteration of an enclosing Loop. Each is rendered as a typed integer under FAE-CTL-107's structured binding, and the validator MUST refuse a `loop.<id>` that does not enclose the stage.
> - *Rationale:* fix rounds inside one task need per-round paths and arguments (`oracle{{loop.round.iteration}}/`, `--exclude-sample` over earlier rounds). Without such a reference the template either hard-codes round 1 or runs each round as a separate detached child with a `round` input (FAE-WF-101, FAE-WF-102), which works today but splits one round's state across tasks.
> - *Acceptance:* tests: (a) a Loop of 3 iterations whose HostCommand argv holds `out/r{{loop.round.iteration}}.json` runs with `r1`, `r2` and `r3`; (b) a brief in the loop body renders the iteration as a typed data block; (c) `{{loop.other.iteration}}` outside that loop is refused at validation.

> Proposed 2026-11-07 (mosaic code-to-model third audit), amendment to FAE-CTL-114: `{{loop.<loop id>.iteration}}` is the 1-based number of the current entry into the Loop's body, counting every entry, including one that FAE-WF-075 `progress` does not charge against `max_iterations`. It MUST differ on every entry within one task (or branch instance). Acceptance: (d) a Loop with `progress` whose measure falls on every round renders `r1`, `r2`, `r3`, `r4` over four rounds. Source: third-audit probe L3 (`scratchpad/reaudit2/gates/lang_test.go`): fae's working tree renders `r1, r2, r2, r2, r2` because the reference reads the charged count (`engine/template_bindings.go:34`, `engine/engine.go:1094-1096`, `engine/policy.go:205-207`).

### 4.5 Artifacts, evidence and provenance

**FAE-WF-050 (MUST)** Every artifact MUST be stored content-addressed (SHA-256 of canonical content), typed by an artifact type declared in the workflow (name plus JSON Schema plus version), and validated before storage.
- *Rationale:* typed evidence is what gates read.
- *Acceptance:* storing an artifact that fails its schema is refused; the same content stored twice yields one artifact id.

**FAE-WF-051 (MUST)** Every artifact MUST carry immutable provenance: producing stage id, primitive, role, binding (provider and model), session id and a best-effort pointer to the agent CLI's own transcript, input artifact ids, worktree tree hash at production, molecule id and time. Provenance MUST be written in the same transaction as the artifact and MUST NOT be updated afterwards.
- *Rationale:* a verdict that cannot name what it judged is not evidence.
- *Acceptance:* an update to a provenance row is refused by the plane; a chain query from a commit back to the spec returns every hop.

**FAE-WF-052 (MUST)** The per-task history read (Section 13) MUST be derivable from artifacts and transitions alone, without any session still existing.
- *Rationale:* sessions are reaped.
- *Acceptance:* after reaping every session, the history of a Done task shows every role, output and verdict.

**FAE-WF-053 (MUST)** An artifact produced by a session for a turn MUST be accepted only if the handoff carries the dispatch id of that turn and was written after the dispatch time. A handoff whose dispatch id is stale or reused MUST be refused.
- *Rationale:* reset dispatch counters re-minted byte-identical handoff paths, so a stale file could be read as a fresh answer.
- *Acceptance:* a test plants an older handoff at the expected path; it is refused and the turn waits for a fresh one.

> Proposed 2026-11-06 (mosaic code-to-model audit). Source: mosaic `requirements/fae-model-building-assessment-execution.md` FAE-WF-111 (carried; its instance-level store narrowed to one task and its branches, widened to agent sessions, and raised from SHOULD to MUST); `docs/DESIGN.md:2076-2079`, `:2170-2171` (catalog D-32), `docs/PIPELINE.md:129-131`, `:833-854` (P-83); catalog P-13, P-50, C-44; audit in mosaic `requirements/fae-code-to-model-audit-2026-11-06.md` (rows gates G10; agent AE-09).
>
> **FAE-WF-111 (MUST)** A task MAY keep files outside its commits in two declared places:
> 1. **Staging area.** A workflow MAY declare a staging area for its tasks: a directory outside every worktree and read root, on disk (FAE-HOST-020), created at admission and removed when the task becomes terminal. It MUST be one directory for the task and all its FanOut branch instances and worktrees, passed to every HostCommand through a typed placeholder and to every session through a fixed environment variable, and reachable by the agent CLI's file tools (FAE-RT-015). Stages and roles that declare `staging: write` MAY write it, from a HostCommand or from an agentic turn's tools and shell; every other session and command reads it read-only, under the read policy (FAE-SEC-008). Its contents MUST persist across turns, attempts, checkpoints and daemon restarts, MUST never be committed, and MUST NOT count as worktree changes in the broker's tree check or the turn's write-scope check. A HostCommand MAY declare `accept: true`; on a run that exits 0, does not time out and parses, the engine MUST then record a content digest of the staging files it wrote as evidence, so that a later gate or brief can cite the accepted version.
> 2. **Scratch.** A role or stage MAY declare `scratch`, a list of worktree-relative globs that an agentic session's tools and shell and the task's HostCommands may write. Scratch paths MUST be excluded from the collection diff, the out-of-scope revert, the turn's refusal, the broker's tree check, the stale-residue check (StaleBranchResidue), the checkpoint tree (FAE-RT-019), every commit and every landing, and MUST NOT overlap the write scope, read-only inputs or protected paths (the validator refuses an overlap). Scratch content MAY persist across turns of the same task and MUST be removed when the task ends.
> - *Rationale:* each mosaic area gates its fragment against the other areas "as they last passed" (`mcheck --stage --accept`) long before anything merges, parallel areas do this at the same time, and the checker keeps its last-good copy under the repository root. At the 08:25 UTC audit a check command's writes were deleted after the run, an agentic turn's snapshot (`git add -A -f`) counted ignored files so any such write was reverted and the turn refused, and each change-producing branch had its own worktree, so no copy could be shared (cited then as `effects/broker.go:476-508`, `provider/agentic.go:773`, `:877-894`, `daemon/live.go:424-443`); fae's working tree at 09:58 UTC implements a workflow-level staging area with accept digests (`daemon/task_directories.go:61-80`, `effects/broker.go:534-545`) and scratch (`daemon/task_scratch.go:10`, `effects/broker.go:969-981`).
> - *Acceptance:* tests: (a) of two parallel change-producing branches, branch 1's accept writes `stage/a.yaml` and branch 2's later command reads it; (b) a staging write from inside an agentic turn by a `staging: write` role survives collection and the turn is not refused, while the same write by a role without it is refused; (c) staging content survives a daemon restart and is removed when the task is Done; (d) an agentic turn that writes `.mosaic/stage/a.yaml` under `scratch: [.mosaic/stage/**]` is accepted; (e) a scratch glob that overlaps the write scope fails validation; (f) nothing under the staging area or a scratch glob enters any commit.

> Proposed 2026-11-06 (mosaic code-to-model audit). Source: mosaic catalog P-73, C-103 (the `results` turn reads its run's stats); audit in mosaic `requirements/fae-code-to-model-audit-2026-11-06.md` (row control 24, which found running agents querying the daemon in CONFLICT with FAE-SEC-003; this resolves it without changing FAE-SEC-003).
>
> **FAE-WF-120 (MUST)** A stage MAY declare an engine-provided input `run_stats: tree | task`. At dispatch the engine MUST bind a read-only artifact holding the stats of FAE-API-035 for the task's run tree (or for the task alone) as of the dispatch, with provenance `engine:run_stats` and the plane sequence it reflects. No role session receives the instance credential or any daemon access to obtain it (FAE-SEC-003, FAE-ARC-005).
> - *Rationale:* mosaic's results stage reports cost, turns and wall time per stage and branch; with the daemon credential barred from role sessions, the stats must come to the role as data.
> - *Acceptance:* tests: (a) a `results` stage with `run_stats: tree` receives an artifact whose totals equal the Tree read at the stated sequence; (b) the session environment and brief contain no instance credential; (c) a stage without the declaration receives no stats.

### 4.6 The gate expression language

**FAE-WF-060 (MUST)** Gates MUST be written in a small, total, side-effect-free expression language over evidence. It MUST support: evidence lookup by artifact type, by producing stage, and by most recent or all; field access by path; comparisons (equal, not equal, less, greater and their inclusive forms); boolean and, or, not; counts over collections with a filter predicate; existence tests; string equality and prefix tests; and membership in a literal set. It MUST NOT support loops, recursion, user-defined functions, I/O, time or randomness.
- *Rationale:* totality and determinism make gates provable and replayable.
- *Acceptance:* a grammar specification exists; a fuzz test over random expressions shows evaluation always terminates within a bounded step count.

> Proposed 2026-11-06 (mosaic code-to-model audit), amendment to FAE-WF-060: a lookup or count inside an element count's filter MUST select from all of the task's evidence, not only the record being counted, so that `in` with a lookup of another artifact or input as its right operand works there, and an element count nested inside an element count's filter MUST be able to read the enclosing element, so that "every requested id names a component" is expressible; the grammar MUST document `in` with a lookup as its right operand. Task inputs (FAE-WF-101) and the tree value (FAE-WF-106) are selectable evidence. Both forms stay within the existing nesting and step bounds. Source: mosaic `.claude/skills/mosaic-generate/compose/zoom.yaml:63-65` gate `component_known` and `.claude/skills/mosaic-generate/compose/ontology-pipeline.yaml:894-897` gate `zoom_known` (catalog P-60, C-65); today `in` already accepts any array-valued right operand, a lookup included (`workflow/gate.go:260-273`), although `workflow/GRAMMAR.md:19` documents literals only, but inside an element count's filter every lookup sees only the record being counted (`workflow/gate.go:310`), so a lookup of another artifact there is unknown, and `item` is the innermost element only (`workflow/GRAMMAR.md:12`, `workflow/gate.go:393-396`); audit in mosaic `requirements/fae-code-to-model-audit-2026-11-06.md` (row workflow W09).
> - *Acceptance:* tests: (a) a gate "each id of input `zoom` is the id of some component of the latest model" passes on matching data and fails on non-matching data; (b) the fuzz bound of FAE-WF-060 still holds with the new forms.

> Proposed 2026-11-07 (mosaic code-to-model re-audit), amendment to FAE-WF-060: an element count nested in an element count's filter MUST read the enclosing element through `outer` (`{"op":"outer","path":[...]}`, legal only two levels in, one per level) or an equivalent named binding. The validator MUST refuse a filter whose `item` (with no path) is compared to `item.<path>` of the same element, a form that is always false and is almost always a mistaken attempt at the enclosing element. Source: re-audit probe P6: the working tree accepts the nested form of mosaic's `zoom_known` (`ontology-pipeline.yaml:894-897`) and evaluates it against the inner element only (`workflow/gate.go:306-308`).

**FAE-WF-061 (MUST)** Evaluation MUST be three-valued: true, false or **unknown**. A missing field, a missing artifact, an unrecognised status value, or a type mismatch MUST yield unknown, and a gate whose result is unknown MUST fail closed.
- *Rationale:* an absent status must never read as success.
- *Acceptance:* tests for each unknown cause; the gate result names the unknown leaf.

**FAE-WF-062 (MUST)** Every gate MUST positively assert that it examined something. The validator MUST reject a gate expression that could pass when the evidence collections it reads are empty, unless the expression explicitly states that an empty collection is acceptable and the workflow marks that gate as an emptiness-permitted gate with a reason. For HostCommand evidence, every test-run gate MUST include a lower bound of at least one on the executed count.
- *Rationale:* gates that reported success while checking nothing were found three times in one day: a truncated list, a misclassified body, and a test filter that matched zero tests and printed "ok, 0 passed".
- *Acceptance:* the validator rejects a "no failures" gate that lacks a positive count; a run that reports zero passed and N ignored fails every test-run gate.

**FAE-WF-063 (MUST)** A gate result MUST record the ids of every evidence item it read and the value of every leaf, so that a reviewer can replay it.
- *Rationale:* auditability.
- *Acceptance:* replaying a recorded gate over its recorded evidence yields the same result.

> Proposed 2026-11-06 (mosaic code-to-model audit). Source: mosaic `requirements/fae-hierarchical-workflows.md` FAE-WF-106 (carried, raised from SHOULD to MUST and widened to tree identity); `.claude/skills/mosaic-generate/compose/ontology-pipeline.yaml:801`, `:912-918` and `compose/zoom.yaml:80` (catalog P-40, whose `zoom.yaml` line numbers are wrong, and P-48); catalog P-59, C-07, C-73, D-56; audit in mosaic `requirements/fae-code-to-model-audit-2026-11-06.md` (rows workflow W02, W23; control 8; gates G4).
>
> **FAE-WF-106 (MUST)** The engine MUST expose a read-only **tree** value for every task, `{task, parent, root, depth, remaining_descendants}`, with `parent` empty and `depth` 1 at the run-tree root, and `remaining_descendants`, the root document's `max_descendants` minus the tree's current non-root tasks, present only when the run tree's root declares `recursion` (FAE-WF-100). Gates and Branch arms MUST read it as evidence, recorded as gate leaves (FAE-WF-063); briefs receive it as a typed data block (FAE-CTL-107); input mappings and projections MAY select it. Child task ids MUST stay hierarchical: `<parent>.<stage>.<visit>`, then one `.<branch-index>` for each enclosing FanOut level (FAE-WF-099, FAE-WF-113) and `.<item-index>` for each child of a multi-child visit, and `<parent>.detached.<n>`, up to the instance recursion ceiling, and MUST NOT be replaced by a hash; the validator MUST refuse a stage id that contains `.`, and the stage id `detached`, so that a SubWorkflow child's id never equals a detached child's.
> - *Rationale:* one mosaic template runs as a root and as a refinement child, choosing the full or light pipeline and the conformance command by `tree.parent`; a recursive template stops zooming deeper by itself, before a budget refusal; and mosaic lists a tree by id prefix. Before fae's 2026-11-06 implementation pass, gate evidence `Tree` was only a git tree hash, the brief carried only `task_id`, ids over 200 bytes became `sub-<hex>`, and stage ids could contain `.` (audit rows W02, W23); the tree value is now `engine/tree_inputs.go:25-63`, ids are `engine/subworkflow.go:41-75`, and the stage-id refusal is `workflow/workflow.go:665`.
> - *Acceptance:* tests: (a) a Branch on `tree.parent` takes the root arm in a root task and the child arm in a SubWorkflow child; (b) a gate `tree.depth < 2` takes its failure route at depth 2 without a `recursion_budget` refusal, and its record lists the tree leaves it read; (c) a brief's tree block names the parent task; (d) a depth-8 child of a 60-byte root id keeps its hierarchical id; (e) the stage ids `a.b` and `detached` fail validation.

### 4.7 Kernel safety floors

**FAE-WF-070 (MUST)** The following floors MUST always be on, for every workflow, and MUST NOT be weakened by content. Content MAY tighten them.
1. Fail-closed parsing and validation (FAE-WF-003, FAE-WF-004).
2. Immutable molecules and revisions (FAE-WF-002, FAE-WF-006).
3. Audited transitions (FAE-WF-022).
4. Task worktree isolation (FAE-ARC-008, FAE-VCS-001).
5. Host guard and leases on every heavy op and every session launch (Section 14).
6. Attempt caps on every loop and a per-task attempt limit (FAE-WF-074).
7. No-bypass (FAE-WF-031).
8. Positive-evidence gates (FAE-WF-062).
9. The protected-commit floor (FAE-WF-071).
10. The external-effect floor (FAE-WF-072).
- *Rationale:* safety properties are platform policy, not content.
- *Acceptance:* a property test generates workflows that try to disable each floor; every one is rejected at validation.

**FAE-WF-071 (MUST)** Protected-commit floor: on **every** path in the graph from the start stage to any VcsCommit, Integrate or ExternalEffect(code-review merge) whose target is a protected branch, the validator MUST find (a) a HostCommand test run followed by a Gate that asserts its exit code and a positive executed count on the same tree hash that is committed, and (b) at least one Review stage whose reviewer role and session differ from every role and session that produced the change. Every protected branch MUST be declared in the target configuration; the instance integration branch is always protected.
- *Rationale:* replaces per-state hand-written obligations with a graph property provable for arbitrary graphs.
- *Acceptance:* the validator rejects a generated workflow with one path that skips the test gate, and one with a self-review; at run time, a commit whose tree hash differs from the gated tree is refused.

**FAE-WF-072 (MUST)** External-effect floor: an ExternalEffect stage MUST be reachable only in Normal mode, MUST carry a pinned argument set and an idempotence key, and MUST be preceded on every path by the protected-commit floor's gates for the tree it publishes.
- *Rationale:* external effects are irreversible.
- *Acceptance:* the validator rejects an ExternalEffect without pins or reachable in Local mode.

**FAE-WF-073 (MUST)** Test-first floor for code-bearing task classes: when a task class is declared code-bearing, the validator MUST require on every path to the implementation stage a RED sequence: a test-authoring turn, a HostCommand that runs the declared test, a Gate asserting it failed for the declared reason with exactly one matched test, and a FreezeArtifact of the RED identity. Every path from implementation to commit MUST include a CompareFrozen asserting that the RED identity is unchanged and a HostCommand asserting that the same test now passes. A task MAY be declared **no-red** only by the intake or spec stage (never by the implementing role), and a no-red declaration MUST be enforced mechanically at commit by a diff classifier the target configuration declares.
- *Rationale:* RED/GREEN identity proves the test tests the change; no-red is a governed exception, not a coder's choice.
- *Acceptance:* the validator rejects a code-bearing workflow without RED or without CompareFrozen; a no-red task whose diff contains production code is refused at commit.

**FAE-WF-074 (MUST)** Every Loop MUST have a finite maximum iteration count, and every task MUST have a per-task attempt limit that is durable, re-read at every attempt boundary, and adjustable by an audited operator verb at any time. Raising the limit of a task escalated for attempt-cap exhaustion and resuming it MUST re-grant attempts; raising it MUST NOT bypass any other escalation kind. Defaults: task attempt limit 20; spec-review loop 3 iterations; code-review remediation loop 5; scope-widen requests 3; test runs per task 8 (all configurable per workflow).
- *Rationale:* bounded churn, but a converging task must not die on a flat count.
- *Acceptance:* tests cover each bound, the mid-flight raise, and the re-grant rule.

**FAE-WF-075 (SHOULD)** A workflow SHOULD be able to declare that an iteration that strictly reduces the open blocking-finding count does not consume a loop iteration, subject to a separate no-improvement streak bound.
- *Rationale:* progress-aware caps avoid killing converging tasks.
- *Acceptance:* a scripted remediation loop that reduces blocking findings 5 → 3 → 1 → 0 finishes even with a loop cap of 2.

**FAE-WF-076 (MUST)** The RED identity MUST be computed over the normalised content of the declared test functions only (a stable syntactic normalisation that is invariant under the project formatter), not over the whole file and not over raw bytes.
- *Rationale:* hashing whole-file test content made adding a required second test, or a formatter pass, falsely trip "modified RED".
- *Acceptance:* tests: reformatting the RED file leaves the identity unchanged; adding a new test function leaves it unchanged; changing the RED body's assertion changes it.

**FAE-WF-077 (MUST)** When a final-judgement decision targets the RED test itself, the task MUST route back to the test-authoring stage, where a new RED identity is captured and verified failing against the task's base revision. The superseded identity MUST remain in the audit trail. A decision targeting production code MUST route to implementation with the RED still frozen. A decision whose target is unknown MUST escalate.
- *Rationale:* an immutable RED with no amendment path made correctable tests unfixable.
- *Acceptance:* a scripted verdict with target RedTest re-enters test authoring and records both identities.

**FAE-WF-078 (MUST)** A role MUST be able to request, through its handoff, a **scope widen** (additional declared files with a reason) or a **RED rework** (with a reason). The request MUST be judged by a declared gate or role, MUST be durable across resume, and MUST be counted against its cap. Approved scope MUST be carried forward to every later brief and gate of the task.
- *Rationale:* a resume once dropped an approved widen and the task then exhausted its cap.
- *Acceptance:* approve a widen, restart the daemon, and assert the widened set appears in the next brief and the cap counter is unchanged.

### 4.8 The default workflow, as content

**FAE-WF-080 (MUST)** Fae MUST ship a default workflow, installed at first start as revision 1 of the default workflow id and expressed entirely as content (Appendix C). Its stage sequence MUST be:
intake → spec (AgentTurn: spec role) → spec review (Review: two spec reviewers, any-blocking-fails; Loop back to spec, at most 3) → plan (AgentTurn: planner) → test-first (AgentTurn: coder, RED phase) → run RED (HostCommand) → Gate (fails for the right reason, exactly one matched test) → FreezeArtifact (RED) → implement (AgentTurn: coder, implement phase) → run tests (HostCommand) → Gate (green, positive count) ∧ CompareFrozen (RED unchanged) → reviews (Review: correctness, quality, security, architecture; each dimension isolated; any-blocking-fails) → final judgement (AgentTurn: oracle, typed decisions) → Branch (RED rework → test-first; production rework → implement; pass → QA) → QA (HostCommand of the required tests and declared checks, then Gate) → Merge (reconcile onto tip; Branch: clean → commit; conflicted → implement with paths; own-hunk loss → refuse) → VcsCommit → Integrate → (Normal mode only: code-review sub-workflow, Section 9) → Done.
- *Rationale:* the prior pipeline, kept as content so authors can change it.
- *Acceptance:* the default workflow validates; a scripted end-to-end run produces the transition sequence of Appendix C.

**FAE-WF-081 (MUST)** The default workflow's role caps MUST be: spec role 2; spec reviewer 2; planner 2; coder 4; each reviewer dimension 4; final-judgement (oracle) 1; QA 1; transition verifier (if used) 1. All are per-workflow declarations, not framework defaults.
- *Rationale:* operator ruling: wider pre-code and review capacity; oracle and QA are deliberate bottlenecks.
- *Acceptance:* the installed content declares these values, and the lease table honours them.

**FAE-WF-082 (MUST)** The default routes MUST be a deep ordered chain per role, configured per instance, with the operator's preferred binding first, so that the failure of any one provider does not stall work.
- *Rationale:* operator: "the goal of fallbacks is to always have work continuing". A two-entry chain stalled for hours when both entries throttled.
- *Acceptance:* the default content declares at least 4 bindings per role across at least 2 providers.

**FAE-WF-083 (MUST)** The default role instructions MUST state each role's purpose, what it produces and what it must not do, as in the following table. The kernel contract is appended after them.

| Role | Purpose | Produces | Must not |
|---|---|---|---|
| Spec | Turn the item into a precise, testable spec. | Scope; acceptance criteria; contracts; risks; required tests stated as intent (what must be proven, name and filter). | Write or format code, run builds or tests, or create files outside its handoff. |
| Spec reviewer | Adversarially check the spec for ambiguity, untestability, missing contracts, scope creep, and that every criterion is satisfiable under the item's own constraints. | Approve or reject with specific findings. | Rewrite the spec, write code or run builds. |
| Planner | Decompose the approved spec into an ordered plan. | Steps, declared files, test placement, risks. | Write code or run builds. |
| Coder (test-first) | Author the failing RED test from the test intent. | A RED in the worktree that fails for the right reason. | Write production code in this phase, or edit outside the worktree. |
| Coder (implement) | Make the frozen RED pass with minimal, clean code. | Code in the worktree and a handoff with a proof note. | Edit the frozen RED (request a RED rework instead), widen scope silently (request a scope widen), or touch anything outside the worktree. |
| Reviewer (one per dimension) | Independently review one dimension of the diff. | Findings with blocking or non-blocking severity. | Edit code, or review other dimensions. |
| Oracle | Decide whether spec, tests, code and reviews satisfy the acceptance. | A typed decision per finding: validity, severity and target (RED test or production). | Edit code, or soften a blocking finding. |
| QA | Run the required tests and checks on the task tree. | Pass/fail evidence from real runs. | Edit source, or skip or ignore tests. |

- *Rationale:* operator-approved role definitions.
- *Acceptance:* the installed content contains this text for each role.

**FAE-WF-084 (MUST)** In the default workflow, the final-judgement role MUST approve only when it records zero valid blocking findings, and non-blocking findings MUST be recorded as durable follow-up artifacts.
- *Rationale:* blocking findings are fixed before passing; deferrals are tracked.
- *Acceptance:* a scripted verdict with one valid blocking finding routes to rework.

**FAE-WF-085 (MUST)** In the default workflow, QA MUST be a separate gate from final judgement, and QA's pass MUST rest on engine-run evidence (FAE-ROB-030, FAE-ROB-031), not on the QA role's prose. The instance's always-required test commands (from the target configuration, optionally scoped by path prefix) MUST run at QA for every task, and a tree that fails to compile under the declared configuration MUST be refused at commit regardless of any verdict.
- *Rationale:* operator ruling: QA evidence must be engine-run and tree-bound.
- *Acceptance:* a QA role reporting "all pass" while the engine run exits nonzero fails the gate.

**FAE-WF-086 (SHOULD)** The default workflow SHOULD carry accepted pre-code evidence (spec, spec reviews, plan) across a re-admission of the same item under a new identity, when the acceptance text is unchanged and the operator's re-admission says so.
- *Rationale:* 37% of role time in the prior system was repeat work, much of it re-running accepted pre-code stages after infrastructure escalations.
- *Acceptance:* a re-admission with carry-forward starts at test-first, with the carried artifacts linked by provenance.

### 4.9 Authoring, validation, simulation, diff and migration

**FAE-WF-090 (MUST)** Fae MUST provide a validator, usable offline from the CLI and from the API, that checks a workflow document against the schema, the kernel floors (as graph properties), role and artifact references, reachability (every stage reachable from start; every non-terminal stage has a path to Done or Escalated), the absence of unbounded cycles (every cycle passes through a Loop with a cap), and that every Branch has a default arm. It MUST report all errors with document paths.
- *Rationale:* authoring raw graphs is error-prone.
- *Acceptance:* a corpus of invalid workflows, one per rule, each fails with the expected path.

**FAE-WF-091 (MUST)** Fae MUST provide a dry-run simulator that executes a workflow revision on the scripted adapter against declared role outputs and host-command results, records every transition, gate result and artifact, and reports the path taken. It MUST NOT touch the live control plane or any worktree outside a scratch area.
- *Rationale:* authors must see what a graph does before publishing it.
- *Acceptance:* simulating the default workflow with a scripted "spec rejected once, oracle blocks once" scenario reports the expected path.

> Proposed 2026-11-06 (mosaic code-to-model audit), amendment to FAE-WF-091: the simulator, and the daemon's scripted rehearsal, are extended by FAE-WF-105 (children, recursion, detached admissions and dynamic FanOuts run as real child tasks; scenario rules by role, task-id glob and branch; seeded outputs for unscripted turns). The scripted rehearsal is a daemon of its own instance and writes that instance's control plane; FAE-WF-091's bar on touching the live control plane applies to the offline simulator. Source: mosaic `requirements/fae-hierarchical-workflows.md` §5 (the `workflow-simulate` amendment); audit in mosaic `requirements/fae-code-to-model-audit-2026-11-06.md` (rows workflow W28; control 9).

**FAE-WF-092 (MUST)** Fae MUST provide a structural diff between two revisions: stages, edges, gates, roles, caps, routes and limits added, removed or changed, and the kernel-floor proof status of each side.
- *Rationale:* reviewing a new revision needs a semantic diff.
- *Acceptance:* diffing revision N and N+1 that changed one cap and added one stage reports exactly those two changes.

**FAE-WF-093 (MUST)** Migration: in-flight tasks finish on their pinned revision. Moving a task to a new revision MUST be an explicit operator verb that validates the task's current evidence against the new revision's stage at the mapped position and refuses when a required input is missing. Every revision ever written MUST remain loadable by later binaries, or the binary MUST refuse to start with a message naming the unloadable revision.
- *Rationale:* reproducibility and safe evolution.
- *Acceptance:* tests: a migration verb on a task whose evidence fits succeeds with an audit row; one whose evidence does not fit is refused.

**FAE-WF-094 (MUST)** The engine MUST be proved against the default workflow by a **parity corpus**: recorded runs (role outputs, host-command results, timings abstracted away) whose transition sequences and gate results are the expected outputs. Any change to the engine MUST keep the corpus green.
- *Rationale:* the engine replaces per-role code paths; parity proves nothing was lost.
- *Acceptance:* the corpus has at least 20 recorded runs covering every Branch arm and every escalation kind of the default workflow, and runs in CI.

> Proposed 2026-11-06 (mosaic code-to-model audit). Source: mosaic `requirements/fae-hierarchical-workflows.md` FAE-WF-105 (carried, widened to scripted rehearsal); catalog D-47, P-92, C-109, C-110, C-111, C-112; audit in mosaic `requirements/fae-code-to-model-audit-2026-11-06.md` (rows workflow W28; control 9).
>
> **FAE-WF-105 (MUST)** The dry-run simulator (FAE-WF-091) and the daemon's scripted rehearsal (`fae serve` on the scripted adapter) MUST:
> 1. run SubWorkflow children, detached children, self-recursion and dynamic FanOuts as real child tasks from the same scenario, and report the run tree they create and every `recursion_budget` and `width` refusal;
> 2. match scenario rules on stage plus optional role, task-id glob and branch index (of the innermost enclosing branch), the first match winning;
> 3. let a rule give a literal output, an injected failure class, a delay, or a scripted detached admission, change request or per-item decision;
> 4. give a turn that no rule matches a deterministic, schema-valid output from a seeded generator, and report that it did.
> - *Rationale:* a mosaic run tree must be rehearsed before hours of agent time are spent on it. Before fae's 2026-11-06 implementation pass the simulator scripted a SubWorkflow's result instead of running the child, rules were keyed by task, branch and stage only, and an unscripted turn was an error; that legacy script format is still accepted (`engine/simulate.go:126-165`, `engine/scripted.go:15-49`) beside the rule scenario this requirement describes (`workflow/scenario.go:14-47`, `engine/scenario_scripted.go:41-152`).
> - *Acceptance:* tests: (a) a scenario scripting a 3-item area list and one recursive zoom yields a tree of 1 root, 3 parallel branches and 1 child, with the child's mapped outputs in the root's join artifact; (b) a rule `task: "root.*"` matches every child of `root`; (c) an unmatched turn gets a seeded output marked `generated`, and two runs with one seed give the same outputs.

> Proposed 2026-11-07 (mosaic code-to-model re-audit), amendment to FAE-WF-105: (5) the simulator and the scripted rehearsal MUST bind every runtime value a live dispatch binds. Each task gets a deterministic output directory (and staging directory when declared) under a scratch root, used by HostCommand placeholders, briefs and write scopes. Incoming change requests (FAE-WF-103) are filled before a ChangeRequest stage binds them. Acceptance adds: (d) a HostCommand with `{{output_dir}}/x` in an argument rehearses with a per-task path and reaches Done; (e) a forwarding ChangeRequest whose value is the incoming requests forwards the child's request with its value. Source: fae `engine/scenario.go:24-60`, `:169-185` (no `OutputDir`, no `IncomingRequests`), `engine/command_templates.go:24`, `:39-40`, `engine/failure.go:145-151`; rehearsal of mosaic `ontology-pipeline-light.yaml:634-651` escalated `CommandBinding: command directory output_dir is unavailable`, and a zoom forwarded `value: null`.

---

## 5. Scheduling and parallelism

**FAE-SCH-001 (MUST)** Dependency edges MUST be used only for real semantic prerequisites. The system MUST NOT create, require or recommend edges or locks for file overlap. Overlapping edits MUST be reconciled by merge-reconcile at integration (Section 7).
- *Rationale:* 44 of 60 edges in the prior system existed only to serialise file edits; they starved the ready set to 1 item of 19 and averaged 1.9 tasks in flight when about 4 were possible.
- *Acceptance:* the scheduler has no file-lock concept; write-class tags are informational only and never withhold a task; a test with two tasks declaring the same file runs both concurrently.

**FAE-SCH-002 (MUST)** A blocking edge's prerequisite MUST count as satisfied when the prerequisite is Done, or Retired with reason class contained or superseded-by (for superseded-by, the dependent's edge is carried to the successor). A prerequisite Retired as obsolete, Cancelled, or Escalated MUST hold the dependent and MUST surface the dead edge in status and the event stream.
- *Rationale:* dead edges to escalated prerequisites made dependents permanently unreachable and were cleared only by direct database edits.
- *Acceptance:* tests for each prerequisite outcome; a dead-edge report names the edge.

**FAE-SCH-003 (MUST)** The ready set MUST be ordered by priority (larger first), then by admission time, re-read live at every round so a reprioritisation takes effect at the next dispatch decision.
- *Rationale:* high-priority items once sat pending for 5 hours while later, lower-priority items were dispatched.
- *Acceptance:* reprioritising a pending item above a running sibling's priority makes it the next dispatch.

**FAE-SCH-004 (MUST)** Tasks in flight MUST be unbounded by default (configurable maximum, default −1 meaning unbounded). Concurrency MUST be limited only by the role caps of each workflow, the molecule concurrency limits, the host guard and the pane limit.
- *Rationale:* operator ruling: decouple tasks in flight from coder slots.
- *Acceptance:* with ample host resources and caps, 8 ready independent tasks all leave Pending in the first round.

**FAE-SCH-005 (MUST)** Role slots MUST be leased per the workflow declaration: workflow-global caps are shared by all molecules of the workflow id across all revisions; task-bound caps apply per molecule. A freed task-bound slot MUST take the next waiting task, with fresh session context. Slots MUST be spawned only on demand, never pre-filled to the cap. There MUST be no host-wide role lease.
- *Rationale:* operator ruling on per-workflow role leases.
- *Acceptance:* tests: two molecules of the same workflow share one oracle slot; the same two molecules each get their own 4 coder slots; no session exists for a role until a stage needs it.

> Proposed 2026-11-05 (mosaic model-building), amendment to FAE-SCH-005: "no host-wide role lease" stays. A provider concurrency pool (FAE-SCH-015) is a provider lease, not a role lease, and a session MAY need both its role slot and a pool lease.

> Proposed 2026-11-06 (mosaic code-to-model audit), amendment to FAE-SCH-005: a role MAY also declare `cap_scope: tree` or `cap_scope: task` (FAE-SCH-024). "No host-wide role lease" stays; a tree or task scope is bounded by one run tree or one task, never host-wide, and FAE-SCH-005's acceptance applies to the default `workflow` scope. Source: mosaic `requirements/fae-hierarchical-workflows.md` §5 (the FAE-SCH-005 amendment); audit in mosaic `requirements/fae-code-to-model-audit-2026-11-06.md` (row control 14).

**FAE-SCH-006 (MUST)** When two revisions of the same workflow resolve different caps for the same workflow-global role, the shared pool MUST use the most recent revision's cap that has a non-terminal task, and the choice MUST be reported in status.
- *Rationale:* the prior design left this unspecified.
- *Acceptance:* a test with revisions declaring caps 1 and 2 uses 2 while the newer revision has live tasks.

**FAE-SCH-007 (MUST)** Molecule concurrency MUST be bounded by two operator-configurable numbers: an instance-wide maximum of running molecules (default −1, unbounded) and a per-workflow maximum (default −1). Because one molecule holds every task of a revision, these limits never cap tasks within a molecule.
- *Rationale:* a per-task molecule identity combined with a default of 1 would serialise everything.
- *Acceptance:* a test with two revisions running concurrently under the defaults runs both.

> Proposed 2026-11-06 (mosaic code-to-model audit), amendment to FAE-SCH-007: for a workflow with `slot_scope: molecule` these limits are checked at slot acquisition (FAE-SCH-016, -018) on every path that starts a molecule, not only on release from Pending. They do not apply to `tree`-scoped run trees, which the host workflow limit and FAE-SCH-025 bound instead.

**FAE-SCH-008 (MUST)** The scheduler MUST be fair: a task that is repeatedly blocked or parked MUST NOT starve unrelated ready tasks that have capacity, and within a role's queue the longest-waiting eligible stage turn MUST be granted next, ties broken by task priority.
- *Rationale:* starvation is invisible until it is long.
- *Acceptance:* a test with a perpetually parking task and five ready tasks shows the five progress every round.

**FAE-SCH-009 (MUST)** Each drive round MUST collect every completed turn and host command, across all tasks, before dispatching, and MUST dispatch all eligible stages within capacity in that round. Collection and dispatch of different tasks MUST NOT be serialised behind one another's turns.
- *Rationale:* a finished handoff once waited behind a long sibling turn.
- *Acceptance:* a test with 3 tasks finishing turns in the same round shows all 3 collected and advanced in that round.

**FAE-SCH-010 (MUST)** Every drive round MUST record why each ready-but-not-dispatched task was withheld (cap, molecule limit, host guard, pane limit, edge, deferral, route unhealthy) as a counter and a trace entry.
- *Rationale:* starvation was invisible because withholding was never logged.
- *Acceptance:* the status and metrics show withheld counts by reason.

> Proposed 2026-11-06 (mosaic code-to-model audit), amendment to FAE-SCH-010: the withheld reasons also include `tree_limit` (FAE-SCH-025), `cap` for tree- and task-scoped caps (FAE-SCH-024), and `workflow_limit`, `host_capacity`, `host_ledger` and `telemetry_gap` (FAE-SCH-021).

**FAE-SCH-011 (MUST)** Heavy-op leases MUST be weighted, FIFO-fair and priority-classed:
1. The heavy-op pool has a capacity in weight units (default 2, configurable). Each heavy op declares a weight (defaults: targeted compile or single-target test run 1; whole-suite test run 2; formatter on a file over the size limit 2; package build 2).
2. The longest waiter MUST be granted the next sufficient capacity. A new arrival MUST NOT overtake an existing waiter.
3. Priority classes (operator, controller, role) break ties between waiters of equal wait only; a higher class MUST NOT jump an older waiter of lower class.
4. A waiter not admitted within a bound (default 300 s) MUST emit exactly one line naming its wait and the current holders, and the same as an event.
5. A request MUST be refused after a maximum wait (default 1800 s), never run unleased.
6. A lease MUST carry a TTL (default 6 h) and an owner (pid and start time), and a lease whose owner is dead MUST be reclaimed.
7. Controller and operator heavy ops MUST take the same lease; they MUST NOT be exempt.
- *Rationale:* a stream of role compiles starved a controller landing for 58 minutes; exempting the controller had earlier caused a memory event.
- *Acceptance:* tests: with a full pool and three waiters, admission order equals arrival order; arrivals during the wait do not overtake; equal waits favour the higher class; the waiting line is emitted exactly once; an empty pool behaves as before.

**FAE-SCH-012 (MUST)** The pane limit (default 60, configurable) MUST be enforced against the **live** pane count reconciled from the runtime each round, never against a ledger that can drift. The daemon MUST refuse to start when the effective pane limit is not above the number of workflow-global role slots plus one.
- *Rationale:* the ledger once reported zero panes while 43 existed; a configuration that can only livelock must not run.
- *Acceptance:* tests: a planted ledger reading 0 with 10 live panes at a limit of 10 refuses the next launch; a start with limit below the floor is refused naming both numbers.

**FAE-SCH-013 (MUST)** When the pane limit is saturated by the instance's own idle long-lived panes with nothing in flight for 3 consecutive rounds (default), the scheduler MUST shed one idle sheddable pre-code workflow-global session (never the final-judgement or QA role) and, if the condition recurs with nothing left to shed, escalate the parked tasks with kind PaneCapStarvation. A limit held by panes the instance does not own MUST keep the action parked and re-ticked without a bound.
- *Rationale:* bounded self-starvation, liveness under foreign pressure.
- *Acceptance:* tests for shedding, escalation and the foreign-held case.

**FAE-SCH-014 (SHOULD)** Long-lived workflow-global sessions that are idle for longer than a bound (default 30 min) SHOULD be retired and relaunched on demand, keeping a handoff of their context.
- *Rationale:* idle sessions cost about 0.7 GiB each.
- *Acceptance:* an idle global session is retired after the bound and its next turn resumes from the recorded handoff.

> Proposed 2026-11-05 (mosaic model-building). Source: mosaic `requirements/fae-model-building-assessment-execution.md` §3.1 FAE-SCH-030 (and §1.2, "Global cap"); related to the unapplied host workflow limit in `requirements/fae-host-workflow-limit.md`; re-checked in `requirements/fae-recheck-2026-11-05.md`.
>
> **FAE-SCH-015 (MUST)** An instance MAY declare **provider concurrency pools**, each `{id, match: {provider, credential identity}, cap}`. Every agent session whose binding matches a pool MUST count against it, whatever its workflow id, revision, role or slot scope, and including Prophet sessions and the sessions of every instance sharing the host lease ledger. A pool MUST be a host-ledger lease (FAE-HOST-031), granted in ready-set order (FAE-SCH-003), and waiting for it MUST charge no attempt and MUST be reported as a withheld reason (FAE-SCH-010). A pool MAY be resized while the daemon runs, audited; a reduction affects later grants only and stops nothing.
> - *Rationale:* provider usage limits are per account, so a model-building run caps about 10 agents across two models' rounds, several workflow ids and every role; role caps are per workflow and cannot express a shared total.
> - *Acceptance:* tests: (a) two workflows with role caps of 8 each and one pool of cap 10 never run more than 10 sessions; (b) two instances sharing the ledger respect the same pool; (c) a Prophet chat counts against the pool; (d) lowering the cap from 10 to 6 stops nothing, and the next grant waits until fewer than 6 run.

> Proposed 2026-11-06 (mosaic code-to-model audit). Source: mosaic `requirements/fae-hierarchical-workflows.md` FAE-SCH-024 (carried; its `pool:<id>` update in `fae-model-building-assessment-execution.md` §3.2 is covered by FAE-SCH-015); catalog D-49, P-66, C-95; audit in mosaic `requirements/fae-code-to-model-audit-2026-11-06.md` (row control 14).
>
> **FAE-SCH-024 (MUST)** A role MAY declare `cap_scope`: `workflow` (the default, FAE-SCH-005's behaviour), `tree` (one cap shared by every task of one run tree: the root, its SubWorkflow children and its detached children, whatever their workflow id), or `task` (one cap per task, shared by its FanOut branches). The lease key MUST be the scope with the workflow id, tree root or task id, plus the role. When tasks sharing one tree-scoped pool declare the role with different caps, the pool MUST use the smallest cap declared by a non-terminal task of the tree, and status MUST report the choice, as FAE-SCH-006 does for revisions. Waiting for a tree- or task-scoped slot MUST behave as a role-slot wait does today (FIFO, no attempt charged, withheld reason `cap`). Whatever the scope, sessions stay bounded by provider pools (FAE-SCH-015), the tree session limit (FAE-SCH-025), tasks in flight and launch admission.
> - *Rationale:* every scope of a mosaic hierarchy runs the same workflow ids, so workflow-wide caps (oracle 4, benchmark 1) make every hierarchy on the host queue behind one another, and task-bound caps are per molecule, that is per revision, not per task (`daemon/live_admission.go:1012-1022`, `daemon/schedule_view.go:34-38`). Mosaic needs coordinator 2 per task and area 8 per tree.
> - *Acceptance:* tests: (a) two trees of one workflow with `oracle: {cap: 1, cap_scope: tree}` run two oracle turns at once, while a second oracle turn within one tree waits; (b) `coordinator: {cap: 2, cap_scope: task}` lets two tasks of one revision each run two coordinator turns; (c) a detached child shares its root's tree-scoped cap; (d) the default scope behaves as today; (e) a root and a child of another workflow that declare role `area` with `cap_scope: tree` and caps 8 and 4 never run more than 4 `area` turns at once.

> Proposed 2026-11-06 (mosaic code-to-model audit). Source: mosaic catalog D-51, P-68, C-98 (`--max-parallel` per run); audit in mosaic `requirements/fae-code-to-model-audit-2026-11-06.md` (rows control 17 and 31). It bounds sessions inside one run tree; FAE-SCH-016 to -023 bound how many trees and molecules run on the host, and FAE-SCH-020 sizes a `tree`-scoped footprint from this limit.
>
> **FAE-SCH-025 (MUST)** Admission MAY set `max_sessions` for a run tree, on the root's admission; its SubWorkflow and detached children inherit it. The daemon MUST NOT run more agent sessions plus HostCommands of that tree at once than `max_sessions`. Excess dispatches MUST wait FIFO by priority then admission time with withheld reason `tree_limit`, charge nothing, and hold no role slot, pool lease, pane or heavy-op unit while waiting. An audited verb MAY change the limit while the tree runs; a reduction stops nothing and affects later grants only. The default is −1, unbounded.
> - *Rationale:* mosaic bounds each run on top of its role caps, with about 20 sessions ready at once after the merge gate. Most of a run's parallelism is FanOut branches inside one task, so the tasks-in-flight limit cannot express it (`daemon/daemon.go:70`, `:292`), and a host-wide pool (FAE-SCH-015) would let one run starve another.
> - *Acceptance:* tests: (a) a tree with `max_sessions: 4` and 10 ready branches runs at most 4 sessions and commands at once; (b) a second tree is not held back by the first one's limit; (c) lowering 4 to 2 stops nothing, and the next grant waits until fewer than 2 run; (d) waiting charges no attempt.

> Proposed 2026-11-06 (mosaic code-to-model audit). Source: operator ruling 2026-10-30 (a global, host-aware, live-configurable limit on how many workflows run on a host; a controller may suggest the number but never override host capacity; a workflow the host cannot sustain fails to spawn). Carries mosaic `requirements/fae-host-workflow-limit.md` FAE-SCH-016 to -023 and FAE-CLI-044, with one change: the counted unit is a **slot holder**, chosen per workflow by `slot_scope`, because a mosaic pipeline reuses one template revision for every run and zoom, so counting molecules alone would never bound its runs (audit in mosaic `requirements/fae-code-to-model-audit-2026-11-06.md`, row control 31). FAE-SCH-016 to -023 below belong together.
>
> **FAE-SCH-016 (MUST)** A workflow MAY declare `slot_scope`: `molecule` (the default) or `tree`. A **slot holder** is a running molecule of a `molecule`-scoped workflow, or a running run tree (FAE-SCH-024) whose root's workflow is `tree`-scoped. A molecule runs while at least one of its tasks that is not a member of a `tree`-scoped run tree is in a stage or Blocked; a run tree runs while any of its tasks is in a stage or Blocked. A slot holder MUST hold a **workflow slot** in the host lease ledger (FAE-HOST-031) while it runs, and acquiring one MUST be the only way a slot holder starts running: no turn, host command, VCS step or external effect may be dispatched for a task whose slot holder holds no slot. This MUST hold on every path that moves such a task into a stage: release from Pending (including a carried re-admission, FAE-WF-086), restart or resume of an Escalated task (FAE-OPS-010), ResumeAtStage (FAE-OPS-035), migration (FAE-WF-093), admission of a detached child into a tree that is not running (FAE-WF-102), and any later path. A slot MUST be acquired before the checkpoint that makes its holder's first task dispatchable and released after the checkpoint in which the holder stops running; within one round the daemon MUST release any slot whose holder is not running and has no release in progress. The ledger is the sole authority for slots (FAE-GEN-011); the plane records each acquisition and release as an audit row and an event. Prophet sessions (FAE-OPS-032), loop schedules (FAE-RT-014), controller operations and daemon-internal operations neither need nor hold slots.
> - *Rationale:* operator ruling (2026-10-30). A molecule is the right unit when many tasks share one composed workflow; a run tree is the right unit when one template revision is run many times as separate runs, as mosaic's generation pipeline is. One acquisition point closes every path; today FAE-SCH-007's molecule limits are checked only on release from Pending.
> - *Acceptance:* tests: (a) with no slot free, a Pending task whose holder holds none is not released and its worktree is not moved; (b) a restart of the only Escalated task of a holder without a slot is recorded and audited, but nothing is dispatched until a slot is acquired; (c) two roots of one `tree`-scoped revision need two slots, and two tasks of one `molecule`-scoped revision need one; (d) a detached child admitted under a Done root of a `tree`-scoped workflow acquires a slot before its first dispatch; (e) a fault injected between acquisition and the release checkpoint leaves no slot held after the next round; (f) a Prophet chat and a loop iteration run with no slot free; (g) a search finds one slot-acquisition function, reached from every path in (a) to (d).
>
> **FAE-SCH-017 (MUST)** Role capacity MUST stay with each workflow. A task released into a slot holder that holds a slot MUST NOT need a slot of its own; its concurrency MUST be bounded only by its role caps and their scope (FAE-SCH-005, -006, -024), the tree session limit (FAE-SCH-025), provider pools (FAE-SCH-015), tasks in flight (FAE-SCH-004) and launch admission (§14.1). A slot MUST NOT grant, pool, raise or lower any role cap, and the host workflow limit MUST NOT be applied to role slots, tasks or sessions.
> - *Rationale:* operator ruling (2026-10-30): role caps are workflow-specific. The host limit bounds how many workflows run; each workflow's caps bound how much it does.
> - *Acceptance:* tests: (a) with the host workflow limit at 1 and one running molecule whose workflow declares coder cap 4, four of its Pending tasks run four coder turns at once; (b) with the limit at 1 and one running `tree`-scoped tree, its 8 parallel FanOut branches run up to its caps; (c) changing the requested limit leaves every role cap and role-slot lease unchanged.
>
> **FAE-SCH-018 (MUST)** A slot holder MAY acquire a slot only while the slots held on the host are fewer than the **host workflow limit**: the smaller of the requested limit and the host ceiling (FAE-SCH-019). Each instance sharing the ledger MAY register one request (FAE-CLI-044); the requested limit is the smallest request of a live instance, and −1 (the default) registers none, so that the host ceiling alone applies. FAE-SCH-007's instance and per-workflow molecule limits MUST be checked at the same point for `molecule`-scoped holders. A change of request, or a fall in the host ceiling, MUST affect only later acquisitions: no running holder may be stopped, parked or signalled because the count exceeds the limit.
> - *Rationale:* a controller or the composer may suggest how many workflows run, but measured capacity always wins. The pane ledger already combines per-instance limits this way.
> - *Acceptance:* tests: (a) a request of 8 against a ceiling of 3: three holders acquire slots and the fourth waits with `host_capacity`; (b) a request of 2 against a ceiling of 5: the third waits with `workflow_limit`; (c) lowering the request from 4 to 2 with four running stops none, and the next acquisition waits until three have stopped; (d) instances requesting 4 and 2 both see 2, and once the second is proved dead the next ledger access yields 4; (e) with no request the limit equals the host ceiling.
>
> **FAE-SCH-019 (MUST)** The **host ceiling** MUST be computed from measured host state and the footprints (FAE-SCH-020) of running slot holders, and nothing may override it: no request, verb, workflow content or authority class may give a slot to a holder the ceiling refuses. Let `M` be memory available from a host sample (FAE-HOST-001) taken in the current round; `S` the effective soft floor (FAE-HOST-002); `F_i` and `P_i` the memory and pane footprint of holder `i`; `U_i` and `L_i` the resident memory and live panes of holder `i`'s sessions as last published to the ledger by its owner, refreshed at least once per staleness bound (default 60 s) while it holds a slot, an older or unmeasured value counting as 0; `H = M − S − Σ max(0, F_i − U_i)`; and, under a pane adapter, `H_p = pane limit − live panes − Σ max(0, P_i − L_i)`. A holder with footprint `(F, P)` MAY acquire a slot only when memory available was read, `H ≥ F`, `H_p ≥ P` and no pressure hold (FAE-HOST-005, the one launch admission uses) is in force. Unreadable pressure or load proceeds on the memory and pane terms and records the gap (FAE-HOST-004); unreadable memory available, an unreadable ledger, or a ledger lock not obtained within a bound (default 1 s) MUST refuse every acquisition. Each acquisition MUST be atomic under the ledger's cross-process lock and MUST add its footprint to the committed sums before any later decision, in this round or by another instance, and MUST NOT wait, back off or poll inside the drive (FAE-WF-032). The reported host ceiling MUST be the slots held plus `min(⌊H ÷ F_ref⌋, ⌊H_p ÷ P_ref⌋)`, where `(F_ref, P_ref)` is the footprint of the latest revision of the instance's default workflow.
> - *Rationale:* whether the host can sustain one more workflow is a question about commitments, not only about the memory free now: a workflow that has just started uses little of what it will need. Counting every running holder's unused footprint stops a burst of spawns from passing the check together and breaching the floor later, the failure FAE-HOST-006 fixed for launches.
> - *Acceptance:* tests with injected telemetry: (a) `M` 20 GiB, `S` 6 GiB, nothing running and every footprint 4 GiB: three holders acquire slots in one round and the fourth waits with `host_capacity`; (b) one holder with `F` 4 GiB and `U` 1 GiB lowers `H` by exactly 3 GiB, and by 4 GiB once its `U` is stale; (c) a pressure hold entered by a launch refuses acquisition until two clear readings 30 s apart; (d) unreadable memory available refuses acquisition with `telemetry_gap`, while unreadable pressure with ample memory admits and records the gap; (e) a ledger held locked by another process for 5 s holds back that round's spawns with `host_ledger` and the round completes; (f) two instances racing for the last uncommitted 4 GiB: exactly one acquires, in every run of a repeated test, and a bounded model of concurrent acquisition never exceeds the limit (FAE-VER-009); (g) firing controls (FAE-VER-001): a request, a workflow declaration and an operator verb each fail to obtain a slot the ceiling refuses.
>
> **FAE-SCH-020 (MUST)** Every workflow revision MUST have a footprint derived from its content by the validator (FAE-WF-090): a session count `n`, a memory footprint of `n` times the per-session reservation of FAE-HOST-006 in force at acquisition (default 0.7 GiB), and, under a pane adapter, a pane footprint of `n`. For a `molecule`-scoped workflow, `n` is the largest number of distinct agent sessions on any path from the start stage to Done (each AgentTurn role once, each reviewer once, a Loop body once, a SubWorkflow stage adding its pinned child's `n`). For a `tree`-scoped workflow, `n` is the tree's peak concurrent sessions: the root's `max_sessions` (FAE-SCH-025) when set; otherwise a bound derived from the content, where a parallel FanOut counts its `max_branches` times its branch region's peak, a SubWorkflow counts `max_children_per_visit` times its child's peak, and self-recursion is bounded by `recursion.max_descendants` (FAE-WF-100); a `tree`-scoped workflow whose peak has no such bound MUST declare `max_sessions` or a session count, or fail validation. Content MAY declare a larger session count; a smaller one than the derived count MUST be refused with its document path (FAE-WF-070). The count is pinned with the revision, and validation and simulation (FAE-WF-091, FAE-WF-105) MUST report the derived and effective footprint.
> - *Rationale:* the composer authors templates, so a template's cost to the host must be known before any of it runs, and an author's claim must not undercut what the graph says. A tree's cost is its peak concurrency, which dynamic FanOut width and recursion make unbounded unless a bound is declared.
> - *Acceptance:* tests: (a) AgentTurn → Gate → Done, `molecule`-scoped, has `n` = 1; (b) an AgentTurn then a Review of four single-reviewer dimensions has `n` = 5; (c) a `tree`-scoped root with `max_sessions: 6` has `n` = 6; (d) a `tree`-scoped workflow with a parallel FanOut of `max_branches: 8` over a one-session branch and no `max_sessions` has `n` = 8; (e) a self-recursive `tree`-scoped workflow without `max_descendants` or `max_sessions` fails validation; (f) declaring 2 on a graph that derives 5 is refused with its path.
>
> **FAE-SCH-021 (MUST)** A spawn that cannot acquire a slot MUST fail closed without failing its task. (1) A Pending task stays Pending; a task that a restart, resume or migration placed in a stage stays there with park reason WorkflowSlot (Appendix B). (2) The round records the task as withheld with exactly one reason: `workflow_limit`, `host_capacity`, `host_ledger`, `telemetry_gap` or `molecule_limit` (FAE-SCH-007). (3) While a holder waits, the drive MUST decide again within a bound (default 30 s), and whenever this instance releases a slot or its request changes, and MUST NOT poll while nothing waits (FAE-ARC-002). (4) Waiting MUST NOT consume an attempt or any bounded-recovery budget (FAE-ROB-017), MUST NOT escalate except under (7), and MUST NOT hold a role slot, pool lease, pane or heavy-op unit or move the task's worktree. (5) Holders waiting for host capacity MUST be served in ready-set order (FAE-SCH-003) across every instance sharing the ledger; none may take uncommitted capacity that a waiter ahead of it is waiting for. (6) A holder withheld only by its own workflow's molecule limit MUST NOT hold back others. (7) A holder whose footprint exceeds the host's total memory less the soft floor, or the pane limit, MUST escalate its tasks with kind FootprintExceedsHost instead of waiting, and MUST NOT hold back other waiters. (8) The reply of an admission, restart, resume or migration that leaves a task waiting MUST say so and name the reason. (9) Each holder MUST produce one event when it starts waiting, one when its reason changes and one when it acquires a slot (FAE-OBS-006); a wait longer than a bound (default 30 min) MUST raise one alert (FAE-OBS-005) naming the holder, its footprint, the reason, uncommitted memory and panes, and the slot holders.
> - *Rationale:* a workflow the host cannot sustain should not start, but the work is still wanted, so it waits where the operator and the composer can see why. Serving waiters in order, as the heavy-op pool does (FAE-SCH-011), keeps a large workflow from starving behind small ones.
> - *Acceptance:* tests: (a) a waiting task's attempt count and budgets are unchanged after 100 rounds; (b) withheld counts for each new reason appear in status and metrics; (c) with capacity for one small holder and a large holder waiting ahead of it, the small one does not acquire a slot, on one instance and across two; (d) with no other event, a waiting holder acquires a slot within 30 s of capacity becoming available; (e) a holder needing 80 GiB on a 64 GiB host escalates with FootprintExceedsHost and does not block a 4 GiB waiter behind it; (f) a 30 min wait raises exactly one alert, and one wait produces exactly one start and one acquisition event.
>
> **FAE-SCH-022 (MUST)** SubWorkflow children and detached children MUST run on the slot of the holder they belong to: a child in a `tree`-scoped tree on the tree's slot, and a child of a `molecule`-scoped parent on its parent's molecule's slot, whatever the child's own workflow and `slot_scope`. A child needs no slot of its own, does not count toward the host workflow limit, and its sessions count in its holder's measured usage; a holder's footprint includes the children it can start (FAE-SCH-020).
> - *Rationale:* a parent keeps its slot while it waits for its child. If children needed slots of their own, a host at its limit with every slot held by a waiting parent would deadlock. The default workflow's Normal-mode code review is a SubWorkflow, and mosaic's zoom is a recursive one.
> - *Acceptance:* tests: (a) with the host workflow limit at 1, a parent that reaches a SubWorkflow stage runs its child to Done and then reaches Done; (b) with the limit at N, N `tree`-scoped roots each recursing to depth 3 all reach Done; (c) a child whose own workflow is `tree`-scoped, started by a `molecule`-scoped parent, acquires no slot.
>
> **FAE-SCH-023 (MUST)** At startup, before its first dispatch, the daemon MUST re-establish a slot for each of its running slot holders, with an audit row each, exempt from the requested limit and the host ceiling, and MUST NOT dispatch for a holder until its slot is re-established. Slots and requests whose owner is proved dead MUST be reclaimed, with an audit entry, on the next ledger access by any instance (FAE-ROB-014, FAE-HOST-031). Re-establishment MAY leave the host above its limit; later acquisitions then wait until the count falls below it. A request MUST survive restart and MUST be registered again before the first acquisition.
> - *Rationale:* running work continues across a crash or a roll, as leases and pane reservations do, and capacity freed by a dead owner returns to the host without operator action.
> - *Acceptance:* tests: (a) SIGKILL a daemon holding three slots, fill the host from another instance, then restart the first daemon: its three holders resume with re-established slots, status reports the host above its limit, and nothing new starts anywhere until the count is below the limit; (b) a dead owner's slots and request are cleared on another instance's next ledger access; (c) a request of 3 is in force after a restart, before any acquisition.

> Proposed 2026-11-07 (mosaic code-to-model re-audit), amendment to FAE-SCH-018 (A1): its first sentence is replaced by: "A slot holder MAY acquire a slot only while the slots held on the host are fewer than the requested limit, when one is in force, and it passes FAE-SCH-019's test for its own footprint `(F, P)`." The host ceiling of FAE-SCH-019, computed from the reference footprint `(F_ref, P_ref)`, is a reported estimate and MUST NOT refuse a holder that passes that test. `fae workflow-limit` labels it an estimate. As written, "the smaller of the requested limit and the host ceiling" makes `F_ref` a gate. fae's default workflow derives 10 sessions (7 GiB), so with 5 GiB uncommitted a 4-session (2.8 GiB) tree is refused `host_capacity`, and with 14.5 GiB only 3 of 5 fit (`scheduler/workflow_slots.go:285`, `:474`; probe P6; daemon probe D1 refused a 1-session root with 6.8 GiB uncommitted).
> - *Acceptance:* tests: (a) reference 10 sessions and 5 GiB uncommitted: a 4-session holder acquires; (b) 14.5 GiB uncommitted: five 4-session holders acquire and the sixth waits with `host_capacity`; (c) the reported ceiling is unchanged by (a) and (b).

> Proposed 2026-11-07 (mosaic code-to-model re-audit), amendment to FAE-SCH-020 (A2):
> 1. A SubWorkflow stage counts one child unless it declares `items_from` (FAE-WF-140). In that case it counts `max_children_per_visit` children, or 16 without a `recursion` block.
> 2. The SubWorkflow stages in the branches of one `parallel` FanOut visit together count at most `max_children_per_visit` children, because the visit shares one child budget. They do not count the FanOut's width times that budget.
> 3. A declared `session_count` is compared with the content-derived count, never with an admission's `max_sessions`. The effective count of a root admitted with `max_sessions` is the larger of `max_sessions` and the declared `session_count`.
>
> Today every SubWorkflow with an `input_map` counts 16 children (`workflow/footprint.go:132-144`): a one-child wrapper of mosaic's light pipeline derives 1,360 instead of 85, and `zoom.yaml` derives 68,640. A declared `session_count: 6` refuses an admission with `max_sessions: 8` ("declared 6 is below derived 8", `:44-48`).
> - *Acceptance:* tests: (a) a SubWorkflow with an `input_map` and no `items_from` counts its child once; (b) a parallel FanOut of width 8 over a SubWorkflow with `max_children_per_visit: 8` counts 8 children; (c) `session_count: 6` with an admission's `max_sessions: 8` is admitted with an effective count of 8.

> Proposed 2026-11-07 (mosaic code-to-model re-audit). Source: fae `daemon/drive.go:488-512`, `daemon/workflow_slots.go:343-347`; probe `reaudit/capacity/daemonp` D3 and `bench_test.go`.
>
> **FAE-SCH-026 (MUST)** Once a slot holder holds a slot, dispatching work for its members MUST NOT need a ledger access. The daemon MUST decide from its own record of granted slots, re-established at startup per FAE-SCH-023. It MUST touch the ledger for a held holder only to reprice it (a footprint increase per FAE-SCH-027, a migration, a raised `max_sessions`), to publish usage and to release it. An unreadable ledger, a lock not obtained within the bound, a pressure hold or a telemetry gap MUST refuse only acquisitions and increases. None of them may withhold a stage of a holder that holds its slot. A drive round MUST NOT write the ledger for a holder whose ledger record does not change.
> - *Rationale:* FAE-SCH-018 forbids stopping running holders, but today every running task re-acquires its slot every round, with a cross-process flock and an fsync'd ledger write of about 26 ms each, plus a plane read of the default workflow. A ledger held by another instance for 5 s withheld a running tree's next stage for 5.1 s (`host_ledger`). Mosaic runs several trees per host and shares the ledger with other instances.
> - *Acceptance:* tests: (a) a holder whose next stage becomes ready while another process holds the ledger lock for 5 s dispatches that stage in the next round; (b) with 30 running tasks of held holders and no waiters, a drive round makes no ledger write (`state.json` unchanged); (c) with memory available unreadable, held holders keep dispatching while a new root waits with `telemetry_gap`; (d) a raised `max_sessions` that the ledger refuses leaves the tree running at its old limit.

> Proposed 2026-11-07 (mosaic code-to-model third audit), amendment to FAE-SCH-026: publishing usage MUST be one ledger transaction per instance per publication covering all its holders. It MUST NOT be made while holding the lock that serialises the drive and mutating RPCs. A publication that cannot obtain the ledger lock within its bound MUST be skipped and retried at the next interval, never retried per holder. While a ledger stall makes an acquisition fail, the drive MUST NOT spend the lock bound again in the same round, nor in following rounds before the stall's next retry, so held holders keep their round rate. Acceptance adds: (e) 10 held trees, another process holding the ledger lock for 40 s across a usage publication: no held tree's next stage is delayed more than one round, and an `Admit` during the stall returns in under 1 s (FAE-ROB-021). Source: fae `daemon/workflow_slots.go:808-815`, `:856-870`, `:466-478`; probe `reaudit2/capacity/daemonp/lockusage_test.go` (D8: next stage delayed 11.2 s and 12.3 s at +29 s; `Admit` timed out at 5 s twice; the held tree's transitions fell from 89 to 25 in 40 s).

> Proposed 2026-11-07 (mosaic code-to-model re-audit). Source: fae `docs/requirements-followup-2026-11-06.md` (late scheduler review: "Dynamic detached-cost admission still needs a pricing or refusal rule"); `daemon/workflow_slots.go:93-150` (a child is priced at its root's footprint), `daemon/tree_admission.go:255-257` (no descendant bound without a root `recursion` block); probe `reaudit/capacity/daemonp` D1; mosaic `.claude/skills/mosaic-generate/compose/zoom.yaml:2-8`, `cmd/mosaic/compose.go:30-34`.
>
> **FAE-SCH-027 (MUST)** A detached child (FAE-WF-102) MUST NOT run on commitment its holder never acquired. When the child's tree root has `max_sessions` (FAE-SCH-025), the holder's footprint is that limit and already bounds the child, so the child needs nothing more. Otherwise the child's own effective footprint MUST be added to its holder's committed footprint before the child's first dispatch. The footprint is the one FAE-SCH-020 derives from the child's pinned revision under the holder's slot scope, its SubWorkflow descendants included. The addition goes through the one acquisition point of FAE-SCH-016, as an increase that:
> - is atomic under the ledger lock;
> - is priced by FAE-SCH-019 against uncommitted memory and panes;
> - is served in ready-set order with the other waiters (FAE-SCH-021).
>
> Until the increase is granted, the child waits as a spawn does (Pending, or parked WorkflowSlot), and its holder and the holder's other members keep running. The increase MUST be released in the round after the child and all its descendants are terminal or Escalated. A child whose own footprint exceeds the host's total memory less the soft floor, or the pane limit, MUST escalate with FootprintExceedsHost without affecting its holder. The same rule applies to a detached child of a `molecule`-scoped holder. `fae workflow-limit` MUST show a holder's committed footprint as the root's footprint plus each priced child's, listed by child.
> - *Rationale:* a holder's footprint covers only what its content can start, but detached children are admitted from outside the content, in any number when the root has no `recursion` block. In the probe, six detached children ran 7 sessions on a 1-session (0.7 GiB) commitment while an unrelated root waited for host capacity. Mosaic zooms and click-to-refine swarms are detached children. Pricing each child at admission keeps FAE-SCH-019's commitment sum true without refusing work the host can carry.
> - *Acceptance:* tests: (a) a root without `max_sessions` deriving 1 session, with 6 detached 1-session children and uncommitted memory for 3 sessions: the root keeps running, at most 2 children run, and the rest wait with `host_capacity`; (b) the same root with `max_sessions: 4`: the children need no increase and at most 4 sessions run at once; (c) when a priced child reaches Done, its increase is released in the next round and a waiting child acquires; (d) a child needing 80 GiB on a 64 GiB host escalates with FootprintExceedsHost and its root is unaffected; (e) two instances pricing children against the last uncommitted memory never commit more than it, in every run of a repeated test.

> Proposed 2026-11-07 (mosaic code-to-model third audit), amendment to FAE-SCH-027: "served in ready-set order with the other waiters" applies only to waiters for host capacity. A waiter held back only by the requested limit (`workflow_limit`) or by its molecule limit (FAE-SCH-007) MUST NOT hold back an increase of a holder that already holds a slot, because an increase adds no slot. A refusal caused only by ready-set order MUST NOT be reported as `host_capacity` when uncommitted memory and panes would cover the candidate. The release in the round after a child ends MUST NOT depend on its sibling children ending. Acceptance adds: (f) limit 1, root A holding, root B waiting `workflow_limit`, ample memory: A's detached child acquires its increase in the next round; (g) two priced children running, one reaching Done: its increase is released in the next round while the other still runs, and a waiting child acquires. Source: fae `scheduler/workflow_slots.go:550-555` (every earlier waiter but molecule-limit ones blocks an increase); `daemon/workflow_slots.go:210-217` with `:575-608` (a running child's cached spec keeps finished siblings funded); probes `reaudit2/capacity/daemonp/more_test.go` D5(c) and D6; fae's `TestWorkflowSlotsNativeDetachedIncreasesReleaseForNextChild` fails 3 of 3 runs on the current tree.

> Proposed 2026-11-07 (mosaic code-to-model re-audit). Source: fae `workflow/footprint.go:103-199` (the tree peak ignores role caps); mosaic `.claude/skills/mosaic-generate/compose/ontology-pipeline.yaml:39-50`; probe `reaudit/capacity/footprint_test.go`.
>
> **FAE-SCH-028 (SHOULD)** For a `tree`-scoped workflow without `max_sessions`, the derived session count SHOULD be the smaller of the graph-derived peak and the role-cap bound of the roles its pinned revisions declare. The role-cap bound is the sum of each `tree`- or `workflow`-scoped role's cap counted once, plus each `task`-scoped role's cap times the most tasks the tree can hold (1 plus `recursion.max_descendants`, or the children its SubWorkflow stages can start). Validation and simulation SHOULD report both numbers.
> - *Rationale:* mosaic's full pipeline derives 4,290 sessions from its graph, but its caps allow at most 27 tree-scoped sessions plus 2 coordinators per task (93 in all). The graph count alone makes every unbounded mosaic root escalate with FootprintExceedsHost on any host, and it overstates what the host must reserve.
> - *Acceptance:* tests: (a) a tree with a parallel FanOut of `max_branches: 32` over a role with `cap: 8, cap_scope: tree` derives 8; (b) a self-recursive tree with `max_descendants: 32` and a `task`-scoped role of cap 2 contributes 66; (c) the graph count still wins when it is smaller.

> Proposed 2026-11-07 (mosaic code-to-model third audit). Source: fae `daemon/workflow_slots.go:94-238` (`slotSpec` decodes every task snapshot, `:135-142`, `:200-208`), called per running task in dispatch (`daemon/drive.go:491`) and 2 to 3 times per non-terminal task in `reconcileWorkflowSlots` (`daemon/workflow_slots.go:576-629`), under the daemon lock (`daemon/drive_isolation.go:188-199`); probe `reaudit2/capacity/daemonp/more_test.go` D4 (a 12-stage tree: 1.27 s alone, 103 to 112 s beside 33 running trees with 91 KB documents; profile: `slotSpec` 66% of CPU) and `decode_test.go` (0.65 ms per mosaic full-pipeline snapshot).
>
> **FAE-SCH-029 (MUST)** The daemon's per-round slot bookkeeping (FAE-SCH-016, -022, -026, -027) MUST cost time linear in the instance's non-terminal tasks, and MUST NOT decode or re-derive a task's checkpoint for each other task. Holder identity, the holder's covered footprint and each holder's live detached children MUST be kept in memory, updated when a task is admitted, changes state or ends, and rebuilt once at startup. A round in which no task changed state MUST make no footprint derivation. Terminal tasks MUST NOT enter any per-round slot computation.
> - *Rationale:* replacing per-round ledger writes (FAE-SCH-026) with a per-member specification that decodes every snapshot made rounds quadratic. Beside 33 running trees with documents the size of mosaic's full pipeline, each stage transition of another tree waited about 9 s, and the daemon lock was held throughout. Terminal tasks of earlier runs add to every round.
> - *Acceptance:* tests: (a) 33 running held trees whose documents are 90 KB and 300 terminal tasks: a 12-stage scripted tree finishes within twice its time on an otherwise idle instance; (b) a round with 33 running tasks and no state change decodes no checkpoint and calls no footprint derivation (counted by instrumentation); (c) after a restart the in-memory holder records match the ledger before the first dispatch.

---

## 6. The agent runtime adapter, routes and provider health

**FAE-RT-001 (MUST)** The runtime adapter interface MUST support: launching a session for a binding with a working directory, a tool profile and an environment policy; delivering a brief; observing readiness, activity and completion signals; reading the handoff file; capturing a bounded output tail for diagnosis; locating the session's own log directory; retiring the session and its whole process tree; and listing live sessions with their owners.
- *Rationale:* the engine needs these and nothing provider-specific.
- *Acceptance:* the scripted adapter and the multiplexer adapter both pass one shared conformance suite.

**FAE-RT-002 (MUST)** A session launch MUST pass, in order: the host hard limits (Section 14), a pane reservation against the live count, the memory reservation (FAE-HOST-006) and the PSI and load admission. Only then is the session started. A refusal at any step MUST release everything reserved by earlier steps and park the action.
- *Rationale:* every pane launch must be gated; partial reservations leak.
- *Acceptance:* a test refusing at each step asserts no reservation remains.

**FAE-RT-003 (MUST)** The multiplexer adapter MUST create or adopt only a dedicated workspace for the instance (named by the instance), MUST ignore any ambient multiplexer workspace variable, and MUST refuse to adopt a workspace holding panes it did not create.
- *Rationale:* a daemon adopting the operator's own workspace could close the operator's panes.
- *Acceptance:* tests for the ambient-variable case and the foreign-panes refusal.

**FAE-RT-004 (MUST)** Agent start MUST be bounded (default 180 s per attempt, 3 attempts). A provable non-execution (the pane never hosted a live agent, or the prompt was never acknowledged) MUST be recovered by relaunching, without consuming the task's attempt budget, and MUST NOT abort the drive.
- *Rationale:* an agentless pane once aborted a whole run.
- *Acceptance:* a scripted never-ready session is relaunched up to the bound, then the task parks with ProviderUnavailable recorded against that binding.

**FAE-RT-005 (MUST)** Stall detection MUST use positive liveness signals (process CPU accrual, session activity events, handoff file growth) rather than the absence of terminal output. A stalled turn MUST be rotated and retried within a bound (default 2 retries) and then escalated with a typed kind.
- *Rationale:* flat terminal output is not a stall; a busy agent can be silent for minutes.
- *Acceptance:* a test with a silent but CPU-accruing session does not trip the stall detector; a session with zero CPU delta over the window does.

**FAE-RT-006 (MUST)** The framework MUST read the handoff file and never the terminal scrape. When the handoff file is absent at collection, the error MUST be a distinct typed error that names the absolute expected path, the role and the dispatch id, and, when the session log yields a provider cause for the dispatch window, that cause. An id-less payload that is present MUST keep its own distinct message. A fallback to the pane scrape MUST NOT be used.
- *Rationale:* "role returned no id and no error" was reported when the role returned nothing, because a model throttle killed the turn; and an echoed prompt can contain a JSON block that parses as an id-less payload.
- *Acceptance:* tests: absent file yields the typed error with path, role and dispatch id; the same with a planted throttle in the session log adds the cause; a present id-less payload keeps its message byte for byte.

**FAE-RT-007 (MUST)** Fae MUST have exactly one session-log reader module with a documented interface. It MUST discover a session's log directory at launch (from the agent CLI's own announcement where it prints one, else by launch timestamp) and MUST classify structured error signals from the log: HTTP 503 or "model temporarily unavailable" as unavailable; throttle errors (for example a model-throttle error, HTTP 429, "experiencing high traffic") as rate-limited; authentication failures as auth-failure. Both provider health and the missing-handoff error MUST use this reader.
- *Rationale:* a throttled turn left provider health reading perfectly healthy because only the pane scrape was read.
- *Acceptance:* tests with planted session logs for each class; a code search finds one reader.

**FAE-RT-008 (MUST)** Every turn outcome MUST be classified into one of: ok, rate-limited, unavailable (down), auth-failure, model-api-error, timeout, malformed-output. A classifier reading free text MUST ignore text inside a structured, valid payload that merely quotes those words as work content.
- *Rationale:* a reviewer whose work text quoted "503" must not demote its route.
- *Acceptance:* tests for each class and for the quoted-text case.

**FAE-RT-009 (MUST)** Provider health MUST be recorded per (provider, model) on every non-ok observation and on ok. Defaults: the score starts at 100 with a floor of 0; a binding is eligible while its score is at least 50; penalties are 60 for rate-limited, 100 for unavailable or auth-failure, and 40 for timeout, model-api-error or malformed-output; scores heal by decay with a 3600 s half-life; after a rate-limited failure, an ok from that binding does not restore its score for a hold of 3600 s. Each non-ok observation MUST produce exactly one health row update and exactly one operator-visible log line and event: never a line without a row, and never a row without a line.
- *Rationale:* availability-first failover with honest reporting.
- *Acceptance:* tests: one throttle makes the binding ineligible and the next dispatch selects the next route; a quoted-text payload does not demote; the row and the line are paired.

**FAE-RT-010 (MUST)** Route selection for each dispatch MUST pick the first eligible binding in the role's ordered route. When none is eligible the action MUST park (all-routes-unavailable) without exiting the drive and without consuming an attempt. Health MUST affect new dispatches only, never evidence already produced. The binding used MUST be recorded in each handoff's provenance.
- *Rationale:* fallbacks exist so work always continues.
- *Acceptance:* a test throttling every binding parks the task, and when one heals it resumes.

**FAE-RT-011 (MUST)** When a turn finishes and is collected, its task-bound session MUST be retired promptly with its whole process tree, including wrapper, sandbox launcher and helper children and any defunct children. Long-lived workflow-global sessions are exempt. Reaping MUST be idempotent and MUST never kill a session that owns an in-flight turn.
- *Rationale:* 16 finished worker session trees and 78 zombie processes were measured alive, about 10 GiB of memory.
- *Acceptance:* after a scripted 3-task run, no task-bound session process remains and no zombie children of the daemon exist.

**FAE-RT-012 (MUST)** The runtime MUST NEVER close a pane or session that a live run holds as an authoritative role reference, except through the retirement path that first releases that reference.
- *Rationale:* manual cleanup of a live run's pane broke dispatch fatally.
- *Acceptance:* a test removes a pane out from under a live slot; the scheduler reconciles the slot and relaunches rather than failing.

**FAE-RT-013 (MUST)** Generated agent configuration files MUST be regenerated on every launch from the tool profile and the mode, and MUST record inside them the mode and the exact tool grant.
- *Rationale:* hand edits are overwritten; the operator must be able to read what a session could do.
- *Acceptance:* a launch writes a configuration whose recorded grant equals the profile.

**FAE-RT-014 (MUST)** Fae MUST provide a provider-neutral loop capability: a schedule that repeatedly prompts the **same** persistent agent session (context carries across iterations), either self-paced or at an interval, with an open-ended run until stopped, an optional maximum iteration count, and a safety auto-expiry (default 7 days, configurable). Loop iterations MUST NOT pass through the gated workflow pipeline. Each iteration MUST write an audit record.
- *Rationale:* operator: the same function as an interactive agent's loop command, across every supported agent.
- *Acceptance:* a scripted loop of 5 iterations shows one session, 5 prompts, 5 audit records, and expiry stops it.

> Proposed 2026-11-05 (mosaic model-building). Source: mosaic `requirements/fae-model-building-assessment-execution.md` Top blocker 1 and §3.1 FAE-RT-020 and FAE-RT-021, and `fae-model-building-assessment-controller.md` FAE-CTL-101; re-checked in `requirements/fae-recheck-2026-11-05.md`. Resolves the open HOST-011 conflict for roles that use it.
>
> **FAE-RT-015 (MUST)** A role MAY declare `session: agentic` with a tool allowlist drawn from: read a file or a line range, list a directory, search by glob and by regular expression, edit, write, and a restricted shell. The runtime adapter MUST launch the provider with exactly those tools, enforced by the agent CLI's own permission rules where it has them, by the pre-tool hook, and by resolving every tool path physically (no symbolic-link or `..` escape). Reads are confined to the task worktree plus the role's read roots (FAE-RT-016) under the effective read policy (FAE-SEC-008). A granted shell MUST run with the FAE-HOST-011 shims first on its PATH and the pre-tool hook registered. Edits happen in the task worktree during the turn; at collection the adapter MUST compare the worktree with the turn's start, revert every change outside the write scope, and refuse the turn as output-refused naming those paths. The turn still ends in one structured handoff validated against the role's output schema (FAE-WF-034). A role MAY declare a turn budget `{wall_seconds, max_assistant_turns}` that overrides the instance-wide provider timeout for that role; an exhausted budget MUST be recorded as its own outcome, MUST NOT penalise provider health, and MUST take a declared route (resume once with the worktree kept, or a named stage). Tool-less sessions stay the default, and a role without `session: agentic` that announces any tool other than the structured-output tool MUST be refused as today.
> - *Rationale:* model-building agents explore a large source tree, run a checker and iterate for up to 45 minutes inside one turn; a single structured reply with no tools cannot do that, and the pre-tool hook guards nothing while no session can run a command.
> - *Acceptance:* tests on the scripted adapter and the Claude bridge: (a) an agentic turn reads a line range of a 5 MiB file and edits one in-scope file, and the edit lands; (b) a write outside the scope is reverted and the turn refused with the path named; (c) a shell call to the build tool by absolute path is denied by the hook; (d) roles with 45 min and 15 min budgets in one instance are each stopped at their own bound, with provider health unchanged; (e) a tool-less role launches with no tools, byte for byte as today.

> Proposed 2026-11-06 (mosaic code-to-model audit), amendment to FAE-RT-015 (working directory and reachable paths): the provider process of an agentic session MUST run with the canonical task worktree as its working directory (FAE-WF-045 applies to the provider process, not only to the bridge). The agent CLI's own file-tool confinement MUST include exactly the worktree, each read root the role names (FAE-RT-016) and, where granted, the task's output directory (FAE-WF-104) and staging area (FAE-WF-111), and each ancestor's output directory, read-only, so that a child can read what FAE-WF-104 lets it read; for Claude Code that is one `--add-dir` for each beside `--restricted`, with writes outside the task's own writable paths refused by a permission rule. A tool-less session MAY keep an empty private working directory. The adapter MUST pass only tool names the installed CLI accepts, and a launch conformance test MUST run against the installed agent CLI, not only against scripted binaries. Source: mosaic catalog P-03, P-04, D-63, C-24; at proposal (fae tree of 2026-11-06 08:25) the provider process ran in an empty private directory with no `--add-dir`; the working tree has since moved an agentic provider into the worktree (`provider/codex.go:760-763`) and passes `--add-dir` for the output directory and staging area (`provider/claude.go:85-91`), but read roots still get neither `--add-dir` nor a permission rule (`runtime/claude_permissions.go:23-28`), under `--restricted` (`provider/claude.go:84`), which Claude Code 2.1.319's `--help` describes as confining the file tools to the working directories plus `--add-dir`; the only installed-CLI test reads `--help` (`runtime/claude_permissions_test.go:56-83`); the agentic tests drive scripted binaries that write by absolute path (`provider/agentic_test.go:224-238`); audit in mosaic `requirements/fae-code-to-model-audit-2026-11-06.md` (rows agent AE-02, AE-03).
> - *Acceptance:* tests with the installed Claude Code: (a) an agentic turn with `read` reads a line range of a worktree file by its repository-relative path; (b) the same turn reads a file under a declared read root by absolute path, and a write there fails; (c) a file outside the worktree, the roots, the output directory and the staging area is refused by the CLI; (d) a granted shell's first command runs in the worktree; (e) a tool-less turn is unchanged.

> Proposed 2026-11-06 (mosaic code-to-model audit), amendment to FAE-RT-015 (stage budgets and routes): an AgentTurn stage MAY declare its own `turn_budget {wall_seconds, max_assistant_turns, on_exhaustion}`; each bound MAY only tighten its role's, and the stage's `on_exhaustion` overrides the role's. An effective `resume-once` MUST resume once even when the stage has a `failure` route, and a second exhaustion then takes that route. `on_exhaustion` MUST also accept `fail`, which takes the stage's failure route (FAE-WF-112) with `failure.reason == "budget"`, or escalates when the stage has none. An exhausted budget with no declared `on_exhaustion` MUST behave as `fail`, and MUST NOT re-dispatch the same stage under the same-gate bound. Source: mosaic catalog P-02 (300 turns and 45 minutes for a build, 250 turns and 45 minutes for the skeleton), P-69, P-94, D-71, D-72, C-19, C-35, C-36, C-42; today budgets are per role (and per follow-up task) only (`workflow/agentic.go:11-15`), a stage's `failure` route is taken before the role's `on_exhaustion` is read, so `resume-once` is skipped (`engine/engine.go:1550-1555`), and an exhaustion with neither is re-dispatched until the same-reason bound of 3 (`engine/engine.go:1544-1585`, `:2137-2196`, `:814`); audit in mosaic `requirements/fae-code-to-model-audit-2026-11-06.md` (rows agent BT-01, BT-03; workflow W07).
> - *Acceptance:* tests: (a) two stages of one role with 45-minute and 15-minute stage budgets are each stopped at their own bound; (b) a stage budget looser than its role's fails validation; (c) two stages of one role that exhaust their budgets take two different failure routes; (d) an exhausted stage without `on_exhaustion` is not dispatched again.

> Proposed 2026-11-06 (mosaic code-to-model audit), amendment to FAE-RT-015 (counting assistant turns): `max_assistant_turns` MUST count distinct assistant messages of the main session, by message id, excluding messages of subagents or side chains, and the adapter MUST enforce the bound by this count; an agent CLI's own turn limit (Claude Code's `--max-turns`) MAY also be passed as a backstop, and its exhaustion (`error_max_turns`) MUST be recorded as turn-budget-exhausted. When the count passes the bound, the adapter MUST stop the provider's whole process tree within a bound (default 5 s) and record turn-budget-exhausted. A turn that ended with a valid result while its count was within the bound MUST NOT be reclassified as exhausted. Source: mosaic catalog P-70, D-73, C-37, C-38 (stream-json repeats a message once per content block, `internal/compose/executor_claude.go:391-410`); today every `assistant` record is counted, with no message-id dedupe or `parent_tool_use_id` filter (`provider/claude.go:129-142`, `:214-218`), passing the count only sets a flag that later refuses a turn that ended successfully (`provider/codex.go:860-873`), and the hard stop rests on `--max-turns` (`provider/claude.go:98-100`), which Claude Code 2.1.319 accepts and its CLI reference documents but `--help` hides, and whose count is the CLI's, not this one; audit in mosaic `requirements/fae-code-to-model-audit-2026-11-06.md` (row agent BT-02).
> - *Acceptance:* tests with a recorded stream-json transcript: (a) a message split over 3 content-block records counts once; (b) subagent messages are not counted; (c) at bound 5, the sixth message stops the process tree within 5 s; (d) a turn of 5 messages that ends in success is accepted.

> Proposed 2026-11-07 (mosaic code-to-model re-audit), amendment to FAE-RT-015 (2026-11-06 amendment, working directory and reachable paths): the installed-CLI conformance test MUST launch exactly the argv and settings file the adapter produces, `--safe-mode` and `--settings` in the session directory included, and MUST show: a Write into the output directory and staging area succeeds, a Write into the worktree outside the write scope and into a read root is denied by the CLI, and the registered pre-tool hook fires on a shell call and denies an absolute-path shim bypass. Permission rules MUST use the CLI's absolute-path form (for Claude Code 2.1.319, `//abs/path`), and an adapter option that disables hooks (Claude Code's `--safe-mode`) MUST NOT be passed while a requirement rests on the hook. Source: fae `runtime/claude_permissions.go:23-89` (single-slash rules), `provider/claude.go:85` (`--safe-mode`); `claude --help` 2.1.319 ("all customizations (... hooks ...) disabled"); the 2.1.319 binary's `//` rule prefix handling; `provider/installed_claude_filesystem_test.go:44` (no `--settings`).

> Proposed 2026-11-07 (mosaic code-to-model third audit), amendment to FAE-RT-015 (2026-11-07 amendment, installed-CLI conformance): the `//` absolute-path form applies only to file-tool rules (Read, Write, Edit, Glob, Grep). A `Bash(...)` rule MUST carry the command shape verbatim, absolute executables included. The installed-CLI conformance test MUST run for each isolation policy the host can use, the no-view `cli-and-collector` and `cli-runtime-mount-opt-in` policies included. It MUST show an allowed absolute `shell_allow` command *succeeding*, not only refusals, and a read refused by a read-root `deny`. Source: fae `runtime/claude_permissions.go:136-143` (probe M settings: `Bash(//tmp/…/mcheck *)`); `provider/installed_claude_filesystem_test.go:27`, `:84` (skips without bwrap; its only absolute shape `git status` must be refused anyway).

> Proposed 2026-11-05 (mosaic model-building). Source: mosaic `requirements/fae-model-building-assessment-controller.md` FAE-CTL-102; related to open question 7 (multi-workspace instances); re-checked in `requirements/fae-recheck-2026-11-05.md`.
>
> **FAE-RT-016 (MUST)** A target configuration MAY declare **read roots** outside the bound workspace, each an absolute path with an identity (a commit, or a content digest of a tree listing) and optional deny globs. A role names the roots its sessions may read. At admission the daemon MUST verify and record each named root's identity on the task; a root whose identity has changed MUST refuse further dispatch with a typed reason. Roots MUST be read-only to every session and every command; the session's filesystem view MUST NOT allow writes into them. A HostCommand MAY receive a root through a typed placeholder (FAE-WF-035). Provenance MUST record the root identities each turn could read.
> - *Rationale:* a model of another system is built from a pinned snapshot of that system's repository, which lives outside the bound workspace and must never be built or changed.
> - *Acceptance:* tests: (a) a root whose commit differs from the declared one refuses dispatch; (b) a write into a root by a tool or a command fails; (c) TaskHistory shows the root identity each turn read; (d) a symbolic link inside a root that points outside it is refused.

> Proposed 2026-11-05 (mosaic model-building). Source: mosaic `requirements/fae-model-building-assessment-execution.md` Top blocker 2 and §2 B2, B3; `fae-model-building-assessment-controller.md` FAE-CTL-104 (edits above 1 MiB); re-checked in `requirements/fae-recheck-2026-11-05.md`.
>
> **FAE-RT-017 (MUST)** The bounds on files sent inline in a brief (default 1 MiB per file and 4 MiB per turn) MUST be target settings, listed in Appendix A, and MUST apply only to what is sent inline. Read scope and write scope MUST be separate: a task or stage MAY bind files as read-only inputs, which are readable and never editable, so a file need not be in the write scope to be read. An agentic turn (FAE-RT-015) reads files of any size by range through its tools, and a replacement edit on a file above the inline bound MUST apply without the whole file being sent. A file over an inline bound that is not readable by tools MUST still be reported as unavailable, never silently dropped.
> - *Rationale:* merged models run from 3.4 to 22 MB and single fragments to 2 MB, and agents must read source and other areas' accepted files that they must not edit; today the scope files are both the only readable and the only editable files, capped at 1 MiB and 4 MiB.
> - *Acceptance:* tests: (a) a read-only input is readable and an edit to it is refused; (b) a replacement edit on a 2 MiB file applies through an agentic turn; (c) a tool-less turn over the configured inline bound reports the file unavailable; (d) raising the target's bound to 8 MiB sends a 6 MiB set inline.

> Proposed 2026-11-05 (mosaic model-building). Source: mosaic `requirements/fae-model-building-assessment-execution.md` Top blocker 5, §2 B6 and §3.1 FAE-SCH-031 and FAE-SCH-032; re-checked in `requirements/fae-recheck-2026-11-05.md`.
>
> **FAE-RT-018 (MUST)** When a turn's provider reports a usage limit with a reset time (for example a rejected rate-limit event with its reset field), the daemon MUST mark the matching provider pool (FAE-SCH-015), or else the (provider, model) binding, `quota-exhausted until T`. Until T plus a grace (default 60 s): no turn on it is dispatched or probed; every task that would use it parks with reason ProviderQuota; no attempt, visit or recovery budget is charged; and the route-exhaustion escalation MUST NOT fire, its clock starting at T. A turn cut off by the limit MUST keep its work under FAE-RT-019 and MUST be re-dispatched first at T. Without a parsed reset time, the health hold of FAE-RT-009 applies with probe turns spaced by exponential backoff (default cap 1 h). An audited operator verb MUST set or clear the mark, and an audited verb MUST pause and unpause new dispatch by scope (instance, workflow, task set or pool) durably across restart, letting turns in flight finish or checkpoint and charging nothing.
> - *Rationale:* session and weekly usage limits stop multi-hour model-building runs; today each one becomes a health hold, probe turns into a limit that has not reset, ProviderUnavailable escalations after 30 minutes, lost turns, and a manual resume of every task.
> - *Acceptance:* tests: (a) a rejected limit with reset T+2h parks 12 tasks as ProviderQuota, launches no turn before T, escalates none, and dispatches the cut-off turn first at T; (b) without a reset time, probes are spaced 1, 2, 4 minutes and so on; (c) the operator's clear resumes dispatch in the next round; (d) a paused task set with 3 turns in flight lets them finish and dispatches nothing more in that set across a daemon restart.

> Proposed 2026-11-05 (mosaic model-building). Source: mosaic `requirements/fae-model-building-assessment-execution.md` §3.1 FAE-RT-022; re-checked in `requirements/fae-recheck-2026-11-05.md`.
>
> **FAE-RT-019 (MUST)** An agentic turn (FAE-RT-015) MUST record its provider session reference and the worktree's tree hash durably at least once per bound (default 5 min). A turn interrupted by a crash, a quota limit (FAE-RT-018) or a budget MUST resume from its last checkpoint in the same provider session, with its in-scope worktree changes kept, instead of being refused with its edits rolled back and its provider context invalidated. Changes made after the checkpoint MUST be kept only within the write scope, and the resumption MUST be recorded in provenance.
> - *Rationale:* a 45-minute agent turn cut off by a usage limit near its end today loses all its edits and its context, and the work is paid for twice.
> - *Acceptance:* a turn killed 20 minutes in resumes in the same provider session with its in-scope edits present, finishes with one handoff, and its provenance names the checkpoint.

> Proposed 2026-11-07 (mosaic code-to-model re-audit). Source: fae `runtime/direct.go:418`, `provider/session_view.go:66`, `:151-157`, `:191-192`, `provider/claude.go:28-60`, `LIVE.md:547`; mosaic `internal/compose/executor_claude.go:325-328` (the emulator inherits HOME and the operator's OAuth login).
>
> **FAE-RT-023 (MUST)** Inside a session filesystem view, the provider process MUST have a writable private HOME and a writable TMPDIR that are not the session log directory, and the credential source the target configures MUST work as it does outside a view: an environment credential passes unchanged, and a configured provider configuration directory (Claude Code's `CLAUDE_CONFIG_DIR`, Codex's home) MUST be mounted read-write at its configured absolute path, and only that directory, never the operator's own home configuration unless the target names it. Provider session state that a later launch resumes (transcripts for `--resume`, FAE-RT-019, the `resume-once` route of FAE-RT-015, FAE-OPS-034) MUST persist across launches of the same provider session, in the configured directory or in a per-task state directory that the view mounts. The preflight MUST refuse a configured directory the view cannot mount, before launch.
> - *Rationale:* fae's own documented login (`CLAUDE_CONFIG_DIR=~/.claude`) is invisible in today's view. HOME and TMPDIR resolve to the read-only log directory. Each launch's private state is discarded, so no Claude session can authenticate through a config directory or be resumed.
> - *Acceptance:* tests: (a) with `CLAUDE_CONFIG_DIR=/abs/cfg` and a view, the provider sees `/abs/cfg` writable, and neither the operator's `~/.claude` nor other files under the operator's home; (b) inside a view, `$HOME` and `$TMPDIR` are writable and differ from the log directory, and the log directory stays read-only; (c) a turn stopped by its budget resumes with `--resume=<id>` in a new view and the CLI finds the transcript (installed-CLI test, capability-gated); (d) a configured directory outside any mountable path refuses before launch with a typed reason.

> Proposed 2026-11-07 (mosaic code-to-model re-audit). Source: fae `provider/session_view.go:121-131` (only `/usr/{bin,sbin,lib,lib64}`, `/bin`, `/sbin`, `/lib*` and a few `/etc` files are mounted); probe P4c (`/usr/local/go`, `~/go/bin/mosaic` and `~/.cache/go-build` are invisible); mosaic `.claude/skills/mosaic-generate/scripts/mcheck:7-8`, compose `ontology-pipeline.yaml:443-453`.
>
> **FAE-RT-024 (MUST)** A target configuration MAY declare `runtime_mounts: [absolute directories]`: toolchains, prebuilt binaries and the provider CLI's installation. Every session view and HostCommand view of the target MUST bind them read-only at the same path, without copying them, and MUST expose the executable that launches the provider together with what it needs to run. Each mount's identity (a content digest of its listing, or the target's pinned value) MUST be recorded in the turn's provenance. A runtime mount MUST NOT contain or overlap the worktree, a read root, an output or staging directory, the instance root, or a credential directory, and it is never writable. A target MAY also declare writable cache directories, which MUST be private per task.
> - *Rationale:* mosaic's agents run a prebuilt `mosaic` checker or `go`. In a view today, anything outside the OS directories is invisible, so a granted shell cannot run the tools the workflow names.
> - *Acceptance:* tests: (a) a target with `runtime_mounts: [/opt/tool]` runs `/opt/tool/bin/x` from an agentic shell and from a HostCommand inside a view, and a write under `/opt/tool` fails; (b) a mount that overlaps a read root or the instance root is refused at configuration; (c) provenance names each mount's identity; (d) a provider installed outside the OS directories (a node-based CLI under the operator's home) launches in a view when its directory is a runtime mount, and is refused before launch when it is not.

> Proposed 2026-11-07 (mosaic code-to-model re-audit). Source: fae `provider/session_view.go:287-355` (copy refuses every symlink), `daemon/read_roots.go:145`, `:191-207` (admission accepts in-root symlinks); probes P4a and P4b; checkouts `cassandra-5.0.9` (5 in-root symlinks) and `vllm-0.32.0` (8).
>
> **FAE-RT-025 (MUST)** A session view MUST make readable every path that FAE-RT-016 admitted and that the effective read policy allows: an in-root symbolic link whose target resolves inside the same root MUST appear in the view and resolve inside the view. A worktree or read root that no read policy or deny glob narrows for the session MUST be bind-mounted read-only, not copied. A copy, where filtering requires one, MUST be removed when the session ends, and the daemon MUST NOT hold a whole tree's file contents in memory to detect changes. View construction time and bytes MUST be recorded per session.
> - *Rationale:* both of mosaic's benchmark checkouts pass admission but are refused by every session. Copying them costs about 3 s and 140 MB per session, and nothing removes the copies; a mosaic round runs 60 to 100 sessions against one root.
> - *Acceptance:* tests: (a) a root with `redhat/x -> ../x` is admitted, and a session reads `redhat/x` in its view; (b) a root symlink pointing outside the root is refused at admission, as today; (c) a session over a 100 MB root with no deny globs creates no copy (bytes recorded ≈ 0) and cannot write the root; (d) after the session ends, no per-session copy remains; (e) a view over a 100 MB writable output directory does not raise the daemon's resident memory by that size.

---

## 7. Worktrees, merge-reconcile and integration

**FAE-VCS-001 (MUST)** Each task MUST get its worktree at creation from the current integration tip. Worktree creation MUST be idempotent and race-free: concurrent creation for the same task yields one worktree; a resumed task that already has a worktree and branch MUST adopt them, never re-create the branch.
- *Rationale:* a resumed task hitting branch-creation on its existing branch exited the drive twice.
- *Acceptance:* tests for concurrent creation and for adopt-on-resume.

**FAE-VCS-002 (MUST)** No role session may run worktree-management commands (create, remove, prune) on any worktree. The command shims MUST refuse them.
- *Rationale:* an agent deleted its own registered worktree with a help-flag invocation that performed removal.
- *Acceptance:* a shim test refuses each such command, including help variants.

**FAE-VCS-003 (MUST)** A task's worktree MUST start clean at its intended base: when a prior attempt left commits or files, the engine MUST either carry them deliberately as the task's own work (recorded) or refuse with StaleBranchResidue. It MUST NOT silently run RED against a tree that already contains the feature.
- *Rationale:* stale residue made every RED pass and was misdiagnosed as an authoring loop.
- *Acceptance:* a test plants a prior implementation commit; the RED stage refuses with the typed kind.

**FAE-VCS-004 (MUST)** Merge-reconcile MUST bring a task onto the current integration tip by replaying the task's own commits **and** preserving its uncommitted own hunks (production and test). After any rebase or conflict resolution, the task's own diff against the new tip MUST contain every file and every hunk present in its pre-reconcile own diff. A missing file or hunk MUST be a fail-closed refusal naming what was lost. The implementation report MAY be re-derived only for content verified to still match the reviewed file and hunk set.
- *Rationale:* a rebase of uncommitted work plus a conflict resolution once left 702 test lines and zero production hunks, and the system adopted the survivor as truth.
- *Acceptance:* tests: uncommitted production and test hunks both survive a rebase onto a moved tip; a genuine conflict re-dispatches implementation with the paths named; a lost file is refused naming it; a kept file that lost a hunk is refused naming the hunk.

**FAE-VCS-005 (MUST)** When merge-reconcile performed by the framework changes the tree after review, the commit stage MUST re-validate instead of escalating: (a) if the reconcile was clean and the task's own diff is patch-identical to the reviewed diff, re-capture the reviewed snapshot against the new base, re-run only QA, then commit; (b) if it conflicted, re-dispatch implementation with the conflict paths, then the normal review, judgement and QA sequence. A genuine post-review edit to the task's own files by a role MUST still be refused.
- *Rationale:* a moving mainline repeatedly destroyed finished, QA-passed work.
- *Acceptance:* tests for (a), (b) and the genuine-edit refusal.

**FAE-VCS-006 (MUST)** The commit stage MUST stage only the task's declared and reviewed files, and MUST commit the exact reviewed bytes. Automatic formatting at commit MUST be limited to the task's own changed items and MUST NOT change reviewed semantics; if formatting would change reviewed bytes, the commit MUST use the reviewed bytes and record new formatting debt as a follow-up. A no-new-formatting-debt check MUST run before final judgement, not at commit.
- *Rationale:* commit-stage auto-formatting that rewrote reviewed bytes exited the drive three times.
- *Acceptance:* tests: an unrelated file in the worktree is not committed; formatting debt is caught before final judgement.

**FAE-VCS-007 (MUST)** Integrate MUST treat a change that is already upstream as **contained**, not as a conflict. A commit is contained when it is an ancestor of the tip, is patch-equivalent to a commit on the tip, or is blob-equal on every path it touches. A cherry-pick that turns out empty MUST also be classified as contained. Containment MUST record the containing sha.
- *Rationale:* equivalent landings via another path were escalated as conflicts.
- *Acceptance:* tests for each containment form and for the empty cherry-pick.

**FAE-VCS-008 (MUST)** Integration MUST be a serialised queue onto the integration target: dependents see the accumulated output of their satisfied prerequisites. Execution MUST NOT be serialised by shared files. On a real conflict the engine MUST abort, leave the tip unchanged, record the conflicting paths and route the task to merge-reconcile and implementation remediation, re-running every gate on the integrated result. Unverified merged code MUST never land.
- *Rationale:* operator ruling.
- *Acceptance:* a test with two tasks editing the same lines integrates the first and routes the second to remediation; the second lands only after its gates re-run.

**FAE-VCS-009 (MUST)** A skipped integration MUST always leave a next action. When a task's own commit is not on the verified tip, the engine MUST take exactly one recorded disposition: re-verify against the current tip and integrate; route back to a role with the reason and paths; or escalate with a typed reason. A task MUST NEVER rest in the committing stage with no lease and no owner. A detector MUST surface any task resting in a commit or integrate stage with no lease beyond a bound (default 10 min).
- *Rationale:* a silent skip stranded a finished task in committing for over two hours and blocked every rolling restart.
- *Acceptance:* tests: the skip produces one of the three dispositions; the detector fires on a planted stuck row and is silent otherwise; the disposition is idempotent across a restart.

**FAE-VCS-010 (MUST)** In Local mode, Fae MUST integrate onto a per-instance integration branch and MUST NOT move the operator's branches. Landing onto the operator's main branch MUST be the explicit landing operation of Section 12.4.
- *Rationale:* Fae must not merge into the operator's branch implicitly.
- *Acceptance:* after a Local run, the operator's main branch is unchanged until a landing command runs.

**FAE-VCS-011 (MUST)** The integration branch's local reference in the bound workspace MUST be fast-forwarded to the landed tip only when the current reference is an ancestor of the tip; otherwise Fae MUST alert and leave it unchanged.
- *Rationale:* a stale local reference made every comparison inside the workspace measure against a weeks-old tree.
- *Acceptance:* tests for fast-forward and for the non-fast-forward alert.

> Proposed 2026-11-05 (mosaic model-building). Source: mosaic `requirements/fae-model-building-assessment-controller.md` FAE-CTL-109 (commit identity); re-checked in `requirements/fae-recheck-2026-11-05.md`.
>
> **FAE-VCS-012 (MUST)** A target configuration MAY declare `commit_identity: {name, email}`. VcsCommit, Integrate and every landing (Section 12.4) MUST then author and commit as that identity, in place of Fae's fixed identity; Fae's own identity MAY be added as a co-author trailer. Every commit MUST also pass FAE-SEC-009's content policy before it is written.
> - *Rationale:* model-building commits go to the operator's own repository under the operator's identity, and a fixed tool identity misattributes them.
> - *Acceptance:* tests: (a) a commit and a landing on a target with `commit_identity` have that author and committer; (b) a target without it keeps today's identity.

---

## 8. Robustness requirements

These requirements encode failures observed in sustained operation of the prior system. Each one names the observed failure in its rationale.

### 8.1 The drive survives per-task failures

**FAE-ROB-001 (MUST)** A per-task error MUST NEVER exit the drive. Every condition in which one task cannot proceed MUST record a typed escalation kind (Appendix B) or a park reason **before** returning, and the drive MUST continue with the other tasks. Only a genuine instance-level infrastructure failure (the plane cannot be restored to consistency, the instance lock is lost) may stop the drive, and then the daemon MUST stay up, serve reads, and re-arm the drive with bounded backoff.
- *Rationale:* each per-task escalation kind in the prior system was fixed one at a time after it exited the drive (verifier veto, stale branch residue, dispatch recovery exhausted, scope widen rejected, uncertain dispatch).
- *Acceptance:* a single choke point classifies per-task errors; a test that returns an unclassified per-task error from a stage fails the build's census test; a property test injects a random per-task failure into a 5-task run and the other 4 reach Done.

**FAE-ROB-002 (MUST)** Validation errors in a role's report (no real repository change, malformed test command, schema failure) MUST route the task to remediation or escalation within the stage's bound, never exit the run.
- *Rationale:* report-validation errors once fatally exited the run, forcing a full resume and re-RED.
- *Acceptance:* tests for each validation error class.

**FAE-ROB-003 (MUST)** A verifier or coordinator rejection of a proposed transition MUST NOT re-ask the same transition in a loop. It MUST re-dispatch the unsatisfied stage, or escalate after a bound (default 2 re-queries) with kind CoordinatorVeto.
- *Rationale:* a rejection was re-asked in a tight loop forever.
- *Acceptance:* a scripted always-reject verifier escalates after the bound.

### 8.2 Shutdown, restarts and resume

**FAE-ROB-010 (MUST)** Graceful shutdown (SIGINT, SIGTERM or the Shutdown RPC, all on one path) MUST: stop accepting mutating RPCs immediately; keep read RPCs answering with a shutting-down flag; have the drive observe the shutdown at every round boundary and dispatch no new stage turn; let an in-flight commit or integrate step finish; leave other in-flight turns for resume; record a durable graceful-stop marker listing the in-flight set left; stop supervised children (UI, then Dolt); and complete within a bound (default 120 s after the last commit step), after which it logs the overrun and exits through the normal path so children are still reaped.
- *Rationale:* a daemon stopped accepting connections on SIGINT but kept driving for 33 minutes, launching panes and committing, until it was killed and its Dolt server orphaned.
- *Acceptance:* tests: shutdown mid-round with 3 tasks dispatches nothing new and resolves within the bound; a task at commit finishes first; a status read during drain-down returns shutting-down true; without shutdown the drive keeps draining.

**FAE-ROB-011 (MUST)** A re-dispatch caused by a planned graceful restart (a graceful-stop marker exists for the prior run and the turn was in its in-flight set) MUST NOT consume the task's attempt budget, its dispatch-recovery budget or its same-gate budget. A genuine crash MUST still consume dispatch-recovery budget.
- *Rationale:* every rolling restart charged in-flight tasks a recovery attempt and pushed several to escalation.
- *Acceptance:* tests for a planned roll (no budget change) and a crash (budget decremented).

**FAE-ROB-012 (MUST)** Resume MUST be stage-aware for every stage: a task resumes at its durable stage, consuming any durable completed handoff or verdict for that stage (for example a QA pass proceeds to commit), and re-dispatches only turns that had not completed.
- *Rationale:* stage-unaware resume regressed a QA-passed task to Pending and re-drove it from RED.
- *Acceptance:* a property test interrupts a scripted run at every stage boundary and mid-stage; every resumed run reaches the same terminal outcome as the uninterrupted run, with no duplicate dispatch of a completed turn.

**FAE-ROB-013 (MUST)** On resume, evidence recorded against an older base MUST be **re-validated** against the current base rather than escalated: a verdict for a tree that no longer exists is stale and its stage re-runs; valid frozen RED evidence MUST be preferred over any class-driven fallback (a task that declared no-red but has valid frozen RED evidence re-validates on the RED; the no-red gate is consulted only when RED evidence is absent); an interrupted QA run is infrastructure, not a QA failure, and is re-run.
- *Rationale:* eight of nine escalations in one window were crash-recovery artefacts, not code findings; a no-red resume discarded strictly stronger RED evidence and escalated.
- *Acceptance:* tests: stale verdict re-runs; no-red with valid RED re-validates on RED; interrupted QA re-runs without a failure verdict.

**FAE-ROB-014 (MUST)** At startup the daemon MUST reclaim, with an audit row each, every lease, lock and ledger entry whose recorded owner is provably dead (pid absent or process start time differs): role-slot leases, heavy-op leases, pane-accounting entries and locks, and the instance lock. Entries whose owner is alive or whose liveness is ambiguous MUST be kept.
- *Rationale:* leaked leases and an orphaned pane-accounting lock refused the next run, or made the new daemon time out and exit.
- *Acceptance:* tests plant dead-owner and live-owner entries of each kind; only dead ones are reclaimed.

**FAE-ROB-015 (MUST)** A stale role-slot lease (owner task terminal, or owning session gone with no in-flight turn) MUST be reclaimed during operation too, not only at startup, within one round.
- *Rationale:* a stale lease deadlocked a role.
- *Acceptance:* a test terminalises a task holding a slot; the slot is free next round.

**FAE-ROB-016 (MUST)** An **uncertain dispatch** (the engine cannot tell whether a turn ran, for example a collection captured only a launch splash, or no valid JSON) MUST park the task with an uncertainty record, never automatically re-run a possibly completed turn, and surface it to the operator. Resolution MUST be an operator verb (re-dispatch, accept the recorded output, or escalate). An uncertain dispatch MUST NOT exit the drive.
- *Rationale:* re-running a turn that may have created an external effect can duplicate it; exiting the drive stalls everything.
- *Acceptance:* a scripted splash-only collection parks with a record; siblings continue.

**FAE-ROB-017 (MUST)** Each bounded recovery class (stall retries, failover rotations, dispatch recoveries, same-gate repeats) MUST have its own counter, stored durably per task, with defaults: stall retries 2; failover rotations per turn 1; dispatch recoveries 2; same-gate repeats 3.
- *Rationale:* separate budgets prevent one failure class from eating another's.
- *Acceptance:* tests for each bound and its escalation kind.

**FAE-ROB-018 (MUST)** Every transition site MUST stage its full context (attempt number, cause) in the transition record; no field may be filled from a default fallback.
- *Rationale:* most transition sites staged no context, so records showed "attempt 0".
- *Acceptance:* a census test asserts every transition row in a scripted run has a non-default attempt and a cause.

### 8.3 IPC liveness

**FAE-ROB-020 (MUST)** Read RPCs MUST NEVER block the IPC surface: every plane read MUST run off the RPC worker threads (a bounded read pool); every request MUST carry a deadline (default 5 s) and return a deadline-exceeded error instead of hanging; topology and board reads MUST be served from cached snapshots refreshed off the request path with bounded staleness (default at most 10 s) reported as snapshot age in the reply; concurrent identical reads MUST coalesce onto one query; and no daemon-wide mutex may be held across a plane query.
- *Rationale:* back-to-back topology reads pinned workers and pushed status latency to 32 s.
- *Acceptance:* tests with a slow fake plane (2 s per read): with 4 topology reads in flight, status answers in under 1 s; a read past its deadline returns the error and frees its worker; 4 concurrent topology reads cause at most one query; the reply carries snapshot age.

**FAE-ROB-021 (MUST)** Mutating RPCs MUST return as soon as the mutation is durable, and MUST NOT wait for the drive.
- *Rationale:* admission must not block behind a long round.
- *Acceptance:* an admit during a 30 s scripted round returns in under 1 s.

### 8.4 Evidence honesty

**FAE-ROB-030 (MUST)** Test commands run by the engine (engine runs) MUST execute in a **sanitized environment**: every variable that selects the daemon's own runtime (instance, home, control-plane selection, heavy-op permit markers and any variable in a declared runtime-selection prefix) MUST be removed from the child environment unless the declared command sets it explicitly, in which case the explicit value wins. The engine-run record MUST store the environment delta (removed and explicitly set variable names). A failure to control a variable MUST fail closed, naming it. The same sanitization MUST apply to role shells.
- *Rationale:* a test asserting legacy-store behaviour passed in a clean environment and failed under the daemon's leaked control-plane selection, so a genuinely green task was failed by the authoritative engine run.
- *Acceptance:* tests: the planted case passes under the engine run; an agnostic command is byte-identical in argument vector and exit code; an explicit setting wins; the record carries the delta; a sanitization failure refuses to run.

**FAE-ROB-031 (MUST)** Engine-run evidence MUST be bound to a tree hash and MUST prove a fresh build of that tree (for compiled languages, an observed compile step after the last source change), so that a stale binary cannot fake a result.
- *Rationale:* after a file swap that preserved older timestamps, the build tool skipped the rebuild and re-ran a stale binary.
- *Acceptance:* a test that swaps a file with an old timestamp forces a rebuild, or the gate refuses for lack of a compile step.

**FAE-ROB-032 (MUST)** Exit codes MUST be captured from the actual command, not from a wrapper, pipeline tail or subshell.
- *Rationale:* runbooks that end with an echo, or capture output in a subshell, exited 0 whatever failed.
- *Acceptance:* a test with a failing command inside a wrapper records the inner exit code.

**FAE-ROB-033 (MUST)** A run that reports zero executed tests MUST never satisfy a test gate, including the case where all matching tests were skipped or ignored.
- *Rationale:* "0 passed; N ignored" is a compile-only signal, not a pass.
- *Acceptance:* covered by FAE-WF-062 tests.

### 8.5 Live configuration

**FAE-ROB-040 (MUST)** A change to a task's acceptance text, operator context, attempt limit or priority MUST reach the next composed brief and the next scheduling decision without a daemon restart. In-flight turns are unaffected.
- *Rationale:* acceptance edits previously took effect only after a restart.
- *Acceptance:* edit acceptance through an audited verb; the next brief contains the new text and its digest.

**FAE-ROB-041 (MUST)** Instance configuration that can safely change at run time (pane limit, heavy-op capacity, host thresholds, tasks-in-flight limit) MUST be re-read at each round. Configuration that cannot (instance root, socket path) MUST be reported in status as fixed for the life of the process.
- *Rationale:* a pane limit read once at construction pinned a run at an unusable value.
- *Acceptance:* changing the pane limit through the API takes effect next round.

**FAE-ROB-042 (MUST)** The Normal-mode disposition (mode, dry-run or live, ticket room, review destination branch, push namespace) MUST be persisted per item at submission and MUST be read by resume and recovery, so that a restarted daemon drives a live item live and a dry-run item dry, with no plane-wide uniformity requirement. A row lacking a disposition MUST fail closed with a message naming the missing context, and the row MUST remain resolvable by an operator verb.
- *Rationale:* an unpersisted disposition made a live task terminal by infrastructure while it held real external effects, and blocked every further live submit.
- *Acceptance:* tests: a dry-run item resumed stays dry and emits nothing live; a live item resumed continues with the same room, branch and namespace and does not re-create effects it already created.

---

## 9. Modes and external effects

### 9.1 Modes

**FAE-MODE-001 (MUST)** Mode MUST be a task-scoped disposition, never a daemon-wide setting. One instance MUST be able to drive Local, Normal dry-run and Normal live tasks concurrently.
- *Rationale:* operator non-negotiable: Local and Normal are task configuration.
- *Acceptance:* a run with one task in each mode produces effects only for the live task.

**FAE-MODE-002 (MUST)** In Local mode, roles and the engine MUST NOT perform any external effect: no ticket or code-review tool use, no `git push`, no `git remote` changes, and no external messaging. Enforcement MUST be mechanical: the tool profile denies those tools, the version-control shim refuses push and remote commands (with a distinct exit code), and the pre-tool hook denies them. Local mode MUST NOT add other blanket bans unless the operator configures them.
- *Rationale:* a narrow but hard local policy.
- *Acceptance:* a Local session attempting each banned action is refused; an allowed tool works.

> Proposed 2026-11-05 (mosaic model-building), amendment to FAE-MODE-002: roles stay banned from push in Local mode. The engine MAY push only through the `git-push` effect of FAE-EXT-023, to a remote and branches the target configuration declares; every other push stays refused.

**FAE-MODE-003 (MUST)** In Normal dry-run mode, every ExternalEffect stage MUST record its intended effect (tool, pinned arguments, digest) as an artifact and perform no external call. A dry-run task MUST NOT reach Done; it MUST end in a terminal review state that is clearly labelled dry-run complete (Escalated with kind DryRunComplete), so it can never be confused with delivered work.
- *Rationale:* dry runs prove the path without effects.
- *Acceptance:* a dry-run task records every intended effect and ends in the labelled state.

**FAE-MODE-004 (MUST)** Normal live mode MUST require, at submission: the commit-external flag, a ticket room id (if the workflow uses tickets), a review destination branch and a push namespace. A missing value MUST be refused at submission.
- *Rationale:* external effects must be fully pinned before they start.
- *Acceptance:* each missing value is refused naming it.

### 9.2 External-effect execution

**FAE-EXT-001 (MUST)** External effects MUST be performed only through role-scoped, tool-level grants, attached only for tasks in Normal live mode, and only to the roles a workflow's ExternalEffect stages name (for example the ticket-create tool to the planner role and the review-create tool to the orchestrator role). Grants MUST be at the level of individual tools, never a whole server and never a wildcard. Local and dry-run sessions MUST have byte-identical configurations to a run without grants.
- *Rationale:* operator-approved, least-privilege design.
- *Acceptance:* tests: a live planner session's configuration lists exactly its tool; a Local session's configuration equals the no-grant baseline byte for byte.

**FAE-EXT-002 (MUST)** The daemon MUST pin the arguments of every external call (for example ticket room id, package names, review destination branch, working directory) and MUST validate the role's reported call arguments against the pins. A mismatch MUST be refused fail-closed with no effect marked done, and recorded.
- *Rationale:* a role must not be able to change where an effect lands.
- *Acceptance:* a role reporting a different destination branch is refused; the refusal is audited.

**FAE-EXT-003 (MUST)** Tool-name validation MUST accept exactly the bare tool name or the MCP-qualified form for the pinned server (server prefix plus separator plus tool name), compared as an exact match on the normalised parts. It MUST NOT use a substring or contains match. A different server, or a different tool on the same server, MUST be refused. The audit row MUST record the canonical bare name. Normalisation MUST happen at one validator for every pin.
- *Rationale:* one agent CLI reports qualified names and another reports bare names; the qualified echo was refused 100% of the time and orphaned a live code review.
- *Acceptance:* tests: a qualified echo is accepted and recorded canonically; another server's tool is refused; a tool name with an extra suffix is refused.

**FAE-EXT-004 (MUST)** When a role reports a created external id, the daemon MUST durably record it (as the task's reference, or as an explicitly labelled unreconciled external effect naming it) **before** any validation verdict may discard the call. A refusal MUST never orphan a live effect.
- *Rationale:* a real code review was created but the system had no record of it, because the record was written only after a verdict that refused it.
- *Acceptance:* a test with a pin mismatch and a reported id leaves a durable unreconciled-effect row naming the id.

**FAE-EXT-005 (MUST)** External effects MUST be idempotent. Before creating a code review, the daemon MUST look for an existing review for the task and **adopt** it, keyed on the task identity plus the review's recorded branch, optionally confirmed by tree hash. It MUST NOT key adoption on commit sha, because re-committing identical content at another location changes the sha. Ticket creation MUST be idempotent on the task identity and the outbox key.
- *Rationale:* a review's recorded commit differed from the pushed branch's commit for byte-identical content, so a sha-keyed check would open a second review.
- *Acceptance:* a resume of a task with an existing review creates none and adopts it; the planted different-sha, same-tree case adopts.

**FAE-EXT-006 (MUST)** Every external call MUST be audited: canonical tool name, argument digest, pinned values, returned id, outcome and the role session that made it.
- *Rationale:* audited external effects.
- *Acceptance:* every live effect in a test run has exactly one audit row.

> Proposed 2026-11-05 (mosaic model-building). Source: mosaic `requirements/fae-model-building-assessment-controller.md` FAE-CTL-110; re-checked in `requirements/fae-recheck-2026-11-05.md`.
>
> **FAE-EXT-023 (SHOULD)** A target configuration MAY declare `push: {remote, branches, mode: fast-forward-only}`, enabling an ExternalEffect kind `git-push` run by the engine, never by a role. It MUST be reachable in Local mode only for the declared remote and branches, MUST be preceded by the protected-commit floor's gates and by FAE-SEC-009's content scan of the whole pushed range, MUST be idempotent on (remote, branch, sha), and MUST be audited as FAE-EXT-006 requires.
> - *Rationale:* after a round, the operator's own private remote receives the landed commits; today the only push path is the Normal-mode code-review lifecycle.
> - *Acceptance:* tests: (a) a push to an undeclared branch is refused; (b) a repeated push of the same sha is recorded as adopted; (c) a pushed range that adds a denied literal is refused; (d) a non-fast-forward push is refused.

### 9.3 Code-review lifecycle

**FAE-EXT-010 (MUST)** Code reviews MUST be created as **drafts**. The automatic publish, automatic merge and automatic merge-with-rerun options of the review-creation tool MUST be omitted or false on every creation call, and a test MUST assert it.
- *Rationale:* Fae must never ship or merge a review as a side effect of creation.
- *Acceptance:* the creation argument set is checked by a test for each flag.

**FAE-EXT-011 (MUST)** After a review exists, a post-review stage MUST poll its status through the review read tool: the build status, every analyzer or check, and the automated-review comments. Publish and merge MUST be **separate, separately audited** calls, each recording its result.
1. Publish MUST happen only when every analyzer whose own declared "pass required for" set includes publish has status Pass.
2. Merge MUST happen only when every analyzer whose declared set includes merge has status Pass, and every blocking automated-review comment has been addressed.
3. The gate logic MUST be derived from each analyzer's own declared requirement, not from a hard-coded analyzer list.
4. Statuses other than Pass (including Blocked, Skipped, unknown or absent) MUST NEVER count as passing.
- *Rationale:* observed analyzer statuses include Pass, Fail, Blocked and Skipped; Blocked and Skipped are neither pass nor fail, and a naive gate would publish on them.
- *Acceptance:* tests: a failing build blocks publish; a Blocked analyzer required for merge blocks merge; an unaddressed blocking comment blocks merge; an absent status blocks both.

**FAE-EXT-012 (MUST)** A blocking automated-review comment, or a failing analyzer whose cause is the change, MUST route to the implementing role as remediation. The fix MUST ship as a **new revision on the same review** (the review-revision tool with the existing review id), never as a new review. The loop MUST be bounded (default 5 revisions), and exhaustion MUST escalate with the reason.
- *Rationale:* one review per task, with traceable revisions.
- *Acceptance:* a scripted blocking comment produces revision 2 on the same review id; the cap escalates.

**FAE-EXT-013 (MUST)** In Normal live mode with the merge stage enabled, a task MUST reach Done only after its merge succeeds, and that event MUST drive the ticket's closed transition.
- *Rationale:* Done means delivered.
- *Acceptance:* a scripted merge failure keeps the task out of Done.

**FAE-EXT-014 (MUST)** The publish-and-merge stage MUST have an instance-level and a task-level opt-out. When opted out, the review MUST remain a draft and the system MUST make **no** publish or merge call (asserted by call absence, not by call failure). Disabled MUST be byte-identical to a workflow without the stage.
- *Rationale:* test reviews must stay drafts forever.
- *Acceptance:* an opted-out run's audit shows no publish or merge call.

**FAE-EXT-015 (MUST)** Review creation and revision MUST run from a **pre-provisioned workspace binding** for the package (declared in the target configuration), pointed at the task branch, with the working directory pinned by the daemon. Fae MUST NOT create, sync or merge build workspaces in the drive's hot path. A missing binding MUST fail closed, naming it.
- *Rationale:* the review tool could not resolve the package from a plain worktree; provisioning in the hot path causes host I/O saturation.
- *Acceptance:* a live-adapter test with the binding absent refuses before any external call.

**FAE-EXT-016 (MUST)** Before the first external review of a package, and as a target-configuration health check, Fae MUST verify that the package's own fleet build of the integration tip is green (for example through a draft review's dry-run build, which the operator has approved as a verification mechanism) and MUST report a red baseline as a blocking target-configuration defect, not as a task failure.
- *Rationale:* a dependency that needed system libraries absent from the build fleet made every review's build fail, independent of the change.
- *Acceptance:* a target with a red baseline reports the defect and keeps tasks out of the publish stage.

### 9.4 Ticket lifecycle

**FAE-EXT-020 (MUST)** Ticket lifecycle mirroring (for example open, in progress, in review, done) MUST be derived from the **real tool schema** and from the room's configured workflow actions, read per room at run time (for example through the room-resources tool), and cached per room with a refresh bound. Fae MUST NOT hard-code status values, action names or argument keys. A needed action absent from the room MUST fail closed naming the room.
- *Rationale:* a hard-coded mapping sent wrong keys and wrong status values, all rejected by the real tool, which accepts only a two-value status and a separate workflow-action field.
- *Acceptance:* a test against a recorded schema and room resources emits arguments that the schema accepts; a room missing an action refuses.

**FAE-EXT-021 (MUST)** Ticket emissions MUST go through a durable outbox with per-row attempts, last error and an emitted flag, and MUST be idempotent on the outbox key.
- *Rationale:* rows sat unemitted with no visibility.
- *Acceptance:* status reports outbox depth and failures.

**FAE-EXT-022 (MUST)** The pinned argument keys for each external tool MUST be keys that exist in that tool's schema; the daemon MUST validate pins against the schema at startup and refuse to enable a live effect whose pins reference a non-existent key.
- *Rationale:* a pin on a non-existent key refused correct calls.
- *Acceptance:* a startup test with a bad pin key refuses the live capability with a message.

---

## 10. Operator authority and task controls

### 10.1 Authority model

**FAE-OPS-001 (MUST)** Authority MUST be divided as follows, and enforced by the daemon's action-permission table rather than by instruction:

| Actor | May | May not |
|---|---|---|
| Operator | Create tasks and documents; author and retire workflows and target configurations; run every task-control verb; direct the Prophets; raise limits | Bypass the state machine or write the plane directly |
| Controller | Everything the operator may, but only by citing an operator message that authorises the verb or class of verbs | Act on its own authority |
| Prophet (per-task or global) | Read; relay; propose verbs to the operator; enact a verb only when an operator message explicitly directs that exact verb | Approve code, waive a gate, change governance, create a quality disposition |
| Workflow roles | Produce artifacts for their stage; request scope widen or RED rework | Mutate the graph, the plane, governance or another task |
| Final judgement and QA (default content) | Decide gate dispositions through their artifacts | Edit code |

- *Rationale:* Oracle and QA own quality; the Prophet only relays; no actor invents a waiver.
- *Acceptance:* a permission-table test enumerates every (actor, action) pair; a role attempting a verb is refused.

**FAE-OPS-002 (MUST)** Every mutating verb by the operator or a controller MUST cite an **authorizing operator message id**: a message previously recorded on the plane through the message verb. The daemon MUST verify that the message exists, is not consumed by an incompatible verb, and is newer than a configurable maximum age (default 24 h), and MUST record the citation on the audit row. Read verbs need no citation.
- *Rationale:* audited operator messages are the authority trail; the prior system folded the id into a note without checking it.
- *Acceptance:* a verb citing a non-existent id is refused with no effect; a verb citing a valid id records it.

**FAE-OPS-003 (MUST NOT)** No actor, path or tool may write the control plane except the daemon's validated state machine and audit writer. There MUST be no "maintenance SQL" path. Any repair that would require one MUST instead be a typed, audited verb.
- *Rationale:* stop-window SQL edits to edges and priorities were a recurring need in the prior system; each one is a missing verb.
- *Acceptance:* the plane's user accounts give write access only to the daemon; the documentation lists no SQL repair procedure.

### 10.2 Task-control verbs

**FAE-OPS-010 (MUST)** The daemon MUST provide these task-control verbs, each audited, each validated by the state machine, and each with its own RPC:

| Verb | Legal from | Effect |
|---|---|---|
| Escalate | any non-terminal state | to Escalated with kind OperatorEscalate and a reason |
| Block | any non-terminal stage state | to Blocked with a reason; re-entry follows the workflow's remediation route |
| Cancel | Pending or Blocked (refused in flight) | to Cancelled; dependents stay held and are reported |
| Defer | Pending | leaves the ready set until an explicit undefer or a given time; refused in flight |
| Undefer | Pending and deferred | returns to the ready set |
| Reprioritize | any non-terminal | sets the priority (integer ≥10, larger first); affects future ordering only |
| Set attempt limit | any non-terminal, or Escalated with kind AttemptCap | sets the per-task limit; for AttemptCap escalations, a raise plus resume re-grants attempts |
| Retire | Escalated only | to Retired with a required reason class |
| Restart with information | Escalated or Pending | appends the information to the task content (append-only, hash recorded); an escalated task returns to the workflow's first stage, a pending task only gains the information; refused for Done and in-flight tasks |
| Resume with context | any non-terminal | attaches context to every later brief of the task, never the turn in flight, without changing stage |
| Resolve uncertain dispatch | a task parked on uncertainty | re-dispatch, accept recorded output, or escalate |
| Migrate | non-terminal | moves the task to a named revision under FAE-WF-093 |

- *Rationale:* every operator intention needs a real, audited enactment, not a recorded intent.
- *Acceptance:* each verb has a positive test, a refusal test for an illegal source state, and an audit-row assertion.

> Proposed 2026-11-05 (mosaic model-building), amendment to FAE-OPS-010: the "Resume with context" row gains an `in-flight` delivery under FAE-OPS-033, and Done tasks gain a follow-up under FAE-OPS-034. The rows' current refusals (no delivery to the turn in flight; restart refused for Done) stay as written for those verbs.

**FAE-OPS-011 (MUST)** Retire MUST require exactly one reason of these forms: **superseded-by** a task id that exists on the plane (blocking dependents are carried to the successor); **contained** a commit sha that resolves in the instance repository (dependents are released); or **obsolete** a non-blank ruling reference (dependents stay held). A blank or unparseable reason MUST be refused with no transition. The backlog row MUST become retired in the same operation, and a retired row MUST never re-enter readiness. Status, history, topology and event reads MUST hide retired tasks by default with an explicit opt-in.
- *Rationale:* retirement must carry a sound reason and a correct effect on dependents.
- *Acceptance:* tests for each reason form, each dependents effect and each refusal.

**FAE-OPS-012 (MUST)** Evidence for a **contained** reason MUST come from the task's own commit reference, or from the integration-branch commit whose subject identifies that task's landing. It MUST NEVER come from a test of whether the task's branch tip is an ancestor of the integration branch.
- *Rationale:* a rebased branch is trivially an ancestor and names another task's commit; that mistake put a wrong sha on nine retirements.
- *Acceptance:* a test with a rebased branch whose tip is an ancestor but whose landing is absent refuses the contained reason.

**FAE-OPS-013 (MUST)** The backlog row MUST reconcile to its task's state. An audited reconcile MUST set a backlog row reading escalated to done when its task is Done, with a reason citing the landing sha (per FAE-OPS-012). The reconcile MUST run once at startup and on demand by a verb, MUST never run as a silent background rewrite, MUST leave a genuinely escalated task untouched, MUST leave alone and report a Done task with no identifiable landing sha, MUST make no write and no audit row for an already-consistent row, and MUST be idempotent across restarts.
- *Rationale:* landed work showed as escalated and could hold dependents.
- *Acceptance:* the five cases above are tests.

**FAE-OPS-014 (MUST)** Edge editing MUST be an audited daemon verb: add an edge (refused if it would create a cycle, naming the cycle; refused if the dependent has already been dispatched past Pending, unless the operator explicitly accepts the change for an in-flight dependent) and remove an edge (refused if absent). Self-edges MUST be refused. Admission MUST persist edges before items so a dependent is never visible as ready without its blocking edge.
- *Rationale:* edges were previously fixed only by direct database edits.
- *Acceptance:* tests for each refusal and for edge-before-item ordering.

> Proposed 2026-11-05 (mosaic model-building). Source: mosaic `requirements/fae-model-building-assessment-execution.md` Top blocker 4, §2 B4 and §3.1 FAE-OPS-040; re-checked in `requirements/fae-recheck-2026-11-05.md`.
>
> **FAE-OPS-033 (MUST)** A message verb on a running task, `Notify(task, kind, body, from_task?)`, MUST support delivery `next-turn` (as Resume with context does today) and `in-flight`. For an agentic turn (FAE-RT-015), `in-flight` MUST deliver the message into the running session at its next tool boundary; where the provider cannot accept input mid-turn, the adapter MUST checkpoint (FAE-RT-019), end the turn, and resume the same session with the message first. `kind` is one of `breaking-change`, `cross-area-need` and `note`. The message is bound as quoted data (FAE-WF-043), `from_task` is recorded as provenance, and the message appears in both tasks' histories. A controller MAY relay a `note` between tasks without a new operator message; other kinds follow FAE-OPS-001's authority.
> - *Rationale:* a coordinator routes one finished agent's breaking change to agents still running; delivered only at the next brief, it arrives after the receiver has built on the old shape.
> - *Acceptance:* tests: (a) an `in-flight` message reaches a running agentic turn and its handoff cites it; (b) a `next-turn` message appears in the next brief only; (c) both tasks' histories show the message with `from_task`.

> Proposed 2026-11-05 (mosaic model-building). Source: mosaic `requirements/fae-model-building-assessment-execution.md` §1.2 ("Resume a finished agent") and §3.1 FAE-OPS-041 and FAE-OPS-042; re-checked in `requirements/fae-recheck-2026-11-05.md`.
>
> **FAE-OPS-034 (MUST)** An audited verb `FollowUp(task, text, budget)` on a Done task MUST admit a new task linked by `follow_up_of`, on the same workflow revision, with the same declared files and read policy, that resumes the predecessor's task-bound provider context (moved to the new task's key) under a turn budget (FAE-RT-015). The Done task's records MUST stay unchanged, and the follow-up lands through the same gates. A role MAY declare `fresh: true`; its sessions MUST then never resume a provider context created for another task, whatever the slot scope, and are never eligible for a follow-up resume.
> - *Rationale:* a coordinator sends a finished agent a five-minute fix, such as a broken caller routed back, while its context is warm; a cold new task re-reads the whole area. Blind measurers, by contrast, must provably start fresh.
> - *Acceptance:* tests: (a) a follow-up on a Done task resumes the predecessor's provider session, edits only the declared files, lands, and leaves the predecessor's record byte-identical; (b) two tasks of a workflow-global `fresh` role launch with different provider sessions; (c) a follow-up on a `fresh` role's task is refused.

> Proposed 2026-11-06 (mosaic code-to-model audit). Source: mosaic `internal/compose/engine.go:70-89`, `:370-417`, `:459-472` and `internal/compose/compose_test.go:597-650` (catalog C-89); audit in mosaic `requirements/fae-code-to-model-audit-2026-11-06.md` (row gates G18, which found FAE-WF-021 and the OPS-010 Restart row in CONFLICT with this need).
>
> **FAE-OPS-035 (MUST)** An audited verb `ResumeAtStage(task, reason)`, joining FAE-OPS-010's verbs, on an Escalated task whose escalation kind is not a floor or authority refusal (NoRedViolation, ScopeWidenRejected, CoordinatorVeto, StaleBranchResidue, MergeOwnHunkLoss, ResumeEvidence, ExternalEffectRefused) MUST return the task to the stage that escalated (FAE-WF-021 amendment of 2026-11-06), in place of the workflow's first stage. Every completed stage's recorded evidence, every passed FanOut branch with its branch ref, and every Done child MUST be kept and not re-run. Failed and unfinished branches and stages are re-dispatched, and the stage's same-gate and schema-retry counts restart; the per-task attempt limit, test runs, Loop counts and scope-widen counts carry over as on a restart (FAE-WF-074), and the verb is refused while no attempt is left under the limit. Evidence whose tree no longer matches the task's tree MUST be re-validated under FAE-ROB-013 before reuse. The verb MUST be refused for kinds the target configuration lists as restart-only.
> - *Rationale:* a model-building run spends hours in fan-out before judging; a failure in a late stage (a quota cut, a broken judge prompt) must not re-run inventory and 32 area agents. At the 08:25 UTC audit resuming an Escalated task sent it to the workflow's first stage (`engine/control.go:176-200` then); fae's working tree at 09:58 UTC implements this verb (`engine/control.go:67-119`, `:329-391`, `daemon/task_controls.go:66-74`), but refuses instead of re-validating evidence whose tree changed, and reads `restart_only` as one flag for every kind.
> - *Acceptance:* tests: (a) a task escalated at its judging stage resumes there, and no inventory or area turn is re-dispatched; (b) a FanOut with 3 passed branches and 1 failed one re-runs only the failed one; (c) resuming a task escalated with kind NoRedViolation is refused, and so is resuming one whose kind the target lists as restart-only; (d) a resume on an AttemptCap escalation with no attempt left is refused until the limit is raised.

### 10.3 Operator messages

**FAE-OPS-020 (MUST)** The daemon MUST record operator messages and replies on the plane through an audited verb, returning a message id. Messages MUST form threads (a reply names its root). An inbox read MUST list Blocked, Escalated and parked-uncertain tasks, undisposed decisions, and unanswered Prophet questions, all from the instance plane.
- *Rationale:* the authority trail and the surfacing channel.
- *Acceptance:* message, reply and inbox round-trip through the daemon with no legacy store.

### 10.4 Prophets

**FAE-OPS-030 (MUST)** Each task MUST be able to have a **per-task Prophet session**: an agent session whose context is the task's content, its path through its molecule so far, its artifacts and decisions, and its operator messages, and nothing else. The session MUST be able to: answer the operator in a chat; propose verbs; and, only when the operator's message explicitly directs it, invoke the daemon's restart-with-information and resume-with-context verbs, citing that message. Chat messages MUST be stored on the plane per task, paged, with author and time.
- *Rationale:* the operator steers a task through its own liaison.
- *Acceptance:* a scripted Prophet session reads only that task's history; an operator message directing restart produces an audited restart citing it; a Prophet attempt without such a message is refused.

**FAE-OPS-031 (MUST)** Fae MUST provide a **global Prophet**: an instance-wide liaison with read access to all tasks, molecules, health and alerts, able to summarise and propose verbs, and able to enact a verb only under the same explicit-direction rule.
- *Rationale:* an instance-wide counterpart to the per-task Prophet.
- *Acceptance:* the global Prophet answers a cross-task question from plane reads; it cannot enact a verb without a directing message.

**FAE-OPS-032 (MUST)** Prophet sessions MUST be ordinary sessions under the host guard, the pane limit and reaping, and MUST NOT hold role slots of any workflow.
- *Rationale:* liaisons must not consume delivery capacity.
- *Acceptance:* opening a Prophet does not change any role's free slots.

---

## 11. Command-line interface

### 11.1 General CLI rules

**FAE-CLI-001 (MUST)** The binary MUST be named `fae`. Every command MUST accept `--instance <name>` (default `default`) and MUST work from any directory, except where it states that it needs the bound workspace. Every mutating command MUST go through the daemon (Section 2). Every mutating operator verb MUST accept `--authorizing-message <id>` and refuse without it (FAE-OPS-002). Every command MUST support `--json` for machine-readable output and MUST exit 0 on success, 1 on a refusal or validation failure (with the reason on standard error), 2 on a usage error, and 3 when the daemon is unreachable.
- *Rationale:* a uniform, scriptable, auditable surface.
- *Acceptance:* a conformance test runs every command with `--json` and checks the exit-code contract.

**FAE-CLI-002 (MUST)** Time inputs MUST be explicit where they are recorded as facts (admission times, retirement times) or taken from the daemon's clock and recorded; the CLI MUST state which in its help. Priorities MUST be integers ≥10, larger first; values below 10 MUST be refused as ambiguous.
- *Rationale:* deterministic records; the prior system refused 0–9 as ambiguous.
- *Acceptance:* tests for both.

**FAE-CLI-003 (MUST)** Commands that need a running daemon MUST NOT start one implicitly, except `fae run`, which MAY start the instance's daemon when `--start` is given and the current directory is the bound workspace.
- *Rationale:* explicit lifecycle.
- *Acceptance:* `fae status` against a stopped instance exits 3 with a message naming the start command.

Each command below is specified by: purpose; inputs; authorization; validation; outputs; failure modes; and an acceptance criterion. The commands may be renamed, but none may be dropped.

### 11.2 Lifecycle and work submission

**FAE-CLI-010 `fae serve` (MUST)**
- **Purpose:** start the instance daemon.
- **Inputs:** `--instance`; `--workspace` (required; must equal the current directory); `--adapter` (multiplexer, process or scripted; default multiplexer); `--default-mode` (the default disposition for submissions that state none; default local); `--run-id` (the drain run id; default `backlog-drain`); `--max-panes`; `--max-tasks-in-flight`; `--max-heavy-weight`; UI child flags `--ui`, `--ui-bin`, `--ui-dist`, `--ui-port`, `--ui-bind` (default 127.0.0.1), `--ui-allow-writes`, `--ui-rpc-log`; `--foreground`.
- **Authorization:** the operator's own shell; it must not be launched from inside a sandboxed agent session (FAE-HOST-030).
- **Validation:** the workspace root guard (FAE-ARC-021); the instance lock; the pane-limit floor (FAE-SCH-012); a non-loopback UI bind is refused (FAE-API-030); a sandboxed launch context is refused; the Dolt port ownership checks (FAE-CP-004); schema migration (FAE-CP-010).
- **Outputs:** a start line naming the instance, adapter, plane endpoint, socket path and UI URL (if enabled); the pid file; supervised children.
- **Failure modes:** a live owner; wrong workspace; floor violation; foreign Dolt endpoint; unrecorded Dolt server; sandboxed context; UI binary missing (warned, the daemon still starts).
- **Acceptance:** tests for each refusal; a successful start creates the layout of FAE-ARC-020.

**FAE-CLI-011 `fae run` (MUST)**
- **Purpose:** submit one task to the running daemon.
- **Inputs:** exactly one of `--prompt <text>` or `--file <markdown path>`; `--mode local|normal`; for normal, exactly one of `--dry-run` or `--commit-external`; `--ticket-room <id>`; `--review-destination-branch <name>`; `--push-namespace <prefix>`; `--workflow <id>` and `--revision <n>`; `--target-config <id>` and `--target-revision <n>`; `--priority`; `--run-id`; `--start` (FAE-CLI-003).
- **Authorization:** intake authority (operator or a controller citing a message); a live Normal submission requires an authorizing message whose text approves live external effects.
- **Validation:** mode completeness (FAE-MODE-004); workflow and target existence and non-retirement; file readability; content size limit (default 1 MiB).
- **Outputs:** the task id, run id, molecule id and disposition.
- **Failure modes:** missing or conflicting flags; unknown workflow; no daemon.
- **Acceptance:** each validation is tested; the returned task appears in status.

**FAE-CLI-012 `fae status` (MUST)**
- **Purpose:** report instance health and work.
- **Inputs:** `--instance`; `--tasks` (include per-task rows); `--include-retired`.
- **Authorization:** read.
- **Validation:** none beyond reachability.
- **Outputs:** instance name; pid and uptime; binary version and build commit; plane (Dolt endpoint, data directory, schema version, last commit); adapter; drive state (running, parked rounds, last round time, last exit cause); counts by state; withheld counts by reason; leases in use by kind; pane counts (live, limit, owned); heavy-op pool (capacity, holders, waiters); host snapshot (memory available, I/O and memory pressure, load); provider health summary; UI child (URL, pid, health, restarts); maintenance marker; shutting-down flag; snapshot age.
- **Failure modes:** daemon unreachable (exit 3).
- **Acceptance:** the JSON output contains every listed field; it answers in under 1 s with 4 concurrent heavy reads in flight.

**FAE-CLI-013 `fae resume` (MUST)**
- **Purpose:** resume a run or a task through the daemon's stage-aware resume.
- **Inputs:** `--instance`; `--run-id` (default the latest); `--task <id>` (optional).
- **Authorization:** operator or a controller citing a message.
- **Validation:** the target exists; a terminal task is refused unless a raised attempt limit re-grants it.
- **Outputs:** the resumed set and each task's resumed stage.
- **Failure modes:** a missing disposition (fails closed, naming it); the target is terminal.
- **Acceptance:** resume of an interrupted run reaches the same outcome as an uninterrupted one (FAE-ROB-012).

**FAE-CLI-014 `fae backlog admit` (MUST)**
- **Purpose:** admit a documented graph of work items.
- **Inputs:** a payload file of items (identity; source reference; priority; acceptance text; optional write class tags; optional workflow selector; optional target configuration; optional disposition; optional structured fields: declared files, required test filter, test command shape) and edges (from, to, kind blocking or relates); `--instance`.
- **Authorization:** intake authority with an authorizing message.
- **Validation:** schema; priorities ≥10; blocking edges only between items in the payload or already on the plane; no self-edges; no cycles (naming the cycle); dispositions complete; keyed upsert (an existing identity updates its non-terminal fields; a terminal identity is refused with a pointer to re-admission under a new identity).
- **Outputs:** counts of admitted, updated and refused items, with reasons.
- **Failure modes:** any validation failure refuses the whole payload, and nothing is written.
- **Acceptance:** tests for upsert, cycle refusal and atomicity.

**FAE-CLI-015 `fae backlog edge add|remove` (MUST)**
- **Purpose:** edit one dependency edge.
- **Inputs:** `--from`, `--to`, `--kind`, `--instance`, `--authorizing-message`, `--accept-in-flight` (add only).
- **Authorization:** operator or controller with a message.
- **Validation:** FAE-OPS-014.
- **Outputs:** the changed edge and the readiness effect.
- **Failure modes:** a cycle, a dispatched dependent, an absent edge.
- **Acceptance:** FAE-OPS-014 tests.

**FAE-CLI-016 `fae backlog show` (MUST)**
- **Purpose:** read the backlog graph with readiness and withheld reasons.
- **Inputs:** `--instance`; filters by state.
- **Authorization:** read.
- **Outputs:** items, edges, ready set, dead edges, deferrals.
- **Acceptance:** dead edges appear with the prerequisite's outcome.

> Proposed 2026-11-06 (mosaic code-to-model audit). Source: mosaic `requirements/fae-hierarchical-workflows.md` FAE-CLI-056 (carried; it named a `fae admit` command, which is `fae run` and `fae backlog admit` here); catalog P-91, C-106; audit in mosaic `requirements/fae-code-to-model-audit-2026-11-06.md` (row control 1). It keeps its mosaic ID, which Fae does not use.
>
> **FAE-CLI-056 `fae run` and `fae backlog admit`: inputs and parents (MUST)** `fae run` (FAE-CLI-011) MUST accept `--input name=@file` (YAML or JSON) and `--input name=value` (JSON when it parses, else a string), both repeatable, plus `--parent <task>`, `--from-parent input=artifact` (repeatable) and `--max-sessions N`. A `fae backlog admit` item (FAE-CLI-014) MUST accept the same fields. They map onto Admit's fields (FAE-API-034), and a refusal prints every error.
> - *Acceptance:* tests: (a) `--input profile=@p.yaml` binds the parsed object; (b) `--input n=3` binds the number 3 and `--input s=abc` the string `abc`; (c) a refusal lists two missing inputs and one unknown parent artifact.

> Proposed 2026-11-07 (mosaic code-to-model re-audit), amendment to FAE-CLI-056: `fae run` and `fae backlog admit` MUST omit `max_sessions` from the Admit request unless `--max-sessions` was given, so that `--parent` without `--max-sessions` admits a detached child. Acceptance gains: (d) `fae run --parent P --from-parent a=b` (no `--max-sessions`) admits `P.detached.1` against a running daemon. Today the flag defaults to -1 and is always sent (`cmd/fae/input_flags.go:99`, `:108`), and the daemon refuses it on every child admission (`daemon/admission.go:59-62`, `daemon/tree_admission.go:139-141`). Source: native probe `reaudit/hier/overlay/zz_hier_probe_test.go` TestZZDetachedAdmissionAsCLISendsIt.

### 11.3 Governance (templates and schedules)

**FAE-CLI-020 `fae template workflow-put|workflow-get|workflow-latest|workflow-list|workflow-retire|workflow-validate|workflow-diff|workflow-simulate` (MUST)**
- **Purpose:** operator-only, versioned workflow management.
- **Inputs:** put: `--workflow`, `--name`, `--content <file>`; get: `--workflow --revision`; latest and list: `--workflow`; retire: `--workflow --reason`; validate: `--content <file>` (offline allowed); diff: `--workflow --from --to`; simulate: `--content` or `--workflow --revision`, plus `--scenario <file>`.
- **Authorization:** put and retire need the operator (a controller with a governance-class message); the others are reads.
- **Validation:** FAE-WF-003, FAE-WF-090.
- **Outputs:** the new revision number and content hash; the content; the list; validation errors; the diff; the simulation report.
- **Failure modes:** invalid content (all errors listed); retired workflow; unknown revision.
- **Acceptance:** every operation works while the daemon runs (writes go through the daemon RPC, never with a stopped daemon only).

**FAE-CLI-021 `fae template target-config-put|target-config-get|target-config-list` (MUST)**
- **Purpose:** versioned target configuration management.
- **Inputs:** put: `--id`, `--content <file>` (revision assigned as latest+1); get: `--id --revision`.
- **Authorization:** as for workflow-put.
- **Validation:** schema; pinned external-effect keys checked against tool schemas when available (FAE-EXT-022); protected branches declared; workspace bindings exist.
- **Outputs:** revision and hash.
- **Acceptance:** a target with a missing pre-provisioned workspace binding is refused for live use.

**FAE-CLI-022 `fae schedule create|list|inspect|pause|resume|retire` (MUST)**
- **Purpose:** durable schedules: cron-like recurrence that admits a templated item, or a provider-neutral loop (FAE-RT-014).
- **Inputs:** create: `--id`, `--kind cron|loop`, the specification (anchor, interval, optional end; or the loop's target role binding, prompt, cadence (interval or self-paced), optional maximum iterations, expiry); the others: `--id`.
- **Authorization:** operator with a message for create, pause, resume and retire.
- **Validation:** fail-closed specification parse; a loop must have a stop condition (expiry is always present).
- **Outputs:** the schedule record and its lifecycle state.
- **Acceptance:** a created schedule is picked up by the running daemon without a restart.

### 11.4 Operator verbs

**FAE-CLI-030 `fae show` (MUST)**
- **Purpose:** a read-only text snapshot of a run or a task.
- **Inputs:** `--run-id`, `--task`.
- **Authorization:** read.
- **Validation:** none.
- **Outputs:** states, current stages, latest artifacts per stage, escalations.
- **Failure modes:** none beyond reachability; it MUST mutate nothing and MUST NOT start any process.
- **Acceptance:** a file-system and process diff around the command is empty.

**FAE-CLI-031 `fae start-work-item` (MUST)**
- **Purpose:** queue a work item through the shared intake path.
- **Inputs:** `--title`, `--content` or `--file`, `--priority`, optional selector and disposition, `--authorizing-message`.
- **Authorization:** intake authority.
- **Validation:** as `fae run`.
- **Outputs:** the item id.
- **Acceptance:** the item appears in the backlog.

**FAE-CLI-032 `fae attach-design-doc` (MUST)**
- **Purpose:** attach a design-document reference to a task or item, for its spec and planning stages.
- **Inputs:** `--task` or `--item`, `--reference` (a URL or a path inside the target's containment root), `--authorizing-message`.
- **Validation:** path references MUST resolve inside the target's containment root (no escape).
- **Outputs:** the attachment id.
- **Acceptance:** a path outside the root is refused; an attached document appears in the next brief's bound inputs.

**FAE-CLI-033 `fae inbox` (MUST)**
- **Purpose:** read the operator inbox (FAE-OPS-020).
- **Inputs:** `--instance`, filters.
- **Outputs:** threads, escalations, uncertain parks, questions.
- **Acceptance:** served from the instance plane only.

**FAE-CLI-034 `fae message` and `fae reply` (MUST)**
- **Purpose:** record an operator message (thread root) or a reply.
- **Inputs:** `--text` or `--file`; `--task` (optional); reply: `--to <message id>`.
- **Authorization:** the operator only; a controller may record a message only to relay the operator's verbatim words, marked as relayed, with the original source recorded.
- **Outputs:** the message id.
- **Acceptance:** the id is accepted as an authorizing message by a later verb.

**FAE-CLI-035 `fae defer` (MUST)** Inputs: `--task`, `--until <time>` or `--undefer`, `--authorizing-message`. Validation and effect: FAE-OPS-010. Failure modes: in flight; terminal. Acceptance: the item leaves and re-enters the ready set as stated.

**FAE-CLI-036 `fae cancel` (MUST)** Inputs: `--task`, `--reason`, `--authorizing-message`. Effect: FAE-OPS-010. Outputs: the transition and the list of held dependents. Failure modes: in flight; terminal. Acceptance: tests.

**FAE-CLI-037 `fae retire` (MUST)** Inputs: `--task`, `--reason "superseded-by <id>" | "contained <sha>" | "obsolete <ruling>"`, `--authorizing-message`. Validation: FAE-OPS-011 and FAE-OPS-012. Outputs: the transition and the effect on dependents. Failure modes: non-escalated task; unparseable reason; unresolvable id or sha. Acceptance: tests.

**FAE-CLI-038 `fae reconcile-done` (MUST)** Inputs: `--authorizing-message`, `--dry-run`. Effect: FAE-OPS-013. Outputs: reconciled rows with shas; rows left alone with reasons. Acceptance: tests.

**FAE-CLI-039 `fae reprioritize` (MUST)** Inputs: `--task`, `--priority`, `--authorizing-message`. Effect: FAE-SCH-003. Acceptance: the next dispatch reflects it.

**FAE-CLI-040 `fae escalate` and `fae block` (MUST)** Inputs: `--task`, `--reason`, `--authorizing-message`. Effect: FAE-OPS-010. Validation: the bound-workspace isolation guard does not apply (these are plane verbs, not worktree operations). Acceptance: tests.

**FAE-CLI-041 `fae set-attempt-limit` (MUST)** Inputs: `--task`, `--limit`, `--authorizing-message`. Effect: FAE-WF-074. Acceptance: a raise re-grants an AttemptCap escalation on resume.

**FAE-CLI-042 `fae resolve-uncertain` (MUST)** Inputs: `--task`, `--action redispatch|accept|escalate`, `--authorizing-message`. Effect: FAE-ROB-016. Acceptance: each action is tested.

**FAE-CLI-043 `fae prophet session|chat|restart|resume` and `fae prophet global` (MUST)**
- **Purpose:** per-task and global Prophet interaction.
- **Inputs:** session: `--task` (prints the bounded history); chat: `--task`, `--text` (sends to that task's Prophet; prints the reply page); restart: `--task`, `--information <text or file>`, `--authorizing-message`; resume: `--task`, `--context <text or file>`, `--authorizing-message`; global: `--text`.
- **Authorization:** session and chat read or append chat only; restart and resume are audited verbs (FAE-OPS-010).
- **Validation:** state legality as in FAE-OPS-010.
- **Outputs:** history, chat pages, the verb result.
- **Failure modes:** a terminal task for resume; a Done or in-flight task for restart.
- **Acceptance:** tests from FAE-OPS-030.

> Proposed 2026-11-06 (mosaic code-to-model audit). Source: carries mosaic `requirements/fae-host-workflow-limit.md` FAE-CLI-044, counting slot holders (FAE-SCH-016) in place of molecules.
>
> **FAE-CLI-044 `fae workflow-limit` (MUST)**
> - **Purpose:** read the host workflow limit (FAE-SCH-018), or request one.
> - **Inputs:** none to read. To request: `--max N` and `--authorizing-message`; `--max -1` withdraws this instance's request.
> - **Authorization:** reading is a read. A request needs the operator, or a controller citing an operator message that grants the `capacity` class (FAE-OPS-001, -002). The `capacity` class MUST authorise this request and nothing else, and no other class, `runtime` included, may authorise it.
> - **Validation:** `N` is −1 or an integer from 1 to 1,000,000; anything else is refused and nothing is stored. A request cannot carry or change host thresholds, the per-session reservation, the pane limit, role caps or `max_sessions`.
> - **Outputs:** this instance's request with its principal, authorizing message and time; the effective request and the instance that set it; slots held on the host and by this instance, each with its holder (molecule or tree root) and footprint; uncommitted memory and panes and the per-session reservation; the host ceiling, the host workflow limit and what binds it (`none`, `request`, `host_memory`, `pane_limit`, `pressure_hold`, `telemetry_gap` or `host_ledger`); and the holders waiting for a slot with footprint, reason and time waited. A request's reply also states whether the limit in force is below the request.
> - **Failure modes:** invalid value or authority (exit 1, nothing stored); unreadable ledger (exit 1, nothing stored); daemon unreachable (exit 3).
> - **Acceptance:** tests: (a) a controller citing a `capacity` grant sets 4 and the read shows it with its principal; (b) a `runtime` grant is refused; (c) `--max 0` is refused and nothing is stored; (d) with the request at 8 and the ceiling at 3 the read names `host_memory` as binding.

> Proposed 2026-11-06 (mosaic code-to-model audit). Source: mosaic `requirements/fae-hierarchical-workflows.md` FAE-CLI-055 (carried); catalog D-69, C-103, C-104; audit in mosaic `requirements/fae-code-to-model-audit-2026-11-06.md` (row control 23). It keeps its mosaic ID, which Fae does not use.
>
> **FAE-CLI-055 `fae tree` (MUST)** `fae tree <task> [--stats] [--json]` MUST print the run tree of FAE-API-035 that contains the task: per node its id, workflow and revision, state, current stage, depth, detached flag, failure or park reason, recursion-budget refusals and change requests, and with `--stats` the rollups. Authorization: read.
> - *Acceptance:* tests: (a) for a scripted root with three branches and one detached child the output shows two nodes and the branch count under its stage; (b) `--json` output validates against the API schema.

> Proposed 2026-11-07 (mosaic code-to-model re-audit). Source: fae `cmd/fae/main.go:15-29` (validate takes one file), `daemon/workflow_governance.go:79-99` (`DeriveFootprint(d, nil, 0)`), `workflow/subworkflow.go:9-13`; offline `fae workflow validate` of the zoom: `footprint: pinned child mosaic-ontology-pipeline@1 requires resolver`; mosaic `cmd/mosaic/compose.go:22-24` (resolves subworkflow templates from the template's directory).
>
> **FAE-CLI-060 (MUST)** `fae workflow validate FILE [--catalog FILE]...` and `fae workflow simulate` MUST accept a catalog of child workflow documents (keys `id` and `id@N`, as in the scenario `workflows`). With a catalog they MUST apply the admission-time checks offline: every pinned child is resolved; `output_map` and `input_map` are checked against the child's signature (FAE-WF-101); cycles have recursion budgets on every document (FAE-WF-100); and the tree footprint is derived. A missing child MUST be reported as an issue naming it, not only as a footprint error.
> - *Rationale:* a family of workflows (pipeline, light pipeline, zoom) must be checked before installation. Today the zoom validates while its output_map names and its cycle budget are unchecked until a task runs.
> - *Acceptance:* tests: (a) a zoom whose output_map names an output the child does not declare is invalid offline with a catalog; (b) a cycle without recursion budgets on the middle document is invalid offline; (c) with the catalog the footprint is reported.

> Proposed 2026-11-07 (mosaic code-to-model re-audit). Source: fae `cmd/fae/main.go:31-38`, `engine/scenario.go:69-79` (branch report keyed by bare id), `workflow/scenario.go:51-73`; rehearsal reports in `scratchpad/reaudit/e2e/out/` (0.3-1.2 MB each; exit 0 for an Escalated root); mosaic `cmd/mosaic/compose.go:24-28` (`compose simulate` prints the run tree and exits 1 unless the root is done).
>
> **FAE-CLI-061 (MUST)** `fae workflow simulate` MUST exit non-zero when the root does not reach Done, naming the root's state and reason on stderr. It MUST accept `--tree`, which prints the FAE-CLI-055 text rendering of the rehearsed tree: each node's workflow and revision, state, depth, detached flag, reason, change requests, refusals (FAE-OBS-011) and failure routes taken (FAE-OBS-010), plus each fan's branches as `<fan stage>/<branch id>` paths with their final outcome. Branch identities in the JSON report MUST be qualified by their fan stage and visit, so that two fans' `branch-0` never overwrite each other.
> - *Rationale:* a rehearsal is run before hours of agent time are spent. Its verdict must be scriptable and readable without parsing a megabyte of evidence. In the light pipeline's report the root's `children` fan overwrote the `areas` fan's `branch-0`.
> - *Acceptance:* tests: (a) a scenario whose root escalates exits non-zero; (b) `--tree` for a root with an areas fan and a children fan prints `areas/branch-0` and `children/branch-0` separately with their outcomes; (c) the JSON report keys branches by fan stage, visit and branch id.

> Proposed 2026-11-07 (mosaic code-to-model third audit). Source: fae `daemon/workflow_slots.go:321-331` (the workflow-slot ledger path is `WorkflowLedgerPath`, else the live config's `host_ledger`, else `~/.fae/host-ledger`), `daemon/daemon.go:53` (`WorkflowLedgerPath` has no flag), `cmd/fae/cli.go:439-470` (no ledger flag on `serve`); the third audit had to set `HOME` to keep a scripted daemon off the host ledger the live daemon uses.
>
> **FAE-CLI-062 (MUST)** `fae serve` MUST accept `--host-ledger DIR` with every adapter, the scripted adapter included, and it MUST take precedence over the live configuration's `host_ledger`. `fae status` and `fae workflow-limit` MUST print the ledger path in use. A scripted instance that opens the default host ledger MUST warn on start, because its trees then take slots of the host workflow limit (FAE-SCH-018) from live instances.
> - *Rationale:* a rehearsal daemon on a host with a live instance silently joins `~/.fae/host-ledger`. Its trees hold and wait on the live instance's workflow limit. Today the only way out is to change `$HOME` for every fae invocation.
> - *Acceptance:* tests: (a) `serve --adapter scripted --host-ledger D` writes workflow slots only under D, and `status` names D; (b) a scripted serve with no flag and no live config warns that it uses the default host ledger.

### 11.5 Maintenance and analysis

**FAE-CLI-050 `fae reclaim` (MUST)**
- **Purpose:** on-demand reconciliation of the pane, session, heavy-op and role-slot ledgers: clear provably dead-owner entries and keep live ones.
- **Inputs:** `--instance`, `--host-wide` (also reconcile host-scoped ledgers shared by instances), `--dry-run`.
- **Authorization:** operator with a message; host-wide requires an explicit host-wide message class.
- **Validation:** liveness checked by pid and process start time.
- **Outputs:** per ledger, the reclaimed and kept entries.
- **Failure modes:** a ledger that is unreadable fails closed.
- **Acceptance:** FAE-ROB-014 tests run through the command.

**FAE-CLI-051 `fae import-legacy` (MUST)**
- **Purpose:** one-way, idempotent import of a legacy single-workspace store into an instance plane.
- **Inputs:** `--instance`, `--source <path>`.
- **Authorization:** operator; requires the daemon to be stopped (the import holds the instance lock) or performs the import through a daemon RPC.
- **Validation:** the source is opened read-only; the instance plane must already exist (import never provisions one).
- **Outputs:** imported counts; an audit file in the instance root.
- **Failure modes:** a daemon holding the lock (when not using the RPC); no plane; the source is unreadable.
- **Acceptance:** importing twice yields no duplicates.

**FAE-CLI-052 `fae analyze-session` (MUST)**
- **Purpose:** read-only, provenance-tagged metrics for a completed task: wall time per stage; turn count and duration per role; retries by class; operator interventions; lease waits; token counts where the session logs expose them. Every number MUST state its source and be labelled exact or estimate; a source it cannot reach MUST be reported as a gap, never fabricated.
- **Inputs:** `--task`, `--instance`.
- **Authorization:** read.
- **Outputs:** JSON.
- **Failure modes:** the task is unknown.
- **Acceptance:** the output for a scripted task matches the recorded timings exactly and labels nothing exact that was estimated. The output MUST NOT be used to infer spend.

**FAE-CLI-053 `fae heavy-op run` (MUST, hidden broker verb)**
- **Purpose:** the seam shims use to run a command under a heavy-op lease.
- **Inputs:** `--weight`, `--class`, then the command vector.
- **Validation:** the host guard and lease (FAE-SCH-011, Section 14).
- **Outputs:** the command's own exit code; the lease marker is removed from the child environment.
- **Acceptance:** the child's environment lacks the marker; a refusal is a distinct exit code.

**FAE-CLI-054 `fae doctor` (SHOULD)**
- **Purpose:** check prerequisites (git, Dolt, agent CLIs, multiplexer), the instance layout, sandbox inheritance, filesystem headroom (bytes and inodes) and configuration.
- **Outputs:** a pass/fail list with remedies.
- **Acceptance:** a missing agent CLI is reported with a remedy.

---

## 12. Built-in controller operations

Operations that a supervising controller performed by script in the prior system are product capabilities in Fae. Each is a CLI command backed by daemon RPCs and audited.

### 12.1 Rolling restart

**FAE-CTL-001 `fae roll` (MUST)**
- **Purpose:** replace the running daemon binary with a new one with no lost work.
- **Inputs:** `--binary <path>` (or `--build-ref <ref>`); `--note`; `--authorizing-message`; `--max-wait` (default 300 s); `--ack-overlap` (deliberate override of the overlap hold).
- **Behaviour, in order:**
  1. Refuse while any task is in a commit, integrate or external-effect stage, polling until the window is clear or `--max-wait` expires.
  2. File-overlap guard: compute the files the new build changes relative to the running build's source, and the files each in-flight task has touched (its own commits plus uncommitted work). On overlap, hold the roll for at most a bound per overlapping task (default 2 h), except hold indefinitely when the overlapped task is marked critical or is a top-priority task within one attempt of its limit. A query failure MUST be treated as overlap (fail closed). `--ack-overlap` overrides, and is recorded.
  3. Write a maintenance-in-progress marker so the watchdog stays silent for this roll only.
  4. Graceful stop per FAE-ROB-010, recording the graceful-stop marker.
  5. Start the new daemon through an **unsandboxed runtime launcher** (FAE-HOST-030), never from the invoking shell if that shell is sandboxed.
  6. Post-start acceptance: the process is alive; it has no inherited sandbox (for example a seccomp filter state of 0 and no no-new-privileges flag inherited from an agent session) for the daemon, its Dolt child and the watchdog; its environment matches the instance configuration; the plane is consistent; the workspace's integration reference is fast-forwarded to the deployed tip (FAE-VCS-011); and the first drive round completes.
  7. Remove the marker.
- **On any abort:** alert loudly (an alert row, an event and standard error), leave an explicit maintenance marker naming the state and the next action, and never leave the instance silently down.
- **Authorization:** operator or controller with a message.
- **Outputs:** the old and new pids and versions, the hold history and the acceptance results.
- **Failure modes:** a commit window that never clears; an overlap hold; a stop past its bound (a regression: abort loudly, never force-kill); a start failure; an acceptance failure (roll back to the previous binary and alert).
- **Acceptance:** tests on the scripted adapter for each step and each abort path; a planned roll charges no task budget (FAE-ROB-011).

**FAE-CTL-002 `fae roll --when-clear` (MUST)** The roll MUST optionally wait for a window in which no task is at QA and none is in a commit stage, batching every landed change available at that moment, and MUST follow the gated reference, never an ungated tip.
- *Rationale:* rolling under a QA task forces a rebase that costs it a full review cycle; an ungated tip was once deployed 24 minutes before its gates finished.
- *Acceptance:* a test with a task at QA waits; the deployed ref is the last gated one.

### 12.2 Maintenance stop and start

**FAE-CTL-010 `fae maintenance stop|start|status` (MUST)**
- **Purpose:** deliberately take an instance down and bring it back.
- **Behaviour:** stop applies the same commit-window refusal as a roll, performs a graceful stop, and writes an explicit maintenance marker (owner, reason, time, no expiry) that silences the watchdog's dead and idle alerts while an hourly heartbeat line records the state. Start runs the roll's start and post-start acceptance and removes the marker. Status reports the marker.
- **Authorization:** operator or controller with a message.
- **Acceptance:** a stopped instance under maintenance produces no dead-daemon alert; a crash with no marker produces one within one watchdog period.

### 12.3 Watchdog

**FAE-CTL-020 `fae watchdog` (MUST)**
- **Purpose:** an independent supervisor process per instance (or a host-wide one for all instances) that detects daemon death, drive exits and stalls.
- **Behaviour:**
  1. It alerts on a dead daemon, a drive exit (any cause), or in-flight tasks with no new transition for a bound (default 45 min) while the daemon is alive.
  2. It re-arms a stopped drive through the daemon's re-arm RPC with a bounded budget per cause (default 3 re-arms per cause per 1 h window), then stops re-arming that cause and alerts.
  3. A re-arm while a drive is alive is a no-op.
  4. It counts in-flight tasks by an **allowlist** of non-terminal states taken from the molecules (never a denylist).
  5. It honours roll and maintenance markers.
  6. It watches filesystem headroom in bytes and inodes for the RAM-backed temporary filesystem and the workspace filesystem, alerting at a threshold (default 80%) at most once per period.
  7. It runs unsandboxed, like the daemon.
- **Outputs:** alert rows and events.
- **Acceptance:** tests for each alert; a planted retired row is not counted as in flight.

### 12.4 Landing and integration onto the operator's branch

**FAE-CTL-030 `fae land` (MUST)**
- **Purpose:** land one Done task's own commits onto a target branch (for example the operator's main branch) with full gates on the **exact post-merge tip**.
- **Inputs:** `--task`, `--target <branch>`, `--authorizing-message`, `--source-ref` (for salvage, FAE-CTL-031).
- **Behaviour, in order:**
  1. Admission: memory available at least the landing floor (default 12 GiB), with a bounded wait (default 30 min); the I/O gate (FAE-HOST-010); the heavy-op lease at controller class.
  2. Select only the task's own commits: drop commits already upstream by patch identity. If none remain, the result is "nothing of its own" (infrastructure), never a success.
  3. Apply them onto the target tip. A conflict is resolved only by a deterministic structural merge of disjoint items, or the landing holds.
  4. Run the gates on the merged tree: an item-scoped formatting gate (no new formatting debt on changed items; whole-file only under a size bound, default 30k lines); compile of all targets including tests; the full test suite with only known, listed flakes tolerated; any frozen-region checks the target declares; the task's required test filter **across all test targets** with a positive executed-count assertion; every QA-declared check re-run on this tip (never cited from the earlier verdict); and own-hunk presence (every reviewed file and hunk of the task is present in the landed diff).
  5. On success, advance the target by fast-forward or a recorded merge commit. On any failure, reset the target to its starting tip and keep the attempt on a held branch.
- **Outputs:** the landed sha, every gate's result with its positive counts, or the held branch and the failing gate.
- **Failure modes:** host admission timeout (infrastructure); no own commits; a conflict; a gate failure; a vacuous gate (refused).
- **Acceptance:** tests: a landing whose filter matches zero tests holds; a landing that would drop a production hunk holds; a clean landing records every gate's count.

> Proposed 2026-11-05 (mosaic model-building). Source: mosaic `requirements/fae-model-building-assessment-controller.md` FAE-CTL-113, and `fae-model-building-assessment-execution.md` §2 B9; re-checked in `requirements/fae-recheck-2026-11-05.md`.
>
> **FAE-CTL-052 Document landings (MUST)** A target configuration MAY declare `document_globs` and `document_checks` (check commands, FAE-WF-095, each with a count field). When the target's diff classifier proves that a task's diff touches only `document_globs`, FAE-CTL-030's required test filter, formatting, compile and suite gates, and the protected-commit floor's test-run requirement, MUST be met by passing `document_checks` runs on the merged tree with positive counts, plus the target's always-required checks. The independent review and every other floor stay unchanged, and a diff that touches any other path gets the full code gates.
> - *Rationale:* a merged model is YAML, not code; requiring a Go test filter and Go formatting for a YAML-only landing either blocks it or invents a meaningless test.
> - *Acceptance:* tests: (a) a YAML-only diff lands on a passing model check with a positive count and a review; (b) the same diff plus one `.go` file needs the full code gates; (c) a document check reporting a zero count holds the landing.

**FAE-CTL-031 `fae land --salvage` (MUST)** A controller-direct landing of work that passed its gates but was escalated for infrastructure reasons MUST take the content from the tree its engine runs recorded as passing (restricted to the task's declared files), MUST re-run every gate of FAE-CTL-030 on the exact tip, and MUST write an evidence trail: the source tree hash, the base, the patch, the gate results and the original verdict references, stored as artifacts linked to the task. The task is then retired with reason contained at the landed sha.
- *Rationale:* salvage was frequent and needed a disciplined trail; one salvage nearly shipped tests without their production code.
- *Acceptance:* a salvage test verifies the trail and that own-hunk presence ran.

**FAE-CTL-032 `fae land --batch` (MUST)** Batch landing MUST land a list of tasks in dependency order, each gated as FAE-CTL-030 on the tip produced by the previous one, stopping at the first hold.
- *Acceptance:* a batch of three with the second failing lands one and holds the rest.

**FAE-CTL-033 `fae land --then-roll` (MUST)** Fast-forward-and-roll MUST land and then perform FAE-CTL-002 when the QA-clear window opens, capturing the roll's real exit status.
- *Acceptance:* a failed roll after a landing reports a nonzero status and an alert.

### 12.5 Census and retirement cleanup

**FAE-CTL-040 `fae census` (MUST)**
- **Purpose:** classify every Escalated (and optionally Blocked) task as superseded, contained, obsolete or live, with evidence.
- **Evidence rules:** superseded requires a named successor that exists and covers the acceptance; contained requires FAE-OPS-012 evidence; obsolete requires a ruling reference; anything else is live. Census output MUST include each task's state, branch tip, landing evidence and proposed reason.
- **Authorization:** read.
- **Outputs:** a census artifact.
- **Acceptance:** a rebased-ancestor branch is never classified contained.

**FAE-CTL-041 `fae census apply` (MUST)**
- **Purpose:** enact a reviewed census: retire each classified task through the audited retire verb, citing one batch authorizing message and its own reason, one at a time with a gap (default 2 s), stopping on a serialization failure that persists past the retry bound.
- **Acceptance:** a test applies a 10-row census and produces 10 audited retirements.

**FAE-CTL-042 `fae cleanup retired` (MUST)**
- **Purpose:** reclaim worktrees and branches of retired tasks. For each task: create an archive ref that keeps every commit reachable; verify the archive ref exists; then remove the worktree and delete the branch. It MUST NEVER delete a commit that is not reachable from an archive ref. It MUST perform only git operations and plane reads, so it cannot collide with the drive. It MUST support `--dry-run` and be paced.
- **Acceptance:** a test with an archive failure deletes nothing.

**FAE-CTL-043 `fae cleanup scratch` (MUST)** Scratch instances (created for probes and tests, marked scratch at creation) MUST be removable by one command that stops their daemon, backs up their plane (FAE-CP-013), archives their refs and then deletes the instance root. A non-scratch instance MUST be refused.
- *Acceptance:* a non-scratch instance is refused; a scratch instance leaves a backup.

### 12.6 Gate self-test and the admission gate

**FAE-CTL-050 `fae gates self-test` (MUST)** Every built-in gate (landing gates, formatting gate, filter gate, own-hunk gate, I/O gate, memory gate, overlap gate) MUST have a self-test mode that feeds it a vacuous or failing input and proves it refuses, and a passing input and proves it passes. The command runs all of them and fails if any gate cannot fail.
- *Rationale:* gate assertions rot silently unless they are proven able to fail.
- *Acceptance:* deliberately breaking one gate's assertion makes the self-test fail.

**FAE-CTL-051 `fae gate run` (MUST)** A host-heavy command run by the operator or a controller MUST be runnable through `fae gate run -- <command>`, which waits for the I/O and memory admission gate (FAE-HOST-010), takes the heavy-op lease at controller class, runs the command with the sanitized environment, and returns the command's own exit code. Waits MUST be bounded and loud: each wait slice names the holder in an alert, and a final timeout exits with a distinct code.
- *Rationale:* a controller landing once waited 58 minutes silently behind a sibling workspace's lock holder.
- *Acceptance:* a test with a held lease emits a holder line per slice and times out with the distinct code.

---

## 13. Daemon API, event stream and the optional UI child

The operator UI is specified separately. This section states only what any UI, or any other client, needs from the daemon.

### 13.1 Read API

**FAE-API-001 (MUST)** The daemon MUST expose these read RPCs, each subject to FAE-ROB-020 (off-thread, deadline, cached where noted, coalesced, snapshot age reported):

| RPC | Returns |
|---|---|
| Status | everything in FAE-CLI-012 |
| Board | tasks across all target configurations with state, stage, priority, molecule, target, disposition; filters |
| BacklogGraph | items, edges, ready set, dead edges, withheld reasons |
| Molecules (topology) | each running molecule's stage graph, with the normal-case role order from its workflow and the tasks at each stage (cached) |
| TaskHistory | a task's transitions, artifacts (by reference), decisions, roles traversed and each role's output, with provenance |
| Leases | role slots, heavy ops, deployment leases and pane reservations, with holders and waiters |
| ProviderHealth | per binding: score, class, holds, and a time series over a window (default 15 min) |
| Panes | live sessions with role, task, binding, owner and age |
| HostMetrics | a ring buffer (default 15 min at 5 s resolution) of memory available, CPU load, disk usage, I/O and memory pressure |
| Alerts | alert rows with acknowledgment state |
| Inbox | FAE-OPS-020 |
| ProphetSession / ProphetChatRead | per-task Prophet history and chat pages |
| Workflows / TargetConfigs | template reads, validation, diff and simulation |

- *Rationale:* a UI needs these reads and nothing privileged.
- *Acceptance:* each RPC has a contract test against the scripted adapter.

> Proposed 2026-11-06 (mosaic code-to-model audit). Source: mosaic `requirements/fae-hierarchical-workflows.md` FAE-API-035 (carried) and `fae-model-building-assessment-controller.md` FAE-CTL-112 (the RunReport part); the stats part is new; catalog D-69, D-75, C-103, C-104, C-113; audit in mosaic `requirements/fae-code-to-model-audit-2026-11-06.md` (row control 23).
>
> **FAE-API-035 (MUST)** A read RPC `Tree(task, stats?)`, joining FAE-API-001's reads, MUST return the run tree that contains the task: per node, its id, workflow id and revision, parent, detached flag, state, current stage, depth, failure or park reason, recursion-budget refusals, the change requests addressed to it (FAE-WF-103) and its mapped outputs; and tree totals (tasks, descendants, maximum depth). With `stats`, it MUST add wall time per task and per top-level stage, with FanOut branches rolled up under their stage and counted, and agent sessions, turns, duration and provider-stated cost from FAE-OBS-008, per stage and in total. Stats MUST be computed from the plane, so that they stay complete after the event window (FAE-API-021) has moved on, and the reply MUST state the plane sequence it reflects.
> - *Rationale:* mosaic's coordinator, its results stage (through FAE-WF-120) and its web UI read the run tree and its cost. At proposal (fae tree of 2026-11-06 08:25) there was no Tree method: TaskGet showed one task's parent and child references and Topology was per molecule; the working tree has since added `Tree` (`daemon/tree.go`, `api/schema.json` `rpcTree`).
> - *Acceptance:* tests: (a) a scripted root with 3 branches and 1 detached child returns 2 nodes, depth 2, and the branch count under its stage; (b) the stats totals equal the sum of the per-turn records; (c) a tree older than the event window still returns full stats.

### 13.2 Write API

**FAE-API-010 (MUST)** Every mutating CLI verb of Section 11 and every controller operation of Section 12 MUST have a corresponding RPC, with the same validation and audit. The CLI MUST be a thin client of these RPCs.
- *Rationale:* one enforcement point.
- *Acceptance:* a coverage test maps every CLI verb to one RPC.

**FAE-API-011 (MUST)** Mutating RPCs MUST be refused with a typed shutting-down error once shutdown begins (FAE-ROB-010), and with a typed maintenance error while a maintenance marker is present (except the maintenance start verb).
- *Acceptance:* tests for both.

**FAE-API-012 (MUST)** Mutating RPCs MUST be idempotent when called with an idempotence key: a repeated call with the same key returns the original result and writes nothing.
- *Rationale:* clients retry.
- *Acceptance:* a duplicate admit with the same key changes nothing.

> Proposed 2026-11-06 (mosaic code-to-model audit). Source: mosaic `requirements/fae-hierarchical-workflows.md` FAE-API-034 (carried); catalog D-67, P-61, P-91, C-74, C-76; audit in mosaic `requirements/fae-code-to-model-audit-2026-11-06.md` (rows control 1, 2; workflow W24).
>
> **FAE-API-034 (MUST)** `Admit` MUST accept `inputs` (an object validated against the workflow signature, FAE-WF-101), `parent_task` and `from_parent` (FAE-WF-102), and `max_sessions` (FAE-SCH-025), which only a root's admission MAY set: an admission that names `parent_task` and sets `max_sessions` MUST be refused as a binding error. A refusal MUST list every signature, binding and budget error at once.
> - *Rationale:* a controller starts every mosaic run with typed inputs, and every zoom run with a parent; at proposal (fae tree of 2026-11-06 08:25) Admit took free-text `content` and an unvalidated `follow_up_of` only; the working tree has since added these fields (`daemon/rpc.go:21-25`, `daemon/tree_admission.go`).
> - *Acceptance:* tests: (a) an Admit with valid `inputs` binds each as an `input:<name>` artifact; (b) an Admit naming a Done `parent_task` and a `from_parent` binding succeeds and links the tasks; (c) a refusal lists two missing inputs and one unknown parent artifact together.

### 13.3 Event stream

**FAE-API-020 (MUST)** The daemon MUST expose a server-streaming event RPC that delivers, in commit order, typed events for: task admitted, released, stage entered, stage left, transition (with from, to, owner, cause), parked (with reason), escalated (with kind), retired, cancelled, deferred; turn dispatched and collected (with binding); host command started and finished; gate result; external effect (intent, performed, refused, adopted); lease granted, waiting line, released; provider health change; session launched and reaped; alert raised; daemon lifecycle (starting, ready, shutting down, stopped); child lifecycle (started, crashed, restarted). Each event MUST carry a monotonic sequence number and a timestamp.
- *Rationale:* any UI animates from events; operators audit from them.
- *Acceptance:* a scripted run's event sequence is a deterministic function of the run.

**FAE-API-021 (MUST)** A subscriber MUST be able to resume from a sequence number; the daemon MUST retain at least a configurable window of events (default 24 h or 100k events) and MUST signal a gap when the requested sequence is older than the window.
- *Rationale:* reconnecting clients must not silently miss events.
- *Acceptance:* a reconnect from a retained sequence delivers every later event once; from an expired one, it receives a gap event first.

**FAE-API-022 (MUST)** A slow subscriber MUST NOT slow the drive or other subscribers. Per-subscriber buffers MUST be bounded; on overflow the subscriber MUST receive a gap event and resume from the plane.
- *Acceptance:* a stalled subscriber does not change drive round times.

> Proposed 2026-11-06 (mosaic code-to-model audit). Source: mosaic `requirements/fae-hierarchical-workflows.md` FAE-API-036 (carried) and `fae-model-building-assessment-controller.md` FAE-CTL-112 (the filters part); catalog C-100, D-74; audit in mosaic `requirements/fae-code-to-model-audit-2026-11-06.md` (row control 21).
>
> **FAE-API-036 (MUST)** Every task event MUST carry `parent_task` and `tree_root` (the task's own id for a root), recorded when the event is written. `Events`, paged and streamed, MUST accept filters on `task`, `tree_root`, `run_id`, `workflow_id` and event kind, and signal gaps as FAE-API-021 and FAE-API-022 do.
> - *Rationale:* a coordinator follows one tree of about 13 tasks per round; a filter replaces paging the instance-wide stream and joining histories by hand. At proposal (fae tree of 2026-11-06 08:25) events carried no tree identity and `Events` took only `after`, `include_retired` and `limit`; the working tree now fills `parent_task` and `tree_root` when events are read, from the task's current record (`daemon/event_filter.go:22-50`), so a deleted task's events lose their tree.
> - *Acceptance:* tests: (a) a subscription filtered on one `tree_root` receives only that tree's events, children and detached children included; (b) a filtered reconnect from an expired sequence gets a gap event first; (c) after a child task is deleted, a filter on its `tree_root` still returns its events.

### 13.4 Listener rules and the optional UI child

**FAE-API-030 (MUST)** Any network listener started by the daemon or its children (for example a UI server) MUST bind loopback only. A configured non-loopback address (including a wildcard address) MUST be refused fail-closed at start, in a message naming the value, with no listener started. Remote access is by a port forward over an authenticated channel, never by a wider bind.
- *Rationale:* loopback is the security boundary for a UI server.
- *Acceptance:* tests for 0.0.0.0, a LAN address and an IPv6 wildcard, all refused.

**FAE-API-031 (MUST)** Any such listener MUST require a token on every request. The token MUST be supplied by the daemon to the child from a mode-0600 file outside any repository, MUST never be logged (redacted in captured output), and MUST be retrievable by the operator through an explicit CLI command.
- *Acceptance:* an anonymous request is refused; logs contain no token.

**FAE-API-032 (MUST)** A UI child MUST be read-only by default. Writes MUST be enabled only with an explicit flag, and when enabled the child MUST receive a write capability that the daemon checks on every mutating RPC; status MUST show that writes are enabled. Writes from a UI MUST go through the same RPCs, validation, authorizing-message rule and audit as the CLI.
- *Acceptance:* a UI-originated write without the capability is refused.

**FAE-API-033 (MUST)** The daemon MUST support supervising one optional UI child as a generic capability: configured by binary path, assets directory, port, bind address, write flag and RPC log path; started after the daemon is ready; given a parent-death signal; stopped explicitly within the shutdown bound; restarted on crash with bounded exponential backoff (default 1 s doubling to 60 s, at most 10 restarts per hour, then left stopped with an alert); and reported in status (URL, pid, health, restart count). A UI failure, including a missing binary, MUST NEVER block or stop the daemon or the drive. Disabled MUST be byte-identical to an instance without the capability.
- *Acceptance:* tests: kill the child and it restarts while the drive keeps its round; SIGINT the daemon and the child exits within the bound with no orphan; a missing binary warns and the daemon starts; a binary that always dies stops restarting at the bound.

---

## 14. Host safety

Host safety is enforced by host admission (memory, pressure, load, leases), never by host-wide role leases. Under pressure the response is fewer concurrent operations, never an unprotected one.

### 14.1 Memory, pressure and load admission

**FAE-HOST-001 (MUST)** Before every session launch and every heavy op, the host guard MUST sample: memory available (from the kernel's memory statistics); swap total; I/O pressure "full" average over 10 s and over 60 s; memory pressure "full" average over 10 s and over 60 s; the 1-minute load average divided by the CPU count; and the live pane count.
- *Rationale:* every pane launch and every heavy op must be gated by measured host state.
- *Acceptance:* a launch-path test with injected telemetry shows each sample consulted.

**FAE-HOST-002 (MUST)** The guard MUST back off (bounded: default 10 checks at 3 s intervals) and then refuse and park when any of these holds (all defaults configurable): memory available below the soft floor (default 6 GiB); I/O pressure full avg10 above 10.0; memory pressure full avg10 above 5.0; load per core above 4.0; live panes at the limit. A value exactly at a threshold MUST proceed; a value just above it MUST back off.
- *Rationale:* the soft floor was the operational line below which the prior host became unstable; PSI catches pressure that memory-available alone misses.
- *Acceptance:* boundary tests at and just above each threshold.

**FAE-HOST-003 (MUST)** A hard circuit breaker MUST refuse immediately, without backoff, when memory available is below the hard floor (default 2 GiB; with no swap configured, default 4 GiB). A hard refusal MUST park the action and MUST raise an alert. It MUST NOT exit the drive.
- *Rationale:* a host with no swap has no cushion before the out-of-memory killer.
- *Acceptance:* tests with and without swap.

**FAE-HOST-004 (MUST)** When telemetry is unreadable, the soft guard MUST proceed and record the gap; the hard breaker MUST proceed only if memory available is unreadable (recording the uncertainty) and MUST refuse whenever it can positively read severe pressure. An unreadable lease ledger MUST refuse (fail closed).
- *Rationale:* fail safe on gaps, fail closed on proven danger and on accounting loss.
- *Acceptance:* tests for each telemetry gap.

**FAE-HOST-005 (MUST)** Lifting a pressure hold MUST require two readings at least 30 s apart both below threshold, and the guard MUST record avg10 and avg60 with their direction (rising or falling) for each reading.
- *Rationale:* a single avg10 sample can catch a trough; avg60 separates a spike from real escalation.
- *Acceptance:* a test where the second reading is above threshold keeps the hold.

**FAE-HOST-006 (MUST)** Launch admission MUST reserve an estimated per-pane memory footprint: a new session is admitted only when memory available is at least the hard floor plus (pending launches plus one) times the per-pane estimate (default 0.7 GiB, configurable). A reservation MUST be held for a warm-up period (default 180 s) after launch, so that a fan-out of N sessions cannot each pass the floor and together breach it. A deferral MUST be traced and MUST NOT fail the task.
- *Rationale:* a fan-out of four reviewers plus five other tasks walked memory available below the floor even though each launch passed the floor check; the measured median per-session footprint was about 0.66 GiB.
- *Acceptance:* a test with N pending launches that would breach the floor defers the excess.

**FAE-HOST-007 (MUST)** Heavy controller operations (landing, formatting gates over large files, package builds) MUST require memory available at or above a landing floor (default 12 GiB), waiting up to a bound (default 30 min), else reporting infrastructure, not failure.
- *Acceptance:* covered by FAE-CTL-030 tests.

### 14.2 Heavy operations and command shims

**FAE-HOST-010 (MUST)** The I/O admission gate for host-heavy commands run by Fae or through `fae gate run` MUST require I/O pressure full avg10 below a threshold (default 5.0) on two readings at least 30 s apart, then take the heavy-op lease.
- *Rationale:* concurrent fresh builds saturated storage (I/O pressure full about 90%) even when CPU and memory looked fine, and pressure stays high for 10 to 30 s after a build ends.
- *Acceptance:* tests for the two-reading rule.

**FAE-HOST-011 (MUST)** Every role session MUST have command shims on its PATH, placed first, for the build tool, the test runner and the formatters. Shims MUST: run compile-, test- and formatter-bearing invocations under the heavy-op lease via the broker verb; clamp build parallelism (default 2 jobs) and test threads (default at most 2); refuse a build output directory under the RAM-backed temporary filesystem (exit code 77 with a message naming the reason); refuse any build, test or formatter invocation by pre-code roles; refuse invocations whose working directory or target files are outside the session's own worktree; and refuse the version-control push and remote commands in Local mode. A pre-tool hook MUST deny attempts to bypass a shim by absolute path.
- *Rationale:* unbrokered compiles, formatters and probes saturated the host repeatedly.
- *Acceptance:* a shim test matrix covers each refusal and the lease path.

> Proposed 2026-11-05 (mosaic model-building), amendment to FAE-HOST-011, resolving its open conflict (provider sessions run with no tools, so the hook guards nothing): this requirement applies to every session that can execute commands, which today means agentic sessions granted a shell (FAE-RT-015) and HostCommands. A tool-less session needs no shims. An agentic session with a shell MUST have the shims and the registered pre-tool hook before its first tool call, or it MUST NOT launch. Source: mosaic `requirements/fae-model-building-assessment-execution.md` Top blocker 1.

> Proposed 2026-11-06 (mosaic code-to-model audit), amendment to FAE-HOST-011: the version-control shim MUST let a role session run read-only queries (`git rev-parse`, `status`, `diff`, `log`, `show`, `ls-files`) inside its own worktree. It still refuses commits and ref changes (the engine's VCS broker makes them), worktree management (FAE-VCS-002), and push and remote commands (FAE-MODE-002, which also forbids Local mode adding any other blanket ban). Source: mosaic catalog P-15 (`regen.py` calls `git rev-parse`); today every git command is refused in role sessions (`commandshim/role.go:256-257`); audit in mosaic `requirements/fae-code-to-model-audit-2026-11-06.md` (row agent AE-07).
> - *Acceptance:* tests: (a) `git rev-parse --show-toplevel` and `git status` succeed in an agentic shell; (b) `git commit`, `git checkout -b x`, `git worktree add` and `git push` are refused with their existing codes; (c) `git -C <path outside the worktree> log` is refused.

> Proposed 2026-11-07 (mosaic code-to-model third audit). Source: fae `commandshim/shim.go:202-204`, `:665`; probes `reaudit2/runtime/probe/hostcmd_test.go` (top-level, nested-refuse and nested-account `go run` all exit 77) and `launch_test.go` case G2 (agent shim `go run`: "unsupported Go verb"); mosaic `.claude/skills/mosaic-generate/scripts/mcheck:8`, compose `ontology-pipeline.yaml:448`, `:514`, `:867`, `:886` (`exec go run ./internal/devtools/check`).
>
> **FAE-RT-026 (MUST)** The Go shims (FAE-HOST-011) and the account-mode nested-command policy (FAE-HOST-014) MUST accept `go run <package inside the worktree> [args]` as a build under the heavy-op lease, with the same parallelism clamp, path scope and RAM-root rule as `go build`. The program it builds MUST then run in the calling command's process group, task cap and lease window, with the session's or run's environment and PATH (the shims still first). A package outside the worktree, a `-exec` or `-toolexec` flag, or a remote module path MUST be refused with exit 77 naming the reason. RAM-backed detection MUST use the filesystem type (statfs), not path prefixes, in every shim.
> - *Rationale:* model-building agents and command stages run their checker as `go run ./internal/devtools/check`. Today that is refused on every path, while a hand-rolled `go build -o $TMPDIR/x && $TMPDIR/x` works only in account mode, only off `/tmp` and `/var/tmp`, and is refused in agent shells for any worktree under `/var/tmp`. That is where the live instance's home is.
> - *Acceptance:* tests: (a) an agentic shell with `build` runs `go run ./cmd/check -h` in its worktree under a lease and gets its output; (b) a HostCommand `sh -c "go run ./cmd/check"` runs in `account` mode and is refused in `refuse` mode; (c) `go run ../other/cmd`, `go run -exec x ./cmd` and `go run example.com/m@latest` are refused with 77; (d) a worktree on a disk-backed `/var/tmp` accepts `go build -o bin/x`, and one on a tmpfs refuses it.

**FAE-HOST-012 (MUST)** Formatter shims MUST refuse a target file larger than a limit (default 1 MiB, or 30k lines) and MUST refuse a target whose module tree, after following module declarations, contains such a file, unless an explicit operator override is set or the formatter is run in per-file mode without recursion. Whole-file formatting of huge files MUST NOT run outside the lease. Refusals MUST name the item-scoped alternative.
- *Rationale:* one formatter run over an 80k-line file peaked at 12 to 20 GiB of RAM and drove memory available to 5 GiB; a crate-root target recursed into it.
- *Acceptance:* a test crate whose root declares a module over 30k lines is refused; a per-file invocation on a small file runs leased.

**FAE-HOST-013 (MUST)** In a multi-package build workspace, any invocation of the build tool's wrapper, including version probes and formatters, MUST be treated as heavy, because the cost is a property of the working tree, not of the verb.
- *Rationale:* a bare version probe triggered a full workspace sync with I/O pressure near 68 for 4.5 minutes.
- *Acceptance:* a shim test classifies a version probe in such a workspace as heavy.

**FAE-HOST-014 (MUST)** The heavy-op lease MUST bound concurrent heavy **processes**, not only lease holders: a leased command that spawns a nested build or test MUST have that nested invocation pass through the same shim and be accounted under the parent's lease or refused.
- *Rationale:* one leased test run spawned an unleased nested build.
- *Acceptance:* a test with a nested invocation shows it accounted or refused.

**FAE-HOST-015 (MUST)** Whole-suite test runs executed by the engine MUST use a fixed, low thread count (default 1) and MUST cap process creation for the run (a per-run process limit), so that tests which spawn processes cannot fork-storm the host.
- *Rationale:* parallel whole-suite runs of process-spawning tests produced resource-exhaustion errors and load of 23 on 16 cores.
- *Acceptance:* a fork-heavy test fixture under the engine run stays within the process limit.

**FAE-HOST-016 (MUST NOT)** Fae MUST NOT run unbounded recursive content searches over the home directory, the workspace root or any root containing many worktrees, in any built-in operation or any default brief. Briefs that need discovery MUST name exact paths or bounded roots.
- *Rationale:* one unbounded recursive search drove I/O pressure to 18 and wedged in uninterruptible sleep; another scanned the whole workspace from an orphaned process.
- *Acceptance:* a review of built-in operations and default templates finds no unbounded recursive search.

### 14.3 Temporary storage, process trees and leak detection

**FAE-HOST-020 (MUST)** Fae MUST treat the system temporary directory as RAM-backed. Build outputs, test scratch directories and backups MUST live on disk (under the instance root or the build output directory). Test scratch directories MUST be removed on every exit path. The watchdog MUST monitor both bytes and inodes of the temporary filesystem.
- *Rationale:* build directories under a 31 GiB RAM-backed temporary directory filled it unseen; 10,956 leaked test directories exhausted its inodes.
- *Acceptance:* Fae's own test suite leaves no entries in the temporary directory (a census test); the watchdog alerts on an inode fill test.

**FAE-HOST-021 (MUST)** Every process Fae launches (Dolt, UI child, agent sessions and their whole trees, host commands) MUST belong to the instance's process tree, tracked by an inventory recording pid, process start time, parent and role. Shutdown and reaping MUST operate on this inventory only.
- *Rationale:* safe teardown and accounting require ownership.
- *Acceptance:* after shutdown, no inventory process remains, and no foreign process was signalled.

**FAE-HOST-022 (MUST)** The daemon MUST detect fork loops and process leaks in its own tree: a per-command-name count and summed resident memory over its inventory, sampled every round, with an alert when a command name's count grows beyond a bound (default 200 processes or 4 GiB) or when defunct children accumulate (default 20). It SHOULD report, but MUST NOT act on, the same signals for foreign processes on the host.
- *Rationale:* a foreign recursive wrapper chain grew to over 3000 processes and 23 GiB; it was visible only by summing memory per command name, because each process was small.
- *Acceptance:* a test fixture spawning a chain of small processes in the instance tree raises the alert.

**FAE-HOST-023 (MUST)** The daemon MUST reap its defunct children promptly and MUST NOT accumulate zombie processes.
- *Acceptance:* after a scripted run, the daemon has no defunct children.

### 14.4 Launch context

**FAE-HOST-030 (MUST)** The daemon, its children and the watchdog MUST NOT inherit an agent session's sandbox. `fae serve` MUST detect a sandboxed launch context (for example a seccomp filter or a no-new-privileges flag inherited from an agent session) and refuse to start, naming the condition and the supported unsandboxed launcher. Fae MUST provide that launcher (for example a dedicated multiplexer pane or a user service) and MUST verify the started processes' sandbox state as part of post-start acceptance.
- *Rationale:* a daemon launched from an agent session lost every system call when that session ended, failing commits and stopping the drive.
- *Acceptance:* a launch from a simulated sandboxed context is refused; post-start acceptance records the sandbox state of the daemon, Dolt and the watchdog.

**FAE-HOST-031 (MUST)** Host-wide ledgers shared by instances (heavy-op pool, pane totals) MUST use a stable host-scoped path, a cross-process lock and owner identity (pid and process start time), and MUST reconcile dead-owner entries on every access.
- *Rationale:* a crashed run's entries otherwise wedge every instance.
- *Acceptance:* a dead owner's entry is cleared on the next access by another instance.

> Proposed 2026-11-07 (mosaic code-to-model re-audit). Source: fae `docs/requirements-followup-2026-11-06.md` ("Older binaries cannot rewrite that ledger"); `daemon/workflow_slots.go:246`, `scheduler/workflow_slots.go:92-93`, `:619-629` (startup writes Version 2), `:386-400` (fence); HEAD `scheduler/ledger.go:141` (old version gate); probe `reaudit/capacity/oldprobe` with `newv2_test.go`.
>
> **FAE-HOST-032 (MUST)** A binary MUST NOT write a host-ledger schema that a live participant cannot read. A live participant is any owner of a pool lease, pane reservation, host-limit record, provider pool or workflow instance. Before upgrading a shared ledger's schema, a binary MUST find every live participant. If any of them has not recorded that it reads the new schema, the binary MUST leave the ledger at its current schema and, per configuration, either refuse to start, naming those participants, or start with the features that need the new schema refused and reported (reason `host_ledger`). A newer binary starting MUST NEVER cost an older participant its heavy-op leases, pane reservations, host-limit registration, provider pools or its ability to restart. Each participant MUST record the newest schema it reads. The upgrade happens, with an audit row, on the first ledger access after the last participant that cannot read the new schema is proved dead.
> - *Rationale:* today a new-binary daemon upgrades the shared ledger at startup. From then on the older binary (HEAD) fails every heavy-op enqueue, host-limit registration and ledger reopen with "lease ledger version/capacity conflict", and the new binary fences every slot acquisition while the older one lives, so both stall. A mosaic pilot on a new fae beside the operator's live, older instance would stop the live instance's builds and launches.
> - *Acceptance:* tests: (a) an older binary holding a heavy-op lease and host limits, then a newer binary starting on the same ledger: the older still enqueues, releases and restarts, and the newer reports workflow slots unavailable naming the older participant, or refuses to start, per configuration; (b) once the older participant is proved dead, the next access upgrades the schema and audits it; (c) a binary never writes a schema it cannot read.

> Proposed 2026-11-07 (mosaic code-to-model re-audit). Source: fae `provider/session_view.go:56-62` (probe at launch only); the daemon test above admitted the task and escalated it after four refusals; `REQUIREMENTS.md` FAE-CLI-054.
>
> **FAE-OPS-036 (MUST)** The daemon MUST probe session-isolation capability (the namespace tool, user and mount namespaces, and the mount plan's canary) at `serve` start and on each `fae doctor` run, and record the result as a host fact with the failing step. `fae doctor` MUST report it with the remedy for the detected platform. Admission MUST refuse, with a typed reason naming the stage and the missing capability, a task whose workflow revision contains a stage that FAE-SEC-012 requires to have a view when the host lacks one. It MUST NOT admit such a task and let it escalate after dispatch.
> - *Rationale:* today the first sign is a SameGate escalation per task after four refusals.
> - *Acceptance:* tests: (a) with the namespace tool absent, `fae doctor` fails the isolation check and names the missing tool and the remedy; (b) on that host, admitting a workflow whose stage is a shell under a read policy is refused at admission with the stage named, and a workflow without one is admitted; (c) after the capability appears, a re-probe (restart or doctor) clears the fact.

> Proposed 2026-11-07 (mosaic code-to-model third audit). Source: fae `runtime/direct.go:464` (session base `PATH=/usr/bin:/bin`), `commandshim/role.go` ("go is not on the session's base PATH"), `cmd/fae/doctor.go` (`go-toolchain:goroot` passes on the daemon's own PATH); probe cases G and G2; mosaic `mcheck` under a fresh provider HOME fails with "module lookup disabled" unless `GOMODCACHE` is set.
>
> **FAE-OPS-037 (SHOULD)** `fae doctor` and live-configuration validation MUST resolve each role-shim tool (FAE-HOST-011) and each `shell_allow` executable against the *session* base PATH (the runtime's PATH plus the live configuration's `PATH`), not the daemon's own. They MUST report every tool that a role with the matching capability would be refused, naming the configuration key that fixes it. For Go, they MUST report when the session's module and build caches resolve to the per-task provider HOME with no network module source, which would make the first build of every task fail or download.
> - *Rationale:* doctor reports the Go toolchain as present, while every session shell gets "go is not on the session's base PATH". Mosaic's checker then fails on module lookup in the private HOME.
> - *Acceptance:* tests: (a) with `go` only in `/usr/local/go/bin` and no live `PATH`, doctor warns "go not on session PATH; set environment.PATH"; (b) with `PATH` set, the warning clears and a build-capable shell's `go build` runs; (c) with `GOPROXY=off` and no `GOMODCACHE`, doctor warns about empty per-task module caches.

---

## 15. Observability

**FAE-OBS-001 (MUST)** Every log line MUST carry an RFC 3339 timestamp with time zone, the instance name, a component name and, where relevant, the task id, stage and turn id. Logs MUST be written to a rotating file under the instance root as well as to standard error.
- *Rationale:* the prior daemon log had no timestamps, so timing analysis had to be reconstructed from the database.
- *Acceptance:* every line in a scripted run's log parses with a timestamp.

**FAE-OBS-002 (MUST)** Status (FAE-CLI-012) MUST include the UI URL and health, the plane identity, the drive state and the maintenance marker, and MUST be answerable during shutdown.
- *Acceptance:* covered by FAE-CLI-012 and FAE-ROB-010 tests.

**FAE-OBS-003 (MUST)** Every task MUST have a history view (FAE-API-001 TaskHistory) showing every transition with owner and cause, every artifact with provenance, every decision, every operator message and verb that touched it, and every lease wait.
- *Acceptance:* the history of a scripted task lists every artifact and transition in order.

**FAE-OBS-004 (MUST)** The daemon MUST expose metrics through an RPC (and MAY expose them on a loopback-only, token-gated endpoint): tasks by state; transitions per hour; stage wall time (median and 90th percentile) per stage; admission-to-release wait; release-to-Done time; gate rejection rate per gate with rejection causes; attempts per task; repeat-grant share of role time; time lost to restarts; withheld counts by reason; lease utilisation per role; heavy-op queue wait; host samples; provider health; external effects by outcome.
- *Rationale:* the parallelism analysis of the prior system had to rebuild these by hand.
- *Acceptance:* each metric has a test against a scripted run with known timings.

**FAE-OBS-005 (MUST)** The daemon MUST keep an alerts log on the plane and as a file: drive exits, re-arm budget exhaustion, hard host refusals, filesystem headroom, process-leak detection, stuck commit stages (FAE-VCS-009), aborted rolls, UI child restart exhaustion, dead edges, and route exhaustion lasting beyond a bound (default 30 min). Alerts MUST be acknowledged by an audited verb.
- *Acceptance:* each alert source has a test that raises it.

**FAE-OBS-006 (MUST)** Every non-ok provider observation, every park, every refusal and every escalation MUST produce exactly one log line and one event; never a line without a row or a row without a line (FAE-RT-009 applies the same rule to health).
- *Acceptance:* a pairing test over a fault-injected run.

**FAE-OBS-007 (SHOULD)** Each turn SHOULD record a correlation reference to the agent CLI's own transcript (session id, pane, timestamps) so the turn can be retrieved from the agent's own store without duplicating raw text.
- *Acceptance:* the history of a turn shows the reference.

> Proposed 2026-11-06 (mosaic code-to-model audit). Source: mosaic catalog D-74, C-101, P-73; audit in mosaic `requirements/fae-code-to-model-audit-2026-11-06.md` (row control 22).
>
> **FAE-OBS-008 (MUST)** For every session attempt of a turn, the runtime adapter MUST record what the provider reports at the end of the session: provider session id, result subtype, error flag, number of assistant turns, provider-stated duration and provider-stated cost, plus the attempt number within the stage visit, the visit and the wall time measured by Fae. Each value MUST be labelled exact (provider-stated or measured) or absent; Fae MUST NOT estimate a missing cost. The record MUST be kept in TaskHistory and emitted as one `turn-stats` event, and attempt numbering MUST continue across resumes. A provider-stated cost is a reading, not the spend inference FAE-CLI-052 forbids.
> - *Rationale:* mosaic reports cost, turns and wall time per stage and branch for every round. At proposal (fae tree of 2026-11-06 08:25) the Claude adapter dropped `num_turns`, `duration_ms` and `total_cost_usd` from the terminal `result` record; the working tree has since added them (`provider/claude.go:131-133`, `provider/turn_stats.go`, `engine/turn_stats.go`), numbering attempts across the whole task (`engine/turn_stats.go:20`).
> - *Acceptance:* tests: (a) a scripted Claude result with `num_turns: 7, total_cost_usd: 0.42` yields a turn-stats event and a history entry with those values labelled exact; (b) a provider that reports no cost yields `cost: absent`; (c) a turn resumed after a restart is attempt 2, not 1, and the next stage's first session is attempt 1.

> Proposed 2026-11-06 (mosaic code-to-model audit). Source: mosaic catalog P-75; audit in mosaic `requirements/fae-code-to-model-audit-2026-11-06.md` (row control 26).
>
> **FAE-OBS-009 (SHOULD)** While an agentic turn (FAE-RT-015) runs, the daemon SHOULD emit a bounded, rate-limited event per call of the agent CLI's own tools (for Claude Code, each `tool_use` block of the main session's stream-json, or the pre-tool hook), not only per call of a Fae tool protocol: the tool name, the paths it read or wrote relative to the worktree or read root, and whether the policy allowed or refused it, without file content. Refused reads and out-of-scope writes MUST still be emitted when the rate limit applies.
> - *Rationale:* a coordinator watching a round can stop an agent that drifts outside its area early, instead of at collection; at proposal (fae tree of 2026-11-06 08:25) refused reads went only to an audit file (`provider/read_policy.go:50-67`) and out-of-scope writes were found only at collection; the working tree now emits `tool-call` events, but only from `AgenticToolbox.Call` (`provider/tool_events.go`, `provider/agentic.go:151`), which Claude reaches only through a top-level `tool_calls` field (`provider/claude.go:141`, `:219-224`) that Claude Code does not emit.
> - *Acceptance:* tests: (a) a recorded Claude Code stream-json transcript with 3 `Read` tool_use blocks, replayed through the adapter, emits 3 tool events naming the files; (b) a refused read emits one event marked refused even when the rate limit is reached; (c) no event carries file content.

> Proposed 2026-11-07 (mosaic code-to-model re-audit). Source: fae `daemon/tree.go:20-34`, `:130-226`; mosaic `.claude/skills/mosaic-generate/compose/ontology-pipeline-light.yaml:614-621` (results lists "turns that needed a retry or a gap turn, from `mosaic compose tree --stats`"), `internal/compose/tree.go:160-172`.
>
> **FAE-OBS-010 (MUST)** The run tree (FAE-API-035) MUST list, per node, every failure route taken (FAE-WF-112): stage, visit, branch path, reason, the handler entered and whether the handler's branch or stage then passed. It MUST also list every Gate, Branch or shaped-command evaluation that routed to a failure edge, citing its gate record (FAE-WF-063). With `stats`, the handler's sessions MUST roll up under the primary stage and be counted apart.
> - *Rationale:* retries and gap turns are recorded gaps that mosaic's results must report apart from scores. Today they are only in task history, so an operator or a `run_stats` reader cannot see them in the tree.
> - *Acceptance:* tests: (a) a scripted run where one build branch fails into `build_retry` and a judge into `judge_gap` shows both routes, with reason `budget`, under their nodes; (b) a gate that failed into a ChangeRequest stage is listed with its gate record id.

> Proposed 2026-11-07 (mosaic code-to-model re-audit). Source: fae `daemon/tree.go:134-138` (matches `recursion-budget`), `engine/subworkflow.go:625-626` (`recursion_budget`), `engine/engine.go:971-979` (`width`); native probe `TestZZTreeReadShowsRecursionRefusal` (no refusal listed for a recorded `recursion_budget` failure); mosaic `internal/compose/tree.go:101-131`, `:170-172` (lists budget and `max_branches` refusals).
>
> **FAE-OBS-011 (MUST)** The run tree MUST list every refusal of the node, with its stage, visit, branch path, reason and detail: `recursion_budget`, `width` and `binding`, whether the refusal took a failure route or escalated. Refusals MUST be read from the reason the engine records, not from an event spelling, so that a renamed event kind cannot hide them.
> - *Rationale:* the tree's `recursion_refusals` is never populated, because the reader matches `recursion-budget` and the engine records `recursion_budget`. Width and binding refusals are not listed at all. mosaic's tree shows both kinds of budget refusal.
> - *Acceptance:* tests: (a) a self-recursive run that exceeds `max_children_per_visit` lists the refusal under the parent node; (b) a FanOut over a list longer than `max` lists a `width` refusal; (c) an `input_map` projection reading a missing field lists a `binding` refusal.

> Proposed 2026-11-07 (mosaic code-to-model third audit). Source: fae `engine/scenario.go` (the simulation report has `timeline`, `transitions`, `evidence`, `gates`, `records`, but no `template-presence` or `projection-default` events); live `template-presence` events in `reaudit2/e2e/live/07-events.json`; `workflow/GRAMMAR.md` ("Defaults emit `projection-default` events").
>
> **FAE-OBS-012 (SHOULD)** The rehearsal report (FAE-WF-105) SHOULD carry, per dispatch, what a live dispatch records about its rendering: the presence decisions (FAE-WF-141), each projection default taken (FAE-WF-122) with the evidence it read, and the rendered brief's content hash. `fae workflow simulate --tree` SHOULD print the presence decisions and defaults under each node.
> - *Rationale:* a mosaic brief now holds root-only and child-only sections. Which section an agent will see is decided per dispatch, and it is visible only in a live daemon's events. The rehearsal is meant to check exactly this before agent time is spent.
> - *Acceptance:* tests: (a) a rehearsal of a root and a child with `{{#present input.contract}}` reports `false` for the root's dispatch and `true` for the child's; (b) an `items_from` element missing `scenarios`, with `default: []`, reports one projection default with its evidence ids.

---

## 16. Security

**FAE-SEC-001 (MUST)** Fae MUST run no unauthenticated listener and no non-loopback listener (FAE-ARC-005, FAE-ARC-006, FAE-API-030, FAE-API-031).
- *Acceptance:* a listener census of a running instance with the UI child enabled shows only the Unix socket and a loopback, token-gated UI port.

**FAE-SEC-002 (MUST)** Least privilege MUST apply to every role session: the tool profile is the minimum the stage needs, external-effect grants are per tool and per role and only in Normal live mode, and pre-code roles receive no build or test capability.
- *Acceptance:* the generated configurations for each role in each mode match a golden set.

**FAE-SEC-003 (MUST)** Secrets (the instance credential, UI token, Dolt credentials, provider credentials) MUST be stored in mode-0600 files outside any repository and worktree, MUST never appear in briefs, artifacts, logs, events or audit rows (redacted where captured output might contain them), and MUST never be passed to role sessions except the provider credentials that the agent CLI itself needs, by the CLI's own mechanism.
- *Acceptance:* a scan of the plane, logs and artifacts of a scripted run for planted secret values finds none.

**FAE-SEC-004 (MUST)** All external effects MUST be audited (FAE-EXT-006), pinned (FAE-EXT-002), idempotent (FAE-EXT-005) and recorded before validation (FAE-EXT-004).
- *Acceptance:* covered by Section 9 tests.

**FAE-SEC-005 (MUST)** Task text, acceptance text and attached documents MUST be treated as untrusted data: inserted into briefs only by structured binding (FAE-WF-043) and into commands only as literal arguments (FAE-WF-035).
- *Acceptance:* injection tests.

**FAE-SEC-006 (MUST)** Fae MUST NOT deploy to production, spend money, or contact systems other than the configured ticket and code-review systems. Test-account deployment, when a workflow uses it, MUST run under an exclusive deployment lease scoped to the account, environment and exact worktree revision, and only against accounts the target configuration marks as approved test accounts.
- *Rationale:* operator non-negotiable.
- *Acceptance:* a deployment request against an account not marked approved is refused.

**FAE-SEC-007 (SHOULD)** Before writing code that touches credentials, network exposure or file serving, the implementers SHOULD consult the organisation's security guidance for those domains and record the outcome in the change's review.
- *Acceptance:* the review records for those changes cite the guidance consulted.

> Proposed 2026-11-05 (mosaic model-building). Source: mosaic `requirements/fae-model-building-assessment-execution.md` §3.1 FAE-SEC-010 and `fae-model-building-assessment-controller.md` FAE-CTL-106; re-checked in `requirements/fae-recheck-2026-11-05.md`.
>
> **FAE-SEC-008 (MUST)** A role and a stage MAY declare a read policy `reads: {allow: [globs], deny: [globs]}` over the worktree and each read root (FAE-RT-016); a task's admission MAY narrow it. Deny MUST win, and a stage or task policy MUST only narrow its role's; a widening MUST be refused at validation or admission. The effective policy MUST filter inline files, the repository path list in every brief, every tool read, listing and search result (FAE-RT-015), and bound inputs whose provenance names a denied stage. When a shell is granted, the policy MUST NOT rest on the hook alone: the session's filesystem view MUST omit denied paths, for example through a sparse worktree or a mount namespace. Each turn's provenance MUST record its effective read policy, and every refused read MUST be audited naming the task, role and path.
> - *Rationale:* a model-building score is blind only if builders never see the benchmark, oracle and judge files and judges never see builders' notes, including between stages of one task; today scope is per task and every brief lists every tracked path name.
> - *Acceptance:* tests: (a) a builder role denied `BENCHMARK*`, `oracle*/**` and `judge*/**` gets a path list without those names, and a tool read, a search and a shell `cat` of one are refused and audited; (b) two stages of one task with different policies see different files; (c) a stage that widens its role's policy fails validation; (d) TaskHistory shows each turn's effective policy.

> Proposed 2026-11-06 (mosaic code-to-model audit), amendment to FAE-SEC-008 and FAE-RT-016: (1) the per-session filesystem view that omits denied paths, and the read-only view of read roots, MUST be built per session (for example with a mount namespace, bind mounts or a sparse per-session checkout). They MUST NOT be built by removing, moving or changing the mode of files in the shared worktree or read root, because read-only parallel branches share the task worktree and many sessions share one read root at once. A daemon crash at any point MUST leave the contents and modes of the worktree and the roots unchanged. (2) Inside a FanOut branch, a stage's `reads` globs MAY be parameterised by the branch element (`{{item.<field>}}`), so that a branch can be denied every sibling's files but its own. Source: mosaic `requirements/fae-model-building-assessment-execution.md` §3.2 (per-element read policy in the FAE-WF-097 update); catalog D-03, D-20, D-21, D-78, P-22, P-77, P-78, P-79; today the mask moves denied files into a RAM-backed temporary directory and restores them afterwards (`provider/read_policy.go:620-700`), and read roots are chmod-locked with a per-session restore that the first finishing session undoes for the others (`provider/read_policy.go:811-846`, `daemon/read_roots.go:391-426`); audit in mosaic `requirements/fae-code-to-model-audit-2026-11-06.md` (rows gates G24, G28; agent AE-03).
> - *Acceptance:* tests: (a) two concurrent read-only branches in one task with different deny lists each read the files the other is denied; (b) killing the daemon during a turn leaves every denied file in the worktree; (c) of two concurrent sessions on one read root, when the first ends the second still cannot write the root; (d) a branch with `deny: [notes/**]` and `allow: [notes/{{item.id}}.md]` reads its own notes and no sibling's.

> Proposed 2026-11-07 (mosaic code-to-model re-audit), amendment to FAE-SEC-008 and FAE-RT-016 (2026-11-06 amendment, part (2) and acceptance (d)): deny always wins, at every layer and against `deny_paths`; an `allow` entry MUST NOT re-admit a path that any layer's `deny` matches, and a branch placeholder only binds a value. To deny a branch its siblings' files, a `deny` glob inside a FanOut branch MAY contain `{{siblings.<field>}}`, which expands at dispatch to one glob per *other* element of the innermost enclosing FanOut (none when the branch is alone; a sibling missing the field is a `binding` failure, FAE-WF-115); `{{item.<field>}}` keeps its meaning in `allow` and `deny`. Validation MUST warn when an `allow` glob of a layer can match only paths that a `deny` glob of the same or a broader layer matches (a dead allow). Acceptance (d) is replaced by: (d) in a FanOut over items `a`, `b`, `c`, a branch with `reads: {deny: ["notes/{{siblings.id}}.md"]}` reads `notes/a.md` (its own) and is refused `notes/b.md` and `notes/c.md`, each refusal audited; (e) a branch with `deny: [notes/**]` and `allow: [notes/{{item.id}}.md]` reads none of `notes/`, and validation reports the dead allow; (f) a target `deny_paths: [notes/a.md]` still refuses branch `a` its own file.

> Proposed 2026-11-05 (mosaic model-building). Source: mosaic `requirements/fae-model-building-assessment-controller.md` FAE-CTL-109 (content policy and deny paths); re-checked in `requirements/fae-recheck-2026-11-05.md`.
>
> **FAE-SEC-009 (MUST)** A target configuration MAY declare `content_policy: {deny_literals, deny_patterns}` (case-insensitive) and `deny_paths: [globs]`. Before any commit, integration, landing or push, the daemon MUST scan the added lines of the diff, the commit message and the paths against `content_policy`; a match MUST refuse with the policy entry's index, never its text, and the scan MUST NOT be skippable by content or authority. `deny_paths` MUST extend the protected path list: a matching path can never enter any task's scope, operator-declared files included, nor any read root's readable set, nor any commit. Policy text is secret-like (FAE-SEC-003) and MUST NOT appear in events, logs, artifacts or briefs.
> - *Rationale:* certain names must never be published from the operator's repositories, and certain files (encrypted keys, TLS keys, environment files inside a snapshot) must never be opened; today these are instructions to agents, which FAE-WF-042 says not to rely on.
> - *Acceptance:* tests: (a) a diff adding a denied literal is refused and the refusal names the entry index only; (b) a declared file matching `deny_paths` is refused at admission, even from the operator; (c) a tool read of a denied path inside a read root is refused; (d) a scan of logs, events and artifacts finds no policy text.

> Proposed 2026-11-06 (mosaic code-to-model audit). Source: mosaic `requirements/fae-model-building-assessment-execution.md` FAE-SEC-010 (the write part, which FAE-SEC-008 did not carry) and `fae-model-building-assessment-controller.md` FAE-CTL-104 (`writes` globs); `.claude/skills/mosaic-generate/compose/ontology-pipeline.yaml:328-329`, `:597`, `:646-648` (catalog P-80); `docs/DESIGN.md:707-710`, `:1663-1667` (D-12, D-19); catalog D-14, D-77, P-05; audit in mosaic `requirements/fae-code-to-model-audit-2026-11-06.md` (rows gates G23; agent AE-04).
>
> **FAE-SEC-011 (MUST)** A role and any stage MAY declare `writes: [globs]` over the worktree (over the output directory in a `produces: artifacts` task, FAE-WF-104), and a task's admission MAY narrow it; inside a FanOut branch the globs MAY be parameterised by the branch element (`{{item.<field>}}`) or bound from inputs (FAE-CTL-107). A stage's effective write scope is the intersection of its role's, its stage's, its branch's and its task's; a layer that declares no `writes` does not narrow it, and a generator's `generate.writes` (FAE-WF-116) is one of its stage's layers. A stage none of whose layers declares `writes` keeps today's scope: its task's declared files for a change producer, nothing for any other stage. A turn, or a generator command (FAE-WF-116) within its `generate.writes`, MAY create, edit and delete files that match the effective scope, including files that did not exist at dispatch: the toolbox, the agent CLI's permission rules and the collection diff MUST all match against the globs, and everything else MUST be reverted at collection and refused, naming the paths (FAE-RT-015). A stage or task MUST only narrow its role's scope; a widening MUST be refused at validation or admission. Edit and write tools MUST be legal on any stage with a non-empty effective write scope, not only on code change producers. A stage that writes this way is a change producer, and a parallel branch of one gets its own worktree. When the task's diff touches only the target's `document_globs`, FAE-CTL-052 decides its landing and the code-only floors (test first, frozen test) do not apply; otherwise every floor applies as to any change producer. In a `produces: artifacts` task (FAE-WF-104) the effective scope lies inside the output directory and nothing is committed.
> - *Rationale:* a mosaic area owns `fragments/<id>.yaml`, `notes/<id>.md`, `skips/<id>*.json` and `gen/<id>/**`, whose file names are not known in advance; a judge owns only `judge<n>/`, the enumerator only `mechanisms/`, and only the coordinator writes `fixtures.yaml`. At the 08:25 UTC audit write scope existed only on FanOut stages and narrowed an exact declared-file list, a new file under a glob was reverted, and only change-producer stages could write (cited then as `workflow/fan.go:193`, `daemon/live_scope.go:441-457`, `provider/agentic.go:462`, `:888`, `workflow/agentic.go:112-124`); fae's working tree at 09:58 UTC implements role, stage and task `writes` (`workflow/filesystem.go:28-58`, `:77-121`, `provider/agentic.go:488-489`, `daemon/task_directories.go:48-52`) but gives a parallel branch its own worktree only for a ChangeProducer or `generate` stage (`daemon/live.go:429`).
> - *Acceptance:* tests: (a) a branch with `writes: [gen/{{item.id}}/**]` creates `gen/a/new.yaml`, and it is kept and lands; (b) the same branch writing `gen/b/x.yaml` is reverted and refused; (c) a judge stage outside any FanOut with `writes: [judge/**]` keeps `judge/r.json` and has `fragments/x.yaml` reverted; (d) a stage that widens its role's `writes` fails validation; (e) a YAML-only document stage on a target whose `document_globs` cover its files lands under FAE-CTL-052 with no test-first evidence, while the same stage writing a `.go` file needs it; (f) in a `produces: artifacts` task a branch with `writes: [fragments/{{item.id}}.yaml]` writes its file in the output directory and has a sibling's file refused.

> Proposed 2026-11-07 (mosaic code-to-model re-audit). Source: fae `effects/broker.go:402`, `provider/codex.go:858-886`, `daemon/admission.go:322`, `daemon/task_directories.go:71`, `provider/session_view.go:38-62`; probes P1 to P3; `go test ./daemon -run TestLiveAccountModeRecordsNestedCommandsDurably` escalated with "refused before exec: session filesystem isolation unavailable".
>
> **FAE-SEC-012 (MUST)** A session or command MUST require a namespace filesystem view only where a requirement cannot be enforced otherwise: a granted shell under a read policy or `deny_paths` (FAE-SEC-008), and a granted shell or command with a read root the daemon's user can write (FAE-RT-016). Everything else MUST launch on a host without namespace capability: a shell-less agentic session is confined by the CLI's working directories and permission rules (FAE-RT-015) plus the collection diff (FAE-SEC-011). A HostCommand or session whose only extra path is its task's output directory, staging area or an ancestor's output directory relies on the write collectors (FAE-WF-104, FAE-WF-111). A read root already read-only to the daemon's user (a read-only mount, or not writable by that user) satisfies FAE-RT-016 without a view. A target MAY additionally opt in to running shells restricted by `shell_allow` (FAE-WF-119) to its `runtime_mounts` executables (FAE-RT-024) without a view, with the read policy then enforced by the CLI's permission rules. That choice MUST be recorded in each turn's provenance. Concurrent sessions sharing a writable directory MUST then have pairwise-disjoint effective write scopes, so collection can attribute every change.
> - *Rationale:* every task now has an output directory, so on a host without namespaces fae refuses every agentic session and every HostCommand, its own default workflow included. No requirement asks for that, and RT-015's CLI confinement and SEC-011's collectors already cover these cases.
> - *Acceptance:* on a host where the namespace probe fails, tests: (a) the default workflow's HostCommands run and a code task reaches Done; (b) a shell-less agentic Claude session with a read root and an artifacts-only output directory launches, reads the root and writes its output, and a write elsewhere is reverted and refused; (c) a shell session with a read policy is refused before launch with a typed reason; (d) a read root mounted read-only by the operator is accepted for a shell session without a view, and a writable one is refused; (e) a `shell_allow` session on an opted-in target runs its checker, two concurrent branches with disjoint `writes` both collect, and overlapping scopes are refused at dispatch.

> Proposed 2026-11-07 (mosaic code-to-model third audit), amendment to FAE-SEC-012: (1) An ancestor's output directory is like the task's own output and staging directories: it alone MUST NOT require a view. Without a view, the session MUST snapshot each ancestor output before launch, and MUST refuse the turn and restore the directory if it changed. (2) "Confined by the CLI's permission rules" means every effective read layer is expressed as rules: task, role and stage policy; `deny_paths`; each read root's own `deny`; and policy globs as they apply inside read roots and ancestor outputs. That includes allow narrowing, so a root or ancestor output is never a blanket `Read(<path>/**)` when a layer narrows it. A layer that cannot be expressed MUST make the launch require a view. (3) A granted shell without `shell_allow` MUST NOT launch without a view unless the target opts in (`unconfined_shell_without_view: true`), recorded in provenance. Without a view such a shell can write the instance root, ledgers and other tasks' worktrees, and no collector sees it. (4) With the runtime-mount opt-in, the pre-tool hook MUST refuse a `shell_allow` argument that names a path the effective read policy denies. (5) Admission, the executor's isolation decision, the provenance label and the dispatch refusal MUST use one function over the same inputs, ancestor outputs included. The label MUST reflect the view actually built, and the refusal MUST name the rule (read policy, writable root `<name>`, deny paths). Acceptance adds: (f) a shell session whose only extra path is an ancestor output launches without a view, and its write into that output is restored and refused; (g) a shell-less session with root `deny: [oracle/**]`, and one with task `reads.deny: [oracle/**]`, each get a CLI deny rule for `<root>/oracle/**`; (h) a full shell without a view is refused unless opted in; (i) under the opt-in, `mcheck <denied path>` is refused by the hook. Source: probes `reaudit2/runtime` cases A, B, C2, F, G2, O; fae `provider/isolation_capability.go:100-115`, `runtime/claude_permissions.go:56-81`, `daemon/session_isolation.go:118-178`, `execution/executor.go:390`.

---

## 17. Testing and verification of Fae itself

**FAE-VER-001 (MUST)** Every check Fae ships (gate, guard, shim refusal, validator rule, census) MUST have a **firing control**: a test that feeds it an input it must refuse and proves it refuses, alongside the test that it passes a good input. A check without a firing control MUST NOT be counted as coverage.
- *Rationale:* a check that cannot fail proves nothing; several prior checks passed vacuously.
- *Acceptance:* a coverage report lists every check with its firing-control test; CI fails on a check with none.

**FAE-VER-002 (MUST)** A firing control MUST prove only what it exercises; a test proving that a check fires on one input MUST NOT be cited as proof that it fires on another class, that it is wired into the live path, or that the base case passes. Live-path reachability MUST be proven separately: each safety check MUST have at least one test that drives it through the real daemon path (scripted adapter), not only a unit call.
- *Rationale:* a capability passed unit tests and independent review yet was never wired into the live intake path.
- *Acceptance:* each safety check has a daemon-path test in addition to its unit test.

**FAE-VER-003 (MUST)** The workflow engine MUST pass the parity corpus (FAE-WF-094) and MUST be property-tested over **generated workflows**: random valid graphs built from the primitive library, with random role outputs and host-command results, asserting that: every kernel floor holds on every run; no stage runs without its inputs; every run terminates (Done, Escalated, Retired or Cancelled, or a parked state that resumes when its cause clears); no terminal task transitions again; every transition has an audit row; resume at any point reaches the same terminal outcome; and every gate that passes has read at least one evidence item.
- *Acceptance:* the property suite runs in CI with at least 10,000 generated cases per run (default) and a fixed seed corpus.

**FAE-VER-004 (MUST)** The validator MUST be tested with a mutation corpus: for each kernel floor, generated workflows that violate it in one way each, all of which must be rejected.
- *Acceptance:* the corpus covers every floor in FAE-WF-070.

**FAE-VER-005 (MUST)** No vacuous gates in Fae's own CI: every CI test step MUST assert a nonzero executed test count, and a step that reports zero executed or only skipped tests MUST fail.
- *Acceptance:* a CI self-test with an empty filter fails.

**FAE-VER-006 (MUST)** Fae's own test environment MUST be clean and sanitized: tests MUST NOT read the developer's instance configuration, Fae environment variables, real control planes or real host ledgers; every test MUST use injected configuration, a per-test instance root on disk (never the RAM-backed temporary directory), and fake or scripted adapters; integration tests that need Dolt MUST start their own server on a port derived from their own scratch directory, never the default port; and the suite MUST leave no files or processes behind (census tests).
- *Rationale:* a test that defaulted to a preserved evidence plane's port could write to it; tests leaked thousands of temporary directories.
- *Acceptance:* running the suite with a planted hostile environment (every Fae variable set to a wrong value) passes and leaves no residue.

**FAE-VER-007 (MUST)** Byte-identity properties MUST be asserted on bytes, not on structured equality that normalises them (for example, path equality that ignores trailing separators).
- *Acceptance:* tests of path storage assert on the byte form.

**FAE-VER-008 (MUST)** Tests that verify a remediation MUST verify behaviour, not the text of a comment at the defect site.
- *Rationale:* a remediation once rewrote a comment to describe a defect as intended, leaving the behaviour unchanged.
- *Acceptance:* each remediation for a blocking finding cites a behavioural test.

**FAE-VER-009 (SHOULD)** The transition-legality function, the kernel-edge graph, the gate evaluator's three-valued logic and the lease arithmetic SHOULD have exhaustive finite-domain tests, and MAY additionally be model-checked.
- *Acceptance:* exhaustive tests exist for each.

**FAE-VER-010 (MUST)** Fae MUST provide fault-injection seams for: plane serialization failure; Dolt child death; agent never ready; malformed handoff; missing handoff; provider throttle in the session log; stall; host pressure; pane loss; daemon SIGKILL at every stage boundary; shutdown mid-round; external tool refusal; pin mismatch with a reported id. Each seam MUST have a test that drives the daemon through it to the specified outcome.
- *Acceptance:* the fault-injection suite covers each seam.

**FAE-VER-011 (MUST)** A live dogfood acceptance MUST exist: a fresh instance with the multiplexer adapter, real agent sessions, at least two real code-bearing tasks with genuine REDs submitted through the daemon, all default stages enforced, the heavy-op lease in force, and a daemon restart mid-run, reaching Done with a complete artifact chain. Scripted runs are rehearsals and do not satisfy this.
- *Acceptance:* a recorded dogfood run with its artifact chain is attached to the release.

---

## 18. Phased delivery plan

Each phase ends with its acceptance tests green, including firing controls. Phases are ordered so that the safety foundations exist before the features that need them.

| Phase | Deliverable | Requirements closed | Exit criterion |
|---|---|---|---|
| 0. Foundations | Repository layout; pure domain and workflow packages; canonical JSON and hashing; gate expression language with three-valued evaluation; kernel state graph; validator skeleton; CI with nonzero-count assertions and the temporary-directory census | FAE-ARC-009, FAE-CP-015, FAE-WF-020, -021, -060 to -063, FAE-VER-005, -006 | Exhaustive kernel-edge tests and gate fuzzing pass. |
| 1. Control plane | Dolt supervision (child, parent-death signal, orphan reap, port derivation, ownership checks); schema and migrations; the commit seam with whole-body retry; scoped reads; audit writer; Dolt commit per batch; backups | FAE-CP-001 to -015 | Fault injection on the Dolt child and 1213 retry tests pass. |
| 2. Daemon and API | Instance layout, lock, workspace guard, Unix socket with credential, typed RPC schema, read pool with deadlines, caching and coalescing, event stream with resume, graceful shutdown | FAE-ARC-001 to -006, -020 to -022, FAE-ROB-010, -020, -021, FAE-API-001, -010 to -012, -020 to -022 | Status answers in under 1 s under read load; shutdown tests pass. |
| 3. Workflow engine on the scripted adapter | Workflow and molecule model; validator with kernel floors; primitives AgentTurn, Review, Gate, HostCommand, Freeze/Compare, Loop, Branch, Escalate; artifact and provenance store; brief composition; scripted adapter; dry-run simulator; diff | FAE-WF-001 to -094 (except external effects), FAE-ARC-007 | The default workflow runs end to end on the scripted adapter; the property suite and the mutation corpus pass. |
| 4. Scheduler and host guard | Ready set, priority, edges, role slots by scope, molecule limits, fairness, withheld reasons; heavy-op lease with weights, FIFO and classes; host guard with PSI, floors, per-pane reservation; shims; process inventory; leak detection | FAE-SCH-001 to -014, FAE-HOST-001 to -031 | Concurrency tests show unbounded tasks limited only by caps and host; shim matrix passes. |
| 5. Worktrees and integration | Worktree manager; merge-reconcile with own-hunk preservation; commit stage; containment; serialized integration; skip dispositions; stuck-stage detector | FAE-VCS-001 to -011 | The merge-reconcile and integration fault tests pass. |
| 6. Runtime adapters and health | Multiplexer and process adapters; session-log reader; outcome classification; provider health and failover; reaping; stall detection; loop schedules | FAE-RT-001 to -014 | Live adapter conformance; throttle failover test with real session logs. |
| 7. Robustness and resume | Stage-aware resume, re-validation, planned-roll budget rule, uncertain dispatch, dead-owner reclaim, sanitized engine runs, live configuration | FAE-ROB-001 to -042 | Property test: interruption at every point reaches the same outcome. |
| 8. Operator surface | Every CLI command and verb; operator messages; Prophet sessions (per-task and global); retire, reconcile-done, edges; status and history | FAE-OPS-*, FAE-CLI-* | CLI conformance; every verb has positive, refusal and audit tests. |
| 9. Controller operations | Roll (with overlap guard and post-start acceptance), maintenance, watchdog, land (all gates), salvage, batch, census and cleanup, gate self-test, gate run | FAE-CTL-* | A rolling restart under load charges no task budget; the gate self-test proves every gate can fail. |
| 10. Normal mode | Dispositions; tool-level grants; pinning; qualified-name validation; record-before-verdict; adoption; draft reviews; status-derived publish and merge gate; revision loop; ticket lifecycle from real schemas; workspace bindings; fleet-build baseline | FAE-MODE-*, FAE-EXT-* | A dry-run proof, then a live proof of one review created as a draft and kept as a draft under opt-out. |
| 11. UI enablement and observability | UI child supervision; metrics; alerts; timestamps; correlation references | FAE-API-030 to -033, FAE-OBS-* | Listener census and UI child supervision tests pass. |
| 12. Dogfood | The live dogfood acceptance | FAE-VER-011 | The recorded run is attached to the release. |

**FAE-PLAN-001 (SHOULD)** Phases 3 and 4 SHOULD be built against the scripted adapter only, so that the engine and scheduler are proven before any live agent runs.
- *Acceptance:* no live adapter code is merged before phase 3's exit criterion holds.

**FAE-PLAN-002 (SHOULD)** During the build of Fae itself, a lean process SHOULD be used (an isolated worktree per change, tests written first, one independent review), with proofs required only for the transition state machine, the no-bypass rule, role and lease capacity, worktree containment and deletion, single-writer control-plane authority, and crash recovery. Host safety and worktree isolation stay mandatory throughout.
- *Rationale:* operator direction: the full workflow is what the framework executes, not the process for building it.
- *Acceptance:* the development guide states this.

---

## 19. Open questions

1. **Heavy-op weights calibration.** The default weights (FAE-SCH-011) are estimates. They should be calibrated from measured peak memory and I/O of each op class on the reference host.
2. **Memory floors.** The soft floor of 6 GiB, the hard floors of 2 and 4 GiB and the landing floor of 12 GiB come from one 64 GiB host. Should defaults scale with total memory (for example, a percentage), with these values as the floor of the scale?
3. **Workflow-global cap across revisions.** FAE-SCH-006 picks the newest live revision's cap. An alternative is the minimum across live revisions. This needs an operator decision.
4. **Gate language surface.** Is a small typed expression language sufficient for every planned gate, or will some gates need a vetted, named built-in predicate (for example "own hunks preserved")? The proposal is to allow named built-ins implemented in the engine, each with its own firing control.
5. **Transition verifier role.** The prior system ran a verification agent turn on every transition. In Fae the kernel validator is authoritative. Should the default workflow keep an asynchronous verifier review at selected transitions only (for example before commit), and with what cap?
6. **Dry-run terminal state.** FAE-MODE-003 ends dry runs in Escalated with kind DryRunComplete. Should there instead be a distinct terminal state?
7. **Multi-workspace instances.** One instance binds one workspace today. Should an instance serve several target workspaces with per-target toolchains and network policies, so that a separate repository (for example the UI's) can run under Fae?
8. **Self-upgrade.** Should a roll be blue-green (start the new daemon, hand over at a round boundary, then stop the old one), rather than stop-then-start?
9. **Global Prophet authority.** May the global Prophet ever enact a batch verb (for example a census apply) under one directing message, or must every enactment be per task?
10. **Event retention.** Are 24 h or 100k events enough for UI reconnection and audit, given that the plane holds the full history?
11. **Review-tool schemas.** The status vocabulary and comment-severity encoding of the code-review read tool must be read from the real schema before phase 10. Which fields mark an automated-review comment as blocking?

---

## Appendix A: defaults

All values are defaults and configurable unless marked fixed.

| Area | Setting | Default |
|---|---|---|
| Instance | home | `~/.fae` |
| Instance | default instance name | `default` |
| Instance | drain run id | `backlog-drain` |
| Dolt | port | 3658 + hash(data directory) mod 4096 |
| Dolt | stop wait before forced kill | 30 s |
| Dolt | restart attempts on failure | 3 |
| Dolt | serialization retry attempts | 5, jittered backoff |
| Plane | maximum inline column size | 64 KiB |
| Plane | topology read budget | 200 ms |
| Workflow | task attempt limit | 20 |
| Workflow | spec-review loop | 3 iterations |
| Workflow | code-review remediation loop | 5 iterations |
| Workflow | scope-widen requests per task | 3 |
| Workflow | test runs per task | 8 |
| Workflow | default role caps | spec 2; spec reviewer 2; planner 2; coder 4; each reviewer dimension 4; oracle 1; QA 1 |
| Scheduler | tasks in flight | −1 (unbounded) |
| Scheduler | running molecules, instance and per workflow | −1 (unbounded) |
| Scheduler | pane limit | 60 |
| Scheduler | pane-starvation rounds before shedding | 3 |
| Scheduler | idle global session retirement | 30 min |
| Heavy op | capacity | 2 weight units |
| Heavy op | weights | targeted compile or test 1; whole suite 2; large-file formatter 2; package build 2 |
| Heavy op | waiting-line bound | 300 s |
| Heavy op | maximum wait before refusal | 1800 s |
| Heavy op | lease TTL | 6 h |
| Shims | build jobs | 2 |
| Shims | test threads (roles) | at most 2 |
| Shims | whole-suite test threads (engine) | 1 |
| Shims | formatter file limit | 1 MiB or 30k lines |
| Shims | refusal exit code for a temporary-directory build output | 77 (fixed) |
| Host | soft memory floor | 6 GiB |
| Host | hard memory floor | 2 GiB; 4 GiB without swap |
| Host | landing memory floor | 12 GiB, wait up to 30 min |
| Host | per-pane memory reservation | 0.7 GiB, held 180 s |
| Host | I/O pressure full avg10 (launch) | 10.0 |
| Host | memory pressure full avg10 (launch) | 5.0 |
| Host | I/O pressure full avg10 (heavy-command gate) | 5.0, two readings 30 s apart |
| Host | load per core | 4.0 |
| Host | soft-guard backoff | 10 checks at 3 s |
| Host | process-leak alert | 200 processes or 4 GiB per command name; 20 defunct |
| Runtime | agent start | 180 s per attempt, 3 attempts |
| Runtime | stall retries | 2 |
| Runtime | failover rotations per turn | 1 |
| Runtime | dispatch recoveries | 2 |
| Runtime | same-gate repeats | 3 |
| Runtime | coordinator re-queries | 2 |
| Health | initial score; floor; eligibility | 100; 0; at least 50 |
| Health | penalties | rate-limited 60; unavailable or auth 100; timeout, model-API error, malformed 40 |
| Health | decay half-life | 3600 s |
| Health | throttle hold | 3600 s |
| Shutdown | drain-down bound | 120 s after the last commit step |
| IPC | read deadline | 5 s |
| IPC | snapshot staleness | at most 10 s |
| Events | retention | 24 h or 100k events |
| Host metrics | ring buffer | 15 min at 5 s |
| Operator | authorizing message maximum age | 24 h |
| VCS | stuck commit-stage detector | 10 min |
| Roll | maximum wait for a clear window | 300 s |
| Roll | overlap hold per task | 2 h (indefinite for critical tasks) |
| Watchdog | idle alert | 45 min |
| Watchdog | re-arm budget | 3 per cause per hour |
| Watchdog | filesystem alert | 80% of bytes or inodes |
| UI child | restart backoff | 1 s doubling to 60 s; 10 per hour |
| Loops | auto-expiry | 7 days |
| Census | pacing between retirements | 2 s |
| Review | revision loop cap | 5 |

## Appendix B: typed escalation kinds

Every per-task escalation MUST carry exactly one kind. Parks are listed separately because they are not escalations.

| Kind | Raised when |
|---|---|
| AttemptCap | The task's attempt limit is exhausted. |
| LoopCap | A workflow Loop's maximum iterations is exhausted (names the loop). |
| SameGate | The same gate failed for the same reason more than the repeat bound. |
| TestRunCap | The per-task test-run cap is exhausted. |
| ScopeWidenRejected | A scope widen was refused, or the widen cap is exhausted. |
| NoRedViolation | A no-red task's diff contains code the classifier forbids. |
| CoordinatorVeto | A verifier rejected a transition past its re-query bound. |
| StaleBranchResidue | The worktree holds unexplained prior work. |
| DispatchRecoveryExhausted | Crash-recovery re-dispatches are exhausted. |
| StallExhausted | A role stalled past its retry bound. |
| ProviderUnavailable | No route could launch after all bindings were tried and parks exceeded the operator's route-exhaustion bound. |
| PaneCapStarvation | The pane limit is starved by the instance's own idle sessions, with nothing left to shed. |
| ResumeEvidence | Resume cannot re-validate the evidence for the durable stage. |
| MergeOwnHunkLoss | Merge-reconcile could not preserve the task's own files or hunks. |
| IntegrationConflict | Integration conflicted and the remediation route is exhausted. |
| ExternalEffectRefused | A pinned external call was refused (the reported id, if any, is recorded). |
| ReviewGateFailed | The code-review publish or merge gate failed past its cap, or saw an unknown status. |
| DryRunComplete | A Normal dry-run task finished recording its intended effects. |
| OperatorEscalate | The operator escalated the task. |
| HumanDecisionRequired | A workflow HumanDecision stage needs the operator. |

| Park reason | Raised when |
|---|---|
| HostPressure | The soft guard refused after backoff. |
| HostHardLimit | The hard breaker refused. |
| PaneLimit | The pane limit is reached. |
| MemoryReservation | The per-pane reservation would breach the floor. |
| RouteUnavailable | No eligible binding exists for the role. |
| RoleCapacity | The role's slots are all leased. |
| HeavyOpWait | The heavy-op lease is not yet granted. |
| PlaneTransient | A plane transaction exhausted its retry bound. |
| UncertainDispatch | It is unknown whether a turn ran; operator action needed. |
| Deferred | The operator deferred the item. |
| MissingInput | A no-bypass input is not yet recorded. |

> Proposed 2026-11-06 (mosaic code-to-model audit), amendment to Appendix B: adds the escalation kind FootprintExceedsHost (a slot holder's footprint exceeds the host's total memory less the soft floor, or the pane limit; FAE-SCH-021) and the park reason WorkflowSlot (no workflow slot is free; FAE-SCH-021).

## Appendix C: the default workflow, stated as content

This appendix describes, in words, the content of the default workflow's first revision. It is the reference for the parity corpus.

**Roles.** spec (workflow-global, cap 2); spec-reviewer (task-bound, cap 2); planner (workflow-global, cap 2); coder (task-bound, cap 4, phases test-first and implement); reviewer-correctness, reviewer-quality, reviewer-security, reviewer-architecture (task-bound, cap 4 each); oracle (workflow-global, cap 1); qa (workflow-global, cap 1). Every role has a deep ordered route of at least 4 bindings across at least 2 providers. Pre-code roles (spec, spec-reviewer, planner) have no build, test or formatter capability.

**Artifact types.** Spec (scope, acceptance criteria, contracts, risks, required tests as intent with name and filter, declared files); SpecReview (verdict, findings with severity); Plan (ordered steps, declared files, test placement, risks); RedReport (test id, test shape, declared files, expected failure fragment); EngineRun (FAE-WF-030 HostCommand output); FrozenIdentity; ImplementationReport (changed files, hunks, proof note, scope-widen or RED-rework requests); Review (dimension, findings with severity and target); OracleVerdict (one typed decision per finding: valid or invalid, blocking or non-blocking, target RedTest or Production, rationale); QaVerdict (checks, each citing an engine-run id); ReconcileReport; Commit; Integration.

**Stages and edges, in the normal case.**
1. Intake: binds the task's content, attachments and target configuration.
2. Spec (AgentTurn, spec role) produces Spec.
3. Spec review (Review: 2 spec reviewers, any-blocking-fails), inside a Loop back to Spec with a maximum of 3 iterations; exhaustion escalates with LoopCap.
4. Plan (AgentTurn, planner) produces Plan.
5. Test-first (AgentTurn, coder, test-first phase) produces RedReport and the test in the worktree. The phase-one change set may touch only declared test files.
6. Run RED (HostCommand, weight 1) runs the declared test shape.
7. RED gate (Gate): exit code nonzero and not "command not found"; exactly one matched test; the expected-failure fragment observed in the output; the test id present in the source. On failure, Loop back to step 5 (bounded by the test-run cap).
8. Freeze RED (FreezeArtifact over the declared test functions' normalised content).
9. Implement (AgentTurn, coder, implement phase) produces ImplementationReport.
10. Run tests (HostCommand, weight 1): the RED test and the declared required filter.
11. Green gate (Gate): exit code zero, executed count at least 1, the RED test reported passed; and CompareFrozen reports the RED unchanged. On failure, back to step 9.
12. Formatting debt (HostCommand plus Gate): no new formatting debt on changed items.
13. Reviews (Review: four dimensions, each isolated, any-blocking-fails).
14. Oracle (AgentTurn, oracle) produces OracleVerdict.
15. Branch on the verdict: any valid blocking finding with target RedTest goes to step 5 (a new RED identity, the old one kept in the trail); any valid blocking finding with target Production goes to step 9; an unknown target escalates; otherwise continue.
16. QA (HostCommand, weight 2: the required filter across all targets, the instance's always-required tests, a compile of all targets, and any declared checks) then QA gate (Gate over engine-run evidence with positive counts) and, if the workflow keeps it, a QA AgentTurn that interprets the runs. Failure goes to step 9 with the reasons.
17. Merge (reconcile onto the integration tip). Clean and patch-identical: refresh the reviewed snapshot and re-run step 16 only. Conflicted: back to step 9 with paths. Own-hunk loss: escalate with MergeOwnHunkLoss.
18. Commit (VcsCommit of the declared and reviewed files only).
19. Integrate onto the instance integration branch, with containment.
20. Normal mode only: the code-review sub-workflow: ExternalEffect ticket lifecycle steps; ExternalEffect review create (draft); push to the namespace; a status-poll Loop; remediation revisions on the same review; publish; merge; ticket closed.
21. Done.

**Kernel edges.** Every non-terminal stage has an edge to Escalated; Blocked re-enters at test-first or implement as routed; Escalated goes to Retired or to stage 2 (restart with information).

**Limits.** Task attempt limit 20; test runs 8; scope widens 3; spec-review loop 3; review revisions 5.

## Appendix D: lessons that produced requirements

| Observed failure in the prior system | Requirement |
|---|---|
| File-overlap edges starved the ready set (1 ready of 19; 1.9 tasks in flight of about 4 possible) | FAE-SCH-001 |
| Escalated prerequisites became dead edges, repaired only by direct database edits | FAE-SCH-002, FAE-OPS-003, FAE-OPS-014 |
| A synchronous verification turn per transition blocked the drive for hours | FAE-WF-032 |
| Each per-task escalation kind exited the drive until fixed individually | FAE-ROB-001 |
| A daemon kept driving for 33 minutes after SIGINT | FAE-ROB-010 |
| Rolling restarts charged in-flight tasks recovery budget | FAE-ROB-011 |
| Resume regressed a QA-passed task to Pending | FAE-ROB-012 |
| A no-red resume discarded valid RED evidence | FAE-ROB-013 |
| Leaked leases and lock files refused the next start | FAE-ROB-014 |
| A serialization failure from a concurrent writer killed the drive | FAE-CP-007 |
| Read RPCs pinned workers; status latency reached 32 s | FAE-ROB-020, FAE-CP-009 |
| Merge-reconcile dropped all of a task's production hunks | FAE-VCS-004 |
| A framework-performed rebase escalated finished work | FAE-VCS-005 |
| An equivalent upstream change was escalated as a conflict | FAE-VCS-007 |
| A skipped integration stranded a task in committing | FAE-VCS-009 |
| A splash-only collection was re-dispatched or exited the drive | FAE-ROB-016 |
| Finished worker sessions and zombies accumulated about 10 GiB | FAE-RT-011, FAE-HOST-023 |
| A daemon launched from an agent sandbox lost all system calls | FAE-HOST-030 |
| A daemon-leaked control-plane variable failed a green test | FAE-ROB-030 |
| Gates passed with zero tests executed | FAE-WF-062, FAE-ROB-033, FAE-VER-005 |
| Throttles left provider health "healthy" | FAE-RT-007, FAE-RT-009 |
| A missing handoff was reported as an id-less answer | FAE-RT-006 |
| An unpersisted disposition made a live task terminal | FAE-ROB-042 |
| A qualified tool name was refused and orphaned a live review | FAE-EXT-003, FAE-EXT-004 |
| A commit-sha adoption key would open a second review | FAE-EXT-005 |
| Hard-coded ticket arguments were rejected by the real schema | FAE-EXT-020 |
| A red package fleet build blocked every review | FAE-EXT-016 |
| A formatter on a huge file consumed 12 to 20 GiB | FAE-HOST-012 |
| A fan-out of sessions walked memory below the floor | FAE-HOST-006 |
| A heavy-op waiter starved for 58 minutes | FAE-SCH-011 |
| The pane ledger read zero while 43 panes existed | FAE-SCH-012 |
| The RAM-backed temporary directory filled, then ran out of inodes | FAE-HOST-020 |
| A recursive wrapper chain grew to 3000 processes | FAE-HOST-022 |
| A wrong containment sha on nine retirements | FAE-OPS-012 |
| Landed work kept an escalated backlog row | FAE-OPS-013 |
| Merged review dimensions downgraded blocking findings | FAE-WF-033 |
| A whole-file RED hash and a formatter tripped "modified RED" | FAE-WF-076 |
| A roll under an in-flight task on the same files lost its hunks | FAE-CTL-001 |
| An aborted roll left the instance silently down | FAE-CTL-001 |
| A watchdog counted retired rows as in flight | FAE-CTL-020 |
| A silent lock wait hid a 58-minute landing stall | FAE-CTL-051 |
