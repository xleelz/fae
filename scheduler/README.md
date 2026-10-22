# Scheduling and durable resource pools

`Pool` implements weighted, oldest-first admission, with priority class only
breaking equal-time ties. It never skips a blocked head to fit a younger request.
Defaults are six-hour TTL, one waiting event at 300 seconds naming holders, and
1800-second maximum queue wait. `CanAcquire` and `CanRelease` are actual production
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
for role scopes in an instance's private SQL plane. `RolePool` identifies global
roles by workflow ID and role, without revision; task roles also include molecule.
There is no host-wide role cap. Differing pool configurations refuse rather than
silently changing an active capacity. Live revision cap reconciliation is pending.

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
one terminal process receipt; herdr sessions require exactly two (server and pane);
HostCommand stages require one. Each receipt must match session ID, instance
process owner, exact parent birth and a distinct process identity. Recovery first
retires the role request in a Dolt transaction, then the shared host request under
flock, with a durable audit each; the journal remains until both succeed. Repeating
a partial recovery is idempotent. Other instances' requests are untouched.

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
