# 公平调度保底与网络管理台账号运维计划

## 一、背景与权衡分析

对照 [原始需求](../../clarifications/原始需求.md#L52) 与 [中转服务需求](../../clarifications/中转服务.md#L7)，当前官方中转调度与网络管理台存在 4 处与终态描述的差异。但它们的性质、当前阶段价值和架构代价完全不同，不应整包重做，必须按“高频路径收益 vs 架构复杂度”拆开决策：

| 事项 | 当前实现 | 影响评估 | 本计划决策 |
|---|---|---|---|
| **1. 竞争期带宽公平** | `relay-access` 已实现 `19:1` 类间权重。宿主批量授出时，每条流只登记一个队头 | 两个持续积压的类可能各得一帧，同档账号也可能因会话更多而获得更多带宽 | **衔接可写队头，复用现有 WFQ** |
| **2. 网络后台展示已封禁 `restrictions` 列表** | `GET /v1/admin/console` 已返回全量 `restrictions.{accounts,shares}`，前端已保存在 `snapshot`，但页面从未渲染 | 管理员封禁后无法查看已封禁清单，解封时只能凭记忆手工粘贴完整 base64 ID。纯前端补齐即可解决 | **纳入实施**（立竿见影、零后端负担） |
| **3. 网络后台补齐已注册账号列表与单账号用量查询** | `account/*`、`username/*`、`usage/<id>/<day>`、`attribution/<id>/<share>` 均已持久化在 DO SQLite，但未暴露任何只读管理接口；前端“查询账号”仅做本地格式解码 | 不影响任何客户端、数据面转发与每分钟 `entitlement` 热写路径；上线/内测排查账号额度或封禁状态时可直接翻查列表或按 ID/用户名定位账号、查看近期日用量与已归因分享并执行封禁 | **纳入实施**（低耦合、仅注册时写 1 条不可变索引键） |
| **4. 竞争态扣减“每日 10 分钟优先时间”** | 原路线图 [global-network.md](global-network.md#L410-L422) 已明确排期到 P5；当前仅按每日总流量（10 GiB）分级 | 见下方“明确暂不做”分析：在当前 0 用户、零拥塞阶段会引入跨全栈重改与双配额心智负担，得不偿失 | **明确不做**（保留 P5 路线图与演进接口） |

### 为什么本期明确不做“竞争态 10 分钟优先时间”

1. **不能在单节点内简单计秒**：WindShare 默认同时挂载 2 台官方中转，若各节点各自计竞争秒数再由 Authority 相加，用户双路传输 1 分钟会被双倍扣减 2 分钟。正确语义是各节点上报 `[start, end]` 竞争区间、Authority 按账号合并时间并集，并在 DO SQLite 中按日维护区间集合——同时受 Cloudflare DO 免费版每日 10 万行写入配额约束。
2. **`Governor` 并不感知物理链路拥塞**：当节点未配置 `capacity_bytes_per_second` 时，[pacing.rs](../../../relay-rust/crates/relay-native/src/node/governor/pacing.rs#L87-L99) 返回 `u64::MAX`，调度器看不到真实网卡或跨境晚高峰拥塞；在尚未完成真实节点带宽标定前，竞争态检测容易失真。
3. **跨全栈协议与客户端改造成本高**：引入优先时间需要修改 `Ticket` / `Entitlement` CBOR 线格式（Rust `relay-access` + Go `core/serviceauthority` + `core/testvectors`）、节点本地 `access_quotas` SQLite 表结构、`UsageRecord` 上报协议、`engine/account`、桌面端与 CLI 状态文案。
4. **对高频用户心智是减分项**：在零竞争阶段，现有“每日 10 GiB 优先额度，用完降级不断网”已经直白易懂；过早把 UI 变成“优先时间剩 X 分 Y 秒（仅竞争时扣）+ 今日总流量剩 Z GiB”反而增加普通用户的理解成本。

现有两级调度器已保留清晰的分层抽象：公共层未来只需在 `Standard` 之上插入 `Priority` 档并配置类权重即可平滑演进到 P5，不会形成架构债。

---

## 二、方案设计

### 模块 1：中转出口公平调度（`relay-native`、`relay-access`）

`Owner` 严格优先。公共带宽在持续竞争时按 `Standard:Lowest = 19:1` 的字节份额分配，同档先按主体、再按会话分配。沿用现有 Finish-Tag WFQ 和重分类日志。[调度器](../../../relay-rust/crates/relay-access/src/governor.rs#L171) · [重分类日志](../../../relay-rust/crates/relay-native/src/node/governor.rs#L212)

`grant_pending` 可在 actor 补报队头前连续授出多帧。仍有可写后继的流在这段间隙看似空闲，类间与账号间的份额都可能失真。[宿主授出循环](../../../relay-rust/crates/relay-native/src/node/governor.rs#L190)

1. `Outbox` 持有载荷和写槽，同一目标连接的所有流共享两个数据写槽。通过提交和 `Event::Writable` 唤醒更新可写窗口内的队头及帧长。写满时让出带宽，恢复可写后再参与调度。[连接发送路径](../../../relay-rust/crates/relay-native/src/connection/mod.rs#L151)
2. 在同一调度临界区完成授出与可写后继队头的登记，保留持续积压的类与主体的服务进度。单流有额度即可同步授出。两类都可发时，2 KiB 的 `Lowest` 信令帧仍可按完成标签先于 64 KiB 的 `Standard` 数据帧发送。

启用官方中转时，按实测带宽设置 `capacity_bytes_per_second`。`contribution.bytes_per_second` 可单独限制公共流量。两项均为空时记录 `relay_pacing_disabled`，调度器仍选帧，可写帧无需等待令牌。[节点模板](../../../deploy/network/node.cn.json#L14) · [限速配置](../../../relay-rust/crates/relay-native/src/node/reload.rs#L135)

---

### 模块 2：网络管理台展示与操作已封禁 `restrictions` 列表

#### 现状问题

- [governance.rs](../../../relay-rust/crates/network-authority/src/governance.rs#L170-L174) 的 `GET /v1/admin/console` 已完整返回 `restrictions: { version, accounts, shares }`。
- [restrictions.tsx](../../../consoles/network/restrictions.tsx#L20-L36) 仅在用户输入具体 ID 或打开单条举报时，拿 `snapshot.restrictions` 做存在性判断，没有任何页面把当前已封禁列表渲染出来。

#### 设计方案（[consoles/network/restrictions.tsx](../../../consoles/network/restrictions.tsx) 与关联路由）

1. **在“举报与限制”工作区直接展示当前生效限制清单**：
   - 将现有 `#reports` 视图扩展为上下或分区清晰的两个区域（或带计数切换的二级视图）：**待查举报**与**生效中的限制（已封禁账号 / 已封禁分享）**，直接消费 `snapshot.restrictions`，无需新增后端请求。
   - **已封禁账号列表**：展示 base64url 账号 ID、快捷跳转到 `#account/<id>` 查看该账号资料与用量、以及行内“移除限制”按钮（调用 `api.restrict({ kind: 'account', id }, false)` 并以单调版本号回写 `snapshot.restrictions`）。
   - **已封禁分享列表**：展示 base64url 分享 ID、快捷复制按钮，以及行内“移除限制”按钮。
   - **空状态与反馈**：无封禁记录时展示简洁空态；解除限制的 `<Notice>` 挂在列表区块顶部（不放行内，避免行卸载时反馈消失），且不丢失列表滚动位置；同时导出 `DirectRestriction` 供 `accounts.tsx` 复用。
2. **精简限制入口，不留兼容死路由**：
   - `#share-access` 保留按未知分享 ID 手工查询并设置限制的能力，并在同页下方直接展示当前已封禁分享清单，避免管理员“封完即失联”；移除 `#account-access` 后顺手精简 `LookupAccess` 中多余的 `account` 分支。
   - 账号查询与限制入口直接并入模块 3 的 `#accounts` / `#account/<id>`，**移除旧的 `#account-access` 路由及按钮**（pre-v1.0 无历史用户，不做无意义的向后兼容跳转）。

---

### 模块 3：已注册账号列表、查询与单账号用量诊断（`network-authority` + `consoles/network`）

#### 1. 数据模型与安全边界（[relay-rust/crates/network-authority/](../../../relay-rust/crates/network-authority/)）

现有持久化键空间及新增的不可变注册时间索引键：
- `account/<account_id_b64>` → `Account { id, created_at, username, password, profile, version }`
- `account-index/<u64::MAX - created_at 20位补零>/<account_id_b64>` → `""`（仅在注册时写一次的不可变倒序索引键）
- `username/<normalized_username>` → `AccountId`
- `usage/<account_id_b64>/<utc_day>` → `u64`（当日已中转字节）
- `attribution/<account_id_b64>/<share_id_b64>` → `[u8; 12]`（关联的官方中转分享）
- `restriction/account/<account_id_b64>` → `Restriction::Account`

新增独立模块 `relay-rust/crates/network-authority/src/account_admin.rs`，定义只读视图结构（严禁复用包含 `HashedPassword` 的内部 `Account` 直接序列化给管理端），并保持字段归属单一、不把网络全局配置和查询时钟塞进每行账号实体：

- **`AccountSummary`**（列表行与查询返回共用的纯账号事实）：
  - `id: AccountId`
  - `created_at: u64`
  - `username: Option<String>`
  - `nickname: Option<String>`（取自 `profile.nickname.value`）
  - `email: Option<String>`
  - `has_password: bool`（仅返回是否已设置密码，绝不泄漏盐值和哈希）
  - `today_bytes: u64`（单键点查 `usage/<id>/<today_day>`）
- **`AccountPage`**：
   - `today_day: u64`（每次请求取一次 UTC 天序号，全页 `today_bytes` 共用该日期）
  - `entries: Vec<AccountSummary>`（不重复携带 `daily_limit_bytes` 与 `restricted`，避免与前端 `snapshot.settings.daily_bytes`、`snapshot.restrictions` 形成双数据源，同时省去每页 50 次多余的 `restriction/account/` 点查）
  - `next_cursor: Option<String>`（格式 `<created_at>.<account_id_b64>`，与 `ReportPosition` 一致，服务端校验后映射为 `account-index/` 键）
- **`DailyUsage`**：
  - `day: u64`（UTC 天序号）
  - `bytes: u64`
- **`AccountDetails`**（单账号诊断视图，自包含便于直接排查与 API 调试）：
  - `#[serde(flatten)] summary: AccountSummary`
  - `today_day: u64`
  - `daily_limit_bytes: u64`（来自当前 `settings.daily_bytes`）
  - `remaining_bytes: u64`（`daily_limit_bytes.saturating_sub(today_bytes)`）
  - `restricted: bool`（单键点查 `restriction/account/<id>`）
  - `entitlement_version: u64`
   - `recent_usage: Vec<DailyUsage>`（详情查询按需扫描 `usage/<id>/`，按 `day` 数值倒序取最近 30 个有用量的日期）
   - `has_older_usage: bool`（还有更早的日用量记录时为真）
  - `shares: Vec<[u8; 12]>`（有界扫描 `attribution/<id>/`，按分享 ID 字典序截取上限 100 条）
  - `has_more_shares: bool`

#### 2. 存储索引与游标分页（`registration.rs` + 复用 `Store::scan_after`）

- 不在 `authority_records` 上对 `account/*` 建 SQLite 表达式部分索引：因为 `account/*` 会在每个在线账号每分钟 `entitlement` 刷新时被更新以递增 `version`，建二级索引会给高频热路径带来持续写放大、额外消耗 Cloudflare DO 免费版每日 10 万行写入配额。
- 邮箱、分享归因和日用量沿用现有写入路径。待详情查询出现性能瓶颈时再优化历史用量读取。
- 趁当前 0 生产用户无历史数据迁移负担，在 [registration.rs](../../../relay-rust/crates/network-authority/src/registration.rs#L196) 创建账号时多写一条不可变索引键：
  `account-index/{:020}/{account_id_b64}`（值为空串，排序前缀为 `u64::MAX - created_at`）。若测试/预发环境存在早期测试账号，直接清库即可，不编写一次性迁移代码。
- 列表查询直接复用现有 `Store::scan_after("account-index/", after_key, Some(ACCOUNT_PAGE_SIZE + 1))` 实现按**注册时间倒序**稳定分页（每页 `ACCOUNT_PAGE_SIZE = 50`）：`Store` trait、`host/storage.rs` 与 `Memory` 均无需新增专用查询方法或手写 SQL。
- 在 `model.rs` 的 `Fault` 枚举新增 `AccountNotFound`，并在 `host/routes.rs` 映射为 HTTP `404`。

#### 3. Admin HTTP 路由与可观测性（`host/routes.rs`）

所有接口均归属 `/v1/admin/*`，复用现有管理员 Cookie 会话校验（`require_admin`）和 `edge.rs` 同源策略：

| 方法与路径 | 参数 | 行为与返回 |
|---|---|---|
| `GET /v1/admin/accounts` | 可选 `?after=<created_at>.<account_id_b64>` | 校验游标并映射为规范 `account-index/` 键，返回按注册时间倒序分页的 `AccountPage` |
| `GET /v1/admin/accounts/lookup` | 必填 `?q=<text>` | 轻量精准定位，命中返回 `AccountSummary`（不扫描 `usage/` 与 `attribution/`）。先对 `q` 去首尾空白，空串返回 `400 invalid`；若符合 16 字节规范 base64url 则先查 `account/<id>`，命中即返回；未命中或非合法 ID 格式时回退按 `username()` 规范化查 `username/<name>`（避免 22 字符合法用户名被误杀）；若输入非合法用户名（如管理员误贴邮箱或昵称）或查无此号，均统一返回 `404 account_not_found`，不返回令人困惑的 `400 invalid` |
| `GET /v1/admin/accounts/<account_id_b64>` | 路径为 22 字符 base64url `AccountId` | 返回完整 `AccountDetails`（资料、今日剩余额度、近期按天用量、已归因分享列表） |

细节注意：
- 在 `routes.rs` 的 `Context::new` 中扩展查询参数提取：`/v1/admin/accounts` 提取 `after`，`/v1/admin/accounts/lookup` 提取 `q`；两者都将原始参数交给 `Authority` 方法在 `require_admin` **之后**再校验（`admin_routes` 对 `GET` 不做统一鉴权，避免未登录请求先返回 `400 invalid` 而非 `403 administrator_session`）。
- `GET /v1/admin/accounts/lookup` 路由匹配必须排在 `path.starts_with("/v1/admin/accounts/")` 之前。
- 列表查询与单账号详情读取分别记录 `tracing::debug!(event = "authority_accounts_listed", ...)` 与 `tracing::debug!(event = "authority_account_read", ...)`，与节点/举报查询日志风格一致。

#### 4. 网络管理台前端（`consoles/network/`）

1. **主导航与路由扩展**（[App.tsx](../../../consoles/network/App.tsx)）：
   - 在顶部工作区导航栏新增 **账号**（`#accounts`，复用 `Icon` 现有 `user` 图标），与 `节点`、`举报`、`策略` 并列；同步收窄 `nodes` 导航高亮条件（排除 `accounts` / `account`），并移除旧的 `#account-access` 路由。
   - 支持路由：`#accounts`（账号列表与查询）、`#account/<id>`（单账号详情与用量诊断）。
   - 全局“刷新快照”按钮同步触发当前账号列表/详情的刷新版本号（与 `nodeRevision` / `reportRevision` 保持一致）。
   - 在 [api.ts](../../../consoles/network/api.ts) 中显式识别 `account_not_found`，避免查无此号时回落为笼统的“请求失败”。
2. **账号总览与列表页**（新增 `consoles/network/accounts.tsx`）：
   - **顶部摘要条**：注册账号总数（`snapshot.account_count`）、当前单账号每日额度（`snapshot.settings.daily_bytes`）、已限制账号数（`snapshot.restrictions.accounts.length`）。
   - **统一查询栏**：单个输入框明确标示为“账号 ID 或用户名”（避免误以为支持邮箱/昵称/分享 ID），回车调用 `GET /v1/admin/accounts/lookup?q=...`，命中后直接进入 `#account/<id>`，未找到给出清晰内联提示。
   - **账号表格**复用 `nodes.tsx` 的表格与游标分页。追加页的 `today_day` 与已加载列表相同时才合并，跨 UTC 日则重新加载首页。
     - 账号（有用户名/昵称时主显名称、下方等宽显示截断 ID；早期自动注册的无名账号直接以等宽 ID 作为主标题，避免满屏重复占位词；沿用 `nodes.tsx` 的 `<th scope="row"><a className="node-link">` 语义链接进入详情）
     - 注册时间（`DateStamp`）
     - 今日中转流量（已用 `entry.today_bytes` / 上限 `snapshot.settings.daily_bytes`，列头以显式 UTC `YYYY-MM-DD` 标明日期，不复用本地时区 `DateStamp`；已耗尽时使用 `warning` 徽标标出）
     - 状态（`正常` / `已限制`，单一数据源实时结合 `snapshot.restrictions.accounts` 渲染，封禁/解封或改策略后无需刷新整页即可同步）
3. **单账号详情与用量诊断页**（`#account/<id>`）：
   - **账号信息面板**：完整账号 ID、用户名、昵称、已验证邮箱、是否设置密码、注册时间、额度快照版本号。
   - **今日额度与近期用量面板**：
     - 今日额度使用同一份 `AccountDetails` 的 `today_day`、`today_bytes`、`daily_limit_bytes` 和 `remaining_bytes`，以易读单位展示并保留精确字节提示。
     - 按 UTC `YYYY-MM-DD` 展示日用量。`has_older_usage` 为真时标明仅列最近 30 个有用量的日期。空记录与加载失败分别显示。
   - **已归因官方中转分享面板**：列出 `attribution/` 中登记的官方中转分享 ID（上限 100 条，不误称为“近期分享”；当 `has_more_shares` 为真时明确标注“仅展示按分享 ID 排序的前 100 条”）；若账号本身已被限制，徽标明确区分“随账号限制”与“已单独限制”，且同时命中两者时保留“已单独限制”可见，避免管理员误判或重复逐条封禁。
   - **账号访问限制操作**：内嵌现有 `DirectRestriction` 组件，支持在详情页直接限制或解除该账号使用官方中转，并同步更新全局 `snapshot.restrictions`。当从已封禁列表点进一个尚未注册的合法 ID（`decode(id, 'account') !== null` 且接口返回 `404`）时，复用 `DirectRestriction` 保留解除限制入口；对畸形 ID 不渲染操作按钮。
4. **双语文案与构建产物**：
   - 同步补齐 [locales/en.ts](../../../consoles/network/locales/en.ts) 与 [locales/zh-CN.ts](../../../consoles/network/locales/zh-CN.ts) 新增文案，保持界面自解释、无冗余说明文字；不改动桌面端/CLI 客户端界面。
   - 执行 `pnpm --dir consoles build:network`，同步更新 `relay-rust/crates/network-authority/console-dist/`，并通过 `lint`、`typecheck` 与 `verify:dist`。

---

## 三、分步实施顺序与涉及文件

按底层无依赖 → 服务端 API → 前端界面与内嵌产物的顺序推进，每一步均独立可提交：

### 第一步：中转出口调度

- 调整 `relay-native` 的 `connection/{mod,pending}.rs`、`node/governor.rs` 及所需的 `relay-access` 调度接口。
- 用宿主多帧测试比较累计实发字节。两类持续可写时份额接近 `19:1`，同档两个账号分别开 1 个和 8 个会话时总份额相近。覆盖慢写端让出带宽，并对比改动前后的单流吞吐与小帧延迟。

### 第二步：Authority 管理端账号与用量只读接口
- 修改 `relay-rust/crates/network-authority/src/model.rs`：新增 `Fault::AccountNotFound`。
- 修改 `relay-rust/crates/network-authority/src/registration.rs`：注册新账号时同步写入不可变倒序索引键 `account-index/{:020}/{id}`。
- 新增 `relay-rust/crates/network-authority/src/account_admin.rs` 与 `account_admin_tests.rs`：基于现有 `Store::scan_after` 实现精简的 `AccountSummary`（`nickname` 取 `profile.nickname.value`）、`AccountPage`（顶层 `today_day`）、`AccountDetails`、`DailyUsage`，以及 `accounts`、`account_by_id`、`lookup_account`（均在 `require_admin` 之后再校验游标与 `q`；`.trim()`、22 字符用户名回退、非法非空输入统一 `AccountNotFound`、永不序列化密码哈希）。
- 修改 `relay-rust/crates/network-authority/src/lib.rs`、`host/routes.rs`、`authority_tests.rs`：挂载 `/v1/admin/accounts`、`/v1/admin/accounts/lookup`、`/v1/admin/accounts/<id>` 路由、查询参数透传、404 映射及测试模块引入。

### 第三步：网络管理台 UI（已封禁清单 + 账号与用量页）与产物更新
- 修改 `consoles/network/model.ts`、`consoles/network/api.ts`：补齐账号分页、统一查询与单账号详情类型、客户端方法及 `account_not_found` 处理。
- 修改 `consoles/network/restrictions.tsx`：导出 `DirectRestriction`；渲染 `snapshot.restrictions` 已封禁账号与已封禁分享列表，提供快捷跳转与行内解除限制（`<Notice>` 放列表区块顶部），移除旧的 `#account-access` 入口并精简 `LookupAccess`。
- 新增 `consoles/network/accounts.tsx`，实现账号分页、查询、用量详情与限制操作。
- 修改 `consoles/network/App.tsx`、`consoles/network/network.css`、`consoles/network/locales/en.ts`、`consoles/network/locales/zh-CN.ts`：接入主导航与高亮互斥、移除 `#account-access` 路由、刷新状态联动、样式与双语文案。
- 构建更新 `relay-rust/crates/network-authority/console-dist/` 并验证 `lint` / `typecheck` / `verify:dist` 与 `make sloc`。
