# Fae

A local Go implementation of [REQUIREMENTS.md](REQUIREMENTS.md), with a runnable
Dolt-backed daemon, durable scripted workflows, and opt-in direct-process or Herdr live execution. This is still a
partial implementation, not a production-ready implementation of all 284
requirements. A trusted agent bridge can execute real turns; brokered host commands and
reviewed Git effects support Local execution and explicitly authorized Normal effects through configurable adapters. Scripted evidence describes rehearsals, not actual
code changes, test executions, commits, or integrations.

## Run locally

Requires Go 1.27.2, Git, and Dolt. The daemon initializes and supervises its own
private Dolt database; clients never connect to an existing database directly.

```sh
mkdir -p .cache/go-build bin
export GOCACHE="$PWD/.cache/go-build"
go build -o bin/fae ./cmd/fae

# Terminal 1, from the canonical workspace root. Supply --dolt if it is not on PATH.
./bin/fae serve --workspace "$PWD" --adapter scripted --foreground

# Terminal 2; use the absolute path to bin/fae when outside this workspace.
./bin/fae message --id demo-authorization --text '{"text":"Run local scripted work and shut down this instance.","authorize":{"classes":["task","runtime"]}}' 
./bin/fae run --id demo --prompt 'Exercise the scripted default workflow' --authorizing-message demo-authorization
./bin/fae status --json
./bin/fae history --task demo --json
./bin/fae board --limit 100 --json
./bin/fae history --task demo --summary --limit 100 --json
./bin/fae events --after 0 --follow
./bin/fae shutdown --authorizing-message demo-authorization
```

The default home is `~/.fae/instances`, with `--home PATH` and `--instance NAME`
available on commands. The instance home must be outside repositories, and its
physical path must contain no symlinks. Instance directories and credentials are
owner-only. A start must use exactly the bound workspace and current directory;
reads and other RPC commands work from any directory. Only `serve` starts a daemon.

First start installs the default workflow as immutable revision 1. It includes
intake, spec/review, plan, RED author/run/gate/freeze, implementation/compare,
reviews, Oracle routing, QA, reconcile, protected commit, integration and Done.
Failures are routed through gates and bounded remediation. With the scripted adapter, HostCommand and VCS
outputs are fixtures; the source identity freeze/compare uses Go syntax trees.

```sh
./bin/fae workflow default --json
./bin/fae workflow scenario --json
./bin/fae workflow validate examples/workflow.json
./bin/fae workflow simulate examples/workflow.json examples/script.json demo
```

`serve --scenario FILE` uses custom stage fixtures; otherwise it uses the built-in
happy-path rehearsal. `template workflow-put --content FILE --authorizing-message
ID` installs validated new revisions through the daemon. In-flight tasks keep
their original revision. Admission can use `--idempotence-key KEY`; retries return
the original admission response without changing the current checkpoint.

The CLI exits 0 on success, 1 on refusal, 2 on operational-command usage errors,
and 3 when a required daemon is unreachable. Recorded timestamps come from the
daemon clock. The offline legacy workflow commands retain their validation-error
behavior. No Normal-mode network external effects are supported yet. See [LIVE.md](LIVE.md)
for live adapter configuration, the Codex bridge, and their trust boundaries.

## Architecture and durability

`domain`, `kernel` and `workflow` hold value types, safety predicates, graph rules,
three-valued gates, schema checks and normalized test identities. `defaults` holds
the shipped graph as data. `engine` prepares dispatches and validates completion
against the pinned workflow, task, tree, token, sessions and artifact schemas.

`daemon` is the singleton writer: it checkpoints dispatch intent before launching
asynchronous execution, then atomically saves completed evidence, transitions,
audit and ordered events in `controlplane`. The control plane uses versioned Dolt
commits and chunked content-addressed payloads. Completed stages are not replayed.
Graceful scripted restart retains dispatch identity without charging retry budgets;
uncertain non-replayable executions park. A bounded Dolt-child restart revalidates
the schema and resumes from durable state.

