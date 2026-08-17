package v2peer

import (
	"context"
	"sync"
	"time"
)

// PeerDiagnosticCategory identifies the sealed evidence stream that lost
// observability. It never conveys transport or fallback authority.
type PeerDiagnosticCategory string

const (
	PeerDiagnosticSenderAttempt       PeerDiagnosticCategory = "sender_attempt"
	PeerDiagnosticReceiverTermination PeerDiagnosticCategory = "receiver_termination"
)

// PeerDiagnosticReason is deliberately text-free: provider errors and recovered
// panic values must remain behind the connectivity boundary.
type PeerDiagnosticReason string

const (
	PeerDiagnosticObserverPanic    PeerDiagnosticReason = "observer_panic"
	PeerDiagnosticObserverCapacity PeerDiagnosticReason = "observer_capacity"
	PeerDiagnosticEvidenceCapacity PeerDiagnosticReason = "evidence_capacity"
	PeerDiagnosticCleanupResidue   PeerDiagnosticReason = "cleanup_residue"
)

// PeerDiagnosticObservation reports the cumulative, saturating count for one
// closed category/reason pair. Later observations supersede earlier counts.
type PeerDiagnosticObservation struct {
	Category PeerDiagnosticCategory
	Reason   PeerDiagnosticReason
	Count    uint64
}

type PeerDiagnosticObserver interface {
	ObservePeerDiagnostic(PeerDiagnosticObservation)
}

type PeerDiagnosticContextObserver interface {
	PeerDiagnosticObserver
	ObservePeerDiagnosticContext(context.Context, PeerDiagnosticObservation)
}

type PeerDiagnosticObserverFunc func(PeerDiagnosticObservation)

func (function PeerDiagnosticObserverFunc) ObservePeerDiagnostic(observation PeerDiagnosticObservation) {
	if function != nil {
		function(observation)
	}
}

type PeerDiagnosticContextObserverFunc func(context.Context, PeerDiagnosticObservation)

func (function PeerDiagnosticContextObserverFunc) ObservePeerDiagnostic(observation PeerDiagnosticObservation) {
	function.ObservePeerDiagnosticContext(context.Background(), observation)
}

func (function PeerDiagnosticContextObserverFunc) ObservePeerDiagnosticContext(
	ctx context.Context,
	observation PeerDiagnosticObservation,
) {
	if function != nil {
		function(ctx, observation)
	}
}

const peerDiagnosticCounterCount = 6

type peerDiagnosticReporter struct {
	mu        sync.Mutex
	observer  PeerDiagnosticObserver
	totals    [peerDiagnosticCounterCount]uint64
	delivered [peerDiagnosticCounterCount]uint64
	active    bool
	closing   bool
	detached  bool

	callbackLive  bool
	drained       bool
	deliveredCut  uint64
	loss          ObservationLoss
	callbackLimit time.Duration
	detach        chan struct{}
	detachOnce    sync.Once
	done          chan struct{}
	doneOnce      sync.Once
}

func newPeerDiagnosticReporter(observer PeerDiagnosticObserver) *peerDiagnosticReporter {
	if observer == nil {
		return nil
	}
	return &peerDiagnosticReporter{
		observer: observer, drained: true, callbackLimit: peerObservationCallbackLimit,
		detach: make(chan struct{}), done: make(chan struct{}),
	}
}

func (factory *ReceiverFactory) reportDiagnostic(category PeerDiagnosticCategory, reason PeerDiagnosticReason) {
	if factory == nil || factory.diagnostics == nil {
		return
	}
	factory.diagnostics.report(category, reason)
}

func (reporter *peerDiagnosticReporter) report(category PeerDiagnosticCategory, reason PeerDiagnosticReason) {
	index, valid := peerDiagnosticIndex(category, reason)
	if reporter == nil || !valid {
		return
	}
	reporter.mu.Lock()
	if reporter.closing || reporter.detached {
		reporter.loss.Undrained = saturatingObservationCount(reporter.loss.Undrained, 1)
		reporter.mu.Unlock()
		return
	}
	reporter.totals[index] = saturatingObservationCount(reporter.totals[index], 1)
	if reporter.active {
		reporter.mu.Unlock()
		return
	}
	reporter.active = true
	reporter.mu.Unlock()
	go reporter.drain()
}

