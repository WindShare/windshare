# 浏览器接收端输出体验重构计划

> 状态：2026-08-07 提议。本专项体验计划承接
> [`即时分享与文件浏览重构收尾计划.md`](.archive/2026.08.06/即时分享与文件浏览重构收尾计划.md)。在实现与维护文档同步切换前，
> [`协议规范.md`](协议规范.md)、[`威胁模型.md`](威胁模型.md) 和现行 `OutputSession` 持久性合同仍是当前权威。

## 原始需求(不准修改这里)

用户分享的东西，怎么下载变成zip了？

## 前因与问题

Web 接收端目前把 `Folder tree` 和 `Browser download` 作为并列选项。`Folder tree` 获取用户选择的
`FileSystemDirectoryHandle`，并在该根目录下写入经过认证的规范路径。只选择部分内容时，现有能力已经会生成一棵裁剪后的目录树：保留所选文件、必要的祖先目录和显式选择的空目录，不请求也不创建无关的兄弟项。

`Browser download` 的实际行为没有这么直观。只有分享根层恰好包含一个文件时，系统才将其识别为直接单文件下载；
其他选择都会变成直接 ZIP 流或经 OPFS 暂存后导出的 ZIP，并统一命名为 `windshare.zip`。ZIP 只是接收端使用仅存储（store）模式进行聚合，不会在发送端预上传、预哈希或压缩内容，也不会改变加密内容协议。

现有底层能力合理，但产品模型隐藏了最终结果：

- `V2OutputIntent = 'directory' | 'download'` 混合了用户可见的输出产物与浏览器获取能力的方式。
- `knownSingleFile` 检查分享根层而非冻结后的实际选择，因此从多文件分享中只选一个文件仍会静默生成 ZIP。
- `acquireOutputCapability` 在用户选择笼统的下载方式后才决定 `single-file` 或 `zip`。
- 受限便携下载只有固定容量，但渐进选择在完整发现前无法证明最终 ZIP 可发布。
- OPFS 是可选暂存层，仍依赖最终发布目标；把它与发布目标并列会隐藏容量和恢复边界。
- UI 文案暴露后端术语，却无法准确说明产物命名、恢复能力、部分文件可见性或 ZIP 封口边界。

因此根因修复必须引入显式的输出产物模型，而不是只在现有单选项旁补一句 ZIP 提示。

本计划裁决：ZIP 可以作为用户明确选择或浏览器唯一安全可用的聚合产物，但不得成为静默默认降级；不采用逐文件触发多次浏览器下载。ZIP 仅打包、不压缩，界面必须说明其用途和封口前不可用的边界。

## 产品行为

| 选择证据与浏览器能力 | 主操作或状态 | 行为 |
|---|---|---|
| 产物形状尚未证明 | `正在确认所选内容…` | 非交互状态；后台继续 discovery，形状稳定前不打开 picker 或静默选择 ZIP |
| discovery 暂时失败且形状尚未证明 | `重试确认` | 保留已认证证据；重试只恢复 discovery，完成后仍由用户点击明确的产物操作 |
| 已证明没有选择任何产物 | 禁用接收 | 显式选择的空目录属于目录树，不属于空选择 |
| 已证明恰好一个文件，且完整产物目标可用 | `下载 <文件名>` | 原子目标完成后报告已保存；浏览器接管只报告下载已开始 |
| 已证明恰好一个文件，但只有目录目标 | `保存到文件夹` | 使用单文件目录产物，不额外创建结果根；明示部分可见性与续传能力 |
| 包含目录、空目录或多个文件，且目录目标可用 | `按原文件夹层级保存` | 默认使用目录树；仅在完整产物工作区与发布目标可用时提供次操作“打包为一个 ZIP（不压缩）” |
| 包含目录、空目录或多个文件，且只有完整产物目标 | `下载 <归档名>.zip` | 明示能否保留进度；不得静默改变产物或恢复语义 |
| 只有受限便携下载 | `检查后下载` | 先完成所选内容发现和容量 admission，成功后才请求内容，完成时显示“下载已开始” |
| 没有安全目标 | 禁用接收 | 说明容量、权限或浏览器能力限制 |

用户可见行为遵循以下规则：

