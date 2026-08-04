# CI 恢复与简化执行计划

> 状态：待执行。本文只重构验证与测试基础设施，不改变 v2 即时分享的产品语义。
> 本地调用者、主机和同用户进程属于可信边界；CI/benchmark 产物不是第三方发布证据。
> “可信”表示不防御同用户进程的主动恶意篡改，不表示文件系统状态不可变化；产品仍须处理正常并发变化与远端输入，以维持路径边界、revision、恢复和原子发布语义。
> 完成后删除本文，长期入口只保留在 `validation.md`。

## 已确定决策

- 决策判据：直接保护远端输入、路径边界、内容一致性、恢复和发布语义的代码保留；只为证明测试环境可信、
  隔离同用户进程、归并跨 workflow 证据或提供发布审计的代码删除或简化。调用者、本机和同用户进程视为可信。
- 保留 E2EE、能力链接、恶意输入校验、root confinement、续传、revision/lease、崩溃恢复、原子发布和连接切换。
- 删除仅服务 CI 证据、artifact、OIDC、历史 reducer 和进程 settlement 的控制平面；不得按名称或目录删除 `core/osfs` 的产品 namespace authority、native identity claim 或 ancestry revalidation。
- 每个 pull request 与 `main` push 阻塞：static、Go vet、short race+coverage、协议向量、Web、Linux Go E2E、Linux Chromium 和 Windows native。
- long-Go、durable、relay/TURN 小矩阵、Windows 浏览器/process、Firefox/WebKit 只由 weekly 自动运行；不恢复大矩阵。
- 本地 `make ci` p95 ≤10 分钟；普通 CI 关键路径 p95 ≤6 分钟，均计入测试准备时间，超时只作挂死保护。
- `make ci` 不安装工具、依赖或浏览器；覆盖率阈值保持 core 90%、root 80%、单包 70%，不降低 lint/SLOC 约束。
- `core-candidate/v*/**` 通过 Linux/Windows release check 后自动创建 `core/v*` tag。
- 仓库已有 ruleset/branch protection 时只同步失效的 required job 名称；不得新启用强制 PR、限制 push 或改变所有者直接推送 `main` 的权限。没有现有保护规则时不新增。

## 背景

即时分享重构同时引入了大量与产品行为无关的 validation authority、evidence、identity、
artifact sealing、release readiness 和跨进程权限框架。后续修复继续围绕这些框架展开，导致
真正的测试被多层控制代码包裹，验证系统本身成为主要维护对象。

当前事实（数量为实施前基线，统计时须区分生产代码、测试代码和 harness）：

- `4ff987b` 的 GitHub CI 作业直接并行执行，完整运行约 2 分 40 秒。
- 此后 101 个提交中有 42 个修改了 `.github`、`scripts/ci` 或 `web/scripts`。
- 上述验证范围已经增长到约 8 万行。
- 远端普通 CI 自 `4ff987b` 后没有成功记录；最新一次运行超过 7 分钟仍在前半段失败。
- 当前 `make ci` 串行组合 race、coverage、synthetic core release 和浏览器进程验证；本机仅
  `make hygiene` 就运行了 190 秒并失败。
- root `./...` 会让 process E2E 同时进入 race 和 coverage。Windows race 还执行 32 MiB
  relay/Pion 进程场景，使昂贵行为重复运行并掩盖真实失败位置。

根因不是质量门太多，而是测试分类失真，并且测试执行被建模成了安全证据控制平面。单人、单机、
pre-v1.0 项目不需要用 OIDC、临时用户、ACL、精确 SHA 证据消费和多阶段 verdict 来证明自己刚执行过测试。

## 目标状态

- 工具、项目依赖和当前平台 Chromium 已安装的本地环境中，`make ci` 不要求预热 Go 编译缓存；
  日常验证 p95 不超过 10 分钟。
- pull request 与 `main` push 的普通 CI 关键路径 p95 不超过 6 分钟；排队时间不计，checkout、setup、
  依赖安装和缓存未命中计入。`timeout-minutes: 10` 只作挂死保险丝。
