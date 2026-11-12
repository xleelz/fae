# Configured commands and formal evidence — 2026-11-12

Interfaces: [command adapters](../../../docs/command-adapters.md) and
[formal verification](../../../docs/formal-verification.md).

The implementation adds a language-independent configured command mode,
formal report normalization and mapped JSON reports, strict proof gates,
source/executable identities, and durable command-linked proof receipts.

Validation:

- `all.log`: final full package suite with real installed/cached language tools.
- `commandshim.log`: full command-policy suite after the final accounted-child
  regression was added; configured children retain their parent's lease and
  explicit environment, while ordinary language grammar remains enforced.
- `native-daemon.log`: all eight daemon cases, including mapped JSON and plain
  configured execution, real commands, persisted gates/receipts and restart.
- `vet.log`, `rpc-schema.log`, `ledger.log`, `diffcheck.log`: supplementary checks.
- `example.json`, `mapped-example.json`: staged CLI validation of both examples.

Effects tests execute the existing Gobra jar through the real broker with Java
and Z3. A specified contract passes; its mutation is disproved. Focused race
tests for proof, workflow, command policy and effects also pass. Gobra's DEBUG
statistics count verified contract-bearing members, not individual solver VCs.

Gomela tests use captured real reports. Kani/Verus have native-report fixtures;
Nagini requires counted reports; Theorem/TScert require configured JSON adapters.
These tools were not installed or executed as part of this pass. CrossHair
counterexample search cannot satisfy a proof gate merely by finding no error.
The docs give the exact profile and trust boundaries. Adapter fixture commands
test orchestration and parsing; they do not establish a mathematical theorem.

No verifier was downloaded or installed, no live service was restarted, and no
deployment or commit was made. The staged binary is
`bin/formal-verification-20261112/fae`.
