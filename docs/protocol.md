# Protocol Overview

> WindShare is in rapid development, and the protocol may change along with the implementation.
> This page only outlines the protocol's shape and vocabulary. The code, including the
> cross-runtime vectors in [`core/testvectors/`](../core/testvectors/), is the source of truth for specifics.

WindShare shares local files through an online sender with end-to-end encryption. A link works as soon
as the sender publishes a small descriptor; directories are scanned when first listed, and file content
is read on demand. Suite `0x02` uses AES-256-GCM, HKDF-SHA256, Ed25519, X25519, and HMAC-SHA256;
structured messages use deterministic CBOR.

## Maintenance

- Keep only what is needed to understand the protocol's shape and vocabulary; added detail is a regression, not an improvement.
- State current facts only: no recommendations, constraint rules, or plans.
- Leave byte layouts, constants, limits, error codes, timing, and in-progress migrations to the code; link to it instead of copying.
- When the shape or vocabulary changes, replace or delete the affected lines, and keep the page about its current size.

## Parties

| Party | Implementation | Role |
|---|---|---|
| Sender | `wind share` | Holds the sender key; serves catalog pages and file content from local sources. |
| Receiver | Browser (`web/`), `wind get` | Holds the link; browses, reads byte ranges, and writes output locally. |
| Relay | `relay/` (`wsrelay`), `relay-rust/` | WebSocket endpoint (`/v2/ws`) that publishes share routes, stores the sealed descriptor, and forwards opaque frames. |

## Layers

- **Relay control** (visible to the relay): `REGISTER`/`RESUME`, `JOIN`, `STOP`, connection liveness, and per-session flow control.
- **Lane**: a logical channel of a protocol session, carried by a `FrameChannel` (a relay session or a WebRTC DataChannel) that delivers ordered frames.
- **Protocol session** (end-to-end): handshake, `OperationEnvelope` framing, and operation messages.
- **Bootstrap descriptor** (end-to-end, independently sender-signed): establishes the share and sender before session admission.

Private source locators preserve native bytes; catalog names use a versioned Unicode grammar. ZIP retains logical names, while disk output preserves supported spellings and records compatible replacements when needed. Checkpoints and download resumption remain receiver-local.

## Flow

1. **Share**: the sender seals the descriptor and registers it with a relay by signing a relay challenge; the relay then publishes the route. `RESUME` restores the route of the same share after a reconnect, using a resume token and a new signed challenge.
2. **Join**: the receiver sends `JOIN` with the `shareId`, receives the sealed descriptor on a new relay session, and verifies it against `pkHash`.
3. **Handshake**: `ClientHello` (HMAC proof of the read capability, ephemeral X25519) and the sender-signed `ServerHello` bind session wire version 3 and establish a `ProtocolSessionID` and per-direction traffic keys. Version mismatches fail before operation admission. The relay session becomes the first lane.
4. **Operate**: the receiver lists directories, leases file revisions, and requests blocks. Peer signaling v5 binds initial, recovery, throughput, or TURN purpose. Each TURN offer carries its session-issued 16-byte grant ID. Direct offers carry CBOR null. A DataChannel joins the session after an authenticated `LANE_ATTACH` grant and a `LaneHello`/sender-signed `LaneAccept` exchange.
5. **Stop**: a signed `STOP` permanently revokes the relay route of that `ShareInstance`.

## Capability link

```text
https://<frontend>/<shareId>[?r=<relay-base>]#<fragment>
fragment = base64url(suite 0x02 || readSecret[16] || pkHash[16])
```

- `pkHash` hashes the sender's Ed25519 public key; `shareId` is derived from `pkHash` and names the relay route.
- `readSecret` is the only secret. The descriptor key and session admission key are derived from it per purpose.
- `?r=` lists relay base URLs; without it, the link's origin is the relay. Browsers do not send the fragment in HTTP requests.

## Content model

- **Descriptor**: `ShareInstance`, synthetic root directory, chunk size, capabilities, and sender public key. It has no directory tree, file count, or total size.
- **Catalog**: per-directory pages bound to a `DirectoryGeneration`. Each page carries the hash of the preceding canonical page; the terminal page completes the generation. Entries carry kind, ID, name, expected size, and an optional modified time.
- **Revision**: `OPEN_REVISIONS` returns, per file, a revision descriptor (`FileRevision`, exact size, modified time, source metadata) with a time-limited lease, or a file error.
- **Blocks**: `BlockRef = (FileID, FileRevision, LocalBlockIndex)`. Blocks have the descriptor's chunk size; the last one may be shorter. Byte ranges `[offset, end)` select blocks. A canonical `BlockRecord` is carried by one or more `BLOCK_FRAGMENT` messages identified by its payload hash. The completion message closes the result set.

