package contentflow

import (
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/session/protocolsession"
)

type AssemblyStatus uint8

const (
	FragmentAccepted AssemblyStatus = iota + 1
	FragmentDuplicate
	RecordComplete
)

type AssemblyResult struct {
	Status        AssemblyStatus
	OperationID   protocolsession.OperationID
	RecordID      records.RecordID
	Object        []byte
	ReceivedBytes uint64
}

type assemblyKey struct {
	operation protocolsession.OperationID
	record    records.RecordID
}

type assemblyState struct {
	count            uint32
	total            uint32
	buffer           []byte
	received         []bool
	receivedCount    uint32
	progressDeadline time.Time
	reservation      *reassemblyReservation
}

type completedRecord struct {
	count uint32
	total uint32
}

// Assembler owns active block-fragment reassembly and its three-tier memory
// budget. Cross-operation late-arrival suppression and retained-identity
// backpressure belong solely to protocolsession.OperationTable, so Assembler
// keeps no post-retirement tombstones. Callers must retire the operation at the
// protocol layer before calling RetireOperation (or Close) to drop in-operation
// state and avoid retaining completedRecords entries.
type Assembler struct {
	sessionID protocolsession.ProtocolSessionID
	hierarchy ReassemblyHierarchy
	now       func() time.Time

	mu               sync.Mutex
	closed           bool
	records          map[assemblyKey]*assemblyState
	operationBytes   map[protocolsession.OperationID]uint64
	completedRecords map[protocolsession.OperationID]map[records.RecordID]completedRecord
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
		completedRecords: make(map[protocolsession.OperationID]map[records.RecordID]completedRecord),
	}, nil
}

func (a *Assembler) AcceptAuthenticated(plaintext []byte) (AssemblyResult, error) {
	return a.acceptAuthenticated(plaintext, nil)
}

func (a *Assembler) acceptAuthenticated(plaintext []byte, capacity *AssemblyCapacity) (AssemblyResult, error) {
	fragment, err := DecodeAuthenticatedFragment(plaintext)
	if err != nil {
		return AssemblyResult{}, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return AssemblyResult{}, ErrAssemblerClosed
	}
	if capacity != nil {
		if capacity.released {
			return AssemblyResult{}, ErrAssemblerClosed
		}
		if uint64(fragment.TotalLength) > capacity.reservation.bytes {
			return AssemblyResult{}, ErrReassemblyBudget
		}
		if capacity.operation.IsZero() {
			capacity.operation, capacity.record = fragment.OperationID, fragment.RecordID
		} else if capacity.operation != fragment.OperationID || capacity.record != fragment.RecordID {
			return AssemblyResult{}, ErrFragmentConflict
		}
	}
	now := a.now()
	if a.operationBytes[fragment.OperationID] > 0 {
		for key, state := range a.records {
			if key.operation == fragment.OperationID && !now.Before(state.progressDeadline) {
				a.retireOperationLocked(fragment.OperationID)
				return AssemblyResult{}, ErrFragmentInactivity
			}
		}
	}
	if completed, ok := a.completedRecords[fragment.OperationID][fragment.RecordID]; ok {
		if completed.count != fragment.Count || completed.total != fragment.TotalLength {
			a.retireOperationLocked(fragment.OperationID)
			return AssemblyResult{}, ErrFragmentConflict
		}
		// The completed record already passed SHA-256 RecordID verification and
		// left the assembly buffer; replays matching its geometry stay idempotent
		// without retaining the sealed object for byte-level comparison.
		return AssemblyResult{Status: FragmentDuplicate, OperationID: fragment.OperationID, RecordID: fragment.RecordID}, nil
	}
	key := assemblyKey{operation: fragment.OperationID, record: fragment.RecordID}
	state, err := a.assemblyLocked(key, fragment, capacity, now)
	if err != nil {
		return AssemblyResult{}, err
	}
	offset := uint64(fragment.Index) * MaxFragmentPayloadBytes
	end := offset + uint64(len(fragment.Payload))
	if end > uint64(len(state.buffer)) {
		a.retireOperationLocked(fragment.OperationID)
		return AssemblyResult{}, ErrFragmentConflict
	}
	if state.received[fragment.Index] {
		if !slices.Equal(state.buffer[offset:end], fragment.Payload) {
			a.retireOperationLocked(fragment.OperationID)
			return AssemblyResult{}, ErrFragmentConflict
		}
		return AssemblyResult{Status: FragmentDuplicate, OperationID: fragment.OperationID, RecordID: fragment.RecordID}, nil
	}
	copy(state.buffer[offset:end], fragment.Payload)
	state.received[fragment.Index] = true
	state.receivedCount++
	// Only new authenticated bytes extend the operation lifetime. Retransmitted
	// duplicates remain harmless, but cannot pin a reserved reassembly buffer.
	state.progressDeadline = now.Add(FragmentInactivityTimeout)
	if state.receivedCount != state.count {
		return AssemblyResult{Status: FragmentAccepted, OperationID: fragment.OperationID, RecordID: fragment.RecordID, ReceivedBytes: uint64(len(fragment.Payload))}, nil
	}
	if !recordDigestMatches(fragment.RecordID, state.buffer) {
		a.retireOperationLocked(fragment.OperationID)
		return AssemblyResult{}, ErrRecordDigest
	}
	// The completed buffer leaves the assembly table. A pre-dispatch reservation
	// remains with its physical attempt through authentication and cancellation.
	object := state.buffer
	completed := completedRecord{count: state.count, total: state.total}
	a.releaseRecordLocked(key)
	a.markRecordCompletedLocked(fragment.OperationID, fragment.RecordID, completed)
	return AssemblyResult{Status: RecordComplete, OperationID: fragment.OperationID, RecordID: fragment.RecordID, Object: object, ReceivedBytes: uint64(len(fragment.Payload))}, nil
}

