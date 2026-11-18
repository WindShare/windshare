package framechannel

import (
	"context"
	"errors"
	"time"
)

// CapacityPressure is optional transport evidence. Its bounded lifetime and
// independent heartbeat preserve failure detection while recovery backs off.
type CapacityPressure interface{ CapacityWait() time.Duration }

type BandwidthWaitError struct {
	Cause      error
	RetryAfter time.Duration
}

func (err *BandwidthWaitError) Error() string { return "relay bandwidth is busy: " + err.Cause.Error() }
func (err *BandwidthWaitError) Unwrap() error { return err.Cause }

// WithCapacityWait is called only at a progress/deadline boundary, never for
// authentication, protocol, EOF or explicit cancellation failures.
func WithCapacityWait(channel any, cause error) error {
	if cause == nil {
		return nil
	}
	if _, existing := errors.AsType[*BandwidthWaitError](cause); existing {
		return cause
	}
	if pressure, ok := channel.(CapacityPressure); ok {
		if delay := pressure.CapacityWait(); delay > 0 {
			return &BandwidthWaitError{Cause: cause, RetryAfter: delay}
		}
	}
	return cause
}

func ClassifyDeadline(channel any, cause error) error {
	if errors.Is(cause, context.DeadlineExceeded) {
		return WithCapacityWait(channel, cause)
	}
	return cause
}
