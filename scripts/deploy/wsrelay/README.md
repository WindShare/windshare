# wsrelay deployment

`wsrelay` is the Rust native relay. It serves encrypted application traffic and
independent UDP STUN, with durable ownership and STOP history in SQLite.
Keep the database and its WAL together, and run only one host per database.
The former Go server and its bbolt state are retired.

Build the container from the repository root:

```sh
docker build -f deploy/wsrelay/Dockerfile -t windshare-wsrelay .
docker run --rm -p 8484:8484/tcp -p 3478:3478/udp \
  -v windshare-relay-state:/state \
  -v /absolute/path/node.json:/state/node.json:ro windshare-wsrelay
```

Copy [node.example.json](../../../deploy/wsrelay/node.example.json) and set its
endpoint, origins and capacity before starting. The container runs
`wsrelay --config /state/node.json`; relative identity and SQLite paths stay in the volume.
The state volume must be writable by UID 65532. Terminate TLS at a reverse proxy.
The public endpoint must be the exact canonical WebSocket URL used by clients;
browser origins require an explicit allowlist. For a local trusted tunnel, add
`"proxy":{"peers":["127.0.0.1","::1"],"header":"CF-Connecting-IP"}`.
See the [native host](../../../relay-rust/crates/relay-native/README.md) for limits,
source policy and recovery, and [STUN](../../../docs/stun.md) for UDP/admin settings.

If correcting an old wildcard endpoint reports `database belongs to a different
public endpoint`, stop the host and use a new configuration `database` path or Docker volume.
Retain the old database and WAL together; their ownership and STOP history belong
to the old endpoint identity.

To extract a static Linux/amd64 executable on Windows, install Docker with Linux
containers, then run:

```powershell
./scripts/deploy/wsrelay/build.ps1
$manifest = Get-Content dist/deploy/wsrelay/manifest.json | ConvertFrom-Json
```

The builder uses the same Dockerfile, writes the binary and provenance manifest
to `dist/deploy/wsrelay/`, and records changes in the Rust workspace and builder.
Add `-RunTests` to run the local `make rust` gate first.

For an already configured systemd host, install the helper once:

```sh
sudo install -o root -g root -m 0755 scripts/deploy/wsrelay/install.sh \
  /usr/local/sbin/windshare-install-wsrelay
```

Configure `windshare-relay.service` to run `/usr/local/bin/wsrelay` with the Rust
`--config /etc/windshare-relay/node.json`, listening on `127.0.0.1:8484`. Upload `wsrelay-linux-amd64`, then:

```sh
sudo /usr/local/sbin/windshare-install-wsrelay \
  --artifact /tmp/wsrelay-linux-amd64 \
  --sha256 "<manifest sha256>" --revision "<manifest revision>"
```

The helper verifies the executable, restarts the service, checks relay health and
restores the previous binary on failure. It preserves service configuration and
storage. The deployment record is `/etc/windshare-relay/deployment.env`.
