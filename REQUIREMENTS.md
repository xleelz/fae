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

### 4.2 Task state model

**FAE-WF-020 (MUST)** The engine MUST distinguish **kernel states**, which exist in every workflow, from **stage states**, which are the workflow's own stages. The kernel states are: Pending (admitted, not released); Blocked (awaiting remediation); Parked (a transient per-round deferral, a derived view, never a stored terminal); Escalated (needs an operator decision); Done (terminal success); Retired (terminal, only from Escalated); and Cancelled (terminal by operator verb, from Pending or Blocked only).
- *Rationale:* lifecycle controls must work identically across workflows.
- *Acceptance:* a property test over generated workflows shows that these states and their legal kernel edges exist in every validated molecule.

**FAE-WF-021 (MUST)** Kernel edges MUST be exactly the following, plus the workflow's own stage edges. Every non-terminal state has an edge to Escalated. Escalated has exactly two out-edges: to Retired (operator verb with reason) and to the workflow's first stage (operator restart with new information, Section 10). Done, Retired and Cancelled have no out-edges.
- *Rationale:* the prior system had no exit from Escalated and accumulated 50 unresolvable escalated rows.
- *Acceptance:* an exhaustive test over the kernel graph checks every pair; a property test checks that terminal states have no out-edges in every generated molecule.

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

**FAE-WF-035 (MUST)** HostCommand templates MUST be argument vectors with typed placeholders bound from artifacts, never shell strings. The engine MUST validate the bound vector against an allowlist of command shapes declared by the target configuration.
- *Rationale:* prevents injection from task text, and lets the gate know what the command means.
- *Acceptance:* a task whose acceptance text contains shell metacharacters produces a vector with those characters as literal arguments; a vector outside the allowlist is refused.

**FAE-WF-036 (MUST)** Test-shape declarations MUST support at least these forms: the unit-test form (one named test, exact match); the integration-target form (a named test target plus one named test, exact match); a dedicated test script inside the worktree; and, for non-Rust or non-Go stacks, a declared runner plus a test identifier. The expected-failure check for a RED MUST tolerate formatting differences between source and output (for example interpolated messages and line continuations) by matching a declared stable fragment of the failure message against the output only, plus the test identifier against the source.
- *Rationale:* rigid shape checks rejected valid integration-test REDs and non-Rust stacks, burning attempts.
- *Acceptance:* a table test accepts each form and rejects a form with more than one matched test or zero matched tests.

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

### 4.6 The gate expression language

**FAE-WF-060 (MUST)** Gates MUST be written in a small, total, side-effect-free expression language over evidence. It MUST support: evidence lookup by artifact type, by producing stage, and by most recent or all; field access by path; comparisons (equal, not equal, less, greater and their inclusive forms); boolean and, or, not; counts over collections with a filter predicate; existence tests; string equality and prefix tests; and membership in a literal set. It MUST NOT support loops, recursion, user-defined functions, I/O, time or randomness.
- *Rationale:* totality and determinism make gates provable and replayable.
- *Acceptance:* a grammar specification exists; a fuzz test over random expressions shows evaluation always terminates within a bounded step count.

**FAE-WF-061 (MUST)** Evaluation MUST be three-valued: true, false or **unknown**. A missing field, a missing artifact, an unrecognised status value, or a type mismatch MUST yield unknown, and a gate whose result is unknown MUST fail closed.
- *Rationale:* an absent status must never read as success.
- *Acceptance:* tests for each unknown cause; the gate result names the unknown leaf.

**FAE-WF-062 (MUST)** Every gate MUST positively assert that it examined something. The validator MUST reject a gate expression that could pass when the evidence collections it reads are empty, unless the expression explicitly states that an empty collection is acceptable and the workflow marks that gate as an emptiness-permitted gate with a reason. For HostCommand evidence, every test-run gate MUST include a lower bound of at least one on the executed count.
- *Rationale:* gates that reported success while checking nothing were found three times in one day: a truncated list, a misclassified body, and a test filter that matched zero tests and printed "ok, 0 passed".
- *Acceptance:* the validator rejects a "no failures" gate that lacks a positive count; a run that reports zero passed and N ignored fails every test-run gate.

**FAE-WF-063 (MUST)** A gate result MUST record the ids of every evidence item it read and the value of every leaf, so that a reviewer can replay it.
- *Rationale:* auditability.
- *Acceptance:* replaying a recorded gate over its recorded evidence yields the same result.

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

**FAE-WF-092 (MUST)** Fae MUST provide a structural diff between two revisions: stages, edges, gates, roles, caps, routes and limits added, removed or changed, and the kernel-floor proof status of each side.
- *Rationale:* reviewing a new revision needs a semantic diff.
- *Acceptance:* diffing revision N and N+1 that changed one cap and added one stage reports exactly those two changes.

**FAE-WF-093 (MUST)** Migration: in-flight tasks finish on their pinned revision. Moving a task to a new revision MUST be an explicit operator verb that validates the task's current evidence against the new revision's stage at the mapped position and refuses when a required input is missing. Every revision ever written MUST remain loadable by later binaries, or the binary MUST refuse to start with a message naming the unloadable revision.
- *Rationale:* reproducibility and safe evolution.
- *Acceptance:* tests: a migration verb on a task whose evidence fits succeeds with an audit row; one whose evidence does not fit is refused.

