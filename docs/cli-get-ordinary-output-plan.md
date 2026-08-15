# CLI 普通接收输出重构计划

> 状态：已实现。W0–W4 与本地 `make check`、`make long-go`、`make ci` 已于 2026-08-14 完成。
>
> 目标：让 CLI 接收在普通下载目录里表现得像一次正常的文件复制，同时保留路径安全、完整性验证、不覆盖已有文件和 best-effort 断点续传。高频的首次下载路径不能为低频的终态幂等重跑付出额外扫描、哈希、复制或复杂提示。

## 1. 用户合同

用户可以直接运行：

```text
windshare get <capability-link>
windshare get -o <directory> <capability-link>
```

不带 `-o` 使用当前目录；`-o` 指定保存容器，不把多个根项目摊平到容器中。目标目录可以是普通的 Downloads 目录，也可以是已经存在的目录；不要求提权、预建私有目录或修改 ACL。

结果布局固定为：单文件直接使用文件名；完整选择一个目录使用 `<目录名>/...`；目录内部分选择使用 `<目录名>-selection/...`；多根或 synthetic selection 使用 `windshare/...`。`-o` 本身永远是容器，不是分享结果根。

这里的“布局冻结”只冻结结果的顶层形状、结果根和名称，不冻结目录清单。shape planner 消费有界认证 metadata，返回只存在于构造期的不可变 `ShapeDecision`：shape kind、认证 anchor、canonical source path、preferred result name 和 fallback reason；不得携带完整 manifest、catalog page、文件路径清单或内容 hash。

`single-file` 必须证明唯一选择是一个文件；`complete-directory` 必须证明一个目录被完整选择且没有排除项；`partial-directory` 必须证明所有选择都位于唯一的最近真实目录祖先下，但没有完整选择该目录；其余情况为 `multi-root/synthetic`。判定只消费有界认证 metadata 和 selection projection/rules，不遍历或比较完整清单。node-ID 选择无法在预算内证明祖先和完整性时立即选择 synthetic `windshare/` fallback，不继续等待。后续 catalog generation/page 继续渐进发现，不改变已经冻结的 shape。

shape 冻结后才生成 operation identity，并在父句柄下有界地预留实际结果名称。`ShapeDecision` 必须一次性物化为现有 canonical 合同：`ArtifactSpec` 表达 shape、anchor、source path 和 preferred name，`MaterializationPlan` 内的 `DestinationReservation` 表达实际结果名与 destination authority。不得新增 `ShapeBinding`，也不得把 shape 判定证据持久化成第二份路径或命名权威；恢复只复用冻结的 `ReceiveIntent`。

`ArtifactPathProjector.Project(sourceNode, ReceiveIntent)` 是纯函数并返回三态：anchor 以上、到达 anchor 所必需的认证祖先为 `traverse-only`，只参与发现；anchor 下被选择或通向被选后代的节点为 `materialize(artifactPath)`；无关分支、anchor 外非祖先或不安全路径为 `reject`。只有 `materialize` 产生输出、checkpoint identity 和失败报告路径，`outputruntime` 不得再次解释 source path。

`ReceiveIntent` canonical 编码是 Go/TypeScript 共享合同。普通输出复用现有 `ArtifactSpec + MaterializationPlan`，`ShapeDecision` 不进入 canonical 编码，因此不改 Web 合同或浏览器恢复记录。

active operation 查找与 `ReceiveIntentDigest` 分离。绑定目标根并冻结 `SelectionSpec` 后，先计算不含 operation ID/reservation 的 `ActiveOperationKey = SelectionSpecDigest + DestinationAuthorityID + ordinary-output policy version`，只查找未终结 operation。`DestinationAuthorityID` 来自当前打开句柄验证的 native root identity 和 root-owned namespace；规范化绝对路径只作定位/展示提示，不授予恢复权限。恰好一个 active 匹配时复用其完整 `ReceiveIntent`、operation ID 和 reservation，不重新规划 shape 或名称；匹配项为 `operation-needs-attention` 时停止并提示处理，没有匹配时才执行 shape planning 并创建新 operation。该 key 不索引或认领历史 final。

