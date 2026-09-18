package clievent

import "time"

const maximumEngineTaskIDBytes = 128

type EngineTaskID struct{ value string }

func NewEngineTaskID(value string) (EngineTaskID, error) {
	if value == "" || len(value) > maximumEngineTaskIDBytes {
		return EngineTaskID{}, ErrInvalidIdentity
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || character == ':' || character == '-' || character == '_' || character == '.' {
			continue
		}
		return EngineTaskID{}, ErrInvalidIdentity
	}
	return EngineTaskID{value: value}, nil
}

func (id EngineTaskID) String() string { return id.value }
func (id EngineTaskID) Valid() bool    { return id.value != "" }

type EngineTaskStage uint8

const (
	EngineTaskStarted EngineTaskStage = iota + 1
	EngineTaskStopping
	EngineTaskSettled
	EngineSourceAcquiring
	EngineSourceAcquired
	EngineSourceFailed
	EngineGenerationChanged
	EngineAdmissionDecision
)

func (stage EngineTaskStage) Name() (string, bool) {
	names := [...]string{"", "task_started", "task_stopping", "task_settled", "source_acquiring", "source_acquired", "source_failed", "generation_changed", "admission_decision"}
	if stage == 0 || int(stage) >= len(names) {
		return "", false
	}
	return names[stage], true
}

type EngineStopReason uint8

const (
	EngineNoStop EngineStopReason = iota
	EngineCancelled
	EngineShareStopped
	EngineApplicationClosed
)

func (reason EngineStopReason) Name() (string, bool) {
	names := [...]string{"none", "cancelled", "share_stopped", "application_closed"}
	if int(reason) >= len(names) {
		return "", false
	}
	return names[reason], true
}

type EngineTaskOutcome string

const (
	EngineOutcomeSuccess   EngineTaskOutcome = "success"
	EngineOutcomePartial   EngineTaskOutcome = "partial"
	EngineOutcomePaused    EngineTaskOutcome = "paused"
	EngineOutcomeCancelled EngineTaskOutcome = "cancelled"
	EngineOutcomeStopped   EngineTaskOutcome = "stopped"
	EngineOutcomeFailed    EngineTaskOutcome = "failed"
)

func (outcome EngineTaskOutcome) valid() bool {
	switch outcome {
	case EngineOutcomeSuccess, EngineOutcomePartial, EngineOutcomePaused, EngineOutcomeCancelled, EngineOutcomeStopped, EngineOutcomeFailed:
		return true
	default:
		return false
	}
}

type EngineFailureClass string

const (
	EngineFailureNone        EngineFailureClass = "none"
	EngineFailureLocal       EngineFailureClass = "local"
	EngineFailureNetwork     EngineFailureClass = "network"
	EngineFailureUsage       EngineFailureClass = "usage"
	EngineFailureSourceDrift EngineFailureClass = "source_drift"
)

func (class EngineFailureClass) valid() bool {
	switch class {
	case EngineFailureNone, EngineFailureLocal, EngineFailureNetwork, EngineFailureUsage, EngineFailureSourceDrift:
		return true
	default:
		return false
	}
}

type EngineAdmissionTrigger string

func (trigger EngineAdmissionTrigger) Name() (string, bool) {
	switch trigger {
	case "":
		return "none", true
	case "peer_failed", "peer_detached", "relay_only_policy":
		return string(trigger), true
	default:
		return "", false
	}
}

type EngineAdmissionTerminalOwner string

func (owner EngineAdmissionTerminalOwner) Name() (string, bool) {
	switch owner {
	case "":
		return "none", true
	case "none", "lifecycle_close", "peer_session_fatal", "runtime_terminal", "resume_failure", "p2p_unavailable":
		return string(owner), true
	default:
		return "", false
	}
}

// EngineTaskSpec correlates application ownership with protocol/output facts.
// The trace run identifies one client invocation; Task identifies the native
// operation, including clients that start several operations in the same run.
type EngineTaskSpec struct {
	Command                Command
	Task                   EngineTaskID
	At                     time.Time
	Stage                  EngineTaskStage
	StopReason             EngineStopReason
	Outcome                EngineTaskOutcome
	FailureClass           EngineFailureClass
	ShareInstance          SharingInstanceID
	Operation              ReceiveOperationID
	Job                    TransferJobID
	Session                ProtocolSessionID
	PreviousSession        ProtocolSessionID
	AdmissionTrigger       EngineAdmissionTrigger
	AdmissionTerminalOwner EngineAdmissionTerminalOwner
	Failure                Failure
	CleanupFailure         Failure
}

func (spec EngineTaskSpec) valid() bool {
	_, stage := spec.Stage.Name()
	_, reason := spec.StopReason.Name()
	_, trigger := spec.AdmissionTrigger.Name()
	_, owner := spec.AdmissionTerminalOwner.Name()
	if !spec.Command.Valid() || !spec.Task.Valid() || !stage || !reason || !trigger || !owner {
		return false
	}
	if spec.Stage == EngineTaskSettled {
		if !spec.Outcome.valid() || !spec.FailureClass.valid() || spec.CleanupFailure.Valid() && spec.Outcome != EngineOutcomeFailed {
			return false
		}
		switch spec.Outcome {
		case EngineOutcomeSuccess, EngineOutcomeStopped:
			return spec.FailureClass == EngineFailureNone && !spec.Failure.Valid()
		case EngineOutcomeCancelled:
			return spec.FailureClass == EngineFailureNone
		default:
			return spec.FailureClass != EngineFailureNone && spec.Failure.Valid()
		}
	}
	if spec.Outcome != "" || spec.FailureClass != "" {
		return false
	}
	switch spec.Stage {
	case EngineSourceAcquiring, EngineSourceAcquired, EngineSourceFailed:
		return spec.Command == CommandShare && spec.ShareInstance.Valid()
	case EngineGenerationChanged, EngineAdmissionDecision:
		return spec.Command == CommandGet && spec.Session.Valid()
	default:
		return true
	}
}

type EngineTaskObserved struct{ spec EngineTaskSpec }

func NewEngineTaskObserved(spec EngineTaskSpec) (EngineTaskObserved, error) {
	if !spec.valid() {
		return EngineTaskObserved{}, ErrInvalidEvent
	}
	return EngineTaskObserved{spec: spec}, nil
}
func (EngineTaskObserved) event()                      {}
func (event EngineTaskObserved) Command() Command      { return event.spec.Command }
func (EngineTaskObserved) Level() Level                { return LevelDebug }
func (event EngineTaskObserved) Facts() EngineTaskSpec { return event.spec }
func (event EngineTaskObserved) Accept(visitor Visitor) error {
	if visitor == nil || !event.spec.valid() {
		return ErrInvalidEvent
	}
	return visitor.VisitEngineTaskObserved(event)
}
