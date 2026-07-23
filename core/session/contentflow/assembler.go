package contentflow

import (
	"bytes"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/session/protocolsession"
)

type AssemblyStatus uint8

const (
	FragmentAccepted AssemblyStatus = iota + 1
	FragmentDuplicate
	RecordComplete
	FragmentTombstoned
)

type AssemblyResult struct {
	Status      AssemblyStatus
	OperationID protocolsession.OperationID
	RecordID    records.RecordID
	Object      []byte
}

type assemblyKey struct {
	operation protocolsession.OperationID
	record    records.RecordID
}

type assemblyState struct {
	count         uint32
	total         uint32
	buffer        []byte
	received      []bool
	receivedCount uint32
	deadline      time.Time
	reservation   *reassemblyReservation
}

type Assembler struct {
	sessionID protocolsession.ProtocolSessionID
	hierarchy ReassemblyHierarchy
	now       func() time.Time

	mu               sync.Mutex
	closed           bool
	records          map[assemblyKey]*assemblyState
	operationBytes   map[protocolsession.OperationID]uint64
	recordTombstones map[assemblyKey]time.Time
	operationTombs   map[protocolsession.OperationID]time.Time
}

func NewAssembler(sessionID protocolsession.ProtocolSessionID, hierarchy ReassemblyHierarchy, now func() time.Time) (*Assembler, error) {
	if sessionID.IsZero() || hierarchy.Process == nil || hierarchy.Share == nil || hierarchy.Session == nil {
		return nil, errors.New("content assembler requires a session identity and three budget accounts")
	}
	if hierarchy.Process == hierarchy.Share || hierarchy.Process == hierarchy.Session || hierarchy.Share == hierarchy.Session {
		return nil, errors.New("content assembler budget accounts must be distinct")
	}
	if now == nil {
		now = time.Now
	}
	return &Assembler{
		sessionID: sessionID, hierarchy: hierarchy, now: now,
		records:          make(map[assemblyKey]*assemblyState),
		operationBytes:   make(map[protocolsession.OperationID]uint64),
		recordTombstones: make(map[assemblyKey]time.Time), operationTombs: make(map[protocolsession.OperationID]time.Time),
	}, nil
}

func (a *Assembler) AcceptAuthenticated(plaintext []byte) (AssemblyResult, error) {
	fragment, err := DecodeAuthenticatedFragment(plaintext)
	if err != nil {
		return AssemblyResult{}, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return AssemblyResult{}, ErrAssemblerClosed
	}
	now := a.now()
	a.pruneLocked(now)
	for key, state := range a.records {
		if key.operation == fragment.OperationID && !now.Before(state.deadline) {
			_ = a.terminateOperationLocked(fragment.OperationID, now)
			return AssemblyResult{}, ErrFragmentTimeout
		}
	}
	key := assemblyKey{operation: fragment.OperationID, record: fragment.RecordID}
	if _, cancelled := a.operationTombs[fragment.OperationID]; cancelled {
		return AssemblyResult{Status: FragmentTombstoned, OperationID: fragment.OperationID, RecordID: fragment.RecordID}, nil
	}
	if _, complete := a.recordTombstones[key]; complete {
		return AssemblyResult{Status: FragmentTombstoned, OperationID: fragment.OperationID, RecordID: fragment.RecordID}, nil
	}
	state := a.records[key]
	if state == nil {
		// Every live assembly reserves the tombstone slot it will need when it
		// completes. This keeps late-arrival protection bounded without evicting
		// a still-live 30-second tombstone under hostile churn.
		if len(a.records)+len(a.recordTombstones)+len(a.operationTombs) >= MaxFragmentTombstones {
			return AssemblyResult{}, ErrReassemblyBudget
		}
		operationBytes := a.operationBytes[fragment.OperationID]
		if uint64(fragment.TotalLength) > MaxOperationReassemblyBytes-operationBytes {
			_ = a.terminateOperationLocked(fragment.OperationID, now)
			return AssemblyResult{}, ErrReassemblyBudget
		}
		reservation, reserveErr := reserveReassembly(a.hierarchy, uint64(fragment.TotalLength))
		if reserveErr != nil {
			return AssemblyResult{}, reserveErr
		}
		state = &assemblyState{
			count: fragment.Count, total: fragment.TotalLength,
			buffer: make([]byte, fragment.TotalLength), received: make([]bool, fragment.Count),
			deadline: now.Add(FragmentTimeout), reservation: reservation,
		}
		a.records[key] = state
		a.operationBytes[fragment.OperationID] = operationBytes + uint64(fragment.TotalLength)
	} else if state.count != fragment.Count || state.total != fragment.TotalLength {
		_ = a.terminateOperationLocked(fragment.OperationID, now)
		return AssemblyResult{}, ErrFragmentConflict
	}
	offset := uint64(fragment.Index) * MaxFragmentPayloadBytes
	end := offset + uint64(len(fragment.Payload))
	if end > uint64(len(state.buffer)) {
		_ = a.terminateOperationLocked(fragment.OperationID, now)
		return AssemblyResult{}, ErrFragmentConflict
	}
	if state.received[fragment.Index] {
		if !slices.Equal(state.buffer[offset:end], fragment.Payload) {
			_ = a.terminateOperationLocked(fragment.OperationID, now)
			return AssemblyResult{}, ErrFragmentConflict
		}
		return AssemblyResult{Status: FragmentDuplicate, OperationID: fragment.OperationID, RecordID: fragment.RecordID}, nil
	}
	copy(state.buffer[offset:end], fragment.Payload)
	state.received[fragment.Index] = true
	state.receivedCount++
	if state.receivedCount != state.count {
		return AssemblyResult{Status: FragmentAccepted, OperationID: fragment.OperationID, RecordID: fragment.RecordID}, nil
	}
	if !recordDigestMatches(fragment.RecordID, state.buffer) {
		_ = a.terminateOperationLocked(fragment.OperationID, now)
		return AssemblyResult{}, ErrRecordDigest
	}
	object := slices.Clone(state.buffer)
	a.releaseRecordLocked(key)
	a.recordTombstones[key] = now.Add(FragmentTombstone)
	return AssemblyResult{Status: RecordComplete, OperationID: fragment.OperationID, RecordID: fragment.RecordID, Object: object}, nil
}

