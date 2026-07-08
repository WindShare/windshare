# testvectors — Go↔TS 黄金测试向量

> 权威规范:[`docs/执行计划.md`](../docs/执行计划.md) §7(测试策略)、附录 B(尤其 B6/B11)、T0.3。
> 本目录是 Go 与 TS 两套实现保持锁步的生命线:同一份向量,双侧各跑一遍,逐字节比对。

## 使用方式(仓库 checkout 为准)

`testvectors/` 位于 core 模块之外——core 的模块 zip **不含**本目录,跨实现对拍
一律以仓库 checkout 为准(§7)。消费方经相对路径读取:

- **Go 侧读取骨架**:[`core/internal/testvec`](../core/internal/testvec)(core 各包测试
  以相对路径 `../../../testvectors/<kind>.json` 传入;所有 Go 侧向量消费者都在 core 模块内)。
- **TS 侧读取骨架**:[`web/test/vectors.ts`](../web/test/vectors.ts)(vitest 在 node
  环境下经 `fs` 读取仓库内文件)。

实际向量由 M1a T1.7 的 `-update` 模式生成并提交;M0 仅提供框架与信封自检样例。

## 文件约定

- 每类向量一个 JSON 文件,文件名 = `<kind>.json`。已规划的 kind:
  - `keyderiv`(HKDF KAT:readSecret → manifestKey/streamKey/segKey)
  - `link`(链接/fragment 编解码,含 split-key)
  - `chunk-seal`(块 AEAD:固定 RNG 注入下的 Seal/Open)
  - `manifest-seal`(清单确定性 CBOR + GCM 封装,sealedManifest 逐字节对拍)
  - `frame-codec`(数据面二进制帧 REQUEST/BLOCK/ERROR,小端)
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
- 块密文项 = `base64(nonce ‖ ct ‖ tag)`(suite `0x01`:12B nonce + 明文长密文 + 16B tag)。
- 清单项 = `base64(nonce ‖ cbor_gcm)`(12B nonce 前置于 GCM 输出)。
- 数据面帧向量 = 整帧字节的 base64(定长小端布局,§6.7);向量过大时允许
  改放同名 `.bin` 旁路文件,JSON 内以相对文件名引用(届时更新本约定)。
- **确定性来源**:随机 nonce 由注入的固定 RNG 喂出、readSecret 由测试注入
  (§6.6 Options、B11)——生成与校验两侧必须使用向量文件中记录的同一输入。
