package protocolsession

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/fxamacker/cbor/v2"
	"github.com/windshare/windshare/core/catalog"
)

const (
	controlWrapperVersionKey  uint64 = 0
	controlWrapperSemanticKey uint64 = 1
	controlSignatureKey       uint64 = 255
	controlWrapperVersion            = 1
)

const (
	controlOperationDomain = "windshare/v2 control/operation"
	controlTerminalDomain  = "windshare/v2 control/session-terminal"
	controlLaneDomain      = "windshare/v2 control/lane-attach"
)

var (
	ErrControlBinding    = errors.New("sender control signature binding is invalid")
	ErrControlBody       = errors.New("sender control wrapper is invalid")
	ErrControlSemantic   = errors.New("sender control semantic body is invalid")
	ErrControlSignature  = errors.New("sender control signature is invalid")
	ErrControlSigningKey = errors.New("sender control signing key is invalid")
)

type ControlDomain uint8

const (
	ControlDomainOperation ControlDomain = iota + 1
	ControlDomainSessionTerminal
	ControlDomainLaneAttach
)

type ControlBinding struct {
	ShareInstance     catalog.ShareInstance
	ProtocolSessionID ProtocolSessionID
	LaneID            uint32
	LaneEpoch         uint32
	Direction         Direction
	Sequence          uint64
	MessageKind       MessageKind
	OperationID       OperationID
	HasOperationID    bool
}

// SenderControlAuthenticator is receiver-side proof that traffic-key authority
// did not substitute for sender-key authority. Its lane binding is fixed at
// construction; only the opener-authenticated sequence and decoded message vary.
type SenderControlAuthenticator struct {
	senderPublicKey ed25519.PublicKey
	base            ControlBinding
	semantic        SenderControlSemanticValidator
}

// PreparedSenderControl contains a fixed-size callback rather than signed bytes.
// Only SessionWriter can invoke it, after it owns the next envelope sequence.
// This prevents a caller from signing a guessed sequence and later emitting the
// control under a different delivery identity.
type PreparedSenderControl struct {
	plaintextBytes int
	kind           MessageKind
	intent         Message
	build          sequencedMessageBuilder
}

func NewSenderControlAuthenticator(
	senderPublicKey ed25519.PublicKey,
	base ControlBinding,
	semantic SenderControlSemanticValidator,
) (*SenderControlAuthenticator, error) {
	if len(senderPublicKey) != ed25519.PublicKeySize {
		return nil, ErrControlSigningKey
	}
	if err := validateControlBase(base); err != nil {
		return nil, ErrControlBinding
	}
	return &SenderControlAuthenticator{
		senderPublicKey: append(ed25519.PublicKey(nil), senderPublicKey...), base: base, semantic: semantic,
	}, nil
}

func (a *SenderControlAuthenticator) AuthenticateInbound(
	sequence uint64,
	message Message,
) (InboundAuthenticationResult, error) {
	return a.authenticate(sequence, message)
}

// SenderControlSemanticBody returns the canonical typed body after the pump has
// authenticated the delivery wrapper. Business decoders never observe wrapper
// versioning or the delivery-only signature field.
func SenderControlSemanticBody(message Message) ([]byte, error) {
	if _, err := senderControlDomain(message.kind); err != nil {
		return nil, err
	}
	semantic, _, err := decodeSignedControlBody(message.body)
	if err != nil {
		return nil, err
	}
	return semantic, nil
}

