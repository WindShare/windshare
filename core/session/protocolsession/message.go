package protocolsession

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/fxamacker/cbor/v2"
	"golang.org/x/text/unicode/norm"
)

// MessageKind identifies the operation carried inside an authenticated envelope.
// Values are wire contract values and therefore intentionally explicit.
type MessageKind uint8

const (
	MessageListChildren      MessageKind = 1
	MessageCatalogResult     MessageKind = 2
	MessageOpenRevisions     MessageKind = 3
	MessageOpenResults       MessageKind = 4
	MessageRenewLease        MessageKind = 5
	MessageReleaseLease      MessageKind = 6
	MessageRequestBlocks     MessageKind = 7
	MessageBlockFragment     MessageKind = 8
	MessageCancel            MessageKind = 9
	MessageOperationError    MessageKind = 10
	MessageSessionTerminal   MessageKind = 11
	MessageLaneAttach        MessageKind = 12
	MessageScanProgress      MessageKind = 13
	MessageOperationComplete MessageKind = 14
	MessageLeaseResult       MessageKind = 15
	MessagePeerOffer         MessageKind = 16
	MessagePeerAnswer        MessageKind = 17
	MessagePeerCandidate     MessageKind = 18
)

const fragmentRoutingHeaderBytes = 20

var (
	ErrInvalidMessage      = errors.New("protocolsession: invalid message")
	ErrNonCanonicalMessage = errors.New("protocolsession: message is not canonical CBOR")
	ErrUnknownMessageKind  = errors.New("protocolsession: unknown message kind")
	ErrInvalidOperationID  = errors.New("protocolsession: invalid operation identity")
	ErrMessageTooLarge     = errors.New("protocolsession: message exceeds the envelope plaintext limit")
	ErrFragmentHeader      = errors.New("protocolsession: invalid fragment routing header")
)

var messageEncMode = func() cbor.EncMode {
	mode, err := cbor.CoreDetEncOptions().EncMode()
	if err != nil {
		panic(err)
	}
	return mode
}()

var messageDecMode = func() cbor.DecMode {
	mode, err := cbor.DecOptions{
		DupMapKey:        cbor.DupMapKeyEnforcedAPF,
		IndefLength:      cbor.IndefLengthForbidden,
		TagsMd:           cbor.TagsForbidden,
		MaxNestedLevels:  16,
		MaxArrayElements: 2048,
		MaxMapPairs:      256,
	}.DecMode()
	if err != nil {
		panic(err)
	}
	return mode
}()

// Message is an immutable authenticated-plaintext message. Body remains opaque
// because catalog, revision, and block schemas belong to their business modules.
// For MessageBlockFragment, Body returns the complete binary fragment plaintext.
type Message struct {
	kind        MessageKind
	operationID OperationID
	hasID       bool
	body        []byte
	plaintext   []byte
}

func (m Message) Kind() MessageKind { return m.kind }

func (m Message) OperationID() (OperationID, bool) {
	return m.operationID, m.hasID
}

func (m Message) Body() []byte { return bytes.Clone(m.body) }

func (m Message) IsData() bool { return m.kind == MessageBlockFragment }

// EncodeBody produces the deterministic CBOR representation expected by
// NewMessage. Keeping this helper here lets services share one canonical mode
// without moving their typed schema into the session runtime.
func EncodeBody(value any) ([]byte, error) {
	encoded, err := messageEncMode.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode message body: %w", err)
	}
	if err := validateCanonicalBody(encoded); err != nil {
		return nil, err
	}
	return encoded, nil
}

