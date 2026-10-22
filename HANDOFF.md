# Crook / Fae engineering handoff

Updated 2026-10-21 after the safe replication unpinning and backup storage quota round.

## Identity and current state

The user refers to this project as **Crook**. The repository, requirements, Go module and executable are currently named **Fae** (`github.com/xleelz/fae`, `fae`); this round did not rename them. The working directory is `/Users/lbethay/workplace/fae`.

This is a substantial working implementation, but **not complete compliance with REQUIREMENTS.md**. Later phases and operator-requested backup extensions coexist with unfinished earlier requirements. The numbered delivery phases must not be marked complete merely because a related package or test exists. The audit below distinguishes confirmed implementation gaps from acceptance criteria not yet demonstrated.

The checkout contains extensive pre-existing untracked implementation and evidence files and a modified go.mod. No commit, push or production deployment was performed in this round. Do not reset or clean the tree. Use the final source manifests to identify the tested state; a normal Git diff does not represent the full implementation.

## What has been built

| Area | Implemented behavior and primary locations |
|---|---|
| Durable control plane | Owned Dolt child, migrations, immutable revisions/bindings, transactional retry, audit/events, content-addressed large payloads, backups and restart reconciliation: `controlplane/`, `daemon/`. |
| Workflow and execution | Pure domain/kernel rules, strict workflow/schema validation, evidence-bound gates, RED/freeze/compare floors, durable stage preparation/completion, independent reviews, budgets and scripted execution: `domain/`, `kernel/`, `workflow/`, `engine/`, `defaults/`. |
| Real local effects | Brokered host commands, isolated Git worktrees, measured tree evidence, reviewed-byte commit, changed-base reconciliation, bounded conflict remediation, serialized integration and own-work containment: `effects/`, `worktree/`, `daemon/`. |
| Provider runtime | Direct and **Herdr** multiplexer execution, persistent provider contexts, resident sessions, provider-neutral loops, health/failover, bounded startup retries, stall handling, process ownership and conservative crash recovery: `execution/`, `provider/`, `providerhealth/`, `processowner/`, `runtime/`. Herdr is the intended multiplexer, not tmux. |
| Resource safety | Durable host-wide weighted leases, dynamic pane/heavy/task limits, pane census, idle-session shedding, pressure admission and command refusal shims: `hostguard/`, `scheduler/`, `commandshim/`. |
| Operator/controller | Unix authenticated RPC, status/history/events, messages and audited task controls, backlog dependencies, Prophet chats, target configs, cron and loop schedules, roll/maintenance/watchdog/independent drive restart: `api/`, `cmd/fae/`, `controller/`, `daemon/`. |
| Configurable Normal mode | Runtime schema discovery, immutable adapter/target revisions, review lifecycles, ordered ticket outboxes, uncertain-effect query recovery, dynamic workspace/fleet provisioning and adapter retirement: `external/`, `daemon/`, `cmd/fae/`. The user chose configurable adapters; no particular vendor was selected. |
| Lifecycle cleanup | Reviewed census and retirement, archive-before-delete worktree cleanup, scratch creation identity, external canonical scratch backup, safe removal of exact duplicate scratch archives. Unique, incomplete and ambiguous recovery data remain conservative. |
| Backup lifecycle | Verified Dolt export/restore/fsck, historical coverage retention, remote archive receipt/content verification, query-only lost-ack recovery, durable retention/creation/replication schedules, now safe unpinning and quota controls. See `daemon/BACKUPS.md`. |

The most useful entry points are `README.md`, `IMPLEMENTATION.md`, `TESTING.md`, `daemon/BACKUPS.md`, `daemon/SCRATCH.md`, and `verification/README.md`. Historical phase paragraphs contain superseded limitations; read the latest phase and source before treating one as a current gap.

## Completed in this round

### Safe replication unpinning

`fae backup unpin --backup PATH --authorizing-message MESSAGE` previews a release; add `--apply --plan-digest DIGEST` to apply the exact reviewed plan.

