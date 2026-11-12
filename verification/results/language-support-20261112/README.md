# Language adapter verification — 2026-11-12

Implementation and configuration: [language support](../../../docs/language-support.md).

- `all.log`: final `go test -p 2 ./...` with offline-provisioned pytest and tsc on PATH.
- `native.log`: actual Dolt-backed daemon execution for Rust, Python and native
  TypeScript, exact selection, persisted evidence, final gates, no leaked leases,
  and no replay after restart. No model/provider calls.
- `vet.log`, `rpc-schema.log`, `ledger.log`, `diffcheck.log`: supplementary checks.

Native tools include installed Cargo/rustc/rustfmt and Node 24.22.0, plus pytest
9.2.0 and TypeScript 5.9.3 provisioned from existing local caches into `/tmp`.
No package downloads or global tool installations were performed. Ruff's policy
and environment are covered by tests; native Ruff execution was unavailable.
Actual agent-provider launches and namespace-restricted toolchain mounts are
not established by these local command/daemon tests.

The staged binary is `bin/fae-languages-20261112`. No live executable was replaced
and no service was restarted or deployed. Requirements and earlier shipped
generation pins were preserved; generation 4 records the expanded RED shape enum.
