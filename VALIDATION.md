# Validation

## Native 0.2.0 validation

The native protocol/transport/runtime tests cover real local TLS HTTP/2 duplex,
HTTP/HTTPS CONNECT and SOCKS proxies, connection reuse, pre-submission fallback,
blocked writes and cancellation, malformed frames, proto3 default IDs, newer
scalar updates, batched results, contract mismatch, exact replay, expiry,
shutdown races and asynchronous accounting. `go test -race ./...`, `go vet ./...`
and the real shared-library C ABI check passed locally on macOS arm64/Go 1.26.10.
The workflow also runs the portable, credential-free A/B harness tests.

A small real-provider comparison on 2026-10-27 kept **Grok 4.7, 500K, priority,
medium reasoning**, the same credential, host/plugin binaries, outbound route,
8-second accounting budget and zero automatic retries. Each case required four
actual receipt-verified tools, a correct final answer and an ordinary follow-up
(six HTTP requests). The Responses case exercised namespace and custom calls.

| Small case | Native sessions | Host stateless |
| --- | ---: | ---: |
| Responses entire protocol | 23.49 s | 149.26 s |
| Responses median first semantic output | 2.05 s | 13.46 s |
| Chat entire protocol | 27.38 s | 116.27 s |
| Chat median first semantic output | 1.51 s | 11.77 s |
| Physical Run IDs in the journal, each case | 2 | 6 |

These are single controlled synthetic samples, not a universal speed guarantee
or decoder TPS measurement. Native retains one Run for four tool continuations
and starts another for the ordinary follow-up. Standard native usage is omitted;
Run measurements belong to the separate journal. The test host is an isolated
CPA 8.0.43 source build with the proposed usage-presence, stream-unload and auth
test patches; it is not the installed production service. Full tool-footprint
and larger-history evidence must be assessed separately.

`integration/native_sessions.py` runs `smoke`, `tools` (33 definitions / 442,107
bytes) and `stress` (additional synthetic history) against already-started
isolated listeners. `tools`/`stress` include a custom Lark grammar; validating the
result packet proves compatibility, not provider-side constrained decoding.
The default evidence root is `.local-tests/native` in the repository, overridable
with `CURSOR_NATIVE_TEST_ROOT`. Reports retain timings, hashes and presence-aware
measurement fields, never credentials or real user transcripts. `compare` checks
the two arms' environment and workload before comparing them. Both native and
stateless paths use the same candidate plugin, so this is a whole-path comparison
of transport plus continuation plus accounting delivery, not a transport-only
microbenchmark.

The full fixture v2 also passed with 33 tools / 442,107 bytes and a custom Lark
grammar accepting both JSON key orders. Native used `step,receipt`; host used
`receipt,step`. The earlier key-order-restricted fixture's host failure remains
a failed test and is not included in these comparisons.

| Responses fixture v2 | Native sessions | Host stateless |
| --- | ---: | ---: |
| Full tools, six-request protocol | 32.99 s | 172.82 s |
| Full tools, median first semantic output | 1.89 s | 14.55 s |
| Full tools + 100,000 padding units, protocol | 40.12 s | 255.43 s |
| Full tools + padding, median first semantic output | 1.86 s | 22.68 s |

A native-only 260,000-padding case passed all six requests in 58.30 seconds.
Its ordinary follow-up Run recorded 278,831 input tokens; the tool-chain Run
recorded a cumulative 1,935,116 input tokens over six observed inference steps.
The latter is **not** a single prompt's occupancy. This validates a larger
history on the 500K route, not filling the entire window or automatic compaction.
An independent later Cursor Dashboard snapshot matched all nine successful
cases' event counts (native 2, host 6) and all available cache/output totals.
Missing Dashboard counters remained unknown; event stability is not finality.

An actual Codex 0.163.0 app-server test sent 42 tools / 446,003 tool-definition
bytes per request, completed exactly three receipt-verified tools (including
the parallel operand batch) and returned the correct final marker in three
HTTP requests. Actual requests retained 500K, medium and priority; the copied
catalog retained 500000 / 400000 context/compaction limits with no compaction.
Stock installed CPA 8.0.43 separately passed the six-request native Responses
functional test. Standard per-step usage remained absent in both tests.

