# Fae-on-Fae acceptance after wave 8, 2026-10-27

`scripts/self-host.py acceptance` ran a live instance on a clone of this repository at
`wave8/integration` 2ad283d. The instance used the shipped default workflow (Local
projection, routes narrowed to `claude-opus-5-5`, the direct adapter), with no pinned
task scope, `nested_commands: account` and a 16 MiB output bound. Its home was a fresh
`~/fae-self8`. The task came from `task.json`, which declares `controller/admission.go`
and `controller/admission_test.go` (FAE-HOST-005: the heavy-op admission's pressure
wait line lacked the avg60 values and directions). It reached Done 1,100 s after
admission:
- 14 provider turns, all completed;
- 2 attempts: at implement, the coder asked for read-only context (`hostguard/hold.go`,
  `daemon/live_admission.go`) through a scope-widen request instead of guessing; the
  oracle judged it, and the coder then implemented within the declared files;
- 4 engine test runs: RED (the named test failed as the task stated), the targeted run,
  whole-suite QA (2,305 tests, 330 s) and post-merge QA (346 s), all under account mode's
  nested-call policy;
- two dispatches, post-merge QA and commit, were held by host admission for IO pressure
  just after a whole-suite run. Each was deferred as infrastructure after 11 checks
  (about 30 s), charged nothing, and was redispatched.

This run exercised wave 8's self-hosting paths: the accounted nested-call policy (QA's
whole suite passed through it), the RedReport check at test-first, and the
`expected_failure` reason rule.

The landed commit is in `landed.patch`; `acceptance.json` is the harness evidence.

**Taken back.** The landed commit was applied to `wave8/integration` with `git am`
(1641553, author Fae). With the fix reverted, its test fails with
`pressure waiting line lacks io_full_avg60`, as the task required.

The first full `scripts/test.sh` run on that tree failed once, on an older race that
Fae's change did not touch. `TestDaemonLandGatesAdvanceOperatorBranchAndSalvageRetires`
parked UncertainDispatch with the cause `EOF`: a process record could be read before
it was written. That race was fixed in f154b81. The next run passed (2,646 cases),
`scripts/verify.sh` passed, and both commits were applied to the main working tree,
uncommitted.
