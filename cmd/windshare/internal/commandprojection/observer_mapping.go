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

func projectProtocolOperationStage(value sessionruntime.ProtocolOperationStage) (clievent.ProtocolOperationStage, bool) {
	switch value {
	case sessionruntime.ProtocolOperationReceiverCompleted:
		return clievent.ProtocolOperationReceiverCompleted, true
	case sessionruntime.ProtocolOperationReceiverFailed:
		return clievent.ProtocolOperationReceiverFailed, true
	case sessionruntime.ProtocolOperationReceiverEnded:
		return clievent.ProtocolOperationReceiverEnded, true
	case sessionruntime.ProtocolOperationSenderRequestReceived:
		return clievent.ProtocolOperationSenderRequestReceived, true
	case sessionruntime.ProtocolOperationSenderResponseSettled:
		return clievent.ProtocolOperationSenderResponseSettled, true
	default:
		return 0, false
	}
}

func projectProtocolMessageKind(value protocolsession.MessageKind) (clievent.ProtocolMessageKind, bool) {
	switch value {
	case protocolsession.MessageListChildren:
		return clievent.ProtocolMessageListChildren, true
	case protocolsession.MessageCatalogResult:
		return clievent.ProtocolMessageCatalogResult, true
	case protocolsession.MessageOpenRevisions:
		return clievent.ProtocolMessageOpenRevisions, true
	case protocolsession.MessageOpenResults:
		return clievent.ProtocolMessageOpenResults, true
	case protocolsession.MessageRenewLease:
		return clievent.ProtocolMessageRenewLease, true
	case protocolsession.MessageReleaseLease:
		return clievent.ProtocolMessageReleaseLease, true
	case protocolsession.MessageRequestBlocks:
		return clievent.ProtocolMessageRequestBlocks, true
	case protocolsession.MessageBlockFragment:
		return clievent.ProtocolMessageBlockFragment, true
	case protocolsession.MessageCancel:
		return clievent.ProtocolMessageCancel, true
	case protocolsession.MessageOperationError:
		return clievent.ProtocolMessageOperationError, true
	case protocolsession.MessageSessionTerminal:
		return clievent.ProtocolMessageSessionTerminal, true
	case protocolsession.MessageLaneAttach:
		return clievent.ProtocolMessageLaneAttach, true
	case protocolsession.MessageScanProgress:
		return clievent.ProtocolMessageScanProgress, true
	case protocolsession.MessageOperationComplete:
		return clievent.ProtocolMessageOperationComplete, true
	case protocolsession.MessageLeaseResult:
		return clievent.ProtocolMessageLeaseResult, true
	case protocolsession.MessagePeerOffer:
		return clievent.ProtocolMessagePeerOffer, true
	case protocolsession.MessagePeerAnswer:
		return clievent.ProtocolMessagePeerAnswer, true
	case protocolsession.MessagePeerCandidate:
		return clievent.ProtocolMessagePeerCandidate, true
	default:
		return 0, false
	}
}

func projectProtocolSendOutcome(value protocolsession.SendOutcome) (clievent.ProtocolSendOutcome, bool) {
	switch value {
	case protocolsession.SendOutcomeUnknown:
		return clievent.ProtocolSendUnknown, true
	case protocolsession.SendOutcomeDelivered:
		return clievent.ProtocolSendDelivered, true
	case protocolsession.SendOutcomeDropped:
		return clievent.ProtocolSendDropped, true
	default:
		return 0, false
	}
}

func projectProtocolOperationCause(value sessionruntime.ProtocolOperationCause) (clievent.ProtocolOperationCause, bool) {
	switch value {
	case sessionruntime.ProtocolOperationCauseNone:
		return clievent.ProtocolOperationCauseNone, true
	case sessionruntime.ProtocolOperationCauseCanceled:
		return clievent.ProtocolOperationCauseCanceled, true
	case sessionruntime.ProtocolOperationCauseDeadline:
		return clievent.ProtocolOperationCauseDeadline, true
	case sessionruntime.ProtocolOperationCauseRuntimeClosed:
		return clievent.ProtocolOperationCauseRuntimeClosed, true
	case sessionruntime.ProtocolOperationCauseLaneUnavailable:
		return clievent.ProtocolOperationCauseLaneUnavailable, true
	case sessionruntime.ProtocolOperationCauseWriterStopped:
		return clievent.ProtocolOperationCauseWriterStopped, true
	case sessionruntime.ProtocolOperationCauseOperationClosed:
		return clievent.ProtocolOperationCauseOperationClosed, true
	case sessionruntime.ProtocolOperationCauseProtocolFailure:
		return clievent.ProtocolOperationCauseProtocolFailure, true
	default:
		return 0, false
	}
}

func projectRelayRetirement(value relayv2.LifecycleRetirementSource) (clievent.RelayRetirementSource, bool) {
	switch value {
	case "":
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

func projectPeerStage(value v2peer.SenderAttemptStage) (clievent.PeerAttemptStage, bool) {
	switch value {
	case v2peer.SenderAttemptStarted:
		return clievent.PeerAttemptStarted, true
	case v2peer.SenderAttemptOfferReceived:
		return clievent.PeerOfferReceived, true
	case v2peer.SenderAttemptAnswerCreated:
		return clievent.PeerAnswerCreated, true
	case v2peer.SenderAttemptAnswerSent:
		return clievent.PeerAnswerSent, true
	case v2peer.SenderAttemptDataChannelOpen:
		return clievent.PeerDataChannelOpen, true
	case v2peer.SenderAttemptLaneAdmissionStarted:
		return clievent.PeerLaneAdmissionStarted, true
	case v2peer.SenderAttemptAdmitted:
		return clievent.PeerAttemptAdmitted, true
	case v2peer.SenderAttemptFailed:
		return clievent.PeerAttemptFailed, true
	default:
		return 0, false
	}
}

func projectPeerFailureScope(value v2peer.AttemptFailureScope) (clievent.PeerFailureScope, bool) {
	switch value {
	case v2peer.AttemptFailureScopeAttempt:
		return clievent.PeerFailureAttempt, true
	case v2peer.AttemptFailureScopeSession:
		return clievent.PeerFailureSession, true
	default:
		return 0, false
	}
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
