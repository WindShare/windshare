# 移除 `/v1/blocks` 与 `attribution/` 冗余链路执行计划

移除浏览器端 `/v1/blocks` 封禁轮询体系及 Authority 侧 `attribution/*` 永久归因表，将官方中继封禁收敛为**服务端单一执行点（Authority 拒票 + `wsrelay` 同步封禁与即时踢断）**，保留纯静态的「举报分享」入口，并补齐浏览器对 `ErrorFrame::AccessDenied` 的本地化体验。

---

## 一、背景分析与架构评估

### 1. 现状问题与源码事实

当前系统为“封禁违规分享/账号”维护了两套高度重叠的机制：一套在服务端（`wsrelay` 每 60 秒同步 `restriction/*` + Authority 领票校验），另一套在浏览器端（`/v1/blocks` + `attribution/*`）。后者在高频路径上造成了严重的资源与复杂度债务：

1. **与 `wsrelay` 100% 功能重叠，且并非安全边界**
   - 浏览器侧 [web/src/application/network/authority.ts](../../web/src/application/network/authority.ts) 仅拦截发往「官方目录中继节点」的连接，不拦 P2P 也不拦自建中转（[web/src/receiver/relay-admission.ts](../../web/src/receiver/relay-admission.ts)）。
   - 官方中继 `wsrelay` 本身已每 60 秒通过 `/v1/node/usage` + `/v1/node/restrictions` 分页同步全量封禁（[relay-rust/crates/relay-native/src/node/usage/restriction_sync.rs](../../relay-rust/crates/relay-native/src/node/usage/restriction_sync.rs)），同步后通过 `watch` 立即通知活跃 Share Actor 踢断在途会话（[relay-rust/crates/relay-native/src/actor.rs](../../relay-rust/crates/relay-native/src/actor.rs)、[actor/access.rs](../../relay-rust/crates/relay-native/src/actor/access.rs)），并对后续 `REGISTER` / `RESUME` / `JOIN` 直接返回 `ErrorFrame::AccessDenied`（code 13）。
   - 浏览器端检查可被自部署前端或 CLI 轻松绕过，真正生效且同时覆盖 Web、CLI、Desktop 以及 `Credential` 撤销的唯一安全边界始终是 `wsrelay` 与 Authority 领票接口。
2. **Cloudflare Free 10 万次/天配额的头号杀手**
   - 浏览器每 60 秒带 `cache: 'no-cache'` 轮询 `GET /v1/blocks`（[web/src/network/authority/feed.ts](../../web/src/network/authority/feed.ts)）。
   - 服务端边缘层 [relay-rust/crates/network-authority/src/host/edge.rs](../../relay-rust/crates/network-authority/src/host/edge.rs) 设置 `Cache-Control: private, no-cache` 并直接 `forward` 到单实例 Durable Object；即便是 `304 Not Modified`，每次请求仍执行 1 次 Worker 调用 + 1 次 DO 调用 + 1 次 `transactionSync` + 2 次 SQLite 读取（[host/mod.rs](../../relay-rust/crates/network-authority/src/host/mod.rs)）。
   - 仅需约 **70 个同时打开官方链接的浏览器标签页**（`70 × 1440 = 100,800` 次/天），单个接口即可打爆免费配额并拖垮全网注册、登录、续票与节点心跳；相比之下 10 台官方节点全天心跳仅 `14,400` 次。
3. **`attribution/*` 永久膨胀与 `/v1/blocks` 炸包隐患**
   - WindShare 分享是临时的（发送端退出即终止，重开分享换新 `ShareID`），但 [outbox.rs](../../relay-rust/crates/relay-native/src/node/usage/outbox.rs) 每分钟将活跃账号分享放入 `Batch.shares`，Authority 的 `remember_shares()` 永久写入 `attribution/<account>/<share>` 且从不清理（[public_blocks.rs](../../relay-rust/crates/network-authority/src/public_blocks.rs)）。
   - 封禁账号时 `update_public_blocks()` 全扫该账号历史上所有已失效 `ShareID` 并全部写入全量 JSON 单行 `public-blocks`。每个 `[u8; 12]` 经 JSON 整数数组 + Base64Url 编码占 ~58 字节；超过 [blocks.ts](../../web/src/network/authority/blocks.ts) 硬上限（`100,000` 条 / `8 MB`）后，全网所有浏览器验包直接失败。
4. **`accounts` 死字段与无意义的隐私暴露**
   - `/v1/blocks` 载荷包含 `accounts` 字段（[public_blocks.rs](../../relay-rust/crates/network-authority/src/public_blocks.rs)），但分享链接本身不含 `AccountID`，整个 `web/src/` 无任何业务代码读取 `snapshot.accounts`，徒增体积并公开暴露全网被封账号 ID。

