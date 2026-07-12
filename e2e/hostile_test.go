// 恶意发送端:黑盒发不出这些清单(发送端 osfs.Walk/manifest.Seal 的 Validate
// 会先拒绝),故按 §T6.1 用 core 类型手工构造恶意清单并以与 core/manifest 同款
// 的确定性 CBOR + 正确 manifestKey 封装——使其能过接收端的解密与 canonical
// 重编码,恰在接收端结构校验(§6.13 纵深防御)处被拒。逐条断言拒绝且零文件落盘。
package e2e

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"

	"github.com/windshare/windshare/core/layout"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/manifest"
	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/core/share"
	"github.com/windshare/windshare/relay/protocol"
	"github.com/windshare/windshare/transport/relay"
)

// manifestKeyLabel 镜像 core/internal/keyderiv 的 HKDF label(该包 internal,
// 不可导入;恶意端本就自行派生,复制常量是恰当的)。
const manifestKeyLabel = "windshare/v1 manifest"

// detEnc 镜像 core/manifest 的确定性 CBOR 编码(RFC 8949 Core Deterministic +
// nil 容器编空数组):唯有逐字节同款,接收端的重编码对拍才不会先把恶意清单挡下。
var detEnc = func() cbor.EncMode {
	opts := cbor.CoreDetEncOptions()
	opts.NilContainers = cbor.NilContainerAsEmpty
	em, err := opts.EncMode()
	if err != nil {
		panic(err)
	}
	return em
}()

// sealMalicious 以正确的 manifestKey 封装一份恶意清单:HKDF(readSecret,label,32)
// → AES-256-GCM(aad=suiteByte),nonce 前置——与 core/manifest.Seal 的线格式一致,
// 只是绕过了其 Validate 前置(恶意端不受本端约束)。
func sealMalicious(t *testing.T, readSecret []byte, m *manifest.Manifest) []byte {
	t.Helper()
	key, err := hkdf.Key(sha256.New, readSecret, nil, manifestKeyLabel, 32)
	if err != nil {
		t.Fatal(err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := detEnc.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	return gcm.Seal(nonce, nonce, plain, []byte{link.SuiteAESGCM})
}

func hostileEntry(path string, size int64) manifest.Entry {
	return manifest.Entry{Path: path, Size: size, MTime: 0, IsDir: false}
}

func hostileManifest(entries ...manifest.Entry) *manifest.Manifest {
	return &manifest.Manifest{Version: manifest.CurrentVersion, ChunkSize: e2eBlockSizeInt, Entries: entries}
}

// maxStreamOverflow 是刚越过布局协议上限的 size,避免测试常量与生产几何漂移。
const maxStreamOverflow = layout.MaxStreamBytes + 1

type hostileCase struct {
	name    string
	m       *manifest.Manifest
	wantErr error
}

func hostileCases() []hostileCase {
	return []hostileCase{
		{"路径穿越", hostileManifest(hostileEntry("../evil.txt", 4)), manifest.ErrInvalidPath},
		{"绝对路径", hostileManifest(hostileEntry("/etc/passwd", 4)), manifest.ErrInvalidPath},
		{"Windows保留名", hostileManifest(hostileEntry("CON", 4)), manifest.ErrInvalidPath},
		{"反斜杠分隔符", hostileManifest(hostileEntry("a\\b.txt", 4)), manifest.ErrInvalidPath},
		{"ADS冒号", hostileManifest(hostileEntry("file.txt:stream", 4)), manifest.ErrInvalidPath},
		{"大小写折叠碰撞", hostileManifest(hostileEntry("README", 4), hostileEntry("readme", 4)), manifest.ErrPathCollision},
		{"负size", hostileManifest(hostileEntry("neg.bin", -1)), manifest.ErrNegativeSize},
		{"前缀和越限", hostileManifest(hostileEntry("huge.bin", maxStreamOverflow)), manifest.ErrStreamTooLarge},
		{"wsresume前缀", hostileManifest(hostileEntry(".wsresume-forged", 4)), manifest.ErrInvalidPath},
	}
}

// TestHostileManifestRejectedLibrary:库级 Receiver 流程——每条恶意清单经解密与
// canonical 解码后,在接收端结构校验处被明确拒绝(errors.Is 命中具体类别),
// 且零文件落盘(§6.13)。
func TestHostileManifestRejectedLibrary(t *testing.T) {
	t.Parallel()
	readSecret := make([]byte, link.ReadSecretBytes)
	for i := range readSecret {
		readSecret[i] = byte(i + 1)
	}
	shareID, err := link.NewShareID(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	lnk := link.Link{Suite: link.SuiteAESGCM, ReadSecret: readSecret, ShareID: shareID}

	for _, tc := range hostileCases() {
		t.Run(tc.name, func(t *testing.T) {
			out := t.TempDir()
			sink, err := osfs.NewSink(out, osfs.SinkOptions{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = sink.Close() })
			sealed := sealMalicious(t, readSecret, tc.m)
			_, err = share.NewReceiver(lnk, sealed, sink)
			if err == nil {
				t.Fatalf("malicious manifest %q should be rejected", tc.name)
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("rejection category mismatch: err=%v, want errors.Is %v", err, tc.wantErr)
			}
			if n := dirEntryCount(t, out); n != 0 {
				t.Errorf("malicious manifest %q should write no files; output contains %d entries", tc.name, n)
			}
		})
	}
}

// TestHostileManifestRejectedBlackBox:恶意清单经真实中转注册,真实 get 进程
// 拉取后在接收端拒绝(用户错误退出 2),零文件落盘。取一条代表性用例(穿越),
// 端到端印证黑盒路径同样闭合。
func TestHostileManifestRejectedBlackBox(t *testing.T) {
	t.Parallel()
	relayURL, _ := startRelay(t)

	readSecret := make([]byte, link.ReadSecretBytes)
	for i := range readSecret {
		readSecret[i] = byte(0xA0 + i)
	}
	shareID, err := link.NewShareID(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sealed := sealMalicious(t, readSecret, hostileManifest(hostileEntry("../escape.txt", 8)))

	// 恶意端经真实 transport 注册任意字节清单(中转是哑管道,不校验内容)。
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	token := make([]byte, protocol.ResumeTokenBytes)
	if _, err := rand.Read(token); err != nil {
		t.Fatal(err)
	}
	sconn, err := relay.DialSender(ctx, relay.SenderConfig{
		RelayURL:       relayURL,
		ShareID:        shareID,
		SealedManifest: sealed,
		ResumeToken:    token,
	})
	if err != nil {
		t.Fatalf("malicious sender registration failed: %v", err)
	}
	defer sconn.Close()

	lnk := link.Link{Suite: link.SuiteAESGCM, ReadSecret: readSecret, ShareID: shareID, Relays: []string{relayURL}}
	full, err := lnk.URL("http://localhost:5173")
	if err != nil {
		t.Fatal(err)
	}

	out := t.TempDir()
	code, _, stderr := runGet(t, getTimeout, full, "-o", out)
	if code != 2 {
		t.Fatalf("get with malicious manifest should exit with 2 (usage error), got %d; stderr=%s", code, stderr)
	}
	if n := dirEntryCount(t, out); n != 0 {
		t.Errorf("malicious manifest should write no files; output contains %d entries", n)
	}
}
