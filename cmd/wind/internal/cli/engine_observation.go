package cli

import (
	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/cmd/wind/internal/commandprojection"
	"github.com/windshare/windshare/engine"
)

// This projection is shared by both task adapters so stable engine ownership is
// retained even when the workflow event has no protocol session yet.
func observeEngineTask(runtime *commandRuntime, command clievent.Command, value engine.Observation) {
	if runtime == nil || !runtime.detailedDiagnosticsEnabled() {
		return
	}
	spec, relevant, err := projectEngineTask(command, value)
	if !relevant {
		return
	}
	if err != nil {
		runtime.ReportObserverLoss(clievent.ObserverLossCommandAdapter, clievent.ObserverLossEventContract, 1)
		return
	}
	projected, err := clievent.NewEngineTaskObserved(spec)
	if err != nil {
		runtime.ReportObserverLoss(clievent.ObserverLossCommandAdapter, clievent.ObserverLossEventContract, 1)
		return
	}
	runtime.Observe(projected)
}

func projectEngineTask(command clievent.Command, envelope engine.Observation) (clievent.EngineTaskSpec, bool, error) {
	spec := clievent.EngineTaskSpec{Command: command, At: envelope.At}
	switch value := envelope.Event.(type) {
	case engine.LifecycleObservation:
		if value.State != engine.TaskFinished && value.Settlement != (engine.TaskSettlement{}) {
			return spec, true, clievent.ErrInvalidEvent
		}
		switch value.State {
		case engine.TaskRunning:
			spec.Stage = clievent.EngineTaskStarted
		case engine.TaskStopping:
			spec.Stage = clievent.EngineTaskStopping
		case engine.TaskFinished:
			spec.Stage = clievent.EngineTaskSettled
			if !value.Valid() {
				return spec, true, clievent.ErrInvalidEvent
			}
			spec.Outcome = projectTaskOutcome(value.Outcome)
			spec.FailureClass = projectTaskFailureClass(value.FailureClass)
		default:
			return spec, true, clievent.ErrInvalidEvent
		}
		switch value.StopReason {
		case engine.NoStop:
			spec.StopReason = clievent.EngineNoStop
		case engine.Cancelled:
			spec.StopReason = clievent.EngineCancelled
		case engine.ShareStopped:
			spec.StopReason = clievent.EngineShareStopped
		case engine.ApplicationClosed:
			spec.StopReason = clievent.EngineApplicationClosed
		default:
			return spec, true, clievent.ErrInvalidEvent
		}
		spec.Failure, _ = commandprojection.ClassifyError(value.Err)
		spec.CleanupFailure, _ = commandprojection.ClassifyError(value.CleanupError)
	case engine.ShareObservation:
		switch value.Milestone {
		case engine.ShareMilestoneSourceAcquiring:
			spec.Stage = clievent.EngineSourceAcquiring
		case engine.ShareMilestoneSourceAcquired:
			spec.Stage = clievent.EngineSourceAcquired
		case engine.ShareMilestoneSourceAcquisitionFailed:
			spec.Stage = clievent.EngineSourceFailed
		default:
			return spec, false, nil
		}
		spec.ShareInstance, _ = clievent.NewSharingInstanceID(value.ShareInstance[:])
		spec.Failure, _ = commandprojection.ClassifyError(value.Failure)
	case engine.ReceiveGenerationChanged:
		spec.Stage = clievent.EngineGenerationChanged
		spec.Operation, _ = clievent.NewReceiveOperationID(value.Operation[:])
		spec.Job, _ = clievent.NewTransferJobID(value.Job[:])
		spec.Session, _ = clievent.NewProtocolSessionID(value.Current[:])
		spec.PreviousSession, _ = clievent.NewProtocolSessionID(value.Previous[:])
	case engine.ReceiveAdmissionObserved:
		spec.Stage = clievent.EngineAdmissionDecision
		spec.Operation, _ = clievent.NewReceiveOperationID(value.Operation[:])
		spec.Job, _ = clievent.NewTransferJobID(value.Job[:])
		spec.Session, _ = clievent.NewProtocolSessionID(value.Session[:])
		spec.AdmissionTrigger = clievent.EngineAdmissionTrigger(value.Trigger)
		spec.AdmissionTerminalOwner = clievent.EngineAdmissionTerminalOwner(value.TerminalOwner)
		spec.Failure, _ = commandprojection.ClassifyError(value.Cause)
	default:
		return spec, false, nil
	}
	var err error
	spec.Task, err = clievent.NewEngineTaskID(string(envelope.TaskID))
	return spec, true, err
}

func projectTaskOutcome(outcome engine.Outcome) clievent.EngineTaskOutcome {
	switch outcome {
	case engine.OutcomeSuccess:
		return clievent.EngineOutcomeSuccess
	case engine.OutcomePartial:
		return clievent.EngineOutcomePartial
	case engine.OutcomePaused:
		return clievent.EngineOutcomePaused
	case engine.OutcomeCancelled:
		return clievent.EngineOutcomeCancelled
	case engine.OutcomeStopped:
		return clievent.EngineOutcomeStopped
	default:
		return clievent.EngineOutcomeFailed
	}
}

func projectTaskFailureClass(class engine.FailureClass) clievent.EngineFailureClass {
	switch class {
	case engine.FailureNone:
		return clievent.EngineFailureNone
	case engine.FailureNetwork:
		return clievent.EngineFailureNetwork
	case engine.FailureUsage:
		return clievent.EngineFailureUsage
	case engine.FailureSourceDrift:
		return clievent.EngineFailureSourceDrift
	default:
		return clievent.EngineFailureLocal
	}
}
