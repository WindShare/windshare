# Playwright 修复执行计划

> 状态：**执行基线**  
> 更新时间：2026-07-28。  
> 目标：修复浏览器 Playwright 门禁，恢复真实能力语义和可审计诊断。

## 1. 目标与判定语义

- 门禁覆盖真实 Chromium、Firefox、WebKit、16 MiB 传输、relay、WebRTC、relay cut 和输出 bytes/hash；主 Playwright 配置与独立 Pion/WebRTC 配置都必须执行相同的浏览器判定。
- 每个样本输出四个正交结果，禁止用单一布尔值代替：

  | 字段 | 值 |
  |---|---|
  | `rtcCapability` | `unavailable`（API 不存在）、`unusable`（API 存在但最小 DataChannel offer 探针失败）、`available`（探针成功） |
  | `peerAttemptOutcome` | `not-started`、`admitted`、`failed` |
  | `deliveryOutcome` | `succeeded`、`failed` |
  | `runtimeOutcome` | `healthy`、`crashed`、`infrastructure-failed`、`unknown` |

- `available` 必须开始真实 attempt；只有 authenticated lane admission、允许的 direct selected pair、relay cut fence 后的新 peer block 和完整 bytes/hash 均成立才通过。
- `unavailable` 只能来自 API 缺失，并必须完成同一负载的精确 relay fallback。`unusable`、`available + failed`、crash 和基础设施失败均阻塞；relay 最终成功不能掩盖 peer 失败。
- 所有引擎使用同一分类器，不按浏览器名称预设能力。
- 本计划只关闭浏览器 Playwright 门禁；core 覆盖率、core-release、Linux/ext4 和完整 R7 的图片/MP4 range/seek 取证仍需各自通过，不能由本计划宣称完成。

## 2. 当前基线

