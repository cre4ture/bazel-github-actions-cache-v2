# Bazel GitHub Actions Cache v2

An experimental GitHub Action that starts a loopback-only Bazel HTTP remote
cache for the lifetime of a job and persists its objects in GitHub Actions
cache v2. It needs no external cache server, cloud account, or secret.

```text
Bazel ── HTTP ──> 127.0.0.1:<dynamic port> ── cache v2 ──> GitHub
```

The Go server runs as a detached process after the action's main step. The
action's post step shuts it down, reports statistics in the job summary, and
removes its private runner temporary directory.

> [!IMPORTANT]
> This project is experimental. It uses the cache-v2 runner service exposed to
> GitHub Actions, through `github.com/tonistiigi/go-actions-cache`. GitHub does
> not document that runner upload/download protocol as a stable public API.
> Pin this action to a full commit SHA and evaluate the limits below before
> making it a required CI dependency.

## Usage

```yaml
permissions:
  contents: read

steps:
  - uses: actions/checkout@<FULL_COMMIT_SHA>

  - id: bazel-cache
    uses: cre4ture/bazel-github-actions-cache-v2@<FULL_COMMIT_SHA>
    with:
      write: auto

  - name: Test
    env:
      CACHE_URL: ${{ steps.bazel-cache.outputs.url }}
      CACHE_WRITABLE: ${{ steps.bazel-cache.outputs.writable }}
    run: >
      bazel test //...
      --remote_cache="$CACHE_URL"
      --remote_upload_local_results="$CACHE_WRITABLE"
```

`write: auto` is deliberately conservative: only a `push` to the repository's
default branch publishes entries. Pull requests are read-only by default, and
fork pull requests remain read-only even if `write: true` is requested. The
server validates but discards accidental PUTs in read-only mode so an optional
cache cannot fail the build.

For a trusted release or scheduled workflow, set `write: true`. For all
untrusted code, keep `write: false` and pass the emitted `writable` value to
Bazel.

### IronMesh example

The action can replace the external-cache URL and token in the Bazel job:

```yaml
- id: bazel-cache
  uses: cre4ture/bazel-github-actions-cache-v2@<FULL_COMMIT_SHA>
  with:
    write: auto
    key-prefix: ironmesh-bazel-v1
    fail-on-cache-error: "false"

- name: Bazel unit tests
  env:
    CACHE_URL: ${{ steps.bazel-cache.outputs.url }}
    CACHE_WRITABLE: ${{ steps.bazel-cache.outputs.writable }}
  run: |
    bazel test //:unit \
      --remote_cache="$CACHE_URL" \
      --remote_upload_local_results="$CACHE_WRITABLE"
```

Do not enable Bazel remote-cache compression with this release.

## Inputs and outputs

| Input | Default | Meaning |
|---|---:|---|
| `write` | `auto` | `auto`, `true`, or `false`; forks are always read-only |
| `fail-on-cache-error` | `false` | Strict mode; by default backend failures degrade to misses/soft upload success |
| `key-prefix` | `bazel-http-v1` | Immutable namespace; change to invalidate entries |
| `max-blob-size-mb` | `512` | Maximum spooled upload/download size |
| `max-concurrent-operations` | `4` | Backend-operation backpressure |
| `max-uploads-per-minute` | `180` | Evenly spaced uploads; must be below 200 |
| `backend-timeout-seconds` | `300` | Per-operation timeout |
| `port` | `0` | Loopback port; zero chooses a free dynamic port |

The main step outputs `url`, `stats-url`, `writable`, `bazel-args`, and
`initial-stats`. The post step emits `final-stats` and always writes the final
counts to the job summary. Because post steps run after normal job steps,
consume `stats-url` during the job if a later step must assert statistics.

## Protocol support

Supported:

- `GET`, `HEAD`, and `PUT` on exactly `/cas/<lowercase-sha256>`
- `GET`, `HEAD`, and `PUT` on exactly `/ac/<lowercase-sha256>`
- mandatory `Content-Length` and identity encoding
- CAS SHA-256 verification before publication and after download
- structural validation of REAPI `ActionResult`, `Tree`, and `Directory`
  messages
- AC hits only when every referenced CAS object still exists
- AC publication only after every referenced CAS object is persistent
- immutable cache keys

