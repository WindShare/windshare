# 浏览器 FSA 可续传 ZIP 执行计划

## 背景与产品决策

ZIP 用于把目录聚合成一个文件并避开浏览器目录项限制，不追求压缩，继续使用 `store` 编码；界面明确写“只打包，不压缩”。

- 普通目录接收仍默认 DirectTree。用户显式选择 ZIP 后只展示一个主操作和一个次操作，operation 开始后冻结选择。
- 使用独立的 `ZipRouteRecommendationPolicyV1` 排序，不复用通用 `SelectionSizeClass`：完整发现且 exact workspace budget 落在具名推荐预算内时推荐“完成后另存为”，大小未知或超过该预算时推荐“保存到文件夹”。推荐只影响展示，不授予容量或 route authority，也不得为排序触发完整目录扫描。
- “保存到文件夹”从创建起可见目标文件，完成前不是可用 ZIP；界面显示“正在生成，完成前不可使用”。正常路径优先速度和空间，只承诺从最近安全 checkpoint 继续，不承诺固定最大回退或断电持久性。
- `workspace-then-publish` 继续提供“完成后才出现 ZIP”和更强恢复能力，但需要浏览器临时空间。
- 两条路径只用用户保证描述，不暴露 FSA/OPFS 等 backend 名称，也不得静默互换。
- DirectTree 切换 ZIP 时创建新 operation；开始前明确提示已接收内容不会复用，需要重新接收。

## 1. 建立独立的直接 ZIP 语义

新增 `direct-resumable-zip` materialization plan 和 FSA owned-file target profile，不复用 `direct-atomic`。

`zip-archive` 仍是 `complete-only` 语义产物；目标文件在接收期间只是 operation-owned incomplete state。只有 central directory、精确长度、bounded closing-tail 和 layout 验证完成后，才能进入 `completed + verified + published`。

ReceiveIntent 只冻结 SelectionSpec、ZIP encoding/checkpoint policy、目标 binding reference、稳定结果名和 operation ID。父目录与文件 handle 作为 operation-owned records 单独保存，不进入 canonical intent，也不持久化绝对路径。

只有通过真实本地 FSA restart probe 的平台才报告 `ProcessRestart`；它表示重新授权后可以继续，不表示权限必然跨重启保持。其他环境不得提供该恢复保证。UI 主进度展示已接收字节，次要文案展示可继续位置，后者才是刷新后的恢复边界。

## 2. 取得并证明 FSA 目标

优先复用重新授权和验证通过的父目录，否则通过目录选择器取得 authority。直接路径使用经现有 compatible-name policy 约束的 `<结果根>.windshare-<operation-token>.zip`，绑定 operation 后立即在 UI 显示精确名称；需要干净文件名的用户选择“完成后另存为”。直接路径不使用可能覆盖已有文件的 save-file target。

FSA handle 和 `isSameEntry` 只作为 locator/permission evidence，不能证明同路径下仍是原物理文件。直接 ZIP 因此增加绑定 operation 的随机 ownership marker，并把 version、magic 和 marker 编码进首个 result-root ZIP header 的私有 extra field；它不产生用户可见 archive member，且计入版本化 layout policy。

创建采用以下 cut：

1. 原子保存 reservation candidate、稳定名称和 marker binding；
2. 用精确名称查询目标；不存在才创建，已存在时只有 marker 匹配当前 candidate 才能认领，否则 retire candidate 并换 token，不得打开 writer；
3. 写入并关闭带 marker 的首个 ZIP prefix，再读回验证；
4. 原子保存父目录/file handle、初始 checkpoint 和 lifecycle，并 retire candidate。

崩溃落在第 3 步时，可以按 candidate 名称查找 entry，但只有 marker 完全匹配才能认领；名称或 handle 单独匹配都不构成 ownership proof。

恢复、重新打开 writer、截断或删除前，重新请求权限，并验证 parent binding、稳定名称、marker、已提交长度和最近 target observation。证据变化时校验 committed epoch digests；不一致进入 `target-verification-required`，无法判断 ownership 才进入 `needs-attention`，不得自动覆盖或删除。

