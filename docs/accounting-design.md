# Measured tool-step usage

Cursor's native Run normally publishes `TurnEnded` only after the caller's tool
results have returned inside that Run. This plugin's stateless Chat Completions
bridge returns the tool batch to its caller and closes the Run first. Therefore a
tool batch can have no native usage message. Dividing request/output bytes by
four does not measure that Run, its cache hits, or its reasoning output.

The bridge closes the native Run and stops its sender before querying
`DashboardService/GetFilteredUsageEvents`. It waits for a bounded interval
(8 seconds by default) for the measurement belonging to that Run. Dashboard
requests use the existing CPA host HTTP callback, proxy, and request-scoped
cancellation. Each HTTP request has a 2 second deadline. The first lookup waits
at least one second after this resolver begins, after the Run has closed;
further lookups wait 500 ms. Transient HTTP failures and HTTP 429
use bounded exponential backoff, with the complete operation bounded by the
original polling deadline. HTTP 401/403 and other nontransient 4xx responses
stop polling. Error messages omit upstream response bodies and credentials.

## Mutable dashboard snapshots

Controlled live validation showed that one `usageEventId` can retain its identity
while its input, cached-input, and output counters increase after the Run closes.
A first populated event is therefore insufficient evidence of a final count.
For the current Run, the polling resolver requires at least three complete,
matching snapshots spanning at least one second. A counter revision resets the sampling window; switching event identities is
ambiguous and rejected;
an absent event or failed lookup also resets it. Pagination must complete for
each sample. The initial delay, sampling, retries, and pagination all share the
same 8 second budget. A value that has not stabilized before that deadline stays
unknown. The accepted result includes its sampling count internally as
`cursor_usage_stability_samples`. Tests can explicitly set `Stable=0` to disable
the settling delay without changing production defaults.

This is bounded settling evidence, not an upstream finality guarantee. Cursor's
dashboard response has no verified final/immutable marker. Its counters could
change again after the sampling window or after a response was delivered. A
later change to an already recorded event must be treated as a correction or a
conflict; it cannot justify silently claiming the earlier value was absolutely
final. Stable totals may also aggregate multiple internal model steps of a Run.
The decoder records native tool-discovery/refusal calls and completed internal
tool batches separately. Both Dashboard and native `TurnEnded` totals are retained
as Run observations when such work occurs; standard context usage is omitted (`context_unknown` at the plugin boundary).
The aggregate is never presented as one model step's context size. No observed
internal batch is only a guard: the protocol exposes no verified per-inference
context counter, so zero observed batches does not prove a single inference.

## Counter contract

| Source | Input | Cached input | Cache writes | Output / reasoning |
| --- | --- | --- | --- | --- |
| Native `TurnEnded` | Native input already includes cached reads and writes | Native cache-read counter | Native cache-write counter | Native output includes reasoning; reasoning is a detail, not additional output |
| Dashboard `tokenUsage` | `inputTokens + cacheReadTokens + cacheWriteTokens` | `cacheReadTokens` | `cacheWriteTokens` | `outputTokens`; include `reasoning_tokens` only if `reasoningTokens` is present |

The dashboard uses proto3 JSON: a present `tokenUsage` message can omit
zero-valued scalar counters, including cache reads and writes. Those omitted
counters are zero. An absent message, absent event, invalid value, unreadable
dashboard, ambiguous match, or timeout is unknown. Unknown usage is never
substituted with a zero measurement or a byte estimate. An explicit all-zero
message remains a measured zero. Negative, fractional, null, nonfinite, and
inexact JSON counters are rejected. Total input and total tokens must remain
within JavaScript's exact integer range.

## Matching and isolation

Every lookup is bounded by the native Run's start and close timestamps, with a
one-second clock-skew allowance. It requires the exact conversation ID and the
resolved model. For a base model, only known reasoning/Fast suffixes are
accepted; an arbitrary name prefix is insufficient. The complete matching
context also includes the account credential fingerprint in the runtime and
journal. Neither a conversation name alone nor a timestamp alone identifies a
request.

