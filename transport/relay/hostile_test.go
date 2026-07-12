// 脚本中转测试:用可编排的假中转覆盖真实 hub 不会产出的畸形/否决路径
// (客户端对中转同样不可全信,拒绝路径必须可测)。
package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/internal/testnetwork"
	"github.com/windshare/windshare/relay/protocol"
)

// startScriptRelay 起一个按连接序号执行脚本的假中转;脚本返回即弃连接。
func startScriptRelay(t *testing.T, script func(n int, ws *websocket.Conn)) *httptest.Server {
	t.Helper()
	testnetwork.RequireOSNetwork(t)
	var count atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/ws/{shareId}", func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		ws.SetReadLimit(1 << 26)
		script(int(count.Add(1)), ws)
		_ = ws.CloseNow()
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func sctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Second)
}

// scriptAcceptRegister 消化 register+清单帧;返回解码出的 register(失败回 nil,
// 客户端侧断言才是判据)。
func scriptAcceptRegister(ws *websocket.Conn) *protocol.Register {
	ctx, cancel := sctx()
	defer cancel()
	_, data, err := ws.Read(ctx)
	if err != nil {
		return nil
	}
	m, err := protocol.Decode(data)
	if err != nil {
		return nil
	}
	reg, ok := m.(*protocol.Register)
	if !ok {
		return nil
	}
	if _, _, err := ws.Read(ctx); err != nil { // 清单帧
		return nil
	}
	return reg
}

// scriptAcceptJoin 消化一条 join 文本。
func scriptAcceptJoin(ws *websocket.Conn) bool {
	ctx, cancel := sctx()
	defer cancel()
	_, _, err := ws.Read(ctx)
	return err == nil
}

func scriptSend(ws *websocket.Conn, m protocol.Message) {
	ctx, cancel := sctx()
	defer cancel()
	data, err := protocol.Encode(m)
	if err != nil {
		panic("script: encode: " + err.Error())
	}
	_ = ws.Write(ctx, websocket.MessageText, data)
}

func scriptSendRaw(ws *websocket.Conn, typ websocket.MessageType, data []byte) {
	ctx, cancel := sctx()
	defer cancel()
	_ = ws.Write(ctx, typ, data)
}

// scriptDrain 读到连接死亡为止(顺带消化客户端 keepalive)。
func scriptDrain(ws *websocket.Conn) {
	ctx, cancel := sctx()
	defer cancel()
	for {
		if _, _, err := ws.Read(ctx); err != nil {
			return
		}
	}
}

func testSessionID(b byte) protocol.SessionID {
	var sid protocol.SessionID
	for i := range sid {
		sid[i] = b
	}
	return sid
}

