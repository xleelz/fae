# External adapter and uncertain-effect recovery

`LiveConfig.external_tools` installs exact daemon-only HTTP tool adapters. Agent
provider sessions receive no additional tool grants. Each entry names an HTTPS
endpoint (plain HTTP is restricted to literal loopback), server, bare tool name,
role and complete string-valued `arguments`. An `ExternalEffect` workflow stage
selects the entry through `subject`; its `pins` must equal the installed argument
map and its role must equal the installed role. All Normal-live adapters require exact `destination` and `push_namespace`
argument pins matching the admitted disposition. Ticket tools additionally
require `ticket_room`; an optional ticket-room pin on another tool must also
match admission. Review creation additionally requires `branch`,
`draft:"true"`, and omitted or false auto-publish/merge flags.

Normal admission uses `--mode normal`. Without `--commit-external`, external
stages persist intent and return dry-run artifacts without effect HTTP calls; completion
is `Escalated` with `DryRunComplete`. Live admission also requires
`--destination`, `--push-namespace`, `--ticket-room` when ticket tools are used,
and an operator message
containing `I approve live external effects`. These are task dispositions; Local
tasks and provider session configurations retain their previous behavior.

The trusted service protocol is:

* `GET ENDPOINT/effects/KEY?task=TASK&digest=DIGEST`: read-only query; 404 means
  absent, 200 returns a receipt.
* `POST ENDPOINT/effects/KEY`: one mutation with an `Intent` JSON body and
  `Idempotency-Key` header. The service must atomically enforce that key. 200 or
  201 returns a receipt. Fae disables HTTP redirect following, request-body
  replay and persistent connection reuse; failures are uncertain.
* Receipt fields are `key`, `task`, `tool`, `digest`, `external_id`, `state`
  (`performed`), and `tree`; optional `commit_sha` is informational. The tool is
  exactly the pinned bare name or `mcp__SERVER__TOOL`. All identity, digest and
  tree fields must match the durable intent. Remote authentication/protocol
  translation belongs in the explicitly installed adapter service.

Intent pins endpoint/server, exact arguments, digest, tree, task, role-session and
workflow/stage identity before any network request. Review-create keys use task,
server/tool and recorded branch, so a revised workflow or different commit with
identical tree adopts the existing review. Ticket-create keys use task,
server/tool and the stage's outbox key. Generic effects additionally bind the
workflow/stage and argument digest. Normal execution queries before creation.
Every reported external ID is durably recorded as an unreconciled observation
before response validation, including pin mismatches and unknown response fields.

After a crash, `resolve-uncertain --action accept` accepts no operator-supplied
result. It checks the unchanged owned worktree/HEAD, exact durable intent and
pinned adapter, then performs only GET. A matching performed receipt can be
adopted; an absent, pending, changed or malformed receipt remains uncertain.
A 404 is never proof that a delayed earlier mutation cannot finish. Automatic
replay and explicit redispatch of a possibly executed effect are refused.

Native tests run an isolated HTTP fixture across SIGKILL after the server's
mutation but before its response/local receipt, then restart the real Dolt-backed
daemon and explicitly adopt with exactly one POST. They create no actual
third-party review or ticket.

## Discovery and read operations

Every installed alias requires `GET ENDPOINT/schemas`, returning an exact server
and MCP-shaped `tools` descriptors with `name`, `inputSchema` and, for reads,
`outputSchema`. Startup validates installed pins. See [SCHEMAS.md](SCHEMAS.md) for
bounded reads, exact names, typed argument encoding, daemon template rules and
room cache behavior. Intents carry `typed_arguments` and `schema_digest` in
addition to the original string pins. The adapter must invoke the vendor with
the validated typed arguments and atomically honor idempotency keys.

Read tools use `GET ENDPOINT/reads/KEY?task=TASK&digest=DIGEST&intent=ENCODED_INTENT`.
They return the ordinary matching receipt plus `data`, validated against the
installed output schema. A read never uses a mutation POST. Startup discovery
is read-only and still occurs for aliases later used by dry-run tasks; the tasks
themselves perform no effect/status/baseline requests.

This protocol deliberately uses configurable HTTP adapters. It does not ship a
native MCP transport, vendor SDK, account discovery or credential management.
The installed adapter must obtain real tool schemas and preserve their semantics
when normalizing vendor responses. An adapter claiming a permissive invented
schema is not evidence that a real vendor integration has been verified.

## Review read contract and lifecycle

A baseline read returns data with `package`, `integration_tip` and `status`.
Only `Pass` at the pinned package/tip is healthy. Review status data contains
`review_id`, `tree`, `build`, `analyzers` and `comments`. Each build/analyzer entry
names its `name`, `status`, own `required_for` set (`publish` and/or `merge`) and
optional `cause` (`change` identifies remediable failures). Comments include
explicit `id`, boolean `blocking` and boolean `addressed` fields. The adapter
must derive these fields from actual vendor schemas and severity encoding;
missing severity must never be inferred as nonblocking. Unknown/absent status
fails closed; Blocked and Skipped never count as Pass. A failed build blocks
publication. Merge also requires all blocking comments addressed.

Review policies explicitly bind the package workspace, branch, integration tip,
read aliases, operation names and vendor argument keys. Creation requires a
green approved baseline and draft-only arguments; publication and merge are
separate audited effects with fresh status gates. Instance/task opt-outs remove
the publication/merge stages. If merge is enabled, Done requires its recorded
success. A configured bounded implementation loop handles remediation and emits
a new revision on the existing review identity, defaulting to five revisions.

The daemon checks an existing physical package workspace and its reviewed tree.
It does not create, synchronize or merge provisioned build workspaces in the
hot path. A separate workspace must already contain the revised task content;
otherwise revision refuses. Configuration examples and constraints are in
[LIVE.md](../LIVE.md).

## Ticket orchestration

`ticket_workflow` selects explicitly enabled workflows and maps lifecycle
states/stages to exact room action names and installed aliases. Room resources
come from `GET ENDPOINT/rooms/ROOM/resources`; the response supplies action tool
names and vendor argument maps. Configured daemon bindings inject admitted task,
room, destination, push namespace and existing ticket identity. Missing actions,
undeclared keys, conflicting immutable pins and wrong types fail closed.

Desired transitions are reconstructed from durable task history and enqueued in
a per-task ordered outbox. Each row retains attempts, last error, intent,
started/emitted state and ticket identity. The creation key binds task/outbox
identity; subsequent steps reuse the recorded ticket. Started uncertainty allows
query reconciliation only, never blind mutation retry. Managed workflow ticket
stages share the same rows as lifecycle mirroring. A successful enabled review
merge permits Done, whose persisted transition drives the configured closed
room action. Status exposes pending/failed/uncertain outbox work, and maintenance
waits for unresolved emissions.
