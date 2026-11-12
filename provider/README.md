# Restricted Claude Code and Codex bridges

`provider.Run` implements the `fae-agent-v1` protocol for a real Claude Code or
Codex CLI. `Config.Provider` selects the protocol (`cmd/fae-claude-bridge` and
`cmd/fae-codex-bridge`); the workflow binding's provider must name the same CLI.
Claude is the default provider for new configurations. The task scope, prompt,
edit policy, output-schema validation, handoff publication and
persistent-context cache below are shared by both bridges.

## Task scope

A turn's scope is its files (read in full and the only editable files) and its
RED test file. It comes from one of two sources, never their union:

- **Pinned:** a `--task-scopes` entry for the task (a JSON literal mapping task
  IDs to `{"files", "test_file"}`), or a non-empty `--files` with an optional
  `--test-file`. With `--task-scopes` set, a task without an entry is refused.
  `--test-file` without `--files` is refused.
- **From the brief:** with nothing pinned, the brief's `task_scope` block
  (`{"kind": "task_scope", "files": [...], "test_file": "..."}`) is the scope.
  Its test file must be one of its files.

Every pinned path and every path in the block follows the daemon's one
declared-path rule (`workflow.DeclaredPathSafe`: relative, clean, and no `.git`
component at any depth, in any letter case), and a block path other than an
approved widen file (below) must also reach no symlink; one bad path refuses the
turn before the provider runs. Under a pin, a `test_file` the block names must
equal the pinned test file: a mismatch means the daemon and the bridge
disagree, and the turn is refused before launch.
Approved scope-widen files from the `stage` block's `approved_scope`
(FAE-WF-078) are added to either source; they follow the same declared-path
rule, so an unsafe one refuses the turn (the engine drops one a job stored
before that rule from its approved scope before the dispatch, so the daemon
never sends one). The Prophet (`fae-prophet`) has no scope at all.

Each file is read with a 1 MiB bound and the files of one turn with a 4 MiB
total bound; over it the turn is refused with the total named, never truncated.
An approved scope-widen file is the exception, the turn's RED test file
included: nothing removes approved scope, so refusing it would refuse every
later turn of the task. When one reaches a symlink, is not a regular file, is
over 1 MiB, or would take the turn over 4 MiB (declared files and the RED test
file are read first), the turn runs without it: an `unavailable_scope` block
lists its path and the reason, the file is never read or sent, and an edit to
it is refused output. For the RED test file that leaves the RED identity as it
was; the engine runs the RED test from the worktree, and a test-first turn can
only report the RED as it stands.

## Prompt

