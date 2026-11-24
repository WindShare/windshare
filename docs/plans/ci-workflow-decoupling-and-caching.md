# CI/CD 工作流职责解耦与构建缓存优化执行计划

## 1. 背景与问题评估

针对近期对 `.github/workflows/` 的审查意见，经逐文件核实代码、部署脚本与用户文档后，按**真实影响、用户/开发者体验、架构清晰度与复杂度收益比**完成分级裁决：

### 1.1 核查汇总与决策矩阵

| 原编号 | 问题摘要 | 事实核查与补充发现 | 决策 |
|---|---|---|---|
| **P0-1** | 直推 `main` 改 TURN/中继客户端与部署单测时漏跑验证 | **属实**。`cloudflare-relay.yml` 的 `push.paths` 漏掉 `pull_request.paths` 已覆盖的全部 9 条跨层 Go 线合约路径（含 `engine/turnissuer/**`、`engine/relaysetup/**`、`relay-rust/tests/_goclient/**`、`relay/protocol/v2/**` 等）；`network-authority.yml` 的 `push.paths` 漏掉 `scripts/deploy/network_authority_test.py`。主仓高频直推 `main`，导致实测覆盖缺口。 | **纳入（治本重构；`network_authority_test.py` 归入 `hygiene.sh` 避免改单测误部署）** |
| **P0-2** | 改 `web/**` 推 `main` 白跑全套 Rust+Go+Wasm 中继测试 | **属实且影响高频**。近期约 40% `main` 提交只改 `web/`，但每次都触发 `cloudflare-relay.yml` 跑 `console` + `verify`（两者均不测试 `web/`），且 `cancel-in-progress: false` 导致并发排队。 | **纳入（治本重构）** |
| **P1-1** | `go-e2e` 与 `weekly.yml` 7 个 job 冷编译 `relay-native` | **属实**。`ci.yml` 的 `go-e2e` 与 `weekly.yml` 的 7 个 job 每次冷编译 `windshare-relay-native`（含 bundled SQLite C 编译）。其中浏览器套件编译预算仅 180s，`windows-browser` 易撞超时；此外 `weekly.yml` 的 `native-output-*` 与 `release.yml` 的 `linux-ext4`/`windows-ntfs` 各自维护孤立缓存，易撞 7 天过期线。 | **纳入（需配 `shared-key` 控配额）** |
| **P1-2** | `worker-build` 重复编译、`deploy` 缺缓存、npm/toolchain 浪费 | **4/5 属实 + 发现 3 处遗漏**：<br>1. `network-authority.yml` 的 `verify` 未设 `cache-bin: false`（默认已缓存），其余 **4 处**确实未缓存 `worker-build`。<br>2. 两个 `deploy` job 均无 `rust-cache`，紧跟 `verify` 后从零重编。<br>3. `network-authority.yml` 缺 `npm` 缓存，且进入 `relay-rust/` 未设 `RUSTUP_TOOLCHAIN: stable`，会受 `relay-rust/rust-toolchain.toml` 影响下载多余的 `clippy`/`rustfmt`/`llvm-tools-preview`。<br>4. **额外发现**：`cloudflare-relay.yml` 的 `verify` 与 `weekly.yml` 的 `cloudflare-managed-turn` 同样漏设 `RUSTUP_TOOLCHAIN: stable`；被 3 个工作流复用的 `console-bundle.yml` 漏配 `pnpm` 缓存。 | **纳入** |
| **P1-3** | `go-test-coverage` 在 `go-core`/`go-non-core` 每次 `go install` | **属实但影响被高估**。`actions/setup-go` 已按 `go.sum` 缓存 `GOMODCACHE` 与 `GOCACHE`，命中时仅耗数秒，且两个 job 远快于 `desktop-windows`/`macos-native`，不在 CI 关键路径上。 | **暂不做（低 ROI）** |
| **P2-1** | `cloudflare-relay.yml` 未使用 GitHub `environment:` 隔离密钥 | **结论失当，忽略受众差异**。`donations`/`network-authority`/`official-network` 面向官方运营者（同仓多套 Cloudflare 服务需同名 Secret 隔离）；`cloudflare-relay.yml` 面向**开源 Fork 自建用户**。强加 `environment:` 会迫使自建用户在两处不同设置页分别配置变量与密钥，显著降级首次上手体验，而主仓本身未配置仓库级中转密钥，安全收益近乎为零。 | **明确不做（补注释防误报）** |
| **P2-2** | `workflow_dispatch` 与 `*_ENABLED` 开关规则分裂 | **属实且代码与文档矛盾**。`docs/deploy/cloudflare-relay.md` 声明 `CLOUDFLARE_RELAY_ENABLED=false` 仅禁用自动更新，但当前 workflow 实际把手动 `Run workflow` 也静默跳过。 | **纳入** |
| **P2-3** | `node-version: latest`、`setup-go@v6`、工作流改名 | 1. `node-version: latest` 在 `ci.yml`（5 处）和 `weekly.yml`（5 处）共 **10 处**，奇数版 Node 存在上游突发破坏风险。<br>2. `official-network.yml:48` 残留 `actions/setup-go@v6`（其余 29 处均为 `@v7`）；`ci.yml:340`（`desktop-windows`）残留 `actions/upload-artifact@v6`（其余 6 处均为 `@v7`）。<br>3. **工作流硬改名不合理**：自建文档直接指引点击 **Deploy private relay**；PR 列表看到 `Deploy` 的根因是职责耦合，不是命名问题。 | **前两项纳入；改名不做（由解耦根治）** |

