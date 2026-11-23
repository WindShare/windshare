# Network Authority 车队瘦身计划（方案 C）

保留“网页入网 + 在线签发/续签 + KV 同步”单一事实源，砍掉 `network-vantage` 探针验收体系与双端维护锁，并补齐轻量防护与可观测性提示，为后续 Issue #37 关系表重构扫清存储与状态机包袱。

## 一、边界总览：留什么 vs 砍什么

| 分类 | 内容 |
|---|---|
| **保留与轻量化增强** | 网页添加节点（`/.well-known/windshare` 实时抓包验签 + SSRF 防护 + 校验节点 `authority` 配置匹配 + 一键安装脚本生成）、默认以 `Canary` 入网（供运维定向自测和发布链接前按需补位）、Authority 在线签发目录与不可变别名账本、90 天自动续签、Cloudflare KV 同步与 Alarm 退避重试、公开目录端点 `/.well-known/windshare-network`、节点每分钟心跳负载（`NodeLoadReport`）及前端离线/策略同步/KV 同步状态提示、手动 `Activate`/`Drain`/`Disable`/`Remove` 与匿名开关、极简无锁宿主机升级回滚脚本 |
| **砍掉（负 ROI 编排）** | ① `network-vantage` 外部探针集群、Staging 异步抢占验收队列、`probe_ticket` / `PROBE_ACCOUNT` 特权通道、`mirror_lag` 上架门禁、只读探针周期表；<br>② 全局单机维护锁（`fleet/maintenance.rs`）、`cmd/network-upgrade` 滚更编排、`relay-native` 本地 `.admission` 维护门锁与维护 CLI 子命令、依赖维护门锁协议的重型 `install-wsrelay.sh` |

## 二、分模块改造清单

### 1. `relay-rust/crates/network-authority`（核心瘦身）

- **直接删除的文件**：
  - `src/fleet/vantage.rs`（探针抢占队列、`probe_ticket`、`mainland_current`、`silent`）
  - `src/fleet/monitoring.rs` 与 `monitoring_tests.rs`（`fleet_targets`、`record_cycle`）
  - `src/fleet/maintenance.rs` 与 `maintenance_tests.rs`（全局维护锁与 `abort`）
  - `src/host/fleet/maintenance.rs`（`FLEET_MAINTENANCE_TOKEN` 鉴权转发）
