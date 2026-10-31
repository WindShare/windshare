# Performance diagnostics

Performance reports are local diagnostics. Correctness and release remain owned by the gates in
[Validation](validation.md).

## Run

From the repository root:

```powershell
$env:GOWORK = 'off'
go -C internal/perfevidence run ./cmd/perfevidence -list
go -C internal/perfevidence run ./cmd/perfevidence -workloads ready-real-disk -samples 5 > performance.json
```

Disabling ambient Go workspaces keeps the isolated evidence module's dependency graph reproducible;
the runner applies the same setting to benchmark child processes. The developer provides the local Go
toolchain; the runner does not install or update it. Use
`-repository` only when the repository cannot be resolved from the current directory. An empty
`-workloads` value runs all seven maintained workloads. Each sample directly executes:

```text
go test -run '^$' -bench '^BenchmarkName$' -benchmem -benchtime=... -count=1 -timeout=15m ./package
```

The workloads cover liveshare ready scaling and real-disk readiness, file-local content, multi-lane
transfer, extreme-width catalog spill, relay registration wire cost, and Pion chunk transfer. Exact
benchmark rows, metrics, and behavioral oracles are versioned with the runner.

Cache insertion timing excludes prior capacity admission and partitioning.
Multi-lane timing includes those costs and attempt settlement, at 64 KiB and the
default 1 MiB block size. Adaptive duplicate fetches are reported; retained
plaintext must reach zero after all attempts settle. These memory-only workloads
do not measure network or disk throughput.

## Native relay throughput

Build the release relay and run the opt-in real-socket benchmark with both Go
endpoints at their default receive capacities:

```powershell
Push-Location relay-rust
rustup run stable cargo build --release --locked -p windshare-relay-native --bin relay-native
Pop-Location
$env:WINDSHARE_RELAY_BENCH_BINARY = (Resolve-Path relay-rust/.target/release/relay-native.exe).Path
go test ./relay-rust/tests/_goclient -run '^$' -bench '^BenchmarkNativeRelayThroughput$' -benchtime=8192x -count=4
```

On Linux, use the binary path without `.exe`. Each sample verifies payload bytes
and FIFO order over 8,192 frames, in both directions at 16 KiB and 64 KiB.
Treat the first sample of each case as warmup and report all remaining samples
and their median. Compare identical release relay binaries and benchmark drivers.
The metrics include feedback controls per data frame and sends exceeding 1 ms;
send duration includes socket work and credit waits, not just credit waits.
This isolates transport into memory, without encryption, disk output or WAN
latency. Use `BenchmarkOpenBlock` in `./core/content/records` to measure the
production decode/authentication path separately.

## Progressive discovery

The sender publishes a NodeID hash index with each immutable directory generation. Budgeted
membership filters skip unrelated generations; exact records remain on disk. Index bytes share
the generation's atomic publication, integrity validation, and spill accounting.

When a folder download pauses, compare file-queue starvation with directory request latency.
A completed-file count equal to the discovered-file count can mean discovery has fallen behind;
it does not imply the entire selected folder is complete.

## Content paths

Route permission controls which paths may carry content. A receiver-wide allocator assigns different
queued blocks using measured payload throughput and outstanding bytes; a 10% cost premium favors
direct paths when completion estimates are close. New or stale paths can sample read-ahead or another
independent file's frontier, including during an ongoing file batch. The two oldest active/queued
frontiers in that priority remain on normal allocation; preview frontiers never become probes.
At most one exploratory allocation is active, with one start every five seconds. A successful sample
is not a prerequisite for normal allocation.

Optional direct-path trials grant 12 seconds of accelerated sampling. Already assigned work
may settle for up to 12 additional seconds; expiry never renews the sampling grant.
Inconclusive trials retry after 15, 30, then at most 60 seconds. Baseline path replacement,
progress-counter reset, or 10 seconds without network demand rearms exploration; a measured
result clears inconclusive history. Initial connectivity and recovery keep their own policy.

