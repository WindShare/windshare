# 浏览器 FSA 小文件传输性能执行计划

## 问题

目录树接收的主要耗时来自浏览器输出层。参考环境的 DirectTree 工作负载从取得目标目录 authority 到成功发布不得超过同机 pure-FSA 基线的 1.5 倍（12 秒），目录选择器等待不计时；pure-FSA 基线继续作为平台下限一并记录。

参考运行数据：

- 源数据共 6,762,858 字节、582 个文件和 105 个物化目录。
- 文件大小中位数为 3,961 字节；478 个文件不超过 16 KiB，568 个文件不超过 64 KiB，另有 31 个空文件。
- 从首次打开 revision 到最后一个 lease 释放共 159.1 秒，即每秒处理 3.66 个文件。
- P2P 在 161 毫秒内完成准入，并在首次 revision 请求后的 112 毫秒完成准入。
- block RPC 延迟为 p50 1 毫秒、p95 4 毫秒。
- 峰值同时打开 4 个 revision，证明文件 worker 确实在并发运行。
- 接收端保留的样本在 80.1 秒内记录了 199 次提交；提交间隔为 p50 395 毫秒、p95 751 毫秒。
- 582 个文件及其全部字节均已写入，但最终 FSA 结算在发布前失败。
- 50 MiB 单文件和命令行目录复制不能证明浏览器 FSA 小文件目标宽松；独立 writer commit 次数才是决定性物理约束。
- 除总时长外，还分别测量目录授权到首次内容请求、首次写入，以及最后字节到发布的尾延迟，防止以延迟起传换取总时长。

证据保存在：

- `cmd/wind/2/sender_trace/share-20260824T020005Z-364df5dd9a2d.ndjson`
- `cmd/wind/2/r.txt`

## 纯 FSA 物理下限

临时脚本 `tmp/fsa-small-file-benchmark/run.ps1` 复用了 `BrowserNativeUiReplay` 的隔离浏览器、原生目录选择器和 Chromium 权限自动化。所有 UI 操作都在计时前完成，页面只调用 FSA，不执行 WindShare 协议、网络、加密、journal、IndexedDB、checkpoint 或 settlement。

Edge 151.0.4129.101、i7-14700KF 和 NVMe NTFS 上的结果为：

- 预创建全部目录与 file handle 后，只执行 551 组 `createWritable/write/close`：并发 8/16 的中位数分别为 7.75/7.69 秒。
- 把 105 个目录与 582 个 file handle 创建也计入：并发 8/16 的中位数分别为 8.10/8.15 秒。
- 并发 32/64 没有继续提高吞吐，materialize 中位数反而增至 8.45/8.57 秒。
- 每个 case 均由主机验证为 582 个文件和 6,762,858 字节。逐文件大小使用匹配已知总量、中位数和阈值计数的确定性合成分布，因为原 trace 没有保留原始逐文件大小清单。

native close 在并发 8 左右饱和：提高并发会近似成比例地增加单次 close 延迟，而不会降低总墙钟。

这意味着架构重构可以删除 WindShare 自己增加的约 137 秒，但无法删除每个非空 DirectTree 文件一次 native commit 的物理成本。普通目录继续默认 DirectTree；ZIP 只能由用户显式选择，性能策略不得静默改变产物。

## 根因

### Root 级全局串行化使文件并发失效

`SerializedFSARootMutationAuthority` 将 root 下的所有操作放在同一个 Promise tail 后执行。`BrowserPersistentFile` 还通过该 authority 执行 `createWritable`、`write` 和 `close`，所以即使有 4 个文件 worker，实际 FSA 写入仍然只能逐个进行。

跨任务 Web Lock 与任务内部执行调度是两类不同职责，当前却由同一把全局锁表达。命名空间冲突防护需要有序执行，但对相互独立、已经取得所有权的文件句柄执行写入，并不需要 root 级串行化。

### 新建小文件承担了完整的可恢复事务成本

每个文件都会重复执行路径所有权检查、文件大小读取、checkpoint candidate 暂存、checkpoint 提升、已提交记录回读、最终 proof 读取和 checkpoint 后复验。深层文件还会反复从 root 开始遍历路径，并重新读取已持久化的目录句柄。因此，固定事务成本远高于实际载荷处理成本。

