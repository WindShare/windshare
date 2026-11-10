package relaysetup

import "context"

type activity struct {
	address string
	cancel  context.CancelFunc
	done    chan struct{}
	// installed is written before done closes, so a cancellation that waited on
	// done can tell an interrupted setup from one that won the race and finished.
	installed bool
}

// interruption is what Cancel found running in this process. It cannot see a
// setup running in another process; that one is only visible through its
// persisted pending record.
type interruption uint8

const (
	nothingRunning interruption = iota
	// stoppedRunning: the setup returned without installing a profile.
	stoppedRunning
	// finishedFirst: the setup installed its profile before it observed the
	// cancellation, so nothing was cancelled.
	finishedFirst
)

func (value interruption) String() string {
	switch value {
	case stoppedRunning:
		return "stopped_running"
	case finishedFirst:
		return "finished_first"
	default:
		return "nothing_running"
	}
}

// begin returns a finish callback that records whether the run installed a
// profile; every successful Run result is a committed installation.
func (service *Service) begin(ctx context.Context, address string) (context.Context, func(installed bool), error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.active != nil || service.cancelling {
		return nil, nil, ErrSetupRunning
	}
	ctx, cancel := context.WithCancel(ctx)
	current := &activity{address: address, cancel: cancel, done: make(chan struct{})}
	service.active = current
	return ctx, func(installed bool) {
		cancel()
		service.mu.Lock()
		defer service.mu.Unlock()
		current.installed = installed
		service.active = nil
		close(current.done)
	}, nil
}

// beginCancellation also reports what it interrupted, because a setup still
// discovering its relay has not persisted a pending record yet, and a setup may
// commit its installation before it notices the cancellation.
func (service *Service) beginCancellation(ctx context.Context, address string) (func(), interruption, error) {
	service.mu.Lock()
	if service.cancelling {
		service.mu.Unlock()
		return nil, nothingRunning, ErrSetupRunning
	}
	current := service.active
	if current != nil && !sameRelay(current.address, address) {
		service.mu.Unlock()
		return nil, nothingRunning, ErrPendingSetup
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
	if current == nil {
		return release, nothingRunning, nil
	}
	// Reserve the configuration until the running transaction has finished unwinding.
	// Cancellation can interrupt browser approval without racing its final file write.
	select {
	case <-current.done:
	case <-ctx.Done():
		release()
		return nil, nothingRunning, ctx.Err()
	}
	service.mu.Lock()
	installed := current.installed
	service.mu.Unlock()
	if installed {
		return release, finishedFirst, nil
	}
	return release, stoppedRunning, nil
}
