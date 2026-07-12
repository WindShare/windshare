# web — WindShare 浏览器接收端

React 19 + TypeScript + Vite;包管理器 **pnpm**(版本钉在 `package.json` 的
`packageManager`)。设计权威见 [`docs/执行计划.md`](../docs/执行计划.md)
§6.10/§6.2;接口契约见 [`docs/M1-实施说明.md`](../docs/M1-实施说明.md)。

## 目录

| 目录 | 职责 |
|---|---|
| `src/contracts/` | 窄共享 TS 契约(链接/清单值、块选择、帧通道/终止交付、随机写 vs 有序 sink) |
| `src/crypto/` | WebCrypto:HKDF-SHA256 / AES-256-GCM / SHA-256,与 `core` 对齐金标向量 |
| `src/manifest/` | 严格 canonical CBOR 清单解析、几何界、版本化路径策略、`TransferPlan`/`PlanID` |
| `src/transport/` | relay WS 与 WebRTC DataChannel 的 `FrameChannel` 实现(含终止先于关闭语义) |
| `src/session/` | 接收调度器(与 Go 端同构:紧凑需求、有界重排、按 sink 声明的交付顺序) |
| `src/download/` | 落盘能力与 sink:FSA 随机写 / 单文件流 / 流式 ZIP + Zip64 |
| `src/connectivity/` | 连接策略(合同 11 归属地):手势门禁后的 8 MiB/10 s 赛跑、P2P 热加入、双路聚合 |
| `src/ui/` | React 界面:密钥输入、文件树勾选、进度/重连/终止错误、fragment 即时抹除 |
| `test/` | Vitest 单元/集成 + `test/browser/` Playwright 组件级场景 |
| `e2e/` | Playwright 真实栈 E2E(真实 `wsrelay` + `windshare` CLI + Chromium) |

## 本地门禁

```powershell
pnpm -C web install --frozen-lockfile
pnpm -C web lint          # eslint
pnpm -C web exec tsc -b   # CI 用 --force
pnpm -C web build
pnpm -C web test          # vitest run(消费全部 12 类金标向量)
```

## Playwright 调用许可

- **Linux**:`pnpm -C web exec playwright test` 直呼即许可路径(CI 即此,含
  M1b+M1c 全部 30 场景与 D1/D2 interop 专用配置)。
- **Windows**:`playwright.config.ts` 硬性拒绝缺少运行器契约的直呼;唯一许可
  路径是仓库根的 `scripts/d5-windows-performance.ps1 -Mode BrowserTests`。
- E2E 分层与决定性 oracle 映射:
  [`docs/.orchestration/e2e-coverage-map.md`](../docs/.orchestration/e2e-coverage-map.md)。

实测浏览器矩阵:Chromium(Playwright 钉定);FSA 与流式 ZIP 两条落盘路径均被
真实栈覆盖;Safari/Firefox 回退按能力设计、未实测(执行计划 §11 状态)。