`api` serves authenticated unary RPC and NDJSON events on the Unix socket.
Status uses an immutable in-memory projection. Event clients resume by sequence;
slow streams do not hold the drive lock. `scheduler`, `hostguard`, `runtime`,
`workspace` and `worktree` provide the scheduling, admission, runtime and isolation
foundations. Live launches use durable role slots and a host-shared weighted
ledger. The Herdr adapter creates private owned servers and gated panes; exact
process identity records and parent-death watchdogs support conservative orphan
recovery before new work starts. Unproven identities do not authorize cleanup or
lease reclamation. Provider failover and complete host process inventory remain open.

Changed integration bases use immutable three-way merge plans with own-hunk
preservation checks. The merged tree repeats tests, frozen-test comparison,
reviews, Oracle and QA before commit. Supported text conflicts return to bounded
implementation with named paths and an immutable original-change contract. Lost
own hunks, frozen-test conflicts and unsupported classes refuse. A second target
advance during an active conflict contract requires operator review. Integration also recognizes ancestry,
patch-equivalent and touched-blob containment without moving the containing tip.

## Tests and formal verification

```sh
./scripts/test.sh
FAE_TEST_DOLT_BINARY=/absolute/path/to/dolt ./scripts/test.sh
FAE_TEST_DOLT_BINARY=/absolute/path/to/dolt python3 scripts/smoke-cli.py
./scripts/install-verifiers.sh
./scripts/verify.sh
```

The full suite uses isolated private Dolt instances and tests real IPC, concurrent
tasks, authority refusal, idempotence, schema rejection, event replay, graceful
restart, malformed sibling isolation and child-process recovery. It asserts
nonzero executed tests and checks test scratch cleanup. Never supply a developer
control-plane DSN; the integration harness accepts only a Dolt executable.

Gobra verifies 33 production safety predicates and rejects 33 weakened mutations.
Gomela/SPIN checks 34 bounded lifecycle models with 32 firing negative controls.
These are scoped proofs/models, not whole-daemon verification.
See [TESTING.md](TESTING.md), [formal results](verification/README.md), and
[implementation status](IMPLEMENTATION.md).

Two real Codex `gpt-6-astra` tasks completed through **Herdr 0.9.5**, private
Dolt, genuine RED/GREEN tests and a drained mid-run daemon restart. Their
checkpoint artifacts survived unchanged. One task exercised the integration
race retry and refreshed all approvals: 35 artifacts at version 83; the other
completed with 22 artifacts at version 52. The capacity-2 host ledger ended
with 58 grants, 58 releases and no holders. See [LIVE.md](LIVE.md) and
[recorded acceptance evidence](verification/results/herdr-dogfood/summary.json).

Only trusted single-operator workspaces are supported. No hostile filesystem or
escaped-process sandbox is claimed. Complete process inventory and lease
reconciliation, interactive brokered agent tools, unsupported conflict classes,
and remaining operator/controller and Normal-mode phases remain open.

The runtime-recovery increment adds audited dynamic pane limits, trusted-adapter
idle-session shedding and bounded safe stall retry. Its final suite passed 494
cases with race detection and vet; authenticated two-task dogfood also passed,
including changing the cap during active work and preserving it across restart.
See the [current verification record](verification/results/runtime-recovery.json)
and [fresh provider result](verification/results/runtime-recovery-dogfood-20261017/result.json).

The latest increment adds optional persistent provider context, dynamic heavy-op,
host-pressure and task limits, and bounded startup recovery. **530 tests passed**
with race detection and vet (77.8% coverage). Fresh two-task dogfood passed with
29 real turns, eight context resumptions, preserved host configuration across
restart and all 58 leases released. Task-bound contexts stay within their task;
reviews stay fresh. See [latest verification](verification/results/persistent-host-startup.json)
and [provider continuation evidence](verification/results/persistent-host-startup-dogfood-20261017/result.json).

The operator/read increment adds server-bound delegated principals, transactional
message-class grants, ten durable task controls, coalesced bounded reads, and
explicit event-retention/overflow gaps. See [authority](daemon/AUTHORITY.md),
[task controls](daemon/TASK_CONTROLS.md) and [read contracts](daemon/READS.md).

