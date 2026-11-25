## Rules

- **Explain "Why", not "What"**: Use comments to explain design rationale, business logic constraints, or non-obvious trade-offs. Code structure and naming should inherently describe the "what."
- **Design for Testability (DfT)**: Favor Dependency Injection and decoupled components. Define interfaces to allow easy mocking, and prefer small, pure functions that can be unit-tested in isolation.
- **Observability by Design**: Emit structured trace logs at critical workflow milestones, major control-flow transitions, and failure-prone or counterintuitive branches. Include stable operation or session identifiers and decision context sufficient to reconstruct the runtime path and diagnose unexpected behavior quickly.
- **Principle of Least Surprise**: Design logic to be intuitive. Code implementation must behave as a developer expects, and functional design must align with the user's intuition.
- **No Backward Compatibility**: Pre-v1.0 with no external consumers to protect. Prioritize first-principles domain modeling and logical orthogonality; favor refactoring core structures to capture native semantics over adding additive flags or 'patch' parameters. Deleting code or rewriting a component from scratch is allowed and encouraged when it yields a cleaner design.
- **Avoid Hardcoding**: Extract unexplained numeric and string values into named constants.
- Don't name your package util, common, or misc. Packages should differ by what they provide, not what they contain.
- **Prefer Deep Modules**: Avoid coupling all functionality at one layer; use meaningful module boundaries to contain complexity. Simply put, each folder should not contain more than 20 code files (excluding test files), otherwise the module is too large.
- **UI & Visual Craft**: Benchmark all interfaces against Linear, Vercel, and Stripe—deliberately engineering typographic hierarchy, spatial rhythm, subtle surface/border contrast, fluid micro-interactions, and complete state coverage (hover/focus/loading/empty/error), while strictly avoiding generic AI clichés (neon gradients, heavy drop shadows, or unstyled defaults). See `docs/ui-design.md` for WindShare's UI design style.
- **Self-explanatory UI**: Omit unnecessary secondary descriptions and helper text. Let primary labels and layout speak for themselves.
- **Semantic Precision**: Avoid ambiguous or overloaded fields.
- **Concise User-Facing Docs**: Keep externally maintained docs (README, docs/) concise and easy to follow; nobody reads verbose documentation.

### docs

- Doc Maintenance: Keep concise, avoid redundancy, clean up outdated content promptly to reduce AI context usage. Keep only what is needed to understand the system's shape and vocabulary; added detail is a regression, not an improvement.
- Use English as much as possible to make it easier for international developers.

### Go Specifics
- **Accept Interfaces, Return Structs**: Define interfaces where they are used (consumer side), not where they are implemented. The bigger the interface, the weaker the abstraction.

### Rust Specifics
- **Prefer Static Dispatch**: Use Generics and Trait Bounds over Trait Objects (e.g., `Box<dyn Trait>`) to leverage monomorphization and compiler optimizations, unless runtime polymorphism is strictly necessary.
- **Make Illegal States Unrepresentable**: Use Enums with associated data to model state machines, rather than Structs with many optional fields.
- **Separate Tests from Code**: Tests in `*_tests.rs`; prefer `#[path="..."] mod ...;` over `pub(crate)` for test access.


### Validation

- `make <gate>` or `make check` or `make ci-parallel`. Local gates use installed prerequisites.
- `make sloc` (sloc-guard per-file line limit).
- See `docs/validation.md` for ownership and release details.

### 其他

- 写计划文档的时候禁止写在文档里边写一堆验收条件，不准出现 验收条件/验收标准 类似标题块，这导致实现了太多无关紧要的东西而忽略的产品真正的需求。
- 新增测试如果没有合理的理由，就不能导致本地测试运行时间大幅增加。
- UI 不要写测试，尤其是脆弱且无意义的测试，例如固定文案、CSS 类名、锁死 HTML 结构等类似行为。
- 当前产品正在开发阶段，一个使用者都没有、没有线上兼容，没有任何负担。
- 如果需要做产品决策，可以从用户使用体验升级/降级角度出发，是否为了低频场景大幅牺牲高频场景体验？或者查看 `docs/clarifications/原始需求.md`。
- UI-UX 设计简单易懂，操作直观，高频路径少选择，降低用户心智负担。


## Project Overview

WindShare is an open-source E2EE file/folder sharing tool. WindShare makes sharing local files and folders feel instant: select, generate a link, and let anyone browse and download in their browser, without waiting for uploads or a full directory scan. End-to-end encryption keeps both content and file metadata private from relays. Prioritize reliable P2P connectivity, seamless relay fallback, and resumable transfers that preserve progress without unnecessary disk usage. Keep everyday sharing effortless, use shared infrastructure efficiently, and give users control through open source and self-hosting. Technical complexity should serve the experience, never burden it.

