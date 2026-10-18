# Cloudflare Preview evidence

Run on 2026-10-18 with Wrangler 4.151.1 and workers-rs 0.8.7. The production Rust
host ran behind the authenticated `entry.mjs` test wrapper, with isolated SQLite
Durable Object state.

- Worker: `windshare-relay-cloudflare`; Preview: `delivery-eaf31bb4369f`.
- Account: `fb1b5468362a79b5545d660ace3e8f9d`.
- Deployment: `c4bab88c-9bfc-400d-ae03-5e2a1fe2e1b4`.
- Stable URL: `https://delivery-eaf31bb4369f-windshare-relay-cloudflare.doraemonkey.workers.dev`.
- Deployment URL: `https://c4bab88c-windshare-relay-cloudflare.doraemonkey.workers.dev`.
- Source base: `d786f8fff0d6a92183bb398c18ca1554c65069e0` plus this change.
- Tested Wasm SHA-256: `16fbe9edf835aa6fd4944a9c8f723e2dca8ec16e111628d3aa3b147d7bac8ef6`.
- Working-tree source-manifest SHA-256: `9117c58551c85173b395d08fb5ba113d809fc51dfbe0783195a9499ae5cc16f3`.

The shared Go-codec client passed in 110 seconds, including two 20-second idle
intervals and Alarm expiry:

| Scenario | Observed result |
| --- | --- |
| Authority and routing | Live Ed25519 proofs and maximum descriptor accepted; absent credentials, invalid proofs/descriptors and mismatched routes refused. JOIN before publication returned NotFound. |
| Alarm and idle hibernation | Absolute Alarm expired an idle handshake without input. Constructor changed while the registered socket and 194-byte attachment survived; subsequent heartbeat and endpoint-isolation checks passed. |
| Delivery | Both directions transferred 81 mixed-size envelopes, 1,992,303 encoded bytes each, across asymmetric receive capacities. Exact payloads and FIFO order matched; repeated cumulative feedback stayed within capacity. |
| Delivery hibernation | Both directions held two unconsumed envelopes. Constructor changed while socket identities and attachments survived; a probe barrier confirmed no extra credit before consumption. Feedback then resumed the same session through frame 81. |
| Fault recovery | Object abort interrupted the old generation. Signed RESUME without deployment credentials restored ownership, and another bidirectional transfer passed. |
| STOP | Both destinations received their exact submitted-prefix settlement before closure despite outstanding unconsumed data. After another abort, REGISTER and RESUME returned Stopped; repeated signed STOP succeeded. |

Delivery share: `Bv0v_JV2a5x-QaAo`; session: `0000000000000002`.
Delivery constructor: `cc1ff15d-63bb-4f73-9504-aa169e8010db` →
`b5b32191-ef99-4f74-83d1-f0f7ffc30863`.

The first attempt hit the client's five-second whole-transfer deadline while frames
were progressing. Multi-round-trip delivery now has a bounded 30-second deadline;
individual I/O deadlines remain five seconds. No host change was needed for the rerun.

This verifies the relay protocol path, not the pending production Go/browser runtime
cutover, sustained throughput or physical buffer release. The prior lifecycle-only
run is retained in Git history. Transcripts, tested Wasm, source manifest and patch
are in `.tmp/cf-delivery-eaf31bb4369f/` (untracked). This run's Preview was deleted
after evidence collection; its temporary test secrets were removed.