- Enumerates every recorded replication destination, bounded to 10,000 outboxes and 64 KiB per payload; overflow/corruption fails closed rather than truncating evidence.
- Refuses pending/invalid outboxes and any pending schedule claim, even when the schedule is paused.
- Requires confirmed receipts and full remote content hashes for every destination, a verified matching local manifest, and fresh operator authority.
- Holds schedule/backup locks in the same order as selection; revalidates authority and local evidence before updating the pin index.
- Releases only the pin. It never deletes local or remote data, and retention still protects newest backups and historical cleanup coverage. Pin changes invalidate old retention previews.
- Fixes a review-discovered edge case: explicit replica reconfirmation may re-pin only a verified matching local source, never a missing or substituted source. Repeated unpin safely refuses an already-unpinned source.

Primary changes: `daemon/replication_unpin.go`, `daemon/replication_unpin_guard.go`, `daemon/replication_unpin_test.go`, `controlplane/replication_markers.go`, `daemon/backup_replication.go`.

### Durable backup storage quotas

`fae backup quota configure --max-bytes N --max-backups N --authorizing-message MESSAGE` replaces both limits; zero means unlimited. `fae backup quota status` reports usage, limits, exceeded state and enforcement semantics. Bounds are 0..2^60 bytes and 0..10,000 backup directories.

- Counts actual regular-file lengths in owned `backup` and `backup-replication` trees, including partial exports and orphan spools, and all top-level backup directories.
- Rejects foreign-owned, symlinked or special objects; persists configuration across restart.
- Refuses new backup allocation at a reached limit; checks again after export and verification. Preserves overrun data and refuses subsequent growth rather than pruning evidence.
- Bounds tar spool writes, including headers/padding, by remaining byte allowance and removes failed temporary spools.
- Does not override retention coverage or replication pins and never automatically deletes data.

**Limit:** Dolt's final export size is unknown before execution. This is admission plus checkpoint enforcement, **not a hard filesystem quota**. Apparent bytes exclude allocated-block/metadata overhead. External scratch archives are outside scope.

Primary changes: `daemon/backup_quota.go`, `daemon/backup_quota_guard.go`, `daemon/backup_quota_test.go`, `daemon/cleanup.go`, `daemon/backup_replication_archive.go`. CLI/API integration is in `cmd/fae/backup_storage.go`, `daemon/rpc.go`, `daemon/authority.go`, and `api/schema.json`.

## Final verification

**1,352 race-enabled test cases across 26 packages passed**, with **73.6% statement coverage**, `go vet`, and the temporary-directory census. **Gobra: 56 proofs and 56 rejected weakened controls. Gomela/SPIN: 57 positive models and 55 firing negative controls.** All final source/evidence fingerprints matched.

The full real-provider acceptance passed **36 genuine Codex turns across two code-bearing tasks**, using resident Herdr sessions, genuine RED/GREEN, mid-run restart and crash recovery, then all existing controller/adapter/retention/archive checks. This round added pending-unpin refusal, confirmed pin release without upload/deletion, stale retention-preview refusal, quota persistence, byte/count refusal before allocation and verified creation after raising the quota. Two scheduled uploads recovered deliberately lost acknowledgments without duplicate PUTs. Final owned host leases/panes were released.

Evidence:

- `verification/results/backup-storage.json`: authoritative round record, limits and evidence hashes.
- `verification/results/backup-storage-source-hashes.json`: 490 final source/binary fingerprints.
- `verification/results/backup-storage-formal/`: verifier log, 166 formal input fingerprints, model CSVs and archived logs/models.
- `verification/results/backup-storage-dogfood-20261021/`: result, artifact/restart evidence, 487 source fingerprints and 36 evidence fingerprints.
- `.cache/backup-storage-suite.log`, `.cache/test.json`, `.cache/coverage.out`: working native outputs; durable copies are referenced by the record.

Formal claims are limited to explicit production predicates and bounded models. They are not proofs of all filesystem races, Dolt, Git, HTTP, provider behavior or complete requirement coverage. Dogfood uses authenticated Codex with disposable localhost review/ticket/replica services; it does not establish third-party vendor conformance.

## Requirements audit

