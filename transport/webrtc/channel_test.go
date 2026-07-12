package webrtc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/core/session"
)

const unitTimeout = 2 * time.Second

type concurrentSendResult struct {
	marker byte
	err    error
}

func TestDefaultDataChannelInit(t *testing.T) {
	first := DefaultDataChannelInit()
	second := DefaultDataChannelInit()
	if first == second || first.Ordered == second.Ordered || first.Protocol == second.Protocol || first.Negotiated == second.Negotiated {
		t.Fatal("DefaultDataChannelInit reused mutable pointer fields")
	}
	if first.Ordered == nil || !*first.Ordered {
		t.Fatal("default channel is not ordered")
	}
	if first.Protocol == nil || *first.Protocol != ChannelProtocol {
		t.Fatalf("default protocol = %v", first.Protocol)
	}
	if first.Negotiated == nil || *first.Negotiated {
		t.Fatal("default channel must use in-band negotiation")
	}
	if first.MaxPacketLifeTime != nil || first.MaxRetransmits != nil || first.ID != nil {
		t.Fatal("default channel unexpectedly limits reliability or pre-negotiates an ID")
	}
}

func TestNewChannelRejectsInvalidConfiguration(t *testing.T) {
	if _, err := NewChannel(nil); !errors.Is(err, ErrNilDataChannel) {
		t.Fatalf("NewChannel(nil) = %v, want ErrNilDataChannel", err)
	}
	one := uint16(1)
	tests := []struct {
		name   string
		mutate func(*fakeDataChannel)
	}{
		{"label", func(dc *fakeDataChannel) { dc.label = "other" }},
		{"protocol", func(dc *fakeDataChannel) { dc.protocol = "other" }},
		{"unordered", func(dc *fakeDataChannel) { dc.ordered = false }},
		{"packet-lifetime", func(dc *fakeDataChannel) { dc.maxPacketLifeTime = &one }},
		{"retransmits", func(dc *fakeDataChannel) { dc.maxRetransmits = &one }},
		{"negotiated", func(dc *fakeDataChannel) { dc.negotiated = true }},
		{"closing", func(dc *fakeDataChannel) { dc.ready = pion.DataChannelStateClosing }},
		{"closed", func(dc *fakeDataChannel) { dc.ready = pion.DataChannelStateClosed }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := newFakeDataChannel(pion.DataChannelStateConnecting)
			test.mutate(fake)
			if _, err := newChannel(fake, defaultFlowControl); !errors.Is(err, ErrInvalidDataChannel) {
				t.Fatalf("newChannel = %v, want ErrInvalidDataChannel", err)
			}
		})
	}

	fake := newFakeDataChannel(pion.DataChannelStateConnecting)
	if _, err := newChannel(fake, flowControlProfile{lowWaterBytes: 4, highWaterBytes: 4}); !errors.Is(err, ErrInvalidFlowControl) {
		t.Fatalf("invalid flow profile = %v, want ErrInvalidFlowControl", err)
	}
}

func TestChannelPublishesOpenOnlyAfterMessageCapability(t *testing.T) {
	fake := newFakeDataChannel(pion.DataChannelStateConnecting)
	fake.maximumMessage = 0
	channel, err := newChannel(fake, defaultFlowControl)
	if err != nil {
		t.Fatalf("construct connecting channel: %v", err)
	}
	if channel.State() != session.Connecting {
		t.Fatalf("initial state = %v, want Connecting", channel.State())
	}
	select {
	case <-channel.Opened():
		t.Fatal("Opened closed before Pion opened")
	default:
	}

	fake.open(session.MaxFrameSize)
	waitOpened(t, channel)
	if channel.State() != session.Open {
		t.Fatalf("opened state = %v, want Open", channel.State())
	}
	if fake.lowThreshold != defaultLowWaterBytes {
		t.Fatalf("low-water threshold = %d, want %d", fake.lowThreshold, defaultLowWaterBytes)
	}
	if err := channel.Close(); err != nil {
		t.Fatalf("close opened channel: %v", err)
	}

	for _, maximum := range []uint32{0, session.MaxFrameSize - 1} {
		t.Run("reject-insufficient-capability", func(t *testing.T) {
			fake := newFakeDataChannel(pion.DataChannelStateConnecting)
			channel, err := newChannel(fake, defaultFlowControl)
			if err != nil {
				t.Fatalf("construct: %v", err)
			}
			fake.open(maximum)
			waitDone(t, channel)
			if !errors.Is(channel.Err(), ErrInvalidDataChannel) {
				t.Fatalf("Err = %v, want ErrInvalidDataChannel", channel.Err())
			}
			select {
			case <-channel.Opened():
				t.Fatal("invalid capability published Opened")
			default:
			}
		})
	}
}

