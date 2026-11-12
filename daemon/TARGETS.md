# Versioned target configurations

`fae template target-config-put --id local --content target.json --authorizing-message MESSAGE` installs a new immutable revision and reports its SHA-256 content hash. Get/list are authenticated reads; `target-config-retire --id local --reason TEXT --authorizing-message MESSAGE` records a durable tombstone, preserving its first reason, actor and timestamp. Retirement blocks revisions and new admissions; already pinned tasks continue.

The strict v1 document is:

```json
{"schema_version":1,"workspace":"/absolute/physical/repository","integration_ref":"refs/heads/integration","protected_branches":["refs/heads/integration"],"allowed_commands":[["go","test","-json","./..."]]}
```

Declare every workflow HostCommand exact argument vector in `allowed_commands`. The integration ref must be declared protected. The workspace must exist, be a physical canonical directory, and equal the daemon workspace. Live targets must also match the daemon's integration ref. The existing live broker command allowlist remains an additional restriction; a target cannot broaden it. Unknown policy fields are refused, and so are placeholder expressions before schema 4's typed slots (below). Writes use the existing serializable, audited SQL boundary; request audit inputs are limited to 64 KiB. Listings fail beyond 10,000 revisions.

`fae run --target-config local --target-revision 1 ...` binds that revision. Omit revision or use zero to resolve latest once during admission. Backlog admission accepts `target_config` and `target_revision` on items and refuses retargeting existing identities. Cron templates accept the same selectors, resolving latest when the schedule is created; each occurrence checks retirement before admission. Execution and daemon recovery reread the pinned revision and verify its content hash, workspace and command policy. A later target revision cannot silently alter admitted work.

Target v1 remains Local-only. Schema v2 adds a required `normal` object (example below), permitting Local, Normal dry-run, and Normal live tasks on one daemon. Schema v4 adds command shapes with typed slots, always-required QA tests, a compile check and a diff classifier (below). Test-account deployments still need further schema and broker implementation; they are not inert accepted configuration fields. Existing tasks/admissions without a target selector preserve the legacy instance live-config binding. Full migration of those bindings is not claimed.

Tests cover exact command matching, schema/path refusal, CLI mapping, native SQL revision pinning and restart, retirement, cron binding, backlog upsert refusal, transaction rollback on audit failure, and retry against a concurrently advanced revision. Gobra checks the production command authorization conjunction; Gomela models policy handoff preceding command completion, with an intentionally broken negative control. These bounded models do not prove the complete filesystem or SQL implementation.

## Normal policy (v2)

Use the same base workspace/integration/command fields with `schema_version: 2` and:

```json
{"normal":{"destination":"main","push_namespace":"fae/example","ticket_room":"room-1","tools":{"ticket-create":{"endpoint":"https://adapter.example","server":"vendor","name":"create_ticket","role":"planner","arguments":{"destination":"main","push_namespace":"fae/example","ticket_room":"room-1"}}}}}
```

Every listed tool must exactly match an installed adapter's endpoint, server, tool name, role and static arguments. Every workflow external stage must be listed and match its tool role/arguments. Normal dry-run and live submissions must supply exactly the target destination, push namespace and optional ticket room. The live flag and separately recorded explicit operator authorization are still required to perform effects. Local mode never gains effects from this policy; dry-run records intentions and terminates as DryRunComplete without adapter effects.

Review targets additionally declare `review_task` and `review` (the full installed `external.ReviewPolicy` for that exact task), plus `disable_review_publish_merge` if the instance opts out. This pins the physical pre-provisioned workspace, task branch, package, baseline/status tools, operation and argument mappings, bounded polling and remediation policy. Both baseline and status tools must be in `tools`. Target writes rediscover schemas and run the approved package integration-tip baseline health read; missing workspace, wrong branch, absent capabilities or red baseline refuse the target as a configuration defect. Admission checks policy before baseline reads, and existing review execution rechecks physical workspace/tree and baseline gates. Workspaces are never provisioned by target operations.

Targets using managed ticket stages must include `ticket_workflow` exactly matching the installed orchestration policy, including discovered-room action/binding definitions. Discovery validates installed keys against runtime schemas. Unknown document fields are rejected.

Target v2 is an immutable restriction on installed configurable adapters, not a second adapter installer. Validation never changes shared configuration. Adapter/policy drift fails closed at startup, dispatch, external recovery and ticket outbox emission; a later target revision cannot silently alter prior pins. Existing retired revisions continue to run. V2 review targets are explicitly task-bound; dynamic review-workspace preparation and concurrent adapter definitions under one alias require v3 below.

Verification covers exact role/argument/endpoint and disposition matching, Local isolation, v1 compatibility, unknown runtime schema keys, native target revision/restart checks, native dry-run/create/adopt/malformed effects, and native review green/red baseline checks. The new Gobra conjunction and Gomela policy-before-effect model are bounded safety models, not proofs of the complete adapter protocol.

