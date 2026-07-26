# Filesystem output ownership and resume design

状态：产品与架构方向已确认，待实现与验证。范围仅包含 native filesystem receiver；browser output 使用独立设计。

## Product contract

核心承诺：中断后可继续；WindShare 永不自动覆盖或删除用户文件。

| 场景 | 用户体验 | 恢复状态 |
|---|---|---|
| 正常接收 | 文件完整且 metadata 已落盘后才出现在 final 路径 | session 成功后清理 |
| Ctrl+C、正常停止 | 显示已暂停；重跑相同 link 与 output root 自动继续 | 保留最近 verified checkpoint |
| 网络重试耗尽 | 返回原始网络错误，并提示进度已保留 | 保留最近 verified checkpoint |
| crash、强制结束、断电 | 下次运行验证 state 与 witness；有效文件继续 | 仅信任验证通过的 checkpoint |
| final 路径已存在 | 不覆盖；该文件报告 collision，其他文件继续 | 若已有完整 stage，则保留供重试 publish |
| 单文件恢复状态不可信 | 不触碰现有 final；该文件显示“需要处理”，其他文件继续 | 仅该文件 quarantine |
| 不支持的 filesystem | 在接收任何文件数据前失败并说明原因 | 不创建文件进度 |
| 用户 Discard | 先显示 session、状态目录和占用空间；确认后仅删除恢复状态 | 不删除 final 或用户目录 |

Pause 只承诺最后一次完成 data sync 并安装 state record 的 checkpoint。未 checkpoint 的缓存字节可以重传。

已确认的产品决策：

- 单文件 collision 或 quarantine 不终止其他文件；最终摘要报告成功与待处理文件；
- 当前 recoverable backend 对未知、网络、FUSE、云占位和未认证 filesystem fail closed；以后如有需要，另做显式 non-recoverable backend；
- 当前版本不检测同一文件对象的原地内容修改；
- 恢复状态不自动过期，必须提供 `resume list` 与显式 `resume discard`；
- 第二次中断直接退出，等价于 crash cut。

## Scope and threat boundary

持久化的 `device + inode` 或 Windows File ID 不能证明删除后的新对象仍是 WindShare 创建的对象，因为编号可能复用。新设计使用：

- 随机 `OutputObjectID` 分配内部 namespace；
- 持久 hard-link anchor 把该 ID 绑定到仍存活的文件对象；
- POSIX `SameFile`、Windows File ID 等只比较当前打开的对象；
- anchor 存在期间对象不会被删除，因此其 inode/File ID 不会被复用给新对象。

设计覆盖：

- process restart 和已认证 filesystem 上声明支持的 power-loss recovery；
- stage、anchor、final 在两次运行之间的缺失、rename 和替换；
- inode/File ID reuse；
- 已定义 crash cut 上的 state/filesystem 不一致；
- 非恶意外部程序造成的路径冲突。

设计不覆盖：

- 同一用户恶意并发篡改 state、anchor、journal 或目录项；
- 通过任一 hard link 对同一对象进行原地写入；
- 谎报 durability 的 filesystem、bit rot 或存储硬件故障。

state checksum 不是 MAC。若需要覆盖本地恶意篡改，必须使用 keyed state 与独立内容完整性设计。

## Invariants

- durable ranges 只有在有效 file state、有效 anchor 和匹配的打开对象同时存在时才可信；
- anchor 必须先于任何非空 durable ranges 持久化；
- state 仍可能引用对象时，不得提前移除 anchor；
- ownership 验证失败只 quarantine 受影响文件，且不得删除或覆盖 final；
- publish 必须 atomic no-replace，并从 anchor 发布；
- final parent、state、stage 与 anchor 必须在同一认证 filesystem，路径不得穿过未认证 nested mount；
- 用户中断、正常 shutdown 和网络失败只能 Pause，不能清理可恢复状态；
- 只有显式 Discard 可以删除 recoverable 或 quarantined state；已确认永久失效且 ownership 有效的内部对象可以安全 retire；
- successful completion 不留下 recovery object；固定 control directory 与 coordinator lock 可以保留；
- 不预读或预哈希发送端内容。

## Private state store

所有新状态位于 output root 下的保留目录。若该目录已存在但不符合严格 schema、类型、权限或 no-follow 要求，必须失败且不得修改它。

```text
.windshare-output/
├── coordinator.lock                         # stable; never unlinked
└── sessions/<session-id>/
    ├── header.state
    ├── session.lock
    ├── files/<shard>/<locator-digest>.state
    ├── anchors/<shard>/<object-id>.anchor
    └── stages/<shard>/<object-id>.stage
```

