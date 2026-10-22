# Opt-in live execution

Direct and multiplexer modes run a configured **trusted agent bridge**, real Go test commands,
and guarded local Git effects in registered task worktrees. No provider account
is selected automatically. The bridge protocol is documented in
[execution/README.md](execution/README.md). It must enforce the role's provider
and tool profile; this is not an OS sandbox for hostile agents. A stock agent CLI
cannot simply replace the bridge executable because handoff and profile contracts
must be implemented by the wrapper. The included `cmd/fae-codex-bridge` supplies
a restricted Codex wrapper: model processes have read-only access with command
tools disabled, and the bridge validates declared-file edits before applying
them. It is still a trusted bridge, not a general hostile-agent sandbox.

Create a dedicated, un-checked-out integration branch from the full commit SHA
selected as the base. Supply a JSON configuration like this with real paths:

```json
{
  "command": ["/absolute/path/to/trusted-agent-bridge", "{config}"],
  "environment": {},
  "allowed_commands": [
    ["go", "test", "-json", "-count=1", "-a", "-x", "./..."],
    ["gofmt", "-l", "."]
  ],
  "files": ["answer.go", "answer_test.go"],
  "base": "FULL_IMMUTABLE_COMMIT_SHA",
  "integration_ref": "refs/heads/fae/integration",
  "commit_message": "Implement verified task",
  "test_file": "answer_test.go",
  "test_id": "TestBehavior",
  "expected_failure": "specific expected assertion failure",
  "host_ledger": "/absolute/physical/path/shared-by-all-instances/host-leases",
  "heavy_capacity": 2,
  "timeout_seconds": 300
}
```

Start from the canonical bound repository root:

```sh
./bin/fae serve --workspace "$PWD" --instance live --adapter direct \
  --live-config /absolute/path/target.json --foreground
```

For Herdr, add `"adapter": "multiplexer"` and
`"herdr_binary": "/absolute/path/to/herdr"` to the target configuration, and use
`--adapter multiplexer` instead of `--adapter direct`. Each turn uses a private
server, session and gated pane; ambient Herdr selection and user configuration are
ignored. No existing pane is adopted. See [runtime/MULTIPLEXER.md](runtime/MULTIPLEXER.md).
The direct and Herdr adapter choices are part of the pinned target configuration.

Install a workflow through `template workflow-put` before admission. The shipped
workflow is a rehearsal template: configure its real provider/model routes and
change every Go HostCommand vector to match the exact `-a -x` allowlist above.
The real broker requires compiler evidence for fresh-build claims; `-count=1`
alone only disables cached test results. `workflow default --json` exports the
template. Use the standard message, workflow-put, run, history and events verbs
with `--instance live`. An operator message must authorize each mutation.

The target configuration hash is pinned in the instance database. Restarting
with a different configuration refuses rather than silently changing the trust
boundary. Base must be an immutable commit SHA; caller-supplied task tree hashes
are replaced with the actual owned worktree tree at live admission. Each instance
must use the same physical host-ledger directory and capacity to share heavy-op
admission. Conflicting ledger capacity refuses. Role pools are scoped by workflow
ID and role across revisions, or by workflow/task/role. Incompatible role caps
currently refuse; live cap reconfiguration is not implemented.

Each bridge session is a fresh owned process group. Environment inheritance is
restricted, session files are private, handoffs are bounded and identity checked,
and timeout/cancellation retires owned descendants. Host commands use literal
allowlisted argv, bounded output and a timeout. The actual test file supplies RED
identity, not an agent's source report. RED turns may change only that test file;
implementation must preserve both its frozen function identity and all test-file bytes. All changes must stay within
the target's declared file set before they can produce accepted evidence.

Commits use a temporary index and the exact reviewed whole-tree bytes. Integration
updates only a dedicated, un-checked-out `refs/heads/fae/*` branch with compare-and-
swap. Ancestor, patch-equivalent, all-touched-path blob, and empty-change
containment report the containing tip without moving it. No developer branch is
automatically merged.

When the target advances, merge prepares an immutable three-way plan, checks
that every own hunk remains, persists the plan, and applies it only to the task
worktree. The current default workflow repeats tests, frozen-test comparison,
format checks, reviews, Oracle and QA on the merged tree before commit. A known no-write integration-tip race takes a bounded reconciliation retry,
including the already committed own delta and fresh approvals; an unknown write
outcome remains uncertain. A further ref race cannot land unverified code. Supported text conflicts enter bounded implementation remediation with named
paths and an immutable original-change contract, followed by fresh tests and
approvals. Missing original additions/deletions, frozen-test conflicts and unsupported
file conflicts refuse. A second target advance while an original conflict contract
is active requires operator review rather than replacing that contract. Automatic adoption of uncertain Git effects remains pending. Partially upstream own work
can conservatively refuse if its diff shrinks rather than silently discard hunks.

