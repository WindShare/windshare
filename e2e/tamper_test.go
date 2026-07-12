// 传输篡改:黑盒需在 WS 内层解出转发帧再翻转数据面帧,不便注入;按 §T6.1
// 许可用进程内 FrameChannel 代理翻转某 BLOCK 帧 payload 一个字节,验证块级
// AEAD 拒收该次到达、经重试(发送端以新 nonce 重发)最终一致;若每次都篡改,
// 则明确失败(块重试耗尽,§6.6)。
package e2e

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/share"
)

// memChan 是进程内 FrameChannel:一对交叉相连的端点,Send 把帧投到对端入站
// 队列;transform(若有)在投递前作用于本端所发帧,用于注入篡改。
type memChan struct {
	inbound   chan session.Frame
	peer      *memChan
	transform func(session.Frame) session.Frame

	mu        sync.Mutex
	state     session.ChannelState
	closed    chan struct{}
	closeOnce sync.Once
}

// newTamperPipe 造一对端点:recvEnd 交给接收会话,sendEnd 交给发送会话;
// transform 只作用于 sendEnd 所发帧(发送端 → 接收端方向)。
func newTamperPipe(transform func(session.Frame) session.Frame) (recvEnd, sendEnd *memChan) {
	recvEnd = &memChan{inbound: make(chan session.Frame, 64), state: session.Open, closed: make(chan struct{})}
	sendEnd = &memChan{inbound: make(chan session.Frame, 64), state: session.Open, closed: make(chan struct{})}
	recvEnd.peer = sendEnd
	sendEnd.peer = recvEnd
	sendEnd.transform = transform
	return recvEnd, sendEnd
}

var errMemChanClosed = errors.New("memchan: closed")

func (c *memChan) Send(ctx context.Context, f session.Frame) error {
	select {
	case <-c.closed:
		return errMemChanClosed
	default:
	}
	g := f
	if c.transform != nil {
		g = c.transform(f)
	}
	select {
	case c.peer.inbound <- g:
		return nil
	case <-c.closed:
		return errMemChanClosed
	case <-c.peer.closed:
		return errMemChanClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *memChan) SendTerminal(ctx context.Context, f session.Frame) error {
	if err := c.Send(ctx, f); err != nil {
		return err
	}
	return c.Close()
}

func (c *memChan) Recv() <-chan session.Frame { return c.inbound }

func (c *memChan) State() session.ChannelState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

func (c *memChan) Close() error {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.state = session.Closed
		c.mu.Unlock()
		close(c.closed)
	})
	return nil
}

// tamperTree 用小块保证每块单帧,便于精确翻转"某 BLOCK 帧"的 payload。
func tamperFixtures(t *testing.T) (*share.Sharer, *share.Receiver, *share.TransferPlan, string, string) {
	t.Helper()
	payload := make([]byte, 5*e2eBlockSizeInt+321) // 6 块
	for i := range payload {
		payload[i] = byte(i*17 + 9)
	}
	srcRoot := writeTree(t, treeSpec{files: map[string][]byte{"data.bin": payload}})
	snap, err := osfs.Walk([]string{srcRoot})
	if err != nil {
		t.Fatal(err)
	}
	metas := make([]share.FileMeta, len(snap.Entries))
	for i, e := range snap.Entries {
		metas[i] = share.FileMeta(e)
	}
	sharer, err := share.NewSharer(metas, osfs.NewSource(snap), share.Options{ChunkSize: e2eBlockSizeInt})
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := sharer.SealedManifest()
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	sink, err := osfs.NewSink(out, osfs.SinkOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sink.Close() })
	rcv, err := share.NewReceiver(sharer.Link(), sealed, sink)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := rcv.Plan(nil)
	if err != nil {
		t.Fatal(err)
	}
	return sharer, rcv, plan, srcRoot, filepath.Join(out, "tree")
}

// flipBlockPayload 造一个翻转指定块首字节 payload 的 transform;gate 控制
// 只翻一次还是每次都翻。
func flipBlockPayload(target uint64, once bool) (func(session.Frame) session.Frame, *atomic.Int64) {
	var hits atomic.Int64
	fn := func(f session.Frame) session.Frame {
		msg, err := session.Decode(f)
		if err != nil {
			return f
		}
		b, ok := msg.(*session.Block)
		if !ok || b.Index != target {
			return f
		}
		if once && hits.Load() > 0 {
			return f
		}
		hits.Add(1)
		corrupt := append([]byte(nil), b.Payload...)
		corrupt[0] ^= 0xff // 翻转首字节(seq 0 时即 nonce 首字节)→ AEAD 校验必败
		nf, err := session.EncodeBlock(session.Block{Index: b.Index, Seq: b.Seq, Last: b.Last, Payload: corrupt})
		if err != nil {
			return f
		}
		return nf
	}
	return fn, &hits
}

// TestTamperedBlockRetriedThenConsistent:翻转块 0 首帧 payload 恰一次 →
// 该次被 AEAD 拒收 → 发送端以新 nonce 重发 → 最终逐字节一致。
func TestTamperedBlockRetriedThenConsistent(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	sharer, rcv, plan, srcRoot, outTree := tamperFixtures(t)

	transform, hits := flipBlockPayload(0, true)
	recvEnd, sendEnd := newTamperPipe(transform)

	ss, err := session.NewSendSession(sendEnd, sharer.BlockStore(), sharer.Sealer())
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = ss.Run(ctx) }()

	sess, err := session.NewReceiveSession(plan.Chunks(), plan.Sink(), rcv.Opener(), session.Options{MaxBlockBytes: rcv.MaxBlockBytes()})
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.AddChannel(recvEnd); err != nil {
		t.Fatal(err)
	}
	if err := sess.Run(ctx); err != nil {
		t.Fatalf("download should complete after one tampered attempt: %v", err)
	}
	_ = ss.Close()

	if hits.Load() == 0 {
		t.Error("tampering did not occur; block 0 first frame was not flipped")
	}
	if err := plan.Finalize(); err != nil {
		t.Fatalf("finalize materialization: %v", err)
	}
	assertTreeEqual(t, srcRoot, outTree)
}

// TestTamperedBlockAlwaysFailsCleanly:每次都翻转块 0 → 块重试次数耗尽 →
// 接收端以明确错误收场(不静默接受被篡改内容,§6.6)。
func TestTamperedBlockAlwaysFailsCleanly(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	sharer, rcv, plan, _, _ := tamperFixtures(t)

	transform, _ := flipBlockPayload(0, false)
	recvEnd, sendEnd := newTamperPipe(transform)

	ss, err := session.NewSendSession(sendEnd, sharer.BlockStore(), sharer.Sealer())
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = ss.Run(ctx) }()

	sess, err := session.NewReceiveSession(plan.Chunks(), plan.Sink(), rcv.Opener(), session.Options{
		MaxBlockBytes:    rcv.MaxBlockBytes(),
		MaxBlockAttempts: 4, // 收敛快;每次到达都被翻转,重试无从成功
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.AddChannel(recvEnd); err != nil {
		t.Fatal(err)
	}
	err = sess.Run(ctx)
	if !errors.Is(err, session.ErrBlockExhausted) {
		t.Fatalf("continuous tampering should exhaust block retries, got: %v", err)
	}
	_ = ss.Close()
}
