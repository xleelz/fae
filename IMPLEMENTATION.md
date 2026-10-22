# Implementation status

This is a partial implementation. A package, schema table, unit test, or model is
not sufficient to close a requirement whose acceptance criterion needs a daemon
path, live adapter, real crash, or external integration. No full delivery phase is
claimed complete here.

| Specification phase | Implemented foundation | Still needed for phase acceptance |
|---|---|---|
| 0 Foundations | Pure types, canonical hashes, kernel transition graph, gate evaluator, validator, exhaustive/generated tests | Full check-to-firing-control census and all phase exit criteria |
| 1 Control plane | Forward v1→v6 migration, audited whole-transaction retry, chunked snapshots, immutable job bindings, CAS, idempotent admission, ordered events, messages/markers, real child-crash recovery tests, verified offline backup/restore | Complete database immutability, all outage/uncertain-commit cases and full lifecycle reconciliation |
| 2 Daemon/API | Runnable singleton, physical workspace binding, private credentialed Dolt child, authenticated Unix RPC, cached Status, bounded reads, resumable NDJSON events, graceful scripted restart, exact owned-process startup recovery and parent-death watchdogs | Physical event archive compaction, richer complete API schema, complete orphan/crash matrix and all drain bounds |
| 3 Workflow engine | Durable Prepare/Complete engine, task/token/tree/session provenance, real JSON Schema validation with external references disabled, local default scripted pipeline including RED/remediation/review/QA/protected commit/integration, revision pinning, generated/restart/mutation tests | Complete Appendix C branches and per-finding semantics, FanOut/FanIn/SubWorkflow, revision migration/diff, full parity corpus; scripted effects remain fixtures; live direct path uses measured trees and guarded real effects |
| 4 Scheduler/host | Ready-set decisions, scoped SQL role leases, host-shared durable weighted FIFO ledger, live launch admission, host telemetry, durable session-linked recovery, live Herdr census and pane/memory reservations, scoped Go command policy and nested-denial shims, dynamic shared pane/heavy/pressure/task limits, opt-in owned idle-session shedding | Complete cross-user host inventory, brokered interactive agent tools, reconciliation of unsupported/ambiguous effects and full inventory, role cap reconfiguration and full scheduler wiring |
| 5 Worktrees | Deterministic isolated creation, registration checks, declared-file/whole-tree snapshots, exact reviewed-byte commit, immutable own-hunk-preserving three-way reconcile, retryable task-tree application, bounded automatic text-conflict remediation, current-tree gate revalidation, atomic dedicated-ref integration with ancestry/patch/blob/empty containment | Unsupported conflict classes and uncertain-effect adoption, full integration queue/dependency acceptance and stuck detector |
| 6 Runtime | Scripted, direct and private-Herdr trusted-bridge adapters; strict fresh handoffs, bounded diagnostics, durable gated process ownership; restricted real Codex bridge, durable provider health and ordered route failover, bounded structured session-log reader, positive-liveness stall detection, durable proved-safe retry, bounded nonexecution startup recovery persistent provider context, resident Herdr workers and audited provider-neutral loops | General CLI log discovery and broader authenticated-provider conformance |
| 7 Recovery | Durable completed-stage preservation, same-token scripted restart, planned-stop counters, uncertain non-replayable parks, explicit durable drain-and-restart, bounded Dolt-child restart, owned-group parent-SIGKILL tests, exact-identity startup recovery, retirement receipts and audited uncertain-turn resolution | Full daemon-SIGKILL matrix, stopped-launch and escaped/ambiguous-process limits, operator resolution of unsupported retained leases and effect-producing uncertain results |
| 8 Operator | Foreground scripted/direct/multiplexer serve with pinned live target configuration, status, board, history, provider-health/provider-log, runtime-config, events/follow, message, workflow installation, local admission and shutdown via RPC; usage/refusal/unreachable codes | Remaining lifecycle verbs, complete Prophet sessions, target configs, full status surface |
| 9 Controllers | Authorized native launcher, guarded roll/rollback, maintenance stop/start, watchdog grants and independent drive rearm, gated isolated deployment builds and protected overlap holds, reviewed census, archived retired cleanup and marked scratch cleanup with external backups | Landing, salvage, admission-gate CLI and broader changed-build crash coverage |
| 10 Normal mode | Runtime schema-discovered configurable adapters, Normal live/dry-run grants and pins, durable effect recovery, draft/revision/publish/merge lifecycle with bounded polling/remediation, ordered audited ticket outbox, immutable dynamic adapter registry, explicit owned review-workspace preparation and native restart fixtures | Native vendor transport/authentication and authenticated third-party review/ticket conformance; adapters currently normalize vendor protocols |
| 11 UI/observability | Unary transport foundation | UI supervision, metrics, alerts, correlation, read models and stream |
| 12 Dogfood | Two genuine Codex tasks through resident Herdr workers, five same-context loop prompts, provider-turn SIGKILL with audited resolution, drained mid-run restart, immutable checkpoint chains, real RED/GREEN and landed contributions; integration-race revalidation exercised | Broader sustained workloads, other provider/OS combinations and broader crash/effect recovery coverage |

