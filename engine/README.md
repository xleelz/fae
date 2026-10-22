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
`Sequence` independently counts all dispatches. Loops, test runs, malformed-output
retries and dispatch recoveries have separate durable counters. Planned safe
scripted replay keeps the exact dispatch/session identities and charges no budget.
Unknown executions park with uncertainty; they are never silently redispatched.

The default content covers the local scripted pipeline: intake, bounded spec
review (two isolated sessions), planning, RED/right-failure/freeze, implementation,
GREEN/frozen comparison, formatting, four isolated review dimensions, typed oracle
routing, QA, reconcile routing, post-merge QA, commit and integration. Every gate
records its evaluated leaves and evidence IDs. Review input bindings exclude
sibling results. The CodeBearing graph check rejects paths missing the RED chain
or a passing unchanged-frozen gate.

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
remain outside this pure package. Live recovery always parks uncertain dispatches,
even if a caller requests scripted replay. No Normal-mode external
sub-workflow runs; external-effect stages fail closed. Fan-out, fan-in and nested
workflows remain rejected by the validator. HumanDecision remains reserved in
validation. No-red classification, scope-widen governance, operator resolution of
uncertainty, audited attempt-limit changes and broader controller behavior remain future work.
Live dispatch now supports durable provider/model route selection and changed-base
revalidation. Provider health affects new dispatches only; no eligible route parks
without consuming an attempt. Typed no-launch infrastructure deferrals also park
and release their attempt charge; a partially executed review remains uncertain. Gate results currently live in durable history,
not separate content-addressed artifact rows. Recovery tests prove persistence
round trips and explicit safe rehearsal replay, not arbitrary real-effect replay.

Tests include 10,000 generated graph executions with mid-stage and stage-boundary
serialization/recovery, full default parity, malformed-output bounds, wrong-token
and wrong-tree refusals, missing-input parking, retry routing, RED identity
mutation, zero tests, QA/review failures, collapsed review dimensions, negative
integration and artifact provenance mutation. Gobra's checked completion guard is
called by `Complete`; the proof scope is documented under `verification/`.
