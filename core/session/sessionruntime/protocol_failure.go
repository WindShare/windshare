package sessionruntime

import (
	"errors"

	"github.com/windshare/windshare/core/session/protocolsession"
)

var ErrProtocolFailure = errors.New("session runtime protocol failure is invalid")

// ProtocolFailureScope retains only the authenticated wire classification. The
// remote provider's explanatory text is deliberately outside this value so it
// cannot cross into tracing or command projection by accident.
type ProtocolFailureScope uint8

const (
	ProtocolFailureDirectory = ProtocolFailureScope(protocolsession.OperationScopeDirectory)
	ProtocolFailureRevision  = ProtocolFailureScope(protocolsession.OperationScopeRevision)
	ProtocolFailureBlock     = ProtocolFailureScope(protocolsession.OperationScopeBlock)
	ProtocolFailurePeer      = ProtocolFailureScope(protocolsession.OperationScopePeer)
)

type ProtocolFailureSettlementKind uint8

const (
	ProtocolFailureSettlementReceivedAuthenticated ProtocolFailureSettlementKind = iota + 1
	ProtocolFailureSettlementResponseSend
)

// ProtocolFailureResponseSendSettlement is a copied result, not live send
// authority. Diagnostics can describe the response without participating in its
// admission, retry, or operation-retirement decisions.
type ProtocolFailureResponseSendSettlement struct {
	Admitted bool
	Settled  bool
	Outcome  protocolsession.SendOutcome
}

type ProtocolFailureSettlement struct {
	kind     ProtocolFailureSettlementKind
	admitted bool
	settled  bool
	outcome  protocolsession.SendOutcome
}

func (settlement ProtocolFailureSettlement) Kind() ProtocolFailureSettlementKind {
	return settlement.kind
}

func (settlement ProtocolFailureSettlement) ResponseSend() (
	ProtocolFailureResponseSendSettlement,
	bool,
) {
	if settlement.kind != ProtocolFailureSettlementResponseSend {
		return ProtocolFailureResponseSendSettlement{}, false
	}
	return ProtocolFailureResponseSendSettlement{
		Admitted: settlement.admitted,
		Settled:  settlement.settled,
		Outcome:  settlement.outcome,
	}, true
}

// ProtocolFailureSpec contains only reviewed protocol facts and typed
// correlation. It intentionally has no message or body field.
type ProtocolFailureSpec struct {
	RequestKind         protocolsession.MessageKind
	WireScope           ProtocolFailureScope
	WireCode            uint16
	Retryable           bool
	RetryAfterMillis    uint32
	HasRetryAfter       bool
	ProtocolSessionID   protocolsession.ProtocolSessionID
	ProtocolOperationID protocolsession.OperationID
	Lane                LaneIdentity
	HasLane             bool
}

// ProtocolFailure is an immutable, unversioned fact constructed at an
// authenticated protocol boundary. Projection owns all JSON naming and identity
// encoding; business code continues to own retry and settlement decisions.
type ProtocolFailure struct {
	requestKind         protocolsession.MessageKind
	wireScope           ProtocolFailureScope
	wireCode            uint16
	retryable           bool
	retryAfterMillis    uint32
	hasRetryAfter       bool
	protocolSessionID   protocolsession.ProtocolSessionID
	protocolOperationID protocolsession.OperationID
	lane                LaneIdentity
	hasLane             bool
	settlement          ProtocolFailureSettlement
}

func NewReceivedAuthenticatedProtocolFailure(spec ProtocolFailureSpec) (ProtocolFailure, error) {
	return newProtocolFailure(spec, ProtocolFailureSettlement{
		kind: ProtocolFailureSettlementReceivedAuthenticated,
	})
}

func NewResponseSendProtocolFailure(
	spec ProtocolFailureSpec,
	response ProtocolFailureResponseSendSettlement,
) (ProtocolFailure, error) {
	if !validProtocolFailureSendSettlement(response) {
		return ProtocolFailure{}, ErrProtocolFailure
	}
	return newProtocolFailure(spec, ProtocolFailureSettlement{
		kind:     ProtocolFailureSettlementResponseSend,
		admitted: response.Admitted,
		settled:  response.Settled,
		outcome:  response.Outcome,
	})
}

