# Windows 网络测试重构计划

状态：已决策

## 目标与决策

在保留真实 relay、WebRTC、CLI、浏览器和进程生命周期验证的同时，让 Windows
测试可直接运行、可诊断且适合日常开发。

- 正确性测试不得依赖固定路径、二进制哈希或 D5 进程授权。
- 根模块 `go test ./...` 默认运行确定性的 Go 测试、loopback 集成和 Go 进程
  E2E；独立的 core 模块必须另行运行 `go -C core test ./...`。
- `-short` 只跳过超出快速检查预算的真实资源场景；短小 loopback 测试可保留。
  Playwright 不隐藏在 Go runner 中。
- PR 保留 Browsergate contract、跨平台 process correctness 和 Windows Chromium
  关键路径；完整跨浏览器与网络拓扑矩阵仅在 nightly 和 release 运行。
- 删除 D5 混合架构。必要的稳定 benchmark、source snapshot 和 profile 能力迁入
  独立性能工具；归档 R8，并基于新 runner 建立不可与 R8 直接比较的新基线。

## 目标测试模型

| 层级 | 内容 | 入口 | 热运行 p95 目标 |
|---|---|---|---:|
| 快速检查 | root/core `-short`、Web 单测及现有 vet/lint/typecheck | `make check` | 60 秒 |
| 原生集成 | loopback TCP/UDP/WebSocket/WebRTC、受控进程生命周期 | `make integration`；Windows/Linux PR | 2 分钟 |
| 产品关键路径 | CLI share/get、回退、P2P 接管、重连、停止、Windows Chromium 下载 | `make e2e`；PR | 3 分钟 |
| 完整浏览器矩阵 | 三浏览器与网络拓扑组合 | `make browser`；nightly/release | 单独测量 |
| 性能证据 | 稳定二进制、吞吐量、profile 和发布证据 | 显式手动或 release 命令 | 不作为正确性门禁 |

时间目标先作为趋势告警；覆盖率、正确性、资源泄漏和清理失败立即作为硬门禁。

## 测试所有权

- 跨组件流程使用语义明确的 integration/E2E 包；组件内短小 loopback 测试保留在所属包。
  suite 调度不得依赖测试名正则、SSA 分析器或包注册表。
- `testing.Short()` 只表达运行成本和真实资源边界，不承担平台授权；不得仅因使用 socket
  就跳过快速测试。
- E2E 二进制由按需、suite 级 fixture 构建；`TestMain` 不得在测试筛选前无条件构建。
- 每个场景必须记录唯一 owner、oracle、目标 OS、instrumentation、timeout 和 cleanup
  owner，删除旧路径前必须已有等价新 oracle。

## 资源与进程架构

- 用独立的 loopback fixture 替换 `internal/testnetwork`。它返回仍由 fixture 持有的
  listener、`PacketConn` 或 Pion socket owner，不返回关闭后等待再次绑定的裸端口。
- 子进程监听 `127.0.0.1:0`，通过结构化 ready 事件发布实际地址，避免端口预留竞态。
- 确定性 Pion 集成测试通过注入的 API/`SettingEngine` 限制 loopback candidate、
  local/remote IP 和 network type；完整浏览器拓扑矩阵保留独立的非 loopback profile。
  两者都不得改变生产默认值。
- 进程 fixture 与网络 fixture 分离，并由测试进程外的监督者持有进程树。Windows
  后端在目标执行前完成 kill-on-close Job Object containment；Linux 后端使用 process
  group、父存活控制管道和 subreaper。抽取复用现有 Browsergate owner 的平台机制，
  不维护第二套 containment。超时、测试退出和父 runner 异常都必须终止并回收整棵树。
- Job Object、Named Pipe 等 Windows 语义保留专门集成测试，但其 runtime 后端仍可
  被所有 Windows 进程测试用于 containment。
- 测试不查询或修改 Firewall/WBEM，也不以防火墙提示或主机策略决定结果。

Make/CI 为每次 integration/E2E 调用生成唯一 run ID；直接 `go test` 时由包进程生成
fallback run ID。每个场景生成 operation ID，并传递给 Go、Node、浏览器及子进程。
结构化日志至少包含 `run_id`、`operation_id`、`scenario`、`component`、`milestone`
和 `outcome`。

## Instrumentation

- `make race` 对 root 和 core 各执行一次完整 race sweep；root sweep 已包含其
  integration/E2E 包，不得再追加重复执行。
- E2E build fixture 使用显式 `plain|race` profile；race 测试启动的 WindShare 和 relay
  子进程必须使用 `-race` 构建。普通 coverage 不对这些子进程采集 coverage。
- `make coverage` 对 root 和 core 各执行一次普通 coverage，保留 core 总计、root 总计
  和单包阈值；删除 D5 profile 拆分与合并。测试 fixture 仅可作为有依据的结构性排除。