通用输出契约还把“已经写入当前 writer”与“已经形成跨重启恢复证据”绑定在一起：传输层在每个 atomic range 后调用 `checkpoint()`，事务要求 checkpoint 覆盖全部 pending ranges，并禁止带 pending ranges 的 `commit()`。因此，FSA 不能在不违反 durable range 语义的前提下自行合并 checkpoint。

### 终态结算重复执行物理检查

结算阶段会依次重新打开并验证每个已提交文件、读取每份最终 proof，然后再逐个验证目录。每个文件提交时已经建立了持久所有权证据，而终态扫描成本很高，也无法形成真正原子的文件系统快照。

### 协议控制面仍然为每个文件执行一次操作

Wire 模型已经支持批量打开 revision，但浏览器 revision reader 每次只打开一个文件。相较于输出串行化，这是次要问题；FSA 成本降低后，它会成为不可忽略的开销。

## 目标架构

### 先统一 FSA 路径坐标

先完成 `fsa-synthetic-root-settlement-fix-plan.md`：逻辑产物路径只服务布局和诊断，FSA 物化相对路径统一用于 directory admission、文件/目录物化、manifest、checkpoint 和 settlement。journal 结算不得建立在当前混用的路径坐标上；性能场景必须以成功发布为终点。

### 分离 root 所有权与 mutation 调度

跨任务 Web Lock 与任务内调度分开建模。第一阶段保留 operation-scoped 父目录 lease 作为保守的跨任务所有权边界，但它不再承担任务内文件调度；同一父目录的跨任务并行不作为本次性能修复前提。任务内将 root 级全局队列替换为具备以下能力的协调器：

- 按父目录 authority identity 隔离命名空间 lane；创建、删除和重命名串行执行，并与同一父目录下从 `createWritable()` 到 `close()`/`abort()` 的 writer 生命周期互斥。
- 有并发上限的文件内容 lane 允许已拥有文件并发写入和关闭。等待中的命名空间 mutation 停止接收该父目录的新 writer，drain 现有 writer 后创建当前已发现的 sibling 批次；批次可以只有一个文件，不等待完整目录发现或为凑批延迟起传。
- 继续通过现有 `PersistentFileTransaction` tail 保证单个文件内的操作顺序。
- 明确 `accepting -> draining -> closed` 生命周期。递归清理、compatible-name 终态修复和结算先停止接收新操作，drain 全部命名空间与文件 lane，再独占执行；它们不得作为普通 key 任务与子文件写入并发。

父级 key 必须来自操作拥有的目录身份，不能来自不可信的显示名称。在获得稳定身份前，result-root reservation 仍由父目录锁保护。

### 让 checkpoint 成本随实际恢复价值增长

先重构通用 `OutputFileTransaction` 契约，显式区分：

- pending ranges：`writeRange()` 已被当前事务接受，但不承诺跨重启恢复；
- durable ranges：`checkpoint()` 关闭并验证当前写入边界后返回的可信恢复范围；
- final proof：`commit()` 吸收全部 pending ranges，完成最后一次关闭、验证和原子 journal 提交后产生的完整文件证据。

`commit()` 不得要求调用方先执行一次重复 checkpoint。`pause()` 必须强制形成 durable cut 后才能报告暂停；异常退出后的恢复只信任 durable ranges，未提交的小文件复用 exact owned handle 并从零重写。pending ranges 不得作为 durable ranges 返回。

能力保证与执行策略保持分离：`OutputCapabilities` 只描述 durability、random write 等保证；独立的 output execution profile 报告有界文件并发、checkpoint trigger 和 checkpoint cost budget。累计 pending 字节、持续时间或显式暂停只产生 trigger；策略还必须根据既有 prefix 复制量、累计写放大和峰值临时空间决定是否允许自动 durable cut。不得按 backend 名称分支，也不增加 `isSmallFile`、`skipCheckpoint` 一类补丁标志。

不预判文件是否会中断。所有新文件使用同一套事务语义：

