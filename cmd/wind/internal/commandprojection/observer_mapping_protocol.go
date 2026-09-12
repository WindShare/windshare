package commandprojection

import (
	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
)

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
	sessionruntime.ProtocolOperationReceiverCompleted:               clievent.ProtocolOperationReceiverCompleted,
	sessionruntime.ProtocolOperationReceiverFailed:                  clievent.ProtocolOperationReceiverFailed,
	sessionruntime.ProtocolOperationReceiverEnded:                   clievent.ProtocolOperationReceiverEnded,
	sessionruntime.ProtocolOperationSenderRequestReceived:           clievent.ProtocolOperationSenderRequestReceived,
	sessionruntime.ProtocolOperationReceiverWaitingActiveCapacity:   clievent.ProtocolOperationReceiverWaitingActiveCapacity,
	sessionruntime.ProtocolOperationReceiverWaitingRetainedCapacity: clievent.ProtocolOperationReceiverWaitingRetainedCapacity,
	sessionruntime.ProtocolOperationReceiverAdmissionReady:          clievent.ProtocolOperationReceiverAdmissionReady,
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
	protocolsession.SendOutcomeUnknown:            clievent.ProtocolSendUnknown,
	protocolsession.SendOutcomeTransportConfirmed: clievent.ProtocolSendTransportConfirmed,
	protocolsession.SendOutcomeDropped:            clievent.ProtocolSendDropped,
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

var protocolFailureScopeProjections = map[sessionruntime.ProtocolErrorScope]clievent.ProtocolErrorScope{
	sessionruntime.ProtocolErrorDirectory: clievent.ProtocolErrorDirectory,
	sessionruntime.ProtocolErrorRevision:  clievent.ProtocolErrorRevision,
	sessionruntime.ProtocolErrorBlock:     clievent.ProtocolErrorBlock,
	sessionruntime.ProtocolErrorPeer:      clievent.ProtocolErrorPeer,
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

func projectProtocolErrorScope(
	value sessionruntime.ProtocolErrorScope,
) (clievent.ProtocolErrorScope, bool) {
	projected, ok := protocolFailureScopeProjections[value]
	return projected, ok
}

var responseSendEvidenceProjections = map[protocolsession.ResponseSendEvidence]clievent.ResponseSendEvidence{
	protocolsession.ResponseSendEvidenceDefinitelyNotSent:  clievent.ResponseSendEvidenceDefinitelyNotSent,
	protocolsession.ResponseSendEvidenceUncertain:          clievent.ResponseSendEvidenceUncertain,
	protocolsession.ResponseSendEvidenceTransportConfirmed: clievent.ResponseSendEvidenceTransportConfirmed,
}

var responseSendEndProjections = map[protocolsession.ResponseSendEnd]clievent.ResponseSendEnd{
	protocolsession.ResponseSendEndPreparationFailed:    clievent.ResponseSendEndPreparationFailed,
	protocolsession.ResponseSendEndRouteUnavailable:     clievent.ResponseSendEndRouteUnavailable,
	protocolsession.ResponseSendEndAuthorityUnavailable: clievent.ResponseSendEndAuthorityUnavailable,
	protocolsession.ResponseSendEndTransportConfirmed:   clievent.ResponseSendEndTransportConfirmed,
	protocolsession.ResponseSendEndPolicySuppressed:     clievent.ResponseSendEndPolicySuppressed,
	protocolsession.ResponseSendEndCallerCanceled:       clievent.ResponseSendEndCallerCanceled,
	protocolsession.ResponseSendEndDeadlineExceeded:     clievent.ResponseSendEndDeadlineExceeded,
	protocolsession.ResponseSendEndRuntimeStopped:       clievent.ResponseSendEndRuntimeStopped,
	protocolsession.ResponseSendEndRetryDisallowed:      clievent.ResponseSendEndRetryDisallowed,
	protocolsession.ResponseSendEndNoUsableLane:         clievent.ResponseSendEndNoUsableLane,
	protocolsession.ResponseSendEndAttemptsExhausted:    clievent.ResponseSendEndAttemptsExhausted,
	protocolsession.ResponseSendEndAuthorityLost:        clievent.ResponseSendEndAuthorityLost,
	protocolsession.ResponseSendEndInvalidReceipt:       clievent.ResponseSendEndInvalidReceipt,
}

var sendAttemptEndProjections = map[protocolsession.SendAttemptEnd]clievent.SendAttemptEnd{
	protocolsession.SendAttemptEndRejectedBeforeReceipt: clievent.SendAttemptEndRejectedBeforeReceipt,
	protocolsession.SendAttemptEndSettled:               clievent.SendAttemptEndSettled,
	protocolsession.SendAttemptEndWaitingEnded:          clievent.SendAttemptEndWaitingEnded,
}

var sendCleanupProjections = map[protocolsession.SendCleanupKind]clievent.SendCleanupKind{
	protocolsession.SendCleanupNone:             clievent.SendCleanupNone,
	protocolsession.SendCleanupRouteReleased:    clievent.SendCleanupRouteReleased,
	protocolsession.SendCleanupOperationRetired: clievent.SendCleanupOperationRetired,
	protocolsession.SendCleanupFailed:           clievent.SendCleanupFailed,
}

var sendAttemptCauseProjections = map[protocolsession.SendAttemptCauseKind]clievent.SendAttemptCauseKind{
	protocolsession.SendAttemptCauseNone:               clievent.SendAttemptCauseNone,
	protocolsession.SendAttemptCauseCanceled:           clievent.SendAttemptCauseCanceled,
	protocolsession.SendAttemptCauseDeadline:           clievent.SendAttemptCauseDeadline,
	protocolsession.SendAttemptCauseControlQueueFull:   clievent.SendAttemptCauseControlQueueFull,
	protocolsession.SendAttemptCauseDataQueueFull:      clievent.SendAttemptCauseDataQueueFull,
	protocolsession.SendAttemptCauseWriterStopped:      clievent.SendAttemptCauseWriterStopped,
	protocolsession.SendAttemptCauseWriterTerminal:     clievent.SendAttemptCauseWriterTerminal,
	protocolsession.SendAttemptCauseTransportFailure:   clievent.SendAttemptCauseTransportFailure,
	protocolsession.SendAttemptCausePreparationFailure: clievent.SendAttemptCausePreparationFailure,
	protocolsession.SendAttemptCauseAdmissionFailure:   clievent.SendAttemptCauseAdmissionFailure,
}