### 1.2 核心根因分析

P0-1（PR/push 路径漂移）、P0-2（改 `web/**` 白跑中继测试）、P2-3（PR 界面误显示 `Deploy private relay`）并非三个孤立问题，而是**同一个架构耦合的三种表现**：

> `.github/workflows/cloudflare-relay.yml` 同时承担了「**CI 协议与 Worker 正确性验证**」和「**自建 Fork 的 CD 自动部署**」两个不同生命周期、不同触发路径、不同并发策略、不同受众的职责。

若仅在原文件内添加 `paths-filter` 与改名，会引入额外的条件编排复杂度，且在主仓未开启部署时每次推送 `web/**` 仍会分配 runner；通过**职责拆分**可以从根源一次性消除上述全部矛盾。

---

## 2. 架构设计决策

### 决策 A：拆分 Cloudflare Relay 的 CI 验证与 CD 部署

1. **新增纯 CI 工作流 `.github/workflows/cloudflare-relay-verify.yml`（`name: Private relay verification`）**
   - 触发方式：`workflow_call`（接收可选布尔入参 `caller_managed`，默认 `false`，用于绕过 GitHub Actions `workflow_call` 继承调用方 `github.event_name == 'push'` 的平台限制）+ `workflow_dispatch`（保留维护者安全手动纯验证入口）+ `pull_request` + `push(main)`。
   - `pull_request.paths` 与 `push.paths` **严格保持一致**，完整迁入当前 `cloudflare-relay.yml` `pull_request.paths` 中的中继 Worker、管理台、部署脚本及**全部 9 条跨层 Go 线合约路径**（`core/networkendpoint/**`、`core/session/protocolsession/turn_control.go`、`core/session/turngrant/**`、`engine/relayconfig/turn.go`、`engine/relayprofile/**`、`engine/relaysetup/**`、`engine/turnissuer/**`、`relay/protocol/v2/**`、`relay-rust/tests/_goclient/**`），**绝不包含 `web/**` 与 `deploy/network/**`**。
   - 防 Fork 双跑门控：`console` 与 `verify` job 均配置 `if: inputs.caller_managed == true || github.event_name != 'push' || vars.CLOUDFLARE_RELAY_ENABLED != 'true'`。主仓（保持不启用 `CLOUDFLARE_RELAY_ENABLED`）及所有 PR、手动触发正常执行；开启了自动部署的自建 Fork 同步 `main` 时，独立验证直接 0 成本跳过，交由同时触发的 `Deploy private relay` 统一调用一次（`frontend` 模式的 Fork 也不会为未部署的中继白烧 Actions 分钟数）。
   - 并发策略：按 `github.workflow` 隔离组名并统一设置 `cancel-in-progress: true`（与 `native-output.yml` 一致）。独立运行时快速丢弃过期提交；被 `Deploy private relay` 调用时因组名不同且调用方已以 `cancel-in-progress: false` 串行保护，不会互相取消，同时规避顶层 `concurrency` 依赖 `inputs` 的多触发器风险。
   - 包含 `console`（复用 `console-bundle.yml`）与 `verify`（部署脚本单测 + Miniflare `npm run test:access`）两个 job。
2. **精简部署工作流 `.github/workflows/cloudflare-relay.yml`（保留 `name: Deploy private relay`）**
   - 触发方式：仅保留 `workflow_dispatch` 与 `push(main)`，**移除 `pull_request`**。
   - `push.paths` 仅包含真正影响线上部署产物的文件（中继 Worker、管理台、部署脚本、`deploy/network/**`、`web/**`），**不包含 Go 客户端路径，也不包含 `.github/actions/setup-worker-build/**`**（避免纯 CI 工具缓存脚本改动触发 Fork 无意义重部署；升级 `worker-build` 版本时 `Cargo.toml`/`Cargo.lock` 同步变更仍会触发部署）。
   - 并发策略：维持 `group: cloudflare-relay` 与 `cancel-in-progress: false`，确保线上发布串行不中断。
   - 门控机制：
     - `verify` job 调用 `./.github/workflows/cloudflare-relay-verify.yml` 并传入 `with: { caller_managed: true }`，前置 `if:` 要求满足 `默认分支 && (workflow_dispatch || CLOUDFLARE_RELAY_ENABLED == 'true') && CLOUDFLARE_DEPLOYMENT_MODE != 'frontend'`。
     - `deploy` job 依赖 `verify`，使用 `!cancelled()` 配合 `needs.verify.result`（防止已取消的工作流在 `frontend` 模式下被 `always()` 误放行）：当 `CLOUDFLARE_RELAY_ENABLED != 'true'` 且非手动触发时，**所有 job 在调度阶段直接跳过，分配零台 runner**；当 `CLOUDFLARE_DEPLOYMENT_MODE == 'frontend'` 时，直接跳过中继编译与验证，只构建并发布前端资产。
   - 保留仓库级 Secrets（不使用 `environment:`），并在文件头注释中明确说明原因，防止后续被机械重构破坏自建体验。

