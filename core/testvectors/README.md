# Shared runtime contracts

This directory contains contracts implemented by multiple runtimes: wire messages,
sealed sender objects, capability links, canonical paths and shared scheduling or
naming algorithms. Go generates the protocol vectors; TypeScript and Rust consume
the relevant cases through their own implementations.

Browser output bindings, persistence records and UI policies belong to `web/`.
Their fixtures live in `web/test/fixtures/output/`; native checkpoint behavior is
tested in `osfs/`. ZIP reader interoperability belongs to `integration/outputformat/`.

## Generate and verify

Run `make vectors-update` to regenerate shared vectors, then `make vectors` for
non-mutating verification. `inventory.txt` is the exact, sorted JSON allowlist.
Web consumers run under `make web`; Rust consumers run under `make rust`.

## Contract families

- `v2-*`: sender objects, links, sessions, authenticated content and protocol semantics.
- `ed25519-acceptance.json`: independent public-key and signature acceptance decisions
  shared by Go, TypeScript and Rust.
- `diagnostic-correlation-v1.json`: protocol/session/path/lane identities and their
  canonical diagnostic projections.
- `content-scheduling-v1.json`: shared delivery estimates, evidence age, read-ahead
  and frontier rescue decisions. Host scheduling and resource ownership stay local.
- `name-collision-v1.json`: deterministic suffixes, extension preservation and UTF-8
  truncation at component limits.
- Signaling, TURN, portable-path and envelope fixtures cover their shared codecs
  and algorithm boundaries.

Byte representations follow each file's established encoding. Integers beyond
JavaScript's safe range use decimal strings. The envelope `version` describes the
fixture format; each case's `kind` identifies the contract.
