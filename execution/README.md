# Real agent execution

`execution.Executor` runs AgentTurn and Review dispatches through `runtime.Direct`.
It requires a canonical task binding, a current Git-tree measurement, and durable
lease admission before each launch. Sessions are fresh private directories, and
reviewer outputs are collected separately. A change producer requires the host's
CheckChange policy before advancing the tree. Other roles must leave the tree
unchanged. Unknown post-launch failures carry `uncertain` so the daemon parks
instead of repeating potentially irreversible work.

## fae-agent-v1 trusted bridge

Configure an absolute executable and argument vector containing the exact token
`{config}`. No shell is used. The token becomes an owner-only JSON configuration
path. Configuration contains protocol `fae-agent-v1`, dispatch/task/session ids,
worktree, mode, provider/model route, exact tool profile, absolute `handoff` path,
`log_directory`, and the structured JSON brief also supplied on stdin. The bridge
must translate the profile into its provider's actual allowed/denied-tool launch
controls and refuse unsupported grants. This is a trusted adapter protocol, **not
an OS sandbox**: arbitrary commands claiming to implement it are not confined.
Do not configure a general agent CLI without a protocol implementation. Provider
credentials may be explicitly supplied in Environment; daemon environment and
credentials are never inherited. Each session receives isolated HOME and TMPDIR.

Write exactly one JSON handoff object to the configured file before exiting zero:

```json
{"dispatch_id":"...","session_id":"...","task_id":"...","output":{}}
```

Identity mismatch, id-less output, missing handoff, duplicate JSON keys, unknown
envelope fields, oversized files and symlinks are refused. Stdout/stderr are only
bounded diagnostic tails, never evidence. Engine schema validation remains the
final acceptance boundary. Output numbers retain exact JSON representations.

Cancellation and timeout kill only the process group created for this launch;
normal completion also cleans up descendants in that group. Bridges must not
escape the group with setsid or delegate to an unsupervised service. Session files
remain owner-only for diagnosis; durable indexing and retention are future work.
Fresh sessions currently select the first configured route; provider health,
route rotation, pooled sessions, interactive multiplexer adapters, positive CPU
stall detection and provider-specific log classification remain unimplemented.
Tests exercise actual subprocess launches, literal argv, secret stripping,
collection failures, cancellation, descendants, lease refusal/release, isolated
review sessions, current-tree checks and mutation rejection.
