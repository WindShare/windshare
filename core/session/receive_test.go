package session

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/core/layout"
)

func TestReceiveSingleChannelHappyPath(t *testing.T) {
	checkNoLeak(t)
	selected := allIndices(20)
	r := newRig(t, 20, 64, selected, defaultOptions())
	_, receiverEnd := r.addSender(64)

	if err := waitErr(t, mustRun(r.sess, context.Background()), 5*time.Second); err != nil {
		t.Fatalf("Run = %v", err)
	}
	r.verify(selected)
	// 每帧 REQUEST 不超过在途窗口(背压不变量)。
	for _, req := range receiverEnd.requestsSent(t) {
		if len(req) > InFlightWindow {
			t.Errorf("REQUEST contains %d blocks, exceeding window %d", len(req), InFlightWindow)
		}
	}
	// 会话完成后调度状态应清空。
	if r.sess.remaining != 0 || len(r.sess.assigned) != 0 || len(r.sess.buffered) != 0 || len(r.sess.retry) != 0 {
		t.Errorf("residual state: remaining=%d assigned=%d buffered=%d retry=%d",
			r.sess.remaining, len(r.sess.assigned), len(r.sess.buffered), len(r.sess.retry))
	}
}

// 续传:预置 Have 位后,只有缺失块会被请求(需求集 = 选中 − Have,§6.6)。
func TestReceiveResumeWithPresetHave(t *testing.T) {
	selected := allIndices(16)
	r := newRigPrepped(t, 16, 64, selected, defaultOptions(), func(sink *memSink) {
		for i := uint64(0); i < 16; i += 2 {
			sink.have.Set(i) // 偶数块已持有
		}
	})
	_, receiverEnd := r.addSender(64)

	if err := waitErr(t, mustRun(r.sess, context.Background()), 5*time.Second); err != nil {
		t.Fatalf("Run = %v", err)
	}
	requested := map[uint64]bool{}
	for _, req := range receiverEnd.requestsSent(t) {
		for _, idx := range req {
			requested[idx] = true
		}
	}
	for i := range uint64(16) {
		if want := i%2 == 1; requested[i] != want {
			t.Errorf("block %d requested = %v, want %v", i, requested[i], want)
		}
	}
	r.verify([]uint64{1, 3, 5, 7, 9, 11, 13, 15})
}

func TestReceiveChunkSetStateIsCompactAtGeometryLimit(t *testing.T) {
	sink := newMemSink(layout.MaxChunkCount)
	sink.order = DeliveryAscending
	sess, err := NewReceiveSession(layout.FullChunkSet(layout.MaxChunkCount), sink, &fakeCodec{}, Options{
		MaxBlockBytes: 1,
	})
	if err != nil {
		t.Fatalf("NewReceiveSession: %v", err)
	}
	if sess.remaining != layout.MaxChunkCount {
		t.Fatalf("remaining = %d, want %d", sess.remaining, layout.MaxChunkCount)
	}
	if len(sess.selectedRanges) != 1 {
		t.Fatalf("selected ranges = %d, want one compact interval", len(sess.selectedRanges))
	}
	if len(sess.orderedWindow) != InFlightWindow {
		t.Fatalf("ordered window = %d, want %d", len(sess.orderedWindow), InFlightWindow)
	}
	if len(sess.retry) != 0 || len(sess.assigned) != 0 || len(sess.buffered) != 0 {
		t.Fatalf("active state expanded before scheduling: retry=%d assigned=%d buffered=%d", len(sess.retry), len(sess.assigned), len(sess.buffered))
	}
}