**一举解决的问题**：
- PR 与直推 `main` 的中继/TURN 验证路径完全同源，永不漂移（根治 P0-1）。
- 主仓未开部署时，推送 `web/**` 零 runner 消耗、零排队（根治 P0-2）。
- PR 检查页只出现 `Private relay verification`，自建用户在 Actions 面板依然看到直观的 `Deploy private relay`（根治 P2-3 命名冲突，且不降级自建体验）。
- 自建 Fork 同步 `main` 只跑一次验证不双跑，同时维护者保留通过 `Private relay verification` 手动安全验证的入口。

### 决策 B：`network-authority.yml` 的最小化治理（不强行拆分）

- 与 `cloudflare-relay.yml` 不同，`network-authority.yml` 不包含 `web/**` 路径、仅面向官方运营者、已配置 `environment: network-authority` 且已支持手动触发绕过开关，因此**无需为了形式对称而强行拆分文件**。
- 将 `scripts/deploy/network_authority_test.py` 纳入 **Linux** 的 `scripts/ci/linux/hygiene.sh`（对齐同目录 `install_web_test.py` 的既有约定，耗时 <0.2s，每次 PR 与直推 `main` 必由 `ci.yml` `static` 执行）：
  - **不加入 `scripts/ci/windows/hygiene.ps1`**：`hygiene.ps1` 当前零 Python 依赖，而该单测依赖 Python 3.11+ 的 `tomllib`，避免为了极低频的官方运维脚本单测给 Windows 开发者高频本地 `make hygiene` / `make ci` 引入新的强制环境依赖。
  - **不加入 `network-authority.yml` 的 `push.paths`**：已由 `hygiene.sh` 全量覆盖，避免仅改单测文件推 `main` 时误触发线上 Authority Worker 生产重部署；保留 `pull_request.paths` 与 `verify` 内的单测执行，并在 `push.paths` 顶部补注释说明分工。

### 决策 C：分层缓存策略与 GitHub 10GB 缓存配额保护

GitHub Actions 单仓库缓存上限为 10GB。若直接向 8 个 job 粘贴无共享键的 `Swatinem/rust-cache@v2`，会按 `github.job` 生成 8 份各自约 300MB 的重复缓存（总增量 >2GB），极易触发 LRU 淘汰并挤掉耗时最长的 `desktop-windows` GPUI 缓存。

采用**按构建产物画像分组的 `shared-key` + 单点写入（只读消费者设 `save-if: false`）**策略（`Swatinem/rust-cache` 在 PR 未改 `Cargo.lock` 时命中精确键自动跳过上传，仅改依赖的 PR 在分支内写入并复用，因此单点写入者及 `desktop-windows` 等独占缓存不额外设 `save-if: main`，避免牺牲改依赖 PR 的迭代体验）：

