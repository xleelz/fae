# Requirements follow-up: host capacity and runtime isolation

This pass implements the additional host workflow requirements SCH-016–023 and
CLI-044, plus the amended workflow, runtime, event and filesystem requirements.
It preserves the requirements document and its proposal markings. No live
instance is restarted or migrated by this implementation work.

## Host capacity

Workflow revisions declare molecule or tree slot scope. Derived/effective
session footprints are pinned atomically with new revisions. One cross-process
ledger acquisition path applies requested limits, measured memory, unused
holder commitments, panes, pressure holds, FIFO ordering and molecule limits.
Children share their ancestor holder; role caps remain separate. Startup
reestablishment, proven-dead-owner reclamation, bounded lock refusal, usage
refresh, waiting events, alerts and audited request changes are covered by
focused native and concurrency tests. See [workflow-capacity.md](workflow-capacity.md).

## Workflow and event amendments

- `serve --recursion-ceiling N` configures depth (default 8, supported range
  1–64). Every child of a FanOut visit shares the visit's child budget.
- Detached admissions require hierarchical IDs, inherit the root session
  limit and start from the parent's last recorded tree. Child admissions
  cannot set `max_sessions`; binding errors aggregate with signature errors.
- Change-request identities include the visit. Width breaches escalate or
  route instead of quarantining; failed routes to joins remain failed at
  every nesting level. SubWorkflow outputs retain branch provenance.
- Element filters can read other task evidence. Projections retain their
  evidence references; branch views refresh current tree data. Failure
  handler bindings replay from their audited transitions.
- Stage turn budgets only tighten role bounds. Stage exhaustion policies
  override role policies; resume-once precedes a failure route, and an
  unspecified policy fails once instead of retrying under same-gate.
- Task event ancestry/run/workflow metadata is stored with the event, remains
  immutable after later task changes, and preserves spilled body references.
- Task and branch write scopes only narrow an existing grant. A read-only
  stage gains no write authority from those layers or from artifact-only mode;
  file-producing stages must declare their writable scope explicitly.

## Runtime isolation and verification limits

Agentic turns use private per-session filesystem views instead of moving or
chmod-changing shared files. Main assistant messages are counted by unique ID;
subagents are excluded and an exceeded limit kills the process tree. Scoped
writes, read roots, ancestor outputs, actual CLI tool events and guarded
read-only Git queries use the same policies across runtime layers. See
[SESSION_FILESYSTEM.md](../provider/SESSION_FILESYSTEM.md).

This environment lacks the required namespace capability. Positive kernel
namespace and installed-Claude/model checks are capability-gated and have not
been verified here; unsupported isolation refuses before provider launch.
Automatic approval review also rejected the opt-in real-provider test because
it can send synthetic test data externally and incur charges; that test was
not run. Its reviewable payload is in
`provider/installed_claude_filesystem_test.go`.
Build/test/format shell profiles inside a private PID namespace also refuse
until a host PID ownership proxy is available; accounted HostCommands keep
their outer host lease path.

The updated read-policy example contradicts the existing deny-wins rule.
Pending clarification, deny-wins remains in effect; item placeholders bind
without creating an exception to a broader deny.

The shared host ledger uses version 2 once workflow slots are enabled.
Older binaries cannot rewrite that ledger; live older participants fence new
acquisitions until upgraded or proved dead. Rollout therefore needs to account
for every Fae instance sharing that host ledger.

## Verification handoff

The workflow, engine, scheduler, control-plane and CLI suites pass. The complete
command shim, provider, runtime, execution and effects suites also pass. Native
slot tests cover recursive sharing, uncharged refusal, migration repricing,
restart restoration of actually granted commitments, and mixed-adapter pricing.
Cross-process/race checks, the bounded capacity model, repository-wide vet,
RPC schema compatibility and the evidence-ledger census pass. The census is an
evidence inventory, not acceptance of every proposed requirement.

A concurrent native run interfered with the orphan-plane cleanup fixture; the
fixture passed three isolated repetitions, followed by a serialized daemon suite
run. Validation logs are under `/tmp/fae-round2-*` for this session. The staged
binary is `bin/fae-requirements-followup-20261106`; the live binary and services
are unchanged.

After operator approval, the opt-in installed-Claude test was invoked. It skipped
before provider launch because bubblewrap is unavailable. `hc machine ls`
returned no registered remote machines, so no capable remote host was available
through this connection. No provider request was made.

A late scheduler review also identified an outstanding capacity case: an
undeclared detached workflow under an unbounded root can exceed the root's
predicted footprint. Static SubWorkflow costs are derived; an explicit root
session bound limits actual family concurrency. Dynamic detached-cost admission
still needs a pricing or refusal rule before this case is considered covered.
