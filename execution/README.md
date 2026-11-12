# Real agent execution

`execution.Executor` runs AgentTurn and Review dispatches through the configured runtime
(`runtime.Direct`, the Herdr `herdr-core` `Multiplexer` or resident Herdr workers).
It requires a canonical task binding, a current Git-tree measurement, and durable
lease admission before each launch. Sessions are fresh private directories, and
reviewer outputs are collected separately. A change producer requires the host's
CheckChange policy before advancing the tree, then the optional CheckOutput
check of its schema-valid output (the live daemon holds a derived RedReport to
its RED rule there); a refusal from either is a ValidationFailed retry on an
unchanged tree and a rejected change otherwise. Other roles must leave the tree
unchanged. Unknown post-launch failures carry `uncertain` so the daemon parks
instead of repeating potentially irreversible work. A trusted bridge's typed
refusal is not unknown (see Bridge refusals).

Every session's role, route and brief are checked and built before the first
launch, so a review whose later reviewer has no route or brief template runs no
reviewer. Any failure before the first session's admission (an unresolvable
binding, a tree that can't be measured, unreadable engine feedback, a role with
no route or brief, a daemon-side `ScopeRefusal`, missing dependencies) is
`refused-before-launch` (`engine.ClassRefusedBeforeLaunch`), which the engine
charges nothing for and bounds by SameGate (FAE-RT-004). A task branch whose
HEAD moved outside the broker stays `stale-residue`, and a cancellation stays
untyped. A host-admission deferral (`AdmissionDeferred`) of the first
session is `infrastructure-deferred` at once, and the engine parks it without
a charge (FAE-HOST-006). A later review session whose admission is deferred
asks again, every `LaterSessionAdmissionRetry` (5 s) within
`LaterSessionAdmissionWait` (30 min, the daemon's own admission bound), so
the reviewers that ran are not run again while the host is busy; past the
wait the dispatch is `infrastructure-deferred` carrying their reviews, which
the engine parks without a charge and counts toward the turn's deferral
bound (FAE-WF-033). A launch boundary that cannot be recorded
(`AdmissionLifecycle.Launching`) launched nothing: `refused-before-launch`,
with the reviews that ran. Any other admission failure stays uncertain.

## Engine feedback

A turn sent back by a failed gate sees why the run failed (Appendix C steps 5,
9 and 16). When a live host command fails (a nonzero exit, counts no test gate
accepts, or new format debt), the daemon builds its excerpt with
`effects.FailureExcerpt`, redacts the secrets it holds from it (FAE-SEC-003;
see [daemon/README.md](../daemon/README.md)) and stores it by reference
(FAE-CP-012) under an
`engine-feedback/` marker keyed by the run's session and bound to its artifact,
before the run's result is recorded. The `Feedback` hook then gives the brief an
`engine_feedback` block, after the stage block and before `operator_context`,
with one entry per failed `EngineRun` or `Format` among the dispatch's input
artifacts, in input order: stage, dispatch id, command, exit code, executed and
matched counts, excerpt and `excerpt_truncated`. A turn without one gets no block.
Each excerpt is at most 16 KiB and all of them at most 48 KiB
(`FeedbackTotalLimit`): over the total, the shortest stay whole and the rest share
what remains, each cut around its middle and flagged. A failed run recorded
without an excerpt, or with a record left by an interrupted attempt, has an empty
excerpt; a failed store read fails the dispatch before launch.

## fae-agent-v1 trusted bridge

Configure an absolute executable and argument vector containing the exact token
`{config}`. No shell is used. The token becomes an owner-only JSON configuration
path. Configuration contains protocol `fae-agent-v1`, dispatch/task/session ids,
worktree, mode, provider/model route, exact tool profile, absolute `handoff` path,
`log_directory`, and the structured JSON brief also supplied on stdin. The bridge
must translate the profile into its provider's actual allowed/denied-tool launch
controls and refuse unsupported grants. This is a trusted adapter protocol, **not
an OS sandbox**: arbitrary commands claiming to implement it are not confined.
Do not configure a general agent CLI without a protocol implementation. Provider
credentials may be explicitly supplied in Environment; daemon environment and
credentials are never inherited. Each session receives isolated HOME and TMPDIR.

Write exactly one JSON handoff object to the configured file before exiting zero:

```json
{"dispatch_id":"...","session_id":"...","task_id":"...","output":{}}
```

A bridge that refuses its turn with a class writes no handoff. It writes
`bridge-refusal.json` in `log_directory` instead and exits 65
(`runtime.RecordBridgeRefusal` writes the record):

```json
{"protocol":"fae-agent-v1","dispatch_id":"...","session_id":"...","task_id":"...","class":"output-refused","detail":"..."}
```

The class is `output-refused` (the provider's turn completed and its answer was
refused before any edit), `binding-refused` (before the provider was launched,
the bridge found it cannot run the turn's binding as configured),
`refused-before-provider` (refused before the provider was launched for any
other reason), `provider-not-started` (the provider never acknowledged the
prompt: its executable did not start, or it exited on its own before writing
anything to its event stream), `provider-failed` (the provider acknowledged
the prompt and then ended without an answer, failing on its own or stopped by
the bridge's timeout, before the bridge wrote anything) or `edits-not-applied`
(the answer passed every check, but the bridge could not write its edits or
handoff and restored the worktree). `runtime.Direct` accepts the record only with exit
status 65, no handoff, and a fresh, bounded, regular file that names this turn
and a known class, and then reports a `TurnError` of that class whose cause is
the `*runtime.BridgeRefusal`. Anything else stays `process-failed`. The same
holds under a Herdr pane, whose wrapper records the bridge's exit status, and
a resident worker, whose turn client exits with it.

Identity mismatch, id-less output, missing handoff, duplicate JSON keys, unknown
envelope fields, oversized files and symlinks are refused. Stdout/stderr are only
bounded diagnostic tails, never evidence. Engine schema validation remains the
final acceptance boundary. Output numbers retain exact JSON representations.

Cancellation and timeout kill only the process group created for this launch;
normal completion also cleans up descendants in that group. Bridges must not
escape the group with setsid or delegate to an unsupervised service. Session files
remain owner-only for diagnosis; durable indexing and retention are future work.
Each session runs the route the daemon selected from durable provider health
(the first declared route when none is set). The bounded session-log reader
classifies the provider outcome; a rate-limited, unavailable or auth-failed turn
with retired processes and an unchanged tree returns `provider-failover` so the
engine rotates once per turn. A binding the bridge refused returns
`binding-refused`, which moves the turn to the next route (below). Positive-liveness stall detection is in `runtime`.
A stalled turn is retried by default (FAE-RT-005) when its effective profile
passes `RestrictedEffects` (only handoff, read and edit, which the trusted
bridge enforces), its processes are retired, its tree is unchanged, observation
persisted and no earlier session of the dispatch executed; otherwise it stays
uncertain. `StallSafeReplay` asserts the restricted contract for every turn.
General CLI log discovery and interactive brokered command tools remain unimplemented.
Tests exercise actual subprocess launches, literal argv, secret stripping,
collection failures, cancellation, descendants, lease refusal/release, isolated
review sessions, current-tree checks and mutation rejection.

## Bridge refusals

A typed bridge refusal is classed by where the bridge stopped (FAE-WF-034,
FAE-RT-004, FAE-RT-009, FAE-RT-010):

- **`launch-refused`** (the runtime's own, `runtime.LaunchRefused`, not a
  bridge refusal): the runtime refused the launch before the bridge started,
  such as a failed session directory, configuration, role shim, brief file
  or pipe. No process ran and no provider is to blame, so no health is
  recorded and the start counts toward no binding's bound; with retired
  processes and an unchanged tree the class is `refused-before-launch`
  (FAE-RT-004, FAE-RT-009), a later review session's carrying the reviews of
  the sessions before it. A changed tree makes it `rejected-change`.
- **`output-refused`:** a completed turn's malformed report, like a handoff
  whose output fails the schema here. Provider health records malformed
  output, unless the provider's own log shows a more specific failure. With
  retired processes and an unchanged tree the class is `malformed-output`, a
  bounded stage retry; a changed tree makes it `rejected-change`.
- **`refused-before-provider`:** no provider ran, so no health is recorded.
  With retired processes and an unchanged tree the class is
  `refused-before-launch`, like a daemon-side scope refusal: it charges no
  attempt, and the fourth in a row at a stage escalates SameGate. A later
  review session's carries the reviews of the sessions before it. A changed
  tree makes it `rejected-change`.
- **`edits-not-applied`:** the provider ran and answered, and the failure is
  the bridge's (a failed write it undid). The provider is observed as having
  answered (ok, unless its log shows a failure). With the health written,
  retired processes and an unchanged tree the class is `validation-failed`, a
  bounded stage retry that spends the attempt the provider ran in; a changed
  tree makes it `rejected-change`.
- **`binding-refused`:** no provider ran, so no health is recorded, but no
  retry of the same binding can succeed either (another provider's binding, an
  effort Claude Code lacks, no credentials in the target environment). With
  retired processes and an unchanged tree the class is `binding-refused` and
  the result's `RefusedRoute` names the session's binding; the engine records
  it and moves the turn to the role's next route (FAE-RT-010, FAE-WF-082). It
  charges no attempt. A later review session's refusal carries the reviews of
  the sessions that ran before it; the engine refunds that dispatch too and
  runs every reviewer again. A `provider-failover` after earlier review
  sessions ran stays charged, since that provider ran. A changed tree makes it
  `rejected-change`.
- **`provider-not-started`:** the provider never acknowledged the prompt, so
  no health is recorded for the start and nothing the model could do ran.
  It is a non-execution proof like the transport's: for a dispatch's first
  session, with retired processes and an unchanged tree, the class is
  `startup-retry` (the detail names the cause), which the engine relaunches
  within the agent-start bound and then parks ProviderUnavailable against the
  binding (FAE-RT-004). A changed tree or unproven retirement leaves it
  uncertain.
- **A later review session's start failure** (this class, or the transport's
  non-execution proof, after earlier sessions of the dispatch ran): with
  retired processes and an unchanged tree the class is
  `later-reviewer-not-started`, carrying the reviews that ran;
  `kernel.CanRetryStartup` decides only a first session, and the engine checks
  that the reviews are the earlier sessions'. The session never ran, so the
  engine charges nothing, records none of the reviews, counts the start toward
  that session's binding's start bound and runs every reviewer again in fresh
  sessions; the binding's third failed start parks ProviderUnavailable against
  it (FAE-RT-004, FAE-WF-033). A runtime admission deferral of a later session
  is `infrastructure-deferred` with the reviews. A changed tree or unproven
  retirement leaves it uncertain.
- **`provider-failed`:** the provider acknowledged the prompt and ended without
  an answer, and the bridge wrote nothing. Provider health is recorded from
  the provider's log (malformed output when it holds no structured error; a
  rate limit, outage or auth failure there fails over as above). With the
  health written, retired processes, an unchanged tree and a profile that
  passes `RestrictedEffects`, the class is `malformed-output`, a bounded stage
  retry (FAE-WF-034). A changed tree, which means edits may have landed, or
  any of the others failing, leaves it uncertain.

The returned error is the bridge's detail without the session paths, so the
same refusal repeated escalates SameGate after the Appendix A repeat bound
(a binding refusal is never repeated on its binding: the engine skips it).
Unproven retirement, or a failed health write, leaves any of them uncertain. Any
other bridge failure after launch, including a failed edit write the bridge
could not undo or a provider stopped because the bridge was cancelled, stays
uncertain and parks.

Tests cover each class against a scripted runtime (unchanged and changed tree,
unproven retirement, an untyped failure) and through `runtime.Direct` with the
production bridge over a scripted Claude Code CLI:
- a schema failure and an unmatched replacement: malformed output, the
  provider charged, the tree untouched;
- a scope refusal and a failed repository listing: refused before launch, the
  provider never launched or charged;
- a Codex binding and missing Claude credentials: binding refused, naming the
  binding, the provider never launched or charged;
- a CLI that exits before acknowledging the prompt and an absolute CLI path
  that doesn't exist: a startup retry, nothing charged
  (`TestProviderNeverAcknowledgedRetriesThroughDirect`);
- a review whose second reviewer's CLI exits before acknowledging: the
  later-reviewer class with the first review, only the first reviewer
  observed (`TestLaterReviewerNeverAcknowledgedThroughDirect`);
- a CLI that announces its session and exits 3: malformed output, the
  provider charged, the tree untouched; uncertain if it wrote into the
  worktree or announced a command tool
  (`TestProviderFailedWithoutAnswerThroughDirect`).