## Evidence limits

- Offline simulation and the daemon scripted adapter consume declared rehearsal fixtures.
  Their records do not establish real tests, repository changes or live side effects.
- Schema fields and callback validators do not enforce every semantic requirement.
  The daemon now checks recorded operator-message existence/age, pinned binding,
  engine legality and schemas. External capabilities, argument/schema pins, server-bound
  principals and transactional message-class consumption are implemented; complete
  role sessions and all lifecycle capabilities remain open.
- Pure scheduling decisions are wrapped in transactional SQL role pools and a
  host-shared flock/fsync ledger for live execution. Exact owned-process recovery and parent-death
  watchdogs are implemented, but foreign/dead-daemon holders remain conservative
  when complete child death cannot be proved. Process-group ownership is not
  containment of escaped descendants. Trusted bridge restrictions are not a
  hostile-agent sandbox; the two-task authenticated-provider acceptance passed with a planned drained
  restart. Current native tests also exercise controlled mid-turn SIGKILL and explicit
  resolution; full OS/process escape and effect-adoption matrices remain open.
- Unix RPC supplies authentication, deadlines, bounded reads and a resumable event
  stream. Reads coalesce within authenticated scopes; retained event windows use a
  durable logical floor with explicit gaps. Physical archive compaction and all latency
  bounds remain open; cached status reads avoid SQL and the daemon writer lock.
- Path validation preserves raw bytes, and containment resolves symlinks. The
  trusted-workspace assumption excludes adversarial symlink replacement races.
- Formal results apply only to the checked-in subjects and assumptions identified
  in the verification report. They do not close unimplemented requirements.

## Earlier conflict and recovery evidence

- Real temporary Git tests cover independent upstream changes, same-file distinct
  hunks, committed and uncommitted own work, binary/deletion/mode preservation,
  conflicts, precise lost-hunk paths, interrupted apply, tip races and concurrent
  single-winner integration. The daemon clean path repeats current-tree tests,
  review, Oracle and QA; supported conflicting production paths return to bounded implementation; lost original hunks and protected conflicts refuse.
- Actual Herdr and process tests cover isolated sessions, strict handoffs,
  cancellation, inherited-helper cleanup, parent SIGKILL, foreign/live/reused
  identities, and leaderless ambiguity. These are bounded execution tests rather
  than a proof of all process-tree recovery behavior.
- Gobra verifies fifteen production safety predicates with fifteen rejected
  weakened mutations. Gomela checks sixteen positive bounded models and fourteen
  firing negative controls. See [verification/README.md](verification/README.md)
  for the exact subjects and assumptions.
- `scripts/dogfood.py` completed two real Codex `gpt-6-astra` tasks through Herdr
  with a drained mid-run restart. Checkpoint chains survived; one task safely
  retried a lost integration race. Final chains contain 35 and 22 artifacts,
  versions 83 and 52. Host capacity 2 ended with 58 grants/releases and no holders.
  Full local acceptance evidence is in `verification/results/`.

