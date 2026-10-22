# Opt-in owned idle sessions

`Direct` and `Multiplexer` retire their transport processes after every turn.
`ResidentMultiplexer`, enabled by `resident_sessions` in the daemon live config,
keeps a private Herdr worker pane and runs each bridge invocation as a separately
owned process group. Provider conversation persistence remains a separate bridge
feature (`--session-root`). Ambient Herdr workspaces are never adopted.

Resident pane reservations last through idle periods until exact retirement;
heavy/role leases belong to individual turns. Fresh reviews use one-shot panes.
Global resident keys omit the task ID; task-bound keys retain it. Concurrent
requests for the same resident defer without consuming startup retries. Each
turn refreshes configuration and writes a `resident-session.json` identity audit.

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
the context before marking the session idle. Context is bounded to 1 MiB, has no
quality authority, and is never accepted as an artifact or an authorization.
The daemon's `ConfigureIdleSessions` callback is the trusted adapter registration
entry point; there is no arbitrary-process registration RPC.

The daemon maintenance round supplies a fresh Herdr census, the effective shared
pane limit and its in-flight count. Three consecutive rounds saturated by owned
idle panes with nothing in flight shed exactly one oldest eligible pre-code
workflow-global session. Active sessions, task-bound sessions, and protected
final-judgement or QA sessions cannot be shed. Stock `oracle`, `qa` and final
judgement role names are protected even if the adapter omits its flag; custom
final roles must be marked protected. If the same condition recurs with no
eligible session, the round reports `PaneCapStarvation`. Foreign-held saturation
resets the own-idle counter and remains a deferral. When owned idle panes alone
fill the limit, the rule applies even if additional foreign panes exist.

Idle global sessions and task-bound residents opted into `Expire` may retire at
30 minutes by default, including protected roles when idle. TTL never authorizes closing an active turn. Both pressure and
TTL retirement retain context. `Handoff` loads it after retirement or daemon
restart for an on-demand adapter relaunch; `Begin` on a retired ID explicitly
requires relaunch. No sessions are pre-filled just to occupy their role caps.

Retirement terminates only exact registered groups, then invokes the adapter's
reap callback and checks retirement before forgetting the session. A failed
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
