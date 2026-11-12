# Crook / Fae engineering handoff

Updated 2026-11-01 after wave 13 (2026-10-31), which fixed the findings of the post-wave-12 re-audit (see Requirements audit: 206 of 284 Met, 4 Contradicted, before wave 13; among them a nested git path bypass and two unbounded reviewer reruns), and after the post-wave-13 re-audit (see Requirements audit: 206 of 284 Met, 6 Contradicted; two IDs up and two down, HOST-006 by wave 13's deferral cap and CTL-042 by cleanup's new plane write). Wave 12 (2026-10-30) applied the operator's RT-004 ruling, fixed the findings of the post-wave-11 re-audit (205 of 284 Met, 6 Contradicted, before wave 12; Fae usable only narrowly, for small Go tasks) and added live task editing (`fae task`) and the `fae-tui` terminal UI. Wave 11 (2026-10-29) fixed the findings of the post-wave-10 re-audit (209 of 284 Met, 3 Contradicted, before wave 11). Wave 10 (2026-10-28/29) fixed the findings of the post-wave-9 re-audit (208 of 284 Met, 4 Contradicted, before wave 10). Wave 9 (2026-10-28) fixed the findings of the post-wave-8 re-audit (207 of 284 Met, 3 Contradicted, before wave 9). Wave 8 (2026-10-26/27) fixed the findings of the post-wave-7 re-audit. In wave 7 (2026-10-26) Fae's shipped default workflow began developing Fae itself (see Completed in wave 7). Wave 6 (2026-10-26) fixed the findings of the post-wave-5 re-audit (see Requirements audit: 208 of 284 Met, 3 Contradicted, before wave 6). Wave 5 (2026-10-25) fixed the findings of the post-wave-4 re-audit under the operator's ruling to keep per-task limits. It followed wave 4 (2026-10-24, the regressions found by the post-wave-3 re-audit), wave 3 (2026-10-23/24, the gaps found by the post-wave-2 re-audit), wave 2 (2026-10-23), wave 1 (2026-10-22: requirements audit plus eight streams) and the Claude provider, host-admission and acceptance-ledger round.

## Identity and current state

The user refers to this project as **Crook**. The repository, requirements, Go module and executable are currently named **Fae** (`github.com/xleelz/fae`, `fae`); this round did not rename them. Development moved from macOS to **Ubuntu 24.04 (x86_64)**; the working directory is now `/home/lbethay/workplace/fae`. Earlier evidence records name macOS paths and Codex; they remain valid for the states they fingerprint.

This is a substantial working implementation, but **not complete compliance with REQUIREMENTS.md**. Later phases and operator-requested backup extensions coexist with unfinished earlier requirements. The numbered delivery phases must not be marked complete merely because a related package or test exists. The audit below distinguishes confirmed implementation gaps from acceptance criteria not yet demonstrated.

Rounds up to `e547c03` are committed. The Claude/host-admission round and waves 1 to 13 are all **uncommitted** in the working tree; no commit, push or production deployment was performed. Do not reset or clean the tree. Use the rounds' source manifests to identify the tested state.

## What has been built

| Area | Implemented behavior and primary locations |
|---|---|
| Durable control plane | Owned Dolt child, migrations (schema 11; 35 tables, and every record family has a production writer), immutable revisions/bindings, molecules bound at admission, relational task/transition/decision/artifact/provenance records written in each checkpoint's transaction with trigger-enforced immutability, topology and provenance-chain reads, transactional retry, audit/events, content-addressed payloads, backups and restart reconciliation: `controlplane/`, `daemon/`. |
| Workflow and execution | Pure domain/kernel rules, strict workflow/schema validation, the full primitive library (HumanDecision, FanOut/FanIn, SubWorkflow included), evidence-bound gates with replayable gate records, RED/freeze/compare floors with no-red classification, task classes, governed scope/RED requests, typed command placeholders, durable stage preparation/completion, independent reviews, budgets and scripted execution: `domain/`, `kernel/`, `workflow/`, `engine/`, `defaults/`. |
| Real local effects | Brokered host commands, isolated Git worktrees, measured tree evidence, reviewed-byte commit, changed-base reconciliation, bounded conflict remediation, serialized integration and own-work containment: `effects/`, `worktree/`, `daemon/`. |
| Provider runtime | Restricted **Claude Code** (default) and Codex trusted bridges; direct and **Herdr** multiplexer execution, persistent provider contexts, resident sessions, provider-neutral loops, health/failover, bounded startup retries, stall handling, process ownership and conservative crash recovery: `execution/`, `provider/`, `providerhealth/`, `processowner/`, `runtime/`. Herdr is the intended multiplexer, not tmux. |
| Self-hosting | Task scope derived from the task itself (admission declared files, the Spec, the RedReport) and shown on every brief as `task_scope`; failed runs' compiler and test output shown to rework turns as `engine_feedback`; a bridge that takes scope from the brief, lists the repository and accepts exact-replacement edits; opt-in accounting of nested commands; per-task commit messages; and the `scripts/self-host.py` harness: `daemon/live_scope.go`, `daemon/engine_feedback.go`, `execution/`, `provider/`, `commandshim/`, `docs/SELF_HOSTING.md`. |
| Resource safety | PSI-full host sampling, two-reading pressure hold and pre-lease I/O gate with back-off for pressure, panes and memory reservation, durable host-wide weighted leases (agent turns take none), dead-owner reclaim, dynamic pane/heavy/task limits (tasks in flight unbounded by default), pane census, idle-session shedding, refusal shims and the `heavy-op run` broker: `hostguard/`, `scheduler/`, `commandshim/`. |
| Operator/controller | Unix authenticated RPC with one exit-code contract, status/history/events, message threads and inbox, audited task controls including `migrate`, backlog dependencies, Prophet chats, workflow governance (get/list/retire/diff/simulate), target configs, cron and loop schedules, roll/maintenance/watchdog/independent drive restart, `gates self-test`, `gate run`, gated `land` with salvage/batch/then-roll: `api/`, `cmd/fae/`, `controller/`, `daemon/`. |
| Optional UI | Supervised, loopback-only, token-gated UI child with bounded restart and a listener census. The UI itself is the separate `fae-ui` binary (`cmd/fae-ui`, `ui/server`), and the terminal UI the separate `fae-tui` client (`cmd/fae-tui`, `tui/`, wave 12), never part of the core `fae` binary: `ui/`, `daemon/ui.go`. |
| Observability | Timestamped, component-tagged, rotating supervision log; one log line per park and escalation; shared alert path with `<root>/alerts.log` and acknowledgement; process census with leak alerts; Metrics/HostMetrics reads; lease waits as task events: `daemon/`, `controlplane/alerts.go`, `processowner/census.go`. |
| Configurable Normal mode | Runtime schema discovery, immutable adapter/target revisions, review lifecycles, ordered ticket outboxes, uncertain-effect query recovery, dynamic workspace/fleet provisioning and adapter retirement: `external/`, `daemon/`, `cmd/fae/`. The user chose configurable adapters; no particular vendor was selected. |
| Lifecycle cleanup | Reviewed census and retirement, archive-before-delete worktree cleanup, scratch creation identity, external canonical scratch backup, safe removal of exact duplicate scratch archives. Unique, incomplete and ambiguous recovery data remain conservative. |
| Backup lifecycle | Verified Dolt export/restore/fsck, historical coverage retention, remote archive receipt/content verification, query-only lost-ack recovery, durable retention/creation/replication schedules, now safe unpinning and quota controls. See `daemon/BACKUPS.md`. |

The most useful entry points are `README.md`, `IMPLEMENTATION.md`, `TESTING.md`, `provider/README.md`, `hostguard/README.md`, `verification/LEDGER.md`, `daemon/BACKUPS.md`, `daemon/SCRATCH.md`, `verification/README.md`, and `docs/SELF_HOSTING.md` (Fae developing Fae). Historical phase paragraphs contain superseded limitations; read the latest phase and source before treating one as a current gap.

## Completed in wave 13 (2026-10-31)

Wave 13 fixed the findings of re-audit 11 (see Requirements audit), including the nested git path bypass and the two unbounded reviewer reruns it found, and applied the coordinator's refinement of the operator's WF-093 decision (to be confirmed; see Decisions). Seven streams worked from a snapshot of the uncommitted tree (base `0b8d771`), and their branches (`wave13/*`) were merged in `wave13/integration`. Every behaviour change has a test that fails on the base.

| Stream | Delivered | Left open |
|---|---|---|
| Nested git (gn) | **A call-named path is checked as git creates it (MODE-002, EXT-001, VCS-002/-010):** every component is walked as the system takes it, a missing one as the directory git makes there, so `scratch/n1/../l/../PWN`, `format-patch -o scratch/nn/../l/../hooks`, `worktree add`, `clone`, `bundle create`, `log --output` and `config --file` through that shape are refused, with the operator's `.git` unchanged (end to end through the account-mode wrapper). **New hole found and closed:** a dangling link, which `open(O_CREAT)` follows, could create a file in the operator's `.git` (`log --output=dl`); a component that exists but can't be followed is now refused. **Census:** `commandshim/role.go`'s `&LocalRefusal` and accounted_git.go's two plain `errors.New` refusals are censused and fired by tests; verification/LEDGER.md lists the shapes the census can't see. | The check-then-exec race and programs a test repository's own configuration names remain. A test repository with a dangling link inside its `.git` is now refused. |
| Bounds and refunds (bd) | **Nothing loops (RT-004, HOST-006):** a turn's agent starts over all bindings are capped at 3 per declared binding, which no exhaustion, refusal, failover or healing resets; an agent turn's admission deferrals are capped at 60, then ProviderUnavailable naming the host condition *(re-audit 12: this fails a task the host keeps deferring, which HOST-006 forbids)*; a later review session whose admission is deferred waits up to 30 minutes, holding the earlier reviews, before giving them up. **Never-executed runs refunded:** the runtime's own `launch-refused` and a failed `Launching` write are refused before launch, with no provider health charged; the broker returns a typed `RefusedBeforeExec` for every failure before its command exists, so a test run, Freeze, Compare, integration or Merge that never started is refunded *(re-audit 12: Merge's steps before its write are not)* and recorded beside its intent; a commit refused before any write escalates refunded (`commit-refused-before-write`). Every refusal before launch has a paired log line (OBS-006); the bridges set `WaitDelay` (10 s) on the provider. Daemon-level test of a later session's deferral. | A later session's provider failover stays charged. Herdr and resident transport-local start failures still count toward the start bound. `vcs-refused` (a write attempted) stays charged. |
| WF-093 and topology (wf) | **The start refuses an unrunnable revision only while an active task is pinned to it** (coordinator's refinement of the operator's decision): unreadable revisions refuse as before; a revision that loads but fails `Parse` refuses the start while a non-terminal task is pinned to it, naming the tasks and the recovery; with no active task it is listed. **Recovery needs no older binary:** `fae serve --quarantine-unrunnable-revisions` (one-shot, never recorded for `run --start`; re-audit 12: a roll replays it) quarantines exactly those tasks, audited, so `fae migrate` (now accepted on such a quarantined task) or `fae quarantine retire` ends them; `quarantine release` refuses while the revision is unrunnable. **The check runs before `installDefault`,** so a refused start writes no revision or task row *(re-audit 12: the plane's schema migration still runs first)*. **Topology** renders a molecule whose graph can't be resolved with an `error` and its tasks by state, instead of failing the page; `include_retired` (`fae topology --include-retired`) is the OPS-011 opt-in. | `workflow.Load` stays decode-only; the engine's Gate dereference is reachable only through payload damage. Refused starts still write the audited alert rows. |
| Landing and redaction (lr) | **The test filter is read as `go test -run` reads it (CTL-030, CLI-014):** `^TestA$`, alternations and substrings land; `/` names subtests at that depth; the derived-scope RED id is quoted and anchored per level; zero matches hold. **The roll's overlap guard sees renames** (`--no-renames -z`, CTL-001). **The format gate examines type changes**, and reads only regular files (closes a symlink-follow); **found and fixed:** the own-hunk gate held every type change. **Redaction (SEC-003):** a 1 KiB test2json cut inside an escape unit (`%XX`, `\xNN`, `\uXXXX`) and HTML references in `-json` output are matched (0 leaks at every offset of the auditor's streams); the residue is listed concretely *(re-audit 12: a secret holding `"` still leaks at one cut offset, unlisted)*. The flake rerun's one residue (an intermittent `TestMain` exit in the flake's package) is documented. | The listed residue encodings. No Dolt-backed landing test for the quoted RED id. |
| Live task verbs (cr) | **An explicit priority below 10 is refused** on the CLI (exit 2, before dialling), in `--file` JSON and on the RPC; only an absent priority takes 10 (CLI-002). **Declared files, test filter and test shape apply from the next dispatch** (operator decision): the turn in flight keeps the values it was dispatched under (`superseded_fields`), for its live scope, brief and change check. **The pending delete is exposed:** TaskGet's `pending_delete` object (`waiting-for-turn`, `retiring`, `failed`) and Board's `pending_delete`. Supersession re-pointing writes an edge event on both tasks; a delete refusal's row precedes its line; a collected delete retires under the tasks-in-flight cap *(re-audit 12: only when the drive round reaches it first)*; TaskGet keeps the newest 1,000 edits with `edits_truncated`. | `test_shape` is now unified across admission, `HostCommand` execution, and landing receipt resolution (`landing-test-receipt-<id>`). |
| Terminal UI (tu) | **The "delete pending" marker comes from the stored fields,** not reason text. **Writes send `expected_version`;** a stale refusal offers a reload. **The create form makes a Normal-mode task** (mode, disposition, task class, revisions), with the CLI's checks. A terminal below 40×12 gets a notice instead of an overflowing frame. **Polling reads only what the open view shows** and backs off to 8× `--refresh` while nothing changes. README corrected (the client-side `--write` guard, the test shape, the real-daemon tests). *Integration:* the form sends structured edits of a task with a turn in flight (cr's change), and the Molecules view shows a molecule's unrunnable-revision error (wf's). | Polls instead of following the event stream; the Graph's BacklogShow is still one unpaged read per Graph poll. |
| Host and cleanup (ho) | **HOST-022:** a running command's defunct processes count against the bound after a two-census grace, and the alert names their parent (`defunct_parents`: pid, command, count, rounds, whether inside a running command). **CP-009:** `fae cleanup done` and `fae cleanup retired` page its candidates by their rows (no snapshot read for non-candidates), records a `worktree-cleaned` event on each task it cleans *(re-audit 12: a plane write, which CTL-042 forbids; skipped tasks still count toward `--limit`)* and skips them next time, and plans at most `--limit` tasks (default 200) instead of refusing above 100,000. **CP-006:** a plane restart records the outage before the consistency check, bounds the check per read (10 s, no overall bound), and sets re-validation even when the check fails. | The restart check's per-read budget isn't timed by a test. The self-host alerts' cause still needs a live run. |

**Integration added:**
- Merges: `cmd/fae/tasks.go`'s `--test-filter`/`--test-shape` help and daemon/BACKLOG.md's filter paragraph join lr's regular-expression rule with cr's test-shape wording.
- The TUI reconciled with cr and wf (above).
- `daemon/README.md`'s rollback note covers every stream's change.
- The first full run failed one test, `TestLiveHostCommandShimPolicyRefusals`, which still expected a refused host command to park UncertainDispatch; it and both ledger paths now expect bd's free refusals and the SameGate escalation. The second failed bd's `TestDaemonTestRunRefusedBeforeExecChargesNothing`, which counted a host-pressure deferral park as a failure; both refusal tests now wait through such parks.

## Completed in wave 12 (2026-10-30)

Wave 12 applied the operator's RT-004 ruling ("a run never executed shouldn't count towards the budget"), fixed the findings of re-audit 10 (see Requirements audit), and added two features the operator asked for: live create, read, update and delete of tasks and their edges, and a terminal UI. Seven streams worked from a snapshot of the uncommitted tree (base `b066ae2`), and their branches (`wave12/*`) were merged in `wave12/integration`. Every behaviour change has a test that fails on the base.

| Stream | Delivered | Left open |
|---|---|---|
| Never-executed runs (bg) | **Nothing that never ran is charged (RT-004 ruling, WF-074, HOST-006).** A later review session's start failure and a later reviewer's binding refusal are refunded like a first session's; the dispatch reruns every reviewer in fresh sessions (WF-033 isolation), charging no attempt. A later session's admission deferral is a typed deferral that parks for the host, not UncertainDispatch. A new class, `refused-before-launch`, covers every failure before the first session launches (a failed resolve or tree measurement, unreadable feedback, a missing route or brief, a scope refusal, the bridge's refusal before the provider) and a test run, Freeze, Compare, Merge or commit refused before it starts: it refunds the attempt, the visit and the test run, and the fourth in a row at a stage escalates SameGate whatever its detail. Every session's route and brief are checked before any launches. **The agent-start bound is per binding** (`startup-recovery:<stage>:<provider>/<model>`), so only the binding that failed three times is charged Unavailable. The route-exhaustion clock keeps running across all of these, so nothing loops *(re-audit 11: except a later session's admission deferral, and later reviewers' start failures once another task heals the binding; see Requirements audit; wave 13 bounds both)*. An `edits-not-applied` turn records the provider's ok health. | Earlier reviewers' turns are spent again on a rerun. A later session's provider failover and `edits-not-applied` stay charged (the provider ran). A non-deferral admission failure and a `Launching` error stay UncertainDispatch. No daemon-level test of a later session's deferral. |
| Bridge start bound and loop health (br) | **A provider that never acknowledged the prompt is not started, however it ended:** it wrote nothing to stdout and left no answer, and it exited (with a failure or 0), the bridge's `--timeout` stopped it, or the new start bound did. **The bridges enforce the 180 s acknowledgement bound** (`--start-timeout`, default 180 s, Appendix A's agent start): a silent provider is stopped with every process it started. `provider-failed` is kept for a provider that acknowledged and then failed or timed out. **Provider-loop turns record provider health (RT-009).** | Fae's own launch failures before any provider runs (session directory, config or brief writes, shims, pipe) still record MalformedOutput against the binding and park UncertainDispatch (documented in LIVE.md); a not-started loop turn still parks its schedule uncertain. |
| Literal paths and nested git (gp) | **Task file names reach git only as literal paths (SEC-005):** every broker git call sets `GIT_LITERAL_PATHSPECS=1` last, so a file named `:(exclude)*` can no longer make `Broker.Integrate` report a landing that didn't happen, or reconcile and conflict resolution judge the wrong files. **No nested call can switch a hook back on:** every call-set `hook.*` key except `enabled=false` is refused, from every config source. **`git worktree repair` with no paths** checks every worktree the repository lists. **New hole found and fixed:** a call-named path with `..` after a symbolic link (`scratch/l/../config`, `l` linked into the operator's `.git`) passed the text-cleaned check and let `bundle create`, `log --output`, `worktree add`, `init`, `clone` and `config --file` write into the operator's `.git`; paths are now resolved physically. **The census covers every refusal shape** (`&LocalRefusal`, the RED subtest refusal, `liveRedTurnChange`), and refuses a censused call written in any other shape. | The check-then-exec race (36 of 150 calls with a toggled link) and the `TestMain` framing spoof are documented only. `commandshim/role.go`'s `&LocalRefusal` isn't censused. |
| Landing and redaction (lr) | **Literal paths in the landing (SEC-005):** `landGit` sets `GIT_LITERAL_PATHSPECS=1`; the format gate filters git's changed names by suffix instead of a `*.go` pathspec. **The flake rerun hides nothing (CTL-030):** run 2 must report the packages run 1 finished and run every top-level test run 1 ran apart from the skipped flakes; a flake is counted as skipped only in its own package, and a namesake elsewhere that can't be shown to have passed holds the landing. **Redaction (SEC-003):** a secret cut by test2json's 1 KiB event split is matched across the events; shaped credentials (a long JSON Web Token) are read across a test's events; new forms: `%q`/`%U`/`%v` of runes, `%o`/`%b` of bytes, base32hex, UTF-16, kebab-case and space-aligned token fields, `Bearer` tokens, escaped line breaks. No false positive on 5.3 MB of Fae's own `go test -json` output. | `od -b`, `%#o`/`%O`/`%x` of non-ASCII runes, hex or base64 of UTF-16, a bare refresh token, `Authorization: token`, and a shaped token over 8 KiB aren't matched. A host-dependent `t.Skip` in run 2 now holds a landing. |
| Startup, host and cleanup (hs) | **WF-093 (operator decision): the start refuses an unloadable stored revision,** naming the workflow, revision and hash, the tasks pinned to it and the recovery; every revision is read (pages of 500, 10 s per read, no overall bound). *(Integration narrowed "unloadable"; see below.)* **CP-006:** the plane check runs before any start event is written, pages through every task, and compares `backlog_items.state`; `quarantine release` repairs the diverged rows. **scratchfs works from the directory handle it checked** (0 outside changes in 2,000 swap races). **Killed `gate run` scratch is swept at start** (an owner record proves it dead); `scratch-quarantine` has a status field and an alert. **A `-run` pattern that matches every test name is a whole suite**, judged against 16 probe names. **`fae cleanup done`** removes an integrated Done task's worktree and branch through the verified cleanup path. **Process-leak alerts** no longer count defunct processes inside a running brokered command. | The process-leak cause isn't proven (alerts.log has no per-process detail); a live self-host QA run will show it. Quarantined trees still need removing by hand. Plane-restart recovery still wraps the check in a 10 s context. |
| Live task editing (cr) | **New RPCs and `fae task create|update|delete|show`** (daemon/TASKS.md). Create joins one task to the live graph through admit's validation (refuses an existing id, names a cycle). Update changes only the fields given; edits to a running task apply from its next stage, the turn in flight keeps its text, and each edit records a `task-edited` event. Delete retires and keeps history: a never-dispatched task is withdrawn, a running one is escalated (its turn collected, never abandoned) and retired by the drive afterwards, durably across a restart; `superseded_by` re-points dependents. Get returns the task, its edges with prerequisite states, readiness and its stage trace with edits. Edge edits record events on both tasks. Nothing assumes Go. | Structured fields (declared files, test filter and shape) are refused while a turn is in flight. The Board doesn't flag a pending delete. A restart while a deleted task's turn is in flight parks UncertainDispatch first. TaskCreate reads the whole backlog graph. |
| Terminal UI (tu) | **`fae-tui`, a separate bubbletea binary** (`cmd/fae-tui`, `tui/`): Molecules (each molecule's stages in workflow order and the tasks at each), Graph (the task DAG laid out by dependency depth, every task's implementation state, edges and dead edges), Attention (escalated tasks with kind, reason and age, alerts, pending deletes), and a task view (description, edges, the trace of stages and edits). With `--write`: create, edit, delete, edge add and remove, alert acknowledgement, each after an authorizing message. Read-only by default. The core `fae` binary still links no UI. | Polls instead of following the event stream; edges aren't drawn as lines in the column layout; other controls (resume, defer) aren't in the TUI. `--write` is a client guard, not a security boundary. |

**Integration added:**
- Merges: LIVE.md and docs/SELF_HOSTING.md join br's "never acknowledged" rule with bg's no-charge rule; provider/README.md says a later reviewer's start failure charges no attempt either.
- `daemon/README.md`'s rollback note covers every stream's change.
- **What "unloadable" means (WF-093).** The first full run showed that hs's refusal used the admission rule (`workflow.Parse`). So an instance holding the old default, whose `main` brief keys a later rule rejects, refused to start, and the default upgrade built for it never ran. That covers every instance the committed binary created. The new `workflow.Load` decides whether this binary can read a stored revision: canonical JSON, no unknown or mistyped field, a known schema version. Only that refuses the start. A revision that loads but fails `Parse` is logged and listed under `startup_checks.revisions.invalid`, with the tasks pinned to it. Those tasks run on the document stored with them *(re-audit 11: scripted tasks do; a live task on the old default's brief keys is refused at its first turn, and a revision `Load` accepts can still be one the engine can't run; wave 13 refuses the start while an active task is pinned to such a revision)*, and an admission or migration that binds it is refused (`TestLoadAcceptsWhatOnlyLaterRulesRefuse`; `TestDefaultWorkflowUpgradeOnStoredPlane` asserts the listing).
- **Test fixes:**
  - a planted empty process record is dated an hour before the test process started, not an hour before now: the daemon package now runs longer than an hour, and recovery removes only a record older than the process;
  - `scripts/test.sh` gives the daemon package 150 minutes (wave 11 took 88 of 90);
  - `cmd/fae-tui` has a test (`TestFaeTUIExitCodes`), since the test counter refuses a package with none.
- Dependencies: `charm.land/bubbletea/v2` v2.1.0, `bubbles/v2` v2.2.1, `lipgloss/v2` v2.0.6 and `github.com/charmbracelet/x/ansi` v0.11.9 (each released at least two weeks before), which move `golang.org/x/sys` to v0.47.0 for the whole module.

## Completed in wave 11 (2026-10-29)

Wave 11 fixed the findings of re-audit 9 (see Requirements audit), most important for self-hosting first, and several self-hosting follow-ups. Eight streams worked from a snapshot of the uncommitted tree (base `52058d1`), and their branches (`wave11/*`) were merged in `wave11/integration`. Every behaviour change has a test that fails on the base.

