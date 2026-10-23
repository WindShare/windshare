package eventproject

import (
	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/session/protocolsession"
	appobs "github.com/windshare/windshare/engine/observation"
	"github.com/windshare/windshare/transport/relayv2"
	wsrtc "github.com/windshare/windshare/transport/webrtc"
)

func projectRelayStage(value relayv2.LifecycleStage) (appobs.RelayLifecycleStage, bool) {
	switch value {
	case relayv2.LifecycleTerminalReserved:
		return appobs.RelayTerminalReserved, true
	case relayv2.LifecycleSendAdmitted:
		return appobs.RelaySendAdmitted, true
	case relayv2.LifecycleSendRejected:
		return appobs.RelaySendRejected, true
	case relayv2.LifecycleSendRolledBack:
		return appobs.RelaySendRolledBack, true
	case relayv2.LifecycleRetirementDeferred:
		return appobs.RelayRetirementDeferred, true
	case relayv2.LifecycleRetired:
		return appobs.RelayRetired, true
	case relayv2.LifecycleTerminalSettled:
		return appobs.RelayTerminalSettled, true
	case relayv2.LifecycleLinkRetiring:
		return appobs.RelayLinkRetiring, true
	case relayv2.LifecycleLinkClosed:
		return appobs.RelayLinkClosed, true
	case relayv2.LifecycleTraceDropped:
		return appobs.RelayTraceDropped, true
	case relayv2.LifecycleHeartbeatProbe:
		return appobs.RelayHeartbeatProbe, true
	case relayv2.LifecycleHeartbeatAcknowledged:
		return appobs.RelayHeartbeatAcknowledged, true
	case relayv2.LifecycleHeartbeatFailed:
		return appobs.RelayHeartbeatFailed, true
	default:
		return 0, false
	}
}

func projectOptionalDisposition(value framechannel.SendDisposition) (appobs.SendDisposition, bool) {
	switch value {
	case 0:
		return 0, true
	case framechannel.SendAccepted:
		return appobs.SendAccepted, true
	case framechannel.SendRejected:
		return appobs.SendRejected, true
	case framechannel.SendRetired:
		return appobs.SendRetired, true
	default:
		return 0, false
	}
}

func projectChannelState(value framechannel.ChannelState) (appobs.ChannelState, bool) {
	switch value {
	case framechannel.Connecting:
		return appobs.ChannelConnecting, true
	case framechannel.Open:
		return appobs.ChannelOpen, true
	case framechannel.Closed:
		return appobs.ChannelClosed, true
	default:
		return 0, false
	}
}

func projectRelayRetirement(value relayv2.LifecycleRetirementSource) (appobs.RelayRetirementSource, bool) {
	switch value {
	case relayv2.LifecycleRetirementNone:
		return appobs.RelayRetirementNone, true
	case relayv2.LifecycleRetirementLocalClose:
		return appobs.RelayRetirementLocalClose, true
	case relayv2.LifecycleRetirementTerminal:
		return appobs.RelayRetirementTerminal, true
	case relayv2.LifecycleRetirementRelaySession:
		return appobs.RelayRetirementSession, true
	case relayv2.LifecycleRetirementLinkClose:
		return appobs.RelayRetirementLinkClose, true
	case relayv2.LifecycleRetirementLinkFailure:
		return appobs.RelayRetirementLinkFailure, true
	case relayv2.LifecycleRetirementIngressFailure:
		return appobs.RelayRetirementIngressFailure, true
	default:
		return 0, false
	}
}

func projectRelayCause(value relayv2.LifecycleCause) (appobs.RelayLifecycleCause, bool) {
	switch value {
	case relayv2.LifecycleCauseNone:
		return appobs.RelayCauseNone, true
	case relayv2.LifecycleCauseCanceled:
		return appobs.RelayCauseCanceled, true
	case relayv2.LifecycleCauseDeadline:
		return appobs.RelayCauseDeadline, true
	case relayv2.LifecycleCauseFrameBounds:
		return appobs.RelayCauseFrameBounds, true
	case relayv2.LifecycleCauseEgressOverflow:
		return appobs.RelayCauseEgressOverflow, true
	case relayv2.LifecycleCauseIngressOverflow:
		return appobs.RelayCauseIngressOverflow, true
	case relayv2.LifecycleCauseSessionRetired:
		return appobs.RelayCauseSessionRetired, true
	case relayv2.LifecycleCauseProtocol:
		return appobs.RelayCauseProtocol, true
	case relayv2.LifecycleCauseClosed:
		return appobs.RelayCauseClosed, true
	case relayv2.LifecycleCauseTransport:
		return appobs.RelayCauseTransport, true
	default:
		return 0, false
	}
}

