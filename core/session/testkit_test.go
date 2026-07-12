package session

// 测试基建:内存管道 FrameChannel(可注入丢帧/延迟/断连)、脚本化发送端、
// 确定性 fake 编解码器与内存 BlockStore/Sink。确定性要求(§7):测试路径
// 不落 crypto/rand,一切"随机"行为由固定内容或显式脚本给出。

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/core/layout"
)

// ---- 内存管道通道 ----

// memChannel 是内存 FrameChannel:newPipe 造一对互连端点,Send 入对端 Recv。
// onSend 钩子在发送方 goroutine 内运行,可 sleep(注入延迟)、返回 false
// (注入丢帧)或触发 Close(注入断连)。
type memChannel struct {
	mu      sync.Mutex
	state   ChannelState
	peer    *memChannel
	recv    chan Frame
	done    chan struct{}
	closed  atomic.Bool
	rw      sync.RWMutex // Close 等在途 deliver 离场后才 close(recv)
	onSend  func(Frame) bool
	sendLog []Frame
	// failNextSend 令下一次 Send 直接报错(状态仍 Open):模拟传输层写失败。
	failNextSend atomic.Bool
}

func newPipe(buf int) (a, b *memChannel) {
	a = &memChannel{state: Open, recv: make(chan Frame, buf), done: make(chan struct{})}
	b = &memChannel{state: Open, recv: make(chan Frame, buf), done: make(chan struct{})}
	a.peer, b.peer = b, a
	return a, b
}

func (c *memChannel) Send(ctx context.Context, f Frame) error {
	if c.closed.Load() {
		return errors.New("memChannel: closed")
	}
	if c.failNextSend.CompareAndSwap(true, false) {
		return errors.New("memChannel: injected send failure")
	}
	c.mu.Lock()
	c.sendLog = append(c.sendLog, f)
	hook := c.onSend
	c.mu.Unlock()
	if hook != nil && !hook(f) {
		return nil // 帧被注入逻辑吞掉:模拟传输中途丢失
	}
	return c.peer.deliver(ctx, f)
}

func (c *memChannel) SendTerminal(ctx context.Context, f Frame) error {
	if err := c.Send(ctx, f); err != nil {
		return err
	}
	// Send is synchronous in this test transport, so closing now proves the
	// peer observes the buffered terminal before Recv closes.
	return c.Close()
}

func (c *memChannel) deliver(ctx context.Context, f Frame) error {
	c.rw.RLock()
	defer c.rw.RUnlock()
	if c.closed.Load() {
		return nil // 送达时本端已关:帧无声消失,与真实断连一致
	}
	select {
	case c.recv <- f:
		return nil
	case <-c.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *memChannel) Recv() <-chan Frame { return c.recv }

func (c *memChannel) State() ChannelState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

func (c *memChannel) setState(s ChannelState) {
	c.mu.Lock()
	c.state = s
	c.mu.Unlock()
}

func (c *memChannel) setOnSend(fn func(Frame) bool) {
	c.mu.Lock()
	c.onSend = fn
	c.mu.Unlock()
}

// Close 单端关闭令双端 Closed(与真实连接的语义一致),并关闭本端入站流。
func (c *memChannel) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	c.setState(Closed)
	close(c.done) // 先令在途 deliver 退出 select,再独占关 recv
	c.rw.Lock()
	close(c.recv)
	c.rw.Unlock()
	_ = c.peer.Close()
	return nil
}

// requestsSent 解码本端发出过的全部 REQUEST 帧(按发送顺序)。
func (c *memChannel) requestsSent(t *testing.T) [][]uint64 {
	t.Helper()
	c.mu.Lock()
	log := append([]Frame(nil), c.sendLog...)
	c.mu.Unlock()
	var out [][]uint64
	for _, f := range log {
		if msg, err := Decode(f); err == nil {
			if req, ok := msg.(*Request); ok {
				out = append(out, req.Indices)
			}
		}
	}
	return out
}

// blocksSent 统计本端发出过 BLOCK 帧的块号集合。
func (c *memChannel) blocksSent(t *testing.T) map[uint64]bool {
	t.Helper()
	c.mu.Lock()
	log := append([]Frame(nil), c.sendLog...)
	c.mu.Unlock()
	out := map[uint64]bool{}
	for _, f := range log {
		if msg, err := Decode(f); err == nil {
			if b, ok := msg.(*Block); ok {
				out[b.Index] = true
			}
		}
	}
	return out
}

// ---- fake 编解码器 ----

const fakeNonceLen, fakeTagLen = 12, 16

// fakeCodec 模拟 chunk.Codec 的封形(nonce‖pt‖tag)而不做真加密:nonce 含
// 每次 Seal 的全局计数(重发不可混拼,与随机 nonce 的关键性质一致),tag
// 绑定块号+nonce+内容(错拼/换块必被 Open 拒绝)。
type fakeCodec struct {
	mu      sync.Mutex
	seals   uint64
	sealErr error
}

