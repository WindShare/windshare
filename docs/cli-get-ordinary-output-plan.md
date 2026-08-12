# CLI 普通接收输出重构计划

> 状态：待实现；基于 2026-08-11 的 Windows 本地复现。
>
> 核心决策：**已发布证明和未完成区间续传是彼此独立的可降级能力。** 普通可写目录不能因为 ACL/DACL 不独占而拒绝下载；路径约束、接收阶段的完整协议内容验证、原子不覆盖和 single-object publication 不得降级。按原始需求，断点续传总体上是 best-effort。普通首次接收和未公开对象的 active resume 不增加整文件扫描；已发布 final 的当前字节重证只服务于低频显式重跑，不是首次下载成功的硬条件。默认 ordinary profile 不计算 content commitment；只有选定相应恢复/幂等证据能力时才计算，不把额外 hash 成本带入普通首次下载。

实现分为两个独立交付：普通目录安全输出与 active/range recovery 是主路径；terminal 幂等重跑是可选派生能力。二者可以同一版本完成，但后者不得参与本次下载的成功判定或 cleanup authority。

## 1. 用户合同

用户应能在普通下载目录直接运行：

```text
windshare get <capability-link>
```

不带 `-o` 时使用 `.`；`-o` 只改变保存位置。两种写法都不要求提权、预建私有目录或修改 ACL。

| 场景 | 行为 |
| --- | --- |
| 支持全部能力 | 直接下载；兼容 active operation 自动续传，显式重跑可快速复用 continuity token 未变化的已发布文件（best-effort） |
| 有 ActivePublicationRecovery、缺少 RestartRangeRecovery | warning 一次；重跑同一命令时保留已证明文件，未完成文件重新下载 |
| 缺少 ActivePublicationRecovery | 首个内容请求前明确 warning；中断后不能认领本次已发布的 final，重跑不覆盖它们，需改用新的空目录重新开始 |
| 缺少 TerminalPublicationProof | 正常下载不提示；后续新 `get` 遇到已有 final 时按未验证 collision 解释 |
| 不支持安全 publication object 或原子 no-replace | 首个内容请求前失败 |
| 某个已发现的子目录或文件不可写 | 只失败该项或子树，其他项继续 |
| 已有普通目录 | 仅作为未认领容器进入；只创建缺失子项，不重放源目录元数据 |
| 选中空目录 | 新目录创建成功或已有普通目录可安全进入即为成功 |
| 收到的根项与 receiver-owned 动态控制目录精确冲突 | 只失败该项或子树；绝不进入控制目录，其他项继续 |
| final 与可信 terminal proof 的 `SelectionPlanDigest`、同一对象和 continuity token 均匹配 | 按 best-effort 记为 `previously-published`，不重复下载 |
| terminal proof 匹配但 continuity token 缺失或不确定 | 接收期间有 commitment 时，显式重跑可做一次 best-effort 内容校验；仍无法稳定验证则 `existing-unverified` |
| 最终文件名或其他对象已存在但无法证明 | 不覆盖、不认领；记为 `existing-unverified` 并返回 partial |
| active operation 的 publication 历史、final identity 或 ownership 无法重新认证 | 只冻结受影响项并报告 `needs-attention`；continuity token 变化按 `existing-unverified` 处理 |
| active operation 的 final 明确缺失且 private witness 与 content evidence 可认证 | 显式 `get` 以原子 no-replace 重新发布同一对象，summary 记为 `previously-published`；不得后台自动 mutation |

文件内容始终写入最终发布的同一个文件对象。完成阶段只允许原子 link/rename，不得完整复制、拼接或重写文件，也不得要求第二份等大的临时空间。

`ContentCommitmentAccumulator` 是可选的加强证据能力。仅在需要 active missing-final adoption，或希望为 token 不足的低频重跑提供内容校验时，才在 plaintext 已位于内存时按 versioned、domain-separated Merkle 规则增量计算；不增加内容 I/O，状态保持有界。未启用时，普通路径和 metadata-only `TerminalPublicationProof` 不计算 commitment；terminal proof 仍可保存 final identity、continuity token 与 plan digest，作为 best-effort 复用提示，不声称当前字节未被原地修改。continuity token 复用现有平台 identity/metadata 规则：Windows 使用 volume/file identity、size 和 last-write baseline，POSIX 使用 device/inode/size/mtime/ctime；它不是内容 hash 或原子快照。同一 native object 且 token 未变化即可按 best-effort 复用，不要求额外整文件扫描。token 缺失或不确定时，只有接收期间已有 commitment，低频显式重跑才可选地读取 final 做一次内容校验；失败或无法稳定观察则按 `existing-unverified` 处理。同 token 原地改写属于已接受残余。

缺少 ActivePublicationRecovery 时，在首个内容请求前输出一次较强 warning，并抑制较弱的 range warning：

