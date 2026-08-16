# CLI UX 与可观测性改造计划

## 目标

本次改造只解决三个高频问题：

1. `share` 立即给出一条易复制的有效链接。
2. `get` 展示真实、稳定的发现与传输进度。
3. 默认输出保持简洁，`-v, --verbose` 和 `--trace` 提供逐级排障能力。

不预扫分享目录，不为发送端退出摘要引入高成本的精确流量统计，也不扩展剪贴板或复杂脚本功能。

## 输出契约

### 能力信息

- `stdout` 始终是唯一的能力信息通道，不因 TTY、重定向或参数组合而改变。
- 默认输出一行完整能力链接：`Link: <url>`。
- `--split-key` 输出 `Bare link: <url>` 和 `Key: <key>`。
- 默认界面不再从完整链接中额外展示 Key。
- 完整链接、Key、认证 token 和私钥不得进入 `stderr`、verbose 或 trace。

### 人类界面

- 默认状态、进度、警告、错误和最终结果写入 `stderr`。
- `stderr` 是 TTY 时启用动态刷新、颜色和受支持的 Unicode 符号。
- `stderr` 被重定向时关闭动态刷新和 ANSI；默认只保留 warning、error 与最终摘要，显式 `-v, --verbose` 额外输出静态里程碑行。
- `-v, --verbose` 不改变链接、结果或退出码。
- `--trace <file>` 将结构化事件写入独立文件，不支持与人类输出混流的 `--trace=-`。

## 用户界面

### `windshare share`

链接在 relay 注册和运行时组装成功后立即写入 `stdout`；写入成功后才发布 ready 事件。链接写入失败时停止分享并返回运行期失败。目录后代继续按需发现。单文件只读取本身的类型与大小；单目录只显示目录类型；多个输入显示选中项数量。

```text
  Link: https://windshare.app/...

  ✨ WindShare is ready
  ➜ Sharing: vacation_photos/ (directory)
  ➜ Relay: Connected

  Waiting for receivers · Press Ctrl+C to stop
```

`Link:` 来自 `stdout`，其他行来自 `stderr`。正常停止只输出可信事实，不推测尚未采集的发送流量：

```text
  ✔ Sharing stopped
```

### `windshare get`

发现未完成时不显示百分比或 ETA：

```text
  ⚡ Content path: Direct
  🔍 Discovering… 14 files | 8.2 MB ready | 2.4 MB/s
```

发现完成后显示确定性进度；速率与 ETA 只在样本有效时出现：

```text
  78% | 27/34 files | 111.2/142.5 MB | 14.8 MB/s | 2s left
```

完成后使用实际发布路径和既有文件结果语义：

```text
  ✔ Download completed in 9.6s
    Destination: ./vacation_photos/
    34 downloaded · 142.5 MB
```

最终 renderer 必须保留既有结果语义：顶层结果仍为 success、partial、paused 或 failed，source drift 是正交的失败原因和退出分类；同时分别报告 downloaded、resumed、collision、item-blocked 与 failed，非成功结果不得使用 completed 文案。

窄终端按宽度依次省略 ETA、速率、进度条和次要计数，保留当前阶段及可用的主要进度；无可靠字节百分比时保留文件计数。

## 真实进度模型

现有 `SelectionMeasure.CompletedBytes` 只在整文件完成时增长，不能直接计算实时速率。新增独立、单调的接收进度快照，区分：

- `DiscoveredFiles` / `DiscoveredBytes`：当前已认证发现的选择范围。
- `PublishedFiles` / `PublishedBytes`：已经安全发布的结果。
- `VerifiedBytes`：当前 receive operation 已认证并被输出事务接纳的唯一字节，包含可复用的恢复范围。
- `NewlyVerifiedBytes`：当前 transfer job 新认证并被输出事务接纳的唯一字节，不包含恢复数据、重复块或重传副本。
- `FileOutcomes`：当前已知的 downloaded、resumed、paused、collision、item-blocked 与 failed 终态计数。
- `Discovery`：open、complete 或 failed。
- `CountersExact`：总量是否仍可精确表示；计数饱和后为 false。

恢复范围只从输出 authority 接纳的 durable checkpoint 计入 `VerifiedBytes`。新范围在认证完成、被输出事务接纳且确认契约通过后同时计入 `VerifiedBytes` 和 `NewlyVerifiedBytes`：durable 输出要求 checkpoint 范围精确前进，transient/live-only 输出允许空范围 checkpoint 以 generation 前进确认本次写入。文件安全发布后才计入 published。界面以 `VerifiedBytes` 表示完成度，以 `NewlyVerifiedBytes` 的时间窗口增量计算端到端有效速率，不统计原始 wire 流量。

只有 discovery complete 且 `CountersExact` 时才显示百分比；还需没有已知的非成功文件终态、剩余字节大于零且速率样本稳定时才显示 ETA。`VerifiedBytes == DiscoveredBytes` 但 published 尚未追平时显示 finalizing；总字节为零时改用文件终态进度，只有成功 settlement 才显示 100%。

