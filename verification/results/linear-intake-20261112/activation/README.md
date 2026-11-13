# Host activation — 2026-11-12

The existing default instance was backed up and restarted with the tested binary,
the direct adapter, the installed default workflow and a registered schema-4
Local target. The stored Linear key was used without copying it into source or
logs. OR-811 was admitted once and its Linear state was confirmed as In Progress.
A real Claude intake turn succeeded; the specification turn was active at this
checkpoint. This is intake/provider/synchronization evidence, not completed-task
or production-readiness evidence.

Live startup exposed two session-resumption defects: allocated registry IDs were
used as provider conversations, and ephemeral provider sessions were treated as
resumable. Both were corrected with regression tests. Mistaken provider-health
penalties were cleared through the audited operator quota-clear command after
the fixes. The existing task was retried, preserving its original issue binding.

The working tree and current branch were preserved. A separate index was used
to create an immutable source snapshot on the dedicated local integration ref.
No remote push or deployment was performed. The previous executable is preserved
as bin/fae-before-linear-20261112. Secrets and private live configuration remain
outside the repository. Recorded serve arguments retain the new startup options.
