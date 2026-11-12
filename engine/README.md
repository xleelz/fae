# Durable interpreter contract

`New` validates and pins an immutable workflow hash and revision. `Prepare` makes
one decision and returns a snapshot and events. Persist those atomically before
launching the returned dispatch. Execute outside the drive loop. `Complete`
accepts only the persisted dispatch token, assigned sessions, task, tree and a
production timestamp no earlier than dispatch; validate its returned snapshot and
persist it and events before preparing again. Completed stages are not reissued.

Artifacts are content-addressed canonical JSON, checked using JSON Schema before
recording, and carry a separate provenance record with the immutable binding,
producer, assigned session, declared input IDs, tree and timestamp. A snapshot is
trusted storage, not an externally signed capability. `Validate` detects corrupt
binding hashes, artifact contents/types/provenance and in-flight configuration,
identity and counter mismatches; it is not an authentication mechanism for JSON
supplied by an untrusted client.

Attempts mean one initial execution attempt plus each repeated execution stage.
First visits along a successful pipeline do not consume additional attempts.
`Sequence` independently counts all dispatches. Loops, test runs, same-gate
repeats (`same-gate:<stage>:<reason digest>`, bound 3, restarted by a success or
a new reason; every refusal before launch at a stage shares the reason
`refused-before-launch`), agent-start retries
(`startup-recovery:<stage>:<provider>/<model>`, `AgentStartCountKey`: 3 failed
starts per binding per turn, FAE-RT-004), the turn's starts over every binding
(`agent-starts:<stage>`: 3 per binding the stage's roles declare,
`StageAgentStartBound`, which no exhaustion, refusal, failover or healing
re-arms), a review turn's later-session deferrals after earlier reviewers ran (`admission-deferred:<stage>`,
diagnostic only; never a failure budget), failover rotations (`failover:<stage>`, 1 per turn,
FAE-RT-010), stall retries (`stall-recovery`, 2 per stalled turn, FAE-RT-005)
and dispatch/crash recoveries have separate durable counters. The
per-turn counters are durable within a turn and cleared by every end of it
(`clearTurnBounds`): a completion, a recorded failure (a malformed or invalid
report, an execution or provider failure), leaving the stage by any edge
(`enter`: an escalation of any kind, StallExhausted and AttemptCap included, a
block, a restart, a resume or a migration remap), a held dispatch's collection
(`Complete`), or an operator redispatch of an UncertainDispatch park
(`ResolveUncertain`, which keeps the crash-recovery count). So the next turn,
at the same stage or another, gets 3 agent-start attempts per binding, 1
failover rotation and 2 stall retries. Within a turn, each binding counts its
own failed starts, so a review dispatch whose reviewers run on different
bindings charges a binding Unavailable only after 3 failed starts of its own:
its third parks ProviderUnavailable naming it, and the next route starts from
its own count. The turn's failover rotation stays spent. A provider failover
and a binding refusal clear the stage's start counts, so the route the turn
moves to gets its full 3.

