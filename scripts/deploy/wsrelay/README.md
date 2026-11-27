# Native community relay

On a Linux host with Docker Compose and Python 3, point a DNS name at the host and run from this checkout:

```sh
sh scripts/deploy/wsrelay/install.sh relay.example.com
```

Open the one-time initialization URL from the relay log, set the console password, then add the relay address in WindShare to pair an owner device. The console controls anonymous contribution, daily traffic, public bandwidth, connections and an optional host uplink estimate. Defaults are 10 GiB/day, 25 Mbps and 64 public connections. Keep the identity file, SQLite database and WAL together in the relay state volume.

Open TCP 443 for HTTPS and WebSockets, UDP 3478 for STUN, and preferably TCP 80 for HTTP-01. A public IP address also works as the installer argument. The pinned Caddy obtains Let's Encrypt's 160-hour `shortlived` IP certificates, uses ACME Renewal Info for automatic renewal and can validate on port 443 using TLS-ALPN-01 when port 80 is blocked. Keep the Caddy data volume across upgrades.

The [Compose bundle](../../../deploy/wsrelay/compose.yaml) uses Linux host networking. Caddy overwrites the source-IP header and proxies to the loopback listener. Edit `deploy/wsrelay/instance/node.json` for services, billing ceilings and untrusted bootstrap URLs. Add independent seeds there as they become available. Rerunning the installer preserves the configuration and volumes. To apply a Caddyfile edit without dropping listeners:

```sh
cd deploy/wsrelay
docker compose --env-file instance/.env exec caddy caddy reload --config /etc/caddy/Caddyfile
```

For standalone deployments, adapt the [example configuration](../../../deploy/wsrelay/node.example.json) and run `wsrelay --check-config /path/to/node.json` before restarting. On Windows, `scripts/deploy/wsrelay/build.ps1` exports a static Linux/amd64 binary and provenance manifest. See [native relay operation](../../../relay-rust/crates/relay-native/README.md) and [STUN](../../../docs/stun.md).