| 缓存池 | 机制与 Key 策略 | 写入者 | 只读复用者（`save-if: false`） | 说明 |
|---|---|---|---|---|
| **Worker 构建工具** | 新建 `.github/actions/setup-worker-build`（`actions/cache` exact key：`worker-build-v1-<image-os>-${{ runner.arch }}-${{ inputs.version }}`，其中 `<image-os>` 在 bash 中读取 `${ImageOS:-${RUNNER_OS:-unknown}}`） | 首次未命中时立即写入 | 全部 5 处 `worker-build` 调用点 | 仅缓存约 15MB 二进制，不随 `Cargo.lock` 或 `rustc` 小版本失效，仅在 runner 镜像大版本换代（如 `ubuntu24`→`ubuntu26`）或升工具版本时刷新，防范系统库 ABI 漂移；全仓 `rust-cache` 保持 `cache-bin: false` |
| **`relay-native` Linux Debug** | `Swatinem/rust-cache@v2` + `shared-key: relay-native-debug` | `ci.yml` 的 `go-e2e` | `ci.yml` 的 `browser-smoke`；`weekly.yml` 的 `integration-linux`、`named-go-long`、`browser-progressive`、`browser-network`、`cross-browser`、`native-output-linux`；`release.yml` 的 `linux-ext4` | Linux 9 个 job 共享 **1 份**缓存，由主分支高频 CI 持续保鲜，消除 `weekly`/`release` 独立缓存 7 天过期与配额占用 |
| **`relay-native` Windows Debug** | `Swatinem/rust-cache@v2` + `shared-key: relay-native-debug` | `ci.yml` 的 `rust-windows` | `weekly.yml` 的 `integration-windows`、`windows-browser`、`native-output-windows`；`release.yml` 的 `windows-ntfs` | `rust-windows`（`cargo test`）因 `[dev-dependencies]` 启用了 `tokio/test-util`，复用者可稳定命中最慢的 `libsqlite3-sys`（C SQLite）与 `aws-lc-sys`/`ring`/`rustls` 等依赖且永不撞 7 天淘汰（仅重编依赖 `tokio` 的 `hyper`/`reqwest` 子树与本地 crate）；**不拖慢高频 `rust-windows`**，仅在 `windows-browser` 以 `working-directory: relay-rust` 前置预编译 `--bin relay-native` 移出 180s Playwright 超时框并将该 job 超时放宽至 15 分钟 |
| **Cloudflare Relay Wasm** | `Swatinem/rust-cache@v2` + `shared-key: relay-cloudflare-wasm` | `cloudflare-relay-verify.yml` | `cloudflare-relay.yml` 的 `deploy` | `deploy` 编 Wasm 前必已通过同分支 `verify`，单点写入跨 job 复用 `--release` 外部 Wasm 依赖与 proc-macro 编译产物（`rust-cache` 默认 `cache-all-crates: false` 清理本地 crate，`deploy` 仅需重编 4 个 workspace crate 及 `wasm-bindgen`/`wasm-opt`，约 20–30s） |
| **`cloudflare-managed-turn` 混合池** | 保留 `weekly.yml` 现有的独立 `Swatinem/rust-cache@v2`（不挂 `shared-key`） | 自身维护 | 自身后续运行（注：周度间隔接近 GitHub 7 天缓存淘汰线，偶尔过期属预期权衡） | 该 job 既编译 `relay-cloudflare` Wasm，又在 Playwright `globalSetup` 中编译原生 Linux debug `relay-native`（~250MB）；独立池核心目标是避免污染高频 `relay-cloudflare-wasm` |
| **Network Authority** | `Swatinem/rust-cache@v2`（`shared-key: network-authority-verify` / `network-authority-deploy`） | `verify` 与 `deploy` 各自维护 | — | `verify` 编 `--dev --features test-release`，`deploy` 编 `--release` + `network-authority-keys`，分开缓存避免相互冲刷 |

### 决策 D：工具链隔离与版本收敛

1. **所有进入 `relay-rust/` 但不需要 lint/coverage 的 Worker job，统一在 job 级 `env` 设置 `RUSTUP_TOOLCHAIN: stable`**：
   - `rustup` 优先级规则为 `RUSTUP_TOOLCHAIN` > `rust-toolchain.toml`，因此配合 `rustup toolchain install stable --profile minimal --target wasm32-unknown-unknown` 后，可彻底阻止进入 `relay-rust/` 时自动下载 `clippy`、`rustfmt`、`llvm-tools-preview`。
   - 覆盖范围：`cloudflare-relay-verify.yml`、`cloudflare-relay.yml`（deploy）、`network-authority.yml`（verify & deploy）、`weekly.yml`（`cloudflare-managed-turn`）。
2. **统一 Node.js 版本为 `24`**：
   - 与现有 `console-bundle.yml`、`donations.yml`、`network-authority.yml` 以及 `web/package.json`（`@types/node: ^24.13.2`）完全对齐，替换 `ci.yml`（5 处）和 `weekly.yml`（5 处）中的 `node-version: latest`（并同步收敛其余 `lts/*` 或保留 `24` 作为全仓唯一标准）。
3. **统一过时 Action 版本至 `@v7`**：
   - 升级 `official-network.yml:48` 的 `actions/setup-go@v6` 为 `@v7`，并顺带升级 `ci.yml:340`（`desktop-windows`）全仓唯一残留的 `actions/upload-artifact@v6` 为 `@v7`。
4. **统一 `workflow_dispatch` 与 `*_ENABLED` 语义**：
   - 全仓明确约定：**`*_ENABLED == 'true'` 仅控制自动触发部署；`workflow_dispatch` 代表操作者显式部署，不受 `*_ENABLED` 静默拦截**（但仍受默认分支检查与必填密钥校验保护，缺失密钥时显式报错）。
   - `cloudflare-relay.yml` 与 `official-network.yml` 均改为支持 `workflow_dispatch` 显式执行（`official-network.yml` 在 `workflow_dispatch` 时需手动输入 40 位 `commit_sha` 与 `vX.Y.Z` 版本号，不存在误点风险，且 `release.yml` 联动调用前已检查 `OFFICIAL_NETWORK_ENABLED == 'true'`）。

---

## 3. 分阶段实施计划

### Phase 1：创建共享 `setup-worker-build` Composite Action