- **核心状态机与校验简化**：
  - `src/fleet/mod.rs`：
    - 删除 `DraftEnrollment`、`Verification`、`EVIDENCE_SECONDS`、`cancel_draft()`、`promote()`；
    - `NodeState` 从三层嵌套收敛为极简 4 态：`Canary | Active | Draining | Disabled`（去掉 `Staging` 和 `maintenance: Option<String>`）；
    - 删除 `Node.operation_id`。`Manage` 在同一事务内比对 `expected_state` 和 `expected_policy_version`（`node.version`），任一不符即返回 409 并提示刷新。`enroll()` 和 `manage()` 继续记录请求级 `operation`。
    - 将 `stage()` 改为直接入网并上架的 `enroll()`：**默认入库为 `Canary`**，对同 `node_key` 且同 `spec` 的重复提交在 `reserve()`/`project()` 前幂等早退（避免把已 `Active` 节点覆盖回 `Canary` 或白涨目录 `serial`），保留 `retired-node/<id>` 防回滚版本号递增（`old + 1`）与 `publication.reserve(&spec)` 永久别名绑定，提交后立即投影目录并同步 KV。
    - 复用已初始化的 Durable Object 时，先结束在途维护，再一次性转换 `node/*`。`Listed` 沿用原目录状态，`Staging` 按原草稿撤回逻辑处理。保留签名目录的 `generation`、`serial`、永久别名及现役和退役节点的策略版本。可丢弃的独立开发实例可直接重建。
  - `src/fleet/descriptor.rs`、`src/model.rs` & `src/host/fleet/edge.rs`：
    - `descriptor.rs` 将 `authority` 反序列化为 `Option<Authority>`；`discover_node` 校验其非空、`issuer` 严格等于 `AUTHORITY_ROOT`，`url` 去除尾部 `/` 规范化后匹配 `API_ORIGIN`、部署注入的 `AUTHORITY_ORIGIN` 或当前请求 origin（同时将 `AUTHORITY_ORIGIN` 加入 SSRF 受保护列表，兼容海外节点直指源站域名）；
    - 在 `DiscoveryFailure` 新增独立的 `AuthorityMismatch` 错误码，避免节点未配置或配错 `authority` 时复用 `InvalidDescriptor`/`DescriptorMismatch` 误导排查。
  - `src/fleet/operations.rs`：
    - `Action` 保留 `Activate`（从 `Canary`、`Draining` 或 `Disabled` 启用）、`Drain`（拒绝纯 STUN 节点）、`Disable` 和 `Remove`。后两者沿用 `cutoff_ready || early_cutoff` 校验。删除 `Verify`、`AbortMaintenance` 及 `entry()` 的维护期 STUN 特判。
  - `src/nodes.rs`：
    - 删掉 `maintenance()`、`operation_id` 及 `relay_drained()` 里的维护态分支；`admission()` 随 4 态映射，**保留纯 STUN 节点（`!spec.roles.contains(&Role::Relay)`）固定下发 `Admission::Draining`** 的 fail-closed 约束；
    - 在 `NodeDetails` 中由服务端时钟（`environment.now()`）计算 `report_stale: bool`（仅针对非 `Disabled` 且已上报过心跳的节点、超过 3 分钟未更新；`report: None` 时为 `false`），避免前端受本机时钟偏差、静置标签页误报及新入网/已停用节点假红标影响；**保持 15 分钟 SQLite 心跳落盘间隔不变**（不牺牲高频心跳写入成本，接受 DO 重启后 ≤60s 短暂自愈抖动）。
  - `src/usage.rs` & `src/registration.rs`：
    - 删除 `PROBE_ACCOUNT` 过滤与保留账号校验分支。
- **Worker 路由、Edge、KV 与定时任务简化**：
  - `src/host/fleet/route.rs`：从 14 条路由砍掉 10 条（删全部 `VantageRoute` 4 条、`MaintenanceRoute` 4 条、`AdminRoute::Promote`、`InternalRoute::Mirrors`），只保留 `Prepare`（仅做 admin session + CSRF 校验防 SSRF，无需再返回 `Envelope`）、`Enroll`（原 `Stage`，调用 `signer(env)` 并触发 `OnCommitPending` 写 KV）、`Manage`、`Tick` 共 4 条。
  - `src/host/fleet/edge.rs`：删掉 `/v1/admin/nodes/promote`、`handle_promote()` 与 `mirrors_match()`，只保留 `/v1/admin/nodes/discover` 和 `/v1/admin/nodes`。
  - `src/host/fleet/mod.rs` & `src/host/fleet/reconcile.rs`：
    - 删掉 `VANTAGE_KEYS` 读取、`origins()`、`MirrorCheck`、`mirrors_current`、`silent_vantages`、`vantages`，以及专供镜像检查的 `MIRROR_DEFER_HEADER` 与 `ReconcileOutcome::mirror_defer_reason()`；
    - 拉长 `RETRY_BACKOFF_MS`（如 `[2_000, 5_000, 15_000, 60_000, 300_000, 900_000]`，覆盖 ~20 分钟 KV 抖动），避免短暂 KV 故障只能等下一次 Cron；
    - `console()` 从现有 `directory/kv-delivery` 状态透出 `kv_sync_pending`（`!matches(digest) || failed_attempts > 0`，含 2s 写入冷却与重试待同步态），零外网请求即可在后台暴露同步状态；
    - `scheduled` / `tick()` 只做 3 件纯粹的事：目录 90 天到期自动续签（`head.renew`）、证书临期警告日志、KV 兜底对账（`AuditKv`）。
  - `wrangler.toml`：将 Cron 从 `*/5 * * * *` 调低为每小时 `0 * * * *`（24 次/天，几乎零免费额度消耗，且不会在 KV 异常或首次冷部署时卡数小时）。
  - `src/host/routes.rs`：删除 `/v1/admin/nodes/cancel`。
  - `src/bin/network-authority-keys.rs`：删除 `verify-vantages` 子命令。