1. 渐进发现得到的 lineage lookup 和 claim install 使用有界批次；创建后将 owned handle 持久化与 candidate promotion 合并为一次原子 IndexedDB transition。事务成功后不再逐记录回读，批处理不得等待完整目录发现或延迟首个文件起传。
2. 通过已拥有的 handle 写入，不再为每个 range 重新遍历命名空间。
3. 自动 checkpoint 只有在 trigger 与 cost budget 同时允许时才执行；普通小文件在边界触发前完成。大文件超过 prefix-copy 或空间预算后保持长生命周期 writer，正常传输继续，但 durable 位置不再自动前移。
4. 显式暂停必须关闭 writer 并形成 durable cut。正常未中断传输不得为 checkpoint 复制完整 prefix 或创建第二份完整产物。继续一个大 prefix 可能需要显著额外空间时，必须在 mutation 前说明并让用户选择释放空间、保留现状、明确重下该文件或改用 native；不得静默删除已有进度。只有用户明确选择重下且 exact owned handle 仍成立时，才能删除 WindShare 拥有的未完成文件。
5. 完成时只执行一次终态 writer close，读取一次文件大小，并验证路径和 owned handle。
6. 最后一次物理验证通过后，在一个 IndexedDB transaction 中原子提交 final checkpoint、final proof 和 materialization ledger entry，不再执行提交后物理复验。外部程序在验证与提交之间并发修改属于已声明的非原子残余风险。

最终事务提交前中断的在途小文件，在恢复时验证 exact owned handle 并从零重写；已完成文件不重下。存在已验证中间 checkpoint 且恢复成本仍可接受的大文件继续按 ranges 续传。空文件走相同的 claim 和最终提交边界。

### 复用活跃 authority

在 root lease 生命周期内缓存已验证的 operation/root binding、父目录 authority 和 exact owned file handle。文件验证复用已认证 authority，不再从 IndexedDB 回放每一级祖先；出现命名空间 mutation 歧义、identity 不匹配、authority 丢失或会话结算时统一失效。

### 增量封存证据，而不是终态重扫

物化期间维护有界、分页、追加式的持久 materialization ledger。文件 final transaction 原子写入不可变 proof 和唯一路径 entry；目录 admission/finalization 也写入同一 operation ledger。文件完成不得逐次重写同一个 operation 级计数器或聚合根；worker drain 后从持久页摘要封存计数和聚合根。不得在内存中保留无界完整 manifest，也不得在终态重新打开文件或逐条调用独立 proof 存储。

终态结算应执行：

1. Drain 所有 mutation lane。
2. 验证一次 operation/root binding，并确认没有活跃 mutation 或 candidate checkpoint。
3. 确认 authenticated discovery 和 directory settlement 已完成，worker summary 与 ledger 的文件数、目录数、字节数和聚合根一致。
4. 封存 ledger root，提交终态 receipt，并清理可恢复状态。

发布证明 WindShare 已经把认证内容写入并在各文件提交边界验证所有权，不承诺对外部程序形成原子文件系统快照。物理复验保留在 writer 打开、文件提交、恢复、重新打开和所有权修复边界；发布前不再逐个重开全部文件和目录。

### 以 native 饱和点约束输出并行度

Output execution profile 同时限制活跃文件数、在途字节和内存。Windows Chromium 参考环境首版文件并发上限为 8；其他环境使用保守的静态 profile，测量后再调整，不引入自适应算法。若最终 journal 提交成为实质瓶颈，可引入按文件数、字节和时间有界的批量终态提交，批内文件在整批 durable 前不得显示完成，异常退出只重写最后一个未提交批次。只有协议控制操作仍是实质瓶颈时，才引入 browser batch revision reader，且首个 revision 立即打开，不为凑批延迟起传。

## 产品取舍

