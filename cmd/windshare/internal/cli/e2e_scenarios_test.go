// T6.1 验收场景补全(执行计划 §11 M1a 验收门):子目录选择性下载、
// 空文件·空目录物化、已存在同名拒绝、跨进程重启续传、篡改块拒收、
// 发送端断线重连续传。全部经真实 relay(进程内 httptest)+ CLI share/get
// 或其会话引擎驱动;基础闭环/单文件选择/预置 journal 续传/漂移中止见
// e2e_test.go,恶意清单构造见 e2e_manifest_hostile_test.go。
package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/share"
	"github.com/windshare/windshare/internal/testnetwork"
	"github.com/windshare/windshare/relay/protocol"
	"github.com/windshare/windshare/transport/relay"
)

// patternBytes 生成按 seed 区分的确定性内容:文件间字节互不相同,
// 块错拼/错位在逐字节比对下立现。
func patternBytes(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i)*seed + seed
	}
	return b
}

// verifyFileSHA256 按验收门口径(哈希一致)逐文件比对 SHA-256,并复核
// mtime 恢复(§6.6 收尾物化语义)。
func verifyFileSHA256(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read output %q: %v", path, err)
	}
	if sha256.Sum256(got) != sha256.Sum256(want) {
		t.Fatalf("%q SHA-256 differs from source (%d bytes vs %d bytes)", path, len(got), len(want))
	}
	verifyMTime(t, path)
}

