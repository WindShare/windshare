# User-Provided TURN

Let share owners configure standard TURN services, including Cloudflare Realtime TURN, while receivers keep opening a link and downloading normally. TURN adds transport choices and reduces pressure on application relays; it remains a relayed route, not a successful P2P direct connection. Initial discovery, authentication, and signaling still require application relays; E2EE and resumable content delivery remain unchanged.

## Context

- Browser configuration already preserves TURN entries in `web/src/connectivity/peer-offer.ts`. Native `transport/webrtc/provider/connection.go` replaces ICE servers with STUN-only configuration, and `connectivity/icepolicy/endpoints.go` accepts only STUN URLs.
- Route admission and transfer accounting distinguish direct, TURN, and application-relay traffic, but scheduling uses a common relay cost factor. PeerSet separates browsing from content demand; product entry points still wire one peer path.
- Standard address/username/password configuration covers compatible public and self-hosted TURN services. Temporary credential issuance varies by provider; coturn supports shared-secret issuance. Public STUN addresses do not provide file relaying. See [WebRTC configuration](https://www.w3.org/TR/webrtc/#rtciceserver-dictionary) and [coturn authentication](https://github.com/coturn/coturn/wiki/turnserver).
- Cloudflare currently offers 1,000 GB/account/month shared with Realtime SFU, then $0.05/GB of billed egress. Credentials last at most 48 hours. UDP, TCP, and TLS 443 are available; individual allocations can encounter packet drops above approximately 50–100 Mbps. China traffic reaches locations outside the China Network. See [pricing](https://developers.cloudflare.com/realtime/sfu/platform/pricing/), [transport limits](https://developers.cloudflare.com/realtime/turn/), and [credential lifecycle](https://developers.cloudflare.com/realtime/turn/faq/).

## Execution

1. **Model services and credentials separately.** Introduce stable account/profile identities, transport endpoints, credential ownership, and explicit fixed or expiring credential states. Add application/CLI configuration for ordinary TURN, Cloudflare credentials, and coturn shared-secret issuance. Express renewal, revocation, and usage reporting as actual provider capabilities.

2. **Supply credentials through a small provider boundary.** Implement static, Cloudflare, and coturn sources with injected HTTP clients and clocks. Engine owns account configuration, issuance, and shared budgets. The native sender delivers credentials to authenticated receivers over encrypted control, bound to the session and peer path. Explain delegation once during owner setup: recipients can reuse TURN credentials until expiry or provider revocation; fixed passwords have no WindShare-enforced expiry. Keep issuer secrets on the owner's device and provider APIs outside `core/**`; add the required Go/TypeScript signaling.

3. **Integrate native and browser connections.** Replace the STUN-only attempt input with an immutable connection configuration that keeps discovery endpoints and TURN credentials semantically distinct. Preserve direct connectivity while adding TURN over UDP/TCP/TLS. Account for TURN connections and allocations in resource ownership, cancellation, and teardown; the existing UDP mux does not cover Pion's separate TURN sockets. The pinned Pion currently skips IPv6 TURN gathering; expose only verified address-family support.

4. **Own paths and credential replacement.** Refactor session-level PeerSet ownership to manage direct and per-service TURN paths. Route renewal and replacement signaling through existing session control on surviving authenticated lanes, including after application-relay loss. Renew credentials ahead of expiry, admit a replacement lane before retiring the old connection, and retain still-valid paths if replacement fails. Reserve bounded overlap capacity during replacement and reclaim retired slots while preserving the download job, completed content, and receiver intent. Pion updates apply only at later gathering; choose explicit connection replacement or ICE restart. Keep issuance and recovery failures local to the affected provider/path.

5. **Activate and schedule by demand.** Keep link creation and browsing independent of TURN issuance; acquire and renew only while preview/download demand exists. Continue available delivery immediately, without a startup speed test. Reuse LaneSet's transfer measurements and completion estimates for bounded exploration and scheduling; preserve direct-attempt capacity and retire unhelpful TURN paths with a cooldown against churn. Treat one service's URLs as alternatives; use independent paths for useful parallel traffic. Prefer direct routes, then user-provided relays over public relays when performance is comparable and budgets allow. Keep `p2p-only` direct-only and `relay-only` free of ICE; ICE's relay candidate policy remains a separate transport setting.

6. **Expose useful control and observations.** Show service health, actual route/provider, and useful TURN bytes. Track transport overhead/retries separately from provider billing estimates; keep billing queries off the transfer path. Enforce each account's local usage budget across shares and receivers managed by the Engine instance; stop issuance and retire only affected paths when exhausted. Distinguish local shutdown from provider-confirmed revocation, and do not promise exact account-wide zero overage. Trace issuance, expiry, allocation, selected-pair changes, and fallback with stable provider/account/profile/session/path/attempt identifiers.

7. **Validate early and keep development reproducible.** Run small real Cloudflare native-to-browser UDP/TLS 443 and credential-replacement trials before finalizing path policy; record selected routes, useful throughput, resource use, and provider egress. Reuse local TURN and transport fixtures with injected clocks/HTTP clients. Cover native-side TURN, TLS fallback, credential replacement after application-relay loss, shared budgets, failure isolation, and cancellation cleanup; normal development remains independent of external accounts.

## Experience and maintenance

Unconfigured users incur no TURN network activity. Configured users gain another path without waiting for it before sharing or starting available transfers. Demand-driven acquisition can delay TURN readiness; extra active paths consume sockets, CPU, memory, and shared uplink capacity, so more connections do not inherently improve speed.

The maintenance cost is credential and connection lifecycle complexity. Keep provider behavior in adapters, account policy in Engine, and path coordination in the session-level PeerSet; reuse existing transfer progress and scheduling. Provider accounting capabilities remain optional, allowing ordinary TURN services to work without a billing integration.

## Implementation evidence

- [Configuration and operating model](../turn.md): static, coturn and Cloudflare accounts, delegated credentials, demand-driven replacement, shared budgets and diagnostics.
- [Cloudflare trial record](../../testdata/connectivity/turn/cloudflare-2026-10-20.json): native/Chromium UDP and TLS 443, overlapping replacements, socket cleanup and independently queried provider usage.
- `web/e2e/v2-local-turn.network.spec.ts` exercises owner-issued credentials and authenticated replacement after application-relay loss, preserving the download and verifying its hash.
- Shared wire fixtures live in `core/testvectors/turn-control-v3.json`; local lifecycle, provider, budget and cancellation tests require no external account.
