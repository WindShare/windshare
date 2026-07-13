package forward

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/windshare/windshare/relay/protocol"
)

var (
	s1 = protocol.SessionID{1}
	s2 = protocol.SessionID{2}
)

type write struct {
	binary bool
	data   string
}

// gatedWriter 把每次写拆成"进入(entered)→ 放行(result)"两步握手,
// 使"写者正阻塞在某次写上"成为测试可显式等待的状态,全程无 sleep。
type gatedWriter struct {
	entered chan write
	result  chan error
}

func newGatedWriter() *gatedWriter {
	return &gatedWriter{entered: make(chan write), result: make(chan error)}
}

func (w *gatedWriter) write(binary bool, data []byte) error {
	w.entered <- write{binary: binary, data: string(data)}
	return <-w.result
}

func (w *gatedWriter) WriteText(data []byte) error   { return w.write(false, data) }
func (w *gatedWriter) WriteBinary(data []byte) error { return w.write(true, data) }

// awaitEntered 等待写者取走一项并阻塞其上。
func (w *gatedWriter) awaitEntered(t *testing.T) write {
	t.Helper()
	select {
	case wr := <-w.entered:
		return wr
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for write")
		return write{}
	}
}

// finish 放行当前阻塞的写。
func (w *gatedWriter) finish() { w.result <- nil }

// drain 放行并收集接下来 n 次写(含当前阻塞的一次,若有须先 awaitEntered)。
func (w *gatedWriter) drain(t *testing.T, n int) []write {
	t.Helper()
	got := make([]write, 0, n)
	for range n {
		got = append(got, w.awaitEntered(t))
		w.finish()
	}
	return got
}

func TestHighLanePreemptsForward(t *testing.T) {
	w := newGatedWriter()
	p := NewPump(w, Options{})
	defer p.Close()
	p.OpenSession(s1)

	if p.EnqueueForward(s1, []byte("lowA")) != Enqueued {
		t.Fatal("failed to enqueue lowA")
	}
	first := w.awaitEntered(t) // 写者持 lowA 阻塞;此后的入队都发生在其阻塞期间
	if first.data != "lowA" {
		t.Fatalf("first write = %+v", first)
	}
	if p.EnqueueForward(s1, []byte("lowB")) != Enqueued {
		t.Fatal("failed to enqueue lowB")
	}
	if p.EnqueueConnection(false, []byte("sig")) != Enqueued {
		t.Fatal("connection control enqueue failed")
	}
	w.finish()
	got := w.drain(t, 2)
	want := []write{{false, "sig"}, {true, "lowB"}}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("item %d = %+v, want %+v because signaling must take priority", i, got[i], want[i])
		}
	}
}

