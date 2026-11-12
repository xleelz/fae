# Workflow verification scope

These tests verify the implemented pure helpers and offline replay subset. They do not establish completion of any delivery phase or conformance of the whole application to REQUIREMENTS.md.

| Check | Positive test | Firing control / refusal test |
|---|---|---|
| Canonical content and numbers | `domain.TestCanonical`, `domain.FuzzCanonical` | duplicate keys, invalid UTF-8, trailing input and oversized exponent in `TestCanonical` |
| Strict workflow parsing | `TestStrictParse` valid document | unknown fields/primitives and wrong field types in the same test; `TestParseNamesEveryErrorPath` lists every structural and semantic error with its document path |
| Kernel transition graph | `TestKernelExhaustive`; `TestGeneratedKernelAndBoundedCycles` | exhaustive terminal/unknown/kernel-edge refusal; generated removed-cap mutants |
| Reachability and cycle caps | generated Loop/Branch graphs in `TestGeneratedKernelAndBoundedCycles` | `TestValidationMutationCorpus` (each case asserts its path): unreachable stage, cycle avoiding a Loop, dead end, no path to Done, ambiguous success route, missing output |
| Three-valued gates | `TestThreeValuedTables`, `TestGateOperationsAndTrace` | `TestUnknownCauses`; all Kleene combinations in `TestThreeValuedTables` |
| Exact number comparison | `TestNumericPrecision` | adjacent integers above 2^53 remain unequal |
| Positive evidence | `TestPositiveGateChecks`, valid protected graph | zero-count test gate mutation; `TestPositivePerCollection` rejects using A to excuse empty B; `defaults.TestDefaultFloorMutationCorpus` refuses test-run gates without an executed-count bound, with or without `test_stage` |
| Gate termination | `FuzzGateBounded` and evaluator depth/work limits | unknown result on malformed operations; the fuzz invariant bounds node visits |
| Protected effect graph paths | valid graph in `TestValidationMutationCorpus` | test bypass, failed-gate bypass, wrong host-command reference and self-review mutations |
| Task-local input prerequisites | valid evidence in `TestNoBypass` | deletion-equivalent missing ID, wrong task/type/stage/version and invalid provenance |
| Tree-bound independent review helper | `TestProtectedRuntime` | stale run tree, same reviewer role/session, invalid change provenance |
| RED function identity | `TestFrozenIdentity` formatting and unrelated-function invariance; `TestFrozenRedFileCompatibility` admits formatting, comments, added tests and imports | changed assertion, duplicate names and missing declared function; changed helpers/constants, removed imports, added init/TestMain/blank imports |
| Test-first floor | default content validates | `defaults.TestDefaultFloorMutationCorpus`: phase-less change producer, missing post-implementation RED pass, RED run without declared test shape |
| Element counts | `TestElementCount` | item outside an element count, missing/non-array fields and unknown filters are unknown or refused |
| HumanDecision (FAE-WF-030) | `TestHumanDecisionValidation` valid graph; `engine.TestHumanDecisionParksDecidesAndRoutes`; `daemon.TestDecideVerbEndToEnd` (inbox, audit, decision row, restart); `TestSimulateHumanDecision` | missing question, auto-decide timeout, undeclared or unrouted options, edge without option, option route bypassing review; duplicate, unauthorized, stale, blocked, escalated and cancelled decisions refused (`engine.TestHumanDecisionHaltAndCancellation`); missing input and schema-invalid decision artifact (`engine.TestHumanDecisionFloors`) |
| Primitive configuration (FAE-WF-030) | `TestPrimitiveConfigurationValidation` valid `configDoc`; `engine.TestReviewAggregationRules`, `engine.TestHostCommandExpectedShapeAndEnvironment`, `engine.TestLoopExhaustionBackoffAndEscalateReason`; `effects.TestCommandEnvironmentPolicy`; `TestSimulateConfiguredPrimitives` | one refusal per malformed or misplaced field (reason placeholders, aggregation rule/quorum/failure route, exit gate, backoff bounds, exhaustion kind, shape, reserved and credential variables, widened profiles, gate names); failed aggregate confers no review on its failure route |
| FanOut/FanIn (FAE-WF-030) | `TestFanValidation` valid graph and re-entry legality; `engine.TestFanOutJoinRules` (all/any/k-of-n with restart at every boundary, early settlement skips unrun branches, sibling isolation of bound inputs, replayable branch verdicts); `daemon.TestFanOutDaemonRestartInsideBranch`; `TestSimulateFan` | branch/join configuration, overlap, outside entry, Done from a branch, change producer or commit in a branch, sibling inputs and verdicts, vacuous verdicts, test and review split across branches; `engine.TestFanJoinCannotLaunderCommitEvidence`; cancellation, abandoned fan on restart, refused migration and tampered fan state (`engine.TestFanCancellationRestartAndMigration`) |
| SubWorkflow (FAE-WF-030) | `TestSubWorkflowValidation` valid graph; `engine.TestSubWorkflowBindsPinnedChild` (pinned child reference, mapped content, bound result with provenance, restart round trip); `daemon.TestSubWorkflowDaemonEndToEnd` (child admitted on its own revision, restart while the child waits, result bound, parent Done); `TestSimulateSubWorkflow` | unpinned, self, unmapped or misplaced configuration; unknown child revision refused at parent admission; running, mismatched, relinked or twice-bound child refused; child without Done, missing mapped output (`engine.TestSubWorkflowFailureAndCancellation`); cancelled parent's child retired as obsolete |
| Gate records (FAE-WF-063) | `engine.TestGateRecordsReplayIdentically` replays every gate, branch-arm and aggregate record of a default run after a durable round trip; `TestSimulateConfiguredPrimitives` replays simulator records | tampered result fails `engine.Validate` (content address) and, re-addressed, fails replay; replay over missing evidence refused |
| Offline simulation | `TestSimulate`; `defaults.TestSimulateDefaultScenario` (spec rejected once, oracle blocks once; same path as the durable engine); `examples/workflow.json` with task `demo` | zero executed tests escalates; loop caps; unreviewed commit escalates `ProtectedEvidence`; stale build is malformed; scripted routing of evaluated stages and cross-task evidence refused |

