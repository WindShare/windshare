package webrtc

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/core/framechannel"
)

func TestInboundProtocolFailuresCloseTheChannel(t *testing.T) {
	tests := []struct {
		name    string
		deliver func(*fakeDataChannel)
	}{
		{"unknown-text", func(dc *fakeDataChannel) { dc.deliverText("unknown") }},
		{"empty-binary", func(dc *fakeDataChannel) { dc.deliverBinary(nil) }},
		{"oversized-binary", func(dc *fakeDataChannel) { dc.deliverBinary(make(framechannel.Frame, framechannel.MaxFrameSize+1)) }},
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
			if channel.State() != framechannel.Closed {
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
	go func() { result <- channel.SendTerminal(ctx, framechannel.Frame{0x71}) }()
	assertNoResult(t, result, "saturated terminal returned before cancellation")
	cancel()
	err := receiveError(t, result)
	if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrTerminalNotAcknowledged) {
		t.Fatalf("SendTerminal cancellation = %v", err)
	}
	if disposition := framechannel.SendDispositionOf(err); disposition != framechannel.SendAccepted {
		t.Fatalf("admitted terminal cancellation disposition=%d", disposition)
	}
	waitDone(t, channel)
	if channel.State() != framechannel.Closed {
		t.Fatalf("state = %v, want Closed", channel.State())
	}
	if err := channel.Send(context.Background(), framechannel.Frame{1}); err == nil {
		t.Fatal("ordinary Send succeeded after terminal cancellation")
	}
	if err := channel.Close(); err != nil {
		t.Fatalf("Close after canceled terminal: %v", err)
	}
}

func TestPreAdmissionCancellationAndRemoteTerminalHaveImmutableWinners(t *testing.T) {
	t.Run("cancellation-before-remote-terminal", func(t *testing.T) {
		lifecycle := newChannelLifecycle()
		lifecycle.publishOpen()
		ctx, cancel := context.WithCancel(context.Background())
		admission := lifecycle.beginSendAdmission(ctx, sendTerminal)

		cancel()
		if !lifecycle.reserveRemoteIntent() {
			t.Fatal("remote terminal did not win the channel transition")
		}
		err := lifecycle.admitLocalTerminal(admission)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("terminal admission = %v, want cancellation", err)
		}
		if disposition := framechannel.SendDispositionOf(err); disposition != framechannel.SendRejected {
			t.Fatalf("cancellation-first disposition = %d, want rejected", disposition)
		}
	})

	t.Run("remote-terminal-before-cancellation", func(t *testing.T) {
		lifecycle := newChannelLifecycle()
		lifecycle.publishOpen()
		ctx, cancel := context.WithCancel(context.Background())
		admission := lifecycle.beginSendAdmission(ctx, sendTerminal)

		if !lifecycle.reserveRemoteIntent() {
			t.Fatal("remote terminal did not win the channel transition")
		}
		cancel()
		err := lifecycle.admitLocalTerminal(admission)
		if !errors.Is(err, ErrChannelClosed) {
			t.Fatalf("terminal admission = %v, want channel retirement", err)
		}
		if errors.Is(err, context.Canceled) {
			t.Fatalf("later cancellation replaced remote retirement: %v", err)
		}
		if disposition := framechannel.SendDispositionOf(err); disposition != framechannel.SendRetired {
			t.Fatalf("remote-first disposition = %d, want retired", disposition)
		}
	})
}

func TestOrdinarySendRefusalCannotBeReclassifiedByLaterTermination(t *testing.T) {
	laterFailure := errors.New("physical failure after terminal ownership")
	t.Run("remote-terminal-before-failure", func(t *testing.T) {
		lifecycle := newChannelLifecycle()
		lifecycle.publishOpen()
		admission := lifecycle.beginSendAdmission(context.Background(), sendOrdinary)

		if !lifecycle.reserveRemoteIntent() {
			t.Fatal("remote terminal did not win admission")
		}
		if !lifecycle.beginTermination(laterFailure) {
			t.Fatal("later physical termination was not published")
		}
		err := lifecycle.sendAdmissionError(admission)
		if disposition := framechannel.SendDispositionOf(err); disposition != framechannel.SendRetired {
			t.Fatalf("terminal-first disposition = %d error=%v", disposition, err)
		}
		if errors.Is(err, laterFailure) {
			t.Fatalf("later failure rewrote immutable retirement: %v", err)
		}
	})

	t.Run("failure-before-remote-terminal", func(t *testing.T) {
		lifecycle := newChannelLifecycle()
		lifecycle.publishOpen()
		admission := lifecycle.beginSendAdmission(context.Background(), sendOrdinary)

		if !lifecycle.beginTermination(laterFailure) {
			t.Fatal("physical failure did not win admission")
		}
		if lifecycle.reserveRemoteIntent() {
			t.Fatal("remote terminal overtook an already-published failure")
		}
		err := lifecycle.sendAdmissionError(admission)
		if disposition := framechannel.SendDispositionOf(err); disposition != framechannel.SendRejected {
			t.Fatalf("failure-first disposition = %d error=%v", disposition, err)
		}
		if !errors.Is(err, laterFailure) {
			t.Fatalf("failure-first cause = %v, want physical failure", err)
		}
	})
}

