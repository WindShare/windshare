package commandprojection

import (
	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/transport/relayv2"
	wsrtc "github.com/windshare/windshare/transport/webrtc"
)

func projectRelayStage(value relayv2.LifecycleStage) (clievent.RelayLifecycleStage, bool) {
	switch value {
	case relayv2.LifecycleTerminalReserved:
		return clievent.RelayTerminalReserved, true
	case relayv2.LifecycleSendAdmitted:
		return clievent.RelaySendAdmitted, true
	case relayv2.LifecycleSendRejected:
		return clievent.RelaySendRejected, true
	case relayv2.LifecycleSendRolledBack:
		return clievent.RelaySendRolledBack, true
	case relayv2.LifecycleRetirementDeferred:
		return clievent.RelayRetirementDeferred, true
	case relayv2.LifecycleRetired:
		return clievent.RelayRetired, true
	case relayv2.LifecycleTerminalSettled:
		return clievent.RelayTerminalSettled, true
	case relayv2.LifecycleLinkRetiring:
		return clievent.RelayLinkRetiring, true
	case relayv2.LifecycleLinkClosed:
		return clievent.RelayLinkClosed, true
	case relayv2.LifecycleTraceDropped:
		return clievent.RelayTraceDropped, true
	default:
		return 0, false
	}
}

func projectOptionalDisposition(value framechannel.SendDisposition) (clievent.SendDisposition, bool) {
	switch value {
	case 0:
		return 0, true
	case framechannel.SendAccepted:
		return clievent.SendAccepted, true
	case framechannel.SendRejected:
		return clievent.SendRejected, true
	case framechannel.SendRetired:
		return clievent.SendRetired, true
	default:
		return 0, false
	}
}

func projectChannelState(value framechannel.ChannelState) (clievent.ChannelState, bool) {
	switch value {
	case framechannel.Connecting:
		return clievent.ChannelConnecting, true
	case framechannel.Open:
		return clievent.ChannelOpen, true
	case framechannel.Closed:
		return clievent.ChannelClosed, true
	default:
		return 0, false
	}
}

func projectRelayRetirement(value relayv2.LifecycleRetirementSource) (clievent.RelayRetirementSource, bool) {
	switch value {
	case relayv2.LifecycleRetirementNone:
		return clievent.RelayRetirementNone, true
	case relayv2.LifecycleRetirementLocalClose:
		return clievent.RelayRetirementLocalClose, true
	case relayv2.LifecycleRetirementTerminal:
		return clievent.RelayRetirementTerminal, true
	case relayv2.LifecycleRetirementRelaySession:
		return clievent.RelayRetirementSession, true
	case relayv2.LifecycleRetirementLinkClose:
		return clievent.RelayRetirementLinkClose, true
	case relayv2.LifecycleRetirementLinkFailure:
		return clievent.RelayRetirementLinkFailure, true
	case relayv2.LifecycleRetirementIngressFailure:
		return clievent.RelayRetirementIngressFailure, true
	default:
		return 0, false
	}
}

func projectRelayCause(value relayv2.LifecycleCause) (clievent.RelayLifecycleCause, bool) {
	switch value {
	case relayv2.LifecycleCauseNone:
		return clievent.RelayCauseNone, true
	case relayv2.LifecycleCauseCanceled:
		return clievent.RelayCauseCanceled, true
	case relayv2.LifecycleCauseDeadline:
		return clievent.RelayCauseDeadline, true
	case relayv2.LifecycleCauseFrameBounds:
		return clievent.RelayCauseFrameBounds, true
	case relayv2.LifecycleCauseEgressOverflow:
		return clievent.RelayCauseEgressOverflow, true
	case relayv2.LifecycleCauseIngressOverflow:
		return clievent.RelayCauseIngressOverflow, true
	case relayv2.LifecycleCauseSessionRetired:
		return clievent.RelayCauseSessionRetired, true
	case relayv2.LifecycleCauseProtocol:
		return clievent.RelayCauseProtocol, true
	case relayv2.LifecycleCauseClosed:
		return clievent.RelayCauseClosed, true
	case relayv2.LifecycleCauseTransport:
		return clievent.RelayCauseTransport, true
	default:
		return 0, false
	}
}