func TestReceiveLargeResumeFindsMissingChunkWithoutPendingExpansion(t *testing.T) {
	const chunks uint64 = 1 << 20
	sink := newMemSink(chunks)
	sink.order = DeliveryAscending
	for index := range chunks - 1 {
		sink.have.Set(index)
	}
	sess, err := NewReceiveSession(layout.FullChunkSet(chunks), sink, &fakeCodec{}, Options{
		MaxBlockBytes: 1,
	})
	if err != nil {
		t.Fatalf("NewReceiveSession: %v", err)
	}
	if sess.remaining != 1 || !slices.Equal(sess.orderedWindow, []uint64{chunks - 1}) {
		t.Fatalf("resume state: remaining=%d window=%v", sess.remaining, sess.orderedWindow)
	}
}

// 选择性下载:调度器只认块号集合,对文件无感。
func TestReceiveSelectiveSubset(t *testing.T) {
	selected := []uint64{3, 5, 9}
	r := newRig(t, 12, 64, selected, defaultOptions())
	_, receiverEnd := r.addSender(64)

	if err := waitErr(t, mustRun(r.sess, context.Background()), 5*time.Second); err != nil {
		t.Fatalf("Run = %v", err)
	}
	r.verify(selected)
	requested := map[uint64]bool{}
	for _, req := range receiverEnd.requestsSent(t) {
		for _, idx := range req {
			requested[idx] = true
		}
	}
	if len(requested) != len(selected) {
		t.Errorf("requested %d blocks, want %d", len(requested), len(selected))
	}
}

// 丢帧:响应在途丢失 → 超时重试补齐。
func TestReceiveDropRetry(t *testing.T) {
	selected := allIndices(10)
	opts := defaultOptions()
	opts.RequestTimeout = 100 * time.Millisecond
	r := newRig(t, 10, 64, selected, opts)
	senderEnd, receiverEnd := r.addSender(64)

	var mu sync.Mutex
	dropped := map[uint64]bool{}
	senderEnd.setOnSend(func(f Frame) bool {
		msg, err := Decode(f)
		if err != nil {
			return true
		}
		b, ok := msg.(*Block)
		if !ok || (b.Index != 2 && b.Index != 7) {
			return true
		}
		mu.Lock()
		defer mu.Unlock()
		if !dropped[b.Index] {
			dropped[b.Index] = true // 首次响应整帧丢失
			return false
		}
		return true
	})

	if err := waitErr(t, mustRun(r.sess, context.Background()), 5*time.Second); err != nil {
		t.Fatalf("Run = %v", err)
	}
	r.verify(selected)
	// 丢过的块必然出现在 ≥2 个 REQUEST 里。
	for _, idx := range []uint64{2, 7} {
		n := 0
		for _, req := range receiverEnd.requestsSent(t) {
			if slices.Contains(req, idx) {
				n++
			}
		}
		if n < 2 {
			t.Errorf("block %d was requested %d times; expected a retry", idx, n)
		}
	}
}

// 断连:通道中途关闭 → 在途块(含半收的多帧块)转移到另一通道重取。
func TestReceiveDisconnectTransfersInflight(t *testing.T) {
	checkNoLeak(t)
	selected := allIndices(12)
	r := newRig(t, 12, 70_000, selected, defaultOptions()) // 每块 2 帧
	senderA, _ := r.addSender(64)
	senderB, _ := r.addSender(64)

	var frames int
	senderA.setOnSend(func(f Frame) bool {
		if f[0] != FrameBlock {
			return true
		}
		if frames++; frames == 4 {
			// 块 0 完整、块 1 只送出 seq0:半块必须被丢弃、整块重取(§6.12)。
			_ = senderA.Close()
			return false
		}
		return true
	})

	if err := waitErr(t, mustRun(r.sess, context.Background()), 5*time.Second); err != nil {
		t.Fatalf("Run = %v", err)
	}
	r.verify(selected)
	if sent := senderB.blocksSent(t); !sent[1] {
		t.Error("block 1 was not retried through the surviving channel")
	}
}