- 正常小文件接收删除无价值的串行等待和重复持久化，属于高频路径升级。
- 中断只会让受文件数和字节预算约束的在途小文件从零重写；已完成文件和大文件的已验证 ranges 保留。
- 正常大文件传输优先保持一个 writer 和有界空间；不以无界 prefix copy 换取更密集的自动恢复点。
- 主进度表示已接收/写入字节；可恢复进度只表示 durable ranges。最终 proof 完成前不得显示文件完成，pause 的 durable cut 完成前不得显示已暂停。
- 取消立即停止新网络和输出准入并显示“正在停止”；超时后界面可以退出等待，但后台资源所有者继续持有 root lease、repository 和 output authority，直到已有 native 操作 drain 完成。durable cut 完成前不得显示已暂停，相同目录不得开始新的 mutation。
- DirectTree 始终产生真实目录，ZIP 只来自用户显式选择；性能策略不得动态推荐或静默改变产物。
- 外部程序并发修改目标目录不属于 WindShare 可提供的快照保证；不让所有正常接收为一次非原子全量复扫付费。

## 执行顺序

1. 记录性能口径：参考环境 DirectTree 从取得目标目录 authority 到成功发布不得超过同机 pure-FSA 基线的 1.5 倍（12 秒），普通目录继续默认 DirectTree。
2. 完成 FSA 路径坐标修复，使成功发布使用唯一的物化相对路径语义。
3. 重构 mutation authority，分离跨任务父目录 lease、按父级隔离的命名空间调度、有界文件内容调度和全局 drain；同一父目录的 namespace mutation 与完整 writer 生命周期互斥，等待中的 mutation 停止接收新 writer，避免 Chromium `InvalidStateError`。同时缓存 lease 生命周期内的 operation/root binding、父目录和 owned file authority，再测量一次参考工作负载。
4. 重构通用输出事务的 pending/durable/final 契约，引入有界 lineage/claim 批处理、合并的 owned-handle transition、execution profile、checkpoint trigger 与 cost budget。覆盖 claim、writer close、final transaction、暂停和恢复处的中断，再测量一次参考工作负载。
5. 将 checkpoint/proof 与追加式分页 materialization ledger 原子提交，以页摘要和 ledger root 封存替代内存 manifest、逐 proof 回读和终态物理重扫；只有测量证明需要时才加入有界批量终态提交。
6. 用 582 文件参考工作负载验证成功发布并量化距 pure-FSA floor 的剩余开销，同时验证渐进发现与传输重叠、超大文件空间预算、暂停/取消和崩溃恢复。仅在证据仍指向控制面时批量打开 revision；不实现自适应并发算法。

## 可观测性

记录稳定的 receive operation、transfer job、output session 和 owned object identity，并包含：

- 命名空间、文件内容、checkpoint 和结算阶段的队列等待与执行耗时。
- 活跃及峰值命名空间、文件操作数量。
- 路径 authority 缓存的命中、未命中、失效次数和遍历深度。
- 仅终态提交与可恢复 checkpoint 的数量，以及 ledger page、entry 和 seal 汇总。
- pending/durable 字节量、checkpoint trigger、cost-budget 决策、prefix-copy/峰值空间估算和异常退出时的最大重写范围。
- Revision open 操作次数；启用批量时记录 batch 大小。
- 物化开始、首次内容请求、最后一次文件提交、结算开始和发布的时间戳。
- 目标目录授权、首次写入和最后字节写入时间，用于分离起传延迟与终态封存延迟。

成功的原生调用优先记录聚合汇总，避免为每次调用生成一条 trace。失败记录继续保留精确路径、阶段、原生异常和 writer 状态。

## 非目标

- 仅靠增加 block 大小或 P2P lane 数量绕过输出瓶颈。
- 在写入仍由 root 全局串行时仅提高文件 worker 并发度。
- 删除大文件或中断传输的可恢复能力。
- 弱化 claim-before-create 或 owned handle 身份保证。
- 在通用传输层按 FSA backend 名称分支，或把 pending ranges 伪装成 durable ranges。
- 为 checkpoint 允许无界 prefix copy、临时空间或累计写放大。
- 在内存中聚合无界 manifest，或在终态逐文件重读 proof。
- 静默把 DirectTree 改成 ZIP，或引入第一版自适应并发算法。
- 为本次单任务性能目标提前开放同一父目录的跨任务并行。
- 未经用户明确选择删除大文件恢复进度，或让正常未中断传输承担完整 prefix copy。
- 在每个文件提交时重写同一个 operation 级 ledger 聚合记录。
- 为外部程序并发修改目标目录提供不存在的原子快照保证。
