# Cross-runtime protocol vectors

This directory freezes byte-level contracts shared by Go and TypeScript. Go is
the generation authority; Web tests reconstruct the same values through public
TypeScript codecs.

## Generate and verify

From the repository root, intentionally rewrite generated vectors once with:

```sh
make vectors-update
```

Use `make vectors` for non-mutating verification. `inventory.txt` is the exact,
sorted JSON allowlist, and Go tests reject missing or extra files.

## Inventory

Cross-runtime diagnostic contracts:

- `diagnostic-correlation-v1.json`: typed protocol/session/path/lane identities,
  canonical unpadded base64url projection, and safe numeric/text boundaries.

Receiver-local canonical contracts:

- `receive-intent-v1.json`: SelectionSpec, ArtifactSpec, binding, plan, and
  ReceiveIntent canonical bytes and digests for every legal plan family.
- `directory-admission-v2.json`: ReceiveIntent-bound layout, generation,
  ancestry, path, modified-time, HMAC, and settlement cases.
- `file-checkpoint-v2.json`: operation/intent/binding authority, owned object,
  lifecycle, checksum, envelope, transition, and verified crash-cut cases. Its
  `checkpointLineageId` rows isolate operation, intent, binding, file, canonical
  path segment, materializer, and authority axes while proving revision, size,
  owned object, ranges, lifecycle, and generations do not affect this receiver-local
  lookup index. Lineage is not `RecordID`, persisted checkpoint, or wire identity.
- `v2-semantics.json`: connection sizing, shape proof, artifact/plan/guarantee
  rows, WorkspaceBudget, picker timing, complete-only ZIP, checkpoint crash
  cuts, and receiver terminal projections alongside preserved protocol rules.

Other generated v2 files retain sender objects, sessions, fragmentation,
identity, and peer signaling. `path-policy.json`, `portable-path-vectors.json`,
and `envelope-sample.json` remain focused language-neutral fixtures.

Canonical receiver-contract identities and byte fields use unpadded base64url.
Older unrelated vector families may still use padded standard base64; each
consumer follows its file's established representation. Integers that may
exceed JavaScript's safe range use decimal strings. The top-level `version` is
the vector-envelope version, while each `kind` names the contract generation.