```text
This destination can save files safely but cannot remember which files were published if interrupted.
Existing files are never overwritten. After an interruption, restart cleanly with: windshare get -o <new-empty-directory> <capability-link>
```

有 ActivePublicationRecovery、仅缺 RestartRangeRecovery 时输出：

```text
This destination can save files safely but cannot reliably retain unfinished progress.
If interrupted, run the same get command again. Completed files are kept; unfinished files are downloaded again.
```

TerminalPublicationProof 缺失不打扰正常下载。若 primary settlement 后写 terminal provenance 失败，本次原有结果和 exit code 不变，只在 summary 说明未来显式重跑可能无法识别这些 final。所有提示都不暴露 ACL、SID、UID 或内部目录，同一影响每次运行只说明一次。

`DurabilityNone` 只表示 verified ranges 不可跨进程恢复，不禁止保存 bounded publication/cleanup evidence。只有 ActivePublicationRecovery 可用时，中断后的同目录重跑才保证保留已证明文件并继续收敛；否则按前置 warning 改用新的空目录。可信 terminal proof 能证明 final 仍是同一发布对象且 continuity token 未变化时记为 `previously-published`；token 不足时，只有接收期间有 commitment 才在该次显式重跑中做 best-effort 校验，仍无法验证的已有文件保持非零 partial。`resume list` 只显示可继续、可处理或需丢弃的 durable operation；小型 terminal provenance 不进入 resume inventory，也不新增 resume 模式开关。

Terminal operation 不作为 active recovery 自动重开。用户再次显式执行 `get` 时，先在 request-key lease 下恢复或报告 compatible active operation；active operation 的 frozen `ReceiveIntent`/plan 是恢复依据，不能把新 revision 静默并入。只有不存在 active operation 且本次 terminal catalog discovery 已完成，才查找 operation-independent `ReceiveRequestKey` terminal proof。terminal discovery 后生成的 `SelectionPlanDigest` 绑定目录 generation 以及每个选中项的 canonical path、`FileID`、`FileRevision` 和 exact size；terminal proof 只有在该 digest、final identity 与 continuity token 都匹配时才可复用。没有匹配 digest 时按新的 selected plan 处理，缺失 final 可在新 operation 中重新下载，foreign replacement 或明确标识变化按 `existing-unverified` 处理。显式 `get` 本身授权补齐缺失的 selected item：active final 明确缺失且 private witness 与 content evidence 可认证时，原子 no-replace 重新发布同一对象；仅 content evidence 不足但 private object 仍精确 owned 时，也只能在 final 已明确不存在时从零重写该私有对象；只要 final 已链接到该对象或其存在状态不确定，就禁止写入，按 `existing-unverified` 或 `needs-attention` 处理。

WindShare 保证发布时的内容、大小和协议声明元数据已验证。最终文件恢复目标目录的普通权限后，其他本地进程可以正常修改它；后续显式 `get` 重新验证 final identity 与支持矩阵中的 continuity token 即可按 best-effort 计为 `previously-published`。token 缺失或明确变化时不覆盖并返回 partial；不要求为低频重跑增加排他锁或整文件重哈希。

## 2. 能力边界

能力事实必须正交：

1. **DestinationSafety（硬要求）**：root-confined/no-follow 路径、原子 no-replace、完整后可见、当前操作所需权限和 identity revalidation。
2. **PublicationObjectSafety（硬要求）**：每个文件只分配一个 publication object，内容直接写入该对象，final 必须是同一 native object。发布前它由 anonymous/delete-on-close handle 或 private ownership anchor 隐藏和保护；仅有可预测名称不合格。
3. **ActivePublicationRecovery（可选）**：以 working receipt、private witness、可选的 content commitment 和 prepared/committed phase 恢复 active operation，不包含 verified ranges；已公开 final 以 same-object 和支持矩阵中的 continuity token 作为恢复证据。没有 commitment 时不能执行依赖内容证据的 missing-final adoption，但不影响安全首次下载。
4. **RestartRangeRecovery（可选）**：operation、directory、file verified ranges 可跨进程重开；它依赖可重开的 publication object，但仍是独立 runtime fact。它不要求 terminal commitment。
5. **TerminalPublicationProof（可选派生能力）**：以 `ReceiveRequestKey` 查找 bounded terminal record，并用 terminal `SelectionPlanDigest`、final identity 和 continuity token 判断现有 final 是否仍为该发布对象；content commitment 只是 token 不足或 missing-final adoption 时的可选加强证据。没有该 proof 时已有 final 按未验证 collision 处理。它不单独增加普通下载的 hash 或整文件读取，也不参与 primary settlement 或 cleanup authority。

