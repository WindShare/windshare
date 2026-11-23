// Package protocolsession implements the authenticated end-to-end session
// primitives shared by protocol roles. Transport and operation routing remain
// outside this package so relay identity can never become an authorization input.
package protocolsession

import (
	"bytes"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strconv"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"github.com/windshare/windshare/core/catalog"
)

const (
	IdentityBytes    = catalog.IdentityBytes
	OperationIDBytes = 8
)

var ErrIdentityLength = errors.New("protocol session identity must be exactly 16 bytes")

type (
	ProtocolSessionID  [IdentityBytes]byte
	ReceiverInstanceID [IdentityBytes]byte
	// OperationID is a session-scoped monotonically increasing 64-bit counter
	// issued by the receiver starting at 1. Zero denotes absence.
	OperationID uint64
)

func identityFromBytes[T ~[IdentityBytes]byte](raw []byte) (T, error) {
	var value T
	if len(raw) != IdentityBytes {
		return value, fmt.Errorf("%w: got %d", ErrIdentityLength, len(raw))
	}
	copy(value[:], raw)
	return value, nil
}

func identityBytes[T ~[IdentityBytes]byte](value T) []byte {
	result := make([]byte, IdentityBytes)
	copy(result, value[:])
	return result
}

func identityEqual[T ~[IdentityBytes]byte](left, right T) bool {
	return subtle.ConstantTimeCompare(left[:], right[:]) == 1
}

func ProtocolSessionIDFromBytes(raw []byte) (ProtocolSessionID, error) {
	return identityFromBytes[ProtocolSessionID](raw)
}

func ReceiverInstanceIDFromBytes(raw []byte) (ReceiverInstanceID, error) {
	return identityFromBytes[ReceiverInstanceID](raw)
}

func OperationIDFromUint64(raw uint64) (OperationID, error) {
	if raw == 0 {
		return 0, ErrInvalidOperationID
	}
	return OperationID(raw), nil
}

func OperationIDFromBigEndian(raw []byte) (OperationID, error) {
	if len(raw) != OperationIDBytes {
		return 0, fmt.Errorf("%w: got %d bytes", ErrIdentityLength, len(raw))
	}
	return OperationIDFromUint64(binary.BigEndian.Uint64(raw))
}

func (id ProtocolSessionID) Bytes() []byte  { return identityBytes(id) }
func (id ReceiverInstanceID) Bytes() []byte { return identityBytes(id) }

func (id ProtocolSessionID) IsZero() bool  { return id == ProtocolSessionID{} }
func (id ReceiverInstanceID) IsZero() bool { return id == ReceiverInstanceID{} }
func (id OperationID) IsZero() bool        { return id == 0 }

func (id ProtocolSessionID) Equal(other ProtocolSessionID) bool { return identityEqual(id, other) }
func (id ReceiverInstanceID) Equal(other ReceiverInstanceID) bool {
	return identityEqual(id, other)
}
func (id OperationID) Equal(other OperationID) bool { return id == other }
func (id OperationID) Uint64() uint64               { return uint64(id) }
func (id OperationID) String() string               { return strconv.FormatUint(uint64(id), 10) }

func (id OperationID) PutBigEndian(dst []byte) {
	binary.BigEndian.PutUint64(dst, uint64(id))
}

func (id OperationID) AppendBigEndian(dst []byte) []byte {
	return binary.BigEndian.AppendUint64(dst, uint64(id))
}

type SenderControlSemanticValidator interface {
	ValidateSenderControl(MessageKind, OperationID, []byte) error
}

type SenderControlSemanticValidatorFunc func(MessageKind, OperationID, []byte) error

func (validate SenderControlSemanticValidatorFunc) ValidateSenderControl(
	kind MessageKind,
	operationID OperationID,
	semantic []byte,
) error {
	if validate == nil {
		return ErrControlSemantic
	}
	return validate(kind, operationID, semantic)
}

// SenderControlSemanticRule binds one sender control kind to its typed
// decoder. Keeping this registry at the authentication boundary prevents a new
// final kind from silently bypassing semantic validation before routing.
type SenderControlSemanticRule struct {
	Kind     MessageKind
	Validate SenderControlSemanticValidatorFunc
}

type SenderControlSemanticRegistry struct {
	validators map[MessageKind]SenderControlSemanticValidatorFunc
}