The specification contains 284 requirement headings. This round reviewed all major areas using source inspection, existing tests and three independent area audits. This is a gap-oriented audit, **not an exhaustive per-requirement acceptance certification**. The next step should create a maintained requirement-to-evidence ledger; unknown acceptance status must remain unknown.

| Priority / status | Requirements | Finding, source evidence, and acceptance still needed |
|---|---|---|
| **P0 — confirmed mismatch** | **FAE-HOST-001, -005, -010** | `hostguard/sample.go` selects Linux PSI **some**, while the requirement specifies **full**. `PressureClear` in `hostguard/guard.go` has unit callers but is not wired into live admission. `daemon/live_admission.go` uses one sample and obtains the heavy lease before the required separate two-sample/30-second I/O-clear gate. Fix production ordering and hysteresis; test sustained pressure, missing telemetry, cancellation, distinct refusal/wait outcomes, and recovery using an injected clock. Existing memory/weighted admission is real, but does not close these criteria. |
| **P1 — partial primitive library** | **FAE-WF-030** | `workflow/workflow.go` explicitly rejects HumanDecision, FanOut, FanIn and SubWorkflow as reserved. Independent Review turns do not implement arbitrary parallel graph paths. Add durable decisions, branch/join state and pinned child workflows, with restart, duplicate decision, cancellation and safety-floor tests. |
| **P1 — missing governed requests** | **FAE-WF-078** | `Limits.ScopeWidens` exists and is validated, but no durable scope-widen/RED-rework request, approval and counter flow was found. Propagate approved scope through briefs and command/VCS checks; test denial, exhaustion and restart. Oracle RedTest→red-loop / Production→implementation routing **does exist** for WF-077; verify replacement RED against the task base before declaring full acceptance. |
| **P1 — partial target policy** | **FAE-WF-035, -085** | `daemon/target.go` permits literal command vectors and rejects `{{`; typed artifact argument binding is absent. TargetConfig lacks path-scoped always-required QA policy. Add a versioned schema and exact typed binding without shell interpolation; required target QA must block commit even when a role omits it. |
| **P1 — partial RED policy** | **FAE-WF-073** | RED graph enforcement and live execution exist (`workflow/red.go`, `workflow/workflow.go`); a target diff classifier and authorized intake/spec-only no-red declaration do not. Add mechanical classification, fail closed for code-bearing diffs, and prove the same declared RED becomes green before commit. Audit adversarial phase labels and unrelated green tests as acceptance hardening, not as an established exploit. |
| **P1 — default content gap** | **FAE-WF-080; FAE-EXT-010..016** | Configurable review draft/read/revise/publish/merge lifecycle works, but `defaults/defaults.go` ends with integration-gate→Done and has no Normal review ExternalEffect stages. Add the default Normal extension while proving Local/dry-run behavior and the merge barrier. Do not describe the existing adapter lifecycle as missing. |
| **P1 — missing controller operations** | **FAE-CTL-030..033, -050, -051** | CLI dispatch/controller code lacks operator `land`, salvage, dependency batch, then-roll, `gates self-test`, and `gate run`. Workflow Integrate is not operator landing onto an exact post-merge target tip. Implement leased, gated landing with held-branch evidence and rollback, then batch/salvage/roll composition; zero tests, missing own hunks and failing QA must hold. |
| **P1 — acceptance ledger absent** | **FAE-VER-001, -002, -003, -004, -010; FAE-WF-094** | There are extensive firing controls, native fault tests and 10,000-case generated suites. No complete check→positive→firing→daemon-path census was found. `TestGeneratedDurableWorkflowParity10000` generates short AgentTurn chains plus a gate; it does not demonstrate all primitive/host-result/park/crash combinations. Expand breadth and wire the ledger into CI; do not infer full safety coverage from totals or statement coverage. |
| **P1 — recovery breadth unproven** | **FAE-ROB-012, -014, -016; FAE-VER-010** | Restart, real mid-turn SIGKILL, owned-process recovery and query-only uncertain effects are implemented. Full every-stage-boundary crash coverage, escaped/ambiguous descendants and all effect-producing result adoption remain incomplete. Conservative retained leases/parks are intentional. Add a state/effect-boundary matrix and explicit audited resolution; never clear an unproven live owner. |
| **P2 — authoring surface incomplete** | **FAE-WF-091..093; FAE-CLI-020** | Offline `workflow/simulate.go` refuses VcsCommit/Integrate/ExternalEffect even though scripted durable execution is richer. Structural workflow diff, explicit task-revision migration and the full get/latest/list/retire CLI surface are absent. Add isolated complete simulation and evidence-validating migration. Durable workflow retirement already exists in `controlplane/workflow.go`; expose its missing operator surface. Database schema migration is a different feature. |
| **P2 — gate-result packaging partial** | **FAE-WF-030, -063** | `engine/engine.go` records gate results, evidence IDs and trace leaves in durable history, but not as separately content-addressed gate-result artifacts. Add atomic artifact/transition persistence and replay tests. |
| **P2 — operator CLI gaps** | **FAE-CLI-032..034, -050..054** | Dispatch has message and core verbs, but no complete attach-design-doc, inbox/reply, explicit host-wide reclaim, legacy import, analyze-session, hidden heavy-op broker command or doctor surface. Existing internal reclaim and command execution are not CLI acceptance. Add each with positive/refusal/audit tests and scoped authorization. Doctor is SHOULD; do not prioritize it over safety MUSTs. |
| **P2 — UI not implemented** | **FAE-API-030..033; FAE-OBS-002; FAE-SEC-001** | Unix authenticated RPC exists, but daemon config/status has no optional UI child binary/assets/bind/write/health lifecycle. Implement supervised loopback token-gated UI child, bounded restart, status during shutdown and listener census. Do not infer UI acceptance from Unix socket authentication. |
| **P2 — observability incomplete** | **FAE-OBS-001, -003..006; FAE-HOST-022** | Current supervision log is append-only; some host-wait lines lack required structured fields. No complete Metrics RPC or alert acknowledgment surface was found. Watchdog filesystem bytes/inodes monitoring **is implemented**; command-group RSS/count/zombie inventories and full log/event pairing remain gaps. Add rotation, correlation, exact/estimated metric provenance, per-source alerts and deterministic timing tests. |
| **P2 — workspace discovery partial** | **FAE-ARC-023** | Safe explicit review/fleet provisioning exists, but marker-based multipackage build-workspace detection/provider selection was not found in `workspace/`. Add validated discovery without drive-time sync/create; test planted/ambiguous markers and pinned workspace identity. |
| **P2 — performance acceptance unverified** | **FAE-CP-009; FAE-ARC-002** | Scoped reads and cached status exist. No identified 100k-transition/row-access benchmark establishes CP-009's topology budget; drive wakes periodically and reconciles backlog, so strict cheap-idle acceptance also needs measurement. Add reproducible native benchmarks and SQL/CPU instrumentation. |
| **P2 — runtime/shim breadth partial** | **FAE-RT-001..014; FAE-HOST-011..014; FAE-SEC-002** | Codex/Herdr trusted-bridge dogfood, persistent sessions, failover and refusal shims are implemented. General authenticated-provider transcript/log discovery and interactive brokered tool conformance remain broader work. Command tools are disabled in the restricted Codex bridge; this is not a hostile-agent sandbox or support for every provider/toolchain. |
| **P2 — external acceptance boundary** | **FAE-MODE-001..004; FAE-EXT-001..022; FAE-SEC-004** | Configurable Normal grants, schema discovery, review/ticket lifecycle, dynamic adapters, fleet provisioning and retirement are implemented and exercised against local services. A real vendor's authentication, pagination/status vocabulary and recovery protocol are not certified. This is a deliberate configurable-adapter boundary, not authorization to choose or contact a vendor now. |
| **P2 — containment fallback incomplete** | **FAE-OPS-012** | `daemon/retirement.go` uses own-commit/landing receipts but lacks integration-subject discovery fallback. This is conservative refusal, not unsafe ancestor-based containment. Add unambiguous subject discovery with wrong-SHA/ambiguous-subject refusal. |
| **P3 — SHOULD follow-ups** | **FAE-WF-075, -086** | Improving-iteration budget discounts and accepted pre-code evidence carryover across new-identity re-admission have no identified end-to-end implementation. Core attempt limits, restart and Prophet controls already exist. Add provenance-preserving opt-in policy after the safety work. |
| **Documented design deviation** | **FAE-CP-010** | `controlplane/schema.go` documents Dolt implicit DDL commits and uses replayable idempotent migration. Do not describe this as fully transactional DDL; demonstrate crash recovery at every migration step or formally revise the acceptance contract. |

