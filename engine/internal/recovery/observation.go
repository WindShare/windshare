package recovery

import "github.com/windshare/windshare/core/transfer/receivecontract"

type Phase uint8

const (
	InspectStarted Phase = iota + 1
	InspectCompleted
	DiscardStarted
	DiscardCompleted
)

// Observation records authority decisions without carrying terminal ordinals or
// making the delivery of a diagnostic part of native resource ownership.
type Observation struct {
	Phase          Phase
	OperationID    receivecontract.OperationID
	OperationCount int
	NeedsAttention bool
	FailureKind    FailureKind
	DiscardStatus  DiscardStatus
	Failed         bool
}

func (Observation) EngineEvent() {}