// PrepareSenderControl validates and copies every caller-owned input but defers
// signing until SessionWriter supplies the sequence it will immediately seal.
func PrepareSenderControl(
	senderPrivateKey ed25519.PrivateKey,
	base ControlBinding,
	kind MessageKind,
	operationID *OperationID,
	canonicalSemanticBody []byte,
) (PreparedSenderControl, error) {
	if len(senderPrivateKey) != ed25519.PrivateKeySize {
		return PreparedSenderControl{}, ErrControlSigningKey
	}
	if err := validateControlBase(base); err != nil {
		return PreparedSenderControl{}, err
	}
	if _, err := senderControlDomain(kind); err != nil {
		return PreparedSenderControl{}, err
	}
	if err := validateMessageIdentity(kind, operationID); err != nil {
		return PreparedSenderControl{}, err
	}
	if _, err := encodeUnsignedControlWrapper(canonicalSemanticBody); err != nil {
		return PreparedSenderControl{}, err
	}

	key := append(ed25519.PrivateKey(nil), senderPrivateKey...)
	body := append([]byte(nil), canonicalSemanticBody...)
	var id OperationID
	hasOperationID := operationID != nil
	if hasOperationID {
		id = *operationID
	}
	build := func(sequence uint64) (Message, error) {
		binding := base
		binding.Sequence = sequence
		binding.MessageKind = kind
		if hasOperationID {
			binding.OperationID = id
			binding.HasOperationID = true
		}
		domain, err := senderControlDomain(kind)
		if err != nil {
			return Message{}, err
		}
		signed, err := SignControlBody(key, domain, binding, body)
		if err != nil {
			return Message{}, err
		}
		if !hasOperationID {
			return NewMessage(kind, nil, signed)
		}
		return NewMessage(kind, &id, signed)
	}

	// Reserve with a fixed-width placeholder. Calling build here would create a
	// real sequence-bound signature outside the writer, defeating the ownership
	// boundary even if those bytes were immediately discarded.
	placeholderBody, err := encodeSignedControlWrapper(body, make([]byte, ed25519.SignatureSize))
	if err != nil {
		return PreparedSenderControl{}, fmt.Errorf("reserve signed sender control body: %w", err)
	}
	var placeholder Message
	if hasOperationID {
		placeholder, err = NewMessage(kind, &id, placeholderBody)
	} else {
		placeholder, err = NewMessage(kind, nil, placeholderBody)
	}
	if err != nil {
		return PreparedSenderControl{}, err
	}
	plaintext, err := EncodeMessage(placeholder)
	if err != nil {
		return PreparedSenderControl{}, err
	}
	return PreparedSenderControl{
		plaintextBytes: len(plaintext), kind: kind, intent: placeholder, build: build,
	}, nil
}

func validateControlBase(base ControlBinding) error {
	if base.ShareInstance.IsZero() || base.ProtocolSessionID.IsZero() || base.LaneID == 0 ||
		base.Direction != DirectionSenderToReceiver || base.Sequence != 0 || base.MessageKind != 0 ||
		base.HasOperationID || !base.OperationID.IsZero() {
		return ErrControlBinding
	}
	return nil
}

// Verify permits binary block fragments to proceed to their object verifier;
// every other sender-to-receiver message must carry a valid sender signature.
func (a *SenderControlAuthenticator) Verify(sequence uint64, message Message) error {
	result, err := a.authenticate(sequence, message)
	if err != nil {
		return err
	}
	if result.operationViolation.valid() {
		return fmt.Errorf("%w: authenticated operation violation %d", ErrControlSemantic, result.operationViolation.code)
	}
	return nil
}

func (a *SenderControlAuthenticator) authenticate(
	sequence uint64,
	message Message,
) (InboundAuthenticationResult, error) {
	if a == nil {
		return InboundAuthenticationResult{}, ErrControlBinding
	}
	if message.IsData() {
		return InboundAuthenticationResult{}, nil
	}
	domain, err := senderControlDomain(message.kind)
	if err != nil {
		return InboundAuthenticationResult{}, err
	}
	binding := a.base
	binding.Sequence = sequence
	binding.MessageKind = message.kind
	binding.OperationID, binding.HasOperationID = message.OperationID()
	semantic, err := VerifyControlBody(a.senderPublicKey, domain, binding, message.Body())
	if err != nil {
		return InboundAuthenticationResult{}, err
	}
	operationID, _ := message.OperationID()
	if err := validateSenderControlSemantic(a.semantic, message.kind, operationID, semantic); err != nil {
		switch message.kind {
		case MessageOperationError:
			return authenticatedOperationViolationResult(
				AuthenticatedOperationViolationMalformedFailure,
			), nil
		case MessagePeerAnswer, MessagePeerCandidate:
			return authenticatedOperationViolationResult(
				AuthenticatedOperationViolationMalformedPeerControl,
			), nil
		}
		return InboundAuthenticationResult{}, err
	}
	return InboundAuthenticationResult{}, nil
}