- **测试清理与更新**：
  - 引入非默认 `test-release` Cargo feature（生产 `wrangler.toml` 的 `worker-build --release` 不开启，保持生产离线根公钥编译期钉死；将测试构建与运行收口进统一本地/CI 命令，免手敲参数），允许 Miniflare 注入测试 `Release` 根公钥并端到端测通 `enroll()`/`manage()`/KV 写入；
  - 精简 `src/fleet/{workflow_tests,operations_tests,resume_tests,descriptor_tests}.rs`、`src/{node_load_tests,authority_tests}.rs`、`src/host/fleet/{route_tests,reconcile_tests}.rs` 及 `tests/{fleet-edge.test.mjs,password-edge.test.mjs,local-worker.mjs,worker.mjs}`（在 `local-worker.mjs` 的 `signedDescriptor()` 补齐 `authority` 字段并更新节点序列化结构）。

### 2. `consoles/network`（管理后台前端）

- **删除**：`consoles/network/verification.tsx`（Staging 验收状态与取消草稿卡片）。
- **精简与展示增强**：
  - `model.ts`：删除 `VantageReport`、`RelayNode.operation_id`、`NodeState` 的 `staging` 与 `maintenance`、`FleetAction` 的 `verify` / `abort_maintenance`、`Publication` 的 `vantages` / `silent_vantages` / `mirrors_current`；在 `NodeDetails` 增加 `report_stale: boolean`，在 `Publication` 增加 `kv_sync_pending: boolean`。
  - `api.ts` 删除 `promote()`、`cancelDraft()` 及废弃错误映射（`vantage_key_bound`、`verification_required`、`maintenance_in_progress`、`mirror_lag`），补充 `authority_mismatch`。`manage()` 提交页面所见的 `expected_state` 和 `expected_policy_version`。
  - `fleet/operations.tsx`：删除 `ProbeWarnings`、`ProbeResults`、维护锁徽标与“中止维护”弹窗、`Verify again` 按钮；按钮组收敛为直观的 `Activate` / `Drain` / `Disable` / `Remove`（`Active` 态不堆砌多余按钮）。
  - `nodes.tsx` & `consoles/ui`：移除探针与 `mirrorLag`/`silentVantages` 徽标与 `staging` 分支；给 `consoles/ui` 的 `Badge` 补齐红色 `danger` tone（复用现有 `--error-ink`/`--error-surface`），在节点列表页补出 `Canary` 等状态徽标；保留每分钟心跳负载、目录版本与证书到期提醒，并补充 3 项低成本纯展示状态：
    - 依据服务端 `report_stale` 在列表和详情页对非 `Disabled` 节点标红提示离线/超时；
    - 节点在线（`!report_stale`）且非 `Disabled` 且 `applied_policy_version < node.version` 时提示“策略同步中”；
    - `kv_sync_pending` 为真时在目录状态栏提示“KV 同步待完成”（避免 2s 写入冷却期误报“同步失败”）。
  - `onboarding.tsx` 保留域名探测与一键安装。新节点以 `Canary` 入网，`refresh()` 后跳转详情页，运维确认策略同步并完成自测后即可 `Activate`。
  - 清理 `locales/en.ts` / `locales/zh-CN.ts` 废弃文案、补齐新文案，并重新构建 `relay-rust/crates/network-authority/console-dist/`。

### 3. `relay-rust/crates/relay-access` & `relay-native`（拔除探针特权与本地维护门锁）