func TestChannelReconcilesCloseDuringCallbackInstallation(t *testing.T) {
	fake := newFakeDataChannel(pion.DataChannelStateConnecting)
	fake.setupClose = fake.remoteClose
	channel, err := newChannel(fake, defaultFlowControl)
	if err != nil {
		t.Fatalf("construct channel during setup close: %v", err)
	}

	waitDone(t, channel)
	if !errors.Is(channel.Err(), ErrRemoteClosed) {
		t.Fatalf("Err = %v, want ErrRemoteClosed", channel.Err())
	}
	if channel.State() != session.Closed {
		t.Fatalf("state = %v, want Closed", channel.State())
	}
	select {
	case <-channel.Opened():
		t.Fatal("setup close published Opened")
	default:
	}
	if err := channel.Close(); err != nil {
		t.Fatalf("cleanup after setup close: %v", err)
	}
}

func TestEarlyOpenCallbackCannotPublishOpened(t *testing.T) {
	fake := newFakeDataChannel(pion.DataChannelStateConnecting)
	channel, err := newChannel(fake, defaultFlowControl)
	if err != nil {
		t.Fatalf("construct connecting channel: %v", err)
	}

	fake.fireOpenCallback()
	waitDone(t, channel)
	if !errors.Is(channel.Err(), ErrInvalidDataChannel) {
		t.Fatalf("Err = %v, want ErrInvalidDataChannel", channel.Err())
	}
	select {
	case <-channel.Opened():
		t.Fatal("early callback published Opened while Pion remained Connecting")
	default:
	}
	if err := channel.Close(); err != nil {
		t.Fatalf("cleanup after early open callback: %v", err)
	}
}

func TestMessageAdmissionSerializesOpenReconciliation(t *testing.T) {
	t.Run("raw-open-message-wins-open-callback", func(t *testing.T) {
		gate, release := newInboundGate(t)
		fake := newFakeDataChannel(pion.DataChannelStateConnecting)
		channel, err := newChannelWithRuntime(fake, defaultFlowControl, channelRuntime{inboundGate: gate})
		if err != nil {
			t.Fatalf("construct connecting channel: %v", err)
		}
		t.Cleanup(func() { _ = channel.Close() })

		fake.markOpenWithoutCallback(session.MaxFrameSize)
		fake.deliverBinary(session.Frame{0x31})
		waitOpened(t, channel)
		if channel.State() != session.Open {
			t.Fatalf("state after message reconciliation = %v, want Open", channel.State())
		}

		release()
		select {
		case frame, ok := <-channel.Recv():
			if !ok || !bytes.Equal(frame, []byte{0x31}) {
				t.Fatalf("reconciled message = %x, open=%t", frame, ok)
			}
		case <-time.After(unitTimeout):
			t.Fatal("timeout waiting for reconciled message")
		}
		fake.fireOpenCallback()
		select {
		case <-channel.Done():
			t.Fatalf("delayed Open callback closed channel: %v", channel.Err())
		default:
		}
	})

	t.Run("actually-connecting-message-fails-closed", func(t *testing.T) {
		gate, release := newInboundGate(t)
		fake := newFakeDataChannel(pion.DataChannelStateConnecting)
		channel, err := newChannelWithRuntime(fake, defaultFlowControl, channelRuntime{inboundGate: gate})
		if err != nil {
			t.Fatalf("construct connecting channel: %v", err)
		}

		fake.deliverBinary(session.Frame{0x32})
		fake.markOpenWithoutCallback(session.MaxFrameSize)
		fake.fireOpenCallback()
		select {
		case <-channel.Opened():
			t.Fatal("message observed while raw channel was Connecting later published Opened")
		default:
		}

		release()
		waitDone(t, channel)
		if !errors.Is(channel.Err(), ErrInvalidDataChannel) {
			t.Fatalf("Err = %v, want ErrInvalidDataChannel", channel.Err())
		}
		if channel.State() != session.Closed {
			t.Fatalf("state = %v, want Closed", channel.State())
		}
		if _, ok := <-channel.Recv(); ok {
			t.Fatal("Recv remained open after pre-open message failure")
		}
		if err := channel.Close(); err != nil {
			t.Fatalf("cleanup failed channel: %v", err)
		}
	})
}

