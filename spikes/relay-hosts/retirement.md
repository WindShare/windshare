# Cloudflare retirement risk — 2026-10-18

[Evidence](evidence/2026-10-18-retirement.json) records isolated authenticated
Previews, tested source/build hashes, client integrity checks, platform measurements
and cleanup. These are platform probes and real-kernel fixture transfers, not a
production WindShare download.

## Findings

| Experiment | Observed result |
| --- | --- |
| Real kernel: consume data, omit feedback, disconnect and rejoin | With three business slots and two retired-generation slots, three losses leave the next receiver rejected with HTTP 409. The healthy sibling continues. Both attachments and the per-event SQLite mirror reproduce this with only 6 KiB of verified old payload. |
| Strict platform risk policy, four 8 MiB reservations | The fifth admission is rejected, including a retry 35 seconds later; healthy transfers continue. The same rejection occurs when all 32 MiB were consumed and verified before disconnect. |
| Immediate policy expiry, 64 paused generations | 512 MiB submitted, all 64 admissions succeed, healthy pings/transfers remain intact. Old peers later recover 251.3125 MiB; only five recover the whole burst. |
| 30-second policy expiry, 32 paused generations | 256 MiB submitted. Seven deliberate capacity refusals recover after 24.2–25.7 seconds of remaining cooldown. Healthy transfers continue. Old peers later recover 170.5625 MiB. |
| Request close, then hold all 32 peers unread for ten minutes | 512 MiB submitted; healthy transfers continue. Three old peers still recover their complete 16 MiB bursts after more than ten minutes. The other 29 return partial data and abnormal EOF; total recovered is 164.875 MiB. |
| Keep eight peers open and unread for ten minutes | 128 MiB submitted; one peer recovers its complete 16 MiB burst afterwards, seven return partial data and abnormal EOF. Total recovered is 61.8125 MiB; healthy transfers continue. |
| Immediate policy expiry, 64 actual TCP resets | 512 MiB submitted; all 64 replacement admissions succeed with no capacity refusal or healthy-peer timeout. |
| Candidate policy through normal hibernation and forced reset | Four reservations and their original absolute expiry survive both. Repeating retirement does not extend expiry. Expiry permits a new verified 8 MiB transfer; old-generation submission remains rejected. |

The kernel reproduction verifies every byte **before** withholding feedback.
It demonstrates admission exhaustion without requiring retained payload inside
Cloudflare. The tested kernel's `Bounded` grace only freed business slots;
its residual-risk pool had no expiry. Once that pool filled, later lost generations
kept their business slots. Exact persistence cannot repair feedback that will
never arrive.
Its deliberately small limits expose the mechanism; three losses are not a
production failure threshold.

The policy comparison uses a separate Rust platform probe: each tested generation
submits one 8 MiB burst against its reservation, the risk pool has four reservations,
and one object may submit at most 512 MiB and accept 96 connections. Expiry changes only the probe's
admission decision. It never calls the production kernel's `Closed` or `Settled`,
and it does not enable the production data plane. SQLite records bounded
experimental lifecycle/events; payloads are never stored.

## Interpretation

A fixed platform cleanup TTL is still unsupported. In the long-close run,
29 abnormal close callbacks arrived together while three old sockets survived
the entire hold. Socket disappearance and partial EOF do not locate the remaining
bytes or provide a physical-release receipt. A later drain is an intervention,
not a passive lifetime measurement.

The observations support a finite **admission-risk policy**, with revocation,
queue cleanup, fixed deadlines, generation fencing and bounded reconnect rates.
They do not establish that 30 seconds is physically safer than immediate expiry.
A cooldown must not extend on retries or reconstruction, and saturation must
have an automatic recovery path without resetting healthy connections. The measured
24–26-second refusals are a user-experience cost, not a recommended reconnect delay.

Continue Cloudflare integration rather than requiring a universal physical
release proof. The production kernel now implements a finite policy-based end
to retired risk accounting, distinct from consumption and physical release.
The current checkpoint probe retains risk for 60 seconds of injected fixture time;
the recorded no-expiry build remains identified by the evidence hashes.
Real CLI/browser download, forced relay use and end-to-end resume remain
integration work; these probes do not certify them.

## Measurement limits

No healthy-peer timeout or integrity failure occurred in these runs. The resource
outcome query returned 481/481 non-OK events: 418 canceled, 57 disconnected responses,
and one exception plus five aborts matching the deliberate recovery reset. No memory
or CPU limit outcome was returned. Sample maxima were 18.91 MiB for long-close,
32.74 MiB for immediate paused admission, 28.73 MiB for cooldown, 20.85 MiB for
long-open, 19.60 MiB for TCP resets and 14.73 MiB for strict-consumed. Samples arrived
late and some minutes were missing; absent data is not zero usage.

Memory samples report the entire V8 isolate hosting the selected object, which
may include sibling objects, and are delayed periodic samples. They are neither
continuous RSS nor a measurement of Cloudflare's complete forwarding path.
See [Cloudflare's metric definition](https://developers.cloudflare.com/durable-objects/observability/metrics-and-analytics/#memory-usage).

The native peer caps its OS receive buffer at 8 KiB and does not poll while paused.
Its abort mode sets `SO_LINGER=0` before closing the TCP socket. This exercises an
abrupt transport reset, not a firewall-induced packet blackhole. Cloudflare
placement and the workstation's network route are not controlled. The native peer
reported `198.18.0.193:443` under workstation proxy routing, so a local TCP reset
and later readable bytes cannot be localized to Cloudflare's transport buffers.
Overlapping bounded runs and their timestamps are retained; no per-object memory
isolation is claimed. Both owned Previews were deleted after all runs completed.

## Reproduce

Use the [Preview runbook](../../docs/skills/cloudflare-worker-previews-skill.md).
Build the native `cloudflare-slow-peer` with `--features cloud-client`; an alternate
build path can be selected through `CLOUDFLARE_SLOW_PEER_BINARY`.
From `cloudflare/`, run only the relevant bounded group:

```powershell
node pressure-probe.mjs <Preview-URL> close 32 256 600000
node pressure-probe.mjs <Preview-URL> live 8 256 600000
node retirement-probe.mjs <Preview-URL> immediate hold 64
node retirement-probe.mjs <Preview-URL> cooldown hold 32
node retirement-probe.mjs <Preview-URL> strict hold 8
node retirement-probe.mjs <Preview-URL> immediate abort 64
node retirement-probe.mjs <Preview-URL> strict consume 8
node retirement-recovery-probe.mjs <Preview-URL>
node checkpoint-probe.mjs <Preview-URL> ack-loss-exhaustion
```

The opt-in probes remain outside ordinary gates. Wasm Clippy, native-client
Clippy, formatting and JavaScript syntax checks cover the changed probe code.
`retirement-evidence.mjs` packages explicit filenames from a run manifest;
raw diagnostics and credentials remain in ignored `.evidence/`.