Cleanup failure boundary 和 persistent-object reopen 是平台分别证明的 composition prerequisites，不是合并 capability 或 profile 枚举。CLI 只有在 ActivePublicationRecovery 同时可用时才启用并宣称 RestartRangeRecovery；孤立的 range evidence 不构成安全的同目录续传模式。

“Crash-clean”只覆盖平台已证明的 failure boundary。Delete-on-close 默认只证明 process termination；power-loss cleanup 必须由独立 crash-cut 或 durable private anchor 证明。

`transfer.DirectTreeCapabilities.Durability` 继续作为 verified-range durability 的唯一 runtime fact：

- `DurabilityProcessRestart` 或更强：允许 durable checkpoint 和自动 reopen；
- `DurabilityNone`：不产生 durable verified ranges，但可独立拥有 cleanup evidence、ActivePublicationRecovery 或 metadata-only TerminalPublicationProof；missing-final adoption 仍要求接收期间已产生 commitment。

PublicationObjectSafety、ActivePublicationRecovery 与 TerminalPublicationProof 都是平台内部能力，不增加 wire 枚举。平台向 runtime 返回不可变的 typed `OutputCapabilityReport`，至少独立表达 DestinationSafety、PublicationObjectSafety、cleanup failure boundary、ActivePublicationRecovery、RestartRangeRecovery 和 TerminalPublicationProof；CLI warning 与运行模式只由该报告决定。`DirectTreeCapabilities.Durability` 仍只表示 verified-range durability，不得代表其他能力。TerminalPublicationProof 默认只使用 bounded identity/continuity/plan evidence；若接收期间显式启用 commitment，低频显式重跑可在 token 缺失时按需读取 final 做 best-effort 校验。普通 ordinary profile 不计算 commitment，也不因 terminal proof 探测增加整文件扫描。无法建立 range recovery repository 不代表不能保存 bounded publication evidence；若在声明的 failure boundary 内既不能创建 crash-clean anonymous object，也不能建立可精确清理的 private ownership anchor，全局安全 admission 必须失败。禁止用完整内容 copy 伪造 fallback。

`OutputCapabilityReport` 的 admission 规则固定为：DestinationSafety 或 PublicationObjectSafety 缺失则在首个内容请求前失败；其余能力只决定 warning、checkpoint 和重跑路径。报告同时携带每项能力的 failure boundary 与 typed failure reason；runtime 不从 `DurabilityNone` 推导其他能力，也不因探测到可选能力而自动启用 commitment。

运行模式在首个内容请求前冻结，包括 cleanup boundary、ActivePublicationRecovery 和 RestartRangeRecovery。选定后任一已声明语义能力的运行时失败必须 pause/fail，不能中途静默降级。TerminalPublicationProof 在 primary settlement 后独立 best-effort 提交，不进入冻结模式；失败时丢弃不完整记录并报告 future-rerun impact，不改变本次结果。Continuity fast path 使用支持矩阵内的 identity/metadata token；token 不确定且接收期间有 commitment 时，低频显式重跑可选择一次 best-effort 内容验证，否则沿用 `existing-unverified` 分类。

### 2.1 两级 admission

首个内容请求前完成全局 admission：

- 打开并绑定 destination root；
- 证明 no-follow、no-replace 和 publication engine 基础能力；
- 证明 single-object allocation、private/anonymous anchoring、namespace-only publication 和 cleanup failure boundary；
- 分别尝试建立 ActivePublicationRecovery 与 RestartRangeRecovery 并冻结语义能力组合；TerminalPublicationProof store 只做非阻塞探测；
- ActivePublicationRecovery 缺失时输出强 warning；仅缺 RestartRangeRecovery 时输出弱 warning；TerminalPublicationProof 按实际影响延迟说明。

增量发现后，在每个目录或文件首次 mutation/content request 前完成 item admission：

- 重新绑定 parent、name 和 root lineage；
- 验证该项实际需要的 create/write/metadata/cleanup 权限；
- 分类 collision、permission denied 和 unsupported publication。

计划不要求先物化整棵树。后发现的 item failure 不回滚已发布文件，也不阻止独立兄弟项。

Directory settlement 区分 `created-directory`、`traversed-existing-directory` 和 `subtree-failed`。新目录在后代完成后应用协议声明的目录 metadata；已有目录永不被认领或重放 metadata。空目录不依赖 file settlement 即可成功。

## 3. 所有权与持久状态

Public destination 由用户管理，允许 inherited ACL、group write、named ACL 和其他可修改主体。WindShare 不修改 public root 权限，也不宣称独占。

Private recovery hierarchy 使用独立认证，并为每个 destination 从 restart-stable `RecoveryRootBinding` 派生 opaque 控制目录名：

```text
<public-root>/                         user-owned
└── .windshare-output-<control-id>/   exact-owned control directory
    └── checkpoints-v3/
        └── operations/<operation>/
            ├── operation
            ├── directories/
            ├── files/
            ├── publications/
            └── objects/
```