| 场景 | 行为 |
| --- | --- |
| 目标目录可安全使用 | 完成有界的顶层 shape probe，无法证明时使用 synthetic fallback；随后立即渐进发现和下载，不加载完整 manifest、不读取或哈希源文件内容 |
| `-o` 容器不存在 | 从最近的已有父目录安全创建容器；父目录不可写或路径经过链接时提前失败 |
| 保存容器已存在 | 将其作为容器使用；不把分享内容摊平到其中，不重放容器 metadata |
| 同一 active operation 已创建结果目录 | 复核结果根 ownership 后进入；其下已有普通目录可按 root-confined/no-follow 规则遍历并创建缺失子目录；无关同名结果根改用后缀，不合并 |
| 选中空目录 | 创建或进入目录即可完成该目录；不等待文件 settlement |
| 结果根或单文件目标已存在 | 永不覆盖、替换、合并或跟随；全新 operation 选择统一、持久化的 operation-derived 短后缀，续传始终复用原名称 |
| operation 内部路径后来发生冲突 | 普通目录可安全遍历；foreign file、链接或 file-as-ancestor 保持不变，只失败该项或子树；不在内部追加后缀 |
| 某个目录或文件不可写 | 只失败该项或子树，已发布文件保留；operation 保持 active，修复权限后同一 `get` 复用原名称并只重试未完成项 |
| 下载中断且有恢复记录 | operation 保持 `resumable`；下次执行同一 `get` 时继续已验证区间，已完成文件保留 |
| 完整 resume 能力不可用 | `SafePublish + CrashCleanup` 可证明时 warning-once 并以 live-only 下载；下次 `get` 视为新 operation，选择新结果名并重新下载整个 selection，已发布结果保留；否则提前失败 |
| 文件系统不支持 `SafePublish` 或 `CrashCleanup` | 首个内容请求前失败，并给出更换目录或文件系统的下一步 |
| checkpoint 与 partial 无法对应 | final 明确缺失时不认领、不删除旧 partial，另建新 partial；无法判断是否已发布时仅将该项标为 `item-blocked` |

成功只表示本次选择均已发布，且没有 failure、collision 或 `item-blocked`。命令结果与 operation 生命周期分离：本次运行可以是 success、partial、paused 或 failed；持久 operation 只分为 `active`、`operation-needs-attention`、`completed`、`discarded` 和 `cleanup-pending`。`item-blocked` 属于文件/子树，不阻止独立兄弟项，operation 仍为 active；只有目标根、registry、lease 或 operation ownership 不确定时才使用 `operation-needs-attention` 并停止整个 operation。可安全重试的 collision、permission、subtree 或 session failure 不终结 operation，并复用已发布 evidence、结果根和名称；删除 terminal `partial-directory` 和 collision-only `discarded`。只有全部选择成功或用户明确 discard 才结束传输；第一版不自动过期。已有 final 没有 active 记录时不读取整文件，直接为新 operation 选择后缀；不提供自动覆盖开关。

## 2. 优先级与明确不做的事

按用户体验排序：

1. 首次下载只走一次有界内存的内容流：认证块到达后直接写盘，不做整文件 hash、Merkle commitment、临时完整副本或完成后的复制。
2. final 只有在内容、大小、路径安全和必要 identity metadata 验证完成后才可见；partial 与 final 必须位于同一文件系统，发布使用原子 no-replace publish。平台可用 rename 或等价 native 原语实现，但不得退化为复制、截断或替换。
3. 续传是 best-effort。丢失或损坏 checkpoint 时重新下载该文件，不把整个目标目录升级为失败。
4. 文件级故障隔离优先于整树回滚；用户已经拿到的文件不因兄弟项失败而消失。
5. 结果根和文件名按普通下载器习惯冲突改名；同一 active operation 的恢复不改变已选名称。

本计划不实现以下低频能力：

- 普通输出的终态历史索引和跨命令自动认领 final；
- 完整内容 commitment/Merkle frontier；普通输出仍要求原子 no-replace publish。平台可以在内部保留隐藏的 native checkpoint/ownership proof（包括 hard-link 或等价原语）来支持重启复核，但不得复制完整内容、暴露第二个 public object 或把内部 proof 变成 CLI 合同；
- 掉电级 cleanup 证明、抵抗同一 OS 账户恶意本地进程的独占 ACL 合同；
- 为了恢复已有 final 而进行整文件重读或重哈希。
- per-file user-data resume 镜像或普通 `get` 路径上的全局 operation inventory。

这些能力以后若有明确用户需求，应作为独立产品决策，不进入普通 `get` 的成功判定或核心写入路径。

