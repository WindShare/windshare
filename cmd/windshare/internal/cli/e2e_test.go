// 进程内集成测试:真实 relay(signaling/httpapi + httptest)+ CLI share/get
// 全流程(§6.9),文件树走真实临时目录,哈希/字节级比对。
package cli

import (
	"bytes"
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/core/layout"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/share"
	"github.com/windshare/windshare/internal/testnetwork"
	"github.com/windshare/windshare/relay/httpapi"
	"github.com/windshare/windshare/relay/signaling"
	"github.com/windshare/windshare/transport/relay"
)

// waitTimeout 只防测试卡死,不承载时序语义。
const waitTimeout = 15 * time.Second

// testBlockSize 用小块让几 KiB 的树也有多块可续传/选择。
const testBlockSize = "1024"

func startRelayServer(t *testing.T) *httptest.Server {
	t.Helper()
	testnetwork.RequireOSNetwork(t)
	hub := signaling.NewHub(signaling.Config{})
	t.Cleanup(hub.Close)
	ts := httptest.NewServer(httpapi.NewHandler(httpapi.Config{Hub: hub}))
	t.Cleanup(ts.Close)
	return ts
}

// syncBuffer 供跨 goroutine 读写 CLI 输出。
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// shareProc 是跑在 goroutine 里的 `windshare share` 进程替身。
type shareProc struct {
	out, errB *syncBuffer
	cancel    context.CancelFunc
	exit      chan int
}

func startShare(t *testing.T, args ...string) *shareProc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	p := &shareProc{out: &syncBuffer{}, errB: &syncBuffer{}, cancel: cancel, exit: make(chan int, 1)}
	app := &App{Stdout: p.out, Stderr: p.errB, Stdin: strings.NewReader("")}
	go func() { p.exit <- app.Run(ctx, append([]string{"share"}, args...)) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-p.exit:
		case <-time.After(waitTimeout):
			t.Errorf("share did not exit after cancellation; stderr=%s", p.errB.String())
		}
	})
	return p
}

// stop 主动停止分享(等价 Ctrl-C)并返回退出码。
func (p *shareProc) stop(t *testing.T) int {
	t.Helper()
	p.cancel()
	return p.wait(t)
}

func (p *shareProc) wait(t *testing.T) int {
	t.Helper()
	select {
	case code := <-p.exit:
		p.exit <- code // 允许 cleanup 再读一次
		return code
	case <-time.After(waitTimeout):
		t.Fatalf("share did not exit; stderr=%s", p.errB.String())
		return -1
	}
}