The prompt is the brief's ordered blocks (FAE-WF-043) after a task header, as
quoted JSON data. Just before the `files` block (the scope's current contents;
a missing file is empty), every turn except the Prophet's gets a
`{"kind": "repository", "files": [...], "truncated": bool}` block: the
worktree's tracked paths from `git ls-files -z`, sorted and capped at 5,000
paths and 256 KiB of path text. A provider loop gets it only when it has files.
The bridge runs that `git` without system or global configuration, inherited
`GIT_*` variables, pager or prompts, does not search above the worktree, and
skips the refusing `git` wrapper the runtime puts first on its `PATH` (the
session's `command-shims`). If git fails the turn is refused: a partial listing
must never look complete. The kernel contract comes last, with the absolute
handoff path and the bridge's output protocol: the `task_scope`, `repository`
and `engine_feedback` blocks, both edit forms, and the test-first rules (one
compiling `_test.go` RED that fails at run time with the declared
`expected_failure`, under a test name unique across the repository, and the
daemon's rule for a derived `expected_failure`: what it is matched against,
that it must say why the test fails, and which fragments it refuses; see
"Task scope" in [LIVE.md](../LIVE.md)).

## Claude Code

The Claude bridge runs headless `claude -p` with `--output-format=stream-json`
and the role output/edit schema as `--json-schema`. It passes `--tools ""`
(no built-in tools), `--restricted`, `--safe-mode` (no CLAUDE.md, skills,
plugins, hooks or custom agents), `--strict-mcp-config` without servers,
`--disable-slash-commands`, `--permission-mode=dontAsk` and
`--permission-prompts=none`. Binding values use `--flag=value` so they cannot be
parsed as options. Effort must be one of `low`, `medium`, `high`, `xhigh` or
`max`; the CLI would otherwise silently substitute its default. Fresh turns use
`--no-session-persistence`; persistent contexts start with a bridge-generated
`--session-id` and continue with `--resume`, and the announced session must
match.

The bridge reads the complete event stream, independently of the bounded
`events.jsonl` prefix. It requires exactly one `system/init` record whose tool
list contains only `StructuredOutput` and whose MCP server list is empty, and
exactly one successful, non-error `result` for that session with
`structured_output`. Anything else refuses the turn before any edit: a wrong
session or tool surface without a class, a missing, repeated, failed or
unstructured result as refused output (see Refusals). Only the structured
output is written to `answer.json`. `providerhealth` classifies
Claude's API-error records, failed results and rejected rate-limit events.

Credentials must be explicit in the pinned target `environment`, because each
session has a fresh HOME: `CLAUDE_CONFIG_DIR` (an absolute, logged-in Claude
Code configuration directory), `CLAUDE_CODE_OAUTH_TOKEN` (from
`claude setup-token`), `ANTHROPIC_API_KEY`/`ANTHROPIC_AUTH_TOKEN`, or a
`CLAUDE_CODE_USE_BEDROCK|VERTEX|FOUNDRY` selection with its own credentials. The
bridge refuses to launch without one. Prefer a directory path or a dedicated
token over embedding an API key: the target configuration is pinned in the
instance database. The bridge sets `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`.
Claude Code writes `.claude.json` and session transcripts under the
configuration directory; persistent contexts depend on those transcripts.

## Codex

`provider.Run` implements the `fae-agent-v1` protocol for a real Codex CLI. It
passes the prompt above on standard input and the output JSON Schema to
`codex exec`. The configured provider/model binding supplies the model and
optional reasoning effort. Authentication comes from the explicitly configured
runtime environment; credentials are not placed in the prompt or handoff.

The bridge starts Codex in an empty private working directory with
`--ignore-user-config`, `--ephemeral`, `--sandbox read-only`, and approval policy
`never`. Shell tools, apps, browser use, and web search are disabled. The model
returns a structured role output and proposed edits. It does not run
the workflow's compilation, tests, formatter, or Git operations. See the upstream
[non-interactive execution documentation](https://learn.chatgpt.com/docs/non-interactive-mode)
and [configuration reference](https://learn.chatgpt.com/docs/config-file/config-reference)
for the CLI interface; installed CLI support for these controls is required.

## Edits

The trusted Go bridge validates the output schema and applies proposed edits
only when the role grants editing and the stage permits it:

- **test-first:** with the task's test file set, only that file; without one,
  at most one file, in scope and named like a Go test (`*_test.go`).
- **implement:** any file in scope except the frozen test file.
- **Other stages:** no edits.

Every edited path must be in scope and must not traverse symlinks or escape the
worktree. Each edit has one strict shape, `{"path", "content", "replacements"}`
with all three required (Codex `--output-schema` and Claude `--json-schema`
take strict schemas), and uses exactly one form:

- **Whole file:** `content` is the new content and `replacements` is `[]`.
  Only this form creates a file (and any missing parent directories).
- **Replacements:** `content` is `null` and `replacements` lists
  `{"old", "new"}` pairs, applied in order. Each `old` must occur exactly once
  in the file's current content after the earlier replacements of the same
  edit; otherwise the whole response is refused with the path and replacement
  index, and nothing is applied.

Each edit's result is bounded to 1 MiB. Every declared input is checked again
before writes to detect concurrent changes.

A turn's edits are written all or nothing (FAE-ROB-012, FAE-ROB-016,
FAE-VCS-003). The bridge first checks every target (a regular file the bridge
may write, or a new path whose parents are directories, and no path needed as
both a file and a directory), then writes and syncs each new content to a
hidden `.fae-edit-*` file beside its target, and only then renames each one
into place, keeping an existing file's mode (a new file is private), and
writes the handoff last. If any step fails, the handoff write included, it
puts back the content and mode every renamed file had before the turn, removes
the staged files, any new files and the directories it created, and refuses
the turn as `edits-not-applied` (below). The rename is atomic per file, not
across files: a bridge killed between two renames leaves the edits renamed so
far, the rest still staged as `.fae-edit-*` files, and no handoff, which the
daemon sees as an uncertain turn with a changed tree.

## Refusals

Every check of the answer runs before the first write, so a refused turn
leaves the worktree as it was. The bridge then tells the runtime why, so the
engine retries the stage instead of parking the task (FAE-WF-034, FAE-RT-004,
FAE-RT-009). It writes `bridge-refusal.json` in the session's log directory
(`{"protocol", "dispatch_id", "session_id", "task_id", "class", "detail"}`,
the detail cut to 4 KiB) and exits with status 65. There are five classes:

- **`output-refused`:** the provider's turn completed and its answer is
  refused. That covers no answer, an answer that isn't JSON or has an unknown
  field, a schema failure, an edit in both or neither form, a replacement whose
  `old` text is missing or not unique, and an edit outside the scope or the
  stage's rules (more than one RED file, a RED edit to a non-test file, an edit
  to the frozen test). For Claude Code it also covers a turn with the requested
  session and tool surface but no single successful structured result. The
  executor records malformed output against the binding and retries the stage
  within its bound.
- **`binding-refused`:** before the provider is launched, the bridge finds it
  cannot run the turn's binding as configured: a binding of a provider this
  bridge doesn't serve (a Codex binding on `fae-claude-bridge`, or the
  reverse), an empty model, an empty or relative provider executable path, a
  timeout that isn't positive, a negative start bound, and the Claude
  preflight (an effort Claude Code lacks, such as `minimal`, an option-shaped
  model, a relative `CLAUDE_CONFIG_DIR`, no credentials in the target
  environment). No retry of
  the same binding can succeed until that configuration changes. The executor
  charges no provider, and the engine moves the turn to the role's next route
  and skips the binding for the task (FAE-RT-010, FAE-WF-082).