### 2. 必须一并处理的 4 个关联细节

1. **管理后台账号详情页的 `attribution/` 消费点**：[account_admin.rs](../../relay-rust/crates/network-authority/src/account_admin.rs) 与 [consoles/network/accounts.tsx](../../consoles/network/accounts.tsx) 读取 `attribution/<account>/` 展示前 100 条按随机 ID 排序的历史分享（无时间、无在线状态），且因主键方向是 `account -> share`，举报详情页（[restrictions.tsx](../../consoles/network/restrictions.tsx)）根本无法通过被举报的 `ShareID` 反查发件账号。移除 `attribution/` 时同步移除该展示卡片；若未来确有“按举报追责账号”需求，应单独设计**带短 TTL（7~30 天）的反向索引 `share -> account`**，而非永久正向表。
2. **浏览器接收 `ErrorFrame::AccessDenied` 的全链路本地化补齐**：此前 `/v1/blocks` 抛 `RelayPolicyDeniedError` 走本地化“官方中转访问受限”提示，而 `wsrelay` 直接返回 `RELAY_ERROR.accessDenied` 在 [recovery-failure.ts](../../web/src/receiver/recovery-failure.ts) 判为终态失败并直接暴露英文 `'Relay access was denied for this share.'`（[socket.ts](../../web/src/transport/relay/receiver/socket.ts)），且会误导性提示“确认发送方在线”。移除 `/v1/blocks` 后，应将 `RELAY_ERROR.accessDenied` 显式映射为类型化的本地化提示，并同时覆盖首次加入失败（`joinFailure`）、会话中途浏览/下载/预览失败（`mapReceiverFailure`）、顶部连接态与任务阻塞/失败结算态（`TaskBlocking` 独立 `'access-denied'` 变体 + `ReceiveFailureReason` 的 `'relay-access-denied'`），以及下载准入/保留任务提示（[coordinator.ts](../../web/src/application/receive/coordinator.ts) / [admission.ts](../../web/src/application/receive/admission.ts)），避免同页面仍引导用户“请重新打开分享链接，恢复连接后再下载”或“请重新开始下载”。
3. **`wsrelay` 对非驻留被封 `ShareID` 的 `JOIN` 拦截补齐**：当前 [ingress.rs](../../relay-rust/crates/relay-native/src/actor/ingress.rs) 处理 `JOIN` 仅依赖 `Actor::resident_revocation()`，而后者要求 `self.residency` 存在（发件端在线或 60 秒 `SENDER_GRACE` 内）。一旦超过 60 秒或发件端离线/未注册该节点，`JOIN` 会退化为 `ErrorFrame::NotFound`，导致浏览器空等 30 秒重试并误导提示“分享链接可能仍然有效”（多官方节点竞速时只要任一节点未驻留也会被拖住 30 秒），同时向中继制造无效重连压力。需在 `FrameKind::Join` 分支仅当 **`self.residency.is_none()`** 时补充 `share_restricted(share)` 内存查表（零 I/O、不碰转发热路径、对正常在线分享不增加第二次读锁），并拆出**仅拒断当前连接**的 helper 返回 `ErrorFrame::AccessDenied`（**不调用** `disconnect_revoked_ports()`，避免静默 `Abort` 同一 `ShareID` 下其他并发握手中的 `JOIN` 连接）。**保留**驻留分享撤销时 `disconnect_revoked_ports()` 的立即 `Closure::Abort` 踢断不动（不在稳态连接补发 `WS2E`，避免拖延资源回收或触发 Go `transport/relayv2/link.go` 的 `ErrProtocol`）。
4. **保留「举报分享」入口**：[authority.ts](../../web/src/application/network/authority.ts) 中的 `watchReport()` / `reportURL()` 纯本地根据签名目录匹配官方节点并拼接 `https://api.<domain>/report#share=<id>`，零后台请求，应完整保留并简化为单一职责模块。

### 3. 架构权衡与收益总结