**What never ran is never charged (FAE-RT-004, FAE-WF-074, operator ruling
2026-10-29).** A dispatch whose failing session never ran is refunded: its
attempt, stage visit and test run go back (`refund`), whatever ran before it.
That covers a start failure (`startup-retry`, or
`ClassLaterReviewerNotStarted` for a review session after earlier ones ran), a
binding refusal (`binding-refused`), a host-admission deferral
(`infrastructure-deferred`, a later review session's included), a stale-residue
refusal and a refusal before launch (`ClassRefusedBeforeLaunch`: the bridge's
`refused-before-provider`, the daemon's scope refusal, an unresolvable task,
binding or tree, unreadable feedback, a role with no route or brief, and a
host command, freeze, compare, merge or commit refused before it started).
When earlier review sessions of the dispatch ran, their reviews are not
recorded (a partial collection never stands for the stage), the event names
them, and the next dispatch runs every reviewer again in fresh sessions, so no
reviewer sees another's output (FAE-WF-033). Bounds keep each path finite,
whatever heals in between: the per-binding start bound (3, after which the
park is re-decided once the binding is eligible again) inside the turn's
total over every binding (`StageAgentStartBound`: 3 per declared binding, so
reaching that total escalates ProviderUnavailable naming the bindings, however
often another task's ok healed one); one refusal per declared binding per
route generation; the route-exhaustion clock, which these results keep
running. Host admission deferrals, including later review sessions after earlier
reviewers ran, remain refunded parks re-decided when capacity returns, without
a failure bound (FAE-HOST-006). The earlier reviews are not carried across
batches, preserving fresh independent sessions. For a refusal before launch, which nothing in
the task changes, SameGate: the fourth consecutive refusal before launch at a
stage escalates SameGate whatever its detail, and a failure for another
reason in between, which spends an attempt, restarts the count. A commit
refused before any object or ref was written (the no-red classifier, the
missing proof of the always-required checks, `commit-refused-before-write`
for a reviewed snapshot or prepared commit the broker could not produce)
escalates for the operator, refunded. A provider failover after earlier
review sessions ran stays charged: the failing session's provider ran.

A `binding-refused` result (`ClassBindingRefused`: the trusted bridge refused
the session's binding before its provider ran, because this instance cannot
run it as configured) records the binding in `RefusedRoutes` and releases the
turn at its stage with no failure and no failover rotation spent; the next
route gets the turn's full agent-start bound. The refused session never ran, so the
dispatch's attempt and visit are refunded, also when earlier review sessions
of the dispatch ran first: the `route-refused` event names the sessions that
ran, their reviews are not recorded, and the stage runs every reviewer again.
A `provider-failover` result after earlier review sessions ran is charged
instead (that provider ran); it still spends the turn's rotation. Route selection
skips a refused binding at every stage of the task while
`Options.RouteGeneration` is the one it was refused in; the daemon starts a new
generation each time it starts (the only way its bridge command or target
environment changes), and Resume or restart with information clears the list.
Each refusal removes one declared binding, so a turn moves past at most as many
refused bindings as the role declares. When every route of a role is refused,
the task parks like any task with no eligible route (FAE-RT-010): its reason is
`all-routes-unavailable: every route for role R was refused by the bridge
before its provider ran: ...`, naming each refusal and what the operator can
change (`RoutesUnavailableReason` recognises both forms). The park charges no
attempt and is decided again each Prepare, without an event or a dispatch
while the generation is unchanged. Its reason is cut on a character boundary
and is valid UTF-8 (`bounded`), so the stored reason reads back exactly as the
engine recomputes it, and the comparison ignores the store's U+FFFD
replacement; a reason that differed would re-park with an event every round.
A new route generation resumes it in place, even for a caller without a route
selector, and so does Resume, which accepts a task parked for want of a route
when the bridge refused one of the stage's routes in the caller's route
generation (`Control.RouteGeneration`; it forgets the refusals and restarts the
route-exhaustion bound). Resume refuses a park on unhealthy routes alone,
including one whose only refusals come from an earlier generation: they no
longer apply. Refused and unhealthy routes together park plain
`all-routes-unavailable` as before. The route-exhaustion clock
(`RoutesUnavailableSince`) starts at the first no-route park and runs until a
dispatch's provider runs: `Complete` clears it for every result except one
whose failing session provably launched nothing (a binding refusal, a start
failure, a no-launch host deferral, a refusal before launch, a stale-residue
refusal), whether or not earlier review sessions of the dispatch ran. So a retry in a
new route generation that the bridge refuses again keeps the clock, and a
refusal that recurs at every daemon start escalates ProviderUnavailable once
the bound has passed since the first park, however often the daemon restarts.
The escalation keeps the clock, which dates the episode for the daemon's
route-exhaustion alert; Resume and restart with information clear it. StallExhausted's
detail names the stalls counted. At one attempt boundary the test-run cap is
checked before the attempt limit, so a task out of both escalates TestRunCap. An
operator Resume of an AttemptCap escalation after a raise re-grants attempts
and re-enters the first stage (FAE-WF-021, FAE-WF-074); like restart with
information it refunds nothing. Loop counts and progress streaks, test runs,
request and failure counters, visits, evidence and approved scope carry over
for the task's life, and the task-control cause records the carried values
(`carriedCounters`); the agent-start, failover and stall counters are per-turn
bounds, not among these budgets, so clearing them refunds nothing.
`CarriedBudgets` reports each budget's count used and limit for the daemon's
Resume and restart replies. A pass from
the first stage draws on the same budgets, so a raised task without enough
left escalates TestRunCap or LoopCap, whose detail names the count used, the
limit, and that the count carries for the life of the task.
`ApplyControl` refuses a control whose result fails `Validate`. Planned safe
scripted replay keeps the exact dispatch/session identities and charges no budget.
`ReplayInterrupted` refunds a live turn a recorded graceful stop interrupted; the
daemon calls it only after proving the workspace and session retirement.
Unknown executions park with uncertainty; they are never silently redispatched.
Every record stages an explicit attempt: the charged count, or attempt 1 before
the first dispatch charges one.

## Escalation kinds

Every escalation carries exactly one `domain.EscalationKind` (the closed
Appendix B set) plus `EscalationDetail`; `Reason` is `Kind[: detail]`, and the
escalating transition event carries `escalation_kind`. `Escalated(kind)` also
accepts legacy untyped reasons. Escalate stages declare `kind` (omitted means
HumanDecisionRequired). Mapping of engine causes: loop exhaustion is LoopCap
naming the loop, or IntegrationConflict for an Integrate stage's remediation
loop; a gate, or a stage's execution (malformed output, validation, execution
or provider failure), failing for the same reason more than 3 consecutive times
is SameGate (the attempt limit bounds failures whose reason keeps changing);
review lifecycle caps and receipts are ReviewGateFailed; unknown Branch arms are
HumanDecisionRequired (`UnknownDecision:` detail); stale protected evidence
re-validates through the stage's declared re-verification route, and is
ResumeEvidence only when the stage has none (FAE-ROB-013); a crash-interrupted
engine run re-runs at recovery (`RerunInterrupted`), charging dispatch recovery; an external effect outside a live Normal disposition is
ExternalEffectRefused; an operator escalation (including of an uncertain
dispatch) is OperatorEscalate, or DispatchRecoveryExhausted once crash recoveries
are spent; a no-eligible-route park past `Options.RouteExhaustion` (default 30
min) is ProviderUnavailable, whether its routes were unhealthy or refused by the
bridge (`binding-refused`), and its detail names the refusals; live typed refusals map to StaleBranchResidue
(`stale-residue`, `rejected-change`) and MergeOwnHunkLoss (`own-hunk-loss`).
`Fault` escalates a valid task whose stage cannot be decided. CoordinatorVeto
is in the set but has no mechanism yet. NoRedViolation is raised
by a typed commit refusal (`no-red-violation`) from the daemon's target floor;
`required-check-failed` escalates HumanDecisionRequired. Governed requests
(`requests.go`, FAE-WF-078) escalate ScopeWidenRejected for a rejected or
cap-exhausted scope widen. A widen naming a path inside `.git` or a protected
path (`workflow.ProtectedPath`, FAE-SEC-005) is malformed output and never
reaches the judge; a stored approved path either rule now refuses is dropped
before the next dispatch with a `scope-dropped` event naming it and why.

Policy-aware job state: a templated HostCommand's bound vector (`Dispatch.Argv`,
re-derived on validation); the pinned task class whose overlays select route
effort and briefs; the pending request, per-kind request counters and approved
scope (carried on every dispatch as `Dispatch.Scope`); progress-aware Loop
measures (FAE-WF-075); and carried-forward pre-code evidence whose provenance
names its source task (`CarryForward`, FAE-WF-086). A carried task stays Pending
with its `CarryEntry` until the release enters that stage; any other edge out
of Pending (an operator Escalate or Cancel, `Fault`) drops the entry in `enter`,
and `Validate` refuses an entry outside Pending. `RedPhase` marks a
declared-shape run that establishes a (replacement) RED.

