# Opt-in owned idle sessions

`Direct` and `Multiplexer` retire their transport processes after every turn.
`ResidentMultiplexer`, enabled by `resident_sessions` in the daemon live config,
keeps a private Herdr worker pane and runs each bridge invocation as a separately
owned process group. Provider conversation persistence remains a separate bridge
feature (`--session-root`). Ambient Herdr workspaces are never adopted.

Resident pane reservations last through idle periods until exact retirement;
role leases belong to individual turns (agent turns take no heavy-op unit). Fresh
reviews use one-shot panes. A new slot's pane reservation (`BeforeStart`) that
fails, whether refused or stopped by a failed pressure, census or ledger step,
launches nothing: the turn defers as infrastructure, never as a startup retry,
so it is not charged to the provider binding (FAE-RT-004).
Global resident keys omit the task ID; task-bound keys retain it. Only
long-lived transports outlive a turn: workflow-global sessions and provider loop
sessions (`fae-loop:` workflows, FAE-RT-014). A task-bound transport (Herdr
server, worker pane and turn group) is retired through `Release` as soon as its
turn is collected (FAE-RT-011); it keeps no context file.

Concurrent turns of one key, such as one workflow-global role's turns from
different tasks, run on distinct resident identities, the slots of that key
(FAE-SCH-005, -009). A turn reuses the lowest-numbered idle slot, else starts
the lowest free one; slot 0 is the key itself, so its recorded handoff carries
over. A turn holds its slot from reservation until `Run` returns, so one
identity never starts two transports. The slots in use at once are as many as
the key's concurrent turns, which the role lease bounds by the role's cap. A
provider loop keeps one identity: an overlapping iteration defers without
consuming startup retries.

