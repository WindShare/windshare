# Global Network Plan

Official servers worldwide, a fast entry in mainland China, and community relays, built on the
existing relay, STUN, and P2P stack.

Inputs: [原始需求](../../clarifications/原始需求.md), [中转服务](../../clarifications/中转服务.md),
[conversation](global-service-历史对话.md).

## 1. Context

### Requirements

- Official servers grow across regions. Mainland China needs ICP-filed hosting and a fast entry of
  its own, and a backup entry exists.
- Self-hosted relays and frontends stay first-class. Official servers differ only by being built
  in, with an emergency update path.
- Receivers, browser or CLI, never need an account. Senders with an account get full official
  service; senders without one get the lowest class where a node allows it.
- Idle official capacity is free to use. Under contention, priority time (10 minutes a day by
  default) is charged only while actually received. Each account has a daily traffic allowance, and
  exhausting it yields bandwidth instead of disconnecting.
- Self-hosted native relays contribute idle capacity by default, and their owners always come
  first. Cloudflare Workers relays do not contribute by default.
- Nodes discover each other while the official operator exists, so the community keeps working
  without it. Keeping old links alive after the official operator exits is not a goal.
- Servers are added from a console, without repository changes.
- Official relaying runs on purchased servers; Cloudflare hosts the official web page.
- More servers must improve the experience, never burden it. P2P stays the primary path.

### Resources

- Overseas VPS with public IPv4 in the US, NZ, KR, JP, SG, AU, UK, VN, IN, and DE at 5, 10, or
  50 Mbps.
- Two independent mainland servers, CN-A and CN-B, with IPv4 at 100 Mbps each.
- The existing ICP-filed `.top` domain (`<filed-domain>`), `windshare.link`, and a free Cloudflare
  account.

### Current state

| Area | Today |
|---|---|
| Sender | Registers with every selected relay in parallel, at most 8 (`connectivity/relayset`); the link is ready on the first admission. A static `AccessKey` travels as a bearer token on fresh registrations only. |
| Selection | The CLI calls `relayconfig.SelectRelays`: saved `active_relays` or `--relay`, nothing implicit. The frontend comes from `front_url` or `--front-url`. |
| Link | Repeated `r` parameters carry percent-encoded relay base URLs; without them the link origin is the relay. |
| Receiver | Races the link's relays, then attaches the rest as extra lanes (`web/src/receiver/relay-race.ts`). Download state lives in origin-scoped IndexedDB and origin-private storage. |
| `wsrelay` | One actor per share. Every registration gets `Attribution::Anonymous` (`relay-rust/crates/relay-native/src/actor/ingress.rs`). STUN over UDP, a browser Origin allowlist (`--origins`), and per-source limits. |
| Cloudflare deployment | A relay with restricted admission, owner console, device enrollment (`relay-access`), and managed TURN, plus the frontend as Worker static assets on a custom domain (`scripts/deploy/cloudflare`). Sized for personal use. |
| STUN | Only `stun:stun.l.google.com:19302` ships (`connectivity/icepolicy/defaults.go`); mainland reach needs measurement. An attempt uses at most two STUN endpoints. |
| TURN | Managed Cloudflare TURN through an enrolled Cloudflare relay, or manual `static`, `coturn`, and `cloudflare` issuers; delegated to receivers on demand over `TURN_CONTROL`. |
| Discovery | `/.well-known/windshare` v1: `access`, `relay_url`, optional `console_url` and `frontend_url`. |

## 2. Decisions

1. **One link host, resolved by the receiver's network.** Senders cannot know where receivers are,
   and a page that fails to load cannot redirect anywhere. Mainland servers may serve only
   ICP-filed names on ports 80 and 443, and most mainland receivers open links inside WeChat or QQ,
   where unfiled domains are more often blocked or interrupted by warnings. Download progress and
   recovery records live in origin-scoped browser storage, so a receiver has to stay on one origin.
   Links therefore use `s.<filed-domain>`, which resolves to CN-A and CN-B on the mainland line and
   to Cloudflare elsewhere. `windshare.link` is the official site and the backup entry; a backup
   entry helps a link open, never a download resume.
2. **The frontend is release configuration, never directory data.** The frontend serves the code
   that reads the key in the link fragment, so whoever chooses the frontend can read shared
   content. Built-in frontends ship with the release, users may set their own, and the directory
   lists only relays and STUN. A directory-key compromise affects availability and metadata and can
   expose P2 bearer tickets for unauthorized relay use; it does not expose content keys. Move hosting
   through DNS on the same hostname; change built-in hosts through signed client updates with the
   desktop release workflow, without a separate frontend-change protocol.
