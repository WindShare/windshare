package v2peer

// ObservationCompletion is a producer-proven snapshot at one stream's
// admission cut. Enqueued does not imply that a consumer received or projected
// an observation.
type ObservationCompletion struct {
	Enqueued uint64
	Loss     ObservationLoss
}

// ObservationLoss contains only facts proved by bounded producer admission.
type ObservationLoss struct {
	CapacityDropped uint64
}

func (loss ObservationLoss) Total() uint64 {
	return loss.CapacityDropped
}

type SenderObservationCompletion struct {
	Attempts    ObservationCompletion
	Diagnostics ObservationCompletion
}

type ReceiverObservationCompletion struct {
	Terminations ObservationCompletion
	Diagnostics  ObservationCompletion
}
