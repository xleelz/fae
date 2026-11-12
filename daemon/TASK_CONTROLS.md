# Durable task controls

Each control has an authenticated, message-authorized RPC and a thin CLI. The
operator audit, immutable job snapshot and events commit in one SQL transaction.
The daemon publishes its cache only after commit (or exact next-version readback).
An uncertain acknowledgement and failed read retain the proposal, preventing
subsequent dispatch/collection until it reconciles. No control is silently replayed.

| CLI | RPC | Behavior |
| --- | --- | --- |
| `escalate --task ID --reason TEXT` | Escalate | Hold a non-terminal task with OperatorEscalate. |
| `block --task ID --reason TEXT` | Block | Hold a declared stage; retain its original stage and Failure remediation route. |
| `cancel --task ID --reason TEXT` | Cancel | Cancel Pending or Blocked, refusing in-flight work; report existing blocking dependents without releasing edges. |
| `defer --task ID [--until RFC3339]` | Defer | Hold Pending indefinitely or until a timestamp. |
| `defer --task ID --undefer` | Undefer | Remove a Pending deferral. |
| `reprioritize --task ID --priority N` | Reprioritize | Priority >=10; future dispatches sort larger first, then task ID. |
| `set-attempt-limit --task ID --limit N` | SetAttemptLimit | Positive per-task limit, without changing the pinned workflow or automatically resuming. |
| `prophet restart --task ID --information TEXT` | RestartWithInformation | Append hashed information; restart Escalated at the first stage, or leave Pending at Pending. Refuse in-flight tasks. |
| `prophet resume --task ID --context TEXT` | ResumeWithContext | Append hashed context for future briefs, preserving state and the existing turn. |
| `resume --task ID` | Resume | Explicitly re-enter a Blocked task's declared Failure stage, or, after raising its limit, restart an AttemptCap task at the first stage (Escalated has exactly two out-edges, FAE-WF-021); the attempt that hit the cap is recorded as `attempt_cap_stage`. The raise re-grants attempts and nothing else (FAE-WF-074): the test-run count, the scope-widen and RED-rework request counts, every Loop iteration count and loop-progress streak, the stage failure counters, visits, evidence and approved scope all carry over for the life of the task, and the audited control records the carried values (`Resume: <reason>; restart from <start> refunds nothing; carries counters {...}`). The reply states them too: `carried` gives each budget's count used and its limit (attempts against the raised limit, test runs, every Loop, any progress streak, request counts); `ResumeRun` puts it on each resumed task. A task parked `all-routes-unavailable` for want of a route, with one of its stage's routes refused by the bridge since the daemon last started (FAE-RT-010), resumes in place: it forgets those refusals and restarts the route-exhaustion bound, so its next turn tries every route again at the same stage, charging nothing. A park on unhealthy routes alone is refused (health heals by decay), and so is one whose only refusals came before the daemon's last start: they no longer apply, and that start already tried those routes again. Every other escalation kind is refused, and where the test-run budget and the attempt limit are both spent at one boundary the task escalates TestRunCap, so a raise never bypasses TestRunCap, LoopCap, ScopeWidenRejected or any other kind. |
| `migrate --task ID --revision N [--stage S]` | Migrate | Move a non-terminal task with no dispatch in flight (a task quarantined because its pinned revision is one this binary cannot run included; FAE-WF-093) to revision N of its own unretired workflow. The mapped position is the current stage (or a `--stage` reached by an edge of revision N), a Blocked task's remediation route, or the start stage for Pending and Escalated tasks. A task at a stage that revision N lacks must be blocked or escalated first. Refused when stored evidence does not validate against N or the mapped stage lacks a required input (FAE-WF-093); review-lifecycle tasks keep their revision. |
| `decide --task ID --option OPTION [--decision ID] [--rationale TEXT]` | Decide | Dispose a task parked at a HumanDecision stage (FAE-WF-030): record a schema-validated decision artifact with operator provenance citing the authorizing message, write the `decisions` row in the same transaction as the transition, and move along the option's declared route. `--decision` pins the pending decision id shown by `fae inbox --kind decisions`. A task not waiting (already decided, blocked, escalated, cancelled or terminal) is refused, so a decision is never applied twice; a waiting decision never times out. |
| `attach-design-doc --task\|--item ID --reference REF` | AttachDesignDoc | Append an http(s) URL, or a path resolving inside the target's containment root (the target configuration's workspace, else the bound workspace), as hashed `design-doc` context bound into every later brief. Idempotent per reference. |