## Provider health, command shims and Herdr accounting

- Provider observations are idempotent by session and atomically persist the health
  row, operator audit line and event. Ordered eligible routes are pinned before
  dispatch and recorded in provenance. All-routes-unavailable parks without task
  budget charge and periodically retries after healing. Launched uncertain work
  is not automatically replayed. Structured errors come from the one bounded
  reader of runtime-owned event logs; generic CLI announcement discovery remains
  unsupported. Operator reads use `fae provider-health` and `fae provider-log`.
- Go host commands retain literal allowlisting, scoped paths, capped parallelism
  and durable broker admission. Role processes and command descendants receive
  PATH-first refusal shims. The Codex bridge disables command tools entirely;
  arbitrary executable containment and interactive agent-to-broker command RPC
  remain outside the supported trust boundary. Rust module traversal/overrides
  and unsupported wrapper grammars refuse rather than execute unchecked.
- Herdr admission counts live panes across discovered servers/workspaces and
  adds pending reservations under a cross-process lock. Memory estimates remain
  reserved through startup and a 180-second warmup. Unreadable inventory refuses;
  exact dead owners are audited on access and surviving panes still count live.
  Uncooperative external launches can race the snapshot, and process visibility
  is limited by OS permissions; this is not a universal cross-user resource cap.

The earlier authenticated two-task dogfood evidence is historical and retains its
original source hashes. The runtime-recovery increment records a separate rerun.

## Dynamic capacity, owned idle sessions and stall recovery

- Pane limits are audited runtime configuration, survive restart, and reconcile
  conservatively across live cooperating instances. Existing reservations remain
  valid when the limit decreases. Exact dead-owner registrations no longer clamp
  capacity. Heavy capacity, host thresholds and task concurrency are now dynamic too.
- An opt-in trusted adapter can register long-lived owned sessions for daemon
  maintenance. Exact process records, active references and role classifications
  govern retirement. Three own-idle saturated rounds shed one eligible pre-code
  global; only pane-deferred tasks can escalate when nothing can be shed. Foreign
  saturation keeps waiting. Durable context survives idle TTL and restart. The
  Codex transport still retires each turn; optional provider conversation reuse
  persists separately from live idle panes.
- Runtime stalls use positive CPU/event/handoff observations. A replacement is
  permitted only after exact retirement, unchanged tree and an explicit restricted
  effects contract. Recovery counters survive durable checkpoints and restart;
  two retries precede typed exhaustion. Unknown activity or effect ownership
  remains conservative. Startup nonexecution now has its own bounded durable recovery; a complete
  crash matrix remains separate work.

Validation for this increment: 494 race-enabled test cases, vet and scratch census
passed (77.7% statement coverage). Gobra checked 15 predicates with 15 rejected
mutations; Gomela/SPIN checked 16 positive and 14 firing negative models. Fresh
authenticated two-task dogfood passed with 29 real turns, dynamic pane changes,
a drained restart and 58 released host leases. See
[the current record](verification/results/runtime-recovery.json).

## Persistent context, dynamic host limits and bounded startup

- The restricted Codex bridge optionally persists explicit provider thread IDs.
  Workflow-global contexts carry across tasks; task-bound contexts carry across
  turns of the same task and never cross tasks. Revision/role/route/profile/mode
  changes isolate context. Reviews always start fresh. A durable pending marker
  rotates uncertain context. Each provider turn retains an auditable task/dispatch
  mapping. No resident provider process or loop scheduler is claimed.
- Dynamic host updates are partial, audited and durable. Cooperating instances
  share the minimum heavy cap and conservative pressure thresholds; instance task
  concurrency updates on the next round. Existing work survives reductions. Status
  distinguishes requested from effective pane limits. Supported task limits are
  1–256; complete default-unbounded scheduler semantics remain phase work.
