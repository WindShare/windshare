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

## Real deployment during download

2026-10-19, Windows, Chromium/Playwright 1.62.1, Wrangler 4.151.1.
Source base: `42dcea496376fd4450aba526429c8923d7948414` plus this scenario.
Production Rust host with the test-only observer; the compiled observer revision changes,
while the Rust Wasm hash remains the baseline above.

- Regular Worker: `windshare-redeploy-ae95fc6430b4`; share: `OSaSWpMloZjw9mJ3`.
  Both deployments use the same workers.dev address and SQLite namespace.
- Deployment IDs: `0a580e8b-3745-4163-8332-261647d27fc7` ?
  `536fc379-489d-4cab-a7c6-84ec76aefaec`.
  Runtime version IDs: `849ce095-8517-4687-91c8-ec50a56ecfd0` ?
  `74d800f8-591f-4230-b245-a0adef156dbd`.
- After 4 MiB was written, ordinary `wrangler deploy` requested immediate object
  replacement. The entry updated first; the object changed about five minutes later,
  with 70 MiB written and the file unfinished. Its boot and compiled revision changed;
  the persistent marker and original registration credential survived. Secrets were
  supplied only at creation. No reset endpoint was called.
- The original socket closed. Sender signed RESUME restored publication in about one
  second, generation 1 ? 2. Browser generation 2 continued operation
  `mRgdlMlzlKkujf_THMpaLg`, job `kVC3jNyWv3u06fpBXU0lOw`, without another click or reload.
  The first recovered content request was block 70; blocks 0?69 were retained.
  Continuous trace collection also checks progress accepted while deployment was pending.
- Completed 101,711,872 bytes (97 MiB) in 458,251 ms; 103,121,649 received relay wire
  bytes across three browser connection attempts, with P2P disabled.
  Source/download SHA-256:
  `32374edb3482ade391bf1067ad12db39ebd7bdc6f033e04df0d5cfff445e27c1`.
  CLI SHA-256: `eec7fd852d7c48fac701e26eaa7e5716f813e776934ec4e0de44eb348d78094c`.

Earlier 30-second observation windows saw the new entry with the old object and
correctly failed to establish in-flight replacement. Even an HTTP-only object probe
remained on the old code for two minutes. The longer product run above passed.
This is one deployment recovery, not a propagation-time guarantee, schema migration,
repeated-disruption test or automated deployment. The owned Worker was deleted.

The refactored reset scenario also passed on a fresh regular Worker, share
`KpNWhw3b_i5WlQis`: 17 MiB in 90,296 ms, resuming at block 4 after one reset.
SHA-256: `9a8ab4cae35eedc28893588d1b0326eb90efc31632d388c3440c93524d7edc06`.
Both scenarios' Workers and temporary credentials were removed; an account inventory
confirmed no remaining `windshare-redeploy-*` Workers from this work.

## Receiver-only disconnect isolation

2026-10-19, Windows/Chromium, source base `764a7b20f363d2e7a894baee6a0d83372c1dd963`
plus the shared ingress fix. Production Rust host and real `wind`, P2P disabled.

- Worker: `windshare-redeploy-3f9dcb1e8fd7`; version:
  `46184836-3aaa-4043-a8a3-2c4e8fd91b50`.
- Two browsers downloaded the same 51,380,224-byte file. One receiver interrupted its
  own connection four times after 2 MiB each; the sibling remained in flight.
- The sender never reconnected. The healthy browser retained its original socket
  throughout; the interrupted browser used five connections and completed automatically.
- Both hashes matched `1144c0a4576885c843626df4540c223e3cc0023a3283aede3cd7b128fa661031`.
  The complete scenario, including provisioning/cleanup, took 322 seconds.
- Three consecutive native runs also passed. Deterministic kernel and both-host tests
  cover late data/admission, metadata collection, hibernation, credit and liveness.
- Both temporary Workers and their credential files were removed; the account
  inventory confirmed neither Worker remained.

The first cloud attempt hit its initial 180-second completion budget after receiving
about 43 MiB. Its aggregate sibling counter included one closure without timing, so
it did not establish isolation. The passing run records a pre-download connection
baseline and individual close events, using the existing cloud suite's 600-second
completion budget. Connection-continuity and hash assertions remain mandatory.

Run `pnpm exec playwright test --config playwright.cloudflare.config.ts --grep "receiver interruptions"`
from `web/`. This run's JSON, sender/browser traces and deployment identity are under
`web/test-results/receiver-isolation-cloudflare-diagnostic/`; the native regression
is included in `make browser`.

Reproduce the other scenarios using the [Cloudflare host instructions](../README.md#validation).
JSON evidence, deployment identities and sender/browser NDJSON are retained by the
opt-in configuration, which is absent from ordinary browser gates.