// A transport disconnect abandons one ciphertext attempt rather than proving the
// block bad. Rejoins must not consume the validation/timeout budget that bounds a
// genuinely faulty source, or one later corrupt response can fail an otherwise
// recoverable download.
func TestReceiveDisconnectDoesNotConsumeBlockFailureBudget(t *testing.T) {
	opts := defaultOptions()
	opts.MaxBlockAttempts = 2
	r := newRig(t, 1, 64, allIndices(1), opts)

	disconnectingSender, _ := r.addSender(8)
	var disconnected bool
	disconnectingSender.setOnSend(func(f Frame) bool {
		if f[0] == FrameBlock && !disconnected {
			disconnected = true
			_ = disconnectingSender.Close()
			return false
		}
		return true
	})

	var corrupted bool
	r.addScripted(8, func(indices []uint64, send func(Frame)) {
		for _, idx := range indices {
			frames := sealedFrames(r.t, r.codec, r.store, idx, MaxBlockPayload)
			if !corrupted {
				corrupted = true
				frames[0][len(frames[0])-1] ^= 0xff
			}
			for _, frame := range frames {
				send(frame)
			}
		}
	})

	if err := waitErr(t, mustRun(r.sess, context.Background()), 5*time.Second); err != nil {
		t.Fatalf("Run after disconnect and one corrupt response = %v", err)
	}
	r.verify(allIndices(1))
}

// 双通道聚合:两条等速通道各自承担一部分块。
func TestReceiveDualChannelAggregation(t *testing.T) {
	selected := allIndices(30)
	r := newRig(t, 30, 64, selected, defaultOptions())
	delay := func(f Frame) bool {
		if f[0] == FrameBlock {
			time.Sleep(3 * time.Millisecond)
		}
		return true
	}
	senderA, _ := r.addSender(64)
	senderA.setOnSend(delay)
	senderB, _ := r.addSender(64)
	senderB.setOnSend(delay)

	if err := waitErr(t, mustRun(r.sess, context.Background()), 10*time.Second); err != nil {
		t.Fatalf("Run = %v", err)
	}
	r.verify(selected)
	a, b := len(senderA.blocksSent(t)), len(senderB.blocksSent(t))
	if a == 0 || b == 0 {
		t.Errorf("aggregation failed: A=%d B=%d", a, b)
	}
}

// 热切换:先只有慢通道;传输中途快通道 Open 入池,后续块应压倒性流向快通道。
func TestReceiveHotSwitch(t *testing.T) {
	checkNoLeak(t)
	selected := allIndices(40)
	r := newRig(t, 40, 64, selected, defaultOptions())
	slowEnd, _ := r.addSender(64)
	slowEnd.setOnSend(func(f Frame) bool {
		if f[0] == FrameBlock {
			time.Sleep(15 * time.Millisecond)
		}
		return true
	})

	var once sync.Once
	fastCh := make(chan *memChannel, 1)
	r.sink.onWrite = func(uint64) {
		once.Do(func() {
			fast, _ := r.addSender(64) // 首块落地后才"连上"快通道
			fastCh <- fast
		})
	}

	if err := waitErr(t, mustRun(r.sess, context.Background()), 15*time.Second); err != nil {
		t.Fatalf("Run = %v", err)
	}
	r.verify(selected)
	fastEnd := <-fastCh
	slow, fast := len(slowEnd.blocksSent(t)), len(fastEnd.blocksSent(t))
	if fast <= slow {
		t.Errorf("hot switch failed: slow channel %d blocks, fast channel %d blocks", slow, fast)
	}
}

