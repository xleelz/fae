# Gate AST, version 1

Gates are JSON syntax trees, not executable strings. `workflow.Parse` rejects unknown object keys. `ValidateExpr` checks operation names, arity and selector requirements. Gate results are three-valued: only `true` passes. `false` and `unknown` fail.

```text
expression := literal | lookup | unary | binary
literal    := {"op":"literal", "value": JSON-value}
lookup     := {"op":"field", selector, "path": [string, ...], ["all":boolean], ["enum":[string,...]]}
            | {"op":"exists", selector}
            | {"op":"count", selector, ["args":[expression]]}
            | {"op":"count", selector, "path": [string, ...], ["all":boolean], ["args":[element-expression]]}
item       := {"op":"item", ["path": [string, ...]], ["enum":[string,...]], ["item_depth":integer]}   (only inside an element count's filter)
selector   := "type":string | "stage":string | "type":string,"stage":string
unary      := {"op":"not", "args":[expression]}
binary     := {"op":binary-op, "args":[expression,expression]}
binary-op  := "and" | "or" | "eq" | "ne" | "lt" | "le" | "gt" | "ge" | "prefix" | "in"
```

`field` selects the most recently recorded matching evidence by slice order, or all matches when `all` is true. Field paths traverse object keys. `count` returns the number of matches; its optional filter is evaluated with one matching record in scope. A `count` with a `path` is an element count: it counts the elements of the array at that path in the most recent match (every match when `all` is true), and its filter reads the current element through `item`; a missing or non-array field is unknown. `item` reads the innermost element by default. `item_depth: 1` reads its enclosing element, `2` the next enclosing element; the depth must name an existing element-count scope. Element counts let gates and branches judge typed findings inside one artifact, for example "no finding of this review is blocking". Unknown filters make the whole count unknown. `exists` returns false for an empty selection. Missing fields, missing field-selected artifacts, unrecognized declared enum values, invalid operations and type mismatches return unknown with a reason. Comparisons support exact decimal numbers; strings and booleans support equality, strings support prefix, and `in` accepts an array-valued expression, including a lookup of another artifact or task input, with same-typed elements. Lookups within an element filter retain the task evidence selection.

Boolean operations use strong Kleene logic: false AND unknown is false; true OR unknown is true; otherwise an unknown operand propagates. Both operands are evaluated for an audit trace. Results record every leaf and the sorted distinct evidence IDs read.

Expression nesting is limited to 64 and evaluation to 10,000 node visits. Number representations/exponents have a 10,000-character/magnitude bound. The language has no recursion, user functions, I/O, clock, random values or executable loops.

The validator conservatively requires a positive-evidence witness for each referenced collection on every passing disjunct. Tests must additionally assert `exit_code == 0` and `executed_count >= 1` on their named host-command stage, and any gate that reads test-run evidence, named `test_stage` or not, must bound `executed_count >= 1` for each such selector as a top-level conjunct. This restricted proof rule can reject otherwise safe complex expressions; it never assumes an arbitrary predicate implies non-emptiness. An emptiness waiver needs both `empty_allowed: true` and a nonempty `empty_reason`, and cannot waive test-run counts.

The AST does not implement schema-derived field typing or array indexing. The default workflow content lives in `defaults/` (Appendix C); see its tests for the routing and floor corpus.

## Durable interpreter extensions

Documents may declare `code_bearing: true`. Such graphs must traverse test-first
phase, a run of a declared test shape, a positive RED/right-failure gate,
freezing, implementation, comparison, a gate asserting that comparison is
unchanged and a gate asserting a post-implementation run passed the RED test
(`red_passed`) before committing/integrating. `phase` is closed to `test-first`
and `implement`; every change producer of a code-bearing graph must declare one,
and every change producer other than test-first is treated as an implementation.
HostCommand test runs may declare `test_shape`: `go-unit` (exactly the task's
pinned Go test), `go-integration` (that test in exactly one named target
package), `rust-unit` (one exact Cargo/libtest test), `python-pytest` (one exact
pytest node ID), `node-test` (one exact built-in Node test), `script` (a dedicated `./` script inside the worktree) or `runner` (a
declared runner program); script and runner receive the pinned identifier as
their final argument and must report TAP (FAE-WF-036). Role brief keys are `default` or a phase declared by a stage of
that role, and every AgentTurn/Review stage must resolve a non-empty template. Review stages support `reviewers`
(role IDs) and `count` (1–16, omitted means 1); each gets an isolated session.
Branch stages support ordered `cases` containing `when` expressions and `next`
edges, plus the required default edge. Freeze/Compare use `subject` to identify
the source artifact producer. Escalate stages may declare `kind`, one of the
closed Appendix B escalation kinds (omitted means `HumanDecisionRequired`); the
validator refuses an unknown kind or `kind` on any other primitive. These fields
are parsed strictly like existing fields. Stage outputs must be permitted by their declared role(s).

