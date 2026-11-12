# Choosing a HostCommand adapter

`command_adapter` is a stage configuration, independent of whether a command
produces test, check or formal evidence:

- Omitted or `builtin`: preserve the existing scoped language adapter and its
  command grammar, worker clamps and evidence rules.
- `configured`: execute the exact operator-allowlisted literal vector without
  guessing a new tool's grammar. Existing host admission, process accounting,
  timeout, write scopes and collection still apply.

Only HostCommands accept this field. A configured command does not acquire
permission merely by naming the adapter: its complete original vector must match
the pinned target allowlist. It runs in the physical task worktree under the
existing heavy-operation lease. Configure a new tool's worker/memory bounds in its
literal arguments or explicit stage environment; no language-specific flags are
silently injected. A relative wrapper must resolve to an executable in that
worktree. No arguments are joined into an implicit shell command.

## A new tool with a structured check

For example, an installed or project-owned tool can emit
`{"checked":2,"ok":true}` on stdout. A stage fragment is:

```yaml
primitive: HostCommand
command_adapter: configured
command: ["./tools/domain-check", "--report=json"]
weight: 1
timeout_seconds: 60
output: DomainCheck
check:
  format: json
  count_field: checked
```

The target must allow that exact vector. The usual structured check parser also
records actual `exit_code`, `timed_out`, `truncated` and output identity. A
following gate should bind this stage's artifact and require the real exit code
to be zero, no timeout/truncation, and the intended positive check count or verdict.
The wrapper's language need not have a FAE-specific compiler grammar. Its outputs
must still stay within declared write/scratch paths.

## A command with no result parser

A configured command may omit `check`, `formal`, `test_run` and `generate`. Its
artifact is an execution receipt with `executed`, `exit_code`, `timed_out`,
`truncated`, `tree` and `output_digest`. A gate can require `executed == true` and
`exit_code == 0`. This is command execution evidence, not a test count, successful
proof, or replacement for a workflow's existing review/test obligations.

Configured test commands still need an existing supported `test_shape` matching
the command. Selecting this adapter cannot invent a new shape or turn arbitrary
stdout into executed-test evidence. Known native language test shapes retain their builtin preparation, environment
and nested-command policy even when `configured` is selected. Configured
`script`/`runner` shapes retain their explicit generic command policy and still
require valid TAP/test evidence.

## Formal verification

A `formal` declaration selects the configured adapter implicitly;
`command_adapter: builtin` cannot be combined with it. Formal output goes through
the proof parser, not the ordinary execution/check interpretation. The tool label
and exit code alone never mean `passed: true`. See
[configured commands and formal evidence](configured-commands.md) for source
binding, actual obligation counts, observed versions and native Gobra acceptance.
