// 集成测试:起 in-process 中转(httptest + 真实 signaling/forward/httpapi
// 组件),用本包客户端跑通注册/加入/转发/重连全链路(§6.7/§6.8)。
package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"

	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/internal/testnetwork"
	"github.com/windshare/windshare/relay/httpapi"
	"github.com/windshare/windshare/relay/protocol"
	"github.com/windshare/windshare/relay/signaling"
)

const (
	testShareID = "dGVzdF9zaGFyZQ"
	// waitTimeout 只防测试卡死,不承载时序语义。
	waitTimeout = 10 * time.Second
)

var (
	testManifest = bytes.Repeat([]byte{0xab, 0xcd}, 128)
	testToken    = bytes.Repeat([]byte{0x42}, protocol.ResumeTokenBytes)
)

func startRelay(t *testing.T) *httptest.Server {
	t.Helper()
	testnetwork.RequireOSNetwork(t)
	hub := signaling.NewHub(signaling.Config{})
	t.Cleanup(hub.Close)
	ts := httptest.NewServer(httpapi.NewHandler(httpapi.Config{Hub: hub}))
	t.Cleanup(ts.Close)
	return ts
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// 短时值只为测试节奏;语义与生产默认一致。
func senderCfg(url string) SenderConfig {
	return SenderConfig{
		RelayURL:          url,
		ShareID:           testShareID,
		SealedManifest:    testManifest,
		ResumeToken:       testToken,
		KeepaliveInterval: 50 * time.Millisecond,
		ReconnectGrace:    5 * time.Second,
		Backoff:           Backoff{Initial: 20 * time.Millisecond, Max: 100 * time.Millisecond},
	}
}

func receiverCfg(url string) ReceiverConfig {
	return ReceiverConfig{
		RelayURL:          url,
		ShareID:           testShareID,
		KeepaliveInterval: 50 * time.Millisecond,
		JoinRetryWindow:   8 * time.Second,
		Backoff:           Backoff{Initial: 20 * time.Millisecond, Max: 100 * time.Millisecond},
	}
}

func dialPair(t *testing.T, ts *httptest.Server) (*SenderConn, *ReceiverConn) {
	t.Helper()
	s, err := DialSender(testCtx(t), senderCfg(ts.URL))
	if err != nil {
		t.Fatalf("DialSender: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	r, err := DialReceiver(testCtx(t), receiverCfg(ts.URL))
	if err != nil {
		t.Fatalf("DialReceiver: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return s, r
}

func awaitSession(t *testing.T, s *SenderConn) *Channel {
	t.Helper()
	select {
	case ch, ok := <-s.Sessions():
		if !ok {
			t.Fatal("Sessions closed before a session arrived")
		}
		return ch
	case <-time.After(waitTimeout):
		t.Fatal("timeout waiting for sender session")
		return nil
	}
}

func recvFrame(t *testing.T, ch *Channel) session.Frame {
	t.Helper()
	select {
	case f, ok := <-ch.Recv():
		if !ok {
			t.Fatal("recv stream closed while expecting a frame")
		}
		return f
	case <-time.After(waitTimeout):
		t.Fatal("timeout waiting for frame")
		return nil
	}
}

func waitRecvClosed(t *testing.T, ch *Channel) {
	t.Helper()
	deadline := time.After(waitTimeout)
	for {
		select {
		case _, ok := <-ch.Recv():
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("timeout waiting for recv stream to close")
		}
	}
}

func waitDone(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(waitTimeout):
		t.Fatalf("timeout waiting for %s", what)
	}
}

// dropSenderLink 模拟发送端网络断线(非 Close 语义):撕掉当前 WS 而不置
// closed,触发宽限重连路径。
func dropSenderLink(t *testing.T, s *SenderConn) {
	t.Helper()
	s.mu.Lock()
	l := s.cur
	s.mu.Unlock()
	if l == nil {
		t.Fatal("sender has no active link to drop")
	}
	l.fail(errors.New("simulated network drop"))
}

func TestRegisterJoinManifestRoundtrip(t *testing.T) {
	ts := startRelay(t)
	_, r := dialPair(t, ts)
	if !bytes.Equal(r.SealedManifest(), testManifest) {
		t.Fatalf("manifest mismatch: got %d bytes", len(r.SealedManifest()))
	}
	if r.Channel().State() != session.Open {
		t.Fatalf("receiver channel state = %v, want Open", r.Channel().State())
	}
	if r.Channel().SessionID() != r.SessionID() {
		t.Fatal("channel/conn sessionId mismatch")
	}
}

// REQUEST/BLOCK 经转发帧包裹双向往返,与 core/session 帧编解码对拍
// (多帧块覆盖 seq/last 语义)。
func TestRequestBlockRoundtrip(t *testing.T) {
	ts := startRelay(t)
	ctx := testCtx(t)
	s, r := dialPair(t, ts)

	req, err := session.EncodeRequest([]uint64{7, 9})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if err := r.Channel().Send(ctx, req); err != nil {
		t.Fatalf("receiver send REQUEST: %v", err)
	}

	sess := awaitSession(t, s)
	msg, err := session.Decode(recvFrame(t, sess))
	if err != nil {
		t.Fatalf("decode at sender: %v", err)
	}
	gotReq, ok := msg.(*session.Request)
	if !ok || len(gotReq.Indices) != 2 || gotReq.Indices[0] != 7 || gotReq.Indices[1] != 9 {
		t.Fatalf("sender got %#v, want REQUEST{7,9}", msg)
	}

	blockCT := bytes.Repeat([]byte{0x5e, 0x1f}, 50) // 100B / 32B 载荷 → 4 帧
	frames, err := session.SplitBlockCT(7, blockCT, 32)
	if err != nil {
		t.Fatalf("SplitBlockCT: %v", err)
	}
	for _, f := range frames {
		if err := sess.Send(ctx, f); err != nil {
			t.Fatalf("sender send BLOCK: %v", err)
		}
	}

	var got []byte
	for seq := uint32(0); ; seq++ {
		m, err := session.Decode(recvFrame(t, r.Channel()))
		if err != nil {
			t.Fatalf("decode at receiver: %v", err)
		}
		b, ok := m.(*session.Block)
		if !ok {
			t.Fatalf("receiver got %#v, want BLOCK", m)
		}
		if b.Index != 7 || b.Seq != seq {
			t.Fatalf("BLOCK index/seq = %d/%d, want 7/%d", b.Index, b.Seq, seq)
		}
		got = append(got, b.Payload...)
		if b.Last {
			break
		}
	}
	if !bytes.Equal(got, blockCT) {
		t.Fatal("reassembled block ciphertext mismatch")
	}
}

func TestTerminalFrameArrivesBeforeReceiveClosureAcrossRelay(t *testing.T) {
	ts := startRelay(t)
	ctx := testCtx(t)
	s, r := dialPair(t, ts)

	request, err := session.EncodeRequest([]uint64{0})
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if err := r.Channel().Send(ctx, request); err != nil {
		t.Fatalf("receiver request: %v", err)
	}
	senderSession := awaitSession(t, s)
	_ = recvFrame(t, senderSession)

	terminal, err := session.EncodeError(session.ErrCodeBlockRead, "source snapshot drift")
	if err != nil {
		t.Fatalf("EncodeError: %v", err)
	}
	if err := senderSession.SendTerminal(ctx, terminal); err != nil {
		t.Fatalf("SendTerminal: %v", err)
	}

	got := recvFrame(t, r.Channel())
	message, err := session.Decode(got)
	if err != nil {
		t.Fatalf("Decode terminal: %v", err)
	}
	remote, ok := message.(*session.Error)
	if !ok || remote.Code != session.ErrCodeBlockRead || remote.Msg != "source snapshot drift" {
		t.Fatalf("terminal = %#v", message)
	}
	waitRecvClosed(t, r.Channel())
	waitDone(t, r.Done(), "receiver close after terminal")
	if r.Err() != nil {
		t.Fatalf("receiver connection error = %v, want terminal carried in FrameChannel", r.Err())
	}
	select {
	case <-s.Done():
		t.Fatalf("sender connection died with session terminal: %v", s.Err())
	case <-time.After(100 * time.Millisecond):
	}
}

// 多接收会话并发隔离:各会话请求不同块,应答只回到各自通道。
func TestMultiReceiverSessionIsolation(t *testing.T) {
	ts := startRelay(t)
	ctx := testCtx(t)
	s, err := DialSender(ctx, senderCfg(ts.URL))
	if err != nil {
		t.Fatalf("DialSender: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	const n = 3
	receivers := make([]*ReceiverConn, n)
	for i := range receivers {
		r, err := DialReceiver(ctx, receiverCfg(ts.URL))
		if err != nil {
			t.Fatalf("DialReceiver #%d: %v", i, err)
		}
		t.Cleanup(func() { _ = r.Close() })
		receivers[i] = r
	}

	// 接收端并发请求并校验回包归属(先起,发送会话由首帧物化)。
	errCh := make(chan error, n)
	for i, r := range receivers {
		go func(idx uint64, r *ReceiverConn) {
			req, _ := session.EncodeRequest([]uint64{idx})
			if err := r.Channel().Send(ctx, req); err != nil {
				errCh <- err
				return
			}
			var got []byte
			for {
				m, err := session.Decode(<-r.Channel().Recv())
				if err != nil {
					errCh <- err
					return
				}
				b := m.(*session.Block)
				if b.Index != idx {
					errCh <- errors.New("received block for foreign session")
					return
				}
				got = append(got, b.Payload...)
				if b.Last {
					break
				}
			}
			if !bytes.Equal(got, bytes.Repeat([]byte{byte(idx)}, 64)) {
				errCh <- errors.New("payload mismatch")
				return
			}
			errCh <- nil
		}(uint64(i+1), r)
	}

	// 发送端:会话按接收端首帧到达顺序物化;每会话一个 goroutine,
	// 按请求块号回填充值 payload。
	seen := make(map[protocol.SessionID]bool)
	for range n {
		sess := awaitSession(t, s)
		if seen[sess.SessionID()] {
			t.Fatalf("duplicate session %s", sess.SessionID())
		}
		seen[sess.SessionID()] = true
		go func(sess *Channel) {
			m, err := session.Decode(<-sess.Recv())
			if err != nil {
				t.Errorf("decode request: %v", err)
				return
			}
			idx := m.(*session.Request).Indices[0]
			frames, err := session.SplitBlockCT(idx, bytes.Repeat([]byte{byte(idx)}, 64), 32)
			if err != nil {
				t.Errorf("split: %v", err)
				return
			}
			for _, f := range frames {
				if err := sess.Send(ctx, f); err != nil {
					t.Errorf("send: %v", err)
					return
				}
			}
		}(sess)
	}

	for range n {
		select {
		case err := <-errCh:
			if err != nil {
				t.Fatalf("receiver flow: %v", err)
			}
		case <-time.After(waitTimeout):
			t.Fatal("timeout in concurrent receiver flows")
		}
	}
}

// 接收端断连(礼貌 bye 与硬断)都应让发送侧对应通道进入 Closed 且入站流
// 关闭——硬断由中转合成 bye(§6.7)。
func TestReceiverGoneClosesSenderSession(t *testing.T) {
	for _, polite := range []bool{true, false} {
		name := "abrupt"
		if polite {
			name = "polite"
		}
		t.Run(name, func(t *testing.T) {
			ts := startRelay(t)
			ctx := testCtx(t)
			s, r := dialPair(t, ts)

			req, _ := session.EncodeRequest([]uint64{1})
			if err := r.Channel().Send(ctx, req); err != nil {
				t.Fatalf("send: %v", err)
			}
			sess := awaitSession(t, s)
			_ = recvFrame(t, sess)

			if polite {
				_ = r.Close()
			} else {
				r.l.fail(errors.New("simulated receiver crash"))
			}
			waitRecvClosed(t, sess)
			if sess.State() != session.Closed {
				t.Fatalf("sender session state = %v, want Closed", sess.State())
			}
			waitDone(t, r.Done(), "receiver conn teardown")
		})
	}
}

// 发送端断线 → 宽限内凭 token 原像重连恢复 → 接收端 rejoin(新 sessionId)
// 续传(§6.8/§6.12)。
func TestSenderReconnectAndReceiverRejoin(t *testing.T) {
	ts := startRelay(t)
	ctx := testCtx(t)
	s, r1 := dialPair(t, ts)

	req, _ := session.EncodeRequest([]uint64{1})
	if err := r1.Channel().Send(ctx, req); err != nil {
		t.Fatalf("send: %v", err)
	}
	sess1 := awaitSession(t, s)
	_ = recvFrame(t, sess1)
	sid1 := sess1.SessionID()

	dropSenderLink(t, s)

	// 旧发送会话随链路闭合;接收端拿到 sender_gone 后由上层 rejoin。
	waitRecvClosed(t, sess1)
	waitDone(t, r1.Done(), "receiver conn death")
	var serr *ServerError
	if !errors.As(r1.Err(), &serr) || serr.Code != protocol.ErrCodeSenderGone {
		t.Fatalf("receiver err = %v, want sender_gone", r1.Err())
	}

	// rejoin:JoinRetryWindow 覆盖发送端重连退避,not_found 期间自动重试。
	r2, err := DialReceiver(ctx, receiverCfg(ts.URL))
	if err != nil {
		t.Fatalf("rejoin: %v", err)
	}
	t.Cleanup(func() { _ = r2.Close() })
	if !bytes.Equal(r2.SealedManifest(), testManifest) {
		t.Fatal("manifest changed across sender reconnect")
	}

	req2, _ := session.EncodeRequest([]uint64{2})
	if err := r2.Channel().Send(ctx, req2); err != nil {
		t.Fatalf("send after rejoin: %v", err)
	}
	sess2 := awaitSession(t, s)
	if sess2.SessionID() == sid1 {
		t.Fatal("rejoined session must get a fresh sessionId")
	}
	m, err := session.Decode(recvFrame(t, sess2))
	if err != nil {
		t.Fatalf("decode after reconnect: %v", err)
	}
	if got := m.(*session.Request).Indices[0]; got != 2 {
		t.Fatalf("request index = %d, want 2", got)
	}
	// 回一块,验证重连后双向可用。
	frames, _ := session.SplitBlockCT(2, []byte{9, 9, 9}, 32)
	if err := sess2.Send(ctx, frames[0]); err != nil {
		t.Fatalf("send block after reconnect: %v", err)
	}
	b := mustDecodeBlock(t, recvFrame(t, r2.Channel()))
	if b.Index != 2 || !b.Last {
		t.Fatalf("got BLOCK{index=%d,last=%v}, want index 2 last frame", b.Index, b.Last)
	}
}

func mustDecodeBlock(t *testing.T, f session.Frame) *session.Block {
	t.Helper()
	m, err := session.Decode(f)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	b, ok := m.(*session.Block)
	if !ok {
		t.Fatalf("got %#v, want BLOCK", m)
	}
	return b
}

// not_found 短窗退避:窗口耗尽明确失败;join 先于 register 的竞态在窗口
// 内自愈(§6.7)。
func TestJoinNotFoundBackoff(t *testing.T) {
	ts := startRelay(t)
	ctx := testCtx(t)

	expired := receiverCfg(ts.URL)
	expired.JoinRetryWindow = 300 * time.Millisecond
	expired.Backoff = Backoff{Initial: 30 * time.Millisecond, Max: 60 * time.Millisecond}
	if _, err := DialReceiver(ctx, expired); !errors.Is(err, ErrShareNotFound) {
		t.Fatalf("err = %v, want ErrShareNotFound", err)
	}

	type result struct {
		r   *ReceiverConn
		err error
	}
	resCh := make(chan result, 1)
	go func() {
		r, err := DialReceiver(ctx, receiverCfg(ts.URL))
		resCh <- result{r, err}
	}()
	time.Sleep(150 * time.Millisecond) // 让 join 先跑起来吃到 not_found
	s, err := DialSender(ctx, senderCfg(ts.URL))
	if err != nil {
		t.Fatalf("DialSender: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	select {
	case res := <-resCh:
		if res.err != nil {
			t.Fatalf("join after register: %v", res.err)
		}
		t.Cleanup(func() { _ = res.r.Close() })
		if !bytes.Equal(res.r.SealedManifest(), testManifest) {
			t.Fatal("manifest mismatch")
		}
	case <-time.After(waitTimeout):
		t.Fatal("timeout waiting for delayed join")
	}
}

// signal 双向透传;offer 先于任何数据帧到达也要物化新会话(§6.11)。
func TestSignalPassthrough(t *testing.T) {
	ts := startRelay(t)
	ctx := testCtx(t)
	s, r := dialPair(t, ts)

	offer := json.RawMessage(`{"sdp":"offer-blob"}`)
	if err := r.Channel().SendSignal(ctx, protocol.SignalKindOffer, offer); err != nil {
		t.Fatalf("receiver SendSignal: %v", err)
	}
	sess := awaitSession(t, s) // 由 signal 物化
	var sig Signal
	select {
	case sig = <-sess.Signals():
	case <-time.After(waitTimeout):
		t.Fatal("timeout waiting for signal at sender")
	}
	if sig.Kind != protocol.SignalKindOffer || !bytes.Equal(sig.Payload, offer) {
		t.Fatalf("sender got signal %+v", sig)
	}
	if sess.SessionID() != r.SessionID() {
		t.Fatal("signal sessionId mismatch")
	}

	answer := json.RawMessage(`{"sdp":"answer-blob"}`)
	if err := sess.SendSignal(ctx, protocol.SignalKindAnswer, answer); err != nil {
		t.Fatalf("sender SendSignal: %v", err)
	}
	select {
	case back := <-r.Channel().Signals():
		if back.Kind != protocol.SignalKindAnswer || !bytes.Equal(back.Payload, answer) {
			t.Fatalf("receiver got signal %+v", back)
		}
	case <-time.After(waitTimeout):
		t.Fatal("timeout waiting for signal at receiver")
	}
}

// shareId 已有活跃发送端时,第二个注册应得到明确的 collision 否决(D13)。
func TestShareIDCollision(t *testing.T) {
	ts := startRelay(t)
	ctx := testCtx(t)
	s, err := DialSender(ctx, senderCfg(ts.URL))
	if err != nil {
		t.Fatalf("DialSender: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	second := senderCfg(ts.URL)
	second.ResumeToken = bytes.Repeat([]byte{0x24}, protocol.ResumeTokenBytes)
	_, err = DialSender(ctx, second)
	var serr *ServerError
	if !errors.As(err, &serr) || serr.Code != protocol.ErrCodeShareIDCollision {
		t.Fatalf("err = %v, want share_id_collision", err)
	}
}

// 发送端 Close:分享随进程消失,接收端得到 sender_gone(§6.9)。
func TestSenderCloseTerminatesReceiver(t *testing.T) {
	ts := startRelay(t)
	s, r := dialPair(t, ts)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	waitDone(t, r.Done(), "receiver conn death")
	var serr *ServerError
	if !errors.As(r.Err(), &serr) || serr.Code != protocol.ErrCodeSenderGone {
		t.Fatalf("receiver err = %v, want sender_gone", r.Err())
	}
	if s.Err() != nil {
		t.Fatalf("sender err after local close = %v, want nil", s.Err())
	}
}

func TestSendFrameValidation(t *testing.T) {
	ts := startRelay(t)
	ctx := testCtx(t)
	_, r := dialPair(t, ts)
	ch := r.Channel()

	if err := ch.Send(ctx, nil); err == nil {
		t.Fatal("empty frame must be rejected")
	}
	if err := ch.Send(ctx, make(session.Frame, session.MaxFrameSize+1)); err == nil {
		t.Fatal("oversize frame must be rejected")
	}
	if err := ch.Close(); err != nil {
		t.Fatalf("channel close: %v", err)
	}
	if err := ch.Send(ctx, session.Frame{1}); !errors.Is(err, ErrChannelClosed) {
		t.Fatalf("send on closed channel = %v, want ErrChannelClosed", err)
	}
}

func TestDialSenderValidation(t *testing.T) {
	ctx := testCtx(t)
	cfg := senderCfg("http://127.0.0.1:0")
	cfg.ResumeToken = []byte{1, 2}
	if _, err := DialSender(ctx, cfg); err == nil {
		t.Fatal("short resume token must be rejected")
	}
	cfg = senderCfg("http://127.0.0.1:0")
	cfg.SealedManifest = nil
	if _, err := DialSender(ctx, cfg); err == nil {
		t.Fatal("empty manifest must be rejected")
	}
	cfg = senderCfg("http://127.0.0.1:0")
	cfg.ShareID = "bad!id"
	if _, err := DialSender(ctx, cfg); err == nil {
		t.Fatal("invalid shareId must be rejected")
	}
}

// keepalive 回显被读循环吞掉,静默期后连接仍健康可用。
func TestKeepaliveKeepsConnectionHealthy(t *testing.T) {
	ts := startRelay(t)
	ctx := testCtx(t)
	s, r := dialPair(t, ts)

	time.Sleep(300 * time.Millisecond) // 数个 keepalive 周期的纯静默
	select {
	case <-s.Done():
		t.Fatalf("sender died during idle: %v", s.Err())
	case <-r.Done():
		t.Fatalf("receiver died during idle: %v", r.Err())
	default:
	}

	req, _ := session.EncodeRequest([]uint64{3})
	if err := r.Channel().Send(ctx, req); err != nil {
		t.Fatalf("send after idle: %v", err)
	}
	sess := awaitSession(t, s)
	m, err := session.Decode(recvFrame(t, sess))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if m.(*session.Request).Indices[0] != 3 {
		t.Fatal("request mismatch after idle period")
	}
}

// Close 幂等;全部连接关闭后本包 goroutine 全数退净。
func TestCloseIdempotentNoGoroutineLeak(t *testing.T) {
	before := runtime.NumGoroutine()

	ts := startRelay(t)
	ctx := testCtx(t)
	s, err := DialSender(ctx, senderCfg(ts.URL))
	if err != nil {
		t.Fatalf("DialSender: %v", err)
	}
	r, err := DialReceiver(ctx, receiverCfg(ts.URL))
	if err != nil {
		t.Fatalf("DialReceiver: %v", err)
	}
	req, _ := session.EncodeRequest([]uint64{1})
	if err := r.Channel().Send(ctx, req); err != nil {
		t.Fatalf("send: %v", err)
	}
	sess := awaitSession(t, s)
	_ = recvFrame(t, sess)

	if err := r.Close(); err != nil {
		t.Fatalf("receiver close: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("receiver double close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("sender close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("sender double close: %v", err)
	}
	waitDone(t, s.Done(), "sender teardown")
	waitDone(t, r.Done(), "receiver teardown")
	if _, ok := <-s.Sessions(); ok {
		t.Fatal("Sessions must be closed after conn teardown")
	}

	// 服务器句柄由 t.Cleanup 收;这里只验证客户端侧不滞留 goroutine。
	// 服务端 ServeConn/泵在连接关闭后同样退出,余量吸收 httptest 的暂态。
	waitUntil(t, func() bool { return runtime.NumGoroutine() <= before+2 },
		"goroutines to settle after close")
}
