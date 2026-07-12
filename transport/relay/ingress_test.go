package relay

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/relay/protocol"
)

func TestSenderFrameIngressOverflowIsSessionLocal(t *testing.T) {
	ts := startRelay(t)
	ctx := testCtx(t)
	sender, err := DialSender(ctx, senderCfg(ts.URL))
	if err != nil {
		t.Fatalf("DialSender: %v", err)
	}
	t.Cleanup(func() { _ = sender.Close() })

	slow := dialTestReceiver(t, ts.URL)
	if err := slow.Channel().Send(ctx, session.Frame{0x10}); err != nil {
		t.Fatalf("seed slow session: %v", err)
	}
	slowSession := awaitSession(t, sender)

	healthy := dialTestReceiver(t, ts.URL)
	healthySeed := session.Frame{0x20}
	if err := healthy.Channel().Send(ctx, healthySeed); err != nil {
		t.Fatalf("seed healthy session: %v", err)
	}
	healthySession := awaitSession(t, sender)
	if got := recvFrame(t, healthySession); !bytes.Equal(got, healthySeed) {
		t.Fatalf("healthy seed = %x, want %x", got, healthySeed)
	}

	// The seed occupies one ordinary slot. Filling the remaining ordinary
	// budget and sending one more frame must close only the slow session.
	for i := range recvBufferFrames {
		if err := slow.Channel().Send(ctx, session.Frame{byte(i + 1)}); err != nil {
			t.Fatalf("slow frame %d: %v", i, err)
		}
	}
	waitUntil(t, func() bool { return slowSession.State() == session.Closed }, "slow frame ingress overflow")
	assertIngressOverflow(t, slowSession.Err(), IngressFrames)

	healthyFrame := session.Frame{0x21, 0x22}
	if err := healthy.Channel().Send(ctx, healthyFrame); err != nil {
		t.Fatalf("healthy send after sibling overflow: %v", err)
	}
	if got := recvFrame(t, healthySession); !bytes.Equal(got, healthyFrame) {
		t.Fatalf("healthy frame = %x, want %x", got, healthyFrame)
	}
	assertSenderAlive(t, sender)
}

func TestSenderSignalIngressOverflowIsSessionLocal(t *testing.T) {
	ts := startRelay(t)
	ctx := testCtx(t)
	sender, err := DialSender(ctx, senderCfg(ts.URL))
	if err != nil {
		t.Fatalf("DialSender: %v", err)
	}
	t.Cleanup(func() { _ = sender.Close() })

	slow := dialTestReceiver(t, ts.URL)
	if err := slow.Channel().Send(ctx, session.Frame{0x30}); err != nil {
		t.Fatalf("seed slow session: %v", err)
	}
	slowSession := awaitSession(t, sender)
	_ = recvFrame(t, slowSession)

	healthy := dialTestReceiver(t, ts.URL)
	if err := healthy.Channel().Send(ctx, session.Frame{0x40}); err != nil {
		t.Fatalf("seed healthy session: %v", err)
	}
	healthySession := awaitSession(t, sender)
	_ = recvFrame(t, healthySession)

	for i := 0; i <= signalBuffer; i++ {
		payload := []byte("null")
		if err := slow.Channel().SendSignal(ctx, protocol.SignalKindCandidate, payload); err != nil {
			t.Fatalf("slow signal %d: %v", i, err)
		}
		if i < signalBuffer {
			want := i + 1
			waitUntil(t, func() bool { return len(slowSession.signalsCh) == want }, "slow signal ingress")
		}
	}
	waitUntil(t, func() bool { return slowSession.State() == session.Closed }, "slow signal ingress overflow")
	assertIngressOverflow(t, slowSession.Err(), IngressSignals)

	want := []byte(`{"candidate":"healthy"}`)
	if err := healthy.Channel().SendSignal(ctx, protocol.SignalKindCandidate, want); err != nil {
		t.Fatalf("healthy signal after sibling overflow: %v", err)
	}
	select {
	case got, ok := <-healthySession.Signals():
		if !ok {
			t.Fatal("healthy signal stream closed after sibling overflow")
		}
		if got.Kind != protocol.SignalKindCandidate || !bytes.Equal(got.Payload, want) {
			t.Fatalf("healthy signal = %+v", got)
		}
	case <-time.After(waitTimeout):
		t.Fatal("timeout waiting for healthy signal")
	}
	assertSenderAlive(t, sender)
}

