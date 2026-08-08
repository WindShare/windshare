package outputsession

import "sync"

type operationGate struct {
	mu             sync.Mutex
	active         uint64
	closing        bool
	closed         bool
	closeRequested chan struct{}
	drained        chan struct{}
	closeDone      chan struct{}
}

type operationLease struct {
	gate           *operationGate
	closeRequested <-chan struct{}
	once           sync.Once
}

func newOperationGate() operationGate {
	return operationGate{
		closeRequested: make(chan struct{}),
		drained:        make(chan struct{}),
		closeDone:      make(chan struct{}),
	}
}

func (gate *operationGate) acquire() (*operationLease, error) {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.closed || gate.closing {
		return nil, sessionClosedError()
	}
	gate.active++
	return &operationLease{gate: gate, closeRequested: gate.closeRequested}, nil
}

func (lease *operationLease) release() {
	if lease == nil || lease.gate == nil {
		return
	}
	lease.once.Do(func() {
		lease.gate.mu.Lock()
		lease.gate.active--
		if lease.gate.closing && lease.gate.active == 0 {
			close(lease.gate.drained)
		}
		lease.gate.mu.Unlock()
	})
}

func (lease *operationLease) closing() <-chan struct{} {
	if lease == nil {
		closed := make(chan struct{})
		close(closed)
		return closed
	}
	return lease.closeRequested
}

// requestClose announces closure before waiting. This lets operations blocked
// on descendant state yield their lease instead of deadlocking the settlement.
func (gate *operationGate) requestClose() (bool, <-chan struct{}) {
	gate.mu.Lock()
	if gate.closed {
		gate.mu.Unlock()
		return false, nil
	}
	if gate.closing {
		done := gate.closeDone
		gate.mu.Unlock()
		<-done
		return false, nil
	}
	gate.closing = true
	close(gate.closeRequested)
	if gate.active == 0 {
		close(gate.drained)
	}
	drained := gate.drained
	gate.mu.Unlock()
	return true, drained
}

func (gate *operationGate) finishClose() {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if !gate.closing || gate.active != 0 {
		panic("outputsession: invalid operation gate close transition")
	}
	close(gate.closeDone)
	gate.closed = true
	gate.closing = false
}