- POSIX control/session directories 使用 `0700`；Windows 使用 Hidden 属性和不继承非必要主体的受保护 ACL；
- locator digest 只负责高效查找；record 内必须保存并验证 canonical final locator；
- `OutputObjectID` 使用 CSPRNG，并通过 exclusive-create state/anchor names 保证 session 内唯一；
- header 与每个 file record 独立 atomic replace，checkpoint 不得重写全 session；
- 创建或删除 internal directory entry 后必须 sync 对应 parent，后续 state 才能引用该 entry；
- record、目录深度和单 record 大小有明确上限；object、file 与 stage 目录均分片；
- session 目录必须通过 no-follow 打开并固定为 directory handle；内部操作只相对该 handle 执行。

### Lock and namespace authority

每次 discovery、open、create 或 namespace removal 都先获取固定的 `coordinator.lock`。该文件不删除，避免 lock-file inode ABA。

打开 session 时，在 coordinator lock 内：

1. no-follow 打开并固定 session directory；
2. 验证目录名、header、root binding、share、resume intent 和 session ID；
3. 获取 `session.lock`；
4. 释放 coordinator lock，继续使用已固定的 session handle。

Finish 或 Discard 删除 session namespace 时，必须在仍持有 session lock 的情况下重新获取 coordinator lock。关闭并删除 session lock、移除 session directory name 的整个过程由 coordinator lock 保护。删除目录名前再次比较 parent entry 与固定 handle；不匹配则停止并报告 unsafe state。

自动 cleanup 只处理有效 record 声明的内部名字。未知目录项导致 session 保留并报告，不进行递归猜测。显式 Discard 授权删除用户确认的固定 session directory object，但仍必须 no-follow、no-cross-mount，且永不包含 final path。

## Durable state machines

### Per-file phase

```text
reserved -> witnessed -> publishing -> published -> retiring
                       publishing -> publishBlocked
publishBlocked -> publishing

reserved | witnessed | publishing | publishBlocked | published -> quarantined
reserved | witnessed | publishBlocked | published -> retiring
```

- `reserved`：file record 已持久化 object ID 与 locators，内部对象可能尚未创建；
- `witnessed`：stage 与 anchor 已创建、同步并验证为同一普通文件，可以记录 durable ranges；
- `publishing`：完整 ranges 与 metadata 已同步，允许执行 no-replace link；
- `publishBlocked`：link 返回 collision；完整 stage/anchor 保留，其他文件继续；
- `published`：final 已验证为同一对象，anchor 继续保留；
- `retiring`：已持久化删除意图，只允许幂等清理内部名字；
- `quarantined`：该文件不再自动 resume、publish 或 cleanup，等待显式 Discard。

`publishBlocked` 与 `quarantined` 必须分开：前者是可恢复的普通 collision，后者表示 ownership 无法证明。

### Session lifecycle

```text
active -> pausing -> paused -> active
active -> completing -> closed
active -> completing -> paused      # retained blocked/quarantined files
active | paused -> discarding -> closed
```

`closed` 不写入 state；进入该状态后 session namespace 已删除。per-file quarantine 不改变 session lifecycle。header 无法验证时，整个 namespace classified as unsafe，OpenOrCreate 不得静默创建同 intent 的新 session。

## Core output contract

删除语义过载的 `FileTransaction.Abort` 与 `OutputSession.AbortJob`。核心合同按调用意图拆分：

- file transaction：`Commit`、`Pause`、`Retire`；
- output session：`PauseJob`、`CompleteJob`；
- output authority：`OpenOrCreate`、`ListResumeState`、`DiscardResumeState`。

`DiscardResumeState` 不属于 transfer job，普通传输错误没有调用它的权限。

事件映射固定如下：

| 事件 | file/session action |
|---|---|
| 用户中断、正常 shutdown、session/network failure | active files `Pause`，随后 `PauseJob` |
| 永久且隔离的单文件失败，ownership 有效 | 该文件 `Retire`，其他文件继续 |
| publish collision | file 进入 `publishBlocked`，其他文件继续 |
| 单文件 ownership 失败 | file 进入 `quarantined`，其他文件继续 |
| header、root 或 namespace authority 失败 | 停止整个 session，报告 unsafe state |
| 文件枚举结束 | `CompleteJob`：无 retained state 则 closed，否则 paused/needs-attention |
| 用户明确放弃恢复状态 | authority 执行 `DiscardResumeState` |

