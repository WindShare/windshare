> **P0 prerequisite:** Complete the [generated-semantic build isolation plan](browsergate-generated-semantic-build-isolation-plan.md) before running `make browser` / `make ci` or declaring this plan complete. Contract-only design work may continue, but full browser validation remains blocked until P0 settles.

main.tests.mjs 确实没有进入任何持续门禁。
workflow-contract.tests.mjs 本身正在阻塞 CI，但它只能验证 WindShare 特有的工作流语义，不能承担 YAML/GitHub Actions 合法性校验。真正缺失的是持续执行的 actionlint。

我需要在产品功能体验、架构设计、可维护性、开发体验(本地测试运行时间)、最佳设计等角度找到平衡。


---

我会选这个平衡点：**目录约定 + 极薄 runner + 分层门禁**。不建议只追加命令，也不建议为 13 个测试维护一套复杂 registry/schema。

## 推荐结构

```text
scripts/ci/browsergate/
├── tests/
│   ├── contract/          纯逻辑、禁止子进程
│   ├── process/           真实进程边界
│   └── suite-discovery/   随产品套件执行的 Playwright discovery
├── testsupport/           assertions、fixtures，不可直接执行
└── test-runner.mjs
```

目录本身就是策略：

- `contract`：runner 自动发现、稳定排序、串行执行；每个文件在独立 Node 子进程中加载 child-process guard。
- `process`：只放真实创建子进程的检查，由 `test:browser:process` 执行。当前 `runtime-command-owner.tests.mjs` 不会启动进程，应迁入 `contract`。
- `suite-discovery`：保留现有 `preExecutionDiscovery` 归属，由 main/pion 产品套件各执行一次；不得并入全局 `browser-preflight` 或 main-only `preflightIntegration`。
- `testsupport`：禁止使用 `.tests.mjs` 后缀，杜绝“看似测试、实际空跑”。

runner 是允许创建测试子进程的外层基础设施，本身不加载 guard；它对每个 contract 执行 `node --import <guard> <test>`。零测试、发现或启动失败、signal、非零退出都必须使门禁失败，同时输出包含稳定 test ID、耗时和结果的结构化记录。发现与结果归并保持为纯函数，执行器可注入测试，不承载 owner/policy 业务模型。

## main/discovery 怎么拆

1. 将 `main.tests.mjs` 改为纯 contract entrypoint，纳入自动发现。
2. 把 [playwright-discovery.tests.mjs](../scripts/ci/browsergate/playwright-discovery.tests.mjs#L272) 中真实 `spawnSync` 的部分迁到 `suite-discovery`，并让 orchestrator 的现有 `preExecutionDiscovery` 入口指向它。
3. 剩余注入 fake launcher 的断言放进 `testsupport/playwright-discovery.assertions.mjs`。
4. `main` 继续受 child-process guard 保护，不给它开例外。否则未来意外新增子进程也会被放过。

这是边界最清晰的设计：

```text
纯编排语义 → 快速 contract
真实进程行为 → process/suite-discovery
浏览器用户路径 → browser E2E
```

## 运行时间分层

| 层级 | 内容 | 本地入口 | 成本 |
|---|---|---|---|
| 快速合同 | Vitest contract + main 编排合同 | `make browser-contract` | 当前 12.6s，修复后约 13–14s |
| Workflow lint | actionlint | `make workflow-lint` | 热缓存约 0.7s，首次约 30s |
| 产品浏览器 | discovery、main/pion、真实浏览器 | `make browser` | 分钟级 |
| 完整验证 | 全部门禁 | `make ci` | 提交前/CI |

我不建议立刻把 browser contract 强塞进每次 `make check`。先提供独立的 `make browser-contract`：

- 普通 Go、UI、小范围业务修改：`make check`
- browsergate、Playwright 分区、CI 编排修改：再跑 `make browser-contract`
- 提交前：`make ci`

如果后续测得 `make check` 热运行仍能稳定控制在团队接受的预算内，例如 60 秒，再考虑合并进去。

新增 `scripts/ci/browser-contract.ps1/.sh`，让 `Makefile` 继续只负责分发；将 `browser-contract` 和 `workflow-lint` 加入独立本地入口，而不是重复加入 `GATES`。`make ci` 仍分别通过 `browser` 和 `lint` 获得这两项门禁。

## actionlint 怎么接最舒服

建立同一逻辑门禁的跨平台入口 `scripts/ci/workflow-lint.ps1/.sh`：

- 固定 `actionlint@v1.7.12`
- 使用 `-shellcheck= -pyflakes=`，避免 Windows/Linux 因外部工具差异产生不同结果
- 无文件参数，自动扫描所有当前和未来 workflow

然后：

- `make workflow-lint` 直接调用它，给 workflow 作者快速反馈。
- 现有 `lint.ps1/.sh` 在昂贵的 golangci-lint 之前调用同一脚本。
- 因此 `make ci` 和 GitHub CI 自动获得阻塞能力，又没有重复实现。

首次 30 秒主要是 Go 下载/编译；之后约 0.7 秒。我认为没必要为了优化一次性冷启动，引入跨平台二进制下载、校验和与缓存管理。

## Workflow semantic contract

保留它，但明确职责：

- `actionlint`：YAML、GitHub Actions schema、表达式、action inputs。
- semantic contract：WindShare 的 DAG、凭证隔离、artifact、deadline、reducer 权威。

随后把自制缩进解析替换为 YAML AST。使用 `parseDocument` 严格拒绝 parser errors 和重复键；别名展开必须有命名上限，semantic contract 不得检查 parser 产生的部分结果。该检查必须能独立失败，不能依赖 actionlint 已先运行。将它移入现有 Vitest browser-contract 测试树，并把锁文件中已有的 `yaml` 提升为直接 devDependency，使其自动被 Vitest 发现。

## 落地顺序

1. 拆分 discovery 的纯合同与真实进程检查。
2. 建目录约定和 fail-closed runner，接入 `main`，迁移误分类测试，删除空跑入口。
3. 增加跨平台 Make 入口和 actionlint 门禁。
4. 最后把 workflow semantic contract 改为严格 YAML AST。
5. 验证 runner 的零测试、启动失败、signal、非零退出合同。
6. 验证 `make browser-contract`、`make workflow-lint`、`make browser`、`make ci`。
