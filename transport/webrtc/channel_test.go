package webrtc

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/core/framechannel"
)

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

func TestDefaultFlowControlKeepsPeakBelowExclusivePublishedBound(t *testing.T) {
	const (
		expectedHighWaterBytes          = 1024 * 1024
		expectedMaxFrameBytes           = 64 * 1024
		expectedSendAdmissionHighWater  = expectedHighWaterBytes - 1
		expectedPeakExclusiveLimitBytes = expectedHighWaterBytes + expectedMaxFrameBytes
	)
	if defaultHighWaterBytes != expectedHighWaterBytes ||
		framechannel.MaxFrameSize != expectedMaxFrameBytes ||
		defaultFlowControl.highWaterBytes != expectedSendAdmissionHighWater {
		t.Fatalf(
			"production peak inputs changed: high=%d/%d frame=%d/%d admission=%d/%d",
			defaultHighWaterBytes,
			expectedHighWaterBytes,
			framechannel.MaxFrameSize,
			expectedMaxFrameBytes,
			defaultFlowControl.highWaterBytes,
			expectedSendAdmissionHighWater,
		)
	}
	maximumAdmittedPeak := defaultFlowControl.highWaterBytes + uint64(framechannel.MaxFrameSize)
	if maximumAdmittedPeak >= expectedPeakExclusiveLimitBytes {
		t.Fatalf(
			"maximum admitted peak = %d, must be below exclusive limit %d",
			maximumAdmittedPeak,
			expectedPeakExclusiveLimitBytes,
		)
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

func TestLifecycleTraceCorrelatesImmutableSendDecisions(t *testing.T) {
	events := make(chan LifecycleTrace, 8)
	fake := newFakeDataChannel(pion.DataChannelStateOpen)
	channel, err := newChannelWithRuntime(fake, defaultFlowControl, channelRuntime{
		lifecycleTracer: LifecycleTraceFunc(func(event LifecycleTrace) { events <- event }),
	})
	if err != nil {
		t.Fatalf("construct traced channel: %v", err)
	}
	waitOpened(t, channel)

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	err = channel.Send(canceled, framechannel.Frame{0x21})
	if disposition := framechannel.SendDispositionOf(err); disposition != framechannel.SendRejected {
		t.Fatalf("canceled send disposition=%d error=%v", disposition, err)
	}
	rejected := receiveLifecycleTrace(t, events)
	if rejected.ChannelID == 0 || rejected.OperationID == 0 ||
		rejected.Operation != LifecycleOperationSend ||
		rejected.Transition != LifecycleTransitionSendRejected ||
		rejected.Disposition != framechannel.SendRejected ||
		rejected.Cause != LifecycleCauseCanceled {
		t.Fatalf("rejected trace = %+v", rejected)
	}

	fake.sendErr = errors.New("provider send failed")
	err = channel.Send(context.Background(), framechannel.Frame{0x22})
	if !errors.Is(err, ErrTransport) || framechannel.SendDispositionOf(err) != framechannel.SendAccepted {
		t.Fatalf("accepted provider failure: %v", err)
	}
	accepted := receiveLifecycleTrace(t, events)
	if accepted.ChannelID != rejected.ChannelID || accepted.OperationID <= rejected.OperationID ||
		accepted.Operation != LifecycleOperationSend ||
		accepted.Transition != LifecycleTransitionSendAccepted ||
		accepted.Disposition != framechannel.SendAccepted ||
		accepted.Cause != LifecycleCauseTransport {
		t.Fatalf("accepted trace = %+v after %+v", accepted, rejected)
	}

	closed := receiveLifecycleTrace(t, events)
	if closed.ChannelID != rejected.ChannelID ||
		closed.Operation != LifecycleOperationChannel ||
		closed.Transition != LifecycleTransitionClosedFailed ||
		closed.State != framechannel.Closed ||
		closed.Cause != LifecycleCauseTransport {
		t.Fatalf("closed trace = %+v", closed)
	}
	if err := channel.Close(); err != nil {
		t.Fatalf("close traced channel: %v", err)
	}
	completion := channel.CompleteObservations(context.Background())
	if !completion.Drained || completion.Delivered != 3 || completion.Loss.Total() != 0 {
		t.Fatalf("channel observation completion = %+v", completion)
	}
}

func TestLifecycleTraceBackpressureIsBoundedAndObservable(t *testing.T) {
	const overflowEvents = 5
	entered := make(chan struct{})
	release := make(chan struct{})
	observed := make(chan LifecycleTrace, lifecycleTraceQueueCapacity+2)
	var first sync.Once
	dispatcher := newLifecycleTraceDispatcher(LifecycleTraceFunc(func(event LifecycleTrace) {
		first.Do(func() {
			close(entered)
			<-release
		})
		observed <- event
	}))
	dispatcher.emit(LifecycleTrace{ChannelID: 7, Transition: LifecycleTransitionSendRejected})
	select {
	case <-entered:
	case <-time.After(unitTimeout):
		t.Fatal("trace observer did not enter its deterministic backpressure gate")
	}
	for operationID := uint64(1); operationID <= lifecycleTraceQueueCapacity+overflowEvents; operationID++ {
		dispatcher.emit(LifecycleTrace{
			ChannelID: operationID, OperationID: operationID,
			Transition: LifecycleTransitionSendRejected,
		})
	}
	dispatcher.shutdown()
	close(release)

	for {
		select {
		case event := <-observed:
			if event.Transition == LifecycleTransitionTraceDropped {
				if event.Dropped != overflowEvents {
					t.Fatalf("dropped trace count = %d, want %d", event.Dropped, overflowEvents)
				}
				completion := dispatcher.complete(context.Background())
				if !completion.Drained || completion.Loss.QueueOverflow != overflowEvents ||
					completion.Loss.Total() != overflowEvents {
					t.Fatalf("overflow completion = %+v", completion)
				}
				return
			}
		case <-time.After(unitTimeout):
			t.Fatal("bounded trace queue did not publish its drop record")
		}
	}
}

func TestWebRTCLifecycleCompletionAccountsTimeoutAndStopsAdmission(t *testing.T) {
	entered := make(chan struct{})
	exited := make(chan struct{})
	var calls atomic.Uint64
	var committed atomic.Uint64
	dispatcher := newLifecycleTraceDispatcher(LifecycleContextTraceFunc(func(ctx context.Context, _ LifecycleTrace) {
		if calls.Add(1) == 1 {
			close(entered)
			<-ctx.Done()
			if ctx.Err() == nil {
				committed.Add(1)
			}
			close(exited)
		}
	}))
	dispatcher.callbackLimit = 10 * time.Millisecond
	for operationID := uint64(1); operationID <= 4; operationID++ {
		dispatcher.emit(LifecycleTrace{
			ChannelID: 31, OperationID: operationID,
			Operation: LifecycleOperationSend, Transition: LifecycleTransitionSendRejected,
			Disposition: framechannel.SendRejected, State: framechannel.Open,
			Terminal: LifecycleTerminalNone, Cause: LifecycleCauseCanceled,
		})
	}
	select {
	case <-entered:
	case <-time.After(unitTimeout):
		t.Fatal("observer callback did not begin")
	}
	completion := dispatcher.complete(context.Background())
	if completion.Drained || completion.Delivered != 0 ||
		completion.Loss.CallbackTimeout != 1 || completion.Loss.Undrained != 3 {
		t.Fatalf("completion = %+v", completion)
	}
	dispatcher.emit(LifecycleTrace{ChannelID: 31})
	select {
	case <-exited:
	case <-time.After(unitTimeout):
		t.Fatal("revoked WebRTC callback did not exit")
	}
	if calls.Load() != 1 {
		t.Fatalf("callbacks begun after completion = %d", calls.Load())
	}
	if committed.Load() != 0 {
		t.Fatalf("revoked callback committed %d late fact(s)", committed.Load())
	}
}

func TestWebRTCLifecycleCompletionAccountsObserverPanic(t *testing.T) {
	var calls atomic.Uint64
	dispatcher := newLifecycleTraceDispatcher(LifecycleTraceFunc(func(LifecycleTrace) {
		calls.Add(1)
		panic("observer defect")
	}))
	dispatcher.emit(LifecycleTrace{
		ChannelID: 32, OperationID: 1,
		Operation: LifecycleOperationSend, Transition: LifecycleTransitionSendRejected,
		Disposition: framechannel.SendRejected, State: framechannel.Open,
		Terminal: LifecycleTerminalNone, Cause: LifecycleCauseCanceled,
	})
	dispatcher.shutdown()
	completion := dispatcher.complete(context.Background())
	if !completion.Drained || completion.Delivered != 0 || calls.Load() != 1 ||
		completion.Loss.ObserverPanic != 1 || completion.Loss.Total() != 1 {
		t.Fatalf("panic completion = %+v", completion)
	}
}

func TestChannelPublishesOpenOnlyAfterMessageCapability(t *testing.T) {
	fake := newFakeDataChannel(pion.DataChannelStateConnecting)
	fake.maximumMessage = 0
	channel, err := newChannel(fake, defaultFlowControl)
	if err != nil {
		t.Fatalf("construct connecting channel: %v", err)
	}
	if channel.State() != framechannel.Connecting {
		t.Fatalf("initial state = %v, want Connecting", channel.State())
	}
	select {
	case <-channel.Opened():
		t.Fatal("Opened closed before Pion opened")
	default:
	}

	fake.open(framechannel.MaxFrameSize)
	waitOpened(t, channel)
	if channel.State() != framechannel.Open {
		t.Fatalf("opened state = %v, want Open", channel.State())
	}
	if fake.lowThreshold != defaultLowWaterBytes {
		t.Fatalf("low-water threshold = %d, want %d", fake.lowThreshold, defaultLowWaterBytes)
	}
	if err := channel.Close(); err != nil {
		t.Fatalf("close opened channel: %v", err)
	}

	for _, maximum := range []uint32{0, framechannel.MaxFrameSize - 1} {
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
	if channel.State() != framechannel.Closed {
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

		fake.markOpenWithoutCallback(framechannel.MaxFrameSize)
		fake.deliverBinary(framechannel.Frame{0x31})
		waitOpened(t, channel)
		if channel.State() != framechannel.Open {
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

		fake.deliverBinary(framechannel.Frame{0x32})
		fake.markOpenWithoutCallback(framechannel.MaxFrameSize)
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
		if channel.State() != framechannel.Closed {
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
	go func() { result <- channel.Send(context.Background(), framechannel.Frame{0x41}) }()
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
			results <- concurrentSendResult{marker: marker, err: channel.Send(context.Background(), framechannel.Frame{marker})}
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
