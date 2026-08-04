## Rules

- **Explain "Why", not "What"**: Use comments to explain design rationale, business logic constraints, or non-obvious trade-offs. Code structure and naming should inherently describe the "what."
- **Design for Testability (DfT)**: Favor Dependency Injection and decoupled components. Define interfaces to allow easy mocking, and prefer small, pure functions that can be unit-tested in isolation.
- **Observability by Design**: Emit structured trace logs at critical workflow milestones, major control-flow transitions, and failure-prone or counterintuitive branches. Include stable operation or session identifiers and decision context sufficient to reconstruct the runtime path and diagnose unexpected behavior quickly.
- **Principle of Least Surprise**: Design logic to be intuitive. Code implementation must behave as a developer expects, and functional design must align with the user's intuition.
- **No Backward Compatibility**: Pre-v1.0 with no external consumers to protect. Prioritize first-principles domain modeling and logical orthogonality; favor refactoring core structures to capture native semantics over adding additive flags or 'patch' parameters. Deleting code or rewriting a component from scratch is allowed and encouraged when it yields a cleaner design.
- **Avoid Hardcoding**: Extract unexplained numeric and string values into named constants.
- Don't name your package util, common, or misc. Packages should differ by what they provide, not what they contain.
- **Prefer Deep Modules**: Avoid coupling all functionality at one layer; use meaningful module boundaries to contain complexity. Simply put, each folder should not contain more than 20 code files (excluding test files), otherwise the module is too large.
- **Semantic Precision**: Avoid ambiguous or overloaded fields.
- **Concise User-Facing Docs**: Keep externally maintained docs (README, docs/) concise and easy to follow; nobody reads verbose documentation.

### docs

- Doc Maintenance: Keep concise, avoid redundancy, clean up outdated content promptly to reduce AI context usage. Update docs promptly whenever code or design changes make them stale.
- Use English as much as possible to make it easier for international developers.

### Go Specifics
- **Accept Interfaces, Return Structs**: Define interfaces where they are used (consumer side), not where they are implemented. The bigger the interface, the weaker the abstraction.
- **Hard Requirement**: CI enforces coverage with go-test-coverage (per-module `.testcoverage.yml`): **core total ≥90%, root total ≥80%, every package ≥70%**.

### Validation

- Local gates consume developer-installed toolchains, Web dependencies, and browsers; they never install or update them. Go gates use `GOTOOLCHAIN=local`, and missing prerequisites fail directly.
- During iteration use a focused `make <gate>`. `make check` is the fast feedback path; `make ci` runs `hygiene sloc workflow-lint lint vet short-go vectors web e2e browser gopls` in fixed order.
- `make race` and `make coverage` are diagnostic. `make long-go` mirrors the automatic weekly Go owners, and `make core-release` is the candidate-release gate; none is part of ordinary `make ci`.
- Ordinary GitHub CI has seven independent jobs: `static`, `go-root`, `go-core`, `web`, `go-e2e`, `browser-chromium`, and `windows-native`. The weekly workflow automatically owns long Go, durable, network, interop, cross-browser, and Windows browser suites.
- Pushing the candidate tag `core-candidate/vX.Y.Z/<candidate>` runs Linux/Windows extracted-core checks and idempotently creates `core/vX.Y.Z`; a conflicting existing tag is never moved.
- Windows Firewall/WBEM are not validation gates. Full environment responsibilities and entrypoint ownership are in `docs/validation.md`.
- Validation simplification must not weaken E2EE, capability links, remote-input validation, root confinement, revision/lease and resumable output semantics, crash recovery, no-replace publication, native ancestry revalidation, relay/WebRTC switching, or stable structured observability.

### 其他

- 工具链不准固定版本，尽量使用大版本的latest，Golang使用最新latest。也包括所有 github action。本地直接使用本地工具链就行。尤其要拒绝为了版本问题写了一堆兜底代码，难以维护。
- 写计划文档的时候禁止写在文档里边写一堆验收条件，不准出现验收条件标题块，这导致实现了太多无关紧要的东西而忽略的产品真正的需求。

## Project Overview

WindShare is an open-source E2EE file/folder sharing tool. It creates links without pre-uploading, reading, or hashing content; receivers use the browser or CLI over WebRTC with relay fallback.

```text
.
├── core/                         Independent Go module; network-free reusable core
│   ├── link/, senderobject/      Capability links and sealed transport-neutral objects
│   ├── catalog/                  Per-directory frozen generations and pages
│   ├── content/                  File revisions, leases, file-local blocks
│   ├── session/                  ProtocolSession, pump/router/writer, catalog/content flows
│   ├── framechannel/             Transport-neutral frame contract
│   ├── transfer/                 Selection rules, jobs, OutputSession contract
│   ├── liveshare/                Sender/receiver runtime assembly
│   ├── osfs/                     Root-confined sources, resumable output, atomic artifact publication
│   ├── testvectors/              Canonical Go↔TypeScript protocol vectors
│   └── internal/keyderiv/        HKDF key hierarchy
├── cmd/
│   ├── windshare/                CLI sender and receiver
│   └── testprocessowner/         Test-only bounded process supervisor
├── connectivity/
│   ├── v2signal/                 E2EE peer signaling validation
│   └── v2peer/                   P2P attempt orchestration and lane adoption
├── transport/
│   ├── relayv2/                  WebSocket FrameChannel adapter
│   └── webrtc/                   Pion DataChannel adapter
├── relay/
│   ├── cmd/wsrelay/              Relay server entry point
│   ├── protocol/v2/              Wire frames and opaque routing envelopes
│   ├── signaling/v2route/        Registration, ownership, session routing
│   ├── signaling/v2endpoint/     WebSocket server and connection lifecycle
│   └── httpapi/, connectionlimit/ Operational endpoints and admission limits
├── web/                          React/TypeScript browser receiver
│   ├── src/crypto/, protocol/    WebCrypto and v2 object validation
│   ├── src/catalog/, content/    Progressive catalog and file-local ranges
│   ├── src/session/, transport/  Browser runtime and frame channels
│   ├── src/connectivity/         P2P/relay race and hot switching
│   ├── src/receiver/             Reconnect and protocol-generation supervision
│   ├── src/transfer/, output/    Jobs, sinks, durable output sessions
│   ├── src/preview/, ui/         Media preview and React interface
│   ├── scripts/browser-network-matrix/ Direct browser/Pion interop runner
│   └── e2e/                      Direct smoke and scheduled product scenarios
├── internal/                     Process lifecycle, benchmarks, and focused network test support
├── e2e/                          Process-level Go end-to-end tests
├── testdata/                     Focused test-topology fixtures
├── scripts/ci/                   Local CI gate implementations
└── docs/                         Protocol and security documentation
```