// ControlSignaturePreimage makes every delivery identity explicit. It wraps the
// typed semantic value before hashing so array and map schemas share one signed
// delivery contract without admitting signature fields into those schemas.
func ControlSignaturePreimage(
	domain ControlDomain,
	binding ControlBinding,
	canonicalSemanticBody []byte,
) ([]byte, error) {
	if err := binding.validate(domain); err != nil {
		return nil, err
	}
	unsignedWrapper, err := encodeUnsignedControlWrapper(canonicalSemanticBody)
	if err != nil {
		return nil, err
	}
	return buildControlSignaturePreimage(domain, binding, unsignedWrapper), nil
}

// SignControlBody wraps any canonical semantic CBOR value before signing. The
// wrapper keeps signature metadata orthogonal to typed map and array schemas.
func SignControlBody(
	senderPrivateKey ed25519.PrivateKey,
	domain ControlDomain,
	binding ControlBinding,
	canonicalSemanticBody []byte,
) ([]byte, error) {
	if len(senderPrivateKey) != ed25519.PrivateKeySize {
		return nil, ErrControlSigningKey
	}
	if err := binding.validate(domain); err != nil {
		return nil, err
	}
	unsignedWrapper, err := encodeUnsignedControlWrapper(canonicalSemanticBody)
	if err != nil {
		return nil, err
	}
	preimage := buildControlSignaturePreimage(domain, binding, unsignedWrapper)
	return encodeSignedControlWrapper(canonicalSemanticBody, ed25519.Sign(senderPrivateKey, preimage))
}

// VerifyControlBody authenticates the exact unsigned wrapper and returns only
// its canonical semantic value for typed decoding.
func VerifyControlBody(
	senderPublicKey ed25519.PublicKey,
	domain ControlDomain,
	binding ControlBinding,
	canonicalSignedBody []byte,
) ([]byte, error) {
	if len(senderPublicKey) != ed25519.PublicKeySize {
		return nil, ErrControlSigningKey
	}
	semantic, signature, err := decodeSignedControlBody(canonicalSignedBody)
	if err != nil {
		return nil, err
	}
	if err := binding.validate(domain); err != nil {
		return nil, err
	}
	unsignedWrapper, err := encodeUnsignedControlWrapper(semantic)
	if err != nil {
		return nil, err
	}
	preimage := buildControlSignaturePreimage(domain, binding, unsignedWrapper)
	if !ed25519.Verify(senderPublicKey, preimage, signature) {
		return nil, ErrControlSignature
	}
	return semantic, nil
}

func (b ControlBinding) validate(domain ControlDomain) error {
	if b.ShareInstance.IsZero() || b.ProtocolSessionID.IsZero() || b.LaneID == 0 || b.Direction != DirectionSenderToReceiver {
		return ErrControlBinding
	}
	if b.HasOperationID {
		if b.OperationID.IsZero() {
			return ErrControlBinding
		}
	} else if !b.OperationID.IsZero() {
		return ErrControlBinding
	}
	switch domain {
	case ControlDomainOperation:
		if !b.HasOperationID || !isOperationControl(b.MessageKind) {
			return ErrControlBinding
		}
	case ControlDomainSessionTerminal:
		if b.HasOperationID || b.MessageKind != MessageSessionTerminal {
			return ErrControlBinding
		}
	case ControlDomainLaneAttach:
		if !b.HasOperationID || b.MessageKind != MessageLaneAttach {
			return ErrControlBinding
		}
	default:
		return ErrControlBinding
	}
	return nil
}

func isOperationControl(kind MessageKind) bool {
	switch kind {
	case MessageCatalogResult, MessageOpenResults, MessageOperationError,
		MessageScanProgress, MessageOperationComplete, MessageLeaseResult,
		MessagePeerAnswer, MessagePeerCandidate:
		return true
	default:
		return false
	}
}

func senderControlDomain(kind MessageKind) (ControlDomain, error) {
	switch {
	case kind == MessageSessionTerminal:
		return ControlDomainSessionTerminal, nil
	case kind == MessageLaneAttach:
		return ControlDomainLaneAttach, nil
	case isOperationControl(kind):
		return ControlDomainOperation, nil
	default:
		return 0, ErrControlBinding
	}
}

