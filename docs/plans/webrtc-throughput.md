# WebRTC Throughput Improvement

2026-10-24

## Implementation

Implemented shared Engine receive/send capacity, pre-dispatch response reservations, native shared block allocation, and demand-driven independent PeerSet paths. Initial connectivity and recovery retain capacity priority; optional paths use aggregate authenticated/output progress and retire without discarding verified data. Migrated block finals retain their authenticated carrier so accepted-but-lost fragments can be reassigned after exact-operation retirement. Browser canonical CBOR validation skips opaque payload bytes without re-encoding them.

The ordinary validation gates pass. [Workflow evidence and reproduction](../../testdata/connectivity/webrtc/adaptive-20261023/README.md) record frozen builds, paired LAN measurements, public direct/TURN attempts, failure recovery, and limitations.

[Follow-up bottleneck research](../../testdata/connectivity/webrtc/bottleneck-20261024/README.md) found premature retirement: the three-second trial could finish before the allocator's five-second retry. Automatic 1 GiB medians were 28.84 / 30.29 MB/s overall/middle, versus 33.31 / 33.49 with three fixed PCs. Constant-only experiments did not reliably resolve this.

Trials now bind an exact path/lane generation and consume offered/settled work evidence. Bounded real-work sampling preserves protected frontiers; insufficient evidence, changed conditions and measured no-gain have distinct outcomes. Successful growth rebuilds its baseline without failure backoff. [Paired LAN validation](../../testdata/connectivity/webrtc/sampling-20261024/README.md) measured 1 GiB medians of 28.39 / 30.21 MB/s overall/middle versus 26.59 / 27.68 before, with 51 complete hash-verified browser/native transfers; sustained 30 MB/s is not established. Native profiles separately identify synchronous UDP submission overhead; native acceleration remains unadopted.

## Rationale

Improve per-connection efficiency and demand-driven parallelism while preserving immediate sharing, responsive browsing, and resumable progress. Treat 40 MB/s as an experimental target for the complete pipeline, not a prerequisite for multi-connection work.

[Single-connection research](../../testdata/connectivity/webrtc/single-connection-2026-10-22.json) measured Windows UDP write overhead and a median increase from 20.84 to 23.89 MB/s with bounded overlapped submission across three 473 MB transfers per arm. Same-host transfers reached a 56.38 MB/s median, disproving a universal 20 MB/s ceiling without establishing physical LAN performance.

[Earlier experiments](../../testdata/connectivity/webrtc/throughput-2026-10-21.json) reached a 38.5 MB/s median with four paced connections and 32.5 MB/s with roughly 100 ms simulated RTT. Both studies used cached encrypted input and memory output.

Incremental receive windows, joint capacity admission, and browser shared pending-work allocation are implemented. [Allocation](../../testdata/connectivity/webrtc/allocation-20261022/README.md), [readiness](../../testdata/connectivity/webrtc/readiness-20261022/README.md), and [SCTP recovery](../../testdata/connectivity/webrtc/stall-20261022/README.md) document those changes. Native send acceleration is not adopted.

[Preimplementation experiments](../../testdata/connectivity/webrtc/preflight-20261023/README.md) measured the initial complete Windows-to-Linux-browser file baseline at 16.96 / 25.81 / 27.41 MB/s medians with one / two / four connections, four runs each. Four beat two in only two matched comparisons; small files, slow output, and ZIP did not reproduce the ordinary-file gain. Concurrent receivers exposed the sender's real socket limit, while fault probes exposed wave-budget exhaustion and activation-wide failure callbacks. This supports incremental, capacity-aware exploration; neither fixed four-connection operation nor public-route gains are established.

Prioritize network-bound transfers, including large files; handle CPU/output saturation through existing backpressure. Higher resource peaks are acceptable when they improve completion without harming responsiveness or stability. Weigh startup latency, recovery, network competition, relay quotas, and maintenance complexity.

## Ownership

