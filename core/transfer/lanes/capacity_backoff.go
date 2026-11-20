package lanes

import (
	"errors"
	"time"

	"github.com/windshare/windshare/core/framechannel"
)

type capacityBackoffKind uint8

const (
	capacityReady capacityBackoffKind = iota
	capacityCooldown
	capacityWaiting
	capacityProbing
)

// laneCapacityBackoff mirrors the browser receiver's congestion backoff:
// server RetryAfter bounds the fallback deadline while an explicit relay
// clearance admits one probe without letting concurrent waiters oscillate.
type laneCapacityBackoff struct {
	kind     capacityBackoffKind
	until    time.Time
	pressure framechannel.CapacityPressure
	stop     chan struct{}
}

func (b *laneCapacityBackoff) readiness(now time.Time, inflight uint32) (ready, recoverable bool, deadline time.Time) {
	switch b.kind {
	case capacityReady:
		return true, false, time.Time{}
	case capacityProbing:
		return false, false, time.Time{}
	}
	if !now.Before(b.until) {
		b.reset()
		return true, false, time.Time{}
	}
	// Let in-flight attempts settle first so their late failures cannot re-arm
	// the wait while the recovery probe is checking the lane.
	if b.kind == capacityWaiting && inflight == 0 && b.pressure != nil && b.pressure.CapacityCleared() {
		return false, true, b.until
	}
	return false, false, b.until
}

func (b *laneCapacityBackoff) canRecover() bool {
	return b.kind == capacityWaiting && b.pressure != nil && b.pressure.CapacityCleared()
}

func (b *laneCapacityBackoff) startRecovery() {
	b.stopWatch()
	b.kind = capacityProbing
}

func (b *laneCapacityBackoff) settle(
	now time.Time,
	fetchErr error,
	reassignable bool,
	watch func(<-chan struct{}, <-chan struct{}),
) (waiting bool, retryAt time.Time) {
	if pressure, isWait := errors.AsType[*framechannel.BandwidthWaitError](fetchErr); isWait && reassignable {
		return true, b.enterWait(now, pressure, watch)
	}
	if b.kind == capacityProbing {
		if fetchErr == nil && b.pressure != nil && b.pressure.CapacityCleared() {
			b.reset()
		} else {
			b.stopWatch()
			b.kind, b.pressure = capacityCooldown, nil
		}
	}
	return false, time.Time{}
}

func (b *laneCapacityBackoff) enterWait(
	now time.Time,
	pressure *framechannel.BandwidthWaitError,
	watch func(<-chan struct{}, <-chan struct{}),
) time.Time {
	delay := min(maximumCapacityRetry, max(minimumCapacityRetry, pressure.RetryAfter))
	until := maxTime(b.until, now.Add(delay))
	// A failed probe spends the early clearance opportunity for this wait so
	// brief relay releases cannot trigger repeated probe storms.
	if b.kind == capacityProbing || (b.kind == capacityCooldown && b.until.After(now)) || pressure.Pressure == nil {
		b.stopWatch()
		b.kind, b.until, b.pressure = capacityCooldown, until, nil
		return until
	}
	if b.kind == capacityWaiting && b.pressure == pressure.Pressure {
		b.until = until
		return until
	}
	b.stopWatch()
	b.kind, b.until, b.pressure = capacityWaiting, until, pressure.Pressure
	if pressure.Pressure.CapacityCleared() || watch == nil {
		return until
	}
	if ch := pressure.Pressure.CapacityClearance(); ch != nil {
		stop := make(chan struct{})
		b.stop = stop
		watch(ch, stop)
	}
	return until
}

func (b *laneCapacityBackoff) reset() {
	b.stopWatch()
	b.kind = capacityReady
	b.until = time.Time{}
	b.pressure = nil
}

func (b *laneCapacityBackoff) stopWatch() {
	if b.stop != nil {
		close(b.stop)
		b.stop = nil
	}
}