- Transport startup defaults to 180 seconds and three total attempts. Only proven
  nonexecution, retirement and unchanged tree permit replay. Per-stage counters
  survive restart, fresh IDs prevent duplicate launches, and no task/stage budget
  is charged. Pre-execution failures retain diagnostics without penalizing a
  provider that never ran. Launched work without acknowledgment stays uncertain.

Final validation: 530 race-enabled cases, vet and scratch census passed with
77.8% coverage; 18 Gobra predicates/mutations and 19 positive/17 negative bounded
Gomela models passed their expected checks. Real-provider acceptance completed
two tasks in 29 turns, including eight context resumptions and two resumptions
of pre-restart threads. Dynamic host limits survived restart, task-bound contexts
stayed isolated, and all 58 leases were released. Details:
[latest verification](verification/results/persistent-host-startup.json).

## Provider-neutral loops, resident Herdr workers and uncertain turns

Loop schedules run outside the gated workflow engine, with a durable checkpoint
before each prompt, persistent context, per-iteration audit, optional iteration
cap and mandatory expiry (seven days by default). Pause/resume/retire and reads
are exposed through authenticated schedule RPC/CLI. Live loops require the
explicit persistent adapter contract, use isolated pinned read-only worktrees,
and validate output and unchanged repository state. Cron is covered by the subsequent increment below.

Opt-in resident Herdr workers execute multiple distinct bridge processes in one
pane. Every turn refreshes its profile and configuration and owns separate
process groups. Heavy/role leases end after turn retirement; pane reservations
last until exact resident retirement. Reviews remain fresh. Task-bound contexts
and panes never cross tasks; global roles may reuse across tasks. TTL and eligible
idle shedding retire only owned, inactive residents. Provider conversation history
persists separately from the worker process.

A genuine crash charges a durable recovery counter once and parks uncertain
work. Explicit audited redispatch requires unchanged tree/HEAD and retired leases,
retains consumed budgets and preserves the original dispatch. Acceptance requires
an original, schema-valid read-only result; effect-producing results cannot be
adopted by supplying JSON. Escalation is also available. Recovery never treats an
absent process leader as proof that all descendants exited.

Retirement receipts remain durable until the matching lease journal acknowledges
them, including the process-retired/journal-still-launching crash window. Startup
socket cleanup requires the private resident ownership marker and matching exact
retirement receipts; a stale pathname alone does not authorize removal.

The final [authenticated acceptance run](verification/results/loops-resident-crash-dogfood-20261017-r3/result.json)
passed with two completed tasks and 34 completed provider turns, including five
loop iterations in one provider thread and one resident pane. Nineteen turns
have resident identity audits; seven panes were reused. A real provider turn was
interrupted by SIGKILL, remained parked after restart, then was explicitly
redispatched. A later drained restart preserved completed evidence and context.
All 62 host grants ended in 61 releases and one exact-owner crash recovery.
Shutdown left zero pane reservations, process records, terminal receipts and
Herdr sockets. Earlier failed runs are retained as regression evidence.

## Cron schedules, controller lifecycle and uncertain local effects

Cron admission now pins workflow revisions and deterministic occurrence identities,
records each admission before advancing its durable cursor, bounds missed-occurrence
catch-up, and shares round capacity fairly. Pause, resume, retirement and restart
preserve the cursor and creation authorization. Strict input validation and actual
Dolt restart-gap tests cover idempotence and skipped-occurrence accounting.

Controller maintenance and same-binary rolling restart use an independent bundled
runtime launcher. Native sandbox probes, pinned executable/environment checks,
exact PID birth identity, commit-window refusal, drain, post-start acceptance and
durable maintenance markers constrain lifecycle actions. The independent watchdog
suppresses maintenance death/stall alerts, persists pending alerts and enforces a
bounded rearm budget. Changed builds require clean VCS metadata and a gated ref.
Build-from-ref, critical-task indefinite overlap policy, deployed integration-tip
fast-forward and in-process recovery of a stopped drive remain follow-up work.

