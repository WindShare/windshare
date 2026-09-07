package commandprojection

import (
	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/transport/relayv2"
	wsrtc "github.com/windshare/windshare/transport/webrtc"
)

func ProjectPeerErrorCode(value v2peer.TypedPeerErrorCode) (clievent.Failure, bool) {
	var code clievent.FailureCode
	switch value {
	case v2peer.TypedPeerErrorNegotiation:
		code = clievent.FailurePeerNegotiation
	case v2peer.TypedPeerErrorTimeout:
		code = clievent.FailurePeerTimeout
	case v2peer.TypedPeerErrorCandidates:
		code = clievent.FailurePeerCandidates
	case v2peer.TypedPeerErrorAdmission:
		code = clievent.FailurePeerAdmission
	case v2peer.TypedPeerErrorPolicy:
		code = clievent.FailurePeerPolicy
	case v2peer.TypedPeerErrorBusy:
		code = clievent.FailurePeerBusy
	case v2peer.TypedPeerErrorSignaling:
		code = clievent.FailurePeerSignaling
	case v2peer.TypedPeerErrorCancelled:
		code = clievent.FailurePeerCanceled
	case v2peer.TypedPeerErrorStopped:
		code = clievent.FailurePeerStopped
	case v2peer.TypedPeerErrorUnexpected:
		code = clievent.FailureUnexpected
	default:
		return clievent.Failure{}, false
	}
	return mustFailure(code), true
}

func ProjectReceiverCauseClass(value v2peer.ReceiverCauseClass) (clievent.Failure, bool) {
	var code clievent.FailureCode
	switch value {
	case v2peer.ReceiverCauseRuntimeClosed:
		code = clievent.FailurePeerStopped
	case v2peer.ReceiverCauseConfiguration:
		code = clievent.FailurePeerConfiguration
	case v2peer.ReceiverCauseOperationMissing:
		code = clievent.FailurePeerOperationMissing
	case v2peer.ReceiverCauseNegotiationTimeout, v2peer.ReceiverCauseAdmissionTimeout:
		code = clievent.FailurePeerTimeout
	case v2peer.ReceiverCauseCandidateLimit:
		code = clievent.FailurePeerCandidates
	case v2peer.ReceiverCauseChannelAdmission:
		code = clievent.FailurePeerAdmission
	case v2peer.ReceiverCauseEventCapacity:
		code = clievent.FailurePeerEventCapacity
	case v2peer.ReceiverCauseNegotiation:
		code = clievent.FailurePeerNegotiation
	case v2peer.ReceiverCauseProtocol:
		code = clievent.FailurePeerProtocol
	case v2peer.ReceiverCauseDeadline:
		code = clievent.FailureDeadline
	case v2peer.ReceiverCausePeerShutdown:
		code = clievent.FailurePeerShutdown
	case v2peer.ReceiverCauseChannelDrain:
		code = clievent.FailurePeerChannelDrain
	case v2peer.ReceiverCauseUnknown:
		code = clievent.FailureUnexpected
	default:
		return clievent.Failure{}, false
	}
	return mustFailure(code), true
}

func ProjectRelayLifecycleCause(value relayv2.LifecycleCause) (clievent.Failure, bool) {
	switch value {
	case relayv2.LifecycleCauseNone:
		return clievent.Failure{}, false
	case relayv2.LifecycleCauseCanceled:
		return mustFailure(clievent.FailureCanceled), true
	case relayv2.LifecycleCauseDeadline:
		return mustFailure(clievent.FailureDeadline), true
	case relayv2.LifecycleCauseFrameBounds, relayv2.LifecycleCauseProtocol:
		return mustFailure(clievent.FailureRelayProtocol), true
	case relayv2.LifecycleCauseEgressOverflow, relayv2.LifecycleCauseIngressOverflow:
		return mustFailure(clievent.FailureRelayOverflow), true
	case relayv2.LifecycleCauseSessionRetired, relayv2.LifecycleCauseClosed:
		return mustFailure(clievent.FailureRelayClosed), true
	case relayv2.LifecycleCauseTransport:
		return mustFailure(clievent.FailureRelayTransport), true
	default:
		return clievent.Failure{}, false
	}
}

func ProjectWebRTCLifecycleCause(value wsrtc.LifecycleCause) (clievent.Failure, bool) {
	switch value {
	case wsrtc.LifecycleCauseNone:
		return clievent.Failure{}, false
	case wsrtc.LifecycleCauseCanceled:
		return mustFailure(clievent.FailureCanceled), true
	case wsrtc.LifecycleCauseDeadline:
		return mustFailure(clievent.FailureDeadline), true
	case wsrtc.LifecycleCauseNotOpen, wsrtc.LifecycleCauseNaturalRetirement, wsrtc.LifecycleCauseRemoteClosed:
		return mustFailure(clievent.FailurePeerStopped), true
	case wsrtc.LifecycleCauseTerminalUnacknowledged, wsrtc.LifecycleCausePeerProtocol:
		return mustFailure(clievent.FailurePeerProtocol), true
	case wsrtc.LifecycleCauseTransport:
		return mustFailure(clievent.FailurePeerNegotiation), true
	case wsrtc.LifecycleCauseOther:
		return mustFailure(clievent.FailureUnexpected), true
	default:
		return clievent.Failure{}, false
	}
}
