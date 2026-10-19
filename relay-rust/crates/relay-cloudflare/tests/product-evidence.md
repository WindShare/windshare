# Production CLI/browser Cloudflare evidence

## Uninterrupted baseline

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

## One controlled object reset

2026-10-19, same platform/tool versions. Source base:
`0e615c8dd91d97927c45a0ce53d473d6d94059c6` plus this recovery scenario and progress identity tracing.
Production Rust host wrapped only by authenticated `tests/entry.mjs` fault controls.

- Preview: `resume-1d69548a45bd`; deployment: `361667ce-36ad-416d-9218-f65ce37d03b9`.
- Share: `Wotj-RAfvZ5OXRtz`; P2P disabled, saved credentialed relay configuration.
- Wasm SHA-256 unchanged from baseline; `wind.exe` SHA-256:
  `d5010e264a75d2a968983350719551a3969fa3347a6b185de095d943d687f83c`.
- Exactly one reset POST after 4,194,304 written bytes (four 1 MiB blocks).
  The object boot changed and the browser's original socket closed.
- Sender automatically completed signed RESUME, connection generation 1 ? 2.
  Browser protocol generation 2 continued operation `qxd6_F6hlbIh6G2TNZva8A`,
  job `noCDtcj0QTZx-kxUW3QgkA`, without reload or another download click.
  Written progress never decreased; subsequent block requests started at index 4
  and never requested indices 0?3 again.
- Completed 17,825,792 bytes in 81,654 ms, with 20,032,847 received relay wire bytes
  across three browser connection attempts. Source/download SHA-256:
  `a4ec3505cdeec01c23d137f2f25f821ebfc009eb9cf089b64d7621a227d7e245`.

An earlier run completed the hash check but exposed a test-observer error:
same-document capability sanitization was counted as a reload. The corrected test
checks document loads and stable browser runtime identity. The run above passed.
This establishes one fault recovery, not redeployment, repeated disruption, disk/crash
recovery or STOP durability. The run's Preview was deleted after evidence retention.

Reproduce using the [Cloudflare host instructions](../README.md#validation).
The opt-in configuration records JSON evidence and sender/browser NDJSON, and is
absent from ordinary browser gates.
