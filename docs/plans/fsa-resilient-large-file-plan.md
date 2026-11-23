# 浏览器大文件下载可靠续传执行计划（退出语义修复 + 长断连保底落盘）

## 1. 背景与方案收敛

### 1.1 现状与真实问题

`fsa` 直写用户目录，`opfs` 在浏览器中暂存，`portable` 使用内存。OPFS 在写入时按 16 MiB 或 10 秒提交断点。配额允许时，超过 16 MiB 的单文件优先走 `opfs:file`，文件夹可选 `opfs:zip`。已有 `BeforeUnloadGuard`、`ScreenWakeLockGuard` 和可恢复中断自动暂停。

1. **页面卸载 `dispose()` 误将活跃任务结算为终态取消**
   - [main.tsx](../../web/src/main.tsx) 在非 bfcache 的 `pagehide` 上触发销毁。[coordinator.ts](../../web/src/application/receive/coordinator.ts) 调用 `activeJob.stop()` 并关闭存储，仍在收尾的 `#runSession` 可能将 FSA 或 OPFS 任务结算为 `failed (cancelled)` 或 `partial (stopped)`。
2. **长断连挂起期间不落盘**
   - 设备唤醒后若发送方离线，[supervisor.ts](../../web/src/receiver/supervisor.ts) 可能持续等待重连。此时离页，FSA 只能恢复到上一次成功提交的断点。
3. **单文件收尾租约释放过晚与流关闭异常状态隐患**
   - [file-transfer.ts](../../web/src/transfer/file-transfer.ts) 在 `writer.commit()` 结束后才释放远端 revision lease，拖长发送端并发槽位占用。
   - [fsa.ts](../../web/src/output/backends/fsa.ts) 的 `#closeWriter()` 在 `await writer.close()` 之前清空 `#writer`，一旦中途 `checkpoint()` 关闭失败且后续继续写入，会以 `keepExistingData: false` 重开流导致前缀清零损坏。

### 1.2 方案取舍（为何不采用「FSA 内部逐文件 OPFS 暂存」）

`c8e2cb6c` / `1860d6a9` 已拆开三种输出后端。保留 FSA 直写，避免正常下载占用额外的浏览器配额或在收尾时复制整份文件。长期弱网下载可选浏览器存储或原生客户端。[原始需求](../clarifications/原始需求.md)

短暂断网自动重连。持续断连时尝试保存新增进度，任务继续等待连接。FSA 重开写入流时可能复制已提交的前缀，占用额外时间和磁盘空间。用户需要离开时，可先等暂停完成。

---

## 2. 架构设计与模块边界

沿用 [依赖边界](../../web/scripts/dependency-boundaries/policies.mjs)、`ReceiveStore` schema 和八种产品状态。`transfer/` 通过输出端口操作文件。为符合 [SLOC 上限](../../.sloc-guard.toml)，将 `FSAOutputFile` 拆至 `web/src/output/backends/fsa-file.ts`，断连调度放在 `web/src/application/receive/disconnect-checkpoint-guard.ts`。

### 2.1 传输契约、退出语义与写入安全（`web/src/transfer/` & `web/src/output/backends/`）

1. **退出语义与取消解耦（[job.ts](../../web/src/transfer/job.ts)、[file-transfer.ts](../../web/src/transfer/file-transfer.ts)、[contract.ts](../../web/src/output/backends/contract.ts)、[coordinator.ts](../../web/src/application/receive/coordinator.ts)、[retained.ts](../../web/src/application/receive/retained.ts)）**
   - `TransferJob.detach()` 中止仍在运行的网络读取，文件循环通过 `FileTransferContext.isDetached()` 放弃未提交写入并返回 `'detached'`。已受理的用户暂停或停止按原意结算。
   - 将 `OutputSessionHandle.settle()` 的参数类型收窄为 `SettledTransferJobResult`（排除 `'detached'`），由类型系统静态阻止 `fsa.ts` / `opfs.ts` / `portable.ts` 收到 `'detached'`。
   - `ReceiveCoordinator.dispose()` 分离运行中的 job，等待已开始的后端打开、恢复和写盘收尾，最后关闭 IndexedDB。销毁后才返回的 session 就地关闭。
   - `#runSession` 仅对 `'detached'` 跳过 `settle()`，由同一流程在写盘结束后关闭 session、释放 Web Lock 和连接。重新打开时，[retained.ts](../../web/src/application/receive/retained.ts) 将失去锁的 `downloading` / `preparing` 任务归为 `paused (interrupted)`，从持久化 ranges 恢复。统计完成文件数时排除 ZIP 目录项。
