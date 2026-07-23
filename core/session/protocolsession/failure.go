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
	peerOperationCodeFirst        = PeerOperationCodeNegotiation
	peerOperationCodeLast         = PeerOperationCodeAdmission
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
	var first, last uint16
	switch scope {
	case OperationScopeDirectory:
		first, last = directoryOperationCodeFirst, directoryOperationCodeLast
	case OperationScopeRevision:
		first, last = revisionOperationCodeFirst, revisionOperationCodeLast
	case OperationScopeBlock:
		first, last = blockOperationCodeFirst, blockOperationCodeLast
	case OperationScopePeer:
		first, last = peerOperationCodeFirst, peerOperationCodeLast
	default:
		return false
	}
	return code >= first && code <= last
}
