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

User and developer instructions retain their original order and text. Changing
tool metadata no longer changes the appended routing instruction; this preserves
that part of the system prefix across requests. Native tool metadata may still
affect upstream caching, so this is not a cache-hit guarantee.

`tool_choice: none` supplies neither the routing instruction nor native tools.
A named function choice restricts the native definitions to the matching caller
tool. Automatic and required choices retain all caller tools, as before. The
executor's allowlist and namespace checks continue to reject unregistered and
foreign tool calls; the system hint never grants authority to execute a tool.
The existing implementation does not enforce a required invocation if Cursor
chooses a text answer. This change does not add that enforcement.

The reference transcript fixtures remain unchanged. Tests replace only their
exact legacy system-catalog suffix with the compact hint before comparing the
entire transcript. User/developer text, assistant reasoning, prior tool calls,
tool results, missing-result handling, and image content continue to match the
reference fixtures.

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
