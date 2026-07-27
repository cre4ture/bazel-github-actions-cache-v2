# Contributing

Issues and pull requests are welcome. Keep the adapter small, fail-open by
default, and compatible with Bazel's documented HTTP remote-cache protocol.

Before submitting a change, run every command in the development section of
the README. Changes to the Go server require rebuilding both checked-in
binaries with the pinned Go version. Changes to GitHub cache behavior should
also pass two separate smoke workflow runs: seed first, then restore-only.

Never log `ACTIONS_RUNTIME_TOKEN`, signed Azure Blob URLs, or the shutdown
token. New third-party GitHub Actions must be pinned to a complete commit SHA.
