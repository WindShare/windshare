package runtrace

import (
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
)

type engineTaskPayloadV4 struct {
	TaskID                 string                      `json:"engine_task_id"`
	ObservedAt             string                      `json:"observed_at"`
	Stage                  string                      `json:"stage"`
	StopReason             string                      `json:"stop_reason"`
	Outcome                clievent.EngineTaskOutcome  `json:"outcome,omitempty"`
	FailureClass           clievent.EngineFailureClass `json:"failure_class,omitempty"`
	ShareInstance          string                      `json:"share_instance,omitempty"`
	ReceiveOperationID     string                      `json:"receive_operation_id,omitempty"`
	TransferJobID          string                      `json:"transfer_job_id,omitempty"`
	PreviousSessionID      string                      `json:"previous_protocol_session_id,omitempty"`
	AdmissionTrigger       string                      `json:"admission_trigger,omitempty"`
	AdmissionTerminalOwner string                      `json:"admission_terminal_owner,omitempty"`
	Failure                *failureV4                  `json:"failure,omitempty"`
	CleanupFailure         *failureV4                  `json:"cleanup_failure,omitempty"`
}

func (engineTaskPayloadV4) runTracePayloadV4() {}

func (visitor *encodeVisitorV4) VisitEngineTaskObserved(event clievent.EngineTaskObserved) error {
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
	payload := engineTaskPayloadV4{
		TaskID: facts.Task.String(), ObservedAt: facts.At.UTC().Format(time.RFC3339Nano),
		Stage: stage, StopReason: reason, Outcome: facts.Outcome, FailureClass: facts.FailureClass,
	}
	if facts.ShareInstance.Valid() {
		payload.ShareInstance = encodeTypedIdentity(facts.ShareInstance.Bytes())
	}
	if facts.Operation.Valid() {
		payload.ReceiveOperationID = encodeTypedIdentity(facts.Operation.Bytes())
	}
	if facts.Job.Valid() {
		payload.TransferJobID = encodeTypedIdentity(facts.Job.Bytes())
	}
	if facts.PreviousSession.Valid() {
		payload.PreviousSessionID = encodeTypedIdentity(facts.PreviousSession.Bytes())
	}
	if facts.Stage == clievent.EngineAdmissionDecision {
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
		projected, err := projectFailure(facts.Failure)
		if err != nil {
			return err
		}
		payload.Failure = &projected
	}
	if facts.CleanupFailure.Valid() {
		projected, err := projectFailure(facts.CleanupFailure)
		if err != nil {
			return err
		}
		payload.CleanupFailure = &projected
	}
	visitor.set("engine_task_observed", correlation, payload)
	return nil
}