Failed or interrupted live effects park as uncertain and are never blindly
replayed after restart. Successful completed stages remain durable. Graceful stop
waits for workers to retire before closing the database. Exact process birth and
group identities are recorded before workloads are permitted to run. Parent-death
watchdogs handle proved owned groups after SIGKILL, and startup recovery runs
under the instance lock before new execution. Records and retirement receipts
support cleanup audit. Foreign, reused, unreadable or leaderless ambiguous groups
refuse recovery; a dead parent alone never proves that a resource lease is free.
A fsynced admission journal links each permit to its exact owner and session.
Startup can reclaim never-launched reservations, completed retirement records,
or a complete expected set of linked watchdog retirement receipts. Role and host
release replay safely across a partial two-store update. Live, foreign, missing
or partial process evidence retains capacity. See
[processowner/README.md](processowner/README.md) for the stopped-launch window,
Darwin signal boundary and escaped-descendant limits. Ledger audit history has a
bounded size and fails closed when archival is required.


## Planned restart at a durable boundary

`fae drain --authorizing-message ID` stops new admissions and stage dispatches.
Active turns continue through validation and durable collection. Poll `status`
until `snapshot.drained` is true, then call `shutdown` and launch the same pinned
instance configuration. The drain flag clears on startup; completed turns keep
their provenance and remaining stages resume. A parked uncertain dispatch does
not count as drained. This explicit planned-restart path does not adopt unknown
effects or change immediate shutdown/cancellation semantics.

For independent task scopes, target configuration may pin a `tasks` map keyed by
task ID, each containing `files`, `test_file`, `test_id`, and `expected_failure`.
The included bridge accepts a matching `--task-scopes` JSON literal inside the
pinned command vector; no mutable external scope file is trusted.

## Real-provider dogfood

The recorded two-task acceptance passed using Codex `gpt-6-astra`, Herdr 0.9.5
and private Dolt. Both tasks generated genuine RED tests and implemented separate
code changes. After both reached specification review with two artifacts each,
the harness drained active turns, stopped the daemon and restarted the same
instance. Both checkpoint chains remained byte-for-byte unchanged in final history.

The tasks reached Done with 35 artifacts/version 83 and 22 artifacts/version 52.
The first lost the integration race and exercised the bounded reconciliation
retry, repeating tests, reviews and QA before landing both contributions. The
host capacity was 2; 58 grants matched 58 releases, with no remaining holders or
waiters. The developer checkout stayed unchanged. Complete chains, restart
checkpoints, ledger audit and tool versions accompany the machine-readable
[result](verification/results/herdr-dogfood/summary.json).

With a configured Codex account, reproduce this opt-in run using:

```sh
go build -o bin/fae ./cmd/fae
go build -o bin/fae-codex-bridge ./cmd/fae-codex-bridge
python3 scripts/dogfood.py --model gpt-6-astra --dolt /absolute/path/to/dolt
```

The harness accepts `--codex` and `--herdr` paths when those binaries are not on
PATH. It uses the existing Codex account and performs model calls. This exercises two genuine code-bearing tasks with a planned drained restart. Persistent interactive sessions,
general brokered agent command tools, complete cross-user host inventory,
Normal-mode external effects and later controller phases remain open.

Protocol-fixture tests remain separate evidence: they exercise real host, Git,
Dolt and lifecycle behavior without contacting a model provider. Neither those
tests nor this real-provider run establish every phase acceptance criterion.

## Health, command policy and pane reservations

Live dispatch selects the first eligible provider/model in each role's ordered
route, persists that selection, and records it in artifact provenance. An unknown
route starts at 100; penalties, one-hour half-life healing and the post-throttle
success hold determine subsequent eligibility. No eligible route parks without
charging an attempt; the daemon periodically rechecks. A provider failure after
launch can leave uncertain edits, so that turn remains parked rather than being
replayed automatically on a fallback.

Inspect effective scores and the transactional operator log with:

```sh
fae provider-health --home /absolute/instance-parent --instance NAME
fae provider-log --home /absolute/instance-parent --instance NAME --after 0 --limit 100
```

