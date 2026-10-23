package eventproject

import (
	"github.com/windshare/windshare/connectivity/v2peer"
	appobs "github.com/windshare/windshare/engine/observation"
	"github.com/windshare/windshare/transport/relayv2"
	wsrtc "github.com/windshare/windshare/transport/webrtc"
)

func ProjectPeerErrorCode(value v2peer.TypedPeerErrorCode) (appobs.Failure, bool) {
	var code appobs.FailureCode
	switch value {
	case v2peer.TypedPeerErrorNegotiation:
		code = appobs.FailurePeerNegotiation
	case v2peer.TypedPeerErrorTimeout:
		code = appobs.FailurePeerTimeout
	case v2peer.TypedPeerErrorCandidates:
		code = appobs.FailurePeerCandidates
	case v2peer.TypedPeerErrorAdmission:
		code = appobs.FailurePeerAdmission
	case v2peer.TypedPeerErrorPolicy:
		code = appobs.FailurePeerPolicy
	case v2peer.TypedPeerErrorBusy:
		code = appobs.FailurePeerBusy
	case v2peer.TypedPeerErrorSignaling:
		code = appobs.FailurePeerSignaling
	case v2peer.TypedPeerErrorCancelled:
		code = appobs.FailurePeerCanceled
	case v2peer.TypedPeerErrorStopped:
		code = appobs.FailurePeerStopped
	case v2peer.TypedPeerErrorUnexpected:
		code = appobs.FailureUnexpected
	default:
		return appobs.Failure{}, false
	}
	return mustFailure(code), true
}

func ProjectReceiverCauseClass(value v2peer.ReceiverCauseClass) (appobs.Failure, bool) {
	var code appobs.FailureCode
	switch value {
	case v2peer.ReceiverCauseRuntimeClosed:
		code = appobs.FailurePeerStopped
	case v2peer.ReceiverCauseConfiguration:
		code = appobs.FailurePeerConfiguration
	case v2peer.ReceiverCauseOperationMissing:
		code = appobs.FailurePeerOperationMissing
	case v2peer.ReceiverCauseNegotiationTimeout, v2peer.ReceiverCauseAdmissionTimeout:
		code = appobs.FailurePeerTimeout
	case v2peer.ReceiverCauseCandidateLimit:
		code = appobs.FailurePeerCandidates
	case v2peer.ReceiverCauseChannelAdmission:
		code = appobs.FailurePeerAdmission
	case v2peer.ReceiverCauseEventCapacity:
		code = appobs.FailurePeerEventCapacity
	case v2peer.ReceiverCauseNegotiation:
		code = appobs.FailurePeerNegotiation
	case v2peer.ReceiverCauseProtocol:
		code = appobs.FailurePeerProtocol
	case v2peer.ReceiverCauseDeadline:
		code = appobs.FailureDeadline
	case v2peer.ReceiverCausePeerShutdown:
		code = appobs.FailurePeerShutdown
	case v2peer.ReceiverCauseChannelDrain:
		code = appobs.FailurePeerChannelDrain
	case v2peer.ReceiverCauseUnknown:
		code = appobs.FailureUnexpected
	default:
		return appobs.Failure{}, false
	}
	return mustFailure(code), true
}

func ProjectRelayLifecycleCause(value relayv2.LifecycleCause) (appobs.Failure, bool) {
	switch value {
	case relayv2.LifecycleCauseNone:
		return appobs.Failure{}, false
	case relayv2.LifecycleCauseCanceled:
		return mustFailure(appobs.FailureCanceled), true
	case relayv2.LifecycleCauseDeadline:
		return mustFailure(appobs.FailureDeadline), true
	case relayv2.LifecycleCauseFrameBounds, relayv2.LifecycleCauseProtocol:
		return mustFailure(appobs.FailureRelayProtocol), true
	case relayv2.LifecycleCauseEgressOverflow, relayv2.LifecycleCauseIngressOverflow:
		return mustFailure(appobs.FailureRelayOverflow), true
	case relayv2.LifecycleCauseSessionRetired, relayv2.LifecycleCauseClosed:
		return mustFailure(appobs.FailureRelayClosed), true
	case relayv2.LifecycleCauseTransport:
		return mustFailure(appobs.FailureRelayTransport), true
	default:
		return appobs.Failure{}, false
	}
}

func ProjectWebRTCLifecycleCause(value wsrtc.LifecycleCause) (appobs.Failure, bool) {
	switch value {
	case wsrtc.LifecycleCauseNone:
		return appobs.Failure{}, false
	case wsrtc.LifecycleCauseCanceled:
		return mustFailure(appobs.FailureCanceled), true
	case wsrtc.LifecycleCauseDeadline:
		return mustFailure(appobs.FailureDeadline), true
	case wsrtc.LifecycleCauseNotOpen, wsrtc.LifecycleCauseNaturalRetirement, wsrtc.LifecycleCauseRemoteClosed:
		return mustFailure(appobs.FailurePeerStopped), true
	case wsrtc.LifecycleCauseTerminalUnacknowledged, wsrtc.LifecycleCausePeerProtocol:
		return mustFailure(appobs.FailurePeerProtocol), true
	case wsrtc.LifecycleCauseTransport:
		return mustFailure(appobs.FailurePeerNegotiation), true
	case wsrtc.LifecycleCauseOther:
		return mustFailure(appobs.FailureUnexpected), true
	default:
		return appobs.Failure{}, false
	}
}
