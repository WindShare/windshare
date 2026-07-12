package relay

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/windshare/windshare/core/manifest"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/relay/protocol"
)

// A physical sender link retains terminal IDs so late traffic cannot recreate a
// completed session. The lifetime cap bounds that safety history under hostile
// or unusually long-lived peers; crossing it recycles the link and its namespace.
const defaultSenderSessionHistoryLimit = 1024

// SenderConfig 配置发送端连接;零值时长取协议默认(测试注入短时值,§7)。
type SenderConfig struct {
	RelayURL string
	ShareID  string

	// SealedManifest 随 register 上传;断线宽限重注册要求逐字节一致(§6.8),
	// 本包持引用不拷贝,连接存续期内调用方不得改写。
	SealedManifest []byte

	// ResumeToken 由调用方生成注入(每中转独立的本地秘密,§6.8):register
	// 只上送其 SHA-256,断线宽限期重注册出示原像。长度须为
	// protocol.ResumeTokenBytes。
	ResumeToken []byte

	// HTTPClient 供 WS 拨号(nil 取 http.DefaultClient);注入点为测试与
	// 自定义传输(代理)留位。
	HTTPClient *http.Client

	KeepaliveInterval time.Duration
	// ReconnectGrace 是断线重连的放弃期限;应不大于中转的
	// SenderReconnectGrace,否则末段重试注定 not_found 期后的抢注失败。
	ReconnectGrace time.Duration
	Backoff        Backoff
	Logf           func(format string, args ...any)

	// SessionHistoryLimit bounds active sessions plus terminal tombstones on one
	// physical link. A non-positive value selects the safe default; the bound
	// cannot be disabled.
	SessionHistoryLimit int
}

func (c SenderConfig) withDefaults() SenderConfig {
	if c.KeepaliveInterval <= 0 {
		c.KeepaliveInterval = protocol.KeepaliveInterval
	}
	if c.ReconnectGrace <= 0 {
		c.ReconnectGrace = protocol.SenderReconnectGrace
	}
	if c.SessionHistoryLimit <= 0 {
		c.SessionHistoryLimit = defaultSenderSessionHistoryLimit
	}
	c.Backoff = c.Backoff.withDefaults()
	if c.Logf == nil {
		c.Logf = func(string, ...any) {}
	}
	return c
}

// SenderConn 是发送端到中转的一条注册连接:维持 keepalive,把 join 带来的
// 接收会话物化为 FrameChannel 事件流,断线后在宽限期内凭 resumeToken 原像
// 重注册恢复(§6.8)。
//
// 中转不向发送端通告 join——新接收会话由首个携带未知 sessionId 的转发帧
// 或 signal 隐式物化(§6.11:offer 或 REQUEST 先到都算),经 Sessions()
// 交给调用方为其起一条发送会话。
type clientSessionState uint8

const (
	clientSessionActive clientSessionState = iota
	clientSessionTerminal
)

type clientSession struct {
	state clientSessionState
	ch    *Channel
}

type SenderConn struct {
	cfg SenderConfig

	registerWire []byte // 首次注册:只送 token 哈希
	resumeWire   []byte // 宽限重注册:出示原像,哈希与清单字节原样不变(§6.8)
	manifestWire []byte

	sessionsCh chan *Channel

	mu     sync.Mutex
	cur    *link // 重连间隙为 nil
	live   map[protocol.SessionID]*clientSession
	closed bool
	err    error

	closeReq  chan struct{}
	closeOnce sync.Once
	done      chan struct{}
}

