# Owned herdr-core runtime

`Multiplexer` implements the same `Run(context.Context, Launch)` handoff contract
as `Direct` using `herdr-core`. Supply an absolute `herdr-core` binary, an instance name, and
the Direct configuration. Each turn creates its own supervised headless server,
private HOME/XDG/config/socket, and instance-named terminal. It refuses existing
sockets or a server containing any terminal. Ambient `HC_*` and `HERDR_*` variables are removed;
no operator session or focused pane is inspected or adopted.

Implementation was checked against installed `herdr-core` (`v0.1.18`).
It uses the documented `serve`, `ls --json`, `run --bg --no-parent --no-notify --name <name> --cwd <dir> --json`, `get <addr> --json`,
and `server stop` commands with `HC_SOCKET`, `HC_SESSION`, and `$XDG_CONFIG_HOME/herdr-core/config.toml` (plus compatibility `HERDR_CONFIG_PATH` and `HERDR_SOCKET_PATH`).
Terminal identities come from returned JSON, not predicted IDs.

The server starts with no terminals. Fae creates a terminal with the canonical
task cwd and a generated gate script configured via `shell = [...]` in `$XDG_CONFIG_HOME/herdr-core/config.toml`. The pane blocks until
its exact OS birth identity and process group are recorded, linked to the durable
session, and protected by a parent-death watchdog. Then Fae delivers the permit
to execute the trusted bridge. A completion status is atomically published; the
pane's leader remains available for checked group retirement. Fae retires the
pane and helpers before stopping its server. Ownership uncertainty is an error.

The same strict bridge config, ordered brief, literal arguments, mode/profile,
identity-bound handoff, file-type/size checks, and diagnostic tail bounds apply
to both adapters. Output flows through a private FIFO to a bounded tail; terminal
output is never interpreted as a handoff. Socket paths must fit the platform
Unix-socket limit; long runtime roots fail with an explicit diagnostic.

Actual `herdr-core` tests cover shared Direct/`herdr-core` handoff conformance, literal shell
metacharacters, absent/malformed/stale/oversized evidence, nonzero exits, concurrent
sessions, ambient selection, refusal to adopt foreign workspaces, cancellation,
descendant retirement, and parent SIGKILL. Provider credentials are unnecessary
for these tests. A separate daemon integration runs real commands/Git effects
using an explicitly labelled bridge fixture; it is not real-provider dogfood.

One bridge process runs per turn (resident workers are in `resident.go`).
Persistent interactive sessions remain separate work; provider health failover,
activity-based stall detection and loop sessions are implemented elsewhere (see
[LIVE.md](../LIVE.md)). The transport startup deadline defaults to 180 seconds
(`start_seconds`); the shared Direct turn deadline defaults to 30 minutes. Stale socket nodes from a SIGKILL may remain
inside retained session diagnostic directories; they are never adopted.

Live admission now performs a host census with `lsof -n -U -a -c herdr-core -Fpn`
and queries `herdr-core ls --json` on each discovered API socket. It counts all
live terminals, including foreign terminals, without adopting or mutating them.
The separate `*-client.sock` attachment socket and `*.holder.*.sock` PTY holder sockets are excluded. A Herdr process with no
API socket is tolerated only when its command line (`/proc`, else `ps`)
positively identifies a client role, which hosts no panes; a server, an
unrecognised role or an unreadable command line fails the census closed. A server disappearing
between discovery and query causes a complete census retry (three attempts within
10 seconds); persistent census failure defers admission rather than treating the
missing answer as zero panes. Native tests create two distinct foreign terminals
on a private server and verify that both are counted. This census requires lsof
and permission to inspect local `herdr-core` processes; arbitrary renamed binaries
and invisible processes are not supported.

Cooperating Fae instances must share the configured `host_ledger` directory. Its
private `panes` subdirectory uses the same stable flock, atomic rename, and fsync
protocol as heavy-operation leases. Admission holds this lock across host sampling,
live census, pending reservation accounting, and reservation publication. A pending
reservation binds to its actual private socket before Herdr starts; once visible,
the live pane replaces the pending pane count. Memory remains reserved for 180
seconds after the first census that observes the pane, and remains reserved
throughout an uncompleted launch. Exact owner death reclaims reservations on every
access, while any surviving orphan panes remain counted by the independent census.
Normal release still requires the durable retirement proof. The default pane limit
is 60 (`pane_limit` in live configuration); workflow-global role caps plus one must
be strictly below it. Startup, workflow installation, admission, and dispatch check
that floor. Pane limits were pinned in this increment; they are now audited
runtime configuration (`runtime-config --pane-limit`, `serve --max-panes`).

## Adapter interface (FAE-RT-001)

`SessionAdapter` (`runtime.go`) is the contract shared by `Scripted`, `Direct`,
`Multiplexer` and `ResidentMultiplexer`: `Run` launches, delivers the brief,
observes readiness/activity/completion and collects the handoff file, a bounded
tail and the session's log directory, retiring the whole process tree before it
returns; `Live` lists live sessions with their owner, task, role, PID and
readiness; `Retire` stops one live session and waits for its tree's retirement.
`TestSessionAdapterConformance` runs one suite over the scripted, direct and
Herdr adapters.