func TestBorrowedConnectionReceiptCompletesAfterWrite(t *testing.T) {
	w := newGatedWriter()
	p := NewPump(w, Options{})
	defer p.Close()
	result, receipt := p.EnqueueConnectionBorrowed(true, []byte("manifest"))
	if result != Enqueued || receipt == nil {
		t.Fatalf("enqueue = %v/%v", result, receipt)
	}
	if got := w.awaitEntered(t); !got.binary || got.data != "manifest" {
		t.Fatalf("write = %+v", got)
	}
	select {
	case err := <-receipt:
		t.Fatalf("receipt completed before write: %v", err)
	default:
	}
	w.finish()
	select {
	case err := <-receipt:
		if err != nil {
			t.Fatalf("receipt = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("receipt did not complete after write")
	}
}

func TestBorrowedConnectionReceiptSurvivesInFlightClose(t *testing.T) {
	w := newGatedWriter()
	p := NewPump(w, Options{})
	result, receipt := p.EnqueueConnectionBorrowed(true, []byte("manifest"))
	if result != Enqueued {
		t.Fatal(result)
	}
	w.awaitEntered(t)
	p.Close()
	select {
	case err := <-receipt:
		t.Fatalf("close released an in-flight borrow before its writer: %v", err)
	default:
	}
	w.finish()
	select {
	case err := <-receipt:
		if err != nil {
			t.Fatalf("completed in-flight write = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight borrow was not released")
	}
	<-p.Done()
}

func TestBorrowedConnectionReceiptFailsWhenQueuedPumpCloses(t *testing.T) {
	w := newGatedWriter()
	p := NewPump(w, Options{})
	if result := p.EnqueueConnection(false, []byte("blocking")); result != Enqueued {
		t.Fatal(result)
	}
	w.awaitEntered(t)
	result, receipt := p.EnqueueConnectionBorrowed(true, []byte("queued-manifest"))
	if result != Enqueued {
		t.Fatal(result)
	}
	p.Close()
	select {
	case err := <-receipt:
		if !errors.Is(err, ErrPumpClosed) {
			t.Fatalf("queued receipt = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued borrow was not cancelled")
	}
	w.finish()
	<-p.Done()
}

func TestPerSessionOverflowIsolated(t *testing.T) {
	w := newGatedWriter()
	p := NewPump(w, Options{SessionQueueFrames: 2})
	defer p.Close()
	p.OpenSession(s1)
	p.OpenSession(s2)

	if p.EnqueueForward(s1, []byte{0}) != Enqueued {
		t.Fatal("failed to enqueue first frame")
	}
	w.awaitEntered(t) // 首帧已出队,写者阻塞;s1 队列此后稳定可填至容量
	for i := 1; i <= 2; i++ {
		if r := p.EnqueueForward(s1, []byte{byte(i)}); r != Enqueued {
			t.Fatalf("s1 frame %d = %v", i, r)
		}
	}
	if r := p.EnqueueForward(s1, []byte{9}); r != Overflow {
		t.Fatalf("s1 should overflow, got %v", r)
	}
	// s1 溢出不波及 s2:各会话独立限额。
	if r := p.EnqueueForward(s2, []byte{0}); r != Enqueued {
		t.Fatalf("s2 should be unaffected, got %v", r)
	}
}

func TestRoundRobinAcrossSessions(t *testing.T) {
	w := newGatedWriter()
	p := NewPump(w, Options{SessionQueueFrames: 8})
	defer p.Close()
	p.OpenSession(s1)
	p.OpenSession(s2)

	p.EnqueueForward(s1, []byte("a1"))
	w.awaitEntered(t) // a1 被持有;以下积压在两会话队列中
	p.EnqueueForward(s1, []byte("a2"))
	p.EnqueueForward(s1, []byte("a3"))
	p.EnqueueForward(s2, []byte("b1"))
	p.EnqueueForward(s2, []byte("b2"))
	w.finish()

	got := w.drain(t, 4)
	// a1 出队后轮转指针已越过 s1,故积压从 s2 起逐帧轮流。
	want := []string{"b1", "a2", "b2", "a3"}
	for i := range want {
		if got[i].data != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestCloseSessionDropsBacklog(t *testing.T) {
	w := newGatedWriter()
	p := NewPump(w, Options{})
	defer p.Close()
	p.OpenSession(s1)
	p.OpenSession(s2)

	p.EnqueueForward(s1, []byte("head"))
	w.awaitEntered(t)
	p.EnqueueForward(s1, []byte("dropped"))
	p.EnqueueForward(s2, []byte("kept"))
	p.CloseSession(s1)
	w.finish()

	if got := w.drain(t, 1); got[0].data != "kept" {
		t.Fatalf("got %v; s1 backlog should be discarded when the session closes", got)
	}
	if r := p.EnqueueForward(s1, []byte("late")); r != UnknownSession {
		t.Fatalf("closed session should return UnknownSession, got %v", r)
	}
}

func TestWriterErrorStopsPump(t *testing.T) {
	w := newGatedWriter()
	p := NewPump(w, Options{})
	p.OpenSession(s1)
	p.EnqueueForward(s1, []byte("x"))
	w.awaitEntered(t)
	boom := errors.New("boom")
	w.result <- boom

	select {
	case <-p.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Done was not closed")
	}
	if !errors.Is(p.Err(), boom) {
		t.Fatalf("Err = %v", p.Err())
	}
	if r := p.EnqueueForward(s1, []byte("y")); r != PumpClosed {
		t.Fatalf("closed pump should return PumpClosed, got %v", r)
	}
	if r := p.EnqueueConnection(false, []byte("z")); r != PumpClosed {
		t.Fatalf("connection enqueue after failure = %v, want PumpClosed", r)
	}
}

func TestConnectionLaneOverflow(t *testing.T) {
	w := newGatedWriter()
	p := NewPump(w, Options{ConnectionQueueMessages: 2})
	defer p.Close()

	if p.EnqueueConnection(false, []byte("h1")) != Enqueued {
		t.Fatal("first connection message enqueue failed")
	}
	w.awaitEntered(t)
	if p.EnqueueConnection(false, []byte("h2")) != Enqueued || p.EnqueueConnection(false, []byte("h3")) != Enqueued {
		t.Fatal("connection messages within capacity should enqueue")
	}
	if got := p.EnqueueConnection(false, []byte("h4")); got != Overflow {
		t.Fatalf("connection overflow = %v, want Overflow", got)
	}
	w.finish()
}

func TestSessionControlPrecedesOwnDataWithoutBreakingFairness(t *testing.T) {
	w := newGatedWriter()
	p := NewPump(w, Options{})
	defer p.Close()
	p.OpenSession(s1)
	p.OpenSession(s2)

	p.EnqueueForward(s1, []byte("a1"))
	if got := w.awaitEntered(t); got.data != "a1" {
		t.Fatalf("first write = %+v", got)
	}
	p.EnqueueForward(s1, []byte("a2"))
	p.EnqueueSessionControl(s1, false, []byte("signal-a"))
	p.EnqueueForward(s2, []byte("b1"))
	w.finish()

	got := w.drain(t, 3)
	want := []write{{true, "b1"}, {false, "signal-a"}, {true, "a2"}}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("write %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestTerminalPreemptsBacklogAndDrainsBeforeRemoval(t *testing.T) {
	w := newGatedWriter()
	p := NewPump(w, Options{})
	defer p.Close()
	p.OpenSession(s1)

	p.EnqueueForward(s1, []byte("in-flight"))
	if got := w.awaitEntered(t); got.data != "in-flight" {
		t.Fatalf("first write = %+v", got)
	}
	p.EnqueueForward(s1, []byte("discard-data"))
	p.EnqueueSessionControl(s1, false, []byte("discard-signal"))
	result, delivered := p.EnqueueSessionTerminal(s1, true, []byte("terminal"))
	if result != Enqueued {
		t.Fatalf("terminal enqueue = %v", result)
	}
	if got := p.EnqueueForward(s1, []byte("late")); got != SessionTerminated {
		t.Fatalf("late data = %v, want SessionTerminated", got)
	}
	w.finish()

	if got := w.awaitEntered(t); got != (write{binary: true, data: "terminal"}) {
		t.Fatalf("terminal write = %+v", got)
	}
	select {
	case <-delivered:
		t.Fatal("receipt completed before terminal writer returned")
	default:
	}
	w.finish()
	select {
	case err := <-delivered:
		if err != nil {
			t.Fatalf("terminal delivery = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("terminal delivery receipt timed out")
	}
	if got := p.EnqueueForward(s1, []byte("after")); got != UnknownSession {
		t.Fatalf("session after terminal drain = %v, want UnknownSession", got)
	}
}

func TestCloseSessionCannotRevokeAcceptedTerminal(t *testing.T) {
	w := newGatedWriter()
	p := NewPump(w, Options{})
	defer p.Close()
	p.OpenSession(s1)

	p.EnqueueForward(s1, []byte("in-flight"))
	if got := w.awaitEntered(t); got.data != "in-flight" {
		t.Fatalf("first write = %+v", got)
	}
	result, delivered := p.EnqueueSessionTerminal(s1, true, []byte("terminal"))
	if result != Enqueued {
		t.Fatalf("terminal enqueue = %v", result)
	}
	if got := p.CloseSession(s1); got != SessionTerminated {
		t.Fatalf("close after accepted terminal = %v, want SessionTerminated", got)
	}

	w.finish()
	if got := w.awaitEntered(t); got.data != "terminal" {
		t.Fatalf("write after close = %+v, want accepted terminal", got)
	}
	if got := p.OpenSession(s1); got != SessionTerminated {
		t.Fatalf("reopen while terminal is writing = %v, want SessionTerminated", got)
	}
	w.finish()
	select {
	case err := <-delivered:
		if err != nil {
			t.Fatalf("terminal delivery = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("terminal delivery receipt timed out")
	}
}

func TestPumpSnapshotsAcceptedPayloads(t *testing.T) {
	w := newGatedWriter()
	p := NewPump(w, Options{})
	defer p.Close()
	s3 := protocol.SessionID{3}
	p.OpenSession(s1)
	p.OpenSession(s2)
	p.OpenSession(s3)

	p.EnqueueForward(s1, []byte("gate"))
	if got := w.awaitEntered(t); got.data != "gate" {
		t.Fatalf("first write = %+v", got)
	}
	connection := []byte("connection")
	control := []byte("control")
	data := []byte("data")
	terminal := []byte("terminal")
	if got := p.EnqueueConnection(false, connection); got != Enqueued {
		t.Fatalf("connection enqueue = %v", got)
	}
	if got := p.EnqueueSessionControl(s2, false, control); got != Enqueued {
		t.Fatalf("control enqueue = %v", got)
	}
	if got := p.EnqueueForward(s3, data); got != Enqueued {
		t.Fatalf("data enqueue = %v", got)
	}
	result, delivered := p.EnqueueSessionTerminal(s1, true, terminal)
	if result != Enqueued {
		t.Fatalf("terminal enqueue = %v", result)
	}
	for _, payload := range [][]byte{connection, control, data, terminal} {
		for i := range payload {
			payload[i] = 'x'
		}
	}

	w.finish()
	got := w.drain(t, 4)
	want := []write{
		{binary: false, data: "connection"},
		{binary: false, data: "control"},
		{binary: true, data: "data"},
		{binary: true, data: "terminal"},
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("write %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if err := <-delivered; err != nil {
		t.Fatalf("terminal delivery = %v", err)
	}
}

func TestEnqueueForwardContextBackpressureAndCancellation(t *testing.T) {
	w := newGatedWriter()
	p := NewPump(w, Options{SessionQueueFrames: 1})
	defer p.Close()
	p.OpenSession(s1)

	p.EnqueueForward(s1, []byte("active"))
	if got := w.awaitEntered(t); got.data != "active" {
		t.Fatalf("first write = %+v", got)
	}
	p.EnqueueForward(s1, []byte("queued"))
	result := make(chan EnqueueResult, 1)
	go func() {
		result <- p.EnqueueForwardContext(context.Background(), s1, []byte("backpressured"))
	}()
	select {
	case got := <-result:
		t.Fatalf("full queue returned early with %v", got)
	case <-time.After(50 * time.Millisecond):
	}

	w.finish()
	select {
	case got := <-result:
		if got != Enqueued {
			t.Fatalf("backpressured enqueue = %v, want Enqueued", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("backpressured enqueue did not resume")
	}
	if got := w.awaitEntered(t); got.data != "queued" {
		t.Fatalf("second write = %+v", got)
	}
	w.finish()
	if got := w.awaitEntered(t); got.data != "backpressured" {
		t.Fatalf("third write = %+v", got)
	}
	w.finish()

	p.EnqueueForward(s1, []byte("active-again"))
	if got := w.awaitEntered(t); got.data != "active-again" {
		t.Fatalf("fourth write = %+v", got)
	}
	p.EnqueueForward(s1, []byte("queued-again"))
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if got := p.EnqueueForwardContext(canceled, s1, []byte("must-not-enqueue")); got != ContextDone {
		t.Fatalf("canceled enqueue = %v, want ContextDone", got)
	}
	w.finish()
	if got := w.awaitEntered(t); got.data != "queued-again" {
		t.Fatalf("write after cancellation = %+v", got)
	}
	w.finish()
}

// errProbeContext 上报 EnqueueForwardContext 每次 Err 检查。producer 从该检查
// 起持有 p.mu,直到 Cond.Wait 完成登记并释放锁;因此测试观察到一次检查后再拿到
// p.mu,即证明 producer 已挂起在 p.wake 上且没有在途唤醒。
type errProbeContext struct {
	context.Context
	checks chan struct{}
}

func (c *errProbeContext) Err() error {
	select {
	case c.checks <- struct{}{}:
	default:
	}
	return c.Context.Err()
}

// awaitParkedProducer 阻塞至由 ctx 驱动的 producer 挂起在 p.wake 上。前提是
// s1 队列已满且无人出队,Overflow(继而 Wait)是 producer 离开临界区的唯一
// 路径;临界区内顺带复核该前提。
func awaitParkedProducer(t *testing.T, p *Pump, ctx *errProbeContext) {
	t.Helper()
	select {
	case <-ctx.checks:
	case <-time.After(2 * time.Second):
		t.Fatal("producer never reached its context check")
	}
	p.mu.Lock()
	full := len(p.sessions[s1].data) >= p.opts.SessionQueueFrames
	p.mu.Unlock()
	if !full {
		t.Fatal("session queue drained before the producer parked")
	}
}

// TestEnqueueForwardContextWakesWhenDequeueFreesCapacity 钉死引发
// TestEnqueueForwardContextBackpressureAndCancellation 偶发超时的交错:写完成
// 后 run 的广播发生在队列仍满时,producer 被唤醒、复查仍 Overflow、再度挂起,
// 随后 next 才出队腾出容量。容量转换只发生在出队这一刻,故必须由出队本身唤醒
// producer——下一次写完成的广播可能任意远(测试写者被门控时则永不到来)。
func TestEnqueueForwardContextWakesWhenDequeueFreesCapacity(t *testing.T) {
	p := newPump(nil, Options{SessionQueueFrames: 1})
	defer p.Close()
	p.OpenSession(s1)
	if got := p.EnqueueForward(s1, []byte("full")); got != Enqueued {
		t.Fatalf("fill enqueue = %v", got)
	}

	ctx := &errProbeContext{Context: context.Background(), checks: make(chan struct{}, 1)}
	result := make(chan EnqueueResult, 1)
	go func() { result <- p.EnqueueForwardContext(ctx, s1, []byte("resume")) }()
	awaitParkedProducer(t, p, ctx)

	if it, ok := p.next(); !ok || string(it.data) != "full" {
		t.Fatalf("dequeue = %q/%v", it.data, ok)
	}
	select {
	case got := <-result:
		if got != Enqueued {
			t.Fatalf("parked producer resumed with %v, want Enqueued", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("dequeue freed a slot but never woke the parked producer")
	}
}

// 互补交错:出队先于 producer 到达,producer 必须直接看到空位立即返回,
// 不依赖任何唤醒。
func TestEnqueueForwardContextSeesCapacityWithoutWakeup(t *testing.T) {
	p := newPump(nil, Options{SessionQueueFrames: 1})
	defer p.Close()
	p.OpenSession(s1)
	if got := p.EnqueueForward(s1, []byte("full")); got != Enqueued {
		t.Fatalf("fill enqueue = %v", got)
	}
	if it, ok := p.next(); !ok || string(it.data) != "full" {
		t.Fatalf("dequeue = %q/%v", it.data, ok)
	}
	if got := p.EnqueueForwardContext(context.Background(), s1, []byte("after")); got != Enqueued {
		t.Fatalf("enqueue after dequeue = %v, want Enqueued", got)
	}
}

// 挂起中的 producer 必须能被取消唤醒(AfterFunc 广播边),且取消不得入队。
func TestEnqueueForwardContextCancelWakesParkedProducer(t *testing.T) {
	p := newPump(nil, Options{SessionQueueFrames: 1})
	defer p.Close()
	p.OpenSession(s1)
	if got := p.EnqueueForward(s1, []byte("full")); got != Enqueued {
		t.Fatalf("fill enqueue = %v", got)
	}

	cancellable, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &errProbeContext{Context: cancellable, checks: make(chan struct{}, 1)}
	result := make(chan EnqueueResult, 1)
	go func() { result <- p.EnqueueForwardContext(ctx, s1, []byte("never")) }()
	awaitParkedProducer(t, p, ctx)

	cancel()
	select {
	case got := <-result:
		if got != ContextDone {
			t.Fatalf("canceled parked producer = %v, want ContextDone", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation never woke the parked producer")
	}
	p.mu.Lock()
	if n := len(p.sessions[s1].data); n != 1 {
		p.mu.Unlock()
		t.Fatalf("queue length after canceled enqueue = %d, want 1", n)
	}
	p.mu.Unlock()
}

func TestSessionControlOverflowIsIsolated(t *testing.T) {
	w := newGatedWriter()
	p := NewPump(w, Options{SessionControlMessages: 1})
	defer p.Close()
	p.OpenSession(s1)
	p.OpenSession(s2)

	p.EnqueueSessionControl(s1, false, []byte("s1-active"))
	w.awaitEntered(t)
	if got := p.EnqueueSessionControl(s1, false, []byte("s1-queued")); got != Enqueued {
		t.Fatalf("queued control = %v", got)
	}
	if got := p.EnqueueSessionControl(s1, false, []byte("overflow")); got != Overflow {
		t.Fatalf("control overflow = %v, want Overflow", got)
	}
	if got := p.EnqueueSessionControl(s2, false, []byte("s2")); got != Enqueued {
		t.Fatalf("other session control = %v, want Enqueued", got)
	}
	w.finish()
	_ = w.drain(t, 2)
}

func TestWaitIdle(t *testing.T) {
	w := newGatedWriter()
	p := NewPump(w, Options{})
	defer p.Close()
	p.OpenSession(s1)

	// 空泵立即 idle。
	if !p.WaitIdle(context.Background()) {
		t.Fatal("empty pump should be idle")
	}

	p.EnqueueForward(s1, []byte("x"))
	w.awaitEntered(t) // 队列空了但写还在途:此刻不算 idle
	short, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if p.WaitIdle(short) {
		t.Fatal("pump should not be idle while a write is in flight")
	}

	idle := make(chan bool, 1)
	go func() { idle <- p.WaitIdle(context.Background()) }()
	w.finish()
	select {
	case ok := <-idle:
		if !ok {
			t.Fatal("pump should be idle after the write completes")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitIdle did not return")
	}

	p.Close()
	if p.WaitIdle(context.Background()) {
		t.Fatal("closed pump should not report idle")
	}
}

func TestCloseIdempotentAndDone(t *testing.T) {
	w := newGatedWriter()
	p := NewPump(w, Options{})
	p.OpenSession(s1)
	p.Close()
	p.Close()
	select {
	case <-p.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Done was not closed")
	}
	if !errors.Is(p.Err(), ErrPumpClosed) {
		t.Fatalf("Err = %v", p.Err())
	}
	// 关闭后一切操作安全无效。
	p.OpenSession(s2)
	if r := p.EnqueueForward(s2, []byte("x")); r != PumpClosed {
		t.Fatalf("got %v", r)
	}
	p.CloseSession(s1)
}

func TestCloseReleasesQueuedPayloads(t *testing.T) {
	w := newGatedWriter()
	p := NewPump(w, Options{})
	p.OpenSession(s1)
	p.OpenSession(s2)
	p.EnqueueForward(s1, []byte("in-flight"))
	w.awaitEntered(t)
	p.EnqueueConnection(false, []byte("connection"))
	p.EnqueueSessionControl(s1, false, []byte("control"))
	p.EnqueueForward(s2, []byte("data"))

	p.Close()
	p.mu.Lock()
	if p.connection != nil || p.sessions != nil || p.rr != nil {
		t.Fatalf("closed pump retained queue storage: connection=%d sessions=%d rr=%d", len(p.connection), len(p.sessions), len(p.rr))
	}
	p.mu.Unlock()
	w.finish()
	select {
	case <-p.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Done did not close after in-flight writer returned")
	}
}