| 维度 | 改动影响 | 说明 |
|---|---|---|
| **用户体验（UX）** | **升级** | 正常高频用户零额外跨源轮询、无需等待 `windshare-authority` IndexedDB 打开即可拨号、消除免费额度被打爆导致全网 503 的隐患；直接封禁的 `ShareID` 在所有可达官方节点上均 1 RTT 拒连并展示一致的本地化终态文案（多节点中若有节点网络不可达，仍按正常 fail-safe 超时，避免误杀可用路由；仅封禁 `Account` 且发件端离线超过 60s Grace 期后自然表现为普通不可达；仅在“刚封禁 + 节点与 Authority 失联 + 浏览器连得上 Authority”这一极端叠加态存在延迟至节点恢复连通）。 |
| **开发者体验（DX）** | **大幅升级** | 删除跨 Rust / TypeScript / Go / Console 四端的一整套签名缓存、跨标签页防回滚、双版本封禁视图与测试向量；同步减轻后续 Authority SQLite 关系表重构（Issue #37）的存储与迁移负担。 |
| **高频 vs 低频权衡** | **纠偏** | 彻底停止让 100% 正常用户的高频打开与每分钟在线，为极低频边缘场景（中继失联兜底/省一次拒连握手）持续买单。 |
| **架构与语义** | **升级 / 更正交** | 回归“控制面只服务节点和账号认证，不直面海量匿名接收端浏览器”原则；封禁事实源唯一（`restriction/*` + `restriction-version`），执行点唯一（服务端），Web / CLI / Desktop 对 `ErrorFrame::AccessDenied` 行为完全一致。 |

---

## 二、边界总览：留什么 vs 砍什么

| 分类 | 内容 |
|---|---|
| **保留** | • Authority `restriction/*` 存储、`restriction-version`、`POST /v1/admin/blocks`、`POST /v1/node/restrictions` 分页同步、发件端领票拒签（[accounts.rs](../../relay-rust/crates/network-authority/src/accounts.rs)）<br>• `wsrelay` 60 秒限制同步、实时踢断活跃连接与拒绝 `REGISTER` / `RESUME` / `JOIN`（`ErrorFrame::AccessDenied`）<br>• `Load.resident_shares: u64`（用于节点 `Drain` / `cutoff_ready` 排空判断，**严禁误删**）<br>• `POST /v1/reports`、管理台举报列表/详情/手动封禁 `Share` 与 `Account`<br>• 浏览器纯静态举报链接（`watchReport` / `reportURL` / `ReportLink.tsx`）<br>• `deploy/network/release.json` 中的 `authority: { url, root }`（Go/Desktop 仍用 `root` 验证票据与证书链，浏览器仅读取 `url`） |
| **砍掉** | • `GET /v1/blocks`、`PublicBlockFeed`、`public_blocks.rs`、`public-blocks` 存储键、`CachePolicy::PrivateRevalidate`<br>• `remember_shares()`、`update_public_blocks()`、`attribution/*` 存储读写<br>• `relay-access::usage::ShareAttribution`、`Batch.shares`、`relay-native` `Service::resident_shares()`<br>• `AccountDetails.{shares, has_more_shares}` 及管理台账号详情页 `Shares` 卡片与相关文案<br>• `web/src/network/authority/` 整目录（`blocks.ts`、`cache.ts`、`feed.ts`、`trust.ts`）与 `web/src/receiver/relay-admission.ts`<br>• 浏览器侧死分支：`RelayPolicyDeniedError`、`RELAY_POLICY_RETRY_MS`、`AllRelaysRestrictedError`、`reason: 'authorization'`、`authorityObserver` / `authority_blocks`<br>• `core/serviceauthority/codec.go` 中死常量 `BlocksDomain` 及 `testdata/network/authority.json` 中的 `blocks` 字段 |

---

## 三、分模块改造清单

### 1. `relay-rust/crates/relay-access` & `relay-native`（移除 `Batch.shares` 上报 + 补齐非驻留封禁 `JOIN` 拦截）

- **`relay-rust/crates/relay-access/src/usage.rs`**
  - 删除 `ShareAttribution` 结构体与 `Batch.shares` 字段。
  - 更新 `Batch::valid()`：移除 `self.shares.len() <= STATUS_BATCH_ENTRIES`、`(self.records.is_empty() || ...)` 中的 `self.shares.is_empty()` 以及 `self.shares.iter().all(...)` 检查（保留 `self.records.is_empty() || self.resident_accounts.is_empty()`）。
  - 注意：**保留** `Load.resident_shares: u64` 与 `STATUS_BATCH_ENTRIES`（`resident_accounts` 仍使用该上限）。
- **`relay-rust/crates/relay-access/src/usage_tests.rs`**
  - 移除测试构造中的 `shares: Vec::new()` 字段。
- **`relay-rust/crates/relay-native/src/node/access/mod.rs`**
  - 删除 `Service::resident_shares()`（保留 `Service::reported_policy()` 与 `Service::resident_accounts()`）。
  - 新增 `Service::share_restricted(&self, share: ShareId) -> bool`：读锁检查 `restrictions` 中任一 issuer 的 `shares` 集合（纯内存查表，零 I/O）。