func NewSenderControlSemanticRegistry(
	rules ...SenderControlSemanticRule,
) (*SenderControlSemanticRegistry, error) {
	if len(rules) == 0 {
		return nil, ErrControlSemantic
	}
	validators := make(map[MessageKind]SenderControlSemanticValidatorFunc, len(rules))
	for _, rule := range rules {
		if rule.Validate == nil {
			return nil, ErrControlSemantic
		}
		if !isSenderControl(rule.Kind) {
			return nil, ErrControlSemantic
		}
		if _, exists := validators[rule.Kind]; exists {
			return nil, ErrControlSemantic
		}
		validators[rule.Kind] = rule.Validate
	}
	return &SenderControlSemanticRegistry{validators: validators}, nil
}

func (registry *SenderControlSemanticRegistry) ValidateSenderControl(
	kind MessageKind,
	operationID OperationID,
	semantic []byte,
) error {
	if registry == nil {
		return ErrControlSemantic
	}
	validate := registry.validators[kind]
	if validate == nil {
		return ErrControlSemantic
	}
	return validate(kind, operationID, semantic)
}

// Core control schemas are validated before operation admission
// even when the composing runtime has no catalog, content, or peer dependency.
func validateSenderControlSemantic(
	external SenderControlSemanticValidator,
	kind MessageKind,
	operationID OperationID,
	semantic []byte,
) error {
	var err error
	switch kind {
	case MessageLaneAttach:
		_, err = DecodeLaneGrant(semantic, operationID)
	case MessageOperationError:
		_, err = DecodeOperationFailure(semantic)
	case MessageScanProgress:
		_, err = DecodeScanProgress(semantic)
	case MessagePeerPathControl, MessageTURNControl:
		err = ValidateSessionControlBody(RoleSender, kind, semantic)
	case MessageSessionTerminal:
		_, err = DecodeSessionTerminal(semantic)
	case MessageOperationRetry:
		_, err = DecodeOperationRetry(semantic)
	default:
		if external == nil {
			return ErrControlSemantic
		}
		err = external.ValidateSenderControl(kind, operationID, semantic)
	}
	if err != nil {
		return fmt.Errorf("%w: %w", ErrControlSemantic, err)
	}
	return nil
}

// The authenticated outer kind selects the schema. Body versions remain local
// to each protocol, so a schema revision cannot redirect another control family.
func ValidateSessionControlBody(origin Role, kind MessageKind, body []byte) error {
	switch kind {
	case MessageTURNControl:
		if origin == RoleReceiver {
			_, err := DecodeTURNDemand(body)
			return err
		}
		_, err := DecodeTURNGrants(body)
		return err
	case MessagePeerPathControl:
		_, err := DecodePeerPathControl(body)
		return err
	default:
		return ErrUnknownMessageKind
	}
}

const (
	SessionTerminalCodeFirst       = uint16(0x1001)
	SessionTerminalCodeLast        = uint16(0x1008)
	MaxSessionTerminalMessageBytes = 512
	controlSemanticSchemaVersion   = uint64(1)
)

var (
	ErrInvalidScanProgress    = errors.New("scan progress body is invalid")
	ErrInvalidSessionTerminal = errors.New("session terminal body is invalid")
)

type ScanProgress struct {
	AttemptID         catalog.ScanAttemptID
	DiscoveredEntries uint64
}

func EncodeScanProgress(progress ScanProgress) ([]byte, error) {
	if progress.AttemptID.IsZero() {
		return nil, ErrInvalidScanProgress
	}
	return EncodeBody(map[uint64]any{
		0: controlSemanticSchemaVersion,
		1: progress.AttemptID.Bytes(),
		2: progress.DiscoveredEntries,
	})
}

