package signaling_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/windshare/windshare/internal/testnetwork"
	"github.com/windshare/windshare/relay/httpapi"
	"github.com/windshare/windshare/relay/protocol"
	"github.com/windshare/windshare/relay/signaling"
)

func TestSignalToUnknownSessionIsSessionScoped(t *testing.T) {
	ts, _ := startRelay(t, signaling.Config{}, httpapi.Config{})
	sender := register(t, ts, shareA, testToken, randomManifest(t, 64))

	bogus := protocol.SessionID{7, 7, 7, 7, 7, 7, 7, 7}
	sender.send(protocol.NewSignal(bogus.String(), protocol.SignalKindOffer, json.RawMessage(`{}`)))
	sender.expectError(protocol.ErrCodeUnknownSession)
	sender.send(protocol.NewKeepalive())
	if _, ok := sender.readMsg().(*protocol.Keepalive); !ok {
		t.Fatal("session-scoped error should not terminate the connection")
	}
}

func TestUnknownSessionFloodClosesConnection(t *testing.T) {
	ts, _ := startRelay(t, signaling.Config{}, httpapi.Config{})
	sender := register(t, ts, shareA, testToken, randomManifest(t, 64))
	for i := byte(1); i < 64; i++ {
		bogus := protocol.SessionID{i}
		sender.send(protocol.NewSignal(bogus.String(), protocol.SignalKindOffer, json.RawMessage(`{}`)))
		message := sender.readMsg()
		relayErr, ok := message.(*protocol.Error)
		if !ok {
			t.Fatalf("response %d = %T", i, message)
		}
		switch relayErr.Code {
		case protocol.ErrCodeUnknownSession:
			continue
		case protocol.ErrCodeProtocol:
			sender.expectClosed()
			return
		default:
			t.Fatalf("response %d code = %s", i, relayErr.Code)
		}
	}
	t.Fatal("unknown-session flood never reached its connection bound")
}

func TestClientDiagnosticFloodClosesConnection(t *testing.T) {
	ts, _ := startRelay(t, signaling.Config{}, httpapi.Config{})
	sender := register(t, ts, shareA, testToken, randomManifest(t, 64))
	const acceptedReports = 8
	for range acceptedReports {
		sender.send(protocol.NewError(protocol.ErrCodeProtocol, "client diagnostic"))
	}
	sender.send(protocol.NewKeepalive())
	if _, ok := sender.readMsg().(*protocol.Keepalive); !ok {
		t.Fatal("bounded diagnostics unexpectedly closed the connection")
	}
	sender.send(protocol.NewError(protocol.ErrCodeProtocol, "one too many"))
	sender.expectError(protocol.ErrCodeProtocol)
	sender.expectClosed()
}

func TestReceiverSignalSpoofRejected(t *testing.T) {
	ts, _ := startRelay(t, signaling.Config{}, httpapi.Config{})
	register(t, ts, shareA, testToken, randomManifest(t, 64))
	r1, _, _ := join(t, ts, shareA)
	_, sid2, _ := join(t, ts, shareA)

	// 接收端只能以自己的 sessionId 说话:signal 冒用与转发帧同罪。
	r1.send(protocol.NewSignal(sid2, protocol.SignalKindOffer, json.RawMessage(`{}`)))
	r1.expectError(protocol.ErrCodeUnknownSession)
	r1.expectClosed()
}

func TestByeWrongSessionRejected(t *testing.T) {
	ts, _ := startRelay(t, signaling.Config{}, httpapi.Config{})
	register(t, ts, shareA, testToken, randomManifest(t, 64))
	r1, _, _ := join(t, ts, shareA)

	bogus := protocol.SessionID{6, 6, 6, 6, 6, 6, 6, 6}
	r1.send(protocol.NewBye(bogus.String()))
	r1.expectError(protocol.ErrCodeUnknownSession)
	r1.expectClosed()
}

func TestByeUnknownSessionFromSenderIgnored(t *testing.T) {
	ts, _ := startRelay(t, signaling.Config{}, httpapi.Config{})
	sender := register(t, ts, shareA, testToken, randomManifest(t, 64))

	bogus := protocol.SessionID{5, 5, 5, 5, 5, 5, 5, 5}
	sender.send(protocol.NewBye(bogus.String()))
	// 会话可能刚被对端正常终结,发送端的 bye 迟到是良性竞态:静默忽略。
	sender.send(protocol.NewKeepalive())
	if _, ok := sender.readMsg().(*protocol.Keepalive); !ok {
		t.Fatal("late bye should not cause an error")
	}
}

