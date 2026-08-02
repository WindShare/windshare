# Performance evidence

Performance measurements are release evidence, not correctness gates. Correctness belongs to the
integration, E2E, browser, race, and coverage suites in [Validation](validation.md).

## Runner

```powershell
go -C internal/perfevidence run ./cmd/perfevidence -list
go -C internal/perfevidence run ./cmd/perfevidence -repository ../..
```

The default runs every maintained workload in 20 fresh processes. Use `-workloads` for a
comma-separated subset, `-profile` for CPU and heap profiles from the measured binary, and
`-output` to select the content-addressed evidence root. Fewer than 20 samples are explicitly
ad-hoc; requests above 100 samples per workload are rejected before staging. This workload-local
cap is unrelated to the scheduled readiness history in [Validation](validation.md). The runner rejects
an output root inside the repository unless the directory itself and its staging, runtime, and
published descendants are covered by Git ignore rules before any stage is created.

## Schema version 4 authority

Before building, the runner creates one private source snapshot from the actual controlled
`go list -deps -test` closure. It:

- selects only `*_perfevidence_test.go` files and overlays every other active internal or external
  test file with a package-correct empty stub;
- compares the selected test graph with the package's production closure and rejects repository-local
  packages introduced only by performance tests unless the workload declares their benchmark-harness
  root explicitly;
- records every compiled Go, cgo, native, assembly, syso, and embed input, workspace/module
  manifests, verified module sums and replacements, overlay mappings, and stub hashes;
- inventories and retains one live `GOROOT` generation, copies only those inventoried files, and
  uses the copied executable and `GOROOT` for every subsequent Go command;
- repeats the live inventory around materialization, then requires an identical offline inventory
  inside the snapshot;
- builds only from the sealed local workspace and isolated verified module cache; and
- records a location-independent build identity, module provenance, per-workload closure hash,
  binary SHA-256/build ID, and path-free `go version -m` output. Raw runtime locations, the exact
  child process environment, effective `go env`, and physical overlay-file hashes remain diagnostic
  evidence and do not redefine source identity.

Snapshot and toolchain traversal rejects more than 250,000 objects, 8 GiB in aggregate, depth 64,
or a single file above 2 GiB. Directory reads are batched, and exact hashing/copying stops at the
declared byte count plus one sentinel so a concurrently growing input cannot bypass those limits.

Builds use `GOENV=off`, empty `GOFLAGS`/`GOEXPERIMENT`, an explicit snapshot `GOWORK`,
`GOTOOLCHAIN=local`, the host `GOOS`/`GOARCH`, `CGO_ENABLED=0`, isolated caches,
`-mod=readonly`, and network-disabled authoritative list/build phases. Dependency prefetch is a
separate public-proxy phase. Module bytes are then held by the consumption authority and verified
again inside the same private, network-disabled domain used for authoritative list/build commands.

After all workload measurements, the runner rehashes every recorded compiled input, overlay file and stub,
every `GOROOT/pkg/include` header, the Go executable, and every binary in `GOTOOLDIR`; it then
recomputes semantic overlay and source identities. Any mutation aborts the stage before publication
instead of relying on read-only bits or the artifact manifest; the final revalidation runs after
staged-artifact verification and immediately before the no-replace rename.

Each sample starts the same immutable test binary with `-test.count=1`. Build, sample, profile, and
profile-verification records bind an explicit phase and succeeded-or-failed outcome to exact global
manifest entries; no artifact may be claimed by two commands. Successful commands cannot carry an
error, while failed commands require one and are valid only in failed evidence. A failed build may
omit binary identity, but cannot claim samples or a profile. Every present binary or profile identity
remains fully bound by path, size, and SHA-256. Evidence also records process identities and
timestamps, nearest-rank p50/p95 aggregates, hard-oracle outcomes, and optional profiles. CPU and
heap profiles are accepted only when those bytes remain stable around controlled `go tool pprof`
parses.

## Baseline boundary

The immutable snapshot is the source authority; mutable before/after worktree observations are
not. Baseline eligibility requires:

- every maintained workload and at least 20 processes per workload;
- a clean Git worktree;
- every compiled local byte matching the recorded commit (ignored or untracked compiled inputs
  are ad-hoc unless a future explicit generated-input contract covers them);
- complete CPU, RAM, kernel, Go toolchain, and controlled build-setting identity; and
- successful measurements and hard oracles.

Node, pnpm, and Playwright versions are optional context for this Go-only runner. Publication is
not acceptance.

The store retains exact filesystem identities for the output, artifact, and runtime directories
before validation or recovery. The invoking user and host are trusted. On Windows, each untrusted
measured child runs in an AppContainer without ambient or network capability, and a handle-bound
recursive change ledger is armed before descendant handles are released for the no-replace rename;
any event, completion error, or identity mismatch fails closed. This boundary does not promise
protection from a malicious same-user process that can tamper with the namespace or force ABA.
Publication is then reopened by exact object identity, rehashed, and checked against both the root
namespace and destination name before the ledger is accepted. Existing-destination reconciliation
retains its destination authority through redundant-stage removal and final durability checks.
Recovery binds the run ID to a platform process-instance token rather than trusting a reusable PID;
rollback deletes only the exact staged identity. A valid existing content-addressed destination is
idempotent success; an invalid destination fails closed. The schema-version-4 payload and every
artifact are verified after publication.

Schema version 4 begins a new measurement lineage. It is not numerically comparable with earlier payloads
or the retired R8 results. Browser measurements remain unbaselined and require a future production
build runner with fresh browser processes.
