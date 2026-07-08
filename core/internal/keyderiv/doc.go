// Package keyderiv 集中全部 HKDF-SHA256 密钥派生(执行计划 §6.3)。
//
// manifestKey/streamKey 由 readSecret 派生,segKey 由 streamKey‖u32_be(段号)
// 派生;salt 一律为空(readSecret 每分享随机唯一),label 取精确 ASCII 字面
// 字节、无结尾 NUL。internal:派生只在此发生,由 core/share 门面接线,
// manifest/chunk 只接受派生好的 key 参数。
package keyderiv