func fakeTag(index uint64, body []byte) []byte {
	h := fnv.New128a()
	var ib [8]byte
	binary.LittleEndian.PutUint64(ib[:], index)
	h.Write(ib[:])
	h.Write(body)
	return h.Sum(nil)
}

func (c *fakeCodec) Seal(index uint64, plaintext []byte) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sealErr != nil {
		return nil, c.sealErr
	}
	c.seals++
	body := make([]byte, fakeNonceLen, fakeNonceLen+len(plaintext)+fakeTagLen)
	binary.LittleEndian.PutUint64(body, c.seals)
	binary.LittleEndian.PutUint32(body[8:], uint32(index))
	body = append(body, plaintext...)
	return append(body, fakeTag(index, body)...), nil
}

func (c *fakeCodec) Open(index uint64, blockCT []byte) ([]byte, error) {
	if len(blockCT) < fakeNonceLen+fakeTagLen {
		return nil, errors.New("fakeCodec: block ciphertext is too short")
	}
	body := blockCT[:len(blockCT)-fakeTagLen]
	if !bytes.Equal(fakeTag(index, body), blockCT[len(blockCT)-fakeTagLen:]) {
		return nil, errors.New("fakeCodec: authentication tag mismatch")
	}
	return append([]byte(nil), body[fakeNonceLen:]...), nil
}

// ---- 内存 BlockStore / Sink ----

type memStore struct {
	mu     sync.Mutex
	blocks [][]byte
	errAt  map[uint64]error
}

func newMemStore(n, blockSize int) *memStore {
	s := &memStore{errAt: map[uint64]error{}}
	for i := range n {
		b := make([]byte, blockSize)
		for k := range b {
			b[k] = byte(i*131 + k*7 + 5) // 非平凡且可复现的内容
		}
		s.blocks = append(s.blocks, b)
	}
	return s
}

func (s *memStore) ReadBlock(index uint64) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.errAt[index]; err != nil {
		return nil, err
	}
	if index >= uint64(len(s.blocks)) {
		return nil, fmt.Errorf("memStore: block %d is out of range", index)
	}
	return s.blocks[index], nil
}

func (s *memStore) BlockCount() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return uint64(len(s.blocks))
}

type memSink struct {
	mu      sync.Mutex
	have    Bitfield
	writes  []uint64
	data    map[uint64][]byte
	errAt   map[uint64]error
	onWrite func(index uint64)
	order   DeliveryOrder
}

func newMemSink(n uint64) *memSink {
	return &memSink{have: NewBitfield(n), data: map[uint64][]byte{}, errAt: map[uint64]error{}}
}

func (s *memSink) WriteBlock(index uint64, plaintext []byte) error {
	s.mu.Lock()
	if err := s.errAt[index]; err != nil {
		s.mu.Unlock()
		return err
	}
	s.writes = append(s.writes, index)
	s.data[index] = append([]byte(nil), plaintext...)
	s.have.Set(index)
	cb := s.onWrite
	s.mu.Unlock()
	if cb != nil {
		cb(index)
	}
	return nil
}

func (s *memSink) Have() Bitfield { return s.have }

func (s *memSink) DeliveryOrder() DeliveryOrder { return s.order }

func (s *memSink) writeOrder() []uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]uint64(nil), s.writes...)
}

// ---- 端到端环境 ----

// rig 把真实 SendSession 挂在管道对端,驱动 ReceiveSession 做端到端调度测试。
type rig struct {
	t     *testing.T
	codec *fakeCodec
	store *memStore
	sink  *memSink
	sess  *ReceiveSession
}

func defaultOptions() Options {
	return Options{MaxBlockBytes: 1 << 20, RequestTimeout: 2 * time.Second}
}

func newRig(t *testing.T, nBlocks, blockSize int, selected []uint64, opts Options) *rig {
	return newRigPrepped(t, nBlocks, blockSize, selected, opts, nil)
}

func newOrderedRig(t *testing.T, nBlocks, blockSize int, selected []uint64, opts Options) *rig {
	return newRigPrepped(t, nBlocks, blockSize, selected, opts, func(sink *memSink) {
		sink.order = DeliveryAscending
	})
}

// newRigPrepped 在会话构造前给 prep 一次预置 Sink 的机会——需求集在构造期
// 由 Have() 扣减,续传位必须先于会话存在。
func newRigPrepped(t *testing.T, nBlocks, blockSize int, selected []uint64, opts Options, prep func(*memSink)) *rig {
	t.Helper()
	r := &rig{
		t:     t,
		codec: &fakeCodec{},
		store: newMemStore(nBlocks, blockSize),
		sink:  newMemSink(uint64(nBlocks)),
	}
	if prep != nil {
		prep(r.sink)
	}
	sess, err := NewReceiveSession(chunkSet(t, selected...), r.sink, r.codec, opts)
	if err != nil {
		t.Fatalf("NewReceiveSession: %v", err)
	}
	r.sess = sess
	return r
}

