# Explicit review workspace preparation

`fae workspace review-plan --task TASK [--run-id RUN]` returns the deterministic task worktree path and branch, the configured base commit and current integration tip. Planning is read-only. Install an adapter bundle whose task review policy uses that exact workspace, branch and integration tip, declares the package, and grants its approved fleet-baseline read mechanism. Then run `fae workspace review-prepare --task TASK --run-id RUN --adapter ID --adapter-revision N --authorizing-message MESSAGE` before installing the versioned review target and admitting the task.

Preparation requires an instance operator and a governance authorization. It acquires weighted host admission, persists an immutable pending record, creates or verifies the ordinary deterministic task worktree, and invokes the package baseline through the pinned adapter descriptors. Only a green, exact package/integration-tip response publishes ready. No arbitrary shell or user-supplied executable runs. The task admission reuses this same registered worktree, avoiding two worktrees on one branch. An optional `review_policies[TASK].provision_tool` selects an installed, schema-checked adapter for package-specific fleet provisioning. Its literal `package`, `workspace`, `branch` and `integration_tip` arguments must match the planned binding; other arguments remain immutable installed pins. Without this field, preparation creates the Git workspace and leaves package-specific fleet validation to the baseline adapter.

A failed or cancelled attempt retains pending evidence and any already-created owned worktree. Explicitly retrying the same adapter revision/hash, task, run and policy verifies/adopts the owned worktree and repeats the read-only baseline. No automatic preparation or synchronization occurs during drive, dispatch, or admission. An incompatible retry refuses. A ready binding with a missing, foreign, symlinked or wrong-branch worktree refuses; retry never recreates a deleted ready binding. The immutable record survives daemon restart. Target validation checks its adapter identity, policy, task/run and physical registration before any hot-path worktree creation can occur.

Preparation has a bounded two-minute RPC budget; shorter caller deadlines still apply. The pending/ready records and authorization audit use existing durable marker/blob transactions. A lost database acknowledgment can be reconciled by repeating the explicit command: the stored immutable record determines whether preparation remains pending or ready. External baseline calls are read-only and are never interpreted as review mutations. The health command resolves a prepared pre-admission adapter binding, or an admitted task's pinned target, without editing shared live configuration.

The native regression covers missing-binding refusal, a red baseline, cancellation without ready publication, restart and explicit retry, idempotent ready verification, target/admission worktree reuse, physical binding loss, and real adapter review creation/revision with the prepared working-directory pins. Gobra proves the approved-package/exact-binding guard with a weakened control; Gomela checks baseline acknowledgment before ready publication. These proofs do not cover Git internals or the vendor build fleet.

## Optional fleet provisioning

The pending preparation stores the immutable external intent before provisioning.
A durable `provision.attempted` flag is committed before its one permitted POST.
The adapter must return a matching receipt whose `data` object contains exactly
`package`, `workspace`, `branch` and `integration_tip`, equal to the bound plan.
The receipt is persisted before the package baseline can publish ready.

After an uncertain response or restart, explicit retry queries the original
intent; it never repeats the mutation. A missing receipt, malformed response or
binding drift blocks preparation. A crash after the attempted flag but before
the request can therefore remain blocked until the vendor supplies authoritative
recovery evidence. A red baseline retains the verified provision receipt, so a
later baseline retry performs no additional fleet mutation.

Adapter retirement prevents creation of a new pending binding atomically. An
already durable pending binding may finish its original provisioning attempt or
recover its receipt and become ready after retirement. Retirement still prevents
using that ready preparation to create a new target or admit a new task. Existing
admitted tasks continue through their immutable adapter pins. Startup, target
validation, admission and drive execution never invoke fleet provisioning.

Fleet provisioning capabilities cannot be used by ordinary workflow external
effect stages, including a second alias for the same endpoint/server/tool.
Execution and uncertain-effect reconciliation repeat that restriction. Health
reads refuse an uncertain fleet intent, and ready binding validation checks its
verified provisioning receipt as well as the physical Git binding.
