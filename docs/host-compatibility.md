# CPA host compatibility

The plugin emits standard token counters only when a measurement is available.
Omission is deliberate: an unknown cache count is not a measured zero, and a
byte estimate must not become a client's current context size.

The tests below run a separate CPA process with a synthetic C ABI executor.
They exercise the real plugin host, usage recorder, HTTP API, and Chat
Completions to Responses translator. They do not call Cursor or use real
credentials. The fixture is separate from the production plugin.

## Verified host contracts

| Behavior | CPA 8.0.38 | CPA 8.0.43 |
| --- | --- | --- |
| Known total input, cache read, output and reasoning reach Responses | Preserved | Preserved |
| Cache write reaches CPA accounting | Preserved | Preserved |
| Cache write reaches Responses | Omitted by host | Omitted by host |
| Missing plugin usage reaches Responses | Usage omitted | Usage omitted |
| Missing plugin usage reaches CPA accounting | Zero counters, `quality=complete` | Zero counters, `quality=complete` |
| Authoritative zero reaches Responses | Explicit zero | Explicit zero |
| Unknown reasoning counter reaches Responses | Not separately tested | Explicit zero; presence lost |
| Custom estimated/deferred usage extensions | Dropped | Dropped |
| Independent late usage correction callback | Unavailable | Unavailable |

The accounting limitation is material: a zero row in these hosts is not proof
that Cursor measured zero input, zero cache or zero cost. The plugin's separate
journal retains the unknown status and late reconciliation. It never adds a
previous Run's input to the next response's `prompt_tokens`. Older CPA hosts
cannot backfill their original row through the public plugin callback ABI.

Similarly, a custom `cursor_usage_estimated` flag cannot make estimated standard
counters safe: both tested hosts discard that flag, classify the counters as
complete, and forward the numeric context size. This plugin therefore does not
publish byte estimates in the standard usage fields.

Tested source revisions:

- CPA 8.0.38: `0db19025571684b198a2ef508f99b41c717337e8`.
- CPA 8.0.43: `307803510bce87f986fcf7b1a84190f105c7b4aa`.

## Proposed upstream host fix (not a released CPA version)

