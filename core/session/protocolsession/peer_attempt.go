package protocolsession

// PeerAttemptBinding identifies one negotiation within a stable logical path.
// It contains no signaling or transport capabilities.
type PeerAttemptBinding struct {
	PeerPathID      [16]byte
	AttemptID       [16]byte
	AttemptSequence uint64
}

func (binding PeerAttemptBinding) valid() bool {
	return binding.PeerPathID != [16]byte{} && binding.AttemptID != [16]byte{} && binding.AttemptSequence != 0
}

func decodePeerAttemptBinding(value any) (PeerAttemptBinding, error) {
	fields, ok := value.([]any)
	if !ok || len(fields) != 3 {
		return PeerAttemptBinding{}, ErrInvalidOperationFailure
	}
	path, pathOK := fields[0].([]byte)
	attempt, attemptOK := fields[1].([]byte)
	sequence, sequenceOK := fields[2].(uint64)
	if !pathOK || len(path) != 16 || !attemptOK || len(attempt) != 16 || !sequenceOK {
		return PeerAttemptBinding{}, ErrInvalidOperationFailure
	}
	binding := PeerAttemptBinding{PeerPathID: [16]byte(path), AttemptID: [16]byte(attempt), AttemptSequence: sequence}
	if !binding.valid() {
		return PeerAttemptBinding{}, ErrInvalidOperationFailure
	}
	return binding, nil
}

// PeerAttemptContinuationClassifier supplies schema-validated identities only.
// Incoming messages may consult an existing retirement boundary, never create it.
type PeerAttemptContinuationClassifier interface {
	ClassifyPeerAttemptContinuation(MessageKind, []byte) (PeerAttemptBinding, bool, error)
}

type peerAttemptAuthority interface {
	PeerAttemptBinding() PeerAttemptBinding
}

const MaximumPeerContinuationPaths = 4

type retiredPeerPath struct {
	issuedThrough  uint64
	retiredThrough uint64
	active         map[uint64]struct{}
}

func operationPeerAttempt(authority *operationAuthority) (PeerAttemptBinding, bool) {
	if authority == nil || authority.continuations == nil {
		return PeerAttemptBinding{}, false
	}
	owner, ok := authority.continuations.authority.(peerAttemptAuthority)
	if !ok {
		return PeerAttemptBinding{}, false
	}
	return owner.PeerAttemptBinding(), true
}

// PeerAttemptBinding returns a copy owned by this opaque generation. A delayed
// producer cannot borrow the binding of a newer operation with the same ID.
func (generation OperationGeneration) PeerAttemptBinding() (PeerAttemptBinding, bool) {
	if generation.table == nil {
		return PeerAttemptBinding{}, false
	}
	generation.table.mu.Lock()
	defer generation.table.mu.Unlock()
	return operationPeerAttempt(generation.authority)
}

func (table *OperationTable) admitPeerAttemptLocked(authority *operationAuthority) error {
	binding, ok := operationPeerAttempt(authority)
	if !ok {
		return nil
	}
	if !binding.valid() {
		return ErrContinuationAuthority
	}
	path := table.peerPaths[binding.PeerPathID]
	if path == nil {
		if len(table.peerPaths) >= MaximumPeerContinuationPaths {
			return ErrContinuationAuthority
		}
		path = &retiredPeerPath{active: make(map[uint64]struct{})}
		table.peerPaths[binding.PeerPathID] = path
	}
	if binding.AttemptSequence <= path.issuedThrough {
		return ErrOperationIDReused
	}
	path.issuedThrough = binding.AttemptSequence
	path.active[binding.AttemptSequence] = struct{}{}
	return nil
}

func (table *OperationTable) retirePeerAttemptLocked(authority *operationAuthority) {
	binding, ok := operationPeerAttempt(authority)
	if !ok {
		return
	}
	path := table.peerPaths[binding.PeerPathID]
	if path == nil {
		return
	}
	delete(path.active, binding.AttemptSequence)
	if binding.AttemptSequence > path.retiredThrough {
		path.retiredThrough = binding.AttemptSequence
	}
}

func (table *OperationTable) peerContinuationBinding(direction Direction, message Message) (PeerAttemptBinding, bool, error) {
	if message.kind != MessagePeerOffer && message.kind != MessagePeerAnswer && message.kind != MessagePeerCandidate && message.kind != MessageOperationError {
		return PeerAttemptBinding{}, false, nil
	}
	body, err := operationContinuationSemanticBody(direction, message)
	if err != nil {
		return PeerAttemptBinding{}, true, err
	}
	if message.kind == MessageOperationError {
		failure, err := DecodeOperationFailure(body)
		if err != nil {
			return PeerAttemptBinding{}, true, err
		}
		if failure.PeerAttempt == nil {
			return PeerAttemptBinding{}, false, nil
		}
		return *failure.PeerAttempt, true, nil
	}
	classifier, ok := table.continuations.(PeerAttemptContinuationClassifier)
	if !ok {
		return PeerAttemptBinding{}, false, nil
	}
	return classifier.ClassifyPeerAttemptContinuation(message.kind, body)
}

func (table *OperationTable) validatePeerAttemptLocked(authority *operationAuthority, direction Direction, message Message) error {
	expected, ok := operationPeerAttempt(authority)
	if !ok || message.kind == MessagePeerOffer {
		return nil
	}
	actual, tracked, err := table.peerContinuationBinding(direction, message)
	if err == nil && tracked && actual != expected {
		err = ErrConflictingContinuation
	}
	if err != nil {
		authority.recordSenderOperationViolationLocked(direction, AuthenticatedOperationViolationContinuationAuthority)
	}
	return err
}

func (table *OperationTable) dropsRetiredPeerLocked(direction Direction, message Message) bool {
	binding, tracked, err := table.peerContinuationBinding(direction, message)
	if err != nil || !tracked {
		return false
	}
	path := table.peerPaths[binding.PeerPathID]
	if path == nil {
		return false
	}
	_, active := path.active[binding.AttemptSequence]
	// A request overtaken on another lane cannot resurrect an older negotiation.
	// A current sequence still requires its exact operation authority.
	if message.kind == MessagePeerOffer && binding.AttemptSequence < path.issuedThrough {
		return !active
	}
	return binding.AttemptSequence <= path.retiredThrough && !active
}
