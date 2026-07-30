# Windows 网络测试重构计划

状态：提案

## 目标

在保留真实 relay、WebRTC、CLI、浏览器和进程生命周期验证能力的同时，让
Windows 验证符合直觉、易于维护，并足够快地服务日常开发。

核心决策是：**正确性测试不得依赖固定路径的可执行文件身份**。只有当可复现
的二进制身份本身是测量要求时，才可将 D5 保留为可选的性能或证据工具。

## 目标测试模型

| 层级 | 目的 | 执行方式 | 热运行目标 |
|---|---|---|---:|
| 快速契约 | 纯协议、调度、密码学和注入式故障行为 | 每次修改运行 `make check` | 60 秒 |
| 原生集成 | 真实 loopback TCP/UDP/WebSocket/WebRTC 和有界子进程 | `make integration`；Windows/Linux PR CI | 2 分钟 |
| 产品关键路径 | CLI share/get、relay 回退、P2P 接管、重连、停止及代表性浏览器下载 | `make e2e`；PR CI | 3 分钟 |
| 完整浏览器矩阵 | 昂贵的浏览器与网络拓扑组合 | `make browser`；合并前或定时 CI | 单独测量 |
| 性能证据 | 可复现的吞吐量、性能剖析和发布证据 | 显式手动或发布命令 | 不作为正确性门禁 |

`go test ./...` 必须在 Windows 和 Linux 上直接运行原生 loopback 测试，不设置
操作系统专属授权门禁。昂贵测试可以遵循 Go 明确的 `-short` 约定，但不得因为
可执行文件缺少 D5 权限而静默跳过。

## 架构

- 将 `internal/testnetwork` 的授权职责替换为小型资源 fixture，只负责分配
  loopback 临时端口、有界 context 和确定性清理。Fixture 负责提供资源，不决定
  测试是否有权运行。
- 在消费端边界保持网络、时钟、文件系统和进程依赖可注入，让大部分行为留在快速、
  确定性的测试中。
- 保留小规模真实栈测试，验证 mock 无法证明的行为：socket 绑定、Pion 协商、
  relay framing、子进程终止和重连。
- 将 Job Object、Named Pipe 等真正的 Windows 语义隔离在专门的 Windows
  集成包中，不让它们为无关测试授予网络权限。
- 为集成测试中的 Pion 实例显式配置 loopback candidate。防火墙提示和主机防火墙
  状态都不查询，也不参与测试结论。
- 为进程和 E2E 运行分配稳定 operation ID，并输出结构化里程碑日志，使超时能够
  定位到所属场景和清理边界。

## 门禁设计

- `make check`：使用 `-short` 运行确定性测试；不运行浏览器矩阵，也不构建
  D5。这是开发内循环入口。
- `make integration`：使用普通 Go test runner，在两个操作系统上运行不带
  `-short` 的原生网络和原生进程包。
- `make race`：直接包含相同的原生集成路径，删除独立的 D5 网络重放。
- `make coverage`：每个包只进行一次普通 coverage 执行，并保留现有 core、
  root 和单包阈值；删除 D5 专属 profile 拆分与合并。
- `make ci`：每项门禁只组合一次，不因为更换编排包装器而重复运行等价的完整包
  测试。
- 托管 CI：Linux 继续作为量化覆盖率权威；Windows 必须运行真实原生集成测试，
  不能在 OS-network 用例跳过后仍报告完整成功。PR 只运行一个代表性的 Windows
  浏览器冒烟路径，完整跨浏览器和拓扑矩阵不进入开发内循环。

## D5 去留

从正确性验证中删除：

- `RequireOSNetwork`、`AssertOSNetwork` 和 runner capability pipe；
- SSA 网络策略分析器及精确包清单；
- 固定路径哈希授权、执行计划和网络调度；
- D5 专属 coverage 组装及强制 `make network` 重放。

只有在存在明确测量需求时才保留：

- 稳定 benchmark 二进制、不可变证据和性能 profile；
- 性能工具下的窄而显式的命令，且不与产品正确性测试共享 import 或运行时契约。

迁移完成后不得长期维护两套架构。临时对拍最多保留一个变更集，随后删除旧路径。

## 迁移步骤

1. 盘点当前所有 gated 场景，将其映射到产品或操作系统专属 oracle，并记录冷热
   运行时间及不稳定情况。
2. 引入 loopback/进程 fixture，将 relay、WebRTC、CLI 和 E2E 包改为普通
   Windows 执行；每组经过重复验证后再删除对应 D5 路径。
3. 让直接 Windows 集成成为本地和托管 CI 的权威；简化 race 与 coverage，使
   每种 instrumentation 对每个包只执行一次。
4. 将 Browsergate 进程 containment 与网络授权解耦，只保留它实际验证的进程
   生命周期语义。
5. 删除 D5 正确性子系统、过期文档、清单和 skip 标签；有合理依据的性能部分移到
   可选命令之后。

## 验收标准

- Relay、P2P、回退、重连、停止、CLI 文件夹传输和代表性浏览器下载继续拥有真实栈
  覆盖。
- Windows `go test ./...` 不再出现依赖 D5 的 OS-network 跳过。
- Windows CI 实际执行网络集成，不能用仅 Linux 成功作为 Windows 行为证据。
- 永不查询、修改 Firewall/WBEM 状态，也不将其用于测试结论。
- 覆盖率继续满足 core 总计 >=90%、root 总计 >=80%、每个包 >=70%。
- 在参考开发机上，`make check` 热运行不超过 60 秒，`make integration`
  热运行不超过 2 分钟；完整本地 CI 不超过重构前记录的基线。
- 每个受支持 CI 操作系统连续运行 20 次原生集成都通过，且没有泄漏子进程或占用端口。
- 正确性测试不再依赖静态分析器、包注册表、固定可执行文件路径、二进制哈希或全局
  授权环境变量。

## 非目标

- 管理或推断 Windows Firewall 策略。
- 用 mock 替代全部真实网络验证。
- 每次本地修改都运行完整浏览器和拓扑矩阵。
- 仅因 D5 已经存在而保留其兼容性。
