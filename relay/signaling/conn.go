package signaling

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/windshare/windshare/relay/admission"
	"github.com/windshare/windshare/relay/forward"
	"github.com/windshare/windshare/relay/protocol"
)

const (
	// writeTimeout 是单次 WS 写的上限。泵已把慢接收端隔离在有界队列后,
	// 一次写还能拖过它的连接已不可用,及时失败让泵停摆、触发清理。
	writeTimeout = 30 * time.Second
	// finalFlushTimeout 是关连接前等待终局消息(error/bye)落地的上限。
	finalFlushTimeout = 2 * time.Second
	// Detached errors are useful diagnostics for a few never-issued session IDs,
	// but each distinct ID creates a short-lived pump queue. A per-connection
	// lifetime cap prevents unknown-ID churn from growing state without bound.
	maxDetachedSessionErrors = 16
	// Client error reports are advisory. Bounding their lifetime count prevents
	// an authenticated connection from turning diagnostics into a log flood.
	maxClientDiagnosticReports = 8
)

// connectionPump is defined at the consumer boundary so lifecycle policy can
// be verified without coupling tests to the pump's internal queue layout.
type connectionPump interface {
	EnqueueConnection(binary bool, data []byte) forward.EnqueueResult
	EnqueueConnectionBorrowed(binary bool, data []byte) (forward.EnqueueResult, <-chan error)
	OpenSession(id protocol.SessionID) forward.EnqueueResult
	CloseSession(id protocol.SessionID) forward.EnqueueResult
	EnqueueSessionControl(id protocol.SessionID, binary bool, data []byte) forward.EnqueueResult
	EnqueueSessionTerminal(id protocol.SessionID, binary bool, data []byte) (forward.EnqueueResult, <-chan error)
	EnqueueForward(id protocol.SessionID, frame []byte) forward.EnqueueResult
	WaitIdle(ctx context.Context) bool
	Close()
}

// conn 包装一条 WS 连接:出站一律经泵(信令高优先 + 每会话有界转发,
// §6.8),入站由所属 serve 循环独占读取。
type conn struct {
	ws     *websocket.Conn
	pump   connectionPump
	ctx    context.Context
	cancel context.CancelFunc
	ip     string
	// inactivityTimeout is enforced after role establishment independently of
	// HTTP timeouts, which no longer apply once the connection is upgraded.
	inactivityTimeout time.Duration
	// sessionIDs is guarded by the owning Hub.mu. Its constant-size
	// connection-lifetime recognizer outlives every pump terminal receipt because
	// the opposite TCP direction can still contain earlier frames.
	sessionIDs        senderSessionIDs
	unknownSessions   unknownSessionTracker
	diagnosticReports atomic.Uint32

	closeOnce sync.Once
	// closedCh 在关闭序列(flush → 泵停 → WS close)完成后关闭。ServeConn
	// 须等它再返回:handler 一返回,请求 context 即被取消,在途的终局
	// error/bye 会死在半路。
	closedCh chan struct{}
}

// wsWriter 把泵的出口适配到 WS 连接(forward.Writer 的实现)。
type wsWriter struct{ c *conn }

func (w wsWriter) WriteText(data []byte) error   { return w.c.write(websocket.MessageText, data) }
func (w wsWriter) WriteBinary(data []byte) error { return w.c.write(websocket.MessageBinary, data) }

func (c *conn) write(typ websocket.MessageType, data []byte) error {
	ctx, cancel := context.WithTimeout(c.ctx, writeTimeout)
	defer cancel()
	return c.ws.Write(ctx, typ, data)
}

func (c *conn) readActive() (websocket.MessageType, []byte, error) {
	ctx, cancel := context.WithTimeout(c.ctx, c.inactivityTimeout)
	defer cancel()
	return c.ws.Read(ctx)
}

// sendConnection is reserved for connection lifecycle and handshake traffic.
// Session control must name a queue so one flooded session cannot starve peers.
func (c *conn) sendConnection(m protocol.Message) bool {
	switch typed := m.(type) {
	case *protocol.Signal, *protocol.Bye:
		panic("signaling: session message sent on connection lane")
	case *protocol.Error:
		if typed.SessionID != "" {
			panic("signaling: session error sent on connection lane")
		}
	}
	data := encodeMessage(m)
	if c.pump.EnqueueConnection(false, data) != forward.Enqueued {
		c.closeNow()
		return false
	}
	return true
}

func (c *conn) sendSessionControl(id protocol.SessionID, m protocol.Message) forward.EnqueueResult {
	return c.pump.EnqueueSessionControl(id, false, encodeMessage(m))
}

