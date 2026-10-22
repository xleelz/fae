# Real effects

`Broker` runs commands only in a registered Fae task worktree. Its caller must
hold durable task and weighted host leases across validation, execution, and
collection. The allowlist contains exact argument vectors; no shell or template
expansion occurs. Configured command vectors are operator trust inputs. Dynamic
typed-placeholder templates are not implemented yet.

Commands receive a minimal environment, a timeout, process-group cleanup on both
normal exit and cancellation,
and bounded output. Output overflow, timeout, malformed Go test JSON, incomplete
test execution, and tree mutation refuse usable evidence. Go test evidence
requires `-json -count=1`, counts executed non-skipped tests from actual events,
and binds expected failure text to the named test's failing output. A fresh-build
attestation additionally requires `-a -x` plus an observed compiler invocation
naming source within the task worktree, from Go build-output events or compiler
stderr. Linking or compiling generated testmain alone does not qualify. Zero tests remain
zero for the engine's positive-count gates. The broker does not infer source
identity from test output; the caller must verify the frozen test bytes.

Tree measurements use temporary Git indexes and include all tracked and
nonignored untracked files. Snapshot accepts exact file paths, refuses directory
and path traversal declarations, and refuses changes outside the reviewed set.
Commit writes the measured reviewed tree with `commit-tree` and compare-and-swap
`update-ref` and bypasses hooks. While holding the real Git index lock, it accepts only the old-base or reviewed index tree, prepares the reviewed index, advances the branch, then atomically installs and syncs the index without changing working files. Foreign staging refuses before the branch advances. Callers must
serialize worktree writers and remeasure evidence before using it. This is not
a sandbox against a malicious process or filesystem race.

Integration only advances an un-checked-out `refs/heads/fae/` integration branch,
using Git's atomic compare-and-swap ref lock. Ancestor, patch-equivalent, and
all-touched-path blob/mode containment return the containing tip without moving
it. Touched paths come from the actual commit rather than a caller-supplied list;
empty changes are contained. Changed bases refuse direct integration with a
`TargetMovedError` only when the broker knows no write occurred: either it
observed a different tip before attempting the write, or Git explicitly rejected
the expected-old-value compare-and-swap. After a CAS refusal it inspects current
containment first. Unknown outcomes, cancellation, permissions and lock failures
are not classified as safe retries.

The live daemon admits the typed refusal through `CanRetryIntegration`, then a
bounded integration retry loop. Reconciliation uses the durable task base rather
than the already-committed task HEAD, preserving the complete committed own
delta. Old approvals are invalidated; the merged result repeats tests, reviews,
QA and commit before a new integration attempt. A two-writer temporary Git test
covers the committed loser, union tree, fresh Go compilation/tests and landing.

`PrepareReconcile` makes a three-way merge using the immutable reviewed base,
reviewed tree (including uncommitted changes), and the current integration tip.
`PrepareReconcileFrom` additionally takes the durable original task fork point,
so committed and uncommitted own work are both included. It writes Git objects
but leaves refs and checkouts alone. Conflicts name paths. Exact zero-context own
patch comparison ignores hunk line offsets and index blob IDs while retaining
added/deleted bytes, modes and binary patches. A missing or changed own hunk
refuses with paths. Fully blob-contained changes are separately identified;
partially upstream own work conservatively refuses when its own diff shrinks.

Persist `ReconcileEvidence` before calling `ApplyReconcile`. Application
recomputes the plan, checks the tip and pre/post task state, compare-and-swaps
the task ref, and installs the merged tree only in its task worktree. The real
Git index lock spans checkout through atomic index installation: working files
contain the merged tree while the index returns to the new base, so subsequent
implementation and conflict-resolution edits remain unstaged. Only index trees
from the persisted plan are accepted; foreign staging and existing locks refuse.
Retrying an interrupted ref-before-checkout application is supported; unknown
edits or moved targets refuse. A process crash that leaves a Git lock requires
explicit operator repair, as with commit index recovery. The primary checkout and integration ref are never
rewritten. The new tree is deliberately uncommitted: the caller must recapture
reviewed evidence and rerun required QA before commit/integration. Conflicts
leave the worktree untouched until a separate immutable conflict plan is persisted.
`ApplyConflict` re-derives the plan and exact path set, checks the same production
application guard, then installs text conflict markers only in the task worktree.
The live daemon refuses frozen-test, undeclared and nonregular/binary conflicts.
Supported conflicts return named context to a bounded implementation loop,
followed by tests, frozen-file checks, reviews, judgement and QA.

`VerifyConflictResolution` checks the immutable pre-conflict snapshot rather than
a replacement agent report: every original added hunk block must remain in the
resolved own delta, original deletion blocks must be absent, and task-owned file
mode changes must remain. Missing files and lost hunks identify their original
paths and hunk ranges. Unresolved marker lines refuse. Text offsets may move;
ambiguous repeated-text deletion/move cases and partially upstream shrinking
deltas conservatively refuse. This is a byte-level preservation guard, not a
proof of semantic equivalence; refreshed tests and review still apply.
The daemon retains an independent per-task original-intent record and rechecks
it at implementation, merge and commit boundaries, so omitting a graph input
cannot bypass preservation. A second upstream advance after a conflict requires
operator review; the current implementation refuses rather than overwrite the
original contract. Successful cleanup and landing do not erase its audit trail. These Git checks are
not a proof of merge semantics or protection against a malicious concurrent
filesystem writer; callers must retain their durable writer lease throughout.

Production callers configure `ProcessRegistry` and `ProcessOwner` so host
commands use durable exact process ownership and a parent-death watchdog.
Unresolved cleanup refuses evidence. The embedding daemon must recover the
same registry before reclaiming its resource leases.

Tests use newly created temporary repositories. They execute Go tests, host
commands, exact commits, integration and containment, while asserting the
primary checkout remains unchanged, measurements preserve the actual index, and successful task commits leave their owned index aligned with HEAD.

Journaled commit adoption completes interrupted index installation only after proving the exact prepared commit, parent and working tree, and accepting the same two trusted index states. Unknown or stale Git index locks are never removed automatically. A SIGKILL inside the index-locked region can therefore require explicit operator investigation and lock repair before retry; refusal preserves the lock, index, branch and working bytes. A completed ref CAS with no remaining lock is recoverable through the recorded commit intent.