// 有序交付:响应刻意倒序回帧,Sink 仍必须按块号升序收到,且重排缓冲有界。
func TestReceiveOrderedDelivery(t *testing.T) {
	selected := allIndices(24)
	opts := defaultOptions()
	r := newOrderedRig(t, 24, 64, selected, opts)
	r.addScripted(64, func(indices []uint64, send func(Frame)) {
		for i := len(indices) - 1; i >= 0; i-- { // 倒序回帧
			for _, f := range sealedFrames(r.t, r.codec, r.store, indices[i], MaxBlockPayload) {
				send(f)
			}
		}
	})

	if err := waitErr(t, mustRun(r.sess, context.Background()), 5*time.Second); err != nil {
		t.Fatalf("Run = %v", err)
	}
	r.verify(selected)
	order := r.sink.writeOrder()
	if !slices.IsSorted(order) || len(order) != 24 {
		t.Errorf("delivery order is not ascending: %v", order)
	}
	if r.sess.maxBuffered > InFlightWindow-1 {
		t.Errorf("reorder buffer peak %d exceeds window bound %d", r.sess.maxBuffered, InFlightWindow-1)
	}
}

// 无序模式对乱序到达同样收敛(只验完整性,不约束次序)。
func TestReceiveUnorderedOutOfOrder(t *testing.T) {
	selected := allIndices(16)
	r := newRig(t, 16, 64, selected, defaultOptions())
	r.addScripted(64, func(indices []uint64, send func(Frame)) {
		for i := len(indices) - 1; i >= 0; i-- {
			for _, f := range sealedFrames(r.t, r.codec, r.store, indices[i], MaxBlockPayload) {
				send(f)
			}
		}
	})
	if err := waitErr(t, mustRun(r.sess, context.Background()), 5*time.Second); err != nil {
		t.Fatalf("Run = %v", err)
	}
	r.verify(selected)
}

// 队头重试最高优先:块 0 首发丢失后,下一个 REQUEST 必须只为块 0 重试;
// 期间缓冲满窗但有界,交付仍严格升序。
func TestReceiveOrderedHeadRetryPriority(t *testing.T) {
	selected := allIndices(16)
	opts := defaultOptions()
	opts.RequestTimeout = 80 * time.Millisecond
	r := newOrderedRig(t, 16, 64, selected, opts)

	var served0 bool
	_, receiverEnd := r.addScripted(64, func(indices []uint64, send func(Frame)) {
		for _, idx := range indices {
			if idx == 0 && !served0 {
				served0 = true // 队头首发吞掉,逼出超时重试
				continue
			}
			for _, f := range sealedFrames(r.t, r.codec, r.store, idx, MaxBlockPayload) {
				send(f)
			}
		}
	})

	if err := waitErr(t, mustRun(r.sess, context.Background()), 5*time.Second); err != nil {
		t.Fatalf("Run = %v", err)
	}
	r.verify(selected)
	order := r.sink.writeOrder()
	if !slices.IsSorted(order) || order[0] != 0 {
		t.Errorf("delivery order: %v", order)
	}
	reqs := receiverEnd.requestsSent(t)
	if len(reqs) < 2 {
		t.Fatalf("REQUEST sequence is too short: %v", reqs)
	}
	if !slices.Equal(reqs[0], allIndices(8)) {
		t.Errorf("first request = %v, want [0..7]", reqs[0])
	}
	if !slices.Equal(reqs[1], []uint64{0}) {
		t.Errorf("head retry request = %v, want [0]", reqs[1])
	}
	// 队头缺席期间,1..7 全部就绪但只能压在缓冲里:峰值恰为窗口-1。
	if r.sess.maxBuffered != InFlightWindow-1 {
		t.Errorf("reorder buffer peak %d, want %d", r.sess.maxBuffered, InFlightWindow-1)
	}
	// 队头未交付前,任何请求都不得越出重排窗(有界缓冲的另一半)。
	for _, req := range reqs[:2] {
		for _, idx := range req {
			if idx >= InFlightWindow {
				t.Errorf("reorder window exceeded by request for block %d", idx)
			}
		}
	}
}

