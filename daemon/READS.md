# Reads and event resumption

Plane read RPCs run in a keyed pool (8 running queries, at most 64 outstanding
keys). Identical canonical method/parameter reads for the same authenticated
actor and task scope share a query. Authorization occurs before joining. Each
waiter can cancel independently; the last waiter cancels its query. An abandoned
query retains its bounded slot until it actually returns. Read deadlines default
to 5 seconds. The optional request `deadline` is an RFC3339 timestamp and can only
shorten the transport's 5-second limit. Errors use `deadline-exceeded`,
`cancelled`, or `read-busy`. Cached Board and Status remain responsive when the
pool is saturated. Board reports `snapshot_age_ms` and rejects snapshots older
than 10 seconds with `snapshot-stale`; Status reports `snapshot.age_ms`.
The writer refreshes snapshots on state changes and its five-second tick.

`serve --event-retention-age 24h --event-retention-count 100000` retains the union
of both windows: all recent events and at least the newest configured count.
A durable, monotonic logical floor survives restart and clock rollback. Startup
and an independent one-second maintenance worker advance it through audited
transactions; reads only select the floor. Advancement does not emit recursive
events. Maintenance failures retain the previous, longer window until retry. Increasing
retention later does not resurrect an already expired streaming cursor.
Archival event rows are never deleted: physical storage continues to grow until
a separate backed-up compaction implementation is available. Task history and
internal forensic reads still access the complete append-only archive.

`Events {after,limit}` returns its existing event array for retained cursors. An
expired cursor returns error code `event-gap`; its JSON error detail contains
`kind`, `reason`, `requested_after`, `resume_after`, and `timestamp`.
`GET /v1/events?after=N` returns NDJSON events. An expired cursor receives a gap
first, then EOF; reconnect with `after=resume_after`. This explicitly acknowledges
the unavailable prefix rather than silently skipping it.

Each subscriber has an independent 100-event queue. Historical catch-up applies
backpressure only to that subscriber's producer. Once caught up, overflow emits
a gap with the last delivered cursor and closes the stream; resume from that
cursor in the plane. Write deadlines bound unresponsive connections. A client
whose socket cannot accept even the gap must reconnect using its own last
successfully received cursor. Neither producer nor socket writes hold a drive
lock. Subscribers also share the bounded/coalesced plane read pool.

This work does not add the remaining topology/host-metric/backlog read RPCs or
physical event compaction. The pool applies to all currently registered read
methods, including schedule list/inspect and controller health; these reads consume immutable cached copies without acquiring the writer mutex.
Controller health reports its snapshot age and startup binary identity; mutating
controller preflight still re-hashes the executable before making decisions.
