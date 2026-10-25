package wire

import (
	time "time"

	appobs "github.com/windshare/windshare/engine/observation"
)

type EngineTaskPayload struct {
	TaskID         string                    `json:"engine_task_id"`
	ObservedAt     string                    `json:"observed_at"`
	Stage          string                    `json:"stage"`
	StopReason     string                    `json:"stop_reason"`
	Outcome        appobs.EngineTaskOutcome  `json:"outcome,omitempty"`
	FailureClass   appobs.EngineFailureClass `json:"failure_class,omitempty"`
	Failure        *Failure                  `json:"failure,omitempty"`
	CleanupFailure *Failure                  `json:"cleanup_failure,omitempty"`
}

func (EngineTaskPayload) ObservationPayload() {}

func (enc *encoder) encodeEngineTaskObserved(event appobs.EngineTaskObserved) error {
	facts := event.Facts()
	stage, err := nameOf(facts.Stage)
	if err != nil {
		return err
	}
	reason, err := nameOf(facts.StopReason)
	if err != nil {
		return err
	}
	payload := EngineTaskPayload{
		TaskID: facts.Task.String(), ObservedAt: facts.At.UTC().Format(time.RFC3339Nano),
		Stage: stage, StopReason: reason, Outcome: facts.Outcome, FailureClass: facts.FailureClass,
	}
	if facts.Failure.Valid() {
		projected, err := EncodeFailure(facts.Failure)
		if err != nil {
			return err
		}
		payload.Failure = &projected
	}
	if facts.CleanupFailure.Valid() {
		projected, err := EncodeFailure(facts.CleanupFailure)
		if err != nil {
			return err
		}
		payload.CleanupFailure = &projected
	}
	enc.set("engine_task_observed", nil, payload)
	return nil
}