Not currently supported:

- Bazel `instance_name` path prefixes
- HTTP or zstd remote-cache compression
- gRPC or remote execution
- range requests
- Windows or macOS runners

## Limits and operational model

Every Bazel AC or CAS object is one GitHub Actions cache entry. This is the
simplest correct mapping, but large build graphs can create thousands of small
entries. GitHub documents a limit of 200 cache creations per minute; the action
defaults to 180 evenly spaced uploads and exposes throttle statistics. If this
becomes a bottleneck, object segmentation should be designed from measurements
rather than silently bypassing the limit.

GitHub's repository cache quota, eviction policy, and branch restrictions all
apply. At the time of writing, the default repository quota is 10 GB and caches
not accessed for seven days may be evicted. Pull-request caches are scoped, while
runs can restore caches from the default branch according to GitHub's cache
scope rules. Cache misses and eviction are normal and must never affect build
correctness.

Because GitHub evicts entries independently, an AC entry can outlive one of its
referenced CAS entries. The adapter checks the complete output closure before
serving or publishing an action result and degrades an incomplete closure to an
ordinary cache miss. Direct output blobs use cache-entry existence checks
without downloading their contents; `Tree` and recursive `Directory` metadata
must be downloaded so their file references can be checked. One action result
is limited to 100,000 distinct validation operations to bound amplification
from a malformed cache entry.

- [GitHub dependency cache reference](https://docs.github.com/en/actions/reference/workflows-and-actions/dependency-caching)
- [GitHub cache limits](https://docs.github.com/en/actions/reference/limits#cache-limits)
- [GitHub cache scope restrictions](https://docs.github.com/en/actions/how-tos/manage-workflow-runs/manage-caches#restrictions-for-accessing-a-cache)
- [Bazel HTTP remote caching](https://bazel.build/remote/caching)

## Failure policy

The default is fail-open:

- backend GET/HEAD errors are logged and returned to Bazel as cache misses;
- backend PUT errors are logged after the request body was bounded, spooled,
  and validated, then returned as a soft success;
- incomplete or malformed action results are not served or published and are
  reported separately in the final statistics;
- invalid paths, sizes, encodings, and CAS digests are always rejected.

Set `fail-on-cache-error: true` to return backend failures as HTTP 502. The
default protects build availability because a remote cache is an optimization,
not a source of truth.

## Threat model

The server binds only to `127.0.0.1` on a dynamic port. Its shutdown token is
random, masked, stored only as action post-state, and never exposed as a normal
output. The GitHub runtime token is inherited by the Go process but never
logged. Uploads are bounded on disk, backend concurrency is limited, and CAS
content is verified in both directions.

Code running in the same job can still reach loopback, inspect its own runner
environment, exhaust the job's local disk, or submit valid AC objects. Do not
run untrusted code in a cache-writing job. Fork pull requests are forced
read-only, but GitHub's general guidance about untrusted workflows and
self-hosted runners still applies.

Action-cache values cannot be content-verified against the action digest (the
digest addresses the action, not the serialized result). Cache poisoning is
therefore controlled by restricting writes to trusted workflows. Their
serialized REAPI structure and referenced SHA-256 CAS closure are still
validated before use.

## Development and reproducible binaries

The repository pins Go in `.go-version` and pins `go-actions-cache` to a full
upstream commit through a Go pseudo-version.

```bash
go test -race ./...
go vet ./...
go run honnef.co/go/tools/cmd/staticcheck@v0.7.0 ./...
go run golang.org/x/vuln/cmd/govulncheck@v1.6.0 ./...
node --test action/*.test.js
VERSION=v0.2.0 scripts/build-dist.sh
git diff --exit-code -- dist
(cd dist && sha256sum --check SHA256SUMS)
```

Release binaries are built with `CGO_ENABLED=0`, `-trimpath`, and an empty Go
build ID, with VCS stamping disabled, for deterministic Linux amd64/arm64 output. CI rebuilds them and
requires a byte-for-byte match.

The smoke workflow has two modes. A default-branch push seeds a stable object.
A separate `workflow_dispatch` restore run is required to download that object
on a new runner and asserts `backend_downloads == 1`.

## License

Apache-2.0.
