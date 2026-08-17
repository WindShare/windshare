package v2peer

import "math"

// ObservationLoss is cumulative and text-free for one producer stream.
type ObservationLoss struct {
	Capacity        uint64
	ObserverPanic   uint64
	CallbackTimeout uint64
	Undrained       uint64
}

func (loss ObservationLoss) Total() uint64 {
	total := saturatingObservationCount(loss.Capacity, loss.ObserverPanic)
	total = saturatingObservationCount(total, loss.CallbackTimeout)
	return saturatingObservationCount(total, loss.Undrained)
}

// ObservationCompletion proves that no callback can begin after this cut.
type ObservationCompletion struct {
	Delivered uint64
	Loss      ObservationLoss
	Drained   bool
}

type SenderObservationCompletion struct {
	Attempts    ObservationCompletion
	Diagnostics ObservationCompletion
}

type ReceiverObservationCompletion struct {
	Terminations ObservationCompletion
	Diagnostics  ObservationCompletion
}

func saturatingObservationCount(current, increment uint64) uint64 {
	if math.MaxUint64-current < increment {
		return math.MaxUint64
	}
	return current + increment
}
