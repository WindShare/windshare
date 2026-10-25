package v2peer

// ObservationCompletion is a producer-proven snapshot at one stream's
// admission cut. Enqueued does not imply that a consumer received or projected
// an observation.
type ObservationCompletion struct {
	Enqueued uint64
	Loss     ObservationLoss
}

// ObservationLoss separates queue overflow from evidence unavailable at its source.
type ObservationLoss struct {
	CapacityDropped uint64
	// SourceDropped includes path evidence capacity and unfinished diagnostic cleanup.
	SourceDropped uint64
}

func (loss ObservationLoss) Total() uint64 {
	return saturatingAdd(loss.CapacityDropped, loss.SourceDropped)
}

type SenderObservationCompletion struct {
	Attempts    ObservationCompletion
	Diagnostics ObservationCompletion
}

type ReceiverObservationCompletion struct {
	Terminations ObservationCompletion
	Diagnostics  ObservationCompletion
}