Each content lane starts one cold block request, then uses unique fragment receipt to admit
more work before the current block finishes. Another request starts when unfinished payload falls
below measured response delay plus 250 ms of traffic, within the receiver's existing concurrency budget.
Cold starts can add one response round trip before filling a fast lane. Idle measurements expire;
cancellation releases queued admission without starting a protocol inactivity
deadline. Browser `content_scheduling.admission` records include the wait, unfinished bytes and
receipt-based admission estimate. Fragment accounting wakes admission only on a blocked-to-ready
transition; completion, capacity release, and route changes retain their own notifications.

Network slots refill independently of ordered output. Read-ahead is bounded by four times each
reader's concurrency and a 64 MiB output buffer budget. A separate 64 MiB block buffer pool covers
shared results, request attempts, and idle cache. Cache borrows unreserved space; its retention limit
can be reduced to zero without restricting transfers. Budget offers rotate between readers. A blocked
output frontier can trigger one duplicate rescue; two rescue attempts may run concurrently, independently
of exploration. Canceled attempts retain lease ownership until they settle. Idle downloads generate no probes.

Directory, revision, and lease requests use a separate latency router with per-kind response samples,
pending request reservations, and content queue estimates. Before throughput is measured, each
outstanding content block contributes its own initial cost; the next request's size cannot inflate
existing work. Reservations remain charged until blocks settle. These requests are never duplicated
for measurement. Inspect `request_scheduling` for request costs/outcomes and `content_scheduling` for
independent allocations and rescues; connection status alone does not identify a transfer bottleneck.
Browser download speed samples received sealed-object bytes, including repeated attempts, separately
from verified output progress. Only new output writes support completion estimates; receipt never
advances saved or resumable coverage. Product receipt updates are limited to four per second, and
`transfer_progress.received_object_bytes` exposes this distinction in traces.

Native control routing seeds each physical lane from its authenticated handshake, then measures
per-kind responses before caller processing. Pending requests and queued content affect selection;
30-second-old response samples return to the handshake estimate without probes or startup waits.
Native `protocol_operation.request_scheduling` records the selected lane's response estimate,
queued content, pending requests, and expected completion time. Explicit operation routes and
content route permissions retain their own authority.

## Browser receive buffer

Desktop Chromium on Windows/Linux negotiates one receive-only video transceiver alongside
the DataChannel, with `max-bundle`, to obtain Chromium's larger shared UDP receive buffer.
It captures and sends no media. This is an implementation-specific compatibility measure;
`receive-buffer` provider facts report selection/negotiation, not a measured kernel buffer size.
Native description failures disable it for subsequent attempts in that receiver session,
including TURN; ordinary recovery budgets and relay availability still govern progress.
Other browsers/platforms use data-only negotiation. Recheck browser/Pion interop after browser
upgrades (`pnpm --dir web run test:browser:interop --project chrome --headed` selects installed Chrome).

## Browser folder saving

Folder claims use bounded inspection workers that refill as files become ready. Independent claims
can reach durable registration without waiting for a slower sibling; database transactions still batch
ready work. Browser namespace operations serialize briefly per parent, while writer ownership and
file removal conflict only for the same verified file. Recursive cleanup requires terminal authority.
`lineage_claims` measures successful unique claims (duplicate requests coalesce); its phase totals
include overlapping waits on shared transactions. The inspector also includes failed attempts.
`inspect_entry` and `create_file` separate namespace inspection from creation. IndexedDB stage events
identify the actual batched transactions.

Compare runs from output-authority acquisition through confirmed publication. A ZIP handed to the
browser has a different completion boundary from a folder confirmed saved. Summed queue wait times
overlap across files and must not be subtracted from elapsed download time.

## Windows native output

Opening an output root checks live volume capabilities and restart identity evidence independently. Subsequent object checks read
current handle identity (volume GUID, serial, and File ID), attributes, and directory case sensitivity;
they do not repeat path-based volume resolution. Ancestry guards still verify each opened directory
against the retained volume. Reopening a root repeats these checks; there is no process-wide path cache.

## Output

Standard output is one schema-versioned JSON report with environment context, command outcomes,
benchmark records, nearest-rank p50/p95 aggregates, and oracle results. Standard error emits JSONL
milestones with stable run, workload, and sample identifiers. Any failed command or oracle is recorded
and makes the runner exit non-zero.

Keep a report only when it helps compare runs from a documented, equivalent environment. Results from
different hosts or toolchains are not directly comparable by default.