func newProtocolFailure(
	spec ProtocolFailureSpec,
	settlement ProtocolFailureSettlement,
) (ProtocolFailure, error) {
	if !receiverRequestKind(spec.RequestKind) ||
		!validProtocolFailureScope(spec.WireScope) ||
		spec.ProtocolSessionID.IsZero() ||
		spec.ProtocolOperationID.IsZero() ||
		spec.Retryable != spec.HasRetryAfter ||
		(spec.HasRetryAfter && (spec.RetryAfterMillis <
			uint32(protocolsession.MinOperationFailureRetryAfter.Milliseconds()) ||
			spec.RetryAfterMillis >
				uint32(protocolsession.MaxOperationFailureRetryAfter.Milliseconds()))) ||
		(spec.HasLane && !spec.Lane.valid(true)) ||
		(!spec.HasLane && spec.Lane != (LaneIdentity{})) ||
		!validProtocolFailureSettlement(settlement) {
		return ProtocolFailure{}, ErrProtocolFailure
	}
	return ProtocolFailure{
		requestKind:         spec.RequestKind,
		wireScope:           spec.WireScope,
		wireCode:            spec.WireCode,
		retryable:           spec.Retryable,
		retryAfterMillis:    spec.RetryAfterMillis,
		hasRetryAfter:       spec.HasRetryAfter,
		protocolSessionID:   spec.ProtocolSessionID,
		protocolOperationID: spec.ProtocolOperationID,
		lane:                spec.Lane,
		hasLane:             spec.HasLane,
		settlement:          settlement,
	}, nil
}

func (failure ProtocolFailure) IsZero() bool {
	return !validProtocolFailureSettlement(failure.settlement)
}

func (failure ProtocolFailure) RequestKind() protocolsession.MessageKind {
	return failure.requestKind
}

func (failure ProtocolFailure) WireScope() ProtocolFailureScope {
	return failure.wireScope
}

func (failure ProtocolFailure) WireCode() uint16 {
	return failure.wireCode
}

func (failure ProtocolFailure) Retryable() bool {
	return failure.retryable
}

func (failure ProtocolFailure) RetryAfterMillis() (uint32, bool) {
	return failure.retryAfterMillis, failure.hasRetryAfter
}

func (failure ProtocolFailure) ProtocolSessionID() protocolsession.ProtocolSessionID {
	return failure.protocolSessionID
}

func (failure ProtocolFailure) ProtocolOperationID() protocolsession.OperationID {
	return failure.protocolOperationID
}

func (failure ProtocolFailure) Lane() (LaneIdentity, bool) {
	return failure.lane, failure.hasLane
}

func (failure ProtocolFailure) Settlement() ProtocolFailureSettlement {
	return failure.settlement
}

func protocolFailureForAuthenticatedReceive(
	sessionID protocolsession.ProtocolSessionID,
	operationID protocolsession.OperationID,
	requestKind protocolsession.MessageKind,
	message protocolsession.Message,
	lane LaneIdentity,
	hasLane bool,
) (ProtocolFailure, bool) {
	if message.Kind() != protocolsession.MessageOperationError {
		return ProtocolFailure{}, false
	}
	semantic, err := protocolsession.SenderControlSemanticBody(message)
	if err != nil {
		return ProtocolFailure{}, false
	}
	wireFailure, err := protocolsession.DecodeOperationFailure(semantic)
	if err != nil {
		return ProtocolFailure{}, false
	}
	return receivedProtocolFailure(
		sessionID, operationID, requestKind, wireFailure, lane, hasLane,
	)
}