`provider-log` returns audit sequence cursors. Each observation commits its row,
log line and event together; repeated collection of the same observation does
not duplicate them. This durable log is the authoritative record, rather than
an independently appended daemon text file.

Multiplexer live configuration accepts `pane_limit` (default 60). The limit must
exceed workflow-global role capacity plus one. All cooperating instances sharing
a host ledger must use the same pane limit. Herdr inventory is measured through
its pane API for discovered server sockets; an unreadable census fails closed.
Reservations include not-yet-visible launches and memory warmup. Pane limits can be changed through the authenticated runtime configuration API,
with an authorizing operator message; other live-target settings remain pinned.

Supported Go host commands pass the literal allowlist and command policy under
the durable heavy lease. Role sessions receive PATH-first denial shims and the
Codex bridge disables command tools, so model edits and broker commands remain
separate stages. See [commandshim/README.md](commandshim/README.md) for supported
flags, formatter bounds and the limits of PATH enforcement.

## Dynamic pane configuration and recovery

```sh
fae message --id capacity-change --text 'Change this instance pane limit to 24'
fae runtime-config --pane-limit 24 --authorizing-message capacity-change
```

The audited value survives restart and is refreshed every dispatch round. Each
cooperating instance registers its requested cap with exact process ownership;
the effective shared cap is the minimum across live registrations. Reducing it
does not terminate existing sessions or invalidate their reservations: it defers
new admissions until capacity is available. The configured floor must exceed
workflow-global slots plus one. Status reports the effective dynamic cap and
which settings remain fixed for the process lifetime.

`stall_seconds` sets the positive-liveness inactivity window (default 300 seconds).
The runtime samples descendant CPU accrual, recognized structured session activity
and handoff growth; a quiet terminal alone is not a stall. An unreadable process
census cannot authorize termination. Use a larger `timeout_seconds` for the
whole-turn deadline so the inactivity detector has time to operate.

`stall_safe_replay: true` is an explicit operator assertion that the configured
bridge has no external side effects and does not change files except the measured
task worktree. It is suitable for the restricted bundled Codex bridge, not an
arbitrary agent CLI. Even with that contract, retry requires complete retirement,
unchanged measured tree, successful observation persistence and lease release,
and no earlier executed session in the dispatch. Two durable retries are allowed;
the next stall yields `StallExhausted`. Route health may defer a retry before then.
Without that proof, the turn remains uncertain and is not replayed.

Idle-session shedding is available to trusted adapters through
`daemon.Config.ConfigureIdleSessions` and the owned idle-session registry. It
retires one eligible pre-code global after three own-idle saturated rounds,
protects active/final/QA sessions, and retains context for relaunch after TTL
retirement. The default bridge transport eagerly retires panes; opt-in resident transport is described below.
See [the adapter lifecycle contract](runtime/IDLE_SESSIONS.md); provider CLI processes are not retained; resident Herdr workers and context
continuation are described below.

The fresh [runtime-recovery acceptance run](verification/results/runtime-recovery-dogfood-20261017/result.json)
completed both real-provider tasks after changing the cap to 16 during active
work, preserving it across a drained restart, then raising it to 24. It matched
29 real provider turns to provenance and released all 58 host leases. This run
had no stalls; forced recovery and idle shedding have separate integration tests.
Use `--evidence-dir` with a new directory when running the harness to retain
checkpoint chains, audits, provider observations and verified file/source hashes.

## Persistent provider context and dynamic host configuration

Pass `--session-root /absolute/private/instance/provider-sessions` in the pinned
`fae-codex-bridge` command to enable persistent Codex context. Workflow-global
roles resume only explicitly recorded provider UUIDs. Workflow revision, role,
route, tool profile and mode form the isolation key. Each turn supplies its
current task header and current files; saved conversation content never grants
permissions or serves as workflow evidence. Task-bound roles resume within their own task and start fresh for another task.
Reviews always remain fresh. A durable pending marker invalidates uncertain context after a crash or
failed turn. Reuse of each exact context is serialized. By default Herdr processes/panes retire every turn; provider conversation history
persists independently.

