# TURN Services

> WindShare is in rapid development, and TURN support may change along with the implementation.
> This page only outlines the TURN subsystem's shape and vocabulary. The code under [`engine/turn/`](../engine/turn/),
> [`connectivity/turnpath/`](../connectivity/turnpath/), and [`core/session/turncontrol/`](../core/session/turncontrol/) is the source of truth for specifics.

TURN adds relayed WebRTC DataChannel paths alongside direct peer connections and application relays.
An application relay still bootstraps discovery, session authentication, and initial signaling; once a
session is authenticated, the sender acquires or derives TURN credentials on demand and delegates them to
the receiver over encrypted [`TURN_CONTROL`](protocol.md#session-messages) messages so both peers allocate on the same service and transport.

## Maintenance

- Keep only what is needed to understand the TURN subsystem's shape and vocabulary; added detail is a regression, not an improvement.
- State current facts only: no recommendations, constraint rules, or plans.
- Leave constants, limits, budgets, cooldowns, retry timing, and byte layouts to the code; link to it instead of copying.
- When the shape or vocabulary changes, replace or delete the affected lines, and keep the page about its current size.

## Service sources

| Source | Configuration | Credential authority |
|---|---|---|
| Managed Cloudflare | Enrolled relay profile in `config.json` (`profiles`, `active_relays`) and optional `managed_turn_policies` | The Cloudflare Worker (`relay-cloudflare`) holds the TURN key secret, issues short-lived credentials to enrolled sender devices, and queues provider revocation when disabled or when a device is revoked. |
| Manual | `turn.accounts` and `turn.profiles` in `config.json` | The sender holds the account credentials locally (`static`, `coturn`, or `cloudflare` issuer) and issues or derives session grants without placing issuer secrets in the capability link. |

- **Managed discovery**: the sender refreshes managed TURN capability (`service_id`, `enabled`) from active relay profiles in the background and caches observations under `config.json.state/turn/`. Discovery does not acquire TURN credentials or delay link publication.
- **Manual issuers**:

| Issuer | `credentials` fields | Profile `urls` |
|---|---|---|
| `static` | `username`, `password`, optional `expires_at` | Required |
| `coturn` | `shared_secret`, optional `lifetime_seconds` | Required |
| `cloudflare` | `key_id`, `api_token`, optional `lifetime_seconds` | Optional (`turn:` / `turns:` subset override) |

```json
{
  "managed_turn_policies": [
    { "relay_url": "https://your-site.example", "local_budget_bytes": 0 }
  ],
  "turn": {
    "accounts": [{
      "id": "personal",
      "provider": "cloudflare",
      "issuer": "cloudflare",
      "local_budget_bytes": 0,
      "credentials": { "key_id": "TURN_KEY_ID", "api_token": "TURN_KEY_SECRET" }
    }],
    "profiles": [{ "id": "primary", "account": "personal", "urls": [] }]
  }
}
```

## Session control (`TURN_CONTROL`)

`TURN_CONTROL` coordinates TURN delegation between sender and receiver over any authenticated lane. Each message carries `Header {SessionID, OwnerPathID, Sequence}`.

| Message | Direction | Role |
|---|---|---|
| `Available` | Sender → Receiver | Publishes the sender's current TURN profile inventory. |
| `Demand` | Receiver → Sender | Requests TURN capacity while preview or download demand is active. |
| `Grant` | Sender → Receiver | Delegates `CredentialKind` (`Fixed` or `Expiring`), `Username`, `Password`, `ValidFor`, and `URLs` for a `PathID` (and optional `Replaces` predecessor). |
| `Unavailable` | Sender → Receiver | Reports that a profile cannot issue a grant, with `RetryAfter` and `Reason`. |
| `Ready` | Receiver → Sender | Confirms that `PathID` is attached to an authenticated session lane (`LaneID`, `LaneEpoch`). |
| `Recover` | Receiver → Sender | Requests bounded replacement of `PathID` (`AllocationStateMismatch` or `ChannelUnavailable`). |
| `Retire` | Either | Retires `PathID` (`Idle`, `Replaced`, `Expired`, `BudgetExhausted`, `ProviderRevoked`, or `Unhelpful`). |
| `Report` | Receiver → Sender | Reports cumulative receiver-confirmed `UsefulBytes` delivered over `PathID`. |
| `Quality` | Receiver → Sender | Reports per-lane-incarnation `ContentBytes`, `BusyMilliseconds`, and `PendingBytes` for transport evaluation. |

## Path lifecycle, routing, and budgets

- **On-demand issuance**: sharing and catalog browsing do not allocate TURN credentials; issuance starts only when the receiver signals content transfer demand (`Demand`) and `--connectivity` permits relayed ICE paths (`auto`).
- **Transport selection and trials**: profiles support `turn:...?transport=udp`, `turn:...?transport=tcp`, and `turns:...?transport=tcp`. Each attempt delegates a single transport to both peers, advances across configured transports on connection failure, and can trial an alternate transport under sustained content demand using `Quality` samples.
- **Make-before-break replacement**: renewal of `Expiring` grants, transport trials, and `Recover` prepare a successor path alongside the serving lane. The sender retires the `Replaces` predecessor only after `Ready` confirms the replacement's exact `(LaneID, LaneEpoch)`.
- **Engine capacity and local budgets**: each `Engine` instance owns independent TURN connection/socket admission pools, handoff reservations, and per-account or per-managed-policy `local_budget_bytes` counters. `TrafficUsage` tracks accepted payload, receiver-confirmed useful bytes, and local socket bytes separately; optional Cloudflare account `analytics` queries provider-sampled UTC day estimates out of band via `Engine.QueryTURNUsage`.

## Code

| Topic | Go | Browser / Relay |
|---|---|---|
| Engine settings, status, budgets, discovery | [`engine/turn`](../engine/turn/), [`engine/turncache`](../engine/turncache/), [`engine/internal/turnruntime`](../engine/internal/turnruntime/), [`engine/relayconfig`](../engine/relayconfig/) | — |
| `TURN_CONTROL` wire contract and vectors | [`core/session/turncontrol`](../core/session/turncontrol/), [`core/testvectors`](../core/testvectors/) | [`web/src/protocol/turn-control.ts`](../web/src/protocol/turn-control.ts) |
| Path delegation, renewal, recovery, quality | [`connectivity/turnpath`](../connectivity/turnpath/) | [`web/src/connectivity/turn`](../web/src/connectivity/turn/) |
| Route planning, capacity pools, ICE config | [`connectivity/turnroute`](../connectivity/turnroute/), [`connectivity/turncapacity`](../connectivity/turncapacity/), [`connectivity/iceconfig`](../connectivity/iceconfig/), [`transport/webrtc`](../transport/webrtc/) | [`web/src/connectivity/turn/rtc-configuration.ts`](../web/src/connectivity/turn/rtc-configuration.ts) |
| Managed Cloudflare TURN provisioning and issuance | — | [`relay-rust/crates/relay-cloudflare`](../relay-rust/crates/relay-cloudflare/) |
