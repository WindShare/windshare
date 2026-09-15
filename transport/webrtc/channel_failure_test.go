package webrtc

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/core/framechannel"
)

func TestPeerFailureCompletesWithoutDataChannelCallbacks(t *testing.T) {
	for _, state := range []pion.DataChannelState{pion.DataChannelStateConnecting, pion.DataChannelStateOpen} {
		t.Run(state.String(), func(t *testing.T) {
			fake := newFakeDataChannel(state)
			channel, err := newChannelWithRuntime(fake, defaultFlowControl, channelRuntime{
				lifecycleObservationCapacity: 16,
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = channel.Close() })
			// An ICE failure need not produce a DataChannel close/error callback.
			fake.OnClose(nil)
			failure := errors.New("owning peer failed")
			channel.Fail(failure)
			channel.Fail(errors.New("duplicate failure"))
			waitDone(t, channel)
			if channel.State() != framechannel.Closed || !errors.Is(channel.Err(), failure) ||
				!errors.Is(channel.Err(), ErrTransport) {
				t.Fatalf("channel state=%v error=%v", channel.State(), channel.Err())
			}
			if _, open := <-channel.Recv(); open {
				t.Fatal("failed channel retained its receiver")
			}
			if err := channel.Close(); err != nil || fake.closeCalls() != 1 {
				t.Fatalf("physical close calls=%d error=%v", fake.closeCalls(), err)
			}
			closed := 0
			for event := range channel.LifecycleTrace() {
				if violation := ValidateLifecycleTrace(event); violation != LifecycleContractValid {
					t.Fatalf("invalid failure trace: %+v", event)
				}
				if event.Transition == LifecycleTransitionClosedFailed {
					closed++
					if event.ChannelID == 0 || event.Cause != LifecycleCauseTransport {
						t.Fatalf("failure trace lost identity/cause: %+v", event)
					}
				}
			}
			if closed != 1 {
				t.Fatalf("failure terminal count=%d, want 1", closed)
			}
		})
	}
}

func TestPeerFailureWakesBlockedSends(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		name := "ordinary"
		if terminal {
			name = "terminal"
		}
		t.Run(name, func(t *testing.T) {
			flow := flowControlProfile{lowWaterBytes: 10, highWaterBytes: 20}
			fake, channel := openFakeChannel(t, flow)
			fake.setBuffered(flow.highWaterBytes)
			waiting := fake.observeBufferedReads()
			result := make(chan error, 1)
			go func() {
				if terminal {
					result <- channel.SendTerminal(context.Background(), framechannel.Frame{1})
				} else {
					result <- channel.Send(context.Background(), framechannel.Frame{1})
				}
			}()
			select {
			case <-waiting:
			case <-time.After(unitTimeout):
				t.Fatal("send did not reach backpressure")
			}
			channel.Fail(nil)
			err := receiveError(t, result)
			if !errors.Is(err, ErrTransport) {
				t.Fatalf("send lost transport failure: %v", err)
			}
			if terminal && !errors.Is(err, ErrTerminalNotAcknowledged) {
				t.Fatalf("terminal falsely acknowledged: %v", err)
			}
			waitDone(t, channel)
		})
	}
}

func TestPeerFailurePreservesBufferedRemoteTerminal(t *testing.T) {
	gate, release := newInboundGate(t)
	fake := newFakeDataChannel(pion.DataChannelStateOpen)
	channel, err := newChannelWithRuntime(fake, defaultFlowControl, channelRuntime{inboundGate: gate})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = channel.Close() })
	ordinary, terminal := framechannel.Frame{1}, framechannel.Frame{2}
	fake.deliverBinary(ordinary)
	fake.deliverText(terminalIntentControl)
	fake.deliverBinary(terminal)

	// Failure must freeze sends without waiting behind an accepted callback or
	// overtaking the final frame already retained by that callback.
	channel.callbackMu.Lock()
	returned := make(chan struct{})
	go func() {
		channel.Fail(errors.New("ICE failed with buffered ingress"))
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(unitTimeout):
		channel.callbackMu.Unlock()
		release()
		t.Fatal("peer failure blocked behind the DataChannel callback")
	}
	channel.callbackMu.Unlock()
	release()
	for index, want := range []framechannel.Frame{ordinary, terminal} {
		select {
		case got, open := <-channel.Recv():
			if !open || !bytes.Equal(got, want) {
				t.Fatalf("frame %d=%x open=%t, want %x", index, got, open, want)
			}
		case <-time.After(unitTimeout):
			t.Fatalf("frame %d was lost during failure", index)
		}
	}
	waitDone(t, channel)
	if !errors.Is(channel.Err(), ErrTransport) || errors.Is(channel.Err(), ErrPeerProtocol) {
		t.Fatalf("buffered terminal was misclassified: %v", channel.Err())
	}
}

func TestPeerFailurePreservesAcknowledgedRemoteTerminal(t *testing.T) {
	gate, release := newInboundGate(t)
	fake := newFakeDataChannel(pion.DataChannelStateOpen)
	channel, err := newChannelWithRuntime(fake, defaultFlowControl, channelRuntime{
		remoteTerminalFinishGate: gate,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = channel.Close() })
	fake.deliverText(terminalIntentControl)
	fake.deliverBinary(framechannel.Frame{1})
	select {
	case <-fake.terminalAck:
	case <-time.After(unitTimeout):
		release()
		t.Fatal("remote terminal was not acknowledged")
	}
	channel.Fail(errors.New("peer failed after acknowledgement"))
	release()
	waitDone(t, channel)
	if channel.Err() != nil {
		t.Fatalf("completed terminal became a failure: %v", channel.Err())
	}
	channel.Fail(errors.New("late peer failure"))
	if channel.Err() != nil {
		t.Fatalf("late failure changed completed terminal: %v", channel.Err())
	}
}
