# Source and attribution

This is an independently maintained successor to
https://github.com/spancerxing/cliproxyapi-cursor-plugin, imported from tag
`v0.1.1`, commit `ad798f3350eb92774cde939c2f2aa452f21b688e`.
The original MIT license and upstream attributions below are preserved.
Changes to accounting, tool-prompt construction, testing and documentation are
maintained by the contributors to `timmyagentic/cliproxyapi-cursor-plugin-next`.
This project is not an official Cursor or CLIProxyAPI product.

The accounting configuration parser uses `gopkg.in/yaml.v3` v3.0.1, under its
MIT and Apache-2.0 terms. Its notices and the Apache license are preserved in
`third_party/` and embedded in the distributed library. See `plugin.license`
for the binary's license/notice texts.

The native implementation is based on the Cursor protocol, authentication,
model selection, transcript and quota behavior in
https://github.com/magpie-community/plugins/tree/main/packages/cursor,
`@magpie-community/opencode-cursor-auth` version 0.2.19, retrieved 2026-10-20.
The original MIT copyright and license are preserved in `LICENSE` and under
`reference/node-v0.2.0/vendor/cursor/LICENSE`.

Version 0.1.0 ports production behavior to Go. The native library contains no
JavaScript engine, JavaScript source assets, embedded worker or subprocess
bridge. The historical 0.2.0 reference implementation under `reference/` is
excluded from native builds.

`testdata/reference-golden.json` contains synthetic model, conversation and
protobuf expectations generated from that MIT reference. Native tests read
these checked-in fixtures without running Node or Bun. The developer-only
`scripts/generate-reference-fixtures.mjs` can regenerate them when intentionally
updating the reference. Running that optional development script needs Node;
installing, building, testing and running the native plugin do not.

The native protocol keeps the upstream MCP namespace `magpie` for wire and
transcript compatibility. This does not launch or require the magpie app.

Version 0.2.0's native session/transport redesign also references the MIT Cursor
plugin **0.2.23**, pinned to `magpie-community/plugins` commit
`9c886c12f92d0da504b9c54cf2dd9df623a61617`, specifically
`packages/cursor/native-session.mjs`, its tests, and `packages/cursor/index.mjs`.
Copyright (c) 2026 magpie-community contributors. Its root MIT license is
identical to the preserved `LICENSE`. The Go implementation adds pooled
connections and explicit CPA transport selection; it does not use Magpie's
heuristic allocation of cumulative Run usage to individual model steps.

`assets/cursor.png` is the unmodified `APP_ICON_25D_LIGHT.png` from Cursor's
official brand asset archive: https://cursor.com/brand (retrieved 2026-10-21).
Cursor's brand assets remain the property of their respective owner and are
not covered by this project's MIT license. This is a community integration.
