# Feature and capability audit — 2026-11-07

Historical snapshot: implementation subsequently addressed these findings and
the newer requirements. See [the fixes handoff](capability-fixes-2026-11-07.md)
for current scope and verification limits. Reproductions below describe the
pre-fix working tree.

This reviews the current working tree against REQUIREMENTS.md, especially the
2026-11-06 amendments. Three agents reviewed engine/scheduling, workflow and
interfaces, and provider/runtime capabilities. This is a feature audit: the
question is what an operator can successfully author, rehearse, run and inspect.
It is not a security assessment or a certification that all requirements pass.
No implementation or requirement changes were made by this audit.

## Capability coverage

| Capability | Current state | Practical boundary |
| --- | --- | --- |
| Typed signatures, defaults and input bindings | Implemented, with scripted/native coverage | Configured recursion ceilings do not work consistently through backlog submission. |
| SubWorkflows, detached children and self-recursion | Implemented, with gaps | Mixed-document recursion budgets are checked incompletely; arbitrary detached work is not incorporated into the holder's footprint. |
| Dynamic, parallel and nested FanOut | Implemented, with composition limits | HumanDecision remains disallowed inside fan regions; offline simulation does not rehearse real timing or scheduler capacity. |
| Failure artifacts, handlers and stage resume | Actual engine paths and tests exist | Production provider-session resumption still depends on a usable HOME and end-to-end CLI verification. |
| Artifact-only output, staging and scratch | Implemented, with runtime gaps | A separate read allowlist can reject an explicitly authorized generated output during collection. |
| Stage briefs, projections and gates | Implemented, with expression gaps | Nested element filters cannot address the enclosing element. |
| Tree/task role caps and live tree limits | Actual admission and operator-update paths exist | The separate host workflow-slot queue has ordering and stale-entry bugs. |
| Host workflow slots and pinned footprints | Implemented, with correctness gaps | Detached costs, superseded resize waiters and FIFO eligibility need fixes; footprint analysis is not adequately allocation-bounded. |
| Tree reads, stats, events and run_stats | Implemented, with reporting gaps | Recursion-refusal reporting uses an event name the engine does not emit. |
| Real agentic file and shell tools | Partial and unverified end to end here | Production HOME/TMPDIR mount conflict; config-directory auth missing; interactive heavy-operation shells explicitly unsupported. |
| Rule scenarios and seeded generated outputs | Implemented for supported schemas | Valid pattern-constrained strings may require a manually scripted literal instead of automatic generation. |

“Implemented” means substantive code paths exist, not merely schema fields.
Passing scripted tests does not establish installed-provider operation.

## Highest-impact gaps

### 1. Production provider HOME and TMPDIR are overmounted read-only

**Impact:** agentic sessions cannot reliably create provider state, temporary
files or persistent session history in their configured locations. This blocks
confidence in normal launch and checkpoint/session resume (RT-015, RT-019).

`runtime/direct.go:419` sets both variables to the session log directory.
`provider/session_view.go:151` initially mounts a writable private HOME there;
`:191–202` subsequently mounts the same log directory read-only. The provider's
production environment therefore differs materially from the direct conformance
test harness.

**Evidence:** confirmed environment and mount-order conflict in source. A real
namespace launch was not possible on this host. Separate writable provider
home/temp from log paths, then verify the production launch and resume path.

### 2. Interactive build/test/format role capabilities are explicitly unsupported

**Impact:** an agentic role with one of these declared capabilities refuses
before provider launch, even on a host with bubblewrap. Users cannot perform
those operations interactively within the role's agentic shell.

`provider/session_view.go:42–52` rejects non-accounted shim policies with
`Build`, `Test` or `Format`. The missing integration is host process ownership
for heavy operations inside the private PID namespace. Accounted HostCommand
stages are available but require a different workflow and do not provide the
interactive capability. This is an explicit implementation limitation, not an
environment-only skip.

### 3. Independent input and output scopes fail collection

**Impact:** a stage reading `inputs/**` and explicitly writing `gen/**` cannot
keep a newly generated `gen/new.yaml`. This prevents a natural artifact or
document-generation workflow using separate input/output scopes (RT-017,
SEC-011).

`provider/session_view.go:395–403` requires changed files to pass the read
allowlist before checking write permission. An isolated reproduction fails with
`session wrote denied path gen/new.yaml`. Read visibility and authorized
writeback need separate treatment, including source-version checks for blind
writes.

### 4. Config-directory authentication is accepted but unavailable in the view

**Impact:** a target using only `CLAUDE_CONFIG_DIR` can pass preflight but start
without its authentication files, unless another unrelated grant happens to
make them visible.

