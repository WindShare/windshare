package session

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/windshare/windshare/core/layout"
)

// 调度默认值(初始默认、依实测调优;§8 同规:一律具名常量)。
const (
	// DefaultRequestTimeout:单块在途超时。慢路径(经中转转发)搬完一个
	// 1 MiB 块的往返也应远小于此;超时视为该次分配失败,丢部分帧换通道重试。
	DefaultRequestTimeout = 10 * time.Second

	// DefaultBlockAttempts:单块尝试上限(含首次)。持续故障下无限重试只是
	// 把失败拖成永久等待,耗尽即让会话以明确错误收场。
	DefaultBlockAttempts = 8
)

var (
	// ErrInvalidOptions:Options 字段越出定义域。
	ErrInvalidOptions = errors.New("session: invalid options")

	// ErrBlockExhausted:某块尝试次数耗尽(超时/校验失败累计)。
	ErrBlockExhausted = errors.New("session: block retry attempts exhausted")
)

// Options 配置接收会话的调度行为。
type Options struct {
	// MaxBlockBytes(必填)是单块密文的重组上限。调度器对清单无感、不知
	// chunkSize,上限须由加密 suite 的权威封装尺寸给出;缺了它,异常/恶意
	// 发送端能用不带 last 位的帧流无限撑大重组内存。
	MaxBlockBytes int64

	// RequestTimeout 是单块在途超时;零值取 DefaultRequestTimeout。
	RequestTimeout time.Duration

	// MaxBlockAttempts 是单块尝试上限(含首次);零值取 DefaultBlockAttempts。
	MaxBlockAttempts int
}

// flight 是一次在途分配:块号 → 承接通道与发出时刻。
type flight struct {
	ce      *chanEntry
	sentAt  time.Time
	pending bool
}

// chanEntry 是池内通道的调度视角:请求队列即 inflight 窗口,重组缓冲按通道
// 隔离(帧只认当前分配的来路,见 reassembly)。
type chanEntry struct {
	ch       FrameChannel
	retired  bool // 已退服:不再派活,但条目暂留池中直至入站流处理完
	inflight map[uint64]struct{}
	partial  map[uint64]*reassembly
	sending  *requestSend

	// score 是「REQUEST 发出 → 整块完成」耗时的 EWMA(纳秒):含排队与多帧
	// 搬运,不是精确 RTT,但足够回答"下一块给谁"。0 = 新通道未证明,排序
	// 最优——热切换要求刚 Open 的 P2P 立刻拿块自证,而非等旧通道空窗(§6.6)。
	score float64
}

// requestSend is one channel-local REQUEST write. Transport backpressure must
// not stop the event loop from scheduling healthy siblings, so at most one
// write per channel runs outside the loop while its indices remain reserved.
type requestSend struct {
	indices []uint64
	cancel  context.CancelFunc
}

// chanEvent 是 pump 汇入事件循环的入站事件。
type chanEvent struct {
	ce      *chanEntry
	f       Frame
	down    bool // 通道入站流已关闭
	send    *requestSend
	sendErr error
}

// ReceiveSession 是接收侧调度器,块协议只在此实现一次(§3):对一组
// FrameChannel 维护需求集、每通道请求窗口、超时重试、帧重组、评分与热切换。
// 它对"清单/文件"无感,只认块号;对传输无感,只认 FrameChannel;密文只经
// 注入的 Opener 验证解密。
//
// 并发模型:调度状态只由 Run 的单一事件循环触碰(免锁、行为可复现),每条
// 通道一个 pump goroutine 把入站帧汇入 events,另至多一条可取消的 REQUEST
// writer 防止传输背压堵住健康兄弟;AddChannel/Close 经受锁的交接区与信号
// 通道进入循环。
type ReceiveSession struct {
	sink          Sink
	opener        Opener
	opts          Options
	deliveryOrder DeliveryOrder
	tick          time.Duration

	mu         sync.Mutex
	pendingAdd []FrameChannel
	runCancel  context.CancelFunc
	finished   bool

	kick      chan struct{}
	events    chan chanEvent
	loopDone  chan struct{}
	closed    chan struct{}
	started   atomic.Bool
	closeOnce sync.Once
	pumps     sync.WaitGroup

	// —— 以下调度状态仅事件循环触碰。
	selectedRanges []layout.ChunkRange // immutable compact demand geometry
	scanRange      int
	scanAt         uint64
	remaining      uint64
	orderedWindow  []uint64 // lowest missing chunks; bounded by InFlightWindow
	retry          []uint64 // unordered failures awaiting reassignment; bounded by in-flight work
	assigned       map[uint64]*flight
	attempts       map[uint64]int
	buffered       map[uint64][]byte // verified, not yet delivered (ordered mode only)
	maxBuffered    int               // reorder peak in blocks; white-box bound assertion
	channels       []*chanEntry
}