- **新增文件**：`.github/actions/setup-worker-build/action.yml`
- **实现要点**：
  - 输入参数 `version`（默认 `0.8.7`）。
  - 前置 1 步轻量 bash 校验仅支持 Unix/Linux runner（非 Unix 明确失败，防止在 Windows 上静默找错无 `.exe` 路径），并读取 `${ImageOS:-${RUNNER_OS:-unknown}}` 输出 `image_os`（注意 `ImageOS` 仅存在于 shell 环境而非 `${{ env.ImageOS }}` context）。
  - 使用 `actions/cache/restore@v6` 恢复 `${{ runner.temp }}/worker-build/bin`，key 为 `worker-build-v1-${{ steps.env.outputs.image_os }}-${{ runner.arch }}-${{ inputs.version }}`（精确匹配，不做前缀回退）。
  - 缓存未命中时，执行 `cargo +stable install worker-build --version "$VERSION" --locked --root "$RUNNER_TEMP/worker-build"`，并验证 `$RUNNER_TEMP/worker-build/bin/worker-build` 可执行后立即调用 `actions/cache/save@v6` 保存（即使后续测试失败也保留工具缓存）。
  - 将 `$RUNNER_TEMP/worker-build/bin` 追加至 `$GITHUB_PATH`。
  - 全部步骤在 `env` 中声明 `RUSTUP_TOOLCHAIN: stable`，避免误读目录工具链文件。

### Phase 2：解耦 Cloudflare Relay 验证与部署 + 修复 `network-authority` 与 `console-bundle` 缓存

1. **新增 `.github/workflows/cloudflare-relay-verify.yml`**
   - `name: Private relay verification`
   - `on`:
     - `workflow_call`（`inputs.caller_managed: { type: boolean, required: false, default: false }`）
     - `workflow_dispatch`
     - `pull_request.paths`、`push(main).paths`（共享同一组中继/管理台/部署脚本及当前 `cloudflare-relay.yml` `pull_request.paths` 中的**全部 9 条 Go 线合约路径**，将 `.github/workflows/cloudflare-relay.yml` **替换为** `.github/workflows/cloudflare-relay-verify.yml`、`.github/actions/setup-worker-build/**`，不含 `web/**` 与 `deploy/network/**`）。
   - `concurrency`:
     ```yaml
     group: cloudflare-relay-verify-${{ github.workflow }}-${{ github.event.pull_request.number || github.ref }}
     cancel-in-progress: true
     ```
   - `jobs`:
     - `console` 与 `verify` 均设置防 Fork 双跑门控（注意被 `Deploy private relay` 通过 `workflow_call` 调用时 `github.event_name` 仍为 `'push'`，需用 `inputs.caller_managed` 显式区分）：
       ```yaml
       if: >-
         inputs.caller_managed == true ||
         github.event_name != 'push' ||
         vars.CLOUDFLARE_RELAY_ENABLED != 'true'
       ```
     - `console`: 调用 `./.github/workflows/console-bundle.yml`（`app: relay`）。
     - `verify`:
       - `env: { RUSTUP_TOOLCHAIN: stable, GOTOOLCHAIN: local, GOWORK: "off" }`
       - `actions/setup-node@v7`（`node-version: 24`, `cache: npm`, `cache-dependency-path: relay-rust/crates/relay-cloudflare/package-lock.json`）
       - `actions/setup-go@v7`
       - `rustup toolchain install stable --profile minimal --target wasm32-unknown-unknown`
       - `Swatinem/rust-cache@v2`（`workspaces: relay-rust -> .target`, `cache-bin: false`, `shared-key: relay-cloudflare-wasm`, `cmd-format: rustup run stable {0}`）
       - `./.github/actions/setup-worker-build`
       - 运行 `scripts/deploy/cloudflare/*.test.mjs`、`npm ci`、`npm run test:access`。