func DecodeScanProgress(encoded []byte) (ScanProgress, error) {
	if err := validateCanonicalBody(encoded); err != nil {
		return ScanProgress{}, errors.Join(ErrInvalidScanProgress, err)
	}
	var fields map[uint64]any
	if err := messageDecMode.Unmarshal(encoded, &fields); err != nil || len(fields) != 3 {
		return ScanProgress{}, ErrInvalidScanProgress
	}
	version, versionOK := fields[0].(uint64)
	attemptBytes, attemptOK := fields[1].([]byte)
	discovered, discoveredOK := fields[2].(uint64)
	if !versionOK || version != controlSemanticSchemaVersion || !attemptOK || !discoveredOK {
		return ScanProgress{}, ErrInvalidScanProgress
	}
	attempt, err := catalog.ScanAttemptIDFromBytes(attemptBytes)
	if err != nil || attempt.IsZero() {
		return ScanProgress{}, ErrInvalidScanProgress
	}
	progress := ScanProgress{AttemptID: attempt, DiscoveredEntries: discovered}
	canonical, err := EncodeScanProgress(progress)
	if err != nil || !bytes.Equal(canonical, encoded) {
		return ScanProgress{}, errors.Join(ErrInvalidScanProgress, err)
	}
	return progress, nil
}

type SessionTerminal struct {
	Code    uint16
	Message string
}

func EncodeSessionTerminal(terminal SessionTerminal) ([]byte, error) {
	if terminal.Code < SessionTerminalCodeFirst || terminal.Code > SessionTerminalCodeLast ||
		terminal.Message == "" || !utf8.ValidString(terminal.Message) ||
		!norm.NFC.IsNormalString(terminal.Message) ||
		len(terminal.Message) > MaxSessionTerminalMessageBytes {
		return nil, ErrInvalidSessionTerminal
	}
	return EncodeBody(map[uint64]any{
		0: controlSemanticSchemaVersion,
		1: uint64(terminal.Code),
		2: terminal.Message,
	})
}

func DecodeSessionTerminal(encoded []byte) (SessionTerminal, error) {
	if err := validateCanonicalBody(encoded); err != nil {
		return SessionTerminal{}, errors.Join(ErrInvalidSessionTerminal, err)
	}
	var fields map[uint64]any
	if err := messageDecMode.Unmarshal(encoded, &fields); err != nil || len(fields) != 3 {
		return SessionTerminal{}, ErrInvalidSessionTerminal
	}
	version, versionOK := fields[0].(uint64)
	code, codeOK := fields[1].(uint64)
	message, messageOK := fields[2].(string)
	if !versionOK || version != controlSemanticSchemaVersion || !codeOK ||
		code > math.MaxUint16 || !messageOK {
		return SessionTerminal{}, ErrInvalidSessionTerminal
	}
	terminal := SessionTerminal{Code: uint16(code), Message: message}
	canonical, err := EncodeSessionTerminal(terminal)
	if err != nil || !bytes.Equal(canonical, encoded) {
		return SessionTerminal{}, errors.Join(ErrInvalidSessionTerminal, err)
	}
	return terminal, nil
}

// OperationRetryReason identifies why the sender declined to admit a request
// without terminating the session, asking the receiver to reissue it with a
// fresh OperationID.
type OperationRetryReason uint8

const (
	OperationRetryReasonReplayWindow OperationRetryReason = 1
)

var ErrInvalidOperationRetry = errors.New("operation retry body is invalid")

func EncodeOperationRetry(reason OperationRetryReason) ([]byte, error) {
	if reason != OperationRetryReasonReplayWindow {
		return nil, ErrInvalidOperationRetry
	}
	return EncodeBody(map[uint64]any{
		0: controlSemanticSchemaVersion,
		1: uint64(reason),
	})
}

func DecodeOperationRetry(encoded []byte) (OperationRetryReason, error) {
	if err := validateCanonicalBody(encoded); err != nil {
		return 0, errors.Join(ErrInvalidOperationRetry, err)
	}
	var fields map[uint64]any
	if err := messageDecMode.Unmarshal(encoded, &fields); err != nil || len(fields) != 2 {
		return 0, ErrInvalidOperationRetry
	}
	version, versionOK := fields[0].(uint64)
	reasonValue, reasonOK := fields[1].(uint64)
	if !versionOK || version != controlSemanticSchemaVersion || !reasonOK ||
		reasonValue != uint64(OperationRetryReasonReplayWindow) {
		return 0, ErrInvalidOperationRetry
	}
	reason := OperationRetryReason(reasonValue)
	canonical, err := EncodeOperationRetry(reason)
	if err != nil || !bytes.Equal(canonical, encoded) {
		return 0, errors.Join(ErrInvalidOperationRetry, err)
	}
	return reason, nil
}