## 门禁 DAG

- `make check` 是开发内循环入口，不直接作为 `make ci` 的包装子项。
- `make integration` 只运行原生 integration 包；`make e2e` 在两个 OS 运行 Go 进程
  E2E，并在 Windows 额外运行一个 Chromium 冒烟路径。
- `make ci` 表示当前 OS 的 PR 门禁镜像：保留现有 vet、core-release、vectors、web、
  hygiene、lint、sloc、Browsergate contract/generated-semantic process 和 OS process-owner
  门禁，并加入 integration/E2E；每个 `(suite, instrumentation)` 只组合一次。托管 PR
  verdict 由 Windows 与 Linux 门禁的并集组成。
- `make ci-full` 在 PR 等价门禁之外加入完整 `make browser`，供 nightly/release 使用。
- 删除独立 `make network`。Linux coverage 继续作为量化覆盖率权威；Windows CI 必须
  自己执行原生网络、进程和 Chromium 关键路径，不得用 Linux 成功替代 Windows 证据。

## D5 删除边界

从正确性验证中删除：

- `RequireOSNetwork`、`AssertOSNetwork`、child authority 和 runner capability pipe；
- SSA 网络策略分析器、精确包清单、固定路径哈希和执行计划；
- D5 coverage 组装、`make network` 重放及全局授权环境变量；
- Browsergate 对 D5 授权的依赖。

新性能工具保留有效 workload、不可变 source identity、稳定 benchmark 二进制、profile
和资源上限 oracle；可独立验证的产品正确性 oracle 迁入普通测试。性能工具不得导入
正确性 fixture、提供网络授权或承担测试调度。迁移对拍最多保留一个变更集，随后删除
D5 代码、命名、文档和 R8 活跃数值基线。

## 迁移步骤

1. 盘点所有 gated 场景，建立 owner/oracle/suite/OS/instrumentation/timeout/cleanup 映射，
   记录冷热耗时及不稳定情况。
2. 建立 `testing.Short()` 边界、按需 E2E build fixture、loopback socket owner 和跨平台
   process-tree fixture。
3. 迁移 relay、WebRTC、CLI、E2E 和 Browsergate；Pion 使用注入式 loopback 配置，
   race 模式向子二进制传播。
4. 在 Windows/Linux 各以独立进程、`-count=1`、无重试连续运行 20 次 integration 和
   Go E2E，并各完整运行一次 race、coverage 和 Browsergate process；Windows 再运行
   Chromium 关键路径。通过后将直接 Windows 执行切换为本地和托管 CI 权威。
5. 简化 race、coverage 和 CI DAG；抽取性能工具，归档 R8 并建立新基线。
6. 删除 D5 正确性子系统、旧清单、skip 标签、过期文档和兼容路径。

## 验收标准

- Relay、P2P、回退、重连、停止、CLI 文件夹传输和代表性浏览器下载继续拥有真实栈
  oracle。
- Windows `go test ./...` 不再出现 D5 OS-network 跳过；core 通过独立命令完整运行。
- Windows CI 实际执行原生 integration、Go 进程 E2E 和 Chromium 冒烟路径。
- race E2E 启动的 WindShare 与 relay 子二进制同样经过 race instrumentation。
- 覆盖率继续满足 core 总计 >=90%、root 总计 >=80%、每个包 >=70%。
- 参考机记录 OS、CPU、RAM、Go、Node、pnpm 和 Playwright 版本；至少 20 次独立热运行
  记录 p95，目标为 `check <=60s`、`integration <=2m`、`e2e <=3m`。完整本地 CI 记录
  迁移前后同口径基线；超限只产生趋势告警。
- Windows/Linux 的 integration 和 Go E2E 切换烟测各连续 20 次无重试通过。Nightly
  发布包含 run、commit、OS、suite 和 outcome 的版本化 JSON artifact；release reducer
  通过 CI API 校验最近 100 次无重试 integration 结果并执行完整浏览器矩阵。缺失或
  非法结果使验收失败，任何可复现的正确性失败阻断 release。
- 每次运行都确认进程树为空且 fixture 持有的 listener/`PacketConn` 已关闭；不得以重新
  绑定端口作为清理 oracle，清理失败直接使测试失败。
- 正确性测试不再以静态分析器、包注册表、固定路径、二进制哈希或全局环境变量作为
  网络运行授权。
- 永不查询或修改 Firewall/WBEM 状态；超时日志能够由 run/operation ID 还原场景、
  里程碑和清理边界。

## 非目标

- 管理或推断 Windows Firewall 策略。
- 用 mock 替代全部真实网络验证。
- 在每次 PR 运行完整浏览器和网络拓扑矩阵。
- 保留 D5 或 R8 runner 兼容性。
