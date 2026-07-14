## Rules

- **Explain "Why", not "What"**: Use comments to explain design rationale, business logic constraints, or non-obvious trade-offs. Code structure and naming should inherently describe the "what."
- **Design for Testability (DfT)**: Favor Dependency Injection and decoupled components. Define interfaces to allow easy mocking, and prefer small, pure functions that can be unit-tested in isolation.
- **Principle of Least Surprise**: Design logic to be intuitive. Code implementation must behave as a developer expects, and functional design must align with the user's intuition.
- **No Backward Compatibility**: Pre-v1.0 with no external consumers to protect. Prioritize first-principles domain modeling and logical orthogonality; favor refactoring core structures to capture native semantics over adding additive flags or 'patch' parameters.
- **Avoid Hardcoding**: Extract unexplained numeric and string values into named constants.
- Don't name your package util, common, or misc. Packages should differ by what they provide, not what they contain.
- **Prefer Deep Modules**: Avoid coupling all functionality at one layer; use meaningful module boundaries to contain complexity.
- **Semantic Precision**: Avoid ambiguous or overloaded fields.
- **Concise User-Facing Docs**: Keep externally maintained docs (README, docs/) concise and easy to follow; nobody reads verbose documentation.

### docs

- Doc Maintenance: Keep concise, avoid redundancy, clean up outdated content promptly to reduce AI context usage.
- Error reporting or log usage in English. Use English as much as possible to make it easier for international developers.

### Go Specifics
- **Accept Interfaces, Return Structs**: Define interfaces where they are used (consumer side), not where they are implemented.
- **Hard Requirement**: CI enforces coverage with go-test-coverage (per-module `.testcoverage.yml`): **core total ≥90%, root total ≥80%, every package ≥70%**.

### Validation

- During iteration: `make <gate>` (see `Makefile`).
- Local CI: `make ci`.

## Project Overview

WindShare is an open-source E2EE file/folder sharing tool. It creates links without pre-uploading, reading, or hashing content; receivers use the browser or CLI over WebRTC with relay fallback.

```text
.
├── core/                         Independent Go module; network-free reusable core
│   ├── link/, chunk/             Capability links and segmented AEAD blocks
│   ├── layout/, manifest/        Packed-stream geometry and encrypted metadata
│   ├── share/, session/          Transfer plans, sender/receiver, scheduling, frames
│   ├── osfs/                     Root-confined source and sink
│   └── internal/keyderiv/        HKDF key hierarchy
├── cmd/windshare/                CLI sender and receiver
├── connectivity/                 Signaling, P2P/relay orchestration, fallback, fan-out
├── transport/
│   ├── relay/                    WebSocket FrameChannel adapter
│   └── webrtc/                   Pion DataChannel adapter
├── relay/
│   ├── cmd/wsrelay/              Relay server entry point
│   ├── protocol/, signaling/     Shared wire types and session hub
│   └── forward/, admission/      Data forwarding, isolation, and resource limits
├── web/                          React/TypeScript browser receiver
│   ├── src/crypto/, manifest/    WebCrypto and manifest validation
│   ├── src/session/, transport/  Browser scheduler and frame channels
│   ├── src/connectivity/         P2P/relay race and hot switching
│   ├── src/download/, ui/        File sinks and React interface
│   └── e2e/                      Playwright full-stack tests
├── e2e/                          Process-level Go end-to-end tests
├── testvectors/                  Go↔TypeScript golden vectors
├── scripts/ci/                   Local CI gate implementations
└── docs/                         Protocol and security documentation
```
