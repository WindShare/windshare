package signaling_test

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/windshare/windshare/relay/admission"
	"github.com/windshare/windshare/relay/forward"
	"github.com/windshare/windshare/relay/httpapi"
	"github.com/windshare/windshare/relay/protocol"
	"github.com/windshare/windshare/relay/signaling"
)

// testGrace 是测试用宽限期:长到期内动作从容完成,短到期满测试不磨叽。
const testGrace = 300 * time.Millisecond

func TestSenderGoneNotifiesReceiverThenResume(t *testing.T) {
	ts, _ := startRelay(t, signaling.Config{SenderReconnectGrace: time.Minute}, httpapi.Config{})
	sealed := randomManifest(t, 256)
	sender := register(t, ts, shareA, testToken, sealed)
	recv, _, _ := join(t, ts, shareA)

	_ = sender.ws.CloseNow()
	// 接收会话随发送端断开终结,接收端被告知后退避 rejoin(§6.8)。
	recv.expectError(protocol.ErrCodeSenderGone)
	recv.expectClosed()

	// 挂起期内 join 视同无分享。
	c := dial(t, ts, shareA)
	c.send(protocol.NewJoin(shareA))
	if _, ok := c.readMsg().(*protocol.NotFound); !ok {
		t.Fatal("join during suspension should receive not_found")
	}

	// 凭正确 token 原像 + 字节一致清单重注册恢复。
	c2 := dial(t, ts, shareA)
	c2.send(protocol.NewResumeRegister(shareA, protocol.HashResumeToken(testToken), protocol.EncodeResumeToken(testToken)))
	c2.sendRaw(websocket.MessageBinary, protocol.EncodeManifestFrame(sealed))
	if _, ok := c2.readMsg().(*protocol.Registered); !ok {
		t.Fatal("re-registration during the grace period should succeed")
	}
	// 恢复后新 join 正常供清单。
	_, _, got := join(t, ts, shareA)
	if !bytes.Equal(got, sealed) {
		t.Fatal("manifest bytes differ after resume")
	}
}

func TestResumeRejectedOnBadToken(t *testing.T) {
	ts, _ := startRelay(t, signaling.Config{SenderReconnectGrace: time.Minute}, httpapi.Config{})
	sealed := randomManifest(t, 128)
	sender := register(t, ts, shareA, testToken, sealed)
	_ = sender.ws.CloseNow()

	// 错误原像:持链者能拿到清单字节,但没有 token 原像,抢注被拒(§6.8)。
	wrong := bytes.Repeat([]byte{0x77}, protocol.ResumeTokenBytes)
	c := dial(t, ts, shareA)
	c.send(protocol.NewResumeRegister(shareA, protocol.HashResumeToken(testToken), protocol.EncodeResumeToken(wrong)))
	c.sendRaw(websocket.MessageBinary, protocol.EncodeManifestFrame(sealed))
	c.expectError(protocol.ErrCodeResumeRejected)
	c.expectClosed()

	// 不出示原像的普通 register 同样被拒。
	c2 := dial(t, ts, shareA)
	c2.send(protocol.NewRegister(shareA, protocol.HashResumeToken(wrong)))
	c2.sendRaw(websocket.MessageBinary, protocol.EncodeManifestFrame(sealed))
	c2.expectError(protocol.ErrCodeResumeRejected)
}

func TestResumeRejectedOnManifestMismatch(t *testing.T) {
	ts, _ := startRelay(t, signaling.Config{SenderReconnectGrace: time.Minute}, httpapi.Config{})
	sealed := randomManifest(t, 128)
	sender := register(t, ts, shareA, testToken, sealed)
	_ = sender.ws.CloseNow()

	c := dial(t, ts, shareA)
	c.send(protocol.NewResumeRegister(shareA, protocol.HashResumeToken(testToken), protocol.EncodeResumeToken(testToken)))
	c.sendRaw(websocket.MessageBinary, protocol.EncodeManifestFrame(randomManifest(t, 128)))
	c.expectError(protocol.ErrCodeResumeRejected)
	c.expectClosed()
}