- 操作只描述最终产物，不暴露 OPFS、stream、intent 或 admission。
- 产物形状确认在后台进行；`正在确认所选内容…` 不可点击。每次选择变化生成新的 projection epoch，旧 epoch 的异步结果不得更新当前操作。只有按钮已经显示最终产物后，用户点击才在同一调用栈启动 picker，不在异步 discovery 完成后自动弹窗；一旦证据已证明 `Tree`，不得为取得无关完整度继续阻塞操作。
- 多文件、目录或空目录在目录目标可用时默认按原层级保存。ZIP 只由明确标注的操作选择；容量不足或能力变化不得自动把 ZIP 改成目录树，或反向改变产物。
- 目录选择器可在接收前取得父目录。`showSaveFilePicker` 会预创建或清空目标，不能证明 no-replace 或回滚，因此不属于本计划的安全完整产物目标。浏览器接管不预选位置；暂存产物封存后再取得发布目标。
- 单个完整目录使用 `<目录名>` 作为结果根、`<目录名>.zip` 作为归档名；ZIP 内保留该结果根。该目录内的部分选择使用 `<目录名>-selection`。跨目录或多根选择使用 `DEFAULT_RESULT_ROOT_NAME` 和 `DEFAULT_ARCHIVE_NAME`，并保留相对 synthetic root 的规范路径。
- 多项目录树保存只让用户选择父目录。WindShare 在其中创建任务绑定的结果根：同一 `ReceiveIntent` 验证 ownership 后重新打开，已观察到的同名项使用持久化短后缀创建新根，归属或并发变化不明时停止并提示，不合并或主动替换既有项。
- 单文件保存到目录使用 `DirectoryTree` 的单文件布局并直接使用文件名，不额外创建结果根。文件只在 revision 打开后创建，可以在完成前可见；持久目标用 checkpoint 续传，已观察到重名时使用持久化短后缀，并发身份不明时停止。
- 建议名称经过长度、保留名、Unicode 和后缀规范化。目录产物使用规范名称或持久化短后缀；浏览器接管只取得建议名称，不保证最终名称。
- 目录产物（包括单文件布局）允许部分可见并保留成功内容。`OriginalFile` 和 ZIP 是完整产物，只能使用 `AtomicNoReplace` 原子目标或 `BrowserHandoff`；任何所选文件或 discovery 失败都不得发布已知残缺产物。
- 系统在不改变已展示产物与恢复语义的前提下选择直接或暂存策略。直接 ZIP 显示“接收并生成 ZIP”；暂存 ZIP 明确区分“接收”“生成 ZIP”“保存”，只有最终 ZIP 在工作区封存后才进入等待保存。发布失败时可换位置，不重新接收或重新打包已封存产物。
- 输出选择器在接收 intent 冻结前取消时，只丢弃 draft、关闭本次连接尝试并返回产物选择。暂存已经完整、但发布选择器取消时进入“等待保存”，不伪装成暂停或失败。
- intent 冻结后，“暂停接收”只在持久状态已复核时进入 `Resumable`；“停止接收”对目录产物保留已成功文件，只清理已确认 ownership 的未完成项并进入 `PartialDirectory(stopped)`；“放弃任务并清理未完成内容”只删除已确认 ownership 的未发布项、workspace、package、checkpoint 与元数据。目录已有成功文件时仍为 `PartialDirectory`，否则清理完成后为 `Discarded`。完整产物不得因停止或放弃发布残缺文件；归属不明时进入 `NeedsAttention`。浏览器 handoff 已开始后不能撤回。
- “可保留进度”只描述输出侧能力；继续接收仍要求发送端在线、revision 未变化且目标权限可重新取得。
- `DirectTree` 在 revision 打开前不得创建同名文件；完整产物目标不得以空文件占位。真实零字节文件仍通过正常 file transaction 成功发布。
- 24 小时保留期只在任务稳定进入 `Resumable` 或 `WaitingToSave` 时开始。接收或发布期间没有到期时间；显式继续后若再次稳定暂停，生成新的到期时间，lease 心跳不续期。界面显示到期时间、占用空间以及与状态一致的继续或删除操作。受控目标确认 `Published` 后立即清理；workspace-backed `DownloadStarted` 保留已封存产物至原到期时间，允许再次下载或显式删除且不重置期限；portable handoff 仅按有界 URL lease 保留内存对象，不宣称恢复。到期后禁止继续并在 WindShare 下次前台运行时清理；浏览器提前回收存储时不得承诺仍可恢复。

