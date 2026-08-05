# 浏览器产品验证恢复执行计划

> 历史参考：`E:\Doraemon\IT\Repository\WindShare-before-c0d5dcc` 固定在大幅删除前的提交 `39c3adf`，便于直接查看旧实现。
>
> 状态：待执行。目标是恢复产品关键行为的直接验证归属，不恢复 Browsergate、OIDC/broker、密封证据、截图或视频证据、精确 SHA 归并、权限沙箱或托管进程证据控制面。实现完成后，长期命令只保留在 `docs/validation.md` 和 `web/README.md`。

## 已确定决策

- Chromium、Firefox 和 WebKit 都是产品支持边界。Chromium 阻塞普通 CI；Firefox/WebKit 由自动 weekly 验证。
- 维持现有输出语义：无原生文件系统能力时，portable download 上限仍为 64 MiB 且不承诺刷新恢复。本计划不重做输出后端。
- 精确 1,000,000 个 ZIP 成员仍是产品规模合同，只在 Chromium periodic 运行。
- 64 MiB 实体分配不作为浏览器合同；固定上限由纯测试锁定，溢出和清理由小型可注入边界验证。
- 不增加公共 STUN、普通 CI ICE-failure、测试注册表或 sender Observer。后者如有需要，另设产品可观测性任务。

## 验证分层

| 层级 | 直接职责 | 自动归属 |
|---|---|---|
| Chromium short | 现有 relay 产品 smoke；浏览器存储、输出、加密、媒体和密钥生命周期的短合同 | `make browser`、普通 CI |
| Cross-browser weekly | Firefox 真实产品热切换；WebKit 原生能力选择与 relay fallback；三引擎代表性输出、目录和媒体合同 | weekly |
| Chromium periodic | 百万成员 ZIP、完整 crash-cut 和跨进程恢复 | weekly |

`web/e2e` 负责真实 sender/relay/UI 产品路径；`web/test/browser` 负责必须在真实浏览器 API 上运行的组件合同。不得把后者描述成完整产品 E2E。

## 执行步骤

### 1. 先固定现有基线

- 单独复现并解决当前 Windows placement-guard 的 `Access is denied` 失败；不得用浏览器测试分类掩盖既有 `make ci` 失败。
- 记录普通 `make browser`、`make ci` 和 weekly 浏览器入口的命令墙钟；只使用本地命令计时和 GitHub 原生时间戳，不建立时间证据系统或阻塞式性能门。

### 2. 建立薄的浏览器合同入口

- 新增 `web/playwright.contract.config.ts`，独占 `web/test/browser/**/*.spec.ts`，直接管理一个 loopback Vite server 和 `baseURL`。
- `chromium-short` 自动运行所有非 periodic 文件；`firefox-short`、`webkit-short` 只运行 `*.cross-browser.spec.ts`；`chromium-periodic` 只运行 `*.periodic.spec.ts`。
- 不建立 suite registry 或 owner scanner。目录 catch-all 保证新 spec 默认进入 Chromium short；跨浏览器和 periodic 归属通过结构化文件名表达。
- `make browser` 依次运行现有 Chromium relay smoke 和 `chromium-short`。`browser-weekly-supplement` 只增加其余项目，并继续运行现有 progressive、network、interop 和 cross-browser 产品场景。`make web` 与 `make check` 保持不变；GitHub 任务复用现有依赖和浏览器安装。

### 3. 按产品语义重构测试集