2. **重构 `.github/workflows/cloudflare-relay.yml`**
   - 保留 `name: Deploy private relay`，移除 `pull_request` 触发器。
   - `push(main).paths` 仅保留部署产物相关路径与 `web/**`，不含 Go 线合约路径，也**不加入** `.github/actions/setup-worker-build/**`（加注释说明分工）。
   - 文件顶部添加注释说明：
     - 面向 Fork 自建用户，有意使用仓库级 Secrets 而非 `environment:`，避免双处配置与 `vars` 作用域陷阱；
     - `CLOUDFLARE_RELAY_ENABLED` 仅控制 `main` 推送自动部署，手动 `workflow_dispatch` 始终执行；主仓保持关闭 `CLOUDFLARE_RELAY_ENABLED` 以确保 `Private relay verification` 覆盖全部 `main` 推送。
   - `jobs.verify`:
     - `if: github.ref == format('refs/heads/{0}', github.event.repository.default_branch) && (github.event_name == 'workflow_dispatch' || vars.CLOUDFLARE_RELAY_ENABLED == 'true') && vars.CLOUDFLARE_DEPLOYMENT_MODE != 'frontend'`
     - `uses: ./.github/workflows/cloudflare-relay-verify.yml`
     - `with: { caller_managed: true }`
   - `jobs.deploy`:
     - `needs: [verify]`
     - `if` 使用 `!cancelled()` 显式处理 `frontend` 模式跳过 `verify` 的情形（既防止 GitHub Actions 因上游 skipped 级联跳过 `deploy`，又阻止已取消工作流在 `frontend` 模式下被 `always()` 误放行）：
       ```yaml
       if: >-
         !cancelled() &&
         github.ref == format('refs/heads/{0}', github.event.repository.default_branch) &&
         (github.event_name == 'workflow_dispatch' || vars.CLOUDFLARE_RELAY_ENABLED == 'true') &&
         (
           (vars.CLOUDFLARE_DEPLOYMENT_MODE == 'frontend' && needs.verify.result == 'skipped') ||
           (vars.CLOUDFLARE_DEPLOYMENT_MODE != 'frontend' && needs.verify.result == 'success')
         )
       ```
     - 将 `RUSTUP_TOOLCHAIN: stable` 提升至 **job 级 `env`**（并移除 deploy step 中的重复声明），保证 `Swatinem/rust-cache@v2` 对 `RUST*` 环境变量的哈希与 `cloudflare-relay-verify.yml` 完全一致。
     - 在 `if: env.CLOUDFLARE_DEPLOYMENT_MODE != 'frontend'` 下接入：
       - `rustup toolchain install stable --profile minimal --target wasm32-unknown-unknown`
       - `Swatinem/rust-cache@v2`（`workspaces: relay-rust -> .target`, `cache-bin: false`, `shared-key: relay-cloudflare-wasm`, `save-if: false`, `cmd-format: rustup run stable {0}`）
       - `./.github/actions/setup-worker-build`
     - `setup-node@v7` 统一 `node-version: 24`。

3. **补齐 `.github/workflows/console-bundle.yml` 的 `pnpm` 缓存**
   - 将 `pnpm/action-setup@v6` 调整到 `actions/setup-node@v7` 之前，并为 `setup-node` 补齐 `cache: pnpm` 与 `cache-dependency-path: consoles/pnpm-lock.yaml`（对齐 `donations.yml`），为 `cloudflare-relay-verify.yml`、`network-authority.yml` 与 `release.yml` 共同提速。

4. **优化 `.github/workflows/network-authority.yml` 与 `hygiene` 门禁**
   - 仅在 `scripts/ci/linux/hygiene.sh` 增加 `python3 -B -m unittest scripts/deploy/network_authority_test.py`（对齐同文件的 `install_web_test.py` 约定，不修改 `scripts/ci/windows/hygiene.ps1` 以避免给 Windows 本地 `make hygiene` 引入 Python 3.11+ 强制依赖），使该单测在每次 PR 和直推 `main` 均受 `ci.yml` `static` 保护。
   - `network-authority.yml` 的 `pull_request.paths` 补充 `.github/actions/setup-worker-build/**`；`push.paths` **不加入** `scripts/deploy/network_authority_test.py` 与 `.github/actions/setup-worker-build/**`（加注释说明，避免纯单测或 CI 缓存脚本改动误触发线上 Authority 重部署）。
   - `verify` 与 `deploy` 两个 job 统一补齐：
     - job 级 `env: { RUSTUP_TOOLCHAIN: stable }`
     - `actions/setup-node@v7` 补齐 `cache: npm` 与 `cache-dependency-path: relay-rust/crates/relay-cloudflare/package-lock.json`
     - `rustup toolchain install stable --profile minimal --target wasm32-unknown-unknown`（替换原 `rustup target add`）
     - `Swatinem/rust-cache@v2`（`workspaces: relay-rust -> .target`, `cache-bin: false`, `cmd-format: rustup run stable {0}`，`verify` 用 `shared-key: network-authority-verify`，`deploy` 用 `shared-key: network-authority-deploy`）
     - `./.github/actions/setup-worker-build`（移除手写 `cargo install worker-build`）。

### Phase 3：为 `go-e2e`、`weekly.yml` 与 `release.yml` 接入共享 `rust-cache` 并修复遗漏

1. **`.github/workflows/ci.yml`**
   - `go-e2e`：在 `Prepare Rust native relay toolchain` 后增加 `Swatinem/rust-cache@v2`：
     - `workspaces: relay-rust -> .target`
     - `cache-bin: false`
     - `shared-key: relay-native-debug`
     - `cmd-format: rustup run stable {0}`
   - `browser-smoke`：将现有 `Swatinem/rust-cache@v2` 对齐到 `shared-key: relay-native-debug` + `save-if: false`（直接消费 `go-e2e` 维护的缓存，不重复写入）。
   - `rust-windows`：将现有 `Swatinem/rust-cache@v2` 对齐到 `shared-key: relay-native-debug`，顺带为周测与发版 Windows job 预热同一份依赖缓存。
   - `desktop-windows`：`actions/upload-artifact@v6` 升级为 `@v7`（其余 6 处均已为 `@v7`）。
   - 5 处 `node-version: latest`（`static`、`desktop-windows`、`web`、`browser-smoke`、`browser-contracts`）以及 `rust` 的 `lts/*` 统一收敛为 `24`，并移除 `rust` job 上的 `check-latest: true`。

