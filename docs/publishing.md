# Publishing

The independent plugin ID is `cursor-next`; the auth provider/model prefix stays
`cursor`. Version 0.2.2 is the plugin-only hardening line. The withdrawn 0.2.1
cross-component proposal is not a release dependency; do not reuse its version.
Source changes, a local installation and a public tagged release are separate
states.

Pull requests and pushes to main run the native Linux AMD64/ARM64, macOS
AMD64/ARM64 and Windows AMD64 build/test matrix. No release is created by a PR
or ordinary branch push. Linux packages target glibc 2.17+, not Alpine/musl.

After review, CI and explicit release authorization, create a fresh version tag
matching `runtime.go` and `registry.json`. The tag workflow tests all platforms,
loads each actual C ABI library, validates ZIPs/provenance/checksums and publishes
the five platform packages with five `.provenance.json` sidecars. ZIP layout
stays one root-level library for CPA store compatibility. Preserve existing
release tags and archives. Source and
binary must retain the MIT license and complete upstream attribution.

All build entry points call `scripts/release.py build`, which fixes the module
root, reads commit/dirty state with `git -C` and embeds an explicit source stamp.
Automatic Go VCS stamping is disabled: Go 1.26's directory-only Git discovery
can skip a linked-worktree `.git` file and select an enclosing repository.
Packaging reads source commit/dirty state from the actual library's stamp and
target/toolchain from its Go build info; it never substitutes the current
checkout for a missing stamp. Each sidecar
binds those fields to the library SHA256. Checksums cover both ZIPs and sidecars,
and tag verification requires every library to come from the clean tagged
commit. Local dirty candidates can be packaged for inspection but cannot pass
the tagged-release check. This is traceable build metadata, not an external
reproducible-build attestation.

The versioned stamp is read only from mapped library data, with bounds checked
for the five supported platform layouts. Unstamped libraries, conflicting
stamps and stamp-like text appended outside the loaded image are rejected.
This does not authenticate a maliciously rewritten binary.

The provenance requirement starts at 0.2.2. Verification and store-submission
preflight can still read older five-ZIP checksum lists; a 0.2.2-or-later release
cannot use that legacy exception to omit its five provenance files.

`python3 scripts/submit-store-pr.py` previews a separate store submission.
`--submit` is an external publication action and requires its own authorization,
a complete matching release and an already-authorized GitHub account. A build,
PR, or third-party registry URL alone does not mean store acceptance or install.

See [installation and rollback](installation-and-rollback.md) and
[validation](../VALIDATION.md) for the candidate's explicit compatibility limits.
