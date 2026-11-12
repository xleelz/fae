# Scratch instance cleanup

Create a disposable instance with `fae serve --scratch ...`. The marker is
creation-only: an existing ordinary instance cannot be relabeled. It binds a
random identity to the canonical instance root, its device/inode, instance name,
workspace and message-age policy. Ordinary restarts preserve that identity without
requiring `--scratch` again.

`fae cleanup scratch` inspects the creation marker without stopping anything.
`fae cleanup scratch --apply --authorizing-message ID --dolt PATH` is a local
controller operation with a two-minute maximum budget. It validates runtime
operator authorization before stopping a reachable daemon, waits for its lock,
and reconciles only the instance's owned process registries. When already stopped,
it reads the same fresh operator authorization and consumed-class restrictions
from Dolt offline before any orphan-process recovery. If a live orphan plane
prevents that read, cleanup fails closed without killing it. Explicit grants can name `CleanupScratch` or class `runtime`.
A live daemon uses `ScratchCleanupAuthorize` before the ordinary Shutdown RPC.
Nonscratch instances, unknown identities, symlinks, ambiguous process ownership,
foreign worktrees and dirty/untracked/ignored worktree files are refused.

The controller takes an offline Dolt `backup sync-url`, restores it and runs fsck.
It also copies the instance's regular files into an owner-only sibling directory
under the configured instance home's `.scratch-archive`. Backup SHA-256 manifests
are streamed and rechecked before deletion. The backup is never inside the root
being removed. No shell HOME or user repository is removed.

Durable job, provider-loop and review-workspace markers identify worktrees;
unknown worktree entries refuse the operation. Every owned branch is archived
before any Git removal. The retired-cleanup manager provides exact branch/clean
worktree validation and archive-verified compare-and-swap ref deletion. An archive
preparation failure deletes nothing. Earlier successful deletions in an interrupted
batch remain protected by their archives and the external backup.

An external fsynced journal records preparation. All startup and cleanup operations
share an external lifecycle lock; pending removal refuses startup. Source file
changes after backup fail closed. After safe Git cleanup the root is atomically
renamed into `.scratch-removing/<identity>` before removal. A retry validates the
staged directory's original device/inode, backup checksums and Git archives, so
partial deletion can finish without relying on a marker file that may already be
gone. Every remaining staged file must still match its backed-up contents;
unknown or changed files and symlinks refuse deletion, while already-removed
files are allowed. Completed retries revalidate backup and archive evidence. A new instance
with the old name receives a different identity and cannot reuse the old journal.

Unfinished backup attempts are retained for inspection; a retry creates a separate
attempt. There is no backup pruning. Failed startup before an initialized,
authorizable plane cannot be cleaned automatically: the controller refuses to
remove data it cannot back up or authorize. Large backups can exceed the bounded
budget and leave a retryable, unverified attempt. The model proof covers the
scratch-marker plus verified-backup prerequisite; native tests cover actual Dolt
backup, Git archival, refusal and staged-recovery behavior.

Retired Herdr (`herdr-core`) IPC is preserved as metadata in `runtime-endpoints.json`, not as
pipe contents or socket streams. Only private, same-owner session `start.pipe`
and `output.pipe`, and resident `mux-client.sock` paths with matching durable
runtime markers are accepted. Resident owner death and existing process registry
recovery precede backup; endpoint type, inode, mode and UID remain in the source
manifest and are checked again before removal, including interrupted cleanup.
`lsof` is required when these endpoints exist. Cleanup enumerates the current
owner's open descriptor paths and refuses open endpoints, tool failures or
ambiguous escaped/non-ASCII endpoint paths. Unknown special files still fail
closed. These checks assume the same trusted local workload/owner boundary as
process recovery; they do not exclude a hostile privileged local process.

## Redundant archive cleanup

`fae cleanup scratch-archive --scratch-instance NAME --source DUPLICATE
--survivor CANONICAL` previews removal of an exact redundant sibling of the
completed scratch-cleanup archive. It runs through the serving instance's daemon
as the `ScratchArchiveCleanup` RPC (FAE-CLI-001): `--instance` names the live
daemon and `--scratch-instance` the removed scratch instance under the same home,
which must differ from the serving one. Apply the returned digest with the same
arguments plus `--apply --plan-digest DIGEST --authorizing-message ID`. The canonical survivor must be the path in the completed cleanup journal.
Both directories must belong to that scratch identity under `.scratch-archive`,
be private and owned by the daemon's OS user. The scratch instance and staged removal must
be absent. Every source file must match the verified survivor, and the original
Git archive refs must still verify. Unique, corrupt, foreign or special-file data
is refused. This never removes the canonical archive or its Git refs.

