# Vendored Cursor transport

Source: https://github.com/magpie-community/plugins/tree/main/packages/cursor

Package: `@magpie-community/opencode-cursor-auth`, version `0.2.19`.
Retrieved on 2026-10-20. The adjacent
tests and fixtures are copied from the same upstream archive; run them with
`bun test ./vendor/cursor` from the plugin root. Upstream's MIT license is
preserved in `LICENSE`.

SHA-256 of the original production sources:

```text
699b6532ff75ce11f6168aa1799c21e2d875d80ba45901dfcb4c8d79099a6f02  index.mjs
03b476c66b71fd3c0d0a83dc88abe2ff1f38f7910b679ccae90433a8a59de047  native-session.mjs
```

Version 0.2.0 applies the small, auditable [cpa-transport.patch](cpa-transport.patch)
to `index.mjs`. It redirects lexical `fetch` calls through an AsyncLocalStorage
transport, selects the existing RunSSE/BidiAppend path while inside CPA, and
selects stateless history reconstruction because CPA HTTP operations cannot
outlive their originating callback scope. No protobuf codec or message schema
is changed. Outside CPA transport scopes, the original direct HTTP/2 and
native retained-session behavior is unchanged.

SHA-256 of the patched `index.mjs`:

```text
00a5713203aaa3f4fd90af60eb1724f65efd181b3532f71d694c1e72c496ec11
```

The integration is implemented in `../../adapter.mjs`, `../../transport.mjs`,
`../../quota.mjs`, `../../worker.mjs`, and the Go C ABI bridge. The original
tests are unchanged.
