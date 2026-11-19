# P2 — Authority, accounts, and fair relaying

Part of the [Global Network Plan](global-network.md#6-phases).

Account admission and fair relaying enable mainland service and public promotion.

- Authority: a new Cloudflare Worker host (`relay-rust/crates/network-authority`, Durable Object
  SQLite, reusing `relay-access`) at `authority.windshare.cc`. Clients and nodes always call
  `api.<filed-domain>` (see [Hosts and domains](global-network.md#4-hosts-and-domains)), because `workers.dev` is
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
- Accounts on the authority: by default the first 1,000 accounts register automatically. The next
  3,000 require proof of work (about 15 seconds of CPU, with progress). Later accounts require email
  verification when mail delivery is configured; otherwise proof of work continues. Operators can
  select a fixed registration mode or return to the account-count policy and set PoW difficulty.
- Registration admission: per-source and global creation-rate limits with generous burst allowance
  and automatic recovery. Repeated source bursts may require PoW or email verification. These
  controls apply to new account creation, independently of existing accounts and transfers.
- Engine account component: start account setup on first use of official service. CLI and desktop
  share local account credentials; coordinate setup and ticket refresh in the engine, outside relay
  handshake deadlines. Reuse valid tickets immediately and refresh in the background. Choose
  admission automatically: new shares use a valid ticket, or permitted anonymous admission while
  setup or refresh continues. Usable paths never wait for the account service; when none is available,
  retain selected files and continue the pending share automatically once authorized. When device
  verification is required, wait for the user to create the account in Settings or with
  `wind account register`. Show estimated progress during that attempt. A share retains its selected
  credential while pending. Parallel official attempts reserve one attribution, account or anonymous.
  Once any registration may have been sent, official candidates reuse that attribution for the share's
  lifetime. If every attempt finishes before sending, the share can select again. Login selects the
  account for new shares.
  `senderrelay.Candidate`'s static `AccessKey` becomes a registration-authorization source, either a
  static key or a ticket provider, defined as an interface in `senderrelay`.
- Authorization outcomes distinguish pending setup, refreshable ticket expiry, temporary service
  failure, and explicit denial. Successful setup/refresh wakes pending registrations without
  recreating the share. Known account blocks never trigger automatic anonymous fallback.
- Ticket: compact signed CBOR with issuer key ID, account, stable credential ID, audience (`official`),
  expiry, and an initial entitlement snapshot. Ticket refresh preserves the credential ID, persisted
  in share attribution. Credential revocation stops ticket issuance, fresh registration, RESUME, and
  active official relay sessions bound to it; other credentials remain valid.
  Identity lasts hours; the snapshot uses the usage loop's independent version, accounting day, and
  `valid_until`. It travels like today's `AccessKey`, as a bearer token on fresh registrations;
  RESUME keeps the attribution. Clients present official tickets only to current official nodes.
  P2 limits ticket misuse through expiry, revocation, and node budgets; stronger ticket binding is deferred.
- `relay-access`: offline ticket verification and attribution for local principals, issuer-scoped
  accounts, and anonymous senders; trusted issuer keys come from node configuration. Preserve the
  existing principal/credential distinction: usage and fairness belong to a stable principal;
  credentials can be revoked independently. Credentials for one account share its principal;
  service keys issued to others have independent principals. Node policy grants owner status and
  derives scheduling class from that status and current entitlements.
  Initially, self-hosted owners use file-backed credentials that `wsrelay` prints on first start;
  P4 adds browser authorization.
- Native host: pass the upgrade's `Authorization` to admission and resolve a local credential, ticket,
  or anonymous sender under the node's policy.
- Admission: enforce per-principal resident-share limits locally alongside node/source connection limits.
  Preserve owner admission capacity under public load; reconnecting the same share reuses its slot.
- Egress governor, node-wide across share actors. This is the largest change in the phase, since
  `wsrelay` runs one actor per share.
  - Configured capacity and classes owner > standard > lowest; lowest includes anonymous senders,
    accounts out of daily traffic, and accounts without fresh entitlement state. Deprioritization
    preserves account identity.
  - Work-conserving within node budgets, so idle capacity goes to anyone.
  - Fair per principal and then per session within a class on each node, so extra shares or receivers
    do not multiply an account's bandwidth share. Entitlements are independent of account age; source
    limits protect admission rather than define bandwidth shares. Account fairness does not guarantee
    one allowance per person.
  - A small per-session scheduling floor is best-effort within account and node limits, with no fixed
    bandwidth reservation or override of owner priority. Opaque frames cannot be classified as
    signaling versus file data. Refactor pending output into bounded per-session queues; schedule
    across sessions while preserving each flow's order and existing flow control. Obtain scheduling
    permission before writer submission and recheck forwarding authority synchronously at submission,
    keeping share actors responsive while waiting for capacity. Coordinate backpressure with handshake, send, and
    content-progress timeouts. Distinguish bandwidth waits from connection failures in existing
    recovery states; preserve real failure detection and use backoff to avoid repeated reconnects
    under sustained congestion. Tasks retain downloaded progress and resume automatically when
    capacity returns, with reconnection when needed.
  - Reuse P0's node relay budget across all classes. Other live paths continue after exhaustion.
    Distinguish spent account allowance, stale entitlement state, and node unavailability in the UI;
    retain receiver recovery state if no usable path remains.
- Usage loop: daily account traffic counts actual relay-forwarded bytes across receivers and nodes;
  P2P file data is excluded. Official nodes send signed per-account byte batches every minute with
  stable IDs for idempotent retries across restarts. Charge usage to its UTC day, splitting intervals
  at midnight; late reports update that day rather than today's allowance. Show the next reset in
  the user's local time. Replies carry entitlements with monotonic per-account versions, accounting
  day, and `valid_until`; nodes ignore older versions. The same loop refreshes entitlements for
  resident or waiting accounts even with zero new bytes; retries of older usage batches must not
  delay current entitlement refreshes.
  Entitlements apply only to their accounting day and tolerate several missed reports before expiry.
  Stale state falls back to lowest while authorized transfers continue within node budgets.
  Old tickets and reconnects cannot refresh entitlement validity.
  Revocation is a separate access decision, never cleared by timeouts or daily resets.
  Include P0's node-load aggregates without account/share identifiers alongside usage records;
  collection and upload never block transfers.
- Reports: the receiver offers a report link for manual review. The console can disable a share by
  ShareID or block an account from official relaying. Distribute share/account blocks to web entrypoints
  and relays; relays also receive credential revocations. Enforce these locally on registration, join,
  resume and active relay sessions, without a per-transfer authority query or directory blocklist.
  This does not delete users' files or stop independent P2P/self-hosted transfers.
- Anonymous class: lowest class, refused first under pressure, with per-source limits. A per-node
  console switch, off by default on `cn` nodes and on for `global` nodes. Enable mainland relay
  service with account admission, then publish its `relay` role.
- Traces: registration decisions (attempt ID, source, mode, reason), node enrollment, ticket
  issuance/refusals, share/account blocks, admission classes, governor/budget transitions, and usage
  batches with versions and expiry, keyed by node, share, and account.