func TestGraceExpiryReclaimsShare(t *testing.T) {
	ts, _ := startRelay(t, signaling.Config{SenderReconnectGrace: testGrace}, httpapi.Config{})
	sender := register(t, ts, shareA, testToken, randomManifest(t, 128))
	_ = sender.ws.CloseNow()

	// 期满后 shareId 被回收:全新 token 的普通 register 可以入驻。
	// 轮询而非精确掐点——测试只关心"终将回收",不关心毫秒级时刻。
	freshToken := bytes.Repeat([]byte{0x11}, protocol.ResumeTokenBytes)
	deadline := time.Now().Add(10 * testGrace)
	for {
		c := dial(t, ts, shareA)
		c.send(protocol.NewRegister(shareA, protocol.HashResumeToken(freshToken)))
		c.sendRaw(websocket.MessageBinary, protocol.EncodeManifestFrame([]byte("fresh")))
		m := c.readMsg()
		if _, ok := m.(*protocol.Registered); ok {
			return
		}
		if e, ok := m.(*protocol.Error); !ok || e.Code != protocol.ErrCodeResumeRejected {
			t.Fatalf("unexpected response: %+v", m)
		}
		_ = c.ws.CloseNow()
		if time.Now().After(deadline) {
			t.Fatal("share was not reclaimed after the grace period")
		}
		time.Sleep(testGrace / 4)
	}
}

func TestManifestTooLargeRejected(t *testing.T) {
	ts, _ := startRelay(t, signaling.Config{MaxManifestSize: 1024}, httpapi.Config{})
	c := dial(t, ts, shareA)
	c.send(protocol.NewRegister(shareA, protocol.HashResumeToken(testToken)))
	c.sendRaw(websocket.MessageBinary, protocol.EncodeManifestFrame(randomManifest(t, 2048)))
	c.expectError(protocol.ErrCodeManifestTooLarge)
	c.expectClosed()
}

func TestManifestBudgetRejectsNewRegister(t *testing.T) {
	cfg := admission.DefaultConfig()
	cfg.MaxTotalManifestBytes = 1500
	cfg.MaxManifestBytesPerSource = 1500
	controller, err := admission.NewController(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts, _ := startRelay(t, signaling.Config{Admission: controller}, httpapi.Config{})
	register(t, ts, shareA, testToken, randomManifest(t, 1024))

	c := dial(t, ts, shareB)
	c.send(protocol.NewRegister(shareB, protocol.HashResumeToken(testToken)))
	c.sendRaw(websocket.MessageBinary, protocol.EncodeManifestFrame(randomManifest(t, 1024)))
	c.expectError(protocol.ErrCodeManifestBudget)
	c.expectClosed()
}

func TestBackpressureKillsSlowSessionOnly(t *testing.T) {
	// 4 帧的迷你队列 + 停止读取的接收端:溢出必然发生;另一会话须不受波及。
	ts, _ := startRelay(t, signaling.Config{
		Pump: forward.Options{SessionQueueFrames: 4},
	}, httpapi.Config{})
	sender := register(t, ts, shareA, testToken, randomManifest(t, 64))
	slow, slowSidStr, _ := join(t, ts, shareA)
	good, goodSidStr, _ := join(t, ts, shareA)
	slowSid, _ := protocol.ParseSessionID(slowSidStr)
	goodSid, _ := protocol.ParseSessionID(goodSidStr)

	// slow 停读(不再调用 Read),灌大帧直到其队列(4)+ TCP 缓冲填满。
	// 发送端并发收听溢出通报——中转始终及时读发送端 WS,写不会反压回来。
	frame := protocol.EncodeForwardFrame(slowSid, bytes.Repeat([]byte{0xcd}, 16*1024))
	type overflowRead struct {
		terminal *protocol.Error
		err      error
	}
	overflow := make(chan overflowRead, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), ioTimeout)
		defer cancel()
		for {
			typ, data, err := sender.ws.Read(ctx)
			if err != nil {
				overflow <- overflowRead{err: err}
				return
			}
			if typ != websocket.MessageText {
				overflow <- overflowRead{err: fmt.Errorf("expected text terminal, got message type %v", typ)}
				return
			}
			m, err := protocol.Decode(data)
			if err != nil {
				overflow <- overflowRead{err: fmt.Errorf("decode terminal: %w", err)}
				return
			}
			if e, ok := m.(*protocol.Error); ok && e.Code == protocol.ErrCodeSessionOverflow {
				overflow <- overflowRead{terminal: e}
				return
			}
		}
	}()
	deadline := time.After(ioTimeout)
	sent := 0
