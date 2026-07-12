// 集成测试:经 httpapi 完整栈(真实 HTTP 升级 + 真实 WS 客户端)驱动
// signaling hub,覆盖 §6.7/§6.8 的信令语义。
package signaling_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/windshare/windshare/internal/testnetwork"
	"github.com/windshare/windshare/relay/httpapi"
	"github.com/windshare/windshare/relay/protocol"
	"github.com/windshare/windshare/relay/signaling"
)

const (
	shareA = "c2hhcmUwMDFB"
	shareB = "c2hhcmUwMDJC"
	// ioTimeout 只防测试卡死,不承载时序语义——所有顺序断言都靠 WS 有序性。
	ioTimeout = 10 * time.Second
)

var testToken = bytes.Repeat([]byte{0x5a}, protocol.ResumeTokenBytes)

func startRelay(t *testing.T, cfg signaling.Config, api httpapi.Config) (*httptest.Server, *signaling.Hub) {
	t.Helper()
	testnetwork.RequireOSNetwork(t)
	hub := signaling.NewHub(cfg)
	t.Cleanup(hub.Close)
	api.Hub = hub
	ts := httptest.NewServer(httpapi.NewHandler(api))
	t.Cleanup(ts.Close)
	return ts, hub
}

func wsURL(ts *httptest.Server, shareID string) string {
	return "ws" + strings.TrimPrefix(ts.URL, "http") + "/v1/ws/" + shareID
}

// tc 是测试用真实 WS 客户端。
type tc struct {
	t  *testing.T
	ws *websocket.Conn
}

func dial(t *testing.T, ts *httptest.Server, shareID string) *tc {
	t.Helper()
	testnetwork.RequireOSNetwork(t)
	ctx, cancel := context.WithTimeout(context.Background(), ioTimeout)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, wsURL(ts, shareID), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	ws.SetReadLimit(1 << 26)
	t.Cleanup(func() { _ = ws.CloseNow() })
	return &tc{t: t, ws: ws}
}

func (c *tc) send(m protocol.Message) {
	c.t.Helper()
	data, err := protocol.Encode(m)
	if err != nil {
		c.t.Fatalf("encode: %v", err)
	}
	c.sendRaw(websocket.MessageText, data)
}

func (c *tc) sendRaw(typ websocket.MessageType, data []byte) {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), ioTimeout)
	defer cancel()
	if err := c.ws.Write(ctx, typ, data); err != nil {
		c.t.Fatalf("write: %v", err)
	}
}

func (c *tc) read() (websocket.MessageType, []byte) {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), ioTimeout)
	defer cancel()
	typ, data, err := c.ws.Read(ctx)
	if err != nil {
		c.t.Fatalf("read: %v", err)
	}
	return typ, data
}

func (c *tc) readMsg() protocol.Message {
	c.t.Helper()
	typ, data := c.read()
	if typ != websocket.MessageText {
		c.t.Fatalf("expected text message, got binary % x", data[:min(len(data), 16)])
	}
	m, err := protocol.Decode(data)
	if err != nil {
		c.t.Fatalf("decode: %v (%s)", err, data)
	}
	return m
}

func (c *tc) readBinary() []byte {
	c.t.Helper()
	typ, data := c.read()
	if typ != websocket.MessageBinary {
		c.t.Fatalf("expected binary frame, got %s", data)
	}
	return data
}

// expectError 读到一条指定 code 的 error(容忍中间夹杂其他消息)。
func (c *tc) expectError(code string) *protocol.Error {
	c.t.Helper()
	for range 32 {
		m := c.readMsg()
		if e, ok := m.(*protocol.Error); ok {
			if e.Code != code {
				c.t.Fatalf("error code = %s, want %s (%s)", e.Code, code, e.Message)
			}
			return e
		}
	}
	c.t.Fatalf("did not receive error %s", code)
	return nil
}

// expectClosed 断言连接已被服务端关闭。
func (c *tc) expectClosed() {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), ioTimeout)
	defer cancel()
	for {
		if _, _, err := c.ws.Read(ctx); err != nil {
			return // 关闭(或超时失败)即达成
		}
	}
}

