# CLIProxyAPI Cursor Plugin Next

Independently maintained Cursor provider for CPA with native HTTP/2 tool continuations and measured Run accounting. Native Go C ABI plugin; no Node runtime is needed. Supports streaming Chat Completions, CPA Responses translation, images, caller tools, authentication and quota reporting.

Based on [spancerxing/cliproxyapi-cursor-plugin](https://github.com/spancerxing/cliproxyapi-cursor-plugin) **v0.1.1**, commit `ad798f3350eb92774cde939c2f2aa452f21b688e`. The original MIT license, magpie attribution and historical reference source are preserved. See [NOTICE.md](NOTICE.md) and [UPSTREAM.json](UPSTREAM.json). This is not an official Cursor or CPA product.

[中文](README_CN.md) · [Validation](VALIDATION.md) · [Accounting design](docs/accounting-design.md) · [Host compatibility](docs/host-compatibility.md) · [Install and rollback](docs/installation-and-rollback.md)

## What changes

- Keep the same upstream Run across tool-result requests, following Magpie Cursor 0.2.23's native session design. Reuse pooled HTTP/2 connections; retain an independently owned HTTP/1 fallback when H2 negotiation is unavailable before submission.
- Validate account, model/Fast/context parameters, history and executable tool contracts before resuming. Bound session lifetime, cancellation, result reservation and exact replay; reject uncertain replays explicitly.
- Return tool handoffs without synchronous Dashboard polling. Reconcile final Run usage asynchronously with a bounded worker, without retaining a completed request's CPA callback.
- Emit raw executor JSON so CPA frames Chat and Responses correctly; avoid double `data:` and duplicate `[DONE]` markers.
- Tool rounds no longer report request/output bytes divided by four as exact tokens.
- Match Dashboard observations by account, stable conversation, model, time window and event identity; require repeated stable samples. The optional legacy `host` path retains its synchronous bounded lookup.
- Preserve cache-read, cache-write, uncached input and output semantics. Never add historical input to the current prompt size.
- A Run with internal tool discovery can contain multiple model steps. Its cumulative Dashboard or native `TurnEnded` usage stays in the separate journal; it is not a current-context measurement.
- Missing, ambiguous or unreadable usage is omitted, not replaced by fabricated zero/cache counts. Late measurements are recorded independently, including after restart when a private persistent journal is configured.
- Retain first/latest observations and revisions for mutable events; periodically recheck previously measured records without adding a second charge.
- Continue model replies when accounting storage is unavailable. The authenticated accounting route reports storage health, a bounded active-journal summary and the latest Run measurement without exposing account or conversation identities.
- Stamp builds with their source commit, dirty state and Go toolchain. Release provenance files bind that embedded information to the actual library SHA256.
- Remove the redundant system copy of full tool descriptions/schemas while retaining native MCP definitions and caller authority checks.

## Important compatibility limits

CPA **8.0.38 and 8.0.43** preserve omitted usage in Responses, but their accounting recorder still makes a zero row with `quality=complete`. They also discard custom quality/deferred fields, omit cache-write detail in Responses, and provide no callback to correct an earlier usage row. The separate journal is therefore needed for an honest accounting audit; CPA's zero row is not evidence of zero upstream use.

Cursor Dashboard events have no verified final marker and can change after first publication. Repeated stable samples reduce the race but cannot prove permanent finality. Internal multi-step totals are withheld from context usage. This project does **not** claim perfect billing finality or complete context measurement for every tool round. See the exercised limits in [host compatibility](docs/host-compatibility.md).

Native continuation is process-local and requires an explicit outbound route
because CPA does not expose its resolved proxy/custom TLS policy to executors.
Permanently lost, ended or expired owners return 410 with a stable error code and
recovery guidance; retrying the same handle cannot recreate it. Busy/in-flight
or correctable contract conflicts remain 409. A new ordinary user turn imports
full history; inspect completed tool side effects first. Exact per-step context
counts and compaction across a pending handoff remain unavailable. See
[native session boundaries](docs/native-sessions.md).

## Request support

| Input or control | Plugin behavior |
| --- | --- |
| Function tools | Declared names and JSON arguments are validated. `required`, forced choice and `parallel_tool_calls=false` are enforced on the complete returned batch before exposing any call. |
| `strict` tool schemas | JSON Schema validation, with format assertions and no external references. Invalid output is rejected, never silently repaired. This is not arbitrary Lark-grammar constrained decoding. |
| Images | PNG/JPEG/GIF/WebP base64 data URLs. Public HTTP(S) image URLs additionally require an explicit native route; private/fake-IP targets and unsafe redirects are rejected. Up to 8 MiB per image, 16 MiB total decoded attachments and eight remote images. |
| Files | Base64 UTF-8 text `file_data`, up to 1 MiB per file, includes real contents. `file_id`, filename-only placeholders, binary files, audio and video are rejected. |
| Other generation controls | Non-neutral controls without a native implementation, including output-token limits, stop sequences, sampling overrides and JSON response formats, return 400 instead of being silently ignored. |
| System/developer roles | Text and ordering are retained in the merged system prompt; native independent developer-role authority is not established. |

Remote-image preparation has a 15-second aggregate budget, 10 seconds per HTTP
request and at most three redirects. Host transport without a native image route
requires data URLs. Unreadable remote attachments return a safe 400 input error
(or 413 for size); they do not acquire the Cursor model's retry/cooldown hint.
Supported parsing and output validation do not prove that
the upstream model will satisfy every tool or grammar request.

## Build and test

Requires a Git checkout, Go 1.23+, Python 3, a C compiler and Make. Build for the same OS/architecture as the host:

Release packaging also needs a Go tool supporting `go version -m -json`; CI
builds and verifies provenance with Go 1.26.x.

```sh
make test
make build
python3 test/abi-smoke.py dist/cursor-next.dylib  # macOS; .so on Linux, .dll on Windows
```

Outputs are in `dist/`. The maintained build command resolves the plugin's
source with `git -C`, including linked worktrees, and embeds commit/dirty state
explicitly. Automatic Go VCS stamping is disabled because Go 1.26 can skip a
worktree's `.git` file and select an enclosing repository. The binary embeds the MIT license
and attribution, also available through the `plugin.license` ABI method. The
historical Node reference is excluded from native builds.

Version **0.2.2** is the plugin-only hardening candidate; the withdrawn 0.2.1
cross-component proposal is not a prerequisite or an upgrade target. Building
does not install or publish a release. Use an independent CPA process for
validation; installation and rollback instructions are separate. A version
string alone is not build provenance: compare the accounting route's `build`
fields with the package's `.provenance.json` and the installed library hash.

## Configuration

```yaml
plugins:
  enabled: true
  dir: /absolute/path/to/plugins
  configs:
    cursor-next:
      enabled: true
      priority: 1
      transport: native
      native-proxy-url: direct # or an explicit approved HTTP(S)/SOCKS proxy
      native-tool-result-timeout-seconds: 120
      native-run-lifetime-seconds: 900
      recover-response-service-tier: false
      usage-journal: /absolute/private/cursor-next/usage.json
      usage-wait-ms: 8000
```

The journal directory must be private (`0700` on Unix); journal and lock files are `0600`. On Windows, use an existing user-private ACL. `CURSOR_USAGE_JOURNAL` is an alternative to the config key; conflicting values are rejected. Empty journal configuration uses memory only and loses unresolved records on restart. A persistent journal has exclusive process ownership, a bounded active checkpoint, small append-only segments and durable event claims; do not share it between CPA instances. Change an in-use path only after stopping its host.

The provider remains `cursor`; existing Cursor credentials and `cursor/…` model IDs are reused. Load only one Cursor provider plugin in a host. The new library/plugin ID is `cursor-next`, so its artifact does not overwrite the upstream `spx-cursor` file.

`transport: auto` is the default: an explicit `native-proxy-url` enables native sessions, while an empty value retains CPA-host HTTP. `transport: host` forces the compatibility path. Native connection settings are independent of the host's authentication/catalog route; match them to the intended proxy and use streaming for prompt cancellation. Native Run billing lookup has an 8-second asynchronous budget (configurable 3000–30000 ms); tool handoffs never wait for it. A bearer is held only in memory for its Run and bounded reconciliation. In host mode, current-request lookup still waits synchronously and later requests recheck up to four old records within two seconds. No credentials or conversation text are stored in the journal.

The tool-result wait is configurable from 15–900 seconds and the Run lifetime
from 60–3600 seconds; the former must be shorter. Actual handoff deadlines are
also capped by the remaining Run lifetime and its processing reserve. This does
not extend upstream resumability. The accounting route reads back effective
transport and configured limits; per-handoff extension fields may be discarded
by CPA Responses and must not be assumed visible in Codex.

`recover-response-service-tier` is an opt-in for a verified CPA translator that
drops Responses `service_tier`. It recovers only an absent effective field from
the original request; an effective value, including null, wins. Leave it off
when a host policy intentionally removes that field.

For bounded real-provider and Codex probes, see [VALIDATION.md](VALIDATION.md). Public fixtures are synthetic; keep live credentials and evidence outside Git.

`GET /v0/management/plugins/cursor-next/accounting` (or the management base
provided by the host) retains `journal`, `active_runs` and `version`, and adds:

- `build`: the loaded library's source commit, dirty state and Go version; an
  unstamped ad hoc build reports `unknown`/`null` rather than the current checkout.
- `contract`: observations are Run totals, per-inference context is unavailable,
  missing is not zero, stable is not final, and late updates stay in the journal.
  It also reports effective transport, active native limits and the 410/new-user-turn
  recovery rule without exposing the proxy URL.
- `summary`: counts by measurement presence, status, source and failure reason,
  plus the latest measured Run's counters and revision. The scope is only the
  bounded active journal, not an account lifetime or daily-billing total.

`deferred` records already have a measurement obtained later; they are not an
unresolved queue. A failed recheck can leave a prior measurement intact with a
reason and `stable=false`. The endpoint is read-only, scans no archived segments
and exposes no credentials, paths or conversation/event identifiers. Standard
Responses may not preserve the plugin's custom accounting fields.
