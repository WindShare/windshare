# 中转选路、连接观测冷却与冷启动排序改造计划

## 1. 背景与现状分析

发送端从签名目录选出最多 4 个候选并行注册，首个成功即可发布链接，后台目标是维持 2 个注册并兼顾区域、厂商多样性。链接发布后，恢复只能使用已写入的中转。见 [relayplan](../../../connectivity/relayplan/plan.go)、[relayset](../../../connectivity/relayset/admission.go) 和 [链接发布](../../../engine/internal/networkruntime/resolve.go)。

### 1.1 冷却与失败归因

- 规划器会硬过滤 `CooldownUntil`，运行时却从未为它赋值。当前目录只有 `cn1`、`us1`，WAN 或代理恢复也未必改变网络代次。直接补写冷却会让已发布链接在整个分享期间少一个备选，两台都被过滤时还可能返回 `NoRelayCandidates`。[规划过滤](../../../connectivity/relayplan/plan.go) · [观测写入](../../../engine/internal/networkruntime/diagnostics.go)
- HTTP 握手丢失状态码与 `Retry-After`，普通网络唤醒还能提前结束重试等待。服务端停服、预算耗尽和瞬时满载需要不同的重试行为。[握手错误](../../../transport/relayv2/upgrade.go) · [重试等待](../../../connectivity/senderrelay/recovery.go)
- `ErrorAccessDenied` 也可由单个 `ShareID` 被封禁引起，`ErrorInvalidProof` 可由本次分享的恢复凭据不匹配引起。将这些拒绝写入节点健康表会影响同账号的其他分享。[拒载范围](../../../relay-rust/crates/relay-access/src/restrictions.rs) · [恢复拒绝](../../../relay-rust/crates/relay-core/src/lifecycle/effect.rs)
- 首次网络快照的代次 `0 → 1` 会丢弃跨越该时刻的注册观测。预留多样性备选时，`BackupIndex` 也可能挑中刚失败的节点。[网络代次](../../../connectivity/networkstate/snapshot.go) · [备选选择](../../../connectivity/relayplan/plan.go)

### 1.2 `CapacityMbps` 与实测 `Duration` 优先级失衡，冷启动平局退化为字典序