- **`relay-rust/crates/relay-native/src/actor/access.rs`** & **`src/actor/ingress.rs`**
  - 将 `refuse_control_access()` 拆出单连接拒断 helper（如 `refuse_connection_access(connection, kind, error, now)`：记录日志、发送 `ErrorFrame`、`port.finish()`、通知 `Input::Disconnected(connection)`），**不调用** `disconnect_revoked_ports()`；驻留分享撤销路径在单连接拒断后再调用 `disconnect_revoked_ports()` 踢断兄弟连接。
  - `FrameKind::Join` 在 `self.resident_revocation()` 返回 `None` 且 **`self.residency.is_none()`**（涵盖 `Vacant` 与 `Stopped` 等无驻留状态，避免正常在线 `JOIN` 重复获取读锁）时，检查 `self.node.access.share_restricted(self.identity.share)`；命中时调用单连接拒断 helper 返回 `ErrorFrame::AccessDenied`（日志中归因记为缺省，不误杀同 `ShareID` 下其他并发握手中的 `JOIN` 连接）。注：`Stopped` 态命中封禁时优先返回 `AccessDenied` 而非 `ErrorFrame::Stopped` 为有意语义（封禁优先级高于已停止，避免引导用户让发送方重开链接）。
  - **保持** `disconnect_revoked_ports()` 现有的 `Closure::Abort` 即刻中断行为不变，不在稳态转发连接上发送 `WS2E`。
- **`relay-rust/crates/relay-native/src/actor_access_tests.rs`**
  - 新增单测：验证无驻留（`Vacant` 与 `Stopped`）的被直接封禁 `ShareID` 收到单个及并发 `JOIN` 时，每个握手连接均直接返回 `ErrorFrame::AccessDenied`（不被静默 `Abort`）。
- **`relay-rust/crates/relay-native/src/node/usage/outbox.rs`**
  - `Reporter::batch()` 删除 `shares: Vec::new()`；`Reporter::refresh()` 删除 `batch.shares = self.node.access.resident_shares(issuer);` 并将 `let mut batch` 改为 `let batch`（保持 `clippy -D warnings` 零告警）。
- **`relay-rust/crates/relay-native/src/node/usage/usage_tests.rs`** & **`src/storage/node_control_tests.rs`**
  - 移除 `Batch { ..., shares: vec![] }` 中的 `shares` 初始化。

### 2. `relay-rust/crates/network-authority`（移除 `/v1/blocks` 与 `attribution/`）

- **整文件删除**：`src/public_blocks.rs`。
- **`src/lib.rs`**：移除 `mod public_blocks;`。
- **`src/governance.rs`**：`Authority::block()` 中移除 `self.update_public_blocks()?;`（保留 `self.set_restriction(entry, blocked)?` 与审计日志）。
- **`src/usage.rs`**：
  - `Authority::usage()` 中：
    - 校验账号存在性循环仅遍历 `batch.records`（不再 `.chain(batch.shares.iter()...)`）；
    - 状态上报分支移除 `self.remember_shares(&batch.shares)?;`。
- **`src/account_admin.rs`**：
  - 删除常量 `SHARE_LIMIT`。
  - 从 `AccountDetails` 移除 `shares: Vec<[u8; 12]>` 与 `has_more_shares: bool`。
  - `account_by_id()` 移除对 `attribution/<id>/` 的全前缀扫描。
- **`src/host/edge.rs`**：
  - 删除 `PRIVATE_REVALIDATE_CACHE_CONTROL` 与 `CachePolicy::PrivateRevalidate`。
  - 移除 `public_blocks` 路径特判及 `CachePolicy::PrivateRevalidate` 分支。
- **`src/host/mod.rs`**：
  - 从 `NetworkAuthority` 移除 `public_blocks: Rc<RefCell<PublicBlockFeed>>` 及其初始化、克隆与传参。
- **`src/host/routes.rs`**：
  - 删除 `PUBLIC_BLOCKS_PATH`、`Context.if_none_match`、`public_block_response()` 及 `dispatch()` 中的 `public_blocks` 参数与 `/v1/blocks` 分支。
  - 保留 `POST /v1/admin/blocks`、`POST /v1/node/restrictions`、`POST /v1/reports` 等所有既有路由。
