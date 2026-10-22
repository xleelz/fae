# Local verification record

Executed on macOS arm64 with Go 1.27.2. Tests are scoped to the implemented
daemon, durable engine, scripted workflow, direct/Herdr execution and restricted
real Codex provider bridge; see [IMPLEMENTATION.md](IMPLEMENTATION.md) for missing product paths.

## Executed checks

- `go test -race -count=1 -coverprofile=.cache/coverage.out ./...` with
  `FAE_TEST_DOLT_BINARY` selecting a disposable Dolt 2.5.0 server: passed.
- `go vet ./...`: passed.
- Domain canonical JSON fuzz: 44,519 executions in the recorded 3-second run,
  passed (two workers).
- Workflow gate fuzz: 246,047 executions in the recorded 3-second run, passed
  (two workers).
- Workflow property suite: 10,000 seeded generated graph cases, plus exhaustive
  kernel edges and three-valued logic tables.
- Durable engine: 10,000 generated graphs with serialized recovery boundaries,
  malformed completion, schema and provenance refusal, bounded retry exhaustion.
- Daemon with real Dolt: authenticated RPC, concurrent scripted jobs, durable event
  stream, graceful restart, malformed-task isolation and owned-child crash recovery.
- Built CLI smoke: 22 scripted artifact records, 61 events, Done state preserved
  across daemon restart at version 52; idempotent admission returns version 1.
- CLI example: validation succeeds; replay follows `Pending -> test -> check ->
  Done`; cross-task evidence is refused.
- Dolt integration: private server, migration replay, chunked >1 MiB blob,
  workflow/molecule binding, task/artifact/transition writes, stale transition
  refusal, atomic rollback and actor-labelled Dolt commits.
- Worktree regression review: symlink ancestors are rejected before directory
  creation, ambient Git repository selectors are removed, concurrent managers
  serialize, dirty removal and foreign repositories are refused, cancellation
  interrupts a contended filesystem lock.
- Credential review: symlink and unsafe-mode files are refused, duplicate JSON
  request keys are rejected, and unauthenticated calls never invoke handlers.

The final integrated run on 2026-10-17 passed **530 test cases** with **77.8%
statement coverage**, using `FAE_TEST_DOLT_BINARY=/tmp/dolt-darwin-arm64/bin/dolt ./scripts/test.sh`. This includes `go vet`, race detection, real Dolt/Herdr
integration tests, 10,000 generated durable-recovery graphs, and the final
scratch-directory census. The rebuilt CLI smoke test separately passed with
22 artifact records, 61 events, and version 52 preserved across restart.

Targeted race runs also exercised actual Herdr lifecycle, durable lease recovery,
automatic text-conflict remediation, omitted-input and commit bypass refusal,
and concurrent integration with the committed loser's changes preserved.

Additional executed evidence:

- Real direct-process bridge fixture: full default workflow, actual RED source
  edit and failing named test, frozen file, actual implementation, forced fresh
  Go compilation, GREEN/reviews/QA, exact reviewed commit and dedicated-ref
  integration. Completed restart does not replay effects; interrupted turns park.
- Host broker: literal argv, sanitized environment, timeouts and owned-group
  cleanup, bounded output, observed-compiler controls, actual nonzero test counts,
  whole-tree reviewed-byte commits and checked-out-target refusal. Changed-base
  tests cover immutable plans, idempotent/interrupted application, exact own
  hunks, committed plus uncommitted work, binary/deletion/mode preservation,
  precise lost-hunk paths, concurrent ref CAS and ancestor/patch/blob/empty
  containment. Full daemon tests require revalidation on the new tree before
  landing. Supported conflicts re-enter bounded implementation; original-hunk
  loss and unsupported conflicts refuse. Known no-write integration races
  reconcile committed deltas and repeat approvals before landing.
- Runtime: fresh isolated sessions, strict identity and handoff schema, stale
  timestamp/symlink/FIFO refusal, output bounds, normal-exit and cancellation
  descendant cleanup, lease lifetime through collection and validation.
