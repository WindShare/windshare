// 分享中改文件即中止:share 侧退出码走真实进程(黑盒),接收端用进程内
// 会话精确断言 terminal ERROR。A3 的 terminal-forward 生命周期保证该域错误
// 在连接关闭前可见,因此连接终结/超时不再是可接受的替代结果。
package e2e

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/share"
	"github.com/windshare/windshare/transport/relay"
)

// TestDriftAbort:注册后改文件 mtime 制造漂移 → 发送端读后复核中止,share 进程
// 以 4 退出并提示重新分享;接收端必须得到 ErrCodeBlockRead terminal(§6.3)。
func TestDriftAbort(t *testing.T) {
	t.Parallel()
	relayURL, _ := startRelay(t)
	payload := make([]byte, 3*e2eBlockSizeInt)
	srcRoot := writeTree(t, treeSpec{files: map[string][]byte{"vol.bin": payload}})
	target := srcRoot + string(os.PathSeparator) + "vol.bin"

	sp := startShare(t, relayURL, []string{target}, "--block-size", e2eBlockSize)
	full := sp.waitLine(t, "Link: ", procIOTimeout)
	lnk, err := link.Parse(full)
	if err != nil {
		t.Fatalf("link is not parseable: %v", err)
	}

	// 注册快照在前,改 mtime 即制造漂移(读后复核 size/mtime 不符)。
	if err := os.Chtimes(target, time.Time{}, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}

	// 进程内接收端:请求块 → 发送端读盘漂移 → 收到分享级 terminal ERROR。
	// 8s 上下文只防测试挂死,不能成为可接受终局。
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	conn, err := relay.DialReceiver(ctx, relay.ReceiverConfig{RelayURL: lnk.Relays[0], ShareID: lnk.ShareID})
	if err != nil {
		t.Fatalf("DialReceiver: %v", err)
	}
	defer conn.Close()
	out := t.TempDir()
	sink, err := osfs.NewSink(out, osfs.SinkOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sink.Close() })
	rcv, err := share.NewReceiver(lnk, conn.SealedManifest(), sink)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := rcv.Plan(nil)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := session.NewReceiveSession(plan.Chunks(), plan.Sink(), rcv.Opener(), session.Options{
		MaxBlockBytes:    rcv.MaxBlockBytes(),
		RequestTimeout:   time.Second,
		MaxBlockAttempts: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.AddChannel(conn.Channel()); err != nil {
		t.Fatal(err)
	}
	runErr := sess.Run(ctx)
	if runErr == nil {
		t.Fatal("receiver should not complete successfully under drift")
	}
	var peerErr *session.Error
	if !errors.As(runErr, &peerErr) || peerErr.Code != session.ErrCodeBlockRead {
		t.Fatalf("receiver terminal = %v, want ErrCodeBlockRead", runErr)
	}

	// share 侧退出码走真实进程(黑盒判据)。
	shareCode := sp.waitExit(t, 20*time.Second)
	if shareCode != 4 {
		t.Fatalf("share aborted by drift should exit with 4, got %d; stderr=%s", shareCode, sp.stderr.String())
	}
	stderr := sp.stderr.String()
	if !strings.Contains(stderr, "source file changed") || !strings.Contains(stderr, "create a new share") {
		t.Errorf("share output lacks a drift explanation; stderr=%s", stderr)
	}
}
