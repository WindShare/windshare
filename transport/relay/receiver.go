package relay

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/relay/protocol"
)

// closeHandshakeTimeout 是礼貌关闭时等待中转收线的上限:bye 经高优先通道
// 出站,中转收到后终结会话并主动关连接;等它既保证 bye 送达,又免去本地
// 与写者的二次同步。超时兜底硬拆。
const closeHandshakeTimeout = 2 * time.Second

var errTerminalDelivered = errors.New("relay: terminal frame delivered")

// ReceiverConfig 配置接收端连接;零值时长取协议默认。
type ReceiverConfig struct {
	RelayURL string
	ShareID  string

	// HTTPClient 供 WS 拨号(nil 取 http.DefaultClient)。
	HTTPClient *http.Client

	KeepaliveInterval time.Duration

	// JoinRetryWindow 约束 join 的整个退避重试期(not_found 与拨号失败共用,
	// §6.7 短窗,覆盖 join 先于 register 的竞态)。断线 rejoin 需要熬过发送
	// 端的重连宽限时,调用方应放大到 SenderReconnectGrace 之上。
	JoinRetryWindow time.Duration
	Backoff         Backoff
	Logf            func(format string, args ...any)
}

func (c ReceiverConfig) withDefaults() ReceiverConfig {
	if c.KeepaliveInterval <= 0 {
		c.KeepaliveInterval = protocol.KeepaliveInterval
	}
	if c.JoinRetryWindow <= 0 {
		c.JoinRetryWindow = DefaultJoinRetryWindow
	}
	c.Backoff = c.Backoff.withDefaults()
	if c.Logf == nil {
		c.Logf = func(string, ...any) {}
	}
	return c
}

// ReceiverConn 是接收端的一条会话连接:join 换取 sessionId 与清单字节,
// 物化单条 FrameChannel。连接生命周期 = 该会话;断线不原地复活——重新
// DialReceiver 即 rejoin(新 sessionId、新通道),清单指纹不变,上层凭
// Sink 的 bitfield 只请求缺失块续传(§6.12)。
type ReceiverConn struct {
	cfg    ReceiverConfig
	sid    protocol.SessionID
	sealed []byte
	l      *link
	ch     *Channel

	mu     sync.Mutex
	closed bool
	err    error
	done   chan struct{}
}

// DialReceiver 建连并 join,拿到 sessionId 与清单字节后返回。not_found 与
// 拨号失败在 JoinRetryWindow 内指数退避重试(§6.7);中转的其他否决
// (ServerError)立即返回。ctx 只约束 join 握手,连接生命周期由 Close 管理。
func DialReceiver(ctx context.Context, cfg ReceiverConfig) (*ReceiverConn, error) {
	cfg = cfg.withDefaults()
	joinWire, err := protocol.Encode(protocol.NewJoin(cfg.ShareID))
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(cfg.JoinRetryWindow)
	joinCtx, cancelJoin := context.WithDeadline(ctx, deadline)
	defer cancelJoin()
	d := &joinDialer{
		cfg:      cfg,
		ctx:      ctx,
		joinCtx:  joinCtx,
		deadline: deadline,
		delay:    cfg.Backoff.Initial,
	}
	for {
		if err := d.interrupted(); err != nil {
			return nil, err
		}
		conn, err := d.attempt(joinWire)
		if conn != nil || err != nil {
			return conn, err
		}
		if !waitBackoff(joinCtx, nil, deadline, &d.delay, cfg.Backoff.Max) {
			d.closeWS()
			return nil, d.windowExpired(joinCtx.Err())
		}
	}
}

// joinDialer 承载 DialReceiver 的重试状态:窗口、退避进度、可复用的连接与
// 供窗口收束时解释终局的最近错误。
type joinDialer struct {
	cfg      ReceiverConfig
	ctx      context.Context // 调用方生命周期,取消优先于一切重试语义
	joinCtx  context.Context // 上叠 JoinRetryWindow 的握手窗口
	deadline time.Time
	delay    time.Duration
	ws       *websocket.Conn

	lastErr         error
	lastSemanticErr error
}

// interrupted 报告调用方取消或 join 窗口收束,两者都终止重试并收回连接。
func (d *joinDialer) interrupted() error {
	if err := d.ctx.Err(); err != nil {
		d.closeWS()
		return err
	}
	if err := d.joinCtx.Err(); err != nil {
		d.closeWS()
		return d.windowExpired(err)
	}
	return nil
}