- Leases: eight separate-process host-ledger contenders, eight real-Dolt clients,
  persisted held lease through child kill/restart, FIFO/capacity/owner guards,
  role scope isolation, telemetry refusal and grant-then-cancel cleanup. Real
  daemon SIGKILL during a leased workload recovers role and host capacity from
  complete session-linked watchdog receipts, retains a foreign holder, and
  replays a partial two-store recovery idempotently.
- Actual Herdr: Direct/Herdr handoff conformance, literal arguments, foreign and
  malformed/stale/oversized output refusal, concurrent private sessions,
  cancellation, owned descendants and daemon-parent SIGKILL.
- Process ownership: exact birth identity and private registry validation,
  watchdog retirement, live-owner/foreign-owner/reused-identity refusal, ambiguous
  leaderless groups and cleanup records. No escaped-descendant containment proof
  is claimed.
- Gobra: eighteen actual production predicates with eighteen rejected weakened mutations.
  Gomela: nineteen positive bounded models and seventeen negative controls. Exact subjects,
  assumptions, state counts and source hashes are in `verification/README.md`.
- Authenticated Codex `gpt-6-astra`: two real tasks through Herdr 0.9.5 and Dolt,
  genuine REDs, fresh GREEN builds, independent reviews and QA, and a drained
  mid-run restart with unchanged checkpoint chains. Both reached Done (35 and 22
  artifacts, versions 83 and 52). One lost integration CAS, reconciled and repeated
  verification before landing. All 58 host-lease grants were released; no owned
  process records or Herdr sockets remained. There were 29 real CLI completed turns,
  matched exactly to AgentTurn/Review provenance. Complete evidence is retained in
  [herdr-dogfood](verification/results/herdr-dogfood/summary.json).

Automated live integration tests use explicit protocol fixtures; the separate
opt-in two-task acceptance uses real provider inference. The restart is a planned
drain to durable stage boundaries; it does not establish recovery from every
possible mid-turn crash. Historical short fuzz counts above are retained from
the foundation increment, not rerun here. Coverage does not establish full
requirement satisfaction.

## Reproduce

```sh
./scripts/test.sh
FAE_TEST_DOLT_BINARY=/absolute/path/to/dolt ./scripts/test.sh
./scripts/install-verifiers.sh
./scripts/verify.sh
go build -o bin/fae ./cmd/fae
FAE_TEST_DOLT_BINARY=/absolute/path/to/dolt python3 scripts/smoke-cli.py
# Optional authenticated provider run; uses the configured Codex account:
go build -o bin/fae-codex-bridge ./cmd/fae-codex-bridge
python3 scripts/dogfood.py --model gpt-6-astra --dolt /absolute/path/to/dolt \
  --evidence-dir verification/results/new-dogfood-run
```

Dolt is optional for the ordinary unit suite, but its isolated integration test
must be explicitly enabled to reproduce the SQL acceptance evidence. The test
harness accepts a binary path, never a DSN, and disables Dolt's asynchronous event
flushing so it cannot recreate test configuration after cleanup. Its port is
computed from the scratch directory. No developer control plane is used.

Formal verification results and tool compatibility patches live under
[verification](verification/). Gobra verifies eighteen production predicates and
rejects eighteen weakened mutations. Gomela checks nineteen bounded channel models
and seventeen broken controls, not the complete runtime. Broader proofs of
transitions, no-bypass, containment/deletion, single-writer authority and crash
recovery remain work to do.

Independent subagent review found and prompted behavioral regression fixes for
Git environment redirection, credential symlinks, empty sanitization prefixes,
worktree writes through ancestor symlinks, mutable workflow protection flags,
cross-task protected evidence, and JSON numeric comparison precision.

## Provider health, command shims and pane accounting checks

New coverage exercises durable health penalties/decay/hold, ordered pinned routes,
all-route parks without attempt charges, restart and healing, transactional
log/event idempotence and provider-log pagination. Structured provider errors
and quoted work content are classified separately.

Command tests cover actual nested exit 77, PATH ordering, parallelism clamps,
absolute/symlink target escapes, executable flags, RAM-temporary outputs, bounded
formatter trees and huge files. The supported provider disables command tools;
these tests do not establish a hostile executable sandbox.