The default content covers the local scripted pipeline: intake, bounded spec
review (two isolated sessions), planning, RED/right-failure/freeze, implementation,
GREEN/frozen comparison, formatting, four isolated review dimensions, typed oracle
routing, QA, reconcile routing, post-merge QA, commit and integration. Every gate
records its evaluated leaves and evidence IDs. Review input bindings exclude
sibling results. The CodeBearing graph check rejects paths missing the RED chain
or a passing unchanged-frozen gate. The shipped default (`defaults.Shipped()`)
adds the Normal-only code-review sub-workflow after integration; Local tasks pin
`workflow.LocalProjection`, which removes it.

## Primitive library (FAE-WF-030)

Every primitive of the FAE-WF-030 table is implemented (configuration in
`workflow/GRAMMAR.md`):

- **HumanDecision** (`decision.go`): parks with reason `HumanDecisionRequired`
  (raised on the park event) and stays waiting until `Decide` records the
  operator's option. The decision artifact cites the authorizing message and
  has operator provenance; the caller writes the returned `DecisionRecord` as a
  `decisions` row in the same transaction.
- **Review aggregation**, **HostCommand expected shape**, **Loop exit gate,
  backoff and exhaustion kind** and **Escalate reason templates** (`records.go`,
  `engine.go`).
