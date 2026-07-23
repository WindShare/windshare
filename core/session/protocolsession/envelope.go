package protocolsession

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"sync/atomic"

	"github.com/windshare/windshare/core/catalog"
)

const (
	EnvelopeNonceBytes        = 12
	EnvelopeHeaderBytes       = 28
	EnvelopeTagBytes          = 16
	EnvelopeOverheadBytes     = EnvelopeHeaderBytes + EnvelopeTagBytes
	MaxEnvelopeBytes          = 65_536
	MaxEnvelopePlaintextBytes = MaxEnvelopeBytes - EnvelopeOverheadBytes
)

const operationEnvelopeDomain = "windshare/v2 operation-envelope\x00"

var (
	ErrEnvelopeBinding        = errors.New("operation envelope binding is invalid")
	ErrEnvelopeMalformed      = errors.New("operation envelope is malformed")
	ErrEnvelopeVersion        = errors.New("operation envelope has an unsupported wire version")
	ErrEnvelopeDirection      = errors.New("operation envelope has the wrong direction")
	ErrEnvelopeSequence       = errors.New("operation envelope sequence is not the next expected value")
	ErrEnvelopeAuthentication = errors.New("operation envelope authentication failed")
	ErrEnvelopeTooLarge       = errors.New("operation envelope exceeds the frame limit")
	ErrSequenceExhausted      = errors.New("operation envelope sequence is exhausted")
	ErrNonceSource            = errors.New("operation envelope nonce source failed")
	ErrEnvelopeClosed         = errors.New("operation envelope cryptographic state is closed")
)

type Direction uint8

const (
	DirectionReceiverToSender Direction = iota
	DirectionSenderToReceiver
)

func (d Direction) valid() bool {
	return d == DirectionReceiverToSender || d == DirectionSenderToReceiver
}

type EnvelopeBinding struct {
	ShareInstance     catalog.ShareInstance
	ProtocolSessionID ProtocolSessionID
	LaneID            uint32
	LaneEpoch         uint32
	Direction         Direction
}

func (b EnvelopeBinding) validate() error {
	if b.ShareInstance.IsZero() || b.ProtocolSessionID.IsZero() || b.LaneID == 0 || !b.Direction.valid() {
		return ErrEnvelopeBinding
	}
	return nil
}

func EnvelopeFrameSize(plaintextBytes int) (int, error) {
	if plaintextBytes < 0 || plaintextBytes > MaxEnvelopePlaintextBytes {
		return 0, fmt.Errorf("%w: plaintext has %d bytes", ErrEnvelopeTooLarge, plaintextBytes)
	}
	return plaintextBytes + EnvelopeOverheadBytes, nil
}

type SealedEnvelope struct {
	Sequence uint64
	Frame    []byte
}

type OpenedEnvelope struct {
	Sequence  uint64
	Plaintext []byte
}

// EnvelopeSealer is intentionally stateful: one lane writer owns one instance,
// making sequence assignment and nonce generation impossible to bypass.
type EnvelopeSealer struct {
	aead        cipher.AEAD
	binding     EnvelopeBinding
	nonceSource io.Reader
	nonce       [EnvelopeNonceBytes]byte
	nonceReady  bool
	next        uint64
	exhausted   bool
	stopped     atomic.Bool
}

func NewEnvelopeSealer(key TrafficKey, binding EnvelopeBinding, nonceSource io.Reader) (*EnvelopeSealer, error) {
	// The constructor owns its by-value key copy. Clearing it after AES expansion
	// prevents a second raw key from surviving on this stack frame.
	defer key.Destroy()
	if err := binding.validate(); err != nil {
		return nil, err
	}
	if !key.valid || key.direction != binding.Direction {
		return nil, ErrTrafficKeyDirection
	}
	if nonceSource == nil {
		return nil, ErrNonceSource
	}
	aead, err := newEnvelopeAEAD(&key)
	if err != nil {
		return nil, err
	}
	return &EnvelopeSealer{aead: aead, binding: binding, nonceSource: nonceSource}, nil
}

func (s *EnvelopeSealer) NextSequence() (uint64, error) {
	if s == nil || s.stopped.Load() {
		return 0, ErrEnvelopeClosed
	}
	if s.exhausted {
		return 0, ErrSequenceExhausted
	}
	// Nonce acquisition is the only fallible step before Seal for an already
	// size-checked plaintext. Reserving it here lets SessionWriter perform every
	// fallible preflight before atomically admitting operation state.
	if !s.nonceReady {
		if _, err := io.ReadFull(s.nonceSource, s.nonce[:]); err != nil {
			clear(s.nonce[:])
			return 0, fmt.Errorf("%w: %w", ErrNonceSource, err)
		}
		if s.stopped.Load() {
			clear(s.nonce[:])
			return 0, ErrEnvelopeClosed
		}
		s.nonceReady = true
	}
	return s.next, nil
}

func (s *EnvelopeSealer) Seal(plaintext []byte) (SealedEnvelope, error) {
	frameSize, err := EnvelopeFrameSize(len(plaintext))
	if err != nil {
		return SealedEnvelope{}, err
	}
	sequence, err := s.NextSequence()
	if err != nil {
		return SealedEnvelope{}, err
	}
	ciphertextLength := uint32(len(plaintext) + s.aead.Overhead())
	frame := make([]byte, EnvelopeHeaderBytes, frameSize)
	frame[0] = WireVersion
	frame[1] = byte(s.binding.Direction)
	binary.BigEndian.PutUint64(frame[4:12], sequence)
	binary.BigEndian.PutUint32(frame[12:16], ciphertextLength)
	copy(frame[16:EnvelopeHeaderBytes], s.nonce[:])
	aad := envelopeAAD(s.binding, sequence, ciphertextLength)
	frame = s.aead.Seal(frame, s.nonce[:], plaintext, aad)
	s.advance(sequence)
	return SealedEnvelope{Sequence: sequence, Frame: frame}, nil
}

