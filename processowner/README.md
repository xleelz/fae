# Owned process recovery

`Start` launches a dedicated process group behind a fixed shell stop gate. It
persists PID, PGID, Darwin microsecond birth time or Linux boot ID/start ticks,
parent birth identity, instance owner, command argv, and working directory before
releasing the command, as one record published once (see "Publishing records"
below). Arguments are positional; no command text is shell source.
A self-exec pipe watchdog inherits only the read end. Daemon death closes the
write end even under SIGKILL, so the watchdog checks identity, kills its proved
owned group, observes no live group members, and retains a retirement receipt.
`Register` plus `Watch` supports independently gated multiplexer pane groups.

Recovery runs under the instance lock before endpoint binding or execution. It
refuses foreign owners, reused identities, live original owners, unreadable
records/census, and groups whose original leader disappeared while helpers remain.
A refusal names the record's file; an empty record an older binary left is removed
instead (see "Empty and unreadable records" below).
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
SIGKILL; workload execution has not been permitted. If the gate's pending record
was already written whole, `Recover` terminates the gate under identity proof
(see "Publishing records"); a SIGKILL before that leaves no identity record
authorizing cleanup, and recovery must not guess ownership. Darwin execution is tested
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
`herdr-core` server plus pane use two. The admission journal consumes this proof only
after proving the recorded parent dead. Foreign, extra, duplicate, active, missing,
or malformed receipts cannot release a lease. A registry is shared by the sessions
of an instance, so a malformed record (wrong mode, invalid identity) whose JSON
still decodes and names another session, or for an acknowledgement another
parent, is skipped by `LinkedRetirement` and `AcknowledgeLinkedRetirement`: it
cannot be one of the caller's linked processes. A malformed record that does not
decode, or that names the caller's session and parent, still fails both closed,
since it might be the caller's own active record. A record that vanishes between
a scan's listing and its read (retired or acknowledged meanwhile) is skipped by
every scan. Tests: `TestRetirementScansIgnoreAnotherSessionsMalformedRecord`,
`TestLinkedRetirementSkipsARecordRetiredMidScan`. This permits interrupted-workload
recovery under the trusted no-detach contract while leaving incomplete evidence
withheld. Normal completion removes process receipts, then admission fsyncs its
retired state; a crash between those steps conservatively retains the lease.

## Publishing records

Every record reaches its final `process-*.json` name in one step, already written
and synced, so a concurrent scan of a shared registry (another session's
`LinkedRetirement` or `AcknowledgeLinkedRetirement`, `Recover`, `Inventory`) reads
it whole or not at all. `publish` writes the record under a pending name no scan
matches (`.pending-process-<publisher pid>-*.json`) and hard-links it to its final
name. Where the filesystem refuses hard links, it renames the pending file instead
with a rename that refuses an existing name (`renameat2` `RENAME_NOREPLACE` on
Linux, `renamex_np` `RENAME_EXCL` on macOS), so a record is never replaced either
way. **The registry directories, all under the instance root, must be on a
filesystem that supports one of the two**; on one that supports neither, every
launch fails with an error saying so. `StartRole` publishes the command and
directory in that same record; nothing rewrites a record in place afterwards
(retirement renames it). Tests: `TestStartRoleRecordIsNeverReadPartway`,
`TestRegistryScanNeverReadsAnUnwrittenRecord`,
`TestPublishWithoutHardLinksRenamesWithoutReplacing` (the no-link path is
simulated; it was not run on a filesystem without hard links).

A publisher that crashes before placing its record leaves the pending file.
`Recover` sweeps these at start, under the instance lock, without racing a live
publisher: a whole pending record names its publisher (the record's parent
identity) and is removed only once that publisher is proved dead, after the
stopped launch gate it names, if that exact process still exists, is terminated
under the same identity proof as an active record. A partial pending file is
removed once no process has the pid in its name. Pending files of live or
unprovable publishers, of other owners, and partial ones from before pending names
carried a pid are kept. Swept gates are not returned as recovered records. Test:
`TestRecoverSweepsOnlyCrashedPublishersPendingRecords`.

### Empty and unreadable records

Before wave 9, `StartRole` rewrote a published record in place to add its command
and directory (truncate, then write). A crash between the two left an empty
`process-*.json`, and a rewrite that failed and was then retired left an empty
`process-*.json.retired`. Wave 9's `publish` can't leave either under a final name,
but one left by an older binary stopped every start with a bare `EOF`
(`recover owned Dolt: EOF` for the plane registry).

`Recover` now removes such a file itself, under the instance lock, when it is a
private regular file (mode 0600, not a symlink), empty, and last written more than
50 ms before the recovering process started. The margin covers file times, which
come from a clock that advances once per scheduler tick: a file written just after
the process started can carry a time slightly before it (re-audit 9 measured
1.09 ms). It names no process, so nothing can be proved alive or dead from it.
Removing it signals nothing and releases no lease: a lease needs a complete set of
matching receipts, and an empty file is never one. A lease journalled for that
launch stays held, as for a launch that crashed before its record was written.
`RecoverClearing` returns each removed file, even when recovery then stops on
another record; the daemon writes a supervision line naming it at once and an
audited `empty-process-record-removed` event once the plane is up (FAE-ROB-014).