- 每周自动运行 long-Go、耐久故障、relay/TURN 小矩阵和跨浏览器 smoke，关键路径不超过 10 分钟；不建立大矩阵。
- 普通远端 CI 保留 Windows build、vet 和 short tests/race；Windows 浏览器/完整 process smoke 放入 weekly，
  本地 Windows `make ci` 仍覆盖当前平台产品路径。
- `core-candidate/v*/**` 自动运行完整 release check，通过后才由最小发布 job 创建 `core/v*` tag。
- 保留真实产品行为、协议向量、有效测试和质量约束；删除只为控制、封装或证明验证流程而存在的代码。

保留的门禁：

- gofmt、whitespace 和退役 v1 生产引用检查；
- `sloc-guard` 的文件与目录边界；
- 现有 `golangci-lint` 约束（包括 golint-style 规则、认知复杂度、嵌套深度、函数长度及正确性规则）；
  不恢复独立的旧版 `golint` gate；
- gopls，作为本地 `make ci` 的最后一个 gate；
- root 总覆盖率至少 80%、core 总覆盖率至少 90%、每个 Go 包至少 70%；
- Go↔TypeScript 黄金向量、Web lint/typecheck/build/Vitest；
- 最小真实 Go process E2E 和 Chromium 产品路径；
- E2EE、能力链接、恶意输入、路径约束、root confinement、恢复及连接切换等产品保护。

`core/osfs` 继续保留 root confinement、断点续传、临时文件、revision/lease、崩溃恢复和原子发布。
其中 `SessionNamespaceAuthority`、`SessionAuthority`、`BoundFileRecord`、`ResumableFileAuthority`、`OutputAncestryBinding`、live native guard 与 identity revalidation 属于产品运行时；只能在保留上述语义并完成引用分析后重构。删除范围仅限 CI evidence authority、控制面协议和纯 harness 故障矩阵。

## 测试分层

先从权威产品合同列出产品承诺，再为每项承诺指定一个 canonical owning gate；单元测试、协议向量和
E2E 可以提供互补保护，但同一昂贵场景不能被多个 gate 无意重复执行。

| 层级 | 职责 | 运行位置 |
|---|---|---|
| short | 纯逻辑、包级集成、小型故障注入 | `make check`、合并的 short race/coverage |
| critical E2E | 真实 CLI/relay/浏览器的短产品路径 | `make ci`、普通 GitHub CI |
| long | 大目录、durable fault cuts、长进程/网络和跨浏览器小 smoke | 每周自动 workflow |
| release | core 独立发布物、提取后构建测试和发布检查 | candidate workflow |

每个昂贵产品场景只能有一个 owning gate：协议互操作由 vectors，最小 sender/relay/receiver 路径由
critical E2E/Chromium，relay cut 与 durable 场景由 weekly，独立 core 发布物由 release。其他层只能提供
互补的小型单元或故障注入，不得重复启动同一大文件、进程或网络矩阵。

现有渐进 catalog、多 receiver、relay→Pion 切换、receiver crash/resume、错误诊断和浏览器 hot-switch
场景在简化前须逐项归入 critical E2E、weekly long 或等价的低层合同。具体归属由实施时按耗时、故障定位
价值和覆盖重叠决定；只有被等价合同明确覆盖的重复 harness 才可删除。

Go 测试采用原生双边界：`testing.Short()` 负责从日常 sweep 排除昂贵场景，混合包中的 weekly 测试统一使用
稳定的 `TestLong...` 顶层命名，使 `make long-go` 能以固定 package 和 `-run '^TestLong'` 反向选择。只包含
长场景的 `integration/...` 包可以直接整包运行；critical process E2E 使用 `TestCritical...` 命名并由
`make e2e` 显式选择。不得引入自定义 suite registry、环境变量选择协议或 build tag 矩阵。