func (c *conn) sendSessionTerminal(id protocol.SessionID, m protocol.Message) (forward.EnqueueResult, <-chan error) {
	return c.pump.EnqueueSessionTerminal(id, false, encodeMessage(m))
}

func (c *conn) sendTerminalForward(id protocol.SessionID, frame []byte) (forward.EnqueueResult, <-chan error) {
	return c.pump.EnqueueSessionTerminal(id, true, frame)
}

// rollbackUnknownSession shares Hub.mu with candidate retirement. Cleanup is
// complete before this call, so the allocator sees either the reservation or a
// reusable wire ID whose diagnostic pump state is already gone.
func (h *Hub) rollbackUnknownSession(c *conn, id protocol.SessionID) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c.unknownSessions.rollback(id)
}

// sendDetachedSessionError consumes an observation already reserved under
// Hub.mu. Repeats and a limit-exceeded ID therefore never touch the pump, while
// failed first diagnostics release their namespace reservation only after pump
// cleanup has completed.
func (h *Hub) sendDetachedSessionError(c *conn, id protocol.SessionID, code, reason string, observation unknownSessionObservation) bool {
	switch observation {
	case unknownSessionFirst:
	case unknownSessionRepeated:
		return true
	case unknownSessionLimitExceeded:
		c.fatal(protocol.ErrCodeProtocol, "too many unknown-session frames")
		return false
	}

	switch result := c.pump.OpenSession(id); result {
	case forward.Enqueued:
	case forward.SessionTerminated:
		// A terminal already owns this queue. Retaining the observation prevents
		// duplicates from reopening it after that terminal receipt drains.
		return true
	case forward.PumpClosed:
		h.rollbackUnknownSession(c, id)
		return false
	default:
		h.rollbackUnknownSession(c, id)
		return true
	}

	result, _ := c.sendSessionTerminal(id, protocol.NewSessionError(id.String(), code, reason))
	switch result {
	case forward.Enqueued, forward.SessionTerminated:
		return true
	case forward.PumpClosed:
		c.pump.CloseSession(id)
		h.rollbackUnknownSession(c, id)
		return false
	default:
		c.pump.CloseSession(id)
		h.rollbackUnknownSession(c, id)
		return true
	}
}

func encodeMessage(m protocol.Message) []byte {
	data, err := protocol.Encode(m)
	if err != nil {
		panic("signaling: constructed invalid protocol message: " + err.Error())
	}
	return data
}

// sendBorrowedBinaryHigh transfers an immutable manifest frame without another
// full-size copy. Its receipt is also the borrowed lease's lifetime boundary.
func (c *conn) sendBorrowedBinaryHigh(frame []byte) (<-chan error, bool) {
	result, receipt := c.pump.EnqueueConnectionBorrowed(true, frame)
	if result != forward.Enqueued {
		c.closeNow()
		return nil, false
	}
	return receipt, true
}