Pane tests compare a zero ledger to a full live census, concurrent reservations,
warmup accounting, exact owner recovery and refusal cleanup. Native Herdr tests
include two workspaces on one server. Historical authenticated dogfood records
remain tied to their original source hashes. The runtime-recovery increment adds
a separate authenticated rerun described below.

The final command, counts, coverage, formal totals and Go source hashes are
recorded in [provider-shims-panes.json](verification/results/provider-shims-panes.json).

## Dynamic pane limits, idle sessions and stall recovery checks

The new tests cover audited pane-limit updates, next-round admission, shared
minimum caps, preserved reservations when lowering a cap, durable custom-workflow
startup floors, and registration removal on close and failed startup. Idle tests
cover protected and active roles, three-round pressure, foreign saturation,
PaneCapStarvation restricted to pane-blocked jobs, TTL, handoff persistence,
identity refusal, actual Herdr retirement and daemon integration with real Dolt.
The trusted adapter hook is excluded from serialized daemon configuration.

Stall tests distinguish silent CPU work from inactivity and refuse to infer
inactivity from a failed process census. Execution tests require retirement,
unchanged tree and an explicit restricted-effects contract before replay. A
Dolt-backed daemon test drains after the first stall, restarts with its durable
counter, and escalates the third stall without leaking leases. General external
effects remain uncertain and are not automatically replayed.

The final runtime-recovery dogfood rerun passed with the rebuilt binaries:
two authenticated `gpt-6-astra` tasks reached Done through Herdr, with 29 real
completed turns matching provenance. The pane limit changed to 16 during active
work, survived the drained restart, and was raised to 24. Both checkpoint chains
were preserved. One integration race caused reconciliation and fresh approvals;
final artifact counts were 35 and 22 (versions 83 and 52). All 58 host leases
were released, no pane reservations remained, and shutdown left no owned process
records or Herdr sockets. No stalls occurred in this provider run; forced stall
recovery is covered by the separate native/Dolt tests above.

[Runtime-recovery verification](verification/results/runtime-recovery.json) records
494 passing cases, 77.7% statement coverage, formal totals and source hashes.
[Fresh provider evidence](verification/results/runtime-recovery-dogfood-20261017/result.json)
includes checkpoints, final evidence, lease/pane audits, provider observations and
completed turns, binary/source hashes and a verified evidence checksum manifest.
The previous provider-shims record and earlier dogfood remain historical.

## Persistent provider, dynamic host and startup checks

Provider tests cover same global context across tasks/restart, task-bound reuse
within a task and isolation across tasks, fresh reviews, policy/revision/profile/
mode/route isolation, explicit task headers, private cache paths, serialized reuse,
pending/failed context rotation, and mismatched provider UUID refusal. A real
provider run separately checks the installed CLI's continuation behavior.

Host tests cover dynamic partial updates and audit authorization, persisted
configuration across restart, next-round telemetry decisions, shared conservative
policies, active-task preservation on cap reduction, exact-owner registration
cleanup, malformed/foreign registration refusal, and distinct requested/effective
pane limits. Lowering a heavy cap preserves overweight holders and FIFO waiters;
raising it resumes admission.

Startup tests cover missing executables, failed transport preparation, bounded
gated readiness, cancellation without late launch, retirement proof, changed tree,
partial multi-review uncertainty, and durable three-attempt exhaustion with fresh
IDs and no task/stage budget charge. A single-route Dolt restart regression proves
nonexecution does not penalize provider health and retains each diagnostic.
Launched failures remain uncertain. The timeout bounds transport readiness, not a
generic provider prompt-acknowledgment protocol.

Final acceptance for this increment passed with **530 cases and 77.8% statement
coverage**, vet, race detection and scratch census. Gobra verified 18 production
predicates and rejected 18 mutations; Gomela/SPIN passed 19 positive models with
17 firing negative controls.

The fresh authenticated run completed both tasks in 29 real provider turns.
Thirteen turns had persistent context audits; eight resumed a prior provider
thread, including two after daemon restart and two task-bound resumptions. Three
global threads served both tasks; no task-bound thread crossed tasks and reviews
stayed fresh. Pane/heavy/task/load limits changed during active execution,
survived restart, and capacity was raised afterwards. Checkpoint chains stayed
unchanged; integration reconciliation repeated verification before landing.
All 58 host leases were released; no owned pane reservation, process record or
Herdr socket remained. Startup faults were not injected into provider calls;
those recovery bounds were exercised by the native/Dolt tests.

