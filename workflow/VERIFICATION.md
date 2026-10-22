# Workflow verification scope

These tests verify the implemented pure helpers and offline replay subset. They do not establish completion of any delivery phase or conformance of the whole application to REQUIREMENTS.md.

| Check | Positive test | Firing control / refusal test |
|---|---|---|
| Canonical content and numbers | `domain.TestCanonical`, `domain.FuzzCanonical` | duplicate keys, invalid UTF-8, trailing input and oversized exponent in `TestCanonical` |
| Strict workflow parsing | `TestStrictParse` valid document | unknown fields/primitives and wrong field types in the same test |
| Kernel transition graph | `TestKernelExhaustive`; `TestGeneratedKernelAndBoundedCycles` | exhaustive terminal/unknown/kernel-edge refusal; generated removed-cap mutants |
| Reachability and cycle caps | generated graphs with bounded Loop vertices | unreachable stage and cycle avoiding a Loop in `TestValidationMutationCorpus` |
| Three-valued gates | `TestThreeValuedTables`, `TestGateOperationsAndTrace` | `TestUnknownCauses`; all Kleene combinations in `TestThreeValuedTables` |
| Exact number comparison | `TestNumericPrecision` | adjacent integers above 2^53 remain unequal |
| Positive evidence | `TestPositiveGateChecks`, valid protected graph | zero-count test gate mutation; `TestPositivePerCollection` rejects using A to excuse empty B |
| Gate termination | `FuzzGateBounded` and evaluator depth/work limits | unknown result on malformed operations; the fuzz invariant bounds node visits |
| Protected effect graph paths | valid graph in `TestValidationMutationCorpus` | test bypass, failed-gate bypass, wrong host-command reference and self-review mutations |
| Task-local input prerequisites | valid evidence in `TestNoBypass` | deletion-equivalent missing ID, wrong task/type/stage/version and invalid provenance |
| Tree-bound independent review helper | `TestProtectedRuntime` | stale run tree, same reviewer role/session, invalid change provenance |
| RED function identity | `TestFrozenIdentity` formatting and unrelated-function invariance | changed assertion, duplicate names and missing declared function |
| Offline replay | `TestSimulate`; `examples/workflow.json` with task `demo` | zero executed tests escalates; bounded retry exhaustion; commit replay explicitly refused |

The generated graph suite executes 10,000 cases with seed 60161, varying graph size, loop caps and extra edges. It exercises graph/kernel properties, not arbitrary workflow semantics or the complete default pipeline. Fuzz runs are bounded campaigns, not proofs of all possible inputs. The package's trace tests are not an end-to-end durable-audit replay test.

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
features describe this pure package and its offline simulator at the initial
baseline, not the newer durable engine.

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

## Independent review boundary

The control-plane subagent independently reviewed workflow protected-effect and runtime evidence checks. Its findings removed the `protected: false` bypass, added task identity checks, and made the simulator refuse effects needing runtime enforcement. The workflow subagent reviewed the API, runtime environment and worktree helpers and reported focused findings to their owner. These were bounded code reviews of newly implemented helpers, not whole-product verification or proof that all requirements are satisfied. Formal verification artifacts and their execution status are documented separately under `verification/`.