The root `go.mod` owns WindShare's production Go packages; the pinned Pion SCTP dependency lives under `third_party/pion/`. Within the root module, `core/**` is the network-free domain and protocol package boundary; dependency-graph gates prevent core packages from importing non-core WindShare packages or concrete networking and transport capabilities. `engine/**` composes those contracts with native connectivity and owns application tasks and shared capacity. The CLI and desktop host consume engine APIs and inject native filesystem adapters; shared engine code does not import `osfs/**` or `adapters/**`.

```text
.
├── .github/workflows/             CI, weekly suites, source/binary releases, and Cloudflare deployment
├── core/                         Network-free domain/protocol packages (links, directory/node auth, catalog/content, sessions, transfer, liveshare)
│   └── testvectors/, internal/   Canonical Go↔TypeScript contract vectors, HKDF, pinned Unicode 15, and test fixtures
├── osfs/                         Native file sources, filesystem output, checkpoints/recovery
├── adapters/filesystem/          Native output/recovery assembly and provider diagnostics
├── engine/                       Shared native application API, tasks, observations, aggregate budgets
│   ├── output/, recovery/        Platform-neutral destination and recovery inventory ports
│   ├── account/                  Official network account registration, login, and credentials
│   ├── builtinnetwork/, networkconfig/, networkdirectory/ Built-in network wiring, trust config, and directory refresh
│   ├── relayprofile/              Relay selection and saved credentials
│   ├── relayconfig/, relaysetup/  Saved relay/frontend settings, discovery, device enrollment, and installation
│   ├── turnissuer/               Managed TURN credential issuance
│   └── internal/                 Share/receive/recovery workflows, native connectivity, and network runtime
├── host/desktop/                 Go engine ownership, share/setup commands, authenticated loopback control
├── cmd/
│   ├── wind/                     CLI requests, presentation, recovery interaction, and exit-code mapping
│   ├── windshare-host/           Go shared library; C ABI for desktop host lifetime and result buffers
│   ├── network-sign/             Offline root authorization and online directory signing CLI
│   └── testprocessowner/         Test-only bounded process supervisor
├── connectivity/                 E2E signaling, PeerSet/Pion orchestration, ICE/socket/gateway policies, and relay admission
├── transport/
│   ├── relayv2/                  Relay registration and multiplexed FrameChannel lifecycle
│   └── webrtc/                   Pion FrameChannels and terminal lifecycle
├── relay/protocol/v2/            Go wire contracts, authenticated proofs, and opaque routes
├── relay-rust/                   Production relay and network authority Cargo workspace
│   ├── console/                  Shared node access console crate and embedded assets
│   ├── crates/relay-access/      Service credential and device enrollment policy
│   ├── crates/relay-core/        No-I/O kernel with strict sender-signature verification
│   ├── crates/relay-native/      Native wsrelay host, SQLite, HTTP/WebSocket, and STUN
│   ├── crates/relay-cloudflare/  Worker/DO host, access console, credential management, and managed TURN
│   └── crates/network-authority/ Official account admission, tickets, usage accounting, and authority Worker/DO
├── consoles/                     React/TypeScript management consoles built into Rust console-dist
│   ├── relay/, network/          Node device console, network authority console, and public report form
│   └── ui/, build/               Shared UI, themes, locales, and embedded asset build helpers
├── desktop/                      GPUI app with native sharing; receive/transfer/SuperShare require --preview
│   ├── crates/windshare-desktop/  Application lifecycle, live share projection, views and previews
│   ├── crates/windshare-host/     Native ABI ownership, reconnecting control client, task snapshots
│   ├── crates/windshare-ui/       UI components, themes, icons, typography, native input
│   ├── crates/windshare-activation/ Native share intents, instance election/IPC, and Windows Share acquisition
│   ├── crates/windshare-shell/    Windows Explorer Share with WindShare COM command
│   └── install/                  Windows package identity, installation, and uninstallation scripts
├── web/                          React/TypeScript landing portal and browser receiver
│   ├── src/                      Protocol/crypto, session/connectivity, transfer/output, network/document, preview, and UI
│   └── scripts/, test/, e2e/     Boundary/evidence scripts, unit/contract tests, and Playwright scenarios
├── internal/                     Connection liveness, platform setup, process ownership, performance evidence, test support
├── integration/                  Native relayv2 and v2peer integration scenarios
├── e2e/                          Process-level Go end-to-end tests
├── spikes/                       R0 feasibility and isolated Pion↔Chromium evidence
├── testdata/                     Browser/output evidence, ICE policy/topology, and provider capabilities
├── third_party/pion/              Pinned SCTP sources, patch, and reproducibility manifest
├── deploy/
│   ├── network/                  Signed network manifests, history, and Caddy templates
│   └── wsrelay/                  Native relay container image
└── scripts/
    ├── ci/                       CI gates, validation tooling, and release certification
    ├── deploy/                   Native relay, network authority, and Cloudflare deployment
    ├── install/                  CLI installers and Windows network setup
    └── unicode15/                Pinned Unicode 15 Go table generator
```