## 目标架构

选择规则、观察证据、产物、接收和发布使用独立模型：

```text
SelectionSpec         = 冻结的用户选择规则
SelectionProjection   = projection epoch + 单调的有界选择拓扑、度量、未结算目标与 ArtifactShapeProof
DiscoveryState        = Idle | Discovering | RetryableFailure | Complete
ArtifactShapeProof    = Unknown | None | SingleFile(file) | Tree(result root, selected roots)
ArtifactSpec          = OriginalFile | DirectoryTree(layout) | ZipArchive(layout, Store, CompleteOnly)
NameAuthority         = ApplicationChosen | UserChosen | BrowserChosen
ReplacementGuarantee = AtomicNoReplace | CoordinatedNoReplace | UserAuthorizedReplace | Unknown
DeliveryMode          = ManagedTarget | BrowserHandoff
CommitVisibility      = AtomicCommit | PrefixVisible | Unobservable
RollbackGuarantee     = ToAbsent | None
EnvironmentOffers     = 目标类型 + 硬限制 + 名称/替换/交付/提交/回滚保证 + 持久性事实
AcquiredDestinationAuthority = ContainerAuthority(parent/root) | AtomicTargetAuthority(instance)
DestinationReservation = stable operation identity + destination authority + 规范名称 + 冲突裁决
MaterializationPlan   = DirectTree | DirectAtomic | WorkspaceThenPublish | PortableHandoff
ReceiveIntent         = SelectionSpec + ArtifactSpec + DestinationReservation/workspace binding
PreparationManifest   = 需要完整预检的计划所用选择项、路径、大小与 catalog generations
MaterializedManifest  = DirectoryEntry | FileEntry(FileID, FileRevision, owned object, checkpoint generation)
SealedMaterialization = WorkspaceThenPublish intent + manifest digests + raw workspace receipt
PackagedArtifact      = SealedMaterialization + artifact receipt + exact byte length + layout digest
PublicationAttempt    = PackagedArtifact + (ManagedPublicationTarget | BrowserHandoffAttempt) + attempt identity
WorkspaceBudget       = unique owned raw/package/temp bytes + durable metadata
```

`SelectionProjection` 由同一 epoch 的冻结规则和已认证 catalog 证据同步推导并保持单调，只保留生成产物所需的有界根拓扑、未结算目标、文件/目录下界与字节度量，不为分类物化整棵树。epoch 只用于 controller fencing，不进入 `ReceiveIntent`。选择变化立即创建新 epoch；旧 discovery 的进度、失败和完成事件全部丢弃。`DiscoveryState` 单独描述当前发现尝试；失败和重试不得丢弃当前 epoch 的已有投影。`Unknown` 表示证据仍不足，`None` 只表示完整证据证明没有选择任何产物；显式空目录必须证明为 `Tree`。只有规则和未结算目标都不可能再加入其他产物时，才能证明 `SingleFile`。

规划分两步：picker 前，纯 `offerArtifacts` 根据 `SelectionProjection`、`DiscoveryState` 和 `EnvironmentOffers` 生成用户可见操作；`Unknown` 或待重试状态只更新非交互状态，异步完成后重新展示操作而不自动打开 picker。picker 后，纯 `bindMaterialization` 用 container、目标或工作区建立持久 `DestinationReservation` 并生成不可变计划。容量估计不能充当 admission 保证；自动策略只能保持已展示的产物与恢复语义，否则必须返回产物选择。

合法计划使用判别联合而不是任意组合：`DirectAtomic` 要求 `ManagedTarget + AtomicNoReplace + AtomicCommit + ToAbsent`；`WorkspaceThenPublish` 使用同样的受控发布端，或 `BrowserHandoff + BrowserChosen + Unobservable + Unknown`；`DirectTree` 要求 `ManagedTarget + (AtomicNoReplace | CoordinatedNoReplace) + PrefixVisible`；受限 Blob 只使用 `BrowserHandoff`。`UserAuthorizedReplace` 不能构造本计划中的合法计划，`PrefixVisible` 只能承载目录产物。保留的选择器调用仍直接发生在用户点击调用栈中，异步目标校验只能发生在选择器已经启动之后。