func (h *Hub) serveSender(c *conn, shareID string, reg *protocol.Register) {
	if reg.ShareID != shareID {
		c.fatal(protocol.ErrCodeShareIDMismatch, "register shareId does not match request path")
		return
	}

	attempt, outcome := h.beginRegister(c, reg)
	if attempt != nil {
		defer attempt.release()
	}
	switch outcome {
	case registerOK:
	case registerCollision:
		c.fatal(protocol.ErrCodeShareIDCollision, "shareId is already active; generate a new one")
		return
	case registerResumeRejected:
		c.fatal(protocol.ErrCodeResumeRejected, "re-registration validation failed during grace period")
		return
	case registerBudgetExceeded:
		c.fatal(protocol.ErrCodeManifestBudget, "relay registration capacity exhausted; retry later or use another relay")
		return
	case registerRateLimited:
		c.fatal(protocol.ErrCodeRateLimited, "register rate limit exceeded; retry later")
		return
	}

	// The manifest follows register on the wire. Streaming keeps both pending
	// and retained memory within the admission transaction.
	sealed, ok := h.readRegisterManifest(c, attempt)
	if !ok {
		return
	}
	sh, outcome := h.finishRegister(c, reg, sealed, attempt)
	switch outcome {
	case registerOK:
	case registerCollision:
		c.fatal(protocol.ErrCodeShareIDCollision, "shareId is already active; generate a new one")
		return
	case registerResumeRejected:
		c.fatal(protocol.ErrCodeResumeRejected, "re-registration validation failed during grace period")
		return
	case registerBudgetExceeded:
		c.fatal(protocol.ErrCodeManifestBudget, "relay registration capacity exhausted; retry later or use another relay")
		return
	case registerRateLimited:
		c.fatal(protocol.ErrCodeRateLimited, "register rate limit exceeded; retry later")
		return
	}
	defer h.senderGone(sh, c)

	if !c.sendConnection(protocol.NewRegistered(shareID)) {
		return
	}
	c.ws.SetReadLimit(h.dataReadLimit())

	for {
		typ, data, err := c.readActive()
		if err != nil {
			return
		}
		if typ == websocket.MessageText {
			msg, err := protocol.Decode(data)
			if err != nil {
				c.fatal(protocol.ErrCodeProtocol, err.Error())
				return
			}
			switch m := msg.(type) {
			case *protocol.Keepalive:
				// 原样回显兼作应用层 pong(§6.7)。
				c.sendConnection(protocol.NewKeepalive())
			case *protocol.Signal:
				if !h.relayControlToReceiver(c, sh, m.SessionID, m) {
					return
				}
			case *protocol.Bye:
				h.byeFromSender(sh, m)
			case *protocol.Error:
				if c.diagnosticReports.Add(1) > maxClientDiagnosticReports {
					c.fatal(protocol.ErrCodeProtocol, "too many client diagnostic reports")
					return
				}
				h.cfg.Logf("signaling: sender reported error share=%s code=%s", shareID, m.Code)
			default:
				c.fatal(protocol.ErrCodeProtocol, "sender must not send "+data2type(data))
				return
			}
			continue
		}
		kind := protocol.BinType(data)
		if kind != protocol.BinTypeForward && kind != protocol.BinTypeTerminalForward {
			c.fatal(protocol.ErrCodeProtocol, "invalid binary frame type")
			return
		}
		if int64(len(data)) > h.cfg.MaxFrameSize+protocol.ForwardOverheadBytes {
			c.fatal(protocol.ErrCodeFrameTooLarge, "forward frame exceeds MaxFrameSize")
			return
		}
		var sid protocol.SessionID
		if kind == protocol.BinTypeTerminalForward {
			sid, _, err = protocol.DecodeTerminalForwardFrame(data)
		} else {
			sid, _, err = protocol.DecodeForwardFrame(data)
		}
		if err != nil {
			c.fatal(protocol.ErrCodeProtocol, err.Error())
			return
		}
		if kind == protocol.BinTypeTerminalForward {
			sess, sender, ok := h.takeSession(sh, sid)
			if !ok {
				continue
			}
			if sender != nil {
				sender.pump.CloseSession(sid)
			}
			if result, _ := sess.recv.sendTerminalForward(sid, data); result != forward.Enqueued {
				sess.recv.pump.CloseSession(sid)
			}
			sess.recv.closeAfterFlush()
			continue
		}
		if !h.routeSenderForward(c, sh, sid, data) {
			return
		}
	}
}

func (h *Hub) routeSenderForward(c *conn, sh *share, sid protocol.SessionID, data []byte) bool {
	sess, state, observation := h.lookupSenderSession(c, sh, sid)
	switch state {
	case senderSessionTerminal:
		return true
	case senderSessionNeverKnown:
		return h.sendDetachedSessionError(c, sid, protocol.ErrCodeUnknownSession, "session is unknown", observation)
	}

	switch sess.recv.pump.EnqueueForward(sid, data) {
	case forward.Enqueued:
	case forward.Overflow:
		h.killSession(sh, sess, protocol.ErrCodeSessionOverflow, "receiver forward queue overflow")
	case forward.UnknownSession, forward.SessionTerminated, forward.PumpClosed:
		h.endSession(sh, sess, true)
	}
	return true
}