Additional limits of the backup extensions are **not automatically missing original requirements**: remote storage retention/credentials, calendar or multiple backup policies, hard filesystem quotas, automatic authority renewal and cleanup of unique/older/incomplete archives remain intentionally unsupported. Prioritize the actual specification gaps before expanding these conveniences.

Long-running operations also need bounded, verified archival of schedule history and process receipts: schedules currently fail closed at a size ceiling, while retained process receipts can grow. Any archival design must preserve crash-recovery evidence and exact identity checks.

Security acceptance still needs its own complete matrix for secret-redaction scans, role-profile goldens, listener/UI lifecycle and approved-test deployment leases (FAE-SEC-001..006). Existing authentication, pinning, structured binding and deployment tests are substantial evidence, but this audit does not certify the entire matrix.


## Prioritized next-work plan

1. **Repair host admission semantics and establish the acceptance ledger.** Delegate telemetry/hysteresis, daemon admission wiring, and requirement/check inventory separately. Correct PSI selection, run the two-sample gate before heavy acquisition, and test cancellation/missing telemetry/pressure recovery. Add a ledger entry for every shipped safety check with positive, firing and native-path evidence; fail CI for unmapped checks. This is the recommended immediate round.

2. **Complete target policy and RED governance.** Version TargetConfig for typed argument binding, required path-scoped QA and explicit no-red classifiers; implement durable scope-widen/RED-rework requests. Delegate schema/binding, governance state and adversarial test corpus with clear file ownership. Acceptance: no shell expansion, no implementing-role exemption, no unrelated-green bypass, no unapproved scope change, and old revisions remain pinned through restart.