// NewMessage validates an ordinary CBOR message and owns copies of all input.
// Binary block fragments use DecodeMessage because their operation identity is
// part of the fixed fragment header rather than the CBOR wrapper.
func NewMessage(kind MessageKind, operationID *OperationID, body []byte) (Message, error) {
	if !kind.valid() {
		return Message{}, fmt.Errorf("%w: %d", ErrUnknownMessageKind, kind)
	}
	if kind == MessageBlockFragment {
		return Message{}, fmt.Errorf("%w: block fragments use their binary plaintext", ErrInvalidMessage)
	}
	if err := validateMessageIdentity(kind, operationID); err != nil {
		return Message{}, err
	}
	if err := validateCanonicalBody(body); err != nil {
		return Message{}, err
	}

	fields := map[uint64]any{
		0: uint64(kind),
		1: nil,
		2: cbor.RawMessage(body),
	}
	var id OperationID
	var hasID bool
	if operationID != nil {
		id = *operationID
		hasID = true
		fields[1] = id.Bytes()
	}
	plaintext, err := messageEncMode.Marshal(fields)
	if err != nil {
		return Message{}, fmt.Errorf("encode message: %w", err)
	}
	if len(plaintext) > MaxEnvelopePlaintextBytes {
		return Message{}, fmt.Errorf("%w: %d bytes", ErrMessageTooLarge, len(plaintext))
	}
	return Message{
		kind:        kind,
		operationID: id,
		hasID:       hasID,
		body:        bytes.Clone(body),
		plaintext:   plaintext,
	}, nil
}

// DecodeMessage rejects merely decodable CBOR. Re-encoding the typed outer map
// and the opaque body prevents alternate integers, map order, and indefinite
// forms from creating multiple authenticated byte representations.
func DecodeMessage(plaintext []byte) (Message, error) {
	if len(plaintext) == 0 {
		return Message{}, fmt.Errorf("%w: empty plaintext", ErrInvalidMessage)
	}
	if len(plaintext) > MaxEnvelopePlaintextBytes {
		return Message{}, fmt.Errorf("%w: %d bytes", ErrMessageTooLarge, len(plaintext))
	}
	if len(plaintext) >= 2 && plaintext[0] == 1 && plaintext[1] == byte(MessageBlockFragment) {
		return decodeFragmentMessage(plaintext)
	}

	var fields map[uint64]cbor.RawMessage
	if err := messageDecMode.Unmarshal(plaintext, &fields); err != nil {
		return Message{}, fmt.Errorf("%w: %w", ErrInvalidMessage, err)
	}
	if len(fields) != 3 || fields[0] == nil || fields[1] == nil || fields[2] == nil {
		return Message{}, fmt.Errorf("%w: expected exactly keys 0, 1, and 2", ErrInvalidMessage)
	}

	var kindValue uint64
	if err := messageDecMode.Unmarshal(fields[0], &kindValue); err != nil || kindValue > 255 {
		return Message{}, fmt.Errorf("%w: message kind", ErrInvalidMessage)
	}
	kind := MessageKind(kindValue)
	operationID, err := decodeOperationID(fields[1])
	if err != nil {
		return Message{}, err
	}
	message, err := NewMessage(kind, operationID, fields[2])
	if err != nil {
		return Message{}, err
	}
	if !bytes.Equal(message.plaintext, plaintext) {
		return Message{}, ErrNonCanonicalMessage
	}
	return message, nil
}

// EncodeMessage returns a copy so callers cannot mutate a queued message after
// it has passed lifecycle validation.
func EncodeMessage(message Message) ([]byte, error) {
	if len(message.plaintext) == 0 || !message.kind.valid() {
		return nil, ErrInvalidMessage
	}
	return bytes.Clone(message.plaintext), nil
}

func decodeFragmentMessage(plaintext []byte) (Message, error) {
	// Full fragment geometry and reassembly are deliberately outside this module;
	// these fields are the minimum authenticated prefix required for safe routing.
	if len(plaintext) < fragmentRoutingHeaderBytes || plaintext[2]&^byte(1) != 0 || plaintext[3] != 0 {
		return Message{}, ErrFragmentHeader
	}
	operationID, err := OperationIDFromBytes(plaintext[4:fragmentRoutingHeaderBytes])
	if err != nil || operationID.IsZero() {
		return Message{}, ErrInvalidOperationID
	}
	owned := bytes.Clone(plaintext)
	return Message{
		kind:        MessageBlockFragment,
		operationID: operationID,
		hasID:       true,
		body:        owned,
		plaintext:   owned,
	}, nil
}

func decodeOperationID(encoded cbor.RawMessage) (*OperationID, error) {
	if bytes.Equal(encoded, []byte{0xf6}) {
		return nil, nil
	}
	var raw []byte
	if err := messageDecMode.Unmarshal(encoded, &raw); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidOperationID, err)
	}
	id, err := OperationIDFromBytes(raw)
	if err != nil || id.IsZero() {
		return nil, ErrInvalidOperationID
	}
	return &id, nil
}