core 只更新轻量计数，CLI 按固定间隔读取快照；数据热路径不得执行终端或文件 IO。数据量与速率统一使用十进制 B、KB、MB、GB。

发送端暂不承诺文件数或字节数摘要。以后只有在 receiver、重连、重传和多 lane 的统计语义明确后再增加。

## 事件与渲染边界

建立小型、强类型的 CLI 事件模型，不创建跨项目的通用事件总线：

- core 只提供传输事实、进度快照和稳定决策，不包含终端文案。
- CLI 将事实投影为 ready、connected、fallback、progress、warning、failed、completed 等命令事件；投影边界将错误转换为类型化错误码和安全文案，renderer 不接收原始 provider 错误文本。
- 默认、verbose 和 trace renderer 消费同一组事实，各自决定展示粒度。
- `TerminalCanvas` 是 `stderr` 的唯一写入协调者，负责清行、日志插入、重绘和结束换行。
- 现有测试专用 `processTrace` 保持独立，不由用户 `--trace` 替代。

默认模式展示当前内容路径及影响用户结果的 warning 和 error。握手细节、重连尝试和 lane epoch 变化只在 `--verbose` 中展开；底层稳定决策只进入 trace。

## Trace 契约

`--trace <file>` 使用版本化 NDJSON。每条事件包含 schema version、run-scoped sequence、wall time、elapsed time、level、event、command 和随机 `run_id`；sequence 与时间在事件进入 recorder 时固定，不按落盘时间重写。`receive_operation_id`、`protocol_session_id`、`protocol_operation_id`、`transfer_job_id`、lane ID 与 epoch 仅在事件实际属于该上下文时出现，不使用含义重叠的通用 `operation_id`。标识使用完整规范编码，不截断，也不从秘密派生。

Trace 采用字段白名单。秘密类型、完整能力链接、Key、认证 token、私钥、原始内容、文件名、catalog path、本地路径、完整命令行、环境变量和未筛选的 provider 文本不得进入事件，也不得用哈希或截断值代替。Relay 只记录规范化 scheme、host 与有效 port，不记录 userinfo、path、query 或 fragment；类型上可能超过 JSON safe integer 的计数始终编码为十进制字符串。

```json
{"schema_version":1,"sequence":7,"time":"2026-08-16T13:00:01.204Z","elapsed_ms":1204,"level":"info","event":"lane_adopted","command":"get","run_id":"2b9f0e81a50d4d45a4b7f12812bb0b69","protocol_session_id":"91c71f3b2e7a4a4080fdd4d2ad558842","transport":"webrtc","lane_id":2,"lane_epoch":1}
```

Trace 文件在 relay 注册或输出 mutation 前打开；打开失败则不启动命令。运行中写入或 flush 失败时停止 trace、在 `stderr` 报告 trace incomplete，但不取消传输或改写传输退出码。启用 trace 时使用单 writer 和有界非阻塞队列；进度只保留最新快照并按固定间隔采样，不挤占生命周期事件。任何非进度事件丢失都将 trace 标记为 incomplete，丢弃计数独立保存；关闭时尽力写入 `trace_summary`，并在 `stderr` 报告不完整状态。

## 终端适配

- TTY、ANSI、颜色策略、终端宽度和单调时钟作为可注入依赖。
- ANSI 不可用时使用普通行输出；Unicode 不可靠时使用 ASCII 符号。
- 使用终端显示宽度而不是字符串字节长度计算清行和截断。
- warning、error 和 verbose 输出前清除活动进度行，输出后按需重绘。
- 用户提供的名称、路径和安全错误文案在渲染前转义控制字符。`TerminalCanvas` 写入失败只关闭人类 renderer，不成为传输失败；能力信息写入 `stdout` 失败除外。

## 实施顺序

### 1. 固定输出与事件契约

保持能力信息只走 `stdout`，固定 run 与领域标识、错误安全边界、结果映射及默认、verbose、trace 的事件可见性，并以强类型命令事件替代散落的用户可见 `k=v` 输出。

### 2. 建立真实接收进度

增加字节级进度快照和单调时间采样，保持 discovery、恢复字节、本次新增验证字节、文件终态与发布结果语义独立。

### 3. 实现渲染器

实现纯函数的人性化格式化、结果映射、默认界面、verbose 里程碑和版本化 NDJSON。速率与 ETA 使用注入时钟测试，不依赖真实等待。

### 4. 接入终端画布

让 `share`、`get` 的状态、进度和并发诊断统一经过 `TerminalCanvas`，补齐窄终端、无 ANSI、重定向和写入失败行为。

### 5. 清理与验证

移除默认终端中的内部状态机账本，更新 CLI 文档与进程夹具。迭代期间运行聚焦测试和 `make check`，交付前运行 `make ci`。