A durable removal receipt binds the journal, directory identities and file
hashes before deletion. A retry with the original digest accepts only unchanged
remaining source files; changed identities or newly introduced content are
refused. Every retry revalidates the canonical archive and Git evidence, including
a repeat after successful removal. The original scratch-cleanup journal is never
rewritten, so `cleanup scratch` can still verify its retained recovery evidence.

Only the instance operator may call it. Ownership, explicit apply and a
reviewed digest remain its authorization boundary; apply additionally needs a
fresh message in the serving instance granting class `runtime` or the verb
`ScratchArchiveCleanup`, checked without consuming it. It does not rely on an
expired message stored in the deleted instance. The same per-instance lifecycle lock
serializes it with instance recreation and scratch cleanup. It does not clean
arbitrary abandoned directories, incomplete archives, unique older backups,
remote copies, or the final recovery copy. Same-user malicious concurrent
filesystem mutation remains outside the ownership contract.

## Host command scratch

Host commands get on-disk scratch directories, never the RAM-backed system
temporary directory (FAE-HOST-020):

- a brokered run's TMPDIR (`worktrees/command-tmp-*`) and its policy
  directory (`worktrees/command-policy-*`: wrappers, nested-call records and
  the sealed GOROOT mirror), removed when the run returns;
- a landing's gate TMPDIR (`landings/<id>/tmp`), removed when the landing
  returns; the gate output the landing keeps beside it stays;
- `fae gate run`'s TMPDIR (`gate-runs/<id>`), removed when the command exits,
  with the owner record beside it (`gate-runs/<id>.owner`: the gate run's pid
  and kernel start identity), which gate run writes before it makes the
  directory.

Each is removed with `scratchfs.RemoveAll`. When a plain removal fails, it
makes every directory of the tree owner-writable (0700) and removes it again.
The tree's own directory is opened once without following a link (on Linux
with `O_PATH`, so even a mode-0000 directory opens), its mode is changed
through that descriptor, and the walk runs through an `os.Root` opened on the
same descriptor (its `/proc/self/fd` link; `/dev/fd` on macOS), which refuses
a link out of the tree. No step resolves the path again after it is checked,
so a link swapped in for the tree, or for a directory in it, changes nothing
outside (`TestRemoveAllUsesTheDirectoryItCheckedWhenTheTopIsSwapped`; other
systems still change and open the top by name). Go's module cache is read-only, so a test that points `GOPATH` or
`GOMODCACHE` into its TMPDIR leaves 0555 directories, and a run can unseal and
fill its GOROOT mirror. A run's or landing's directory that still cannot be
removed is logged as a `component=scratch` line; `gate run` prints its line
on standard error.

At start, after orphan termination and lease recovery, and only when no host
command's retirement is unproven, the daemon sweeps the run directories and
landing TMPDIRs a killed daemon left. A tree it cannot remove is moved into
`scratch-quarantine/<name>-<random>/<name>` under the instance root, outside
`worktrees` (where an unknown entry refuses scratch cleanup), and logged; one
it can neither remove nor move is logged and left. A leftover scratch tree
never stops the daemon starting.

Every start, live or scripted, also sweeps `gate-runs`. A gate run can be
running beside the daemon, so a `gate-runs/<id>` is removed (or quarantined)
only when its owner record proves the gate run dead and, on Linux, no process
still runs with it as `TMPDIR` or `GOTMPDIR` (a command a SIGKILLed gate run
started runs in its own process group and can outlive it; that scratch is kept
and logged until it exits). A scratch without an owner record (an older
binary's gate run) is logged and left; an owner record whose scratch is gone
is removed once its owner is dead.

Nothing removes a quarantined tree: remove it by hand (`chmod -R u+w`, then
`rm -rf`). So it cannot grow unseen, each start measures `scratch-quarantine`
(its entries, the apparent size of its regular files, at most 200,000 entries
visited, without following links) into status `startup_checks.scratch_quarantine`
(`path`, `entries`, `bytes`, the first 20 `names`, and `incomplete` when part
could not be measured), and, when it holds anything, logs it and raises a
`scratch-quarantine` warning alert. The alert is deduplicated on its detail: a
start that finds the same trees raises no second alert, and one that finds
more, or other, trees raises a new one. The measurement is taken at start, so
status does not see a tree removed by hand until the next start.

Scratch cleanup's final removal of the staged instance uses the same
removal, so a read-only tree in the instance, such as a quarantined one, does
not stop it (`TestScratchCleanupRemovesAReadOnlyTree`).