Local effects now retain immutable intent, start and completion checkpoints with
audit/event references. VCS commit preparation fixes the exact object before CAS;
linked process ownership proves retirement across crashes. Explicit adoption uses
daemon receipts and remeasured tree/HEAD/ref postconditions, never supplied effect
JSON. Completed CAS operations with a missing result can be reconstructed from
exact intent and retirement evidence. Host effects without completed receipts stay
uncertain; redispatch requires positive nonexecution. Descendant-tip integration
adoption and external-service effects remain conservative refusals.

This increment passed 621 race-enabled cases across 25 packages, vet and scratch
census, with 74.8% statement coverage; formal checks passed 24 production guards
and mutations plus 25 positive/23 negative bounded models. The
[authenticated dogfood](verification/results/cron-controller-effects-dogfood-20261018/result.json)
completed two scheduled tasks and five loop iterations in 34 provider turns.
Cron admitted each occurrence exactly once. A real turn crash required explicit
redispatch; the planned controller restart changed daemon PID, preserved limits
and evidence, and cleared maintenance only after acceptance. All 66 host leases
were released or exactly recovered, and shutdown left no owned resources.
See the [complete verification record](verification/results/cron-controller-effects.json),
including the retained initial test-boundary failure and clean full rerun.

## Deployment policies, independent drive and external-effect recovery

Deployment can build a pinned gated source revision in an isolated clean clone,
retain the executable in instance deployment storage, and fast-forward the
integration ref only after ancestry checks and compare-and-swap. Changed builds
must have clean VCS metadata and source history in the bound workspace.
`--when-clear` waits for QA and commit windows before resolving the gated tip;
a gate that moves during compilation causes a safe refusal and requires retry.
Durable overlap holds bind task, executable hash and gated ref. Ordinary holds
expire at the configured bound (two hours by default); critical tasks and
highest-priority tasks near their attempt limit require explicit acknowledgment.
Preflight failures retain an alert and failure marker without draining the daemon.
Acceptance failures retain the launched process identity for exact rollback.
The startup executable hash is pinned; replacing its pathname makes deployment
refuse rather than misidentify the new file as the running build.

The drive now has serialized, independently restartable generations. Stopping it
retains workers and pending completions while RPC, the daemon and Dolt remain
alive. Rearming checks cached versus durable checkpoints and preserves dispatch
identity. Failed generations can rearm within a durable three-per-cause hourly
budget. Lost save acknowledgments reconcile only the exact next version and
canonical payload. Task and loop proposals survive failed acknowledgments and
failed reconciliation reads; cache advancement and worker retirement follow
durable confirmation. Ambiguous saves block drain and deployment until resolved.
Watchdog registration grants ongoing alert/rearm authority;
it survives message aging and daemon restart and can be explicitly revoked.
Other operator mutations require a valid operator message within the configured
maximum age (24 hours by default).

Normal-mode effects now use explicitly installed, pinned daemon HTTP adapters.
Dry-run stages produce intent without HTTP calls and end in `DryRunComplete`.
Live execution queries before creation, persists external IDs before validation,
and disables transport-level mutation replay. Recovery queries the pinned
service and adopts only a matching completed receipt after checking local tree
and HEAD. Absence does not authorize retry of a possibly executed mutation.
Review identity is task/branch based and tolerates differing commits with the
same tree. The [adapter protocol](external/README.md) documents the boundary:
vendor schema discovery, review workspace/build/publish/merge lifecycle, and a
complete per-room ticket outbox scheduler remain follow-up work.

Final validation passed **681 race-enabled cases across 26 packages**, vet and
strict scratch census, with **75.6% statement coverage**. Gobra passed 27
production guards and rejected 27 mutations; Gomela/SPIN passed 28 positive and
26 firing negative models. The [complete verification record](verification/results/deployment-drive-external.json)
retains the earlier cleanup failure and harness assertion failure as well as the
clean reruns.

