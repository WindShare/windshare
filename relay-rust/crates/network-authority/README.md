# Network authority

The Worker hosts account admission, signed tickets, usage accounting, and the operator
console in one SQLite Durable Object. Clients and nodes use `https://api.ourworld.top`;
`authority.windshare.cc` is its Cloudflare origin. No transfer waits for an authority query.

## Deploy

Generate the signing identity and the one-time administrator link on the operator machine:

```text
cargo run --manifest-path relay-rust/Cargo.toml -p windshare-network-authority --bin network-authority-keys -- <private-directory> 1
python scripts/deploy/network-authority.py --api-host <api-host> --prepare-initialization <private-directory>/initialize.token
```

In GitHub Environment `network-authority`, set variables `AUTHORITY_API_HOST`,
`AUTHORITY_WORKER_NAME`, `AUTHORITY_ALLOWED_ORIGINS` and `AUTHORITY_TRUST`
(the entire public `trust-1.json`). Store `online-1.seed` as secret
`AUTHORITY_SIGNING_SEED` and the saved token as `INITIALIZATION_TOKEN`.
Provide deployment secrets `CLOUDFLARE_API_TOKEN` and `CLOUDFLARE_ACCOUNT_ID`.
For split DNS also set `AUTHORITY_ORIGIN_HOST`, `AUTHORITY_ZONE` and
`AUTHORITY_TRUSTED_PROXY_IPS`. Point `AUTHORITY_DIRECTORY_MIRROR` at your
published snapshot and use your own public trust in `deploy/network/release.json`.

Run **Deploy network authority**, open the saved initialization link and choose
administrator credentials. Set repository variable `NETWORK_AUTHORITY_ENABLED=true`
for subsequent default-branch deployments. Keep the Worker name and environment
stable. Keep `root.seed` offline. Rotate the certificate and online seed together
using a higher generation. Certificates last at most 180 days.

Provision `api.ourworld.top` as a Cloudflare SaaS custom hostname on `windshare.cc`,
using the existing zone fallback. The helper installs its exact Worker route and the
origin custom domain. See [Cloudflare routing](https://developers.cloudflare.com/cloudflare-for-platforms/cloudflare-for-saas/start/advanced-settings/worker-as-origin/).
Mainland DNS points to trusted API ingress hosts; use the
[ingress templates](../../../deploy/network/authority.Caddyfile.template) and
[DNS-01 certificate renewal](../../../deploy/network/operations.md#certificate-renewal). Pass their outbound IPs in `--trusted-proxy-ips` so the Worker
can apply source limits to original clients.

For email registration and account email changes, set variable `EMAIL_WEBHOOK_URL` and secret `EMAIL_WEBHOOK_TOKEN` in the same environment.
Both use the same [consumer mailbox domain allowlist](src/profile.rs), optionally extended via comma-separated `EXTRA_EMAIL_DOMAINS`, and issue 6-digit verification codes with per-source, per-recipient, and global delivery limits.
Each canonical email address maps to one account: passwordless email verification recovers the existing account with a new device credential while preserving its allowance and block status.
The HTTPS webhook receives bearer-authenticated JSON `{to,subject,text}`; a 2xx
response confirms acceptance. Its timeout is 15 seconds.

## Enroll and publish

1. Install each native node with its [full configuration](../relay-native/README.md).
   Preserve the generated identity file. Verify its discovery v2 descriptor and
   enroll the actual `node_key` in the console. Mainland nodes default to account-only access.
2. Set `node.authority` to `{"url":"https://api.ourworld.top","issuer":"<root>"}`
   and add `{"root":"<root>","certificate":"<certificate>"}` to
   `node.access.trusted_issuers`. Restart to start the reporting loop.
3. Add the public `authority: {url, root}` to the client release manifest.
   Add each real `node_key` to its directory entry and publish a higher signed serial.
   Enable the mainland node's relay service only after account admission and usage
   replies work; then publish its relay role.

After an enabled native node receives a verified usage reply advertising browser login, Desktop offers **Sign in to this service** when adding it. The authority serves `/device`, and the node serves `/device/complete`. Keep both routes on their respective public HTTPS origins.

The console records enrollment and the most recent signed report separately. It
marks a node published only when the live mirror matches the verified release
snapshot and contains its relay key. Transfer a file with a fresh candidate
client and check the account's usage before publishing that snapshot and client.

## Operations

The console controls registration mode/difficulty, source/global creation rates,
daily allowance, node admission, and share/account blocks. Account milestones at
1,000 and 4,000 request review without changing registration mode.
Use **Accounts** to find an account by ID or username and inspect its daily usage
and attributed shares. **Reports** lists active account and share restrictions.

Tickets last 12 hours; entitlements last five minutes and apply to one UTC day.
Usage retries preserve batch IDs and accounting days across restarts. Current
entitlement refreshes continue independently of old retries. Stale or exhausted
allowance lowers scheduling priority while keeping identity and downloaded progress.
Revocations and blocks persist until an explicit newer decision.

PoW defaults to 28 bits, about 16 seconds at 16M attempts/s. Calibrate with
`go test ./engine/account -run '^$' -bench BenchmarkProofAttempt`; difficulty
remains operator-controlled. Run `make rust vectors web` for shared contracts and hosts.

For an isolated local Worker with generation-1 test keys, run
`node relay-rust/crates/network-authority/tests/worker.mjs <loopback-url> <trust-1.json> <initialization-token>`.
It verifies the Wasm/SQLite API, cookies, usage signatures, retries and access decisions.
It uses disposable `local-operator` credentials and refuses non-loopback destinations.

From this crate, run `worker-build --dev --locked` followed by `node --test tests/mail.test.mjs tests/registration-email.test.mjs`. The tests use the relay-cloudflare npm dependencies and a local mail webhook. CI runs the registration test on authority pull requests.