所有结果必须使用 typed settlement，调用方不得解析错误字符串来决定清理。`CompleteJob` 返回 `closed` 或 `paused-needs-attention`；后者不是 fatal output error。

## File protocol

### Begin

1. 验证 final parent 位于已认证 filesystem，且路径未跨 nested mount；
2. 持久化 `reserved` file record；
3. exclusive-create stage，设置 exact size，sync file，再 sync stage parent；
4. 创建 stage 到 anchor 的 hard link，再 sync anchor parent；
5. 打开 stage 与 anchor，验证它们是相同普通文件、size 正确并与当前 volume 匹配；
6. 持久化 `witnessed`。只有此后才能写入非空 durable ranges。

### Checkpoint

1. 写入 pending ranges；
2. sync 打开的文件；
3. 再次验证打开对象与 anchor 匹配；
4. atomic replace 单个 file record，安装递增 generation 与 merged ranges。

内存 durable state 只能在 record reopen/verify 成功后前进。

### Publish

1. 验证 ranges 完整，通过打开的对象 handle 设置全部 file metadata，再 sync 文件；
2. 验证 stage、anchor 与打开对象相同；
3. 持久化 `publishing`；
4. 从 anchor atomic no-replace link 到 final；
5. 若 final 已存在，持久化 `publishBlocked` 并返回 typed collision；
6. link 成功后 sync final parent；
7. 验证 final、anchor 与原打开对象相同且 metadata 正确，持久化 `published`；
8. 删除 stage 并 sync stage parent；anchor 保留到 session completion。

无法在 link 前完成 file metadata 的 filesystem/backend 不满足本合同。任一验证失败都不得删除 final。

### Recovery matrix

| Phase | Observed state | Action |
|---|---|---|
| `reserved` | stage 与 anchor 都不存在 | 重试 Begin object creation |
| `reserved` | stage 与 anchor 存在且匹配 | 验证、sync 后进入 `witnessed` |
| `reserved` | 只有一个存在或两者不匹配 | quarantine |
| `witnessed` | stage/anchor 匹配，final 不存在 | resume durable ranges |
| `witnessed` | final/anchor 匹配且 ranges 完整 | 采纳 publish crash cut |
| `witnessed` | final 存在但不匹配 | quarantine |
| `publishing` | final 不存在 | 重试 no-replace publish |
| `publishing` | final/anchor 匹配且 ranges 完整 | 完成 metadata 后进入 `published` |
| `publishing` | final 存在但不匹配 | quarantine |
| `publishBlocked` | 不匹配的 final 仍存在 | 保持 blocked，报告 collision |
| `publishBlocked` | final 已移走 | 不重传内容，重试 publish |
| `published` | final/anchor 匹配 | completed |
| `published` | final 缺失或不匹配 | quarantine；不复活或覆盖 final |
| `retiring` | 预期内部名字缺失 | 视为上次删除已完成，继续 cleanup |
| `retiring` | 内部名字存在但不匹配 | quarantine；保留该名字 |
| `quarantined` | 任意状态 | 不自动 resume、publish 或 cleanup |

除 `retiring` 外，anchor 缺失或不匹配都使 durable ranges 失去可信度。final 在 Begin 前已存在且没有 file record 时，只报告普通 collision，不创建恢复状态。

## Pause, completion, and discard

### Pause

1. 停止接收新 frame 和启动新 file transaction；
2. 使用独立 settle context 调用 active file `Pause`；每个文件要么安装 data-sync-first checkpoint，要么保留上一个 checkpoint；
3. 持久化 `paused`，关闭 handles，释放 session lock；
4. 不进入 `retiring`，不删除 stage、anchor 或 state。

`fsync` 不能被 Go context 可靠取消。settle deadline 只停止新工作；第二次信号直接退出，按最近一次 durable state 恢复。

### Complete

- 没有 blocked/quarantined file 时，header 进入 `completing`；
- 每个 published 或已确认永久失败的 record 先进入 `retiring`，再删除并 sync stage/anchor，最后删除 record；
- `retiring` 中缺失的内部名字是幂等成功；存在但 identity 不匹配则停止并 quarantine；
- 所有 file record 删除后，header 最后删除；session lock 与 directory name 在 coordinator lock 下移除；
- final 和传输期间创建的用户目录永不由 completion 删除；
- 仍有 blocked/quarantined file 时，session 转为 `paused` 并返回 needs-attention summary。

### Discard

