# Owner-provided TURN

TURN adds relayed WebRTC paths alongside direct connections and application
relays. Recipients still open the link normally. Initial discovery, authentication
and signaling require an application relay; existing authenticated lanes can
carry subsequent credential replacement after that relay disconnects.

Add `turn` to the [sharing configuration](cli.md), then run
`wind config import config.json`. Import replaces the complete sharing settings.
For example, retain your existing frontend/relay fields and add:

```json
{
  "turn": {
    "accounts": [{
      "id": "personal",
      "provider": "cloudflare",
      "issuer": "cloudflare",
      "local_budget_bytes": 1073741824,
      "credentials": {
        "key_id": "YOUR_TURN_KEY_ID",
        "api_token": "YOUR_TURN_KEY_SECRET",
        "lifetime_seconds": 3600
      }
    }],
    "profiles": [{ "id": "primary", "account": "personal", "urls": [] }]
  }
}
```

Accounts own credentials and budgets; profiles select endpoint alternatives.
Profile and account IDs must be distinct within their respective lists, using
letters, digits, `_`, `-` or `.`. A profile references one account. Cloudflare can
supply its endpoints when `urls` is empty. Alternatively, select a subset such
as `["turns:turn.cloudflare.com:443?transport=tcp"]`.

| Issuer | `credentials` fields | Profile URLs |
| --- | --- | --- |
| `static` | `username`, `password`, optional RFC 3339 `expires_at` | Required |
| `coturn` | `shared_secret`, optional `lifetime_seconds` | Required |
| `cloudflare` | `key_id`, `api_token`, optional `lifetime_seconds` | Optional |

Use `turn:host:3478?transport=udp`, `turn:host:3478?transport=tcp`, or
`turns:host:443?transport=tcp`. Native TURN allocations currently support IPv4.
Several URLs in one profile are alternatives within one ICE connection.
Issuers default to one hour; supported lifetimes are one minute through 48 hours.
Static expiring credentials cannot be renewed automatically.

Recipients receive TURN usernames and passwords through encrypted session
control. They can reuse those credentials until provider expiry or confirmed
revocation. **Fixed passwords have no WindShare-enforced expiry.** The issuer
secret stays on the owner's device and never enters the share link. Import
explains this delegation once; protect the saved configuration like a password.

Credentials are acquired only for preview/download demand. Sharing and browsing
do not contact TURN issuers. The session permits two services plus one temporary
replacement path, reserving direct-attempt capacity. Both ends authenticate a
new lane before retiring the old lane. Failed renewal leaves a still-valid lane
available. Unhelpful sampled paths cool down before another attempt. Scheduling
uses real content measurements; comparable paths prefer direct, then owner TURN,
then application relays. `p2p-only` admits direct traffic only; `relay-only`
creates no ICE connections. Process admission permits 16 ordinary TURN connections
and one shared temporary handoff. The handoff admits both endpoints when sender
and receiver share a process: at most two extra connections and 16 extra socket
reservations beyond the ordinary 64. Gathering reserves its configured socket
cost; queued renewals retain the old path while waiting for capacity.

`local_budget_bytes: 0` disables the local limit. Nonzero limits share socket
usage across all profiles, shares and recipients using that account in one
Engine instance. Counters reset with the Engine. Exhaustion stops issuance and
retires that account's paths, leaving other services available. Socket samples
include local TURN/TLS framing and retries visible to the process, separately
from receiver-confirmed useful content. Sampling, in-flight data, external
credential reuse and other applications mean this is **not an account-wide
zero-overage guarantee**.

Connection details show the TURN service, health and useful received bytes.
CLI traces associate issuance, replacement, retirement, selected pairs and
socket counters with their service, session, path and available attempt IDs.
Engine callers can inspect `TURNStatus()` and subscribe through
`TURNDependencies.Observe`; local shutdown and provider-confirmed revocation are
separate events. Cloudflare supports credential revocation; static and coturn
credentials remain reusable until their own authority expires.

Optional Cloudflare `analytics` on an account takes `account_id` and a separate
`api_token` with Account Analytics permission. Call `Engine.QueryTURNUsage`
explicitly with inclusive UTC dates. This uses the key's sampled provider usage,
not an invoice or the local budget. Analytics never runs on the transfer path.

Offline coverage uses a local TURN server, injected clocks/HTTP clients and
shared Go/TypeScript vectors. Run `make vectors web short-go`; the full
native/browser renewal scenario is
`pnpm -C web exec playwright test v2-local-turn.network.spec.ts --config=playwright.network.config.ts --project=chromium`.
The opt-in [Cloudflare trial runner](../web/scripts/browser-network-matrix/run-turn-trial.mjs)
reads `WINDSHARE_TURN_TRIAL_KEY` pointing to a private JSON file with `uid` and
`secret`. See [recorded trial evidence](../testdata/connectivity/turn/cloudflare-2026-10-20.json).

Provider references: [Cloudflare credentials](https://developers.cloudflare.com/realtime/turn/generate-credentials/),
[Cloudflare analytics](https://developers.cloudflare.com/realtime/turn/analytics/),
[coturn authentication](https://github.com/coturn/coturn/wiki/turnserver).
