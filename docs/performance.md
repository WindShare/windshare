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

## Output

Standard output is one schema-versioned JSON report with environment context, command outcomes,
benchmark records, nearest-rank p50/p95 aggregates, and oracle results. Standard error emits JSONL
milestones with stable run, workload, and sample identifiers. Any failed command or oracle is recorded
and makes the runner exit non-zero.

Keep a report only when it helps compare runs from a documented, equivalent environment. Results from
different hosts or toolchains are not directly comparable by default.
