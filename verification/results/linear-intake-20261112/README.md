# Linear intake verification — 2026-11-12

Scope: Symphony-compatible Linear polling into the native Fae engine, durable UUID
bindings, explicit blockers and pagination, tracker scope reload, and opt-in
team-scoped state/comment writes.

Passed checks:

- `go test -p 2 ./...` (`full-suite.log`; native Dolt tests separately enabled below).

- `FAE_TEST_DOLT_BINARY=/usr/local/bin/dolt go test ./daemon -run '^(TestLinearBridgeDurableBinding|TestLinearIntakeNativeDecisionDependenciesAndRestart)$' -count=1` (`native.log`).
- `go test -race ./symphony ./tracker ./daemon -run 'Linear|Symphony|FaeConfig' -count=1` (`race.log`).
- `go vet ./...` (`vet.log`, empty on success).
- `python3 scripts/check-ledger.py` (`ledger.log`; census, not requirement acceptance).
- Additive RPC schema check (`schema.log`).
- Example workflow parsing and staged executable build; executable hash in `binary.sha256`.

Native tests use real local Dolt and the Fae engine with a scripted executor and a
fake tracker. Tracker contract tests use mocked GraphQL responses, including
paginated blockers and team-specific state lookup. No real Linear organization,
API credential, external tracker mutation, or live model turn was used. These
checks establish local integration behavior, not production provider acceptance.
The staged executable does not replace or restart the running service.