FSA 没有 exclusive-create 原语，只能报告 `CoordinatedNoReplace`：root Web Lock 串行化 WindShare 页面，创建前检查不存在，持久化实际 handle，并在取得 writer 前及提交后复核 parent/object identity。已存在项使用持久化后缀；观察到外部并发或 ownership 证据不确定时进入 `NeedsAttention`。该保证不排除非协作进程在检查与创建之间的竞争，不得用于完整产物或冒充宿主原生 `AtomicNoReplace`。

直接文件、直接 ZIP 和目录根的 `ReceiveIntent` 绑定 `DestinationReservation`，不把尚未创建的文件冒充 acquired object。单文件目录布局在 revision 打开前只持久化 parent identity、规范名称、冲突裁决和 stable operation identity；创建后才把 owned-file identity 写入 checkpoint 与 `MaterializedManifest`。同一 reservation 用于恢复，新任务使用新 operation identity。暂存原文件或 ZIP 只绑定唯一工作区和产物语义；封存后取得的发布目标才绑定为 `PublicationAttempt`，因此换目标不使接收 checkpoint 失效。现有单目标 `TransferIntent.output` 由新的 `ReceiveIntent`、digest、directory admission scope 和 Go↔TypeScript 向量直接替换，不双读旧合同，也不增加发送端线协议字段。

`PreparationManifest` 只属于受限 `PortableHandoff` 和显式选择的 `WorkspaceThenPublish ZipArchive`，用于内容请求前的完整布局与峰值空间预检；目录树、原文件和安全直接目标不得因此恢复整树屏障。它不能证明实际接收的 revision。工作区封存时在同一 repository lease 下从目录记录和完整 checkpoint 生成排序的 `MaterializedManifest`：目录项绑定规范路径、generation 与 owned-directory identity，文件项另绑定 `FileID`、`FileRevision`、精确大小、owned-file identity 和最终 checkpoint generation。

`SealedMaterialization` 仅属于 `WorkspaceThenPublish`，是现有 checkpoint repository 的持久聚合记录，绑定 intent digest、全部使用的 generations、manifest digests、布局版本、工作区 ownership 和原始内容封存状态。ZIP packaging 从该只读封存生成新的 owned OPFS 对象；原始内容保留到 ZIP 的 close、实际长度、layout digest 和对象身份全部复核成功。只有此时形成不可变 `PackagedArtifact` 并进入等待保存；失败可清理已确认 ownership 的 package temp 后重新生成，不重新接收，清理归属不明则进入 `cleanup-unknown`。原文件可直接以已封存的单一 owned object 形成 `PackagedArtifact`。

`PackagedArtifact` 的发布、结果记录和清理使用持久状态转换及跨标签页 lease。发布结果无法确认时进入 `publication-unknown`，不得自动重试或覆盖目标。聚合记录扩展现有恢复系统，不另建第二套 have-state 权威；逐文件可恢复范围仍只来自 checkpoint。

受限便携 ZIP 与工作区 ZIP 使用显式 preparation gate：先遍历已提交 generations，生成有界的 `PreparationManifest` 和精确布局度量；manifest 封存且容量 admission 成功后，content gate 才允许请求文件。受限便携下载校验最终产物硬上限；工作区 ZIP 校验 `WorkspaceBudget` 峰值。manifest 绑定全部所用 generations，避免二次遍历改变证据；超过元数据预算同样在零内容请求时失败。目录树、原文件和安全直接目标继续边发现边传输。

OPFS admission 的 job/process 上限计入整个 `WorkspaceBudget`，不再只计算原始文件；旧 staging limit 直接替换为语义精确的 `DEFAULT_OPFS_JOB_WORKSPACE_LIMIT`、`DEFAULT_OPFS_PROCESS_WORKSPACE_LIMIT`，quota 仍保留 `MINIMUM_OPFS_QUOTA_RESERVE`。ZIP 为 store 模式，峰值至少包含原始内容与近似等大的最终归档；`planZipLayout` 必须给出精确归档字节数。浏览器 quota 估计不是预留，所有分配仍重新 admission；动态 quota 失败保留可验证工作区并进入 `Resumable`，不得生成残缺 ZIP 或自动改为目录树。

