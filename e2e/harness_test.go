package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/internal/testnetwork"
	"github.com/windshare/windshare/relay/httpapi"
	"github.com/windshare/windshare/relay/signaling"
)

// procIOTimeout 是等待进程产出(链接行/监听行)的上限;只防卡死,不承载时序。
const procIOTimeout = 20 * time.Second

// capBuf 是并发安全的输出缓冲:进程 goroutine 写、测试 goroutine 轮询读。
type capBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *capBuf) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *capBuf) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

// proc 是一个受管子进程:stdout/stderr 全程捕获,退出经 finished 广播一次。
type proc struct {
	name     string
	cmd      *exec.Cmd
	stdout   *capBuf
	stderr   *capBuf
	exitErr  error
	finished chan struct{}
}

// startCmd 启动一个子进程并登记 Cleanup 兜底清理(用例正常路径应显式收尾)。
func startCmd(t *testing.T, name, bin string, args ...string) *proc {
	t.Helper()
	testnetwork.RequireOSNetwork(t)
	p := &proc{name: name, stdout: &capBuf{}, stderr: &capBuf{}, finished: make(chan struct{})}
	p.cmd = exec.Command(bin, args...)
	p.cmd.Stdout = p.stdout
	p.cmd.Stderr = p.stderr
	if err := testnetwork.StartGuardedProcess(p.cmd); err != nil {
		t.Fatalf("start %s: %v", name, err)
	}
	go func() {
		p.exitErr = p.cmd.Wait()
		close(p.finished)
	}()
	t.Cleanup(p.kill)
	return p
}

// kill 强制结束进程并等回收(幂等)。黑盒用例的 share 进程无优雅 Ctrl-C
// 通道(Windows 不便向子进程投递 SIGINT),测试收尾一律 Kill;需要自然退出
// 码的用例(漂移)改用 waitExit 等其自行结束。
func (p *proc) kill() {
	select {
	case <-p.finished:
		return
	default:
	}
	_ = p.cmd.Process.Kill()
	<-p.finished
}

// waitExit 等进程在 timeout 内自行退出并返回退出码;超时即判定挂死。
func (p *proc) waitExit(t *testing.T, timeout time.Duration) int {
	t.Helper()
	select {
	case <-p.finished:
		return exitCode(p.exitErr)
	case <-time.After(timeout):
		p.kill()
		t.Fatalf("%s did not exit within %v; stderr=%s", p.name, timeout, p.stderr.String())
		return -1
	}
}

// exited 报告进程是否已退出(不阻塞)。
func (p *proc) exited() bool {
	select {
	case <-p.finished:
		return true
	default:
		return false
	}
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		return ee.ExitCode()
	}
	return -1
}

// waitLine 轮询 stdout,取以 prefix 开头那一行的余下内容(share 的链接走
// stdout,是机器可读产物)。进程提前退出即失败并附 stderr 便于定位。
func (p *proc) waitLine(t *testing.T, prefix string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for line := range strings.SplitSeq(p.stdout.String(), "\n") {
			if rest, ok := strings.CutPrefix(line, prefix); ok {
				return strings.TrimSpace(rest)
			}
		}
		if p.exited() {
			t.Fatalf("%s exited with code %d before producing %q; stderr=%s", p.name, exitCode(p.exitErr), prefix, p.stderr.String())
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s output %q; stdout=%q stderr=%q", p.name, prefix, p.stdout.String(), p.stderr.String())
	return ""
}

// relayListenRE 从 wsrelay 的启动日志抽出实际监听地址(-listen 127.0.0.1:0
// 的随机端口经此发现)。格式见 relay/cmd/wsrelay:"wsrelay: listening on <addr> (...)"。
var relayListenRE = regexp.MustCompile(`listening on ([^\s(]+)`)

// startRelay 起一个 wsrelay 进程,返回其 ws:// 基址与进程句柄。
func startRelay(t *testing.T) (wsURL string, p *proc) {
	t.Helper()
	p = startCmd(t, "wsrelay", relayBin, "-listen", "127.0.0.1:0")
	addr := waitForSubmatch(t, p.stderr, relayListenRE, procIOTimeout, "relay listen address")
	return "ws://" + addr, p
}

func waitForSubmatch(t *testing.T, buf *capBuf, re *regexp.Regexp, timeout time.Duration, what string) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if m := re.FindStringSubmatch(buf.String()); m != nil {
			return m[1]
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s; output=%q", what, buf.String())
	return ""
}

// startShare 起一个 windshare share 进程(位置参数在前,flag 在后,§6.9)。
func startShare(t *testing.T, relayURL string, paths []string, extra ...string) *proc {
	t.Helper()
	args := append([]string{"share"}, paths...)
	args = append(args, "--relay", relayURL)
	args = append(args, extra...)
	return startCmd(t, "share", windshareBin, args...)
}

// runGet 跑一个 windshare get 进程至结束,返回退出码与两路输出。
func runGet(t *testing.T, timeout time.Duration, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	p := startCmd(t, "get", windshareBin, append([]string{"get"}, args...)...)
	code = p.waitExit(t, timeout)
	return code, p.stdout.String(), p.stderr.String()
}

// ── 目录树构造与全树哈希 ────────────────────────────────────────────────

// treeSpec 描述一棵待分享的树:files 为相对 slash 路径→内容;dirs 为需显式
// 建立的(可能为空的)目录。
type treeSpec struct {
	files map[string][]byte
	dirs  []string
}

// writeTree 把 spec 铺到临时目录下的 tree/ 子目录并返回其绝对路径。分享该目录,
// 接收端收到同名顶层文件夹 tree/(osfs.Walk 以 basename 作顶层条目)。
func writeTree(t *testing.T, spec treeSpec) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "tree")
	for rel, data := range spec.files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range spec.dirs {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(d)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// hashFiles 返回 root 下每个文件的 relSlash→sha256(hex)。目录不入表——它们
// 的存在性由 assertDirExists 单独校验。
func hashFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		out[filepath.ToSlash(rel)] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatalf("hash %s: %v", root, err)
	}
	return out
}