## Dynamic adapter installation (v3)

`fae template adapter-put --id vendor --content adapter.json --authorizing-message MESSAGE` installs an immutable schema-checked bundle without changing the live instance configuration. The v1 adapter document has `schema_version: 1`, `tools` (the exact alias/tool definitions above), optional `review_policies` keyed by task, optional `ticket_workflow`, and optional `disable_review_publish_merge`. Installation discovers every tool schema before its audited transaction; failed discovery or failed audit installs nothing. It configures already reachable adapter endpoints; it does not start or install vendor server executables. Use `adapter-get --id vendor --revision N` and `adapter-list` to inspect installed revisions.

Target v3 adds `normal.adapter: {"id":"vendor","revision":1,"hash":"..."}`. Omit revision/hash or use revision zero when putting a target to resolve latest once; the stored target always contains the exact revision and descriptor hash. Its normal tool/review/ticket declarations must match the selected bundle. Installation of a newer bundle does not alter existing targets or tasks. Registry documents and discovered schemas are persisted together, bounded to 100 tools, 1 MiB per installed document and 1,000 revisions per identity; audited request size is also subject to the control plane inline limit. Registry identities use a separate hashed marker namespace, avoiding target identity collisions. `adapter-retire --id vendor --reason TEXT --authorizing-message MESSAGE` records an immutable object-level tombstone, retaining its first reason, actor and timestamp. Get/list expose retirement status; retirement returns the first `{id, actor, reason, retired_at}` tombstone on every repeat. Retirement refuses later revisions, newly created targets, new admissions and new preparation bindings, including exact revision selectors. Already admitted tasks and durable pending preparations retain access to their pinned revision. Revision writes and new binding transactions recheck retirement under the event serialization boundary, so schema discovery that started earlier cannot install after retirement.

Runtime admission, execution, review completion/recovery and ticket outbox resolve fresh private configuration copies from the task's immutable target binding. They never modify shared maps. Current remote schema drift is refused before effect query/mutation. Normal dry-run performs no effect/schema call at execution. Review targets require an explicit ready `workspace review-prepare` record matching the adapter revision, task/run and policy; physical binding is revalidated without creation. The operator must prepare these workspaces before admitting work. Static target v1/v2 behavior remains compatible.

## Policy (v4)

Schema 4 accepts the v1 fields with an optional `normal` object (which, as in v3, must carry `normal.adapter`), plus:

```json
{"allowed_commands":[["go","test","-json","-count=1","-a","-x","./..."],["go","vet","./..."],["runner","--case","{{test-id}}","--","{{text}}"]],
 "required_tests":[{"name":"suite","argv":["go","test","-json","-count=1","-a","-x","./..."],"timeout_seconds":900},{"name":"daemon","argv":["go","test","-json","-count=1","-a","-x","./..."],"paths":["daemon/"],"timeout_seconds":900}],
 "compile_check":{"argv":["go","vet","./..."],"timeout_seconds":600},
 "diff_classifier":{"code":["**/*.go"],"not_code":["**/*_test.go","**/testdata/**"]}}
```

- **Typed command shapes (FAE-WF-035).** After the program, an argument may be a whole `{{type}}` slot of a placeholder type (`go-test-name`, `test-id`, `relative-path`, `go-package`, `text`, `integer`). Admission requires each templated HostCommand stage to fit a shape whose slots hold its placeholders' types; execution validates the exact bound vector against the shapes and runs it as literal arguments through a per-dispatch broker that allows only that vector. The instance live allowlist must still contain the stage template literally. Earlier schemas refuse slots.
- **Always-required tests and compile check (FAE-WF-085).** Each required test is a literal allowlisted vector with a timeout; `paths` scopes it by path prefix of the task's base..tree diff (none means always). Every whole-suite QA run (a test-run HostCommand without a declared shape) also runs the selected required tests and the compile check under its own heavy lease, folds them into its engine run (any failure fails it; executed counts add) and records a durable proof for the exact tree and target revision. A commit is refused, escalating `HumanDecisionRequired` with `RequiredCheckFailed`, unless that proof exists for the committed tree, whatever any verdict says; the commit itself runs nothing heavy.
- **Diff classifier (FAE-WF-073).** A changed path is production code when it matches a `code` glob and no `not_code` glob (`*` and `?` within a segment, `**` across segments). At commit, a task with recorded frozen RED evidence relies on it; otherwise a production-code diff escalates `NoRedViolation`, and a task its intake or spec declared no-red is refused when the target declares no classifier.

Earlier-schema revisions cannot carry these sections, so a stored v1-v3 revision keeps its exact meaning, and a task pinned to it keeps that policy through restarts and later revisions (tested natively).
