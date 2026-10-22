# Executed verification and its limits

`./scripts/test.sh` runs vet, race-enabled unit tests, a positive executed-test count and a pure-package import check. `./scripts/install-verifiers.sh` bootstraps pinned verifier sources after native prerequisites are installed. `./scripts/verify.sh` executes both verifiers and requires their negative controls to fire. CI includes the same commands; the remote CI job has not been executed in this local session.

## Gobra: actual production capacity predicate

Gobra verified `scheduler/capacity.go` directly, including its annotations, with **zero errors**. Under `0 <= used <= capacity`, successful acquisition implies positive weight and `used + weight <= capacity`. The implementation uses subtraction to avoid Go integer overflow. The predicate is called by the production heavy-op pool. This is a proof of this arithmetic decision, not the complete pool, fairness, persistence, or cross-process ownership.

The firing control is generated from that same source by removing the capacity check. Gobra exits nonzero and reports that the capacity postcondition might not hold. Logs: `logs/gobra-capacity.log`, `logs/gobra-negative.log`.

## Gobra: production completion admission guard

Gobra also verifies `kernel/completion.go`, called by `engine.Complete` with the
durable task state, in-flight presence, exact dispatch-token match, and computed
result-envelope validity. Acceptance implies a nonterminal task, pending dispatch,
matching token and valid envelope flag. A generated mutation removing token
matching fails its postcondition. Logs are `logs/gobra-completion.log` and
`logs/gobra-completion-negative.log`. This is a proof of the boolean admission
predicate; deriving those inputs, JSON Schema validation, database provenance and
all of engine.Complete are tested but are not covered by that predicate proof.

## Gobra: production lease-release identity guard

Gobra verifies `scheduler/release.go` directly. A successful release requires a
positive PID and start timestamp and equality with both recorded owner fields.
The generated negative removes timestamp equality and fails the postcondition.
This is the production `Pool.Release` predicate, not a proof of process identity
collection, SQL/OS locking, or child termination. Logs are `gobra-release.log`
and `gobra-release-negative.log`.

## Gomela + SPIN: bounded completion and daemon protocol models

`gomela/completion.go` models the buffered outcome protocol in `runtime.Scripted.Dispatch`. It contains one producer, one one-element result channel, and (in Completion) one-element cancellation and timer channels. Both signals are available, allowing either select branch. Payload parsing and real context/timer implementations are abstracted; named worker functions replace the production closure for the model. This is **not direct verification of the runtime implementation**, arbitrary goroutine counts, or the whole daemon.

Executed results:

| Model | Explored states | Result |
|---|---:|---|
| Completion | 247 | No reported channel, counter, unlock, panic, or model-deadlock error |
| AbandonedCollection | 68 | Same; producer can finish without a collector |
| PersistBeforeDispatch | 523 | No reported communication or model-deadlock error |
| ShutdownWithTwoWorkers | 810 | Two abandoned completions fit the two-slot buffer |
| LeaseRelease | 26893 | Two competing workers release a capacity token after process-exit acknowledgment |
| ParentDeathRetirement | 984 | Group-exit acknowledgment precedes capacity reuse |
| OwnedPaneLifecycle | 1172 | Registered pane completes and retires before private server shutdown |
| ConflictRemediation | 1939 | Original evidence and own-hunk check precede fresh approval and landing |
| DrainBeforeRestart | 7086 | Two completed turns receive durable acknowledgment before restart |
| DurableLeaseRecovery | 984 | Complete retirement acknowledgment precedes capacity reuse |
| Negative: ConflictMissingFreshApproval | 55 | Missing receipt leaves the mandatory approval producer blocked |
| Negative: DrainMissingAcknowledgment | 65 | Missing durable acknowledgment blocks restart |
| Negative: MissingLeaseRecoveryProof | 17 | Missing retirement proof withholds capacity |
| Negative: ParentDeathMissingAcknowledgement | 36 | Missing group-exit acknowledgment blocks reuse |
| Negative: LostPaneCompletion | 48 | Missing completion blocks orderly retirement |
| Negative: AbandonedWithoutBuffer | 11 | Blocked producer send detected; upstream reports `Model deadlock: true` |
| Negative: ShutdownWithInsufficientBuffer | 53 | One-slot buffer leaves a producer blocked |
| Negative: LeaseReleaseMissing | 39 | Missing release leaves the second worker blocked |