Each role declares its ordered `routes` (FAE-WF-041, FAE-WF-082): bindings of a
`provider`, a `model` (both required) and an optional `effort`, one of `minimal`,
`low`, `medium`, `high` or `xhigh` (omitted leaves the provider's default). The
engine dispatches a turn on the first eligible route and fails over down the list.
Claude Code has no `minimal` effort, so a `claude` route with effort `minimal`, or a
task-class overlay that applies `minimal` to a role with a `claude` route, could
never run. A write refuses it: `WorkflowInstall` and `workflow validate` apply
`workflow.RouteEffortIssues` (which `ValidateWrite` also includes). `Validate`
doesn't, so a revision stored before the rule existed still loads: tasks can be
admitted on it and it can be read, diffed and simulated. At run time the Claude
bridge refuses such a binding before Claude starts (a binding refusal) and the turn
moves to the role's next route. Other providers keep `minimal`.

Policy sections (all optional, parsed strictly):

- `task_classes` and `default_class` (FAE-WF-001, -044): a class may be
  `code_bearing` and may declare per-role `overlays` replacing the effort of
  every route binding and named brief templates (keys as for role briefs). A
  code-bearing class puts the whole graph under the RED floor above; the legacy
  `code_bearing: true` document flag makes every class code-bearing. A task's
  class is pinned at admission (`task_class`, else the default class).
- `no_red_stages` (FAE-WF-073): pre-code AgentTurn stages (never a change
  producer, never a change-producing role, unreachable from any change
  producer) whose artifact may declare `no_red: true`. Only a Branch case that
  conjunctively asserts that declaration may reach implementation or commit
  without the RED sequence; the target's diff classifier enforces it at commit.
- `requests` (FAE-WF-078): `from` stages whose artifact may carry a typed
  `request` (`scope-widen` with `files`, or `red-rework`, each with a
  `reason`), a `judge` AgentTurn of a role other than every requester and
  change producer whose artifact has a boolean `approved`, a `widen` route, and
  optionally a `rework` route with a positive `rework_cap`. Each requester needs
  an edge to the judge and the judge routes only to those routes; the engine
  chooses both. Scope widens are capped by `limits.scope_widens`.
- HostCommand `bindings` (FAE-WF-035): each whole-argument `{{name}}`
  placeholder of `command` declares a closed `type` (`go-test-name`,
  `test-id`, `relative-path`, `go-package`, `text`, `integer`), a required
  `input` and an artifact `field`; a bound value is always one literal
  argument and never starts with `-`. The program itself is never a
  placeholder, and partial placeholders are refused.
- Loop `progress` (FAE-WF-075): a `count` `measure` of open blocking findings
  and a positive no-improvement `streak`; an entry that strictly reduces the
  measure is not charged an iteration. A streak past the bound escalates
  LoopCap; the detail names the streak and the bound, and that the streak
  carries across resume and restart.
- Role and stage `profile` `shims` and `effects` (FAE-WF-042): the command
  shims a session may run (the shim policy refuses any other before capability
  or lease checks; absent or empty leaves every shim, still bound by `build`,
  `test` and `format`) and the external-effect subjects the role holds grants
  for. A stage override may only tighten both. From schema 2 an ExternalEffect
  stage acts only under its role's grant; the Local projection drops grants
  only removed stages used.
- `target_requirements` (FAE-WF-001; required from schema 2): `commands`, the
  literal vectors of the HostCommand stages (exactly those), plus
  `diff_classifier` and `required_checks` flags. Pinning a target refuses one
  that lacks any of them.
- AgentTurn `follow_ups` (FAE-WF-084): an array `path` of the output and a
  `when` filter over `item`; each matching element is recorded as a durable
  `follow-up` event citing the artifact.

Optional policy hooks (FAE-WF-001) are not implemented: the requirements give
them no concrete shape.

## Schema versions (FAE-WF-030, FAE-WF-093)

`schema_version` 1 and 2 both validate, so every stored revision stays
loadable. Schema 2 (`workflow.CurrentSchema`) additionally requires each
Gate's `name`, each Escalate stage's `kind` and `reason`, effect grants and
`target_requirements`. `workflow.ParseWrite` and `workflow.ValidateWrite` refuse a schema-1 document.
New installation and offline/API authoring validation apply `ParseWrite`,
including all write rules, while stored reads keep `Parse`/`Load` compatibility. Rules added after revisions were stored that only keep new
content runnable go in `ValidateWrite`, never `Validate`, which `Parse` runs on
every stored revision. A stored revision falls in one of two classes
(FAE-WF-093, operator decision of 2026-10-30). Whether a binary can read it
at all is `workflow.Load`: canonical JSON, no unknown or mistyped field and a
known schema version; the daemon refuses to start on a revision that fails
it. Whether the binary can run it is `Parse`: the structural rules the engine
dereferences (a Gate's `expr`, the start and edge endpoints, primitives, Loop
caps) and the rules added since it was stored (brief keys, caps, floors).
`Load` carries none of them, on purpose: a document the binary can read is
never called unreadable, and the daemon never drives a task on one that
fails `Parse`. It refuses to start while an active task is pinned to such a
revision (the old default's `main` brief keys, say), naming it and the
recovery, or quarantines those tasks under `serve
--quarantine-unrunnable-revisions` so they can be migrated or retired; a
revision no active task is pinned to is listed, and admission and migration,
which `Parse`, refuse it. The shipped default is schema 2.

## Simulation (FAE-WF-091)

`engine.Simulate` is the durable engine itself driven offline on a scripted
adapter, so a simulated path is the engine's: same-gate repeats, progress
discounts, request routing and caps, protected evidence and every floor apply.
Scripted records stand in for role outputs and host-command results; a step's
`next` may select a Merge's reverify route (a changed base), an Integrate's (a
moved target) or a SubWorkflow's failure route. `TestSimulatorAgreesWithParityCorpus`
replays the parity corpus through it.

Artifact schemas are compiled with JSON Schema (2020-12 by default), supporting
local references and declared older drafts. Invalid schemas are rejected during
workflow validation. Output JSON is canonicalized (including duplicate-key
refusal) then schema-validated. Schema compilation cannot fetch network or file
resources: its loader always refuses external references. The compiled-schema
cache is bounded and performs no I/O.

## Primitive configuration (FAE-WF-030)

Fields belonging to one primitive are refused on every other primitive, so a
misplaced key never silently does nothing. Fields added here are optional in
the JSON encoding of every other stage, so existing revisions keep their
content hashes.

**HumanDecision** declares `question` (non-empty, at most 4096 bytes),
`options` (1 to 32, each `{"id", "next", "description"?}` with a unique id of
letters, digits, `-`, `_` or `.`, routed to an outgoing edge; every outgoing
edge must be some option's route), `on_timeout` (required, and only
`"stay-waiting"`: a decision is never taken automatically) and `output`, the
decision artifact type. It takes no role and produces no change. On arrival the
engine parks the task with reason `HumanDecisionRequired`, raising that kind on
the park event, and leaves it waiting (restarts included) until `fae decide`
names an option. The decision artifact has the fields `decision_id`,
`question`, `option`, `authorizing_message`, `decided_by` and optionally
`rationale`; it must satisfy the declared type, and is recorded with operator
provenance (role `operator`, session and dispatch id = the decision id) before
the task moves along the option's route. Every option route is part of the
graph, so the protected-commit and other floors are checked on it like any
other path. The simulator takes `{"decision": "<option>"}` script steps for the
stage and stops with reason `HumanDecisionRequired` when a visit has none.

**Escalate** may declare `reason`, a template of at most 1024 bytes in which
`{task}`, `{stage}` and `{attempt}` are substituted as data (never
re-expanded); any other brace is refused. It becomes the escalation detail
after the kind; omitted (schema 1 only), the detail is `workflow stage <id>`.
Schema 2 requires it, and the `kind`.

**Review** may declare `aggregation`: `all-pass` (all N isolated reviews
present and every `approved` is true), `k-of-n` with `quorum` k between 1 and N
(at least k approved), or `any-blocking-fails` (no review has a finding whose
`severity` is `blocking`; a severity other than `blocking`/`non-blocking` or a
missing `findings` list is unknown). N is the number of reviewer roles times
`count`. Every rule first asserts that all N reviews are present. An aggregated
Review requires a `failure` edge: after collection the engine records the
aggregate verdict and routes a failed or unknown verdict to it. On that route
the validator confers no independent-review fact. Without `aggregation` a
Review keeps its single success route and gates judge it, as before.

**Loop** may declare `exit_gate` (a Gate whose `failure` route is this loop),
`backoff_seconds` (0 to 86400: before each repeated iteration the task parks
with reason `LoopBackoff` and resumes once the delay has elapsed; the first
entry never waits; the offline simulator has no clock and does not wait) and
`on_exhaustion` (the Appendix B kind raised when `max_iterations` is exhausted,
default `LoopCap`, or `IntegrationConflict` for an Integrate reverify loop).
The exhaustion detail names the loop, when declared its exit gate, and the
iterations used against `max_iterations`; the count carries for the life of the
task, across resume and restart, and the detail says so.

**HostCommand** may declare `expected_shape` with `exit` (`zero` or `nonzero`),
`executed` (exact) or `min_executed`, and `matched` (exact `matched_count`,
for example exactly one matched test). At least one is required, a zero value
means unset, and `executed` and `min_executed` are exclusive. A shaped command
requires a `failure` edge: the engine records the shape check over the run it
just collected and routes a mismatch (or a missing field) there.
`check` (`FAE-WF-030`, `FAE-WF-095`) declares a structured check command:
`format` (`"json"` or `"lines"`), optional `output` (`"stdout"` by default or a
safe relative file path in the worktree), `count_field` (required JSON field/path
for `"json"` or synthetic line-count field for `"lines"`), and optional `writes`
(safe relative path globs that the check command may write during execution).
Before execution the broker verifies that `writes` globs are disjoint from the
task's declared editable files; after execution paths matching `writes` are
exempted from the worktree mutation check and restored/cleaned so they are never
staged or committed into git. Any Gate or non-failure Branch arm that reads a
check stage's artifact must assert a positive lower bound (`>= 1` or `> 0`) on
`count_field` (`FAE-WF-062`).
`environment` is a map of at most 32 explicit `NAME: value` settings applied
over the broker's sanitized base. Nothing else is inherited from the daemon.
Names are upper case. The base variables (`PATH`, `HOME`, `TMPDIR`, `LANG`,
`LC_*`), `GIT_*`, `FAE_*`, loader (`LD_*`, `DYLD_*`), provider and cloud
prefixes, and credential-like names (containing `TOKEN`, `SECRET`, `PASSWORD`,
`CREDENTIAL`, `KEY` or `AUTH`) are refused.

**AgentTurn** and **Review** may declare `profile`, a tool profile that may
only tighten each of the stage's roles: allowed capabilities must be a subset,
every role denial must be kept, and build/test/format may only turn off. The
runtime launches with the effective profile (allowed intersected, denials
united, permissions conjoined).

**Gate** declares `name` (required from schema 2), a printable human-readable
name of at most 128 bytes, recorded with each of its results.

**FanOut** declares either static `branches` (2 to 16, each
`{"id", "start", "pass"?, "verdict"?, "write_scope"?}`) or runtime-derived
`branches_from` (`{"stage", "field"?, "min"?, "max"?, "empty"?}`, with `max`
between 1 and 256, default 256, and `empty` required as an outgoing stage edge
when `min == 0`) paired with a single branch template (`branch_template` or a
1-element `branches` list), plus optional `parallel: true`, optional
stage-level `write_scope`, and `join`, the FanIn that joins them (`FAE-WF-096`).
A FanOut's outgoing edges are its branch starts (plus `branches_from.empty` when
declared). A branch's region is every stage reachable from its start without
passing the join, the FanOut or a kernel state; Escalate stages escalate the
whole task and belong to no branch. Regions are disjoint, are entered only from
the FanOut, leave only through the join (or an Escalate stage), and the join is
entered only from them. A region holds no VcsCommit, Merge, Integrate,
ExternalEffect. A region may contain nested FanOut/FanIn, SubWorkflow and
HumanDecision stages. Branch decisions park only their own branch; independent
siblings continue. Inbox exposes each pending decision with its stable decision
id and hierarchical branch path. Use `fae decide --task ID --decision ID` or
`--branch PATH` when more than one question is pending; an unqualified ambiguous
decision is refused. Per-item decisions still require every projected item.
A region may
contain `change_producer: true` stages if and only if each branch declares
disjoint `write_scope` globs (pairwise-disjoint static globs, or parameterized
globs such as `"pkg/{{item.pkg}}/**"` verified pairwise disjoint and within the
task's declared files at runtime before dispatch) and each change-producing
branch satisfies the verification and review floor (`TestRun`/`Check` and
`Review` in the branch or after the join before commit). When `branches_from` is
set, each array element is bound as structured `item` data (`FAE-WF-043`) into
its branch (`branch-0 .. branch-(N-1)`). When `parallel: true` is set, ready
branches may execute concurrently (`PrepareBranch` / `CompleteBranch`), and
change-producing branches execute in per-branch forked git worktrees whose
commits are integrated in deterministic branch order (`0..N-1`) into the parent
task worktree at `FanIn`. A region stage may not bind an input from, or read in
a gate or branch, a sibling branch's stage. `pass` (or `verdict`) is an optional
branch verdict, a positive-evidence gate expression over the branch's evidence
recorded as a `branch-verdict` gate record when the branch reaches the join.
Without it, a branch passes unless it arrives by a failed check's failure
route (a Gate, aggregated Review or shaped HostCommand whose `failure` is the
join).

**Document Landings (`FAE-CTL-052`)**: A target configuration (schema 4) may
declare `document_globs` and `document_checks` (`{"id", "command", "format",
"output"?, "count_field", "writes"?}`). When a task's changed files are
non-empty and every changed file matches `document_globs`, passing
`document_checks` runs (each exiting 0 with `count_field >= 1`) plus any
`required_tests` satisfy the protected-commit test-run floor and the landing
required test filter, formatting, compile and suite gates; touching any path
outside `document_globs` or reporting a zero count requires/holds the full code
landing gates.

**FanIn** declares `rule`: `all`, `any`, or `k-of-n` with `quorum` k between 1
and the number of branches. It also requires a `failure` edge, taken when the
join fails. Exactly one FanOut names it.

Execution: the engine keeps one dispatch in flight per task, so the logically
parallel branches run one after another. The FanOut records durable fan state
on the job (`fan`: stage, join, visit and each branch's
pending/running/passed/failed/skipped status) and enters the first branch. Each
arrival at the join records that branch's outcome. The join then enters the
next pending branch (a content-derived re-entry edge from the FanIn to a branch
start of its own FanOut, accepted by `CanTransition`), or, once the rule is
settled, marks the unrun branches `skipped` (cancelled), records the `join`
verdict event with one leaf per branch, clears the fan state and routes to its
success or failure edge. While a branch runs, the evidence of sibling branches
is hidden from its bound inputs, gates and verdicts (FAE-WF-033 isolation).
After the join every branch's evidence is visible. Leaving the fan without its
join (an escalation restart) abandons the fan state; a migration inside a
fan-out is refused. Because branches hold no change producers, facts
established in a branch persist across the join, and the validator's per-path
floor check is sound but conservative: a test gate in one branch and a review
in another never combine into commit evidence. The runtime protected-commit
check still applies at the commit, so a passing join never substitutes for an
approved independent review on the committed tree. The simulator mirrors this
execution and isolation.

**SubWorkflow** declares `workflow` (`{"id", "revision"}`, a pinned child
revision >= 1 that is never this workflow itself), `input_map` (uniquely named
`{"name", "stage"?, "type"?}` selections of this workflow's artifacts),
`output_map` (1 to 32 uniquely named `{"name", "stage", "type"?}` selections of
the child's artifacts by producing child stage), `output` (the parent artifact
the mapped outputs become) and an optional `failure` edge. It takes no role
and produces no change in the parent. Parent admission fails closed when a
pinned child revision is unknown or its workflow is retired, when an
`output_map` entry names an unknown stage or artifact type in the child
workflow, and when nesting is cyclic or deeper than 4. On arrival the parent
parks with reason `SubWorkflowPending` and records the child it waits for
(`child`: stage, visit, task id `<parent>.<stage>.<visit>`, workflow id and
revision). A stage starts one child unless it declares `items_from`, a projection
whose value must be an array. Field names such as `items` in mapped artifacts,
including FanIn join artifacts, never change the child count. An explicit list
starts one child per element; multiple children use `<parent>.<stage>.<visit>.<idx>`
(`0 <= idx < N`). The visit's child limit remains 16 without recursion, or its
recursion `max_children_per_visit` (at most 256). An empty explicit list binds
an empty child result. Each child's `input_map` projections may use `item` to
select that child's element. Only those mapped values bind the child's
signature inputs; the structured admission content wrapper is never its inputs.
Each drive round the daemon then
acts. It admits each pinned child through the ordinary admission path, as its
own task bound to the pinned child revision (its own molecule), with a Local
disposition, the parent's run, priority and target, a `parent` link, and the
mapped parent artifacts as structured task content (`parent_task`,
`parent_stage`, `inputs`, plus `"item"` and `"item_index"` on each fanned-out
child). When the child finishes Done (or all N children finish Done), it binds
the child's mapped artifacts into one parent artifact (`child_task`,
`workflow_id`, `revision`, `workflow_hash`, `outputs`, `artifacts`, plus
`"children"` holding each child's mapped entry on a multi-child visit) with
provenance naming the child task(s) and child artifact ids. The parent then
takes its success route. A child that ends without Done, lacks a mapped
artifact, cannot be admitted or yields a schema-invalid binding takes the
`failure` route when declared, else escalates `HumanDecisionRequired`
(`SubWorkflowFailed: ...`), and any remaining active sibling children of a
multi-child visit are escalated and retired as obsolete. A child whose parent
no longer waits for it (the parent was cancelled, retired, restarted or moved
on) is escalated and retired as obsolete. A waiting parent is never migrated.
Scenario rehearsal admits and executes pinned children from its workflow catalog
with the same explicit list and per-child input binding as the daemon.

## Gate records (FAE-WF-063)

Every Gate evaluation, every evaluated Branch arm (1-based `arm`), every Review
aggregate verdict and every HostCommand shape check is stored on the job as a
separately content-addressed gate record. A record holds its `kind`, `stage`,
`name`, `arm`, `tree` and `expression`, the ordered references
(`id`, `type`, `stage`, `session`) of every evidence item a lookup of the
expression could select, and the full result (value, every leaf, the ids read
and the step count). Its `id` is the SHA-256 of that canonical content, and the
event that routed on it cites the id. `workflow.Replay` (`engine.ReplayGates`
for a job) resolves the references from the recorded, content-addressed
evidence, evaluates the expression again and requires an identical result.
`engine.Validate` checks every record's content address. Branch arm evaluations
now appear as `branch-arm` events. The simulator returns the same records.

An element filter nested inside another element filter can compare the two
objects: `{"op":"eq","args":[{"op":"item","path":["id"]},{"op":"item","item_depth":1,"path":["id"]}]}`.
The left operand is the inner element; the right is the enclosing element.
Out-of-scope or negative depths are refused during validation. These bindings
use the existing depth-64 and 10,000-step evaluation bounds.

A SubWorkflow `output_map` entry may select a child's declared `output` and
publish it with `as: ParentArtifactType`. The composite stage artifact remains;
the alias is an additional artifact at the SubWorkflow stage, with child task
and artifact provenance. Catalog validation proves the child schema satisfies
the parent alias schema and checks mapped signature input names and types.

Projections also accept `index` (negative indexes count backward), followed by
`then` (a field path), `default` for a missing source/field/index/table key,
`lookup: {table: {...}, key: <projection>}`, and `literal` JSON values inside
objects. Defaults emit `projection-default` events with their read evidence.
These forms retain the projection depth and work limits.

When a containing FanOut is visited again, durable child-task and change-request
identities add `.fan.<outer visit>.<inner visit>...` in nesting order. The first
fan instance keeps its existing identity. Local stage visits may start at 1 in
each instance; the suffix prevents reuse of a previous instance's finished
children or decisions. Decision identities incorporate the same qualification.
