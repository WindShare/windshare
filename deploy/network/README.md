# Network release

`release.json` is shared by Go and the browser build. The link host is
`https://ws.ourworld.top`; frontend hosts never come from directory data.
The export remains unprovisioned until operator root keys are installed. Verify
the configured fleet before publishing a signed directory.

Build the local signer with `go build ./cmd/network-sign`. Generate two offline
roots on separate storage and one online key; keep production private keys outside the checkout:

```text
network-sign keygen --id root-a --private <offline-root.pem> --public <root-a.json>
network-sign keygen --id root-b --private <separate-offline-root.pem> --public <root-b.json>
network-sign keygen --id online-1 --private <online.pem> --public <online.json>
network-sign authorize --root-private <offline-root.pem> --root-id root-a --online-public <online.json> --generation 1 --not-before <RFC3339> --expires <RFC3339> --out <certificate.json>
network-sign sign --online-private <online.pem> --certificate <certificate.json>
network-sign verify
```

Copy the two public root records into `release.json.roots`. Edit `directory.json`
with actual nodes and current validity dates; mainland nodes advertise only STUN
until account admission is deployed. Keep both root storage and the online key
outside the checkout. Increase serial for each update; an offline root must
authorize every higher generation. Signing checks all historical alias bindings,
updates the shared embedded snapshot, and writes an immutable export under `history/`.
Certificates last at most 180 days and payloads 90 days. Clients allow seven days of
payload expiry grace, never past certificate expiry.

Publish `windshare-network.json` as `/.well-known/windshare-network` on both
frontends and as a GitHub release asset. Keep the embedded snapshot current in client
releases. Cache expiry disables directory use for new work without stopping transfers.

For local zero-configuration sharing, run `make development-build` from this
checkout. The [development profile](development/README.md) contains its own public
roots and signed history; the CLI, desktop and Web select it explicitly without
changing the production manifest. Outputs and persistent test state belong in
`.windshare-dev/`; local development private keys may stay in the ignored `.secret/`.
Ordinary rebuilds need no signing keys.

Public STUN fallbacks: the existing Google endpoint,
[Cloudflare](https://developers.cloudflare.com/realtime/turn/), and
[Twilio](https://www.twilio.com/docs/stun-turn/api). Provider diversity is known;
mainland reachability and peak-hour measurements remain to be collected on the real fleet.