func (h *Hub) serveReceiver(c *conn, shareID string, join *protocol.Join) {
	// join 状态循环:not_found 后允许同连接重试(短窗退避的竞态是常态,
	// §6.7),每次尝试都过限速闸。
	var sh *share
	var sess *receiverSession
	var manifestPin *admission.LeasePin
	for {
		if join.ShareID != shareID {
			c.fatal(protocol.ErrCodeShareIDMismatch, "join shareId does not match request path")
			return
		}
		var joinAttempt *admission.JoinAttempt
		if h.cfg.Admission != nil {
			var decision admission.Decision
			joinAttempt, decision = h.cfg.Admission.BeginJoin(c.ip)
			if !decision.Allowed() {
				c.fatal(protocol.ErrCodeRateLimited, "join rate limit exceeded; retry with backoff")
				return
			}
		}
		if sh = h.findShare(shareID); sh != nil {
			if joinAttempt != nil {
				decision := joinAttempt.AllowShare(sh.admissionLease)
				if decision == admission.JoinRateExceeded {
					c.fatal(protocol.ErrCodeRateLimited, "join rate limit exceeded; retry with backoff")
					return
				}
				if !decision.Allowed() {
					sh = nil
				}
			}
		}
		if sh != nil {
			if sh.admissionLease != nil {
				var ok bool
				manifestPin, ok = sh.admissionLease.Pin()
				if !ok {
					sh = nil
				}
			}
			if sh != nil {
				sess = h.openSession(sh, c)
			}
			if sess != nil {
				break
			}
			manifestPin.Release()
			manifestPin = nil
		}
		if !c.sendConnection(protocol.NewNotFound()) {
			return
		}
		typ, data, err := c.readActive()
		if err != nil {
			return
		}
		msg, derr := decodeText(typ, data)
		if derr != nil {
			c.fatal(protocol.ErrCodeProtocol, derr.Error())
			return
		}
		switch m := msg.(type) {
		case *protocol.Join:
			join = m
		case *protocol.Keepalive:
			c.sendConnection(protocol.NewKeepalive())
		default:
			c.fatal(protocol.ErrCodeProtocol, "only join or keepalive is allowed before session establishment")
			return
		}
	}
	// 接收端断开(或本循环退出)即会话终结;未及 bye 由中转代发,发送端
	// 无需区分"告别"与"消失"。
	defer h.endSession(sh, sess, true)
	defer manifestPin.Release()

	// 唯一的清单获取路径(D14):manifest{sessionId} + 清单帧,同高优先
	// 通道保序,紧随语义成立(§6.7)。
	if !c.sendConnection(protocol.NewManifest(sess.id.String())) {
		return
	}
	receipt, ok := c.sendBorrowedBinaryHigh(sh.manifestFrame)
	if !ok {
		return
	}
	if err := <-receipt; err != nil {
		return
	}
	manifestPin.Release()
	c.ws.SetReadLimit(h.dataReadLimit())

	for {
		typ, data, err := c.readActive()
		if err != nil {
			return
		}
		if typ == websocket.MessageText {
			msg, err := protocol.Decode(data)
			if err != nil {
				c.fatal(protocol.ErrCodeProtocol, err.Error())
				return
			}
			switch m := msg.(type) {
			case *protocol.Keepalive:
				c.sendConnection(protocol.NewKeepalive())
			case *protocol.Signal:
				if !h.relayControlToSender(c, sh, sess, m.SessionID, m) {
					return
				}
			case *protocol.Bye:
				if m.SessionID != sess.id.String() {
					c.fatal(protocol.ErrCodeUnknownSession, "bye sessionId does not belong to this connection")
					return
				}
				h.endSession(sh, sess, true)
				c.closeAfterFlush()
				return
			case *protocol.Error:
				if c.diagnosticReports.Add(1) > maxClientDiagnosticReports {
					c.fatal(protocol.ErrCodeProtocol, "too many client diagnostic reports")
					return
				}
				h.cfg.Logf("signaling: receiver reported error share=%s code=%s", shareID, m.Code)
			default:
				c.fatal(protocol.ErrCodeProtocol, "receiver must not send "+data2type(data))
				return
			}
			continue
		}
		kind := protocol.BinType(data)
		if kind != protocol.BinTypeForward && kind != protocol.BinTypeTerminalForward {
			c.fatal(protocol.ErrCodeProtocol, "invalid binary frame type")
			return
		}
		if int64(len(data)) > h.cfg.MaxFrameSize+protocol.ForwardOverheadBytes {
			c.fatal(protocol.ErrCodeFrameTooLarge, "forward frame exceeds MaxFrameSize")
			return
		}
		var sid protocol.SessionID
		if kind == protocol.BinTypeTerminalForward {
			sid, _, err = protocol.DecodeTerminalForwardFrame(data)
		} else {
			sid, _, err = protocol.DecodeForwardFrame(data)
		}
		if err != nil {
			c.fatal(protocol.ErrCodeProtocol, err.Error())
			return
		}
		if sid != sess.id {
			c.fatal(protocol.ErrCodeUnknownSession, "forward frame sessionId does not belong to this connection")
			return
		}
		if kind == protocol.BinTypeTerminalForward {
			_, sender, ok := h.takeSession(sh, sid)
			if !ok || sender == nil {
				return
			}
			c.pump.CloseSession(sid)
			if result, _ := sender.sendTerminalForward(sid, data); result != forward.Enqueued {
				sender.pump.CloseSession(sid)
			}
			c.closeAfterFlush()
			return
		}
		_, sender, ok := h.lookupSession(sh, sid)
		if !ok {
			c.fatal(protocol.ErrCodeSenderGone, "session is no longer active")
			return
		}
		switch sender.pump.EnqueueForward(sid, data) {
		case forward.Enqueued:
		case forward.Overflow:
			h.killSession(sh, sess, protocol.ErrCodeSessionOverflow, "sender forward queue overflow")
			return
		case forward.UnknownSession, forward.SessionTerminated, forward.PumpClosed:
			h.endSession(sh, sess, false)
			c.fatal(protocol.ErrCodeSenderGone, "sender disconnected")
			return
		}
	}
}