- **测试与契约向量清理**：
  - `src/governance_tests.rs`：删除 `public()` 辅助函数、`account_blocks_cover_observed_shares_without_polluting_explicit_share_blocks` 与 `public_snapshot_reuses_bytes_until_renewal_or_policy_change`（保留 `block()` 通过 `authority.blocks()` / `restriction_version()` 生效的测试覆盖，必要时补 1 条精简断言验证 `Block::Share` / `Block::Account` 的幂等封禁与解封）。
  - `src/account_admin_tests.rs`：移除向 `attribution/` 写入测试数据及对 `details.shares` / `details.has_more_shares` 的断言。
  - `src/authority_tests.rs`、`src/node_load_tests.rs`、`src/fleet/operations_tests.rs`：移除 `Batch` 构造中的 `shares: vec![]`。
  - `src/contract_vectors_tests.rs`：移除 `PublicBlockFeed` 与 JSON 向量中的 `"blocks"` 字段，通过 `UPDATE_AUTHORITY_VECTORS=1` 重新生成 `testdata/network/authority.json`。
  - `tests/worker.mjs`：移除 `publicPolicy` 特判、`/v1/blocks` 断言（ETag / 304 / 更新校验）与 `Batch` 中的 `shares: []`（可补 1 条断言确认 `GET /v1/blocks` 返回 `404`，同时保留 `POST /v1/admin/blocks` 与节点 `/v1/node/restrictions` 同步测试）。

### 3. `core/serviceauthority`（Go 契约清理）

- **`core/serviceauthority/codec.go`**：删除未使用的常量 `BlocksDomain = "windshare/public-blocks/v1\x00"`。
- **`core/serviceauthority/contract_test.go`**：确认重新生成后的 `testdata/network/authority.json`（不含 `blocks` 字段）可正常通过全部验证。

### 4. `consoles/network`（管理后台精简与重新构建）

- **`consoles/network/model.ts`**：从 `AccountDetails` 移除 `shares: number[][]` 与 `has_more_shares: boolean`（保留 `Restrictions`、`Load.resident_shares`）。
- **`consoles/network/accounts.tsx`**：删除 `Shares` 组件及其在 `AccountDetail` 中的渲染。
- **`consoles/network/locales/en.ts`** & **`zh-CN.ts`**：删除废弃文案键 `attributedShares`、`noShares`、`shareLimit`、`viaAccount`、`directlyRestricted`（注意核查 `directlyRestricted` 是否仅在 `Shares` 中使用；`restrictions.tsx` 用的是 `directShare` / `directAccount` / `restricted` / `noDirectRestriction`）。
- **构建产物同步**：重新构建管理台前端，更新 `relay-rust/crates/network-authority/console-dist/`。

### 5. `web/`（浏览器接收端清理 + `AccessDenied` 本地化体验补齐）

#### 5.1 删除冗余封禁拉取与 Admission 机制
- **整目录删除**：`web/src/network/authority/`（`blocks.ts`、`cache.ts`、`feed.ts`、`trust.ts`）。
- **整文件删除**：`web/src/receiver/relay-admission.ts`。
- **简化 `web/src/application/network/authority.ts`（建议重命名为 `report.ts` / `OfficialShareReporter`）**：
  - 仅保留严谨的 authority `url` 校验（HTTPS、无凭据/查询/片段、拒绝 `.workers.dev`）、`watchReport(share, candidates, directory, onAvailable)` 与 `reportURL(share)`；
  - 移除 `AuthorityBlockFeed`、`admission()`、`close()` 及对 `root` 的依赖。
- **`web/src/application/network/builtin.ts`**：
  - `BuiltinNetworkRelease['authority']` 仅读取 `{ readonly url: string }`（与 `release.json` 中现有的 `{ url, root }` 结构兼容，无需改动 `release.json`）；
  - 构造参数移除 `authorityObserve`；
  - `resolve()` 调用 `resolveLinkRelays(capability, source)`（不再传递 `admission`），保留 `watchReport()` 逻辑；
  - `close()` 移除 `this.#authority?.close()`。
- **`web/src/application/network/diagnostics.ts`** & **`web/src/document/receiver-runtime.tsx`**：
  - 移除 `authorityObserver` 及 `authority_blocks` trace 事件，移除 `new BuiltinNetwork(...)` 第二个参数。
- **`web/src/application/network/resolve.ts`** & **`web/src/receiver/relay-selection/candidates.ts`**：
  - 移除 `admission?: RelayAdmission` 参数与 `RelayCandidates.admission` 属性。
- **`web/src/receiver/browser-join.ts`** & **`web/src/receiver/session-factory.ts`**：
  - 直接调用 `dialRelayReceiver(...)` / `this.#dialRelay(...)`，不再经由 `admittedRelay(...)` 包装。

#### 5.2 清理 `'authorization'` 冷却死分支 & 接入 `RELAY_ERROR.accessDenied` 本地化
- **`web/src/receiver/recovery-failure.ts`**
  - 删除 `RelayPolicyDeniedError`、`RELAY_POLICY_RETRY_MS`、`AllRelaysRestrictedError` 及 `RecoveryRetry.reason` 中的 `'authorization'`；
  - 保留 `isTerminalRecoveryFailure()` 对 `RELAY_ERROR.accessDenied` 返回 `true` 的终态判定；
  - 新增纯函数 `isRelayAccessDenied(error: unknown): boolean`：递归解包 `RelayEndpointFailure` 与 `AggregateError`（所有子错误均为 `accessDenied` 且非空时返回 `true`，与 [connection-state.ts](../../web/src/receiver/connection-state.ts) 中 `confirmedShareEnd` 的多中继聚合语义一致），识别 `error instanceof RelayReceiverError && error.relayError?.code === RELAY_ERROR.accessDenied`。
