# Mainline publication — 2026-11-13

Publication source candidate:7bf89f86a3667084b73bd43bb889606d0cc1365f.
It combines the current source with OR-811's verified README and local integration
history. The subsequent publication record and Linear-first AGENTS.md policy are
documentation-only additions; README trailing whitespace was removed.

Passed in an isolated checkout:

- `go test -p 2 ./...` (tests.log).
- `go vet ./...` (vet.log, empty on success).
- `python3 scripts/check-ledger.py` (ledger.log; census, not acceptance).
- Real Dolt Linear durable-binding, dependency/restart and dynamic nested-policy
  tests with FAE_TEST_DOLT_BINARY=/usr/local/bin/dolt (native.log).
- Existing local Herdr mod contract harness (herdr-ui.log). This uses mock UI/CLI
  adapters; it is not a visual or herdr-app check acceptance of a recreated UX.

General Go suite native tests that require optional installed prerequisites may
skip; the specific Dolt-backed integration checks above were separately enabled.
No new complete formal-verifier run was performed for this publication.

OR-811 completed in Fae and integrated README-only commit
2f08b72529ba7e698e4865b8d54eee3cf8a1b921. Latest full suite, QA and post-merge QA
receipts all have exit_code0. Host credentials and generated binaries remain out
of the source tree; the stored Linear API key was checked against candidate files
and was absent. No candidate file exceeded10MiB.