func TestSenderSessionEventOverflowRejectsOnlyUnpublishedSession(t *testing.T) {
	ts := startRelay(t)
	ctx := testCtx(t)
	sender, err := DialSender(ctx, senderCfg(ts.URL))
	if err != nil {
		t.Fatalf("DialSender: %v", err)
	}
	t.Cleanup(func() { _ = sender.Close() })

	receivers := make([]*ReceiverConn, 0, sessionEventBuffer+2)
	for i := range sessionEventBuffer {
		receiver := dialTestReceiver(t, ts.URL)
		receivers = append(receivers, receiver)
		if err := receiver.Channel().Send(ctx, session.Frame{byte(i + 1)}); err != nil {
			t.Fatalf("seed buffered session %d: %v", i, err)
		}
		want := i + 1
		waitUntil(t, func() bool { return len(sender.sessionsCh) == want }, "buffered session event")
	}

	overflowed := dialTestReceiver(t, ts.URL)
	receivers = append(receivers, overflowed)
	if err := overflowed.Channel().Send(ctx, session.Frame{0xee}); err != nil {
		t.Fatalf("seed overflowed session: %v", err)
	}
	var overflowedChannel *Channel
	waitUntil(t, func() bool {
		sender.mu.Lock()
		defer sender.mu.Unlock()
		entry := sender.live[overflowed.SessionID()]
		if entry == nil || entry.state != clientSessionTerminal {
			return false
		}
		overflowedChannel = entry.ch
		return overflowedChannel != nil && overflowedChannel.State() == session.Closed
	}, "session event overflow tombstone")
	assertIngressOverflow(t, overflowedChannel.Err(), IngressSessionEvents)

	for range sessionEventBuffer {
		channel := awaitSession(t, sender)
		_ = recvFrame(t, channel)
	}

	healthy := dialTestReceiver(t, ts.URL)
	receivers = append(receivers, healthy)
	want := session.Frame{0xfa, 0xce}
	if err := healthy.Channel().Send(ctx, want); err != nil {
		t.Fatalf("seed post-overflow session: %v", err)
	}
	healthyChannel := awaitSession(t, sender)
	if got := recvFrame(t, healthyChannel); !bytes.Equal(got, want) {
		t.Fatalf("post-overflow frame = %x, want %x", got, want)
	}
	assertSenderAlive(t, sender)
}

func TestReceiverFrameIngressOverflowClosesOnlyItsSession(t *testing.T) {
	ts := startRelay(t)
	ctx := testCtx(t)
	sender, receiver := dialPair(t, ts)
	if err := receiver.Channel().Send(ctx, session.Frame{0x51}); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	senderSession := awaitSession(t, sender)
	_ = recvFrame(t, senderSession)

	for i := range recvBufferFrames {
		if err := senderSession.Send(ctx, session.Frame{byte(i + 1)}); err != nil {
			t.Fatalf("receiver-bound frame %d: %v", i, err)
		}
		want := i + 1
		waitUntil(t, func() bool { return len(receiver.Channel().recvCh) == want }, "receiver frame ingress")
	}
	if err := senderSession.Send(ctx, session.Frame{0xff}); err != nil && !errors.Is(err, ErrChannelClosed) {
		t.Fatalf("overflowing receiver-bound frame: %v", err)
	}
	waitDone(t, receiver.Done(), "receiver frame ingress overflow")
	assertIngressOverflow(t, receiver.Err(), IngressFrames)
	assertSenderAlive(t, sender)
}