`ZipEncodingPolicy` 是唯一的 ZIP 编码与长度规则，覆盖 UTF-8 路径、headers、data descriptor、central directory 和 ZIP32/Zip64 封口。普通直接 ZIP 为每个已 admission entry 生成不可变 `ZipEntryPlan` 并追加 `ZipLayoutLedger`，不等待完整 discovery；writer 封口时反证账本总长度。

受限便携 ZIP 和工作区 ZIP 在 `PreparationManifest` 封存后由纯 `planZipLayout` 生成完整 `SealedZipLayoutPlan`，供容量 admission、packaging 和 writer 共同消费。直接 ZIP 复用相同 planning primitives，不维护第二套公式。

## 生命周期与终态

```text
common                draft → shape-confirming → artifact-offered → artifact-chosen → destination-authority-acquired? → destination-reserved/workspace-bound → receive-intent-frozen
DirectTree            receiving ↔ Resumable → Published | PartialDirectory(failures | stopped) | Discarded | NeedsAttention
DirectAtomic          receiving → Published | RestartRequired | Discarded | NeedsAttention
WorkspaceThenPublish  preparing → receiving ↔ Resumable(receive, expires-at) → materialization-sealed → packaging ↔ Resumable(package, expires-at) → artifact-sealed → waiting-to-save(expires-at)
                       waiting-to-save ↔ publishing-managed → Published | NeedsAttention
                       waiting-to-save/DownloadStarted ↔ handing-off → DownloadStarted(retryable-until) | NeedsAttention
                       resumable/waiting-to-save/DownloadStarted → Discarded | NeedsAttention
PortableHandoff       preparing → receiving → DownloadStarted(none) | RestartRequired | Discarded
```

终态使用判别联合，避免任意组合出矛盾状态：

```text
Published(clean | cleanup-pending)
DownloadStarted(suggested-name, retryable-until | none)
PartialDirectory(failures | stopped, resumable | none)
WaitingToSave(packaged artifact, expires-at)
Resumable(receive | package, materialization/checkpoints, expires-at)
RestartRequired(reason)
Discarded(clean)
Expired(cleanup-pending)
NeedsAttention(target-ownership-unknown | publication-unknown | cleanup-unknown)
```

目录文件失败或用户停止可得到 `PartialDirectory` 并保留成功文件；`DirectTree` 只有尚无成功文件且 owned transient state 已清理时才能得到 `Discarded`。`DirectAtomic` 已确认回滚、portable 已确认中止时显示 `RestartRequired`；持久工作区或目录 checkpoint 确认可继续时才显示 `Resumable`；只有 ownership-scoped 清理完成才显示 `Discarded`。`Published` 只表示受控目标提交已确认；`BrowserHandoff` 只能得到 `DownloadStarted`，不宣称已经保存或保证最终名称。workspace-backed handoff 在原到期时间前保留再次下载能力，portable handoff 不保留恢复能力。发布完成但清理待重试仍是 `Published`；到期任务进入 `Expired` 并禁止继续，清理无法确认时进入 `NeedsAttention`。

前台恢复先检查持久状态和 lease，再由纯 `recoverAbandonedOperation` 归约无主活动状态：持久目录或工作区遗留 `receiving` 在 checkpoint/ownership 复核后进入 `Resumable(receive)`，`DirectAtomic` 则按回滚证据进入 `RestartRequired` 或 `NeedsAttention`；遗留 `materialization-sealed`/`packaging` 只有在 package temp 可确认清理或继续时进入 `Resumable(package)`，否则进入 `cleanup-unknown`；遗留 `artifact-sealed` 进入 `WaitingToSave`。遗留受控发布若能证明提交则进入 `Published`，能证明尚未提交则回到 `WaitingToSave`，否则进入 `publication-unknown`；遗留 handoff 能证明尚未开始时回到 `WaitingToSave`，否则进入 `publication-unknown`。已记录的 workspace-backed `DownloadStarted` 在原期限内恢复再次下载能力。每次归约到新的稳定可继续状态时生成 24 小时到期时间，绝不根据旧 heartbeat 或 handoff 重置期限。

## 实施顺序