2. **远端租约与本地落盘并行解耦（[file-transfer.ts](../../web/src/transfer/file-transfer.ts)）**
   - `streamMissingRanges` 结束即发起一次 `opened.release()`，与 `writer.commit()` 并行。`finally` 等待已发起的释放，尚未发起时补做。网络释放失败仍允许本地提交已收全的文件。
3. **Checkpoint 原因与结果契约（[output-port.ts](../../web/src/transfer/output-port.ts)、`fsa-file.ts`、[fsa.ts](../../web/src/output/backends/fsa.ts)、[opfs.ts](../../web/src/output/backends/opfs.ts)、[portable.ts](../../web/src/output/backends/portable.ts)）**
   - 定义 `CheckpointReason = 'periodic' | 'disconnect' | 'retry'` 与 `CheckpointOutcome = 'saved' | 'skipped'`，使 `TransferOutputFile.checkpoint(ranges, reason)` 显式表达触发原因与执行结果。
   - `FSAOutputFile` 从已验证的 `completedRanges` 初始化已保存字节数。仅对 `disconnect` 按增量决策。首个有数据的断点即保存，后续只有新增覆盖量达到上次已保存量才再次提交，否则保留写入流并返回 `'skipped'`。例如，首次长断连时已收 50 GiB 就保存，已有 50 GiB 断点、仅新增 20 MiB 时跳过。用户暂停、`retry` 与最终提交沿用原有规则。
   - FSA 建立断点时依次关闭写入流、读取磁盘指纹、提交 IndexedDB，全部成功才更新已保存量。失败后该 writer 仅允许 `abort()` 清理。OPFS 原位 flush 并提交 IndexedDB 后返回 `'saved'`，portable 返回 `'skipped'`。
   - FSA 仅在复用已提交范围前核对文件大小和修改时间，覆盖同会话重开流和跨会话续写。无法确认时保留原件并报告冲突。`partial` / `failed` 结算或显式丢弃时，在遗忘记录前清理归属与指纹均匹配的未完成文件。[fsa-cleanup.ts](../../web/src/output/backends/fsa-cleanup.ts)
4. **单文件写入与外部 checkpoint 互斥串行化（[file-transfer.ts](../../web/src/transfer/file-transfer.ts)、[job.ts](../../web/src/transfer/job.ts)）**
   - 每个文件串行写入、更新 ranges、保存断点和收尾，文件间继续并发。提交中的文件跟踪到 `commit()` 完成。单文件取消监听任务信号，外部断点失败时保留原始写盘错误，避免将其误判为用户停止。
   - `TransferJob.checkpointActive(signal)` 只处理有新增覆盖的可续传文件。在文件锁内读取最新 ranges，仅在 `'saved'` 后清零 `dirtyBytes`。轮次失效或任务退出时结束后续遍历，已开始的写盘安全收尾。摘要返回本轮是否已确认全部接收进度，以及 `savedFiles`、`skippedFiles`、`failedFiles`、`savedBytes`。
   - 输出端用 `FaultScope` 标明故障范围。FSA 文件局部故障只结束该文件，OPFS ZIP 共享写入对象失效由任务统一收尾。空间不足或权限失效按已有原因暂停，其他故障按失败或部分完成结算。[故障契约](../../web/src/transfer/fault.ts) · [共享写入对象](../../web/src/output/native-object/coordinator.ts)

### 2.2 长断连保底落盘守卫（`web/src/application/receive/disconnect-checkpoint-guard.ts`）

`#runSession` 仅在 `session.transferOutput.capabilities.resumable` 为真时挂载守卫，直接订阅 `joined.subscribeConnection(...)`，在会话收尾时解除订阅。[coordinator.ts](../../web/src/application/receive/coordinator.ts)