// attempt 执行一次完整 join:按需拨号、发 join、读应答。返回的连接非 nil
// 即成功;err 非 nil 即终局否决;二者皆零值表示已记下失败,应退避重试。
func (d *joinDialer) attempt(joinWire []byte) (*ReceiverConn, error) {
	if d.ws == nil {
		w, err := dialWS(d.joinCtx, d.cfg.RelayURL, d.cfg.ShareID, d.cfg.HTTPClient)
		if err != nil {
			// 中转暂不可达与 not_found 同窗退避:rejoin 场景里两者都
			// 意味着"再等等",区分只会把重试逻辑推给每个调用方。
			d.lastErr = err
			return nil, nil
		}
		d.ws = w
		// join 应答期唯一的大消息是清单帧。
		d.ws.SetReadLimit(manifestReadLimit)
	}
	if err := d.ws.Write(d.joinCtx, websocket.MessageText, joinWire); err != nil {
		d.closeWS()
		d.lastErr = err
		return nil, nil
	}
	r, retry, err := awaitJoinReply(d.joinCtx, d.ws, d.cfg)
	if err == nil {
		return r, nil
	}
	if !retry {
		d.closeWS()
		return nil, err
	}
	d.noteRetryable(err)
	// not_found 后中转仍在同连接上等待下一次 join(服务端 join 循环
	// 语义);其余可重试错误(读失败/限速断连)须重新拨号。
	if !errors.Is(err, ErrShareNotFound) {
		d.closeWS()
	}
	return nil, nil
}

// noteRetryable 记录一次可重试失败;not_found 与 rate_limited 是"分享状态"
// 级别的答复,窗口收束时优先于传输错误作为解释保留。
func (d *joinDialer) noteRetryable(err error) {
	d.lastErr = err
	if errors.Is(err, ErrShareNotFound) {
		d.lastSemanticErr = err
		return
	}
	if serverErr, ok := errors.AsType[*ServerError](err); ok && serverErr.Code == protocol.ErrCodeRateLimited {
		d.lastSemanticErr = err
	}
}

func (d *joinDialer) closeWS() {
	if d.ws != nil {
		_ = d.ws.CloseNow()
		d.ws = nil
	}
}

func (d *joinDialer) windowExpired(fallback error) error {
	return joinWindowExpired(d.ctx, d.lastSemanticErr, d.lastErr, fallback)
}

func joinWindowExpired(ctx context.Context, semanticErr, lastErr, fallback error) error {
	// The caller owns the outer lifecycle. Its cancellation must not be hidden by
	// a retryable relay response observed earlier in the join window.
	if err := ctx.Err(); err != nil {
		return err
	}
	// A relay response explains the product state better than the canceled I/O
	// used to enforce the window at its boundary. Keep the newest retryable
	// not_found/rate_limited result while still exposing transport-only failures.
	cause := semanticErr
	if cause == nil {
		cause = lastErr
	}
	if cause == nil {
		cause = fallback
	}
	return fmt.Errorf("relay: join window expired: %w", cause)
}