`usageEventId` is the preferred event identity. The resolver exposes an opaque
fingerprint plus the event timestamp internally for durable claims and overlap
checks. If the upstream ID is absent, it fingerprints the available event
identity and measurement. Duplicate copies of one event are deduplicated;
conflicting copies and distinct same-millisecond events are rejected as
ambiguous. Measurements from several events are never summed to create a
current context size.

Pagination is inspected before accepting a candidate: a matching event on a
later page could make the result ambiguous. Queries request 50 records per page,
up to 20 pages. Truncated or inconsistent pagination returns unknown rather than
accepting a partial view. An empty proto3 response `{}` is an empty result and
can be polled again. An event without a populated `tokenUsage` message can also
be polled again.

The runtime serializes Runs for the same account/conversation, preserves its
stable conversation ID for upstream prompt-cache affinity, and uses its journal
to reject an event already claimed by that account. Other Runs with a proven
distinct event no longer make an overlapping timestamp ambiguous. An overlapping
unresolved Run remains ambiguous. Known other-Run event IDs are excluded before
counting candidates; timestamps never authorize reuse of an event. Distinct accounts and conversations may run concurrently.
The resolver itself retains no token, account-global cache, or background job.
The separate one-shot page-collection helper performs matching and pagination
without a settling wait; a single helper result does not establish stability.

## Late usage and host boundary

Measurements found after a response completes belong to the original journal
record. Reconciliation may update that record, independently of a later
response's standard usage. It must never add earlier input to the next
`prompt_tokens`, because clients use that value as the next request's context
occupancy. The journal handles durable claims, cancellation, restart auditing,
and separate deferred measurements; the resolver only returns a single Run's
measurement.

Some CPA versions discard extension fields or synthesize their own accounting
record when standard usage is absent. See [host compatibility](host-compatibility.md)
for the exercised host behavior and its limits. A plugin marker alone cannot
change that host contract. This implementation must not describe omitted usage
as proof that every downstream database or client represents unknown correctly.

## Deterministic coverage

`usage_test.go` exercises cached/uncached/write-cache accounting, reasoning
presence, measured zero and unknown, exact JSON integers, stable event identity,
model/conversation/time separation, delayed and mutable publication, stable
snapshot duration and sample count, deadline failure, pagination, duplicate
and ambiguous events, permissions, rate limits, retries, cancellation, HTTP
timeouts, and account isolation through the actual host callback interface.
It uses synthetic values and no network or real credentials. The multi-tool
dispatch and real-host suites exercise integration beyond this resolver.

## Revision journal and availability

All observations remain provisional. `FirstUsage` preserves the initial value;
`Usage` is the latest observation and `Revision` increases when the same event's
counters change. A bounded observation history retains changes and identity
conflicts, while durable log segments retain older entries. `Stable` means only
that consecutive samples agreed, not that Cursor finalized the event. A review
failure preserves the last observation and records the reason; it does not
replace known data with zero. Revisions replace a view of one charge, never add
another charge or alter a later response's current-context counters.

Historical checks use the same resolver as the current request, with no initial
delay and at most four concurrent candidates within two seconds. They run while
the current native Run is working. Request completion/cancellation drains the
worker before callbacks close; no credential survives in a background worker.
Previously measured and deferred Dashboard observations are eligible again.
Active candidates rotate fairly; cold archived records require offline review.

The v2 journal retains at most 10,000 active records and a 16 MiB checkpoint,
rotates small append-only segments, and keeps account/event ownership digests.
A bounded 512-entry/4 MiB in-memory spool absorbs write failures; exhausted spool
space is reported through dropped-observation counters. Corrupt, insecure or
already-owned paths are not overwritten. They enter degraded operation while
model replies continue. Unknown ownership suppresses Dashboard counters.
Memory-only overflow reports audit loss and cannot guarantee claims across
eviction/restart. Segments are audit archives, not an unlimited-storage promise;
archive/retention remains an operator responsibility. OS file syscalls themselves
have no Go deadline.

The authenticated accounting management endpoint exposes health only. A host
that drops custom response fields can still inspect degradation there. Schema v1
is imported without deleting its original bytes, and interrupted active Runs
remain audit records with no invented broad reconciliation window.