func TestFlowControlRejectsStaleAndCoalescedWakes(t *testing.T) {
	flow := flowControlProfile{lowWaterBytes: 10, highWaterBytes: 20}
	fake, channel := openFakeChannel(t, flow)
	fake.setBuffered(flow.highWaterBytes)

	result := make(chan error, 1)
	go func() { result <- channel.Send(context.Background(), session.Frame{0x41}) }()
	assertNoResult(t, result, "saturated Send returned")

	fake.setBuffered(flow.highWaterBytes - 1)
	fake.fireLow()
	fake.fireLow()
	assertNoResult(t, result, "stale low-water callback released Send")

	fake.setBuffered(flow.lowWaterBytes)
	fake.fireLow()
	if err := receiveError(t, result); err != nil {
		t.Fatalf("Send after low-water crossing: %v", err)
	}
	select {
	case sent := <-fake.sent:
		if !bytes.Equal(sent.frame, []byte{0x41}) {
			t.Fatalf("sent frame = %x", sent.frame)
		}
	case <-time.After(unitTimeout):
		t.Fatal("frame was not sent after capacity recovery")
	}
	_ = channel.Close()
}

func TestFlowControlSerializesConcurrentWakeups(t *testing.T) {
	flow := flowControlProfile{lowWaterBytes: 10, highWaterBytes: 20}
	fake, channel := openFakeChannel(t, flow)
	fake.sendIncrement = flow.highWaterBytes
	fake.setBuffered(flow.highWaterBytes)

	results := make(chan concurrentSendResult, 2)
	for _, marker := range []byte{0x51, 0x52} {
		go func() {
			results <- concurrentSendResult{marker: marker, err: channel.Send(context.Background(), session.Frame{marker})}
		}()
	}
	assertNoSendResult(t, results, "concurrent saturated Send returned")

	fake.setBuffered(flow.lowWaterBytes)
	fake.fireLow()
	first := receiveSendResult(t, results)
	if first.err != nil {
		t.Fatalf("first recovered Send: %v", first.err)
	}
	assertNoSendResult(t, results, "second sender reused a stale low-water observation")

	fake.setBuffered(flow.lowWaterBytes)
	fake.fireLow()
	second := receiveSendResult(t, results)
	if second.err != nil {
		t.Fatalf("second recovered Send: %v", second.err)
	}
	if first.marker == second.marker {
		t.Fatalf("same sender completed twice: 0x%x", first.marker)
	}
	_ = channel.Close()
}

