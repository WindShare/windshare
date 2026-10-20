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

func (service *Service) beginCancellation(ctx context.Context, address string) (func(), error) {
	service.mu.Lock()
	if service.cancelling {
		service.mu.Unlock()
		return nil, ErrSetupRunning
	}
	current := service.active
	if current != nil && !sameRelay(current.address, address) {
		service.mu.Unlock()
		return nil, ErrPendingSetup
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
			return nil, ctx.Err()
		}
	}
	return release, nil
}
