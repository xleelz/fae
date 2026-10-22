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
| `resume --task ID` | Resume | Explicitly re-enter a Blocked task's declared Failure stage, or an AttemptCap task's exact interrupted stage after raising its limit. |

All commands require `--authorizing-message ID`. Top-level `restart` and
`resume-with-context` are aliases for the corresponding Prophet controls.
Terminal tasks refuse every control. Restart and explicit resume retain attempt,
visit and test counters; they do not refund budgets. An old AttemptCap snapshot
without a recorded interrupted stage refuses ordinary resume and requires the
explicit restart-with-information operation.

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

This phase does not implement task migration, retirement, backlog admission or
edge editing. Cancellation reports already-existing blocking edges; it does not
claim that backlog readiness orchestration is implemented. Prophet chat/session
management is also separate from these task-control aliases.

Live restart/resume additionally verifies the existing owned worktree and its
physical HEAD/tree before committing a transition. If a VCS commit has advanced
HEAD beyond the durable reconciliation base, this phase refuses restart even
when the commit is the task's own: adopting it as Base could discard the task's
unintegrated patch. A separate proved execution-head/reconciliation model is a
remaining gap. The refusal preserves the escalation, content and attempt budget.
It is covered by a native workflow through an actual broker commit. The check
also refuses missing/foreign worktrees and unrecorded source changes.