// awaitJoinReply 读取一次 join 的应答:manifest{sessionId}+清单帧即成功;
// not_found/rate_limited/读失败标记为可重试,其余否决终局。
func awaitJoinReply(ctx context.Context, ws *websocket.Conn, cfg ReceiverConfig) (*ReceiverConn, bool, error) {
	for {
		typ, data, err := ws.Read(ctx)
		if err != nil {
			return nil, true, fmt.Errorf("relay: awaiting join reply: %w", err)
		}
		if typ != websocket.MessageText {
			return nil, false, protocolViolation(
				ProtocolViolationUnexpectedBinary,
				"binary frame before manifest message",
				nil,
			)
		}
		msg, err := protocol.Decode(data)
		if err != nil {
			return nil, false, protocolViolation(ProtocolViolationMalformedMessage, "message while joining", err)
		}
		switch m := msg.(type) {
		case *protocol.Keepalive:
			continue
		case *protocol.NotFound:
			return nil, true, fmt.Errorf("%w: share %s", ErrShareNotFound, cfg.ShareID)
		case *protocol.Manifest:
			sid, err := protocol.ParseSessionID(m.SessionID)
			if err != nil {
				return nil, false, protocolViolation(ProtocolViolationMalformedManifest, "manifest sessionId", err)
			}
			typ, blob, err := ws.Read(ctx)
			if err != nil {
				return nil, true, fmt.Errorf("relay: awaiting manifest frame: %w", err)
			}
			if typ != websocket.MessageBinary {
				return nil, false, protocolViolation(
					ProtocolViolationManifestSequence,
					"manifest message not followed by manifest frame",
					nil,
				)
			}
			sealed, err := protocol.DecodeManifestFrame(blob)
			if err != nil {
				return nil, false, protocolViolation(ProtocolViolationMalformedManifest, "manifest frame", err)
			}
			ws.SetReadLimit(clientDataReadLimit)
			l := newLink(ws, cfg.KeepaliveInterval)
			r := &ReceiverConn{
				cfg:    cfg,
				sid:    sid,
				sealed: sealed,
				l:      l,
				ch:     newChannel(sid, l),
				done:   make(chan struct{}),
			}
			go r.run()
			return r, false, nil
		case *protocol.Error:
			serr := &ServerError{Code: m.Code, Message: m.Message}
			if m.Code == protocol.ErrCodeRateLimited {
				// 限速是"叫我们退避"而非终局(§6.8 中转直接关连接,
				// 让退避发生在客户端)——换连接重试。
				return nil, true, serr
			}
			return nil, false, serr
		default:
			return nil, false, protocolViolation(
				ProtocolViolationUnexpectedMessage,
				fmt.Sprintf("%T while joining", msg),
				nil,
			)
		}
	}
}

// SealedManifest 返回中转回放的加密清单字节(原样,不解密——密钥在链接
// fragment,本包不经手)。rejoin 后上层应比对清单指纹一致再续传(§6.12)。
func (r *ReceiverConn) SealedManifest() []byte { return r.sealed }

// SessionID 返回中转分配的会话标识。
func (r *ReceiverConn) SessionID() protocol.SessionID { return r.sid }

// Channel 返回本会话的 FrameChannel(生命周期与连接一体)。
func (r *ReceiverConn) Channel() *Channel { return r.ch }

// Done 在连接彻底终结(所有 goroutine 退净)后关闭。
func (r *ReceiverConn) Done() <-chan struct{} { return r.done }

// Err 返回连接终结原因;须在 Done 之后读取。nil 表示本地 Close 或发送端
// 正常 bye;ServerError(如 sender_gone)提示上层退避 rejoin。
func (r *ReceiverConn) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

// Close 幂等关闭:经 bye 礼貌告别,等中转按其关闭序列收线,超时硬拆。
func (r *ReceiverConn) Close() error {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	if err := r.ch.Close(); err != nil {
		r.l.fail(err)
	}
	select {
	case <-r.done:
	case <-time.After(closeHandshakeTimeout):
		r.l.fail(ErrConnClosed)
		<-r.done
	}
	return nil
}

// run 驱动读循环并做终局收尾;通道 recvCh 的投递与关闭都收敛在本 goroutine。
func (r *ReceiverConn) run() {
	err := r.serveLink()
	if errors.Is(err, errTerminalDelivered) {
		err = nil
	}
	if websocket.CloseStatus(err) == websocket.StatusNormalClosure {
		// 本端 bye 后中转按正常关闭序收线(§6.7)——预期终局,不是故障。
		err = nil
	}
	r.l.fail(ErrConnClosed) // 正常 bye 退出时链路尚活,此处统一拆
	r.l.wait()
	r.ch.shut(err)
	r.mu.Lock()
	if r.closed {
		err = nil // 本地主动关闭:终局不是故障
	}
	r.err = err
	r.mu.Unlock()
	close(r.done)
}

// serveLink 是会话期读循环:吞 keepalive 回显、透传 signal、解包转发帧。
// 返回 nil 表示发送端正常 bye;ServerError 表示中转/发送端侧终结
// (sender_gone 等,上层据此退避 rejoin,§6.8)。
func (r *ReceiverConn) serveLink() error {
	l := r.l
	for {
		typ, data, err := l.ws.Read(l.ctx)
		if err != nil {
			l.fail(err)
			return l.cause()
		}
		if typ == websocket.MessageText {
			if done, err := r.handleSignaling(data); done {
				return err
			}
			continue
		}
		if done, err := r.handleForwardFrame(data); done {
			return err
		}
	}
}

