# Cross-runtime protocol vectors

This directory freezes the byte-level contracts shared by Go and TypeScript. The
normative design lives in the repository's [protocol specification](https://github.com/windshare/windshare/blob/main/docs/%E5%8D%8F%E8%AE%AE%E8%A7%84%E8%8C%83.md) and
[live-share refactor closeout plan](https://github.com/windshare/windshare/blob/main/docs/%E5%8D%B3%E6%97%B6%E5%88%86%E4%BA%AB%E4%B8%8E%E6%96%87%E4%BB%B6%E6%B5%8F%E8%A7%88%E9%87%8D%E6%9E%84%E6%94%B6%E5%B0%BE%E8%AE%A1%E5%88%92.md).

## Generation and verification

The Go generators produce these vectors deterministically. From the repository
root, explicitly update generated files with:

```sh
make vectors-update
```

Use `make vectors` to regenerate the expected bytes in memory and compare them
with the worktree without writing files. `inventory.txt` is the single
authoritative JSON filename allowlist. Keeping this directory inside the
independent core module lets released Go tests and root TypeScript tests consume
one byte-identical authority; the Go test rejects missing or extra JSON files.

## Inventory

Generated v2 contracts:

- `v2-identity.json`: suite-02 link identity and domain-separated keys.
- `v2-sender-objects.json`: canonical sender objects, encryption, and signatures.
- `v2-session.json`: relay identity, proofs, handshake, traffic keys, and controls.
- `v2-fragment.json`: authenticated block fragmentation and limits.
- `v2-semantics.json`: budgets, operation finals, selection, output, and lifecycle semantics.
- `v2-peer-signaling.json`: peer-signaling CBOR plus signed answer/candidate wrappers.

`v2-semantics.json` records protocol observations, including selection and output
classification. Its historical selection identity fields are not a durable
resume contract. The v1 transfer contract fixtures are generated from the Go
codecs and replayed by Web tests:

- `transfer-intent-v1.json`: canonical intent bytes and SHA-256 digest for node-ID
  and catalog-path selections. The synthetic root is the descriptor's opaque
  16-byte ID; run identifiers are included only as proof that they do not enter
  the durable bytes.
- `directory-admission-v1.json`: session-secret-scoped admission proofs with
  modified-time, parent, and exact terminal-settlement bindings.
- `file-checkpoint-v1.json`: canonical checkpoint bindings, phase transitions,
  certified ownership, and supported crash cuts.

Frozen cross-runtime fixtures:

- `path-policy.json`: catalog path canonicalization, rejection, and collision rules.
- `portable-path-vectors.json`: portable artifact paths, ordering, and collision rules.
- `envelope-sample.json`: language-neutral JSON envelope parser fixture.

Binary fields use padded standard base64. Integers that may exceed JavaScript's
safe integer range use decimal strings. Generated files must remain byte-stable
for identical inputs.