- 只能由明确用户操作触发；
- 操作前显示 session、固定状态目录和 no-follow 统计的占用空间；
- 获取 coordinator/session locks，固定并重新验证 directory object；
- header 有效时先持久化 `discarding`；删除其他内部项并 sync，header 最后删除；
- header 损坏时，用户确认授权的是当前固定 directory object；路径发生替换则中止；
- 永不遍历或删除 final locator、用户可见目录或 state directory 外的路径。

不实现基于文件名、年龄、inode 或旧 journal 的自动 GC。

## Filesystem support

recoverable backend 必须同时具备：

- regular-file hard links 和 atomic no-replace link；
- 当前对象的 same-file comparison；
- atomic state replacement，以及与声明 durability level 对应的 file/directory sync；
- root、control state 和每个 final parent 的同 filesystem/no-cross-mount 验证；
- 经过真实 process-restart 或 power-loss fault test 的明确 allowlist。

API feature probe 只能证明调用可用，不能证明 durability。未知、网络、FUSE 和云占位 filesystem 默认拒绝。probe 必须在接收数据前运行并清理自身临时项。未来的 non-recoverable backend 必须由用户显式选择，不能静默降级。

job admission 在 selected catalog 确定后、请求第一段 file content 前验证所有 selected final parents；`Begin` 再次验证当前 parent，防止 admission 后的 mount/path 变化。

## Content integrity boundary

hard-link witness 证明对象连续性，不证明字节未被原地修改。同一对象经 final 或其他 hard link 被等长改写时，identity、size 与 witness 都可能保持有效。

当前合同只承诺 object ownership 与 crash consistency，不承诺检测本地原地内容修改。本里程碑不加入 block digest。若未来需要该能力，应在接收认证数据时计算 per-block digest，并在恢复时验证 durable blocks；无需发送端预读或预哈希。

## Schema and migration

- 新设计使用 state schema v3：小型 session header 加独立 per-file records；
- v2 的 `device + inode` / File ID 不迁移为可信 witness；
- v2 journal 只 quarantine，不依据旧 identity 自动删除 stage 或 final；无法证明 ownership 的旧 stage 需用户手工处理；
- v3 namespace 与旧扁平 namespace 分离；
- checksum 检测损坏但不授予 ownership；
- structured trace 记录稳定 session/object ID、前后 lifecycle/phase、触发事件、settlement、collision 与 quarantine reason。

## Validation gates

### State-machine and fault injection

- reserved record、stage create/sync、anchor link/sync、witness install、每次 checkpoint、publishing、final link、final-dir sync、published install、retiring 和 namespace removal 的每个 crash cut；
- 任一错误后内存状态不得领先于 durable record；
- cancellation/network failure 在 file 与 job 两层都只能 Pause；
- collision 保留完整 object，quarantine 只影响一个文件；
- `retiring` cleanup 幂等，Discard 只能来自 explicit authority；
- corrupted header 阻止同 intent 自动创建新 session。

### Namespace and concurrency

- session directory rename/replacement、unknown entry 和 nested mount fail closed；
- lock-file inode ABA、两个 OpenOrCreate、OpenOrCreate 与 Finish/Discard 并发；
- cleanup 前后 parent entry 与固定 directory handle 不匹配时不删除；
- checkpoint 不重写其他 file records；大量文件与并发 checkpoint 保持 bounded record size 和近似 O(checkpoints) 写放大。

### Real filesystem and end-to-end

- 强制 inode/File ID reuse，旧 ranges 不得被新对象采用；
- stage、anchor、final 的缺失与替换逐项覆盖 recovery matrix；
- publish collision 后移走 final 并重跑，不重新接收内容；
- Ctrl+C、第二次中断、网络重试耗尽、crash 和 power-loss fault 后恢复最近 checkpoint；
- 单文件 quarantine 后其他文件继续并正确汇总；
- unsupported filesystem 和 nested mount 在该文件数据接收前失败；
- Linux、macOS、Windows allowlist 上的真实 filesystem 测试；未知 filesystem 拒绝；
- Complete 与 Discard 后不遗留 recovery object，固定 coordinator metadata 除外。

## References

- `core/transfer/output.go`
- `core/transfer/job.go`
- `core/osfs/output_session.go`
- `core/osfs/output_authority.go`
- `core/osfs/output_journal.go`
- `core/osfs/pathlimit_other.go`
- `core/osfs/pathlimit_windows.go`
- POSIX `link(2)` and `unlink(2)`
- Microsoft `FILE_ID_INFO`