Web 的 FSA/workspace/portable 输出、浏览器保存/交付行为和 `ReceiveLifecycleState` 不在本计划内。Web 只同步共享 canonical contract；不得因 CLI 重构引入 `DestinationAuthorityID`、`OperationRegistry`、native publish 或 CLI naming/resume policy。

## 3. 输出架构

普通输出只保留三个逻辑职责；优先收敛现有实现，不并行维护第二套 output、ranges 或 settlement 模块：

### 3.1 `DestinationAuthority`

它持有绑定到目标根的 native handle，并负责：

- canonical path 解析、root-confined lookup 和 no-follow ancestor 检查；
- 目录的创建/遍历及每个 item 的实际权限检查；
- final parent 与目标根 identity 的重新验证；
- 原子 no-replace publish 的平台能力报告；底层可用 rename 或等价 native 原语实现，但不得退化为复制或替换。
- 认证并打开 root-owned operation namespace；新 operation 在内容请求前预留单文件目标或结果根名称，冲突时持久化 operation-derived 后缀。

它不计算 ACL/DACL 的“独占性”，也不把路径字符串当作后续 mutation authority。普通 inherited ACL、group write、named ACL 只要当前 mutation 具备实际所需权限，就不应被全局拒绝；未知对象永不自动接管、删除或替换。

平台报告四个正交事实：`SafePublish`（root-confined、no-replace、完整后原子可见）、`OperationRecovery`（复核 active operation 与已发布 evidence）、`RangeRecovery`（复核 partial/checkpoint 并继续 verified ranges）和 `CrashCleanup`（进程崩溃后仍能只清理自有 partial）。CLI 只暴露两种执行模式：四项均成立时为 resumable；否则仅当 `SafePublish + CrashCleanup` 成立时为 live-only，不持久化可恢复 operation。`SafePublish` 或 `CrashCleanup` 不成立时必须在首个内容请求前失败。能力探测只检查当前句柄和 native 原语，不扫描目录；NTFS/ext4 是首要支持目标，网络盘、FUSE、云占位或不明文件系统不得静默宣称可恢复。

### 3.2 `PartialFileTransaction`

每个文件只分配一份内容数据，并且 partial 与 final 位于同一文件系统。partial 可以位于最终父目录的应用管理子目录，也可以位于目标根内的 native checkpoint namespace；具体位置由平台实现决定，但必须支持原子 no-replace publish 和无内容复制，resumable 模式还必须支持重启后的归属复核：

```text
<target-root>/<partial-namespace>/<random>.part
                              │ WriteAt authenticated blocks
                              │ checkpoint verified ranges
                               └─ atomic no-replace publish ─→ <final-parent>/<final-name>
```

`<partial-namespace>` 可以是 final parent 下的隐藏目录，也可以是目标根内的 native checkpoint namespace；它不是用户可见的结果目录。

partial 所在目录由平台隐藏管理；隐藏只是体验细节，不产生所有权。文件继承正常 permission profile；不修改 ACL，不把 hard-link 或其他 native proof 暴露为 final 的 public 权限。正常结束或可证明归属时清理 partial，目录为空时删除；陌生残留永不自动认领。live-only 也必须通过 delete-on-close 或最小的 root-bound cleanup proof 保证 `CrashCleanup`；该 proof 只能清理自有残留，不能恢复 operation 或内容。

接口复用现有 `transfer.FileTransaction` 生命周期，不再另造一套 ranges/settlement 合同：

```text
Binding()
WriteRange(offset, authenticatedBytes)
Checkpoint() -> verified ranges
Commit() -> downloaded/resumed or collision
Pause(reason)
Retire(reason)
```

`Begin/Reopen` 属于 `DirectTreeSession` 的 admission；句柄关闭和资源回收由 transaction/session 内部负责，不作为 CLI 可见状态。

平台实现封装 `CreateFile`、`openat`、`WriteAt`、同步和 no-replace publish 的差异；runtime 不搬运 syscall、句柄或 permission evidence。live-only 不写 durable checkpoint，但仍经过同一 `SafePublish` 事务，并在首次写入前建立 `CrashCleanup` 保证；崩溃残留在句柄关闭时或下次打开目标根时清理，未知对象永不认领。

`os.WriteFile` 不作为大文件数据 API：它有完整 `[]byte` 和截断语义。内容路径使用 `io.WriterAt`/`*os.File.WriteAt`，内存只受单个协议块和重排窗口限制。

