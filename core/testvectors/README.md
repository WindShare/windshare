# Cross-runtime contracts

This directory contains interoperability contracts for wire messages, bootstrap
descriptors, content payloads, capability links, paths and diagnostics.
Go generates protocol vectors; TypeScript and Rust consume the relevant cases.

Scheduling and retry policies belong to each receiver. Their regressions live in
`core/transfer/lanescheduling/` and `web/test/content/`, without requiring identical
forecasts or timing. Authentication, revision identity and the meaning of completed
or recoverable output remain correctness requirements in every runtime.

Browser output bindings, persistence records and UI policies belong to `web/`.
Their fixtures live in `web/test/fixtures/output/`; native checkpoint behavior is
tested in `osfs/`.

## Generate and verify

Run `make vectors-update` to regenerate shared vectors, then `make vectors` for
non-mutating verification. `inventory.txt` is the exact, sorted JSON allowlist.
Web consumers run under `make web`; Rust consumers run under `make rust`.

## Contract families

- `v2-*`: capability links, signed bootstrap descriptors, authenticated sessions,
  canonical content payloads and protocol semantics.
- `ed25519-acceptance.json`: independent public-key and signature acceptance decisions
  shared by Go, TypeScript and Rust.
- `diagnostic-correlation-v1.json`: protocol/session/path/lane identities and their
  canonical diagnostic projections.
- `turn-control.json`, `path-policy.json` and `envelope-sample.json`: TURN control
  bodies, Unicode 15 logical path policy and vector envelope conventions.

Byte representations follow each file's established encoding. Integers beyond
JavaScript's safe range use decimal strings. The envelope `version` describes the
fixture format; each case's `kind` identifies the contract.
