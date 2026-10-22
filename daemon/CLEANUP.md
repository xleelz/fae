# Retired task cleanup and plane backups

`fae backup plane --authorizing-message ID` creates a fresh owner-only directory
inside the instance's `backup` directory. It uses Dolt's `sync-url` backup,
restores it into a separate repository, reads the restored job metadata, runs
`dolt fsck`, then records SHA-256 hashes of every backup/restore file. No database
files are copied from the live server. No global Dolt identity or backup remotes
are modified. Live instances acquire a weighted host admission lease for backup.

`fae cleanup retired` plans without changes by default (`--dry-run` is explicit).
`--apply --backup PATH --authorizing-message ID` applies the plan, with `--pace`
(default 100ms, range 10ms–1m) between tasks. Both methods require the instance
operator. Backup and apply require a recent operator message; cleanup only reads
that message and the plane, without consuming it or writing audit rows. Existing
consumed-class compatibility is checked, and explicit verb/class restrictions
remain binding. A plain fresh operator message retains the usual operator
authority. Compatible runtime authorization can be reused because cleanup does
not consume it. Backup
and cleanup RPCs have a bounded two-minute budget; normal RPCs keep five seconds.
Shorter request deadlines still apply. Large backups can time out and safely
leave an unverified backup directory for investigation; cleanup refuses it.

Only durable Retired live tasks with no active or unresolved execution qualify.
Each task must have the same version and payload digest in the restored backup.
All worktree plans and backup coverage are checked before starting. All immutable
per-tip archive refs are created and verified before any worktree is removed.
Archive refs are `refs/fae/archive/<run-task-identity>/<commit>` and preserve the
entire ancestry reachable from the task branch tip. Cleanup verifies deterministic
ownership, repository identity, exact tip, registration, and clean tracked,
untracked, and ignored files. A branch registered at another path is refused.
No force or recursive filesystem deletion is used.

Before each removal, cleanup rechecks durable task state, version, payload,
backup hashes and the Git source. Branch deletion uses a Git reference transaction
that verifies the archive and compares the original branch tip. Concurrent
external Git mutation fails closed; an interruption may leave an archive, or an
archived branch without its worktree, which a fresh invocation can safely finish.
Repeated cleanup with valid archive evidence is idempotent across daemon restarts.
An archive or backup failure during preparation deletes nothing; a later source
change can leave earlier tasks safely cleaned and refuses the remaining work.

This implements FAE-CTL-042 and the retired-ref prerequisite of FAE-CP-013.
Scratch-instance destruction (FAE-CTL-043) is available through the separate
[marked scratch cleanup controller](SCRATCH.md). Owned plane backups support explicit [retention and remote replication](BACKUPS.md).
Externally retained scratch archives are not pruned. Archive refs remain
local to the task repository. Administrative deletion of archives/backups outside
Fae is outside the ownership contract. Guard/model proofs cover the explicit
backup-plus-archive prerequisite, not Git or Dolt internals.
