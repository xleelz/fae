# RPC authority

The owner credential authenticates the operator. Request fields never select the actor.
Start the daemon with `serve --principal-config /absolute/path/principals.json`
to configure delegated credentials. The configuration is an owner-only regular
JSON file (mode 0600), containing for example:

```json
[{"credential_file":"/absolute/path/controller.credential","actor":"controller"},
 {"credential_file":"/absolute/path/task-prophet.credential","actor":"prophet","task":"task-id"}]
```

Use a private directory outside the repository. Missing credential files are
created with 32 random bytes and mode 0600; existing files are reused across
restarts. Supported actors are controller, prophet, workflow-role, and qa. The
daemon itself issues the `ui` (read RPCs only) and, with `serve
--ui-allow-writes`, `ui-writer` credentials to its UI child; the configuration
may not declare either. `ui-writer` may call mutating RPCs other than
`ProphetChat`, but its cited message must direct the exact verb, and
operator-only handlers (`UIToken`, backups, cleanup, ...) still refuse it (see
[ui/README.md](../ui/README.md)).
Delegates send their credential through the existing authenticated Unix RPC API.
The operator CLI continues to use the owner credential. Removing a binding and
restarting revokes that delegated credential. Credentials never belong in messages.

A free-text operator message authorizes an operator action and is consumed for its
first successful action class. Further actions in that class may reuse it until
its age limit expires. An incompatible class requires another message, or explicit
multi-class directions. To direct a controller, record message text such as:

```json
{"text":"Install this workflow and admit tasks","authorize":{"verbs":["WorkflowInstall","Admit"]}}
```

Alternatively explicitly grant classes (`task`, `governance`, `runtime`,
`controller`, `schedule`) using `authorize.classes`. `fae reclaim --host-wide`
additionally requires a message that explicitly grants the `host-wide` class
(or the verb `Reclaim.host-wide`), even for the operator. Controller subactions use
exact names such as `Controller.rearm` and `Schedule.create`. A Prophet requires
an exact verb direction; only `RestartWithInformation` and `ResumeWithContext`
are enacted. It cannot change governance or quality dispositions. Per-task
credentials are confined to their task history and those two controls; they
cannot open the instance event stream. Workflow roles and QA cannot mutate RPC
control state. Artifact production and quality artifacts retain their separate
stage capabilities; this table does not create a new artifact-write endpoint.

Consumption and the mutation's audit row commit atomically. Internal writes
reached from that RPC inherit the citation. Rollback preserves the message's
previous consumption. Explicit lifecycle/watchdog continuations retain their
existing durable-grant age exemption; new actions still require a fresh message.

## Messages, replies and relays

`fae message --text|--file [--task ID] [--id ID]` records a thread root and
returns its id; the daemon assigns the id unless one is supplied.
`fae reply --to ID --text|--file` records a reply in the same thread (the
thread root and the task scope are inherited). Either id may be cited later as
an authorizing message. `fae inbox [--task ID] [--kind K,...]` reads threads,
escalated, blocked and uncertain-parked tasks, HumanDecision parks (with their
question, options and pending decision id) and unanswered Prophet proposals or
questions from the plane. `fae decide --task ID --option OPTION` disposes a
HumanDecision park under the `task` class (`Decide`); a Prophet never enacts it.
`QuarantineInspect` is a read; `QuarantineRelease` and `QuarantineRetire` are
`task` class. `ScratchArchiveCleanup` is `runtime` class and, like `UIToken`
(a read), is answered only for the instance operator.

A controller may call `Message` only as a relay: `relayed: true` with a
non-empty `source` naming where the operator's verbatim words came from. A relay
is stored with author `relay`, threaded and shown with its source, but it never
satisfies an authorizing-message check, because a controller has no authority
of its own. The operator records messages directly and may not mark them relayed.

## Listeners (FAE-SEC-001)

Every listener Fae runs authenticates its caller; file mode alone never
grants control:

- the instance RPC socket: the per-instance credential (FAE-ARC-005);
- the optional UI child: loopback only, a token on every request (FAE-API-031);
- a resident worker's request socket: a per-worker key delivered in a private
  file, never on argv;
- the controller runtime launcher (`fae runtime-launcher --socket S`, started
  per instance with its `--home`/`--instance`): a fresh 32-byte key in
  `launcher.key` beside the owner-only socket, compared in constant time. The
  launcher runs only a launch that the instance's maintenance marker authorized
  for that socket while the daemon is stopped, and hashes the binary against the
  marker's digest, never against one the caller sends. Full roll and maintenance
  operations also execute here so they survive daemon replacement. Roll/stop
  submissions require an HMAC over the action and all options, signed using the
  fixed instance credential only after the daemon authorizes and audits the
  `ControllerOperation` RPC. A launcher key alone cannot authorize these
  operations or redirect one instance's authorization to another. Offline start
  still requires the exact authorization and launch recorded in the maintenance
  marker. Each phase uses the existing validated Controller RPC;
- Dolt: 127.0.0.1 with a password.

Boundary: Herdr's own API sockets (one `mux.sock` (`HC_SOCKET`) per `herdr-core` multiplexer session) are
unauthenticated by Herdr's design. Fae creates each inside the session's private
0700 directory under `<root>/sessions`, so only the instance owner can reach it;
Fae does not add authentication to Herdr's protocol.
