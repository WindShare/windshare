# testvectors — Go↔TS 黄金测试向量

> 权威规范:[`docs/执行计划.md`](../docs/执行计划.md) §7(测试策略)、附录 B(尤其 B6/B11)、T0.3。
> 本目录是 Go 与 TS 两套实现保持锁步的生命线:同一份向量,双侧各跑一遍,逐字节比对。

## 使用方式(仓库 checkout 为准)

`testvectors/` 位于 core 模块之外——core 的模块 zip **不含**本目录,跨实现对拍
一律以仓库 checkout 为准(§7)。消费方经相对路径读取:

- **Go 侧读取骨架**:[`core/internal/testvec`](../core/internal/testvec)(core 各包测试
  以相对路径传入);根模块的 relay 信封生成器因 Go `internal` 边界而在
  [`relay/protocol/vectors_test.go`](../relay/protocol/vectors_test.go) 校验同一信封 schema。
- **TS 侧读取骨架**:[`web/test/vectors.ts`](../web/test/vectors.ts)(vitest 在 node
  环境下经 `fs` 读取仓库内文件)。

实际向量由确定性 Go 生成器生成并提交。core 契约/密码向量与 relay 外层信封
分属两个 Go module,因此再生成必须同时运行两条门禁:

```powershell
Push-Location core
go test -count=1 ./share -update
Pop-Location
go test -count=1 ./relay/protocol -update
```

生成完全由固定输入(注入 readSecret/shareId/计数 RNG/固定文件树)决定,
重跑无 diff;core 的 `TestVectorFilesUpToDate` 与 relay/protocol 的向量新鲜度测试
在常规 CI 路径校验「文件字节 ⩵ 当前实现的生成结果」,新鲜度与幂等由同一
比对保证。各 kind 的解码步骤说明写在其 JSON 的
`description` 字段(TS 侧 T5.1 按此消费)。全部 12 类向量在 web 侧均有消费测试
(Vitest / Playwright;逐文件消费者映射见
[`docs/.orchestration/R-B-final.md`](../docs/.orchestration/R-B-final.md) §3)。

## 文件约定

- 每类向量一个 JSON 文件,文件名 = `<kind>.json`。已提交的 kind:
  - `keyderiv`(HKDF KAT:readSecret → manifestKey/streamKey/segKey)
  - `link`(链接/fragment 编解码,含 split-key)
  - `chunk-seal`(块 AEAD:固定 RNG 注入下的 Seal/Open,含跨文件块/末短块/跨段块)
  - `manifest-seal`(清单确定性 CBOR + GCM 封装,canonical 明文与 sealedManifest 逐字节对拍)
  - `frame-codec`(数据面二进制帧 REQUEST/BLOCK/ERROR,小端;含恰为 MaxFrameSize 的整帧上限用例)
  - `geometry`(块大小、流长度、块数与稠密状态边界,并把独立 16 GiB 加密段与 4 MiB 块上限分开钉死)
  - `path-policy`(版本化 NFC/full-fold/碰撞/保留名路径契约)
  - `transfer-plan`(选择、已选字节、半开块区间与确定性 PlanID,含 UTF-8/UTF-16 排序分歧用例)
  - `relay-envelope`(manifest/forward/terminal-forward 外层二进制信封与 9B routed overhead)
  - `relay-signaling`(Go/浏览器精确字段名、Unicode scalar、可选字段、64 层结构上限与 hostile JSON 接受矩阵；
    `canonical` 仅在 opaque payload 有共同词法形式时出现，缺省时不虚构跨 parser 的字节规范化)
  - `relay-endpoint`(Go `net/url` 与浏览器 WHATWG URL 的中转端点规范化/拒绝矩阵，含
    ASCII DNS/IP authority、显式边界空白集与 path/query/userinfo 全 printable-ASCII 分量表)
  - `envelope-sample`(非加密向量:信封格式自检,锁定双端读取骨架)
- 统一信封(envelope)schema:

  ```json
  {
    "version": 1,
    "kind": "<kind>",
    "description": "……",
    "cases": [ { "name": "<用例名>", "…kind 特定字段…": "…" } ]
  }
  ```

  `version` 是**信封**格式版本(与线协议 v1、suiteByte 无关);`cases[*]` 的
  kind 特定字段由各消费测试自行解码(Go 侧保持 `json.RawMessage`,TS 侧保持
  `unknown`),信封解析器不理解具体 kind。

## 编码约定(附录 B 尾注)

- **二进制字节串一律 base64(标准字母表、含填充)**,包括密钥、nonce、密文、帧字节。
- **可能超过 JavaScript safe integer 或要求完整 u64/int64 语义的值一律十进制字符串**
  (如 `"index": "16777216"`);版本/byte/u32 等已证明有界的字段可用 JSON number。
  Go 测试会递归拒绝任何超 `2⁵³-1` 的 JSON number。协议字符串(shareId、keyString
  等 base64url 形态)按其线上形态原样入 JSON,不再二次编码。
- 块密文项 = `base64(nonce ‖ ct ‖ tag)`(suite `0x01`:12B nonce + 明文长密文 + 16B tag)。
- 清单项 = `base64(nonce ‖ cbor_gcm)`(12B nonce 前置于 GCM 输出)。
- 数据面帧向量 = 整帧字节的 base64(定长小端布局,§6.7);向量过大时允许
  改放同名 `.bin` 旁路文件,JSON 内以相对文件名引用(届时更新本约定)。
- **确定性来源**:随机 nonce 由注入的固定 RNG 喂出、readSecret 由测试注入
  (§6.6 Options、B11)——生成与校验两侧必须使用向量文件中记录的同一输入。