[Final verification record](verification/results/persistent-host-startup.json)
and [provider result](verification/results/persistent-host-startup-dogfood-20261017/result.json)
retain exact counts, checkpoints, provider-session audits, resource audits,
source/binary hashes and a verified evidence checksum manifest. Reproduce with
`--persistent-sessions --evidence-dir verification/results/a-new-run` in addition
to the model and binary paths shown above. Historical records remain unchanged.

## Provider-neutral loops, resident Herdr sessions and crash resolution

New coverage exercises five audited scripted loop prompts in one context,
expiry/cadence/pause/resume/retirement, loaded-state validation, interruption
without replay, deferred admission with fresh attempt identities, and loop/workflow
fairness at one dispatch slot. Native loop execution uses real Dolt leases,
separate pinned worktrees, read-only checks and strict output validation.

Resident tests execute successive bridge children inside the same Herdr worker,
refresh configuration, isolate task-bound sessions, share eligible globals,
run independent tasks concurrently, defer overlapping global requests, protect
active residents from shedding, expire idle residents, and retire cancelled turns.
Lifetime pane reservations are distinct from each turn's heavy/role leases.

Native SIGKILL tests cover redispatch, read-only acceptance, escalation, retained
budgets, exact dispatch identity, authority, tree/HEAD checks and no automatic
replay. A separate crash lands after process retirement but before journal
acknowledgment, proving retained retirement receipts recover both lease pools
without releasing foreign holders. Silent MySQL handshakes time out without
applying startup deadlines to subsequent runtime queries.

The formal increment checks 21 production predicates with 21 rejected mutations,
and 22 positive/20 negative bounded Gomela/SPIN models. These results do not imply
verification of the complete daemon or arbitrary provider and OS behavior.

Final consolidated verification passed **582 cases across 23 packages**, with
**77.0% statement coverage**, vet and race detection. This combines the full run
for unchanged packages with complete final daemon/runtime reruns. One live
lifecycle fixture exceeded its 40-second limit while suites and provider work
overlapped; the unchanged test passed on rerun. Original and consolidated test
streams and coverage are retained, rather than presenting the initial runs as
clean. See the [verification record](verification/results/loops-resident-crash.json).

The [final real-provider run](verification/results/loops-resident-crash-dogfood-20261017-r3/result.json)
completed two tasks and five same-context loop iterations in 34 completed provider
turns. Seven resident panes were reused; the five loop prompts used one thread
and one pane. SIGKILL recovery required explicit audited redispatch, and a later
drained restart preserved context and evidence. All 62 host grants ended in
61 releases and one recovery; no owned resources or socket residue remained.
The checksum manifest and all recorded source/binary hashes match the final code.

## Cron, controller and local-effect recovery increment

New tests exercise pinned cron revisions, end/catch-up boundaries, fair admission,
aged creation grants, lifecycle operations and admission-before-cursor restart.
Controller tests use actual native sandbox probes and a private launcher, check
pinned hashes and exact process identity, maintenance suppression, durable alert
replay and bounded watchdog rearming. Effect tests use real subprocess SIGKILL at
seven boundaries, including pre-execution, post-CAS and post-receipt; forged
intent, changed trees/refs and host commands without receipts are refused.

The three added production guards are cron admission, controller maintenance and
local-effect adoption. Their weakened mutations and missing-handoff protocol
models must fail. These remain predicate proofs and bounded protocol checks,
not proof of the whole daemon or an arbitrary provider/OS.

Final verification passed **621 cases across 25 packages**, with **74.8% statement
coverage**, vet, race detection and a clean temporary-directory census. The first
run's live lifecycle fixture crossed its old 40-second wait while its final read
already showed `Done`. The helper now allows 60 seconds within the existing
90-second fixture context and checks the final observation; the complete fresh
rerun passed. Both streams are retained in the
[verification record](verification/results/cron-controller-effects.json).

