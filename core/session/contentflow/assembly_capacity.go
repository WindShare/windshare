package contentflow

import (
	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/session/protocolsession"
)

// AssemblyCapacity is one physical response, including its complete sealed
// object while authentication runs. Receipt alone cannot return these bytes.
type AssemblyCapacity struct {
	assembler   *Assembler
	reservation *reassemblyReservation
	operation   protocolsession.OperationID
	record      records.RecordID
	released    bool
}

func (a *Assembler) TryReserve(bytes uint64) (*AssemblyCapacity, <-chan struct{}, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return nil, nil, ErrAssemblerClosed
	}
	reservation, changed, err := tryReserveReassembly(a.hierarchy, bytes)
	if reservation == nil {
		return nil, changed, err
	}
	return &AssemblyCapacity{assembler: a, reservation: reservation}, nil, nil
}

func (capacity *AssemblyCapacity) AcceptAuthenticated(plaintext []byte) (AssemblyResult, error) {
	return capacity.assembler.acceptAuthenticated(plaintext, capacity)
}

func (capacity *AssemblyCapacity) Release() {
	a := capacity.assembler
	a.mu.Lock()
	defer a.mu.Unlock()
	if capacity.released {
		return
	}
	capacity.released = true
	if !capacity.operation.IsZero() && !a.closed {
		_ = a.terminateOperationLocked(capacity.operation, a.now())
	}
	capacity.reservation.Release()
}
