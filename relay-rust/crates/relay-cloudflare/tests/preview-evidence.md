# Lifecycle Preview evidence

Run on 2026-10-17 UTC with Wrangler 4.151.1 and workers-rs 0.8.7.
The production Rust host ran behind the authenticated `entry.mjs` test wrapper.

- Worker: `windshare-relay-cloudflare`; Preview: `lifecycle-60b22e77e444`.
- Deployment: `2ed54c48-ad3e-4c4a-a40e-ddebab1e1c16`.
- Source base: `70f29aaf89a6b108150a8392a166ab1401f1b625` plus this change.
- Wasm SHA-256: `8968e33b2beeeb5b4e5e6e30253ff87a995acb5163046579b1bc2118bd59d8c2`.
- Local source-manifest SHA-256: `61f7bd8de175b01f827c503811c657e464bebce7467b58dca0e41e65c681d2c4`.

The shared Go client passed in 47 seconds:

| Scenario | Observed result |
| --- | --- |
| Registration | Fresh Ed25519 proofs accepted; missing deployment permission, invalid proofs/descriptors and mismatched routes refused. |
| Idle handshake | Stored Alarm matched the cloud clock's absolute deadline; Alarm closed the socket without further client traffic. |
| Normal hibernation | Constructor identity changed; socket identity and 193-byte attachment survived; the original connection acknowledged the next heartbeat nonce. |
| Endpoint isolation | An alternate hostname was refused both on cold restore and with loaded state; the existing sender acknowledged subsequent heartbeats. |
| Fault reconstruction | Object abort closed the old socket; signed RESUME succeeded without the deployment credential. |
| Permanent STOP | After another abort, both fresh REGISTER and RESUME returned Stopped; repeated signed STOP was acknowledged. |

Hibernation share: `iQyDhBuqx30EyQtD`; constructor IDs:
`feb00d6b-ec21-4434-a0bf-f18adda65986` → `e869018d-b318-4853-a8b4-d1bbd2f0efe2`.

The initial Alarm test exposed the SDK's integer-as-relative-delay conversion.
The adapter now passes an explicit `ScheduledTime` date, and the scenario checks
stored timestamps against the cloud clock before waiting for expiry.

This verifies lifecycle controls, not download forwarding or physical buffer release.
The run's Preview was deleted after evidence collection. Local transcripts and the
source manifest are retained in `.tmp/cf-lifecycle-320e8148d2f6/` (untracked).
