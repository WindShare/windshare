# Production CLI/browser Cloudflare evidence

2026-10-19, Windows, Chromium via Playwright 1.62.1, Wrangler 4.151.1.
Production Rust entry, isolated Preview SQLite, no protocol test wrapper.
Source base: `70ec36ff84d0d0c09db090870041dc95f7ee0fcc` plus the sender relay configuration change.

- Worker: `windshare-relay-cloudflare`; Preview: `cli-fce69d792094`.
- Deployment: `6e52cdd9-d8cf-4393-b65a-5be545960b52`.
- Base URL: `https://cli-fce69d792094-xxx.workers.dev`.
- Wasm SHA-256: `0f4b760e0b7a3a3078b5d1d460431cc3933780efeee8948884ad8b39e71fcc21`.
- `wind.exe` SHA-256: `7f53715da328de6b418bc87753390f232683d6101d808cb1d4c493165763dfa0`.

`wind share` loaded one saved URL/registration-token pair through `WINDSHARE_RELAY_CONFIG`,
without `--relay` or a per-share credential argument. The generated link contained only
the public relay address. Browser JOIN needed no deployment credential.

With browser P2P disabled, share `qCR-fCcepaDakgKs` completed a 17,825,792-byte download
over one Cloudflare `/v2/ws` connection: 17,866,953 received wire bytes in 78,241 ms.
Source/download SHA-256 matched:
`b3e3ebc08e5536e18ebc2b00ed6f054be6e0da954914f58d46b344bdf16b64b9`.
The link and sender trace did not contain the registration token.

Earlier 60-second runs timed out during continuing transfer; the explicit cloud test
uses a 120-second download budget. These observations establish functionality, not a
throughput guarantee. Reconnect, deployment during transfer and STOP durability are
outside this product run. This run's Preview was deleted after evidence retention.

Reproduce using the [Cloudflare host instructions](../README.md#validation).
The opt-in Playwright configuration records JSON evidence and is absent from ordinary browser gates.
