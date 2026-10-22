# Restricted Codex bridge

`provider.Run` implements the `fae-agent-v1` protocol for a real Codex CLI. It
passes the ordered role brief, declared file contents, and an output JSON Schema
to `codex exec`. The configured provider/model binding supplies the model and
optional reasoning effort. Multi-task runs pass `--task-scopes` as a JSON literal in
the pinned bridge command; the bridge selects files/test file by the exact
BridgeConfig task ID and refuses missing or malformed task entries. Authentication comes from the explicitly configured
runtime environment; credentials are not placed in the prompt or handoff.

The bridge starts Codex in an empty private working directory with
`--ignore-user-config`, `--ephemeral`, `--sandbox read-only`, and approval policy
`never`. Shell tools, apps, browser use, and web search are disabled. The model
returns a structured role output and proposed whole-file edits. It does not run
the workflow's compilation, tests, formatter, or Git operations. See the upstream
[non-interactive execution documentation](https://learn.chatgpt.com/docs/non-interactive-mode)
and [configuration reference](https://learn.chatgpt.com/docs/config-file/config-reference)
for the CLI interface; installed CLI support for these controls is required.

The trusted Go bridge validates the output schema and applies proposed edits
only when the role grants editing and the stage permits it. Test-first turns can
edit only the pinned test file; implementation turns cannot edit that frozen
file. Other stages cannot edit files. Every edited path must belong to the
configured allowlist and must not traverse symlinks or escape the worktree.
Every declared input is checked again before writes to detect concurrent changes.
Host-command capabilities are refused by this bridge: the production effect
broker executes the real RED, successful tests, formatting checks, commits and
integration under its resource leases and evidence rules.

An existing handoff, including a dangling symlink, is refused before provider
execution. After inference, the handoff is exclusively reserved before any task
edits, so a concurrent handoff also prevents edits. Failed publication removes
the bridge's incomplete handoff. The runtime independently validates the final
handoff's dispatch/session/task identity, freshness, type and size.

Provider event and stderr files each retain at most the first 1 MiB while excess
bytes are drained and discarded. They are private files in the session's provider
log directory. Final answers and file inputs are also limited to 1 MiB. These
bounds apply per turn; aggregate log retention is a separate instance policy.

Tests cover edit/profile restrictions, symlink/traversal refusal, output bounds,
a subprocess protocol round trip, stale and concurrent handoff refusal before
edits, and large real subprocess stdout/stderr streams without deadlock. These
unit tests use an explicit local provider fixture. `scripts/dogfood.py` exercises
real Codex inference through Herdr and the real daemon/effect broker, with two
independent task scopes and a planned drained restart. Results and complete chains
are recorded separately from protocol-fixture tests.
