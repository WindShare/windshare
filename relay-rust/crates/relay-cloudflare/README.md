# Cloudflare relay host

`entry.mjs` serves the receiver frontend and routes `/v2/ws?share_id=...` to a SQLite
`ShareRelayObject`. A separate console origin reaches the SQLite `AccessObject` authority.
The shared `relay-access` crate owns credential and enrollment transitions; `relay-core`
consumes one-operation registration grants without knowing about HTTP or accounts.

Fresh REGISTER supplies `Authorization: Bearer <access_key>`. The edge stores only its
digest on the socket; admission checks current credential status for every fresh registration.
Committed registrations retain principal and credential attribution. RESUME and signed STOP
keep independent authority, and frame forwarding performs no account lookup. Revocation can
also queue durable termination of the credential's existing shares, preventing later RESUME.

Share storage retains endpoint identity, connection allocation, authority and checkpoint
manifests. Attachments retain protocol state and delivery ledgers through hibernation; opaque
payloads never enter SQLite. Alarms enforce original deadlines and durable STOP barriers.
Each share has eight business slots and a separate eight-socket control reserve.
Do not delete storage to update code.

## Deployment and local validation

Use the [GitHub Actions deployment guide](../../../docs/deploy/cloudflare-relay.md).
It provisions separate console/receiver origins, installs `OWNER_PASSWORD`, and prints
`wind relay onboard <url>`. Password rotation invalidates administrator sessions, preserving
client credentials. Discovery is at `/.well-known/windshare`.

With Go, the Rust Wasm target and `worker-build` installed, run from this directory:

```sh
npm ci
npm run test:access
```

This builds the actual Wasm host and uses local workerd/SQLite to test approval, cancellation,
request isolation, credential revocation, password rotation, automatic client installation,
and administrative termination through production Go protocol clients. `make rust` covers
kernel/native tests, coverage, Clippy and Wasm builds; `make hygiene` covers deployment contracts.
The opt-in [live deployment smoke](../../../docs/validation.md#ordinary-github-ci) checks both
hosting modes and retained credentials/storage across updates.

Browser recovery scenarios remain opt-in:

```sh
cd ../../../web
pnpm exec playwright test --config playwright.cloudflare.config.ts
```

They own isolated Workers, temporary credentials and SQLite, exercise receiver isolation,
reset/redeployment during downloads, and remove their resources afterward. Evidence is stored
under `web/test-results/cloudflare/`. See [Preview evidence](tests/preview-evidence.md),
[capacity evidence](tests/capacity-evidence.md) and the [Preview runbook](../../../docs/skills/cloudflare-worker-previews-skill.md).
The test-only `tests/entry.mjs` adds authenticated observation/reset endpoints; production
entrypoints never expose them. Preview probes use a console-created access key in
`WINDSHARE_RELAY_ACCESS_KEY` and the separate test secret in `WINDSHARE_RELAY_PREVIEW_SECRET`.
