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
	WorkflowKind Workflow
	Source       LossSource
	Category     ObserverLossCategory
	Reason       ObserverLossReason
	Count        uint64
}

func (LossSnapshot) EngineEvent() {}

func (source LossSource) Name() (string, bool) {
	names := [...]string{"", "relay_queue", "webrtc_queue", "lane_queue", "sender_attempt_capacity",
		"sender_path_capacity", "sender_cleanup_residue", "sender_diagnostic_drain",
		"receiver_termination_capacity", "receiver_diagnostic_drain", "native_queue", "protocol_queue"}
	if source == 0 || int(source) >= len(names) {
		return "", false
	}
	return names[source], true
}

func (value LossSnapshot) Workflow() Workflow { return value.WorkflowKind }
func (LossSnapshot) Level() Level             { return LevelDebug }
func (value LossSnapshot) Validate() error {
	_, source := value.Source.Name()
	_, category := value.Category.Name()
	_, reason := value.Reason.Name()
	if !value.WorkflowKind.Valid() || !source || !category || !reason {
		return ErrInvalidEvent
	}
	return nil
}