func projectWebRTCOperation(value wsrtc.LifecycleOperation) (appobs.WebRTCOperation, bool) {
	switch value {
	case wsrtc.LifecycleOperationChannel:
		return appobs.WebRTCChannel, true
	case wsrtc.LifecycleOperationSend:
		return appobs.WebRTCSend, true
	case wsrtc.LifecycleOperationSendTerminal:
		return appobs.WebRTCSendTerminal, true
	default:
		return 0, false
	}
}

func projectWebRTCTransition(value wsrtc.LifecycleTransition) (appobs.WebRTCTransition, bool) {
	switch value {
	case wsrtc.LifecycleTransitionSendAccepted:
		return appobs.WebRTCSendAccepted, true
	case wsrtc.LifecycleTransitionSendRejected:
		return appobs.WebRTCSendRejected, true
	case wsrtc.LifecycleTransitionSendRetired:
		return appobs.WebRTCSendRetired, true
	case wsrtc.LifecycleTransitionRemoteTerminalReserved:
		return appobs.WebRTCRemoteTerminalReserved, true
	case wsrtc.LifecycleTransitionTerminationPending:
		return appobs.WebRTCTerminationPending, true
	case wsrtc.LifecycleTransitionClosedClean:
		return appobs.WebRTCClosedClean, true
	case wsrtc.LifecycleTransitionClosedFailed:
		return appobs.WebRTCClosedFailed, true
	case wsrtc.LifecycleTransitionTraceDropped:
		return appobs.WebRTCTraceDropped, true
	default:
		return 0, false
	}
}

func projectWebRTCTerminal(value wsrtc.LifecycleTerminalState) (appobs.WebRTCTerminalState, bool) {
	switch value {
	case wsrtc.LifecycleTerminalNone:
		return appobs.WebRTCTerminalNone, true
	case wsrtc.LifecycleTerminalLocalPending:
		return appobs.WebRTCTerminalLocalPending, true
	case wsrtc.LifecycleTerminalRemotePending:
		return appobs.WebRTCTerminalRemotePending, true
	default:
		return 0, false
	}
}

func projectWebRTCCause(value wsrtc.LifecycleCause) (appobs.WebRTCLifecycleCause, bool) {
	switch value {
	case wsrtc.LifecycleCauseNone:
		return appobs.WebRTCCauseNone, true
	case wsrtc.LifecycleCauseCanceled:
		return appobs.WebRTCCauseCanceled, true
	case wsrtc.LifecycleCauseDeadline:
		return appobs.WebRTCCauseDeadline, true
	case wsrtc.LifecycleCauseNotOpen:
		return appobs.WebRTCCauseNotOpen, true
	case wsrtc.LifecycleCauseNaturalRetirement:
		return appobs.WebRTCCauseNaturalRetirement, true
	case wsrtc.LifecycleCauseRemoteClosed:
		return appobs.WebRTCCauseRemoteClosed, true
	case wsrtc.LifecycleCauseTerminalUnacknowledged:
		return appobs.WebRTCCauseTerminalUnacknowledged, true
	case wsrtc.LifecycleCausePeerProtocol:
		return appobs.WebRTCCausePeerProtocol, true
	case wsrtc.LifecycleCauseTransport:
		return appobs.WebRTCCauseTransport, true
	case wsrtc.LifecycleCauseOther:
		return appobs.WebRTCCauseOther, true
	default:
		return 0, false
	}
}

