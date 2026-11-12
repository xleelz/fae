# Trees and execution statistics

`fae tree TASK --stats` reads the persisted run tree at one event sequence. It includes attached and detached children, parent links, branch counts, mapped outputs, recursion refusals, and ancestor change requests. `fae tree-limit --task TASK --max-sessions N --authorizing-message MESSAGE` changes the tree's session ceiling; use `-1` for no ceiling. Reductions affect future admission and do not terminate existing sessions.

Provider turn counts, duration and cost are exact only when the provider reports them. Missing readings remain `absent`; FAE does not infer token counts or prices. Session wall time is measured around execution, while task/tree elapsed time uses persisted lifecycle timestamps. Parallel session durations are summed, while elapsed time spans the tree. Completed statistics remain available when event retention removes old events.

A stage declaring `run_stats: tree` or `run_stats: task` receives the immutable `engine:run_stats` artifact. Its sequence identifies the read snapshot. Workflows cannot declare that producer or impersonate its provenance.

`fae events` and `fae events --follow` accept `--task`, `--tree-root`, `--run-id`, `--workflow`, and `--kind` filters. Reconnect using the last cursor; expired cursors report an explicit gap. Tool-call events contain tool names, worktree-relative paths, timestamps and allowance decisions, without command bodies, file contents or tool output. Successful calls are rate/turn bounded, while refused calls are retained.

`fae change-request list --task TASK` reads ancestor requests. Operators can record a disposition using `fae change-request disposition --task TASK --id REQUEST --state accepted|declined --reason TEXT --authorizing-message MESSAGE`. A request never rewrites the ancestor's artifacts by itself.

`fae resume-at-stage --task TASK --authorizing-message MESSAGE` resumes the failed stage under its pinned workflow. Targets may set `restart_only: true` to refuse this operation and require a full restart. Installed target revisions remain immutable; policy changes require a new revision.