func validateMessageIdentity(kind MessageKind, operationID *OperationID) error {
	if kind == MessageSessionTerminal {
		if operationID != nil {
			return fmt.Errorf("%w: session terminal must not have an operation identity", ErrInvalidOperationID)
		}
		return nil
	}
	if operationID == nil || operationID.IsZero() {
		return fmt.Errorf("%w: operation message requires a nonzero identity", ErrInvalidOperationID)
	}
	return nil
}

func validateCanonicalBody(body []byte) error {
	if len(body) == 0 {
		return fmt.Errorf("%w: empty body", ErrInvalidMessage)
	}
	var value any
	if err := messageDecMode.Unmarshal(body, &value); err != nil {
		return fmt.Errorf("%w: body: %w", ErrInvalidMessage, err)
	}
	if err := validateCBORValue(value); err != nil {
		return err
	}
	canonical, err := messageEncMode.Marshal(value)
	if err != nil {
		return fmt.Errorf("%w: body: %w", ErrInvalidMessage, err)
	}
	if !bytes.Equal(canonical, body) {
		return fmt.Errorf("%w: body", ErrNonCanonicalMessage)
	}
	return nil
}

func validateCBORValue(value any) error {
	switch current := value.(type) {
	case nil, bool, uint64, []byte:
		return nil
	case string:
		if !norm.NFC.IsNormalString(current) {
			return fmt.Errorf("%w: text is not NFC", ErrInvalidMessage)
		}
		return nil
	case []any:
		for _, item := range current {
			if err := validateCBORValue(item); err != nil {
				return err
			}
		}
		return nil
	case map[any]any:
		for key, item := range current {
			if _, ok := key.(uint64); !ok {
				return fmt.Errorf("%w: schema map key is not a nonnegative integer", ErrInvalidMessage)
			}
			if err := validateCBORValue(item); err != nil {
				return err
			}
		}
		return nil
	default:
		// This rejects negative integers, floats, undefined/simple values, and
		// any future decoder representation not frozen by the wire contract.
		return fmt.Errorf("%w: forbidden CBOR value %T", ErrInvalidMessage, value)
	}
}

func (m Message) operationFingerprint(direction Direction) [sha256.Size]byte {
	body := m.operationFingerprintBody(direction)
	preimage := make([]byte, 0, 2+IdentityBytes+len(body))
	preimage = append(preimage, byte(direction), byte(m.kind))
	if m.hasID {
		preimage = append(preimage, m.operationID[:]...)
	} else {
		preimage = append(preimage, make([]byte, IdentityBytes)...)
	}
	preimage = append(preimage, body...)
	return sha256.Sum256(preimage)
}

func (m Message) operationFingerprintBody(direction Direction) []byte {
	// Sender-control signatures bind lane, epoch, and sequence. Those delivery
	// axes necessarily change when the same semantic final is retried on another
	// lane, so operation idempotency must compare the authenticated unsigned body
	// rather than the per-delivery signature bytes.
	if direction != DirectionSenderToReceiver {
		return m.body
	}
	if _, err := senderControlDomain(m.kind); err != nil {
		return m.body
	}
	semantic, _, err := decodeSignedControlBody(m.body)
	if err != nil {
		return m.body
	}
	unsigned, err := encodeUnsignedControlWrapper(semantic)
	if err != nil {
		return m.body
	}
	return unsigned
}

func (kind MessageKind) valid() bool {
	return kind >= MessageListChildren && kind <= MessagePeerCandidate
}

func (kind MessageKind) isRequest() bool {
	switch kind {
	case MessageListChildren, MessageOpenRevisions, MessageRenewLease,
		MessageReleaseLease, MessageRequestBlocks, MessagePeerOffer:
		return true
	default:
		return false
	}
}

func (kind MessageKind) isFinal() bool {
	switch kind {
	case MessageCatalogResult, MessageOpenResults, MessageCancel,
		MessageOperationError, MessageOperationComplete, MessageLeaseResult:
		return true
	default:
		return false
	}
}