func projectWebRTCOperation(value wsrtc.LifecycleOperation) (clievent.WebRTCOperation, bool) {
	switch value {
	case wsrtc.LifecycleOperationChannel:
		return clievent.WebRTCChannel, true
	case wsrtc.LifecycleOperationSend:
		return clievent.WebRTCSend, true
	case wsrtc.LifecycleOperationSendTerminal:
		return clievent.WebRTCSendTerminal, true
	default:
		return 0, false
	}
}

func projectWebRTCTransition(value wsrtc.LifecycleTransition) (clievent.WebRTCTransition, bool) {
	switch value {
	case wsrtc.LifecycleTransitionSendAccepted:
		return clievent.WebRTCSendAccepted, true
	case wsrtc.LifecycleTransitionSendRejected:
		return clievent.WebRTCSendRejected, true
	case wsrtc.LifecycleTransitionSendRetired:
		return clievent.WebRTCSendRetired, true
	case wsrtc.LifecycleTransitionRemoteTerminalReserved:
		return clievent.WebRTCRemoteTerminalReserved, true
	case wsrtc.LifecycleTransitionTerminationPending:
		return clievent.WebRTCTerminationPending, true
	case wsrtc.LifecycleTransitionClosedClean:
		return clievent.WebRTCClosedClean, true
	case wsrtc.LifecycleTransitionClosedFailed:
		return clievent.WebRTCClosedFailed, true
	case wsrtc.LifecycleTransitionTraceDropped:
		return clievent.WebRTCTraceDropped, true
	default:
		return 0, false
	}
}

func projectWebRTCTerminal(value wsrtc.LifecycleTerminalState) (clievent.WebRTCTerminalState, bool) {
	switch value {
	case wsrtc.LifecycleTerminalNone:
		return clievent.WebRTCTerminalNone, true
	case wsrtc.LifecycleTerminalLocalPending:
		return clievent.WebRTCTerminalLocalPending, true
	case wsrtc.LifecycleTerminalRemotePending:
		return clievent.WebRTCTerminalRemotePending, true
	default:
		return 0, false
	}
}

func projectWebRTCCause(value wsrtc.LifecycleCause) (clievent.WebRTCLifecycleCause, bool) {
	switch value {
	case wsrtc.LifecycleCauseNone:
		return clievent.WebRTCCauseNone, true
	case wsrtc.LifecycleCauseCanceled:
		return clievent.WebRTCCauseCanceled, true
	case wsrtc.LifecycleCauseDeadline:
		return clievent.WebRTCCauseDeadline, true
	case wsrtc.LifecycleCauseNotOpen:
		return clievent.WebRTCCauseNotOpen, true
	case wsrtc.LifecycleCauseNaturalRetirement:
		return clievent.WebRTCCauseNaturalRetirement, true
	case wsrtc.LifecycleCauseRemoteClosed:
		return clievent.WebRTCCauseRemoteClosed, true
	case wsrtc.LifecycleCauseTerminalUnacknowledged:
		return clievent.WebRTCCauseTerminalUnacknowledged, true
	case wsrtc.LifecycleCausePeerProtocol:
		return clievent.WebRTCCausePeerProtocol, true
	case wsrtc.LifecycleCauseTransport:
		return clievent.WebRTCCauseTransport, true
	case wsrtc.LifecycleCauseOther:
		return clievent.WebRTCCauseOther, true
	default:
		return 0, false
	}
}

