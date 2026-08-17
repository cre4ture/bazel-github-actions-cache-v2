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

### CARv2 packs and manifest DAGs

`storage-mode: packs` is the v0.3 storage format. Instead of creating one
GitHub cache entry for every CAS or Action Cache object, it writes many values
to a modest CARv2 archive (8 MiB by default) and publishes one immutable
DAG-CBOR manifest after that archive is available. A large output is placed in
its own archive. The result is normally two Actions-cache creations per pack
(one pack and one manifest), rather than one per Bazel object.

```yaml
permissions:
  contents: read
  actions: read # manifest discovery for storage-mode: packs

steps:
  - id: bazel-cache
    uses: cre4ture/bazel-github-actions-cache-v2@<FULL_COMMIT_SHA>
    with:
      write: auto
      key-prefix: ironmesh-bazel-car-v1
      storage-mode: packs
      pack-size-mb: "8"
```

The `github-token` input defaults to `${{ github.token }}` and is used only to
list immutable manifest keys through GitHub's documented Actions-cache REST
endpoint. It must have `actions: read`; no external service, secret, or write
token is needed. Keep the packed mode in a new `key-prefix`: it deliberately
does not reinterpret or mix v0.2 object keys.

Writers discover all current manifest heads at startup and make each new
manifest a child of every head they observed. Parallel writers can therefore
publish siblings without overwriting each other. Readers discover and merge all
heads. If two manifests contain different Action Results for the same action
digest, that digest is treated as a cache miss rather than picking one result.
Manifest discovery is eventually consistent by design: an unseen manifest is
only a temporary miss.

The server flushes a pending pack when it reaches `pack-size-mb`, at least once
per `pack-flush-seconds`, and unconditionally during the action post step. It
uploads the CARv2 file first and the manifest second; the manifest is the
commit point. Thus cancellation, eviction, a corrupt pack, or a missing output
closure can only lose a cache hit and cannot supply incomplete build output.

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
| `storage-mode` | `objects` | `objects` (v0.2) or `packs` (CARv2 and manifest DAG) |
| `pack-size-mb` | `8` | Target size for a CARv2 archive; only for `packs`, 1–32 MiB |
| `pack-flush-seconds` | `30` | Maximum local staging interval; only for `packs` |
| `max-manifests` | `2048` | Bound on REST-discovered immutable manifests; only for `packs` |
| `github-token` | `${{ github.token }}` | `actions: read` token for packed-manifest discovery |
| `max-blob-size-mb` | `512` | Maximum spooled upload/download size |
| `max-concurrent-operations` | `4` | Backend-operation backpressure |
| `max-uploads-per-minute` | `180` | Evenly spaced uploads; must be below 200 |
| `backend-timeout-seconds` | `300` | Timeout for one GitHub cache operation and initial packed-manifest discovery |
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
- implicit handling of the standard SHA-256 zero-byte CAS digest
- immutable cache keys
- per-job coalescing of duplicate immutable `PUT`s before rate limiting and backend publication
- opt-in CARv2 archives with footer indexes, DAG-CBOR manifests, concurrent
  writer head merging, and action-result conflict detection

Not currently supported:

- Bazel `instance_name` path prefixes
- HTTP or zstd remote-cache compression
- gRPC or remote execution
- range requests
- Windows or macOS runners

## Limits and operational model

The `objects` format maps every Bazel AC or CAS object to one GitHub Actions
cache entry. It remains the default rollback path. GitHub documents a limit of
200 cache creations per minute; the action defaults to 180 evenly spaced
uploads. In `packs` mode a successful flush consumes one creation for the CARv2
pack and one for its manifest, so a large build graph is governed by pack count
instead of object count. `pack_uploads`, `manifest_uploads`, and
`pack_downloads` make that distinction explicit in the final statistics.

GitHub's repository cache quota, eviction policy, and branch restrictions all
apply. At the time of writing, the default repository quota is 10 GB and caches
not accessed for seven days may be evicted. Pull-request caches are scoped, while
runs can restore caches from the default branch according to GitHub's cache
scope rules. Cache misses and eviction are normal and must never affect build
correctness.

Because GitHub evicts entries independently, an AC entry can outlive one of its
referenced CAS entries. The adapter checks the complete output closure before
serving or publishing an action result and degrades an incomplete closure to an
ordinary cache miss. In packed mode this restores and verifies each referenced
CARv2 pack and CAS block before serving the Action Result. One action result is
limited to 100,000 distinct validation operations to bound amplification from a
malformed cache entry.

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
VERSION=v0.3.1 scripts/build-dist.sh
git diff --exit-code -- dist
(cd dist && sha256sum --check SHA256SUMS)
```

Release binaries are built with `CGO_ENABLED=0`, `-trimpath`, and an empty Go
build ID, with VCS stamping disabled, for deterministic Linux amd64/arm64 output. CI rebuilds them and
requires a byte-for-byte match.

The smoke workflow has two modes. A default-branch push seeds a stable packed
object. A separate `workflow_dispatch` restore run downloads the manifest and
one CARv2 pack on a new runner and asserts the persistent hit statistics.

## License

Apache-2.0.