The separate [plugin-usage-presence patch](https://github.com/router-for-me/CLIProxyAPI/pull/7012) on top of CPA 8.0.43 has passed the real
C-ABI twelve-case suite with `--expect-patched`. It changes only the plugin path:
missing measurements have `quality=unclassified` and an explicit `observed`
field-presence object; authoritative zero remains distinguishable. Responses
retains cache-write detail and omits unobserved reasoning instead of inventing
zero. Streaming and nonstreaming cases are checked independently. Legacy
non-plugin producers keep their existing default contract.

This patch does not add an estimated-usage protocol or a late-correction
callback. A custom extension still cannot make guessed standard counters safe.
The installed released host must not be assumed to contain these changes merely
because this candidate passed its tests.

## Reproduce the offline host test

Requires Python 3, PyYAML, Go 1.23+, a C compiler, and a CPA binary for the same
OS and architecture. Run from the project directory. Choose an empty private
state directory and an unused loopback port; existing evidence is never erased.

```sh
python3 integration/cpa_compat.py \
  --cpa-binary /path/to/cli-proxy-api \
  --state-dir "$PWD/.local-tests/compat-8.0.43" \
  --port 28348
```

The default twelve cases cover streaming and nonstreaming exact usage, missing
usage, authoritative zero, estimated extensions, deferred extensions, and
unknown reasoning presence. The first ten cases were exercised on both listed
hosts; the two unknown-reasoning cases were separately exercised on 8.0.43. The exact
fixture has total input 140260, cache read 140160, cache write 20, total output
669, and reasoning 200. The nonoverlapping uncached input is 80. These are
synthetic counters; reasoning is already included in the output total. The test
asserts recorder and client values independently, saves only accounting fields,
and terminates its own CPA process. It does not load the real plugin.

To repeat only the reasoning-presence check, add `--scenario reasoning-unknown`.
The fixture deliberately omits `completion_tokens_details`; CPA 8.0.43 records
reasoning as zero and emits explicit `output_tokens_details.reasoning_tokens=0`
on both Responses paths. That zero is introduced by the host and does not prove
that the upstream measured zero reasoning. Presence-sensitive consumers must
consult the journal rather than reconstructing certainty from the translated
field.

## Bounded real-provider protocol

`start_isolated.py` starts an independent CPA with the actual compiled plugin,
one explicitly selected existing Cursor credential, a private journal directory,
random local test keys, request logging disabled, and a loopback listener. It
does not modify or reload an existing CPA process. A proxy configuration may be
read solely to reuse its configured outbound proxy.

```sh
python3 integration/start_isolated.py \
  --cpa-binary /path/to/cli-proxy-api \
  --plugin "$PWD/dist/cursor-next.dylib" \
  --auth-source /private/path/to/existing-cursor-auth.json \
  --state-dir "$PWD/.local-tests/provider-run" \
  --port 28347
```

In another terminal, `controlled_tools.py` makes at most four model requests
using only synthetic text and three allowlisted arithmetic tools. Its protocol
requires two tools in the first batch, actual results, another tool in the
second batch, its actual result, and a final answer.

```sh
python3 integration/controlled_tools.py \
  --state-dir "$PWD/.local-tests/provider-run" \
  --port 28347 --model cursor/grok-4.7@256k
```

This proves the tool/result continuation path only. Accurate accounting requires
an independent later dashboard comparison as well as the plugin journal,
Responses terminal usage, and CPA recorder. A dashboard event can be revised
after its first appearance, and one Run may include internal model steps;
functional completion alone does not prove final counts or current context
size. Keep real-provider evidence private. Do not commit credentials, raw CPA
queue records, real conversations, or account identifiers.

The independent read-only comparison can be repeated later to detect revisions:

```sh
python3 integration/compare_dashboard.py \
  --state-dir "$PWD/.local-tests/provider-run"
```

It compares each controlled Run's token counters against the private journal,
client Responses usage and sanitized CPA recorder fields. It keeps Run totals
separate when internal tool discovery caused additional model steps, and reports
that their context usage was deliberately withheld. It does not treat missing
client usage or a zero CPA row as a successful exact measurement.
CPA records are attributed by unique request start timestamps, including when
the queue order differs. Ambiguous attribution and numeric mismatches preserve
the report and return a nonzero exit status. To compare a Codex probe instead,
pass `--codex-result /private/path/to/result.json` and
`--cpa-records /private/path/to/sanitized-usage-queue.json`.

## Active stream unloading

CPA 8.0.43 invalidates plugin callbacks before invoking the plugin shutdown
hook. Its unload cleanup cancels upstream HTTP operations and HTTP body streams;
the downstream executor stream bridge is a separate resource. Downstream bridge
cleanup is tied to explicit `host.stream.close` or the original client request's
cancellation. A plugin cannot guarantee an explicit close after the host has
already invalidated its callbacks. Do not hot-swap a plugin with active client
streams; finish or cancel them first. The isolated test runner terminates its
own listener before loading another library in a fresh process.

A separate [upstream lifecycle patch](https://github.com/router-for-me/CLIProxyAPI/pull/7013) binds async executor streams to their exact
plugin instance. Its tests cover idle and blocked consumers, pending executor
calls, reloaded instances, and partial replies interrupted by unload. This is
independent of usage presence and is not included in released 8.0.43.

When a reasoning counter is unknown, an empty `output_tokens_details: {}` is
also unsafe: Codex requires `reasoning_tokens` when that object is present. The
proposed patch removes an empty details object entirely while preserving other
detail fields and explicit zero counters. The isolated Codex probe records
terminal parse errors as well as HTTP status so a successful transport alone
cannot pass this compatibility check.
