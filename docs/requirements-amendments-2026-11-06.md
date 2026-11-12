# November 6 requirements implementation

This change implements the new requirements and amendments in REQUIREMENTS.md.
It leaves their proposal/acceptance markings unchanged. It does not deploy or
restart a live instance, and does not modify existing task records.

## Implementation areas

| Area | Requirements | Implementation and acceptance evidence |
| --- | --- | --- |
| Workflow data and children | WF-099–104, WF-106, WF-114, WF-117, API-034, CLI-056 | Pinned signature inputs and defaults, aggregate admission refusals, required outputs, recursive/self and detached children, hierarchical IDs and ancestry. `engine/tree_signature_test.go`, native daemon tree-admission tests, `daemon/scenario_test.go`. |
| Failure recovery | WF-034 amendment, WF-112, OPS-035 | Schema retries and rejection feedback, typed failure artifacts, handler replacement binding, guarded stage resume with passed-branch retention. `engine/recovery_extensions_test.go`, protected review-receipt regressions. New writes pin schema retry default 1; old stored documents retain read compatibility. |
| Branching and joins | WF-096 amendments, WF-113 | Persisted nested frames, sibling isolation, ordered list joins, settle, session cancellation, worktree integration before post-join execution. `engine/recovery_extensions_test.go`, `daemon/fan_cancellation_test.go`, `daemon/nested_fan_worktree_test.go`. |
| Operator data | WF-103, WF-118, CTL-108 | Audited artifact supply, complete per-item decisions, durable ancestor requests and dispositions, explicit forwarding without target-turn delivery. `daemon/change_requests_test.go`, engine tree/signature and scenario tests. |
| Filesystem and execution | WF-104, WF-111, WF-115–116, WF-119, CTL-107, SEC-011 and amendments to WF-035/WF-095/SEC-008 | Artifact output/staging directories, retained scratch, generator trees and capture, typed briefs/argv, shell restrictions and intersected write scopes. Native provider, execution, effects, runtime and nested-worktree tests. |
| Scheduling and observability | SCH-024–025, WF-120, API-035–036, CLI-055, OBS-008–009 | Scoped role caps, FIFO tree session budget, tree reads/stat snapshots, filtered events, provider-reported or absent attempt statistics, bounded content-free tool events. Native tree, migration, telemetry and race tests. |
| Rehearsal | WF-105 | Scenario rules select stage, role, task glob and branch; actual child jobs execute with the same budgets, decision and request paths, with seeded schema-valid fallback. Native/offline scenario parity tests. |

## Validation and operating notes

The affected engine suite includes 10,000 generated workflow cases. Native
tests use real Git worktrees and, where required, a private Dolt instance.
New revisions must declare an array output for every FanIn; stored revisions
retain read compatibility. Scenario catalogs can pin multiple revisions using
keys such as `child` and `child@2`.
Broker tests must use a disk-backed TMPDIR; the broker deliberately refuses
RAM-backed compiler output. See `docs/tree-observability.md` for tree and
request commands, limit changes, statistics semantics and restart-only policy.

The recursion ceiling defaults to 8; the subsequent
[requirements follow-up](requirements-followup-2026-11-06.md) adds
`serve --recursion-ceiling N` (1–64). Seeded scenario generation validates every generated
value and refuses schemas it cannot satisfy rather than inventing invalid
evidence. Existing proposal markings are not a claim that every acceptance
case has been independently certified.

Final verification passed the complete engine, workflow, daemon, CLI and
controlplane suites; complete provider, execution, runtime, controller and
disk-backed effects suites; focused race and native Git/Dolt checks; Go vet;
and the acceptance-ledger census. The census has 171 mapped checks and no
unknown native paths for those checks; it does not certify whole requirements.
The separately built executable is `bin/fae-requirements-20261106`.
