# wsrelay deployment

This workflow rebuilds the current relay source graph and safely replaces the binary on an already
configured WindShare relay host. It deliberately leaves systemd, Caddy, TLS, state, and origin policy
under the host's existing configuration.

The single `wsrelay` executable also runs the controlled STUN service in the same process.
It defaults to UDP 3478 and private STUN admin HTTP at `127.0.0.1:8081`.
Allow UDP 3478 through the deployment firewall. Existing application relay health and
rollback checks remain independent of STUN availability. See [STUN configuration](../../../docs/stun.md)
for rate limits, health, disable flags, and explicit optional UDP 443 deployment.

Build the same product as a container from the repository root:

```sh
docker build -f deploy/wsrelay/Dockerfile -t windshare-wsrelay .
docker run --rm -p 8484:8484/tcp -p 3478:3478/udp \
  -v windshare-relay-state:/state windshare-wsrelay \
  -state-dir /state -relay-base-url wss://relay.example.com
```

The state volume must be writable by container UID 65532. Terminate TLS at the host
proxy as usual. The STUN admin port is private and is not published by this command.
UDP 443 requires an explicit listener, port publication, available bind privileges,
and no competing UDP service; TCP 443 on the TLS proxy does not supply UDP STUN.

Build from the repository root on Windows:

```powershell
pwsh -NoProfile -File scripts/deploy/wsrelay/build.ps1
$manifest = Get-Content dist/deploy/wsrelay/manifest.json | ConvertFrom-Json
```

The builder runs uncached `relay/...` tests, targets static Linux/amd64, and writes the binary plus a JSON
manifest to `dist/deploy/wsrelay/`. `source_graph_status` is derived from the relay's actual in-repository
dependency graph, so unrelated working-tree files do not make the deployment provenance ambiguous.

Install the remote helper once:

```bash
sudo install -o root -g root -m 0755 scripts/deploy/wsrelay/install.sh \
  /usr/local/sbin/windshare-install-wsrelay
```

For each deployment, upload `wsrelay-linux-amd64`, then pass the exact manifest values:

```bash
sudo /usr/local/sbin/windshare-install-wsrelay \
  --artifact /tmp/wsrelay-linux-amd64 \
  --sha256 "<manifest sha256>" \
  --revision "<manifest revision>"
```

The installer serializes deployments, verifies the artifact before switching, probes the new executable,
restarts `windshare-relay.service`, waits for the loopback health endpoint, and restores
`/usr/local/bin/wsrelay.previous` on failure. The last successful provenance is stored at
`/etc/windshare-relay/deployment.env`.
