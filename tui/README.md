# Terminal UI (fae-tui)

`fae-tui` is a terminal view of a running daemon, built with Bubble Tea. It
shows each running molecule's stages and the tasks at them, the backlog graph
with every task's implementation state, a task's description, edges and the
trace of stages it has been through, and the tasks and alerts that need the
operator. With `--write` it also creates, edits and deletes tasks and adds and
removes edges.

It is a separate executable, like `fae-ui`: the core `fae` binary links neither
this package nor its charm modules (FAE-GEN-010 non-goal 6;
`TestCoreBinaryExcludesTheOperatorUI` checks both). It reaches the daemon only
over the instance RPC socket, as any client does, and never imports the daemon
package. It never starts a daemon and never creates a credential.

## Build and run

```sh
go build -o bin/fae-tui ./cmd/fae-tui
./bin/fae-tui                                   # ~/.fae/instances/default, read-only
./bin/fae-tui --home /abs/instances --instance dev --write --refresh 5s
```

| Flag | Default | Meaning |
|---|---|---|
| `--home DIR` | `~/.fae/instances` | instance home, resolved as the `fae` CLI resolves it (an empty value keeps the default) |
| `--instance NAME` | `default` | instance name |
| `--write` | off | allow the writes below; without it the TUI never sends one |
| `--refresh D` | `2s` | base polling interval, 250ms to 10m (see Live data: polls slow down while nothing changes) |

It reads `HOME/INSTANCE/run/credential` (an owner-only 32-byte file, refused
otherwise) and dials `HOME/INSTANCE/run/rpc.sock`, so it runs as the instance
operator. Exit codes: 0 on quit, 2 for a bad flag, 1 if the terminal fails. It
needs a terminal of at least 40×12; a smaller one shows only a `terminal too
small` notice (and `q` still quits) until it is enlarged.

## Live data

The first refresh reads Status, Board, BacklogShow (the backlog graph),
Topology (molecules), Alerts (unacknowledged) and Inbox. After that, each poll
reads Status, Board, Alerts (unacknowledged) and Inbox on every view (so the
header, the tab counts including Attention, and each task's state stay fresh),
plus Topology for Molecules and the backlog graph for Graph; switching views
refreshes the new view at once, and an open task is re-read with each poll.
The reads run all at once and off the interface loop, so a slow read never
blocks a key press; a refresh that is still running is not started again (one
more follows it if the view changed meanwhile).

`--refresh` is the base interval. While nothing changes, polling backs off:
after five unchanged refreshes the interval doubles per refresh, raised to at
least the daemon's own snapshot age (which a faster poll could not beat) and
then capped at eight times the base (16 s at the default, and at most 10m). Any
change, a key (`r` polls now), a view switch or a write brings it back to the
base; the header says `idle: polling every …` while it is stretched.

The header shows the daemon's pid and uptime and how old the snapshot is (the
daemon's own snapshot age plus the time since the read). A read that fails
keeps the previous data and is named in the header. When the daemon cannot be
reached (no credential yet, nothing listening) the header says `daemon
unreachable`, keeps the last data with its age, and tries again every
interval. The event stream is not followed: polling the cached reads is cheap
and keeps the client simple.

## Views

Switch with `1`, `2`, `3`, `tab` or `shift+tab`; `enter` opens the selected
task; `esc` goes back; `?` lists every key; `q` or `ctrl+c` quits. Arrow keys
or `h j k l` move; `pgup`, `pgdown`, `home` and `end` page. `r` refreshes now
and `R` shows or hides retired tasks.

- **Molecules.** The running molecules (workflow, revision, task count) and,
  for the selected one, its stage graph in the workflow's normal-case order
  with the normal-case roles, then the off-path stages that hold tasks
  (failure routes, Blocked, Escalated). A `SubWorkflow` stage shows its pinned
  target workflow `→ <workflow>@r<rev>`, and a task with a `SubWorkflow`
  parent or active child shows `parent: <task> (<stage>#<visit>)` or
  `child: <task> (<workflow>@r<rev>)` (or `children (N): <c0>, <c1>, ... (<workflow>@r<rev>)`
  when a fan-out `SubWorkflow` stage spawns multiple child tasks). Each stage lists its tasks with their
  priority and state. `←`/`→` pick a molecule, `↑`/`↓` a task, `p` and `c`
  jump to the selected task's parent or active child task/molecule (when a
  parent has multiple fan-out children, consecutive presses of `c` cycle
  through `Children[0..N-1]`), `z` hides
  the empty stages.
