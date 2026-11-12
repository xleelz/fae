# Backup retention and replication

These operator commands extend the verified plane backup prerequisite in
FAE-CP-013. They run outside provider workflows and the drive. They do not change
Local-mode effect permissions or create a remotely hosted daemon.

Create a backup with `fae backup plane --authorizing-message MESSAGE`.
The command returns its absolute path after Dolt restore/fsck and content hashing.

Preview retention with `fae backup retention --keep 3`. Apply the reviewed plan
with `fae backup retention --keep 3 --apply --plan-digest DIGEST
--authorizing-message MESSAGE`. The keep count is a minimum: historical job
snapshot coverage can retain additional backups. Every source referenced by a
replication journal is conservatively pinned, including completed replications.
Pins participate in the preview digest and currently have no unpin operation. The preview digest binds the
backup contents, manifest and filesystem identities. Unknown, incomplete,
foreign, symlinked or corrupted entries refuse the operation. A new backup or
changed identity invalidates a preview. Backup creation, retired cleanup,
retention and replication serialize within the daemon.

Retention only manages owned plane backups in the live instance's backup
folder. It does not prune externally retained scratch archives or Git archive
refs. An interrupted partial directory removal fails closed on a subsequent
scan and requires operator investigation; there is no force-delete repair.
These checks do not lock out a malicious process running as the same OS user.

Replicate a verified backup with `fae backup replicate --backup PATH --endpoint
https://backup-service.example --authorizing-message MESSAGE`. An endpoint is an
operator-configured service implementing the protocol below. HTTP is restricted
to literal loopback addresses for local services; redirects are refused.
`--max-bytes` bounds the disk-spooled tar archive, defaulting to 256 MiB with a
1 GiB ceiling. Scratch files live under the instance, and transfers stream from
disk. Normal completion removes the spool; SIGKILL can leave a private orphan
spool that requires operator inspection and removal. No vendor-specific service or credential-management system is installed.
Use a trusted, access-controlled endpoint: a plane backup contains instance data.

The archive is a deterministic tar of verified backup files and `verified.json`.
Its SHA-256 is the object ID. Before the first upload, a durable plane marker
records the exact endpoint, source path, archive hash, manifest hash and size.
Only that initial invocation can upload. Every subsequent invocation queries the
same object, including after a crash or lost acknowledgement. A missing remote
object does not authorize a repeat upload. A crash between saving the marker and
sending the request therefore leaves an uncertain operation for investigation.

The service protocol is:

- `PUT ENDPOINT/backups/SHA256`: tar body with Content-Length,
  Idempotency-Key equal to SHA256, and X-Fae-Manifest-SHA256. Return 200 or 201.
- `GET ENDPOINT/backups/SHA256`: return the stored receipt, status 200.
- `GET ENDPOINT/backups/SHA256/content`: return the exact archive bytes, status 200.

Both receipt endpoints return JSON with `id`, `sha256`, `manifest_sha256`, `size`
and `state` (exactly `stored`). Receipt identity must match the durable intent.
Fae then downloads and verifies the archive's full size and checksum before
persisting `replicated`. Repeated successful calls recheck remote content.
This verifies byte equality with a locally restore-tested backup; it does not
promise future remote availability, remote storage durability or confidentiality.
Remote service retention is the operator's responsibility.

## Automatic retention

`fae backup retention-schedule configure --interval 1h --keep 3
--authorizing-message MESSAGE` opts this instance into automatic plane-backup
retention. `status` inspects the durable policy and latest run; `pause` stops
future runs; `resume --authorizing-message MESSAGE` renews authorization and
reactivates a paused or blocked policy. Pause also requires an authorizing
message. The interval must be a whole number of seconds from one second to
365 days, defaulting to one hour. Choose an interval shorter than the remaining
authorization lifetime; a longer interval requires a longer configured message
maximum age. There is one policy per instance. Configuration replaces that policy.

The citation must explicitly grant the runtime class, or both
`BackupRetentionSchedule` and `BackupRetention` verbs. Plain operator prose does
not authorize recurring deletion. Each due run rechecks the citation's age,
restrictions and consumption state, and checks again between preview and apply.
Expired or missing authority blocks future runs until an operator resumes with
fresh authority. The normal message expiry applies; this is not an indefinite
authority grant. The policy creates no backups and does not replicate them.

A daemon background worker runs outside the drive, claims at most one due
occurrence and persists the next deadline before deletion. It obtains host
capacity, computes a fresh retention preview, and uses the same guarded apply as
the manual command. All historical coverage and replication-source pins remain
protected. Pause/resume/configure refuse while a run is busy. There is no missed
occurrence backlog. Interrupted running claims become blocked on daemon restart;
explicit resume computes a new plan instead of replaying a stale deletion list.
Filesystem or stale-plan errors are recorded in status; a later interval may
retry after revalidation. A partial deletion still needs operator investigation.

