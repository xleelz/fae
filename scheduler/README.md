# Scheduling and durable resource pools

`Pool` implements weighted, oldest-first admission, with priority class, then
task priority, only breaking equal-time ties. It never skips a blocked head to
fit a younger request.
Defaults are six-hour TTL, one waiting event at 300 seconds naming holders, and
1800-second maximum queue wait. The daemon turns that waiting event into exactly
one `host lease waiting` log line and one `heavy-lease-waiting` event, on the
waiting task's history when the waiter is one of its tasks and on the instance
stream otherwise (FAE-SCH-011 clause 4). An `Unbounded` request (every role-queue
request, FAE-SCH-008) is exempt from the maximum wait: it keeps its place until
granted, cancelled or its owner dies, so the longest role waiter is granted next.
Heavy-op requests stay bounded and are refused at the maximum wait.
`CanAcquire` and `CanRelease` are actual production
predicates verified with Gobra; exhaustive unit tests cover their finite domains.

`OpenLedger(physicalAbsoluteDirectory, capacity)` stores a host-wide heavy pool.
Every daemon on the same host must use the same directory and capacity. Operations
hold a stable OS flock; state and audit records are written together via a private
temporary file, fsync, atomic rename and directory fsync. Independent processes
cannot oversubscribe. Corrupt/missing state, conflicting capacity, symlinks and
nonprivate state/lock files fail closed. The retained audit history has a 64MiB
ceiling and refuses further changes pending archival rather than discarding
history. Automatic archival and filesystem failure injection remain future work.

`controlplane.Store` resource-pool APIs provide the same durable queue semantics
for role scopes in an instance's private SQL plane. There is no host-wide role
cap. A workflow-global pool is keyed by workflow ID and role, shared by every
revision, and uses the cap of the newest revision with a non-terminal task
(`RolePool`, FAE-SCH-006); a task-bound pool is keyed by molecule, one per
(workflow ID, revision), so its tasks share the slots (FAE-SCH-005). A changed
cap governs future grants only: holders keep their slots and drain, and the
validation ceiling never shrinks below them. The daemon reports the choice in
status (`scheduler.role_caps`) and sweeps, every round, role leases of its own
that no admission still owns (FAE-ROB-015).

The heavy ledger also holds Direct-session launch reservations
(`ReserveLaunch`): memory available must cover the hard floor plus (pending + 1)
per-session estimates, counting every reservation still in its 180 s warm-up
host-wide. A dead owner's reservation is dropped on the next access.

Liveness is Alive, Dead or Unknown. Expiration never authorizes reclaiming a
resource. Pool arithmetic retains the legacy PID/second timestamp owner tuple;
production recovery additionally pins the exact kernel birth identity in a
private per-instance operation journal, so a coarse timestamp alone cannot
authorize reclaiming a launched session.

The journal is fsynced before host enqueue, links the exact host request and role
pool, and records `reserved -> launching -> retired`. The launch transition is
persisted before an executor may run. Successful normal cleanup is recorded before
releasing either pool. Startup under the singleton lock can recover a dead owner's
never-launched or retired reservation. Interrupted Direct sessions require exactly
one terminal process receipt; `herdr-core` sessions require exactly two (server and pane);
HostCommand stages require one. Each receipt must match session ID, instance
process owner, exact parent birth and a distinct process identity. Recovery first
retires the role request in a Dolt transaction, then the shared host request under
flock, with a durable audit each; the journal remains until both succeed. Repeating
a partial recovery is idempotent. Other instances' requests are untouched by
journal recovery; while waiting for the heavy lease, an admission reclaims the
entries of a foreign owner proven dead by pid and start time (FAE-HOST-031) and
keeps owners whose liveness is ambiguous.

This relies on the documented trusted-command contract that children stay in their
owned process groups. It is not hostile-code containment. Missing registrations,
missing/duplicate/extra/foreign receipts, a live/ambiguous owner, unsupported Git
operation process sets, or unproved cleanup keep capacity reserved. The narrow
crash window after normal process receipt deletion but before journal retirement
also stays withheld. Timer expiry is never substituted for evidence. A malformed
journal or ledger blocks recovery. External/legacy leases without these linked
proofs remain Unknown for operator reconciliation. TTL-driven process termination
and archival of retained journals/receipts remain future work.

Validation includes an eight-process host-ledger contention test, persistence
reopen, FIFO head blocking, owner mismatch, cancellation, corruption, symlinks,
expired Alive/Unknown retention and real-Dolt concurrent clients/crash recovery.
The Gomela model verifies a bounded two-worker acquire/exit/release protocol and
rejects a missing-release control; it abstracts filesystem, SQL and process I/O.

Recovery validation includes exhaustive proof-gate cases, private/corrupt journal
refusals, a subprocess SIGKILL after retirement fsync, and real daemon SIGKILL
while a leased host workload is running. The latter restarts private Dolt and
reclaims both role/host pools from watchdog receipts while retaining a foreign
holder. A missing-proof model remains blocked as its negative control; the
bounded model does not prove filesystem, SQL, or OS behavior.

## Workflow slots and ledger compatibility

Workflow slots use the same stable host directory and cross-process lock as
heavy-operation and provider leases. Enabling workflow slots upgrades the shared
ledger header from version 1 to version 2 under that lock only after every live
lease, pane, host-policy, provider-pool and workflow owner has advertised schema
2 support. Otherwise startup refuses with the identities of unaware participants
and leaves version 1 unchanged, so their existing operations and restarts work. Existing heavy-pool,
provider-pool and audit data remain intact. Current binaries read both versions;
a version 1 binary refuses version 2 before invoking a mutation, rather than
silently dropping the workflow holders and requests it cannot decode.

This requires a coordinated rollout of the instances that share a host ledger.
A new workflow acquisition waits with `host_ledger` while a live host-policy
owner has not registered as a slot-aware participant. The fence lifts only after
that owner upgrades and registers, or its exact process identity is proved dead.
Existing holders may continue and startup re-establishment remains exempt from
new-acquisition ceilings. Mixed old and new binaries are not silently treated as
fully participating in workflow-slot accounting. The isolated tests use private
ledgers; they perform no rollout or migration of an installed host ledger.

Each slot transaction prices unused committed memory and panes before adding a
holder. Existing-holder enlargement reserves only the additional unused
commitment; its baseline never shrinks while that holder remains live. Detached subtrees
are individually priced additions, released after the subtree finishes. The
default-workflow ceiling is a reported estimate; acquisitions use each actual
candidate footprint and the requested count limit.
Keeping the reference estimate unchanged means keeping the SCH-019 formula,
not freezing its numeric result as commitments change. For example, with
14.5 GiB uncommitted and a 7 GiB reference, the initial estimate is 2. Five
2.8 GiB holders can acquire; the same formula then reports 5 held plus zero
additional reference holders. The sixth 2.8 GiB candidate waits. Request
reductions do not revoke a holder. Waiting spawn retries use a bounded timer only
while a holder waits; the separate usage refresh maintains resident-memory and
pane observations without polling spawn decisions when there are no waiters.

Session counts and reference counts are priced inside that lock using the
conservative shared per-session reservation, so a local lower reservation cannot
undercut another instance's policy. Process-adapter holders commit no panes;
pane-adapter holders commit their session count. A task persists its last granted
binding separately from a newer footprint still awaiting capacity. Restart
restores that granted commitment, including conservative larger ancestor grants,
rather than exempting an ungranted migration target from capacity checks.