// DialSender 建连、注册并等待 registered 确认后返回(§10:收到 ack 才算
// 注册成功,调用方此后才可打印链接——不许以"未见 error"猜测)。ctx 只约束
// 本次拨号与注册握手,连接生命周期由 Close 管理。
func DialSender(ctx context.Context, cfg SenderConfig) (*SenderConn, error) {
	cfg = cfg.withDefaults()
	if len(cfg.ResumeToken) != protocol.ResumeTokenBytes {
		return nil, fmt.Errorf("relay: resume token must be %d bytes, got %d", protocol.ResumeTokenBytes, len(cfg.ResumeToken))
	}
	if n := len(cfg.SealedManifest); n == 0 || n > manifest.MaxManifestSize {
		return nil, fmt.Errorf("relay: sealed manifest size %d out of range (1..%d)", n, manifest.MaxManifestSize)
	}
	hash := protocol.HashResumeToken(cfg.ResumeToken)
	token := protocol.EncodeResumeToken(cfg.ResumeToken)
	registerWire, err := protocol.Encode(protocol.NewRegister(cfg.ShareID, hash))
	if err != nil {
		return nil, err
	}
	resumeWire, err := protocol.Encode(protocol.NewResumeRegister(cfg.ShareID, hash, token))
	if err != nil {
		return nil, err
	}
	s := &SenderConn{
		cfg:          cfg,
		registerWire: registerWire,
		resumeWire:   resumeWire,
		manifestWire: protocol.EncodeManifestFrame(cfg.SealedManifest),
		sessionsCh:   make(chan *Channel, sessionEventBuffer),
		live:         make(map[protocol.SessionID]*clientSession),
		closeReq:     make(chan struct{}),
		done:         make(chan struct{}),
	}
	l, err := s.dialAndRegister(ctx, s.registerWire)
	if err != nil {
		return nil, err
	}
	s.cur = l
	go s.run(l)
	return s, nil
}

// Sessions returns newly materialized receiver sessions and closes when the
// connection terminates. If its bounded event buffer fills, only the unpublished
// session is terminated; the shared reader and published siblings keep running.
func (s *SenderConn) Sessions() <-chan *Channel { return s.sessionsCh }

// Done 在连接彻底终结(所有 goroutine 退净)后关闭。
func (s *SenderConn) Done() <-chan struct{} { return s.done }

// Err 返回连接终结原因;须在 Done 之后读取,nil 表示本地 Close。
func (s *SenderConn) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Close 幂等关闭:终止当前链路与重连循环,阻塞至全部 goroutine 退净。
// 协议没有"注销分享"消息——发送端消失本身就是信号,中转按宽限期回收
// (§6.9:分享生命周期 = 进程)。
func (s *SenderConn) Close() error {
	s.mu.Lock()
	s.closed = true
	cur := s.cur
	s.mu.Unlock()
	s.closeOnce.Do(func() { close(s.closeReq) })
	if cur != nil {
		cur.fail(ErrConnClosed)
	}
	<-s.done
	return nil
}

func (s *SenderConn) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// dialAndRegister 完成一次"建连 → register+清单帧 → 等 registered"握手
// (§6.7 清单帧紧随 register)。注册期读写全部同步进行,成功后才启动链路
// goroutine——WS 库要求单读者/单写者,握手期不与循环共存。
func (s *SenderConn) dialAndRegister(ctx context.Context, regWire []byte) (*link, error) {
	ws, err := dialWS(ctx, s.cfg.RelayURL, s.cfg.ShareID, s.cfg.HTTPClient)
	if err != nil {
		return nil, err
	}
	// 注册应答只有小 JSON;中转永不向发送端回清单帧。
	ws.SetReadLimit(protocol.MaxSignalingMessageBytes)
	if err := ws.Write(ctx, websocket.MessageText, regWire); err != nil {
		_ = ws.CloseNow()
		return nil, fmt.Errorf("relay: send register: %w", err)
	}
	if err := ws.Write(ctx, websocket.MessageBinary, s.manifestWire); err != nil {
		_ = ws.CloseNow()
		return nil, fmt.Errorf("relay: send manifest frame: %w", err)
	}
	for {
		typ, data, err := ws.Read(ctx)
		if err != nil {
			_ = ws.CloseNow()
			return nil, fmt.Errorf("relay: awaiting registered ack: %w", err)
		}
		if typ != websocket.MessageText {
			_ = ws.CloseNow()
			return nil, protocolViolation(ProtocolViolationUnexpectedBinary, "binary frame while awaiting registered ack", nil)
		}
		msg, err := protocol.Decode(data)
		if err != nil {
			_ = ws.CloseNow()
			return nil, protocolViolation(ProtocolViolationMalformedMessage, "message while awaiting registered ack", err)
		}
		switch m := msg.(type) {
		case *protocol.Registered:
			if m.ShareID != s.cfg.ShareID {
				_ = ws.CloseNow()
				return nil, protocolViolation(
					ProtocolViolationRegisteredShareMismatch,
					fmt.Sprintf("registered ack for share %q, want %q", m.ShareID, s.cfg.ShareID),
					nil,
				)
			}
			ws.SetReadLimit(clientDataReadLimit)
			return newLink(ws, s.cfg.KeepaliveInterval), nil
		case *protocol.Error:
			_ = ws.CloseNow()
			return nil, &ServerError{Code: m.Code, Message: m.Message}
		default:
			_ = ws.CloseNow()
			return nil, protocolViolation(
				ProtocolViolationUnexpectedMessage,
				fmt.Sprintf("%T while awaiting registered ack", msg),
				nil,
			)
		}
	}
}

