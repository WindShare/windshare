# WebRTC Throughput Improvement

2026-10-22

## Rationale

Improve each connection before adding demand-driven parallelism. Pursue repeatable gains toward 40 MB/s across the complete transfer pipeline while preserving immediate sharing, responsive browsing, and resumable progress.

[Single-connection research](../../testdata/connectivity/webrtc/single-connection-2026-10-22.json) found that Windows UDP write calls dominate much of the measured send-loop time. Three 473 MB transfers per arm improved from a 20.84 MB/s baseline median to 23.89 MB/s with bounded overlapped submission on the same socket. Three same-host transfers had a 56.38 MB/s median, disproving a universal 20 MB/s protocol ceiling; they do not establish physical LAN performance. Window, pacing, MTU, and batching changes have environment-dependent results.

[Earlier experiments](../../testdata/connectivity/webrtc/throughput-2026-10-21.json) reached a 38.5 MB/s median with four paced, dynamically scheduled connections and 32.5 MB/s with roughly 100 ms simulated RTT. Both studies used cached encrypted input and memory output. Complete-product performance and actual WAN throughput remain open.

Peak CPU, memory, and power consumption are not drawbacks. Capacity limits protect stable operation. Weigh startup latency, browsing responsiveness, recovery, network competition, relay quotas, and maintenance complexity. Large transfers are intended uses; their frequency is unknown.

## Ownership

- **Content scheduling** owns eligible pending blocks, byte reservations, priorities, assignment, and cancellation. Available lanes claim work; observations guide pipeline depth rather than fixed traffic shares.
- **Connectivity** owns connection creation, recovery, retirement, and attempt budgets. Initial connectivity and recovery take priority over throughput exploration.
- **Transport** owns send admission and platform I/O, preserving datagram boundaries, deadlines, cancellation, and SCTP congestion control. Platform mechanisms stay outside the network-free core.
- **Engine and receiver resource owners** bound aggregate capacity across connections and jobs.

## Execution

1. **Locate the active limit in each connection.** Connect source reading, sealing, send admission, SCTP windows, native writes, authenticated reception, and output backpressure. Separate starvation, transport delay, and consumer pressure. Compare physical LAN, same-host, reversed endpoints, and real public routes; local UDP wall time alone cannot identify a driver or CPU cause. Reuse bounded tracing with session/path identities.

2. **Improve the native send boundary.** Prioritize repeated gains from bounded outstanding writes and platform batching. Keep ownership of buffers, completion, cancellation, deadlines, and socket closure together behind a small transport interface. Preserve ordinary socket operation when an optimization is unavailable. Do not ship the experiment's fixed timeout, connect an ICE socket to one peer, or increase path MTU globally. Adopt mechanisms only where their benefit justifies their lifecycle complexity; keep platform tuning out of user flows.

3. **Keep the pipeline supplied through explicit byte ownership.** Distinguish locally queued bytes, unfinished responses, and received data awaiting authentication or output. Queue drainage is not remote completion. Replenish the receive window incrementally and adapt outstanding work to progress and response delay. Larger buffers are a diagnostic tool, not a universal default.

4. **Make admission and assignment one decision.** Replace selecting a lane before waiting for capacity with a shared queue of unassigned blocks. Eligible lanes reserve capacity and claim blocks together; faster lanes naturally claim more. Avoid preliminary speed tests, fixed file partitions, and deep private queues. Preserve preview/output-frontier priorities and route eligibility. Cancellation releases reservations; stalled-block rescue remains separate from ordinary unique assignment.

5. **Add demand-driven independent connections.** Keep relay and WebRTC startup immediate. With sustained work, explore additional connections using useful blocks within shared capacity and attempt budgets. Retain connections that improve aggregate authenticated progress and completion time; retire unhelpful ones without discarding verified progress. Use stable observation windows to prevent churn. Improved per-connection throughput does not imply linear multiplication.

6. **Refine through complete-product comparisons.** Compare short and large transfers over LAN and actual public direct/TURN routes, including output pressure and competing jobs. Follow first response, completion, recovery, and useful bytes alongside throughput. Cover changing lane speeds, stalled tails, and cancellation with focused scenarios and injected clocks; keep expensive cross-host experiments separate. A higher endpoint ceiling helps WAN transfers only when bandwidth, RTT, loss, windows, and relay capacity allow it.

Ordinary failure of an additional connection reduces parallelism; authentication failures retain their protocol meaning. Users keep one download task and continuous progress. LAN HTTP remains independently optional. Reuse existing block, lease, and output mechanisms rather than the experiment's whole-file memory assembly.