The authenticated operator/read dogfood also passed: both tasks reached Done,
with 34 completed provider turns, five loop iterations, seven reused resident
Herdr panes, mid-turn SIGKILL recovery, independent drive restart and guarded
daemon roll. Host grants ended with 62 releases and one recovery out of 63,
zero holders and zero pane reservations. Source, binary and formal-input hashes
match the archived evidence. See the
[complete phase record](verification/results/operator-controls-reads.json).

The next increment implements [Prophet sessions](daemon/PROPHET.md),
[backlog dependencies](daemon/BACKLOG.md) and
[safe post-commit restart](daemon/POSTCOMMIT.md). Verification passed 1,062
race-enabled cases across 26 packages (74.6% coverage), vet, builds, 36 Gobra
guards/36 rejected mutations and 37 positive/35 negative Gomela models.
Authenticated dogfood passed both tasks, 36 provider turns and task/global
Prophet replies, with no final lease holders or pane reservations. See the
[current verification record](verification/results/prophet-backlog-restart.json).


## Retirement, target policies and Prophet routes

This increment adds audited task retirement with atomic dependency transfer,
contained-release and obsolete-hold behavior, plus retired read filtering. It also
adds immutable Local target policies pinned across run/backlog/cron admission and
restart, and Prophet routes using durable provider health with cleanup-gated,
bounded failover. See [retirement](daemon/RETIREMENT.md),
[target policies](daemon/TARGETS.md) and [Prophet routing](daemon/PROPHET.md).

The final full suite passed **1,116 race-enabled cases across 26 packages**,
with **74.1% statement coverage**, vet, both builds and strict scratch cleanup.
Gobra verified **39 production guards** and rejected **39 mutations**;
Gomela/SPIN passed **40 positive / 38 firing negative models**. All 115 formal
input hashes match. Independent review found and corrected a supersession-chain
edge case before this final full rerun.

Expanded authenticated dogfood passed both tasks with **36 completed provider
turns**, five provider-loop iterations and seven reused resident Herdr panes.
Both tasks retained target revision 1 despite a restrictive revision 2 and
mid-turn crash/restart. Both Prophet scopes answered and recorded provider health.
Retirement transferred a dependency, then kept it held after obsolete retirement;
retired rows remained hidden by default. All 69 host grants were released (68)
or recovered (1), with zero final holders and zero pane reservations.

See the [verification record](verification/results/retirement-target-routes.json)
for final logs, source/evidence hashes, the earlier full pass, review findings and
limits. Explicit targets currently support Local mode only; unsupported target
policies fail closed. Containment without retained task evidence cannot yet be
recovered from integration commit subjects alone.


## Normal targets, census and retired cleanup

Target schema v2 binds Normal-mode destination, push namespace, installed tool
definitions, review baseline/workspace policy and ticket orchestration. Tasks keep
immutable revisions; adapter drift refuses admission or emission. Target v3 selects immutable
adapter bundles installed through operator commands; review workspaces are prepared
explicitly before admission.
See [target policies](daemon/TARGETS.md).

`fae census` emits a reviewable snapshot; `fae census apply` retires unchanged
rows one at a time through the audited control, stopping with partial progress
on refusal. `fae backup plane` verifies a Dolt restore before producing backup
evidence. `fae cleanup retired` previews by default; apply verifies backup coverage
and archives all candidate tips before removing owned clean worktrees and branches.
See [census](daemon/CENSUS.md) and [cleanup](daemon/CLEANUP.md). Marked scratch instances can be removed through the
[scratch cleanup controller](daemon/SCRATCH.md), which retains verified backups
outside the instance root. Automatic backup pruning remains unimplemented.

Verification: **1,165 race-enabled cases**, Gobra **42 guards / 42 rejected
mutations**, Gomela **43 positive / 41 negative controls**, and a passing
**36-turn** real-provider run with crash/restart, mixed-mode isolation and
verified retired cleanup. See the [verification record](verification/results/normal-target-census-cleanup.json).