// NewReceiveSession constructs demand as selected ChunkSet minus sink.Have().
// Selection stays interval-compact; only active window, retry, and reassembly
// state scale with concurrency. Channels subsequently enter through AddChannel.
func NewReceiveSession(selected layout.ChunkSet, sink Sink, opener Opener, opts Options) (*ReceiveSession, error) {
	if sink == nil || opener == nil {
		return nil, fmt.Errorf("%w: sink and opener are required", ErrNilDependency)
	}
	if opts.MaxBlockBytes <= 0 {
		return nil, fmt.Errorf("%w: MaxBlockBytes is required and must include chunkSize plus encryption overhead", ErrInvalidOptions)
	}
	if opts.RequestTimeout < 0 || opts.MaxBlockAttempts < 0 {
		return nil, fmt.Errorf("%w: RequestTimeout and MaxBlockAttempts must not be negative", ErrInvalidOptions)
	}
	if opts.RequestTimeout == 0 {
		opts.RequestTimeout = DefaultRequestTimeout
	}
	if opts.MaxBlockAttempts == 0 {
		opts.MaxBlockAttempts = DefaultBlockAttempts
	}
	deliveryOrder := sink.DeliveryOrder()
	if deliveryOrder != DeliveryAnyOrder && deliveryOrder != DeliveryAscending {
		return nil, fmt.Errorf("%w: %d", ErrInvalidDeliveryOrder, deliveryOrder)
	}
	have := sink.Have()
	ranges := selected.Ranges()
	var completed uint64
	for _, chunkRange := range ranges {
		if chunkRange.End > have.Len() {
			return nil, fmt.Errorf("%w:selected range [%d,%d) exceeds sink geometry %d", ErrInvalidOptions, chunkRange.First, chunkRange.End, have.Len())
		}
		completed += have.countRange(chunkRange.First, chunkRange.End)
	}
	r := &ReceiveSession{
		sink:           sink,
		opener:         opener,
		opts:           opts,
		deliveryOrder:  deliveryOrder,
		selectedRanges: ranges,
		remaining:      selected.Count() - completed,
		assigned:       map[uint64]*flight{},
		attempts:       map[uint64]int{},
		buffered:       map[uint64][]byte{},
		kick:           make(chan struct{}, 1),
		events:         make(chan chanEvent),
		loopDone:       make(chan struct{}),
		closed:         make(chan struct{}),
	}
	if r.ordered() {
		r.fillOrderedWindow()
	}
	// tick 驱动超时扫描与 Connecting→Open 的状态轮询(State 是拉模型,没有
	// 事件可等)。取超时的 1/4:下限防忙转,上限保热切换的感知延迟。
	r.tick = min(max(opts.RequestTimeout/4, time.Millisecond), 250*time.Millisecond)
	return r, nil
}

func (r *ReceiveSession) ordered() bool { return r.deliveryOrder == DeliveryAscending }

