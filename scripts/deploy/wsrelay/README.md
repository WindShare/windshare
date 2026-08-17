# wsrelay deployment

This workflow rebuilds the current relay source graph and safely replaces the binary on an already
configured WindShare relay host. It deliberately leaves systemd, Caddy, TLS, state, and origin policy
under the host's existing configuration.

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
