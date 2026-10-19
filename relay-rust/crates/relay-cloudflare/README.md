# Cloudflare relay host

This Worker routes `/v2/ws?share_id=...` to one SQLite Durable Object per share.
The object binds its stored endpoint and share to that route, rejects mismatched
upgrades without evicting healthy connections, and passes real
REGISTER, RESUME, JOIN, session admission, cumulative consumption, settlement, STOP
and heartbeat controls to `ShareRelay`. Opaque envelopes remain event-owned and pass
through the kernel's synchronous `submit` before WebSocket delivery in either direction.
Descriptors come only from confirmed authority rows. The Go sender accepts saved
[relay configuration](../../../docs/cli.md#saved-relays), including registration credentials.

Fresh REGISTER requires `Authorization: Bearer <REGISTRATION_SECRET>` on the
upgrade. The edge overwrites its internal permission header and stores permission
as a socket tag. Missing credentials never grant registration. Signed STOP and
token-bound RESUME do not require this deployment credential. Each share has 8 business
slots and a separate 8-socket control reserve. STOP needs no business slot; authentication,
control concurrency, attempt-rate and absolute deadlines still apply.

SQLite stores endpoint identity, a never-reused connection allocator, authority
and the checkpoint manifest. Binary WebSocket attachments retain handshake state,
capacity, delivery ledgers, heartbeat nonce and original deadlines. Payloads and feedback
update attachments, not SQL counters. Window replenishment reads kernel occupancy,
so duplicate feedback and hibernation cannot refund submitted debt. Each flow targets
two maximum-sized envelopes within the destination's declared capacity and the host's
16-frame connection budget. Alarms are read once per wake and only move earlier;
each event expires overdue state before processing new input. REGISTER atomically writes ownership and
descriptor in one SQL operation; exact completion and the Cloudflare output gate
keep acknowledgements behind that commit and permanent STOP. Uncertain events require
authoritative recovery before more controls. Recovery retains committed ownership even
when its receipt was lost, entering grace if the owner is gone. Storage schema version 3
rejects older storage.

A close callback reports communication loss, never physical buffer release.
Risk reservations and their absolute expiry survive in the manifest even when all
sockets/attachments disappear. The host selects immediate expiry after revocation
and both application queues are discarded, preserving the reconnect-rate budget.
The experiments establish no safer physical-buffer TTL; their 30-second interval
is not a release guarantee. A positive host retention interval also expires through
the shared Alarm schedule, without renewing on recovery or retry. Unreadable
authority or inventory refuses recovery; individual attachment loss is isolated.
Only a newly created identity with empty storage and no sockets permits an empty
responsibility baseline. Existing identities require a manifest even without authority
or surviving sockets. Cleanup-generated settlement notices run before subsequent
close commands. No descriptor or payload appears in traces.

From this directory, with the Rust Wasm target and `worker-build` installed:

```sh
npm ci
npm run build
npx wrangler secret put REGISTRATION_SECRET
npx wrangler deploy
```

For `wind share`, import a [relay configuration](../../../docs/cli.md#saved-relays)
with `wind relay import relay.json`, using the stable deployment base URL without
`/v2/ws` and the registration secret. Subsequent shares load it automatically. Protocol probes
that take an endpoint use the full URL ending in `/v2/ws`. Endpoint identity is retained in
storage; an alternate hostname is a different relay identity, even for the same
Worker. Do not delete storage to update code: that would erase permanent STOP.

## Validation

`make rust` includes native fault-injected host tests and Wasm compilation/Clippy.
The shared Go codec client is at `relay-rust/tests/_goclient`.
[Recorded Preview evidence](tests/preview-evidence.md) covers live signatures,
bidirectional delivery, hibernation with unconsumed debt, fault recovery, ordered STOP
and Alarm expiry.

For CLI/browser recovery, log in with the package's pinned Wrangler, then run from `web/`:

```sh
pnpm exec playwright test --config playwright.cloudflare.config.ts
# Only the real deployment scenario:
pnpm exec playwright test --config playwright.cloudflare.config.ts --grep redeploy
```

Each scenario creates and deletes its own randomly named Worker with isolated SQLite
and generated test credentials. It builds Rust and `wind`, disables browser P2P,
then resets the object or runs `wrangler deploy --durable-objects-code-update-mode immediate`
after 4 MiB is written. Immediate switching is requested only by the test.
[Global propagation](https://developers.cloudflare.com/durable-objects/platform/known-issues/#code-updates)
can still take minutes, so reset uses 17 MiB and redeployment uses 97 MiB with real
browser origin storage. Redeployment retains the address, secrets and namespace;
runtime version/code identity and a persisted marker verify the switch while content
is still outstanding. Both scenarios check one browser task, monotonic progress,
completed-block retention and the final hash. Lost connections must recover automatically.
Evidence, command logs and sender/browser traces go to `web/test-results/cloudflare/`;
temporary credentials are removed during cleanup. If cleanup fails, the recorded
Worker name identifies the owned resource to delete. These cloud scenarios are opt-in
and absent from ordinary browser gates.

For protocol-level probes, follow the [Preview runbook](../../../docs/skills/cloudflare-worker-previews-skill.md).
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

The client checks signatures, endpoint isolation and Alarm expiry, then transfers
mixed-size frames in both directions beyond asymmetric windows with duplicate feedback.
It observes constructor re-entry with unchanged sockets/attachments and unconsumed
delivery debt, resumes transfer after consumption, aborts the object and transfers again
after signed RESUME. STOP must deliver exact settlement prefixes before closure and
remain permanent across another abort. Hibernation stays within the original silence
deadline. Delete only the run's Preview after retaining evidence.
