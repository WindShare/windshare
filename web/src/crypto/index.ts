// WebCrypto 侧加密(执行计划 §6.2/§6.10,M1b T5.1):HKDF-SHA256 派生、
// AES-256-GCM 解密、SHA-256,与 core/chunk、core/manifest 对齐黄金向量。
// 接收侧只读随机 nonce(blockCT 首 12B),无需生成 nonce,天然确定可测。
export {};