Gobra verified 24 production predicates and rejected 24 weakened mutations.
Gomela/SPIN passed 25 positive models and detected all 23 negative controls.
The authenticated dogfood completed two cron-admitted tasks with no duplicate
occurrences, an actual provider-turn SIGKILL with explicit recovery, and a later
maintenance stop/start through the native launcher. The independent watchdog
suppressed the expected death alert during maintenance. Five loop iterations
reused one provider thread and resident pane; 34 provider turns completed in all.
Seven panes were reused, and all 66 host grants ended in 65 releases and one crash
recovery. Shutdown left zero owned pane reservations, process records, retirement
receipts and Herdr sockets. Evidence manifests and every recorded Go source,
binary, harness and formal-source hash match the final files.

## Deployment, independent drive and external recovery increment

Deployment tests cover clean-source proof, build environment isolation, gated
refs, integration ancestry/CAS, durable overlap holds, protected tasks, native
launcher acceptance failure and rollback. Independent-drive tests hold live
workers across loop generations, retain pending completions, reconcile exact
lost-save acknowledgments, contain panics and enforce durable rearm budgets. Task and loop regressions cover
precommit failures, lost acknowledgments with unavailable reconciliation reads,
queued completions after administrative saves, altered checkpoint refusal, and
exactly-once launch/collection across generations.
Watchdog tests cover aged grants, revocation, restart, fresh general mutations,
and recovery after failed deployment preflight with unchanged daemon/Dolt PIDs.

External tests use an isolated HTTP fixture, including actual SIGKILL after the
server performs a mutation but before the response/local receipt. Restart and
explicit query adoption must finish with exactly one POST. Forged results,
mismatched pins, missing queries, malformed known-ID responses and transport
replay are checked. Dry-run performs zero HTTP requests. No real third-party
ticket or review is created by these tests.

The authenticated harness adds `--drive-restart --deployment-roll`: it stops and
watchdog-rearms the drive during a real provider turn, checks the unchanged
process/plane/dispatch identities and exactly one final completion, then performs
a same-binary guarded roll through the native launcher. Changed-source build
policies are covered by native fixtures rather than the unrelated toy repository.

Final verification for this increment passed **681 cases across 26 packages**,
with **75.6% statement coverage**, vet, race detection and a clean scratch census.
The earlier broad run passed its 662 cases but failed the scratch census: a
cancelled command could leave an empty owner directory before registration.
The fix removes only empty unregistered directories and preserves unresolved
ownership records. Both the original 20ms cancellation path and a deterministic
early-cancellation regression passed 20 race-enabled repetitions with their own
scratch checks. The original stream and failure log are retained alongside the
complete clean final rerun.

The first dogfood reached Done for both tasks but failed a new harness assertion
that incorrectly required completed-turn provenance for an in-flight dispatch
that never started. The recorded nonexecution diagnostic and deferral explained
its later replacement dispatch. The corrected harness waits for actual provider
turn start, checks one start/completion/provenance for retained turns, and requires
explicit nonexecution evidence plus zero provider starts for deferred dispatches.
Regression checks against the retained run reject duplicate completion and
missing nonexecution evidence. No production change was needed for this correction.

The [complete fresh dogfood rerun](verification/results/deployment-drive-external-dogfood-20261018-r2/result.json)
passed both tasks and all strengthened identity assertions, five loop iterations,
34 completed provider turns, seven reused resident panes, explicit crash recovery,
independent drive rearm and guarded daemon roll. All 63 host grants were released
or recovered; owned resource and socket censuses were empty after shutdown.
See the [verification record](verification/results/deployment-drive-external.json)
for checksums, original failures, final results and remaining implementation scope.

## Adapter schema, review and outbox coverage

The schema tests use recorded descriptors served by local HTTP fixtures, with no
configuration-only discovery bypass. They cover exact/qualified tool identity,
unknown pin keys, required fields, enums, types, large integers, duplicate JSON,
response limits, redirects, cancellation, forbidden remote schema references,
room-cache copies/expiry, immutable pin conflicts, and daemon-only templates.
Startup can accept a configured numeric revision token, but concrete enum/type
violations must fail before any effect request. Schema changes cannot silently
reinterpret an existing intent.

