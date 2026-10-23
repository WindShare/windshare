package eventproject

import (
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
	appobs "github.com/windshare/windshare/engine/observation"
)

func projectProtocolRole(value protocolsession.Role) (appobs.ProtocolRole, bool) {
	switch value {
	case protocolsession.RoleReceiver:
		return appobs.ProtocolRoleReceiver, true
	case protocolsession.RoleSender:
		return appobs.ProtocolRoleSender, true
	default:
		return 0, false
	}
}

var protocolOperationStageProjections = map[sessionruntime.ProtocolOperationStage]appobs.ProtocolOperationStage{
	sessionruntime.ProtocolOperationReceiverCompleted:               appobs.ProtocolOperationReceiverCompleted,
	sessionruntime.ProtocolOperationReceiverFailed:                  appobs.ProtocolOperationReceiverFailed,
	sessionruntime.ProtocolOperationReceiverEnded:                   appobs.ProtocolOperationReceiverEnded,
	sessionruntime.ProtocolOperationSenderRequestReceived:           appobs.ProtocolOperationSenderRequestReceived,
	sessionruntime.ProtocolOperationReceiverWaitingActiveCapacity:   appobs.ProtocolOperationReceiverWaitingActiveCapacity,
	sessionruntime.ProtocolOperationReceiverWaitingRetainedCapacity: appobs.ProtocolOperationReceiverWaitingRetainedCapacity,
	sessionruntime.ProtocolOperationReceiverAdmissionReady:          appobs.ProtocolOperationReceiverAdmissionReady,
}

var protocolMessageKindProjections = map[protocolsession.MessageKind]appobs.ProtocolMessageKind{
	protocolsession.MessageListChildren:      appobs.ProtocolMessageListChildren,
	protocolsession.MessageCatalogResult:     appobs.ProtocolMessageCatalogResult,
	protocolsession.MessageOpenRevisions:     appobs.ProtocolMessageOpenRevisions,
	protocolsession.MessageOpenResults:       appobs.ProtocolMessageOpenResults,
	protocolsession.MessageRenewLease:        appobs.ProtocolMessageRenewLease,
	protocolsession.MessageReleaseLease:      appobs.ProtocolMessageReleaseLease,
	protocolsession.MessageRequestBlocks:     appobs.ProtocolMessageRequestBlocks,
	protocolsession.MessageBlockFragment:     appobs.ProtocolMessageBlockFragment,
	protocolsession.MessageCancel:            appobs.ProtocolMessageCancel,
	protocolsession.MessageOperationError:    appobs.ProtocolMessageOperationError,
	protocolsession.MessageSessionTerminal:   appobs.ProtocolMessageSessionTerminal,
	protocolsession.MessageLaneAttach:        appobs.ProtocolMessageLaneAttach,
	protocolsession.MessageScanProgress:      appobs.ProtocolMessageScanProgress,
	protocolsession.MessageOperationComplete: appobs.ProtocolMessageOperationComplete,
	protocolsession.MessageLeaseResult:       appobs.ProtocolMessageLeaseResult,
	protocolsession.MessagePeerOffer:         appobs.ProtocolMessagePeerOffer,
	protocolsession.MessagePeerAnswer:        appobs.ProtocolMessagePeerAnswer,
	protocolsession.MessagePeerCandidate:     appobs.ProtocolMessagePeerCandidate,
}

var protocolSendOutcomeProjections = map[protocolsession.SendOutcome]appobs.ProtocolSendOutcome{
	protocolsession.SendOutcomeUnknown:            appobs.ProtocolSendUnknown,
	protocolsession.SendOutcomeTransportConfirmed: appobs.ProtocolSendTransportConfirmed,
	protocolsession.SendOutcomeDropped:            appobs.ProtocolSendDropped,
}

var protocolOperationCauseProjections = map[sessionruntime.ProtocolOperationCause]appobs.ProtocolOperationCause{
	sessionruntime.ProtocolOperationCauseNone:            appobs.ProtocolOperationCauseNone,
	sessionruntime.ProtocolOperationCauseCanceled:        appobs.ProtocolOperationCauseCanceled,
	sessionruntime.ProtocolOperationCauseDeadline:        appobs.ProtocolOperationCauseDeadline,
	sessionruntime.ProtocolOperationCauseRuntimeClosed:   appobs.ProtocolOperationCauseRuntimeClosed,
	sessionruntime.ProtocolOperationCauseLaneUnavailable: appobs.ProtocolOperationCauseLaneUnavailable,
	sessionruntime.ProtocolOperationCauseWriterStopped:   appobs.ProtocolOperationCauseWriterStopped,
	sessionruntime.ProtocolOperationCauseOperationClosed: appobs.ProtocolOperationCauseOperationClosed,
	sessionruntime.ProtocolOperationCauseProtocolFailure: appobs.ProtocolOperationCauseProtocolFailure,
	sessionruntime.ProtocolOperationCauseSuperseded:      appobs.ProtocolOperationCauseSuperseded,
}

var protocolFailureScopeProjections = map[sessionruntime.ProtocolErrorScope]appobs.ProtocolErrorScope{
	sessionruntime.ProtocolErrorDirectory: appobs.ProtocolErrorDirectory,
	sessionruntime.ProtocolErrorRevision:  appobs.ProtocolErrorRevision,
	sessionruntime.ProtocolErrorBlock:     appobs.ProtocolErrorBlock,
	sessionruntime.ProtocolErrorPeer:      appobs.ProtocolErrorPeer,
}

