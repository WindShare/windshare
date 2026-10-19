# Production capacity evidence

Measured 2026-10-19 on an account whose Workers plan page showed **Free / $0**.
The production release Wasm, ordinary Worker entry, SQLite binding, heartbeat
policy and enabled observability were retained. Each initial scenario used its
own Worker and namespace. P2P was disabled in a real Chromium receiver using the
production frontend and Go CLI. Payloads were random bytes.

## Observed workload

| Scenario | Result | End-to-end elapsed | Payload throughput |
| --- | --- | --- | --- |
| One sender + one idle receiver | Original socket stayed open for 300 s; 20 browser probes and 20 replies | 300 s | — |
| One receiver, 32 MiB | SHA-256 matched; no reconnect | 159.30 s | 0.201 MiB/s |
| Two receivers, 16 MiB each | Both hashes matched; no reconnect | Normal 86.39 s; delayed feedback 150.48 s | 0.213 MiB/s aggregate |

The second receiver delayed consumption reports by 400 ms. This exercises
backpressure; it is not a shaped network or evidence of an operating-system buffer
bound. Throughput includes browser output finalization and this machine's network
route/proxy. It is not the platform's maximum throughput.

## Resource observations

These are **incomplete returned Durable Object samples**, not full-run totals:

| Workload / samples | DO CPU seconds | Duration GB-s | Max sampled memory | SQL rows read / written |
| --- | --- | --- | --- | --- |
| 32 MiB transfer / 3 | 0.853189 | 0.220156416 | 21.56 MiB | 11 / 18 |
| Idle control / 7, including later diagnostic read | 0.039243 | 0.044363904 | 4.15 MiB | 37 / 15 |

Their CPU/memory/fatal error counters were zero.
Memory is an [isolate-wide periodic sample](https://developers.cloudflare.com/durable-objects/observability/metrics-and-analytics/#memory-usage),
not peak process RSS or a per-connection memory bound.

Analytics coverage is incomplete: initial idle and paired namespaces returned no
periodic/storage samples, and repeated queries returned different subsets for the
single namespace. The retained-deployment control initially returned empty metrics,
then exposed the partial samples above about eight minutes after deployment.
Stored-byte gauges remained empty. Empty results are **unknown**, not zero usage.
These samples must not be extrapolated into a daily byte quota. After the
control's idle workload, a read-only SQL inspection found three lifecycle values
totalling 1,780 bytes; SQLite/index/internal overhead and billed storage remain
unmeasured. That diagnostic read added 3 reads and no writes.

Invocation logs separately recorded 10 / 5 / 5 Alarm callbacks over the complete
idle / single / paired lifecycles, including setup and teardown. In the exact
300-second idle interval, 9 Alarm callbacks were returned. Browser probe counts
come from wire observation, not sampled log counts.

**Free-plan CPU headroom is unresolved.** The initial scenarios' edge Worker fetch
logs reached 10 / 20 / 18 ms CPU. Free Workers specify 10 ms per request, with
[flexibility for occasional overruns](https://developers.cloudflare.com/workers/platform/limits/#cpu-time);
successful downloads therefore do not establish that repeated connections will
remain within the limit. The Durable Object's separate CPU allowance must not be
confused with the edge Worker's allowance. A further 5-minute idle run and eight
successive new-share browse sessions passed on one retained deployment. Its
18 returned edge invocation logs included 4 above 10 ms, with a 21 ms maximum;
later calls were mostly 1–2 ms. This is a reproduced headroom concern, not evidence
of CPU termination. Fetch logs marked `canceled` accompany closed WebSockets;
the browser workload outcomes and resource-exhaustion outcomes must be distinguished.

## Free-plan accounting

[Policy checked on 2026-10-19](https://developers.cloudflare.com/durable-objects/platform/pricing/):

- SQLite Durable Objects: 100,000 request equivalents/day, 13,000 GB-s/day,
  5,000,000 rows read/day, 100,000 rows written/day, and 5 GB total storage.
- Incoming application WebSocket messages count at 20:1; upgrades and Alarm
  callbacks count separately. Each `setAlarm()` costs one row written.
- Eligible idle time is excluded from duration; billed duration uses allocated
  128 MB, not the measured heap. Daily limits reset at 00:00 UTC; exceeding a
  free limit causes the affected operations to fail.

At the current 15-second heartbeat cadence, two continuously connected peers
generate about 11,520 heartbeat messages/day, or 576 request equivalents, before
other traffic. Extrapolating the observed 9 Alarms/300 s would add about 2,592
requests/day. This is conditional arithmetic, not a 24-hour measurement, and
excludes transfers, reconnects, Alarm writes and other shares on the account.
It demonstrates why keeping a link online is not zero-cost.

Current evidence supports a small personal trial with one share and one or two
receivers transferring tens of MiB. It does not certify a daily GiB allowance,
all 8 business slots, many simultaneous shares, sustained slow networks or a
public relay. Resolve edge CPU headroom and obtain complete idle/transfer billing
metrics before advertising a quantitative free-tier capacity.

## Reproduce

With the repository prerequisites installed and the package's Wrangler logged in,
run from the repository root:

```sh
node web/scripts/cloudflare-capacity/run.mjs
```

This opt-in runner builds production artifacts, verifies hashes and original
connections, and deletes only its own random Workers. It is not part of ordinary
CI. Local build/deployment identities, wire counters and traces are written under
`web/test-results/cloudflare-capacity/`; generated credentials are removed.
[Retained measurements](evidence/capacity-2026-10-19.json) preserve build hashes,
workload windows and telemetry with consistent anonymous resource labels. Account,
deployment and request identifiers remain only in ignored local captures. Use those
local identities to query invocation logs by `$workers.scriptName` and GraphQL
periodic/invocation/storage datasets by namespace and UTC window. Preserve
missing-data status when summarizing.
