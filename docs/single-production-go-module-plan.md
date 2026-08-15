# 单一生产 Go Module 执行计划

## 决策

WindShare 将只使用一个生产 Go Module，根路径为
`github.com/windshare/windshare`。`core/` 继续作为无网络依赖的应用与协议边界，
但不再拥有独立的 `go.mod`、版本和发布流程。

`internal/perfevidence` 与 `spikes/webrtc` 继续作为隔离的证据模块，通过显式命令
运行，不再因此保留覆盖整个仓库的 workspace 文件。

这次调整优先保证源码提交自洽，并让本地与 CI 使用完全一致的依赖解析。只有当
`core` 出现真实外部消费者，并拥有独立的兼容性与发布生命周期后，才重新考虑将其
作为独立 Module 发布。

## 目标结构

- 根 `go.mod` 管理包括 `core/**` 在内的全部生产 Go package。
- 现有 `github.com/windshare/windshare/core/...` package import path 保持不变。
- `core/**` 不得依赖 core 之外的根模块 package、具体传输、信令服务商、relay
  实现、CLI 代码或重型网络库。
- Core 继续使用独立的测试范围，总覆盖率不低于 90%，每个 package 不低于 70%；
  非 core 生产 package 继续遵守现有根模块门槛。
- 日常本地门禁与 GitHub CI 在不使用 `go.work` 的情况下解析同一依赖图。
- 根模块发布流程验证唯一对外发布的生产 Module 与可执行程序。

## 实施顺序

### 1. 建立显式 package 边界

- 基于 `go list -deps` 为 `core/**` 增加生产依赖图门禁，拒绝 core 路径之外的
  WindShare package，并禁止 `net/http`、WebSocket、Pion 等网络与传输能力。
- 对第三方 Module 使用显式白名单；初始仅允许现有的 CBOR、`x/sys`、`x/text`
  和 `float16` 依赖，任何新增项都必须经过审查。
- 将不带 `-test` 的跨平台生产依赖图与测试依赖增量分开检查；允许原生测试所需的
  具名能力，但不得借此放宽生产依赖。依赖门禁至少覆盖 Linux、Windows 与 Darwin
  源文件。
- 在仓库 hygiene 门禁和日常 GitHub CI 中运行该检查。

该门禁直接表达 WindShare 真正需要维护的架构约束，取代由 Module 划分偶然形成的
边界。

### 2. 合并生产 Module 元数据

- 将 core 的依赖合入根 `go.mod`，并通过 `go mod tidy` 整理 `go.sum`。
- 删除 `core/go.mod` 和 `core/go.sum`。
- 删除根模块对 `github.com/windshare/windshare/core` 的 require。
- 删除 `go.work`；其余证据模块通过 `go -C` 显式运行。
- 确认所有生产 package 的 import path 均未改变。
- 更新 e2e 与性能证据工具的仓库定位逻辑：解析根 `go.mod` 并精确校验 Module path，
  不再依赖 `go.work`、`core/go.mod` 或仅凭 `go.mod` 存在判断仓库根。

### 3. 重建 Go 验证职责

- 以 `go list ./...` 作为完整生产 package 集合，以 `go list ./core/...` 得到 core
  集合，再通过全集减法生成非 core 集合，禁止维护容易遗漏 `e2e`、`integration`
  或未来顶层 package 的硬编码目录列表。
- 门禁验证两组集合互不相交且并集等于全集；每组 package 只运行一次，避免遗漏或
  重复执行高成本测试。
- 保留独立的 core 与 root 覆盖率 profile 和门槛。
- Core coverage 保留 `github.com/windshare/windshare/core` 的 `local-prefix`，从根目录
  执行 checker 时显式指定 `--source-dir=core`。
- 将 check、vet、lint、vectors、coverage、长测试及各平台脚本中的 `go -C core`
  调用，替换为针对正确 package 集合的统一 Module 命令。
- 清理 Makefile 目标、CI 契约测试及 workflow 缓存中的双 Module 假设；所有
  `cache-dependency-path` 只使用根 `go.sum`。
- Linux 与 Windows 原生行为继续由现有对应测试套件负责。

### 4. 移除双 Module 发布控制面

- 删除 `root-release-graph` 及其 Linux、Windows 调度脚本。
- 删除 core candidate workflow、tag 解析器及仅用于发布 `core/vX.Y.Z` 的脚手架。
- 删除旧发布脚本前，逐项迁移其中的 Linux loop-ext4、Windows NTFS、进程重启、
  inode 复用和互斥锁恢复认证；快速测试留在普通 CI，重型认证由 weekly 与根模块
  release 负责。
- 评估并改造确定性 Module 归档 helper 供根模块发布复用；只有纯 core 发布逻辑才
  直接删除。
- 删除 `core-release` Make target 和 candidate tag 相关文档。
- 将发布验证统一到根模块：tidy diff、依赖校验、构建、测试、Module 归档检查，
  以及从 release candidate 安装 CLI。
- 生产发布只创建根模块语义版本 tag。

### 5. 同步文档与仓库约定

- 更新 `AGENTS.md`、`docs/validation.md`、`core/README.md` 和当前路线图，以
  package 边界取代独立 Module 描述。
- 删除 candidate tag、两提交功能交付和消费已发布 core 的过时说明。
- 记录未来拆分 core 的触发条件，不为尚不存在的消费者提前承诺兼容性。

## 风险与控制

- **Module 依赖图扩大：**通过显式依赖门禁维护 core 纯净性；core 消费者的 package
  编译图仍不会包含不可达的根模块 package。
- **验证重复或变慢：**生成互不重叠的 package 集合，每个测试套件和覆盖率运行只保留
  一个负责人。
- **根 Module 归档变大：**检查发布归档，避免将生成文件、缓存和重型非源码资源纳入
  生产发布。
- **未来拆分 core：**将其视为一次正式的产品/API 迁移，在承诺稳定公开 core API 前，
  明确选择新的 Module path 与仓库。

## 交付方式

将迁移作为一个完整变更实施，避免任何已提交状态同时包含统一源码引用与过期的已发布
core 元数据。迭代期间运行聚焦的 Go 和 CI 门禁，交付前运行 `make ci`。本次迁移不得改变
协议、密码学、传输、文件系统安全或任何用户可见行为。