// addSender 建一条管道:发送端挂真实 SendSession,接收端交给调度器。
// 返回两端(sender 端供注入钩子/统计 BLOCK,receiver 端供统计 REQUEST)。
func (r *rig) addSender(buf int) (senderEnd, receiverEnd *memChannel) {
	senderEnd, receiverEnd = newPipe(buf)
	ss, err := NewSendSession(senderEnd, r.store, r.codec)
	if err != nil {
		r.t.Errorf("NewSendSession: %v", err)
		return senderEnd, receiverEnd
	}
	go func() { _ = ss.Run(context.Background()) }()
	r.t.Cleanup(func() { _ = ss.Close() })
	if err := r.sess.AddChannel(receiverEnd); err != nil {
		r.t.Errorf("AddChannel: %v", err)
	}
	return senderEnd, receiverEnd
}

// addScripted 建一条管道:发送端挂脚本化对端(精确控制回帧顺序/丢弃),
// 接收端交给调度器。
func (r *rig) addScripted(buf int, respond func(indices []uint64, send func(Frame))) (senderEnd, receiverEnd *memChannel) {
	senderEnd, receiverEnd = newPipe(buf)
	startScript(r.t, senderEnd, respond)
	if err := r.sess.AddChannel(receiverEnd); err != nil {
		r.t.Errorf("AddChannel: %v", err)
	}
	return senderEnd, receiverEnd
}

// verify 校验选中块全部落地且内容与源一致。
func (r *rig) verify(selected []uint64) {
	r.t.Helper()
	r.sink.mu.Lock()
	defer r.sink.mu.Unlock()
	for _, idx := range selected {
		got, ok := r.sink.data[idx]
		if !ok {
			r.t.Errorf("block %d was not delivered", idx)
			continue
		}
		if !bytes.Equal(got, r.store.blocks[idx]) {
			r.t.Errorf("block %d content mismatch", idx)
		}
	}
}

func allIndices(n int) []uint64 {
	out := make([]uint64, n)
	for i := range out {
		out[i] = uint64(i)
	}
	return out
}

func chunkSet(t *testing.T, chunks ...uint64) layout.ChunkSet {
	t.Helper()
	ranges := make([]layout.ChunkRange, len(chunks))
	for i, chunk := range chunks {
		ranges[i] = layout.ChunkRange{First: chunk, End: chunk + 1}
	}
	set, err := layout.NewChunkSet(ranges...)
	if err != nil {
		t.Fatalf("NewChunkSet: %v", err)
	}
	return set
}

// ---- 脚本化发送端(需要精确控制回帧顺序/丢弃时用) ----

// startScript 在 ch 上跑脚本化对端:每收到一个 REQUEST 调一次 respond,
// respond 经 send 回帧。Recv 关闭即退出;Cleanup 关通道并等 goroutine 收尾。
func startScript(t *testing.T, ch *memChannel, respond func(indices []uint64, send func(Frame))) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for f := range ch.Recv() {
			msg, err := Decode(f)
			if err != nil {
				continue
			}
			if req, ok := msg.(*Request); ok {
				respond(req.Indices, func(out Frame) { _ = ch.Send(context.Background(), out) })
			}
		}
	}()
	t.Cleanup(func() {
		_ = ch.Close()
		<-done
	})
}

// sealedFrames 现封现切:每次调用都换新 nonce(计数递增),与真实重发一致。
// 常被脚本 goroutine 调用,故失败走 Errorf(FailNow 只许测试 goroutine 用)。
func sealedFrames(t *testing.T, codec *fakeCodec, store *memStore, index uint64, maxPayload int) []Frame {
	t.Helper()
	plaintext, err := store.ReadBlock(index)
	if err != nil {
		t.Errorf("ReadBlock(%d): %v", index, err)
		return nil
	}
	blockCT, err := codec.Seal(index, plaintext)
	if err != nil {
		t.Errorf("Seal(%d): %v", index, err)
		return nil
	}
	frames, err := SplitBlockCT(index, blockCT, maxPayload)
	if err != nil {
		t.Errorf("SplitBlockCT(%d): %v", index, err)
		return nil
	}
	return frames
}

// ---- goroutine 泄漏检查 ----

// checkNoLeak 在测试开头注册:Cleanup 时(LIFO,最后执行)等 goroutine 数
// 回落到起点,超时即判泄漏。仅用于串行测试。
func checkNoLeak(t *testing.T) {
	t.Helper()
	start := runtime.NumGoroutine()
	t.Cleanup(func() {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if runtime.NumGoroutine() <= start {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Errorf("goroutine leak: started with %d, ended with %d", start, runtime.NumGoroutine())
	})
}

// mustRun 在辅助 goroutine 里跑 Run 并把结果送回。
func mustRun(sess interface{ Run(context.Context) error }, ctx context.Context) <-chan error {
	done := make(chan error, 1)
	go func() { done <- sess.Run(ctx) }()
	return done
}

// waitErr 等 Run 结束,超时 fatal。
func waitErr(t *testing.T, done <-chan error, timeout time.Duration) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for Run (%v)", timeout)
		return nil
	}
}