Scratch cleanup and dynamic adapter/workspace provisioning are implemented and
verified. The latest increment passed 1,242 race-enabled cases,
46 Gobra guards and 47 positive/45 negative Gomela models, followed by a
36-turn real-provider run with restart and verified scratch deletion.
See the [verification record](verification/results/scratch-provisioning.json)
and [implementation limits](IMPLEMENTATION.md).


Adapter retirement and configurable fleet provisioning are now implemented.
Explicit preparation recovers uncertain provisioning through receipt lookup;
retirement blocks new bindings while preserving existing tasks. Verification
passed 1,264 race-enabled cases, 48 Gobra guards, 49 positive/47 negative
Gomela controls and a 36-turn real-provider run. See the
[verification record](verification/results/adapter-fleet.json).

## Backup retention and replication

Operator backup lifecycle commands now support reviewed retention and configurable remote replication. Retention preserves historical job coverage and replication sources; uncertain uploads query durable remote proof and verify the downloaded archive before confirmation. See [backup commands and protocol](daemon/BACKUPS.md).

1,293 race-enabled test cases across 26 packages passed, with 73.5% statement coverage, vet and scratch census. Gobra verified 50 production predicates and rejected 50 mutations; Gomela/SPIN passed 51 positive models and observed 49 firing negative controls. The two-task real-provider run completed 36 turns, recovered one lost-acknowledgement upload after restart without a duplicate, restored the downloaded Dolt backup and passed fsck, then pruned a redundant local backup and cleaned the scratch instance. [Verification record](verification/results/backup-lifecycle.json).

## Automatic retention and redundant scratch archives

`backup retention-schedule configure|pause|resume|status` adds opt-in durable retention of existing plane backups. Each run rechecks explicit authorization and preserves historical coverage and replication pins. `cleanup scratch-archive` removes only a reviewed exact duplicate while retaining the canonical recovery archive and Git refs. See [retention policy](daemon/BACKUPS.md) and [archive cleanup](daemon/SCRATCH.md).

1,319 race-enabled test cases across 26 packages passed, with 73.6% statement coverage, vet and scratch census. Gobra verified 52 production predicates and rejected 52 mutations; Gomela/SPIN passed 53 positive models and observed 51 firing negative controls. Real-provider acceptance completed 36 turns and verified automatic pruning after pause/restart/resume, canonical archive survival, duplicate removal and retry. [Verification record](verification/results/retention-automation.json).

## Automatic backup creation and replication

`backup creation-schedule configure|pause|resume|status` creates verified local backups on an opt-in interval. `backup replication-schedule` independently sends the oldest unreplicated verified source, retaining its exact claim across uncertain results and restarts. Both default to a one-hour interval, revalidate explicit authority and honor host limits. See [commands and recovery limits](daemon/BACKUPS.md).

1,337 race-enabled test cases across 26 packages passed, with 73.7% statement coverage, vet and scratch census. Gobra verified 54 production predicates and rejected 54 mutations; Gomela/SPIN passed 55 positive models and observed 53 firing negative controls. Real-provider acceptance completed 36 turns, automatically created and replicated a verified backup, preserved pending replication across restart, refused destination changes while pending and observed no duplicate uploads. [Verification record](verification/results/backup-schedules.json).


## Replication unpinning and backup quotas

`backup unpin --backup PATH` previews a digest-bound release after verifying all remote replicas; `--apply --plan-digest DIGEST` applies it. Pending claims refuse release. `backup quota configure --max-bytes N --max-backups N` persists limits; `backup quota status` reports usage, including partial backups and replication spools. Mutations require operator authority. Dolt export limits use admission/checkpoints and preserve overruns; no quota implicitly deletes data. See [backup policy](daemon/BACKUPS.md).

1,352 race-enabled test cases across 26 packages passed, with 73.6% statement coverage, vet and scratch census. Gobra verified 56 production predicates and rejected 56 mutations; Gomela/SPIN passed 57 positive models and observed 55 firing negative controls. [Evidence](verification/results/backup-storage.json).
