# Official network operations

The link host is **ws.ourworld.top**; **windshare.cc** is the backup entry.
Use an independent frontend Worker and the same web build on mainland servers.
Only hosts whose administrators are all trusted may serve the frontend, API ingress, or installers.
Node-only hosts must not receive frontend or API TLS keys, DNS credentials, signing keys, or
authority credentials; their own TLS certificate covers only their node hostname.
Keep account IDs, host inventories, and deployment status in local `*_gitignore`
records. Private keys and API credentials stay outside the checkout.

## Provisioning

1. Keep `ourworld.top` on split-line DNS. Mainland answers for `ws.ourworld.top`
   point only to approved frontend hosts; overseas answers CNAME to the SaaS fallback in `windshare.cc`.
   The fallback is a proxied AAAA `100::` record, without an origin server.
   Validate the `ws.ourworld.top` custom hostname and its certificate in Cloudflare
   for SaaS before enabling publication. The deployment retains an exact
   `ws.ourworld.top/*` Worker route. Follow Cloudflare's
   [Worker origin setup](https://developers.cloudflare.com/cloudflare-for-platforms/cloudflare-for-saas/start/advanced-settings/worker-as-origin/).
   Disable automatic RUM injection for this zone so both regions serve identical HTML.
   Provision only the required custom hostnames; budget alerts do not cap usage.
2. Reuse the host's TLS ingress. Import `/etc/caddy/sites/windshare.caddy` on each
   mainland host; `Caddyfile.template` shows the configuration with a DNS-01 certificate.
   Set `WINDSHARE_NODE_HOST` when rendering its node hostname. On trusted mainland API
   ingress hosts, also import `authority.Caddyfile.template` after DNS-01 issuance.
   Alternatively render `nginx.conf.template` with
   `envsubst '${NODE_HOST} ${FRONTEND_TLS_DIR} ${NODE_TLS_DIR}'` and, for API ingress,
   `nginx-authority.conf.template` with `envsubst '${API_TLS_DIR}'`.
   Each TLS directory contains `fullchain.pem` and `privkey.pem`. Hosts without the
   frontend or API ingress role use `nginx-node.conf.template`, rendering only `${NODE_HOST}`.
3. Create a `windshare-network` system user, install each `wsrelay` under
   `/opt/windshare-network/releases/`, and point `current` to the selected release.
   Install `windshare-network.service` and the complete node configuration at
   `/etc/windshare-network/node.json`. Replace `REPLACE_NODE_HOST` and set
   `node.capacity_bytes_per_second` to measured sustainable egress before enabling relay.
   Use `node.contribution.bytes_per_second` for a separate public traffic cap.
   Preserve the generated identity file across upgrades.
   Keep unit/config files root-owned with mode 0644 and the binary executable.
   Follow [authority enrollment](../../relay-rust/crates/network-authority/README.md)
   before enabling mainland relay service and publishing its relay role.
4. For a metered overseas node, set the purchased byte allowance and UTC reset day
   (1–28) in `node.metered.example.json` before enabling relay service. The example
   is not a purchased quota. Configuration reloads every second; budget storage
   stays in the relay database across restart. A billing day cannot change after
   that database has established a ledger. Reservations use 1 MiB batches and may
   conservatively count unused batches and pending socket writes after a crash.
   Host billing also counts non-relay traffic.
5. Open TCP 443 and, only for STUN nodes, UDP 3478. Keep TCP 8484 on loopback.
   Ingress overwrites `X-Windshare-Source-Ip`; the service trusts only loopback.
   Start `windshare-network.service`; inspect `/.well-known/windshare` and the
   `node_settings_changed`, `relay_pacing_disabled`, `relay_service_unavailable`, and `node_load_summary` logs.

For subsequent binary changes use the [single-node upgrade runner](../../scripts/deploy/wsrelay/README.md#official-node-upgrades).

## Certificate renewal

Install pinned `acme.sh` 3.1.6 in `/opt/windshare-acme/3.1.6`, with private
state in `/var/lib/windshare-acme` (0700). Give a dedicated RAM identity only the
[renewal policy](alidns-renewal-policy.example.json), filling in the account ID,
DNS zone, and renewal server's public source IP (without a `/32` suffix).
Save the filled policy as `alidns-renewal-policy_gitignore.json`. The permission
scope is the DNS zone, not an individual TXT record. Store `Ali_Key` and `Ali_Secret` in
`/etc/windshare-network/credentials/alidns.env` (root-owned, 0600).

Issue each split-line host separately with
`--server letsencrypt --dns dns_ali --keylength ec-256 -d <host>` (`ws.ourworld.top`,
and `api.ourworld.top` on trusted mainland API ingress hosts), using the home/config
paths above. Install each host's full chain and key into
`/etc/windshare-network/tls/<host>/{fullchain,privkey}.pem` (directories `0750`,
files `0640`, `root:caddy`, or the Nginx group when using Nginx) so every DNS-01
target stays under the single `ReadWritePaths` root in `windshare-cert-renewal.service`.
Register `caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile && systemctl reload caddy`
(or `/usr/local/libexec/windshare-reload-nginx` for Nginx) as each host's
`acme.sh --install-cert --reloadcmd`. Install and enable
`windshare-cert-renewal.timer`; it checks daily and follows the CA renewal window.
Exercise DNS issuance, challenge cleanup, ingress reload, and the scheduled service
before enabling split DNS. Each renewal server needs its source IP authorized.

For node-only Nginx hosts, install pinned lego 5.5.2 at `/opt/windshare-acme/lego-5.5.2`.
Keep `/var/lib/windshare-node-acme` root-owned, mode 0700, and its separate
`/var/lib/windshare-node-acme-webroot` readable by Nginx. Bootstrap the HTTP block
from `nginx-node.conf.template` before first issuance, then enable its HTTPS block.
Set `NODE_HOST` and a node-local `ACME_ACCOUNT` in `/etc/windshare-network/node-acme.env`;
install `reload-nginx.sh` as `/usr/local/libexec/windshare-reload-nginx` and the
`windshare-node-cert-renewal` service/timer. The service issues through HTTP-01,
reloads Nginx after issuance, and checks the CA renewal window daily.

When withdrawing a frontend host, remove its DNS record and certificate access first.
Generate a new frontend private key and revoke every certificate using the exposed key;
deleting copies on a host controlled by another administrator is insufficient.

## Release

For each trusted mainland frontend, install a Linux build of `./cmd/network-sign` at
`/opt/windshare-network/network-sign` and run
`install -d -m 0755 -o windshare-network /srv/windshare/directory`. Install
`windshare-directory.service` and `windshare-directory.timer` under `/etc/systemd/system/`,
then run `systemctl daemon-reload && systemctl enable --now windshare-directory.timer`.
The timer and web installer use the same directory lock and atomically select the newest verified signed revision. Install the matching `network-sign` before updating the frontend. The timer checks the authority once per minute.
Both ingress templates serve this file with a 60-second TTL and a release-bundle fallback. If the last accepted revision expires, the timer retires it to `.expired` and ingress returns an uncached 503 until a usable revision arrives.

Set `DIRECTORY_KV_ID` in `official-cloudflare` to the authority's directory namespace.
Export the signed release head for each client release as described in the [release guide](README.md).

Install `scripts/deploy/network/install-web.sh` only on approved frontend hosts as
`/usr/local/sbin/windshare-install-web`. It requires Python 3.12+, web-server read access
to `/srv/windshare`, and a deployment SSH user allowed to invoke this installer via
sudo. It verifies the archive, publishes versioned downloads under
`/srv/windshare/downloads` for retained releases, atomically switches `current`,
and retains recent releases for operator rollback.

Set repository variable `OFFICIAL_FRONTEND_NODES` to the trusted frontend aliases
(JSON array, default `["cn-a"]`; add `cn-b` only when its host meets that trust requirement).
Configure their GitHub environments `official-<alias>` with `SSH_HOST`,
`SSH_USER`, `SSH_PRIVATE_KEY`, and pinned `SSH_KNOWN_HOSTS`. Configure
`official-cloudflare` with `CLOUDFLARE_API_TOKEN`, `CLOUDFLARE_ACCOUNT_ID`,
`CLOUDFLARE_WORKER_NAME`, and `CLOUDFLARE_ZONE_ID` for `windshare.cc`. The token
needs Worker deployment/routes, Zone Read, and SSL and Certificates Read for the
zone's custom hostnames.
Set `OFFICIAL_NETWORK_ENABLED=true` after provisioning. The root release workflow
builds once and publishes the identical archive to Cloudflare and those hosts. The
`Publish official network frontend` workflow can retry a released SHA/version.
Concurrent regional uploads are not globally atomic; rerun a failed destination.

## Network measurements

During each region's evening peak, collect dated CLI diagnostic exports and
`iperf3 --json` TCP/UDP results from CN-A, CN-B, an overseas vantage point, and home
networks in both zones. Record access ISP, endpoints, loss, throughput, registration
success, and first-join success through the final admitted pair. Keep
`global_pair_covers_mainland=false` until that pair has measured coverage.

On real iOS and Android devices, record WeChat/QQ versions and results for preview,
single-file saving, streamed ZIP, and WebRTC. The receiver currently gives those
embedded browsers a system-browser hint; it does not assert an unmeasured support
matrix or block preview. Keep the same link origin when testing resume.