- Ubuntu runner 使用真实 Chromium、Firefox、WebKit，Playwright `1.61.1`、`workers=1`、`retries=0`；场景启动真实 `windshare`/`wsrelay`，传输 16 MiB 并验证 relay、WebRTC、relay cut 和输出 hash。
- 最新运行 [30321334065](https://github.com/WindShare/windshare/actions/runs/30321334065) 中，Firefox 的 `peerCapable=true` 但没有 peer lane admission；write barrier 等待约 `15.15s` 后超时，传输以 `0` 字节 `Aborted` 结束。相同类别的 Firefox no-peer-route 和 WebKit 崩溃也在[较早运行](https://github.com/WindShare/windshare/actions/runs/30242095797)出现。
- `peerCapable` 当前只是 API 探测，不代表连接或 admission；连接策略会把 peer 失败限定为 attempt 级并保留 relay，因此测试必须先收到明确的 peer outcome，不能让 barrier 超时制造假象。
- 当前失败已经产生 trace、screenshot、video 和 Playwright 自动 `error-context.md`；这些产物对排查有价值，CI 应保留并上传。保护边界只针对真实 GitHub/runner/repository secret，fixture 生成的 capability URL、key、SDP 和 ICE 数据属于合成诊断数据。

## 3. 已确定的决策

| ID | 决策 | 约束 |
|---|---|---|
| P1 | 三个引擎都按动态能力阻塞 | `available` 必须通过 direct hot-switch；`unavailable` 必须通过 relay fallback；其他结果失败 |
| P2 | PR 冻结 `TestIceTopology` | 明确测试接口/地址族、browser RTC 配置、Pion `PeerConnectionFactory`/`SettingEngine` 和 candidate admission；仅设置 `iceServers: []` 不算受控拓扑 |
| P3 | direct 与 TURN 分开判定 | PR selected pair 的两端 candidate type 只能是 `host`/`prflx` 且均不得为 `relay`；协议和地址族必须记录。coturn、公共 STUN、受限网络放入非阻塞 scheduled/manual 矩阵 |
| P4 | 产品 fallback 与能力门禁分层 | peer attempt 失败可以继续 relay，但必须报告 typed failure，不能静默把能力失败计为通过 |
| P5 | 全部等待事件驱动 | attempt 只由 `admitted`/`failed` 终结；`fallback-active` 只是 delivery 事件，不能结束 peer barrier。timeout 必须由命名的 peer/lane/transfer deadline 推导 |
| P6 | CI 保留诊断产物并保护真实 secret | 上传前扫描显式注入的 secret、GitHub token 格式及解包后的 trace ZIP；命中时隔离附件，不改变测试判定 |
| P7 | 两套配置职责分离 | 主配置验证产品 hot-switch/fallback；Pion 配置验证原生 adapter/interop；两者共享能力分类器，由 CI 聚合最终结果 |

## 4. 执行阶段

### Phase 0：恢复可观测的 peer attempt 状态

- 将现有分散的 peer error、signaling 和 lane admission 回调整合为可选的 consumer-side `V2ConnectivityObserver`，贯通 connectivity → supervisor → gateway；不修改 v2 协议。
- capability probe 单独产出 typed result，不伪造 `attemptId`。attempt event 使用 stage-specific union：
  - 公共字段：`sessionId`、`generation`、`peerPathId`、`attemptId`、`side`、`sideSequence`、`elapsedMs`、`stage`。
  - `laneId` 仅从 `lane-granted` 开始；candidate 计数按可用阶段记录；selected pair 仅在 admission 前读取；`failureScope`/`typedErrorCode` 仅属于 `failed`。
  - stage 固定为 `started`、`offer-sent`、`answer-received`、`datachannel-open`、`lane-granted`、`lane-attached`、`admitted`、`failed`；只有最后两者是 attempt 终局。
- `sideSequence` 只保证单侧单调；fixture collector 另加接收序号，不比较跨进程时钟。receiver 使用进程内 observer，Go sender 通过结构化 JSONL stderr 提供同一 correlation ID，不新增协议字段。
- 保留并向上透传具体 peer operation code，不得统一丢成 generic error。
- 第一批 relay block 后进入测试 barrier：`admitted` 后执行 relay cut fence 再释放；`failed`/`unavailable`/`unusable` 直接释放并让 relay 完成；`fallback-active` 不释放 barrier。最终同时等待 peer 和 delivery 终局，再执行断言。
- 断言 observer 抛错隔离、barrier 取消、fallback、lane 和 session 清理，不产生未处理 rejection。

**完成条件：** Firefox 日志能区分 ICE/negotiation、lane grant、lane attach、admission 和测试同步问题；relay 健康时不会因 peer attempt 失败而被测试主动 abort。

### Phase 1：建立确定性的 PR ICE 环境

- 通过 gateway/fixture 贯通 `BrowserOfferChannelFactory` 已有的 `RTCConfiguration` 注入；为 CLI/App 增加 consumer-side peer factory 注入，使 E2E fixture 可提供受控 Pion API/`SettingEngine`，生产继续使用默认配置。
- 冻结 `TestIceTopology` 的接口、地址族和 candidate admission。若受控 runner 不能让某引擎建立允许的 direct pair，则该阶段未完成，不能 skip 或改判 unavailable。
- admission 前使用 `getStats()` 归一化 local/remote candidate type、protocol、address family 和 selected pair；缺少可验证 pair 时失败。coturn 证据必须标为 TURN，不能冒充 direct。
- focused PR evidence 每个引擎运行 3 个独立 Playwright 进程，`retries=0`；每个进程只跑一个样本并创建全新 browser/context。不得在单个 test/page 内循环样本。

**完成条件：** 受控拓扑下 Chromium/Firefox 三次均观察到 `relay → admitted peer → relay cut fence → post-fence peer block`，并完成 bytes/hash；失败时存在 stage/code evidence。

### Phase 2：按能力语义修复并验证 WebKit

- WebKit 同样每个样本使用独立 Playwright 进程；采集 `page.crash`、`pageerror`、console、`browser.disconnected`、runner exit code/stderr，不依赖 Playwright private fields。
- 使用共享分类器区分 API 缺失、探针失败和 admission；不能用浏览器名称或一次异常归类为 unavailable。
- `unavailable` 必须完成 relay fallback、bytes/hash 和 transfer terminal；`unusable`、attempt failed、`Target crashed`、资源/基础设施异常均阻塞。
- 保留 trace、video、screenshot、`error-context.md` 和 stderr，按第 6 节 guard 后上传。

**完成条件：** WebKit 的每次失败都能明确归类为 capability、浏览器进程、资源、应用或基础设施问题；能力缺失时 fallback 证据完整，能力存在时按 hot-switch 语义验收。

### Phase 3：收敛 CI、脚本和证据

- 主配置负责 hot-switch/fallback；Pion 配置负责 adapter/interop。两者运行三引擎并共享分类器；Pion 遇到 `unavailable` 时输出显式 not-applicable evidence，最终通过仍必须由主配置的 relay fallback 证据支撑，不能静默 skip。
- CI 将两套配置作为独立 job 或 `if: !cancelled()` 步骤执行，始终运行 guard/upload，最后由聚合步骤按四个 outcome 判定；一个 suite 失败不能阻止另一个产生诊断。
- 更新 `.github/workflows/ci.yml`、Linux/Windows browser scripts 和 `make browser`，保持项目、样本、判定和产物一致。本地只保留本地 artifact。
- 新增非阻塞 scheduled/manual browser network matrix：scheduled 对公共 STUN、受限 UDP 和 coturn 各运行 5 个独立样本；真实 NAT 仅在声明拓扑的 manual/self-hosted runner 执行。结果不改变 PR 判定。
- 先运行 focused evidence，再运行 `make browser`；`make ci` 独立报告其他门禁。GitHub 记录实际 checkout SHA；本地结果不能替代 Ubuntu runner。
- peer 能力相关 spec 禁止 skip、retry、提高全局 timeout 或关闭断言；timeout 只能来自命名 deadline。

## 5. 验收矩阵

| `rtcCapability` | `peerAttemptOutcome` | 阻塞门禁必须验证 |
|---|---|---|
| `available` | `admitted` | direct selected pair；`relay→admitted peer→cut fence→post-fence peer dispatch`；完整 bytes/hash；runtime healthy |
| `available` | `failed` | 阻塞失败并保留 stage/code；relay 健康时仍完成 fallback bytes/hash 后再断言失败 |
| `unavailable` | `not-started` | 同一负载的精确 relay fallback、完整 bytes/hash、runtime healthy |
| `unusable` | `not-started` | 阻塞失败并保留 probe evidence；relay 健康时仍验证 fallback |
| 任意值 | 任意值 | crash、基础设施失败、unknown、delivery failure 均阻塞，不能改判 unavailable |

relay cut 使用异步 `cutAndWait` fence；它只在 proxy 停止接入且 receiver 已观察 relay lane detached/ineligible 后完成。route evidence 必须记录 block dispatch sequence；cut 前已派发的 relay block 可在 cut 后完成，但 fence 后不得新增 relay dispatch，且必须出现 fence 后的 peer dispatch。

## 6. 验证入口与产物

| 环境 | 入口 | 说明 |
|---|---|---|
| Ubuntu/GitHub | `make browser` | 三引擎、focused evidence、主配置和 Pion 配置；CI 内部独立执行并聚合 |
| Windows 本地 | `make browser` | D5 BrowserTests 与 Pion 配置均执行；防火墙提示不参与 verdict |
| 总门禁 | `make ci` | 独立报告 core/Go/Web 全部结果，不改变本计划的浏览器判定 |

CI artifact 策略：

- 失败时允许上传 Playwright trace、video、screenshot、`error-context.md`、console 和 browser stderr；这些产物是排查 Firefox/WebKit 问题的必要证据。
- 允许 fixture 生成的 capability URL、key、SDP、ICE candidate 和文件内容摘要；它们不得来自生产链接或 job secret。
- 上传前只检查显式注入且非空的 GitHub/runner/repository secret，并补充 GitHub token 格式。ZIP 必须解包逐项扫描；命中时隔离整个附件并只记录文件名和规则名，不输出 secret；测试判定不变。
- 不要求对普通诊断数据做过度脱敏；目标是防止 GitHub 密钥泄漏，而不是牺牲 CI 排查能力。

## 7. 不在本计划内

- `core` 覆盖率、core-release 路径和 Linux/ext4 认证；它们是独立 CI 工作线。
- 修改 v2 协议、lane admission 语义或生产 STUN/TURN 默认策略。
- 图片与 MP4 的独立 range/seek 全量取证；本计划不以 hot-switch 通过宣称完整 R7 已完成。
- 通过提高全局 timeout、增加重试次数、跳过浏览器或把崩溃改成非阻塞来改变失败判定。