func buildControlSignaturePreimage(
	domain ControlDomain,
	binding ControlBinding,
	canonicalUnsignedWrapper []byte,
) []byte {
	domainValue, _ := domain.value()
	bodyDigest := sha256.Sum256(canonicalUnsignedWrapper)
	preimage := make([]byte, 0, len(domainValue)+1+catalog.IdentityBytes+IdentityBytes+4+4+1+8+1+IdentityBytes+sha256.Size)
	preimage = append(preimage, domainValue...)
	preimage = append(preimage, 0)
	preimage = append(preimage, binding.ShareInstance.Bytes()...)
	preimage = append(preimage, binding.ProtocolSessionID[:]...)
	preimage = binary.BigEndian.AppendUint32(preimage, binding.LaneID)
	preimage = binary.BigEndian.AppendUint32(preimage, binding.LaneEpoch)
	preimage = append(preimage, byte(binding.Direction))
	preimage = binary.BigEndian.AppendUint64(preimage, binding.Sequence)
	preimage = append(preimage, byte(binding.MessageKind))
	if binding.HasOperationID {
		preimage = append(preimage, binding.OperationID[:]...)
	} else {
		preimage = append(preimage, make([]byte, IdentityBytes)...)
	}
	return append(preimage, bodyDigest[:]...)
}

func (d ControlDomain) value() (string, error) {
	switch d {
	case ControlDomainOperation:
		return controlOperationDomain, nil
	case ControlDomainSessionTerminal:
		return controlTerminalDomain, nil
	case ControlDomainLaneAttach:
		return controlLaneDomain, nil
	default:
		return "", ErrControlBinding
	}
}

func encodeUnsignedControlWrapper(canonicalSemanticBody []byte) ([]byte, error) {
	if err := validateCanonicalBody(canonicalSemanticBody); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrControlBody, err)
	}
	encoded, err := messageEncMode.Marshal(map[uint64]any{
		controlWrapperVersionKey:  uint64(controlWrapperVersion),
		controlWrapperSemanticKey: cbor.RawMessage(canonicalSemanticBody),
	})
	if err != nil || len(encoded) > MaxEnvelopePlaintextBytes {
		return nil, ErrControlBody
	}
	return encoded, nil
}

func encodeSignedControlWrapper(canonicalSemanticBody, signature []byte) ([]byte, error) {
	if len(signature) != ed25519.SignatureSize {
		return nil, ErrControlSignature
	}
	if _, err := encodeUnsignedControlWrapper(canonicalSemanticBody); err != nil {
		return nil, err
	}
	encoded, err := messageEncMode.Marshal(map[uint64]any{
		controlWrapperVersionKey:  uint64(controlWrapperVersion),
		controlWrapperSemanticKey: cbor.RawMessage(canonicalSemanticBody),
		controlSignatureKey:       signature,
	})
	if err != nil || len(encoded) > MaxEnvelopePlaintextBytes {
		return nil, ErrControlBody
	}
	return encoded, nil
}

func decodeSignedControlBody(encoded []byte) ([]byte, []byte, error) {
	if len(encoded) == 0 || len(encoded) > MaxEnvelopePlaintextBytes {
		return nil, nil, ErrControlBody
	}
	if err := validateCanonicalBody(encoded); err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrControlBody, err)
	}
	var fields map[uint64]cbor.RawMessage
	if err := messageDecMode.Unmarshal(encoded, &fields); err != nil || len(fields) != 3 ||
		fields[controlWrapperVersionKey] == nil || fields[controlWrapperSemanticKey] == nil ||
		fields[controlSignatureKey] == nil {
		return nil, nil, ErrControlBody
	}
	var version uint64
	if err := messageDecMode.Unmarshal(fields[controlWrapperVersionKey], &version); err != nil ||
		version != controlWrapperVersion {
		return nil, nil, ErrControlBody
	}
	semantic := append([]byte(nil), fields[controlWrapperSemanticKey]...)
	if err := validateCanonicalBody(semantic); err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrControlBody, err)
	}
	var signature []byte
	if err := messageDecMode.Unmarshal(fields[controlSignatureKey], &signature); err != nil ||
		len(signature) != ed25519.SignatureSize {
		return nil, nil, ErrControlSignature
	}
	return semantic, append([]byte(nil), signature...), nil
}
