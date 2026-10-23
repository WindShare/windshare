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
temporary credentials through encrypted session control. Managed credentials last
up to one hour; the client derives their remaining lifetime from the issuer's HTTP
Date, subtracting request time instead of comparing clocks across devices.

Managed services follow the configured active relay selection even while an
application relay is disconnected. Discovery refreshes in the background; timeout
retains cached authority. Disable or device revocation withdraws it. Discovery
does not acquire TURN credentials or delay link publication. Shares using the same
relay share one discovery loop per Engine; the loop stops when its last share ends. Managed inventory
uses no manual account slots; sessions admit a bounded mix of both sources.

The console enables or disables the service. Disable persists across deployments,
stops issuance, and queues outstanding credentials for revocation. Pending,
provider-confirmed and expired counts are distinct: recipients may reuse a
credential until provider revocation or expiry. Cloudflare Realtime charges are
separate from Worker charges; automatic scheduling does not force TURN traffic.
Lease capacity includes unfinished issuance and unexpired credentials awaiting
revocation; retained records of completed leases do not consume capacity. Managed
requests reuse one lease ID across bounded retries. Uncertain results retain cleanup
ownership until the issuer accepts cleanup or authority expires. Early cancellation
blocks late issuance; shutdown reports unresolved cleanup. `TURNStatus()` separates
active leases from pending cleanup.

`managed_turn_policies[].local_budget_bytes` defaults to `0` (unlimited). A nonzero value
limits the deployment identified by that entry's `relay_url` within one Engine, subject to the budget semantics
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
Several URLs in one profile are alternatives for the same service. Initial
connections try TCP, then TLS, then UDP; each attempt delegates the same transport
to both peers. Failed connections advance to the next configured transport.
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
direct-attempt capacity. Readiness names an exact authenticated lane generation;
the sender retires its predecessor only while that same local admission remains
live. Failed renewal preserves a still-valid lane. Canceled or superseded requests
do not impose a failure cooldown.
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

Sustained content demand can evaluate one alternate transport using real queued
content. Readiness alone never replaces a working path: a contender must show a
sustained throughput improvement, scoped to the exact authenticated lanes and
both network generations. Trials have time and sampled-content budgets, share
the existing handoff capacity, yield to renewal/recovery, and cool down after completion. Idle or nearly
drained queues skip exploration. Learned transport preferences remain within
the session and network; transport selection does not change the configured exit.

Transient channel failures retry the same binding with bounded backoff. Only
selected paths with active demand can trigger allocation replacement, including
fixed credentials. Both ends must authenticate a healthy replacement before
retirement. Efficiency recovery leaves usable indication traffic running;
suspected allocation-state mismatch has a bounded recovery lifetime. Repeated
recovery is capped until fresh demand, network change or stable useful progress.

Cumulative reports coalesce per grant; lifecycle commands retain their order and
apply cancellable backpressure when full. Local acceptance means retained ownership,
not remote execution. Readiness retries after send failure and is idempotent for
its lane generation; an ended attempt cannot authorize a later one.

`local_budget_bytes: 0` disables the local limit. Nonzero limits share socket
usage across all profiles, shares and recipients using that account in one
Engine instance. Counters reset with the Engine. `ConfigureTURN(turn.Settings)`
validates and publishes manual accounts and managed deployment policies together,
preserving usage by account ID, including removal/re-import. Application settings
project through `relayconfig.Config.TURNSettings()`; discovery observations remain
separate. New requests use the latest settings; existing leases retain their issuing source.
Unrelated settings/discovery preserve pending issuance and renewal ownership;
changes to a profile's authority or endpoints invalidate only dependent requests.
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
`engine/turn` exposes settings and observations; account budgets, discovery and provider
assembly live in `engine/internal/turnruntime`. Engine callers inspect `TURNStatus()` and subscribe through
`TURNDependencies.Observe`; `TURNDependencies.Cache` supplies optional discovery persistence.
Local shutdown and provider-confirmed revocation are
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
with `uid` and `secret`. Positional arguments select `udp`, `tcp`, `tls`, or the full
`profile`; `WINDSHARE_TURN_TRIAL_BYTES` sets payload size (default 512 KiB, maximum
64 MiB). JSON reports handshake time, actual TURN control protocols, throughput and
SCTP window samples; stderr streams progress even if a transfer fails. Compare the
same transport and payload size; short transfers include congestion slow start.
Raw output includes network addresses; sanitize it before publishing.
Recorded evidence: [managed lifecycle](../testdata/connectivity/turn/managed-cloudflare-2026-10-20.json),
[provider connectivity](../testdata/connectivity/turn/cloudflare-2026-10-20.json),
[network throughput comparison](../testdata/connectivity/turn/network-throughput-2026-10-21.json).

Provider references: [Cloudflare credentials](https://developers.cloudflare.com/realtime/turn/generate-credentials/),
[Cloudflare analytics](https://developers.cloudflare.com/realtime/turn/analytics/),
[coturn authentication](https://github.com/coturn/coturn/wiki/turnserver).