func TestLocalTerminalProtocolFailurePreservesAcknowledgementError(t *testing.T) {
	fake, channel := openFakeChannel(t, defaultFlowControl)
	result := make(chan error, 1)
	go func() {
		result <- channel.SendTerminal(context.Background(), framechannel.Frame{0x70})
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
		fake.deliverBinary(framechannel.Frame{byte(index + 1)})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- channel.SendTerminal(ctx, framechannel.Frame{0x72}) }()

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
			terminalResult <- channel.SendTerminal(ctx, framechannel.Frame{byte(iteration + 1)})
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
			if disposition := framechannel.SendDispositionOf(err); disposition != framechannel.SendRetired {
				t.Fatalf("iteration %d terminal/Close disposition=%d", iteration, disposition)
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

			want := framechannel.Frame{0x81, 0x82}
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
			if err := channel.SendTerminal(context.Background(), framechannel.Frame{0x83}); err == nil || framechannel.SendDispositionOf(err) != framechannel.SendRejected {
				t.Fatalf("terminal after failed transport = %v disposition=%d", err, framechannel.SendDispositionOf(err))
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
			go func() { result <- channel.SendTerminal(context.Background(), framechannel.Frame{0x91}) }()
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
	go func() { sendResult <- channel.Send(context.Background(), framechannel.Frame{0x73}) }()
	assertNoResult(t, sendResult, "saturated ordinary Send returned")

	ctx, cancel := context.WithTimeout(context.Background(), unitTimeout)
	defer cancel()
	terminalResult := make(chan error, 1)
	go func() { terminalResult <- fake.deliverTerminal(ctx, framechannel.Frame{0x74}) }()
	if got := <-channel.Recv(); !bytes.Equal(got, []byte{0x74}) {
		t.Fatalf("remote terminal = %x", got)
	}

	wokeBeforeCapacity := false
	var sendErr error
	select {
	case err := <-sendResult:
		wokeBeforeCapacity = err != nil
		sendErr = err
	case <-time.After(50 * time.Millisecond):
	}
	fake.setBuffered(flow.lowWaterBytes)
	fake.fireLow()
	if !wokeBeforeCapacity {
		sendErr = receiveError(t, sendResult)
		if sendErr == nil {
			t.Fatal("ordinary Send succeeded after remote terminal intent")
		}
	}
	if disposition := framechannel.SendDispositionOf(sendErr); disposition != framechannel.SendRetired {
		t.Fatalf("ordinary send after remote terminal disposition=%d error=%v", disposition, sendErr)
	}
	if err := receiveError(t, terminalResult); err != nil {
		t.Fatalf("deliver remote terminal: %v", err)
	}
	if !wokeBeforeCapacity {
		t.Fatal("remote terminal intent did not wake the saturated ordinary Send")
	}
}

func TestLocalTerminalRetiresSaturatedOrdinarySendBeforeTransmission(t *testing.T) {
	flow := flowControlProfile{lowWaterBytes: 10, highWaterBytes: 20}
	fake, channel := openFakeChannel(t, flow)
	fake.setBuffered(flow.highWaterBytes)
	bufferedRead := fake.observeBufferedReads()

	ordinaryResult := make(chan error, 1)
	go func() {
		ordinaryResult <- channel.Send(context.Background(), framechannel.Frame{0x75})
	}()
	select {
	case <-bufferedRead:
	case <-time.After(unitTimeout):
		t.Fatal("ordinary send did not reach saturated capacity wait")
	}

	terminalResult := make(chan error, 1)
	go func() {
		terminalResult <- channel.SendTerminal(context.Background(), framechannel.Frame{0x76})
	}()
	ordinaryErr := receiveError(t, ordinaryResult)
	if disposition := framechannel.SendDispositionOf(ordinaryErr); disposition != framechannel.SendRetired {
		t.Fatalf("saturated ordinary disposition = %d error=%v", disposition, ordinaryErr)
	}
	select {
	case sent := <-fake.sent:
		t.Fatalf("frame transmitted before terminal capacity admission: %+v", sent)
	default:
	}

	fake.setBuffered(flow.lowWaterBytes)
	fake.fireLow()
	select {
	case sent := <-fake.sent:
		if !sent.terminal || !bytes.Equal(sent.frame, []byte{0x76}) {
			t.Fatalf("post-saturation send = terminal:%t frame:%x", sent.terminal, sent.frame)
		}
	case <-time.After(unitTimeout):
		t.Fatal("terminal did not acquire the released send turn")
	}
	fake.deliverText(terminalAckControl)
	if err := receiveError(t, terminalResult); err != nil {
		t.Fatalf("terminal after saturated ordinary send: %v", err)
	}
}

func TestRemoteTerminalClosesRecvBeforeAcknowledgement(t *testing.T) {
	fake, channel := openFakeChannel(t, defaultFlowControl)
	terminal := framechannel.Frame{0x7a, 0x7b}
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
	fake.deliverBinary(framechannel.Frame{0xa1, 0xa2})
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

func TestRemoteTerminalAckNormalizesConcurrentPhysicalTermination(t *testing.T) {
	tests := []struct {
		name      string
		terminate func(*fakeDataChannel)
	}{
		{
			name:      "remote-close",
			terminate: func(dc *fakeDataChannel) { dc.fireCloseCallback() },
		},
		{
			name: "transport-error",
			terminate: func(dc *fakeDataChannel) {
				dc.fail(errors.New("physical error after terminal acknowledgement"))
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			finishGate, releaseFinish := newInboundGate(t)
			fake := newFakeDataChannel(pion.DataChannelStateOpen)
			channel, err := newChannelWithRuntime(fake, defaultFlowControl, channelRuntime{
				remoteTerminalFinishGate: finishGate,
			})
			if err != nil {
				t.Fatalf("construct gated channel: %v", err)
			}
			waitOpened(t, channel)

			fake.deliverText(terminalIntentControl)
			fake.deliverBinary(framechannel.Frame{0xb1, 0xb2})
			select {
			case <-fake.terminalAck:
			case <-time.After(unitTimeout):
				t.Fatal("remote terminal acknowledgement was not transmitted")
			}
			test.terminate(fake)
			releaseFinish()

			waitDone(t, channel)
			if channel.Err() != nil {
				t.Fatalf("acknowledged remote terminal Err = %v", channel.Err())
			}
			channel.lifecycle.mu.Lock()
			pending := channel.lifecycle.terminationPending
			pendingBase := channel.lifecycle.terminationBase
			channel.lifecycle.mu.Unlock()
			if pending || pendingBase != nil {
				t.Fatalf("completed lifecycle retained termination pending=%t base=%v", pending, pendingBase)
			}
			err = channel.SendTerminal(context.Background(), framechannel.Frame{0xb3})
			if disposition := framechannel.SendDispositionOf(err); disposition != framechannel.SendRetired {
				t.Fatalf("send after clean remote completion disposition=%d error=%v", disposition, err)
			}
			if err := channel.Close(); err != nil {
				t.Fatalf("cleanup acknowledged remote terminal: %v", err)
			}
		})
	}
}

func TestPhysicalTerminationBeforeRemoteAckRemainsFailure(t *testing.T) {
	flow := flowControlProfile{lowWaterBytes: 10, highWaterBytes: 20}
	tests := []struct {
		name      string
		terminate func(*fakeDataChannel)
		want      error
	}{
		{
			name:      "remote-close",
			terminate: func(dc *fakeDataChannel) { dc.fireCloseCallback() },
			want:      ErrRemoteClosed,
		},
		{
			name: "transport-error",
			terminate: func(dc *fakeDataChannel) {
				dc.fail(errors.New("physical error before terminal acknowledgement"))
			},
			want: ErrTransport,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake, channel := openFakeChannel(t, flow)
			fake.setBuffered(flow.highWaterBytes)
			bufferedRead := fake.observeBufferedReads()

			fake.deliverText(terminalIntentControl)
			fake.deliverBinary(framechannel.Frame{0xc1, 0xc2})
			select {
			case <-bufferedRead:
			case <-time.After(unitTimeout):
				t.Fatal("remote acknowledgement did not reach saturated capacity wait")
			}
			test.terminate(fake)

			waitDone(t, channel)
			if !errors.Is(channel.Err(), test.want) {
				t.Fatalf("Err = %v, want %v", channel.Err(), test.want)
			}
			select {
			case <-fake.terminalAck:
				t.Fatal("acknowledgement transmitted after physical termination")
			default:
			}
			err := channel.SendTerminal(context.Background(), framechannel.Frame{0xc3})
			if disposition := framechannel.SendDispositionOf(err); disposition != framechannel.SendRejected {
				t.Fatalf("send after failed remote completion disposition=%d error=%v", disposition, err)
			}
		})
	}
}

func TestCloseCannotOvertakeObservedRemoteTerminal(t *testing.T) {
	fake, channel := openFakeChannel(t, defaultFlowControl)
	for index := range receiveQueueFrames {
		fake.deliverBinary(framechannel.Frame{byte(index + 1)})
	}
	terminal := framechannel.Frame{0xf1, 0xf2}
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