2. **`.github/workflows/weekly.yml`**
   - 5 个 Linux `relay-native` job（`integration-linux`、`named-go-long`、`browser-progressive`、`browser-network`、`cross-browser`）与 2 个 Windows job（`integration-windows`、`windows-browser`）：均在 `Prepare Rust native relay toolchain` 后增加 `Swatinem/rust-cache@v2`（`workspaces: relay-rust -> .target`, `cache-bin: false`, `shared-key: relay-native-debug`, `save-if: false`, `cmd-format: rustup run stable {0}`）。
   - `native-output-linux` 与 `native-output-windows`：将现有 `Swatinem/rust-cache@v2` 对齐到 `shared-key: relay-native-debug` + `save-if: false`（消除孤立周度缓存撞 7 天过期与配额占用）。
   - `windows-browser`：将 job `timeout-minutes` 从 `10` 放宽至 `15`（Playwright 内部仍有 4.5 分钟 `globalTimeout` 兜底，防 Windows runner 抖动 + 无缓存下载 Chromium 误杀），并在 `Run the Windows browser and process smoke` 之前增加预编译步骤，**必须显式设置 `working-directory: relay-rust`** 并执行 `rustup run stable cargo build --locked -p windshare-relay-native --bin relay-native`（不可在根目录用 `--manifest-path`，否则 Cargo 不读 `relay-rust/.cargo/config.toml` 会编到 `relay-rust/target` 导致缓存与 Playwright 均失效），将不带 `test-util` 的 `tokio`/`hyper`/`reqwest` 剩余增量编译移出 Playwright 180s 超时框外，且不对高频 `ci.yml` 的 `rust-windows` 施加任何额外负担。
   - `cloudflare-managed-turn`：
     - 增加 job 级 `env: { RUSTUP_TOOLCHAIN: stable }`；
     - **保留现有的独立 `Swatinem/rust-cache@v2`（不切到 `shared-key: relay-cloudflare-wasm` 也不设 `save-if: false`）**，避免其浏览器套件编译的 `relay-native`（~250MB）污染高频 Wasm 缓存；
     - 改用 `./.github/actions/setup-worker-build` 替换裸 `cargo +stable install worker-build`；
     - `setup-node@v7` 统一为 `node-version: 24`。
   - 其余 5 处 `node-version: latest` 统一收敛为 `24`。

3. **`.github/workflows/release.yml`**
   - `linux-ext4` 与 `windows-ntfs`：将现有 `Swatinem/rust-cache@v2` 对齐到 `shared-key: relay-native-debug` + `save-if: false`，发版验证直接复用主分支高频保鲜缓存并释放 ~600MB 孤立缓存配额（保持与 `scripts/ci/release-workflow.tests.sh` 契约兼容）。

### Phase 4：统一 `official-network.yml` 开关语义与 Action 版本

- **`.github/workflows/official-network.yml`**
  - L31 `build.if` 调整为 `github.event_name == 'workflow_dispatch' || vars.OFFICIAL_NETWORK_ENABLED == 'true'`，与 `donations.yml`、`network-authority.yml`、`cloudflare-relay.yml` 四者语义完全一致（`release.yml` 自动联动调用前已有 `if: vars.OFFICIAL_NETWORK_ENABLED == 'true'` 保护）。
  - L48 `actions/setup-go@v6` 升级为 `actions/setup-go@v7`。
  - L64、L105 `node-version: lts/*` 收敛为 `24`（可选，保持全仓 Node 单一基线）。

### Phase 5：同步文档说明

仅更新受影响的精简说明，保持文档与实际行为一致：
- `docs/validation.md`：更新 Cloudflare 验证与部署的职责说明（验证在 `Private relay verification`，部署仍在 `Deploy private relay`）。
- `docs/deploy/cloudflare-relay.md` 与 `docs/deploy/cloudflare-api-token.zh-CN.md`：将首次部署变量表（`cloudflare-relay.md` L31）中的 `CLOUDFLARE_RELAY_ENABLED` 调整为可选（`Variable, optional`），并明确其仅用于开启同步 `main` 时的自动部署，首次手动点击 **Run workflow** 不再被静默跳过。

---

## 4. 验证与验收标准

### 4.1 本地验证（提交前必须全绿）

1. `make workflow-lint`（`actionlint` 校验全部新增/修改的 workflow 与 composite action 语法、表达式类型、`needs` 引用及 `workflow_call` 契约）。
2. `make hygiene`（含新增的 `network_authority_test.py` 及原有部署脚本单测）。
3. `bash scripts/ci/release-workflow.tests.sh`（确认未破坏 release / weekly 契约断言）。

### 4.2 远端 GitHub Actions 验收清单（合入后观察）

