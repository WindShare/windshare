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

To upgrade a standalone `wsrelay` binary without Docker, validate the existing
configuration with the new executable, replace the binary atomically, and restart
your service:

```sh
./wsrelay --check-config /path/to/node.json
sudo install -m 0755 ./wsrelay /usr/local/bin/wsrelay.next
sudo mv -f /usr/local/bin/wsrelay.next /usr/local/bin/wsrelay
sudo systemctl restart <your-service>
```