func TestDialSenderHandshakeRejections(t *testing.T) {
	cases := []struct {
		name  string
		reply func(ws *websocket.Conn)
		check func(t *testing.T, err error)
	}{
		{
			name:  "ack for wrong share",
			reply: func(ws *websocket.Conn) { scriptSend(ws, protocol.NewRegistered("b3RoZXI")) },
			check: wantProtocolViolation(ProtocolViolationRegisteredShareMismatch),
		},
		{
			name:  "unexpected message type",
			reply: func(ws *websocket.Conn) { scriptSend(ws, protocol.NewNotFound()) },
			check: wantProtocolViolation(ProtocolViolationUnexpectedMessage),
		},
		{
			name: "binary while awaiting ack",
			reply: func(ws *websocket.Conn) {
				scriptSendRaw(ws, websocket.MessageBinary, []byte{protocol.BinTypeForward})
			},
			check: wantProtocolViolation(ProtocolViolationUnexpectedBinary),
		},
		{
			name:  "garbage json",
			reply: func(ws *websocket.Conn) { scriptSendRaw(ws, websocket.MessageText, []byte("{")) },
			check: wantProtocolViolation(ProtocolViolationMalformedMessage),
		},
		{
			name:  "closed without reply",
			reply: func(ws *websocket.Conn) { _ = ws.CloseNow() },
			check: func(t *testing.T, err error) {
				if _, ok := errors.AsType[*ProtocolViolation](err); ok {
					t.Fatalf("transport close misclassified as protocol violation: %v", err)
				}
			},
		},
		{
			name: "server error reply",
			reply: func(ws *websocket.Conn) {
				scriptSend(ws, protocol.NewError(protocol.ErrCodeManifestBudget, "no room"))
			},
			check: func(t *testing.T, err error) {
				var serr *ServerError
				if !errors.As(err, &serr) || serr.Code != protocol.ErrCodeManifestBudget {
					t.Fatalf("err = %v, want manifest_budget_exceeded", err)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := startScriptRelay(t, func(n int, ws *websocket.Conn) {
				if scriptAcceptRegister(ws) == nil {
					return
				}
				tc.reply(ws)
				scriptDrain(ws)
			})
			_, err := DialSender(testCtx(t), senderCfg(ts.URL))
			if err == nil {
				t.Fatal("DialSender must fail")
			}
			tc.check(t, err)
		})
	}
}

// 首次 register 不携带 token 原像,宽限重注册必须携带且清单逐字节一致(§6.8)。
func TestSenderReconnectResumesWithTokenPreimage(t *testing.T) {
	regs := make(chan *protocol.Register, 4)
	ts := startScriptRelay(t, func(n int, ws *websocket.Conn) {
		reg := scriptAcceptRegister(ws)
		if reg == nil {
			return
		}
		regs <- reg
		switch n {
		case 1:
			scriptSend(ws, protocol.NewRegistered(testShareID))
			_ = ws.CloseNow() // 模拟中转侧断线
		case 2:
			// 半开竞态:中转还以为旧发送端活着 → collision 必须可退避重试。
			scriptSend(ws, protocol.NewError(protocol.ErrCodeShareIDCollision, "stale sender"))
		default:
			scriptSend(ws, protocol.NewRegistered(testShareID))
			scriptDrain(ws)
		}
	})
	s, err := DialSender(testCtx(t), senderCfg(ts.URL))
	if err != nil {
		t.Fatalf("DialSender: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	waitUntil(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.cur != nil && len(regs) >= 3
	}, "sender to reconnect past collision")

	first := <-regs
	if first.ResumeToken != "" {
		t.Fatal("initial register must not carry the token preimage")
	}
	for range 2 {
		resume := <-regs
		if resume.ResumeToken == "" {
			t.Fatal("resume register must present the token preimage")
		}
		if resume.ResumeTokenHash != first.ResumeTokenHash {
			t.Fatal("resume register must keep the same token hash")
		}
	}
	select {
	case <-s.Done():
		t.Fatalf("sender died: %v", s.Err())
	default:
	}
}

func TestSenderReconnectRetriesRateLimitedRegistration(t *testing.T) {
	reconnected := make(chan struct{}, 1)
	ts := startScriptRelay(t, func(n int, ws *websocket.Conn) {
		if scriptAcceptRegister(ws) == nil {
			return
		}
		switch n {
		case 1:
			scriptSend(ws, protocol.NewRegistered(testShareID))
			_ = ws.CloseNow()
		case 2:
			scriptSend(ws, protocol.NewError(protocol.ErrCodeRateLimited, "retry later"))
		default:
			scriptSend(ws, protocol.NewRegistered(testShareID))
			select {
			case reconnected <- struct{}{}:
			default:
			}
			scriptDrain(ws)
		}
	})
	cfg := senderCfg(ts.URL)
	cfg.Backoff = Backoff{Initial: 10 * time.Millisecond, Max: 20 * time.Millisecond}
	s, err := DialSender(testCtx(t), cfg)
	if err != nil {
		t.Fatalf("DialSender: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	select {
	case <-reconnected:
	case <-s.Done():
		t.Fatalf("transient rate limit terminated sender reconnect: %v", s.Err())
	case <-time.After(2 * time.Second):
		t.Fatal("sender did not retry rate-limited registration")
	}
}

func TestSenderReconnectResumeRejectedIsTerminal(t *testing.T) {
	ts := startScriptRelay(t, func(n int, ws *websocket.Conn) {
		if scriptAcceptRegister(ws) == nil {
			return
		}
		if n == 1 {
			scriptSend(ws, protocol.NewRegistered(testShareID))
			_ = ws.CloseNow()
			return
		}
		scriptSend(ws, protocol.NewError(protocol.ErrCodeResumeRejected, "bad preimage"))
	})
	s, err := DialSender(testCtx(t), senderCfg(ts.URL))
	if err != nil {
		t.Fatalf("DialSender: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	waitDone(t, s.Done(), "sender terminal failure")
	var serr *ServerError
	if !errors.As(s.Err(), &serr) || serr.Code != protocol.ErrCodeResumeRejected {
		t.Fatalf("err = %v, want resume_rejected", s.Err())
	}
}

func TestSenderReconnectGraceExpiry(t *testing.T) {
	ts := startScriptRelay(t, func(n int, ws *websocket.Conn) {
		if n == 1 {
			if scriptAcceptRegister(ws) == nil {
				return
			}
			scriptSend(ws, protocol.NewRegistered(testShareID))
		}
		_ = ws.CloseNow() // 之后的重连一律拒之门外
	})
	cfg := senderCfg(ts.URL)
	cfg.ReconnectGrace = 250 * time.Millisecond
	cfg.Backoff = Backoff{Initial: 30 * time.Millisecond, Max: 60 * time.Millisecond}
	s, err := DialSender(testCtx(t), cfg)
	if err != nil {
		t.Fatalf("DialSender: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	waitDone(t, s.Done(), "sender grace expiry")
	if !errors.Is(s.Err(), ErrReconnectGraceExpired) {
		t.Fatalf("err = %v, want ErrReconnectGraceExpired", s.Err())
	}
}

func TestSenderCloseDuringReconnect(t *testing.T) {
	ts := startScriptRelay(t, func(n int, ws *websocket.Conn) {
		if n == 1 {
			if scriptAcceptRegister(ws) == nil {
				return
			}
			scriptSend(ws, protocol.NewRegistered(testShareID))
		}
		_ = ws.CloseNow()
	})
	cfg := senderCfg(ts.URL)
	cfg.ReconnectGrace = 30 * time.Second // 重连本身不会自然结束,只能被 Close 打断
	s, err := DialSender(testCtx(t), cfg)
	if err != nil {
		t.Fatalf("DialSender: %v", err)
	}
	time.Sleep(100 * time.Millisecond) // 让它进入重连退避
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	waitDone(t, s.Done(), "sender teardown")
	if s.Err() != nil {
		t.Fatalf("err after local close = %v, want nil", s.Err())
	}
	if _, ok := <-s.Sessions(); ok {
		t.Fatal("Sessions remained open after sender close")
	}
}

// 会话级 error 只终结该会话;未知会话的 bye/error 留下 tombstone;连接不受波及。
func TestSenderSessionLevelErrorClosesOnlySession(t *testing.T) {
	sid := testSessionID(9)
	ghost := testSessionID(7)
	req, _ := session.EncodeRequest([]uint64{4})
	ts := startScriptRelay(t, func(n int, ws *websocket.Conn) {
		if scriptAcceptRegister(ws) == nil {
			return
		}
		scriptSend(ws, protocol.NewRegistered(testShareID))
		// Terminal control may precede data. A later same-SID frame must not
		// materialize a session that has already ended.
		scriptSend(ws, protocol.NewBye(ghost.String()))
		scriptSend(ws, protocol.NewSessionError(ghost.String(), protocol.ErrCodeUnknownSession, "ghost"))
		scriptSendRaw(ws, websocket.MessageBinary, protocol.EncodeForwardFrame(ghost, req))
		// 物化真实会话后对其发会话级 error。
		scriptSendRaw(ws, websocket.MessageBinary, protocol.EncodeForwardFrame(sid, req))
		scriptSend(ws, protocol.NewSessionError(sid.String(), protocol.ErrCodeSessionOverflow, "slow receiver"))
		scriptDrain(ws)
	})
	s, err := DialSender(testCtx(t), senderCfg(ts.URL))
	if err != nil {
		t.Fatalf("DialSender: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	sess := awaitSession(t, s)
	if sess.SessionID() != sid {
		t.Fatalf("session id = %s, want %s", sess.SessionID(), sid)
	}
	_ = recvFrame(t, sess)
	waitRecvClosed(t, sess)
	var serr *ServerError
	if !errors.As(sess.Err(), &serr) || serr.Code != protocol.ErrCodeSessionOverflow {
		t.Fatalf("session err = %v, want session_overflow", sess.Err())
	}
	select {
	case <-s.Done():
		t.Fatalf("conn must survive a session-level error, died with %v", s.Err())
	case <-time.After(200 * time.Millisecond):
	}
}

func TestSenderDiscardsForwardAfterByeWithoutRevivingSession(t *testing.T) {
	sid := testSessionID(4)
	request, _ := session.EncodeRequest([]uint64{1})
	ts := startScriptRelay(t, func(n int, ws *websocket.Conn) {
		if scriptAcceptRegister(ws) == nil {
			return
		}
		scriptSend(ws, protocol.NewRegistered(testShareID))
		scriptSendRaw(ws, websocket.MessageBinary, protocol.EncodeForwardFrame(sid, request))
		scriptSend(ws, protocol.NewBye(sid.String()))
		scriptSendRaw(ws, websocket.MessageBinary, protocol.EncodeForwardFrame(sid, request))
		scriptDrain(ws)
	})
	s, err := DialSender(testCtx(t), senderCfg(ts.URL))
	if err != nil {
		t.Fatalf("DialSender: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ch := awaitSession(t, s)
	_ = recvFrame(t, ch)
	waitRecvClosed(t, ch)
	select {
	case duplicate := <-s.Sessions():
		t.Fatalf("post-bye frame revived session %s", duplicate.SessionID())
	case <-time.After(200 * time.Millisecond):
	}
	select {
	case <-s.Done():
		t.Fatalf("post-bye frame killed sender connection: %v", s.Err())
	case <-time.After(100 * time.Millisecond):
	}
}

func TestSenderSessionHistoryRejectsUnboundedTombstones(t *testing.T) {
	const historyLimit = 2
	s := &SenderConn{
		cfg:  SenderConfig{SessionHistoryLimit: historyLimit},
		live: make(map[protocol.SessionID]*clientSession),
	}
	for i := byte(1); i <= historyLimit; i++ {
		if err := s.terminateSession(testSessionID(i), nil, nil); err != nil {
			t.Fatalf("track tombstone %d: %v", i, err)
		}
	}
	err := s.terminateSession(testSessionID(historyLimit+1), nil, nil)
	if !errors.Is(err, ErrSenderSessionHistoryFull) {
		t.Fatalf("history overflow = %v, want ErrSenderSessionHistoryFull", err)
	}
	if got := len(s.live); got != historyLimit {
		t.Fatalf("tracked sessions = %d, want bounded at %d", got, historyLimit)
	}
}

func TestSenderSessionHistoryOverflowRecyclesLink(t *testing.T) {
	const historyLimit = 2
	reconnected := make(chan struct{})
	ts := startScriptRelay(t, func(n int, ws *websocket.Conn) {
		if scriptAcceptRegister(ws) == nil {
			return
		}
		scriptSend(ws, protocol.NewRegistered(testShareID))
		switch n {
		case 1:
			for i := byte(1); i <= historyLimit+1; i++ {
				scriptSend(ws, protocol.NewBye(testSessionID(i).String()))
			}
		case 2:
			close(reconnected)
		}
		scriptDrain(ws)
	})
	cfg := senderCfg(ts.URL)
	cfg.SessionHistoryLimit = historyLimit
	cfg.ReconnectGrace = 2 * time.Second
	cfg.Backoff = Backoff{Initial: 10 * time.Millisecond, Max: 20 * time.Millisecond}
	s, err := DialSender(testCtx(t), cfg)
	if err != nil {
		t.Fatalf("DialSender: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	select {
	case <-reconnected:
	case <-time.After(2 * time.Second):
		t.Fatal("sender did not recycle the link after session history overflow")
	}
	s.mu.Lock()
	tracked := len(s.live)
	s.mu.Unlock()
	if tracked != 0 {
		t.Fatalf("session history survived link recycle: %d entries", tracked)
	}
	select {
	case <-s.Done():
		t.Fatalf("sender terminated instead of recovering on a clean link: %v", s.Err())
	case <-time.After(100 * time.Millisecond):
	}
}

func TestSenderConnLevelErrorIsTerminal(t *testing.T) {
	ts := startScriptRelay(t, func(n int, ws *websocket.Conn) {
		if scriptAcceptRegister(ws) == nil {
			return
		}
		scriptSend(ws, protocol.NewRegistered(testShareID))
		scriptSend(ws, protocol.NewError(protocol.ErrCodeProtocol, "you broke the rules"))
		scriptDrain(ws)
	})
	cfg := senderCfg(ts.URL)
	cfg.ReconnectGrace = 30 * time.Second // 若误走重连,waitDone 会超时暴露
	s, err := DialSender(testCtx(t), cfg)
	if err != nil {
		t.Fatalf("DialSender: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	waitDone(t, s.Done(), "sender terminal failure")
	var serr *ServerError
	if !errors.As(s.Err(), &serr) || serr.Code != protocol.ErrCodeProtocol {
		t.Fatalf("err = %v, want protocol_error", s.Err())
	}
	if _, ok := <-s.Sessions(); ok {
		t.Fatal("Sessions must close on terminal failure")
	}
}

// 中转发疯(畸形帧/不该出现的消息)按传输故障走重连;假中转拒绝重连,
// 最终以宽限耗尽收场,首因保留在错误链里。
func TestSenderProtocolViolationReconnectsThenExpires(t *testing.T) {
	poisons := []struct {
		name   string
		poison func(ws *websocket.Conn)
		want   ProtocolViolationKind
	}{
		{
			name:   "unexpected registered mid-session",
			poison: func(ws *websocket.Conn) { scriptSend(ws, protocol.NewRegistered(testShareID)) },
			want:   ProtocolViolationUnexpectedMessage,
		},
		{
			name:   "malformed forward frame",
			poison: func(ws *websocket.Conn) { scriptSendRaw(ws, websocket.MessageBinary, []byte{0x07}) },
			want:   ProtocolViolationMalformedFrame,
		},
		{
			name:   "garbage json mid-session",
			poison: func(ws *websocket.Conn) { scriptSendRaw(ws, websocket.MessageText, []byte("{")) },
			want:   ProtocolViolationMalformedMessage,
		},
	}
	for _, tc := range poisons {
		t.Run(tc.name, func(t *testing.T) {
			ts := startScriptRelay(t, func(n int, ws *websocket.Conn) {
				if n > 1 {
					_ = ws.CloseNow()
					return
				}
				if scriptAcceptRegister(ws) == nil {
					return
				}
				scriptSend(ws, protocol.NewRegistered(testShareID))
				tc.poison(ws)
				scriptDrain(ws)
			})
			cfg := senderCfg(ts.URL)
			cfg.ReconnectGrace = 200 * time.Millisecond
			cfg.Backoff = Backoff{Initial: 20 * time.Millisecond, Max: 50 * time.Millisecond}
			s, err := DialSender(testCtx(t), cfg)
			if err != nil {
				t.Fatalf("DialSender: %v", err)
			}
			t.Cleanup(func() { _ = s.Close() })
			waitDone(t, s.Done(), "sender expiry after violation")
			if !errors.Is(s.Err(), ErrReconnectGraceExpired) {
				t.Fatalf("err = %v, want ErrReconnectGraceExpired", s.Err())
			}
			wantProtocolViolation(tc.want)(t, s.Err())
		})
	}
}

func TestDialReceiverJoinRejections(t *testing.T) {
	sid := testSessionID(3)
	cases := []struct {
		name  string
		reply func(ws *websocket.Conn)
		check func(t *testing.T, err error)
	}{
		{
			name: "manifest message without frame",
			reply: func(ws *websocket.Conn) {
				scriptSend(ws, protocol.NewManifest(sid.String()))
				scriptSend(ws, protocol.NewKeepalive())
			},
			check: wantProtocolViolation(ProtocolViolationManifestSequence),
		},
		{
			name: "bad manifest frame",
			reply: func(ws *websocket.Conn) {
				scriptSend(ws, protocol.NewManifest(sid.String()))
				scriptSendRaw(ws, websocket.MessageBinary, []byte{protocol.BinTypeForward, 0x01})
			},
			check: wantProtocolViolation(ProtocolViolationMalformedManifest),
		},
		{
			name:  "permanent server error",
			reply: func(ws *websocket.Conn) { scriptSend(ws, protocol.NewError(protocol.ErrCodeProtocol, "nope")) },
			check: func(t *testing.T, err error) {
				var serr *ServerError
				if !errors.As(err, &serr) || serr.Code != protocol.ErrCodeProtocol {
					t.Fatalf("err = %v, want protocol_error", err)
				}
			},
		},
		{
			name:  "unexpected message",
			reply: func(ws *websocket.Conn) { scriptSend(ws, protocol.NewRegistered(testShareID)) },
			check: wantProtocolViolation(ProtocolViolationUnexpectedMessage),
		},
		{
			name:  "garbage json",
			reply: func(ws *websocket.Conn) { scriptSendRaw(ws, websocket.MessageText, []byte("{")) },
			check: wantProtocolViolation(ProtocolViolationMalformedMessage),
		},
		{
			name: "binary before manifest",
			reply: func(ws *websocket.Conn) {
				scriptSendRaw(ws, websocket.MessageBinary, []byte{protocol.BinTypeForward})
			},
			check: wantProtocolViolation(ProtocolViolationUnexpectedBinary),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := startScriptRelay(t, func(n int, ws *websocket.Conn) {
				if !scriptAcceptJoin(ws) {
					return
				}
				tc.reply(ws)
				scriptDrain(ws)
			})
			_, err := DialReceiver(testCtx(t), receiverCfg(ts.URL))
			if err == nil {
				t.Fatal("DialReceiver must fail")
			}
			tc.check(t, err)
		})
	}
}

func TestDialReceiverRateLimitedRetriesThenExpires(t *testing.T) {
	var joins atomic.Int32
	ts := startScriptRelay(t, func(n int, ws *websocket.Conn) {
		if !scriptAcceptJoin(ws) {
			return
		}
		joins.Add(1)
		scriptSend(ws, protocol.NewError(protocol.ErrCodeRateLimited, "slow down"))
		// 与真实中转同规:限速后直接关连接,让退避发生在客户端(§6.8)。
	})
	cfg := receiverCfg(ts.URL)
	cfg.JoinRetryWindow = 300 * time.Millisecond
	cfg.Backoff = Backoff{Initial: 40 * time.Millisecond, Max: 80 * time.Millisecond}
	_, err := DialReceiver(testCtx(t), cfg)
	var serr *ServerError
	if !errors.As(err, &serr) || serr.Code != protocol.ErrCodeRateLimited {
		t.Fatalf("err = %v, want wrapped rate_limited", err)
	}
	if joins.Load() < 2 {
		t.Fatalf("expected retries across connections, got %d join(s)", joins.Load())
	}
}

func TestDialReceiverJoinWindowInterruptsSilentRelay(t *testing.T) {
	ts := startScriptRelay(t, func(_ int, ws *websocket.Conn) {
		if !scriptAcceptJoin(ws) {
			return
		}
		scriptDrain(ws)
	})
	cfg := receiverCfg(ts.URL)
	cfg.JoinRetryWindow = 100 * time.Millisecond
	cfg.Backoff = Backoff{Initial: 10 * time.Millisecond, Max: 20 * time.Millisecond}
	done := make(chan error, 1)
	go func() {
		_, err := DialReceiver(context.Background(), cfg)
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("silent relay error = %v, want join-window deadline", err)
		}
	case <-time.After(time.Second):
		t.Fatal("JoinRetryWindow did not interrupt a silent relay handshake")
	}
}

func TestDialReceiverCallerCancellationOverridesPriorNotFound(t *testing.T) {
	retried := make(chan struct{})
	ts := startScriptRelay(t, func(_ int, ws *websocket.Conn) {
		if !scriptAcceptJoin(ws) {
			return
		}
		scriptSend(ws, protocol.NewNotFound())
		if !scriptAcceptJoin(ws) {
			return
		}
		close(retried)
		scriptDrain(ws)
	})
	cfg := receiverCfg(ts.URL)
	cfg.JoinRetryWindow = 5 * time.Second
	cfg.Backoff = Backoff{Initial: time.Millisecond, Max: time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := DialReceiver(ctx, cfg)
		done <- err
	}()

	select {
	case <-retried:
		cancel()
	case <-time.After(time.Second):
		cancel()
		t.Fatal("receiver did not retry after not_found")
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("DialReceiver error = %v, want caller cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("caller cancellation did not interrupt join")
	}
}

func TestReceiverSessionProtocolViolations(t *testing.T) {
	sid := testSessionID(5)
	foreign := testSessionID(6)
	req, _ := session.EncodeRequest([]uint64{1})
	cases := []struct {
		name   string
		poison func(ws *websocket.Conn)
		check  func(t *testing.T, err error)
	}{
		{
			name: "signal for foreign session",
			poison: func(ws *websocket.Conn) {
				scriptSend(ws, protocol.NewSignal(foreign.String(), "offer", json.RawMessage(`{}`)))
			},
			check: wantProtocolViolation(ProtocolViolationForeignSession),
		},
		{
			name:   "bye for foreign session",
			poison: func(ws *websocket.Conn) { scriptSend(ws, protocol.NewBye(foreign.String())) },
			check:  wantProtocolViolation(ProtocolViolationForeignSession),
		},
		{
			name: "error for foreign session",
			poison: func(ws *websocket.Conn) {
				scriptSend(ws, protocol.NewSessionError(foreign.String(), protocol.ErrCodeSessionOverflow, "slow"))
			},
			check: wantProtocolViolation(ProtocolViolationForeignSession),
		},
		{
			name: "forward frame for foreign session",
			poison: func(ws *websocket.Conn) {
				scriptSendRaw(ws, websocket.MessageBinary, protocol.EncodeForwardFrame(foreign, req))
			},
			check: wantProtocolViolation(ProtocolViolationForeignSession),
		},
		{
			name:   "malformed binary frame",
			poison: func(ws *websocket.Conn) { scriptSendRaw(ws, websocket.MessageBinary, []byte{0x07}) },
			check:  wantProtocolViolation(ProtocolViolationMalformedFrame),
		},
		{
			name:   "garbage json",
			poison: func(ws *websocket.Conn) { scriptSendRaw(ws, websocket.MessageText, []byte("{")) },
			check:  wantProtocolViolation(ProtocolViolationMalformedMessage),
		},
		{
			name:   "unexpected message in session",
			poison: func(ws *websocket.Conn) { scriptSend(ws, protocol.NewNotFound()) },
			check:  wantProtocolViolation(ProtocolViolationUnexpectedMessage),
		},
		{
			name: "session error",
			poison: func(ws *websocket.Conn) {
				scriptSend(ws, protocol.NewSessionError(sid.String(), protocol.ErrCodeSessionOverflow, "slow"))
			},
			check: func(t *testing.T, err error) {
				var serr *ServerError
				if !errors.As(err, &serr) || serr.Code != protocol.ErrCodeSessionOverflow {
					t.Fatalf("err = %v, want session_overflow", err)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := startScriptRelay(t, func(n int, ws *websocket.Conn) {
				if !scriptAcceptJoin(ws) {
					return
				}
				scriptSend(ws, protocol.NewManifest(sid.String()))
				scriptSendRaw(ws, websocket.MessageBinary, protocol.EncodeManifestFrame(testManifest))
				tc.poison(ws)
				scriptDrain(ws)
			})
			r, err := DialReceiver(testCtx(t), receiverCfg(ts.URL))
			if err != nil {
				t.Fatalf("DialReceiver: %v", err)
			}
			t.Cleanup(func() { _ = r.Close() })
			waitDone(t, r.Done(), "receiver death")
			tc.check(t, r.Err())
			if r.Channel().State() != session.Closed {
				t.Fatal("channel must be Closed after conn death")
			}
		})
	}
}

func wantProtocolViolation(kind ProtocolViolationKind) func(t *testing.T, err error) {
	return func(t *testing.T, err error) {
		t.Helper()
		violation, ok := errors.AsType[*ProtocolViolation](err)
		if !ok || violation.Kind != kind {
			t.Fatalf("err = %v, want ProtocolViolation kind %q", err, kind)
		}
	}
}

// 发送端 bye:接收端以干净终局收场(Err=nil,通道 Closed)。
func TestReceiverSenderByeIsClean(t *testing.T) {
	sid := testSessionID(8)
	ts := startScriptRelay(t, func(n int, ws *websocket.Conn) {
		if !scriptAcceptJoin(ws) {
			return
		}
		scriptSend(ws, protocol.NewManifest(sid.String()))
		scriptSendRaw(ws, websocket.MessageBinary, protocol.EncodeManifestFrame(testManifest))
		scriptSend(ws, protocol.NewBye(sid.String()))
		scriptDrain(ws)
	})
	r, err := DialReceiver(testCtx(t), receiverCfg(ts.URL))
	if err != nil {
		t.Fatalf("DialReceiver: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	waitDone(t, r.Done(), "receiver clean end")
	if r.Err() != nil {
		t.Fatalf("err = %v, want nil after sender bye", r.Err())
	}
	if r.Channel().State() != session.Closed || r.Channel().Err() != nil {
		t.Fatalf("channel state/err = %v/%v, want Closed/nil", r.Channel().State(), r.Channel().Err())
	}
	if !bytes.Equal(r.SealedManifest(), testManifest) {
		t.Fatal("manifest mismatch")
	}
	if err := r.Channel().SendSignal(context.Background(), protocol.SignalKindOffer, json.RawMessage(`{}`)); !errors.Is(err, ErrChannelClosed) {
		t.Fatalf("SendSignal after death = %v, want ErrChannelClosed", err)
	}
}