3. **Finish workflow execution/authoring.** Implement durable HumanDecision first, then FanOut/FanIn and pinned SubWorkflow with cancellation. In a separate authoring track, add complete offline simulation, structural diff, the missing workflow read/retire CLI/API surface and audited task migration. Persist content-addressed gate-result artifacts atomically with transitions. Expand the fixed-seed generated corpus across supported primitives and interruption points; preserve terminal immutability, evidence consumption and kernel floors.

4. **Ship operator landing and gate commands.** Build `gate run` and executable gate self-tests, then single-task `land` on the exact post-merge tree with formatting, compile, positive test counts, required QA and own-hunk evidence. Add held-branch/rollback recovery before salvage, dependency batch and then-roll. Acceptance: an empty filter holds, a dropped production hunk holds, a failed second batch item leaves the first landed and the rest held, and roll failure returns nonzero with an alert.

5. **Close recovery and operator-surface gaps.** Add explicit reclaimed/retained owner reports, every-stage crash matrix, uncertain-result adoption where proof is available, and missing CLI operations. Preserve conservative parks where ownership/effect proof is absent. Import must be read-only/idempotent; analysis must label exact versus estimated metrics and never infer spend.

6. **Add Normal-mode default content and provider/vendor conformance.** Wire the working review/ticket mechanisms into default Normal workflow content while preserving Local/dry-run boundaries. Broaden authenticated-provider fixtures and schema contracts. Only after an operator selects a vendor, add its transport/authentication and real-account acceptance with draft opt-out and crash/adoption tests. No vendor choice is needed to finish configurable adapter/default-content tests.

7. **Implement observability and optional UI.** Structured rotating logs first, then Metrics RPC, paired events and auditable alerts, process/leak inventory, and provenance-based session analysis. Add UI child supervision only on the established authenticated loopback boundary. Acceptance: scripted exact timings, secret scan, listener census, shutdown-status liveness, child restart exhaustion and alert acknowledgment.