// 分享级 ERROR(读块失败/漂移):整个会话以 *Error 失败。
func TestReceiveFatalErrorFrame(t *testing.T) {
	selected := allIndices(8)
	r := newRig(t, 8, 64, selected, defaultOptions())
	drift := errors.New("source snapshot drift")
	r.store.errAt[3] = drift
	r.addSender(64)

	err := waitErr(t, mustRun(r.sess, context.Background()), 5*time.Second)
	var e *Error
	if !errors.As(err, &e) || e.Code != ErrCodeBlockRead {
		t.Fatalf("Run = %v, want *Error(BlockRead)", err)
	}
}

// 通道级 ERROR(BadRequest):只弃用来路通道,其余通道接管补齐。
func TestReceiveChannelErrorDropsChannelOnly(t *testing.T) {
	selected := allIndices(16)
	r := newRig(t, 16, 64, selected, defaultOptions())
	badEnd, _ := r.addScripted(64, func(indices []uint64, send func(Frame)) {
		f, _ := EncodeError(ErrCodeBadRequest, "误报")
		send(f)
	})
	r.addSender(64)

	if err := waitErr(t, mustRun(r.sess, context.Background()), 5*time.Second); err != nil {
		t.Fatalf("Run = %v", err)
	}
	r.verify(selected)
	if badEnd.State() != Closed {
		t.Error("misreporting channel should be closed and removed")
	}
}