func TestInboundProtocolFailuresCloseTheChannel(t *testing.T) {
	tests := []struct {
		name    string
		deliver func(*fakeDataChannel)
	}{
		{"unknown-text", func(dc *fakeDataChannel) { dc.deliverText("unknown") }},
		{"empty-binary", func(dc *fakeDataChannel) { dc.deliverBinary(nil) }},
		{"oversized-binary", func(dc *fakeDataChannel) { dc.deliverBinary(make(session.Frame, session.MaxFrameSize+1)) }},
		{"unsolicited-ack", func(dc *fakeDataChannel) { dc.deliverText(terminalAckControl) }},
		{"duplicate-intent", func(dc *fakeDataChannel) {
			dc.deliverText(terminalIntentControl)
			dc.deliverText(terminalIntentControl)
		}},
		{"intent-without-frame", func(dc *fakeDataChannel) {
			dc.deliverText(terminalIntentControl)
			dc.remoteClose()
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake, channel := openFakeChannel(t, defaultFlowControl)
			test.deliver(fake)
			waitDone(t, channel)
			if !errors.Is(channel.Err(), ErrPeerProtocol) {
				t.Fatalf("Err = %v, want ErrPeerProtocol", channel.Err())
			}
			if channel.State() != session.Closed {
				t.Fatalf("state = %v, want Closed", channel.State())
			}
		})
	}
}

func TestTerminalCancellationOwnsAndClosesLifecycle(t *testing.T) {
	flow := flowControlProfile{lowWaterBytes: 10, highWaterBytes: 20}
	fake, channel := openFakeChannel(t, flow)
	fake.setBuffered(flow.highWaterBytes)

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- channel.SendTerminal(ctx, session.Frame{0x71}) }()
	assertNoResult(t, result, "saturated terminal returned before cancellation")
	cancel()
	err := receiveError(t, result)
	if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrTerminalNotAcknowledged) {
		t.Fatalf("SendTerminal cancellation = %v", err)
	}
	waitDone(t, channel)
	if channel.State() != session.Closed {
		t.Fatalf("state = %v, want Closed", channel.State())
	}
	if err := channel.Send(context.Background(), session.Frame{1}); err == nil {
		t.Fatal("ordinary Send succeeded after terminal cancellation")
	}
	if err := channel.Close(); err != nil {
		t.Fatalf("Close after canceled terminal: %v", err)
	}
}

func TestLocalTerminalProtocolFailurePreservesAcknowledgementError(t *testing.T) {
	fake, channel := openFakeChannel(t, defaultFlowControl)
	result := make(chan error, 1)
	go func() {
		result <- channel.SendTerminal(context.Background(), session.Frame{0x70})
	}()

	select {
	case sent := <-fake.sent:
		if !sent.terminal || !bytes.Equal(sent.frame, []byte{0x70}) {
			t.Fatalf("terminal send = terminal:%t frame:%x", sent.terminal, sent.frame)
		}
	case <-time.After(unitTimeout):
		t.Fatal("timeout waiting for local terminal send")
	}
	fake.deliverText("unknown")

	err := receiveError(t, result)
	if !errors.Is(err, ErrPeerProtocol) || !errors.Is(err, ErrTerminalNotAcknowledged) {
		t.Fatalf("SendTerminal protocol failure = %v", err)
	}
	waitDone(t, channel)
	if !errors.Is(channel.Err(), ErrPeerProtocol) || !errors.Is(channel.Err(), ErrTerminalNotAcknowledged) {
		t.Fatalf("Err = %v, want ErrPeerProtocol and ErrTerminalNotAcknowledged", channel.Err())
	}
}

func TestLocalTerminalAdmissionBypassesStalledInboundDelivery(t *testing.T) {
	fake, channel := openFakeChannel(t, defaultFlowControl)
	for index := range receiveQueueFrames + 1 {
		fake.deliverBinary(session.Frame{byte(index + 1)})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- channel.SendTerminal(ctx, session.Frame{0x72}) }()

	sent, err := fake.receiveSent(ctx)
	if err != nil {
		t.Fatalf("receive terminal send: %v", err)
	}
	if !sent.Terminal || !bytes.Equal(sent.Frame, []byte{0x72}) {
		t.Fatalf("terminal send = terminal:%t frame:%x", sent.Terminal, sent.Frame)
	}
	if err := receiveError(t, result); err != nil {
		t.Fatalf("SendTerminal behind stalled inbound delivery: %v", err)
	}
}

