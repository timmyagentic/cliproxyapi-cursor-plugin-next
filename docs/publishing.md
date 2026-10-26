# Publishing

The independent plugin ID is `cursor-next`; the auth provider/model prefix stays
`cursor`. Version 0.1.2 is a candidate until separately approved for release.

Pull requests and pushes to main run the native Linux AMD64/ARM64, macOS
AMD64/ARM64 and Windows AMD64 build/test matrix. No release is created by a PR
or ordinary branch push. Linux packages target glibc 2.17+, not Alpine/musl.

After review, CI and explicit release authorization, create a fresh version tag
matching `runtime.go` and `registry.json`. The tag workflow tests all platforms,
loads each actual C ABI library, validates ZIPs/checksums and publishes the five
platform packages. Preserve existing release tags and archives. Source and
binary must retain the MIT license and complete upstream attribution.

`python3 scripts/submit-store-pr.py` previews a separate store submission.
`--submit` is an external publication action and requires its own authorization,
a complete matching release and an already-authorized GitHub account. A build,
PR, or third-party registry URL alone does not mean store acceptance or install.

See [installation and rollback](installation-and-rollback.md) and
[validation](../VALIDATION.md) for the candidate's explicit compatibility limits.
