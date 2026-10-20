# TURN services

TURN adds relayed WebRTC paths alongside direct connections and application
relays. Recipients still open the link normally. Initial discovery, authentication
and signaling require an application relay; existing authenticated lanes can
carry subsequent credential replacement after that relay disconnects.

## Managed Cloudflare service

The Cloudflare deployment workflow provisions TURN in both deployment modes.
`wind relay onboard` saves the service reference automatically; approved devices
request temporary credentials from the Worker. The deployment token stays in
Actions, the TURN key secret stays in the Worker, and recipients receive only
temporary credentials through encrypted session control.

Managed services follow the configured active relay selection even while an
application relay is disconnected. Discovery refreshes in the background; timeout
retains cached authority. Disable or device revocation withdraws it. Discovery
does not acquire TURN credentials or delay link publication. Managed inventory
uses no manual account slots; sessions admit a bounded mix of both sources.

The console enables or disables the service. Disable persists across deployments,
stops issuance, and queues outstanding credentials for revocation. Pending,
provider-confirmed and expired counts are distinct: recipients may reuse a
credential until provider revocation or expiry. Cloudflare Realtime charges are
separate from Worker charges; automatic scheduling does not force TURN traffic.
Lease capacity includes unfinished issuance and unexpired credentials awaiting
revocation; retained records of completed leases do not consume capacity.

`managed_turn[].local_budget_bytes` defaults to `0` (unlimited). A nonzero value
limits that deployment's usage within one Engine, subject to the budget semantics
below. It is not an account-wide spending cap. Optional console usage queries use
the separate read-only `CLOUDFLARE_ANALYTICS_API_TOKEN` deployment secret and run
only when requested; estimates cover the previous seven complete UTC days.

## Manual services

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
revocation. **Fixed passwords have no WindShare-enforced expiry.** For manual
services, the issuer secret stays on the owner's device and never enters the share link. Import
explains this delegation once; protect the saved configuration like a password.

Credentials are acquired only for preview/download demand. Sharing and browsing
do not request credentials; managed capability discovery may run in the background.
The session permits two services plus one temporary replacement path, reserving
direct-attempt capacity. Both ends authenticate a
new lane before retiring the old lane. Failed renewal leaves a still-valid lane
available. Canceled or superseded requests do not impose a failure cooldown.
Unhelpful sampled paths cool down before another attempt. Scheduling
uses real content measurements; comparable paths prefer direct, then owner TURN,
then application relays. `p2p-only` admits direct traffic only; `relay-only`
creates no ICE connections. Process admission permits 16 ordinary TURN connections
and one shared temporary handoff. The handoff admits both endpoints when sender
and receiver share a process: at most two extra connections and 16 extra socket
reservations beyond the ordinary 64. One route plan drives both gathering and
admission: one initial OS-routed socket per URL, with at most two staggered
source-bound backups on failure or stalled allocation. Backups acquire capacity
only when started; failed and unselected routes release it. Proven exits are
preferred within the current network generation.

Transient channel failures retry the same binding with bounded backoff. Only
selected paths with active demand can trigger allocation replacement, including
fixed credentials. Both ends must authenticate a healthy replacement before
retirement. Efficiency recovery leaves usable indication traffic running;
suspected allocation-state mismatch has a bounded recovery lifetime. Repeated
recovery is capped until fresh demand, network change or stable useful progress.

`local_budget_bytes: 0` disables the local limit. Nonzero limits share socket
usage across all profiles, shares and recipients using that account in one
Engine instance. Counters reset with the Engine. `ConfigureTURN` preserves usage
by account ID, including removal/re-import, and updates running sessions. New
requests use the latest settings; existing leases retain their issuing source.
Removing an account/profile or lowering its limit below usage retires affected
paths. Raising a limit permits new leases without reviving revoked ones.
Exhaustion stops issuance and retires that account's paths, leaving other services
available. Socket samples
include local TURN/TLS framing and retries visible to the process, separately
from receiver-confirmed useful content. Sampling, in-flight data, external
credential reuse and other applications mean this is **not an account-wide
zero-overage guarantee**.

Connection details show the TURN service, health and useful received bytes.
CLI traces associate route decisions, binding failures, issuance, replacement, retirement, selected pairs and
socket counters with their service, session, path and available attempt IDs.
Cleanup distinguishes local protocol completion from a forced deadline; neither
claims remote deallocation acknowledgment.
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
managed Worker discovery and native/browser renewal scenario (requires `worker-build`
and the Worker package's `npm ci`) is
`pnpm -C web exec playwright test v2-local-turn.network.spec.ts --config=playwright.network.config.ts --project=chromium`.
The opt-in [Cloudflare trial runner](../web/scripts/browser-network-matrix/run-turn-trial.mjs)
accepts `WINDSHARE_TURN_TRIAL_CONFIGURATION` pointing to an enrolled deployment's
private configuration, or `WINDSHARE_TURN_TRIAL_KEY` pointing to a private JSON file
with `uid` and `secret`. Recorded evidence covers [managed onboarding, renewal and cleanup](../testdata/connectivity/turn/managed-cloudflare-2026-10-20.json)
and the [direct provider trial](../testdata/connectivity/turn/cloudflare-2026-10-20.json).

Provider references: [Cloudflare credentials](https://developers.cloudflare.com/realtime/turn/generate-credentials/),
[Cloudflare analytics](https://developers.cloudflare.com/realtime/turn/analytics/),
[coturn authentication](https://github.com/coturn/coturn/wiki/turnserver).
