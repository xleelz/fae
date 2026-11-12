# Developing Fae

This guide states how Fae itself is built (FAE-PLAN-002). The full workflow in
REQUIREMENTS.md is what Fae executes for its operators; it is not the process for
building Fae. Building Fae uses a lean process.

## The lean process

1. **One isolated worktree per change.** Every change is made on its own branch in
   its own Git worktree, never in the operator's checkout. Keep the worktree path
   short (for example `~/workplace/w3/<name>`): Unix socket paths are limited to
   108 bytes, and daemon, Dolt and Herdr sockets live under fixture roots.
2. **Tests first.** Write the failing test, run it and see it fail for the
   expected reason, then implement and see it pass. A remediation is proved by a
   behavioural test, never by the text of a comment (FAE-VER-008).
3. **One independent review.** Someone who did not write the change, a person or
   an agent, reviews the diff before it is integrated. In the parallel rounds the
   integrator's cross-check of every stream and the following per-requirement
   re-audit played this role; a change made outside a round still needs its own
   reviewer.
4. **Proofs only where the spec requires them.** Formal or exhaustive proof is
   required only for the six areas below. Everything else is covered by
   behavioural tests with firing controls (FAE-VER-001).
5. **Host safety and worktree isolation are never relaxed**, in tests or in
   development runs: heavy commands go through the heavy-op lease, fixtures live
   in their own roots, and nothing touches the operator's instance, plane, Herdr
   or `~/.config/herdr-core` (`$XDG_CONFIG_HOME/herdr-core/config.toml`).

## Required proofs and where they live

| Area (FAE-PLAN-002) | Current evidence |
|---|---|
| Transition state machine | `TestKernelExhaustive` (every kernel and stage edge), Gobra `completion`, `control` and `drive` guards, the generated workflow property suites in `engine/` |
| No-bypass rule | validator protected-floor and reference rules (`check:validator-*`), the validator mutation corpora, input checks in the generated property suites |
| Role and lease capacity | Gobra `capacity`, `release`, `dynamic-host`, `pane`, `dynamic-pane`; Gomela `lease`, `panes`, `dynamic_host`; `TestCapacityExhaustive` |
| Worktree containment and deletion | `workspace/path_test.go` (byte-form paths, FAE-VER-007), Gobra and Gomela `cleanup`, `scratch`, `scratch-archive`, shim scope refusals |
| Single-writer control-plane authority | Gobra and Gomela `authority`, the typed refusal tests; the plane's single-writer property itself has no formal proof (see `verification/README.md`) |
| Crash recovery | Gobra `recovery`, `reconcile`, `drain`, `uncertain`, `effect`, `effect-adoption`, `restart`; Gomela `recovery`, `restart`, `daemon`; `TestCrashMatrixEveryDurableBoundary` and the effect SIGKILL tests |

A new safety predicate goes into a small pure `*_guard.go` function with a Gobra
contract (`// @ ensures ...`), one `verify_guard` line in `scripts/verify.sh` with a
weakened mutation that Gobra must reject, and an entry in `verification/ledger/`
(see `verification/LEDGER.md`). Keep existing guard signatures and bodies unless
the fix requires a change.

## Checks every change keeps green

- `go build ./...`, `go vet` and `gofmt -l` on the changed packages.
- Targeted tests while working: `go test -count=1 -run 'Pattern' ./pkg/`; each
  changed package in full once at the end. The `daemon` package takes about 67
  minutes; run it alone, last.
- `python3 -I scripts/check-ledger.py`. A new validator rule, shim or role-shim
  refusal (`RunShim`'s `deny(refuse(...))`, `deny(fmt.Errorf(...))` and
  `&LocalRefusal{...}`), pre-tool hook denial, landing gate, host-gate reason or
  bridge refusal class must be claimed by a `native-check` entry whose
  firing-control test names it verbatim, or the census fails. A new `refuse(...)`
  or `&LocalRefusal{...}` in `commandshim/accounted_git.go`,
  `liveScopeRefusal(...)` in `daemon/live_scope.go` or
  `daemon/live_red_subtests.go`, or error returned by `liveRedTurnChange` must be
  claimed by a ledger entry for its file and fire in that file's in-test census
  (`TestAccountedGitProtectsTheRunsRepository`, `TestLiveScopeRefusalCensus`);
  write each as one call ending its line. A refusal made in another shape (a
  `&Refusal{...}` literal, a new helper, a plain `errors.New` that exits 77) is
  outside the census and must not be added; `verification/LEDGER.md` lists the
  shapes it cannot see.
