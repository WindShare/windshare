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

	PeerOperationCodeFirst            uint16 = 0x5000
	PeerOperationCodeLast             uint16 = 0x5fff
	PeerOperationCodeNegotiation      uint16 = 0x5001
	PeerOperationCodeTimeout          uint16 = 0x5002
	PeerOperationCodeCandidates       uint16 = 0x5003
	PeerOperationCodeAdmission        uint16 = 0x5004
	PeerOperationCodeICE              uint16 = 0x5005
	PeerOperationCodeSTUN             uint16 = 0x5006
	PeerOperationCodeTransport        uint16 = 0x5007
	PeerOperationCodeDTLS             uint16 = 0x5008
	PeerOperationCodePolicy           uint16 = 0x5009
	PeerOperationCodeAuthentication   uint16 = 0x500a
	PeerOperationCodeSessionInvariant uint16 = 0x500b
	PeerOperationCodeCapacity         uint16 = 0x500c

	MaxOperationFailureMessageBytes = 512
	MinOperationFailureRetryAfter   = time.Millisecond
	MaxOperationFailureRetryAfter   = 30 * time.Second

	operationFailureSchemaVersion     = uint64(1)
	peerOperationFailureSchemaVersion = uint64(3)
	directoryOperationCodeFirst       = uint16(0x2001)
	directoryOperationCodeLast        = uint16(0x2008)
	revisionOperationCodeFirst        = uint16(0x3001)
	revisionOperationCodeLast         = uint16(0x3008)
	blockOperationCodeFirst           = uint16(0x4001)
	blockOperationCodeLast            = uint16(0x4006)
)

type OperationFailure struct {
	Scope       uint8
	Code        uint16
	Retryable   bool
	RetryAfter  time.Duration
	Message     string
	PeerAttempt *PeerAttemptBinding
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
	fields := map[uint64]any{
		0: operationFailureSchemaVersion, 1: uint64(failure.Scope), 2: uint64(failure.Code),
		3: failure.Retryable, 4: retryAfter, 5: failure.Message,
	}
	if failure.Scope == OperationScopePeer {
		if failure.PeerAttempt == nil || !failure.PeerAttempt.Valid() {
			return nil, ErrInvalidOperationFailure
		}
		fields[0] = peerOperationFailureSchemaVersion
		fields[6] = []any{failure.PeerAttempt.PeerPathID[:], failure.PeerAttempt.AttemptID[:], failure.PeerAttempt.AttemptSequence, uint64(failure.PeerAttempt.PeerSlot)}
	} else if failure.PeerAttempt != nil {
		return nil, ErrInvalidOperationFailure
	}
	return EncodeBody(fields)
}

// DecodeOperationFailure is the receive-side authority for OPERATION_ERROR semantics.
func DecodeOperationFailure(encoded []byte) (OperationFailure, error) {
	if err := validateCanonicalBody(encoded); err != nil {
		return OperationFailure{}, errors.Join(ErrInvalidOperationFailure, err)
	}
	var fields map[uint64]any
	if err := messageDecMode.Unmarshal(encoded, &fields); err != nil || (len(fields) != 6 && len(fields) != 7) {
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
	if !versionOK || !scopeOK || scope > 255 ||
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
	if failure.Scope == OperationScopePeer {
		if version != peerOperationFailureSchemaVersion || len(fields) != 7 {
			return OperationFailure{}, ErrInvalidOperationFailure
		}
		binding, err := decodePeerAttemptBinding(fields[6])
		if err != nil {
			return OperationFailure{}, err
		}
		failure.PeerAttempt = &binding
	} else if version != operationFailureSchemaVersion || len(fields) != 6 {
		return OperationFailure{}, ErrInvalidOperationFailure
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
		return code >= PeerOperationCodeFirst && code <= PeerOperationCodeLast
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
// sender-control authentication but before ordinary operation dispatch.
type AuthenticatedOperationViolationCode uint8

const (
	AuthenticatedOperationViolationMalformedFailure AuthenticatedOperationViolationCode = iota + 1
	AuthenticatedOperationViolationMalformedPeerControl
	AuthenticatedOperationViolationConflictingPeerAnswer
	AuthenticatedOperationViolationConflictingFinal
	AuthenticatedOperationViolationContinuationContract
)

type AuthenticatedOperationViolation struct {
	code AuthenticatedOperationViolationCode
}

func (violation AuthenticatedOperationViolation) Code() AuthenticatedOperationViolationCode {
	return violation.code
}

func (violation AuthenticatedOperationViolation) Valid() bool {
	return violation.code >= AuthenticatedOperationViolationMalformedFailure &&
		violation.code <= AuthenticatedOperationViolationContinuationContract
}

func (violation AuthenticatedOperationViolation) MatchesAuthenticatedMessage(message Message) bool {
	return (violation.code == AuthenticatedOperationViolationMalformedFailure &&
		message.kind == MessageOperationError) ||
		(violation.code == AuthenticatedOperationViolationMalformedPeerControl &&
			(message.kind == MessagePeerAnswer || message.kind == MessagePeerCandidate))
}

func NewAuthenticatedOperationViolation(code AuthenticatedOperationViolationCode) (AuthenticatedOperationViolation, error) {
	violation := AuthenticatedOperationViolation{code: code}
	if !violation.Valid() {
		return AuthenticatedOperationViolation{}, ErrInvalidAuthenticatedOperationViolation
	}
	return violation, nil
}

// InboundValidationResult is a sealed result of validating one authenticated message.
type InboundValidationResult struct {
	operationViolation AuthenticatedOperationViolation
}

func (result InboundValidationResult) HasOperationViolation() bool {
	return result.operationViolation.Valid()
}

func (result InboundValidationResult) OperationViolation() AuthenticatedOperationViolation {
	return result.operationViolation
}

func authenticatedOperationViolationResult(
	code AuthenticatedOperationViolationCode,
) InboundValidationResult {
	return InboundValidationResult{
		operationViolation: AuthenticatedOperationViolation{code: code},
	}
}

type PeerFailureRecoveryScope string

const (
	PeerFailureAttemptTransient PeerFailureRecoveryScope = "attempt-transient"
	PeerFailureResourceDeferred PeerFailureRecoveryScope = "resource-deferred"
	PeerFailurePathTerminal     PeerFailureRecoveryScope = "path-terminal"
	PeerFailureSessionTerminal  PeerFailureRecoveryScope = "session-terminal"
)

// PeerFailureScope grants recovery authority only to known typed reasons. Text
// and Retryable never revive an operation, and an unknown reason cannot confer
// session termination authority or an automatic retry.
func PeerFailureScope(code uint16) PeerFailureRecoveryScope {
	switch code {
	case PeerOperationCodeCapacity:
		return PeerFailureResourceDeferred
	case PeerOperationCodeNegotiation, PeerOperationCodeTimeout, PeerOperationCodeICE,
		PeerOperationCodeSTUN, PeerOperationCodeTransport, PeerOperationCodeDTLS:
		return PeerFailureAttemptTransient
	case PeerOperationCodeAuthentication, PeerOperationCodeSessionInvariant:
		return PeerFailureSessionTerminal
	default:
		return PeerFailurePathTerminal
	}
}