All commands require `--authorizing-message ID`. Top-level `restart` and
`resume-with-context` are aliases for the corresponding Prophet controls.
Terminal tasks refuse every control. No control refunds a budget: restart with
information, a Blocked resume and an AttemptCap resume all keep the attempt,
visit, Loop, loop-progress, test-run, request and failure counters, so Appendix
A's 8 test runs and 3 scope widens per task hold for the task's life (operator
ruling: keep per-task limits). The stall-retry counter is not a budget: it
bounds one stalled turn (FAE-RT-005), and every end of that turn clears it,
whether the turn completes, records a failure, leaves its stage by any edge
(an escalation of any kind, StallExhausted and AttemptCap included, or an
operator hold), or is redispatched after an uncertain dispatch
(`resolve-uncertain --action redispatch`, which keeps the crash-recovery
count). A restarted task's first stalled turn therefore gets its 2 retries
again. Resume and restart with information also forget the bindings the bridge
refused for the task (FAE-RT-010), so its next turn tries every route again,
and start a new route-exhaustion clock (a daemon restart does not). A restart of an Escalated task with information records the
carried counters, and replies with `carried`, as Resume does. Each restart from
the first stage re-runs the pipeline on those counters: a full pass of the
shipped default enters `spec-loop` (cap 3), `red-loop` and `implement-loop` once
each and spends four test runs (RED, GREEN, QA, post-merge QA). A raised task
with fewer test runs or loop iterations left than a pass needs therefore cannot
finish: its new pass escalates TestRunCap or LoopCap, and that escalation's
detail names the count used, the limit, and that the count carries for the life
of the task. Resume does not refuse such a task up front (an open decision); the
reply's `carried` budgets show it. Retire it, re-admit the item
(`fae run --carry-from` keeps its pre-code evidence), or migrate it to a
revision with larger limits and restart it with information. Every state change
these controls make is an edge accepted by `workflow.CanTransition`, the one
transition-legality function (FAE-WF-008).

A control's result must pass `engine.Validate` (`ApplyControl` refuses it
otherwise), and the daemon validates the row again before any operator
checkpoint (task controls, decisions, design-doc attachments, and uncertainty
resolutions, live or of a scripted hold) saves it, so no operator verb stores a
row the drive would quarantine. Each of these refuses while another checkpoint
is unresolved. A carry-forward re-admission waits Pending with its carry entry
(FAE-WF-086). Defer, Undefer and restart with information keep it; any edge out
of Pending other than the release (Escalate, Cancel, an engine fault) drops it,
so an escalated carried task keeps both Escalated out-edges.

Context is append-only and SHA-256 hashed. Dispatch preparation freezes the
context suffix; the live binding combines it with immutable admitted content.
An in-flight context update therefore cannot change a brief even if execution
has not yet resolved its worktree. Escalate and Block retain the exact running
dispatch and collect its result/provenance while holding the operator state.
Their completion does not publish a hypothetical next-state ticket transition.
A crash during a held turn parks on uncertainty and requires recorded adoption
or escalation, never implicit re-dispatch.

A workflow stage without a declared Failure remediation stage may be blocked,
but `resume` refuses to invent a route. `prophet resume` is context-only and
cannot release that hold. Escalate followed by explicit restart with information
is available when restarting the workflow is intended.

Retirement (RETIREMENT.md), backlog admission and edge editing (BACKLOG.md) are
separate verbs, as are the live task verbs (TASKS.md): `task update` edits a
task's admission fields from its next stage, and `task delete` composes Escalate
and Retire, holding a turn in flight until the drive collects it. While such a
delete is pending, `restart`, `resume` and `block` of the task are refused;
`retire` is refused too while the turn is in flight (the kernel refuses Retire
with a turn in flight) and ends the delete once nothing is. Cancellation
reports already-existing blocking edges and the
dependents it holds. Prophet chat/session management is also separate from these
task-control aliases.

Live restart/resume additionally verifies the existing owned worktree and its
physical HEAD/tree before committing a transition. If a VCS commit has advanced
HEAD beyond the durable reconciliation base, this phase refuses restart even
when the commit is the task's own: adopting it as Base could discard the task's
unintegrated patch. A separate proved execution-head/reconciliation model is a
remaining gap. The refusal preserves the escalation, content and attempt budget.
It is covered by a native workflow through an actual broker commit. The check
also refuses missing/foreign worktrees and unrecorded source changes.

## Quarantined rows (FAE-ROB-001, FAE-ROB-042)

A task whose durable or cached row the drive cannot safely act on (undecodable,
invalid, bound to another executor kind, diverged, or non-terminal without the
disposition persisted at submission, which is never defaulted to Local and is
named in the cause) is quarantined: it leaves the
drive with an audited `task-quarantined` alert and appears in status. Startup
quarantines such a row instead of refusing the instance; only the pane-limit
floor (FAE-SCH-012) still refuses startup. Quarantine is never lifted silently,
including by a plane reload: one of these verbs resolves it.

| CLI | RPC | Behavior |
| --- | --- | --- |
| `quarantine [inspect] [--task ID]` | QuarantineInspect (read) | Each quarantined task's cause, durable row identity (state, version, canonical digest, in-flight dispatch), its re-validation now (`ok` once repaired) and, when its task or backlog row records another state than its snapshot (FAE-CP-006), `row_divergence` naming it; and every retired row. |
| `quarantine release --task ID` | QuarantineRelease (task) | Re-run the full startup validation on the durable row and return it to the drive. Refused while it still fails, while a dispatch of it is still running, or while the task is pinned to a stored revision this binary cannot run (FAE-WF-093; `fae migrate` or `fae quarantine retire` are its ways out, see the daemon README's startup checks). A task or backlog row state that differs from the snapshot's is first set to the snapshot's in an audited write (`quarantine-release-repair`; the result's `repaired` names it), so the next start or plane restart does not quarantine the task again; a task whose task row is missing is refused (retire it or restore the plane). An in-flight dispatch recovers as the startup load would have; one whose outcome run-time isolation dropped is never charged (scripted replays its exact token, live parks UncertainDispatch). |
| `quarantine retire --task ID --reason TEXT` | QuarantineRetire (task) | Durably hold that exact row (version and digest) out of the drive across restarts without a new alert. The row is untouched and its backlog dependents stay held; a changed row is validated again. |

Release and retire require `--authorizing-message ID` and are audited.