The generated graph suite executes 10,000 cases with seed 60161, varying graph size, loop caps and extra Branch arms. It exercises graph/kernel properties, not arbitrary workflow semantics or the complete default pipeline. Fuzz runs are bounded campaigns, not proofs of all possible inputs. The package's trace tests are not an end-to-end durable-audit replay test.

## Durable implementation update

The original helper-only baseline below is retained to explain package scope.
The integrated `engine` now validates artifact payloads using JSON Schema, pins
workflow revisions, validates dispatch/session/task/tree provenance, enforces
separate durable budgets, and checkpoints transitions through controlplane v2.
Code-bearing graphs require RED, freeze, unchanged comparison and passing test
evidence on protected paths. The `defaults` package supplies the complete local
scripted pipeline. Its 22 artifact records are explicitly marked scripted;
commit/integration records do not establish real Git effects. See
[engine/README.md](../engine/README.md) and [TESTING.md](../TESTING.md) for current
coverage and remaining execution boundaries. The statements below about absent
features describe this pure package and the offline simulator it held at the
initial baseline, not the newer durable engine. That simulator has since left
the package: `engine.Simulate` drives the durable engine itself on a scripted
adapter (FAE-WF-091), so a simulated path is the engine's path, with every
floor, cap and gate record the engine applies. This package keeps only the
script and result types (`simulate.go`), and its simulator tests
(`simulate_test.go`) drive `engine.Simulate`. The simulator sentences in items
3, 4, 6, 7, 9 and 10 below describe the retired package simulator.

## Initial FAE-WF-070 helper floor coverage and gaps

1. **Fail-closed parsing and validation:** strict JSON keys, duplicate-key rejection, primitive/role references, selected required fields and graph checks exist. The complete workflow document schema and JSON Schema validation of artifact payloads are **not implemented**. Several primitive configurations remain unsupported or only structurally represented.
2. **Immutable molecules and revisions:** this package supplies content hashing only. It does **not** implement durable molecule identity, revision persistence, retirement or binding. Control-plane tests have separate scope; pure workflow tests cannot establish this floor.
3. **Audited transitions:** `CanTransition` checks legality and the simulator returns an in-memory trace. This package does **not** atomically persist transitions, projection updates, evidence digests or decisions; it does not enforce stale projection rejection in a running daemon.
4. **Task worktree isolation:** **not implemented or proved by this package**. The separate worktree package must enforce filesystem and Git ownership at execution time; simulation performs no filesystem execution.
5. **Host guards and leases:** **not enforced by this package**. Host-command weight/timeout structure is checked, but no actual session or command runs here. Scheduler/lease helpers do not establish wiring of every future launch through admission checks.
6. **Attempt caps:** validation requires positive limits and a bounded Loop on every graph cycle; simulation enforces its total visit bound and loop visit caps. Durable per-task limits, live operator adjustment/re-grant, test-run accounting and all production retry boundaries are **not implemented here**.
7. **No bypass:** `CheckInputs` checks task/type/version/stage and an input provenance-validity flag. Cryptographic provenance-chain verification, actual recorded-store existence, artifact schema validation, and mandatory integration of this helper into a production dispatcher are **not implemented here**. Simulator records are explicitly supplied trusted replay data.
8. **Positive-evidence gates:** per-collection non-vacuity and positive test counts are conservatively checked for the supported AST. Schema-derived status typing, a full evidence expression grammar and the complete gate mutation/parity corpus are **not implemented**. Unknown status rejection requires a declared `enum`.
9. **Protected commits:** graph checks conservatively cover all commit/integration nodes and external effects, with no content opt-out. `CheckProtectedEvidence` checks a shared task/tree and independent review identities. Real tree acquisition, validated provenance, protected branch configuration, aggregate review semantics and runtime commit enforcement are **not implemented here**. The simulator refuses commit and integration execution. These helpers therefore do not authorize a real commit.
10. **External effects:** validation requires mode/pins/idempotence metadata and protected graph prerequisites. Actual mode reachability proof, capability enforcement, daemon-pinned arguments, schema-validated tool calls, record-before-verdict and adoption/reconciliation are **not implemented here**. The simulator refuses external-effect execution.

FAE-WF-073's full test-first graph floor is also **not implemented**: normalized Go test identity is a standalone helper, not proof that every implementation path includes RED authoring, failing-engine evidence, freeze, unchanged comparison and a subsequent passing run. A no-red diff classifier is absent.

(Superseded in part: `code_bearing` graphs now enforce the FAE-WF-073 floor in the validator, see [GRAMMAR.md](GRAMMAR.md), and the simulator now replays commit, integration and Normal-mode external-effect stages as scripted results, refusing them without independent, tree-bound test and review evidence. The no-red route is validated per task class and the commit-time diff classifier lives in the target configuration, see GRAMMAR.md and `daemon/TARGETS.md`.)

## Independent review boundary

The control-plane subagent independently reviewed workflow protected-effect and runtime evidence checks. Its findings removed the `protected: false` bypass, added task identity checks, and made the simulator refuse effects needing runtime enforcement. The workflow subagent reviewed the API, runtime environment and worktree helpers and reported focused findings to their owner. These were bounded code reviews of newly implemented helpers, not whole-product verification or proof that all requirements are satisfied. Formal verification artifacts and their execution status are documented separately under `verification/`.