// 协议违规通道被摘除,存活通道补齐;违规后续帧按迟到帧丢弃。
func TestReceiveViolatingChannelDropped(t *testing.T) {
	extraBlock := func(r *rig, idx uint64) Frame {
		return Frame(sealedFrames(r.t, r.codec, r.store, idx, MaxBlockPayload)[0])
	}
	tests := []struct {
		name    string
		respond func(r *rig) func(indices []uint64, send func(Frame))
	}{
		{"畸形帧", func(r *rig) func([]uint64, func(Frame)) {
			return func(indices []uint64, send func(Frame)) {
				send(Frame{0xFF, 0x00})
				send(extraBlock(r, indices[0])) // 违规后的余帧走迟到帧路径
			}
		}},
		{"错位帧型(REQUEST)", func(r *rig) func([]uint64, func(Frame)) {
			return func(indices []uint64, send func(Frame)) {
				f, _ := EncodeRequest([]uint64{0})
				send(f)
				send(extraBlock(r, indices[0]))
			}
		}},
		{"重组违规(重复 seq)", func(r *rig) func([]uint64, func(Frame)) {
			return func(indices []uint64, send func(Frame)) {
				f, _ := EncodeBlock(Block{Index: indices[0], Seq: 0, Payload: []byte{1}})
				send(f)
				send(f) // 同 seq 二至
				send(extraBlock(r, indices[0]))
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			selected := allIndices(12)
			r := newRig(t, 12, 64, selected, defaultOptions())
			badEnd, _ := r.addScripted(64, tt.respond(r))
			r.addSender(64)
			if err := waitErr(t, mustRun(r.sess, context.Background()), 5*time.Second); err != nil {
				t.Fatalf("Run = %v", err)
			}
			r.verify(selected)
			if badEnd.State() != Closed {
				t.Error("violating channel should be closed")
			}
		})
	}
}

// 传输层写失败:该通道退服、在途转移,会话不受牵连。
func TestReceiveSendFailureRetiresChannel(t *testing.T) {
	selected := allIndices(10)
	r := newRig(t, 10, 64, selected, defaultOptions())
	_, receiverA := r.addSender(64)
	receiverA.failNextSend.Store(true)
	r.addSender(64)

	if err := waitErr(t, mustRun(r.sess, context.Background()), 5*time.Second); err != nil {
		t.Fatalf("Run = %v", err)
	}
	r.verify(selected)
	if receiverA.State() != Closed {
		t.Error("channel with write failure should be closed and retired")
	}
}

// AEAD 校验失败:丢弃该次到达并重试,不处死通道。
func TestReceiveOpenFailureRetries(t *testing.T) {
	selected := allIndices(4)
	r := newRig(t, 4, 64, selected, defaultOptions())
	var corrupted bool
	_, receiverEnd := r.addScripted(64, func(indices []uint64, send func(Frame)) {
		for _, idx := range indices {
			frames := sealedFrames(r.t, r.codec, r.store, idx, MaxBlockPayload)
			if idx == 1 && !corrupted {
				corrupted = true
				frames[0][len(frames[0])-1] ^= 0xFF // 篡改密文尾字节
			}
			for _, f := range frames {
				send(f)
			}
		}
	})

	if err := waitErr(t, mustRun(r.sess, context.Background()), 5*time.Second); err != nil {
		t.Fatalf("Run = %v", err)
	}
	r.verify(selected)
	n := 0
	for _, req := range receiverEnd.requestsSent(t) {
		if slices.Contains(req, 1) {
			n++
		}
	}
	if n < 2 {
		t.Errorf("block 1 was requested %d times; expected retry after authentication failure", n)
	}
}

// 校验持续失败同样计入尝试预算,耗尽即失败(而非无限重取坏密文)。
func TestReceiveOpenFailureExhausts(t *testing.T) {
	opts := defaultOptions()
	opts.MaxBlockAttempts = 1
	r := newRig(t, 2, 64, allIndices(2), opts)
	r.addScripted(64, func(indices []uint64, send func(Frame)) {
		for _, idx := range indices {
			frames := sealedFrames(r.t, r.codec, r.store, idx, MaxBlockPayload)
			if idx == 0 {
				frames[0][len(frames[0])-1] ^= 0xFF // 每次都给坏密文
			}
			for _, f := range frames {
				send(f)
			}
		}
	})
	if err := waitErr(t, mustRun(r.sess, context.Background()), 5*time.Second); !errors.Is(err, ErrBlockExhausted) {
		t.Fatalf("Run = %v, want ErrBlockExhausted", err)
	}
}

// 尝试耗尽:某块始终无响应 → 以 ErrBlockExhausted 明确失败。
func TestReceiveBlockExhausted(t *testing.T) {
	selected := allIndices(8)
	opts := defaultOptions()
	opts.RequestTimeout = 60 * time.Millisecond
	opts.MaxBlockAttempts = 2
	r := newRig(t, 8, 64, selected, opts)
	r.addScripted(64, func(indices []uint64, send func(Frame)) {
		for _, idx := range indices {
			if idx == 3 {
				continue // 永不响应
			}
			for _, f := range sealedFrames(r.t, r.codec, r.store, idx, MaxBlockPayload) {
				send(f)
			}
		}
	})

	err := waitErr(t, mustRun(r.sess, context.Background()), 5*time.Second)
	if !errors.Is(err, ErrBlockExhausted) {
		t.Fatalf("Run = %v, want ErrBlockExhausted", err)
	}
}

// Connecting 通道先入池、Open 才参与分配(热切换入池语义)。
func TestReceiveConnectingChannelDeferred(t *testing.T) {
	selected := allIndices(6)
	opts := defaultOptions()
	opts.RequestTimeout = 200 * time.Millisecond // tick=50ms,Open 感知延迟上界
	r := newRig(t, 6, 64, selected, opts)
	_, receiverEnd := r.addSender(64)
	receiverEnd.setState(Connecting)

	done := mustRun(r.sess, context.Background())
	time.Sleep(80 * time.Millisecond)
	if reqs := receiverEnd.requestsSent(t); len(reqs) != 0 {
		t.Errorf("requests dispatched while Connecting: %v", reqs)
	}
	receiverEnd.setState(Open)
	if err := waitErr(t, done, 5*time.Second); err != nil {
		t.Fatalf("Run = %v", err)
	}
	r.verify(selected)
}

func TestReceiveContextCancel(t *testing.T) {
	checkNoLeak(t)
	opts := defaultOptions()
	opts.RequestTimeout = 10 * time.Second // 只验取消路径,不让超时干扰
	r := newRig(t, 4, 64, allIndices(4), opts)
	r.addScripted(4, func([]uint64, func(Frame)) {}) // 永不回帧

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := waitErr(t, mustRun(r.sess, ctx), 5*time.Second); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run = %v, want DeadlineExceeded", err)
	}
}

