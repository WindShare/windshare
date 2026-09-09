package protocolsession

import (
	"context"
	"errors"
	"time"
)

// IsOperationCapacityError identifies a retryable refusal before any operation
// authority or wire delivery exists. Results for admitted operations cannot
// encounter this condition because admission reserves their retained identity.
func IsOperationCapacityError(err error) bool {
	return errors.Is(err, ErrActiveOperationBudget) || errors.Is(err, ErrTrackedOperationBudget)
}

type operationCapacity struct {
	available  bool
	changed    <-chan struct{}
	retryAfter time.Duration
	reason     OperationCapacityWaitReason
}

type OperationCapacityWaitReason uint8

const (
	OperationWaitingActiveCapacity OperationCapacityWaitReason = iota + 1
	OperationWaitingRetainedCapacity
)

// WaitForCapacity waits outside the session's sole reader and writer. Capacity
// can be contested again before admission; callers must retry only a proven
// pre-admission capacity refusal, never an ambiguous physical send.
func (table *OperationTable) WaitForCapacity(ctx context.Context, observe func(OperationCapacityWaitReason)) error {
	if table == nil {
		return ErrNilRuntimeDependency
	}
	var observed OperationCapacityWaitReason
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		capacity, err := table.capacity()
		if err != nil || capacity.available {
			return err
		}
		if observe != nil && observed != capacity.reason {
			observed = capacity.reason
			observeCapacityWait(observe, capacity.reason)
		}
		if err := capacity.wait(ctx); err != nil {
			return err
		}
	}
}

func (table *OperationTable) capacity() (operationCapacity, error) {
	table.mu.Lock()
	defer table.mu.Unlock()
	if table.terminal {
		return operationCapacity{}, ErrSessionTerminated
	}
	table.pruneExpired()
	if len(table.active) < table.limits.MaxActive &&
		len(table.active)+len(table.tombstones) < table.limits.MaxTracked {
		return operationCapacity{available: true}, nil
	}
	if table.capacityChanged == nil {
		table.capacityChanged = make(chan struct{})
	}
	capacity := operationCapacity{changed: table.capacityChanged}
	capacity.reason = OperationWaitingRetainedCapacity
	if len(table.active) >= table.limits.MaxActive {
		capacity.reason = OperationWaitingActiveCapacity
	}
	now := table.now()
	for _, tombstone := range table.tombstones {
		// A writer-held identity has no reclaimable deadline until its last
		// pin is released. Release wakes waiters and may extend retention.
		if tombstone.authority.pins != 0 {
			continue
		}
		delay := tombstone.expiresAt.Sub(now)
		if delay <= 0 {
			delay = time.Nanosecond
		}
		if capacity.retryAfter == 0 || delay < capacity.retryAfter {
			capacity.retryAfter = delay
		}
	}
	return capacity, nil
}

func observeCapacityWait(observe func(OperationCapacityWaitReason), reason OperationCapacityWaitReason) {
	// Observers cannot own admission or strand a caller by panicking.
	defer func() { _ = recover() }()
	observe(reason)
}

func (table *OperationTable) notifyCapacityLocked() {
	if table.capacityChanged != nil {
		close(table.capacityChanged)
		table.capacityChanged = nil
	}
}

func (capacity operationCapacity) timer() (<-chan time.Time, func()) {
	if capacity.retryAfter <= 0 {
		return nil, func() {}
	}
	timer := time.NewTimer(capacity.retryAfter)
	return timer.C, func() { timer.Stop() }
}

func (capacity operationCapacity) wait(ctx context.Context) error {
	expiry, stop := capacity.timer()
	defer stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-capacity.changed:
	case <-expiry:
	}
	return nil
}
