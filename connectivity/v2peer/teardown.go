package v2peer

import "errors"

var (
	errPeerShutdown = errors.New("v2 peer connection shutdown failed")
	errChannelDrain = errors.New("v2 peer channel drain failed")
)

// PeerTeardownTransition is an ordered, text-free lifecycle milestone suitable
// for correlating teardown with an attempt's stable signaling identity.
type PeerTeardownTransition string

const (
	PeerTeardownPeerShutdownInitiated PeerTeardownTransition = "peer_shutdown_initiated"
	PeerTeardownPeerShutdownReturned  PeerTeardownTransition = "peer_shutdown_returned"
	PeerTeardownChannelDrainStarted   PeerTeardownTransition = "channel_drain_started"
	PeerTeardownChannelDrainJoined    PeerTeardownTransition = "channel_drain_joined"
)

type peerCloseOwner interface {
	Close() error
}

type peerTransportTeardown struct {
	transitions       []PeerTeardownTransition
	peerShutdownError error
	channelDrainError error
}

func teardownPeerTransport(
	peer peerCloseOwner,
	channel peerCloseOwner,
) peerTransportTeardown {
	var teardown peerTransportTeardown
	if peer != nil {
		teardown.transitions = append(teardown.transitions, PeerTeardownPeerShutdownInitiated)
		teardown.peerShutdownError = peer.Close()
		teardown.transitions = append(teardown.transitions, PeerTeardownPeerShutdownReturned)
	}
	if channel != nil {
		// The PeerConnection owns physical shutdown while the FrameChannel owns
		// logical drain. Initiating the owner first breaks a cycle when Close is
		// waiting for a peer callback; joining the channel afterward still keeps
		// finalization inside the attempt's exact completion barrier.
		teardown.transitions = append(teardown.transitions, PeerTeardownChannelDrainStarted)
		teardown.channelDrainError = channel.Close()
		teardown.transitions = append(teardown.transitions, PeerTeardownChannelDrainJoined)
	}
	return teardown
}

func (teardown peerTransportTeardown) cause() error {
	var peerFailure error
	if teardown.peerShutdownError != nil {
		peerFailure = errors.Join(errPeerShutdown, teardown.peerShutdownError)
	}
	var channelFailure error
	if teardown.channelDrainError != nil {
		channelFailure = errors.Join(errChannelDrain, teardown.channelDrainError)
	}
	return errors.Join(peerFailure, channelFailure)
}

func (teardown peerTransportTeardown) transitionSnapshot() []PeerTeardownTransition {
	return append([]PeerTeardownTransition(nil), teardown.transitions...)
}

func (teardown peerTransportTeardown) peerShutdownFailed() bool {
	return teardown.peerShutdownError != nil
}

func (teardown peerTransportTeardown) channelDrainFailed() bool {
	return teardown.channelDrainError != nil
}