动态控制目录使用固定前缀，以及由 `RecoveryRootBinding` 和 bounded attempt ordinal 派生、不能从 capability 或 sender content 推导的 `control-id`。Receiver 直接尝试候选名，不扫描整个 destination；exact foreign collision 只推进下一候选。目录以原子 no-replace 创建，并复核 version-neutral marker、root binding、private policy 和 native identity；foreign 动态前缀项始终是普通用户内容，legacy tombstone 除外。Receiver 的 discovery/output reducer 只排除能够重新认证为 WindShare-owned 的控制目录，永不遍历或修改自有控制目录来承接同名内容；Cleaner 只删除 exact-owned 的空控制层级。

每层都验证 certification、operation binding、marker 和 exact native identity；名称本身不产生所有权。

可恢复 operation 至少记录：

- `OperationID`、`ReceiveIntentDigest`、materialization binding；
- operation-independent `ReceiveRequestKey`；
- destination `AuthorityRef` 与独立的 restart-stable `RecoveryRootBinding`；
- directory path、catalog DirectoryID/generation、parent admission 和 public directory identity；
- file revision、path、size、checkpoint generation、ranges、可选的 bounded commitment accumulator state 和 publication object identity；
- publication phase、private anchor witness 和 committed receipt。

operation terminal receipt 先独立提交 settlement 并授权 cleanup。随后，可用的独立 `TerminalProofStore` 从该 receipt 或等价的 immutable in-memory settlement 生成 terminal provenance；它只保留 `ReceiveRequestKey`、terminal `SelectionPlanDigest`、canonical path/file revision/geometry binding、final identity、可选 content commitment、continuity token 与 committed settlement，不保留内容 anchor 或 verified ranges。该提交失败不回滚 primary settlement、不阻止 cleanup；proof 可能在 cleanup 后才可见，期间发生 crash 时下一次显式 `get` 允许重新下载，但仍不得覆盖或认领 foreign object。不完整记录不可见。记录按有界数量/时间回收，并且不出现在 `resume list`。Terminal proof 是低频复用提示，不是比 `FileCheckpointV2` 更强的内容真相。

`AuthorityRef` 只表达 destination reservation。`RecoveryRootBinding` 由独立 `RecoveryCertificationID` 产生并绑定该 `AuthorityRef`；live-only destination identity 不能认证 recovery repository。

`ReceiveRequestKey` 复用现有 compatible-operation key 的稳定投影：对 `SelectionSpec`、`ArtifactSpec`、`PlanDirectTree`/authority kind 标签和 `RecoveryRootBinding` 做 versioned domain-separated digest。禁止散列包含随机身份的 `MaterializationPlan` 或 `DestinationReservation` canonical bytes；key 排除 `OperationID`、reservation/session/job ID、connectivity policy、relay 地址和 control-directory attempt ordinal。`SelectionPlanDigest` 只在 terminal catalog discovery 后生成，使用 versioned、domain-separated digest，按 canonical path 顺序绑定本次冻结选择的目录 generation 以及每个选中项的 `FileID`、`FileRevision` 和 exact size；只保存摘要，不保存整份清单。Active operation lookup 与 terminal proof lookup 使用独立索引，但同一 request-key lease 必须从 active lookup 一直持有到 terminal discovery、proof lookup 和 new-operation install 完成；lease 由 OS handle 释放，不用超时猜测 stale owner。lease 冲突直接返回 `recovery lease contended`，不得并行创建第二个 operation。terminal proof lookup 必须等待当前 plan digest，digest 不匹配时不得复用 proof。

最终文件和目录提交后是 user-owned。Cleaner 只删除 exact-owned private objects，永不删除公开对象。

新路径只读写动态控制目录下的 `checkpoints-v3`。不迁移、不双读、不新增 v2 observer；已存在的旧固定 `.windshare-output` 及已知 legacy bootstrap/probe 名称作为不可遍历 tombstone，exact sender collision 只失败该项或子树，其他路径继续。普通输出不读取、修改或删除这些旧状态，它们也不阻止动态 v3 control allocation。

## 4. Single-object publication transaction

“Stage”只表示 publication object 尚未公开的生命周期阶段，不表示第二个文件。平台必须让同一 native object 同时满足 private receive、restart witness 和 final permission semantics。

平台 `PublicationEngine` transaction 负责产生“当前调用者在 final parent 正常创建同类文件”的权限结果。Generic runtime 不计算或重放 ACL/DACL/umask。

事务顺序：