The [final authenticated run](verification/results/deployment-drive-external-dogfood-20261018-r2/result.json)
completed both cron-admitted tasks, an actual provider-turn SIGKILL with explicit
recovery, independent watchdog drive rearm with unchanged daemon/Dolt identities,
and a guarded same-binary roll. The retained started turn executed and completed
once; another dispatch had explicit nonexecution/deferral proof. Five loop turns
reused one thread and pane. Across 34 completed provider turns, seven resident
panes were reused. All 63 host grants ended in 62 releases and one exact recovery;
shutdown left no owned reservations, process records, retirement receipts or
Herdr sockets. Final source, binary, formal-input and evidence hashes were checked.

## Adapter schemas, review lifecycle and ticket outbox

The next increment implements these capabilities through explicitly installed
HTTP adapters. Startup discovers each alias's exact server/tool descriptor,
checks pinned property names and types, and installs its schema. Typed arguments
and input-schema identity are carried in durable intents. Only configured
review tokens and ticket source bindings defer value checks until substitution;
every concrete call must validate before network I/O. Room workflow actions are
read at runtime and cached with a bounded refresh. Neither schema failures nor
expired cache refresh failures fall back to invented vendor arguments.

Review policies bind a task to a pre-provisioned package workspace, branch and
integration tip. An approved read-only fleet-baseline check reports target
configuration defects before review creation. Creation is draft-only; status
reads check the build, each analyzer's own declared publish/merge requirements,
and explicit automated-comment severity. Publish and merge have separate
intents, calls and receipts. Configured instance/task opt-outs remove their
stages before workflow hashing. When merge is enabled, successful merge is a
kernel floor for Done. Remediation routes through a declared bounded workflow
loop into an implementing role, then creates another revision on the same review.
The default limit is five revisions. Provisioning or synchronizing package
workspaces remains an operator/adapter responsibility: Fae checks the physical
binding and reviewed tree and refuses drift rather than constructing a workspace
in the drive.

Ticket mirroring reconstructs desired transitions from persisted task history
and stores durable per-row attempts, errors, intent, started and emitted state.
The configured room action supplies vendor keys/values; daemon bindings supply
admitted task, room, destination, push namespace and previously recorded ticket
identity. Immutable installed pins cannot be redirected by room resources.
Rows execute in task order and use task/outbox idempotency keys. After a possibly
performed mutation, restart permits only receipt queries until its outcome is
proven; an absent response does not permit another POST. Managed workflow ticket
stages share this outbox instead of creating a second mutation path. Status and
maintenance account for pending, failed and uncertain emissions.

This is a configurable adapter implementation, not a bundled native MCP
transport, vendor SDK, vendor authentication system or automatic workspace
provisioner. Adapters must obtain actual installed tool schemas and preserve
vendor status/comment semantics in the documented normalized read contract.
See [the adapter protocol](external/README.md) and [schema contract](external/SCHEMAS.md).
Final validation passed **770 race-enabled cases across 26 packages**, vet and
strict scratch census, with **74.8% statement coverage**. Gobra verified 30
production guards and rejected 30 mutations; Gomela/SPIN passed 31 positive and
29 firing negative models. The complete daemon workflow performed two
implementation and verification cycles, then submitted revision 2 on the same
review before separately publishing and merging. Durable vendor polling has
its own bounded budget and does not spend implementation attempts. A changed
tree invalidates earlier merge evidence.

The authenticated two-task dogfood passed with 34 completed provider turns,
five loop iterations, seven reused resident Herdr panes, mid-turn SIGKILL and
explicit recovery, independent drive rearm, and guarded daemon roll. Both tasks
reached Done. All 63 host grants ended in 62 releases and one recovery, with no
owned resources remaining after shutdown. Source, binary, formal-input and
evidence hashes match the final files. See the
[complete verification record](verification/results/vendor-review-ticket.json),
which retains initial test failures and the clean rerun. Earlier result sections
remain historical evidence for their respective source snapshots.

## Operator authority, task controls and read streams

Server-bound credentials now distinguish operator, controller, Prophet and role
principals. Caller parameters cannot select authority. Operator citations consume
action classes transactionally; explicit multi-class grants support supervised
workflows. See [authority configuration](daemon/AUTHORITY.md).

