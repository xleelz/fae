# Finished task cleanup and plane backups

`fae backup plane --authorizing-message ID` creates a fresh owner-only directory
inside the instance's `backup` directory. It uses Dolt's `sync-url` backup,
restores it into a separate repository, reads the restored job metadata, runs
`dolt fsck`, then records SHA-256 hashes of every backup/restore file. No database
files are copied from the live server. No global Dolt identity or backup remotes
are modified. Live instances acquire a weighted host admission lease for backup.

`fae cleanup retired` plans without changes by default (`--dry-run` is explicit).
`--apply --backup PATH --authorizing-message ID` applies the plan, with `--pace`
(default 100ms, range 10ms–1m) between tasks and `--limit` (default 200, at most
10,000) cleanable tasks planned per run. Both methods require the instance
operator. Backup and apply require a recent operator message; cleanup only reads
that message, without consuming it, and writes nothing to the plane (no events,
audit rows or Dolt commits), performing only git operations and plane reads so
it cannot collide with the drive (FAE-CTL-042). Existing
consumed-class compatibility is checked, and explicit verb/class restrictions
remain binding. A plain fresh operator message retains the usual operator
authority. Compatible runtime authorization can be reused because cleanup does
not consume it. A maintenance marker or shutdown before or during a run stops
further cleanup work with a typed refusal (FAE-API-011). Backup
and cleanup RPCs have a bounded two-minute budget; normal RPCs keep five seconds.
Shorter request deadlines still apply. Large backups can time out and safely
leave an unverified backup directory for investigation; cleanup refuses it.

`fae cleanup done` does the same for Done tasks, with the same flags, backup,
archive and pacing (it is the `CleanupRetired` RPC with `"done": true`). A Done
live task qualifies once its own landing commit (the `sha` of its live
Integrate artifact, as `fae reconcile-done` reads it) is reachable from the
live configuration's integration ref (`git merge-base --is-ancestor`); the
check is repeated under the lock before the archive and before each removal.
A Done task that has no landing of its own, whose landing is not in the ref,
or whose worktree or branch cannot be planned (dirty or ignored files, a moved
or missing worktree without archive evidence) is listed as `skipped` with the
reason and left as it is; it does not refuse the batch. Cleanup is explicit,
not automatic after Done: removing a branch from the operator's repository
needs a verified plane backup (FAE-CP-013), and a backup per finished task
would be a Dolt backup, restore and `fsck` each time.

Only durable Retired (or, for `cleanup done`, integrated Done) live tasks with
no active or unresolved execution qualify. Each task must have the same state,
version and payload digest in the restored backup.

Finished tasks are never removed from the plane, so the candidates are found
by their rows and git state, not by reading every snapshot (FAE-CP-009): the
tasks of the chosen state are paged through the jobs state index in id order
(1,000 rows a read, each read within 10 s), joined by key to the task row's
run and execution kind (and any legacy `worktree-cleaned` event), and checked
against a single git read of the repository's task branches, worktree
registrations and archive refs. A task whose worktree and branch are already
removed with a verified archive ref (`refs/fae/archive/<run-task-identity>/<commit>`),
or whose task row says it was not live (or has an unknown execution kind and
no worktree, branch or archive ref), is passed over without reading its
snapshot; only a remaining candidate's snapshot is read, by key. A run plans
at most `--limit` cleanable tasks (tasks already cleaned and skipped Done tasks
are not counted toward `--limit`, so skipped tasks early in id order never
starve later candidates); when more remain the result ends with an item whose
`task` is empty and whose `skipped` says so, and the next run continues.
All worktree plans and backup coverage are checked before starting. All immutable
per-tip archive refs are created and verified before any worktree is removed.
Archive refs are `refs/fae/archive/<run-task-identity>/<commit>` and preserve the
entire ancestry reachable from the task branch tip. Cleanup verifies deterministic
ownership, repository identity, exact tip, registration, and clean tracked,
untracked, and ignored files. A branch registered at another path is refused.
No force or recursive filesystem deletion is used.

Before each removal, cleanup rechecks durable task state, version, payload,
backup hashes, a Done task's landing in the integration ref, and the Git source. Branch deletion uses a Git reference transaction
that verifies the archive and compares the original branch tip. Concurrent
external Git mutation fails closed; an interruption may leave an archive, or an
archived branch without its worktree, which a fresh invocation can safely finish.
Repeated cleanup with valid archive evidence is idempotent across daemon restarts.
An archive or backup failure during preparation deletes nothing; a later source
change can leave earlier tasks safely cleaned and refuses the remaining work.

This implements FAE-CTL-042, for Retired and Done tasks, and the backup and
archive prerequisite of FAE-CP-013 for both.
Scratch-instance destruction (FAE-CTL-043) is available through the separate
[marked scratch cleanup controller](SCRATCH.md). Owned plane backups support explicit [retention and remote replication](BACKUPS.md).
Externally retained scratch archives are not pruned. Archive refs remain
local to the task repository. Administrative deletion of archives/backups outside
Fae is outside the ownership contract. Guard/model proofs cover the explicit
backup-plus-archive prerequisite, not Git or Dolt internals.
