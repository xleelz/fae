# Linear through Symphony and the FAE engine

FAE can use a Symphony `WORKFLOW.md` as a Linear intake source. The embedded
`fae serve --workflow-md /absolute/path/WORKFLOW.md` path admits selected issues to
an already installed FAE workflow and target. FAE's existing engine owns task
execution, worktrees, budgets, artifacts and recovery; this path does not start a
second Symphony coding-agent loop for each issue.

The standalone `fae symphony --workflow-md /absolute/path/WORKFLOW.md` command
continues to use Symphony's own Codex app-server runner and its workspace/hooks
settings. Its default remains `codex app-server`. Adding `fae` selectors does not
silently turn that standalone command into the embedded FAE executor. Use the
embedded serve entry point when the FAE workflow should own execution.

## Configure a scope before activation

[The example workflow](../examples/LINEAR_WORKFLOW.md) is a template, not an active
integration. Its project/team and FAE workflow/target/authority references are
unset placeholders. No project was selected, no integration was activated and no
Linear writes were performed while adding this configuration.

The `tracker.provider` map supports `team_id`, `team_key`, `project_id` and
`project_slug`. Configure at least one actual scope; use both team and project
when intake should be restricted to that intersection. These are tracker scope
selectors, not FAE workflow identifiers. Keep `required_labels: [Fae]` to select
Fae-labelled issues; all configured labels must match, case-insensitively. Match
`active_states` and `terminal_states` to the team's real state names. A label or a
parent relationship does not override terminal-state filtering.

The example polls every 30 seconds, limits Symphony intake to two concurrent
agents/tasks and supplies per-state concurrency limits. FAE's own workflow,
provider and host admission limits continue to apply. Adjust these values to the
chosen team/project and host rather than assuming the example's state names are
universal.

## Secret references

The example uses `tracker.provider.api_key: $LINEAR_API_KEY`. Set the credential in
the service host's environment through the normal secret mechanism; never replace
that reference with a real token in a committed workflow. The workflow file keeps
the reference while the runtime resolves its value in memory. An explicitly named
custom variable, such as `$MY_LINEAR_TOKEN`, is also supported. An unset credential
fails preflight instead of allowing a tracker operation with an empty key.

Both `fae serve` and `fae symphony` automatically load
`$XDG_CONFIG_HOME/fae/linear.env`, or `~/.config/fae/linear.env` when
`XDG_CONFIG_HOME` is unset, before resolving the workflow. A missing file is fine.
Existing environment variables take precedence, including explicitly empty ones.
The file must be a regular file with no group/other permissions (`chmod 600`).
It accepts `NAME=value`, optional `export ` prefixes, blank lines, comments on
their own lines, and matching single/double quotes around values. Values are
literal: shell commands, variable expansion, escape sequences and inline comments
are not interpreted. Invalid assignments stop startup without printing values.
Changes take effect on the next service start; this file is not hot-reloaded.
Keep the workflow's `$LINEAR_API_KEY` reference unchanged.

Do not copy resolved configuration containing tracker credentials into a log or a
support message. The workflow's `fae.authorizing_message` is a reference to an
existing FAE operator-authorizing message, not a Linear token.

## Embedded FAE selectors

The `fae` section is typed:

| Field | Meaning |
| --- | --- |
| `workflow_id` | Existing installed FAE workflow identifier. |
| `workflow_revision` | Nonnegative revision; zero selects the current revision, which admission pins for the task. |
| `target_config` | Existing registered FAE target identifier. |
| `target_revision` | Nonnegative target revision; zero resolves and pins the current revision. |
| `authorizing_message` | Existing FAE authority reference used for admission. |
| `mode` | `Local`, the default and supported bridge mode; this does not authorize deployment. |
| `sync_state` | Explicit opt-in to tracker state synchronization; defaults to `false`. |
| `post_comments` | Explicit opt-in to tracker comments; defaults to `false`. |

Workflow, target and authorizing-message selectors are required for embedded FAE
execution. They remain optional when resolving a standalone Symphony configuration,
so existing standalone workflows keep their behavior. Revision fields must be
integers. Write opt-ins must be real YAML booleans: the string `"false"` is rejected
rather than interpreted as truthy. Unknown `fae` keys fail configuration resolution.

`Local` mode controls FAE's deployment/external-effect policy. It does not itself
grant tracker mutations. Leave both write opt-ins false for reader/intake behavior;
turning either on is a separate explicit choice. The example contains no deployment
or shell hooks, and creating the file does not make either CLI entry point run.

## Dependencies and hierarchy