The negative fixtures remove the result buffer or reduce a two-worker completion buffer to one slot. The reported property is a goroutine blocked on an abandoned send, not proof of an application-wide deadlock. In this pinned version's CSV, column 9 (zero-based index 8) is `Global_deadlock` in the implementation, printed as `Model deadlock`; column 10 is panic. The script checks the actual schema, positive state counts, exactly nineteen positive and seventeen negative results, no tool errors, and the negative result. Logs and machine-readable CSVs are in `logs/` and `results/`; generated Promela models and input SHA-256 hashes are retained.

`gomela/daemon.go` models a persistence-acknowledgment-before-dispatch protocol
and a bounded two-worker shutdown. The SQL transaction is abstracted as a channel
acknowledgment; SPIN does not verify Dolt or prove that all production call sites
use this order. Daemon integration tests separately exercise persisted dispatch,
collection, restart and malformed completion paths. The positive models have
fixed worker counts and do not establish unbounded liveness or crash recovery.

## Toolchain and compatibility

- Go: 1.27.2, Darwin arm64.
- Gobra: `d2c5af5450540c502855e53f4a5af1ba7630c41f`, built with SBT 1.13.0, Java 17.0.20.1; Silicon backend with Z3 5.3.1.
- Gomela: `b115cd2fd9556566817f092635edaa667a580898` (before the incomplete no-close optimization), with the checked-in `patches/gomela-go127.patch`.
- SPIN: `090f74209025a53297990ec17deca1bd51cb92a5`, built from source with Bison 3.8.2 and the local C compiler.

The Gomela patch upgrades its old `golang.org/x/tools` parser to v0.52.0 for Go 1.27 and propagates the child's `ContainsReceiver` flag into the parent model so generated helper processes are declared. It does not weaken safety assertions. Upstream master first failed with an old Go parser, then emitted inconsistent channel representations after updating that dependency. The pinned earlier revision plus this small propagation repair produces executable models; positive and negative controls were both rerun after the repair. Failed exploratory output was removed to prevent stale models being mistaken for successful verification. The final script always generates fresh models.

