# Owned herdr runtime

`Multiplexer` implements the same `Run(context.Context, Launch)` handoff contract
as `Direct` using herdr. Supply an absolute herdr binary, an instance name, and
the Direct configuration. Each turn creates its own supervised headless server,
private HOME/config/socket, and instance-labelled workspace. It refuses existing
sockets or a server containing any workspace. Ambient HERDR variables are removed;
no operator session or focused pane is inspected or adopted.

Implementation was checked against installed herdr 0.9.5 and its bundled API
schema (protocol 22), plus the [official documentation index](https://herdr.dev/llms.txt).
It uses the documented `server`, `workspace list/create`, `pane process-info`,
and `server stop` commands with `HERDR_CONFIG_PATH` and `HERDR_SOCKET_PATH`.
Workspace and pane identities come from returned JSON, not predicted IDs.

The server starts with no workspaces. Fae creates a workspace with the canonical
task cwd and a generated gate script as its default shell. The pane blocks until
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

Actual herdr tests cover shared Direct/herdr handoff conformance, literal shell
metacharacters, absent/malformed/stale/oversized evidence, nonzero exits, concurrent
sessions, ambient selection, refusal to adopt foreign workspaces, cancellation,
descendant retirement, and parent SIGKILL. Provider credentials are unnecessary
for these tests. A separate daemon integration runs real commands/Git effects
using an explicitly labelled bridge fixture; it is not real-provider dogfood.

One bridge process runs per turn. Persistent interactive sessions, provider
readiness/health failover, activity-based stall detection, and loop sessions are
separate work. The transport startup deadline is 10 seconds; the shared Direct
turn deadline defaults to 30 minutes. Stale socket nodes from a SIGKILL may remain
inside retained session diagnostic directories; they are never adopted.

Live admission now performs a host census with `lsof -n -U -a -c herdr -Fpn`
and queries `herdr pane list` on each discovered API socket. It counts all
workspaces, including foreign workspaces, without adopting or mutating them.
The separate `*-client.sock` attachment socket is excluded. A server disappearing
between discovery and query causes a complete census retry (three attempts within
10 seconds); persistent census failure defers admission rather than treating the
missing answer as zero panes. Native tests create two distinct foreign workspaces
on a private server and verify that both are counted. This census requires lsof
and permission to inspect local Herdr processes; arbitrary renamed Herdr binaries
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
that floor. Pane limits remain pinned configuration in this increment.
