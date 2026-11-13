# OR-811 runner recovery — 2026-11-12

The initial README task consumed20 attempts and5/8 test runs while the refusing
nested-command policy sent full-suite failures back through implementation.

The deployed fix adds audited, durable `runtime-config --nested-commands
account|refuse`, preserving the pinned private live configuration. Native Dolt,
race, CLI, full daemon/CLI, vet, schema and ledger checks passed.

A complete accounted suite inventory ran3554 tests and recorded13969 nested
calls with no timeout/truncation. It failed13 fixture tests in commandshim,
effects and daemon. Corrections are test-only: native tool discovery avoids
outer wrappers, Unix socket fixtures use short private paths, and intentional
local Git-push fixtures test their own policy against temporary local repos.
All13 original failures passed targeted accounted checks; installed Rustfmt and
TypeScript also passed native probes. Production refusal policies were unchanged.
This inventory predates the corrections; it is not a whole-suite PASS receipt.
The restarted task must generate fresh evidence from the corrected baseline.

Reviewed fixture-only commits advanced only refs/heads/fae/linear-testing-20261112.
The operator's current branch/index/working files were preserved. A plane backup
was taken at /var/tmp/fae-herdr-live/default/backup/plane-1906353566.

Existing audited workflow migration was used instead of mutating task pins or
manually rebasing its worktree: default revision3 reconciles OR-811's preserved
README hunks against the reviewed integration tip, then runs every original
validation stage. Latest default revision4 reconciles after spec approval for
new tasks so scope is available. Engine clean/conflict/fresh-task proofs and
native reconciliation/own-hunk-loss tests passed.

OR-811's bound task ID stayed unchanged. Its total attempt ceiling is explicitly
40, with20 previously spent;5/8 test runs,6/8 implement-loop visits,2/3 spec-loop
visits and all request counters carried over without refunds. Actual recovery
moved task HEAD to e8a352f8e1cc843546ecc7d95359e239ea83db53, retained only README.md
as its own diff, and reached spec with a real provider turn in flight. Full task
completion and final suite results are pending at this checkpoint.