func protocolFailureForResponseSend(
	sessionID protocolsession.ProtocolSessionID,
	operationID protocolsession.OperationID,
	requestKind protocolsession.MessageKind,
	responseKind protocolsession.MessageKind,
	body []byte,
	lane LaneIdentity,
	hasLane bool,
	completion protocolsession.SendCompletion,
) (ProtocolFailure, bool) {
	if responseKind != protocolsession.MessageOperationError {
		return ProtocolFailure{}, false
	}
	wireFailure, err := protocolsession.DecodeOperationFailure(body)
	if err != nil {
		return ProtocolFailure{}, false
	}
	spec, ok := protocolFailureSpec(
		sessionID, operationID, requestKind, wireFailure, lane, hasLane,
	)
	if !ok {
		return ProtocolFailure{}, false
	}
	failure, err := NewResponseSendProtocolFailure(spec, ProtocolFailureResponseSendSettlement{
		Admitted: completion.Admitted,
		Settled:  completion.Settled,
		Outcome:  completion.Outcome,
	})
	return failure, err == nil
}

func receivedProtocolFailure(
	sessionID protocolsession.ProtocolSessionID,
	operationID protocolsession.OperationID,
	requestKind protocolsession.MessageKind,
	wireFailure protocolsession.OperationFailure,
	lane LaneIdentity,
	hasLane bool,
) (ProtocolFailure, bool) {
	spec, ok := protocolFailureSpec(
		sessionID, operationID, requestKind, wireFailure, lane, hasLane,
	)
	if !ok {
		return ProtocolFailure{}, false
	}
	failure, err := NewReceivedAuthenticatedProtocolFailure(spec)
	return failure, err == nil
}

func protocolFailureSpec(
	sessionID protocolsession.ProtocolSessionID,
	operationID protocolsession.OperationID,
	requestKind protocolsession.MessageKind,
	wireFailure protocolsession.OperationFailure,
	lane LaneIdentity,
	hasLane bool,
) (ProtocolFailureSpec, bool) {
	scope := ProtocolFailureScope(wireFailure.Scope)
	if !validProtocolFailureScope(scope) {
		return ProtocolFailureSpec{}, false
	}
	retryAfterMillis := uint32(0)
	hasRetryAfter := wireFailure.Retryable
	if hasRetryAfter {
		retryAfterMillis = uint32(wireFailure.RetryAfter.Milliseconds())
	}
	if !hasLane {
		lane = LaneIdentity{}
	}
	return ProtocolFailureSpec{
		RequestKind:         requestKind,
		WireScope:           scope,
		WireCode:            wireFailure.Code,
		Retryable:           wireFailure.Retryable,
		RetryAfterMillis:    retryAfterMillis,
		HasRetryAfter:       hasRetryAfter,
		ProtocolSessionID:   sessionID,
		ProtocolOperationID: operationID,
		Lane:                lane,
		HasLane:             hasLane,
	}, true
}

func validProtocolFailureScope(scope ProtocolFailureScope) bool {
	switch scope {
	case ProtocolFailureDirectory, ProtocolFailureRevision, ProtocolFailureBlock,
		ProtocolFailurePeer:
		return true
	default:
		return false
	}
}

func validProtocolFailureSettlement(settlement ProtocolFailureSettlement) bool {
	switch settlement.kind {
	case ProtocolFailureSettlementReceivedAuthenticated:
		return !settlement.admitted && !settlement.settled &&
			settlement.outcome == protocolsession.SendOutcomeUnknown
	case ProtocolFailureSettlementResponseSend:
		return validProtocolFailureSendSettlement(ProtocolFailureResponseSendSettlement{
			Admitted: settlement.admitted,
			Settled:  settlement.settled,
			Outcome:  settlement.outcome,
		})
	default:
		return false
	}
}

func validProtocolFailureSendSettlement(settlement ProtocolFailureResponseSendSettlement) bool {
	switch settlement.Outcome {
	case protocolsession.SendOutcomeUnknown:
		return true
	case protocolsession.SendOutcomeDelivered:
		return settlement.Settled && settlement.Admitted
	case protocolsession.SendOutcomeDropped:
		return settlement.Settled
	default:
		return false
	}
}
