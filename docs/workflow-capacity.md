# Workflow capacity

`fae workflow-limit` reads this instance's request, the effective host request, host ceiling, slot holders and waiting holders. `fae workflow-limit --max N --authorizing-message MESSAGE` requests a limit; `-1` withdraws the request. Values other than `-1` or `1..1000000` are refused. Lowering a limit affects future acquisitions and leaves running work alone.

The operator may request a limit. A controller must cite a message explicitly granting the `capacity` class. A `runtime` grant or a grant naming only the verb is insufficient. The capacity class authorizes this request alone: it cannot change host thresholds, reservations, panes, role caps or tree session limits. Host measurements always bound the requested number. Status and Metrics include the same `workflow_limit` view; unavailable ledger reads are reported as errors rather than an unlimited capacity.

Workflow content declares `slot_scope: molecule` (the default) or `slot_scope: tree`. A molecule footprint counts distinct agent roles along a path, individual review sessions, a loop body once, and sessions of pinned child revisions. A tree footprint bounds peak concurrency: parallel branches combine their peaks, dynamic branches use their maximum width, and children and bounded recursion are included. A root `max_sessions` supplies an explicit tree bound. `session_count` may declare a larger reservation, but cannot understate the derived count. A new revision whose child footprint cannot be resolved is refused before installation.

Validation and simulation report `footprint.derived_sessions` and `footprint.effective_sessions`. Installed content and pinned child revisions determine these counts; tasks persist their effective footprint at admission. Memory is priced at acquisition using the active per-session reservation, and panes use the same session count under a pane adapter. Neither figure bypasses launch admission.

Session history uses `attempt` within a stage visit and branch, continuing across resumes. `sequence` orders all session readings in the task. Provider readings remain exact or absent. Native Claude main-session `tool_use` blocks produce bounded, content-free tool metadata; policy refusals remain visible after the allowance-event limit is reached.

Offline hierarchical authoring can resolve exact child pins without an instance:
`fae workflow validate parent.json --catalog workflows.json` or
`fae template workflow-validate --content parent.json --catalog workflows.json`.
The catalog is a JSON map from `id@revision` to workflow documents (`id` means revision 1); a scenario's `workflows` object is also accepted. Missing pins refuse complete validation instead of silently omitting the footprint.

Tree statistics now use top-level stage rollups. `stage_details` retains leaf stages and nested fan rollups for drilldown; these overlapping details should not be summed together. Recursion refusals include the kernel's child-refusal and failure-route records.

Offline authoring and rehearsal accept repeated `--catalog FILE` arguments;
conflicting definitions are refused. Catalog checks include exact child pins,
input and output signatures, alias schema compatibility, and budgets on every
member of a recursion cycle. `fae workflow simulate FILE SCENARIO TASK --tree`
prints qualified fan/branch identities, failure routes, refusals, and incoming
change requests. A root that does not reach Done returns a nonzero exit status
with its state and reason. Tree statistics attribute recovery sessions to the
primary stage and expose them separately under `handlers`.
