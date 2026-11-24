# Web FSA 安全改名实时提示与按需文件名还原执行计划

## 1. 背景与问题分析

### 1.1 问题起因

在支持 File System Access API（FSA）的桌面浏览器（Chrome / Edge）中，WindShare 默认优先推荐“保存到文件夹”（[contract.ts](../../web/src/output/backends/contract.ts#L183-L185)）。

当接收方通过浏览器直接下载**软件文件夹/工具包**（含 `.dll`、`.exe`、`.bat`、`.cfg`、`.lnk` 等）或**代码库/开发环境**（含 `pyvenv.cfg`、`.cfg`、`.sh`、`.bat`、`.dll`、`.local` 等）时，FSA 会因两类原因拒绝原始文件名：

1. **浏览器安全策略拦截（真实中高频痛点）**：Chromium FSA 禁止网页直接写入部分“可执行/高危/Shell 集成”扩展名（例如 `.dll`、`.exe`、`.bat`、`pyvenv.cfg`、`.cfg`、`.lnk`、`.local`、`.scf`、`.url` 等），即使这些文件名在目标操作系统上完全合法。下载数 GB 软件目录后若几十个 `.dll`/`.cfg` 被改名，软件将直接无法运行，而改用 OPFS ZIP 会触发双倍磁盘占用甚至超出浏览器配额。
2. **操作系统/文件系统限制（低频）**：Windows 保留设备名（`CON`、`NUL`、`PRN`、`COM1` 等）、非法字符（`< > : " | ? *`）、末尾点/空格、大小写不敏感引发的类型/同名冲突（`TypeMismatchError`），或单段路径超过 255 字节。

> **前置实测确认（动工前必查）**：当前 [fsa-reservation.ts](../../web/src/output/backends/fsa-reservation.ts#L29-L31) 仅在 `getFileHandle()` 抛出 `TypeError` / `TypeMismatchError` 时触发安全改名。实施前须在真实 Windows Chrome / Edge 上对 `.dll` / `.exe` / `.bat` / `.cfg` / `.lnk` 实测 `getFileHandle()`、`createWritable()`、`writer.close()`（Safe Browsing `AfterWriteCheck`）三阶段的实际拦截点与错误类型：
> - 若 `createWritable()` 抛 `SecurityError`，现行 [fault.ts](../../web/src/transfer/fault.ts#L188-L191) 会将其判为 `PermissionDenied` 导致整任务暂停而非单文件安全改名；
> - 若 `writer.close()` 阶段拦截，Chromium 会删除 `.crswap` 且现行 [fsa-file.ts](../../web/src/output/backends/fsa-file.ts#L16) 仅允许 `<= 128 KiB` 重试，大文件需提前按扩展名预判或配合 `file-transfer.ts` 回退重传。

在 commit `2f7a2696` 中，项目移除了约 1.5 万行的旧版实时 sidecar 还原引擎，改为无状态确定性安全重命名（[safe-name.ts](../../web/src/output/backends/safe-name.ts)）。这一重构大幅降低了架构复杂度，但当前实现仍有 3 个明确缺口：

1. **下载中途无感知**：[execution-state.ts](../../web/src/application/receive/execution-state.ts#L355-L363) 仅在任务 `settle`（完成/暂停/停止）时才将 `safeRenamed` 同步到 UI。用户下载大目录时，哪怕前 5 秒就发生改名，也必须等全部下完才看到提示，错失及时切换 ZIP 或 `wind get` 的止损时机。
2. **续传覆盖、崩溃恢复丢失与事实源漂移**：[fsa.ts](../../web/src/output/backends/fsa.ts#L207) 在 `reopenFSASession` 时创建空的 `SafeNameRegistry`，而 `settle()` 使用 `this.#safeNames.summary() ?? this.#record.safeRenamed`（[fsa.ts](../../web/src/output/backends/fsa.ts#L369)），续传产生新改名时会覆盖旧记录；崩溃恢复路径 [pauseFromCheckpoints](../../web/src/application/receive/retained.ts#L416-L433) 未从 `files` 表重建 `safeRenamed`；同时内存 `SafeNameRegistry` 在打开文件/目录时即记录，若文件失败或停止后被 [cleanupFSATaskEntries](../../web/src/output/backends/fsa-cleanup.ts#L97-L138) 清理，会留下“幽灵计数”。
3. **大目录大量改名后缺少低成本、可信的补救路径**：当前详情页最多展示 20 条样本（[state.ts](../../web/src/transfer/state.ts#L43)），仅提示“改用 ZIP 重新下载”。若用户已通过 FSA 下完数 GB 软件/代码目录且有数十个 `.dll`/`.cfg` 被改名，既看不到完整清单，也缺乏安全直观的一步还原路径。

---

## 2. 方案选型与跨平台可行性分析

### 2.1 为什么不恢复旧版 1.5 万行引擎

旧方案的复杂度主要来自**“下载中途实时往磁盘增量写 `.data` sidecar + 双 IndexedDB 账本 + 崩溃追赶同步状态机 + 续传状态耦合 + 强行调用 Win32 `MoveFileExW` 还原 `CON` 等设备名”**。该设计投入产出比极低，且强行还原 `CON` 等保留名会产生资源管理器无法打开/删除的畸形文件。

### 2.2 为什么不采用“默认自带 `.txt` 改名双击”或“`.txt` + Base64 + `scriptblock::Create`”

| 方案 | 核心问题 |
|---|---|
| **默认自动写 `.txt` 让用户改名双击** | 1. 污染所有普通下载目录，并重新耦合暂停/崩溃/清理状态机。<br>2. Windows 默认隐藏已知扩展名，改名易成 `.cmd.txt`；`cmd` 默认 ANSI 易乱码且极易受 `%`/`!`/`&`/`^` 注入。<br>3. macOS / Linux 浏览器写出的文件无 `+x` 权限，双击不可执行。 |
| **写 `.ps1` 降级 `.txt` + Base64 + `[scriptblock]::Create` / `-ExecutionPolicy Bypass`** | 1. Chromium FSA 的危险扩展名策略在 Windows 上常态拦截 `.ps1`，导致高频落入 `.txt` 动态执行分支。<br>2. “读取 `.txt` → `scriptblock::Create` + Base64 解码执行”与 ClickFix 无文件恶意软件特征高度同构，极易触发 Defender (AMSI) / 杀软 / EDR 拦截，并严重损害用户对开源 E2EE 工具的信任。<br>3. Base64 让用户和开发者无法肉眼审计命令内容。 |

### 2.3 选定方案：实时提示 + 终态按需还原（数据与执行分离、100% 明文可审计）

设计原则：**普通无改名下载 0 感知/0 残留/0 性能开销；不新建 IndexedDB 表、不破坏 `paused` 续传与崩溃恢复持久化不变量、不使用 Base64 或 Safe Browsing 绕过 hack，核心逻辑收口在独立纯函数与深模块中（保持各文件与目录满足 `make sloc` 和 `pnpm check:boundaries` 限制）。**

1. **下载中实时提示**：首次发生安全改名即刻以精炼中性文案推送到任务卡片，不打断下载，详情可查看实时样本（便于用户尽早决定继续、换 ZIP 或改用 `wind get`）。
2. **单一事实源，跨续传与崩溃无损**：
   - 下载进行中由内存 `SafeNameRegistry` 驱动实时提示（续传时同时导入既有已完成和 `receiving` 记录，确保续传期间实时计数不断层）；
   - 在 `settle()` 中**保持先 `putTask` 持久化状态、再执行 `cleanupFSATaskEntries`**（守住崩溃恢复不变量），其中 `failed` 直接不持久化改名字段，`saved` / `paused` / `partial` 统一基于已提交 `files` 表 + 根条目 + 保留空改名目录计算；仅当 `partial` 清理实际裁剪了空改名目录时补写一次 `putTask`；崩溃恢复 `pauseFromCheckpoints` 路径复用同一纯函数，确保卡片计数、详情清单与还原步骤完全一致，消除续传覆盖、崩溃丢失与幽灵计数。
3. **仅终态（`saved` / `partial`）按需还原（支持 Windows / macOS / Linux）**：
   - **严格限定在终态（`saved` / `partial`）**：`downloading` / `preparing` / `paused` 均不提供磁盘改名还原，彻底守住 `paused` 任务的续传校验（`#checkCompleted`）、渐进发现与未提交 0 字节占位清理（`discardFSATask`）不变量，避免制造“还原后点击继续下载直接炸冲突”的非法状态。`paused` 详情仅展示摘要，提示“完成或停止下载后可在此还原文件名”。
   - **少量文件（≤ 10 项且内联命令长度 ≤ 2000 字符）**：优先提供「复制还原命令」（自动匹配当前系统，极低频的跨系统切换弱化为次级入口），粘贴到终端执行，目录 0 残留，**100% 明文可读、零 Base64**；同时详情清单每项支持一键「复制原名」，方便极少量文件（1~3 项）时直接手动改名。
   - **任意数量（尤其是 > 10 项或路径过长）**：点击时才按需写入单个文件到目标目录并给出单行透明执行命令：
     - **Windows（数据与执行分离）**：往目标目录写入**纯数据清单 `windshare-restore-names.json`**（UTF-8；`.json` 永远不被 Chromium FSA / Safe Browsing 拦截，无降级 hack，本身即可直接阅读并可供未来原生 CLI `wind` 复用），配合一段**无 Base64、无 `scriptblock::Create`、无 `-ExecutionPolicy Bypass`**、兼容 Windows PowerShell 5.1 的透明标准 PowerShell 命令（基于 `Get-Content -LiteralPath ... -Raw -Encoding UTF8 | ConvertFrom-Json`、`$PWD.ProviderPath`、`[IO.Path]::Combine` 与 `Rename-Item -LiteralPath` 执行）。
     - **macOS / Linux**：写入 100% 明文可读的 POSIX `sh` 脚本 `windshare-restore-names.sh`（基于 `$(dirname -- "$0")` 定位基准目录，零 Base64；仅在浏览器因名称/安全策略拒绝 `.sh` 时，先 `writer.abort()` 并删除本次新建的 0 字节 `.sh` 占位，再降级 `.txt`），命令固定为标准 `sh ./windshare-restore-names.sh`（或 `.txt`）——**无需 `+x` 权限、无需改后缀**。同时支持通过浏览器下载导出纯数据 `windshare-restore-names.json` 留档/供自定义脚本消费。
   - **OS 非法名与冲突边界**：只还原目标系统合法且无同级冲突的文件名；Windows 侧对 OS 本身非法名（如 `:`、`CON`、`NUL`、末尾点/空格）以及同级大小写折叠撞名（如文件 `FOO` 与目录 `foo`）明确归类为“当前系统无法还原原名，已保留安全名”；POSIX 侧仅按 `isCatalogName` + 255 字节上限判合法性，不做静态大小写拒绝（避免误杀 Linux 及大小写敏感 APFS 上的合法文件，交由脚本运行期 `[ -e "$dst" ] || [ -L "$dst" ]` 不覆盖检查安全兜底）。

---

## 3. 架构与数据模型设计

### 3.1 数据源与持久化（零 Schema 升级 + 单一事实源）

现有 `windshare-receive-v1`（[database.ts](../../web/src/output/store/database.ts#L8-L16)）已有 `tasks` 和 `files` 两个对象仓库，且每个已提交的 FSA `ReceiveFileRecord` 已保存 `path` 和 `fsaPath`（[fsa-file.ts](../../web/src/output/backends/fsa-file.ts#L345)）。

唯一不在 `files` 表中的安全改名条目有两类：
1. **根条目改名**（顶级文件夹或单文件本身被拒写）：可从 `ReceiveTaskRecord` 现有的 `artifact.name`、`target.collisionIndex`、`target.reservedName` 精确推导，无需冗余存储。在 `SafeNameRegistry` 与 `collectSafeRenamedFromRecords` 中须将**根条目**与**根下子条目**按不同作用域区分，避免根目录与同名子目录（如 `CON/CON`）在键空间碰撞。
2. **被改名的空目录**（通过 `ensureDirectory` 创建、最终保留在磁盘上且其下无文件提交）：在 `ReceiveTaskRecord` 上新增可选字段（JSON 向后兼容，不升版本号）：
   ```ts
   readonly safeRenamedDirectories?: readonly SafeRenamedDirectoryRecord[]
   // { logicalPath: readonly string[]; physicalPath: readonly string[] }
   ```
   常规下载为 `undefined`。

**单一事实源结算流程（保崩溃恢复持久化顺序）**：
- `settle()` 先以已提交 `files`（仅 `status === 'completed'`）+ 根条目 + 候选改名目录调用统一纯函数 `collectSafeRenamedFromRecords(task, files, candidateDirs)`（若 `nextLifecycle.kind === 'failed'` 则不写入 `safeRenamed` 与 `safeRenamedDirectories`），并**先执行 `await this.#store.putTask(nextRecord)`**（守住 [fsa-recovery.test.ts](../../web/test/output/fsa-recovery.test.ts#L334-L355) 的“先落库再清理磁盘”不变量）。
- 若 `nextLifecycle.kind === 'failed' || nextLifecycle.kind === 'partial'`，随后执行 `cleanupFSATaskEntries`（针对 `partial` 清理未完成文件及空 `#fileDirectories` / 被退回文件父目录）；清理完成后核对候选空改名目录是否被裁剪（含上一轮创建、本轮被 `removeUnfinishedFile` 顺带裁剪的空目录），**仅当 `safeRenamed` 或 `safeRenamedDirectories` 实际变化时**再做一次 `putTask` 修正并返回最终记录。
- 崩溃恢复路径 `pauseFromCheckpoints`（[retained.ts](../../web/src/application/receive/retained.ts#L416-L433)）复用同一纯函数，从已提交 `files` + 根条目重建 `record.safeRenamed`，确保崩溃重启后正常露出“已调整的文件名”。
- 已知边界：若下载中途崩溃且用户此后直接 `stop`/`discard`，仅可能遗漏崩溃前创建的纯空改名目录；点击续传则会由目录发现重新补全。

### 3.2 还原计划纯函数模块与依赖边界

遵守仓库依赖边界（[policies.mjs](../../web/scripts/dependency-boundaries/policies.mjs#L7) / [policies.mjs](../../web/scripts/dependency-boundaries/policies.mjs#L60-L68)：`ui/**` 仅可导入 `output/model/**` 或经由 `application/**` 转发，不可直引 `output/names/**`）：
- **`web/src/output/model/safe-name-restore.ts`**：放置纯类型契约（`RestorePlatform`、`SafeNameRestoreStep`、`SafeNameUnsupportedEntry`、`SafeNameRestorePlan`、`SafeNameRestoreExportResult` 等，无任何私有 `output` 依赖）。
- **`web/src/output/names/restore-plan.ts`**：无 I/O、易单测的纯函数模块，由 `output/backends/fsa-restore.ts` 和 `application/receive/` 消费。

核心语义：

1. **输入与准入**：
   - `task`: `Pick<ReceiveTaskRecord, 'artifact' | 'target' | 'lifecycle' | 'safeRenamedDirectories'>`（要求 `target.kind === 'fsa'` 且 `lifecycle.kind === 'saved' || lifecycle.kind === 'partial'`；非终态直接返回空计划或拒绝）。
   - `files`: `readonly Pick<ReceiveFileRecord, 'path' | 'fsaPath' | 'status'>[]`（仅使用 `status === 'completed'` 且 `fsaPath?.length === path.length` 的已提交文件）。
   - `platform`: `'windows' | 'posix'`。
2. **推导原子重命名步骤（去重 + 由深到浅 + 单文件/根目录区分）**：
   - 按目标形态区分工作目录、精准目标目录名（`targetFolderLabel`）与路径前缀（对照 [fsa.ts](../../web/src/output/backends/fsa.ts#L114) 与 [fsa.ts](../../web/src/output/backends/fsa.ts#L425-L442)）：
     - **单文件下载（`target.singleFile === true`）**：`workingDirectory = 'parent-folder'`，`targetFolderLabel = target.parentHandle.name`。此时 `rootHandle` 即 `parentHandle`，`file.fsaPath` 已是 `[target.reservedName]`，**不可**再前缀 `target.reservedName`，也**不可**走目录树遍历逻辑或追加目录步骤；仅当该单文件已 `completed` 且 `target.reservedName !== decideCollisionName(artifact.name, 'file', target.collisionIndex)` 时，生成 1 条根文件重命名。
     - **目录下载且根目录本身被改名**（`target.reservedName !== decideCollisionName(artifact.name, 'directory', target.collisionIndex)`）：`workingDirectory = 'parent-folder'`，`targetFolderLabel = target.parentHandle.name`（避免 Windows 锁定当前工作目录导致根目录无法改名），所有子项统一前缀物理根 `[target.reservedName]` 与逻辑根 `[logicalRootName]`，并将根目录作为最后一步。
     - **目录下载且根目录未改名**：`workingDirectory = 'inside-result'`，`targetFolderLabel = target.reservedName`（含同名避让后缀，如 `my-repo (1)`），所有路径相对结果根目录。
   - 遍历每个 `status === 'completed'` 文件的 `path` 与 `fsaPath`（以及 `safeRenamedDirectories`），逐段对比逻辑段与物理段，若第 `i` 段不同则产生一条原子重命名步骤（携带 `parentPhysicalPath`、`parentLogicalPath`、`fromName`、`toName`、`logicalPath`、`kind`），按 `parentPhysicalPath.length` **从深到浅**排序。
3. **目标平台合法性与同级冲突分类**：
   - 任何段不满足 `isCatalogName` 直接拒绝，绝不进入计划。
   - `platform === 'windows'`：使用 [isPortableOutputName](../../web/src/output/names/policy.ts#L25-L31) 判断 `toName`；不通过归入 `unsupported`（原因：`invalid-for-os`）。
   - `platform === 'posix'`：使用 `isCatalogName(toName) && fitsPortableOutputNameBudget(toName)` 判断。
   - **同级大小写冲突检测（仅 Windows 静态归类）**：在 `platform === 'windows'` 下，同一逻辑父目录内若另一已完成文件或目录的逻辑名与 `toName` 在大小写折叠下冲突且字面不同（例如 Linux 分享中同时存在文件 `FOO` 与目录 `foo`），归入 `unsupported`（原因：`case-collision`），避免生成注定报冲突的步骤。`platform === 'posix'` 不做静态大小写拒绝（避免误杀 Linux 及大小写敏感 APFS 上因扩展名拦截改名的合法兄弟项 `Foo.cfg`/`foo.cfg`，交由脚本运行期 `[ -e "$dst" ] || [ -L "$dst" ]` 不覆盖检查自然兜底）。
   - 若某父目录被判为 `unsupported`，其子文件仍以该父目录的物理名正常还原，互不阻塞。
4. **明文命令与清单/脚本生成（单一契约 + 防注入 + 嵌套容错 + 真幂等 + 绝不覆盖）**：
   - **单一执行器契约**：各平台只维护一套统一的重命名生成逻辑与安全/四态契约（不拆“简单/复杂”双模板），避免安全转义与测试矩阵分叉；较长命令的视觉克制由 UI 折叠展示与单条「复制原名」消化。
   - **安全防线（零 Base64、100% 明文可审计）**：
     - **Windows (PowerShell，兼容 PowerShell 5.1 & 7)**：
       - 全链路使用 `$PWD.ProviderPath`、`[IO.Path]::Combine(...)`（避免 PS 5.1 下 `Join-Path` 不支持 3+ 参数）与 `-LiteralPath`（彻底兼容含 `[` `]` 的目录名，如 Next.js `[id]`）；
       - 少量内联命令（`restorable.length <= 10` 且总长度 `<= 2000` 字符）：采用单行明文 JSON 字面量传给 `ConvertFrom-Json`，序列化时将 PowerShell 识别的全部 5 种单引号定界符（`'`、`U+2018`、`U+2019`、`U+201A`、`U+201B`）以及 `U+2028` / `U+2029` 转义为标准 JSON `\uXXXX`（其余 ASCII、中文、`.dll`/`.cfg` 全部保持人类明文可读），置于单引号字符串中，**数学上不可能闭合字符串逃逸，零 Base64**；
       - 大批量导出：往目标目录写入纯数据 `windshare-restore-names.json`（UTF-8），给出明文读取该 JSON 并逐项 `Rename-Item -LiteralPath` 的标准 PowerShell 命令，**零 `scriptblock::Create`、零 `-ExecutionPolicy Bypass`**。
     - **POSIX (`macOS / Linux sh`)**：
       - 严格单引号转义（`'` → `'\''`）加 `--` 终止选项（防 `-rf.cfg` 类参数注入）；脚本基于 `$(dirname -- "$0")` 定位基准目录，100% 明文可读。
   - **嵌套目录容错与真幂等**：
     - **限定目录类型的逐级动态父目录解析**：处理每条步骤前，沿 `parentPhysicalPath` / `parentLogicalPath` 逐级向下解析，且**严格校验目标为目录**（Windows: `Test-Path -LiteralPath $p -PathType Container`；POSIX: `! [ -L "$p" ] && [ -d "$p" ]`），避免被同名文件或符号链接误判。某级仅存在其一目录即进入；两者同为目录则判冲突；皆不存在则判缺失。
     - **仅对真实 I/O 重命名失败阻断祖先改名**：子项因文件占用/杀毒锁发生重命名异常时将其祖先物理路径加入 `blockedAncestors` 跳过外层改名；目标已存在冲突不阻断外层目录。
   - **四态语义（绝不覆盖任何文件）**：
     - 源存在 & 目标不存在 → 执行重命名（Windows: `Rename-Item -LiteralPath`；POSIX: 先 `[ -e "$dst" ] || [ -L "$dst" ]` 判存防覆盖悬空符号链接，再 `mv --`）
     - 源不存在 & 目标存在 → 视为已还原（幂等跳过）
     - 源存在 & 目标存在 → 跳过并输出冲突提示，**绝不覆盖**
     - 源不存在 & 目标不存在 → 输出缺失提示
   - **执行结果摘要、开错目录提示与清理提示**：
     - 找不到本地清单文件（Windows `.json`），或全部条目缺失（`X = 0 && Y = 0 && W > 0`）时，明确提示“请确认当前终端目录是否为 `<targetFolderLabel>`”；
     - 末尾打印 `已还原 X，已跳过(已还原) Y，冲突 Z，缺失 W，失败 F`；当 `Z = W = F = 0` 且使用了本地清单/脚本文件时，提示“全部文件名已还原，可安全删除该文件”（不做自动自删）。

---

## 4. 详细实施步骤

### Phase 0：前置实测（Windows Chrome / Edge）

0. 在真实 Windows Chrome / Edge 浏览器中对包含 `.dll`、`.exe`、`.bat`、`.cfg`、`.lnk` 的测试目录执行一次 FSA 写入，分别记录 `getFileHandle()`、`createWritable()`、`writer.close()`（`AfterWriteCheck`）三阶段的真实拦截结果与异常名：
   - 若 `createWritable()` 抛 `SecurityError`，须将名称/策略拒绝与目录权限拒绝区分开（避免被 [fault.ts](../../web/src/transfer/fault.ts#L188-L191) 判为 `PermissionDenied` 导致整任务暂停）；
   - 若 `.dll`/`.exe` 存在 `writer.close()` 阶段拒绝，结合 [fsa-file.ts](../../web/src/output/backends/fsa-file.ts#L16) 现有 `128 KiB` 重试上限评估提前预判或 `file-transfer.ts` 回退重传方案，确认后再推进后续 Phase。

### Phase 1：修复续传/崩溃丢失、对齐单一事实源并打通实时推送

> **SLOC 与目录结构约束提示**：[fsa.ts](../../web/src/output/backends/fsa.ts) 当前 `sloc-guard` 已达 651/700 行（93% warning），[coordinator.ts](../../web/src/application/receive/coordinator.ts) 为 601/700，[controller.ts](../../web/src/ui/controller.ts) 为 615/700；此外 `web/src/application/receive/` 已有 **16/20** 个代码文件（接近 18/20 warning 线）。新增逻辑优先落入 `web/src/output/names/`、`web/src/output/model/`、`web/src/output/backends/fsa-restore.ts` 等低占用目录，避免突破单文件行数或目录文件数上限。

1. **`web/src/output/backends/safe-name.ts`**
   - 为 `SafeNameRegistry` 增加构造参数 `onChange?: (summary: SafeRenamedSummary) => void`（仅在 `size` 变化时触发）及 `recordDirectory(logicalPath, physicalPath)` / `renamedDirectories()`；同时将**根条目**与**根下目录/文件**的 key 隔离（杜绝 `CON/CON` 碰撞）。
   - 抽出纯函数 `collectSafeRenamedFromRecords(task, files, renamedDirectories?)` 供 `settle()` 与 `pauseFromCheckpoints(...)` 共用：
     - 仅持久化统计 `status === 'completed'` 的文件记录与保留的改名目录，杜绝未完成/已清理文件产生幽灵计数。
     - **单文件避让边界**：当 `target.singleFile === true` 时，普通同名避让（`collisionIndex > 0`，如 `report (1).pdf`）也会导致 `file.path[0] !== file.fsaPath[0]`，因此单文件任务仅按 `target.reservedName !== decideCollisionName(artifact.name, 'file', target.collisionIndex)` 判定根条目改名（且要求单文件已完成）。
   - `SafeNameRegistry.seedFromPrior(task, priorFiles, renamedDirectories?)`：除已提交条目外，也将 `status === 'receiving'` 的物理路径导入内存 `#assigned` / `#recorded`，保证续传下载期间实时计数与 `#resolveDirectory` 查表不断层。
2. **`web/src/output/backends/fsa.ts`、`web/src/output/backends/fsa-cleanup.ts` & `web/src/output/backends/contract.ts`**
   - `reopenFSASession` 读取 `priorFiles` 后调用 `seedFromPrior(task, priorFiles, task.safeRenamedDirectories)`。
   - `#openSubdirectory` 发生安全改名时，记录完整逻辑路径与物理路径。
   - `settle()` **保持先 `putTask(nextRecord)` 再执行 `cleanupFSATaskEntries`**（`failed` 不写入 `safeRenamed` / `safeRenamedDirectories`）；`partial` 清理完成后核对候选空改名目录存活状态（覆盖跨续传被 `removeUnfinishedFile` 裁剪的空目录），仅在结果发生变化时补写一次 `putTask`。
   - 在 `OutputSessionHandle` 增加可选 `subscribeSafeRenamed?(listener: (summary: SafeRenamedSummary) => void): () => void`，不污染传输层 `TransferJob`。
3. **`web/src/application/receive/retained.ts`**
   - `pauseFromCheckpoints` 遍历 `files` 后，对 `backend === 'fsa'` 任务复用 `collectSafeRenamedFromRecords` 补全 `safeRenamed`，确保崩溃恢复的任务在刷新后正常展示“已调整的文件名”。
4. **`web/src/application/receive/execution-state.ts` & `web/src/application/receive/session-runner.ts`**
   - `WorkspaceTask` 新增 `observeSafeRenamed(summary: SafeRenamedSummary | null): boolean`（仅在 `count` 变化时返回 `true`）。
   - `ReceiveSessionRunner.run()` 在 `beginActive` 后立刻同步初始 `safeRenamedSummary()`，并订阅 `session.subscribeSafeRenamed`（与 `#observeProgress` 共用独立的 100ms `#scheduleThrottledPublish()`，避免无字节进度时漏刷，并在 `finally` 取消订阅）。

### Phase 2：实现纯函数还原规划与明文命令/清单生成模块

5. **新增 `web/src/output/model/safe-name-restore.ts`（纯契约类型）与 `web/src/output/names/restore-plan.ts`（纯逻辑）**
   - `output/model/safe-name-restore.ts` 导出：`RestorePlatform = 'windows' | 'posix'`、`SafeNameRestoreStep`、`SafeNameUnsupportedEntry`、`SafeNameRestorePlan`、`SafeNameRestoreExportResult` 等。
   - `output/names/restore-plan.ts` 导出：
     - `detectRestorePlatform(navigatorLike?): RestorePlatform`
     - `buildSafeNameRestorePlan(input): SafeNameRestorePlan`
       - 仅处理 `saved` / `partial` 终态 FSA 任务；返回 `workingDirectory: 'inside-result' | 'parent-folder'`、`targetFolderLabel`、`restorable: readonly SafeNameRestoreStep[]`、`unsupported: readonly SafeNameUnsupportedEntry[]`（含原因 `'invalid-for-os' | 'case-collision'`，其中 `'case-collision'` 仅在 Windows 侧静态判出）、`totalCount`。
     - `formatInlineRestoreCommand(plan, platform): string | null`（`restorable.length <= 10` 且总长度 `<= 2000` 字符时返回明文可读、零 Base64 命令；超出返回 `null` 引导保存清单/脚本）。
     - `renderRestoreArtifact(plan, platform)`:
       - Windows：返回纯数据 `{ suggestedName: 'windshare-restore-names.json', fallbackName: null, content: <pretty JSON>, runCommand: (filename) => <兼容 PS 5.1 的明文 PowerShell 读取 JSON 命令> }`。
       - POSIX：返回明文 `{ suggestedName: 'windshare-restore-names.sh', fallbackName: 'windshare-restore-names.txt', content: <明文 POSIX sh 脚本>, runCommand: (filename) => 'sh ./' + ... }`。
     - `serializeRestoreManifestJson(plan): string`（供任意平台可选通过浏览器下载导出纯数据对照表）。

### Phase 3：按需写入动作与并发安全的 Application 层编排

6. **新增独立模块 `web/src/output/backends/fsa-restore.ts`（不并入 `fsa.ts`）**
   - 实现 `loadFSARestorePlan(store, taskId, platform)`：读取 `getTask` + `listTaskFiles`，调用 `buildSafeNameRestorePlan`。
   - 实现 `writeFSARestoreArtifact(store, taskId, platform, locks?, diagnostics?, signal?)`：
     - **并发与锁安全**：持有 `acquireTaskLock(taskId, locks)`（禁用有竞态的 `isTaskLockedElsewhere`），确保与其他标签页的 `forget` / `discard` 严格互斥。
     - **保护浏览器瞬态用户激活（User Activation）**：先做单行 O(1) `store.getTask(taskId)`，校验 `target.kind === 'fsa'` 且处于终态（`saved` / `partial`，拒绝 `downloading` / `preparing` / `paused`）后，**立即在用户点击手势内调用 `ensureFSAPermission(target.parentHandle, true)`**；获得授权后再异步读取 `listTaskFiles(taskId)` 并构建计划，避免大目录（数万行）读表耗时导致权限弹窗因失去用户激活而抛 `SecurityError`。
     - 定位目标目录句柄（`parentHandle` 或 `parentHandle.getDirectoryHandle(target.reservedName)`）；拒绝授权时抛出 `TransferFaultError(OutputFaultCode.PermissionDenied)`（复用现有 `mapReceiverFailure` 映射），目录缺失（`NotFoundError`）时抛出可被 UI 友好映射的错误，不透出原生 `DOMException`。
     - 无覆盖分配唯一文件名（复用 `entryExists` 探测文件与目录冲突，冲突时追加 `(1)`…）：
       - Windows 直接写 `windshare-restore-names.json`（无扩展名拦截）；
       - POSIX 写 `windshare-restore-names.sh`；**仅当捕获到名称/安全策略拦截类错误（`TypeError` / `InvalidStateError` / `SecurityError` / 非己方取消的 `AbortError`）**时，先 `await writer.abort().catch(() => undefined)` 并通过 `removeEntry` 清理本次新建的 0 字节 `.sh` 占位，再降级写入 `.txt`；若是 `QuotaExceededError` / `NotAllowedError` 等真实存储或权限错误则直接抛出，绝不误降级。
     - 返回 `SafeNameRestoreExportResult`（`{ fileName, runCommand, directoryLabel }`），并记录结构化诊断事件（`emitOutputTrace(diagnostics?.trace, 'publication', { backend: 'file_system_access', task_id: taskId, operation: 'safe_name_restore_export', ... })`，复用现有 `OutputTraceEvent['eventName']` 联合类型，不含明文文件名）。
7. **`web/src/application/receive/{execution-state,contracts,coordinator}.ts` & `web/src/ui/controller.ts`**
   - **洁净并发槽类型**：不污染 `ReceiveAction`；在 `execution-state.ts` 区分 `RetainedLocalCommand`（`'save' | 'save-partial' | 'discard' | 'forget'`）与 `LocalSlotActionKind = RetainedLocalCommand | 'export-restore'`，仅将 `PendingRetainedAction['action']` 放宽为 `ReceiveAction | 'export-restore'`，即可在不改核心传输状态机与 `admission.ts` / `actions.ts` 的前提下实现同标签页导出与 `forget` / `discard` 互斥。
   - **经由 `application` 暴露给 UI（满足边界检查）**：在 `ReceiveCoordinator` / `ReceiverController` 暴露：
     - `detectRestorePlatform()`（或由 `application/receive/` 转发）；
     - `prepareSafeNameRestore(taskId, platform)`：按需读取完整 `SafeNameRestorePlan` 与内联命令；
     - `exportSafeNameRestoreArtifact(taskId, platform)`：持有本地互斥槽按需写入本地清单/脚本并返回执行命令；
     - `downloadSafeNameRestoreManifest(plan)`：复用 [coordinator.ts](../../web/src/application/receive/coordinator.ts#L282-L291) 现有 `saveLocalBlob`（传入 `application/json;charset=utf-8`）触发浏览器下载。

### Phase 4：UI 体验打磨与文案更新

8. **新增独立组件 `web/src/ui/tasks/SafeNameRestorePanel.tsx`，更新 [model.ts](../../web/src/ui/tasks/model.ts)、[present.ts](../../web/src/ui/tasks/present.ts)、[TaskView.tsx](../../web/src/ui/tasks/TaskView.tsx)、[Downloads.tsx](../../web/src/ui/downloads/Downloads.tsx)、[ReceiverTasks.tsx](../../web/src/ui/receiver-presentation/ReceiverTasks.tsx)、[TaskDownloads.tsx](../../web/src/ui/experience/TaskDownloads.tsx) 及 `web/src/ui/locales/{zh-CN,en}/{downloads,progress}.ts`**
   - **显式派生状态，避免从 `TaskStage` 反推**：在 `TaskPresentation`（[model.ts](../../web/src/ui/tasks/model.ts)）新增 `safeNameRestore: 'none' | 'active' | 'paused' | 'available'`，由 [present.ts](../../web/src/ui/tasks/present.ts) 直接基于 `facts.backend`、`facts.lifecycle.kind` 与 `facts.safeRenamed` 计算（彻底规避 `paused` 在首页/切分享/缺权限时映射为 `stage === 'needs-action'`、收尾映射为 `'finishing'` 导致的误判）；同时清理 [present.ts](../../web/src/ui/tasks/present.ts#L56-L59) `collectTaskDetails` 中重复且冲突的旧 `progress.adjustedHint` 文案。
   - **保持视图层解耦**：`TaskDetails` 通过独立可选 prop（类似 `actions: TaskViewActions`）接收 `safeNameRestoreActions`，不让 `TaskView.tsx` 直接依赖 `ReceiverController` 或 `output/names/**`。
   - **高频零干扰原则**：`safeNameRestore === 'none'` 时完全不渲染该区域、零额外查询。
   - **语义化详情分区直达**：将 `TaskCard` 的 `onDetails` 扩展为 `(invoker, section?: TaskDetailSection)`（`TaskDetailSection = 'overview' | 'adjusted-names'`），`TaskDetails` 接收 `initialSection?: TaskDetailSection`，避免引入布尔补丁参数。终态卡片（`safeNameRestore === 'available'`）提示行内提供轻量直达入口（`已调整 N 个文件名 · 还原`），点击直接打开详情并自动展开还原区。
   - **进行中（`safeNameRestore === 'active'`）**：
     - 任务卡片上实时显示单行、克制、中性提示（不标红、不打断）：`已调整 N 个文件名（完成后可还原）`。
     - 详情折叠区仅 O(1) 展示实时样本，首句先安抚“文件内容完整，浏览器仅临时调整受限文件名，下载完成后可在此还原；如需直接保留原名可改用 ZIP 下载”（不对普通浏览器收件人抛出无上下文的 CLI 术语）。
   - **暂停态（`safeNameRestore === 'paused'`）**：
     - 详情折叠区仅 O(1) 展示样本摘要，提示“继续完成或停止下载后可在此还原文件名”，**不露出磁盘改名还原入口**。
   - **终态（`safeNameRestore === 'available'`）详情页「已调整的文件名」折叠区**：
     - **展开即按需加载**：采用干净的 `idle | loading | ready | error` 状态机（带请求序号防并发竞态与 `<details open>` 首次 `toggle` 重复请求）。未展开时 0 查询（不阻塞大目录详情打开）；展开 `<details>`（或通过卡片直达展开）瞬间自动异步调用 `prepareSafeNameRestore(taskId, platform)`，取消“先看样本再点一次按钮加载”的冗余中间态。
     - **默认匹配当前系统**：自动按当前 OS 选定 `Windows` 或 `macOS · Linux`，将极低频的跨系统切换弱化为面板次级小切换，不与主操作争抢视觉焦点。
     - 若 `restorable.length > 0`：
       - **精确标出目标目录名（`plan.targetFolderLabel`）与确定可执行引导**：Windows 统一写明“打开文件夹 `xxx` → 地址栏输入 `powershell` 回车 → 粘贴运行”（Win10/Win11 通用，避免误开 CMD 或跑错目录）；macOS / Linux 写明“在文件夹 `xxx` 内打开终端粘贴运行”。
       - **单条轻路径**：清单每项提供一键「复制原名」，仅 1~3 个文件且不想开终端的用户可直接手动改名。
       - **分规模主操作 + 长命令默认折叠**：
         - `inlineCommand !== null`（`≤ 10` 个且长度 `≤ 2000` 字符）：主操作「复制还原命令」，次级操作「保存清单与命令」（Windows 写 `.json`、POSIX 写 `.sh`/`.txt`）。
         - `inlineCommand === null`（`> 10` 个或路径过长）：主操作「保存还原清单并获取命令」（Windows 写 `windshare-restore-names.json`、POSIX 写明文脚本），成功后展示生成的文件名与「复制命令」主按钮；另提供轻量「导出清单 (.json)」供留档。
         - 较长命令（尤其是 Windows 读取 JSON 的透明 PowerShell 命令）默认折叠/单行预览，突出「复制命令」主按钮，点击「查看命令」可展开 100% 明文审计，保持留白克制。
       - **操作后轻反馈（不轮询磁盘）**：复制命令或保存清单成功后，就地展示极简反馈（提示在终端运行后即可恢复原名，重复运行安全无副作用；此记录反映下载时的改名情况）。
       - 导出失败（目录已移动/删除、拒绝授权、另一标签页活跃）时就地展示友好提示。
     - 若 `unsupported.length > 0`：清晰区分“系统不支持该原名”与“存在大小写同名冲突”，列出对应条目。
9. **文档同步**
   - 精简更新 [docs/browser-downloads.md](../browser-downloads.md#L23)，补充“下载中实时提示 + 完成后按需复制明文命令或导出清单还原”的说明。

---

## 5. 验证计划

遵循项目测试原则（聚焦纯逻辑、并发契约与真实命令行为，不写脆弱 UI 结构/文案测试，不显著拖慢本地测试）：

1. **新增单元测试 `web/test/output/restore-plan.test.ts`**：
   - 嵌套改名（父目录 + 子文件 + 多层空目录）与根/子同名目录（`CON/CON`）的**由深到浅**排序、作用域不撞键及祖先物理/逻辑路径映射。
   - 工作目录判定：普通目录（`inside-result` + 精确 `targetFolderLabel`）、根目录自身改名（`parent-folder` + 最终根步骤）、单文件改名（`parent-folder` + 不重复前缀 + 无多余目录步骤），以及 `collisionIndex > 0` 使用 `decideCollisionName(...)`。
   - 生命周期准入：`paused` / `downloading` / `preparing` 拒绝产出还原计划。
   - 平台与冲突分类：Windows 上 `.dll`/`.cfg`/`.lnk`/`.local` 判 `restorable`，`CON`/`a:b`/`foo.` 判 `unsupported (invalid-for-os)`，同级大小写碰撞（文件 `FOO` 与目录 `foo`）判 `unsupported (case-collision)`；POSIX 上 `CON` 以及大小写不同兄弟项（`Foo.cfg` 与 `foo.cfg`）均判 `restorable`。
   - 防注入、PowerShell 5.1 兼容与零 Base64 契约：
     - Windows 内联命令与 JSON 清单消费命令**不含 Base64、`scriptblock::Create`、`-ExecutionPolicy Bypass`**；使用 `$PWD.ProviderPath`、`[IO.Path]::Combine` 与 `-LiteralPath`；JSON 对全部 5 种 PowerShell 单引号字符及 `U+2028`/`U+2029` 做 `\uXXXX` 转义，恶意文件名（`'`、`‘’`、`$()`、反引号、`;`、`&`、`[id]`、中文/Emoji）不可逃逸；超过 `2000` 字符上限时 `formatInlineRestoreCommand` 返回 `null`。
     - POSIX 命令/脚本使用 `! [ -L "$p" ] && [ -d "$p" ]`、`[ -e "$dst" ] || [ -L "$dst" ]`、`$(dirname -- "$0")` 与 `--`，同样不可逃逸。
   - 嵌套容错、开错目录提示与幂等：父目录解析限定目录类型、仅 I/O 失败阻断祖先、四态不覆盖检查、开错目录/清单缺失提示与摘要提示。
2. **补充现有 FSA 集成测试（`web/test/output/fsa-*.test.ts`）**：
   - 下载中首次改名（含仅建改名目录）即刻触发 `subscribeSafeRenamed`（早于 `settle`）。
   - 崩溃持久化不变量与单一事实源：`settle()` 中 `putTask` 失败时仍不提前删除磁盘文件；未完成/失败被 `cleanupFSATaskEntries` 清理的文件及被裁剪的空改名目录（含上一轮建的目录），在结算修正后不会残留在 `safeRenamed` 与 `loadFSARestorePlan` 中。
   - 跨暂停/续传：Run 1 改名 `a.cfg` + 空目录 `dir.cfg` + 未完成改名文件 `part.dll` → 暂停 → Run 2 续传期间实时计数不断层且新增改名 `b.dll` → 完成后 `loadFSARestorePlan` 完整包含全部已完成项，无覆盖；验证单文件 `collisionIndex > 0` 续传不被误判为安全改名。
   - 崩溃恢复：`pauseFromCheckpoints` 能从 `files` 重建 `safeRenamed`。
   - `writeFSARestoreArtifact`：持有 `acquireTaskLock`；`ensureFSAPermission` 在 `listTaskFiles` 之前调用（保用户激活）；`paused` / `downloading` 拒绝导出；Windows 写 `.json` 无覆盖；POSIX 在名称/安全策略拒绝 `.sh` 时先 `writer.abort()` 并 `removeEntry` 清理 0 字节 `.sh` 占位再降级 `.txt`，而磁盘满/权限错误直接透出不误降级。
3. **真实 Shell / PowerShell 端到端行为验证**：
   - 在临时目录用本机真实 `powershell`（5.1）/ `pwsh` 与 `sh` 执行 `renderRestoreArtifact` 和 `formatInlineRestoreCommand` 产出的实际输出，验证嵌套目录、含 `[id]` 方括号与中文/空格/特殊引号文件名、大小写碰撞不误伤兄弟项、开错目录给出明确提示、目标已存在绝不覆盖、以及二次重跑幂等零报错。
4. **验证命令**：
   - 聚焦运行：`pnpm --dir web test`（覆盖新旧 output/receive 测试）。
   - 收尾门禁：`make web`（含 `pnpm check:boundaries` 依赖边界检查）与 `make sloc`（确保 `fsa.ts`、`coordinator.ts`、`controller.ts` 及 `application/receive/` 均不超限）。

---

## 6. 明确的非目标（防止复杂度回流）

- ❌ 不在下载过程中自动往用户目录写入任何脚本或清单文件。
- ❌ 不新增 IndexedDB Object Store，不升级数据库版本。
- ❌ 不在 `downloading` / `preparing` / `paused` 状态下提供磁盘文件名还原，绝不破坏 `paused` 续传校验与清理闭环。
- ❌ 不使用 Base64 载荷、不使用 `.txt` + `[scriptblock]::Create`、不使用 `-ExecutionPolicy Bypass` 绕过浏览器安全策略。
- ❌ 不维护“简单/复杂”双套命令生成模板（保持唯一命令执行契约与安全测试面，长命令通过 UI 折叠预览消化视觉负担）。
- ❌ 不在还原后主动轮询或二次扫描本地磁盘回写任务状态（不为此引入额外权限弹窗与持久化状态机）。
- ❌ 不做脚本/清单自动删除自身（仅在全部成功时打印可安全手动删除的提示）。
- ❌ 不在 Windows 上通过 `\\?\` / P/Invoke `MoveFileExW` 强行还原 `CON`、`NUL`、末尾点等 OS 保留名。
