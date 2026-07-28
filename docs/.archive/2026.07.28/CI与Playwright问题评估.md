# 本地 CI 与 GitHub 失败问题评估

> 记录时间：2026-07-28。本文重点评估 Playwright，并补记本轮 Linux output/core-release 闭环证据。

## 结论

`make ci` 通过不等于 GitHub CI 通过：两者的操作系统、文件系统、Shell、浏览器和资源限制不同。当时本地 Windows `make ci` 输出 `ci: all gates passed`，而历史运行 [30071578869](https://github.com/WindShare/windshare/actions/runs/30071578869) 的 11 个任务中 6 个通过、5 个在 Ubuntu 上失败；这不足以证明 Linux/浏览器门禁正确。本轮 Linux 修复已推送到 `codex/linux-output-platform-tests` 并在 Ubuntu 上验证。

主线基线运行 [30321334065](https://github.com/WindShare/windshare/actions/runs/30321334065)（`e952e59`）为失败：11 个任务中 8 个通过，剩余 3 个是核心覆盖率、核心发布认证和 Playwright。

## 历史问题（运行 30071578869）

| 类别 | 根因/证据 | 判断 |
|---|---|---|
| Linux lint | Linux 专属 `core/osfs/posix_stability_linux.go` 在 Windows 本地不会被检查；Ubuntu 报 `ST1005` 和 `unconvert`。 | 环境矩阵差异，属于可修复门禁问题。 |
| POSIX 输出身份 | Linux 仅凭 `Dev+Ino` 识别文件；删除并重建文件后 inode 可复用，旧恢复记录可能误认新文件。 | 独立 native 工作线；本文不把它列为 Playwright 待办。 |
| Golden vectors | workflow 直接执行脚本，仓库中的脚本没有可执行位，退出码 126；Windows 走 PowerShell 入口。 | 调用方式/权限问题。 |
| Firefox hot-switch | `peerCapable=true` 只代表浏览器 API 可用，实际没有 authenticated peer lane admission；传输先走完 relay。 | 测试观察点与运行时语义曾不一致。 |
| WebKit | `page.evaluate: Target crashed`。 | 与 Firefox 独立，需单独定位浏览器/资源/应用崩溃原因。 |

## 当前状态（基线 30321334065 + 本轮验证）

已验证通过：Lint（Linux/Windows、两个 Go module）、golden-vector、根模块构建与覆盖率、Web 基础检查、Hygiene、Windows 测试等。说明显式 `bash`、lint 矩阵等简单门禁修复有效。

SLOC 输出中的 `Files: 17/19/20 (limit: 20)` 是警告注释，不是失败；该任务本身为通过。

基线失败项与本轮结果：

1. **核心覆盖率（已修）**：新增真实 Linux 文件描述符上的 platform/directory/file 契约测试；只对 ext4 证书身份做确定性 syscall 注入，使 overlayfs runner 仍能覆盖 adapter 生命周期。完整 Linux job [30325768889](https://github.com/WindShare/windshare/actions/runs/30325768889/job/90170781280) 中 `outputlinux` 为 `78.3%`，核心总覆盖率为 `90.3%`，包级 `70%` 与总计 `90%` 门禁均通过，未降低阈值。
2. **核心发布认证（已修）**：除过期静态路径外，native runner 仍只编译 `./osfs`，导致已迁入 `./osfs/internal/outputlinux` 的 inode-reuse 测试没有进入测试二进制。runner 现按包编译两个静态二进制，并在同一个 loop-ext4 fixture 中运行；定向运行 [30325713405](https://github.com/WindShare/windshare/actions/runs/30325713405) 和完整 Linux job [30325768889](https://github.com/WindShare/windshare/actions/runs/30325768889/job/90170781280) 均通过三项 native/restart 认证。
3. **Playwright（未处理）**：Firefox 和 WebKit 各 1 个失败，见下文。

## Playwright 是什么

Playwright 是“自动操作真实浏览器”的测试工具，不是 WindShare 的运行时依赖。测试会启动 Chromium、Firefox、WebKit，打开接收页面，执行点击/下载，并检查真实网络、WebRTC、文件输出和 UI 状态；因此它能发现 Go 单测和 Vitest 看不到的跨浏览器问题。

- **Relay**：浏览器 → 中继服务器 → 发送端。
- **Peer/P2P**：浏览器与发送端通过 WebRTC 直接传输。
- **Hot-switch**：先用 relay 开始传输，再切换到已认证的 peer，且同一文件不能损坏或丢数据。

当前 Firefox 证据是：`peerCapable=true`，但 `laneAdmissions` 只有 relay；写入 barrier 等待约 15.15 秒后超时，传输以 0 字节中止。也就是说“浏览器支持 P2P”不等于“本次 P2P 已成功接管”。当前代码已有 `onContentLaneAdmitted` 观测回调，下一步应查 peer negotiation/attempt 为什么没有到达 admission。WebKit 则是页面进程直接崩溃，不能与 Firefox 合并处理。

## 建议与待决策项

建议保留 Playwright 为阻塞门禁，不用 `skip`、固定 sleep 或盲目重试掩盖失败。

1. **Linux 门禁已闭环**：过期路径、跨平台 literal contract、`outputlinux` 覆盖率和双包 native runner 均已有 Ubuntu 证据。
2. **Firefox**：记录并断言三种状态——API 可用、peer attempt 结果、实际 lane admission；用事件驱动 barrier 验证“成功切 peer”或“明确 fallback relay”。
3. **WebKit**：保留 trace、video、console/browser 日志，单独复现并检查资源/Renderer 崩溃；在原因明确前不把它改成跳过。
4. **需要决策**：WebKit 是否继续作为合并阻塞门禁；P2P admission/fallback 的最终验收语义。

> 范围说明：本轮只闭环 Linux output/core-release；上述证据不代表 Firefox 或 WebKit 已通过。