1. 先冻结 projection epoch、`SelectionProjection`、`DiscoveryState`、名称/替换/交付/提交/回滚保证、`DestinationReservation`、`MaterializationPlan`、`PackagedArtifact` 及各计划生命周期的跨层语义。
2. 在 core 定义 `ArtifactSpec`、`ArtifactLayout` 和 `ReceiveIntent`，切换 canonical vectors、admission 与 checkpoint binding，删除旧 `TransferIntent.output` 合同。
3. 实现 epoch-scoped 单调选择投影、独立 discovery 状态和纯产物规划器，替换分享根层单文件启发式；controller 只接受当前 epoch 的异步事件，并在 `Tree` 已证明时立即停止形状阻塞。
4. 重构浏览器目标获取：分离 environment offers、picker adapter、destination authority acquisition、`DestinationReservation`、publication attempt 和工作区；排除 `showSaveFilePicker` 的不安全完整产物计划，实现单文件目录布局、`CoordinatedNoReplace`、同任务恢复和冲突停止。
5. 拆分 materialization、packaging 与 publication；在现有 repository 中实现 manifest digest、原始内容封存、最终产物封存、跨标签页 lease、`recoverAbandonedOperation`、等待保存、workspace-backed handoff 重试、暂停/停止/放弃、闲置 24 小时到期及前台清理。
6. 为受限便携下载和工作区 ZIP 实现 preparation gate、manifest 预算与完整 `WorkspaceBudget` admission；共享 `ZipEncodingPolicy`、渐进 ledger 和封存 layout plan，保证预检超限时零内容请求。
7. 用面向产物的操作、阶段进度和精确终态替换 `V2OutputIntent` 与后端文案，区分 `Published` 与 `DownloadStarted`；结构化 trace 携带稳定标识和决策上下文，不记录密钥、文件名或明文路径。
8. 同步精简的行为摘要、产品澄清、协议表述和测试；直接删除旧适配器、启发式、OPFS cancel partial-export、向量和 UI 分支，不保留兼容路径。

## 验证

- 表驱动覆盖未知、无选择、显式空目录、单文件、一个文件加空目录、完整/部分目录、多根和未结算目标；证明投影单调、失败重试保留证据、旧 epoch 事件被丢弃、`Tree` 证明后不继续阻塞、异步确认不自动打开 picker，非法产物与目标组合不可构造。
- 证明目录与 ZIP 使用同一结果根布局，并覆盖建议名称、单文件目录布局、workspace 续传、外部重名和归属不明。
- 用 writer 实际输出反证渐进 ledger 与封存 layout plan，覆盖 ZIP32/Zip64 边界、原始内容加最终 ZIP 的峰值预算、预检超限及 manifest 预算耗尽时零内容请求。
- 覆盖名称权威、替换、交付、提交和回滚保证的合法组合、FSA 只能报告 `CoordinatedNoReplace`、拒绝 `UserAuthorizedReplace`、禁止前缀目标承载完整产物、下载 handoff 不报告已保存，以及发布结果不确定且不自动重试。
- 覆盖接收、原始内容封存、packaging、最终产物封存、受控发布和 handoff 的崩溃切点，materialized revision 或 owned object 不匹配、跨标签页竞争、刷新后换目标、取消发布进入等待保存、暂停/停止/放弃不生成残缺产物、无主状态恢复、活跃操作不过期、workspace-backed 下载在原期限内重试、闲置 24 小时后禁止继续、下次前台清理和受控发布成功后清理。
- 保留用户激活、路径约束、禁止覆盖、checkpoint、目录失败隔离和真实零字节文件测试；复用现有浏览器 fixture，避免显著增加本地测试时间。
- 实施期间运行聚焦 Web 测试或 `make check`；代码、向量和维护文档同步后运行 `make ci`。

## 范围边界

本次不增加发送端归档或压缩、整棵分享快照、Service Worker 保活、新的 relay/WebRTC 行为、CLI 用户输出模式或第二套恢复系统。CLI 只适配新的 core intent，不新增产品选项。

不增加逐文件多次下载、残缺 ZIP、分卷 ZIP、`showSaveFilePicker` 覆盖模式、feature flag、旧 UI 分支或自动格式降级。除受限便携下载和显式工作区 ZIP 的有界 `PreparationManifest` 与容量预检外，普通输出不等待整树 discovery 才打开输出或写入首个已 admission 文件。
