package commandprojection

import (
	"github.com/windshare/windshare/cmd/windshare/internal/clievent"
	"github.com/windshare/windshare/connectivity/v2peer"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
	"github.com/windshare/windshare/core/transfer"
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

func projectProtocolRole(value protocolsession.Role) (clievent.ProtocolRole, bool) {
	switch value {
	case protocolsession.RoleReceiver:
		return clievent.ProtocolRoleReceiver, true
	case protocolsession.RoleSender:
		return clievent.ProtocolRoleSender, true
	default:
		return 0, false
	}
}

var protocolOperationStageProjections = map[sessionruntime.ProtocolOperationStage]clievent.ProtocolOperationStage{
	sessionruntime.ProtocolOperationReceiverCompleted:     clievent.ProtocolOperationReceiverCompleted,
	sessionruntime.ProtocolOperationReceiverFailed:        clievent.ProtocolOperationReceiverFailed,
	sessionruntime.ProtocolOperationReceiverEnded:         clievent.ProtocolOperationReceiverEnded,
	sessionruntime.ProtocolOperationSenderRequestReceived: clievent.ProtocolOperationSenderRequestReceived,
	sessionruntime.ProtocolOperationSenderResponseSettled: clievent.ProtocolOperationSenderResponseSettled,
}

var protocolMessageKindProjections = map[protocolsession.MessageKind]clievent.ProtocolMessageKind{
	protocolsession.MessageListChildren:      clievent.ProtocolMessageListChildren,
	protocolsession.MessageCatalogResult:     clievent.ProtocolMessageCatalogResult,
	protocolsession.MessageOpenRevisions:     clievent.ProtocolMessageOpenRevisions,
	protocolsession.MessageOpenResults:       clievent.ProtocolMessageOpenResults,
	protocolsession.MessageRenewLease:        clievent.ProtocolMessageRenewLease,
	protocolsession.MessageReleaseLease:      clievent.ProtocolMessageReleaseLease,
	protocolsession.MessageRequestBlocks:     clievent.ProtocolMessageRequestBlocks,
	protocolsession.MessageBlockFragment:     clievent.ProtocolMessageBlockFragment,
	protocolsession.MessageCancel:            clievent.ProtocolMessageCancel,
	protocolsession.MessageOperationError:    clievent.ProtocolMessageOperationError,
	protocolsession.MessageSessionTerminal:   clievent.ProtocolMessageSessionTerminal,
	protocolsession.MessageLaneAttach:        clievent.ProtocolMessageLaneAttach,
	protocolsession.MessageScanProgress:      clievent.ProtocolMessageScanProgress,
	protocolsession.MessageOperationComplete: clievent.ProtocolMessageOperationComplete,
	protocolsession.MessageLeaseResult:       clievent.ProtocolMessageLeaseResult,
	protocolsession.MessagePeerOffer:         clievent.ProtocolMessagePeerOffer,
	protocolsession.MessagePeerAnswer:        clievent.ProtocolMessagePeerAnswer,
	protocolsession.MessagePeerCandidate:     clievent.ProtocolMessagePeerCandidate,
}

var protocolSendOutcomeProjections = map[protocolsession.SendOutcome]clievent.ProtocolSendOutcome{
	protocolsession.SendOutcomeUnknown:   clievent.ProtocolSendUnknown,
	protocolsession.SendOutcomeDelivered: clievent.ProtocolSendDelivered,
	protocolsession.SendOutcomeDropped:   clievent.ProtocolSendDropped,
}

var protocolOperationCauseProjections = map[sessionruntime.ProtocolOperationCause]clievent.ProtocolOperationCause{
	sessionruntime.ProtocolOperationCauseNone:            clievent.ProtocolOperationCauseNone,
	sessionruntime.ProtocolOperationCauseCanceled:        clievent.ProtocolOperationCauseCanceled,
	sessionruntime.ProtocolOperationCauseDeadline:        clievent.ProtocolOperationCauseDeadline,
	sessionruntime.ProtocolOperationCauseRuntimeClosed:   clievent.ProtocolOperationCauseRuntimeClosed,
	sessionruntime.ProtocolOperationCauseLaneUnavailable: clievent.ProtocolOperationCauseLaneUnavailable,
	sessionruntime.ProtocolOperationCauseWriterStopped:   clievent.ProtocolOperationCauseWriterStopped,
	sessionruntime.ProtocolOperationCauseOperationClosed: clievent.ProtocolOperationCauseOperationClosed,
	sessionruntime.ProtocolOperationCauseProtocolFailure: clievent.ProtocolOperationCauseProtocolFailure,
}

var protocolOperationErrorScopeProjections = map[sessionruntime.ProtocolOperationErrorScope]clievent.ProtocolOperationErrorScope{
	sessionruntime.ProtocolOperationErrorDirectory: clievent.ProtocolOperationErrorDirectory,
	sessionruntime.ProtocolOperationErrorRevision:  clievent.ProtocolOperationErrorRevision,
	sessionruntime.ProtocolOperationErrorBlock:     clievent.ProtocolOperationErrorBlock,
	sessionruntime.ProtocolOperationErrorPeer:      clievent.ProtocolOperationErrorPeer,
}

func projectProtocolOperationStage(value sessionruntime.ProtocolOperationStage) (clievent.ProtocolOperationStage, bool) {
	projected, ok := protocolOperationStageProjections[value]
	return projected, ok
}

func projectProtocolMessageKind(value protocolsession.MessageKind) (clievent.ProtocolMessageKind, bool) {
	projected, ok := protocolMessageKindProjections[value]
	return projected, ok
}

func projectProtocolSendOutcome(value protocolsession.SendOutcome) (clievent.ProtocolSendOutcome, bool) {
	projected, ok := protocolSendOutcomeProjections[value]
	return projected, ok
}

func projectProtocolOperationCause(value sessionruntime.ProtocolOperationCause) (clievent.ProtocolOperationCause, bool) {
	projected, ok := protocolOperationCauseProjections[value]
	return projected, ok
}

func projectProtocolOperationErrorScope(
	value sessionruntime.ProtocolOperationErrorScope,
) (clievent.ProtocolOperationErrorScope, bool) {
	projected, ok := protocolOperationErrorScopeProjections[value]
	return projected, ok
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

func projectTransferStage(value transfer.TransferLifecycleStage) (clievent.TransferLifecycleStage, bool) {
	switch value {
	case transfer.TransferDiscoveryStarted:
		return clievent.TransferDiscoveryStarted, true
	case transfer.TransferGenerationCommitted:
		return clievent.TransferGenerationCommitted, true
	case transfer.TransferDiscoveryCompleted:
		return clievent.TransferDiscoveryCompleted, true
	case transfer.TransferAdmissionStarted:
		return clievent.TransferAdmissionStarted, true
	case transfer.TransferAdmissionCompleted:
		return clievent.TransferAdmissionCompleted, true
	case transfer.TransferDirectoryAdmitted:
		return clievent.TransferDirectoryAdmitted, true
	case transfer.TransferDirectoryFinalized:
		return clievent.TransferDirectoryFinalized, true
	case transfer.TransferFileEnqueued:
		return clievent.TransferFileEnqueued, true
	case transfer.TransferFileStarted:
		return clievent.TransferFileStarted, true
	case transfer.TransferFileAdmitted:
		return clievent.TransferFileAdmitted, true
	case transfer.TransferFileFirstWrite:
		return clievent.TransferFileFirstWrite, true
	case transfer.TransferFileSettled:
		return clievent.TransferFileSettled, true
	case transfer.TransferJobSettled:
		return clievent.TransferJobSettled, true
	default:
		return 0, false
	}
}

func projectFileSelection(value transfer.FileSelectionDecision) (clievent.FileSelectionDecision, bool) {
	switch value {
	case 0:
		return clievent.FileSelectionNone, true
	case transfer.FileSelectionInherited:
		return clievent.FileSelectionInherited, true
	case transfer.FileSelectionNodeOverride:
		return clievent.FileSelectionNodeOverride, true
	case transfer.FileSelectionCatalogPathTarget:
		return clievent.FileSelectionCatalogPathTarget, true
	default:
		return 0, false
	}
}

func projectFileSettlement(value transfer.FileSettlementKind) (clievent.FileSettlement, bool) {
	switch value {
	case 0:
		return clievent.FileSettlementNone, true
	case transfer.FilePublished:
		return clievent.FilePublished, true
	case transfer.FilePaused:
		return clievent.FilePaused, true
	case transfer.FileCollision:
		return clievent.FileCollision, true
	case transfer.FileItemBlocked:
		return clievent.FileItemBlocked, true
	case transfer.FileFailed:
		return clievent.FileFailed, true
	default:
		return 0, false
	}
}

func projectTreeSettlement(value transfer.DirectTreeSettlementKind) (clievent.TreeSettlement, bool) {
	switch value {
	case 0:
		return clievent.TreeSettlementNone, true
	case transfer.DirectTreeSettlementSuccess:
		return clievent.TreeSettlementSuccess, true
	case transfer.DirectTreeSettlementPartial:
		return clievent.TreeSettlementPartial, true
	case transfer.DirectTreeSettlementPaused:
		return clievent.TreeSettlementPaused, true
	case transfer.DirectTreeSettlementFailed:
		return clievent.TreeSettlementFailed, true
	default:
		return 0, false
	}
}

func projectFilesystemOperation(value osfs.FilesystemOutputTraceOperation) (clievent.FilesystemOutputOperation, bool) {
	switch value {
	case osfs.TraceFilesystemCertified:
		return clievent.FilesystemCertified, true
	case osfs.TraceFeatureProbeCompleted:
		return clievent.FilesystemFeatureProbeCompleted, true
	case osfs.TraceCheckpointNamespaceOpened:
		return clievent.FilesystemCheckpointNamespaceOpened, true
	case osfs.TraceNativeLock:
		return clievent.FilesystemNativeLock, true
	case osfs.TraceSessionOpened:
		return clievent.FilesystemSessionOpened, true
	case osfs.TraceCheckpointReconciled:
		return clievent.FilesystemCheckpointReconciled, true
	case osfs.TraceRuntimeDecision:
		return clievent.FilesystemRuntimeDecision, true
	default:
		return 0, false
	}
}

func projectFilesystemCertification(value osfs.FilesystemOutputCertificationID) (clievent.FilesystemCertification, bool) {
	switch value {
	case "":
		return 0, true
	case osfs.FilesystemOutputCertificationLinuxExt4ProcessRestart:
		return clievent.FilesystemCertificationLinuxExt4ProcessRestart, true
	case osfs.FilesystemOutputCertificationWindowsNTFSProcessRestart:
		return clievent.FilesystemCertificationWindowsNTFSProcessRestart, true
	default:
		return 0, false
	}
}

func projectFilesystemRootDisposition(value osfs.FilesystemOutputRootDisposition) (clievent.FilesystemRootDisposition, bool) {
	switch value {
	case "":
		return 0, true
	case osfs.FilesystemOutputCallerProvidedContainer:
		return clievent.FilesystemRootCallerProvidedContainer, true
	case osfs.FilesystemOutputAuthorityCreatedRoot:
		return clievent.FilesystemRootAuthorityCreated, true
	default:
		return 0, false
	}
}

func projectFilesystemRuntimeComponent(value osfs.FilesystemOutputRuntimeComponent) (clievent.FilesystemRuntimeComponent, bool) {
	if value == 0 {
		return 0, true
	}
	if value < osfs.FilesystemOutputRuntimeSession || value > osfs.FilesystemOutputRuntimeCheckpoint {
		return 0, false
	}
	return clievent.FilesystemRuntimeComponent(value), true
}

func projectFilesystemRuntimeOperation(value osfs.FilesystemOutputRuntimeOperation) (clievent.FilesystemRuntimeOperation, bool) {
	if value == 0 {
		return 0, true
	}
	if value < osfs.FilesystemOutputRuntimeOpenDirectTree || value > osfs.FilesystemOutputRuntimeCleanup {
		return 0, false
	}
	return clievent.FilesystemRuntimeOperation(value), true
}

func projectFilesystemRuntimeDecision(value osfs.FilesystemOutputRuntimeDecision) (clievent.FilesystemRuntimeDecisionKind, bool) {
	if value == 0 {
		return 0, true
	}
	if value < osfs.FilesystemOutputRuntimeValidated || value > osfs.FilesystemOutputRuntimeCleanupPending {
		return 0, false
	}
	return clievent.FilesystemRuntimeDecisionKind(value), true
}

func projectFilesystemNativeLockScope(value osfs.FilesystemOutputNativeLockScope) (clievent.FilesystemNativeLockScope, bool) {
	if value == 0 {
		return 0, true
	}
	if value != osfs.FilesystemOutputNativeLockSession {
		return 0, false
	}
	return clievent.FilesystemNativeLockSession, true
}

func projectFilesystemNativeLockMilestone(value osfs.FilesystemOutputNativeLockMilestone) (clievent.FilesystemNativeLockMilestone, bool) {
	if value == 0 {
		return 0, true
	}
	if value < osfs.FilesystemOutputNativeLockAcquired || value > osfs.FilesystemOutputNativeLockReleaseReportedFailure {
		return 0, false
	}
	return clievent.FilesystemNativeLockMilestone(value), true
}

func projectFilesystemFailureStage(value osfs.FilesystemOutputFailureStage) (clievent.FilesystemFailureStage, bool) {
	if value == 0 {
		return 0, true
	}
	if !value.Valid() {
		return 0, false
	}
	return clievent.FilesystemFailureStage(value), true
}

func projectFilesystemReconciliationStep(value osfs.FilesystemCheckpointReconciliationStep) (clievent.FilesystemReconciliationStep, bool) {
	if value == 0 {
		return 0, true
	}
	if !value.Valid() {
		return 0, false
	}
	return clievent.FilesystemReconciliationStep(value), true
}

func projectFilesystemNativeErrorClass(value osfs.FilesystemNativeErrorClass) (clievent.FilesystemNativeErrorClass, bool) {
	if value == 0 {
		return 0, true
	}
	if !value.Valid() {
		return 0, false
	}
	return clievent.FilesystemNativeErrorClass(value), true
}

func projectSenderTerminalTransport(value sessionruntime.SenderTerminalTransportDisposition) (clievent.SenderTerminalTransport, bool) {
	switch value {
	case sessionruntime.SenderTerminalTransportAccepted:
		return clievent.SenderTerminalAccepted, true
	case sessionruntime.SenderTerminalTransportNotReached:
		return clievent.SenderTerminalNotReached, true
	case sessionruntime.SenderTerminalTransportUnsettled:
		return clievent.SenderTerminalUnsettled, true
	case sessionruntime.SenderTerminalTransportRejected:
		return clievent.SenderTerminalRejected, true
	case sessionruntime.SenderTerminalTransportRetired:
		return clievent.SenderTerminalRetired, true
	default:
		return 0, false
	}
}

func projectSenderTerminalOutcome(value sessionruntime.SenderTerminalOutcome) (clievent.SenderTerminalOutcome, bool) {
	switch value {
	case sessionruntime.SenderTerminalOutcomeDelivered:
		return clievent.SenderTerminalDelivered, true
	case sessionruntime.SenderTerminalOutcomeDropped:
		return clievent.SenderTerminalDropped, true
	case sessionruntime.SenderTerminalOutcomeUnknown:
		return clievent.SenderTerminalUnknown, true
	default:
		return 0, false
	}
}

func projectSenderTerminalDecision(value sessionruntime.SenderTerminalDecision) (clievent.SenderTerminalDecision, bool) {
	switch value {
	case sessionruntime.SenderTerminalDecisionDelivered:
		return clievent.SenderTerminalDecisionDelivered, true
	case sessionruntime.SenderTerminalDecisionNaturalRetirement:
		return clievent.SenderTerminalNaturalRetirement, true
	case sessionruntime.SenderTerminalDecisionFailed:
		return clievent.SenderTerminalFailed, true
	default:
		return 0, false
	}
}

func projectCatalogStorageOperation(value liveshare.CatalogStorageOperation) (clievent.CatalogStorageOperation, bool) {
	switch value {
	case liveshare.CatalogStorageCreating:
		return clievent.CatalogStorageCreating, true
	case liveshare.CatalogStorageCreated:
		return clievent.CatalogStorageCreated, true
	case liveshare.CatalogStorageRecovering:
		return clievent.CatalogStorageRecovering, true
	case liveshare.CatalogStorageRecovered:
		return clievent.CatalogStorageRecovered, true
	case liveshare.CatalogStorageBudgetRejected:
		return clievent.CatalogStorageBudgetRejected, true
	case liveshare.CatalogStorageCleaning:
		return clievent.CatalogStorageCleaning, true
	case liveshare.CatalogStorageCleaned:
		return clievent.CatalogStorageCleaned, true
	default:
		return 0, false
	}
}

func projectCatalogStorageCause(value liveshare.CatalogStorageCause) (clievent.CatalogStorageCause, bool) {
	switch value {
	case liveshare.CatalogStorageCauseNone:
		return clievent.CatalogStorageCauseNone, true
	case liveshare.CatalogStorageCauseCanceled:
		return clievent.CatalogStorageCauseCanceled, true
	case liveshare.CatalogStorageCauseDeadlineExceeded:
		return clievent.CatalogStorageCauseDeadline, true
	case liveshare.CatalogStorageCauseBudgetExceeded:
		return clievent.CatalogStorageCauseBudget, true
	case liveshare.CatalogStorageCauseUnexpected:
		return clievent.CatalogStorageCauseUnexpected, true
	default:
		return 0, false
	}
}

func projectRootPrefetchDecision(value liveshare.RootPrefetchDecision) (clievent.RootPrefetchDecision, bool) {
	switch value {
	case liveshare.RootPrefetchAttemptStarted:
		return clievent.RootPrefetchAttemptStarted, true
	case liveshare.RootPrefetchYieldedToDemand:
		return clievent.RootPrefetchYieldedToDemand, true
	case liveshare.RootPrefetchRetryScheduled:
		return clievent.RootPrefetchRetryScheduled, true
	case liveshare.RootPrefetchCommitted:
		return clievent.RootPrefetchCommitted, true
	case liveshare.RootPrefetchBudgetFailed:
		return clievent.RootPrefetchBudgetFailed, true
	case liveshare.RootPrefetchScanFailed:
		return clievent.RootPrefetchScanFailed, true
	case liveshare.RootPrefetchStopped:
		return clievent.RootPrefetchStopped, true
	default:
		return 0, false
	}
}
