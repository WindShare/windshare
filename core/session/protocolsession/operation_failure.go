package protocolsession

import (
	"bytes"
	"errors"
	"time"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

const (
	OperationScopeDirectory uint8 = 2
	OperationScopeRevision  uint8 = 3
	OperationScopeBlock     uint8 = 4
	OperationScopePeer      uint8 = 5

	PeerOperationCodeNegotiation uint16 = 0x5001
	PeerOperationCodeTimeout     uint16 = 0x5002
	PeerOperationCodeCandidates  uint16 = 0x5003
	PeerOperationCodeAdmission   uint16 = 0x5004

	MaxOperationFailureMessageBytes = 512
	MinOperationFailureRetryAfter   = time.Millisecond
	MaxOperationFailureRetryAfter   = 30 * time.Second

	operationFailureSchemaVersion = uint64(1)
	directoryOperationCodeFirst   = uint16(0x2001)
	directoryOperationCodeLast    = uint16(0x2008)
	revisionOperationCodeFirst    = uint16(0x3001)
	revisionOperationCodeLast     = uint16(0x3008)
	blockOperationCodeFirst       = uint16(0x4001)
	blockOperationCodeLast        = uint16(0x4006)
)

type OperationFailure struct {
	Scope      uint8
	Code       uint16
	Retryable  bool
	RetryAfter time.Duration
	Message    string
}

var ErrInvalidOperationFailure = errors.New("operation failure body is invalid")

// EncodeOperationFailure owns the cross-service wire schema. Keeping it at the
// protocol-session boundary prevents a directory operation from depending on a
// content-only validator that cannot represent its frozen error scope.
func EncodeOperationFailure(failure OperationFailure) ([]byte, error) {
	if !operationFailureCodeInScope(failure.Scope, failure.Code) {
		return nil, errors.New("operation failure code is outside its scope")
	}
	if failure.Scope == OperationScopePeer && failure.Retryable {
		return nil, errors.New("peer operation failures are permanent within one negotiation identity")
	}
	if failure.Message == "" || !utf8.ValidString(failure.Message) ||
		!norm.NFC.IsNormalString(failure.Message) || len(failure.Message) > MaxOperationFailureMessageBytes {
		return nil, errors.New("operation failure message must be non-empty NFC UTF-8 within its byte limit")
	}
	var retryAfter any
	if failure.Retryable {
		if failure.RetryAfter < MinOperationFailureRetryAfter ||
			failure.RetryAfter > MaxOperationFailureRetryAfter ||
			failure.RetryAfter%MinOperationFailureRetryAfter != 0 {
			return nil, errors.New("retryable operation failure delay must be an integral millisecond within its limit")
		}
		retryAfter = uint64(failure.RetryAfter / time.Millisecond)
	} else if failure.RetryAfter != 0 {
		return nil, errors.New("permanent operation failure cannot carry a retry delay")
	}
	return EncodeBody(map[uint64]any{
		0: operationFailureSchemaVersion, 1: uint64(failure.Scope), 2: uint64(failure.Code),
		3: failure.Retryable, 4: retryAfter, 5: failure.Message,
	})
}

// DecodeOperationFailure is the receive-side authority for signed OPERATION_ERROR
// semantics. Signature verification proves who sent the bytes; this decoder
// separately proves that the authenticated value belongs to the frozen schema.
func DecodeOperationFailure(encoded []byte) (OperationFailure, error) {
	if err := validateCanonicalBody(encoded); err != nil {
		return OperationFailure{}, errors.Join(ErrInvalidOperationFailure, err)
	}
	var fields map[uint64]any
	if err := messageDecMode.Unmarshal(encoded, &fields); err != nil || len(fields) != 6 {
		return OperationFailure{}, ErrInvalidOperationFailure
	}
	for key := uint64(0); key <= 5; key++ {
		if _, exists := fields[key]; !exists {
			return OperationFailure{}, ErrInvalidOperationFailure
		}
	}
	version, versionOK := fields[0].(uint64)
	scope, scopeOK := fields[1].(uint64)
	code, codeOK := fields[2].(uint64)
	retryable, retryableOK := fields[3].(bool)
	message, messageOK := fields[5].(string)
	if !versionOK || version != operationFailureSchemaVersion || !scopeOK || scope > 255 ||
		!codeOK || code > 65_535 || !retryableOK || !messageOK {
		return OperationFailure{}, ErrInvalidOperationFailure
	}
	var retryAfter time.Duration
	if retryable {
		milliseconds, ok := fields[4].(uint64)
		if !ok || milliseconds < uint64(MinOperationFailureRetryAfter/time.Millisecond) ||
			milliseconds > uint64(MaxOperationFailureRetryAfter/time.Millisecond) {
			return OperationFailure{}, ErrInvalidOperationFailure
		}
		retryAfter = time.Duration(milliseconds) * time.Millisecond
	} else if fields[4] != nil {
		return OperationFailure{}, ErrInvalidOperationFailure
	}
	failure := OperationFailure{
		Scope: uint8(scope), Code: uint16(code), Retryable: retryable,
		RetryAfter: retryAfter, Message: message,
	}
	canonical, err := EncodeOperationFailure(failure)
	if err != nil || !bytes.Equal(canonical, encoded) {
		return OperationFailure{}, errors.Join(ErrInvalidOperationFailure, err)
	}
	return failure, nil
}

func operationFailureCodeInScope(scope uint8, code uint16) bool {
	switch scope {
	case OperationScopeDirectory:
		return code >= directoryOperationCodeFirst && code <= directoryOperationCodeLast
	case OperationScopeRevision:
		return code >= revisionOperationCodeFirst && code <= revisionOperationCodeLast
	case OperationScopeBlock:
		return code >= blockOperationCodeFirst && code <= blockOperationCodeLast
	case OperationScopePeer:
		return code >= PeerOperationCodeNegotiation && code <= PeerOperationCodeAdmission
	default:
		return false
	}
}

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
	AuthenticatedOperationViolationMalformedFailure AuthenticatedOperationViolationCode = iota + 1
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
	return violation.code >= AuthenticatedOperationViolationMalformedFailure &&
		violation.code <= AuthenticatedOperationViolationContinuationAuthority
}