The installed `spx-cursor` v0.1.1 binary was also copied into an isolated stock
CPA for a migration check. The candidate passed all six full-tool requests in
34.11 seconds; v0.1.1 passed the four tools and final marker but its sixth,
ordinary follow-up failed with 502 EOF. No retry was made. Core, credential,
proxy and fixture identities matched, with different plugin binaries by design.
This incomplete pair does **not** establish a full-run speedup against v0.1.1.

`integration/transport_cooldown.py` additionally verifies the existing retry-hint
ABI without provider calls. For both synchronous errors and `stream.close`,
stock 8.0.43 returned 502/Retry-After:2, blocked an immediate request without
executing it, and allowed one new execution after 2.25 seconds. Without the hint,
the same paths still blocked after 2.25 seconds with about 58 seconds remaining.
No global cooldown setting was changed. Native local-TLS tests verify that only
classified network faults receive the hint, not HTTP auth/quota or protocol faults.

After the transport-retry and parked-turn recovery fixes, the final local
library passed the complete race/vet suites, 43 Python tests, C ABI loading and
packaging. The actual Codex app-server probe passed again on installed stock
8.0.43 with 42 tools, three verified tool calls and the original 500K/400K limits.
One additional full-tool custom-grammar repeat failed at the fourth call because
the model supplied a 62-byte string that was not valid JSON. The first three
tools and native continuity had succeeded. That call was rejected without tool
execution or argument repair, and the failed test is retained. Prompt-based
grammar adaptation does not provide reliable constrained decoding; the earlier
successful grammar cases must not be interpreted as a guarantee.

For an actual Codex app-server tool probe, `integration/codex_probe.py` accepts
`--model cursor/grok-4.7@500k --reasoning-effort medium --service-tier priority`.
Select the existing 500K catalog entry; the helper preserves its context and
compaction settings. It runs with a private test home and synthetic tools only.

This document describes the independently maintained candidate. The exact
v0.1.1 import remains in Git history and is identified in `UPSTREAM.json`.
All checked-in fixtures and example counters are synthetic. Keep live-provider
credentials, account identifiers, request logs and local evidence outside Git.

## Deterministic regression

```sh
make test
python3 -m unittest discover -s test -p 'test_*.py'
```

`go test -race ./...` and `go vet ./...` cover the existing authentication,
model selection, images, transcript continuity, native wire protocol and host
callback behavior, plus these accounting regressions:

- Stream and nonstream multi-tool batches, actual client-side arithmetic tool
  results, another tool round, and a final response through the native protocol.
- A deliberately large synthetic prompt recreates the old 236373 byte-derived
  estimate. Its independent synthetic measurement is 140260 input, 140160 cached
  input and 669 output. The measured numbers replace the estimate.
- Dashboard input includes uncached, cache read and cache write exactly once;
  native TurnEnded input is already total. Output includes reasoning; absent
  reasoning detail remains unknown at the plugin boundary.
- Present zero counters, missing events/messages, mutable snapshots, complete
  pagination, ambiguous matches, account/session isolation, simultaneous calls,
  HTTP 401/403/429, transient failures, bounded waits and cancellation.
- Independent deferred records, event claims, failed persistence, atomic writes,
  process ownership, real child-process crash/restart, interrupted Run auditing,
  fair reconciliation, and no addition of previous input to current context.
- Internal native tool discovery can create multiple inference steps. Dashboard
  Run totals are withheld from standard context usage and retained separately.
- Stream shutdown joins workers and drains already admitted callbacks. It does
  not claim to fix the host's separate hot-unload behavior for active clients.
- Complete native tool schemas, caller allowlists, namespace checks, tool choice,
  history/images, and system prompt reduction against historical fixtures.

## Build and C ABI

```sh
make build
python3 test/abi-smoke.py dist/cursor-next.dylib
python3 scripts/release.py package --goos darwin --goarch arm64 --library dist/cursor-next.dylib
```

Use `.so`/`.dll` and the actual target flags on other platforms. CI builds and
loads each platform library; a cross-compiled test binary alone is not runtime
validation. The native binary embeds `LICENSE` and `NOTICE.md`, available through
`plugin.license`. Release packages retain the single-library CPA store layout.

## Actual CPA compatibility

