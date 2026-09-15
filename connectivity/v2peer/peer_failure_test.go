package v2peer

import (
	"context"
	"errors"
	"sync"
	"testing"

	pion "github.com/pion/webrtc/v4"
	"github.com/windshare/windshare/core/framechannel"
)

func TestPeerChannelFailureSurvivesBindingOrder(t *testing.T) {
	for _, order := range []string{"failure first", "channel first", "concurrent"} {
		t.Run(order, func(t *testing.T) {
			var failure peerChannelFailure
			channel := newTestPeerChannel()
			switch order {
			case "failure first":
				failure.fail(errPeerConnectionFailed)
				failure.bind(channel)
			case "channel first":
				failure.bind(channel)
				failure.fail(errPeerConnectionFailed)
			case "concurrent":
				var work sync.WaitGroup
				work.Add(2)
				go func() { defer work.Done(); failure.bind(channel) }()
				go func() { defer work.Done(); failure.fail(errPeerConnectionFailed) }()
				work.Wait()
			}
			failure.fail(errors.New("duplicate failure"))
			receiveTest(t, channel.Done())
			if !errors.Is(channel.Err(), errPeerConnectionFailed) {
				t.Fatalf("first failure was lost: %v", channel.Err())
			}
		})
	}
}

func TestSenderPeerFailureRetiresAdmittedChannelAfterEventLoopExit(t *testing.T) {
	for _, stopLoop := range []bool{false, true} {
		name := "active attempt"
		if stopLoop {
			name = "event loop already stopped"
		}
		t.Run(name, func(t *testing.T) {
			peer := newTestPeerConnection()
			channel := newTestPeerChannel()
			session := newTestPeerSession(13)
			factory := mustTestFactory(t, Config{})
			attempt := newPeerAttempt(peerAttemptConfig{
				factory: factory, session: session,
				offer: v2signalOffer(testBinding(101)),
			})
			ctx, cancel := context.WithCancelCause(context.Background())
			attempt.cancel = cancel
			phaseContext, err := attempt.phases.beginNegotiation(ctx)
			if err != nil {
				t.Fatal(err)
			}
			execution := newAttemptExecution(attempt, ctx, peer)
			execution.phaseContext = phaseContext
			execution.registerCallbacks()
			if err := execution.startDataChannel(channel); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				cancel(context.Canceled)
				_ = execution.transport.Close()
				execution.children.Wait()
			})
			result := make(chan error, 1)
			go func() { result <- execution.runEvents() }()
			owner := receiveTest(t, session.ownedAdmissions)
			waitForTest(t, func() bool { return attempt.attached.Load() })

			peer.mu.Lock()
			stateChanged := peer.onState
			peer.mu.Unlock()
			stateChanged(pion.PeerConnectionStateDisconnected)
			stateChanged(pion.PeerConnectionStateConnected)
			if channel.State() != framechannel.Open {
				t.Fatal("transient disconnection retired an admitted channel")
			}

			// Core owns physical Close after the receive side retires. The peer
			// failure must wake that owner without depending on an attempt event.
			coreClosed := make(chan error, 1)
			go func() {
				<-owner.Recv()
				coreClosed <- owner.Close()
			}()
			cleanup := make(chan error, 1)
			if stopLoop {
				cancel(context.Canceled)
				err := receiveTest(t, result)
				go func() { cleanup <- execution.close(err) }()
			}
			stateChanged(pion.PeerConnectionStateFailed)
			if !stopLoop {
				err := receiveTest(t, result)
				go func() { cleanup <- execution.close(err) }()
			}
			if err := receiveTest(t, cleanup); err != nil {
				t.Fatalf("attempt cleanup: %v", err)
			}
			if err := receiveTest(t, coreClosed); err != nil {
				t.Fatalf("core channel cleanup: %v", err)
			}
			if !errors.Is(channel.Err(), errPeerConnectionFailed) ||
				peer.closeCalls.Load() != 1 || channel.closeCalls.Load() != 1 {
				t.Fatalf("retirement cause=%v peer closes=%d channel closes=%d",
					channel.Err(), peer.closeCalls.Load(), channel.closeCalls.Load())
			}
			if !attempt.attached.Load() {
				t.Fatal("transport failure rewrote successful admission")
			}
			attempt.finish(ctx, errPeerConnectionFailed, nil, false)
			select {
			case failure := <-session.failures:
				t.Fatalf("admitted lane emitted negotiation failure: %+v", failure)
			default:
			}
		})
	}
}

func TestReceiverPeerFailureRetiresChannelAfterAttemptCompletion(t *testing.T) {
	for _, stopAttempt := range []bool{false, true} {
		name := "active attempt"
		if stopAttempt {
			name = "completed attempt"
		}
		t.Run(name, func(t *testing.T) {
			harness := newReceiverHarness(t, nil)
			harness.answer(t)
			harness.openAndAwaitLane(t)
			t.Cleanup(func() {
				_ = harness.admittedOwner.Close()
				_ = harness.attempt.Close()
			})
			harness.peer.mu.Lock()
			stateChanged := harness.peer.onState
			harness.peer.mu.Unlock()
			stateChanged(pion.PeerConnectionStateDisconnected)
			stateChanged(pion.PeerConnectionStateConnected)
			if stopAttempt {
				if err := harness.attempt.Close(); err != nil {
					t.Fatalf("stop admitted attempt: %v", err)
				}
			}
			if harness.channel.State() != framechannel.Open {
				t.Fatal("signaling completion or transient disconnection closed a healthy lane")
			}
			coreClosed := make(chan error, 1)
			go func() {
				<-harness.admittedOwner.Recv()
				coreClosed <- harness.admittedOwner.Close()
			}()
			stateChanged(pion.PeerConnectionStateFailed)
			if err := receiveTest(t, coreClosed); err != nil {
				t.Fatalf("core channel cleanup: %v", err)
			}
			receiveTest(t, harness.attempt.Done())
			if !errors.Is(harness.channel.Err(), errPeerConnectionFailed) ||
				harness.channel.closeCalls.Load() != 1 {
				t.Fatalf("retirement cause=%v closes=%d",
					harness.channel.Err(), harness.channel.closeCalls.Load())
			}
		})
	}
}