// NewAuthenticatedOperationViolation constructs a verified violation value.
func NewAuthenticatedOperationViolation(code AuthenticatedOperationViolationCode) (AuthenticatedOperationViolation, error) {
	violation := AuthenticatedOperationViolation{code: code}
	if !violation.valid() {
		return AuthenticatedOperationViolation{}, ErrInvalidAuthenticatedOperationViolation
	}
	return violation, nil
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
	code AuthenticatedOperationViolationCode,
) {
	violation := AuthenticatedOperationViolation{code: code}
	if authority == nil || !violation.valid() || authority.authenticatedViolation.valid() {
		return
	}
	authority.authenticatedViolation = violation
}

func (authority *operationAuthority) recordSenderOperationViolationLocked(
	direction Direction,
	code AuthenticatedOperationViolationCode,
) {
	if direction == DirectionSenderToReceiver {
		authority.recordAuthenticatedOperationViolationLocked(code)
	}
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
	authority.recordAuthenticatedOperationViolationLocked(violation.code)
	notification := authority.authenticatedOperationViolationNotificationLocked()
	table.mu.Unlock()
	notification.deliver()
	return true, nil
}

func (violation AuthenticatedOperationViolation) matchesAuthenticatedMessage(message Message) bool {
	return (violation.code == AuthenticatedOperationViolationMalformedFailure &&
		message.kind == MessageOperationError) ||
		(violation.code == AuthenticatedOperationViolationMalformedPeerControl &&
			(message.kind == MessagePeerAnswer || message.kind == MessagePeerCandidate))
}

func (table *OperationTable) authenticatedOperationAuthorityLocked(
	operationID OperationID,
) (*operationAuthority, MessageKind) {
	if active, ok := table.active[operationID]; ok {
		return active.authority, active.requestKind
	}
	tombstone := table.tombstones[operationID]
	return tombstone.authority, tombstone.requestKind
}

func (table *OperationTable) addTombstone(
	operationID OperationID,
	direction Direction,
	message Message,
	cancelled bool,
	requestKind MessageKind,
	requestFingerprint [32]byte,
	authority *operationAuthority,
) error {
	if _, exists := table.tombstones[operationID]; !exists && len(table.tombstones) >= table.limits.MaxTombstones {
		return ErrTombstoneBudget
	}
	if authority == nil {
		authority = &operationAuthority{}
	}
	table.tombstones[operationID] = operationTombstone{
		expiresAt:          table.now().Add(OperationTombstoneLifetime),
		requestKind:        requestKind,
		requestFingerprint: requestFingerprint,
		finalKind:          message.kind,
		fingerprint:        message.operationFingerprint(direction),
		cancelled:          cancelled,
		authority:          authority,
	}
	return nil
}

func (table *OperationTable) pruneExpired() {
	now := table.now()
	for operationID, tombstone := range table.tombstones {
		if !now.Before(tombstone.expiresAt) && tombstone.authority.pins == 0 {
			delete(table.tombstones, operationID)
		}
	}
}