- **`relay-access`**：
  - 删除 `src/probe.rs`（`PROBE_ACCOUNT`、`credential()`、`permits()`）及 `src/lib.rs` 导出。
  - 注意：不动 `relay-core` 中的 `ConnectionProbe`（正常 WebSocket 活性心跳帧）。
- **`relay-native`**：
  - 删除 `src/node/maintenance.rs` 与 `src/node/maintenance_tests.rs`（`.admission` 本地门锁文件与 `JoinLease`）。
  - `src/node/mod.rs` & `src/node/descriptor.rs`：删除 `Runtime.maintenance`、`Gate::install()`、`descriptor()` 中的 `open_to_users()` 及 `self.access.identify()` 调用。
  - `src/main.rs` & `src/main_tests.rs`：删除 `--enter-maintenance`、`--release-maintenance`、`--force-release-maintenance`、`--check-hold` 以及依赖 `maintenance::check_identity` 的 3 参数版 `--check-config <path> <node_key>`（保留 `--config <path>` 与 2 参数 `--check-config <path>`）。
  - `src/actor/access.rs`、`src/actor/ingress.rs`、`src/connection/mod.rs`：删除 `maintenance_refusal`、`admit_maintenance`、`join_lease`、`LocalMaintenance`、`MaintenanceGateError` 及对应测试。
  - `src/node/access/mod.rs`、`src/node/access/admission.rs`、`src/node/access/residency.rs`、`src/node/usage/mod.rs`：删除 `is_probe`、`scoped_probe`、`State.node_key`、`Service::identify`、`has_reserved_admission`、`ProbeScopeMismatch`、`PROBE_ACCOUNT` 豁免及对应测试。
  - 伴随死代码与测试清理（适配 `-D warnings`）：删除 `storage.rs` 中失活的 `Database::path()`、`open_read_only()`、`is_busy()` 及 `actor/access.rs` 的 `MAINTENANCE_RETRY`；同步精简 `node/descriptor_tests.rs`、`actor_access_tests.rs`（保留 `Draining` 下存量分享恢复覆盖，只摘掉 `Gate`）与 `node/usage/usage_tests.rs`。

### 4. Go 侧外围工具清理与客户端灰度选路补齐

- **先内联再删除 `internal/networkprobe/`**：
  - `cmd/network-sign/mirror.go` 与 `export.go` 依赖 `internal/networkprobe` 的 `LoadRelease` / `ResolveAuthority` / `ReleaseClient` / `Fetch`；先将这 ~80 行公共工具及对应单测迁入 `cmd/network-sign/`，保持 `cmd/network-sign` 独立编译运行（离线根授权、发布导出 `export`、大陆主机定时同步 `mirror`、验证 `verify`）。
  - **整目录删除**：`cmd/network-vantage/`、`cmd/network-upgrade/`、`internal/networkprobe/`。
  - **精简保留 `integration/relayv2/enrollment_probe_test.go`**：移除 `internal/networkprobe` 与 `PROBE_ACCOUNT` 依赖，通过 TLS 反向代理向真实 `wsrelay` 使用普通账号票据注册，并验证 `/.well-known/windshare` 跨语言描述文件契约。
- **签名目录内的中继选路**
  - `connectivity/relayplan` 按剩余链接名额和 `TargetNetworkRegistrations` 确定目标。可拨 `Active` 充足时只让它们并行注册，不足时用可拨 `Canary` 补位。`backupIndex()` 将 `notCanary` 排在 `withoutFailure` 与 `pairZone` 之间，并补相应单测。
  - 若首轮只选了 `Active` 且发布前注册全部失败，`connectivity/relayset` 在同一次分享中立即启用可拨的备用 `Canary`，沿用并发上限和服务端退避。
  - `engine` 在首个中继注册成功时发布链接。`PublishRelayHints` 保留注册中的候选，排除已被拒绝的节点和未启用的备用 `Canary`。后续重连沿用链接中的节点。

