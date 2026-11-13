# Fae

A local Go implementation of [REQUIREMENTS.md](REQUIREMENTS.md), with a runnable
Dolt-backed daemon, durable scripted workflows, and opt-in direct-process or Herdr live execution. This is still a
partial implementation, not a production-ready implementation of all 284
requirements. A trusted agent bridge can execute real turns; brokered host commands and
reviewed Git effects support Local execution and explicitly authorized Normal effects through configurable adapters. Scripted evidence describes rehearsals, not actual
code changes, test executions, commits, or integrations.

Scoped Go, Rust, Python, and TypeScript build/test adapters are available. See
[language support](docs/language-support.md) for command configuration, real-tool
coverage, and the supported limits of each ecosystem.

[Configured command adapters](docs/command-adapters.md) support additional tools
without a language adapter. [Formal stages](docs/formal-verification.md) add
source-bound proof receipts, native verifier profiles, configurable JSON report
mappings, and explicit proof gates.

[Linear intake](docs/linear-integration.md) polls selected issues, respects explicit
blocking dependencies, and admits durable tasks to the Fae engine through a
Symphony-compatible workflow file. Tracker writes require explicit opt-in.

## Requirements

Core (enough for the scripted example in [Getting Started](#getting-started)):

- **Go 1.27.2.**
- **Git 2.45 or newer.** Changed-base reconciliation passes trees to
  `git merge-tree`; Ubuntu 24.04's stock Git 2.43 refuses them.
- **Dolt**, found on `PATH` or passed with `serve --dolt`. The daemon initializes
  and supervises its own private Dolt database; clients never connect to an
  existing database directly.

Additionally, for live runs:

- `lsof`;
- ripgrep, needed by `scripts/dogfood.py`;
- Herdr (`herdr-core`) for the multiplexer adapter;
- a logged-in Claude Code (the default) or Codex CLI.

Instance home rules:

- The instance home must be on a filesystem with hard links or a no-replace
  rename (Linux `renameat2`, macOS `renamex_np`); owned-process records are
  placed that way, and a launch fails if neither works
  ([processowner](processowner/README.md)).
- The home must be outside any repository, and its physical path must contain no
  symlinks. Instance directories and credentials are owner-only.

Only trusted single-operator workspaces are supported. No hostile filesystem or
escaped-process sandbox is claimed.

## Installation

Build from the repository root:

```sh
mkdir -p .cache/go-build bin
export GOCACHE="$PWD/.cache/go-build"
go build -o bin/fae ./cmd/fae

# Optional: the terminal view of a running daemon
go build -o bin/fae-tui ./cmd/fae-tui
```

Then check your setup and see what the CLI offers:

```sh
./bin/fae doctor   # offline check of prerequisites, instance layout, sandbox and filesystems
./bin/fae help     # lists every verb; `fae COMMAND --help` prints a verb's flags
```

Help, `version`, the offline `workflow` forms, `template workflow-validate`,
`gates self-test --offline` and `doctor` need no home (doctor reports a missing
one as a warning).

## Getting Started

This example task needs no live provider: it uses the scripted adapter, which
rehearses the shipped default workflow with fixture outputs and does not change
any code. It needs only Go, Git and Dolt.

**1. Start the daemon** (terminal 1, from the canonical workspace root). Only
`serve` starts a daemon; `--foreground` keeps it attached to the terminal.
Supply `--dolt` if Dolt is not on `PATH`.

```sh
./bin/fae serve --workspace "$PWD" --adapter scripted --foreground
```

**2. Authorize the work** (terminal 2; use the absolute path to `bin/fae` when
outside this workspace). Every mutating verb needs `--authorizing-message`;
without it the CLI refuses with exit 1. This message grants the `task` and
`runtime` classes, so it can authorize both the run and the later shutdown.

```sh
./bin/fae message --id demo-authorization --text '{"text":"Run local scripted work and shut down this instance.","authorize":{"classes":["task","runtime"]}}' 
```

**3. Submit the task.** `run` admits task `demo` on the default workflow,
authorized by the message above.

```sh
./bin/fae run --id demo --prompt 'Exercise the scripted default workflow' --authorizing-message demo-authorization
```

**4. Watch it progress.** `status` reports the daemon and its tasks; `history`
shows the task's checkpoints; `board` and `history --summary` give paged
overviews; `events --follow` streams ordered events from the start (stop it with
Ctrl-C).

```sh
./bin/fae status --json
./bin/fae history --task demo --json
./bin/fae board --limit 100 --json
./bin/fae history --task demo --summary --limit 100 --json
./bin/fae events --after 0 --follow
```

**5. Shut down.** Graceful stop lets in-flight commit/integrate steps finish
within the drain bound.

```sh
./bin/fae shutdown --authorizing-message demo-authorization
```

Next steps: to run Fae on a clone of itself with real Claude turns, see
[docs/SELF_HOSTING.md](docs/SELF_HOSTING.md). For live adapter configuration,
Normal-mode adapters and the Claude Code and Codex bridges, see [LIVE.md](LIVE.md).

## Features

- **Durable singleton daemon** backed by a private Dolt control plane: dispatch
  intent is checkpointed before execution, completed stages are not replayed,
  graceful stop drains within a 120 s bound, and restart recovers from durable
  state (see [Architecture and durability](#architecture-and-durability)).
- **Shipped default workflow**: intake, spec/review, plan, RED author/run/gate/freeze,
  implementation/compare, reviews, Oracle routing, QA, reconcile, protected commit,
  integration and Done, plus a Normal-only code-review sub-workflow.
- **Execution adapters**: `scripted` (rehearsals), `direct` (alias `process`) and
  `multiplexer` (Herdr `herdr-core`, private owned servers and gated panes).
- **Agent bridges**: Claude Code is the default provider
  ([provider/README.md](provider/README.md)); the Codex bridge remains
  ([LIVE.md](LIVE.md)).
- **Local and Normal modes**: `serve --default-mode local|normal`; Normal needs a
  live adapter and explicitly authorized effects ([target policies](daemon/TARGETS.md)).
- **Language adapters** for Go, Rust, Python and TypeScript
  ([language support](docs/language-support.md)).
- **Configured command adapters** for other tools
  ([command adapters](docs/command-adapters.md)).
- **Formal stages** with source-bound proof receipts and proof gates
  ([formal verification](docs/formal-verification.md)).
- **Linear intake**, with tracker writes opt-in only
  ([Linear integration](docs/linear-integration.md)).
- **Workflow primitives**: HumanDecision, FanOut/FanIn (branches run
  sequentially) and SubWorkflow, with replayable gate records.
- **Operator verbs, alerts and metrics**: messages and inbox, task
  create/update/delete/show, decide, quarantine, reclaim, `fae alerts`,
  `fae metrics` and `fae host-metrics` ([read contracts](daemon/READS.md),
  [live task verbs](daemon/TASKS.md), [task controls](daemon/TASK_CONTROLS.md)).
- **Host admission** using PSI pressure and an I/O gate
  ([hostguard/README.md](hostguard/README.md)).
- **UI child** (`serve --ui`, read-only unless `--ui-allow-writes`;
  [ui/README.md](ui/README.md)) and the **`fae-tui`** terminal view
  ([tui/README.md](tui/README.md)).
- **Backup and storage**: verified plane backups, reviewed retention, remote
  replication, opt-in retention, creation and replication schedules, unpinning
  and quotas ([backups](daemon/BACKUPS.md)); **census**
  ([census](daemon/CENSUS.md)); and **cleanup** of retired and Done tasks and
  scratch instances ([cleanup](daemon/CLEANUP.md), [scratch](daemon/SCRATCH.md)).

## Roadmap

### Done

Each item is described in its increment section under [Reference](#reference).

- Runtime recovery: audited dynamic pane limits, idle-session shedding and
  bounded safe stall retry.
- Persistent provider context, dynamic heavy-op/host-pressure/task limits and
  bounded startup recovery.
- Operator controls and reads: delegated principals, message-class grants,
  durable task controls and bounded reads.
- Prophet sessions, backlog dependencies and safe post-commit restart.
- Task retirement, Local target policies and Prophet routes.
- Normal targets (schemas v2/v3), census and retired cleanup.
- Scratch cleanup and dynamic adapter/workspace provisioning.
- Adapter retirement and configurable fleet provisioning.
- Backup lifecycle: reviewed retention and remote replication.
- Opt-in retention schedules and redundant scratch-archive cleanup.
- Opt-in backup creation and replication schedules.
- Replication unpinning and backup quotas.
- Claude provider (default), host admission and the acceptance ledger.
- Wave 2: records, primitives, Normal content, UI and recovery.

### Open

These are limits the repository states today:

- Complete process inventory and lease reconciliation, including complete
  cross-user host process inventory.
- Interactive brokered agent tools.
- Unsupported conflict classes (they refuse).
- Remaining operator/controller and Normal-mode phases.
- Explicit targets supporting Local mode only, as stated by the
  retirement/target-policies increment; target schemas v2/v3 later added Normal
  bindings.
- FanOut/FanIn branches currently run sequentially.
- Containment without retained task evidence cannot yet be recovered from
  integration commit subjects alone, as stated by the
  retirement/target-policies increment.
- The ledger maps only 101 of 284 requirement IDs to at least one check (Wave 2).
- Eight newer pure guards are not yet in `verify.sh` (Wave 2).

## Troubleshooting

Start with `fae doctor` (offline prerequisite, layout, sandbox and filesystem
check) and `fae COMMAND --help`; each command's `--help --json` lists its exit
codes.

- **What do the exit codes mean?** 0 is success; 1, 2 and 3 are below. The
  full table is the exit-code paragraph under [Run locally](#run-locally).
- **A command exits 3.** The daemon is unreachable. Only `serve` starts a
  daemon; `run --start` re-runs the recorded `serve` invocation, and
  `maintenance start` and `roll` relaunch it. Other commands never start one.
- **A command exits 1.** Refusal or validation failure; the reason goes to
  standard error. A common cause is a mutating verb without
  `--authorizing-message`.
- **A command exits 2.** Usage error, for example a value that does not parse
  (`--inline-limit x`).
- **`gate run` exits 75 or 76.** 75: its admission wait timed out; 76: admission
  was refused. The hidden `heavy-op run` broker verb exits 75 on a lease or host
  refusal, 77 when a role shim policy refuses and 78 when Local mode refuses a
  git push or remote change; see the exit-code paragraph under
  [Run locally](#run-locally) in the Reference.
- **`git merge-tree` fails during reconciliation.** Git is older than 2.45
  (Ubuntu 24.04's stock 2.43 refuses trees); install Git 2.45 or newer.
- **Dolt is not found.** Put it on `PATH` or pass `serve --dolt`.
- **The home is refused.** A relative home is refused; the home must be outside
  repositories and its physical path must contain no symlinks.
- **A launch fails on the instance filesystem.** The home needs hard links or a
  no-replace rename (`renameat2` / `renamex_np`); see
  [processowner](processowner/README.md).
- **`--live-config` is refused.** It must name an owner-only (0600) regular file
  outside every repository and worktree.
- **`serve` asks for `--live-config`.** `serve` defaults to `--adapter multiplexer`
  (Herdr), which needs `--live-config`. For a first run use
  `--adapter scripted`.
- **Unix-socket tests fail.** They need a short scratch path; see
  [TESTING.md](TESTING.md).
- **Where are the logs?** `HOME/INSTANCE/supervision.log` (daemon),
  `HOME/INSTANCE.serve.log` (a detached serve), `HOME/INSTANCE/alerts.log`
  (alerts as JSON lines); read alerts with `fae alerts [list] [--unacknowledged]`.
  See the logging paragraph under [Run locally](#run-locally) in the Reference.

## Reference

### Run locally

Requires Go 1.27.2, Git 2.45 or newer (changed-base reconciliation passes trees
to `git merge-tree`; Ubuntu 24.04's stock 2.43 refuses them), and Dolt. Live
runs also need lsof, ripgrep for `scripts/dogfood.py`, Herdr (`herdr-core`) for the multiplexer
adapter, and a logged-in Claude Code (default) or Codex CLI. The daemon
initializes and supervises its own private Dolt database; clients never connect
to an existing database directly.
The instance home must be on a filesystem with hard links or a no-replace rename
(Linux `renameat2`, macOS `renamex_np`); owned-process records are placed that
way, and a launch fails if neither works ([processowner](processowner/README.md)).

```sh
mkdir -p .cache/go-build bin
export GOCACHE="$PWD/.cache/go-build"
go build -o bin/fae ./cmd/fae

# Terminal 1, from the canonical workspace root. Supply --dolt if it is not on PATH.
./bin/fae serve --workspace "$PWD" --adapter scripted --foreground

# Terminal 2; use the absolute path to bin/fae when outside this workspace.
./bin/fae message --id demo-authorization --text '{"text":"Run local scripted work and shut down this instance.","authorize":{"classes":["task","runtime"]}}' 
./bin/fae run --id demo --prompt 'Exercise the scripted default workflow' --authorizing-message demo-authorization
./bin/fae status --json
./bin/fae history --task demo --json
./bin/fae board --limit 100 --json
./bin/fae history --task demo --summary --limit 100 --json
./bin/fae events --after 0 --follow
./bin/fae shutdown --authorizing-message demo-authorization
```

To run Fae on a clone of itself with real Claude turns, see [docs/SELF_HOSTING.md](docs/SELF_HOSTING.md).

`serve` defaults to `--adapter multiplexer` (Herdr `herdr-core`; needs `--live-config`);
`process` is an alias of `direct`. Without `--foreground` it detaches once ready,
prints the start line and logs to `HOME/INSTANCE.serve.log`; `run --start`
relaunches a stopped instance from its recorded serve arguments. Other serve
flags: `--default-mode local|normal` (disposition for submissions that state
none; `normal` needs a live adapter), `--max-panes N` (multiplexer; 0 keeps the
live configuration or 60), `--max-heavy-weight N` (live adapters; 0 keeps the
live configuration or 2), `--max-tasks-in-flight N` (default -1, unbounded), and
`--max-molecules N` / `--max-workflow-molecules N` (running molecules overall and
per workflow; default -1, unbounded; a Pending task that would start a molecule
beyond either is withheld as `molecule_limit`). `--ui` supervises the optional UI child (`--ui-port`,
`--ui-bind` (literal loopback only, default 127.0.0.1), `--ui-bin`, `--ui-dist`,
`--ui-allow-writes`, `--ui-rpc-log`; see [ui/README.md](ui/README.md)).
A flag carrying a configurable default that the spec states falls back to that
default when its value is empty, out of range or not finite. These flags are the
common `--home` and `--instance`; serve's adapter, default mode, UI bind, limits,
waits, windows, retention, Dolt port and stop wait, inline limit, re-arm budget
and Prophet failovers; the watchdog's stall, re-arm and filesystem bounds; the
roll and maintenance waits; `land --then-roll`'s `--roll-max-wait` and
`--overlap-wait`; `gate run`'s `--memory-floor-gib`, `--max-wait` and
`--capacity`; `heavy-op run --max-wait`; `census --gap`; a loop's
`schedule create --expires-in`; and `provider-health --window`. Each fallback
writes one warning line to standard error naming the command, the flag, the
value and the default, in the daemon's log-line form
(`TIME instance=NAME component=cli warning: ...`). The daemon's ProviderHealth RPC
applies the same fallback to an out-of-range `window_seconds` from any client
and names it in the response's `window_warning`. `--home`,
`--instance`, the adapter, the mode and the bind fall back only when empty: a
relative home, an invalid instance name, an unknown adapter or mode and a
non-loopback bind are still refused, so a mistyped instance never reaches the
default one. `gate run --memory-floor-gib` accepts any finite floor from the
host's hard memory floor up to 1 PiB, so a floor between the hard floor and
12 GiB lowers that run's landing floor, as Appendix A allows. NaN, an infinity,
a floor above 1 PiB and one below the hard memory floor fall back to 12 GiB. A
value that does not parse (`--inline-limit x`) is a usage error (2). Some flags
keep their own checks and refuse an invalid value instead of falling back.
`--ui-port`, `watchdog --period`, `gate run --slice`, `land --poll` and
`metrics --window` have no default in the spec; `land --poll` and
`watchdog --period` must be at least 10 ms, since a shorter interval would run
the landing's status reads (an IPC round trip and a plane read each) for the
whole landing, or the watchdog's ticks (a daemon read and a state write each)
for its whole life, nearly back to back. `controller.Watchdog.Run` refuses a
shorter period from any caller. `runtime-config`'s limits and
the `--weight` of `gate run` and `heavy-op run` do (Appendix A's pane limit,
heavy capacity, tasks in flight and weights), but a fallback would be wrong for
them: a `runtime-config` limit changes a running instance (0 leaves a value
unchanged, and a refused change keeps the running value), and falling back to
weight 1 would under-count a whole-suite run. The pane-limit floor is still a
refusal.
`fae help [--json]` lists every verb; `fae COMMAND --help` prints its flags, and
each verb (and each `maintenance`, `drive` or `backlog` action) accepts only its
own. Help, `version`, the offline `workflow` forms, `template workflow-validate`,
`gates self-test --offline` and `doctor` need no home (doctor reports a missing
one as a warning), and an explicit `--home` needs no `$HOME`.

The default home is `~/.fae/instances`, with `--home PATH` and `--instance NAME`
(default `default`) available on commands, before the verb or after it, as
`--flag VALUE` or `--flag=VALUE`. The instance home must be outside repositories, and its
physical path must contain no symlinks. Instance directories and credentials are
owner-only. A start must use exactly the bound workspace and current directory;
reads and other RPC commands work from any directory. Only `serve` starts a daemon:
`run --start` re-runs the recorded `serve` invocation, and `maintenance start` and
`roll` relaunch it through the runtime launcher. Other commands never start one;
they exit 3 when it is unreachable.

First start installs the shipped default workflow as immutable revision 1. It includes
intake, spec/review, plan, RED author/run/gate/freeze, implementation/compare,
reviews, Oracle routing, QA, reconcile, protected commit, integration and Done.
After integration, Normal tasks run a Normal-only code-review sub-workflow
(ticket open, ticket in review, draft review create, review read with a
remediation loop capped at 5, publish, merge, ticket closed); Local tasks pin its
Local projection, which is the Local pipeline byte for byte.
Failures are routed through gates and bounded remediation. With the scripted adapter, HostCommand and VCS
outputs are fixtures; the source identity freeze/compare uses Go syntax trees.
The same content runs live unpatched: role routes are Claude `claude-opus-5-5`,
then `claude-sonnet-5-5`, then Codex `gpt-6-astra` (medium, then low); RED and
green runs use `go test -json -count=1 -a -x ./...`, which the broker narrows to
`-run=^TestID$` at heavy weight 1, and QA runs the whole suite at weight 2. At
each start the daemon reconciles the stored default with the shipped one
(`defaults.Generation`, now 3): it installs the shipped content as a new audited
revision when the latest stored revision fails current validation or is an older
daemon-installed generation, and keeps a valid operator revision, a retired
default or a builtin of the same or a newer generation. Tasks keep their pinned
revisions. Status `default_workflow` reports the outcome (`installed`,
`current`, `upgraded`, `operator-revision`, `kept` or `retired`).

```sh
./bin/fae workflow default --json           # shipped content; --local prints the Local projection
./bin/fae workflow scenario --json
./bin/fae workflow validate examples/workflow.json
./bin/fae workflow simulate examples/workflow.json examples/script.json demo
```

`serve --scenario FILE` uses custom stage fixtures; otherwise it uses the built-in
happy-path rehearsal, which `workflow scenario` prints. `workflow simulate` (and
`workflow-simulate --scenario`) takes either the simulator's own records
(`examples/script.json`) or such a scripted-adapter scenario: a `TASK/stage`
key applies to that task only, each output (or review) becomes a record of the
stage's output artifact, freeze and compare identities are computed from the
declared test source, a `target-moved` step on an Integrate stage selects its
reverify route, and a step typed with any other outcome is refused.
`template workflow-put --content FILE --authorizing-message
ID` installs validated new revisions through the daemon; a document with any
issue is refused before the daemon with every issue listed, the write rules
(a Claude `minimal` route) among them. `workflow-get --workflow
ID --revision N`, `workflow-latest`, `workflow-list`, `workflow-diff --workflow ID
--from N --to M` and `workflow-simulate --scenario FILE (--content FILE |
--workflow ID [--revision N])` also go through the daemon; `workflow-validate
--content FILE` is offline and lists every error with its document path.
`workflow-retire --workflow ID --reason TEXT` refuses new bindings. In-flight
tasks keep their original revision unless `migrate --task ID --revision N` moves
them. Admission can use `--idempotence-key KEY`; retries return the original
admission response without changing the current checkpoint. `run --task-class
CLASS` selects one of the workflow's declared `task_classes` (default its
`default_class`), whose overlays set route effort and briefs; `run --carry-from
TASK` re-admits an earlier task with unchanged content on the same workflow
revision, carrying its accepted pre-code evidence (FAE-WF-086); like any task it
forks from the current integration tip, waits in Pending until released, and
then enters its carried stage. `run --declared-file PATH` (repeatable, also on
`start-work-item`) declares the task's files; a live instance without pinned
`files` takes each task's scope from them (see [LIVE.md](LIVE.md)).

HostCommand templates may use typed whole-argument `{{name}}` placeholders, each
bound to exactly one literal argument. Target configuration schema 4 declares the
command shapes (typed `{{type}}` slots) they must fit, path-scoped
`required_tests` and a `compile_check` that whole-suite QA must prove for the
exact tree before commit, and a diff classifier under which a production-code
commit without RED evidence escalates `NoRedViolation` ([target
policies](daemon/TARGETS.md)). HostCommand test runs may declare a `test_shape`
(`go-unit`, `go-integration`, `script` or `runner`, the last two reporting TAP;
[grammar](workflow/GRAMMAR.md)). The workflow primitive
library is complete: HumanDecision, FanOut/FanIn (branches run sequentially) and
SubWorkflow, with content-addressed, replayable gate records.

The CLI exits 0 on success, 1 on refusal or validation failure (including a
mutating verb without `--authorizing-message`), 2 on usage errors and 3 when the
daemon is unreachable; the reason goes to standard error. `gate run` exits 75
when its admission wait times out and 76 when admission is refused; the hidden
`heavy-op run` broker verb exits 75 on a lease or host refusal, 77 when a role
shim policy refuses and 78 when Local mode refuses a git push or remote change.
Both otherwise exit with the child command's code, except that `gate run` exits
1, with nothing run, when it cannot read its working directory or make a run
identity, and an account-mode `heavy-op run --shim-policy` exits 1 when
cancelled before the tool runs and 126 when the tool cannot be executed. The
`heavy-op hook` pre-tool hook exits 0 with its decision or 2, which blocks the
tool call; never 1. Each
command's `--help --json` lists its exit codes; for these three it lists their
own codes rather than the general table.
Recorded times come from the daemon clock; explicit time
inputs (`--until`, `--anchor`, `--end`) are recorded as given. See [LIVE.md](LIVE.md)
for live adapter configuration, Normal-mode adapters, the Claude Code (default)
and Codex bridges, and their trust boundaries. `--live-config` must name an
owner-only (0600) regular file outside every repository and worktree.

Operator verbs: `message --task ID` and `reply --to MESSAGE_ID` record threaded
messages (the daemon assigns IDs unless `--id` is given; controller relays never
authorize); `inbox` lists threads plus escalated, blocked, uncertain, decision
and question items; `attach-design-doc` binds a URL or contained path to a task
or backlog item for later briefs; `reclaim [--dry-run]` releases role-slot and
session entries (with `--host-wide`, also heavy-op and pane entries) whose owner
is proven dead;
`analyze-session --task ID` reports provenance-labelled timings; `doctor` checks
prerequisites, instance layout, sandbox and filesystems offline; `migrate` moves a
non-terminal task to another revision of its workflow. `task create|update|delete|show`
creates one task joined to the live graph, edits a task (from its next stage while a
turn runs), deletes one (it is withdrawn or stopped, then retired, keeping its
history) and reads one with its edges and stage trace
([live task verbs](daemon/TASKS.md)). `decide --task ID --option
OPTION [--decision ID] [--rationale TEXT]` disposes a HumanDecision park;
`quarantine [inspect] | release | retire` resolves a quarantined task
([task controls](daemon/TASK_CONTROLS.md)). `history --task ID --records` pages
the plane's transitions, artifacts and decisions; `topology [--workflow ID]`
pages running molecules with their stage graphs; `provenance --commit SHA` walks
a commit back to the spec ([read contracts](daemon/READS.md)). `ui token` gives
the instance operator the UI token and one-time login URL. `land`, `gates self-test`
and `gate run` are described under [deployment](LIVE.md#deployment-and-independent-drive-operations);
landing now uses the task's approved scope, and the gate-pass, landing-floor,
target-advance and gated-deploy predicates are Gobra-checked in `verify.sh`.

The daemon writes `HOME/INSTANCE/supervision.log` (mode 0600). Each line is
`RFC3339-UTC-time instance=NAME component=NAME text` (CLI warnings take the
same form with `component=cli`; they go to standard error, and a detached
serve's also to its rotated `HOME/INSTANCE.serve.log`); the file rotates at 64 MiB
keeping four older files (`.1` newest) and is copied to standard error through a
bounded queue that drops lines rather than block. Every committed park,
escalation and route refusal (a binding the bridge refused) also gets one
structured `component=task` line paired with its event row. Alerts (`host-admission` hard refusals, `stuck-commit-stage`, `process-leak`,
`dead-edge`, `route-exhaustion`, `ui-child`, `scratch-quarantine` and controller alerts such as
`task-quarantined` and `drive-step-fault`) are plane rows with an `alert-raised` event, mirrored as
JSON lines to `HOME/INSTANCE/alerts.log`. Read them with `fae alerts [list]
[--unacknowledged]`, acknowledge with `fae alerts ack --id N
--authorizing-message ID`; `fae metrics [--window D]` and `fae host-metrics`
return derived and host metrics. See [read contracts](daemon/READS.md).

### Architecture and durability

`domain`, `kernel` and `workflow` hold value types, safety predicates, graph rules,
three-valued gates, schema checks and normalized test identities. `defaults` holds
the shipped graph as data. `engine` prepares dispatches and validates completion
against the pinned workflow, task, tree, token, sessions and artifact schemas.

`daemon` is the singleton writer: it checkpoints dispatch intent before launching
asynchronous execution, then atomically saves completed evidence, transitions,
audit and ordered events in `controlplane`. The control plane uses versioned Dolt
commits and chunked content-addressed payloads (schema version 10). Admission binds
each task to a molecule (workflow, revision and content) in its transaction, and
every checkpoint transaction also writes the relational task, transition,
decision, artifact and provenance rows; Dolt triggers refuse updates and deletes
of the last four. Completed stages are not replayed.
Graceful stop lets in-flight commit/integrate steps finish within a 120 s drain
bound and writes a clean-stop marker listing the turns left in flight. On restart
the scripted adapter replays an interrupted token without charging retry budgets;
a live turn named by the marker is replayed without charge only after its
workspace and session retirement are proved. Other interrupted or uncertain
executions park, and an invalid stored task is quarantined rather than refusing
the instance. A bounded (three-attempt) Dolt-child restart revalidates the schema
and resumes from durable state.

`api` serves authenticated unary RPC and NDJSON events on the Unix socket.
Status uses an immutable in-memory projection. Event clients resume by sequence;
slow streams do not hold the drive lock. `scheduler`, `hostguard`, `runtime`,
`workspace` and `worktree` provide the scheduling, admission, runtime and isolation
foundations. Live launches use durable role slots and a host-shared weighted
ledger. The Herdr (`herdr-core`) adapter creates private owned servers and gated panes; exact
process identity records and parent-death watchdogs support conservative orphan
recovery before new work starts. Unproven identities do not authorize cleanup or
lease reclamation. Agent turns take a role slot and host/pane admission but no
heavy-op unit. A rate-limited, unavailable or auth-failed turn that left the tree
unchanged rotates once to the next route; other launched failures park. A
process census raises leak alerts: the full walk, which also reports foreign
processes, every drive round while there is work; while idle, a census of the
daemon's own tree every 60 s, and the full walk when its descendant set changes
and at least every 10 minutes. A running brokered command's defunct children
are left to it for two censuses, then count against the defunct bound, and a
defunct leak names the processes that have not collected their children. On Linux
the daemon is the child subreaper of its descendants, reaps adopted orphans and
kills them at shutdown ([processowner](processowner/README.md)); complete
cross-user host process inventory remains open. An idle daemon blocks on events
and deadlines instead of polling the plane.

`ui` is the optional UI child: embedded assets, a token required on every
request, loopback-only binding with a Host check, read-only unless
`--ui-allow-writes`. The daemon supervises it with bounded restart
([ui/README.md](ui/README.md)).

`fae-tui` (`go build -o bin/fae-tui ./cmd/fae-tui`) is a terminal view of a
running daemon over its RPC socket: each running molecule's stages and their
tasks, the backlog graph laid out by dependency depth with every task's
implementation state, a task's description, edges and trace of stages, and the
escalated tasks and alerts that need the operator. It is read-only unless
started with `--write`; then it creates, edits and deletes tasks and edits
edges through the same authorized RPCs as the CLI. Like `fae-ui` it is a
separate binary ([tui/README.md](tui/README.md)).

Changed integration bases use immutable three-way merge plans with own-hunk
preservation checks. The merged tree repeats tests, frozen-test comparison,
reviews, Oracle and QA before commit. Supported text conflicts return to bounded
implementation with named paths and an immutable original-change contract. Lost
own hunks, frozen-test conflicts and unsupported classes refuse. A second target
advance during an active conflict contract requires operator review. Integration also recognizes ancestry,
patch-equivalent and touched-blob containment without moving the containing tip.

### Tests and formal verification

```sh
./scripts/test.sh
FAE_TEST_DOLT_BINARY=/absolute/path/to/dolt ./scripts/test.sh
FAE_TEST_DOLT_BINARY=/absolute/path/to/dolt python3 scripts/smoke-cli.py
./scripts/install-verifiers.sh
./scripts/verify.sh
```

The full suite uses isolated private Dolt instances and tests real IPC, concurrent
tasks, authority refusal, idempotence, schema rejection, event replay, graceful
restart, malformed sibling isolation and child-process recovery. It asserts
nonzero executed tests and checks test scratch cleanup. Never supply a developer
control-plane DSN; the integration harness accepts only a Dolt executable.
`scripts/test.sh` runs every other package first and then the daemon package
alone (about 50 minutes in total). Daemon live tests admit against real host
pressure; `FAE_TEST_QUIET_HOST_PRESSURE=1` reports quiet readings for
development runs only.

Gobra production-guard proofs (each with a rejected weakened mutation) and
Gomela/SPIN bounded models (each with a firing negative control) are censused in
the [acceptance ledger](verification/LEDGER.md); `scripts/check-ledger.py` fails
the suite on an unmapped or stale check. These are scoped proofs/models, not
whole-daemon verification. Unix-socket tests need a short scratch path; see
[TESTING.md](TESTING.md). Also see [formal results](verification/README.md) and
[implementation status](IMPLEMENTATION.md).

Two real Codex `gpt-6-astra` tasks completed through **Herdr 0.9.5**, private
Dolt, genuine RED/GREEN tests and a drained mid-run daemon restart. Their
checkpoint artifacts survived unchanged. One task exercised the integration
race retry and refreshed all approvals: 35 artifacts at version 83; the other
completed with 22 artifacts at version 52. The capacity-2 host ledger ended
with 58 grants, 58 releases and no holders. See [LIVE.md](LIVE.md) and
[recorded acceptance evidence](verification/results/herdr-dogfood/summary.json).

Only trusted single-operator workspaces are supported. No hostile filesystem or
escaped-process sandbox is claimed. Complete process inventory and lease
reconciliation, interactive brokered agent tools, unsupported conflict classes,
and remaining operator/controller and Normal-mode phases remain open.

The runtime-recovery increment adds audited dynamic pane limits, trusted-adapter
idle-session shedding and bounded safe stall retry. Its final suite passed 494
cases with race detection and vet; authenticated two-task dogfood also passed,
including changing the cap during active work and preserving it across restart.
See the [current verification record](verification/results/runtime-recovery.json)
and [fresh provider result](verification/results/runtime-recovery-dogfood-20261017/result.json).

The latest increment adds optional persistent provider context, dynamic heavy-op,
host-pressure and task limits, and bounded startup recovery. **530 tests passed**
with race detection and vet (77.8% coverage). Fresh two-task dogfood passed with
29 real turns, eight context resumptions, preserved host configuration across
restart and all 58 leases released. Task-bound contexts stay within their task;
reviews stay fresh. See [latest verification](verification/results/persistent-host-startup.json)
and [provider continuation evidence](verification/results/persistent-host-startup-dogfood-20261017/result.json).

The operator/read increment adds server-bound delegated principals, transactional
message-class grants, ten durable task controls, coalesced bounded reads, and
explicit event-retention/overflow gaps. See [authority](daemon/AUTHORITY.md),
[task controls](daemon/TASK_CONTROLS.md) and [read contracts](daemon/READS.md).

The authenticated operator/read dogfood also passed: both tasks reached Done,
with 34 completed provider turns, five loop iterations, seven reused resident
Herdr panes, mid-turn SIGKILL recovery, independent drive restart and guarded
daemon roll. Host grants ended with 62 releases and one recovery out of 63,
zero holders and zero pane reservations. Source, binary and formal-input hashes
match the archived evidence. See the
[complete phase record](verification/results/operator-controls-reads.json).

The next increment implements [Prophet sessions](daemon/PROPHET.md),
[backlog dependencies](daemon/BACKLOG.md) and
[safe post-commit restart](daemon/POSTCOMMIT.md). Verification passed 1,062
race-enabled cases across 26 packages (74.6% coverage), vet, builds, 36 Gobra
guards/36 rejected mutations and 37 positive/35 negative Gomela models.
Authenticated dogfood passed both tasks, 36 provider turns and task/global
Prophet replies, with no final lease holders or pane reservations. See the
[current verification record](verification/results/prophet-backlog-restart.json).

### Retirement, target policies and Prophet routes

This increment adds audited task retirement with atomic dependency transfer,
contained-release and obsolete-hold behavior, plus retired read filtering. It also
adds immutable Local target policies pinned across run/backlog/cron admission and
restart, and Prophet routes using durable provider health with cleanup-gated,
bounded failover. See [retirement](daemon/RETIREMENT.md),
[target policies](daemon/TARGETS.md) and [Prophet routing](daemon/PROPHET.md).

The final full suite passed **1,116 race-enabled cases across 26 packages**,
with **74.1% statement coverage**, vet, both builds and strict scratch cleanup.
Gobra verified **39 production guards** and rejected **39 mutations**;
Gomela/SPIN passed **40 positive / 38 firing negative models**. All 115 formal
input hashes match. Independent review found and corrected a supersession-chain
edge case before this final full rerun.

Expanded authenticated dogfood passed both tasks with **36 completed provider
turns**, five provider-loop iterations and seven reused resident Herdr panes.
Both tasks retained target revision 1 despite a restrictive revision 2 and
mid-turn crash/restart. Both Prophet scopes answered and recorded provider health.
Retirement transferred a dependency, then kept it held after obsolete retirement;
retired rows remained hidden by default. All 69 host grants were released (68)
or recovered (1), with zero final holders and zero pane reservations.

See the [verification record](verification/results/retirement-target-routes.json)
for final logs, source/evidence hashes, the earlier full pass, review findings and
limits. Explicit targets currently support Local mode only; unsupported target
policies fail closed. Containment without retained task evidence cannot yet be
recovered from integration commit subjects alone.

### Normal targets, census and retired cleanup

Target schema v2 binds Normal-mode destination, push namespace, installed tool
definitions, review baseline/workspace policy and ticket orchestration. Tasks keep
immutable revisions; adapter drift refuses admission or emission. Target v3 selects immutable
adapter bundles installed through operator commands; review workspaces are prepared
explicitly before admission.
See [target policies](daemon/TARGETS.md).

`fae census` emits a reviewable snapshot; `fae census apply` retires unchanged
rows one at a time through the audited control, stopping with partial progress
on refusal. `fae backup plane` verifies a Dolt restore before producing backup
evidence. `fae cleanup retired` previews by default; apply verifies backup coverage
and archives all candidate tips before removing owned clean worktrees and branches.
`fae cleanup done` does the same for Done tasks whose own landing is in the
integration ref, and lists the others as skipped.
See [census](daemon/CENSUS.md) and [cleanup](daemon/CLEANUP.md). Marked scratch instances can be removed through the
[scratch cleanup controller](daemon/SCRATCH.md), which retains verified backups
outside the instance root. Automatic backup pruning remains unimplemented.

Verification: **1,165 race-enabled cases**, Gobra **42 guards / 42 rejected
mutations**, Gomela **43 positive / 41 negative controls**, and a passing
**36-turn** real-provider run with crash/restart, mixed-mode isolation and
verified retired cleanup. See the [verification record](verification/results/normal-target-census-cleanup.json).

Scratch cleanup and dynamic adapter/workspace provisioning are implemented and
verified. The latest increment passed 1,242 race-enabled cases,
46 Gobra guards and 47 positive/45 negative Gomela models, followed by a
36-turn real-provider run with restart and verified scratch deletion.
See the [verification record](verification/results/scratch-provisioning.json)
and [implementation limits](IMPLEMENTATION.md).

Adapter retirement and configurable fleet provisioning are now implemented.
Explicit preparation recovers uncertain provisioning through receipt lookup;
retirement blocks new bindings while preserving existing tasks. Verification
passed 1,264 race-enabled cases, 48 Gobra guards, 49 positive/47 negative
Gomela controls and a 36-turn real-provider run. See the
[verification record](verification/results/adapter-fleet.json).

### Backup retention and replication

Operator backup lifecycle commands now support reviewed retention and configurable remote replication. Retention preserves historical job coverage and replication sources; uncertain uploads query durable remote proof and verify the downloaded archive before confirmation. See [backup commands and protocol](daemon/BACKUPS.md).

1,293 race-enabled test cases across 26 packages passed, with 73.5% statement coverage, vet and scratch census. Gobra verified 50 production predicates and rejected 50 mutations; Gomela/SPIN passed 51 positive models and observed 49 firing negative controls. The two-task real-provider run completed 36 turns, recovered one lost-acknowledgement upload after restart without a duplicate, restored the downloaded Dolt backup and passed fsck, then pruned a redundant local backup and cleaned the scratch instance. [Verification record](verification/results/backup-lifecycle.json).

### Automatic retention and redundant scratch archives

`backup retention-schedule configure|pause|resume|status` adds opt-in durable retention of existing plane backups. Each run rechecks explicit authorization and preserves historical coverage and replication pins. `cleanup scratch-archive` removes only a reviewed exact duplicate while retaining the canonical recovery archive and Git refs. See [retention policy](daemon/BACKUPS.md) and [archive cleanup](daemon/SCRATCH.md).

1,319 race-enabled test cases across 26 packages passed, with 73.6% statement coverage, vet and scratch census. Gobra verified 52 production predicates and rejected 52 mutations; Gomela/SPIN passed 53 positive models and observed 51 firing negative controls. Real-provider acceptance completed 36 turns and verified automatic pruning after pause/restart/resume, canonical archive survival, duplicate removal and retry. [Verification record](verification/results/retention-automation.json).

### Automatic backup creation and replication

`backup creation-schedule configure|pause|resume|status` creates verified local backups on an opt-in interval. `backup replication-schedule` independently sends the oldest unreplicated verified source, retaining its exact claim across uncertain results and restarts. Both default to a one-hour interval, revalidate explicit authority and honor host limits. See [commands and recovery limits](daemon/BACKUPS.md).

1,337 race-enabled test cases across 26 packages passed, with 73.7% statement coverage, vet and scratch census. Gobra verified 54 production predicates and rejected 54 mutations; Gomela/SPIN passed 55 positive models and observed 53 firing negative controls. Real-provider acceptance completed 36 turns, automatically created and replicated a verified backup, preserved pending replication across restart, refused destination changes while pending and observed no duplicate uploads. [Verification record](verification/results/backup-schedules.json).

### Replication unpinning and backup quotas

`backup unpin --backup PATH` previews a digest-bound release after verifying all remote replicas; `--apply --plan-digest DIGEST` applies it. Pending claims refuse release. `backup quota configure --max-bytes N --max-backups N` persists limits; `backup quota status` reports usage, including partial backups and replication spools. Mutations require operator authority. Dolt export limits use admission/checkpoints and preserve overruns; no quota implicitly deletes data. See [backup policy](daemon/BACKUPS.md).

1,352 race-enabled test cases across 26 packages passed, with 73.6% statement coverage, vet and scratch census. Gobra verified 56 production predicates and rejected 56 mutations; Gomela/SPIN passed 57 positive models and observed 55 firing negative controls. [Evidence](verification/results/backup-storage.json).

### Claude provider, host admission and acceptance ledger

Claude Code is now the default provider through the restricted `cmd/fae-claude-bridge`
(see [provider/README.md](provider/README.md)); the Codex bridge remains. Host admission
samples PSI *full*, holds pressure until two below-threshold readings 30 s apart, and runs
host-heavy commands through a two-reading I/O gate before the heavy-op lease
([hostguard/README.md](hostguard/README.md)). Every formal check is censused in
[verification/ledger](verification/LEDGER.md), enforced by `scripts/check-ledger.py` in CI.

On Ubuntu 24.04: 1,406 race-enabled test cases across 27 packages passed (74.0% coverage, vet,
ledger and scratch census). Gobra proved 59 production guards and rejected 59 mutations;
Gomela/SPIN passed 57 positive models and observed 55 firing negative controls. The full
real-provider acceptance passed with 36 Claude (`claude-opus-5-5`) turns, crash/restart
recovery and all controller, provisioning and backup checks
([evidence](verification/results/claude-host-admission-dogfood-20261022/result.json)).

### Wave 2: records, primitives, Normal content, UI and recovery

Waves 1 (2026-10-22) and 2 (2026-10-23) were parallel stream rounds; see
[HANDOFF.md](HANDOFF.md) for per-stream detail and open gaps. Wave 2 added
molecules and same-transaction relational records with immutable provenance
(schema 8, `history --records`, `topology`, `provenance`); the full primitive
library with `fae decide`; target schema 4 policy, task classes, governed
scope-widen/RED-rework requests and `--carry-from`; the Normal review
sub-workflow in the shipped default with audited default upgrade and a 33-run
parity corpus; the optional UI child; `fae quarantine`, deterministic planned
replay, automatic adoption of completed local effect receipts and a scripted
every-boundary crash matrix; role shims through `heavy-op run`, a per-run process
cap (live config `process_limit`), a child subreaper and an idle daemon without
plane polling; and a real-host-pressure test suite.

`scripts/test.sh` with real host pressure: 2,049 race-enabled test cases across 28
packages passed (78.4% coverage, vet, ledger and scratch census). `verify.sh`:
Gobra 64 production guards with rejected mutations; Gomela/SPIN 57 positive / 55
negative models. The ledger maps 101 of 284 requirement IDs to at least one check.
Eight newer pure guards are not yet in `verify.sh`. Real-provider acceptance is
recorded in [TESTING.md](TESTING.md).