func TestCloseAndTerminalAdmissionHaveOneLinearization(t *testing.T) {
	for iteration := range 50 {
		fake, channel := openFakeChannel(t, defaultFlowControl)
		ctx, cancel := context.WithCancel(context.Background())
		start := make(chan struct{})
		terminalResult := make(chan error, 1)
		closeResult := make(chan error, 1)
		go func() {
			<-start
			terminalResult <- channel.SendTerminal(ctx, session.Frame{byte(iteration + 1)})
		}()
		go func() {
			<-start
			closeResult <- channel.Close()
		}()
		close(start)

		select {
		case sent := <-fake.sent:
			if !sent.terminal {
				t.Fatalf("iteration %d emitted ordinary frame during terminal race", iteration)
			}
			select {
			case err := <-closeResult:
				t.Fatalf("iteration %d Close overtook admitted terminal: %v", iteration, err)
			default:
			}
			if calls := fake.closeCalls(); calls != 0 {
				t.Fatalf("iteration %d physical Close calls before ACK = %d", iteration, calls)
			}
			fake.deliverText(terminalAckControl)
			if err := receiveError(t, terminalResult); err != nil {
				t.Fatalf("iteration %d admitted terminal failed: %v", iteration, err)
			}
			if err := receiveError(t, closeResult); err != nil {
				t.Fatalf("iteration %d Close after ACK: %v", iteration, err)
			}
		case err := <-terminalResult:
			if err == nil || !errors.Is(err, ErrChannelClosed) {
				t.Fatalf("iteration %d terminal losing to Close = %v", iteration, err)
			}
			if err := receiveError(t, closeResult); err != nil {
				t.Fatalf("iteration %d winning Close: %v", iteration, err)
			}
			select {
			case sent := <-fake.sent:
				t.Fatalf("iteration %d losing terminal emitted frame: %+v", iteration, sent)
			default:
			}
		case <-time.After(unitTimeout):
			cancel()
			t.Fatalf("iteration %d terminal/Close race did not settle to an observable winner", iteration)
		}
		assertChannelSettled(t, channel)
		cancel()
	}
}

func TestInboundTerminationDrainsAcceptedBinaryBeforeEOF(t *testing.T) {
	tests := []struct {
		name      string
		terminate func(*fakeDataChannel)
		want      error
	}{
		{"remote-close", func(dc *fakeDataChannel) { dc.remoteClose() }, ErrRemoteClosed},
		{"transport-error", func(dc *fakeDataChannel) { dc.fail(errors.New("read failed after binary")) }, ErrTransport},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gate, release := newInboundGate(t)
			fake := newFakeDataChannel(pion.DataChannelStateOpen)
			channel, err := newChannelWithRuntime(fake, defaultFlowControl, channelRuntime{inboundGate: gate})
			if err != nil {
				t.Fatalf("construct gated channel: %v", err)
			}
			waitOpened(t, channel)

			want := session.Frame{0x81, 0x82}
			fake.deliverBinary(want)
			test.terminate(fake)
			release()

			select {
			case frame, ok := <-channel.Recv():
				if !ok || !bytes.Equal(frame, want) {
					t.Fatalf("frame before termination = %x, open=%t", frame, ok)
				}
			case <-time.After(unitTimeout):
				t.Fatal("timeout waiting for callback-accepted frame")
			}
			if _, ok := <-channel.Recv(); ok {
				t.Fatal("Recv remained open after ordered termination")
			}
			waitDone(t, channel)
			if !errors.Is(channel.Err(), test.want) {
				t.Fatalf("Err = %v, want %v", channel.Err(), test.want)
			}
			if err := channel.Close(); err != nil {
				t.Fatalf("cleanup terminated channel: %v", err)
			}
		})
	}
}

