# Playwright 修复执行计划

> 状态：**执行基线**
> 更新时间：2026-07-29。
> 目标：恢复真实浏览器能力语义，同时保持本地门禁可运行、可诊断、可维护。

## 1. 产品判定语义

每个样本输出四个正交结果：

| 字段 | 值 |
|---|---|
| `rtcCapability` | `unknown`、`unavailable`、`unusable`、`available` |
| `peerAttemptOutcome` | `not-started`、`admitted`、`failed` |
| `deliveryOutcome` | `not-started`、`succeeded`、`failed` |
| `executionOutcome` | `healthy`、`crashed`、`infrastructure-failed`、`unknown` |

- 所有引擎使用同一动态分类器，禁止按浏览器名称预设能力。
- `available` 必须开始真实 peer attempt，并验证 direct selected pair、`relay → peer admission → relay cut fence → post-fence peer block`、16 MiB 完整 bytes/hash。
- `unavailable` 只能来自 API 缺失；产品必须在创建 binding/attempt 前执行同一 gate，并完成相同负载的精确 relay fallback。
- API 存在但探针失败为 `unusable`。产品可继续 relay 交付，但门禁必须失败，不能用 relay 成功掩盖 peer 回归。
- crash 或基础设施失败保留 `unknown`/`not-started`，不得伪造为能力不可用或 delivery failure。
- 样本保留完整 `attempts[]`。无 attempt=`not-started`，任一 attempt failed=`failed`，否则至少一个 admitted=`admitted`。
- peer 能力相关 spec 禁止 skip、retry、提高全局 timeout 或关闭断言。

## 2. 稳态门禁架构

| 组件 | 职责 |
|---|---|
| `browser-contract` | schema、reducer、guard、workflow contract；纯单元测试，只运行一次 |
| `browser-main` | 三引擎各 1 个独立产品样本；随后运行排除 focused spec 的 main remainder suite |
| `browser-pion` | 三引擎各 1 个独立 adapter/interop 样本；随后运行排除 focused spec 的 Pion remainder suite |
| `browser-verdict` | `if: always()` 聚合两套结果；只使用 Node 标准库，不安装 pnpm 或浏览器，超时 10–15 分钟 |
| `windows-browser-process` | 只验证 Windows Job Object、Named Pipe 和真实进程树清理 |

`browser-main` 与 `browser-pion` 在 GitHub 并行，在本地顺序运行以避免端口、D5 lease 和机器资源竞争。两套 suite 使用独立 topology lock 和 artifact namespace。

Pion suite 只证明 adapter/interop，不重复完整产品传输。RTC API 不存在时输出 not-applicable；最终通过必须由 main suite 的精确 relay fallback 样本支撑。

## 3. 样本层级与本地入口

- 本轮修复关闭：每个引擎、每个适用 suite 连续运行 3 个独立进程，全部通过。
- 长期 PR/push 与 `make browser`：每个引擎、每个 suite 运行 1 个独立样本，不重试。
- `make browser-stability`：每个引擎、每个适用 suite 运行 5 个独立样本，用于 scheduled/manual 稳定性检查，不改变 PR verdict。
- focused sample 与 remainder suite 必须互斥，禁止重复执行同一 spec。

本地入口：

| 入口 | 语义 |
|---|---|
| `make check` | 快速静态检查和单元测试，不构成完整 CI 证明 |
| `make browser` | 无 Docker 的完整本地 browser gate；contract、main、Pion、guard、verdict 各执行一次 |
| `make ci` | 当前平台可执行的全部阻塞型 PR/push 门禁，并恰好包含一次 `make browser` |
| `make browser-stability` | 5-sample 稳定性入口 |
| `make browser-network` | 环境相关的非阻塞网络矩阵 |

单台机器无法复现 GitHub 的 Linux/ext4 与 Windows runner 联合集合；本地门禁保证命令和判定契约一致，GitHub runner 仍是对应平台的最终证据。

目标 warm runtime：`make check` ≤5 分钟、`make browser` ≤20 分钟、`make ci` ≤30 分钟。安装、构建和 browser preflight 在每个 runner 内各执行一次，并输出阶段耗时。

## 4. 实施阶段

### Phase 0：恢复 peer attempt 可观测性

- 浏览器增加 consumer-side `V2ConnectivityObserver`；Go `v2peer` 增加独立 observer，由 CLI 投影为版本化 JSONL。它们不修改 v2 协议。
- capability probe 独立、限时并在 `finally` 关闭临时 PeerConnection；它只分类，不控制产品。
- 产品分配 `attemptId` 后立即发出 `started`，所有退出路径产生唯一 `admitted`/`failed` 终局并保留 typed error code。
- evidence envelope 使用 `schemaVersion`、`sessionId`、`peerPathId`、`attemptId`、`side`、`sideSequence`、`attemptElapsedMs`、`stage`。只校验单侧顺序，不比较跨进程时钟。
- lane 使用 `{laneId, laneEpoch}`；dispatch 前分配单调 `dispatchSequence`。完成态事件不能替代 dispatch 证据。
- barrier 只由 `admitted`/`failed` 或能力终局释放；`fallback-active` 不是 peer terminal。最终同时等待 peer、delivery 和 runtime 终局。