func (reporter *peerDiagnosticReporter) drain() {
	for {
		reporter.mu.Lock()
		index := reporter.nextLocked()
		if reporter.detached || index < 0 {
			reporter.active = false
			if reporter.closing || reporter.detached {
				reporter.doneOnce.Do(func() { close(reporter.done) })
			}
			reporter.mu.Unlock()
			return
		}
		count := reporter.totals[index]
		reporter.callbackLive = true
		reporter.mu.Unlock()

		category, reason := peerDiagnosticPair(index)
		outcome := reporter.invoke(PeerDiagnosticObservation{Category: category, Reason: reason, Count: count})
		reporter.mu.Lock()
		reporter.callbackLive = false
		if reporter.detached || outcome == observationCallbackAbandoned {
			reporter.active = false
			reporter.doneOnce.Do(func() { close(reporter.done) })
			reporter.mu.Unlock()
			return
		}
		switch outcome {
		case observationCallbackDelivered:
			reporter.delivered[index] = count
			reporter.deliveredCut = saturatingObservationCount(reporter.deliveredCut, 1)
		case observationCallbackPanicked:
			reporter.delivered[index] = count
			reporter.loss.ObserverPanic = saturatingObservationCount(reporter.loss.ObserverPanic, 1)
			reporter.detachPendingLocked()
		case observationCallbackTimedOut:
			reporter.delivered[index] = count
			reporter.loss.CallbackTimeout = saturatingObservationCount(reporter.loss.CallbackTimeout, 1)
			reporter.detachPendingLocked()
		}
		reporter.mu.Unlock()
	}
}

func (reporter *peerDiagnosticReporter) invoke(observation PeerDiagnosticObservation) observationCallbackOutcome {
	result := make(chan observationCallbackOutcome, 1)
	callbackContext, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		outcome := observationCallbackDelivered
		defer func() {
			if recover() != nil {
				outcome = observationCallbackPanicked
			}
			result <- outcome
		}()
		if contextual, ok := reporter.observer.(PeerDiagnosticContextObserver); ok {
			contextual.ObservePeerDiagnosticContext(callbackContext, observation)
			return
		}
		reporter.observer.ObservePeerDiagnostic(observation)
	}()
	timer := time.NewTimer(reporter.callbackLimit)
	defer timer.Stop()
	select {
	case outcome := <-result:
		return outcome
	case <-timer.C:
		cancel()
		return observationCallbackTimedOut
	case <-reporter.detach:
		cancel()
		return observationCallbackAbandoned
	}
}

func (reporter *peerDiagnosticReporter) detachPendingLocked() {
	for index := range reporter.totals {
		if reporter.totals[index] != reporter.delivered[index] {
			reporter.loss.Undrained = saturatingObservationCount(reporter.loss.Undrained, 1)
		}
	}
	reporter.detached = true
	reporter.drained = false
	reporter.detachOnce.Do(func() { close(reporter.detach) })
}

func (reporter *peerDiagnosticReporter) complete(ctx context.Context) ObservationCompletion {
	if reporter == nil {
		return ObservationCompletion{Drained: true}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	reporter.mu.Lock()
	if !reporter.closing {
		reporter.closing = true
		if !reporter.active {
			reporter.doneOnce.Do(func() { close(reporter.done) })
		}
	}
	reporter.mu.Unlock()
	select {
	case <-reporter.done:
	default:
		select {
		case <-reporter.done:
		case <-ctx.Done():
			reporter.mu.Lock()
			if !reporter.detached {
				reporter.detachPendingLocked()
				reporter.doneOnce.Do(func() { close(reporter.done) })
			}
			reporter.mu.Unlock()
			<-reporter.done
		}
	}
	reporter.mu.Lock()
	completion := ObservationCompletion{
		Delivered: reporter.deliveredCut, Loss: reporter.loss, Drained: reporter.drained,
	}
	reporter.mu.Unlock()
	return completion
}

func (reporter *peerDiagnosticReporter) nextLocked() int {
	for index := range reporter.totals {
		if reporter.totals[index] != reporter.delivered[index] {
			return index
		}
	}
	return -1
}

func peerDiagnosticIndex(category PeerDiagnosticCategory, reason PeerDiagnosticReason) (int, bool) {
	switch category {
	case PeerDiagnosticSenderAttempt:
		switch reason {
		case PeerDiagnosticObserverPanic:
			return 0, true
		case PeerDiagnosticObserverCapacity:
			return 1, true
		case PeerDiagnosticEvidenceCapacity:
			return 2, true
		case PeerDiagnosticCleanupResidue:
			return 3, true
		}
	case PeerDiagnosticReceiverTermination:
		switch reason {
		case PeerDiagnosticObserverPanic:
			return 4, true
		case PeerDiagnosticObserverCapacity:
			return 5, true
		}
	}
	return 0, false
}

func peerDiagnosticPair(index int) (PeerDiagnosticCategory, PeerDiagnosticReason) {
	switch index {
	case 0:
		return PeerDiagnosticSenderAttempt, PeerDiagnosticObserverPanic
	case 1:
		return PeerDiagnosticSenderAttempt, PeerDiagnosticObserverCapacity
	case 2:
		return PeerDiagnosticSenderAttempt, PeerDiagnosticEvidenceCapacity
	case 3:
		return PeerDiagnosticSenderAttempt, PeerDiagnosticCleanupResidue
	case 4:
		return PeerDiagnosticReceiverTermination, PeerDiagnosticObserverPanic
	case 5:
		return PeerDiagnosticReceiverTermination, PeerDiagnosticObserverCapacity
	default:
		panic("invalid peer diagnostic counter index")
	}
}

func saturatingAdd(current, increment uint64) uint64 {
	return saturatingObservationCount(current, increment)
}
