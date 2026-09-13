package v2peer

import (
	"context"
	"errors"
	"testing"
	"time"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/core/framechannel"
)

func TestSenderDataChannelCallbackRetainsFirstFrameBeforeAttemptDispatch(t *testing.T) {
	peer := newTestPeerConnection()
	channel := newTestPeerChannel()
	channel.receive = make(chan framechannel.Frame, 1)
	var deliver func(framechannel.Frame)
	factory := mustTestFactory(t, Config{
		DataChannels: DataChannelAdapterFunc(func(*pion.DataChannel) (PeerDataChannel, error) {
			deliver = func(frame framechannel.Frame) { channel.receive <- frame }
			return channel, nil
		}),
	})
	attempt := &peerAttempt{
		config: peerAttemptConfig{factory: factory},
		events: make(chan attemptEvent, 1),
	}
	t.Cleanup(attempt.closeInbox)
	execution := newAttemptExecution(attempt, context.Background(), peer)
	execution.registerCallbacks()

	// Pion starts reading immediately after this callback returns. Hold the
	// attempt dispatcher idle so scheduler speed cannot hide a missing receiver.
	peer.emitDataChannel(&pion.DataChannel{})
	if deliver == nil {
		t.Fatal("Pion callback returned before a receiver existed for the first frame")
	}
	const hello = "first-lane-hello"
	deliver(framechannel.Frame(hello))
	select {
	case frame := <-channel.Recv():
		if string(frame) != hello {
			t.Fatalf("first frame = %q, want %q", frame, hello)
		}
	default:
		t.Fatal("first frame was lost before attempt dispatch")
	}
	event := <-attempt.events
	if event.channel != channel || event.err != nil {
		t.Fatalf("queued channel = %T, error = %v", event.channel, event.err)
	}
	_ = event.channel.Close()
}

func TestSenderDataChannelHandoffClosesUnadoptedChannelsOnce(t *testing.T) {
	for _, boundary := range []string{"queued at stop", "callback after stop", "inbox overflow"} {
		t.Run(boundary, func(t *testing.T) {
			channel := newTestPeerChannel()
			factory := mustTestFactory(t, Config{
				DataChannels: DataChannelAdapterFunc(func(*pion.DataChannel) (PeerDataChannel, error) {
					return channel, nil
				}),
			})
			ctx, cancel := context.WithCancelCause(context.Background())
			t.Cleanup(func() { cancel(nil) })
			attempt := &peerAttempt{
				config: peerAttemptConfig{factory: factory},
				events: make(chan attemptEvent, 1),
				cancel: cancel,
			}
			switch boundary {
			case "callback after stop":
				attempt.closeInbox()
			case "inbox overflow":
				attempt.push(attemptEvent{kind: attemptRemoteCandidate})
			}
			attempt.receiveDataChannel(&pion.DataChannel{})
			attempt.closeInbox()
			attempt.closeInbox()
			if channel.closeCalls.Load() != 1 {
				t.Fatalf("unadopted channel closed %d times, want 1", channel.closeCalls.Load())
			}
			if len(attempt.events) != 0 {
				t.Fatal("closed attempt retained a DataChannel")
			}
			if boundary == "inbox overflow" && !errors.Is(context.Cause(ctx), ErrEventCapacity) {
				t.Fatalf("overflow cause = %v", context.Cause(ctx))
			}
		})
	}
}

