package protocolsession

import (
	"crypto/sha256"
	"errors"

	"github.com/windshare/windshare/core/session/peerslot"
)

const MaximumOperationContinuations = 1_024

var (
	ErrContinuationContract    = errors.New("protocolsession: operation continuation contract is invalid")
	ErrConflictingContinuation = errors.New("protocolsession: operation continuation conflicts with its contract")
	ErrContinuationPending     = errors.New("protocolsession: operation continuation admission is still pending")
)

// OperationContinuationClassifier creates the immutable continuation contract for
// one request generation. The session runtime owns replay state; an injected
// schema owner only decodes the opaque request and continuation bodies.
type OperationContinuationClassifier interface {
	BeginOperationContinuation(
		requestKind MessageKind,
		canonicalRequestBody []byte,
	) (OperationContinuationContract, bool, error)
	ClassifyUnboundOperationContinuation(
		kind MessageKind,
		canonicalBody []byte,
	) (OperationContinuationScope, bool, error)
}

type OperationContinuationScope [sha256.Size]byte

// OperationContinuationContract validates a continuation against the request
// that created it and returns its canonical semantic fingerprint.
type OperationContinuationContract interface {
	ClassifyOperationContinuation(
		kind MessageKind,
		canonicalBody []byte,
	) ([sha256.Size]byte, bool, error)
	OperationContinuationScope() OperationContinuationScope
	MaximumContinuations() int
}

// PeerAttemptBinding identifies one negotiation. PeerSlot is session-scoped;
// its sequence survives replacement of the physical PeerPathID.
type PeerAttemptBinding struct {
	PeerSlot        uint8
	PeerPathID      [16]byte
	AttemptID       [16]byte
	AttemptSequence uint64
}

func (binding PeerAttemptBinding) Valid() bool {
	return binding.PeerSlot < peerslot.Count &&
		binding.PeerPathID != [16]byte{} &&
		binding.AttemptID != [16]byte{} &&
		binding.AttemptSequence != 0
}

func decodePeerAttemptBinding(value any) (PeerAttemptBinding, error) {
	fields, ok := value.([]any)
	if !ok || len(fields) != 4 {
		return PeerAttemptBinding{}, ErrInvalidOperationFailure
	}
	path, pathOK := fields[0].([]byte)
	attempt, attemptOK := fields[1].([]byte)
	sequence, sequenceOK := fields[2].(uint64)
	slot, slotOK := fields[3].(uint64)
	if !pathOK || len(path) != 16 || !attemptOK || len(attempt) != 16 || !sequenceOK || !slotOK || slot >= peerslot.Count {
		return PeerAttemptBinding{}, ErrInvalidOperationFailure
	}
	binding := PeerAttemptBinding{
		PeerSlot:        uint8(slot),
		PeerPathID:      [16]byte(path),
		AttemptID:       [16]byte(attempt),
		AttemptSequence: sequence,
	}
	if !binding.Valid() {
		return PeerAttemptBinding{}, ErrInvalidOperationFailure
	}
	return binding, nil
}

// PeerAttemptContinuationClassifier supplies schema-validated identities only.
// Incoming messages may consult an existing retirement boundary, never create it.
type PeerAttemptContinuationClassifier interface {
	ClassifyPeerAttemptContinuation(MessageKind, []byte) (PeerAttemptBinding, bool, error)
}

// PeerAttemptAuthority is optionally implemented by an OperationContinuationContract
// whose request owns a peer attempt binding.
type PeerAttemptAuthority interface {
	PeerAttemptBinding() PeerAttemptBinding
}