// relayControlToReceiver keeps signal ordering inside the target session. A
// signal flood overflows and terminates only that session.
func (h *Hub) relayControlToReceiver(c *conn, sh *share, sidStr string, m protocol.Message) bool {
	sid, err := protocol.ParseSessionID(sidStr)
	if err != nil {
		return true
	}
	sess, state, observation := h.lookupSenderSession(c, sh, sid)
	switch state {
	case senderSessionTerminal:
		return true
	case senderSessionNeverKnown:
		return h.sendDetachedSessionError(c, sid, protocol.ErrCodeUnknownSession, "session is unknown", observation)
	}
	switch sess.recv.sendSessionControl(sid, m) {
	case forward.Enqueued:
	case forward.Overflow:
		h.killSession(sh, sess, protocol.ErrCodeSessionOverflow, "session control queue overflow")
	case forward.UnknownSession, forward.SessionTerminated, forward.PumpClosed:
		h.endSession(sh, sess, true)
	}
	return true
}

// relayControlToSender routes non-terminal signal traffic through the
// session's bounded control queue.
func (h *Hub) relayControlToSender(c *conn, sh *share, sess *receiverSession, sidStr string, m protocol.Message) bool {
	if sidStr != sess.id.String() {
		c.fatal(protocol.ErrCodeUnknownSession, "sessionId does not belong to this connection")
		return false
	}
	_, sender, ok := h.lookupSession(sh, sess.id)
	if !ok {
		c.fatal(protocol.ErrCodeSenderGone, "session is no longer active")
		return false
	}
	switch sender.sendSessionControl(sess.id, m) {
	case forward.Enqueued:
		return true
	case forward.Overflow:
		h.killSession(sh, sess, protocol.ErrCodeSessionOverflow, "session control queue overflow")
	case forward.UnknownSession, forward.SessionTerminated, forward.PumpClosed:
		h.endSession(sh, sess, false)
	}
	return false
}

// byeFromSender:发送端结束某个接收会话。
func (h *Hub) byeFromSender(sh *share, m *protocol.Bye) {
	sid, err := protocol.ParseSessionID(m.SessionID)
	if err != nil {
		return
	}
	h.mu.Lock()
	sess, ok := sh.sessions[sid]
	if !ok || h.shares[sh.id] != sh {
		h.mu.Unlock()
		return
	}
	_, sender, _ := h.takeSessionLocked(sh, sid)
	h.mu.Unlock()

	if sender != nil {
		sender.pump.CloseSession(sid)
	}
	if result, _ := sess.recv.sendSessionTerminal(sid, m); result != forward.Enqueued {
		sess.recv.pump.CloseSession(sid)
	}
	sess.recv.closeAfterFlush()
}

// decodeText 校验文本帧并解码;二进制帧在会话建立前一律非法。
func decodeText(typ websocket.MessageType, data []byte) (protocol.Message, error) {
	if typ != websocket.MessageText {
		return nil, errNotText
	}
	return protocol.Decode(data)
}

var errNotText = errProtocol("only text signaling is allowed before session establishment")

type errProtocol string

func (e errProtocol) Error() string { return string(e) }

// data2type 提取消息 type 字段用于报错文本;失败时返回原始截断。
func data2type(data []byte) string {
	m, err := protocol.Decode(data)
	if err == nil {
		switch m.(type) {
		case *protocol.Register:
			return "register"
		case *protocol.Manifest:
			return "manifest"
		case *protocol.NotFound:
			return "not_found"
		case *protocol.Registered:
			return "registered"
		case *protocol.Join:
			return "join"
		}
	}
	if len(data) > 32 {
		data = data[:32]
	}
	return string(data)
}
