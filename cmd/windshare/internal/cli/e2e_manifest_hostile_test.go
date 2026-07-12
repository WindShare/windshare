// T6.1 恶意清单验收(§6.13/B7):模拟"持链攻击者"手工构造加密清单——
// 发送端构建期校验(manifest.Seal 内 Validate)会拒绝这些路径,攻击者却可
// 拿链接密钥自行加密任意 entries。本文件用链接派生参数复刻这条攻击路径,
// 驱动接收门面 share.NewReceiver,断言下载开始前即被拒、输出根零落盘。
// 依据任务边界(执行计划 §11 T6.1),恶意路径类以构造清单直驱接收端,
// 不必经网络;网络路径上的清单原样透传由 relay/transport 测试另行覆盖。
package cli

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"errors"
	"os"
	"slices"
	"testing"

	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/manifest"
	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/core/share"
)

// hostileManifestLabel 复刻 §6.3 的清单密钥派生 info(HKDF-SHA256、空 salt)。
// 攻击者只持链接,派生参数是公开协议的一部分——测试在包外重独立实现,
// 恰证明"能解密清单的人也造不出绕过校验的清单"。
const hostileManifestLabel = "windshare/v1 manifest"

func hostileManifestKey(t *testing.T, readSecret []byte) []byte {
	t.Helper()
	key, err := hkdf.Key(sha256.New, readSecret, nil, hostileManifestLabel, 32)
	if err != nil {
		t.Fatalf("derive manifest key with HKDF: %v", err)
	}
	return key
}

// sealHostileManifest 产出攻击者手工加密的清单:先经合法路径 Seal 一份
// 等长占位清单,再解密、在 CBOR 明文里做同长度字节替换、重加密。文本串
// 长度头不变,严格解码与确定性重编码比对因此都能通过——恶意路径确实
// 抵达 Validate 这一层,而非提前死于编码差异(那样就测不到路径校验了)。
func sealHostileManifest(t *testing.T, readSecret []byte, m *manifest.Manifest, from, to string) []byte {
	t.Helper()
	if len(from) != len(to) {
		t.Fatalf("placeholder and malicious path must have equal length: %q (%d) vs %q (%d)", from, len(from), to, len(to))
	}
	key := hostileManifestKey(t, readSecret)
	sealed, err := manifest.Seal(key, m, bytes.NewReader(make([]byte, 32)))
	if err != nil {
		t.Fatalf("seal placeholder manifest: %v", err)
	}
	blk, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(blk)
	if err != nil {
		t.Fatal(err)
	}
	nonce, ct := sealed[:aead.NonceSize()], sealed[aead.NonceSize():]
	aad := []byte{link.SuiteAESGCM}
	plain, err := aead.Open(nil, nonce, ct, aad)
	if err != nil {
		t.Fatalf("decrypt placeholder manifest: %v", err)
	}
	if from != "" {
		needle := []byte(from)
		if n := bytes.Count(plain, needle); n != 1 {
			t.Fatalf("placeholder %q appears %d times in CBOR plaintext, want exactly once", from, n)
		}
		plain = bytes.Replace(plain, needle, []byte(to), 1)
	}
	return aead.Seal(slices.Clone(nonce), nonce, plain, aad)
}

// TestHostileManifestRejectedBeforeDownload 覆盖 §6.13 接收端拒绝矩阵:
// ../ 穿越、绝对路径、盘符、Windows 保留名、ADS 冒号、.wsresume 保留前缀、
// 非 NFC、结尾点、控制字符、大小写/Unicode 折叠碰撞、重复路径。
func TestHostileManifestRejectedBeforeDownload(t *testing.T) {
	secret := make([]byte, link.ReadSecretBytes)
	for i := range secret {
		secret[i] = byte(i + 1)
	}
	lnk := link.Link{Suite: link.SuiteAESGCM, ReadSecret: secret}
	entry := func(p string) manifest.Entry {
		return manifest.Entry{Path: p, Size: 8, MTime: 1_700_000_000_000}
	}

	cases := []struct {
		name     string
		entries  []manifest.Entry
		from, to string
		want     error // nil = 控制组:攻击者的加密外壳本身可被接收端解封
	}{
		{"控制组:未篡改可解封", []manifest.Entry{entry("ok.txt")}, "", "", nil},
		{"路径穿越 ..", []manifest.Entry{entry("zz/up.txt")}, "zz/up.txt", "../up.txt", manifest.ErrInvalidPath},
		{"绝对路径", []manifest.Entry{entry("zabs.txt")}, "zabs.txt", "/abs.txt", manifest.ErrInvalidPath},
		{"Windows 盘符", []manifest.Entry{entry("Cz/x.txt")}, "Cz/x.txt", "C:/x.txt", manifest.ErrInvalidPath},
		{"保留名 CON", []manifest.Entry{entry("XON.txt")}, "XON.txt", "CON.txt", manifest.ErrInvalidPath},
		{"ADS 冒号", []manifest.Entry{entry("aXb.txt")}, "aXb.txt", "a:b.txt", manifest.ErrInvalidPath},
		{"journal 保留前缀", []manifest.Entry{entry("Xwsresume-j")}, "Xwsresume-j", ".wsresume-j", manifest.ErrInvalidPath},
		// 目标路径是 café 的 NFD 分解形态(e ‖ U+0301 组合重音,共 6 字节)。
		{"非 NFC 形态", []manifest.Entry{entry("cafeXY")}, "cafeXY", "café", manifest.ErrInvalidPath},
		{"结尾点", []manifest.Entry{entry("dotX")}, "dotX", "dot.", manifest.ErrInvalidPath},
		{"控制字符", []manifest.Entry{entry("ctlQ.txt")}, "ctlQ.txt", "ctl\x01.txt", manifest.ErrInvalidPath},
		{"大小写折叠碰撞", []manifest.Entry{entry("Qa.txt"), entry("aa.txt")}, "Qa.txt", "Aa.txt", manifest.ErrPathCollision},
		// É(U+00C9)大小写折叠为 é(U+00E9),两条 NFC 路径折叠后同名。
		{"Unicode 折叠碰撞", []manifest.Entry{entry("Zq.txt"), entry("é.txt")}, "Zq.txt", "É.txt", manifest.ErrPathCollision},
		{"重复路径", []manifest.Entry{entry("qq.txt"), entry("aa.txt")}, "qq.txt", "aa.txt", manifest.ErrDuplicatePath},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := t.TempDir()
			sink, err := osfs.NewSink(out, osfs.SinkOptions{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = sink.Close() })
			sealed := sealHostileManifest(t, secret, manifest.New(1024, tc.entries), tc.from, tc.to)
			_, err = share.NewReceiver(lnk, sealed, sink)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("control case should unseal so rejection cases cannot pass due to a wrong key: %v", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("expected rejection by %v, got: %v", tc.want, err)
			}
			ents, rerr := os.ReadDir(out)
			if rerr != nil {
				t.Fatal(rerr)
			}
			if len(ents) != 0 {
				t.Errorf("rejection occurs before download; output root should be untouched: %v", ents)
			}
		})
	}
}