本地日常 Go gate 用一次 `-short -race -covermode=atomic` 运行同时产生 coverage profile；critical E2E 只选择
最小产品场景，long gate 不得用 non-short `./...` 重跑 short 和 critical E2E。独立
`race`/`coverage` 只作诊断，不再被 `make ci` 串联。先逐包比较 full 与 short 的覆盖率差异，再为每个包选择短边界、long owning gate，或删除/排除
纯 harness。产品包不得用排除项掩盖 short coverage；仅删除后仍属纯 harness 的代码才可在 `.testcoverage.yml`
使用带注释的 `exclude.paths`。GitHub 实施时以少量重复运行先比较合并 job 与 race/coverage 并行 job 的墙钟时间，
采用更快且满足 6 分钟目标的静态结构，再用普通 workflow 的原生时间戳持续确认 p95；若合并 job 超出目标，
固定拆成 race 与 coverage 两个并行 job，不恢复
`changes`/`required-verdict` 或动态选择框架。critical process E2E 直接执行一次；long 测试由每周 workflow
接管。不得让 long 测试因 `-short` 跳过后失去执行入口，也不得降低覆盖率换取速度。若 short coverage
不足，应把状态机、调度和故障分支重构为可注入的小型测试边界，而不是让 coverage 重跑真实大文件和进程场景；不得降低
core 90%、root 80% 或单包 70% 阈值。

relay/Pion cut 不再依靠 16/32 MiB payload 制造时序窗口。应在稳定协议里程碑注入 relay cut，以小 payload
验证切换和字节完整性；真实进程只保留一个短 smoke。

## 本地环境边界

本地工具链和项目依赖由开发者安装和维护。`make ci` 只消费当前环境，不运行 `go install`、带版本的 `go run`、
`pnpm install`、浏览器下载或系统包管理器。Go、Node、pnpm、GNU Make、golangci-lint、gopls、
sloc-guard、actionlint、Web dependencies 和 Chromium 缺失时由对应命令直接报错，不建立 prerequisite
扫描框架。Go 命令使用 `GOTOOLCHAIN=local`，module dependency 缓存解析仍由 Go 负责。

GitHub-hosted runner 使用最新大版本的官方 setup action 和工具准备当前工具链并复用缓存；依赖安装只发生在
对应 job 内。删除 action commit SHA、`gopls.version`、工具精确 patch 及其固定版本自测；项目依赖仍由
Go module 与 pnpm lockfile 冻结，不能把依赖锁定误删成工具链清理。

## Makefile 边界

Makefile 只保留公开 target、固定顺序和一次平台入口选择，不负责安装、测试分类、动态命令、重试、证据、
artifact 或 GitHub policy。每个 target 对应直接命令或短平台脚本，不能改造成巨型 shell/Node orchestrator。
默认本地顺序执行；若实测无法达到 10 分钟，只允许把无共享状态的固定 gate 分组并行，GitHub 并行仍由 workflow DAG 表达。

| 入口 | 直接职责 |
|---|---|
| `make check` | root/core short tests、Web unit/typecheck 的快速反馈 |
| `make hygiene` | gofmt、`git diff --check`、退役生产引用扫描 |
| `make sloc` | 直接运行 sloc-guard |
| `make workflow-lint` | 直接运行本地 actionlint |
| `make lint` | 使用现有配置直接检查 root 与 core |
| `make vet` | root/core vet 与必要的 `GOWORK=off` 构建；不重复 short-go 已覆盖的普通编译 |
| `make short-go` | root/core 各一次 short race+coverage，并执行现有覆盖率阈值 |
| `make race` | 仅供诊断的 root/core short race |
| `make coverage` | 仅供诊断的 root/core short coverage 与阈值 |
| `make vectors` | 重生成并比较协议向量 |
| `make web` | ESLint、TypeScript/Vite build、Vitest |
| `make e2e` | 直接运行一次最小产品 process E2E |
| `make browser` | 直接运行一次 Chromium 产品 smoke |
| `make gopls` | 直接运行本地 gopls 检查 |
| `make long-go` | 显式运行命名的 long suite/package，不进入 `make ci` |
| `make core-release` | 验证提取后的独立 core 发布物，不进入普通 CI |
| `make ci` | 运行 hygiene、sloc、workflow-lint、lint、vet、short-go、vectors、web、e2e、browser、gopls |