- New guards: run the pinned Gobra jar on the single file outside the tracked
  logs, the way `verify.sh` invokes it, and confirm the mutation is rejected.
- Default workflow content changes: bump `defaults.Generation`, append its hash to
  `defaults/testdata/shipped-generation.json` (never re-record a generation),
  and re-record the parity corpus with
  `go test ./engine -run TestDefaultWorkflowParityCorpus -update-parity`, then review
  the diff.
- New CLI verbs and flags: a `cmd/fae/help.go` row, a `cmd/fae/conformance_test.go`
  row and the usage line. Exit codes: 0 ok, 1 refusal, 2 usage, 3 daemon unreachable.
  New RPCs need a `daemon/authority.go` entry.

## Test environment rules (FAE-VER-006)

- Tests use injected configuration only. The explicit injections are
  `FAE_TEST_DOLT_BINARY` (daemon and control-plane integration tests skip without
  it), `FAE_TEST_HERDR_BINARY` (else `herdr-core` on the test PATH) and
  `FAE_TEST_QUIET_HOST_PRESSURE`. `scripts/test.sh` unsets every other `FAE_*`.
- Daemon tests admit against real host pressure by default (an operator
  decision). `FAE_TEST_QUIET_HOST_PRESSURE=1` is for development runs on a loaded
  host; a new daemon test must also pass without it on a quiet host. Use the helpers
  in `daemon/pressure_helpers_test.go` (`call`, `awaitState`, `awaitThroughDeferred`,
  `awaitJob`, `admitted`, `startSubprocess`).
- Fixture roots live outside the checkout, on disk (`/var/tmp`, created with
  `os.MkdirTemp`) and each test removes its own. Never use the RAM-backed temporary
  directory for instance roots.
- Subprocess-helper protocol variables never use the `FAE_` prefix (for example
  `CRASH_MATRIX`, `BRIDGE_HELPER`): `FAE_*` is Fae's own configuration.
- `scripts/test-hygiene.py` is the residue census: `$TMPDIR`, every fixture root
  created during the run and any process still carrying the run's `TMPDIR` must be
  gone. Its `hostile` mode plants every Fae variable with a wrong value and must not
  change outcomes; `scripts/test.sh` runs it over the non-daemon packages and a
  daemon subset.

## A parallel round, as waves 1 to 3 ran

1. **Audit.** Re-audit the requirements against the current tree, per section,
   with file and line evidence, before claiming compliance.
2. **Snapshot the base.** Commit the operator's uncommitted tree as a snapshot on a
   side branch (not on `main`); every stream branches from it.
3. **Disjoint streams.** Give each stream its own short-path worktree and branch
   (`wave<N>/<stream>`), its own `TMPDIR`, and explicit file ownership. Streams make
   minimal hunks in files they do not own and list them; schema migrations take the
   next number and the integrator renumbers. Streams commit early and often on their
   branch; they never push or touch `main`.
4. **Integrate** in a fresh short-path worktree: merge the branches, fix
   cross-stream interactions, renumber migrations and reconcile the ledger.
5. **Verify on a quiet host:** `FAE_TEST_DOLT_BINARY=/usr/local/bin/dolt
   FAE_TEST_HERDR_BINARY=<herdr-core> bash scripts/test.sh` with real host pressure (about
   50 minutes, daemon package last and alone), then `bash scripts/verify.sh` (about
   9 minutes; it regenerates `verification/logs` and `verification/results`), then
   `python3 -I scripts/check-ledger.py --report`.
6. **Apply.** Confirm the operator's tree still equals the snapshot, then apply the
   integrated diff to it.
7. **Dogfood.** Build `bin/fae` and the bridges, then run `scripts/dogfood.py` with a
   fresh evidence directory (the full command is in `HANDOFF.md`). A scripted run is
   a rehearsal; only the recorded live run counts (FAE-VER-011).
8. **Hand off.** Update `HANDOFF.md` from the evidence: what passed, what is still
   open, and every decision that needs the operator.