Ten task controls persist state, context, priority and attempt-limit changes with
audit events. In-flight holds preserve completion evidence and leases; context
updates affect subsequent dispatches. Lost checkpoint acknowledgements reconcile
exact durable proposals before further execution. Live restart/resume refuses
workspace drift, including an unadopted post-commit HEAD; broad post-commit restart
requires separate execution-head reconciliation. Complete backlog dependency
orchestration and Prophet sessions remain open.

Bounded reads coalesce within principal scopes and preserve independent caller
deadlines. Cached schedule/controller snapshots avoid the writer lock. Retention
uses an audited monotonic logical floor; slow and expired subscribers receive
explicit resume gaps. Physical archive rows remain intact. See
[read and event contracts](daemon/READS.md).

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

## Prophet sessions, backlog dependencies and post-commit restart

This increment adds asynchronous per-task/global Prophet chat with durable pages,
explicit verb and target authorization, bounded context, and normal host/pane
admission without workflow role leases. A private fresh provider context prevents
repository-file and cross-task session leakage. See [Prophet sessions](daemon/PROPHET.md).

Backlog admission validates and commits a complete graph, its task bindings and
edges atomically. Blocking edges hold Pending tasks until prerequisites are Done;
relates edges remain metadata. Upserts preserve pinned execution identities,
state transitions mirror backlog rows, and startup/on-demand reconciliation
requires task-owned landing evidence. Uncertain batch outcomes pause task mutation
until authoritative readback. See [backlog orchestration](daemon/BACKLOG.md).

Live tasks now retain a broker-proved execution HEAD separately from their
reconciliation base. Explicit restart/resume checks exact HEAD/tree provenance,
preserves committed own changes across changed-base reconciliation, and can
upgrade legacy snapshots only from validated commit receipts. New execution still
passes ordinary RED, test, review and commit gates. See [post-commit restart](daemon/POSTCOMMIT.md).

Native verification initially stalled because the automatic approval-review
service returned a model-capacity error before executing commands. The completed
verification below supersedes that blocker; earlier phase results above apply
only to their archived source snapshots.

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

## Backup retention and remote replication phase

The user-requested backup lifecycle extension is implemented around the existing FAE-CP-013 restore-tested plane backups. `backup retention` previews a digest binding content, exact manifest and filesystem identity, pins historical snapshot coverage and replication sources, and rechecks before removal. `backup replicate` persists source protection and attempted intent before a bounded deterministic archive upload. Uncertain recovery only queries; receipt identity and a complete content round-trip must match before confirmation. Operator runtime authorization and weighted host admission apply outside drive/provider workflows.

1,293 race-enabled test cases across 26 packages passed, with 73.5% statement coverage, vet and scratch census. Gobra verified 50 production predicates and rejected 50 mutations; Gomela/SPIN passed 51 positive models and observed 49 firing negative controls. Real-provider acceptance completed 36 turns with the existing crash/restart, Herdr, Prophet, target, retirement and fleet checks, plus one remote upload whose lost acknowledgment was recovered after daemon restart. Remote restore/fsck, stale preview refusal after replication pinning, local pruning and scratch cleanup passed. Evidence: [backup-lifecycle.json](verification/results/backup-lifecycle.json).

This does not implement automatic retention schedules, scratch-archive pruning, remote storage retention, vendor-specific credentials, unpinning, or force recovery of uncertain uploads/partial removals. Conservative historical and replication pins may exceed the requested keep count. Remaining requirements elsewhere in this document remain open.

## Automatic retention and safe scratch-archive cleanup phase

The user-requested backup lifecycle continuation is implemented. A singleton interval policy is durable, operator-controlled, and explicitly authorized for recurring retention. Runs stay outside the drive, respect drain/maintenance and weighted host admission, claim one occurrence before deletion, and compute a fresh preview. Restart blocks interrupted claims until explicit resume. Authority is rechecked each run and before apply; normal expiry remains effective.

Offline scratch-archive cleanup accepts only a reviewed exact redundant sibling of the current completed journal's canonical archive. It validates private ownership, inode identities, all file hashes and Git archive refs, writes a durable removal receipt, and supports partial deletion retry without changing the canonical journal or backup. Active/recreated instances, staged cleanup, unique data and changed evidence refuse removal.

