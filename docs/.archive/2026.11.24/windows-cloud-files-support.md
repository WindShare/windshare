# Windows 已本地水合云文件（OneDrive / Cloud Files）分享支持执行计划

## 1. 背景与决策分析

### 1.1 问题来源

BUG-03 排查揭示了 Windows 分享路径上的两层拦截：

- **第一层（已修复，`39ba1a59`）**：Rust 激活层 [provenance.rs](../../desktop/crates/windshare-activation/src/windows/provenance.rs) 原先写死 6 个 Known Folder 白名单、限制 `DRIVE_FIXED` 并逐级拒绝祖先重解析点，导致 Windows Share Target 误杀 `D:\`、`E:\`、项目目录、U 盘与 junction 祖先路径。现已重构为“非持久化属性检查 + 暂存目录黑名单 + 独立重开校验”，并刻意将重解析点/符号链接准入边界统一归口给 Go `osfs`。
- **第二层（本计划解决）**：Go 文件系统层 `osfs` 当前在 Windows 上**一刀切拒绝所有 `FILE_ATTRIBUTE_REPARSE_POINT`**。由于 Windows 11 的 OneDrive Files On-Demand（以及 iCloud Drive、新版 Dropbox 等基于 `cldflt.sys` Cloud Files API 的同步盘）会给**已下载到本地硬盘的（已水合，hydrated）**目录与文件持续保留 `FILE_ATTRIBUTE_REPARSE_POINT` + `IO_REPARSE_TAG_CLOUD_*` 标签，导致 **Windows 全部 5 个分享入口**（Explorer 右键、App 内选文件、拖拽、CLI `wind share`、系统共享面板）全部拒绝这些合法本地文件。

### 1.2 高频 vs 低频权衡与体验评估

| 维度 | 现状（全盘拒 `FILE_ATTRIBUTE_REPARSE_POINT`） | 改造后（白名单放行已本地水合 Cloud Files） |
|---|---|---|
| **OneDrive 桌面/文档分享（高频）** | 已下载的云文件仍会被拒绝 | 本地可读的文件直接分享，无需固定或复制 |
| **普通本地文件（高频）** | 已有属性可判断常规文件 | 复用已有属性和句柄，仅重解析点需要查询标签 |
| **尚未在本地就绪的云文件** | 被笼统归为 `reparse point` | 选中时提示先使其在本机可用，浏览目录时只跳过该项 |
| **符号链接 / 目录联接（低频高危）** | 严格拒绝，防止跳出分享根目录 | **100% 维持原有 no-follow 安全红线**，无任何放宽 |
| **开发者体验（DX）** | 重解析点判定分散在 `osfs` 4 处，且与 Go 标准库 `ModeIrregular` 隐式耦合 | 收敛为单一 Windows 入口准入策略模块，纯判定逻辑可毫秒级单元测试，行为可观测 |

---

## 2. 根因与代码全景（为何不能只改 `pathlimit_windows.go`）

逐层核查 `osfs` 与 Go 1.27 标准库后，实际存在 **4 处耦合拦截点**，必须作为整体重构而非局部打补丁：

1. **入口与扫描拦截点 — [pathlimit_windows.go](../../osfs/pathlimit_windows.go#L8-L17)**
   - `isReparsePoint(fs.FileInfo)` 直接以 `FILE_ATTRIBUTE_REPARSE_POINT != 0` 返回 `true`，被以下 3 处调用：
     - [catalog_source.go](../../osfs/catalog_source.go#L129)（`openSelectedRoot` 拒绝选中根）
     - [catalog_omission.go](../../osfs/catalog_omission.go#L22)（`catalogOmissionFor` 扫描子目录时以 `SourceOmissionReparsePoint` 跳过）
     - [revision_source.go](../../osfs/revision_source.go#L166) 及 [revision_source.go](../../osfs/revision_source.go#L206-L207)（`OpenStable` 拒绝打开修订句柄）
   - Windows 上 `fs.FileInfo.Sys()` 返回的 `Win32FileAttributeData` 缺少 `ReparseTag`。完整的属性和标签可取自目录枚举记录，或通过属性句柄查询 `FileAttributeTagInfo`。

2. **Go 标准库 `ModeIrregular` 隐式拦截**
   - Go 1.23+（当前仓库使用 Go 1.27.3）的 `os/types_windows.go` 对所有非 `SYMLINK` / `AF_UNIX` / `DEDUP` 的重解析点（**包含 `IO_REPARSE_TAG_CLOUD_*`**）都会设置 `fs.ModeIrregular`，导致 **`info.Mode().IsRegular()` 返回 `false`**。
   - 即使绕过 `isReparsePoint`，[catalog_source.go](../../osfs/catalog_source.go#L129)、[catalog_omission.go](../../osfs/catalog_omission.go#L24)、[revision_source.go](../../osfs/revision_source.go#L166) 中的 `!info.Mode().IsRegular()` 仍会把已水合 OneDrive 文件当成 `special_file` 拒掉。

3. **稳定性基线与读路径硬拦截 — [windows_revision_native.go](../../osfs/windows_revision_native.go#L381-L383)**
   - `inspectWindowsObjectMetadata` 在 `platformCatalogBaseline`、`BindStable`、每次 `Verify` / `ReadAt` 中都会独立检查：
     ```go
     if information.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
         return windowsMutationToken{}, false, content.ErrRevisionStale
     }
     ```
   - 发现、绑定和读取都要使用同一准入规则。持有写排除句柄期间继续验证版本及云文件的可读状态。

4. **底层 NT 打开标志与 `cldflt.sys` 内核交互**
   - Go 的 `os.Root`（`Openat` 传 `OBJ_DONT_REPARSE`）与 [windows_revision_native.go](../../osfs/windows_revision_native.go#L416-L467)（`NtCreateFile` 传 `OBJ_DONT_REPARSE` + `FILE_OPEN_REPARSE_POINT` + 拒绝 `FILE_SHARE_WRITE`）直接与 Windows 云文件微过滤驱动 `cldflt.sys` 交互。
   - Cloud Files 在这些标志下的读回内容、目录枚举与写排除行为由 Phase 0 实测。WOF 等其他标签沿用拒绝策略。

5. **相关边界说明**
   - **上游激活层**在路径规范化前检查来源持久性，并用 WinRT 身份独立重开。首次打开的召回行为纳入 Phase 0，重解析点准入归 `osfs`。[provenance.rs](../../desktop/crates/windshare-activation/src/windows/provenance.rs)
   - **桌面端错误与跳过可见性**：当前 `engine.SourceOmissions` 仅经有损队列 `task.Observations()` 流向 [host/desktop/observations.go](../../host/desktop/observations.go#L47-L65) 写入日志，未进入权威 `ShareSnapshot` 暴露给桌面 UI；[host/desktop/creation.go](../../host/desktop/creation.go#L152) 绕过了 `errorText`，运行期终结的 `Completion.Error` 仅为裸字符串，且桌面端缺少中英文本地化及出错/冲突文件名展示。Phase 1 一并补齐。
   - **接收端**：[platform_windows.go](../../osfs/internal/output/platform_windows.go#L59-L75)（及 L140、L272、L388、L521）目前同样拒写 `FILE_ATTRIBUTE_REPARSE_POINT`。接收端涉及 OneDrive 后台上传锁与原子 rename 竞争，列为独立后续阶段（Phase 4），不阻塞分享端主线。

---

## 3. 目标架构与核心设计

### 3.1 安全边界与语义分类表

| 文件/目录形态 | 关键 Win32 特征 | 准入决策 | 领域错误 / 目录跳过语义 |
|---|---|---|---|
| 普通本地文件/目录 | 无重解析点且无适用于该查询的脱机或召回属性 | 复用已有类型和属性 | — |
| 已本地就绪的云文件 | 云标签，状态含 `PLACEHOLDER`，无 `PARTIAL`、`PARTIALLY_ON_DISK` 或适用的脱机/召回标志 | 通过稳定读取校验后放行，`PINNED` 为可选 | — |
| Cloud Files 目录 | 判断目录本身能否本地枚举 | 放行可本地列出的混合目录，逐项分类。需补全目录项时依 Phase 0 结论处理 | — |
| 尚未就绪的云文件、需跳过的占位目录 | 存在适用的召回标志或云状态表明尚未就绪 | 选中根时提示先使其本地可用，扫描时跳过该项 | `SourceOmissionCloudPlaceholder` / `SourceFailureCloudPlaceholder` |
| 符号链接 | `IO_REPARSE_TAG_SYMLINK` / `fs.ModeSymlink` | **严格拒绝** | `SourceOmissionSymbolicLink` / `SourceFailureUnsupported` |
| 目录联接 / 挂载点 / 未知重解析点（含 WOF、AppExecLink 等） | `tag & 0x20000000 != 0` 或非白名单 `ReparseTag` | **严格拒绝（fail-closed）** | `SourceOmissionReparsePoint` / `SourceFailureUnsupported` |
| 设备/管道等特殊文件 | 非目录且非磁盘常规文件（`FILE_TYPE_DISK` 校验失败） | **严格拒绝** | `SourceOmissionSpecialFile` / `SourceFailureUnsupported` |

> **云标签与状态判定**
> - `IO_REPARSE_TAG_CLOUD` (`0x9000001A`) 至 `IO_REPARSE_TAG_CLOUD_F` (`0x9000F01A`)，使用 Microsoft 官方掩码 `IO_REPARSE_TAG_CLOUD_MASK = 0x0000F000` 验证 `(tag &^ cloudTagMask) == ioReparseTagCloud`，并显式断言 `tag & reparseTagNameSurrogateBit == 0`。
> - 对白名单标签，将同一条枚举记录或同一次 `FileAttributeTagInfo` 查询的属性、标签传给 `CfGetPlaceholderStateFromAttributeTag`。`INVALID` 或无法识别的状态按不支持处理。[Cloud Files 状态定义](https://learn.microsoft.com/en-us/windows/win32/api/cfapi/nf-cfapi-cfgetplaceholderstatefromattributetag)
> - `RECALL_ON_OPEN` 只在目录枚举信息中表示召回。`GetFileAttributesW` 和句柄查询中的同一位 `0x00040000` 不能作为召回证据，它与 `FILE_ATTRIBUTE_EA` 共用位值。[文件属性定义](https://learn.microsoft.com/en-us/windows/win32/fileio/file-attribute-constants)

### 3.2 `osfs` 单一权威策略重构（Deep Module）

不再保留只看 `fs.FileInfo` 的 `isReparsePoint`，也不让调用方自行用 `Mode().IsRegular()` 猜测 Windows 文件类型；改为由平台层提供语义封闭的枚举分类器（如 `sourceEntryKind`）：

```go
type sourceEntryKind uint8