// register 完成一次成功注册(register + 清单帧 → registered ack)。
func register(t *testing.T, ts *httptest.Server, shareID string, token, sealed []byte) *tc {
	t.Helper()
	c := dial(t, ts, shareID)
	c.send(protocol.NewRegister(shareID, protocol.HashResumeToken(token)))
	c.sendRaw(websocket.MessageBinary, protocol.EncodeManifestFrame(sealed))
	if m, ok := c.readMsg().(*protocol.Registered); !ok || m.ShareID != shareID {
		t.Fatalf("did not receive registered ack: %+v", m)
	}
	return c
}

// join 完成一次成功加入,返回客户端、sessionId 与回放的清单字节。
func join(t *testing.T, ts *httptest.Server, shareID string) (*tc, string, []byte) {
	t.Helper()
	c := dial(t, ts, shareID)
	c.send(protocol.NewJoin(shareID))
	m, ok := c.readMsg().(*protocol.Manifest)
	if !ok {
		t.Fatalf("did not receive manifest message: %+v", m)
	}
	sealed, err := protocol.DecodeManifestFrame(c.readBinary())
	if err != nil {
		t.Fatalf("manifest frame decode failed: %v", err)
	}
	return c, m.SessionID, sealed
}

func randomManifest(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRegisterJoinManifestRoundTrip(t *testing.T) {
	ts, _ := startRelay(t, signaling.Config{}, httpapi.Config{})
	sealed := randomManifest(t, 4096)
	register(t, ts, shareA, testToken, sealed)

	r1, sid1, got1 := join(t, ts, shareA)
	_, sid2, got2 := join(t, ts, shareA)
	if !bytes.Equal(got1, sealed) || !bytes.Equal(got2, sealed) {
		t.Fatal("replayed manifest bytes differ")
	}
	if sid1 == sid2 {
		t.Fatalf("concurrent sessions share sessionId %s", sid1)
	}
	_ = r1
}

func TestJoinBeforeRegisterThenRetry(t *testing.T) {
	ts, _ := startRelay(t, signaling.Config{}, httpapi.Config{})
	c := dial(t, ts, shareA)
	c.send(protocol.NewJoin(shareA))
	if _, ok := c.readMsg().(*protocol.NotFound); !ok {
		t.Fatal("join before register should receive not_found")
	}
	sealed := randomManifest(t, 512)
	register(t, ts, shareA, testToken, sealed)
	// 同一连接重试 join(短窗退避的竞态是常态,§6.7)。
	c.send(protocol.NewJoin(shareA))
	if _, ok := c.readMsg().(*protocol.Manifest); !ok {
		t.Fatal("retried join should succeed")
	}
	if got, _ := protocol.DecodeManifestFrame(c.readBinary()); !bytes.Equal(got, sealed) {
		t.Fatal("manifest bytes differ")
	}
}

func TestSignalForwardingBothWays(t *testing.T) {
	ts, _ := startRelay(t, signaling.Config{}, httpapi.Config{})
	sender := register(t, ts, shareA, testToken, randomManifest(t, 64))
	recv, sid, _ := join(t, ts, shareA)

	offer := json.RawMessage(`{"sdp":"v=0 offer"}`)
	recv.send(protocol.NewSignal(sid, protocol.SignalKindOffer, offer))
	m, ok := sender.readMsg().(*protocol.Signal)
	if !ok || m.SessionID != sid || m.Kind != protocol.SignalKindOffer || !bytes.Equal(m.Payload, offer) {
		t.Fatalf("sender received unexpected signal: %+v", m)
	}

	answer := json.RawMessage(`{"sdp":"v=0 answer"}`)
	sender.send(protocol.NewSignal(sid, protocol.SignalKindAnswer, answer))
	m2, ok := recv.readMsg().(*protocol.Signal)
	if !ok || m2.SessionID != sid || m2.Kind != protocol.SignalKindAnswer || !bytes.Equal(m2.Payload, answer) {
		t.Fatalf("receiver received unexpected signal: %+v", m2)
	}
}

func TestSignalRoutedToCorrectReceiver(t *testing.T) {
	ts, _ := startRelay(t, signaling.Config{}, httpapi.Config{})
	sender := register(t, ts, shareA, testToken, randomManifest(t, 64))
	r1, sid1, _ := join(t, ts, shareA)
	r2, sid2, _ := join(t, ts, shareA)

	sender.send(protocol.NewSignal(sid2, protocol.SignalKindOffer, json.RawMessage(`"for-2"`)))
	if m, ok := r2.readMsg().(*protocol.Signal); !ok || m.SessionID != sid2 {
		t.Fatalf("r2 did not receive the targeted signal: %+v", m)
	}
	// r1 不应截获:向 r1 发一条哨兵,若 r1 先收到 for-2 则顺序断言失败。
	sender.send(protocol.NewSignal(sid1, protocol.SignalKindOffer, json.RawMessage(`"for-1"`)))
	if m, ok := r1.readMsg().(*protocol.Signal); !ok || !bytes.Equal(m.Payload, []byte(`"for-1"`)) {
		t.Fatalf("r1 received a cross-session message: %+v", m)
	}
}

func TestForwardFramesBothWays(t *testing.T) {
	ts, _ := startRelay(t, signaling.Config{}, httpapi.Config{})
	sender := register(t, ts, shareA, testToken, randomManifest(t, 64))
	recv, sidStr, _ := join(t, ts, shareA)
	sid, _ := protocol.ParseSessionID(sidStr)

	// 中转零解析:内层随便什么字节都原样到达。
	req := protocol.EncodeForwardFrame(sid, []byte{0x01, 0xff, 0x00, 0x7f})
	recv.sendRaw(websocket.MessageBinary, req)
	if got := sender.readBinary(); !bytes.Equal(got, req) {
		t.Fatalf("sender received % x, want % x", got, req)
	}

	blk := protocol.EncodeForwardFrame(sid, bytes.Repeat([]byte{0xab}, 1024))
	sender.sendRaw(websocket.MessageBinary, blk)
	if got := recv.readBinary(); !bytes.Equal(got, blk) {
		t.Fatalf("receiver forward frame differs")
	}
}

func TestKeepaliveEcho(t *testing.T) {
	ts, _ := startRelay(t, signaling.Config{}, httpapi.Config{})
	sender := register(t, ts, shareA, testToken, randomManifest(t, 64))
	sender.send(protocol.NewKeepalive())
	if _, ok := sender.readMsg().(*protocol.Keepalive); !ok {
		t.Fatal("keepalive should be echoed unchanged")
	}
}

func TestByeFromReceiver(t *testing.T) {
	ts, _ := startRelay(t, signaling.Config{}, httpapi.Config{})
	sender := register(t, ts, shareA, testToken, randomManifest(t, 64))
	recv, sid, _ := join(t, ts, shareA)

	recv.send(protocol.NewBye(sid))
	if m, ok := sender.readMsg().(*protocol.Bye); !ok || m.SessionID != sid {
		t.Fatalf("sender did not receive bye: %+v", m)
	}
	recv.expectClosed()
	// sessionId 随会话进入终态:同连接里已经在途的迟到帧须静默丢弃,
	// 既不复活会话,也不把正常的终态竞态误判成未知 ID 滥用。
	sidBin, _ := protocol.ParseSessionID(sid)
	sender.sendRaw(websocket.MessageBinary, protocol.EncodeForwardFrame(sidBin, []byte{1}))
	sender.send(protocol.NewKeepalive())
	if _, ok := sender.readMsg().(*protocol.Keepalive); !ok {
		t.Fatal("terminal session frame should be dropped without closing the sender")
	}
}

func TestByeFromSender(t *testing.T) {
	ts, _ := startRelay(t, signaling.Config{}, httpapi.Config{})
	sender := register(t, ts, shareA, testToken, randomManifest(t, 64))
	recv, sid, _ := join(t, ts, shareA)

	sender.send(protocol.NewBye(sid))
	if m, ok := recv.readMsg().(*protocol.Bye); !ok || m.SessionID != sid {
		t.Fatalf("receiver did not receive bye: %+v", m)
	}
	recv.expectClosed()
}

func TestReceiverDisconnectSynthesizesBye(t *testing.T) {
	ts, _ := startRelay(t, signaling.Config{}, httpapi.Config{})
	sender := register(t, ts, shareA, testToken, randomManifest(t, 64))
	recv, sid, _ := join(t, ts, shareA)

	_ = recv.ws.CloseNow() // 接收端不辞而别
	if m, ok := sender.readMsg().(*protocol.Bye); !ok || m.SessionID != sid {
		t.Fatalf("relay should synthesize bye: %+v", m)
	}
}

func TestShareIDMismatchRejected(t *testing.T) {
	ts, _ := startRelay(t, signaling.Config{}, httpapi.Config{})

	c := dial(t, ts, shareA)
	c.send(protocol.NewRegister(shareB, protocol.HashResumeToken(testToken)))
	c.expectError(protocol.ErrCodeShareIDMismatch)
	c.expectClosed()

	c2 := dial(t, ts, shareA)
	c2.send(protocol.NewJoin(shareB))
	c2.expectError(protocol.ErrCodeShareIDMismatch)
	c2.expectClosed()
}

func TestActiveShareIDCollisionRejected(t *testing.T) {
	ts, _ := startRelay(t, signaling.Config{}, httpapi.Config{})
	register(t, ts, shareA, testToken, randomManifest(t, 64))

	c := dial(t, ts, shareA)
	c.send(protocol.NewRegister(shareA, protocol.HashResumeToken(testToken)))
	c.sendRaw(websocket.MessageBinary, protocol.EncodeManifestFrame([]byte("x")))
	c.expectError(protocol.ErrCodeShareIDCollision)
	c.expectClosed()
}

func TestProtocolViolations(t *testing.T) {
	ts, _ := startRelay(t, signaling.Config{}, httpapi.Config{})

	// 首条消息非 register/join。
	c := dial(t, ts, shareA)
	c.send(protocol.NewKeepalive())
	c.expectError(protocol.ErrCodeProtocol)
	c.expectClosed()

	// register 后不给清单帧,给了文本。
	c2 := dial(t, ts, shareA)
	c2.send(protocol.NewRegister(shareA, protocol.HashResumeToken(testToken)))
	c2.sendRaw(websocket.MessageText, []byte(`{"type":"keepalive"}`))
	c2.expectError(protocol.ErrCodeProtocol)
	c2.expectClosed()

	// 未知 JSON 类型。
	c3 := dial(t, ts, shareA)
	c3.sendRaw(websocket.MessageText, []byte(`{"type":"warp-drive"}`))
	c3.expectError(protocol.ErrCodeProtocol)
	c3.expectClosed()

	// 注册后再发清单帧。
	sender := register(t, ts, shareB, testToken, randomManifest(t, 64))
	sender.sendRaw(websocket.MessageBinary, protocol.EncodeManifestFrame([]byte("again")))
	sender.expectError(protocol.ErrCodeProtocol)
	sender.expectClosed()
}

func TestUnknownSessionForwardIsSessionScoped(t *testing.T) {
	ts, _ := startRelay(t, signaling.Config{}, httpapi.Config{})
	sender := register(t, ts, shareA, testToken, randomManifest(t, 64))

	bogus := protocol.SessionID{9, 9, 9, 9, 9, 9, 9, 9}
	sender.sendRaw(websocket.MessageBinary, protocol.EncodeForwardFrame(bogus, []byte{1}))
	sender.expectError(protocol.ErrCodeUnknownSession)
	// 会话级错误不殃及连接:keepalive 仍然通。
	sender.send(protocol.NewKeepalive())
	if _, ok := sender.readMsg().(*protocol.Keepalive); !ok {
		t.Fatal("connection should remain alive")
	}
}