func TestReceiveLifecycle(t *testing.T) {
	checkNoLeak(t)
	t.Run("Close 幂等且终止 Run", func(t *testing.T) {
		r := newRig(t, 4, 64, allIndices(4), defaultOptions())
		r.addScripted(4, func([]uint64, func(Frame)) {})
		done := mustRun(r.sess, context.Background())
		if err := r.sess.Close(); err != nil {
			t.Errorf("Close 1: %v", err)
		}
		if err := r.sess.Close(); err != nil {
			t.Errorf("Close 2: %v", err)
		}
		if err := waitErr(t, done, 5*time.Second); !errors.Is(err, ErrSessionClosed) {
			t.Errorf("Run = %v", err)
		}
		if err := r.sess.AddChannel(newSilentChannel()); !errors.Is(err, ErrSessionClosed) {
			t.Errorf("AddChannel after session completion = %v", err)
		}
	})
	t.Run("Run 之前 Close 就地回收通道", func(t *testing.T) {
		r := newRig(t, 4, 64, allIndices(4), defaultOptions())
		_, receiverEnd := newPipe(4)
		if err := r.sess.AddChannel(receiverEnd); err != nil {
			t.Fatalf("AddChannel: %v", err)
		}
		_ = r.sess.Close()
		if receiverEnd.State() != Closed {
			t.Error("unadopted channel should close with the session")
		}
		if err := r.sess.Run(context.Background()); !errors.Is(err, ErrSessionClosed) {
			t.Errorf("Run = %v", err)
		}
	})
	t.Run("Run 只跑一次", func(t *testing.T) {
		r := newRig(t, 1, 8, allIndices(1), defaultOptions())
		r.addSender(8)
		if err := waitErr(t, mustRun(r.sess, context.Background()), 5*time.Second); err != nil {
			t.Fatalf("Run 1 = %v", err)
		}
		if err := r.sess.Run(context.Background()); !errors.Is(err, ErrSessionReused) {
			t.Errorf("Run 2 = %v", err)
		}
	})
	t.Run("空需求集立即完成", func(t *testing.T) {
		r := newRig(t, 4, 64, nil, defaultOptions())
		if err := waitErr(t, mustRun(r.sess, context.Background()), 5*time.Second); err != nil {
			t.Errorf("Run = %v", err)
		}
	})
	t.Run("Have 全覆盖立即完成", func(t *testing.T) {
		r := newRigPrepped(t, 4, 64, allIndices(4), defaultOptions(), func(sink *memSink) {
			for i := range uint64(4) {
				sink.have.Set(i)
			}
		})
		if err := waitErr(t, mustRun(r.sess, context.Background()), 5*time.Second); err != nil {
			t.Errorf("Run = %v", err)
		}
	})
}

// newSilentChannel 造一个无对端的空闲通道(仅生命周期测试用)。
func newSilentChannel() *memChannel {
	a, _ := newPipe(1)
	return a
}

// Sink 写失败是本地致命错误(磁盘满/路径拒绝),会话立即失败。
func TestReceiveSinkWriteError(t *testing.T) {
	r := newRig(t, 4, 64, allIndices(4), defaultOptions())
	boom := errors.New("disk full")
	r.sink.errAt[2] = boom
	r.addSender(64)

	if err := waitErr(t, mustRun(r.sess, context.Background()), 5*time.Second); !errors.Is(err, boom) {
		t.Fatalf("Run = %v, want disk full", err)
	}
}

