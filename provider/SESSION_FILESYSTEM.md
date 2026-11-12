# Session filesystem and provider state

SEC012 selects namespaces only when mechanical confinement needs them: a shell
with narrowed reads or deny paths, or a shell/command accessing an external read
root writable by the daemon user. Output, staging and ancestor output directories
alone do not require a view. Shell-less sessions use native CLI permission rules
and disk-backed restoration collectors. Read roots already read-only to the
user do not require a view. A target may explicitly set
`shell_runtime_mounts_without_view` when every `shell_allow` executable is absolute
and inside a pinned runtime mount; each turn records that choice.

Namespace-required launches need Linux, `bubblewrap` and working unprivileged
user/PID/mount namespaces. The daemon and doctor probe this capability, including
the mount plan's host-file canary. Admission refuses a stage requiring a missing
capability. There is no automatic weaker fallback.

A filtered view contains only readable files at their canonical paths. Safe
symlinks resolving inside the same root are preserved and absolute in-root links
are rebased inside the copy. Unfiltered read roots and read-only worktrees bind
read-only without copying. Writable worktree/output/staging views keep private
copies and merge permitted changes using streamed hashes and atomic file copies;
no full tree's file contents are retained in memory for change detection.
Changed source content or modes and hidden pre-existing output conflicts are
refused. Owned copies are removed when the launch ends; construction time and
copied bytes are recorded. A sibling marker binds each copy to its creator host
PID and birth identity. Startup and post-retirement sweeps reclaim copies left
by killed bridges or a dead daemon only after that identity is proved dead; live,
unknown, foreign and provider-state paths are preserved. Sweeps advance a bounded
cursor through retained session logs. Read-root and ancestor-output mounts are read-only.

Runtime HOME is a private durable task/role/branch state directory. TMPDIR is a
separate writable launch directory. Both remain writable after the read-only
session log mount. Explicit environment credentials pass unchanged. An explicit
`CLAUDE_CONFIG_DIR` or `CODEX_HOME` is validated before launch and mounted
read-write at that exact configured path, preserving credentials and resume
transcripts. Ambient operator HOME configuration is not exposed.

Target `runtime_mounts` directories bind read-only at their original paths. Target
revisions pin `runtime_mount_identities` as hashes of deterministic listings;
launch refuses identity changes and records the identities in provenance. Mounts
cannot overlap task, instance, read-root, output, staging or configured credential
directories. `cache_dirs` allocate stable private task caches; configured cache
environment values are redirected to those paths, whose mapping is available in
`FAE_CACHE_DIRS`. Cache aliases cannot overlap runtime or task-owned mounts. The
pinned target PATH must contain its toolchain; ambient operator PATH is not
inherited. Provider installation dependencies may be declared as runtime mounts.

For namespace-free output/staging access, collectors keep restoration data on
disk. The daemon publishes active and completed disjoint write grants for the
current collection epoch. Collectors preserve changes proved to belong to a
foreign grant, including a peer that finished first, and fail closed if their own
admission identity is missing. Descendant output ownership is durable and refreshed
at collection, including descendants admitted after launch. Admission publication
and collector comparison/application share a file lock. Parent roles never gain
write authority over descendant outputs.

Interactive Go builds/tests and gofmt use the existing scoped adapters. A private
namespace session receives a Unix socket rather than the host heavy-operation
ledger. The host server authenticates peer credentials and bridge ancestry,
resolves namespace init to a host birth identity, and admits each command under
its own weighted lease. Sibling commands queue normally. Nested heavy commands
are refused under HOST014 before queueing even if they remove environment
markers. Queued disconnects cancel their waiter. Normal commands wait their child;
abnormal protocol or cancellation kills namespace init and waits for it to stop
before releasing the lease. Non-namespace sessions retain ordinary host-owned
shim admission. Other build tools require scoped adapters. Already-accounted
HostCommands keep their outer lease.

Claude native permission rules use `//` absolute paths. Hook-dependent launches
never pass `--safe-mode`, which disables hooks. The opt-in
`FAE_TEST_INSTALLED_CLAUDE=1` test builds and uses the real broker, bridge and
`runtime.Direct` production launch, including its settings and credentials. It
requires correlated successful reads and output/staging writes, denied reads and
writes, and on-disk proof that the hook refused an absolute shim bypass. It uses a
real provider account and may incur charges. No external provider calls were made
for this implementation; this host lacks bwrap, so successful native namespace
and installed-model execution remains unverified. Local subprocess, collector,
capability and race tests cover the implemented mechanisms without calling a model.