- `DISCONNECT_CHECKPOINT_DELAY_MS = 30_000`。同一轮 `reconnecting` 只启动一次定时器，订阅首帧和 `connecting ↔ waiting` 切换均沿用原倒计时。
- 到期调用 `job.checkpointActive(signal)`。`connected`、`ended`、`unavailable` 或任务关闭均使本轮失效，撤销定时器和已保存提示。再次断连独立计时。
- 仅当返回结果仍属于当前任务和断连轮次、遍历完整且全部已接收区间均已持久化时，发布 `outageCheckpointed = true`。跳过脏文件、写入失败、提交尚未结束或尚无已收数据时，继续显示等待提示。新的未提交写入使已保存提示失效。

### 2.3 展示层反馈与透明路线引导（`web/src/ui/`）

沿用现有保存路线和操作。活跃下载显示「已接收」，暂停后按 `retainedBytes` 显示可续传字节。[进度展示](../../web/src/ui/tasks/progress.ts)

- `outageCheckpointed` 随 `ReceiveRuntimeSnapshot` 经 `presentReceiveOutput`、`activeTaskFacts` 传到等待提示。仅在本轮确认落盘后显示「已保存当前进度，正在等待重新连接」。切换任务、分享或结束会话时清空。[ui/output.ts](../../web/src/ui/output.ts) · [adapters.ts](../../web/src/ui/tasks/adapters.ts) · [stage.ts](../../web/src/ui/tasks/stage.ts)
- 同步更新中英文进度、等待提示和保存须知。直写保留「请在暂停完成后再离开」的提示，长时间下载可选择浏览器存储或原生客户端。[现有文案](../../web/src/ui/locales/zh-CN/saving.ts)

---

## 3. 结构化可观测性（Observability）

复用 [output/diagnostics.ts](../../web/src/output/diagnostics.ts) 的 `OutputTraceEvent`（`checkpoint` 与 `settlement` 事件），在关键决策点输出结构化 trace：

- 断连 `checkpoint` 记录 `task_id`、断连轮次、`trigger: 'disconnect_timeout'`、本轮完成情况、保存/跳过/失败文件数和新增保存字节数。
- FSA 跳过断点时用 `checkpoint` 记录 `task_id`、`file_id`、`outcome: 'skipped'`、上次已保存量和本次新增覆盖量。
- **退出分离**（`settlement`）：记录 `task_id`、`transition: 'detached'`，便于回溯页面关闭保留续传路径。

---

## 4. 实施顺序与精简验证

1. **契约、退出语义与写入安全**
   - 拆出 `FSAOutputFile`，接入断点结果、续写校验和终态清理。实现 `detach()`、统一会话收尾及租约并行释放。
2. **长断连保底落盘守卫**
   - 接入单文件写入协调与 `checkpointActive(signal)`，在可续传会话中挂载守卫并注入时钟。
3. **UI 状态反馈与保存须知**
   - 透传本轮保存结果，更新中英文进度和保存须知。
4. **高效测试与门禁检查**
   - 用 Vitest 假时钟与内存后端覆盖以下场景。
     - 退出时仍在打开会话或写盘、用户先停止再关页、刷新后恢复断点，以及 OPFS ZIP 仅有目录的计数。
     - 写入与断点串行、租约释放失败仍能提交、FSA 局部故障和 OPFS ZIP 共享故障分别收尾。
     - FSA 首次大断点与小增量跳过用逻辑 ranges 验证。覆盖关闭失败、续写前磁盘被改动、部分完成后清理半文件。
     - 短断网、同轮重复通知、上一轮结果迟到、在途提交和 `ended`。portable 跳过守卫。
   - 用已有的 582 文件样本对照吞吐，保持新文件正常完成时额外 `getFile()` 为零。大文件长断连单独观察重开耗时和峰值空间。[小文件夹样本](../../testdata/browser-evidence/v1/fsa-small-file-workload.json)
   - 运行 `make web`（含边界检查、lint、tsc、单元测试）及 `make sloc` 验证所有改动满足门禁要求。
