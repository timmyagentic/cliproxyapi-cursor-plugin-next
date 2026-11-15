# Tool payload reduction

The plugin still provides every available tool's exact name, description,
structured argument schema, and original compact schema JSON in Cursor's native
MCP configuration. It preserves both protocol locations used by v0.1.1:
RunAction configuration field 7 and Run MCP configuration field 1. Removing one
of these wire fields needs separate upstream protocol evidence.

The system message previously contained another complete copy of every tool's
description and schema, enclosed in `dynamic_tool_catalog`. That extra copy is
replaced by a fixed 425-byte routing instruction. The instruction explicitly
points to native MCP definitions, preserves the `magpie` namespace and
`CallDynamicTool` invocation shape, and tells the model to use the provided exact
name and schema. Caller tool names, descriptions, and schemas are not
interpolated into this system instruction. Tools with no complete description
still retain their exact native definitions.

User text is retained in turn order. System and developer text is merged into
the system prompt in encounter order; this does not establish native independent
developer-role authority. Changing
tool metadata no longer changes the appended routing instruction; this preserves
that part of the system prefix across requests. Native tool metadata may still
affect upstream caching, so this is not a cache-hit guarantee.

For a fresh Run, `tool_choice: none` supplies neither the routing instruction nor
native tools. A named function choice restricts the native definitions to the matching caller
tool. Automatic and required choices retain all caller tools, as before. The
executor's allowlist and namespace checks continue to reject unregistered and
foreign tool calls; the system hint never grants authority to execute a tool.
The completed output batch is checked before any call is exposed: `required`
needs at least one call, forced choice needs exactly one matching call, and
`parallel_tool_calls=false` permits at most one. A text-only completion cannot
satisfy a required invocation. A rejected batch ends the native owner rather
than exposing a valid prefix of an invalid batch.

Within a retained native Run, the complete tool declarations and schemas must
stay unchanged, while `tool_choice` and `parallel_tool_calls` can change for the
next response. The new policy takes effect only when a complete tool-result batch
is validated and reserved. `required` followed by `auto` or `none` can therefore
return final text without opening another Run. An identical result retry must
also retain its reserved policy; cached output is not reused under a different
policy.

Policy changes can narrow the original native catalog and later restore tools
from that catalog. They cannot introduce new executable tools. For example,
declaring `read` and `write` while initially forcing `read` bootstraps only `read`;
switching to `auto` would add `write` and is rejected with 409. To finish with text,
use `none` and preserve the declarations, or begin an explicit new user turn to
bootstrap a broader catalog. Forcing the only declared tool and then selecting
`auto` is supported. Changing a schema or strictness remains a contract conflict,
including when the next policy is `none`.

For `strict` function tools, JSON Schema validation uses a maintained validator,
Draft 2020-12 by default (or the schema's supported explicit draft), with format
assertions enabled. Local definitions are supported; network and local-file
external references are not. Invalid argument output is rejected without
repairing names, values or types. This is response validation, not an upstream
guarantee of arbitrary Lark grammar constrained decoding.

Valid reference transcript fixtures still compare the compact catalog hint,
assistant reasoning, prior tool calls, results and data-URL image bytes. The
legacy filename-only/unfetched-URL attachment fixture is now intentionally
rejected rather than retaining a misleading placeholder. Dedicated attachment
tests require real decoded UTF-8 file data or image bytes, and cover the explicit
native-route requirement for public image downloads. Tests must not call that
intentional rejection equivalent to reading the original attachment.

A deterministic synthetic fixture with 32 tools measures bytes, not tokens:

| Quantity | v0.1.1 | Current |
| --- | ---: | ---: |
| Caller tools JSON | 425,665 | 425,665 |
| Appended system catalog | 427,022 | 425 |
| Serialized transcript message blobs | 433,819 | 650 |

This fixture saves 433,169 serialized transcript bytes. Complete native tool
definitions remain present in both protocol locations, so the table does not
claim the whole request is 650 bytes. Upstream token counts must come from
measured usage; this byte reduction does not make a local byte/token estimate
accurate.

Reproduce with:

```sh
go test -run 'TestCompactToolRouting|TestReferenceConversationParity' -v .
```

These deterministic tests verify transport and transcript preservation. Actual
model tool discovery and multi-round continuation additionally require the
isolated host and real-model acceptance test described in the main validation
documentation.

A model may fetch the native MCP catalog before its caller-facing tool batch.
That creates internal inference steps and can make Dashboard usage a cumulative
Run total. The accounting layer detects these calls and withholds the total from
standard context usage. Tool payload reduction is therefore not a guarantee of
less latency or exact context measurement for every tool round.