Review tests exercise draft-only creation, explicit severity presence, per-check
requirements, failing/blocked/skipped/absent statuses, baseline health, workspace
binding refusal, separate publish/merge calls, opt-out call absence, the Done
merge floor and bounded same-review revision handling. Native fixtures retain
review identity across restart and record returned IDs before verdicts. Ticket
tests exercise persisted-history transitions, ordered outbox progress, required
admission/role bindings, schema-derived room actions, durable attempts/errors,
query-only uncertain restart and exactly one create mutation. These fixtures
operate against disposable local servers and Dolt; they do not open third-party
reviews or tickets.

Startup schema GET requests are expected for installed capabilities. Dry-run
assertions distinguish those installation reads from task execution: no effects
GET/POST or review status/baseline request is permitted by a dry-run task.
Real-provider dogfood remains a separate acceptance layer for authenticated
provider sessions, crash/restart and resource cleanup; fixture success alone
must not be reported as successful authenticated vendor review/ticket delivery.
Formal predicate and bounded channel checks likewise complement, rather than
replace, native SQL/HTTP/process tests.

The final schema/review/outbox suite passed **770 race-enabled cases across 26
packages**, vet and strict scratch census, with **74.8% statement coverage**.
The complete daemon workflow runs implementation, tests and approvals twice,
then submits revision 2 to the same review and separately publishes and merges.
Pending vendor checks have a separate durable polling budget and preserve
implementation attempts. The initial aggregate run exposed stale migration
mock expectations and a non-atomic crash-test readiness file; both were fixed
and its logs are retained alongside the complete clean rerun.


The [authenticated adapter-phase dogfood](verification/results/vendor-review-ticket-dogfood-20261018/result.json)
passed both tasks, 34 completed provider turns, five same-thread loop turns,
mid-turn crash recovery, independent drive restart and guarded roll. All owned
host grants and panes were released or recovered. The
[complete phase record](verification/results/vendor-review-ticket.json) includes
final source/binary hashes, formal evidence and retained initial failures.

## Operator authority, task controls and read streams

The phase adds native authenticated delegation tests (including cross-task and
role refusals), transactional citation consumption and rollback, all ten task
controls, exact lost-acknowledgement checkpoint recovery, and in-flight hold and
context isolation. A real broker commit verifies that unsafe post-commit restart
refuses before changing state or budgets. CLI tests check the actual RPC shape.

Read tests cover bounded/coalesced query slots, independent waiter cancellation,
writer-lock-independent cached reads, audited retention advancement, clock
rollback/restart, preserved archive rows, authenticated gap/resume, and slow
subscriber overflow. Controller and cron regressions cover cache publication
boundaries; deployment mutations still verify current executable bytes.

Gobra checks the production authority, task-control and cursor guards; each has
a rejected mutation. Gomela adds corresponding positive/negative bounded models.
The aggregate is 33 production predicates/33 rejected mutations and 34 positive
models/32 firing negative controls. These do not prove the complete daemon or
its external environment.

The expanded native daemon suite uses a twenty-minute per-package Go test
timeout. The initial run exceeded Go's former ten-minute default; individual
operation deadlines remain bounded. Its stale cached-health assertion and
aggregate-timeout evidence are retained with the final phase record.

The final operator/read suite passed **993 race-enabled cases across 26 packages**,
`go vet`, and the strict scratch census, with **75.3% statement coverage**.
The original cached-health assertion failure and aggregate ten-minute timeout
are archived separately; the clean full rerun completed in about 12.4 minutes.
Formal verification passed 33 Gobra predicates/33 rejected mutations and
34 positive/32 firing negative Gomela models.

The authenticated operator/read dogfood also passed: both tasks reached Done,
with 34 completed provider turns, five loop iterations, seven reused resident
Herdr panes, mid-turn SIGKILL recovery, independent drive restart and guarded
daemon roll. Host grants ended with 62 releases and one recovery out of 63,
zero holders and zero pane reservations. Source, binary and formal-input hashes
match the archived evidence. See the
[complete phase record](verification/results/operator-controls-reads.json).

## Prophet/backlog/restart verification