### 3.3 `OperationRegistry`

`OperationRegistry` 位于目标根的 root-owned namespace，按 `ActiveOperationKey` 保存有界的 operation 级记录：operation ID、冻结的 intent、lease 和持久生命周期。记录大小不得随文件数增长，也不保存 checkpoint reference、文件路径、revision、ranges 或 phase；目标根内按 operation 分区的 `FileCheckpointV2` 是文件状态的唯一事实来源。

第一版只维护目标根内的 active-only operation registry，不维护全局索引。所有命令先打开用户指定的目标根并复核 root identity；普通 `get` 只做有界 active lookup，只有恢复已匹配 operation 或执行 `resume list/discard` 才复核其 checkpoint、partial 和 final。这样不会为低频 inventory 增加首次下载成本。

destination binding 分离 `DestinationAuthorityID` 与规范化绝对路径：前者是恢复 authority，后者只是展示提示。当前打开根的 identity 与 root-owned namespace 都匹配时，同文件系统重命名不使 operation 失效；路径相同但 identity 已变化、跨文件系统复制或记录无法认证时不得恢复。不扫描 public output tree，不猜测、不删除陌生文件。记录采用小记录原子替换；仅在首次写入前确认完整 resume 不可用且 `SafePublish + CrashCleanup` 成立时使用 live-only。registry 写入结果不确定时进入 `operation-needs-attention`。

新的 `get` 先按 `ActiveOperationKey` 查找 operation：恰好一个 active 匹配则复用其冻结 intent；匹配项为 `operation-needs-attention` 时停止，没有匹配才创建新 operation。只有恢复已匹配 operation 或执行 `resume list/discard` 时，才按页枚举其 checkpoint metadata；文件到达时再以 `ReceiveIntentDigest + DestinationAuthorityID + canonical artifact path + FileID/FileRevision/size` 精确匹配。该低频枚举不得进入首次下载写入路径。多个 operation、root/registry/lease ownership 不确定时不猜测；lease 明确被持有时报告 operation 已在运行。completed、discarded 和 cleanup-pending 不参加 active lookup，其中 cleanup-pending 只进入 `resume list`。

成功的 `WriteRange`、进度 UI、aggregate summary 和错误等级都不能自行制造 ranges。恢复、发布、清理和 `resume list/discard` 的最终决策必须回到目标根 authority 与 `FileCheckpointV2`。不要为普通 CLI 改写 Web/TypeScript 共享的 `FileCheckpointV2` 合同。

## 4. 主路径与恢复路径

### 4.1 全局 admission

首个内容请求前只做不读取内容的准备：

1. 打开并绑定 destination root，探测四项能力；`SafePublish` 或 `CrashCleanup` 不成立就提前失败。
2. 冻结 `SelectionSpec` 并计算 `ActiveOperationKey`。resumable 模式只查询有界的 `OperationRegistry`；恰好一个 active 匹配则取得 lease 并复用冻结 intent/reservation，`operation-needs-attention` 匹配不得自动继续。
3. 没有 active 匹配时，在 content lane 保持 suspended 的情况下执行有界、已认证的 metadata-only shape resolution；不加载完整 manifest。随后生成 operation identity、预留结果根/单文件名称，并将 `ShapeDecision` 一次性物化为 canonical `ArtifactSpec + MaterializationPlan`。
4. resumable 模式在首个内容请求前持久化 active-operation record 并取得 lease；live-only 只使用进程内 lease、`SafePublish` 和 `CrashCleanup`。
5. 只有在首次写入前确认 `SafePublish + CrashCleanup` 成立且完整 resume 不可用，才可 warning-once 后使用 live-only。多个匹配、lease contention、持久写入结果不确定或 root/registry/lease ownership 不明不得降级：分别报告 already-running 或 `operation-needs-attention`。

全局 admission 不要求扫描目标目录，也不要求证明公共目录对当前用户“独占”；只允许有界的父目录、目标名称和必要碰撞探测。

### 4.2 增量 item admission

目录 generation 到达后，逐项执行：

- 先校验 canonical source path，再调用 `ArtifactPathProjector`；`traverse-only` 只推进认证 catalog ancestry，不访问 destination，`reject` 不产生输出；
- 只有 `materialize(artifactPath)` 才从 retained destination handle 创建或打开目录；已有普通目录只做安全遍历；
- 在文件首次 mutation 前重新确认 parent identity、create/write/publish 权限和 final 名称状态；
- 将 collision、permission denied、unsupported item 和 network/content failure 分成 typed fault。