| 验收场景 | 预期结果 |
|---|---|
| 仅改 `web/src/**` 直推 `main`（主仓未开 `CLOUDFLARE_RELAY_ENABLED`） | `Private relay verification` **不触发**；`Deploy private relay` 触发但全部 job 在 0 秒内 `skipped`（零 runner 启动）。 |
| 改 `engine/turnissuer/**` 直推 `main` 或提 PR | `Private relay verification` 触发并运行 `console` + `verify`；PR 页面不出现 `Deploy private relay`。 |
| 观察 `cloudflare-relay-verify.yml` / `network-authority.yml` 日志 | 无 `downloading component 'clippy'` / `'rustfmt'` / `'llvm-tools-preview'` 输出；二次运行 `setup-worker-build` 与 `rust-cache` 均命中缓存。 |
| 观察 `ci.yml` 的 `go-e2e` 与 `browser-smoke` | 二次运行命中同一个 `relay-native-debug` 缓存，Rust 中继编译耗时从分钟级降至 15 秒以内。 |
| 手动触发一次 `weekly.yml`（或下周定时运行） | 7 个新增缓存的 job 及 `native-output-linux`/`native-output-windows` 均命中 `relay-native-debug`，无新增孤立缓存条目导致配额激增；`windows-browser` 在 `relay-rust/` 下预编译进 `.target` 后 Playwright 阶段瞬时复用、无 180s 超时风险；`cloudflare-managed-turn` 独立池不污染 `relay-cloudflare-wasm`（注：周度间隔接近 7 天淘汰线，偶发过期属预期）。 |
| 在测试 Fork（`CLOUDFLARE_RELAY_ENABLED` 未设置）手动点击 **Deploy private relay** | 不再静默跳过，而是按预期执行验证并在缺少密钥时给出明确提示（配置密钥后完成部署）。 |
| 在启用 `CLOUDFLARE_RELAY_ENABLED=true` 的测试 Fork 同步含中继改动的 `main` | `Private relay verification` 直接跳过（0 runner），仅 `Deploy private relay` 统一执行一次验证并完成部署；手动触发 `Private relay verification` 可独立完成纯校验。 |

---

## 5. 风险与应对

1. **`needs` 级联跳过与取消竞态风险**：`cloudflare-relay.yml` 的 `deploy` 在 `frontend` 模式下依赖被跳过的 `verify`，必须通过 `!cancelled()`（而非 `always()`）结合精确的 `needs.verify.result` 分支判断，既避免 `frontend` 模式被误跳过，也防止已取消工作流在 `frontend` 模式下或 `verify` 失败时误放行。该逻辑需经 `actionlint` 检查并核对逻辑真值表。
2. **缓存配额挤压与键一致性风险**：只读复用者（含 `cloudflare-relay.yml` `deploy`、`browser-smoke`、`weekly.yml` 与 `release.yml` 消费者）均显式配置 `save-if: false`；单点写入者及 `desktop-windows` 等独占缓存不额外设 `save-if: main`（`Swatinem/rust-cache` 在 PR 未改 `Cargo.lock` 时命中精确键自动跳过上传，仅改依赖的 PR 写入分支隔离缓存以保留迭代速度，PR 关闭后由 GitHub 自动回收）；混合构建的 `cloudflare-managed-turn` 保持独立缓存避免污染 `relay-cloudflare-wasm`；共享 `relay-cloudflare-wasm` 的所有 job 均在 **job 级 `env`** 统一设置 `RUSTUP_TOOLCHAIN: stable`，且 `windows-browser` 预编译必须设 `working-directory: relay-rust` 以命中 `.cargo/config.toml` 的 `.target`。
3. **复用工作流并发、事件名陷阱与主仓不变量**：`cloudflare-relay-verify.yml` 的并发组包含 `${{ github.workflow }}` 并统一采用 `cancel-in-progress: true`（与 `native-output.yml` 一致），独立运行与部署调用分属不同组且后者由 `cloudflare-relay.yml` 串行保护，规避顶层 `concurrency` 依赖 `inputs` 的风险；防 Fork 双跑门控通过 `workflow_call` 显式入参 `caller_managed` 区分，避免因 `workflow_call` 继承调用方 `github.event_name == 'push'` 导致误跳过部署前校验。**主仓不变量**：主仓本身不可开启 `CLOUDFLARE_RELAY_ENABLED=true`，否则主仓仅改 Go 线合约路径直推 `main` 会在 `Private relay verification` 中被跳过。
4. **分支保护规则检查**：若仓库 GitHub Branch Protection / Rulesets 强绑定了旧工作流来源（`Deploy private relay`）的检查项，合入后需同步确认指向 `Private relay verification`。
5. **明确不做的边界**：本计划不修改 `cloudflare-relay.yml` 的仓库级密钥模型（P2-1），不改名 `Deploy private relay`（P2-3），不为 `go-test-coverage` 额外引入复合缓存动作（P1-3），不在 `hygiene.ps1` 引入 Python 强依赖，以保持自建体验与本地开发体验在最优平衡点。
