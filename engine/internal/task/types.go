// Package task owns one native operation's cancellation and durable completion.
package task

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/windshare/windshare/core/observationstream"
)

type ID string

type State uint8

const (
	Running State = iota + 1
	Stopping
	Finished
)

type StopReason uint8

const (
	NoStop StopReason = iota
	Cancelled
	ShareStopped
	ApplicationClosed
)

var (
	ErrCancelled         = errors.New("task cancelled")
	ErrShareStopped      = errors.New("share explicitly stopped")
	ErrApplicationClosed = errors.New("application closed")
)

func (reason StopReason) cause() error {
	switch reason {
	case Cancelled:
		return ErrCancelled
	case ShareStopped:
		return ErrShareStopped
	case ApplicationClosed:
		return ErrApplicationClosed
	default:
		return nil
	}
}

func Reason(ctx context.Context) StopReason {
	switch cause := context.Cause(ctx); {
	case errors.Is(cause, ErrShareStopped):
		return ShareStopped
	case errors.Is(cause, ErrApplicationClosed):
		return ApplicationClosed
	case cause != nil:
		return Cancelled
	default:
		return NoStop
	}
}

type Outcome uint8

const (
	OutcomeSuccess Outcome = iota + 1
	OutcomePartial
	OutcomePaused
	OutcomeCancelled
	OutcomeStopped
	OutcomeFailed
)

type FailureClass uint8

const (
	FailureNone FailureClass = iota
	FailureLocal
	FailureNetwork
	FailureUsage
	FailureSourceDrift
)

// Event admits concrete domain observations without importing presentation types.
// Receivers switch on the concrete workflow event; terminal wording is not an
// application fact.
type Event interface {
	EngineEvent()
}

type Observation struct {
	TaskID ID
	At     time.Time
	Event  Event
}

type Lifecycle struct {
	State      State
	StopReason StopReason
	// Settlement is populated only for Finished; lifecycle and outcome are independent.
	Settlement
}

func (Lifecycle) EngineEvent() {}

type Control struct {
	ID             ID
	Now            func() time.Time
	Random         io.Reader
	CleanupContext func() (context.Context, context.CancelFunc)
	Emit           func(Event) bool
}

type Completion[T any] struct {
	Settlement
	Value T
}

type Result[T any] struct {
	Completion[T]
	StopReason   StopReason
	StartedAt    time.Time
	FinishedAt   time.Time
	Observations observationstream.Completion
}