The installed CLI's explicit `exec resume` path and structured events are checked
against the [official non-interactive documentation](https://learn.chatgpt.com/docs/non-interactive-mode).
Each continued turn records a private `provider-session.json` linking dispatch,
task, scope and provider UUID. The root must be private and physical; arbitrary
ambient sessions are never selected. Provider-neutral loops and resident Herdr workers can be enabled as described below.

`runtime-config` now accepts partial updates for `--heavy-capacity`,
`--tasks-in-flight` (1–256), and `--host-thresholds` (a complete JSON Config), as
well as `--pane-limit`. An audited update preserves omitted values, survives
restart and is refreshed every round. Shared heavy capacity uses the minimum
live registration. Shared memory floors/per-pane estimates use the maximum,
and pressure ceilings use the minimum. Lowering capacity never revokes a held
lease or cancels an active task; new work waits for sufficient capacity.
`Pool.AdmissionLimit` is the current heavy cap; `Pool.Capacity` retains a maximum
needed to validate older held and waiting weights. FIFO waiters remain ordered.
Status reports these settings as dynamic; root and socket remain fixed.

Host threshold keys are `Soft`, `Hard`, `HardNoSwap`, `PerPane` (bytes), `IO`,
`MemoryPressure`, `Load`, and `PaneLimit`. Supply the full threshold object from
status with the intended fields edited; invalid relationships and nonfinite
numbers refuse. The dedicated pane limit remains the Herdr cap.

`start_seconds` bounds transport startup (default 180 seconds), including gated
Herdr readiness. Proven nonexecution can retry up to three total attempts with
fresh dispatch/session IDs and a durable per-stage counter, without consuming
task or stage attempt budgets. Exhaustion yields `StartupExhausted`; completed
work resets that stage's startup counter. Retirement and an unchanged worktree
are required. Once a bridge has executed, missing provider acknowledgment does
not prove nonexecution: that outcome remains uncertain or follows the separately
bounded stall path. This is not a generic provider acknowledgment protocol.

The [persistent-provider acceptance run](verification/results/persistent-host-startup-dogfood-20261017/result.json)
completed two real tasks in 29 turns with eight verified resumptions, including
two across restart and two within task-bound contexts. Three global contexts
served both tasks; task-bound contexts never crossed tasks and reviews stayed
fresh. Pane/heavy/task/load overrides survived restart. All 58 leases and owned
process/pane resources were released. Enable the harness's `--persistent-sessions`
flag to exercise these assertions; evidence is retained with `--evidence-dir`.

## Resident sessions, loops and crash resolution

Set `resident_sessions: true` in the pinned live configuration to keep owned
Herdr worker panes across turns. This requires the multiplexer adapter. The
worker stays resident; each bridge/provider invocation remains a separate owned
turn process. Keep `--session-root` to retain provider conversation context.
`resident-session.json` records each turn's resident ID and server/pane PIDs.

Set `persistent_loops: true` only for a bridge that implements persistent context.
The bundled Codex bridge requires `--session-root`. For example:

```sh
fae schedule create --id observe --kind loop --role observer   --prompt 'Report a short observation in the message field.'   --provider codex --model gpt-6-astra --effort medium   --interval-seconds 10 --max-iterations 5 --expires-in 1h   --authorizing-message AUTHORIZATION
fae schedule inspect --id observe
fae schedule pause --id observe --authorizing-message AUTHORIZATION
fae schedule resume --id observe --authorizing-message AUTHORIZATION
fae schedule retire --id observe --authorizing-message AUTHORIZATION
```

Loops use a dedicated pinned read-only worktree. A crash during an iteration
parks that schedule as uncertain; retire it explicitly rather than automatically
replaying a possibly executed prompt. Cron is covered by the subsequent increment below.

For a parked workflow turn, inspect history and resolve the exact dispatch:

```sh
fae resolve-uncertain --id TASK --dispatch DISPATCH --action redispatch   --authorizing-message AUTHORIZATION
```

`accept --result FILE` accepts only schema-valid original read-only evidence with
unchanged tree and HEAD. `escalate` stops the task. All actions require retired
turn ownership, an audited operator message and matching uncertain dispatch.
Redispatch preserves budgets and refuses after three genuine crashes.

The dogfood harness adds `--resident-sessions --provider-loop --crash-during-turn`
to exercise physical pane reuse, five real loop prompts and an actual mid-turn
SIGKILL with explicit recovery, alongside the existing drained restart.

The [final resident/loop/crash acceptance run](verification/results/loops-resident-crash-dogfood-20261017-r3/result.json)
passed two workflow tasks and five loop iterations in 34 completed real provider
turns. It proved physical pane reuse, a provider-turn SIGKILL followed by audited
redispatch, and a separate drained restart. All owned leases, processes, terminal
receipts, pane reservations and sockets were cleared. The evidence manifest
includes source/binary hashes captured before execution.

## Cron admission and controller lifecycle

Create an interval-anchored cron schedule using `schedule create --kind cron
--spec FILE --authorizing-message ID`. The JSON contains `id`, `kind: "cron"`,
RFC3339 `anchor`, `interval_seconds`, optional exclusive `end`, optional
`catch_up_limit` (default 10, maximum 100), and `template` with `workflow_id`,
`revision` (zero pins the latest revision at creation), and `content`. Each
occurrence has a deterministic task ID. Admission precedes cursor advancement;
recovery does not repeat admission effects. Older missed occurrences outside the
catch-up bound are recorded as skipped. Creation authority persists until the
schedule ends or is retired; lifecycle mutations require fresh authorization.
This is interval recurrence, not a five-field cron expression parser.

Start the bundled launcher outside agent containment in a private physical
0700 directory, then use the authenticated controller:

```sh
fae runtime-launcher --socket /absolute/private/runtime.sock
fae maintenance stop --launcher-socket /absolute/private/runtime.sock \
  --note 'planned restart' --authorizing-message ID
fae maintenance status
fae maintenance start --authorizing-message ID
```

The launcher checks native sandbox state. Maintenance pins executable hashes,
arguments, environment and process identity; post-start acceptance checks the
new daemon and Dolt, plane consistency and a completed drive round. The marker
has no expiry. `watchdog --authorizing-message ID` runs independently and honors
maintenance markers, monitors drive progress and filesystem capacity, and
persists pending alerts while the daemon is unavailable. Watchdog alert/rearm
mutations still require a current operator authorization message; renew that
citation for supervision beyond the message freshness window. Health reads and
continuation of an already-authorized maintenance operation do not expire.

`roll --binary PATH` uses this lifecycle. Changed binaries additionally require
clean embedded VCS identities and `--gated-ref refs/fae/gated/NAME`. Build-from-ref,
critical-task indefinite overlap holds, deployed-tip integration fast-forward,
and recovery of a stopped drive without daemon restart remain follow-up work.
Do not treat this increment as complete coverage of every FAE-CTL requirement.

For an uncertain local host/VCS effect, `resolve-uncertain --id TASK --dispatch
DISPATCH --action accept --authorizing-message ID` loads the daemon's immutable
receipt and rechecks exact postconditions. It does not accept supplied result
JSON. A Git CAS completed before receipt persistence may be reconstructed only
from the pinned intended SHA and exact retirement proof. A host command with no
completed receipt remains uncertain. Explicit redispatch of effects requires
positive proof that execution never started. External service effects are not
covered by local receipts.

The dogfood harness adds `--cron-schedules --controller-lifecycle` to admit both
real-provider tasks through durable schedules and replace the planned restart
with maintenance stop/start through the independent launcher.

The [latest authenticated acceptance](verification/results/cron-controller-effects-dogfood-20261018/result.json)
passed both cron tasks, provider-turn crash recovery, controller restart and
maintenance watchdog suppression. It completed 34 provider turns including five
loop iterations, reused seven resident panes, accounted for all 66 host leases,
and left no owned process/pane/socket residue. Source and evidence hashes were
checked against the final implementation.

## Deployment and independent drive operations

`fae roll --build-ref refs/fae/gated/NAME --launcher-socket /absolute/runtime.sock
--note "deploy gated revision" --authorizing-message MESSAGE` builds in an isolated
clean clone and retains the resulting binary under the instance deployment
folder. `--gated-ref` can pin the approval ref explicitly; the requested source
must equal its tip. Changed-binary source history must exist in the bound
workspace. `--when-clear` also waits for QA, while commit windows are always
protected. A gate moving during build requires retry. `--overlap-wait` defaults
to two hours; `admit --critical` protects a task from timed overlap overrides.
Use `--ack-overlap` only when intentionally authorizing the recorded overlaps.
The existing `--binary /absolute/fae` path supports same-binary rolling restarts.

`fae drive status` reports daemon/plane identity and drive generation.
`fae drive stop --authorizing-message MESSAGE` holds the scheduling loop without
stopping workers or Dolt. `fae drive rearm --authorizing-message MESSAGE` validates
durable checkpoints and resumes collection. Active maintenance suppresses both
operations; a failed preflight record does not. Rearm attempts consume a durable
three-per-cause hourly budget.

Starting `fae watchdog --authorizing-message MESSAGE` while the daemon is reachable
registers a narrow persistent grant for alerts and drive rearm. Revoke it with
`fae watchdog --revoke-grant OLD_MESSAGE --authorizing-message FRESH_MESSAGE`.
A revoked citation cannot register again. General mutations still require a
fresh operator citation; `serve --message-max-age 72h` configures a positive
duration, defaulting to 24 hours and retained across controller restarts.

For Normal live/dry-run disposition flags, installed external adapters and
query-only recovery, see [external adapter operations](external/README.md).

## Configuring review and ticket adapters

Install each capability under `external_tools` with an exact endpoint, server,
tool name, daemon role and immutable argument pins. The adapter must expose
`GET /schemas`; configured aliases are schema-checked during startup, including
when subsequent tasks use dry-run. Dry-run itself makes no effect query, status
read or mutation. Remote endpoints require HTTPS; literal loopback may use HTTP.
The [schema contract](external/SCHEMAS.md) defines descriptors, typed arguments,
size/time bounds, refresh behavior and source binding validation. No native
vendor transport or credentials are configured by Fae; the explicitly installed
adapter handles vendor authentication and translates the documented protocol.

`review_policies` is keyed by task ID. Each policy names `package`, absolute
physical `workspace`, `branch`, `integration_tip`, `baseline_tool`, `status_tool`
and an explicitly approved `baseline_approved` verification mechanism. The
workspace must already exist on the task branch and contain the reviewed tree.
Fae never provisions or syncs that workspace during execution. The baseline read resolves the current integration ref and pins that exact commit
in its request; a red or mismatched response is a target-configuration defect.
`fae review-health --task ID` checks that target through read-only adapter calls.

For vendor-specific names, `operations` maps installed aliases to semantic
`review_create`, `review_read`, `review_revision`, `review_publish`, `review_merge`
or `review_baseline` operations. `argument_keys` maps semantic fields such as
`workspace`, `branch`, `package`, `review_id`, `revision`, `draft` and automatic
publish/merge flags to the vendor's actual schema keys. Installed pin values may
use daemon-owned `$workspace`, `$branch`, `$package`, `$integration_tip`, `$review`
and integer `$revision` tokens. Unknown tokens and incompatible schema types
are refused. Baseline/status aliases must expose output schemas and the
normalized read data described in [external/README.md](external/README.md).

Set instance `disable_review_publish_merge` or task policy
`disable_publish_merge` to omit publish/merge stages. Each removed stage must
have a single successor. `remediation_stage` names a workflow `Loop` whose edge
enters a change-producing implementing `AgentTurn`; the review-read stage's
failure edge must target that loop. The workflow must re-establish ordinary
verification evidence before a review revision. The create stage’s alternate
`failure` edge must name the sole `review_revision` stage: once a review ID
exists, returning to creation routes to that revision stage without another
creation call. Read remediation carries a daemon-measured committed HEAD at the
reviewed tree into the next implementation turn. Pre-provisioning the task’s
`worktree.Manager.Binding(run, task)` path lets the same real workspace follow
implementation, commit and review revisions without build-workspace syncing.
`max_revisions` defaults to five and must be within 1..100. Pending vendor
status observations use a separate durable budget: `poll_seconds` defaults to
30 and `max_polls` to 120 (each accepts 1..3600). They do not consume the task’s
implementation-attempt budget. Counts and retry deadlines survive daemon
replacement; reaching the budget escalates with `ReviewPollCap`. A ready read,
remediation handoff, or successful lifecycle operation resets that wait budget.
 A missing, dirty, mismatched or symlinked review
workspace fails closed; updating a separate provisioned workspace is not an
implicit part of remediation.

`ticket_workflow` explicitly lists enabled `workflows`, a `refresh_seconds`
value in 1..300, and `actions` keyed by lifecycle state/stage. `Pending` is the
creation action. Each action names an installed `tool` alias, the exact runtime
room `action` name and `bindings` from actual vendor argument keys to daemon
sources: `task`, `room`, `ticket`, `destination`, `push_namespace`. Each action
must bind room, destination and push namespace; creation cannot require a prior
ticket. Dynamic keys can be omitted from static tool pins, but must exist with
compatible types in the discovered schema. Static pins remain immutable.
The workflow must grant the installed tool's exact role. A managed ticket stage
must correspond to its configured lifecycle action; it uses the same durable
row as background mirroring.

Normal live admission still requires the explicit external-effects approval
phrase and pinned task destinations. Ticket-enabled workflows also require a
room. Local and dry-run dispositions never emit ticket mutations. Inspect
`status`'s `ticket_outbox` for depth, failures and uncertainty; attempts and
last errors remain in each durable row. An
uncertain started row is query-only after restart; do not interpret a missing
receipt as permission to create another ticket.