func projectProtocolOperationStage(value sessionruntime.ProtocolOperationStage) (appobs.ProtocolOperationStage, bool) {
	projected, ok := protocolOperationStageProjections[value]
	return projected, ok
}

func projectProtocolMessageKind(value protocolsession.MessageKind) (appobs.ProtocolMessageKind, bool) {
	projected, ok := protocolMessageKindProjections[value]
	return projected, ok
}

func projectProtocolSendOutcome(value protocolsession.SendOutcome) (appobs.ProtocolSendOutcome, bool) {
	projected, ok := protocolSendOutcomeProjections[value]
	return projected, ok
}

func projectProtocolOperationCause(value sessionruntime.ProtocolOperationCause) (appobs.ProtocolOperationCause, bool) {
	projected, ok := protocolOperationCauseProjections[value]
	return projected, ok
}

func projectProtocolErrorScope(
	value sessionruntime.ProtocolErrorScope,
) (appobs.ProtocolErrorScope, bool) {
	projected, ok := protocolFailureScopeProjections[value]
	return projected, ok
}

var responseSendEvidenceProjections = map[protocolsession.ResponseSendEvidence]appobs.ResponseSendEvidence{
	protocolsession.ResponseSendEvidenceDefinitelyNotSent:  appobs.ResponseSendEvidenceDefinitelyNotSent,
	protocolsession.ResponseSendEvidenceUncertain:          appobs.ResponseSendEvidenceUncertain,
	protocolsession.ResponseSendEvidenceTransportConfirmed: appobs.ResponseSendEvidenceTransportConfirmed,
}

var responseSendEndProjections = map[protocolsession.ResponseSendEnd]appobs.ResponseSendEnd{
	protocolsession.ResponseSendEndPreparationFailed:    appobs.ResponseSendEndPreparationFailed,
	protocolsession.ResponseSendEndRouteUnavailable:     appobs.ResponseSendEndRouteUnavailable,
	protocolsession.ResponseSendEndAuthorityUnavailable: appobs.ResponseSendEndAuthorityUnavailable,
	protocolsession.ResponseSendEndTransportConfirmed:   appobs.ResponseSendEndTransportConfirmed,
	protocolsession.ResponseSendEndPolicySuppressed:     appobs.ResponseSendEndPolicySuppressed,
	protocolsession.ResponseSendEndCallerCanceled:       appobs.ResponseSendEndCallerCanceled,
	protocolsession.ResponseSendEndDeadlineExceeded:     appobs.ResponseSendEndDeadlineExceeded,
	protocolsession.ResponseSendEndRuntimeStopped:       appobs.ResponseSendEndRuntimeStopped,
	protocolsession.ResponseSendEndRetryDisallowed:      appobs.ResponseSendEndRetryDisallowed,
	protocolsession.ResponseSendEndNoUsableLane:         appobs.ResponseSendEndNoUsableLane,
	protocolsession.ResponseSendEndAttemptsExhausted:    appobs.ResponseSendEndAttemptsExhausted,
	protocolsession.ResponseSendEndAuthorityLost:        appobs.ResponseSendEndAuthorityLost,
	protocolsession.ResponseSendEndInvalidReceipt:       appobs.ResponseSendEndInvalidReceipt,
}

var sendAttemptEndProjections = map[protocolsession.SendAttemptEnd]appobs.SendAttemptEnd{
	protocolsession.SendAttemptEndRejectedBeforeReceipt: appobs.SendAttemptEndRejectedBeforeReceipt,
	protocolsession.SendAttemptEndSettled:               appobs.SendAttemptEndSettled,
	protocolsession.SendAttemptEndWaitingEnded:          appobs.SendAttemptEndWaitingEnded,
}

var sendCleanupProjections = map[protocolsession.SendCleanupKind]appobs.SendCleanupKind{
	protocolsession.SendCleanupNone:             appobs.SendCleanupNone,
	protocolsession.SendCleanupRouteReleased:    appobs.SendCleanupRouteReleased,
	protocolsession.SendCleanupOperationRetired: appobs.SendCleanupOperationRetired,
	protocolsession.SendCleanupFailed:           appobs.SendCleanupFailed,
}

var sendAttemptCauseProjections = map[protocolsession.SendAttemptCauseKind]appobs.SendAttemptCauseKind{
	protocolsession.SendAttemptCauseNone:               appobs.SendAttemptCauseNone,
	protocolsession.SendAttemptCauseCanceled:           appobs.SendAttemptCauseCanceled,
	protocolsession.SendAttemptCauseDeadline:           appobs.SendAttemptCauseDeadline,
	protocolsession.SendAttemptCauseControlQueueFull:   appobs.SendAttemptCauseControlQueueFull,
	protocolsession.SendAttemptCauseDataQueueFull:      appobs.SendAttemptCauseDataQueueFull,
	protocolsession.SendAttemptCauseWriterStopped:      appobs.SendAttemptCauseWriterStopped,
	protocolsession.SendAttemptCauseWriterTerminal:     appobs.SendAttemptCauseWriterTerminal,
	protocolsession.SendAttemptCauseTransportFailure:   appobs.SendAttemptCauseTransportFailure,
	protocolsession.SendAttemptCausePreparationFailure: appobs.SendAttemptCausePreparationFailure,
	protocolsession.SendAttemptCauseAdmissionFailure:   appobs.SendAttemptCauseAdmissionFailure,
}
