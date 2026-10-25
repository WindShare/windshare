package eventproject

import (
	task "github.com/windshare/windshare/engine/internal/task"
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
		if err := projectTaskLifecycle(&spec, value); err != nil {
			return spec, true, err
		}
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

func projectTaskLifecycle(spec *appobs.EngineTaskSpec, value task.Lifecycle) error {
	if value.State != task.Finished && value.Settlement != (task.Settlement{}) {
		return appobs.ErrInvalidEvent
	}
	switch value.State {
	case task.Running:
		spec.Stage = appobs.EngineTaskStarted
	case task.Stopping:
		spec.Stage = appobs.EngineTaskStopping
	case task.Finished:
		spec.Stage = appobs.EngineTaskSettled
		if !value.Valid() {
			return appobs.ErrInvalidEvent
		}
		spec.Outcome = projectTaskOutcome(value.Outcome)
		spec.FailureClass = projectTaskFailureClass(value.FailureClass)
	default:
		return appobs.ErrInvalidEvent
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
		return appobs.ErrInvalidEvent
	}
	spec.Failure = value.Failure
	spec.CleanupFailure = value.CleanupFailure
	return nil
}