FSA 没有跨本地进程的条件创建、条件替换和条件删除。计划保证 WindShare 标签页之间不并发写，并检测普通的非并发替换；提交前验证与 `close()` 之间的本机并发替换属于文档化残余风险。UI 明示完成前不要移动或修改目标文件。

停止时提供“保留以便继续”和“删除 WindShare 创建的残缺 ZIP”。自动删除是低频破坏性操作，必须实际读回并通过 marker 与 committed epoch digests；否则只提示用户手动处理。该验证不进入正常传输快路径。

## 3. 让 ZIP writer 可恢复且避免写放大

新增 operation-scoped `DirectZipJournal`，把 checkpoint、candidate、持久 layout pages、central-directory records 和 lifecycle 放入同一 IndexedDB 数据库。它替代当前不可 reopen 的内存 ledger/临时 spool，并通过窄 repository interface 隔离 IndexedDB。

`DirectZipJournalBudgetV1` 限制最多 1,000,000 个成员和 256 MiB canonical metadata，并限制单页与单事务大小。每次 page admission 都先 checked accounting；超限保留最近 checkpoint 并提示使用 native，不允许无界内存聚合或在完成时一次加载全部 central records。

版本化 `DirectZipCheckpoint` 绑定 operation、intent、target、encoding policy 和 journal root，记录：

- entry ordinal、FileID、FileRevision、精确大小和 source range authority；
- archive offset、member payload offset 和可恢复 CRC32 状态；
- committed length、last-modified observation、writer-computed SHA-256 epoch digest root、layout-page root、central-record root 和 checkpoint generation；
- `between-members`、`inside-member` 或 `closing` 阶段。

持久 layout page 同时保存恢复所需的 source coordinate、directory admission 和 authenticated discovery evidence；刷新后可以验证重放渐进 discovery，不依赖旧内存对象，也不增加完整目录预扫描。

FSA `createWritable({ keepExistingData: true })` 会复制既有 prefix，因此 `DirectZipEpochPolicyV1` 以正常传输速度和空间为先：保持一个长生命周期 writer，不按 block、member、固定时间或固定字节关闭。自动 checkpoint 只有在下一 epoch 的 committed-prefix 峰值和累计 prefix-copy 仍处于具名预算内时才允许；超预算后传输继续，但安全位置不再前移。用户主动暂停时关闭当前 epoch 并显示“正在保存可继续位置”；再次继续前按 committed length 明示最多可能需要的额外临时空间。策略不承诺固定最大回退。

一次 checkpoint cut 为：

1. 在同一 writable 中暂存一个或多个 member，并在写入时计算 expected epoch digest；
2. quiesce writer，原子保存包含 staged end、source/layout evidence 和 expected epoch digest 的 candidate；
3. `close()` 前复核 committed predecessor 的 bounded marker、精确长度、target observation 和 journal lineage；证据未变化走快路径，证据变化才读回相关 committed epoch digests，不一致则 abort writer；
4. `close()` 发布临时文件，再复核 bounded marker、精确长度、target observation，以及 closing 时的 bounded tail/layout；正常快路径不读回整个 epoch；
5. 原子提交 checkpoint、layout/central pages、expected digest 与 lifecycle，并 retire candidate。

恢复遇到 candidate 时：目标匹配 committed predecessor 就丢弃 candidate 并重放；目标匹配 candidate observation 时进入慢路径，校验 candidate range digest 后才能 promote；只有 marker 与 predecessor digests 仍成立时才可截断未知尾部。任何其他结果进入 `target-verification-required`，不得仅凭长度接受数据。全 prefix 复核只用于 observation 变化或 candidate 歧义。

`inside-member` checkpoint 从已验证 payload offset、source revision 和 CRC32 状态继续；证据不足时只回退该 member。closing 单独 checkpoint，允许 central directory 写入失败后安全重写。正常完成使用 writer-computed digest root、成功 close、bounded marker/tail、精确长度、layout digest 和 journal roots 验证；只有 target evidence 变化或 candidate 歧义才读取相关 epoch 或全 prefix。

## 4. 路由、空间与界面

两条 ZIP 路径使用稳定 choice ID，但同时只展示一个主操作和一个次操作：