Upstream instructions: [Gobra](https://github.com/viperproject/gobra), [Gomela](https://github.com/nicolasdilley/Gomela), [SPIN](https://github.com/nimble-code/Spin).

## Remaining verification boundary

No proof currently establishes the full transition machine, no-bypass graph property, filesystem containment/deletion, SQL single-writer authority, crash recovery, durable host ledger, or complete scheduler fairness. Unit/race/integration tests cover portions of those implementations; they must not be described as formal proofs. The host-scoped ledger now has durability, cross-process exclusion and corruption-refusal tests. Its filesystem protocol and the SQL resource-pool serialization remain outside formal proof. The bounded lease model abstracts persistence and real child execution; it does not prove arbitrary worker counts or weighted FIFO fairness.

## Orphan recovery and changed-base reconciliation increment

Gobra verifies the actual `processowner.CanTerminate` conjunction of owner, exact
birth identity, group identity, and private record validation, and the actual
`kernel.CanApplyReconcile` conjunction of current tip, known head, known tree,
and valid evidence. Derived negative controls remove identity and known-tree
checks respectively; both fail their postconditions. This proves the Boolean
decisions, not correctness of OS observations or Git transactions.

The bounded `ParentDeathRetirement` Gomela model explores 984 states without
reported errors. It orders watchdog death notification, group-exit acknowledgment,
and capacity reuse. Removing the exit acknowledgment yields a detected blocked
model (36 states). Process groups, filesystem persistence, watchdog scheduling,
and arbitrary process trees remain outside this model. All sixteen positive and fourteen
negative models were regenerated and checked in this run.

The bounded `OwnedPaneLifecycle` multiplexer model also orders persisted
registration before permit, completion before pane retirement, and pane retirement
before private server shutdown. Its lost-completion negative control fires. This
is a channel-protocol model, not a verification of Herdr or its OS process groups.

## Conflict remediation, safe lease recovery and planned restart

The conflict/recovery increment checked nine production Gobra predicates.
The current run checks **eighteen production Gobra predicates**, each with a derived
negative mutation, and **nineteen positive / seventeen negative Gomela models**. The
additional production guards are:

- `kernel.CanKeepOwnHunk`: additions and deletion effects must both be retained.
  Removing the additions check fails its postcondition. Git diff parsing and
  deciding whether these conditions hold are integration-tested, not proven.
- `scheduler.CanRecoverLease`: exact owner death and linkage are required, with
  proof either that launch was never permitted or that retirement completed.
  Removing owner death fails. Journal persistence, OS identity observation and
  complete process linkage remain outside this Boolean proof.
- `kernel.CanFinishDrain`: draining, zero active workers and zero durable pending
  dispatches are required. Removing the pending-dispatch check fails.

The new channel models abstract immutable conflict evidence and renewed approval,
two-turn durable drain acknowledgments, and lease retirement acknowledgment.
Their negative controls demonstrate blocked protocols when a required receipt is
missing; they do not prove that every production call site respects those protocols.
Recorded CSVs, generated models, logs and source hashes identify the executed inputs.

The integration-race increment also verifies `kernel.CanRetryIntegration`:
retry requires a known no-write refusal, an advanced target, a known own commit,
and a bounded retry route. Removing the no-write condition fails verification.
The broker distinguishes a pre-write mismatch or explicit Git CAS rejection from
an unknown effect outcome. Interpreting Git outcomes and preserving committed
changes through re-reconciliation are integration-tested, not proven by Gobra.

## Provider, shim and pane admission increment

The current run directly verifies three more production predicates:

- `providerhealth.Eligible`: integer milli-score must be between 50,000 and
  100,000. Removing the lower threshold fails its postcondition. Floating-point
  decay, timestamp handling, SQL transactions and log classification are tested,
  not formally proved.
- `commandshim.CanExecute`: role permission, scope, output safety, lease admission
  and absence of nesting are all necessary. Removing the lease condition fails.
  The proof assumes the supplied booleans; it does not prove OS containment or
  that an arbitrary child cannot bypass PATH.
- `scheduler.CanReservePane`: for nonnegative inputs, successful admission implies
  live plus pending is strictly below the cap. Removing pending accounting fails.
  Live Herdr API measurements, OS visibility and ledger persistence are outside
  this arithmetic proof.

Additional bounded Gomela/SPIN results:

| Model | Explored states | Result |
|---|---:|---|
| ProviderObservationBeforeDispatch | 607 | Durable observation receipt precedes selection and launch |
| ShimLeaseRetirement | 418 | Nested refusal and retirement precede capacity return |
| PaneReservationLifecycle | 734 | Pane retirement precedes reservation reuse |
| Negative: MissingProviderObservationAcknowledgment | 15 | Missing durable receipt blocks launch |
| Negative: ShimMissingRetirement | 39 | Missing retirement receipt blocks reuse |
| Negative: MissingPaneRetirement | 46 | Missing pane retirement blocks reuse |

All 16 positive models report no checked errors; all 14 negative controls report
the expected model deadlock. These are fixed-size channel protocol models, not
direct proofs of SQL, Herdr, provider calls, process cleanup or arbitrary scale.
Source hashes and generated Promela models identify the checked inputs.

## Dynamic pane limits, idle shedding and stall recovery

The final run verifies fifteen production predicates and rejects all fifteen
weakened mutations. Three added predicates are `scheduler.TightenPaneLimit`
(the effective shared cap never exceeds either requested cap), `kernel.CanShedIdle`
(owned, inactive, eligible pre-code globals only), and `kernel.CanRetryStall`
(restricted replay contract, completed retirement, unchanged tree and first session).
Mutations remove cap tightening, the inactive condition and retirement respectively.
These proofs assume correctly derived inputs; they do not prove process observations,
persistence, side-effect confinement, or whole-daemon liveness.

| Model | Explored states | Result |
|---|---:|---|
| DynamicPaneLimitUpdate | 953 | Durable update acknowledgment precedes subsequent admission |
| IdleRetirement | 262 | Handoff acknowledgment precedes retirement |
| StallRetirementBeforeRetry | 1152 | Retirement acknowledgment precedes replacement |
| Negative: MissingPaneUpdateAcknowledgment | 10 | Missing update receipt blocks admission |
| Negative: IdleMissingHandoff | 13 | Missing handoff receipt blocks retirement |
| Negative: StallMissingRetirement | 21 | Missing retirement receipt blocks replacement |

All sixteen positive bounded models pass and all fourteen negative controls fire.
Real process, Herdr and Dolt integration tests separately cover dynamic cap changes,
shutdown/startup registration cleanup, idle retirement and durable stall counters.

## Persistent context, dynamic host capacity and startup recovery

The latest run verifies 18 production predicates and rejects all 18 weakened
mutations. `kernel.CanResumeProvider` requires an enabled, scoped policy and a
non-fresh turn (reviews must remain fresh). Its negative drops the freshness
condition. `scheduler.CanAcquireDynamic` allows existing usage above a reduced
cap but forbids new grants until enough capacity drains; its negative drops
capacity checks. `kernel.CanRetryStartup` requires positive nonexecution,
completed retirement and the first session of a dispatch; its negative drops
retirement. Scope derivation, provider UUID authenticity, filesystem durability,
OS observations and correctly classifying nonexecution remain outside these
predicate proofs and have separate execution tests.

| Model | Explored states | Result |
|---|---:|---|
| PersistentProviderCommit | 416 | Completed context commit precedes reuse |
| DynamicHostDrain | 789 | Existing holder retirement precedes admission at lower capacity |
| StartupProofBeforeRetry | 232 | Nonexecution proof precedes replacement launch |
| Negative: PersistentProviderMissingCommit | 21 | Missing commit receipt blocks reuse |
| Negative: DynamicHostMissingRetirement | 15 | Missing retirement blocks admission |
| Negative: StartupMissingProof | 10 | Missing proof blocks replacement |

All 19 positive models pass and all 17 negative controls fire. The fixed-size
protocol models do not prove the complete daemon, provider conversation semantics,
SQL transactions, weighted fairness or arbitrary process-tree recovery.

## Provider-neutral loops, resident workers and crash resolution

The current verification run checks 21 production predicates and rejects 21
weakened mutations. The added guards enforce loop inactivity/cadence/expiry/count
bounds (`loops.CanDispatch`), exact live resident identity and freshness
(`runtime.CanReuseResident`), and authorized matching uncertain dispatches with
retirement and unchanged-state proof (`kernel.CanResolveUncertain`). Mutations
remove the in-flight, live-identity and retirement conditions respectively.

Gomela/SPIN checks 22 positive bounded models and 20 firing negative controls.
The new protocols cover checkpoint-before-prompt, turn retirement before resident
reuse, and crash resolution authorization/retirement. These are bounded channel
models, not whole-daemon or OS process-tree proofs. Native tests separately
exercise Herdr reuse, concurrent tasks, cancellation, Dolt restart and SIGKILL.
Actual source hashes and generated models are retained in `verification/results`.

## Cron, controller lifecycle and effect receipts

`cron/admission.go`, `controller/guard.go`, and `kernel/effect.go` are exercised
by `scripts/verify.sh` alongside the existing guards. Each has a mutation that
removes a required condition and must be rejected by Gobra. The added Gomela
models cover admission-before-cursor, acceptance-before-marker-removal, and
receipt-before-adoption; their missing-handoff controls must report deadlock.
The archived run is in `results/cron-controller-effects-formal/`: 24 verified
production predicates, 24 rejected mutations, 25 positive and 23 negative
bounded models. Source hashes bind this record to the checked guard/model files.

These checks cover the stated predicates and bounded protocols. Native tests and
real-provider acceptance provide separate evidence about subprocess, Dolt, Git,
Herdr and controller behavior; they are not replaced by formal model success.

## Deployment, drive and external adoption predicates

Gobra now checks 27 production predicates and rejects their 27 weakened
mutations. The new guards cover protected deployment overlap release,
nonduplicated/budgeted drive rearm, and matching performed external receipt
adoption. Gomela/SPIN passes 28 positive bounded models and detects all 26
negative controls, including missing deployment acceptance, drive handoff and
external acknowledgment. Logs, generated models and 79 input source hashes are
archived in `results/deployment-drive-external-formal/`.

These prove the selected predicates and check bounded communication models.
They do not prove Git/HTTP/SQL implementations, source-to-binary correspondence,
external service idempotency, arbitrary process behavior, or the entire daemon.

The [final deployment/drive/external record](results/deployment-drive-external.json)
contains 681 passing race-enabled cases across 26 packages (75.6% coverage), vet,
strict scratch census and the full authenticated two-task dogfood rerun. Both tasks
reached Done, independent drive rearm preserved an actually started turn exactly
once, and a guarded roll replaced the daemon. The five-turn resident loop and
final resource census passed. Earlier cleanup and harness failures are retained
with their corrections; the final dogfood manifest matches current sources and
binaries. The full-suite source snapshot intentionally retains the pre-correction
harness hash; Go production/test sources are unchanged since that suite.

## Schema, review and ticket protocol checks

The adapter increment adds production predicates in
`external/schema_guard.go`, `external/review_guard.go` and
`external/ticket_guard.go`. The schema predicate enforces declaration of pin
keys, the review predicate requires passing/known gate evidence and cleared
comments, and the ticket predicate excludes a second mutation after a row is
started or emitted and requires ordered live execution. Weakening mutations
must fail verification.

The corresponding positive Gomela fixtures model discovery/validation before
an effect, review gate handoffs, and durable outbox ordering. Negative fixtures
remove a required handoff and must expose the resulting blocked protocol.
These are bounded communication models and selected production predicates,
not proofs of arbitrary vendor behavior, schema translation, HTTP/SQL atomicity,
or complete workflow execution. Native tests separately cover typed schema
validation, record-before-verdict, restart recovery and mutation counts.
The complete run passed **30 Gobra production guards**, with **30 rejected
mutations**, and **31 positive / 29 firing negative Gomela models**. The archived
formal input manifest contains 88 matching files. The final native suite passed
770 race-enabled cases, vet and scratch census. See
[the phase record](results/vendor-review-ticket.json) for evidence hashes and
authenticated dogfood results.

## Operator controls and bounded reads

Three additional production predicates check authenticated action permission,
terminal/in-flight task-control safety, and retained event cursor admission.
Gobra verifies all 33 predicates and rejects all 33 weakened mutations. Gomela
and SPIN accept 34 positive models and fire 32 negative controls, including the
new authority/control/read-stream models. Archived inputs and outputs are in
[operator-controls-reads-formal](results/operator-controls-reads-formal/).
These are predicate proofs and bounded model checks, not whole-program proofs
of RPC authorization, storage transactions, process recovery or provider behavior.

The [complete phase record](results/operator-controls-reads.json) links 993
race-enabled test cases, 75.3% coverage, vet, scratch census and authenticated
two-task dogfood. It preserves the initial cached-health assertion failure and
package timeout alongside the clean full rerun and evidence checksums.

## Prophet, backlog and restart guards

The current formal run verifies 36 production guards and rejects 36 weakened
mutations. Gomela/SPIN pass 37 positive models and fire 35 negative controls.
The additions cover explicit Prophet action direction, blocking prerequisite
release and exact execution HEAD/tree restart admission. All 106 formal input
hashes match the archived sources in
[prophet-backlog-restart-formal](results/prophet-backlog-restart-formal/).
These scoped proofs and bounded models do not replace native verification.
The [phase record](results/prophet-backlog-restart.json) records 1,062 passing
race-enabled cases across 26 packages, 74.6% coverage, vet, scratch cleanup and
passing authenticated two-task dogfood with 36 completed provider turns and two
Prophet replies. It retains the historical approval-service blocker and fixture
failures alongside the final passing evidence and matching source hashes.


## Retirement, target policies and Prophet failover

Gobra verifies 39 production predicates and rejects 39 weakened mutations.
Gomela/SPIN accepts 40 positive models and fires 38 negative controls. The new
models cover containment ownership, pinned target-policy handoff and durable
health/retirement before Prophet failover. All 115 archived formal input hashes
match [the formal evidence](results/retirement-target-routes-formal/).

The [phase record](results/retirement-target-routes.json) links the final 1,116-case
native race suite (26 packages, 74.1% coverage), vet, scratch cleanup, and expanded
authenticated dogfood. A review-discovered supersession-chain bug was fixed and
the whole suite rerun. These predicate proofs and bounded models supplement the
native and live tests; they do not prove the complete program.


## Normal targets, census and retired cleanup

Normal target schema v2 now pins destination, namespace, installed tool definitions,
review workspace/approved baseline and ticket policies. Admission, restart,
dispatch, external recovery and ticket emission refuse policy drift. Configured
adapters must already match installed definitions; review targets bind an explicit
task and provisioned workspace. See [targets](../daemon/TARGETS.md).

The read-only [census](../daemon/CENSUS.md) emits reviewable version/digest evidence.
Apply retires one unchanged row at a time through the audited Retire path, with
pacing and partial-progress reporting. [Retired cleanup](../daemon/CLEANUP.md) uses a
Dolt backup verified by offline restore/fsck, prepares every archive before any
deletion, and refuses dirty, foreign, changed or symbolic refs/worktrees. Cleanup
uses plane reads only and preserves partial-recovery/idempotence behavior.

Verification passed **1,165 race-enabled cases across 26 packages**, **74.0%
statement coverage**, vet, both builds and strict scratch cleanup. A final
same-SHA symbolic archive fixture was strengthened after suite launch and passed
a separate focused race run; production sources remained unchanged. Gobra proved
**42 production guards** and rejected **42 mutations**; Gomela/SPIN passed
**43 positive / 41 firing negative controls**. All 124 formal input hashes match.

The final real Codex/Herdr dogfood passed **36 provider turns**, both tasks Done,
five loop iterations, four reused resident panes and two Prophet replies. One
Local task used cron; the Normal task used explicit admission because cron remains
Local-only. Exactly one external effect reached the disposable localhost adapter,
from the Normal task. Different modes/revisions kept their provider contexts
isolated while two turns reused contexts across restart. Census retired the
reviewed fixture, Dolt backup verification passed, and two retired worktrees were
removed only after both archive refs were verified. All 68 host grants were
released (67) or recovered (1); final holders and pane reservations were zero.

Independent review fixed cleanup authority compatibility and symbolic-ref
deletion risks before the full suite. Two dogfood harness errors were corrected:
Normal admission through Local-only cron, and a stale cross-task sharing
expectation for different modes/revisions. Initial evidence remains archived.
See the [verification record](results/normal-target-census-cleanup.json)
for logs, 410 source/binary hashes, 407 dogfood source hashes and 25 evidence hashes.
Scratch-instance cleanup, backup pruning/remote policy, dynamic adapter installation
and review-workspace provisioning remain outside this increment. Formal models
cover their explicit predicates/protocols, not Git, Dolt or provider internals.


## Scratch cleanup and dynamic provisioning — 2026-10-19

Creation-only scratch instances now support authorized shutdown, offline Dolt
backup/restore/fsck, verified external file backups, archive-before-delete ordering,
and inode-bound staged deletion with checked retry. Ordinary instances and dirty
worktrees refuse removal. Retired Herdr IPC requires exact owned bindings and
no open descriptors; endpoint identity metadata is retained in the hashed backup.
The IPC path requires lsof and refuses ambiguous or unsupported paths.
See [scratch cleanup](../daemon/SCRATCH.md).

Audited adapter installation discovers and pins immutable schema revisions.
Target schema v3 resolves a pinned adapter per task without mutating shared live
configuration. Explicit review-workspace planning/preparation creates the exact
Git binding, verifies an approved package baseline, and persists pending/ready
state across restart. Admission reuses that workspace and refuses lost or changed
bindings. See [targets](../daemon/TARGETS.md) and
[workspace preparation](../daemon/REVIEW_WORKSPACES.md).

The final native suite passed **1,242 race-enabled cases across
26 packages**, **73.6% statement coverage**,
vet, both builds and scratch census. Gobra proved **46 production guards** and
rejected **46 mutations**; Gomela/SPIN passed **47 positive / 45 firing negative
controls**. Formal checks cover their explicit predicates and protocols, not
Git, Dolt, filesystem or provider internals.

The expanded real Codex/Herdr run passed **36 provider turns**,
both tasks Done, five loop iterations, two Prophet replies, mid-turn crash
recovery, drive/controller restart and mixed Local/Normal isolation. The Normal
task retained adapter revision 1 after revision 2 was installed and performed
exactly one effect against the disposable localhost adapter. Scratch cleanup
removed the instance root, verified 2076 retained backup
files and 4 owned archive refs, and passed an idempotent
repeat. Final leases and pane reservations were zero; 70 grants were
released (69) or recovered (1).

Dogfood exposed a real stale-index bug: commit-tree/ref CAS advanced HEAD while
leaving the task index at the old base. Guarded index reconciliation now updates
only a known base/reviewed index under Git's lock, preserves working files and
foreign staging, and also runs before verified interrupted-commit adoption.
Native regressions cover clean completion, adoption, foreign staging, lock
contention and SIGKILL. Unknown abandoned Git locks remain protected and require
explicit operator inspection/repair. The initial failed runs remain archived.

See the [verification record](results/scratch-provisioning.json) for
source manifests, checksummed logs, native/formal results and dogfood evidence.
All production, test and binary sources were frozen for the final suite and
dogfood. Reconciliation also normalizes its known index to the new base under
the same lock, preserving resolved edits for the next reviewed commit.
Adapter installation configures reachable endpoints; vendor executable/fleet
provisioning, adapter retirement, and backup pruning/remote replication remain
outside this increment. Cron remains Local-only.


## Adapter retirement and configurable fleet provisioning — 2026-10-20

`fae template adapter-retire --id ID --reason REASON --authorizing-message ID`
records an immutable object tombstone and returns the first actor, reason and
retirement time on every repeat. Historical revisions remain readable. New
installs, target bindings, task/backlog admissions and preparation creation check
retirement in the same transaction as their write. Existing pinned tasks and
already-pending preparations continue, including receipt recovery after restart.

A review policy may declare `provision_tool`, an installed schema-discovered
adapter alias with exact package, workspace, branch and integration-tip pins.
Explicit `fae workspace review-prepare` persists intent and an attempted marker
before one provisioning POST. Ambiguous results recover only through exact
receipt lookup; a missing receipt never permits blind replay. The receipt must
prove the exact binding before a green fleet baseline can publish ready.
Provisioning tools and aliases of the same operation are refused by workflow
admission, execution and uncertain-effect recovery. Endpoint comparison follows
the HTTP transport's trailing-slash normalization. Health refuses incomplete
provisioning and revalidates its immutable evidence before checking the baseline.
See [targets](../daemon/TARGETS.md) and [workspace preparation](../daemon/REVIEW_WORKSPACES.md).

Final verification passed **1,264 race-enabled cases across 26
packages**, **73.5% statement coverage**, vet, both builds and
scratch census. Gobra proved **48 production guards** and rejected **48 weakened
mutations**. Gomela/SPIN passed **49 positive / 47 firing negative controls**.
These are predicate and bounded protocol checks, not proofs of vendor, database,
Git, filesystem or provider internals.

The final real Codex/Herdr run passed **36 provider turns**, both tasks Done,
five loop iterations, two Prophet replies, mid-turn crash recovery and controller
restart. Exactly one fleet mutation occurred: its failed acknowledgment was
recovered after restart through receipt lookup, with no duplicate POST. Adapter
retirement preserved the first tombstone, refused new writes/bindings and allowed
the bound Normal task to finish after restart. Scratch deletion and repeat cleanup
passed with 2,076 verified retained backup files and
4 owned archive refs. Final leases and pane reservations were zero.

Review caught a trailing-slash alias bypass after the earlier verification run
started. It was fixed and covered by unit/native refusal tests; both full native
verification and dogfood were rerun on the final sources. The final native runner
uses the standard script with separate result filenames to preserve earlier
evidence. All final source, binary and evidence hashes match the
[verification record](results/adapter-fleet.json).

Fleet effects remain confined to configurable adapters; no vendor-specific
implementation, executable installation, adapter deletion/reactivation or
credential lifecycle is supplied. An attempted operation without a provable
receipt remains uncertain and requires operator investigation; no force-replay
command is provided. Backup pruning/remote replication and other remaining
requirements are outside this increment.

## Backup lifecycle phase

1,293 race-enabled test cases across 26 packages passed, with 73.5% statement coverage, vet and scratch census. Gobra verified 50 production predicates and rejected 50 mutations; Gomela/SPIN passed 51 positive models and observed 49 firing negative controls.

The production guards are `CanPruneBackup` (verified, unprotected, same reviewed identity) and `CanConfirmBackupReplication` (matching receipt and remote content). Bounded models cover survivor/identity checks before deletion and durable attempt/receipt/content ordering. Negative controls remove those obligations and must fire. These prove the guards and models, not full filesystem, HTTP, Dolt or provider correctness.

[Evidence record](results/backup-lifecycle.json) contains source/evidence hashes and explicit limits; [formal archive](results/backup-lifecycle-formal/verify.log) and [real-provider acceptance](results/backup-lifecycle-dogfood-20261020/result.json) are retained. Native and dogfood remote effects use disposable localhost services.

## Automatic retention and scratch-archive phase

1,319 race-enabled test cases across 26 packages passed, with 73.6% statement coverage, vet and scratch census. Gobra verified 52 production predicates and rejected 52 mutations; Gomela/SPIN passed 53 positive models and observed 51 firing negative controls.

Production predicates `CanRunRetentionSchedule` and `CanRemoveScratchArchive` are checked alongside bounded durable-claim-before-prune and verified-survivor-before-removal models. Weakened guards and models must fail. These checks do not prove the entire filesystem, Dolt, HTTP, Git or provider implementations.

[Evidence record](results/retention-automation.json) includes exact source/evidence hashes, [formal output](results/retention-automation-formal/verify.log) and [real-provider acceptance](results/retention-automation-dogfood-20261020/result.json). Offline archive authorization is explicitly the local owner plus apply and reviewed digest; the canonical recovery copy is preserved.

## Automatic backup creation and replication phase

1,337 race-enabled test cases across 26 packages passed, with 73.7% statement coverage, vet and scratch census. Gobra verified 54 production predicates and rejected 54 mutations; Gomela/SPIN passed 55 positive models and observed 53 firing negative controls.

Production guards cover due/authorized backup creation and replication scheduling. Bounded models require a durable occurrence before creation and verified replication receipt before advancing the pending source. Weakened guards/models are rejected. These are predicate/order proofs, not complete verification of the filesystem, database or remote service.

[Evidence](results/backup-schedules.json) retains exact source/evidence hashes, [formal output](results/backup-schedules-formal/verify.log) and [real-provider acceptance](results/backup-schedules-dogfood-20261021/result.json).


## Replication unpinning and backup quotas phase

1,352 race-enabled test cases across 26 packages passed, with 73.6% statement coverage, vet and scratch census. Gobra verified 56 production predicates and rejected 56 mutations; Gomela/SPIN passed 57 positive models and observed 55 firing negative controls.

`CanUnpinReplication` and `CanAdmitBackupStorage` production guards are checked alongside bounded pin-release and serialized-capacity models. Weakened predicates and models must fail; these checks do not prove complete filesystem, Dolt, HTTP or provider implementations.

[Evidence record](results/backup-storage.json), [formal output](results/backup-storage-formal/verify.log), and [real-provider acceptance](results/backup-storage-dogfood-20261021/result.json) preserve exact source and evidence hashes.
