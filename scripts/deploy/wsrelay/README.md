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

## Official node upgrades

Provision the system user, unit and release symlink using the [official host layout](../../../deploy/network/operations.md#provisioning). Both releases must support the persistent maintenance fence. Install the first fence-capable release during a coordinated node and authority cutover. Keep the node identity and SQLite database in `/var/lib/windshare-network`.

Install the root-owned helper once on the node host.

```sh
sudo install -o root -g root -m 0755 scripts/deploy/wsrelay/install.sh \
  /usr/local/sbin/windshare-install-wsrelay
```

On the operator runner, pin the node's SSH host key and configure its alias for batch access to that helper through `sudo -n`. Keep SSH credentials on the runner. Give the authority and runner the same dedicated `FLEET_MAINTENANCE_TOKEN` (32 to 128 random base64url characters). The token authorizes drain, status, scoped synthetic tickets and release.

```sh
go build -o network-upgrade ./cmd/network-upgrade
./network-upgrade -host "$SSH_ALIAS" -node-key "$NODE_KEY" \
  -artifact ./wsrelay-linux-amd64 -revision "$GIT_COMMIT"
```

The runner stages and checks the candidate as the service account while the old release serves users. It then waits up to five minutes for the signed drain confirmation and an idle transfer window. Exit 75 defers a busy node. A successful upgrade exits zero. A failed candidate exits nonzero and logs whether the previous release was verified. Both switches use the installed key, endpoint and database. The local fence rejects ordinary joins with a one-second retry.

If a runner stops with a hold outstanding, use its logged operation ID. Recovery closes an unentered or already-released operation directly; if the local maintenance hold is active, it checks the selected release, rolls back to the recorded previous binary if needed, and releases the matching hold after verification.

```sh
./network-upgrade -host "$SSH_ALIAS" -node-key "$NODE_KEY" -recover-operation "$OPERATION_ID"
```

When rollback cannot be verified, the fence stays in place. Inspect the journal for `windshare-network.service` and the root-owned release paths in `/opt/windshare-network/operations/$OPERATION_ID/`, repair the host, then repeat recovery.

If the authority hold was already aborted or released separately and the host has been verified, clear the local fence in place as the service user instead of deleting the `.admission` file (which leaves the running process holding the old unlinked descriptor):

```sh
sudo runuser -u windshare-network -- \
  /opt/windshare-network/current/wsrelay --force-release-maintenance \
  /etc/windshare-network/node.json "$NODE_KEY"
```
