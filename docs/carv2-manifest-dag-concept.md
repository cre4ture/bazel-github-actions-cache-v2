# Concept: CARv2 packs and a manifest DAG

Status: proposed for a future v0.3 release. This document does not change the
v0.2 object-per-entry format or its wire compatibility.

## Problem

The v0.2 backend maps every Bazel CAS object and Action Cache (AC) value to an
individual immutable GitHub Actions cache entry. That is deliberately simple
and correct, but a cold build can produce thousands of entries. GitHub limits
cache creation rate, so the entry count can dominate the time needed to seed a
cache. Independent cache eviction can also leave an AC value without one of its
referenced CAS objects.

The design below reduces cache-entry creation without adding an externally
hosted cache server or weakening Remote Execution API (REAPI) correctness.

## Goals

- Store many immutable CAS blocks in a bounded number of cache entries.
- Preserve the existing HTTP `/cas/<sha256>` and `/ac/<sha256>` behaviour for
  Bazel.
- Make a published Action Result visible only after its complete CAS closure is
  available.
- Support concurrent trusted writers without lost updates.
- Treat discovery delay, eviction, and all backend errors as ordinary cache
  misses, never as a source of incorrect build output.
- Reuse established Go implementations for the archive and DAG data formats.

## Non-goals

- Changing Bazel's SHA-256 plus size digest model.
- Making GitHub Actions Cache a durable artifact store.
- Supporting writes from untrusted pull requests or forks.
- Introducing remote execution, a gRPC API, or a persistent service.
- Replacing the v0.2 key space in place. The new backend uses a new namespace
  and can be rolled out or removed independently.

## Data model

### Pack

A pack is an immutable [CARv2][go-car] file produced with
`github.com/ipld/go-car/v2`.

- CAS values and serialized Action Result values are stored as raw CAR blocks.
- A CARv2 footer index maps block CIDs to byte offsets, so the runner can read a
  requested block after the pack has been restored locally.
- The CID uses the existing SHA-256 content hash. Bazel's digest remains the
  authoritative identifier at the HTTP boundary; the adapter also verifies the
  expected byte size before serving CAS data.
- A pack is identified by the SHA-256 of its complete CARv2 bytes and stored
  under an immutable Actions-cache key such as
  `<prefix>-pack-v1-<pack-sha256>`.
- Packs are not additionally compressed. The GitHub cache transport already
  compresses its value.

Packs should have a modest target size, initially 8 MiB and at most 32 MiB.
Oversized output blobs receive a single-blob pack. GitHub Actions Cache restores
the complete pack rather than an HTTP range, so very large packs would turn a
small CAS read into excessive transfer.

### Manifest

A manifest is a small immutable DAG-CBOR block encoded with
`go-ipld-prime`. Its CID is the manifest identifier and its Actions-cache key
is derived from that CID.

The final generated schema is a design-time decision, but its logical contents
are:

```text
Manifest {
  format_version
  parents: [ManifestCID]
  packs: [PackDescriptor { pack_cid, cache_key, byte_size }]
  cas_entries: [CASDigest -> PackCID]
  action_entries: [ActionDigest -> ActionResultCID, closure_pack_cids]
}
```

Entries are deltas introduced by that manifest, not a full copy of every
ancestor. A reader merges them from the roots toward the heads. The mapping
must be deterministic: the same CAS digest always denotes identical bytes. Two
different Action Result values for one Action Digest indicate a non-deterministic
build and are not silently selected; the adapter reports a cache miss and logs
the conflict.

Using [CARv2][go-car] and [IPLD][go-ipld] avoids inventing either a packed
content-addressed file format or a graph serialization and traversal format.
The remaining schema is intentionally REAPI-specific: it records the action
key and output-closure semantics that neither generic library can know.

## Publication protocol

1. On `PUT /cas`, the loopback server verifies and spools the block locally. It
   adds the block to a pending batch instead of creating an individual cache
   entry.
2. On `PUT /ac`, the server parses the Action Result and verifies its complete
   local-or-resolved CAS closure. It records an Action Digest mapping only when
   that closure is eligible for publication.
3. A bounded batch is flushed on size or time threshold and unconditionally by
   the action post step. It creates and uploads the CARv2 pack first.
4. Only after a successful pack upload does the server upload the immutable
   manifest delta which references that pack. The manifest is the commit point.
5. The post step waits for all pending flushes. A failed flush is logged and
   becomes a future cache miss; it never changes the build result.

Returning a successful `PUT` to Bazel before the final post-step flush is safe:
the current runner retains the local objects, while a cancelled workflow merely
loses an optimization. The manifest is never published before all objects it
advertises are available.

## Parallel writers and discovery

There is no mutable `latest` manifest because Actions-cache keys are immutable.
A numerical version alone cannot discover concurrent siblings. Each manifest
therefore has `parents[]`, not a single parent.

```text
M0
├── MA  (writer A, parents: [M0])
└── MB  (writer B, parents: [M0])
```

Readers list manifest keys by a fixed prefix through the GitHub Actions Cache
REST API, find manifests which no other listed manifest references as a parent,
then traverse and merge every head. A later checkpoint may publish
`parents: [MA, MB]`, but correctness never relies on every writer seeing every
concurrent head immediately.

The workflow needs read access to that REST API in addition to the existing
runtime cache token. Discovery can be eventually consistent: an unseen manifest
simply is not a cache hit in that job. Prefixes, digest shards, checkpoints, and
periodic compaction bound the number of manifests that a reader must list and
merge.

## Read protocol and eviction

1. Bootstrap an in-memory manifest view from all discoverable heads.
2. For `GET /ac/<action-digest>`, resolve the Action Result mapping, restore its
   pack, and validate every declared closure pack and referenced CAS digest.
3. For `GET /cas/<digest>`, resolve its pack, restore it on demand, look up the
   CID in the CARv2 index, and verify digest and size before responding.
4. If a manifest, pack, or closure member is missing or fails verification,
   return a cache miss.

GitHub can evict any cache entry independently. The manifest must therefore not
be treated as a promise that a pack still exists. This retains the v0.2 safety
property that incomplete Action Results are never served.

## Rollout and validation

The implementation should be a separate backend mode and namespace, initially
opt-in. Keep v0.2 available as a rollback path.

Required tests include:

- CAR round trips for CAS bytes, Action Result bytes, empty blobs, and large
  blobs.
- Manifest merge of independent sibling writers and a later multi-parent
  checkpoint.
- Visibility ordering: a manifest cannot be resolved before its pack exists.
- Missing or corrupt pack and incomplete closure behaviour, all as cache misses.
- Action Digest conflict detection.
- GitHub-cache API discovery pagination, unavailable API handling, and scoped
  branch/default-branch reads.
- An end-to-end cold seed followed by a fresh-runner warm restore, measuring
  cache-entry creations, transfer volume, and hit rate.

Before implementation, validate the chosen manifest schema and pack-size targets
with a prototype against a representative large Bazel build.

[go-car]: https://github.com/ipld/go-car
[go-ipld]: https://github.com/ipld/go-ipld-prime