New tests cover scoped Prophet context and paged chat, exact directed actions,
private provider input and host leases; backlog graph cycles, edge ordering, SQL
rollback, restart-safe dependency dispatch and own-landing reconciliation; and
execution-head provenance, foreign-head/tree refusal and legacy post-commit
restart through another RED/green cycle and changed-base integration.

`scripts/dogfood.py --prophet-sessions` adds real per-task/global read-only Prophet
turns before the final lease census. The existing two-task crash/restart, provider
loop and resident-Herdr assertions remain enabled by their corresponding flags.
The full native suite and authenticated dogfood now pass for this increment.

Prophet/backlog/restart verification passed **1,062 race-enabled cases across 26
packages**, with **74.6% statement coverage**, vet, both binary builds and the
strict scratch census. Gobra verified **36 production guards** and rejected
**36 mutations**; Gomela passed **37 positive / 35 firing negative models**.
All 106 archived formal input hashes still match.

Authenticated two-task dogfood passed with **36 completed provider turns**,
five loop iterations, seven reused resident Herdr panes and two answered Prophet
scopes. Mid-turn SIGKILL recovery, independent drive restart and guarded daemon
roll passed. All 67 host grants were released (66) or recovered (1); final lease
holders and pane reservations were zero. All 367 dogfood source/binary hashes
and 21 evidence hashes match.

The user selected full access after upstream automatic-review overload blocked
command launch. Native execution then exposed fixture-only post-commit restart
issues: restore needed an explicit HEAD source, and the repeated workflow plus
reconciliation needed explicit attempt/test budgets. Production guards and
default limits were unchanged. Initial failures and final passing logs are
retained in the [verification record](verification/results/prophet-backlog-restart.json).


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

Normal target schema v2 now pins destination, namespace, installed tool definitions,
review workspace/approved baseline and ticket policies. Admission, restart,
dispatch, external recovery and ticket emission refuse policy drift. Configured
adapters must already match installed definitions; review targets bind an explicit
task and provisioned workspace. See [targets](daemon/TARGETS.md).

The read-only [census](daemon/CENSUS.md) emits reviewable version/digest evidence.
Apply retires one unchanged row at a time through the audited Retire path, with
pacing and partial-progress reporting. [Retired cleanup](daemon/CLEANUP.md) uses a
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
See the [verification record](verification/results/normal-target-census-cleanup.json)
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
See [scratch cleanup](daemon/SCRATCH.md).

Audited adapter installation discovers and pins immutable schema revisions.
Target schema v3 resolves a pinned adapter per task without mutating shared live
configuration. Explicit review-workspace planning/preparation creates the exact
Git binding, verifies an approved package baseline, and persists pending/ready
state across restart. Admission reuses that workspace and refuses lost or changed
bindings. See [targets](daemon/TARGETS.md) and
[workspace preparation](daemon/REVIEW_WORKSPACES.md).

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

See the [verification record](verification/results/scratch-provisioning.json) for
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
See [targets](daemon/TARGETS.md) and [workspace preparation](daemon/REVIEW_WORKSPACES.md).

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
[verification record](verification/results/adapter-fleet.json).

Fleet effects remain confined to configurable adapters; no vendor-specific
implementation, executable installation, adapter deletion/reactivation or
credential lifecycle is supplied. An attempted operation without a provable
receipt remains uncertain and requires operator investigation; no force-replay
command is provided. Backup pruning/remote replication and other remaining
requirements are outside this increment.

## Backup lifecycle verification

1,293 race-enabled test cases across 26 packages passed, with 73.5% statement coverage, vet and scratch census. Gobra verified 50 production predicates and rejected 50 mutations; Gomela/SPIN passed 51 positive models and observed 49 firing negative controls.

The backup tests cover keep bounds and snapshot union coverage, source pins, stale preview/content/manifest/inode/permission drift, unknown/symlinked entries, serialized lifecycle operations, operator/runtime authorization with zero-effect refusal assertions, deterministic archive bounds and corruption, lost acknowledgment/restart, missing objects, wrong receipt identity, altered/oversized content and repeated remote verification. The dogfood fixture deliberately persists one upload then returns 503; restart recovery and repeat use queries, with exactly one upload. It additionally extracts only validated regular relative tar members, verifies every manifest hash, restores the remote Dolt backup and runs fsck. Local retention and marked scratch cleanup follow.