// handleSignaling 处理会话期一条文本信令。done 为真表示读循环应以 err 终局
// (发送端 bye 时 err 为 nil);为假则继续读取。
func (r *ReceiverConn) handleSignaling(data []byte) (done bool, _ error) {
	msg, err := protocol.Decode(data)
	if err != nil {
		err = protocolViolation(ProtocolViolationMalformedMessage, "signaling message", err)
		r.l.fail(err)
		return true, err
	}
	switch m := msg.(type) {
	case *protocol.Keepalive:
		return false, nil
	case *protocol.Signal:
		if err := r.requireOwnSession(m.SessionID, "signal"); err != nil {
			return true, err
		}
		if result := r.ch.deliverSignal(Signal{Kind: m.Kind, Payload: m.Payload}); result == ingressOverflow {
			return true, r.failSessionIngress(IngressSignals)
		}
		return false, nil
	case *protocol.Bye:
		if err := r.requireOwnSession(m.SessionID, "bye"); err != nil {
			return true, err
		}
		// 发送端正常结束会话;中转随后关连接,这里直接收尾。
		return true, nil
	case *protocol.Error:
		if m.SessionID != "" {
			if err := r.requireOwnSession(m.SessionID, "error"); err != nil {
				return true, err
			}
		}
		// 连接生命周期 = 会话,会话级与连接级错误对接收端同义。
		return true, &ServerError{Code: m.Code, Message: m.Message}
	default:
		err := protocolViolation(
			ProtocolViolationUnexpectedMessage,
			fmt.Sprintf("signaling message %T", msg),
			nil,
		)
		r.l.fail(err)
		return true, err
	}
}

// requireOwnSession 校验信令归属本会话;异会话即协议违规并终结链路。
func (r *ReceiverConn) requireOwnSession(sid, kind string) error {
	if sid == r.sid.String() {
		return nil
	}
	err := protocolViolation(
		ProtocolViolationForeignSession,
		kind+" for session "+sid,
		nil,
	)
	r.l.fail(err)
	return err
}

// handleForwardFrame 解包一个二进制转发帧并投递给会话通道。done 为真表示
// 读循环应以 err 终局(终帧投递成功即 errTerminalDelivered);为假则继续。
func (r *ReceiverConn) handleForwardFrame(data []byte) (done bool, _ error) {
	var sid protocol.SessionID
	var inner []byte
	var err error
	switch protocol.BinType(data) {
	case protocol.BinTypeForward:
		sid, inner, err = protocol.DecodeForwardFrame(data)
	case protocol.BinTypeTerminalForward:
		sid, inner, err = protocol.DecodeTerminalForwardFrame(data)
	default:
		err = protocolViolation(
			ProtocolViolationMalformedFrame,
			fmt.Sprintf("unexpected binary frame type 0x%02x", protocol.BinType(data)),
			nil,
		)
	}
	if err != nil {
		if _, ok := errors.AsType[*ProtocolViolation](err); !ok {
			err = protocolViolation(ProtocolViolationMalformedFrame, "forward frame", err)
		}
		r.l.fail(err)
		return true, err
	}
	if sid != r.sid {
		err := protocolViolation(
			ProtocolViolationForeignSession,
			"forward frame for session "+sid.String(),
			nil,
		)
		r.l.fail(err)
		return true, err
	}
	if protocol.BinType(data) == protocol.BinTypeTerminalForward {
		if result := r.ch.deliverTerminal(session.Frame(inner)); result == ingressOverflow {
			return true, r.failSessionIngress(IngressFrames)
		}
		return true, errTerminalDelivered
	}
	if result := r.ch.deliver(session.Frame(inner)); result == ingressOverflow {
		return true, r.failSessionIngress(IngressFrames)
	}
	return false, nil
}

func (r *ReceiverConn) failSessionIngress(kind IngressKind) error {
	reason := &SessionIngressOverflow{Kind: kind}
	wire := mustEncode(protocol.NewBye(r.sid.String()))
	if err := r.l.enqueueTerminalNoWait(r.sid, outItem{data: wire}); err != nil &&
		!errors.Is(err, ErrSessionTerminal) && !errors.Is(err, ErrConnClosed) {
		r.cfg.Logf("relay: failed to notify peer after session ingress overflow %s: %v", r.sid, err)
	}
	r.ch.shut(reason)
	return reason
}
