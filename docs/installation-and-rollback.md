# Installation and rollback plan

The PR and local/CI build are review artifacts. Installation, merge and release
are separate actions; no running installation is changed by the build or tests.

## Reviewable candidate

Build the exact reviewed commit for the CPA host's OS/architecture, run the C ABI
smoke test, and record its SHA256. The candidate library is `cursor-next.dylib`,
`cursor-next.so` or `cursor-next.dll`; provider and model prefix remain `cursor`.
Read `NOTICE.md`, `VALIDATION.md` and the host-compatibility limits before a
switch. In particular, CPA's own zero/complete rows remain unreliable for omitted
usage and Dashboard observations lack an immutable final marker.

## Isolated trial

Use a different listener, configuration, auth directory, plugin directory and
private journal directory. `integration/start_isolated.py` requires explicit
paths, copies only a selected existing Cursor credential and the candidate,
generates private local test keys, disables request logging and binds loopback.
It never rewrites the supplied credential or existing CPA configuration.

Run both the controlled real-provider protocol and independent later comparison.
Keep their artifacts private. Stop the test instance using its own PID after
validation; do not search-and-kill by a shared process name.

## Optional future switch

1. Record the exact current CPA version, plugin checksum, enabled plugin ID and
   configuration. Keep the existing plugin and a private configuration backup.
   Drain active requests before stopping/restarting the host; hot unload during
   an active stream has a host-side cancellation limitation.
2. Copy the candidate to the new plugin ID/path. Configure a private persistent
   journal. Disable `spx-cursor` before enabling `cursor-next`; do not let two
   executors own provider `cursor` in one host.
3. Preserve credentials, aliases, routing and context/compaction settings. Restart
   only the explicitly selected host, then read back loaded plugin ID/version
   and checksum. A file copy alone is not proof it was loaded.
4. Run a short tool/result continuation, check measured/unknown journal states,
   and inspect the client's current-step usage. Accept only the documented
   behavior; do not treat the host's synthetic zero row as a measurement.

## Rollback

Drain the candidate's requests, stop that host, disable `cursor-next`, restore the
previous enabled plugin ID/configuration and start the previous library. Retain
the candidate journal separately for accounting/audit; do not feed deferred
usage into the old plugin or another conversation. Read back the loaded version
and make one bounded tool/result check. No credential rotation, model alias
change or global context tuning is required for this plugin-only rollback.

## Journal operations

Use one private journal per host process. The OS lock releases automatically on
exit; the lock file itself is intentionally retained. After a crash, already
closed pending Runs can be reconciled by later requests. A Run whose close time
was lost stays `unknown/interrupted` and is not automatically matched against a
broad timestamp range.

Pending observations expire from automatic lookup after seven days while their
unknown audit records remain. The snapshot is bounded at 10000 records/16 MiB;
at capacity the journal rejects new Runs rather than silently discarding billing
evidence. Archive/rotate it offline only after stopping its host, retain the old
file as private evidence, and start with a new journal path. In-memory mode has
the same bound but no restart recovery. This limit is explicit operational scope,
not an automatic claim that old unknown usage was zero.
