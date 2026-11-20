# P3 — Console onboarding and fleet automation

Part of the [Global Network Plan](global-network.md#6-phases).

Enroll official nodes from the console without repository changes, and operate the fleet without
expanding the authority's security blast radius. [Community discovery](global-network.md#p4--community-nodes)
and per-node directory mirroring follow in P4.

## Enroll and publish

- Reuse node enrollment and signed reports. **Add node** records alias, zone, region, provider,
  roles, capacity, and a traffic budget for metered relays, prefilling `node_key` and node settings
  from the node's verified `/.well-known/windshare` v2 signed descriptor. For fresh installs, the
  console can render a copyable install command with the public authority configuration (`url`,
  `issuer`, and certificate); asymmetric node keys and one operator approval bind enrollment without
  a stateful join-token protocol.
- Before listing a node, run an on-demand verification check from the intended zone: relay
  registration, a synthetic relay-only transfer through that node with official `Origin` headers,
  STUN binding, and TLS certificate validity. Keep pre-publication `staging` (unlisted) distinct
  from the directory's `canary` status (listed at lower selection priority for bounded live
  traffic). The operator explicitly promotes a verified node to `canary` or `active`.
- Make the authority the sole online directory publisher while keeping offline-root authorization,
  permanent alias bindings, and atomic serial advancement on change or renewal (keeping directory
  validation consistent across Go, TypeScript, and Rust). Official frontends (`ws.ourworld.top` and
  `windshare.cc`) mirror the latest verified snapshot independently of frontend releases and serve
  their last verified copy within its trust window during outages, replacing deploy-time
  `DIRECTORY_PUBLICATION` coupling. Show the signed revision in the console, check primary mirror
  lag or failure, and export snapshots to Git for history and embedded client releases.
- Publish the link on first admission once the sender can accept joins. Receivers try explicit
  endpoints immediately and add locally resolved endpoints as soon as the local directory is
  verified, without stalling known aliases behind a network refresh. Missing aliases share one
  background refresh (up to three mirrors, six seconds total), continuing past valid snapshots that
  still lack a requested alias.

## Operate the fleet

- Limit fleet probes to CN-A, CN-B, and one overseas vantage point at a bounded interval (5–10
  minutes) within Workers/DO free-tier budgets. Check TCP, WebSocket with each official `Origin`,
  STUN as applicable, and certificate expiry, then report to the authority for console visibility
  and expiry alerts. Withdraw a service from new selection automatically only on sustained failures
  across all applicable vantage points (keeping its alias resolvable); surface single-vantage or
  cross-border jitter as warnings without churning the directory.
- Replace binary node pause (`enabled: bool`) with explicit policy states (`active`, `draining`,
  `disabled`). Exclude a `draining` node from new selection while keeping its alias resolvable: it
  refuses fresh registrations and continues accepting joins and reconnects for existing shares.
  Report local resident-share counts so removal waits for local confirmation that all shares have
  ended (including idle shares and those in reconnect grace), retaining the alias binding in
  history. Early cutoff requires operator confirmation.
- Keep host upgrade credentials and root execution strictly outside the authority Worker so an
  authority compromise cannot reach frontend hosts (`ws.ourworld.top`) or weaken E2EE isolation.
  Unify `scripts/deploy/wsrelay/install.sh` with the official `/opt/windshare-network` and
  `windshare-network.service` layout. Run rolling upgrades one node at a time from GitHub Actions
  or an operator script: call the authority API to set `draining`, wait within a bounded drain
  window (clearing drain and deferring if active relay traffic persists past the deadline),
  upgrade and verify over SSH with rollback while preserving endpoint, key, and SQLite state so
  idle shares can reconnect after restart, run public service checks, and restore `active`.
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
