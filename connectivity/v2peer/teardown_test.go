package v2peer

import (
	"errors"
	"reflect"
	"testing"
)

type teardownCloseFunc func() error

func (function teardownCloseFunc) Close() error { return function() }

func TestPeerTransportTeardownOrdersOwnershipAndRetainsStageFailures(t *testing.T) {
	peerFailure := errors.New("peer close failed")
	channelFailure := errors.New("channel drain failed")
	var order []string
	teardown := teardownPeerTransport(
		teardownCloseFunc(func() error {
			order = append(order, "peer")
			return peerFailure
		}),
		teardownCloseFunc(func() error {
			order = append(order, "channel")
			return channelFailure
		}),
	)

	if !reflect.DeepEqual(order, []string{"peer", "channel"}) {
		t.Fatalf("teardown order=%v", order)
	}
	if !reflect.DeepEqual(teardown.transitionSnapshot(), expectedPeerTeardownTransitions) {
		t.Fatalf("teardown transitions=%v", teardown.transitionSnapshot())
	}
	if cause := teardown.cause(); !errors.Is(cause, errPeerShutdown) ||
		!errors.Is(cause, peerFailure) || !errors.Is(cause, errChannelDrain) ||
		!errors.Is(cause, channelFailure) {
		t.Fatalf("teardown cause=%v", cause)
	}
	if !teardown.peerShutdownFailed() || !teardown.channelDrainFailed() {
		t.Fatalf("teardown failure flags=%+v", teardown)
	}
	classes := ReceiverCauseClasses(teardown.cause())
	if !containsReceiverCauseClass(classes, ReceiverCausePeerShutdown) ||
		!containsReceiverCauseClass(classes, ReceiverCauseChannelDrain) {
		t.Fatalf("teardown cause classes=%v", classes)
	}
}

func TestPeerTransportTeardownPreservesCompletedTerminalDrain(t *testing.T) {
	peerShutdown := make(chan struct{})
	terminalDrained := make(chan struct{})
	close(terminalDrained)
	teardown := teardownPeerTransport(
		teardownCloseFunc(func() error {
			close(peerShutdown)
			return nil
		}),
		teardownCloseFunc(func() error {
			select {
			case <-terminalDrained:
			default:
				t.Fatal("channel join overtook its completed terminal drain")
			}
			select {
			case <-peerShutdown:
			default:
				t.Fatal("channel join began before peer shutdown initiation")
			}
			return nil
		}),
	)

	if cause := teardown.cause(); cause != nil {
		t.Fatalf("completed terminal drain teardown=%v", cause)
	}
	if !reflect.DeepEqual(teardown.transitionSnapshot(), expectedPeerTeardownTransitions) {
		t.Fatalf("teardown transitions=%v", teardown.transitionSnapshot())
	}
}
