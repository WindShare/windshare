# Official network operations

The link host is **ws.ourworld.top**; **windshare.cc** is the backup entry.
Use an independent frontend Worker and the same web build on mainland servers.
Keep account IDs, host inventories, and deployment status in local `*_gitignore`
records. Private keys and API credentials stay outside the checkout.

## Provisioning

1. Keep `ourworld.top` on split-line DNS. Mainland answers for `ws.ourworld.top`
   point to CN-A/CN-B; overseas answers CNAME to the SaaS fallback in `windshare.cc`.
   The fallback is a proxied AAAA `100::` record, without an origin server.
   Validate the `ws.ourworld.top` custom hostname and its certificate in Cloudflare
   for SaaS before enabling publication. The deployment retains an exact
   `ws.ourworld.top/*` Worker route. Follow Cloudflare's
   [Worker origin setup](https://developers.cloudflare.com/cloudflare-for-platforms/cloudflare-for-saas/start/advanced-settings/worker-as-origin/).
   Disable automatic RUM injection for this zone so both regions serve identical HTML.
   Provision only the required custom hostnames; budget alerts do not cap usage.
2. Reuse the host's TLS ingress. Import `/etc/caddy/sites/windshare.caddy` on each
   mainland host; `Caddyfile.template` shows the configuration with a DNS-01 certificate.
   Set `WINDSHARE_NODE_HOST` when rendering its node hostname. Alternatively render
   `nginx.conf.template` with `envsubst '${NODE_HOST}'`. Mainland nodes serve both
   virtual hosts; overseas nodes need only the node virtual host.
3. Create a `windshare-network` system user, install each `wsrelay` under
   `/opt/windshare-network/releases/`, and point `current` to the selected release.
   Install `windshare-network.service`. Put `PUBLIC_ENDPOINT=wss://<node>/v2/ws`
   and `STUN_LISTEN=0.0.0.0:3478` in `/etc/windshare-network/node.env`.
   Install `node.cn.json` as `/etc/windshare-network/node.json` on mainland servers.
   Keep unit/config files root-owned with mode 0644 and the binary executable.
   Keep relay service disabled there until P2 account admission is deployed.
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
   `node_settings_changed`, `relay_service_unavailable`, and `node_load_summary` logs.

## Certificate renewal

Install pinned `acme.sh` 3.1.6 in `/opt/windshare-acme/3.1.6`, with private
state in `/var/lib/windshare-acme` (0700). Give a dedicated RAM identity only the
[renewal policy](alidns-renewal-policy.example.json), filling in the account ID,
DNS zone, and renewal server's public source IP (without a `/32` suffix).
Save the filled policy as `alidns-renewal-policy_gitignore.json`. The permission
scope is the DNS zone, not an individual TXT record. Store `Ali_Key` and `Ali_Secret` in
`/etc/windshare-network/credentials/alidns.env` (root-owned, 0600).

Issue with `--server letsencrypt --dns dns_ali --keylength ec-256 -d ws.ourworld.top`,
using the home/config paths above. Install the full chain and key into
`/etc/windshare-network/tls/{fullchain,privkey}.pem` (root:caddy, 0640). Register
`caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile && systemctl reload caddy`
as the `acme.sh --install-cert --reloadcmd`. Install and enable
`windshare-cert-renewal.timer`; it checks daily and follows the CA renewal window.
Exercise DNS issuance, challenge cleanup, Caddy reload, and the scheduled service
before enabling split DNS. Each renewal server needs its source IP authorized.

## Release

Sign the directory as described in [README](README.md). Keep aliases permanently
bound to the original canonical relay endpoint, including removed nodes.

Install `scripts/deploy/network/install-web.sh` on both CN servers as
`/usr/local/sbin/windshare-install-web`. It requires Python 3.12+, web-server read access
to `/srv/windshare`, and a deployment SSH user allowed to invoke this installer via
sudo. It verifies the archive and atomically switches `current`; prior releases
remain available for operator rollback.

Configure GitHub environments `official-cn-a` and `official-cn-b` with `SSH_HOST`,
`SSH_USER`, `SSH_PRIVATE_KEY`, and pinned `SSH_KNOWN_HOSTS`. Configure
`official-cloudflare` with `CLOUDFLARE_API_TOKEN`, `CLOUDFLARE_ACCOUNT_ID`,
`CLOUDFLARE_WORKER_NAME`, and `CLOUDFLARE_ZONE_ID` for `windshare.cc`. The token
needs Worker deployment/routes, Zone Read, and SSL and Certificates Read for the
zone's custom hostnames.
Set `OFFICIAL_NETWORK_ENABLED=true` after provisioning. The root release workflow
builds once and publishes the identical archive to all three destinations. The
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