**FAE-WF-094 (MUST)** The engine MUST be proved against the default workflow by a **parity corpus**: recorded runs (role outputs, host-command results, timings abstracted away) whose transition sequences and gate results are the expected outputs. Any change to the engine MUST keep the corpus green.
- *Rationale:* the engine replaces per-role code paths; parity proves nothing was lost.
- *Acceptance:* the corpus has at least 20 recorded runs covering every Branch arm and every escalation kind of the default workflow, and runs in CI.

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

**FAE-SCH-006 (MUST)** When two revisions of the same workflow resolve different caps for the same workflow-global role, the shared pool MUST use the most recent revision's cap that has a non-terminal task, and the choice MUST be reported in status.
- *Rationale:* the prior design left this unspecified.
- *Acceptance:* a test with revisions declaring caps 1 and 2 uses 2 while the newer revision has live tasks.

**FAE-SCH-007 (MUST)** Molecule concurrency MUST be bounded by two operator-configurable numbers: an instance-wide maximum of running molecules (default −1, unbounded) and a per-workflow maximum (default −1). Because one molecule holds every task of a revision, these limits never cap tasks within a molecule.
- *Rationale:* a per-task molecule identity combined with a default of 1 would serialise everything.
- *Acceptance:* a test with two revisions running concurrently under the defaults runs both.

**FAE-SCH-008 (MUST)** The scheduler MUST be fair: a task that is repeatedly blocked or parked MUST NOT starve unrelated ready tasks that have capacity, and within a role's queue the longest-waiting eligible stage turn MUST be granted next, ties broken by task priority.
- *Rationale:* starvation is invisible until it is long.
- *Acceptance:* a test with a perpetually parking task and five ready tasks shows the five progress every round.

**FAE-SCH-009 (MUST)** Each drive round MUST collect every completed turn and host command, across all tasks, before dispatching, and MUST dispatch all eligible stages within capacity in that round. Collection and dispatch of different tasks MUST NOT be serialised behind one another's turns.
- *Rationale:* a finished handoff once waited behind a long sibling turn.
- *Acceptance:* a test with 3 tasks finishing turns in the same round shows all 3 collected and advanced in that round.

**FAE-SCH-010 (MUST)** Every drive round MUST record why each ready-but-not-dispatched task was withheld (cap, molecule limit, host guard, pane limit, edge, deferral, route unhealthy) as a counter and a trace entry.
- *Rationale:* starvation was invisible because withholding was never logged.
- *Acceptance:* the status and metrics show withheld counts by reason.

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

### 13.2 Write API

**FAE-API-010 (MUST)** Every mutating CLI verb of Section 11 and every controller operation of Section 12 MUST have a corresponding RPC, with the same validation and audit. The CLI MUST be a thin client of these RPCs.
- *Rationale:* one enforcement point.
- *Acceptance:* a coverage test maps every CLI verb to one RPC.

**FAE-API-011 (MUST)** Mutating RPCs MUST be refused with a typed shutting-down error once shutdown begins (FAE-ROB-010), and with a typed maintenance error while a maintenance marker is present (except the maintenance start verb).
- *Acceptance:* tests for both.

**FAE-API-012 (MUST)** Mutating RPCs MUST be idempotent when called with an idempotence key: a repeated call with the same key returns the original result and writes nothing.
- *Rationale:* clients retry.
- *Acceptance:* a duplicate admit with the same key changes nothing.

### 13.3 Event stream

**FAE-API-020 (MUST)** The daemon MUST expose a server-streaming event RPC that delivers, in commit order, typed events for: task admitted, released, stage entered, stage left, transition (with from, to, owner, cause), parked (with reason), escalated (with kind), retired, cancelled, deferred; turn dispatched and collected (with binding); host command started and finished; gate result; external effect (intent, performed, refused, adopted); lease granted, waiting line, released; provider health change; session launched and reaped; alert raised; daemon lifecycle (starting, ready, shutting down, stopped); child lifecycle (started, crashed, restarted). Each event MUST carry a monotonic sequence number and a timestamp.
- *Rationale:* any UI animates from events; operators audit from them.
- *Acceptance:* a scripted run's event sequence is a deterministic function of the run.

**FAE-API-021 (MUST)** A subscriber MUST be able to resume from a sequence number; the daemon MUST retain at least a configurable window of events (default 24 h or 100k events) and MUST signal a gap when the requested sequence is older than the window.
- *Rationale:* reconnecting clients must not silently miss events.
- *Acceptance:* a reconnect from a retained sequence delivers every later event once; from an expired one, it receives a gap event first.

**FAE-API-022 (MUST)** A slow subscriber MUST NOT slow the drive or other subscribers. Per-subscriber buffers MUST be bounded; on overflow the subscriber MUST receive a gap event and resume from the plane.
- *Acceptance:* a stalled subscriber does not change drive round times.

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
