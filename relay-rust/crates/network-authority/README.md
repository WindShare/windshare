# Network authority

The Worker hosts account admission, signed tickets, usage accounting, and the operator
console in one SQLite Durable Object. Clients and nodes use `https://api.ourworld.top`;
`authority.windshare.cc` is its Cloudflare origin. No transfer waits for an authority query.

## Deploy

Generate the signing identity, password pepper, and one-time administrator link on the operator machine:

```text
cargo run --manifest-path relay-rust/Cargo.toml -p windshare-network-authority --bin network-authority-keys -- <private-directory> 1
python scripts/deploy/network-authority.py --prepare-pepper <private-directory>/password.pepper
python scripts/deploy/network-authority.py --api-host <api-host> --prepare-initialization <private-directory>/initialize.token
```

In GitHub Environment `network-authority`, set variables `AUTHORITY_API_HOST`,
`AUTHORITY_WORKER_NAME`, `AUTHORITY_ALLOWED_ORIGINS` and `AUTHORITY_TRUST`
(the entire public `trust-1.json`). Store `online-1.seed` as secret
`AUTHORITY_SIGNING_SEED`, `initialize.token` as `INITIALIZATION_TOKEN`, and
`password.pepper` as `PASSWORD_PEPPER`. Back up `PASSWORD_PEPPER` offline: it
keys every stored password MAC in SQLite, so losing or overwriting it invalidates
all account and administrator passwords. To rotate peppers without lockouts,
prepend a new entry while keeping previous entries (`v2:<new-key>,v1:<old-key>`).
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

1. In **Nodes → Add node**, enter the public HTTPS origin. The setup command configures a freshly provisioned host with the authority URL, issuer and certificate. Existing nodes can set `node.authority` and `node.access.trusted_issuers` directly.
2. Inspect the signed descriptor, confirm the alias, location, roles and capacity, and enroll. The authority verifies the issuer and authority origin, binds the alias permanently and signs the directory with the new node listed as **Canary**. Submitting the same settings again preserves its state and directory revision.
3. Wait for a heartbeat confirming the policy, [test the advertised services](../../../deploy/network/operations.md#test-a-new-node), then choose **Activate**. A Canary can fill a vacant registration slot or take over when all initial Active candidates fail before link publication.

The authority serves the signed head at `/.well-known/windshare-network`. A transaction records directory changes before delivery to KV. The console shows the signed generation and serial, pending KV delivery and certificate expiry. Alarms retry pending writes, and an hourly pass reconciles KV and renews the 90-day directory near expiry. See the [directory export guide](../../../deploy/network/README.md) for Git history and embedded releases.

## Fleet operations

**Drain** stops fresh relay registrations while existing shares can resume and accept receivers. **Disable** withdraws all advertised services. **Remove** retires a disabled node and retains its policy version and permanent alias. Disable and removal require a fresh confirmation of the current policy and zero resident shares, or an explicit early cutoff. Activate can restore a Canary, draining or disabled node. STUN-only nodes always refuse new relay registrations.

Nodes report load every minute. The console marks a previously reporting node offline after three minutes, and shows policy updates awaiting application. For binary updates, follow the [host upgrade procedure](../../../deploy/network/operations.md#official-node-upgrades).

In-place Worker upgrades assume a compatible Durable Object storage schema. Pre-v1.0 schema breaks in persisted credentials or node records require a fresh authority Durable Object and administrator initialization; bundled `deploy/network/release.json` and `history/` restore the signed directory and alias ledger, and any node with a higher cached `policy.version` needs its local access cache cleared.

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

From the repository root, run `node relay-rust/crates/network-authority/tests/run.mjs` with `worker-build` and the relay-cloudflare npm dependencies installed. It builds the isolated test Worker and exercises signed discovery, enrollment, management and KV retries with Miniflare.