1. 绑定并复核 root、final parent 和 reserved name。
2. 需要 persistent anchor/evidence 时，先选择唯一 object ID/name，持久化并复核 `AllocationPlanned`，再创建任何可跨进程存活的对象。
3. 分配唯一 `PublicationObject`，赋予 final-equivalent permission profile，并通过 anonymous handle 或 private anchor 隐藏；持久对象完成 sync/reopen 后写入 `ObjectAnchored`。纯 live-only anonymous object 不写 allocation record。
4. 所有内容块直接写入该 object；只有完整认证的 protocol block（末块使用 exact length）才能推进 verified ranges；启用 commitment 时同步推进 Merkle frontier。checkpoint 以同一 generation 原子绑定 object identity、ranges 与 bounded frontier/window；声明 power-loss durability 时，内容 durability barrier 必须先于 record promotion。不保存 per-block leaf 列表，也不做额外整文件读取。
5. 完成内容认证、长度/metadata 检查和所需 sync；启用 commitment 时同时完成 content commitment，支持 active publication evidence 时写入 `PublicationPrepared`。
6. 将 private anchor 作为 `FinalPublicationWitness`，并在 final-name mutation 前重新验证 permission evidence。Parent policy 已变化时，只能在对象仍私有时原位修正并复核，或 pause/fail；不得分配新 object。
7. 以原子 no-replace link 将同一 object 公开为 final name并保留 witness；无 persistent evidence 的 live-only composition 可使用原子 no-replace link/rename。`PublicationPrepared` 是 direct-tree 的可重放 cut：后续显式 `get` 只有在 final 明确 absent、witness 与 content commitment/continuity evidence 仍匹配且取得 fresh mutation authority 时，才可重试同一对象的 no-replace publication；证据不足时，只有 final 仍明确不存在且对象仍为私有时才可从零重写；final 已存在或存在状态不确定时禁止写入。随后从 final parent 重新打开并复核 same-object、size/metadata continuity token 和 parent binding；不要求完整内容重读，低频显式重跑可在 token 缺失且接收期间已有 commitment 时选择一次 best-effort 内容校验。
8. 完成 file/parent sync；支持 active publication evidence 时写入包含 final identity、content commitment 和 continuity evidence 的 `PublicationCommitted` receipt。
9. 有 durable working evidence 时，只有 operation terminal receipt 已持久化包含该文件 settlement 后，才按 cleanup transition 退休 working receipt 和 private anchor；live-only operation 在 primary settlement 后关闭 anonymous witness。Terminal provenance 在独立事务中生成，失败不阻止上述 cleanup。Active、resumable 和 `needs-attention` operation 保留完整 witness。

核心不变量：

- receive object、checkpoint object、witness object 和 final object 必须是同一 native object；
- publication path 不得读取或重写内容字节；
- 普通首次接收和未公开对象的 active resume 不得增加整文件扫描；恢复 crash 前已公开的 final 时，优先使用 continuity 证据，低频显式重跑才按需读取该 final，不增加首次接收的内容 I/O；
- 任何可跨进程存活的 named object 必须先有已同步的 `AllocationPlanned`；
- 任何仍可驱动 final adoption 的 durable receipt 都必须有可重新认证的 private anchor witness；
- inventory/background recovery 永不补建 public final；显式 `get` 在 final 明确 absent、private witness 与 content commitment/continuity evidence 精确匹配且 fresh authority 通过时，可对 `PublicationPrepared`/`PublicationCommitted` 的同一对象重试原子 no-replace publication；没有 commitment 时不做依赖内容证据的 missing-final adoption，只有在 final 明确 absent 且对象仍私有时才可从零重写；final 已存在或状态不确定时禁止写入；
- cleanup 先持久化 retiring transition，再移除 working receipt、移除 anchor，最后完成 transition；terminal provenance 不持有内容对象。
- active 或 terminal same-object observation 与支持矩阵内未变化的 continuity token 即可产生 best-effort 复用；它不是并发修改的原子内容快照，也不声称当前字节绝对未变。同 token 原地改写是已接受残余，不要求额外整文件重哈希；对象替换、rename 或 token 明确变化仍必须 fail closed。

`DurabilityNone` 不写 verified-range checkpoint。若相应 evidence 可用，仍可保存 allocation、cleanup、active receipt 或 terminal proof；若不可用，则 anonymous/delete-on-close object 在已认证 failure boundary 内必须 crash-clean，下一次 `get` 只能把无法证明 final identity/continuity 的对象视为 `existing-unverified`。

Commitment 只在显式选择的 recovery profile 中启用，并受固定 CPU、内存和持久化预算约束；ordinary profile 不计算、不提交 frontier，也不增加 fsync。Terminal proof 超出记录数量、大小或提交时间预算时丢弃未完成记录，只影响 future-rerun reuse，不改变 primary settlement、exit code 或 cleanup。

若平台无法在不复制内容的前提下同时满足 final permission profile、private pre-publication 和 no-replace，结构性缺失在全局 admission 失败；某个 parent 的具体 profile 无法表达时，该 item 在首个内容请求前失败。