- **`web/src/transfer/fault.ts`** & **`web/src/transfer/state.ts`**
  - 从 `fault.ts` 移除对 `AllRelaysRestrictedError` 的导入及 `isTransientLaneOrTransportError` 中的对应分支；
  - 在 `state.ts` 的 `ReceiveFailureReason` 新增 `'relay-access-denied'`（`output/store/` 无硬编码校验，零存储迁移负担）；
  - 在 `fault.ts` 提供纯函数（如 `transferFailureReason(error)`，统一合并 `catalogCapacityReason` 与 `isRelayAccessDenied`），供 `execution-state.ts`（`markFailed`）与输出后端（`fsa.ts`、`opfs-settlement.ts`、`portable.ts`）共用，消除四处重复的失败原因判断。
- **`web/src/receiver/relay-selection/recovery.ts`**
  - 移除 `#selectionFailure()` 中针对 `retry.reason === 'authorization'` 与 `AllRelaysRestrictedError` 的特判分支（多官方中继全返回 `accessDenied` 时会自然命中已有的 `every(terminal)` 分支，返回单一 `RelayEndpointFailure` 或包含全部终态失败的 `AggregateError`）。
- **`web/src/receiver/initial-join.ts`**
  - 移除 `JoinHoldReason` / `UNAVAILABLE_MESSAGES` 中的 `'authorization'` 分支。
- **`web/src/receiver/connection-state.ts`**
  - 从 `ReceiverReconnectActivity` 的 `reason` 联合类型中移除 `'authorization'`；
  - 在 `ReceiverConnectionSnapshot` 的 `unavailable` 变体补充原因：`'recovery-exhausted' | 'protocol-failed' | 'access-denied'`；
  - `failed(error)` 中，在 `confirmedShareEnd(error)` 之后检查 `isRelayAccessDenied(error)`，命中时发布 `{ kind: 'unavailable', reason: 'access-denied' }`。
- **`web/src/session/diagnostics.ts`**
  - 从 `ConnectionRecoveryTraceEvent['waitReason']` 移除 `'authorization'`。
