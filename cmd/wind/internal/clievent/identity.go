package clievent

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
)

const IdentityBytes = 16
const RelaySessionIdentityBytes = 8
const ReceiveIntentDigestBytes = 32

var ErrInvalidIdentity = errors.New("CLI event identity is invalid")

type identity [IdentityBytes]byte
type relaySessionIdentity [RelaySessionIdentityBytes]byte
type receiveIntentDigest [ReceiveIntentDigestBytes]byte
type capacityDecisionDigest [sha256.Size]byte
type senderRevisionDigest [sha256.Size]byte

func newIdentity(raw []byte) (identity, error) {
	var value identity
	if len(raw) != len(value) {
		return identity{}, ErrInvalidIdentity
	}
	copy(value[:], raw)
	if value == (identity{}) {
		return identity{}, ErrInvalidIdentity
	}
	return value, nil
}

func (value identity) bytes() []byte { return append([]byte(nil), value[:]...) }
func (value identity) hex() string   { return hex.EncodeToString(value[:]) }
func (value identity) valid() bool   { return value != (identity{}) }

func newRelaySessionIdentity(raw []byte) (relaySessionIdentity, error) {
	var value relaySessionIdentity
	if len(raw) != len(value) {
		return relaySessionIdentity{}, ErrInvalidIdentity
	}
	copy(value[:], raw)
	if value == (relaySessionIdentity{}) {
		return relaySessionIdentity{}, ErrInvalidIdentity
	}
	return value, nil
}

func (value relaySessionIdentity) bytes() []byte { return append([]byte(nil), value[:]...) }
func (value relaySessionIdentity) hex() string   { return hex.EncodeToString(value[:]) }
func (value relaySessionIdentity) valid() bool   { return value != (relaySessionIdentity{}) }

func newReceiveIntentDigest(raw []byte) (receiveIntentDigest, error) {
	var value receiveIntentDigest
	if len(raw) != len(value) {
		return receiveIntentDigest{}, ErrInvalidIdentity
	}
	copy(value[:], raw)
	if value == (receiveIntentDigest{}) {
		return receiveIntentDigest{}, ErrInvalidIdentity
	}
	return value, nil
}

func (value receiveIntentDigest) hex() string { return hex.EncodeToString(value[:]) }
func (value receiveIntentDigest) valid() bool { return value != (receiveIntentDigest{}) }

func newCapacityDecisionDigest(raw string) (capacityDecisionDigest, error) {
	if raw == "" {
		return capacityDecisionDigest{}, ErrInvalidIdentity
	}
	return sha256.Sum256([]byte(raw)), nil
}

func (value capacityDecisionDigest) bytes() []byte { return append([]byte(nil), value[:]...) }
func (value capacityDecisionDigest) hex() string   { return hex.EncodeToString(value[:]) }
func (value capacityDecisionDigest) valid() bool   { return value != (capacityDecisionDigest{}) }

func newSenderRevisionDigest(raw []byte) (senderRevisionDigest, error) {
	if len(raw) == 0 {
		return senderRevisionDigest{}, ErrInvalidIdentity
	}
	return sha256.Sum256(raw), nil
}

func (value senderRevisionDigest) bytes() []byte { return append([]byte(nil), value[:]...) }
func (value senderRevisionDigest) hex() string   { return hex.EncodeToString(value[:]) }
func (value senderRevisionDigest) valid() bool   { return value != (senderRevisionDigest{}) }

// Each identity remains a distinct type so a protocol operation can never be
// serialized under the receive-operation field merely because both are 16 bytes.
type ReceiveOperationID struct{ value identity }
type ProtocolSessionID struct{ value identity }
type ProtocolOperationID struct{ value identity }
type CapacityWaitID struct{ value identity }
type CapacityGenerationID struct{ value identity }
type CapacityDecisionID struct{ value capacityDecisionDigest }
type RevisionLeaseID struct{ value identity }
type SenderRevisionID struct{ value senderRevisionDigest }
type TransferJobID struct{ value identity }
type PeerPathID struct{ value identity }
type PeerAttemptID struct{ value identity }
type RelaySessionID struct{ value relaySessionIdentity }
type OutputSessionID struct{ value identity }
type ReceiveIntentDigest struct{ value receiveIntentDigest }

func NewReceiveOperationID(raw []byte) (ReceiveOperationID, error) {
	value, err := newIdentity(raw)
	return ReceiveOperationID{value: value}, err
}

func NewProtocolSessionID(raw []byte) (ProtocolSessionID, error) {
	value, err := newIdentity(raw)
	return ProtocolSessionID{value: value}, err
}

func NewProtocolOperationID(raw []byte) (ProtocolOperationID, error) {
	value, err := newIdentity(raw)
	return ProtocolOperationID{value: value}, err
}

func NewCapacityWaitID(raw []byte) (CapacityWaitID, error) {
	value, err := newIdentity(raw)
	return CapacityWaitID{value: value}, err
}

func NewCapacityGenerationID(raw []byte) (CapacityGenerationID, error) {
	value, err := newIdentity(raw)
	return CapacityGenerationID{value: value}, err
}

func NewCapacityDecisionID(raw string) (CapacityDecisionID, error) {
	value, err := newCapacityDecisionDigest(raw)
	return CapacityDecisionID{value: value}, err
}

