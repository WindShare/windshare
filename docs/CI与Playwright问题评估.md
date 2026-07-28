# 本地 CI 与 GitHub 失败问题评估

> 记录时间：2026-07-28。本文重点评估 Playwright；POSIX/native output 只作为历史 CI 背景，不是本次修复对象。

## 结论

`make ci` 通过不等于 GitHub CI 通过：两者的操作系统、文件系统、Shell、浏览器和资源限制不同。当时本地 Windows `make ci` 输出 `ci: all gates passed`，而历史运行 [30071578869](https://github.com/WindShare/windshare/actions/runs/30071578869) 的 11 个任务中 6 个通过、5 个在 Ubuntu 上失败；这不足以证明 Linux/浏览器门禁正确。当前修复仍在本地工作树，远端运行不会自动看到未提交内容。

最新主线运行 [30321334065](https://github.com/WindShare/windshare/actions/runs/30321334065)（`e952e59`）仍为失败：11 个任务中 8 个通过，剩余 3 个是核心覆盖率、核心发布认证和 Playwright。

## 历史问题（运行 30071578869）

| 类别 | 根因/证据 | 判断 |
|---|---|---|
| Linux lint | Linux 专属 `core/osfs/posix_stability_linux.go` 在 Windows 本地不会被检查；Ubuntu 报 `ST1005` 和 `unconvert`。 | 环境矩阵差异，属于可修复门禁问题。 |
| POSIX 输出身份 | Linux 仅凭 `Dev+Ino` 识别文件；删除并重建文件后 inode 可复用，旧恢复记录可能误认新文件。 | 独立 native 工作线；本文不把它列为 Playwright 待办。 |
| Golden vectors | workflow 直接执行脚本，仓库中的脚本没有可执行位，退出码 126；Windows 走 PowerShell 入口。 | 调用方式/权限问题。 |
| Firefox hot-switch | `peerCapable=true` 只代表浏览器 API 可用，实际没有 authenticated peer lane admission；传输先走完 relay。 | 测试观察点与运行时语义曾不一致。 |
| WebKit | `page.evaluate: Target crashed`。 | 与 Firefox 独立，需单独定位浏览器/资源/应用崩溃原因。 |

## 当前状态（运行 30321334065）

已验证通过：Lint（Linux/Windows、两个 Go module）、golden-vector、根模块构建与覆盖率、Web 基础检查、Hygiene、Windows 测试等。说明显式 `bash`、lint 矩阵等简单门禁修复有效。

SLOC 输出中的 `Files: 17/19/20 (limit: 20)` 是警告注释，不是失败；该任务本身为通过。

仍失败：

1. **核心覆盖率**：`osfs/internal/outputlinux` 为 `64.4%`（要求 `70%`），核心总覆盖率 `89.1%`（要求 `90%`）。优先补充新 Linux 输出身份代码的定向测试，不应降低阈值。
2. **核心发布认证**：根因已确认。Linux contract 仍引用重构前的 `core/osfs/linux_output_persistent_identity_native_test.go`，真实文件已移动到 `core/osfs/internal/outputlinux/`，因此 `grep` 报 `No such file or directory`。这是简单的静态路径遗漏；修复后仍需下一次 GitHub 运行确认后续 ext4 门禁。
3. **Playwright**：Firefox 和 WebKit 各 1 个失败，见下文。

## Playwright 是什么

Playwright 是“自动操作真实浏览器”的测试工具，不是 WindShare 的运行时依赖。测试会启动 Chromium、Firefox、WebKit，打开接收页面，执行点击/下载，并检查真实网络、WebRTC、文件输出和 UI 状态；因此它能发现 Go 单测和 Vitest 看不到的跨浏览器问题。

- **Relay**：浏览器 → 中继服务器 → 发送端。
- **Peer/P2P**：浏览器与发送端通过 WebRTC 直接传输。
- **Hot-switch**：先用 relay 开始传输，再切换到已认证的 peer，且同一文件不能损坏或丢数据。

当前 Firefox 证据是：`peerCapable=true`，但 `laneAdmissions` 只有 relay；写入 barrier 等待约 15.15 秒后超时，传输以 0 字节中止。也就是说“浏览器支持 P2P”不等于“本次 P2P 已成功接管”。当前代码已有 `onContentLaneAdmitted` 观测回调，下一步应查 peer negotiation/attempt 为什么没有到达 admission。WebKit 则是页面进程直接崩溃，不能与 Firefox 合并处理。

## 建议与待决策项

建议保留 Playwright 为阻塞门禁，不用 `skip`、固定 sleep 或盲目重试掩盖失败。

1. **先做简单项**：修复 core-release 的过期静态路径，并让跨平台 CI contract 检查此类断言目标；补齐 `outputlinux` 覆盖率后再跑 Ubuntu CI。
2. **Firefox**：记录并断言三种状态——API 可用、peer attempt 结果、实际 lane admission；用事件驱动 barrier 验证“成功切 peer”或“明确 fallback relay”。
3. **WebKit**：保留 trace、video、console/browser 日志，单独复现并检查资源/Renderer 崩溃；在原因明确前不把它改成跳过。
4. **需要决策**：Linux 发布认证是否必须限定真实 ext4 runner；WebKit 是否继续作为合并阻塞门禁；P2P admission/fallback 的最终验收语义。

> 范围说明：POSIX/native output 的实现修复与 Linux/ext4 认证另行追踪；本文只用它解释历史 CI 差异，不用它判断 Playwright 是否通过。