var peerStageProjections = map[v2peer.SenderAttemptStage]appobs.PeerAttemptStage{
	v2peer.SenderAttemptStarted:                    appobs.PeerAttemptStarted,
	v2peer.SenderAttemptNegotiationDeadlineArmed:   appobs.PeerNegotiationDeadlineArmed,
	v2peer.SenderAttemptNegotiationDeadlineExpired: appobs.PeerNegotiationDeadlineExpired,
	v2peer.SenderAttemptOfferReceived:              appobs.PeerOfferReceived,
	v2peer.SenderAttemptAnswerCreated:              appobs.PeerAnswerCreated,
	v2peer.SenderAttemptAnswerSent:                 appobs.PeerAnswerSent,
	v2peer.SenderAttemptDataChannelOpen:            appobs.PeerDataChannelOpen,
	v2peer.SenderAttemptAdmissionDeadlineArmed:     appobs.PeerAdmissionDeadlineArmed,
	v2peer.SenderAttemptAdmissionDeadlineExpired:   appobs.PeerAdmissionDeadlineExpired,
	v2peer.SenderAttemptLaneHelloAuthenticated:     appobs.PeerLaneHelloAuthenticated,
	v2peer.SenderAttemptAdmissionResponseSettled:   appobs.PeerAdmissionResponseSettled,
	v2peer.SenderAttemptAdmitted:                   appobs.PeerAttemptAdmitted,
	v2peer.SenderAttemptFailed:                     appobs.PeerAttemptFailed,
}

var peerPhaseProjections = map[v2peer.SenderAttemptPhase]appobs.PeerAttemptPhase{
	v2peer.SenderAttemptPhaseNegotiation: appobs.PeerPhaseNegotiation,
	v2peer.SenderAttemptPhaseAdmission:   appobs.PeerPhaseAdmission,
}

var peerAdmissionDispositionProjections = map[v2peer.SenderAdmissionDisposition]appobs.PeerAdmissionDisposition{
	v2peer.SenderAdmissionAccepted: appobs.PeerAdmissionAccepted,
	v2peer.SenderAdmissionRejected: appobs.PeerAdmissionRejected,
}

var peerResponseDeliveryProjections = map[v2peer.SenderResponseDelivery]appobs.PeerResponseDelivery{
	v2peer.SenderResponseDelivered:      appobs.PeerResponseDelivered,
	v2peer.SenderResponseDeliveryFailed: appobs.PeerResponseDeliveryFailed,
}

var peerLaneRejectionProjections = map[protocolsession.LaneRejectCode]appobs.PeerLaneRejectionCode{
	protocolsession.LaneRejectUnknownSession:   appobs.PeerLaneRejectUnknownSession,
	protocolsession.LaneRejectStaleEpoch:       appobs.PeerLaneRejectStaleEpoch,
	protocolsession.LaneRejectGrantConsumed:    appobs.PeerLaneRejectGrantConsumed,
	protocolsession.LaneRejectGrantExpired:     appobs.PeerLaneRejectGrantExpired,
	protocolsession.LaneRejectAdmissionLimited: appobs.PeerLaneRejectAdmissionLimited,
	protocolsession.LaneRejectStopping:         appobs.PeerLaneRejectStopping,
	protocolsession.LaneRejectGrantMismatch:    appobs.PeerLaneRejectGrantMismatch,
}

var peerFailureScopeProjections = map[v2peer.AttemptFailureScope]appobs.PeerFailureScope{
	v2peer.AttemptFailureScopeAttempt: appobs.PeerFailureAttempt,
	v2peer.AttemptFailureScopeSession: appobs.PeerFailureSession,
}

func projectPeerStage(value v2peer.SenderAttemptStage) (appobs.PeerAttemptStage, bool) {
	projected, ok := peerStageProjections[value]
	return projected, ok
}

func projectPeerPhase(value v2peer.SenderAttemptPhase) (appobs.PeerAttemptPhase, bool) {
	projected, ok := peerPhaseProjections[value]
	return projected, ok
}

func projectPeerAdmissionDisposition(value v2peer.SenderAdmissionDisposition) (appobs.PeerAdmissionDisposition, bool) {
	projected, ok := peerAdmissionDispositionProjections[value]
	return projected, ok
}

func projectPeerResponseDelivery(value v2peer.SenderResponseDelivery) (appobs.PeerResponseDelivery, bool) {
	projected, ok := peerResponseDeliveryProjections[value]
	return projected, ok
}

func projectPeerLaneRejection(value protocolsession.LaneRejectCode) (appobs.PeerLaneRejectionCode, bool) {
	projected, ok := peerLaneRejectionProjections[value]
	return projected, ok
}

func projectPeerFailureScope(value v2peer.AttemptFailureScope) (appobs.PeerFailureScope, bool) {
	projected, ok := peerFailureScopeProjections[value]
	return projected, ok
}