A slot whose transport's retirement is unproven (below) is not in use, but
keeps its pane, or at least its pane reservation, until a maintenance round
(or a provider loop's next iteration) proves it retired; one whose processes
already stopped has released both.
Each unproven retirement therefore leaves at most one pane beyond the role's
cap, and several can be outstanding at once, each retried every maintenance
round. The host pane limit still holds: the pane ledger counts
these panes and their reservations, so a new slot is refused at the limit.
`status` reports their number as `scheduler.unretired_transports`.

The multiplexer lock only reserves and publishes an identity. The liveness
check, removed-slot retirement, pane reservation (with its pressure back-off)
and Herdr startup run outside it, and the idle registry releases its own lock
while it retires a transport. So one task's slow launch, or another session's
retirement, never delays another task's turn (FAE-SCH-009). A maintenance
round that must census processes for a removed pane does hold the registry
lock for that census, so a launch's `Begin` can wait for it. `Close` refuses
later launches, cancels and waits for launches in progress, each of which
retires whatever it started instead of publishing it. Each turn refreshes
configuration and writes a `resident-session.json` identity audit that names
its resident and whether it was task-bound.

A turn never leaves its identity active. When the turn's own retirement is
unproven, or its handoff cannot be recorded, the transport is no longer trusted:
the turn ends and the transport is retired through the retirement path.
`Abandon` keeps a long-lived session's last recorded context; `Release` drops a
task-bound one's. If that retirement is unproven too, the session stays
registered and refused for reuse (it is counted as unretired). That holds even
when its processes already stopped and only a later registry step failed
(record retirement, link acknowledgement, handoff removal). Later turns of its
key skip the slot and run on another one; a provider loop, which keeps one
identity, retries the retirement at its next iteration instead. Every
maintenance round retries it, and once one proves it retired the registry
releases the slot (`OnRetired`), which a later turn can relaunch.

`OwnedIdleSessions` provides that capability's ownership and lifecycle boundary.
A trusted adapter registers a private session after proving its worker is live,
and calls `Begin` before releasing the first turn. Registration supplies its logical ID, socket under the owned runtime
root, role classification, exact live `processowner.Record` values for its pane
and server, and a callback that reaps children after termination. Records must
match private durable registrations under the runtime's `.processes` directory,
current daemon PID and exact birth, process owner, and session ID. A foreign
socket or discovered workspace cannot be registered by inventory alone.

Before reusing a session, call `Begin`: it atomically marks the session active
and returns its context handoff. The adapter must revalidate the current task's
canonical worktree and tool profile and provide an explicit task header. After
collection, call `End` with the new JSON context. It fsyncs and atomically renames
the context before marking the session idle. A turn whose session must not be
reused ends with `Release` or `Abandon` instead. Context is bounded to 1 MiB, has no
quality authority, and is never accepted as an artifact or an authorization.
The daemon's `ConfigureIdleSessions` callback is the trusted adapter registration
entry point; there is no arbitrary-process registration RPC.

The daemon maintenance round supplies a fresh Herdr census, the effective shared
pane limit and its in-flight count, read under the writer lock after the census
so a turn started meanwhile is counted. The census, any retirement and the
audited action rows run without the daemon's writer lock (FAE-ROB-021). An
unreadable census, or a refusal by the registry, skips the round; it is recorded
as `idle-maintenance-refused` (an audit row, an event and a supervision line)
when the refusal starts or changes cause, then at most every 10 minutes while
it persists, counting the refused rounds left unrecorded. A change of cause
also records the previous cause's unrecorded count
(`previous_unrecorded_refusals`), so none is dropped. Three consecutive rounds saturated by owned
idle panes with nothing in flight shed exactly one oldest eligible pre-code
workflow-global session. Active sessions, task-bound sessions, and protected
final-judgement or QA sessions cannot be shed. Stock `oracle`, `qa` and final
judgement role names are protected even if the adapter omits its flag; custom
final roles must be marked protected. If the same condition recurs with no
eligible session, the round reports `PaneCapStarvation`; the daemon records it
and escalates the parked tasks only after re-checking, under the writer lock,
that nothing is in flight. Foreign-held saturation
resets the own-idle counter and remains a deferral. When owned idle panes alone
fill the limit, the rule applies even if additional foreign panes exist.

Idle global sessions and retained task-bound residents opted into `Expire`
(provider loops) may retire at 30 minutes by default, including protected roles
when idle. TTL never authorizes closing an active turn. Both pressure and TTL
retirement retain context. On an on-demand relaunch, including after a daemon
restart, `ResidentMultiplexer` loads the recorded context with `Handoff` and
registers it instead of an empty one, and the relaunched session's first turn
receives it as a non-authoritative `session_handoff` brief block placed ahead
of the kernel contract (FAE-SCH-014). `Begin` on a retired ID explicitly
requires relaunch. No sessions are pre-filled just to occupy their role caps.

A pane removed out from under an idle slot (an operator's manual `kill`/`rm`,
or a dead server) is reconciled rather than failing (FAE-RT-012). The next
launch for that slot finds its identity no longer live and calls `Retire`,
which releases the slot only through exact retirement (pane reservation
included) before relaunching; a slot owning an in-flight turn is never closed.
If that retirement is unproven, the slot is set aside like any unretired
transport: refused for reuse, skipped by later turns and retried by every
maintenance round. The turn launched nothing and the failure is local, so it
defers as an admission deferral, which consumes no startup retry and charges
no provider binding; its next attempt runs on another slot (a provider loop's
next iteration retries the retirement). The adapter reports the deferral with
the turn's launch context (`ReconciliationDeferred`), and the daemon withholds
the task as `resident_unretired` (FAE-SCH-010).
The maintenance round does the same for an idle slot whose socket reports no
pane and whose registered group a fresh process census proves gone, reporting
`idle-session-reconciled`; a missing socket with live registered groups still
fails the round closed. A released or abandoned session whose retirement was
unproven is refused for reuse and retried by every later round. A retry that
stays unproven is reported with the round's actions and never stops the rest
of the round.

Retirement terminates only exact registered groups, then invokes the adapter's
reap callback and checks retirement before forgetting the session. It runs
without the registry lock: the session is refused for reuse meanwhile, and a
second retirement of it waits for the first. A failed
retirement retains the entry and its context; it never frees an uncertain pane.
Normal process-owner startup recovery handles an old daemon's processes, while
context files survive separately. Graceful closure refuses active entries.

Tests exercise actual process groups, role protection, active-turn exclusion,
round resets, foreign saturation, starvation escalation, TTL, persisted context,
record tampering, and native Herdr server plus pane retirement. The Gobra guard
covers the explicit pressure-shedding predicate; the Gomela model covers the
handoff-before-retirement acknowledgement protocol, not operating-system signals
or arbitrary adapter implementations.

Resident ownership markers are private and durable before launch. After a crash,
startup removes leftover sockets only when the marker matches the recovered
owner identity and complete server/pane retirement receipts. Linked receipts
remain until the matching durable lease acknowledgment; sockets and receipts
must be retired before shutdown cleanup is considered complete.
