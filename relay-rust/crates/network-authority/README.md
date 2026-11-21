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
`AUTHORITY_TRUSTED_PROXY_IPS`. Set `DIRECTORY_KV_ID` to the shared frontend KV namespace, `DIRECTORY_CERTIFICATE` to the offline-root-signed directory certificate JSON, and secret `DIRECTORY_SIGNING_SEED` to its private key (the generated PEM file or an unpadded base64url Ed25519 seed). Use the [directory key ceremony](../../../deploy/network/README.md) for this separate signing generation.

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

1. Generate a separate probe key for each vantage point with `network-sign keygen --id cn-a --private probe.pem --public probe.json`.
   Configure `VANTAGE_KEYS` as a JSON array of `{"id":"cn-a","key":"<probe.json key>","zone":"cn"}` entries (`cn` or `global`).
   Install `./cmd/network-vantage` in that zone and run `network-vantage --key probe.pem`. Roll out matching agent and authority builds when the signed probe protocol changes.
   Configure up to two CN probes and one overseas probe. The authority assigns the split-DNS mirror check to CN agents.
2. In **Nodes**, choose **Add node** and enter the public HTTPS origin. For a fresh host with a working reverse proxy,
   use the setup command after installing the [official unit and binary](../../../deploy/network/operations.md#provisioning). Keep its generated identity and database.
   For existing nodes, configure `node.authority` and `node.access.trusted_issuers` using the same public trust.
3. Inspect the signed descriptor, confirm the alias, location, provider, roles and capacity, and enroll.
   A staging node can report immediately. Its signed load report includes its locally enforced metered budget.
   A vantage in the selected zone tests the advertised services. Promote to **Canary** or **Active** once verification passes
   and both Cloudflare mirrors and the mainland mirror report the current signed revision.

The console shows the signed generation/serial, mirror state, silent vantages and upcoming directory or ticket certificate expiry.
Use [directory export](../../../deploy/network/README.md) to capture a verified revision for Git and embedded clients.

After an accepting native node receives a verified usage reply advertising browser login, Desktop and the CLI offer service-account sign-in when adding it. The authority serves `/device`, and the node serves `/device/complete`. Keep both routes on their respective public HTTPS origins.

## Fleet operations

Vantages poll for enrollment every minute and send one signed fleet report every five minutes. **Nodes** shows per-vantage reachability, WebSocket, STUN and TLS expiry. Three consecutive agreeing cycles from at least two reporting vantages, including one in the node's zone, can withdraw or restore a service. A fleet-wide fault, conflicting evidence or loss of the last healthy active service in a zone holds routing steady.

Use **Drain** to stop new registrations while existing links continue serving. Disable or remove after a fresh signed report confirms the policy version and zero resident shares, or confirm early cutoff. Removing a node preserves its published alias binding. A disabled node must pass verification again before promotion.

During a [host upgrade](../../../deploy/network/operations.md#official-node-upgrades), the maintenance hold keeps the node probe-accessible. A disable requested during the hold takes effect on release. An administrator can abort a stuck maintenance hold from the console, leaving a listed node draining or completing a queued disable without clearing any local host fence. Use the operation ID and node key to correlate authority, vantage and host logs.

## Operations

The console controls registration policy/difficulty, source/global creation rates,
daily allowance, node admission, and share/account blocks. Under the default
`by_account_count` policy, new accounts switch from automatic to proof of work
after 1,000 accounts and to email verification after 4,000 when mail delivery
is configured (otherwise staying on proof of work); selecting a fixed mode keeps
it unchanged. Both milestones prompt a capacity review.
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

From this crate run `worker-build --dev --locked`, then `node --test tests/*.test.mjs tests/worker.mjs` with the relay-cloudflare npm dependencies installed.
The tests use isolated local Workers, signed node descriptors, SQLite and a local mail webhook.