// run 串联"服务当前链路 → 断线收尾 → 宽限重连"的生命周期;连接的一切
// 会话/事件流状态变更都收敛到本 goroutine(recvCh 的 close 与 send 才能
// 免锁串行)。
func (s *SenderConn) run(l *link) {
	for {
		err := s.serveLink(l)
		s.teardownLink(l)
		if s.isClosed() {
			s.finish(nil)
			return
		}
		if _, ok := errors.AsType[*ServerError](err); ok {
			// 中转的连接级否决(协议违规等)与传输故障不同:重连只会得到
			// 同一答案,直接终结。
			s.finish(err)
			return
		}
		nl, rerr := s.reconnect(err)
		if rerr != nil {
			if s.isClosed() {
				// Close 打断了重连:终局是本地关闭,不算故障。
				rerr = nil
			}
			s.finish(rerr)
			return
		}
		l = nl
	}
}

func (s *SenderConn) finish(err error) {
	s.mu.Lock()
	s.err = err
	s.mu.Unlock()
	close(s.sessionsCh)
	close(s.done)
}

// serveLink 是当前链路的读循环(单读者):解信令、按 sessionId 解包路由
// 转发帧、隐式物化新会话。返回值区分传输故障(可重连)与中转否决
// (ServerError,终局)。
func (s *SenderConn) serveLink(l *link) error {
	for {
		typ, data, err := l.ws.Read(l.ctx)
		if err != nil {
			l.fail(err)
			return l.cause()
		}
		if typ == websocket.MessageText {
			msg, err := protocol.Decode(data)
			if err != nil {
				err = protocolViolation(ProtocolViolationMalformedMessage, "signaling message", err)
				l.fail(err)
				return err
			}
			switch m := msg.(type) {
			case *protocol.Keepalive:
				// 自己 keepalive 的回显,吞掉。
			case *protocol.Signal:
				sid, err := protocol.ParseSessionID(m.SessionID)
				if err != nil {
					l.fail(err)
					return fmt.Errorf("relay: signal with bad sessionId: %w", err)
				}
				// §6.11:offer 可先于任何数据帧到达——signal 同样物化新会话。
				ch, err := s.ensureSession(l, sid)
				if err != nil {
					l.fail(err)
					return err
				}
				if ch == nil {
					continue // terminal tombstone: never revive a completed session
				}
				if result := ch.deliverSignal(Signal{Kind: m.Kind, Payload: m.Payload}); result == ingressOverflow {
					s.failSessionIngress(l, sid, IngressSignals)
				}
			case *protocol.Bye:
				if sid, err := protocol.ParseSessionID(m.SessionID); err == nil {
					if err := s.terminateSession(sid, nil, nil); err != nil {
						l.fail(err)
						return err
					}
				}
			case *protocol.Error:
				if m.SessionID != "" {
					if sid, err := protocol.ParseSessionID(m.SessionID); err == nil {
						if err := s.terminateSession(sid, &ServerError{Code: m.Code, Message: m.Message}, nil); err != nil {
							l.fail(err)
							return err
						}
					}
					s.cfg.Logf("relay: session error from relay: %s (%s)", m.Code, m.Message)
					continue
				}
				serr := &ServerError{Code: m.Code, Message: m.Message}
				l.fail(serr)
				return serr
			default:
				err := protocolViolation(
					ProtocolViolationUnexpectedMessage,
					fmt.Sprintf("signaling message %T", msg),
					nil,
				)
				l.fail(err)
				return err
			}
			continue
		}
		switch protocol.BinType(data) {
		case protocol.BinTypeForward:
			sid, inner, err := protocol.DecodeForwardFrame(data)
			if err != nil {
				err = protocolViolation(ProtocolViolationMalformedFrame, "forward frame", err)
				l.fail(err)
				return err
			}
			ch, err := s.ensureSession(l, sid)
			if err != nil {
				l.fail(err)
				return err
			}
			if ch != nil {
				if result := ch.deliver(session.Frame(inner)); result == ingressOverflow {
					s.failSessionIngress(l, sid, IngressFrames)
				}
			}
		case protocol.BinTypeTerminalForward:
			sid, inner, err := protocol.DecodeTerminalForwardFrame(data)
			if err != nil {
				err = protocolViolation(ProtocolViolationMalformedFrame, "terminal forward frame", err)
				l.fail(err)
				return err
			}
			ch, err := s.ensureSession(l, sid)
			if err != nil {
				l.fail(err)
				return err
			}
			if ch != nil {
				if err := s.terminateSession(sid, nil, session.Frame(inner)); err != nil {
					l.fail(err)
					return err
				}
			}
		default:
			err := protocolViolation(
				ProtocolViolationMalformedFrame,
				fmt.Sprintf("unexpected binary frame type 0x%02x", protocol.BinType(data)),
				nil,
			)
			l.fail(err)
			return err
		}
	}
}