// AddChannel 把通道交给会话(热切换入池,§6.6):Connecting 也可先入池,
// 进 Open 才参与分配。入池即移交所有权,会话结束时统一关闭;会话已结束则
// 拒收(返回 ErrSessionClosed),通道仍归调用方。
func (r *ReceiveSession) AddChannel(ch FrameChannel) error {
	if ch == nil {
		return fmt.Errorf("%w:ch", ErrNilDependency)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finished {
		return ErrSessionClosed
	}
	r.pendingAdd = append(r.pendingAdd, ch)
	select {
	case r.kick <- struct{}{}:
	default:
	}
	return nil
}

// Run 阻塞调度,直到需求集清空(返回 nil)、上下文取消、Close 或致命错误。
// 空文件/空目录的物化等收尾不归调度器(§6.6,由 share/osfs 侧完成)。
func (r *ReceiveSession) Run(ctx context.Context) error {
	if !r.started.CompareAndSwap(false, true) {
		return ErrSessionReused
	}
	runCtx, cancel := context.WithCancel(ctx)
	r.mu.Lock()
	r.runCancel = cancel
	select {
	case <-r.closed:
		cancel()
	default:
	}
	r.mu.Unlock()
	defer func() {
		// Request writers use runCtx and are included in pumps. Cancellation must
		// precede teardown's Wait so a transport blocked in Send can leave.
		cancel()
		r.teardown()
	}()
	ticker := time.NewTicker(r.tick)
	defer ticker.Stop()
	for {
		r.adopt()
		r.reapClosed()
		if err := r.deliverReady(); err != nil {
			return err
		}
		if r.remaining == 0 {
			return nil
		}
		if err := r.schedule(runCtx); err != nil {
			select {
			case <-r.closed:
				return ErrSessionClosed
			default:
			}
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-r.closed:
			return ErrSessionClosed
		case <-r.kick:
		case <-ticker.C:
			if err := r.expire(); err != nil {
				return err
			}
		case ev := <-r.events:
			if ev.send != nil {
				r.finishRequestSend(ev)
				break
			}
			if ev.down {
				r.dropChannel(ev.ce)
				break
			}
			if err := r.onFrame(ev.ce, ev.f); err != nil {
				return err
			}
		}
	}
}

// Close 幂等地终止会话;正在 Run 的循环以 ErrSessionClosed 退出并回收资源。
// 从未 Run 过的会话由此处就地关闭已入池通道(不会再有 teardown 来收)。
func (r *ReceiveSession) Close() error {
	r.closeOnce.Do(func() { close(r.closed) })
	r.mu.Lock()
	cancel := r.runCancel
	var orphans []FrameChannel
	if !r.started.Load() {
		r.finished = true
		orphans = r.pendingAdd
		r.pendingAdd = nil
	}
	r.mu.Unlock()
	if cancel != nil {
		// Send is the only potentially blocking scheduler operation with an
		// explicit cancellation contract. Interrupt it so Close does not depend on
		// transport queue progress before the event loop can observe r.closed.
		cancel()
	}
	for _, ch := range orphans {
		_ = ch.Close()
	}
	return nil
}

// teardown 终结全部资源:拒绝后续入池、停 pump、关闭池内与未收编的通道。
// goroutine 不泄漏由 pump 对 loopDone 的 select 与此处的 Wait 共同保证。
func (r *ReceiveSession) teardown() {
	r.mu.Lock()
	r.finished = true
	r.runCancel = nil
	orphans := r.pendingAdd
	r.pendingAdd = nil
	r.mu.Unlock()
	close(r.loopDone)
	for _, ce := range r.channels {
		_ = ce.ch.Close()
	}
	for _, ch := range orphans {
		_ = ch.Close()
	}
	r.pumps.Wait()
}

// adopt 收编 AddChannel 交来的通道:入池并起 pump。Connecting 状态照收,
// schedule 只对 Open 通道派活。
func (r *ReceiveSession) adopt() {
	r.mu.Lock()
	add := r.pendingAdd
	r.pendingAdd = nil
	r.mu.Unlock()
	for _, ch := range add {
		ce := &chanEntry{
			ch:       ch,
			inflight: map[uint64]struct{}{},
			partial:  map[uint64]*reassembly{},
		}
		r.channels = append(r.channels, ce)
		r.pumps.Add(1)
		go r.pump(ce)
	}
}

// pump 把一条通道的入站帧汇入事件循环;入站流关闭或会话终结即退出。
func (r *ReceiveSession) pump(ce *chanEntry) {
	defer r.pumps.Done()
	for {
		select {
		case <-r.loopDone:
			return
		case f, ok := <-ce.ch.Recv():
			select {
			case r.events <- chanEvent{ce: ce, f: f, down: !ok}:
				if !ok {
					return
				}
			case <-r.loopDone:
				return
			}
		}
	}
}

// reapClosed 轮询通道状态:Closed 即退服(热切换的另一半——断连转移在途块)。
func (r *ReceiveSession) reapClosed() {
	for _, ce := range r.channels {
		if !ce.retired && ce.ch.State() == Closed {
			r.retireChannel(ce)
		}
	}
}

// retireChannel 让通道退出服务:在途分配退回未派状态、部分重组帧丢弃
// (§6.12:两次发送 nonce 不同,残帧无法与重发拼接,只能整块重取)、底层
// 关闭。退回的块由下轮 schedule 择优重派,尝试计数在重派时才累加——通道
// 故障不该吃掉块的预算。条目暂留池中:通道断开时 Recv 缓冲里可能还压着
// 关闭前合法送达的帧,尤其分享级 ERROR——丢了它会话就失去中止信号;
// 出池推迟到 pump 的 down 事件(入站流处理完毕)。
func (r *ReceiveSession) retireChannel(ce *chanEntry) {
	if ce.retired {
		return
	}
	ce.retired = true
	if ce.sending != nil {
		ce.sending.cancel()
	}
	_ = ce.ch.Close()
	for idx := range ce.inflight {
		if fl := r.assigned[idx]; fl != nil {
			r.releaseAssignment(idx, fl, true)
			// A disconnected transport provides no evidence that the block or
			// source is bad. Preserve the bounded failure budget for completed
			// ciphertext attempts (timeout or authentication failure), otherwise
			// repeated rejoins can make one later corrupt response fail immediately.
			r.refundAttempt(idx)
		}
	}
	clear(ce.inflight)
	clear(ce.partial)
}

// dropChannel 彻底摘除通道:退服并出池,其后到达的帧一律按迟到帧丢弃。
// 用于入站流已尽(down)与协议违规(对端不可信,余帧不值得处理)两种场合。
func (r *ReceiveSession) dropChannel(ce *chanEntry) {
	r.retireChannel(ce)
	if i := slices.Index(r.channels, ce); i >= 0 {
		r.channels = slices.Delete(r.channels, i, i+1)
	}
}

// deliverReady(有序模式)把重排缓冲里已就绪的队头依次交付,推进前沿;
// 无序模式缓冲恒空,天然 no-op。
func (r *ReceiveSession) deliverReady() error {
	for len(r.orderedWindow) > 0 {
		head := r.orderedWindow[0]
		plaintext, ok := r.buffered[head]
		if !ok {
			return nil
		}
		if err := r.sink.WriteBlock(head, plaintext); err != nil {
			return fmt.Errorf("session: sink write block %d: %w", head, err)
		}
		delete(r.buffered, head)
		r.orderedWindow = r.orderedWindow[1:]
		r.remaining--
		r.fillOrderedWindow()
	}
	return nil
}

// schedule 为每个空窗的 Open 通道派发请求。评分升序遍历:新通道(0 分)与
// 更快通道先拿块——"后续块优先给更优通道"(§6.6)由此成立;慢通道只在
// 优者满窗后才拿到剩余块,这即是聚合而非均分。
func (r *ReceiveSession) schedule(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	open := make([]*chanEntry, 0, len(r.channels))
	for _, ce := range r.channels {
		if !ce.retired && ce.ch.State() == Open {
			open = append(open, ce)
		}
	}
	slices.SortStableFunc(open, func(a, b *chanEntry) int { return cmp.Compare(a.score, b.score) })
	for _, ce := range open {
		if ce.sending != nil {
			continue
		}
		free := InFlightWindow - len(ce.inflight)
		if free <= 0 {
			continue
		}
		batch := r.eligible(free)
		if len(batch) == 0 {
			return nil // 升序候选耗尽:最优通道都无块可派,其余同样没有
		}
		f, err := EncodeRequest(batch)
		if err != nil {
			return err // 不可达:batch ≤ 窗口 ≪ MaxRequestIndices
		}
		sendCtx, cancel := context.WithTimeout(ctx, r.opts.RequestTimeout)
		request := &requestSend{indices: batch, cancel: cancel}
		ce.sending = request
		started := time.Now()
		for _, idx := range batch {
			// Pending demand is reserved while Send is in progress. Expiration starts
			// only after transport acceptance, but a very fast response may race the
			// send-result event and still needs a meaningful latency origin.
			r.assigned[idx] = &flight{ce: ce, sentAt: started, pending: true}
			ce.inflight[idx] = struct{}{}
		}
		r.pumps.Add(1)
		go r.sendRequest(sendCtx, ce, request, f)
	}
	return nil
}

func (r *ReceiveSession) sendRequest(ctx context.Context, ce *chanEntry, request *requestSend, frame Frame) {
	defer r.pumps.Done()
	err := ce.ch.Send(ctx, frame)
	request.cancel()
	select {
	case r.events <- chanEvent{ce: ce, send: request, sendErr: err}:
	case <-r.loopDone:
	}
}

func (r *ReceiveSession) finishRequestSend(ev chanEvent) {
	ce := ev.ce
	request := ev.send
	if ce.sending != request {
		return
	}
	ce.sending = nil
	if ce.retired {
		return
	}
	if ev.sendErr != nil {
		// A failed transport write owns no block attempt. Retiring the channel
		// releases both pending and already accepted work for healthy siblings.
		r.retireChannel(ce)
		return
	}
	now := time.Now()
	for _, idx := range request.indices {
		fl := r.assigned[idx]
		if fl == nil || fl.ce != ce || !fl.pending {
			continue
		}
		fl.pending = false
		fl.sentAt = now
		r.attempts[idx]++
	}
}

// eligible 从需求集挑至多 want 个未派块,升序——最小未交付块号永远排第一,
// 它的重试因此天然最高优先且落在当轮最优通道上(§6.6)。有序模式再叠加
// 重排窗约束:只允许最低的 InFlightWindow 个缺失块进入在途,已完成未交付块
// 全落在窗内,重排缓冲从而有界(≤ 在途窗口 × 块大小)。
func (r *ReceiveSession) eligible(want int) []uint64 {
	out := make([]uint64, 0, want)
	if r.ordered() {
		for _, idx := range r.orderedWindow {
			if len(out) == want {
				break
			}
			if _, inFlight := r.assigned[idx]; inFlight {
				continue
			}
			if _, done := r.buffered[idx]; done {
				continue
			}
			out = append(out, idx)
		}
		return out
	}

	for len(out) < want && len(r.retry) != 0 {
		idx := r.retry[0]
		r.retry = r.retry[1:]
		if !r.sink.Have().Get(idx) {
			out = append(out, idx)
		}
	}
	for len(out) < want {
		idx, ok := r.nextMissing()
		if !ok {
			break
		}
		out = append(out, idx)
	}
	return out
}

func (r *ReceiveSession) nextMissing() (uint64, bool) {
	have := r.sink.Have()
	for r.scanRange < len(r.selectedRanges) {
		chunkRange := r.selectedRanges[r.scanRange]
		start := max(r.scanAt, chunkRange.First)
		if idx, ok := have.nextClear(start, chunkRange.End); ok {
			r.scanAt = idx + 1
			return idx, true
		}
		r.scanRange++
		r.scanAt = 0
	}
	return 0, false
}

func (r *ReceiveSession) fillOrderedWindow() {
	for len(r.orderedWindow) < InFlightWindow {
		idx, ok := r.nextMissing()
		if !ok {
			return
		}
		r.orderedWindow = append(r.orderedWindow, idx)
	}
}

func (r *ReceiveSession) addRetry(idx uint64) {
	i, found := slices.BinarySearch(r.retry, idx)
	if found {
		return
	}
	r.retry = slices.Insert(r.retry, i, idx)
}

// expire 撤销超时在途:丢部分帧、退回需求池,由下轮 schedule 队头优先、
// 择优重派;尝试耗尽即整体失败,比无限等待诚实。
func (r *ReceiveSession) expire() error {
	now := time.Now()
	for idx, fl := range r.assigned {
		if fl.pending {
			continue
		}
		if now.Sub(fl.sentAt) < r.opts.RequestTimeout {
			continue
		}
		// 把超时通道的评分推到至少一个超时周期:半死通道(可写不回帧)若仍
		// 挂着乐观分,会持续赢得重派、把块预算拖到耗尽。
		fl.ce.score = max(fl.ce.score, float64(r.opts.RequestTimeout))
		r.releaseAssignment(idx, fl, true)
		if r.attempts[idx] >= r.opts.MaxBlockAttempts {
			return fmt.Errorf("%w: block %d failed after %d attempts (timeout)", ErrBlockExhausted, idx, r.attempts[idx])
		}
	}
	return nil
}

func (r *ReceiveSession) releaseAssignment(idx uint64, fl *flight, retry bool) {
	delete(r.assigned, idx)
	delete(fl.ce.inflight, idx)
	delete(fl.ce.partial, idx)
	if retry && !r.ordered() {
		r.addRetry(idx)
	}
}

func (r *ReceiveSession) refundAttempt(idx uint64) {
	attempts := r.attempts[idx]
	if attempts <= 1 {
		delete(r.attempts, idx)
		return
	}
	r.attempts[idx] = attempts - 1
}

// onFrame 处理一条入站帧。畸形帧/错位帧型是通道对端的实现缺陷:弃通道保
// 会话,其余通道不受牵连;分享级 ERROR 才终止整个会话。
func (r *ReceiveSession) onFrame(ce *chanEntry, f Frame) error {
	if !slices.Contains(r.channels, ce) {
		return nil // 已出池通道的迟到帧
	}
	msg, err := Decode(f)
	if err != nil {
		r.dropChannel(ce)
		return nil
	}
	switch m := msg.(type) {
	case *Block:
		return r.onBlock(ce, m)
	case *Error:
		if FatalCode(m.Code) {
			return m // 分享级:换通道也会命中同一份出错的源(§6.6)
		}
		r.dropChannel(ce)
		return nil
	default:
		r.dropChannel(ce) // 接收侧不该收到 REQUEST
		return nil
	}
}

func (r *ReceiveSession) onBlock(ce *chanEntry, b *Block) error {
	fl, ok := r.assigned[b.Index]
	if !ok || fl.ce != ce {
		// 未请求/改派后的迟到帧:丢弃。只认当前持有分配的来路,既免跨次
		// 发送(不同 nonce)混拼,也把重组内存钉在 窗口×MaxBlockBytes 内。
		return nil
	}
	if fl.pending {
		// A response can beat the asynchronous Send result back to the event loop.
		// Its arrival proves transport acceptance, so this is a real block attempt
		// and must consume the same bounded retry budget as the ordinary ordering.
		fl.pending = false
		r.attempts[b.Index]++
	}
	as, ok := ce.partial[b.Index]
	if !ok {
		as = newReassembly()
		ce.partial[b.Index] = as
	}
	blockCT, complete, err := as.add(b, r.opts.MaxBlockBytes)
	if err != nil {
		// 重组违规(重复 seq/双 last/超预算)按通道级处置:出池、在途重派。
		r.dropChannel(ce)
		return nil
	}
	if !complete {
		return nil
	}
	delete(ce.partial, b.Index)
	plaintext, err := r.opener.Open(b.Index, blockCT)
	if err != nil {
		// AEAD 验证失败:密文被改或对端错发。计一次失败、退回重派;不立即
		// 处死通道——中转路径上的偶发损坏与恶意难分,交给重试上限收敛。
		r.releaseAssignment(b.Index, fl, true)
		if r.attempts[b.Index] >= r.opts.MaxBlockAttempts {
			return fmt.Errorf("%w: block %d failed after %d attempts (authentication failed: %v)", ErrBlockExhausted, b.Index, r.attempts[b.Index], err)
		}
		return nil
	}
	r.releaseAssignment(b.Index, fl, false)
	delete(r.attempts, b.Index)
	// EWMA 偏向近期:热切换后旧通道的历史分数应尽快被新现实覆盖。
	dur := float64(time.Since(fl.sentAt))
	if ce.score == 0 {
		ce.score = dur
	} else {
		ce.score = 0.7*ce.score + 0.3*dur
	}
	if r.ordered() {
		if b.Index != r.orderedWindow[0] {
			r.buffered[b.Index] = plaintext
			r.maxBuffered = max(r.maxBuffered, len(r.buffered))
			return nil
		}
		if err := r.sink.WriteBlock(b.Index, plaintext); err != nil {
			return fmt.Errorf("session: sink write block %d: %w", b.Index, err)
		}
		r.orderedWindow = r.orderedWindow[1:]
		r.remaining--
		r.fillOrderedWindow()
		return nil
	}
	if err := r.sink.WriteBlock(b.Index, plaintext); err != nil {
		return fmt.Errorf("session: sink write block %d: %w", b.Index, err)
	}
	r.remaining--
	return nil
}