3. **Relays are stateful, so links name them.** A share route exists only on the node the sender
   registered with (SQLite, the live sender socket, and a relay identity hashed from the canonical
   endpoint URL), so GeoDNS or anycast would land receivers on nodes without the route. Each node
   keeps its own hostname. Replacing a machine keeps the hostname, certificate, and node key, so
   ordinary replacement needs no ContactID or per-share lookup service.
4. **"Official" is a signed directory with an offline root.** Clients embed a snapshot and pinned
   root keys and refresh from any mirror: frontends, GitHub, and later every node. Signatures make
   mirrors untrusted, which provides the emergency update path and survives GitHub being
   unreachable from mainland China. An offline root authorizes time-limited online signing
   generations; P1 bounds cached trust and permits recovery after a key leak. Directory-sourced
   STUN is trusted deployment configuration; signaling and links cannot supply endpoints.
   Applications inject the network into the engine, so library callers get no implicit
   infrastructure and forks ship their own root keys.
5. **The engine targets two registrations from bounded candidates.** CLI, desktop, and mobile
   share the planner. P0 reachability and recent connection results order candidates while preserving
   zone and provider diversity. Actual registration measures usability; TCP success alone is not
   admission. Sharing starts on the first admission; receivers schedule paths by measured performance.
   Connection attempts stay bounded as the fleet grows.
6. **Data plane and control plane are separate.** Nodes are cheap, replaceable, and independent.
   They verify tickets offline; an authority outage blocks new logins and ticket refresh while
   authorized transfers continue within local capacity and budget limits.
7. **Tickets carry identity; entitlement state travels in a usage loop.** Nodes report usage every
   minute and receive versioned, expiring account entitlements. This keeps soft allowances shared
   across nodes without reservations or a central call per transfer; the interval is not a hard
   overrun bound. Identity tickets last hours; stale entitlements fall back to the lowest class.
   Priority time is the union of intervals across nodes: a minute on two nodes costs one minute.
8. **User entitlements and node budgets are separate limits.** Spending an account's daily traffic
   moves it to the lowest class, yielding under contention rather than disconnecting. P0 node relay
   budgets cover owner and public relay egress; P4 contribution caps cover public relay egress only.
   Exhaustion stops covered traffic, including existing sessions. Control traffic and any finite
   communication reserve count toward the total. P2 adds scheduling within these limits.
9. **Anonymous senders get the lowest class.** Senders without a ticket may use idle official
   capacity, are refused first under pressure, and are limited per source; the console switches
   the class per node. It is off by default on `cn` nodes and on for `global` nodes. Mainland relay
   service stays disabled until P2 adds account admission. Existing P2P paths continue independently;
   new P2P negotiation still needs an admitted signaling path.
10. **Cloudflare hosts the web and the authority, not relaying.** The official site and the
    non-mainland side of the link host are static assets. The authority runs on Workers and Durable
    Object SQLite. The Cloudflare Workers relay and Cloudflare TURN are not part of the official
    service; official relaying runs on purchased servers. Existing
    [capacity evidence](../../../relay-rust/crates/relay-cloudflare/tests/capacity-evidence.md) supports
    personal trials, not public relay capacity. Personal Cloudflare deployments remain self-hosted relays.
11. **Community nodes are discovered, not approved.** A node's identity is its key, published in a
    self-signed descriptor. Nodes and clients exchange bounded descriptor samples, and the official
    directory only adds recommendation weight, so discovery works throughout rather than starting
    after the official operator disappears. Relays are untrusted by design (end-to-end encryption,
    sender signatures, content verification), so admission guards availability and metadata
    exposure, not confidentiality.
12. **Complexity waits for measurements.** P0 peak-hour network tests, ongoing node-load summaries,
    and client diagnostics guide the Hong Kong line, receiver-suggested relays, official TURN, and
    relay forwarding. Relay load alone does not establish P2P failure or identify the bottleneck.

## 3. Rejected alternatives