- 两个节点均有成功样本时，现有比较器先看标称带宽。即使 `us1`（100 Mbps）注册耗时为 130 ms、`cn1`（200 Mbps）为 950 ms，`cn1` 仍排第一。[现有排序](../../../connectivity/relayplan/plan.go)
- 5–10 Mbps 和 50–200 Mbps 节点适合不同负载。同档内可用实测注册耗时排序，但当前 `AttemptParked.Duration` 还包含退场时关闭连接的时间。[容量设计](global-network.md#L180-L201) · [注册计时](../../../connectivity/senderrelay/recovery.go) · [停放连接](../../../connectivity/senderrelay/recovery.go)
- 同级节点以 `Identity` 字典序打破平局，目录超出候选上限后，冷启动客户端会反复选中相同端点。[比较器](../../../connectivity/relayplan/plan.go)

### 1.3 明确延后的低 ROI 项（不纳入本计划）

遵循 [global-network.md](global-network.md#L125-L128) 的测量原则，以下两项延后。

- **冷启动出口提示（`ExitHint` / `/.well-known/windshare-edge`）**。当前两台目录节点均可参加竞速。待 `>=50 Mbps` 主力节点超过 4 台且实测跨洲首连偏慢，再在桌面端规划模式评估非阻塞探测。届时统一 `region` 格式，大陆出口返回 `zone=cn, region=as`。
- **逐节点 P0 先验权重**。签名目录严格校验字段，Go、Rust 和 TypeScript 共用契约。待有晚高峰实测数据再引入静态权重。[目录校验](../../../core/nodedirectory/verify.go)

---

## 2. 设计原则与高频路径权衡

1. 正常分享用已有目录快照直接竞速，首个注册成功即发布。`Plan()` 只在选出目录节点后调用现有的 `NewShareAuthorization(ctx)`。
2. 本地冷却只影响候选优先级与备选启用时机。服务端明确给出的重试期限约束对应端点，其他节点继续注册。
3. `relayplan` 做纯计算，`networkruntime` 管目录节点健康，`senderrelay` 管本次注册与重试。凭证或分享级拒载留在本次分享的该节点，确证的账号封禁、吊销由账号服务处理。

---

## 3. 核心设计方案

### 3.1 握手退避与节点健康

1. **显式 `Retry-After`**

   - `WebSocketDialError` 保留 `StatusCode`，仅对 `429`/`503` 解析 `RetryAfter`。支持秒数和 HTTP-date，过去日期归零，秒数在换算前钳制以防溢出。[upgrade.go](../../../transport/relayv2/upgrade.go)
   - `senderrelay` 统一处理 HTTP 和 WS2E 的显式期限，沿用 `2 * senderRelaySlowRetry` 上限。普通 `Wake()` 可提前结束本地等待，重拨前仍须检查该端点的服务端截止。无提示的 503 沿用现有快慢重试节奏。分享注册、节点健康和准入共用引擎时钟，显式等待跨过 55 秒时进入等待状态。原生接收端按端点保留 HTTP/WS2E 截止，其他端点可继续重连。[recovery.go](../../../connectivity/senderrelay/recovery.go)
   - 原生中转停服或公共日预算用尽时，在升级前向公共请求返回 `503` 和 `Retry-After: 30`。仅在预算耗尽且归属未知时查持久化的分享记录，以便重启后的 owner 分享仍能无 bearer 恢复和接收。若预算在升级之后耗尽，WS2E 拒绝帧也受硬限额约束，客户端按实际收到的响应或断连处理。[HTTP 入口](../../../relay-rust/crates/relay-native/src/http/mod.rs) · [预算归属](../../../relay-rust/crates/relay-native/src/node/budget/egress.rs) · [注册授权](../../../relay-rust/crates/relay-native/src/actor/access.rs)

2. **规划输入**。`CooldownUntil` 是本地避让截止，`ServerRetryAt` 是目录端点通过 HTTP 明示的最早重试时间。规划器只读取事实，计数器留在运行时。

```go
type CooldownReason string

const (
    CooldownNone             CooldownReason = ""
    CooldownServerBackoff    CooldownReason = "server_backoff"
    CooldownNodeRejected     CooldownReason = "node_rejected"
    CooldownTransportFailure CooldownReason = "transport_failure"
)

type Observation struct {
    Identity          string
    NetworkGeneration string
    At                time.Time
    Admitted          bool
    Duration          time.Duration
    CooldownUntil     time.Time
    CooldownReason    CooldownReason
    ServerRetryAt     time.Time
}
```

3. **健康表**。存储从 `relayplan` 迁入 `engine/internal/networkruntime/health.go`，删除原存储文件。`RecordAttempt` 与 `Snapshot` 按代次、端点原子读写，沿用 `ObservationAge` 与 `nodedirectory.MaximumNodes` 上限。[节点健康表](../../../engine/internal/networkruntime/health.go)

| 观测 | 跨分享处理 |
|---|---|
| HTTP `429` / `503` | `server_backoff`，首次即本地避让，阶梯为 15 / 30 / 60 / 120 秒。有显式期限时单独记录 `ServerRetryAt`。 |
| 直接 404 且带基址修正提示的 `WebSocketRouteError` | `node_rejected`，按 60 / 120 / 300 秒避让。 |
| 其余传输失败（DNS、TCP、TLS、套接字、拨号超时等） | `transport_failure`，首次只降序，从第二个计数窗口起按 15 / 30 / 60 / 120 秒避让。 |
| WS2E 拒绝（含 `ErrorAccessDenied`、`ErrorInvalidProof`、`ErrorAdmission`、`ErrorAccessUnavailable`）、归因不明的 HTTP 403/404 或 `ErrProtocol`、用户取消 | 保留本次注册的错误与恢复语义。WS2E 明示的等待由当前注册执行，这些结果不写入节点健康表。 |

每 10 秒最多计一次失败。只有计数递增才更新 `lastCountedAt` 并计算本档冷却，同窗重复失败只可延长服务端明示的截止。从 `lastCountedAt`、`CooldownUntil`、`ServerRetryAt` 中最晚的时刻起算，静默 3 分钟后重置连败数。内部保留衰减起点，规划和诊断分别投影尚有效的软、硬截止。

4. **成功与代次**。握手耗时从实际拨号计到 `Dial` 成功返回。之后的 `Admit()`、停放与关闭耗时只计入整次尝试。`AttemptSucceeded` 和 `AttemptParked` 均写入这次成功样本并清除本地连败与软冷却。
   `Observer()` 只为 `Selection.Plan.Network` 写健康表。按 `AttemptStarted` 的网络代次归属结果，首次快照 `0 → 1` 视为同一代，真实切网时清空表并忽略迟到的旧回调。配置中转仍输出注册诊断。

### 3.2 规划器两级软冷却、异步目录唤醒与多样性备选修复

配置中转优先占位。目录中重复、无 `relay` 角色、`draining` 或策略拒绝的节点记入 `Decision.Filters`。其余分为 `ready` 和 `cooled`，后者指 `CooldownUntil` 或 `ServerRetryAt` 尚未到期。目录名额 `available = min(MaximumNetworkCandidates, MaximumEndpoints - len(configured))`，注册目标 `target = min(TargetNetworkRegistrations, available)`。

| 剩余目录候选 | 选择与拨号 |
|---|---|
| `len(ready) >= target` | 只从 `ready` 选择，最多 `available` 个，正常并行竞速。浏览器打开链接会尝试所有入选端点，已有足够备选时无需再补冷却节点。 |
| 只有 1 个 `ready`，`target == 2` | 立即拨这个节点，从 `cooled` 最多补 1 个延迟备选写入同一链接。备选到期后可注册。若首选失败、退避或等待授权，可提前越过本地软冷却。 |
| 没有 `ready`，但有 `cooled` 且有名额 | 置 `Decision.CooldownFallback = true`。本地软冷却允许立即有限并发竞速。若全部带有效的服务端期限，保留候选，在最早期限到达时自动拨号。 |

- 补位和兜底先选允许立即拨号的节点。若都须等待，先选 `ServerRetryAt` 最早者。同组内优先 `transport_failure` / `server_backoff`，再按 `CooldownUntil` 和常规顺序选择。备选在同类节点中预留区域、厂商多样性，允许 1 秒以内的到期偏差。全冷却兜底忽略本地截止之间的偏差，实际拨号仍遵守各端点的服务端期限。
- 保留 `BackupIndex(primary, candidates, policy)`。常规备选优先从 `rank > 0` 的节点选择，其余名额保留现有的未测节点探索。补入的冷却备选随链接发布，只有因冷却跳过的节点才记 `reason: "cooldown"`。[链接发布](../../../engine/internal/networkruntime/resolve.go) · [浏览器首次连接](../../../web/src/receiver/relay-selection/race.ts)
- 等待中的备选暂不占拨号名额，也不参与活跃备选的准入竞争。只有全冷却兜底才调用 `directory.TriggerRefresh()` 唤醒后台循环。失败沿用目录退避，成功后的自动唤醒至少间隔 `MinimumBackoff`（30 秒）。未知别名仍可显式 `Refresh()`。[注册准入](../../../connectivity/relayset/admission.go) · [目录刷新](../../../engine/networkdirectory/directory.go)

### 3.3 带宽分档 + 实测耗时分桶 + 冷启动哈希打散

调整 [connectivity/relayplan/selection.go](../../../connectivity/relayplan/selection.go) 的 `less()` 比较链，并用命名常量定义阈值：

```go
const (
    BulkCapacityThresholdMbps uint32        = 50
    DurationBucket            time.Duration = 100 * time.Millisecond
)
```

排序优先级（从高到低）：

1. **观测结果等级 (`rank`)**：`Admitted (2)` > `Unmeasured (1)` > `Failed (0)`。
2. **发布状态 (`Canary`)**：`active` 优先于 `canary`。
3. **粗粒度带宽档位 (`capacityTier`)**：`CapacityMbps >= 50`（主力中转档）优先于 `< 50`（5–10 Mbps 轻量信令档）。
4. **实测注册耗时分桶（仅当双方均为 `Admitted`）**。比较 `max(Duration, 0) / DurationBucket`。130 ms 和 950 ms 落入不同桶，110 ms 和 140 ms 落在同桶时交给带宽打破平局。
5. **标称带宽 (`CapacityMbps`)**：同档位同耗时桶（或同为未测节点）内，带宽更高者优先。
6. **确定性哈希打散 (`SelectionSeed`)**：
   - `relayplan.Input` 新增 `SelectionSeed string`（生产环境由 `engine.Engine` 传入已有每进程随机 `engine.identity`，单测留空时退回 `a.Identity < b.Identity`）。
   - 使用 `fnv32a(SelectionSeed + "|" + NetworkGeneration + "|" + candidate.Identity)` 打破前述条件完全相同的平局，碰撞时按 `Identity` 排序。带宽与耗时影响入围顺序，入围后的主节点仍由实际注册竞速决定。

### 3.4 可观测性设计

- `share_network_plan` 输出 `cooldown_fallback`、入选的延迟备选及其冷却和服务端截止时间，并沿用 `filters` 记录跳过原因。
- `relay_registration_attempt` 先更新健康表再输出诊断。握手取得 HTTP 状态码时记 `http_status`，触发或延长冷却时附 `cooldown_ms`、`cooldown_reason`、`consecutive_failures`，显式等待注明服务端截止时间。
- 用点击分享到链接可用、打开链接到首屏目录可见的 P95，以及每次打开的握手数比较改造效果。

---

## 4. 实施步骤与模块改动清单

### 步骤 1：重构 `connectivity/relayplan` 核心规划与排序策略

- `relayplan.Build()` 实现 §3.2 的槽位规则和 §3.3 的比较链，`networkruntime.New` 接收引擎已生成的随机身份作为 `SelectionSeed`。将延迟备选及其拨号条件传给目录注册协调器。[规划器](../../../connectivity/relayplan/plan.go) · [引擎装配](../../../engine/engine.go) · [目录注册协调器](../../../connectivity/relayset/admission.go)
- 规划和准入测试覆盖健康候选充足、只剩一个、全冷却、共享槽位上限及备选解冻。排序测试覆盖多样性、耗时桶与同级种子打散。

### 步骤 2：接入握手退避与 `networkruntime` 健康表

- `transport/relayv2`、`connectivity/senderrelay` 和原生中转接通 §3.1 的两条拒载路径。用现有假时钟验证 `Wake()`、显式期限和无头 503，覆盖匿名预算与 owner 恢复，以及停放耗时剔除。
- `networkruntime` 接管观测存储，通过 `engine/share.go` 将 `Selection` 交给 `Observer()`，接入目录唤醒并透传 §3.4 的决策字段。回归覆盖固定计数窗口、显式期限延长、衰减、成功重置、拒载作用域、网络代次和目录刷新节流。[分享装配](../../../engine/share.go) · [诊断投影](../../../engine/internal/networkruntime/diagnostics.go) · [决策结构](../../../engine/networkconfig/config.go)

---

## 5. 关键用户场景与产品预期对照

| 场景 | 预期行为 |
|---|---|
| 当前两台目录节点均正常 | 两台并发，首个成功即发布。 |
| 一台暂时不可达，或两台都曾因本地断网失败 | 单个常规候选可带一个延迟备选。全部为本地软冷却时立即竞速，网络恢复后原链接仍有机会接入。 |
| 停服或公共预算耗尽 | 收到 30 秒显式退避的端点等到期，其他候选继续注册。瞬时满载且无提示时快速重试。 |
| 某次分享被拒、随后登录或换票 | 新分享按当前凭证尝试，旧分享的拒绝只影响原注册。 |
| 全部旧节点退役 | 全冷却规划异步唤醒目录刷新，更新后的目录用于后续分享。 |
| Windows 右键冷启动 / CLI 冷启动 | 首次网络快照保留握手样本。CLI 以进程种子打散同级平局。 |
| 自建中转或 `ConfiguredOnly` | 显式中转独立注册。仅配置模式的规划跳过账号读取和目录刷新。 |

## 6. 本地对照测量

在 Windows 回环网络上，用同一套原生中转和 Chromium 前端对比基线 `e16a108e` 与初次实现 `a3ae8762`。发送进程跨分享保留观测，每次打开使用新的浏览器上下文并关闭 WebRTC。每个版本、每种场景预热 2 次，再交替采集各 24 次。

| 目录场景 | 发起分享到链接可用 P95，毫秒 | 打开链接到首个目录项可见 P95，毫秒 | 每次打开的平均 WebSocket 握手数 |
|---|---:|---:|---:|
| 两台健康中转 | 51.6 → 53.4 | 169.9 → 174.6 | 3 → 3 |
| 两台健康，另有一台返回 503 并要求等待 30 秒 | 50.5 → 50.9 | 164.7 → 171.6 | 5 → 3 |

箭头左侧为基线，右侧为初次实现。分享计时从调用 `Engine.StartShare` 到链接生成。浏览器握手统计截至首个目录项可见。503 场景每次少发起 2 次握手，两种场景的本机 P95 均略有上升。

随后以 `a3ae8762` 为基线复测本次修复。每个版本使用对应的 Go 发送端和 Rust 中转，共用浏览器前端。两轮分别预热后交替采样 24 和 72 次，下表按每个场景、每个版本的全部 96 次计算。

| 目录场景 | 分享到链接 P95，毫秒 | 打开到首个目录项 P95，毫秒 | 每次打开的平均 WebSocket 握手数 |
|---|---:|---:|---:|
| 两台健康中转 | 47.7 → 50.2 | 157.1 → 162.0 | 3.00 → 3.00 |
| 两台健康，另有一台返回 503 并要求等待 30 秒 | 49.9 → 48.0 | 160.1 → 161.1 | 3.02 → 3.01 |

这些是本机回环数据。健康场景的 P95 分别增加 2.6 和 4.9 毫秒，握手数相同。