后发现的 item 失败不回滚已发布文件，也不阻止独立兄弟项。

### 4.3 文件事务

1. 有匹配 `FileCheckpointV2` 且 partial 的 parent、identity、size、phase 均通过复核时，`Reopen` 并从 verified ranges 的缺口继续；匹配由 `ReceiveIntentDigest + DestinationAuthorityID + canonical artifact path + FileID/FileRevision/size` 唯一确定。
2. 没有可恢复 record 时，在平台选定的 partial namespace 中以随机名和 `O_EXCL`/Windows 等价语义创建 partial；该 namespace 必须与 final 位于同一文件系统，绝不以 final 名称创建并截断已有对象。单文件目标或结果根的重名已由 destination reservation 在内容请求前裁决；同一 active operation 永远复用已预留名称。
3. 仅把完整认证且位于 expected geometry 内的块写入 partial。乱序块使用 `WriteAt`；resumable checkpoint 可以批量提交，不为每块强制 fsync。
4. 收齐 exact size 后验证 ranges 覆盖整个文件；resumable 模式按声明的 process-restart durability 同步文件和 checkpoint，live-only 只完成安全发布所需的同步。内容、大小、路径安全和必要 identity metadata 是硬失败；mtime 等显示属性尽力设置，失败只 warning，不让已验证内容退回 partial。
5. resumable 模式在发布前持久化 `publishing` checkpoint，绑定 exact partial identity 与 final parent/name expectation；live-only 只保留进程内 phase 与 crash-cleanup binding。随后在仍持有 parent authority 时执行 atomic no-replace publish，并同步 final parent。目标后来被占用时保留 foreign final，该项 settle 为 `collision`；只有无法判断 publish 是否发生时才标为 `item-blocked`。不临时改名、不 unlink、不 replace。
6. resumable 模式在同步完成后持久化 `published` checkpoint，并保留到 operation terminal settlement，使恢复能跳过已完成文件；live-only 不保留恢复 evidence。随后尽力清理；清理失败不撤销 final，只有 resumable operation 显示 `cleanup-pending`。

发布前后都不完整复制、拼接或重写内容，因此 100 GB 文件的 owned data space 约为 100 GB 加有界 metadata。

### 4.4 恢复 reducer

| 观察 | 决策 | 范围 |
| --- | --- | --- |
| active record 与 partial 完全匹配，final 缺失 | 继续缺口 | file |
| `publishing`/`published` checkpoint 与已有 final 的 native identity 完全匹配 | 认定 publish 已完成，不再写内容 | file |
| final 已存在但没有匹配 active record | 为新 operation 选择持久化后缀，不覆盖、不读取整文件 | operation admission |
| partial 缺失且 final 明确缺失 | 为该文件建立新的 partial，从零下载 | file |
| checkpoint 缺失或损坏，final 明确缺失 | verified ranges 失效；保留身份不明的旧 partial，另建新 partial | file |
| final/partial 状态或 publish 结果无法判定 | `item-blocked`；保留对象并继续独立兄弟项 | file/subtree |
| 目标根、registry、operation 或 lease ownership 无法判定 | `operation-needs-attention`；停止整个 operation | operation |

这里只恢复 active operation。operation 完成并清理后，下一次全新的 `get` 不会自动把同名 final 认领为本次下载结果。

## 5. 用户可见结果与提示

CLI 只输出影响和下一步，不暴露 ACL、SID、UID、内部路径或 capability 内容。每次运行最多提示一次能力降级：

```text
This destination supports safe saving, but this download cannot be resumed.
If interrupted, completed files stay; the next get uses a new name and downloads the selection again.
```

交互终端用单行刷新显示已发现/已完成文件数和字节数；discovery 未完成时不显示百分比。stderr 被重定向时不输出周期进度，只写 warning、失败项和最终摘要；stdout 不承载进度。失败项使用相对 artifact path，稳定机器输出以后通过独立 `--json` 合同提供，不要求脚本解析英文文案。

文件摘要使用少量稳定结果：

- `downloaded`：本次从零接收并发布；
- `resumed`：从匹配 active partial 继续并发布；
- `renamed`：新 operation 因顶层重名使用了持久化后缀；
- `collision`：operation 内部路径后来被占用，已有对象保持不变；
- `failed`：该项未完成；
- `item-blocked`：本地状态无法安全判定，保留现状并提示用 `resume list` 处理；
- 目录 `created`、`traversed` 或 `subtree-failed`。