All three schedule workers (retention, creation, replication) are
deadline-driven (FAE-ARC-002): each sleeps until its record's next deadline or a
write to the record, so an idle daemon makes no plane reads for them; a due run
held by drain, maintenance, a concurrent operation or a plane error retries with
backoff from 1 s up to 30 s. A run that host admission defers before any write
withdraws its claim: no run is consumed, the deadline stays due, `LastError`
records the refusal and the audit outcome is `deferred`; a later tick retries
the same occurrence (replication keeps its durable selection).

## Automatic backup creation and replication

`fae backup creation-schedule configure --interval 1h --authorizing-message
MESSAGE` enables a singleton schedule that creates and restore-verifies local
plane backups. `fae backup replication-schedule configure --interval 1h --endpoint
https://backup-service.example --authorizing-message MESSAGE` enables a separate
schedule that selects the oldest verified backup not yet confirmed at that
endpoint. Both support `status`, `pause`, and `resume`; mutating lifecycle commands
require an authorizing message. The one-hour CLI default fits within a fresh
24-hour default grant lifetime. Explicit intervals accept whole seconds from one
second to 365 days, but must fit the remaining authorization lifetime to execute.

Recurring creation requires a structured runtime grant, or both `BackupSchedule`
and `PlaneBackup` verb grants. Recurring replication requires runtime, or both
`BackupReplicationSchedule` and `BackupReplicate`. Every run rechecks fresh
operator authority, including after waiting for host capacity. Creation and
replication honor drain, maintenance, shutdown and weighted host admission; they
run outside provider workflows and the drive. They do not replay a backlog of
missed timer occurrences or overlap their own runs. Status exposes the durable
policy, run count, running state, latest result and error.

Creation durably claims an occurrence before invoking the existing verified
backup path. An interrupted claim becomes blocked at restart. An error after
allocating a backup directory also blocks future creation, preserving its path
for investigation. A busy operation that allocated nothing can retry at a later
interval. Explicit resume starts a new occurrence rather than adopting an
unproven partial backup. Creation does not automatically prune its outputs;
configure retention separately if wanted.

Replication processes at most one source per interval, oldest first. Selection
and source protection serialize with retention. A durable pending claim pins
source path, endpoint, size bound and manifest digest before transfer. Pause and
restart preserve that claim. Configuration changes are refused while a claim is
pending; resume renews authority without changing the source or destination.
Startup itself performs no network calls. An attempted transfer uses the existing
durable replication outbox, so later runs query the original remote object and
verify its receipt and full content rather than blindly uploading again. A
selection interrupted before any upload attempt may safely begin its first
transfer once authority is revalidated.

The replication schedule accepts the same `--max-bytes` bounds as manual
replication and uses the same HTTPS/literal-loopback HTTP protocol. A verified
outbox record prevents repeated successful uploads in later selection rounds.
This is not continuous remote health monitoring or a promise that remote storage
will remain available forever. A pending claim without provable remote content
remains pending and prevents destination changes; there is no force-replay or
abandon command. Source pins remain conservative until an explicit verified unpin operation;
retention can preserve more backups than its keep count. Unique partial
backups and malformed evidence fail closed and require operator investigation.


## Replication pin release and storage quotas

`fae backup unpin --backup /absolute/instance/backup/plane-ID
--authorizing-message MESSAGE` previews release of a replication source pin.
Apply the returned digest with `--apply --plan-digest DIGEST`. Both operations
require current operator authority because they query remote replica evidence.
Every recorded destination must have a confirmed receipt and matching full remote
content. Pending or invalid outboxes and pending schedule claims prevent release.
The apply step revalidates the preview and remote evidence before changing the
pin index. It does not delete local data or remote objects. A subsequent successful
explicit replication protects the source again. Retention still preserves newest
backups and historical cleanup coverage, and an unpin invalidates earlier
retention previews. Remote proof establishes availability at that moment only.

`fae backup quota configure --max-bytes 1073741824 --max-backups 20
--authorizing-message MESSAGE` persists an instance policy; `backup quota status`
reports current usage and enforcement. Zero means unlimited for that dimension;
configure both values to zero to disable limits. Configuration replaces both
limits. Byte limits accept 0 through 2^60; backup counts accept 0 through 10,000.
A policy may be lowered below existing usage without deleting anything.

Accounting includes apparent regular-file lengths in the instance's `backup`
and `backup-replication` directories, including incomplete exports and orphaned
replication spools. All top-level backup directories count, including partials.
Foreign-owned objects, symlinks and special files fail closed. External scratch
archives, filesystem metadata and allocated block overhead are outside this
policy. Creation checks admission before allocating a directory and checks again
after export and final verification. Dolt export size is unknown beforehand:
this is an admission/checkpoint policy, not a hard filesystem quota. An overrun
preserves the output for investigation and blocks further growth. Replication
spool writes are bounded by remaining byte allowance. No quota automatically
prunes backups, releases pins, or deletes partial evidence; use reviewed retention
and unpin operations when their safety conditions are satisfied.