var peerStageProjections = map[v2peer.SenderAttemptStage]clievent.PeerAttemptStage{
	v2peer.SenderAttemptStarted:                    clievent.PeerAttemptStarted,
	v2peer.SenderAttemptNegotiationDeadlineArmed:   clievent.PeerNegotiationDeadlineArmed,
	v2peer.SenderAttemptNegotiationDeadlineExpired: clievent.PeerNegotiationDeadlineExpired,
	v2peer.SenderAttemptOfferReceived:              clievent.PeerOfferReceived,
	v2peer.SenderAttemptAnswerCreated:              clievent.PeerAnswerCreated,
	v2peer.SenderAttemptAnswerSent:                 clievent.PeerAnswerSent,
	v2peer.SenderAttemptDataChannelOpen:            clievent.PeerDataChannelOpen,
	v2peer.SenderAttemptAdmissionDeadlineArmed:     clievent.PeerAdmissionDeadlineArmed,
	v2peer.SenderAttemptAdmissionDeadlineExpired:   clievent.PeerAdmissionDeadlineExpired,
	v2peer.SenderAttemptLaneHelloAuthenticated:     clievent.PeerLaneHelloAuthenticated,
	v2peer.SenderAttemptAdmissionResponseSettled:   clievent.PeerAdmissionResponseSettled,
	v2peer.SenderAttemptAdmitted:                   clievent.PeerAttemptAdmitted,
	v2peer.SenderAttemptFailed:                     clievent.PeerAttemptFailed,
}

var peerPhaseProjections = map[v2peer.SenderAttemptPhase]clievent.PeerAttemptPhase{
	v2peer.SenderAttemptPhaseNegotiation: clievent.PeerPhaseNegotiation,
	v2peer.SenderAttemptPhaseAdmission:   clievent.PeerPhaseAdmission,
}

var peerAdmissionDispositionProjections = map[v2peer.SenderAdmissionDisposition]clievent.PeerAdmissionDisposition{
	v2peer.SenderAdmissionAccepted: clievent.PeerAdmissionAccepted,
	v2peer.SenderAdmissionRejected: clievent.PeerAdmissionRejected,
}

var peerResponseDeliveryProjections = map[v2peer.SenderResponseDelivery]clievent.PeerResponseDelivery{
	v2peer.SenderResponseDelivered:      clievent.PeerResponseDelivered,
	v2peer.SenderResponseDeliveryFailed: clievent.PeerResponseDeliveryFailed,
}

var peerLaneRejectionProjections = map[protocolsession.LaneRejectCode]clievent.PeerLaneRejectionCode{
	protocolsession.LaneRejectUnknownSession:   clievent.PeerLaneRejectUnknownSession,
	protocolsession.LaneRejectStaleEpoch:       clievent.PeerLaneRejectStaleEpoch,
	protocolsession.LaneRejectGrantConsumed:    clievent.PeerLaneRejectGrantConsumed,
	protocolsession.LaneRejectGrantExpired:     clievent.PeerLaneRejectGrantExpired,
	protocolsession.LaneRejectAdmissionLimited: clievent.PeerLaneRejectAdmissionLimited,
	protocolsession.LaneRejectStopping:         clievent.PeerLaneRejectStopping,
	protocolsession.LaneRejectGrantMismatch:    clievent.PeerLaneRejectGrantMismatch,
}

var peerFailureScopeProjections = map[v2peer.AttemptFailureScope]clievent.PeerFailureScope{
	v2peer.AttemptFailureScopeAttempt: clievent.PeerFailureAttempt,
	v2peer.AttemptFailureScopeSession: clievent.PeerFailureSession,
}

func projectPeerStage(value v2peer.SenderAttemptStage) (clievent.PeerAttemptStage, bool) {
	projected, ok := peerStageProjections[value]
	return projected, ok
}

func projectPeerPhase(value v2peer.SenderAttemptPhase) (clievent.PeerAttemptPhase, bool) {
	projected, ok := peerPhaseProjections[value]
	return projected, ok
}

func projectPeerAdmissionDisposition(value v2peer.SenderAdmissionDisposition) (clievent.PeerAdmissionDisposition, bool) {
	projected, ok := peerAdmissionDispositionProjections[value]
	return projected, ok
}

func projectPeerResponseDelivery(value v2peer.SenderResponseDelivery) (clievent.PeerResponseDelivery, bool) {
	projected, ok := peerResponseDeliveryProjections[value]
	return projected, ok
}

func projectPeerLaneRejection(value protocolsession.LaneRejectCode) (clievent.PeerLaneRejectionCode, bool) {
	projected, ok := peerLaneRejectionProjections[value]
	return projected, ok
}

func projectPeerFailureScope(value v2peer.AttemptFailureScope) (clievent.PeerFailureScope, bool) {
	projected, ok := peerFailureScopeProjections[value]
	return projected, ok
}