func TestTerminalAckAdmissionCannotBeOvertakenByTermination(t *testing.T) {
	tests := []struct {
		name      string
		terminate func(*fakeDataChannel)
	}{
		{"transport-error", func(dc *fakeDataChannel) { dc.fail(errors.New("read failed after acknowledgement")) }},
		{"remote-close", func(dc *fakeDataChannel) { dc.fireCloseCallback() }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gate, release := newInboundGate(t)
			fake := newFakeDataChannel(pion.DataChannelStateOpen)
			channel, err := newChannelWithRuntime(fake, defaultFlowControl, channelRuntime{inboundGate: gate})
			if err != nil {
				t.Fatalf("construct gated channel: %v", err)
			}
			waitOpened(t, channel)

			result := make(chan error, 1)
			go func() { result <- channel.SendTerminal(context.Background(), session.Frame{0x91}) }()
			select {
			case sent := <-fake.sent:
				if !sent.terminal {
					t.Fatal("terminal frame was not marked terminal")
				}
			case <-time.After(unitTimeout):
				t.Fatal("timeout waiting for terminal frame")
			}

			fake.deliverText(terminalAckControl)
			test.terminate(fake)
			release()
			if err := receiveError(t, result); err != nil {
				t.Fatalf("acknowledged SendTerminal = %v", err)
			}
			waitDone(t, channel)
			if channel.Err() != nil {
				t.Fatalf("acknowledged terminal Err = %v", channel.Err())
			}
			if err := channel.Close(); err != nil {
				t.Fatalf("cleanup acknowledged channel: %v", err)
			}
		})
	}
}

func TestRemoteTerminalIntentWakesSaturatedOrdinarySend(t *testing.T) {
	flow := flowControlProfile{lowWaterBytes: 10, highWaterBytes: 20}
	fake, channel := openFakeChannel(t, flow)
	fake.setBuffered(flow.highWaterBytes)

	sendResult := make(chan error, 1)
	go func() { sendResult <- channel.Send(context.Background(), session.Frame{0x73}) }()
	assertNoResult(t, sendResult, "saturated ordinary Send returned")

	ctx, cancel := context.WithTimeout(context.Background(), unitTimeout)
	defer cancel()
	terminalResult := make(chan error, 1)
	go func() { terminalResult <- fake.deliverTerminal(ctx, session.Frame{0x74}) }()
	if got := <-channel.Recv(); !bytes.Equal(got, []byte{0x74}) {
		t.Fatalf("remote terminal = %x", got)
	}

	wokeBeforeCapacity := false
	select {
	case err := <-sendResult:
		wokeBeforeCapacity = err != nil
	case <-time.After(50 * time.Millisecond):
	}
	fake.setBuffered(flow.lowWaterBytes)
	fake.fireLow()
	if !wokeBeforeCapacity {
		if err := receiveError(t, sendResult); err == nil {
			t.Fatal("ordinary Send succeeded after remote terminal intent")
		}
	}
	if err := receiveError(t, terminalResult); err != nil {
		t.Fatalf("deliver remote terminal: %v", err)
	}
	if !wokeBeforeCapacity {
		t.Fatal("remote terminal intent did not wake the saturated ordinary Send")
	}
}

func TestRemoteTerminalClosesRecvBeforeAcknowledgement(t *testing.T) {
	fake, channel := openFakeChannel(t, defaultFlowControl)
	terminal := session.Frame{0x7a, 0x7b}
	ackObserved := make(chan error, 1)
	fake.onAck = func() {
		got, ok := <-channel.Recv()
		if !ok {
			ackObserved <- errors.New("Recv closed before terminal frame")
			return
		}
		if !bytes.Equal(got, terminal) {
			ackObserved <- errors.New("terminal frame changed")
			return
		}
		if _, ok := <-channel.Recv(); ok {
			ackObserved <- errors.New("Recv remained open when acknowledgement was sent")
			return
		}
		ackObserved <- nil
	}

	fake.deliverText(terminalIntentControl)
	fake.deliverBinary(terminal)
	if err := receiveError(t, ackObserved); err != nil {
		t.Fatal(err)
	}
	waitDone(t, channel)
	if channel.Err() != nil {
		t.Fatalf("acknowledged remote terminal Err = %v", channel.Err())
	}
	if err := channel.Close(); err != nil {
		t.Fatalf("explicit cleanup after remote terminal: %v", err)
	}
}