func (s *EnvelopeSealer) advance(sequence uint64) {
	clear(s.nonce[:])
	s.nonceReady = false
	if sequence == math.MaxUint64 {
		s.exhausted = true
		return
	}
	s.next = sequence + 1
}

// EnvelopeOpener advances only after AEAD authentication. A forged frame can
// therefore fail closed without consuming the legitimate sender's sequence.
type EnvelopeOpener struct {
	aead      cipher.AEAD
	binding   EnvelopeBinding
	next      uint64
	exhausted bool
	stopped   atomic.Bool
}

func NewEnvelopeOpener(key TrafficKey, binding EnvelopeBinding) (*EnvelopeOpener, error) {
	// The caller retains its TrafficKey value; this constructor clears only the
	// by-value copy used to expand the lane cipher.
	defer key.Destroy()
	if err := binding.validate(); err != nil {
		return nil, err
	}
	if !key.valid || key.direction != binding.Direction {
		return nil, ErrTrafficKeyDirection
	}
	aead, err := newEnvelopeAEAD(&key)
	if err != nil {
		return nil, err
	}
	return &EnvelopeOpener{aead: aead, binding: binding}, nil
}

func (o *EnvelopeOpener) NextSequence() (uint64, error) {
	if o == nil || o.stopped.Load() {
		return 0, ErrEnvelopeClosed
	}
	if o.exhausted {
		return 0, ErrSequenceExhausted
	}
	return o.next, nil
}

func (o *EnvelopeOpener) Open(frame []byte) (OpenedEnvelope, error) {
	expected, err := o.NextSequence()
	if err != nil {
		return OpenedEnvelope{}, err
	}
	header, err := parseEnvelopeHeader(frame, o.binding.Direction)
	if err != nil {
		return OpenedEnvelope{}, err
	}
	if header.sequence != expected {
		return OpenedEnvelope{}, fmt.Errorf("%w: got %d, expected %d", ErrEnvelopeSequence, header.sequence, expected)
	}
	aad := envelopeAAD(o.binding, header.sequence, header.ciphertextLength)
	plaintext, err := o.aead.Open(nil, header.nonce, frame[EnvelopeHeaderBytes:], aad)
	if err != nil {
		return OpenedEnvelope{}, ErrEnvelopeAuthentication
	}
	o.advance(header.sequence)
	return OpenedEnvelope{Sequence: header.sequence, Plaintext: plaintext}, nil
}

func (o *EnvelopeOpener) advance(sequence uint64) {
	if sequence == math.MaxUint64 {
		o.exhausted = true
		return
	}
	o.next = sequence + 1
}

type envelopeHeader struct {
	sequence         uint64
	ciphertextLength uint32
	nonce            []byte
}

func parseEnvelopeHeader(frame []byte, direction Direction) (envelopeHeader, error) {
	if len(frame) > MaxEnvelopeBytes {
		return envelopeHeader{}, ErrEnvelopeTooLarge
	}
	if len(frame) < EnvelopeOverheadBytes {
		return envelopeHeader{}, fmt.Errorf("%w: got %d bytes", ErrEnvelopeMalformed, len(frame))
	}
	if frame[0] != WireVersion {
		return envelopeHeader{}, ErrEnvelopeVersion
	}
	if frame[1] > byte(DirectionSenderToReceiver) || frame[2] != 0 || frame[3] != 0 {
		return envelopeHeader{}, ErrEnvelopeMalformed
	}
	if Direction(frame[1]) != direction {
		return envelopeHeader{}, ErrEnvelopeDirection
	}
	ciphertextLength := binary.BigEndian.Uint32(frame[12:16])
	if ciphertextLength < EnvelopeTagBytes || uint64(ciphertextLength) > uint64(MaxEnvelopeBytes-EnvelopeHeaderBytes) {
		return envelopeHeader{}, ErrEnvelopeMalformed
	}
	if uint64(len(frame)) != uint64(EnvelopeHeaderBytes)+uint64(ciphertextLength) {
		return envelopeHeader{}, ErrEnvelopeMalformed
	}
	return envelopeHeader{
		sequence: binary.BigEndian.Uint64(frame[4:12]), ciphertextLength: ciphertextLength,
		nonce: frame[16:EnvelopeHeaderBytes],
	}, nil
}

func envelopeAAD(binding EnvelopeBinding, sequence uint64, ciphertextLength uint32) []byte {
	aad := make([]byte, 0, len(operationEnvelopeDomain)+2+catalog.IdentityBytes+IdentityBytes+4+4+8+4)
	aad = append(aad, operationEnvelopeDomain...)
	aad = append(aad, WireVersion, byte(binding.Direction))
	aad = append(aad, binding.ShareInstance.Bytes()...)
	aad = append(aad, binding.ProtocolSessionID[:]...)
	aad = binary.BigEndian.AppendUint32(aad, binding.LaneID)
	aad = binary.BigEndian.AppendUint32(aad, binding.LaneEpoch)
	aad = binary.BigEndian.AppendUint64(aad, sequence)
	return binary.BigEndian.AppendUint32(aad, ciphertextLength)
}

func newEnvelopeAEAD(key *TrafficKey) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key.value[:])
	if err != nil {
		return nil, fmt.Errorf("create operation envelope cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create operation envelope GCM: %w", err)
	}
	return aead, nil
}
