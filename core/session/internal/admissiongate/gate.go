// Package admissiongate joins short-lived work before its owner's resources are released.
package admissiongate

import (
	"context"
	"sync"
)

// Gate's zero value admits work. Sealing is permanent; role-specific shutdown
// and the lifetime of resources transferred by admitted work remain with the owner.
type Gate struct {
	mu       sync.Mutex
	sealed   bool
	lifetime context.Context
	cancel   context.CancelFunc
	pending  sync.WaitGroup
}

// Begin borrows the caller's context without making it the gate's lifetime.
// The returned release must follow all cleanup, even after context cancellation.
func (gate *Gate) Begin(caller context.Context) (context.Context, func(), bool) {
	if gate == nil || caller == nil {
		return nil, nil, false
	}
	gate.mu.Lock()
	if gate.sealed {
		gate.mu.Unlock()
		return nil, nil, false
	}
	if gate.lifetime == nil {
		gate.lifetime, gate.cancel = context.WithCancel(context.Background())
	}
	// Admission and sealing share the lock so Close cannot race a late Add.
	gate.pending.Add(1)
	lifetime := gate.lifetime
	gate.mu.Unlock()

	ctx, cancel := context.WithCancel(caller)
	stopLifetime := context.AfterFunc(lifetime, cancel)
	return ctx, sync.OnceFunc(func() {
		stopLifetime()
		cancel()
		gate.pending.Done()
	}), true
}

// Seal rejects new work and cancels admitted work without waiting for cleanup.
// Only the first caller returns true, allowing one owner to initiate shutdown.
func (gate *Gate) Seal() bool {
	if gate == nil {
		return false
	}
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.sealed {
		return false
	}
	gate.sealed = true
	if gate.cancel != nil {
		gate.cancel()
		gate.lifetime = nil
		gate.cancel = nil
	}
	return true
}

// Close also seals the gate, so joining never depends on a caller remembering
// a separate admission barrier. Concurrent closes join the same admitted work.
func (gate *Gate) Close() {
	if gate == nil {
		return
	}
	gate.Seal()
	gate.pending.Wait()
}
