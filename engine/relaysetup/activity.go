package relaysetup

import "context"

type activity struct {
	address string
	cancel  context.CancelFunc
	done    chan struct{}
}

func (service *Service) begin(ctx context.Context, address string) (context.Context, func(), error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.active != nil || service.cancelling {
		return nil, nil, ErrSetupRunning
	}
	ctx, cancel := context.WithCancel(ctx)
	current := &activity{address: address, cancel: cancel, done: make(chan struct{})}
	service.active = current
	return ctx, func() {
		cancel()
		service.mu.Lock()
		defer service.mu.Unlock()
		service.active = nil
		close(current.done)
	}, nil
}

// beginCancellation reports whether it interrupted a running setup, because a
// setup still discovering its relay has not persisted a pending record yet.
func (service *Service) beginCancellation(ctx context.Context, address string) (func(), bool, error) {
	service.mu.Lock()
	if service.cancelling {
		service.mu.Unlock()
		return nil, false, ErrSetupRunning
	}
	current := service.active
	if current != nil && !sameRelay(current.address, address) {
		service.mu.Unlock()
		return nil, false, ErrPendingSetup
	}
	service.cancelling = true
	if current != nil {
		current.cancel()
	}
	service.mu.Unlock()
	release := func() {
		service.mu.Lock()
		service.cancelling = false
		service.mu.Unlock()
	}
	// Reserve the configuration until the running transaction has finished unwinding.
	// Cancellation can interrupt browser approval without racing its final file write.
	if current != nil {
		select {
		case <-current.done:
		case <-ctx.Done():
			release()
			return nil, false, ctx.Err()
		}
	}
	return release, current != nil, nil
}
