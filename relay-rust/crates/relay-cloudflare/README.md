# Cloudflare relay lifecycle host

This Worker routes `/v2/ws?share_id=...` to one SQLite Durable Object per share.
The object binds its stored endpoint and share to that route, rejects mismatched
upgrades without evicting healthy connections, and passes real
REGISTER, RESUME, STOP and heartbeat controls to `ShareRelay`. JOIN and payload
forwarding return `UnsupportedMode`; this is not yet a download relay.

Fresh REGISTER requires `Authorization: Bearer <REGISTRATION_SECRET>` on the
upgrade. The edge overwrites its internal permission header and stores permission
as a socket tag. Missing credentials never grant registration. Signed STOP and
token-bound RESUME do not require this deployment credential.

SQLite stores endpoint identity, a never-reused connection allocator, authority
and the checkpoint manifest. Binary WebSocket attachments retain handshake state,
capacity, heartbeat nonce and original deadlines. Heartbeats update attachments,
not SQL delivery counters. Alarms only move earlier; each event expires overdue
state before processing new controls. Shared `authority` transitions and the
Cloudflare output gate keep acknowledgements behind publication and permanent STOP.

A close callback reports communication loss, never physical buffer release.
This versioned lifecycle schema has never admitted payloads: after losing every
socket it can establish zero delivery debt from that restriction. A future data
host must replace that recovery assumption explicitly. Unreadable authority or
inventory closes connections and retries authoritative recovery; a lost individual
attachment is isolated by the kernel. No descriptor or payload appears in traces.

From this directory, with the Rust Wasm target and `worker-build` installed:

```sh
npm ci
npm run build
npx wrangler secret put REGISTRATION_SECRET
npx wrangler deploy
```

Use the stable deployment URL ending in `/v2/ws`. Endpoint identity is retained in
storage; an alternate hostname is a different relay identity, even for the same
Worker. Do not delete storage to update code: that would erase permanent STOP.

## Validation

`make rust` includes native fault-injected host tests and Wasm compilation/Clippy.
The shared Go codec client is at `relay-rust/tests/_goclient`.
[Recorded Preview evidence](tests/preview-evidence.md) covers live signatures,
normal hibernation, fault recovery, permanent STOP and Alarm expiry.

For an isolated real-cloud run, follow the [Preview runbook](../../../docs/skills/cloudflare-worker-previews-skill.md).
Create a temporary JSON secrets file containing `REGISTRATION_SECRET` and a separate
`PREVIEW_TEST_SECRET`, then run:

```sh
npx wrangler preview tests/entry.mjs --name <run-name> --secrets-file <temporary-json> --json
```

The entry override wraps the production Rust host with authenticated status and
object-abort controls. It does not implement protocol behavior and is absent from
the normal deployment entry. From the repository root, set
`WINDSHARE_RELAY_REGISTRATION_SECRET` and `WINDSHARE_RELAY_PREVIEW_SECRET` in the
environment, then run:

```sh
go run ./relay-rust/tests/_goclient -preview -endpoint wss://<stable-preview-host>/v2/ws
```

The client signs fresh challenges, rejects invalid signatures and route mismatches,
observes constructor re-entry with the same open socket/attachment, rejects an
alternate endpoint while preserving that socket before and after restore, resumes after
an actual object abort, verifies permanent STOP after another abort, and waits for
Alarm-driven handshake expiry. It keeps its hibernation interval below the original
silence deadline. Delete only the run's Preview after retaining evidence.
