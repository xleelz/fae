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

Retired Herdr IPC is preserved as metadata in `runtime-endpoints.json`, not as
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

`fae cleanup scratch-archive --source DUPLICATE --survivor CANONICAL` previews
removal of an exact redundant sibling of the completed scratch-cleanup archive.
Apply the returned digest with the same arguments plus `--apply --plan-digest
DIGEST`. The canonical survivor must be the path in the completed cleanup journal.
Both directories must belong to that scratch identity under `.scratch-archive`,
be private and owned by the current OS user. The instance and staged removal must
be absent. Every source file must match the verified survivor, and the original
Git archive refs must still verify. Unique, corrupt, foreign or special-file data
is refused. This never removes the canonical archive or its Git refs.

A durable removal receipt binds the journal, directory identities and file
hashes before deletion. A retry with the original digest accepts only unchanged
remaining source files; changed identities or newly introduced content are
refused. Every retry revalidates the canonical archive and Git evidence, including
a repeat after successful removal. The original scratch-cleanup journal is never
rewritten, so `cleanup scratch` can still verify its retained recovery evidence.

This is an offline local-owner operation: ownership, explicit apply and a
reviewed digest are its authorization boundary. It does not rely on an expired
message stored in a deleted instance. The same per-instance lifecycle lock
serializes it with instance recreation and scratch cleanup. It does not clean
arbitrary abandoned directories, incomplete archives, unique older backups,
remote copies, or the final recovery copy. Same-user malicious concurrent
filesystem mutation remains outside the ownership contract.