## 5. Recovery reducer

Platform 只产生带 identity evidence 的 observation；operation、directory、file 和 publication reducer 决定动作。

| Observation | Decision |
| --- | --- |
| `AllocationPlanned` 且 object absent | 按已记录 ID/name 创建同一 allocation；不得生成第二个 identity |
| `AllocationPlanned` 且 exact object 已存在 | 重新认证并推进 `ObjectAnchored` |
| persistent object 存在但缺少可认证 allocation record | 不认领、不猜测清理；缩小到该 operation/item 的 `needs-attention` |
| operation、directory、publication object、checkpoint 全部匹配 | 从 verified ranges 继续 |
| ActivePublicationRecovery 可用、RestartRangeRecovery 不可用，unfinished private object 明确仍为私有且 final 明确 absent | 原子失效未完成 ranges/commitment，从零覆盖写入同一 object；不得分配第二个仍存活对象 |
| ActivePublicationRecovery 可用、RestartRangeRecovery 不可用，但 final 已存在或存在状态不确定 | 禁止写入该 object；同一对象且 token 可认证则复用，否则按 `existing-unverified` 或 `needs-attention` 处理 |
| publication 尚未开始，private object 明确缺失且 final absent | 先持久化 file invalidation，再分配新 object；保留其他文件 |
| owned directory 明确缺失、名称仍 absent 且无 descendant publication ambiguity | 失效旧 directory record 后重建 |
| 名称为 exact foreign 普通目录 | 作为 unowned traversal container 进入；不认领、不删除、不重放源目录元数据，继续处理后代 |
| 文件名或目录位置为 foreign 非目录对象 | 记录 `existing-unverified`；跳过该项或子树，继续其他项 |
| committed receipt、final witness 与 final same-object，且 continuity token 未变化 | 按 best-effort 返回 published；仅在 terminal receipt 已包含该 settlement 时幂等完成 cleanup |
| committed receipt、witness 与 final same-object，但 continuity token 缺失或不确定 | 默认 `existing-unverified`；接收期间有 commitment 时，低频显式重跑可做一次 best-effort 内容校验，仍无法稳定验证时不覆盖 |
| prepared/committed receipt、witness 与 content evidence 匹配，但 final 明确缺失 | inventory 只报告；显式 `get` 取得 fresh authority 后原子 no-replace 重新发布同一对象并复核，记为 `previously-published` |
| terminal proof、request key、`SelectionPlanDigest`、file binding、final identity 均匹配，且 continuity token 未变化 | 按 best-effort 快速返回 `previously-published`；不重开旧 operation |
| terminal proof 与 plan/final identity 匹配，但 continuity token 缺失或不确定 | 默认 `existing-unverified`；接收期间有 commitment 时，仅在该次显式重跑中可做一次 best-effort 内容校验，匹配才复用，否则不覆盖 |
| terminal provenance 存在但 final absent | 显式 `get` 创建新 operation 并重新下载缺失文件 |
| terminal provenance 与现有 final identity 不同 | `existing-unverified`；不覆盖 |
| publication 可能发生，但 final/witness 无法确定 | `needs-attention` |
| ownership evidence 冲突，继续或清理需要猜测 | `needs-attention` |
| repository 缺失或不可认证 | 普通 `get` 仅在 PublicationObjectSafety 可用时创建 live-only operation；只按实际缺失能力的提示策略说明，不修改旧状态 |

`needs-attention` 只表示 active state 缺少安全 mutation/adoption/cleanup authority，或其 final identity/ownership 无法重新认证；continuity token 变化或内容不匹配归 `existing-unverified`，同标识原地修改不触发该状态。它不是权限不足、能力不支持或 collision 的兜底。CLI 必须给出可执行的 inventory/discard 下一步。

Inventory 是只读 observation，不要求 create authority。真正 reopen/discard 时再取得 mutation authority，并复核 lease、root 和 operation generation。

## 6. 平台要求

### Windows / local NTFS

- Public root 允许 inherited DACL 和 unrelated SID；只验证当前 token 对本次操作所需的实际权限。
- Recovery repository 单独使用受保护的最小 DACL，并从 handle 复核 owner、DACL、reparse state 和 identity。
- Hard link 不触发 final parent ACL inheritance；平台事务必须在对象仍私有时产生并复核 native-create-equivalent security descriptor，再把同一 object 原子发布。禁止 full-file copy fallback。
- Retained handles/delete-sharing 只证明其有效期间的 namespace transition；每次 publication、checkpoint 和 cleanup 仍重新验证 identity。

### Linux / ext4