func TestRemoteTerminalRequiresAcknowledgementTransmissionForCleanClose(t *testing.T) {
	flow := flowControlProfile{lowWaterBytes: 10, highWaterBytes: 20}
	fake, channel := openFakeChannel(t, flow)
	fake.setBuffered(flow.highWaterBytes)
	bufferedRead := fake.observeBufferedReads()

	fake.deliverText(terminalIntentControl)
	fake.deliverBinary(session.Frame{0xa1, 0xa2})
	select {
	case frame, ok := <-channel.Recv():
		if !ok || !bytes.Equal(frame, []byte{0xa1, 0xa2}) {
			t.Fatalf("remote terminal = %x, open=%t", frame, ok)
		}
	case <-time.After(unitTimeout):
		t.Fatal("timeout waiting for remote terminal")
	}
	if _, ok := <-channel.Recv(); ok {
		t.Fatal("Recv remained open after remote terminal publication")
	}
	select {
	case <-bufferedRead:
	case <-time.After(unitTimeout):
		t.Fatal("terminal acknowledgement did not reach saturated capacity wait")
	}

	fake.remoteClose()
	waitDone(t, channel)
	if !errors.Is(channel.Err(), ErrRemoteClosed) {
		t.Fatalf("Err = %v, want ErrRemoteClosed before acknowledgement transmission", channel.Err())
	}
	if errors.Is(channel.Err(), ErrPeerProtocol) {
		t.Fatalf("published terminal was misclassified as missing: %v", channel.Err())
	}
	select {
	case <-fake.terminalAck:
		t.Fatal("terminal acknowledgement was reported sent after remote close")
	default:
	}
	if err := channel.Close(); err != nil {
		t.Fatalf("cleanup failed remote terminal: %v", err)
	}
}

func TestCloseCannotOvertakeObservedRemoteTerminal(t *testing.T) {
	fake, channel := openFakeChannel(t, defaultFlowControl)
	for index := range receiveQueueFrames {
		fake.deliverBinary(session.Frame{byte(index + 1)})
	}
	terminal := session.Frame{0xf1, 0xf2}
	fake.deliverText(terminalIntentControl)
	fake.deliverBinary(terminal)

	closeResult := make(chan error, 1)
	go func() { closeResult <- channel.Close() }()
	assertNoResult(t, closeResult, "Close overtook queued remote terminal")
	for index := range receiveQueueFrames {
		select {
		case frame, ok := <-channel.Recv():
			if !ok || len(frame) != 1 || frame[0] != byte(index+1) {
				t.Fatalf("ordinary frame %d = %x, open=%t", index, frame, ok)
			}
		case <-time.After(unitTimeout):
			t.Fatalf("timeout draining ordinary frame %d", index)
		}
	}
	if got := <-channel.Recv(); !bytes.Equal(got, terminal) {
		t.Fatalf("terminal frame = %x, want %x", got, terminal)
	}
	if _, ok := <-channel.Recv(); ok {
		t.Fatal("Recv remained open after queued remote terminal")
	}
	if err := receiveError(t, closeResult); err != nil {
		t.Fatalf("Close after remote terminal acknowledgement: %v", err)
	}
}

func TestCallbackRacesCloseExactlyOnce(t *testing.T) {
	for range 50 {
		fake, channel := openFakeChannel(t, defaultFlowControl)
		var group sync.WaitGroup
		group.Add(4)
		go func() { defer group.Done(); fake.fireLow() }()
		go func() { defer group.Done(); fake.fail(errors.New("transport broke")) }()
		go func() { defer group.Done(); fake.remoteClose() }()
		go func() { defer group.Done(); _ = channel.Close() }()
		group.Wait()
		waitDone(t, channel)
		if fake.closeCalls() != 1 {
			t.Fatalf("physical Close calls = %d, want 1", fake.closeCalls())
		}
	}
}

