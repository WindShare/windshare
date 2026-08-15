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

## Output

Standard output is one schema-versioned JSON report with environment context, command outcomes,
benchmark records, nearest-rank p50/p95 aggregates, and oracle results. Standard error emits JSONL
milestones with stable run, workload, and sample identifiers. Any failed command or oracle is recorded
and makes the runner exit non-zero.

Keep a report only when it helps compare runs from a documented, equivalent environment. Results from
different hosts or toolchains are not directly comparable by default.