- **Graph.** The backlog graph: every Board task and backlog item, laid out in
  layers by dependency depth over blocking edges (longest path, so each task
  sits one layer below its deepest prerequisite; ties keep priority order, and
  each layer is ordered by its prerequisites' rows to keep edges short). The
  layout is deterministic and cannot hang: a malformed graph with a blocking
  cycle still lays out, with the cycle's tasks in a last layer marked
  `cycle`. Columns show as many layers as fit and scroll; `v` switches to a
  list with each task's full state and edges. Every task shows its state by
  glyph, colour and label: `○ ready`, `◌ withheld` (and why: the prerequisites
  that hold it, or the drive's withheld reason), `◌ deferred`, `◆` its stage,
  `▶` its stage with a turn in flight, `◌ parked` (and why), `■ Blocked`,
  `! Escalated` (and the kind), `× deleting` (the Board's stored
  `pending_delete` flag: a TaskDelete that has not finished; the task view
  says where it stands), `✓ Done`, `× Cancelled` or `Retired`. The panel
  under the graph names the selected task's prerequisites (`↑`) and dependents
  (`↓`) with their states; relates edges are `~`; a dead edge (a prerequisite
  that can no longer release it) is marked `✗ dead`. `p` jumps to the parent
  task/molecule (or to a prerequisite edge), `c` jumps to the active child
  task/molecule (cycling through `Children[0..N-1]` across consecutive presses
  on a multi-child parent), `n` jumps to a dependent, `/` finds a task by id, `f` hides
  finished tasks. Several hundred tasks lay out and scroll at once.
- **Attention.** What needs the operator, most urgent first: escalated tasks
  (kind, reason, and since when; one with a stored pending delete is marked
  `delete pending` from the Board's flag, and, once its `TaskGet` has been
  read, says whether it retires once its turn in flight is collected, is
  uncertain after a restart and needs `fae resolve-uncertain --action escalate
  --id <id>` to finish retiring, is retiring at the next drive round, or failed
  and stays Escalated; the escalation reason text is never used to guess this),
  quarantined tasks, uncertain dispatches,
  decisions waiting for the operator, blocked tasks, unanswered Prophet
  questions, and unacknowledged alerts. `enter` opens the task.
- **Task.** Title, description, acceptance (the description is the acceptance
  text; its digest is shown), state, stage, why it waits, escalation kind,
  reason and time, priority, workflow, target, molecule, source, the
  structured fields (declared files, test filter, test shape, plus any
  `superseded_fields` that a turn in flight is still running under), attempts, a
  pending delete (`TaskGet`'s `pending_delete`: its reason, successor, request
  time and status), the `SubWorkflow` **Hierarchy** section (`Parent` and
  `Child` links with `p` and `c` to jump between parent and child tasks,
  including multi-child fan-out entries with each child task's current Board
  state), every
  edge in and out with the other task's state, and the
  trace: each stage it entered, when it left, how long it took, the attempt
  and the outcome, with the edits recorded between them (`older edits not
  shown` when `TaskGet` reports `edits_truncated`). It is read with
  `TaskGet` (daemon/TASKS.md). A daemon without `TaskGet` is read with `Show`
  and `TaskHistory` instead: the trace is rebuilt from the transition log and
  the view says which fields that daemon cannot report; the TUI asks for
  `TaskGet` again a minute later.

## Writes

Writes need `--write`; without it the keys below only say the session is
read-only and nothing is sent. That is a client-side guard only: the daemon
authorizes each write by the operator credential the TUI holds, the action's
class and the authorizing message, exactly as it does a `fae` CLI write, and it
cannot tell a TUI write from a CLI write (there is no daemon-checked TUI
capability; FAE-API-032's read-only default applies to a supervised UI child,
which `fae-tui` is not). Each write first records an operator message that
authorizes it (`{"text": ..., "authorize": {"classes": ["task"]}}`, `runtime`
for an alert), then sends the RPC citing that message, through the same RPCs,
validation and audit as the `fae` CLI. If the message is refused, nothing else
is sent. The result, or the daemon's refusal text, is shown until dismissed,
and the views refresh.

| Key | Write | RPC |
|---|---|---|
| `c` | create a task: id, title, description, priority (10 or more), optional source, write class, declared files, test filter, test shape, workflow and pinned revision (an empty workflow with a positive revision pins that revision of the default workflow), target configuration and revision, task class, mode (`local` or `normal`; empty takes the instance default) with a normal task's disposition (`dry-run` or `commit-external`), and its edges (blocked by, blocks, relates to) | `TaskCreate` |
| `e` | edit the selected task: only the fields you change are sent, with the version the form was filled from (`expected_version`); a running task takes them from its next stage and its turn in flight keeps its text and the declared files, test filter and test shape it was dispatched under | `TaskUpdate` |
| `D` | delete the selected task: a reason and an optional successor, with the version the dialog was filled from; the dialog says whether it will be withdrawn (it never started) or stopped and retired (its turn in flight collected first), and whether its blocking dependents are held or move to the successor | `TaskDelete` |
| `+` | add an edge (from, to, blocking or relates); an edge to a dependent that already left Pending asks first and then sends the operator's acceptance in flight | `BacklogEdge` |
| `-` | remove one of the selected task's edges | `BacklogEdge` |
| `A` | acknowledge the selected alert (Attention view) | `AlertAcknowledge` |

A terminal task (Done, Cancelled, Retired) refuses edits and deletes before
anything is sent. A change to the declared files, test filter or test shape of
a task with a turn in flight is sent like any other edit: the daemon applies it
from the next dispatch, and the turn in flight keeps the values it was
dispatched under (shown from `TaskGet`'s `superseded_fields`; daemon/TASKS.md).
The create form checks the mode rules the `fae` CLI checks before daemon
contact: the mode is `local` or `normal`, a normal task states exactly one of
`dry-run` or `commit-external`, a `commit-external` task's Note must include
`"I approve live external effects"`, and a local task states neither
(FAE-MODE-001). `TaskCreate` likewise asks before an edge to a dependent that
already left Pending. When the daemon refuses an edge for want of that
acceptance anyway (a Pending task that ran before), the TUI offers to send it
again with the acceptance. Against a daemon that predates the live task-editing
RPCs the write is refused, and the result says so.

An edit or delete sends the task version its form was filled from. If the task
changed meanwhile (another edit, a stage change), the daemon refuses the write
as stale; the TUI shows the refusal and offers to read the task again and
reopen the dialog on its current values while keeping the field edits you typed
in the form. Nothing is retried on its own.

Declared files are repo-relative paths, the test filter is a `go test -run`
expression (a `/` splits subtest levels, each level compiled as its own Go
regular expression; see daemon/TASKS.md for how landing uses it), and the test
shape is one of `go-unit`, `go-integration`, `script` or `runner`, recorded
with the task, injected into agent briefs, and enforced by landing gates (for
example, `script` and `runner` shapes require an explicit `verify` command on
the target configuration).

## Tests

- `tui/dag`: layout depth, determinism whatever the edge order, every
  blocking edge pointing down over 400 tasks, termination on cycles and
  self-edges.
- `tui/client`: the socket client against a real `api.Server` on a Unix
  socket (framing, typed refusals, an unreachable daemon, unsafe and wrong
  credentials), paging, the `TaskGet` contract shape and near shapes (the
  `pending_delete` object, `edits_truncated`, the Board's `pending_delete`
  flag), the `TaskHistory` fallback and its one-minute memory, the write
  parameters (mode, disposition, class, pins, `expected_version`) and the
  stale-version and mode-completeness rules.
- `tui`: model tests that feed keys and messages to `Update` and read `View`
  for each view, navigation, per-view polling and its back-off, an
  unreachable daemon, failed reads, the small-terminal notice (down to 20×6),
  the pending-delete marker from the stored fields, write gating (no write
  without `--write`; the authorizing message always first), each write's
  dialog, parameters and result, the normal-mode create form and the
  stale-version reload offer. `TestPathsMatchTheDaemonLayout` checks the path
  resolution against `daemon.Paths`. `TestReadsAgainstARealDaemon` runs every
  read against a scripted daemon with Dolt, waits until Topology lists the
  molecule the first task runs in and checks the Molecules view shows it, and
  `TestWritesAgainstARealDaemon` drives the create (in stated local mode),
  edit, edge and delete dialogs against one, has a concurrent CLI-style edit
  make the TUI's edit stale and checks the refusal and the reload, and reads
  the result back with `TaskGet` (both skipped without
  `FAE_TEST_DOLT_BINARY`; the write test also skips on a daemon without
  `TaskCreate`). The real-daemon tests check the `pending_delete` fields only
  when the daemon reports them (they arrive with the wave-13 daemon); the
  fake-daemon tests assert them fully.