## Chromium 与进程测试边界

普通 Chromium smoke 直接启动真实 sender/relay，接收一个含小文件的微型目录并验证页面状态和输出字节。
GitHub 使用 Linux Chromium，本地使用当前平台 Chromium；Windows 浏览器 smoke 由 weekly 覆盖。目标运行
30–60 秒，硬超时不超过 90 秒；热切换、渐进 catalog 和证据采集不得继续捆绑在同一个 smoke 中。

删除独立 `browser-process` gate。现有 browser-evidence process runner、authority frame、settlement schema、
publisher self-test 和进程 authority proof 不属于产品行为。真实 E2E 只保留最小进程启动、readiness、超时、
终止、退出码和诊断；若 Windows Job Object 或 Linux subreaper 是可靠清理子进程所需的最小机制，则保留其实现和小型测试。

普通远端 CI 不运行 Windows Chromium 或完整 Windows process E2E。Windows job 只负责 native build、vet 和
short tests/race；weekly 运行一个小型 Windows 浏览器/process smoke，当前 Windows 产品路径仍由本地 Windows
`make ci` 覆盖。M2 Windows 桌面实现后，再为真实 GUI、文件系统和安装流程增加针对性 E2E。

## GitHub Actions 结构

普通 `ci.yml` 在 pull request 与 `main` push 上运行，保留同 ref 的 superseded-run cancellation，删除
change-range selector、required-verdict 和文档变更特判。以下是逻辑分组；static 内若实测形成关键路径，固定拆成
互不依赖的 hygiene/SLOC 与 lint/gopls job，不引入动态选择框架。主要 job 独立并行：

1. static：hygiene、SLOC、workflow lint、Go lint、gopls；
2. go-root：root vet、合并的 short race/coverage；
3. go-core：core vet、合并的 short race/coverage、独立模块构建；
4. web：frozen dependency state 下的 lint、build、Vitest；
5. go-e2e：Linux 最小真实 process E2E；
6. browser-chromium：Linux 直接 Playwright Chromium 产品 smoke；
7. windows-native：Windows build、vet 和 root/core short race，不生成重复 coverage。

普通 CI 关键路径 p95 不超过 6 分钟；`timeout-minutes: 10` 只作挂死保险丝，不是预算，setup、安装和测试都计入
测量。连续稳定测量后再收紧 timeout。若 Windows short race 实测使普通 workflow 超出目标，则普通 job
只跑 short tests，Windows race 移入每周 workflow。

删除聚合 verdict 后，只有仓库已存在 required checks 时才同步最终采用的固定 job（包括拆分后的 static job），
并删除旧的 `required-verdict` 名称。不得借此启用强制 PR、push 限制或移除所有者的 `main` 直接推送能力；
普通 CI 是 push 后的自动验证，不是所有者推送前的授权门。

普通 job 直接运行目标命令，不上传“成功证据”再由另一个 job 重新证明。失败 diagnostics 仅在需要时上传，
并避免保存能力链接 fragment。成功路径不生成大型 artifact。

每周自动 workflow 只运行以下有明确产品价值的 long 入口，总墙钟时间不超过 10 分钟：

- Linux/Windows `go test ./integration/...` stability 套件；
- `core/catalog` scale 与 `core/osfs/internal/outputruntime` durable 场景；
- relay cut/proxy join 进程测试（`e2e/relay_cut_proxy_test.go`、`e2e/relay_proxy_join_test.go`）；
- direct/TURN/relay 小型网络矩阵和一个 Windows 浏览器/process smoke；
- Web D1/D2 WebRTC interop（`web/test/transport/webrtc/pion-interop.spec.ts` 及其
  `browser.playwright.config.ts`）和 Firefox/WebKit 小 smoke。

不得恢复 45 组网络身份、100 次稳定性样本、全量组合矩阵或证据归并。
Chromium 是普通 CI 阻塞浏览器；Firefox/WebKit 当前只由每周 smoke 覆盖。