The separate host suite was exercised against CPA 8.0.38 and 8.0.43, ten synthetic
cases each. It loads an actual C ABI fixture and independently asserts Responses
and CPA recorder behavior. See [host compatibility](docs/host-compatibility.md)
for the reproduction command and exact commits. Passing the suite confirms both
preserved behavior and the documented gaps; it does not imply those gaps vanish.

## Real-provider and Codex acceptance

First run the three-request arithmetic protocol in an independent CPA instance,
then query Dashboard separately after completion. The later comparison must
check per-Run identity and all counters. Repeat the later read to detect mutable
snapshots. Do not equate HTTP 200, a working tool call, or a stable early sample
with final accounting.

The scripts and commands are in [host compatibility](docs/host-compatibility.md).
A real protocol run established that the first Run can perform an internal tool
catalog lookup. Its cumulative Dashboard count belongs in the journal, while
subsequent single-step requests can be compared directly at every layer. This
behavior is explicitly covered by the deterministic regression.

To exercise the real Codex app-server path, use its installed executable and
an existing model catalog. The probe copies the selected model entry without
raising its context window or compaction threshold, uses only synthetic dynamic
tools with real arithmetic/receipt validation, and records whitelisted request
metadata, Responses usage, Codex token updates and compaction events:

```sh
python3 integration/codex_probe.py \
  --codex /path/to/codex \
  --base-url http://127.0.0.1:28347/v1 \
  --key-file "$PWD/.local-tests/provider-run/keys.json" \
  --model-catalog /private/path/to/model-catalog.json \
  --catalog-slug your-existing-cursor-model \
  --output "$PWD/.local-tests/codex-probe" --large-tools
```

The probe has request/tool/time budgets and an allowlist of local arithmetic
operations. It verifies the final nonce-derived marker and absence of compaction
in that test. Inspect `tools_json_bytes` rather than assuming Codex preserves all
supplied schema annotations: clients may normalize or defer them. Compare every
emitted usage with the matching plugin journal, later Dashboard and CPA record;
unknown steps must be labeled as such. Results are scoped to the tested workload,
not a promise that legitimate future context compaction will never be needed.

## Remaining upstream/host limits

Dashboard has no verified finality marker. A bounded stable sample may still be
revised later. Multi-step Run totals are not single-step context occupancy.
Missing/inaccessible/ambiguous measurements remain unknown; an interrupted Run
without a persisted close time cannot safely be reconstructed after a crash.

CPA 8.0.43 also turns missing reasoning detail into explicit zero, losing its
unknown/zero distinction. CPA 8.0.38/8.0.43 omit unknown usage in Responses but make a zero `complete` row
in their own recorder. They discard custom quality/deferred fields, filter the
auxiliary header by default, drop cache-write detail in Responses and expose no
late correction callback. Deferred journal records therefore do not repair the
CPA row, and its zero is not a measured zero. Native Run retention is not
implemented; tool results continue through authenticated imported history.

These are explicit delivery boundaries. Neither a larger context limit, disabled
compaction, renamed Fast alias nor an estimated flag is used as the repair.

## Context growth and natural compaction

`integration/context_stress.py` creates only synthetic padding/tool results and
requires fresh receipts across every tool round. It copies an explicitly chosen
catalog entry's existing context/compaction settings unchanged. Use `--dry-run`
to review the workload before a real request. `--scenario approach` stays below
the intended threshold; `--scenario cross` grows past it. The latter requires
measured near-threshold occupancy, natural compaction, a measured decrease and
continued correct tool results. Unknown usage or zero observed compactions is
not sufficient to pass. Exit 2 means coverage is inconclusive; exit 1 indicates
a functional/budget failure. The rough cost guard uses explicit assumptions and
is not an upstream billing cap.

```sh
python3 integration/context_stress.py \
  --codex /path/to/codex --base-url http://127.0.0.1:28347/v1 \
  --key-file /private/isolated/keys.json \
  --model-catalog /private/prior-model-catalog.json \
  --catalog-slug cursor/grok-4.7@256k \
  --output "$PWD/.local-tests/context-cross" --scenario cross
```

Use an isolated host and keep all live evidence private. A changed live catalog
is a separate configuration change, not a reason to raise the test threshold.