- group-writable、setgid、access/default ACL 不是自动拒绝理由。
- Publication object 优先在 final parent 使用 `O_TMPFILE` 取得 native inheritance，再建立 private anchor；无特权实现必须证明受控 `/proc/self/fd` link 或不暴露内容的 same-inode fallback，不能假设 `AT_EMPTY_PATH` capability。禁止 full-file copy。
- Private object 在安装到 control namespace 前完成 owner/mode/ACL/setgid normalization，不出现先公开宽权限再收紧的窗口。
- Directory fd 不阻止 rename；checkpoint、publication 和 cleanup 前重新证明 root lineage、parent 和 entry identity。

Phase A 必须先以真实 NTFS/ext4 fixture 证明上述 permission profile、single-object identity、no-replace 和 crash cuts；证明结果形成平台 capability certificate。无法在普通用户权限下证明的文件系统在内容请求前返回 PublicationObjectSafety unsupported，不在 runtime 中加入复制或 ACL 重放 fallback。

publication 前，能够绕过 OS 权限修改 private object 的主动恶意本地进程不在威胁模型内。Final 一旦公开，任何本地修改都是预期输入；same-object 与支持矩阵内的 identity/metadata continuity token 用于低频恢复和重跑，但不是原子内容快照。同 token 原地改写属于已接受残余，不要求额外整文件重读；replacement、rename 和 token drift 仍必须 fail closed。Public ACL drift 使缓存 permission evidence 失效并触发 item re-admission；只有权限丢失或 final-equivalent profile 无法重新证明时才失败。Private recovery ACL/owner/marker drift 必须 fail closed。

## 7. Core 重构

Phase A 之前不冻结最终接口形状。删除职责过载的 `outputcap.Platform` 时保留深事务边界：

- `DestinationAuthority`：root/item admission、handle-relative lookup/create 和 identity binding；
- `PublicationEngine`：平台内部完成 allocation、final-equivalent permissions、anonymous/private anchor、same-object publication 和每个 native crash cut，并返回具体 transaction struct；
- `RecoveryRepository`：动态 control ownership、request-key/operation lease、checkpoint、working receipt 和 operation terminal receipt；
- `TerminalProofStore`：独立消费 committed settlement，管理 request-key index 与 bounded terminal provenance；其失败不能反向影响 `RecoveryRepository` cleanup；

`outputruntime` 只负责编排 capability selection、纯 observation/reducer 和 lifecycle，不跨接口搬运可伪造的平台 permission/object evidence。Syscall、ACL、anchor 顺序与 native identity 留在平台 transaction。禁止增加 `AllowPermissiveRoot`、`DisableResume` 等绕过参数。

稳定 decision/fault reason 保持正交：

- destination permission denied；
- PublicationObjectSafety unsupported；
- ActivePublicationRecovery unavailable；
- RestartRangeRecovery unavailable；
- terminal publication proof unavailable；
- recovery lease contended；
- collision；
- recovery needs-attention；
- content integrity failure；
- publication failure。

PublicationObjectSafety 可用时，ActivePublicationRecovery、RestartRangeRecovery 或 TerminalPublicationProof 缺失都不阻止本次安全下载：ActivePublicationRecovery 缺失输出强 warning，仅缺 RestartRangeRecovery 时输出弱 warning；TerminalPublicationProof 缺失或提交失败不改变 primary result，只按实际 future-rerun impact 说明。

控制流只使用 typed result/fault，不解析错误文本。Network/session failure 不得被 output cleanup 的 cancellation 覆盖。

## 8. 实施顺序

### Phase A：先证明主路径平台算法

- Windows Downloads-like DACL 与 Linux group/setgid/default ACL fixture 完成真实嵌套文件接收。
- Windows 证明 hard-link 前的 native-create-equivalent DACL；Linux 证明普通用户可用的 `O_TMPFILE` anchoring 或 same-inode fallback。
- 证明 `AllocationPlanned` 起始的 single-object allocation、private anchoring、atomic no-replace 和关键 crash cut；publication 期间不得发生内容 I/O。
- 对显式启用 commitment 的 profile，证明首次接收和未公开对象的 active resume 只写内容一次且没有独立 hash pass；bounded accumulator 在 100 GiB/最小 block geometry 下不按 block 数量增长。ordinary profile 必须证明无 commitment、无额外整文件读取和无额外 publication copy。恢复已公开 final 优先使用 continuity 标识；低频重跑的内容校验仅作为可选 best-effort 路径。
- 证明动态 control direct lookup 不扫描 destination，且不受合法同名/同前缀内容或旧固定 control 目录影响。
- 平台证据确定接口形状后再冻结 core contracts。

### Phase B：重建 ordinary-output core