8. **Run a release-grade acceptance census.** Resolve remaining specification decisions, benchmark 100k-transition reads and idle behavior, calibrate host weights, and test supported OS/provider combinations. Rerun native race tests, all formal positive/negative controls, and two-task real-provider dogfood after each meaningful round. Attach source/evidence manifests to a deliberate versioned release; do not equate the present uncommitted working tree with a release.

For every round: assign disjoint agent ownership, review safety interactions, freeze source/test inputs, finish focused controls, then full native/formal/dogfood verification. Update this handoff and the requirement ledger using current evidence, not appended historical claims. Ask the operator only for genuine product choices or external credentials; routine implementation and local verification are already authorized.


## Reproduction and operating notes

Run from `/Users/lbethay/workplace/fae`. Keep build/test scratch on disk under `.cache/`; never run a test against a developer or historical plane. The current native fixture binary is `/tmp/dolt-darwin-arm64/bin/dolt` (the binary's location does not determine test scratch placement). Preserve unrelated existing processes and instances. The current session has full local access and approval mode never; the older approval-review timeout was not a blocker for this round. Do not attempt to change managed approval settings as part of routine verification.

```sh
FAE_TEST_DOLT_BINARY=/tmp/dolt-darwin-arm64/bin/dolt bash scripts/test.sh
bash scripts/verify.sh
GOCACHE="$PWD/.cache/go-build" TMPDIR="$PWD/.cache/tmp" go build -o bin/fae ./cmd/fae
GOCACHE="$PWD/.cache/go-build" TMPDIR="$PWD/.cache/tmp" go build -o bin/fae-codex-bridge ./cmd/fae-codex-bridge
```

The full native suite takes roughly 16–17 minutes on this host. The formal tools are already installed under `.tools`. `scripts/test.sh` sanitizes FAE environment variables, runs vet and race tests with nonzero-count checks, measures coverage, and verifies scratch cleanup.

Full acceptance command (choose a fresh evidence directory):

```sh
python3 scripts/dogfood.py --model gpt-6-astra \
  --dolt /tmp/dolt-darwin-arm64/bin/dolt \
  --codex /Applications/ChatGPT.app/Contents/Resources/codex-cli/CodexCLI.app/Contents/MacOS/codex \
  --herdr /Users/lbethay/.local/bin/herdr \
  --resident-sessions --provider-loop --crash-during-turn --cron-schedules \
  --controller-lifecycle --drive-restart --deployment-roll --prophet-sessions \
  --target-configs --retirement --normal-targets --census-cleanup \
  --dynamic-provisioning --scratch-cleanup --fleet-provisioning --adapter-retirement \
  --backup-policy --retention-automation --backup-schedules --backup-storage \
  --evidence-dir verification/results/NEXT-UNIQUE-RUN
```

Freeze production **and test** inputs before final verification; build both binaries, snapshot hashes, run native/formal/real-provider checks, and verify manifests again before publishing the record. Do not rebuild `bin/fae` while dogfood is running. A passed old record does not certify later changes. Optional provider/account availability failures must be reported honestly rather than counted as passes.

## Decisions and boundaries to preserve

- Keep Herdr support; do not substitute tmux.
- Keep configurable adapters until the operator selects a real vendor and credentials; schema discovery alone is not proof of vendor behavior.
- No implicit replay of uncertain external mutations, force-unpin, unsafe lease reclamation, or quota-driven deletion of recovery evidence.
- Preserve exact authorization, pinned revisions, source identity and historical cleanup coverage.
- Full local dependency installation is authorized. No commit/push, production deployment, arbitrary remote deletion or project rename was requested in this round.
- Backup schedules are opt-in singleton interval policies with normal grant expiry; they do not renew authority automatically.
- Requirements section 19 remains an explicit decision list: host-weight/floor calibration, cross-revision role-cap semantics, multi-workspace direction, roll topology, global Prophet authority and vendor schema choices should be resolved explicitly when their work starts.
