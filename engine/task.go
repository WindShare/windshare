package engine

import (
	"context"

	"github.com/windshare/windshare/engine/internal/task"
)

type TaskID = task.ID
type TaskState = task.State
type StopReason = task.StopReason
type Outcome = task.Outcome
type FailureClass = task.FailureClass
type Observation = task.Observation
type Event = task.Event
type LifecycleObservation = task.Lifecycle
type TaskSettlement = task.Settlement
type TaskCompletion[T any] = task.Completion[T]
type TaskResult[T any] = task.Result[T]

var ErrInvalidTaskSettlement = task.ErrInvalidSettlement

const (
	TaskRunning  = task.Running
	TaskStopping = task.Stopping
	TaskFinished = task.Finished

	NoStop            = task.NoStop
	Cancelled         = task.Cancelled
	ShareStopped      = task.ShareStopped
	ApplicationClosed = task.ApplicationClosed

	OutcomeSuccess   = task.OutcomeSuccess
	OutcomePartial   = task.OutcomePartial
	OutcomePaused    = task.OutcomePaused
	OutcomeCancelled = task.OutcomeCancelled
	OutcomeStopped   = task.OutcomeStopped
	OutcomeFailed    = task.OutcomeFailed

	FailureNone        = task.FailureNone
	FailureLocal       = task.FailureLocal
	FailureNetwork     = task.FailureNetwork
	FailureUsage       = task.FailureUsage
	FailureSourceDrift = task.FailureSourceDrift
)

type Task[T any] struct {
	current *task.Task[T]
}

func (current *Task[T]) ID() TaskID                       { return current.current.ID() }
func (current *Task[T]) State() TaskState                 { return current.current.State() }
func (current *Task[T]) Done() <-chan struct{}            { return current.current.Done() }
func (current *Task[T]) Observations() <-chan Observation { return current.current.Observations() }
func (current *Task[T]) Cancel()                          { current.current.Cancel() }
func (current *Task[T]) Wait(ctx context.Context) (TaskResult[T], error) {
	return current.current.Wait(ctx)
}