func NewRevisionLeaseID(raw []byte) (RevisionLeaseID, error) {
	value, err := newIdentity(raw)
	return RevisionLeaseID{value: value}, err
}

func NewSenderRevisionID(raw []byte) (SenderRevisionID, error) {
	value, err := newSenderRevisionDigest(raw)
	return SenderRevisionID{value: value}, err
}

func NewTransferJobID(raw []byte) (TransferJobID, error) {
	value, err := newIdentity(raw)
	return TransferJobID{value: value}, err
}

func NewPeerPathID(raw []byte) (PeerPathID, error) {
	value, err := newIdentity(raw)
	return PeerPathID{value: value}, err
}

func NewPeerAttemptID(raw []byte) (PeerAttemptID, error) {
	value, err := newIdentity(raw)
	return PeerAttemptID{value: value}, err
}

func NewRelaySessionID(raw []byte) (RelaySessionID, error) {
	value, err := newRelaySessionIdentity(raw)
	return RelaySessionID{value: value}, err
}

func NewOutputSessionID(raw []byte) (OutputSessionID, error) {
	value, err := newIdentity(raw)
	return OutputSessionID{value: value}, err
}

func NewReceiveIntentDigest(raw []byte) (ReceiveIntentDigest, error) {
	value, err := newReceiveIntentDigest(raw)
	return ReceiveIntentDigest{value: value}, err
}

func (id ReceiveOperationID) Bytes() []byte   { return id.value.bytes() }
func (id ProtocolSessionID) Bytes() []byte    { return id.value.bytes() }
func (id ProtocolOperationID) Bytes() []byte  { return id.value.bytes() }
func (id CapacityWaitID) Bytes() []byte       { return id.value.bytes() }
func (id CapacityGenerationID) Bytes() []byte { return id.value.bytes() }
func (id CapacityDecisionID) Bytes() []byte   { return id.value.bytes() }
func (id RevisionLeaseID) Bytes() []byte      { return id.value.bytes() }
func (id SenderRevisionID) Bytes() []byte     { return id.value.bytes() }
func (id TransferJobID) Bytes() []byte        { return id.value.bytes() }
func (id PeerPathID) Bytes() []byte           { return id.value.bytes() }
func (id PeerAttemptID) Bytes() []byte        { return id.value.bytes() }
func (id RelaySessionID) Bytes() []byte       { return id.value.bytes() }
func (id OutputSessionID) Bytes() []byte      { return id.value.bytes() }

func (id ReceiveOperationID) Hex() string   { return id.value.hex() }
func (id ProtocolSessionID) Hex() string    { return id.value.hex() }
func (id ProtocolOperationID) Hex() string  { return id.value.hex() }
func (id CapacityWaitID) Hex() string       { return id.value.hex() }
func (id CapacityGenerationID) Hex() string { return id.value.hex() }
func (id CapacityDecisionID) Hex() string   { return id.value.hex() }
func (id RevisionLeaseID) Hex() string      { return id.value.hex() }
func (id SenderRevisionID) Hex() string     { return id.value.hex() }
func (id TransferJobID) Hex() string        { return id.value.hex() }
func (id PeerPathID) Hex() string           { return id.value.hex() }
func (id PeerAttemptID) Hex() string        { return id.value.hex() }
func (id RelaySessionID) Hex() string       { return id.value.hex() }
func (id OutputSessionID) Hex() string      { return id.value.hex() }
func (id ReceiveIntentDigest) Hex() string  { return id.value.hex() }

func (id ReceiveOperationID) Valid() bool   { return id.value.valid() }
func (id ProtocolSessionID) Valid() bool    { return id.value.valid() }
func (id ProtocolOperationID) Valid() bool  { return id.value.valid() }
func (id CapacityWaitID) Valid() bool       { return id.value.valid() }
func (id CapacityGenerationID) Valid() bool { return id.value.valid() }
func (id CapacityDecisionID) Valid() bool   { return id.value.valid() }
func (id RevisionLeaseID) Valid() bool      { return id.value.valid() }
func (id SenderRevisionID) Valid() bool     { return id.value.valid() }
func (id TransferJobID) Valid() bool        { return id.value.valid() }
func (id PeerPathID) Valid() bool           { return id.value.valid() }
func (id PeerAttemptID) Valid() bool        { return id.value.valid() }
func (id RelaySessionID) Valid() bool       { return id.value.valid() }
func (id OutputSessionID) Valid() bool      { return id.value.valid() }
func (id ReceiveIntentDigest) Valid() bool  { return id.value.valid() }

type LaneIdentity struct {
	id    uint32
	epoch uint32
}

func NewLaneIdentity(id, epoch uint32) (LaneIdentity, error) {
	if id == 0 {
		return LaneIdentity{}, ErrInvalidIdentity
	}
	// Epoch zero is the authenticated transcript lane. Attached lanes use a
	// positive epoch, so rejecting zero here would erase useful initial-lane facts.
	return LaneIdentity{id: id, epoch: epoch}, nil
}

func (identity LaneIdentity) ID() uint32    { return identity.id }
func (identity LaneIdentity) Epoch() uint32 { return identity.epoch }
func (identity LaneIdentity) Valid() bool   { return identity.id != 0 }
