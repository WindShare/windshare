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
- **Hard Requirement**: CI enforces coverage with go-test-coverage over generated, disjoint package sets: **core total ≥90%, non-core total ≥80%, every package ≥70%**.

### Validation

- During iteration, use a focused `make <gate>` or `make check`; run `make ci-parallel` after final code modifications are complete for handoff. Local gates use installed prerequisites.
- See `docs/validation.md` for ownership and release details.

### 其他

- 写计划文档的时候禁止写在文档里边写一堆验收条件，不准出现 验收条件/验收标准 标题块，这导致实现了太多无关紧要的东西而忽略的产品真正的需求。
- 新增测试如果没有合理的理由，就不能导致本地测试运行时间大幅增加。
- 当前产品正在开发阶段，一个使用者都没有、没有线上兼容，没有任何负担。
- 如果需要做产品决策，可以从用户使用体验升级/降级角度出发，是否为了低频场景大幅牺牲高频场景体验？或者查看 `docs\clarifications\原始需求.md`。
- 日志与 trace 设计，不考虑任何严格脱敏，方便开发者 debug。

## Project Overview

WindShare is an open-source E2EE file/folder sharing tool. WindShare makes sharing local files and folders feel instant: select, generate a link, and let anyone browse and download in their browser, without waiting for uploads or a full directory scan. End-to-end encryption keeps both content and file metadata private from relays. Prioritize reliable P2P connectivity, seamless relay fallback, and resumable transfers that preserve progress without unnecessary disk usage. Keep everyday sharing effortless, use shared infrastructure efficiently, and give users control through open source and self-hosting. Technical complexity should serve the experience, never burden it.

WindShare has one production Go module. Within it, `core/**` is the network-free application and protocol package boundary; dependency-graph gates prevent core packages from importing non-core WindShare packages or concrete networking and transport capabilities.

```text
.
├── .github/workflows/             Ordinary CI, weekly suites, and root release
├── core/                         Network-free reusable package subtree
│   ├── link/, senderobject/      Capability links and sealed transport-neutral objects
│   ├── catalog/                  Committed directory generations/pages, durable storage/recovery
│   ├── content/                  File revisions, ranges, leases/capacity, keys, authenticated records
│   ├── session/                  Authenticated protocol engine, catalog/content flows, sender/receiver runtime
│   ├── framechannel/             Transport-neutral frame contract
│   ├── observationstream/        Bounded producer-owned observation queues
│   ├── transfer/                 Receive contracts/selection, jobs, output transactions/settlement
│   ├── liveshare/                Sender/receiver runtime assembly
│   ├── osfs/                     Root-confined sources, native output authority, checkpoints/recovery
│   ├── testvectors/              Canonical Go↔TypeScript contract vectors
│   └── internal/                 HKDF, pinned Unicode 15, protocol contracts, and test fixtures
├── cmd/
│   ├── wind/                     Share/get/resume CLI and recovery management
│   └── testprocessowner/         Test-only bounded process supervisor
├── connectivity/
│   ├── v2signal/                 Canonical E2E signaling schema, codec, and validation
│   └── v2peer/                   Peer negotiation, lane admission, lifecycle diagnostics
├── transport/
│   ├── relayv2/                  Relay registration and multiplexed FrameChannel lifecycle
│   └── webrtc/                   Pion DataChannel FrameChannel and terminal lifecycle
├── relay/
│   ├── cmd/wsrelay/              Relay daemon entry and state/policy assembly
│   ├── protocol/v2/              Authenticated relay control, proofs, and opaque routes
│   ├── signaling/v2route/        Registration/resume/STOP authority and session routing
│   ├── signaling/v2endpoint/     Authenticated WebSocket roles and bounded forwarding
│   ├── httpapi/                  WebSocket upgrade, Origin policy, and admission hook
│   └── connectionlimit/          Global and per-source connection limits
├── web/                          React/TypeScript landing portal and browser receiver
│   ├── src/crypto/, protocol/    Suite-02 links/key hierarchy, sealed-object auth, canonical CBOR/text
│   ├── src/contracts/, session/  Browser FrameChannel contract and ProtocolSession runtime
│   ├── src/catalog/, content/    Progressive catalog, revisions/ranges, leases, lane scheduling
│   ├── src/unicode/              Pinned Unicode 15 path normalization and case folding
│   ├── src/transport/            Relay/WebRTC channels and frame adapters
│   ├── src/connectivity/         Signaling, path policy, and lane adoption
│   ├── src/receiver/             Reconnect and protocol-generation supervision
│   ├── src/transfer/             Intent/discovery, jobs/direct ZIP, capacity backpressure, settlement
│   ├── src/output/               Artifact planning/authority, materialization, publication, recovery
│   ├── src/diagnostics/          Bounded tracing, incident correlation, and developer export
│   ├── src/security/             Capability redaction and bounded diagnostic formatting
│   ├── src/preview/, ui/         Media preview, receiver UI, and landing portal
│   ├── scripts/                  Browser/Pion interop and output/diagnostic evidence
│   ├── test/                     Unit and browser component-contract tests
│   └── e2e/                      Direct smoke and scheduled product scenarios
├── internal/                     Performance evidence, process ownership, test topology, and scenario/trace support
├── integration/                  Native relayv2 and v2peer integration scenarios
├── e2e/                          Process-level Go end-to-end tests
├── spikes/                       R0 feasibility and isolated Pion↔Chromium evidence
├── testdata/                     Versioned browser/output evidence and ICE-topology fixtures
└── scripts/ci/                   CI gates, validation tooling, and release certification
```