只有全部选中文件为 `downloaded`/`resumed`（允许结果根或单文件目标为 `renamed`），且没有 collision、failed 或 `item-blocked` 时返回成功。mtime 等显示属性 warning 不改变成功；空目录按目录结果独立成功。warning 保持退出码 0；未完成、collision、item-blocked、operation-needs-attention 或整体暂停返回非 0。文件/目录故障只隔离该项或子树；sender 退出、session terminal 或整体连接不可用使本次命令 paused/failed，operation 仍为 active，不伪装成多个 file-local fault。

`windshare resume list -o <directory>` 按 operation 分列 `incomplete`、`resumable`、`cleanup-pending` 和 `operation-needs-attention`，并在 operation 下列出 `item-blocked` 项；不维护 terminal provenance。`incomplete` 表示没有可用 partial、但仍可按原 intent 重试；只有 `resumable` 含 active partial。`resume discard` 只能删除 record 明确绑定且 identity 仍匹配的 unfinished partial，永不删除 final 或陌生对象。

## 6. 核心重构边界

保留：

- `ReceiveIntent` 对 selection、canonical `ArtifactSpec` 和 destination reservation/binding 的冻结；构造期 `ShapeDecision` 不持久化为第二份权威，ordinary CLI 在首个内容请求前只完成顶层 shape/layout 规划；
- 独立于 `ReceiveIntentDigest` 的 `ActiveOperationKey`，只定位未终结 operation，不索引 terminal final；
- `OperationRegistry` 的 active lookup/lease 和 `FileCheckpointV2` 的 verified ranges、partial 归属及文件生命周期；不建立 per-file user-data 镜像；
- `DirectTreeSession` 的目录/文件失败隔离及 typed settlement；
- Windows/Linux 的 root-relative、no-follow 和 native identity 复核；
- 结构化 trace：destination admitted、file admitted、first write、checkpoint promoted、publish committed、collision、pause 和 cleanup decision。

删除或从 ordinary output 移出：

- 终态历史、跨命令自动认领 final、content commitment/Merkle accumulator 和 terminal `SelectionPlanDigest`；active-only operation lookup 仍须保留；
- 将 hard-link、anchor、witness 或内部 recovery evidence 暴露为 public publication 合同。必要的 native proof 只留在目标根绑定的 checkpoint namespace；
- 通过解析错误文本或用户开关改变安全语义；执行模式只由 typed capability facts 决定。

建议的 consumer-side contracts：

```text
DestinationAuthority  // root-bound item admission and no-replace publication
PartialFileTransaction // one data object, WriteAt, checkpoint, commit
OperationRegistry      // bounded root-bound active lookup, lease and lifecycle
```

`outputruntime` 只编排这三个模块和 catalog/session 生命周期；shape planner 只在 `ReceiveIntent` 构造期消费有界认证 metadata，`ArtifactPathProjector` 只解释冻结后的 intent 并返回 traverse-only/materialize/reject。优先重构现有 transfer/output contracts，不并行维护第二套 ranges、settlement、路径或 ownership 语义。平台 syscall、权限继承和 native identity 留在对应 transaction。core 继续网络无关，CLI 不重新推断 paths、ranges 或 ownership。

## 7. 实施顺序

### Phase 0：冻结用户合同

- 固定 `SafePublish`、`OperationRecovery`、`RangeRecovery`、`CrashCleanup` 能力事实，以及 resumable/live-only 两种执行模式；live-only 的 warning 明确下次使用新名称并重下整个 selection。
- 固定构造期 `ShapeDecision`、`ArtifactPathProjector` 三态和预算；shape 一次性物化为 canonical `ArtifactSpec + MaterializationPlan`，超出预算立即使用 `windshare/`。
- 固定 `ActiveOperationKey`、`DestinationAuthorityID`、有界 active-only `OperationRegistry` 和 operation lifecycle；区分 `item-blocked` 与 `operation-needs-attention`。
- 明确旧 output records 不自动迁移或认领；实现阶段显式停用旧 namespace，过期记录只按已证明的 ownership 清理。
- 固定 Windows/NTFS、Linux/ext4、普通继承 ACL、网络盘/FUSE 的能力矩阵；普通 profile 不因可用 ACL 被全局拒绝。