func TestTerminalControlFixtureMatchesImplementation(t *testing.T) {
	data, err := os.ReadFile("testdata/terminal-control.json")
	if err != nil {
		t.Fatalf("read terminal fixture: %v", err)
	}
	var fixture struct {
		Version        int      `json:"version"`
		TerminalIntent string   `json:"terminalIntent"`
		TerminalFrame  string   `json:"terminalFrame"`
		TerminalAck    string   `json:"terminalAck"`
		Sequence       []string `json:"sequence"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatalf("decode terminal fixture: %v", err)
	}
	wantSequence := []string{terminalIntentControl, fixture.TerminalFrame, terminalAckControl}
	if fixture.Version != 1 || fixture.TerminalIntent != terminalIntentControl || fixture.TerminalAck != terminalAckControl || fixture.TerminalFrame == "" || len(fixture.Sequence) != len(wantSequence) {
		t.Fatalf("terminal fixture does not match implementation: %+v", fixture)
	}
	for index := range wantSequence {
		if fixture.Sequence[index] != wantSequence[index] {
			t.Fatalf("terminal sequence[%d] = %q, want %q", index, fixture.Sequence[index], wantSequence[index])
		}
	}
}

func openFakeChannel(t *testing.T, flow flowControlProfile) (*fakeDataChannel, *Channel) {
	t.Helper()
	fake := newFakeDataChannel(pion.DataChannelStateOpen)
	channel, err := newChannel(fake, flow)
	if err != nil {
		t.Fatalf("construct channel: %v", err)
	}
	waitOpened(t, channel)
	t.Cleanup(func() { _ = channel.Close() })
	return fake, channel
}

func waitOpened(t *testing.T, channel *Channel) {
	t.Helper()
	select {
	case <-channel.Opened():
	case <-channel.Done():
		t.Fatalf("channel closed before Opened: %v", channel.Err())
	case <-time.After(unitTimeout):
		t.Fatal("timeout waiting for Opened")
	}
}

func waitDone(t *testing.T, channel *Channel) {
	t.Helper()
	select {
	case <-channel.Done():
	case <-time.After(unitTimeout):
		t.Fatal("timeout waiting for Done")
	}
}

func receiveError(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(unitTimeout):
		t.Fatal("timeout waiting for operation result")
		return nil
	}
}

func assertNoResult(t *testing.T, result <-chan error, message string) {
	t.Helper()
	select {
	case err := <-result:
		t.Fatalf("%s: %v", message, err)
	case <-time.After(50 * time.Millisecond):
	}
}

func assertNoSendResult(t *testing.T, result <-chan concurrentSendResult, message string) {
	t.Helper()
	select {
	case got := <-result:
		t.Fatalf("%s: marker=0x%x err=%v", message, got.marker, got.err)
	case <-time.After(50 * time.Millisecond):
	}
}

func receiveSendResult(t *testing.T, result <-chan concurrentSendResult) concurrentSendResult {
	t.Helper()
	select {
	case got := <-result:
		return got
	case <-time.After(unitTimeout):
		t.Fatal("timeout waiting for concurrent Send result")
		return concurrentSendResult{}
	}
}

func newInboundGate(t *testing.T) (<-chan struct{}, func()) {
	t.Helper()
	gate := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	t.Cleanup(release)
	return gate, release
}

func assertChannelSettled(t *testing.T, channel *Channel) {
	t.Helper()
	for name, settled := range map[string]<-chan struct{}{
		"Done":         channel.Done(),
		"inboundDone":  channel.inboundDone,
		"physicalDone": channel.physicalDone,
	} {
		select {
		case <-settled:
		default:
			t.Fatalf("%s remained unsettled", name)
		}
	}
}
