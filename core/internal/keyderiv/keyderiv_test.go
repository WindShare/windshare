package keyderiv_test

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/windshare/windshare/core/internal/keyderiv"
)

// KAT 向量由独立的手写 RFC5869 实现(HMAC-SHA256 逐步展开,不经 crypto/hkdf)
// 一次性生成并硬编码:交叉校验 stdlib 接线(空 salt、label 字面、u32_be 段号),
// label 打错一个字节即失败。
var (
	secretA = mustHex("000102030405060708090a0b0c0d0e0f")
	secretB = mustHex("ffffffffffffffffffffffffffffffff")
	// streamA = StreamKey(secretA),作为 SegKey KAT 的输入。
	streamA = mustHex("2ac0aedcddd6c143c9bc2e503a43410d84f8665031dea80721c50c5022f0adfe")
)

func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

func TestKAT(t *testing.T) {
	tests := []struct {
		name string
		got  []byte
		want string
	}{
		{"ManifestKey(A)", keyderiv.ManifestKey(secretA), "4a04eafaa40ee9e993dc254e3ddb1cd1e75e167b9645c671a55f8b728f5f842c"},
		{"StreamKey(A)", keyderiv.StreamKey(secretA), "2ac0aedcddd6c143c9bc2e503a43410d84f8665031dea80721c50c5022f0adfe"},
		{"ManifestKey(B)", keyderiv.ManifestKey(secretB), "ca5728a4e7d5e2e50028960fb701e7ce9e2baf67e12223d53a113843ec668d31"},
		{"StreamKey(B)", keyderiv.StreamKey(secretB), "36a95997cac684930e3c3d6c2565b0145f6a89e99c7cda3aac5873b81f9f6bad"},
		{"SegKey(streamA,0)", keyderiv.SegKey(streamA, 0), "da728f0d1e66651bb1c94b75d8cee8255e1e9263125d84366d73b6ca51da1a61"},
		{"SegKey(streamA,1)", keyderiv.SegKey(streamA, 1), "b008ff5072bfd0b23d054034b630009c60054ea9e68f2c79c930664ac3cb9162"},
		{"SegKey(streamA,max)", keyderiv.SegKey(streamA, 0xffffffff), "8adbb0524a4203ec20a582ef0419aeebd1faad0b5cd5d6bf0ddd5a7da52384f5"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hex.EncodeToString(tt.got); got != tt.want {
				t.Errorf("got %s, want %s", got, tt.want)
			}
		})
	}
}

func TestKeyLength(t *testing.T) {
	for name, key := range map[string][]byte{
		"manifest": keyderiv.ManifestKey(secretA),
		"stream":   keyderiv.StreamKey(secretA),
		"seg":      keyderiv.SegKey(streamA, 7),
	} {
		if len(key) != keyderiv.KeyBytes {
			t.Errorf("%s: length %d, want %d", name, len(key), keyderiv.KeyBytes)
		}
	}
}

// 同一 readSecret 下三类密钥必须两两不同(label/段号做域分隔),
// 且同参数重复派生结果稳定(确定性是金标向量的前提)。
func TestDomainSeparationAndDeterminism(t *testing.T) {
	keys := [][]byte{
		keyderiv.ManifestKey(secretA),
		keyderiv.StreamKey(secretA),
		keyderiv.SegKey(streamA, 0),
		keyderiv.SegKey(streamA, 1),
		keyderiv.SegKey(streamA, 256), // 与 seg=1 校验 u32_be 编码不含字节序歧义
	}
	for i := range keys {
		for j := i + 1; j < len(keys); j++ {
			if bytes.Equal(keys[i], keys[j]) {
				t.Errorf("keys[%d] equals keys[%d]", i, j)
			}
		}
	}
	if !bytes.Equal(keyderiv.SegKey(streamA, 42), keyderiv.SegKey(streamA, 42)) {
		t.Error("repeated derivation with identical parameters differs")
	}
}