// 有序模式下写失败发生在重排缓冲 flush 路径,同样致命。
func TestReceiveSinkWriteErrorOrdered(t *testing.T) {
	opts := defaultOptions()
	r := newOrderedRig(t, 4, 64, allIndices(4), opts)
	boom := errors.New("disk full")
	r.sink.errAt[2] = boom
	r.addScripted(64, func(indices []uint64, send func(Frame)) {
		for i := len(indices) - 1; i >= 0; i-- { // 倒序回帧,逼块 2 走 flush 路径
			for _, f := range sealedFrames(r.t, r.codec, r.store, indices[i], MaxBlockPayload) {
				send(f)
			}
		}
	})
	if err := waitErr(t, mustRun(r.sess, context.Background()), 5*time.Second); !errors.Is(err, boom) {
		t.Fatalf("Run = %v, want disk full", err)
	}
}

// 零值 Options 字段归一到具名默认值。
func TestReceiveOptionDefaults(t *testing.T) {
	sess, err := NewReceiveSession(chunkSet(t), newMemSink(1), &fakeCodec{}, Options{MaxBlockBytes: 1})
	if err != nil {
		t.Fatalf("NewReceiveSession: %v", err)
	}
	if sess.opts.RequestTimeout != DefaultRequestTimeout {
		t.Errorf("RequestTimeout = %v", sess.opts.RequestTimeout)
	}
	if sess.opts.MaxBlockAttempts != DefaultBlockAttempts {
		t.Errorf("MaxBlockAttempts = %d", sess.opts.MaxBlockAttempts)
	}
	if sess.deliveryOrder != DeliveryAnyOrder {
		t.Errorf("delivery order = %d, want DeliveryAnyOrder", sess.deliveryOrder)
	}
}

func TestNewReceiveSessionRejectsInvalidSinkDeliveryOrder(t *testing.T) {
	sink := newMemSink(1)
	sink.order = DeliveryOrder(255)
	if _, err := NewReceiveSession(chunkSet(t), sink, &fakeCodec{}, Options{MaxBlockBytes: 1}); !errors.Is(err, ErrInvalidDeliveryOrder) {
		t.Fatalf("NewReceiveSession = %v, want ErrInvalidDeliveryOrder", err)
	}
}

func TestNewReceiveSessionValidation(t *testing.T) {
	sink := newMemSink(4)
	codec := &fakeCodec{}
	ok := defaultOptions()
	tests := []struct {
		name string
		err  error
		want error
	}{
		{"nil sink", func() error { _, err := NewReceiveSession(chunkSet(t), nil, codec, ok); return err }(), ErrNilDependency},
		{"nil opener", func() error { _, err := NewReceiveSession(chunkSet(t), sink, nil, ok); return err }(), ErrNilDependency},
		{"MaxBlockBytes 缺失", func() error {
			_, err := NewReceiveSession(chunkSet(t), sink, codec, Options{})
			return err
		}(), ErrInvalidOptions},
		{"负超时", func() error {
			_, err := NewReceiveSession(chunkSet(t), sink, codec, Options{MaxBlockBytes: 1, RequestTimeout: -1})
			return err
		}(), ErrInvalidOptions},
		{"负尝试数", func() error {
			_, err := NewReceiveSession(chunkSet(t), sink, codec, Options{MaxBlockBytes: 1, MaxBlockAttempts: -1})
			return err
		}(), ErrInvalidOptions},
		{"selection exceeds sink geometry", func() error {
			_, err := NewReceiveSession(chunkSet(t, 4), sink, codec, ok)
			return err
		}(), ErrInvalidOptions},
	}
	for _, tt := range tests {
		if !errors.Is(tt.err, tt.want) {
			t.Errorf("%s: %v, want %v", tt.name, tt.err, tt.want)
		}
	}
	if err := func() error {
		sess, err := NewReceiveSession(chunkSet(t), sink, codec, ok)
		if err != nil {
			return err
		}
		return sess.AddChannel(nil)
	}(); !errors.Is(err, ErrNilDependency) {
		t.Errorf("AddChannel(nil) = %v", err)
	}
}
