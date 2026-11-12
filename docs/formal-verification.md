# Formal verification stages

A formal stage is an ordinary scoped, allowlisted `HostCommand` with a
`formal` configuration. The command executes the verifier; Fae records and
normalizes the resulting evidence. Fae does not establish a verifier's
soundness, install tools or translate source programs into proof obligations.

```json
{
  "primitive": "HostCommand",
  "command": ["my-proof-adapter", "verify", "model.spec"],
  "weight": 1,
  "timeout_seconds": 120,
  "output": "ProofReceipt",
  "formal": {
    "tool": "my-checker",
    "parser": "normalized-json",
    "kind": "deductive",
    "sources": ["model.spec"],
    "min_obligations": 1
  }
}
```

The stage's normal command vector remains authoritative. The target must
allowlist it and provide the executable. A configured command does not gain
write authority, network access or a resource-limit exception. Verifiers which
invoke compiler children may need the instance's `nested_commands: account`
setting; those children remain under their parent's lease and command policy.
The stage cannot simultaneously declare `test_run`, `test_shape`, `check`,
`generate`, `capture` or `accept`.

`sources` optionally selects unique literal relative files for the source
identity. With no list, the broker binds the receipt to the current Git tree.
Explicit sources must remain in scope and may not change during verification.
The runtime binds source digest, tool version when reported or supplied by the
runtime, and configuration metadata to the recorded result. An optional `tool_version` pins the required
version. `bounds` is a map of strings; `assumptions` is a list of explicit
assumptions. They describe what the evidence establishes rather than remove
obligations.

Named adapters include Gobra and Verus for deductive verification, GoMela and
Kani for bounded model checking, Nagini for Python deductive verification,
CrossHair for counterexample search, and `tscert` for certificate evidence.
Only report formats recognized by the selected adapter are evidence. A
zero-error banner without a positive, complete obligation count does not
establish a proof. Unsupported tool versions or report forms must fail closed;
use `normalized-json` or the configured `json` field mapping below for an
additional tool or a report format that is not recognized natively.

A custom adapter writes exactly one JSON object to stdout:

```json
{
  "outcome": "proved",
  "obligations": 2,
  "verified": 2,
  "failed": 0,
  "unknown": 0,
  "skipped": 0
}
```

Accepted outcomes are `proved`, `disproved`, `unknown`, `error`, `unsupported`
and `skipped`. The count fields are required and must agree. `kind` and
`tool_version` may also be reported when consistent with the configuration.
A custom tool needs `parser: normalized-json` or `parser: json` and one explicit
evidence kind:
`deductive`, `bounded-model`, `counterexample-search` or `certificate`.

The report cannot supply its own `passed` bit. Fae derives `passed: true` only
from an exit-zero `proved` report with sufficient positive obligations, every
obligation verified and zero failed, unknown or skipped obligations. Timeout,
truncation, malformed output, inconsistent counts and version mismatch cannot
pass. Counterexample search does not become a deductive proof merely because
it found no counterexample; its result does not pass a proof gate.

## Mapping an existing tool's JSON report

A future verifier can use its own JSON schema without a new built-in language
adapter. Set `parser: json`, declare its evidence kind, and map every outcome
and count field explicitly:

```json
{
  "tool": "future-checker",
  "parser": "json",
  "kind": "deductive",
  "sources": ["model.spec"],
  "json_fields": {
    "outcome": "summary.status",
    "obligations": "summary.counts.total",
    "verified": "summary.counts.verified",
    "failed": "summary.counts.failed",
    "unknown": "summary.counts.unknown",
    "skipped": "summary.counts.skipped",
    "tool_version": "metadata.version",
    "status_map": {
      "PROVED": "proved",
      "DISPROVED": "disproved",
      "INCONCLUSIVE": "unknown",
      "ERROR": "error",
      "UNSUPPORTED": "unsupported",
      "SKIPPED": "skipped"
    }
  }
}
```

