# Fae-on-Fae acceptance, 2026-10-26

`scripts/self-host.py acceptance` ran a live instance on a clone of this repository at
`wave7/integration` 23e48db. The instance used the shipped default workflow (Local
projection, routes narrowed to `claude-opus-5-5`), with no pinned task scope,
`nested_commands: account` and a 16 MiB output bound. The task came from `task.json`,
which declares `controller/watchdog.go` and `controller/watchdog_test.go`. It reached
Done on its first attempt, 879 s after admission:
- 12 provider turns;
- 4 engine test runs;
- whole-suite QA took 285 s before the merge and 313 s after it.

The landed commit is in `landed.patch`. `acceptance.json` is the harness evidence. It
was collected after the run, because the run's own collection hit a harness bug, since
fixed: a daemon event can carry a non-object payload.

**Attempt 1** (clone at 25027ef, instance archived as `~/fae-self-attempt1`)
- It found two defects that the 2-file dogfood repository cannot reach. Both are fixed
  on `wave7/integration`.
- **Ownership record over 64 KiB.** The format stage's `gofmt -l .` expands to 899
  absolute paths. The command's ownership record therefore came to 120 KB, over the
  64 KiB read bound. Retire failed, and the dispatch parked UncertainDispatch.
  - Fixed in 26dc8e2: records keep a bounded command, and legacy records up to 4 MiB
    are read. After the restart, recovery read the record and redispatched by itself.
- **Zombie check in `TestResidentTaskBoundRetiresAtCollection`.** QA then failed on
  this test, deterministically under the daemon. The daemon reaps adopted daemonized
  Herdr servers after a 5 s grace (FAE-HOST-021/-023), so the retired process is
  briefly a zombie. The test checked `kill(pid, 0)` only once.
  - The coder correctly reported the failure as outside its scope.
  - Fixed in 23e48db.

**Taken back.** The landed commit was applied to `wave7/integration` with `git am`
(a6da137, author Fae). It passed the full `scripts/test.sh` run and `verify.sh`, and it
is in the main working tree with the rest of wave 7.
