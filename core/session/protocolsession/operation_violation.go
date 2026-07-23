package protocolsession

import "errors"

var (
	ErrAuthenticatedOperationViolation        = errors.New("protocolsession: authenticated operation invariant violated")
	ErrInvalidAuthenticatedOperationViolation = errors.New("protocolsession: authenticated operation violation is invalid")
	ErrAuthenticatedOperationObserver         = errors.New("protocolsession: authenticated operation observer is invalid")
)

// AuthenticatedOperationViolationCode names facts proved after envelope and
// sender-control authentication but before ordinary operation dispatch. The
// value carrying a code is sealed; callers can observe producer facts but cannot
// manufacture them with a public literal.
type AuthenticatedOperationViolationCode uint8

const (
	authenticatedOperationViolationInvalid AuthenticatedOperationViolationCode = iota
	AuthenticatedOperationViolationMalformedFailure
	AuthenticatedOperationViolationMalformedPeerControl
	AuthenticatedOperationViolationConflictingPeerAnswer
	AuthenticatedOperationViolationConflictingFinal
	AuthenticatedOperationViolationContinuationAuthority
)

type AuthenticatedOperationViolation struct {
	code AuthenticatedOperationViolationCode
}

func (violation AuthenticatedOperationViolation) Code() AuthenticatedOperationViolationCode {
	return violation.code
}

func (violation AuthenticatedOperationViolation) valid() bool {
	switch violation.code {
	case AuthenticatedOperationViolationMalformedFailure,
		AuthenticatedOperationViolationMalformedPeerControl,
		AuthenticatedOperationViolationConflictingPeerAnswer,
		AuthenticatedOperationViolationConflictingFinal,
		AuthenticatedOperationViolationContinuationAuthority:
		return true
	default:
		return false
	}
}

// InboundAuthenticationResult is a sealed result of authenticating one opened
// message. Its zero value means ordinary authenticated traffic. A non-zero
// result is consumed by ProtocolPump before generic runtime shutdown.
type InboundAuthenticationResult struct {
	operationViolation AuthenticatedOperationViolation
}

func (result InboundAuthenticationResult) HasOperationViolation() bool {
	return result.operationViolation.valid()
}

func authenticatedOperationViolationResult(
	code AuthenticatedOperationViolationCode,
) InboundAuthenticationResult {
	return InboundAuthenticationResult{
		operationViolation: AuthenticatedOperationViolation{code: code},
	}
}

type authenticatedOperationViolationNotification struct {
	handler   func(AuthenticatedOperationViolation)
	violation AuthenticatedOperationViolation
}

func (notification authenticatedOperationViolationNotification) deliver() {
	if notification.handler != nil && notification.violation.valid() {
		notification.handler(notification.violation)
	}
}

func (authority *operationAuthority) recordAuthenticatedOperationViolationLocked(
	violation AuthenticatedOperationViolation,
) {
	if authority == nil || !violation.valid() || authority.authenticatedViolation.valid() {
		return
	}
	authority.authenticatedViolation = violation
}

func (authority *operationAuthority) authenticatedOperationViolationNotificationLocked() authenticatedOperationViolationNotification {
	if authority == nil || authority.authenticatedViolationDelivered ||
		authority.authenticatedViolationHandler == nil || !authority.authenticatedViolation.valid() {
		return authenticatedOperationViolationNotification{}
	}
	authority.authenticatedViolationDelivered = true
	return authenticatedOperationViolationNotification{
		handler: authority.authenticatedViolationHandler, violation: authority.authenticatedViolation,
	}
}

// RegisterAuthenticatedOperationViolationHandler binds one synchronous observer
// to this exact generation. Pending evidence is retained on operation authority,
// so a frame that wins the request-delivery/registration race is still observed.
func (generation OperationGeneration) RegisterAuthenticatedOperationViolationHandler(
	handler func(AuthenticatedOperationViolation),
) error {
	if generation.IsZero() || generation.table == nil || handler == nil {
		return ErrAuthenticatedOperationObserver
	}
	generation.table.mu.Lock()
	generation.table.pruneExpired()
	if generation.table.operationAuthority(generation.operationID) != generation.authority ||
		generation.authority.authenticatedViolationHandler != nil {
		generation.table.mu.Unlock()
		return ErrAuthenticatedOperationObserver
	}
	generation.authority.authenticatedViolationHandler = handler
	notification := generation.authority.authenticatedOperationViolationNotificationLocked()
	generation.table.mu.Unlock()
	notification.deliver()
	return nil
}

func (table *OperationTable) authenticatedOperationViolationNotificationLocked(
	message Message,
) authenticatedOperationViolationNotification {
	operationID, ok := message.OperationID()
	if !ok {
		return authenticatedOperationViolationNotification{}
	}
	authority := table.operationAuthority(operationID)
	return authority.authenticatedOperationViolationNotificationLocked()
}

// RecordAuthenticatedOperationViolation attaches an authenticator-produced fact
// to the operation authority current at the routing linearization point. A
// missing/tombstoned observer does not suppress the caller's session shutdown.
func (table *OperationTable) RecordAuthenticatedOperationViolation(
	message Message,
	violation AuthenticatedOperationViolation,
) (bool, error) {
	if table == nil || !violation.valid() || !violation.matchesAuthenticatedMessage(message) {
		return false, ErrInvalidAuthenticatedOperationViolation
	}
	operationID, ok := message.OperationID()
	if !ok {
		return false, ErrInvalidAuthenticatedOperationViolation
	}
	table.mu.Lock()
	table.pruneExpired()
	authority, requestKind := table.authenticatedOperationAuthorityLocked(operationID)
	if authority == nil {
		table.mu.Unlock()
		return false, nil
	}
	if violation.code == AuthenticatedOperationViolationMalformedPeerControl &&
		requestKind != MessagePeerOffer {
		table.mu.Unlock()
		return false, nil
	}
	authority.recordAuthenticatedOperationViolationLocked(violation)
	notification := authority.authenticatedOperationViolationNotificationLocked()
	table.mu.Unlock()
	notification.deliver()
	return true, nil
}

func (violation AuthenticatedOperationViolation) matchesAuthenticatedMessage(message Message) bool {
	switch violation.code {
	case AuthenticatedOperationViolationMalformedFailure:
		return message.kind == MessageOperationError
	case AuthenticatedOperationViolationMalformedPeerControl:
		return message.kind == MessagePeerAnswer || message.kind == MessagePeerCandidate
	default:
		return false
	}
}

func (table *OperationTable) authenticatedOperationAuthorityLocked(
	operationID OperationID,
) (*operationAuthority, MessageKind) {
	if active, ok := table.active[operationID]; ok {
		return active.authority, active.requestKind
	}
	if tombstone, ok := table.tombstones[operationID]; ok {
		return tombstone.authority, tombstone.requestKind
	}
	return nil, 0
}
