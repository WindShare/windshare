# Network authority

The Worker hosts account admission, signed tickets, usage accounting, and the operator
console in one SQLite Durable Object. Clients and nodes use `https://api.ourworld.top`;
`authority.windshare.cc` is its Cloudflare origin. No transfer waits for an authority query.

## Deploy

Generate an offline root and its first online generation outside the checkout:

```text
cargo run --manifest-path relay-rust/Cargo.toml -p windshare-network-authority --bin network-authority-keys -- <offline-directory> 1
python scripts/deploy/network-authority.py --api-host api.ourworld.top --allowed-origins https://ws.ourworld.top,https://windshare.cc --trust <offline-directory>/trust-1.json --online-seed <offline-directory>/online-1.seed --initialize
```

The helper uploads only the online seed and prints a one-time administrator link.
Open it to choose a username and password. Later deployments omit `--initialize`.
Keep `root.seed` offline; rotate with a strictly higher generation and deploy its
certificate and online seed together. Certificates last at most 180 days.

Provision `api.ourworld.top` as a Cloudflare SaaS custom hostname on `windshare.cc`,
using the existing zone fallback. The helper installs its exact Worker route and the
origin custom domain. See [Cloudflare routing](https://developers.cloudflare.com/cloudflare-for-platforms/cloudflare-for-saas/start/advanced-settings/worker-as-origin/).
Mainland DNS points to trusted API ingress hosts; use the
[ingress templates](../../../deploy/network/authority.Caddyfile.template) and DNS-01
certificate renewal. Pass their outbound IPs in `--trusted-proxy-ips` so the Worker
can apply source limits to original clients.

Email mode requires `EMAIL_WEBHOOK_URL` and secret `EMAIL_WEBHOOK_TOKEN`.
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

The checked-in production manifest remains unprovisioned. The previous development
directory must be re-signed with the upgraded fleet's real keys before its next build;
the signer audits older keyless history without admitting it as a current directory.

## Operations

The console controls registration mode/difficulty, source/global creation rates,
daily allowance, node admission, and share/account blocks. Account milestones at
1,000 and 4,000 request review without changing registration mode.

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