// assertTreeEqual 断言两棵树的文件集合与内容逐一相等(SHA-256 全树比对)。
func assertTreeEqual(t *testing.T, wantRoot, gotRoot string) {
	t.Helper()
	want := hashFiles(t, wantRoot)
	got := hashFiles(t, gotRoot)
	for rel, wh := range want {
		gh, ok := got[rel]
		if !ok {
			t.Errorf("output is missing file %q", rel)
			continue
		}
		if gh != wh {
			t.Errorf("file %q content mismatch", rel)
		}
	}
	for rel := range got {
		if _, ok := want[rel]; !ok {
			t.Errorf("output contains extra file %q not present in source", rel)
		}
	}
}

func assertDirExists(t *testing.T, path string) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Errorf("directory %q was not materialized: %v", path, err)
		return
	}
	if !fi.IsDir() {
		t.Errorf("%q is not a directory", path)
	}
}

// dirEntryCount 返回目录下的条目数(用于"零文件落盘"断言)。
func dirEntryCount(t *testing.T, dir string) int {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %q: %v", dir, err)
	}
	return len(ents)
}

// treeBytes 汇总目录下所有文件的字节数(轮询下载进度,避免直接并发读 Bitfield
// 造成数据竞争)。
func treeBytes(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if fi, err := d.Info(); err == nil {
			total += fi.Size()
		}
		return nil
	})
	return total
}

// waitUntil 轮询条件直至为真或超时。
func waitUntil(t *testing.T, cond func() bool, timeout time.Duration, what string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for condition: %s", what)
}

// ── 节流 TCP 代理:让回环下载慢到可被确定性打断(断点续传用) ──────────────

// throttleProxy 是一条透明 TCP 转发(WS 在其上原样流过):下行(relay→客户端)
// 方向按 delay 分块限速,使几 MiB 的回环下载耗时可控,从而能在中途稳定
// 观察到部分块落盘并杀掉进程。
type throttleProxy struct {
	ln       net.Listener
	upstream string
	delay    atomic.Int64 // 每 16KiB 下行块的休眠纳秒;0 = 不限速
}

func startThrottleProxy(t *testing.T, upstreamWS string) *throttleProxy {
	t.Helper()
	testnetwork.RequireOSNetwork(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	p := &throttleProxy{ln: ln, upstream: strings.TrimPrefix(upstreamWS, "ws://")}
	go p.acceptLoop()
	t.Cleanup(func() { _ = ln.Close() })
	return p
}

func (p *throttleProxy) url() string { return "ws://" + p.ln.Addr().String() }

func (p *throttleProxy) setDelay(d time.Duration) { p.delay.Store(int64(d)) }

func (p *throttleProxy) acceptLoop() {
	for {
		client, err := p.ln.Accept()
		if err != nil {
			return
		}
		go p.handle(client)
	}
}

func (p *throttleProxy) handle(client net.Conn) {
	testnetwork.AssertOSNetwork()
	up, err := net.Dial("tcp", p.upstream)
	if err != nil {
		_ = client.Close()
		return
	}
	// 上行不限速;下行(relay→client)按 delay 分块限速。
	go func() {
		_, _ = io.Copy(up, client)
		_ = up.Close()
	}()
	p.copyThrottled(client, up)
	_ = client.Close()
	_ = up.Close()
}

func (p *throttleProxy) copyThrottled(dst, src net.Conn) {
	buf := make([]byte, 16*1024)
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			if d := p.delay.Load(); d > 0 {
				time.Sleep(time.Duration(d))
			}
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if rerr != nil {
			return
		}
	}
}

// ── 进程内 relay 与可切断的拨号器(重连用例) ────────────────────────────

// startInProcRelay 起一个进程内中转(真实 signaling/httpapi 组件),返回 ws:// 基址。
// 重连用例需要以进程内拨号器切断发送端链路,故不走独立 wsrelay 进程。
func startInProcRelay(t *testing.T) string {
	t.Helper()
	testnetwork.RequireOSNetwork(t)
	hub := signaling.NewHub(signaling.Config{})
	t.Cleanup(hub.Close)
	ts := httptest.NewServer(httpapi.NewHandler(httpapi.Config{Hub: hub}))
	t.Cleanup(ts.Close)
	return "ws" + strings.TrimPrefix(ts.URL, "http")
}

// dialTracker 记录它拨出的所有 TCP 连接,severAll 一次性切断以模拟发送端
// 网络断线(触发其宽限重连路径)。注入为发送端 http.Client 的 DialContext。
type dialTracker struct {
	mu    sync.Mutex
	conns []net.Conn
}

func (d *dialTracker) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	testnetwork.AssertOSNetwork()
	c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
	if err == nil {
		d.mu.Lock()
		d.conns = append(d.conns, c)
		d.mu.Unlock()
	}
	return c, err
}

func (d *dialTracker) severAll() {
	d.mu.Lock()
	cs := d.conns
	d.conns = nil
	d.mu.Unlock()
	for _, c := range cs {
		_ = c.Close()
	}
}