完成条件：Firefox 日志能区分 ICE/negotiation、lane grant、lane attach、admission 和测试同步失败；relay 健康时不会因 peer 失败被测试主动 abort。

### Phase 1：确定性 PR ICE 拓扑

- 定义版本化 `TestIceTopology` profile，并通过测试专用 CLI 入口向共享 App 注入 peer factory；生产入口始终使用默认 provider。
- browser fixture 与 Pion 使用同一 profile、地址族和 candidate policy。仅设置 `iceServers: []` 不构成受控拓扑。
- 先让 Chromium/Firefox 各 3 个独立 browser↔Pion 样本通过，再接入真实 WindShare hot-switch。
- browser 和 Pion 证据必须绑定同一 `attemptId`。selected pair 两端只允许 `host`/`prflx`，不得为 `relay`；地址族由 Pion 侧确认。
- 产品接入后，Chromium/Firefox 各 3 个独立样本必须完成 relay cut 后 peer dispatch 和最终 bytes/hash。

### Phase 2：WebKit

- WebKit 先运行 3 个独立 browser↔Pion 样本，再运行 3 个产品样本。
- 采集公共 `page.crash`、`pageerror`、console、`browser.disconnected`、runner exit code/stdout/stderr，不读取 Playwright private fields。
- `unavailable` 必须完成 relay fallback；`unusable`、attempt failed、crash 或基础设施异常均阻塞。

### Phase 3：收敛 CI 与 runner

- 删除 hosted OCI image authority、dependency-volume sealing、nonce retirement、containment validation、空 topology readiness 和多层 publication authority。
- 将 focused specs 从 main/Pion remainder suite 中排除；contract tests 不再由 hygiene、main、Pion 重复执行。
- GitHub 使用 `browser-contract → {browser-main, browser-pion} → browser-verdict`；Windows process integration 独立并行。
- Browser gate 对所有非纯文档 PR/push 运行。普通 `ci.yml` 不响应 schedule。
- 在真实 fixture 完成后新增独立 `browser-network-matrix.yml`，仅响应 schedule/manual；公共 STUN、restricted UDP、coturn 各 5 个样本，real NAT 仅在声明拓扑的 operator-provisioned host 运行。结果非阻塞。
- Network matrix CLI 不推断 helper：先运行 `pnpm --dir web build:browser-network-matrix-helpers -- ABSOLUTE_NEW_OUTPUT_DIR`；`execute`/`aggregate` 必须显式传 `--publisher-helper`，Windows 另传 `--windows-job-helper`，并按 helper manifest 的绝对路径和 SHA-256 复核。
- 阻塞门禁和本地完整入口不得依赖 Docker；当前不保留 Docker 复现子系统。

## 5. Artifact 边界

- 父 runner 在启动 Playwright 前原子写 provisional `result.json`，确认子进程树退出后写 final result；页面和测试子进程不拥有最终文件。
- Playwright、sample runner 和测试子进程不得获得 `GITHUB_TOKEN` 或 repository secret。
- 独立 guard 拒绝 symlink、路径越界、超限和非法 ZIP，扫描显式注入的真实 secret 值及 GitHub token 格式。
- guard 将获准附件复制到全新私有 staging，写入单一 manifest 和 `guard.json`，再原子封存 upload 目录；只上传该目录。
- `guardOutcome` 独立于产品 outcome。`quarantined` 和 `failed` 都阻塞最终 verdict，但不得改写样本的四个产品结果。
- fixture 生成的 capability URL、key、SDP、ICE candidate 和内容摘要属于合成诊断数据，可在通过 guard 后上传。
- Workflow 使用 `permissions: contents: read`，checkout 设置 `persist-credentials: false`；真实 secret 只在独立 guard/finalize step 注入。

## 6. 验收矩阵

| `rtcCapability` | `peerAttemptOutcome` | 结果 |
|---|---|---|
| `available` | `admitted` | direct pair、hot-switch、cut fence、post-fence peer dispatch、bytes/hash 和 healthy runtime 全部成立 |
| `available` | `failed`/`not-started` | 阻塞；relay 健康时仍完成 fallback 后再报告 peer 失败 |
| `unavailable` | `not-started` | 精确 relay fallback、bytes/hash 和 healthy runtime 成立 |
| `unavailable` | `admitted`/`failed` | 阻塞 capability gate 契约错误 |
| `unusable`/`unknown` | 任意 | 阻塞并保留 probe、runtime 和 fallback 证据 |
| 任意 | 任意 | crash、基础设施失败、delivery 未开始/失败或 guard 非 passed 均阻塞 |

relay cut 使用异步 fence：receiver 确认 relay lane detached/ineligible 后才完成；cut 前 dispatch 可在 cut 后结束，但 fence 后不得产生新 relay dispatch，且必须出现 peer dispatch。

## 7. 不在本计划内

- core 覆盖率、core-release、Linux/ext4 认证和完整 R7 图片/MP4 range/seek 取证。
- 修改 v2 协议、lane admission 语义或生产 STUN/TURN 默认策略。
- 防御同账户恶意进程、供应链攻击，或证明 OCI image/volume 已销毁。