// TestGetOnlySubdirectory 验证 --only 选中目录子树(§6.9):子树全量物化
// (含其内空文件/空目录),树外条目不落盘。
//
// 子树总字节数故意不按块对齐,其末块与 top.txt 共享。边界块整体认证,
// 但 TransferPlan 只允许选中 ranges 物化。
func TestGetOnlySubdirectory(t *testing.T) {
	ts := startRelayServer(t)
	root := filepath.Join(t.TempDir(), "tree")
	topData := patternBytes(1024, 3)
	innerData := patternBytes(1500, 5)
	leafData := patternBytes(700, 7)
	writeFileT(t, root, "top.txt", topData)
	writeFileT(t, root, "sub/inner.bin", innerData)
	writeFileT(t, root, "sub/deep/leaf.txt", leafData)
	writeFileT(t, root, "sub/zero.txt", nil)
	if err := os.MkdirAll(filepath.Join(root, "sub", "emptydir"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 文件在先、目录自深向浅(同 makeTree:后写子内容会刷新父目录 mtime)。
	for _, rel := range []string{
		"top.txt", "sub/inner.bin", "sub/deep/leaf.txt", "sub/zero.txt",
		"sub/emptydir", "sub/deep", "sub", ".",
	} {
		chtimesT(t, filepath.Join(root, filepath.FromSlash(rel)))
	}

	sp := startShare(t, root, "--relay", ts.URL, "--block-size", testBlockSize)
	full := sp.waitForPrefix(t, "Link: ")

	out := t.TempDir()
	code, stderr := runGetCmd(t, "", full, "-o", out, "--only", "tree/sub")
	if code != ExitOK {
		t.Fatalf("get --only subdirectory exit code %d; stderr=%s", code, stderr)
	}
	verifyFileSHA256(t, filepath.Join(out, "tree", "sub", "inner.bin"), innerData)
	verifyFileSHA256(t, filepath.Join(out, "tree", "sub", "deep", "leaf.txt"), leafData)
	verifyFileSHA256(t, filepath.Join(out, "tree", "sub", "zero.txt"), nil)
	fi, err := os.Stat(filepath.Join(out, "tree", "sub", "emptydir"))
	if err != nil || !fi.IsDir() {
		t.Errorf("empty directory inside selected subtree should be materialized: %v", err)
	}
	verifyMTime(t, filepath.Join(out, "tree", "sub", "emptydir"))
	verifyMTime(t, filepath.Join(out, "tree", "sub"))
	if _, err := os.Stat(filepath.Join(out, "tree", "top.txt")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("unselected top.txt should not be written (stat err=%v)", err)
	}
	assertNoJournal(t, out)

	if code := sp.stop(t); code != ExitOK {
		t.Errorf("stopping share should return exit code 0, got %d", code)
	}
}

// TestEmptyOnlyShareMaterialization 验证空文件·空目录物化(§6.6 收尾)在
// 零块流这一极端几何下成立:整份分享不占流(NumChunks=0),没有任何数据
// 面帧往来,get 仍须创建空文件与空目录、恢复 mtime 并正常收尾。
func TestEmptyOnlyShareMaterialization(t *testing.T) {
	ts := startRelayServer(t)
	root := filepath.Join(t.TempDir(), "voidtree")
	writeFileT(t, root, "empty.txt", nil)
	if err := os.MkdirAll(filepath.Join(root, "hollow"), 0o755); err != nil {
		t.Fatal(err)
	}
	chtimesT(t, filepath.Join(root, "empty.txt"))
	chtimesT(t, filepath.Join(root, "hollow"))
	chtimesT(t, root)

	sp := startShare(t, root, "--relay", ts.URL)
	full := sp.waitForPrefix(t, "Link: ")

	out := t.TempDir()
	code, stderr := runGetCmd(t, "", full, "-o", out)
	if code != ExitOK {
		t.Fatalf("zero-block share get exit code %d; stderr=%s", code, stderr)
	}
	verifyFile(t, filepath.Join(out, "voidtree", "empty.txt"), nil)
	fi, err := os.Stat(filepath.Join(out, "voidtree", "hollow"))
	if err != nil || !fi.IsDir() {
		t.Fatalf("empty directory should be materialized: %v", err)
	}
	verifyMTime(t, filepath.Join(out, "voidtree", "hollow"))
	verifyMTime(t, filepath.Join(out, "voidtree"))
	assertNoJournal(t, out)

	if code := sp.stop(t); code != ExitOK {
		t.Errorf("stopping share should return exit code 0, got %d", code)
	}
}

// TestExistingFileRefusedNoOverwrite 验证已存在同名文件、非续传 → 拒绝且
// 不动用户数据(§6.13:不静默覆盖,明确报错)。
func TestExistingFileRefusedNoOverwrite(t *testing.T) {
	ts := startRelayServer(t)
	dir := t.TempDir()
	srcData := patternBytes(2048, 9)
	p := writeFileT(t, dir, "keep.txt", srcData)
	chtimesT(t, p)

	sp := startShare(t, p, "--relay", ts.URL, "--block-size", testBlockSize)
	full := sp.waitForPrefix(t, "Link: ")

	out := t.TempDir()
	precious := []byte("user's precious data, do not touch")
	writeFileT(t, out, "keep.txt", precious)

	for attempt := 1; attempt <= 2; attempt++ {
		code, stderr := runGetCmd(t, "", full, "-o", out)
		if code != ExitUsage {
			t.Fatalf("attempt %d: existing file should return %d, got %d; stderr=%s", attempt, ExitUsage, code, stderr)
		}
		if !strings.Contains(stderr, "will not overwrite") {
			t.Errorf("attempt %d: error should explain overwrite refusal; stderr=%s", attempt, stderr)
		}
		got, err := os.ReadFile(filepath.Join(out, "keep.txt"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, precious) {
			t.Fatalf("attempt %d changed the existing file: %q", attempt, got)
		}
		assertNoJournal(t, out)
	}

	if code := sp.stop(t); code != ExitOK {
		t.Errorf("stopping share should return exit code 0, got %d", code)
	}
}

// TestCrossProcessRestartResume 验证跨进程重启续传(§6.12):首次 get 中途
// 诚实失败(撞上同名文件保护),真实错误路径落下 journal;按错误提示移走
// 同名文件后,新一次 get(全新 App = 进程替身)凭 journal 只补缺失块,
// 最终逐文件 SHA-256 与源一致、journal 删除。
//
// 用"末序文件预置同名"制造确定性的中途失败:zz.txt 排在流末,其首块写入
// 必然失败,此前文件的块已按真实下载路径落盘并进入位图。
func TestCrossProcessRestartResume(t *testing.T) {
	ts := startRelayServer(t)
	root := filepath.Join(t.TempDir(), "tree")
	aData := patternBytes(2048, 3) // 块 0–1
	mData := patternBytes(4096, 5) // 块 2–5
	zData := patternBytes(2048, 7) // 块 6–7(流末)
	writeFileT(t, root, "a.txt", aData)
	writeFileT(t, root, "m.bin", mData)
	writeFileT(t, root, "zz.txt", zData)
	for _, rel := range []string{"a.txt", "m.bin", "zz.txt", "."} {
		chtimesT(t, filepath.Join(root, rel))
	}

	sp := startShare(t, root, "--relay", ts.URL, "--block-size", testBlockSize)
	full := sp.waitForPrefix(t, "Link: ")

	out := t.TempDir()
	writeFileT(t, out, "tree/zz.txt", []byte("pre-existing"))

	code, stderr := runGetCmd(t, "", full, "-o", out)
	if code != ExitUsage {
		t.Fatalf("first get against an existing file should fail with %d, got %d; stderr=%s", ExitUsage, code, stderr)
	}
	if !strings.Contains(stderr, "will not overwrite") {
		t.Errorf("error should identify the existing path; stderr=%s", stderr)
	}
	journals, err := filepath.Glob(filepath.Join(out, journalNamePrefix+"*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(journals) != 1 {
		t.Fatalf("interruption should leave exactly one resume journal, got %v", journals)
	}

	// 按提示"移走同名文件"后,以全新 App 实例重启下载。
	if err := os.Remove(filepath.Join(out, "tree", "zz.txt")); err != nil {
		t.Fatal(err)
	}
	code, stderr = runGetCmd(t, "", full, "-o", out)
	if code != ExitOK {
		t.Fatalf("restart-resume exit code %d; stderr=%s", code, stderr)
	}
	if !strings.Contains(stderr, "resume state found") {
		t.Errorf("download should resume from the journal; stderr=%s", stderr)
	}
	verifyFileSHA256(t, filepath.Join(out, "tree", "a.txt"), aData)
	verifyFileSHA256(t, filepath.Join(out, "tree", "m.bin"), mData)
	verifyFileSHA256(t, filepath.Join(out, "tree", "zz.txt"), zData)
	assertNoJournal(t, out)

	if code := sp.stop(t); code != ExitOK {
		t.Errorf("stopping share should return exit code 0, got %d", code)
	}
}

// memSource 是内存版 share.FileSource:恶意发送端不必依赖真实文件系统。
type memSource map[string][]byte

func (s memSource) ReadRange(path string, off, n int64) ([]byte, error) {
	data, ok := s[path]
	if !ok || off < 0 || off+n > int64(len(data)) {
		return nil, fmt.Errorf("memSource: out-of-range read %q [%d,+%d)", path, off, n)
	}
	return data[off : off+n], nil
}

// serveCorruptBlocks 扮演恶意/故障发送端:对每个 REQUEST 回以尾字节翻转的
// 密文块——GCM tag 被破坏,密文结构完好,接收端只能靠 AEAD 验证识破。
func serveCorruptBlocks(ctx context.Context, sharer *share.Sharer, ch *relay.Channel) {
	for f := range ch.Recv() {
		msg, err := session.Decode(f)
		if err != nil {
			return
		}
		req, ok := msg.(*session.Request)
		if !ok {
			continue
		}
		for _, idx := range req.Indices {
			ct, err := sharer.Chunk(idx)
			if err != nil {
				return
			}
			ct[len(ct)-1] ^= 0x01
			frames, err := session.SplitBlockCT(idx, ct, session.MaxBlockPayload)
			if err != nil {
				return
			}
			for _, bf := range frames {
				if ch.Send(ctx, bf) != nil {
					return
				}
			}
		}
	}
}

// TestTamperedBlockRejected 验证篡改块经真实中转投递给真实 CLI get 时被
// AEAD 拒收(§6.5):重试耗尽后以网络类退出码诚实失败,损坏数据从未落盘
// (输出文件根本不存在)。
func TestTamperedBlockRejected(t *testing.T) {
	ts := startRelayServer(t)
	payload := patternBytes(2048, 11)
	src := memSource{"t.bin": payload}
	sharer, err := share.NewSharer(
		[]share.FileMeta{{Path: "t.bin", Size: int64(len(payload)), MTime: treeMTime.UnixMilli()}},
		src, share.Options{ChunkSize: 1024})
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := sharer.SealedManifest()
	if err != nil {
		t.Fatal(err)
	}
	token := make([]byte, protocol.ResumeTokenBytes)
	conn, err := relay.DialSender(t.Context(), relay.SenderConfig{
		RelayURL:       ts.URL,
		ShareID:        sharer.Link().ShareID,
		SealedManifest: sealed,
		ResumeToken:    token,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	go func() {
		for ch := range conn.Sessions() {
			go serveCorruptBlocks(t.Context(), sharer, ch)
		}
	}()

	lnk := sharer.Link()
	lnk.Relays = []string{ts.URL}
	full, err := lnk.URL(DefaultFrontURL)
	if err != nil {
		t.Fatal(err)
	}

	out := t.TempDir()
	code, stderr := runGetCmd(t, "", full, "-o", out)
	if code != ExitNetwork {
		t.Fatalf("tampered-block retry exhaustion should exit with %d, got %d; stderr=%s", ExitNetwork, code, stderr)
	}
	if !strings.Contains(stderr, "authentication failed") {
		t.Errorf("failure should identify authentication failure; stderr=%s", stderr)
	}
	if _, err := os.Stat(filepath.Join(out, "t.bin")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("corrupt block must not be written; t.bin should not exist (stat err=%v)", err)
	}
}

// tcpProxy 是可拆断的 TCP 转发器:severAll 掐断全部现存连接(链路故障
// 注入),新连接始终放行——"断"与"重连可达"由测试精确控制。
type tcpProxy struct {
	ln     net.Listener
	target string

	mu    sync.Mutex
	conns map[net.Conn]struct{}
}

func startTCPProxy(t *testing.T, target string) *tcpProxy {
	t.Helper()
	testnetwork.RequireOSNetwork(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &tcpProxy{ln: ln, target: target, conns: make(map[net.Conn]struct{})}
	go p.acceptLoop()
	t.Cleanup(func() {
		_ = ln.Close()
		p.severAll()
	})
	return p
}

func (p *tcpProxy) url() string { return "ws://" + p.ln.Addr().String() }

func (p *tcpProxy) acceptLoop() {
	testnetwork.AssertOSNetwork()
	for {
		down, err := p.ln.Accept()
		if err != nil {
			return
		}
		up, err := net.Dial("tcp", p.target)
		if err != nil {
			_ = down.Close()
			continue
		}
		p.mu.Lock()
		p.conns[down] = struct{}{}
		p.conns[up] = struct{}{}
		p.mu.Unlock()
		go proxyHalf(down, up)
		go proxyHalf(up, down)
	}
}

// proxyHalf 单向搬运;任一方向断开即拆整条链(WS 是全双工长连接,半开
// 状态对被代理的双方没有意义)。
func proxyHalf(dst, src net.Conn) {
	_, _ = io.Copy(dst, src)
	_ = dst.Close()
	_ = src.Close()
}

func (p *tcpProxy) severAll() {
	p.mu.Lock()
	conns := make([]net.Conn, 0, len(p.conns))
	for c := range p.conns {
		conns = append(conns, c)
	}
	clear(p.conns)
	p.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

// TestSenderLinkLossReconnectResume 验证发送端断线重连续传(§6.8/§6.12):
// 发送端(真实 CLI share)经可拆代理接中转;拆链后中转向接收会话合成
// sender_gone,发送端凭 resumeToken 原像在宽限内经代理重注册;接收端
// rejoin 到同一接收会话(需求集/位图保留),下载完成且逐文件 SHA-256
// 与源一致。接收端直连真实中转,链路故障只注入发送端一侧。
func TestSenderLinkLossReconnectResume(t *testing.T) {
	ts := startRelayServer(t)
	proxy := startTCPProxy(t, strings.TrimPrefix(ts.URL, "http://"))
	root, files := makeTree(t)
	sp := startShare(t, root, "--relay", proxy.url(), "--block-size", testBlockSize)
	full := sp.waitForPrefix(t, "Link: ")
	lnk, err := link.Parse(full)
	if err != nil {
		t.Fatal(err)
	}

	cfg := relay.ReceiverConfig{RelayURL: ts.URL, ShareID: lnk.ShareID}
	conn, err := relay.DialReceiver(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	sealed := conn.SealedManifest()
	fp, err := manifestFingerprint(sealed)
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	sink, err := osfs.NewSink(out, osfs.SinkOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sink.Close() })
	rcv, err := share.NewReceiver(lnk, sealed, sink)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := rcv.Plan(nil)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := session.NewReceiveSession(plan.Chunks(), plan.Sink(), rcv.Opener(), session.Options{MaxBlockBytes: rcv.MaxBlockBytes()})
	if err != nil {
		t.Fatal(err)
	}
	// 起传前拆发送端链路:首次重注册可能撞上中转尚未回收的旧连接
	// (collision),发送端的退避重试正是被测行为的一部分(§6.8)。
	proxy.severAll()

	app, _, errBuf := newTestApp("")
	app.RejoinWindow = waitTimeout
	if err := app.runReceiveRecovery(t.Context(), sess, conn, cfg, fp); err != nil {
		t.Fatalf("download should complete after sender reconnect: %v; get stderr=%s; share stderr=%s",
			err, errBuf.String(), sp.errB.String())
	}
	if !strings.Contains(errBuf.String(), "reconnected; continuing download") {
		t.Errorf("receiver should rejoin; stderr=%s", errBuf.String())
	}
	// 下载完成蕴含发送端已重注册;日志写入与本读取跨 goroutine,轮询兜底。
	deadline := time.Now().Add(waitTimeout)
	for !strings.Contains(sp.errB.String(), "sender reconnected") {
		if time.Now().After(deadline) {
			t.Errorf("sender should re-register during the grace period; stderr=%s", sp.errB.String())
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := plan.Finalize(); err != nil {
		t.Fatalf("finalize materialization: %v", err)
	}
	for rel, data := range files {
		verifyFileSHA256(t, filepath.Join(out, "tree", filepath.FromSlash(rel)), data)
	}
	if code := sp.stop(t); code != ExitOK {
		t.Errorf("stopping share should return exit code 0, got %d; stderr=%s", code, sp.errB.String())
	}
}