// ensureSession is the only active-state constructor. Terminal entries remain
// tombstones until link teardown, so a late frame cannot recreate the session.
func (s *SenderConn) ensureSession(l *link, sid protocol.SessionID) (*Channel, error) {
	s.mu.Lock()
	entry, ok := s.live[sid]
	if ok {
		if entry.state == clientSessionTerminal {
			s.mu.Unlock()
			return nil, nil
		}
		if entry.ch.State() == session.Closed {
			entry.state = clientSessionTerminal
			s.mu.Unlock()
			return nil, nil
		}
	}
	if !ok {
		if err := s.checkSessionCapacityLocked(); err != nil {
			s.mu.Unlock()
			return nil, err
		}
		entry = &clientSession{state: clientSessionActive, ch: newChannel(sid, l)}
		s.live[sid] = entry
	}
	ch := entry.ch
	s.mu.Unlock()
	if !ok {
		select {
		case <-l.ctx.Done():
			return nil, nil
		default:
		}
		select {
		case s.sessionsCh <- ch:
		default:
			s.failSessionIngress(l, sid, IngressSessionEvents)
			return nil, nil
		}
	}
	return ch, nil
}

func (s *SenderConn) checkSessionCapacityLocked() error {
	if len(s.live) >= s.cfg.SessionHistoryLimit {
		return &SenderSessionHistoryOverflow{Limit: s.cfg.SessionHistoryLimit}
	}
	return nil
}

func (s *SenderConn) terminateSession(sid protocol.SessionID, reason error, terminal session.Frame) error {
	s.mu.Lock()
	entry, ok := s.live[sid]
	if !ok {
		// Terminal control is authoritative even when it races ahead of the first
		// data/signal. Retaining a channel-free tombstone prevents a late frame
		// with the same SID from materializing a session that already ended.
		if err := s.checkSessionCapacityLocked(); err != nil {
			s.mu.Unlock()
			return err
		}
		s.live[sid] = &clientSession{state: clientSessionTerminal}
		s.mu.Unlock()
		return nil
	}
	if entry.state == clientSessionTerminal {
		s.mu.Unlock()
		return nil
	}
	entry.state = clientSessionTerminal
	ch := entry.ch
	s.mu.Unlock()

	if terminal != nil {
		if result := ch.deliverTerminal(terminal); result == ingressOverflow {
			ch.shut(&SessionIngressOverflow{Kind: IngressFrames})
		}
		return nil
	}
	ch.shut(reason)
	return nil
}

