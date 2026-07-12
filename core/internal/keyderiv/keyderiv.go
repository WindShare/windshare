package keyderiv

import (
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
)

// KeyBytes 是全部派生密钥的统一长度(AES-256 密钥)。
const KeyBytes = 32

// label 是精确 ASCII 字面字节、无结尾 NUL(§6.3):"windshare/v1" 前缀做域分隔,
// 协议版本入 label,未来套件换 label 即整棵密钥树彻底分离。
const (
	labelManifest  = "windshare/v1 manifest"
	labelStream    = "windshare/v1 stream"
	labelSegPrefix = "windshare/v1 seg" // 完整 info = labelSegPrefix ‖ u32_be(seg)
)

// ManifestKey 派生清单封装密钥:HKDF(readSecret, "windshare/v1 manifest", 32)。
func ManifestKey(readSecret []byte) []byte {
	return derive(readSecret, labelManifest)
}

// StreamKey 派生打包流根密钥:HKDF(readSecret, "windshare/v1 stream", 32)。
// 它不直接加密任何块,只作为各段 SegKey 的派生输入。
func StreamKey(readSecret []byte) []byte {
	return derive(readSecret, labelStream)
}

// SegKey 派生第 seg 段(每 SegmentBytes 轮换)的块加密密钥:
// HKDF(streamKey, "windshare/v1 seg"‖u32_be(seg), 32)。
// 段号入 info 而非 salt,与另两条派生保持同构(salt 一律为空)。
func SegKey(streamKey []byte, seg uint32) []byte {
	info := make([]byte, 0, len(labelSegPrefix)+4)
	info = append(info, labelSegPrefix...)
	info = binary.BigEndian.AppendUint32(info, seg)
	return derive(streamKey, string(info))
}

// derive 统一走 HKDF-SHA256。salt 恒为空:密钥的每分享唯一性已由 readSecret
// 随机唯一保证(§6.3),空 salt 使 Go↔TS 双实现只需对齐 info 一个参数。
func derive(secret []byte, info string) []byte {
	key, err := hkdf.Key(sha256.New, secret, nil, info, KeyBytes)
	if err != nil {
		// 不可达:KeyBytes 固定 32,远低于 HKDF-SHA256 的 255×32 输出上限;
		// 派生失败没有可降级路径,静默返回错误密钥比崩溃更危险。
		panic(fmt.Sprintf("keyderiv: HKDF derivation failed: %v", err))
	}
	return key
}