func TestSenderRemoteTerminalDoesNotAffectSiblingTraffic(t *testing.T) {
	ts := startRelay(t)
	ctx := testCtx(t)
	sender, err := DialSender(ctx, senderCfg(ts.URL))
	if err != nil {
		t.Fatalf("DialSender: %v", err)
	}
	t.Cleanup(func() { _ = sender.Close() })

	terminalPeer := dialTestReceiver(t, ts.URL)
	if err := terminalPeer.Channel().Send(ctx, session.Frame{0x61}); err != nil {
		t.Fatalf("seed terminal peer: %v", err)
	}
	terminalSession := awaitSession(t, sender)
	_ = recvFrame(t, terminalSession)

	healthyPeer := dialTestReceiver(t, ts.URL)
	if err := healthyPeer.Channel().Send(ctx, session.Frame{0x62}); err != nil {
		t.Fatalf("seed healthy peer: %v", err)
	}
	healthySession := awaitSession(t, sender)
	_ = recvFrame(t, healthySession)

	terminal := session.Frame{0x7f, 0x01}
	if err := terminalPeer.Channel().SendTerminal(ctx, terminal); err != nil {
		t.Fatalf("peer terminal: %v", err)
	}
	if got := recvFrame(t, terminalSession); !bytes.Equal(got, terminal) {
		t.Fatalf("terminal = %x, want %x", got, terminal)
	}
	waitRecvClosed(t, terminalSession)

	want := session.Frame{0x63, 0x64}
	if err := healthyPeer.Channel().Send(ctx, want); err != nil {
		t.Fatalf("healthy traffic after sibling terminal: %v", err)
	}
	if got := recvFrame(t, healthySession); !bytes.Equal(got, want) {
		t.Fatalf("healthy frame = %x, want %x", got, want)
	}
	assertSenderAlive(t, sender)
}

func TestTerminalUsesReservedIngressSlot(t *testing.T) {
	link, cancel := deadEndLink()
	t.Cleanup(func() {
		cancel()
		link.pump.Close()
		<-link.pump.Done()
	})
	channel := newChannel(protocol.SessionID{0x77}, link)
	for i := range recvBufferFrames {
		if got := channel.deliver(session.Frame{byte(i)}); got != ingressAccepted {
			t.Fatalf("ordinary ingress %d = %v, want accepted", i, got)
		}
	}
	terminal := session.Frame{0xde, 0xad}
	if got := channel.deliverTerminal(terminal); got != ingressAccepted {
		t.Fatalf("terminal ingress = %v, want accepted", got)
	}
	for i := range recvBufferFrames {
		if _, ok := <-channel.Recv(); !ok {
			t.Fatalf("Recv closed before ordinary frame %d", i)
		}
	}
	if got, ok := <-channel.Recv(); !ok || !bytes.Equal(got, terminal) {
		t.Fatalf("reserved terminal = %x/%v, want %x/true", got, ok, terminal)
	}
	if _, ok := <-channel.Recv(); ok {
		t.Fatal("Recv remained open after reserved terminal")
	}
}

func dialTestReceiver(t *testing.T, relayURL string) *ReceiverConn {
	t.Helper()
	receiver, err := DialReceiver(testCtx(t), receiverCfg(relayURL))
	if err != nil {
		t.Fatalf("DialReceiver: %v", err)
	}
	t.Cleanup(func() { _ = receiver.Close() })
	return receiver
}

func assertIngressOverflow(t *testing.T, err error, kind IngressKind) {
	t.Helper()
	if !errors.Is(err, ErrSessionIngressOverflow) {
		t.Fatalf("error = %v, want ErrSessionIngressOverflow", err)
	}
	var overflow *SessionIngressOverflow
	if !errors.As(err, &overflow) || overflow.Kind != kind {
		t.Fatalf("overflow = %#v, want kind %q", overflow, kind)
	}
}

func assertSenderAlive(t *testing.T, sender *SenderConn) {
	t.Helper()
	select {
	case <-sender.Done():
		t.Fatalf("sender connection died after session-local overflow: %v", sender.Err())
	case <-time.After(100 * time.Millisecond):
	}
}
