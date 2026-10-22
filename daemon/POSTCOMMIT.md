# Restart after a broker commit

A task has two independent Git identities. `base` is the original reconciliation
origin. `execution_head` is the branch HEAD established by the last completed
broker commit or reconciliation. Every dispatch freezes both. Agent turns check
HEAD against the execution identity; merge and integration retain the original
base so a committed task patch remains part of the task's complete intent.

Restart-with-information and resume verify the bound worktree, exact recorded
HEAD and durable tree before saving the audited control checkpoint. Foreign
commits (even with an identical tree), arbitrary resets and changed working bytes
are refused. An unresolved commit dispatch cannot be restarted. Its existing
receipt-based uncertain-effect reconciliation must finish first.

Older snapshots without `execution_head` can upgrade at the explicit control:
HEAD must equal the latest validated broker commit receipt, the old base must
remain its ancestor, and the durable tree must match. The upgrade and control
share one checkpoint and its lost-acknowledgement recovery protocol. The base is
never silently replaced with HEAD.

Restart still enters the workflow's first stage. The workflow's ordinary test,
review and commit gates apply again; existing green code does not waive a new
RED requirement. Resume follows its existing explicit remediation route. Review
revision handling retains its existing landed-commit base semantics.

The native regression exercises an actual broker commit, foreign-HEAD/tree
refusals, legacy checkpoint upgrade across daemon restart, a new RED/green cycle,
concurrent upstream changes, complete verification and integration of both sets
of changes. The standalone engine tests cover provenance and uncertain commits.