core release 不进入普通 `make ci`。推送 `core-candidate/vX.Y.Z/<candidate>` 后，workflow 在 Linux 和 Windows
运行完整 `make core-release`；全部通过后，仅最终 job 获得 `contents: write` 并为同一 commit 创建
`core/vX.Y.Z`。同版本 candidate 串行处理；正式 tag 已存在时仅允许其指向同一 commit，禁止移动，并让重复运行
保持幂等。不再把已经公开的正式 tag 错当成发布前门禁；手动 dispatch 只用于诊断。

## 删除与保留边界

删除 change selection、required verdict、exact-SHA consumer、release reducer、Browsergate authority、OIDC、
sealed artifact、generated reducer、验证沙箱、stability evidence、browser-process proof、工具版本固定自测及其专用代码和文档。
`internal/perfevidence` 改为直接 benchmark runner 与 JSON 输出；`internal/processowner` 只保留
Job Object/Linux subreaper、超时和清理。`core/osfs` 只删除纯 CI/test harness 包装；产品 namespace authority、native identity、ancestry revalidation、输出安全和恢复语义必须保留或等价重构。

保留协议与产品测试、relay/Pion/恢复/浏览器行为、v1 生产引用扫描、最小 E2E 进程管理与必要的 OS 子进程清理、
ProtocolSessionID/OperationID/场景 ID/结构化阶段日志以及有回归价值的 Go benchmark。benchmark 不再依赖
sealed snapshot、权限沙箱或发布证据框架，性能入口退回直接、可重复的 Go benchmark runner；JSON 结果不作为
正确性或发布门禁。

删除前用引用图确认消费者。不能因为文件位于 test/CI 目录就假定无价值，也不能为保留已退役包装器而
保留整条依赖链。

## 执行顺序

1. 固定当前统计基线和测量环境，区分生产代码、测试代码与 harness；从协议、威胁模型和产品澄清中列出产品承诺，
   指定 short、critical E2E、long 或 release 的 owning gate；逐包记录 full-vs-short 覆盖率差异。
2. 建立 `testing.Short()` 和 critical/long 包边界，先保证所有被跳过的长测试都有每周入口，short coverage
   仍满足现有阈值；纯 harness 仅在删除后按路径排除。
3. 先建立可独立调用的 short-go、critical E2E、Chromium smoke 和 long 入口；将 relay/Pion cut 改为小 payload、
   稳定里程碑和直接 Playwright/Go 执行，在旧 workflow 旁验证这些入口。
4. 用已经验证的直接入口重写 Makefile 和普通 `ci.yml`，实现快速 `make check`、本地 `make ci` p95 不超过
   10 分钟、普通 CI 关键路径 p95 不超过 6 分钟。
5. 切断并删除失去消费者的 CI evidence/permission/browser-process 框架及其自测；`core/osfs` 仅按具体符号和生产消费者重构，保留 namespace authority、native identity、ancestry revalidation 及其产品语义。
6. 修复薄入口暴露的真实产品或测试问题，不修复即将删除的控制平面自测。
7. 分别测量本地、普通 GitHub CI 和每周 workflow；优先消除重复编译、重复安装、重复测试和串行依赖，
   仅在现有 ruleset/branch protection 中同步失效的必要检查名称，并保持所有者直接推送 `main` 的权限不变。
8. 扫描全部活跃文档和配置的旧 workflow、target 与 evidence 引用，简洁更新 `AGENTS.md`、`validation.md`、
   `performance.md`、`web/README.md`、`后续计划.md` 等实际受影响文件，删除本计划和过期控制平面文档。

耗时只通过命令计时和 GitHub Actions 原生时间戳判断，不新增性能 evidence、artifact、reducer、采样服务或耗时门禁。
实施期间不改写用户其他未提交文档，不用全仓回退代替依赖分析，也不以删除产品测试、降低覆盖率或
放宽 lint/SLOC 规则换取速度。
