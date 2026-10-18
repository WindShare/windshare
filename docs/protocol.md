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
- **Sender objects** (end-to-end, sender-signed): descriptor, catalog pages, directory errors, revision descriptors, and block records, sealed independently of any session or lane.

Output naming, ZIP, checkpoints, and download resumption are receiver-local and not part of the protocol.

## Flow

1. **Share**: the sender seals the descriptor and registers it with a relay by signing a relay challenge; the relay then publishes the route. `RESUME` restores the route of the same share after a reconnect, using a resume token and a new signed challenge.
2. **Join**: the receiver sends `JOIN` with the `shareId`, receives the sealed descriptor on a new relay session, and verifies it against `pkHash`.
3. **Handshake**: `ClientHello` (HMAC proof of the read capability, ephemeral X25519) and the sender-signed `ServerHello` establish a `ProtocolSessionID` and per-direction traffic keys. The relay session becomes the first lane.
4. **Operate**: the receiver lists directories, opens file revisions to obtain leases, and requests blocks for byte ranges. `PEER_*` messages negotiate WebRTC inside the session; a DataChannel joins as another lane after a sender-signed `LANE_ATTACH` grant and a `LaneHello`/`LaneAccept` exchange.
5. **Stop**: a signed `STOP` permanently revokes the relay route of that `ShareInstance`.

## Capability link

```text
https://<frontend>/<shareId>[?r=<relay-base>]#<fragment>
fragment = base64url(suite 0x02 || readSecret[16] || pkHash[16])
```

- `pkHash` hashes the sender's Ed25519 public key; `shareId` is derived from `pkHash` and names the relay route.
- `readSecret` is the only secret. Content and session-authentication keys are derived from it per purpose.
- `?r=` lists relay base URLs; without it, the link's origin is the relay. Browsers do not send the fragment in HTTP requests.

## Content model

- **Descriptor**: `ShareInstance`, synthetic root directory, chunk size, capabilities, and sender public key. It has no directory tree, file count, or total size.
- **Catalog**: per-directory pages bound to a `DirectoryGeneration`. Each page references the previous one; the terminal page completes the generation. Entries carry kind, ID, name, expected size, and an optional modified time.
- **Revision**: `OPEN_REVISIONS` returns, per file, a revision descriptor (`FileRevision`, exact size, modified time) with a time-limited lease, or a file error.
- **Blocks**: `BlockRef = (FileID, FileRevision, LocalBlockIndex)`. Blocks have the descriptor's chunk size; the last one may be shorter. Byte ranges `[offset, end)` select blocks. Each block is a sealed `BlockRecord` carried by one or more `BLOCK_FRAGMENT` messages.

## Session messages

After the handshake, lane frames are `OperationEnvelope`s: AES-256-GCM bound to the `ShareInstance`, `ProtocolSessionID`, lane, epoch, direction, and sequence. The plaintext is CBOR `{kind, OperationID, body}`, except for the binary `BLOCK_FRAGMENT`. Sender results and controls also carry an Ed25519 sender signature.

| Group | Messages |
|---|---|
| Catalog | `LIST_CHILDREN`, `CATALOG_RESULT`, `SCAN_PROGRESS` |
| Revisions and leases | `OPEN_REVISIONS`, `OPEN_RESULTS`, `RENEW_LEASE`, `LEASE_RESULT`, `RELEASE_LEASE` |
| Blocks | `REQUEST_BLOCKS`, `BLOCK_FRAGMENT` |
| Operation end | `OPERATION_COMPLETE`, `OPERATION_ERROR`, `CANCEL` |
| Lanes | `LANE_ATTACH` |
| Direct path | `PEER_OFFER`, `PEER_ANSWER`, `PEER_CANDIDATE`, `PEER_PATH_CONTROL` |
| Session | `SESSION_TERMINAL` |

## Identities

| Identity | Identifies |
|---|---|
| `shareId`, `pkHash` | The relay route and the sender key binding carried by the link |
| `ShareInstance` | One shared content instance, spanning descriptor, catalog, revisions, and sessions |
| `DirectoryID`, `FileID` | Catalog nodes |
| `DirectoryGeneration` | One committed scan of a directory |
| `FileRevision` | One verified version of a source file |
| `LeaseID` | One read grant on an opened revision |
| `ProtocolSessionID` | One authenticated receiver–sender session |
| `OperationID` | One request and its responses within a session |
| `LaneID`, `LaneEpoch` | A lane within a session, and the generation of the channel carrying it |
| `RelaySessionID` | One receiver connection's route through a relay |
| `PeerPathID`, `AttemptID` | A direct path, and one negotiation attempt on it |

## Code

| Topic | Go | Browser |
|---|---|---|
| Link, keys, sender objects | `core/link`, `core/internal/keyderiv`, `core/senderobject` | `web/src/crypto` |
| Catalog and content | `core/catalog`, `core/content`, `core/session/{catalogflow,contentflow}` | `web/src/catalog`, `web/src/content` |
| Session, envelope, lanes | `core/session/protocolsession`, `core/session/sessionruntime` | `web/src/session` |
| Direct-path signaling | `connectivity/v2signal` | `web/src/connectivity` |
| Relay protocol | `relay/protocol/v2`, `relay/signaling`, `transport/relayv2` | `web/src/transport` |

The Rust relay kernel is `relay-rust/crates/relay-core`, and `make vectors` checks the shared vectors. Security boundaries are in the [threat model](威胁模型.md).