Revision record schema 2 carries a 32-byte opaque source-metadata token (zero means unavailable), authenticated with the descriptor. It binds share/file, provider identity/version, size, modification evidence and chunk geometry independently of each open handle's random `FileRevision`. Equal tokens support best-effort reuse of locally verified plaintext ranges across handle reopens, while concurrent readers and reconnects within resume grace share the resident handle's `FileRevision`. A new identity requires checkpoint admission using the current lease.

The sender cache can serve canonical payloads across recipients. Catalog reloads verify payloads against the committed record and page hash. The block cache verifies each reload against the record identity committed by the revision read. Delivery authenticates either payload in the destination session.

## Session messages

After the handshake, every lane frame is an AES-256-GCM `OperationEnvelope` bound to the `ShareInstance`, `ProtocolSessionID`, lane, epoch, direction, and sequence. Its authenticated plaintext binds the message kind, operation identity and body, including the binary `BLOCK_FRAGMENT`. Sender controls and content share this boundary.

| Group | Messages |
|---|---|
| Catalog | `LIST_CHILDREN`, `CATALOG_RESULT`, `SCAN_PROGRESS` |
| Revisions and leases | `OPEN_REVISIONS`, `OPEN_RESULTS`, `RENEW_LEASE`, `LEASE_RESULT`, `RELEASE_LEASE` |
| Blocks | `REQUEST_BLOCKS`, `BLOCK_FRAGMENT` |
| Operation end | `OPERATION_COMPLETE`, `OPERATION_ERROR`, `CANCEL` |
| Lanes | `LANE_ATTACH` |
| Peer paths | `PEER_OFFER`, `PEER_ANSWER`, `PEER_CANDIDATE`, `PEER_PATH_CONTROL` |
| TURN (kind 20) | Session-scoped `Demand` requests `parallel` or `all` services. `Grants` reports `ok`, `unconfigured`, `retry_later`, or `partial_retryable`. With partial success, use the returned grants immediately and retry remaining capacity under bounded backoff while demand persists. |
| Session | `SESSION_TERMINAL` |

`PEER_PATH_CONTROL` (kind 19) is a session-scoped control carrying reachability
demand, revocation, mapping-ready, and network-change notices. Its body schema
version evolves independently.

A TURN grant advertises the milliseconds remaining until its issuer’s expiry. Zero denotes a static credential. Once an offer is accepted for negotiation, its grant remains valid until that deadline across subsequent rotations.

## Identities

| Identity | Identifies |
|---|---|
| `shareId`, `pkHash` | The relay route and the sender key binding carried by the link |
| `ShareInstance` | One shared content instance, spanning descriptor, catalog, revisions, and sessions |
| `DirectoryID`, `FileID` | Catalog nodes |
| `DirectoryGeneration` | One committed scan of a directory |
| `FileRevision` | One random read identity per opened file handle |
| `LeaseID` | One read grant on an opened revision |
| `ProtocolSessionID` | One authenticated receiver–sender session |
| `OperationID` | One request and its responses within a session |
| `LaneID`, `LaneEpoch` | A reusable session lane slot, and its current channel generation |
| `RelaySessionID` | One receiver connection's route through a relay |
| `PeerPathID`, `AttemptID` | A direct or TURN peer path, and one negotiation attempt on it |
| `PeerSlot`, `AttemptSequence` | One of eight session negotiation slots and its increasing attempt sequence |

## Code

| Topic | Go | Browser |
|---|---|---|
| Link, keys, signed descriptor | `core/link`, `core/internal/keyderiv`, `core/senderobject` | `web/src/crypto` |
| Catalog and content | `core/catalog`, `core/content`, `core/session/{catalogflow,contentflow}` | `web/src/catalog`, `web/src/content` |
| Session, envelope, lanes | `core/session/protocolsession`, `core/session/sendersession`, `core/session/receiversession` | `web/src/session` |
| Peer signaling | `connectivity/v2signal`, `core/session/peerslot` | `web/src/connectivity`, `web/src/session/peer-slots` |
| Relay protocol | `relay/protocol/v2`, `relay-rust/crates/relay-core`, `transport/relayv2` | `web/src/transport` |

The Rust relay kernel is `relay-rust/crates/relay-core`, and `make vectors` checks the shared vectors. Security boundaries are in the [threat model](威胁模型.md).