const (
    sourceEntryDirectory sourceEntryKind = iota + 1
    sourceEntryRegularFile
    sourceEntrySymbolicLink
    sourceEntryReparsePoint
    sourceEntryCloudPlaceholder
    sourceEntrySpecialFile
)
```

- 出链接前只检查选中根，子项按需枚举。普通项复用已有属性，仅对缺少标签的重解析点做无召回属性查询。
- Windows 的 `os.Lstat` 遇到重解析点时会尝试打开句柄，`Root.Lstat` 本身也会打开句柄。从首次访问起防止隐式召回正文，在实际使用的句柄上复核身份和云状态。
- `windowsRevisionMetadata` 提供可注入的标签和文件类型查询。打开或绑定时复用同一句柄已取得的类型，缺少证据才查 `GetFileType`。普通文件的 `Verify` / `ReadAt` 复用现有元数据，云文件按需复核就绪状态。
- `windowsStableFile.verifyLocked()` 将已绑定文件脱水或标签变化引起的 `ErrRevisionStale` 转为 `content.ErrSourceDrift`。

### 3.3 平台中立的领域错误与可观测性

- 遵循 `core/` 不泄漏 Windows 专有概念的原则：
  - `core/catalog` 新增 `SourceOmissionCloudPlaceholder = "cloud_placeholder"` 与 `SourceFailureCloudPlaceholder`（对称命名，避免 `offline` 与断网语义混淆，同样适用于未来 macOS iCloud `SF_DATALESS` 占位符），`engine` 对应新增 `FailureSourceCloudPlaceholder = "source_cloud_placeholder"`。
  - **不在 `core/content` 增设无修订消费者的 `ErrSourceOffline`**：选根采集阶段直接由 `osfs` 内部哨兵或 `&catalog.SourceError{Failure: catalog.SourceFailureCloudPlaceholder}` 分类。
- **结构化观测日志**：
  - 在 `osfs` 诊断事件中为云文件关键分支补充结构化字段（如 `reparse_tag` 十六进制、`attributes` 掩码、判定结果 `hydrated_cloud` / `cloud_placeholder` / `rejected_reparse`），便于排查第三方云盘（iCloud / Dropbox / 企业同步盘）的异常标签或后台锁冲突。

---

## 4. 分阶段执行计划

### Phase 0：Windows 11 OneDrive / Cloud Filter 真机内核探针（前置验证）

在真实 Windows 11 OneDrive 目录复测分享入口，并在受控 Cloud Files 同步根通过 `CfConnectSyncRoot` 分别记录 `FETCH_DATA`（文件正文）和 `FETCH_PLACEHOLDERS`（目录项补全）。

1. 记录属性的查询来源、标签和解码状态。覆盖已缓存、固定、部分水合、仅云端文件，以及同步根、已展开、未展开和混合目录。
2. 从首次访问覆盖 `os.Lstat`、`os.OpenRoot`、`Root.Lstat`、`Root.Open`、`Root.OpenRoot` 及两遍目录枚举。在预检与打开之间让文件脱水，验证选根、扫描和 `OpenStable` 均未触发 `FETCH_DATA`。用未展开目录观察 `FETCH_PLACEHOLDERS`。
3. **`NtCreateFile` 标志组合对比**：覆盖真实生产分享链路 **`retainWindowsRevisionRoot`（`os.OpenRoot` → `authority.Open(".")` → `DuplicateHandle`）** 以及 `openWindowsRootHandle`，在其上调用 `openWindowsRelativeStableHandle` 与属性查询 `NtCreateFile`，分别实测**根下直接文件**与**多层嵌套云子目录内的文件**（如 `subdir\nested\file.txt`，中间目录同样带 `IO_REPARSE_TAG_CLOUD_*`）：
   - 方案 1（现状）：`OBJ_DONT_REPARSE` + `FILE_OPEN_REPARSE_POINT` + `FILE_SHARE_READ|FILE_SHARE_DELETE`；
   - 方案 2（备选）：`OBJ_DONT_REPARSE` + `FILE_OPEN_NO_RECALL`（不带 `FILE_OPEN_REPARSE_POINT`）+ `FILE_SHARE_READ|FILE_SHARE_DELETE` + 句柄级 `AttributeTag` 复核。
   - 比对已水合文件完整读回的 SHA-256，确认仅云端文件的 `FETCH_DATA` 计数为零。验证选中根之下的中间或末级 symlink / junction 均被拒绝。
4. 持有写排除句柄时尝试释放空间或写入，验证操作受阻或读取报告漂移，并观察读前、读后的召回记录。记录后台同步及 Office 占用时 `ERROR_SHARING_VIOLATION` 的频率和时长。
5. 从系统共享面板选择云文件与目录，验证 `canonicalize`、`GetFileFromPathAsync` / `GetFolderFromPathAsync` 和 `IsEqual` 的结果及召回行为。

> 优先验证 `os.Root` 以及 `Root.OpenFile` 可透传的 Windows 标志。仅对无法满足根约束或无召回要求的打开点，封装相对根句柄的原生操作。

---

### Phase 1：领域错误、桌面端可见性与提示语义升级

**涉及文件**：
- [core/catalog/omission.go](../../core/catalog/omission.go)、[core/catalog/source_error.go](../../core/catalog/source_error.go)
- [engine/failure.go](../../engine/failure.go)、[engine/share_snapshot.go](../../engine/share_snapshot.go)、[engine/internal/share/controller.go](../../engine/internal/share/controller.go)、[engine/internal/share/observation.go](../../engine/internal/share/observation.go)
- [cmd/wind/internal/humanoutput/source_omissions.go](../../cmd/wind/internal/humanoutput/source_omissions.go)、[cmd/wind/internal/humanoutput/format.go](../../cmd/wind/internal/humanoutput/format.go)
- [host/desktop/protocol.go](../../host/desktop/protocol.go)、[host/desktop/snapshot.go](../../host/desktop/snapshot.go)、[host/desktop/creation.go](../../host/desktop/creation.go)、[host/desktop/request_log.go](../../host/desktop/request_log.go)
- [desktop/crates/windshare-host/src/model.rs](../../desktop/crates/windshare-host/src/model.rs)、[desktop/crates/windshare-desktop/src/model/share.rs](../../desktop/crates/windshare-desktop/src/model/share.rs)、[desktop/crates/windshare-desktop/src/live/projection.rs](../../desktop/crates/windshare-desktop/src/live/projection.rs)、[desktop/crates/windshare-desktop/src/localization/catalog.rs](../../desktop/crates/windshare-desktop/src/localization/catalog.rs)、[desktop/crates/windshare-desktop/src/sharing/result.rs](../../desktop/crates/windshare-desktop/src/sharing/result.rs)、[desktop/crates/windshare-desktop/src/sharing/detail.rs](../../desktop/crates/windshare-desktop/src/sharing/detail.rs)
- [desktop/crates/windshare-activation/src/windows/provenance.rs](../../desktop/crates/windshare-activation/src/windows/provenance.rs)

**实施内容**：
1. **领域错误与跳过原因**：
   - `core/catalog` 新增 `SourceOmissionCloudPlaceholder = "cloud_placeholder"`（纳入 `SourceOmissionReason.Valid()`）与 `SourceFailureCloudPlaceholder`。
   - `engine/failure.go` 新增 `FailureSourceCloudPlaceholder = "source_cloud_placeholder"` 并纳入 `isSourcePathFailureCode` 与 `classifySourceFailure`。
2. **CLI 文案**：
   - 目录跳过示例 `item not fully available on this device`。
   - 选中根失败附带 `Path`，提示 `Make this item available on this device, then share it again.`
3. **桌面端错误码透传与双语本地化**：
   - `host/desktop`（同步修正 `creation.go:152` 绕过 `errorText`）在 `Failure` 中以独立字段透传 `engine.FailureCode`（如 `EngineCode`）、`SourcePath` 以及 `SelectionCollision`（`FirstPath` / `SecondPath`），**保留既有 `Failure.Code` 协议语义不重载**（避免破坏账号/设置/网络等现有匹配）；同时在 `projectTask` 处理运行期失败的 `Completion` 时补齐结构化 `Failure`（避免 `ShareReadyReached` 后终结只剩裸字符串 `Completion.Error`），从而在 Rust `model::Share` 实现真正统一的结构化错误投影；`request_log.go` 严守隐私边界，不将 `SourcePath` 或冲突路径写入默认支持日志。
   - Rust 桌面端对高频可操作的源错误码（如 `source_cloud_placeholder`、`source_unsupported`、`source_missing`、`source_access_denied`、`selection_collision` 等）做中英文本地化渲染并标明出错/冲突文件名（适配中文「始终保留在此设备上」），其余错误平滑回退 `error.message`。
4. **桌面端目录跳过摘要可见性**：
   - 复用目录成功提交后的 `ObserveSourceOmissions` 回调。`share.Controller` 按 `DirectoryID` 汇总一次，保留总数和有界样例，有跳过项才调用 `NotifyStateChanged()`，由 `ShareSnapshot` 投影给桌面。默认支持日志仅记录数量和原因，核查 `sample.Detail` 脱敏。
   - 桌面端仅在 `omitted_count > 0` 时于分享结果卡片/详情页展示克制的轻量提示条（参照 `relay_notice`，不弹打断式 Toast、不阻塞出链接、无跳过项时零 UI 渲染），文案诚实体现渐进发现下的当前已跳过条目与样例原因（如仅存云端），不误导为全量扫描总数。
5. **Share Target 文案对齐**：
   - `provenance.rs` 按 `GetFileAttributesW` 适用的属性预检。已确认的云占位项提示先使其在本机可用，暂存文件沿用 `SAVE_LOCALLY`。

---

### Phase 2：`osfs` 统一文件准入重构与 Windows 云文件支持

**涉及文件**：
- 重构 [osfs/pathlimit_windows.go](../../osfs/pathlimit_windows.go) 与 [osfs/pathlimit_other.go](../../osfs/pathlimit_other.go)（重命名为语义更明确的 `source_policy_windows.go` / `source_policy_other.go`）
- [osfs/catalog_source.go](../../osfs/catalog_source.go)
- [osfs/catalog_omission.go](../../osfs/catalog_omission.go)
- [osfs/revision_source.go](../../osfs/revision_source.go)
- [osfs/selected_file_source.go](../../osfs/selected_file_source.go)
- [osfs/windows_revision_native.go](../../osfs/windows_revision_native.go)
- [osfs/windows_stable_revision.go](../../osfs/windows_stable_revision.go)
- 视 Phase 0 结论决定是否微调 [provenance.rs](../../desktop/crates/windshare-activation/src/windows/provenance.rs)

**实施内容**：
1. **统一入口分类模块（`source_policy_windows.go` / `source_policy_other.go`）**：
   - `inspectSelectedRootKind`、`inspectRootChildKind` 和 `inspectHandleKind` 共用 §3.1 的分类规则。由纯函数根据查询来源、文件类型和已解码的云状态判定。
   - 子项查询复用扫描目录的句柄，嵌套路径始终相对已保留的根解析。打开标志和属性访问方式按 Phase 0 结论选定。
2. **改造 `catalog_source.go` 与 `catalog_omission.go`**：
   - `openSelectedRoot` 改用 `inspectSelectedRootKind`（并顺手清理该函数中 4 处重复 `root.Close()`）：
     - `sourceEntryCloudPlaceholder` → 返回包装 `osfs` 内部云占位哨兵的错误，经 `classifySourceError` 映射为 `catalog.SourceFailureCloudPlaceholder`；
     - `sourceEntryDirectory` / `sourceEntryRegularFile` → 通过已验证的打开路径建立 `platformCatalogBaseline`。若确认选中对象在打开前脱水，保留云占位错误分类。
     - 其余种类 → 返回 `ErrUnsupportedStability`（`SourceFailureUnsupported`）。
   - `scanChild` 改用 `inspectRootChildKind` + `omissionForEntryKind`：
     - `sourceEntryCloudPlaceholder` → 映射为 `SourceOmissionCloudPlaceholder`；
     - `sourceEntrySymbolicLink` / `sourceEntryReparsePoint` / `sourceEntrySpecialFile` → 映射为对应既有 omission 原因；
     - 通过无召回元数据确认 `before` 和打开的子项仍为同一对象。仅该子项脱水时记录 `SourceOmissionCloudPlaceholder`，继续扫描兄弟项。目录身份变化、条目被替换或前后枚举不一致才返回 `ErrDirectoryStale`。
3. **改造 `revision_source.go`、`windows_revision_native.go` 与 `windows_stable_revision.go`**：
   - `RootedRevisionSource.OpenStable` 从首次 `Lstat` 到稳定打开采用 Phase 0 验证的路径。保留 `before`、`after` 与已打开句柄的 `os.SameFile`、预期大小和版本校验，用 `inspectHandleKind` 替换分散的 `IsRegular()` / `isReparsePoint()` 判断。
   - `inspectWindowsObjectMetadata` 复用已有 `FileInformation`。普通文件的 `Verify` / `ReadAt` 只使用既有元数据查询。云文件通过复核后签发 `windowsMutationToken`，重开时遇到脱水或非白名单标签返回 `ErrRevisionStale`，已绑定后发生该变化返回 `ErrSourceDrift`。
   - 保留 `STATUS_REPARSE_POINT_ENCOUNTERED` 的失败分类。仅在实测短暂占用常见且重试有效时，对受影响文件在根锁外做可取消的限时退避。持续占用的 `SourceBusy` 线协议留待后续评估。
4. **结构化日志与诊断**：
   - 在 `osfs` 诊断 Sink 中记录云占位符跳过（携带 `reparse_tag` 与 `attributes`）及已水合云根接入日志，便于问题回溯。

---

### Phase 3：测试与验证

**原则**：新增单测全部基于纯函数与注入接口（`windowsRevisionMetadata` / fake `FileInfo`），运行耗时控制在毫秒级，不显著增加本地测试时间；不对 UI 文案/样式写脆弱测试。

1. **纯函数与模拟边界单元测试**（更新 [path_facts_windows_test.go](../../osfs/path_facts_windows_test.go)、[windows_stability_test.go](../../osfs/windows_stability_test.go)、[windows_revision_capability_test.go](../../osfs/windows_revision_capability_test.go) 等）：
   - 覆盖云标签族及已就绪、`PARTIAL`、`PARTIALLY_ON_DISK`、`INVALID` 状态，区分目录枚举的 `RECALL_ON_OPEN` 与其他查询中的同位值。已缓存和已固定的就绪文件均应放行。
   - 模拟同一子项在打开前脱水，验证可读兄弟项正常提交。用假句柄统计普通文件读前、读后的标签和类型查询次数，确认没有新增调用。
   - 覆盖 `IO_REPARSE_TAG_SYMLINK`、`IO_REPARSE_TAG_MOUNT_POINT`、`IO_REPARSE_TAG_WOF`、未知自定义 tag 及非 `FILE_TYPE_DISK`（均应拒绝）；
   - 覆盖分享中途 `Verify` / `ReadAt` 期间属性突变为 `RECALL_ON_DATA_ACCESS` 或 `OFFLINE` 时通过 `verifyLocked()` 触发 `ErrSourceDrift`；
   - 覆盖 `core/catalog`、`engine`（`ShareSnapshot` 跳过摘要权威聚合与 `DirectoryID` 去重）、`host/desktop`（含 `SelectionCollision` 与运行期失败 `Completion` 的结构化错误投影）、`cmd/wind` 对 `SourceOmissionCloudPlaceholder`、`FailureSourceCloudPlaceholder` 的测试。
2. **本地质量门禁**：
   - `go test ./core/catalog/... ./osfs/... ./engine/... ./host/desktop/... ./cmd/wind/...`
   - `make desktop-check`
   - 最终跑 `make check` 与 `make sloc`（注意单文件行数和目录文件数上限）。
3. **Windows 11 OneDrive 真机冒烟验证**：
   - 从 Explorer 右键、拖拽、Picker、CLI、Share Target 分别分享 OneDrive 桌面/文档中的已水合单文件与文件夹，浏览器端成功浏览并完整下载且哈希一致；
   - 分享包含“已水合 + 纯云端 ☁️”混合文件的 OneDrive 目录：已水合文件正常可见可下载，纯云端文件被跳过且显示清晰提示、**未触发 OneDrive 后台下载**；
   - 直接选中纯云端 ☁️ 文件发起分享：立即失败并展示本地下载引导，无卡顿、无静默水合；
   - 目录内嵌套 NTFS symlink / junction：仍严格跳过/拒绝，无法越界读取。
   - 同机对照改造前后普通目录的出链接时间、首次列目录耗时和普通大文件吞吐。已水合大文件与同内容的本地副本对照。

---

### Phase 4（独立后续，不与分享端强绑）：接收端 OneDrive 目标目录评估

当前 `wind receive` 与后续桌面接收端在 [osfs/internal/output/platform_windows.go](../../osfs/internal/output/platform_windows.go#L59-L75)（以及 `verifyOpenHandle` L272、`lstatChild` L388、`entryMatchesSpelling` L521）同样全盘拒绝 `FILE_ATTRIBUTE_REPARSE_POINT`。默认 `Downloads` 目录通常不受 OneDrive 迁移影响，但建议在桌面端接收功能脱离 `--preview` 之前完成本阶段，避免“能从桌面分享、却无法保存到桌面”的不对称体验：

- 仅允许**目标容器目录及其已有常规子目录**携带已水合 `IO_REPARSE_TAG_CLOUD_*` 标签；
- 目标文件若为未水合云占位符，仍视作 `EntryOther` / `ErrUnsafeNamespace` 拒绝覆盖，避免无意触发云端拉取或破坏冲突策略；
- 实测 OneDrive 客户端在文件刚落盘、原子 rename 替换期间的句柄占用行为，评估是否与现有 Windows rename backoff 机制兼容。

---

## 5. 关键风险与防御措施

1. **`cldflt.sys` 与 `FILE_OPEN_REPARSE_POINT` 语义不一致**：通过 Phase 0 真机实测完整文件读回 + SHA-256 校验前置消解；绝不在未实测前提下合入生产代码。
2. **目录补全与内容召回**。按 Phase 0 的独立回调记录核对首次打开、枚举和读取，防止将目录项补全与文件数据下载混为一谈。
3. **误放行其他滤镜重解析点导致静默读空（如 `WOF`）**：采用严格白名单（仅限 `IO_REPARSE_TAG_CLOUD_*`）而非黑名单（`!IsNameSurrogate`），未知标签一律 fail-closed。
4. **平台规则再次漂移到 Rust 层**：坚持 `osfs` 是 Windows 重解析点策略的唯一权威；`provenance.rs` 仅维护 Share Target 专属的来源持久性检查与准确提示文案，不复制 `ReparseTag` 判定逻辑。
