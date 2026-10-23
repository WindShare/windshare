package eventproject

import (
	"github.com/windshare/windshare/engine/internal/receive"
	"github.com/windshare/windshare/engine/internal/share"
	"github.com/windshare/windshare/engine/internal/task"
	appobs "github.com/windshare/windshare/engine/observation"
)

func projectTask(workflow appobs.Workflow, envelope task.Observation) (appobs.EngineTaskObserved, error) {
	spec, _, err := projectTaskSpec(workflow, envelope)
	if err != nil {
		return appobs.EngineTaskObserved{}, err
	}
	return appobs.NewEngineTaskObserved(spec)
}
func projectTaskSpec(workflow appobs.Workflow, envelope task.Observation) (appobs.EngineTaskSpec, bool, error) {
	spec := appobs.EngineTaskSpec{Workflow: workflow, At: envelope.At}
	switch value := envelope.Event.(type) {
	case task.Lifecycle:
		if value.State != task.Finished && value.Settlement != (task.Settlement{}) {
			return spec, true, appobs.ErrInvalidEvent
		}
		switch value.State {
		case task.Running:
			spec.Stage = appobs.EngineTaskStarted
		case task.Stopping:
			spec.Stage = appobs.EngineTaskStopping
		case task.Finished:
			spec.Stage = appobs.EngineTaskSettled
			if !value.Valid() {
				return spec, true, appobs.ErrInvalidEvent
			}
			spec.Outcome = projectTaskOutcome(value.Outcome)
			spec.FailureClass = projectTaskFailureClass(value.FailureClass)
		default:
			return spec, true, appobs.ErrInvalidEvent
		}
		switch value.StopReason {
		case task.NoStop:
			spec.StopReason = appobs.EngineNoStop
		case task.Cancelled:
			spec.StopReason = appobs.EngineCancelled
		case task.ShareStopped:
			spec.StopReason = appobs.EngineShareStopped
		case task.ApplicationClosed:
			spec.StopReason = appobs.EngineApplicationClosed
		default:
			return spec, true, appobs.ErrInvalidEvent
		}
		spec.Failure = value.Failure
		spec.CleanupFailure = value.CleanupFailure
	case share.Observation:
		switch value.Milestone {
		case share.SourceAcquiring:
			spec.Stage = appobs.EngineSourceAcquiring
		case share.SourceAcquired:
			spec.Stage = appobs.EngineSourceAcquired
		case share.SourceAcquisitionFailed:
			spec.Stage = appobs.EngineSourceFailed
		default:
			return spec, false, nil
		}
		spec.ShareInstance, _ = appobs.NewSharingInstanceID(value.ShareInstance[:])
		spec.Failure, _ = ClassifyError(value.Failure)
	case receive.GenerationChanged:
		spec.Stage = appobs.EngineGenerationChanged
		spec.Operation, _ = appobs.NewReceiveOperationID(value.Operation[:])
		spec.Job, _ = appobs.NewTransferJobID(value.Job[:])
		spec.Session, _ = appobs.NewProtocolSessionID(value.Current[:])
		spec.PreviousSession, _ = appobs.NewProtocolSessionID(value.Previous[:])
	case receive.AdmissionObserved:
		spec.Stage = appobs.EngineAdmissionDecision
		spec.Operation, _ = appobs.NewReceiveOperationID(value.Operation[:])
		spec.Job, _ = appobs.NewTransferJobID(value.Job[:])
		spec.Session, _ = appobs.NewProtocolSessionID(value.Session[:])
		spec.AdmissionTrigger = appobs.EngineAdmissionTrigger(value.Trigger)
		spec.AdmissionTerminalOwner = appobs.EngineAdmissionTerminalOwner(value.TerminalOwner)
		spec.Failure, _ = ClassifyError(value.Cause)
	default:
		return spec, false, nil
	}
	var err error
	spec.Task, err = appobs.NewEngineTaskID(string(envelope.TaskID))
	return spec, true, err
}

func projectTaskOutcome(outcome task.Outcome) appobs.EngineTaskOutcome {
	switch outcome {
	case task.OutcomeSuccess:
		return appobs.EngineOutcomeSuccess
	case task.OutcomePartial:
		return appobs.EngineOutcomePartial
	case task.OutcomePaused:
		return appobs.EngineOutcomePaused
	case task.OutcomeCancelled:
		return appobs.EngineOutcomeCancelled
	case task.OutcomeStopped:
		return appobs.EngineOutcomeStopped
	default:
		return appobs.EngineOutcomeFailed
	}
}

func projectTaskFailureClass(class task.FailureClass) appobs.EngineFailureClass {
	switch class {
	case task.FailureNone:
		return appobs.EngineFailureNone
	case task.FailureNetwork:
		return appobs.EngineFailureNetwork
	case task.FailureUsage:
		return appobs.EngineFailureUsage
	case task.FailureSourceDrift:
		return appobs.EngineFailureSourceDrift
	default:
		return appobs.EngineFailureLocal
	}
}