- **`refused-before-provider`:** the turn is refused before the provider is
  launched for any other reason. That covers the task scope (no
  `--task-scopes` entry for the task, a `task_scope` block with an unsafe path
  or a test file outside its files, a pin that disagrees with the block, an
  unsafe `approved_scope` path), a declared file over 1 MiB or declared files
  over 4 MiB in total, a failed repository listing,
  the role's capabilities, the Prophet and provider-loop policies, and local
  steps such as creating the provider directory. The executor charges no
  provider and no attempt: the turn is refused before launch, and the fourth
  such refusal in a row at a stage escalates SameGate.
- **`provider-not-started`:** the provider never acknowledged the prompt
  (FAE-RT-004). Claude Code's stream-json and Codex's `--json` output start
  with the session the CLI opens (`system/init`, `thread.started`), before the
  model is asked anything, so a CLI that wrote nothing to its event stream
  (stdout) never took the turn; it has no tools here, and its edits could
  reach the task only through a validated answer. The class covers:
  - the absolute provider executable did not start (it doesn't exist or
    can't be run);
  - the provider wrote nothing to its event stream and left no answer file,
    however it ended: it exited on its own, with a failure or with status 0;
    the start bound stopped it; or this bridge's own `--timeout` stopped it.

  The start bound is Appendix A's agent start, 180 s per attempt
  (`--start-timeout`, `Config.StartTimeout`; zero uses 180 s, a negative
  value is a binding refusal). A provider that hasn't written anything when
  it runs out is stopped: the bridge kills it and every descendant a process
  census taken just before finds under it (a descendant is signalled only
  while its start identity still matches the census), and the runtime
  retires the rest of the bridge's process group when the bridge exits. The
  decision is taken once, when the bound runs out, so output the provider
  writes while it is being stopped doesn't change it. A provider that wrote
  anything in time is never stopped by the bound, however long it then runs.
  Any output at all, even text that isn't JSON, makes a later failure
  `provider-failed` instead. A descendant that left the provider's tree
  before the census (a setsid) and holds the event or error stream open
  cannot keep the bridge waiting once the provider exited or was stopped:
  the wait is bounded (`ProviderWaitDelay`, 10 s), after which the streams
  are closed and the provider's own exit decides the turn. A cancelled bridge stays untyped. The executor
  charges no provider and treats it as a start failure: the engine relaunches
  the turn within the agent-start bound, then parks ProviderUnavailable
  against the binding and moves to the next route. A later review session's
  start failure is relaunched the same way, as every reviewer of the
  dispatch, again charging no attempt (execution/README.md). Prophet follows the same rule (daemon/PROPHET.md).
- **`provider-failed`:** the provider acknowledged the prompt and then ended
  without an answer (FAE-WF-034): it wrote anything to its event stream and
  then exited with a failure on its own, or this bridge's own `--timeout`
  stopped it after it had written something. The bridge wrote nothing to the
  task, since edits are written only from a completed and validated answer,
  and the provider had no tool that writes. For Claude Code
  the class also needs the announced surface to be the allowed one: at most
  one `system/init` record, with only `StructuredOutput` and no MCP servers,
  and no record skipped as oversized. The detail names the exit status or the
  timeout and no per-turn path (`provider exited with a failure after
  acknowledging the prompt: exit status 3`), so the same failure repeated
  counts toward SameGate. The executor records the provider's health from its
  log (malformed output when the log has no structured error) and, with
  retired processes and an unchanged tree, fails the turn as malformed
  output, a bounded stage retry. A changed tree keeps it uncertain.
- **`edits-not-applied`:** the provider's answer passed every check, but the
  bridge could not write its edits or the handoff, and restored the worktree
  (see Edits). Nothing of the answer reached the task and the failure is the
  bridge's, so the executor records the provider as having answered (no
  failure charged to it) and fails the turn as ValidationFailed, a bounded
  stage retry.