| Stream | Delivered | Left open |
|---|---|---|
| RED evidence (rd) | **Only a test's own result, or go's own timeout naming it, is RED evidence (WF-073, WF-036).** A test cut off any other way (a `TestMain` goroutine's `os.Exit`, a test binary killed from outside, a crash outside the test, the broker's deadline) is not counted as executed and proves no expected failure; its run is a failed run the workflow routes (red-gate into rework, QA into remediation), with an excerpt saying why. Only a killed `go` process or a stream cut mid-line that the broker didn't cut leaves a run uncertain. **Every goroutine stack is left out of matching**, not only the final dump, so an `httptest` handler panic or `debug.PrintStack()` no longer hides the test's own later message. **Output over the run's byte bound is evidence** of what was kept, not an UncertainDispatch park; a RED proves its failure only if its own result is in the kept output. **The reason rule:** `expected error`, `want error` and `got error` are reasons again (`error` alone and `runtime error` stay refused), and bare positions (`go:9`, `o:12`) are refused; the protocol lists the exact refused words (`RedReasonTraceWords`). **A derived RED that calls `t.Run` on its own `*testing.T` is refused** when its test-first turn ends. | A test that prints go's exact alarm text itself counts as alarm-named; text a test `t.Log`s before an unrelated failure still matches; testify's labels aren't trace words; subtests started through a helper reach `red-gate` (which fails them). A `-a -x` trace cut at the bound can hide the compile line. |
| Nested git (gt) | **Re-audit 9's routes into the operator's repository are closed (MODE-002, EXT-001, VCS-002/-010).** A call may set only the `GIT_*` variables the policy knows (`GIT_TEST_FSMONITOR` and other unknown ones exit 77). Every nested call runs with hooks (`core.hooksPath=/dev/null` and every `hook.<event>.enabled=false`) and fsmonitor off, through `GIT_CONFIG_COUNT`, which outranks any config file, and with trace2 off unless the call sets it *(re-audit 10: a call's own `-c hook.<event>.enabled=true` outranked `GIT_CONFIG_COUNT` and passed the policy, so a hook could run again; wave 12 refuses it)*. Config files a call names (`GIT_CONFIG_GLOBAL`/`SYSTEM`) are checked like `-c`; a nested `git config` can't write program or include keys or rename a section; `init --template` is refused like `clone --template`. Files a call names for git to write (`--output`, `format-patch -o` and `format.outputDirectory`, `archive -o`, `bundle create` and others) must be where the call may write. A writable repository's git directories are walked for links out of the run's places (refs, worktrees, objects/pack) and its `extensions.refStorage`; `worktree remove`/`move`/`repair` targets are checked. **The call runs on the repository it checked:** the exec is pinned to canonical `-C`, `--git-dir` and `--work-tree` paths. Fae's commandshim, worktree, controller and effects suites passed behind an outer accounting wrapper. | The check-then-exec race is narrowed, not closed: a concurrent process can still swap paths or files between the check and git's start (closing it needs OS isolation). Programs a test repository's own configuration names (filters, diff drivers) still run, as do a protected repository's configured programs on a read and programs git finds on PATH. Submodule `.git` files and hard links aren't checked. |
| Routes (rt) | **The refused-route park no longer spins (RT-010):** `bounded` cuts on a character boundary, and the re-park check compares the reason as stored. **The 30-minute clock runs until a route launches**, across restarts and new route generations; Resume and restart with information start a new episode. **Resume of a route park needs a refusal in the current route generation**, as engine/README.md and daemon/TASK_CONTROLS.md say. **A failover gives the next route fresh starts.** **The route-exhaustion alert** uses the configured bound and the engine's clock, covers a ProviderUnavailable escalation, and restarts on Resume. The rollback note covers waves 10 → 9 and 11 → 10. | The execution diagnostic and the reviews-ran cause are still cut by bytes (event text only, never compared). |
| Provider failures (pv) | **Prophet charges a provider that never acknowledged (RT-009, RT-004):** it relaunches the route up to three times, then records it unavailable and fails over. **A later review session's start failure is a bounded outcome (RT-004):** a new class reruns every reviewer within the turn's start bound, with the attempt charged since earlier reviewers ran, instead of parking UncertainDispatch; exhaustion charges the failing session's binding. **Output, then failure (WF-034):** a provider that acknowledged the prompt and then failed on its own, or that the bridge's timeout stopped, is the bridge class `provider-failed`; with the provider observed, processes retired, a restricted profile and an unchanged tree it is a MalformedOutput stage retry. | A later session's admission deferral or stall still parks UncertainDispatch. The 180 s acknowledgement bound isn't enforced inside the bridge. Provider loops record no provider health. *(Wave 12: the deferral is a typed deferral, the bound is enforced, and loops record health.)* |
| Host and startup (hs) | **A read-only scratch tree no longer stops the daemon (HOST-020):** scratch is removed even when a test left read-only directories (new package `scratchfs`, which follows no link out of the tree); what can't be removed is logged, and the start sweep moves it to `<instance>/scratch-quarantine/` instead of refusing to start. Landing gate TMPDIRs, `gate run` scratch and scratch cleanup use the same removal. **A `-run` that matches every test is a whole suite (HOST-015):** empty, `.`, `.*`, `Test` and the other listed total patterns get one thread on the engine path and in nested calls. **cgo programs are pinned:** `CC`, `CXX`, `FC` and `PKG_CONFIG` name the run's own compilers, so a `gcc` a test puts first on PATH isn't run. **A controller- or operator-class `heavy-op run` waits until `--max-wait`**, not the role re-check bound. **`heavy-op run` and `gate run` lines carry time, instance and component (OBS-001).** | Quarantined trees are never removed automatically and raise no alert. Programs gcc itself finds (`as`, `ld`) aren't pinned. The CLI lines go to stderr only. `gate-runs/<id>` isn't swept at start. Nested calls still have no concurrency bound. |
| Redaction and landing (sc) | **Wrapped and encoded secrets (SEC-003):** a configured or environment secret wrapped across lines (raw or escaped newlines, indented) is redacted whole, across two `go test -json` events too; Go's `%v`, `%#v` and `% #x` byte forms, base32 and HTML character references are matched; xxd, `hexdump -C`, `od -t x1` and `hex.Dump` dumps have each line that holds a byte of a secret replaced. **Credentials by shape** (new package `secretshape`): an `sk-` key without its marker, OAuth token fields (`refresh_token`, `access_token`, `id_token`), and `sk-ant-` tokens and JSON Web Tokens wrapped across lines. **The provider's raw `events.jsonl` and `stderr.log` are redacted as written.** **A listed flake explains a nonzero exit only through a clean rerun (CTL-030):** the landing reruns the suite with the failing listed flakes skipped, and only a clean exit there explains the first. **A redaction count mismatch, or a redacted copy that doesn't parse, holds the landing as infrastructure.** | A Codex refresh token printed without its field name, `od -x` and plain `hexdump` layouts, dump lines under 8 bytes, base32 at an offset, a value split by log prefixes, and a shaped token's hex or base64 aren't matched (documented). |
| Dead ends, edits and startup (op) | **No Retire-only dead ends from widens (WF-078):** an approved path that is now refused (a stored pre-wave-9 nested `.git` widen) is dropped between dispatches with a `scope-dropped` event, and the resolver leaves it out; an approved RED file the bridge can't load is listed as unavailable. **The bridge writes a turn's edits all or nothing (ROB-012/-016, VCS-003):** every edit is checked and staged first, then renamed into place, with the handoff last; on any failure it restores the worktree and records `edits-not-applied`, a ValidationFailed stage retry that charges no provider. **Startup checks:** each removed empty process record gets a daemon line, an audit row and an event (ROB-014); `CheckConsistency` runs at start, refusing it only when the event clock is behind the plane's events (re-audit 10: or when the check fails or takes over 10 s) and quarantining a diverged task (CP-006); every stored workflow revision is parsed, and an unloadable one raises an audited alert naming the tasks pinned to it (WF-093) *(wave 12: one this binary cannot read refuses the start, by operator decision; one that only fails a later rule is listed)*; status shows `startup_checks`. **Protected paths (SEC-005):** a widen or a Spec can't put CI and forge files, hook managers, editor and agent configuration, `.gitmodules` or credential-like files into a model-chosen scope; a Spec that declares one has its change turns refused, naming it. Declared paths reach git as literal paths, not pathspecs. *(Re-audit 10: in snapshot staging and ls-tree only; integration and reconcile still passed task file names as pathspecs. Wave 12: everywhere, the landing included.)* | A bridge crash between renames still leaves part of a turn's edits (UncertainDispatch, then StaleBranchResidue). `fae cleanup scratch` still calls the silent `Recover`. No test plants a dead-owner and a live-owner instance lock. No daemon-level test shows a protected Spec's refused turn or an `edits-not-applied` turn. |
| CLI and census (cl) | **`workflow-put` lists every write issue** (WF-003), the Claude/`minimal` route with the others, as install does. `land --poll` has a 10 ms floor, and `controller.Watchdog.Run` refuses a period below it for every caller. **`workflow simulate` accepts what `workflow scenario` prints.** `daemon/READS.md` documents all 17 withheld reasons, checked against the code. **The ledger censuses the nested git policy and the derived-scope refusals rule by rule** (a new rule fails the census until a test fires it; re-audit 10: for `refuse(` and `liveScopeRefusal(` call shapes only; wave 12: every shipped shape), and maps route refusals and redaction as families. | `runtime-config` limits and the `--weight` of `gate run` and `heavy-op run` refuse an invalid value instead of falling back to the default (README.md explains why). |

**Integration added:**
- Merges: `provider/codex.go` joins sc's log flush with pv's failure typing (only the provider's own failure is typed; a log flush failure after a clean exit stays untyped); `runtime/direct.go` and `execution/executor.go` keep pv's `provider-failed` and op's `edits-not-applied` side by side.
- The ledger census follows the merged tree: gt's thirteen new nested git refusals, rd's reworded vacuous-reason refusal and op's protected-path refusal are claimed, and `TestLiveScopeRefusalCensus` fires the protected Spec path refusal now that an unsafe approved widen is left out rather than refused.
- `daemon/README.md`'s rollback note covers every stream's change.

## Completed in wave 10 (2026-10-28/29)

Wave 10 fixed the findings of re-audit 8 (see Requirements audit), most important for self-hosting first. Seven streams worked from a snapshot of the uncommitted tree (base `205aabf`), and their branches (`wave10/*`) were merged in `wave10/integration`. Every behaviour change has a test that fails on the base.

| Stream | Delivered | Left open |
|---|---|---|
| RED evidence (rd) | **A test that hangs, calls `os.Exit` or crashes is a failed run, not an UncertainDispatch park (WF-073, WF-036).** The broker gives a shaped go test run (`go-unit`, `go-integration`) go test's own `-timeout` at half the run's deadline, so go names the test that hung, and a test left without its own result counts as executed and failed when its package failed or the run timed out. Any brokered command killed at its own deadline returns evidence (`timed_out`, exit -1) instead of an error. **`test timed out` is now a reason that can match**, and the protocol gives Go's real nil-map message (`assignment to entry in nil map`). **The reason rule's residue:** numbers alone (`1`, `0x`, `42`), `error` and `runtime error` are refused; a number next to a word or a comparison still passes; path-only messages (`testdata/missing.json`) and words from the RED file's directories are accepted. The fragment is matched up to the goroutine dump, so trace frames and paths never match. The protocol says a RED is one top-level test without subtests. | Output over the run's byte bound and a kill from outside (OOM) still leave a run uncertain. *(Re-audit 9: only when the `go` process itself dies. A killed test binary, or a `TestMain` goroutine's `os.Exit`, now counts as the running test's failure, with whatever was printed as its reason (wave 11: it is no longer counted, and only a killed `go` process leaves a run uncertain); and matching stops at any goroutine header, `httptest`'s panic log included. See Requirements audit.)* A fragment the test `t.Log`s before an unrelated failure still matches (documented). A RED with subtests still can't pass `red-gate`. |
| Routes (rt) | **Every route refused parks (RT-010, Appendix B):** the task parks `all-routes-unavailable` with a reason naming each refusal (withheld as `route_refused`), resumes in place after the daemon restart that starts a new route generation or on `fae resume --task`, and escalates ProviderUnavailable only past the 30-minute route-exhaustion bound. A refusal gives the next route its full three starts, and Prophet's failover budget no longer pays for one. **A later reviewer's refusal refunds nothing (RT-004, WF-033):** only a dispatch in which no session ran is refunded; otherwise the attempt is charged and every reviewer runs again. The ProviderFailover branch had the same hole. **A provider that never acknowledged the prompt is a start failure (RT-004):** an absolute binary that can't start, or a CLI that exits on its own with no output, is retried three times, then parks ProviderUnavailable against the binding and fails over. | A start failure on a review dispatch's later session still parks UncertainDispatch *(wave 11: it reruns every reviewer within the start bound)*. Refusals are learned per task, not per instance. |
| Nested git (gt) | **Nested git loads no configuration or program the call chose (MODE-002, EXT-001).** *(Re-audit 9: not quite. `GIT_TEST_FSMONITOR`, trace2 and `format.outputDirectory` settings, and symlinks inside a test repository's `.git` still let a call change the operator's repository. See Requirements audit.)* On every repository, a test's own included: `-c`, `--config-env` and `GIT_CONFIG_COUNT` keys that name a program, a config file or a bundle URI are refused (built from git 2.55's key list), as are `GIT_SSH_COMMAND`-style variables, editors and pagers, `clone --template`, `ls-remote --exec`, unknown merge strategies and trace files outside the run's directories. Every passed call runs the real git with `GIT_CONFIG_GLOBAL=/dev/null` and `GIT_CONFIG_NOSYSTEM=1`, so `HOME` and `XDG_CONFIG_HOME` load nothing, and with `GIT_ALLOW_PROTOCOL=file`. **Only local repositories are reached (GEN-010):** a URL that isn't a path or `file://` is refused for clone, fetch, pull, ls-remote, archive and submodule add. **A Local task's nested push or remote change exits 78 in refuse mode too** (`InstallLocalDenials`). On the base, the policy's own `rev-parse` could append a git trace to the operator's `.git/config`. 7,411 nested git calls from Fae's own packages passed through the real broker; only a test's deliberate bad call was refused. | No `GIT_EXEC_PATH` mirror (measured; see decisions): programs git starts in a test's own repository still run the real git, now restricted to local repositories *(wave 11: no hook or fsmonitor runs in any nested call; programs a test's own configuration names still do; re-audit 10: a call's own `-c hook.<event>.enabled=true` switched a hook back on; wave 12 refuses it)*. A protected repository's own configured programs still run on a read. |
| Nested go (gr) | **The nested go environment is an allowlist (HOST-014/-020).** A call may change only the build target, module source and runtime settings (`GOOS`, `GOPROXY`, `GODEBUG` and the like); `CC`, `CXX`, `CGO_*FLAGS`, `PKG_CONFIG`, `GOCACHEPROG`, `GOVCS` and every other `GO*`/`CGO_*` key must keep the run's value, and `GOMAXPROCS` stays within the call's clamp. **Flags and targets:** `-cpu` values above the clamp, test-binary flags after `-args` that write files, and the linker's `-I`/`-r` are refused; `all`, `std`, `cmd`, `work` and `tool` are whole-suite targets. **The mirror:** each run's GOROOT mirror is sealed read-only and removed with the run; a role session's mirror goes with the session; a go outside any GOROOT gets a daemon line and a `fae doctor` warning. A daemon-path test shows four refusals through a live account-mode run. | Programs go finds on PATH by name (`gcc`, `pkg-config`) and the C toolchain's own variables aren't checked *(wave 11 pins cgo's programs to the run's own)*. Nested calls still have no concurrency bound. The seal is not containment. |
| Redaction and landing (sc) | **Every secret in every form (SEC-003):** configured and environment values get the hex, base64, percent, JSON and Go forms file credentials had; base64 is matched at all three offsets (`Authorization: Basic …`) and hex in either case. **Provider logins by shape:** `sk-ant-`, OpenAI keys and JSON Web Tokens, so a Codex login is redacted without the daemon reading it. Names that carry no secret (`GIT_AUTHOR_NAME`, `SSH_AUTH_SOCK`, the askpass variables) are no longer redacted. **A redaction can't turn a failure into a pass (CTL-030):** a package's or test's strongest outcome wins, the suite gate fails on an unexplained nonzero exit, and the landing parses the raw capture too and fails the gate if any count differs. Unreadable secrets stop a landing as infrastructure, not `compile`. | A token wrapped across lines leaks the part after the break; `xxd` layouts and a ChatGPT refresh token aren't matched. *(Wave 11: wrapped values, dumps and token fields are matched.)* |
| Startup and widens (op) | **An empty process record no longer blocks every start (CP-003):** an empty, regular record older than the process's start is removed; any other unreadable record is refused naming its path and the remedy. **Rolling back across wave 9 is documented** (`daemon/README.md`). **A stored Claude/`minimal` revision loads again (WF-093):** the rule is a write rule (`RouteEffortIssues`), applied by install and `workflow validate`; `workflow/GRAMMAR.md` documents route efforts. **Widen dead ends (WF-078):** an approved file the bridge can't load (a symlink, a non-regular file, over 1 MiB or past 4 MiB) is listed as unavailable instead of refusing every later turn; the bridge holds `approved_scope` to `DeclaredPathSafe`; a malformed request in an edit turn records the turn's edits, so restart works. | A stored pre-wave-9 nested `.git` widen still makes the resolver refuse every dispatch; retire and resubmit (documented). *(Wave 11 drops such a path with a `scope-dropped` event.)* |
| Smaller gaps (cl) | **Controller readings (HOST-005):** the reading that starts a hold and the one that lifts it are recorded on every controller path, `heavy-op run` included. The hook denies `py.test` and `pytest-3`. Offline `workflow` parses the common flag set. The ProviderHealth RPC falls back to 15 minutes with a `window_warning`. `land --poll` names itself, `watchdog --period` has a 10 ms floor, and refused and timed-out landing waits get their own audit outcomes. **The ledger** maps WF-078 and WF-082, censuses hook denials and bridge refusal classes, and names the refusal families outside the census. Stale comments corrected. | A role's `heavy-op run` under soft pressure can't lift within its re-check bound under the default policy (documented). *(Wave 11: only a role-class run has that bound.)* |

**Integration added:**
- The ledger maps the streams' new tests into its refusal families (`check:red-reason`, `check:bridge-refusal-classes`, `check:scope-widen`) and `check:validator-references`. It censuses `InstallLocalDenials` as `check:local-denials`, and counts `check:shim-accounted-go` as native-evidenced through `TestLiveAccountModeRefusesNestedGoOutsideThePolicy`.
- `workflow/GRAMMAR.md` now says that install and `workflow validate` apply only the route-effort rule, not all of `ValidateWrite`.
- Two new tests failed in the first full run because of the environment `scripts/test.sh` gives them. Under the script, the go wrapper is the coverage-built test binary, which prints a `GOCOVERDIR` warning when it exits on a refusal, so the tests' one-line refusal check saw two lines. Their check now ignores that warning (`TestAccountedGoEnvironmentIsAnAllowlist`, `TestAccountedGoClampsCPUTestBinaryFlagsAndPatterns`).

## Completed in wave 9 (2026-10-28)

Wave 9 fixed the findings of re-audit 7 (see Requirements audit), most important for self-hosting first. Seven streams worked from a snapshot of the uncommitted tree (base `9f758cd`), and their branches (`wave9/*`) were merged in `wave9/integration`. Every behaviour change has a test that fails on the base.

| Stream | Delivered | Left open |
|---|---|---|
| Go toolchain (gr) | **A Go test's own `go` and `gofmt` now reach the wrappers, in both modes (HOST-014/-015/-012).** go test puts `$GOROOT/bin` first on its tests' PATH, so the broker runs go with `GOROOT` set to a per-run mirror of the real GOROOT whose `bin/go` and `bin/gofmt` are the run's wrappers (`commandshim/goroot.go`), and with `GOTOOLCHAIN=local` and `GOENV=off`. Refuse mode refuses those calls (77); account mode records, checks and clamps them. Role sessions get a mirror too. **Account mode's go policy is an allowlist:** listed flags only, in the arguments and in `GOFLAGS`; every path flag inside the scope and off RAM-backed roots; compiler and linker flags that run programs refused; a foreign GOROOT, toolchain or Go env file refused. Tests use a real `go test` parent, not `/bin/sh`. | `CC`/`CGO_*` flags, test-binary arguments after `-args`, import-path targets and absolute go paths aren't checked. *(Wave 10 checks `CC`, `CGO_*` and the arguments after `-args`.)* `-run=` (empty) still counts as a filtered run. Nothing limits how many nested calls run at once. |
| Nested git (gt) | **Account mode's nested git can't change the operator's repository (MODE-002, EXT-001, VCS-002/-010).** *(Re-audit 9: it still can, through deliberate test code. See Requirements audit.)* The policy asks the real git which repository a call uses (`commandshim/accounted_git.go`). A call may change only a repository in the run's TMPDIR, nested in its worktree, or in `/tmp`, `/var/tmp` or `/dev/shm`, and never the run's own. Every other repository, the workspace and its task worktrees included, may only be read, with no configuration or program injected. Push and remote calls, mail, foreign-VCS, server and credential commands, aliases and unknown subcommands, and forms that have git run a program are refused everywhere. *(Re-audit 8: only the forms the policy names. On a test's own repository, `-c` keys that name a program pass, and a call can load configuration through `HOME` even on a protected repository. See Requirements audit. Wave 10 refuses those keys on every repository and runs git with no global or system configuration.)* Fae's own suite passed under the policy (about 6,800 nested git calls). | Programs git starts in a test's own repository (hooks, `core.sshCommand`) run the real git, and a read of a protected repository still runs that repository's configured programs. *(Wave 10: still, but nested git now reaches only local repositories.)* The wrappers remain evidence and guidance, not containment. |
| Process registry (pr) | **A concurrent scan can no longer read a process record partway (HOST-021, ROB-016).** `StartRole` publishes the complete record once; the in-place rewrite is gone. Where hard links are missing, records are placed with a rename that never replaces one. Recovery sweeps a crashed publisher's pending files, stopping its stopped launch gate under the usual identity proof. A malformed record of another session no longer fails this session's retirement proof. **Every pressure reading the controller records carries avg10, avg60 and both directions (HOST-005)**, and a `gate lifted` line records the reading that lifts a recorded wait. | Readings inside one wait slice are still unrecorded on the controller path. An empty record left by the old crash window still makes recovery refuse. *(Wave 10 records the readings that start and lift a hold, and clears such an empty record at start.)* |
| RED rule and widens (rd) | **The expected-failure rule is stated before the turn (WF-043):** the bridge protocol says what the fragment is matched against, what it must say and what is refused. **The rule no longer refuses real reasons:** *(Re-audit 8: a timeout can never produce RED evidence, path-only messages are still refused, and a lone digit now passes. See Requirements audit. Wave 10 fixed all three.)* numbers (`0 != 42`), message words that also occur in the test name, panic messages and `timed out` pass; generic trace text and a bare `got` or `want` are refused. A frozen report's reason isn't checked again. **One path rule for widens (WF-078):** `workflow.DeclaredPathSafe` refuses a widen into `.git` at any depth before the judge sees it, and also holds the instance `files`/`test_file` pins and the bridges' pinned and brief paths. **A frozen RED file survives a widen** that adds another test file the report lists. | A frozen RED file that itself came in by a widen, followed by a second widen of another listed test file, is still ambiguous (documented). The rule isn't repeated in the `task_scope` block. |
| Route failover (fo) | **A binding the bridge refuses locally fails over (RT-010, WF-082).** A provider the bridge doesn't serve, an empty model, bad bridge settings and the Claude preflight (effort, `CLAUDE_CONFIG_DIR`, credentials) are binding refusals. No provider health is charged, the attempt is refunded and the task moves to the next route. The binding is skipped until the daemon restarts or the operator resumes or restarts the task. When every route is refused, the task escalates ProviderUnavailable at once, naming each refusal. *(Re-audit 8: this contradicts RT-010's park; and a later reviewer's refusal also refunds reviewers that ran. Wave 10 parks, and charges a later reviewer's refusal.)* Workflow validation refuses effort `minimal` on Claude routes *(wave 10: on writes only, so stored revisions still load)*. **A ResolveUncertain redispatch starts with fresh per-turn counters.** | — |
| Redaction (sc) | **SEC-003's residuals:** nested-call arguments and directories are redacted before their marker is stored, and withheld if the secrets can't be read. Landing gate output is redacted before it is kept, and gate results are parsed from the redacted copy; a landing holds if the secrets can't be read. Percent-encoded and JSON or Go escaped forms (double-escaped too) are matched, `sk-ant-` shaped tokens are redacted without their value being known, and the daemon's own credential-named environment values join the secret set. | A provider login the daemon never reads (a `CLAUDE_CONFIG_DIR`) is redacted only where a token has the `sk-ant-` shape (a documented boundary). *(Re-audit 8: a Codex login isn't redacted at all, and configured and environment secrets are matched only raw. Wave 10 redacts OpenAI keys and JSON Web Tokens by shape and matches every secret in every form.)* Split or wrapped lines, decimal bytes and HTML entities aren't matched. |
| CLI and docs (cl) | `provider-health --window` gets the configured-value fallback (15 minutes), and `watchdog --period` ≤ 0 is a usage error. The hook also denies `go1.N`/`gotip`, `python -m pytest` and `rustup run` where a command starts, and its documented pass list is accurate and tested *(re-audit 8: it missed `py.test`, `pytest-3`, `gccgo` and `tinygo`; wave 10 denies the pytest names and lists the compilers)*. The ledger's `gobra:shim` no longer maps HOST-014. Offline `workflow` accepts `--instance=V`. README's landing-floor and spec-default claims and `daemon/BACKLOG.md`'s writer-lock claim are corrected. | `controller.Watchdog.Run` still turns a period ≤ 0 into 1 minute for callers other than the CLI. |

**Integration added:**
- A landing's new `gate lifted` line is audited as `lifted`, not `waiting`. *(Re-audit 8: untested; wave 10 adds `TestLandingAdmissionAuditsEachLineWithItsOutcome`.)*
- The UI child's log redactor kept only a 128-byte tail of an unterminated flood, shorter than an encoded secret. It now keeps one long enough for any form (`TestUIRedactWriterKeepsAnEncodedSecretAcrossAFlood`).
- Two new tests failed in the first full run because they depended on the environment `scripts/test.sh` gives them. `TestAccountedGitProtectsTheRunsRepository` ran its no-repository case under the script's repository `TMPDIR`, where git found the integration checkout; it now sets `GIT_CEILING_DIRECTORIES`. `TestBrokerAccountsGoAndGofmtStartedByARealGoTest` matched the end of a refusal that the coverage-built broker follows with a `GOCOVERDIR` warning; it now matches the message alone.

## Completed in wave 8 (2026-10-26/27)

Wave 8 fixed the findings of re-audit 6 (see Requirements audit), most important for self-hosting first. Six streams worked from a snapshot of the uncommitted tree (base `28492b5`), and their branches (`wave8/*`) were merged in `wave8/integration`. Every behaviour change has a test that fails on the base.

| Stream | Delivered | Left open |
|---|---|---|
| Malformed output (mo) | **A refused model answer no longer parks a task (WF-034).** A bridge that refuses its turn writes `bridge-refusal.json` in its session log directory and exits 65. The runtime types the refusal only when both signals agree, with no handoff and a fresh, valid record naming the turn. This works under Direct, the one-shot Herdr pane and resident workers (`runtime/direct.go`, `provider/refusal.go`). **A refused answer** (schema, edit shape, edit rules, unmatched replacements) is MalformedOutput: provider health is charged and the stage retries within its bound, then SameGate. **A turn refused before the provider ran** (task scope, brief, a failed `git ls-files`) retries as ValidationFailed and charges no provider (RT-004/-009). A one-shot pane-bind failure now defers as infrastructure. | A provider CLI that fails with no structured error still parks and is charged. *(Wave 10: one that never acknowledged the prompt is a start failure. Wave 11: one that failed after acknowledging is a bounded retry when nothing changed.)* |
| RED and scope (rd) | **A malformed derived RedReport is retried at test-first.** `checkRedReport` checks it when the turn completes, through a new `Executor.CheckOutput` hook. A refusal restores the turn's one RED file, retries test-first as ValidationFailed, and tells the retried turn why. test-first may launch to replace a broken report, so a restart recovers the task. **An expected failure must state a reason (WF-073):** it is matched without go test's framing lines, and a derived fragment made only of the test's name, file name or go test words is refused. **One path rule** (`DeclaredPathSafe`) refuses `.git` at any depth, in the CLI, at admission, in the resolver and in overrides. **A SubWorkflow child** gets its parent's resolved scope. A derived-scope task lands through `fae land` in a test. | An approved widen can break a frozen RedReport that named an out-of-scope test file (a restart recovers it). The bridge prompt doesn't describe the reason rule. |
| Nested commands (nx) | **Account mode is now HOST-014's accounted branch.** *(Re-audit 7: only for calls that reach a wrapper. A Go test's own `go` and `gofmt` reach none, in either mode, and nested git other than push and remote runs as called, including ref and config writes into the operator's repository. See Requirements audit. Wave 9 routes a test's `go` and `gofmt` through the wrappers and scopes nested git; see Completed in wave 9.)* Each nested call is recorded, then runs `fae heavy-op run --shim-policy` in an accounted mode. That mode applies the same policy as a top-level engine command, takes no lease and `exec`s the tool: nested `go` is clamped and confined to the worktree and run scratch, `gofmt` gets the formatter limits, and nested `git push`/`remote`/`send-pack`/`http-push` is refused (78 for a Local task, 77 otherwise). Account mode requires the daemon to be the `fae` binary. **Records are per run** (`nested-commands/<dispatch, command, start>`), with task and dispatch ids. **The hook** denies git and cargo dashed subcommands, Go toolchain internals by path, and `systemd-run`. | Nested calls are clamped one by one; nothing limits how many run at once beyond the run's parallelism and the task cap. A git alias for push (`git -c alias.p=push p`) is not contained. *(Wave 9: refused.)* |
| Counters and slots (rt) | **Per-turn bounds reset per turn** (RT-004, ROB-017). `clearTurnBounds` clears the stall, start and failover counters on any state change, any recorded failure, success and held collection, refunding no budget. **An unretired resident slot is skipped**, and becomes usable again once retirement is proven. Its deferral is withheld as `resident_unretired`. **A local pane-reservation failure defers** as infrastructure, with no provider charge. | — |
| CLI values (cl) | **No input lowers the 12 GiB landing floor:** *(Re-audit 7: overstated. A finite floor between the 2 GiB hard floor and 12 GiB still lowers it, as Appendix A allows.)* NaN, Inf, out-of-range and below-hard-floor values fall back with a warning, and the conversion is bounded. **The configured-value rule now covers every flag with a spec default:** *(Re-audit 7: all but `provider-health --window`; wave 9 added it.)* `census --gap`, `gate run`, `heavy-op run --max-wait`, `land --roll-max-wait`/`--overlap-wait`, `schedule create --expires-in`, and empty `--home`, `--instance`, `--adapter`, `--default-mode` and `--ui-bind`. Warnings carry OBS-001's time, instance and component. `version`, `workflow`, `doctor` and offline `gates self-test` run without `$HOME`. Per-command help exit codes. | `watchdog --period` ≤ 0 still silently becomes 1 min (no spec default). *(Wave 9: a usage error on the CLI.)* |
| Reads and redaction (st) | **The edge edit reads by key (CP-009):** `DependentEdges` reads by key, and `BlockingPath` is a bounded keyed walk between the edge's ends that names any cycle, so no whole-graph read remains. **`TicketStats`** is one aggregate over pending outbox rows; no payload is read. **Failed-run excerpts are redacted (SEC-003)** before they are stored or reach a brief. The redaction covers every credential the daemon knows (operator, plane, principals, UI, and credential-named live-config environment values), in raw, hex and base64 forms *(re-audit 8: hex and base64 only for file credentials; configured and environment values are matched raw; wave 10 matches every form)*. It runs again on read, for older excerpts. | Unreferenced blobs are still never collected (CP-012). Whole reads remain for backlog admission, `backlog show`, landing, `TaskHeads` and a supersession retirement's edge transfer. |

**Integration added:**
- A Prophet turn the bridge refused before the provider ran charges no binding, as the workflow path does. There is a new case in `TestProphetRouteFailoverSafety`.
- The self-hosting doc describes the bridge refusals and the accounted nested policy.
- Two new tests (`TestMultiplexerBeforeStartFailureDefers`, `TestOneShotPaneBindFailureDefersWithoutCharge`) failed in the first full run: under test.sh's repository `TMPDIR`, their Herdr socket path exceeded its bound, so the launch was refused before the step they check. They now use a short `/var/tmp` root, as the Herdr fixtures do.
- **Fae's own commit, taken back.** The self-host acceptance below ran a real Fae task, FAE-HOST-005, through the shipped default workflow. The task was that the heavy-op admission's pressure wait line also records the avg60 values and their directions. Fae's landed commit (`controller/admission.go`, and `TestHeavyAdmissionPressureWaitRecordsAvg60AndDirection` in `controller/admission_test.go`) was applied with `git am`, keeping Fae as the author. With the fix reverted, its test fails naming `io_full_avg60`, as the task required.
- **A process-record race (production, older than wave 8).** The full run after that take-back failed once. In `TestDaemonLandGatesAdvanceOperatorBranchAndSalvageRetires`, the second of two concurrent tasks parked UncertainDispatch at reviews with the cause `EOF`. A process record was created under its final `process-*.json` name and only then written, so another session's retirement acknowledgement, scanning the shared registry, could read it empty. Records are now written under a pending name and linked into place (`processowner.publish`), which never replaces another record. `TestRegistryScanNeverReadsAnUnwrittenRecord` failed 6 of 6 runs without the fix. *(Re-audit 7: partial. `StartRole` still rewrites the published record in place with `O_TRUNC`, so a scan can still read it empty; see Requirements audit. Wave 9 publishes the complete record once.)*

## Completed in wave 7 (2026-10-26): Fae develops Fae

Goal of this wave: Fae's shipped default workflow can take real Fae tasks to Done and land them, with no instance-configuration edit or restart per task.

**Status: met.** A live instance on a clone of this repository ran a real Fae bug through the shipped default workflow. The instance had no pinned scope and used real Claude turns. The task reached Done on its first attempt, 879 s after admission. The landed commit is on the clone's `fae/integration`. The work is in [docs/SELF_HOSTING.md](docs/SELF_HOSTING.md); the evidence is in `verification/results/claude-wave7-selfhost-20261026/`.

Five streams worked from a snapshot of the uncommitted tree (base `4a3a912`), and their branches (`wave7/*`) were merged in `wave7/integration`. Every behaviour change has a test that fails on the base.