`provider/claude.go:37–60` accepts this authentication mode.
`provider/session_view.go` constructs a minimal filesystem view without mounting
that directory. Support the declared credential mode explicitly or refuse it
with a clear capability error before launch. This finding is based on source;
no account credentials were read or provider requests made.

### 5. A superseded migration can strand the workflow-slot queue

**Impact:** unrelated workflows can remain waiting despite enough host capacity.

A holder at revision 1 requests a larger revision 2 footprint and waits. It then
successfully migrates to revision 3, which fits its existing reservation. The
existing-holder early return at `scheduler/workflow_slots.go:337–347` leaves the
obsolete revision 2 waiter in the ledger. Later acquisitions see that phantom
FIFO head. Reproduced with temporary overlays (SCH-021, WF-093).

### 6. Host workflow-slot priority is reversed; eligibility exemptions go stale

**Impact:** the scheduler can grant lower-priority work ahead of higher-priority
work. Even at equal priorities, a younger waiter can bypass an older waiter
whose molecule limit has cleared.

`waiterBefore` at `scheduler/workflow_slots.go:297–304` orders priority ascending,
while SCH-003 requires larger numbers first. A queued priority-100 holder is
bypassed by priority 10 in the reproduction. Separately, `IgnoreFIFO` set at
`:456–459` is not recomputed when another holder releases; the older waiter's
exemption remains until its next retry (SCH-021).

### 7. Detached workflows do not contribute their own dynamic footprint

**Impact:** an expensive detached child under an unbounded, cheap root can start
on a holder reservation sized only for the root. The host workflow-slot layer
therefore does not implement the promised spawn-capacity decision for this case.
Other per-launch guards are separate and do not repair this accounting gap.

`daemon/workflow_slots.go:110–150` walks to the root and prices its footprint,
without incorporating an undeclared child's obligations. A child with effective
footprint 1,000 is priced as one session in the isolated reproduction
(SCH-019/020/022). Dynamic repricing or an enforceable family bound is needed.
Children inheriting a molecule parent's slot is itself intended by SCH-022.

## Workflow authoring, rehearsal and reporting gaps

### 8. Mixed-budget mutual recursion is admitted

**Impact:** a workflow author cannot rely on the promised “every document in the
cycle declares recursion” rule (WF-100).

Four valid documents `A → B → C → D → A`, with recursion declared by A/B/D but
not C, admit A at depth 5 and B at depth 6 before eventually stopping later.
`daemon/tree_admission.go:225–244` and `engine/scenario.go:111–119` do not inspect
every intermediate document in the cycle. Offline behavior was reproduced;
the corresponding native admission logic was inspected. Check the entire cycle
segment when the ancestor workflow is encountered.

### 9. Backlog submission ignores a configured recursion ceiling

**Impact:** a workflow installed successfully with depth 9 under instance ceiling
12 can fail when submitted through the backlog path.

`daemon/backlog.go:189` calls package-level `validWorkflow`, which invokes
`workflow.Parse` with the default ceiling 8 (`daemon/rpc.go:89`), rather than the
daemon-context parser. The reproduction confirms configured parsing succeeds
while the backlog-equivalent validator rejects `recursion.max_depth` as outside
1..8 (WF-100).

### 10. Large workflows can exhaust memory during footprint derivation

**Impact:** otherwise modest workflow documents can impose excessive validation
memory and prevent practical authoring/admission of large graphs.

`workflow/footprint.go:115–119` and `:289–293` copy the full visited map at every
path depth. The 100,000-step bound does not count those allocations. A 1,800-stage,
140,844-byte graph allocates approximately 127 MiB in the reproduced derivation;
the problem grows quadratically. Use a backtracking visited set and account for
analysis depth/allocation work (SCH-020, ROB-020). This was measured at the
footprint entry point, not by forcing a daemon OOM.

### 11. Automatic rehearsal output does not cover all accepted schemas

**Impact:** an unmatched turn with a valid schema such as a string matching
`^[A-Z]{3}$` fails generation even though the literal `"ABC"` validates. The
operator must supply a literal rule instead of receiving the promised generated
schema-valid output (WF-105(4)).

`workflow/fixture.go:14–29` tries a bounded family of generated values, and its
string generation does not synthesize this simple pattern. Both offline and
native rule scenarios use this generator through
`engine/scenario_scripted.go:125`. This is a partial capability, not a claim that
it silently records invalid evidence.

### 12. Offline simulation does not rehearse scheduling or delay ordering

**Impact:** a successful offline run does not show that parallel areas finish in
the expected order/time or obey real host/tree/role capacity.