loop:
	for {
		select {
		case result := <-overflow:
			if result.err != nil {
				t.Fatalf("waiting for overflow report: %v", result.err)
			}
			e := result.terminal
			if e.SessionID != slowSidStr {
				t.Fatalf("overflow report targets %s, want %s", e.SessionID, slowSidStr)
			}
			break loop
		case <-deadline:
			t.Fatalf("overflow was not triggered after %d frames", sent)
		default:
			sender.sendRaw(websocket.MessageBinary, frame)
			sent++
		}
	}

	// 慢会话被断,健康会话照常收帧(逐帧回声验证通路)。
	goodFrame := protocol.EncodeForwardFrame(goodSid, []byte("still-alive"))
	sender.sendRaw(websocket.MessageBinary, goodFrame)
	if got := good.readBinary(); !bytes.Equal(got, goodFrame) {
		t.Fatal("healthy session was terminated by the slow session")
	}
	slow.expectClosed()
}

func TestJoinRateLimited(t *testing.T) {
	cfg := admission.DefaultConfig()
	cfg.JoinPerShare = admission.Rate{PerSecond: 0.0001, Burst: 2}
	cfg.JoinPerSource = admission.Rate{PerSecond: 0.0001, Burst: 100}
	controller, err := admission.NewController(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts, _ := startRelay(t, signaling.Config{Admission: controller}, httpapi.Config{})
	register(t, ts, shareA, testToken, randomManifest(t, 64))

	for range 2 {
		c, _, _ := join(t, ts, shareA)
		_ = c.ws.CloseNow()
	}
	c := dial(t, ts, shareA)
	c.send(protocol.NewJoin(shareA))
	c.expectError(protocol.ErrCodeRateLimited)
	c.expectClosed()
}

func TestOversizeForwardFrameRejected(t *testing.T) {
	ts, _ := startRelay(t, signaling.Config{MaxFrameSize: 1024}, httpapi.Config{})
	sender := register(t, ts, shareA, testToken, randomManifest(t, 64))
	recv, sidStr, _ := join(t, ts, shareA)
	sid, _ := protocol.ParseSessionID(sidStr)

	recv.sendRaw(websocket.MessageBinary, protocol.EncodeForwardFrame(sid, bytes.Repeat([]byte{1}, 2048)))
	recv.expectError(protocol.ErrCodeFrameTooLarge)
	recv.expectClosed()
	// 发送端同规。
	sender.sendRaw(websocket.MessageBinary, protocol.EncodeForwardFrame(sid, bytes.Repeat([]byte{1}, 2048)))
	sender.expectError(protocol.ErrCodeFrameTooLarge)
	sender.expectClosed()
}

func TestReceiverCannotSpoofOtherSession(t *testing.T) {
	ts, _ := startRelay(t, signaling.Config{}, httpapi.Config{})
	register(t, ts, shareA, testToken, randomManifest(t, 64))
	r1, _, _ := join(t, ts, shareA)
	_, sid2Str, _ := join(t, ts, shareA)
	sid2, _ := protocol.ParseSessionID(sid2Str)

	// r1 冒用 r2 的 sessionId 发转发帧:sessionId 是中转分配的路由凭据。
	r1.sendRaw(websocket.MessageBinary, protocol.EncodeForwardFrame(sid2, []byte{1}))
	r1.expectError(protocol.ErrCodeUnknownSession)
	r1.expectClosed()
}

func TestHubCloseDisconnectsEveryone(t *testing.T) {
	ts, hub := startRelay(t, signaling.Config{}, httpapi.Config{})
	sender := register(t, ts, shareA, testToken, randomManifest(t, 64))
	recv, _, _ := join(t, ts, shareA)

	hub.Close()
	sender.expectClosed()
	recv.expectClosed()
}
