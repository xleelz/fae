# Owned process recovery

`Start` launches a dedicated process group behind a fixed shell stop gate. It
persists PID, PGID, Darwin microsecond birth time or Linux boot ID/start ticks,
parent birth identity, instance owner, command argv, and working directory before
releasing the command. Arguments are positional; no command text is shell source.
A self-exec pipe watchdog inherits only the read end. Daemon death closes the
write end even under SIGKILL, so the watchdog checks identity, kills its proved
owned group, observes no live group members, and retains a retirement receipt.
`Register` plus `Watch` supports independently gated multiplexer pane groups.

Recovery runs under the instance lock before endpoint binding or execution. It
refuses foreign owners, reused identities, live original owners, unreadable
records/census, and groups whose original leader disappeared while helpers remain.
Retirement receipts allow the next daemon to audit cleanup that happened while
its database was closed. `Retire` refuses a still-live group. Record directories
must be canonical, symlink-free, and mode 0700; records are mode 0600.

Tests send SIGKILL to a real parent and check its group disappears while a foreign
process survives; additional cases cover live-owner refusal, reused identity,
foreign owner, private-path rejection, literal arguments, and leaderless ambiguity.

This is process-group ownership, not an OS sandbox or a process-tree containment
proof. A trusted bridge/command must not detach descendants with setsid or move
out of its group. Unknown descendants cannot justify reclaiming a resource lease.
The tiny pre-registration stopped-launch window can leave a stopped gate after
SIGKILL; workload execution has not been permitted and there is no identity record
authorizing cleanup. Recovery must not guess ownership. Darwin execution is tested
locally; Linux identity parsing is cross-compiled and requires Linux runtime CI.
The check-to-signal boundary depends on kernel process-group identity and has no
pidfd-style atomic signal handle on Darwin. Receipt retention is currently unbounded.

Normal child completion uses `Wait`: it observes the child as an unreaped zombie
using native OS process state, validates its exact birth identity, and terminates
remaining group members before `Cmd.Wait` reaps the leader. An unreaped child PID
cannot be recycled. On Darwin, where `getpgid` rejects a zombie, the retained
exact identity plus zombie state anchors the dedicated PGID recorded at launch.
This removes the former post-reap raw group signal from direct execution, host
commands, and owned Dolt. Failed retirement stays visible; temporary command
registries are retained on unresolved cleanup instead of deleting ownership proof.

Process records can carry an immutable session ID through `StartLinked` and
`RegisterLinked`. `LinkedRetirement` accepts only a complete expected set of
terminal receipts matching session, instance process owner, exact parent birth,
and distinct child identity. Direct and host-command launches use one record;
herdr server plus pane use two. The admission journal consumes this proof only
after proving the recorded parent dead. Foreign, extra, duplicate, active, missing,
or malformed receipts cannot release a lease. This permits interrupted-workload
recovery under the trusted no-detach contract while leaving incomplete evidence
withheld. Normal completion removes process receipts, then admission fsyncs its
retired state; a crash between those steps conservatively retains the lease.

On Darwin an owned group's last live member may exit between census and a group
signal, yielding EPERM. Signal errors cause a fresh census; only a now-empty group
can be retired. A still-live or unreadable group remains an error.