Other failures exit 1 with no record: a provider stopped because the bridge
itself was cancelled (a signal), a Claude Code session or tool-surface refusal
(of a finished turn, or of a failed one that announced a tool or MCP server
beyond the allowed surface or two sessions), another writer's handoff, a file
that changed during the turn, a refusal whose record can't be written, and a
failed edit write the bridge could not undo. The runtime reports them as
`process-failed`, and the executor keeps them uncertain.

Where the bridge stopped decides the class, never the provider's answer. Only a
step before the provider's launch can refuse locally, only a completed provider
turn's answer can be refused as output, only a provider that never started, or
wrote nothing and left no answer, is `provider-not-started`, and only a provider that ended without
an answer before the bridge wrote anything is `provider-failed`. The model can't choose a class
either: it has no tools (Claude Code runs with `--tools ""`, Codex with a
read-only sandbox and no shell), and the record and the exit status come from
the bridge process. The runtime accepts a record only with exit status 65, no
handoff, and a fresh regular file (not a symlink) that names this turn and a
known class.

Host-command capabilities are refused by this bridge: the production effect
broker executes the real RED, successful tests, formatting checks, commits and
integration under its resource leases and evidence rules.

An existing handoff, including a dangling symlink, is refused before provider
execution. After inference, the handoff is exclusively reserved before any task
edits, so a concurrent handoff also prevents edits, and written after them.
Failed publication removes the bridge's incomplete handoff and puts the edits
back. The runtime independently validates the final
handoff's dispatch/session/task identity, freshness, type and size.

Provider event and stderr files each retain at most the first 1 MiB while excess
bytes are drained and discarded. They are private files in the session's provider
log directory. Final answers and each file input are also limited to 1 MiB, and
a turn's file inputs to 4 MiB in total. These bounds apply per turn; aggregate
log retention is a separate instance policy.

Tests cover edit/profile restrictions, the test-first and implement edit rules,
the class, record and exit status of refusals (the answer refusals, Claude
Code's unusable results, and the scope, file-bound and repository-listing
refusals before launch) and that untyped failures keep exit 1 with no record,
task scope from a pin or the brief (precedence, test-file mismatch, unsafe
paths, the total bound), the repository listing (contents, caps, git failure,
the role's git wrapper), replacement edits (applied, ambiguous, missing, mixed
with whole-file edits), all-or-nothing edit writing (a failed third write, a
failed rename or handoff write after earlier renames, and targets refused
before any write) and the strict schema shape for every shipped artifact,
symlink/traversal refusal, output bounds, a subprocess protocol round trip for
both bridges with the scope from the brief, both bridge binaries' refusal exit
status and record, stale and concurrent handoff refusal
before edits, and large real subprocess stdout/stderr streams without deadlock.
These unit tests use an explicit local provider fixture. `scripts/dogfood.py` exercises
real Codex inference through Herdr (`herdr-core`) and the real daemon/effect broker, with two
independent task scopes and a planned drained restart. Results and complete chains
are recorded separately from protocol-fixture tests.