// failSessionIngress converts local consumer backpressure into a session-local
// terminal. Terminal ownership is handed to the pump before the Channel closes,
// so CloseSession cannot revoke the best-effort bye and siblings keep flowing.
func (s *SenderConn) failSessionIngress(l *link, sid protocol.SessionID, kind IngressKind) {
	reason := &SessionIngressOverflow{Kind: kind}
	s.mu.Lock()
	entry, ok := s.live[sid]
	if !ok {
		s.mu.Unlock()
		return
	}
	if entry.state == clientSessionTerminal {
		s.mu.Unlock()
		return
	}
	entry.state = clientSessionTerminal
	ch := entry.ch
	s.mu.Unlock()

	wire := mustEncode(protocol.NewBye(sid.String()))
	if err := l.enqueueTerminalNoWait(sid, outItem{data: wire}); err != nil &&
		!errors.Is(err, ErrSessionTerminal) && !errors.Is(err, ErrConnClosed) {
		s.cfg.Logf("relay: failed to notify peer after session ingress overflow %s: %v", sid, err)
	}
	ch.shut(reason)
	s.cfg.Logf("relay: terminated session %s after local %s ingress overflow", sid, kind)
}

// teardownLink 是链路死亡的唯一收尾点:等写者/保活退净后闭合全部会话。
// 旧接收会话不跨链路存活——中转侧已随 senderGone 终结它们(§6.8),
// 重连后由接收端 rejoin 换取新 sessionId 重建。
func (s *SenderConn) teardownLink(l *link) {
	l.fail(ErrConnClosed) // 读循环异常退出时已 fail,此处为幂等兜底
	l.wait()
	cause := l.cause()
	s.mu.Lock()
	victims := make([]*Channel, 0, len(s.live))
	for _, entry := range s.live {
		if entry.ch != nil {
			victims = append(victims, entry.ch)
		}
	}
	s.live = make(map[protocol.SessionID]*clientSession)
	s.cur = nil
	s.mu.Unlock()
	for _, ch := range victims {
		ch.shut(cause)
	}
}

// reconnect 在宽限窗内指数退避重拨,凭 resumeToken 原像 + 同字节清单重注册
// (§6.8)。cause 保留触发重连的首因,窗口耗尽时随错误上抛。
func (s *SenderConn) reconnect(cause error) (*link, error) {
	deadline := time.Now().Add(s.cfg.ReconnectGrace)
	delay := s.cfg.Backoff.Initial
	for attempt := 1; ; attempt++ {
		remain := time.Until(deadline)
		if remain <= 0 {
			return nil, fmt.Errorf("%w: %w", ErrReconnectGraceExpired, cause)
		}
		actx, cancel := s.closeAwareContext(remain)
		l, err := s.dialAndRegister(actx, s.resumeWire)
		cancel()
		if err == nil {
			s.mu.Lock()
			if s.closed {
				s.mu.Unlock()
				l.fail(ErrConnClosed)
				l.wait()
				return nil, ErrConnClosed
			}
			s.cur = l
			s.mu.Unlock()
			s.cfg.Logf("relay: sender reconnected to share %s after %d attempt(s)", s.cfg.ShareID, attempt)
			return l, nil
		}
		if s.isClosed() {
			return nil, ErrConnClosed
		}
		var srvErr *ServerError
		if errors.As(err, &srvErr) &&
			srvErr.Code != protocol.ErrCodeShareIDCollision &&
			srvErr.Code != protocol.ErrCodeRateLimited {
			// 身份/内容被否决(resume_rejected 等):重试只会重复同一答案。
			// collision 可能是旧连接尚未落地的半开竞态,rate_limited 明确
			// 要求退避;二者都应在既有重连宽限内重试。
			return nil, err
		}
		s.cfg.Logf("relay: reconnect attempt %d failed: %v", attempt, err)
		if !waitBackoff(context.Background(), s.closeReq, deadline, &delay, s.cfg.Backoff.Max) {
			if s.isClosed() {
				return nil, ErrConnClosed
			}
			return nil, fmt.Errorf("%w: %w", ErrReconnectGraceExpired, cause)
		}
	}
}

// closeAwareContext 派生同时受剩余宽限与本地 Close 约束的拨号 ctx。
func (s *SenderConn) closeAwareContext(d time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	go func() {
		select {
		case <-s.closeReq:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}
