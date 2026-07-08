// Package chunk 实现打包流的分块 AEAD(执行计划 §6.3,附录 B1–B3/B12/B13)。
//
// 每次 Seal 自注入的 io.Reader 取全新随机 12B nonce,输出 nonce‖ct‖tag;
// AAD = suiteByte‖u64_be(全局块号) 承担位置绑定;segKey 按块号所属段
// (SegmentBytes)选取,每 segKey 的 Seal 计数达 MaxSealsPerSegKey 即熔断。
// Open 解析按 suiteByte 分派、不硬编码密文尾部长度,为 suite 0x02 的
// 逐块签名留演进位(§6.14)。
package chunk
