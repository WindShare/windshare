package observation

// LossSource identifies the producer whose cumulative counter is sampled.
// Different producers must not erase each other's evidence when their categories match.
type LossSource uint8

const (
	LossRelayQueue LossSource = iota + 1
	LossWebRTCQueue
	LossLaneQueue
	LossSenderAttemptCapacity
	LossSenderPathCapacity
	LossSenderCleanupResidue
	LossSenderDiagnosticDrain
	LossReceiverTerminationCapacity
	LossReceiverDiagnosticDrain
	LossNativeQueue
	LossProtocolQueue
)

// LossSnapshot is a cumulative producer fact, unlike ObserverLossObserved,
// which records a delta. Repeated snapshots are safe to reconcile.
type LossSnapshot struct {
	Source   LossSource
	Category ObserverLossCategory
	Reason   ObserverLossReason
	Count    uint64
}

func (LossSnapshot) EngineEvent() {}