- **UI 层本地化接入（严格面向真实协议错误，不假定一定是“官方”中转，因为自建中转同样可能返回 `AccessDenied`）**：
  - **`web/src/ui/model.ts`**：在 `ReceiverFailure` 联合类型新增 `relayAccessDenied`（带 `detail: string`，与 `invalidLink` / `permissionDenied` 同构）；将 `PreviewSnapshot` 的 `state: 'error'` 变体从 `message: string` 统一为 `failure: ReceiverFailure`（与 `BrowseState` 及 `FailureNotice` 对齐）。
  - **`web/src/ui/controller/failures.ts`**：在 **`joinFailure(error, detail)` 与 `mapReceiverFailure(error, publicError)` 两处**均判断 `isRelayAccessDenied(error)` 并返回 `{ kind: 'relayAccessDenied', detail }`（同时覆盖首次加入失败与会话中途浏览/下载/预览失败）。
  - **`web/src/ui/preview-controller.ts`** & **`web/src/ui/share-content/MediaPreview.tsx`**：`PreviewController` 在 `state: 'error'` 分支改用 `mapReceiverFailure(error, this.#host.publicError)` 生成 `failure`（`video.failure` 的局部 seek 解码提示保持不变）；`MediaPreview.tsx` 传入 `failure={preview.failure}`，并在 `failure` 为 `relayAccessDenied` 或 `source-changed` 时隐藏 `downloadOriginalHint`（避免矛盾引导下载原文件）。
  - **`web/src/ui/locales/{en,zh-CN}/failures.ts`**：新增 `relayAccessDenied` 文案：
    - en: `'Relay access was denied for this share.'`
    - zh-CN: `'中转已拒绝此分享的访问。'`
  - **`web/src/ui/tasks/model.ts`** & **`web/src/ui/experience/share-presentation.ts`** & **`web/src/ui/tasks/stage.ts`**：
    - `share-presentation.ts` 移除 `initialJoinHoldLabel` / `initialJoinStatus` 中 dead `join.reason === 'authorization'` 分支；
    - `shareConnectionLabel` 中对 `connection.kind === 'unavailable' && connection.reason === 'access-denied'` 返回本地化标签（读 `locales/*/connection.ts` 的 `relayAccessDenied`：en `'Relay access denied'` / zh-CN `'中转拒绝访问'`）；
    - `TaskBlocking['kind']` 新增独立变体 `'access-denied'`，在 `share-presentation.ts` 复用模块级冻结常量 `ACCESS_DENIED`（保持 `TaskComposer` 引用相等缓存与多语言分层，不借壳 `'unavailable'` 传本地化文案）；
    - `stage.ts` 同步更新 `pausedStage()`（将 `'access-denied'` 纳入阻塞守卫）、`blockingStage()`（渲染 `relayAccessDenied` / `accessDeniedHint`）与 `failedStage()`（对 `reason === 'relay-access-denied'` 渲染本地化拒绝访问提示，避免下载中途被封或刷新回看历史时误导“请重新开始下载”并暴露英文 detail）；多文件下载已有部分完成文件时仍按既有 `partial` 态结算以保留导出入口，不新增 `ReceivePartialReason` 变体。
  - **`web/src/ui/connection/ConnectionRecovery.tsx`** & **`web/src/ui/locales/{en,zh-CN}/connection.ts`** & **`web/src/ui/locales/{en,zh-CN}/stage.ts`**：
    - `ConnectionRecovery.tsx` 移除 `case 'authorization'`；`locales/*/connection.ts` 移除废弃文案键 `authorization`、`relayRestricted`、`restrictedStatus` 并补齐 `relayAccessDenied`；
    - `locales/*/stage.ts` 补齐 `relayAccessDenied` 与 `accessDeniedHint`（与 `shareEndedHint` 对齐，明确本地已保存文件仍可导出，如 en: `'Relay access was denied for this share. Files already saved in this browser can still be saved.'`，zh-CN: `'此分享的中转访问已被拒绝。已保存在浏览器中的文件仍可直接保存。'`）。
  - **下载准入与保留任务链路（消除与拒绝访问相矛盾的重开链接引导）**：
    - **`web/src/application/receive/contracts.ts`**：`ReceiveBlockReason` 新增 `'relayAccessDenied'`。
    - **`web/src/application/receive/admission.ts`** & **`coordinator.ts`** & **`web/src/ui/controller.ts`** & **`web/src/ui/experience/retained-match.ts`**：
      - 将原本重叠的 `connectionUnavailable` + `shareEnded` 两个布尔字段收敛为单一终态连接状态（如 `'share-ended' | 'access-denied' | 'unavailable' | null`）；`observeConnection` 接收完整终态原因，在 `'access-denied'` 时统一返回 `blockedAdmission('relayAccessDenied')`。
    - **`web/src/ui/saving/present.ts`**：将 `'relayAccessDenied'` 纳入 `terminalConnection`（避免误显示重试按钮）。
    - **`web/src/ui/locales/{en,zh-CN}/admission.ts`**：新增 `relayAccessDenied` 文案（en: `'Relay access was denied for this share.'`，zh-CN: `'此分享的中转访问已被拒绝。'`）。

#### 5.3 Web 单元测试清理与补充
- **删除**：
  - `web/test/network/authority-blocks.test.ts`
  - `web/test/receiver/relay-admission.test.ts`
- **更新**：
  - `web/test/network/official-relay-authority.test.ts`（可重命名为 `official-share-report.test.ts`）：删除 `official relay policy demand` 套件及无用的 `testdata/network/authority.json` 导入，完整保留 `official share report scope` 套件，并验证 `BuiltinNetwork.resolve` 零 `/v1/blocks` 网络请求。
  - `web/test/receiver/relay-race.test.ts`、`initial-join.test.ts`、`handshake-recovery.test.ts`、`reconnect-controls.test.ts`：删除基于 `RelayPolicyDeniedError` / `AllRelaysRestrictedError` 的测试，补充针对 `RelayReceiverError(RELAY_ERROR.accessDenied)` 的终态判定、单中继/多中继聚合（`isRelayAccessDenied`）、`ReceiverConnectionState` 转移到 `access-denied` 以及 `mapReceiverFailure` / `transferFailureReason` 分类的纯逻辑单测（不写脆弱 UI 快照/文案锁死测试）。
  - 同步更新调用 `observeConnection` / `admitRetainedAction` 的相关单测与夹具（`receive-coordinator.test.ts`、`receive-state-convergence.test.ts`、`retained-match.test.ts`、`web/test/browser/receive-process-recovery-harness.ts`、`web/test/output/fsa-safe-name-restore.test.ts`、`web/test/output/opfs-lifecycle.test.ts`），覆盖 `access-denied` 映射为 `'relayAccessDenied'`。

### 6. 文档与 Issue #37 衔接