The command still supplies exactly one JSON object on stdout. Paths address
dotted object fields, not JSONPath expressions: no array indexes, wildcards,
calculations, defaults or coercion. All six outcome/count paths are required;
`tool_version` is optional. Every selected count must be an actual nonnegative
JSON integer at most one billion. Missing counts do not silently mean zero,
and numeric strings or floating-point representations are refused. The
selected status must be a string explicitly present in `status_map`. Duplicate
JSON fields, malformed reports and inconsistent totals fail closed, including
in report sections the mapping does not select.

Mapping chooses where evidence is read; it cannot set `passed`, override a
nonzero process exit or turn unknown/skipped obligations into verified ones.
A tool without all required information needs an operator-supplied report
adapter with a documented interpretation. Naming a new tool in configuration
does not grant execution authority: its exact command must still be in the
pinned target allowlist, and normal resource, path and write policies apply.

See `examples/formal-mapped-workflow.json` and its illustrative report
`examples/formal-mapped-report.json`. Both are configuration examples; they do
not install or claim to execute a future verifier.

## Tool profiles and validation performed

The profiles describe evidence categories, not interchangeable proof strength.
Actual executable digests and source identities remain part of the receipt.

| Tool profile | Evidence/report support | Validation performed in this implementation session |
| --- | --- | --- |
| Gobra | Deductive; native debug report with explicit discharged obligations, or a JSON adapter | Actual Gobra native execution exercised. A zero-error banner without a positive obligation count remains unknown. |
| GoMela | Bounded model checking; recognized native model reports, or a JSON adapter | Captured real GoMela reports exercised; no fresh model-checking run was performed in this session. |
| Kani | Bounded model checking; recognized native verification summaries, or a JSON adapter | Native-format fixtures exercised; Kani was not installed for a real invocation. |
| Verus | Deductive; recognized native verification summaries, or a JSON adapter | Native-format fixtures exercised; Verus was not installed for a real invocation. |
| Nagini | Deductive; requires explicit obligation evidence | A success/zero-error report without counts remains unknown and needs a report adapter. No actual Nagini verification was claimed. |
| CrossHair | Counterexample search | Never passes a proof Gate merely because no counterexample was found. No deductive-proof claim is made. |
| Theorem / `tscert` | Deductive / certificate categories through `normalized-json` or configured `json` | Experimental adapter contracts; bare native CLI success is unsupported, and no real theorem or certificate checker invocation was claimed. |
| Configured future tool | Explicit kind plus `normalized-json` or mapped `json` | Real local report-adapter commands exercised parsing, success/nonpassing gates, durable receipts and restart behavior; this is not proof of the adapter's soundness. |

## Consuming the receipt

Labeling a command records proof evidence; it does not itself decide which
outcomes a workflow accepts. Add a Gate that explicitly requires the derived
bit before treating the formal result as success:

```json
{
  "primitive": "Gate",
  "name": "formal obligations discharged",
  "expression": {
    "op": "eq",
    "args": [
      {"op": "field", "stage": "verify", "path": ["passed"]},
      {"op": "literal", "value": true}
    ]
  },
  "failure": "stop"
}
```

Every passing disjunction that reads a formal receipt must include this
requirement for the exact selector. Checking only `exit_code == 0`, an outcome
label or receipt existence is refused. A stage selector is the clearest way
to identify the intended verifier; a type-only selector cannot also select a
non-formal producer. Unknown or missing evidence cannot pass the Gate.

Formal Gates supplement existing workflow floors. They do not replace a code
change's genuine RED test, executed tests, independent review, frozen artifact
checks or protected-effect controls. Bounded results must be read with their
recorded bounds and assumptions; certificate validity depends on the checker
and certificate semantics.

`examples/formal-workflow.json` is a complete minimal workflow using the
configured-report abstraction. Its `my-proof-adapter` executable is a
placeholder that an operator supplies and allowlists; the example does not
claim that a verifier is installed or that a proof was run.
