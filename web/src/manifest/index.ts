// 清单 CBOR 解析(执行计划 §6.2,M1b T5.1):与 core/manifest 对齐的严格
// 解码(拒非 canonical),避开 JSON.parse 的 float64 精度坑;几何由 entries
// 数组顺序对 size 前缀和自行推导(B14),并做 §6.13 的结构校验。
export {};
