# Feature and capability fixes — 2026-11-07

This implementation pass includes the newer requirements in REQUIREMENTS.md, in
addition to the gaps recorded in the earlier feature audit. Requirements were
not rewritten. Changes remain in the working tree; no live service was deployed
or restarted.

Implemented capabilities include:

- Scheduler ordering, detached-child footprint accounting, bounded footprint
  analysis, candidate-specific admission, safe ledger migration, and dispatch
  using an existing grant without reacquiring the ledger lock. Concurrent
  ledger bootstrap now publishes an already-held lock so another opener cannot
  enter before initial state publication; established missing state still fails.
- Durable nested FanOut state, independent branch retries and checkpoints,
  failure joins, branch decisions, recursion budgets, and explicit per-item
  SubWorkflow inputs.
- Artifact aliases, indexed/default/lookup projections, optional references,
  presence sections, visit and loop references, outer-element filters, sibling
  deny expansion, and warnings for ineffective read allows.
- Offline virtual timing and capacity, pinned workflow catalogs, tree reports,
  structured failure/refusal reports, simulated change requests, and bounded
  seeded generation for supported regular-expression patterns.
- Selective filesystem isolation, persistent provider state, pinned runtime
  mounts, private caches, interactive heavy-operation accounting, streamed
  collectors, concurrent-write attribution, descendant output ownership, and
  cleanup of copies left by dead sessions.
- Isolation capability facts, admission diagnostics, and doctor reprobes.

The host reference ceiling retains its documented formula. Admission uses each
candidate's footprint; the numeric diagnostic can change as held work changes.
Offline timing models workflow sessions and role/tree caps; live host pressure
and operation pools require native execution. Fixture generation is bounded and
does not solve arbitrary combinations of JSON Schema constraints.

Verification includes ordinary package suites, race checks for affected runtime
and collector paths, and native Dolt-backed admission, branch, tree, ownership,
and scheduling regressions. The evidence census passes with 178 entries and 83
validator rules; it inventories evidence rather than accepting requirements.
The final run logs are retained under
`verification/results/capability-fixes-20261107/`.

Final verification: `go test -p 2 ./...`, `go vet ./...`, additive RPC-schema
validation, evidence census, source whitespace checks, and the staged build all
pass. Native Dolt regressions pass, including final selected-root admission,
locked-ledger dispatch, branch decisions across restart, and branch-local
schema retries. Bootstrap contention passed 20 repetitions; deterministic
bootstrap and missing-state controls passed 10 repetitions. The earlier failed
integrated run is retained as `all-before-contention-fix.log`; `all.log` is the
passing final run.

The staged executable is `bin/fae-capability-fixes-20261107`; the live executable
was not replaced. Later fan visits now use distinct readable identities for
children, change requests, and decisions while preserving first-visit IDs.

Actual namespace execution remains unverified on this host because bubblewrap
is unavailable. Automatic approval review rejected the updated installed-Claude
production-path conformance invocation before execution; no external provider
call was made by that attempt. Local subprocess and bridge tests do not replace
that acceptance check.

The [original audit](feature-capability-audit-2026-11-07.md) and saved reproductions
describe the earlier snapshot. Their old failure assertions are historical,
not claims that the same defects remain in this implementation.