- **Content scheduling** owns pending blocks, byte reservations, priorities, assignment, and cancellation. Capacity, observed completion cost, and output order guide allocation without fixed traffic shares.
- **Connectivity** owns connection creation, recovery, retirement, and attempt budgets through the existing PeerSet. It consumes scheduling observations; initial connectivity and recovery take priority over throughput exploration.
- **Transport** owns send admission, aggregate burst control, and platform I/O integration; `socketauthority` retains socket lifecycle ownership. Preserve datagram boundaries, deadlines, cancellation, and SCTP congestion control outside the network-free core.
- **Engine and receiver resource owners** inject shared budgets across connections and jobs, including attempts, sockets, reassembly, plaintext, and send capacity.

## Execution

1. **Use the measured bottlenecks.** Repeat archived transport controls alongside complete-product runs before attributing their difference to pipeline overhead or a regression. The current baseline and diagnostic CPU profiles cover source reading/sealing through authenticated output. Windows UDP submission and browser canonical-CBOR re-encoding/comparison are measurable costs; neither alone establishes the limiting cause. Investigate redundant work without weakening canonical validation or authentication. Keep starvation, transport delay, and consumer pressure distinct. Extend evidence to reversed endpoints and public routes; reuse bounded tracing with session/path identities.

2. **Establish shared capacity and refine receive windows.** Inject native reassembly and plaintext budgets from Engine instead of creating process accounts inside each receiver. Reserve reassembly and authentication capacity before dispatch; temporary contention waits without failing the download, and receiver shutdown closes only its own accounts. Bound aggregate outstanding sends and bursts while keeping lane queues independent and capacity available for control traffic. Adjust pipeline depth where measurements show starvation. Keep send queues, unfinished responses, authentication, and output holdings distinct; receipt replenishes supply without releasing still-owned buffers. Larger buffers remain diagnostic.

3. **Complete shared allocation.** Refactor Go's per-demand probe races into shared pending-work allocation, following the browser's distinct-block model. Choose route-eligible work and capacity together using expected completion, preview and output-frontier priorities. Compare waiting for a fast lane with using a slower idle lane; reserve duplicate racing for stalled-block rescue. Cancellation withdraws queued work immediately; admitted attempts release resources after physical settlement. Avoid preliminary speed tests, fixed partitions, and private backlogs.

4. **Add demand-driven independent connections.** Expose queued and in-flight work, capacity-blocking reasons, and known or unknown remaining work; progressive discovery never waits for a full scan. Keep relay and WebRTC startup immediate. Carry connection purpose (initial connectivity, recovery, or throughput exploration) through admission on both peers. Within existing session/provider limits, optional paths preserve essential attempt/socket reserves and yield existing slots when another receiver needs initial connectivity or recovery. Account for actual socket cost, defer exploration under shared capacity pressure, and avoid spending recovery retries on optional growth. Replace single-path unavailable callbacks with aggregate eligible-path and recovery decisions before ending download activations; authentication failures retain their protocol meaning. Grow incrementally from sustained network demand, subject to existing backpressure, with fairness across jobs and receivers. Use a small state machine with an injected clock and cooldowns to prevent churn. Judge added paths by aggregate unique authenticated progress and output completion over stable observation windows, not traffic on the added path alone. Retire unhelpful paths without discarding verified progress; gains need not scale linearly.

5. **Compare complete workflows.** Measure click-to-first-output, completion, recovery, and useful throughput for short and large transfers over LAN and public direct/TURN routes, including output pressure and competing jobs/receivers. Cover changing lane speeds, stalled tails, and cancellation with focused scenarios and injected clocks; keep expensive cross-host experiments separate.

6. **Reopen send acceleration only on new evidence.** [Workflow controls](../../testdata/connectivity/webrtc/slowdown-20261022/README.md) found no stable portable-batching gain; complete-workflow native gains remain unmeasured. Skip further experiments unless current profiling identifies a local send bottleneck and a specific change worth testing; multi-connection work proceeds independently. Compare candidates against the current complete-product baseline. Integrate buffers, completion, cancellation, deadlines, and closure with the existing socket/mux lifecycle through a small injected interface. Preserve ordinary socket operation when unavailable. Do not ship the experimental fixed timeout, connect an ICE socket to one peer, or increase MTU globally. Adopt repeatable gains that justify maintenance cost; keep tuning out of user flows.

Ordinary failure of an additional connection reduces parallelism. Users keep one download task and continuous progress. LAN HTTP remains independently optional. Reuse existing block, lease, and output mechanisms rather than the experiment's whole-file memory assembly.