func TestSenderDataChannelHandoffRejectsFailedAdapters(t *testing.T) {
	adapterErr := errors.New("callback installation failed")
	for _, outcome := range []string{"error", "partial channel and error", "nil channel"} {
		t.Run(outcome, func(t *testing.T) {
			raw := &pion.DataChannel{}
			var channel *testPeerChannel
			factory := mustTestFactory(t, Config{
				DataChannels: DataChannelAdapterFunc(func(*pion.DataChannel) (PeerDataChannel, error) {
					switch outcome {
					case "partial channel and error":
						channel = newTestPeerChannel()
						return channel, adapterErr
					case "nil channel":
						return nil, nil
					default:
						return nil, adapterErr
					}
				}),
			})
			attempt := &peerAttempt{
				config: peerAttemptConfig{factory: factory},
				events: make(chan attemptEvent, 1),
			}
			attempt.receiveDataChannel(raw)
			event := <-attempt.events
			if event.channel != nil || !errors.Is(event.err, errChannelAdmission) {
				t.Fatalf("failed adapter event = %#v", event)
			}
			if outcome != "nil channel" && !errors.Is(event.err, adapterErr) {
				t.Fatalf("adapter cause was lost: %v", event.err)
			}
			if channel == nil && raw.ReadyState() != pion.DataChannelStateClosing {
				t.Fatal("failed adapter retained its unwrapped Pion channel")
			}
			if channel != nil && channel.closeCalls.Load() != 1 {
				t.Fatal("failed adapter retained its partially initialized channel")
			}
			execution := newAttemptExecution(attempt, context.Background(), newTestPeerConnection())
			if _, err := execution.handleEvent(event); !errors.Is(err, errChannelAdmission) {
				t.Fatalf("adapter failure did not terminate admission: %v", err)
			}
		})
	}
}

type abandonedTestPeerChannel struct {
	*testPeerChannel
	closeStarted chan struct{}
	drained      <-chan struct{}
	onClose      func()
}

func (channel *abandonedTestPeerChannel) Close() error {
	close(channel.closeStarted)
	channel.onClose()
	<-channel.drained
	return channel.testPeerChannel.Close()
}

func TestSenderInboxDisposalDrainsUnadoptedFramesOutsideLock(t *testing.T) {
	attempt := &peerAttempt{events: make(chan attemptEvent, 1)}
	drained := make(chan struct{})
	channel := &abandonedTestPeerChannel{
		testPeerChannel: newTestPeerChannel(),
		closeStarted:    make(chan struct{}),
		drained:         drained,
		onClose: func() {
			// A synchronous transport callback must observe the closed inbox
			// without waiting behind the disposal that invoked it.
			attempt.push(attemptEvent{kind: attemptConnectionFailed})
		},
	}
	attempt.push(attemptEvent{kind: attemptDataChannel, channel: channel})
	go func() {
		channel.receive <- framechannel.Frame("unadopted-frame")
		close(drained)
	}()
	closed := make(chan struct{})
	go func() {
		attempt.closeInbox()
		close(closed)
	}()
	receiveTest(t, channel.closeStarted)
	select {
	case <-closed:
	case <-time.After(peerTestTimeout):
		t.Fatal("inbox disposal did not drain the unadopted channel and join Close")
	}
	if channel.closeCalls.Load() != 1 || len(attempt.events) != 0 {
		t.Fatal("inbox disposal retained a channel or accepted a shutdown callback")
	}
}

func TestSenderDataChannelCallbackRejectsDuplicateBeforeReceivingFrames(t *testing.T) {
	owner := newTestPeerChannel()
	wrapCalls := 0
	factory := mustTestFactory(t, Config{
		DataChannels: DataChannelAdapterFunc(func(*pion.DataChannel) (PeerDataChannel, error) {
			wrapCalls++
			return owner, nil
		}),
	})
	attempt := &peerAttempt{
		config: peerAttemptConfig{factory: factory},
		events: make(chan attemptEvent, 2),
	}
	t.Cleanup(attempt.closeInbox)
	attempt.receiveDataChannel(&pion.DataChannel{})
	duplicate := &pion.DataChannel{}
	attempt.receiveDataChannel(duplicate)
	// A duplicate never gains a FrameChannel receiver. Otherwise it can reserve
	// a remote terminal before dispatch and make the actor's Close wait forever.
	if wrapCalls != 1 || duplicate.ReadyState() != pion.DataChannelStateClosing {
		t.Fatal("duplicate was not rejected within Pion's callback barrier")
	}
	if owner.closeCalls.Load() != 0 {
		t.Fatal("duplicate DataChannel changed the existing transport owner")
	}
	first, second := <-attempt.events, <-attempt.events
	if first.channel != owner || !errors.Is(second.err, errChannelAdmission) || second.channel != nil {
		t.Fatalf("channel events first=%#v, second=%#v", first, second)
	}
	_ = owner.Close()
}
