# WebRTC Throughput Improvement

2026-10-22

## Rationale

Improve per-connection efficiency and demand-driven parallelism while preserving immediate sharing, responsive browsing, and resumable progress. Treat 40 MB/s as an experimental target for the complete pipeline, not a prerequisite for multi-connection work.

[Single-connection research](../../testdata/connectivity/webrtc/single-connection-2026-10-22.json) measured Windows UDP write overhead and a median increase from 20.84 to 23.89 MB/s with bounded overlapped submission across three 473 MB transfers per arm. Same-host transfers reached a 56.38 MB/s median, disproving a universal 20 MB/s ceiling without establishing physical LAN performance.

[Earlier experiments](../../testdata/connectivity/webrtc/throughput-2026-10-21.json) reached a 38.5 MB/s median with four paced connections and 32.5 MB/s with roughly 100 ms simulated RTT. Both studies used cached encrypted input and memory output.

Incremental receive windows and joint capacity admission are implemented; the browser also has shared pending-work allocation. [Full-workflow evidence](../../testdata/connectivity/webrtc/allocation-20261022/README.md), [readiness-driven scheduling](../../testdata/connectivity/webrtc/readiness-20261022/README.md), and [SCTP recovery](../../testdata/connectivity/webrtc/stall-20261022/README.md) cover subsequent changes. Scheduling overhead and controlled loss recovery improved; stable end-to-end throughput gains and actual WAN performance remain unproven. Native send acceleration is not adopted.

Higher resource peaks are acceptable when they improve completion without harming responsiveness or stability. Weigh startup latency, recovery, network competition, relay quotas, and maintenance complexity. Large transfers are intended uses; their frequency is unknown.

## Ownership

- **Content scheduling** owns pending blocks, byte reservations, priorities, assignment, and cancellation. Capacity, observed completion cost, and output order guide allocation without fixed traffic shares.
- **Connectivity** owns connection creation, recovery, retirement, and attempt budgets. Initial connectivity and recovery take priority over throughput exploration.
- **Transport** owns send admission and platform I/O integration; `socketauthority` retains socket lifecycle ownership. Preserve datagram boundaries, deadlines, cancellation, and SCTP congestion control outside the network-free core.
- **Engine and receiver resource owners** bound aggregate capacity across connections and jobs.

## Execution

1. **Re-establish the current bottleneck.** Measure source reading, sealing, send admission, SCTP, native writes, authentication, and output after the implemented changes. Separate starvation, transport delay, and consumer pressure. Compare physical LAN, same-host, reversed endpoints, and public routes; UDP wall time alone cannot identify a driver or CPU cause. Reuse bounded tracing with session/path identities.

2. **Evaluate native send acceleration.** Compare bounded outstanding writes and batching against the current complete-product baseline. Integrate buffers, completion, cancellation, deadlines, and closure with the existing socket/mux lifecycle through a small injected interface. Preserve ordinary socket operation when unavailable. Do not ship the experimental fixed timeout, connect an ICE socket to one peer, or increase MTU globally. Adopt repeatable gains that justify maintenance cost; keep tuning out of user flows.

3. **Refine existing byte ownership and receive windows.** Adjust pipeline depth where measurements show starvation. Keep local send queues, unfinished responses, authentication, and output holdings distinct. Receipt replenishes supply without releasing still-owned buffers. Larger buffers remain a diagnostic tool.

4. **Refine shared allocation.** Extend existing admission to choose route-eligible work and available capacity together, balancing expected completion with preview and output-frontier priorities. Avoid parking work behind a busy preferred lane or handing urgent blocks to a slower idle lane. Align Go/browser exploration around unique blocks; keep stalled-block rescue separate. Cancellation withdraws queued work immediately; admitted attempts release their resources after physical settlement. Avoid preliminary speed tests, fixed partitions, and private backlogs.

5. **Add demand-driven independent connections.** Keep relay and WebRTC startup immediate. Use sustained dispatchable work, output pressure, and remaining work to explore extra PeerConnections without a full directory scan. Share capacity across jobs; reserve attempt budget and reclaim optional connection slots for initial connectivity and recovery. Retain connections that improve aggregate authenticated progress and completion time; retire unhelpful ones without discarding verified progress. Use stable observation windows to avoid churn; gains need not scale linearly.

6. **Compare complete workflows.** Measure click-to-first-output, completion, recovery, and useful throughput for short and large transfers over LAN and public direct/TURN routes, including output pressure and competing jobs. Cover changing lane speeds, stalled tails, and cancellation with focused scenarios and injected clocks; keep expensive cross-host experiments separate.

Ordinary failure of an additional connection reduces parallelism; authentication failures retain their protocol meaning. Users keep one download task and continuous progress. LAN HTTP remains independently optional. Reuse existing block, lease, and output mechanisms rather than the experiment's whole-file memory assembly.
