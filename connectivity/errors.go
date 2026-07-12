package connectivity

import "errors"

var (
	ErrNilDependency          = errors.New("connectivity: dependency must not be nil")
	ErrSignalingClosed        = errors.New("connectivity: signaling channel is closed")
	ErrInvalidSignal          = errors.New("connectivity: signaling message is invalid")
	ErrUnexpectedSignal       = errors.New("connectivity: signaling message is unexpected")
	ErrUnexpectedDataChannel  = errors.New("connectivity: peer created an unexpected DataChannel")
	ErrCandidateLimitExceeded = errors.New("connectivity: ICE candidate limit exceeded")
	ErrPeerConnectionFailed   = errors.New("connectivity: peer connection failed")
	ErrReceiverPoolClosed     = errors.New("connectivity: receiver pool is closed")
	ErrReceiverSourceEnded    = errors.New("connectivity: receiver session source ended")
	ErrRelayRecoveryFailed    = errors.New("connectivity: relay recovery failed")
	ErrManifestIdentity       = errors.New("connectivity: rejoined manifest identity mismatch")
)
