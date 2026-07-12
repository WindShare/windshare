package share_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/core/session"
)

// pipeChannel 是内存 FrameChannel:两条缓冲通道背靠背,验证 share 适配器
// 与真实调度器/发送会话的接线(T1.5 衔接点),仍是零网络。任一端 Close
// 即拆整条链路(共享 done)——与真实传输一致,对端 Recv 流随之关闭,
// 发送会话才能以"对端收工"收场。
type pipeChannel struct {
	recv chan session.Frame
	out  chan<- session.Frame
	st   *pipeState
}

// pipeState 是链路两端共享的关闭状态。
type pipeState struct {
	done chan struct{}
	once sync.Once
}

func (st *pipeState) close() { st.once.Do(func() { close(st.done) }) }

// pipeBuffer 需容住一轮在途:InFlightWindow 块 × 每块 1 帧(1 KiB 块远小于
// MaxBlockPayload)+ REQUEST/ERROR 帧;64 给足裕量,避免测试死锁。
const pipeBuffer = 64

func newPipePair() (a, b *pipeChannel) {
	st := &pipeState{done: make(chan struct{})}
	ab := make(chan session.Frame, pipeBuffer)
	ba := make(chan session.Frame, pipeBuffer)
	a = &pipeChannel{recv: make(chan session.Frame), out: ab, st: st}
	b = &pipeChannel{recv: make(chan session.Frame), out: ba, st: st}
	go forward(ba, a.recv, st.done)
	go forward(ab, b.recv, st.done)
	return a, b
}

// forward 把入站帧搬给消费者;链路关闭后先排空缓冲再关闭消费流——关闭前
// 送达的帧(尤其分享级 ERROR)不得丢弃,这是调度器对传输实现的退服假设
// (session.retireChannel 依赖入站流处理完毕才出池)。
func forward(in <-chan session.Frame, recv chan<- session.Frame, done <-chan struct{}) {
	defer close(recv)
	deliver := func(f session.Frame) bool {
		select {
		case recv <- f:
			return true
		case <-time.After(5 * time.Second):
			return false // 消费者已消失(会话终结),放弃以免测试 goroutine 悬死
		}
	}
	for {
		select {
		case f := <-in:
			if !deliver(f) {
				return
			}
		case <-done:
			for {
				select {
				case f := <-in:
					if !deliver(f) {
						return
					}
				default:
					return
				}
			}
		}
	}
}

func (p *pipeChannel) Send(ctx context.Context, f session.Frame) error {
	select {
	case p.out <- f:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-p.st.done:
		return errors.New("pipe: channel is closed")
	}
}

func (p *pipeChannel) SendTerminal(ctx context.Context, f session.Frame) error {
	if err := p.Send(ctx, f); err != nil {
		return err
	}
	return p.Close()
}

func (p *pipeChannel) Recv() <-chan session.Frame { return p.recv }

func (p *pipeChannel) State() session.ChannelState {
	select {
	case <-p.st.done:
		return session.Closed
	default:
		return session.Open
	}
}

func (p *pipeChannel) Close() error {
	p.st.close()
	return nil
}

// 会话级端到端:Sharer.BlockStore/Sealer 挂发送会话,TransferPlan.Sink 与
// Receiver.Opener 挂接收调度器,经内存帧通道走完整块协议后收尾重建全树。
func TestSessionAdaptersEndToEnd(t *testing.T) {
	files, src := goldTree()
	s := newGoldSharer(t, src, files)
	r, sink := newGoldReceiver(t, s)
	plan := mustPlan(t, r, nil)

	recvCh, sendCh := newPipePair()
	ss, err := session.NewSendSession(sendCh, s.BlockStore(), s.Sealer())
	if err != nil {
		t.Fatalf("NewSendSession: %v", err)
	}
	rs, err := session.NewReceiveSession(plan.Chunks(), plan.Sink(), r.Opener(), session.Options{
		MaxBlockBytes: r.MaxBlockBytes(),
	})
	if err != nil {
		t.Fatalf("NewReceiveSession: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sendDone := make(chan error, 1)
	go func() { sendDone <- ss.Run(ctx) }()
	if err := rs.AddChannel(recvCh); err != nil {
		t.Fatalf("AddChannel: %v", err)
	}
	if err := rs.Run(ctx); err != nil {
		t.Fatalf("ReceiveSession.Run: %v", err)
	}
	// 接收会话收工即关闭通道,发送会话据此以 nil 收场(对端收工语义)。
	if err := <-sendDone; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("SendSession.Run: %v", err)
	}
	if have := plan.Sink().Have().Count(); have != plan.Chunks().Count() {
		t.Fatalf("have=%d, want %d", have, plan.Chunks().Count())
	}
	if err := plan.Finalize(); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	requireTreeRebuilt(t, src, files, sink)
}

// 会话级漂移中止:Source 报漂移 → 发送会话回分享级 ERROR(code=BlockRead)
// → 接收调度器以该错误整体失败(换通道也救不了同一份源,§6.6)。
func TestSessionDriftAbort(t *testing.T) {
	files, src := goldTree()
	s := newGoldSharer(t, src, files)
	r, _ := newGoldReceiver(t, s)
	plan := mustPlan(t, r, nil)
	src.fail = errors.New("osfs: source file changed; reshare required")

	recvCh, sendCh := newPipePair()
	ss, err := session.NewSendSession(sendCh, s.BlockStore(), s.Sealer())
	if err != nil {
		t.Fatalf("NewSendSession: %v", err)
	}
	rs, err := session.NewReceiveSession(plan.Chunks(), plan.Sink(), r.Opener(), session.Options{
		MaxBlockBytes: r.MaxBlockBytes(),
	})
	if err != nil {
		t.Fatalf("NewReceiveSession: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go func() { _ = ss.Run(ctx) }()
	if err := rs.AddChannel(recvCh); err != nil {
		t.Fatalf("AddChannel: %v", err)
	}
	err = rs.Run(ctx)
	var sessErr *session.Error
	if !errors.As(err, &sessErr) || sessErr.Code != session.ErrCodeBlockRead {
		t.Fatalf("Run err = %v, want share-scoped ERROR(code=BlockRead)", err)
	}
}