The launch the record was written for was still stopped in its gate when the
daemon died. Nothing identifies it, so recovery can't clean it up. The kernel
normally ends such a gate when its parent dies (an orphaned process group with a
stopped member gets SIGHUP, then SIGCONT). If the daemon ignored SIGHUP (started
under `nohup`, for example), the gate continued into its workload, which then ran
untracked. Look for a leftover process of the instance before relying on the
start.

Any other record `Recover` can't read (truncated or garbled JSON, the wrong mode, a
symlink, an invalid identity, or an empty file written after, or within 50 ms
before, the recovering process started) still stops recovery. The refusal names
the file, for example `recover owned Dolt: process record <path> cannot be read
(...)`. Check that no process it names (its pid and group, when the file decodes)
is still running, then move the file out of the registry and start again. The
other refusals (another owner's record, a live original parent, a parent death
that can't be proved) also name the record's file. Tests:
`TestRecoverClearsEmptyRecordsAnEarlierProcessLeft`,
`TestRecoverRefusesAnUnreadableRecordNamingIt`,
`TestRecoverClearingReportsEachRemovedRecord`,
`TestRecoverKeepsAnEmptyRecordWithinTheFileClockSlack`, and through a daemon start
`TestStartClearsAnEmptyProcessRecordAndNamesAnUnreadableOne` and
`TestStartLogsAndAuditsEachEmptyProcessRecordItRemoves` (need Dolt).

On Darwin an owned group's last live member may exit between census and a group
signal, yielding EPERM. Signal errors cause a fresh census; only a now-empty group
can be retired. A still-live or unreadable group remains an error.

`Census` is a read-only host process census (Linux `/proc`, `ps` elsewhere) that
never signals. A `Tree` attributes processes to an instance across censuses: the
root, descendants of members, members of groups led by a member (not the root's
own group, which a shell pipeline may share), and any process previously seen
as a member whose exact start identity is unchanged, so a descendant that calls
setsid stays owned after its parent exits. A process reparented before any
census observed it cannot be attributed. The tree also counts, per member, the
consecutive censuses that saw it defunct (`Process.DefunctRounds`). `Analyze`
sums the tree per command name (count, resident memory, defunct) against
`LeakLimits` (default 200, 4 GiB, 20; a value at a bound does not alert) and
lists foreign commands beyond a bound for reporting only. Given the brokered
commands running now (their census rows, matched by exact identity), a defunct
process inside one's tree (a descendant, or a member of the group it leads) is
left to that command for `CommandDefunctGrace` censuses (two: the one that
first sees it defunct and the next), reported as `DefunctInCommands` and not
held against the defunct bound; from the third census it counts like any
other, whether or not the command still runs. The report's `DefunctParents`
names the processes that have not collected their defunct children (command,
pid, how many, how many of them still graced, and for how many censuses the
oldest has been seen defunct), most defunct first, so a defunct leak says
which command they belong to. Teardown still uses the group records above.

## Inventory roles and the reaper (FAE-HOST-021, FAE-HOST-023)

Every record carries a `Role` (`StartRole`/`RegisterRole`): `dolt`, `agent-session`,
`multiplexer-server`, `agent-pane`, `resident-worker`, `host-command`, and `ui` for
the UI child. Legacy records without a role still read and recover. `Inventory(dir)`
lists a registry's live (unretired) records: pid, start identity, parent identity
and role.

`StartReaper` (called by the daemon at start; Linux) makes the daemon the child
subreaper of its descendants, so an orphan whose parent exits is re-parented to the
daemon instead of escaping to init. The reaper runs on SIGCHLD and at grace deadlines
only (no polling) and never steals an owned exit status: children started by
`StartRole` are explicitly owned by their waiter (`Wait`) and never collected. Fae
never forks a child into a foreign process group, so a defunct non-leader of a
foreign group is adopted and reaped at once. Any other unowned defunct child (an
orphan in the daemon's own group, a daemonized session leader, or an own fork whose
exec.Cmd waiter has not yet collected it) is reaped once it has stayed unclaimed for
`ReaperGrace` (5 s); exec.Cmd reaps at exit (Wait collects the process before
draining output), so an own fork is never left that long. At shutdown `KillAdopted`
kills and reaps every adopted child (foreign-group members and other-session leaders
that are not owned; they are descendants of the instance tree, and an unreaped
child's PID cannot be recycled), after the owned groups are retired. Own forks and
processes outside the tree are never signalled. `Wait` blocks in
`waitid(WEXITED|WNOWAIT)` (Linux) instead of polling the child's state.