- 将 `output-capability.spec.ts` 和大型 probe 按 portable output、durable recovery、catalog persistence、media preview、crypto runtime 拆分；删除 R0/R5 里程碑式命名。
- 删除 `scaffold.spec.ts`、始终跳过的 headed picker，以及 `r0-storage-contract.spec.ts` 中直接操作裸 OPFS/WritableStream、未经过 WindShare 生产模块的检查。独有规则只有在生产合同缺失时才迁移。
- 将 schema-v5 版本、store 和 index 名称断言改为：不兼容本地数据库被安全重建，旧所有权不被复用，新事务可以正常提交和重开。
- Chromium short 保留真实 IndexedDB 目录事务、小型单文件/ZIP、代表性生产 OPFS/FSA/IndexedDB 恢复、会话隔离与清理、活动浏览器 Curve25519 runtime/fallback 和媒体行为。
- Firefox/WebKit 只运行四字段输出能力、小型 portable 下载/ZIP、不兼容数据库重建、Curve25519 runtime/fallback 和媒体预览。
- 将百万成员 ZIP、全部 checkpoint crash phase、完整跨进程恢复和其余生产恢复矩阵移入 Chromium periodic；短合同使用小数据和已存在的 limit、clock、crash hook、exporter 注入边界。

### 4. 恢复真实产品连接合同

- 保留普通 Chromium smoke 的显式 relay-only 注入，使普通 CI 不依赖 ICE。
- 为 weekly 建立未注入 `RTCPeerConnection` 的跨浏览器产品场景。Firefox 复用现有 hot-switch 产品路径，验证 relay→P2P 后继续传输并得到精确字节；场景和 operation ID 必须反映实际浏览器。
- WebKit 先按原生能力分支：RTC 可用时等待 peer adoption 后切断 relay，并验证后续 peer dispatch；不可用时不得 skip、模拟 P2P 或切断 relay，应在首个 relay dispatch 后释放输出栅栏，等待传输终态，再断言全过程没有 peer adoption/dispatch 且 relay 完成同一负载。
- D1/D2 Pion interop 以 `pion-interop.spec.ts` 作为唯一聚焦适配器合同，不把它当作 Firefox 产品路径的替代。删除 `browser.spec.ts` 前，依赖真实 API 的原生关闭与配置拒绝语义并入该合同，其余归入 Vitest；`browser.playwright.config.ts` 继续作为 Pion 合同 owner。现有 Chromium direct、TURN 和 hot-switch 仍由 weekly owner 负责。

### 5. 恢复最小能力密钥合同

- 将 `DirectProcess` 的可选 `redactStdout` 替换为必选、按流表达的诊断披露策略；原始捕获只供 readiness 匹配，所有对外诊断统一由一个消费不可变快照、递归处理嵌套错误的纯 formatter 生成，能力输出始终脱敏，同时保留经过策略筛选的安全 stderr。
- capability-bearing `page.goto`、附件和最终错误统一使用同一个按实际 fragment、完整 URL 和分离密钥值工作的 redactor，防止 Playwright 重新输出秘密；canary 只作为测试向量，不作为脱敏规则，不恢复 artifact scanner。
- Vitest 验证 location 捕获/擦除顺序、统一诊断格式化和 canary 脱敏。Chromium short 验证实际启动时 fragment 在 gateway 工作前清除，以及分离密钥输入在 controller 与错误 UI 处理前同步清空。
- Capability 场景禁用 trace、video 和 screenshot；其他失败依靠结构化场景、operation 和阶段日志诊断。

### 6. 接入并清理

- 只有新 owner 运行通过后，才删除三个休眠 Playwright 配置、过时脚本和旧引用；不得删除现行 smoke、progressive、network、interop 或 cross-browser 配置。
- 实现完成后简洁更新 `docs/validation.md` 与 `web/README.md`，删除本计划；历史实现只通过 Git 提交引用。

## 实施验证

迭代时先运行相关 Vitest 和单个 Playwright 项目，再运行 `chromium-short`、跨浏览器项目和 `chromium-periodic`。交付前在具备所需浏览器的主机运行一次 `make ci-full`；`make browser-weekly` 只用于聚焦复现，不能在同一交付验证中重复执行。确认 Chromium short 不重复运行。普通 `make browser` 的 warm-cache 目标仍为 60 秒内；超出时先消除重复启动和不必要的实体规模，不能删除产品语义或降低既有质量门槛。
