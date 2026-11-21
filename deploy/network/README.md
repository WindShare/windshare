# Network release

`release.json` is shared by Go and the browser build. The link host is
`https://ws.ourworld.top`; frontend hosts never come from directory data.
The production manifest pins two directory roots and the account authority.
`cn1` and `us1` advertise relay and STUN. Verify with `go run ./cmd/network-sign verify`.

The authority signs every new directory revision and renews it before expiry. For a new
signing generation, generate an online Ed25519 key and authorize its public key with one of
the offline roots pinned in `release.json`. Keep the private keys outside this checkout.

```sh
network-sign keygen --id online-2 --private online.pem --public online.json
network-sign authorize --root-private <offline-root.pem> --root-id root-a --online-public online.json --generation 2 --not-before <RFC3339> --expires <RFC3339> --out certificate.json
```

Set the authority's `DIRECTORY_CERTIFICATE` to that certificate and configure its private
PEM file as secret `DIRECTORY_SIGNING_SEED`. See the [authority deployment guide](../../relay-rust/crates/network-authority/README.md).
A certificate can authorize up to 180 days. Each payload lasts at most 90 days.
Clients allow seven days of payload expiry grace within the certificate lifetime.

Before cutting a client release, run `go run ./cmd/network-sign export` and
`go run ./cmd/network-sign verify`. Review and commit the changes in this directory.
Export verifies the public authority snapshot, checks immutable alias history, and updates
the Go/browser release manifest, readable payload, signed archive and GitHub release asset.
The live frontend mirrors update independently of this export.

To verify first-run sharing, create a fresh settings file containing `{"profiles":[]}`
and point `WINDSHARE_CONFIG` at it. Share a test file with `go run ./cmd/wind share <file>`
or from `make desktop`. Open the link in a fresh browser profile and compare the
download with the source. This smoke check uses the official frontend and live
relay nodes. For local relay and browser work, run `dev.ps1`.

Public STUN fallbacks: the existing Google endpoint,
[Cloudflare](https://developers.cloudflare.com/realtime/turn/), and
[Twilio](https://www.twilio.com/docs/stun-turn/api). Provider diversity is known;
mainland reachability and peak-hour measurements remain to be collected on the real fleet.
`public-stun-candidates.json` holds an unreviewed, disabled `icepolicy` candidate
catalog for future measurement and promotion; it is not embedded or dialed by clients.
