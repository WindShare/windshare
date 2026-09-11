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
direct paths when completion estimates are close. New or stale paths can receive an independent
read-ahead block, with at most one exploratory allocation active and one start every five seconds.
A successful sample is not a prerequisite for normal allocation.

Network slots refill independently of ordered output. Read-ahead is bounded by four times each
reader's concurrency and a shared 64 MiB reservation budget, separate from the 64 MiB block cache.
Budget offers rotate between readers. A blocked output frontier can trigger one duplicate rescue;
two rescue attempts may run concurrently, independently of exploration. Canceled attempts retain
lease ownership until they settle. Idle downloads generate no probes.

Directory, revision, and lease requests use a separate latency router with per-kind response samples,
pending request reservations, and content queue estimates. These requests are never duplicated for
measurement. Inspect `request_scheduling` for request costs/outcomes and `content_scheduling` for
independent allocations and rescues; connection status alone does not identify a transfer bottleneck.

## Output

Standard output is one schema-versioned JSON report with environment context, command outcomes,
benchmark records, nearest-rank p50/p95 aggregates, and oracle results. Standard error emits JSONL
milestones with stable run, workload, and sample identifiers. Any failed command or oracle is recorded
and makes the runner exit non-zero.

Keep a report only when it helps compare runs from a documented, equivalent environment. Results from
different hosts or toolchains are not directly comparable by default.