### 5. 部署脚本、CI 与文档

- `scripts/deploy/network-authority.py` & `network_authority_test.py`：移除 `VANTAGE_KEYS`、`FLEET_MAINTENANCE_TOKEN` 和 `verify-vantages` 校验；保留 `DIRECTORY_KV_ID`、`DIRECTORY_CERTIFICATE`、`DIRECTORY_SIGNING_SEED` 和 `verify-publisher`；当 `origin != host` 时将 `AUTHORITY_ORIGIN` 显式写入 Worker `[vars]`。
- `.github/workflows/network-authority.yml`：移除 `VANTAGE_KEYS` 和 `FLEET_MAINTENANCE_TOKEN` 环境变量传入；`verify` 阶段调用统一的 `test-release` Worker 测试入口。
- `scripts/deploy/network/install-wsrelay.sh` & `install-wsrelay.tests.sh`：移除全部维护门锁协议与操作目录状态机，**重写为极简无锁升级脚本**——保留脚本级 `flock` 防并发踩踏（调 `runuser`/`systemctl` 前 `9>&-` 关闭锁 FD）、`sha256` 校验 → `--check-config` → 原子切换 `current` 软链 → 在允许失败的条件分支内执行阻塞式 `systemctl restart`（不用 `--no-block`）与本机 `/healthz` 轮询（避免 `set -euo pipefail` 下启动即崩跳过回滚；存在上一版软链时自动切回并重启，首次安装直接报错退出）→ 保留最近 3 个版本清理旧目录；同步重写 `install-wsrelay.tests.sh`（移除对 `cmd/network-upgrade/rollout.go` 的契约读取，`scripts/ci/linux/hygiene.sh` 继续保留对应精简测试）。
- `deploy/network/windshare-network.service`：清理提及 `install-wsrelay.sh` 维护协议与 `flock leases` 的过时注释。
- 同步 `deploy/network/operations.md`、`relay-rust/crates/network-authority/README.md` 和 `docs/plans/global-service/p3-console-onboarding-fleet-automation.md`。TLS 到期、STUN 可达性与大陆镜像同步由外部监控或 systemd timer 告警。
  - 自测新中继时，发送端使用独立的 `WINDSHARE_CONFIG`。受限中继先用 `wind relay onboard <url>` 配对一次。用 `wind share <file> --relay <url>` 分享小文件，再从接收端运行 `wind get <link> --connectivity relay-only` 确认下载。纯 STUN 节点用 UDP Binding 自测。
  - 计划升级可先 `Drain`。等新心跳确认 `applied_policy_version == node.version` 后，参考 `active_sessions == 0` 择机重启。存量分享仍可能有接收端加入，抢修时可直接重启。

## 三、改完后对 Issue #37（关系表重构）的影响

本次精简直接消除了 `vantage-lease/*`、`fleet-cycle/*`、`maintenance-complete/*`、`directory/mirrors` 四类杂散键、僵尸字段 `operation_id` 与嵌套状态机。Issue #37 可据此整理车队存储。

1. `nodes` 表：`node_key`, `spec_json`, `status`（`canary | active | draining | disabled`）, `anonymous`, `policy_version`, `load_checkpoint_json`（另配 `retired_nodes` 表保 `version` 防回滚）。
2. `directory_state` 单行表：当前已签名目录 `envelope_json`、永久别名映射 `aliases_json`、KV 同步交付状态 `kv_delivery_json`。

## 四、建议的拆分执行顺序

1. 移除两端维护锁及 `cmd/network-upgrade`，切换到带健康检查和回滚的宿主机升级脚本。
2. 先迁移 `cmd/network-sign` 需要的工具，再移除探针与 `Staging`，接通 `Canary` 入网、目录签发和发布前兜底。
3. 同步控制台并重建 `console-dist`，更新部署脚本、CI 与运维文档。
4. 推进 Issue #37 的 SQLite 关系表、免事务免克隆读路径与 TTL 清理。
