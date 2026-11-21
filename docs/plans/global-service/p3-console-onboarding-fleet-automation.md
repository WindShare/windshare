# P3 — Console onboarding and fleet automation

Part of the [Global Network Plan](global-network.md#6-phases).

Enroll official nodes from the console without repository changes, and operate the fleet while
keeping frontends, E2EE keys, DNS credentials, and host access strictly outside the authority.
[Community discovery](global-network.md#p4--community-nodes) and per-node directory mirroring
follow in P4.

## Enroll and publish

- Reuse node enrollment and signed reports. **Add node** records alias, zone, region, provider,
  roles, and capacity, prefilling `node_key` and node settings by having the stateless authority
  edge Worker—after verifying the admin session and CSRF against the DO, outside Durable Object
  transactions, and without loosening console CSP (removing the legacy `DIRECTORY_ORIGIN` exception
  once mirror checks move server-side)—fetch and verify `/.well-known/windshare` v2 over a validated
  public HTTPS URL with redirects refused. Report locally enforced metered `relay_budget`
  (read-only) over the authenticated signed node-to-authority status loop rather than exposing
  billing limits in the public discovery descriptor. For fresh installs on hosts with DNS and TLS
  ingress in place, the console can render a copyable node setup command with the public authority
  configuration (`url`, `issuer`, and certificate); asymmetric node keys and one operator approval
  bind enrollment without a stateful join-token protocol.
- Before listing a node, verify it from its intended zone. Go vantage agents claim work through
  outbound polls to `/v1/vantage/*` about every 60 s, using enrolled probe-only Ed25519 keys separate from
  `wsrelay` identities and frontend TLS/ACME credentials. Reuse the client `relayv2`/STUN/TLS stack.
  Check health, the signed descriptor and TLS for every node. Test STUN binding for the `stun`
  role. For `relay`, register, join and run a synthetic relay-only transfer with each official
  `Origin` and a non-official HTTPS `Origin` to cover self-hosted frontends. Bind the internal
  `probe_account`'s short-lived ticket to the target `node_key` and synthetic `share_id` and
  verify both locally on the normal registration path. Accept probe activity in signed node reports
  and exclude it from user counts, account indexing, billing and permanent share attribution.
  An operator promotes a verified, unlisted `staging` node to `canary` or `active`. For primary
  relay selection, `active` ranks ahead of `canary` when registration history is equal.
- Make the authority the sole online directory publisher. Before its first publish or renewal,
  import the latest verified release snapshot in full, including its cursor, nodes and `public_stun`.
  Merge existing enrollment policies by `node_key` and restore permanent alias bindings from
  verified release history. Keep offline-root authorization, atomic serial advancement on change
  or renewal, and shared canonical test vectors across Go, TypeScript, and Rust. Drive
  automatic payload renewal, KV publish reconciliation after DO SQLite commits, and silent-vantage
  detection with a Durable Object alarm/scheduled pass. Official frontends (`ws.ourworld.top` and
  `windshare.cc`) mirror the latest verified snapshot independently of frontend releases—written
  by the authority to a shared Cloudflare KV namespace on publish and served by the frontend Worker
  with a 60 s KV edge cache TTL (`cacheTtl: 60`) and bundled-asset fallback (keeping read traffic
  off the authority DO), and pulled by mainland hosts from a public KV-backed directory endpoint on
  the authority edge Worker, verified, and persisted outside `/srv/windshare/current` (updating
  mainland ingress templates to serve that path with a 60 s cache TTL and release-bundle fallback)—
  continuing to serve their last verified copy within its trust window during outages and replacing
  deploy-time `DIRECTORY_PUBLICATION` coupling. Show the signed revision in the console, check
  primary mirror lag or failure before promoting traffic (checking Cloudflare mirrors directly and
  mainland `ws.ourworld.top` via CN vantage reports because of split-line DNS), alert before online
  directory or ticket signing certificates expire, and export snapshots to Git for history and
  embedded client releases.
- Keep first-admission link publication intact. Receivers try explicit endpoints immediately and
  emit locally resolved endpoints as soon as the local directory is verified, without stalling
  known aliases behind a network refresh. Missing aliases pass their target aliases into one
  shared background refresh (up to three mirrors, six seconds total), continuing past valid
  snapshots that still lack a requested alias without letting an in-flight untargeted startup
  refresh short-circuit on a stale mirror or letting nonexistent aliases trigger global directory
  failure backoff. During untargeted refreshes, try live primary mirrors (`ws.ourworld.top`,
  `windshare.cc`) before cold release-asset fallbacks (GitHub) so clients do not short-circuit on
  stale release snapshots between client releases.

## Operate the fleet

- Limit fleet probes to CN-A, CN-B, and one overseas vantage point at a bounded interval (5–10
  minutes), writing one aggregated state record per vantage point per cycle to stay within
  Workers/DO free-tier budgets. Check TCP and certificate expiry on every node, WebSocket with
  official and public HTTPS `Origin` headers on configured relays, and binding on configured STUN
  nodes. CN vantage points also check the mainland `ws.ourworld.top` mirror revision. Report results
  to the authority for console visibility and expiry alerts. Derive directory entries and node policies
  from operator intent (`staging`, `canary`, `active`, `draining`, `disabled`) and independent health
  overrides for `relay` and `stun`. Update Go and TypeScript `icepolicy` so
  `status: draining` only drains `relay` while STUN follows the `stun` role, omitting a STUN-only
  node when it has no remaining healthy role so `roles` never becomes empty. Withdraw a failing
  service automatically only on sustained failures across all reporting applicable vantage points:
  move a failing `relay` to `draining` (keeping its alias resolvable) and drop a failing `stun`
  role without taking down the sibling healthy service. Treat silent vantage points as probe faults
  rather than node failures, never auto-withdraw the last healthy `active` node per `(zone,
  service)` or during a fleet-wide fault burst, surface single-vantage or cross-border jitter as
  warnings without churning the directory, and require sustained recovery or explicit operator
  confirmation to clear an automatic health override (never overriding an explicit operator
  `draining` or `disabled` state).
- Replace binary node pause (`enabled: bool`) with `NodePolicy` admission states (`accepting`,
  `draining`, `disabled`) and the designated `probe_account`. Cut the authority and nodes over
  together. In a one-time SQLite transaction, migrate stored policies in `access_authorities`
  from `true` to `accepting` and `false` to `disabled`, preserving each issuer's highest trusted
  signing generation, policy version and anonymous-sharing setting. A draining node stays resolvable
  for existing links and is excluded from new relay selection. It advertises `relay_available: false`,
  suppresses browser device-login advertisement and returns retryable `AccessUnavailable` for ordinary
  fresh registrations so clients refresh the directory in the background. It admits scoped probes
  and serves joins, reconnects (`RESUME`) and forwarding for existing shares. Moving to `disabled`
  closes live sessions. In `wsrelay`, release residency when live attribution ends, including on `STOP`.
  Report `applied_policy_version`, the locally enforced `relay_budget` and total resident shares
  across all attributions, including idle shares and those in reconnect grace. Drain waits for policy
  confirmation. Removal waits for the node to confirm zero resident shares and retains the historical
  alias binding. Early cutoff requires operator confirmation.
- Keep host upgrade credentials and root execution strictly outside the authority Worker so an
  authority compromise cannot reach frontend hosts (`ws.ourworld.top`) or weaken E2EE isolation.
  Move the official node upgrade helper to `scripts/deploy/network/install-wsrelay.sh` for the
  official `/opt/windshare-network` and `windshare-network.service` layout. Upgrade one node at a time over SSH from GitHub Actions or
  an operator script. Scope a dedicated env-configured credential to maintenance drain/undrain, status
  reads and probe ticket issuance. Stage and preflight the binary while the current release serves
  traffic. Request a maintenance drain and wait for the node to confirm `applied_policy_version`.
  Just before restart, the node atomically switches to probe-only admission if it has zero active
  relay sessions and no pending joins. If still busy at the drain deadline, release this operation's
  drain and defer. Keep the local probe-only window across restart and rollback, preserving the
  endpoint, key and SQLite state for idle shares. Verify the candidate directly from the runner
  using the same role-specific enrollment checks and roll back on failure. Use a short maintenance
  retry delay and budget verification, rollback and client redial within the remaining reconnect
  grace (up to 60 s). After the upgrade or rollback is verified, release only this operation's hold
  and apply the latest operator intent and health overrides.
- Trace enrollment, verification, directory publication (generation, serial, changes, mirror
  checks), probe outcomes, and drain or maintenance transitions by `operation_id` and `node_id`.

## Provisioning boundaries

- Keep stable hostnames with manual DNS records and the
  [existing ingress and renewal tooling](../../../deploy/network/operations.md#certificate-renewal).
  Do not store DNS write credentials for `ourworld.top` or `windshare.cc` in the authority.
- Mainland hosts keep the reverse proxy, which also serves the frontend on port 443. New mainland
  hosting providers need filing access.
- Defer direct TLS / built-in ACME in `wsrelay` (including 160-hour IP certificates for domainless
  hosts, ARI renewal, and zero-downtime reload) to P4 community nodes, where it can be evaluated
  against a scripted Caddy setup.