// waitForPrefix 轮询 share 的 stdout,取以 prefix 开头那一行的余下内容。
func (p *shareProc) waitForPrefix(t *testing.T, prefix string) string {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		for line := range strings.SplitSeq(p.out.String(), "\n") {
			if rest, ok := strings.CutPrefix(line, prefix); ok {
				return strings.TrimSpace(rest)
			}
		}
		select {
		case code := <-p.exit:
			p.exit <- code
			t.Fatalf("share exited early with code %d; stderr=%s", code, p.errB.String())
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for output %q; stdout=%q stderr=%q", prefix, p.out.String(), p.errB.String())
	return ""
}

func writeFileT(t *testing.T, dir, rel string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// treeMTime 用整秒时间避开文件系统时间戳精度差异(清单存毫秒)。
var treeMTime = time.Now().Add(-time.Hour).Truncate(time.Second)

// makeTree 铺一棵覆盖各形态的树:多块文件、单块文件、空文件、空目录。
func makeTree(t *testing.T) (root string, files map[string][]byte) {
	t.Helper()
	base := t.TempDir()
	root = filepath.Join(base, "tree")
	bin := make([]byte, 3500)
	for i := range bin {
		bin[i] = byte(i*7 + 3)
	}
	files = map[string][]byte{
		"a.txt":     []byte("hello windshare CLI\n"),
		"empty.txt": {},
		"sub/b.bin": bin,
	}
	for rel, data := range files {
		writeFileT(t, root, rel, data)
	}
	if err := os.MkdirAll(filepath.Join(root, "sub", "emptydir"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 文件在先、目录自深向浅:后写子内容会刷新父目录 mtime。
	for rel := range files {
		chtimesT(t, filepath.Join(root, filepath.FromSlash(rel)))
	}
	chtimesT(t, filepath.Join(root, "sub", "emptydir"))
	chtimesT(t, filepath.Join(root, "sub"))
	chtimesT(t, root)
	return root, files
}

func chtimesT(t *testing.T, p string) {
	t.Helper()
	if err := os.Chtimes(p, time.Time{}, treeMTime); err != nil {
		t.Fatal(err)
	}
}

// verifyFile 校验内容与 mtime 双恢复(§6.6 收尾物化语义)。
func verifyFile(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read output %q: %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%q content mismatch: %d bytes vs %d bytes", path, len(got), len(want))
	}
	verifyMTime(t, path)
}

func verifyMTime(t *testing.T, path string) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.ModTime().UnixMilli() != treeMTime.UnixMilli() {
		t.Errorf("%q mtime was not restored: %v != %v", path, fi.ModTime(), treeMTime)
	}
}

func assertNoJournal(t *testing.T, dir string) {
	t.Helper()
	leftovers, err := filepath.Glob(filepath.Join(dir, journalNamePrefix+"*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(leftovers) != 0 {
		t.Errorf("journal was not removed after completion: %v", leftovers)
	}
}

func runGetCmd(t *testing.T, stdin string, args ...string) (int, string) {
	t.Helper()
	app, _, errBuf := newTestApp(stdin)
	app.RejoinWindow = 3 * time.Second
	code := app.Run(context.Background(), append([]string{"get"}, args...))
	return code, errBuf.String()
}

func TestShareGetEndToEnd(t *testing.T) {
	ts := startRelayServer(t)
	root, files := makeTree(t)
	sp := startShare(t, root, "--relay", ts.URL, "--block-size", testBlockSize)
	full := sp.waitForPrefix(t, "Link: ")
	lnk, err := link.Parse(full)
	if err != nil {
		t.Fatalf("share printed an unparseable link: %v (%q)", err, full)
	}
	if len(lnk.Relays) == 0 || lnk.Relays[0] != ts.URL {
		t.Fatalf("link should contain a ?r= relay hint, got %v", lnk.Relays)
	}

	t.Run("完整下载", func(t *testing.T) {
		out := t.TempDir()
		code, stderr := runGetCmd(t, "", full, "-o", out)
		if code != ExitOK {
			t.Fatalf("get exit code %d; stderr=%s", code, stderr)
		}
		for rel, data := range files {
			verifyFile(t, filepath.Join(out, "tree", filepath.FromSlash(rel)), data)
		}
		verifyMTime(t, filepath.Join(out, "tree", "sub", "emptydir"))
		verifyMTime(t, filepath.Join(out, "tree", "sub"))
		verifyMTime(t, filepath.Join(out, "tree"))
		assertNoJournal(t, out)
	})

	t.Run("选择性下载", func(t *testing.T) {
		out := t.TempDir()
		code, stderr := runGetCmd(t, "", full, "-o", out, "--only", "tree/a.txt")
		if code != ExitOK {
			t.Fatalf("get --only exit code %d; stderr=%s", code, stderr)
		}
		verifyFile(t, filepath.Join(out, "tree", "a.txt"), files["a.txt"])
		// a.txt shares its only packed-stream block with sub/b.bin. The boundary
		// bytes are authenticated, but no unselected file or empty entry may appear.
		for _, absent := range []string{"tree/empty.txt", "tree/sub/b.bin", "tree/sub/emptydir"} {
			if _, err := os.Stat(filepath.Join(out, filepath.FromSlash(absent))); err == nil {
				t.Errorf("unselected entry %s should not be materialized", absent)
			}
		}
		assertNoJournal(t, out)
	})

	t.Run("only未知路径", func(t *testing.T) {
		out := t.TempDir()
		code, stderr := runGetCmd(t, "", full, "-o", out, "--only", "tree/nope.txt")
		if code != ExitUsage {
			t.Fatalf("unknown --only path should return a usage error, got %d; stderr=%s", code, stderr)
		}
		if !strings.Contains(stderr, "not present") {
			t.Errorf("error message is unclear: %s", stderr)
		}
	})

	// 后续子测试需要清单指纹:直连中转取一次 sealedManifest。
	conn, err := relay.DialReceiver(t.Context(), relay.ReceiverConfig{RelayURL: ts.URL, ShareID: lnk.ShareID})
	if err != nil {
		t.Fatalf("DialReceiver: %v", err)
	}
	sealed := conn.SealedManifest()
	fp, err := manifestFingerprint(sealed)
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()

	t.Run("changing only rejects prior plan", func(t *testing.T) {
		out := t.TempDir()
		receiver, err := share.NewReceiver(lnk, sealed, journalFileSink{})
		if err != nil {
			t.Fatal(err)
		}
		oldPlan, err := receiver.Plan([]string{"tree/a.txt"})
		if err != nil {
			t.Fatal(err)
		}
		have := session.NewBitfield(oldPlan.Sink().Have().Len())
		for index := range oldPlan.Chunks().Iter() {
			have.Set(index)
			break // one shared boundary chunk is enough to prove it cannot cross plans
		}
		if err := writeJournal(journalPath(out, fp), journalState{
			fingerprint: fp,
			planID:      oldPlan.PlanID(),
			have:        have,
		}); err != nil {
			t.Fatal(err)
		}
		code, stderr := runGetCmd(t, "", full, "-o", out, "--only", "tree/sub/b.bin")
		if code != ExitUsage {
			t.Fatalf("changed --only returned %d; stderr=%s", code, stderr)
		}
		if !strings.Contains(stderr, "transfer plan mismatch") {
			t.Fatalf("changed --only error = %s", stderr)
		}
		if _, err := os.Stat(filepath.Join(out, "tree", "sub", "b.bin")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("changed plan materialized output: %v", err)
		}
	})

	t.Run("journal损坏", func(t *testing.T) {
		out := t.TempDir()
		jp := journalPath(out, fp)
		if err := os.WriteFile(jp, []byte("not a journal"), 0o644); err != nil {
			t.Fatal(err)
		}
		code, stderr := runGetCmd(t, "", full, "-o", out)
		if code != ExitUsage {
			t.Fatalf("corrupt journal should fail explicitly, got %d; stderr=%s", code, stderr)
		}
		if !strings.Contains(stderr, "corrupt") {
			t.Errorf("error message is unclear: %s", stderr)
		}
	})

	t.Run("journal指纹不符", func(t *testing.T) {
		out := t.TempDir()
		other := fp
		other[len(other)-1] ^= 0xff // 前缀同名、全指纹不同(跨分享错拼形态)
		if err := writeJournal(journalPath(out, fp), journalState{fingerprint: other, have: session.NewBitfield(4)}); err != nil {
			t.Fatal(err)
		}
		code, stderr := runGetCmd(t, "", full, "-o", out)
		if code != ExitUsage {
			t.Fatalf("fingerprint mismatch should fail explicitly, got %d; stderr=%s", code, stderr)
		}
		if !strings.Contains(stderr, "fingerprint mismatch") {
			t.Errorf("error message is unclear: %s", stderr)
		}
	})

	t.Run("断点续传", func(t *testing.T) {
		out := t.TempDir()
		// 用 core 原语先取前两块并写 journal,模拟"下了一半的进程"。
		pconn, err := relay.DialReceiver(t.Context(), relay.ReceiverConfig{RelayURL: ts.URL, ShareID: lnk.ShareID})
		if err != nil {
			t.Fatal(err)
		}
		journal, err := loadResume(journalPath(out, fp), fp)
		if err != nil {
			t.Fatal(err)
		}
		sink, err := osfs.NewSink(out, osfs.SinkOptions{Ownership: journal})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = sink.Close() })
		rcv, err := share.NewReceiver(lnk, pconn.SealedManifest(), sink)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := rcv.Plan(nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := journal.Bind(plan); err != nil {
			t.Fatal(err)
		}
		if err := journal.Checkpoint(); err != nil {
			t.Fatal(err)
		}
		firstTwo, err := layout.NewChunkSet(layout.ChunkRange{End: 2})
		if err != nil {
			t.Fatal(err)
		}
		sess, err := session.NewReceiveSession(firstTwo, plan.Sink(), rcv.Opener(), session.Options{MaxBlockBytes: rcv.MaxBlockBytes()})
		if err != nil {
			t.Fatal(err)
		}
		if err := sess.AddChannel(pconn.Channel()); err != nil {
			t.Fatal(err)
		}
		if err := sess.Run(t.Context()); err != nil {
			t.Fatalf("pre-download failed: %v", err)
		}
		pconn.Close()
		if err := journal.Checkpoint(); err != nil {
			t.Fatal(err)
		}
		if err := sink.Close(); err != nil {
			t.Fatal(err)
		}

		code, stderr := runGetCmd(t, "", full, "-o", out)
		if code != ExitOK {
			t.Fatalf("resumed get exit code %d; stderr=%s", code, stderr)
		}
		if !strings.Contains(stderr, "resume state found: 2/4 selected blocks already present") {
			t.Errorf("download should resume from the journal; stderr=%s", stderr)
		}
		for rel, data := range files {
			verifyFile(t, filepath.Join(out, "tree", filepath.FromSlash(rel)), data)
		}
		assertNoJournal(t, out)
	})

	if code := sp.stop(t); code != ExitOK {
		t.Errorf("Ctrl-C should stop sharing with exit code 0, got %d; stderr=%s", code, sp.errB.String())
	}
}

func TestSplitKeyMatrix(t *testing.T) {
	ts := startRelayServer(t)
	dir := t.TempDir()
	content := []byte("split key payload")
	writeFileT(t, dir, "hello.txt", content)
	chtimesT(t, filepath.Join(dir, "hello.txt"))

	sp := startShare(t, filepath.Join(dir, "hello.txt"), "--relay", ts.URL, "--split-key")
	bare := sp.waitForPrefix(t, "Bare link: ")
	key := sp.waitForPrefix(t, "Key: ")
	if strings.Contains(bare, "#") {
		t.Fatalf("bare link should not contain a fragment: %q", bare)
	}
	if _, err := link.Merge(bare, key); err != nil {
		t.Fatalf("bare link and key should merge: %v", err)
	}

	t.Run("key参数", func(t *testing.T) {
		out := t.TempDir()
		code, stderr := runGetCmd(t, "", bare, "--key", key, "-o", out)
		if code != ExitOK {
			t.Fatalf("get --key exit code %d; stderr=%s", code, stderr)
		}
		verifyFile(t, filepath.Join(out, "hello.txt"), content)
	})

	t.Run("交互输入密钥", func(t *testing.T) {
		out := t.TempDir()
		code, stderr := runGetCmd(t, key+"\n", bare, "-o", out)
		if code != ExitOK {
			t.Fatalf("interactive-key get exit code %d; stderr=%s", code, stderr)
		}
		if !strings.Contains(stderr, "enter the key string") {
			t.Errorf("interactive prompt is missing; stderr=%s", stderr)
		}
		verifyFile(t, filepath.Join(out, "hello.txt"), content)
	})

	t.Run("合并回完整链接", func(t *testing.T) {
		merged, err := link.Merge(bare, key)
		if err != nil {
			t.Fatal(err)
		}
		full, err := merged.URL(DefaultFrontURL)
		if err != nil {
			t.Fatal(err)
		}
		out := t.TempDir()
		code, stderr := runGetCmd(t, "", full, "-o", out)
		if code != ExitOK {
			t.Fatalf("merged-link get exit code %d; stderr=%s", code, stderr)
		}
		verifyFile(t, filepath.Join(out, "hello.txt"), content)
	})

	if code := sp.stop(t); code != ExitOK {
		t.Errorf("stopping share should return exit code 0, got %d", code)
	}
}

// TestRejoinAfterConnLoss 验证传输中断的 rejoin(§6.12):第一条接收连接
// 拆除后,ReceiverRecovery 重拨新连接(新 sessionId/新通道)热入池到同一会话,
// 需求集与位图保留,下载最终完成。连接在起传前拆除,时序完全确定。
func TestRejoinAfterConnLoss(t *testing.T) {
	ts := startRelayServer(t)
	root, files := makeTree(t)
	sp := startShare(t, root, "--relay", ts.URL, "--block-size", testBlockSize)
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
	// 起传前拆除首条连接:调度器把在途退回需求池,rejoin 后经新通道重派。
	conn.Close()

	app, _, errBuf := newTestApp("")
	app.RejoinWindow = waitTimeout
	if err := app.runReceiveRecovery(t.Context(), sess, conn, cfg, fp); err != nil {
		t.Fatalf("download should complete after rejoin: %v; stderr=%s", err, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "reconnected; continuing download") {
		t.Errorf("expected a reconnect; stderr=%s", errBuf.String())
	}
	// 未 Finalize:只校验占流内容(空文件/空目录/mtime 属收尾物化,另有覆盖)。
	for _, rel := range []string{"a.txt", "sub/b.bin"} {
		got, err := os.ReadFile(filepath.Join(out, "tree", filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, files[rel]) {
			t.Errorf("%s content mismatch", rel)
		}
	}
	if code := sp.stop(t); code != ExitOK {
		t.Errorf("stopping share should return exit code 0, got %d", code)
	}
}

// TestDriftAbort 验证漂移中止(§6.3):分享期间文件被改,发送端读后复核
// 发现漂移 → 进程以 ExitDrift 退出并提示重新分享。
func TestDriftAbort(t *testing.T) {
	ts := startRelayServer(t)
	dir := t.TempDir()
	payload := make([]byte, 3000)
	p := writeFileT(t, dir, "vol.txt", payload)
	chtimesT(t, p)

	sp := startShare(t, p, "--relay", ts.URL, "--block-size", testBlockSize)
	full := sp.waitForPrefix(t, "Link: ")

	// 注册在快照之后:改 mtime 即制造漂移。
	if err := os.Chtimes(p, time.Time{}, treeMTime.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	out := t.TempDir()
	code, stderr := runGetCmd(t, "", full, "-o", out)
	// SendTerminal and the relay terminal-forward envelope make the sender's
	// domain failure observable before session teardown; a network fallback here
	// would regress the accepted A3 lifecycle contract.
	if code != ExitDrift {
		t.Fatalf("get under drift should exit with %d, got %d; stderr=%s", ExitDrift, code, stderr)
	}
	t.Logf("get terminal drift exit code %d", code)

	if shareCode := sp.wait(t); shareCode != ExitDrift {
		t.Fatalf("share aborted by drift should exit with %d, got %d; stderr=%s", ExitDrift, shareCode, sp.errB.String())
	}
	if !strings.Contains(sp.errB.String(), "source file changed; create a new share") {
		t.Errorf("share output lacks a drift explanation; stderr=%s", sp.errB.String())
	}
}