`engine/scenario.go:403–404` executes prepared dispatches synchronously with
scenario delay disabled. The native scripted daemon supports actual delays and
is the available route for concurrency rehearsal. Treat this as a product
boundary rather than asserting that every offline timing feature is required.

### 13. HumanDecision does not compose inside FanOut branches

**Impact:** an operator cannot pause one area/branch for a HumanDecision while
other branches continue, including nested and per-item workflows.

`workflow/fan.go:377–378` rejects HumanDecision in a branch region. Top-level
per-item decisions exist. This is a composition limitation; the interaction
between the original fan exclusions and the newer broad nested-region language
needs a product decision before treating it as a strict WF-113 violation.

### 14. Nested element filters cannot reference the enclosing element

**Impact:** an author cannot express a generic relation such as “every requested
ID has a matching component object” with nested element counts (WF-060 amendment).
Artifacts must be preprocessed into a different shape as a workaround.

`workflow/gate.go:309–312` replaces the sole `item` binding while evaluating the
inner filter. The AST has no binder or outer-element selector. A reproduction
with a missing requested ID nevertheless returns true when the two intended
inner/outer `item.id` operands both resolve to the inner component. Adding full
task-evidence lookup did not implement this separate outer-element capability.

### 15. Tree reads omit actual recursion refusals

**Impact:** `fae tree` and Tree API consumers see an empty `recursion_refusals`
list even when the engine has recorded the refusals (API-035, CLI-055).

`daemon/tree.go:135–138` matches an exact hyphenated `recursion-budget` kind or
cause. Actual engine refusals are `sub-workflow-failed`, `failure-route` and
transition events whose causes contain `recursion_budget`. The reproduction
uses an actual `ChildFailed` result: all three emitted events miss the Tree
filter. The data exists, but the promised structured view does not expose it.

### 16. Nested work is not rolled up under its top-level stage

**Impact:** a results stage or `fae tree --stats` consumer cannot obtain the
promised top-level cost/turn/wall breakdown for nested parallel work (API-035,
WF-120).

An actual nested dispatch reproduction labels inner branches `FanStage=inner`
and the sibling `FanStage=outer`. `daemon/tree.go:182–195` buckets directly by
that innermost fan stage, leaving inner work outside the outer stage's totals.
HostCommand timings at `:213` similarly use the raw stage. Preserve the top-level
ancestry for aggregation while retaining the inner detail.

### 17. Offline validation cannot resolve hierarchical footprints

**Impact:** an author validating a parent containing a pinned SubWorkflow cannot
get its required derived/effective footprint before installation using the
current offline command (SCH-020).

`cmd/fae/workflow.go:99` invokes content validation with no child resolver.
`daemon/workflow_governance.go:86–92` can return `valid: true` with
`footprint_error` instead of a footprint. No catalog-backed authoring-validation
flow is exposed. This is a partial authoring experience, distinct from daemon
installation/admission paths that resolve installed children.

## End-to-end acceptance boundary

The installed-Claude test remains insufficient evidence of production support:
`provider/installed_claude_filesystem_test.go:44` bypasses the actual production
launch environment/settings/shims. Its assertions do not require every requested
negative operation and corresponding failed result. A pass would not establish
all promised tool behavior or catch the HOME mount conflict above.

The operator-approved test was invoked in the prior pass but skipped before any
provider request because bubblewrap is unavailable. This connection has no
registered `hc` remote machine. Real-provider behavior remains **unverified**;
the explicit heavy-shell refusal is **unimplemented**, and the HOME/config/scope
issues are **implementation defects**. These are different statuses.

Checkpoint integration was inspected: private edits are collected before periodic
snapshot/save and after provider termination (`provider/codex.go:632`, `:907`).
No additional quota/budget edit-loss issue was confirmed.

## Reproduction artifacts and next acceptance work

Local probes are preserved under
`verification/audits/requirements-2026-11-07/`. Run:

```sh
python3 verification/audits/requirements-2026-11-07/reproduce.py
```

This uses temporary Go overlays and synthetic files. It calls no model, starts
no daemon and modifies no live ledger. Capacity probes assert the observed bugs;
the independent-scope probe asserts required behavior and is expected to fail
on the audited code. Output records distinguish those conventions.

Prioritize the production provider launch path and interactive heavy-shell
capability, then the scope/generation path, then capacity/migration fairness and
hierarchical workflow/rehearsal/reporting gaps. Acceptance should exercise an
actual installed provider through the same production environment and adapter,
followed by a scripted multi-area run covering generated files, children,
failures, resumption and tree stats. Passing unit suites alone is insufficient.
