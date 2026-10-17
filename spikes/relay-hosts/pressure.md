# Cloudflare transport pressure — 2026-10-17

[Evidence](evidence/2026-10-17-pressure.json) contains nine runs, source/build
hashes, versions, deployment identities, GraphQL measurements and cleanup.
This is a platform experiment, not a complete WindShare transfer.

## Results

Each load group sent 32 × 16 MiB to native TLS peers with an 8 KiB receive buffer.
Peers stopped polling until all submissions and a final 30-second hold completed.
Payloads used a deterministic high-entropy fixture, without WebSocket compression;
every delivered frame was checked byte for byte.

| Group | Submitted | Read after resuming | Reported DO memory sample maximum | Healthy connection |
| --- | --- | --- | --- | --- |
| Keep slow sockets open | 512 MiB | 512 MiB, all 32 complete | 10.6 MiB | Survived; new connection worked |
| Request close after each burst | 512 MiB | 512 MiB, all 32 complete | 10.6 MiB | Survived; new connection worked |
| Reset after each burst | 512 MiB | 150 MiB total, 2.5–7.94 MiB per old peer, then abnormal EOF | 30.1 MiB | Disconnected in all 32 resets; all replacements worked |

Open/close groups kept the same Rust instance throughout loading and holding.
Old peers resumed 66–84 seconds after their submissions in the close group,
and 47–181 seconds afterward in the reset group. Draining is an intervention:
these ages establish observed retention, not a maximum lifetime or release TTL.

All healthy echo samples passed integrity checks. Continuous pings had no timeout
errors; explicit cleanup cancellations are recorded separately. Ping medians were
178–183 ms across the three main groups. The 1 MiB echo measurements include
both network directions and connection warmup; they are not relay capacity
benchmarks. A sibling object remained reachable during repeated resets.

## Memory attribution

The delayed GraphQL `durableObjectsPeriodicGroups` results are scoped to this
Preview's namespace and each run's object ID. Their sampled maxima are not
continuous process RSS or a complete accounting of the network path.

Independent controls retained 192 MiB of ArrayBuffers or Wasm memory; reported
memory reached about 196 MiB. Incremental JS double arrays caused reconstruction
after a successful 144 MiB allocation. The Wasm control and a single-event
192 MiB heap burst also returned successfully, then reconstructed before the next
status observation. The ArrayBuffer control retained the same instance.
No `exceededMemory` or `exceededCpu` outcome was captured.
Thus the metric responds to application allocations, but the hard-limit/OOM
calibration is inconclusive. Object reconstruction alone is not an OOM diagnosis.

The data does **not** support equating 512 MiB cumulatively submitted with 512 MiB
resident in the object's reported memory. It also does not locate retained bytes:
Cloudflare forwarding, intermediate network/proxy buffers and the client path
were not isolated. The tests used this workstation's route to the SJC edge.

## Design consequence

Continue the Cloudflare implementation; this bounded workload did not reproduce
unusable service or memory growth proportional to submitted data. Preserve live
credit/backpressure and bounded admission. Separate retired-generation metadata
from host residual-risk policy, without fabricating a physical-release receipt.
No measured TTL or immediate physical refund follows from these runs. Whole-share
reset remains inappropriate for routine cleanup of one slow receiver.

## Reproduce

Build the native peer and deploy an authenticated Preview using the
[runbook](../../docs/skills/cloudflare-worker-previews-skill.md). Run groups
sequentially; each command creates fresh object state:

```powershell
node pressure-probe.mjs <Preview-URL> live 32 256 30000
node pressure-probe.mjs <Preview-URL> close 32 256 30000
node pressure-probe.mjs <Preview-URL> reset 32 256 30000
node pressure-probe.mjs <Preview-URL> calibration 32 256 30000 arraybuffer
node pressure-probe.mjs <Preview-URL> calibration 32 256 30000 heap
node pressure-probe.mjs <Preview-URL> calibration 32 256 30000 wasm
node pressure-probe.mjs <Preview-URL> calibration 32 256 30000 heap-burst
```

Server-side budgets survive resets: at most 512 MiB slow payload, 96 accepted
connections and 32 resets per object; calibration is capped at 192 MiB.
The driver stops load on a request/progress failure, then drains at most four
peers concurrently. Raw diagnostics stay in ignored `cloudflare/.evidence/`.
The experiment-owned Preview was deleted; production deployments were untouched.
