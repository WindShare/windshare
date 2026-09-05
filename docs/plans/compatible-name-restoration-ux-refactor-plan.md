# 兼容名称恢复体验重构计划

Status: Implemented. Canonical paired artifacts, automatic sidecar lookup, lifecycle-aware restoration UI, and independent local catch-up are in place. The background below records the original rationale.

## 背景

在 Windows 上，只有当原始名称被原生 File System Access 查询拒绝后，浏览器接收端才会回退到兼容的物理名称。恢复产物包含一个不可变的 PowerShell 脚本和一个可变的 `.data` sidecar，后者记录已提交的名称恢复检查点。

当前脚本要求用户传入 `-SidecarPath`，网页展示的长命令同时包含脚本和 sidecar 路径。用户直接运行脚本会遇到缺少参数的错误，离开网页后也难以判断文件如何配对。直接缩短命令仍不够：Windows 默认执行策略可能阻止脚本运行，不能把先失败再查帮助作为默认流程。

这种使用阻力源于模型，而不只是文案：

- 脚本和 sidecar 的名称被独立分配；发生冲突时，两者可能得到不同 token，但产品仍将它们呈现为一个文件对。
- 入口脚本无法推导其配套文件，把内部路径依赖暴露成了必需的用户参数。
- 界面把整条命令当作操作说明，要求用户理解启动参数，而不是提供明确的恢复动作。
- `CompatibleNameRepairSummary` 持久化了预先格式化的 `runCommand`，使持久恢复状态与展示语法耦合。

## 产品决策

将脚本和 sidecar 视为一个拥有共享身份的恢复产物，脚本自行定位清单。网页默认复制包含必要启动参数的完整命令，优先保证首次运行成功；短命令仅作为已允许脚本执行环境下的便捷用法。恢复提示只在实际使用兼容名称时出现，普通下载不增加步骤。

继续保留两个独立文件：sidecar 会在接收期间变化，而脚本必须保持不可变。同时保留活动检查点确认、相邻目录边界、禁止覆盖的移动操作、可安全重复运行，以及“执行名称恢复后，浏览器不能再续传该输出”的规则。

## 目标设计

### 恢复文件对身份

引入文件对级模型，由它持有共享 stem/token 和两个文件身份。模型必须强制名称满足以下形式：

```text
restore.windshare-<pair-token>.ps1
restore.windshare-<pair-token>.data
```

专用文件对分配器每次尝试只派生一个候选 token，在同一父目录中同时检查两个名称；任一名称已被声明或占用时，整个文件对一起重试。根名称和内容名称的分配仍然是相互独立的关注点。

创建任一文件前先持久化文件对选择，然后继续使用现有的逐文件句柄和 owned-object 证据保障变更安全。不转换名称相互独立的开发期旧记录，只允许清理这些记录。

### PowerShell 入口

移除公开的 `-SidecarPath` 要求。脚本使用 `[IO.Path]::ChangeExtension($PSCommandPath, '.data')` 推导精确的相邻 sidecar；文件缺失时，错误信息应给出预期的绝对路径。

保留内部的 `Invoke-WindShareRestoration -Path`，作为由聚焦主机契约测试注入依赖的函数。禁止使用通配符搜索或接受任意相邻 sidecar；严格的同 stem 解析可以避免同一目录包含多次 WindShare 操作时发生歧义恢复。

### 持久状态与展示

从 `CompatibleNameRepairSummary` 中移除 `runCommand`。持久状态只保留有语义的文件对名称、放置位置、检查点进度和受影响路径样本。

将 sidecar 同步进度与终态收尾分开建模，不再由 `pendingCatchUp` 同时表示两者。普通同步落后不阻断继续下载。异常停止后可独立执行本地补齐：重新取得输出独占权限、验证文件对与账本，从持久账本重放已提交映射并验证 sidecar；无需发送端在线，也不要求存在 `pendingTerminalOutcome`。无终态记录时保留 `active` footer 和续传资格；已有终态记录时完成原有收尾。

展示层提供“复制恢复命令”，默认生成不含 sidecar 参数的完整命令；命令正文和技术参数收进可展开详情：

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File ".\restore.windshare-<pair-token>.ps1"
```

详情中可提供已允许脚本执行时的短命令 `.\restore.windshare-<pair-token>.ps1`。“无法运行？”帮助在恢复入口内随时可展开，不依赖网页检测本地执行结果；启动参数只作用于本次 PowerShell 进程，不要求用户永久修改系统策略。

操作旁简洁说明：“在恢复文件所在文件夹打开 PowerShell，粘贴命令。保留两个配套文件，并保持它们与下载内容的相对位置。”

恢复动作按接收状态展示：

- 接收中只提示名称已调整；仍可续传时优先引导继续下载。异常停止后的恢复放在次级详情，紧邻命令说明“恢复原名后，此输出不能再续传”。
- sidecar 尚未追上检查点时，所有入口均不提供可复制的恢复命令，包括详情和帮助；异常停止后的详情提供本地补齐入口，重开页面后仍可使用。补齐成功后再按接收状态开放恢复命令。
- 接收结束且终态检查点完整后，提供“恢复原名”入口。

## 实施顺序

1. 添加文件对级 token 派生、联合命名空间分配器和模型不变量，替换根节点与后代节点激活流程中分别分配脚本和 sidecar 的逻辑。
2. 更新持久化的兼容名称操作状态和 IndexedDB 投影，使用文件对身份和语义化恢复摘要，分离同步进度与终态收尾，并实现异常停止后的独立本地补齐流程。
3. 修改 Windows 模板，让它从 `$PSCommandPath` 解析 sidecar，并为文件对缺失和位置错误提供可操作的错误信息。
4. 将命令构造移入生命周期展示层，默认复制完整启动命令，补齐运行位置说明、可展开帮助和按接收状态展示的恢复入口。
5. 更新聚焦的 TypeScript、浏览器契约和 PowerShell 主机测试，覆盖共享 stem 冲突、空格或 Unicode 路径、脚本自动定位 sidecar、sidecar 缺失、活动状态确认和安全重复运行；验证默认复制命令在 Restricted 策略下的启动行为，以及各接收状态的命令可见性。覆盖映射已提交但 sidecar 未同步时崩溃、尚无终态记录的场景，验证发送端离线时重开页面仍可本地补齐、补齐后恢复入口可用且续传资格保留。
6. 删除面向旧参数的测试夹具，并清理仍将 `-SidecarPath` 描述为正常用户流程的简洁文档。

## 范围边界

- 不把可变 sidecar 合并进不可变脚本。
- 不从浏览器自动启动 PowerShell。
- 不增加全目录 sidecar 搜索，也不静默回退到其他文件对。
- 不改变兼容物理名称映射或名称恢复顺序语义。
- 不增加旧调用方式；本次 pre-v1 重构只建立一种规范的恢复流程。