- `保存到文件夹`：边接收边生成，适合大文件；完成前文件不可用；意外关闭后从界面显示的安全位置继续，其后的内容需要重新接收；继续时可能需要最多相当于 committed prefix 的额外临时空间。
- `完成后另存为`：需要浏览器临时空间；目标目录只出现完整 ZIP；恢复能力更强并允许用户选择干净文件名。

主次排序只在 operation 创建前由 `ZipRouteRecommendationPolicyV1` 根据 discovery、checked workspace budget 和 route availability 确定；不得在用户点击后变化。

现有 bounded portable 仅在其 hard limit 内提供。大任务在 FSA 与 workspace 都不可用时优先提示 native。

FSA 主进度显示“已接收”，次要文案显示“意外关闭后可从 `<安全字节>` 继续”；OPFS 保持 complete-only 文案。选中原始字节和 ZIP 字节在 discovery 完成前标为估算值，不伪装成精确值。进度不得在 closing 完成前显示 100%；慢路径验证单独显示“正在确认已保存内容”。

正常未重开的 FSA fast path 不得产生第二份完整 artifact；checkpoint 或继续可能复制既有 prefix，必须受 epoch budget 约束并在继续前显示额外空间上界。OPFS budget 继续计算 raw materialization、package、spool 和 metadata；最终 handoff 的目标磁盘空间不伪装成已预留。

路径不兼容流程仍以“跳过不兼容项并继续 DirectTree”为首选；ZIP 与 native 是显式 fallback。

## 5. 生命周期、并发与诊断

新增非终态 `authorization-required`、`target-verification-required` 和 `destination-space-required`。权限拒绝可重新授权；空间不足先解析 candidate 并保留最近 verified checkpoint，用户释放空间后重试；只有 ownership 无法判断才进入终态 `needs-attention`。目标文件被用户删除则进入 `restart-required`，不得自动重建并假装续传。

所有创建、checkpoint、恢复和删除都持有 operation lease 与父目录 Web Lock，禁止同源多标签页并发修改。刷新或连接中断后只恢复同一 frozen operation；用户显式停止可保留最近 checkpoint 或执行 ownership-safe 删除。

诊断记录 plan kind、checkpoint phase、epoch offset class、prefix-copy/peak-space budget decision、permission/identity/space/cleanup decision 和稳定 operation/session ID。本地诊断保留原始异常与 FSA stage facts，导出时生成有界 projection；不得记录内容数据。

OPFS `waiting-to-save`、重新下载和 complete-only 清理语义保持不变。

## 6. 实施与验证顺序

1. 先做真实本地 FSA spike，不以 OPFS 或 mock 代替；真实 fixture 总量不超过约 10 GiB，执行前检查并保留具名磁盘安全余量，结束后只清理 ownership-proven 测试文件。覆盖首次写入、`keepExistingData` 重开、浏览器终止、重新授权和外部替换；磁盘满只用受控配额或 fault injection 模拟，不得主动耗尽本地磁盘。测量 prefix copy、峰值空间、close 与读回成本，据此冻结支持矩阵、workspace 推荐预算和 `DirectZipEpochPolicyV1` 具名常量。
2. 同步重构 Go/TypeScript canonical choice identity、plan/target guarantee、lifecycle byte 和两套用户文案；直接升级版本，不双读旧合同。
3. 实现 FSA reservation candidate、ZIP ownership marker、handle persistence、operation lease 与安全清理。
4. 实现带 budget admission 的 `DirectZipJournal`，持久化 layout、discovery、central records、checkpoint 与 candidate transitions。
5. 重构 ZIP writer 为 epoch-based resumable state machine，接入暂停、空间不足、恢复、快慢验证路径和 closing 重写。
6. 用小型 fixture 和 fault injection 覆盖 marker 创建 cut、占用名称、close 前外部修改、close/IDB cut、空间不足、journal 超限、同名同大小替换、member 内恢复、多标签并发和清理失败；验证写放大/峰值空间预算、正常完成不全量读回，以及 ownership extra field 可被 Go `archive/zip` 和受支持平台归档工具读取。
7. 验证 DirectTree 与 OPFS 行为未改变，更新产品说明和 canonical vectors，运行 focused Web/Go gates，最后运行 `make ci`。
