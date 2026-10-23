# WebRTC Throughput Improvement

2026-10-23

## Rationale

Improve per-connection efficiency and demand-driven parallelism while preserving immediate sharing, responsive browsing, and resumable progress. Treat 40 MB/s as an experimental target for the complete pipeline, not a prerequisite for multi-connection work.

[Single-connection research](../../testdata/connectivity/webrtc/single-connection-2026-10-22.json) measured Windows UDP write overhead and a median increase from 20.84 to 23.89 MB/s with bounded overlapped submission across three 473 MB transfers per arm. Same-host transfers reached a 56.38 MB/s median, disproving a universal 20 MB/s ceiling without establishing physical LAN performance.

[Earlier experiments](../../testdata/connectivity/webrtc/throughput-2026-10-21.json) reached a 38.5 MB/s median with four paced connections and 32.5 MB/s with roughly 100 ms simulated RTT. Both studies used cached encrypted input and memory output.

Incremental receive windows and joint capacity admission are implemented; the browser also has shared pending-work allocation. [Full-workflow evidence](../../testdata/connectivity/webrtc/allocation-20261022/README.md), [readiness-driven scheduling](../../testdata/connectivity/webrtc/readiness-20261022/README.md), and [SCTP recovery](../../testdata/connectivity/webrtc/stall-20261022/README.md) cover subsequent changes. Scheduling overhead and controlled loss recovery improved; stable end-to-end throughput gains and actual WAN performance remain unproven. Native send acceleration is not adopted.

Higher resource peaks are acceptable when they improve completion without harming responsiveness or stability. Weigh startup latency, recovery, network competition, relay quotas, and maintenance complexity. Large transfers are intended uses; their frequency is unknown.

## Ownership

- **Content scheduling** owns pending blocks, byte reservations, priorities, assignment, and cancellation. Capacity, observed completion cost, and output order guide allocation without fixed traffic shares.
- **Connectivity** owns connection creation, recovery, retirement, and attempt budgets. It consumes scheduling observations; initial connectivity and recovery take priority over throughput exploration.
- **Transport** owns send admission, aggregate burst control, and platform I/O integration; `socketauthority` retains socket lifecycle ownership. Preserve datagram boundaries, deadlines, cancellation, and SCTP congestion control outside the network-free core.
- **Engine and receiver resource owners** inject shared budgets across connections and jobs, including attempts, sockets, reassembly, plaintext, and send capacity.

## Execution

1. **Re-establish the current bottleneck.** Measure source reading, sealing, send admission, SCTP, native writes, authentication, and output after the implemented changes. Separate starvation, transport delay, and consumer pressure. Compare physical LAN, same-host, reversed endpoints, and public routes; UDP wall time alone cannot identify a driver or CPU cause. Reuse bounded tracing with session/path identities.

2. **Establish shared capacity and refine receive windows.** Inject native reassembly and plaintext budgets from Engine instead of creating process accounts inside each receiver. Reserve reassembly and authentication capacity before dispatch; temporary contention waits without failing the download, and receiver shutdown closes only its own accounts. Bound aggregate outstanding sends and bursts across connections. Adjust pipeline depth where measurements show starvation. Keep send queues, unfinished responses, authentication, and output holdings distinct; receipt replenishes supply without releasing still-owned buffers. Larger buffers remain diagnostic.

3. **Complete shared allocation.** Refactor Go's per-demand probe races into shared pending-work allocation, following the browser's distinct-block model. Choose route-eligible work and capacity together using expected completion, preview and output-frontier priorities. Compare waiting for a fast lane with using a slower idle lane; reserve duplicate racing for stalled-block rescue. Cancellation withdraws queued work immediately; admitted attempts release resources after physical settlement. Avoid preliminary speed tests, fixed partitions, and private backlogs.

4. **Add demand-driven independent connections.** First expose scheduling observations of dispatchable work, output pressure, and known or unknown remaining work; progressive discovery never waits for a full scan. Keep relay and WebRTC startup immediate. Carry connection purpose (initial connectivity, recovery, or throughput exploration) through admission on both peers; optional direct paths must preserve essential attempt reserves and yield slots within existing session/provider limits. Grow from sustained network demand with shared capacity and authentication/output headroom, fairly across jobs and receivers. Use a small state machine with an injected clock and cooldowns to prevent churn. Judge added paths by aggregate unique authenticated progress and output completion over stable observation windows, not traffic on the added path alone. Retire unhelpful paths without discarding verified progress; gains need not scale linearly.

5. **Compare complete workflows.** Measure click-to-first-output, completion, recovery, and useful throughput for short and large transfers over LAN and public direct/TURN routes, including output pressure and competing jobs/receivers. Cover changing lane speeds, stalled tails, and cancellation with focused scenarios and injected clocks; keep expensive cross-host experiments separate.

6. **Reopen send acceleration only on new evidence.** [Workflow controls](../../testdata/connectivity/webrtc/slowdown-20261022/README.md) found no stable portable-batching gain; complete-workflow native gains remain unmeasured. Skip further experiments unless current profiling identifies a local send bottleneck and a specific change worth testing; multi-connection work proceeds independently. Compare candidates against the current complete-product baseline. Integrate buffers, completion, cancellation, deadlines, and closure with the existing socket/mux lifecycle through a small injected interface. Preserve ordinary socket operation when unavailable. Do not ship the experimental fixed timeout, connect an ICE socket to one peer, or increase MTU globally. Adopt repeatable gains that justify maintenance cost; keep tuning out of user flows.

Ordinary failure of an additional connection reduces parallelism. Replace single-path unavailable callbacks with aggregate eligible-path and recovery decisions before ending download activations; authentication failures retain their protocol meaning. Users keep one download task and continuous progress. LAN HTTP remains independently optional. Reuse existing block, lease, and output mechanisms rather than the experiment's whole-file memory assembly.