func (a *Assembler) assemblyLocked(key assemblyKey, fragment Fragment, capacity *AssemblyCapacity, now time.Time) (*assemblyState, error) {
	state := a.records[key]
	if state != nil {
		if state.count != fragment.Count || state.total != fragment.TotalLength {
			a.retireOperationLocked(fragment.OperationID)
			return nil, ErrFragmentConflict
		}
		return state, nil
	}
	// A single REQUEST_BLOCKS operation can legally request at most
	// MaxRequestedBlockIndices records; bounding in-operation state here
	// prevents an unretired operation from growing completedRecords unboundedly.
	if len(a.completedRecords[fragment.OperationID])+a.activeOperationRecordsLocked(fragment.OperationID) >= content.MaxRequestedBlockIndices {
		a.retireOperationLocked(fragment.OperationID)
		return nil, ErrFragmentConflict
	}
	operationBytes := a.operationBytes[fragment.OperationID]
	if uint64(fragment.TotalLength) > MaxOperationReassemblyBytes-operationBytes {
		a.retireOperationLocked(fragment.OperationID)
		return nil, ErrReassemblyBudget
	}
	var reservation *reassemblyReservation
	if capacity == nil {
		var reserveErr error
		reservation, reserveErr = reserveReassembly(a.hierarchy, uint64(fragment.TotalLength))
		if reserveErr != nil {
			return nil, reserveErr
		}
	}
	state = &assemblyState{
		count: fragment.Count, total: fragment.TotalLength,
		buffer: make([]byte, fragment.TotalLength), received: make([]bool, fragment.Count),
		progressDeadline: now.Add(FragmentInactivityTimeout), reservation: reservation,
	}
	a.records[key] = state
	a.operationBytes[fragment.OperationID] = operationBytes + uint64(fragment.TotalLength)
	return state, nil
}

// RetireOperation releases active reassembly buffers and in-operation
// completed-record markers for operationID. It is idempotent while open.
func (a *Assembler) RetireOperation(operationID protocolsession.OperationID) error {
	if operationID.IsZero() {
		return ErrFragmentMalformed
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return ErrAssemblerClosed
	}
	a.retireOperationLocked(operationID)
	return nil
}

func (a *Assembler) ActiveRecords() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.records)
}

func (a *Assembler) activeOperationRecordsLocked(operationID protocolsession.OperationID) int {
	if a.operationBytes[operationID] == 0 {
		return 0
	}
	count := 0
	for key := range a.records {
		if key.operation == operationID {
			count++
		}
	}
	return count
}

func (a *Assembler) markRecordCompletedLocked(operationID protocolsession.OperationID, recordID records.RecordID, completed completedRecord) {
	recordsByID := a.completedRecords[operationID]
	if recordsByID == nil {
		recordsByID = make(map[records.RecordID]completedRecord, 1)
		a.completedRecords[operationID] = recordsByID
	}
	recordsByID[recordID] = completed
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

func (a *Assembler) retireOperationLocked(operationID protocolsession.OperationID) {
	if a.operationBytes[operationID] > 0 {
		for key := range a.records {
			if key.operation == operationID {
				a.releaseRecordLocked(key)
			}
		}
	}
	delete(a.completedRecords, operationID)
}

// Close releases all three levels of reservations immediately.
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
	clear(a.completedRecords)
	clear(a.operationBytes)
}