Scheduling blockers come from explicit Linear blocking relationships. An issue
that is explicitly blocked by unfinished work is held according to tracker/runner
policy. A parent issue is not automatically blocked by every child, and children
are not automatically dependent on their parent. Parent/child metadata may be
shown in context, but hierarchy alone must not manufacture dependency edges. Add
an explicit `blocks` relationship when execution order is intended.

The selected FAE workflow remains responsible for its own gates, child tasks and
completion requirements. Linear issue hierarchy is not an implicit replacement
for those engine rules.

## Example preparation checklist

Before an operator starts the embedded command, choose the intended team/project,
confirm its state names and Fae label, make the credential available to the service,
and set the example's `FAE_LINEAR_WORKFLOW_ID`, `FAE_LINEAR_TARGET_CONFIG` and
`FAE_LINEAR_AUTHORIZING_MESSAGE` references to installed/authorized objects. Keep
revisions explicit if new intake must remain on a particular version. Those values
are deployment-specific and are intentionally not filled with guesses here.

## Durable execution and tracker updates

The integration follows the [Symphony specification](https://github.com/openai/symphony/blob/main/SPEC.md)
for polling, scope, issue context and dependency-aware dispatch. Embedded execution
uses FAE's task engine. Issue UUIDs, rather than changeable display identifiers,
are bound durably to tasks. Repeated polls, issue renames and daemon restarts reuse
that binding. Reopening a completed Linear issue does not implicitly create a new
FAE attempt; use FAE's explicit task controls or create a new issue for new work.
Edits to an admitted ticket do not silently replace its accepted content or pinned
workflow/target revisions.

Incoming blocking relationships, labels and children are paginated. A prerequisite
is cleared by a terminal Linear state, including its completed/canceled state type
when another team uses different names. Unknown or incomplete blocker metadata
holds dispatch. Parent/child hierarchy alone never clears or creates blockers.

With `sync_state: true`, running tasks and tasks awaiting a FAE operator decision
request `In Progress`; successful terminal tasks request `Done`; other terminal
tasks request `Canceled`. Use `tracker.provider.state_map` to map these semantic names to the
team's actual states. FAE's internal review stages do not automatically mean a
Linear review handoff. Each state ID is resolved against the issue's own team.

For a fully automatic dependency chain, enable state synchronization or arrange
for another actor to mark completed prerequisites terminal in Linear. With writes
disabled, completing a FAE task leaves its Linear state unchanged, so dependent
issues continue waiting. Mapping `Done` to a nonterminal review state likewise
keeps dependents waiting until that review finishes.

`post_comments: true` posts task-state/version summaries. Local receipts suppress
repeated comments, but a crash after Linear accepts a comment and before FAE saves
the receipt can produce a duplicate. State synchronization is idempotent.

## Start the embedded service

First use the normal FAE setup to install the workflow, register the target and
record an operator message explicitly authorizing recurring Linear intake for the
chosen scope. This message is a durable recurrence citation, checked for integrity
at startup and admission; it is not subject to the freshness timeout for a new
interactive action. Configure a dedicated citation for this integration.

Copy the example outside the tracked source if it contains host-specific settings,
set its referenced environment variables in the service environment, then start
FAE with the usual runtime options plus:

```sh
/path/to/fae serve --workspace /absolute/path/to/workspace \
  --workflow-md /absolute/path/to/LINEAR_WORKFLOW.md --foreground
```

An initial daemon without `--workflow-md` can be used to prepare the installed
objects and citation before starting the integration. Preserve the deployment's
normal adapter/runtime flags; the workflow file does not select a live provider.
The embedded path uses FAE's worktrees and runtime configuration. Symphony shell
hooks and SSH worker pools are rejected here; use standalone Symphony if those
features are required.

Valid workflow-file edits reload the tracker scope and routing configuration.
Invalid edits retain the last valid configuration. Keep the API key out of the
file, command arguments and logs. Activation still requires an actual chosen
Linear scope and a credential; the repository example alone performs no intake.

## OpenRelay testing configuration

[`LINEAR_WORKFLOW.md`](../LINEAR_WORKFLOW.md) scopes intake to OpenRelay Engineering
(`OR`) and Fae Testing (`P-OR-21`), using the team/project UUIDs verified through
Linear. State synchronization is enabled; comments remain disabled. It requires
Todo/In Progress issues carrying the `Fae` label. Create that label if absent.

Before startup, set `FAE_LINEAR_WORKFLOW_ID`, `FAE_LINEAR_TARGET_CONFIG` and
`FAE_LINEAR_AUTHORIZING_MESSAGE` in the private environment file to the installed
FAE workflow, registered target and recorded recurring-intake authority. Start
with `--workflow-md /absolute/path/to/fae/LINEAR_WORKFLOW.md` along with the normal
runtime options. This configuration file does not itself activate the service.