1,319 race-enabled test cases across 26 packages passed, with 73.6% statement coverage, vet and scratch census. Gobra verified 52 production predicates and rejected 52 mutations; Gomela/SPIN passed 53 positive models and observed 51 firing negative controls. Dogfood completed 36 turns with the previous provider/Herdr/crash/replication checks, persisted a paused retention policy across restart, resumed actual automatic pruning while retaining pinned/newest backups, then removed a duplicate scratch archive. Repeated original scratch cleanup remained valid. Final host holders and panes were zero. [Evidence](verification/results/retention-automation.json).

Remaining scope: automatic backup creation/replication, remote retention, calendar-based or multiple retention policies, replication unpinning, cleanup of unique/older/incomplete/orphan archives, and broader requirements elsewhere remain open. The canonical recovery copy is intentionally never pruned.

## Automatic backup creation and replication phase

The user-requested automatic backup lifecycle continuation is implemented as two independent durable interval policies. Creation claims before export, verifies Dolt restore/fsck and hashes, and blocks interrupted/partial attempts. Replication selects oldest-first under the backup lifecycle mutex, durably pins source and exact destination/size/manifest before transfer, and preserves pending work across pause/restart. Existing upload outboxes provide query-only uncertain recovery; matching receipt and full remote content are required before advancing.

Both policies require explicit structured recurring authority, recheck it after host admission, remain outside drive/provider workflow execution, and respect drain/maintenance/shutdown. Defaults are one hour, with ordinary grant expiry retained. Partial creation and uncertain replication remain visible in status and never force replay or delete evidence.

1,337 race-enabled test cases across 26 packages passed, with 73.7% statement coverage, vet and scratch census. Gobra verified 54 production predicates and rejected 54 mutations; Gomela/SPIN passed 55 positive models and observed 53 firing negative controls. Final real-provider acceptance completed 36 turns with all earlier provider/Herdr/crash/fleet/retention/archive checks, plus actual scheduled creation, 2 scheduled uploads with deliberately failed acknowledgments, restart with a pending claim, destination-change refusal, and eventual replication of the automatically created backup with no duplicate PUT. [Evidence](verification/results/backup-schedules.json).

Remaining scope includes calendar/multiple policies, automatic grant renewal, storage quotas, replication unpinning, remote retention/credentials, and cleanup of unique incomplete or orphan archives. Other remaining requirements elsewhere in this document remain open.


## Safe replication unpinning and backup storage quotas phase

All recorded replicas must provide matching receipts and full remote content before a reviewed source pin can be released. Pending or invalid outboxes and scheduled claims fail closed. Selection, retention and unpinning serialize; apply revalidates authority and source identity. Removing a pin never removes data or overrides historical retention coverage. Manual replica reconfirmation verifies the local source before repinning.

Durable byte/count policies account actual owned backup and spool files, including incomplete exports and orphan spools. Creation refuses before allocation at limits, checks after export/verification, and preserves overrun evidence. Tar writes are bounded by remaining allowance. Unknown export size makes this admission/checkpoint enforcement, not a hard filesystem quota. External scratch archives are outside quota scope.

1,352 race-enabled test cases across 26 packages passed, with 73.6% statement coverage, vet and scratch census. Gobra verified 56 production predicates and rejected 56 mutations; Gomela/SPIN passed 57 positive models and observed 55 firing negative controls. Real-provider acceptance completed 36 turns, exercised pending-claim refusal, successful unpin without upload/deletion, stale retention preview refusal, quota persistence across restart, byte/count refusal without allocation, and successful verified backup after raising quota. All prior provider/Herdr/restart/schedule/cleanup checks remain enabled. [Evidence](verification/results/backup-storage.json).

Remaining scope includes remote retention/credentials, calendar/multiple policies, automatic grant renewal, hard filesystem quotas, unique incomplete/orphan archive cleanup, and other requirements described elsewhere.
