# Validation

This document describes the independently maintained 0.1.3 candidate. The exact
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