- 提炼 `DestinationAuthority`、deep `PublicationEngine` transaction 和 `RecoveryRepository` 的 consumer-side contracts。
- 将 compatible-operation key 收敛为稳定 `ReceiveRequestKey`，增加可选 bounded content commitment accumulator、allocation/operation/directory/file/publication/terminal receipt 与纯 reducer。
- 在 core result model 中固定 created/traversed/subtree-failed directory settlement、downloaded/previously-published/existing-unverified file outcome，以及 capability/fault 到 CLI exit 的 typed 映射；CLI 不通过解析错误文本或重新推断状态。
- 分离 `AuthorityRef`、DestinationSafety/PublicationObjectSafety certification 和 `RecoveryRootBinding`。
- 分离 active publication evidence 与 range durability，冻结两者组合并落实对应 warning。
- 固定 request-key lease 下 active-first；无 active 时完成 terminal discovery 生成 `SelectionPlanDigest`，再 terminal-proof-second、new-operation-last。
- 让 directory settlement 独立于 file settlement，空目录和已有目录 traversal 可直接完成。

### Phase C：实现主路径平台与 repository

- 实现 Windows/NTFS 和 Linux/ext4 composition。
- 在动态 control 目录中建立 `checkpoints-v3`、allocation/directory recovery、active-without-ranges 恢复（仅限 final 明确 absent 的私有对象）、file invalidation、single-object witness、operation terminal receipt 和 publication cleanup。
- 删除 v2 active create/resume path，不增加兼容层。
- 同步重建 `resume list/discard` 的 v3 inventory，只读取可认证的动态 control namespace。

### Phase D：独立实现 terminal 幂等重跑

- 基于接收期间已有 settlement 的 `ReceiveRequestKey` 增加 `TerminalProofStore`、bounded terminal provenance 与独立回收策略；没有 commitment 的普通 operation 仍可生成 metadata-only terminal proof，`outputruntime` 的 primary settlement 和 cleanup 不依赖它。
- 从 committed settlement 独立提交 terminal proof；失败或 crash 只留下不可见记录，不回滚当前结果。
- terminal provenance 写入受有界记录数、记录大小和提交时间预算约束；预算耗尽直接放弃本次 proof，不阻塞首次下载。
- 无 compatible active operation 时，显式 terminal 重跑在当前 terminal discovery 后匹配 `SelectionPlanDigest`、final identity 和 continuity token；标识不足且接收期间有 commitment 时，可读取 pinned final 做 best-effort 校验。没有匹配 proof 时按 `existing-unverified` collision 处理。同一对象被替换或 identity/metadata 标识明确变化时不得误报成功；同标识原地改写属于已接受残余。

### Phase E：收口 CLI 与文档

- 普通 `get` 实现两级 warning-once、collision continuation，以及 downloaded、previously-published、created-directory、traversed-existing-directory、existing-unverified 和 failure summary。
- discovery terminal 后，所有选中文件均 downloaded/previously-published、所有选中目录均 created/traversed 且没有 unverified/failure 才返回成功；空目录不依赖文件结果。其余 partial、resumable 或 needs-attention 均非零并给出下一步。
- 仅在低频 active-publication 或 terminal fallback 读取已有 final 时显示验证进度；除此之外，首次接收和未公开对象的 active resume 不增加阶段或提示。
- 默认错误只说明影响和下一步；底层平台事实保留在结构化 trace。
- 更新 get/resume、威胁模型和 native output 文档，删除 public root 必须独占的旧说明。
- 删除旧 `Platform` 聚合和重复 admission-only tests。
- 按双模块发布顺序交付：先发布并验证 core candidate，再更新根模块的 released core 版本；完成 `make root-release-graph` 后才合并根 CLI 变更。

## 9. Observability 与验证

在 destination admitted、active/range capabilities frozen、lease acquired、allocation planned/anchored、directory/file admitted、checkpoint promoted、publication prepared/committed/replayed、active content proof decided、cleanup decided 和 terminal proof attempted 时发结构化事件。事件携带现有 redacted operation/session/file correlation ID 与 decision code；不记录 `ReceiveRequestKey`、content commitment 或 path。本计划不新增全命令 `--verbose`/`--trace` flags。

验证集中在：

- capability selection、两级 warning-once、request-key 并发与稳定投影、active/range 冻结和 terminal store 故障隔离；
- reducers 覆盖空目录、existing-directory traversal、active-without-ranges、anchor retirement、active final 的 continuity/fallback/mismatch、missing-final 显式重发布和 terminal rerun；
- Windows/Linux permission profile、same-object no-replace、动态/legacy control collision 和 crash cuts；
- 内容 I/O 证明高频路径无额外整文件读取；启用 commitment 时，100 GiB/最小 block geometry 下 checkpoint/provenance 不保存线性 leaf 列表；
- 进程级多文件/嵌套目录、collision、重启、live-only 重跑和 terminal proof 写入失败；
- focused `make <gate>`/`make check`，完成后运行 `make ci`。