- 同步更新 `relay-rust/crates/network-authority/README.md`，删除关于 `attributed shares` 的描述。
- 后续推进 **Issue #37（Authority SQLite 关系表重构）** 时，直接省去 `share_attributions` 表与 `public_blocks` 投影状态。

---

## 四、建议执行顺序与部署注意事项

### 1. 推荐拆分执行顺序

1. **Step 1 — Rust 协议与节点侧**：修改 `relay-access` 与 `relay-native`（移除 `ShareAttribution`、`Batch.shares`、`Service::resident_shares`，补充非驻留被封 `ShareID` 的单连接 `JOIN` 拦截与并发单测），运行 `cargo test -p windshare-relay-access -p windshare-relay-native`。
2. **Step 2 — Authority 控制面与契约向量**：删除 `public_blocks.rs`、移除 `/v1/blocks` 与 `attribution/`、精简 `AccountDetails`，重生成 `testdata/network/authority.json`，清理 Go `BlocksDomain`，运行 Rust 单测、Worker 集成测试与 `go test ./core/serviceauthority/...`。
3. **Step 3 — 管理后台前端与产物**：精简 `consoles/network` 并重建 `relay-rust/crates/network-authority/console-dist/`。
4. **Step 4 — 浏览器端清理与 `AccessDenied` 接入**：删除 `web/src/network/authority/` 与 `relay-admission.ts`，收敛 `report.ts`，清理 `'authorization'` 与 `fault.ts` 死分支，接通 `isRelayAccessDenied`、全链路本地化错误映射（含 `mapReceiverFailure`、`TaskBlocking` 与 `ReceiveFailureReason`）及 `ReceiveAdmission` 终态准入，更新受影响的纯逻辑单测。
5. **Step 5 — 全量门禁验证**：执行统一校验门禁并检查 `sloc` 与 lint 零告警。

### 2. 部署与存量数据注意事项

- **协同升级约束与本地 outbox 隔离**：`relay-access::usage::Batch` 启用了 `#[serde(deny_unknown_fields)]`。因处于 pre-v1.0，不引入向后兼容字段；若已有运行中的开发/测试环境，升级时需**将新版 `wsrelay` 与新版 `network-authority` 同步部署**（旧版 `wsrelay` 发送带 `"shares": []` 的批次会被新版 Authority 拒绝）。此外，`wsrelay` 本地 SQLite `usage_outbox`（`node_control.rs`）中残留的旧批次在升级重启后会因反序列化失败被自动移入 `usage_outbox_quarantine`（`reason = "invalid_json"`），不会崩溃或阻塞新批次上报。
- **已有 DO SQLite 中的残留键**：若复用旧开发 DO 实例，库中残留的 `public-blocks` 与 `attribution/*` 行不再被任何路径读取或写入，**无需专门编写在线批量 `DELETE` 迁移**（避免白白消耗 Cloudflare Free 10 万行/天写配额），在重建开发 DO 或执行 Issue #37 关系表迁移时一并丢弃即可。

---

## 五、验收标准与验证门禁

1. **行为验收**
   - 打开包含官方中继的分享链接时，浏览器 Network 面板零 `/v1/blocks` 请求，不再创建 `windshare-authority` IndexedDB 数据库；「举报分享」链接仍对官方中继分享正常展示、对纯自建中继分享隐藏。
   - 管理员封禁 `Share` 后，官方 `wsrelay` 在 ≤60 秒内通过 `/v1/node/restrictions` 同步生效：立即 `Abort` 踢断活跃连接，并对新 `REGISTER` / `RESUME` / `JOIN`（包括发件端已离线或超过 60s Grace 的无驻留 `ShareID`、并发 `JOIN` 与多官方候选节点）均返回 `ErrorFrame::AccessDenied`；浏览器首次打开或会话/下载中途被踢重连后均展示一致的本地化拒绝提示（含顶部状态、错误横幅、下载按钮/保留任务禁态与失败任务历史，不再误导重开原链接或重新开始下载）且不陷入无意义重试。管理员封禁 `Account` 后，踢断活跃连接并拒绝新票据/注册/恢复。
   - `GET /v1/blocks` 返回 `404`；管理台账号详情页正常渲染且无历史归因卡片。
2. **本地与 CI 验证命令**
   - Rust：`cargo test --workspace`、`cargo clippy --workspace --all-targets -- -D warnings` 及 `network-authority` Worker 集成测试（`relay-rust/crates/network-authority/tests/run.mjs`）
   - Go：`go test ./core/serviceauthority/...`
   - Consoles & Web：重新构建 `consoles/network`、`web` 类型检查与 `vitest` 单测
   - 全局门禁：`make check`（或 `make ci-parallel`）与 `make sloc` 全部通过
