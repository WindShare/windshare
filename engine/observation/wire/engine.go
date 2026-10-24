package wire

import (
	"time"

	appobs "github.com/windshare/windshare/engine/observation"
)

type EngineTaskPayload struct {
	TaskID                 string                    `json:"engine_task_id"`
	ObservedAt             string                    `json:"observed_at"`
	Stage                  string                    `json:"stage"`
	StopReason             string                    `json:"stop_reason"`
	Outcome                appobs.EngineTaskOutcome  `json:"outcome,omitempty"`
	FailureClass           appobs.EngineFailureClass `json:"failure_class,omitempty"`
	ShareInstance          string                    `json:"share_instance,omitempty"`
	ReceiveOperationID     string                    `json:"receive_operation_id,omitempty"`
	TransferJobID          string                    `json:"transfer_job_id,omitempty"`
	PreviousSessionID      string                    `json:"previous_protocol_session_id,omitempty"`
	AdmissionTrigger       string                    `json:"admission_trigger,omitempty"`
	AdmissionTerminalOwner string                    `json:"admission_terminal_owner,omitempty"`
	Failure                *Failure                  `json:"failure,omitempty"`
	CleanupFailure         *Failure                  `json:"cleanup_failure,omitempty"`
}

func (EngineTaskPayload) ObservationPayload() {}

func (enc *encoder) encodeEngineTaskObserved(event appobs.EngineTaskObserved) error {
	facts := event.Facts()
	correlation, err := ProjectCorrelationV1(CorrelationInput{ProtocolSessionID: facts.Session})
	if err != nil {
		return err
	}
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
	if facts.ShareInstance.Valid() {
		payload.ShareInstance = EncodeIdentity(facts.ShareInstance.Bytes())
	}
	if facts.Operation.Valid() {
		payload.ReceiveOperationID = EncodeIdentity(facts.Operation.Bytes())
	}
	if facts.Job.Valid() {
		payload.TransferJobID = EncodeIdentity(facts.Job.Bytes())
	}
	if facts.PreviousSession.Valid() {
		payload.PreviousSessionID = EncodeIdentity(facts.PreviousSession.Bytes())
	}
	if facts.Stage == appobs.EngineAdmissionDecision || facts.Stage == appobs.EngineRelayContentClaimed {
		payload.AdmissionTrigger, err = nameOf(facts.AdmissionTrigger)
		if err != nil {
			return err
		}
		payload.AdmissionTerminalOwner, err = nameOf(facts.AdmissionTerminalOwner)
		if err != nil {
			return err
		}
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
	enc.set("engine_task_observed", correlation, payload)
	return nil
}