### Phase A：固定平台主路径

- 先为 Windows/Linux 平台补齐 `PublishNoReplace(partial, final)` 的统一语义：同文件系统、原子不覆盖、无内容复制；resumable 模式另需重启后复核 partial 归属。平台可选择 rename、hard-link 或等价 native 原语，但实现细节不进入 public contract。
- 删除 ordinary profile 对 group/named ACL 的全局排他性拒绝，只保留实际 create/write/publish、root-confined 和 no-follow 检查；分别探测四项能力，只有 `SafePublish + CrashCleanup` 成立时才允许 live-only。
- 确认 publication 期间没有内容复制；只允许有界 metadata 写入。
- 分别报告四项能力；无法保证 `SafePublish` 或 `CrashCleanup` 的文件系统在首个内容请求前返回 typed unsupported。

### Phase B：重建 core output

- 将 selection projection/shape planner 接入 `ReceiveIntent` 构造；`ShapeDecision` 只存在于构造期并一次性物化为现有 canonical 合同，预算内无法证明时使用 synthetic fallback。`ArtifactPathProjector` 以 traverse-only/materialize/reject 统一控制发现和输出。
- 将现有 file execution 收敛为 `PartialFileTransaction`；删除 terminal/public hard-link 合同和重复的 publication history，但在新的 partial identity/publish contract 验证前保留必要的内部 native recovery proof；把内容/身份硬失败与 mtime 等显示属性 warning 分开。
- 为 `SafePublish + CrashCleanup` 成立但完整 resume 不可用的文件系统提供 live-only 路径；不创建持久 operation 或可误认领的 checkpoint。
- 以 `ActiveOperationKey` 重构有界的目标根 `OperationRegistry`；checkpoint 只在恢复/list/discard 时按 operation 分页枚举，`FileCheckpointV2` 继续承载唯一的文件事实。
- 仅重写 ordinary native/CLI lifecycle reducer，将 command result 与 operation lifecycle 分离，并区分 `item-blocked` 与 `operation-needs-attention`；删除 terminal `partial-directory` 和 collision-only `discarded`，保留 sibling failure isolation。Web lifecycle 不变。

### Phase C：接入 CLI 与恢复

- `get` 默认使用当前目录；`-o` 只传递保存容器，先以有界 `ShapeDecision` 构造 canonical artifact 与顶层 reservation，再继续渐进发现。
- 实现 active-first resume、顶层冲突持久后缀、内部 collision isolation、warning-once 和 TTY-aware summary；通过注入式 progress sink/TTY detector 输出，core 不直接写终端。
- 实现按 operation 分页的 incomplete/resumable/cleanup-pending/operation-needs-attention inventory，并列出 item-blocked；live-only 必须先证明 crash cleanup，不认领或删除身份不明对象。
- 更新 get/resume、native output、validation 和威胁模型文档，说明 active-only registry、普通继承权限和内部 recovery evidence 边界；共享 `FileCheckpointV2` 与现有 `ReceiveIntent` canonical 合同不变，不改变 Web 下载行为。

## 8. 验证重点

- 普通 Downloads 目录、继承 ACL/group write、已有容器和空目录；单文件、完整目录、部分目录及 synthetic 的 source-to-artifact 投影不得重复或遗漏结果根；
- 多页/多 generation catalog 的渐进发现；验证只冻结顶层布局，不加载完整 manifest，后续发现不改名、不搬移；
- symlink/junction/reparse、路径折叠、file-as-ancestor、foreign collision 和 no-replace race；
- 顶层重名持久后缀、同一 operation 恢复名称稳定、内部冲突不改名以及多匹配不猜测；permission/collision/断线使本次命令失败但 operation 保持 active，重试不得创建新结果根；
- 大文件单次写入路径、乱序 range、进程中断后 active resume、已发布兄弟项恢复、publishing/published crash cuts、checkpoint 丢失后的从零重建及孤儿 partial 防护；大量文件不得产生 per-file user-data 双写；
- 已发布文件保留、mtime warning、兄弟项/子树故障隔离、session terminal 整体暂停、registry 不确定时 operation-needs-attention，以及 cleanup-pending；
- TTY 单行进度、非 TTY 无周期刷屏，以及 stdout/stderr 边界；
- 使用聚焦 `make <gate>`/`make check` 迭代，交付前运行 `make ci`。