func TestClientErrorReportsTolerated(t *testing.T) {
	ts, _ := startRelay(t, signaling.Config{}, httpapi.Config{})
	sender := register(t, ts, shareA, testToken, randomManifest(t, 64))
	recv, _, _ := join(t, ts, shareA)

	// 客户端的 error 是诊断通报,不定义中转侧行为:记录并继续服务。
	sender.send(protocol.NewError("client_diag", "sender side note"))
	recv.send(protocol.NewError("client_diag", "receiver side note"))
	sender.send(protocol.NewKeepalive())
	if _, ok := sender.readMsg().(*protocol.Keepalive); !ok {
		t.Fatal("sender should remain alive")
	}
	recv.send(protocol.NewKeepalive())
	if _, ok := recv.readMsg().(*protocol.Keepalive); !ok {
		t.Fatal("receiver should remain alive")
	}
}

func TestRoleForbiddenMessages(t *testing.T) {
	ts, _ := startRelay(t, signaling.Config{}, httpapi.Config{})

	// 发送端不得 join。
	sender := register(t, ts, shareA, testToken, randomManifest(t, 64))
	sender.send(protocol.NewJoin(shareA))
	sender.expectError(protocol.ErrCodeProtocol)
	sender.expectClosed()

	// 接收端不得 register(会话内)。
	s2 := register(t, ts, shareB, testToken, randomManifest(t, 64))
	defer func() { _ = s2.ws.CloseNow() }()
	recv, _, _ := join(t, ts, shareB)
	recv.send(protocol.NewRegister(shareB, protocol.HashResumeToken(testToken)))
	recv.expectError(protocol.ErrCodeProtocol)
	recv.expectClosed()
}

func TestPreSessionReceiverRules(t *testing.T) {
	ts, _ := startRelay(t, signaling.Config{}, httpapi.Config{})

	// not_found 之后:keepalive 可用,二进制帧非法。
	c := dial(t, ts, shareA)
	c.send(protocol.NewJoin(shareA))
	if _, ok := c.readMsg().(*protocol.NotFound); !ok {
		t.Fatal("expected not_found")
	}
	c.send(protocol.NewKeepalive())
	if _, ok := c.readMsg().(*protocol.Keepalive); !ok {
		t.Fatal("pre-session keepalive should be echoed")
	}
	c.sendRaw(websocket.MessageBinary, []byte{protocol.BinTypeForward, 0, 0, 0, 0, 0, 0, 0, 0})
	c.expectError(protocol.ErrCodeProtocol)
	c.expectClosed()

	// not_found 之后发 signal(尚无会话)→ 协议错误。
	c2 := dial(t, ts, shareA)
	c2.send(protocol.NewJoin(shareA))
	if _, ok := c2.readMsg().(*protocol.NotFound); !ok {
		t.Fatal("expected not_found")
	}
	sid := protocol.SessionID{1}
	c2.send(protocol.NewSignal(sid.String(), protocol.SignalKindOffer, json.RawMessage(`{}`)))
	c2.expectError(protocol.ErrCodeProtocol)
	c2.expectClosed()
}

func TestFirstMessageMustBeText(t *testing.T) {
	ts, _ := startRelay(t, signaling.Config{}, httpapi.Config{})
	c := dial(t, ts, shareA)
	c.sendRaw(websocket.MessageBinary, protocol.EncodeManifestFrame([]byte("premature")))
	c.expectError(protocol.ErrCodeProtocol)
	c.expectClosed()
}

func TestDialAfterHubClosed(t *testing.T) {
	testnetwork.RequireOSNetwork(t)
	ts, hub := startRelay(t, signaling.Config{}, httpapi.Config{})
	hub.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/v1/ws/" + shareA
	ws, resp, err := websocket.Dial(ctx, url, nil)
	if ws != nil {
		_ = ws.CloseNow()
	}
	if err == nil || resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("closed hub accepted upgrade: err=%v response=%v", err, resp)
	}
}

func TestReceiverBinaryManifestInSessionRejected(t *testing.T) {
	ts, _ := startRelay(t, signaling.Config{}, httpapi.Config{})
	register(t, ts, shareA, testToken, randomManifest(t, 64))
	recv, _, _ := join(t, ts, shareA)

	recv.sendRaw(websocket.MessageBinary, protocol.EncodeManifestFrame([]byte("nope")))
	recv.expectError(protocol.ErrCodeProtocol)
	recv.expectClosed()
}