- **FanOut/FanIn** (`fanout.go`): durable per-visit branch state. Branches run
  one after another because a task keeps one dispatch in flight. A running
  branch never sees sibling evidence. The join settles all/any/k-of-n, skips
  unrun branches and routes.
- **SubWorkflow** (`subworkflow.go`): the parent parks with a pinned
  `ChildRef`. The daemon admits the child as its own task; `BindChild` binds a
  finished child's mapped artifacts back, `ChildFailed` takes the failure route
  or escalates, and `Obsolete` identifies children to retire.

Every Gate, Branch arm, Review aggregate, shape check and branch verdict is a
content-addressed `workflow.GateRecord`; `ReplayGates` re-evaluates them over the
recorded evidence (FAE-WF-063), and `Validate` checks their addresses.

## Deliberate limits

The `Scripted` executor is a deterministic rehearsal adapter. Every rehearsal job and
artifact provenance record persists `execution_kind: "scripted"`; missing or
relabeled markers fail validation. Its test, commit,
and integration outputs are explicitly fixture evidence, not claims about an
actual repository. Freeze/Compare calculate real Go AST identities over supplied
fixture source. The explicit `NewLive` path accepts only live executor completions. Trusted
change turns advance the measured tree; old approvals cannot satisfy current-tree
gates. Informational artifacts and frozen identities survive a tree change.
The daemon now supplies actual tree acquisition, brokered commands, declared-file
and RED restrictions, guarded Git effects, pinned target configuration and durable
leases. `execution` launches configured trusted bridge processes. These operations
remain outside this pure package. Live recovery parks uncertain dispatches unless
the caller proves a planned graceful interruption (`ReplayInterrupted`). External-effect stages
fail closed (`ExternalEffectRefused`) outside a live Normal disposition. Operator
resolution of uncertainty and audited attempt-limit changes are daemon operator controls.
Live dispatch now supports durable provider/model route selection and changed-base
revalidation. Provider health affects new dispatches only; no eligible route parks
without consuming an attempt, bounded by the route-exhaustion bound. Startup
exhaustion parks `ProviderUnavailable` (the daemon records it against the
binding) and rotation takes the next route. A review dispatch whose later
session provably never started after earlier reviewers ran
(`ClassLaterReviewerNotStarted`) counts toward that session's binding's start
bound and, like a later reviewer's refusal, charges nothing, records no review
and runs every reviewer again; its exhaustion names that binding (FAE-RT-004,
FAE-WF-033). Typed no-launch infrastructure deferrals, a later review
session's included, also park and release their attempt charge. A live report that fails
its schema or change policy on an untouched tree retries within the stage bound;
one that left unrecorded edits escalates StaleBranchResidue. A change turn whose
only fault is a malformed request (FAE-WF-078) records its checked edits as the
task's tree before the retry, so it never leaves unrecorded edits. Gate results are
content-addressed records on the job (`GateRecords`), not yet rows of the plane's
artifacts table. Recovery tests prove persistence
round trips and explicit safe rehearsal replay, not arbitrary real-effect replay.

Tests include 10,000 generated graph executions with mid-stage and stage-boundary
serialization/recovery (`TestGeneratedDurableWorkflowParity10000` covers short
generated AgentTurn chains plus a gate), the FAE-WF-094 parity corpus
(`TestDefaultWorkflowParityCorpus` replays 42 recorded default-workflow runs in
`testdata/parity`; re-record intended changes with `-update-parity` and review the
diff), malformed-output bounds, wrong-token
and wrong-tree refusals, missing-input parking, retry routing, RED identity
mutation, zero tests, QA/review failures, collapsed review dimensions, negative
integration and artifact provenance mutation. Gobra's checked completion guard is
called by `Complete`; the proof scope is documented under `verification/`.
