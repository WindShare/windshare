# Network release

`release.json` is shared by Go and the browser build. The link host is
`https://ws.ourworld.top`; frontend hosts never come from directory data.
The export remains unprovisioned until operator root keys are installed. Verify
the configured fleet before publishing a signed directory.

Build the local signer with `go build ./cmd/network-sign`. Generate two offline
roots on separate storage and one online key; never put private files in this repository:

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

For development, provision a separate checkout with a dedicated network identity.
Keep private keys in encrypted storage outside all checkouts; reuse its public roots,
certificate, snapshot, and signed history across builds. Build CLI, desktop, and Web
from that checkout. Use a reachable development frontend and an isolated
`WINDSHARE_CONFIG` containing `{"profiles":[]}`, keeping its adjacent
`.state/network/` directory and a separate browser profile. No public release or
mirror is required while the embedded snapshot is valid. Never clear only the
settings file to reset trust.

Public STUN fallbacks: the existing Google endpoint,
[Cloudflare](https://developers.cloudflare.com/realtime/turn/), and
[Twilio](https://www.twilio.com/docs/stun-turn/api). Provider diversity is known;
mainland reachability and peak-hour measurements remain to be collected on the real fleet.