func (table *OperationTable) observeTombstone(
	operationID OperationID,
	tombstone operationTombstone,
	direction Direction,
	message Message,
) (OperationDisposition, error) {
	// CANCEL is a race signal rather than conflicting result content. If a final
	// was already admitted, that final wins; tearing down the ProtocolSession for
	// a normal cross-lane cancellation race would make multi-lane use unstable.
	if message.kind == MessageCancel {
		return OperationDrop, nil
	}
	if tombstone.cancelled {
		return table.observeCancelledTombstone(operationID, tombstone, direction, message)
	}
	if direction == DirectionReceiverToSender && message.kind.isRequest() {
		if tombstone.requestKind == message.kind &&
			tombstone.requestFingerprint == message.operationFingerprint(direction) {
			return OperationDrop, nil
		}
		return OperationDrop, ErrOperationIDReused
	}
	if message.kind.isFinal() || message.kind == MessageLaneAttach {
		if tombstone.finalKind == message.kind && tombstone.fingerprint == message.operationFingerprint(direction) {
			return OperationDrop, nil
		}
		tombstone.authority.recordSenderOperationViolationLocked(
			direction, AuthenticatedOperationViolationConflictingFinal,
		)
		return OperationDrop, ErrConflictingFinal
	}
	if messageAllowedForOperation(tombstone.requestKind, message.kind) {
		return OperationDrop, nil
	}
	return OperationDrop, ErrOperationIDReused
}

func (table *OperationTable) observeCancelledTombstone(
	operationID OperationID,
	tombstone operationTombstone,
	direction Direction,
	message Message,
) (OperationDisposition, error) {
	isRequest := direction == DirectionReceiverToSender &&
		(message.kind.isRequest() || message.kind == MessageLaneAttach)
	if tombstone.requestKind == 0 {
		return table.bindPreemptivelyCancelledRequestLocked(operationID, tombstone, direction, message, isRequest)
	}
	if isRequest {
		if tombstone.requestKind == message.kind &&
			tombstone.requestFingerprint == message.operationFingerprint(direction) {
			return OperationDrop, nil
		}
		return OperationDrop, ErrOperationIDReused
	}
	if !messageAllowedForOperation(tombstone.requestKind, message.kind) {
		return OperationDrop, ErrOperationIDReused
	}
	if message.kind.isFinal() ||
		(tombstone.requestKind == MessageLaneAttach && message.kind == MessageLaneAttach) {
		fingerprint := message.operationFingerprint(direction)
		if tombstone.finalKind == MessageCancel {
			// The first compatible final may have crossed the cancellation on another
			// lane. Remembering it makes only exact repeats idempotent thereafter.
			tombstone.finalKind = message.kind
			tombstone.fingerprint = fingerprint
			table.tombstones[operationID] = tombstone
			return OperationDrop, nil
		}
		if tombstone.finalKind != message.kind || tombstone.fingerprint != fingerprint {
			tombstone.authority.recordSenderOperationViolationLocked(
				direction, AuthenticatedOperationViolationConflictingFinal,
			)
			return OperationDrop, ErrConflictingFinal
		}
	}
	return OperationDrop, nil
}

func (table *OperationTable) bindPreemptivelyCancelledRequestLocked(
	operationID OperationID,
	tombstone operationTombstone,
	direction Direction,
	message Message,
	isRequest bool,
) (OperationDisposition, error) {
	if !isRequest {
		return OperationDrop, ErrOperationIDReused
	}
	authority, err := table.newOperationAuthority(message)
	if err != nil {
		return OperationDrop, err
	}
	if !deferredContinuationMatches(tombstone.authority, authority) {
		tombstone.authority.recordSenderOperationViolationLocked(
			direction, AuthenticatedOperationViolationContinuationAuthority,
		)
		return OperationDrop, ErrConflictingContinuation
	}
	// A preemptive CANCEL learns the request family from the raced request.
	// Retaining it narrows every later drop to that operation's continuations.
	tombstone.requestKind = message.kind
	tombstone.requestFingerprint = message.operationFingerprint(direction)
	tombstone.authority.continuations = authority.continuations
	tombstone.authority.deferredContinuationScope = OperationContinuationScope{}
	tombstone.authority.hasDeferredContinuationScope = false
	table.tombstones[operationID] = tombstone
	return OperationDrop, nil
}

