// Package manifest 实现目录模型、确定性 CBOR 编解码与清单 GCM 封装
// (执行计划 §6.4,附录 B4/B5/B14/B15)。
//
// 编码取 fxamacker/cbor 的 Core Deterministic 选项,解码严格模式拒非
// canonical;解码先宽容探测版本 v,未知版本明确报"请升级"。Seal/Open 接受
// 调用方传入的 manifestKey(派生在 core/internal/keyderiv);sealedManifest =
// nonce(12)‖cbor_gcm,每分享仅 Seal 一次、字节复用(§6.3)。清单不含
// offset/streamLen(B14)。路径 canonical(NFC)与折叠碰撞检测在此(§6.13)。
package manifest