| Alternative | Reason |
|---|---|
| `windshare.link` as the main link host | The filed host uses the existing mainland deployment. Existing overseas VPS can host `windshare.link` without buying a Hong Kong server; mainland reach needs measurement |
| Frontend chosen by the sender's zone | Senders cannot know where receivers are |
| Redirect script or "switch entry" button on the receiver page | A page that fails to load runs no code, and another origin cannot see the download state |
| Frontends in the signed directory | The directory signer could point new links at a page that reads their keys |
| One geo-routed relay hostname | Routes are node-local; receivers would land on nodes without the share |
| Global "where is this share" lookup | Central state on every join; stable hostnames already cover ordinary replacement |
| One-shot anonymous byte allowance as a signaling guarantee | Opaque file data can exhaust it before negotiation or recovery; keep anonymous admission, source limits, and node budgets |
| Relay-to-relay forwarding now | Each byte spends bandwidth on two 5–50 Mbps nodes; waits for measured failures |
| Separate rendezvous and bulk relays now | P2P carries bulk and users are few; receiver-suggested relays and TURN delegation cover it when measured |
| Budgets enforced inside tickets | Each node spends the same budget |
| Quota reservations or priority slices | Unnecessary coordination for soft account allowances |
| Per-node device enrollment for official relaying | Users would enroll on every server; one account works across all nodes |
| Directory changes through GitHub pull requests | GitHub is unreliable from mainland China; the console is the operating surface |
| Official approval or subscribed community directories | The community would depend on a publisher; descriptor exchange needs none |
| Public relaying on Cloudflare free | Capacity evidence supports personal trials only |
| Unsigned `server.json` on GitHub | Unreliable from mainland China; unsigned mirrors would become trusted infrastructure |
| IP geolocation as a hard node-selection rule | Exit location is only a cold-start hint; measured reachability and performance take precedence |

## 4. Hosts and domains

| Host | Mainland line | Other lines | Serves |
|---|---|---|---|
| `s.<filed-domain>` | CN-A, CN-B | Cloudflare | Links, receiver, mainland official site, installers |
| `api.<filed-domain>` | CN-A, CN-B (proxy) | Cloudflare | Authority API, from P2 |
| `cn1.`, `cn2.<filed-domain>` | Own server | Own server | Mainland relay and STUN |
| `<region><n>.windshare.link` | Own server | Own server | Overseas relay and STUN |
| `windshare.link` | Cloudflare | Cloudflare | Official site, backup entry |
| `authority.windshare.link` | Cloudflare | Cloudflare | Origin behind the `api.` proxy |

- The filed domain's DNS stays on a provider with an overseas resolution line, such as DNSPod or
  Aliyun DNS, whose free tiers have it; Cloudflare's free DNS cannot answer by region.
  `windshare.link` stays on Cloudflare, with node records DNS-only (STUN is UDP, and proxying adds
  latency).
- Workers custom domains require the hostname's zone on Cloudflare, so `s.` and `api.` reach the
  same Workers as Cloudflare for SaaS custom hostnames on the `windshare.link` zone. They must be
  subdomains, because the free plan cannot take a zone apex as a custom hostname.
- CN-A and CN-B obtain certificates for `s.` and `api.` through DNS-01, since validators abroad
  resolve those names to Cloudflare. Node hostnames use HTTP-01.
- Official public relays accept valid HTTP(S) browser origins, including self-hosted frontends.
  Admission and capacity policies apply independently; private deployments may restrict origins.
- After P2 enables mainland relaying, a link can read
  `https://s.<filed-domain>/<ShareID>?n=cn1&n=jp1#<key>`.
- Overseas nodes live under `windshare.link`, so a problem with the filed domain cannot take them
  down.
- Keep the filed `.top` domain. Publish the exact official link hostname in the app and website;
  no brand-domain migration is planned.

## 5. Capacity and node roles

- 50 Mbps is about 6 MB/s, and every relayed byte enters and leaves the node, so one node sustains
  only a few full-speed relayed transfers. 5–10 Mbps nodes suit STUN, signaling, and small files.
- Total relay capacity is a few hundred Mbps overseas and 200 Mbps in the mainland: enough for
  early adoption with P2P carrying bulk traffic, not for relay-first use. P2P success remains the
  main lever.
- Resident-share limits bound each node's registration coverage, not global share availability:
  another admitted relay can keep a share usable when one zone is full.
- An attempt uses at most two STUN endpoints, and a wave at most four. A larger candidate pool can
  improve fallback coverage without increasing these per-client limits.
- A metered plan bounds relaying more than its bandwidth does: 50 Mbps at full speed moves about
  16 TB a month. Nodes that can incur overage charges need a relay quota before public use;
  fixed-cost nodes may omit it. Host billing also includes traffic outside the relay.
- Subdomains share the filing, and each mainland hosting provider needs its own filing access.

| Node | Roles |
|---|---|
| CN-A, CN-B (100 Mbps) | Mainland line of `s.` and `api.`; STUN, relay from P2; health vantage points |
| Overseas 50 Mbps | Relay, optional STUN; one is a health vantage point |
| Overseas 5–10 Mbps | Low-weight relay, optional STUN |
| Cloudflare | `windshare.link`, the other lines of `s.` and `api.`, the authority (Worker and Durable Object SQLite), directory mirror |