func (a *Assembler) CancelOperation(operationID protocolsession.OperationID) error {
	if operationID.IsZero() {
		return ErrFragmentMalformed
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return ErrAssemblerClosed
	}
	now := a.now()
	a.pruneLocked(now)
	return a.terminateOperationLocked(operationID, now)
}

func (a *Assembler) CompleteOperation(operationID protocolsession.OperationID) error {
	return a.CancelOperation(operationID)
}

func (a *Assembler) SweepTimeouts() []protocolsession.OperationID {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	a.pruneLocked(now)
	if a.closed {
		return nil
	}
	timedOut := make(map[protocolsession.OperationID]struct{})
	for key, state := range a.records {
		if !now.Before(state.deadline) {
			timedOut[key.operation] = struct{}{}
		}
	}
	result := make([]protocolsession.OperationID, 0, len(timedOut))
	for operationID := range timedOut {
		_ = a.terminateOperationLocked(operationID, now)
		result = append(result, operationID)
	}
	slices.SortFunc(result, func(left, right protocolsession.OperationID) int {
		return bytes.Compare(left.Bytes(), right.Bytes())
	})
	return result
}

func (a *Assembler) ActiveRecords() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.records)
}

func (a *Assembler) releaseRecordLocked(key assemblyKey) {
	state := a.records[key]
	if state == nil {
		return
	}
	delete(a.records, key)
	operationBytes := a.operationBytes[key.operation] - uint64(state.total)
	if operationBytes == 0 {
		delete(a.operationBytes, key.operation)
	} else {
		a.operationBytes[key.operation] = operationBytes
	}
	state.reservation.Release()
}

func (a *Assembler) terminateOperationLocked(operationID protocolsession.OperationID, now time.Time) error {
	for key := range a.records {
		if key.operation == operationID {
			a.releaseRecordLocked(key)
		}
	}
	if _, exists := a.operationTombs[operationID]; !exists && len(a.recordTombstones)+len(a.operationTombs) >= MaxFragmentTombstones {
		return ErrReassemblyBudget
	}
	a.operationTombs[operationID] = now.Add(FragmentTombstone)
	return nil
}

func (a *Assembler) pruneLocked(now time.Time) {
	for key, expiry := range a.recordTombstones {
		if !now.Before(expiry) {
			delete(a.recordTombstones, key)
		}
	}
	for operation, expiry := range a.operationTombs {
		if !now.Before(expiry) {
			delete(a.operationTombs, operation)
		}
	}
}

// Close releases all three levels of reservations immediately. The assembler
// cannot receive late frames once its owning ProtocolSession is gone, so
// retaining tombstones after this boundary would only prolong memory charges.
func (a *Assembler) Close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return
	}
	a.closed = true
	for key := range a.records {
		a.releaseRecordLocked(key)
	}
	clear(a.recordTombstones)
	clear(a.operationTombs)
	clear(a.operationBytes)
}