// AcceptOutboundReplay validates an exact permit without repeating the normal
// state transition. Final expiry and PeerAnswer multiplicity remain unchanged.
func (table *OperationTable) AcceptOutboundReplay(
	direction Direction,
	message Message,
	permit OutboundReplayPermit,
) (OutboundAdmission, error) {
	if table == nil {
		return OutboundAdmission{Disposition: OperationDrop}, errors.New("protocolsession: nil operation table")
	}
	table.mu.Lock()
	defer table.mu.Unlock()
	table.pruneExpired()
	if table.terminal {
		return OutboundAdmission{Disposition: OperationDrop}, nil
	}
	operationID, ok := message.OperationID()
	if !ok || permit.table != table || permit.authority == nil || permit.direction != direction ||
		permit.kind != message.kind || permit.operationID != operationID {
		return OutboundAdmission{Disposition: OperationDrop}, ErrOperationIDReused
	}
	if tombstone, found := table.tombstones[operationID]; found {
		return table.acceptTombstoneReplayLocked(operationID, tombstone, direction, message, permit)
	}
	active, found := table.active[operationID]
	if !found || active.authority != permit.authority {
		return OutboundAdmission{Disposition: OperationDrop}, nil
	}
	return table.acceptActiveReplayLocked(operationID, active, direction, message, permit)
}

func (table *OperationTable) acceptTombstoneReplayLocked(
	operationID OperationID,
	tombstone operationTombstone,
	direction Direction,
	message Message,
	permit OutboundReplayPermit,
) (OutboundAdmission, error) {
	if tombstone.authority != permit.authority {
		return OutboundAdmission{Disposition: OperationDrop}, nil
	}
	if err := validateReplayFingerprint(message, direction, permit); err != nil {
		return OutboundAdmission{Disposition: OperationDrop}, err
	}
	if tombstone.cancelled && (direction != DirectionReceiverToSender || message.kind != MessageCancel) {
		return OutboundAdmission{Disposition: OperationDrop}, nil
	}
	if tombstone.cancelled {
		return table.replayAdmissionLocked(operationID, tombstone.authority, permit)
	}
	if !message.kind.isFinal() && message.kind != MessageLaneAttach {
		return OutboundAdmission{Disposition: OperationDrop}, nil
	}
	if tombstone.finalKind != message.kind || tombstone.fingerprint != permit.fingerprint {
		return OutboundAdmission{Disposition: OperationDrop}, nil
	}
	return table.replayAdmissionLocked(operationID, tombstone.authority, permit)
}

func (table *OperationTable) acceptActiveReplayLocked(
	operationID OperationID,
	active activeOperation,
	direction Direction,
	message Message,
	permit OutboundReplayPermit,
) (OutboundAdmission, error) {
	if err := validateReplayFingerprint(message, direction, permit); err != nil {
		return OutboundAdmission{Disposition: OperationDrop}, err
	}
	if direction == DirectionReceiverToSender && message.kind.isRequest() {
		if active.requestKind != message.kind || active.requestFingerprint != permit.fingerprint {
			return OutboundAdmission{Disposition: OperationDrop}, ErrOperationIDReused
		}
		return table.replayAdmissionLocked(operationID, active.authority, permit)
	}
	if !messageAllowedForOperation(active.requestKind, message.kind) {
		return OutboundAdmission{Disposition: OperationDrop}, ErrUnexpectedOperation
	}
	// Reserve only after every fallible admission precondition. A failed replay
	// must not leave a pending semantic fingerprint that suppresses its retry.
	if active.authority.pins >= MaximumOperationPins {
		return OutboundAdmission{Disposition: OperationDrop}, ErrOperationPinBudget
	}
	continuation, drop, err := table.reserveReplayContinuationLocked(active.authority, direction, message)
	if err != nil {
		return OutboundAdmission{Disposition: OperationDrop}, err
	}
	if drop {
		return OutboundAdmission{Disposition: OperationDrop}, nil
	}
	admission, err := table.replayAdmissionLocked(operationID, active.authority, permit)
	admission.continuation = continuation
	return admission, err
}

func (table *OperationTable) replayAdmissionLocked(
	operationID OperationID,
	authority *operationAuthority,
	permit OutboundReplayPermit,
) (OutboundAdmission, error) {
	if authority.pins >= MaximumOperationPins {
		return OutboundAdmission{Disposition: OperationDrop}, ErrOperationPinBudget
	}
	return OutboundAdmission{
		Disposition: OperationDeliver,
		Generation: OperationGeneration{
			table: table, authority: authority, operationID: operationID,
		},
		Replay: permit,
		pin:    table.pinLocked(operationID, authority, false),
	}, nil
}

func validateReplayFingerprint(
	message Message,
	direction Direction,
	permit OutboundReplayPermit,
) error {
	if permit.fingerprint == message.operationFingerprint(direction) {
		return nil
	}
	if message.kind.isFinal() || message.kind == MessageLaneAttach {
		return ErrConflictingFinal
	}
	return ErrOperationIDReused
}