See [record](verification/results/backup-lifecycle.json), [native suite](verification/results/backup-lifecycle-suite.log), [formal suite](verification/results/backup-lifecycle-formal/verify.log), and [dogfood result](verification/results/backup-lifecycle-dogfood-20261020/result.json). The native, formal and dogfood source manifests match the final frozen inputs.

## Automatic retention and scratch-archive verification

1,319 race-enabled test cases across 26 packages passed, with 73.6% statement coverage, vet and scratch census. Gobra verified 52 production predicates and rejected 52 mutations; Gomela/SPIN passed 53 positive models and observed 51 firing negative controls.

The full native suite ran with the final daemon/library sources. A later CLI-only default correction from 24h to 1h was covered by a new default assertion and fresh cmd/fae/API race tests; dogfood was rerun with the rebuilt binary. The record retains both native-start and final hashes.

Native tests cover schedule configuration bounds, explicit and restricted grants, expiry/removal, background execution, pause/restart/resume, bounded catch-up, interrupted claims, busy controls and drain/maintenance. Archive tests cover canonical/foreign/pending/live/staged/symlink/permission/extra-data/inode refusals and real Dolt/Git cleanup with persisted receipt, partial removal, retry and unchanged canonical recovery.

Dogfood creates backups while the policy is paused, restarts, confirms the durable paused state, then resumes and observes actual deletion plus survivor/pin preservation. It checks the final host ledger for holders/waiters and release coverage. After scratch cleanup it creates an exact duplicate, proves extra data is refused, removes the duplicate with its preview digest, repeats removal, and revalidates the original scratch cleanup and every canonical hash.

[Record](verification/results/retention-automation.json), [native suite](verification/results/retention-automation-suite.log), [formal suite](verification/results/retention-automation-formal/verify.log), and [real-provider result](verification/results/retention-automation-dogfood-20261020/result.json) retain hashes and explicit limitations.

## Automatic backup schedule verification

1,337 race-enabled test cases across 26 packages passed, with 73.7% statement coverage, vet and scratch census. Gobra verified 54 production predicates and rejected 54 mutations; Gomela/SPIN passed 55 positive models and observed 53 firing negative controls.

Native tests cover verified scheduled creation, default/boundary controls, pause/restart/resume, bounded catch-up, interrupted claims, allocated partial failure, admission waits with grant expiry and no directory creation. Replication tests cover oldest-first selection, pinned claims, missing/corrupt proof, selected-before-outbox recovery, lost acknowledgment/restart, destination-change refusal, manifest replacement, drain/maintenance, and expired admission-time authority with no network effect.

The real-provider harness persists paused schedules across restart, resumes creation and verifies every manifest hash, then performs scheduled uploads against the disposable replica service. The fixture stores each PUT and returns 503. Pending state survives pause/restart; reconfiguration is refused until resolution; later runs query rather than reupload. The created backup is ultimately confirmed and every uploaded digest is unique. Final host holders/waiters and pane reservations are zero; canonical scratch evidence survives duplicate cleanup.

[Record](verification/results/backup-schedules.json), [native suite](verification/results/backup-schedules-suite.log), [formal suite](verification/results/backup-schedules-formal/verify.log), and [real-provider result](verification/results/backup-schedules-dogfood-20261021/result.json) include final frozen source/evidence hashes.


## Replication unpin and storage quota verification

1,352 race-enabled test cases across 26 packages passed, with 73.6% statement coverage, vet and scratch census. Gobra verified 56 production predicates and rejected 56 mutations; Gomela/SPIN passed 57 positive models and observed 55 firing negative controls. Native coverage includes multiple replica endpoints, lost acknowledgments, corrupted remote proof, pending schedules, stale previews, restart, restricted authority, absent/substituted local sources, count limits, export overruns, partial/orphan usage, and bounded spool cleanup.

Dogfood adds `--backup-storage` (implies backup schedules and retention automation) to the established full command. It exercises real-provider turns and mid-run restart, then unpin/quota operations against disposable local replica services. [Source hashes and evidence](verification/results/backup-storage.json).
