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
	sessionruntime.ProtocolOperationReceiverCompleted:     clievent.ProtocolOperationReceiverCompleted,
	sessionruntime.ProtocolOperationReceiverFailed:        clievent.ProtocolOperationReceiverFailed,
	sessionruntime.ProtocolOperationReceiverEnded:         clievent.ProtocolOperationReceiverEnded,
	sessionruntime.ProtocolOperationSenderRequestReceived: clievent.ProtocolOperationSenderRequestReceived,
	sessionruntime.ProtocolOperationSenderResponseSettled: clievent.ProtocolOperationSenderResponseSettled,
	sessionruntime.ProtocolOperationSenderContentDecision: clievent.ProtocolOperationSenderContentDecision,
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

var protocolFailureScopeProjections = map[sessionruntime.ProtocolFailureScope]clievent.ProtocolFailureScope{
	sessionruntime.ProtocolFailureDirectory: clievent.ProtocolFailureDirectory,
	sessionruntime.ProtocolFailureRevision:  clievent.ProtocolFailureRevision,
	sessionruntime.ProtocolFailureBlock:     clievent.ProtocolFailureBlock,
	sessionruntime.ProtocolFailurePeer:      clievent.ProtocolFailurePeer,
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

func projectProtocolFailureScope(
	value sessionruntime.ProtocolFailureScope,
) (clievent.ProtocolFailureScope, bool) {
	projected, ok := protocolFailureScopeProjections[value]
	return projected, ok
}