## 6. Phases

P1 needs the P0 fleet; P2 can start alongside P1. P0–P2 enable mainland relaying and public
promotion. P3 automates fleet operations afterward; P1's local signing already adds servers without
client releases. P4 builds on P2's descriptors and classes. P5's priority time builds on P2's
governor and can ship before P4.
[Desktop live sharing](../desktop-live-sharing.md) step 2 builds on P1.

### P0 — Fleet bring-up

Links open fast in both regions, and mainland P2P improves.

- Servers: CN-A, CN-B, and two or three 50 Mbps overseas servers, one each near Asia, Europe, and
  the Americas. Other servers can join through P1's signed directory; P3 automates onboarding.
- DNS and certificates follow [Hosts and domains](#4-hosts-and-domains); verify split DNS, certificate
  issuance, and Cloudflare for SaaS routing to the Workers.
- On CN-A and CN-B, one TLS reverse proxy on port 443 routes by hostname: `s.` to the web build and
  installers, `cnN.` to `wsrelay`, and from P2 `api.` to `authority.windshare.link`.
- Add an explicit public Origin mode to `wsrelay` for official relays. Run it under systemd behind
  TLS ingress on TCP 443 with `--public-endpoint`; selected STUN hosts also open UDP 3478.
- Node configuration owns enabled services; ingress and discovery use the same state. Until P2,
  mainland relay service is disabled: reject registration, resume and joins, and close relay sessions
  when disabling it. STUN remains available; directory roles and Origin allowlists are not access control.
- Before exposing a relay on an overage-billed plan, enforce a persistent node-wide egress byte budget
  shared across actors, reset only by the configured billing period. Exhaustion stops forwarding
  on existing connections too. Account classes and fair scheduling arrive in P2.
- The Cloudflare deployment gains a `frontend` mode beside `full` and `relay-only`: static assets
  only, serving `windshare.link` and the other lines of `s.`.
- One release pipeline publishes each web build to Cloudflare and both mainland servers, because
  one origin is served from both sides. `/downloads/` serves installers from CN-A and CN-B on the
  mainland line and redirects to GitHub releases on Cloudflare.
- STUN: seed a small pool of reviewed public services and measured mainland endpoints in
  `connectivity/icepolicy/defaults.go` and `web/src/connectivity/ice-policy/defaults.ts`.
  Rank by local reachability, recent success, and provider diversity within existing attempt/wave
  limits, without delaying link creation. P1 adds directory sources; embedded public fallbacks remain.
- In-app browsers: find out which receiver functions (preview, saving, streamed ZIP, WebRTC) work
  inside WeChat and QQ. Where saving does not, the receiver page detects the in-app browser and asks
  the user to open the link in the system browser.
- Measure, at evening peak, connection success, throughput and loss between mainland and overseas
  nodes, and from home networks in both zones to each node. Results set initial weights and zone
  preferences and decide the Hong Kong line and the [data-triggered work](#data-triggered-work).
- Record node-local active-session counts, relay bytes, per-session active-forwarding duration and
  throughput distributions, and admission/queue pressure; P2 exports these aggregates in its usage loop.

### P1 — Built-in network

Any client shares globally with zero configuration.

Directory payload, signed as exact bytes with Ed25519 so no JSON canonicalization is needed:

```json
{
  "format": 1, "network": "windshare", "generation": 1, "serial": 42,
  "issued_at": "2026-11-09T00:00:00Z", "expires_at": "2027-02-09T00:00:00Z",
  "nodes": [{
    "alias": "cn1", "relay": "https://cn1.<filed-domain>", "stun": ["stun:cn1.<filed-domain>:3478"],
    "roles": ["stun"], "zone": "cn", "region": "cn-east", "provider": "<provider>",
    "operator": "official", "capacity_mbps": 100, "status": "active"
  }],
  "public_stun": []
}
```

- The envelope carries the base64url payload, a root-signed certificate binding the online key,
  generation, and validity, and the online signature. Clients pin two root keys held apart and
  reject rollback within a generation. Only a root-authorized higher generation can replace it
  and restart serial numbering; online keys cannot advance the generation.
- Directory trust ends at the earlier of certificate expiry or payload expiry plus a bounded
  client-defined grace period. Refresh in the background; afterward use explicit relays or
  independent community discovery. Expiry alone does not end live transfers.
- `status` is `canary`, `active`, or `draining`; `roles` reflects enabled node services. Until P2,
  `cn` nodes advertise only `stun` and enforce P0's relay shutdown locally.
- `public_stun` holds standalone ICE endpoint records (URL, region, provider, failure domain);
  public STUN services need no relay identity or community enrollment.
- `core/nodedirectory` (new, network-free): parse, verify the root → online key → payload chain, and
  check unique aliases and endpoint identities. Extract shared pure relay/STUN validation into core;
  relay protocol and connectivity packages consume it without reversing the core dependency boundary.
- Signing: the source lives in `deploy/network/`. The operator signs locally with the online key
  and publishes to the mirrors; the repository keeps the signed export as history. P3 moves
  signing into the console.
- Mirrors: `/.well-known/windshare-network` on `s.<filed-domain>` and `windshare.link`, plus a
  GitHub release asset; from P3, every node.
- `engine/builtinnetwork` (new, imported only by applications such as `cmd/wind` and the desktop
  app): embedded snapshot, pinned root keys, mirrors, and the built-in frontends (link host and
  backup entry), injected through `engine.Config`. The web build uses the same snapshot and
  embedded public STUN fallbacks.
- `engine/networkdirectory` (new): load the snapshot, refresh from mirrors in random order with
  backoff, and persist the highest accepted generation and its highest serial under
  `config.json.state/network/`.
- Connection observations: actual registration attempts record TCP/TLS/WebSocket/admission outcomes
  and timings. Keep recent results in memory per network generation with an age limit, for ordering
  only. Reuse registration timeouts and backoff; no separate probe scheduler or persistent probe store.
  Optional coarse exit-location hints seed cold-start order without excluding regions or delaying sharing.
- `connectivity/relayplan` (new, pure): configured candidates, directory nodes, connection observations,
  and policy in; an ordered plan and a decision record out.
  1. Configured active relays come first, up to the protocol limit.
  2. Unless the user chose configured relays only, candidates are `active` and `canary` nodes with the
     `relay` role, allowed by admission policy and not in cooldown.
  3. Order by recent connection results, P0 priors, capacity, and canary status; retain an unmeasured
     candidate when available. Low RTT alone does not imply high throughput.
  4. Prefer another provider and operator. With a `cn` primary, prefer a `global` backup. With a
     `global` primary, prefer another `global` when P0 measurements support mainland first-join
     reachability through at least one of the pair; otherwise prefer a usable `cn` backup.
     Reserve candidate slots for these preferences before racing.
  5. If the preferred backup is unavailable, use the best remaining candidate; one usable node
     can serve alone.

  Zone preferences follow the admitted primary, not the sender's presumed location. Registration
  advances through candidates with at most four network attempts in flight, within the protocol limit.
  The first admission makes the link ready with a frozen set of at most four network candidates.
  Target two network registrations under the diversity policy; later fallback stays within the
  published set. Refactor `relayset` to park surplus candidates while retaining recovery material,
  without sending `STOP` or disrupting admitted sessions. Permanent share shutdown stops every
  attempted registration. Dormant candidates provide sender fallback, not extra live receiver
  entrypoints. Receivers stop routine retries of unavailable candidates while current paths meet
  demand, revisiting them for recovery or measured throughput needs. Explicit relays can serve alone.
  Changes to the sender's active count or wake-up behavior require measured first-join coverage failures.
- Frontend: an explicit frontend (`front_url`, `--front-url`), else the built-in link host, else the
  backup entry when the link host is known unreachable from the sender. Bounded frontend checks on
  network changes never delay sharing and cannot detect receiver-only blocking. An optional "Copy
  backup link" action reuses the share on the built-in backup host; switching hosts does not move
  browser download state.
- Engine API: `ShareRequest` carries a relay intent, either explicit (`--relay`, the complete set as
  today) or planned, plus an optional explicit frontend. `ShareReady` reports published candidates,
  current admissions, frontend, and the decision. The CLI no longer calls `SelectRelays` or requires
  a frontend.
- `engine/relayconfig`: configured relays (profiles, `active_relays`), a configured-relays-only switch,
  and the `front_url` override. The application injects the network through `engine.Config`.
  Initialization and refresh preserve explicit choices, including configured-only mode and disabled defaults.
- STUN: merge and deduplicate embedded public fallbacks, valid directory node/public STUN entries,
  and, from P4, accepted community descriptors. Senders and receivers independently use P0's
  selection policy; directory unavailability does not remove embedded public fallbacks.
- Links: official nodes use repeated `n` values naming directory aliases; explicit endpoints use `r`,
  omitting the scheme for HTTPS roots (`core/link`, `web/src/crypto/suite02-link.ts`). Aliases are never
  reassigned and resolve to canonical endpoints, not new relay identities. CLI and browser resolve from
  trusted embedded/cached directories, including draining nodes for existing links. Unknown aliases
  trigger one bounded refresh (same-origin mirror first in browsers), without delaying known endpoints.
  Parsing stays network-free; engine/web application layers resolve. Use the link origin only when
  neither `n` nor `r` is present.
- Traces: planner decisions (candidates, timings, filters, admissions and reasons, published set,
  frontend), directory refreshes (mirror, generation, serial, verification and expiry), and registration
  attempts (attempt ID, stage, outcome, timeout/backoff).

### P2 — Authority, accounts, and fair relaying

Account admission and fair relaying enable mainland service and public promotion.

- Authority: a new Cloudflare Worker host (`relay-rust/crates/network-authority`, Durable Object
  SQLite, reusing `relay-access`) at `authority.windshare.link`. Clients and nodes always call
  `api.<filed-domain>` (see [Hosts and domains](#4-hosts-and-domains)), because `workers.dev` is
  unreachable from mainland China.
- The first deployment prints a one-time initialization link: open it, set the administrator's
  username and password, and land signed in. The link expires once used.
- Keys: a root key certifies each online signing generation for a bounded period. Rotation and
  leak recovery follow P1's generation and cache rules.
- Node identity: `wsrelay` generates an Ed25519 node key on first start, and the node ID derives from
  it. Discovery v2 at `/.well-known/windshare` publishes a self-signed descriptor: key, relay
  endpoint, enabled services, STUN, zone, region, capacity, classes, version, and expiry. P4 adds
  contribution state. Directory entries gain the node key.
- A node configuration file replaces the growing flag list: enabled services, endpoint, origins,
  capacity, P0's optional monthly quota and reset day, node key, authority URL, class policy, and
  trusted issuers. P4 adds contribution budgets.
- Register node keys with the authority manually and keep P1's local directory signing. Existing
  TLS ingress, certificate renewal, health checks, and the install helper remain the deployment path.
- Accounts on the authority: start with automatic registration (users may set a username and
  password). The console can switch to proof-of-work (about 15 seconds of CPU, with progress) or
  email verification and set PoW difficulty. The 1,000 and 4,000 account milestones prompt operator
  review only; counts do not switch modes automatically.
- Registration admission: per-source and global creation-rate limits with generous burst allowance
  and automatic recovery. Repeated source bursts may require PoW or email verification. These
  controls apply to new account creation, independently of existing accounts and transfers.
- Engine account component: start account setup on first use of official service, show PoW progress,
  and retain selected files through authorization. Reuse saved credentials and refresh tickets in
  the background. New shares use an available valid ticket for official nodes; anonymous admission
  is a fallback when none is available. Usable authorized or permitted anonymous paths need not wait
  for the account service; otherwise surface authorization/retry. Login applies to new shares;
  existing anonymous registrations retain their attribution for that share's lifetime.
  `senderrelay.Candidate`'s static `AccessKey` becomes a registration-authorization source, either a
  static key or a ticket provider, defined as an interface in `senderrelay`.
- Ticket: compact signed CBOR with issuer key ID, account, audience (`official`), expiry, and an
  initial entitlement snapshot. Identity lasts hours; the snapshot uses the usage loop's independent
  version, accounting day, and `valid_until`. It travels like today's `AccessKey`, as a bearer token
  on fresh registrations; RESUME keeps the attribution. Clients present official tickets only to
  current official nodes. P2 limits ticket misuse through expiry, revocation, and node budgets;
  stronger ticket binding is deferred.
- `relay-access`: offline ticket verification and an attribution enum of owner, account, and
  anonymous; trusted issuer keys come from node configuration. Initially, self-hosted owners get the
  owner class through file-backed credentials that `wsrelay` prints on first start; P4 adds browser authorization.
- Native host: pass the upgrade's `Authorization` to admission and resolve owner credential, ticket,
  or anonymous under the node's policy.
- Admission: enforce per-account resident-share limits locally alongside node/source connection limits.
  Preserve owner admission capacity under public load; reconnecting the same share reuses its slot.
- Egress governor, node-wide across share actors. This is the largest change in the phase, since
  `wsrelay` runs one actor per share.
  - Configured capacity and classes owner > standard > lowest; lowest includes anonymous senders,
    accounts out of daily traffic, and accounts without fresh entitlement state. Deprioritization
    preserves account identity.
  - Work-conserving within node budgets, so idle capacity goes to anyone.
  - Fair per account and then per session within a class, so extra shares or receivers add no
    bandwidth. Entitlements are independent of account age; source limits protect admission rather
    than define bandwidth shares. Account fairness does not guarantee one allowance per person.
  - A small per-session scheduling floor is best-effort within account and node limits; opaque frames
    cannot be classified as signaling versus file data. Bound queueing and coordinate backpressure
    with handshake, send, and content-progress timeouts. Reuse existing recovery states and backoff
    so throttled tasks retain downloaded progress and resume automatically when capacity returns.
  - Reuse P0's node relay budget across all classes. Other live paths continue after exhaustion.
    Distinguish account deprioritization from node unavailability in the UI; retain receiver recovery
    state if no usable path remains.
- Usage loop: official nodes send signed per-account byte batches every minute with stable IDs for
  idempotent retries across restarts. Charge usage to its UTC day, splitting intervals at midnight;
  late reports update that day rather than today's allowance. Replies carry entitlements with
  monotonic per-account versions, accounting day, and `valid_until`; nodes ignore older versions.
  Entitlements apply only to their accounting day and tolerate several missed reports before expiry.
  Stale state falls back to lowest while authorized transfers continue within node budgets.
  Old tickets and reconnects cannot refresh entitlement validity.
  Revocation is a separate access decision, never cleared by timeouts or daily resets.
  Include P0's node-load aggregates without account/share identifiers alongside usage records;
  collection and upload never block transfers.
- Reports: the receiver offers a report link for manual review. The console can disable a share by
  ShareID or block an account from official relaying. Distribute blocks to web entrypoints and relays,
  enforcing them locally on registration, join, resume and active relay sessions, without a
  per-transfer authority query or directory blocklist.
  This does not delete users' files or stop independent P2P/self-hosted transfers.
- Anonymous class: lowest class, refused first under pressure, with per-source limits. A per-node
  console switch, off by default on `cn` nodes and on for `global` nodes. Enable mainland relay
  service with account admission, then publish its `relay` role.
- Traces: registration decisions (attempt ID, source, mode, reason), node enrollment, ticket
  issuance/refusals, share/account blocks, admission classes, governor/budget transitions, and usage
  batches with versions and expiry, keyed by node, share, and account.

### P3 — Console onboarding and fleet automation

Adding a server takes one command and one approval.

- `wsrelay` gains built-in ACME, including Let's Encrypt IP certificates for hosts without a domain,
  so an overseas or community node is one binary plus one config file. IP certificates last 160 hours:
  follow ACME Renewal Information (ARI), with renewal at half-life as fallback. Persist certificates
  and retry renewals with backoff; keep serving with the current valid certificate and reload replacements
  without interrupting sessions. Expose expiry and renewal failures in node health. Mainland servers
  keep the reverse proxy, which also serves the frontend on port 443.
- Join flow: the console's "Add node" takes name, zone, region, and capacity and returns a one-line
  install command with a one-time join token. The node installs, generates its key, obtains a
  certificate, and binds with the token. Once vantage-point probes succeed it enters as `canary`,
  and it becomes `active` after a healthy period. The console creates DNS records through the
  provider API: Cloudflare for `windshare.link`, DNSPod or Aliyun for the filed domain. A new
  mainland provider also needs filing access.
- Directory signing moves into the authority: every console change publishes a new serial, and
  frontends and nodes serve the latest signed directory from the authority with caching. The
  repository keeps an exported copy for history, never as a gate.
- Health: CN-A, CN-B, and one overseas node probe enabled services (TCP, WebSocket upgrade with
  each official Origin for relays, STUN binding for STUN hosts) and report to the authority;
  status changes publish as new serials. Nodes do not probe each other.
- Removing marks a node `draining` until its live shares end, then deletes it. Updating walks
  official nodes one at a time through the existing install helper (verify, restart, health check,
  rollback), draining each first.
- Traces: join steps keyed by node ID and token, probe results per vantage point, and directory
  publications (generation, serial, change, mirrors).

### P4 — Community nodes

Volunteers join by running a node, and discovery works without the official operator.

- Inclusion needs no approval: a node publishes its self-signed descriptor and announces itself to a
  few bootstrap nodes, official ones by default. Nodes and clients exchange bounded descriptor
  samples and cache signature-verified descriptors. Self-signatures establish identity, not service
  trust; apply local endpoint policy before enabling advertised STUN. Operator and capacity fields
  remain unverified hints.
- Recommendation affects ranking, not admission. Use P1's bounded registration attempts for community
  candidates too; without reachable recommended nodes, community nodes can serve alone or together.
  Prefer distinct operators and providers without requiring them. Explicit self-hosted relays work
  independently, and users can turn community nodes off.
- Native owner onboarding: initialize admin credentials through a one-time link and sign in.
  Address-only setup uses browser authorization to claim and save the owner key; reuse `relay-access`
  and `engine/relaysetup`. Public admission and owner authorization are independent; manual keys still work.
- Community template: contribution on by default with conservative automatic budgets (daily public
  traffic, share of peak bandwidth, relay connections), editable by the owner. Owner traffic comes
  first and may push community traffic to zero. Reaching the daily public traffic cap stops public
  forwarding. The anonymous class is on; nothing depends on the official authority.
- Cloudflare Workers relays contribute only when their owner enables it, with budgets that count
  platform request limits, not just bytes.
- Eligibility stays soft (中转服务.md): community nodes are offered when the user owns no native
  relay, owns at least one contributing native relay, or owns only Cloudflare relays. Configuring
  access to another person's relay does not establish ownership. Turning off contribution on the
  last contributing native relay shows a one-time notice and pauses new community requests;
  owner saturation, maintenance, or a spent budget does not count as refusal. Nothing else mentions contribution.
- Every node mirrors the signed directory and its descriptor cache.
- Traces: descriptor exchange (source, accepted or rejected, reason) and community picks in planner
  decisions.

### P5 — Priority time and redeem codes

Accounts get priority under contention, charged only while received.

- A priority class between owner and standard: accounts with fresh entitlements and both daily
  traffic and priority time remaining are served ahead under congestion.
- Nodes record the intervals when an account was actually served ahead of waiting traffic and add
  them to usage batches. The authority merges intervals per account across nodes and debits the
  union from the daily allowance, 10 minutes by default; idle time is never charged. A "priority
  exhausted" state returns otherwise eligible accounts to standard.
- SuperShare codes are generated in the console and redeemed online to activate
  [cosmetic features](../../clarifications/super-share.md), independently of resource allowances.
- Traces: priority intervals per node and merged debits per account.

### Data-triggered work

Use node summaries for relay load. Confirm join/P2P failures and path byte totals through client
diagnostic exports and repeatable network tests, including failed first joins. TURN benefits require
comparison measurements; relay usage is not a P2P failure rate.

| Work | Trigger | Shape |
|---|---|---|
| Stronger multi-account controls | Persistent multi-account contention harms legitimate transfers despite registration limits | Evaluate targeted source aggregation against shared-exit false positives |
| Mainland static hosting isolation | Frontend load or relay-IP outages justify separation | Object storage + domestic CDN on the same link hostname; publish the same build as Cloudflare and configure separate cost limits |
| Hong Kong optimized line (CN2 GIA or CMI) | Cross-border legs limit relayed transfers at evening peak | A `global` node that mainland networks reach well |
| Receiver-suggested relays | Joined transfers are limited by the link relays' distance or capacity | Over an existing E2E path, ask the sender to publish on an allowed node near the receiver; extend P1's bounded membership beyond the published set for that receiver. Cannot repair first-join failure; not required for launch |
| Cross-border quality in planning | Vantage-point data shows cross-border quality differs widely between nodes | The planner weighs measured cross-border quality, not only RTT from the sender |
| Official TURN | Measured transfers where WebRTC over TURN would beat the application relays | coturn on purchased nodes; the authority issues short-lived credentials to ticket holders, delegated through the existing `TURN_CONTROL` `Grant` |
| Relay-to-relay forwarding | P2P fails and no relay is reachable by both sides | Directed forwarding for that case only |

## 7. Risks and open inputs

| Item | Note |
|---|---|
| ICP filing | Links and mainland nodes depend on it. Confirm that the filing's subject and site purpose allow file sharing, since personal filings restrict site content. Each mainland provider needs filing access. |
| Mainland reach of Cloudflare | The `api.` proxy crosses the border, and mainland users on foreign DNS resolvers land on Cloudflare. Tickets that last hours absorb short gaps. |
| In-app browsers | WeChat and QQ may block saving or WebRTC; P0 finds out. |
| Link-host outage | A backup serves reissued links; node-directory updates cannot repair URLs already sent or migrate browser storage. CDN isolation does not fix domain blocking. |
| Metered plans and cost | Set P0 relay budgets before overage-billed relaying. Configure provider cost controls separately for all host/CDN traffic; relay bytes are not the full bill. |
| Authority on Workers free | 100,000 Worker requests/day and 100,000 DO rows written/day. Minute batches use about 1,440 requests/node/day; writes scale with active accounts across nodes, indexes, and retry bookkeeping. Measure P2 batch costs and upgrade before either limit; failed writes can block entitlement refresh. |
| Root key custody | Losing both root keys means shipping a release with new pinned keys. |
| Community abuse | Black holes, metadata collection, and Sybil nodes: registration results, recommendation weight, operator diversity, multiple relays per share, and user opt-out. |