| Stream | Delivered | Left open |
|---|---|---|
| Task scope (sc) | **Each task's scope comes from the task itself** (`daemon/live_scope.go`). An instance whose live config pins no `files` derives each task's files: the admission's declared files, else the task's latest Spec `declared_files`, plus approved scope widens. Its RED identity comes from the latest RedReport: test id, the single in-scope `*_test.go` file and the expected failure. After a freeze, only a new RED loop changes it. Unsafe paths and missing RED identities are refused before launch (`execution.ScopeRefusal`, ValidationFailed). Pinned configs (`files`, `tasks[id]`) behave as before. **Every agent brief carries a `task_scope` block.** **An empty `commit_message`** builds each commit's message from the task. **`fae run --declared-file`** (repeatable). | A model's RedReport naming two test files escalates through ValidationFailed retries instead of looping back to test-first. *(Re-audit 6: worse, since after a restart the test-first turn is refused too, so the task can't recover. Wave 8: fixed; the report is checked when test-first completes and retried there.)* Sub-workflow children have no declared files, so derived mode refuses their change turns. *(Wave 8: fixed; a child gets its parent's scope.)* Script and runner RED shapes still need a pinned identity. |
| Engine feedback (fb) | **A rework turn sees why its runs failed.** A failed host-command run stores a bounded excerpt by reference (FAE-CP-012): build errors, failed tests' output and package summaries, with no `-x` trace (`effects/excerpt.go`). Each turn whose inputs include such a run gets an `engine_feedback` block, at most 16 KiB per run and 48 KiB in total (`execution/feedback.go`, `daemon/engine_feedback.go`). | — |
| Bridge (br) | **The bridge takes scope from the brief** when nothing is pinned, and a pin still wins (never a union). **Every turn gets a repository listing** (tracked paths from `git ls-files`, at most 5,000 paths and 256 KiB). **Exact-replacement edits** (`{path, replacements: [{old, new}]}`, each `old` unique) work alongside whole-file edits. Test-first may edit one in-scope `_test.go` file; implement may edit any in-scope file but the RED file. The prompt text explains the blocks and the RED rules. | A failed later write leaves earlier edits written (older hazard). *(Wave 11: the bridge writes a turn's edits all or nothing; a crash between renames still leaves part.)* |
| Nested commands (nx) | **Opt-in `nested_commands: account`** (FAE-HOST-014's accounted option): nested `go`/`git`/build tools in an engine run are recorded and run under the parent's lease, instead of refused. Each run's calls are kept durably (`nested-commands/<digest>` marker). The default stays `refuse`, unchanged. *(Re-audit 6: the record is keyed by output digest alone, so runs with identical output share one; and the mode skips the shim policy and clamps, so HOST-014 is Partial. Wave 8 fixed both: records are per run, and nested calls pass the shim policy.)* **`max_output_bytes`** sets the broker's output bound (1–64 MiB, default 4 MiB). Socket-path tests in `api`, `ui/server` and `cmd/fae` use short directories. | The accounting records are evidence, not containment (same user). |
| Self-host harness (sh) | **`scripts/self-host.py`**: setup (a fresh clone at a commit, a live config with no task scope, the default workflow with routes narrowed to Claude), start, submit (`--declared-file`), status, stop, land (`--fetch-into`), and acceptance (evidence JSON). It has a self-test with 23 cases, run by `test.sh`. **`docs/SELF_HOSTING.md`** covers operating it, what a task needs, escalations, and taking landed commits back. | — |

**Measured before the streams started.** The default suite vector, run through the real broker on a fresh clone, could not pass:
- the output (5.56 MB) exceeded the 4 MiB bound, so the evidence was refused;
- about 70 tests failed with exit 77 on the nested-command denial shims;
- two socket paths exceeded 108 bytes.

With nx's settings, the suite passes through the broker: 2,086 tests in 5m20s. Dolt-backed tests skip because the broker passes no `FAE_TEST_DOLT_BINARY`, so `scripts/test.sh` remains the gate before landed commits are taken.

**Integration and the live acceptance found and fixed:**
- **Bridge pin without a test file:** sc's block names the instance's RED file on every turn. A bridge pinned with `--files` but no `--test-file` then refused even intake (`TestDaemonClaudeProviderPath`). The mismatch now refuses only change turns when the pin names no test file.
- **Ownership records over 64 KiB (production bug):** the format stage's `gofmt -l .` expands to 899 absolute paths on Fae's own tree. The resulting 120 KB ownership record could not be read back: Retire failed, the dispatch parked UncertainDispatch, and startup recovery would have refused the registry. Records now keep a bounded command, and legacy records up to 4 MiB are read.
- **Zombie check in resident tests:** the daemon reaps adopted daemonized Herdr servers after a 5 s grace (FAE-HOST-021/-023). Under a live daemon, a retired Herdr process is therefore briefly a zombie, and four resident tests that checked `kill(pid, 0)` once failed QA deterministically. They now wait up to 5 s and count a zombie as gone. The coder in that run correctly reported the failure as outside its scope.
- **Harness evidence:** daemon events with a bare (non-object) payload broke evidence collection.
- **Dolt metrics race in one test (the first full run's only failure):** a Dolt server that exits starts a detached `dolt send-metrics`, which writes under `DOLT_ROOT_PATH`. `TestEndpointUnrecordedOwnServerRefusedWithRemedy` was the only Dolt fixture without `DOLT_DISABLE_EVENT_FLUSH=1`, so its temp-directory cleanup could fail under host pressure. The daemon already sets the flag in production (`daemon/plane.go`).
- **Merge resolutions:** `daemon/live_check.go` (nx's bound checks and sc's scope validation), and the brief block order (`task_scope` before `engine_feedback`).

**Fae's own commit, taken back.** The acceptance task's landed commit (`411ead4` on the clone, authored by Fae) was applied to `wave7/integration` with `git am`. It fixes the next-work item "NaN accepted by `controller.Watchdog.bounds` from non-CLI callers" (`controller/watchdog.go`) and adds `TestWatchdogBoundsRefusesNonFiniteThreshold` (renamed `TestWatchdogBoundsFallsBackOnNonFiniteThreshold` in wave 8, since the bound falls back rather than refuses). It passed the full suite below.

## Completed in wave 6 (2026-10-26)

Five streams fixed the findings of the post-wave-5 re-audit. They worked from a snapshot of the uncommitted tree (base `07f13d4`), each in its own short-path worktree, and their branches (`wave6/*`) were merged in `wave6/integration`. Every fix has a test that fails on the base.

| Stream | Delivered | Left open |
|---|---|---|
| Engine budgets (en) | **The stall bound ends with every turn (RT-005):** any state change clears the stall counter (`engine/engine.go` `enter`), and so does collecting a held dispatch. A StallExhausted or AttemptCap task restarted or resumed gets its 2 retries on its next stalled turn. **Carried budgets are visible (OPS-010):** TestRunCap, loop-exhaustion and progress-streak escalations name the count, the limit and that the count carries for the task's life. Resume and restart-with-information reply with `carried` (used and limit for attempts, test runs, every loop, streaks and requests), and run-wide resume adds it per task. **Uncertain resolutions are validated before saving,** and refused while another checkpoint is open (`daemon/uncertain.go`). The refused-resume test now checks the returned task. | The failover (`failover:<stage>`) and startup-retry (`startup-recovery:<stage>`) per-turn counters still survive an escalation, like the stall counter did. *(Wave 8: fixed.)* |
| Writer lock (lk) | **Admit's lost-acknowledgement window (WF-005):** admission-row writes are booked under the lock. An id whose write returned an error is re-read from the plane under the lock, so a same-id Admit is refused before any fork. **Off the lock (ROB-021):** run-wide resume's `ActiveBacklog`/`LatestRunID` and an edge edit's endpoint reads, each re-checked under the lock. **Per-key exclusion (ROB-020):** the daemon-wide `keyedMu` is replaced by per-key turns. **`daemon/README.md`** lists, verified, everything still holding `d.mu` across I/O. | An edge edit's graph read still holds the lock (retirement writes edges). A task committed but left uncached by a lost acknowledgement isn't driven until restart (older gap). |
| Pool reads and Prophet (rd) | **Census and ReviewTargetHealth no longer wait on `d.mu` (ROB-020):** they read a task copy published with each status snapshot. **The global Prophet is scoped (CP-009):** drive tasks from the published copy; tasks finished before start from the summary or `TerminalRowsAfter`; at most 1,000 entries and 512 KiB, with `tasks_total`/`tasks_truncated`. It no longer fails past 1,000 tasks. **Prophet session start** reserves its slot under the lock without I/O; its reads and writes, and route observations, run off the lock. | Prophet replies still hold the lock (the slot release and any enacted control). `ListJobs` has only test callers. |
| Runtime, idle and withheld reasons (rt) | **A failed slot reconciliation** sets the slot aside as unretired for maintenance to retry, and defers the turn without charging the provider. **`status` shows `scheduler.unretired_transports`.** `runtime/IDLE_SESSIONS.md` states the true bound: one pane per unproven retirement, all within the pane limit. **An idle refusal's cause change** carries the previous cause's suppressed count. **Withheld reasons:** a mid-round pending checkpoint withholds the undecided tasks as `checkpoint_unsettled`; a resident pane-limit refusal counts as `pane_limit`; `plane_unconfirmed` has a test. | A closed-but-registered slot can still be re-picked. Non-refusal pane-reservation errors still count as startup retries. *(Wave 8: both fixed.)* |
| CLI, hook and shims (cx) | **Hook literal bypasses closed (HOST-011):** `env` by basename with every PATH-clearing option form, `exec -c/-l/-a`, `command -p` in clusters, login, interactive and rc-file shells, `BASH_ENV`, brace expansion, and `.`/`source` in command position. **The hook exits 2** when its decision can't be delivered (SIGPIPE, closed or `/dev/null` stdout). **Out-of-range values:** empty, out-of-range or non-finite configurable flag values fall back to the default with a warning (REQUIREMENTS.md conventions). A malformed token stays a usage error. **CLI drift:** controller verbs take only their own flags. Missing backlog operands exit 2. Help and an explicit `--home` need no `$HOME`. A cited conformance proof that skips while Dolt is configured now fails. | The hook stays lexical: unevaluable expansions, interpreters and `su`/`sudo`/`ssh` pass. `controller.Watchdog.bounds` still accepts NaN from non-CLI callers. *(Wave 7: fixed.)* |

Integration found and fixed:
- **Test hooks:** `TestDriveSurvivesRandomPerTaskFaults` and `TestAdmitDoesNotWaitForTheRound` counted every fault point for their task id as a drive save. lk's new `admit-acknowledgement` point therefore injected the save fault into Admit. Both hooks now filter on the `save` point.
- **Zombie check:** `TestShutdownLeavesNoInventoryProcessAndNoZombie` (processowner, untouched by wave 6) checked for zombies once, immediately after `KillAdopted`. Under the full suite's load, a child still being collected showed as defunct. It now uses the bounded `awaitNoDefunct`, as its sibling test does, which still fails on a zombie nobody reaps.
- **`daemon/README.md`:** the Prophet and withheld-reason lines were reconciled after rd's and rt's changes.

## Completed in wave 5 (2026-10-25)

Five streams fixed the findings of the post-wave-4 re-audit, from a snapshot of the uncommitted tree (base `450c093`), each in its own short-path worktree; their branches (`wave5/*`) were merged in `wave5/integration`. Every fix has a test that fails on the base. Operator ruling for WF-074: **keep per-task limits**.

| Stream | Delivered | Left open |
|---|---|---|
| Engine limits and carried tasks (en) | **Per-task limits (WF-074):** an AttemptCap Resume re-grants attempts against the raised limit and re-enters the first stage, refunding nothing: test runs, scope-widen and RED-rework counts, Loop iteration counts and loop-progress streaks carry over for the life of the task, and the audited cause records them (`restart from <start> refunds nothing; carries counters {…}`). Restart-with-information records the same. TestRunCap is checked before AttemptCap at one boundary. The re-audit's probe scenarios now keep every task within 8 test runs and end TestRunCap. **Carried tasks (WF-021):** leaving Pending by any edge drops `carry_entry`; `ApplyControl` and every operator checkpoint validate their result before saving, so an escalated carried task retires without quarantine. **Stall retries per stalled turn (RT-005):** the durable counter resets when a turn ends (success or recorded failure). **Generation pinning (WF-080):** every shipped generation's hash is a constant in the test, so re-recording one in place fails. | A raised task whose other budgets are spent can't finish: one pass of the shipped default spends about 4 test runs and one spec-loop iteration. Resume doesn't refuse such a task up front; it escalates TestRunCap or LoopCap when it gets there. |
| Reads after restart (rd) | **Schema 11:** `tasks.execution_kind` and `tasks.mode`, backfilled for finished tasks. Board and `status --tasks` rows for tasks finished before a restart are built from task-row columns (`Store.TerminalRowsAfter`, one range per state with every Board filter pushed down); `--in-flight` skips the plane. A finished-task summary is built lazily on first need, outside the read pool, so Board and Status are then served from memory even with the pool saturated (ROB-020). `status --tasks` refuses an oversized answer by count before reading. Status holds `d.childMu` only to copy the child's pid and port. **Topology is a cached snapshot** with `snapshot_at`/`snapshot_age_ms`, refreshed on demand (stale after 5 s, refreshed before answering after 10 s), outside the read pool (API-001, ROB-020). | The summary holds one Board row per finished task for the daemon's lifetime (decision). Before it is built, a filtered page scans state ranges inside Dolt (no index on mode or priority). Whole-table reads remain in `Backlog`, `TaskHeads` and the global Prophet. |
| Writer lock (lk) | A mutating RPC's checking plane reads now run before it takes `d.mu` and are re-checked under it: Admit's existing-task read, run-wide `resume`, and `backlog edge`'s prerequisite reads. Dispatch's `TaskDependencies` read runs off the lock under the checkpoint discipline (ROB-021). Withheld reasons for every skipped task: `edge_unread`, `release_changed`, and `plane_unconfirmed`/`checkpoint_unsettled`/`draining`/`round_read` for rounds that end early (SCH-010). `daemon/README.md` lists, verified, everything that still holds `d.mu` across I/O. | Drive-exit and re-arm audits, startup-exhaustion observations, recovery quarantines, provider observations, Prophet replies and cron admission's fork still hold the lock. |
| CLI, hook and shims (cx) | Each conformance proof is bound to the row's verb and exit code: a cited test must declare `proves(t, row, code)` and one of its runs must exit that code, so the wave-4 false hook proof is refused; `maintenance stop|start` have a real CLI proof (CLI-001). `heavy-op hook` exits 2, never 1, on a missing `$HOME` or an unwritten decision. The hook reads words with shell quoting and escapes removed (`g''o`, `g\o`, `$'\x67o'`). Local `git -C dir push` and other global-option forms exit 78 (MODE-002). Backlog actions take only their own flags; `--accept-in-flight` is documented. | The hook stays lexical (`/usr/bin/$x` passes). The engine broker's git refusal stays 77 (it can't tell Local from Normal). *(Re-audit 8: the rationale is stale, as the broker has known the mode since wave 8. Wave 10 gives a Local task's nested push 78 in refuse mode too.)* |
| Runtime and idle (rt) | Concurrent turns of a shared role get distinct resident identities (slot 0 keeps the old identity), so a cap-2 workflow-global role runs two turns and a third waits on the role lease (SCH-005/-009). An unproven turn retirement ends and releases the turn; an unretired transport is refused for reuse and retried every idle round. The idle registry no longer holds its lock across a retirement. Idle maintenance reads the in-flight count under the lock just before its round, writes a starvation row only with its escalation, and records census and registry refusals as a row, event and paired log line once per episode (every 10 min while it lasts) (SCH-013, OBS-006). A test covers the 60 s idle own-tree census. | A resident pane-reservation refusal is still labelled `host_guard` when the cause is the pane limit. |

Integration found and fixed:
- One build break: lk's `storedRow` type and rd's `storedRow` function collided; rd's is now `finishedBoardRow`.
- The first full run had two failures. `TestDaemonIdleMaintenanceAuditAndStarvation` failed intermittently because rt's idle round now retakes the writer lock after its census to count turns in flight. The drive's own round, begun at start against the real census, slipped between the test's rounds while the test's census ran unlocked, and reset the saturated-round count. Only the drive runs these rounds in production. The four idle tests now stop the drive first, and the refusal test counts from after the stop. `TestStartAcceptsOnlyLaunchedPinnedProcess` (controller, untouched by wave 5) gave its race-built child 200 ms to start; under the full suite's load it timed out, so the bound is now 5 s.
- `IMPLEMENTATION.md`'s phase table still listed record-family writers, startup loading every snapshot and the default's missing request judge and no-red route as gaps; corrected (schema 11, a 42-run parity corpus).

## Completed in wave 4 (2026-10-24)

Five streams fixed the regressions verified by the post-wave-3 re-audit, from a snapshot of the uncommitted tree (base `e714235`), each in its own short-path worktree; their branches (`wave4/*`) were merged in `wave4/integration`. Every fix has a test that fails on the base.

| Stream | Delivered | Left open |
|---|---|---|
| Terminal reads (rd) | Board and `status --tasks` page tasks that finished before a restart from the plane (`Store.TerminalJobsAfter`, a range read on the state index, in the read pool) and merge them with the snapshot, so a restart changes no read; nothing more is loaded at startup and the 100k topology budget holds (API-001, OPS-011). `backlog edge` readiness and `resume` look tasks up through `storedJob`; resume of a terminal task or all-terminal run is refused as terminal again (CLI-013, CLI-015). A sweep of every `d.jobs` reader found one more: the runtime pane-floor check counted finished tasks' workflows, so a pane limit was refused before a restart but not after. `TestRestartLeavesTerminalReadsUnchanged` compares 24 read results across a restart (Board views paged two ways, four Status views, eight edge results and two resumes). | After a restart, Board and `status --tasks` can answer `read-busy`/`deadline-exceeded` when the read pool is saturated. |
| Engine and default (en) | Resume of an AttemptCap escalation starts a **new pass** from the first stage: loop counts, the test-run count, loop-progress streaks and request caps reset, the prior values go into the audited decision rationale, and attempts continue against the raised limit (WF-074, OPS-010, CLI-041; WF-021 unchanged). The probe scenario now reaches Done; every other escalation kind is still refused. `defaults.Generation` is **3**; `shipped-generation.json` keeps every generation's hash (generation 2's content stored as a fixture), so changing `Shipped()` without a new generation fails. A Dolt test upgrades a generation-2 instance on start while its in-flight task finishes on its pinned revision (WF-080, WF-005). | Restart-with-information of an AttemptCap escalation keeps every counter. |
| Forks and admission (vc) | `forkAtRelease` decides under `d.mu`, forks without it (checkpoint discipline), then re-checks the task; an Admit during a slowed refork returns in about 90 ms. `worktree.Manager.Refork` moves the tree outside the manager lock (ROB-021). Admit refuses an existing task id (memory, quarantine, plane) before any fork (WF-005). **Carry-forward follows VCS-001:** a carried task forks from the current tip, waits Pending until released, re-forks at release, then enters its carried stage through the durable `carry_entry`; no carried evidence is base-specific (WF-086). | `dispatchTask`'s `TaskDependencies` read still runs under `d.mu` (ROB-020). |
| Runtime and idle (rt) | `ResidentMultiplexer` holds its lock only to reserve and publish; the census, removed-slot retire, pane reservation and Herdr start run outside it, one start per identity, and Close retires overtaken launches (SCH-009, RT-011/-012). The status snapshot is refreshed on demand instead of by a 5 s timer, and the idle leak census runs once a minute while nothing changes. Idle CPU measured at 13–14 ms per 30 s and **324 / 353 ms per 10 minutes** (spec: under 1 s); the idle test asserts the spec rate, and `FAE_TEST_IDLE_FULL=1` runs the full 10 minutes (ARC-002). Idle-session maintenance runs off the writer lock; `RecordIdleAction` now accepts `idle-session-reconciled`, which every RT-012 reconciliation had turned into a drive-step fault. | Other housekeeping steps still hold `d.mu` across I/O (listed in `daemon/README.md`). |
| CLI and payloads (cx) | Conformance rows must account for every exit code 0–3 with a test that exists (or a stated reason); the false `heavy-op hook` proof is gone: the hook exits 0, or 2 to block (fail closed), never 1. `watchdog --json` emits only JSON, and the heartbeat carries time, instance and component. Explicit `--help` exits 0. The port-clash remedy names `serve --dolt-port`, and a test checks every serve flag named in a message. Bad content in a named file exits 1; exit 2 is reserved for the invocation (CLI-001, CLI-010, CTL-010). `serve --inline-limit` (4 KiB–64 KiB, default 64 KiB) and lease-pool payloads spill to blobs; **migration 10** moves oversized pools and bounds `resource_pools.payload` (CP-012). | `ticket_emissions.payload` is unbounded; spilled blobs are never collected. |

Integration found and fixed:
- One merge conflict: rd moved `statusSummary` to `daemon/reads.go` while rt changed it to read through the on-demand snapshot keeper; Status now reads through the keeper without waiting.
- No cross-stream test failures; the first full suite run passed.

## Completed in wave 3 (2026-10-23/24)

Nine streams worked the post-wave-2 re-audit's gaps in parallel from a snapshot of the uncommitted tree (base `121a8a9`), each in its own short-path worktree; their branches (`wave3/*`) were merged and reconciled in `wave3/integration`.

| Stream | Delivered | Left open |
|---|---|---|
| Regressions and CI (rg) | Idle daemons keep a fresh status snapshot (worker re-snapshots in-memory state every 5 s when nothing published; no plane reads; idle round trips still 0) so Board/Schedule/Controller answer while idle (ROB-020). One `controllerClient` constructor: roll, maintenance and `land --then-roll` all carry the watchdog identity (CTL-001). **The operator UI ships as a separate `fae-ui` binary** (`cmd/fae-ui`, `ui/server`); the daemon keeps generic UI-child supervision, finds `fae-ui` beside its own executable or via `--ui-bin`, and warns if it is missing; `go list -deps ./cmd/fae` excludes the UI server (test + test.sh census) (GEN-010 non-goal 6, API-033). CI installs Dolt 2.5.1 and Herdr 0.9.5 pinned by SHA-256, sets `FAE_TEST_DOLT_BINARY`, and fails on any test skipped for a missing Dolt/Herdr/lsof (VER-002/-005). Worker starts are refused once Close begins (`beginWorker`), including Land and Prophet RPC workers. The maintenance gate reads the committed plane marker cached in memory, not the file mirror (GEN-011). | CI workflow never run on GitHub. |
| Control plane (cp) | All 23 tables *(then; the plane has 35 now)* have a production writer, projected in the same transaction as their single authority (leases, ticket_outbox, external_effects, sessions, code_reviews, schedules); `engine_runs` is itself authoritative and records the environment delta by name only (CP-011, EXT-006, ROB-030; **migration 9**). Startup loads only non-terminal jobs; ~15 call sites read terminal tasks by key; drive rounds read only the active backlog; 100k topology budget test always runs (CP-009). Plane writes are named Dolt commits ("drive round N checkpoint <task>"); request-key and migration-table rows ride in the next one (CP-008). Hung-Dolt detection by ping (two failures kill), outage records, post-restart consistency check with quarantine (CP-006). Dolt stop wait 30 s configurable (`serve --dolt-stop-wait`), terminate-then-kill orphan reaping with audit (CP-002/-003). Oversized payloads spill to content-addressed blobs (CP-012). Port-clash refusals name our own stray Dolt vs a foreign process (CP-004). | Drive commits are per checkpoint, not per round. Board after restart shows active + finished-since-start. |
| VCS (vc) | Task worktrees fork from the current integration tip, base recorded; released dependents re-fork onto the tip; a branch without its directory is adopted (VCS-001/-008). Integration is a serialised queue, so a busy mainline no longer exhausts the retry loop. A clean patch-identical reconcile re-runs QA only; approvals carry under `CanCarryReview` (VCS-005a; no conflict with WF-071). `contained_by` recorded (VCS-007). Every integrate/commit failure gets a disposition; durable stuck-stage clock (VCS-009). New tests for VCS-002/-003/-011 and two real tasks editing the same lines. | — |
| Robustness and kernel (rb) | `workflow.CanTransition` is the one legality function and `engine.enter` the only state assignment; Resume of an AttemptCap escalation restarts at the first stage (WF-008/-021). Drive read faults skip the round with an alert instead of ending the generation; auto re-arm backs off 1 s→60 s within 3/hour (ROB-001). SameGate = same stage, same reason, escalates on the 4th (ROB-002/-017). Crash-interrupted HostCommand/Freeze/Compare re-run automatically when provably safe; stale protected evidence re-validates via the Reverify route (ROB-013). Rows without a disposition fail closed and quarantine (ROB-042). Acceptance edits apply at the next turn with a digest (ROB-040). Checkpoint I/O runs without the writer lock; Admit < 1 s (ROB-021). Role cap −1 is unbounded end to end (WF-040). | The default `commit` stage has no Reverify route, so a stale verdict there still escalates ResumeEvidence. `daemon/live.go` still names default-content stages. |
| Runtime and scheduler (rt) | Task-bound resident transports retire at collection; removed panes are detected and relaunched; relaunch resumes the saved handoff (RT-011/-012, SCH-014). Proven stalls retry by default (RT-005); safety rests on `CanRetryStall` (retirement, unchanged tree, first session), the profile check being always true. Each round collects every delivered completion first (SCH-009). Role waiters keep their queue place (SCH-008). Withheld counts by every reason, trace every round (SCH-010). Admission time for every task (SCH-003). Heavy-pool waiting warning is also an event (SCH-011.4). | Concurrent FanOut branches: design only (WF-030). |
| Workflow content (wf) | Shipped default governs requests (independent `request-judge` run by the oracle role, widen/rework routes, caps 3) and has a no-red route; briefs consistent (WF-078/-083/-073). Parity corpus 42 runs, 19 of 20 Appendix B kinds (CoordinatorVeto documented unreachable) (WF-094). Typed remote refusal → ExternalEffectRefused; ResumeEvidence at load; IntegrationConflict for non-landing integration. Simulator drives the real engine offline; 26 corpus runs replay identically (WF-091). Kernel contract last in the prompt with the absolute handoff path (WF-043). Profile shim/effect grants and `target_requirements` (WF-042/-001). Schema 2 requires Gate `name` / Escalate `kind`+`reason` (WF-030). Follow-up records (WF-084), token RED identity for non-Go (WF-076), security citations in reviews (SEC-007, partial: the gate matches counts, not domains). | Policy hooks (spec not concrete). `ValidateWrite` (refuse new schema-1 writes) not wired: fixtures, `examples/workflow.json` and dogfood still write schema 1. |
| Host and security (hs) | Shared heavy admission (`controller/admission.go`) for the roll build, `heavy-op run`, `gate run` and landing: hard breaker with alert, soft hold, 12 GiB landing floor, two-reading I/O gate, dead-owner reclaim on every ledger access, host-stable ledger default `~/.fae/host-ledger` (HOST-001/-007/-010/-020/-031). Controller launcher authenticated by key file and bound to the instance's maintenance marker, binary hash checked against the marker (SEC-001). Command scratch swept at start (HOST-020). Own-tree leak census every round (HOST-022). Process cap degrades to RLIMIT_NPROC (HOST-015). serve.log rotation; credentials refused inside repositories; planted-secret scan (SEC-003); refusal/event pairing (OBS-006); operator messages in history (OBS-003); sandbox refusal message (HOST-030); UI child role (HOST-021). | HOST-011 conflict (below). `alerts.log` unrotated. |
| API, CLI, controller (ap) | `run --mode normal` needs exactly one of `--dry-run`/`--commit-external`, enforced in the daemon too (CLI-011). Exit-code holes closed; conformance checks every code or cites a proof (CLI-001; a few holes remain, see the audit). Every daemon-backed verb maps to one RPC (API-010, partial). Leases, Panes, Board filters, ProviderHealth series, TargetConfig validate/diff/simulate (API-001); idempotence keys on every keyed mutating RPC (API-012); lifecycle events with a determinism test (API-020); generated per-method schema with additive-evolution check (ARC-004). CLI remainder (CLI-002/-013/-014/-015/-016/-043/-052/-054/-010). Roll-abort tests, watchdog on the workspace filesystem, configurable re-arm bounds (CTL-001/-020). Landing flakes/frozen from the pinned target (CTL-030, partial). Local push/remote refused with exit 78 (MODE-002, partial); per-value mode errors (MODE-004); unmapped publish/merge flags fail closed (EXT-010). | Roll, maintenance, `gate run`, `cleanup scratch --apply` stay client-orchestrated; landing gates Go-only; pre-tool hook doesn't refuse Local push. |
| Verification (fv) | Eight more guards in Gobra (72 total) with mutations; ledger maps **126 of 284** requirements, native-unknown 8 → 3; landing gates and role-shim refusals censused (VER-001/-002). Generated corpus covers the full primitive library (VER-003); generated floor-weakening refusals (WF-070); hostile-environment run and residue census in `test.sh` (VER-006); `FAE_TEST_HERDR_BINARY` injection; `docs/DEVELOPMENT.md` (PLAN-002); live crash matrix over 51 boundaries plus pane-loss seam (VER-010). | 3 honest native-unknowns (OverlapGate, RunShim, gomela:completion). |

Integration found and fixed:
- **Cross-stream test assumptions:**
  - rb's SameGate bound versus wf's tests.
  - The migrate test's edge after wf's new red-route.
  - hs's spec-required 30 s two-reading heavy-op gate versus ap's 2 s conformance bound.
  - Roll's new `--binary`/`--note` requirement.
  - The UI guard call sites, which moved with the `fae-ui` split.
  - The role-shim refusal census versus ap's exit-78 Local refusal and wf's shim-list refusal.
  - Tests written before ap's explicit dry run, rg's plane-marker gate, wf's `target_requirements` and rg's `fae-ui` split.
  - Store-less task-control tests, after cp's keyed terminal reads.
  - Helper-protocol variables, after fv's rename.
- **Latent defects:**
  - `LandStatus` could report a just-finished landing as interrupted.
  - Crash-helper ready files were written non-atomically.
  - A daemon start that failed between `Listen` and `Serve` left its RPC socket listening; `http.Server.Shutdown` doesn't close a listener it never adopted.
  - Crash-matrix instances could collide on hashed Dolt ports.
  - The controller test helper could outlive its fixture.
  - A leaked test `herdr-core serve` with a deleted socket made the host-wide Herdr census fail closed for every instance on the host.
- **Re-recorded:** the parity corpus, and generation 2's hash. Re-recording the hash without a new generation was the mistake that kept wave-2 instances on the old default; wave 4 bumped to generation 3 and now keeps every generation's hash. The `test.sh` daemon timeout was raised to 90 min.

After integration, the live dogfood's first attempt failed only because `dogfood.py` installed a schema-2 workflow whose ExternalEffect role lacked the effect grant; the script now grants it, and the second attempt passed (see Final verification).

## Completed in wave 2 (2026-10-23)

*Several "Left open" items below were closed in wave 3 (writers for every table, terminal jobs not loaded at start, the no-red route and request judge, live crash-matrix boundaries, loop-collection fault isolation, quarantine of rows without a disposition). The wave-3 table is current.*

First, at the operator's request, daemon tests were switched to **real host pressure by default**; `FAE_TEST_QUIET_HOST_PRESSURE=1` opts into quiet readings for development runs only. Eight streams then ran in parallel from a snapshot of the uncommitted tree, and their branches were merged and reconciled in a short-path integration worktree.

| Stream | Delivered | Left open |
|---|---|---|
| Control-plane data model | Molecules bound inside the admission transaction (id = hash of workflow id, revision, content), `ended_at`, reopen on a new task. Every job write projects tasks, transitions (with evidence digest), decisions, content-addressed artifacts and provenance **in the same transaction**; a state change without a transition, or a completion without an artifact, fails closed (FAE-WF-006/-007/-022, CP-011/-008, migration 8). Dolt triggers refuse UPDATE/DELETE/REPLACE on provenance, artifacts, transitions and decisions; provenance carries the session transcript reference; a `ProvenanceChain` read walks a commit back to the spec (WF-051/-052, OBS-007). Role pools keyed by molecule; `--max-molecules`/`--max-workflow-molecules` (−1) with `molecule_limit` withholding (SCH-005/-006/-007). `Topology` read: 13.7 ms at 100k transitions (CP-009). | Sessions, external_effects, code_reviews, schedules and engine_runs still have no writer; startup still loads every job snapshot. |
| Real-pressure suite | Pressure helpers in `daemon/pressure_helpers_test.go`: `call` retries deferred refusals (`callOnce` doesn't); progress-aware `awaitState`/hang guards wait through `infrastructure-deferred` parks but fail on charges, escalations and non-deferred refusals. Fixtures reap their processes, sweep orphaned planes and skip occupied Dolt ports. `scripts/test.sh` runs the daemon package alone after the rest (60 min). Deferred backup/retention/replication schedule runs withdraw their claim instead of consuming a run. | The suite's own Dolt activity keeps I/O PSI around 4–6 on this host; stale fixture directories in /var/tmp are not swept. |
| Deterministic recovery | Root cause of the nondeterministic planned replay found (a cancelled launch gate lacked positive retirement proof): planned stops now replay deterministically; unproven retirement parks (ROB-011/-012). `fae quarantine [inspect] | release | retire` (ROB-042); executor-kind mismatch and invalid disposition quarantine instead of refusing start. Completed, exactly remeasured host/VCS effect receipts are adopted automatically (ROB-016). Housekeeping step faults are isolated with a `drive-step-fault` alert (ROB-001). Every-boundary crash matrix: 51 durable boundaries × (SIGKILL, graceful) in scripted mode match the uninterrupted run (ROB-012, VER-010). `publishStatus` O(n²) fixed. Close joins the drive before its workers (WaitGroup race). | Live-mode crash-matrix boundaries; a `collectLoop` error still ends a drive generation. |
| Target policy and governance | Typed whole-argument `{{name}}` placeholders bound to literal argv (WF-035); TargetConfig schema 4 with path-scoped `required_tests` and `compile_check`, proven per tree before commit (WF-085); test shapes go-integration/script/runner (WF-036); `no_red_stages` + diff classifier → NoRedViolation (WF-073); task classes with overlays, `--task-class` (WF-044/-001); scope-widen and RED-rework requests with an independent judge, durable counters and approved scope carried into briefs, bridge allowlists and VCS checks → ScopeWidenRejected (WF-078, OPS-001); replacement RED verified at the task base (WF-077); progress-aware loop discount (WF-075); `--carry-from` pre-code evidence carry-over (WF-086). | Policy hooks (spec not concrete); the default workflow has no no-red route or request judge yet; landing runs templated QA stages literally. |
| Primitive library | HumanDecision end to end with `fae decide` (parks in stage, durable decision row, inbox prompt); WF-030 config fields (Escalate reason, Review aggregation, Loop exit gate/backoff/exhaustion kind, HostCommand expected shape and environment, AgentTurn profile tightening, Gate name); FanOut/FanIn with durable branch state and isolation; SubWorkflow with pinned child tasks; gate, branch-arm, aggregate and shape results stored as content-addressed `GateRecord`s that replay identically (WF-063). | FanOut branches run sequentially (one in-flight dispatch per task). |
| Normal content and ledger | Shipped default includes a Normal-only review sub-workflow (ticket → draft review → read/remediate (cap 5) → publish → merge → ticket closed); Local tasks run a byte-identical Local projection; dry runs walk it recording intents only (WF-080, EXT-010..016). Audited default-revision upgrade for existing instances (`defaults.Generation`). Ledger: five new Gobra guards in `verify.sh`; a native-check census of validator rules, shim refusals and host-gate reasons; **101 of 284** requirements now map to a check (was 68). Parity corpus: 33 recorded default-workflow runs covering every Branch arm (WF-094). | IntegrationConflict unreachable within default limits for the corpus. |
| UI child | Optional supervised UI child (`serve --ui*`), loopback-only, token-gated (never on argv), read-only unless `--ui-allow-writes`; bounded restart with one exhaustion alert; status `ui` fields; child lifecycle events; `fae ui token`. Listener census test of the whole process tree (API-030..033, OBS-002, SEC-001, ARC-006). | — |
| Observability and runtime | Role shims route heavy commands through `heavy-op run` under the host lease (HOST-011, CLI-053); one structured log line per committed park/escalation (OBS-006); process records carry a role, the daemon is a child subreaper, `processowner.Wait` no longer polls (HOST-021/-023); idle daemon makes **0** plane round trips in 30 s (was 616) (ARC-002); build-workspace detection with pinned identity (ARC-023); `SessionAdapter` conformance across scripted/direct/Herdr (RT-001); `cleanup scratch-archive` via the daemon; per-run process cap via `systemd-run --user --scope TasksMax=512` where pids is delegated (HOST-015). | Hosts without cgroup pids delegation run uncapped (logged). |

After integration, the live dogfood found two schedule defects introduced by deadline-driven schedule workers: controls starved by back-to-back runs, and intervals counted from the run start. Both are fixed (see Final verification). Integration notes: the cross-stream reconciliation covered decision readers (`TaskDecisions` records vs `TaskDecisionPayloads`), workflow/engine struct and route-rule unions (pl × tp × dm), the shipped-default hash re-pin (generation 2; the re-recorded parity corpus was byte-identical), the validator-rule census and ledger markers, the role/ownership start path (`StartRole` with `GateError` named returns), board items carrying molecule ids, quarantine-aware backlog reload, CLI usage and conformance rows, and the planned-replay assertion (deterministic replay, tolerating a replay deferred by real host pressure).

## Completed in wave 1 (2026-10-22)

*Several "Left open" items below were closed in wave 2. Among them: molecule ids and molecule-keyed caps, role shims routed through `heavy-op run`, scratch-archive cleanup through the daemon, the per-run process cap, OBS-006/-007, primitives, the Normal review sub-workflow, the default-revision upgrade and the quarantine verb. The wave-2 table and the requirements audit below are current.*

A per-requirement audit of all 284 IDs (nine parallel section audits, static evidence with spot-checks) found 77 Met, 34 implemented-but-unproven, 131 partial, 32 missing and 8 contradicting the spec. Eight parallel streams then worked from that audit in isolated worktrees branched from a snapshot of the uncommitted tree, and were merged, cross-checked and verified together. Per stream (requirement IDs in brackets):

| Stream | Delivered | Left open |
|---|---|---|
| Security and correctness | Herdr census accepts client-role processes (the operator's `remote-client-bridge` made every Herdr launch refuse; SCH-012 fail-closed kept for servers). Daemon environment sealed in a 0600 `controller-environment-<digest>.json`; plane, events and RPCs carry only the digest (SEC-003). One maintenance/shutting-down refusal reading the real controller marker for every mutating RPC, typed codes (CTL/API-011); the old `<root>/maintenance` check read a file nothing wrote. Resident worker socket authenticated by key file, sockets created 0600 (SEC-001). Controller roll `go build` under the host heavy-op lease (SCH-011.7). Dolt port probe dials instead of binding (ARC-006). Live config must be owner-only and outside any repository. | Secrets already in Dolt history/backups from earlier runs are not purged. Launcher socket is permission-protected only. |
| Default workflow and floors | Shipped default runs live unpatched: phase/`default` brief keys (validator rejects others), §4.8 role text, real routes (claude-opus-5-5, claude-sonnet-5-5, codex gpt-6-astra ×2), coder `edit`, `go test -json -count=1 -a -x ./...` with RED/green narrowed to `-run=^TestID$` (weight 1) and whole-suite QA weight 2; `dogfood.py` no longer patches it (WF-040/043/082/083, ROB-031, WF-036). Test-first holes closed (WF-073/062), token-identity frozen-RED check (WF-076). Rework stages bind review/oracle/QA findings; typed per-finding oracle decisions (WF-084). Validator: path to Done, all errors with paths (WF-090/003/004). Simulator runs the default workflow (WF-091). | No-red classifier and `code_bearing` per task class; Normal-mode review sub-workflow in default content; target-config QA tests; WF-077 replacement RED not checked against base. Existing instances keep their stored revision-1 default (old `main` brief keys) — install the new default as a new revision. |
| Scheduler and host admission | Unbounded tasks in flight by default (−1) (SCH-004); agent turns take no heavy-op unit; nothing scarce held while waiting on a role cap, class-then-priority ordering (SCH-008); newest-live-revision global role cap (SCH-006); per-reason withheld counts in status (SCH-010); admission-time ties (SCH-003). PaneLimit/MemoryReservation back-off, resident hold, direct-mode pending reservations (HOST-002/006); hard-refusal alert (HOST-003); unreadable MemAvailable proceeds with a logged gap (HOST-004); dead foreign heavy-lease reclaim (HOST-031); role-lease sweep (ROB-015); I/O gate on backup/replication/cleanup (HOST-010); brokered TMPDIR on disk (HOST-020); whole-suite runs `-p=1 -parallel=1`. | Per-run process limit (HOST-015) needs cgroup `pids.max` and a spec value. Task-bound caps keyed by workflow+revision until molecules exist. |
| Robustness and escalations | Closed Appendix B kind set on every escalation; Escalate stages take `kind`; SameGate, LoopCap naming its loop, IntegrationConflict, ProviderUnavailable (bounded), ResumeEvidence, ExternalEffectRefused, StaleBranchResidue, MergeOwnHunkLoss now raised. Per-task faults isolated to the task (quarantine, bounded persist, per-step lock) (ROB-001, CP-007, ROB-021). Live malformed output retries then escalates (WF-034/ROB-002); startup exhaustion parks ProviderUnavailable (RT-004); one failover rotation per turn (RT-010). Commit/integrate finish within a 120 s drain; clean-stop marker lists in-flight turns; proven planned replay without charge; interrupted QA redispatchable (ROB-010..013). Stuck commit/integrate detector with alert (VCS-009). | Quarantine has no operator verb (ROB-042). Non-transient cron/loop/runtime-config errors still end a drive generation. Live planned replay lacks a cross-process end-to-end test. ScopeWidenRejected/NoRedViolation/CoordinatorVeto defined but not raised. |
| CLI governance | `template workflow-get/-latest/-list/-retire/-diff/-validate/-simulate` through the daemon; retirement refuses new bindings; structural diff (CLI-020, WF-002/007/092). One exit-code contract with a conformance test over every command (CLI-001). Status fields: version/build, plane, leases, panes, heavy ops, host, provider health, `--tasks` (CLI-012). `run --dry-run/--start`, `start-work-item --content` via backlog, `resume/show --run-id`, serve flags and detach (CLI-010/011/013/030/031). `smoke-cli.py` fixed. | `molecule_id` omitted until molecules exist. `cleanup scratch-archive` still mutates locally. |
| Operator verbs | Threads, `reply --to`, `message --task`, daemon-assigned ids, controller relay (never authorizes), `inbox` (OPS-020, CLI-033/034; schema v7). `attach-design-doc`, `reclaim`, `analyze-session`, hidden `heavy-op run`, `doctor` (CLI-032/050/052/053/054). `migrate` verb (OPS-010, WF-093). Prophet context includes the task's messages (OPS-030). | `import-legacy` (format unspecified). Scope-widen/RED-rework requests (WF-078). Role shims still refuse rather than call `heavy-op run`. |
| Controller operations | `gates self-test` proving every gate can fail and `gate run` with the 12 GiB floor, I/O hold and controller-class lease (CTL-050/051, HOST-007/010). `refs/fae/gated/*` advances only after a fully gated landing with a durable proof; roll refuses unproven tips. `land` with all §12.4 gates, CAS fast-forward, held branches, salvage, batch, `--then-roll` (CTL-030..033). Roll acceptance checks the watchdog; rollback/abort/when-clear/no-budget tests (CTL-001/002). | Landing gates are Go-only; conflicts always hold; no lease journal for an interrupted landing. New guards in `controller/gates_guard.go` are not yet in `verify.sh`. |
| Observability | Timestamped, component-tagged, rotating `supervision.log` with stderr copy (OBS-001). Shared alert helper, `<root>/alerts.log`, `fae alerts list/ack`; process-leak, dead-edge, route-exhaustion, host hard-refusal and stuck-commit alerts (OBS-005). Process census incl. setsid descendants with leak thresholds (HOST-022). `Metrics`/`HostMetrics` RPCs and CLI (OBS-004, partial). Lease waits as task events (OBS-003). | Provider session references in provenance (OBS-007); park/escalation log pairing (OBS-006); generic reaper and process role field (HOST-021/023). |

Integration notes: merged in an isolated worktree; cross-branch fixes covered the drive loop (S4's per-task dispatch with S3's withheld counters and lease sweep), lease-wait tracing on S3's admission order, alert routing, simulator escalation kinds, the detached-serve start-line reader (S8's stderr log lines), CLI help/conformance for every new verb, and fixtures. Wave 1 was verified with host PSI and load reported as quiet in the daemon test binary, because the suite's own `-a` builds raised real I/O pressure enough to refuse unrelated live tests. On 2026-10-23 the default was inverted at the operator's request: live tests use **real host pressure by default**, and `FAE_TEST_QUIET_HOST_PRESSURE=1` opts into quiet readings for development runs only.

## Previous round: Claude provider, host admission and acceptance ledger

### Claude Code as the default provider

The user asked for Claude instead of Codex. `cmd/fae-claude-bridge` implements `fae-agent-v1` over headless Claude Code (`claude -p --output-format=stream-json --json-schema=...`) and shares the existing prompt, declared-file edit policy, schema validation, handoff publication and persistent-context cache with the Codex bridge (`provider/codex.go` now dispatches on `Config.Provider`; Codex is retained, unchanged in behaviour).

- Launch controls: `--tools ""`, `--restricted`, `--safe-mode`, `--strict-mcp-config` with no servers, `--disable-slash-commands`, `--permission-mode=dontAsk`, `--permission-prompts=none`; values as `--flag=value`. Unknown effort values are refused because the CLI silently substitutes its default.
- Acceptance (`provider/claude.go`, guard `provider.CanAcceptClaudeTurn`): the bridge reads the complete stream (not only the bounded log) and requires one `system/init` whose tools are only `StructuredOutput` with no MCP servers, the requested/resumed session identity, and exactly one successful non-error `result` with `structured_output` for that session. Anything else refuses before edits.
- Persistent contexts: new contexts pass a bridge-generated `--session-id`; resumed ones `--resume`; mismatches refuse and rotate the context (verified live: resume preserves identity and memory).
- Credentials must be explicit in the pinned target environment (`CLAUDE_CONFIG_DIR`, `CLAUDE_CODE_OAUTH_TOKEN`, `ANTHROPIC_API_KEY`/`ANTHROPIC_AUTH_TOKEN` or a Bedrock/Vertex/Foundry selection); the bridge refuses without one. OAuth credential files are never copied (refresh-token rotation could log out the operator's own session). Dogfood pins `CLAUDE_CONFIG_DIR=~/.claude`.
- `providerhealth.ReadLog` classifies Claude API-error records, failed results and rejected rate-limit events; message content and successful results are never searched.
- Defaults: `serve --prophet-provider claude --prophet-model claude-opus-5-5`; `scripts/dogfood.py --provider claude` (default) with `--model claude-opus-5-5`; `--provider codex --model MODEL` still works.

See `provider/README.md` and `LIVE.md`.

### Host admission semantics (HANDOFF item 1, first half)

FAE-HOST-001/-005/-010 P0 mismatch corrected (`hostguard/`, `daemon/live_admission.go`; see `hostguard/README.md`):

- PSI **full** avg10/avg60 for I/O and memory; a missing full line is an Unknown gap. Swap is **SwapTotal** (no swap configured ⇒ stricter hard floor).
- `hostguard.Hold`/`Wait`: injected clock and sampler, two strictly-below readings at least 30 s apart lift a hold (guard `CanLiftPressureHold`), a reading at/above threshold keeps it and restarts the count, readings over 60 s apart restart the chain, every reading records avg10/avg60 with rising/falling/steady direction, unknown telemetry proceeds with a logged gap, waits are bounded (first reading plus 10 re-checks at 3 s) and cancellable, with distinct proceed/waiting/refused/stop outcomes.
- HostCommand, VcsCommit and Integrate pass the two-reading I/O gate (full avg10 below 5.0) **before** the journal reservation and heavy-lease enqueue (guard `CanEnqueueHeavyLease`). Every launch still samples under the lease; soft pressure backs off and is lifted only by the two-reading rule; the hard breaker and non-pressure refusals remain immediate. Exhausted waits return deferred refusals (`IOPressure`, `IOPressureHold`, `HostPressure`, `HostPressureHold`) that park without charging an attempt. Structured `host admission reading|refused` log lines.
- The daemon **test binary** scales the policy window to 30 ms (`daemon/live_admission_pressure_test.go` `init`) so existing 60 s-bounded live tests stay meaningful; production defaults are asserted by a test.

The host gaps this round left (PaneLimit/MemoryReservation back-off, resident-pane hold, HOST-003 alert, heavy lease held during post-lease back-off) were closed in wave 1.

### Acceptance ledger (HANDOFF item 1, second half)

`verification/ledger/*.json` maps every formal check shipped in `scripts/verify.sh` (59 Gobra guards with their exact mutations; 55 positive Gomela models each paired with exactly one negative model) to requirement IDs or an explicit `unmapped_reason`, production predicate and call sites, positive tests, firing control and native daemon-path evidence (`unknown` stays unknown). `scripts/check-ledger.py` (run by `scripts/test.sh`, so CI) fails on any unmapped or stale check, missing file/function/test, unknown requirement ID or mutation drift; `--report` lists coverage for all 284 requirement IDs. Current census: 114 entries, 68 requirement IDs with at least one mapped check, 24 entries with stated unmapped reasons, 7 entries with unknown native evidence. This is an evidence census, not requirement certification. See `verification/LEDGER.md`.

### Linux portability

- `git merge-tree` reconciliation requires **Git ≥ 2.45** (tree merge bases); Ubuntu 24.04 needs `ppa:git-core/ppa`.
- Linux `lsof -F n` appends ` type=STREAM` (and state) to UNIX socket names. Both parsers now use `runtime.LsofName`. This fixed a **fail-open** scratch-IPC check (a live socket compared unequal and looked unused) and the Herdr pane census.
- Stall CPU liveness reads `/proc/<pid>/stat` ticks on Linux (procps `TIME` has whole-second resolution).
- `daemon/scratch_ipc_test.go` no longer hard-codes `/private/tmp`.

The previous round's replication unpinning and backup quotas are documented in `daemon/BACKUPS.md` and `verification/results/backup-storage.json`.

## Final verification

**Wave 13 (this working tree):**

- `scripts/test.sh` passed on the third run on the integrated tree with **real host pressure** (no quiet variable). It covered **3,189 race-enabled test cases across 36 packages** (1 skipped), with 81.1% statement coverage. It also ran:
  - `go vet`;
  - the ledger census;
  - the self-host harness self-test (23 cases);
  - the full 10,000-case generated corpus;
  - the hostile-environment runs;
  - the residue census.

  The daemon package took 88 minutes under `-race`, and the script 106.

  The two earlier runs:
  - **Run 1** failed one test, `TestLiveHostCommandShimPolicyRefusals`. It still expected a host command the shim policy refuses to park UncertainDispatch; wave 13 makes that a free refusal that escalates SameGate. The test and both ledger paths now say so.
  - **Run 2** failed two tests. `TestDaemonTestRunRefusedBeforeExecChargesNothing` (new in wave 13) counted a host-pressure deferral park as a failure; both refusal tests now wait through such parks. `TestTopologyScopedOverLargePlane` derived Dolt port 6767 from its temporary directory, and another program on the host held it, so Fae refused it as a foreign listener. That fixture flaw predates wave 13 and is not fixed (see Next work).
- `scripts/verify.sh` passed in 11 minutes: **Gobra 72 production guards and Gomela 57 positive / 55 negative bounded models; every firing control was rejected.** No guard or model changed.
- Ledger census: 168 entries, **134 of 284 requirements** mapped; the same nine entries have unknown native evidence.
- Real-provider acceptance: **passed on Claude Code on the first attempt** (`claude-opus-5-5`, Claude Code 2.1.310, Herdr 0.9.5, Dolt 2.5.1) with every feature flag, in 8 minutes 13 seconds.
  - Both code-bearing tasks reached Done (22 and 23 artifact records; 31 real completed turns).
  - A SIGKILL during a live turn ended in explicit redispatch.
  - Drained restart, drive stop/rearm, a guarded roll through the authenticated launcher, both Prophet scopes and the 5-iteration provider loop worked.
  - Persistent contexts: 17 audited, 8 resumed, 2 after restart, no cross-task threads.
  - Resident sessions: 18 audited turns, 1 reused pane.
  - Host leases: 25 granted, 25 released, 0 final holders; 0 final pane reservations. One `workspace review-prepare` was deferred for I/O pressure (`IOPressureHold`) and retried.
  - Also passed: Normal (1 live effect), cron, target, retirement, census, scratch and scratch-archive cleanup, fleet (a lost acknowledgement recovered after restart), dynamic provisioning, adapter retirement, backup policy, schedules (2 scheduled uploads, 0 duplicates), storage and retention.
  - The run exercised none of wave 13's new outcomes (the start and deferral bounds, a refund before exec, WF-093's refusal or the quarantine flag, the leak grace), nor `fae task` or `fae-tui`; the tests above cover them.
  - Evidence: `verification/results/claude-wave13-dogfood-20261031/` (37 files; its 1,089 source hashes match this tree).
- Fae-on-Fae acceptance: not rerun on the wave-9 to wave-13 trees; wave 8's run is below. What raises QA's `process-leak` alerts needs that run, with a new task; wave 13's alert names the parent command.

**Wave 12 (2026-10-30, superseded by wave 13):**

- `scripts/test.sh` passed on the third run on the integrated tree with **real host pressure** (no quiet variable). It covered **3,140 race-enabled test cases across 36 packages** (1 skipped), with 80.8% statement coverage. It also ran:
  - `go vet`;
  - the ledger census;
  - the self-host harness self-test (23 cases);
  - the full 10,000-case generated corpus;
  - the hostile-environment runs;
  - the residue census.

  The daemon package took 87 minutes under `-race`, and the script 104.

  The two earlier runs:
  - **Run 1** stopped when the daemon package passed its 90-minute timeout. Before that, `TestDefaultWorkflowUpgradeOnStoredPlane` and `TestStartNamesADamagedEventClockBeforeWritingAnEvent` failed: the first on what WF-093 calls unloadable, the second on a planted file time. Both are fixed; see Integration added under Completed in wave 12. The timeout is now 150 minutes.
  - **Run 2** passed every Go test, then `count-tests.py` refused `cmd/fae-tui`, which had no test. It has one now.
- `scripts/verify.sh` passed in 11 minutes: **Gobra 72 production guards and Gomela 57 positive / 55 negative bounded models; every firing control was rejected.** No guard or model changed.
- Ledger census: 168 entries, **134 of 284 requirements** mapped.
  - New: `check:nested-git-local-refusals`, `check:red-subtest-refusal` and `check:red-turn-change-refusals`.
  - Nine entries have unknown native evidence: wave 11's seven plus the two new RED entries.
- Real-provider acceptance: **passed on Claude Code on the first attempt** (`claude-opus-5-5`, Claude Code 2.1.310, Herdr 0.9.5, Dolt 2.5.1) with every feature flag, in 8 minutes 15 seconds.
  - Both code-bearing tasks reached Done (22 and 23 artifact records; 31 real completed turns).
  - A SIGKILL during a live turn ended in explicit redispatch.
  - Drained restart, drive stop/rearm, a guarded roll through the authenticated launcher, both Prophet scopes and the 5-iteration provider loop worked.
  - Persistent contexts: 17 audited, 8 resumed, 2 after restart, no cross-task threads.
  - Resident sessions: 18 audited turns, 1 reused pane.
  - Host leases: 25 granted, 25 released, 0 final holders; 0 final pane reservations. One `workspace review-prepare` was deferred for I/O pressure (`IOPressureHold`) and retried.
  - Also passed: Normal (1 live effect), cron, target, retirement, census, scratch and scratch-archive cleanup, fleet (a lost acknowledgement recovered after restart), dynamic provisioning, adapter retirement, backup policy, schedules (2 scheduled uploads, 0 duplicates), storage and retention.
  - The run exercised none of wave 12's new outcomes (a refusal before launch, a provider that never acknowledged, the per-binding start bound), nor `fae task` or `fae-tui`; the tests above cover them, including `TestReadsAgainstARealDaemon` and `TestWritesAgainstARealDaemon` against a real daemon.
  - Evidence: `verification/results/claude-wave12-dogfood-20261030/` (37 files; its 1,080 source hashes match this tree).
- Fae-on-Fae acceptance: not rerun on the wave-9 to wave-12 trees; wave 8's run is below. Whether wave 12 stopped QA's `process-leak` alerts needs that run, with a new task.

**Wave 11 (2026-10-29, superseded by wave 12):**

- `scripts/test.sh` passed on the first run on the integrated tree with **real host pressure** (no quiet variable). It covered **2,976 race-enabled test cases across 30 packages** (1 skipped), with 80.6% statement coverage. It also ran:
  - `go vet`;
  - the ledger census;
  - the self-host harness self-test (23 cases);
  - the full 10,000-case generated corpus;
  - the hostile-environment runs;
  - the residue census.

  The daemon package took 88 minutes under `-race`, and the script 105. In an earlier package-by-package run outside the script, `TestListenerCensusWithAndWithoutUI` (cmd/fae) failed once because another package's Dolt took the port its daemon had picked; it passes alone and passed in the script, and no wave-11 change touches port choice.
- `scripts/verify.sh` passed: **Gobra 72 production guards and Gomela 57 positive / 55 negative bounded models; every firing control was rejected.** No guard or model changed.
- Ledger census: 165 entries (new: `check:nested-git-refusals` and `check:derived-scope-refusals`, which census every rule and fail when a test doesn't fire one, and the families `check:route-refusals`, `check:secret-redaction`, `check:bridge-edits-not-applied` and `check:protected-paths`), **134 of 284 requirements** mapped. Seven entries have unknown native evidence: `check:landing-overlap-gate`, `check:role-shim-refusals`, `gomela:completion`, `check:local-denials`, `check:hook-denials`, and the new `check:bridge-edits-not-applied` and `check:protected-paths`.
- Real-provider acceptance: **passed on Claude Code on the first attempt** (`claude-opus-5-5`, Claude Code 2.1.310, Herdr 0.9.5, Dolt 2.5.1) with every feature flag, in 8 minutes 35 seconds.
  - Both code-bearing tasks reached Done (22 and 23 artifact records; 31 real completed turns).
  - A SIGKILL during a live turn ended in explicit redispatch.
  - Drained restart, drive stop/rearm, a guarded roll through the authenticated launcher, both Prophet scopes and the 5-iteration provider loop worked.
  - Persistent contexts: 17 audited, 8 resumed, 2 after restart, no cross-task threads.
  - Resident sessions: 18 audited turns, 1 reused pane.
  - Host leases: 25 granted, 25 released, 0 final holders; 0 final pane reservations. No command was held for host pressure in this run.
  - Normal (1 live effect), cron, target, retirement, census, scratch and scratch-archive cleanup, fleet (lost acknowledgement recovered after restart), dynamic provisioning, adapter retirement, backup policy, schedules (2 scheduled uploads, 0 duplicates), storage and retention checks passed.
  - The run exercised none of wave 11's new outcomes (`scope-dropped`, `edits-not-applied`, `provider-failed`, scratch quarantine); the tests above cover them.
  - Evidence: `verification/results/claude-wave11-dogfood-20261029/` (37 files; its 1,029 source hashes match this tree).
- Fae-on-Fae acceptance: not rerun on the wave-9, wave-10 or wave-11 trees; wave 8's run is below. The task prepared after wave 10 (a `go test -run` value that matches every test counts as a whole suite) was fixed in wave 11, so the next run needs a new task.

**Wave 10 (2026-10-29, superseded by wave 11):**

- `scripts/test.sh` passed on the integrated tree with **real host pressure** (no quiet variable). It covered **2,832 race-enabled test cases across 30 packages** (1 skipped), with 80.2% statement coverage. It also ran:
  - `go vet`;
  - the ledger census;
  - the self-host harness self-test (23 cases);
  - the full 10,000-case generated corpus;
  - the hostile-environment runs;
  - the residue census.

  The daemon package took 85 minutes under `-race`, and the script 101. The first run failed only on two new nested-go tests that depended on the script's coverage-built wrapper (see Completed in wave 10, Integration added).
- `scripts/verify.sh` passed: **Gobra 72 production guards and Gomela 57 positive / 55 negative bounded models; every firing control was rejected.** No guard or model changed.
- Ledger census: 159 entries (new: `check:red-reason`, `check:bridge-refusal-classes`, `check:scope-widen`, `check:hook-denials`, `check:local-denials`, `check:validator-requests`), **130 of 284 requirements** mapped. Five entries have unknown native evidence: `check:landing-overlap-gate`, `check:role-shim-refusals`, `gomela:completion`, and the new `check:local-denials` and `check:hook-denials`. `check:shim-accounted-go` is now native-evidenced.
- Real-provider acceptance: **passed on Claude Code on the first attempt** (`claude-opus-5-5`, Claude Code 2.1.310, Herdr 0.9.5, Dolt 2.5.1) with every feature flag, in 7 minutes 35 seconds.
  - Both code-bearing tasks reached Done (22 and 23 artifact records; 31 real completed turns).
  - A SIGKILL during a live turn ended in explicit redispatch.
  - Drained restart, drive stop/rearm, a guarded roll through the authenticated launcher, both Prophet scopes and the 5-iteration provider loop worked.
  - Persistent contexts: 17 audited, 8 resumed, 2 after restart, no cross-task threads.
  - Resident sessions: 18 audited turns, 1 reused pane.
  - Host leases: 25 granted, 25 released, 0 final holders; 0 final pane reservations. No command was held for host pressure in this run.
  - Normal (1 live effect), cron, target, retirement, census, scratch and scratch-archive cleanup, fleet (lost acknowledgement recovered after restart), dynamic provisioning, adapter retirement, backup policy, schedules (2 scheduled uploads, 0 duplicates), storage and retention checks passed.
  - Evidence: `verification/results/claude-wave10-dogfood-20261029/` (37 files; its 971 source hashes match this tree).
- Fae-on-Fae acceptance: not rerun on the wave-9 or wave-10 trees; wave 8's run is below. A task is prepared for the next run (a `go test -run` value that matches every test counts as a whole suite, in `commandshim`). *(Wave 11 fixed that bug by hand; the next run needs a new task.)*

**Wave 9 (2026-10-28, superseded by wave 10):**

- `scripts/test.sh` on the integrated tree with **real host pressure** (no quiet variable) passed. It covered **2,734 race-enabled test cases across 30 packages**, with 80.0% statement coverage. It also ran:
  - `go vet`;
  - the ledger census;
  - the self-host harness self-test (23 cases);
  - the full 10,000-case generated corpus;
  - the hostile-environment runs;
  - the residue census.

  The daemon package took 83 minutes under `-race`, and the script 98. The first run failed only on two new tests that depended on the script's environment (see Completed in wave 9, Integration added).
- `scripts/verify.sh`: **Gobra 72 production guards and Gomela 57 positive / 55 negative bounded models passed; every firing control was rejected.** No guard changed.
- Ledger census: 153 entries (the new `check:shim-accounted-go`), **127 of 284 requirements** mapped; 4 entries with unknown native evidence (`check:shim-accounted-go` joins the earlier 3). `gobra:shim` no longer maps HOST-014.
- Real-provider acceptance: **passed on Claude Code on the first attempt** (`claude-opus-5-5`, Claude Code 2.1.310, Herdr 0.9.5, Dolt 2.5.1) with every feature flag, in 7 minutes 58 seconds.
  - Both code-bearing tasks reached Done (22 and 23 artifact records; 31 real completed turns).
  - A SIGKILL during a live turn ended in explicit redispatch.
  - Drained restart, drive stop/rearm, a guarded roll through the authenticated launcher, both Prophet scopes and the 5-iteration provider loop worked.
  - Persistent contexts: 17 audited, 8 resumed, 2 after restart, no cross-task threads.
  - Resident sessions: 18 audited turns, 1 reused pane.
  - Host leases: 25 granted, 25 released, 0 final holders; 0 final pane reservations. Host admission held one `workspace review-prepare` for IO pressure, and the script retried it.
  - Normal (1 live effect), cron, target, retirement, census, scratch and scratch-archive cleanup, fleet (lost acknowledgement recovered after restart), dynamic provisioning, adapter retirement, backup policy, schedules (2 scheduled uploads, 0 duplicates), storage and retention checks passed.
  - Evidence: `verification/results/claude-wave9-dogfood-20261028/` (37 files; its 952 source hashes match this tree).
- Fae-on-Fae acceptance: not rerun on the wave-9 tree; wave 8's run is below. The next run should show QA's own `go` calls in its nested-command records.

**Wave 8 (2026-10-27, superseded by wave 9):**

- `scripts/test.sh` on the integrated tree with **real host pressure** (no quiet variable) passed. It covered **2,646 race-enabled test cases across 30 packages**, with 79.5% statement coverage. It also ran:
  - `go vet`;
  - the ledger census;
  - the self-host harness self-test (23 cases);
  - the full 10,000-case generated corpus;
  - the hostile-environment runs;
  - the residue census.

  The daemon package took 78 minutes under `-race`, and the script 94. Two earlier runs failed (see Completed in wave 8, Integration added):
  - the first, on two new tests whose Herdr socket path exceeded its bound under the script's repository `TMPDIR`;
  - the third, after the take-back, on the process-record race.
- `scripts/verify.sh`: **Gobra 72 production guards and Gomela 57 positive / 55 negative bounded models passed; every firing control was rejected.** No guard changed.
- Ledger census: 152 entries (the new `check:shim-accounted-nested`), **126 of 284 requirements** mapped; 3 entries with unknown native evidence (unchanged).
- Real-provider acceptance: **passed on Claude Code** (`claude-opus-5-5`, Claude Code 2.1.310, Herdr 0.9.5, Dolt 2.5.1) with every feature flag, in 7 minutes 40 seconds. Its pinned instance config behaved as before under wave 8's bridge refusals, RED check and reason rule.
  - Both code-bearing tasks reached Done (22 and 23 artifact records; 31 real completed turns).
  - A SIGKILL during a live turn ended in explicit redispatch.
  - Drained restart, drive stop/rearm, a guarded roll through the authenticated launcher, both Prophet scopes and the 5-iteration provider loop worked.
  - Persistent contexts: 17 audited, 8 resumed, 2 after restart, no cross-task threads.
  - Resident sessions: 18 audited turns, 1 reused pane.
  - Host leases: 25 granted, 25 released, 0 final holders; 0 final pane reservations.
  - Normal (1 live effect), cron, target, retirement, census, scratch and scratch-archive cleanup, fleet (lost acknowledgement recovered after restart), dynamic provisioning, adapter retirement, backup policy, schedules (2 scheduled uploads, 0 duplicates), storage and retention checks passed.
  - Evidence: `verification/results/claude-wave8-dogfood-20261027/` (37 files; its 927 source hashes match this tree).
- Fae-on-Fae acceptance: **passed** (`scripts/self-host.py acceptance` on a clone at `wave8/integration`, shipped default workflow, no pinned scope, `nested_commands: account`).
  - FAE-HOST-005 reached Done 1,100 s after admission: 14 provider turns and 2 attempts. At implement, the coder asked for read-only context through a scope-widen request instead of guessing.
  - QA's whole suite (2,305 tests) passed through account mode's nested-call policy, before and after the merge. *(Re-audit 7: only its nested `git` calls reached the policy. A Go test's own `go` calls bypass the wrappers, so they were neither recorded nor clamped. Wave 9 routes them through the wrappers.)*
  - Two dispatches were held by host admission for IO pressure just after a whole-suite run. Each was deferred as infrastructure, charged nothing, and redispatched.
  - The landed commit was taken back (see Completed in wave 8). Evidence: `verification/results/claude-wave8-selfhost-20261027/`.

**Wave 7 (2026-10-26, superseded by wave 8):**

- `scripts/test.sh` on the integrated tree with **real host pressure** (no quiet variable) passed on the second run. It covered **2,512 race-enabled test cases across 30 packages**, with 79.5% statement coverage. It also ran:
  - `go vet`;
  - the ledger census;
  - the self-host harness self-test (23 cases);
  - the full 10,000-case generated corpus;
  - the hostile-environment runs;
  - the residue census.

  The daemon package took 75 minutes under `-race`, and the script 91. The first run's only failure, a Dolt metrics race in one endpoint test, is described under Completed in wave 7. Fae's own commit (the Watchdog NaN fix) was in the second run.
- `scripts/verify.sh`: **Gobra 72 production guards and Gomela 57 positive / 55 negative bounded models passed; every firing control was rejected.** No guard changed.
- Ledger census: 151 entries, **126 of 284 requirements** mapped; 3 entries with unknown native evidence (unchanged). The `gomela:shim` entry now also cites account mode's tests.
- **Fae-on-Fae acceptance:** a live instance on a clone of this repository, with no pinned scope, took a real Fae bug through every stage of the shipped default workflow to Done on its first attempt, in 879 s, with real Claude turns. Its commit landed on the clone's `fae/integration` and is now in this tree. Evidence: `verification/results/claude-wave7-selfhost-20261026/`.
- Real-provider acceptance: **passed on Claude Code on the first attempt** (`claude-opus-5-5`, Claude Code 2.1.310, Herdr 0.9.5, Dolt 2.5.1) with every feature flag, in 7 minutes 43 seconds. Its pinned instance config behaved as before under wave 7's scope resolver and bridge.
  - Both code-bearing tasks reached Done (22 and 23 artifact records; 31 real completed turns).
  - A SIGKILL during a live turn ended in explicit redispatch.
  - Drained restart, drive stop/rearm, a guarded roll through the authenticated launcher, both Prophet scopes and the 5-iteration provider loop worked.
  - Persistent contexts: 17 audited, 8 resumed, 2 after restart, no cross-task threads.
  - Resident sessions: 18 audited turns, 1 reused pane.
  - Host leases: 25 granted, 25 released, 0 final holders; 0 final pane reservations.
  - Normal (1 live effect), cron, target, retirement, census, scratch and scratch-archive cleanup, fleet (lost acknowledgement recovered after restart), dynamic provisioning, adapter retirement, backup policy, schedules (2 scheduled uploads, 0 duplicates), storage and retention checks passed.
  - Evidence: `verification/results/claude-wave7-dogfood-20261026/` (37 files, with source and evidence hashes).

**Wave 6 (2026-10-26, superseded by wave 7):**

- `scripts/test.sh` on the integrated tree with **real host pressure** (no quiet variable) passed on the third run. It covered **2,404 race-enabled test cases across 30 packages**, with 79.2% statement coverage, plus:
  - `go vet`;
  - the ledger census;
  - the full 10,000-case generated corpus;
  - the hostile-environment runs;
  - the residue census.

  The daemon package took 72 minutes under `-race`, and the script 88. The first two runs' failures, a test fault hook tripped by Admit's new fault point and a pre-existing zombie-check flake, are described under Completed in wave 6.
- `scripts/verify.sh`: **Gobra 72 production guards and Gomela 57 positive / 55 negative bounded models passed; every firing control was rejected.** No guard changed.
- Ledger census: 151 entries, **126 of 284 requirements** mapped; 3 entries with unknown native evidence (unchanged).
- Real-provider acceptance: **passed on Claude Code on the first attempt** (`claude-opus-5-5`, Claude Code 2.1.310, Herdr 0.9.5, Dolt 2.5.1) with every feature flag, in 7 minutes 50 seconds.
  - Both code-bearing tasks reached Done (22 and 23 artifact records; 31 real completed turns).
  - A SIGKILL during a live turn ended in explicit redispatch.
  - Drained restart, drive stop/rearm, a guarded roll through the authenticated launcher, both Prophet scopes and the 5-iteration provider loop worked.
  - Persistent contexts: 17 audited, 8 resumed, 2 after restart, no cross-task threads.
  - Resident sessions: 18 audited turns, 1 reused pane.
  - Host leases: 25 granted, 25 released, 0 final holders; 0 final pane reservations.
  - Normal (1 live effect), cron, target, retirement, census, scratch and scratch-archive cleanup, fleet (lost acknowledgement recovered after restart), dynamic provisioning, adapter retirement, backup policy, schedules (2 scheduled uploads, 0 duplicates), storage and retention checks passed.
  - Evidence: `verification/results/claude-wave6-dogfood-20261026/` (37 files, with source and evidence hashes).

**Wave 5 (2026-10-25, superseded by wave 6):**

- `scripts/test.sh` on the integrated tree with **real host pressure** (no quiet variable) passed on the second run. It covered **2,372 race-enabled test cases across 30 packages**, with 78.9% statement coverage, plus:
  - `go vet`;
  - the ledger census;
  - the full 10,000-case generated corpus;
  - the hostile-environment runs;
  - the residue census.

  The daemon package took 71 minutes under `-race`. The first run's two failures, an idle-test interleaving and a controller start bound, are described under Completed in wave 5.
- `scripts/verify.sh`: **Gobra 72 production guards and Gomela 57 positive / 55 negative bounded models passed; every firing control was rejected.** No guard changed.
- Ledger census: 151 entries, **126 of 284 requirements** mapped; 3 entries with unknown native evidence (unchanged).
- Real-provider acceptance: **passed on Claude Code on the first attempt** (`claude-opus-5-5`, Claude Code 2.1.309, Herdr 0.9.5, Dolt 2.5.1) with every feature flag.
  - Both code-bearing tasks reached Done (22 and 23 artifact records; 31 real completed turns).
  - A SIGKILL during a live turn ended in explicit redispatch.
  - Drained restart, drive stop/rearm, a guarded roll through the authenticated launcher, both Prophet scopes and the 5-iteration provider loop worked.
  - Persistent contexts: 17 audited, 8 resumed, 2 after restart, no cross-task threads.
  - Resident sessions: 18 audited turns, 1 reused pane.
  - Host leases: 25 granted, 25 released, 0 final holders; 0 final pane reservations.
  - Normal (1 live effect), cron, target, retirement, census, scratch and scratch-archive cleanup, fleet, dynamic provisioning, adapter retirement, backup policy, schedules (2 scheduled uploads, 0 duplicates), storage and retention checks passed.
  - Evidence: `verification/results/claude-wave5-dogfood-20261025/` (37 files, with source and evidence hashes).

**Wave 4 (2026-10-24, superseded by wave 5):**

- `scripts/test.sh` on the integrated tree with **real host pressure** (no quiet variable) passed on the first run: **2,336 race-enabled test cases across 30 packages**, 78.8% statement coverage, plus `go vet`, the ledger census, the full 10,000-case generated corpus, the hostile-environment runs and the residue census. The daemon package took 68 minutes under `-race`.
- `scripts/verify.sh`: **Gobra 72 production guards and Gomela 57 positive / 55 negative bounded models passed; every firing control was rejected.** rt moved the `gobra:resident` call site from `Run` to `acquire`; no guard body changed.
- Ledger census: 151 entries, **126 of 284 requirements** mapped; 3 entries with unknown native evidence (unchanged).
- Idle cost (ARC-002), by process accounting: 13–14 ms per 30 s; 324 and 353 ms over two full 10-minute windows (`FAE_TEST_IDLE_FULL=1`), against the 1 s bound.
- Real-provider acceptance: **passed on Claude Code on the first attempt** (`claude-opus-5-5`, Claude Code 2.1.309, Herdr 0.9.5, Dolt 2.5.1) with every feature flag. Both code-bearing tasks reached Done (22 and 23 artifact records; 31 real completed turns); SIGKILL during a live turn ended in explicit redispatch; drained restart, drive stop/rearm, a guarded roll through the authenticated launcher, both Prophet scopes and the 5-iteration provider loop worked; persistent contexts 17 audited, 8 resumed, 2 after restart, no cross-task threads; resident sessions 18 audited turns, 1 reused pane; host leases 25 granted, 25 released, 0 final holders, 0 final pane reservations; Normal (1 live effect), cron, target, retirement, census, scratch and scratch-archive cleanup, fleet, dynamic provisioning, adapter retirement, backup policy, schedules (2 scheduled uploads, 0 duplicates), storage and retention checks passed. Evidence: `verification/results/claude-wave4-dogfood-20261024/`.

**Wave 3 (2026-10-24, superseded by wave 4):**

- `scripts/test.sh` on the integrated tree with **real host pressure** (no quiet variable): **2,288 race-enabled test cases across 30 packages passed**, 78.4% statement coverage, plus `go vet`, the ledger census, the full 10,000-case generated corpus, the hostile-environment runs and the temporary-directory, fixture-root and process census. The daemon package alone takes about 67 minutes under `-race`; its timeout is now 90 minutes, and the whole script takes about 80.
  - The first integrated run had 12 failures. Nine were cross-stream test assumptions, and one was a nil-store panic that stopped the daemon package. Two were latent defects, both fixed with tests: a `Start` that failed between `Listen` and `Serve` left its RPC socket listening (`http.Server.Shutdown` doesn't close a listener it never adopted), and crash-matrix instances could collide on hashed Dolt ports.
  - The second run's only failure was a conformance socket path over the 108-byte Unix limit under the hostile run's longer TMPDIR. It was fixed, and the script's remaining steps were re-run and passed.
- `scripts/verify.sh`: **Gobra 72 production guards and Gomela 57 positive / 55 negative bounded models passed; every firing control was rejected.** The eight guards listed as missing after wave 2 are now included.
  - Still outside `verify.sh`: wave 3's own new pure guards: `CanCarryReview`, `IntegrateDisposition`, `CommitDisposition`, `SubmissionModeComplete`, `ImplicitPublicationFlag`, `CanDriveDisposition` and `execution.RestrictedEffects`. They are pure and unit-tested, except `CanCarryReview`, which is exercised only through one engine test.
- Ledger census: 151 entries, **126 of 284 requirements** with at least one mapped check (was 101); 3 entries with unknown native evidence (was 8).
- Real-provider acceptance: **passed on Claude Code** (`claude-opus-5-5`, Claude Code 2.1.309, Herdr 0.9.5, Dolt 2.5.1) with every feature flag in the command below. Results:
  - Both code-bearing tasks reached Done (22 and 23 artifact records).
  - A SIGKILL during a live provider turn ended in explicit redispatch.
  - Drained restart, independent drive stop/rearm and a guarded roll through the authenticated launcher all worked.
  - Both Prophet scopes ran, and the 5-iteration provider loop kept one context.
  - Persistent contexts: 17 audited, 8 resumed, 2 after restart, no cross-task threads.
  - Resident sessions: 18 audited turns, 1 reused pane. Task-bound transports now retire when their turn is collected (RT-011), so fewer panes are reused than wave 2's 4.
  - Normal (1 live effect), cron, target, retirement, census, scratch and scratch-archive cleanup, fleet, dynamic provisioning, backup policy, schedules (2 scheduled uploads, 0 duplicates), storage and automatic retention checks passed.
  - Host leases: capacity 2, 25 granted, 25 released, 0 final holders; 0 final pane reservations.
  - Evidence: `verification/results/claude-wave3-dogfood-20261024/` (37 files, with source and evidence hashes). The first attempt failed only on the script's schema-2 workflow (missing effect grant); it is kept as `claude-wave3-dogfood-20261024-before-effect-grant`.

**Wave 2 (2026-10-23, superseded by wave 3):**

- `scripts/test.sh` on the integrated tree with **real host pressure** (no quiet variable): **2,049 race-enabled test cases across 28 packages passed** (final run on the main tree after the two post-dogfood schedule fixes; the integration run before them passed 2,047), 78.4% statement coverage, `go vet`, the ledger census and the temporary-directory census (exit 0). The script runs every other package first and then the daemon package alone, which takes about 42 minutes; the whole suite takes about 50 minutes. The first integrated real-pressure run had 2 failures, both cross-stream test-fixture issues (a fixture root inside the checkout, and the decision test counting the gate decisions that the data model now records), fixed before the final run.
- `scripts/verify.sh`: **Gobra 64 production guards and Gomela 57 positive / 55 negative bounded models passed; every firing control was rejected.** That is five new Gobra guards: gate pass, landing floor, advance target, deploy gated and default upgrade. Newer guards not yet in `verify.sh`: `ui.MayListen`, `ui.MayServeRequest`, `ui.MayWrite`, `daemon.MayRestartUIChild`, `CanReleaseIntoMolecule`, `CanCommitWithoutRed`, `CanCommitTree` and `CanAutoAdoptEffect`. They are pure, with truth-table tests.
- Ledger census: 134 entries, **101 of 284 requirements** with at least one mapped check, plus 15 non-formal native-check entries.
- Real-provider acceptance: **passed on Claude Code** (`claude-opus-5-5`, Claude Code 2.1.308, Herdr 0.9.5, Dolt 2.5.1) with every feature flag in the command below. Results:
  - Both code-bearing tasks reached Done (32 and 23 artifact records) and landed at `cb5af39`.
  - A SIGKILL during a live provider turn ended in explicit redispatch.
  - Drained restart, independent drive stop/rearm and a guarded roll all worked.
  - Both Prophet scopes ran, and the 5-iteration provider loop kept one context.
  - Resident Herdr panes: 19 audited turns, 4 reused panes. Persistent contexts: 18 audited, 9 resumed, 2 after restart, no cross-task threads.
  - Cron, target, Normal effect, retirement, census, scratch cleanup and scratch-archive cleanup (through a serving helper instance), fleet, backup policy, backup schedules (2 scheduled uploads, 0 duplicates), storage and automatic retention checks passed.
  - Host leases: capacity 2, 30 granted, 30 released, 0 final holders; 0 final pane reservations.
  - Evidence: `verification/results/claude-wave2-dogfood-20261023/` (37 files, with source and evidence hashes).

  Four earlier attempts failed late in the backup and cleanup sections. Each is kept as `claude-wave2-dogfood-20261023-before-*`. Two of them exposed **real daemon defects** in the new deadline-driven schedules, both fixed with tests:
  - A schedule control (pause, configure, resume, unpin) was refused for as long as a run was in progress, so a short-interval schedule could not be paused. Controls now take priority: no new run starts while one waits, and the control waits up to 4 s for the current run (`daemon/schedule_control.go`).
  - A schedule's next run was due one interval after the run *started*, so runs longer than the interval followed each other back to back. That produced 15 backups instead of one and starved the replication check. The interval now also runs from completion, and `TestBackupScheduleIntervalRunsFromCompletion` fails without the fix.

  The other two were script updates: retrying a "retry control after current run" refusal, and running scratch-archive cleanup through a serving instance per CLI-001.

**Wave 1 (2026-10-22, superseded by wave 2):**

- `scripts/test.sh` on the integrated tree: **1,717 race-enabled test cases across 27 packages passed**, 76.8% statement coverage, `go vet`, the ledger census and the temporary-directory census (exit 0). The daemon package now takes about 26 minutes, so the package timeout was raised to 45 minutes. Integrated runs went from 22 failures to 6 to 2 to 0; each cause was a cross-stream interaction or a load-sensitive test, fixed in the integration commits (see the round notes above).
- `scripts/verify.sh`: **Gobra 59 production guards and Gomela 57 positive / 55 negative bounded models passed; every firing control was rejected.** Evidence regenerated under `verification/logs/` and `verification/results/`.
- Real-provider acceptance: **passed on Claude Code** (`claude-opus-5-5`, Claude Code 2.1.308, Herdr 0.9.5, Dolt 2.5.1) with every feature flag in the command below. The run used the **shipped default workflow unpatched**; `dogfood.py` only filters routes to its own bridge's provider. Results:
  - Both code-bearing tasks reached Done (35 and 23 artifact records) and landed at `ebc09c6`.
  - A SIGKILL during a live provider turn ended in explicit redispatch.
  - Drained restart, independent drive stop/rearm and a guarded roll all worked.
  - Both Prophet scopes ran, and the 5-iteration provider loop kept one context.
  - Resident Herdr panes: 19 audited turns, 4 reused panes. Persistent contexts: 18 audited, 9 resumed, 2 after restart, no cross-task threads.
  - Cron, target, retirement, census, cleanup, fleet, backup policy, schedules, storage and automatic retention checks passed.
  - Host leases: capacity 2, 34 granted, 34 released, 0 final holders; 0 final pane reservations.
  - Evidence: `verification/results/claude-wave1-dogfood-20261022/` (37 files, with source and evidence hashes).

  Two earlier attempts failed on script timing that the new HOST-010 I/O gate exposed, not on daemon behaviour. Their evidence is kept as `*-before-deferred-retry` and `*-before-gate-wait`. `dogfood.py` now retries deferred host-admission refusals (bounded, recorded in `result.json`) and allows 150 s for gated backup, replication and retention runs.

Earlier dogfood records fingerprint earlier states and do not certify wave 2.

Formal claims are limited to explicit production predicates and bounded models. They are not proofs of all filesystem races, Dolt, Git, HTTP, provider behavior or complete requirement coverage. Dogfood uses an authenticated Claude Code account with disposable localhost review/ticket/replica services; it does not establish third-party vendor conformance.

## Requirements audit

**Re-audit, 2026-11-01 (after wave 13).** Nine parallel per-section audits of all 284 IDs compared the tree against re-audit 11, using the same method and the operator's RT-004 ruling; a tenth agent judged `fae task` and `fae-tui` after wave 13's fixes against the operator's request.
- **What ran:**
  - static reads, and `git diff wave13/base wave13/integration` for every file wave 13 touched;
  - pure-engine probes of every charge point and bound (re-audit 11's probes again, healing bindings, deferrals and refusals interleaved, a crash before launch, a Merge refused before its write), and the real `execution.Executor` with a stubbed runtime;
  - real git 2.56 through the accounted-git policy on throwaway repositories: re-audit 11's bypass shapes, `-C`, `--git-dir`, the `GIT_DIR` family, `gitdir:` files, `commondir`, absolute names;
  - `workflow.Load` and `workflow.Parse` on re-audit 11's documents, traced to what the start now does;
  - 28 landing filters through a real `go test -json`, re-audit 11's 23 two-run streams, the roll guard and the format gate on renames, type changes and odd names;
  - the leak census on real processes (a leader holding 150 unreaped children; a lazy reaper), and a copy of the redaction code on real `go test -json` streams cut at every offset of each escape form;
  - `fae` and `fae-tui` built into the scratchpad and run without a daemon; `fae-tui`'s model rendered against a fake daemon; `check-ledger.py --report`.
- **Where the findings are:** the per-section reports were not committed. The headline findings below were checked in source.

| | Met | Implemented-unproven | Partial | Missing | Contradicted | N/A |
|---|---|---|---|---|---|---|
| Before wave 1 | 77 | 34 | 131 | 32 | 8 | 2 |
| After wave 2 | 155 | 29 | 90 | 3 | 5 | 2 |
| After wave 3 | 206 | 22 | 50 | 1 | 3 | 2 |
| After wave 4 | 206 | 21 | 50 | 1 | 4 | 2 |
| After wave 5 | 208 | 21 | 49 | 1 | 3 | 2 |
| After waves 6 and 7 | 207 | 21 | 50 | 1 | 3 | 2 |
| After wave 8 | 207 | 21 | 50 | 1 | 3 | 2 |
| After wave 9 | 208 | 21 | 48 | 1 | 4 | 2 |
| After wave 10 | 209 | 21 | 48 | 1 | 3 | 2 |
| After wave 11 | 205 | 21 | 49 | 1 | 6 | 2 |
| After wave 12 | 206 | 21 | 50 | 1 | 4 | 2 |
| After wave 13 | **206** | 21 | 48 | 1 | 6 | 2 |

**What moved:** two IDs up, two down.
- **Up:**
  - **OPS-011 (Partial → Met).** Topology has the retired opt-in (`include_retired`, `fae topology --include-retired`), so every read OPS-011 names hides retired tasks by default.
  - **HOST-022 (Partial → Met).** A running command's defunct processes count after a two-census grace, and the alert names their parent: a leader holding 150 unreaped children raises at the third census.
- **Down:**
  - **HOST-006 (Met → Contradicted).** Wave 13 caps an agent turn's admission deferrals at 60 and then escalates ProviderUnavailable (`engine/engine.go:1054-1067`), first sessions included. HOST-006: "A deferral MUST be traced and MUST NOT fail the task." The escalated task leaves only by retire or a restart from the first stage. When admission refuses at once (the hard memory breaker, an unreadable Herdr inventory; `daemon/live_admission.go:251-257`) the cap is reached in about 5 to 10 minutes, not the "at least 35 minutes" the code's comment claims. ProviderUnavailable doesn't fit Appendix B either: no binding was tried.
  - **CTL-042 (Met → Contradicted).** `fae cleanup --apply` now writes a `worktree-cleaned` event and audit row per cleaned task, under the event-clock lock (`daemon/cleanup.go:483`, `controlplane/alerts.go:225-235`). CTL-042: cleanup "MUST perform only git operations and plane reads, so it cannot collide with the drive." daemon/CLEANUP.md still says it implements CTL-042.
- **RT-004 stays Contradicted, narrowly.** Every charge point re-audit 11 booked is fixed, and every loop ends (probes: 4 starts with a binding healed after each exhaustion, 61 later-session deferrals, SameGate after mixed refusals with varying detail). Still charged:
  - a crash while a live turn waits before launch (for a role slot or the host): recovery parks UncertainDispatch (`engine/engine.go:1528-1543`) though the admission journal shows nothing launched, and each redispatch costs an attempt (probe: 1, 2, 3, then DispatchRecoveryExhausted); older than wave 13;
  - a Merge refused before its write (`verifyActiveConflict`, snapshot and `PrepareReconcileFrom` errors, "requires workflow reverify route"; `daemon/live.go:686-728`) and VcsCommit's `verifyActiveConflict` error park UncertainDispatch, and redispatch charges them; HANDOFF's "Merge's own steps … are refunded" covers only `Launching` and the tree measurement;
  - the broker's launch gate: `started = cmd.Process != nil` is true for the stop-gate shell, so a `GateError` is charged (`effects/broker.go:383`, `processowner/process.go:263-320`);
  - a non-launch whose lease release or retirement write then fails becomes uncertain (`execution/executor.go:232-238`, `daemon/live.go:515-518`);
  - a provider-loop turn that never started is still not relaunched.
- **Contradicted:** RT-004, HOST-006, CTL-042; GEN-010 and SEC-006 (off-host backup replication, an open decision); PLAN-001 (historical).
- **Still missing:** CLI-051.
- **Re-audit 11's findings:** confirmed fixed in source and by probe, except as listed here: the nested git `n/../l/..` bypass (every shape refused end to end, the operator's `.git` unchanged), both unbounded reviewer reruns, Fae's own launch failures and the `Launching` write, never-started test runs, Freeze, Compare and integration, commit refusals before any write, priority 0 on `fae task create`, the `go test -run` filter at landing, renames in the roll guard, type changes in the format gate, redaction across a cut inside an escape unit and of HTML references in `-json` output, the TUI's stored pending-delete marker and `expected_version`, Topology on an unrunnable revision, the paired refused-before-launch line, cleanup paging and the restart check's budget.

New findings that change no rating:
- **WF-093 (the coordinator's refinement, not yet confirmed by the operator):** it complies with WF-093's text, and no path lets a quarantined task reach the engine. But:
  - a refused start isn't write-free: the plane's schema migration and two lifecycle events run before the revision check (`daemon/plane.go:110`, `daemon/daemon.go:358-359` before `:384`). The committed binary (e547c03) has schema 6 and refuses a newer one; this tree has 11. So on an instance it created, the refusal's own advice ("start the binary that wrote the revision again") stops working once this binary has tried;
  - the "one-shot" `--quarantine-unrunnable-revisions` survives a roll or maintenance relaunch, which replays the daemon's own arguments (`daemon/controller.go:169`, `:215`); and the quarantine it makes is rebuilt at each start, so a crash or `run --start` refuses again until the flag is repeated;
  - a Done task with an unsent ticket counts as an active pin (`daemon/stored_jobs.go:18-38`); under the flag its only exit is `quarantine retire`;
  - an instance the committed binary created that still has an active task on its old default refuses to start without the flag ("so instances the committed binary created can still start" holds only when no active task is pinned).
- **Landing filter (CTL-030, already Partial):** a named test that skips isn't counted (`controller/gates.go:342`), so `TestA$|TestAB` lands with `TestAB` skipped, contradicting daemon/BACKLOG.md's "Every test named must run and pass"; before wave 13 the filter named one exact test, so a skip held. A plain name stored before wave 13 now matches by substring. Admission compiles the whole filter as one expression (`daemon/admission.go:381`), so `\Q/\E` is admitted, but `go test` refuses it level by level and the task can never land.
- **Cleanup and topology (CP-009, already Partial):** skipped Done tasks count toward `--limit` and are never marked (`daemon/cleanup.go:389`, before the skip at `:391-398`), so with 200 permanently skipped tasks early in id order every run plans the same set and never reaches the rest. Topology now always reads Retired tasks, and they count toward its 10,000-task bound (`daemon/topology_cache.go:150`), so a long-lived molecule can fail the default page.
- **Bounds:** the total start bound is checked after the start (`engine/startup.go:111`), so a turn makes 3 × bindings + 1 starts, possibly all on one binding; it and the deferral cap escalate ProviderUnavailable, which Appendix B defines as every binding tried past the route-exhaustion bound. A crash during a later reviewer's 30-minute wait loses the held reviews, and the rerun is charged.
- **Live task editing and `fae-tui` (the tenth agent):** six of re-audit 11's eight bugs are fixed and one partly: a collected delete still waits behind a full tasks-in-flight cap unless the drive round reaches it first (`daemon/drive.go:423-430`). `test_shape` is still consumed by nothing. Neither TaskGet nor the TUI shows which structured values the turn in flight uses, and the old values are tested only at the dispatch-admission level. The TUI's per-view polling leaves the Attention badge and the pending labels stale outside their view; a stale-version reload drops what was typed; a pending delete after a restart reads as finishing on its own, though it needs `resolve-uncertain`. Readiness: usable for supervised work on a small Go repository with the CLI beside the TUI; none of it has run against real agent turns.
- **Smaller:** `fae schedule create --priority 0` is stored as 10 (`cron/cron.go:53-55`; older); `controller.Git` trims its output, so a first listed name that begins with whitespace loses it in the roll guard (`controller/deployment.go:53`); a secret holding `"` leaks at one cut offset in kept output (`daemon/secret_redaction.go:401-405`; older, unlisted), and `secretEscapeWidth` adds `len("&")` where the six-byte `\u0026` was meant (the file's comments lost the escape too); a lazy reaper inside a running command can hold up to about 200 zombies unalerted; admission's refusal lines multiply during a later session's 30-minute wait (OBS-006); the bridges' `WaitDelay` has no test; `fae cleanup` keeps writing when a maintenance marker is set mid-run (API-011); the accounted shim checks a passed `Dir` without changing into it (only tests pass one).
- **Doc claims the code doesn't support:** "a collected delete retires under the tasks-in-flight cap"; "Merge's own steps … are refunded" and "refunds the rest"; "the check writes nothing first" and "a refused start writes no revision or task row" (true for workflow and job rows only); "one-shot"; "at least 35 minutes"; "at most 3 agent starts per declared binding per turn"; "the residue is listed concretely"; "Every behaviour change has a test" (`WaitDelay`); daemon/CLEANUP.md's "implements FAE-CTL-042" and "the next run continues"; daemon/READS.md's "never fails the page"; daemon/BACKLOG.md's "Every test named must run and pass".

**Previous re-audit, 2026-10-30 (after wave 12; superseded above).** Nine parallel per-section audits of all 284 IDs compared the tree against re-audit 10, using the same method and the operator's RT-004 ruling; a tenth agent judged wave 12's live task editing and `fae-tui` against the operator's request.
- **What ran:**
  - static reads, and `git diff wave12/base wave12/integration` for every file wave 12 touched;
  - pure-engine probes of every charge point and bound (later-session deferrals, start exhaustions with a healing binding, refusals before launch mixed with charged failures, never-started effect runs redispatched), and the real `execution.Executor` with a stubbed runtime;
  - the real `Broker.Integrate` with seven pathspec-magic file names; real git 2.56 through account-mode wrappers on throwaway repositories (hook re-enable routes, `worktree repair`, links in `.git`);
  - `workflow.Load` and `workflow.Parse` on 21 documents, and tasks pinned to the ones `Load` accepts;
  - the real `scratchfs` (2,000 top swaps, 300 subdirectory swaps), `Policy.Prepare` on 56 `-run` patterns, the leak census on 150 unreaped children, and a copy of the redaction code on real `go test -json` streams with secrets at every offset of test2json's 1 KiB cut;
  - the gate functions on 23 crafted two-run streams; `fae` and `fae-tui` built into the scratchpad and run without a daemon; `fae-tui`'s model rendered against a fake daemon; `check-ledger.py --report`.
- **Where the findings are:** the per-section reports were not committed. The headline findings below were checked in source.

| | Met | Implemented-unproven | Partial | Missing | Contradicted | N/A |
|---|---|---|---|---|---|---|
| Before wave 1 | 77 | 34 | 131 | 32 | 8 | 2 |
| After wave 2 | 155 | 29 | 90 | 3 | 5 | 2 |
| After wave 3 | 206 | 22 | 50 | 1 | 3 | 2 |
| After wave 4 | 206 | 21 | 50 | 1 | 4 | 2 |
| After wave 5 | 208 | 21 | 49 | 1 | 3 | 2 |
| After waves 6 and 7 | 207 | 21 | 50 | 1 | 3 | 2 |
| After wave 8 | 207 | 21 | 50 | 1 | 3 | 2 |
| After wave 9 | 208 | 21 | 48 | 1 | 4 | 2 |
| After wave 10 | 209 | 21 | 48 | 1 | 3 | 2 |
| After wave 11 | 205 | 21 | 49 | 1 | 6 | 2 |
| After wave 12 | **206** | 21 | 50 | 1 | 4 | 2 |

**What moved:** five IDs up, two down. *(Wave 13 fixed the findings behind both drops and most of the new defects below; Since re-audit 11, below, lists what it left.)*
- **Up:**
  - **WF-074 (Contradicted → Partial).** A test run, Freeze, Compare, Merge or commit refused before it starts is refunded, and every limit still bounds (a mix of refunds and charged failures ends in TestRunCap). Still charged: never-started effect runs the executor doesn't type (below, under RT-004).
  - **WF-093 (Contradicted → Partial).** The start refuses a revision this binary can't read, naming it, its hash, its pinned tasks and the recovery, and every revision is read. But `workflow.Load` only decodes, so a revision this binary can't run is listed, not refused (below).
  - **RT-009 (Partial → Met).** Provider-loop turns record provider health.
  - **HOST-006 (Partial → Met).** A later session's admission deferral is a typed deferral that charges nothing (its loop is booked under RT-004).
  - **SEC-005 (Partial → Met).** Every git call that takes a task file name takes it literally; with the real `Broker.Integrate`, all seven magic names land only when the ref is free and are refused when it moved.
- **Down:**
  - **CLI-002 (Met → Partial).** `fae task create --priority 0` (or `"priority": 0` in `--file`) is admitted at 10: the flag defaults to 0 (`cmd/fae/tasks.go:82`) and TaskCreate maps 0 to 10 (`daemon/tasks.go:75-77`). Every other path refuses it.
  - **HOST-022 (Met → Partial).** Wave 12 subtracts every defunct process inside a running brokered command from the bound of 20 (`processowner/census.go:290-298`, `daemon/leaks.go:98-120`): a command holding 150 unreaped children raises nothing. The self-host alerts' cause is still unproven.
- **RT-004 stays Contradicted.** Re-audit 10's six charge points and the shared start bound are fixed. Still charged or unbounded:
  - Fae's own launch failures (`runtime/direct.go:315-420`) and a failed `Launching` write (`execution/executor.go:196-201`) record MalformedOutput against the binding and park UncertainDispatch; redispatch charges an attempt (`engine/uncertain.go:36-42`). Documented as open.
  - Never-started effect runs: every failure after `daemon/live.go:539-545` is uncertain, including the broker's refusals before exec and the intent write; Merge's own `Launching` and tree failures are `execution-error`; a commit refused before any write escalates unrefunded. Three never-started test runs, redispatched, spent 3 attempts and 3 test runs.
  - **New, unbounded:** a later review session's admission deferral refunds and parks 5 s with no counter (`engine/engine.go:1043-1058`), and each retry reruns every earlier reviewer as a real provider turn. Probes: 100, 300 and 4,999 cycles with no escalation. Wave 11 parked this for the operator.
  - **New, unbounded for later reviewers:** the per-binding start count resets at each exhaustion (`engine/startup.go:76`), any OK from another task heals the binding at once (`providerhealth/health.go:51-56`), and the route clock starts only when no route is eligible (`engine/engine.go:798-801`). A probe ran an earlier reviewer's turn 600 times at 0 attempts. Before wave 12 the attempt cap ended it.

  So HANDOFF's "so nothing loops" and engine/README.md's "Bounds keep each path finite" don't hold for these two paths.
- **Contradicted:** RT-004 (above); GEN-010 and SEC-006 (off-host backup replication, an open decision); PLAN-001 (historical).
- **Still missing:** CLI-051.
- **Re-audit 10's findings:** confirmed fixed in source and by probe, except as listed here: the charge points and shared start bound, integrate and reconcile pathspecs, hook re-enable (every route tried), `worktree repair` with no paths, the census shapes, the 1 KiB redaction cut for plain forms and the new encodings, the flake rerun's namesake and empty run 2, loop health, `scratchfs` swaps (0 of 2,000), the whole-suite `-run` patterns, `quarantine release`, READS.md and the Generation claim.

New findings that change no rating (wave 13 fixed most of them; Since re-audit 11, below, lists what it left):
- **Nested git (MODE-002, EXT-001; already Partial): wave 12's physical path resolution is bypassed.** `accountedGitResolve` follows links only while components exist and hands the rest to the textual `resolve` (`commandshim/accounted_git.go:997-1018`). git creates the missing directory and the kernel then follows the link. With `scratch/l` linked to the operator's `.git/refs`, `git init scratch/n1/../l/../PWN` created `.git/PWN`, `format-patch -o scratch/nn/../l/../hooks` wrote a file into `.git/hooks`, and `worktree add` went through. This contradicts commandshim/README.md, LIVE.md, effects/README.md and HANDOFF, and isn't in "Not fixed".
- **WF-093's narrowing (integration, not yet confirmed by the operator):** right in principle (the spec asks that a revision stay usable by its in-flight tasks, not that it pass later rules), too permissive in detail:
  - a Gate with no `expr` passes `Load`, and a task pinned to it panics `engine.PrepareWith` (`engine/engine.go:710`), a drive panic at every re-arm; `start: nowhere` passes too (only plane damage can store either, since every write validates);
  - the old default's `main` brief keys pass, but a live task on them is refused before launch at intake four times, then SameGate (`execution/executor.go:455-462`), as since wave 3; so "tasks pinned to it run on the document stored with them" is true only for scripted tasks;
  - Topology `Parse`s each molecule's revision and fails the whole page on one error (`daemon/records_reads.go:175`, `daemon/topology_cache.go:179-181`, `:212-213`), so `fae topology` and the TUI's Molecules view are empty on such an instance;
  - `installDefault` writes before the check refuses (`daemon/daemon.go:364` vs `:373`).
- **Live task editing and `fae-tui` (the tenth agent):** largely delivered: create, read, update and delete of tasks and edges while the daemon runs, each an audited, idempotent, authorized RPC with a `fae task` verb; delete composes Escalate and Retire only; the TUI shows molecules and their stages' tasks, the DAG with states, a task's description and stage trace, and the escalated tasks and alerts, and writes only with `--write`. Bugs:
  - **the test filter is validated and documented as a regular expression, but landing runs `-run ^QuoteMeta(f)$` and counts only a test named exactly `f`** (`daemon/admission.go:381`, `daemon/land.go:232-233`, `controller/land.go:41`, `controller/gates.go:227`): `^TestA$` or `A|B` runs every stage and is held at landing;
  - the TUI guesses "delete pending" from the escalation reason text (`tui/data.go:415-418`): wrong for a failed delete, for an escalation whose reason starts `TaskDelete:`, and after a restart;
  - TUI updates and deletes send no `expected_version`; declared files, test filter and test shape are refused mid-turn rather than applied from the next stage; the `test_shape` field is read nowhere at run time; the create form can't make a Normal-mode task.
- **Landing and controller:** the roll's overlap guard runs `git diff --name-only` without `--no-renames`, so a renamed file escapes the hold (`daemon/controller.go:391`, `:418`; older than wave 12). The format gate's `--diff-filter=AM` skips a symlink replaced by a Go file (`controller/land.go:737`). The stricter flake rerun held every crafted failure except an intermittent `TestMain` exit in the flake's own package.
- **Redaction (SEC-003):** a 1 KiB cut inside an escape unit (`%XX`, `\xNN`, `\uXXXX`) still leaks in the landing's kept output (the join is tried only on a plain-character mismatch, `daemon/secret_redaction.go:372`); HTML references leak in `-json` output, where `&` is `&` (older).
- **Smaller:** a later session's provider failover stays charged while a first session's is refunded (`engine/engine.go:1101-1104`; a decision to confirm); the bridges set no `WaitDelay` on the provider command; `cleanup done` reads every Done task's payload and stops working above 100,000; the TUI polls six RPCs every 2 s, including the whole-graph BacklogShow; the restart path still gives the paged plane check one 10 s budget; topology has no retired opt-in (OPS-011); `commandshim/role.go:228` and two `errors.New` refusals in accounted_git.go are outside the census; `refused-before-launch` failures have an event but no log line.
- **Doc claims the code doesn't support:** "a `..` after a link no longer escapes"; "so nothing loops" and "Bounds keep each path finite"; "a test run, Freeze, Compare, Merge or commit refused before it starts … refunds" (typed refusals only); "tasks pinned to it run on the document stored with them"; "the flake rerun hides nothing" (nearly); daemon/TASKS.md's "scope the turn in flight" for the test filter and shape, and "retire still ends it" during a pending delete; tui/README.md's test-shape and pending-delete claims; test.yml's 42-minute daemon estimate (now 87).

**Since re-audit 11 (wave 13, 2026-10-31; re-audited above).**
- **Fixed:**
  - nested git: call-named paths are checked as git creates them, so a missing directory before a link no longer escapes, and a dangling link is refused; the remaining uncensused refusals are in the census (MODE-002, EXT-001, VCS-002/-010, VER-001);
  - the two unbounded reviewer reruns: agent starts per turn are capped over all bindings, admission deferrals per turn are capped, and a later session waits for the host instead of rerunning earlier reviewers (RT-004, HOST-006);
  - never-executed runs: the runtime's own launch refusals, a failed `Launching` write, effect runs refused before their command existed, Merge's own steps and commits refused before any write are refunded (RT-004, WF-074); refusals before launch have a log line (OBS-006);
  - WF-093: an unrunnable revision with active tasks refuses the start, with a serve flag that quarantines them; the check writes nothing first (re-audit 12: no workflow or job row; the schema migration runs first); Topology tolerates such a molecule and has a retired opt-in (OPS-011);
  - HOST-022: a running command's old defunct processes count again, and the alert names their parent; CLI-002: `fae task create` refuses a priority below 10;
  - the test filter is read as `go test -run` reads it; the roll's overlap guard sees renames; the format and own-hunk gates handle type changes; redaction across a cut inside an escape unit and of HTML references in `-json` output (CTL-001, CTL-030, SEC-003);
  - CP-009: `fae cleanup` pages and remembers what it cleaned; CP-006: a plane restart records the outage before its check; the TUI reads the stored pending delete, sends versions, creates Normal-mode tasks and polls less.
- **Not fixed:** nested git's check-then-exec race; programs a test repository's own configuration names, and programs git and gcc find on PATH; Herdr transport-local start failures count toward the start bound; a bridge crash between edit renames leaves part of a turn's edits; nested calls have no concurrency bound; `test_shape` is consumed by nothing; non-Go repositories still can't use derived scope, the format stage or QA counting.

Details are under Completed in wave 13.

**Previous re-audit, 2026-10-30 (after wave 11; superseded above).** Nine parallel per-section audits of all 284 IDs compared the tree against re-audit 9, using the same method; a tenth agent assessed readiness for normal use. The audit applied the operator's RT-004 ruling of 2026-10-29: a run that never executed must not count towards the attempt budget.
- **What ran:**
  - static reads, and `git diff wave11/base wave11/integration` for every file wave 11 touched;
  - real git 2.56 against throwaway repositories: an "operator" repository with linked task worktrees and test repositories (re-audit 9's 19 network vectors again, a test repository's own hook configuration, a planted `.git/refs` link toggled 150 times, `worktree repair`);
  - the real `Broker.Integrate`, `scratchfs.RemoveAll` and `Quarantine`, `RecoverClearing`, `Policy.Prepare` and `PrepareAccounted` (37 `-run` patterns, 20 environments), and `workflow.ProtectedPath`/`DeclaredPathSafe` (107 paths) on throwaway inputs; a real cgo build with `CC` pinned;
  - verbatim copies of the RED parser and the redaction matcher on real Go 1.27.2 `go test -json` streams and 63 synthetic encodings;
  - pure-engine probes of every charge point, of refused-route parks across restarts and Resume, and of start failures mixed across reviewers;
  - the gate functions on 13 crafted two-run streams, CLI probes of a scratchpad `fae` binary, and `check-ledger.py --report`.
- **Where the findings are:** the per-section reports were not committed. The headline findings below were checked in source.

| | Met | Implemented-unproven | Partial | Missing | Contradicted | N/A |
|---|---|---|---|---|---|---|
| Before wave 1 | 77 | 34 | 131 | 32 | 8 | 2 |
| After wave 2 | 155 | 29 | 90 | 3 | 5 | 2 |
| After wave 3 | 206 | 22 | 50 | 1 | 3 | 2 |
| After wave 4 | 206 | 21 | 50 | 1 | 4 | 2 |
| After wave 5 | 208 | 21 | 49 | 1 | 3 | 2 |
| After waves 6 and 7 | 207 | 21 | 50 | 1 | 3 | 2 |
| After wave 8 | 207 | 21 | 50 | 1 | 3 | 2 |
| After wave 9 | 208 | 21 | 48 | 1 | 4 | 2 |
| After wave 10 | 209 | 21 | 48 | 1 | 3 | 2 |
| After wave 11 | **205** | 21 | 49 | 1 | 6 | 2 |

**What moved:** six IDs down, none up. *(Wave 12 fixed the findings behind all six; Since re-audit 10, below, lists what it left.)* No wave-11 code change made a requirement worse. Three drops follow the RT-004 ruling, one judges a wave-11 decision against the spec, and two are older gaps found this time.
- **Under the RT-004 ruling:**
  - **RT-004 (Partial → Contradicted).** Runs that provably never executed still consume the attempt budget in six places:
    - a later review session's start failure: `engine/startup.go:40-44` refunds only when no reviewer ran;
    - a later reviewer's binding refusal: `engine/route_refusal.go:225-227`;
    - a later session's admission deferral, which parks UncertainDispatch (`execution/executor.go:193-201`), after which `redispatch` charges again (`engine/uncertain.go:28-42`);
    - the bridge's or the daemon's refusal before the provider launched, which is ValidationFailed with no refund (`engine/engine.go:1106-1114`);
    - an untyped failure before any launch (`engine/engine.go:1125`);
    - a provider that wrote nothing before the bridge's timeout, typed `provider-failed` (`provider/codex.go:482-493`), although RT-004 names "the prompt was never acknowledged" as a non-execution.

    The tests pin the charges (`Attempts == 4` in `TestDaemonLaterReviewerThatNeverAcknowledgesRerunsEveryReviewer`). WF-033 and WF-034 keep their own clauses; their charge points are counted once, here.
  - **WF-074 (Met → Contradicted).** A test run, Freeze, Compare, Merge or commit refused before anything starts is ValidationFailed with no refund: four such refusals spend 4 attempts and 4 of the task's 8 test runs, then SameGate (`daemon/live.go:452-461`). The per-task limits themselves hold as ruled.
  - **HOST-006 (Met → Partial).** A later review session whose pane or memory admission defers parks UncertainDispatch instead of a typed MemoryReservation or PaneLimit deferral (`execution/executor.go:197-199`).
- **WF-093 (Partial → Contradicted).** Wave 11 parses every stored revision at start and, by documented decision, only reports an unloadable one. WF-093 says the binary MUST refuse to start naming it (`daemon/startup_checks.go:128-193`). Revisions past the 30 s bound go unchecked.
- **RT-009 (Met → Partial), an older gap.** Provider-loop turns record no provider health (`daemon/loops_live.go` has no `Observe`).
- **SEC-005 (Met → Partial), an older gap.** A model-chosen file name still reaches git as a pathspec in integration and reconcile (`effects/reconcile.go:78`, `:306-334`; `GIT_LITERAL_PATHSPECS` is set only for snapshot staging, `effects/broker.go:754`). With the real `Broker.Integrate`, a task commit adding a file named `:(exclude)*` (accepted by `DeclaredPathSafe`, `ProtectedPath` and `CheckArg`) returned `Landed:true` while the integration ref didn't move, so the task would go on as integrated with its work lost.
- **Contradicted:** RT-004, WF-074 and WF-093 (above); GEN-010 and SEC-006 (off-host backup replication, an open decision); PLAN-001 (historical).
- **Still missing:** CLI-051.
- **Re-audit 9's findings:** confirmed fixed in source and by probe. That covers the RED regressions (cut-off tests, goroutine stacks, `expected error`, bare `go:N`), the nested git routes (`GIT_TEST_FSMONITOR`, trace2 and `format.outputDirectory` targets, links inside `.git`, the `-C` swap), the refused-route park (spin, clock, Resume, failover starts, Prophet), read-only scratch at start, the listed `-run` patterns, cgo's `CC`/`CXX`/`FC`/`PKG_CONFIG`, the listed redaction forms, the flake explanation, `workflow-put`, and the smaller gaps.

New findings that change no rating (wave 12 fixed most of them; Since re-audit 10, below, lists what it left):
- **Nested git (MODE-002, EXT-001, VCS-002/-010):**
  - **A call can switch hooks back on.** git reads `-c` after `GIT_CONFIG_COUNT`, and on a writable repository the policy passes `hook.<event>.enabled=true` (`commandshim/accounted_git.go:1325-1387`; only protected repositories get the false-only rule, `:1763`). With a test repository's own `[hook]` configuration, `git -c hook.pre-commit.enabled=true commit` ran a hook that moved a stand-in operator repository's `main`. It adds no power beyond a test's own filter driver (documented), but "no hook runs in any nested call" is false.
  - **`git worktree repair` with no paths** rewrites the `.git` file of every worktree the test repository lists, another task's included: only positional paths are checked (`:874-890`).
  - **The check-then-exec race:** toggling a planted `.git/refs` link got 36 of 150 calls through (re-audit 9: 25).
  - **Census gaps:** three `&LocalRefusal{…}` sites in accounted_git.go and the subtest refusal in `daemon/live_red_subtests.go` sit outside the new census, which covers only `refuse(` and `liveScopeRefusal(` call shapes.
- **A new wave-11 bug in the start bound (RT-004):** the per-stage start counter mixes reviewers' bindings, and exhaustion charges the binding of the session that failed last. Two failed starts of one reviewer's binding, then one of another's, charge the second binding Unavailable after a single failure (`engine/startup.go:40-55`, `daemon/drive.go:751-762`). The default workflow is unaffected (its roles share routes).
- **Redaction (SEC-003):** `go test -json` splits an output line over 1 KiB into events with no newline, and the matcher joins events only at a newline (`daemon/secret_redaction.go:416-484`). A secret across the cut keeps both halves in the landing's kept gate output; the engine's excerpt is safe (redacted again after decoding). Also unmatched: `%q`/`%U` of runes, `%o`/`%b` of bytes, base32hex, UTF-16, and a JWT with an escaped `\n`.
- **Landing (CTL-030):** the flake rerun skips the flake's name in every package, so a namesake elsewhere goes unchecked, and it doesn't compare run 2's executed count (a run 2 with zero tests passes).
- **Host:** `scratchfs` checks the top path, then chmods and opens it by path; swapping in a link between those steps made it chmod an outside directory's subdirectory in 12 of 2,000 races (modes only; `scratchfs/scratchfs.go:49-63`). Whole-suite `-run` values off the fixed list (`Test.*`, `(.*)`, `^Test.*`) still get 2 threads. With `CC` pinned, gcc still runs `as` and `ld` from PATH and from `COMPILER_PATH` (documented).
- **RED (WF-073), adversarial:** a `TestMain` that never calls `m.Run` but prints test2json framing lines makes a test that never ran count as executed and failed.
- **Startup (CP-006):** `quarantine release` doesn't repair the task row, so a released task is quarantined again at each start until it checkpoints; the divergence query stops at 1,000 tasks; a failed or slow check also refuses the start.
- **Doc claims the code doesn't support:** "no hook runs in any nested call"; "declared paths reach git as literal paths"; "refusing it only when the event clock is behind"; "a new rule fails the census until a test fires it" (two call shapes only); "every flag with a spec default" (`runtime-config` limits and `--weight` refuse); `daemon/READS.md` omits `startup_checks`; README.md says `defaults.Generation` is 2 (the code says 3).

**Readiness for normal use** (the tenth agent; checked in source):
- **Usable narrowly:** small, tightly specified tasks (a few declared files, a stated test name and failure text) in a **Go** repository whose whole `go test ./...` passes, is hermetic, and finishes well inside 15 minutes run one package at a time. Run it on a clone through `scripts/self-host.py`, then fetch, review and merge by hand. The two real tasks that reached Done took 879 s (12 Opus turns) and 1,100 s (14), with no intervention; Claude Code reported about $8 and $11. Whole-suite QA took 61–68% of the wall time.
- **Not usable on non-Go repositories:** derived scope needs a Go test name and exactly one `_test.go` (`daemon/live_scope.go:246`, `:431-437`, `:519`); the only non-test command is `gofmt -l .` (`daemon/live.go:613-614`); QA counts only `go test -json` or TAP output; a script or runner RED needs a pin in the hash-pinned live configuration, set before the instance's first start.
- **Not for untrusted repositories or task text:** model-written tests run as the operator, with HOME and network, before any review.
- **Unproven on this tree:** waves 9–11 reworked RED evidence, nested git and edit writing, and no real task has run since wave 8.
- **Practical:** `cmd/fae-claude-bridge` is in no commit (`origin/main` can't build the default provider); finished tasks' worktrees and `fae/task/*` branches are never removed (cleanup covers Retired tasks only, and only Escalated tasks retire; 86 MB on the wave-8 instance) *(wave 12: `fae cleanup done` removes them once integrated, and `fae task delete` retires any non-terminal task)*; every self-host run raised 2–4 `process-leak` error alerts during QA *(wave 12 stops counting defunct processes inside running commands; unconfirmed live)*; each instance's base is fixed, so tasks never see newer commits.

**Since re-audit 10 (wave 12, 2026-10-30; re-audited above).**
- **Fixed:**
  - the RT-004 ruling: a later reviewer's start failure, binding refusal and admission deferral, refusals before launch (agent turns and host commands) and a provider silent until the bridge's timeout no longer consume attempts or test runs; the agent-start bound is per binding; the bridge enforces the 180 s acknowledgement bound (RT-004, WF-074, HOST-006);
  - task file names reach git as literal paths in integration, reconcile, conflict resolution and the landing, so a file named `:(exclude)*` can't fake a landing (SEC-005);
  - nested git: hooks can't be switched back on, `worktree repair` checks listed worktrees, and paths are resolved physically (a `..` after a link no longer escapes; re-audit 11: it still does when a missing directory comes first); the census covers every refusal shape (MODE-002, EXT-001, VCS-002/-010, VER-001);
  - redaction across test2json's 1 KiB event cut and the extra encodings; the flake rerun must run what the suite ran (SEC-003, CTL-030);
  - WF-093: a stored revision this binary can't load refuses the start (operator decision); one that loads but fails a later rule is listed; CP-006's start check pages through every task, compares the backlog rows and runs before any event; release repairs rows; provider loops record health (RT-009); scratchfs works from directory handles; killed gate-run scratch is swept; whole-suite `-run` patterns are recognised more generally.
- **New features (operator requests):** live `fae task create|update|delete|show` with matching RPCs (daemon/TASKS.md), `fae cleanup done`, and the `fae-tui` terminal UI (tui/README.md).
- **Not fixed:** nested git's check-then-exec race; programs a test repository's own configuration names, and programs git and gcc find on PATH; Fae's own launch failures are charged to the provider; a bridge crash between edit renames leaves part of a turn's edits; nested calls have no concurrency bound; non-Go repositories still can't use derived scope, the format stage or QA counting.

Details are under Completed in wave 12.

**Previous re-audit, 2026-10-29 (after wave 10; superseded above).** Nine parallel per-section audits of all 284 IDs compared the tree against re-audit 8, using the same method.
- **What ran:**
  - static reads, and `git diff wave10/base wave10/integration` for every file wave 10 touched;
  - the real nested-call wrappers (`fae heavy-op run --shim-policy`) with real git 2.55 against throwaway repositories: an "operator" repository with linked task worktrees, test repositories in a run TMPDIR, a bare upstream, and remotes at `127.0.0.1:9`, `ext::` and `fd::` (about 60 calls by one auditor, 19 network vectors by another, 58 `-c` keys);
  - `PrepareAccounted` and `Prepare` on go argument vectors and 73 environments;
  - verbatim copies of the broker's RED parsing and the reason rule on real Go 1.27.2 `go test -json` streams (hangs, `os.Exit` from a `TestMain` goroutine, a SIGKILLed test binary, an `httptest` handler panic, `debug.PrintStack`, `t.Error("expected error")`; about 75 fragments);
  - pure-engine probes of refused-route parks across restarts, Resume and provider failover;
  - `Recover` on planted registries, the gate functions on crafted streams, the redaction matcher on synthetic encodings, and CLI probes of a scratchpad `fae` binary.
- **Where the findings are:** the per-section reports were not committed. The headline findings below were checked in source.

| | Met | Implemented-unproven | Partial | Missing | Contradicted | N/A |
|---|---|---|---|---|---|---|
| Before wave 1 | 77 | 34 | 131 | 32 | 8 | 2 |
| After wave 2 | 155 | 29 | 90 | 3 | 5 | 2 |
| After wave 3 | 206 | 22 | 50 | 1 | 3 | 2 |
| After wave 4 | 206 | 21 | 50 | 1 | 4 | 2 |
| After wave 5 | 208 | 21 | 49 | 1 | 3 | 2 |
| After waves 6 and 7 | 207 | 21 | 50 | 1 | 3 | 2 |
| After wave 8 | 207 | 21 | 50 | 1 | 3 | 2 |
| After wave 9 | 208 | 21 | 48 | 1 | 4 | 2 |
| After wave 10 | **209** | 21 | 48 | 1 | 3 | 2 |

**What moved:** one ID up.
- **RT-010 (Contradicted → Met).** When every route is refused, the task parks `all-routes-unavailable` with the refusals named, spends no attempt, resumes in a new route generation or on Resume, and escalates ProviderUnavailable only past the 30-minute bound (`engine/engine.go:775-812`, `engine/route_refusal.go`). Defects in the park are below.
- **Contradicted:** GEN-010 and SEC-006 (off-host backup replication, an open decision); PLAN-001 (historical).
- **Still missing:** CLI-051.
- **Re-audit 8's findings:** confirmed fixed. That covers the RED timeout trap, digit-only reasons, the multi-reviewer refund, HOME/XDG configuration and `-c` program keys in nested git, nested git's network reach (no transport reached a remote through the wrapper), the nested go environment, `-cpu`, `-args` and `all`/`std`, configured and environment secrets in every form, the CTL-030 false pass, the empty process record, the rollback note, the stored Claude/`minimal` revision, and the smaller gaps.

New findings that change no rating (wave 11 fixed most of them; Since re-audit 9, below, lists what it left):
- **Nested git can still change the operator's repository (MODE-002, EXT-001, VCS-002/-010).** Each was shown with the real wrapper and git 2.55; the first two also pass on the wave-9 tree.
  - `GIT_TEST_FSMONITOR` names a program git runs on a read, and the policy doesn't check it (`commandshim/accounted_git.go:753-758`). A program run this way from `git status` in the protected task worktree planted a branch in the operator repository.
  - On a test repository, a call-named `GIT_CONFIG_GLOBAL` with `trace2.eventTarget=<operator>/.git/config` appended JSON to the operator's config, after which every git call there failed; `-c format.outputDirectory=<operator>/.git/…` wrote into its git directory. The trace refusal checks only `GIT_TRACE*`.
  - The policy resolves the common and git directories, objects, index and toplevel (`:372`, `:417`), not `refs/` or `worktrees/`. A test repository whose `.git/refs` or `.git/worktrees` links into the operator's moved its `main` or removed another task's worktree, and a symlink swapped between the policy's check and the exec won 25 of 150 races.
  - A hook installed by a nested `core.hooksPath` or `include.path` write ran the real git (documented in `commandshim/README.md`).
  - All of these need deliberate test code; this handoff's "loads no configuration or program the call chose" and "can't change the operator's repository" overstate.
- **RED evidence regressed in three ways (WF-036, WF-073; new in wave 10).**
  - **A test cut off from outside counts as failing, with whatever was printed as its reason** (`effects/broker.go:574-580`). A `TestMain` goroutine that printed `fixture server crashed: bind: address already in use` and called `os.Exit(1)` while the RED test slept satisfied `red-gate` with that fragment. A SIGKILLed test binary reports `signal: killed` under the running test, so an outside kill of the test binary is a charged failure, not uncertain as this handoff says.
  - **Matching stops at the first goroutine header** (`:604-614`). net/http's recovered-panic log (any `httptest` handler test) and `debug.PrintStack()` print one mid-test, so the test's own later message never matches and a correct RED loops to the test-run cap, pinned fragments included. Wave 9 matched the whole output.
  - **`error` is now a trace word** (`daemon/live_scope.go:214`), so `expected error`, `want error` and `got error` are refused, the common `t.Error("expected error")` idiom among them; a bare position `go:9` passes and matches, though the protocol says positions are refused.
- **The refused-route park (RT-010, RT-009, RT-004).**
  - The joined refusals are cut at 1024 bytes, which can split a UTF-8 character (`engine/route_refusal.go:72`, `engine/engine.go:1345`). The stored reason then never equals the recomputed one, so the task re-parks with an event, a plane commit and a drive wake-up every round until the 30-minute bound (probe: 5 events in 5 rounds).
  - A new route generation's dispatch clears the clock (`engine/engine.go:856`), so a refusal that recurs at every start never escalates if the daemon restarts within 30 minutes; "the clock survives a restart" holds only for health parks. Resume accepts a health-only park that carries an old generation's refusal (`engine/control.go:201`), against `engine/README.md` and `daemon/TASK_CONTROLS.md`.
  - A provider failover keeps the turn's start count (`engine/engine.go:1067-1075`), so the next binding can be charged Unavailable after one failed start.
  - **Prophet no longer charges a provider that never acknowledged the prompt** (regression): `ProviderNotStarted` counts as before-provider (`runtime/direct.go:143-145`), which Prophet treats as local (`daemon/prophet_routes.go:103-110`).
  - The route-exhaustion alert uses a fixed 30 minutes, and Resume doesn't restart its clock.
- **Host and startup.**
  - A run that leaves a non-writable tree in its scratch (Go's read-only module cache under a test's own `GOPATH`) isn't removed (the broker ignores the error, `effects/broker.go:287`), and the next start's sweep then refuses to start the daemon (`daemon/host_scratch.go:43`, `daemon/live.go:193`). Predates wave 10.
  - HOST-015: a `-run` value that matches every test (`-run=`, `.`, `.*`) is still a filter, so a whole suite runs with `-p=2`.
  - `CGO_ENABLED=1` with no `CC` runs whatever `gcc` a test puts first on PATH; nested calls still have no concurrency bound.
  - A controller- or operator-class `heavy-op run` under soft pressure refuses after the role re-check bound (`commandshim/heavyop.go:137`), against `hostguard/README.md`. Predates wave 10.
- **Redaction (SEC-003).** A secret split across lines leaks whole, not just its tail; `xxd`, `hexdump -C`, `od`, Go's `%#v` of a `[]byte`, base32 and HTML entities leak; so do a Codex refresh token and an OpenAI key without its marker. The provider's raw `events.jsonl` and `stderr.log` are kept unredacted.
- **Landing (CTL-030).** A failing listed flake explains any nonzero exit (`controller/gates.go:293`), so a package whose `TestMain` exits 1 next to a failing listed flake passes the suite gate. A count mismatch caused by redaction holds as a failing gate, not as infrastructure.
- **Smaller gaps:**
  - `fae workflow-put` returns `Parse`'s error before the RPC, so a document with another error lists only that one, not its Claude/`minimal` route.
  - `land --poll 1ns` is accepted; `fae workflow scenario` prints a fixture `fae workflow simulate` rejects; HANDOFF's "every flag with a spec default" still overstates (`runtime-config` limits, `gate run --weight`).
  - Rolling wave 10 back to wave 9 leaves a refused-routes park parked with no alert (wave 9 matches the old reason exactly); the rollback note doesn't say so.
  - `Recover`'s removal of an empty record leaves no audit row or log line.
  - "The widen dead ends are gone" overstates: a stored pre-wave-9 nested `.git` widen and an approved RED test file the bridge can't load still need Retire.
  - The protocol's refused-word list omits `error`, `name`, `go`, `in` and `local`; `daemon/READS.md` documents only the `molecule_limit` withheld reason.

Largest remaining gaps:
- **For self-hosting:** the RED regressions; the nested git holes; the refused-park spin and clock; the scratch sweep that blocks start; the HOST-015 `-run` thread count; no live run yet of the wave-9/10 nested policies (the Fae-on-Fae acceptance was last run on wave 8).
- **Operator decisions:** HOST-011 (the hook is registered nowhere); GEN-010 and SEC-006 (off-host replication); CLI-051; nested calls' unbounded concurrency; FanOut running sequentially (SCH-009).
- **Verification:** CI has never run; 5 ledger entries have unknown native evidence; the census still omits the nested git policy, derived-scope refusals, redaction and route refusals.
- **Unchanged:** the writer lock and checkpoint waits (ROB-020/-021), whole reads (CP-009), never-collected blobs (CP-012), the commit stage's Reverify route (ROB-013), a later review session's start failure parking UncertainDispatch (RT-004), schema 2 on write, partial bridge edits, API-010 and API-020, CTL-030's Go-only gates, unauthenticated Herdr sockets (SEC-001), derived scope's protected paths (SEC-005), and OBS-001's fields on controller lines.

**Since re-audit 9 (wave 11, 2026-10-29; re-audited above).**
- **Fixed, for self-hosting first:**
  - RED evidence comes only from a test's own result or go's alarm naming it; every goroutine stack is left out of matching; `expected error` is a reason again and bare `go:N` positions are refused; a derived RED that calls `t.Run` is refused; output over the byte bound is evidence (WF-036, WF-073);
  - nested git takes only known `GIT_*` variables, runs no hook or fsmonitor and traces nowhere a config file names, checks named config files, nested config writes and the files it writes, refuses links out of a writable repository, and runs on the repository it checked (MODE-002, EXT-001, VCS-002/-010);
  - the refused-route park can't spin, its clock runs until a route launches, Resume needs a refusal of the current generation, a failover gives fresh starts, and the alert follows the engine (RT-009/-010);
  - Prophet charges a provider that never acknowledged, a later review session's start failure reruns the reviewers, and output followed by a failure is a bounded retry (RT-004, RT-009, WF-034);
  - a read-only scratch tree no longer stops the daemon starting; a `-run` that matches every test is a whole suite; cgo's programs are pinned; a controller-class `heavy-op run` waits for `--max-wait` (HOST-020, HOST-015, HOST-014, HOST-005);
  - wrapped and encoded secrets, unmarked keys and OAuth token fields are redacted, raw provider logs included; a listed flake explains an exit only through a clean rerun; a redaction mismatch holds as infrastructure (SEC-003, CTL-030);
  - widens no longer leave Retire as the only way out; the bridge writes a turn's edits all or nothing; empty-record removal is audited; plane consistency and stored revisions are checked at start; model-chosen scope can't hold protected paths (WF-078, ROB-012/-014/-016, CP-006, WF-093, SEC-005).
- **Smaller gaps fixed:** `workflow-put` lists every issue; `land --poll` and the watchdog period have a 10 ms floor for every caller; `workflow simulate` reads `workflow scenario`'s output; `daemon/READS.md` documents every withheld reason; OBS-001 fields on `heavy-op run` and `gate run` lines; the ledger censuses nested git and derived-scope refusals (134 of 284 requirements mapped).
- **Doc claims:** re-audit 9's overstatements are corrected in LIVE.md, commandshim/README.md, effects/README.md, docs/SELF_HOSTING.md, daemon/README.md, engine/README.md, daemon/TASK_CONTROLS.md, daemon/READS.md and hostguard/README.md, and annotated in this handoff.
- **Not fixed:** nested git's check-then-exec race is narrowed, not closed; nested calls still have no concurrency bound; a later review session's admission deferral or stall still parks UncertainDispatch; a bridge crash between edit renames leaves part of a turn's edits; a Codex refresh token without its field name leaks; programs a test repository's own configuration names still run.

Details are under Completed in wave 11.

**Previous re-audit, 2026-10-28 (after wave 9; superseded above).** Nine parallel per-section audits of all 284 IDs compared the tree against re-audit 7, using the same method.
- **What ran:**
  - static reads;
  - real `go test` runs of tiny modules in a run environment built as the broker builds one, in both nested modes (29 nested-call cases each), and `PrepareAccounted` on 75 go and 58 git vectors;
  - the real git against throwaway repositories: an "operator" repository with a linked task worktree, a run TMPDIR and a bare upstream;
  - verbatim copies of the RED reason rule on real Go 1.27.2 failure streams (seven failure kinds, 67 fragments), of the redaction matcher on 36 synthetic encodings, and of the hook on 106 command lines;
  - a pure-engine probe of binding refusals in a multi-reviewer dispatch, and `SuiteGate` on a crafted redacted stream;
  - CLI probes of a `fae` binary built into the scratchpad;
  - a coordinator probe of `go test -json` on a test killed by `-timeout`.
- **Where the findings are:** the per-section reports were not committed. The findings below were checked in source.

| | Met | Implemented-unproven | Partial | Missing | Contradicted | N/A |
|---|---|---|---|---|---|---|
| Before wave 1 | 77 | 34 | 131 | 32 | 8 | 2 |
| After wave 2 | 155 | 29 | 90 | 3 | 5 | 2 |
| After wave 3 | 206 | 22 | 50 | 1 | 3 | 2 |
| After wave 4 | 206 | 21 | 50 | 1 | 4 | 2 |
| After wave 5 | 208 | 21 | 49 | 1 | 3 | 2 |
| After waves 6 and 7 | 207 | 21 | 50 | 1 | 3 | 2 |
| After wave 8 | 207 | 21 | 50 | 1 | 3 | 2 |
| After wave 9 | **208** | 21 | 48 | 1 | 4 | 2 |

**What moved:** two IDs up and one down.
- **WF-043 (Partial → Met).** The RED reason rule is in the kernel contract's output protocol on every bridge turn, Claude and Codex, resumed sessions included (`provider/codex.go:607`, `:657`). The stated and applied rules still differ at the edges (below).
- **WF-082 (Partial → Met).** A binding the bridge refuses before the provider runs fails over to the role's next route (`engine/route_refusal.go`, `engine/engine.go:771`). `TestDaemonBindingRefusalFailsOverToTheNextRoute` takes a task whose Codex route is refused to Done on Claude.
- **RT-010 (Met → Contradicted), a coordinator ruling.** When every route is refused, wave 9 escalates ProviderUnavailable at once (`engine/engine.go:771-775`, `engine/route_refusal.go:79-83`).
  - RT-010 says the action MUST park when no route is eligible, and Appendix B's ProviderUnavailable needs parks past the route-exhaustion bound. The decision is documented, so under the audit's rule it is Contradicted. One auditor judged it defensible.
  - A park would resume by itself after the daemon restart that starts a new route generation. The escalation needs restart with information from intake, on carried budgets.
- **Contradicted:** RT-010 (above); GEN-010 and SEC-006 (off-host backup replication, an open decision); PLAN-001 (historical).
- **Still missing:** CLI-051.
- **Re-audit 7's findings:** confirmed fixed, except where noted below. That covers a Go test's own `go` and `gofmt` in both modes (probed through `exec.LookPath`, `$GOROOT/bin`, `runtime.GOROOT()`, `go env GOROOT` and `go tool`), the go allowlist, nested git moving the operator's refs and worktrees (about 35 vectors, `GIT_DIR`, `commondir` and symlink redirects included), the `StartRole` race, the hard-link dependency, widens into `.git`, a widen breaking a frozen RED, local binding refusals, redispatch counters, nested-call argv and landing redaction, and the doc claims.

New findings that change no rating (wave 10 fixed most of them; Since re-audit 8, below, lists what it left):
- **A RED that fails by timing out can never produce evidence, and the protocol now suggests one (WF-073; new in wave 9).**
  - `go test -json` emits no test-level `fail` for a test killed by `-timeout`, only the package's (coordinator probe, Go 1.27.2). The broker refuses the stream as incomplete (`effects/broker.go:474-478`), and the live run parks UncertainDispatch (`daemon/live.go:526-530`). The RED stage's 600 s deadline also pre-empts go test's 10-minute alarm.
  - The protocol (`provider/codex.go:607`), `LIVE.md:98` and this handoff's wave-9 decision list `test timed out` as a valid reason. Wave 8 refused it at turn completion, a bounded retry.
  - The protocol's "nil map write" example isn't Go's message (`assignment to entry in nil map`).
- **The RED reason rule's residue changed shape (WF-073).**
  - Any token with a digit counts as content (`daemon/live_scope.go:266-267`), so `1` or `0` passes; in a probe, `1` matched 5 of 6 unrelated failures of one test. Bare hex (`0x`), `runtime error`, module and worktree path words and `t.Log` text also pass.
  - Still refused though legitimate: path-only messages (`testdata/missing.json`) and words inside the RED file's path.
  - A RED written with `t.Run` subtests can never pass `red-gate` (`executed_count` 2), and the protocol doesn't say so.
- **A later reviewer's binding refusal refunds a dispatch whose earlier reviewers ran (RT-004, WF-033).** The executor's binding-refused branch has no first-session guard (`execution/executor.go:303-311`), unlike startup retry and admission deferral, and `completeBindingRefused` ignores collected reviews (`engine/route_refusal.go:100-118`). A probe re-ran the first reviewer with Attempts 1 → 0. Bounded by the declared bindings per generation.
- **Nested git's "nothing injected" claim doesn't hold (MODE-002, EXT-001).**
  - A call that unsets `GIT_CONFIG_GLOBAL` and points `HOME` or `XDG_CONFIG_HOME` at a directory it wrote loads that configuration, even on a protected repository. `accountedGitInjectedEnv` doesn't check them (`commandshim/accounted_git.go:714-739`).
  - On a test's own repository, `-c` keys that name a program (`core.sshCommand`, `core.hooksPath`, `credential.helper`, `remote.<n>.uploadpack`) pass; `-c` is checked only on protected reads (`:519-526`). Programs git starts run the real git, and in a probe a hook pushed into a throwaway bare upstream.
  - Nested `clone`, `fetch`, `pull` and `ls-remote` reach any remote from a temporary repository or from none (GEN-010).
  - This handoff's wave-9 "Left open" names hooks and `core.sshCommand`, but "forms that have git run a program are refused everywhere" overstates, as do the policy comment, LIVE.md and commandshim/README.md. The wrappers remain evidence and guidance, not containment.
- **Nested go residuals (HOST-014/-015/-020).**
  - The environment check refuses only a foreign `GOROOT`, `GOTOOLCHAIN` and `GOENV` (`commandshim/shim.go:719-758`). `CC` (a probe's `CC` program ran), `CGO_*`, `PKG_CONFIG` and `GOMAXPROCS` pass, as do `-cpu`, `-args` tails and `go test all` or `std` (2 threads).
  - The run can rewrite its own mirror (same user). When the go on PATH sits outside a GOROOT (a version-manager shim), no mirror is made and nothing warns (`commandshim/goroot.go:100-128`, `effects/broker.go:272-278`).
  - Landing gates, `gate run` and explicit `heavy-op run` get neither wrappers nor a mirror. Nested calls still have no concurrency bound.
- **SEC-003 residuals.** A Codex login isn't redacted at all, and a Claude login only as raw `sk-ant-` text. Configured and environment secrets are matched only raw (`daemon/secret_redaction.go:526-537`), so this handoff's wave-8 "raw, hex and base64 forms" for environment values overstates. Base64 at a non-zero offset (`Authorization: Basic …`), mixed-case hex and wrapped lines pass.
- **Landing (CTL-030).** Gate results are now parsed from the redacted copy. Two packages whose import paths differ only inside a secret would collapse into one result, and the last `pass` wins (`controller/gates.go:166-176`; `SuiteGate` ignores the exit code). Theoretical. Unreadable secrets hold the landing as a `compile` failure, not as infrastructure.
- **Startup and rollback.**
  - An empty `process-*.json` left by the old crash window refuses every start with a bare `recover owned Dolt: EOF` or `EOF`, naming no file (`processowner/process.go:443-449`, `daemon/plane.go:34-36`).
  - A wave-8 binary can't decode a snapshot carrying wave 9's `route_generation`, which every live agent dispatch sets (strict `decodeEnvelope`, `daemon/rpc.go:524`). Rolling back across wave 9 fails those tasks; nothing documents it.
  - A stored Claude/`minimal` revision no longer parses: the rule sits in `Validate`, not `ValidateWrite` (`workflow/workflow.go:369-371, 433-435`).
- **Scope widens (WF-078): Retire-only dead ends remain.** An approved widen file the bridge can't load (a symlink, a non-regular file, over 1 MiB, or 4 MiB in total) refuses every later turn, and approved scope is never removed. An edit turn whose request is malformed escalates StaleBranchResidue, and restart is refused.
- **Smaller gaps:**
  - HOST-005: the controller logs one reading per slice, and `heavy-op run` (slice = maximum wait) records none unless it times out. The "Since re-audit 7" bullet below overstates.
  - The hook's documented pass list misses `py.test`, `pytest-3`, `gccgo` and `tinygo`.
  - Stale text: the cycle-walk comment (`daemon/backlog.go:232-235`, `edge_scope_test.go:28-30`); TESTING.md's "47 refused" (the test has 48; corrected); wave 8's git-alias "not contained" lines (aliases are refused since wave 9; annotated); `workflow/GRAMMAR.md` says nothing of route efforts; LEDGER.md's list of refusals outside the census omits `accounted_git.go`.
  - The `lifted` landing audit outcome has no test, so "every behaviour change has a test that fails on the base" overstates for that integration change.
  - The daemon's `GIT_AUTHOR_*` values (names containing AUTH) are redacted if exported.
  - Wave 5's "the broker's git refusal stays 77" rationale is stale: the broker has known the mode since wave 8.
  - `GOENV=off` dropping the operator's `go env -w` settings is stated only in this handoff.

Largest remaining gaps:
- **For self-hosting:**
  - the RED timeout trap and digit-only reasons;
  - the multi-reviewer refund;
  - the nested git and go residuals (injected configuration, `CC`, concurrency);
  - no live run yet of wave 9's nested policies: the Fae-on-Fae acceptance wasn't rerun.
- **Operator decisions:**
  - RT-010: keep the immediate escalation (RT-010 stays Contradicted) or park;
  - HOST-011 (the hook is registered nowhere);
  - GEN-010 and SEC-006 (off-host replication);
  - CLI-051;
  - nested calls' unbounded concurrency.
- **Verification:**
  - CI has never run.
  - 4 ledger entries have unknown native evidence, and no check maps WF-078 or WF-082.
  - The new nested-go refusals have no daemon-path test.
- **Writer lock, reads and storage (ROB-020/-021, CP-009, CP-012):** unchanged.
- **Unchanged:**
  - the commit stage's Reverify route (ROB-013);
  - FanOut runs sequentially (SCH-009, WF-030);
  - schema 2 on write;
  - API-010 and API-020;
  - CTL-030's Go-only gates;
  - unauthenticated Herdr sockets (SEC-001);
  - landing and `gate-runs` scratch (HOST-020).

**Since re-audit 8 (wave 10, 2026-10-28/29; re-audited above).**
- **Fixed, for self-hosting first:**
  - a RED test that hangs, exits or crashes is a failed run, not an UncertainDispatch park, and `test timed out` can now match (WF-073, WF-036);
  - the reason rule refuses numbers alone and accepts path messages, and the protocol says a RED is one top-level test without subtests (WF-073, WF-043);
  - every route refused parks `all-routes-unavailable`, resumes after a new route generation or on Resume, and escalates only past the 30-minute bound (RT-010);
  - a later reviewer's binding refusal refunds nothing, and a provider that never acknowledged the prompt is a start failure (RT-004, WF-033);
  - nested git loads no configuration or program the call chose and reaches only local repositories; a Local task's nested push exits 78 in refuse mode too (MODE-002, EXT-001, GEN-010);
  - nested go's environment is an allowlist; `-cpu`, test-binary flags after `-args` and reserved patterns are checked; the mirror is sealed, and a go outside any GOROOT is reported (HOST-014/-015/-020);
  - every secret is matched in every form, and provider logins by shape; a redaction can't turn a landing failure into a pass (SEC-003, CTL-030);
  - an empty process record no longer blocks every start, rolling back across wave 9 is documented, a stored Claude/`minimal` revision loads again, and the widen dead ends are gone (CP-003, WF-093, WF-078) *(re-audit 9: two Retire-only cases remain)*.
- **Smaller gaps fixed:** the readings that start and lift a controller hold (HOST-005); the hook's pytest names; offline `workflow` flags; the ProviderHealth RPC's window; `land --poll` and `watchdog --period`; the landing audit's refused and timed-out outcomes; stale comments. The ledger maps WF-078 and WF-082 and censuses hook denials, bridge refusal classes and Local denials.
- **Doc claims:** re-audit 8's overstatements are corrected in LIVE.md, commandshim/README.md, effects/README.md, docs/SELF_HOSTING.md, daemon/README.md and workflow/GRAMMAR.md, and annotated in this handoff.
- **Not fixed:** nested calls still have no concurrency bound; programs go and git find by name on PATH, and git's own subprocesses in a test's own repository, run unchecked; output over the run's byte bound still leaves a RED run uncertain; a start failure on a later review session still parks UncertainDispatch; a secret wrapped across lines leaks its tail *(re-audit 9: it leaks whole)*; derived scope has no protected-path rule (SEC-005); the fallback warnings still go to stderr only (OBS-001).

Details are under Completed in wave 10.

**Previous re-audit, 2026-10-27 (after wave 8; superseded above).** Nine parallel per-section audits of all 284 IDs compared the tree against re-audit 6, using the same method.
- **What ran:**
  - static reads;
  - copies of the pure RED-reason, framing and hook predicates, run on fragments, synthetic `go test -json` streams and 136 command lines;
  - `commandshim.Policy.PrepareAccounted` called directly on about 90 nested vectors;
  - a replay of `StartRole`'s file operations against a registry scan;
  - a pure-engine probe of the RED retry and scope-widen paths;
  - CLI probes of a `fae` binary built into the scratchpad;
  - a coordinator probe of which `go` a test binary resolves under `go test`.
- **Where the findings are:** the per-section reports were not committed. The findings below were checked in source.

| | Met | Implemented-unproven | Partial | Missing | Contradicted | N/A |
|---|---|---|---|---|---|---|
| Before wave 1 | 77 | 34 | 131 | 32 | 8 | 2 |
| After wave 2 | 155 | 29 | 90 | 3 | 5 | 2 |
| After wave 3 | 206 | 22 | 50 | 1 | 3 | 2 |
| After wave 4 | 206 | 21 | 50 | 1 | 4 | 2 |
| After wave 5 | 208 | 21 | 49 | 1 | 3 | 2 |
| After waves 6 and 7 | 207 | 21 | 50 | 1 | 3 | 2 |
| After wave 8 | **207** | 21 | 50 | 1 | 3 | 2 |

**What moved:** one ID up and one down, so the totals are unchanged. Delivery phases are unchanged at 8 (1, 2, 3, 4, 5, 7, 9, 11).
- **WF-034 (Partial → Met).** Wave 8's typed bridge refusals hold on Direct, the one-shot Herdr pane and resident workers, and `TestDaemonBridgeRefusalsRetryWithoutParking` shows the acceptance through the daemon.
  - Nothing inside a turn can forge the record: the model has no tools, and the bridge exits 65 only after its own `O_EXCL` write (`provider/refusal.go:28-48`), so a planted record makes it exit 1.
  - A provider CLI that exits nonzero with no structured error still parks UncertainDispatch and is charged (documented).
- **WF-043 (Met → Partial): new in wave 8.** The `expected_failure` reason rule (`liveRedReasonVacuous`, `daemon/live_scope.go:217-231`) refuses a derived RedReport, but nothing the model reads before its turn states it.
  - The test-first brief asks for "a stable expected-failure fragment" (`defaults/defaults.go:168`), and the bridge protocol for "the declared expected_failure text" (`provider/codex.go:590`). The kernel contract's gates are empty for test-first.
  - The model learns the rule only from a note appended to `operator_context` after a refused turn (`daemon/live.go:307`). Each refusal costs an attempt; four identical ones escalate SameGate.
- **Contradicted:** GEN-010 and SEC-006 (off-host backup replication, an open decision); PLAN-001 (historical).
- **Still missing:** CLI-051.
- **Re-audit 6's findings:** confirmed fixed, except where noted below.

New findings that change no rating:
- **A Go test's own `go` and `gofmt` reach no wrapper, in either mode (HOST-014/-015/-012; older than wave 8).**
  - `go test` runs each test binary with `PATH=$GOROOT/bin:$PATH` (Go's `cmd/go/internal/base/env.go`, `AppendPATH`). The broker only prepends its shim directory and sets no GOROOT (`effects/broker.go:146`, `commandshim/shim.go:57`).
  - So a test that execs `go` or `gofmt` runs the real toolchain: refuse mode doesn't refuse it, and account mode doesn't record or clamp it. A probe confirmed it: with a fake `go` first on PATH, a test resolved `/usr/local/go/bin/go`.
  - Live evidence: the wave-8 self-host QA record lists only `git` calls, although `TestCoreBinaryExcludesTheOperatorUI` runs `go list` three times *(four since wave 12 added `cmd/fae-tui`)*.
  - Every nested-call test uses a `/bin/sh` parent, so none catches it.
- **Account mode's nested git is unscoped beyond push and remote (MODE-002, EXT-001, VCS-002, VCS-010).**
  - `PrepareAccounted` refuses only `push`, `remote`, `send-pack` and `http-push`, and runs every other git subcommand as called (`commandshim/shim.go:367-373`).
  - Task worktrees are added from the operator's workspace (`worktree/manager.go:97`), and nothing sets `extensions.worktreeConfig`, so they share its `.git/config` and refs. A nested `git config remote.<n>.url`, `update-ref refs/heads/main`, `branch -f` or `worktree remove --force <other task>` changes the operator's repository.
  - Also unrefused: aliases (documented), git-started children such as `!` aliases, hooks, `submodule foreach`, `rebase -x` and `subtree push` (git puts its own exec path first), and `imap-send` and `send-email`.
  - Re-audit 6 named the ref and worktree case; this handoff's "Not fixed" list dropped it.
- **Account mode's go policy is a denylist (HOST-014).** It refuses only `-toolexec`, `-exec`, `-vettool` and `-C`, where the top-level `Prepare` is an allowlist. RAM-backed `-pkgdir`/`-coverprofile`/`-outputdir`, `-overlay` and `-modfile` outside the scope, `-ldflags=-extld` and a `GOENV`-supplied `GOFLAGS` pass (auditor probe).
- **The process-registry race fix is partial (HOST-021, ROB-016).** `publish` writes the first record whole, but `StartRole` then reopens it with `O_TRUNC` and rewrites it to add the command and directory (`processowner/process.go:270-279`). A concurrent retirement scan can still read it empty and park a sibling task, and a crash in that window leaves an empty record that `Recover` refuses at start. The regression test covers `RegisterRole` only. `publish` also needs hard links (undocumented), and a crash before the link leaves an unswept pending file.
- **The RED reason rule is narrowed, not closed (WF-073 stays Met).**
  - Still accepted: panic-trace words (`runtime`, `SIGSEGV`, module and worktree path words), generic `want`/`got`, and a fragment the test `t.Log`s before it fails.
  - Refused though legitimate: numeric-only reasons (`0 != 42`), name-word reasons (`divide by zero` for `TestDivideByZero`) and `timed out`.
- **Scope widens (WF-078).**
  - The engine's `relative-path` check refuses only a root `.git` (`workflow/command.go:61`), so an approved `sub/.git/x` widen passes, then the resolver refuses every later dispatch, restart included (`daemon/live_scope.go:110-113`). Only Retire escapes.
  - A widen naming a test file the frozen RedReport listed refuses implement 4 times, then SameGate. Only a restart with test runs left recovers it, and `docs/SELF_HOSTING.md` doesn't say so.
- **A binding the bridge refuses locally never fails over (RT-010, WF-082).** A pre-provider refusal charges nothing (`execution/executor.go:248`), so a misconfigured route stays eligible: for example the codex half of the default chain on a Claude instance, or a Claude route with effort `minimal`. Each task on that role ends SameGate.
- **SEC-003 residuals.** Provider credentials the daemon never reads (a `CLAUDE_CONFIG_DIR` login) aren't redacted; only raw, hex and base64 forms are covered; nested-call argv reaches plane markers unredacted (`daemon/live_nested.go:76-91`); landing gate output persists unredacted in landing scratch.
- **HOST-005 on the controller path (Fae's own commit).** avg60 and the directions appear only on the rate-limited wait line (`controller/admission.go:161-167,204-206`). The test checks only that the key names appear.
- **Smaller gaps:**
  - `daemon/BACKLOG.md:66` says the cycle walk is read before the writer lock and again under it; it runs only under the lock.
  - The `DeclaredPathSafe` comment says it covers every scope path, but the instance-level `files` pin and the bridge's `--files` skip it.
  - A ResolveUncertain redispatch keeps the parked turn's counters (`engine/uncertain.go:28-36`).
  - `provider-health --window` still exits 2 on an out-of-range value; `watchdog --period` ≤ 0 still silently becomes 1 minute.
  - The hook's "what passes" list omits other managers' PATHs (`tmux`, `crontab`, `systemctl --user`, `docker`) and `python -m pytest`, `go1.N`, `rustup run`.

Doc claims the auditors found overstated:
- **"No input lowers the 12 GiB landing floor"** (this handoff, `README.md:67`). `gate run --memory-floor-gib 4` sets 4 GiB, which Appendix A allows. What holds is that no non-finite, out-of-range or below-hard-floor value lowers it; `help.go` says so correctly.
- **"The fallback covers every flag with a spec default"** (this handoff). `provider-health --window` doesn't, and `README.md:70` wrongly says the runtime-config and `--weight` flags have no spec default.
- **"A concurrent registry scan never reads one empty"** (this handoff, the `publish` comment), as above.
- **Account mode "applies the same policy as a top-level engine command"** (this handoff, LIVE.md, `docs/SELF_HOSTING.md`), "refuse makes every nested call exit 77" (LIVE.md), "a remote change exits 78" (LIVE.md, `docs/SELF_HOSTING.md`) and "an engine run never pushes" (`effects/README.md`), as above.
- **"QA's whole suite passed through account mode's nested-call policy"** (this handoff's wave-8 verification): its nested `go` calls bypassed the wrappers.
- **`effects/README.md:19-21`:** "the test id never matches on its own" is false when the RED test panics; the trace names it.
- **Stale lines:** this handoff's wave-6 row (the watchdog "still accepts NaN from non-CLI callers", fixed in wave 7) and "All 23 tables" (the plane has 35); `TESTING.md` says the self-host self-test runs after the suite, but `test.sh` runs it first.

Largest remaining gaps:
- **For self-hosting:**
  - a Go test's nested `go` bypasses both modes, so QA under account mode isn't accounted for its builds;
  - account mode's git scoping (ref and config writes into the operator's repository);
  - the registry race through `StartRole`;
  - the RED reason rule: state it in the contract and stop refusing real reasons.
- **Operator decisions:**
  - HOST-011 (the hook is registered nowhere);
  - GEN-010 and SEC-006 (off-host replication);
  - CLI-051;
  - account mode's unbounded concurrent nested calls.
- **Verification:**
  - CI has never run.
  - The ledger matches none of waves 7 and 8's refusals (hook denials, derived-scope refusals including the reason rule, `DeclaredPathSafe`, bridge-refusal typing, redaction).
  - `gobra:shim` maps HOST-014, though account mode's path never calls `CanExecute`.
  - Dolt-backed tests skip in self-hosted engine QA.
- **Writer lock (ROB-020/-021):** housekeeping I/O under `d.mu`; the edge edit's keyed reads still query the plane under it. Every mutation except Admit still waits on drive checkpoints.
- **Reads and storage (CP-009, CP-012):** whole reads on the backlog verbs, landing, `TaskHeads` and a supersession Retire's edge transfer; `ticket_emissions.payload` up to 1 MiB; blobs are never collected.
- **Unchanged:**
  - the commit stage's Reverify route (ROB-013);
  - FanOut runs sequentially (SCH-009, WF-030);
  - schema 2 on write;
  - API-010 and API-020;
  - CTL-030;
  - unauthenticated Herdr sockets (SEC-001);
  - landing and `gate-runs` scratch (HOST-020).

**Since re-audit 7 (wave 9, 2026-10-28; re-audited above).**
- **Fixed, for self-hosting first:**
  - a Go test's own `go` and `gofmt` reach the wrappers in both modes, and account mode's go policy is an allowlist (HOST-014/-015/-012);
  - account mode's nested git can't change the operator's repository: only repositories in the run's own directories and the system temporary ones may be changed (MODE-002, EXT-001, VCS-002/-010);
  - a process record is published once, whole, which closes the registry race through `StartRole` (HOST-021, ROB-016);
  - the RED reason rule is stated in the bridge protocol and no longer refuses real reasons (WF-043, WF-073);
  - a widen into a nested `.git` is refused before the judge, and a widen no longer breaks a frozen RED that lists its file (WF-078);
  - a binding the bridge refuses locally fails over, and every route refused escalates ProviderUnavailable at once (RT-010, WF-082; wave 10 parks instead); a redispatch starts with fresh per-turn counters;
  - SEC-003's residuals: nested-call arguments, landing gate output, encoded forms and the daemon's own environment;
  - every controller pressure reading carries avg60 and directions (HOST-005) *(re-audit 8: every reading it records, at most one per slice; wave 10 also records the readings that start and lift a hold)*.
- **Smaller gaps fixed:** `provider-health --window` and `watchdog --period`; the hook's other names for shimmed tools; `gobra:shim`'s HOST-014 mapping; a record retired mid-scan; malformed foreign records; stale pending records; the UI log redactor's tail.
- **Doc claims:** re-audit 7's overstatements are corrected in README.md, LIVE.md, docs/SELF_HOSTING.md, effects/README.md, commandshim/README.md and daemon/BACKLOG.md, and annotated in this handoff. *(Re-audit 8 found new overstatements; see above.)*
- **Not fixed:** a provider login the daemon never reads is redacted only in its `sk-ant-` shape (documented; wave 10 adds OpenAI keys and JSON Web Tokens); the protected-path rule in derived scope (SEC-005); the fallback warnings still go to stderr only (OBS-001); nested calls still have no concurrency bound.

Details are under Completed in wave 9.

**Previous re-audit, 2026-10-26 (after waves 6 and 7; superseded above).** Nine parallel per-section audits of all 284 IDs compared the tree against re-audit 5, using the same method.
- **What ran:**
  - static reads;
  - a pure-engine probe of a malformed derived RedReport;
  - a copy of the hook predicate run on 139 commands;
  - CLI probes of a `fae` binary built into the scratchpad: help, usage, and `gate run` on private ledgers.
- **Where the findings are:** the per-section reports were not committed. The findings below were checked in source.

| | Met | Implemented-unproven | Partial | Missing | Contradicted | N/A |
|---|---|---|---|---|---|---|
| Before wave 1 | 77 | 34 | 131 | 32 | 8 | 2 |
| After wave 2 | 155 | 29 | 90 | 3 | 5 | 2 |
| After wave 3 | 206 | 22 | 50 | 1 | 3 | 2 |
| After wave 4 | 206 | 21 | 50 | 1 | 4 | 2 |
| After wave 5 | 208 | 21 | 49 | 1 | 3 | 2 |
| After waves 6 and 7 | **207** | 21 | **50** | 1 | 3 | 2 |

**What moved:** one ID up and two down. Delivery phases are unchanged at 8 (1, 2, 3, 4, 5, 7, 9, 11).
- **RT-005 (Partial → Met).**
  - Any state change clears the stall counter (`engine/engine.go:428-430`), and so do a completion and a held collection.
  - Three engine tests show the next stalled turn gets both retries.
- **WF-034 (Met → Partial): pre-existing, and wave 7 widened it.**
  - **Malformed output never becomes the typed outcome.**
    - The shipped bridges validate the role output themselves and exit 1 on failure (`provider/codex.go:457-462`, `cmd/fae-claude-bridge/main.go:31-33`).
    - The runtime reports `process-failed`, and the executor's malformed-output branch never runs, since it needs a handoff (`execution/executor.go:222-225,272-275`).
    - The class stays `uncertain`, and the task parks UncertainDispatch for an operator, instead of a typed outcome and a bounded stage retry.
  - **Wave 7's new edit refusals end the same way:** a replacement whose `old` text is missing or not unique, both or neither of content and replacements, or more than one RED file.
  - **Local refusals are charged to the provider (RT-004/-009).** Some refusals happen before the provider runs: a failed repository listing, task files over 4 MiB, a scope/pin mismatch. They are recorded as MalformedOutput against the binding.
- **HOST-014 (Met → Partial): wave 7's opt-in `nested_commands: account` is not the spec's accounted branch.**
  - **No shim policy.** The wrapper records the call, then `exec`s the real tool (`commandshim/accounting.go:84-101`). Nested calls skip the shim policy:
    - no `-p`/`-parallel` clamp (`GOFLAGS` is cleared, `effects/broker.go:110-121`);
    - no flag, path or formatter checks;
    - `git push` and `git remote` run, for Local tasks too.
  - **No process bound.** Only the per-run task cap (512) bounds the processes under one lease.
  - **Who is affected.** The default refuse mode still meets the requirement, but the self-host harness requires account mode.
- **Contradicted:** GEN-010 and SEC-006 (off-host backup replication, an open decision); PLAN-001 (historical).
- **Still missing:** CLI-051.
- **Re-audit 5's findings:** confirmed fixed, except where noted below.

New findings that change no rating:
- **A malformed derived RedReport makes the task unrecoverable (Appendix C step 7).**
  - **Trigger:** a RedReport naming zero or two in-scope test files, a non-Go test id, or no `expected_failure`. Each makes the next `run-red` refused before it runs (`daemon/live_scope.go:138-163`).
  - **First pass:** each refusal spends a test run and an attempt, and the task escalates SameGate. The probe spent 4 of 8 test runs.
  - **After a restart:** the evidence carries, and `brokenRed` refuses the test-first turn that would replace the report (`daemon/live.go:292-295`). Only Retire and resubmission remain.
- **A derived RED's "declared reason" can be vacuous (WF-073).** The model now chooses `expected_failure`, and the broker matches it anywhere in the test's output, `--- FAIL: <id>` included (`effects/broker.go:418`). So a fragment equal to the test id or `FAIL` always matches. The test must still run and fail.
- **Account mode lets Local tasks push (MODE-002, GEN-010).**
  - The setting is instance-wide and never checks the task's mode (`daemon/live.go:68,183`). The Local push refusal lives only in the role shim (`commandshim/role.go:197-200`).
  - Role-written test code can also run `git worktree remove` or move refs, which the broker's tree check doesn't see.
- **Nested-command records collide (CP-012).** They are keyed by output digest alone (`daemon/live_nested.go:36`; digest at `effects/broker.go:302-303`) and written with REPLACE. So two runs with identical output (any silent run) share a record. LIVE.md, `effects/README.md` and this handoff claim one record per run.
- **The edge edit's pre-lock read became whole-table (CP-009).** Wave 6 replaced a keyed `TaskDependencies` read with `Store.Backlog` (`daemon/backlog.go:315-321`). Every `backlog edge` now reads both backlog tables twice.
- **Ticket statistics read every payload (CP-009).** `TicketStats` reads every `ticket_emissions` payload with no limit (`controlplane/outbox.go:145`), at startup and after every live drive round (`daemon/ticket.go:152`). Neither VERIFICATION.md nor this handoff listed it.
- **Per-stage start and failover counters carry across turns (RT-004, ROB-017, Appendix A).**
  - `startup-recovery:<stage>` and `failover:<stage>` are cleared only on completion (`engine/engine.go:1059-1060`).
  - A failed turn or an escalation leaves them, so a later visit gets fewer than 3 start attempts or no failover rotation.
- **`gate run --memory-floor-gib NaN` (or `Inf`) disables the landing floor (HOST-007, pre-existing).** It passes the `<= 0` check (`cmd/fae/gates.go:114`) and converts to a nonsense floor (`:159`). Probe: `Inf` was admitted where 1000 GiB timed out.
- **Engine-feedback excerpts are stored and sent to providers unredacted (SEC-003).** A failed run's output (up to 16 KiB) becomes a blob and a brief block with no redaction step (`daemon/engine_feedback.go:98-110`).
- **No protected-path rule in derived scope (SEC-005).** *(Wave 11 adds one, and declared paths reach git as literal paths.)* `.github/`, `scripts/test.sh` and `defaults/` can be declared, and only spec review judges them (`daemon/live_scope.go:68-70`). The paths reach git as pathspecs.
- **Hook literal bypasses remain (HOST-011).** The by-path rule matches only the shim names, so `/usr/lib/git-core/git-push` passes (`commandshim/hook.go:49`). `systemd-run --user` and toolchain internals by path also pass. Wave 6's other fixes are confirmed.
- **Smaller gaps:**
  - A SubWorkflow child has no declared files, so a derived-scope instance refuses its change turns (`daemon/subworkflow.go:157`).
  - Admission accepts `.git` declared files; the CLI and the resolver refuse them later (`daemon/admission.go:367-378`).
  - A closed-but-registered resident slot is picked first (`runtime/resident.go:264`).
  - No test lands a derived-scope task through `fae land`.
  - The configuration-fallback warnings go to stderr only (OBS-001).

Doc claims the auditors found overstated:
- **This handoff's wave-6 decision "an empty, out-of-range or non-finite configurable flag falls back".** It holds for `serve` and the controller verbs only. Elsewhere:
  - `census --gap` and `gate run` limits exit 2;
  - `heavy-op run --max-wait` ≤ 0 silently becomes 1800 s;
  - `land --roll-max-wait` and `--overlap-wait` accept negatives;
  - `--instance ""` exits 1.

  README and help scope it correctly.
- **"FAE-HOST-014's accounted option"** (this handoff, `commandshim/README.md:13`, LIVE.md, `effects/README.md`), as above.
- **"Each run's calls are kept durably"** (nested-command records), as above.
- **`docs/SELF_HOSTING.md`:**
  - "The Spec declares the task's files" (admission files win);
  - it doesn't mention the RedReport dead end, or that a malformed response parks.
- **`runtime/IDLE_SESSIONS.md:53,114-118`:** "runs on another slot" overstates the closed-but-registered case.
- **`engine/README.md:22-23`:** "1 per turn" for failover overstates it, as above.
- **`commandshim/README.md:28`:** its exception list misses the git dashed builtins.
- **This handoff's wave-6 Prophet decision:** "finished before the session started" should read before the daemon started.

Largest remaining gaps:
- **For self-hosting:**
  - the parks on malformed output (WF-034);
  - the RedReport dead end;
  - account mode's missing policy (HOST-014).
- **Operator decisions:**
  - HOST-011 (the hook is registered nowhere);
  - GEN-010 and SEC-006 (off-host replication);
  - CLI-051;
  - whether account mode is acceptable as an opt-in outside HOST-014.
- **Verification:**
  - CI has never run.
  - Seven wave-3 pure guards are outside `verify.sh`.
  - Wave 6 and 7's new refusals (hook denials, derived-scope refusals, bridge scope and edit refusals) match no ledger pattern.
  - Dolt-backed tests skip in self-hosted engine QA.
- **Writer lock (ROB-020/-021):** housekeeping I/O under `d.mu`. Every mutation except Admit still waits on drive checkpoints.
- **Reads and storage (CP-009, CP-012):**
  - whole-table reads on the backlog verbs, `TaskHeads` and ticket statistics;
  - the unbounded finished-task summary;
  - blobs are never collected, and wave 7 adds more.
- **Unchanged:**
  - the commit stage's Reverify route (ROB-013);
  - FanOut runs sequentially (SCH-009, WF-030);
  - schema 2 on write;
  - API-010 and API-020;
  - CTL-030;
  - unauthenticated Herdr sockets (SEC-001);
  - heavy work outliving a reclaimed lease (HOST-014/-021/-031);
  - landing and `gate-runs` scratch (HOST-020).

**Since re-audit 6 (wave 8, 2026-10-26/27; re-audited above).**
- **Fixed, for self-hosting first:**
  - a refused model answer, or a turn the bridge refused before the provider ran, no longer parks UncertainDispatch: the first is MalformedOutput with a bounded stage retry, the second a ValidationFailed retry that charges no provider (WF-034, RT-004/-009);
  - a malformed derived RedReport is retried at test-first, and a restart recovers the task (Appendix C step 7); a derived `expected_failure` must state a reason (WF-073);
  - account mode is HOST-014's accounted branch: nested calls pass the shim policy and clamps, and nested push is refused, 78 for Local tasks (HOST-014, MODE-002) *(re-audit 7: only for calls that reach a wrapper)*;
  - nested-command records are per run (CP-012);
  - the start and failover counters reset per turn (RT-004, ROB-017);
  - `gate run` can't lower the 12 GiB landing floor with a non-finite, out-of-range or below-hard-floor value (HOST-007);
  - the edge edit reads by key, and ticket statistics are one aggregate (CP-009);
  - failed-run excerpts are redacted (SEC-003); the hook denies dashed subcommands, toolchain internals by path and `systemd-run` (HOST-011);
  - the configured-value fallback covers every flag with a spec default but `provider-health --window` (added in wave 9), and its warnings carry OBS-001's fields.
- **Smaller gaps fixed:** a SubWorkflow child's scope, `.git` declared files at admission, the closed-but-registered resident slot, and a test that lands a derived-scope task.
- **Found and fixed during integration:** a process-registry race older than wave 8 that could park a concurrent task UncertainDispatch (see Completed in wave 8).
- **Doc claims:** every one listed above is corrected, including the four in this handoff. *(Re-audit 7 found new overstatements; see above.)*
- **Not fixed:** the protected-path rule in derived scope (SEC-005); the git alias bypass of account mode (documented as not contained; refused since wave 9); account-mode ref and worktree moves (re-audit 6, not listed here before re-audit 7); the fallback warnings still go to stderr only (OBS-001).

Details are under Completed in wave 8.

**Previous re-audit, 2026-10-26 (after wave 5; superseded above).** Nine parallel per-section audits of all 284 IDs compared the tree against re-audit 4, using the same method.
- **What ran:**
  - static reads;
  - re-audit 4's in-memory WF-074 engine probe, rebuilt and re-run;
  - a copy of the hook predicate run on 48 commands.
- **Where the findings are:** the per-section reports were not committed. The findings below were checked in source.

| | Met | Implemented-unproven | Partial | Missing | Contradicted | N/A |
|---|---|---|---|---|---|---|
| Before wave 1 | 77 | 34 | 131 | 32 | 8 | 2 |
| After wave 2 | 155 | 29 | 90 | 3 | 5 | 2 |
| After wave 3 | 206 | 22 | 50 | 1 | 3 | 2 |
| After wave 4 | 206 | 21 | 50 | 1 | 4 | 2 |
| After wave 5 | **208** | 21 | **49** | 1 | **3** | 2 |

**What moved:** two IDs up and none down. Delivery phases are unchanged at 8 (1, 2, 3, 4, 5, 7, 9, 11).
- **WF-074 (Contradicted → Met), per the operator's ruling.**
  - Resume and restart-with-information carry every counter (`engine/control.go:246-262`).
  - TestRunCap is checked before AttemptCap (`engine/engine.go:781-788`).
  - Re-audit 4's probe now ends TestRunCap at exactly 8 test runs in every scenario: a raise at attempt 33, 36 or 37, lower then raise, repeated raises, and restart. Small repeated raises end LoopCap.
- **WF-021 (Partial → Met).**
  - Every edge out of Pending goes through `enter`, which drops `carry_entry` (`engine/engine.go:416-420`).
  - Every save path validates first.
- **Contradicted:** GEN-010 and SEC-006 (off-host backup replication, an open decision); PLAN-001 (historical).
- **Still missing:** CLI-051.
- **Wave-4 findings:** all confirmed fixed. No regression was found.

New findings that change no rating:
- **Admit's pre-lock read has a narrow window (WF-005, ROB-021; fails closed).**
  - Under the lock, Admit re-checks only `d.jobs` and quarantine (`daemon/admission.go:265-282`).
  - An Admit whose `AdmitJob` commits but returns an error never enters `d.jobs` (`:38-41`).
  - A concurrent same-id Admit whose pre-read ran before that commit then forks, adopting the tree, before the primary key refuses it. Wave 4 re-read the plane under the lock.
- **Stall counter after escalation (RT-005).**
  - The counter resets only when a turn completes or records a failure (`engine/engine.go:1049,1287`).
  - A StallExhausted escalation leaves it above the bound, and restart-with-information carries it.
  - So the first stall after such a restart escalates with no retry. Wave 5's "fixed" overstates this.
- **Failed resident slot reconciliation** (read, not traced end to end).
  - When retiring a dead slot fails inside `acquire` (`runtime/resident.go:466-469`), the slot is not marked unretired.
  - Later turns pick it again and fail startup until the task parks ProviderUnavailable. A local failure is then charged to the provider.
- **Unretired transports.** Each unproven retirement leaves one extra pane against the role cap; the decision says one pane in total. The host pane limit still holds, because the pane ledger counts them (`scheduler/panes.go:66-106`).
- **Hook literal bypasses (HOST-011).**
  - The `env` check matches only the bare word `env` (`commandshim/hook.go:31`). So these pass: `/usr/bin/env -i go`, `env - go`, `env -iv go`, `env -uPATH go`, `env --unset=PATH go` and `{/usr/bin/go,build}`.
  - These are literal forms, not unevaluable expansions. The finding is not Contradicted only because no command tool is reachable.
  - The hook also dies of SIGPIPE (141) when its reader has closed the pipe, and exits 0 when stdout is closed at start.
- **CP-009 summary scope.**
  - Any Board read that needs finished rows starts the full finished-task build (`daemon/reads.go:149`), even `board --run X --limit 5`.
  - The summary holds every finished row for the daemon's lifetime.
- **Pool reads that take the writer lock (ROB-020).** Re-audit 4's "reads take no `d.mu`" was wrong.
  - The Census and ReviewTargetHealth pool reads take it (`daemon/census.go:96,197-205`; `daemon/stored_jobs.go:186-195`).
  - A writer stuck in I/O can therefore pin read-pool slots past their deadline.
- **Operator visibility of carried budgets (OPS-010).**
  - Resume accepts a task whose test-run or loop budget is already spent, and its reply is only `{id, stage, version}`.
  - The later TestRunCap escalation has an empty detail (`engine/engine.go:782`), and LoopCap's doesn't say the iterations carried over.
- **Out-of-range configuration.** The spec's conventions say an out-of-range configured value falls back to the default. `serve --inline-limit` and `--dolt-stop-wait` refuse it with a usage error instead (`cmd/fae/cli.go:374-380`).
- **Smaller gaps:**
  - A mid-round skip on a pending checkpoint records no withheld reason (`daemon/drive.go:449`).
  - No test covers `plane_unconfirmed`.
  - An idle refusal's cause change drops the previous cause's suppressed count (`daemon/idle.go:132-134`).
  - A conformance proof skipped for lack of Dolt is never checked, outside CI's `--forbid-skip`.

Doc claims the auditors found overstated:
- **HANDOFF's "`daemon/README.md` lists, verified, everything that still holds `d.mu`".** These are missing from the list:
  - task controls' git checks (`checkControlWorkspace`, `daemon/task_controls.go:77`, and Retire's `rev-parse`);
  - Cancel and Retire's `HeldDependents` read;
  - run-wide resume's `ActiveBacklog` and `LatestRunID` (`daemon/run_controls.go:65,87`), so its "checking reads off the lock" is partial;
  - the edge edit's `knownTasks` read;
  - Prophet session start;
  - `keyedMu`, held across a keyed mutation (`daemon/idempotence.go:35-55`).
- **HANDOFF:** "Fixed: RT-005" and "one pane beyond the cap", as above.
- **HANDOFF re-audit-4 gap "instance secrets aren't checked to be outside a repository (SEC-003)"** was wrong. `daemon/layout.go:124-135` refuses an instance root under any `.git` ancestor, though no test exercises it.
- **HANDOFF's "every one of the 23 tables":** the plane creates 35. The writer claim holds for the record families.
- **`daemon/TASK_CONTROLS.md:43-46`:** the live uncertain-resolution path saves directly (`daemon/uncertain.go:133`). The engine validates the result, so no invalid row is saved.
- **`commandshim/README.md:13`:** names only unevaluable expansions as misses, but the literal bypasses above also pass.
- **`README.md:203`:** "per-round census" is stale for idle rounds.
- **`daemon/lifecycle_events_test.go:68-70`:** claims lease lifecycle events that the test never asserts.

Largest remaining gaps:
- **The new findings above,** especially the Admit window, the stall counter after escalation, and slot reconciliation.
- **Operator decisions:**
  - HOST-011 (the hook is registered nowhere, and has literal bypasses);
  - GEN-010 and SEC-006 (off-host replication);
  - CLI-051 (`import-legacy`, which §19 doesn't list as open).
- **Verification:**
  - CI has never run.
  - Seven wave-3 pure guards are outside `verify.sh`.
  - The hook decision and the Local push refusal match no ledger pattern.
  - Wave 5's resident slot selection is outside the resident guard.
- **Writer lock (ROB-020/-021):** the housekeeping I/O plus the unlisted RPC I/O above. Every mutation except Admit still waits on drive checkpoints.
- **Reads (CP-009):**
  - whole-table reads on the backlog verbs, `TaskHeads` and the global Prophet (which fails past 1,000 jobs);
  - the unbounded finished-task summary.
- **Concurrency:**
  - FanOut is sequential (SCH-009 clause 2, WF-030).
  - One stale foreign Herdr socket fails the census host-wide.
- **Unchanged:**
  - the commit stage's Reverify route (ROB-013);
  - schema 2 on write (WF-001/-030);
  - API-010 and API-020;
  - CTL-030;
  - unauthenticated Herdr sockets (SEC-001);
  - heavy work outliving a reclaimed lease (HOST-014/-021/-031);
  - landing and `gate-runs` scratch (HOST-020).

**Since re-audit 5 (waves 6 and 7, 2026-10-26; re-audited above).**
- **Fixed:**
  - the Admit window (WF-005);
  - the stall counter after an escalation, restart or resume (RT-005);
  - failed resident slot reconciliation, and the unretired-pane claim;
  - the hook's literal `env`, `exec`, brace, rc-file and `.`/`source` bypasses, and its SIGPIPE and closed-stdout exits (HOST-011);
  - pool reads under the writer lock (ROB-020);
  - the global Prophet past 1,000 jobs (CP-009);
  - carried-budget visibility (OPS-010);
  - out-of-range configuration;
  - the smaller gaps: the mid-round withheld reason, a `plane_unconfirmed` test, the idle cause-change count, and skipped conformance proofs.
- **Writer lock (ROB-021):** run-wide resume's reads, the edge edit's endpoint reads and Prophet session start run off the lock, and `keyedMu` became per-key turns. `daemon/README.md` lists, verified, what still holds `d.mu`.
- **Doc claims:** every one listed above is corrected, including the four in this handoff.

Details are under Completed in wave 6.

Wave 7 added self-hosting. Task scope now comes from the task (WF-073/-076/-078). Rework turns see why their runs failed (Appendix C step 16). Nested commands can be accounted (HOST-014's accounted option). The live acceptance also found a production defect, now fixed: ownership records over 64 KiB couldn't be read back (HOST-021/-023). Details are under Completed in wave 7.

**Previous re-audit, 2026-10-24 (after wave 4; superseded above).** Nine parallel per-section audits of all 284 IDs compared the tree against the post-wave-3 re-audit. IDs whose evidence wave 4 didn't touch were carried forward once their cited code and tests were confirmed to exist; touched IDs and every earlier gap were audited in full. They were static reads, plus two in-memory engine probes; the per-section reports were not committed.

| | Met | Implemented-unproven | Partial | Missing | Contradicted | N/A |
|---|---|---|---|---|---|---|
| Before wave 1 | 77 | 34 | 131 | 32 | 8 | 2 |
| After wave 2 | 155 | 29 | 90 | 3 | 5 | 2 |
| After wave 3 | 206 | 22 | 50 | 1 | 3 | 2 |
| After wave 4 | **206** | 21 | 50 | 1 | **4** | 2 |

**What moved:** four IDs up, four down; delivery phases unchanged at 8 (1, 2, 3, 4, 5, 7, 9, 11).
- **Up:** ARC-002 (unproven → Met: on-demand snapshot, measured idle cost), VCS-001 (Partial → Met: carried tasks fork from the tip), CLI-001 and CLI-015 (Partial → Met).
- **WF-074 (Met → Contradicted), a wave-4 decision.**
  - The attempt check runs before the test-run check (`engine/engine.go:772-778`). A task that has used all 8 test runs can therefore escalate AttemptCap.
  - Raise plus Resume then refunds the test-run and loop budgets (`newPass`, `engine/control.go:235-251`).
  - Probe: a task escalated AttemptCap at attempt 36 with 8 test runs used. After a raise and resume it reached Done with 12 test runs. Lowering the limit once the budget is spent, then raising it, does the same.
  - So a raise bypasses TestRunCap and LoopCap, which WF-074 forbids, and Appendix A's "per task" limits hold per pass.
  - `daemon/TASK_CONTROLS.md`'s "never bypasses TestRunCap, LoopCap" is false. Restart-with-information keeps every counter, so the two verbs disagree.
- **WF-021 (Met → Partial), a wave-4 code regression.**
  - An operator Escalate or Cancel of a carried task still Pending leaves `carry_entry` set, and `Validate` rejects that outside Pending (`engine/engine.go:281`).
  - Neither `ApplyControl` nor the operator checkpoint validates its result, so the invalid row is saved.
  - Retire and Restart are then refused, and the drive quarantines the task. A cancelled one can't be a carry-forward source.
  - Probe confirmed. No test covers a control on a carried Pending task. No drive path escalates a carried task before release.
- **RT-005 (Met → Partial), pre-existing.** The stall-retry counter is task-wide and never reset; the startup and failover counters reset when a turn completes. A task's third stall anywhere therefore escalates without a retry. ROB-017's "stored per task" makes this arguable.
- **ROB-021 (Met → Partial), a stricter reading.**
  - Admit no longer waits for drive checkpoints or the release refork. Other mutating RPCs still wait for an open checkpoint by design.
  - Every mutating RPC still waits for housekeeping I/O done under the lock: cron admission's `git worktree add`, runtime-config ledger I/O, and dispatch's dependency read.
  - `daemon/README.md` omits some I/O done under the lock: the drive-exit and re-arm audits, startup-exhaustion observations, post-restart quarantines, provider observations and Prophet replies.
- **Contradicted:** WF-074 (new); GEN-010 and SEC-006 (off-host backup replication, open decision); PLAN-001 (historical). **Still missing:** CLI-051.
- **Wave-3 regressions:** all confirmed fixed. The WF-074 fix over-corrects, as above.

Wave-4 weakenings that change no rating:
- **Reads after a restart cost more as history grows.**
  - The plane query applies only Board's state, target, workflow and molecule filters. `--in-flight`, `--min-priority`, mode and run are applied after each finished row is fetched and decoded (`daemon/reads.go`, `withStoredRows`).
  - So `board --in-flight`, which no finished task can match, scans all of them. `status --tasks` reads them all, up to the 1 MiB response limit. The tests use 8 tasks.
  - ROB-020's cached-snapshot clause is missed for these reads, and `daemon/READS.md`'s "cached Board and Status remain responsive when the pool is saturated" no longer holds after a restart.
- **Status holds `d.childMu`** across `status --tasks`' plane read (`daemon/rpc.go:224-241`). During a Dolt outage this can delay the plane restart or shutdown by up to the 5 s read deadline.
- **New plane reads under the writer lock:** `refuseExistingTask` on every Admit, run-wide `resume`, `backlog edge`.
- **Idle leak census:** memory growth in an unchanged idle tree is caught within 60 s, not 5 s. No test covers the 60 s path, and `daemon/leaks.go` still says "every round". Judged a defensible reading of HOST-022.
- **Hook exit codes:** judged consistent with HOST-011, since a deny is exit 0 with a decision and an exit 1 would let the provider run the tool. Two cases still exit 1, which fails open: a missing `$HOME` and a failed stdout write. The lexical check misses quoting inside a word (`/usr/bin/g''o`).
- **Conformance proof check:**
  - It accepts any existing test that quotes one word of the row, and never checks the exit code (`cmd/fae/conformance_test.go:563-590`).
  - Its self-test accepts the citation that made the old hook proof false.
  - The `maintenance stop|start` exit-0 proofs never run the CLI verb.
- **Generation pinning:** only generation 2's hash is fixed in the test, so editing generation 3's recorded hash in place still passes.
- **Withheld reasons:** a Pending task skipped because it changed during the unlocked refork records no withheld reason.

Doc claims the auditors found overstated (HANDOFF's corrected above):
- HANDOFF: the restart test compares 24 read results, not 31.
- `daemon/TASK_CONTROLS.md` (Resume row) and `daemon/READS.md:10-11`, as above.
- `runtime/IDLE_SESSIONS.md`: "one task's slow launch never delays another task's turn" is false for a shared workflow-global identity. That identity defers the later turn and retries it every 5 s, so with resident sessions on, a cap-2 global role runs one turn at a time.
- `commandshim/README.md:7`: says Local push/remote refuses with 77. It is 78 when `push`/`remote` is the first git argument.
- `workflow/GRAMMAR.md:104-105`: describes `ValidateWrite`'s schema-1 refusal, but nothing calls it.
- `IMPLEMENTATION.md:13` and the historical gap table below: still say the default lacks a request judge and no-red route, and that `external_effects` has no writer. All three exist now.
- `controlplane/VERIFICATION.md:55`: says reads are keyed and paginated.

Largest remaining gaps:
- **The two wave-4 findings above** (WF-074, WF-021).
- **Verification:** CI has never run. Seven wave-3 pure guards are outside `verify.sh`, and wave 4 added none.
- **HOST-011:** the pre-tool hook is registered nowhere, and the leased role-shim path is unreachable (operator decision).
- **Heavy work outliving a reclaimed lease** (HOST-014/-021/-031); landing and `gate-runs` scratch never swept (HOST-020).
- **Security:** instance secrets aren't checked to be outside a repository (SEC-003). Herdr sockets are unauthenticated (SEC-001). *(Re-audit 5: the SEC-003 half was wrong; `daemon/layout.go:124-135` refuses an instance root under any `.git` ancestor.)*
- **Reads:** whole-table reads on the backlog verbs, `TaskHeads` and the global Prophet (CP-009). Topology is not a cached snapshot (ROB-020). The restart-read costs above.
- **Concurrency:** FanOut is sequential (SCH-009). An unproven resident turn retirement pins its identity until restart (`runtime/resident.go:340-342`). One stale foreign Herdr socket fails the census host-wide, and idle maintenance then skips silently.
- **Unchanged since re-audit 3:** the shipped `commit` stage has no Reverify route (ROB-013); schema 2 isn't enforced on write (WF-030); API-010/-020 and CTL-030 remain.

**Since re-audit 4 (wave 5, 2026-10-25; re-audited above).**
- **Fixed:** WF-074 under the operator's ruling to keep per-task limits; WF-021; RT-005. *(Re-audit 5: RT-005 was partial; wave 6 completed it.)*
- **ROB-021:** the checking plane reads of Admit, run-wide `resume` and `backlog edge`, and dispatch's dependency read, now run off the writer lock. The other housekeeping I/O listed in `daemon/README.md` still holds it.
- **Weakenings above, now addressed:**
  - Restart reads use task columns and a cached finished-task summary.
  - Status holds `d.childMu` only to copy the child's identity, and Topology is a cached snapshot.
  - The 60 s idle census has a test.
  - The hook exits 2 on its former exit-1 paths and reads words with shell quoting removed.
  - Each conformance proof is bound to its row's verb and exit code.
  - Every shipped generation's hash is pinned.
  - A refork change records a withheld reason.
- **Also fixed:** an unproven resident retirement no longer pins its identity, shared roles get one resident identity per concurrent turn, and a skipped idle-maintenance round is recorded.
- **Doc claims:** every one listed above is corrected.

Details are under Completed in wave 5.

**Previous re-audit, 2026-10-24 (after wave 3; superseded above).** Nine parallel per-section audits of all 284 IDs compared the tree against the post-wave-2 re-audit. They were static reads, plus one in-memory engine probe; the per-section reports were not committed.

| | Met | Implemented-unproven | Partial | Missing | Contradicted | N/A |
|---|---|---|---|---|---|---|
| Before wave 1 | 77 | 34 | 131 | 32 | 8 | 2 |
| After wave 2 | 155 | 29 | 90 | 3 | 5 | 2 |
| After wave 3 | **206** | 22 | 50 | **1** | **3** | 2 |

**What moved:**
- **Delivery phases** whose exit criterion is demonstrably met went from 6 to 8 (1, 2, 3, 4, 5, 7, 9, 11). Phases 3 and 7 joined through the full-library generated corpus and the scripted plus live crash matrices.
- **By section:** most improvement in VCS/robustness (14→25 Met of 32), host/observability/security (12→19 of 34) and scheduling/runtime (15→23 of 28).
- **Fixed:** the three post-wave-2 regressions and every previous "largest gap" except FanOut concurrency.
- **Still missing:** CLI-051 `import-legacy` (format unspecified).
- **Contradicted:**
  - GEN-010 and SEC-006: off-host backup replication, an open operator decision. The UI half of GEN-010 is fixed.
  - PLAN-001: historical.
- **No longer contradicted:** CLI-011 and WF-021.

Two ratings went down. Both are re-ratings, not code regressions:
- **ARC-002** (Met → unproven): wave 3 added idle work (a 5 s snapshot worker and a per-round own-tree census). The idle test asserts 250 ms of CPU per 30 s, five times the spec bound, and no measurement exists for this tree. *Wave 4: snapshot on demand, idle census once a minute, the test asserts the spec rate, and two full 10-minute windows measured 324 and 353 ms.*
- **CP-012** (unproven → Partial): `resource_pools.payload` is unbounded, and the 64 KiB inline limit can't be configured. *Wave 4: `serve --inline-limit`, and lease pools spill to blobs (migration 10); `ticket_emissions.payload` is still unbounded.*

Wave-3 regressions, verified in source (**all fixed in wave 4**, each with a test that fails on the wave-3 tree; see "Completed in wave 4"):
- **Lazy terminal loading has visible symptoms** (API-001, OPS-011, CLI-015, CLI-013). Startup no longer loads finished tasks, but several readers still use the in-memory job map:
  - After a restart, `board`/`status --tasks` with `--include-retired` omit every task finished before it (`daemon/reads.go` board reads the status snapshot).
  - `backlog edge` reports a Done prerequisite as "missing" in its readiness effect (`daemon/backlog.go`, `readiness(g, d.jobs, …)`). The scheduler itself reads plane rows and is unaffected.
  - `resume` misreports a finished target as unknown.
- **Raise-and-resume can't finish a task** (WF-074, OPS-010).
  - Resume of an AttemptCap escalation now enters the first stage (`engine/control.go`) without resetting test-run or loop counters.
  - An in-memory probe of the shipped default hit AttemptCap at 20, raised the limit to 100, resumed, and replayed from intake into TestRunCap at 8.
  - The spec doesn't say whether a restart resets counters, so this needs a decision.
- **Existing instances keep the wave-2 default.** `defaults.Generation` stayed 2 while `Shipped()` changed (request judge, no-red route, schema 2), so an instance first started on wave-2 code is never upgraded. Bump the generation before any such instance is upgraded.
- **New writer-lock and serialisation waits:**
  - The release-time refork (`daemon/worktree_base.go`, `forkAtRelease`) runs git commands and a plane write under `d.mu`, so an Admit can wait on it (ROB-021; the `daemon/README.md` no-wait claim is too broad).
  - Task-bound resident launches start a Herdr server while holding `ResidentMultiplexer.mu`, so launches across tasks serialise (SCH-009 intent).
- **Admit with an existing task id** and an explicitly different idempotency key can re-fork that task's pristine worktree before the primary-key refusal (WF-005). This is a static finding.
- **Carry-forward base:** a carry-forward task forks from its source's base, contradicting VCS-001's "from the current integration tip". Documented decision; reconsider.

Doc claims the auditors found overstated (corrected below where they were HANDOFF's):
- **SEC-007 is Partial:** the reviews gate compares counts of domains and citations; it doesn't match them.
- **Stall retry:** the "restricted-effects proof" is always true by validation. Retry safety comes from `CanRetryStall`.
- **Named commits:** request-key and migration-table writes land in the next named commit, not their own.
- **CLI conformance** (all three fixed in wave 4):
  - One cited proof (the `heavy-op hook` exit 1) is false.
  - `watchdog --json` mixes a text heartbeat into its JSON.
  - `fae --help` exits 2.
- **Dolt port-clash remedy:** the refusal suggested `serve --port`; the flag is `--dolt-port` (fixed in wave 4).

Largest remaining gaps:
- **Verification:** CI has never run. Seven wave-3 pure guards are outside `verify.sh` and the ledger census (`CanCarryReview` has no direct test). The I/O-before-lease ordering of the shared `controller.HeavyAdmission` isn't the Gobra-verified `CanEnqueueHeavyLease`.
- **HOST-011:** the pre-tool hook is registered nowhere, and the leased role-shim path is unreachable (operator decision).
- **Heavy work outliving a reclaimed lease:** landing gate, `gate run` and roll-build processes have no inventory record or parent-death kill, while every ledger access now reclaims dead owners (HOST-014/-021/-031).
- **Instance secrets:** the credential, plane password and UI token are not checked to be outside a repository (SEC-003). Herdr API sockets are unauthenticated Fae-launched listeners behind a 0700 directory, with no recorded decision (SEC-001).
- **Whole-table reads remain** on the backlog verbs and `land`, and the global Prophet fails past 1,000 jobs (CP-009).
- **Shipped `commit` stage** has no Reverify route (ROB-013). Topology reads aren't cached snapshots with an age (ROB-020).
- **API surface:**
  - Roll, maintenance, `gate run`, then-roll, `heavy-op run` and `cleanup scratch` stay client-orchestrated (API-010).
  - Turn-collected, host-command, stage-enter/leave and heavy-lease-release events are missing (API-020).
  - Landing gates are Go-only (CTL-030).
- **Schema 2 isn't enforced on write** (`ValidateWrite` unwired; WF-030/-001/-042). FanOut is still sequential.
- **Residue:** landing scratch and `gate-runs/<id>` are never swept (HOST-020). *(Wave 11 removes and sweeps landing scratch; `gate-runs/<id>` is removed on exit.)* `alerts.log` is unrotated. `HeavyAdmission` readings aren't recorded with avg60 and direction (HOST-005).

**Previous re-audit, 2026-10-23 (after wave 2; superseded above).** Nine parallel per-section audits of all 284 IDs, comparing with the wave-1 baseline. They were static reads with spot-checks; the per-section reports were not committed.

| | Met | Implemented-unproven | Partial | Missing | Contradicted | N/A |
|---|---|---|---|---|---|---|
| Before wave 1 | 77 | 34 | 131 | 32 | 8 | 2 |
| After wave 2 | **155** | 29 | 90 | **3** | 5 | 2 |

Delivery phases whose exit criterion is demonstrably met went from 2 (1, 5) to 6 (1, 2, 4, 5, 9, 11).
- **Still missing:** CLI-051 `import-legacy`, PLAN-002 (development guide) and SEC-007 (security guidance in reviews).
- **Contradicted:** GEN-010 and SEC-006 (off-host backup replication; GEN-010 also because wave 2 put the operator web UI itself into the core binary, which non-goal 6 excludes), WF-021 (operator Resume of an AttemptCap escalation adds a third Escalated out-edge), CLI-011 (implicit dry run for `run --mode normal`) and PLAN-001 (historical).

Regressions introduced by the waves, verified in source (all fixed in wave 3):
- An idle daemon stops republishing its status snapshot (`daemon/idle_gate.go`), so `Board` and the other cached reads fail `snapshot-stale` 10 s after the last activity (ROB-020).
- `land --then-roll` builds its controller client without `WatchdogIdentity` (`cmd/fae/land.go`), so a roll after a landing skips the watchdog sandbox acceptance (CTL-001).
- The built-in UI pages in `ui/` are in the core `fae` binary (GEN-010 non-goal 6). The generic UI-child supervision (API-033) is right; the UI itself should ship separately.

Further weakenings reported by the auditors:
- The maintenance gate reads a file mirror written after the plane commit.
- RPC handlers (land, prophet) can still `workers.Add` during Close.
- `heavy-op run` takes one host reading, with no two-reading I/O gate.
- The idle leak census runs every 10 min.
- `failed()` labels cumulative any-reason stage failures as SameGate.
- The simulator lacks SameGate, the progress discount and request caps.
- Unloadable job rows quarantine instead of refusing start (WF-093).

FAE-VER-006 tension (operator decision): the spec requires injected configuration in tests, while the operator chose real host pressure as the test default.

Largest remaining gaps:
- **CI:** CI still installs no Dolt, so about half the daemon tests skip there (VER-002).
- **Unwritten tables:** 7 record tables have no writer (CP-011).
- **Worktree base:** worktrees fork from a pinned base, not the integration tip (VCS-001/-008).
- **Reconcile cost:** a clean reconcile re-runs the whole approval chain (VCS-005a).
- **Drive-ending errors:** some outside the per-task classifier still end a generation (ROB-001).
- **Kernel bypass:** operator controls, carry-forward and migrate bypass `CanTransition` (WF-008).
- **Governed requests:** the shipped default silently drops them; it has no request policy or no-red route (WF-078).
- **Resident sessions:** gaps in RT-011/-012.
- **Controller roll build:** it skips host admission beyond its lease (HOST-001/-007).
- **Launcher socket:** the controller launcher socket is unauthenticated (SEC-001).

The gap table below predates the re-audit and stays as context.

| Priority | Requirements | Remaining gap |
|---|---|---|
| **P1 — re-audit** | all | Re-run the per-requirement audit against the current tree before any compliance claim. |
| **P1 — data-model remainder** | FAE-CP-011, -009; FAE-ARC-002 | The sessions, external_effects, code_reviews, schedules and engine_runs tables still have no writer; leases and ticket_outbox stay unused in favour of `resource_pools` and `ticket_emissions`. Startup still loads every job snapshot. |
| **P1 — secrets already persisted** | FAE-SEC-003 | New leaks stopped in wave 1, but earlier maintenance markers put the daemon environment into Dolt history, events and backups. Purge and credential rotation are operator decisions. |
| **P1 — recovery remainder** | FAE-ROB-012, -001, -042; FAE-VER-010 | The crash matrix covers scripted boundaries only, not live ones. A loop-collection error still ends a drive generation. Legacy rows without a disposition are driven as Local rather than quarantined. |
| **P1 — formal coverage** | FAE-VER-001..004 | Eight new pure guards are not in `verify.sh` (listed under Final verification). 8 ledger entries have unknown native evidence; 183 requirement IDs have no mapped check. |
| **P2 — default-content follow-ups** | FAE-WF-073, -078, -085; FAE-CTL-030 | The shipped default has no no-red route or request judge, so requests in its ImplementationReport are accepted but ignored. Landing runs templated QA stages literally. |
| **P2 — scheduling and admission details** | FAE-WF-030; FAE-RT-010; FAE-HOST-015 | FanOut branches run sequentially (one in-flight dispatch per task). A Prophet turn refused by host admission fails over to all-routes-unavailable instead of deferring. `daemon/land.go` samples `hostguard.Observe` directly. The per-run process cap applies only where cgroup pids is delegated. |
| **P2 — controller and CLI remainder** | FAE-CTL-030..033; FAE-CLI-051 | Landing gates are Go-only, conflicts always hold, and an interrupted landing has no lease journal. No `import-legacy` (format unspecified). |
| **P2 — boundaries (unchanged)** | FAE-MODE/EXT/SEC-004; FAE-OPS-012; FAE-WF-001 policy hooks | Real vendor conformance is not certified; containment has no integration-subject fallback; policy hooks are not defined concretely enough in the spec to build. |
| **Spec conflicts** | FAE-GEN-010; FAE-CP-010 | Operator-requested off-host backup replication sits outside §9's external-effect capability; amend the spec or gate it. Dolt DDL is not transactional (documented deviation). |

Additional limits of the backup extensions are **not automatically missing original requirements**: remote storage retention/credentials, calendar or multiple backup policies, hard filesystem quotas, automatic authority renewal and cleanup of unique/older/incomplete archives remain intentionally unsupported.

## Prioritized next-work plan

1. **Fix re-audit 12's findings (wave 14),** most severe first (details under Requirements audit):
   - HOST-006: let a first session's host deferral stay a park re-decided each round (it launches nothing), and bound only what spends provider turns (the later-session rerun), or ask the operator for a host-condition escalation kind bounded in time;
   - CTL-042: record a cleaned task without writing the plane (the archive ref present and the branch gone is git-only evidence), or ask the operator to amend CTL-042;
   - the landing filter: count a named test that skips as not passed; check the filter at admission level by level, as `go test` does; say in the rollback note that a plain name stored before wave 13 now matches by substring;
   - WF-093: run the revision check before the schema migration and the lifecycle events; keep the quarantine flag out of roll and maintenance relaunches (or record the quarantine durably); don't count terminal ticketed tasks as active pins;
   - RT-004: replay refunded a crashed turn none of whose sessions reached `launching`; type Merge's and VcsCommit's refusals before their write as refused before launch; wrap the broker's launch-gate failure as `RefusedBeforeExec`; keep refused-before-launch when a later release fails;
   - `fae cleanup done`: count only planned tasks toward `--limit` (or mark skipped ones); Topology: keep Retired tasks out of the default snapshot's bound; a collected delete: let it pass the cap wherever it falls in the round;
   - smaller: `schedule create` priority 0; `controller.Git`'s trim in the roll guard; the `"` cut in redaction and `secretEscapeWidth`; the total start bound checked before the start; TaskGet and the TUI showing the values the turn in flight uses; a test of the bridges' `WaitDelay`; the doc claims listed under Requirements audit.

   Then run the Fae-on-Fae acceptance on the new tree with a new, less scripted task (which also shows, with the parent now named, what raised the QA `process-leak` alerts), and a few ordinary tasks on another Go repository, driving them from `fae task` and `fae-tui`. Then confirm the decisions of waves 13 and 14 (listed under Decisions and boundaries), WF-093's refinement first. Support for non-Go repositories (derived scope for script and runner REDs, a configurable formatter stage and QA count, landing gates that aren't `go test`, a test filter that isn't `go test -run` syntax, a consumed `test_shape`, pins that can change without a new instance) is the next larger piece of work, to be discussed with the operator. Also give the test fixtures a free Dolt port: the port derived from a temporary directory (3658 to 7753) collides with any long-running listener on the host, as wave 13's second test run did on 6767.
2. **Self-hosting follow-ups** (see Completed in waves 7 to 13):
   - Herdr and resident transport-local start failures (socket path, fifo) still count toward the start bound and charge the binding Unavailable at exhaustion;
   - a later review session's stall still parks UncertainDispatch;
   - a bridge crash between edit renames leaves part of a turn's edits (UncertainDispatch, then StaleBranchResidue);
   - nothing limits how many nested calls run at once beyond the run's parallelism and the task cap;
   - nested git's check-then-exec race is narrowed, not closed; programs a test repository's own configuration names, and programs git and gcc find on PATH, still run;
   - script and runner RED shapes still need a pinned identity;
   - Dolt-backed tests skip in engine QA (the broker passes no `FAE_TEST_DOLT_BINARY`), so `scripts/test.sh` stays the gate before taking landed commits back;
   - what raises the QA `process-leak` alerts needs a live self-host run (wave 13's alert names the parent).
3. **Operator decisions.** Resolve the wave-3 decisions below, the HOST-011 conflict first: either amend the requirement, or add a restricted shell tool with the pre-tool hook. Then confirm the decisions of waves 4 to 13 (listed under Decisions and boundaries), including:
   - wave 5's: a raised task whose other budgets are spent escalates later rather than being refused up front; the finished-task summary's memory (CP-009); the withheld reasons; the engine broker's git refusal staying 77; the 10-minute idle refusal interval;
   - wave 4's: carried tasks waiting Pending, the hook's 0/2 exit codes, the content-versus-invocation exit-code rule, and the idle census cadence;
   - wave 8's, including account mode's unbounded concurrent nested calls;
   - wave 9's: the brokered Go environment (`GOTOOLCHAIN=local`, `GOENV=off`), the nested go and git scopes, binding refusals that spend no failover rotation, and frozen reasons not checked again;
   - wave 10's: the all-refused park and its 30-minute clock, charging a later reviewer's refusal, deadline-killed commands as failed runs, no git exec-path mirror, the nested go environment allowlist, Codex logins by shape and fail-closed landings;
   - wave 11's: cut-off tests as failed runs, no hooks in nested git, the route clock running until a route launches, cgo pinned, the flake rerun and the protected-path list (its charged later-reviewer start failure and report-only revision checks were overruled on 2026-10-29 and 2026-10-30);
   - wave 12's: every reviewer rerun after a later session's non-execution, SameGate by class for refusals before launch, any stdout byte as acknowledgement, withdrawal as retirement, structured fields frozen during a turn (changed in wave 13), the stricter flake rerun, an explicit `fae cleanup done`, WF-093's refusal only for a revision this binary can't read (refined in wave 13), and the charm dependencies;
   - wave 13's: WF-093's refusal while an active task is pinned to an unrunnable revision with the quarantine flag, the deferral and total start bounds, a later session waiting 30 minutes for the host, a later session's failover still charged, `go test -run` filter syntax, structured edits from the next dispatch, `test_shape` unconsumed, the two-census defunct grace and `fae cleanup`'s 200-task limit.
4. **Formal coverage.** Put wave 3's new pure guards into `verify.sh` with Gobra specs and weakened mutations, and keep raising the mapped-requirement count (134 of 284). The guards are listed under Final verification.
5. **Remaining partials from the stream reports:**
   - the commit stage's Reverify route (ROB-013), so stale verdicts at commit re-validate instead of escalating;
   - wiring `ValidateWrite`, and moving fixtures, `examples/workflow.json` and dogfood to schema 2 (WF-030);
   - concurrent FanOut branches, using rt's design (WF-030);
   - landing gates beyond Go, and a pre-tool hook that refuses Local push (CTL-030, MODE-002);
   - the client-orchestrated verbs (API-010);
   - removing default-content names from `daemon/live.go` (WF-008);
   - `alerts.log` rotation;
   - the remaining writer-lock I/O (the housekeeping steps listed in `daemon/README.md`; ROB-020/-021);
   - bounding `ticket_emissions.payload` and collecting unreferenced spilled blobs (CP-012);
   - whole-table reads on the backlog verbs and `TaskHeads` (CP-009);
   - a task committed but uncached by a lost acknowledgement waiting for restart;
   - a supersession Retire's edge transfer (`transferRetiredEdges`) reads every backlog edge;
   - the gate wait and heavy-op lines carry OBS-001's fields on stderr only (wave 11), not in the instance's log file.
6. **Herdr census robustness.** A single stale foreign Herdr socket blocks every instance on the host. Decide whether a server whose socket and working directory are both deleted counts as dead.
7. **CI.** Run `.github/workflows/test.yml` on GitHub once, and confirm the Dolt tarball layout and runner memory (16 GB or more).
8. **Release census.** Resolve §19, then native, formal and dogfood verification on a versioned release.

For every round: assign disjoint ownership, snapshot the uncommitted tree as the base, integrate in a short-path worktree (long paths overflow Unix-socket limits), run the full suite with real pressure and `verify.sh` on a quiet host, confirm the main tree still equals the snapshot, apply the diff, then dogfood. Update this handoff from current evidence.

## Reproduction and operating notes

Run from `/home/lbethay/workplace/fae` on Ubuntu 24.04. Installed prerequisites: Go 1.27.2 (`/usr/local/go`, on PATH via `~/.profile`), Dolt 2.5.1 (`/usr/local/bin/dolt`), Git 2.55 (`ppa:git-core/ppa`; ≥ 2.45 required), Herdr Core 0.1.18 at `~/.local/bin/herdr-core` (daemon tests take `FAE_TEST_HERDR_BINARY`, else `herdr-core` on PATH), Claude Code 2.1.309 (`~/.local/bin/claude`, logged in), Codex CLI 0.163.0 (npm, not logged in), lsof, ripgrep, JDK 17, Z3 4.8.12, Bison 3.8.2, SBT 1.13.0 (`/usr/local/sbt`). The formal tools are built under `.tools` by `scripts/install-verifiers.sh` (which rewrites two tracked build logs). Keep build/test scratch on disk under `.cache/`; never run a test against a developer or historical plane. Preserve unrelated existing processes and instances. Run tests from a short checkout path: Unix socket paths overflow under long worktree paths. Daemon live tests use real host PSI and load by default; `FAE_TEST_QUIET_HOST_PRESSURE=1` (passed through by `scripts/test.sh`) reports them as quiet for development runs on a loaded host. Memory and swap are always real. Run the full suite alone. First check `pgrep -af 'herdr-core serve'`: a leaked test Herdr server whose socket was deleted makes the host-wide Herdr census fail closed, and every multiplexer admission on the host defers.

```sh
export PATH=/usr/local/go/bin:$PATH
FAE_TEST_DOLT_BINARY=/usr/local/bin/dolt FAE_TEST_HERDR_BINARY=$HOME/.local/bin/herdr-core \
  bash scripts/test.sh                                            # ~80 min (daemon package ~67 min, run last and alone; then hostile and residue checks)
bash scripts/verify.sh                                            # ~11 min, rewrites verification/ evidence
python3 -I scripts/check-ledger.py --report
GOCACHE="$PWD/.cache/go-build" go build -o bin/fae ./cmd/fae
GOCACHE="$PWD/.cache/go-build" go build -o bin/fae-claude-bridge ./cmd/fae-claude-bridge
GOCACHE="$PWD/.cache/go-build" go build -o bin/fae-ui ./cmd/fae-ui   # optional operator UI (serve --ui finds it beside bin/fae)
GOCACHE="$PWD/.cache/go-build" go build -o bin/fae-tui ./cmd/fae-tui # optional terminal UI (tui/README.md)
```

Full acceptance command (choose a fresh evidence directory; the daemon's PATH must contain `go`):

```sh
python3 scripts/dogfood.py --provider claude --model claude-opus-5-5 \
  --dolt /usr/local/bin/dolt --claude ~/.local/bin/claude --herdr ~/.local/bin/herdr-core \
  --resident-sessions --provider-loop --crash-during-turn --cron-schedules \
  --controller-lifecycle --drive-restart --deployment-roll --prophet-sessions \
  --target-configs --retirement --normal-targets --census-cleanup \
  --dynamic-provisioning --scratch-cleanup --fleet-provisioning --adapter-retirement \
  --backup-policy --retention-automation --backup-schedules --backup-storage \
  --evidence-dir verification/results/NEXT-UNIQUE-RUN
```

The run pins `CLAUDE_CONFIG_DIR` (default `~/.claude`) and performs real model calls on that account. Claude Code creates `$CLAUDE_CONFIG_DIR/.claude.json` and session transcripts there. Freeze production **and test** inputs before final verification; build the binaries, snapshot hashes, run native/formal/real-provider checks, and verify manifests again before publishing the record. Do not rebuild `bin/fae` while dogfood is running. A passed old record does not certify later changes. Optional provider/account availability failures must be reported honestly rather than counted as passes.

## Decisions and boundaries to preserve

- Keep Herdr support; do not substitute tmux.
- Claude Code is the default provider (user decision, 2026-10-22). Keep the Codex bridge working; both share one protocol core. Credentials stay explicit in the pinned target environment; never copy OAuth credential files.
- Keep configurable adapters until the operator selects a real vendor and credentials; schema discovery alone is not proof of vendor behavior.
- No implicit replay of uncertain external mutations, force-unpin, unsafe lease reclamation, or quota-driven deletion of recovery evidence.
- Preserve exact authorization, pinned revisions, source identity and historical cleanup coverage.
- Full local dependency installation is authorized. No commit/push, production deployment, arbitrary remote deletion or project rename was requested in this round.
- Backup schedules are opt-in singleton interval policies with normal grant expiry; they do not renew authority automatically.
- Operator decisions surfaced by wave 1 (implemented as stated; confirm or change):
  - **Secrets:** purge and rotate credentials that leaked into Dolt history and backups before wave 1?
  - **Off-host replication (GEN-010):** amend the spec, or gate it behind Normal live?
  - **Defaults and routes:** newest-live-revision role caps (§19 Q3); default routes claude-opus-5-5, claude-sonnet-5-5, codex gpt-6-astra (instances serve one provider, so filter routes per instance); existing instances' default revision needs an upgrade path.
  - **CLI semantics:**
    - A missing authorizing message exits 1.
    - ~~`run --mode normal` without `--dry-run`/`--commit-external` is a dry run.~~ Changed in wave 3: exactly one is required (CLI-011); an unstated mode on a `--default-mode normal` instance is still never live.
    - Retiring the built-in default workflow is allowed.
    - Relayed controller messages never authorize.
    - The heavy-op refusal code is 75; `gate run` uses 75 (timed out) and 76 (refused).
  - **Escalation tuning:** SameGate escalates on the 4th consecutive same-reason failure (since wave 3, for stage failures too, reset on success or a new reason); the ProviderUnavailable bound is 30 min (Config only).
  - **Maintenance and leases:**
    - Strict maintenance refusal during a roll drain: should ResolveUncertain be allowed?
    - ExternalEffect, Freeze and Compare still take a weight-1 heavy lease.
    - Dead foreign owners' leases are reclaimed even if their children may survive.
  - **Observability:** log rotation 64 MiB × 4; leak alerts only beyond (not at) 200 processes / 4 GiB / 20 defunct.
  - **Landing:** flakes and frozen checks come from the pinned target's `landing` policy since wave 3 (were passed per landing); salvage doesn't verify that the escalation was infrastructure.
  - **Process limit:** HOST-015's per-run process limit needs a value and a cgroup approach.
- Operator decisions surfaced by wave 2 (implemented as stated; confirm or change):
  - **Data model:**
    - `molecule_concurrency` is optional in workflow content.
    - A running molecule is one with a task in a stage or Blocked.
    - An ended molecule reopens when a new task binds its revision.
    - Legacy history without transitions stays in the snapshot only.
  - **Primitives:**
    - FanOut branches run sequentially.
    - HumanDecision parks in its stage rather than Escalated.
    - Escalate `reason` and Gate `name` are optional so stored revisions still load. Since wave 3, schema 2 requires them, and schema-1 revisions still load.
    - Sub-workflow children always run Local.
  - **Policy:**
    - A RED-rework rejection and a failed required check escalate `HumanDecisionRequired` ("RedRework", "RequiredCheckFailed"); no Appendix B kind fits.
    - A commit with no RED, no classifier and no declaration is still allowed (legacy behaviour).
  - **Normal default:**
    - Dry runs walk the review sub-workflow, recording intents only.
    - Tickets are mandatory for Normal tasks.
    - An opted-out review still closes the ticket.
  - **Recovery:**
    - A completed, exactly remeasured effect receipt is adopted without an operator verb.
    - Quarantine retire holds the row without an OPS-011 Retired transition.
    - An executor-kind mismatch quarantines instead of refusing start.
  - **UI:**
    - Default port 18658 + hash mod 4096.
    - The token is regenerated per start.
    - UI writes need a verb-exact authorizing message.
  - **Runtime:**
    - Per-run process cap of 512.
    - 5 s reaper grace for unowned zombies.
    - Role shims refuse all git and non-Go wrappers.
    - A build-workspace package refuses live execution until a binding provider exists.
  - **Schedules:** schedule runs deferred by host admission withdraw their claim.
- Operator decisions surfaced by wave 3 (implemented as stated; confirm or change):
  - **HOST-011 conflict:** provider bridges run with no tools and `CanAcceptClaudeTurn` admits structured output only, so a role's build/test grant can't run and the pre-tool hook guards nothing. Either amend HOST-011 to sessions that can execute commands, or enable a restricted shell tool with the hook registered (a provider change, plus re-verifying the guard).
  - **Resume (WF-021):** Resume of an AttemptCap escalation restarts at the first stage. ~~Since wave 4 it starts a new pass: loop, test-run and request counters reset (prior values audited); attempts carry on against the raised limit.~~ Changed in wave 5 by operator ruling: it refunds nothing (see wave 5's decisions).
  - **Migrate:** remaps only along edges of the new revision.
  - **SameGate:** covers repeated execution, provider, malformed and validation failures with the same reason.
  - **Unstated mode:** an unstated mode on a `--default-mode normal` instance is still a dry run. External effects always need `--commit-external`.
  - **Two-reading I/O gate:** every standalone `heavy-op run` / `gate run` waits at least 30 s (HOST-010; there is no chain shared across processes).
  - **Process cap:** degrades to RLIMIT_NPROC where cgroup pids isn't delegated (HOST-015).
  - **Stall retry:** on by default, resting on the restricted capability set; `stall_safe_replay` is now redundant.
  - **Role waits:** no time bound (owner death or cancel still removes a waiter).
  - **Withheld trace:** a line every round while a task is withheld.
  - **Commits:** drive commits are per checkpoint, named by round and task.
  - ~~**Board after restart:** shows active tasks plus those finished since start.~~ Changed in wave 4: Board and `status --tasks` also page tasks finished before the start from the plane (API-001, OPS-011).
  - **Integration retry:** `integration-retry-loop` is 1, and the oracle doubles as the request judge.
  - ~~**Carry-forward base:** a carry-forward task forks from its source's base, not the tip.~~ Changed in wave 4 to follow VCS-001: it forks from the current tip, waits Pending until released, then enters its carried stage.
  - **Typed refusals:** integrate refusals escalate IntegrationConflict; commit refusals escalate HumanDecisionRequired "CommitRefused".
  - **CI:** keeps real host pressure, with a repository-variable escape hatch.
  - **Rollback compatibility:** the new strictly-decoded snapshot fields (`Carries`, `AdmittedAt`, `Unbounded`, `superseded_acceptance`) can't be read by older binaries.
  - **Herdr census fragility:** one stale foreign Herdr socket anywhere on the host blocks every instance (SCH-012 fail-closed). Consider treating a server whose socket and working directory are both deleted as dead.
- Operator decisions surfaced by wave 4 (implemented as stated; confirm or change):
  - ~~**Resume counters (WF-074):** an AttemptCap resume resets loop counts, the test-run count, loop-progress streaks and the scope-widen/RED-rework request caps, despite Appendix A's "per task" wording; restart-with-information keeps every counter.~~ *Re-audit 4 rated this Contradicted. The operator ruled to keep per-task limits; wave 5 refunds nothing.*
  - **Carried tasks:** Pending until released by their edges (before, they started in their carry stage at admission); carried spec reviews are not re-run on a moved base. The new `carry_entry` snapshot field can't be read by older binaries. *Re-audit 4: consistent with VCS-001, but an operator Escalate or Cancel while Pending strands the task (WF-021). Fixed in wave 5: leaving Pending drops `carry_entry`, and every operator control validates its result before saving.*
  - **Hook exit codes:** `heavy-op hook` exits 0 (decision on stdout) or 2 (block, fail closed), never 1, following the provider pre-tool protocol rather than CLI-001's "1 = refusal". *Re-audit 4: judged consistent with HOST-011; a missing `$HOME` and a failed stdout write still exit 1. Wave 5: both exit 2.*
  - **Exit-code rule:** exit 2 is for the invocation itself (flags, operands, unparsable flag values); bad content in a named file, or content contradicting a flag, exits 1.
  - **Inline limit:** `serve --inline-limit` accepts 4 KiB to 64 KiB, not 0; spilled blobs are never collected.
  - **Idle census:** while idle and unchanged, the own-tree leak census runs once a minute (descendant walk every 5 s), reading HOST-022's "every round" as every drive round. *Re-audit 4: judged a defensible reading.*
  - **Status freshness:** Status never waits for a refresh, so the first status after a long idle can report an old (correct) snapshot with its age.
  - **Reads after restart:** Board and `status --tasks` can answer `read-busy`/`deadline-exceeded` after a restart, because they now read finished tasks from the plane. *Re-audit 4: their cost grows with finished history (see Requirements audit). Wave 5: they read task columns with every filter pushed down, then a cached summary.*
- Decisions of wave 13 (2026-10-31; implemented as stated; confirm or change):
  - **WF-093, refined by the coordinator after re-audit 11** (the operator's "refuse to start" decision, narrowed so instances the committed binary created can still start; re-audit 12: it complies with WF-093's text, but such an instance with an active task on its old default still needs the flag): an unreadable revision refuses the start; a revision that loads but fails a later rule refuses it only while an active task is pinned to it, and `fae serve --quarantine-unrunnable-revisions` quarantines exactly those tasks for `fae migrate` or `fae quarantine retire`; otherwise it is listed. `workflow.Load` stays decode-only.
  - **Bounds:** at most 60 admission deferrals per agent turn *(re-audit 12: contradicts HOST-006)*, then ProviderUnavailable naming the host condition (Appendix A has no row; proposed "agent-turn deferrals before launch: 60"); at most 3 agent starts per declared binding per turn in total *(re-audit 12: 3 × bindings + 1, possibly all on one binding)*; a later review session waits up to 30 minutes for admission, holding the earlier reviews in memory, before giving them up.
  - **Charges:** a later session's provider failover stays charged while a first session's is refunded; `vcs-refused` (a write attempted) stays charged; a commit refused before any write escalates refunded; a runtime launch refusal with a changed tree is `rejected-change`. A host command the broker refuses before exec (a shim policy refusal, a missing tool) is a free refusal, so a repeated one escalates SameGate after its retries instead of parking UncertainDispatch.
  - **Test filter:** `go test -run` syntax everywhere (unanchored matches by substring; a filter with no `/` counts top-level tests only); the derived-scope RED id is quoted and anchored per level.
  - **Live task verbs:** structured fields apply from the next dispatch, which includes a redispatch of the same stage; `test_shape` is unified across admission, live `HostCommand` execution, and landing check receipt resolution (`landing-test-receipt-<id>`); TaskGet keeps the newest 1,000 edits.
  - **Host and cleanup:** a defunct process inside a running command is exempt for two censuses (about 10 s busy, up to about 2 minutes idle); `fae cleanup` plans at most 200 tasks per run by default and marks each cleaned task with a `worktree-cleaned` event and audit row *(re-audit 12: contradicts CTL-042)*.
  - **Nested git:** a dangling link anywhere the resolver runs is refused, inside a writable test repository's `.git` too.
  - **TUI:** polling backs off to 8× `--refresh` while nothing changes; a stale write offers a reload, never a retry.
- Operator decisions of 2026-10-30, implemented in wave 12:
  - **Deleting a task retires it and keeps its history.** A task that never dispatched is withdrawn; a running or parked one is escalated (its turn in flight collected, never abandoned) and then retired; it works on any non-terminal task; dependents are held or re-pointed (`superseded_by`).
  - **Editing a running task applies from its next stage.** The turn in flight keeps its text; each edit is audited and appears in the task's trace.
  - **WF-093: the daemon refuses to start on an unloadable stored revision,** naming it (the spec's wording).
  - **The TUI is a separate binary, read-only by default;** `--write` enables writes.
- Decisions surfaced by wave 12 (implemented as stated; confirm or change):
  - **Never-executed runs:** after a later session's start failure, refusal or deferral, the next dispatch reruns every reviewer, uncharged, rather than keeping the earlier reviews (keeping them would need a new stored field and invalidation rules). Refusals before launch count toward SameGate by class, not detail, so a varying detail can't loop. A later session's provider failover and `edits-not-applied` stay charged: the provider ran.
  - **Bridges:** acknowledgement is any byte on stdout; a silent clean exit is not started; a silent provider is charged Unavailable only when the start bound is exhausted, never Timeout health per start.
  - **Live task editing:** withdrawal is a retirement, not Cancel, so every deleted task ends Retired; declared files, test filter and test shape can't change while a turn is in flight; an update gives a `run`-admitted task a backlog row; `backlog admit`'s upsert now replaces the structured fields it names.
  - **Landing:** a flake is skipped only in its own package, so a same-named test elsewhere must have passed in the first run; a test skipped (`t.Skip`) in the rerun holds the landing.
  - **Nested git:** every call-set `hook.*` key but `enabled=false` is refused, harmless ones included.
  - **Startup:** every stored revision is read, with no overall bound (10 s per read); a refused start writes one alert per unloadable revision each time. "Unloadable" means this binary can't read the revision (`workflow.Load`: not canonical JSON, an unknown or mistyped field, an unknown schema version). A revision that loads but breaks a rule added since it was stored, such as the old default's `main` brief keys, is listed, not refused; otherwise every instance the committed binary created would refuse to start.
  - **Cleanup:** finished tasks' worktrees are removed by an explicit `fae cleanup done` (a verified plane backup each run), not automatically.
  - **TUI dependencies:** bubbletea v2 (v2.1.0) and its companions, which move `golang.org/x/sys` to v0.47.0; building the module offline now needs them in the module cache.
- Operator ruling, 2026-10-29 (RT-004, the attempt budget): "A run never executed shouldn't count towards the budget." It overrules the wave-10 and wave-11 decisions that a later review session's binding refusal or start failure charges the attempt, and covers every provable non-execution: start failures, binding refusals, local refusals before the provider runs, admission deferrals, and a provider silent until the bridge's timeout. The 3-start bound, SameGate for deterministic refusals and the route-exhaustion clock stay, so nothing loops. Wave 12 implemented it *(re-audit 11: two paths now loop without bound, and some never-started runs are still charged; see Requirements audit; wave 13 bounds them and refunds the rest, Herdr transport-local start failures aside; re-audit 12: except a crash before launch and Merge's refusals before its write)*.
- Operator decisions surfaced by wave 11 (implemented as stated; confirm or change):
  - **RED evidence:** a test cut off from outside is not counted and its run is a failed run, not uncertain (HANDOFF had suggested uncertain): repeated OOM kills in QA spend remediation attempts instead of parking at once. The broker's deadline kill no longer counts the running test either. Over the byte bound, results reported before the cut count and the exit code stands, so a QA run over the bound with exit 0 passes on the tests counted; a cut `gofmt -l` listing counts as debt. Every goroutine stack is left out of matching, pinned fragments included. Subtests are refused for derived scope only; a pinned identity is the operator's.
  - **Nested git:** an allowlist of `GIT_*` variables (a third-party one such as `GIT_LFS_*` is refused in account mode); no hook runs in any nested call, a test's own repository included (no Fae test relies on one; re-audit 10: a call's `-c hook.<event>.enabled=true` still passed; wave 12 refuses it); the pinned exec gives programs git starts `GIT_DIR` and `GIT_WORK_TREE`; paths a call names for git to write are checked against every writable place. The repository walk is bounded at 20,000 entries.
  - **Routes:** "the same refusal" is any time no route launches; operator actions start a new episode, a daemon restart doesn't. A health park that heals into start failures keeps its original clock. Resume refuses a health-only park whatever an earlier generation refused. The alert also covers a ProviderUnavailable escalation.
  - **Provider failures:** ~~a later reviewer's start failure keeps the attempt charged and reruns every reviewer, although RT-004 says a non-execution doesn't consume the attempt budget (the earlier reviewers ran); it shares the turn's start count.~~ *Overruled 2026-10-29 (see the operator ruling above); re-audit 10 rates RT-004 Contradicted until wave 12 changes it, and found that the shared start count charges the wrong binding. Wave 12 implements the ruling and keys the start count by binding.* `provider-failed` is MalformedOutput, charged, and SameGate escalates after four identical failures; the bridge's own timeout is always `provider-failed` *(re-audit 10: for a provider that wrote nothing, this contradicts RT-004 and the ruling; wave 12: a provider that wrote nothing is not started, timeout or not)*; a changed tree after it stays uncertain.
  - **Host:** cgo is pinned, not refused (Linux `-race` needs cgo); a call whose PATH has a compiler the run's PATH lacks is refused (77), and a nested test sees `CC=/usr/bin/gcc` and the like. The whole-suite pattern list is fixed and documented, `Test` and `^Test` included. Quarantined scratch is logged, never removed automatically.
  - **Landing and redaction:** a landing with a failing listed flake runs the suite twice; an unlisted test that flakes in the second run holds it. A Codex refresh token is matched by its field, not its shape. The streaming hold grows to about 25 KB for a typical secret set (up to about 270 KB for a 2 KB environment value).
  - **Startup:** an unloadable stored revision is reported, never a reason to refuse the start *(re-audit 10: this contradicts WF-093, which says the binary MUST refuse to start naming the revision; the operator decides; wave 12 refuses the start on a revision it cannot read, by operator decision of 2026-10-30)*; CP-006 refuses the start only for an event clock behind its events and quarantines diverged tasks. Empty-record removal needs the record to be 50 ms older than the process.
  - **Scope:** the protected-path rule judges model-chosen paths only (Spec `declared_files`, widens); the operator's `--declared-file` and pinned scopes are exempt; `defaults/` and `scripts/test.sh` are Fae-specific and not protected. An edit-write failure is a new class, ValidationFailed, charging no provider.
  - **CLI:** the `land --poll` floor is the watchdog's 10 ms; `runtime-config` limits and `--weight` keep refusing invalid values.
- Operator decisions surfaced by wave 10 (implemented as stated; confirm or change):
  - **Every route refused (RT-010):** the task parks `all-routes-unavailable`, as RT-010 requires, instead of wave 9's immediate escalation. The operator didn't rule to keep the escalation. The park shares the 30-minute route-exhaustion clock, which survives a restart *(re-audit 9: not when the refusal recurs; the new generation's dispatch clears it. Wave 11: it runs until a route launches)*. Resume is accepted on such a park and restarts the clock; past the bound the task escalates ProviderUnavailable.
  - **Charging:** a binding refusal or failover after a review dispatch's first session costs the attempt, and every reviewer runs again (earlier reviews aren't reused), so a rate limit mid-review now costs one attempt. A provider binary that can't start (a missing absolute path) or a CLI that exits on its own with no output is a start failure: three relaunches, then ProviderUnavailable against the binding and the next route.
  - **RED evidence:** `test timed out` stays a valid reason; a hung RED costs about 300 s plus its build. Any brokered command killed at its own deadline is a failed run (exit -1), so a required check that times out fails QA. The goroutine dump is left out of matching, for pinned fragments too *(wave 11: every goroutine stack)*. A number alone is refused (wave 9 accepted `42`). A fragment the test `t.Log`s is documented, not refused; a RED with subtests is stated in the protocol, not refused at freeze *(wave 11 refuses a derived RED that calls `t.Run`)*.
  - **Nested git:** no `GIT_EXEC_PATH` mirror. Git's own subprocesses (348 in a measured run of Fae's suite, Fae's own `worktree add` among them) would be refused, so a test repository's own configuration and hooks count as test setup *(wave 11: hooks no longer run in any nested call, and named config files and nested config writes are checked)*. `GIT_ALLOW_PROTOCOL=file` lets submodules clone local paths only. A nested `git config --global` without its own `GIT_CONFIG_GLOBAL` fails. Editors and pagers are refused except harmless ones (`:`, `true`, `cat`).
  - **Nested go:** the environment allowlist in Completed in wave 10. `-cpu` values above the clamp are refused; reserved patterns run as whole suites (one thread); external linking, plugins and c-shared builds are allowed. A go outside any GOROOT is reported (a daemon line and a `fae doctor` warning), not refused. The sealed mirror is not containment (same user). A `GOTOOLCHAIN` in a stage's environment is replaced by `local` without a warning.
  - **Redaction:** a Codex login is redacted by shape only (OpenAI `sk-…T3BlbkFJ` keys and JSON Web Tokens); a ChatGPT refresh token, a key without that marker and encoded shaped tokens aren't. A secret wrapped across lines leaks its tail *(re-audit 9: whole)*, and `xxd` layouts aren't matched. *(Wave 11 matches both, an unmarked key and OAuth token fields.)* Names that carry no secret (`GIT_AUTHOR_*`, `SSH_AUTH_SOCK`, `XAUTHORITY`, the askpass variables, `GNOME_KEYRING_CONTROL`, `PASSWORD_STORE_DIR`) are no longer redacted.
  - **Landing (CTL-030):** fail-closed. Two passing packages whose names merge under redaction also hold the landing; with `-count=N`, any failing repetition fails; any redaction error, a full disk included, holds the landing as infrastructure.
  - **Startup:** `Recover` deletes an empty process record older than the daemon's own start, with no audit row and no signal *(wave 11: a line, an audit row and an event)*; a journalled lease stays held. Any other unreadable record refuses the start, naming the file.
  - **Workflow writes:** install and `workflow validate` refuse a Claude route with effort `minimal`; a stored revision with one loads, and tasks on it are admitted. At run time its Claude binding is refused and the turn fails over.
  - **Controller:** a real hold adds a line, and on a landing one more `landing-admission-wait` row (an error alert). A role's `heavy-op run` under soft pressure can't lift within its re-check bound under the default policy (documented; wave 11 applies that bound to role-class runs only). `watchdog --period` has a 10 ms floor. `gccgo` and `tinygo` pass the hook.
- Operator decisions surfaced by wave 9 (implemented as stated; confirm or change):
  - **Brokered Go environment:** every brokered run uses a per-run GOROOT mirror with `GOTOOLCHAIN=local` and `GOENV=off`. A module whose go line needs a newer Go fails instead of switching toolchains, and the operator's Go env file (`go env -w` settings) no longer applies to brokered runs. The mirror costs no recompilation; a nested binary build relinks once per run.
  - **Nested go scope:** account-mode nested go and gofmt also accept an on-disk `/var/tmp`, where Fae's own tests keep short-path fixtures; `/var/tmp` counts as RAM-backed only when it is tmpfs or ramfs. The top-level command policy still treats `/var/tmp` as RAM-backed.
  - **Nested git:** a call may change only repositories in the run's TMPDIR, nested in its worktree, or in the system temporary directories; every other repository may only be read. A short list of harmless `-c` keys is allowed on those reads, because Go's VCS stamping passes `log.showsignature`. Push and remote stay refused everywhere, so a nested go that fetches a module straight from version control fails.
  - **Binding refusals:** they spend no failover rotation and refund the attempt. A refused binding is skipped until a daemon restart, Resume or restart with information. When every route is refused, the task escalates ProviderUnavailable at once, without the 30-minute park. *(Re-audit 8: RT-010 is Contradicted, because it says the action MUST park. Rule on it, or park. Wave 10 parks; see its decisions.)* Effort `minimal` on a Claude route is now a validation error *(wave 10: on writes only)*.
  - **Expected-failure reasons:** numbers, message words that also occur in the test name, panic messages and `timed out` pass; generic trace text and a bare `got` or `want` are refused. A frozen report's reason is not checked again, so tasks frozen under wave 8's rule keep their RED. *(Re-audit 8: `timed out` can never match, because a timed-out go test leaves an incomplete stream; and any token with a digit passes, so a lone `1` does. Wave 10 fixed both; see its decisions.)*
  - **Process records:** retirement proofs skip a malformed record that names another session; one that won't decode, or that names this session, still fails closed. Recovery stops a crashed publisher's stopped launch gate.
  - **Redaction:** the daemon doesn't read a provider login it isn't given; only `sk-ant-` shaped tokens from one are redacted. The daemon's own environment values are redacted by credential-like name (on this host that includes `SSH_AUTH_SOCK`'s path). A landing holds rather than keep unredacted gate output. *(Wave 10: OpenAI keys and JSON Web Tokens are redacted by shape too; `SSH_AUTH_SOCK` and other names that carry no secret are no longer redacted; a landing that can't redact holds as infrastructure.)*
  - **Controller lift lines** follow only a recorded wait, so a short wait stays quiet.
  - **Hook:** it denies only other names of shimmed tools, and only where a command starts; process managers and `uv run`-style commands are documented as passes.
- Operator decisions surfaced by wave 8 (implemented as stated; confirm or change):
  - **Bridge refusals:** a refused model answer is MalformedOutput and charges provider health; two in a row make a binding ineligible, so self-host roles rotate to their next route, and a single-route role parks all-routes-unavailable. A turn refused before the provider ran is ValidationFailed: it costs an attempt and charges no provider. In Prophet sessions too.
  - **RED retries:** a refused RedReport restores only the turn's RED file and retries test-first. Each retry costs an attempt, not a test run.
  - **Expected-failure reasons:** matched without go test's framing lines. A derived fragment made only of the test's name, file-name parts or go test words is refused; the rule errs towards refusing. Pinned values aren't checked for vacuity. *(Re-audit 7: WF-043 is Partial because no brief or contract states the rule before the turn; it also refuses numeric-only reasons such as `0 != 42`, and still accepts panic-trace words. Wave 9 states the rule and changed it; see wave 9's decisions.)*
  - **Sub-workflow scope:** a child's files are its parent's resolved scope, and they win over the child's own Spec.
  - **Account mode:** requires the daemon to be the `fae` binary. Nested push is refused with 78 for Local tasks and 77 otherwise; `send-pack`/`http-push` now give 78 for Local role sessions too. Nested `-o` is refused under RAM-backed roots. Nested calls are clamped one by one, with no per-run limit on how many run at once. *(Re-audit 7: the go policy is a denylist; git other than push and remote runs as called; and a Go test's own `go` reaches no wrapper. Wave 9 fixed all three.)*
  - **Configured values:** the fallback covers only flags with a spec default; the rest stay usage errors. `--home` and `--instance` fall back only when empty. A landing floor below the hard floor falls back to 12 GiB (it used to exit 76). Warnings go to stderr only.
  - **Edge edits** check only their own edge and any cycle it closes, so an unrelated quarantined task's edge no longer blocks every add.
  - **Redaction:** failed-run excerpts are redacted for the operator, plane, principal and UI credentials and credential-named live-config environment values of 8 bytes or more, in raw, hex and base64 forms. If the secrets can't be read, the excerpt is withheld. *(Re-audit 8: hex and base64 only for file credentials. Wave 10 matches every form for every secret.)*
- Operator decisions surfaced by wave 7 (implemented as stated; confirm or change):
  - **Task scope:** an instance whose live config pins no `files` derives each task's scope from the task: the admission's declared files, else the latest Spec's `declared_files`, plus approved widens. Pinned `files` and `tasks[id]` still win, unchanged. Declared files that don't exist yet are left out of tree snapshots.
  - **RED identity:** taken from the latest RedReport; after a freeze, only a new RED loop changes it. A derived RED needs a Go test name, so script and runner RED shapes still need a pinned identity.
  - **Bridge pins:** a pin (a `--task-scopes` entry or a non-empty `--files`) wins over the brief's `task_scope`; the two are never merged. A pin without `--test-file` refuses a different block test file only on change turns.
  - **Commit messages:** an empty `commit_message` gives each commit a subject from the task plus `Fae task:` and `Fae dispatch:` trailers, in pinned configs too.
  - **Nested commands:** `nested_commands` defaults to `refuse`, as before. `account` lets nested `go`, `git` and build tools run under the parent's lease and records each call. The records are evidence, not containment. *(Re-audit 6 found this was not HOST-014's accounted branch; wave 8 made it so. See wave 8's decisions.)*
  - **Output bound:** `max_output_bytes` is 1 to 64 MiB, default 4 MiB; the self-host config uses 16 MiB.
  - **Engine feedback:** at most 16 KiB per run and 48 KiB per turn. Excerpts leave out the `-x` build trace.
  - **Ownership records** keep at most 16 KiB of command arguments, then `... N more arguments`. Records up to 4 MiB are read back, so records written before the fix still load.
- Operator decisions surfaced by wave 6 (implemented as stated; confirm or change):
  - **Configured values:** an empty, out-of-range or non-finite configurable flag falls back to its default with a warning (REQUIREMENTS.md conventions); a malformed token stays a usage error (exit 2). *(Re-audit 6 found this held for `serve` and the controller verbs only; wave 8 extended it to every flag with a spec default but `provider-health --window`, which wave 9 added.)*
  - **Carried budgets:** Resume and restart-with-information reply with `carried` (used and limit for attempts, test runs, every loop, streaks and requests). TestRunCap, loop-exhaustion and streak escalations name the count, the limit and that it carries.
  - **Stall bound:** any state change ends it, and so does collecting a held dispatch.
  - **Rounds:** a mid-round pending checkpoint ends the round, withholding the undecided tasks as `checkpoint_unsettled`. A resident pane-limit refusal counts as `pane_limit`.
  - **Failed slot reconciliation** sets the slot aside as unretired for maintenance to retry, and defers the turn without charging the provider. `status` shows `scheduler.unretired_transports`.
  - **Global Prophet:** drive tasks plus tasks finished before the daemon started, at most 1,000 entries and 512 KiB, with `tasks_total` and `tasks_truncated`.
  - **Hook:** `.` and `source` in command position are denied; `env -u NAME` passes unless NAME is PATH. It stays lexical: unevaluable expansions, interpreters and `su`/`sudo`/`ssh` pass.
- Operator decisions surfaced by wave 5 (implemented as stated; confirm or change):
  - **Per-task limits (WF-074; operator ruling, 2026-10-25):**
    - An AttemptCap Resume re-grants attempts against the raised limit and re-enters the first stage, refunding nothing.
    - TestRunCap is checked before AttemptCap.
    - Restart-with-information records the same carried counters.
    - A raised task whose test-run or loop budget is spent can't finish. Resume doesn't refuse it up front; it escalates TestRunCap or LoopCap when it reaches the cap. One pass of the shipped default spends about 4 test runs and one spec-loop iteration.
  - **Stall retries (RT-005):** the bound is per stalled turn; the counter resets when a turn ends, by success or by a recorded failure.
  - **Finished-task summary (in tension with CP-009):**
    - Board and Status keep one row per finished task in memory for the daemon's lifetime, built lazily on first need.
    - Until it is built, a filtered page scans state ranges in Dolt (no index on mode or priority).
    - Migration 11 decodes every finished snapshot once to backfill.
  - **Freshness:** Topology is stale after 5 s and refreshed before answering after 10 s. `board --in-flight` skips the plane.
  - **Writer lock:**
    - Checking plane reads run before `d.mu` and are re-checked under it, rather than dropping the lock mid-RPC.
    - New withheld reasons: `edge_unread`, `release_changed`, `plane_unconfirmed`, `checkpoint_unsettled`, `draining`, `round_read`.
  - **Shims and hook:**
    - The engine broker's git refusal stays 77, because it can't tell Local from Normal. Local role shims exit 78 for `git -C dir push`. *(Re-audit 8: the rationale is stale, as the broker has known the mode since wave 8. Wave 10 gives a Local task's nested push 78 in refuse mode too.)*
    - The hook stays lexical (`/usr/bin/$x` passes).
    - Backlog actions take only their own flags.
  - **Resident identities:**
    - Slot 0 keeps the old identity; loops keep one identity.
    - An unretired transport holds its pane until it retires: one pane per unproven retirement, all within the host pane limit (corrected in wave 6; `runtime/IDLE_SESSIONS.md`).
  - **Idle refusals:** recorded when they start or change cause, then every 10 minutes while they last.
- Requirements section 19 remains an explicit decision list: host-weight/floor calibration, cross-revision role-cap semantics, multi-workspace direction, roll topology, global Prophet authority and vendor schema choices should be resolved explicitly when their work starts.
